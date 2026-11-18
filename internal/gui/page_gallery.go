package gui

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/storage"
)

// galleryPageSize is how many cards one gallery page shows.
const galleryPageSize = 24

// gallery is the history page's state.
type gallery struct {
	keyword  string
	queried  string // keyword the current items were loaded for
	page     int
	total    int
	items    []storage.Record
	stale    bool
	err      string
	selected storage.Record
	open     bool
}

// load reads one page of history for the current keyword. The history file is
// local and small, so this runs on the window thread.
func (m *model) loadGallery() {
	g := &m.gal
	if g.keyword != g.queried {
		g.page = 0
	}
	total, items, err := m.core.Engine.History(storage.Query{Limit: galleryPageSize, Offset: g.page * galleryPageSize, Keyword: g.keyword})
	g.total, g.items, g.queried, g.stale = total, items, g.keyword, false
	g.err = ""
	if err != nil {
		g.err = err.Error()
	}
}

// galleryPage declares the search bar, the thumbnail grid with paging, and
// the preview dialog.
func (m *model) galleryPage(c ui.C) {
	theme := c.Theme()
	g := &m.gal
	if g.stale || g.items == nil && g.err == "" && g.total == 0 && g.queried == "" {
		m.loadGallery()
	}
	scaffold(c, "图库", "本应用（MCP 调用与调试台）生成的全部图片，按时间倒序。记录保存在 ~/.gen-openai-image/history.jsonl。", func() {
		ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
			search := ui.TextField(c, &g.keyword, "搜索", "按 prompt、模型、服务商或路径搜索").Grow(1)
			if search.Submitted() {
				c.After(m.loadGallery)
			}
			if ui.Button(c, "搜索").Outline().Clicked() {
				c.After(m.loadGallery)
			}
			if ui.Button(c, "刷新").Ghost().Clicked() {
				c.After(m.loadGallery)
			}
			if ui.Button(c, "打开图片目录").Ghost().Clicked() {
				m.revealOwnDir(m.core.Engine.Config().Defaults.OutputDir)
			}
		})
		if g.err != "" {
			ui.Paragraph(c, g.err, 4).Color(theme.Danger)
		}
		if len(g.items) == 0 {
			card(c, "", func() {
				ui.Column(c).FillWidth().Center().PadY(40).Gap(6).Children(func() {
					ui.Text(c, "还没有图片").FontSize(16).Weight(gfx.WeightMedium)
					ui.Text(c, "在调试台生成一张，或让 Agent 调用 generate_image。").Muted()
				})
			})
			return
		}
		ui.Grid(c, 200).FillWidth().Gap(12).Children(func() {
			for _, rec := range g.items {
				m.galleryCard(c, rec)
			}
		})
		pages := (g.total + galleryPageSize - 1) / galleryPageSize
		ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf("共 %d 张 · 第 %d / %d 页", g.total, g.page+1, max(pages, 1))).Muted().Grow(1)
			if ui.Button(c, "上一页").Outline().Small().Disabled(g.page == 0).Clicked() {
				g.page--
				c.After(m.loadGallery)
			}
			if ui.Button(c, "下一页").Outline().Small().Disabled(g.page+1 >= pages).Clicked() {
				g.page++
				c.After(m.loadGallery)
			}
		})
	})
	m.galleryPreview(c)
}

// galleryCard declares one thumbnail card; clicking it opens the preview.
func (m *model) galleryCard(c ui.C, rec storage.Record) {
	theme := c.Theme()
	exists := fileExists(rec.Path)
	ui.Column(c.Key(rec.ID+rec.Path)).Gap(6).Pad(8).Radius(theme.Metrics.RadiusSurface).Background(theme.Card).Border(1, theme.Border).Children(func() {
		if exists {
			m.imageBox(c, rec.Path, 184, 184)
		} else {
			ui.Column(c).Size(184, 184).Center().Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
				ui.Text(c, "文件已不存在").Muted().FontSize(12)
			})
		}
		ui.Paragraph(c, rec.Prompt, 2).FontSize(12)
		ui.Text(c, rec.Model).Muted().FontSize(11).Ellipsis()
		ui.Row(c).FillWidth().Gap(4).Items(ui.Center).Children(func() {
			ui.Text(c, rec.CreatedAt.Format("01-02 15:04")).Muted().FontSize(11).Grow(1)
			if ui.Button(c, "详情").XSmall().Ghost().Clicked() {
				m.gal.selected, m.gal.open = rec, true
			}
		})
	})
}

// galleryPreview is the large preview dialog with the record's details and
// actions, including "用此图编辑" which jumps to the debug console.
func (m *model) galleryPreview(c ui.C) {
	theme := c.Theme()
	g := &m.gal
	rec := g.selected
	ui.Modal(c, &g.open, filepath.Base(rec.Path), func() {
		ui.Row(c).Gap(16).Items(ui.Start).Children(func() {
			if fileExists(rec.Path) {
				m.imageBox(c, rec.Path, 420, 420)
			}
			ui.Column(c).Width(280).Gap(8).Children(func() {
				detail := func(k, v string) {
					if v == "" {
						return
					}
					ui.Column(c).Gap(2).Children(func() {
						ui.Text(c, k).Muted().FontSize(theme.Metrics.FontSizeS)
						ui.Paragraph(c, v, 8).FontSize(13)
					})
				}
				detail("Prompt", rec.Prompt)
				detail("改写后的提示词", rec.RevisedPrompt)
				detail("模型 / 服务商", rec.Model+" · "+rec.Provider)
				detail("尺寸", fmt.Sprintf("%d×%d  %s", rec.Width, rec.Height, rec.MIME))
				detail("来源", kindLabel(rec.Kind)+" · "+sourceLabel(rec.Source))
				detail("时间", rec.CreatedAt.Format("2006-01-02 15:04:05"))
				detail("路径", rec.Path)
			})
		})
	}, func() {
		if ui.Button(c, "复制路径").Outline().Clicked() {
			m.copy(rec.Path, "路径已复制")
		}
		if ui.Button(c, labelReveal).Outline().Clicked() {
			m.reveal(rec.Path)
		}
		if ui.Button(c, "打开").Outline().Clicked() {
			m.open(rec.Path)
		}
		if ui.Button(c, "用此图编辑").Clicked() {
			g.open = false
			m.pg.mode = modeEdit
			m.pg.addImage(rec.Path)
			m.tab = tabPlayground
		}
	})
}

// kindLabel names a history kind.
func kindLabel(k string) string {
	if k == "edit" {
		return "编辑"
	}
	return "生成"
}

// sourceLabel names a history source.
func sourceLabel(s string) string {
	if s == "debug" {
		return "调试台"
	}
	return "MCP 调用"
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
