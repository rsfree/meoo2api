package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"meoo2api/internal/upstream"
)

// 本文件实现 chat-adapter 规范的公开错误契约：
//
//   - 公开体只允许 {"error":{"message","type","param","code"}} 四键，顶层只允许 error
//   - HTTP 402 无条件判额度不足（上游常夹带误导性的内层 type/code）
//   - 结构化 type/code 先于 message 文本；文本只用于分类，绝不复制到输出
//   - 未命中再按 HTTP 状态兜底，最后回落 502 api_error/upstream_error
//   - message 一律取固定文案表，上游原文只进内部日志

// Spec 是一条公开错误规格。
type Spec struct {
	Status    int
	Type      string
	Code      string
	Message   string
	Retryable bool
}

var specs = map[string]Spec{
	"invalid_request":       {400, "invalid_request_error", "invalid_request", "The request is invalid. Check the request parameters.", false},
	"context_length":        {400, "invalid_request_error", "context_length_exceeded", "The request exceeds the model context length. Reduce the input or output length.", false},
	"unsupported_parameter": {400, "invalid_request_error", "unsupported_parameter", "A request parameter is not supported. Remove or replace it.", false},
	"content_filter":        {400, "invalid_request_error", "content_filter", "The request was rejected by content safety checks.", false},
	"request_too_large":     {413, "invalid_request_error", "request_too_large", "The request is too large. Reduce its size.", false},
	"authentication":        {401, "authentication_error", "invalid_api_key", "Authentication failed.", false},
	"permission":            {403, "permission_error", "permission_denied", "Permission denied.", false},
	"not_found":             {404, "not_found_error", "not_found", "The requested resource or model was not found.", false},
	"conflict":              {409, "conflict_error", "conflict", "The request conflicts with the current resource state.", false},
	"request_timeout":       {408, "timeout_error", "request_timeout", "The request timed out. Retry later.", true},
	"quota":                 {429, "rate_limit_error", "insufficient_quota", "Insufficient quota or account balance.", false},
	"rate_limit":            {429, "rate_limit_error", "rate_limit_exceeded", "Rate limit reached. Retry later.", true},
	"overloaded":            {503, "server_error", "server_overloaded", "The service is overloaded. Retry later.", true},
	"server":                {500, "server_error", "internal_error", "The service encountered an error. Retry later.", true},
	"unavailable":           {503, "server_error", "service_unavailable", "The service is temporarily unavailable. Retry later.", true},
	"timeout":               {504, "timeout_error", "upstream_timeout", "The service timed out. Retry later.", true},
	"connection":            {502, "api_error", "connection_error", "The service could not be reached. Retry later.", true},
	"stream_interrupted":    {502, "api_error", "stream_interrupted", "The response stream ended before a completion terminal state.", false},
	"aborted":               {502, "api_error", "generation_aborted", "The generation was interrupted before completion.", false},
	"bad_response":          {502, "api_error", "invalid_upstream_response", "The service returned an invalid response.", false},
	"unknown":               {502, "api_error", "upstream_error", "The request could not be completed.", false},
}

