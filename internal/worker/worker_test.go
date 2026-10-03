package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/metrics"
	"github.com/skylarng89/smtp-handler/internal/store"
	"github.com/skylarng89/smtp-handler/internal/store/sqlstore"
	"github.com/skylarng89/smtp-handler/internal/transport"
	"github.com/skylarng89/smtp-handler/internal/transport/smtptest"
	"github.com/skylarng89/smtp-handler/internal/worker"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlstore.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func deliveryCfg() config.Delivery {
	ms := func(n int) config.Duration { return config.Duration(time.Duration(n) * time.Millisecond) }
	return config.Delivery{
		Workers: 4, BatchSize: 4, PollInterval: ms(20), LeaseDuration: ms(2000), SendTimeout: ms(1500),
		MaxAttempts: 3, MaxAge: config.Duration(time.Hour), BackoffBase: ms(10), BackoffMax: ms(40),
		BreakerCooldown: ms(300), DrainTimeout: ms(2000),
	}
}

func newTransport(t *testing.T, srv *smtptest.Server, mutate ...func(*config.SMTP)) transport.Transport {
	t.Helper()
	cfg := config.SMTP{
		Host: srv.Host, Port: srv.Port, TLS: "none", AuthMethod: "auto",
		Timeout: config.Duration(3 * time.Second), MaxConnections: 4, IdleTimeout: config.Duration(30 * time.Second),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	tr, err := transport.NewSMTP("p", "node", cfg, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func enqueue(t *testing.T, s store.Store, project, to string, maxAttempts int) string {
	t.Helper()
	id := ulid.Make().String()
	payload, _ := json.Marshal(message.Message{
		ID: id, MessageID: id + "@example.com", Date: time.Now(),
		From: message.Address{Email: "noreply@example.com"}, To: []message.Address{{Email: to}},
		Subject: "Test " + id, Text: "hello",
	})
	_, err := s.Enqueue(context.Background(), store.NewMessage{
		ID: id, ProjectID: project, MessageIDHeader: id + "@example.com", Subject: "Test", From: "noreply@example.com",
		To: to, Payload: payload, MaxAttempts: maxAttempts, MaxAge: time.Hour,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func start(t *testing.T, s store.Store, node string, projects map[string]*worker.Project) (stop func()) {
	t.Helper()
	d := worker.New(deliveryCfg(), node, s, projects, quiet, metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("dispatcher did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(t *testing.T, s store.Store, id string) *store.Message {
	t.Helper()
	m, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func projects(tr transport.Transport, semantics string) map[string]*worker.Project {
	return map[string]*worker.Project{"p": {Transport: tr, Semantics: semantics}}
}

func TestDeliversAndRecordsAttempt(t *testing.T) {
	srv, s := smtptest.New(t), newStore(t)
	start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))

	id := enqueue(t, s, "p", "user@example.org", 3)
	eventually(t, "message sent", func() bool { return state(t, s, id).State == store.StateSent })

	if got := len(srv.Received()); got != 1 {
		t.Fatalf("server received %d messages", got)
	}
	atts, _ := s.Attempts(context.Background(), id)
	if len(atts) != 1 || atts[0].Outcome != "sent" || atts[0].Node != "n1" {
		t.Fatalf("attempts = %+v", atts)
	}
}

func TestTransientFailureRetriesThenDeadLetters(t *testing.T) {
	srv, s := smtptest.New(t), newStore(t)
	start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))

	id := enqueue(t, s, "p", "tempfail@example.org", 3)
	eventually(t, "message failed", func() bool { return state(t, s, id).State == store.StateFailed })

	got := state(t, s, id)
	if got.Attempt != 3 || got.LastErrorClass != "transient" || got.LastErrorCode != 451 {
		t.Fatalf("unexpected final state: %+v", got)
	}
	atts, _ := s.Attempts(context.Background(), id)
	if len(atts) != 3 {
		t.Fatalf("recorded %d attempts, want 3", len(atts))
	}
}

func TestPermanentFailureDoesNotRetry(t *testing.T) {
	srv, s := smtptest.New(t), newStore(t)
	start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))

	id := enqueue(t, s, "p", "permfail@example.org", 5)
	eventually(t, "message failed", func() bool { return state(t, s, id).State == store.StateFailed })
	if got := state(t, s, id); got.Attempt != 1 || got.LastErrorCode != 550 {
		t.Fatalf("a 5xx reply must dead-letter on the first attempt: %+v", got)
	}
}

func TestUnknownOutcomeFollowsDeliverySemantics(t *testing.T) {
	t.Run("at_least_once retries", func(t *testing.T) {
		srv, s := smtptest.New(t), newStore(t)
		start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))
		id := enqueue(t, s, "p", "dropdata@example.org", 3)
		eventually(t, "attempts exhausted", func() bool { return state(t, s, id).State == store.StateFailed })
		if got := state(t, s, id); got.Attempt != 3 {
			t.Fatalf("attempt = %d, want 3 retries of an unknown outcome", got.Attempt)
		}
	})
	t.Run("at_most_once dead-letters", func(t *testing.T) {
		srv, s := smtptest.New(t), newStore(t)
		start(t, s, "n1", projects(newTransport(t, srv), "at_most_once"))
		id := enqueue(t, s, "p", "dropdata@example.org", 3)
		eventually(t, "message failed", func() bool { return state(t, s, id).State == store.StateFailed })
		got := state(t, s, id)
		if got.Attempt != 1 || got.LastErrorClass != "unknown" {
			t.Fatalf("at_most_once must not resend: %+v", got)
		}
	})
}

func TestConfigErrorPausesProjectWithoutConsumingAttempts(t *testing.T) {
	srv := smtptest.New(t, smtptest.WithAuth("mailer", "right"))
	s := newStore(t)
	bad := newTransport(t, srv, func(c *config.SMTP) { c.Username, c.Password, c.AuthMethod = "mailer", "wrong", "plain" })
	start(t, s, "n1", projects(bad, "at_least_once"))

	id := enqueue(t, s, "p", "user@example.org", 2)
	eventually(t, "config failure recorded", func() bool { return state(t, s, id).LastErrorClass == "config" })

	time.Sleep(100 * time.Millisecond)
	got := state(t, s, id)
	if got.State != store.StateRetryScheduled || got.Attempt != 0 {
		t.Fatalf("a credentials problem must not burn attempts or fail the message: %+v", got)
	}
	// The breaker stops the worker hammering the provider while it is open.
	if n := srv.AuthFailures(); n > 3 {
		t.Fatalf("%d auth attempts in 100ms: circuit breaker is not limiting retries", n)
	}
}

func TestUnknownProjectIsReleasedNotLost(t *testing.T) {
	srv, s := smtptest.New(t), newStore(t)
	start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))

	id := enqueue(t, s, "removed-project", "user@example.org", 3)
	eventually(t, "job released", func() bool {
		m := state(t, s, id)
		return m.State == store.StateRetryScheduled && m.LastErrorClass == "unknown_project"
	})
	if got := state(t, s, id); got.Attempt != 0 {
		t.Fatalf("attempt = %d, want 0", got.Attempt)
	}
}

