package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
)

// providerText holds the provider form's free-text fields and transient UI
// state, parallel to m.draft.Providers.
type providerText struct {
	headers string // one "Name: value" per line
	extra   string // JSON object merged into every request
	showKey bool
	testing bool
	testMsg string
	testOK  bool
	testIDs []string // model IDs from the last successful /models call
}

// newProviderText fills the text fields from a provider.
func newProviderText(p *config.Provider) providerText {
	var t providerText
	keys := make([]string, 0, len(p.Headers))
	for k := range p.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, p.Headers[k])
	}
	t.headers = b.String()
	if len(p.ExtraBody) > 0 {
		if data, err := json.MarshalIndent(p.ExtraBody, "", "  "); err == nil {
			t.extra = string(data)
		}
	}
	return t
}

// apply parses the text fields into p, returning a message naming the provider
// when the headers or JSON are malformed.
func (t *providerText) apply(p *config.Provider) error {
	headers := map[string]string{}
	for i, line := range strings.Split(t.headers, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return fmt.Errorf("服务商 %s 的请求头第 %d 行应为「名称: 值」", p.Name, i+1)
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(headers) == 0 {
		headers = nil
	}
	p.Headers = headers
	p.ExtraBody = nil
	if s := strings.TrimSpace(t.extra); s != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(s), &extra); err != nil {
			return fmt.Errorf("服务商 %s 的 extra_body 不是有效的 JSON 对象: %v", p.Name, err)
		}
		p.ExtraBody = extra
	}
	return nil
}

// importDialog lists the IDs from GET /models for picking.
type importDialog struct {
	open      bool
	provider  string
	ids       []string
	picks     []bool
	imageOnly bool
}

// providersPage declares the provider list and the form for the selected one.
func (m *model) providersPage(c ui.C) {
	scaffold(c, "服务商", "配置一次，到处复用：每个服务商有自己的 base_url、API Key 和模型列表。调用时按模型自动选择服务商，失败时可回退到下一个提供同名模型的服务商。", func() {
		ui.Row(c).FillWidth().Gap(16).Items(ui.Start).Children(func() {
			m.providerList(c)
			ui.Column(c).Grow(1).Gap(16).Children(func() {
				if len(m.draft.Providers) == 0 {
					m.providerEmpty(c)
					return
				}
				m.providerForm(c, &m.draft.Providers[m.provSel], &m.provText[m.provSel])
			})
		})
	})
	m.importModal(c)
	m.deleteModal(c)
}

// providerList declares the left column of providers.
func (m *model) providerList(c ui.C) {
	theme := c.Theme()
	ui.Column(c).Width(220).Shrink(0).Gap(12).Children(func() {
		card(c, "", func() {
			for i := range m.draft.Providers {
				p := &m.draft.Providers[i]
				ui.Row(c.KeyInt(i)).FillWidth().Gap(6).Items(ui.Center).Children(func() {
					name := p.Name
					if name == "" {
						name = "（未命名）"
					}
					b := ui.Button(c, name).Grow(1)
					if i == m.provSel {
						b.Secondary()
					} else {
						b.Ghost()
					}
					if b.Clicked() {
						m.provSel = i
					}
					if !p.Enabled {
						badge(c, toneNeutral, "停用")
					}
				})
			}
			if ui.Button(c, "+ 新增服务商").Outline().FillWidth().Clicked() {
				c.After(m.addProvider)
			}
		})
		if m.core.Loaded().EnvProvider {
			card(c, "来自环境变量", func() {
				env := m.core.Loaded().Effective.Providers[0]
				ui.Text(c, "env（只读）").Weight(gfx.WeightMedium)
				ui.Text(c, env.BaseURL).Mono().FontSize(11).Ellipsis()
				ui.Paragraph(c, "由 OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODELS 生成，优先级最高，不会写入配置文件。", 4).Muted().FontSize(theme.Metrics.FontSizeS)
			})
		}
	})
}

