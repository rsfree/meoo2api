package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meoo2api/internal/upstream"
)

// 契约：HTTP 402 无条件判额度不足，不看内层误导性的 type/code。
func Test402AlwaysQuota(t *testing.T) {
	err := &upstream.Error{Status: 402, Code: "invalid_request_error", Message: "Insufficient Balance", Kind: upstream.KindQuota}
	spec := specOf(err)
	if spec.Status != 429 || spec.Code != "insufficient_quota" || spec.Type != "rate_limit_error" {
		t.Fatalf("402 应归一为 429/insufficient_quota，得到 %+v", spec)
	}
	if spec.Retryable {
		t.Fatal("额度不足不应标记可重试")
	}
}

// 契约：结构化 code 优先于文本，文本只用于分类。
func TestStructuredCodeBeatsText(t *testing.T) {
	// message 提到 rate limit，但 code 明确是余额不足 → 必须判额度
	err := &upstream.Error{Status: 429, Code: "insufficient_quota", Message: "rate limit reached, but quota empty"}
	if spec := specOf(err); spec.Code != "insufficient_quota" {
		t.Fatalf("结构化 code 应优先，得到 %+v", spec)
	}
	// 反之：code 是限流，文本提到余额 → 仍判限流
	err = &upstream.Error{Status: 429, Code: "rate_limit_exceeded", Message: "insufficient balance mentioned in text"}
	if spec := specOf(err); spec.Code != "rate_limit_exceeded" {
		t.Fatalf("结构化 code 优先于文本，得到 %+v", spec)
	}
}

func TestStatusFallback(t *testing.T) {
	cases := []struct {
		err  *upstream.Error
		want string
	}{
		{&upstream.Error{Status: 400}, "invalid_request"},
		{&upstream.Error{Status: 401}, "authentication"},
		{&upstream.Error{Status: 403}, "permission"},
		{&upstream.Error{Status: 404}, "not_found"},
		{&upstream.Error{Status: 408}, "request_timeout"},
		{&upstream.Error{Status: 413}, "request_too_large"},
		{&upstream.Error{Status: 429}, "rate_limit"},
		{&upstream.Error{Status: 500}, "server"},
		{&upstream.Error{Status: 502}, "connection"},
		{&upstream.Error{Status: 503}, "unavailable"},
		{&upstream.Error{Status: 504}, "timeout"},
		{&upstream.Error{Status: 418}, "unknown"},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("status %d → %q，期望 %q", c.err.Status, got, c.want)
		}
	}
}

func TestContextLengthAndContentFilter(t *testing.T) {
	ctx := &upstream.Error{Status: 400, Code: "context_length_exceeded"}
	if spec := specOf(ctx); spec.Code != "context_length_exceeded" || spec.Status != 400 {
		t.Fatalf("上下文超限映射错误: %+v", spec)
	}
	cf := &upstream.Error{Status: 400, Message: "blocked by content filter"}
	if spec := specOf(cf); spec.Code != "content_filter" {
		t.Fatalf("内容过滤应按文本分类，得到 %+v", spec)
	}
	// Ark 族前缀变体
	ark := &upstream.Error{Status: 400, Code: "InputTextSensitiveContentDetected"}
	if spec := specOf(ark); spec.Code != "content_filter" {
		t.Fatalf("Ark 敏感内容前缀匹配失败: %+v", spec)
	}
}

func TestNetworkAndCancel(t *testing.T) {
	if spec := specOf(&upstream.Error{Kind: upstream.KindNetwork}); spec.Code != "connection_error" {
		t.Fatalf("连接失败应映射 connection_error: %+v", spec)
	}
	if spec := specOf(context.Canceled); spec.Code != "stream_interrupted" {
		t.Fatalf("客户端取消应映射 stream_interrupted: %+v", spec)
	}
	if spec := specOf(context.DeadlineExceeded); spec.Code != "upstream_timeout" {
		t.Fatalf("超时应映射 upstream_timeout: %+v", spec)
	}
	if spec := specOf(errors.New("something odd")); spec.Code != "upstream_error" || spec.Status != 502 {
		t.Fatalf("未知错误应回落 502/upstream_error: %+v", spec)
	}
}

