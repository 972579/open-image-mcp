package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/engine"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/mcpserver"
	"github.com/972579/open-image-mcp/internal/provider"
	"github.com/972579/open-image-mcp/internal/storage"
)

// Debug console modes and call paths.
const (
	modeGenerate = "生成"
	modeEdit     = "编辑"
	viaEngine    = "直连引擎"
	viaMCP       = "经由 MCP 服务"
)

var (
	modes     = []string{modeGenerate, modeEdit}
	vias      = []string{viaEngine, viaMCP}
	qualities = []string{"", "auto", "low", "medium", "high", "standard", "hd"}
	styles    = []string{"", "vivid", "natural"}
)

// modelChoice is one entry of the model select: a provider and model, or the
// zero value for "use the default".
type modelChoice struct {
	Provider string
	Model    string
}

// playground is the debug console's state.
type playground struct {
	mode, via        string
	prompt, negative string
	choice           modelChoice
	choices          []modelChoice
	choicesKey       string
	size, n, seed    string
	quality, style   string
	extra            string
	images           []string
	mask             string

	running  bool
	cancel   context.CancelFunc
	started  time.Time
	progress string
	runID    int
	result   *pgResult

	showDebug bool
}

// pgResult is the outcome of one debug run.
type pgResult struct {
	err     string
	images  []storage.Saved
	summary string
	debug   []string
}

// init sets the console's starting values.
func (p *playground) init() {
	p.mode, p.via, p.n = modeGenerate, viaEngine, "1"
	p.prompt = "一张赛博朋克风格的雨夜街道，霓虹灯倒映在积水中，电影感构图"
}

// addImage appends a reference image unless it is already listed.
func (p *playground) addImage(path string) {
	for _, x := range p.images {
		if x == path {
			return
		}
	}
	p.images = append(p.images, path)
}

// refreshChoices rebuilds the model list when the configuration or mode
// changed: the default entry, then every enabled model with the capability.
func (m *model) refreshChoices() {
	pg := &m.pg
	key := fmt.Sprint(m.cfgVersion, pg.mode)
	if key == pg.choicesKey {
		return
	}
	pg.choicesKey = key
	capability := string(provider.Generate)
	if pg.mode == modeEdit {
		capability = string(provider.Edit)
	}
	pg.choices = []modelChoice{{}}
	found := pg.choice == modelChoice{}
	for _, info := range m.core.Engine.Models("", capability) {
		mc := modelChoice{Provider: info.Provider, Model: info.ID}
		pg.choices = append(pg.choices, mc)
		found = found || mc == pg.choice
	}
	if !found {
		pg.choice = modelChoice{}
	}
}

// choiceLabel names a model choice in the select.
func (m *model) choiceLabel(mc modelChoice) string {
	if mc.Model == "" {
		def := m.core.Engine.Config().Defaults.Model
		if def == "" {
			return "默认模型（第一个可用）"
		}
		return "默认模型（" + def + "）"
	}
	return mc.Model + "  ·  " + mc.Provider
}

// qualityLabel / styleLabel show "" as "默认".
func qualityLabel(s string) string { return orDefault(s) }

// orDefault shows "" as "默认".
func orDefault(s string) string {
	if s == "" {
		return "默认"
	}
	return s
}

