package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// fakeMeoo 模拟站点侧全部接口；Counts 用于断言"没有多余的上游动作"。
type fakeMeoo struct {
	srv        *httptest.Server
	userID     string
	projectHit int32
	approveHit int32
	key        string
	projectAK  string
}

func newFakeMeoo(t *testing.T) *fakeMeoo {
	t.Helper()
	f := &fakeMeoo{userID: "u_test_1", key: "meoo_ak_test_1234567890", projectAK: "meoo_project_test_1234567890"}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/user/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sid=good") {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}
		ok(w, map[string]any{"data": map[string]any{"user_id": f.userID, "username": "测试账号"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/session", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true, "data": map[string]any{"sessionId": "sid-1"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/approve", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.approveHit, 1)
		ok(w, map[string]any{"success": true, "data": map[string]any{"keyPrefix": "meoo_ak_test"}})
	})
	mux.HandleFunc("/api/v1/cli/auth/session/sid-1", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true, "data": map[string]any{
			"status": "approved", "apiKey": f.key, "userId": f.userID, "username": "测试账号",
		}})
	})
	mux.HandleFunc("/open/v1/cli-compat/account/identity", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"user_id": f.userID, "username": "测试账号"}})
	})
	mux.HandleFunc("/open/v1/cli-compat/account/points", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"account": map[string]any{"availableBalance": 1234}}})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.projectHit, 1)
		ok(w, map[string]any{"data": map[string]any{"url_id": "proj_1"}})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects/proj_1/cloud", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"success": true})
	})
	mux.HandleFunc("/open/v1/cli-compat/projects/proj_1/cloud/environment", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"data": map[string]any{"meooProjectApiKey": f.projectAK, "enabled": true}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newSvc(t *testing.T, f *fakeMeoo) *Service {
	t.Helper()
	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Service{St: st, Up: upstream.New("http://upstream.invalid/v1"), Base: f.srv.URL}
}

func TestAttachFromCookieRejectsBadCookie(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	if _, err := svc.AttachFromCookie(context.Background(), "sid=bad", ""); err == nil {
		t.Fatal("无效 Cookie 必须报错")
	}
	if atomic.LoadInt32(&f.projectHit) != 0 {
		t.Fatal("Cookie 无效时不得调用建项目接口")
	}
}

func TestAttachFromCookieFullProvision(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	out, err := svc.AttachFromCookie(context.Background(), "sid=good; other=1", "主账号")
	if err != nil {
		t.Fatalf("接入失败: %v", err)
	}
	if out["project_id"] != "proj_1" {
		t.Fatalf("项目 ID 未返回: %v", out)
	}
	if atomic.LoadInt32(&f.approveHit) != 1 {
		t.Fatalf("应恰好调用一次 approve，实际 %d", f.approveHit)
	}

	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if len(svc.St.Data.Accounts) != 1 {
		t.Fatalf("应写入 1 个账号，实际 %d", len(svc.St.Data.Accounts))
	}
	a := svc.St.Data.Accounts[0]
	if a.UserID != "u_test_1" || a.Label != "主账号" || a.PersonalAK != f.key {
		t.Fatalf("账号字段错误: %+v", a)
	}
	if !strings.Contains(a.Cookie, "sid=good") {
		t.Fatal("Cookie 未落库")
	}
	if a.Points != 1234 {
		t.Fatalf("积分未写入: %v", a.Points)
	}
	if len(svc.St.Data.Keys) != 1 || svc.St.Data.Keys[0].Key != f.projectAK {
		t.Fatalf("项目 AK 未入池: %+v", svc.St.Data.Keys)
	}
	// 授权会话里的 AK 副本必须清掉
	if d := svc.devices["sid-1"]; d != nil && d.status.APIKey != "" {
		t.Fatal("授权会话仍保留 AK 明文")
	}
}

func TestAttachFromCookieOnlyUpdatesWhenKeyExists(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	// 预置：账号已在库且有可用凭证
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = append(svc.St.Data.Accounts, &model.Account{
		ID: "a_exist", UserID: f.userID, Enabled: true, PersonalAK: "meoo_ak_old_1234567890",
	})
	svc.St.Data.Keys = append(svc.St.Data.Keys, &model.Key{
		ID: "k_exist", Key: f.projectAK, AccountID: "a_exist", Enabled: true, Health: model.Health{State: "ok"},
	})
	svc.St.Mu.Unlock()

	out, err := svc.AttachFromCookie(context.Background(), "sid=good", "")
	if err != nil {
		t.Fatalf("接入失败: %v", err)
	}
	if out["cookie_only"] != true {
		t.Fatalf("已有可用凭证时只应补 Cookie，实际 %v", out)
	}
	if atomic.LoadInt32(&f.approveHit) != 0 {
		t.Fatal("已有可用凭证时不应再走授权")
	}
	if atomic.LoadInt32(&f.projectHit) != 0 {
		t.Fatal("已有可用凭证时不应再建项目")
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if len(svc.St.Data.Accounts) != 1 {
		t.Fatalf("不应新增账号，实际 %d", len(svc.St.Data.Accounts))
	}
	if svc.St.Data.Accounts[0].Cookie == "" {
		t.Fatal("Cookie 未更新")
	}
	if svc.St.Data.Accounts[0].PersonalAK != "meoo_ak_old_1234567890" {
		t.Fatal("不应覆盖已有的个人 AK")
	}
}

func TestAttachFromCookieReusesExistingAccountWithoutKey(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	// 账号在库但没凭证（上次开通失败）：应复用账号，不新建重复账号
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = append(svc.St.Data.Accounts, &model.Account{
		ID: "a_half", UserID: f.userID, Enabled: true,
	})
	svc.St.Mu.Unlock()

	if _, err := svc.AttachFromCookie(context.Background(), "sid=good", ""); err != nil {
		t.Fatalf("接入失败: %v", err)
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if len(svc.St.Data.Accounts) != 1 {
		t.Fatalf("同一 user_id 不应产生第二个账号，实际 %d", len(svc.St.Data.Accounts))
	}
	if svc.St.Data.Accounts[0].PersonalAK != f.key {
		t.Fatal("个人 AK 未补上")
	}
}

func TestRepairProjectKeyUpdatesRotatedKey(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	// 池里放一个旧值（模拟云端已换血）
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = append(svc.St.Data.Accounts, &model.Account{
		ID: "a1", UserID: f.userID, Enabled: true, PersonalAK: f.key, ProjectID: "proj_1",
	})
	svc.St.Data.Keys = append(svc.St.Data.Keys, &model.Key{
		ID: "k1", Key: "meoo_project_OLD_value", AccountID: "a1", ProjectID: "proj_1",
		Enabled: true, Health: model.Health{State: "invalid", Reason: "401"},
	})
	svc.St.Mu.Unlock()

	ak, changed, err := svc.RepairProjectKey(context.Background(), "a1")
	if err != nil {
		t.Fatalf("自愈失败: %v", err)
	}
	if !changed || ak != f.projectAK {
		t.Fatalf("应更新为云端值，changed=%v ak=%s", changed, ak)
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if svc.St.Data.Keys[0].Key != f.projectAK {
		t.Fatal("池内凭证未更新")
	}
	if svc.St.Data.Keys[0].Health.State != "ok" {
		t.Fatal("失效标记未清除")
	}
	if svc.St.Data.Keys[0].ID != "k1" {
		t.Fatal("应原地替换，保留统计与 ID")
	}
}

func TestRepairProjectKeyNoChangeClearsInvalid(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = append(svc.St.Data.Accounts, &model.Account{
		ID: "a1", UserID: f.userID, Enabled: true, PersonalAK: f.key, ProjectID: "proj_1",
	})
	svc.St.Data.Keys = append(svc.St.Data.Keys, &model.Key{
		ID: "k1", Key: f.projectAK, AccountID: "a1", ProjectID: "proj_1",
		Enabled: true, Health: model.Health{State: "invalid"},
	})
	svc.St.Mu.Unlock()

	_, changed, err := svc.RepairProjectKey(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("值相同不应报 changed")
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if svc.St.Data.Keys[0].Health.State != "ok" {
		t.Fatal("值相同也应清除失效标记（说明是误判）")
	}
}

func TestRepairProjectKeyWithoutProjectFails(t *testing.T) {
	f := newFakeMeoo(t)
	svc := newSvc(t, f)
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = append(svc.St.Data.Accounts, &model.Account{
		ID: "a1", UserID: f.userID, Enabled: true, PersonalAK: f.key,
	})
	svc.St.Mu.Unlock()
	if _, _, err := svc.RepairProjectKey(context.Background(), "a1"); err == nil {
		t.Fatal("没有项目时应明确报错，而不是返回空凭证")
	}
}
