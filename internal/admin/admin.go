package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/catalog"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/reqlog"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// Handler 处理 /admin/* 请求；ServeHTTP 返回 true 表示路径已归属管理端。
type Handler struct {
	St      *store.Store
	Picker  *pool.Picker
	Acc     *account.Service
	Catalog *catalog.Service
	Up      *upstream.Client
	Log     *reqlog.Log
	Start   time.Time

	Sessions   *Sessions
	failMu     sync.Mutex
	loginFails map[string]failState
}

// New 构造管理端。
func New(st *store.Store, pk *pool.Picker, acc *account.Service, cat *catalog.Service, up *upstream.Client, lg *reqlog.Log) *Handler {
	return &Handler{
		St: st, Picker: pk, Acc: acc, Catalog: cat, Up: up, Log: lg, Start: time.Now(),
		Sessions: NewSessions(), loginFails: map[string]failState{},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg, "code": code})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.UseNumber()
	return dec.Decode(v)
}

// ServeHTTP 实现管理端路由。返回 false 表示不是 /admin 请求。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "/admin" || path == "/admin/" {
		path = "/admin"
	}
	if !strings.HasPrefix(path, "/admin") {
		if strings.HasPrefix(path, "/api/") {
			// 面板前端约定的短路径
			return h.route(w, r, "/admin"+path)
		}
		return false
	}
	h.route(w, r, path)
	return true
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, path string) bool {
	// 公开端点
	switch path {
	case "/admin/api/login":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method", "方法不支持")
			return true
		}
		h.login(w, r)
		return true
	case "/admin/api/logout":
		if c, err := r.Cookie(sessionCookie); err == nil {
			h.Sessions.drop(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		writeJSON(w, 200, map[string]any{"ok": true})
		return true
	case "/admin/api/ping":
		writeJSON(w, 200, map[string]any{"ok": true, "authed": h.authed(r), "service": "meoo2api", "uptime": int64(time.Since(h.Start).Seconds())})
		return true
	}

	if !h.authed(r) {
		writeErr(w, 401, "unauthorized", "请先登录")
		return true
	}
	if r.Method == http.MethodPost && !sameOrigin(r) {
		writeErr(w, 403, "cross_origin", "拒绝跨站请求")
		return true
	}

	switch {
	case path == "/admin/api/state":
		h.state(w, r)
	case path == "/admin/api/device/start":
		h.deviceStart(w, r)
	case path == "/admin/api/device/poll":
		h.devicePoll(w, r)
	case path == "/admin/api/device/attach":
		h.deviceAttach(w, r)
	case path == "/admin/api/cookie/attach":
		h.cookieAttach(w, r)
	case path == "/admin/api/account/refresh":
		h.accountRefresh(w, r)
	case path == "/admin/api/account/update":
		h.accountUpdate(w, r)
	case path == "/admin/api/account/delete":
		h.accountDelete(w, r)
	case path == "/admin/api/account/project":
		h.accountProject(w, r)
	case path == "/admin/api/account/repair":
		h.accountRepair(w, r)
	case path == "/admin/api/keys/add":
		h.keyAdd(w, r)
	case path == "/admin/api/keys/action":
		h.keyAction(w, r)
	case path == "/admin/api/clientkeys/create":
		h.clientKeyCreate(w, r)
	case path == "/admin/api/clientkeys/action":
		h.clientKeyAction(w, r)
	case path == "/admin/api/config":
		h.configUpdate(w, r)
	case path == "/admin/api/logs":
		h.logs(w, r)
	case path == "/admin/api/cost/sample":
		h.Acc.SampleCost(r.Context())
		writeJSON(w, 200, map[string]any{"ok": true})
	case path == "/admin/api/catalog/refresh":
		h.Catalog.Refresh(r.Context())
		writeJSON(w, 200, map[string]any{"ok": true, "status": h.Catalog.Status()})
	case path == "/admin/api/tasks/overview":
		h.tasksOverview(w, r)
	case path == "/admin/api/tasks/checkin":
		h.tasksCheckin(w, r)
	case path == "/admin/api/tasks/invite":
		h.tasksInvite(w, r)
	case path == "/admin/api/test":
		h.test(w, r)
	default:
		writeErr(w, 404, "not_found", "未知管理接口")
	}
	return true
}

// ---------------- 状态总览 ----------------

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	now := model.NowMilli()
	h.St.Mu.RLock()
	accs := make([]map[string]any, 0, len(h.St.Data.Accounts))
	for _, a := range h.St.Data.Accounts {
		keys := make([]map[string]any, 0)
		for _, k := range h.St.Data.Keys {
			if k.AccountID == a.ID {
				keys = append(keys, keyView(k, now))
			}
		}
		accs = append(accs, map[string]any{
			"cost": costView(a),
			"id":   a.ID, "user_id": a.UserID, "name": a.Name, "label": a.Label,
			"enabled": a.Enabled, "paused": a.Paused, "created_at": a.CreatedAt,
			"personal_ak": model.Mask(a.PersonalAK), "has_personal_ak": a.PersonalAK != "",
			"project_id": a.ProjectID, "points": a.Points, "points_at": a.PointsAt,
			"last_error": a.LastError, "last_check_at": a.LastCheckAt, "check_ok": a.CheckOK,
			"has_cookie": a.Cookie != "",
			"checkin_at": a.CheckinAt, "checkin_date": a.CheckinDate,
			"checkin_amount": a.CheckinAmount, "checkin_err": a.CheckinErr,
			"keys": keys,
		})
	}
	orphan := make([]map[string]any, 0)
	for _, k := range h.St.Data.Keys {
		bound := false
		for _, a := range h.St.Data.Accounts {
			if a.ID == k.AccountID {
				bound = true
				break
			}
		}
		if !bound {
			orphan = append(orphan, keyView(k, now))
		}
	}
	cks := make([]map[string]any, 0, len(h.St.Data.ClientKeys))
	for _, c := range h.St.Data.ClientKeys {
		cks = append(cks, map[string]any{
			"id": c.ID, "key": model.Mask(c.Key), "label": c.Label, "enabled": c.Enabled,
			"created_at": c.CreatedAt, "requests": c.Requests, "tokens": c.Tokens, "last_used": c.LastUsed,
		})
	}
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	h.St.Mu.RUnlock()

	ms, stale, ferr := h.Catalog.List()
	models := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		models = append(models, map[string]any{
			"id": m.ID, "label": m.Label, "provider": m.Provider,
			"context_window": m.ContextWindow, "max_tokens": m.MaxTokens,
			"multimodal": m.Multimodal, "description": m.Description,
			"alias": m.OwnedBy == "meoo-alias",
		})
	}
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"now":         now,
		"uptime_sec":  int64(time.Since(h.Start).Seconds()),
		"accounts":    accs,
		"orphan_keys": orphan,
		"client_keys": cks,
		"config":      cfg,
		"models":      models,
		"catalog": map[string]any{
			"status": h.Catalog.Status(), "stale": stale, "error": ferr,
		},
		"stats":      h.Log.Stats(),
		"usable":     len(h.Picker.Usable()),
		"save_error": h.St.LastError(),
	})
}