// playgroundPage declares the request form, the run controls, the result
// grid and the debug view.
func (m *model) playgroundPage(c ui.C) {
	theme := c.Theme()
	pg := &m.pg
	m.refreshChoices()
	scaffold(c, "调试台", "直接测试生图和改图：「直连引擎」调用与 MCP 工具相同的内部逻辑；「经由 MCP 服务」通过本地 HTTP 端点完整走一遍 MCP 协议。可把图片拖进窗口作为参考图。", func() {
		card(c, "", func() {
			ui.Row(c).FillWidth().Gap(16).Items(ui.Center).Children(func() {
				ui.Choice(c, &pg.mode, modes, nil)
				ui.Choice(c, &pg.via, vias, nil)
				ui.Spacer(c)
				ui.Select(c, &pg.choice, pg.choices, m.choiceLabel).Width(340)
			})
			field(c, "Prompt", "", func() {
				ui.TextArea(c, &pg.prompt, "Prompt", "描述你想要的画面…").FillWidth().Height(96)
			})
			ui.Row(c).FillWidth().Gap(12).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					field(c, "尺寸", "", func() { ui.TextField(c, &pg.size, "尺寸", "默认").FillWidth() })
				})
				ui.Column(c).Width(80).Children(func() {
					field(c, "数量", "", func() { ui.TextField(c, &pg.n, "数量", "1").FillWidth() })
				})
				ui.Column(c).Width(130).Children(func() {
					field(c, "画质", "", func() { ui.Select(c, &pg.quality, qualities, qualityLabel).FillWidth() })
				})
				if pg.mode == modeGenerate {
					ui.Column(c).Width(120).Children(func() {
						field(c, "风格", "", func() { ui.Select(c, &pg.style, styles, orDefault).FillWidth() })
					})
				}
				ui.Column(c).Width(130).Children(func() {
					field(c, "Seed", "", func() { ui.TextField(c, &pg.seed, "Seed", "随机").FillWidth() })
				})
			})
			ui.Row(c).FillWidth().Gap(12).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					field(c, "反向提示词（可选）", "", func() {
						ui.TextField(c, &pg.negative, "反向提示词", "SiliconFlow 等支持").FillWidth()
					})
				})
				ui.Column(c).Grow(1).Children(func() {
					field(c, "extra（JSON，可选）", "", func() {
						ui.TextField(c, &pg.extra, "extra", `{"output_format":"webp"}`).FillWidth()
					})
				})
			})
			if pg.mode == modeEdit {
				m.referenceImages(c)
			}
			ui.Row(c).FillWidth().Gap(12).Items(ui.Center).Children(func() {
				if pg.running {
					if ui.Button(c, "取消").DestructiveOutline().Clicked() && pg.cancel != nil {
						pg.cancel()
					}
					ui.Spinner(c)
					ui.Text(c, fmt.Sprintf("%s（%.0fs）", pg.progress, time.Since(pg.started).Seconds())).Muted().Grow(1).Ellipsis()
				} else {
					label := "生成图片"
					if pg.mode == modeEdit {
						label = "编辑图片"
					}
					if ui.Button(c, label).Clicked() {
						c.After(m.runPlayground)
					}
					if pg.via == viaMCP && m.status.State != mcpserver.StateRunning {
						ui.Text(c, "需要先启动 MCP 服务").Color(theme.Danger)
					}
				}
			})
		})
		if pg.result != nil {
			m.playgroundResult(c, pg.result)
		}
	})
}

// referenceImages declares the edit-mode reference image strip and mask.
func (m *model) referenceImages(c ui.C) {
	theme := c.Theme()
	pg := &m.pg
	field(c, "参考图", "支持 PNG / JPEG / WebP，可多选，也可直接拖入窗口。", func() {
		ui.Flow(c).FillWidth().Gap(8).Children(func() {
			for i, path := range pg.images {
				ui.Column(c.Key(path)).Gap(4).Width(112).Children(func() {
					m.imageBox(c, path, 112, 112)
					ui.Text(c, filepath.Base(path)).FontSize(11).Muted().Ellipsis()
					if ui.Button(c, "移除").XSmall().Ghost().Clicked() {
						idx := i
						c.After(func() { pg.images = append(pg.images[:idx], pg.images[idx+1:]...) })
					}
				})
			}
			if ui.Button(c, "+ 选择图片").Outline().Clicked() {
				c.After(func() {
					paths, err := app.OpenFiles(app.OpenFileOptions{Title: "选择参考图", AllowMultiple: true, Filters: imageFilters})
					if err == nil {
						for _, p := range paths {
							pg.addImage(p)
						}
					}
				})
			}
		})
	})
	ui.Row(c).FillWidth().Gap(8).Items(ui.Center).Children(func() {
		ui.Text(c, "蒙版（可选）").FontSize(13).Weight(gfx.WeightMedium)
		if pg.mask == "" {
			ui.Text(c, "未选择：透明区域表示要修改的部分").Muted().FontSize(theme.Metrics.FontSizeS).Grow(1)
		} else {
			ui.Text(c, shortPath(pg.mask)).Mono().FontSize(12).Grow(1).Ellipsis()
			if ui.Button(c, "清除").Small().Ghost().Clicked() {
				pg.mask = ""
			}
		}
		if ui.Button(c, "选择蒙版").Small().Outline().Clicked() {
			c.After(func() {
				paths, err := app.OpenFiles(app.OpenFileOptions{Title: "选择蒙版 PNG", Filters: []app.FileFilter{{Name: "PNG", Pattern: "*.png"}}})
				if err == nil && len(paths) > 0 {
					pg.mask = paths[0]
				}
			})
		}
	})
}

// imageFilters limit the open panel to supported images.
var imageFilters = []app.FileFilter{{Name: "图片", Pattern: "*.png;*.jpg;*.jpeg;*.webp"}}

