package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/upstream"
)

// proxyStream 把上游 SSE 逐帧转发给客户端，并在上游静默时补心跳。
// 心跳与业务帧共用同一写路径（单 goroutine 主循环），不会把一帧劈成两半。
func (h *Handler) proxyStream(ctx context.Context, w http.ResponseWriter, body map[string]any, key *model.Key,
	resolved string, cl caller, r *http.Request, started time.Time, retried bool) error {

	up := h.ClientFor(key)
	resp, err := up.DoStream(ctx, up.Base+"/chat/completions", body, upstream.MergeHeader(key.Key, nil))
	if err != nil {
		ms := time.Since(started).Milliseconds()
		h.logEntry(r, cl, key, resolved, statusOfErr(err), true, started, ms, ms, nil, err, retried)
		return err
	}
	defer resp.Body.Close()

	flusher, _ := w.(http.Flusher)
	h.St.Mu.RLock()
	keepSec := h.St.Data.Config.KeepaliveSec
	h.St.Mu.RUnlock()

	type line struct {
		text string
		err  error
	}
	ch := make(chan line, 32)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			select {
			case ch <- line{text: sc.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case ch <- line{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	// 首帧探测：上游可能在 HTTP 200 之后直接吐 {"error":…}（内容过滤最常见）。
	// 此时还没有写过任何字节，仍可换号重试。
	var pending []string
	headerSent := false
	sendHeader := func() {
		if headerSent {
			return
		}
		headerSent = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// fail 记一条日志再返回：首帧阶段失败时响应头还没发，chat() 若判定不可重试会
	// 直接 failErr，之前这些请求（内容过滤、首帧限流）在运行日志里完全不存在。
	fail := func(err error) error {
		ms := time.Since(started).Milliseconds()
		h.logEntry(r, cl, key, resolved, statusOfErr(err), true, started, ms, ms, nil, err, retried)
		return err
	}
	for i := 0; i < 3; i++ {
		ln, ok := <-ch
		if !ok {
			if len(pending) == 0 {
				return fail(&upstream.Error{Kind: upstream.KindServer, Message: "上游在首帧前关闭连接"})
			}
			break
		}
		if ln.err != nil {
			return fail(ln.err)
		}
		if e := streamErrorOf(ln.text); e != nil {
			return fail(e)
		}
		pending = append(pending, ln.text)
		if strings.HasPrefix(strings.TrimSpace(ln.text), "data:") {
			break
		}
	}
	sendHeader()
	for _, t := range pending {
		if _, werr := w.Write([]byte(t + "\n")); werr != nil {
			return nil
		}
	}
	if flusher != nil {
		flusher.Flush()
	}

	var tick <-chan time.Time
	if keepSec > 0 {
		t := time.NewTicker(time.Duration(keepSec) * time.Second)
		defer t.Stop()
		tick = t.C
	}
	var u usage
	var ttfb int64 = -1
	gotFinish := false
	seenDone := false
	contentLen, reasoningLen := 0, 0
	lastFinish := ""

	for {
		select {
		case <-r.Context().Done():
			// 客户端断开：直接结束，不把长度计入失败。
			h.Picker.MarkOK(key.ID, resolved, u.total)
			h.logEntry(r, cl, key, resolved, 499, true, started, ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, nil, retried)
			return nil
		case <-tick:
			if _, werr := w.Write([]byte(": keepalive\n\n")); werr != nil {
				return nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		case ln, ok := <-ch:
			if !ok {
				h.Picker.MarkOK(key.ID, resolved, u.total)
				if !gotFinish {
					// 上游没给 finish_reason。两种情况必须分开：
					//
					//  1) 收到了 [DONE] 且确实有输出 —— 上游认为这一轮结束了，只是没标
					//     终止原因（DeepSeek 系常见）。内容已经完整交付给客户端，此时再补
					//     一帧 502 会把一段可用的回答判死，客户端还会丢掉已收到的文本。
					//     24 小时日志里 8 次「流缺少语义终态」全部属于这一类。
					//
					//  2) 没有 [DONE]，或压根没吐出任何内容 —— 真中断，按契约报 502。
					if seenDone && (contentLen > 0 || reasoningLen > 0) {
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						if flusher != nil {
							flusher.Flush()
						}
						h.logEntryNote(r, cl, key, resolved, 200, true, started,
							ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, nil, retried,
							"上游未给 finish_reason，但有 [DONE] 与完整内容，已按正常结束收尾")
						return nil
					}
					spec := specs["stream_interrupted"]
					_, _ = w.Write(sseErrorFrame(spec))
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
					reason := "流缺少语义终态"
					if !seenDone {
						reason = "流在 [DONE] 前结束"
					} else if contentLen == 0 && reasoningLen == 0 {
						reason = "流无任何内容产出"
					}
					h.logEntry(r, cl, key, resolved, spec.Status, true, started, ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, &upstream.Error{Kind: upstream.KindServer, Message: reason}, retried)
					return nil
				}
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				note := ""
				if lastFinish == "length" && contentLen == 0 && reasoningLen > 0 {
					note = "思考已耗尽 max_tokens，正文为空；推理模型请把预算调到 512 以上"
				}
				h.logEntryNote(r, cl, key, resolved, 200, true, started, ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, nil, retried, note)
				return nil
			}
			if ln.err != nil {
				if statusOfErr(ln.err) == 499 {
					// 客户端断开导致的读错误：写帧也没人收，只记账。
					h.logEntry(r, cl, key, resolved, 499, true, started, ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, nil, retried)
					return nil
				}
				// 头已发出，状态码不可改：按契约补一帧公开 error，再 [DONE]。
				spec := specOf(ln.err)
				_, _ = w.Write(sseErrorFrame(spec))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				h.logEntry(r, cl, key, resolved, spec.Status, true, started, ttfbOrZero(ttfb, started), time.Since(started).Milliseconds(), u, ln.err, retried)
				return nil
			}
			if ttfb < 0 {
				ttfb = time.Since(started).Milliseconds()
			}
			// [DONE] 必须延迟到收尾再写，否则"没有 finish_reason 就不算成功"
			// 的契约会被一个提前发出的 [DONE] 破坏（客户端会看到两个 [DONE]）。
			if payload, ok := strings.CutPrefix(strings.TrimSpace(ln.text), "data:"); ok {
				payload = strings.TrimSpace(payload)
				if payload == "[DONE]" {
					seenDone = true
					continue
				}
				if payload != "" {
					uu, fin, cl, rl := scanChunk(payload)
					if uu.total > 0 || uu.prompt > 0 {
						u = uu
					}
					contentLen += cl
					reasoningLen += rl
					if fin != "" && fin != "null" {
						gotFinish = true
						lastFinish = fin
					}
				}
			}
			if _, werr := w.Write([]byte(ln.text + "\n")); werr != nil {
				return nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// streamErrorOf 识别"HTTP 200 但首帧就是 error 对象"的上游形态。
// 返回非 nil 表示这轮还没写过任何业务帧，调用方仍可换号。
func streamErrorOf(lineText string) *upstream.Error {
	t := strings.TrimSpace(lineText)
	payload, ok := strings.CutPrefix(t, "data:")
	if !ok {
		return nil
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var probe struct {
		Error  json.RawMessage `json:"error"`
		Object string          `json:"object"`
	}
	if json.Unmarshal([]byte(payload), &probe) != nil || len(probe.Error) == 0 {
		return nil
	}
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	_ = json.Unmarshal(probe.Error, &e)
	code := e.Type
	switch c := e.Code.(type) {
	case string:
		if c != "" {
			code = c
		}
	case float64:
		code = fmt.Sprintf("%v", c)
	}
	// 首帧 error 的处置动作要与普通上游错误一致：限流/额度/过载要能换号，
	// 参数错误才直接返回。硬编码 KindBadRequest 会让可恢复的错误一张口就死。
	return &upstream.Error{Status: http.StatusOK, Code: code, Message: e.Message,
		Kind: upstream.ClassifyBusiness(code, e.Message)}
}

// scanChunk 从单个 SSE data 帧里取 usage 与 finish_reason。
// scanChunk 解析一帧：用量、终止原因，以及正文/思考的增量长度。
// 长度用于识别"预算全烧在思考上、正文为空"的可疑形态。
func scanChunk(payload string) (usage, string, int, int) {
	var v struct {
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			FinishReason any `json:"finish_reason"`
			Delta        struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	u := usage{}
	if json.Unmarshal([]byte(payload), &v) != nil {
		return u, "", 0, 0
	}
	u = usage{prompt: v.Usage.Prompt, completion: v.Usage.Completion, total: v.Usage.Total}
	if u.total == 0 {
		u.total = u.prompt + u.completion
	}
	fin := ""
	contentLen, reasoningLen := 0, 0
	for _, c := range v.Choices {
		if s, ok := c.FinishReason.(string); ok && s != "" {
			fin = s
		}
		contentLen += len(c.Delta.Content)
		reasoningLen += len(c.Delta.ReasoningContent)
	}
	return u, fin, contentLen, reasoningLen
}

var _ = reqlog.Entry{}

// ttfbOrZero 把"首帧还没到"归一成整段耗时（客户端至少等到了响应头）。
func ttfbOrZero(ttfb int64, started time.Time) int64 {
	if ttfb >= 0 {
		return ttfb
	}
	return time.Since(started).Milliseconds()
}