// costView 汇总某账号的用量与花费，并给出按 token 占比分摊的分模型估算。
func costView(a *model.Account) map[string]any {
	totalTokens := int64(0)
	for _, v := range a.Usage {
		totalTokens += v
	}
	perModel := map[string]map[string]any{}
	// 每个样本的 delta 按该样本内各模型 token 占比分摊（估算：假设窗口内单价一致）
	alloc := map[string]float64{}
	for _, smp := range a.CostSamples {
		sum := int64(0)
		for _, v := range smp.Tokens {
			sum += v
		}
		if sum == 0 {
			continue
		}
		for m, v := range smp.Tokens {
			alloc[m] += float64(smp.Delta) * float64(v) / float64(sum)
		}
	}
	for m, tok := range a.Usage {
		item := map[string]any{"tokens": tok}
		if cost, ok := alloc[m]; ok && cost > 0 {
			item["cost"] = int64(cost + 0.5)
			if tok > 0 {
				item["tokens_per_credit"] = int64(float64(tok) / cost)
			}
		}
		perModel[m] = item
	}
	// 单价只能用"被采样覆盖"的 token 算：a.Usage 里含采样开始前的历史用量，
	// 拿它除累计花费会得出虚高的单价。
	sampledTokens := int64(0)
	for _, smp := range a.CostSamples {
		for _, v := range smp.Tokens {
			sampledTokens += v
		}
	}
	out := map[string]any{
		"tokens": totalTokens, "sampled_tokens": sampledTokens,
		"spent": a.Spent, "samples": len(a.CostSamples),
		"per_model": perModel, "points_base": a.PointsBase,
	}
	if a.Spent > 0 && sampledTokens > 0 {
		out["tokens_per_credit"] = int64(float64(sampledTokens) / float64(a.Spent))
	}
	if n := len(a.CostSamples); n > 0 {
		out["last_sample_at"] = a.CostSamples[n-1].At
	}
	return out
}

