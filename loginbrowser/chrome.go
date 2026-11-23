package main

import (
	"context"
	"log"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// 这一层只做一件事：用真实浏览器走完阿里云登录，把 meoo 的 Cookie 取出来。
// 不实现任何风控逻辑——风控看到的就是一个真浏览器，短信/账密都由页面自己完成。

const (
	havanaBase = "https://passport.aliyun.com/havanaone/login/login.htm"
	ssoBase    = "https://account.aliyun.com/sso/login.htm"
	meooOrigin = "https://meoo.com"
)

var errNoSuchElement = errors.New("页面元素不存在（可能还没渲染完，或上游改版）")

func chromePath() string {
	if p := os.Getenv("CHROME_PATH"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		return "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	return "chromium"
}

// buildHavanaURL 顶层打开登录组件页。ssoParams 必须带上，登录成功后页面才知道往哪回跳。
func buildHavanaURL(mode, state string) string {
	entrance := "oneday_sms"
	if mode == "pwd" {
		entrance = "oneday_pwd"
	}
	sso, _ := json.Marshal(map[string]string{
		"client_id":     "oneday",
		"response_type": "code",
		"scope":         "openid",
		"state":         state,
		"redirect_uri":  meooOrigin + "/aliyunLogin",
		"biz_variant":   "personal",
	})
	bizPass, _ := json.Marshal(map[string]string{"tenantName": "oneday"})
	q := url.Values{}
	q.Set("lang", "zh_CN")
	q.Set("appName", "oneday")
	q.Set("appEntrance", entrance)
	q.Set("styleType", "vertical")
	q.Set("bizParams", "")
	q.Set("notLoadSsoView", "true")
	// false = 允许"保持登录"：否则阿里只发 session cookie，浏览器一重启就得重新登录，
	// 6 小时一次的调度会变成 4 次/天登录，风控必然介入。
	q.Set("notKeepLogin", "false")
	q.Set("isMobile", "false")
	q.Set("bizPassParams", string(bizPass))
	q.Set("ssoParams", string(sso))
	return havanaBase + "?" + q.Encode()
}

// buildSSOURL 登录成功后回放它，让阿里 SSO 直接签发 code 并跳到 meoo。
func buildSSOURL(state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "oneday")
	q.Set("redirect_uri", meooOrigin+"/aliyunLogin")
	q.Set("scope", "openid")
	q.Set("state", state)
	q.Set("biz_variant", "personal")
	return ssoBase + "?" + q.Encode()
}

type browser struct {
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
}

func launchBrowser(profileDir string) (*browser, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath()),
		chromedp.Flag("no-sandbox", os.Getenv("CHROME_SANDBOX") != "1"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("lang", "zh-CN"),
		chromedp.WindowSize(1280, 900),
	)
	if p := os.Getenv("CHROME_PROXY"); p != "" {
		opts = append(opts, chromedp.ProxyServer(p))
	} else {
		// 环境里只要存在 HTTP(S)_PROXY，Chrome 连回环和阿里云都会去走代理，
		// 表现是 navigate 卡死几十秒。默认直连，需要代理时显式给 CHROME_PROXY。
		opts = append(opts, chromedp.Flag("no-proxy-server", true))
	}
	if profileDir != "" {
		opts = append(opts, chromedp.UserDataDir(profileDir))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx,
		chromedp.WithErrorf(func(f string, a ...any) { log.Printf("[cdp] "+f, a...) }),
		chromedp.WithLogf(func(f string, a ...any) { log.Printf("[cdp] "+f, a...) }))

	// 🔴 预热必须用长生命周期 ctx：chromedp 把浏览器绑定在"首次 Run 用的那个 ctx"上。
	// 若首次 Run 发生在带 timeout 的派生 ctx 里，调用方 cancel 时浏览器会被一并关掉，
	// 表现为 navigate 看似成功、后续所有调用都报 context canceled。
	if err := chromedp.Do(ctx, chromedp.Navigate("about:blank")); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("浏览器启动失败: %w", err)
	}
	return &browser{ctx: ctx, cancel: cancel, allocCancel: allocCancel}, nil
}

func (b *browser) close() {
	b.cancel()
	b.allocCancel()
}

func (b *browser) op(timeout time.Duration, steps ...chromedp.Action[chromedp.Void]) error {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Do(ctx, steps...)
}

func (b *browser) eval(timeout time.Duration, expr string) (string, error) {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, chromedp.Evaluate[string](expr))
}

