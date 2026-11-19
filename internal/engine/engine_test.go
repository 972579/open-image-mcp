package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/storage"
	"github.com/972579/open-image-mcp/internal/testutil"
)

// newEngine builds an engine over the given providers with instant retries,
// saving into a temp directory.
func newEngine(t *testing.T, providers ...config.Provider) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Defaults.OutputDir = filepath.Join(dir, "out")
	cfg.Providers = providers
	cfg.Request.MaxRetries = 1
	config.Normalize(&cfg)
	e := New(&cfg, imageapi.New(nil), storage.New(filepath.Join(dir, "history.jsonl")), nil)
	e.sleep = func(context.Context, time.Duration) error { return nil }
	return e, dir
}

// prov returns a provider pointing at u that serves models.
func prov(name string, u *testutil.Upstream, dialect string, models ...config.Model) config.Provider {
	return config.Provider{Name: name, Dialect: dialect, BaseURL: u.BaseURL(), APIKey: "sk-" + name + "-secret-key", Enabled: true, Models: models}
}

var genModel = config.Model{ID: "gpt-image-2", Generate: true, Edit: true}

// TestGenerateSavesAndRecords checks files land on disk and in history.
func TestGenerateSavesAndRecords(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(int, testutil.Request) (int, any) { return 200, testutil.B64Response(2) }
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	var msgs []string
	res, err := e.Generate(context.Background(), GenerateRequest{Prompt: "赛博朋克雨夜街道", N: 2, FilenamePrefix: "my/../prefix"}, func(m string) { msgs = append(msgs, m) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 || res.Provider != "a" || res.Model != "gpt-image-2" {
		t.Fatalf("unexpected result: %+v", res)
	}
	for _, img := range res.Images {
		if !filepath.IsAbs(img.Path) || img.Width != 32 || img.Height != 24 || img.MIME != "image/png" {
			t.Errorf("bad saved image: %+v", img)
		}
		if _, err := os.Stat(img.Path); err != nil {
			t.Errorf("file missing: %v", err)
		}
		if strings.Contains(filepath.Base(img.Path), "/") || strings.Contains(filepath.Base(img.Path), "..") {
			t.Errorf("prefix not sanitized: %s", img.Path)
		}
	}
	total, recs, err := e.History(storage.Query{Keyword: "赛博朋克"})
	if err != nil || total != 2 || recs[0].Provider != "a" || recs[0].Kind != "generate" {
		t.Fatalf("history = %d %+v %v", total, recs, err)
	}
	if len(msgs) == 0 {
		t.Error("no progress messages")
	}
}

// TestOutputDirKeepsGalleryCopy checks an MCP call with its own output_dir
// saves there and also keeps a copy in the gallery directory, that history
// points at the gallery copy, and that deleting from the gallery removes only
// the gallery copy and the record.
func TestOutputDirKeepsGalleryCopy(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(int, testutil.Request) (int, any) { return 200, testutil.B64Response(1) }
	e, dir := newEngine(t, prov("a", u, "openai", genModel))
	custom := filepath.Join(dir, "project", "assets")

	res, err := e.Generate(context.Background(), GenerateRequest{Prompt: "海报", OutputDir: custom}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img := res.Images[0]
	if !strings.HasPrefix(img.Path, custom+string(filepath.Separator)) {
		t.Fatalf("output path %s not under %s", img.Path, custom)
	}
	gallery := filepath.Join(dir, "out")
	if !strings.HasPrefix(img.GalleryPath, gallery+string(filepath.Separator)) || filepath.Base(img.GalleryPath) != filepath.Base(img.Path) {
		t.Fatalf("gallery copy = %q", img.GalleryPath)
	}
	a, errA := os.ReadFile(img.Path)
	b, errB := os.ReadFile(img.GalleryPath)
	if errA != nil || errB != nil || string(a) != string(b) {
		t.Fatalf("copies differ: %v %v", errA, errB)
	}
	_, recs, err := e.History(storage.Query{})
	if err != nil || len(recs) != 1 || recs[0].Path != img.GalleryPath || recs[0].OutputPath != img.Path {
		t.Fatalf("history = %+v, %v", recs, err)
	}

	if _, err := e.DeleteImage(recs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(img.GalleryPath); !os.IsNotExist(err) {
		t.Errorf("gallery copy still there: %v", err)
	}
	if _, err := os.Stat(img.Path); err != nil {
		t.Errorf("output copy must be kept: %v", err)
	}
	if total, _, _ := e.History(storage.Query{}); total != 0 {
		t.Errorf("history still has %d records", total)
	}
	if _, err := e.DeleteImage(recs[0].ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

// TestDefaultDirHasNoCopy checks a call without output_dir saves only into
// the gallery directory.
func TestDefaultDirHasNoCopy(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(int, testutil.Request) (int, any) { return 200, testutil.B64Response(1) }
	e, dir := newEngine(t, prov("a", u, "openai", genModel))
	res, err := e.Generate(context.Background(), GenerateRequest{Prompt: "猫"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if img := res.Images[0]; img.GalleryPath != "" || !strings.HasPrefix(img.Path, filepath.Join(dir, "out")) {
		t.Fatalf("image = %+v", img)
	}
	_, recs, _ := e.History(storage.Query{})
	if len(recs) != 1 || recs[0].OutputPath != "" || recs[0].Path != res.Images[0].Path {
		t.Fatalf("history = %+v", recs)
	}
}

// TestRetryThenSuccess checks a 500 is retried on the same provider.
func TestRetryThenSuccess(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(n int, _ testutil.Request) (int, any) {
		if n == 1 {
			return 502, map[string]any{"error": map[string]any{"message": "bad gateway"}}
		}
		return 200, testutil.B64Response(1)
	}
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	res, err := e.Generate(context.Background(), GenerateRequest{Prompt: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Error == "" {
		t.Errorf("attempts = %+v", res.Attempts)
	}
}

// TestFallbackToNextProvider checks a model error on A falls back to B.
func TestFallbackToNextProvider(t *testing.T) {
	a, b := testutil.NewUpstream(), testutil.NewUpstream()
	defer a.Close()
	defer b.Close()
	a.Handler = func(int, testutil.Request) (int, any) {
		return 503, map[string]any{"error": map[string]any{"message": "当前分组下对于模型 gpt-image-2 无可用渠道"}}
	}
	e, _ := newEngine(t, prov("a", a, "openai", genModel), prov("b", b, "openai", genModel))
	res, err := e.Generate(context.Background(), GenerateRequest{Prompt: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "b" || len(a.Requests()) != 1 {
		t.Errorf("provider=%s, requests to a=%d (model errors should not be retried)", res.Provider, len(a.Requests()))
	}
}

// TestNoFallbackOnAuth checks an auth failure is reported directly.
func TestNoFallbackOnAuth(t *testing.T) {
	a, b := testutil.NewUpstream(), testutil.NewUpstream()
	defer a.Close()
	defer b.Close()
	a.Handler = func(int, testutil.Request) (int, any) {
		return 401, map[string]any{"error": map[string]any{"message": "invalid key"}}
	}
	e, _ := newEngine(t, prov("a", a, "openai", genModel), prov("b", b, "openai", genModel))
	_, err := e.Generate(context.Background(), GenerateRequest{Prompt: "x"}, nil)
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want *Failure, got %v", err)
	}
	if apiErr, _ := imageapi.AsError(err); apiErr == nil || apiErr.Kind != imageapi.KindAuth {
		t.Errorf("want KindAuth, got %v", err)
	}
	if len(b.Requests()) != 0 {
		t.Error("auth errors must not fall back")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Errorf("key leaked: %v", err)
	}
}

// TestURLDownloadAndSiliconFlowEdit checks URL results are downloaded and the
// siliconflow edit sends a data URL.
func TestURLDownloadAndSiliconFlowEdit(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(int, testutil.Request) (int, any) {
		return 200, map[string]any{"images": []any{map[string]any{"url": u.URL + "/files/x.png"}}}
	}
	e, dir := newEngine(t, prov("sf", u, "siliconflow", config.Model{ID: "Qwen/Qwen-Image-Edit", Edit: true}))
	ref := filepath.Join(dir, "ref.png")
	if err := os.WriteFile(ref, testutil.PNG(8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Edit(context.Background(), EditRequest{GenerateRequest: GenerateRequest{Prompt: "edit", Model: "Qwen/Qwen-Image-Edit"}, Images: []string{ref}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Images[0].Width != 16 || res.Images[0].SourceURL == "" {
		t.Errorf("download not saved: %+v", res.Images[0])
	}
	img, _ := u.Requests()[0].JSON["image"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("siliconflow edit should send a data URL, got %.40q", img)
	}
}

// TestEditCapabilityCheck checks a generation-only model is refused for edit.
func TestEditCapabilityCheck(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	e, dir := newEngine(t, prov("a", u, "openai", config.Model{ID: "flux-dev", Generate: true}))
	ref := filepath.Join(dir, "ref.png")
	_ = os.WriteFile(ref, testutil.PNG(4, 4), 0o644)
	_, err := e.Edit(context.Background(), EditRequest{GenerateRequest: GenerateRequest{Prompt: "x", Model: "flux-dev"}, Images: []string{ref}}, nil)
	if err == nil || !strings.Contains(err.Error(), "未声明支持编辑") {
		t.Fatalf("want capability error, got %v", err)
	}
	if len(u.Requests()) != 0 {
		t.Error("no request should be sent")
	}
}

// TestValidation checks request validation errors.
func TestValidation(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	for _, req := range []GenerateRequest{{Prompt: " "}, {Prompt: "x", N: 11}, {Prompt: "x", OutputDir: "relative/dir"}, {Prompt: "x", Model: "nope"}} {
		if _, err := e.Generate(context.Background(), req, nil); err == nil {
			t.Errorf("expected error for %+v", req)
		}
	}
}
