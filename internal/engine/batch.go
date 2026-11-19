package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MaxBatchTasks caps how many tasks one GenerateBatch call may carry.
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
	if len(reqs) == 0 {
		return nil, badRequest("tasks 至少需要一个任务")
	}
	if len(reqs) > MaxBatchTasks {
		return nil, badRequest(fmt.Sprintf("tasks 最多 %d 个，当前 %d 个", MaxBatchTasks, len(reqs)))
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
	e.log.Info("image batch finished", "tasks", total, "succeeded", ok, "failed", total-ok)
	return items, nil
}
