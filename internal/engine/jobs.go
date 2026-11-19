package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// JobRetention is how long a finished job stays retrievable by ID.
const JobRetention = time.Hour

// maxJobs caps how many jobs (running or finished but retained) the engine
// keeps; starting another job beyond it fails until old ones expire.
const maxJobs = 256

// Job is image work running in the background, detached from whoever started
// it: a caller that stops waiting, or an MCP client that times out and
// cancels, does not stop the job, so images that are nearly done are still
// saved to disk and history and can be fetched later by ID. A job holds one
// or more tasks that run concurrently under request.max_concurrency. It ends
// when every task has finished or the engine is closed.
type Job struct {
	// ID identifies the job for Engine.Job.
	ID string
	// Kind is "generate" or "edit".
	Kind string
	// Created is when the job started.
	Created time.Time
	// Info is opaque data the starter attaches, such as how to describe the
	// results; the engine never reads it.
	Info any

	done chan struct{}

	mu        sync.Mutex
	items     []BatchItem
	finished  []bool
	progress  []string
	remaining int
	ended     time.Time
	listeners map[int]BatchProgress
	nextL     int
}

// JobSnapshot is a consistent copy of a job's state.
type JobSnapshot struct {
	// Items holds each task's outcome; Result and Err are nil while it runs.
	Items []BatchItem
	// Finished reports, per task, whether it has ended.
	Finished []bool
	// Progress is each task's latest status message.
	Progress []string
	// Done reports whether every task has ended.
	Done bool
	// Elapsed is the time since the job started, or its total run time once
	// done.
	Elapsed time.Duration
}

// Done returns a channel closed when every task of the job has ended.
func (j *Job) Done() <-chan struct{} { return j.done }

// Wait blocks until the job is done, ctx is done, or d has passed. It never
// affects the job itself.
//
// ctx is the waiter's context (an MCP request); d is the longest wait, with
// d <= 0 meaning no limit other than ctx. It returns true when the job is
// done.
func (j *Job) Wait(ctx context.Context, d time.Duration) bool {
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-j.done:
		return true
	case <-ctx.Done():
	case <-timeout:
	}
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// Snapshot copies the job's current state under its lock, so callers can
// render it while tasks keep finishing. It returns the copy.
func (j *Job) Snapshot() JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := JobSnapshot{
		Items:    append([]BatchItem(nil), j.items...),
		Finished: append([]bool(nil), j.finished...),
		Progress: append([]string(nil), j.progress...),
		Done:     j.remaining == 0,
	}
	if s.Done {
		s.Elapsed = j.ended.Sub(j.Created)
	} else {
		s.Elapsed = time.Since(j.Created)
	}
	return s
}

// Listen registers fn to receive the job's progress messages until the
// returned stop function is called, first replaying each task's latest
// message so a listener attached after the tasks started (an MCP call that
// begins waiting, or get_image_job) still learns where each task is. Several
// listeners may be attached at once (for example two MCP calls waiting on the
// same job).
//
// fn receives the task index and message; it is called from task goroutines
// and must be safe for concurrent use. It returns a function that detaches
// fn; calling it more than once is harmless.
func (j *Job) Listen(fn BatchProgress) (stop func()) {
	j.mu.Lock()
	id := j.nextL
	j.nextL++
	j.listeners[id] = fn
	current := append([]string(nil), j.progress...)
	j.mu.Unlock()
	for i, msg := range current {
		if msg != "" {
			fn(i, msg)
		}
	}
	return func() {
		j.mu.Lock()
		delete(j.listeners, id)
		j.mu.Unlock()
	}
}

// report records task i's latest status and forwards it to every listener,
// outside the lock.
//
// i is the task index; msg is the status message. There is no return value.
func (j *Job) report(i int, msg string) {
	j.mu.Lock()
	j.progress[i] = msg
	fns := make([]BatchProgress, 0, len(j.listeners))
	for _, fn := range j.listeners {
		fns = append(fns, fn)
	}
	j.mu.Unlock()
	for _, fn := range fns {
		fn(i, msg)
	}
}

// finish stores task i's outcome and, when it is the last task, marks the job
// done and closes Done.
//
// i is the task index; item is its outcome. It returns how many tasks have
// ended and whether the job is now done.
func (j *Job) finish(i int, item BatchItem) (ended int, done bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.items[i], j.finished[i] = item, true
	j.remaining--
	if j.remaining == 0 {
		j.ended = time.Now()
		close(j.done)
	}
	return len(j.items) - j.remaining, j.remaining == 0
}