func TestTwoNodesDeliverEachMessageExactlyOnce(t *testing.T) {
	srv, s := smtptest.New(t), newStore(t)
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		start(t, s, node, projects(newTransport(t, srv), "at_least_once"))
	}

	const total = 60
	ids := make([]string, total)
	for i := range total {
		ids[i] = enqueue(t, s, "p", "user@example.org", 3)
	}
	eventually(t, "all messages sent", func() bool {
		for _, id := range ids {
			if state(t, s, id).State != store.StateSent {
				return false
			}
		}
		return true
	})

	got := srv.Received()
	if len(got) != total {
		t.Fatalf("server received %d messages for %d enqueued: duplicates or losses", len(got), total)
	}
	seen := map[string]bool{}
	for _, r := range got {
		subject := subjectOf(r.Data)
		if seen[subject] {
			t.Fatalf("message delivered twice: %s", subject)
		}
		seen[subject] = true
	}
}

func subjectOf(raw string) string {
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			return line
		}
	}
	return raw
}

func TestGracefulDrainFinishesInFlightAndReleasesLeases(t *testing.T) {
	srv := smtptest.New(t)
	srv.SlowDelay = 400 * time.Millisecond
	s := newStore(t)
	stop := start(t, s, "n1", projects(newTransport(t, srv), "at_least_once"))

	id := enqueue(t, s, "p", "slow@example.org", 3)
	eventually(t, "message in flight", func() bool { return state(t, s, id).State == store.StateSending })

	stop() // SIGTERM equivalent: must wait for the in-flight send

	if got := state(t, s, id); got.State != store.StateSent {
		t.Fatalf("in-flight message was not finished during drain: %+v", got)
	}
	if got := len(srv.Received()); got != 1 {
		t.Fatalf("server received %d", got)
	}
}

func TestDrainTimeoutAbortsAndHandsOffToAnotherNode(t *testing.T) {
	srv := smtptest.New(t)
	srv.SlowDelay = 6 * time.Second
	s := newStore(t)

	cfg := deliveryCfg()
	cfg.DrainTimeout = config.Duration(300 * time.Millisecond)
	d := worker.New(cfg, "dying", s, projects(newTransport(t, srv), "at_least_once"), quiet, metrics.New())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	id := enqueue(t, s, "p", "slow@example.org", 5)
	eventually(t, "message in flight", func() bool { return state(t, s, id).State == store.StateSending })
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("dispatcher ignored the drain timeout")
	}

	got := state(t, s, id)
	if got.State == store.StateSending || got.LeaseOwner != "" {
		t.Fatalf("the dying node left its lease behind: %+v", got)
	}
	if got.State != store.StateRetryScheduled {
		t.Fatalf("state = %s, want retry_scheduled so another replica can take over", got.State)
	}
}

func TestBackoffIsBoundedAndGrows(t *testing.T) {
	base, maxD := 30*time.Second, time.Hour
	prevCeil := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		ceil := min(base<<(attempt-1), maxD)
		for range 50 {
			got := worker.Backoff(attempt, base, maxD)
			if got < ceil/2 || got > ceil {
				t.Fatalf("attempt %d: %s outside [%s, %s]", attempt, got, ceil/2, ceil)
			}
		}
		if ceil < prevCeil {
			t.Fatal("ceiling shrank")
		}
		prevCeil = ceil
	}
	if got := worker.Backoff(0, base, maxD); got < base/2 || got > base {
		t.Fatalf("attempt 0 treated as 1: %s", got)
	}
}
