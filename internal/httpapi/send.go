package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/compose"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/payload"
	"github.com/skylarng89/smtp-handler/internal/ratelimit"
	"github.com/skylarng89/smtp-handler/internal/store"
)

// msgQueueUnavailable is the client-facing detail for any store failure;
// the underlying cause is only logged.
const msgQueueUnavailable = "the queue is temporarily unavailable"

type sendResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// handleSend implements POST /v1/messages. Guards run cheapest-first and
// every rejection happens before anything is written to the store.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, err := s.d.Auth.Authenticate(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, _ := s.d.Cfg.Project(principal.ProjectID)
	reject := func(err error) {
		ae := apperr.As(err)
		s.d.Metrics.Rejected.WithLabelValues(project.ID, ae.Code).Inc()
		s.fail(w, r, err)
	}

	if err := s.checkOrigin(r, principal); err != nil {
		reject(err)
		return
	}

	rates := rateSet(project, principal)
	ip := s.d.Trust.ClientIP(r)
	for _, c := range []struct {
		key  string
		rule *config.Rule
	}{
		{"key:" + principal.KeyID, rates.PerKey},
		{"ip:" + project.ID + ":" + string(principal.Type) + ":" + ip, rates.PerIP},
	} {
		if rule, ok := ratelimit.FromConfig(c.rule); ok {
			if err := s.allow(ctx, c.key, rule); err != nil {
				reject(err)
				return
			}
		}
	}

	if !isJSON(r.Header.Get("Content-Type")) {
		reject(apperr.New(apperr.KindUnsupportedMedia, "unsupported-media-type", "Content-Type must be application/json"))
		return
	}
	limit := s.d.Cfg.Server.MaxBodySecret
	if !principal.IsSecret() {
		limit = s.d.Cfg.Server.MaxBodyPublishable
	}
	req, err := payload.Decode(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		reject(err)
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" && !isIdempotencyKey(idemKey) {
		reject(apperr.Validation("Idempotency-Key must be 1-128 characters of [A-Za-z0-9_.:-]"))
		return
	}
	fingerprint := req.Fingerprint(project.ID)
	idemTTL := 24 * time.Hour
	if idemKey == "" && project.DedupeWindow > 0 {
		idemKey, idemTTL = "auto:"+fingerprint, project.DedupeWindow.Std()
	}

	// A replay is answered before captcha: tokens are single-use, so a
	// legitimate retry would otherwise fail verification.
	if idemKey != "" {
		switch id, err := s.d.Store.FindIdempotent(ctx, project.ID, idemKey, fingerprint); {
		case err == nil:
			s.replay(w, project.ID, id)
			return
		case errors.Is(err, store.ErrIdempotencyMismatch):
			reject(idempotencyMismatch())
			return
		case errors.Is(err, store.ErrNotFound):
		default:
			reject(apperr.Unavailable(msgQueueUnavailable, err))
			return
		}
	}

	if !principal.IsSecret() {
		if v := s.d.Captcha[project.ID]; v != nil {
			token := req.CaptchaToken
			if token == "" {
				token = r.Header.Get("X-Captcha-Token")
			}
			if err := v.Verify(ctx, token, ip); err != nil {
				reject(err)
				return
			}
		}
	}

	prepared, err := s.d.Composer.Prepare(project.ID, principal.IsSecret(), req, s.d.Now())
	if err != nil {
		reject(err)
		return
	}
	if prepared.Honeypot {
		// Look successful to the bot; send nothing.
		s.d.Metrics.Accepted.WithLabelValues(project.ID, "honeypot").Inc()
		writeJSON(w, http.StatusAccepted, sendResponse{ID: ulid.Make().String(), Status: "queued"})
		return
	}

	if err := s.recipientLimits(ctx, project, principal, rates, prepared); err != nil {
		reject(err)
		return
	}

	res, err := s.enqueue(ctx, project, principal, prepared, idemKey, fingerprint, idemTTL)
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyMismatch) {
			err = idempotencyMismatch()
		} else if _, ok := err.(*apperr.Error); !ok { //nolint:errorlint // only a direct apperr is passed through
			err = apperr.Unavailable(msgQueueUnavailable, err)
		}
		reject(err)
		return
	}
	if res.Replayed {
		s.replay(w, project.ID, res.ID)
		return
	}

	s.d.Metrics.Accepted.WithLabelValues(project.ID, "queued").Inc()
	if s.d.Wake != nil {
		s.d.Wake()
	}
	writeJSON(w, http.StatusAccepted, sendResponse{ID: res.ID, Status: string(store.StateQueued)})
}