func keyView(k *model.Key, now int64) map[string]any {
	cool := int64(0)
	if k.Health.Until > now {
		cool = (k.Health.Until - now) / 1000
	}
	return map[string]any{
		"id": k.ID, "key": model.Mask(k.Key), "project_id": k.ProjectID, "label": k.Label,
		"enabled": k.Enabled, "state": k.Health.State, "reason": k.Health.Reason,
		"until": k.Health.Until, "cooldown_sec": cool,
		"requests": k.Requests, "tokens": k.Tokens, "fails": k.Fails, "last_used": k.LastUsed,
	}
}

// ---------------- 设备授权 ----------------

func (h *Handler) deviceStart(w http.ResponseWriter, r *http.Request) {
	sid, login, err := h.Acc.StartDevice(r.Context())
	if err != nil {
		writeErr(w, 502, "device_start_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "session_id": sid, "login_url": login, "expires_in": 900})
}

func (h *Handler) devicePoll(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	st, err := h.Acc.PollDevice(r.Context(), sid)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "status": st.Status, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": st.Status, "user_id": st.UserID, "username": st.Username})
}

func (h *Handler) deviceAttach(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	out, err := h.Acc.AttachFromDevice(ctx, in.SessionID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "detail": out})
		return
	}
	h.Catalog.Refresh(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "result": out})
}

// cookieAttach 用一段网页 Cookie 完成接入（免浏览器）。
// Cookie 是敏感凭证：不写日志、不回显，只在响应里给出身份与结果。
func (h *Handler) cookieAttach(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cookie string `json:"cookie"`
		Label  string `json:"label"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	if len(in.Cookie) > 8192 {
		writeErr(w, 400, "cookie_too_large", "Cookie 过长")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	out, err := h.Acc.AttachFromCookie(ctx, in.Cookie, in.Label)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "detail": out})
		return
	}
	h.Catalog.Refresh(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "result": out})
}

// ---------------- 账号运维 ----------------

func (h *Handler) accountRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	writeJSON(w, 200, map[string]any{"ok": true, "result": h.Acc.RefreshAccount(ctx, in.ID)})
}

func (h *Handler) accountUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string  `json:"id"`
		Enabled *bool   `json:"enabled"`
		Paused  *bool   `json:"paused"`
		Label   *string `json:"label"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	h.St.Mu.Lock()
	var found bool
	for _, a := range h.St.Data.Accounts {
		if a.ID == in.ID {
			found = true
			if in.Enabled != nil {
				a.Enabled = *in.Enabled
			}
			if in.Paused != nil {
				a.Paused = *in.Paused
			}
			if in.Label != nil {
				a.Label = *in.Label
			}
			break
		}
	}
	h.St.Mu.Unlock()
	if !found {
		writeErr(w, 404, "not_found", "账号不存在")
		return
	}
	_ = h.St.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	h.St.Mu.Lock()
	keepAcc := h.St.Data.Accounts[:0]
	for _, a := range h.St.Data.Accounts {
		if a.ID != in.ID {
			keepAcc = append(keepAcc, a)
		}
	}
	h.St.Data.Accounts = keepAcc
	keepKey := h.St.Data.Keys[:0]
	for _, k := range h.St.Data.Keys {
		if k.AccountID != in.ID {
			keepKey = append(keepKey, k)
		}
	}
	h.St.Data.Keys = keepKey
	h.St.Mu.Unlock()
	_ = h.St.Save()
	writeJSON(w, 200, map[string]any{"ok": true, "note": "仅清理本地凭证，未删除远端 Meoo 项目"})
}

func (h *Handler) accountProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Second)
	defer cancel()
	pid, ak, err := h.Acc.EnsureProject(ctx, in.ID, "")
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "project_id": pid})
		return
	}
	h.Catalog.Refresh(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "project_id": pid, "key": model.Mask(ak)})
}

// accountRepair 只重读项目云环境变量，不建项目、不开通。
func (h *Handler) accountRepair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	ak, changed, err := h.Acc.RepairProjectKey(ctx, in.ID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed, "key": model.Mask(ak),
		"message": map[bool]string{true: "已更新为新凭证", false: "云端值与本地一致"}[changed]})
}

// ---------------- 凭证 ----------------

func (h *Handler) keyAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	k, err := h.Acc.AddProjectKey(r.Context(), in.Key, in.Label)
	if err != nil {
		writeErr(w, 400, "add_failed", err.Error())
		return
	}
	h.Catalog.Refresh(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "id": k.ID})
}

