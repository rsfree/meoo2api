// Package account 管理 Meoo 账号：设备授权、身份校验、项目开通与项目 AK 入库。
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

const (
	// 默认站点；Service.Base 可覆盖（测试与镜像部署）。
	defaultBase  = "https://meoo.com"
	deviceCreate = "/api/v1/cli/auth/session"
	devicePoll   = "/api/v1/cli/auth/session/%s"
	loginPage    = "/cli/login?session_id=%s"

	approveEndpoint  = "/api/v1/cli/auth/approve"
	webMeEndpoint    = "/api/v1/user/me"
	identityEndpoint = "/open/v1/cli-compat/account/identity"
	pointsEndpoint   = "/open/v1/cli-compat/account/points"
	projectsEndpoint = "/open/v1/cli-compat/projects"

	deviceTTL     = 15 * time.Minute
	maxDevices    = 128
	maxProjects   = 20
	cliClientHead = "cli"
)

// Service 是账号域的入口。
type Service struct {
	St *store.Store
	Up *upstream.Client
	// Base 是 Meoo 站点根地址（默认 https://meoo.com），测试与镜像部署可覆盖。
	Base string

	gateOnce sync.Once
	gate     chan struct{} // 串行化"写凭证/建项目"，避免同一身份并发建出多个项目
	devMu    sync.Mutex
	devices  map[string]*deviceSession
}

type deviceSession struct {
	expires   time.Time
	status    DeviceState
	consumed  bool   // approved 凭证只能被取用一次
	accountID string // 已接入账号，用于重试幂等
}

// DeviceState 是设备授权状态；APIKey 仅在服务端使用，绝不出现在面板响应里。
type DeviceState struct {
	Status   string `json:"status"`
	APIKey   string `json:"-"`
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	LoginURL string `json:"login_url,omitempty"`
}

func (s *Service) base() string {
	if s.Base != "" {
		return strings.TrimRight(s.Base, "/")
	}
	return defaultBase
}

// url 拼接站点内绝对地址。
func (s *Service) url(path string) string { return s.base() + path }

func (s *Service) loginURL(sid string) string {
	return s.base() + fmt.Sprintf(loginPage, url.QueryEscape(sid))
}

