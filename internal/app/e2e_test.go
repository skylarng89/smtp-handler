package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylarng89/smtp-handler/internal/app"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/transport/smtptest"
)

const (
	adminPassword = "correct horse battery staple"
	pkOrigin      = "https://app.acme.test"
)

type env struct {
	t        *testing.T
	pub, ops string
	smtp     *smtptest.Server
	pk, sk   string
	cancel   context.CancelFunc
	done     chan error
}

type envOpts struct {
	extraProject string // YAML appended inside the project block
	pkRates      string // YAML for rate_limits.publishable
}

func newEnv(t *testing.T, opts ...envOpts) *env {
	t.Helper()
	var o envOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	srv := smtptest.New(t)
	pk, pkHash, _ := auth.GenerateKey(auth.Publishable)
	sk, skHash, _ := auth.GenerateKey(auth.Secret)
	adminHash, err := auth.HashPassword(adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	rates := o.pkRates
	if rates == "" {
		rates = `{per_key: {limit: 1000, window: 1m}, per_ip: {limit: 1000, window: 1m}, per_recipient: {limit: 0, window: 1h}}`
	}
	yaml := fmt.Sprintf(`
server: {addr: "127.0.0.1:0", admin_addr: "127.0.0.1:0", shutdown_timeout: 5s}
store: {driver: sqlite, dsn: %q}
admin:
  enabled: true
  username: ops
  password_hash: %q
  session_secret: "0123456789abcdef0123456789abcdef"
  insecure_cookies: true
delivery: {workers: 4, poll_interval: 20ms, backoff_base: 10ms, backoff_max: 40ms, max_attempts: 3, drain_timeout: 2s, lease_duration: 5s, send_timeout: 3s}
logging: {level: error}
projects:
  - id: acme
    from: "Acme <noreply@acme.test>"
    smtp: {host: %s, port: %d, tls: none}
    keys:
      - {type: publishable, hash: %q, allowed_origins: ["%s", "https://*.preview.acme.test"]}
      - {type: secret, hash: %q}
    policy: {raw_recipients: domain_allowlist, allowed_recipient_domains: [customers.test]}
    rate_limits:
      publishable: %s
    templates:
      - name: contact
        subject: "Contact from {{.name}}"
        html: "<p>{{.message}}</p>"
        recipients: {mode: fixed, to: [support@acme.test]}
        reply_to_field: email
        reply_to_name_field: name
        honeypot: website
        fields:
          name: {type: string, required: true, max: 50}
          email: {type: email, required: true}
          message: {type: text, required: true, max: 500}
      - name: dept
        subject: "Question"
        text: "{{.message}}"
        recipients:
          mode: alias
          aliases: {sales: [sales@acme.test], billing: [billing@acme.test]}
        fields:
          message: {type: text, required: true}
      - name: notify
        subject: "Notification"
        text: "Hello {{.name}}"
        public: false
        recipients: {mode: request}
      - name: internal
        subject: "Internal {{.note}}"
        text: "{{.note}}"
        public: false
        recipients: {mode: fixed, to: [ops@acme.test]}
%s
`, filepath.Join(dir, "q.db"), adminHash, srv.Host, srv.Port, pkHash, pkOrigin, skHash, rates, o.extraProject)

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatalf("app: %v", err)
	}
	e := &env{t: t, pub: "http://" + a.PublicAddr(), ops: "http://" + a.OpsAddr(), smtp: srv, pk: pk, sk: sk,
		cancel: cancel, done: make(chan error, 1)}
	go func() { e.done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(15 * time.Second):
			t.Error("app did not shut down")
		}
	})
	return e
}

type resp struct {
	*http.Response
	body []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.StatusCode, r.body)
	}
	return m
}

func (r resp) problem(t *testing.T) string {
	t.Helper()
	m := r.json(t)
	typ, _ := m["type"].(string)
	return typ[strings.LastIndex(typ, "/")+1:]
}

func do(t *testing.T, method, url string, headers map[string]string, body any) resp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return resp{res, raw}
}

func (e *env) send(key, origin string, body any, extra ...string) resp {
	h := map[string]string{"Authorization": "Bearer " + key}
	if origin != "" {
		h["Origin"] = origin
	}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return do(e.t, http.MethodPost, e.pub+"/v1/messages", h, body)
}

