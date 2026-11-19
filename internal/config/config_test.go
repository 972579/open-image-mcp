package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envMap returns a Getenv backed by a map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const sampleYAML = `
server:
  port: 9000
defaults:
  model: gpt-image-2
providers:
  - name: siliconflow
    dialect: siliconflow
    base_url: https://api.siliconflow.cn/v1/
    api_key: sk-siliconflow-secret
    models:
      - Qwen/Qwen-Image
      - id: Qwen/Qwen-Image-Edit
        generate: false
        edit: true
  - name: relay
    base_url: https://relay.example.com/v1
    api_key: ${RELAY_KEY}
    models: [gpt-image-2]
`

// TestLoadPriority checks env > file > flags > defaults and ${ENV} expansion.
func TestLoadPriority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, model := "127.0.0.2", 7000, "flag-model"
	out := "/tmp/flag-out"
	loaded, err := Load(LoadOptions{
		Flags:  Overrides{ConfigPath: path, Host: &host, Port: &port, DefaultModel: &model, OutputDir: &out},
		Getenv: envMap(map[string]string{"RELAY_KEY": "sk-relay-1234567890", "OUTPUT_DIR": "/tmp/env-out"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := loaded.Effective
	if eff.Server.Port != 9000 || loaded.Sources[KeyServerPort] != SourceFile {
		t.Errorf("file should beat flag for port: got %d from %s", eff.Server.Port, loaded.Sources[KeyServerPort])
	}
	if eff.Server.Host != host || loaded.Sources[KeyServerHost] != SourceFlag {
		t.Errorf("flag should fill host the file left out: got %q from %s", eff.Server.Host, loaded.Sources[KeyServerHost])
	}
	if eff.Defaults.Model != "gpt-image-2" {
		t.Errorf("file should beat flag for model: got %q", eff.Defaults.Model)
	}
	if eff.Defaults.OutputDir != "/tmp/env-out" || loaded.Sources[KeyOutputDir] != SourceEnv {
		t.Errorf("env should beat flag for output dir: got %q from %s", eff.Defaults.OutputDir, loaded.Sources[KeyOutputDir])
	}
	if got := eff.Providers[1].APIKey; got != "sk-relay-1234567890" {
		t.Errorf("${RELAY_KEY} not expanded: %q", got)
	}
	if got := loaded.File.Providers[1].APIKey; got != "${RELAY_KEY}" {
		t.Errorf("file layer must keep the literal reference, got %q", got)
	}
	if got := eff.Providers[0].BaseURL; got != "https://api.siliconflow.cn/v1" {
		t.Errorf("trailing slash not trimmed: %q", got)
	}
	if !eff.Providers[0].Enabled || eff.Providers[1].Dialect != DialectOpenAI {
		t.Errorf("enabled/dialect defaults not applied: %+v", eff.Providers)
	}
	m := eff.Providers[0].Models
	if !m[0].Generate || m[0].Edit || m[1].Generate || !m[1].Edit {
		t.Errorf("model capability parsing wrong: %+v", m)
	}
	if err := eff.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

// TestEnvProvider checks that OPENAI_API_KEY synthesizes a leading provider
// that is never saved.
func TestEnvProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded, err := Load(LoadOptions{
		Flags: Overrides{ConfigPath: path},
		Getenv: envMap(map[string]string{
			"OPENAI_API_KEY": "sk-env-abcdefgh", "OPENAI_BASE_URL": "https://relay.cn/v1", "OPENAI_MODELS": "a, b",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.EnvProvider || loaded.Effective.Providers[0].Name != "env" || len(loaded.Effective.Providers[0].Models) != 2 {
		t.Fatalf("env provider missing: %+v", loaded.Effective.Providers)
	}
	if err := Save(path, loaded.Effective); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-env") {
		t.Errorf("environment secret written to disk:\n%s", data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestSaveRoundTrip checks saving and reloading keeps providers, references
// and the short model form.
func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	c := Default()
	c.Providers = []Provider{{
		Name: "p", Dialect: DialectOpenAI, BaseURL: "https://x/v1", APIKey: "${K}", Enabled: true,
		Models: []Model{{ID: "flux-dev", Generate: true}, {ID: "gpt-image-2", Generate: true, Edit: true}},
	}}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- flux-dev") {
		t.Errorf("generation-only model should be saved as a bare string:\n%s", data)
	}
	loaded, err := Load(LoadOptions{Flags: Overrides{ConfigPath: path}, Getenv: envMap(map[string]string{"K": "sk-123456789"})})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.File.Providers[0]; got.APIKey != "${K}" || len(got.Models) != 2 || !got.Models[1].Edit {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// TestValidate checks the main validation rules.
func TestValidate(t *testing.T) {
	c := Default()
	c.Server.Host = "0.0.0.0"
	c.Providers = []Provider{
		{Name: "a", Dialect: "openai", BaseURL: "ftp://x", Enabled: true},
		{Name: "a", Dialect: "weird", BaseURL: "https://y/v1", Enabled: true, Models: []Model{{ID: "m"}}},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"auth_token", "base_url", "重复", "dialect", "至少要支持"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

// TestMaxConcurrency checks request.max_concurrency defaults to 4, that a
// missing or zero value is filled in by Normalize, and that out-of-range
// values are rejected by Validate.
func TestMaxConcurrency(t *testing.T) {
	if got := Default().Request.MaxConcurrency; got != 4 {
		t.Fatalf("default max_concurrency = %d, want 4", got)
	}
	c := Default()
	c.Request.MaxConcurrency = 0
	Normalize(&c)
	if c.Request.MaxConcurrency != 4 {
		t.Errorf("Normalize left max_concurrency = %d", c.Request.MaxConcurrency)
	}
	c.Request.MaxConcurrency = MaxConcurrencyLimit + 1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_concurrency") {
		t.Errorf("over-limit max_concurrency not rejected: %v", err)
	}
	c.Request.MaxConcurrency = MaxConcurrencyLimit
	if err := c.Validate(); err != nil {
		t.Errorf("max_concurrency at the limit rejected: %v", err)
	}
}

// TestMaxWait checks request.max_wait defaults to 50s, that 0 (wait until
// done) is accepted and kept, and that values below 5s or above an hour are
// rejected.
func TestMaxWait(t *testing.T) {
	c := Default()
	if got := c.Request.MaxWait.Std(); got != 50*time.Second {
		t.Fatalf("default max_wait = %v, want 50s", got)
	}
	c.Request.MaxWait = 0
	Normalize(&c)
	if c.Request.MaxWait != 0 {
		t.Errorf("Normalize changed max_wait 0 to %v", c.Request.MaxWait.Std())
	}
	if err := c.Validate(); err != nil {
		t.Errorf("max_wait 0 rejected: %v", err)
	}
	for _, bad := range []time.Duration{2 * time.Second, 2 * time.Hour, -time.Second} {
		c.Request.MaxWait = Duration(bad)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_wait") {
			t.Errorf("max_wait %v not rejected: %v", bad, err)
		}
	}
}

// TestMask checks secret masking.
func TestMask(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"short":               "****",
		"sk-abcdefghijk1234":  "sk-****1234",
		"${OPENAI_API_KEY}":   "${OPENAI_API_KEY}",
		"plainsecretvalue987": "****e987",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExampleConfig checks config.example.yaml parses and validates.
func TestExampleConfig(t *testing.T) {
	loaded, err := Load(LoadOptions{
		Flags:  Overrides{ConfigPath: "../../config.example.yaml"},
		Getenv: envMap(map[string]string{"SILICONFLOW_API_KEY": "sk-sf-1234567890"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Exists || len(loaded.Effective.Providers) != 2 {
		t.Fatalf("example not loaded: %+v", loaded.Effective.Providers)
	}
	if err := loaded.Effective.Validate(); err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if got := loaded.Effective.Providers[1].Models[1].Extra["num_inference_steps"]; got != 28 {
		t.Errorf("model extra not parsed: %v", got)
	}
	if got := loaded.Effective.Request.MaxConcurrency; got != 4 {
		t.Errorf("example max_concurrency = %d, want 4", got)
	}
	if got := loaded.Effective.Request.MaxWait.Std(); got != 50*time.Second {
		t.Errorf("example max_wait = %v, want 50s", got)
	}
}
