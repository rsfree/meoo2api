// Package catalog 同步上游模型目录，并对外输出 OpenAI 形态的模型卡片。
package catalog

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// Model 是归一化后的模型条目。
type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
	Multimodal    bool   `json:"multimodal,omitempty"`
	Description   string `json:"description,omitempty"`
}

// Service 持有模型目录的内存缓存。
type Service struct {
	St     *store.Store
	Picker *pool.Picker
	Up     *upstream.Client

	mu       sync.RWMutex
	models   []Model
	defaultM string
	fetched  time.Time
	stale    bool
	lastErr  string
}

// New 构造目录服务。
func New(st *store.Store, pk *pool.Picker, up *upstream.Client) *Service {
	return &Service{St: st, Picker: pk, Up: up}
}

// Start 每 5 分钟刷新一次，直到 ctx 结束。
// 首次同步由调用方在启动时同步执行（StartupSync），避免监听后仍返回空目录。
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Refresh(ctx)
			}
		}
	}()
	return done
}

// Refresh 拉取上游目录。成功则整体替换，失败保留旧目录并标记过期。
func (s *Service) Refresh(ctx context.Context) {
	if s.Picker == nil {
		return
	}
	// 最多试 3 个不同凭证，避免单 key 故障导致目录长期不更新。
	tried := map[string]bool{}
	for i := 0; i < 3; i++ {
		k := s.Picker.Pick(tried)
		if k == nil {
			break
		}
		tried[k.ID] = true
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		code, raw, err := s.Up.DoJSON(cctx, "GET", s.Up.Base+"/models", nil, upstream.MergeHeader(k.Key, nil))
		cancel()
		if err != nil || code != 200 {
			if ue, ok := err.(*upstream.Error); ok && ue.Retryable() {
				s.Picker.Cooldown(k.ID, ue)
			}
			s.setErr(describe(code, err))
			continue
		}
		ms, def, perr := parse(raw)
		if perr != nil {
			s.setErr(perr.Error())
			continue
		}
		s.mu.Lock()
		s.models, s.defaultM, s.fetched, s.stale, s.lastErr = ms, def, time.Now(), false, ""
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if len(s.models) > 0 {
		s.stale = true
	}
	s.mu.Unlock()
}

func describe(code int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "模型目录 HTTP " + strconv.Itoa(code)
}

func (s *Service) setErr(msg string) {
	s.mu.Lock()
	s.lastErr = msg
	if len(s.models) > 0 {
		s.stale = true
	}
	s.mu.Unlock()
}

// mJSON 是一个模型条目的宽松形态，兼容 OpenAI data[] 与 Meoo models[] 两种字段命名。
type mJSON struct {
	ID            string `json:"id"`
	Key           string `json:"key"`
	Value         string `json:"value"`
	Label         string `json:"label"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	Provider      string `json:"provider"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	MaxToken      int    `json:"max_token"`
	Multimodal    bool   `json:"multimodal"`
	Description   string `json:"description"`
}

func (m mJSON) toModel() (Model, bool) {
	id := m.ID
	if id == "" {
		id = m.Value
	}
	if id == "" {
		id = m.Key
	}
	if id == "" {
		return Model{}, false
	}
	obj := m.Object
	if obj == "" {
		obj = "model"
	}
	owner := m.OwnedBy
	if owner == "" {
		owner = m.Provider
	}
	if owner == "" {
		owner = "meoo"
	}
	maxTok := m.MaxTokens
	if maxTok == 0 {
		maxTok = m.MaxToken
	}
	return Model{ID: id, Object: obj, Created: m.Created, OwnedBy: owner,
		ContextWindow: m.ContextWindow, MaxTokens: maxTok, Multimodal: m.Multimodal, Description: m.Description}, true
}

// parseModelList 把一段 JSON 解成模型条目列表。
// 单项可能是纯字符串（官方网关 `{"models":["qwen3.8-flash",…]}`）
// 或对象（OpenAI `{id,…}` / Web 端 `{key,value,…}`）。
func parseModelList(raw json.RawMessage) ([]mJSON, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]mJSON, 0, len(arr))
		for _, it := range arr {
			var s string
			if json.Unmarshal(it, &s) == nil {
				if s != "" {
					out = append(out, mJSON{ID: s})
				}
				continue
			}
			var m mJSON
			if json.Unmarshal(it, &m) == nil {
				out = append(out, m)
			}
		}
		return out, ""
	}
	// data 是对象：{"models":[…],"defaultModel":"…"}
	var obj struct {
		Models       []json.RawMessage `json:"models"`
		DefaultModel string            `json:"defaultModel"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil, ""
	}
	list, _ := parseModelList(mustMarshal(obj.Models))
	return list, obj.DefaultModel
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// parse 兼容四种上游形态：
//
//	{"data":[{id:…}]}                              OpenAI
//	{"data":["a","b"]}                             OpenAI 字符串数组
//	{"models":["a","b"],"defaultModel":"…"}        官方 Meoo 网关（实测）
//	{"data":{"models":[…]},"defaultModel":"…"}     Web 端 /api/v1/agent/models
func parse(raw []byte) ([]Model, string, error) {
	var root struct {
		Data         json.RawMessage `json:"data"`
		Models       json.RawMessage `json:"models"`
		DefaultModel string          `json:"defaultModel"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, "", err
	}
	items, def := parseModelList(root.Data)
	if def == "" {
		def = root.DefaultModel
	}
	if len(items) == 0 {
		var d2 string
		items, d2 = parseModelList(root.Models)
		if def == "" {
			def = d2
		}
	}
	out := make([]Model, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		m, ok := it.toModel()
		if !ok || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, "", errNoModels
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, def, nil
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNoModels = constErr("上游模型目录为空")

// List 返回对外可见的模型（应用别名与隐藏规则）。
func (s *Service) List() ([]Model, bool, string) {
	s.mu.RLock()
	base := append([]Model(nil), s.models...)
	stale, ferr := s.stale, s.lastErr
	s.mu.RUnlock()
	if s.St == nil {
		return base, stale, ferr
	}
	s.St.Mu.RLock()
	hidden := map[string]bool{}
	for _, h := range s.St.Data.Config.HiddenModels {
		hidden[h] = true
	}
	aliases := s.St.Data.Config.Aliases
	s.St.Mu.RUnlock()

	out := make([]Model, 0, len(base)+len(aliases))
	seen := map[string]bool{}
	for _, m := range base {
		if hidden[m.ID] || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	// 别名为独立条目，客户端可以直接用别名请求。
	names := make([]string, 0, len(aliases))
	for name, target := range aliases {
		if name == "" || hidden[name] {
			continue
		}
		if len(base) > 0 && !seen[target] {
			continue // 目标模型已下架，别名不再展示
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := Model{ID: name, Object: "model", OwnedBy: "meoo-alias", Description: "别名 → " + aliases[name]}
		for _, b := range base {
			if b.ID == aliases[name] {
				m.ContextWindow, m.MaxTokens, m.Multimodal = b.ContextWindow, b.MaxTokens, b.Multimodal
				break
			}
		}
		out = append(out, m)
	}
	return out, stale, ferr
}

// Resolve 把别名或原始 ID 解析成上游模型 ID。
func (s *Service) Resolve(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		s.mu.RLock()
		def := s.defaultM
		s.mu.RUnlock()
		if def != "" {
			return def
		}
		return "qwen3.8-flash"
	}
	if s.St == nil {
		return name
	}
	s.St.Mu.RLock()
	target, ok := s.St.Data.Config.Aliases[strings.ToLower(name)]
	s.St.Mu.RUnlock()
	if ok {
		return target
	}
	return name
}

// Status 给面板用。
func (s *Service) Status() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := "live"
	if s.stale {
		src = "stale"
	}
	var fetched int64
	if !s.fetched.IsZero() {
		fetched = s.fetched.UnixMilli()
	}
	return map[string]any{
		"count":         len(s.models),
		"default_model": s.defaultM,
		"fetched_at":    fetched,
		"source":        src,
		"error":         s.lastErr,
	}
}
