package upstream

import (
	"regexp"
	"strings"
	"sync"
)

// Registry 管理全部上游供应商：name → 共享连接池的 Client。
//
// 设计取舍：不引入 Provider interface。OpenAI 兼容协议 + Client 的通用错误
// 分类已经是抽象本身——凡是兼容 /chat/completions 的上游（Meoo、DeepSeek、
// 火山 Ark、Kimi…）接入只需要一个 base URL；协议不兼容的上游应先做适配层
// 把它变成 OpenAI 兼容，而不是在这里加第二种调用路径。
//
// 并发：Add 主要发生在启动期（env 解析），Get 在每请求热路径上，读锁足够。
type Registry struct {
	mu    sync.RWMutex
	items map[string]*Client
	def   string
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// NewRegistry 构造注册表并预置默认供应商（通常为 meoo）。
func NewRegistry(defName, defBase string) *Registry {
	r := &Registry{items: map[string]*Client{}, def: defName}
	r.Add(defName, defBase)
	return r
}

// Add 注册或替换一个供应商，返回其 Client。名字非法时静默忽略（返回 nil）。
func (r *Registry) Add(name, base string) *Client {
	name = NormalizeProvider(name)
	if name == "" || base == "" {
		return nil
	}
	if !nameRe.MatchString(name) {
		return nil
	}
	c := New(base)
	r.mu.Lock()
	r.items[name] = c
	if _, ok := r.items[r.def]; !ok {
		r.def = name
	}
	r.mu.Unlock()
	return c
}

// Get 按名取供应商；未知名回退默认。永不返回 nil（Registry 至少含默认项）。
func (r *Registry) Get(name string) *Client {
	name = NormalizeProvider(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.items[name]; ok {
		return c
	}
	return r.items[r.def]
}

// Has 判断名字是否已注册（用于面板校验，不回退）。
func (r *Registry) Has(name string) bool {
	name = NormalizeProvider(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.items[name]
	return ok
}

// Default 返回默认供应商名。
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.def
}

// Names 返回全部供应商名（排序，供面板与 /healthz）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.items))
	for n := range r.items {
		out = append(out, n)
	}
	for i := 1; i < len(out); i++ { // 插入排序，条目数个位数，不值得引 sort
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// NormalizeProvider 统一供应商名：小写、去空白。空串返回空串。
func NormalizeProvider(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
