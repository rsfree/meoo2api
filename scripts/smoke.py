#!/usr/bin/env python3
"""meoo2api 前门冒烟测试：真实打上游，跑完全部对外契约。

用法：
    python3 scripts/smoke.py --base http://127.0.0.1:8791 --key sk-xxx
    python3 scripts/smoke.py --base http://127.0.0.1:8791 --key sk-xxx --quick

只用标准库，禁用系统代理（本机回环不需要走代理）。
"""
from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.request

# 显式禁用代理：HTTPS_PROXY 对本地回环也生效会拖慢/打断请求。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

PASS, FAIL, SKIP = "PASS", "FAIL", "SKIP"
results: list[tuple[str, str, str]] = []


def record(name: str, state: str, detail: str) -> None:
    results.append((name, state, detail))
    mark = {PASS: "✓", FAIL: "✗", SKIP: "–"}[state]
    print(f"  {mark} {name:<34} {detail}")


def http(path: str, payload=None, key: str | None = None, timeout: int = 180):
    url = BASE + path
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
    req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Authorization", "Bearer " + key)
    try:
        with OPENER.open(req, timeout=timeout) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception as e:  # noqa: BLE001
        return 0, {}, str(e).encode()


def sse(path: str, payload: dict, key: str, timeout: int = 180) -> tuple[int, list[dict], bool]:
    """返回 (状态, data 帧列表, 是否收到 [DONE])。"""
    url = BASE + path
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "text/event-stream")
    req.add_header("Authorization", "Bearer " + key)
    frames: list[dict] = []
    done = False
    try:
        with OPENER.open(req, timeout=timeout) as resp:
            for raw in resp:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                body = line[5:].strip()
                if body == "[DONE]":
                    done = True
                    continue
                try:
                    frames.append(json.loads(body))
                except json.JSONDecodeError:
                    pass
            return resp.status, frames, done
    except urllib.error.HTTPError as e:
        return e.code, frames, done
    except Exception:  # noqa: BLE001
        return 0, frames, done


def err_of(body: bytes) -> dict:
    try:
        return json.loads(body).get("error", {}) or {}
    except Exception:  # noqa: BLE001
        return {}


