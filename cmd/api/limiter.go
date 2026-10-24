package main

import (
	"context"
	"sync"
)

// limiter is a counting semaphore whose capacity can change at runtime
// (the max_ffmpeg_jobs setting). Shrinking never revokes held slots: new
// acquires just wait until enough are released.
type limiter struct {
	mu   sync.Mutex
	max  int
	used int
	wake chan struct{} // closed and replaced whenever a slot may be free
}

func newLimiter(n int) *limiter {
	l := &limiter{wake: make(chan struct{})}
	l.SetMax(n)
	return l
}

// SetMax sets the capacity (min 1) and wakes waiters so a grow takes effect
// immediately.
func (l *limiter) SetMax(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	l.max = n
	l.signalLocked()
	l.mu.Unlock()
}

func (l *limiter) signalLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

// Acquire blocks until a slot is free or ctx is done.
func (l *limiter) Acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		if l.used < l.max {
			l.used++
			l.mu.Unlock()
			return nil
		}
		wake := l.wake
		l.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release returns a slot taken by Acquire.
func (l *limiter) Release() {
	l.mu.Lock()
	l.used--
	l.signalLocked()
	l.mu.Unlock()
}
