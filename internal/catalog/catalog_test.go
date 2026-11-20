package catalog

import "testing"

func TestParseOpenAIAndNativeShapes(t *testing.T) {
	openai := []byte(`{"object":"list","data":[{"id":"qwen3.8-flash","object":"model","owned_by":"bailian"}]}`)
	ms, def, err := parse(openai)
	if err != nil {
		t.Fatalf("OpenAI 形态解析失败: %v", err)
	}
	if len(ms) != 1 || ms[0].ID != "qwen3.8-flash" || ms[0].OwnedBy != "bailian" {
		t.Fatalf("OpenAI 形态结果错误: %+v", ms)
	}
	if def != "" {
		t.Fatalf("OpenAI 形态没有 defaultModel，得到 %q", def)
	}

	native := []byte(`{"data":{"models":[
		{"key":"qwen3.8-flash","value":"qwen3.8-flash","context_window":200000,"max_token":64000,"multimodal":true},
		{"key":"deepseekV4","value":"deepseek-v4-flash-0731","max_token":64000}
	],"defaultModel":"qwen3.8-flash"}}`)
	ms, def, err = parse(native)
	if err != nil {
		t.Fatalf("原生形态解析失败: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("原生形态应解析 2 个模型，得到 %d", len(ms))
	}
	if def != "qwen3.8-flash" {
		t.Fatalf("defaultModel 应被保留，得到 %q", def)
	}
	// 按 id 排序
	if ms[0].ID != "deepseek-v4-flash-0731" || ms[1].ID != "qwen3.8-flash" {
		t.Fatalf("结果未排序: %+v", ms)
	}
	if ms[1].ContextWindow != 200000 || !ms[1].Multimodal {
		t.Fatalf("上下文/多模态字段丢失: %+v", ms[1])
	}
}

func TestParseEmptyIsError(t *testing.T) {
	if _, _, err := parse([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("空目录必须报错，避免用空结果覆盖旧缓存")
	}
}

func TestResolvePrefersAlias(t *testing.T) {
	s := &Service{}
	// 没有 store 时也要能解析别名（面板未初始化场景不应 panic）
	if got := s.Resolve("qwen"); got != "qwen" {
		t.Fatalf("无别名表时应原样返回，得到 %q", got)
	}
	if got := s.Resolve(""); got == "" {
		t.Fatal("空模型名必须回落到默认模型")
	}
}

func TestParseStringArrayShape(t *testing.T) {
	// 官方网关真实响应：models 是纯字符串数组
	raw := []byte(`{"models":["qwen3.8-flash","qwen4-max","glm-5.2"],"defaultModel":"qwen3.8-flash"}`)
	ms, def, err := parse(raw)
	if err != nil {
		t.Fatalf("字符串数组形态解析失败: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("应有 3 个模型，得到 %d: %+v", len(ms), ms)
	}
	if def != "qwen3.8-flash" {
		t.Fatalf("defaultModel 丢失: %q", def)
	}
	if ms[0].ID != "glm-5.2" || ms[0].OwnedBy != "meoo" {
		t.Fatalf("排序或默认 owned_by 错误: %+v", ms[0])
	}
}

func TestParseMixedArrayKeepsObjects(t *testing.T) {
	raw := []byte(`{"data":[{"id":"a","context_window":200000},"b"]}`)
	ms, _, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("混合数组应解析 2 项，得到 %d", len(ms))
	}
	if ms[0].ID != "a" || ms[0].ContextWindow != 200000 {
		t.Fatalf("对象项字段丢失: %+v", ms[0])
	}
}