func (s *Service) acquire(ctx context.Context) (func(), error) {
	s.gateOnce.Do(func() { s.gate = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) request(ctx context.Context, method, endpoint string, body any, hdr map[string]string) (int, []byte, error) {
	client := *s.Up.HTTP
	// 上游的任何 3xx 都不是合法 API 结果，也不应把 AK/Cookie 带去第三方域名。
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	up := *s.Up
	up.HTTP = &client
	return up.DoJSON(ctx, method, endpoint, body, hdr)
}

// ---------------- 设备授权 ----------------

// StartDevice 创建一次设备授权会话，返回 (sessionId, 登录页 URL)。
func (s *Service) StartDevice(ctx context.Context) (string, string, error) {
	s.devMu.Lock()
	if s.devices == nil {
		s.devices = map[string]*deviceSession{}
	}
	s.gcDevicesLocked()
	full := len(s.devices) >= maxDevices
	s.devMu.Unlock()
	if full {
		return "", "", fmt.Errorf("授权会话过多，请等待已有会话过期")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodPost, s.url(deviceCreate), map[string]any{}, nil)
	if err != nil {
		return "", "", err
	}
	var d struct {
		Success *bool `json:"success"`
		Data    struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.SessionID == "" {
		return "", "", fmt.Errorf("创建设备授权会话失败 HTTP %d", code)
	}
	sid := d.Data.SessionID
	login := s.loginURL(sid)
	s.devMu.Lock()
	s.devices[sid] = &deviceSession{expires: time.Now().Add(deviceTTL), status: DeviceState{Status: "pending", LoginURL: login}}
	s.devMu.Unlock()
	return sid, login, nil
}

func (s *Service) gcDevicesLocked() {
	now := time.Now()
	for k, v := range s.devices {
		if now.After(v.expires) {
			delete(s.devices, k)
		}
	}
}

// PollDevice 查询授权状态。approved 凭证只向上游读取一次并就地缓存。
func (s *Service) PollDevice(ctx context.Context, sid string) (DeviceState, error) {
	s.devMu.Lock()
	d := s.devices[sid]
	s.devMu.Unlock()
	if d == nil {
		return DeviceState{Status: "unknown"}, fmt.Errorf("授权会话不存在或已过期")
	}
	s.devMu.Lock()
	if d.consumed || d.status.Status != "pending" {
		st := d.status
		s.devMu.Unlock()
		return st, nil
	}
	s.devMu.Unlock()
	if time.Now().After(d.expires) {
		return DeviceState{Status: "expired"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, s.url(fmt.Sprintf(devicePoll, url.PathEscape(sid))), nil, nil)
	if err != nil {
		return DeviceState{}, err
	}
	var resp struct {
		Success *bool `json:"success"`
		Data    struct {
			Status   string `json:"status"`
			APIKey   string `json:"apiKey"`
			UserID   string `json:"userId"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &resp) != nil {
		return DeviceState{}, fmt.Errorf("查询授权状态失败 HTTP %d", code)
	}
	switch resp.Data.Status {
	case "pending", "expired", "denied", "approved":
	default:
		return DeviceState{}, fmt.Errorf("上游返回未知授权状态 %q", resp.Data.Status)
	}
	s.devMu.Lock()
	d.status.Status = resp.Data.Status
	d.status.UserID = resp.Data.UserID
	d.status.Username = resp.Data.Username
	d.status.LoginURL = s.loginURL(sid)
	if resp.Data.Status == "approved" {
		d.status.APIKey = resp.Data.APIKey
	}
	st := d.status
	s.devMu.Unlock()
	return st, nil
}

// ---------------- 身份与积分 ----------------

// Identity 用个人 AK 换取身份。
func (s *Service) Identity(ctx context.Context, ak string) (userID, name string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet,
		s.url(identityEndpoint), nil,
		map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": cliClientHead})
	if err != nil {
		return "", "", err
	}
	var d struct {
		Data struct {
			UserID   string `json:"user_id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.UserID == "" {
		return "", "", fmt.Errorf("身份校验失败 HTTP %d", code)
	}
	name = d.Data.Name
	if name == "" {
		name = d.Data.Username
	}
	return d.Data.UserID, name, nil
}

// Points 查询个人可用积分。
func (s *Service) Points(ctx context.Context, ak string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, s.url(pointsEndpoint), nil,
		map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": cliClientHead})
	if err != nil {
		return 0, err
	}
	var d struct {
		Data struct {
			Account *struct {
				Available *int64 `json:"availableBalance"`
			} `json:"account"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.Account == nil || d.Data.Account.Available == nil {
		return 0, fmt.Errorf("查询积分失败 HTTP %d", code)
	}
	return *d.Data.Account.Available, nil
}

// ---------------- 项目开通 ----------------

// EnsureProject 保证账号下有一个已开通云服务的项目，返回项目 AK。
// 中途失败可重入：已建的项目 ID 会先落盘，重试只会续做后续步骤。
func (s *Service) EnsureProject(ctx context.Context, accountID, name string) (projectID, projectAK string, err error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return "", "", err
	}
	defer release()

	s.St.Mu.RLock()
	var acc *model.Account
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			acc = a
			break
		}
	}
	if acc == nil {
		s.St.Mu.RUnlock()
		return "", "", fmt.Errorf("账号不存在")
	}
	ak := acc.PersonalAK
	pid := acc.ProjectID
	s.St.Mu.RUnlock()
	if ak == "" {
		return "", "", fmt.Errorf("该账号没有个人 AK，请先完成设备授权")
	}
	if existing := s.findKeyByAccount(accountID); existing != nil && existing.ProjectID == pid && pid != "" {
		return pid, existing.Key, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": cliClientHead}

	// 1) 建项目（幂等：已有 project_id 就复用）
	if pid == "" {
		if strings.TrimSpace(name) == "" {
			name = "meoo2api-" + model.RandID("", 3)
		}
		code, raw, perr := s.request(ctx, http.MethodPost, s.url(projectsEndpoint),
			map[string]any{"name": name, "type": "create-desktop"}, hdr)
		if perr != nil {
			return "", "", perr
		}
		var d struct {
			Data struct {
				URLID string `json:"url_id"`
			} `json:"data"`
		}
		if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.URLID == "" {
			return "", "", fmt.Errorf("创建项目失败 HTTP %d", code)
		}
		pid = d.Data.URLID
		s.St.Mu.Lock()
		acc.ProjectID = pid
		s.St.Mu.Unlock()
		if err := s.St.Save(); err != nil {
			return pid, "", fmt.Errorf("项目已创建但状态保存失败：%w", err)
		}
	}

	// 2) 开通云服务（409 = 已开通，视为成功）
	base := s.url(projectsEndpoint) + "/" + url.PathEscape(pid) + "/cloud"
	code, _, perr := s.request(ctx, http.MethodPost, base,
		map[string]any{"description": name + " cloud"}, hdr)
	if perr != nil {
		if ue, ok := perr.(*upstream.Error); !ok || ue.Status != http.StatusConflict {
			return pid, "", perr
		}
	} else if code != 200 && code != 201 {
		return pid, "", fmt.Errorf("开通云服务失败 HTTP %d", code)
	}

	// 3) 读环境变量拿项目 AK（开通后可能异步生效，有限重试）
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		code, raw, e := s.request(ctx, http.MethodGet, base+"/environment", nil, hdr)
		if e == nil && code == 200 {
			var d struct {
				Data struct {
					Key     string `json:"meooProjectApiKey"`
					Enabled *bool  `json:"enabled"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &d) == nil && d.Data.Key != "" {
				if d.Data.Enabled == nil || *d.Data.Enabled {
					return pid, d.Data.Key, nil
				}
				lastErr = fmt.Errorf("项目云服务未启用")
			} else {
				lastErr = fmt.Errorf("读取项目凭证失败 HTTP %d", code)
			}
		} else if e != nil {
			lastErr = e
			if ue, ok := e.(*upstream.Error); ok && !ue.Retryable() {
				return pid, "", e
			}
		} else {
			lastErr = fmt.Errorf("读取项目凭证失败 HTTP %d", code)
		}
		select {
		case <-ctx.Done():
			return pid, "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return pid, "", lastErr
}

func (s *Service) findKeyByAccount(accountID string) *model.Key {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	for _, k := range s.St.Data.Keys {
		if k.AccountID == accountID {
			return k
		}
	}
	return nil
}

// AttachFromDevice 消费一次已 approved 的设备授权，落库账号并把项目 AK 入池。
func (s *Service) AttachFromDevice(ctx context.Context, sid string) (map[string]any, error) {
	s.devMu.Lock()
	d := s.devices[sid]
	if d == nil {
		s.devMu.Unlock()
		return nil, fmt.Errorf("授权会话不存在或已过期")
	}
	if d.consumed {
		accID := d.accountID
		s.devMu.Unlock()
		if accID == "" {
			return nil, fmt.Errorf("该授权会话已使用")
		}
		pid, key, err := s.EnsureProject(ctx, accID, "")
		if err != nil {
			return map[string]any{"account_id": accID, "stage": "project"}, err
		}
		s.upsertKey(accID, pid, key)
		return map[string]any{"account_id": accID, "project_id": pid, "resumed": true}, s.St.Save()
	}
	if d.status.Status != "approved" || d.status.APIKey == "" {
		s.devMu.Unlock()
		return nil, fmt.Errorf("授权尚未完成（当前状态 %s）", d.status.Status)
	}
	ak := d.status.APIKey
	uid := d.status.UserID
	s.devMu.Unlock()

	userID, name, err := s.Identity(ctx, ak)
	if err != nil {
		return nil, err
	}
	if uid != "" && uid != userID {
		return nil, fmt.Errorf("授权凭证与身份不一致")
	}

	s.St.Mu.Lock()
	var acc *model.Account
	for _, a := range s.St.Data.Accounts {
		if a.UserID == userID {
			acc = a
			break
		}
	}
	if acc == nil {
		acc = &model.Account{ID: model.RandID("a_", 6), UserID: userID, Enabled: true, CreatedAt: model.NowMilli()}
		s.St.Data.Accounts = append(s.St.Data.Accounts, acc)
	}
	acc.PersonalAK = ak
	acc.PersonalAKAt = model.NowMilli()
	acc.Name = name
	if acc.Label == "" {
		acc.Label = name
	}
	acc.LastError = ""
	accID := acc.ID
	s.St.Mu.Unlock()
	if err := s.St.Save(); err != nil {
		return nil, err
	}

	s.devMu.Lock()
	d.consumed = true
	d.status.APIKey = "" // 凭证只保留在账号表里，授权会话不再持有
	d.accountID = accID
	s.devMu.Unlock()

	pid, key, err := s.EnsureProject(ctx, accID, "")
	if err != nil {
		s.setAccountError(accID, err.Error())
		return map[string]any{"account_id": accID, "stage": "project"}, err
	}
	s.upsertKey(accID, pid, key)
	if pts, perr := s.Points(ctx, ak); perr == nil {
		s.St.Mu.Lock()
		if a := s.findLocked(accID); a != nil {
			a.Points = float64(pts)
			a.PointsAt = model.NowMilli()
		}
		s.St.Mu.Unlock()
	}
	if err := s.St.Save(); err != nil {
		return nil, err
	}
	return map[string]any{"account_id": accID, "user_id": userID, "project_id": pid, "name": name}, nil
}

func (s *Service) findLocked(id string) *model.Account {
	for _, a := range s.St.Data.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *Service) setAccountError(accountID, msg string) {
	s.St.Mu.Lock()
	if a := s.findLocked(accountID); a != nil {
		a.LastError = msg
	}
	s.St.Mu.Unlock()
}

// upsertKey 写入/刷新账号的项目 AK。
func (s *Service) upsertKey(accountID, projectID, ak string) {
	now := model.NowMilli()
	s.St.Mu.Lock()
	defer s.St.Mu.Unlock()
	for _, k := range s.St.Data.Keys {
		if k.Key == ak {
			k.AccountID = accountID
			k.ProjectID = projectID
			k.Enabled = true
			k.Health = model.Health{State: "ok"}
			return
		}
	}
	k := &model.Key{
		ID: model.RandID("k_", 6), Key: ak, AccountID: accountID, ProjectID: projectID,
		Provider: model.DefaultProvider,
		Enabled: true, Health: model.Health{State: "ok"}, CreatedAt: now,
	}
	s.St.Data.Keys = append(s.St.Data.Keys, k)
	if a := s.findLocked(accountID); a != nil {
		a.KeyIDs = append(a.KeyIDs, k.ID)
	}
}

// AddProjectKey 手动粘贴凭证入库：provider=meoo（或空）时粘贴项目 AK，
// 其他 provider 粘贴该供应商的 API Key（形态不做 Meoo 前缀校验）。
func (s *Service) AddProjectKey(ctx context.Context, ak, label, provider string) (*model.Key, error) {
	ak = strings.TrimSpace(ak)
	prov := model.NormalizeProvider(provider)
	if prov == model.DefaultProvider || prov == "" {
		prov = model.DefaultProvider
		if !model.IsProjectAK(ak) {
			return nil, fmt.Errorf("项目 AK 形态不合法（应以 %s 开头）", model.ProjectAKPrefix)
		}
	}
	if strings.TrimSpace(label) == "" && prov != model.DefaultProvider {
		label = prov
	}
	s.St.Mu.RLock()
	for _, k := range s.St.Data.Keys {
		if k.Key == ak {
			s.St.Mu.RUnlock()
			return nil, fmt.Errorf("该凭证已在池中")
		}
	}
	s.St.Mu.RUnlock()
	k := &model.Key{
		ID: model.RandID("k_", 6), Key: ak, Label: label, ProjectID: "-",
		Provider: prov,
		Enabled: true, Health: model.Health{State: "ok"}, CreatedAt: model.NowMilli(),
	}
	s.St.Mu.Lock()
	s.St.Data.Keys = append(s.St.Data.Keys, k)
	s.St.Mu.Unlock()
	if err := s.St.Save(); err != nil {
		return nil, err
	}
	return k, nil
}

// RefreshAccount 刷新账号积分与探活状态，失败原因写回账号。
func (s *Service) RefreshAccount(ctx context.Context, accountID string) map[string]any {
	out := map[string]any{}
	s.St.Mu.RLock()
	var ak string
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			ak = a.PersonalAK
			break
		}
	}
	s.St.Mu.RUnlock()
	if ak == "" {
		out["error"] = "该账号没有个人 AK"
		return out
	}
	ok := false
	if uid, name, err := s.Identity(ctx, ak); err == nil {
		ok = true
		s.St.Mu.Lock()
		if a := s.findLocked(accountID); a != nil {
			a.UserID = uid
			a.Name = name
			a.CheckOK = true
			a.LastCheckAt = model.NowMilli()
			a.LastError = ""
		}
		s.St.Mu.Unlock()
		out["identity"] = name
	} else {
		s.St.Mu.Lock()
		if a := s.findLocked(accountID); a != nil {
			a.CheckOK = false
			a.LastCheckAt = model.NowMilli()
			a.LastError = err.Error()
		}
		s.St.Mu.Unlock()
		out["error"] = err.Error()
	}
	if ok {
		if pts, err := s.Points(ctx, ak); err == nil {
			s.St.Mu.Lock()
			if a := s.findLocked(accountID); a != nil {
				a.Points = float64(pts)
				a.PointsAt = model.NowMilli()
			}
			s.St.Mu.Unlock()
			out["points"] = pts
		}
	}
	s.St.Touch()
	return out
}

// ---------------- Cookie 接入 ----------------

// IdentityByCookie 用网页会话换取身份。
func (s *Service) IdentityByCookie(ctx context.Context, cookie string) (userID, name string, err error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return "", "", fmt.Errorf("Cookie 为空")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, s.url(webMeEndpoint), nil,
		map[string]string{"Cookie": cookie, "Referer": s.base() + "/"})
	if err != nil {
		return "", "", err
	}
	var d struct {
		Data struct {
			UserID   string `json:"user_id"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.UserID == "" {
		return "", "", fmt.Errorf("Cookie 无效或已过期（HTTP %d）", code)
	}
	return d.Data.UserID, d.Data.Username, nil
}

// approveWithCookie 用网页登录态替用户点"授权登录"，免浏览器。
// 返回后仍需轮询才能取到个人 AK（approve 只回 keyPrefix）。
func (s *Service) approveWithCookie(ctx context.Context, cookie, sid, keyName string) error {
	body := map[string]any{
		"sessionId":      sid,
		"keyName":        keyName,
		"keyDescription": "meoo2api 自动接入（" + time.Now().Format("2006-01-02 15:04") + "）",
		"expiresInDays":  0,
	}
	code, raw, err := s.request(ctx, http.MethodPost, s.url(approveEndpoint), body,
		map[string]string{"Cookie": cookie, "Referer": s.base() + "/cli/login"})
	if err != nil {
		return err
	}
	var d struct {
		Success *bool `json:"success"`
		Data    struct {
			KeyPrefix string `json:"keyPrefix"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || (d.Success != nil && !*d.Success) {
		return fmt.Errorf("授权失败 HTTP %d", code)
	}
	return nil
}

// pollApproved 在本地会话上轮询直到 approved 或超时。
func (s *Service) pollApproved(ctx context.Context, sid string) (DeviceState, error) {
	deadline := time.Now().Add(25 * time.Second)
	for {
		st, err := s.PollDevice(ctx, sid)
		if err != nil {
			return st, err
		}
		switch st.Status {
		case "approved":
			if st.APIKey == "" {
				return st, fmt.Errorf("授权已通过但未拿到凭证，请重试")
			}
			return st, nil
		case "denied", "expired":
			return st, fmt.Errorf("授权被拒绝或已过期（%s）", st.Status)
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("等待授权超时")
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// AttachFromCookie 用一段网页 Cookie 完成接入：
//   - 账号已在库且已有可用项目 AK：只更新 Cookie 与个人 AK，不再建项目
//   - 否则补一次设备授权（用 Cookie 代点 approve），再走项目开通
func (s *Service) AttachFromCookie(ctx context.Context, cookie, label string) (map[string]any, error) {
	cookie = strings.TrimSpace(cookie)
	userID, name, err := s.IdentityByCookie(ctx, cookie)
	if err != nil {
		return nil, err
	}

	// 已有账号：先判断是否只需补 Cookie。
	s.St.Mu.RLock()
	var existing *model.Account
	for _, a := range s.St.Data.Accounts {
		if a.UserID == userID {
			existing = a
			break
		}
	}
	var existingID string
	var hasKey bool
	if existing != nil {
		existingID = existing.ID
		for _, k := range s.St.Data.Keys {
			if k.AccountID == existing.ID && k.Enabled && k.Health.State != "invalid" {
				hasKey = true
				break
			}
		}
	}
	s.St.Mu.RUnlock()

	if existingID != "" && hasKey {
		s.St.Mu.Lock()
		for _, a := range s.St.Data.Accounts {
			if a.ID == existingID {
				a.Cookie = cookie
				if label != "" {
					a.Label = label
				}
				a.LastError = ""
				break
			}
		}
		s.St.Mu.Unlock()
		if err := s.St.Save(); err != nil {
			return nil, err
		}
		return map[string]any{"account_id": existingID, "user_id": userID, "name": name, "cookie_only": true}, nil
	}

	// 需要授权：创建会话 → 用 Cookie approve → 轮询取 AK。
	sid, _, err := s.StartDevice(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.approveWithCookie(ctx, cookie, sid, "meoo2api"); err != nil {
		return nil, err
	}
	st, err := s.pollApproved(ctx, sid)
	if err != nil {
		return nil, err
	}

	s.St.Mu.Lock()
	var acc *model.Account
	for _, a := range s.St.Data.Accounts {
		if a.UserID == userID {
			acc = a
			break
		}
	}
	if acc == nil {
		acc = &model.Account{ID: model.RandID("a_", 6), UserID: userID, Enabled: true, CreatedAt: model.NowMilli()}
		s.St.Data.Accounts = append(s.St.Data.Accounts, acc)
	}
	acc.PersonalAK = st.APIKey
	acc.PersonalAKAt = model.NowMilli()
	acc.Cookie = cookie
	acc.Name = name
	if label != "" {
		acc.Label = label
	} else if acc.Label == "" {
		acc.Label = name
	}
	acc.LastError = ""
	accID := acc.ID
	s.St.Mu.Unlock()
	if err := s.St.Save(); err != nil {
		return nil, err
	}
	// 个人 AK 已落盘，授权会话里的副本立刻清掉。
	s.devMu.Lock()
	if d := s.devices[sid]; d != nil {
		d.consumed = true
		d.status.APIKey = ""
		d.accountID = accID
	}
	s.devMu.Unlock()

	pid, key, err := s.EnsureProject(ctx, accID, "")
	if err != nil {
		s.setAccountError(accID, err.Error())
		return map[string]any{"account_id": accID, "user_id": userID, "stage": "project"}, err
	}
	s.upsertKey(accID, pid, key)
	if pts, perr := s.Points(ctx, st.APIKey); perr == nil {
		s.St.Mu.Lock()
		if a := s.findLocked(accID); a != nil {
			a.Points = float64(pts)
			a.PointsAt = model.NowMilli()
		}
		s.St.Mu.Unlock()
	}
	if err := s.St.Save(); err != nil {
		return nil, err
	}
	return map[string]any{"account_id": accID, "user_id": userID, "name": name, "project_id": pid}, nil
}

// ---------------- 凭证巡检 ----------------

// RepairAll 供巡检调用：对每个账号尝试自愈项目 AK。
// 返回修复成功的账号数。
func (s *Service) RepairAll(ctx context.Context) int {
	s.St.Mu.RLock()
	ids := make([]string, 0, len(s.St.Data.Accounts))
	for _, a := range s.St.Data.Accounts {
		if a.PersonalAK != "" && a.ProjectID != "" {
			ids = append(ids, a.ID)
		}
	}
	s.St.Mu.RUnlock()
	fixed := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return fixed
		}
		if _, changed, err := s.RepairProjectKey(ctx, id); err == nil && changed {
			fixed++
		}
	}
	return fixed
}

// HealthLoop 定期用个人 AK 探活，把失效账号标红。
// 没有 refresh 接口（Meoo 登录走阿里云 SSO），所以这里只能"发现失效"，不能"续期"。
func (s *Service) HealthLoop(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		defer close(done)
		// 启动后先等一轮，避免和服务启动抢流量。
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.St.Mu.RLock()
			ids := make([]string, 0, len(s.St.Data.Accounts))
			for _, a := range s.St.Data.Accounts {
				if a.PersonalAK != "" {
					ids = append(ids, a.ID)
				}
			}
			s.St.Mu.RUnlock()
			for _, id := range ids {
				if ctx.Err() != nil {
					return
				}
				s.refreshQuietly(ctx, id)
				// 个人 AK 探活之后顺带重读项目 AK：
				// 值没变 = 免费探活；值变了 = 自动换血。全程不需要人介入。
				_, _, _ = s.RepairProjectKey(ctx, id)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			s.St.Touch()
		}
	}()
	return done
}

// refreshQuietly 与面板手动刷新同源，但把"凭证失效"与"网络抖动"分开记：
// 只有 401/403 才标记为需要重新接入。
func (s *Service) refreshQuietly(ctx context.Context, accountID string) {
	s.St.Mu.RLock()
	var ak string
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			ak = a.PersonalAK
			break
		}
	}
	s.St.Mu.RUnlock()
	if ak == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	_, name, err := s.Identity(cctx, ak)
	now := model.NowMilli()
	s.St.Mu.Lock()
	a := s.findLocked(accountID)
	if a != nil {
		a.LastCheckAt = now
		if err == nil {
			a.CheckOK = true
			a.LastError = ""
			if name != "" {
				a.Name = name
			}
		} else if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.KindAuth {
			a.CheckOK = false
			a.LastError = "凭证已失效，请重新接入（设备授权或粘贴 Cookie）"
		}
		// 网络类错误不改状态，避免抖动把账号误标红。
	}
	s.St.Mu.Unlock()
}

// ---------------- 项目 AK 自愈 ----------------

// ReadProjectAK 只读项目云环境变量，不建项目、不开通。
func (s *Service) ReadProjectAK(ctx context.Context, accountID string) (projectID, ak string, err error) {
	s.St.Mu.RLock()
	var pak, pid string
	for _, a := range s.St.Data.Accounts {
		if a.ID == accountID {
			pak, pid = a.PersonalAK, a.ProjectID
			break
		}
	}
	s.St.Mu.RUnlock()
	if pak == "" {
		return "", "", fmt.Errorf("该账号没有个人 AK")
	}
	if pid == "" {
		return "", "", fmt.Errorf("该账号还没有项目")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + pak, "X-Meoo-Client": cliClientHead}
	code, raw, err := s.request(ctx, http.MethodGet,
		s.url(projectsEndpoint)+"/"+url.PathEscape(pid)+"/cloud/environment", nil, hdr)
	if err != nil {
		return pid, "", err
	}
	var d struct {
		Data struct {
			Key     string `json:"meooProjectApiKey"`
			Enabled *bool  `json:"enabled"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.Key == "" {
		return pid, "", fmt.Errorf("读取项目凭证失败 HTTP %d", code)
	}
	if d.Data.Enabled != nil && !*d.Data.Enabled {
		return pid, "", fmt.Errorf("项目云服务未启用")
	}
	return pid, d.Data.Key, nil
}

// RepairProjectKey 在项目 AK 失效时重读云环境变量：
//   - 拿到新 AK → 原地替换池里的凭证（保留统计），返回 changed=true
//   - 拿到同值 → 说明 AK 没问题，是别的原因（返回 changed=false）
//
// 这是"保 AK"的核心动作：项目 AK 存在项目云环境变量里，不轮换，
// 但重新读一次就能拿到权威值，不需要重新走设备授权。
func (s *Service) RepairProjectKey(ctx context.Context, accountID string) (string, bool, error) {
	pid, ak, err := s.ReadProjectAK(ctx, accountID)
	if err != nil {
		return "", false, err
	}
	now := model.NowMilli()
	s.St.Mu.Lock()
	defer s.St.Mu.Unlock()
	for _, k := range s.St.Data.Keys {
		if k.AccountID != accountID {
			continue
		}
		if k.Key == ak {
			// 值没变：清掉失效标记，让它重新参与选号
			k.Health = model.Health{State: "ok"}
			k.Enabled = true
			k.LastUsed = now
			return ak, false, nil
		}
		k.Key = ak
		k.ProjectID = pid
		k.Health = model.Health{State: "ok"}
		k.Enabled = true
		return ak, true, nil
	}
	// 账号下没有凭证记录（例如上次入库失败）：补一条
	k := &model.Key{
		ID: model.RandID("k_", 6), Key: ak, AccountID: accountID, ProjectID: pid,
		Enabled: true, Health: model.Health{State: "ok"}, CreatedAt: now,
	}
	s.St.Data.Keys = append(s.St.Data.Keys, k)
	if a := s.findLocked(accountID); a != nil {
		a.KeyIDs = append(a.KeyIDs, k.ID)
	}
	return ak, true, nil
}

// ---------------- 成本采样 ----------------

const maxCostSamples = 200

// CostLoop 每 interval 查一次可用积分，用两次快照相减得出这段时间花了多少分，
// 再把消耗按各模型的 token 增量记录下来。
//
// 为什么不用"每请求前后各查一次"：那是两倍的上游调用，而且积分是整数精度，
// 小额请求的差值会四舍五入成 0。定期快照能把误差摊薄。
func (s *Service) CostLoop(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.sampleCostOnce(ctx)
		}
	}()
	return done
}

// SampleCost 供面板手动触发一次成本采样。
func (s *Service) SampleCost(ctx context.Context) { s.sampleCostOnce(ctx) }

func (s *Service) sampleCostOnce(ctx context.Context) {
	type target struct {
		id, ak string
		base   float64
	}
	s.St.Mu.RLock()
	targets := make([]target, 0, len(s.St.Data.Accounts))
	for _, a := range s.St.Data.Accounts {
		if a.PersonalAK != "" {
			targets = append(targets, target{id: a.ID, ak: a.PersonalAK, base: a.PointsBase})
		}
	}
	s.St.Mu.RUnlock()

	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		pts, err := s.Points(ctx, t.ak)
		if err != nil {
			continue
		}
		now := model.NowMilli()
		s.St.Mu.Lock()
		for _, a := range s.St.Data.Accounts {
			if a.ID != t.id {
				continue
			}
			cur := float64(pts)
			a.Points = cur
			a.PointsAt = now
			// 首次采样只建立基线，不产生样本（否则第一笔 delta 会把历史消耗全算进去）
			if t.base > 0 && t.base >= cur {
				delta := int64(t.base - cur)
				if delta > 0 {
					diff := map[string]int64{}
					for m, v := range a.Usage {
						if d := v - a.UsageSnapshot[m]; d > 0 {
							diff[m] = d
						}
					}
					if len(diff) > 0 {
						a.CostSamples = append(a.CostSamples, model.CostSample{At: now, Delta: delta, Tokens: diff})
						if len(a.CostSamples) > maxCostSamples {
							a.CostSamples = a.CostSamples[len(a.CostSamples)-maxCostSamples:]
						}
						a.Spent += delta
					}
				}
			}
			snap := make(map[string]int64, len(a.Usage))
			for m, v := range a.Usage {
				snap[m] = v
			}
			a.UsageSnapshot = snap
			a.PointsBase = cur
			break
		}
		s.St.Mu.Unlock()
	}
	s.St.Touch()
}
