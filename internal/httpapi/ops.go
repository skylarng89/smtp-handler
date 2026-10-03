package httpapi

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/skylarng89/smtp-handler/internal/metrics"
	"github.com/skylarng89/smtp-handler/internal/store"
)

// Readiness reports whether this replica should receive traffic. It turns
// unready as soon as shutdown begins so load balancers drain it first.
type Readiness struct {
	draining atomic.Bool
	store    store.Store
	checks   []func(context.Context) error
}

func NewReadiness(st store.Store, extra ...func(context.Context) error) *Readiness {
	return &Readiness{store: st, checks: extra}
}

// StartDraining flips readiness to false permanently.
func (r *Readiness) StartDraining() { r.draining.Store(true) }

func (r *Readiness) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.store.Ping(ctx); err != nil {
		return err
	}
	if err := r.store.SchemaOK(ctx); err != nil {
		return err
	}
	for _, c := range r.checks {
		if err := c(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Register adds /healthz, /readyz and /metrics to mux.
func Register(mux *http.ServeMux, rd *Readiness, m *metrics.Metrics) {
	// Liveness never touches dependencies: a database outage must not make
	// an orchestrator kill otherwise healthy replicas.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if rd.draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining\n"))
			return
		}
		if err := rd.check(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n")) // details stay in logs, not on the wire
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
}
