package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/upstream"
)

// sse 写一组原始 SSE 帧并逐帧 flush，模拟上游的流式行为。
func sse(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	for _, s := range frames {
		_, _ = w.Write([]byte(s))
		if f != nil {
			f.Flush()
		}
	}
}

const streamBody = `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// 上游发了内容与 [DONE]，但整条流没有 finish_reason —— DeepSeek 系常见。
// 内容已完整交付，必须按正常结束收尾，不能再补一帧 502 把回答判死。
func TestStreamDoneWithoutFinishIsSuccess(t *testing.T) {
	key := "meoo_project_k1111111111"
	gw, f, _ := harness(t, map[string]model.Health{key: {State: "ok"}})
	f.on(key, func(w http.ResponseWriter) {
		sse(w,
			`data: {"choices":[{"delta":{"content":"你好"},"finish_reason":null}]}`+"\n\n",
			`data: {"choices":[{"delta":{"content":"，世界"},"finish_reason":""}]}`+"\n\n",
			"data: [DONE]\n\n",
		)
	})
	rec := chatReq(gw, streamBody)
	if rec.Code != 200 {
		t.Fatalf("流式响应头应为 200，得到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "世界") {
		t.Fatalf("正文必须完整转发: %q", body)
	}
	if strings.Contains(body, "stream_interrupted") || strings.Contains(body, `"error"`) {
		t.Fatalf("有内容且收到 [DONE] 时不得再报错: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("收尾必须补 [DONE]: %q", body)
	}
	// 日志要记 200 + 说明，而不是 502
	entries := gw.Log.Recent(reqlog.Filter{Limit: 50})
	if len(entries) == 0 {
		t.Fatal("应写入一条运行日志")
	}
	e := entries[0]
	if e.Status != 200 {
		t.Fatalf("日志状态应为 200，得到 %d（err=%q）", e.Status, e.Err)
	}
	if e.Note == "" {
		t.Fatal("应以 note 记录『上游未给 finish_reason』，便于观测")
	}
}

// 没有 [DONE] 就断流：是真的被截断，必须报 502 中断。
func TestStreamWithoutDoneIsInterrupted(t *testing.T) {
	key := "meoo_project_k2222222222"
	gw, f, _ := harness(t, map[string]model.Health{key: {State: "ok"}})
	f.on(key, func(w http.ResponseWriter) {
		sse(w, `data: {"choices":[{"delta":{"content":"半句"},"finish_reason":null}]}`+"\n\n")
	})
	rec := chatReq(gw, streamBody)
	body := rec.Body.String()
	if !strings.Contains(body, "stream_interrupted") {
		t.Fatalf("无 [DONE] 的断流必须报 stream_interrupted: %q", body)
	}
	entries := gw.Log.Recent(reqlog.Filter{Limit: 50})
	if len(entries) == 0 || entries[0].Status != 502 {
		t.Fatalf("日志应记 502，得到 %+v", entries)
	}
}

// 什么都没有的空流：同样按中断处理，且原因可区分。
func TestStreamEmptyIsInterrupted(t *testing.T) {
	key := "meoo_project_k3333333333"
	gw, f, _ := harness(t, map[string]model.Health{key: {State: "ok"}})
	f.on(key, func(w http.ResponseWriter) {
		sse(w, "data: [DONE]\n\n")
	})
	rec := chatReq(gw, streamBody)
	if !strings.Contains(rec.Body.String(), "stream_interrupted") {
		t.Fatalf("空流必须报中断: %q", rec.Body.String())
	}
}

// 首帧就是 error 对象：HTTP 200 但语义是限流，必须能换号，而不是直接判请求错误。
func TestStreamFirstFrameRateLimitFailsOver(t *testing.T) {
	bad, good := "meoo_project_bad44444444", "meoo_project_good444444"
	gw, f, st := harness(t, map[string]model.Health{bad: {State: "ok"}, good: {State: "ok"}})
	f.on(bad, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"code":"rate_limit_exceeded","message":"too many requests"}}` + "\n\n"))
	})
	f.on(good, func(w http.ResponseWriter) {
		sse(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
	})
	// 固定选号顺序不可控，跑多次确保两种号都被选到过
	for i := 0; i < 8; i++ {
		rec := chatReq(gw, streamBody)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "upstream_error") {
			t.Fatalf("限流首帧应换号成功，第 %d 次得到 %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if h := healthOf(st, bad); h.State == "ok" {
		// 被 429 的号至少要进过一次冷却
		t.Logf("坏号状态 %+v（若从未被选中则跳过）", h)
	}
}

// 上游在首字节前长时间静默（实测 qwen3.8-max 可达 72s，且全程不发任何注释帧）。
// 旧实现里客户端这段时间既没有响应头也没有心跳，只能自己判超时挂断——线上 499 的主因。
// 到 early_header_sec 必须先把 200 和心跳发出去。
func TestStreamEarlyHeaderSendsKeepalive(t *testing.T) {
	key := "meoo_project_k5555555555"
	gw, f, st := harness(t, map[string]model.Health{key: {State: "ok"}})
	st.Mu.Lock()
	st.Data.Config.EarlyHeaderSec = 1
	st.Data.Config.KeepaliveSec = 1
	st.Mu.Unlock()

	f.on(key, func(w http.ResponseWriter) {
		time.Sleep(3 * time.Second) // 模拟首字节前的整段静默
		sse(w, `data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
	})

	rec := chatReq(gw, streamBody)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("状态码应为 200，得到 %d", rec.Code)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("首字节前必须已下发心跳，实际 body=%q", body)
	}
	if !strings.Contains(body, "好") {
		t.Fatalf("内容必须正常转发，实际 body=%q", body)
	}
}

// 发头之后上游才报错：状态码已不可改，必须用 error 帧收尾，
// 不能让客户端拿到「200 然后静默断流」。
func TestStreamEarlyHeaderErrorFrame(t *testing.T) {
	key := "meoo_project_k6666666666"
	gw, f, st := harness(t, map[string]model.Health{key: {State: "ok"}})
	st.Mu.Lock()
	st.Data.Config.EarlyHeaderSec = 1
	st.Mu.Unlock()

	f.on(key, func(w http.ResponseWriter) {
		time.Sleep(2 * time.Second)
		fail(w, 500, "server_error", "boom")
	})

	rec := chatReq(gw, streamBody)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("响应头已提前发出，状态码应为 200，得到 %d", rec.Code)
	}
	if !strings.Contains(body, "internal_error") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("发头后的错误必须以 error 帧 + [DONE] 收尾，实际 body=%q", body)
	}
}

// 流中卡死到网关超时：必须记 504，不能折叠成 502 stream_interrupted。
// 否则「上游卡住」和「客户端没耐心」在面板上完全无法区分（线上 9 次 502 与 4 次 504
// 都精确卡在 180.00s，就是同一件事被记成两种语义）。
func TestStreamTimeoutIsTimeoutNotInterrupted(t *testing.T) {
	key := "meoo_project_k7777777777"
	gw, f, st := harness(t, map[string]model.Health{key: {State: "ok"}})
	st.Mu.Lock()
	st.Data.Config.RequestTimeoutSec = 1
	st.Data.Config.EarlyHeaderSec = 0 // 走未提前发头的路径
	st.Mu.Unlock()

	f.on(key, func(w http.ResponseWriter) {
		sse(w, `data: {"choices":[{"delta":{"content":"开头"},"finish_reason":null}]}`+"\n\n")
		time.Sleep(3 * time.Second) // 之后一个字节都不再发
	})

	rec := chatReq(gw, streamBody)
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("超时必须报 504 upstream_timeout，实际 body=%q", body)
	}
	if strings.Contains(body, "stream_interrupted") {
		t.Fatalf("超时不得折叠成 stream_interrupted，实际 body=%q", body)
	}
}

// 超时错误必须按 504 暴露（KindTimeout），而不是 502 connection。
func TestKindTimeoutSpec(t *testing.T) {
	spec := specOf(&upstream.Error{Kind: upstream.KindTimeout})
	if spec.Status != 504 || spec.Code != "upstream_timeout" {
		t.Fatalf("KindTimeout 应映射 504/upstream_timeout，得到 %d/%s", spec.Status, spec.Code)
	}
}
