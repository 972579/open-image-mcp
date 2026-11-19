package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/engine"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/storage"
	"github.com/972579/open-image-mcp/internal/testutil"
)

// testEngine returns an engine over one fake provider that serves
// gpt-image-2 (generate+edit) and flux-dev (generate only).
func testEngine(t *testing.T, u *testutil.Upstream) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Defaults.Model = "gpt-image-2"
	cfg.Defaults.OutputDir = filepath.Join(dir, "out")
	cfg.Providers = []config.Provider{{
		Name: "fake", Dialect: "openai", BaseURL: u.BaseURL(), APIKey: "sk-fake-1234567890", Enabled: true,
		Models: []config.Model{{ID: "gpt-image-2", Generate: true, Edit: true}, {ID: "flux-dev", Generate: true}},
	}}
	config.Normalize(&cfg)
	return engine.New(&cfg, imageapi.New(nil), storage.New(filepath.Join(dir, "history.jsonl")), nil)
}

// connectInMemory connects an MCP client to a server built for eng.
func connectInMemory(t *testing.T, eng *engine.Engine) *mcp.ClientSession {
	t.Helper()
	srv, err := NewServer(eng, nil)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// TestToolsInMemory exercises all four tools through the MCP protocol.
func TestToolsInMemory(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	eng := testEngine(t, u)
	cs := connectInMemory(t, eng)
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"generate_image", "edit_image", "list_models", "list_generated_images"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}

	// list_models
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_models", Arguments: map[string]any{"capability": "edit"}})
	if err != nil || res.IsError {
		t.Fatalf("list_models: %v %+v", err, res)
	}
	var lm ListModelsOutput
	decode(t, res.StructuredContent, &lm)
	if len(lm.Models) != 1 || lm.Models[0].ID != "gpt-image-2" || !lm.Models[0].IsDefault {
		t.Errorf("list_models edit filter wrong: %+v", lm)
	}

	// generate_image
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "画一张赛博朋克风格的雨夜街道", "size": "1024x1024"}})
	if err != nil || res.IsError {
		t.Fatalf("generate_image: %v %+v", err, res.Content)
	}
	var gen ImageResult
	decode(t, res.StructuredContent, &gen)
	if len(gen.Images) != 1 || !filepath.IsAbs(gen.Images[0].Path) {
		t.Fatalf("generate result: %+v", gen)
	}
	if _, err := os.Stat(gen.Images[0].Path); err != nil {
		t.Errorf("image not on disk: %v", err)
	}
	var sawText, sawImage bool
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			sawText = strings.Contains(c.Text, gen.Images[0].Path)
		case *mcp.ImageContent:
			sawImage = c.MIMEType == "image/jpeg" && len(c.Data) > 0
		}
	}
	if !sawText || !sawImage {
		t.Errorf("content missing path text (%v) or preview (%v)", sawText, sawImage)
	}

	// edit_image using the generated path
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_image", Arguments: map[string]any{"prompt": "改成白天", "images": []string{gen.Images[0].Path}, "include_preview": false}})
	if err != nil || res.IsError {
		t.Fatalf("edit_image: %v %+v", err, res.Content)
	}
	if last := u.Requests()[len(u.Requests())-1]; last.Path != "/v1/images/edits" || len(last.Files["image"]) != 1 {
		t.Errorf("edit request wrong: %+v", last)
	}
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			t.Error("include_preview=false should omit previews")
		}
	}

	// edit with a generation-only model is a tool error, not a protocol error
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_image", Arguments: map[string]any{"prompt": "x", "images": []string{gen.Images[0].Path}, "model": "flux-dev"}})
	if err != nil || !res.IsError {
		t.Fatalf("want tool error, got %v %+v", err, res)
	}

	// invalid enum is rejected by schema validation
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "x", "quality": "ultra"}})
	if err != nil || !res.IsError {
		t.Errorf("invalid quality should be a tool error: %v %+v", err, res)
	}

	// list_generated_images
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_generated_images", Arguments: map[string]any{"limit": 10}})
	if err != nil || res.IsError {
		t.Fatalf("list_generated_images: %v %+v", err, res)
	}
	var li ListImagesOutput
	decode(t, res.StructuredContent, &li)
	if li.Total != 2 || !li.Items[0].Exists || li.Items[0].Kind != "edit" {
		t.Errorf("history: %+v", li)
	}

	// generate_image into its own output_dir also keeps a gallery copy, and the
	// history lists the gallery copy with the requested file beside it.
	custom := filepath.Join(t.TempDir(), "assets")
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "项目里的图标", "output_dir": custom, "include_preview": false}})
	if err != nil || res.IsError {
		t.Fatalf("generate_image with output_dir: %v %+v", err, res.Content)
	}
	decode(t, res.StructuredContent, &gen)
	img := gen.Images[0]
	if !strings.HasPrefix(img.Path, custom) || img.GalleryPath == "" || strings.HasPrefix(img.GalleryPath, custom) {
		t.Fatalf("output_dir result: %+v", img)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "图库") {
		t.Errorf("summary does not mention the gallery copy: %s", text)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_generated_images", Arguments: map[string]any{"keyword": "项目里的图标"}})
	if err != nil || res.IsError {
		t.Fatalf("list_generated_images: %v %+v", err, res)
	}
	decode(t, res.StructuredContent, &li)
	if li.Total != 1 || li.Items[0].Path != img.GalleryPath || li.Items[0].OutputPath != img.Path {
		t.Errorf("history for output_dir image: %+v", li)
	}
}

