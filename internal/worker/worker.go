// Package worker claims queued messages and delivers them. It is the only
// component that talks to transports, and every state change it makes is
// fenced by the job's lease token, so several replicas can run it against
// one store without delivering a message twice (barring the documented
// unknown-outcome case).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/logging"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/metrics"
	"github.com/skylarng89/smtp-handler/internal/store"
	"github.com/skylarng89/smtp-handler/internal/transport"
)

const (
	unknownProjectDelay = 30 * time.Second
	busyDelayMin        = time.Second
	finalizeTimeout     = 10 * time.Second
	claimErrorBackoff   = 2 * time.Second
)

// Project is the per-project delivery setup.
type Project struct {
	Transport transport.Transport
	// Semantics is "at_least_once" or "at_most_once"; it decides what happens
	// when the outcome of a delivery is unknown.
	Semantics string
	DropBody  bool
}

// Dispatcher runs the delivery loop for one node.
type Dispatcher struct {
	cfg      config.Delivery
	node     string
	store    store.Store
	projects map[string]*Project
	log      *slog.Logger
	met      *metrics.Metrics

	breaker *breaker
	wake    chan struct{}
	wg      sync.WaitGroup
	busy    atomic.Int32
}

func New(cfg config.Delivery, node string, st store.Store, projects map[string]*Project, log *slog.Logger, met *metrics.Metrics) *Dispatcher {
	return &Dispatcher{
		cfg: cfg, node: node, store: st, projects: projects, log: log, met: met,
		breaker: newBreaker(cfg.BreakerCooldown.Std(), met),
		wake:    make(chan struct{}, 1),
	}
}

// Wake nudges the loop to poll immediately (called after a local enqueue).
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Busy reports deliveries currently in flight on this node.
func (d *Dispatcher) Busy() int { return int(d.busy.Load()) }

// Run claims and delivers until ctx is cancelled, then drains: in-flight
// sends get DrainTimeout to finish, after which they are aborted, and any
// lease still held is released so other replicas can take over immediately.
func (d *Dispatcher) Run(ctx context.Context) {
	sendCtx, abortSends := context.WithCancel(context.Background())
	defer abortSends()

	sem := make(chan struct{}, d.cfg.Workers)
	poll := time.NewTimer(0)
	defer poll.Stop()

	for ctx.Err() == nil {
		more := false
		if free := cap(sem) - len(sem); free > 0 {
			limit := min(free, d.cfg.BatchSize)
			jobs, err := d.store.Claim(ctx, d.node, limit, d.cfg.LeaseDuration.Std(), d.breaker.openProjects())
			switch {
			case err != nil && ctx.Err() == nil:
				d.log.Error("claim failed", "error", err)
				d.sleep(ctx, poll, claimErrorBackoff)
				continue
			case err == nil:
				for _, job := range jobs {
					sem <- struct{}{}
					d.wg.Add(1)
					go func() {
						defer func() { <-sem; d.wg.Done() }()
						d.process(sendCtx, job)
					}()
				}
				more = len(jobs) == limit
			}
		}
		if !more {
			d.sleep(ctx, poll, d.cfg.PollInterval.Std())
		}
	}
	d.drain(abortSends)
}

// sleep waits for the poll interval, a wake-up, or cancellation.
func (d *Dispatcher) sleep(ctx context.Context, t *time.Timer, dur time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	// Jitter keeps replicas from polling in lockstep.
	t.Reset(dur + time.Duration(rand.Int64N(int64(dur/4)+1)))
	select {
	case <-t.C:
	case <-d.wake:
	case <-ctx.Done():
	}
}

func (d *Dispatcher) drain(abortSends context.CancelFunc) {
	d.log.Info("draining deliveries", "in_flight", d.Busy())
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(d.cfg.DrainTimeout.Std()):
		d.log.Warn("drain timeout reached, aborting in-flight sends", "in_flight", d.Busy())
		abortSends()
		<-done
	}

	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	if n, err := d.store.ReleaseOwner(ctx, d.node); err != nil {
		d.log.Error("release leases on shutdown", "error", err)
	} else if n > 0 {
		d.log.Info("released leases for other replicas", "count", n)
	}
}

