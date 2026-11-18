// Package gui is the viva desktop window: server control, provider and
// settings forms, client config snippets, a debug console and a gallery.
//
// Threading follows viva's rules: the page function and every model field run
// on the window thread. Background work (server stop, connection tests, image
// calls, thumbnail decoding) never touches the model directly; it hands a
// closure to post, which queues it and wakes the window, and OnWake runs the
// queue on the window thread before the next paint.
package gui

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/appcore"
	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/mcpserver"
)

// Page names, also the sidebar labels.
const (
	tabServer     = "服务"
	tabProviders  = "服务商"
	tabSettings   = "设置"
	tabClients    = "客户端配置"
	tabPlayground = "调试台"
	tabGallery    = "图库"
	tabAbout      = "关于"
)

var tabs = []string{tabServer, tabProviders, tabSettings, tabClients, tabPlayground, tabGallery, tabAbout}

// model is the whole window state. Only the window thread reads or writes it.
type model struct {
	core *appcore.Core
	inv  *app.Invalidator

	inboxMu sync.Mutex
	inbox   []func()

	toaster ui.Toaster
	tab     string

	// server page
	status     mcpserver.Status
	logs       []string
	logVersion uint64
	showToken  bool
	stopping   bool

	// configuration draft shared by the providers and settings pages
	draft      config.Config
	provText   []providerText
	provSel    int
	settings   settingsForm
	cfgVersion int

	// sub-page state
	clients clientsPage
	pg      playground
	gal     gallery
	thumbs  *thumbCache
	imp     importDialog
	upd     updateState

	confirmDeleteOpen bool
	closeOpen         bool
	closeAllow        func()

	// closeHides makes the window's close button hide the window instead of
	// quitting (macOS convention); hide does it. Run sets both on macOS.
	closeHides bool
	hide       func()
}

// Run opens the window and blocks until the application quits. When autostart
// is set the MCP server is started before the window appears.
//
// On macOS the app behaves like other Mac apps: the window's close button (or
// Cmd+W) only hides the window while the app — and the MCP server — keep
// running; clicking the Dock icon or opening the app again shows the window;
// Quit from the Dock menu or the app menu (Cmd+Q) stops the server and exits.
// On Windows closing the window quits, after a confirmation while the server
// runs.
//
// core supplies configuration and services; the caller closes it afterwards.
func Run(core *appcore.Core) error {
	m := newModel(core)
	m.autostart()
	if core.Loaded().Effective.Update.CheckOnStartup {
		m.checkUpdate(false)
	}
	m.closeHides = runtime.GOOS == "darwin"
	return app.RunApplication(func(a *app.Application) error {
		canvas := new(gfx.Canvas)
		win, err := a.OpenWindow(m.windowConfig(true), canvas)
		if err != nil {
			return err
		}
		m.hide = win.Hide
		a.OnReopen(win.Show)
		a.OnQuitRequest(func(quit func()) { m.quitApp(a, quit) })
		return nil
	})
}

// quitApp handles Quit from the Dock or the app menu: it stops the MCP server
// gracefully in the background (giving in-flight requests up to three
// seconds), then quits on the event-loop thread. quit closes every window
// without consulting their close handlers, so a hidden window does not block it.
//
// a is the running application; quit is the allow callback from OnQuitRequest.
func (m *model) quitApp(a *app.Application, quit func()) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.core.Manager.Stop(ctx); err != nil {
			m.core.Log.Warn("stop server on quit", "error", err.Error())
		}
		a.Post(quit)
	}()
}

// newModel builds the window state over core and subscribes to server and log
// changes so background events repaint the window.
func newModel(core *appcore.Core) *model {
	m := &model{core: core, inv: new(app.Invalidator), tab: tabServer}
	m.thumbs = newThumbCache(m.post)
	m.resetDraft()
	m.pg.init()
	m.clients.tab = "Claude Desktop"
	core.Manager.OnChange(m.inv.Invalidate)
	core.Ring.OnAppend(m.inv.Invalidate)
	m.refreshStatus()
	return m
}

// autostart starts the MCP server when server.autostart is set, showing a
// toast if it fails (for example when the port is taken).
func (m *model) autostart() {
	if !m.core.Loaded().Effective.Server.Autostart {
		return
	}
	if err := m.core.StartServer(); err != nil {
		m.toaster.Show("MCP 服务自动启动失败", err.Error(), ui.ToastError)
	}
	m.refreshStatus()
}