// waitInput 等表单渲染。阿里云这个组件是异步挂载的，navigate 返回不代表可交互。
func (b *browser) waitInput(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// 注意：Evaluate[string] 要求 JS 返回字符串；直接写 !!x 会返回布尔，
	// 解码成 string 会报 "cannot unmarshal bool into Go value of type string"。
	expr := fmt.Sprintf(`String(!!document.getElementById(%q))`, id)
	for time.Now().Before(deadline) {
		ok, err := b.eval(5*time.Second, expr)
		if err == nil && ok == "true" {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待 #%s 超时：%w", id, errNoSuchElement)
}

// setInput 用原生 setter 写值并派发 input/change，兼容页面自己的框架绑定。
func (b *browser) setInput(id, val string) error {
	vb, _ := json.Marshal(val)
	expr := fmt.Sprintf(`(() => {
	  const el = document.getElementById(%q);
	  if (!el) return 'no-el';
	  el.focus();
	  const d = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
	  d.set.call(el, %s);
	  el.dispatchEvent(new Event('input', { bubbles: true }));
	  el.dispatchEvent(new Event('change', { bubbles: true }));
	  return el.value;
	})()`, id, string(vb))
	got, err := b.eval(8*time.Second, expr)
	if err != nil {
		return fmt.Errorf("写入 #%s 失败: %w", id, err)
	}
	if got == "no-el" {
		return fmt.Errorf("#%s 不存在", id)
	}
	if got != val {
		return fmt.Errorf("#%s 回读不一致（写入 %q，读到 %q），页面可能重渲染了", id, val, got)
	}
	return nil
}

// clickBySelector 直接派发 click；阿里云的"获取验证码"是 <a href="javascript:void(0)">。
func (b *browser) clickBySelector(sel string) error {
	sb, _ := json.Marshal(sel)
	got, err := b.eval(8*time.Second, fmt.Sprintf(`(() => {
	  const el = document.querySelector(%s);
	  if (!el) return 'no-el';
	  el.scrollIntoView({block:'center'});
	  el.click();
	  return 'ok';
	})()`, string(sb)))
	if err != nil {
		return err
	}
	if got == "no-el" {
		return fmt.Errorf("选择器 %s 未命中：%w", sel, errNoSuchElement)
	}
	return nil
}

// ensureChecked 把复选框置为「已勾选」，幂等。
// 🔴 阿里云这版登录页的协议框默认就是勾上的，无脑 click 等于把它取消：
// 表现是点「获取验证码」毫无反应、页面只提示「请阅读并同意协议」、一条短信都不发，
// 而调用方拿到的是空 note —— 面板于是对外宣称「验证码已发送」，用户永远等不到码。
func (b *browser) ensureChecked(id string) error {
	expr := fmt.Sprintf(`(() => {
	  const el = document.getElementById(%q);
	  if (!el) return 'no-el';
	  if (el.checked) return 'ok';
	  el.scrollIntoView({block:'center'});
	  el.click();
	  return el.checked ? 'ok' : 'unchecked';
	})()`, id)
	got, err := b.eval(8*time.Second, expr)
	if err != nil {
		return fmt.Errorf("勾选 #%s 失败: %w", id, err)
	}
	switch got {
	case "ok":
		return nil
	case "no-el":
		return fmt.Errorf("#%s 不存在", id)
	default:
		return fmt.Errorf("#%s 点击后仍未勾上（%s）", id, got)
	}
}

// clickButtonByText 提交按钮没有稳定 id，按可见文字点。
func (b *browser) clickButtonByText(text string) error {
	// 页面上是 "登录 / 注册"，实际 innerText 可能带各种空白；两边都归一化再比。
	norm := strings.Join(strings.Fields(text), "")
	tb, _ := json.Marshal(norm)
	got, err := b.eval(8*time.Second, fmt.Sprintf(`(() => {
	  const nodes = [...document.querySelectorAll('button, [role=button], .fm-button')];
	  const hit = nodes.find(n => (n.innerText || '').replace(/\s+/g, '') === %s);
	  if (!hit) return 'no-el';
	  hit.scrollIntoView({block:'center'});
	  hit.click();
	  return 'ok';
	})()`, string(tb)))
	if err != nil {
		return err
	}
	if got == "no-el" {
		return fmt.Errorf("找不到文字为 %q 的提交按钮：%w", text, errNoSuchElement)
	}
	return nil
}

type pageState struct {
	URL              string `json:"url"`
	Title            string `json:"title"`
	Text             string `json:"text"`
	CheckcodeVisible bool   `json:"checkcode_visible"`
	SendBtn          string `json:"send_btn"`
}

// pageState 采集"人眼能看到的信息"，用于判断风控是否升级（图片验证码是否冒出来）。
func (b *browser) pageState() (pageState, error) {
	raw, err := b.eval(10*time.Second, `(() => {
	  const el = document.getElementById('fm-login-checkcode');
	  const vis = !!(el && el.offsetParent && el.getBoundingClientRect().height > 0);
	  const btn = document.querySelector('.send-btn-link');
	  return JSON.stringify({
	    url: location.href,
	    title: document.title,
	    text: (document.body ? document.body.innerText : '').replace(/\s+/g, ' ').slice(0, 400),
	    checkcode_visible: vis,
	    send_btn: btn ? (btn.innerText || '') : ''
	  });
	})()`)
	if err != nil {
		return pageState{}, err
	}
	var st pageState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return pageState{}, err
	}
	return st, nil
}

// clipText 截断页面文本：错误信息里带一点上下文就够，别把整页正文塞进响应。
func clipText(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func (b *browser) screenshot() ([]byte, error) {
	ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
	defer cancel()
	return chromedp.Run(ctx, chromedp.CaptureScreenshot())
}

func (b *browser) navigate(u string, wait time.Duration) error {
	return b.op(30*time.Second, chromedp.Navigate(u), chromedp.Sleep(wait))
}

// cookiesFor 取某个站点名下所有 Cookie（含 HttpOnly）。
func (b *browser) cookiesFor(origin string) ([]*network.Cookie, error) {
	ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
	defer cancel()
	return chromedp.Run(ctx, func(ctx context.Context, t *chromedp.Target) ([]*network.Cookie, error) {
		res, err := cdp.Call(ctx, t, network.GetCookies, network.GetCookiesParams{URLs: []string{origin}})
		if err != nil {
			return nil, err
		}
		return res.Cookies, nil
	})
}

// isMeooURL 必须按 host 判断：登录页的 query 里就带着 redirect_uri=https://meoo.com/aliyunLogin，
// 用 strings.Contains 会把"还没登录"误判成"已经到 meoo"。
func isMeooURL(raw string) bool {
	p, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(p.Hostname())
	return h == "meoo.com" || strings.HasSuffix(h, ".meoo.com")
}

func cookieHeader(cs []*network.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func findCookie(cs []*network.Cookie, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// harvest 是整条链路唯一的未验证假设所在：登录完回放 SSO，靠阿里会话换 meoo 的 code。
func (b *browser) harvest(state string, timeout time.Duration) (cookieHeaderStr, landingURL string, err error) {
	deadline := time.Now().Add(timeout)
	// 先看有没有已经落在 meoo 上（登录成功时页面可能自己跳了）
	for time.Now().Before(deadline) {
		if u, e := b.eval(5*time.Second, `location.href`); e == nil && isMeooURL(u) {
			landingURL = u
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if landingURL == "" {
		if err := b.navigate(buildSSOURL(state), 2*time.Second); err != nil {
			return "", "", fmt.Errorf("回放 SSO 失败: %w", err)
		}
		for time.Now().Before(deadline) {
			u, e := b.eval(5*time.Second, `location.href`)
			if e == nil && isMeooURL(u) {
				landingURL = u
				break
			}
			time.Sleep(700 * time.Millisecond)
		}
	}
	cs, cerr := b.cookiesFor("https://meoo.com")
	if cerr != nil {
		return "", landingURL, fmt.Errorf("读 meoo Cookie 失败: %w", cerr)
	}
	if findCookie(cs, "login_oneday_ticket") == "" {
		if landingURL == "" {
			return "", "", fmt.Errorf("没等到 meoo 落地页，也没拿到 login_oneday_ticket；当前仍在 %s", b.currentURL())
		}
		return "", landingURL, fmt.Errorf("已到 meoo 但缺 login_oneday_ticket，可能 SSO 未授权或 Cookie 未写入")
	}
	return cookieHeader(cs), landingURL, nil
}

func (b *browser) currentURL() string {
	u, err := b.eval(5*time.Second, `location.href`)
	if err != nil {
		return "?"
	}
	return u
}

func randState() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

var _ = sync.Mutex{}
