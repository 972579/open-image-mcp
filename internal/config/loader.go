package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Source says which layer supplied an effective value.
type Source string

// The layers, from lowest to highest priority as the requirement specifies:
// environment variables > config file > startup flags > built-in default.
const (
	SourceDefault Source = "default"
	SourceFlag    Source = "flag"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
)

// Keys of the fields that flags or environment variables can override. The GUI
// uses them to show where an effective value came from.
const (
	KeyServerHost   = "server.host"
	KeyServerPort   = "server.port"
	KeyServerToken  = "server.auth_token"
	KeyAutostart    = "server.autostart"
	KeyDefaultModel = "defaults.model"
	KeyOutputDir    = "defaults.output_dir"
)

// Overrides are the values startup flags supply. A nil pointer means the flag
// was not given.
type Overrides struct {
	ConfigPath   string
	Host         *string
	Port         *int
	OutputDir    *string
	DefaultModel *string
	Autostart    *bool
}

// LoadOptions tells Load where to read from.
type LoadOptions struct {
	// Flags are the startup-flag overrides (lowest priority above defaults).
	Flags Overrides
	// Getenv reads environment variables; nil means os.Getenv. Tests pass a
	// map-backed function.
	Getenv func(string) string
}

// Loaded is the result of reading every layer.
type Loaded struct {
	// Path is the config file that was read (it may not exist yet).
	Path string
	// Exists reports whether Path existed when it was read.
	Exists bool
	// File is the file layer on top of defaults, with ${ENV} references kept
	// literally. The GUI edits a clone of this and saves it back.
	File Config
	// Effective is every layer merged, ${ENV} expanded and paths normalized.
	// The engine and the MCP server use this.
	Effective Config
	// Sources maps a Key* constant to the layer that supplied its value.
	Sources map[string]Source
	// EnvProvider reports whether OPENAI_API_KEY synthesized the "env" provider.
	EnvProvider bool
}

// HomeDir returns ~/.gen-openai-image, where config, history and logs live.
func HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".gen-openai-image")
}

// DefaultConfigPath returns ~/.gen-openai-image/config.yaml.
func DefaultConfigPath() string { return filepath.Join(HomeDir(), "config.yaml") }

// HistoryPath returns ~/.gen-openai-image/history.jsonl.
func HistoryPath() string { return filepath.Join(HomeDir(), "history.jsonl") }

// LogDir returns ~/.gen-openai-image/logs.
func LogDir() string { return filepath.Join(HomeDir(), "logs") }

// ResolvePath picks the config file path: GENIMAGE_CONFIG beats --config, which
// beats the default location.
//
// opts supplies the flag value and the environment reader.
func ResolvePath(opts LoadOptions) string {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if p := strings.TrimSpace(getenv("GENIMAGE_CONFIG")); p != "" {
		return ExpandHome(p)
	}
	if p := strings.TrimSpace(opts.Flags.ConfigPath); p != "" {
		return ExpandHome(p)
	}
	return DefaultConfigPath()
}

