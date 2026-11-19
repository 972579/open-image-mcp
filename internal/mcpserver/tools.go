// Package mcpserver exposes the engine as MCP tools over Streamable HTTP and
// manages the HTTP server's lifecycle for the GUI.
package mcpserver

import (
	"context"
	"errors"
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

// maxBatchPreviews caps the previews of a batch result: the first image of
// each successful task, up to this many in all.
const maxBatchPreviews = 8

// serverInstructions is sent to clients on initialize and steers the model
// towards the batch tool when it wants several images at once.
const serverInstructions = "图像生成工具：generate_image 文生图、batch_generate_images 一次提交多个提示词并发生成、edit_image 图生图/编辑、list_models 查看可用模型、list_generated_images 查看历史。需要多张不同内容的图片时，优先用 batch_generate_images 一次提交，服务端会并发生成，比逐个调用 generate_image 快得多。生成的图片保存在本机，结果中的 path 可直接给后续工具使用。"

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
	OutputDir      string         `json:"output_dir,omitempty" jsonschema:"保存目录（绝对路径），留空保存到默认图片目录；指定其他目录时，默认图片目录（图库）里也会保留一份副本"`
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
	OutputDir      string         `json:"output_dir,omitempty" jsonschema:"保存目录（绝对路径），留空保存到默认图片目录；指定其他目录时图库里也会保留一份副本"`
	FilenamePrefix string         `json:"filename_prefix,omitempty" jsonschema:"文件名前缀"`
	IncludePreview *bool          `json:"include_preview,omitempty" jsonschema:"是否附带缩略图预览"`
	Extra          map[string]any `json:"extra,omitempty" jsonschema:"额外上游参数"`
}

