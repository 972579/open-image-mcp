package gui

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/mcpserver"
)

// serverPage shows the MCP server status with start/stop/restart, the token,
// the autostart switch and the live log.
func (m *model) serverPage(c ui.C) {
	theme := c.Theme()
	eff := m.core.Loaded().Effective
	scaffold(c, "MCP 服务", "以 Streamable HTTP 方式对外提供 generate_image / batch_generate_images / edit_image / batch_edit_images / get_image_job / list_models / list_generated_images 七个工具。", func() {
		card(c, "", func() {
			ui.Row(c).FillWidth().Gap(12).Items(ui.Center).Children(func() {
				statusDot(c, m.status.State)
				ui.Text(c, stateLabel(m.status.State)).FontSize(16).Weight(gfx.WeightSemibold)
				if m.status.State == mcpserver.StateRunning {
					badge(c, toneSuccess, "Streamable HTTP")
				}
				ui.Spacer(c)
				running := m.status.State == mcpserver.StateRunning
				if running {
					if ui.Button(c, "重启").Outline().Disabled(m.stopping).Clicked() {
						m.restartServer()
					}
					if ui.Button(c, "停止服务").Destructive().Disabled(m.stopping).Clicked() {
						m.stopServer()
					}
				} else if ui.Button(c, "启动服务").Disabled(m.stopping).Clicked() {
					m.startServer()
				}
			})
			url := m.status.URL
			if url == "" {
				url = mcpserver.EndpointURL(eff.Server)
			}
			ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
				ui.Text(c, "地址").Muted().Width(72)
				ui.Text(c, url).Mono().Grow(1).Ellipsis()
				if ui.Button(c, "复制地址").Small().Outline().Clicked() {
					m.copy(url, "地址已复制")
				}
			})
			if m.status.State == mcpserver.StateRunning {
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					ui.Text(c, "启动于").Muted().Width(72)
					ui.Text(c, m.status.StartedAt.Format("2006-01-02 15:04:05")).Grow(1)
				})
				ui.Row(c).FillWidth().Gap(16).Children(func() {
					m.stat(c, "HTTP 请求", fmt.Sprint(m.status.Requests))
					m.stat(c, "工具调用", fmt.Sprint(m.status.ToolCalls))
					m.stat(c, "拒绝（Token 错误）", fmt.Sprint(m.status.Rejected))
				})
			}
			if m.status.LastError != "" {
				ui.Paragraph(c, m.status.LastError, 4).Color(theme.Danger)
			}
			if m.closeHides {
				ui.Paragraph(c, "关闭窗口后应用会在后台继续运行，MCP 服务不中断；点 Dock 图标可重新打开窗口，从 Dock 右键或菜单栏选择「退出」才会停止服务并退出。", 3).Muted().FontSize(theme.Metrics.FontSizeS)
			}
			if m.core.NeedsRestart() {
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					badge(c, toneWarning, "待重启")
					ui.Text(c, "监听地址或 Token 已修改，重启服务后生效").Muted()
				})
			}
		})

		card(c, "访问控制", func() {
			token := m.draft.Server.AuthToken
			src := m.core.Loaded().Sources[config.KeyServerToken]
			if src == config.SourceEnv {
				token = eff.Server.AuthToken
			}
			ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
				ui.Text(c, "Token").Muted().Width(72)
				switch {
				case token == "":
					ui.Text(c, "未设置（仅监听本机时可不设置）").Muted().Grow(1)
				case m.showToken:
					ui.Text(c, token).Mono().Grow(1).Ellipsis()
				default:
					ui.Text(c, config.Mask(token)).Mono().Grow(1)
				}
				if src == config.SourceEnv {
					badge(c, toneWarning, "来自环境变量 GENIMAGE_TOKEN")
					return
				}
				if token != "" {
					label := "显示"
					if m.showToken {
						label = "隐藏"
					}
					if ui.Button(c, label).Small().Ghost().Clicked() {
						m.showToken = !m.showToken
					}
					if ui.Button(c, "复制").Small().Outline().Clicked() {
						m.copy(token, "Token 已复制")
					}
				}
				if ui.Button(c, "生成新 Token").Small().Outline().Clicked() {
					tok := config.NewToken()
					m.patchFile(func(f *config.Config) { f.Server.AuthToken = tok }, "已生成新 Token")
				}
				if token != "" && ui.Button(c, "清除").Small().Ghost().Clicked() {
					m.patchFile(func(f *config.Config) { f.Server.AuthToken = "" }, "已清除 Token")
				}
			})
			ui.Paragraph(c, "设置 Token 后客户端需携带 Authorization: Bearer <token>；「客户端配置」页生成的示例会自动带上。默认只监听 127.0.0.1，并启用 DNS 重绑定防护。", 3).Muted().FontSize(theme.Metrics.FontSizeS)
			ui.Divider(c)
			ui.Row(c).FillWidth().Gap(16).Items(ui.Center).Children(func() {
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, "打开 App 时自动启动服务").Weight(gfx.WeightMedium)
					ui.Text(c, "保存在配置文件 server.autostart").Muted().FontSize(theme.Metrics.FontSizeS)
				})
				before := m.draft.Server.Autostart
				ui.Switch(c, &m.draft.Server.Autostart, "自动启动")
				if on := m.draft.Server.Autostart; on != before {
					c.After(func() {
						m.patchFile(func(f *config.Config) { f.Server.Autostart = on }, "已更新自动启动设置")
					})
				}
			})
		})

		card(c, "运行日志", func() {
			ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("最近 %d 行（API Key 已脱敏）", len(m.logs))).Muted().FontSize(theme.Metrics.FontSizeS).Grow(1)
				if ui.Button(c, "打开日志目录").Small().Outline().Clicked() {
					m.revealOwnDir(config.LogDir())
				}
			})
			ui.Scroll(c).FillWidth().Height(260).Follow(true).Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
				ui.Column(c).FillWidth().Pad(10).Gap(2).Children(func() {
					if len(m.logs) == 0 {
						ui.Text(c, "暂无日志").Muted().Mono().FontSize(11)
					}
					for _, line := range m.logs {
						ui.Wrap(c, line).Mono().FontSize(11)
					}
				})
			})
		})
	})
}

