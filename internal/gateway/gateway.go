// Package gateway 提供 OpenAI 兼容前门：/v1/models 与 /v1/chat/completions。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/catalog"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// Handler 是 /v1 的入口。
type Handler struct {
	St      *store.Store
	Picker  *pool.Picker
	Catalog *catalog.Service
	Up      *upstream.Client
	Log     *reqlog.Log
}

// caller 描述调用方身份。
type caller struct {
	BYOK     string
	Label    string
	ClientID string
}

const maxBody = 16 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// failSpec 输出契约化错误：固定文案 + 白名单 param，绝不回显上游原文。
func failSpec(w http.ResponseWriter, key, param string) {
	spec, ok := specs[key]
	if !ok {
		spec = specs["unknown"]
	}
	writeSpec(w, spec, param)
}

// failErr 把任意失败映射到公开规格。
func failErr(w http.ResponseWriter, err error, param string) {
	writeSpec(w, specOf(err), param)
}

func (h *Handler) resolveCaller(r *http.Request) (caller, string) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return caller{}, "缺少 Bearer Authorization 头"
	}
	tok := parts[1]
	if model.IsProjectAK(tok) {
		return caller{BYOK: tok, Label: "BYOK"}, ""
	}
	if model.IsPersonalAK(tok) {
		return caller{}, "个人 AK 不能调用模型网关，请使用项目 AK（meoo_project_…）或面板签发的 sk-…"
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	for _, c := range h.St.Data.ClientKeys {
		if c.Key == tok && c.Enabled {
			return caller{ClientID: c.ID, Label: c.Label}, ""
		}
	}
	return caller{}, "无效的 API Key"
}

// ServeHTTP 路由 /v1/*。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/v1/models" || path == "/models":
		h.listModels(w, r)
	case strings.HasPrefix(path, "/v1/models/"):
		h.getModel(w, r, strings.TrimPrefix(path, "/v1/models/"))
	case path == "/v1/chat/completions" || path == "/chat/completions":
		h.chat(w, r)
	default:
		failSpec(w, "not_found", "")
	}
}

