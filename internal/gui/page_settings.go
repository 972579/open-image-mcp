package gui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/config"
)

// settingsForm holds the settings page's text inputs; numbers are parsed when
// saving.
type settingsForm struct {
	host, port, path string
	model, size, n   string
	outputDir        string
	prefix           string
	previewPx        string
	timeout, retries string
}

// newSettingsForm fills the form from the draft.
func newSettingsForm(c *config.Config) settingsForm {
	return settingsForm{
		host: c.Server.Host, port: strconv.Itoa(c.Server.Port), path: c.Server.Path,
		model: c.Defaults.Model, size: c.Defaults.Size, n: strconv.Itoa(c.Defaults.N),
		outputDir: c.Defaults.OutputDir, prefix: c.Defaults.FilenamePrefix,
		previewPx: strconv.Itoa(c.Defaults.PreviewMaxPx),
		timeout:   strconv.Itoa(int(c.Request.Timeout.Std().Seconds())), retries: strconv.Itoa(c.Request.MaxRetries),
	}
}

// apply parses the form into c, reporting the first malformed number.
func (f *settingsForm) apply(c *config.Config) error {
	num := func(label, s string, lo, hi int) (int, error) {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < lo || v > hi {
			return 0, fmt.Errorf("%s 必须是 %d-%d 之间的整数", label, lo, hi)
		}
		return v, nil
	}
	var err error
	if c.Server.Port, err = num("端口", f.port, 1, 65535); err != nil {
		return err
	}
	if c.Defaults.N, err = num("默认数量 n", f.n, 1, 10); err != nil {
		return err
	}
	if c.Defaults.PreviewMaxPx, err = num("预览尺寸", f.previewPx, 64, 2048); err != nil {
		return err
	}
	secs, err := num("请求超时（秒）", f.timeout, 5, 3600)
	if err != nil {
		return err
	}
	c.Request.Timeout = config.Duration(time.Duration(secs) * time.Second)
	if c.Request.MaxRetries, err = num("重试次数", f.retries, 0, 5); err != nil {
		return err
	}
	c.Server.Host = strings.TrimSpace(f.host)
	c.Server.Path = strings.TrimSpace(f.path)
	c.Defaults.Model = strings.TrimSpace(f.model)
	c.Defaults.Size = strings.TrimSpace(f.size)
	c.Defaults.OutputDir = strings.TrimSpace(f.outputDir)
	c.Defaults.FilenamePrefix = strings.TrimSpace(f.prefix)
	return nil
}