// playgroundResult declares the outcome: error text, or the image grid with
// per-image actions, then the optional debug lines.
func (m *model) playgroundResult(c ui.C, r *pgResult) {
	theme := c.Theme()
	card(c, "结果", func() {
		if r.err != "" {
			ui.Paragraph(c, r.err, 8).Color(theme.Danger)
		} else {
			ui.Text(c, r.summary).Muted()
			ui.Flow(c).FillWidth().Gap(12).Children(func() {
				for _, img := range r.images {
					path := img.Path
					ui.Column(c.Key(path)).Gap(6).Width(240).Children(func() {
						m.imageBox(c, path, 240, 240)
						ui.Text(c, filepath.Base(path)).FontSize(11).Mono().Ellipsis()
						ui.Text(c, fmt.Sprintf("%d×%d · %s · %dKB", img.Width, img.Height, strings.TrimPrefix(img.MIME, "image/"), img.Bytes/1024)).Muted().FontSize(11)
						ui.Row(c).Gap(4).Children(func() {
							if ui.Button(c, "打开").XSmall().Outline().Clicked() {
								m.open(path)
							}
							if ui.Button(c, fileManager).XSmall().Ghost().Clicked() {
								m.reveal(path)
							}
							if ui.Button(c, "复制路径").XSmall().Ghost().Clicked() {
								m.copy(path, "路径已复制")
							}
							if ui.Button(c, "编辑").XSmall().Ghost().Clicked() {
								m.pg.mode = modeEdit
								m.pg.addImage(path)
							}
						})
					})
				}
			})
		}
		if len(r.debug) > 0 {
			ui.Switch(c, &m.pg.showDebug, "显示请求 / 响应详情（已脱敏）")
			if m.pg.showDebug {
				codeBlock(c, r.debug)
			}
		}
	})
}

// runPlayground validates the form, then runs the call in the background via
// the engine or the local MCP endpoint. Progress and the result come back
// through post; a ticker repaints once a second so the elapsed time moves.
func (m *model) runPlayground() {
	pg := &m.pg
	req, err := m.playgroundRequest()
	if err != nil {
		pg.result = &pgResult{err: err.Error()}
		return
	}
	if pg.via == viaMCP && m.status.State != mcpserver.StateRunning {
		pg.result = &pgResult{err: "MCP 服务未启动：请先在「服务」页启动，或改用「直连引擎」。"}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pg.runID++
	id := pg.runID
	pg.running, pg.cancel, pg.started, pg.progress, pg.result = true, cancel, time.Now(), "准备中", nil
	progress := func(msg string) {
		m.post(func() {
			if pg.runID == id {
				pg.progress = msg
			}
		})
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.inv.Invalidate()
			}
		}
	}()
	via, mode := pg.via, pg.mode
	status := m.status
	go func() {
		defer close(done)
		var res *pgResult
		if via == viaMCP {
			res = m.callViaMCP(ctx, status, mode, req, progress, id)
		} else {
			res = m.callEngine(ctx, mode, req, progress)
		}
		m.post(func() {
			if pg.runID != id {
				return
			}
			cancel()
			pg.running, pg.cancel, pg.result = false, nil, res
			m.gal.stale = true
			for _, img := range res.images {
				m.thumbs.forget(img.Path)
			}
		})
	}()
}

// playgroundRequest turns the form into an engine edit request (generation
// uses its embedded GenerateRequest), parsing n, seed and extra JSON.
func (m *model) playgroundRequest() (engine.EditRequest, error) {
	pg := &m.pg
	var req engine.EditRequest
	req.Prompt = strings.TrimSpace(pg.prompt)
	if req.Prompt == "" {
		return req, errors.New("请填写 Prompt")
	}
	req.Model, req.Provider = pg.choice.Model, pg.choice.Provider
	req.Size = strings.TrimSpace(pg.size)
	if s := strings.TrimSpace(pg.n); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 10 {
			return req, errors.New("数量必须是 1-10 的整数")
		}
		req.N = n
	}
	if s := strings.TrimSpace(pg.seed); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return req, errors.New("Seed 必须是整数")
		}
		req.Seed = &v
	}
	req.Quality = pg.quality
	if pg.mode == modeGenerate {
		req.Style = pg.style
	}
	req.NegativePrompt = strings.TrimSpace(pg.negative)
	if s := strings.TrimSpace(pg.extra); s != "" {
		if err := json.Unmarshal([]byte(s), &req.Extra); err != nil {
			return req, fmt.Errorf("extra 不是有效的 JSON 对象: %v", err)
		}
	}
	req.Source = "debug"
	if pg.mode == modeEdit {
		if len(pg.images) == 0 {
			return req, errors.New("编辑模式需要至少一张参考图")
		}
		req.Images = append([]string(nil), pg.images...)
		req.Mask = pg.mask
	}
	return req, nil
}