// providerEmpty is shown when no provider is configured.
func (m *model) providerEmpty(c ui.C) {
	card(c, "", func() {
		ui.Column(c).FillWidth().Center().Gap(8).PadY(32).Children(func() {
			ui.Text(c, "还没有服务商").FontSize(16).Weight(gfx.WeightMedium)
			ui.Paragraph(c, "添加一个 OpenAI 兼容的服务（OpenAI、new-api / one-api 中转、SiliconFlow 等），填写 base_url、API Key 和图像模型即可。", 3).Muted().TextCenter().MaxWidth(420)
			if ui.Button(c, "新增服务商").MarginTop(8).Clicked() {
				c.After(m.addProvider)
			}
		})
	})
}

// providerForm declares the editing form for one provider.
//
// p and t point into the draft and its parallel text slice for the selected
// provider.
func (m *model) providerForm(c ui.C, p *config.Provider, t *providerText) {
	theme := c.Theme()
	card(c, "基本信息", func() {
		ui.Row(c).FillWidth().Gap(16).Items(ui.End).Children(func() {
			ui.Column(c).Grow(1).Children(func() {
				field(c, "名称", "", func() { ui.TextField(c, &p.Name, "名称", "例如 siliconflow").FillWidth() })
			})
			ui.Column(c).Width(200).Children(func() {
				field(c, "接口格式 (dialect)", "", func() {
					ui.Select(c, &p.Dialect, config.Dialects, dialectLabel).FillWidth()
				})
			})
			ui.Switch(c, &p.Enabled, "启用").MarginBottom(6)
		})
		field(c, "Base URL", "需包含 /v1。将请求："+imageapi.EndpointPreview(config.ExpandEnv(p.BaseURL, os.Getenv)), func() {
			ui.TextField(c, &p.BaseURL, "Base URL", "https://api.openai.com/v1").FillWidth()
		})
		field(c, "API Key", "支持 ${ENV_NAME} 引用环境变量。保存在本机配置文件（权限 0600），界面与日志只显示脱敏值。", func() {
			ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
				f := ui.TextField(c, &p.APIKey, "API Key", "sk-...").Grow(1)
				if !t.showKey {
					f.Secure()
				}
				label := "显示"
				if t.showKey {
					label = "隐藏"
				}
				if ui.Button(c, label).Outline().Clicked() {
					t.showKey = !t.showKey
				}
			})
		})
	})

	card(c, "图像模型", func() {
		ui.Paragraph(c, "勾选每个模型支持的能力：「生成」对应 generate_image，「编辑」对应 edit_image。", 2).Muted().FontSize(theme.Metrics.FontSizeS)
		for j := range p.Models {
			mdl := &p.Models[j]
			ui.Row(c.KeyInt(j)).FillWidth().Gap(12).Items(ui.Center).Children(func() {
				ui.TextField(c, &mdl.ID, "模型 ID", "例如 gpt-image-1").Grow(1)
				ui.Checkbox(c, &mdl.Generate, "生成")
				ui.Checkbox(c, &mdl.Edit, "编辑")
				if ui.Button(c, "删除").Ghost().Small().Clicked() {
					idx := j
					c.After(func() { p.Models = append(p.Models[:idx], p.Models[idx+1:]...) })
				}
			})
		}
		ui.Row(c).FillWidth().Gap(8).Children(func() {
			if ui.Button(c, "+ 添加模型").Outline().Small().Clicked() {
				c.After(func() { p.Models = append(p.Models, config.Model{Generate: true}) })
			}
			if ui.Button(c, "从 /models 导入").Outline().Small().Disabled(t.testing).Clicked() {
				m.testProvider(m.provSel, true)
			}
		})
	})

	card(c, "高级", func() {
		field(c, "附加请求头", "每行一个「名称: 值」，值支持 ${ENV_NAME}。", func() {
			ui.TextArea(c, &t.headers, "附加请求头", "X-Custom: value").FillWidth().Height(64)
		})
		field(c, "extra_body（JSON）", "合并进该服务商每次请求的请求体，例如 {\"num_inference_steps\": 20}。", func() {
			ui.TextArea(c, &t.extra, "extra_body", "{}").FillWidth().Height(64).Mono()
		})
	})

	ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
		if ui.Button(c, "测试连接").Outline().Disabled(t.testing).Clicked() {
			m.testProvider(m.provSel, false)
		}
		if t.testing {
			ui.Spinner(c)
			ui.Text(c, "正在请求 /models …").Muted()
		} else if t.testMsg != "" {
			col := theme.Danger
			if t.testOK {
				col = theme.Success
			}
			ui.Paragraph(c, t.testMsg, 3).Color(col).Grow(1)
		}
		ui.Spacer(c)
		if ui.Button(c, "撤销修改").Ghost().Clicked() {
			c.After(m.resetDraft)
		}
		if ui.Button(c, "删除服务商").DestructiveOutline().Clicked() {
			m.confirmDeleteOpen = true
		}
		if ui.Button(c, "保存").Clicked() {
			c.After(func() { m.saveDraft() })
		}
	})
}

