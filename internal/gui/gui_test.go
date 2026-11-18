package gui

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vivaui/viva/app"

	"github.com/972579/open-image-mcp/internal/appcore"
	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/mcpserver"
	"github.com/972579/open-image-mcp/internal/testutil"
	"github.com/972579/open-image-mcp/internal/update"
)

// openApp builds a Core over a temporary HOME whose config has one fake
// provider, and drives the real page headlessly.
func openApp(t *testing.T) (*model, *app.Driver, *testutil.Upstream) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	u := testutil.NewUpstream()
	t.Cleanup(u.Close)

	port := freePort(t)
	cfg := config.Default()
	cfg.Server.Port = port
	cfg.Defaults.Model = "gpt-image-2"
	cfg.Defaults.OutputDir = filepath.Join(home, "out")
	cfg.Providers = []config.Provider{{
		Name: "fake", Dialect: config.DialectOpenAI, BaseURL: u.BaseURL(), APIKey: "sk-fake-abcdefgh1234", Enabled: true,
		Models: []config.Model{{ID: "gpt-image-2", Generate: true, Edit: true}},
	}}
	if err := config.Save(config.DefaultConfigPath(), cfg); err != nil {
		t.Fatal(err)
	}
	core, err := appcore.Open(config.LoadOptions{Getenv: func(string) string { return "" }}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	m := newModel(core)
	d, err := app.NewDriver(m.windowConfig(false), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	// A tall window keeps every control of a page on screen, so tests can
	// click controls below the first screenful without scrolling.
	if err := d.Resize(1400, 3200); err != nil {
		t.Fatal(err)
	}
	return m, d, u
}

// click presses the control whose accessible label is exactly name.
func click(t *testing.T, d *app.Driver, name string) {
	t.Helper()
	for _, hit := range d.Snapshot().Hits {
		if hit.Label == name {
			b := hit.Bounds
			if err := d.Click(b.X+b.Width/2, b.Y+b.Height/2); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	var labels []string
	for _, hit := range d.Snapshot().Hits {
		labels = append(labels, hit.Label)
	}
	t.Fatalf("no control named %q; controls: %q", name, labels)
}

// shows reports whether any painted text contains want.
func shows(d *app.Driver, want string) bool {
	for _, l := range d.Snapshot().Labels {
		if strings.Contains(l.Value, want) {
			return true
		}
	}
	return false
}

// waitFor pumps background wakes until want is painted or 10s pass.
func waitFor(t *testing.T, d *app.Driver, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !shows(d, want) {
		select {
		case <-d.Wakes():
			if err := d.Pump(); err != nil {
				t.Fatal(err)
			}
		case <-time.After(200 * time.Millisecond):
			_ = d.Pump()
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// TestDesktopFlow walks the main user path: start the server, test the
// provider, read client configs, generate in the debug console via MCP, and
// find the image in the gallery.
func TestDesktopFlow(t *testing.T) {
	m, d, u := openApp(t)

	if !shows(d, "已停止") {
		t.Fatal("server should start stopped")
	}
	click(t, d, "启动服务")
	waitFor(t, d, "运行中")
	if m.core.Manager.Status().State != mcpserver.StateRunning {
		t.Fatal("manager not running")
	}

	click(t, d, "服务商")
	if !shows(d, "fake") {
		t.Fatal("provider list missing fake")
	}
	click(t, d, "测试连接")
	waitFor(t, d, "连接成功")

	click(t, d, "客户端配置")
	if !shows(d, "mcp-remote") {
		t.Error("Claude Desktop snippet should mention mcp-remote")
	}
	click(t, d, "复制")
	if got, _ := app.ClipboardText(); got != "" && !strings.Contains(got, "mcpServers") {
		t.Errorf("clipboard = %q", got)
	}
	click(t, d, "Codex")
	if !shows(d, "[mcp_servers.gen-image]") {
		t.Error("Codex TOML snippet missing")
	}

	click(t, d, "调试台")
	click(t, d, viaMCP)
	click(t, d, "生成图片")
	waitFor(t, d, "（经由 MCP）")
	if len(u.Requests()) == 0 {
		t.Fatal("upstream received no request")
	}
	if m.pg.result == nil || len(m.pg.result.images) != 1 {
		t.Fatalf("debug result: %+v", m.pg.result)
	}
	if _, err := os.Stat(m.pg.result.images[0].Path); err != nil {
		t.Errorf("generated file missing: %v", err)
	}
	t.Logf("generated via MCP: %s (%s); upstream requests: %d", m.pg.result.images[0].Path, m.pg.result.summary, len(u.Requests()))

	click(t, d, "图库")
	waitFor(t, d, "共 1 张")

	click(t, d, "服务")
	click(t, d, "停止服务")
	waitFor(t, d, "已停止")
}

// TestSettingsSave checks editing a setting writes the file and takes effect.
func TestSettingsSave(t *testing.T) {
	m, d, _ := openApp(t)
	click(t, d, "设置")
	m.settings.size = "1536x1024"
	click(t, d, "保存")
	if got := m.core.Loaded().Effective.Defaults.Size; got != "1536x1024" {
		t.Fatalf("effective size = %q", got)
	}
	if got := m.core.Engine.Config().Defaults.Size; got != "1536x1024" {
		t.Fatalf("engine not reloaded: %q", got)
	}
	data, _ := os.ReadFile(config.DefaultConfigPath())
	if !strings.Contains(string(data), "1536x1024") {
		t.Errorf("not saved:\n%s", data)
	}
}

// freePort returns an unused TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestUpdateCheck checks the about page finds a newer release from a fake
// GitHub API and offers the download for this platform.
func TestUpdateCheck(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := fmt.Sprintf("genimage-9.9.9-%s-%s.zip", runtime.GOOS, runtime.GOARCH)
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","name":"9.9.9","body":"- 新功能 A\n- 修复 B","html_url":"https://github.com/o/r/releases/tag/v9.9.9","published_at":"2026-12-01T00:00:00Z",
		"assets":[{"name":%q,"browser_download_url":"https://github.com/o/r/releases/download/v9.9.9/%s","size":1048576}]}`, asset, asset)
	}))
	defer gh.Close()
	old := update.APIBase
	update.APIBase = gh.URL
	defer func() { update.APIBase = old }()

	_, d, _ := openApp(t)
	click(t, d, "关于")
	click(t, d, "检查更新")
	waitFor(t, d, "新版本 v9.9.9")
	if !shows(d, "新功能 A") {
		t.Error("release notes not shown")
	}
	if !shows(d, "有新版本 v9.9.9") {
		t.Error("sidebar badge missing")
	}
	click(t, d, "打开发布页")
}