func (e *env) waitDelivered(n int) {
	e.t.Helper()
	if !e.smtp.WaitFor(n, 8*time.Second) {
		e.t.Fatalf("expected %d delivered messages, have %d", n, len(e.smtp.Received()))
	}
}

func contactBody() map[string]any {
	return map[string]any{
		"template": "contact",
		"data":     map[string]any{"name": "Jane Doe", "email": "Jane@Example.org", "message": "Hello <script>alert(1)</script>"},
	}
}

func TestTemplateSendFromBrowser(t *testing.T) {
	e := newEnv(t)
	res := e.send(e.pk, pkOrigin, contactBody())
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", res.StatusCode, res.body)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != pkOrigin || res.Header.Get("X-Request-Id") == "" {
		t.Fatalf("missing CORS/request-id headers: %v", res.Header)
	}
	id, _ := res.json(t)["id"].(string)
	if id == "" {
		t.Fatalf("no id: %s", res.body)
	}

	e.waitDelivered(1)
	got := e.smtp.Received()[0]
	if len(got.To) != 1 || !strings.EqualFold(strings.Trim(got.To[0], "<>"), "support@acme.test") {
		t.Fatalf("recipients = %v: the browser must not influence them", got.To)
	}
	for _, want := range []string{"Reply-To:", "Jane@example.org", "Subject: Contact from Jane Doe", "Message-ID: <" + id + "@acme.test>"} {
		if !strings.Contains(got.Data, want) {
			t.Errorf("message lacks %q:\n%s", want, got.Data)
		}
	}
	// Only the HTML part must be escaped; the plain-text part shows the text as typed.
	htmlPart := got.Data[strings.Index(got.Data, "text/html"):]
	if strings.Contains(htmlPart, "<script>") || !strings.Contains(htmlPart, "&lt;script&gt;") {
		t.Errorf("HTML part was not escaped:\n%s", htmlPart)
	}
	if !strings.Contains(got.Data, "text/plain") {
		t.Errorf("a plain-text alternative must be generated from the HTML")
	}
}

