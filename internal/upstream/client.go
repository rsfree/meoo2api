// Package upstream 封装对 Meoo OpenAI 兼容网关的 HTTP 调用与错误分类。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// 错误种类决定账号处置动作：只有 Retryable 的才换号。
const (
	KindOK         = "ok"
	KindQuota      = "quota"       // 403 QUOTA_EXCEEDED：余额不足
	KindAuth       = "auth"        // 401/403：凭证失效
	KindRate       = "rate"        // 429：限流
	KindServer     = "server"      // 5xx：上游故障
	KindBadRequest = "bad_request" // 400 等：请求本身有问题，换号无意义
	KindNetwork    = "network"     // 连接失败
	// KindTimeout 与 KindNetwork 分开：超时要按 504 暴露，而不是 502 connection。
	// 之前 ctx 超时被归进 network，于是同一个 180 秒超时在运行日志里
	// 时而 504（读 body 报 DeadlineExceeded）时而 502（channel 关闭无终态），同一件事两种语义。
	KindTimeout  = "timeout"  // 连接已建但上游不回响应头 / 上下文超时
	KindCanceled = "canceled" // 客户端取消
)

// Error 描述一次可分类的上游失败。
type Error struct {
	Status  int
	Code    string
	Message string
	Kind    string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("上游 %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("上游 %d: %s", e.Status, e.Message)
}

// Retryable 表示"换个账号同一请求可能成功"。
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindQuota, KindAuth, KindRate, KindServer, KindTimeout:
		return true
	}
	return false
}

// Client 是共享连接池的上游 HTTP 客户端。
type Client struct {
	Base string
	HTTP *http.Client
}

// New 构造客户端；base 形如 https://api.meoo.host/meoo-ai/compatible-mode/v1
func New(base string) *Client {
	dial := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// 优先 IPv4：上游域名偶尔只解析出 AAAA，而部署机（如 ai-prod）没有 IPv6 路由，
		// 直连会得到 "dial tcp [2408:...]:443: connect: network is unreachable"，
		// 表现是「目录突然空了 / 所有请求 502」。先试 tcp4，失败再按原样回退，
		// 这样有 IPv6 的机器行为不变，没 IPv6 的机器不会被一条 AAAA 记录打死。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network == "tcp" {
				if c, err := dial.DialContext(ctx, "tcp4", addr); err == nil {
					return c, nil
				}
			}
			return dial.DialContext(ctx, network, addr)
		},
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout(),
		ExpectContinueTimeout: 3 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Transport: tr}}
}

// DoJSON 发送 JSON 请求并读回原始响应体；非 2xx 会解析为 *Error。
func (c *Client) DoJSON(ctx context.Context, method, endpoint string, body any, hdr map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, classifyNet(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, classifyNet(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Meoo 部分接口用 HTTP 200 + {"success":false,"errCode":…} 表达业务失败，
		// 直接当成功会把错误数据当成正常响应透给客户端。
		if be := parseBusinessEnvelope(raw); be != nil {
			return resp.StatusCode, raw, be
		}
		return resp.StatusCode, raw, nil
	}
	return resp.StatusCode, raw, parseError(resp.StatusCode, raw)
}

// DoStream 发起流式请求，成功时返回未读取的响应体，由调用方消费并关闭。
func (c *Client) DoStream(ctx context.Context, endpoint string, body any, hdr map[string]string) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, classifyNet(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return nil, parseError(resp.StatusCode, raw)
}

// MergeHeader 返回带鉴权的最小头集合。
func MergeHeader(ak string, extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + ak}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func classifyNet(err error) error {
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: KindCanceled, Message: "客户端取消"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Message: "上游超时"}
	}
	// ResponseHeaderTimeout 触发的是 net.Error 的 Timeout，而不是 ctx 的
	// DeadlineExceeded（ctx 可能还有余量）。不单独判的话会被当成普通连接失败，
	// 一个「迟迟不给响应头」的上游会被报成 502 connection。
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Message: "上游响应头超时"}
	}
	return &Error{Kind: KindNetwork, Message: err.Error()}
}