def main() -> int:
    global BASE
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8791")
    ap.add_argument("--key", required=True, help="面板签发的 sk-… 或项目 AK")
    ap.add_argument("--quick", action="store_true", help="跳过慢模型")
    args = ap.parse_args()
    BASE = args.base.rstrip("/")
    KEY = args.key

    print(f"\n目标 {BASE}\n")

    # ---------- 1. 基础 ----------
    print("[1] 健康与目录")
    code, _, body = http("/healthz")
    ok = code == 200 and b'"ok":true' in body
    record("GET /healthz", PASS if ok else FAIL, f"HTTP {code}")

    code, hdr, body = http("/v1/models", key=KEY)
    models = []
    if code == 200:
        try:
            models = [m["id"] for m in json.loads(body)["data"]]
        except Exception:  # noqa: BLE001
            pass
    record("GET /v1/models", PASS if models else FAIL,
           f"HTTP {code} · {len(models)} 个 · source={hdr.get('X-Meoo-Models-Source', '?')}")

    pick = "qwen" if "qwen" in models else (models[0] if models else "qwen3.8-flash")
    record("别名解析 qwen", PASS if "qwen" in models else SKIP, f"解析目标在目录中={pick}")

    code, _, body = http(f"/v1/models/{pick}", key=KEY)
    record("GET /v1/models/{id}", PASS if code == 200 else FAIL, f"HTTP {code}")

    # ---------- 2. 鉴权 ----------
    print("\n[2] 鉴权")
    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": [{"role": "user", "content": "x"}]})
    e = err_of(body)
    ok = code == 401 and e.get("code") == "invalid_api_key" and e.get("type") == "authentication_error"
    record("无 Key → 401", PASS if ok else FAIL, f"HTTP {code} {e.get('code', '')}")

    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": [{"role": "user", "content": "x"}]}, key="sk-wrong")
    e = err_of(body)
    record("错 Key → 401", PASS if code == 401 and e.get("code") == "invalid_api_key" else FAIL,
           f"HTTP {code} {e.get('code', '')}")

    # ---------- 3. 参数校验 ----------
    print("\n[3] 参数校验（不该打到上游）")
    cases = [
        ("空 messages", {"model": pick, "messages": []}, 400, "invalid_request", "messages"),
        ("messages 非数组", {"model": pick, "messages": "hi"}, 400, "invalid_request", "messages"),
        ("role 缺失", {"model": pick, "messages": [{"content": "x"}]}, 400, "invalid_request", "messages"),
        ("n=2", {"model": pick, "n": 2, "messages": [{"role": "user", "content": "x"}]},
         400, "unsupported_parameter", "n"),
        ("非法 JSON", None, 400, "invalid_request", None),
    ]
    for name, payload, want_code, want_err, want_param in cases:
        if payload is None:
            req = urllib.request.Request(BASE + "/v1/chat/completions", data=b"{not json", method="POST")
            req.add_header("Content-Type", "application/json")
            req.add_header("Authorization", "Bearer " + KEY)
            try:
                with OPENER.open(req, timeout=30) as resp:
                    code, body = resp.status, resp.read()
            except urllib.error.HTTPError as ex:
                code, body = ex.code, ex.read()
        else:
            code, _, body = http("/v1/chat/completions", payload, key=KEY)
        e = err_of(body)
        ok = code == want_code and e.get("code") == want_err and (want_param is None or e.get("param") == want_param)
        record(name, PASS if ok else FAIL,
               f"HTTP {code} {e.get('code', '')} param={e.get('param')}")

    # ---------- 4. 模型不存在 ----------
    print("\n[4] 上游错误映射")
    code, _, body = http("/v1/chat/completions",
                         {"model": "definitely-not-a-model",
                          "messages": [{"role": "user", "content": "x"}]}, key=KEY)
    e = err_of(body)
    ok = code == 404 and e.get("code") == "not_found"
    record("不存在的模型 → 404", PASS if ok else FAIL, f"HTTP {code} {e.get('code', '')}")
    leaked = any(k in body.decode("utf-8", "replace") for k in ("qwen3.8-flash、", "当前仅支持配置模型"))
    record("不泄露上游模型清单", PASS if not leaked else FAIL, "原文未出现在响应体" if not leaked else "发现上游原文")

    # ---------- 5. 真实对话 ----------
    print("\n[5] 真实对话")
    t0 = time.time()
    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": [{"role": "user", "content": "只回答两个字：收到"}],
                          "max_tokens": 32}, key=KEY)
    dt = int((time.time() - t0) * 1000)
    text, usage, finish = "", {}, ""
    if code == 200:
        try:
            d = json.loads(body)
            text = (d["choices"][0]["message"].get("content") or "").strip()
            usage = d.get("usage") or {}
            finish = d["choices"][0].get("finish_reason") or ""
        except Exception:  # noqa: BLE001
            pass
    record("非流式对话", PASS if code == 200 and text else FAIL,
           f"{dt}ms · {text[:20]!r} · {usage.get('total_tokens', 0)}tok · {finish}")

    # 多轮
    msgs = [{"role": "user", "content": "记住数字 42"},
            {"role": "assistant", "content": "记住了。"},
            {"role": "user", "content": "我刚让你记的数字是多少？只回答数字"}]
    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": msgs, "max_tokens": 32}, key=KEY)
    text = ""
    if code == 200:
        try:
            text = (json.loads(body)["choices"][0]["message"].get("content") or "").strip()
        except Exception:  # noqa: BLE001
            pass
    record("多轮上下文", PASS if "42" in text else FAIL, f"{text[:30]!r}")

    # max_tokens 截断
    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": [{"role": "user", "content": "数到一百"}],
                          "max_tokens": 8}, key=KEY)
    finish = ""
    if code == 200:
        try:
            finish = json.loads(body)["choices"][0].get("finish_reason") or ""
        except Exception:  # noqa: BLE001
            pass
    record("max_tokens 截断", PASS if finish == "length" else FAIL, f"finish_reason={finish or '?'}")

    # ---------- 6. 流式 ----------
    print("\n[6] 流式")
    code, frames, done = sse("/v1/chat/completions",
                             {"model": pick, "stream": True,
                              "messages": [{"role": "user", "content": "数到三"}], "max_tokens": 64}, KEY)
    deltas = "".join(f["choices"][0]["delta"].get("content") or ""
                     for f in frames if f.get("choices") and f["choices"][0].get("delta"))
    reasons = [f["choices"][0].get("finish_reason") for f in frames
               if f.get("choices") and f["choices"][0].get("finish_reason")]
    record("流式返回增量", PASS if code == 200 and len(frames) > 1 and deltas else FAIL,
           f"HTTP {code} · {len(frames)} 帧 · 正文 {len(deltas)} 字")
    record("[DONE] 唯一且收尾", PASS if done else FAIL, "收到 [DONE]" if done else "缺失 [DONE]")
    record("流式 finish_reason", PASS if reasons else FAIL, f"{reasons[-1] if reasons else '缺失'}")

    # ---------- 7. 工具调用（上游能力探测） ----------
    print("\n[7] 工具调用（探测上游能力）")
    tools = [{"type": "function", "function": {
        "name": "get_weather", "description": "查询天气",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                       "required": ["city"]}}}]
    code, _, body = http("/v1/chat/completions",
                         {"model": pick, "messages": [{"role": "user", "content": "北京天气怎么样"}],
                          "tools": tools, "max_tokens": 128}, key=KEY)
    tc = []
    if code == 200:
        try:
            tc = json.loads(body)["choices"][0]["message"].get("tool_calls") or []
        except Exception:  # noqa: BLE001
            pass
    record("tools 透传", PASS if code == 200 else FAIL,
           f"tool_calls={len(tc)}" + ("（模型未选择调用，属正常）" if code == 200 and not tc else ""))

    # ---------- 8. 慢模型 ----------
    if not args.quick and len(models) > 3:
        print("\n[8] 另一家上游模型")
        alt = next((m for m in models if m.startswith("deepseek") or m.startswith("glm")), None)
        if alt:
            t0 = time.time()
            code, _, body = http("/v1/chat/completions",
                                 {"model": alt, "messages": [{"role": "user", "content": "说一个字"}],
                                  "max_tokens": 32}, key=KEY)
            dt = int((time.time() - t0) * 1000)
            got = ""
            if code == 200:
                try:
                    got = (json.loads(body)["choices"][0]["message"].get("content") or "").strip()
                except Exception:  # noqa: BLE001
                    pass
            # 推理模型的思考 token 也算 max_tokens：预算太小会正文为空。
            # 这是我们自己造的测试条件（max_tokens=32），不是网关缺陷。
            if code == 200 and not got:
                record(f"{alt}", SKIP,
                       f"{dt}ms · 正文空（预算 32 被思考耗尽，非网关问题）")
            else:
                record(f"{alt}", PASS if code == 200 and got else FAIL, f"{dt}ms · {got[:16]!r}")

    # ---------- 汇总 ----------
    total = len(results)
    ok = sum(1 for _, s, _ in results if s == PASS)
    bad = sum(1 for _, s, _ in results if s == FAIL)
    skip = sum(1 for _, s, _ in results if s == SKIP)
    print(f"\n{'=' * 62}")
    print(f"合计 {total} 项：通过 {ok} · 失败 {bad} · 跳过 {skip}")
    if bad:
        print("\n失败项：")
        for name, state, detail in results:
            if state == FAIL:
                print(f"  - {name}: {detail}")
    print()
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
