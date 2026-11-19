package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/storage"
	"github.com/972579/open-image-mcp/internal/testutil"
)

// peakTracker records how many upstream requests a fake upstream is serving
// at once, and the highest number seen.
type peakTracker struct {
	inflight, peak atomic.Int32
}

// enter marks one request as started and updates the peak.
func (p *peakTracker) enter() {
	cur := p.inflight.Add(1)
	for {
		old := p.peak.Load()
		if cur <= old || p.peak.CompareAndSwap(old, cur) {
			return
		}
	}
}

// leave marks one request as finished.
func (p *peakTracker) leave() { p.inflight.Add(-1) }

// slowHandler answers every request after delay with one image, failing with
// a 400 when the prompt (JSON or multipart) contains "坏", and tracks
// concurrency in pt.
func slowHandler(pt *peakTracker, delay time.Duration) func(int, testutil.Request) (int, any) {
	return func(_ int, r testutil.Request) (int, any) {
		pt.enter()
		defer pt.leave()
		time.Sleep(delay)
		if strings.Contains(requestPrompt(r), "坏") {
			return 400, map[string]any{"error": map[string]any{"message": "prompt rejected"}}
		}
		return 200, testutil.B64Response(1)
	}
}

// requestPrompt returns the prompt of a fake-upstream request, from the JSON
// body (generations) or the multipart form (edits).
func requestPrompt(r testutil.Request) string {
	if p, ok := r.JSON["prompt"].(string); ok {
		return p
	}
	if v := r.Form["prompt"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// TestBatchRunsConcurrentlyWithinLimit checks a batch runs its tasks in
// parallel, never exceeding request.max_concurrency upstream requests at a
// time, keeps results in task order, and saves every image to history.
func TestBatchRunsConcurrentlyWithinLimit(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	pt := &peakTracker{}
	const delay = 150 * time.Millisecond
	u.Handler = slowHandler(pt, delay)
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	cfg := e.Config().Clone()
	cfg.Request.MaxConcurrency = 3
	e.Reload(&cfg)

	reqs := make([]GenerateRequest, 6)
	for i := range reqs {
		reqs[i] = GenerateRequest{Prompt: "并发任务 " + string(rune('A'+i))}
	}
	var mu sync.Mutex
	var queued bool
	start := time.Now()
	job, err := e.StartGenerate(reqs, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := job.Listen(func(_ int, msg string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(msg, "排队") {
			queued = true
		}
	})
	items := waitItems(t, job)
	stop()
	elapsed := time.Since(start)
	if got := pt.peak.Load(); got != 3 {
		t.Errorf("peak concurrent upstream requests = %d, want 3", got)
	}
	// 6 tasks, 3 at a time: about two rounds, far less than six in a row.
	if elapsed >= 5*delay {
		t.Errorf("batch took %v; tasks did not run in parallel", elapsed)
	}
	if !queued {
		t.Error("no queueing progress message although tasks exceeded the limit")
	}
	for i, it := range items {
		if it.Err != nil || len(it.Result.Images) != 1 {
			t.Fatalf("task %d: %+v %v", i, it.Result, it.Err)
		}
	}
	total, _, err := e.History(storage.Query{Keyword: "并发任务"})
	if err != nil || total != 6 {
		t.Errorf("history has %d batch images (%v), want 6", total, err)
	}
	if active, limit := e.Concurrency(); active != 0 || limit != 3 {
		t.Errorf("Concurrency() = %d/%d after the batch, want 0/3", active, limit)
	}
}

// TestBatchPartialFailure checks one failing task is reported on its own and
// does not stop the others.
func TestBatchPartialFailure(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = slowHandler(&peakTracker{}, 0)
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	job, err := e.StartGenerate([]GenerateRequest{{Prompt: "好图"}, {Prompt: "坏图"}, {Prompt: "另一张好图"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := waitItems(t, job)
	if items[0].Err != nil || items[2].Err != nil {
		t.Fatalf("good tasks failed: %v / %v", items[0].Err, items[2].Err)
	}
	var f *Failure
	if !errors.As(items[1].Err, &f) || len(f.Attempts) != 1 {
		t.Fatalf("bad task error = %v, want a *Failure with one attempt (400 is not retried)", items[1].Err)
	}
}

// TestBatchValidatesBeforeCalling checks an invalid task rejects the whole
// batch before any upstream request, and that the task count is bounded.
func TestBatchValidatesBeforeCalling(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	_, err := e.StartGenerate([]GenerateRequest{{Prompt: "ok"}, {Prompt: "  "}}, nil)
	if err == nil || !strings.Contains(err.Error(), "第 2 个任务") {
		t.Fatalf("want error naming task 2, got %v", err)
	}
	if _, err := e.StartGenerate([]GenerateRequest{{Prompt: " "}}, nil); err == nil || strings.Contains(err.Error(), "第 1 个任务") {
		t.Errorf("a single call's error should not name a task: %v", err)
	}
	if n := len(u.Requests()); n != 0 {
		t.Errorf("upstream got %d requests for a rejected batch", n)
	}
	if _, err := e.StartGenerate(nil, nil); err == nil {
		t.Error("empty batch accepted")
	}
	if _, err := e.StartGenerate(make([]GenerateRequest, MaxBatchTasks+1), nil); err == nil {
		t.Error("oversized batch accepted")
	}
}

// writePNG saves a small PNG under dir and returns its absolute path, for edit
// tests that pass reference images by path.
func writePNG(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, testutil.PNG(8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEditBatchRunsConcurrentlyWithinLimit checks an edit batch runs its tasks
// in parallel under request.max_concurrency, sends each task's own reference
// image to the edits endpoint, and records every result as an edit.
func TestEditBatchRunsConcurrentlyWithinLimit(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	pt := &peakTracker{}
	u.Handler = slowHandler(pt, 120*time.Millisecond)
	e, dir := newEngine(t, prov("a", u, "openai", genModel))
	cfg := e.Config().Clone()
	cfg.Request.MaxConcurrency = 2
	e.Reload(&cfg)

	reqs := make([]EditRequest, 4)
	for i := range reqs {
		src := writePNG(t, dir, "src-"+string(rune('a'+i))+".png")
		reqs[i] = EditRequest{GenerateRequest: GenerateRequest{Prompt: "批量编辑水彩"}, Images: []string{src}}
	}
	job, err := e.StartEdit(reqs, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := waitItems(t, job)
	for i, it := range items {
		if it.Err != nil || len(it.Result.Images) != 1 {
			t.Fatalf("task %d: %+v %v", i, it.Result, it.Err)
		}
	}
	if got := pt.peak.Load(); got != 2 {
		t.Errorf("peak concurrent upstream requests = %d, want 2", got)
	}
	names := map[string]bool{}
	for _, r := range u.Requests() {
		if r.Path != "/v1/images/edits" || len(r.Files["image"]) != 1 {
			t.Fatalf("not an edit request with one image: %+v", r)
		}
		names[r.Files["image"][0].Name] = true
	}
	if len(names) != 4 {
		t.Errorf("upstream saw reference images %v, want 4 distinct", names)
	}
	_, recs, err := e.History(storage.Query{Keyword: "批量编辑水彩"})
	if err != nil || len(recs) != 4 || recs[0].Kind != "edit" {
		t.Errorf("history = %+v %v", recs, err)
	}
}

// TestEditBatchValidatesBeforeCalling checks a missing reference image or a
// model that cannot edit rejects the whole edit batch before any upstream
// request.
func TestEditBatchValidatesBeforeCalling(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	e, dir := newEngine(t, prov("a", u, "openai", genModel, config.Model{ID: "gen-only", Generate: true}))
	src := writePNG(t, dir, "ok.png")
	ok := EditRequest{GenerateRequest: GenerateRequest{Prompt: "改"}, Images: []string{src}}

	missing := EditRequest{GenerateRequest: GenerateRequest{Prompt: "改"}, Images: []string{filepath.Join(dir, "nope.png")}}
	if _, err := e.StartEdit([]EditRequest{ok, missing}, nil); err == nil || !strings.Contains(err.Error(), "第 2 个任务") {
		t.Errorf("missing file: want error naming task 2, got %v", err)
	}
	genOnly := ok
	genOnly.Model = "gen-only"
	if _, err := e.StartEdit([]EditRequest{ok, genOnly}, nil); err == nil || !strings.Contains(err.Error(), "第 2 个任务") {
		t.Errorf("generation-only model: want error naming task 2, got %v", err)
	}
	if _, err := e.StartEdit([]EditRequest{{GenerateRequest: GenerateRequest{Prompt: "改"}}}, nil); err == nil {
		t.Error("task without images accepted")
	}
	if n := len(u.Requests()); n != 0 {
		t.Errorf("upstream got %d requests for rejected batches", n)
	}
}

// TestCachedLoaderSharesImages checks the batch loader reads a reference
// image used by several tasks once and hands back the same bytes, while a
// different name (another position, or the mask) is loaded on its own.
func TestCachedLoaderSharesImages(t *testing.T) {
	src := writePNG(t, t.TempDir(), "shared.png")
	load := cachedLoader()
	a, err := load(src, "image-1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := load(src, "image-1")
	if &a.Data[0] != &b.Data[0] {
		t.Error("same image loaded twice instead of shared")
	}
	m, _ := load(src, "mask")
	if &m.Data[0] == &a.Data[0] {
		t.Error("mask entry should be cached separately from the image entry")
	}
	if _, err := load(src+".missing", "image-1"); err == nil {
		t.Error("missing file not reported")
	}
}

// waitItems waits (up to ten seconds) for job to finish and returns its task
// outcomes, failing the test on a timeout.
func waitItems(t *testing.T, job *Job) []BatchItem {
	t.Helper()
	if !job.Wait(context.Background(), 10*time.Second) {
		t.Fatal("job did not finish in 10s")
	}
	return job.Snapshot().Items
}

// TestJobOutlivesWaiter checks a job keeps running after its waiter gives up
// (the wait budget or a cancelled client request), reports partial state
// meanwhile, saves its image, and stays retrievable by ID until it expires.
func TestJobOutlivesWaiter(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = slowHandler(&peakTracker{}, 300*time.Millisecond)
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	job, err := e.StartGenerate([]GenerateRequest{{Prompt: "后台继续"}}, "info")
	if err != nil {
		t.Fatal(err)
	}
	// A cancelled waiter returns at once without touching the job.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if job.Wait(ctx, 0) {
		t.Fatal("job reported done immediately")
	}
	if job.Wait(context.Background(), 50*time.Millisecond) {
		t.Fatal("job done before the upstream answered")
	}
	snap := job.Snapshot()
	if snap.Done || snap.Finished[0] || snap.Progress[0] == "" {
		t.Errorf("partial snapshot wrong: %+v", snap)
	}
	items := waitItems(t, job)
	if items[0].Err != nil || len(items[0].Result.Images) != 1 {
		t.Fatalf("job result: %+v %v", items[0].Result, items[0].Err)
	}
	if total, _, _ := e.History(storage.Query{Keyword: "后台继续"}); total != 1 {
		t.Errorf("history has %d images, want 1", total)
	}
	got, ok := e.Job(job.ID)
	if !ok || got != job || got.Info != "info" {
		t.Fatalf("Job(%s) = %v %v", job.ID, got, ok)
	}
	// Expired jobs are pruned on lookup.
	job.mu.Lock()
	job.ended = time.Now().Add(-JobRetention - time.Minute)
	job.mu.Unlock()
	if _, ok := e.Job(job.ID); ok {
		t.Error("expired job still listed")
	}
}

// TestCloseCancelsJobs checks Engine.Close cancels a running job, which then
// ends with a cancellation error.
func TestCloseCancelsJobs(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = slowHandler(&peakTracker{}, 2*time.Second)
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	job, err := e.StartGenerate([]GenerateRequest{{Prompt: "关闭时取消"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	e.Close()
	items := waitItems(t, job)
	if apiErr, ok := imageapi.AsError(items[0].Err); !ok || apiErr.Kind != imageapi.KindCanceled {
		t.Errorf("job after Close: %v, want a cancellation", items[0].Err)
	}
}

// TestConcurrentCallsShareLimit checks separate Generate calls running at the
// same time share one limit set through Reload: with max_concurrency 1 they
// all succeed but reach the upstream one at a time.
func TestConcurrentCallsShareLimit(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	pt := &peakTracker{}
	u.Handler = slowHandler(pt, 100*time.Millisecond)
	e, _ := newEngine(t, prov("a", u, "openai", genModel))
	cfg := e.Config().Clone()
	cfg.Request.MaxConcurrency = 1
	e.Reload(&cfg)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = e.Generate(context.Background(), GenerateRequest{Prompt: "单独调用"}, nil)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := pt.peak.Load(); got != 1 {
		t.Errorf("peak = %d with max_concurrency 1", got)
	}
}

// TestLimiter checks the limiter's queueing, live limit changes and
// cancellation.
func TestLimiter(t *testing.T) {
	l := newLimiter(1)
	ctx := context.Background()
	if err := l.acquire(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// A second holder has to wait and says so once.
	got := make(chan error, 1)
	var waits atomic.Int32
	go func() { got <- l.acquire(ctx, func(active, limit int) { waits.Add(1) }) }()
	select {
	case err := <-got:
		t.Fatalf("second acquire did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Raising the limit lets it in without a release.
	l.setLimit(2)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("raising the limit did not wake the waiter")
	}
	if waits.Load() != 1 {
		t.Errorf("onWait called %d times, want 1", waits.Load())
	}

	// A cancelled wait returns the context error and holds nothing.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := l.acquire(cctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled acquire = %v", err)
	}
	l.release()
	l.release()
	if active, limit := l.stats(); active != 0 || limit != 2 {
		t.Errorf("stats = %d/%d, want 0/2", active, limit)
	}
}