func TestBrowserGuards(t *testing.T) {
	e := newEnv(t)

	cases := []struct {
		name   string
		key    string
		origin string
		body   any
		status int
		code   string
	}{
		{"wrong origin", e.pk, "https://evil.test", contactBody(), 403, "origin-not-allowed"},
		{"missing origin", e.pk, "", contactBody(), 403, "origin-not-allowed"},
		{"scheme matters", e.pk, "http://app.acme.test", contactBody(), 403, "origin-not-allowed"},
		{"subdomain is not implied", e.pk, "https://x.app.acme.test", contactBody(), 403, "origin-not-allowed"},
		{"raw mode needs a secret key", e.pk, pkOrigin, map[string]any{"to": "a@customers.test", "subject": "s", "text": "t"}, 403, "raw-mode-requires-secret-key"},
		{"private template is hidden", e.pk, pkOrigin, map[string]any{"template": "internal", "data": map[string]any{"note": "x"}}, 404, "not-found"},
		{"unknown template", e.pk, pkOrigin, map[string]any{"template": "nope"}, 404, "not-found"},
		{"client cannot pick recipients", e.pk, pkOrigin, map[string]any{"template": "contact", "to": "victim@x.test", "data": map[string]any{"name": "a", "email": "a@b.co", "message": "m"}}, 400, "validation-failed"},
		{"raw fields with template", e.pk, pkOrigin, map[string]any{"template": "contact", "subject": "x", "data": map[string]any{"name": "a", "email": "a@b.co", "message": "m"}}, 400, "validation-failed"},
		{"unknown data field", e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": "a", "email": "a@b.co", "message": "m", "evil": "x"}}, 400, "validation-failed"},
		{"missing required", e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": "a"}}, 400, "validation-failed"},
		{"bad email", e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": "a", "email": "nope", "message": "m"}}, 400, "validation-failed"},
		{"newline in single-line field", e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": "a\r\nBcc: x@y.z", "email": "a@b.co", "message": "m"}}, 400, "validation-failed"},
		{"nested data", e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": map[string]any{"x": 1}}}, 400, "validation-failed"},
		{"unknown top-level field", e.pk, pkOrigin, map[string]any{"template": "contact", "bogus": 1}, 400, "validation-failed"},
		{"bad alias", e.pk, pkOrigin, map[string]any{"template": "dept", "to": "ceo", "data": map[string]any{"message": "m"}}, 400, "validation-failed"},
		{"invalid key", "pk_nope", pkOrigin, contactBody(), 401, "unauthorized"},
		{"secret key from a browser", e.sk, pkOrigin, contactBody(), 403, "secret-key-in-browser"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := e.send(tc.key, tc.origin, tc.body)
			if res.StatusCode != tc.status || res.problem(t) != tc.code {
				t.Fatalf("got %d %s, want %d %s: %s", res.StatusCode, res.problem(t), tc.status, tc.code, res.body)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q", ct)
			}
		})
	}
	if n := len(e.smtp.Received()); n != 0 {
		t.Fatalf("%d messages leaked through rejected requests", n)
	}
	if res := do(t, "POST", e.pub+"/v1/messages", nil, contactBody()); res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("missing key: %d %v", res.StatusCode, res.Header)
	}

	// Field-level errors are reported per field.
	res := e.send(e.pk, pkOrigin, map[string]any{"template": "contact", "data": map[string]any{"name": "a"}})
	errs, _ := res.json(t)["errors"].([]any)
	if len(errs) != 2 {
		t.Fatalf("expected errors for email and message, got %v", errs)
	}
}

func TestWildcardOriginAndPreflight(t *testing.T) {
	e := newEnv(t)
	if res := e.send(e.pk, "https://pr-42.preview.acme.test", contactBody()); res.StatusCode != http.StatusAccepted {
		t.Fatalf("wildcard origin rejected: %d %s", res.StatusCode, res.body)
	}

	pre := do(t, http.MethodOptions, e.pub+"/v1/messages", map[string]string{
		"Origin": pkOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type",
	}, nil)
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != pkOrigin ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Fatalf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
	bad := do(t, http.MethodOptions, e.pub+"/v1/messages", map[string]string{"Origin": "https://evil.test", "Access-Control-Request-Method": "POST"}, nil)
	if bad.StatusCode != http.StatusForbidden || bad.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unknown origin preflight: %d %v", bad.StatusCode, bad.Header)
	}
}

func TestSecretKeyRawSendAndPolicy(t *testing.T) {
	e := newEnv(t)
	ok := e.send(e.sk, "", map[string]any{
		"to":      []any{"Alice <alice@customers.test>", map[string]any{"email": "bob@customers.test", "name": "Bob"}},
		"subject": "Your receipt", "html": "<h1>Thanks</h1><p>Order <a href=\"https://shop.test/o/1\">#1</a></p>",
		"attachments": []any{map[string]any{"filename": "receipt.txt", "content": "cmVjZWlwdCBib2R5"}},
	})
	if ok.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", ok.StatusCode, ok.body)
	}
	e.waitDelivered(1)
	data := e.smtp.Received()[0].Data
	for _, want := range []string{"multipart/mixed", "multipart/alternative", `filename="receipt.txt"`, "Thanks", "#1 (https://shop.test/o/1)"} {
		if !strings.Contains(data, want) {
			t.Errorf("message lacks %q", want)
		}
	}

	for name, body := range map[string]map[string]any{
		"recipient outside the allowlist": {"to": "x@elsewhere.test", "subject": "s", "text": "t"},
		"disallowed from address":         {"to": "a@customers.test", "from": "ceo@acme.test", "subject": "s", "text": "t"},
		"executable attachment":           {"to": "a@customers.test", "subject": "s", "text": "t", "attachments": []any{map[string]any{"filename": "x.exe", "content": "TVo="}}},
		"mismatched attachment type":      {"to": "a@customers.test", "subject": "s", "text": "t", "attachments": []any{map[string]any{"filename": "x.png", "content": "PGh0bWw+"}}},
		"newline in subject":              {"to": "a@customers.test", "subject": "hi\r\nBcc: x@y.z", "text": "t"},
		"no body":                         {"to": "a@customers.test", "subject": "s"},
		"data without template":           {"to": "a@customers.test", "subject": "s", "text": "t", "data": map[string]any{"a": "b"}},
	} {
		t.Run(name, func(t *testing.T) {
			if res := e.send(e.sk, "", body); res.StatusCode < 400 || res.StatusCode >= 500 {
				t.Fatalf("status %d: %s", res.StatusCode, res.body)
			}
		})
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(e.smtp.Received()); n != 1 {
		t.Fatalf("%d messages delivered, want only the valid one", n)
	}

	// Secret keys may use any template, including private ones.
	if res := e.send(e.sk, "", map[string]any{"template": "internal", "data": map[string]any{"note": "disk full"}}); res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, res.body)
	}
}

