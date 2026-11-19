package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/engine"
)

// Status values for ImageResult.Status and BatchResult.Status.
const (
	statusDone    = "done"
	statusRunning = "running"
)

// Status values for BatchItemOut.Status.
const (
	itemSucceeded = "succeeded"
	itemFailed    = "failed"
	itemRunning   = "running"
)

// runningHint is appended to every image tool's description so the model
// knows a "running" result is not a failure and must not be resubmitted.
const runningHint = "单次调用最多等待 request.max_wait（默认 50 秒）：到时还没完成就先返回已完成的部分，status 为 running，未完成的在后台继续运行。" +
	"此时不要重新提交，请调用 get_image_job 并传入返回的 job_id 继续等待并获取结果。"

// GetJobInput is get_image_job's arguments.
type GetJobInput struct {
	JobID          string `json:"job_id" jsonschema:"生成/编辑工具在 status 为 running 时返回的 job_id"`
	IncludePreview *bool  `json:"include_preview,omitempty" jsonschema:"是否附带尚未发送过的缩略图预览，默认沿用提交时的设置"`
}

// jobView is what the MCP layer attaches to an engine job (Job.Info): how to
// describe its tasks and whether to send previews, plus which previews have
// already been sent so a later get_image_job does not repeat them.
type jobView struct {
	// single marks a job started by generate_image or edit_image.
	single bool
	// title names the job in summaries ("生成", "编辑", "批量生成", "批量编辑").
	title string
	// tasks describe each task's input, in task order.
	tasks []batchTaskInfo
	// preview is the include_preview choice made when the job was submitted.
	preview bool

	mu        sync.Mutex
	previewed map[int]bool
}

// newJobView returns a view for a job with the given tasks.
//
// single marks generate_image / edit_image; title names the job; tasks
// describe the inputs; preview is the resolved include_preview. It returns
// the view.
func newJobView(single bool, title string, tasks []batchTaskInfo, preview bool) *jobView {
	return &jobView{single: single, title: title, tasks: tasks, preview: preview, previewed: map[int]bool{}}
}

// takePreview reports whether task i's preview may be attached now, and if so
// marks it as sent. It is safe for concurrent use.
//
// i is the task index. It returns true the first time per task.
func (v *jobView) takePreview(i int) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.previewed[i] {
		return false
	}
	v.previewed[i] = true
	return true
}

// waiter holds what the image tools need to wait for jobs: the engine (for
// the wait budget and lookups) and the logger.
type waiter struct {
	eng *engine.Engine
	log *slog.Logger
}

// newWaiter returns a waiter logging through the engine's logger.
//
// eng is the engine the tools are bound to.
func newWaiter(eng *engine.Engine) *waiter {
	return &waiter{eng: eng, log: eng.Logger()}
}

// await waits for job up to request.max_wait (0 = until done), forwarding the
// job's progress to the client as MCP progress notifications, and logs how
// the wait ended: done, budget reached (the caller returns a job_id), or the
// client gave up (its request was cancelled; the job keeps running). It never
// stops the job.
//
// ctx is the tool request's context; req is the request (for the progress
// token and client name); tool names the tool for the log; job is the job to
// wait for. It returns the job's state when the wait ended.
func (w *waiter) await(ctx context.Context, req *mcp.CallToolRequest, tool string, job *engine.Job) engine.JobSnapshot {
	wait := w.eng.Config().Request.MaxWait.Std()
	n := len(job.Snapshot().Items)
	w.log.Info("tool call", "tool", tool, "job", job.ID, "tasks", n, "client", clientName(req), "progress_token", hasProgressToken(req), "max_wait", wait)
	tick := startProgress(ctx, req)
	stop := job.Listen(func(i int, msg string) {
		if n > 1 {
			msg = fmt.Sprintf("[任务 %d/%d] %s", i+1, n, msg)
		}
		tick.report(msg)
	})
	start := time.Now()
	done := job.Wait(ctx, wait)
	stop()
	tick.done()
	waited := time.Since(start).Round(time.Millisecond)
	switch {
	case done:
	case ctx.Err() != nil:
		w.log.Warn("client stopped waiting; job continues in background", "tool", tool, "job", job.ID, "waited", waited, "client", clientName(req))
	default:
		w.log.Info("wait budget reached; returning job_id, job continues in background", "tool", tool, "job", job.ID, "waited", waited)
	}
	return job.Snapshot()
}

