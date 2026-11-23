package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meoo2api/internal/model"
	"meoo2api/internal/upstream"
)

// msg 构造一条 user 消息，content 原样放入。
func msg(content any) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

func TestNormalizeInputText(t *testing.T) {
	// Responses API 风格（input_text）在 chat 兼容层必须归一成 text，
	// 否则上游直接 400「Unexpected item type in content」。
	body := []any{msg([]any{map[string]any{"type": "input_text", "text": "hi"}})}
	if err := normalizeMessages(body, false, true); err != nil {
		t.Fatalf("input_text 应被兼容，得到 %v", err)
	}
	item := body[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if item["type"] != "text" {
		t.Fatalf("input_text 未归一，得到 %v", item["type"])
	}
	if item["text"] != "hi" {
		t.Fatalf("归一不得丢字段，得到 %v", item["text"])
	}
}

func TestNormalizeRejectsImageForTextModel(t *testing.T) {
	// 目录确认纯文本的模型收到图片：本地就要拒，且必须给专属错误码，
	// 让客户端能换模型，而不是死在一句「请求无效」上。
	body := []any{msg([]any{
		map[string]any{"type": "text", "text": "看图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
	})}
	err := normalizeMessages(body, false, true)
	var ce *contentError
	if !errors.As(err, &ce) || ce.spec != specVision {
		t.Fatalf("应返回 vision_unsupported，得到 %v", err)
	}
	if ce.param != "messages" {
		t.Fatalf("param 应为 messages，得到 %q", ce.param)
	}
}

func TestNormalizeKeepsImageForVisionModel(t *testing.T) {
	body := []any{msg([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}}})}
	if err := normalizeMessages(body, true, true); err != nil {
		t.Fatalf("多模态模型应放行图片: %v", err)
	}
}

func TestNormalizeUnknownCapabilityPasses(t *testing.T) {
	// 元数据未知（不在目录 / 裸条目）：宁放行不误拒。
	body := []any{msg([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}}})}
	if err := normalizeMessages(body, false, false); err != nil {
		t.Fatalf("能力未知时不得拒绝: %v", err)
	}
}

func TestNormalizeRejectsUnforwardableTypes(t *testing.T) {
	for _, typ := range []string{"tool_result", "thinking", "audio", "input_audio", "file"} {
		body := []any{msg([]any{map[string]any{"type": typ}})}
		err := normalizeMessages(body, true, true)
		var ce *contentError
		if !errors.As(err, &ce) || ce.spec != specContent {
			t.Fatalf("%s 应被拒为 content_unsupported，得到 %v", typ, err)
		}
	}
}