func TestRequestRecipientTemplatesAreBackendOnly(t *testing.T) {
	e := newEnv(t)
	body := func(to string) map[string]any {
		return map[string]any{"template": "notify", "to": to, "data": map[string]any{"name": "Ada"}}
	}

	// A backend (secret key) picks the recipient; the project policy still applies.
	if res := e.send(e.sk, "", body("ada@customers.test")); res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, res.body)
	}
	e.waitDelivered(1)
	got := e.smtp.Received()[0]
	if len(got.To) != 1 || !strings.Contains(got.To[0], "ada@customers.test") || !strings.Contains(got.Data, "Hello Ada") {
		t.Fatalf("delivered %+v", got)
	}
	if res := e.send(e.sk, "", body("ada@elsewhere.test")); res.StatusCode != 403 || res.problem(t) != "recipient-domain-not-allowed" {
		t.Fatalf("policy bypassed through a template: %d %s", res.StatusCode, res.body)
	}
	if res := e.send(e.sk, "", map[string]any{"template": "notify", "data": map[string]any{"name": "Ada"}}); res.StatusCode != 400 {
		t.Fatalf("missing recipient accepted: %d", res.StatusCode)
	}

	// Browsers must not be able to reach it: it looks like any non-public template.
	if res := e.send(e.pk, pkOrigin, body("victim@elsewhere.test")); res.StatusCode != 404 {
		t.Fatalf("a publishable key reached a request-recipient template: %d %s", res.StatusCode, res.body)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(e.smtp.Received()); n != 1 {
		t.Fatalf("%d messages delivered", n)
	}
}

func TestAliasRecipients(t *testing.T) {
	e := newEnv(t)
	if res := e.send(e.pk, pkOrigin, map[string]any{"template": "dept", "to": "billing", "data": map[string]any{"message": "invoice question"}}); res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, res.body)
	}
	e.waitDelivered(1)
	if to := e.smtp.Received()[0].To; len(to) != 1 || !strings.Contains(to[0], "billing@acme.test") {
		t.Fatalf("alias resolved to %v", to)
	}
}

func TestHoneypotLooksSuccessfulButSendsNothing(t *testing.T) {
	e := newEnv(t)
	body := contactBody()
	body["data"].(map[string]any)["website"] = "http://spam.test"
	res := e.send(e.pk, pkOrigin, body)
	if res.StatusCode != 202 || res.json(t)["id"] == "" {
		t.Fatalf("honeypot response must look like success: %d %s", res.StatusCode, res.body)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(e.smtp.Received()); n != 0 {
		t.Fatalf("honeypot submission was delivered (%d)", n)
	}
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t)

	first := e.send(e.pk, pkOrigin, contactBody(), "Idempotency-Key", "form-123")
	second := e.send(e.pk, pkOrigin, contactBody(), "Idempotency-Key", "form-123")
	if first.StatusCode != 202 || second.StatusCode != 202 {
		t.Fatalf("%d / %d", first.StatusCode, second.StatusCode)
	}
	if first.json(t)["id"] != second.json(t)["id"] {
		t.Fatalf("a retry created a second message: %s vs %s", first.body, second.body)
	}
	if second.Header.Get("Idempotent-Replayed") != "true" || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("replay header wrong: %q / %q", first.Header.Get("Idempotent-Replayed"), second.Header.Get("Idempotent-Replayed"))
	}

	other := contactBody()
	other["data"].(map[string]any)["message"] = "something else"
	if res := e.send(e.pk, pkOrigin, other, "Idempotency-Key", "form-123"); res.StatusCode != 422 || res.problem(t) != "idempotency-key-reused" {
		t.Fatalf("reusing a key with a different body: %d %s", res.StatusCode, res.body)
	}
	if res := e.send(e.pk, pkOrigin, contactBody(), "Idempotency-Key", "bad key!"); res.StatusCode != 400 {
		t.Fatalf("malformed key accepted: %d", res.StatusCode)
	}
	e.waitDelivered(1)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.smtp.Received()); n != 1 {
		t.Fatalf("delivered %d messages for one idempotent request", n)
	}
}

func TestIdempotencyUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	const n = 25
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]int{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := e.send(e.sk, "", map[string]any{"to": "a@customers.test", "subject": "once", "text": "t"}, "Idempotency-Key", "pay-9")
			if res.StatusCode != 202 {
				t.Errorf("status %d: %s", res.StatusCode, res.body)
				return
			}
			mu.Lock()
			ids[res.json(t)["id"].(string)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 1 {
		t.Fatalf("%d concurrent identical requests produced %d messages", n, len(ids))
	}
	e.waitDelivered(1)
	time.Sleep(200 * time.Millisecond)
	if got := len(e.smtp.Received()); got != 1 {
		t.Fatalf("delivered %d", got)
	}
}

func TestBodyAndContentTypeLimits(t *testing.T) {
	e := newEnv(t)
	big := map[string]any{"template": "contact", "data": map[string]any{"name": "a", "email": "a@b.co", "message": strings.Repeat("x", 80<<10)}}
	if res := e.send(e.pk, pkOrigin, big); res.StatusCode != 413 {
		t.Fatalf("oversized browser payload: %d %s", res.StatusCode, res.body)
	}
	if res := do(t, "POST", e.pub+"/v1/messages", map[string]string{"Authorization": "Bearer " + e.pk, "Origin": pkOrigin, "Content-Type": "text/plain"}, "hi"); res.StatusCode != 415 {
		t.Fatalf("wrong content type: %d", res.StatusCode)
	}
	for name, body := range map[string]string{"empty": "", "garbage": "{nope", "array": "[]", "trailing": `{"template":"contact"} {"x":1}`} {
		t.Run(name, func(t *testing.T) {
			res := do(t, "POST", e.pub+"/v1/messages", map[string]string{"Authorization": "Bearer " + e.pk, "Origin": pkOrigin, "Content-Type": "application/json"}, body)
			if res.StatusCode != 400 {
				t.Fatalf("%d %s", res.StatusCode, res.body)
			}
		})
	}
}

func TestRateLimiting(t *testing.T) {
	e := newEnv(t, envOpts{pkRates: `{per_key: {limit: 3, window: 1m}, per_ip: {limit: 0, window: 1m}, per_recipient: {limit: 0, window: 1h}}`})
	for i := range 3 {
		if res := e.send(e.pk, pkOrigin, contactBody()); res.StatusCode != 202 {
			t.Fatalf("request %d: %d %s", i, res.StatusCode, res.body)
		}
	}
	res := e.send(e.pk, pkOrigin, contactBody())
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" || res.problem(t) != "rate-limited" {
		t.Fatalf("4th request: %d %v %s", res.StatusCode, res.Header, res.body)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != pkOrigin {
		t.Fatalf("429 must carry CORS headers so browsers can read it, got %q", got)
	}
	// The secret key has its own bucket.
	if res := e.send(e.sk, "", map[string]any{"to": "a@customers.test", "subject": "s", "text": "t"}); res.StatusCode != 202 {
		t.Fatalf("secret key was throttled by the publishable key's limit: %d", res.StatusCode)
	}
}

func TestPerRecipientLimitCapsInboxFlooding(t *testing.T) {
	e := newEnv(t, envOpts{pkRates: `{per_key: {limit: 1000, window: 1m}, per_ip: {limit: 0, window: 1m}, per_recipient: {limit: 2, window: 1h}}`})
	for i := range 2 {
		if res := e.send(e.pk, pkOrigin, contactBody()); res.StatusCode != 202 {
			t.Fatalf("request %d: %d", i, res.StatusCode)
		}
	}
	if res := e.send(e.pk, pkOrigin, contactBody()); res.StatusCode != 429 {
		t.Fatalf("flooding a recipient was not stopped: %d", res.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	e := newEnv(t)
	res := e.send(e.sk, "", map[string]any{"to": "a@customers.test", "subject": "s", "text": "t"})
	id := res.json(t)["id"].(string)

	var status string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r := do(t, "GET", e.pub+"/v1/messages/"+id, map[string]string{"Authorization": "Bearer " + e.sk}, nil)
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, r.body)
		}
		if status, _ = r.json(t)["status"].(string); status == "sent" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "sent" {
		t.Fatalf("status = %q", status)
	}
	if r := do(t, "GET", e.pub+"/v1/messages/"+id, map[string]string{"Authorization": "Bearer " + e.pk, "Origin": pkOrigin}, nil); r.StatusCode != 403 {
		t.Fatalf("publishable key can read status: %d", r.StatusCode)
	}
	if r := do(t, "GET", e.pub+"/v1/messages/01NOTAREALID", map[string]string{"Authorization": "Bearer " + e.sk}, nil); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestCaptcha(t *testing.T) {
	var verified []string
	var mu sync.Mutex
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		verified = append(verified, r.Form.Get("response"))
		mu.Unlock()
		if r.Form.Get("secret") != "s3cret" {
			t.Errorf("wrong secret sent to provider")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": r.Form.Get("response") == "good"})
	}))
	defer verify.Close()

	e := newEnv(t, envOpts{extraProject: fmt.Sprintf("    captcha: {provider: turnstile, secret: s3cret, verify_url: %q}\n", verify.URL)})
	withToken := func(token string) map[string]any {
		b := contactBody()
		if token != "" {
			b["captcha_token"] = token
		}
		return b
	}

	if res := e.send(e.pk, pkOrigin, withToken("")); res.StatusCode != 403 || res.problem(t) != "captcha-required" {
		t.Fatalf("missing token: %d %s", res.StatusCode, res.body)
	}
	if res := e.send(e.pk, pkOrigin, withToken("bad")); res.StatusCode != 403 || res.problem(t) != "captcha-failed" {
		t.Fatalf("bad token: %d %s", res.StatusCode, res.body)
	}
	if res := e.send(e.pk, pkOrigin, withToken("good"), "Idempotency-Key", "c-1"); res.StatusCode != 202 {
		t.Fatalf("good token: %d %s", res.StatusCode, res.body)
	}
	// Tokens are single-use: a retry with the same idempotency key must be
	// answered as a replay without asking the provider again.
	before := len(verified)
	if res := e.send(e.pk, pkOrigin, withToken("good"), "Idempotency-Key", "c-1"); res.StatusCode != 202 || res.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", res.StatusCode, res.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(verified) != before {
		t.Fatal("a replayed request consumed another captcha verification")
	}
	// Secret keys (backends) are not subject to captcha.
	mu.Unlock()
	if res := e.send(e.sk, "", map[string]any{"to": "a@customers.test", "subject": "s", "text": "t"}); res.StatusCode != 202 {
		t.Fatalf("secret key hit captcha: %d", res.StatusCode)
	}
	mu.Lock()
}