// 契约：公开体只有四键，且绝不包含上游原文。
func TestPublicBodyHasOnlyFourKeys(t *testing.T) {
	err := &upstream.Error{
		Status:  403,
		Code:    "QUOTA_EXCEEDED",
		Message: "账号 u_test_987654321 余额不足，请联系 admin@internal.example.com",
		Kind:    upstream.KindQuota,
	}
	body := publicError(specOf(err), "messages")
	raw, _ := json.Marshal(body)
	var parsed map[string]map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		t.Fatalf("公开体不是对象: %s", raw)
	}
	if len(parsed) != 1 {
		t.Fatalf("顶层只允许 error，得到 %v", parsed)
	}
	e := parsed["error"]
	if len(e) != 4 {
		t.Fatalf("error 内只允许四键，得到 %v", e)
	}
	for _, k := range []string{"message", "type", "param", "code"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("缺少键 %s: %v", k, e)
		}
	}
	s := string(raw)
	for _, leaked := range []string{"u_test_987654321", "internal.example.com", "admin@"} {
		if strings.Contains(s, leaked) {
			t.Fatalf("公开体泄露了上游原文 %q: %s", leaked, s)
		}
	}
}

// 契约：param 只接受已知顶层字段，嵌套路径与配置键一律 null。
func TestParamWhitelist(t *testing.T) {
	body := publicError(specs["invalid_request"], "messages[0].content")
	e := body["error"].(map[string]any)
	if e["param"] != nil {
		t.Fatalf("嵌套路径必须置 null，得到 %v", e["param"])
	}
	body = publicError(specs["invalid_request"], "messages")
	if e := body["error"].(map[string]any); e["param"] != "messages" {
		t.Fatalf("已知顶层参数应保留，得到 %v", e["param"])
	}
}

func TestRetryAfterOnlyWhenRetryable(t *testing.T) {
	cases := []struct {
		err  *upstream.Error
		want bool
	}{
		{&upstream.Error{Status: 429, Code: "rate_limit_exceeded"}, true},
		{&upstream.Error{Status: 429, Code: "insufficient_quota"}, false},
		{&upstream.Error{Status: 400}, false},
	}
	for _, c := range cases {
		if got := specOf(c.err).Retryable; got != c.want {
			t.Errorf("%+v Retryable = %v，期望 %v", c.err, got, c.want)
		}
	}
}

func TestStreamErrorFrameIsValidSSE(t *testing.T) {
	frame := string(sseErrorFrame(specs["content_filter"]))
	if !strings.HasPrefix(frame, "data: ") || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("SSE 错误帧格式错误: %q", frame)
	}
	var v struct {
		Error map[string]any `json:"error"`
	}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data: "))
	if json.Unmarshal([]byte(payload), &v) != nil || len(v.Error) != 4 {
		t.Fatalf("SSE 错误帧内容不合契约: %s", payload)
	}
}

func TestStreamErrorOfDetectsErrorFrame(t *testing.T) {
	// 上游 200 但首帧就是错误对象
	ue := streamErrorOf(`data: {"error":{"message":"Input data may contain inappropriate content.","code":"DataInspectionFailed","type":"invalid_request_error"}}`)
	if ue == nil {
		t.Fatal("应识别出 error 帧")
	}
	if spec := specOf(ue); spec.Code != "content_filter" {
		t.Fatalf("内容过滤应被识别，得到 %+v", spec)
	}
	// 正常增量帧不应误判
	if streamErrorOf(`data: {"choices":[{"delta":{"content":"hi"}}]}`) != nil {
		t.Fatal("正常帧被误判为错误")
	}
	if streamErrorOf(`data: [DONE]`) != nil {
		t.Fatal("[DONE] 被误判为错误")
	}
	if streamErrorOf(`: keepalive`) != nil {
		t.Fatal("注释帧被误判为错误")
	}
}

func TestWriteSpecStatusKeepsSpec(t *testing.T) {
	// 405 场景：状态改写但公开 type/code 仍标准化
	rec := httptest.NewRecorder()
	writeSpecStatus(rec, specs["invalid_request"], http.StatusMethodNotAllowed, "")
	if rec.Code != 405 {
		t.Fatalf("status = %d，期望 405", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("公开 code 应保持 invalid_request: %s", rec.Body.String())
	}
}

// Meoo 把"模型不可用"包在 invalid_request_error 里：具体码应优先，得到 404。
func TestModelNotSupportedMapsToNotFound(t *testing.T) {
	err := &upstream.Error{
		Status:  400,
		Code:    "MODEL_NOT_SUPPORTED",
		Message: "当前仅支持配置模型: qwen3.8-flash、…",
	}
	spec := specOf(err)
	if spec.Code != "not_found" || spec.Status != 404 {
		t.Fatalf("MODEL_NOT_SUPPORTED 应映射 404/not_found，得到 %+v", spec)
	}
	if strings.Contains(spec.Message, "qwen3.8-flash") {
		t.Fatalf("公开文案不得包含上游模型清单: %q", spec.Message)
	}
}