// addProvider appends a new provider with OpenAI defaults and selects it.
func (m *model) addProvider() {
	name := "provider"
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("provider-%d", n)
		if m.draft.ProviderByName(candidate) == nil {
			name = candidate
			break
		}
	}
	p := config.Provider{
		Name: name, Dialect: config.DialectOpenAI, BaseURL: "https://api.openai.com/v1", Enabled: true,
		Models: []config.Model{{ID: "gpt-image-1", Generate: true, Edit: true}},
	}
	m.draft.Providers = append(m.draft.Providers, p)
	m.provText = append(m.provText, newProviderText(&p))
	m.provSel = len(m.draft.Providers) - 1
}

// deleteModal confirms removing the selected provider from the draft; the
// change is written when the user saves.
func (m *model) deleteModal(c ui.C) {
	if len(m.draft.Providers) == 0 {
		m.confirmDeleteOpen = false
		return
	}
	name := m.draft.Providers[m.provSel].Name
	ui.Modal(c, &m.confirmDeleteOpen, "删除服务商？", func() {
		ui.Paragraph(c, fmt.Sprintf("将删除服务商「%s」及其模型配置，点击「保存」后写入配置文件。", name), 3).MaxWidth(380)
	}, func() {
		if ui.Button(c, "取消").Outline().Clicked() {
			m.confirmDeleteOpen = false
		}
		if ui.Button(c, "删除").Destructive().Clicked() {
			m.confirmDeleteOpen = false
			idx := m.provSel
			c.After(func() {
				m.draft.Providers = append(m.draft.Providers[:idx], m.draft.Providers[idx+1:]...)
				m.provText = append(m.provText[:idx], m.provText[idx+1:]...)
				m.provSel = max(0, idx-1)
			})
		}
	})
}

// testProvider calls GET {base}/models for the provider at index i in the
// background with the form's current (unsaved) values, and reports the
// result in its text state. With openImport the model picker opens on success.
func (m *model) testProvider(i int, openImport bool) {
	p := m.draft.Providers[i]
	t := &m.provText[i]
	if err := t.apply(&p); err != nil {
		t.testOK, t.testMsg = false, err.Error()
		return
	}
	getenv := os.Getenv
	target := imageapi.Target{
		Provider: p.Name, Dialect: p.Dialect,
		BaseURL: strings.TrimRight(strings.TrimSpace(config.ExpandEnv(p.BaseURL, getenv)), "/"),
		APIKey:  strings.TrimSpace(config.ExpandEnv(p.APIKey, getenv)),
		Headers: map[string]string{},
	}
	for k, v := range p.Headers {
		target.Headers[k] = config.ExpandEnv(v, getenv)
	}
	configured := make([]string, 0, len(p.Models))
	for _, md := range p.Models {
		configured = append(configured, md.ID)
	}
	t.testing, t.testMsg = true, ""
	name := p.Name
	client := m.core.Engine.Client()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ids, dbg, err := client.ListModels(ctx, target)
		m.post(func() {
			idx := -1
			for k := range m.draft.Providers {
				if m.draft.Providers[k].Name == name {
					idx = k
					break
				}
			}
			if idx < 0 {
				return
			}
			pt := &m.provText[idx]
			pt.testing = false
			if err != nil {
				pt.testOK, pt.testMsg = false, err.Error()
				return
			}
			pt.testOK, pt.testIDs = true, ids
			have := map[string]bool{}
			for _, id := range ids {
				have[id] = true
			}
			var missing []string
			for _, id := range configured {
				if id != "" && !have[id] {
					missing = append(missing, id)
				}
			}
			msg := fmt.Sprintf("连接成功 · HTTP %d · %.1fs · 上游共 %d 个模型", dbg.Status, dbg.Elapsed.Seconds(), len(ids))
			if len(missing) > 0 {
				msg += "；未在 /models 中找到：" + strings.Join(missing, ", ") + "（部分中转不列出图像模型，可忽略）"
			}
			pt.testMsg = msg
			if openImport {
				m.openImport(idx, ids)
			}
		})
	}()
}

