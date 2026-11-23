package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"meoo2api/internal/catalog"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// 站点目录给出的 provider 是「上游后端厂商标签」（bailian=阿里百炼），不是我方
// 注册过的上游名。号池里只有 meoo 时不能被这个标签带偏。
//
// 回归背景（v0.0.18）：v0.0.15 起 overlay 把目录 provider 写进模型，当时它只是
// 展示字段；v0.0.18 引入多供应商路由后，ProviderOf 把同一个字段当成选号键，
// 于是 provider=bailian 的模型全去找一个不存在的 bailian 上游，Pick 返回 nil，
// 整条线（10 个主模型 + 2 个别名）一律 503，只有 provider 为空的 extra 模型能通。
func TestBackendProviderLabelFallsBackToDefaultUpstream(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			// 目录条目直接带后端厂商标签，复刻生产形态
			_, _ = w.Write([]byte(`{"models":[{"id":"test-model","provider":"bailian"}],"defaultModel":"test-model"}`))
		case "/v1/chat/completions":
			atomic.AddInt32(&hits, 1)
			ok200(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	st.Mu.Lock()
	st.Data.Accounts = append(st.Data.Accounts, &model.Account{ID: "a1", UserID: "u1", Enabled: true})
	st.Data.Keys = append(st.Data.Keys, &model.Key{
		ID: "k1", Key: "meoo_project_abc", AccountID: "a1", Enabled: true,
		Health: model.Health{State: "ok"},
	})
	st.Data.ClientKeys = append(st.Data.ClientKeys, &model.ClientKey{
		ID: "c1", Key: "sk-test", Label: "test", Enabled: true,
	})
	st.Mu.Unlock()

	base := srv.URL + "/v1"
	up := upstream.New(base)
	reg := upstream.NewRegistry(model.DefaultProvider, base) // 只注册 meoo，没有 bailian
	pk := &pool.Picker{St: st, Up: up}
	cat := catalog.New(st, pk, up)
	cat.Refresh(context.Background())

	// 前提校验：目录里该模型确实带着 bailian 标签，否则这个测试是空跑
	if got := cat.ProviderOf(cat.Resolve("test-model")); got != "bailian" {
		t.Fatalf("测试前提不成立：目录 provider 应为 bailian，实际 %q", got)
	}

	gw := &Handler{St: st, Picker: pk, Catalog: cat, Up: up, Reg: reg}
	rec := chatReq(gw, `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("后端厂商标签不该阻断路由，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("应命中上游 1 次，实际 %d", n)
	}
}
