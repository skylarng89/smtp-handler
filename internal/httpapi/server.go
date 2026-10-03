// Package httpapi implements the public send API and the operational
// (health/metrics) endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/captcha"
	"github.com/skylarng89/smtp-handler/internal/compose"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/metrics"
	"github.com/skylarng89/smtp-handler/internal/netutil"
	"github.com/skylarng89/smtp-handler/internal/ratelimit"
	"github.com/skylarng89/smtp-handler/internal/store"
)

// Deps are the collaborators of the public API.
type Deps struct {
	Cfg      *config.Config
	Store    store.Store
	Auth     *auth.Authenticator
	Limiter  ratelimit.Limiter
	Captcha  map[string]captcha.Verifier // by project ID; absent = disabled
	Composer *compose.Composer
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	Trust    netutil.ProxyTrust
	// Wake nudges the local dispatcher after an enqueue (optional).
	Wake func()
	// Now is injectable for tests.
	Now func() time.Time
}

// Server holds the public API state.
type Server struct {
	d Deps
}

type ctxKey int

const requestIDKey ctxKey = iota

// NewPublic returns the handler for the public listener.
func NewPublic(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Server{d: d}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handleSend)
	mux.HandleFunc("GET /v1/messages/{id}", s.handleStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, apperr.NotFound("no such route"))
	})

	return chain(mux, s.requestID, s.recoverer, s.observe, s.cors, s.preAuthLimit)
}

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ulid.Make().String()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
					panic(rec)
				}
				s.d.Log.Error("panic in handler", "request_id", requestID(r.Context()), "panic", rec,
					"stack", string(debug.Stack()))
				s.fail(w, r, apperr.Internal(nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status code for metrics and access logs.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		s.d.Metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
		s.d.Metrics.HTTPDuration.WithLabelValues(route).Observe(elapsed.Seconds())
		s.d.Log.Info("request", "request_id", requestID(r.Context()), "method", r.Method, "route", route,
			"status", sw.status, "duration_ms", elapsed.Milliseconds(), "ip", s.d.Trust.ClientIP(r))
	})
}

const corsAllowHeaders = "Authorization, Content-Type, Idempotency-Key, X-API-Key, X-Captcha-Token"

// cors answers preflights and decorates responses for browser origins that
// some publishable key allows. This is a convenience for browsers, not a
// security boundary: the per-key origin check in handleSend is.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		known := origin != "" && s.d.Auth.OriginKnown(origin)
		if known {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Retry-After, Idempotent-Replayed, X-Request-Id")
		}
		if r.Method == http.MethodOptions {
			if known {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// preAuthLimit throttles every client IP before keys are checked, bounding
// key-guessing and cheap floods.
func (s *Server) preAuthLimit(next http.Handler) http.Handler {
	rule, enabled := ratelimit.FromConfig(&s.d.Cfg.Server.PreAuthRate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled {
			if err := s.allow(r.Context(), "pre:"+s.d.Trust.ClientIP(r), rule); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allow counts one event and returns a 429 error when over the limit.
func (s *Server) allow(ctx context.Context, key string, rule ratelimit.Rule) *apperr.Error {
	d, err := s.d.Limiter.Allow(ctx, key, rule)
	if err != nil {
		return apperr.Unavailable("rate limiting is temporarily unavailable", err)
	}
	if !d.Allowed {
		return apperr.RateLimited(d.RetryAfter)
	}
	return nil
}

// fail writes err as problem+json, logging server-side causes.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	ae := apperr.As(err)
	rid := requestID(r.Context())
	if ae.Kind == apperr.KindInternal || ae.Kind == apperr.KindUnavailable {
		s.d.Log.Error("request failed", "request_id", rid, "code", ae.Code, "error", ae.Cause)
	}
	if ae.Kind == apperr.KindUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="smtp-handler"`)
	}
	apperr.Write(w, ae, rid)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func isIdempotencyKey(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_.:-", r):
		default:
			return false
		}
	}
	return true
}