func TestCaptchaProviderOutageFailsClosed(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	e := newEnv(t, envOpts{extraProject: fmt.Sprintf("    captcha: {provider: hcaptcha, secret: s, verify_url: %q, timeout: 500ms}\n", down.URL)})
	b := contactBody()
	b["captcha_token"] = "good"
	if res := e.send(e.pk, pkOrigin, b); res.StatusCode != 503 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("an unreachable captcha provider must not become a bypass: %d %s", res.StatusCode, res.body)
	}
}

func TestOpsEndpoints(t *testing.T) {
	e := newEnv(t)
	e.send(e.pk, pkOrigin, contactBody())
	for path, want := range map[string]string{"/healthz": "ok", "/readyz": "ready", "/metrics": "smtph_messages_accepted_total"} {
		res := do(t, "GET", e.ops+path, nil, nil)
		if res.StatusCode != 200 || !strings.Contains(string(res.body), want) {
			t.Fatalf("%s: %d %s", path, res.StatusCode, res.body)
		}
	}
	if res := do(t, "GET", e.ops+"/v1/messages/x", nil, nil); res.StatusCode == 200 {
		t.Fatal("public API must not be reachable on the ops listener")
	}
}

func TestReadinessFlipsDuringShutdown(t *testing.T) {
	e := newEnv(t)
	e.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(e.ops + "/readyz") //nolint:noctx // test helper polling a local listener
		if err != nil {
			return // listener already closed: correct while draining
		}
		_ = res.Body.Close()
		if res.StatusCode == 503 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("readiness did not flip to not-ready after shutdown began")
}

