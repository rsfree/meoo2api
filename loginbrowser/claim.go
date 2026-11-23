package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
)

// claim 模式：无人值守地"保活登录态 + 领每日积分"。
//
// 与 server 模式的区别只有三点：
//   1. profile 持久化（默认 /data/profile），否则每次都是"新设备"，风控必炸；
//   2. 失败不抛给人，输出机器可读 JSON（exit 2 = 需要人工介入）；
//   3. 不用开 HTTP，跑完即退，交给 cron/循环调度。
//
// 它解决不了的那件事要说清楚：阿里主账号会话有 48h（可调 72h）硬上限，
// 到期后必须重新走一次登录。本模式的作用是把"重新登录"从人工变成自动，
// 而不是让会话永不过期。

// evalAwait 支持 await，用于在页面上下文里直接调 fetch。
// chromedp v0.24 是泛型 API：Evaluate[T] 直接返回值，等待 Promise 要显式挂 EvalAwaitPromise。
func (b *browser) evalAwait(expr string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, chromedp.Evaluate[string](expr, chromedp.EvalAwaitPromise))
}

const meFetchExpr = `(async () => {
  try {
    const r = await fetch(%q, {credentials: 'include', method: %q, headers: {'Content-Type':'application/json'}, body: %s});
    const t = await r.text();
    return JSON.stringify({status: r.status, body: t.slice(0, 900)});
  } catch (e) { return JSON.stringify({status: 0, body: 'fetch_error: ' + String(e)}); }
})()`

func pageFetch(b *browser, path, method, body string, timeout time.Duration) (int, string, error) {
	if body == "" {
		body = "null"
	}
	expr := fmt.Sprintf(meFetchExpr, path, method, body)
	raw, err := b.evalAwait(expr, timeout)
	if err != nil {
		return 0, "", err
	}
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return 0, raw, err
	}
	return out.Status, out.Body, nil
}