func (d *Dispatcher) process(sendCtx context.Context, job store.Job) {
	d.busy.Add(1)
	d.met.WorkersBusy.Inc()
	defer func() { d.busy.Add(-1); d.met.WorkersBusy.Dec() }()

	att := store.Attempt{Attempt: job.Attempt, Node: d.node, StartedAt: time.Now()}
	defer func() {
		// A panic must cost one attempt, never the worker pool.
		if r := recover(); r != nil {
			d.log.Error("panic while delivering", "message_id", job.ID, "panic", r, "stack", string(debug.Stack()))
			att.FinishedAt = time.Now()
			att.Outcome, att.Error = string(transport.ClassTransient), fmt.Sprintf("internal panic: %v", r)
			d.settle(job, att, func(ctx context.Context) error {
				_, err := d.store.MarkRetry(ctx, job.ID, job.LeaseToken, d.backoff(job.Attempt), att)
				return err
			})
		}
	}()

	proj, ok := d.projects[job.ProjectID]
	if !ok {
		d.deferJob(job, att, "unknown_project", "project is not configured on this node", unknownProjectDelay)
		return
	}

	var msg message.Message
	if len(job.Payload) == 0 || json.Unmarshal(job.Payload, &msg) != nil {
		att.FinishedAt = time.Now()
		att.Outcome, att.Error = string(transport.ClassPermanent), "stored message body is missing or unreadable"
		d.settle(job, att, func(ctx context.Context) error { return d.store.MarkFailed(ctx, job.ID, job.LeaseToken, att) })
		return
	}

	res := d.send(sendCtx, proj, job, &msg)
	att.FinishedAt = time.Now()
	att.Outcome, att.SMTPCode, att.EnhancedCode, att.Error = string(res.Class), res.Code, res.Enhanced, res.Detail
	d.met.Deliveries.WithLabelValues(job.ProjectID, string(res.Class)).Inc()
	d.met.DeliverySecs.WithLabelValues(job.ProjectID).Observe(att.FinishedAt.Sub(att.StartedAt).Seconds())

	log := d.log.With("message_id", job.ID, "project", job.ProjectID, "attempt", job.Attempt,
		"class", res.Class, "smtp_code", res.Code)

	switch res.Class {
	case transport.ClassSent:
		d.settle(job, att, func(ctx context.Context) error {
			return d.store.MarkSent(ctx, job.ID, job.LeaseToken, att, proj.DropBody)
		})
		log.Info("delivered")

	case transport.ClassPermanent:
		d.settle(job, att, func(ctx context.Context) error { return d.store.MarkFailed(ctx, job.ID, job.LeaseToken, att) })
		log.Warn("delivery failed permanently", "detail", logging.RedactText(res.Detail))

	case transport.ClassConfig:
		d.breaker.open(job.ProjectID)
		log.Error("smtp configuration problem; pausing project", "detail", logging.RedactText(res.Detail),
			"cooldown", d.cfg.BreakerCooldown.Std())
		d.deferJob(job, att, string(res.Class), res.Detail, d.cfg.BreakerCooldown.Std())

	case transport.ClassBusy:
		delay := busyDelayMin + time.Duration(rand.Int64N(int64(2*time.Second)))
		d.settle(job, att, func(ctx context.Context) error {
			return d.store.Release(ctx, job.ID, job.LeaseToken, delay, nil)
		})
		log.Debug("no connection slot; requeued without using an attempt")

	case transport.ClassUnknown:
		if proj.Semantics == "at_most_once" {
			att.Error = "delivery outcome unknown (connection lost before the server replied); not retried (at_most_once): " + att.Error
			d.settle(job, att, func(ctx context.Context) error { return d.store.MarkFailed(ctx, job.ID, job.LeaseToken, att) })
			log.Warn("outcome unknown; dead-lettered per at_most_once", "detail", logging.RedactText(res.Detail))
			return
		}
		fallthrough

	default: // transient
		delay := d.backoff(job.Attempt)
		d.settle(job, att, func(ctx context.Context) error {
			state, err := d.store.MarkRetry(ctx, job.ID, job.LeaseToken, delay, att)
			if err == nil {
				log.Warn("delivery failed", "next_state", state, "retry_in", delay, "detail", logging.RedactText(res.Detail))
			}
			return err
		})
	}
}

