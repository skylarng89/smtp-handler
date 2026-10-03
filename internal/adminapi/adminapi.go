// Package adminapi implements the operator API behind the optional web UI:
// browsing the queue, inspecting messages and retrying or cancelling them.
//
// Mutations are guarded twice: browsers must pass Go's cross-origin
// protection (plus SameSite=Strict cookies), and every retry/cancel carries
// the version the operator saw (If-Match), so two operators — or an operator
// and a worker — can never silently overwrite each other.
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/netutil"
	"github.com/skylarng89/smtp-handler/internal/ratelimit"
	"github.com/skylarng89/smtp-handler/internal/store"
)

// msgQueueUnavailable is the client-facing detail for any store failure;
// the underlying cause is only logged.
const msgQueueUnavailable = "the queue is temporarily unavailable"

// Prefix is where the API is mounted.
const Prefix = "/admin/api/v1"

const (
	cookieName   = "smtph_admin"
	maxBodyBytes = 1 << 20
	maxBulkItems = 200
	loginPerMin  = 10
	dummyHash    = "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type Deps struct {
	Cfg           *config.Config
	Store         store.Store
	Limiter       ratelimit.Limiter
	Log           *slog.Logger
	Trust         netutil.ProxyTrust
	SessionSecret []byte
	Wake          func()
	Now           func() time.Time
}

type API struct{ d Deps }

// New returns the admin handler (already prefixed with Prefix).
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	a := &API{d: d}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix+"/login", a.login)
	mux.HandleFunc("POST "+Prefix+"/logout", a.logout)
	mux.HandleFunc("GET "+Prefix+"/session", a.protected(a.session))
	mux.HandleFunc("GET "+Prefix+"/projects", a.protected(a.projects))
	mux.HandleFunc("GET "+Prefix+"/stats", a.protected(a.stats))
	mux.HandleFunc("GET "+Prefix+"/messages", a.protected(a.list))
	mux.HandleFunc("GET "+Prefix+"/messages/{id}", a.protected(a.detail))
	mux.HandleFunc("POST "+Prefix+"/messages/{id}/retry", a.protected(a.act(actionRetry)))
	mux.HandleFunc("POST "+Prefix+"/messages/{id}/cancel", a.protected(a.act(actionCancel)))
	mux.HandleFunc("POST "+Prefix+"/messages/bulk", a.protected(a.bulk))

	// Rejects cross-origin state-changing requests from browsers (Origin /
	// Sec-Fetch-Site checks); non-browser clients are unaffected.
	cop := http.NewCrossOriginProtection()
	return noStore(cop.Handler(mux))
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	ae := apperr.As(err)
	if ae.Kind == apperr.KindInternal || ae.Kind == apperr.KindUnavailable {
		a.d.Log.Error("admin request failed", "code", ae.Code, "error", ae.Cause)
	}
	apperr.Write(w, ae, "")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.Validation("request body is not valid JSON for this endpoint")
	}
	return nil
}

// --- authentication ---------------------------------------------------------

type userKey struct{}

func (a *API) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			a.fail(w, apperr.Unauthorized("sign in required"))
			return
		}
		user, err := auth.VerifySession(a.d.SessionSecret, c.Value, a.d.Now())
		if err != nil {
			a.fail(w, apperr.Unauthorized("session is invalid or expired"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	}
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.fail(w, err)
		return
	}

	rule := ratelimit.Rule{Limit: loginPerMin, Window: time.Minute}
	if d, err := a.d.Limiter.Allow(r.Context(), "admin-login:"+a.d.Trust.ClientIP(r), rule); err == nil && !d.Allowed {
		a.fail(w, apperr.RateLimited(d.RetryAfter))
		return
	}

	// Always run the (expensive) hash so response time does not reveal
	// whether the username exists.
	hash, userOK := a.d.Cfg.Admin.PasswordHash, body.Username == a.d.Cfg.Admin.Username
	if !userOK {
		hash = dummyHash
	}
	passOK, err := auth.VerifyPassword(body.Password, hash)
	if err != nil && userOK {
		a.fail(w, apperr.Internal(err))
		return
	}
	if !userOK || !passOK {
		a.d.Log.Warn("admin login failed", "ip", a.d.Trust.ClientIP(r))
		a.fail(w, apperr.Unauthorized("invalid username or password"))
		return
	}

	ttl := a.d.Cfg.Admin.SessionTTL.Std()
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is on unless the operator opts out for plain-HTTP development
		Name: cookieName, Value: auth.SignSession(a.d.SessionSecret, body.Username, ttl, a.d.Now()),
		Path: "/", MaxAge: int(ttl.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: !a.d.Cfg.Admin.InsecureCookies,
	})
	a.d.Log.Info("admin login", "user", body.Username, "ip", a.d.Trust.ClientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"username": body.Username})
}