// ImageOut is one saved image in a tool result.
type ImageOut struct {
	Path          string `json:"path" jsonschema:"图片的本机绝对路径（在 output_dir 中），可传给 edit_image 或其他工具"`
	GalleryPath   string `json:"gallery_path,omitempty" jsonschema:"指定了 output_dir 时，图库（默认图片目录）中保留的副本路径"`
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

// BatchTaskInput is one task of batch_generate_images. Fields left empty fall
// back to the batch's shared parameters, then to the configured defaults.
type BatchTaskInput struct {
	Prompt         string         `json:"prompt" jsonschema:"该任务的图像描述（必填）"`
	Model          string         `json:"model,omitempty" jsonschema:"该任务的模型 ID，留空使用批次的 model 或默认模型"`
	Provider       string         `json:"provider,omitempty" jsonschema:"该任务指定的服务商名称"`
	Size           string         `json:"size,omitempty" jsonschema:"该任务的图片尺寸"`
	N              int            `json:"n,omitempty" jsonschema:"该任务生成数量 1-10"`
	Quality        string         `json:"quality,omitempty" jsonschema:"该任务的画质"`
	Style          string         `json:"style,omitempty" jsonschema:"该任务的风格（仅 dall-e-3）"`
	Seed           *int64         `json:"seed,omitempty" jsonschema:"该任务的随机种子"`
	OutputFormat   string         `json:"output_format,omitempty" jsonschema:"该任务的输出格式"`
	Background     string         `json:"background,omitempty" jsonschema:"该任务的背景"`
	NegativePrompt string         `json:"negative_prompt,omitempty" jsonschema:"该任务的反向提示词"`
	FilenamePrefix string         `json:"filename_prefix,omitempty" jsonschema:"该任务的文件名前缀"`
	Extra          map[string]any `json:"extra,omitempty" jsonschema:"该任务的额外上游参数，与批次的 extra 合并，同名键以任务为准"`
}

// BatchGenerateInput is batch_generate_images' arguments: the tasks plus
// parameters shared by every task that does not set its own.
type BatchGenerateInput struct {
	Tasks          []BatchTaskInput `json:"tasks" jsonschema:"要并发生成的任务（1-16 个），每个任务一个 prompt，可单独覆盖下面的公共参数"`
	Model          string           `json:"model,omitempty" jsonschema:"公共模型 ID，留空使用默认模型"`
	Provider       string           `json:"provider,omitempty" jsonschema:"公共服务商名称；留空则按配置顺序自动选择并在失败时回退"`
	Size           string           `json:"size,omitempty" jsonschema:"公共图片尺寸，如 1024x1024、1536x1024、1024x1536 或 auto"`
	N              int              `json:"n,omitempty" jsonschema:"每个任务的生成数量 1-10，默认 1"`
	Quality        string           `json:"quality,omitempty" jsonschema:"公共画质"`
	Style          string           `json:"style,omitempty" jsonschema:"公共风格（仅 dall-e-3）"`
	ResponseFormat string           `json:"response_format,omitempty" jsonschema:"url 或 b64_json；默认不发送"`
	OutputFormat   string           `json:"output_format,omitempty" jsonschema:"公共输出格式：png、jpeg 或 webp"`
	Background     string           `json:"background,omitempty" jsonschema:"公共背景：transparent、opaque 或 auto"`
	NegativePrompt string           `json:"negative_prompt,omitempty" jsonschema:"公共反向提示词"`
	OutputDir      string           `json:"output_dir,omitempty" jsonschema:"保存目录（绝对路径），所有任务共用；留空保存到默认图片目录，指定其他目录时图库里也会保留副本"`
	FilenamePrefix string           `json:"filename_prefix,omitempty" jsonschema:"公共文件名前缀"`
	IncludePreview *bool            `json:"include_preview,omitempty" jsonschema:"是否附带缩略图预览（每个成功任务的第一张，最多 8 张），默认按配置"`
	Extra          map[string]any   `json:"extra,omitempty" jsonschema:"所有任务共用的额外上游参数"`
}

// BatchItemOut is one task's outcome in a batch result.
type BatchItemOut struct {
	Index         int          `json:"index" jsonschema:"任务在 tasks 中的下标（从 0 开始）"`
	Prompt        string       `json:"prompt"`
	OK            bool         `json:"ok" jsonschema:"该任务是否成功"`
	Error         string       `json:"error,omitempty" jsonschema:"失败原因"`
	Images        []ImageOut   `json:"images,omitempty"`
	Model         string       `json:"model,omitempty"`
	Provider      string       `json:"provider,omitempty"`
	RevisedPrompt string       `json:"revised_prompt,omitempty"`
	Seed          *int64       `json:"seed,omitempty"`
	ElapsedMS     int64        `json:"elapsed_ms" jsonschema:"该任务耗时（含排队）"`
	Attempts      []AttemptOut `json:"attempts,omitempty"`
}

// BatchResult is the structured result of batch_generate_images.
type BatchResult struct {
	Items          []BatchItemOut `json:"items"`
	Succeeded      int            `json:"succeeded"`
	Failed         int            `json:"failed"`
	MaxConcurrency int            `json:"max_concurrency" jsonschema:"本次使用的上游并发上限（request.max_concurrency）"`
	ElapsedMS      int64          `json:"elapsed_ms" jsonschema:"整个批次的耗时"`
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
	Path       string `json:"path" jsonschema:"图库中的图片路径"`
	OutputPath string `json:"output_path,omitempty" jsonschema:"生成时指定了 output_dir 的话，保存在该目录中的那一份"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Kind       string `json:"kind"`
	Size       string `json:"size,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Exists     bool   `json:"exists" jsonschema:"文件当前是否仍在磁盘上"`
	CreatedAt  string `json:"created_at"`
	Source     string `json:"source"`
}

// ListImagesOutput is list_generated_images' result.
type ListImagesOutput struct {
	Total int              `json:"total"`
	Items []ImageRecordOut `json:"items"`
}

// NewServer builds an MCP server with the five image tools bound to eng.
// Each tool call reads eng's current configuration, so saving the config in
// the GUI takes effect without rebuilding the server.
//
// onCall, when not nil, is told the tool name each time a tool is invoked (the
// GUI counts calls with it). It returns an error only if a schema cannot be
// built, which indicates a programming mistake.
func NewServer(eng *engine.Engine, onCall func(tool string)) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "gen-openai-image", Title: "GenImage MCP", Version: Version}, &mcp.ServerOptions{
		Instructions: serverInstructions,
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
		Description: "根据文字描述调用 OpenAI 兼容的图像接口生成图片，保存到本机并返回绝对路径和缩略图预览。适用于「画一张…」「生成一张…的图片」之类的请求。一次要多张不同提示词的图片时，请改用 batch_generate_images 并发生成。",
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

	batchSchema, err := jsonschema.For[BatchGenerateInput](nil)
	if err != nil {
		return nil, fmt.Errorf("batch_generate_images schema: %w", err)
	}
	setEnums(batchSchema)
	if p := batchSchema.Properties["tasks"]; p != nil {
		one, most := 1, engine.MaxBatchTasks
		p.MinItems, p.MaxItems = &one, &most
		if p.Items != nil {
			setEnums(p.Items)
		}
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "batch_generate_images",
		Title: "批量并发生成图片",
		Description: fmt.Sprintf("一次提交多个文生图任务（1-%d 个，每个任务一个 prompt，可分别指定模型、尺寸等），服务端并发生成并等待全部完成，"+
			"比逐个调用 generate_image 快得多。适用于「画几张不同风格的…」「为每个场景各生成一张图」之类的请求。"+
			"上游并发数受配置 request.max_concurrency 限制，超出的任务排队。某个任务失败不影响其他任务，结果按任务顺序列出每张图的绝对路径和失败原因。", engine.MaxBatchTasks),
		InputSchema: batchSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in BatchGenerateInput) (*mcp.CallToolResult, BatchResult, error) {
		count("batch_generate_images")
		stop := startProgress(ctx, req)
		start := time.Now()
		total := len(in.Tasks)
		items, err := eng.GenerateBatch(ctx, in.requests(), func(i int, msg string) {
			stop.report(fmt.Sprintf("[任务 %d/%d] %s", i+1, total, msg))
		})
		stop.done()
		if err != nil {
			return nil, BatchResult{}, err
		}
		_, limit := eng.Concurrency()
		return batchResult(in, items, time.Since(start), limit, previewWanted(eng, in.IncludePreview), eng.Config().Defaults.PreviewMaxPx)
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
				Path: r.Path, OutputPath: r.OutputPath, Prompt: r.Prompt, Model: r.Model, Provider: r.Provider, Kind: r.Kind,
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

// requests converts batch arguments into one engine request per task. A task
// field that is empty (zero) takes the batch's shared value; extra maps are
// merged with the task's keys winning. Empty values left after that take the
// configured defaults inside the engine.
func (in BatchGenerateInput) requests() []engine.GenerateRequest {
	pick := func(task, shared string) string {
		if strings.TrimSpace(task) != "" {
			return task
		}
		return shared
	}
	out := make([]engine.GenerateRequest, len(in.Tasks))
	for i, t := range in.Tasks {
		n := t.N
		if n == 0 {
			n = in.N
		}
		out[i] = engine.GenerateRequest{
			Prompt: t.Prompt, Model: pick(t.Model, in.Model), Provider: pick(t.Provider, in.Provider),
			Size: pick(t.Size, in.Size), N: n, Quality: pick(t.Quality, in.Quality), Style: pick(t.Style, in.Style),
			Seed: t.Seed, ResponseFormat: in.ResponseFormat, OutputFormat: pick(t.OutputFormat, in.OutputFormat),
			Background: pick(t.Background, in.Background), NegativePrompt: pick(t.NegativePrompt, in.NegativePrompt),
			OutputDir: in.OutputDir, FilenamePrefix: pick(t.FilenamePrefix, in.FilenamePrefix),
			Extra: mergeExtra(in.Extra, t.Extra), Source: "mcp",
		}
	}
	return out
}

// mergeExtra returns shared overlaid with task (task keys win), or nil when
// both are empty. Neither input is modified.
//
// shared is the batch-level extra; task is one task's extra.
func mergeExtra(shared, task map[string]any) map[string]any {
	if len(shared) == 0 && len(task) == 0 {
		return nil
	}
	out := make(map[string]any, len(shared)+len(task))
	for k, v := range shared {
		out[k] = v
	}
	for k, v := range task {
		out[k] = v
	}
	return out
}

// previewWanted resolves include_preview against the configured default.
func previewWanted(eng *engine.Engine, v *bool) bool {
	if v != nil {
		return *v
	}
	return eng.Config().Defaults.IncludePreview
}

// imageResult builds the tool result: a text summary listing every absolute
// path (and how many gallery copies were kept when output_dir pointed
// elsewhere), up to four JPEG previews, and the structured output.
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
	copies := 0
	out.Images = imagesOut(res)
	for i, img := range res.Images {
		fmt.Fprintf(&b, "%d. %s (%dx%d)\n", i+1, img.Path, img.Width, img.Height)
		if img.GalleryPath != "" {
			copies++
		}
	}
	if copies > 0 {
		fmt.Fprintf(&b, "图库（默认图片目录）中也保留了 %d 份副本，见 gallery_path。\n", copies)
	}
	if res.RevisedPrompt != "" {
		fmt.Fprintf(&b, "改写后的提示词：%s\n", res.RevisedPrompt)
	}
	out.Attempts = attemptsOut(res.Attempts)
	content = append(content, &mcp.TextContent{Text: b.String()})
	if preview {
		for i, img := range res.Images {
			if i >= maxPreviews {
				break
			}
			if jpg, ok := previewJPEG(img.Path, maxPx); ok {
				content = append(content, jpg)
			}
		}
	}
	return &mcp.CallToolResult{Content: content}, out, nil
}

// batchResult builds the batch tool result: a text summary with each task's
// status and image paths (or its error), previews of the first image of each
// successful task (at most maxBatchPreviews), and the structured output. When
// every task failed the result is marked IsError so the client treats the
// call as failed, while still carrying each task's error.
//
// in is the batch arguments (for the prompts); items are the engine outcomes
// in task order; elapsed is the whole batch's duration; limit is the
// concurrency limit in force; preview says whether to attach previews; maxPx
// is the preview's longer side.
func batchResult(in BatchGenerateInput, items []engine.BatchItem, elapsed time.Duration, limit int, preview bool, maxPx int) (*mcp.CallToolResult, BatchResult, error) {
	out := BatchResult{Items: make([]BatchItemOut, 0, len(items)), MaxConcurrency: limit, ElapsedMS: elapsed.Milliseconds()}
	var body strings.Builder
	var previews []mcp.Content
	for i, it := range items {
		item := BatchItemOut{Index: i, Prompt: strings.TrimSpace(in.Tasks[i].Prompt), ElapsedMS: it.Elapsed.Milliseconds()}
		if it.Err != nil {
			out.Failed++
			item.Error = it.Err.Error()
			var f *engine.Failure
			if errors.As(it.Err, &f) {
				item.Attempts = attemptsOut(f.Attempts)
			}
			fmt.Fprintf(&body, "[%d] 失败（%.1fs）：%s\n    prompt：%s\n", i+1, it.Elapsed.Seconds(), item.Error, oneLine(item.Prompt, 60))
			out.Items = append(out.Items, item)
			continue
		}
		res := it.Result
		out.Succeeded++
		item.OK = true
		item.Model, item.Provider, item.RevisedPrompt, item.Seed = res.Model, res.Provider, res.RevisedPrompt, res.Seed
		item.Images = imagesOut(res)
		item.Attempts = attemptsOut(res.Attempts)
		fmt.Fprintf(&body, "[%d] 成功：%d 张（模型 %s，服务商 %s，%.1fs）prompt：%s\n", i+1, len(res.Images), res.Model, res.Provider, it.Elapsed.Seconds(), oneLine(item.Prompt, 60))
		for _, img := range res.Images {
			fmt.Fprintf(&body, "    %s (%dx%d)\n", img.Path, img.Width, img.Height)
		}
		if preview && len(previews) < maxBatchPreviews && len(res.Images) > 0 {
			if jpg, ok := previewJPEG(res.Images[0].Path, maxPx); ok {
				previews = append(previews, jpg)
			}
		}
		out.Items = append(out.Items, item)
	}
	head := fmt.Sprintf("批量生成完成：%d 个任务，成功 %d 个，失败 %d 个（总耗时 %.1fs，上游并发上限 %d）：\n", len(items), out.Succeeded, out.Failed, elapsed.Seconds(), limit)
	content := append([]mcp.Content{&mcp.TextContent{Text: head + body.String()}}, previews...)
	return &mcp.CallToolResult{Content: content, IsError: out.Succeeded == 0}, out, nil
}

// imagesOut converts an engine result's saved images into tool output.
//
// res is a successful engine result. It returns one ImageOut per image.
func imagesOut(res *engine.Result) []ImageOut {
	out := make([]ImageOut, 0, len(res.Images))
	for _, img := range res.Images {
		out = append(out, ImageOut{
			Path: img.Path, GalleryPath: img.GalleryPath, Width: img.Width, Height: img.Height, Bytes: img.Bytes, MIME: img.MIME,
			SourceURL: img.SourceURL, RevisedPrompt: img.RevisedPrompt,
		})
	}
	return out
}

// attemptsOut converts engine attempts into tool output.
//
// attempts are the tries of one call. It returns nil when there are none.
func attemptsOut(attempts []engine.Attempt) []AttemptOut {
	var out []AttemptOut
	for _, a := range attempts {
		out = append(out, AttemptOut{Provider: a.Provider, Model: a.Model, Error: a.Error, ElapsedMS: a.Elapsed.Milliseconds()})
	}
	return out
}

// previewJPEG reads a saved image and makes its JPEG preview content.
//
// path is the saved file; maxPx is the preview's longer side. It returns the
// content and true, or false when the file cannot be read or decoded (the
// preview is then simply left out).
func previewJPEG(path string, maxPx int) (mcp.Content, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	jpg, err := storage.Preview(data, maxPx)
	if err != nil {
		return nil, false
	}
	return &mcp.ImageContent{Data: jpg, MIMEType: "image/jpeg"}, true
}

// progressTicker sends MCP progress notifications while a slow tool runs: each
// engine status message, plus a heartbeat every five seconds so clients with
// short request timeouts keep waiting.
type progressTicker struct {
	ctx   context.Context
	req   *mcp.CallToolRequest
	token any
	mu    sync.Mutex // guards last
	// sendMu serializes notifications so their progress values reach the
	// client in increasing order even when batch tasks report at once.
	sendMu sync.Mutex
	step   float64
	last   string
	stop   chan struct{}
	once   sync.Once
	start  time.Time
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

// send emits one progress notification with a strictly increasing value. It
// holds sendMu while notifying, so concurrent reporters (batch tasks, the
// heartbeat) cannot deliver a lower value after a higher one.
//
// msg is the status text. There is no return value; delivery errors are
// ignored because progress is best effort.
func (t *progressTicker) send(msg string) {
	if t.token == nil {
		return
	}
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	t.step++
	_ = t.req.Session.NotifyProgress(t.ctx, &mcp.ProgressNotificationParams{ProgressToken: t.token, Progress: t.step, Message: msg})
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