func (h *Handler) keyAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"` // probe | release | toggle | delete
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	h.St.Mu.RLock()
	var target *model.Key
	for _, k := range h.St.Data.Keys {
		if k.ID == in.ID {
			cp := *k
			target = &cp
			break
		}
	}
	h.St.Mu.RUnlock()
	if target == nil {
		writeErr(w, 404, "not_found", "凭证不存在")
		return
	}
	switch in.Action {
	case "probe":
		ok, msg := h.Picker.Probe(r.Context(), target)
		if ok {
			writeJSON(w, 200, map[string]any{"ok": true, "message": msg})
			return
		}
		// 探活失败先别下结论：项目 AK 存在项目云环境变量里，重读一次往往就能拿到权威值。
		if target.AccountID != "" {
			if _, changed, rerr := h.Acc.RepairProjectKey(r.Context(), target.AccountID); rerr == nil {
				if changed {
					writeJSON(w, 200, map[string]any{"ok": true, "message": "凭证已从云端重读并更新", "repaired": true})
					return
				}
				writeJSON(w, 200, map[string]any{"ok": false, "message": "云端凭证与本地一致，问题不在 AK：" + msg})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"ok": false, "message": msg})
		return
	case "repair":
		if target.AccountID == "" {
			writeJSON(w, 200, map[string]any{"ok": false, "message": "该凭证未绑定账号，无法重读"})
			return
		}
		ak, changed, err := h.Acc.RepairProjectKey(r.Context(), target.AccountID)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "message": map[bool]string{true: "已更新为新凭证", false: "云端值与本地一致"}[changed], "key": model.Mask(ak)})
		return
	case "release":
		h.Picker.Release(in.ID)
		_ = h.St.Save()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case "toggle":
		h.St.Mu.Lock()
		for _, k := range h.St.Data.Keys {
			if k.ID == in.ID {
				k.Enabled = !k.Enabled
			}
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case "delete":
		h.St.Mu.Lock()
		out := h.St.Data.Keys[:0]
		for _, k := range h.St.Data.Keys {
			if k.ID != in.ID {
				out = append(out, k)
			}
		}
		h.St.Data.Keys = out
		h.St.Mu.Unlock()
		_ = h.St.Save()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeErr(w, 400, "bad_action", "未知操作")
}

// ---------------- 客户端密钥 ----------------

func (h *Handler) clientKeyCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
		Key   string `json:"key"` // 可选：自定义密钥值（如 sk-meoo2api），留空则随机
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	if strings.TrimSpace(in.Label) == "" {
		in.Label = "api-key"
	}
	ck, err := NewClientKeyWith(strings.TrimSpace(in.Label), in.Key)
	if err != nil {
		writeErr(w, 400, "bad_key", err.Error())
		return
	}
	h.St.Mu.Lock()
	for _, c := range h.St.Data.ClientKeys {
		if c.Key == ck.Key {
			h.St.Mu.Unlock()
			writeErr(w, 409, "duplicate_key", "该密钥值已存在")
			return
		}
	}
	h.St.Data.ClientKeys = append(h.St.Data.ClientKeys, ck)
	h.St.Mu.Unlock()
	if err := h.St.Save(); err != nil {
		writeErr(w, 500, "save_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": ck.Key, "id": ck.ID, "note": "完整密钥只显示这一次"})
}

func (h *Handler) clientKeyAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"` // toggle | delete
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	h.St.Mu.Lock()
	switch in.Action {
	case "toggle":
		for _, c := range h.St.Data.ClientKeys {
			if c.ID == in.ID {
				c.Enabled = !c.Enabled
			}
		}
	case "delete":
		out := h.St.Data.ClientKeys[:0]
		for _, c := range h.St.Data.ClientKeys {
			if c.ID != in.ID {
				out = append(out, c)
			}
		}
		h.St.Data.ClientKeys = out
	default:
		h.St.Mu.Unlock()
		writeErr(w, 400, "bad_action", "未知操作")
		return
	}
	h.St.Mu.Unlock()
	_ = h.St.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- 配置 ----------------