// settingsPage declares defaults, request behaviour and the listen address.
func (m *model) settingsPage(c ui.C) {
	theme := c.Theme()
	loaded := m.core.Loaded()
	eff := loaded.Effective
	f := &m.settings
	scaffold(c, "设置", "优先级：环境变量 > 配置文件 > 启动参数。被环境变量覆盖的项以只读方式显示；保存后写入 "+shortPath(loaded.Path)+"。", func() {
		card(c, "默认值", func() {
			field(c, "默认模型", "工具调用未指定 model 时使用。", func() {
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					if loaded.Sources[config.KeyDefaultModel] == config.SourceEnv {
						ui.Text(c, eff.Defaults.Model).Mono().Grow(1)
						badge(c, toneWarning, "环境变量 DEFAULT_MODEL")
						return
					}
					choices := m.modelIDChoices()
					ui.Select(c, &f.model, choices, modelChoiceLabel).Width(320)
					sourceBadge(c, loaded.Sources[config.KeyDefaultModel])
				})
			})
			ui.Row(c).FillWidth().Gap(16).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					field(c, "默认尺寸", "", func() { ui.TextField(c, &f.size, "默认尺寸", "1024x1024").FillWidth() })
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "默认数量 n", "", func() { ui.TextField(c, &f.n, "默认数量", "1").FillWidth() })
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "文件名前缀", "", func() { ui.TextField(c, &f.prefix, "文件名前缀", "img").FillWidth() })
				})
			})
			field(c, "图片保存目录", "按日期分子目录保存，例如 …/2026-11-18/img_20261118-153012_gpt-image-2_a1b2c3_01.png", func() {
				ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
					if loaded.Sources[config.KeyOutputDir] == config.SourceEnv {
						ui.Text(c, eff.Defaults.OutputDir).Mono().Grow(1).Ellipsis()
						badge(c, toneWarning, "环境变量 OUTPUT_DIR")
						return
					}
					ui.TextField(c, &f.outputDir, "图片保存目录", "~/Pictures/gen-openai-image").Grow(1)
					if ui.Button(c, "选择…").Outline().Clicked() {
						c.After(func() {
							if dir, err := app.OpenDirectory(app.OpenDirectoryOptions{Title: "选择图片保存目录"}); err == nil && dir != "" {
								f.outputDir = dir
							}
						})
					}
					if ui.Button(c, labelOpenDir).Ghost().Clicked() {
						m.revealOwnDir(config.ExpandHome(strings.TrimSpace(f.outputDir)))
					}
				})
			})
			ui.Row(c).FillWidth().Gap(16).Items(ui.Center).Children(func() {
				ui.Switch(c, &m.draft.Defaults.IncludePreview, "工具结果附带缩略图预览")
				ui.Text(c, "预览最长边（px）").Muted()
				ui.TextField(c, &f.previewPx, "预览尺寸", "512").Width(90)
			})
		})

		card(c, "请求", func() {
			ui.Row(c).FillWidth().Gap(16).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					field(c, "请求超时（秒）", "单次上游请求的最长等待时间。", func() {
						ui.TextField(c, &f.timeout, "请求超时", "180").FillWidth()
					})
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "重试次数", "仅对网络错误、429、5xx 重试，指数退避。", func() {
						ui.TextField(c, &f.retries, "重试次数", "1").FillWidth()
					})
				})
			})
			ui.Switch(c, &m.draft.Request.Fallback, "失败后回退到下一个提供同名模型的服务商（鉴权失败和参数错误不回退）")
		})

		card(c, "监听地址", func() {
			ui.Row(c).FillWidth().Gap(16).Children(func() {
				ui.Column(c).Grow(2).Children(func() {
					field(c, "Host", "默认 127.0.0.1；非回环地址必须设置 Token。", func() {
						ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
							if loaded.Sources[config.KeyServerHost] == config.SourceEnv {
								ui.Text(c, eff.Server.Host).Mono().Grow(1)
								badge(c, toneWarning, "GENIMAGE_HOST")
								return
							}
							ui.TextField(c, &f.host, "Host", "127.0.0.1").Grow(1)
							sourceBadge(c, loaded.Sources[config.KeyServerHost])
						})
					})
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "端口", "", func() {
						ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
							if loaded.Sources[config.KeyServerPort] == config.SourceEnv {
								ui.Text(c, strconv.Itoa(eff.Server.Port)).Mono().Grow(1)
								badge(c, toneWarning, "GENIMAGE_PORT")
								return
							}
							ui.TextField(c, &f.port, "端口", "8765").Grow(1)
							sourceBadge(c, loaded.Sources[config.KeyServerPort])
						})
					})
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "路径", "", func() { ui.TextField(c, &f.path, "路径", "/mcp").FillWidth() })
				})
			})
		})

		ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
			ui.Text(c, "配置文件："+shortPath(loaded.Path)).Muted().FontSize(theme.Metrics.FontSizeS).Grow(1).Ellipsis()
			if ui.Button(c, labelReveal).Ghost().Clicked() {
				m.reveal(loaded.Path)
			}
			if ui.Button(c, "撤销修改").Ghost().Clicked() {
				c.After(m.resetDraft)
			}
			if ui.Button(c, "保存").Clicked() {
				c.After(func() { m.saveDraft() })
			}
		})
	})
}

// modelIDChoices lists the distinct model IDs in the draft (plus the current
// default and "" for none), sorted, for the default-model select.
func (m *model) modelIDChoices() []string {
	seen := map[string]bool{"": true}
	out := []string{""}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, p := range m.draft.Providers {
		for _, md := range p.Models {
			add(md.ID)
		}
	}
	add(m.settings.model)
	sort.Strings(out[1:])
	return out
}

// modelChoiceLabel shows "" as "（不设置）".
func modelChoiceLabel(id string) string {
	if id == "" {
		return "（不设置，使用第一个可用模型）"
	}
	return id
}

// sourceBadge marks a value that currently comes from a startup flag.
func sourceBadge(c ui.C, s config.Source) {
	if s == config.SourceFlag {
		badge(c, toneNeutral, "启动参数")
	}
}