// Load reads every layer and merges them. A missing config file is not an
// error: the file layer is then just the defaults. It does not validate; call
// Effective.Validate for that.
//
// opts supplies startup flags and the environment reader. It returns an error
// only when the file exists but cannot be read or parsed.
func Load(opts LoadOptions) (*Loaded, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path := ResolvePath(opts)
	loaded := &Loaded{Path: path, File: Default(), Sources: map[string]Source{}}

	var root *yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		loaded.Exists = true
		if err := yaml.Unmarshal(data, &loaded.File); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err == nil && len(doc.Content) > 0 {
			root = doc.Content[0]
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	eff := loaded.File.Clone()
	src := loaded.Sources
	// Mark which overridable keys the file itself set.
	for _, key := range []string{KeyServerHost, KeyServerPort, KeyServerToken, KeyAutostart, KeyDefaultModel, KeyOutputDir} {
		if hasKey(root, key) {
			src[key] = SourceFile
		} else {
			src[key] = SourceDefault
		}
	}

	// Startup flags: lowest explicit layer, so they only fill keys the file left out.
	f := opts.Flags
	applyFlag := func(key string, set bool, apply func()) {
		if set && src[key] != SourceFile {
			apply()
			src[key] = SourceFlag
		}
	}
	applyFlag(KeyServerHost, f.Host != nil, func() { eff.Server.Host = *f.Host })
	applyFlag(KeyServerPort, f.Port != nil, func() { eff.Server.Port = *f.Port })
	applyFlag(KeyOutputDir, f.OutputDir != nil, func() { eff.Defaults.OutputDir = *f.OutputDir })
	applyFlag(KeyDefaultModel, f.DefaultModel != nil, func() { eff.Defaults.Model = *f.DefaultModel })
	applyFlag(KeyAutostart, f.Autostart != nil, func() { eff.Server.Autostart = *f.Autostart })

	// Environment variables: highest layer.
	applyEnv := func(key, name string, apply func(string) error) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return
		}
		if apply(v) == nil {
			src[key] = SourceEnv
		}
	}
	applyEnv(KeyServerHost, "GENIMAGE_HOST", func(v string) error { eff.Server.Host = v; return nil })
	applyEnv(KeyServerPort, "GENIMAGE_PORT", func(v string) error {
		p, err := strconv.Atoi(v)
		if err == nil {
			eff.Server.Port = p
		}
		return err
	})
	applyEnv(KeyServerToken, "GENIMAGE_TOKEN", func(v string) error { eff.Server.AuthToken = v; return nil })
	applyEnv(KeyDefaultModel, "DEFAULT_MODEL", func(v string) error { eff.Defaults.Model = v; return nil })
	applyEnv(KeyOutputDir, "OUTPUT_DIR", func(v string) error { eff.Defaults.OutputDir = v; return nil })

	if key := strings.TrimSpace(getenv("OPENAI_API_KEY")); key != "" {
		eff.Providers = append([]Provider{envProvider(getenv, key, eff.Defaults.Model)}, eff.Providers...)
		loaded.EnvProvider = true
	}

	expandReferences(&eff, getenv)
	Normalize(&eff)
	loaded.Effective = eff
	return loaded, nil
}

// envProvider builds the provider described by OPENAI_BASE_URL, OPENAI_API_KEY
// and OPENAI_MODELS (or DEFAULT_MODEL when OPENAI_MODELS is empty).
//
// getenv reads variables, key is the API key and defaultModel is the effective
// default model. Every model is marked as supporting generation and editing,
// since the environment cannot say which it supports.
func envProvider(getenv func(string) string, key, defaultModel string) Provider {
	base := strings.TrimSpace(getenv("OPENAI_BASE_URL"))
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	var models []Model
	for _, id := range strings.Split(getenv("OPENAI_MODELS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			models = append(models, Model{ID: id, Generate: true, Edit: true})
		}
	}
	if len(models) == 0 {
		id := defaultModel
		if id == "" {
			id = "gpt-image-1"
		}
		models = []Model{{ID: id, Generate: true, Edit: true}}
	}
	return Provider{
		Name:    "env",
		Dialect: DialectOpenAI,
		BaseURL: base,
		APIKey:  key,
		Enabled: true,
		Models:  models,
		FromEnv: true,
	}
}

// hasKey reports whether a dotted key such as "server.port" is present in the
// parsed YAML mapping root (nil means no file).
func hasKey(root *yaml.Node, dotted string) bool {
	node := root
	for _, part := range strings.Split(dotted, ".") {
		if node == nil || node.Kind != yaml.MappingNode {
			return false
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == part {
				next = node.Content[i+1]
				break
			}
		}
		node = next
	}
	return node != nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${NAME} references in s using getenv. Unset variables
// expand to "".
func ExpandEnv(s string, getenv func(string) string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return getenv(envRef.FindStringSubmatch(m)[1])
	})
}

// expandReferences expands ${ENV} in the fields where secrets or endpoints
// usually come from the environment: provider base_url, api_key and headers,
// and the server token.
func expandReferences(c *Config, getenv func(string) string) {
	c.Server.AuthToken = ExpandEnv(c.Server.AuthToken, getenv)
	for i := range c.Providers {
		p := &c.Providers[i]
		p.BaseURL = ExpandEnv(p.BaseURL, getenv)
		p.APIKey = ExpandEnv(p.APIKey, getenv)
		for k, v := range p.Headers {
			p.Headers[k] = ExpandEnv(v, getenv)
		}
	}
}

