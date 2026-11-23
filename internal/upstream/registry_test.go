package upstream

import "testing"

func TestRegistryBasics(t *testing.T) {
	r := NewRegistry("meoo", "https://meoo.example/v1")
	if got := r.Get("meoo").Base; got != "https://meoo.example/v1" {
		t.Fatalf("默认供应商 base 错误: %s", got)
	}
	if got := r.Get("unknown").Base; got != "https://meoo.example/v1" {
		t.Fatalf("未知名应回退默认: %s", got)
	}
	r.Add("DeepSeek", "https://ds.example/v1")
	if !r.Has("deepseek") || !r.Has("DEEPSEEK") {
		t.Fatal("名字应归一化大小写")
	}
	if got := r.Get("DeepSeek").Base; got != "https://ds.example/v1" {
		t.Fatalf("归一化后应能取到: %s", got)
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "deepseek" || names[1] != "meoo" {
		t.Fatalf("Names 应排序返回: %v", names)
	}
	if r.Default() != "meoo" {
		t.Fatalf("默认供应商应为 meoo: %s", r.Default())
	}
}

func TestRegistryAddValidation(t *testing.T) {
	r := NewRegistry("meoo", "https://meoo.example/v1")
	if r.Add("", "https://x.example") != nil {
		t.Fatal("空名字应被拒绝")
	}
	if r.Add("Bad Name!", "https://x.example") != nil {
		t.Fatal("非法名字应被拒绝")
	}
	if r.Add("ok-name_2", "") != nil {
		t.Fatal("空 base 应被拒绝")
	}
	if r.Add("ok-name_2", "https://x.example/v1") == nil {
		t.Fatal("合法名字应注册成功")
	}
}