func runClaim() {
	profile := env("PROFILE_DIR", "/data/profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		fail(map[string]any{"stage": "profile", "error": err.Error()})
	}
	// 单实例锁：循环调度和手动触发可能撞车，两个 chromium 抢同一个 profile 会直接起不来。
	lock, err := acquireLock(profile)
	if err != nil {
		out(map[string]any{"ok": true, "skipped": "另一个 claim 进程正在使用该 profile"}, 0)
	}
	defer lock.Close()
	// 容器重建后 hostname 会变，旧的 SingletonLock 会把 profile 判成"别的机器占着"，
	// 表现为永远起不来。持锁期间清掉是安全的。
	clearStaleSingleton(profile)

	loginID := os.Getenv("MEOO_LOGIN_ID")
	loginPass := os.Getenv("MEOO_LOGIN_PASS")

	res := map[string]any{"at": time.Now().Format(time.RFC3339), "profile": profile}

	b, err := launchBrowser(profile)
	if err != nil {
		fail(map[string]any{"stage": "launch", "error": err.Error()})
	}
	defer b.close()

	// 第 1 步：能不能白嫖现有会话（SSO 还活着就不用登录）
	login := "none"
	if err := b.navigate(meooOrigin+"/", 3*time.Second); err != nil {
		fail(map[string]any{"stage": "navigate", "error": err.Error()})
	}
	if code, body, err := pageFetch(b, "/api/v1/user/me", "GET", "", 25*time.Second); err == nil && code == 200 && strings.Contains(body, `"data":{`) {
		login = "reused"
	} else if err != nil {
		res["me_error"] = err.Error()
	}

	// 第 2 步：没会话就走账密登录
	needsHuman := ""
	if login != "reused" {
		if loginID == "" || loginPass == "" {
			fail(map[string]any{"stage": "creds", "error": "未配置 MEOO_LOGIN_ID / MEOO_LOGIN_PASS", "login": "none"})
		}
		state := randState()
		if err := b.navigate(buildHavanaURL("pwd", state), 2*time.Second); err != nil {
			fail(map[string]any{"stage": "open_login", "error": err.Error()})
		}
		if err := b.waitInput("fm-login-id", 35*time.Second); err != nil {
			st, _ := b.pageState()
			fail(map[string]any{"stage": "form", "error": err.Error(), "page": st})
		}
		if err := b.setInput("fm-login-id", loginID); err != nil {
			fail(map[string]any{"stage": "fill_id", "error": err.Error()})
		}
		if err := b.setInput("fm-login-password", loginPass); err != nil {
			fail(map[string]any{"stage": "fill_pass", "error": err.Error()})
		}
		// 🔴 协议框默认就是勾上的（checked=true，只是 CSS 隐藏）。
		// 上一版无条件 el.click() 反而把它取消勾选 → 提交按钮变灰 → 点了没反应，
		// 表现为"登录后未回到 meoo"这种假故障。这里改成读状态、只补不取消。
		if err := b.ensureAgree(); err != nil {
			fail(map[string]any{"stage": "agree", "error": err.Error()})
		}
		time.Sleep(300 * time.Millisecond)

		submitted := false
		if err := b.clickButtonByText("登录"); err == nil {
			submitted = true
		} else if err := b.clickButtonByText("登录/注册"); err == nil {
			submitted = true
		}
		if !submitted {
			// 兜底：直接点表单里的 submit
			if err := b.clickBySelector("button.fm-submit, .fm-button"); err != nil {
				st, _ := b.pageState()
				fail(map[string]any{"stage": "submit", "error": err.Error(), "page": st})
			}
		}

		// 第 3 步：等结果（成功跳 meoo / 验证码 / 密码错误）
		deadline := time.Now().Add(70 * time.Second)
		for time.Now().Before(deadline) {
			st, _ := b.pageState()
			if isMeooURL(st.URL) {
				login = "pwd"
				break
			}
			if st.CheckcodeVisible {
				needsHuman = "图片验证码"
				break
			}
			for _, kw := range []string{"密码错误", "账号或密码", "账户或密码", "验证码", "存在风险", "安全验证"} {
				if strings.Contains(st.Text, kw) {
					needsHuman = kw
					break
				}
			}
			if needsHuman != "" {
				break
			}
			time.Sleep(1500 * time.Millisecond)
		}
		if login != "pwd" && needsHuman == "" {
			needsHuman = "登录后未回到 meoo"
		}
	}

	res["login"] = login
	if needsHuman != "" {
		res["needs_human"] = needsHuman
		st, _ := b.pageState()
		res["page"] = st
		if shot, err := b.screenshot(); err == nil {
			p := profile + "/last_fail.png"
			_ = os.WriteFile(p, shot, 0o600)
			res["screenshot"] = p
		}
		out(map[string]any{"ok": false, "result": res}, 2)
	}

	// 第 4 步：领积分（这一步必须发生在"已登录"的 meoo 页面里）
	if !isMeooURL(b.currentURL()) {
		if err := b.navigate(meooOrigin+"/", 4*time.Second); err != nil {
			fail(map[string]any{"stage": "back_to_meoo", "error": err.Error(), "result": res})
		}
	}
	code, body, err := pageFetch(b, "/api/mc/entitlement/init", "POST", "{}", 35*time.Second)
	if err != nil {
		fail(map[string]any{"stage": "init", "error": err.Error(), "result": res})
	}
	res["init_status"] = code
	res["init_body"] = body

	if code == 200 {
		var j struct {
			Success bool `json:"success"`
			Data    struct {
				DailyLoginGranted bool    `json:"dailyLoginGranted"`
				Amount            float64 `json:"dailyLoginGrantAmount"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &j); err == nil {
			res["granted"] = j.Data.DailyLoginGranted
			res["amount"] = j.Data.Amount
		}
	}

	// 第 5 步：报余额（用同会话的 points 接口，别用 AK，省一次外部依赖）
	if code, body, err := pageFetch(b, "/api/mc/entitlement/get", "GET", "", 25*time.Second); err == nil && code == 200 {
		var j struct {
			Data struct {
				UserPointInfo struct {
					Account struct {
						AvailableBalance float64 `json:"availableBalance"`
					} `json:"account"`
				} `json:"userPointInfo"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &j); err == nil {
			res["balance"] = j.Data.UserPointInfo.Account.AvailableBalance
		}
	}

	// 会话能不能跨进程复用，全看这个 cookie 是持久型还是 session 型（Expires=-1）。
	// 它是我们下一步能不能"每 6 小时跑一次而不重复登录"的唯一依据。
	if cks, err := b.cookiesFor(meooOrigin); err == nil {
		for _, c := range cks {
			if c.Name == "login_oneday_ticket" {
				if c.Expires > 0 {
					res["ticket_expires"] = time.Unix(int64(c.Expires), 0).Format(time.RFC3339)
				} else {
					res["ticket_expires"] = "session"
				}
			}
		}
	}

	res["ok"] = true
	out(map[string]any{"ok": true, "result": res}, 0)
}

// ensureAgree 保证协议框处于勾选状态：默认已是 true，只在 false 时才补点 label。
func (b *browser) ensureAgree() error {
	expr := `(() => {
	  const el = document.getElementById('fm-agreement-checkbox');
	  if (!el) return 'no-el';
	  if (!el.checked) {
	    const lb = document.querySelector('label[for="fm-agreement-checkbox"]');
	    if (lb) { lb.click(); } else { el.click(); }
	  }
	  return String(!el.checked ? 'still-unchecked' : 'checked');
	})()`
	got, err := b.eval(8*time.Second, expr)
	if err != nil {
		return err
	}
	if got == "no-el" {
		return fmt.Errorf("找不到协议勾选框 #fm-agreement-checkbox")
	}
	if got == "still-unchecked" {
		return fmt.Errorf("协议勾选框未能置为选中")
	}
	return nil
}

// acquireLock 用 flock 保证同一 profile 只有一个 chromium。
func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".claim.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func clearStaleSingleton(dir string) {
	for _, n := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); err == nil {
			_ = os.Remove(p)
		}
	}
}

func out(v any, code int) {	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	os.Exit(code)
}

func fail(v map[string]any) {
	v["ok"] = false
	out(v, 1)
}
