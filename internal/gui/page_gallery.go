package gui

import (
	"errors"
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
	open     bool // the preview dialog for selected
	confirm  bool // the delete confirmation for selected
}

// load reads one page of history for the current keyword. The history file is
// local and small, so this runs on the window thread. A page that became
// empty (its last image was deleted) falls back to the previous page.
func (m *model) loadGallery() {
	g := &m.gal
	if g.keyword != g.queried {
		g.page = 0
	}
	total, items, err := m.core.Engine.History(storage.Query{Limit: galleryPageSize, Offset: g.page * galleryPageSize, Keyword: g.keyword})
	if err == nil && len(items) == 0 && g.page > 0 && total > 0 {
		g.page = (total - 1) / galleryPageSize
		total, items, err = m.core.Engine.History(storage.Query{Limit: galleryPageSize, Offset: g.page * galleryPageSize, Keyword: g.keyword})
	}
	g.total, g.items, g.queried, g.stale = total, items, g.keyword, false
	g.err = ""
	if err != nil {
		g.err = err.Error()
	}
}

// galleryPage declares the search bar, the thumbnail grid with paging, and
// the preview and delete dialogs.
func (m *model) galleryPage(c ui.C) {
	theme := c.Theme()
	g := &m.gal
	if g.stale || g.items == nil && g.err == "" && g.total == 0 && g.queried == "" {
		m.loadGallery()
	}
	scaffold(c, "图库", "本应用（MCP 调用与调试台）生成的全部图片，按时间倒序；MCP 指定了 output_dir 的图片也会在图片目录里保留一份。点图片查看大图和提示词。", func() {
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
	m.galleryDeleteDialog(c)
}

// galleryCard declares one thumbnail card. Clicking the thumbnail (or 详情)
// opens the preview dialog for rec.
func (m *model) galleryCard(c ui.C, rec storage.Record) {
	theme := c.Theme()
	exists := fileExists(rec.Path)
	ui.Column(c.Key(rec.ID+rec.Path)).Gap(6).Pad(8).Radius(theme.Metrics.RadiusSurface).Background(theme.Card).Border(1, theme.Border).Children(func() {
		thumb := ui.Column(c).Hit(ui.ID(c), "预览 "+filepath.Base(rec.Path)).Children(func() {
			if exists {
				m.imageBox(c, rec.Path, 184, 184)
			} else {
				ui.Column(c).Size(184, 184).Center().Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
					ui.Text(c, "文件已不存在").Muted().FontSize(12)
				})
			}
		})
		if thumb.Clicked() {
			m.gal.selected, m.gal.open = rec, true
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

// previewDialogWidth fits the 420px image, the 16px gap and the 320px details
// column inside the dialog's padding. The default dialog (448px) is narrower
// than the image alone, which pushed the prompt and details out of view.
const previewDialogWidth = 820

// galleryPreview is the large preview dialog: the image, the full prompt (and
// the provider's revised prompt) in their own scrollable sections with copy
// buttons, the record's details, and the actions: copy path, reveal, open,
// "用此图编辑" (jumps to the debug console) and 删除 (asks first).
func (m *model) galleryPreview(c ui.C) {
	theme := c.Theme()
	g := &m.gal
	rec := g.selected
	ui.ModalWith(c, &g.open, filepath.Base(rec.Path), ui.ModalSpec{Width: previewDialogWidth}, func() {
		ui.Row(c).Gap(16).Items(ui.Start).Children(func() {
			if fileExists(rec.Path) {
				m.imageBox(c, rec.Path, 420, 420)
			} else {
				ui.Column(c).Size(420, 420).Center().Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
					ui.Text(c, "文件已不存在").Muted()
				})
			}
			ui.Column(c).Width(320).Gap(10).Children(func() {
				m.promptSection(c, "提示词", rec.Prompt, "复制提示词")
				m.promptSection(c, "改写后的提示词", rec.RevisedPrompt, "复制改写后的提示词")
				detail := func(k, v string) {
					if v == "" {
						return
					}
					ui.Column(c).Gap(2).Children(func() {
						ui.Text(c, k).Muted().FontSize(theme.Metrics.FontSizeS)
						ui.Paragraph(c, v, 4).FontSize(13)
					})
				}
				detail("模型 / 服务商", rec.Model+" · "+rec.Provider)
				detail("尺寸", fmt.Sprintf("%d×%d  %s", rec.Width, rec.Height, rec.MIME))
				detail("来源", kindLabel(rec.Kind)+" · "+sourceLabel(rec.Source))
				detail("时间", rec.CreatedAt.Format("2006-01-02 15:04:05"))
				detail("图库路径", rec.Path)
				detail("指定保存路径", rec.OutputPath)
			})
		})
	}, func() {
		if ui.Button(c, "删除").Destructive().Clicked() {
			g.confirm = true
		}
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

// promptSection declares one titled prompt in the preview: the whole text,
// scrolling past about ten lines, with a button that copies it. Nothing is
// declared for an empty text.
//
// title heads the section; text is the prompt; copyLabel names the copy
// button, which must be unique in the dialog.
func (m *model) promptSection(c ui.C, title, text, copyLabel string) {
	if text == "" {
		return
	}
	theme := c.Theme()
	ui.Column(c).FillWidth().Gap(4).Children(func() {
		ui.Row(c).FillWidth().Items(ui.Center).Children(func() {
			ui.Text(c, title).Muted().FontSize(theme.Metrics.FontSizeS).Grow(1)
			if ui.Button(c, copyLabel).XSmall().Ghost().Clicked() {
				m.copy(text, "已复制"+title)
			}
		})
		ui.Scroll(c).FillWidth().MaxHeight(200).Pad(8).Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Children(func() {
			ui.Paragraph(c, text, 0).FontSize(13).FillWidth()
		})
	})
}

// galleryDeleteDialog asks before deleting the selected image. Deleting
// removes it from the history and deletes the gallery's file for good; a copy
// saved to an MCP output_dir elsewhere is kept, which the dialog says. On
// success both dialogs close and the grid reloads.
func (m *model) galleryDeleteDialog(c ui.C) {
	g := &m.gal
	rec := g.selected
	ui.Modal(c, &g.confirm, "删除这张图片？", func() {
		ui.Column(c).Gap(8).MaxWidth(420).Children(func() {
			ui.Paragraph(c, "会从图库移除这条记录，并删除图片文件（不进废纸篓，无法恢复）：", 3)
			ui.Paragraph(c, rec.Path, 3).Mono().FontSize(12)
			if rec.OutputPath != "" {
				ui.Paragraph(c, "生成时指定目录里的那一份会保留："+rec.OutputPath, 4).Muted().FontSize(12)
			}
		})
	}, func() {
		if ui.Button(c, "取消").Outline().Clicked() {
			g.confirm = false
		}
		if ui.Button(c, "删除图片").Destructive().Clicked() {
			g.confirm = false
			m.deleteGalleryImage(rec)
		}
	})
}

// deleteGalleryImage deletes rec through the engine (history record and
// gallery file), closes the preview, reloads the grid and reports the outcome
// as a toast. A record that is already gone just refreshes the grid.
func (m *model) deleteGalleryImage(rec storage.Record) {
	_, err := m.core.Engine.DeleteImage(rec.ID)
	switch {
	case err == nil:
		m.gal.open = false
		m.toaster.Show("已删除", filepath.Base(rec.Path), ui.ToastSuccess)
	case errors.Is(err, storage.ErrNotFound):
		m.gal.open = false
		m.toaster.Show("图片已不在图库中", "", ui.ToastInfo)
	default:
		m.toaster.Show("删除失败", err.Error(), ui.ToastError)
	}
	m.loadGallery()
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