// --- admin API ---------------------------------------------------------------

type adminClient struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
}

func (c *adminClient) do(method, path string, body any, headers ...string) resp {
	h := map[string]string{}
	if c.cookie != nil {
		h["Cookie"] = c.cookie.String()
	}
	for i := 0; i+1 < len(headers); i += 2 {
		h[headers[i]] = headers[i+1]
	}
	return do(c.t, method, c.base+"/admin/api/v1"+path, h, body)
}

func (e *env) admin() *adminClient {
	c := &adminClient{t: e.t, base: e.ops}
	res := c.do("POST", "/login", map[string]string{"username": "ops", "password": adminPassword})
	if res.StatusCode != 200 {
		e.t.Fatalf("login: %d %s", res.StatusCode, res.body)
	}
	for _, ck := range res.Cookies() {
		if ck.Name == "smtph_admin" {
			c.cookie = ck
			if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
				e.t.Fatalf("session cookie flags: %+v", ck)
			}
		}
	}
	if c.cookie == nil {
		e.t.Fatal("no session cookie")
	}
	return c
}

func TestAdminAuthentication(t *testing.T) {
	e := newEnv(t)
	anon := &adminClient{t: t, base: e.ops}
	for _, p := range []string{"/messages", "/stats", "/projects", "/session", "/messages/x"} {
		if res := anon.do("GET", p, nil); res.StatusCode != 401 {
			t.Fatalf("GET %s without a session: %d", p, res.StatusCode)
		}
	}
	for _, creds := range []map[string]string{
		{"username": "ops", "password": "wrong password!!"},
		{"username": "root", "password": adminPassword},
	} {
		if res := anon.do("POST", "/login", creds); res.StatusCode != 401 {
			t.Fatalf("bad credentials accepted: %d", res.StatusCode)
		}
	}
	forged := &adminClient{t: t, base: e.ops, cookie: &http.Cookie{Name: "smtph_admin", Value: "v1.eyJ1Ijoib3BzIiwiZSI6OTk5OTk5OTk5OX0.AAAA"}} //nolint:gosec // G124: deliberately forged test cookie
	if res := forged.do("GET", "/session", nil); res.StatusCode != 401 {
		t.Fatalf("forged session accepted: %d", res.StatusCode)
	}

	c := e.admin()
	if res := c.do("GET", "/session", nil); res.StatusCode != 200 || res.json(t)["username"] != "ops" {
		t.Fatalf("%d %s", res.StatusCode, res.body)
	}
	// Cross-site browsers cannot drive state-changing endpoints.
	res := c.do("POST", "/messages/bulk", map[string]any{"action": "retry", "items": []any{map[string]any{"id": "x", "version": 1}}},
		"Origin", "https://evil.test", "Sec-Fetch-Site", "cross-site")
	if res.StatusCode != 403 {
		t.Fatalf("cross-origin mutation allowed: %d %s", res.StatusCode, res.body)
	}
}