// tokenRules 按 OpenAI → DeepSeek → Ark → Kimi 顺序匹配结构化错误值。
// 顺序是契约的一部分：一个错误体可能同时带多个语义。
var tokenRules = []struct {
	spec   string
	tokens []string
}{
	// 具体码优先于笼统 type：上游常把"模型不可用"包在 invalid_request_error 里，
	// 若先匹配 type 会丢掉 not_found 语义（Meoo 的 MODEL_NOT_SUPPORTED 就是这种）。
	{"not_found", []string{"model_not_supported", "modelnotsupported", "model_not_found"}},
	// OpenAI 段
	{"invalid_request", []string{"invalid_request_error", "unprocessable_entity_error"}},
	{"context_length", []string{"context_length_exceeded"}},
	{"content_filter", []string{"content_filter", "content_policy_violation"}},
	{"unsupported_parameter", []string{"unsupported_parameter", "unsupported_value"}},
	{"authentication", []string{"authentication_error", "invalid_api_key", "incorrect_api_key", "invalid_authentication_error"}},
	{"permission", []string{"permission_error", "permission_denied", "permission_denied_error"}},
	{"not_found", []string{"not_found_error", "resource_not_found_error", "model_not_found"}},
	{"conflict", []string{"conflict_error"}},
	{"quota", []string{"insufficient_quota", "credit_balance_exhausted", "exceeded_current_quota_error"}},
	{"rate_limit", []string{"rate_limit_error", "rate_limit_exceeded", "rate_limit_reached_error", "slow_down"}},
	{"timeout", []string{"request_timeout_error", "api_timeout_error"}},
	{"unavailable", []string{"service_unavailable", "server_unavailable", "service_unavailable_error"}},
	{"stream_interrupted", []string{"stream_interrupted", "client_closed_request", "request_canceled"}},
	{"aborted", []string{"aborted", "generation_aborted"}},
	{"bad_response", []string{"invalid_upstream_response"}},
	{"server", []string{"server_error", "internal_error", "unexpected_output"}},
	// DeepSeek 段
	{"quota", []string{"insufficient_balance"}},
	{"invalid_request", []string{"invalid_format", "invalid_parameters"}},
	// Ark 段（精确码）
	{"context_length", []string{"contextlengthexceeded", "outofcontexterror"}},
	{"content_filter", []string{"sensitivecontentdetected", "contentfilter", "inputdatasensitive", "outputdatasensitive"}},
	{"invalid_request", []string{"invalidparameter", "missingparameter", "invalidrequestformat", "invalidargumenterror"}},
	{"authentication", []string{"authenticationfailure", "authenticationerror", "invalidauthentication", "apikeynotvalid", "signaturenotmatch"}},
	{"permission", []string{"accessdenied", "permissiondenied", "invalidsubscription", "forbidden"}},
	{"not_found", []string{"modelnotfound", "modelnotopen", "invalidendpointnotfound", "pathnotfound", "notfound", "unsupportedmodel"}},
	{"quota", []string{"accountarrearage", "accountoverdue", "accountoverdueerror", "quotaexceeded", "accountquotaexceeded", "setlimitexceeded", "modelaccounttpdratelimitexceeded"}},
	{"overloaded", []string{"serveroverloaded", "requestbursttoofast", "modelloading"}},
	{"rate_limit", []string{"modelaccountrpmratelimitexceeded", "modelaccounttpmratelimitexceeded", "modelaccountipmratelimitexceeded", "modelaccountflextpmratelimitexceeded", "modelaccountconcurrentrequestslimitexceeded", "apiratelimit", "accountratelimitexceeded", "inflightbatchsizeexceeded", "ratelimited"}},
	{"stream_interrupted", []string{"requestcanceled"}},
	{"timeout", []string{"modeltimeout", "requesttimeout"}},
	{"unavailable", []string{"serviceunavailable", "serviceconnectionrefused", "noavailableservice"}},
	{"server", []string{"internalserviceerror", "internalservererror"}},
	// Kimi 段
	{"content_filter", []string{"content_filter"}},
	{"authentication", []string{"invalid_authentication_error", "incorrect_api_key_error"}},
	{"permission", []string{"permission_denied_error"}},
	{"not_found", []string{"resource_not_found_error"}},
	{"quota", []string{"exceeded_current_quota_error"}},
	{"overloaded", []string{"engine_overloaded_error"}},
	{"rate_limit", []string{"rate_limit_reached_error"}},
	{"timeout", []string{"request_timeout_error"}},
	{"unavailable", []string{"server_unavailable"}},
	{"server", []string{"unexpected_output", "api_error"}},
}

