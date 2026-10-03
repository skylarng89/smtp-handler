// Package app assembles the service: store, limiter, transports, dispatcher
// and HTTP listeners, and owns the startup and graceful-shutdown order.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/skylarng89/smtp-handler/internal/adminapi"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/captcha"
	"github.com/skylarng89/smtp-handler/internal/compose"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/httpapi"
	"github.com/skylarng89/smtp-handler/internal/metrics"
	"github.com/skylarng89/smtp-handler/internal/netutil"
	"github.com/skylarng89/smtp-handler/internal/ratelimit"
	"github.com/skylarng89/smtp-handler/internal/render"
	"github.com/skylarng89/smtp-handler/internal/store"
	"github.com/skylarng89/smtp-handler/internal/store/sqlstore"
	"github.com/skylarng89/smtp-handler/internal/transport"
	"github.com/skylarng89/smtp-handler/internal/ui"
	"github.com/skylarng89/smtp-handler/internal/worker"
)

// App is a fully wired, not-yet-running service.
type App struct {
	cfg *config.Config
	log *slog.Logger

	store      store.Store
	dispatcher *worker.Dispatcher
	transports []transport.Transport
	readiness  *httpapi.Readiness
	metrics    *metrics.Metrics
	closers    []func() error

	publicLn, opsLn net.Listener
	public, ops     *http.Server
	node            string
	limiterCancel   context.CancelFunc
}

// New builds everything and binds both listeners, so configuration and port
// problems surface before any traffic is served.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	a := &App{cfg: cfg, log: log, metrics: metrics.New(), node: nodeID()}
	ok := false
	defer func() {
		if !ok {
			a.cleanup()
		}
	}()

	for _, w := range cfg.Warnings {
		log.Warn("configuration", "warning", w)
	}

	st, err := sqlstore.Open(ctx, cfg.Store)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	a.store = st
	if err := st.SchemaOK(ctx); err != nil {
		return nil, err
	}
	a.metrics.RegisterQueueGauge(st)

	limiter, extraChecks, err := a.buildLimiter(ctx)
	if err != nil {
		return nil, err
	}

	reg, err := render.NewRegistry(cfg)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	composer, err := compose.New(cfg, reg)
	if err != nil {
		return nil, err
	}

	captchas := map[string]captcha.Verifier{}
	projects := map[string]*worker.Project{}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		if v := captcha.New(p.Captcha); v != nil {
			captchas[p.ID] = v
		}
		tr, err := transport.NewSMTP(p.ID, a.node, p.SMTP, st, log)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", p.ID, err)
		}
		a.transports = append(a.transports, tr)
		projects[p.ID] = &worker.Project{Transport: tr, Semantics: p.Delivery.Semantics, DropBody: p.DropBody(cfg.Retention)}
	}
	a.dispatcher = worker.New(cfg.Delivery, a.node, st, projects, log, a.metrics)

	prefixes, err := config.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	trust := netutil.NewProxyTrust(prefixes)

	a.readiness = httpapi.NewReadiness(st, extraChecks...)
	a.public = &http.Server{
		Handler: httpapi.NewPublic(httpapi.Deps{
			Cfg: cfg, Store: st, Auth: auth.New(cfg), Limiter: limiter, Captcha: captchas,
			Composer: composer, Metrics: a.metrics, Log: log, Trust: trust, Wake: a.dispatcher.Wake,
		}),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	opsMux := http.NewServeMux()
	httpapi.Register(opsMux, a.readiness, a.metrics)
	if cfg.Admin.Enabled {
		secret, err := sessionSecret(cfg.Admin)
		if err != nil {
			return nil, err
		}
		opsMux.Handle(adminapi.Prefix+"/", adminapi.New(adminapi.Deps{
			Cfg: cfg, Store: st, Limiter: limiter, Log: log, Trust: trust, SessionSecret: secret, Wake: a.dispatcher.Wake,
		}))
		if cfg.Admin.UI {
			opsMux.Handle(ui.Prefix, ui.Handler())
			opsMux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, ui.Prefix, http.StatusFound)
			})
			if !ui.Built() {
				log.Warn("admin UI is enabled but this binary was built without it; run `make ui build`")
			}
		}
	}
	a.ops = &http.Server{
		Handler:           opsMux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	lc := &net.ListenConfig{}
	if a.publicLn, err = lc.Listen(ctx, "tcp", cfg.Server.Addr); err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Server.Addr, err)
	}
	if a.opsLn, err = lc.Listen(ctx, "tcp", cfg.Server.AdminAddr); err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Server.AdminAddr, err)
	}
	ok = true
	return a, nil
}

// PublicAddr and OpsAddr report the bound addresses (useful with port 0).
func (a *App) PublicAddr() string { return a.publicLn.Addr().String() }
func (a *App) OpsAddr() string    { return a.opsLn.Addr().String() }

// Store exposes the store (used by tests and tooling).
func (a *App) Store() store.Store { return a.store }

func nodeID() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}

