package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

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

// TestEnrichFillsFieldsWithoutExpandingSet 守住本次改动的核心不变量：
// 站点目录只用于补字段，站点独有的模型绝不进入 /v1/models。
func TestEnrichFillsFieldsWithoutExpandingSet(t *testing.T) {
	mux := http.NewServeMux()
	// 兼容网关：只有两个纯字符串 ID
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":["qwen3.8-flash","glm-5.2"],"defaultModel":"qwen3.8-flash"}`))
	})
	// 站点目录：两个重叠 + 一个只在站点存在的（kimi）
	mux.HandleFunc("/api/v1/agent/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer meoo_project_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"models":[
			{"key":"qwen3.8-flash","value":"qwen3.8-flash","label":"Qwen3.8-Flash","provider":"bailian","context_window":200000,"max_token":64000,"multimodal":true,"description":"千问最新平价模型"},
			{"key":"glm-5.2","value":"glm-5.2","label":"GLM-5.2","provider":"bailian","context_window":128000,"max_token":32000},
			{"key":"moonshot/kimi-k3","value":"moonshot/kimi-k3","label":"Kimi K3","context_window":200000,"max_token":64000}
		]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	st.Mu.Lock()
	st.Data.Keys = []*model.Key{{
		ID: "k1", Key: "meoo_project_test", Enabled: true, Health: model.Health{State: "ok"},
	}}
	st.Mu.Unlock()

	up := upstream.New(srv.URL)
	s := New(st, &pool.Picker{St: st, Up: up}, up)
	s.WebBase = srv.URL
	s.Refresh(context.Background())

	ms, _, ferr := s.List()
	if ferr != "" {
		t.Fatalf("目录出错: %s", ferr)
	}
	// 默认配置自带 qwen 别名，断言只针对上游模型本身。
	byID := map[string]Model{}
	for _, m := range ms {
		if m.OwnedBy == "meoo-alias" {
			continue
		}
		byID[m.ID] = m
	}
	if len(byID) != 2 {
		t.Fatalf("只能有兼容网关的 2 个模型，得到 %d: %+v", len(byID), ms)
	}
	if _, ok := byID["moonshot/kimi-k3"]; ok {
		t.Fatal("站点独有模型不得进入 /v1/models —— 它调不通，列出去只会误导客户端")
	}
	q := byID["qwen3.8-flash"]
	if q.ContextWindow != 200000 || q.MaxTokens != 64000 || !q.Multimodal ||
		q.Label != "Qwen3.8-Flash" || q.Provider != "bailian" || q.Description == "" {
		t.Fatalf("元数据未补齐: %+v", q)
	}
	if g := byID["glm-5.2"]; g.ContextWindow != 128000 || g.MaxTokens != 32000 {
		t.Fatalf("非法条目元数据错误: %+v", g)
	}

	stat := s.Status()
	if stat["meta_ok"] != 2 {
		t.Fatalf("meta_ok 应为 2，得到 %v", stat["meta_ok"])
	}
	only, _ := stat["web_only"].([]string)
	if len(only) != 1 || only[0] != "moonshot/kimi-k3" {
		t.Fatalf("web_only 应只含 kimi-k3，得到 %v", stat["web_only"])
	}
}

// TestEnrichFailureKeepsCatalog 上游目录不可用时，主目录与已补字段都不能被破坏。
func TestEnrichFailureKeepsCatalog(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":["qwen3.8-flash"]}`))
	})
	mux.HandleFunc("/api/v1/agent/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _, _ := store.Load(t.TempDir())
	defer st.Close()
	st.Mu.Lock()
	st.Data.Keys = []*model.Key{{ID: "k1", Key: "meoo_project_test", Enabled: true, Health: model.Health{State: "ok"}}}
	st.Mu.Unlock()

	up := upstream.New(srv.URL)
	s := New(st, &pool.Picker{St: st, Up: up}, up)
	s.WebBase = srv.URL
	s.Refresh(context.Background())

	ms, _, _ := s.List()
	upstreamCount := 0
	for _, m := range ms {
		if m.OwnedBy != "meoo-alias" {
			upstreamCount++
		}
	}
	if upstreamCount != 1 {
		t.Fatalf("元数据失败不应影响可调用集合，得到 %d: %+v", upstreamCount, ms)
	}
	if errMsg, _ := s.Status()["meta_error"].(string); errMsg == "" {
		t.Fatal("元数据失败必须记录 meta_error 供面板排查")
	}
}
