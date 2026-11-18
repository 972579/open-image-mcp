// Package imageapi calls OpenAI-compatible image endpoints
// (/v1/images/generations and /v1/images/edits) and the dialects relays use,
// and turns their responses and failures into one shape.
package imageapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Target is one provider plus the model to call on it.
type Target struct {
	Provider   string
	Dialect    string
	BaseURL    string // includes /v1, no trailing slash
	APIKey     string
	Headers    map[string]string
	ExtraBody  map[string]any // provider-level fields merged into every request
	Model      string
	ModelExtra map[string]any // model-level fields merged into every request
}

// GenerateParams are the text-to-image options. Empty strings and zero values
// are left out of the request so each upstream applies its own default.
type GenerateParams struct {
	Prompt         string
	Size           string
	N              int
	Quality        string
	Style          string
	Seed           *int64
	ResponseFormat string
	OutputFormat   string
	Background     string
	NegativePrompt string
	// Extra is merged last, so it can override anything above.
	Extra map[string]any
}

// InputImage is a reference image or mask for an edit.
type InputImage struct {
	Name string // file name sent in multipart uploads
	MIME string // image/png, image/jpeg or image/webp
	Data []byte
}

// EditParams are the image-to-image options.
type EditParams struct {
	GenerateParams
	Images []InputImage
	Mask   *InputImage
}

// Image is one generated image: decoded bytes, a URL to download, or both.
type Image struct {
	Data          []byte
	URL           string
	RevisedPrompt string
}

// Response is a successful generation or edit.
type Response struct {
	Images []Image
	Seed   *int64
	Debug  Debug
}

// Debug describes the HTTP exchange with secrets and base64 payloads removed,
// for the GUI's request/response viewer.
type Debug struct {
	Method       string
	URL          string
	RequestBody  string
	Status       int
	ResponseBody string
	Elapsed      time.Duration
}

// Client sends requests to providers. It is safe for concurrent use.
type Client struct {
	HTTP *http.Client
	// MaxResponseBytes caps how much of a response body is read (default 256 MiB).
	MaxResponseBytes int64
}

// New returns a Client using hc, or a default client when hc is nil. Timeouts
// come from the caller's context, so the default client has none of its own.
func New(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{HTTP: hc, MaxResponseBytes: 256 << 20}
}

// Generate calls the provider's text-to-image endpoint and parses the images
// out of the response.
//
// ctx bounds the call (its deadline is the request timeout); t names the
// provider and model; p holds the options. The returned Debug is filled even
// when an error is returned, as far as the exchange got.
func (c *Client) Generate(ctx context.Context, t Target, p GenerateParams) (*Response, Debug, error) {
	var body map[string]any
	switch t.Dialect {
	case "siliconflow":
		body = siliconflowGenerateBody(t, p)
	default:
		body = openaiGenerateBody(t, p)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, Debug{}, &Error{Kind: KindBadRequest, Provider: t.Provider, Message: "请求参数无法编码为 JSON: " + err.Error()}
	}
	return c.do(ctx, t, "/images/generations", "application/json", payload, RedactJSON(payload))
}

// Edit calls the provider's image-to-image endpoint: multipart
// /images/edits for the openai dialect, JSON /images/generations with data
// URLs for siliconflow.
//
// ctx bounds the call; t names the provider and model; p holds the prompt,
// options and reference images (at least one).
func (c *Client) Edit(ctx context.Context, t Target, p EditParams) (*Response, Debug, error) {
	if len(p.Images) == 0 {
		return nil, Debug{}, &Error{Kind: KindBadRequest, Provider: t.Provider, Message: "编辑至少需要一张参考图"}
	}
	if t.Dialect == "siliconflow" {
		body := siliconflowEditBody(t, p)
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, Debug{}, &Error{Kind: KindBadRequest, Provider: t.Provider, Message: "请求参数无法编码为 JSON: " + err.Error()}
		}
		return c.do(ctx, t, "/images/generations", "application/json", payload, RedactJSON(payload))
	}
	payload, contentType, summary, err := openaiEditMultipart(t, p)
	if err != nil {
		return nil, Debug{}, &Error{Kind: KindBadRequest, Provider: t.Provider, Message: "构造 multipart 请求失败: " + err.Error()}
	}
	return c.do(ctx, t, "/images/edits", contentType, payload, summary)
}

