// Package mcpserver exposes the engine as MCP tools over Streamable HTTP and
// manages the HTTP server's lifecycle for the GUI.
package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/engine"
	"github.com/972579/open-image-mcp/internal/provider"
	"github.com/972579/open-image-mcp/internal/storage"
	"github.com/972579/open-image-mcp/internal/version"
)

// Version is reported to MCP clients as the server version.
const Version = version.Version

// maxPreviews caps how many ImageContent previews one tool result carries.
const maxPreviews = 4

// GenerateInput is generate_image's arguments. Fields without omitempty are
// required in the inferred JSON schema.
type GenerateInput struct {
	Prompt         string         `json:"prompt" jsonschema:"图像描述（必填），建议写清主体、风格、构图、光线"`
	Model          string         `json:"model,omitempty" jsonschema:"模型 ID，留空使用默认模型；可先调用 list_models 查看"`
	Provider       string         `json:"provider,omitempty" jsonschema:"指定服务商名称；留空则按配置顺序自动选择并在失败时回退"`
	Size           string         `json:"size,omitempty" jsonschema:"图片尺寸，如 1024x1024、1536x1024、1024x1536 或 auto"`
	N              int            `json:"n,omitempty" jsonschema:"生成数量 1-10，默认 1"`
	Quality        string         `json:"quality,omitempty" jsonschema:"画质：gpt-image 用 auto/low/medium/high，dall-e-3 用 standard/hd"`
	Style          string         `json:"style,omitempty" jsonschema:"风格（仅 dall-e-3）：vivid 或 natural"`
	Seed           *int64         `json:"seed,omitempty" jsonschema:"随机种子，上游支持时可复现结果"`
	ResponseFormat string         `json:"response_format,omitempty" jsonschema:"url 或 b64_json；默认不发送（gpt-image 系列不接受该参数）"`
	OutputFormat   string         `json:"output_format,omitempty" jsonschema:"输出格式（gpt-image）：png、jpeg 或 webp"`
	Background     string         `json:"background,omitempty" jsonschema:"背景（gpt-image）：transparent、opaque 或 auto"`
	NegativePrompt string         `json:"negative_prompt,omitempty" jsonschema:"反向提示词（SiliconFlow 等支持）"`
	OutputDir      string         `json:"output_dir,omitempty" jsonschema:"保存目录（绝对路径），留空使用默认目录"`
	FilenamePrefix string         `json:"filename_prefix,omitempty" jsonschema:"文件名前缀"`
	IncludePreview *bool          `json:"include_preview,omitempty" jsonschema:"是否在结果中附带缩略图预览，默认按配置"`
	Extra          map[string]any `json:"extra,omitempty" jsonschema:"原样合并进上游请求体的额外参数（各家私有参数兜底）"`
}

// EditInput is edit_image's arguments.
type EditInput struct {
	Prompt         string         `json:"prompt" jsonschema:"编辑指令（必填），描述要如何修改参考图"`
	Images         []string       `json:"images" jsonschema:"参考图（必填，1-16 张）：本机绝对路径、data URL 或 base64；可直接使用 generate_image 返回的 path"`
	Mask           string         `json:"mask,omitempty" jsonschema:"可选蒙版（PNG，透明区域为要修改的部分），格式同 images"`
	Model          string         `json:"model,omitempty" jsonschema:"支持编辑的模型 ID，留空使用默认模型"`
	Provider       string         `json:"provider,omitempty" jsonschema:"指定服务商名称"`
	Size           string         `json:"size,omitempty" jsonschema:"输出尺寸"`
	N              int            `json:"n,omitempty" jsonschema:"生成数量 1-10"`
	Quality        string         `json:"quality,omitempty" jsonschema:"画质"`
	Seed           *int64         `json:"seed,omitempty" jsonschema:"随机种子"`
	ResponseFormat string         `json:"response_format,omitempty" jsonschema:"url 或 b64_json；默认不发送"`
	OutputFormat   string         `json:"output_format,omitempty" jsonschema:"输出格式：png、jpeg 或 webp"`
	Background     string         `json:"background,omitempty" jsonschema:"背景：transparent、opaque 或 auto"`
	OutputDir      string         `json:"output_dir,omitempty" jsonschema:"保存目录（绝对路径）"`
	FilenamePrefix string         `json:"filename_prefix,omitempty" jsonschema:"文件名前缀"`
	IncludePreview *bool          `json:"include_preview,omitempty" jsonschema:"是否附带缩略图预览"`
	Extra          map[string]any `json:"extra,omitempty" jsonschema:"额外上游参数"`
}

