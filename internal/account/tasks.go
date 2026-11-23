// 任务中心：每日签到（领登录赠送积分）与邀请赚积分。
//
// 这两件事只存在于 Meoo 网页端（browser cookie 鉴权），个人 AK 调一次就是
// 403 API_KEY_ROUTE_FORBIDDEN。所以本文件的所有能力都建立在"账号已存 Cookie"之上，
// 并且必须把"没有 Cookie / Cookie 过期"和"上游真的失败了"区分清楚 ——
// 前者是用户要动手的事，后者才值得重试。
package account

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"meoo2api/internal/model"
)

const (
	checkinEndpoint = "/api/mc/entitlement/init" // GET，领当天登录赠送积分
	inviteCardPath  = "/api/growth/invite/myCard"
	inviteGenPath   = "/api/growth/invite/generate"
	dailyLoginSrc   = "DAILY_LOGIN" // activeGrants 里"登录赠送积分"的来源类型
	webUserAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// cst 固定东八区：上游的时间串是不带时区的墙钟时间（"2026-11-21T00:42:47.046"），
// 用本地时区解析会在 UTC 容器里把日期算错一天，签到状态就跟着错。
var cst = time.FixedZone("CST", 8*3600)

// webReply 是 Meoo 网页端统一信封。data / result 两种都出现过，取非 null 的那个。
type webReply struct {
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Result  json.RawMessage `json:"result"`
	ErrCode string          `json:"errCode"`
	ErrMsg  string          `json:"errMessage"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RedirectURL string `json:"redirectUrl"`
}

func (r *webReply) payload() json.RawMessage {
	if len(r.Result) > 0 && strings.TrimSpace(string(r.Result)) != "null" {
		return r.Result
	}
	return r.Data
}

func (r *webReply) code() string {
	if r.ErrCode != "" {
		return r.ErrCode
	}
	if r.Error != nil {
		return r.Error.Code
	}
	return ""
}

func (r *webReply) message() string {
	if r.ErrMsg != "" {
		return r.ErrMsg
	}
	if r.Error != nil && r.Error.Message != "" {
		return r.Error.Message
	}
	return ""
}

// webCall 用网页 Cookie 发一次站内请求，返回原始状态码与响应体。
//
// 刻意不走 upstream.DoJSON：那里把 {"success":false} 当成 error 返回，
// 而这里需要读到 errCode（NEED_LOGIN / INVITE_BLACKLISTED …）才能给出人话提示。
func (s *Service) webCall(ctx context.Context, cookie, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url(path), rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", s.base()+"/")
	req.Header.Set("User-Agent", webUserAgent)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 复制一份客户端：网页接口的 3xx 是"跳登录页"，绝不能跟着跳，否则 Cookie 会被带去第三方域名。
	client := *s.Up.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// webLoginOK 判断一段 Cookie 现在还能不能代表已登录用户。
// /api/v1/user/me 未登录时不是 401，而是 200 + data:null + redirectUrl，
// 所以必须看 data.user_id —— 只看状态码会把"已掉线"当成"登录正常"。
func (s *Service) webLoginOK(ctx context.Context, cookie string) (bool, string) {
	if strings.TrimSpace(cookie) == "" {
		return false, "未接入网页 Cookie"
	}
	code, raw, err := s.webCall(ctx, cookie, http.MethodGet, webMeEndpoint, nil)
	if err != nil {
		return false, "校验登录态失败：" + err.Error()
	}
	var rep webReply
	_ = json.Unmarshal(raw, &rep)
	if rep.code() == "NEED_LOGIN" || code == 401 || code == 403 {
		return false, "登录态已失效，请重新粘贴 Cookie"
	}
	if code != 200 {
		return false, fmt.Sprintf("校验登录态失败 HTTP %d", code)
	}
	var me struct {
		UserID string `json:"user_id"`
	}
	if len(rep.Data) > 0 {
		_ = json.Unmarshal(rep.Data, &me)
	}
	if me.UserID == "" {
		return false, "登录态已失效，请重新粘贴 Cookie"
	}
	return true, ""
}

// ---------------- 积分账本 ----------------

// dailyGrants 从 AK 只读的积分账本里取出"登录赠送积分"批次。
// 这是签到状态唯一可靠的来源：AK 不会过期，而 Cookie 会。
func (s *Service) dailyGrants(ctx context.Context, ak string) (balance int64, dates []string, err error) {
	if ak == "" {
		return 0, nil, fmt.Errorf("该账号没有个人 AK")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, s.url(pointsEndpoint), nil,
		map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": cliClientHead})
	if err != nil {
		return 0, nil, err
	}
	var d struct {
		Data struct {
			Account *struct {
				Available *int64 `json:"availableBalance"`
			} `json:"account"`
			ActiveGrants []struct {
				SourceType   string `json:"sourceType"`
				GrantDetails []struct {
					EffectiveDate string `json:"effectiveDate"`
				} `json:"grantDetails"`
			} `json:"activeGrants"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil {
		return 0, nil, fmt.Errorf("读积分账本失败 HTTP %d", code)
	}
	if d.Data.Account != nil && d.Data.Account.Available != nil {
		balance = *d.Data.Account.Available
	}
	for _, g := range d.Data.ActiveGrants {
		if g.SourceType != dailyLoginSrc {
			continue
		}
		for _, gd := range g.GrantDetails {
			if day := dayOf(gd.EffectiveDate); day != "" {
				dates = append(dates, day)
			}
		}
	}
	sort.Strings(dates)
	return balance, dates, nil
}

// dayOf 取上游时间串的日期部分（东八区墙钟时间，形如 2026-11-21T00:42:47.046）。
func dayOf(ts string) string {
	ts = strings.TrimSpace(ts)
	if len(ts) < 10 {
		return ""
	}
	day := ts[:10]
	if _, err := time.ParseInLocation("2006-01-02", day, cst); err != nil {
		return ""
	}
	return day
}

// streakOf 从签到日期集合算连续天数：今天没签就从昨天往前数，昨天也没签就是 0。
func streakOf(days []string, today string) int {
	if len(days) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, d := range days {
		set[d] = true
	}
	base, err := time.ParseInLocation("2006-01-02", today, cst)
	if err != nil {
		return 0
	}
	cur := base
	if !set[cur.Format("2006-01-02")] {
		cur = cur.AddDate(0, 0, -1)
	}
	n := 0
	for set[cur.Format("2006-01-02")] {
		n++
		cur = cur.AddDate(0, 0, -1)
	}
	return n
}

// ---------------- 签到 ----------------

// CheckinResult 是单个账号的签到结果；Error 只放"用户能据此动手"的原因。
type CheckinResult struct {
	AccountID string  `json:"id"`
	Label     string  `json:"label"`
	OK        bool    `json:"ok"`
	Already   bool    `json:"already"` // 今天之前已经领过（不算失败）
	Amount    float64 `json:"amount"`
	Balance   int64   `json:"balance"`
	Date      string  `json:"date,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Checkin 用一个账号的网页 Cookie 领当天的登录赠送积分。
// 重复调用是幂等的：上游只会认当天第一次，后面返回 granted=false。
func (s *Service) Checkin(ctx context.Context, accountID string) CheckinResult {
	res := CheckinResult{AccountID: accountID}

	s.St.Mu.RLock()
	var cookie, ak string
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			cookie, ak, res.Label = a.Cookie, a.PersonalAK, a.Label
			break
		}
	}
	s.St.Mu.RUnlock()

	if cookie == "" {
		res.Error = "未接入网页 Cookie（账号池 → 添加账号 → 粘贴 Cookie）"
		s.markCheckin(accountID, res)
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, raw, err := s.webCall(ctx, cookie, http.MethodGet, checkinEndpoint, nil)
	if err != nil {
		res.Error = "领取请求失败：" + err.Error()
		s.markCheckin(accountID, res)
		return res
	}
	var rep webReply
	_ = json.Unmarshal(raw, &rep)
	switch {
	case rep.code() == "NEED_LOGIN" || rep.code() == "API_KEY_ROUTE_FORBIDDEN" || code == 401:
		res.Error = "登录态已失效，请重新粘贴 Cookie"
	case code != 200:
		res.Error = fmt.Sprintf("领取出错 HTTP %d %s", code, strings.TrimSpace(rep.message()))
	default:
		var d struct {
			Initialized bool    `json:"initialized"`
			Granted     bool    `json:"dailyLoginGranted"`
			Amount      float64 `json:"dailyLoginGrantAmount"`
		}
		if jerr := json.Unmarshal(rep.payload(), &d); jerr != nil || !d.Initialized {
			res.Error = "上游未返回签到结果，稍后重试"
			break
		}
		res.OK = true
		res.Date = time.Now().In(cst).Format("2006-01-02")
		if d.Granted {
			res.Amount = d.Amount
		} else {
			// 今天已经领过：不是失败，但也不能报"领了 0 积分"让人以为出问题。
			res.Already = true
		}
	}

	// 无论领到多少，都用 AK 把余额刷新一遍 —— 账本才是权威，签到接口只是触发器。
	if ak != "" {
		if bal, _, gerr := s.dailyGrants(ctx, ak); gerr == nil {
			res.Balance = bal
			s.St.Mu.Lock()
			if a := s.findLocked(accountID); a != nil {
				a.Points = float64(bal)
				a.PointsAt = model.NowMilli()
			}
			s.St.Mu.Unlock()
		}
	}
	s.markCheckin(accountID, res)
	return res
}

// markCheckin 落盘签到结果；失败也要记（面板靠它显示"为什么没签上"）。
func (s *Service) markCheckin(accountID string, res CheckinResult) {
	s.St.Mu.Lock()
	if a := s.findLocked(accountID); a != nil {
		a.CheckinAt = model.NowMilli()
		if res.OK {
			// 今天已经领过时上游回 0 分：别把先前记下的金额抹成 0，
			// 否则自动签到会把「今天 +2000」显示成「今天 +0」。
			if !res.Already || a.CheckinDate != res.Date {
				a.CheckinAmount = res.Amount
			}
			a.CheckinDate = res.Date
			a.CheckinErr = ""
		} else {
			a.CheckinErr = res.Error
		}
	}
	s.St.Mu.Unlock()
	s.St.Touch()
}

// CheckinAll 依次签所有"有 Cookie 且启用"的账号。串行是故意的：
// 上游对同一 IP 的并发敏感，且账号通常只有个位数，省这点时间不值得。
func (s *Service) CheckinAll(ctx context.Context) []CheckinResult {
	s.St.Mu.RLock()
	ids := make([]string, 0, len(s.St.Data.Accounts))
	for _, a := range s.St.Data.Accounts {
		if a.Enabled && !a.Paused && a.Cookie != "" {
			ids = append(ids, a.ID)
		}
	}
	s.St.Mu.RUnlock()

	out := make([]CheckinResult, 0, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		out = append(out, s.Checkin(ctx, id))
	}
	return out
}

// ---------------- 概览 ----------------

// TaskRow 是任务中心表格的一行。
type TaskRow struct {
	AccountID     string  `json:"id"`
	Label         string  `json:"label"`
	Name          string  `json:"name"`
	HasCookie     bool    `json:"has_cookie"`
	CookieOK      bool    `json:"cookie_ok"`
	CookieErr     string  `json:"cookie_err,omitempty"`
	HasAK         bool    `json:"has_ak"`
	Balance       int64   `json:"balance"`
	PointsAt      int64   `json:"points_at"`
	TodaySigned   bool    `json:"today_signed"`
	Today         string  `json:"today"`
	LastGrantDate string  `json:"last_grant_date,omitempty"`
	Streak        int     `json:"streak"`
	CheckinAt     int64   `json:"checkin_at,omitempty"`
	CheckinDate   string  `json:"checkin_date,omitempty"`
	CheckinAmount float64 `json:"checkin_amount,omitempty"`
	CheckinErr    string  `json:"checkin_err,omitempty"`
	// 账本读取失败（不是签到失败），面板据此把"未签"标成"未知"。
	LedgerErr string `json:"ledger_err,omitempty"`
}

// TaskOverview 汇总所有账号的签到状态。
//
// 每个账号要两次只读调用（AK 读账本 + Cookie 验登录态），并发 3 个一组，
// 账号多了也不会把面板拖成几十秒。全部只读，不消耗积分。
func (s *Service) TaskOverview(ctx context.Context) []TaskRow {
	type snap struct {
		id, label, name, cookie, ak string
		enabled                     bool
		checkinAt                   int64
		checkinDate                 string
		checkinAmount               float64
		checkinErr                  string
		pointsAt                    int64
	}
	s.St.Mu.RLock()
	snaps := make([]snap, 0, len(s.St.Data.Accounts))
	for _, a := range s.St.Data.Accounts {
		snaps = append(snaps, snap{
			id: a.ID, label: a.Label, name: a.Name, cookie: a.Cookie, ak: a.PersonalAK,
			enabled: a.Enabled, checkinAt: a.CheckinAt, checkinDate: a.CheckinDate,
			checkinAmount: a.CheckinAmount, checkinErr: a.CheckinErr, pointsAt: a.PointsAt,
		})
	}
	s.St.Mu.RUnlock()

	today := time.Now().In(cst).Format("2006-01-02")
	rows := make([]TaskRow, len(snaps))

	sem := make(chan struct{}, 3)
	done := make(chan struct{}, len(snaps))
	for i, sn := range snaps {
		go func(i int, sn snap) {
			defer func() { <-sem; done <- struct{}{} }()
			sem <- struct{}{}
			r := TaskRow{
				AccountID: sn.id, Label: sn.label, Name: sn.name,
				HasCookie: sn.cookie != "", HasAK: sn.ak != "", Today: today,
				CheckinAt: sn.checkinAt, CheckinDate: sn.checkinDate,
				CheckinAmount: sn.checkinAmount, CheckinErr: sn.checkinErr,
				PointsAt: sn.pointsAt,
			}
			if sn.cookie != "" {
				r.CookieOK, r.CookieErr = s.webLoginOK(ctx, sn.cookie)
			} else {
				r.CookieErr = "未接入网页 Cookie"
			}
			if sn.ak != "" {
				if bal, days, err := s.dailyGrants(ctx, sn.ak); err == nil {
					r.Balance = bal
					for _, d := range days {
						if d == today {
							r.TodaySigned = true
						}
					}
					if len(days) > 0 {
						r.LastGrantDate = days[len(days)-1]
					}
					r.Streak = streakOf(days, today)
				} else {
					r.LedgerErr = err.Error()
				}
			}
			rows[i] = r
		}(i, sn)
	}
	for range snaps {
		<-done
	}
	return rows
}

// ---------------- 邀请 ----------------

// InviteCard 读某账号的邀请卡片：邀请码、链接、已注册/已订阅/已获得积分。
func (s *Service) InviteCard(ctx context.Context, accountID string) (map[string]any, error) {
	cookie, err := s.accountCookie(accountID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 先确认真的是登录态：匿名请求 myCard 也返回 200（code/stats 全 null），
	// 不先判定就会把"掉线"显示成"还没生成邀请码"。
	if ok, msg := s.webLoginOK(ctx, cookie); !ok {
		// 规则是公开的：掉线时也带回去，用户至少知道这条链接值不值得修。
		return s.anonRules(ctx), fmt.Errorf("%s", msg)
	}
	code, raw, err := s.webCall(ctx, cookie, http.MethodGet, inviteCardPath, nil)
	if err != nil {
		return nil, fmt.Errorf("读取邀请信息失败：%v", err)
	}
	var rep webReply
	_ = json.Unmarshal(raw, &rep)
	if rep.code() == "NEED_LOGIN" || code == 401 {
		return nil, fmt.Errorf("登录态已失效，请重新粘贴 Cookie")
	}
	if code != 200 {
		return nil, fmt.Errorf("读取邀请信息失败 HTTP %d %s", code, strings.TrimSpace(rep.message()))
	}
	return s.inviteView(rep.payload())
}

// InviteGenerate 生成邀请码（账号还没有专属邀请码时用）。
func (s *Service) InviteGenerate(ctx context.Context, accountID string) (map[string]any, error) {
	cookie, err := s.accountCookie(accountID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, raw, err := s.webCall(ctx, cookie, http.MethodPost, inviteGenPath, nil)
	if err != nil {
		return nil, fmt.Errorf("生成邀请码失败：%v", err)
	}
	var rep webReply
	_ = json.Unmarshal(raw, &rep)
	if rep.code() == "NEED_LOGIN" || code == 401 {
		return nil, fmt.Errorf("登录态已失效，请重新粘贴 Cookie")
	}
	if code != 200 {
		return nil, fmt.Errorf("生成邀请码失败 HTTP %d %s", code, strings.TrimSpace(rep.message()))
	}
	// 生成接口只回 code/expiresAt，统计要回头再读一次卡片。
	out, err := s.inviteView(rep.payload())
	if err != nil {
		return nil, err
	}
	if _, ok := out["code"]; ok {
		if card, cerr := s.InviteCard(ctx, accountID); cerr == nil {
			for k, v := range card {
				out[k] = v
			}
		}
	}
	return out, nil
}

// anonRules 匿名读一次邀请规则。上游对未登录请求不拦，只是把 code/stats 置空，
// 所以这一段能安全地当"公开配置"用，失败就返回 nil（面板会退化成只显示错误）。
func (s *Service) anonRules(ctx context.Context) map[string]any {
	code, raw, err := s.webCall(ctx, "", http.MethodGet, inviteCardPath, nil)
	if err != nil || code != 200 {
		return nil
	}
	var rep webReply
	if json.Unmarshal(raw, &rep) != nil {
		return nil
	}
	out, err := s.inviteView(rep.payload())
	if err != nil {
		return nil
	}
	delete(out, "code")
	delete(out, "link")
	delete(out, "stats")
	return out
}

// inviteView 把上游卡片摊平成面板直接能用的字段（snake_case，和其余接口一致）。
func (s *Service) inviteView(payload json.RawMessage) (map[string]any, error) {
	var card struct {
		Code      string `json:"code"`
		ExpiresAt string `json:"expiresAt"`
		Stats     *struct {
			RegisteredCount   int     `json:"registeredCount"`
			SubscribedCount   int     `json:"subscribedCount"`
			TotalRewardPoints float64 `json:"totalRewardPoints"`
			RiskPendingCount  int     `json:"riskPendingCount"`
		} `json:"stats"`
		RewardConfig *struct {
			RegisterRewardInviter       float64 `json:"registerRewardInviter"`
			RegisterRewardInvitee       float64 `json:"registerRewardInvitee"`
			FirstPaidReward             float64 `json:"firstPaidReward"`
			FirstPaidPurchaseWindowDays int     `json:"firstPaidPurchaseWindowDays"`
			FirstPaidDelayDays          int     `json:"firstPaidDelayDays"`
		} `json:"rewardConfig"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &card) != nil {
		return nil, fmt.Errorf("上游返回结构无法解析")
	}
	out := map[string]any{"code": card.Code, "expires_at": card.ExpiresAt}
	if card.Code != "" {
		// 邀请链接就是站点首页 + inviteCode，前端 kQ() 也是这么拼的。
		out["link"] = s.base() + "/?inviteCode=" + card.Code
	}
	if card.Stats != nil {
		out["stats"] = map[string]any{
			"registered":   card.Stats.RegisteredCount,
			"subscribed":   card.Stats.SubscribedCount,
			"earned":       card.Stats.TotalRewardPoints,
			"risk_pending": card.Stats.RiskPendingCount,
		}
	}
	if card.RewardConfig != nil {
		out["reward_config"] = map[string]any{
			"register_inviter":       card.RewardConfig.RegisterRewardInviter,
			"register_invitee":       card.RewardConfig.RegisterRewardInvitee,
			"first_paid":             card.RewardConfig.FirstPaidReward,
			"first_paid_window_days": card.RewardConfig.FirstPaidPurchaseWindowDays,
			"first_paid_delay_days":  card.RewardConfig.FirstPaidDelayDays,
		}
	}
	return out, nil
}