// TestManagerHTTP checks start/stop, the token check, a real Streamable HTTP
// client session, port conflicts and port release.
func TestManagerHTTP(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	eng := testEngine(t, u)
	port := freePort(t)
	sc := config.ServerConfig{Host: "127.0.0.1", Port: port, Path: "/mcp", AuthToken: "gim_testtoken"}

	m := NewManager(eng, nil)
	if err := m.Start(sc); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(sc); err == nil {
		t.Error("second Start should fail")
	}
	st := m.Status()
	if st.State != StateRunning || st.URL != "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp" {
		t.Fatalf("status: %+v", st)
	}

	// health check needs no token
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()

	// wrong token
	resp, err = http.Post(st.URL, "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token should be 401: %v %v", err, resp)
	}
	resp.Body.Close()

	// real client with token
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	transport := &mcp.StreamableClientTransport{Endpoint: st.URL, HTTPClient: &http.Client{Transport: bearer{"gim_testtoken"}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "hello"}})
	if err != nil || res.IsError {
		t.Fatalf("generate over HTTP: %v %+v", err, res)
	}
	cs.Close()
	if st := m.Status(); st.ToolCalls != 1 || st.Requests == 0 || st.Rejected != 1 {
		t.Errorf("counters: %+v", st)
	}

	// a second manager on the same port reports the conflict clearly
	other := NewManager(eng, nil)
	if err := other.Start(sc); err == nil || !strings.Contains(err.Error(), "已被占用") {
		t.Errorf("want port-in-use error, got %v", err)
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != StateStopped {
		t.Errorf("state after stop: %s", m.Status().State)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	ln.Close()
}

// TestNonLoopbackNeedsToken checks Start refuses 0.0.0.0 without a token.
func TestNonLoopbackNeedsToken(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	m := NewManager(testEngine(t, u), nil)
	if err := m.Start(config.ServerConfig{Host: "0.0.0.0", Port: freePort(t), Path: "/mcp"}); err == nil {
		t.Fatal("expected refusal")
	}
}

// bearer adds an Authorization header to every request.
type bearer struct{ token string }

// RoundTrip sets the header and delegates to the default transport.
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// freePort returns a currently unused TCP port on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// decode re-marshals structured content into out.
func decode(t *testing.T, v any, out any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}