func sessionSecret(c config.Admin) ([]byte, error) {
	if c.SessionSecret != "" {
		return []byte(c.SessionSecret), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// buildLimiter picks the rate-limit backend: the shared database on
// PostgreSQL, else Redis when configured, else per-node memory. Shared
// backends degrade to memory if they fail.
func (a *App) buildLimiter(ctx context.Context) (ratelimit.Limiter, []func(context.Context) error, error) {
	limCtx, cancel := context.WithCancel(context.Background())
	a.limiterCancel = cancel
	memory := ratelimit.NewMemory(limCtx)

	backend := a.cfg.RateLimit.Backend
	if backend == "auto" {
		switch {
		case a.cfg.Store.Driver == "postgres":
			backend = "store"
		case a.cfg.Redis.URL != "":
			backend = "redis"
		default:
			backend = "memory"
		}
	}
	a.log.Info("rate limiter", "backend", backend)

	resilient := func(primary ratelimit.Limiter) ratelimit.Limiter {
		return &ratelimit.Resilient{Primary: primary, Fallback: memory, Log: a.log, OnError: a.metrics.LimiterErrors.Inc}
	}
	switch backend {
	case "store":
		return resilient(ratelimit.Store{C: a.store}), nil, nil
	case "redis":
		r, err := ratelimit.NewRedis(a.cfg.Redis.URL)
		if err != nil {
			return nil, nil, err
		}
		a.closers = append(a.closers, r.Close)
		pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
		defer pingCancel()
		if err := r.Ping(pingCtx); err != nil {
			// Not fatal: the resilient wrapper serves from memory until Redis returns.
			a.log.Warn("redis is not reachable at startup", "error", err)
		}
		return resilient(r), nil, nil
	default:
		return memory, nil, nil
	}
}

// Run serves until ctx is cancelled, then shuts down in order:
//  1. report not-ready (and optionally wait ShutdownDelay for load balancers)
//  2. stop accepting HTTP requests and let in-flight ones finish
//  3. drain the dispatcher: finish in-flight sends, release remaining leases
//  4. close transports, rate-limit backends and the store
func (a *App) Run(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		serveCh = make(chan error, 2)
	)
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	purgeCtx, stopPurge := context.WithCancel(context.Background())

	wg.Add(2)
	go func() { defer wg.Done(); a.dispatcher.Run(dispatchCtx) }()
	go func() { defer wg.Done(); a.purgeLoop(purgeCtx) }()

	serve := func(srv *http.Server, ln net.Listener, tls bool) {
		var err error
		if tls {
			err = srv.ServeTLS(ln, a.cfg.Server.TLSCertFile, a.cfg.Server.TLSKeyFile)
		} else {
			err = srv.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			serveCh <- err
		}
	}
	go serve(a.public, a.publicLn, a.cfg.Server.TLSCertFile != "")
	go serve(a.ops, a.opsLn, false)

	a.log.Info("smtp-handler started", "node", a.node, "public", a.PublicAddr(), "ops", a.OpsAddr(),
		"store", a.store.Driver(), "projects", len(a.cfg.Projects), "admin", a.cfg.Admin.Enabled, "ui", a.cfg.Admin.UI)

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested")
	case runErr = <-serveCh:
		a.log.Error("listener failed", "error", runErr)
	}

	a.readiness.StartDraining()
	if d := a.cfg.Server.ShutdownDelay.Std(); d > 0 {
		time.Sleep(d)
	}
	stopPurge()
	stopDispatch() // sends begin draining while HTTP finishes

	shutCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout.Std())
	defer cancel()
	for _, srv := range []*http.Server{a.public, a.ops} {
		if err := srv.Shutdown(shutCtx); err != nil {
			a.log.Warn("http shutdown", "error", err)
			_ = srv.Close()
		}
	}
	wg.Wait()
	a.cleanup()
	a.log.Info("shutdown complete")
	return runErr
}

func (a *App) purgeLoop(ctx context.Context) {
	cfg := a.cfg
	rules := make([]store.RetentionRule, 0, len(cfg.Projects))
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		rules = append(rules, store.RetentionRule{
			ProjectID: p.ID, Sent: p.SentRetention(cfg.Retention), Failed: p.FailedRetention(cfg.Retention),
		})
	}
	def := store.RetentionRule{Sent: cfg.Retention.Sent.Std(), Failed: cfg.Retention.Failed.Std()}

	run := func() {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		res, err := a.store.Purge(pctx, rules, def, 1000)
		switch {
		case err != nil && ctx.Err() == nil:
			a.log.Error("retention purge failed", "error", err)
		case res.Skipped:
			a.log.Debug("retention purge skipped: another node holds the lock")
		case res.Messages > 0:
			a.metrics.Purged.Add(float64(res.Messages))
			a.log.Info("retention purge", "messages", res.Messages, "idempotency", res.Idempotency, "counters", res.Counters)
		}
	}

	// Start after a short random delay so replicas do not all purge at once.
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
			t.Reset(cfg.Retention.PurgeInterval.Std())
		}
	}
}

func (a *App) cleanup() {
	if a.limiterCancel != nil {
		a.limiterCancel()
	}
	for _, t := range a.transports {
		_ = t.Close()
	}
	for _, c := range a.closers {
		_ = c()
	}
	if a.publicLn != nil {
		_ = a.publicLn.Close()
	}
	if a.opsLn != nil {
		_ = a.opsLn.Close()
	}
	if a.store != nil {
		_ = a.store.Close()
	}
	a.transports, a.closers = nil, nil
}