func TestNormalizeTextItemWithoutType(t *testing.T) {
	// 少数老客户端只给 {"text": "..."}，补上 type=text 即可兼容。
	body := []any{msg([]any{map[string]any{"text": "hi"}})}
	if err := normalizeMessages(body, false, true); err != nil {
		t.Fatalf("无 type 有 text 应兼容: %v", err)
	}
	item := body[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if item["type"] != "text" {
		t.Fatalf("未补 type，得到 %v", item["type"])
	}
}

func TestNormalizeLeavesStringAndNullContent(t *testing.T) {
	body := []any{msg("纯字符串"), map[string]any{"role": "assistant", "content": nil}}
	if err := normalizeMessages(body, false, true); err != nil {
		t.Fatalf("字符串与 null content 应放行: %v", err)
	}
}

func TestNormalizeRejectsNonArrayContent(t *testing.T) {
	body := []any{msg(map[string]any{"x": 1})}
	var ce *contentError
	if err := normalizeMessages(body, true, true); !errors.As(err, &ce) || ce.spec != specInputText {
		t.Fatalf("非法 content 形态应被拒，得到 %v", err)
	}
}

func TestAutoBudget(t *testing.T) {
	cases := map[int]int{-1: 0, 0: autoBudgetDefault, 512: 512, 8192: 8192}
	for in, want := range cases {
		if got := autoBudget(in); got != want {
			t.Fatalf("autoBudget(%d)=%d，期望 %d", in, got, want)
		}
	}
}

func TestShapeInjectsAutoBudget(t *testing.T) {
	out, err := shapeRequest(decode(t, `{"model":"q","messages":[{"role":"user","content":"a"}]}`), "qwen3.8-flash", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if out["max_tokens"] != autoBudgetDefault {
		t.Fatalf("0 应注入自动预算 %d，得到 %v", autoBudgetDefault, out["max_tokens"])
	}
	// -1 是逃生阀：完全不注入
	raw, _ := shapeRequest(decode(t, `{"model":"q"}`), "qwen3.8-flash", -1, true)
	if _, has := raw["max_tokens"]; has {
		t.Fatalf("-1 表示跟随上游，不得注入，得到 %v", raw["max_tokens"])
	}
}

func TestStatusOfErrRecordsRealCode(t *testing.T) {
	// 日志必须记客户端实际收到的状态码，0 会被面板当成成功。
	cases := []struct {
		err  error
		want int
	}{
		{nil, 200},
		{&upstream.Error{Status: 400, Kind: upstream.KindBadRequest}, 400},
		{&upstream.Error{Status: 413, Kind: upstream.KindBadRequest}, 413},
		{&upstream.Error{Status: 429, Kind: upstream.KindRate}, 429},
		{&upstream.Error{Status: 500, Kind: upstream.KindServer}, 500},
		{context.DeadlineExceeded, 504},
	}
	for _, c := range cases {
		if got := statusOfErr(c.err); got != c.want {
			t.Fatalf("statusOfErr(%v)=%d，期望 %d", c.err, got, c.want)
		}
	}
	if got := statusOfErr(nil); got == 0 {
		t.Fatal("成功请求不得记 0")
	}
}

func TestSpecsHaveDedicatedContentCodes(t *testing.T) {
	for _, k := range []string{specVision, specContent} {
		sp, ok := specs[k]
		if !ok {
			t.Fatalf("缺少错误规格 %s", k)
		}
		if sp.Status != 400 || sp.Code == "" || sp.Message == "" {
			t.Fatalf("%s 规格不完整: %+v", k, sp)
		}
	}
	// 公开体必须四键齐全且能正常序列化
	buf, err := json.Marshal(publicError(specs[specVision], "messages"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatal(err)
	}
	if v["error"]["code"] != "model_does_not_support_vision" {
		t.Fatalf("公开码不符: %v", v)
	}
	if v["error"]["param"] != "messages" {
		t.Fatalf("param 应保留 messages: %v", v)
	}
}

// 超过上游 bodyParser 上限的请求必须在入口就拒，且一个字节都不要打给上游。
func TestChatRejectsOversizedBody(t *testing.T) {
	key := "meoo_project_k5555555555"
	gw, f, _ := harness(t, map[string]model.Health{key: {State: "ok"}})
	big := `{"model":"test-model","messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxBody) + `"}]}`
	rec := chatReq(gw, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应回 413，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Meoo-Max-Body") == "" {
		t.Fatal("应回 X-Meoo-Max-Body 让客户端知道上限")
	}
	if n := f.count(key); n != 0 {
		t.Fatalf("超限请求不得转发到上游，实际打了 %d 次", n)
	}
}

// 没有 Content-Length（chunked）时只能读完再判，同样要 413 而不是 400。
func TestChatRejectsOversizedChunkedBody(t *testing.T) {
	key := "meoo_project_k6666666666"
	gw, f, _ := harness(t, map[string]model.Health{key: {State: "ok"}})
	big := `{"model":"test-model","messages":[{"role":"user","content":"` +
		strings.Repeat("y", maxBody) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	req.ContentLength = -1 // 让服务端拿不到长度，走 LimitReader 分支
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 超限应回 413，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := f.count(key); n != 0 {
		t.Fatalf("超限请求不得转发到上游，实际打了 %d 次", n)
	}
}
