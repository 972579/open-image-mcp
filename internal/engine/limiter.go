package engine

import (
	"context"
	"sync"
)

// limiter caps how many upstream requests run at once. Unlike a buffered
// channel its limit can change while requests are in flight: lowering it lets
// running requests finish and only holds back new ones; raising it wakes
// waiters at once. It is safe for concurrent use.
type limiter struct {
	mu     sync.Mutex
	active int
	limit  int
	// wake is closed (and replaced) whenever a slot frees or the limit
	// changes, so every waiter re-checks.
	wake chan struct{}
}

// newLimiter returns a limiter allowing n concurrent holders (at least one).
//
// n is the initial limit; values below 1 are treated as 1.
func newLimiter(n int) *limiter {
	return &limiter{limit: max(n, 1), wake: make(chan struct{})}
}

// setLimit changes the maximum number of holders and wakes every waiter so
// the new limit applies right away. Holders above a lowered limit keep their
// slot until they release it.
//
// n is the new limit; values below 1 are treated as 1. There is no return
// value.
func (l *limiter) setLimit(n int) {
	l.mu.Lock()
	l.limit = max(n, 1)
	l.broadcastLocked()
	l.mu.Unlock()
}

// acquire takes a slot, waiting until one is free or ctx is done. Every
// successful acquire must be paired with one release.
//
// ctx cancels the wait; onWait, when not nil, is called once (outside the
// lock) with the number of running requests and the limit if the caller has
// to queue, so it can tell the user why nothing is happening yet. It returns
// nil once a slot is held, or ctx's error (and then holds no slot).
func (l *limiter) acquire(ctx context.Context, onWait func(active, limit int)) error {
	notified := false
	for {
		l.mu.Lock()
		if l.active < l.limit {
			l.active++
			l.mu.Unlock()
			return nil
		}
		active, limit, wake := l.active, l.limit, l.wake
		l.mu.Unlock()
		if !notified && onWait != nil {
			onWait(active, limit)
			notified = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

// release gives back a slot taken by acquire and wakes the waiters. There is
// no return value.
func (l *limiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.broadcastLocked()
	l.mu.Unlock()
}

// stats returns how many slots are held and the current limit, for status
// displays and tests.
func (l *limiter) stats() (active, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.limit
}

// broadcastLocked wakes every goroutine blocked in acquire. The caller must
// hold l.mu.
func (l *limiter) broadcastLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}
