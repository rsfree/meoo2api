#!/usr/bin/env python3
"""meoo2api 深度探测：模型可用性 / 多轮上下文 / 上下文上限 / 并发频控。

用法：
    python3 scripts/probe.py --key sk-xxx --stage models
    python3 scripts/probe.py --key sk-xxx --stage multi
    python3 scripts/probe.py --key sk-xxx --stage context --targets 5000,20000,60000
    python3 scripts/probe.py --key sk-xxx --stage load --levels 3,6,12

注意：load 阶段可能触发上游限流，网关会冷却凭证，建议最后跑。
"""
from __future__ import annotations

import argparse
import json
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
BASE = "http://127.0.0.1:8791"
KEY = ""


def post(path: str, payload: dict, timeout: int = 300):
    req = urllib.request.Request(BASE + path, data=json.dumps(payload).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + KEY)
    t0 = time.time()
    try:
        with OPENER.open(req, timeout=timeout) as r:
            return r.status, json.loads(r.read()), int((time.time() - t0) * 1000)
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            return e.code, json.loads(body), int((time.time() - t0) * 1000)
        except Exception:  # noqa: BLE001
            return e.code, {"raw": body.decode("utf-8", "replace")[:200]}, int((time.time() - t0) * 1000)
    except Exception as e:  # noqa: BLE001
        return 0, {"error": str(e)}, int((time.time() - t0) * 1000)


def chat(model: str, messages: list, max_tokens: int = 64, **kw):
    payload = {"model": model, "messages": messages, "max_tokens": max_tokens}
    payload.update(kw)
    return post("/v1/chat/completions", payload)


def brief(resp: dict) -> tuple[str, str, dict]:
    """返回 (正文, finish_reason, usage)。"""
    try:
        c = resp["choices"][0]
        return (c["message"].get("content") or "").strip(), c.get("finish_reason") or "", resp.get("usage") or {}
    except Exception:  # noqa: BLE001
        return "", "", resp.get("usage") or {}


# ---------------- 1. 模型可用性 ----------------

def stage_models(models: list[str]) -> None:
    print(f"\n{'模型':<26}{'状态':<8}{'延迟':>8}{'tokens':>9}  回复/错误")
    print("-" * 96)
    ok = 0
    for m in models:
        code, resp, ms = chat(m, [{"role": "user", "content": "只回答两个字：可用"}], max_tokens=64)
        text, fin, usage = brief(resp)
        if code == 200 and text:
            ok += 1
            note = f"{text[:24]!r} {fin}"
        elif code == 200 and not text:
            note = f"[空正文] finish={fin} reasoning={bool(resp['choices'][0]['message'].get('reasoning_content'))}"
        else:
            e = (resp.get("error") or {})
            note = f"HTTP {code} {e.get('code', '')} {e.get('message', '')[:40]}"
        print(f"{m:<26}{code:<8}{ms:>7}ms{usage.get('total_tokens', 0):>9}  {note}")
    print(f"\n可用 {ok}/{len(models)}")


# ---------------- 2. 多轮上下文 ----------------

def stage_multi(model: str, rounds: int = 8) -> None:
    print(f"\n多轮上下文测试（{model}，{rounds} 轮）")
    print("-" * 96)
    facts = [f"暗号{i}是{1000 + i}" for i in range(rounds)]
    msgs: list[dict] = []
    ok = 0
    for i, fact in enumerate(facts):
        msgs.append({"role": "user", "content": f"记住：{fact}。只回复 OK"})
        code, resp, ms = chat(model, msgs, max_tokens=128)
        text, fin, usage = brief(resp)
        if code != 200:
            print(f"  第 {i + 1} 轮失败 HTTP {code} {resp.get('error', {}).get('code', '')}")
            return
        msgs.append({"role": "assistant", "content": text or "OK"})
        prompt_tok = usage.get("prompt_tokens", 0)
        print(f"  第 {i + 1} 轮  {ms:>5}ms  prompt={prompt_tok:<6} 回复={text[:16]!r}")
        ok += 1

    # 回问最早的两条
    msgs.append({"role": "user", "content": "暗号0和暗号1分别是什么？只回答两个数字，用逗号分隔"})
    code, resp, ms = chat(model, msgs, max_tokens=64)
    text, fin, usage = brief(resp)
    hit0 = "1000" in text
    hit1 = "1001" in text
    print(f"\n  回问最早两轮：{text[:40]!r}")
    print(f"  命中暗号0={hit0} 暗号1={hit1} · prompt={usage.get('prompt_tokens', 0)} tok · {ms}ms")
    print(f"  结论：{'上下文确实延续' if hit0 or hit1 else '未能召回早期内容'}")


# ---------------- 3. 上下文上限 ----------------