// ImageOut is one saved image in a tool result.
type ImageOut struct {
	Path          string `json:"path" jsonschema:"图片的本机绝对路径，可传给 edit_image 或其他工具"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Bytes         int    `json:"bytes"`
	MIME          string `json:"mime"`
	SourceURL     string `json:"source_url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// AttemptOut is one upstream try in a tool result.
type AttemptOut struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Error     string `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// ImageResult is the structured result of generate_image and edit_image.
type ImageResult struct {
	Images        []ImageOut   `json:"images"`
	Model         string       `json:"model"`
	Provider      string       `json:"provider"`
	RevisedPrompt string       `json:"revised_prompt,omitempty"`
	Seed          *int64       `json:"seed,omitempty"`
	ElapsedMS     int64        `json:"elapsed_ms"`
	Attempts      []AttemptOut `json:"attempts,omitempty"`
}

// ListModelsInput is list_models' arguments.
type ListModelsInput struct {
	Provider   string `json:"provider,omitempty" jsonschema:"只列出该服务商的模型"`
	Capability string `json:"capability,omitempty" jsonschema:"按能力过滤：generate 或 edit"`
}

// ListModelsOutput is list_models' result.
type ListModelsOutput struct {
	DefaultModel string               `json:"default_model"`
	Models       []provider.ModelInfo `json:"models"`
}

// ListImagesInput is list_generated_images' arguments.
type ListImagesInput struct {
	Limit   int    `json:"limit,omitempty" jsonschema:"返回条数，默认 20，最大 100"`
	Offset  int    `json:"offset,omitempty" jsonschema:"跳过的条数（最新的在前）"`
	Model   string `json:"model,omitempty" jsonschema:"按模型 ID 过滤"`
	Keyword string `json:"keyword,omitempty" jsonschema:"在 prompt / 模型 / 路径中搜索的关键词"`
	Since   string `json:"since,omitempty" jsonschema:"只返回该时间之后的图片，RFC3339 格式，如 2026-11-18T00:00:00+08:00"`
}

// ImageRecordOut is one history entry in list_generated_images.
type ImageRecordOut struct {
	Path      string `json:"path"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	Size      string `json:"size,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Exists    bool   `json:"exists" jsonschema:"文件当前是否仍在磁盘上"`
	CreatedAt string `json:"created_at"`
	Source    string `json:"source"`
}

// ListImagesOutput is list_generated_images' result.
type ListImagesOutput struct {
	Total int              `json:"total"`
	Items []ImageRecordOut `json:"items"`
}

// NewServer builds an MCP server with the four image tools bound to eng.
// Each tool call reads eng's current configuration, so saving the config in
// the GUI takes effect without rebuilding the server.
//
// onCall, when not nil, is told the tool name each time a tool is invoked (the
// GUI counts calls with it). It returns an error only if a schema cannot be
// built, which indicates a programming mistake.
func NewServer(eng *engine.Engine, onCall func(tool string)) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "gen-openai-image", Title: "GenImage MCP", Version: Version}, &mcp.ServerOptions{
		Instructions: "图像生成工具：generate_image 文生图、edit_image 图生图/编辑、list_models 查看可用模型、list_generated_images 查看历史。生成的图片保存在本机，结果中的 path 可直接给后续工具使用。",
	})
	count := func(name string) {
		if onCall != nil {
			onCall(name)
		}
	}

	genSchema, err := jsonschema.For[GenerateInput](nil)
	if err != nil {
		return nil, fmt.Errorf("generate_image schema: %w", err)
	}
	setEnums(genSchema)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "generate_image",
		Title:       "生成图片",
		Description: "根据文字描述调用 OpenAI 兼容的图像接口生成图片，保存到本机并返回绝对路径和缩略图预览。适用于「画一张…」「生成一张…的图片」之类的请求。",
		InputSchema: genSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in GenerateInput) (*mcp.CallToolResult, ImageResult, error) {
		count("generate_image")
		stop := startProgress(ctx, req)
		res, err := eng.Generate(ctx, in.request(), stop.report)
		stop.done()
		if err != nil {
			return nil, ImageResult{}, err
		}
		return imageResult(res, previewWanted(eng, in.IncludePreview), eng.Config().Defaults.PreviewMaxPx)
	})

	editSchema, err := jsonschema.For[EditInput](nil)
	if err != nil {
		return nil, fmt.Errorf("edit_image schema: %w", err)
	}
	setEnums(editSchema)
	if p := editSchema.Properties["images"]; p != nil {
		one, sixteen := 1, 16
		p.MinItems, p.MaxItems = &one, &sixteen
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_image",
		Title:       "编辑图片",
		Description: "以一张或多张参考图为基础按指令编辑/重绘（图生图），需要模型支持编辑。参考图可用本机绝对路径（例如 generate_image 返回的 path）或 base64。",
		InputSchema: editSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in EditInput) (*mcp.CallToolResult, ImageResult, error) {
		count("edit_image")
		stop := startProgress(ctx, req)
		res, err := eng.Edit(ctx, in.request(), stop.report)
		stop.done()
		if err != nil {
			return nil, ImageResult{}, err
		}
		return imageResult(res, previewWanted(eng, in.IncludePreview), eng.Config().Defaults.PreviewMaxPx)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_models",
		Title:       "列出图像模型",
		Description: "列出当前配置中可用的图像模型、所属服务商以及是否支持生成/编辑。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListModelsInput) (*mcp.CallToolResult, ListModelsOutput, error) {
		count("list_models")
		capability := strings.TrimSpace(in.Capability)
		if capability != "" && capability != "generate" && capability != "edit" {
			return nil, ListModelsOutput{}, fmt.Errorf("capability 只能是 generate 或 edit")
		}
		models := eng.Models(in.Provider, capability)
		if models == nil {
			models = []provider.ModelInfo{}
		}
		out := ListModelsOutput{DefaultModel: eng.Config().Defaults.Model, Models: models}
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 个模型（默认：%s）\n", len(models), orNone(out.DefaultModel))
		for _, m := range models {
			caps := []string{}
			if m.Generate {
				caps = append(caps, "生成")
			}
			if m.Edit {
				caps = append(caps, "编辑")
			}
			fmt.Fprintf(&b, "- %s（%s）[%s]\n", m.ID, m.Provider, strings.Join(caps, "/"))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_generated_images",
		Title:       "查看已生成图片",
		Description: "按时间倒序列出本服务已生成并保存的图片（路径、prompt、模型），可按模型、关键词、时间过滤。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListImagesInput) (*mcp.CallToolResult, ListImagesOutput, error) {
		count("list_generated_images")
		q := storage.Query{Limit: in.Limit, Offset: in.Offset, Model: in.Model, Keyword: in.Keyword}
		if q.Limit > 100 {
			q.Limit = 100
		}
		if s := strings.TrimSpace(in.Since); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, ListImagesOutput{}, fmt.Errorf("since 必须是 RFC3339 时间，例如 2026-11-18T00:00:00+08:00")
			}
			q.Since = t
		}
		total, recs, err := eng.History(q)
		if err != nil {
			return nil, ListImagesOutput{}, fmt.Errorf("读取历史记录失败: %w", err)
		}
		out := ListImagesOutput{Total: total, Items: make([]ImageRecordOut, 0, len(recs))}
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 张，本页 %d 张：\n", total, len(recs))
		for _, r := range recs {
			_, statErr := os.Stat(r.Path)
			out.Items = append(out.Items, ImageRecordOut{
				Path: r.Path, Prompt: r.Prompt, Model: r.Model, Provider: r.Provider, Kind: r.Kind,
				Size: r.Size, Width: r.Width, Height: r.Height, Exists: statErr == nil,
				CreatedAt: r.CreatedAt.Format(time.RFC3339), Source: r.Source,
			})
			fmt.Fprintf(&b, "- %s  %s  %s\n", r.CreatedAt.Format("2006-01-02 15:04"), r.Path, oneLine(r.Prompt, 60))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, out, nil
	})
	return srv, nil
}

// setEnums restricts the closed-set string parameters in a tool schema.
func setEnums(s *jsonschema.Schema) {
	enums := map[string][]any{
		"quality":         {"auto", "low", "medium", "high", "standard", "hd"},
		"style":           {"vivid", "natural"},
		"response_format": {"url", "b64_json"},
		"output_format":   {"png", "jpeg", "webp"},
		"background":      {"transparent", "opaque", "auto"},
	}
	for name, values := range enums {
		if p := s.Properties[name]; p != nil {
			p.Enum = values
		}
	}
	if p := s.Properties["n"]; p != nil {
		lo, hi := 1.0, 10.0
		p.Minimum, p.Maximum = &lo, &hi
	}
}

// request converts tool arguments into an engine request.
func (in GenerateInput) request() engine.GenerateRequest {
	return engine.GenerateRequest{
		Prompt: in.Prompt, Model: in.Model, Provider: in.Provider, Size: in.Size, N: in.N,
		Quality: in.Quality, Style: in.Style, Seed: in.Seed, ResponseFormat: in.ResponseFormat,
		OutputFormat: in.OutputFormat, Background: in.Background, NegativePrompt: in.NegativePrompt,
		OutputDir: in.OutputDir, FilenamePrefix: in.FilenamePrefix, Extra: in.Extra, Source: "mcp",
	}
}

// request converts tool arguments into an engine edit request.
func (in EditInput) request() engine.EditRequest {
	return engine.EditRequest{
		GenerateRequest: engine.GenerateRequest{
			Prompt: in.Prompt, Model: in.Model, Provider: in.Provider, Size: in.Size, N: in.N,
			Quality: in.Quality, Seed: in.Seed, ResponseFormat: in.ResponseFormat,
			OutputFormat: in.OutputFormat, Background: in.Background,
			OutputDir: in.OutputDir, FilenamePrefix: in.FilenamePrefix, Extra: in.Extra, Source: "mcp",
		},
		Images: in.Images,
		Mask:   in.Mask,
	}
}

// previewWanted resolves include_preview against the configured default.
func previewWanted(eng *engine.Engine, v *bool) bool {
	if v != nil {
		return *v
	}
	return eng.Config().Defaults.IncludePreview
}

// imageResult builds the tool result: a text summary listing every absolute
// path, up to four JPEG previews, and the structured output.
//
// res is the engine result; preview says whether to attach previews; maxPx is
// the preview's longer side.
func imageResult(res *engine.Result, preview bool, maxPx int) (*mcp.CallToolResult, ImageResult, error) {
	out := ImageResult{
		Model: res.Model, Provider: res.Provider, RevisedPrompt: res.RevisedPrompt, Seed: res.Seed,
		ElapsedMS: res.Elapsed.Milliseconds(),
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已生成 %d 张图片（模型 %s，服务商 %s，耗时 %.1fs）：\n", len(res.Images), res.Model, res.Provider, res.Elapsed.Seconds())
	content := []mcp.Content{}
	for i, img := range res.Images {
		out.Images = append(out.Images, ImageOut{
			Path: img.Path, Width: img.Width, Height: img.Height, Bytes: img.Bytes, MIME: img.MIME,
			SourceURL: img.SourceURL, RevisedPrompt: img.RevisedPrompt,
		})
		fmt.Fprintf(&b, "%d. %s (%dx%d)\n", i+1, img.Path, img.Width, img.Height)
	}
	if res.RevisedPrompt != "" {
		fmt.Fprintf(&b, "改写后的提示词：%s\n", res.RevisedPrompt)
	}
	for _, a := range res.Attempts {
		out.Attempts = append(out.Attempts, AttemptOut{Provider: a.Provider, Model: a.Model, Error: a.Error, ElapsedMS: a.Elapsed.Milliseconds()})
	}
	content = append(content, &mcp.TextContent{Text: b.String()})
	if preview {
		for i, img := range res.Images {
			if i >= maxPreviews {
				break
			}
			data, err := os.ReadFile(img.Path)
			if err != nil {
				continue
			}
			jpg, err := storage.Preview(data, maxPx)
			if err != nil {
				continue
			}
			content = append(content, &mcp.ImageContent{Data: jpg, MIMEType: "image/jpeg"})
		}
	}
	return &mcp.CallToolResult{Content: content}, out, nil
}

// progressTicker sends MCP progress notifications while a slow tool runs: each
// engine status message, plus a heartbeat every five seconds so clients with
// short request timeouts keep waiting.
type progressTicker struct {
	ctx   context.Context
	req   *mcp.CallToolRequest
	token any
	mu    sync.Mutex
	step  float64
	last  string
	stop  chan struct{}
	once  sync.Once
	start time.Time
}

// startProgress begins progress reporting if the client sent a progress token;
// otherwise the returned ticker's methods do nothing.
func startProgress(ctx context.Context, req *mcp.CallToolRequest) *progressTicker {
	t := &progressTicker{ctx: ctx, req: req, stop: make(chan struct{}), start: time.Now()}
	if req == nil || req.Params == nil || req.Session == nil {
		return t
	}
	t.token = req.Params.GetProgressToken()
	if t.token == nil {
		return t
	}
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				t.mu.Lock()
				msg := t.last
				t.mu.Unlock()
				t.send(fmt.Sprintf("%s（已等待 %ds）", msg, int(time.Since(t.start).Seconds())))
			}
		}
	}()
	return t
}

// report records an engine status message and forwards it to the client.
func (t *progressTicker) report(msg string) {
	t.mu.Lock()
	t.last = msg
	t.mu.Unlock()
	t.send(msg)
}

// send emits one progress notification with a monotonically increasing value.
func (t *progressTicker) send(msg string) {
	if t.token == nil {
		return
	}
	t.mu.Lock()
	t.step++
	step := t.step
	t.mu.Unlock()
	_ = t.req.Session.NotifyProgress(t.ctx, &mcp.ProgressNotificationParams{ProgressToken: t.token, Progress: step, Message: msg})
}

// done stops the heartbeat.
func (t *progressTicker) done() { t.once.Do(func() { close(t.stop) }) }

// orNone shows "（未设置）" for an empty string.
func orNone(s string) string {
	if s == "" {
		return "（未设置）"
	}
	return s
}

// oneLine flattens and truncates text for one-line listings.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
