// Package storetest is a driver-agnostic contract suite for store.Store.
// Every implementation (SQLite, PostgreSQL) must pass it unchanged.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/skylarng89/smtp-handler/internal/store"
)

// Factory returns a fresh, migrated, empty store.
type Factory func(t *testing.T) store.Store

func Run(t *testing.T, newStore Factory) {
	t.Helper()
	tests := map[string]func(*testing.T, store.Store){
		"EnqueueAndGet":                 testEnqueueAndGet,
		"IdempotencyConcurrent":         testIdempotencyConcurrent,
		"IdempotencyMismatchAndExpiry":  testIdempotencyMismatchAndExpiry,
		"ClaimIsExclusive":              testClaimIsExclusive,
		"FencingRejectsStaleWorker":     testFencing,
		"RetryExhaustion":               testRetryExhaustion,
		"ReleaseDoesNotConsumeAttempt":  testRelease,
		"PoisonMessageFailsAfterExpiry": testPoison,
		"ExcludeProjects":               testExcludeProjects,
		"AdminRetryCancel":              testAdminRetryCancel,
		"AdminConcurrentRetry":          testAdminConcurrentRetry,
		"DropBody":                      testDropBody,
		"ListFilters":                   testList,
		"CountersAreExact":              testCounters,
		"SlotsAreBounded":               testSlots,
		"Purge":                         testPurge,
		"ReleaseOwner":                  testReleaseOwner,
		"SchemaOK":                      func(t *testing.T, s store.Store) { must(t, s.SchemaOK(ctx())) },
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			fn(t, s)
		})
	}
}

func ctx() context.Context { return context.Background() }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func newMsg(project string) store.NewMessage {
	id := ulid.Make().String()
	return store.NewMessage{
		ID: id, ProjectID: project, Template: "t", KeyID: "k1", MessageIDHeader: id + "@example.com",
		Subject: "Hello", From: "a@example.com", To: "b@example.com",
		Payload: []byte(`{"subject":"Hello"}`), MaxAttempts: 3, MaxAge: time.Hour,
	}
}

func enqueue(t *testing.T, s store.Store, m store.NewMessage) {
	t.Helper()
	_, err := s.Enqueue(ctx(), m, nil)
	must(t, err)
}

