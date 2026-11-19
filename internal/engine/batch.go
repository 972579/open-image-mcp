package engine

import (
	"fmt"
	"time"

	"github.com/972579/open-image-mcp/internal/imageapi"
)

// MaxBatchTasks caps how many tasks one job may carry.
const MaxBatchTasks = 16

// BatchItem is the outcome of one task of a job: Result on success, or Err
// (usually a *Failure carrying every attempt) on failure. Both are nil while
// the task is still running.
type BatchItem struct {
	Result *Result
	Err    error
	// Elapsed is how long the task took, queueing for a slot included.
	Elapsed time.Duration
}

// BatchProgress receives status updates for one task of a job. index is the
// task's position in the request slice; msg is human-readable. It is called
// from several goroutines at once and must be safe for that.
type BatchProgress func(index int, msg string)

// planGenerateBatch validates and routes every text-to-image task against one
// configuration snapshot, without calling any upstream, so one bad task
// rejects the whole job before anything is spent.
//
// reqs are the tasks (1 to MaxBatchTasks). It returns the planned jobs in
// order, or the first error; with several tasks the error names the task.
func (e *Engine) planGenerateBatch(reqs []GenerateRequest) ([]*job, error) {
	if err := checkBatchSize(len(reqs)); err != nil {
		return nil, err
	}
	cfg := e.Config()
	jobs := make([]*job, len(reqs))
	for i, req := range reqs {
		j, err := e.planGenerate(cfg, req)
		if err != nil {
			return nil, taskError(i, len(reqs), err)
		}
		jobs[i] = j
	}
	return jobs, nil
}

// planEditBatch validates every image-to-image task, loads its reference
// images and mask and resolves its providers before any upstream call, so a
// missing file or a model that cannot edit rejects the whole job. A reference
// image used by several tasks (the same path or base64 in the same position)
// is read into memory once and shared.
//
// reqs are the tasks (1 to MaxBatchTasks), each with 1-16 images. It returns
// the planned jobs in order, or the first error; with several tasks the error
// names the task.
func (e *Engine) planEditBatch(reqs []EditRequest) ([]*job, error) {
	if err := checkBatchSize(len(reqs)); err != nil {
		return nil, err
	}
	cfg := e.Config()
	load := cachedLoader()
	jobs := make([]*job, len(reqs))
	for i, req := range reqs {
		j, err := e.planEdit(cfg, req, load)
		if err != nil {
			return nil, taskError(i, len(reqs), err)
		}
		jobs[i] = j
	}
	return jobs, nil
}

// taskError prefixes a planning error with the task number when the job has
// several tasks, so a single call's error reads exactly as before.
//
// i is the task's index; n is the number of tasks; err is the planning
// error, kept wrapped for errors.As.
func taskError(i, n int, err error) error {
	if n == 1 {
		return err
	}
	return fmt.Errorf("第 %d 个任务：%w", i+1, err)
}

// checkBatchSize rejects an empty job or one with more than MaxBatchTasks
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
// image data after loading, so sharing it between tasks is safe. The cache
// lives as long as the returned function, i.e. one job's planning.
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
