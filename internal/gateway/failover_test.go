package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/catalog"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// fakeUpstream 按 Bearer 决定行为，用来验证选号/冷却/分流。
type fakeUpstream struct {
	srv    *httptest.Server
	hits   map[string]*int32
	routes map[string]func(w http.ResponseWriter)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{hits: map[string]*int32{}, routes: map[string]func(http.ResponseWriter){}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":["test-model"],"defaultModel":"test-model"}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, ok := f.hits[key]; ok {
			atomic.AddInt32(c, 1)
		}
		if h, ok := f.routes[key]; ok && h != nil {
			h(w)
			return
		}
		ok200(w, "ok")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// on 注册某凭证的行为；h 为 nil 时只初始化计数器。
func (f *fakeUpstream) on(key string, h func(http.ResponseWriter)) {
	c := int32(0)
	if _, exists := f.hits[key]; !exists {
		f.hits[key] = &c
	}
	if h != nil {
		f.routes[key] = h
	}
}

func (f *fakeUpstream) count(key string) int32 {
	if c, ok := f.hits[key]; ok {
		return atomic.LoadInt32(c)
	}
	return 0
}

func ok200(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion", "model": "test-model",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": "x"}})
}

// harness 组装一个指向假上游的网关。
func harness(t *testing.T, keys map[string]model.Health) (*Handler, *fakeUpstream, *store.Store) {
	t.Helper()
	f := newFakeUpstream(t)
	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.Mu.Lock()
	st.Data.Accounts = append(st.Data.Accounts, &model.Account{ID: "a1", UserID: "u1", Enabled: true})
	i := 0
	for k, h := range keys {
		i++
		st.Data.Keys = append(st.Data.Keys, &model.Key{
			ID: "k" + string(rune('0'+i)), Key: k, AccountID: "a1", Enabled: true, Health: h,
		})
		f.on(k, nil)
	}
	st.Data.ClientKeys = append(st.Data.ClientKeys, &model.ClientKey{
		ID: "c1", Key: "sk-test", Label: "test", Enabled: true,
	})
	st.Mu.Unlock()

	up := upstream.New(f.srv.URL + "/v1")
	pk := &pool.Picker{St: st, Up: up}
	cat := catalog.New(st, pk, up)
	return &Handler{St: st, Picker: pk, Catalog: cat, Up: up}, f, st
}

