package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// newTaskSvc 起一个只覆盖任务中心所需接口的假站点。
// grantToday=true 时账本里带一条今天的 DAILY_LOGIN 批次。
func newTaskSvc(t *testing.T, loggedIn, granted bool, grantToday bool) (*Service, *int32) {
	t.Helper()
	var initHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		switch r.URL.Path {
		case "/api/v1/user/me":
			if !loggedIn {
				// 上游未登录的真实形状：200 + data:null + redirectUrl
				write(map[string]any{"success": true, "data": nil, "redirectUrl": "https://meoo.com/api/auth/login_redirect"})
				return
			}
			write(map[string]any{"success": true, "data": map[string]any{"user_id": "u_1"}})
		case "/api/mc/entitlement/init":
			initHits++
			if !loggedIn {
				write(map[string]any{"success": false, "errCode": "NEED_LOGIN", "errMessage": "未登录"})
				return
			}
			write(map[string]any{"success": true, "data": map[string]any{
				"initialized": true, "dailyLoginGranted": granted, "dailyLoginGrantAmount": 2000,
			}})
		case "/open/v1/cli-compat/account/points":
			grants := []any{}
			if grantToday {
				grants = append(grants, map[string]any{
					"sourceType": "DAILY_LOGIN",
					"grantDetails": []any{map[string]any{
						"remainingAmount": 2000,
						"effectiveDate":   time.Now().In(cst).Format("2006-01-02") + "T00:10:00",
					}},
				})
			}
			write(map[string]any{"data": map[string]any{
				"account":      map[string]any{"availableBalance": 9052},
				"activeGrants": grants,
			}})
		case "/api/growth/invite/myCard":
			if r.Header.Get("Cookie") == "" || !loggedIn {
				write(map[string]any{"success": true, "data": map[string]any{
					"code": nil, "expiresAt": nil, "stats": nil,
					"rewardConfig": map[string]any{
						"registerRewardInviter": 5000, "registerRewardInvitee": 5000,
						"firstPaidReward": 10000, "firstPaidPurchaseWindowDays": 7, "firstPaidDelayDays": 5,
					},
				}})
				return
			}
			write(map[string]any{"success": true, "data": map[string]any{
				"code": "AB12CD34", "expiresAt": "2027-01-31T23:59:59",
				"stats":        map[string]any{"registeredCount": 12, "subscribedCount": 4, "totalRewardPoints": 140000},
				"rewardConfig": map[string]any{"registerRewardInviter": 5000, "registerRewardInvitee": 5000, "firstPaidReward": 10000},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &Service{St: st, Up: upstream.New("http://upstream.invalid/v1"), Base: srv.URL}
	svc.St.Mu.Lock()
	svc.St.Data.Accounts = []*model.Account{{
		ID: "a_1", Label: "主账号", Enabled: true,
		PersonalAK: "meoo_ak_test", Cookie: "sid=good",
	}}
	svc.St.Mu.Unlock()
	return svc, &initHits
}

func TestDayOf(t *testing.T) {
	if got := dayOf("2026-11-21T00:42:47.046"); got != "2026-11-21" {
		t.Fatalf("dayOf 解析错误: %q", got)
	}
	if got := dayOf(""); got != "" {
		t.Fatalf("空串应返回空: %q", got)
	}
	if got := dayOf("not-a-date"); got != "" {
		t.Fatalf("非法日期应返回空: %q", got)
	}
}

func TestStreakOf(t *testing.T) {
	today := "2026-11-23"
	if n := streakOf([]string{"2026-11-21", "2026-11-22"}, today); n != 2 {
		t.Fatalf("昨天+前天应连续 2 天，实际 %d", n)
	}
	if n := streakOf([]string{"2026-11-21", "2026-11-23"}, today); n != 1 {
		t.Fatalf("中间断档应只算今天 1 天，实际 %d", n)
	}
	if n := streakOf([]string{"2026-11-20"}, today); n != 0 {
		t.Fatalf("断了两天应为 0，实际 %d", n)
	}
	if n := streakOf(nil, today); n != 0 {
		t.Fatalf("无批次应为 0，实际 %d", n)
	}
}

func TestCheckinGrantsAndPersists(t *testing.T) {
	svc, hits := newTaskSvc(t, true, true, true)
	res := svc.Checkin(context.Background(), "a_1")
	if !res.OK || res.Amount != 2000 || res.Already {
		t.Fatalf("签到结果不符: %+v", res)
	}
	if res.Balance != 9052 {
		t.Fatalf("余额未用账本刷新: %d", res.Balance)
	}
	if *hits != 1 {
		t.Fatalf("应调用一次 init，实际 %d", *hits)
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	a := svc.St.Data.Accounts[0]
	if a.CheckinDate != time.Now().In(cst).Format("2006-01-02") || a.CheckinAmount != 2000 || a.CheckinErr != "" {
		t.Fatalf("签到未落盘: %+v", a)
	}
	if a.Points != 9052 {
		t.Fatalf("账号余额未更新: %v", a.Points)
	}
}

func TestCheckinAlreadyGrantedIsNotFailure(t *testing.T) {
	svc, _ := newTaskSvc(t, true, false, true)
	res := svc.Checkin(context.Background(), "a_1")
	if !res.OK || !res.Already || res.Amount != 0 {
		t.Fatalf("已领过应成功且已签，不该报错: %+v", res)
	}
}

func TestCheckinExpiredCookieReportsActionableError(t *testing.T) {
	svc, _ := newTaskSvc(t, false, false, false)
	res := svc.Checkin(context.Background(), "a_1")
	if res.OK {
		t.Fatalf("登录态失效不该算成功: %+v", res)
	}
	if !strings.Contains(res.Error, "登录态") {
		t.Fatalf("错误信息应指向 Cookie，实际 %q", res.Error)
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if svc.St.Data.Accounts[0].CheckinErr == "" {
		t.Fatal("失败原因未落盘，面板会显示不出为什么没签上")
	}
}

func TestCheckinWithoutCookie(t *testing.T) {
	svc, hits := newTaskSvc(t, true, true, true)
	svc.St.Mu.Lock()
	svc.St.Data.Accounts[0].Cookie = ""
	svc.St.Mu.Unlock()
	res := svc.Checkin(context.Background(), "a_1")
	if res.OK || !strings.Contains(res.Error, "Cookie") {
		t.Fatalf("无 Cookie 应提示接入: %+v", res)
	}
	if *hits != 0 {
		t.Fatal("没有 Cookie 时不该打上游")
	}
}

func TestTaskOverviewReadsLedger(t *testing.T) {
	svc, _ := newTaskSvc(t, true, true, true)
	rows := svc.TaskOverview(context.Background())
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(rows))
	}
	r := rows[0]
	if !r.TodaySigned || !r.CookieOK || r.Balance != 9052 || r.Streak != 1 {
		t.Fatalf("概览字段不符: %+v", r)
	}
}

func TestTaskOverviewSurvivesDeadCookie(t *testing.T) {
	svc, _ := newTaskSvc(t, false, false, false)
	rows := svc.TaskOverview(context.Background())
	r := rows[0]
	if r.CookieOK {
		t.Fatal("失效 Cookie 应判定为不可用")
	}
	if !strings.Contains(r.CookieErr, "登录态") {
		t.Fatalf("应给出可操作提示: %q", r.CookieErr)
	}
	// 账本仍要读到：签到状态不依赖 Cookie
	if r.TodaySigned || r.Balance != 9052 {
		t.Fatalf("账本读取不该被 Cookie 影响: %+v", r)
	}
}

func TestInviteCardBuildsLink(t *testing.T) {
	svc, _ := newTaskSvc(t, true, true, true)
	card, err := svc.InviteCard(context.Background(), "a_1")
	if err != nil {
		t.Fatalf("读邀请卡失败: %v", err)
	}
	if card["code"] != "AB12CD34" {
		t.Fatalf("邀请码错误: %v", card["code"])
	}
	if !strings.HasSuffix(card["link"].(string), "/?inviteCode=AB12CD34") {
		t.Fatalf("邀请链接拼接错误: %v", card["link"])
	}
	st, _ := card["stats"].(map[string]any)
	if st["registered"] != 12 {
		t.Fatalf("统计未透传: %v", st)
	}
}

// 掉线时仍要把公开的奖励规则带回去，否则面板只能显示一句干巴巴的错误。
func TestInviteCardKeepsPublicRulesWhenLoggedOut(t *testing.T) {
	svc, _ := newTaskSvc(t, false, false, false)
	card, err := svc.InviteCard(context.Background(), "a_1")
	if err == nil {
		t.Fatal("掉线应返回错误")
	}
	if card == nil || card["reward_config"] == nil {
		t.Fatalf("应带回奖励规则: %v", card)
	}
	if _, has := card["code"]; has {
		t.Fatal("未登录不该凭空造出邀请码")
	}
}

func TestParseHM(t *testing.T) {
	cases := map[string]bool{"09:00": true, "9:05": true, "23:59": true, "24:00": false, "09:60": false, "": false, "abc": false}
	for in, want := range cases {
		if _, ok := parseHM(in); ok != want {
			t.Fatalf("parseHM(%q) = %v, want %v", in, ok, want)
		}
	}
}

func TestValidInviteCode(t *testing.T) {
	cases := map[string]bool{
		"WkqDpEhL": true, "ab12": true, "AB12CD34": true,
		"abc": false, "": false, "WkqDpEhL!": false, "空格 码": false,
	}
	for in, want := range cases {
		if got := validInviteCode(in); got != want {
			t.Fatalf("validInviteCode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSetInviteCodeBuildsLink(t *testing.T) {
	svc, hits := newTaskSvc(t, true, true, true)
	card, err := svc.SetInviteCode(context.Background(), "a_1", "  WkqDpEhL  ")
	if err != nil {
		t.Fatalf("手工设置失败: %v", err)
	}
	if card["code"] != "WkqDpEhL" || !strings.HasSuffix(card["link"].(string), "/?inviteCode=WkqDpEhL") {
		t.Fatalf("链接拼接不对: %v", card)
	}
	if *hits != 0 {
		t.Fatal("手工设码不该打上游")
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	if svc.St.Data.Accounts[0].InviteCode != "WkqDpEhL" {
		t.Fatal("邀请码未落盘")
	}
}

func TestSetInviteCodeRejectsBadCode(t *testing.T) {
	svc, _ := newTaskSvc(t, true, true, true)
	if _, err := svc.SetInviteCode(context.Background(), "a_1", "no way"); err == nil {
		t.Fatal("非法邀请码必须拒绝")
	}
	if _, err := svc.SetInviteCode(context.Background(), "missing", "WkqDpEhL"); err == nil {
		t.Fatal("不存在的账号必须报错")
	}
}

// 掉线 + 本地有码 = 仍然要能复制链接，只是没有统计。
func TestInviteCardFallsBackToCachedCode(t *testing.T) {
	svc, _ := newTaskSvc(t, false, false, false)
	if _, err := svc.SetInviteCode(context.Background(), "a_1", "WkqDpEhL"); err != nil {
		t.Fatalf("预置邀请码失败: %v", err)
	}
	card, err := svc.InviteCard(context.Background(), "a_1")
	if err == nil {
		t.Fatal("掉线仍应返回错误提示")
	}
	if card == nil {
		t.Fatal("应回落到本地邀请码，实际为空")
	}
	if link, _ := card["link"].(string); !strings.HasSuffix(link, "/?inviteCode=WkqDpEhL") {
		t.Fatalf("链接应回落到本地邀请码: %v", card)
	}
	if card["stale"] != true {
		t.Fatalf("应标记为未刷新: %v", card)
	}
	if _, has := card["stats"]; has {
		t.Fatal("掉线不该编造统计数字")
	}
}

// 上线后拿到码要落盘：否则下一次 Cookie 过期又会退回"没有邀请码"。
func TestInviteCardCachesUpstreamCode(t *testing.T) {
	svc, _ := newTaskSvc(t, true, true, true)
	if _, err := svc.InviteCard(context.Background(), "a_1"); err != nil {
		t.Fatalf("读邀请卡失败: %v", err)
	}
	svc.St.Mu.RLock()
	defer svc.St.Mu.RUnlock()
	a := svc.St.Data.Accounts[0]
	if a.InviteCode != "AB12CD34" || a.InviteExpAt == "" || a.InviteCodeAt == 0 {
		t.Fatalf("上游邀请码未缓存: %+v", a)
	}
}

// 连 Cookie 都没有的号，只要本地存过码，邀请卡也不该是空的。
func TestInviteCardFallsBackWithoutCookie(t *testing.T) {
	svc, _ := newTaskSvc(t, true, true, true)
	if _, err := svc.SetInviteCode(context.Background(), "a_1", "WkqDpEhL"); err != nil {
		t.Fatalf("预置邀请码失败: %v", err)
	}
	svc.St.Mu.Lock()
	svc.St.Data.Accounts[0].Cookie = ""
	svc.St.Mu.Unlock()
	card, err := svc.InviteCard(context.Background(), "a_1")
	if err == nil || !strings.Contains(err.Error(), "Cookie") {
		t.Fatalf("无 Cookie 应给出接入提示: %v", err)
	}
	if link, _ := card["link"].(string); !strings.HasSuffix(link, "/?inviteCode=WkqDpEhL") {
		t.Fatalf("应回落到本地邀请码: %v", card)
	}
}