func (a *API) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see login
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: !a.d.Cfg.Admin.InsecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": r.Context().Value(userKey{}).(string)})
}

// --- read endpoints ---------------------------------------------------------

type projectInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	From      string   `json:"from"`
	Templates []string `json:"templates"`
	Semantics string   `json:"delivery_semantics"`
}

func (a *API) projects(w http.ResponseWriter, _ *http.Request) {
	out := make([]projectInfo, 0, len(a.d.Cfg.Projects))
	for _, p := range a.d.Cfg.Projects {
		info := projectInfo{ID: p.ID, Name: p.Name, From: p.From, Semantics: p.Delivery.Semantics, Templates: []string{}}
		for _, t := range p.Templates {
			info.Templates = append(info.Templates, t.Name)
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	counts, err := a.d.Store.Stats(r.Context())
	if err != nil {
		a.fail(w, apperr.Unavailable(msgQueueUnavailable, err))
		return
	}
	totals := map[store.State]int64{}
	for _, s := range store.AllStates {
		totals[s] = 0
	}
	for _, c := range counts {
		totals[c.State] += c.Count
	}
	writeJSON(w, http.StatusOK, map[string]any{"totals": totals, "by_project": counts})
}

type listResponse struct {
	Messages   []store.Message `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListFilter{ProjectID: q.Get("project"), Query: q.Get("q"), Before: q.Get("before")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			a.fail(w, apperr.Validation("limit must be a positive integer"))
			return
		}
		f.Limit = n
	}
	for _, s := range strings.Split(q.Get("state"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if !store.State(s).Valid() {
			a.fail(w, apperr.Validation("unknown state "+strconv.Quote(s)))
			return
		}
		f.States = append(f.States, store.State(s))
	}

	pageSize := f.Limit
	if pageSize <= 0 {
		pageSize = 50
	}
	pageSize = min(pageSize, 200)
	f.Limit = pageSize + 1 // one extra row tells us whether another page exists

	msgs, err := a.d.Store.List(r.Context(), f)
	if err != nil {
		a.fail(w, apperr.Unavailable(msgQueueUnavailable, err))
		return
	}
	resp := listResponse{Messages: msgs}
	if len(msgs) > pageSize {
		resp.Messages = msgs[:pageSize]
		resp.NextCursor = msgs[pageSize-1].ID
	}
	if resp.Messages == nil {
		resp.Messages = []store.Message{}
	}
	writeJSON(w, http.StatusOK, resp)
}

type preview struct {
	From        string          `json:"from"`
	ReplyTo     []string        `json:"reply_to,omitempty"`
	To          []string        `json:"to"`
	CC          []string        `json:"cc,omitempty"`
	BCC         []string        `json:"bcc,omitempty"`
	Subject     string          `json:"subject"`
	Text        string          `json:"text,omitempty"`
	HTML        string          `json:"html,omitempty"`
	Attachments []attachmentRef `json:"attachments,omitempty"`
}

type attachmentRef struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

type detailResponse struct {
	store.Message
	Attempts []store.Attempt `json:"attempts"`
	Preview  *preview        `json:"preview"`
}

func (a *API) detail(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	m, err := a.d.Store.Get(ctx, id)
	if err != nil {
		a.fail(w, mapStoreErr(err))
		return
	}
	attempts, err := a.d.Store.Attempts(ctx, id)
	if err != nil {
		a.fail(w, apperr.Unavailable(msgQueueUnavailable, err))
		return
	}
	resp := detailResponse{Message: *m, Attempts: attempts}
	if attempts == nil {
		resp.Attempts = []store.Attempt{}
	}
	if raw, err := a.d.Store.GetPayload(ctx, id); err == nil {
		var msg message.Message
		if json.Unmarshal(raw, &msg) == nil {
			resp.Preview = toPreview(&msg)
		}
	}
	w.Header().Set("ETag", etag(m.Version))
	writeJSON(w, http.StatusOK, resp)
}

func toPreview(m *message.Message) *preview {
	addrs := func(in []message.Address) []string {
		var out []string
		for _, a := range in {
			out = append(out, a.String())
		}
		return out
	}
	p := &preview{
		From: m.From.String(), ReplyTo: addrs(m.ReplyTo), To: addrs(m.To), CC: addrs(m.CC), BCC: addrs(m.BCC),
		Subject: m.Subject, Text: m.Text, HTML: m.HTML,
	}
	for _, at := range m.Attach {
		p.Attachments = append(p.Attachments, attachmentRef{at.Filename, at.ContentType, len(at.Data)})
	}
	return p
}

// --- mutations --------------------------------------------------------------

type action int

const (
	actionRetry action = iota
	actionCancel
)

func (a *API) apply(ctx context.Context, act action, id string, version int64) (*store.Message, error) {
	if act == actionRetry {
		m, err := a.d.Store.Retry(ctx, id, version)
		if err == nil && a.d.Wake != nil {
			a.d.Wake()
		}
		return m, err
	}
	return a.d.Store.Cancel(ctx, id, version)
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func parseIfMatch(h string) (int64, error) {
	h = strings.Trim(strings.TrimSpace(h), `"`)
	if h == "" {
		return 0, apperr.New(apperr.KindPreconditionFailed, "if-match-required",
			"send the message version you saw in an If-Match header")
	}
	v, err := strconv.ParseInt(h, 10, 64)
	if err != nil || v < 0 {
		return 0, apperr.Validation("If-Match must be a message version")
	}
	return v, nil
}

func (a *API) act(act action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			a.fail(w, err)
			return
		}
		m, err := a.apply(r.Context(), act, r.PathValue("id"), version)
		if err != nil {
			a.fail(w, mapStoreErr(err))
			return
		}
		a.audit(r, act, m.ID)
		w.Header().Set("ETag", etag(m.Version))
		writeJSON(w, http.StatusOK, m)
	}
}

