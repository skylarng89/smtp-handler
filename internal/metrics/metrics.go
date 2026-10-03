// Package metrics defines the Prometheus instruments used across the service.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/skylarng89/smtp-handler/internal/store"
)

// Metrics bundles every instrument. Construct one per process (or per test)
// with New so registries never collide.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests  *prometheus.CounterVec   // route, method, status
	HTTPDuration  *prometheus.HistogramVec // route
	Accepted      *prometheus.CounterVec   // project, kind (queued|replayed|honeypot)
	Rejected      *prometheus.CounterVec   // project, reason
	Deliveries    *prometheus.CounterVec   // project, outcome
	DeliverySecs  *prometheus.HistogramVec // project
	LeaseLost     prometheus.Counter
	BreakerOpen   *prometheus.GaugeVec // project
	LimiterErrors prometheus.Counter
	Purged        prometheus.Counter
	WorkersBusy   prometheus.Gauge
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smtph_http_requests_total", Help: "HTTP requests by route, method and status.",
		}, []string{"route", "method", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "smtph_http_request_duration_seconds", Help: "HTTP request latency.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route"}),
		Accepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smtph_messages_accepted_total", Help: "Send requests accepted (202).",
		}, []string{"project", "kind"}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smtph_messages_rejected_total", Help: "Send requests rejected, by reason.",
		}, []string{"project", "reason"}),
		Deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smtph_deliveries_total", Help: "Delivery attempts by outcome class.",
		}, []string{"project", "outcome"}),
		DeliverySecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "smtph_delivery_duration_seconds", Help: "Time spent in one delivery attempt.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"project"}),
		LeaseLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smtph_lease_lost_total", Help: "Results discarded because the job lease was lost (fencing).",
		}),
		BreakerOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "smtph_circuit_breaker_open", Help: "1 while a project's SMTP circuit breaker is open on this node.",
		}, []string{"project"}),
		LimiterErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smtph_ratelimit_backend_errors_total", Help: "Rate-limit backend failures (fell back to memory).",
		}),
		Purged: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smtph_purged_messages_total", Help: "Messages deleted by retention.",
		}),
		WorkersBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "smtph_workers_busy", Help: "Deliveries currently in flight on this node.",
		}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.Accepted, m.Rejected, m.Deliveries,
		m.DeliverySecs, m.LeaseLost, m.BreakerOpen, m.LimiterErrors, m.Purged, m.WorkersBusy)
	return m
}

// RegisterQueueGauge exposes queue depth by project and state. Counts come
// from the store on scrape, cached briefly so frequent scrapes stay cheap.
func (m *Metrics) RegisterQueueGauge(s store.Store) {
	m.Registry.MustRegister(&queueCollector{
		store: s,
		desc: prometheus.NewDesc("smtph_queue_messages", "Messages in the queue by project and state.",
			[]string{"project", "state"}, nil),
	})
}

type queueCollector struct {
	store store.Store
	desc  *prometheus.Desc

	at     time.Time
	counts []store.Count
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	if time.Since(c.at) > 5*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if counts, err := c.store.Stats(ctx); err == nil {
			c.counts, c.at = counts, time.Now()
		}
	}
	for _, cnt := range c.counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(cnt.Count), cnt.ProjectID, string(cnt.State))
	}
}