// arkPrefixRules 只用于 Ark 的点号/风险识别族变体。
// 契约规定前缀匹配不得跨厂商生效，否则 api_error 之类会误伤。
var arkPrefixRules = []struct {
	spec    string
	phrases []string
}{
	{"content_filter", []string{"sensitivecontentdetected", "inputtextsensitivecontentdetected", "inputimagesensitivecontentdetected", "inputvideosensitivecontentdetected", "inputaudiosensitivecontentdetected", "outputtextsensitivecontentdetected", "outputimagesensitivecontentdetected", "outputvideosensitivecontentdetected", "outputaudiosensitivecontentdetected", "inputtextriskdetection", "inputimageriskdetection", "outputtextriskdetection", "outputimageriskdetection", "datainspectionfailed"}},
	{"quota", []string{"quotaexceeded", "accountquotaexceeded", "accountoverdue", "setlimitexceeded", "modelaccounttpdratelimitexceeded"}},
	{"rate_limit", []string{"ratelimitexceeded", "modelaccountrpmratelimitexceeded", "modelaccounttpmratelimitexceeded", "modelaccountipmratelimitexceeded", "modelaccountflextpmratelimitexceeded", "modelaccountconcurrentrequestslimitexceeded", "apiratelimit", "accountratelimitexceeded", "inflightbatchsizeexceeded"}},
	{"overloaded", []string{"serveroverloaded", "requestbursttoofast", "modelloading"}},
	{"not_found", []string{"invalidendpointormodel", "invalidendpointnotfound", "invalidendpointmodelidaccessdisabled", "modelnotopen", "unsupportedmodel", "pathnotfound"}},
	{"unsupported_parameter", []string{"invalidparameterunsupportedparameter"}},
	{"invalid_request", []string{"invalidparameter", "invalidargumenterror", "invalidimageurl", "invalidendpointclosedendpoint", "missingparameter"}},
	{"permission", []string{"accessdenied", "operationdenied", "forbidden", "invalidsubscription"}},
	{"authentication", []string{"authenticationerror", "invalidauthentication", "apikeynotvalid", "signaturenotmatch"}},
	{"unavailable", []string{"serviceconnectionrefused", "noavailableservice", "serviceunavailable"}},
	{"timeout", []string{"requesttimeout", "modeltimeout"}},
	{"stream_interrupted", []string{"requestcanceled"}},
	{"server", []string{"internalserviceerror", "internalservererror"}},
}

// messageRules 是最后的结构化兜底：文本只用于分类，绝不进公开文案。
var messageRules = []struct {
	spec    string
	phrases []string
}{
	{"context_length", []string{"context length", "context window", "token length too long", "maximum context length", "too many tokens"}},
	{"content_filter", []string{"content filter", "high risk", "sensitive content", "content safety", "risk control", "inappropriate content", "data inspection"}},
	{"quota", []string{"insufficient balance", "insufficient quota", "credit balance", "account arrearage", "account overdue", "token quota"}},
	{"overloaded", []string{"engine overloaded", "server overloaded", "currently overloaded", "request burst too fast"}},
	{"rate_limit", []string{"rate limit", "too many requests", "rpm limit", "tpm limit"}},
	{"authentication", []string{"invalid authentication", "incorrect api key", "invalid api key", "apikeynotvalid"}},
	{"permission", []string{"permission denied", "access denied", "not allowed to access"}},
	{"not_found", []string{"model not found", "resource not found"}},
	{"timeout", []string{"gateway time-out", "gateway timeout", "request timeout", "timed out"}},
}

// statusSpecs 是状态码兜底映射。
func statusSpec(status int) (string, bool) {
	switch status {
	case 400, 422:
		return "invalid_request", true
	case 401:
		return "authentication", true
	case 402:
		return "quota", true // 契约优先级 0：先于内层 type/code
	case 403:
		return "permission", true
	case 404:
		return "not_found", true
	case 408:
		return "request_timeout", true
	case 409:
		return "conflict", true
	case 413:
		return "request_too_large", true
	case 429:
		return "rate_limit", true
	case 500:
		return "server", true
	case 502:
		return "connection", true
	case 503:
		return "unavailable", true
	case 504:
		return "timeout", true
	}
	if status >= 500 {
		return "server", true
	}
	return "", false
}

