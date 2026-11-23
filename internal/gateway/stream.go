package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/upstream"
)

// proxyStream 把上游 SSE 逐帧转发给客户端。
//
// 两条写路径约束：
//   - 心跳与业务帧共用同一 goroutine（单写者），不会把一帧劈成两半；
//   - 「提前发响应头」与正常收尾共用同一个 sendHeader，保证 200 只写一次。
//
// 为什么要提前发头：上游在首字节前整段静默（实测 qwen3.8-max 首字节 P50 12s、
// 最坏 72s，且全程不发任何 SSE 注释帧），而响应头又被上游攒到首帧数据一起吐。
// 旧实现里客户端在这段时间既收不到响应头也收不到心跳，只能自行判超时挂断——
// 线上 499 全部落在这个窗口。代价是发头之后不能再换号，首帧错误只能以 error 帧告知，
// 所以用一个可配的 early_header_sec 把这段「用重试能力换可见性」的权衡交给运维。
func (h *Handler) proxyStream(ctx context.Context, w http.ResponseWriter, body map[string]any, key *model.Key,
	resolved string, cl caller, r *http.Request, started time.Time, retried bool) error {

	up := h.ClientFor(key)

	flusher, _ := w.(http.Flusher)
	h.St.Mu.RLock()
	keepSec := h.St.Data.Config.KeepaliveSec
	earlySec := h.St.Data.Config.EarlyHeaderSec
	h.St.Mu.RUnlock()

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
	writeKeepalive := func() error {
		if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// ticker 在函数级只建一次：「首字节前」与「转发中」两段等待共用，
	// 分开建会漏 Stop（旧实现只有主循环那一个）。
	var ticker *time.Ticker
	var tick <-chan time.Time
	if keepSec > 0 {
		ticker = time.NewTicker(time.Duration(keepSec) * time.Second)
		defer ticker.Stop()
		tick = ticker.C
	}

	// 上游请求必须放到 goroutine：否则主 goroutine 在 DoStream 里阻塞，
	// 首字节前那段静默期根本没有机会给客户端发东西。
	type upResult struct {
		resp *http.Response
		err  error
	}
	resCh := make(chan upResult, 1)
	go func() {
		resp, err := up.DoStream(ctx, up.Base+"/chat/completions", body, upstream.MergeHeader(key.Key, nil))
		resCh <- upResult{resp: resp, err: err}
	}()

	var early <-chan time.Time
	if earlySec > 0 {
		t := time.NewTimer(time.Duration(earlySec) * time.Second)
		defer t.Stop()
		early = t.C
	}

	var resp *http.Response
	var err error
	for resp == nil && err == nil {
		select {
		case res := <-resCh:
			resp, err = res.resp, res.err
		case <-early:
			// 只触发一次：到点先发头 + 起心跳，让客户端知道「服务活着」。
			early = nil
			sendHeader()
		case <-tick:
			// 头没发就写会隐式提交 200，把换号机会吃掉——这句判断是保住重试能力的关键。
			if !headerSent {
				continue
			}
			if werr := writeKeepalive(); werr != nil {
				ms := time.Since(started).Milliseconds()
				h.logEntry(r, cl, key, resolved, 499, true, started, ms, ms, nil, nil, retried)
				return nil
			}
		case <-ctx.Done():
			err = ctx.Err()
		}
	}

	if err != nil {
		ms := time.Since(started).Milliseconds()
		if !headerSent {
			// 一个字节都没写过，仍可换号：原样交给上层决定。
			h.logEntry(r, cl, key, resolved, statusOfErr(err), true, started, ms, ms, nil, err, retried)
			return err
		}
		// 头已发出，状态码改不了；用 error 帧收尾，客户端至少能拿到明确原因，
		// 而不是「200 然后连接静默断掉」。
		spec := specOf(err)
		_, _ = w.Write(sseErrorFrame(spec))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		h.logEntry(r, cl, key, resolved, spec.Status, true, started, ms, ms, nil, err, retried)
		return nil
	}
	defer resp.Body.Close()

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
	// 此时还没有写过任何业务帧——未发头就仍可换号重试，已发头只能以 error 帧收尾。
	var pending []string
	// fail 记一条日志再返回：首帧阶段失败时响应头还没发，chat() 若判定不可重试会
	// 直接 failErr，之前这些请求（内容过滤、首帧限流）在运行日志里完全不存在。
	fail := func(err error) error {
		ms := time.Since(started).Milliseconds()
		if headerSent {
			spec := specOf(err)
			_, _ = w.Write(sseErrorFrame(spec))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			h.logEntry(r, cl, key, resolved, spec.Status, true, started, ms, ms, nil, err, retried)
			return nil
		}
		h.logEntry(r, cl, key, resolved, statusOfErr(err), true, started, ms, ms, nil, err, retried)
		return err
	}
	for i := 0; i < 3; i++ {
		var ln line
		var ok bool
		select {
		case ln, ok = <-ch:
		case <-ctx.Done():
			return fail(ctx.Err())
		}
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

	var u usage
	var ttfb int64 = -1
	gotFinish := false
	seenDone := false
	contentLen, reasoningLen := 0, 0
	lastFinish := ""

	// closeOut 统一收尾日志：状态码必须是客户端真实收到/将要收到的语义。
	closeOut := func(status int, ue *upstream.Error) {
		ms := time.Since(started).Milliseconds()
		h.logEntry(r, cl, key, resolved, status, true, started, ttfbOrZero(ttfb, started), ms, u, ue, retried)
	}

	for {
		select {
		case <-ctx.Done():
			// 超时记 504、客户端断开记 499：两者若都折进 502 stream_interrupted，
			// 「上游卡死」和「客户端没耐心」在面板上就长得一模一样。
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				spec := specs["timeout"]
				_, _ = w.Write(sseErrorFrame(spec))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				closeOut(spec.Status, &upstream.Error{Kind: upstream.KindTimeout, Message: "网关等待上游超时"})
				return nil
			}
			h.Picker.MarkOK(key.ID, resolved, u.total)
			closeOut(499, nil)
			return nil
		case <-tick:
			if werr := writeKeepalive(); werr != nil {
				closeOut(499, nil)
				return nil
			}
		case ln, ok := <-ch:
			if !ok {
				h.Picker.MarkOK(key.ID, resolved, u.total)
				// 先判 ctx，再判「上游没给终态」：ctx 到点是超时（504），
				// 客户端断开是取消（499），只有上游自己结束却没给终态才是真正的 502。
				if cerr := ctx.Err(); cerr != nil {
					if errors.Is(cerr, context.DeadlineExceeded) {
						spec := specs["timeout"]
						_, _ = w.Write(sseErrorFrame(spec))
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						if flusher != nil {
							flusher.Flush()
						}
						closeOut(spec.Status, &upstream.Error{Kind: upstream.KindTimeout, Message: "网关等待上游超时"})
						return nil
					}
					closeOut(499, nil)
					return nil
				}
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
					closeOut(spec.Status, &upstream.Error{Kind: upstream.KindServer, Message: reason})
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
				// 读错误同样先判 ctx：上游读超时与客户端取消的处置完全不同。
				if cerr := ctx.Err(); cerr != nil {
					if errors.Is(cerr, context.DeadlineExceeded) {
						spec := specs["timeout"]
						_, _ = w.Write(sseErrorFrame(spec))
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						if flusher != nil {
							flusher.Flush()
						}
						closeOut(spec.Status, &upstream.Error{Kind: upstream.KindTimeout, Message: "网关等待上游超时"})
						return nil
					}
				}
				if statusOfErr(ln.err) == 499 {
					// 客户端断开导致的读错误：写帧也没人收，只记账。
					closeOut(499, nil)
					return nil
				}
				// 头已发出，状态码不可改：按契约补一帧公开 error，再 [DONE]。
				spec := specOf(ln.err)
				_, _ = w.Write(sseErrorFrame(spec))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				closeOut(spec.Status, asUpstreamErr(ln.err))
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

// asUpstreamErr 把任意读错误规整成 *upstream.Error，只用于日志。
func asUpstreamErr(err error) *upstream.Error {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue
	}
	return &upstream.Error{Kind: upstream.KindNetwork, Message: err.Error()}
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
