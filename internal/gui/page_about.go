package gui

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/update"
	"github.com/972579/open-image-mcp/internal/version"
)

// updateState is the result of the last update check and the progress of an
// in-app update. Only the window thread touches it.
type updateState struct {
	checking  bool
	checkedAt time.Time
	avail     *update.Update // newer release from the signed feed; nil when up to date or unchecked
	err       string         // last check error
	notes     []string       // release notes split into lines, cached

	downloading bool
	installing  bool
	done, total int64              // download progress in bytes
	failure     string             // last download or install error
	cancel      context.CancelFunc // cancels the running download
}

// installFunc installs a downloaded update and quits; update.Install in the
// app, a stub in tests (the real one would replace the test binary's bundle).
type installFunc func(artifact string, u *update.Update, quit func()) error

// busy reports a download or install in progress, during which the update
// check and the update button are disabled.
func (u *updateState) busy() bool { return u.downloading || u.installing }

// checkUpdate reads the signed update feed in the background and, when it
// offers a newer version, the release notes from the GitHub API (best effort:
// without them the update is still offered). A manual check reports every
// outcome as a toast; the startup check stays quiet unless a newer version
// exists, and only logs failures. Nothing is downloaded here.
//
// manual says whether the user pressed 检查更新.
func (m *model) checkUpdate(manual bool) {
	if m.upd.checking || m.upd.busy() {
		return
	}
	m.upd.checking = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		avail, err := update.Check(ctx, version.Version)
		notes := ""
		if err == nil && avail != nil {
			rel, nerr := update.Latest(ctx, nil, version.Repo, version.Version)
			switch {
			case nerr != nil:
				m.core.Log.Info("release notes unavailable", "error", nerr.Error())
			case rel.Version == avail.Version:
				notes = rel.Notes
			}
		}
		m.post(func() {
			u := &m.upd
			u.checking, u.checkedAt = false, time.Now()
			if err != nil {
				u.err = err.Error()
				m.core.Log.Warn("update check failed", "error", u.err)
				if manual {
					m.toaster.Show("检查更新失败", u.err, ui.ToastError)
				}
				return
			}
			u.err, u.avail, u.failure, u.notes = "", avail, "", nil
			// No notes (API unreachable or an empty release body) shows no block.
			if text := strings.TrimSpace(notes); text != "" {
				u.notes = splitLines(text)
				if len(u.notes) > 40 {
					u.notes = append(u.notes[:40], "…")
				}
			}
			switch {
			case avail != nil && avail.Installable():
				m.toaster.Show("发现新版本 v"+avail.Version, "在「关于」页一键更新", ui.ToastInfo)
			case avail != nil:
				m.toaster.Show("发现新版本 v"+avail.Version, "在「关于」页下载新版本", ui.ToastInfo)
			case manual:
				m.toaster.Show("已是最新版本", "当前 v"+version.Version, ui.ToastSuccess)
			}
		})
	}()
}

// updatesDir is where update archives are downloaded and resumed:
// ~/.gen-openai-image/updates, one folder per version.
func updatesDir() string { return filepath.Join(config.HomeDir(), "updates") }

// startUpdate downloads the offered release in the background, showing its
// progress on the about page and the Dock icon, then installs it: the app
// stops the MCP server and quits, and the update helper replaces the app and
// starts the new version. A failed download or install leaves the current
// version running and shows why, with a retry; a partial download is resumed
// by the retry.
func (m *model) startUpdate() {
	u := &m.upd
	avail := u.avail
	if avail == nil || !avail.Installable() || u.busy() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.downloading, u.failure, u.cancel = true, "", cancel
	u.done, u.total = 0, avail.Asset.Size
	go func() {
		defer cancel()
		var lastPost time.Time
		path, err := update.Download(ctx, avail, version.Version, updatesDir(), func(done, total int64) {
			// The callback fires for every chunk; repaint at most ten times a second.
			if now := time.Now(); now.Sub(lastPost) >= 100*time.Millisecond || done == total {
				lastPost = now
				m.post(func() {
					m.upd.done, m.upd.total = done, total
					if total > 0 {
						m.setAppProgress(float32(done) / float32(total))
					}
				})
			}
		})
		if err != nil {
			m.post(func() {
				m.upd.downloading, m.upd.cancel, m.upd.failure = false, nil, err.Error()
				m.setAppProgress(-1)
			})
			return
		}
		m.post(func() {
			m.upd.downloading, m.upd.cancel, m.upd.installing = false, nil, true
			m.setAppProgress(-1)
		})
		m.core.Log.Info("installing update", "version", avail.Version, "archive", path)
		if err := m.install(path, avail, m.quitForUpdate); err != nil {
			m.core.Log.Warn("install update failed", "error", err.Error())
			m.post(func() { m.upd.installing, m.upd.failure = false, err.Error() })
		}
		// On success quitForUpdate has run and the app is ending.
	}()
}

