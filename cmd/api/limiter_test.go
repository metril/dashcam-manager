package main

import (
	"context"
	"testing"
	"time"
)

func TestLimiterResize(t *testing.T) {
	l := newLimiter(1)
	ctx := context.Background()
	if err := l.Acquire(ctx); err != nil {
		t.Fatal(err)
	}

	// At capacity: a second acquire blocks until ctx expires.
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	if err := l.Acquire(short); err == nil {
		t.Fatal("acquire beyond max should block")
	}
	cancel()

	// Growing wakes a blocked waiter.
	got := make(chan error, 1)
	go func() { got <- l.Acquire(ctx) }()
	time.Sleep(20 * time.Millisecond)
	l.SetMax(2)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("grow did not wake the waiter")
	}

	// Shrinking keeps held slots; new acquires wait until used < max.
	l.SetMax(1)
	short, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
	if err := l.Acquire(short); err == nil {
		t.Fatal("acquire should block while used (2) >= max (1)")
	}
	cancel()
	l.Release()
	l.Release()
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("acquire after releases: %v", err)
	}
	l.SetMax(0) // clamps to 1
	if l.max != 1 {
		t.Errorf("max = %d, want clamp to 1", l.max)
	}
}
