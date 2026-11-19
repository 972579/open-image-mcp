package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/testutil"
)

// slowUpstream makes u answer each request after delay with one image (a 400
// when the prompt, JSON or multipart, contains "坏") and returns a counter of
// the highest number of requests it served at once.
func slowUpstream(u *testutil.Upstream, delay time.Duration) *atomic.Int32 {
	var inflight, peak atomic.Int32
	u.Handler = func(_ int, r testutil.Request) (int, any) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for old := peak.Load(); cur > old && !peak.CompareAndSwap(old, cur); old = peak.Load() {
		}
		time.Sleep(delay)
		if strings.Contains(promptOf(r), "坏") {
			return 400, map[string]any{"error": map[string]any{"message": "prompt rejected"}}
		}
		return 200, testutil.B64Response(1)
	}
	return &peak
}

// promptOf returns a fake-upstream request's prompt from its JSON body
// (generations) or multipart form (edits).
func promptOf(r testutil.Request) string {
	if p, ok := r.JSON["prompt"].(string); ok {
		return p
	}
	if v := r.Form["prompt"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// TestParallelToolCalls checks that several generate_image calls sent at once
// over one MCP session reach the upstream in parallel: the server does not
// serialize tool calls.
func TestParallelToolCalls(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	peak := slowUpstream(u, 200*time.Millisecond)
	cs := connectInMemory(t, testEngine(t, u))
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]string, 3)
	for i := range errs {
		wg.Go(func() {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "并行调用", "include_preview": false}})
			switch {
			case err != nil:
				errs[i] = err.Error()
			case res.IsError:
				errs[i] = "tool error"
			}
		})
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Fatalf("call %d: %s", i, e)
		}
	}
	if got := peak.Load(); got != 3 {
		t.Errorf("peak concurrent upstream requests = %d, want 3", got)
	}
}

// TestBatchTool checks batch_generate_images through the MCP protocol: shared
// parameters reach every task unless the task overrides them, results come
// back in task order with paths and previews, one failing task is reported
// without failing the call, and a batch where every task fails is a tool
// error.
func TestBatchTool(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	peak := slowUpstream(u, 100*time.Millisecond)
	cs := connectInMemory(t, testEngine(t, u))
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var schemaOK bool
	for _, tool := range tools.Tools {
		if tool.Name == "batch_generate_images" {
			schemaOK = strings.Contains(string(mustJSON(t, tool.InputSchema)), `"maxItems":16`)
		}
	}
	if !schemaOK {
		t.Fatal("batch_generate_images missing or tasks not capped at 16")
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch_generate_images", Arguments: map[string]any{
		"size":  "1536x1024",
		"extra": map[string]any{"shared_key": "s", "both": "shared"},
		"tasks": []any{
			map[string]any{"prompt": "第一张：海边日落"},
			map[string]any{"prompt": "第二张：坏的提示词"},
			map[string]any{"prompt": "第三张：竖版海报", "size": "1024x1536", "extra": map[string]any{"both": "task"}},
		},
	}})
	if err != nil || res.IsError {
		t.Fatalf("batch_generate_images: %v %+v", err, res)
	}
	var out BatchResult
	decode(t, res.StructuredContent, &out)
	if out.Succeeded != 2 || out.Failed != 1 || len(out.Items) != 3 || out.MaxConcurrency != 4 {
		t.Fatalf("batch summary wrong: %+v", out)
	}
	if !out.Items[0].OK || out.Items[1].OK || !out.Items[2].OK || out.Items[1].Error == "" || out.Items[2].Index != 2 {
		t.Errorf("items wrong: %+v", out.Items)
	}
	for _, i := range []int{0, 2} {
		if len(out.Items[i].Images) != 1 {
			t.Fatalf("task %d images: %+v", i, out.Items[i])
		}
		if _, err := os.Stat(out.Items[i].Images[0].Path); err != nil {
			t.Errorf("task %d image not on disk: %v", i, err)
		}
	}
	if got := peak.Load(); got != 3 {
		t.Errorf("batch tasks did not run in parallel: peak %d", got)
	}

	// Each upstream request got the shared size unless the task set its own,
	// and the task's extra won over the batch's on the shared key.
	sizes := map[string]string{}
	for _, r := range u.Requests() {
		p, _ := r.JSON["prompt"].(string)
		sizes[p], _ = r.JSON["size"].(string)
		if strings.HasPrefix(p, "第三张") && (r.JSON["both"] != "task" || r.JSON["shared_key"] != "s") {
			t.Errorf("task extra not merged over batch extra: %+v", r.JSON)
		}
	}
	if sizes["第一张：海边日落"] != "1536x1024" || sizes["第三张：竖版海报"] != "1024x1536" {
		t.Errorf("sizes sent = %v", sizes)
	}

	var text string
	previews := 0
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			text += c.Text
		case *mcp.ImageContent:
			previews++
		}
	}
	if previews != 2 || !strings.Contains(text, out.Items[0].Images[0].Path) || !strings.Contains(text, "失败") {
		t.Errorf("content: %d previews, text %q", previews, text)
	}

	// Every task failing marks the whole call as a tool error but keeps the
	// per-task details.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch_generate_images", Arguments: map[string]any{
		"tasks": []any{map[string]any{"prompt": "坏一"}, map[string]any{"prompt": "坏二"}},
	}})
	if err != nil || !res.IsError {
		t.Fatalf("all-failed batch should be a tool error: %v %+v", err, res)
	}
	decode(t, res.StructuredContent, &out)
	if out.Failed != 2 || out.Succeeded != 0 {
		t.Errorf("all-failed summary: %+v", out)
	}

	// An invalid task rejects the batch before anything runs.
	before := len(u.Requests())
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch_generate_images", Arguments: map[string]any{
		"tasks": []any{map[string]any{"prompt": "好"}, map[string]any{"prompt": "x", "model": "no-such-model"}},
	}})
	if err != nil || !res.IsError {
		t.Fatalf("invalid task should be a tool error: %v %+v", err, res)
	}
	if after := len(u.Requests()); after != before {
		t.Errorf("rejected batch still sent %d requests", after-before)
	}
}

