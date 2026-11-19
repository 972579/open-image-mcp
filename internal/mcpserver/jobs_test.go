package mcpserver

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/engine"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/storage"
	"github.com/972579/open-image-mcp/internal/testutil"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the lock.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// jobEngine returns an engine like testEngine but with request.max_wait set
// to maxWait (below the 5s config minimum, which only Validate enforces) and
// logging into logs.
func jobEngine(t *testing.T, u *testutil.Upstream, maxWait time.Duration, logs *syncBuffer) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Defaults.Model = "gpt-image-2"
	cfg.Defaults.OutputDir = filepath.Join(dir, "out")
	cfg.Request.MaxWait = config.Duration(maxWait)
	cfg.Providers = []config.Provider{{
		Name: "fake", Dialect: "openai", BaseURL: u.BaseURL(), APIKey: "sk-fake-1234567890", Enabled: true,
		Models: []config.Model{{ID: "gpt-image-2", Generate: true, Edit: true}},
	}}
	config.Normalize(&cfg)
	eng := engine.New(&cfg, imageapi.New(nil), storage.New(filepath.Join(dir, "history.jsonl")), slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(eng.Close)
	return eng
}

// promptDelays makes u answer after a delay chosen by the prompt: "慢" takes
// slow, anything else fast; every answer is one PNG.
func promptDelays(u *testutil.Upstream, fast, slow time.Duration) {
	u.Handler = func(_ int, r testutil.Request) (int, any) {
		if strings.Contains(promptOf(r), "慢") {
			time.Sleep(slow)
		} else {
			time.Sleep(fast)
		}
		return 200, testutil.B64Response(1)
	}
}

// callJob calls get_image_job until the job is done (at most 20 times) and
// returns the final result and the number of previews seen across all calls.
func callJob(t *testing.T, cs *mcp.ClientSession, id string) (BatchResult, int) {
	t.Helper()
	previews := 0
	for range 20 {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_image_job", Arguments: map[string]any{"job_id": id}})
		if err != nil || res.IsError {
			t.Fatalf("get_image_job: %v %+v", err, res)
		}
		for _, c := range res.Content {
			if _, ok := c.(*mcp.ImageContent); ok {
				previews++
			}
		}
		var out BatchResult
		decode(t, res.StructuredContent, &out)
		if out.JobID != id {
			t.Fatalf("job_id %q, want %q", out.JobID, id)
		}
		if out.Status == statusDone {
			return out, previews
		}
	}
	t.Fatal("job never finished")
	return BatchResult{}, 0
}

// TestWaitBudgetReturnsJob checks generate_image returns status running with
// a job_id once request.max_wait passes, that the job finishes in the
// background, and that get_image_job then returns the image with its preview
// exactly once.
func TestWaitBudgetReturnsJob(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	promptDelays(u, 0, 500*time.Millisecond)
	logs := &syncBuffer{}
	cs := connectInMemory(t, jobEngine(t, u, 150*time.Millisecond, logs))

	start := time.Now()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "慢慢画的海报"}})
	if err != nil || res.IsError {
		t.Fatalf("generate_image: %v %+v", err, res)
	}
	if waited := time.Since(start); waited > 400*time.Millisecond {
		t.Errorf("call returned after %v; the wait budget was 150ms", waited)
	}
	var gen ImageResult
	decode(t, res.StructuredContent, &gen)
	if gen.Status != statusRunning || gen.JobID == "" || len(gen.Images) != 0 {
		t.Fatalf("want a running result with a job_id, got %+v", gen)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, gen.JobID) || !strings.Contains(text, "get_image_job") {
		t.Errorf("text does not tell the model what to do: %q", text)
	}

	out, previews := callJob(t, cs, gen.JobID)
	if out.Succeeded != 1 || len(out.Items) != 1 || out.Items[0].Status != itemSucceeded || len(out.Items[0].Images) != 1 {
		t.Fatalf("job result: %+v", out)
	}
	if previews != 1 {
		t.Errorf("saw %d previews across get_image_job calls, want 1", previews)
	}
	if !strings.Contains(logs.String(), "wait budget reached") {
		t.Errorf("wait budget not logged:\n%s", logs.String())
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_image_job", Arguments: map[string]any{"job_id": "job_nope"}})
	if err != nil || !res.IsError {
		t.Errorf("unknown job should be a tool error: %v %+v", err, res)
	}
}

// TestBatchReturnsPartialResults checks a batch whose tasks outlast the wait
// budget returns the finished tasks at once (with their previews), marks the
// rest running, and that get_image_job delivers the rest without repeating
// previews already sent.
func TestBatchReturnsPartialResults(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	promptDelays(u, 20*time.Millisecond, 700*time.Millisecond)
	cs := connectInMemory(t, jobEngine(t, u, 300*time.Millisecond, &syncBuffer{}))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "batch_generate_images", Arguments: map[string]any{
		"tasks": []any{map[string]any{"prompt": "快图"}, map[string]any{"prompt": "慢图"}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("batch: %v %+v", err, res)
	}
	var first BatchResult
	decode(t, res.StructuredContent, &first)
	if first.Status != statusRunning || first.Succeeded != 1 || first.Running != 1 ||
		first.Items[0].Status != itemSucceeded || first.Items[1].Status != itemRunning || first.JobID == "" {
		t.Fatalf("partial result wrong: %+v", first)
	}
	previews := 0
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			previews++
		}
	}
	if previews != 1 {
		t.Errorf("partial result carried %d previews, want 1 (the finished task)", previews)
	}

	out, later := callJob(t, cs, first.JobID)
	if out.Succeeded != 2 || out.Running != 0 || out.Items[1].Status != itemSucceeded {
		t.Fatalf("final result: %+v", out)
	}
	if later != 1 {
		t.Errorf("get_image_job sent %d previews, want 1 (only the newly finished task)", later)
	}
}

// TestClientCancelKeepsJob checks that a client giving up on a call (its
// context ends, so it sends notifications/cancelled) does not stop the
// upstream work: the image is still saved to history, and both the cancel and
// the abandoned wait are logged.
func TestClientCancelKeepsJob(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	promptDelays(u, 0, 400*time.Millisecond)
	logs := &syncBuffer{}
	eng := jobEngine(t, u, 0, logs) // 0: wait until done, like a client with no timeout
	cs := connectInMemory(t, eng)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate_image", Arguments: map[string]any{"prompt": "慢：客户端先放弃"}}); err == nil {
		t.Fatal("call should have been cut short by the client's context")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		total, _, _ := eng.History(storage.Query{Keyword: "客户端先放弃"})
		if total == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("image never saved after the client cancelled; log:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"client cancelled a request", "client stopped waiting", "image call succeeded"} {
		if !waitForLog(logs, want, time.Second) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "image call failed") {
		t.Errorf("the client's cancel reached the upstream call:\n%s", logs.String())
	}
}

// waitForLog polls logs for want for up to d, since some lines are written
// by goroutines that finish slightly after the event the test waited for.
//
// It returns whether want appeared.
func waitForLog(logs *syncBuffer, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