// callEngine runs the request through the engine directly and formats the
// debug lines from the HTTP exchange.
func (m *model) callEngine(ctx context.Context, mode string, req engine.EditRequest, progress engine.Progress) *pgResult {
	var res *engine.Result
	var err error
	if mode == modeEdit {
		res, err = m.core.Engine.Edit(ctx, req, progress)
	} else {
		res, err = m.core.Engine.Generate(ctx, req.GenerateRequest, progress)
	}
	if err != nil {
		out := &pgResult{err: err.Error()}
		var f *engine.Failure
		if errors.As(err, &f) {
			out.debug = debugLines(f.Debug, f.Attempts)
		}
		return out
	}
	out := &pgResult{images: res.Images, debug: debugLines(res.Debug, res.Attempts)}
	out.summary = fmt.Sprintf("%d 张 · %s · %s · %.1fs", len(res.Images), res.Model, res.Provider, res.Elapsed.Seconds())
	if res.RevisedPrompt != "" {
		out.summary += " · 改写后：" + res.RevisedPrompt
	}
	return out
}

// debugLines formats one HTTP exchange and the attempts for the code block.
func debugLines(d imageapi.Debug, attempts []engine.Attempt) []string {
	var b strings.Builder
	if d.URL != "" {
		fmt.Fprintf(&b, "%s %s\n", d.Method, d.URL)
		fmt.Fprintf(&b, "HTTP %d · %.2fs\n\n", d.Status, d.Elapsed.Seconds())
		b.WriteString("── 请求 ──\n")
		b.WriteString(d.RequestBody)
		b.WriteString("\n\n── 响应 ──\n")
		b.WriteString(d.ResponseBody)
		b.WriteString("\n")
	}
	if len(attempts) > 0 {
		b.WriteString("\n── 尝试记录 ──\n")
		for i, a := range attempts {
			status := "成功"
			if a.Error != "" {
				status = a.Error
			}
			fmt.Fprintf(&b, "%d. %s / %s · %.1fs · %s\n", i+1, a.Provider, a.Model, a.Elapsed.Seconds(), status)
		}
	}
	return splitLines(imageapi.RedactText(b.String()))
}

