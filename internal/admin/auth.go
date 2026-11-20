// Package admin 提供控制台管理 API 与会话鉴权。
package admin

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
)

const (
	sessionCookie = "m2a_sess"
	sessionTTL    = 24 * time.Hour
	maxSessions   = 64
)

type session struct {
	expires time.Time
	ip      string
}

// Sessions 是内存会话表。
type Sessions struct {
	mu   sync.Mutex
	data map[string]session
}

// NewSessions 构造会话表。
func NewSessions() *Sessions { return &Sessions{data: map[string]session{}} }

func (s *Sessions) create(ip string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.data {
		if now.After(v.expires) {
			delete(s.data, k)
		}
	}
	if len(s.data) >= maxSessions {
		// 淘汰最早过期的
		var oldest string
		var t time.Time
		for k, v := range s.data {
			if oldest == "" || v.expires.Before(t) {
				oldest, t = k, v.expires
			}
		}
		delete(s.data, oldest)
	}
	s.data[tok] = session{expires: now.Add(sessionTTL), ip: ip}
	return tok
}

func (s *Sessions) valid(tok, ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[tok]
	if !ok {
		return false
	}
	if time.Now().After(v.expires) {
		delete(s.data, tok)
		return false
	}
	return true
}

func (s *Sessions) drop(tok string) {
	s.mu.Lock()
	delete(s.data, tok)
	s.mu.Unlock()
}

// authed 判断请求是否已登录。
func (h *Handler) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return h.Sessions.valid(c.Value, clientIP(r))
}

// sameOrigin 拒绝跨站写操作（防 CSRF）。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // 非浏览器客户端（curl / 脚本）没有 Origin
	}
	host := r.Host
	return strings.Contains(origin, "://"+host) || strings.HasSuffix(origin, "//"+host)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// Login 校验密码并下发会话 Cookie。
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	// 简单防爆破：同一 IP 连续失败递增等待（内存计数，重启清零）。
	if !h.allowLogin(clientIP(r)) {
		writeErr(w, 429, "too_many_attempts", "尝试过于频繁，请稍后再试")
		return
	}
	h.St.Mu.RLock()
	enc := h.St.Data.Config.AdminPass
	h.St.Mu.RUnlock()
	ok, legacy := store.CheckPassword(in.Password, enc)
	if !ok {
		h.noteLoginFail(clientIP(r))
		writeErr(w, 401, "bad_password", "密码不正确")
		return
	}
	if legacy {
		h.St.Mu.Lock()
		h.St.Data.Config.AdminPass = store.HashPassword(in.Password)
		h.St.Mu.Unlock()
		_ = h.St.Save()
	}
	h.clearLoginFail(clientIP(r))
	tok := h.Sessions.create(clientIP(r))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// 登录失败节流。
func (h *Handler) allowLogin(ip string) bool {
	h.failMu.Lock()
	defer h.failMu.Unlock()
	f, ok := h.loginFails[ip]
	if !ok {
		return true
	}
	if f.count < 5 {
		return true
	}
	return time.Now().After(f.until)
}

func (h *Handler) noteLoginFail(ip string) {
	h.failMu.Lock()
	defer h.failMu.Unlock()
	f := h.loginFails[ip]
	f.count++
	if f.count >= 5 {
		wait := time.Duration(f.count-4) * 10 * time.Second
		if wait > 5*time.Minute {
			wait = 5 * time.Minute
		}
		f.until = time.Now().Add(wait)
	}
	h.loginFails[ip] = f
}

func (h *Handler) clearLoginFail(ip string) {
	h.failMu.Lock()
	delete(h.loginFails, ip)
	h.failMu.Unlock()
}

type failState struct {
	count int
	until time.Time
}

// NewClientKey 签发一个 sk-… 客户端密钥。
func NewClientKey(label string) *model.ClientKey {
	k := "sk-" + model.RandID("", 16)
	return &model.ClientKey{ID: model.RandID("c_", 6), Key: k, Label: label, Enabled: true, CreatedAt: model.NowMilli()}
}
