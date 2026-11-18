package imageapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Kind classifies an upstream failure so callers can decide whether to retry,
// fall back to another provider, or report straight away.
type Kind string

// Error kinds. The string values are the Chinese labels shown to users.
const (
	KindAuth        Kind = "鉴权失败"
	KindModel       Kind = "模型不支持"
	KindRateLimit   Kind = "限流"
	KindUpstream    Kind = "上游错误"
	KindTimeout     Kind = "请求超时"
	KindBadRequest  Kind = "参数错误"
	KindNetwork     Kind = "网络错误"
	KindBadResponse Kind = "响应异常"
	KindConfig      Kind = "配置错误"
	KindCanceled    Kind = "已取消"
)

// Error is a classified failure from one provider. Its message never contains
// the API key.
type Error struct {
	Kind     Kind
	Provider string
	Status   int    // HTTP status, 0 when no response arrived
	Message  string // upstream or local message, truncated
	Code     string // upstream error code when it sent one
	Hint     string // what the user can do about it
	Err      error  // underlying error, for errors.Is
}

// Error formats the failure as "[鉴权失败] provider=x status=401: msg 建议：hint".
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", e.Kind)
	if e.Provider != "" {
		fmt.Fprintf(&b, " provider=%s", e.Provider)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " status=%d", e.Status)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " code=%s", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Hint != "" {
		b.WriteString(" 建议：")
		b.WriteString(e.Hint)
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether the same provider may succeed if asked again:
// network failures, timeouts, rate limits and 5xx responses.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindNetwork, KindTimeout, KindRateLimit, KindUpstream:
		return true
	}
	return false
}

// Fallbackable reports whether another provider serving the same model is
// worth trying. Authentication and parameter errors are reported directly, as
// the approved plan specifies, and a cancelled call stops everything.
func (e *Error) Fallbackable() bool {
	switch e.Kind {
	case KindAuth, KindBadRequest, KindCanceled, KindConfig:
		return false
	}
	return true
}

// AsError returns err as an *Error when it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// classifyTransport turns an http.Client error into an *Error.
//
// provider names the provider; err is what Client.Do returned.
func classifyTransport(provider string, err error) *Error {
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Kind: KindCanceled, Provider: provider, Message: "调用已取消", Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: KindTimeout, Provider: provider, Message: "上游在超时时间内没有响应", Hint: "生图较慢时可在设置中调大请求超时", Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: KindTimeout, Provider: provider, Message: "上游在超时时间内没有响应", Hint: "生图较慢时可在设置中调大请求超时", Err: err}
	}
	return &Error{Kind: KindNetwork, Provider: provider, Message: truncate(err.Error(), 300), Hint: "检查 base_url 是否可访问、是否需要代理", Err: err}
}

// parseAPIError turns a non-2xx response into an *Error, reading the common
// error body shapes: {"error":{"message","code","type"}}, {"error":"..."},
// {"message","code"} (SiliconFlow) and {"detail":"..."}.
//
// provider names the provider, status is the HTTP status and body the raw
// response body.
func parseAPIError(provider string, status int, body []byte) *Error {
	msg, code := extractMessage(body)
	if msg == "" {
		msg = http.StatusText(status)
	}
	e := &Error{Provider: provider, Status: status, Message: truncate(msg, 500), Code: code}
	lower := strings.ToLower(msg + " " + code)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = KindAuth
		e.Hint = "检查 API Key 是否正确、是否有该模型的权限"
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimit
		e.Hint = "请求过于频繁或额度不足，稍后再试"
	case isModelProblem(status, lower):
		e.Kind = KindModel
		e.Hint = "确认模型 ID 拼写正确，且该服务商确实提供此模型"
	case status == http.StatusNotFound:
		e.Kind = KindBadRequest
		e.Hint = "接口地址不存在：检查 base_url 是否包含 /v1，以及服务商是否支持该接口"
	case status >= 500:
		e.Kind = KindUpstream
		e.Hint = "上游服务异常，可稍后重试或切换服务商"
	default:
		e.Kind = KindBadRequest
		e.Hint = "检查 size / quality 等参数是否被该模型支持"
	}
	return e
}

// isModelProblem detects "this model does not exist / is not served here"
// across OpenAI, new-api and SiliconFlow wording.
func isModelProblem(status int, lower string) bool {
	if strings.Contains(lower, "model_not_found") || strings.Contains(lower, "无可用渠道") {
		return true
	}
	if !strings.Contains(lower, "model") && !strings.Contains(lower, "模型") {
		return false
	}
	for _, s := range []string{"not found", "does not exist", "not exist", "not support", "unsupported", "invalid model", "不存在", "不支持"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return status == http.StatusNotFound
}

// extractMessage pulls a human message and an error code out of an error body.
func extractMessage(body []byte) (msg, code string) {
	var shape struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  any             `json:"detail"`
		Code    any             `json:"code"`
	}
	if json.Unmarshal(body, &shape) != nil {
		return truncate(strings.TrimSpace(string(body)), 500), ""
	}
	if len(shape.Error) > 0 {
		var inner struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		}
		if json.Unmarshal(shape.Error, &inner) == nil && inner.Message != "" {
			code = stringify(inner.Code)
			if code == "" {
				code = inner.Type
			}
			return inner.Message, code
		}
		var s string
		if json.Unmarshal(shape.Error, &s) == nil && s != "" {
			return s, stringify(shape.Code)
		}
	}
	if shape.Message != "" {
		return shape.Message, stringify(shape.Code)
	}
	if shape.Detail != nil {
		return stringify(shape.Detail), stringify(shape.Code)
	}
	return truncate(strings.TrimSpace(string(body)), 500), ""
}

// stringify renders a JSON scalar (string or number) as text; other shapes are
// re-encoded compactly.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// truncate shortens s to at most n runes, adding "…" when it cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
