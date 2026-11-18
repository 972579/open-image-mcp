package imageapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// rawImage is one entry of data[] or images[]. Relays disagree on field names,
// so every known spelling is read.
type rawImage struct {
	B64           string `json:"b64_json"`
	Base64        string `json:"base64"`
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt"`
}

// parseImages reads data[] (OpenAI) and images[] (SiliconFlow) from a success
// body, whichever dialect was used. Base64 may arrive bare or as a data URL,
// and a "url" may itself be a data URL.
//
// raw is the response body. It returns an error when the body is not JSON or
// holds no image.
func parseImages(raw []byte) (*Response, error) {
	var body struct {
		Data   []rawImage `json:"data"`
		Images []rawImage `json:"images"`
		Seed   *int64     `json:"seed"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("响应不是有效的 JSON: %s", truncate(strings.TrimSpace(string(raw)), 200))
	}
	out := &Response{Seed: body.Seed}
	for _, r := range append(body.Data, body.Images...) {
		img := Image{RevisedPrompt: r.RevisedPrompt}
		b64 := r.B64
		if b64 == "" {
			b64 = r.Base64
		}
		switch {
		case b64 != "":
			data, err := DecodeBase64(b64)
			if err != nil {
				return nil, fmt.Errorf("图片 base64 解码失败: %w", err)
			}
			img.Data = data
		case strings.HasPrefix(r.URL, "data:"):
			data, err := DecodeBase64(r.URL)
			if err != nil {
				return nil, fmt.Errorf("图片 data URL 解码失败: %w", err)
			}
			img.Data = data
		case r.URL != "":
			img.URL = r.URL
		default:
			continue
		}
		out.Images = append(out.Images, img)
	}
	if len(out.Images) == 0 {
		return nil, fmt.Errorf("响应中没有图片（未找到 data[] 或 images[]）: %s", truncate(RedactJSON(raw), 200))
	}
	return out, nil
}

// DecodeBase64 decodes standard or URL-safe base64, with or without padding,
// optionally wrapped as a data URL ("data:image/png;base64,....").
func DecodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		i := strings.Index(s, ",")
		if i < 0 {
			return nil, errors.New("data URL 缺少逗号")
		}
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := enc.DecodeString(s); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("不是有效的 base64")
}

// RedactJSON renders a JSON body for display with long base64 strings and data
// URLs replaced by "<base64 123KB>" and secret-looking fields masked. Non-JSON
// input is returned truncated.
func RedactJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return RedactText(truncate(string(raw), 2000))
	}
	v = redactValue("", v)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return truncate(string(raw), 2000)
	}
	return string(out)
}

// redactValue walks a decoded JSON value, shortening base64 payloads and
// masking values under secret-looking keys.
func redactValue(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = redactValue(k, x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = redactValue(key, x)
		}
		return t
	case string:
		lk := strings.ToLower(key)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "authorization") || strings.Contains(lk, "secret") {
			return "****"
		}
		if len(t) > 256 && (strings.HasPrefix(t, "data:") || looksBase64(t)) {
			return fmt.Sprintf("<base64 %dKB>", len(t)*3/4/1024)
		}
		return RedactText(t)
	default:
		return v
	}
}

// looksBase64 reports whether s is plausibly a base64 payload (checks a prefix
// so huge strings stay cheap).
func looksBase64(s string) bool {
	n := len(s)
	if n > 512 {
		n = 512
	}
	for _, r := range s[:n] {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

var secretPattern = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{4})[A-Za-z0-9_\-]{4,}|(bearer\s+)[A-Za-z0-9_\-.]{8,}`)

// RedactText masks API-key-looking substrings ("sk-…", "Bearer …") in free
// text. It is the safety net applied to every log line and debug string.
func RedactText(s string) string {
	if !strings.Contains(s, "sk-") && !strings.Contains(strings.ToLower(s), "bearer") {
		return s
	}
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(strings.ToLower(m), "bearer") {
			return "Bearer ****"
		}
		if len(m) > 4 {
			return m[:3] + "****" + m[len(m)-4:]
		}
		return "****"
	})
}