// callViaMCP connects to the running local MCP endpoint with the official
// client, calls the tool and parses its structured result: a true end-to-end
// check of what Claude or Codex would see. When the call outlasts the
// server's wait budget and returns status "running", it keeps calling
// get_image_job with the job_id until the job is done, as an agent would.
func (m *model) callViaMCP(ctx context.Context, st mcpserver.Status, mode string, req engine.EditRequest, progress engine.Progress, id int) *pgResult {
	client := mcp.NewClient(&mcp.Implementation{Name: "genimage-debug", Version: mcpserver.Version}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			if r.Params != nil && r.Params.Message != "" {
				progress("[MCP] " + r.Params.Message)
			}
		},
	})
	hc := &http.Client{}
	if tok := st.Server.AuthToken; tok != "" {
		hc.Transport = bearerTransport{token: tok}
	}
	progress("正在连接 " + st.URL)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: st.URL, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		return &pgResult{err: "连接 MCP 服务失败: " + err.Error()}
	}
	defer cs.Close()

	tool, args := "generate_image", map[string]any{"prompt": req.Prompt}
	if mode == modeEdit {
		tool = "edit_image"
		args["images"] = req.Images
		if req.Mask != "" {
			args["mask"] = req.Mask
		}
	}
	set := func(k, v string) {
		if v != "" {
			args[k] = v
		}
	}
	set("model", req.Model)
	set("provider", req.Provider)
	set("size", req.Size)
	set("quality", req.Quality)
	set("style", req.Style)
	set("negative_prompt", req.NegativePrompt)
	if mode == modeEdit {
		delete(args, "style")
		delete(args, "negative_prompt")
	}
	if req.N > 0 {
		args["n"] = req.N
	}
	if req.Seed != nil {
		args["seed"] = *req.Seed
	}
	if len(req.Extra) > 0 {
		args["extra"] = req.Extra
	}
	argJSON, _ := json.MarshalIndent(args, "", "  ")
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	params.SetProgressToken(fmt.Sprintf("debug-%d", id))
	progress("正在调用 " + tool)
	res, err := cs.CallTool(ctx, params)
	debug := []string{"MCP tools/call → " + st.URL, " ", "── 参数 ──"}
	debug = append(debug, splitLines(string(argJSON))...)
	if err != nil {
		return &pgResult{err: "调用失败: " + err.Error(), debug: debug}
	}
	debug = append(debug, " ", "── 返回内容 ──")
	var text strings.Builder
	images := 0
	for _, ct := range res.Content {
		switch v := ct.(type) {
		case *mcp.TextContent:
			text.WriteString(v.Text)
		case *mcp.ImageContent:
			images++
		}
	}
	debug = append(debug, splitLines(text.String())...)
	debug = append(debug, fmt.Sprintf("（另含 %d 个 ImageContent 预览）", images))
	if res.IsError {
		return &pgResult{err: text.String(), debug: debug}
	}
	var out mcpserver.ImageResult
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		return &pgResult{err: "无法解析 structuredContent: " + err.Error(), debug: debug}
	}
	if out.Status == "running" {
		var errText string
		out, debug, errText = m.followJob(ctx, cs, out.JobID, progress, id, debug)
		if errText != "" {
			return &pgResult{err: errText, debug: debug}
		}
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	debug = append(debug, " ", "── structuredContent ──")
	debug = append(debug, splitLines(string(pretty))...)
	saved := make([]storage.Saved, 0, len(out.Images))
	for _, img := range out.Images {
		saved = append(saved, storage.Saved{Path: img.Path, Width: img.Width, Height: img.Height, Bytes: img.Bytes, MIME: img.MIME})
	}
	return &pgResult{
		images: saved, debug: debug,
		summary: fmt.Sprintf("%d 张 · %s · %s · 服务端耗时 %.1fs（经由 MCP）", len(saved), out.Model, out.Provider, float64(out.ElapsedMS)/1000),
	}
}

// followJob polls get_image_job until the job started by a tool call that
// returned status "running" is done, appending each round to debug, and
// converts the job's single task into the ImageResult the tool would have
// returned had it finished in time.
//
// ctx cancels the polling (the debug console's cancel button); cs is the
// connected MCP session; jobID is from the running result; progress shows
// status; id makes the progress tokens unique; debug is the transcript so
// far. It returns the final result, the extended transcript, and an error
// text ("" on success) for a failed task or call.
func (m *model) followJob(ctx context.Context, cs *mcp.ClientSession, jobID string, progress engine.Progress, id int, debug []string) (mcpserver.ImageResult, []string, string) {
	for round := 1; ; round++ {
		progress(fmt.Sprintf("[MCP] 服务端仍在后台生成，第 %d 次调用 get_image_job（%s）", round, jobID))
		params := &mcp.CallToolParams{Name: "get_image_job", Arguments: map[string]any{"job_id": jobID, "include_preview": false}}
		params.SetProgressToken(fmt.Sprintf("debug-%d-job-%d", id, round))
		res, err := cs.CallTool(ctx, params)
		if err != nil {
			return mcpserver.ImageResult{}, debug, "get_image_job 调用失败: " + err.Error()
		}
		var text strings.Builder
		for _, ct := range res.Content {
			if v, ok := ct.(*mcp.TextContent); ok {
				text.WriteString(v.Text)
			}
		}
		debug = append(debug, " ", fmt.Sprintf("── get_image_job 第 %d 次 ──", round))
		debug = append(debug, splitLines(text.String())...)
		var br mcpserver.BatchResult
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, &br); err != nil {
			return mcpserver.ImageResult{}, debug, "无法解析 get_image_job 的 structuredContent: " + err.Error()
		}
		if br.Status == "running" {
			continue
		}
		if len(br.Items) == 0 {
			return mcpserver.ImageResult{}, debug, "get_image_job 没有返回任务结果"
		}
		it := br.Items[0]
		if !it.OK {
			return mcpserver.ImageResult{}, debug, it.Error
		}
		return mcpserver.ImageResult{
			Status: "done", Images: it.Images, Model: it.Model, Provider: it.Provider,
			RevisedPrompt: it.RevisedPrompt, Seed: it.Seed, ElapsedMS: br.ElapsedMS, Attempts: it.Attempts,
		}, debug, ""
	}
}

// bearerTransport adds the MCP server token to every request.
type bearerTransport struct{ token string }

// RoundTrip sets the Authorization header on a copy of r.
func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