func claimOne(t *testing.T, s store.Store, owner string) store.Job {
	t.Helper()
	jobs, err := s.Claim(ctx(), owner, 1, time.Minute, nil)
	must(t, err)
	if len(jobs) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

func attempt(n int) store.Attempt {
	now := time.Now()
	return store.Attempt{Attempt: n, Node: "n1", StartedAt: now, FinishedAt: now, Outcome: "transient", Error: "boom"}
}

func testEnqueueAndGet(t *testing.T, s store.Store) {
	m := newMsg("p")
	res, err := s.Enqueue(ctx(), m, nil)
	must(t, err)
	if res.ID != m.ID || res.Replayed {
		t.Fatalf("unexpected result %+v", res)
	}
	got, err := s.Get(ctx(), m.ID)
	must(t, err)
	if got.State != store.StateQueued || got.Version != 1 || got.Subject != "Hello" || got.MaxAttempts != 3 {
		t.Fatalf("unexpected message %+v", got)
	}
	payload, err := s.GetPayload(ctx(), m.ID)
	must(t, err)
	if string(payload) != string(m.Payload) {
		t.Fatalf("payload = %s", payload)
	}
	_, err = s.Get(ctx(), "missing")
	wantErr(t, err, store.ErrNotFound)
}

func testIdempotencyConcurrent(t *testing.T, s store.Store) {
	const workers = 50
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ids      = map[string]int{}
		replayed atomic.Int32
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Enqueue(ctx(), newMsg("p"), &store.Idempotency{Key: "k", Fingerprint: "fp", TTL: time.Hour})
			if err != nil {
				t.Errorf("enqueue: %v", err)
				return
			}
			if res.Replayed {
				replayed.Add(1)
			}
			mu.Lock()
			ids[res.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 1 {
		t.Fatalf("got %d distinct message IDs, want exactly 1: %v", len(ids), ids)
	}
	if replayed.Load() != workers-1 {
		t.Fatalf("replayed = %d, want %d", replayed.Load(), workers-1)
	}
	list, err := s.List(ctx(), store.ListFilter{ProjectID: "p"})
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("stored %d messages, want 1", len(list))
	}
}

func testIdempotencyMismatchAndExpiry(t *testing.T, s store.Store) {
	_, err := s.Enqueue(ctx(), newMsg("p"), &store.Idempotency{Key: "k", Fingerprint: "a", TTL: time.Hour})
	must(t, err)
	_, err = s.Enqueue(ctx(), newMsg("p"), &store.Idempotency{Key: "k", Fingerprint: "b", TTL: time.Hour})
	wantErr(t, err, store.ErrIdempotencyMismatch)

	if _, err := s.FindIdempotent(ctx(), "p", "k", "a"); err != nil {
		t.Fatalf("find: %v", err)
	}
	_, err = s.FindIdempotent(ctx(), "p", "k", "b")
	wantErr(t, err, store.ErrIdempotencyMismatch)
	_, err = s.FindIdempotent(ctx(), "p", "other", "a")
	wantErr(t, err, store.ErrNotFound)

	// Keys are scoped per project.
	res, err := s.Enqueue(ctx(), newMsg("q"), &store.Idempotency{Key: "k", Fingerprint: "b", TTL: time.Hour})
	must(t, err)
	if res.Replayed {
		t.Fatal("idempotency leaked across projects")
	}

	// An expired key is reusable (expiry uses the database clock).
	_, err = s.Enqueue(ctx(), newMsg("p"), &store.Idempotency{Key: "short", Fingerprint: "a", TTL: 20 * time.Millisecond})
	must(t, err)
	time.Sleep(120 * time.Millisecond)
	res, err = s.Enqueue(ctx(), newMsg("p"), &store.Idempotency{Key: "short", Fingerprint: "different", TTL: time.Hour})
	must(t, err)
	if res.Replayed {
		t.Fatal("expired idempotency key should not replay")
	}
}

func testClaimIsExclusive(t *testing.T, s store.Store) {
	const total = 120
	for range total {
		enqueue(t, s, newMsg("p"))
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]string{}
	)
	for w := range 6 {
		owner := fmt.Sprintf("node-%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				jobs, err := s.Claim(ctx(), owner, 7, time.Minute, nil)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				mu.Lock()
				for _, j := range jobs {
					if prev, dup := seen[j.ID]; dup {
						t.Errorf("job %s claimed by both %s and %s", j.ID, prev, owner)
					}
					seen[j.ID] = owner
					if j.State != store.StateSending || j.Attempt != 1 || j.LeaseToken == "" || len(j.Payload) == 0 {
						t.Errorf("bad claimed job %+v", j)
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("claimed %d jobs, want %d", len(seen), total)
	}
}

func testFencing(t *testing.T, s store.Store) {
	m := newMsg("p")
	enqueue(t, s, m)

	first, err := s.Claim(ctx(), "slow-node", 1, 700*time.Millisecond, nil)
	must(t, err)
	if len(first) != 1 {
		t.Fatalf("first claim: %d", len(first))
	}
	// While the lease is live nobody else can take the job.
	none, err := s.Claim(ctx(), "other", 1, time.Minute, nil)
	must(t, err)
	if len(none) != 0 {
		t.Fatal("a live lease was claimed by another node")
	}

	time.Sleep(1200 * time.Millisecond) // lease expires on the database clock
	second, err := s.Claim(ctx(), "fast-node", 1, time.Minute, nil)
	must(t, err)
	if len(second) != 1 || second[0].Attempt != 2 || second[0].LeaseToken == first[0].LeaseToken {
		t.Fatalf("reclaim failed: %+v", second)
	}

	// The stale worker must not be able to complete, renew or fail the job.
	wantErr(t, s.MarkSent(ctx(), m.ID, first[0].LeaseToken, attempt(1), false), store.ErrLeaseLost)
	wantErr(t, s.RenewLease(ctx(), m.ID, first[0].LeaseToken, time.Minute), store.ErrLeaseLost)
	wantErr(t, s.MarkFailed(ctx(), m.ID, first[0].LeaseToken, attempt(1)), store.ErrLeaseLost)
	_, err = s.MarkRetry(ctx(), m.ID, first[0].LeaseToken, time.Second, attempt(1))
	wantErr(t, err, store.ErrLeaseLost)

	must(t, s.RenewLease(ctx(), m.ID, second[0].LeaseToken, time.Minute))
	must(t, s.MarkSent(ctx(), m.ID, second[0].LeaseToken, attempt(2), false))

	got, err := s.Get(ctx(), m.ID)
	must(t, err)
	if got.State != store.StateSent || got.SentAt == nil || got.LeaseOwner != "" {
		t.Fatalf("unexpected final state %+v", got)
	}
	// A completed job cannot be completed twice.
	wantErr(t, s.MarkSent(ctx(), m.ID, second[0].LeaseToken, attempt(2), false), store.ErrLeaseLost)
	atts, err := s.Attempts(ctx(), m.ID)
	must(t, err)
	if len(atts) != 1 || atts[0].Attempt != 2 {
		t.Fatalf("attempts = %+v", atts)
	}
}

func testRetryExhaustion(t *testing.T, s store.Store) {
	m := newMsg("p")
	m.MaxAttempts = 2
	enqueue(t, s, m)

	j := claimOne(t, s, "n")
	state, err := s.MarkRetry(ctx(), m.ID, j.LeaseToken, 0, attempt(1))
	must(t, err)
	if state != store.StateRetryScheduled {
		t.Fatalf("state = %s, want retry_scheduled", state)
	}

	j = claimOne(t, s, "n")
	if j.Attempt != 2 {
		t.Fatalf("attempt = %d", j.Attempt)
	}
	state, err = s.MarkRetry(ctx(), m.ID, j.LeaseToken, 0, attempt(2))
	must(t, err)
	if state != store.StateFailed {
		t.Fatalf("state = %s, want failed after exhausting attempts", state)
	}
	got, _ := s.Get(ctx(), m.ID)
	if got.LastError != "boom" || got.LastErrorClass != "transient" {
		t.Fatalf("last error not recorded: %+v", got)
	}

	// A retry delay beyond the message's max age fails immediately.
	m2 := newMsg("p")
	m2.MaxAge = time.Second
	enqueue(t, s, m2)
	j = claimOne(t, s, "n")
	state, err = s.MarkRetry(ctx(), m2.ID, j.LeaseToken, time.Hour, attempt(1))
	must(t, err)
	if state != store.StateFailed {
		t.Fatalf("state = %s, want failed past max age", state)
	}
}

func testRelease(t *testing.T, s store.Store) {
	m := newMsg("p")
	enqueue(t, s, m)
	j := claimOne(t, s, "n")
	must(t, s.Release(ctx(), m.ID, j.LeaseToken, 0, nil))

	got, err := s.Get(ctx(), m.ID)
	must(t, err)
	if got.Attempt != 0 || got.State != store.StateRetryScheduled {
		t.Fatalf("release consumed an attempt: %+v", got)
	}

	// A release that carries an attempt records why the job was deferred.
	j = claimOne(t, s, "n")
	why := attempt(1)
	why.Outcome, why.Error, why.SMTPCode = "config", "bad credentials", 535
	must(t, s.Release(ctx(), m.ID, j.LeaseToken, 0, &why))
	got, err = s.Get(ctx(), m.ID)
	must(t, err)
	if got.LastErrorClass != "config" || got.LastError != "bad credentials" || got.LastErrorCode != 535 || got.Attempt != 0 {
		t.Fatalf("deferral reason not recorded: %+v", got)
	}
	// A delayed release is not immediately claimable.
	j = claimOne(t, s, "n")
	must(t, s.Release(ctx(), m.ID, j.LeaseToken, time.Hour, nil))
	jobs, err := s.Claim(ctx(), "n", 1, time.Minute, nil)
	must(t, err)
	if len(jobs) != 0 {
		t.Fatal("released job was claimable before its delay elapsed")
	}
}

func testPoison(t *testing.T, s store.Store) {
	m := newMsg("p")
	m.MaxAttempts = 1
	enqueue(t, s, m)
	_, err := s.Claim(ctx(), "crashy", 1, 20*time.Millisecond, nil)
	must(t, err)
	time.Sleep(120 * time.Millisecond)

	jobs, err := s.Claim(ctx(), "next", 1, time.Minute, nil)
	must(t, err)
	if len(jobs) != 0 {
		t.Fatal("a message past its final attempt was reclaimed")
	}
	got, _ := s.Get(ctx(), m.ID)
	if got.State != store.StateFailed || got.LastErrorClass != "lease_expired" {
		t.Fatalf("poison message not failed: %+v", got)
	}
}

func testExcludeProjects(t *testing.T, s store.Store) {
	enqueue(t, s, newMsg("down"))
	enqueue(t, s, newMsg("up"))
	jobs, err := s.Claim(ctx(), "n", 10, time.Minute, []string{"down"})
	must(t, err)
	if len(jobs) != 1 || jobs[0].ProjectID != "up" {
		t.Fatalf("exclusion not applied: %+v", jobs)
	}
}

func testAdminRetryCancel(t *testing.T, s store.Store) {
	m := newMsg("p")
	m.MaxAttempts = 1
	enqueue(t, s, m)
	j := claimOne(t, s, "n")

	// A message being sent can be neither retried nor cancelled.
	_, err := s.Cancel(ctx(), m.ID, -1)
	wantErr(t, err, store.ErrConflict)
	_, err = s.Retry(ctx(), m.ID, -1)
	wantErr(t, err, store.ErrConflict)

	_, err = s.MarkRetry(ctx(), m.ID, j.LeaseToken, time.Hour, attempt(1))
	must(t, err) // exhausted -> failed
	failed, err := s.Get(ctx(), m.ID)
	must(t, err)
	if failed.State != store.StateFailed {
		t.Fatalf("state = %s", failed.State)
	}

	_, err = s.Retry(ctx(), m.ID, failed.Version-1)
	wantErr(t, err, store.ErrVersionMismatch)
	_, err = s.Retry(ctx(), "missing", -1)
	wantErr(t, err, store.ErrNotFound)

	retried, err := s.Retry(ctx(), m.ID, failed.Version)
	must(t, err)
	if retried.State != store.StateQueued || retried.Attempt != 0 || retried.Version != failed.Version+1 || retried.LastError != "" {
		t.Fatalf("unexpected retried message: %+v", retried)
	}
	// The stale version cannot cancel it any more.
	_, err = s.Cancel(ctx(), m.ID, failed.Version)
	wantErr(t, err, store.ErrVersionMismatch)

	canceled, err := s.Cancel(ctx(), m.ID, retried.Version)
	must(t, err)
	if canceled.State != store.StateCanceled {
		t.Fatalf("state = %s", canceled.State)
	}
	jobs, err := s.Claim(ctx(), "n", 1, time.Minute, nil)
	must(t, err)
	if len(jobs) != 0 {
		t.Fatal("a cancelled message was claimed")
	}
	// A cancelled message can be revived.
	_, err = s.Retry(ctx(), m.ID, -1)
	must(t, err)
}

func testAdminConcurrentRetry(t *testing.T, s store.Store) {
	m := newMsg("p")
	m.MaxAttempts = 1
	enqueue(t, s, m)
	j := claimOne(t, s, "n")
	_, err := s.MarkRetry(ctx(), m.ID, j.LeaseToken, 0, attempt(1))
	must(t, err)
	failed, _ := s.Get(ctx(), m.ID)

	// Several operators click "retry" on the same version at once.
	var (
		wg       sync.WaitGroup
		ok, lost atomic.Int32
	)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Retry(ctx(), m.ID, failed.Version)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, store.ErrVersionMismatch), errors.Is(err, store.ErrConflict):
				lost.Add(1)
			default:
				t.Errorf("retry: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || lost.Load() != 9 {
		t.Fatalf("ok=%d lost=%d, want exactly one winner", ok.Load(), lost.Load())
	}
}

func testDropBody(t *testing.T, s store.Store) {
	m := newMsg("p")
	enqueue(t, s, m)
	j := claimOne(t, s, "n")
	must(t, s.MarkSent(ctx(), m.ID, j.LeaseToken, attempt(1), true))

	_, err := s.GetPayload(ctx(), m.ID)
	wantErr(t, err, store.ErrPayloadGone)
	got, _ := s.Get(ctx(), m.ID)
	if !got.BodyDropped {
		t.Fatal("body_dropped not set")
	}
}

func testList(t *testing.T, s store.Store) {
	var ids []string
	for i := range 5 {
		m := newMsg("a")
		m.Subject = fmt.Sprintf("Invoice %d", i)
		m.To = "customer@corp.test"
		ids = append(ids, m.ID)
		enqueue(t, s, m)
	}
	other := newMsg("b")
	other.Subject = "100%_sure"
	enqueue(t, s, other)

	all, err := s.List(ctx(), store.ListFilter{})
	must(t, err)
	if len(all) != 6 || all[0].ID <= all[1].ID {
		t.Fatalf("expected 6 messages newest-first, got %d", len(all))
	}
	onlyA, err := s.List(ctx(), store.ListFilter{ProjectID: "a", Limit: 2})
	must(t, err)
	if len(onlyA) != 2 {
		t.Fatalf("limit not applied: %d", len(onlyA))
	}
	next, err := s.List(ctx(), store.ListFilter{ProjectID: "a", Before: onlyA[1].ID, Limit: 10})
	must(t, err)
	if len(next) != 3 {
		t.Fatalf("cursor page = %d, want 3", len(next))
	}
	byText, err := s.List(ctx(), store.ListFilter{Query: "INVOICE 3"})
	must(t, err)
	if len(byText) != 1 {
		t.Fatalf("subject search = %d", len(byText))
	}
	byWildcard, err := s.List(ctx(), store.ListFilter{Query: "100%_"})
	must(t, err)
	if len(byWildcard) != 1 {
		t.Fatalf("LIKE wildcards must be escaped, got %d matches", len(byWildcard))
	}
	byID, err := s.List(ctx(), store.ListFilter{Query: ids[2]})
	must(t, err)
	if len(byID) != 1 {
		t.Fatalf("id search = %d", len(byID))
	}
	queued, err := s.List(ctx(), store.ListFilter{States: []store.State{store.StateSent}})
	must(t, err)
	if len(queued) != 0 {
		t.Fatalf("state filter = %d", len(queued))
	}

	stats, err := s.Stats(ctx())
	must(t, err)
	var total int64
	for _, c := range stats {
		total += c.Count
	}
	if total != 6 {
		t.Fatalf("stats total = %d", total)
	}
}

func testCounters(t *testing.T, s store.Store) {
	const n = 80
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int64]bool{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, remaining, err := s.Incr(ctx(), "key", time.Hour)
			if err != nil {
				t.Errorf("incr: %v", err)
				return
			}
			if remaining <= 0 || remaining > time.Hour {
				t.Errorf("remaining = %s", remaining)
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[c] {
				t.Errorf("count %d handed out twice: increments are not atomic", c)
			}
			seen[c] = true
		}()
	}
	wg.Wait()
	for i := int64(1); i <= n; i++ {
		if !seen[i] {
			t.Fatalf("count %d never observed", i)
		}
	}
	c, _, err := s.Incr(ctx(), "another", time.Hour)
	must(t, err)
	if c != 1 {
		t.Fatalf("separate keys must not share counters, got %d", c)
	}
}

func testSlots(t *testing.T, s store.Store) {
	a0, ok, err := s.AcquireSlot(ctx(), "p", 2, "node-a", time.Minute)
	must(t, err)
	a1, ok2, err := s.AcquireSlot(ctx(), "p", 2, "node-b", time.Minute)
	must(t, err)
	if !ok || !ok2 || a0 == a1 {
		t.Fatalf("slots: %d/%v %d/%v", a0, ok, a1, ok2)
	}
	_, ok, err = s.AcquireSlot(ctx(), "p", 2, "node-c", time.Minute)
	must(t, err)
	if ok {
		t.Fatal("acquired more slots than the global cap")
	}

	renewed, err := s.RenewSlot(ctx(), "p", a0, "node-a", time.Minute)
	must(t, err)
	if !renewed {
		t.Fatal("owner could not renew its slot")
	}
	renewed, err = s.RenewSlot(ctx(), "p", a0, "node-c", time.Minute)
	must(t, err)
	if renewed {
		t.Fatal("non-owner renewed a slot")
	}

	must(t, s.ReleaseSlot(ctx(), "p", a0, "node-a"))
	got, ok, err := s.AcquireSlot(ctx(), "p", 2, "node-c", time.Minute)
	must(t, err)
	if !ok || got != a0 {
		t.Fatalf("released slot not reusable: %d %v", got, ok)
	}

	// An expired slot (crashed node) is taken over.
	_, _, err = s.AcquireSlot(ctx(), "q", 1, "dead", 20*time.Millisecond)
	must(t, err)
	time.Sleep(120 * time.Millisecond)
	_, ok, err = s.AcquireSlot(ctx(), "q", 1, "alive", time.Minute)
	must(t, err)
	if !ok {
		t.Fatal("expired slot was not reclaimed")
	}
}

func testPurge(t *testing.T, s store.Store) {
	send := func(project string) string {
		m := newMsg(project)
		enqueue(t, s, m)
		j := claimOne(t, s, "n")
		must(t, s.MarkSent(ctx(), m.ID, j.LeaseToken, attempt(1), false))
		return m.ID
	}
	oldSent := send("short")
	keptSent := send("long")
	orphan := send("removed")

	_, err := s.Enqueue(ctx(), newMsg("short"), &store.Idempotency{Key: "x", Fingerprint: "f", TTL: time.Millisecond})
	must(t, err)
	_, _, err = s.Incr(ctx(), "stale", time.Millisecond)
	must(t, err)
	time.Sleep(150 * time.Millisecond)

	rules := []store.RetentionRule{
		{ProjectID: "short", Sent: 50 * time.Millisecond, Failed: time.Hour},
		{ProjectID: "long", Sent: time.Hour, Failed: time.Hour},
	}
	def := store.RetentionRule{Sent: 50 * time.Millisecond, Failed: time.Hour}
	res, err := s.Purge(ctx(), rules, def, 10)
	must(t, err)
	if res.Skipped || res.Messages != 2 || res.Idempotency != 1 || res.Counters < 1 {
		t.Fatalf("unexpected purge result %+v", res)
	}
	_, err = s.Get(ctx(), oldSent)
	wantErr(t, err, store.ErrNotFound)
	_, err = s.Get(ctx(), orphan)
	wantErr(t, err, store.ErrNotFound) // projects missing from config use the default rule
	if _, err := s.Get(ctx(), keptSent); err != nil {
		t.Fatalf("a message inside its retention was purged: %v", err)
	}
	// Attempts cascade with their message.
	atts, err := s.Attempts(ctx(), oldSent)
	must(t, err)
	if len(atts) != 0 {
		t.Fatalf("attempts of a purged message survived: %d", len(atts))
	}
}

func testReleaseOwner(t *testing.T, s store.Store) {
	for range 3 {
		enqueue(t, s, newMsg("p"))
	}
	jobs, err := s.Claim(ctx(), "draining", 3, time.Hour, nil)
	must(t, err)
	if len(jobs) != 3 {
		t.Fatalf("claimed %d", len(jobs))
	}
	n, err := s.ReleaseOwner(ctx(), "draining")
	must(t, err)
	if n != 3 {
		t.Fatalf("released %d, want 3", n)
	}
	// Another replica can take them straight away, without waiting for lease expiry.
	again, err := s.Claim(ctx(), "other", 10, time.Minute, nil)
	must(t, err)
	if len(again) != 3 || again[0].Attempt != 1 {
		t.Fatalf("released jobs not immediately claimable: %d (attempt %d)", len(again), again[0].Attempt)
	}
}
