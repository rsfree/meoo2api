package gateway

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("测试数据非法: %v", err)
	}
	return m
}

func TestShapeRejectsN(t *testing.T) {
	_, err := shapeRequest(decode(t, `{"model":"x","n":2,"messages":[{"role":"user","content":"a"}]}`), "real-model", 0, false)
	if err == nil {
		t.Fatal("n=2 必须报错，不能静默降级")
	}
	out, err := shapeRequest(decode(t, `{"model":"x","n":1}`), "real-model", 0, false)
	if err != nil {
		t.Fatalf("n=1 应通过: %v", err)
	}
	if out["model"] != "real-model" {
		t.Fatalf("别名未落地: %v", out["model"])
	}
}

// 0 的语义是「自动」：注入 4096 而不是跟随上游。
// 24 小时日志里 18 次 HTTP 200 但正文为空，全是推理模型思考把预算烧光；
// 这些请求大多没传 max_tokens，上游默认值太小。要回到旧行为把配置设成 -1。
func TestShapeAutoBudgetDefault(t *testing.T) {
	out, err := shapeRequest(decode(t, `{"model":"q","messages":[{"role":"user","content":"a"}]}`), "qwen3.8-flash", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if out["max_tokens"] != 4096 {
		t.Fatalf("0 应注入 4096，得到 %v", out["max_tokens"])
	}
	out2, _ := shapeRequest(decode(t, `{"model":"q"}`), "qwen3.8-flash", 4096, false)
	if out2["max_tokens"] != 4096 {
		t.Fatalf("配置了默认值应注入，得到 %v", out2["max_tokens"])
	}
	out3, _ := shapeRequest(decode(t, `{"model":"q"}`), "qwen3.8-flash", -1, false)
	if _, has := out3["max_tokens"]; has {
		t.Fatalf("-1 表示跟随上游，不应注入，得到 %v", out3["max_tokens"])
	}
}

func TestShapeClientMaxWins(t *testing.T) {
	out, _ := shapeRequest(decode(t, `{"model":"q","max_completion_tokens":300}`), "real", 4096, false)
	if out["max_tokens"] != float64(300) {
		t.Fatalf("max_completion_tokens 应归一为 max_tokens 且优先于默认值，得到 %v", out["max_tokens"])
	}
	if _, has := out["max_completion_tokens"]; has {
		t.Fatal("max_completion_tokens 应被删除")
	}
	// 显式 max_tokens 不被覆盖
	out2, _ := shapeRequest(decode(t, `{"model":"q","max_tokens":128}`), "real", 4096, false)
	if out2["max_tokens"] != float64(128) {
		t.Fatalf("客户端显式 max_tokens 必须优先，得到 %v", out2["max_tokens"])
	}
}

func TestShapeDropsStreamOptionsWhenNotStreaming(t *testing.T) {
	out, _ := shapeRequest(decode(t, `{"model":"q","stream":false,"stream_options":{"include_usage":true}}`), "real", 0, false)
	if _, has := out["stream_options"]; has {
		t.Fatal("非流式请求必须丢掉 stream_options")
	}
	out2, _ := shapeRequest(decode(t, `{"model":"q","stream":true,"stream_options":{"include_usage":true}}`), "real", 0, true)
	if _, has := out2["stream_options"]; !has {
		t.Fatal("流式请求应原样保留 stream_options")
	}
}

func TestScanChunk(t *testing.T) {
	u, fin, cl, _ := scanChunk(`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`)
	if fin != "" || u.total != 0 {
		t.Fatalf("中间帧不应有 finish_reason: %+v %q", u, fin)
	}
	if cl != 1 {
		t.Fatalf("正文增量长度应为 1，得到 %d", cl)
	}
	u, fin, _, _ = scanChunk(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	if fin != "stop" {
		t.Fatalf("finish_reason 丢失: %q", fin)
	}
	if u.prompt != 10 || u.completion != 5 || u.total != 15 {
		t.Fatalf("usage 解析错误: %+v", u)
	}
	// 只有 prompt/completion 没有 total 时要自己加
	u, _, _, _ = scanChunk(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	if u.total != 10 {
		t.Fatalf("total 应回落为 prompt+completion，得到 %d", u.total)
	}
}

func TestParseUsageNonStream(t *testing.T) {
	u := parseUsage([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if u.total != 7 {
		t.Fatalf("非流式 usage 合计错误: %+v", u)
	}
	if u := parseUsage([]byte(`not json`)); u.total != 0 {
		t.Fatal("非法响应不应 panic，也不应产生用量")
	}
}

func TestCompletionNoteFlagsReasoningOnly(t *testing.T) {
	// 推理模型把预算全烧在思考上：200 + finish=length + 正文空
	raw := []byte(`{"choices":[{"finish_reason":"length","message":{"content":"","reasoning_content":"想了想"}}]}`)
	if note := completionNote(raw); note == "" {
		t.Fatal("应识别出思考耗尽预算")
	}
	// 正常结束不该误报
	ok := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"好的"}}]}`)
	if note := completionNote(ok); note != "" {
		t.Fatalf("正常响应不应提示: %q", note)
	}
	// 正文和思考都空（例如内容过滤）也不该套用这条提示
	empty := []byte(`{"choices":[{"finish_reason":"length","message":{"content":""}}]}`)
	if note := completionNote(empty); note != "" {
		t.Fatalf("无思考内容时不应提示: %q", note)
	}
}