// mustJSON marshals v for substring checks on schemas.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBatchEditTool checks batch_edit_images through the MCP protocol: a task
// without its own prompt or images takes the shared ones, a task with its own
// images does not inherit the shared mask, tasks run in parallel against the
// edits endpoint, one failing task does not fail the call, and an invalid
// task (missing file, no prompt at all) rejects the batch before any request.
func TestBatchEditTool(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	peak := slowUpstream(u, 100*time.Millisecond)
	cs := connectInMemory(t, testEngine(t, u))
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, testutil.PNG(8, 8), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a, b, mask, mask2 := write("a.png"), write("b.png"), write("mask.png"), write("mask2.png")

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch_edit_images", Arguments: map[string]any{
		"prompt":          "统一改成水彩",
		"images":          []string{a},
		"mask":            mask,
		"include_preview": false,
		"tasks": []any{
			map[string]any{},                                // shared prompt, images and mask
			map[string]any{"images": []string{b}},           // own images: no shared mask
			map[string]any{"prompt": "改成夜景", "mask": mask2}, // shared images, own mask
			map[string]any{"prompt": "坏的指令"},                // fails upstream
		},
	}})
	if err != nil || res.IsError {
		t.Fatalf("batch_edit_images: %v %+v", err, res)
	}
	var out BatchResult
	decode(t, res.StructuredContent, &out)
	if out.Succeeded != 3 || out.Failed != 1 || out.Items[3].OK || out.Items[3].Error == "" {
		t.Fatalf("batch summary wrong: %+v", out)
	}
	if out.Items[0].Prompt != "统一改成水彩" || out.Items[2].Prompt != "改成夜景" ||
		len(out.Items[1].SourceImages) != 1 || out.Items[1].SourceImages[0] != b || out.Items[0].SourceImages[0] != a {
		t.Errorf("items show wrong inputs: %+v", out.Items)
	}
	if got := peak.Load(); got != 4 {
		t.Errorf("edit tasks did not run in parallel: peak %d", got)
	}

	// key: prompt + reference image name → mask file name ("" for none)
	masks := map[string]string{}
	for _, r := range u.Requests() {
		if r.Path != "/v1/images/edits" || len(r.Files["image"]) != 1 {
			t.Fatalf("not an edit with one image: %+v", r)
		}
		m := ""
		if f := r.Files["mask"]; len(f) > 0 {
			m = f[0].Name
		}
		masks[promptOf(r)+"|"+r.Files["image"][0].Name] = m
	}
	want := map[string]string{
		"统一改成水彩|a.png": "mask.png",
		"统一改成水彩|b.png": "",
		"改成夜景|a.png":   "mask2.png",
		"坏的指令|a.png":   "mask.png",
	}
	for k, v := range want {
		if got, ok := masks[k]; !ok || got != v {
			t.Errorf("request %s: mask %q (sent: %v), want %q", k, got, ok, v)
		}
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if !strings.Contains(text, "批量编辑完成") || !strings.Contains(text, "参考图："+b) || !strings.Contains(text, out.Items[0].Images[0].Path) {
		t.Errorf("summary text: %q", text)
	}

	// Invalid tasks reject the whole batch before any upstream request.
	before := len(u.Requests())
	for name, args := range map[string]map[string]any{
		"missing file": {"prompt": "改", "tasks": []any{map[string]any{"images": []string{a}}, map[string]any{"images": []string{filepath.Join(dir, "nope.png")}}}},
		"no prompt":    {"images": []string{a}, "tasks": []any{map[string]any{"prompt": "有"}, map[string]any{}}},
		"no images":    {"prompt": "改", "tasks": []any{map[string]any{}}},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch_edit_images", Arguments: args})
		if err != nil || !res.IsError {
			t.Errorf("%s: want tool error, got %v %+v", name, err, res)
		}
	}
	if after := len(u.Requests()); after != before {
		t.Errorf("rejected batches still sent %d requests", after-before)
	}
}