// cancelUpdate stops a running download; the bytes received so far stay on
// disk so the next attempt resumes them.
func (m *model) cancelUpdate() {
	if m.upd.cancel != nil {
		m.upd.cancel()
	}
}

// quitForUpdate ends the app so the update helper can replace it: it stops
// the MCP server (giving in-flight requests up to three seconds) and quits
// the event loop, closing the window whatever its close handler says. It runs
// on the install goroutine, which update.Install calls it on, and does
// nothing beyond stopping the server when no application is running (tests).
func (m *model) quitForUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.core.Manager.Stop(ctx); err != nil {
		m.core.Log.Warn("stop server for update", "error", err.Error())
	}
	if m.app != nil {
		m.app.Quit()
	}
}

// setAppProgress shows fraction (0–1) of the download on the Dock or taskbar
// icon, or removes the bar when fraction is negative. It must run on the
// window thread and does nothing in tests, which have no application.
func (m *model) setAppProgress(fraction float32) {
	if m.app != nil {
		m.app.SetProgress(fraction)
	}
}

// updateButtonLabel is the label of the button that downloads and installs
// avail, with the archive size; after a failure it offers a retry.
func updateButtonLabel(avail *update.Update, failed bool) string {
	if failed {
		return "重试更新"
	}
	return fmt.Sprintf("下载并更新（%.1f MB）", float64(avail.Asset.Size)/(1<<20))
}

// updateBadge is the sidebar notice shown while a newer release exists; it
// opens the about page.
func (m *model) updateBadge(c ui.C) {
	if m.upd.avail == nil {
		return
	}
	if ui.Button(c, "有新版本 v"+m.upd.avail.Version).Small().FillWidth().MarginBottom(8).Clicked() {
		m.tab = tabAbout
	}
}

// aboutPage shows the version, the update check and where data is stored.
func (m *model) aboutPage(c ui.C) {
	theme := c.Theme()
	u := &m.upd
	loaded := m.core.Loaded()
	scaffold(c, "关于", "", func() {
		card(c, "", func() {
			ui.Row(c).FillWidth().Gap(12).Items(ui.Center).Children(func() {
				ui.Column(c).Grow(1).Gap(4).Children(func() {
					ui.Text(c, "GenImage MCP").FontSize(18).Weight(gfx.WeightSemibold)
					ui.Text(c, fmt.Sprintf("版本 v%s · %s/%s", version.Version, runtime.GOOS, runtime.GOARCH)).Muted()
					ui.Text(c, version.RepoURL).Mono().FontSize(12).Muted()
				})
				if ui.Button(c, "GitHub 仓库").Outline().Clicked() {
					m.openURL(version.RepoURL)
				}
			})
		})

		card(c, "检查更新", func() {
			ui.Row(c).FillWidth().Gap(12).Items(ui.Center).Children(func() {
				if ui.Button(c, "检查更新").Disabled(u.checking || u.busy()).Clicked() {
					m.checkUpdate(true)
				}
				switch {
				case u.checking:
					ui.Spinner(c)
					ui.Text(c, "正在查询 GitHub Releases …").Muted()
				case u.err != "":
					ui.Paragraph(c, u.err, 3).Color(theme.Danger).Grow(1)
				case u.avail != nil:
					badge(c, toneAccent, "新版本 v"+u.avail.Version)
					if !u.avail.Date.IsZero() {
						ui.Text(c, "发布于 "+u.avail.Date.Local().Format("2006-01-02")).Muted()
					}
				case !u.checkedAt.IsZero():
					badge(c, toneSuccess, "已是最新")
					ui.Text(c, fmt.Sprintf("当前 v%s · 检查于 %s", version.Version, u.checkedAt.Format("15:04"))).Muted()
				default:
					ui.Text(c, "尚未检查").Muted()
				}
			})
			if u.avail != nil {
				m.updateOffer(c)
			}
			ui.Divider(c)
			ui.Row(c).FillWidth().Gap(16).Items(ui.Center).Children(func() {
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, "启动时自动检查更新").Weight(gfx.WeightMedium)
					ui.Text(c, "只从 github.com 读取签名的版本信息和公开的发布说明，不发送任何配置或个人数据。").Muted().FontSize(theme.Metrics.FontSizeS)
				})
				before := m.draft.Update.CheckOnStartup
				ui.Switch(c, &m.draft.Update.CheckOnStartup, "自动检查更新")
				if on := m.draft.Update.CheckOnStartup; on != before {
					c.After(func() {
						m.patchFile(func(f *config.Config) { f.Update.CheckOnStartup = on }, "已更新检查更新设置")
					})
				}
			})
		})

		card(c, "数据位置", func() {
			m.pathRow(c, "配置文件", loaded.Path, false)
			m.pathRow(c, "历史记录", config.HistoryPath(), false)
			m.pathRow(c, "日志目录", config.LogDir(), true)
			m.pathRow(c, "图片目录", loaded.Effective.Defaults.OutputDir, true)
		})
	})
}