// openImport fills and opens the model picker for provider idx.
func (m *model) openImport(idx int, ids []string) {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	m.imp = importDialog{open: true, provider: m.draft.Providers[idx].Name, ids: sorted, picks: make([]bool, len(sorted)), imageOnly: true}
}

// imageKeywords spot image models among /models IDs.
var imageKeywords = []string{"image", "dall-e", "flux", "kolors", "stable-diffusion", "sdxl", "sd3", "qwen-image", "seedream", "imagen", "kontext", "hidream", "cogview", "wanx", "jimeng", "recraft", "ideogram", "midjourney", "playground"}

// looksLikeImageModel reports whether a model ID looks like an image model.
func looksLikeImageModel(id string) bool {
	l := strings.ToLower(id)
	for _, k := range imageKeywords {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// importModal declares the /models picker; importing adds the picked IDs that
// the provider does not have yet, guessing capabilities from the name.
func (m *model) importModal(c ui.C) {
	theme := c.Theme()
	ui.Modal(c, &m.imp.open, "从 /models 导入模型", func() {
		ui.Column(c).Width(460).Gap(8).Children(func() {
			ui.Switch(c, &m.imp.imageOnly, "只显示疑似图像模型")
			ui.Scroll(c).FillWidth().Height(320).Children(func() {
				ui.Column(c).FillWidth().Gap(4).Children(func() {
					shown := 0
					for i, id := range m.imp.ids {
						if m.imp.imageOnly && !looksLikeImageModel(id) {
							continue
						}
						shown++
						ui.Checkbox(c, &m.imp.picks[i], id)
					}
					if shown == 0 {
						ui.Text(c, "没有匹配的模型，关闭「只显示疑似图像模型」查看全部").Muted()
					}
				})
			})
			ui.Text(c, "名称含 edit 的模型标记为仅编辑；gpt-image / kontext 标记为生成+编辑；其余为仅生成。导入后可再调整。").Muted().FontSize(theme.Metrics.FontSizeS)
		})
	}, func() {
		if ui.Button(c, "取消").Outline().Clicked() {
			m.imp.open = false
		}
		if ui.Button(c, "导入选中").Clicked() {
			m.imp.open = false
			c.After(m.importPicked)
		}
	})
}

// importPicked adds the picked model IDs to the provider named in the dialog.
func (m *model) importPicked() {
	p := m.draft.ProviderByName(m.imp.provider)
	if p == nil {
		return
	}
	have := map[string]bool{}
	for _, md := range p.Models {
		have[md.ID] = true
	}
	added := 0
	for i, id := range m.imp.ids {
		if !m.imp.picks[i] || have[id] {
			continue
		}
		l := strings.ToLower(id)
		md := config.Model{ID: id, Generate: true}
		switch {
		case strings.Contains(l, "edit"):
			md.Generate, md.Edit = false, true
		case strings.Contains(l, "gpt-image") || strings.Contains(l, "kontext"):
			md.Edit = true
		}
		p.Models = append(p.Models, md)
		added++
	}
	m.toaster.Show(fmt.Sprintf("已导入 %d 个模型", added), "点击「保存」写入配置文件", ui.ToastInfo)
}

// dialectLabel names a dialect for the select.
func dialectLabel(d string) string {
	switch d {
	case config.DialectSiliconFlow:
		return "SiliconFlow"
	default:
		return "OpenAI 兼容"
	}
}
