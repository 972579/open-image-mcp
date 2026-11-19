// Package config defines the GenImage MCP configuration schema, how it is
// layered from defaults, startup flags, the YAML file and environment
// variables, and how the file layer is saved back to disk.
package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Dialect names how a provider's image endpoints are shaped.
const (
	// DialectOpenAI is the standard OpenAI /v1/images/* format, also used by
	// new-api, one-api and most relays.
	DialectOpenAI = "openai"
	// DialectSiliconFlow is SiliconFlow's variant: image_size/batch_size on
	// generations, image editing through the generations endpoint, and an
	// images[] response array.
	DialectSiliconFlow = "siliconflow"
)

// Dialects lists every dialect the image client understands, in display order.
var Dialects = []string{DialectOpenAI, DialectSiliconFlow}

// Config is the whole configuration. The same type holds the file layer (what
// is written to disk, with ${ENV} references unexpanded) and the effective
// configuration (all layers merged and references expanded).
type Config struct {
	Version   int           `yaml:"version"`
	Server    ServerConfig  `yaml:"server"`
	Defaults  Defaults      `yaml:"defaults"`
	Request   RequestConfig `yaml:"request"`
	Update    UpdateConfig  `yaml:"update"`
	Providers []Provider    `yaml:"providers"`
}

// UpdateConfig controls the GitHub Releases update check.
type UpdateConfig struct {
	// CheckOnStartup checks for a newer release when the app opens.
	CheckOnStartup bool `yaml:"check_on_startup"`
}

// ServerConfig controls the Streamable HTTP MCP endpoint.
type ServerConfig struct {
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	Path      string `yaml:"path"`
	AuthToken string `yaml:"auth_token"`
	Autostart bool   `yaml:"autostart"`
}

// Defaults are the values a tool call uses when it leaves a parameter out.
type Defaults struct {
	Model          string `yaml:"model"`
	Size           string `yaml:"size"`
	N              int    `yaml:"n"`
	OutputDir      string `yaml:"output_dir"`
	FilenamePrefix string `yaml:"filename_prefix"`
	IncludePreview bool   `yaml:"include_preview"`
	PreviewMaxPx   int    `yaml:"preview_max_px"`
}

// RequestConfig controls upstream HTTP behaviour.
type RequestConfig struct {
	Timeout    Duration `yaml:"timeout"`
	MaxRetries int      `yaml:"max_retries"`
	Fallback   bool     `yaml:"fallback"`
	// MaxConcurrency caps how many upstream image requests run at the same
	// time across every tool call, batch task and debug-console call; further
	// requests wait for a free slot. Retries and fallbacks each take a slot
	// only while their request is in flight.
	MaxConcurrency int `yaml:"max_concurrency"`
}

// MaxConcurrencyLimit is the highest request.max_concurrency accepted.
const MaxConcurrencyLimit = 16

