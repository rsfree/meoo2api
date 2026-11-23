package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// fakeSidecar 模拟 loginbrowser：记录收到的请求，按脚本返回。
type fakeSidecar struct {
	srv      *httptest.Server
	deleted  int32
	phoneHit int32
	getHit   int32
	cookie   string // harvest 返回的 Cookie
}

func newFakeSidecar(t *testing.T, cookie string) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{cookie: cookie}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Login-Key") != "sidecar-key" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		var in struct {
			Mode       string `json:"mode"`
			InviteCode string `json:"invite_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Mode != "sms" || in.InviteCode != "WkqDpEhL" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"id":"lb-1","status":"ready"}`))
	})
	mux.HandleFunc("POST /v1/sessions/lb-1/action", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch in.Type {
		case "phone":
			atomic.AddInt32(&f.phoneHit, 1)
		case "getcode":
			atomic.AddInt32(&f.getHit, 1)
		}
		_, _ = w.Write([]byte(`{"ok":true,"id":"lb-1","status":"ready"}`))
	})
	mux.HandleFunc("GET /v1/sessions/lb-1/state", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"id":"lb-1","status":"submitted","note":"已跳转到 meoo，可执行 harvest 取 Cookie"}`))
	})
	mux.HandleFunc("POST /v1/sessions/lb-1/harvest", func(w http.ResponseWriter, r *http.Request) {
		if f.cookie == "" {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"ok":false,"error":"没等到 meoo 落地页"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"id":"lb-1","cookies":"` + f.cookie + `","bytes":9,"invite":{"requested":true,"ok":true,"http":200}}`))
	})
	mux.HandleFunc("DELETE /v1/sessions/lb-1", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.deleted, 1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newAdminFixture 组装带 fake Meoo + fake sidecar 的管理端。
func newAdminFixture(t *testing.T, cookie string) (*Handler, *fakeSidecar, *account.Service, *store.Store) {
	t.Helper()
	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fake := &fakeAdminMeoo{srv: httptest.NewServer(fakeAdminMeooMux())}
	t.Cleanup(fake.srv.Close)

	acc := &account.Service{St: st, Up: upstream.New("http://upstream.invalid/v1"), Base: fake.srv.URL}
	lb := newFakeSidecar(t, cookie)
	h := &Handler{
		St: st, Acc: acc, Up: upstream.New("http://upstream.invalid/v1"),
		LBURL: lb.srv.URL, LBKey: "sidecar-key", Start: time.Now(),
		Sessions: NewSessions(), loginFails: map[string]failState{},
	}
	return h, lb, acc, st
}

func TestLBAttachFullFlow(t *testing.T) {
	h, lb, _, st := newAdminFixture(t, "sid=good; login_oneday_ticket=t")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/loginbrowser/attach",
		strings.NewReader(`{"id":"lb-1","label":"手机号登录"}`))
	h.lbAttach(w, r)

	var out struct {
		Ok     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Invite map[string]any `json:"invite"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v / %s", err, w.Body.String())
	}
	if !out.Ok {
		t.Fatalf("全链路应成功: %s", w.Body.String())
	}
	if out.Result["user_id"] != "u_test_1" {
		t.Fatalf("应返回接入账号身份: %+v", out.Result)
	}
	if out.Invite["ok"] != true {
		t.Fatalf("邀请归因结果应透传: %+v", out.Invite)
	}
	// 项目 AK 已入池且 provider=meoo
	st.Mu.RLock()
	n := len(st.Data.Keys)
	var prov string
	if n > 0 {
		prov = st.Data.Keys[0].ProviderName()
	}
	st.Mu.RUnlock()
	if n != 1 {
		t.Fatalf("应有 1 个项目 AK 入池，得到 %d", n)
	}
	if prov != "meoo" {
		t.Fatalf("新凭证 provider 应为 meoo，得到 %q", prov)
	}
	// 会话必须被清理，Cookie 明文不留在 sidecar 内存
	if atomic.LoadInt32(&lb.deleted) != 1 {
		t.Fatalf("attach 成功后应删除 sidecar 会话，deleted=%d", lb.deleted)
	}
}

func TestLBAttachBadCookieNoProject(t *testing.T) {
	h, _, _, st := newAdminFixture(t, "sid=bad")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/loginbrowser/attach",
		strings.NewReader(`{"id":"lb-1"}`))
	h.lbAttach(w, r)

	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("坏 Cookie 应失败: %s", w.Body.String())
	}
	st.Mu.RLock()
	nKeys, nAccs := len(st.Data.Keys), len(st.Data.Accounts)
	st.Mu.RUnlock()
	if nKeys != 0 {
		t.Fatalf("Cookie 无效时不得有凭证入池，得到 %d", nKeys)
	}
	if nAccs != 0 {
		t.Fatalf("Cookie 无效时不得创建账号，得到 %d", nAccs)
	}
}

func TestLBSessionDisabled(t *testing.T) {
	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &Handler{St: st, Start: time.Now(), Sessions: NewSessions(), loginFails: map[string]failState{}}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/loginbrowser/session", strings.NewReader(`{}`))
	h.lbSession(w, r)
	if w.Code != 503 {
		t.Fatalf("未配置 sidecar 应 503，得到 %d", w.Code)
	}
}

// ---------- fake Meoo（复刻 account 包的 fakeMeoo 关键路由） ----------

type fakeAdminMeoo struct{ srv *httptest.Server }

func fakeAdminMeooMux() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	unauth := func(w http.ResponseWriter) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"success":false,"errCode":"NEED_LOGIN"}`))
	}
	mux.HandleFunc("/api/v1/user/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sid=good") {
			unauth(w)
			return
		}
		ok(w, map[string]any{"data": map[string]any{"user_id": "u_test_1", "username": "测试账号"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/session", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true, "data": map[string]any{"sessionId": "sid-1"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/approve", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true, "data": map[string]any{"keyPrefix": "meoo_ak_test"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/session/sid-1", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true, "data": map[string]any{
			"status": "approved", "apiKey": "meoo_ak_test_1234567890", "userId": "u_test_1", "username": "测试账号",
		}})
	})
	mux.HandleFunc("/open/v1/cli-compat/account/identity", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "meoo_ak_test") {
			unauth(w)
			return
		}
		ok(w, map[string]any{"data": map[string]any{"user_id": "u_test_1", "username": "测试账号"}})
	})
	mux.HandleFunc("/open/v1/cli-compat/account/points", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"account": map[string]any{"availableBalance": 1234}}})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"url_id": "proj_1"}})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects/proj_1/cloud", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects/proj_1/cloud/environment", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"meooProjectApiKey": "meoo_project_test_1234567890", "enabled": true}})
	})
	return mux
}