// Normalize cleans an effective configuration in place: trims whitespace,
// strips trailing slashes from base URLs, expands ~ in the output directory
// and fills zero values with defaults.
func Normalize(c *Config) {
	d := Default()
	c.Server.Host = strings.TrimSpace(c.Server.Host)
	if c.Server.Host == "" {
		c.Server.Host = d.Server.Host
	}
	if c.Server.Port == 0 {
		c.Server.Port = d.Server.Port
	}
	c.Server.Path = strings.TrimSpace(c.Server.Path)
	if c.Server.Path == "" {
		c.Server.Path = d.Server.Path
	}
	c.Server.AuthToken = strings.TrimSpace(c.Server.AuthToken)
	c.Defaults.Model = strings.TrimSpace(c.Defaults.Model)
	if c.Defaults.N == 0 {
		c.Defaults.N = 1
	}
	if strings.TrimSpace(c.Defaults.OutputDir) == "" {
		c.Defaults.OutputDir = d.Defaults.OutputDir
	}
	c.Defaults.OutputDir = ExpandHome(strings.TrimSpace(c.Defaults.OutputDir))
	if c.Defaults.FilenamePrefix == "" {
		c.Defaults.FilenamePrefix = d.Defaults.FilenamePrefix
	}
	if c.Defaults.PreviewMaxPx <= 0 {
		c.Defaults.PreviewMaxPx = d.Defaults.PreviewMaxPx
	}
	if c.Request.Timeout <= 0 {
		c.Request.Timeout = d.Request.Timeout
	}
	if c.Request.MaxRetries < 0 {
		c.Request.MaxRetries = 0
	}
	if c.Request.MaxConcurrency <= 0 {
		c.Request.MaxConcurrency = d.Request.MaxConcurrency
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.APIKey = strings.TrimSpace(p.APIKey)
		if p.Dialect == "" {
			p.Dialect = DialectOpenAI
		}
	}
}

// ExpandHome turns a leading "~" or "~/" into the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Resolve builds an effective configuration from an edited file layer, using
// the same flags and environment as an earlier Load. The GUI calls it after
// saving so the engine picks up the change without re-reading the disk.
//
// file is the edited file layer; prev is the Loaded whose Sources say which
// keys flags and the environment control. It returns the new Loaded.
func Resolve(file Config, prev *Loaded, opts LoadOptions) *Loaded {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	next := &Loaded{Path: prev.Path, Exists: true, File: file.Clone(), Sources: map[string]Source{}}
	eff := file.Clone()
	for key, s := range prev.Sources {
		// A key the old file did not set may be set now; the GUI always writes
		// full files, so treat every key as file-supplied unless env wins.
		if s == SourceEnv {
			next.Sources[key] = SourceEnv
		} else {
			next.Sources[key] = SourceFile
		}
	}
	envOverride := func(key, name string, apply func(string)) {
		if next.Sources[key] == SourceEnv {
			if v := strings.TrimSpace(getenv(name)); v != "" {
				apply(v)
			}
		}
	}
	envOverride(KeyServerHost, "GENIMAGE_HOST", func(v string) { eff.Server.Host = v })
	envOverride(KeyServerPort, "GENIMAGE_PORT", func(v string) {
		if p, err := strconv.Atoi(v); err == nil {
			eff.Server.Port = p
		}
	})
	envOverride(KeyServerToken, "GENIMAGE_TOKEN", func(v string) { eff.Server.AuthToken = v })
	envOverride(KeyDefaultModel, "DEFAULT_MODEL", func(v string) { eff.Defaults.Model = v })
	envOverride(KeyOutputDir, "OUTPUT_DIR", func(v string) { eff.Defaults.OutputDir = v })
	if key := strings.TrimSpace(getenv("OPENAI_API_KEY")); key != "" {
		eff.Providers = append([]Provider{envProvider(getenv, key, eff.Defaults.Model)}, eff.Providers...)
		next.EnvProvider = true
	}
	expandReferences(&eff, getenv)
	Normalize(&eff)
	next.Effective = eff
	return next
}
