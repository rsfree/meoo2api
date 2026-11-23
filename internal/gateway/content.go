package gateway

import (
	"strings"
)

// 本文件做「出站前的内容体检」。
//
// 为什么必须在本地做，而不是把上游的 400 原样透出去：
//
//	实测（2026-11-23）qwen3.7-max / glm-5.2 这类纯文本模型收到 image_url 时，
//	上游只回一句 "The provided messages input is invalid"，公开文案按契约又必须
//	归一成固定句子 —— 客户端最后只能看到「请求无效」，完全不知道是自己发了图片。
//	24 小时运行日志里 32/536 次 400 全是这一类，全部无法归因。
//
// 所以这里做三件事：
//  1. 把 Responses API 风格的 input_text 归一成 chat 风格的 text（纯兼容，无副作用）
//  2. 目录明确标注为纯文本的模型收到图片时，本地直接拒绝，文案点明原因与替代做法
//  3. 无法转发的 content 类型本地拒绝，不再浪费一次上游调用与十几秒等待
//
// 未在目录里或元数据未知的模型一律放行 —— 宁可让上游回答，也不误拒。

// contentError 携带公开错误规格，供 chat() 直接写响应。
type contentError struct {
	spec  string
	param string
}

func (e *contentError) Error() string { return e.spec }

// specInputText / specVision / specContent 对应 errmap.go 里的固定文案表。
const (
	specInputText = "invalid_request"
	specVision    = "vision_unsupported"
	specContent   = "content_unsupported"
)

// visionCapability 报告模型是否明确支持图片输入。
// supported 为真表示目录已确认多模态；known 为假表示元数据缺失，调用方应放行。
func (h *Handler) visionCapability(modelID string) (supported, known bool) {
	if h.Catalog == nil || modelID == "" {
		return false, false
	}
	ms, _, _ := h.Catalog.List()
	for _, m := range ms {
		if m.ID != modelID {
			continue
		}
		// Provider 非空说明这条目被站点目录补过元数据，其中 multimodal 字段可信；
		// 只有上游 /models 报出来的裸条目（MiniMax-M2.5 等）没有能力信息，不能据此拒绝。
		if m.Provider == "" {
			return false, false
		}
		return m.Multimodal, true
	}
	return false, false
}

// normalizeMessages 就地整理 messages：归一 input_text、校验 content 类型。
// 返回 *contentError 时由调用方按 spec 写响应；其余错误视为请求不合法。
func normalizeMessages(msgs []any, visionOK, visionKnown bool) error {
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			return &contentError{spec: specInputText, param: "messages"}
		}
		content, exists := m["content"]
		if !exists || content == nil {
			// tool 消息可以没有 content（只有 tool_calls / 工具结果在别处）。
			continue
		}
		// 字符串形态是标准写法，直接放行。
		if _, ok := content.(string); ok {
			continue
		}
		items, ok := content.([]any)
		if !ok {
			// 既不是字符串也不是数组：上游一定拒，本地先拒。
			return &contentError{spec: specInputText, param: "messages"}
		}
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				return &contentError{spec: specInputText, param: "messages"}
			}
			typ, _ := item["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "text":
				// 原样保留
			case "input_text":
				// Responses API 风格 → chat 风格。字段名一致，只改 type。
				item["type"] = "text"
			case "image_url", "input_image":
				if visionKnown && !visionOK {
					return &contentError{spec: specVision, param: "messages"}
				}
			case "":
				// 没写 type 但有 text 字段：按 text 处理（部分老客户端如此）。
				if _, has := item["text"]; has {
					item["type"] = "text"
					continue
				}
				return &contentError{spec: specContent, param: "messages"}
			default:
				// tool_result / thinking / audio / file 等：兼容网关不支持，本地拒。
				return &contentError{spec: specContent, param: "messages"}
			}
		}
	}
	return nil
}
