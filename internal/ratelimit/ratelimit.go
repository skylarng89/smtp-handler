// Package ratelimit provides fixed-window rate limiters with memory, store
// (SQL) and Redis backends. The backend is chosen by deployment shape: the
// shared ones give exact limits across replicas.
package ratelimit

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/skylarng89/smtp-handler/internal/config"
)

// Rule is Limit events per Window.
type Rule struct {
	Limit  int
	Window time.Duration
}

func FromConfig(r *config.Rule) (Rule, bool) {
	if !r.Enabled() {
		return Rule{}, false
	}
	return Rule{Limit: r.Limit, Window: r.Window.Std()}, true
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration // time until the window resets
}

// Limiter counts one event against key and reports whether it is within rule.
// Callers namespace keys by rule (e.g. "ip:<addr>").
type Limiter interface {
	Allow(ctx context.Context, key string, r Rule) (Decision, error)
}

// Memory is a sharded in-process limiter. Windows start at the first event.
type Memory struct {
	shards [32]memShard
}

type memShard struct {
	mu      sync.Mutex
	entries map[string]*memEntry
}

type memEntry struct {
	count   int
	resetAt time.Time
}

// NewMemory starts the limiter; its janitor stops when ctx is cancelled.
func NewMemory(ctx context.Context) *Memory {
	m := &Memory{}
	for i := range m.shards {
		m.shards[i].entries = make(map[string]*memEntry)
	}
	go m.janitor(ctx)
	return m
}

func (m *Memory) shard(key string) *memShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &m.shards[h.Sum32()%uint32(len(m.shards))]
}

func (m *Memory) Allow(_ context.Context, key string, r Rule) (Decision, error) {
	now := time.Now()
	sh := m.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := sh.entries[key]
	if e == nil || !now.Before(e.resetAt) {
		e = &memEntry{resetAt: now.Add(r.Window)}
		sh.entries[key] = e
	}
	e.count++
	return Decision{Allowed: e.count <= r.Limit, RetryAfter: e.resetAt.Sub(now)}, nil
}

func (m *Memory) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for i := range m.shards {
				sh := &m.shards[i]
				sh.mu.Lock()
				for k, e := range sh.entries {
					if !now.Before(e.resetAt) {
						delete(sh.entries, k)
					}
				}
				sh.mu.Unlock()
			}
		}
	}
}

// Counter is the slice of store.Store the SQL limiter needs.
type Counter interface {
	Incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error)
}

// Store limits through the shared database; exact across replicas.
type Store struct{ C Counter }

func (s Store) Allow(ctx context.Context, key string, r Rule) (Decision, error) {
	count, remaining, err := s.C.Incr(ctx, key+"|"+r.Window.String(), r.Window)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: count <= int64(r.Limit), RetryAfter: remaining}, nil
}

// Resilient wraps a shared limiter. If the shared backend fails it degrades
// to a per-node limiter instead of either blocking all mail or letting
// everything through.
type Resilient struct {
	Primary  Limiter
	Fallback Limiter
	Log      *slog.Logger
	OnError  func()

	mu       sync.Mutex
	lastWarn time.Time
}

func (l *Resilient) Allow(ctx context.Context, key string, r Rule) (Decision, error) {
	d, err := l.Primary.Allow(ctx, key, r)
	if err == nil {
		return d, nil
	}
	if l.OnError != nil {
		l.OnError()
	}
	l.mu.Lock()
	if time.Since(l.lastWarn) > 30*time.Second {
		l.lastWarn = time.Now()
		l.Log.Warn("rate-limit backend unavailable; using per-node limits", "error", err)
	}
	l.mu.Unlock()
	return l.Fallback.Allow(ctx, key, r)
}