def stage_context(model: str, targets: list[int]) -> None:
    print(f"\n上下文上限探测（{model}）")
    print("-" * 96)
    # 校准：1 个中文字符大致对应多少 prompt token
    filler = "天地玄黄宇宙洪荒" * 100  # 800 字
    code, resp, ms = chat(model, [{"role": "user", "content": filler + "\n只回答两个字：收到"}], max_tokens=16)
    if code != 200:
        print(f"  校准失败 HTTP {code}")
        return
    _, _, usage = brief(resp)
    base_tokens = usage.get("prompt_tokens", 0)
    chars = len(filler)
    per_char = base_tokens / chars if chars else 1.0
    print(f"  校准：{chars} 字 → {base_tokens} prompt tokens（约 {per_char:.2f} tok/字）")

    for target in targets:
        need = int(target / per_char) if per_char else target
        text = ("天地玄黄宇宙洪荒" * ((need // 8) + 1))[:need]
        payload = [{"role": "user", "content": text + "\n数一下上面大概多少字，只回答一个数字"}]
        code, resp, ms = chat(model, payload, max_tokens=32, timeout=600)
        _, fin, usage = brief(resp)
        pt = usage.get("prompt_tokens", 0)
        if code == 200:
            print(f"  目标 {target:>7} tok  实际 {pt:>7}  prompt  {ms:>6}ms  finish={fin or '?'}  通过")
        else:
            e = resp.get("error") or {}
            print(f"  目标 {target:>7} tok  实际 {pt:>7}  HTTP {code}  {e.get('code', '')}  {e.get('message', '')[:40]}  ← 上限")
            break


# ---------------- 4. 并发与频控 ----------------

def stage_load(model: str, levels: list[int]) -> None:
    print(f"\n并发与频控（{model}）")
    print("-" * 96)
    print(f"{'并发':<6}{'成功':<7}{'失败':<7}{'总耗时':>8}{'P50':>8}{'P95':>8}  错误分布")
    print("-" * 96)
    for n in levels:
        results: list[tuple[int, int, str]] = []
        lock = threading.Lock()
        barrier = threading.Barrier(n)

        def worker() -> None:
            barrier.wait()
            code, resp, ms = chat(model, [{"role": "user", "content": "只回答一个字：好"}], max_tokens=32, timeout=300)
            e = (resp.get("error") or {}).get("code", "")
            with lock:
                results.append((code, ms, e))

        t0 = time.time()
        threads = [threading.Thread(target=worker) for _ in range(n)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        wall = int((time.time() - t0) * 1000)

        lat = sorted(ms for code, ms, _ in results if code == 200)
        okc = sum(1 for code, _, _ in results if code == 200 and code != 0)
        bad = len(results) - okc
        p50 = statistics.median(lat) if lat else 0
        p95 = lat[int(len(lat) * 0.95) - 1] if len(lat) >= 2 else (lat[-1] if lat else 0)
        errs: dict[str, int] = {}
        for code, _, e in results:
            if code != 200:
                errs[f"{code}/{e or '?'}"] = errs.get(f"{code}/{e or '?'}", 0) + 1
        print(f"{n:<6}{okc:<7}{bad:<7}{wall:>7}ms{p50:>7}ms{p95:>7}ms  {errs or '—'}")

        # 出现鉴权/额度类失败就没必要继续加压
        if any(code in (401, 403, 429) for code, _, _ in results):
            print("  ↑ 出现限流/额度类失败，停止加压（避免把凭证打进长冷却）")
            break


def main() -> int:
    global BASE, KEY
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8791")
    ap.add_argument("--key", required=True)
    ap.add_argument("--stage", required=True, choices=["models", "multi", "context", "load"])
    ap.add_argument("--model", default="qwen3.8-flash")
    ap.add_argument("--models", default="", help="逗号分隔，默认取 /v1/models 全部")
    ap.add_argument("--rounds", type=int, default=8)
    ap.add_argument("--targets", default="5000,20000,60000")
    ap.add_argument("--levels", default="3,6,12")
    args = ap.parse_args()
    BASE, KEY = args.base.rstrip("/"), args.key

    if args.stage == "models":
        models = [m.strip() for m in args.models.split(",") if m.strip()]
        if not models:
            req = urllib.request.Request(BASE + "/v1/models")
            req.add_header("Authorization", "Bearer " + KEY)
            with OPENER.open(req, timeout=30) as r:
                models = [m["id"] for m in json.loads(r.read())["data"]]
        stage_models(models)
    elif args.stage == "multi":
        stage_multi(args.model, args.rounds)
    elif args.stage == "context":
        stage_context(args.model, [int(x) for x in args.targets.split(",") if x.strip()])
    elif args.stage == "load":
        stage_load(args.model, [int(x) for x in args.levels.split(",") if x.strip()])
    return 0


if __name__ == "__main__":
    sys.exit(main())