// StartGenerate validates and routes every text-to-image task, then runs them
// as a background job: concurrently, under request.max_concurrency, detached
// from any caller's context, saving images and history as they finish. A bad
// task rejects the whole job before any upstream call. Use Job.Wait and
// Job.Snapshot to collect the results, or look the job up later with
// Engine.Job.
//
// reqs are the tasks (1 to MaxBatchTasks; one for a single call); info is
// attached to the job unchanged. It returns the running job, or a validation
// error (naming the task when there are several), or an error when too many
// jobs are retained.
func (e *Engine) StartGenerate(reqs []GenerateRequest, info any) (*Job, error) {
	jobs, err := e.planGenerateBatch(reqs)
	if err != nil {
		return nil, err
	}
	return e.launch("generate", jobs, info)
}

// StartEdit is StartGenerate for image-to-image tasks: reference images and
// masks are loaded (shared images once) and providers resolved before the
// job starts, so a missing file or a model that cannot edit rejects the whole
// job up front.
//
// reqs are the tasks (1 to MaxBatchTasks); info is attached to the job. It
// returns the running job or an error as for StartGenerate.
func (e *Engine) StartEdit(reqs []EditRequest, info any) (*Job, error) {
	jobs, err := e.planEditBatch(reqs)
	if err != nil {
		return nil, err
	}
	return e.launch("edit", jobs, info)
}

// Job returns the job with that ID while it runs and for JobRetention after
// it ends, pruning expired jobs first.
//
// id is Job.ID. It returns the job and true, or false when it is unknown or
// expired.
func (e *Engine) Job(id string) (*Job, bool) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	e.pruneJobsLocked(time.Now())
	j, ok := e.jobs[id]
	return j, ok
}

// launch registers a job for planned tasks and starts one goroutine per task
// on the engine's background context, so only Engine.Close (not a caller)
// can cancel it. Each task logs its own outcome in run; launch adds a log
// line when the whole job ends.
//
// kind is "generate" or "edit"; jobs are the planned tasks; info is attached
// to the job. It returns the job, or an error when maxJobs jobs are already
// retained.
func (e *Engine) launch(kind string, jobs []*job, info any) (*Job, error) {
	n := len(jobs)
	j := &Job{
		ID: newJobID(), Kind: kind, Created: time.Now(), Info: info,
		done:  make(chan struct{}),
		items: make([]BatchItem, n), finished: make([]bool, n), progress: make([]string, n),
		remaining: n, listeners: map[int]BatchProgress{},
	}
	e.jobsMu.Lock()
	e.pruneJobsLocked(j.Created)
	if len(e.jobs) >= maxJobs {
		e.jobsMu.Unlock()
		return nil, badRequest(fmt.Sprintf("后台任务过多（%d 个），请等待已有任务完成", maxJobs))
	}
	e.jobs[j.ID] = j
	e.jobsMu.Unlock()

	e.log.Info("image job started", "job", j.ID, "kind", kind, "tasks", n)
	for i, planned := range jobs {
		go func() {
			start := time.Now()
			res, err := e.run(e.base, planned, func(msg string) { j.report(i, msg) })
			ended, done := j.finish(i, BatchItem{Result: res, Err: err, Elapsed: time.Since(start)})
			if err != nil {
				j.report(i, fmt.Sprintf("失败（已结束 %d/%d）：%v", ended, n, err))
			} else {
				j.report(i, fmt.Sprintf("完成，%d 张（已结束 %d/%d）", len(res.Images), ended, n))
			}
			if done {
				e.logJobEnd(j)
			}
		}()
	}
	return j, nil
}

// logJobEnd writes one line summarizing a finished job. There is no return
// value.
//
// j is a job whose tasks have all ended.
func (e *Engine) logJobEnd(j *Job) {
	s := j.Snapshot()
	ok := 0
	for _, it := range s.Items {
		if it.Err == nil {
			ok++
		}
	}
	e.log.Info("image job finished", "job", j.ID, "kind", j.Kind, "tasks", len(s.Items), "succeeded", ok, "failed", len(s.Items)-ok, "elapsed", s.Elapsed.Round(time.Millisecond))
}

// pruneJobsLocked drops jobs that ended more than JobRetention before now.
// The caller must hold e.jobsMu.
//
// now is the reference time. There is no return value.
func (e *Engine) pruneJobsLocked(now time.Time) {
	for id, j := range e.jobs {
		j.mu.Lock()
		expired := j.remaining == 0 && now.Sub(j.ended) > JobRetention
		j.mu.Unlock()
		if expired {
			delete(e.jobs, id)
		}
	}
}

// Close cancels every background job (their running upstream requests are
// aborted and they end as cancelled). Call it once when the app exits; the
// engine must not start new jobs afterwards. There is no return value.
func (e *Engine) Close() { e.stop() }

// newJobID returns a random job ID such as "job_1a2b3c4d5e6f".
func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}