func TestAdminQueueWorkflow(t *testing.T) {
	e := newEnv(t)
	c := e.admin()

	// One message that fails permanently, one that succeeds.
	failing := e.send(e.sk, "", map[string]any{"to": "permfail@customers.test", "subject": "Will bounce", "text": "t"}).json(t)["id"].(string)
	e.send(e.sk, "", map[string]any{"to": "ok@customers.test", "subject": "Will pass", "text": "t"})

	var failedMsg map[string]any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		res := c.do("GET", "/messages/"+failing, nil)
		if m := res.json(t); m["state"] == "failed" {
			failedMsg = m
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if failedMsg == nil {
		t.Fatal("message never reached the dead-letter state")
	}
	if failedMsg["last_error_code"].(float64) != 550 {
		t.Fatalf("last error: %v", failedMsg)
	}
	preview := failedMsg["preview"].(map[string]any)
	if preview["subject"] != "Will bounce" || len(failedMsg["attempts"].([]any)) != 1 {
		t.Fatalf("detail: %v", failedMsg)
	}

	list := c.do("GET", "/messages?state=failed", nil).json(t)["messages"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != failing {
		t.Fatalf("failed filter: %v", list)
	}
	if res := c.do("GET", "/messages?state=nonsense", nil); res.StatusCode != 400 {
		t.Fatalf("bad filter: %d", res.StatusCode)
	}
	stats := c.do("GET", "/stats", nil).json(t)["totals"].(map[string]any)
	if stats["failed"].(float64) != 1 {
		t.Fatalf("stats: %v", stats)
	}

	version := fmt.Sprintf("%d", int64(failedMsg["version"].(float64)))
	if res := c.do("POST", "/messages/"+failing+"/retry", nil); res.StatusCode != 412 {
		t.Fatalf("retry without If-Match: %d", res.StatusCode)
	}
	if res := c.do("POST", "/messages/"+failing+"/retry", nil, "If-Match", `"1"`); res.StatusCode != 412 {
		t.Fatalf("retry with a stale version: %d %s", res.StatusCode, res.body)
	}
	res := c.do("POST", "/messages/"+failing+"/retry", nil, "If-Match", `"`+version+`"`)
	if res.StatusCode != 200 || res.json(t)["state"] != "queued" {
		t.Fatalf("retry: %d %s", res.StatusCode, res.body)
	}
	// Doing it twice with the same version loses: someone else (or the worker) moved on.
	if res := c.do("POST", "/messages/"+failing+"/retry", nil, "If-Match", `"`+version+`"`); res.StatusCode != 412 && res.StatusCode != 409 {
		t.Fatalf("second retry with the same version: %d", res.StatusCode)
	}

	// It fails again (permfail is still permanent); wait, then cancel via bulk.
	deadline = time.Now().Add(8 * time.Second)
	var again map[string]any
	for time.Now().Before(deadline) {
		if m := c.do("GET", "/messages/"+failing, nil).json(t); m["state"] == "failed" && m["version"] != failedMsg["version"] {
			again = m
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if again == nil {
		t.Fatal("retried message did not fail again")
	}
	bulk := c.do("POST", "/messages/bulk", map[string]any{"action": "cancel", "items": []any{
		map[string]any{"id": failing, "version": again["version"]},
		map[string]any{"id": "missing", "version": 1},
	}}).json(t)["results"].([]any)
	if r := bulk[0].(map[string]any); r["ok"] != true || r["state"] != "canceled" {
		t.Fatalf("bulk[0] = %v", r)
	}
	if r := bulk[1].(map[string]any); r["ok"] == true || r["code"] != "not-found" {
		t.Fatalf("bulk[1] = %v (one bad item must not abort the rest)", r)
	}
	if res := c.do("POST", "/messages/bulk", map[string]any{"action": "explode", "items": []any{}}); res.StatusCode != 400 {
		t.Fatalf("%d", res.StatusCode)
	}

	if projects := c.do("GET", "/projects", nil).body; !strings.Contains(string(projects), `"acme"`) || strings.Contains(string(projects), "sha256") {
		t.Fatalf("projects endpoint: %s", projects)
	}
}

func TestAdminSessionRoundTripAcrossLogout(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	res := c.do("POST", "/logout", nil)
	if res.StatusCode != 204 {
		t.Fatalf("%d", res.StatusCode)
	}
	cleared := false
	for _, ck := range res.Cookies() {
		cleared = cleared || (ck.Name == "smtph_admin" && ck.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("logout did not clear the cookie")
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	e := newEnv(t)
	if res := do(t, "GET", e.pub+"/", nil, nil); res.StatusCode != 404 || res.problem(t) != "not-found" {
		t.Fatalf("%d %s", res.StatusCode, res.body)
	}
	if res := do(t, "DELETE", e.pub+"/v1/messages", nil, nil); res.StatusCode != 404 {
		t.Fatalf("%d", res.StatusCode)
	}
}