func (h *Handler) configUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DefaultMaxTokens  *int               `json:"default_max_tokens"`
		RequestTimeoutSec *int               `json:"request_timeout_sec"`
		QuotaCooldownSec  *int               `json:"quota_cooldown_sec"`
		RateCooldownSec   *int               `json:"rate_cooldown_sec"`
		Failover          *bool              `json:"failover"`
		KeepaliveSec      *int               `json:"keepalive_sec"`
		Aliases           *map[string]string `json:"aliases"`
		AliasNotes        *map[string]string `json:"alias_notes"`
		HiddenModels      *[]string          `json:"hidden_models"`
		CheckinAuto       *bool              `json:"checkin_auto"`
		CheckinTime       *string            `json:"checkin_time"`
		NewPassword       string             `json:"new_password"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	h.St.Mu.Lock()
	c := &h.St.Data.Config
	if in.DefaultMaxTokens != nil {
		if *in.DefaultMaxTokens < 0 {
			h.St.Mu.Unlock()
			writeErr(w, 400, "bad_value", "default_max_tokens 不能为负")
			return
		}
		c.DefaultMaxTokens = *in.DefaultMaxTokens
	}
	if in.RequestTimeoutSec != nil {
		v := *in.RequestTimeoutSec
		if v < 30 {
			v = 30
		}
		if v > 900 {
			v = 900
		}
		c.RequestTimeoutSec = v
	}
	if in.QuotaCooldownSec != nil && *in.QuotaCooldownSec >= 0 {
		c.QuotaCooldownSec = *in.QuotaCooldownSec
	}
	if in.RateCooldownSec != nil && *in.RateCooldownSec >= 0 {
		c.RateCooldownSec = *in.RateCooldownSec
	}
	if in.Failover != nil {
		c.Failover = *in.Failover
	}
	if in.KeepaliveSec != nil && *in.KeepaliveSec >= 0 {
		c.KeepaliveSec = *in.KeepaliveSec
	}
	if in.Aliases != nil {
		m := map[string]string{}
		for k, v := range *in.Aliases {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "" && strings.TrimSpace(v) != "" {
				m[k] = strings.TrimSpace(v)
			}
		}
		c.Aliases = m
	}
	if in.AliasNotes != nil {
		m := map[string]string{}
		for k, v := range *in.AliasNotes {
			k = strings.ToLower(strings.TrimSpace(k))
			// 别名已删掉时，备注跟着消失，别留孤儿键
			if _, ok := c.Aliases[k]; ok && strings.TrimSpace(v) != "" {
				m[k] = strings.TrimSpace(v)
			}
		}
		c.AliasNotes = m
	}
	if in.HiddenModels != nil {
		c.HiddenModels = *in.HiddenModels
	}
	if in.CheckinAuto != nil {
		c.CheckinAuto = *in.CheckinAuto
	}
	if in.CheckinTime != nil {
		v := strings.TrimSpace(*in.CheckinTime)
		if v == "" {
			v = "09:00"
		}
		var hh, mm int
		if _, err := fmt.Sscanf(v, "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			h.St.Mu.Unlock()
			writeErr(w, 400, "bad_value", "自动签到时刻需形如 09:00")
			return
		}
		// 统一成补零格式：面板里显示和比较都靠它
		c.CheckinTime = fmt.Sprintf("%02d:%02d", hh, mm)
	}
	if in.NewPassword != "" {
		if len(in.NewPassword) < 8 {
			h.St.Mu.Unlock()
			writeErr(w, 400, "weak_password", "密码至少 8 位")
			return
		}
		c.AdminPass = store.HashPassword(in.NewPassword)
	}
	h.St.Mu.Unlock()
	if err := h.St.Save(); err != nil {
		writeErr(w, 500, "save_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- 日志 ----------------

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &limit); n != 1 || err != nil {
			limit = 200
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":    true,
		"items": h.Log.Recent(reqlog.Filter{Channel: q.Get("ch"), Status: q.Get("status"), Limit: limit}),
		"stats": h.Log.Stats(),
	})
}

// ---------------- 在线调试 ----------------

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model    string `json:"model"`
		Prompt   string `json:"prompt"`
		MaxToken int    `json:"max_tokens"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeErr(w, 400, "empty_prompt", "请输入测试内容")
		return
	}
	key := h.Picker.Pick(map[string]bool{})
	if key == nil {
		writeErr(w, 503, "no_available_key", "号池中没有可用凭证")
		return
	}
	mdl := h.Catalog.Resolve(in.Model)
	body := map[string]any{
		"model": mdl, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": in.Prompt}},
	}
	if in.MaxToken > 0 {
		body["max_tokens"] = in.MaxToken
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	started := time.Now()
	code, raw, err := h.Up.DoJSON(ctx, http.MethodPost, h.Up.Base+"/chat/completions", body, upstream.MergeHeader(key.Key, nil))
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		if ue, ok := err.(*upstream.Error); ok && ue.Retryable() {
			h.Picker.Cooldown(key.ID, ue)
		}
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "elapsed_ms": elapsed, "http": code})
		return
	}
	h.Picker.MarkOK(key.ID, mdl, 0)
	writeJSON(w, 200, map[string]any{"ok": true, "elapsed_ms": elapsed, "model": mdl, "account": key.Label, "raw": json.RawMessage(raw)})
}