func (s *Service) accountCookie(accountID string) (string, error) {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			if strings.TrimSpace(a.Cookie) == "" {
				return "", fmt.Errorf("该账号未接入网页 Cookie，邀请功能只能用网页登录态")
			}
			return a.Cookie, nil
		}
	}
	return "", fmt.Errorf("账号不存在")
}

// ---------------- 自动签到 ----------------

// CheckinLoop 每 interval 醒一次，判断今天是否已经过了配置的签到时刻。
//
// 用"过了时刻就补签"而不是"正好到点才签"：服务重启、容器滚动都会错过整分钟，
// 补签对上游是幂等的（重复领只会返回 granted=false），代价远小于漏一天 2000 分。
func (s *Service) CheckinLoop(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		last := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				last = s.autoCheckinOnce(ctx, last)
			}
		}
	}()
	return done
}

func (s *Service) autoCheckinOnce(ctx context.Context, last string) string {
	s.St.Mu.RLock()
	on := s.St.Data.Config.CheckinAuto
	at := s.St.Data.Config.CheckinTime
	s.St.Mu.RUnlock()
	if !on {
		return last
	}
	hh, mm := 9, 0
	if t, ok := parseHM(at); ok {
		hh, mm = t[0], t[1]
	}
	now := time.Now().In(cst)
	today := now.Format("2006-01-02")
	if last == today {
		return last
	}
	if now.Hour() < hh || (now.Hour() == hh && now.Minute() < mm) {
		return last
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// 结果已经写进各账号的 checkin_* 字段，面板直接读；这里只要保证一天跑一次。
	_ = s.CheckinAll(runCtx)
	return today
}

// parseHM 解析 "09:00" / "9:00"；非法返回 false。
func parseHM(v string) ([2]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return [2]int{}, false
	}
	var h, m int
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil {
		return [2]int{}, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return [2]int{}, false
	}
	return [2]int{h, m}, true
}
