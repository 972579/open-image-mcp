package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/972579/open-image-mcp/internal/imageapi"
)

// MaxBatchTasks caps how many tasks one GenerateBatch or EditBatch call may
// carry.
const MaxBatchTasks = 16

// BatchItem is the outcome of one task in a batch: Result on success, or Err
// (usually a *Failure carrying every attempt) on failure.
type BatchItem struct {
	Result *Result
	Err    error
	// Elapsed is how long the task took, queueing for a slot included.
	Elapsed time.Duration
}

// BatchProgress receives status updates for one task of a batch. index is the
// task's position in the request slice; msg is human-readable. It is called
// from several goroutines at once and must be safe for that.
type BatchProgress func(index int, msg string)

// GenerateBatch runs several text-to-image calls concurrently and waits for
// all of them. Every task is validated and routed first; if any task is
// invalid the whole batch is rejected before an upstream is called, so a typo
// in one prompt never spends money on the others. Valid tasks then run in
// parallel, each with its own retries and fallback, sharing the engine's
// request.max_concurrency slots with every other call (tasks beyond the
// limit queue). A failing task does not stop the others. Images are saved and
// recorded in history exactly as single Generate calls would.
//
// ctx cancels every task; reqs are the tasks (1 to MaxBatchTasks), each
// filled with the configured defaults like a single call; progress, when not
// nil, receives per-task status updates and a note as each task finishes. It
// returns one BatchItem per task, in the order of reqs, or a validation error
// naming the first bad task (then nothing ran).
func (e *Engine) GenerateBatch(ctx context.Context, reqs []GenerateRequest, progress BatchProgress) ([]BatchItem, error) {
	if err := checkBatchSize(len(reqs)); err != nil {
		return nil, err
	}
	cfg := e.Config()
	jobs := make([]*job, len(reqs))
	for i, req := range reqs {
		j, err := e.planGenerate(cfg, req)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个任务：%w", i+1, err)
		}
		jobs[i] = j
	}
	return e.runBatch(ctx, "generate", jobs, progress), nil
}

// EditBatch runs several image-to-image calls concurrently and waits for all
// of them, with the same rules as GenerateBatch: every task is validated, its
// reference images and mask are loaded and its providers resolved before any
// upstream call (a missing file or a model that cannot edit rejects the whole
// batch), then the tasks run in parallel under request.max_concurrency and a
// failing task does not stop the others. A reference image used by several
// tasks (the same path or base64 in the same position) is read into memory
// once and shared, so editing many variants of one picture does not multiply
// memory use.
//
// ctx cancels every task; reqs are the tasks (1 to MaxBatchTasks), each with
// 1-16 images; progress, when not nil, receives per-task status updates and a
// note as each task finishes. It returns one BatchItem per task, in the order
// of reqs, or an error naming the first bad task (then nothing ran).
func (e *Engine) EditBatch(ctx context.Context, reqs []EditRequest, progress BatchProgress) ([]BatchItem, error) {
	if err := checkBatchSize(len(reqs)); err != nil {
		return nil, err
	}
	cfg := e.Config()
	load := cachedLoader()
	jobs := make([]*job, len(reqs))
	for i, req := range reqs {
		j, err := e.planEdit(cfg, req, load)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个任务：%w", i+1, err)
		}
		jobs[i] = j
	}
	return e.runBatch(ctx, "edit", jobs, progress), nil
}

// checkBatchSize rejects an empty batch or one with more than MaxBatchTasks
// tasks.
//
// n is the number of tasks. It returns a bad-request error, or nil.
func checkBatchSize(n int) error {
	if n == 0 {
		return badRequest("tasks 至少需要一个任务")
	}
	if n > MaxBatchTasks {
		return badRequest(fmt.Sprintf("tasks 最多 %d 个，当前 %d 个", MaxBatchTasks, n))
	}
	return nil
}

// cachedLoader returns an imageLoader that reads each distinct (name, source)
// pair once and hands every later request the same bytes. Nothing writes to
// image data after loading, so sharing it between tasks is safe. The cache lives as
// long as the returned function, i.e. one batch.
func cachedLoader() imageLoader {
	cache := map[string]imageapi.InputImage{}
	return func(src, name string) (imageapi.InputImage, error) {
		key := name + "\x00" + src
		if img, ok := cache[key]; ok {
			return img, nil
		}
		img, err := LoadInputImage(src, name)
		if err != nil {
			return imageapi.InputImage{}, err
		}
		cache[key] = img
		return img, nil
	}
}

// runBatch runs planned jobs in parallel, one goroutine each, and waits for
// all of them. Upstream concurrency is bounded inside run by the shared
// slots, so the goroutines simply queue. Each job's outcome lands in its own
// slot of the result, in job order, and progress hears a closing note per
// job with how many have finished.
//
// ctx cancels every job; kind ("generate" or "edit") is for the log; jobs are
// the planned tasks; progress may be nil. It returns one BatchItem per job.
func (e *Engine) runBatch(ctx context.Context, kind string, jobs []*job, progress BatchProgress) []BatchItem {
	total := len(jobs)
	items := make([]BatchItem, total)
	var finished atomic.Int32
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Go(func() {
			var p Progress
			if progress != nil {
				p = func(msg string) { progress(i, msg) }
			}
			start := time.Now()
			res, err := e.run(ctx, j, p)
			items[i] = BatchItem{Result: res, Err: err, Elapsed: time.Since(start)}
			done := finished.Add(1)
			if progress != nil {
				if err != nil {
					progress(i, fmt.Sprintf("失败（已结束 %d/%d）：%v", done, total, err))
				} else {
					progress(i, fmt.Sprintf("完成，%d 张（已结束 %d/%d）", len(res.Images), done, total))
				}
			}
		})
	}
	wg.Wait()
	ok := 0
	for _, it := range items {
		if it.Err == nil {
			ok++
		}
	}
	e.log.Info("image batch finished", "kind", kind, "tasks", total, "succeeded", ok, "failed", total-ok)
	return items
}
