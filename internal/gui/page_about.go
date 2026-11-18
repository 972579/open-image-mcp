package gui

import (
	"context"
	"fmt"
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

// updateState is the result of the last update check.
type updateState struct {
	checking  bool
	checkedAt time.Time
	latest    *update.Release
	newer     bool
	err       string
	notes     []string // release notes split into lines, cached
}

// checkUpdate asks GitHub for the latest release in the background. A manual
// check reports every outcome as a toast; the startup check stays quiet unless
// a newer version exists, and only logs failures.
func (m *model) checkUpdate(manual bool) {
	if m.upd.checking {
		return
	}
	m.upd.checking = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rel, err := update.Latest(ctx, nil, version.Repo, version.Version)
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
			u.err, u.latest = "", rel
			u.newer = update.Newer(rel.Version, version.Version)
			u.notes = splitLines(strings.TrimSpace(rel.Notes))
			if len(u.notes) > 40 {
				u.notes = append(u.notes[:40], "…")
			}
			switch {
			case u.newer:
				m.toaster.Show("发现新版本 v"+rel.Version, "在「关于」页查看更新内容并下载", ui.ToastInfo)
			case manual:
				m.toaster.Show("已是最新版本", "当前 v"+version.Version, ui.ToastSuccess)
			}
		})
	}()
}

// updateBadge is the sidebar notice shown while a newer release exists; it
// opens the about page.
func (m *model) updateBadge(c ui.C) {
	if !m.upd.newer || m.upd.latest == nil {
		return
	}
	if ui.Button(c, "有新版本 v"+m.upd.latest.Version).Small().FillWidth().MarginBottom(8).Clicked() {
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
				if ui.Button(c, "检查更新").Disabled(u.checking).Clicked() {
					m.checkUpdate(true)
				}
				switch {
				case u.checking:
					ui.Spinner(c)
					ui.Text(c, "正在查询 GitHub Releases …").Muted()
				case u.err != "":
					ui.Paragraph(c, u.err, 3).Color(theme.Danger).Grow(1)
				case u.latest != nil && u.newer:
					badge(c, toneAccent, "新版本 v"+u.latest.Version)
					ui.Text(c, "发布于 "+u.latest.Published.Local().Format("2006-01-02")).Muted()
				case u.latest != nil:
					badge(c, toneSuccess, "已是最新")
					ui.Text(c, fmt.Sprintf("最新发布 v%s · 检查于 %s", u.latest.Version, u.checkedAt.Format("15:04"))).Muted()
				default:
					ui.Text(c, "尚未检查").Muted()
				}
			})
			if u.latest != nil && u.newer {
				rel := u.latest
				if len(u.notes) > 0 {
					codeBlock(c, u.notes)
				}
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					if a := rel.AssetFor(runtime.GOOS, runtime.GOARCH); a != nil {
						if ui.Button(c, fmt.Sprintf("下载 %s（%.1f MB）", a.Name, float64(a.Size)/(1<<20))).Clicked() {
							m.openURL(a.URL)
						}
					}
					if ui.Button(c, "打开发布页").Outline().Clicked() {
						m.openURL(rel.URL)
					}
				})
				ui.Paragraph(c, "下载后退出本应用再安装新版本；配置、历史与图片保存在用户目录，升级不会丢失。", 2).Muted().FontSize(theme.Metrics.FontSizeS)
			}
			ui.Divider(c)
			ui.Row(c).FillWidth().Gap(16).Items(ui.Center).Children(func() {
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, "启动时自动检查更新").Weight(gfx.WeightMedium)
					ui.Text(c, "只访问 api.github.com 读取公开的版本信息，不发送任何配置或个人数据。").Muted().FontSize(theme.Metrics.FontSizeS)
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
// github.com are opened, since release URLs come from the API response.
func (m *model) openURL(u string) {
	if !strings.HasPrefix(u, "https://github.com/") {
		m.toaster.Show("已阻止打开链接", u, ui.ToastWarning)
		return
	}
	if err := app.OpenURL(u); err != nil {
		m.toaster.Show("无法打开浏览器", err.Error(), ui.ToastError)
	}
}