type bulkRequest struct {
	Action string `json:"action"`
	Items  []struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	} `json:"items"`
}

type bulkResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Version int64  `json:"version,omitempty"`
	State   string `json:"state,omitempty"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"`
}

// bulk reports a result per item; one stale or conflicting message never
// aborts the rest.
func (a *API) bulk(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, err)
		return
	}
	var act action
	switch req.Action {
	case "retry":
		act = actionRetry
	case "cancel":
		act = actionCancel
	default:
		a.fail(w, apperr.Validation(`action must be "retry" or "cancel"`))
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxBulkItems {
		a.fail(w, apperr.Validation("items must contain between 1 and "+strconv.Itoa(maxBulkItems)+" entries"))
		return
	}

	results := make([]bulkResult, 0, len(req.Items))
	for _, it := range req.Items {
		m, err := a.apply(r.Context(), act, it.ID, it.Version)
		if err != nil {
			ae := apperr.As(mapStoreErr(err))
			results = append(results, bulkResult{ID: it.ID, Error: ae.Detail, Code: ae.Code})
			continue
		}
		a.audit(r, act, m.ID)
		results = append(results, bulkResult{ID: m.ID, OK: true, Version: m.Version, State: string(m.State)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (a *API) audit(r *http.Request, act action, id string) {
	name := "retry"
	if act == actionCancel {
		name = "cancel"
	}
	a.d.Log.Info("admin action", "action", name, "message_id", id,
		"user", r.Context().Value(userKey{}), "ip", a.d.Trust.ClientIP(r))
}

func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("message not found")
	case errors.Is(err, store.ErrConflict):
		return apperr.Conflict("state-conflict", "the message is not in a state that allows this action")
	case errors.Is(err, store.ErrVersionMismatch):
		return apperr.PreconditionFailed("the message changed since you loaded it; refresh and try again")
	case errors.Is(err, store.ErrPayloadGone):
		return apperr.Conflict("body-dropped", "the message body was removed by the retention policy and cannot be resent")
	default:
		return apperr.Unavailable(msgQueueUnavailable, err)
	}
}