// specFromTokens 用小写化的 token 做精确/前缀匹配。
func specFromTokens(tokens []string) (string, bool) {
	seen := map[string]bool{}
	for _, v := range tokens {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		for _, rule := range tokenRules {
			for _, t := range rule.tokens {
				if v == t {
					return rule.spec, true
				}
			}
		}
		// Ark 的点号变体：按族前缀匹配（仅 Ark 段，不跨厂商）
		for _, rule := range arkPrefixRules {
			for _, t := range rule.phrases {
				if len(t) >= 8 && strings.HasPrefix(v, t) {
					return rule.spec, true
				}
			}
		}
	}
	return "", false
}

// specFromText 只在没有结构化线索时使用，且只返回分类结果。
func specFromText(text string) (string, bool) {
	t := strings.ToLower(text)
	if t == "" {
		return "", false
	}
	for _, r := range messageRules {
		for _, p := range r.phrases {
			if strings.Contains(t, p) {
				return r.spec, true
			}
		}
	}
	return "", false
}

// classify 把一次失败归到 spec key。
func classify(err error) string {
	if err == nil {
		return "unknown"
	}
	// 1) 客户端取消 / 上下文超时
	if errors.Is(err, context.Canceled) {
		return "stream_interrupted"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return "unknown"
	}
	// 2) 402 先行：上游常把余额不足包成 invalid_request_error
	if ue.Status == http.StatusPaymentRequired {
		return "quota"
	}
	// 3) 结构化 code / type
	if s, ok := specFromTokens([]string{ue.Code}); ok {
		return s
	}
	// 4) 文本仅用于分类
	if s, ok := specFromText(ue.Message); ok {
		return s
	}
	// 5) 传输层
	switch ue.Kind {
	case upstream.KindCanceled:
		return "stream_interrupted"
	case upstream.KindNetwork:
		if ue.Status == 0 {
			return "connection"
		}
	case upstream.KindQuota:
		return "quota"
	case upstream.KindAuth:
		if ue.Status == http.StatusForbidden {
			return "permission"
		}
		return "authentication"
	case upstream.KindRate:
		return "rate_limit"
	}
	// 6) 状态码兜底
	if ue.Status > 0 {
		if s, ok := statusSpec(ue.Status); ok {
			return s
		}
	}
	return "unknown"
}

// specOf 返回公开规格。
func specOf(err error) Spec {
	if s, ok := specs[classify(err)]; ok {
		return s
	}
	return specs["unknown"]
}

// publicError 是公开错误体（严格四键）。
func publicError(spec Spec, param string) map[string]any {
	p := any(nil)
	if param != "" && allowedParam(param) {
		p = param
	}
	return map[string]any{
		"error": map[string]any{
			"message": spec.Message,
			"type":    spec.Type,
			"param":   p,
			"code":    spec.Code,
		},
	}
}

// allowedParams 是 OpenAI 已知顶层参数白名单：嵌套路径、配置键、URL 一律置 null。
var allowedParams = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "n": true, "stream": true, "stream_options": true,
	"stop": true, "presence_penalty": true, "frequency_penalty": true, "logit_bias": true,
	"user": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"response_format": true, "seed": true, "logprobs": true, "top_logprobs": true,
	"reasoning_effort": true, "modalities": true, "audio": true, "prediction": true,
	"service_tier": true, "store": true, "metadata": true, "functions": true,
	"function_call": true,
}

func allowedParam(p string) bool { return allowedParams[p] }

// writeSpec 输出公开错误。
func writeSpec(w http.ResponseWriter, spec Spec, param string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if spec.Retryable {
		// 只在内部可重试时给出 Retry-After，且固定为整数秒。
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(spec.Status)
	_ = json.NewEncoder(w).Encode(publicError(spec, param))
}

// writeSpecStatus 允许在保持公开 type/code 的前提下改写 HTTP 状态
// （例如 405 Method Not Allowed 仍归类为 invalid_request）。
func writeSpecStatus(w http.ResponseWriter, spec Spec, status int, param string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(publicError(spec, param))
}

// sseErrorFrame 生成流式失败帧；首包后失败只能这样暴露，不能再改 HTTP 状态。
func sseErrorFrame(spec Spec) []byte {
	buf, _ := json.Marshal(publicError(spec, ""))
	return []byte("data: " + string(buf) + "\n\n")
}