// send runs the transport with the lease kept alive. If the lease is lost
// (another node took over) the send is aborted and its result discarded by
// the fenced store call that follows.
func (d *Dispatcher) send(parent context.Context, proj *Project, job store.Job, msg *message.Message) transport.Result {
	ctx, cancel := context.WithTimeout(parent, d.cfg.SendTimeout.Std())
	defer cancel()

	renewDone := make(chan struct{})
	// Renewal deliberately uses fresh contexts (below): cancelling the send
	// must not be able to cancel the very write that records the lease.
	go func() { //nolint:gosec // G118: intentional, see comment above
		defer close(renewDone)
		ticker := time.NewTicker(d.cfg.LeaseDuration.Std() / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rctx, rcancel := context.WithTimeout(context.Background(), finalizeTimeout)
				err := d.store.RenewLease(rctx, job.ID, job.LeaseToken, d.cfg.LeaseDuration.Std())
				rcancel()
				switch {
				case errors.Is(err, store.ErrLeaseLost):
					d.met.LeaseLost.Inc()
					d.log.Warn("lease lost during send; aborting", "message_id", job.ID)
					cancel()
					return
				case err != nil:
					d.log.Warn("lease renewal failed", "message_id", job.ID, "error", err)
				}
			}
		}
	}()

	res := proj.Transport.Send(ctx, msg)
	cancel()
	<-renewDone
	return res
}

// deferJob returns a job to the queue without consuming an attempt, or fails
// it if it has outlived its deadline (so a permanent misconfiguration cannot
// keep a message alive forever).
func (d *Dispatcher) deferJob(job store.Job, att store.Attempt, class, detail string, delay time.Duration) {
	att.FinishedAt = time.Now()
	att.Outcome, att.Error = class, detail
	if !job.DeadlineAt.IsZero() && time.Now().After(job.DeadlineAt) {
		att.Error = "message expired before it could be delivered: " + detail
		d.settle(job, att, func(ctx context.Context) error { return d.store.MarkFailed(ctx, job.ID, job.LeaseToken, att) })
		return
	}
	d.settle(job, att, func(ctx context.Context) error {
		return d.store.Release(ctx, job.ID, job.LeaseToken, delay, &att)
	})
}

// settle runs a fenced store update on a fresh context (the send context may
// already be cancelled) and handles a lost lease uniformly.
func (d *Dispatcher) settle(job store.Job, _ store.Attempt, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	err := fn(ctx)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrLeaseLost):
		d.met.LeaseLost.Inc()
		d.log.Warn("result discarded: lease no longer held", "message_id", job.ID)
	default:
		// The lease will expire and another claim will retry the job.
		d.log.Error("could not record delivery result", "message_id", job.ID, "error", err)
	}
}

func (d *Dispatcher) backoff(attempt int) time.Duration {
	return Backoff(attempt, d.cfg.BackoffBase.Std(), d.cfg.BackoffMax.Std())
}

// Backoff returns an exponential delay with equal jitter: half the capped
// exponential value is fixed and half is random, which spreads replicas'
// retries while keeping a floor so a retry is never immediate.
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	d = min(d, maxDelay)
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// breaker pauses claiming for a project after a configuration-class failure
// (bad credentials, TLS problems) so workers stop hammering the provider.
type breaker struct {
	mu       sync.Mutex
	until    map[string]time.Time
	cooldown time.Duration
	met      *metrics.Metrics
}

func newBreaker(cooldown time.Duration, met *metrics.Metrics) *breaker {
	return &breaker{until: map[string]time.Time{}, cooldown: cooldown, met: met}
}

func (b *breaker) open(project string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until[project] = time.Now().Add(b.cooldown)
	b.met.BreakerOpen.WithLabelValues(project).Set(1)
}

func (b *breaker) openProjects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var out []string
	for p, until := range b.until {
		if now.Before(until) {
			out = append(out, p)
			continue
		}
		delete(b.until, p)
		b.met.BreakerOpen.WithLabelValues(p).Set(0)
	}
	return out
}