// windowConfig returns the native window configuration: the page, size
// limits, and the wake, close and file-drop hooks. Tests pass it to
// app.NewDriver to run the real page without a window.
//
// followSystem makes the page track macOS light/dark mode. Tests pass false:
// watching the system appearance needs the main thread's run loop, which a
// test goroutine does not have.
func (m *model) windowConfig(followSystem bool) app.Config {
	page := ui.NewPage(ui.PageConfig{View: m.page, FollowSystemTheme: followSystem, Invalidator: m.inv})
	cfg := page.Config(app.Config{
		Title: "GenImage MCP", Width: 1180, Height: 800, MinWidth: 980, MinHeight: 640,
		Invalidator: m.inv,
	})
	cfg.OnWake = m.wake
	cfg.OnCloseRequest = m.onCloseRequest
	cfg.OnFileDrop = m.onFileDrop
	return cfg
}

// post queues fn to run on the window thread and wakes the window. It is the
// only way background goroutines may change the model. Safe from any goroutine.
func (m *model) post(fn func()) {
	m.inboxMu.Lock()
	m.inbox = append(m.inbox, fn)
	m.inboxMu.Unlock()
	m.inv.Invalidate()
}

// wake runs on the window thread before each repaint an Invalidate asked for:
// it drains the inbox and copies the server status and new log lines.
func (m *model) wake() {
	m.inboxMu.Lock()
	queue := m.inbox
	m.inbox = nil
	m.inboxMu.Unlock()
	for _, fn := range queue {
		fn()
	}
	m.refreshStatus()
}

// refreshStatus copies the manager status and, when the log ring changed, its
// lines into the model.
func (m *model) refreshStatus() {
	m.status = m.core.Manager.Status()
	if v := m.core.Ring.Version(); v != m.logVersion {
		m.logs, m.logVersion = m.core.Ring.Snapshot()
	}
}

// resetDraft discards unsaved edits and reloads the draft from the current
// file layer, rebuilding the per-provider text fields and settings strings.
func (m *model) resetDraft() {
	m.draft = m.core.Loaded().File.Clone()
	m.provText = make([]providerText, len(m.draft.Providers))
	for i := range m.draft.Providers {
		m.provText[i] = newProviderText(&m.draft.Providers[i])
	}
	if m.provSel >= len(m.draft.Providers) {
		m.provSel = len(m.draft.Providers) - 1
	}
	if m.provSel < 0 {
		m.provSel = 0
	}
	m.settings = newSettingsForm(&m.draft)
	m.cfgVersion++
}

// saveDraft parses the text fields into the draft, validates and saves it, and
// makes it effective. On success it reloads the draft from what was saved and
// shows a toast; when the running server's listen settings changed it says a
// restart is needed. It returns whether the save succeeded.
func (m *model) saveDraft() bool {
	file := m.draft.Clone()
	for i := range file.Providers {
		if err := m.provText[i].apply(&file.Providers[i]); err != nil {
			m.toaster.Show("无法保存", err.Error(), ui.ToastError)
			return false
		}
	}
	if err := m.settings.apply(&file); err != nil {
		m.toaster.Show("无法保存", err.Error(), ui.ToastError)
		return false
	}
	if err := m.core.SaveFile(file); err != nil {
		m.toaster.Show("保存失败", err.Error(), ui.ToastError)
		return false
	}
	m.resetDraft()
	if m.core.NeedsRestart() {
		m.toaster.Show("已保存", "监听地址或 Token 已变化，重启 MCP 服务后生效", ui.ToastWarning)
	} else {
		m.toaster.Show("已保存", "配置已生效", ui.ToastSuccess)
	}
	return true
}

// onCloseRequest handles the window's close button. On macOS it hides the
// window and leaves the app and the MCP server running in the background.
// Elsewhere closing quits, so it asks for confirmation while the server runs
// and closes at once otherwise.
//
// allow closes the window (and, as the only window, ends the app).
func (m *model) onCloseRequest(allow func()) {
	if m.closeHides && m.hide != nil {
		m.hide()
		return
	}
	if !m.core.Manager.Running() {
		allow()
		return
	}
	m.closeOpen, m.closeAllow = true, allow
	m.inv.Invalidate()
}