// headerTimeout 是「连接已建立、但上游迟迟不回响应头」的上限。
//
// 为什么需要它：实测上游在首字节前整段静默（qwen3.8-max 首字节 P50 12s、最坏 72s），
// 且响应头与首帧数据几乎同时到达。没有这个上限时，一个卡死的上游只能靠 ctx 的
// 180s 兜底——连接空占三分钟、积分白扣、客户端干等。
// 调大不会让正常请求变慢，只是在更早的时刻把「确定卡死」判出来。
func headerTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("MEOO_HEADER_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

// parseBusinessEnvelope 识别 HTTP 200 的业务失败信封。
func parseBusinessEnvelope(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var a struct {
		Success *bool  `json:"success"`
		ErrCode string `json:"errCode"`
		Message string `json:"message"`
	}
	if json.Unmarshal(trimmed, &a) != nil || a.Success == nil || *a.Success {
		return nil
	}
	return &Error{Status: http.StatusOK, Code: a.ErrCode, Message: a.Message, Kind: ClassifyBusiness(a.ErrCode, a.Message)}
}

// ClassifyBusiness 按业务错误码/文案推断处置动作。
//
// 导出是为了让 gateway 处理「HTTP 200 但首帧就是 error 对象」时复用同一套判据：
// 之前那类错误被硬编码成 KindBadRequest，于是一条夹在 SSE 里的限流/额度错误
// 既不会被换号重试，也不会以 429 返回给客户端，而是变成 502 upstream_error。
func ClassifyBusiness(code, msg string) string {
	u := strings.ToUpper(code + " " + msg)
	switch {
	case strings.Contains(u, "QUOTA"), strings.Contains(msg, "余额"), strings.Contains(msg, "额度"), strings.Contains(msg, "积分不足"):
		return KindQuota
	case strings.Contains(u, "OVERLOAD"), strings.Contains(u, "TOOBUSY"), strings.Contains(msg, "繁忙"):
		return KindServer
	case strings.Contains(u, "RATE"), strings.Contains(u, "LIMIT"), strings.Contains(u, "FREQUENT"), strings.Contains(msg, "频繁"):
		return KindRate
	case strings.Contains(u, "AUTH"), strings.Contains(u, "TOKEN"), strings.Contains(u, "LOGIN"), strings.Contains(u, "UNAUTHORIZED"):
		return KindAuth
	}
	return KindBadRequest
}

// parseError 把上游错误体归一化为 *Error。
// 兼容两种信封：{"error":{...}} 与 {"success":false,"errCode":"…","message":"…"}
func parseError(status int, raw []byte) error {
	if status >= 200 && status < 300 {
		if be := parseBusinessEnvelope(raw); be != nil {
			return be
		}
	}
	code, msg := "", strings.TrimSpace(string(raw))
	if len(raw) > 0 {
		var a struct {
			Error struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
				Type    string `json:"type"`
			} `json:"error"`
			Success *bool  `json:"success"`
			ErrCode string `json:"errCode"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &a) == nil {
			switch {
			case a.Error.Message != "":
				msg = a.Error.Message
				switch c := a.Error.Code.(type) {
				case string:
					code = c
				case float64:
					code = fmt.Sprintf("%v", c)
				}
				if code == "" {
					code = a.Error.Type
				}
			case a.ErrCode != "" || a.Message != "":
				code, msg = a.ErrCode, a.Message
			}
		}
	}
	kind := KindBadRequest
	switch {
	case status == http.StatusPaymentRequired:
		// 402 在所有 HTTP 语义里只有"需要付款/额度不足"这一含义。
		// 上游常把它包成 invalid_request_error，必须先于内层码判定。
		kind = KindQuota
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = KindAuth
		if strings.Contains(strings.ToUpper(code), "QUOTA") || strings.Contains(strings.ToUpper(msg), "余额") ||
			strings.Contains(strings.ToUpper(msg), "QUOTA") || strings.Contains(msg, "额度") {
			kind = KindQuota
		}
	case status == http.StatusTooManyRequests:
		kind = KindRate
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || status >= 500:
		kind = KindServer
	}
	return &Error{Status: status, Code: code, Message: msg, Kind: kind}
}
