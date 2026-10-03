package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryFixedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory(ctx)
	rule := Rule{Limit: 3, Window: 150 * time.Millisecond}

	for i := range 3 {
		if d, _ := m.Allow(ctx, "k", rule); !d.Allowed {
			t.Fatalf("request %d denied", i)
		}
	}
	d, _ := m.Allow(ctx, "k", rule)
	if d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > rule.Window {
		t.Fatalf("4th request: %+v", d)
	}
	if d, _ := m.Allow(ctx, "other", rule); !d.Allowed {
		t.Fatal("keys must not share counters")
	}
	time.Sleep(200 * time.Millisecond)
	if d, _ := m.Allow(ctx, "k", rule); !d.Allowed {
		t.Fatal("window did not reset")
	}
}

func TestMemoryIsExactUnderConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory(ctx)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _ := m.Allow(ctx, "k", Rule{Limit: 100, Window: time.Minute}); d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 100 {
		t.Fatalf("allowed %d, want exactly 100", allowed.Load())
	}
}

type failing struct{}

func (failing) Allow(context.Context, string, Rule) (Decision, error) {
	return Decision{}, errors.New("backend down")
}

func TestResilientFallsBackToMemory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var errs atomic.Int32
	l := &Resilient{
		Primary: failing{}, Fallback: NewMemory(ctx), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnError: func() { errs.Add(1) },
	}
	rule := Rule{Limit: 1, Window: time.Minute}
	if d, err := l.Allow(ctx, "k", rule); err != nil || !d.Allowed {
		t.Fatalf("%v %+v", err, d)
	}
	if d, _ := l.Allow(ctx, "k", rule); d.Allowed {
		t.Fatal("the fallback must still enforce limits (fail closed, not open)")
	}
	if errs.Load() != 2 {
		t.Fatalf("errors counted = %d", errs.Load())
	}
}

type fakeCounter struct{ n int64 }

func (f *fakeCounter) Incr(context.Context, string, time.Duration) (int64, time.Duration, error) {
	f.n++
	return f.n, time.Second, nil
}

func TestStoreLimiter(t *testing.T) {
	l := Store{C: &fakeCounter{}}
	rule := Rule{Limit: 2, Window: time.Minute}
	for i, want := range []bool{true, true, false} {
		if d, _ := l.Allow(context.Background(), "k", rule); d.Allowed != want {
			t.Fatalf("call %d: %+v", i, d)
		}
	}
}
