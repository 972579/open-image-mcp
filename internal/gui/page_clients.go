package gui

import (
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/clientcfg"
	"github.com/972579/open-image-mcp/internal/mcpserver"
)

// clientsPage is the client-config page's cached state: the snippets are
// regenerated only when the endpoint URL or token changes.
type clientsPage struct {
	tab      string
	key      string
	snippets []clientcfg.Snippet
	lines    [][]string
	names    []string
}

// refresh rebuilds the snippets when url or token changed since last time.
func (p *clientsPage) refresh(url, token string) {
	key := url + "\x00" + token
	if key == p.key {
		return
	}
	p.key = key
	p.snippets = clientcfg.All(clientcfg.Info{Name: "gen-image", URL: url, Token: token})
	p.lines = make([][]string, len(p.snippets))
	p.names = make([]string, len(p.snippets))
	for i, s := range p.snippets {
		p.lines[i] = splitLines(s.Body)
		p.names[i] = s.Client
	}
}

// clientsPage shows ready-to-paste MCP configs for each client, generated from
// the effective server settings (the running server's when it differs).
func (m *model) clientsPage(c ui.C) {
	theme := c.Theme()
	eff := m.core.Loaded().Effective.Server
	url := mcpserver.EndpointURL(eff)
	token := eff.AuthToken
	m.clients.refresh(url, token)
	scaffold(c, "客户端配置", "把下面的配置加到对应客户端即可使用。配置中的地址和 Token 来自当前设置；修改设置后这里会自动更新。", func() {
		if m.status.State != mcpserver.StateRunning {
			ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Pad(12).Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
				badge(c, toneWarning, "服务未启动")
				ui.Text(c, "客户端连接前请先在「服务」页启动 MCP 服务。").Grow(1)
				if ui.Button(c, "启动服务").Small().Clicked() {
					m.startServer()
				}
			})
		}
		ui.Tabs(c, &m.clients.tab, m.clients.names, nil)
		for i, s := range m.clients.snippets {
			if s.Client != m.clients.tab {
				continue
			}
			body, lines := s.Body, m.clients.lines[i]
			card(c.Key(s.Client), "", func() {
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					ui.Text(c, s.Client).Weight(gfx.WeightMedium)
					badge(c, toneNeutral, s.Language)
					ui.Spacer(c)
					if ui.Button(c, "复制").Clicked() {
						m.copy(body, s.Client+" 配置已复制")
					}
				})
				if s.File != "" {
					ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
						ui.Text(c, "配置文件").Muted().FontSize(theme.Metrics.FontSizeS)
						ui.Text(c, s.File).Mono().FontSize(12).Grow(1).Ellipsis()
						if ui.Button(c, labelReveal).Small().Ghost().Clicked() {
							m.revealConfigFile(s.File)
						}
					})
				}
				codeBlock(c, lines)
				for _, n := range s.Notes {
					ui.Paragraph(c, "• "+n, 3).Muted().FontSize(theme.Metrics.FontSizeS)
				}
			})
		}
		card(c, "验证连接", func() {
			ui.Paragraph(c, "可用 MCP Inspector 检查服务：npx @modelcontextprotocol/inspector，传输方式选 Streamable HTTP，地址填 "+url+"（有 Token 时在 Authorization 头填 Bearer <token>）。也可以在「调试台」选择「经由 MCP 服务」做一次端到端调用。", 4).Muted()
		})
	})
}

// revealConfigFile shows a client's config file in the file browser, or its folder when
// the file does not exist yet.
func (m *model) revealConfigFile(path string) {
	p := expandClientPath(path)
	if _, err := statPath(p); err != nil {
		p = parentDir(p)
	}
	m.reveal(p)
}
