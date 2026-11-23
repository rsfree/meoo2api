package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 服务端浏览器登录 sidecar：只监听回环，由 meoo2api 主服务代理调用。
// 目标不是"复刻登录协议"，而是"让真浏览器把登录做完，我们只取 Cookie"。

type session struct {
	id         string
	mode       string // sms | pwd
	state      string
	profile    string
	b          *browser
	inviteCode string // 非空时，登录落地后自动触发邀请归因

	mu      sync.Mutex
	created time.Time
	lastUse time.Time
	status  string // ready | submitted | harvested | error
	note    string
}

type manager struct {
	mu       sync.Mutex
	sessions map[string]*session
	max      int
	ttl      time.Duration
}

func newManager() *manager {
	max := 2
	if v := os.Getenv("MAX_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	ttl := 300 * time.Second
	if v := os.Getenv("SESSION_TTL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	m := &manager{sessions: map[string]*session{}, max: max, ttl: ttl}
	go m.sweep()
	return m
}

func (m *manager) sweep() {
	for range time.Tick(20 * time.Second) {
		var dead []*session
		m.mu.Lock()
		for id, s := range m.sessions {
			s.mu.Lock()
			idle := time.Since(s.lastUse)
			s.mu.Unlock()
			if idle > m.ttl {
				dead = append(dead, s)
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		for _, s := range dead {
			log.Printf("回收会话 %s（空闲超过 %s）", s.id, m.ttl)
			s.close()
		}
	}
}

func (s *session) close() {
	s.b.close()
	if s.profile != "" {
		// Chrome 会写大量小文件，异步删，别挡住请求
		go os.RemoveAll(filepath.Clean(s.profile))
	}
}

func (m *manager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func main() {
	// 子命令 claim：无人值守领取每日积分（跑完即退，给 cron 用）
	if len(os.Args) > 1 && os.Args[1] == "claim" {
		runClaim()
		return
	}
	addr := env("BIND", "127.0.0.1") + ":" + env("PORT", "8792")
	key := os.Getenv("LOGIN_KEY")
	m := newManager()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "sessions": m.count(), "chrome": chromePath()})
	})
	mux.HandleFunc("POST /v1/sessions", guard(key, m.create))
	mux.HandleFunc("GET /v1/sessions/{id}/screen", guard(key, m.screen))
	mux.HandleFunc("GET /v1/sessions/{id}/state", guard(key, m.stateOf))
	mux.HandleFunc("POST /v1/sessions/{id}/action", guard(key, m.action))
	mux.HandleFunc("POST /v1/sessions/{id}/harvest", guard(key, m.harvest))
	mux.HandleFunc("DELETE /v1/sessions/{id}", guard(key, m.drop))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("login-browser 监听 %s，chrome=%s，上限 %d 会话", addr, chromePath(), m.max)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// guard 做两件事：共享密钥校验；非回环地址且没配密钥时直接拒绝（防止裸奔）。
func guard(key string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key != "" {
			got := r.Header.Get("X-Login-Key")
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				writeJSON(w, 401, map[string]any{"ok": false, "error": "密钥不正确"})
				return
			}
		} else {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				writeJSON(w, 403, map[string]any{"ok": false, "error": "未配置 LOGIN_KEY，只允许回环访问"})
				return
			}
		}
		h(w, r)
	}
}