// onFileDrop accepts files dragged over the window and adds dropped images to
// the debug console's reference images, switching it to edit mode.
func (m *model) onFileDrop(ev gfx.FileDropEvent) gfx.EventResult {
	switch ev.Phase {
	case gfx.FileDropOver:
		return gfx.EventResult{Handled: true}
	case gfx.FileDropDrop:
	default:
		return gfx.EventResult{}
	}
	added := 0
	for _, p := range ev.Paths {
		if isImagePath(p) {
			m.pg.addImage(p)
			added++
		}
	}
	if added == 0 {
		m.toaster.Show("未添加", "只支持 PNG / JPEG / WebP / GIF 图片", ui.ToastWarning)
		return gfx.EventResult{Handled: true, Repaint: true}
	}
	m.tab, m.pg.mode = tabPlayground, modeEdit
	m.toaster.Show(fmt.Sprintf("已添加 %d 张参考图", added), "", ui.ToastSuccess)
	return gfx.EventResult{Handled: true, Repaint: true}
}

// page declares the whole window: sidebar, the active page, dialogs, toasts.
func (m *model) page(c ui.C) {
	theme := c.Theme()
	ui.Row(c).Fill().Children(func() {
		m.sidebar(c)
		ui.Column(c).Grow(1).FillHeight().Background(theme.Background).Children(func() {
			switch m.tab {
			case tabServer:
				m.serverPage(c)
			case tabProviders:
				m.providersPage(c)
			case tabSettings:
				m.settingsPage(c)
			case tabClients:
				m.clientsPage(c)
			case tabPlayground:
				m.playgroundPage(c)
			case tabGallery:
				m.galleryPage(c)
			case tabAbout:
				m.aboutPage(c)
			}
		})
	})
	m.closeDialog(c)
	ui.Toasts(c, &m.toaster)
}

// sidebar declares the navigation column with the server status at its foot.
func (m *model) sidebar(c ui.C) {
	theme := c.Theme()
	ui.Column(c).Width(196).Shrink(0).FillHeight().Pad(16).Gap(4).Background(theme.Card).Border(1, theme.Border).Children(func() {
		ui.Text(c, "GenImage MCP").FontSize(17).Weight(gfx.WeightSemibold)
		ui.Text(c, "OpenAI 兼容生图服务").Muted().FontSize(theme.Metrics.FontSizeS).MarginBottom(16)
		for _, t := range tabs {
			b := ui.Button(c.Key(t), t).FillWidth()
			if t == m.tab {
				b.Secondary()
			} else {
				b.Ghost()
			}
			if b.Clicked() {
				m.tab = t
				if t == tabGallery {
					m.gal.stale = true
				}
			}
		}
		ui.Spacer(c)
		m.updateBadge(c)
		ui.Divider(c).MarginBottom(8)
		ui.Row(c).Gap(8).Items(ui.Center).Children(func() {
			statusDot(c, m.status.State)
			ui.Text(c, stateLabel(m.status.State)).FontSize(theme.Metrics.FontSizeS).Weight(gfx.WeightMedium)
		})
		if m.status.URL != "" {
			ui.Text(c, strings.TrimPrefix(m.status.URL, "http://")).Muted().Mono().FontSize(11).Ellipsis()
		}
	})
}

// closeDialog is the confirmation shown when the window is closed while the
// MCP server runs: "停止并退出" stops the server in the background and then
// lets the window close.
func (m *model) closeDialog(c ui.C) {
	ui.Modal(c, &m.closeOpen, "退出 GenImage MCP？", func() {
		ui.Paragraph(c, "MCP 服务正在运行，关闭窗口会停止服务，已连接的客户端将无法继续调用。", 3).MaxWidth(380)
	}, func() {
		if ui.Button(c, "取消").Outline().Clicked() {
			m.closeOpen, m.closeAllow = false, nil
		}
		if ui.Button(c, "停止并退出").Destructive().Clicked() {
			allow := m.closeAllow
			m.closeOpen, m.closeAllow = false, nil
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				_ = m.core.Manager.Stop(ctx)
				m.post(func() {
					if allow != nil {
						allow()
					}
				})
			}()
		}
	})
}

// statusDot paints a small colored circle for a server state.
func statusDot(c ui.C, st mcpserver.State) {
	theme := c.Theme()
	col := theme.MutedForeground
	switch st {
	case mcpserver.StateRunning:
		col = theme.Success
	case mcpserver.StateError:
		col = theme.Danger
	case mcpserver.StateStopping:
		col = theme.Chart[2]
	}
	ui.Row(c).Size(8, 8).Radius(4).Background(col).Shrink(0)
}

// stateLabel names a server state in Chinese.
func stateLabel(st mcpserver.State) string {
	switch st {
	case mcpserver.StateRunning:
		return "运行中"
	case mcpserver.StateStopping:
		return "正在停止"
	case mcpserver.StateError:
		return "启动失败"
	default:
		return "已停止"
	}
}