func chatReq(gw *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func healthOf(st *store.Store, key string) model.Health {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, k := range st.Data.Keys {
		if k.Key == key {
			return k.Health
		}
	}
	return model.Health{}
}

const body1 = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

// 429 只冷却该凭证，且后续请求不再选它（有备用号时直接分流）。
func TestRateLimitCoolsAndFailsOver(t *testing.T) {
	bad, good := "meoo_project_bad1111111", "meoo_project_good111111"
	gw, f, st := harness(t, map[string]model.Health{
		bad:  {State: "ok"},
		good: {State: "ok"},
	})
	f.on(bad, func(w http.ResponseWriter) { fail(w, 429, "rate_limit_exceeded", "slow down") })

	// 连发 6 次：坏号最多被打一次（冷却后不再选），全部请求应成功。
	for i := 0; i < 6; i++ {
		rec := chatReq(gw, body1)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次请求应成功，得到 %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := f.count(bad); got > 1 {
		t.Fatalf("被 429 的凭证应冷却后不再使用，实际被打了 %d 次", got)
	}
	if h := healthOf(st, bad); h.State != "cooldown" && h.State != "ok" {
		t.Fatalf("状态异常: %+v", h)
	}
}

// 401 直接标 invalid（不再参与选号）。
func TestAuthErrorInvalidatesKey(t *testing.T) {
	bad := "meoo_project_bad1111111"
	gw, f, st := harness(t, map[string]model.Health{bad: {State: "ok"}})
	f.on(bad, func(w http.ResponseWriter) { fail(w, 401, "invalid_api_key", "bad token") })

	rec := chatReq(gw, body1)
	if rec.Code != 401 {
		t.Fatalf("单号失效应返回 401，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if h := healthOf(st, bad); h.State != "invalid" {
		t.Fatalf("401 应标记 invalid，实际 %+v", h)
	}
	// 第二个请求：无可用号
	rec = chatReq(gw, body1)
	if rec.Code != 503 {
		t.Fatalf("无可用凭证应 503，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Fatalf("503 应映射 service_unavailable: %s", rec.Body.String())
	}
}

// 400 是请求本身的问题：不冷却、不换号、直接回给客户端。
func TestBadRequestDoesNotCoolOrFailover(t *testing.T) {
	bad := "meoo_project_bad1111111"
	gw, f, st := harness(t, map[string]model.Health{bad: {State: "ok"}})
	f.on(bad, func(w http.ResponseWriter) { fail(w, 400, "invalid_request", "bad param") })

	rec := chatReq(gw, body1)
	if rec.Code != 400 {
		t.Fatalf("400 应原样返回，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if h := healthOf(st, bad); h.State == "cooldown" || h.State == "invalid" {
		t.Fatalf("参数错误不应冷却凭证，实际 %+v", h)
	}
	// 参数错误换号只会重复计费：4 次客户端请求必须只打 4 次上游
	before := f.count(bad)
	for i := 0; i < 3; i++ {
		if c := chatReq(gw, body1).Code; c != 400 {
			t.Fatalf("第 %d 次仍应 400，得到 %d", i+2, c)
		}
	}
	if delta := f.count(bad) - before; delta != 3 {
		t.Fatalf("400 触发了额外重试：3 次客户端请求打了 %d 次上游", delta)
	}
}

// 全部不可用 → 503，且不逐个打上游。
func TestAllCoolingReturns503WithoutHittingUpstream(t *testing.T) {
	k1 := "meoo_project_a1111111111"
	gw, f, st := harness(t, map[string]model.Health{
		k1: {State: "cooldown", Until: model.NowMilli() + 60_000},
	})
	rec := chatReq(gw, body1)
	if rec.Code != 503 {
		t.Fatalf("全冷却应 503，得到 %d", rec.Code)
	}
	if f.count(k1) != 0 {
		t.Fatal("冷却中的凭证不应被调用")
	}
	_ = st
}

// 并发下同一个 429 号不会被反复选中（冷却写回要及时）。
func TestConcurrentRateLimitDoesNotStampede(t *testing.T) {
	bad, good := "meoo_project_bad1111111", "meoo_project_good111111"
	gw, f, _ := harness(t, map[string]model.Health{bad: {State: "ok"}, good: {State: "ok"}})
	f.on(bad, func(w http.ResponseWriter) { fail(w, 429, "rate_limit_exceeded", "slow down") })

	done := make(chan int, 24)
	for i := 0; i < 24; i++ {
		go func() {
			rec := chatReq(gw, body1)
			done <- rec.Code
		}()
	}
	ok := 0
	for i := 0; i < 24; i++ {
		if <-done == 200 {
			ok++
		}
	}
	if ok == 0 {
		t.Fatal("并发下应至少有一个请求成功（好号可用）")
	}
	// 24 个请求同时出发时，429 的反馈还没回来，先飞出去的那批必然打到坏号。
	// 完全均分是 12 次；只要不超过"均分 + 余量"就说明在途计数起到了分摊作用。
	if got := f.count(bad); got > 14 {
		t.Fatalf("并发下坏号被打了 %d 次（均分约 12），分摊未生效", got)
	}
	// 冷却写回之后再打一轮：坏号必须一次都不被打。
	time.Sleep(150 * time.Millisecond)
	before := f.count(bad)
	for i := 0; i < 12; i++ {
		if c := chatReq(gw, body1).Code; c != 200 {
			t.Fatalf("冷却后应全部走好号，得到 %d", c)
		}
	}
	if delta := f.count(bad) - before; delta != 0 {
		t.Fatalf("冷却后仍打了坏号 %d 次", delta)
	}
}