// Provider is one OpenAI-compatible service with its own key and models.
type Provider struct {
	Name      string            `yaml:"name"`
	Dialect   string            `yaml:"dialect"`
	BaseURL   string            `yaml:"base_url"`
	APIKey    string            `yaml:"api_key"`
	Enabled   bool              `yaml:"enabled"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	ExtraBody map[string]any    `yaml:"extra_body,omitempty"`
	Models    []Model           `yaml:"models"`

	// FromEnv marks the provider synthesized from OPENAI_* variables. It is
	// never written to the file.
	FromEnv bool `yaml:"-"`
}

// Model is one image model a provider serves, with the capabilities the
// router may use it for.
type Model struct {
	ID       string         `yaml:"id"`
	Generate bool           `yaml:"generate"`
	Edit     bool           `yaml:"edit"`
	Sizes    []string       `yaml:"sizes,omitempty"`
	Extra    map[string]any `yaml:"extra,omitempty"`
}

// Duration is a time.Duration that reads and writes as "180s" in YAML.
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalYAML writes the duration in Go's duration syntax, e.g. "3m0s".
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// UnmarshalYAML accepts Go duration strings ("180s", "3m") or a bare number of
// seconds.
//
// node is the YAML scalar being decoded. It returns an error when the value is
// neither form.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if v, err := time.ParseDuration(s); err == nil {
		*d = Duration(v)
		return nil
	}
	var secs int
	if _, err := fmt.Sscanf(s, "%d", &secs); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	return fmt.Errorf("invalid duration %q", s)
}

// UnmarshalYAML decodes a provider, defaulting enabled to true and dialect to
// openai when the file leaves them out.
//
// node is the provider's mapping node.
func (p *Provider) UnmarshalYAML(node *yaml.Node) error {
	type raw Provider
	r := raw{Enabled: true, Dialect: DialectOpenAI}
	if err := node.Decode(&r); err != nil {
		return err
	}
	*p = Provider(r)
	return nil
}

// UnmarshalYAML accepts either a bare model ID ("flux-dev", generation only)
// or a mapping with id/generate/edit/sizes/extra. A mapping that omits
// generate defaults it to true.
//
// node is the scalar or mapping node for one model entry.
func (m *Model) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var id string
		if err := node.Decode(&id); err != nil {
			return err
		}
		*m = Model{ID: strings.TrimSpace(id), Generate: true}
		return nil
	}
	type raw Model
	r := raw{Generate: true}
	if err := node.Decode(&r); err != nil {
		return err
	}
	r.ID = strings.TrimSpace(r.ID)
	*m = Model(r)
	return nil
}

// MarshalYAML writes a generation-only model with no extras as a bare string,
// keeping saved files as short as hand-written ones.
func (m Model) MarshalYAML() (any, error) {
	if m.Generate && !m.Edit && len(m.Sizes) == 0 && len(m.Extra) == 0 {
		return m.ID, nil
	}
	type raw Model
	return raw(m), nil
}

// Default returns the built-in configuration used under every other layer.
func Default() Config {
	return Config{
		Version: 1,
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 8765,
			Path: "/mcp",
		},
		Defaults: Defaults{
			Size:           "1024x1024",
			N:              1,
			OutputDir:      "~/Pictures/gen-openai-image",
			FilenamePrefix: "img",
			IncludePreview: true,
			PreviewMaxPx:   512,
		},
		Request: RequestConfig{
			Timeout:        Duration(180 * time.Second),
			MaxRetries:     1,
			Fallback:       true,
			MaxConcurrency: 4,
		},
		Update: UpdateConfig{CheckOnStartup: true},
	}
}

// Clone returns a deep copy, so the GUI can edit a draft without touching the
// configuration the engine is serving from.
func (c Config) Clone() Config {
	out := c
	out.Providers = make([]Provider, len(c.Providers))
	for i, p := range c.Providers {
		cp := p
		cp.Headers = cloneStrings(p.Headers)
		cp.ExtraBody = cloneAny(p.ExtraBody)
		cp.Models = make([]Model, len(p.Models))
		for j, m := range p.Models {
			mm := m
			mm.Sizes = append([]string(nil), m.Sizes...)
			mm.Extra = cloneAny(m.Extra)
			cp.Models[j] = mm
		}
		out.Providers[i] = cp
	}
	return out
}

// ProviderByName returns the provider with that name, or nil.
//
// name is compared exactly.
func (c *Config) ProviderByName(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// Validate checks the configuration for mistakes that would make the server or
// a tool call fail, and returns them joined in one error (nil when valid).
// It is run on the effective configuration, after ${ENV} expansion.
func (c *Config) Validate() error {
	var problems []string
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		problems = append(problems, fmt.Sprintf("server.port %d 不在 1-65535 范围内", c.Server.Port))
	}
	if !strings.HasPrefix(c.Server.Path, "/") {
		problems = append(problems, "server.path 必须以 / 开头")
	}
	if !IsLoopbackHost(c.Server.Host) && strings.TrimSpace(c.Server.AuthToken) == "" {
		problems = append(problems, fmt.Sprintf("server.host=%s 不是本机回环地址，必须设置 server.auth_token", c.Server.Host))
	}
	if c.Defaults.N < 1 || c.Defaults.N > 10 {
		problems = append(problems, "defaults.n 必须在 1-10 之间")
	}
	if c.Request.MaxConcurrency < 1 || c.Request.MaxConcurrency > MaxConcurrencyLimit {
		problems = append(problems, fmt.Sprintf("request.max_concurrency 必须在 1-%d 之间", MaxConcurrencyLimit))
	}
	seen := map[string]bool{}
	for i, p := range c.Providers {
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if strings.TrimSpace(p.Name) == "" {
			problems = append(problems, fmt.Sprintf("服务商 %s 缺少 name", label))
		} else if seen[p.Name] {
			problems = append(problems, fmt.Sprintf("服务商名称 %q 重复", p.Name))
		}
		seen[p.Name] = true
		if p.Dialect != DialectOpenAI && p.Dialect != DialectSiliconFlow {
			problems = append(problems, fmt.Sprintf("服务商 %s 的 dialect %q 无效（可选 openai / siliconflow）", label, p.Dialect))
		}
		if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			problems = append(problems, fmt.Sprintf("服务商 %s 的 base_url %q 不是有效的 http(s) 地址", label, p.BaseURL))
		}
		ids := map[string]bool{}
		for _, m := range p.Models {
			if m.ID == "" {
				problems = append(problems, fmt.Sprintf("服务商 %s 有空的模型 ID", label))
				continue
			}
			if ids[m.ID] {
				problems = append(problems, fmt.Sprintf("服务商 %s 的模型 %s 重复", label, m.ID))
			}
			ids[m.ID] = true
			if !m.Generate && !m.Edit {
				problems = append(problems, fmt.Sprintf("服务商 %s 的模型 %s 至少要支持生成或编辑之一", label, m.ID))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("配置有误：\n- %s", strings.Join(problems, "\n- "))
}

// IsLoopbackHost reports whether host only accepts connections from this
// machine (localhost, 127.0.0.0/8 or ::1).
//
// host is a bare host name or IP, without a port.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cloneStrings copies a string map; nil stays nil.
func cloneStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneAny deep-copies a JSON-like map so drafts never share nested values.
func cloneAny(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies one JSON-like value (maps and slices recursively).
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAny(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = cloneValue(x)
		}
		return out
	default:
		return v
	}
}