// stat declares one small figure with its caption.
func (m *model) stat(c ui.C, caption, value string) {
	theme := c.Theme()
	ui.Column(c).Grow(1).Pad(12).Gap(2).Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
		ui.Text(c, value).FontSize(20).Weight(gfx.WeightSemibold)
		ui.Text(c, caption).Muted().FontSize(theme.Metrics.FontSizeS)
	})
}

// startServer starts the MCP server with the effective settings and reports
// the outcome as a toast. Binding is quick, so it runs on the window thread.
func (m *model) startServer() {
	if err := m.core.StartServer(); err != nil {
		m.toaster.Show("启动失败", err.Error(), ui.ToastError)
	} else {
		m.toaster.Show("MCP 服务已启动", m.core.Manager.Status().URL, ui.ToastSuccess)
	}
	m.refreshStatus()
}

// stopServer stops the server in the background (graceful shutdown can wait
// for a running generation) and reports back through post.
func (m *model) stopServer() {
	m.stopping = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		err := m.core.Manager.Stop(ctx)
		m.post(func() {
			m.stopping = false
			if err != nil {
				m.toaster.Show("停止失败", err.Error(), ui.ToastError)
			} else {
				m.toaster.Show("MCP 服务已停止", "", ui.ToastInfo)
			}
		})
	}()
}

// restartServer restarts the server in the background with the effective
// settings, so listen-address and token changes take effect.
func (m *model) restartServer() {
	m.stopping = true
	go func() {
		err := m.core.RestartServer()
		m.post(func() {
			m.stopping = false
			if err != nil {
				m.toaster.Show("重启失败", err.Error(), ui.ToastError)
			} else {
				m.toaster.Show("MCP 服务已重启", m.core.Manager.Status().URL, ui.ToastSuccess)
			}
		})
	}()
}

// patchFile changes one setting in the saved file layer and saves it at once,
// without saving other unsaved edits in the draft. The same change is applied
// to the draft so the forms stay in step. done is the success toast title.
func (m *model) patchFile(apply func(*config.Config), done string) {
	file := m.core.Loaded().File.Clone()
	apply(&file)
	if err := m.core.SaveFile(file); err != nil {
		m.toaster.Show("保存失败", err.Error(), ui.ToastError)
		return
	}
	apply(&m.draft)
	if m.core.NeedsRestart() {
		m.toaster.Show(done, "重启 MCP 服务后生效", ui.ToastWarning)
	} else {
		m.toaster.Show(done, "", ui.ToastSuccess)
	}
}

// copy puts text on the clipboard and confirms with a toast.
func (m *model) copy(text, done string) {
	if err := app.SetClipboardText(text); err != nil {
		m.toaster.Show("复制失败", err.Error(), ui.ToastError)
		return
	}
	m.toaster.Show(done, "", ui.ToastSuccess)
}

// reveal shows a file or folder in Finder (Explorer on Windows). It never creates anything; use
// revealOwnDir for this app's own folders that may not exist yet.
func (m *model) reveal(path string) {
	if err := app.RevealPath(path); err != nil {
		m.toaster.Show("无法在"+fileManager+"中显示", err.Error(), ui.ToastError)
	}
}

// revealOwnDir creates one of this app's own folders (log or output
// directory) when missing, then shows it in the file browser.
func (m *model) revealOwnDir(path string) {
	if _, err := os.Stat(path); err != nil {
		_ = os.MkdirAll(path, 0o755)
	}
	m.reveal(path)
}

// open opens a file with its default application (Preview for images).
func (m *model) open(path string) {
	if err := app.OpenPath(path); err != nil {
		m.toaster.Show("无法打开", err.Error(), ui.ToastError)
	}
}

// homeDir returns the user's home directory or "".
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