// singleResult turns the state of a generate_image / edit_image job into that
// tool's result: the usual images and previews when it is done, the task's
// error when it failed, or a "running" result with the job_id when the wait
// budget ran out first.
//
// view and job describe the job; snap is its state; maxPx is the preview's
// longer side. It returns the tool result, its structured output, and the
// task's error for a failed call.
func (w *waiter) singleResult(view *jobView, job *engine.Job, snap engine.JobSnapshot, maxPx int) (*mcp.CallToolResult, ImageResult, error) {
	if !snap.Done {
		now := snap.Progress[0]
		if now == "" {
			now = "已提交"
		}
		text := fmt.Sprintf("图片仍在%s中（已运行 %.0fs，当前：%s）。任务在后台继续运行，不要重新提交；请调用 get_image_job，参数 job_id=%q，获取结果。",
			view.title, snap.Elapsed.Seconds(), now, job.ID)
		out := ImageResult{Status: statusRunning, JobID: job.ID, Images: []ImageOut{}, ElapsedMS: snap.Elapsed.Milliseconds()}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
	it := snap.Items[0]
	if it.Err != nil {
		return nil, ImageResult{}, it.Err
	}
	preview := view.preview && view.takePreview(0)
	return imageResult(it.Result, preview, maxPx)
}

// jobResult renders any job (a batch, or a single call fetched through
// get_image_job) as a BatchResult: a text summary with each task's status,
// reference images (for edits) and output paths, error or current progress;
// previews of the first image of each newly succeeded task (each sent at most
// once per job, at most maxBatchPreviews per result); and the structured
// output. A finished job whose tasks all failed is marked IsError; a running
// one never is.
//
// view describes the job (nil falls back to bare prompts-less output); job is
// the job; snap is its state; preview says whether to attach previews; maxPx
// is the preview's longer side. It returns the tool result and its structured
// output.
func (w *waiter) jobResult(view *jobView, job *engine.Job, snap engine.JobSnapshot, preview bool, maxPx int) (*mcp.CallToolResult, BatchResult, error) {
	if view == nil {
		view = newJobView(false, "任务", nil, preview)
	}
	_, limit := w.eng.Concurrency()
	out := BatchResult{Items: make([]BatchItemOut, 0, len(snap.Items)), JobID: job.ID, MaxConcurrency: limit, ElapsedMS: snap.Elapsed.Milliseconds()}
	out.Status = statusRunning
	if snap.Done {
		out.Status = statusDone
	}
	var body strings.Builder
	var previews []mcp.Content
	for i, it := range snap.Items {
		var info batchTaskInfo
		if i < len(view.tasks) {
			info = view.tasks[i]
		}
		item := BatchItemOut{Index: i, Prompt: info.Prompt, SourceImages: info.Sources, ElapsedMS: it.Elapsed.Milliseconds()}
		sources := ""
		if len(info.Sources) > 0 {
			sources = "    参考图：" + strings.Join(info.Sources, "、") + "\n"
		}
		switch {
		case !snap.Finished[i]:
			out.Running++
			item.Status = itemRunning
			item.ElapsedMS = snap.Elapsed.Milliseconds()
			now := snap.Progress[i]
			if now == "" {
				now = "已提交"
			}
			fmt.Fprintf(&body, "[%d] 进行中：%s\n    prompt：%s\n%s", i+1, now, oneLine(item.Prompt, 60), sources)
		case it.Err != nil:
			out.Failed++
			item.Status = itemFailed
			item.Error = it.Err.Error()
			var f *engine.Failure
			if errors.As(it.Err, &f) {
				item.Attempts = attemptsOut(f.Attempts)
			}
			fmt.Fprintf(&body, "[%d] 失败（%.1fs）：%s\n    prompt：%s\n%s", i+1, it.Elapsed.Seconds(), item.Error, oneLine(item.Prompt, 60), sources)
		default:
			res := it.Result
			out.Succeeded++
			item.Status, item.OK = itemSucceeded, true
			item.Model, item.Provider, item.RevisedPrompt, item.Seed = res.Model, res.Provider, res.RevisedPrompt, res.Seed
			item.Images = imagesOut(res)
			item.Attempts = attemptsOut(res.Attempts)
			fmt.Fprintf(&body, "[%d] 成功：%d 张（模型 %s，服务商 %s，%.1fs）prompt：%s\n%s", i+1, len(res.Images), res.Model, res.Provider, it.Elapsed.Seconds(), oneLine(item.Prompt, 60), sources)
			for _, img := range res.Images {
				fmt.Fprintf(&body, "    %s (%dx%d)\n", img.Path, img.Width, img.Height)
			}
			if preview && len(previews) < maxBatchPreviews && len(res.Images) > 0 && view.takePreview(i) {
				if jpg, ok := previewJPEG(res.Images[0].Path, maxPx); ok {
					previews = append(previews, jpg)
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	var head string
	if snap.Done {
		head = fmt.Sprintf("%s完成：%d 个任务，成功 %d 个，失败 %d 个（总耗时 %.1fs，上游并发上限 %d）：\n", view.title, len(snap.Items), out.Succeeded, out.Failed, snap.Elapsed.Seconds(), limit)
	} else {
		head = fmt.Sprintf("%s仍在进行：%d 个任务，已成功 %d 个，失败 %d 个，进行中 %d 个（已运行 %.0fs）。进行中的任务在后台继续运行，不要重新提交；请调用 get_image_job，参数 job_id=%q，获取其余结果。\n",
			view.title, len(snap.Items), out.Succeeded, out.Failed, out.Running, snap.Elapsed.Seconds(), job.ID)
	}
	content := append([]mcp.Content{&mcp.TextContent{Text: head + body.String()}}, previews...)
	return &mcp.CallToolResult{Content: content, IsError: snap.Done && out.Succeeded == 0}, out, nil
}

// addGetJobTool registers get_image_job: it looks a job up, waits for it up
// to request.max_wait like the submitting tools, and returns its state as a
// BatchResult (a single call's job has one item). It is read-only, so clients
// may run it alongside other calls.
//
// srv is the server; w is the shared waiter; count records the call for the
// GUI. There is no return value.
func addGetJobTool(srv *mcp.Server, w *waiter, count func(string)) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "get_image_job",
		Title: "获取图片任务结果",
		Description: fmt.Sprintf("获取 generate_image / edit_image / batch_generate_images / batch_edit_images 在后台继续运行的任务结果。"+
			"传入它们在 status 为 running 时返回的 job_id；最多等待 request.max_wait（默认 50 秒），完成则返回全部结果（status 为 done），"+
			"否则返回目前已完成的部分（status 仍为 running），可以再次调用继续等待。任务完成后保留 %v。", engine.JobRetention),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in GetJobInput) (*mcp.CallToolResult, BatchResult, error) {
		count("get_image_job")
		id := strings.TrimSpace(in.JobID)
		job, ok := w.eng.Job(id)
		if !ok {
			return nil, BatchResult{}, fmt.Errorf("找不到任务 %q：可能已过期（完成后保留 %v）或 App 已重启；已完成的图片可以用 list_generated_images 查看", id, engine.JobRetention)
		}
		view, _ := job.Info.(*jobView)
		preview := previewWanted(w.eng, in.IncludePreview)
		if view != nil && in.IncludePreview == nil {
			preview = view.preview
		}
		snap := w.await(ctx, req, "get_image_job", job)
		return w.jobResult(view, job, snap, preview, w.eng.Config().Defaults.PreviewMaxPx)
	})
}

// logCancels is receiving middleware that logs every notifications/cancelled
// a client sends, with its reason and request ID, so a client-side timeout
// is visible in the app log as such. It never changes the message.
//
// next is the rest of the handler chain. It returns the wrapped handler.
func (w *waiter) logCancels(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/cancelled" {
			if p, ok := req.GetParams().(*mcp.CancelledParams); ok && p != nil {
				ss, _ := req.GetSession().(*mcp.ServerSession)
				w.log.Warn("client cancelled a request", "request_id", fmt.Sprint(p.RequestID), "reason", p.Reason, "client", sessionClient(ss))
			}
		}
		return next(ctx, method, req)
	}
}

// clientName returns the calling client's name and version from its
// initialize request, or "unknown".
//
// req is the tool request.
func clientName(req *mcp.CallToolRequest) string {
	if req == nil {
		return "unknown"
	}
	return sessionClient(req.Session)
}

// sessionClient returns a session's client name and version, or "unknown".
//
// ss is the server session, possibly nil.
func sessionClient(ss *mcp.ServerSession) string {
	if ss == nil {
		return "unknown"
	}
	p := ss.InitializeParams()
	if p == nil || p.ClientInfo == nil || p.ClientInfo.Name == "" {
		return "unknown"
	}
	if p.ClientInfo.Version != "" {
		return p.ClientInfo.Name + " " + p.ClientInfo.Version
	}
	return p.ClientInfo.Name
}

// hasProgressToken reports whether the client asked for progress
// notifications on this call (without a token the server cannot send any,
// and its heartbeat cannot keep the client waiting).
//
// req is the tool request.
func hasProgressToken(req *mcp.CallToolRequest) bool {
	return req != nil && req.Params != nil && req.Params.GetProgressToken() != nil
}