func (m *manager) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode       string `json:"mode"`
		InviteCode string `json:"invite_code"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in)
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "sms"
	}
	if mode != "sms" && mode != "pwd" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "mode 只支持 sms / pwd"})
		return
	}
	if m.count() >= m.max {
		writeJSON(w, 429, map[string]any{"ok": false, "error": fmt.Sprintf("并发会话已达上限 %d", m.max)})
		return
	}

	dir, err := os.MkdirTemp("", "meoo-login-*")
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": "无法创建临时 profile: " + err.Error()})
		return
	}
	b, err := launchBrowser(dir)
	if err != nil {
		os.RemoveAll(dir)
		writeJSON(w, 500, map[string]any{"ok": false, "error": "启动浏览器失败: " + err.Error()})
		return
	}

	s := &session{
		id: randState()[:16], mode: mode, state: randState(), profile: dir, b: b,
		inviteCode: strings.TrimSpace(in.InviteCode),
		created: time.Now(), lastUse: time.Now(), status: "starting",
	}

	entryID := "#fm-sms-login-id"
	if mode == "pwd" {
		entryID = "#fm-login-id"
	}
	go func() {
		if err := b.navigate(buildHavanaURL(mode, s.state), 1*time.Second); err != nil {
			log.Printf("会话 %s 打开登录页失败: %v", s.id, err)
			s.mu.Lock()
			s.status, s.note = "error", "打开登录页失败: "+err.Error()
			s.mu.Unlock()
			return
		}
		if err := b.waitInput(strings.TrimPrefix(entryID, "#"), 25*time.Second); err != nil {
			log.Printf("会话 %s 等待表单失败: %v（当前 %s）", s.id, err, b.currentURL())
			s.mu.Lock()
			s.status, s.note = "error", err.Error()
			s.mu.Unlock()
			return
		}
		log.Printf("会话 %s 就绪", s.id)
		s.mu.Lock()
		s.status, s.lastUse = "ready", time.Now()
		s.mu.Unlock()
	}()

	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	// 给表单一点渲染时间，但别让调用方干等太久
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		st := s.status
		s.mu.Unlock()
		if st != "starting" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	s.mu.Lock()
	resp := map[string]any{"ok": s.status != "error", "id": s.id, "mode": s.mode, "status": s.status, "note": s.note}
	s.mu.Unlock()
	if resp["status"] == "error" {
		writeJSON(w, 502, resp)
		return
	}
	writeJSON(w, 200, resp)
}

func (m *manager) get(w http.ResponseWriter, r *http.Request) *session {
	id := r.PathValue("id")
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "会话不存在或已回收"})
		return nil
	}
	return s
}

func (m *manager) screen(w http.ResponseWriter, r *http.Request) {
	s := m.get(w, r)
	if s == nil {
		return
	}
	s.mu.Lock()
	shot, err := s.b.screenshot()
	s.lastUse = time.Now()
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": "截图失败: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(shot)
}

func (m *manager) stateOf(w http.ResponseWriter, r *http.Request) {
	s := m.get(w, r)
	if s == nil {
		return
	}
	s.mu.Lock()
	st, err := s.b.pageState()
	s.lastUse = time.Now()
	out := map[string]any{"ok": err == nil, "id": s.id, "mode": s.mode, "status": s.status, "note": s.note}
	s.mu.Unlock()
	if err != nil {
		out["error"] = err.Error()
	}
	out["page"] = st
	writeJSON(w, 200, out)
}

func (m *manager) action(w http.ResponseWriter, r *http.Request) {
	s := m.get(w, r)
	if s == nil {
		return
	}
	var in struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUse = time.Now()

	var err error
	note := ""
	switch strings.ToLower(in.Type) {
	case "phone":
		err = s.b.setInput("fm-sms-login-id", in.Value)
	case "code":
		err = s.b.setInput("fm-smscode", in.Value)
	case "account":
		err = s.b.setInput("fm-login-id", in.Value)
	case "password":
		err = s.b.setInput("fm-login-password", in.Value)
	case "agree":
		err = s.b.clickBySelector("#fm-agreement-checkbox")
	case "getcode":
		if err = s.b.clickBySelector(".send-btn-link"); err == nil {
			time.Sleep(1500 * time.Millisecond)
			if st, e := s.b.pageState(); e == nil {
				if st.CheckcodeVisible {
					note = "图片验证码已弹出：风控介入，需要人工识别后填 #fm-login-checkcode"
				}
				if st.SendBtn != "" && st.SendBtn != "获取验证码" {
					note = strings.TrimSpace(note + " 按钮状态：" + st.SendBtn)
				}
			}
		}
	case "submit":
		label := "登录 / 注册"
		if s.mode == "pwd" {
			label = "登录"
		}
		if err = s.b.clickButtonByText(label); err == nil {
			time.Sleep(3 * time.Second)
			s.status = "submitted"
			if isMeooURL(s.b.currentURL()) {
				note = "已跳转到 meoo，可执行 harvest 取 Cookie"
			}
		}
	default:
		writeJSON(w, 400, map[string]any{"ok": false, "error": "未知 action：" + in.Type})
		return
	}

	st, _ := s.b.pageState()
	if err != nil {
		s.status = "error"
		s.note = err.Error()
		writeJSON(w, 502, map[string]any{"ok": false, "id": s.id, "status": s.status, "error": err.Error()})
		return
	}
	if s.note == "" {
		s.note = note
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": s.id, "status": s.status, "note": note, "page": st})
}

func (m *manager) harvest(w http.ResponseWriter, r *http.Request) {
	s := m.get(w, r)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUse = time.Now()

	cookies, landing, err := s.b.harvest(s.state, 40*time.Second)
	if err != nil {
		s.status, s.note = "error", err.Error()
		writeJSON(w, 502, map[string]any{"ok": false, "id": s.id, "status": s.status, "error": err.Error(), "landing": landing})
		return
	}

	// 邀请归因：Meoo 前端没有独立的 bind 接口，归因发生在
	// GET /api/growth/invite/notifications?invite=<code>（bundle 里 Fut 函数，
	// 登录成功落地后由 fromInvite 分支触发）。我们在页面上下文原样复刻这次
	// fetch——同域 + credentials，等价于前端自己的调用。
	invite := map[string]any{"requested": false}
	if code := s.inviteCode; code != "" {
		invite["requested"] = true
		u := meooOrigin + "/api/growth/invite/notifications?invite=" + url.QueryEscape(code)
		expr := fmt.Sprintf(inviteFetchExpr, u)
		out, ierr := s.b.evalAwait(expr, 10*time.Second)
		if ierr != nil {
			invite["ok"] = false
			invite["error"] = ierr.Error()
		} else {
			invite["raw"] = out
			var parsed struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
			}
			if json.Unmarshal([]byte(out), &parsed) == nil {
				invite["http"] = parsed.Status
				invite["ok"] = parsed.Status == 200
			}
		}
		log.Printf("会话 %s 邀请归因 code=%s 结果=%v", s.id, code, invite["ok"])
	}

	s.status, s.note = "harvested", "已拿到 meoo Cookie"
	writeJSON(w, 200, map[string]any{
		"ok": true, "id": s.id, "status": s.status, "landing": landing,
		"cookies": cookies, "bytes": len(cookies), "invite": invite,
	})
}

// inviteFetchExpr 在页面上下文发起同域 fetch；模板里只注入完整 URL。
const inviteFetchExpr = `(async () => {
  try {
    const r = await fetch(%q, {credentials: 'include'});
    const t = await r.text();
    return JSON.stringify({status: r.status, body: t.slice(0, 400)});
  } catch (e) { return JSON.stringify({status: 0, body: 'fetch_error: ' + String(e)}); }
})()`

func (m *manager) drop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s == nil {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "会话不存在"})
		return
	}
	s.close()
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
