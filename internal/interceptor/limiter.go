package interceptor

import (
	"context"
	"sync"
)

// dynamicLimiter is shared by every configuration generation. Calls already
// admitted before a lower limit are allowed to finish, but no new call is
// admitted until the combined old/current active count is below the new
// limit. This preserves leases without letting generations multiply the
// configured concurrency budget.
type dynamicLimiter struct {
	mu     sync.Mutex
	limit  int
	active int
	closed bool
	wake   chan struct{}
	epoch  uint64
}

func newDynamicLimiter() *dynamicLimiter {
	return &dynamicLimiter{closed: true, wake: make(chan struct{})}
}

func (l *dynamicLimiter) configure(limit int) {
	if limit < 1 {
		limit = 1
	}
	l.mu.Lock()
	l.limit = limit
	l.closed = false
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *dynamicLimiter) Acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	epoch := l.epoch
	l.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		if l.closed || l.epoch != epoch {
			l.mu.Unlock()
			return ErrRuntimeUnavailable
		}
		if l.active < l.limit {
			l.active++
			l.mu.Unlock()
			return nil
		}
		wake := l.wake
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (l *dynamicLimiter) Release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *dynamicLimiter) shutdown() {
	l.mu.Lock()
	l.closed = true
	l.epoch++
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *dynamicLimiter) notifyLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}
