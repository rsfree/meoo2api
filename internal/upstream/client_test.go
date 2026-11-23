package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		code   string
		retry  bool
	}{
		{"余额不足", 403, `{"error":{"message":"额度不足，请充值","code":"QUOTA_EXCEEDED"}}`, KindQuota, "QUOTA_EXCEEDED", true},
		{"鉴权失败", 401, `{"error":{"message":"invalid token","code":"UNAUTHORIZED"}}`, KindAuth, "UNAUTHORIZED", true},
		{"速率限制", 429, `{"error":{"message":"too many requests"}}`, KindRate, "", true},
		{"上游故障", 503, `{"error":{"message":"unavailable"}}`, KindServer, "", true},
		{"参数错误不换号", 400, `{"error":{"message":"messages is required","code":"BAD_REQUEST"}}`, KindBadRequest, "BAD_REQUEST", false},
		{"业务信封余额", 200, `{"success":false,"errCode":"QUOTA_EXCEEDED","message":"积分不足"}`, KindQuota, "QUOTA_EXCEEDED", true},
		{"业务信封格式", 400, `{"success":false,"errCode":"SERVICE_AK_FORMAT_INVALID","message":"项目服务 AK 格式非法"}`, KindBadRequest, "SERVICE_AK_FORMAT_INVALID", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := parseError(c.status, []byte(c.body))
			ue, ok := err.(*Error)
			if !ok {
				t.Fatalf("期望 *Error，得到 %T", err)
			}
			if ue.Kind != c.want {
				t.Errorf("Kind = %q，期望 %q", ue.Kind, c.want)
			}
			if c.code != "" && ue.Code != c.code {
				t.Errorf("Code = %q，期望 %q", ue.Code, c.code)
			}
			if ue.Retryable() != c.retry {
				t.Errorf("Retryable = %v，期望 %v", ue.Retryable(), c.retry)
			}
		})
	}
}

// timeoutErr 模拟 ResponseHeaderTimeout 触发的 net.Error：
// 它满足 net.Error 的 Timeout()，但不是 context.DeadlineExceeded。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyNet(t *testing.T) {
	if ue, ok := classifyNet(context.Canceled).(*Error); !ok || ue.Kind != KindCanceled {
		t.Fatalf("取消应归为 canceled，得到 %+v", classifyNet(context.Canceled))
	}
	// 超时必须单独归类：它要映射成 504，折进 network 会变成 502 connection。
	if ue, ok := classifyNet(context.DeadlineExceeded).(*Error); !ok || ue.Kind != KindTimeout {
		t.Fatalf("上下文超时应归为 timeout，得到 %+v", classifyNet(context.DeadlineExceeded))
	}
	if ue, ok := classifyNet(timeoutErr{}).(*Error); !ok || ue.Kind != KindTimeout {
		t.Fatalf("响应头超时应归为 timeout，得到 %+v", classifyNet(timeoutErr{}))
	}
	if ue, ok := classifyNet(errors.New("dial tcp: refused")).(*Error); !ok || ue.Kind != KindNetwork {
		t.Fatalf("连接失败应归为 network")
	}
}

// ResponseHeaderTimeout：上游连接建好却迟迟不回响应头时，必须按「超时」收场，
// 而不是干等到 ctx 的 180s，也不该被归成 502 connection。
func TestResponseHeaderTimeout(t *testing.T) {
	t.Setenv("MEOO_HEADER_TIMEOUT_SEC", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.DoStream(context.Background(), srv.URL+"/x", map[string]any{"a": 1}, nil)
	if err == nil {
		t.Fatal("上游不回响应头应超时")
	}
	ue, ok := err.(*Error)
	if !ok || ue.Kind != KindTimeout {
		t.Fatalf("应归为 timeout，得到 %+v", err)
	}
}

func TestMergeHeaderKeepsAuth(t *testing.T) {
	h := MergeHeader("meoo_project_x", map[string]string{"X-Meoo-Client": "cli"})
	if h["Authorization"] != "Bearer meoo_project_x" {
		t.Fatalf("Authorization 头错误: %v", h)
	}
	if h["X-Meoo-Client"] != "cli" {
		t.Fatalf("额外头丢失: %v", h)
	}
}

func TestErrorStatusShape(t *testing.T) {
	// 400 不应被误判为可换号，避免重复计费
	e := &Error{Status: http.StatusBadRequest, Kind: KindBadRequest, Message: "bad"}
	if e.Retryable() {
		t.Fatal("400 不应可重试")
	}
}