// modelCard 把目录条目转成 OpenAI 模型卡。列表与单查共用同一份，
// 避免出现「列表有元数据、单查没有」这种字段漂移。
func modelCard(m catalog.Model) map[string]any {
	item := map[string]any{
		"id": m.ID, "object": "model", "created": m.Created, "owned_by": m.OwnedBy,
	}
	if m.ContextWindow > 0 {
		item["context_window"] = m.ContextWindow
	}
	if m.MaxTokens > 0 {
		item["max_tokens"] = m.MaxTokens
	}
	if m.Multimodal {
		item["multimodal"] = true
	}
	if m.Label != "" {
		item["label"] = m.Label
	}
	if m.Provider != "" {
		item["provider"] = m.Provider
	}
	if m.Description != "" {
		item["description"] = m.Description
	}
	return item
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeSpecStatus(w, specs["invalid_request"], http.StatusMethodNotAllowed, "")
		return
	}
	if _, msg := h.resolveCaller(r); msg != "" {
		failSpec(w, "authentication", "")
		return
	}
	ms, stale, ferr := h.Catalog.List()
	// 启动竞态或首次同步失败时，别让客户端看到"只有别名"的假目录。
	if len(ms) == 0 {
		h.Catalog.Refresh(r.Context())
		ms, stale, ferr = h.Catalog.List()
	}
	data := make([]any, 0, len(ms))
	for _, m := range ms {
		data = append(data, modelCard(m))
	}
	w.Header().Set("X-Meoo-Models-Source", map[bool]string{true: "stale", false: "live"}[stale])
	if ferr != "" {
		w.Header().Set("X-Meoo-Models-Stale", "1")
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request, id string) {
	if _, msg := h.resolveCaller(r); msg != "" {
		failSpec(w, "authentication", "")
		return
	}
	ms, _, _ := h.Catalog.List()
	for _, m := range ms {
		if m.ID == id {
			writeJSON(w, http.StatusOK, modelCard(m))
			return
		}
	}
	failSpec(w, "not_found", "model")
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeSpecStatus(w, specs["invalid_request"], http.StatusMethodNotAllowed, "")
		return
	}
	cl, msg := h.resolveCaller(r)
	if msg != "" {
		failSpec(w, "authentication", "")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		failSpec(w, "invalid_request", "")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		failSpec(w, "invalid_request", "")
		return
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		failSpec(w, "invalid_request", "messages")
		return
	}
	// 核心不变量：每条消息必须是对象且 role 非空。
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			failSpec(w, "invalid_request", "messages")
			return
		}
		if role, _ := mm["role"].(string); strings.TrimSpace(role) == "" {
			failSpec(w, "invalid_request", "messages")
			return
		}
	}
	reqModel, _ := body["model"].(string)
	resolved := h.Catalog.Resolve(reqModel)
	stream, _ := body["stream"].(bool)

	h.St.Mu.RLock()
	defMax := h.St.Data.Config.DefaultMaxTokens
	timeoutSec := h.St.Data.Config.RequestTimeoutSec
	failover := h.St.Data.Config.Failover
	h.St.Mu.RUnlock()
	if timeoutSec <= 0 {
		timeoutSec = 180
	}

	out, err := shapeRequest(body, resolved, defMax, stream)
	if err != nil {
		failSpec(w, "unsupported_parameter", "n")
		return
	}

	var tried = map[string]bool{}
	attempts := 1
	if failover {
		attempts = 4
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		var key *model.Key
		if cl.BYOK != "" {
			key = &model.Key{ID: "byok", Key: cl.BYOK, Label: "BYOK"}
		} else {
			key = h.Picker.PickLeased(tried)
			if key == nil {
				if lastErr != nil {
					break
				}
				failSpec(w, "unavailable", "")
				return
			}
			tried[key.ID] = true
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second)
		if stream {
			err = h.proxyStream(ctx, w, out, key, resolved, cl, r, started, i > 0)
		} else {
			err = h.proxyJSON(ctx, w, out, key, resolved, cl, r, started, i > 0)
		}
		cancel()
		if key.ID != "byok" {
			h.Picker.Done(key.ID) // 归还在途名额，必须在冷却判定之前
		}
		if err == nil {
			return
		}
		lastErr = err
		ue, ok := err.(*upstream.Error)
		// 不可换号的错误（参数问题、BYOK、或响应头已发出）必须在这里收口：
		// 之前直接 return 会让客户端收到 200 + 空 body。
		if !ok || !ue.Retryable() || cl.BYOK != "" || !h.safeToRetry(w) {
			if h.safeToRetry(w) {
				failErr(w, lastErr, "")
			}
			return
		}
		if key.ID != "byok" {
			h.Picker.Cooldown(key.ID, ue)
		}
	}
	if lastErr != nil {
		// 上游原文只进内部日志（logEntry 已记录），公开体走固定文案表。
		failErr(w, lastErr, "")
	}
}

// safeToRetry 通过响应头是否已写出判断能否换号重试。
func (h *Handler) safeToRetry(w http.ResponseWriter) bool {
	if sw, ok := w.(interface{ Written() bool }); ok {
		return !sw.Written()
	}
	return true
}

// proxyJSON 处理非流式请求。
func (h *Handler) proxyJSON(ctx context.Context, w http.ResponseWriter, body map[string]any, key *model.Key,
	resolved string, cl caller, r *http.Request, started time.Time, retried bool) error {
	code, raw, err := h.Up.DoJSON(ctx, http.MethodPost, h.Up.Base+"/chat/completions", body, upstream.MergeHeader(key.Key, nil))
	if err != nil {
		h.logEntry(r, cl, key, resolved, 0, false, started, time.Since(started).Milliseconds(), time.Since(started).Milliseconds(), nil, err, retried)
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("上游返回 HTTP %d", code)
	}
	u := parseUsage(raw)
	h.Picker.MarkOK(key.ID, resolved, u.total)
	elapsed := time.Since(started).Milliseconds()
	h.logEntryNote(r, cl, key, resolved, 200, false, started, elapsed, elapsed, u, nil, retried, completionNote(raw))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	return nil
}