// updateOffer declares the part of the update card shown while a newer
// release exists: its notes, then either the in-app update (button, download
// progress with cancel, install spinner, last error) or, where the app cannot
// replace itself (the Windows portable zip, or after the install failed), a
// download in the browser. It reads m.upd.avail, which must be set.
func (m *model) updateOffer(c ui.C) {
	theme := c.Theme()
	u := &m.upd
	avail := u.avail
	if len(u.notes) > 0 {
		codeBlock(c, u.notes)
	}
	switch {
	case u.downloading:
		var fraction float32
		if u.total > 0 {
			fraction = float32(u.done) / float32(u.total)
		}
		ui.Progress(c, fraction).FillWidth()
		ui.Row(c).FillWidth().Gap(12).Items(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf("正在下载 v%s · %.0f%% · %.1f / %.1f MB", avail.Version, fraction*100,
				float64(u.done)/(1<<20), float64(u.total)/(1<<20))).Muted().Grow(1)
			if ui.Button(c, "取消").Outline().Small().Clicked() {
				m.cancelUpdate()
			}
		})
	case u.installing:
		ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
			ui.Spinner(c)
			ui.Text(c, fmt.Sprintf("正在安装 v%s，完成后应用会自动重启 …", avail.Version)).Muted()
		})
	default:
		ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
			switch {
			case avail.Asset == nil:
				ui.Text(c, fmt.Sprintf("这个版本没有适用于 %s/%s 的安装包", runtime.GOOS, runtime.GOARCH)).Muted()
			case avail.Installable():
				if ui.Button(c, updateButtonLabel(avail, u.failure != "")).Clicked() {
					m.startUpdate()
				}
			}
			if avail.Asset != nil && (!avail.Installable() || u.failure != "") {
				if ui.Button(c, "在浏览器中下载").Outline().Clicked() {
					m.openURL(avail.Asset.URL)
				}
			}
		})
		if u.failure != "" {
			ui.Paragraph(c, u.failure, 4).Color(theme.Danger)
		}
	}
	hint := "下载后退出本应用再安装新版本；配置、历史与图片保存在用户目录，升级不会丢失。"
	if avail.Installable() {
		hint = "更新会在应用内下载并校验签名，然后自动退出、替换并重启；MCP 服务会中断几秒。配置、历史与图片保存在用户目录，不受影响。"
	}
	ui.Paragraph(c, hint, 2).Muted().FontSize(theme.Metrics.FontSizeS)
}

// pathRow declares one labeled path with a reveal button. ownDir marks this
// app's own folders, which are created when missing before revealing.
func (m *model) pathRow(c ui.C, label, path string, ownDir bool) {
	ui.Row(c.Key(label)).FillWidth().Gap(8).Items(ui.Center).Children(func() {
		ui.Text(c, label).Muted().Width(72)
		ui.Text(c, shortPath(path)).Mono().FontSize(12).Grow(1).Ellipsis()
		if ui.Button(c, labelReveal).Small().Ghost().Clicked() {
			if ownDir {
				m.revealOwnDir(path)
			} else {
				m.reveal(path)
			}
		}
	})
}

// openURL opens a GitHub page in the default browser. Only https links on
// github.com are opened, since release URLs come from the network.
func (m *model) openURL(u string) {
	if !strings.HasPrefix(u, "https://github.com/") {
		m.toaster.Show("已阻止打开链接", u, ui.ToastWarning)
		return
	}
	if err := app.OpenURL(u); err != nil {
		m.toaster.Show("无法打开浏览器", err.Error(), ui.ToastError)
	}
}