func idempotencyMismatch() *apperr.Error {
	e := apperr.New(apperr.KindUnprocessable, "idempotency-key-reused",
		"this Idempotency-Key was already used with a different request body")
	return e
}

func (s *Server) replay(w http.ResponseWriter, projectID, id string) {
	s.d.Metrics.Accepted.WithLabelValues(projectID, "replayed").Inc()
	w.Header().Set("Idempotent-Replayed", "true")
	writeJSON(w, http.StatusAccepted, sendResponse{ID: id, Status: string(store.StateQueued)})
}

// checkOrigin binds publishable keys to their allowed origins and keeps
// secret keys out of browsers.
func (s *Server) checkOrigin(r *http.Request, p *auth.Principal) error {
	origin := r.Header.Get("Origin")
	if p.IsSecret() {
		if origin != "" {
			return apperr.Forbidden("secret-key-in-browser",
				"secret keys must only be used from servers; use a publishable key in browsers")
		}
		return nil
	}
	if origin == "" || !p.OriginAllowed(origin) {
		return apperr.Forbidden("origin-not-allowed", "this key may not be used from this origin")
	}
	return nil
}

func rateSet(p *config.Project, principal *auth.Principal) config.RateSet {
	if principal.IsSecret() {
		return p.RateLimits.Secret
	}
	return p.RateLimits.Publishable
}

// recipientLimits applies the limits that need the resolved message: per
// recipient (caps inbox flooding) and the daily per-key quota.
func (s *Server) recipientLimits(ctx context.Context, p *config.Project, principal *auth.Principal, rates config.RateSet, prepared *compose.Result) error {
	if rule, ok := ratelimit.FromConfig(rates.PerRecipient); ok {
		seen := map[string]bool{}
		for _, a := range prepared.Msg.Recipients() {
			if seen[a.Email] {
				continue
			}
			seen[a.Email] = true
			if err := s.allow(ctx, "rcpt:"+p.ID+":"+a.Email, rule); err != nil {
				return err
			}
		}
	}
	if rule, ok := ratelimit.FromConfig(rates.Daily); ok {
		if err := s.allow(ctx, "daily:"+principal.KeyID, rule); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) enqueue(ctx context.Context, p *config.Project, principal *auth.Principal, prepared *compose.Result,
	idemKey, fingerprint string, idemTTL time.Duration) (store.EnqueueResult, error) {
	msg := prepared.Msg
	body, err := json.Marshal(msg)
	if err != nil {
		return store.EnqueueResult{}, apperr.Internal(err)
	}

	maxAttempts := s.d.Cfg.Delivery.MaxAttempts
	if p.Delivery.MaxAttempts > 0 {
		maxAttempts = p.Delivery.MaxAttempts
	}
	to := make([]string, len(msg.To))
	for i, a := range msg.To {
		to[i] = a.Email
	}
	nm := store.NewMessage{
		ID: msg.ID, ProjectID: p.ID, Template: prepared.Template, KeyID: principal.KeyID,
		MessageIDHeader: msg.MessageID, Subject: msg.Subject, From: msg.From.Email,
		To: strings.Join(to, ", "), Payload: body,
		MaxAttempts: maxAttempts, MaxAge: s.d.Cfg.Delivery.MaxAge.Std(),
	}
	var idem *store.Idempotency
	if idemKey != "" {
		idem = &store.Idempotency{Key: idemKey, Fingerprint: fingerprint, TTL: idemTTL}
	}
	return s.d.Store.Enqueue(ctx, nm, idem)
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

type statusResponse struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	CreatedAt      time.Time  `json:"created_at"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	LastErrorClass string     `json:"last_error_class,omitempty"`
	LastErrorCode  int        `json:"last_error_code,omitempty"`
}

// handleStatus implements GET /v1/messages/{id}; secret keys only, scoped to
// the key's own project.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	principal, err := s.d.Auth.Authenticate(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !principal.IsSecret() {
		s.fail(w, r, apperr.Forbidden("secret-key-required", "message status requires a secret key"))
		return
	}
	m, err := s.d.Store.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && m.ProjectID != principal.ProjectID:
		s.fail(w, r, apperr.NotFound("message not found"))
		return
	case err != nil:
		s.fail(w, r, apperr.Unavailable(msgQueueUnavailable, err))
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{
		ID: m.ID, Status: string(m.State), Attempts: m.Attempt, CreatedAt: m.CreatedAt, SentAt: m.SentAt,
		LastErrorClass: m.LastErrorClass, LastErrorCode: m.LastErrorCode,
	})
}