type usage struct{ prompt, completion, total int64 }

func parseUsage(raw []byte) usage {
	var v struct {
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &v) == nil {
		u := usage{prompt: v.Usage.Prompt, completion: v.Usage.Completion, total: v.Usage.Total}
		if u.total == 0 {
			u.total = u.prompt + u.completion
		}
		return u
	}
	return usage{}
}

func (h *Handler) logEntry(r *http.Request, cl caller, key *model.Key, mdl string, status int,
	stream bool, started time.Time, ttfb, dur int64, u any, err error, retried bool) {
	h.logEntryNote(r, cl, key, mdl, status, stream, started, ttfb, dur, u, err, retried, "")
}

// logEntryNote 允许附加一条"200 但可疑"的说明。
func (h *Handler) logEntryNote(r *http.Request, cl caller, key *model.Key, mdl string, status int,
	stream bool, started time.Time, ttfb, dur int64, u any, err error, retried bool, note string) {
	if h.Log == nil {
		return
	}
	var us usage
	if u != nil {
		us, _ = u.(usage)
	}
	e := reqlog.Entry{
		Time: started.UnixMilli(), ID: model.RandID("r_", 4), Model: mdl,
		KeyLabel: cl.Label, Stream: stream, Status: status, TTFB: ttfb, Dur: dur,
		Prompt: us.prompt, Compl: us.completion, Tokens: us.total,
		IP: clientIP(r), UA: truncate(r.UserAgent(), 120), Channel: "chat",
	}
	if cl.BYOK != "" {
		e.KeyLabel = "BYOK"
	}
	if key != nil {
		e.Account = key.Label
		if e.Account == "" {
			e.Account = key.ProjectID
		}
	}
	if err != nil {
		e.Err = err.Error()
	}
	e.Note = note
	if retried {
		e.Channel = "chat"
	}
	h.Log.Add(e)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// shapeRequest 做出站整形：别名落地、token 字段归一、零值不注入、n 只允许 1。
// 抽成纯函数便于回归测试。
func shapeRequest(body map[string]any, resolved string, defMax int, stream bool) (map[string]any, error) {
	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	out["model"] = resolved
	if v, ok := out["max_completion_tokens"]; ok {
		if _, has := out["max_tokens"]; !has {
			out["max_tokens"] = v
		}
		delete(out, "max_completion_tokens")
	}
	if _, ok := out["max_tokens"]; !ok && defMax > 0 {
		out["max_tokens"] = defMax
	}
	if n, ok := out["n"]; ok {
		switch f := n.(type) {
		case float64:
			if f != 1 {
				return nil, errNNotSupported
			}
		case int:
			if f != 1 {
				return nil, errNNotSupported
			}
		case json.Number:
			if f.String() != "1" {
				return nil, errNNotSupported
			}
		default:
			return nil, errNNotSupported
		}
	}
	if !stream {
		delete(out, "stream_options")
	}
	return out, nil
}

type gwErr string

func (e gwErr) Error() string { return string(e) }

const errNNotSupported = gwErr("当前实现仅支持 n=1")

// completionNote 识别"HTTP 200 但正文为空"的可疑形态。
// 最常见的原因：推理模型的思考 token 也算进 max_tokens，
// 预算给撑爆后 reasoning 写满、content 一个字都没来得及输出。
func completionNote(raw []byte) string {
	var v struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Choices) == 0 {
		return ""
	}
	c := v.Choices[0]
	if c.FinishReason == "length" && strings.TrimSpace(c.Message.Content) == "" && c.Message.ReasoningContent != "" {
		return "思考已耗尽 max_tokens，正文为空；推理模型请把预算调到 512 以上"
	}
	return ""
}
