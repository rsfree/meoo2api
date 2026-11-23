// Package catalog 同步上游模型目录，并对外输出 OpenAI 形态的模型卡片。
package catalog

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
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
	Label         string `json:"label,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Description   string `json:"description,omitempty"`
}

// Service 持有模型目录的内存缓存。
type Service struct {
	St     *store.Store
	Picker *pool.Picker
	Up     *upstream.Client

	// WebBase 是 Meoo 站点根（如 https://meoo.com）。非空时用它拉富元数据目录
	// （/api/v1/agent/models，项目 AK 可读）给兼容网关已上架的模型补字段。
	// 只补字段、不扩集合：站点目录里的 kimi/glm-5.3 等走的是另一条通道，兼容网关调不通。
	WebBase string
	// RefreshEvery 是目录自动刷新周期；0 表示默认 5 分钟。
	RefreshEvery time.Duration
	// Extra 是上游 /models 没报、但实测能调的模型 ID（如 glm-5 / MiniMax-M2.5）。
	// 必须经 ProbeExtra 验证回显一致才会并入目录 —— 上游对未知模型名会静默回落到
	// 默认模型，不验证就等于把「假可用」放进目录。
	Extra []string

	mu       sync.RWMutex
	models   []Model
	defaultM string
	fetched  time.Time
	stale    bool
	lastErr  string
	webOnly  []string // 站点目录有、兼容网关没有的模型（仅面板提示，不可调用）
	extraOK  []string // Extra 中验证通过（回显与请求一致）的模型
	metaOK   int      // 本轮成功补到元数据的条数
	metaAt   time.Time
	metaErr  string
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
		t := time.NewTicker(s.interval())
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

// interval 返回自动刷新周期：默认 5 分钟，可用 RefreshEvery 覆盖（最短 30 秒，
// 防止误配成 1 秒把上游打成 DDoS）。
func (s *Service) interval() time.Duration {
	if s.RefreshEvery >= 30*time.Second {
		return s.RefreshEvery
	}
	return 5 * time.Minute
}

// Refresh 拉取上游目录。成功则整体替换，失败保留旧目录并标记过期。
func (s *Service) Refresh(ctx context.Context) {
	if s.Picker == nil {
		return
	}
	// 最多试 3 个不同凭证，避免单 key 故障导致目录长期不更新。
	tried := map[string]bool{}
	for i := 0; i < 3; i++ {
		k := s.Picker.Pick(tried, model.DefaultProvider)
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
		ms = mergeExtra(ms, s.extraOK)
		s.models, s.defaultM, s.fetched, s.stale, s.lastErr = ms, def, time.Now(), false, ""
		s.mu.Unlock()
		// 元数据补全与主目录解耦：失败只记 metaErr，不影响可调用集合。
		s.enrich(ctx, k)
		return
	}
	s.mu.Lock()
	if len(s.models) > 0 {
		s.stale = true
	}
	s.mu.Unlock()
}

// mergeExtra 把已验证的补充模型并入目录，按 ID 去重（上游若开始自己上报，以原条目为准）。
func mergeExtra(base []Model, extra []string) []Model {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, m := range base {
		seen[m.ID] = true
	}
	out := base
	for _, id := range extra {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Model{ID: id, Object: "model", OwnedBy: "meoo-extra"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ProbeExtra 逐个验证 Extra 里的模型是否真的生效：
// 发一个 max_tokens=1 的请求，只有响应 model 与请求 id 完全一致才算通过。
//
// 这一步不能省。上游对未知模型名并不总是报错 —— 实测 kimi-k2.5、deepseek-v3.2
// 会直接静默回落到默认模型（qwen3.8-flash）并返回 200，客户端拿到的是另一个模型的
// 回答还以为一切正常。宁可目录里少一个，也不能放进来一个假的。
//
// 成本：每个候选一次 1 token 请求，只在启动与手动刷新时跑。
//
// 并发执行的理由：3 个模型串行时每个要 5~10 秒，实测吃掉 19 秒，会把调用方给的
// 启动预算整个耗光（主目录同步因此拿不到预算）。并发后总耗时 = 最慢的一个。
func (s *Service) ProbeExtra(ctx context.Context) {
	if len(s.Extra) == 0 || s.Picker == nil {
		return
	}
	type result struct {
		id     string
		ok     bool
		reason string
	}
	ch := make(chan result, len(s.Extra))
	sem := make(chan struct{}, 3) // 同时最多 3 个，别把上游打花
	var wg sync.WaitGroup
	for _, raw := range s.Extra {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, reason := s.probeOne(ctx, id)
			ch <- result{id: id, ok: ok, reason: reason}
		}(id)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	ok := make([]string, 0, len(s.Extra))
	for r := range ch {
		if r.ok {
			ok = append(ok, r.id)
			continue
		}
		log.Printf("补充模型 %s 验证失败：%s", r.id, r.reason)
	}
	s.mu.Lock()
	s.extraOK = ok
	s.mu.Unlock()
	if len(ok) > 0 {
		sort.Strings(ok)
		log.Printf("补充模型验证通过 %d/%d：%v", len(ok), len(s.Extra), ok)
	}
}

// probeOne 验证单个补充模型；返回失败原因便于日志定位。
func (s *Service) probeOne(ctx context.Context, id string) (bool, string) {
	k := s.Picker.Pick(nil, model.DefaultProvider)
	if k == nil {
		return false, "没有可用凭证"
	}
	body := map[string]any{
		"model": id, "messages": []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1, "stream": false,
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(cctx, "POST", s.Up.Base+"/chat/completions", body, upstream.MergeHeader(k.Key, nil))
	if err != nil || code != 200 {
		return false, describe(code, err)
	}
	var resp struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return false, "响应不是 JSON"
	}
	if resp.Model != id {
		return false, "上游静默回落到 " + resp.Model + "，不纳入目录"
	}
	return true, ""
}

// enrich 从 Meoo 站点目录补富元数据（context_window / max_tokens / multimodal / label / provider）。
//
// 为什么不把它直接当模型源：兼容网关对模型有硬白名单（实测 MODEL_NOT_SUPPORTED，
// 报错里直接列出 10 个可配置模型），站点目录里 kimi-k3、glm-5.3、minimax、auto 这些
// 在兼容网关调不通。列进 /v1/models 只会让客户端以为能用、一请求就 400。
// 所以这里的契约是「只补字段、不扩集合」，多出来的记进 webOnly 供面板提示。
func (s *Service) enrich(ctx context.Context, k *model.Key) {
	if s.WebBase == "" || k == nil {
		return
	}
	// 用 WithoutCancel：启动同步总共只有 15 秒预算，主目录拉完可能已所剩无几，
	// 元数据不该因此被砍掉而白等 5 分钟。停机时最多多拖 12 秒。
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	defer cancel()
	url := strings.TrimRight(s.WebBase, "/") + "/api/v1/agent/models"
	code, raw, err := s.Up.DoJSON(cctx, "GET", url, nil, upstream.MergeHeader(k.Key, nil))
	if err != nil || code != 200 {
		s.mu.Lock()
		s.metaErr = "站点目录 " + describe(code, err)
		s.mu.Unlock()
		return
	}
	web, _, perr := parse(raw)
	if perr != nil {
		s.mu.Lock()
		s.metaErr = "站点目录解析失败: " + perr.Error()
		s.mu.Unlock()
		return
	}
	byID := make(map[string]Model, len(web))
	for _, m := range web {
		byID[m.ID] = m
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	known := make(map[string]bool, len(s.models))
	filled := 0
	for i := range s.models {
		cur := &s.models[i]
		known[cur.ID] = true
		w, ok := byID[cur.ID]
		if !ok {
			continue
		}
		filled++
		// 逐字段「有值才覆盖」：站点目录给 false/空值时保留原值，避免元数据回退。
		if w.ContextWindow > 0 {
			cur.ContextWindow = w.ContextWindow
		}
		if w.MaxTokens > 0 {
			cur.MaxTokens = w.MaxTokens
		}
		if w.Label != "" {
			cur.Label = w.Label
		}
		if w.Provider != "" {
			cur.Provider = w.Provider
		}
		if w.Description != "" {
			cur.Description = w.Description
		}
		cur.Multimodal = cur.Multimodal || w.Multimodal
	}
	only := make([]string, 0, len(web))
	for _, m := range web {
		if !known[m.ID] {
			only = append(only, m.ID)
		}
	}
	sort.Strings(only)
	s.webOnly, s.metaOK, s.metaErr, s.metaAt = only, filled, "", time.Now()
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
		ContextWindow: m.ContextWindow, MaxTokens: maxTok, Multimodal: m.Multimodal,
		Label: m.Label, Provider: m.Provider, Description: m.Description}, true
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

// ModelCount 返回真实上游模型条数（不含本地别名）。
// 调用方判断「目录是否为空」必须用它：List 永远至少含别名。
func (s *Service) ModelCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.models)
}

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
				m.Label, m.Provider = b.Label, b.Provider
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
	var metaAt int64
	if !s.metaAt.IsZero() {
		metaAt = s.metaAt.UnixMilli()
	}
	// webOnly 是站点目录独有、兼容网关调不通的模型；面板据此提示，绝不并入 List。
	webOnly := append([]string(nil), s.webOnly...)
	return map[string]any{
		"count":         len(s.models),
		"default_model": s.defaultM,
		"fetched_at":    fetched,
		"source":        src,
		"error":         s.lastErr,
		"web_only":      webOnly,
		"extra_ok":      append([]string(nil), s.extraOK...),
		"meta_ok":       s.metaOK,
		"meta_at":       metaAt,
		"meta_error":    s.metaErr,
	}
}

// ProviderOf 返回某上游模型 ID 所属的供应商；目录里没有时回退默认。
// 这是「模型 → 上游」路由的唯一入口：别名请先过 Resolve 再进来。
func (s *Service) ProviderOf(resolved string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.models {
		if m.ID == resolved {
			if p := model.NormalizeProvider(m.Provider); p != "" {
				return p
			}
			break
		}
	}
	return model.DefaultProvider
}
