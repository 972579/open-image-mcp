package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivaui/viva/app"

	"github.com/972579/open-image-mcp/internal/appcore"
	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/engine"
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

// TestGalleryPreviewAndDelete generates one image the way an MCP call with its
// own output_dir does, then in the gallery: clicking the thumbnail opens the
// preview with the whole prompt, and deleting (after confirming) removes the
// gallery copy and the record but keeps the file in output_dir.
func TestGalleryPreviewAndDelete(t *testing.T) {
	m, d, _ := openApp(t)
	prompt := strings.Repeat("一只戴着围巾的橘猫坐在雪地里，背景是黄昏的小镇和远处的灯光，", 4) + "结尾标记XYZ"
	outDir := filepath.Join(t.TempDir(), "project")
	res, err := m.core.Engine.Generate(context.Background(), engine.GenerateRequest{Prompt: prompt, OutputDir: outDir, Source: "mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img := res.Images[0]
	if img.GalleryPath == "" {
		t.Fatal("no gallery copy kept")
	}

	click(t, d, "图库")
	waitFor(t, d, "共 1 张")
	click(t, d, "预览 "+filepath.Base(img.GalleryPath))
	waitFor(t, d, "结尾标记XYZ") // the card shows two lines; only the preview shows the end
	if !shows(d, "指定保存路径") || !shows(d, "复制提示词") {
		t.Error("preview misses the output path or the copy-prompt button")
	}

	click(t, d, "删除")
	waitFor(t, d, "删除这张图片？")
	click(t, d, "删除图片")
	waitFor(t, d, "还没有图片")
	if _, err := os.Stat(img.GalleryPath); !os.IsNotExist(err) {
		t.Errorf("gallery copy still on disk: %v", err)
	}
	if _, err := os.Stat(img.Path); err != nil {
		t.Errorf("output_dir copy was deleted: %v", err)
	}
}

// TestUpdateCheck walks the in-app update against a fake signed feed and a
// fake GitHub API: the about page finds the newer release with its notes,
// downloads the archive the feed lists, and hands it to the installer. The
// first install fails, which must leave the app running with the error, a
// retry and the browser fallback; the retry installs the same verified file.
func TestUpdateCheck(t *testing.T) {
	archive := bytes.Repeat([]byte("new version "), 30000)
	feed := testutil.NewFeed(update.AppID, "9.9.9", archive)
	defer feed.Close()
	oldURL, oldKeys, oldClient := update.FeedURL, update.PublicKeys, update.HTTPClient
	update.FeedURL, update.PublicKeys, update.HTTPClient = feed.URL(), []string{feed.PublicKey}, feed.Client()
	defer func() { update.FeedURL, update.PublicKeys, update.HTTPClient = oldURL, oldKeys, oldClient }()

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v9.9.9","name":"9.9.9","body":"- 新功能 A\n- 修复 B","html_url":"https://github.com/o/r/releases/tag/v9.9.9","published_at":"2026-12-01T00:00:00Z"}`)
	}))
	defer gh.Close()
	oldAPI := update.APIBase
	update.APIBase = gh.URL
	defer func() { update.APIBase = oldAPI }()

	m, d, _ := openApp(t)
	var mu sync.Mutex
	var installed []string
	m.install = func(artifact string, u *update.Update, quit func()) error {
		mu.Lock()
		defer mu.Unlock()
		installed = append(installed, artifact)
		if len(installed) == 1 {
			return errors.New("无法替换当前应用（测试）")
		}
		return nil
	}

	click(t, d, "关于")
	click(t, d, "检查更新")
	waitFor(t, d, "新版本 v9.9.9")
	if !shows(d, "新功能 A") {
		t.Error("release notes not shown")
	}
	if !shows(d, "有新版本 v9.9.9") {
		t.Error("sidebar badge missing")
	}

	click(t, d, updateButtonLabel(m.upd.avail, false))
	waitFor(t, d, "无法替换当前应用（测试）")
	click(t, d, "在浏览器中下载") // a non-GitHub test URL: blocked with a toast
	click(t, d, "重试更新")
	waitFor(t, d, "正在安装 v9.9.9")
	// The page says 正在安装 before the background goroutine calls the
	// installer, so wait for the call itself.
	for deadline := time.Now().Add(10 * time.Second); ; {
		mu.Lock()
		n := len(installed)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("installer called %d times, want 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(installed) != 2 || installed[0] != installed[1] {
		t.Fatalf("installed = %q", installed)
	}
	got, err := os.ReadFile(installed[1])
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("installed archive: %d bytes, err %v", len(got), err)
	}
	if !strings.HasPrefix(installed[1], filepath.Join(config.HomeDir(), "updates", "9.9.9")) {
		t.Errorf("archive downloaded to %s", installed[1])
	}
	if feed.Downloads() != 1 {
		t.Errorf("archive requested %d times; the retry should reuse the verified file", feed.Downloads())
	}
}

// TestCloseButton checks the close button hides the window on macOS-style
// runs, and asks before quitting elsewhere while the server runs.
func TestCloseButton(t *testing.T) {
	m, d, _ := openApp(t)

	// macOS: hide, never close.
	hidden, allowed := false, false
	m.closeHides, m.hide = true, func() { hidden = true }
	m.onCloseRequest(func() { allowed = true })
	if !hidden || allowed {
		t.Fatalf("mac close: hidden=%v allowed=%v, want hidden only", hidden, allowed)
	}

	// Windows-style, server stopped: close at once.
	m.closeHides, allowed = false, false
	m.onCloseRequest(func() { allowed = true })
	if !allowed {
		t.Fatal("close with server stopped should allow at once")
	}

	// Windows-style, server running: ask first.
	click(t, d, "启动服务")
	waitFor(t, d, "运行中")
	allowed = false
	m.onCloseRequest(func() { allowed = true })
	if allowed || !m.closeOpen {
		t.Fatalf("close with server running should ask: allowed=%v open=%v", allowed, m.closeOpen)
	}
	if err := d.Paint(); err != nil {
		t.Fatal(err)
	}
	if !shows(d, "退出 GenImage MCP？") {
		t.Error("confirmation dialog not shown")
	}
}