// ListModels calls GET {base}/models and returns the model IDs, for the GUI's
// connection test and model import.
//
// ctx bounds the call; t names the provider (Model is ignored).
func (c *Client) ListModels(ctx context.Context, t Target) ([]string, Debug, error) {
	start := time.Now()
	dbg := Debug{Method: http.MethodGet, URL: t.BaseURL + "/models"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dbg.URL, nil)
	if err != nil {
		return nil, dbg, &Error{Kind: KindConfig, Provider: t.Provider, Message: "base_url 无效: " + err.Error()}
	}
	setHeaders(req, t)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		dbg.Elapsed = time.Since(start)
		return nil, dbg, classifyTransport(t.Provider, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	dbg.Status, dbg.Elapsed, dbg.ResponseBody = resp.StatusCode, time.Since(start), RedactJSON(raw)
	if resp.StatusCode >= 300 {
		return nil, dbg, parseAPIError(t.Provider, resp.StatusCode, raw)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, dbg, &Error{Kind: KindBadResponse, Provider: t.Provider, Status: resp.StatusCode, Message: "无法解析 /models 响应: " + truncate(string(raw), 200)}
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, dbg, nil
}

// Download fetches an image URL returned by a provider. The provider's
// Authorization header is sent only when the URL is on the provider's own
// host, so the key never leaks to third-party storage.
//
// ctx bounds the download; t is the provider that returned the URL; rawURL is
// the image link. It returns the image bytes.
func (c *Client) Download(ctx context.Context, t Target, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &Error{Kind: KindBadResponse, Provider: t.Provider, Message: "上游返回的图片 URL 无效"}
	}
	if sameHost(rawURL, t.BaseURL) {
		setHeaders(req, t)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, classifyTransport(t.Provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, &Error{Kind: KindBadResponse, Provider: t.Provider, Status: resp.StatusCode, Message: "下载图片失败", Hint: "图片链接可能已过期"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()))
	if err != nil {
		return nil, classifyTransport(t.Provider, err)
	}
	return data, nil
}

// do sends one POST and parses the image response.
//
// path is appended to the base URL; contentType and payload form the body;
// summary is the redacted body text kept in Debug.
func (c *Client) do(ctx context.Context, t Target, path, contentType string, payload []byte, summary string) (*Response, Debug, error) {
	start := time.Now()
	dbg := Debug{Method: http.MethodPost, URL: t.BaseURL + path, RequestBody: summary}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dbg.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, dbg, &Error{Kind: KindConfig, Provider: t.Provider, Message: "base_url 无效: " + err.Error()}
	}
	setHeaders(req, t)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		dbg.Elapsed = time.Since(start)
		return nil, dbg, classifyTransport(t.Provider, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()))
	dbg.Status, dbg.Elapsed = resp.StatusCode, time.Since(start)
	if err != nil {
		return nil, dbg, classifyTransport(t.Provider, err)
	}
	dbg.ResponseBody = RedactJSON(raw)
	if resp.StatusCode >= 300 {
		return nil, dbg, parseAPIError(t.Provider, resp.StatusCode, raw)
	}
	out, perr := parseImages(raw)
	if perr != nil {
		return nil, dbg, &Error{Kind: KindBadResponse, Provider: t.Provider, Status: resp.StatusCode, Message: perr.Error(), Hint: "该服务商的返回格式可能不兼容，可在调试台查看原始响应"}
	}
	out.Debug = dbg
	return out, dbg, nil
}

// setHeaders adds authorization and the provider's custom headers.
func setHeaders(req *http.Request, t Target) {
	if t.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
	}
	req.Header.Set("User-Agent", "gen-openai-image-mcp/1.0")
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
}

// maxBytes returns the response cap, defaulting to 256 MiB.
func (c *Client) maxBytes() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return 256 << 20
}

// sameHost reports whether two URLs share a host name.
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Hostname(), ub.Hostname())
}

// EndpointPreview returns the full URL a generation request will go to, for
// the provider form's "将请求：" hint.
func EndpointPreview(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/images/generations", base)
}
