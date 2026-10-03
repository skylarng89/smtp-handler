package transport_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/transport"
	"github.com/skylarng89/smtp-handler/internal/transport/smtptest"
)

func newTransport(t *testing.T, srv *smtptest.Server, mutate ...func(*config.SMTP)) *transport.SMTP {
	t.Helper()
	cfg := config.SMTP{
		Host: srv.Host, Port: srv.Port, TLS: "none", AuthMethod: "auto",
		Timeout:        config.Duration(5 * time.Second),
		MaxConnections: 2, IdleTimeout: config.Duration(30 * time.Second),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	tr, err := transport.NewSMTP("proj", "node-1", cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func msg(to ...string) *message.Message {
	var rcpts []message.Address
	for _, a := range to {
		rcpts = append(rcpts, message.Address{Email: a})
	}
	return &message.Message{
		ID: "01TEST", MessageID: "01TEST@example.com", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		From: message.Address{Name: "Acme", Email: "noreply@example.com"},
		To:   rcpts, Subject: "Hello ✓", Text: "plain body", HTML: "<p>html body</p>",
	}
}

func TestSendDeliversWithExpectedHeaders(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv)

	m := msg("user@example.org")
	m.BCC = []message.Address{{Email: "hidden@example.org"}}
	m.ReplyTo = []message.Address{{Name: "Jane", Email: "jane@example.org"}}

	if res := tr.Send(context.Background(), m); !res.OK() {
		t.Fatalf("send failed: %+v", res)
	}
	got := srv.Received()
	if len(got) != 1 {
		t.Fatalf("received %d messages", len(got))
	}
	if !contains(got[0].To, "hidden@example.org") || !contains(got[0].To, "user@example.org") {
		t.Fatalf("envelope recipients = %v (BCC must be in the envelope)", got[0].To)
	}

	parsed, err := mail.ReadMessage(strings.NewReader(got[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	h := parsed.Header
	if h.Get("Message-Id") != "<01TEST@example.com>" {
		t.Errorf("Message-ID = %q", h.Get("Message-Id"))
	}
	if !strings.Contains(h.Get("From"), "noreply@example.com") || !strings.Contains(h.Get("Reply-To"), "jane@example.org") {
		t.Errorf("From/Reply-To = %q / %q", h.Get("From"), h.Get("Reply-To"))
	}
	if h.Get("Bcc") != "" || strings.Contains(got[0].Data, "hidden@example.org\r\n") && strings.Contains(strings.SplitN(got[0].Data, "\r\n\r\n", 2)[0], "hidden@") {
		t.Errorf("BCC leaked into the headers")
	}
	if !strings.HasPrefix(h.Get("Content-Type"), "multipart/alternative") {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
	if h.Get("User-Agent") != "" || h.Get("X-Mailer") != "" {
		t.Errorf("client identification headers must not be sent: %v", h)
	}
	if h.Get("Date") == "" || !strings.Contains(h.Get("Subject"), "=?") {
		t.Errorf("Date/Subject: %q / %q", h.Get("Date"), h.Get("Subject"))
	}
}

func TestSendReusesConnections(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv)
	for i := range 6 {
		if res := tr.Send(context.Background(), msg("user@example.org")); !res.OK() {
			t.Fatalf("send %d: %+v", i, res)
		}
	}
	if n := srv.Connections(); n != 1 {
		t.Fatalf("opened %d connections for 6 sequential sends, want 1", n)
	}
}

func TestSendClassifiesServerReplies(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv)

	tests := []struct {
		to       string
		class    transport.Class
		code     int
		enhanced string
	}{
		{"tempfail@example.org", transport.ClassTransient, 451, "4.2.0"},
		{"permfail@example.org", transport.ClassPermanent, 550, "5.1.1"},
		{"dropdata@example.org", transport.ClassUnknown, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.to, func(t *testing.T) {
			res := tr.Send(context.Background(), msg(tc.to))
			if res.Class != tc.class || res.Code != tc.code {
				t.Fatalf("got %+v, want class=%s code=%d", res, tc.class, tc.code)
			}
			if tc.enhanced != "" && res.Enhanced != tc.enhanced {
				t.Fatalf("enhanced = %q, want %q", res.Enhanced, tc.enhanced)
			}
		})
	}
	// A failed connection is never reused, but the transport recovers.
	if res := tr.Send(context.Background(), msg("ok@example.org")); !res.OK() {
		t.Fatalf("transport did not recover after failures: %+v", res)
	}
}

func TestAuth(t *testing.T) {
	srv := smtptest.New(t, smtptest.WithAuth("mailer", "s3cret"))

	good := newTransport(t, srv, func(c *config.SMTP) { c.Username, c.Password, c.AuthMethod = "mailer", "s3cret", "plain" })
	if res := good.Send(context.Background(), msg("user@example.org")); !res.OK() {
		t.Fatalf("valid credentials rejected: %+v", res)
	}

	bad := newTransport(t, srv, func(c *config.SMTP) { c.Username, c.Password, c.AuthMethod = "mailer", "wrong", "plain" })
	res := bad.Send(context.Background(), msg("user@example.org"))
	if res.Class != transport.ClassConfig || res.Code != 535 {
		t.Fatalf("bad password: got %+v, want config/535", res)
	}

	anon := newTransport(t, srv)
	res = anon.Send(context.Background(), msg("user@example.org"))
	if res.Class != transport.ClassConfig || res.Code != 530 {
		t.Fatalf("missing auth: got %+v, want config/530", res)
	}
}

func TestUnreachableServerIsTransient(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv, func(c *config.SMTP) { c.Port = 1 }) // nothing listens on port 1
	res := tr.Send(context.Background(), msg("user@example.org"))
	if res.Class != transport.ClassTransient {
		t.Fatalf("got %+v, want transient", res)
	}
}

func TestContextCancelDuringSendIsUnknown(t *testing.T) {
	srv := smtptest.New(t)
	srv.SlowDelay = 5 * time.Second
	tr := newTransport(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := tr.Send(ctx, msg("slow@example.org"))
	if res.Class != transport.ClassUnknown {
		t.Fatalf("got %+v, want unknown outcome", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("send did not abort promptly: %s", time.Since(start))
	}
}

func TestConnectionCapIsRespected(t *testing.T) {
	srv := smtptest.New(t)
	srv.SlowDelay = 150 * time.Millisecond
	tr := newTransport(t, srv, func(c *config.SMTP) { c.MaxConnections = 2 })

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			to := "slow@example.org"
			if res := tr.Send(context.Background(), msg(to)); !res.OK() {
				t.Errorf("send %d: %+v", i, res)
			}
		}()
	}
	wg.Wait()
	if peak := srv.PeakConcurrent(); peak > 2 {
		t.Fatalf("peak concurrent connections = %d, cap is 2", peak)
	}
	if got := len(srv.Received()); got != 8 {
		t.Fatalf("delivered %d/8", got)
	}
}

func TestAttachmentsAreDelivered(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv)
	m := msg("user@example.org")
	m.Attach = []message.Attachment{{Filename: "report.txt", ContentType: "text/plain", Data: []byte("quarterly numbers")}}
	if res := tr.Send(context.Background(), m); !res.OK() {
		t.Fatalf("send failed: %+v", res)
	}
	data := srv.Received()[0].Data
	if !strings.Contains(data, "multipart/mixed") || !strings.Contains(data, `filename="report.txt"`) {
		t.Fatalf("attachment missing from message:\n%s", data)
	}
}

func TestNoBodyIsPermanent(t *testing.T) {
	srv := smtptest.New(t)
	tr := newTransport(t, srv)
	m := msg("user@example.org")
	m.Text, m.HTML = "", ""
	if res := tr.Send(context.Background(), m); res.Class != transport.ClassPermanent {
		t.Fatalf("got %+v", res)
	}
}

func TestClassifyDial(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want transport.Class
	}{
		{"auth failure", &textproto.Error{Code: 535, Msg: "bad creds"}, transport.ClassConfig},
		{"server refuses session", &textproto.Error{Code: 554, Msg: "no service"}, transport.ClassConfig},
		{"greylisting", &textproto.Error{Code: 451, Msg: "later"}, transport.ClassTransient},
		{"timeout", context.DeadlineExceeded, transport.ClassTransient},
		{"network", errors.New("dial tcp: connection refused"), transport.ClassTransient},
		{"wrapped code", fmt.Errorf("auth: %w", &textproto.Error{Code: 535, Msg: "x"}), transport.ClassConfig},
		{"code in text", errors.New("failed: 535 5.7.8 authentication failed"), transport.ClassConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := transport.ClassifyDial(tc.err).Class; got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.Trim(s, "<>"), want) {
			return true
		}
	}
	return false
}

func TestTLSModes(t *testing.T) {
	t.Run("starttls with verification disabled", func(t *testing.T) {
		srv := smtptest.New(t, smtptest.WithSTARTTLS())
		tr := newTransport(t, srv, func(c *config.SMTP) { c.TLS, c.InsecureSkipVerify = "starttls", true })
		if res := tr.Send(context.Background(), msg("user@example.org")); !res.OK() {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("certificates are verified by default", func(t *testing.T) {
		srv := smtptest.New(t, smtptest.WithSTARTTLS())
		tr := newTransport(t, srv, func(c *config.SMTP) { c.TLS = "starttls" })
		res := tr.Send(context.Background(), msg("user@example.org"))
		if res.Class != transport.ClassConfig {
			t.Fatalf("an untrusted certificate must be a config error, got %+v", res)
		}
		if len(srv.Received()) != 0 {
			t.Fatal("message was sent over an unverified channel")
		}
	})
	t.Run("starttls is mandatory", func(t *testing.T) {
		srv := smtptest.New(t) // does not offer STARTTLS
		tr := newTransport(t, srv, func(c *config.SMTP) { c.TLS = "starttls" })
		res := tr.Send(context.Background(), msg("user@example.org"))
		if res.Class != transport.ClassConfig || len(srv.Received()) != 0 {
			t.Fatalf("downgrade must fail closed, got %+v", res)
		}
	})
	t.Run("implicit tls", func(t *testing.T) {
		srv := smtptest.New(t, smtptest.WithImplicitTLS(), smtptest.WithAuth("u", "p"))
		tr := newTransport(t, srv, func(c *config.SMTP) {
			c.TLS, c.InsecureSkipVerify, c.Username, c.Password = "tls", true, "u", "p"
		})
		for range 3 {
			if res := tr.Send(context.Background(), msg("user@example.org")); !res.OK() {
				t.Fatalf("%+v", res)
			}
		}
		if srv.Connections() != 1 {
			t.Fatalf("connections = %d, want 1 (pooled)", srv.Connections())
		}
	})
	t.Run("implicit tls abort is prompt", func(t *testing.T) {
		srv := smtptest.New(t, smtptest.WithImplicitTLS())
		srv.SlowDelay = 5 * time.Second
		tr := newTransport(t, srv, func(c *config.SMTP) { c.TLS, c.InsecureSkipVerify = "tls", true })
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		start := time.Now()
		res := tr.Send(ctx, msg("slow@example.org"))
		if res.Class != transport.ClassUnknown || time.Since(start) > 3*time.Second {
			t.Fatalf("got %+v after %s", res, time.Since(start))
		}
	})
}

func TestSaturatedPoolIsBusyNotAFailure(t *testing.T) {
	srv := smtptest.New(t)
	srv.SlowDelay = 700 * time.Millisecond
	tr := newTransport(t, srv, func(c *config.SMTP) { c.MaxConnections = 1 })

	hold := make(chan transport.Result, 1)
	go func() { hold <- tr.Send(context.Background(), msg("slow@example.org")) }()
	time.Sleep(150 * time.Millisecond) // let it take the only connection

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if res := tr.Send(ctx, msg("user@example.org")); res.Class != transport.ClassBusy {
		t.Fatalf("waiting for a free connection must be busy (no attempt consumed), got %+v", res)
	}
	if res := <-hold; !res.OK() {
		t.Fatalf("the in-flight send was disturbed: %+v", res)
	}
}
