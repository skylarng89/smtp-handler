// Package smtptest provides an in-process SMTP server for tests. Its
// behaviour is driven by the recipient's local part:
//
//	tempfail@  RCPT answers 451 (transient failure)
//	permfail@  RCPT answers 550 (permanent failure)
//	dropdata@  the connection is cut after DATA without a reply (unknown outcome)
//	slow@      DATA stalls for Server.SlowDelay
package smtptest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Received is one accepted message.
type Received struct {
	From string
	To   []string
	Data string
}

// Server is a running fake SMTP server.
type Server struct {
	Host      string
	Port      int
	SlowDelay time.Duration

	user, pass string
	startTLS   bool
	implicit   bool

	srv *smtp.Server

	mu       sync.Mutex
	received []Received

	conns      atomic.Int32 // total connections accepted
	concurrent atomic.Int32
	peak       atomic.Int32
	authFails  atomic.Int32
}

type Option func(*Server)

// WithAuth requires AUTH PLAIN with the given credentials.
func WithAuth(user, pass string) Option {
	return func(s *Server) { s.user, s.pass = user, pass }
}

// WithSTARTTLS advertises STARTTLS using a throwaway self-signed certificate.
func WithSTARTTLS() Option { return func(s *Server) { s.startTLS = true } }

// WithImplicitTLS serves TLS from the first byte (SMTPS) with a throwaway
// self-signed certificate.
func WithImplicitTLS() Option { return func(s *Server) { s.implicit = true } }

func selfSignedConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smtptest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// New starts a server on a random local port and stops it at test cleanup.
func New(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	s := &Server{SlowDelay: 3 * time.Second}
	for _, o := range opts {
		o(s)
	}

	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	s.Host, s.Port = addr.IP.String(), addr.Port

	s.srv = smtp.NewServer(backend{s})
	s.srv.Domain = "smtptest.local"
	s.srv.AllowInsecureAuth = true
	s.srv.ReadTimeout = 10 * time.Second
	s.srv.WriteTimeout = 10 * time.Second
	s.srv.MaxMessageBytes = 30 << 20
	if s.startTLS || s.implicit {
		cfg, err := selfSignedConfig()
		if err != nil {
			tb.Fatal(err)
		}
		if s.startTLS {
			s.srv.TLSConfig = cfg
		}
		if s.implicit {
			l = tls.NewListener(l, cfg)
		}
	}

	go func() {
		if err := s.srv.Serve(l); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			tb.Logf("smtptest serve: %v", err)
		}
	}()
	tb.Cleanup(func() { _ = s.srv.Close() })
	return s
}

// Received returns a snapshot of accepted messages.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

func (s *Server) Connections() int    { return int(s.conns.Load()) }
func (s *Server) PeakConcurrent() int { return int(s.peak.Load()) }
func (s *Server) AuthFailures() int   { return int(s.authFails.Load()) }

// WaitFor polls until n messages arrived or the timeout elapses.
func (s *Server) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(s.Received()) >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return len(s.Received()) >= n
}

type backend struct{ s *Server }

func (b backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	b.s.conns.Add(1)
	cur := b.s.concurrent.Add(1)
	for {
		peak := b.s.peak.Load()
		if cur <= peak || b.s.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	return &session{s: b.s, conn: c, authed: b.s.user == ""}, nil
}

type session struct {
	s      *Server
	conn   *smtp.Conn
	authed bool
	from   string
	to     []string
}

func (ss *session) AuthMechanisms() []string {
	if ss.s.user == "" {
		return nil
	}
	return []string{sasl.Plain}
}

func (ss *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username != ss.s.user || password != ss.s.pass {
			ss.s.authFails.Add(1)
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "authentication failed"}
		}
		ss.authed = true
		return nil
	}), nil
}

func (ss *session) Reset() { ss.from, ss.to = "", nil }

func (ss *session) Logout() error {
	ss.s.concurrent.Add(-1)
	return nil
}

func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	if !ss.authed {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "authentication required"}
	}
	ss.from = from
	return nil
}

func (ss *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	local, _, _ := strings.Cut(to, "@")
	switch local {
	case "tempfail":
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 2, 0}, Message: "mailbox busy, try later"}
	case "permfail":
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	}
	ss.to = append(ss.to, to)
	return nil
}

func (ss *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	for _, to := range ss.to {
		switch local, _, _ := strings.Cut(to, "@"); local {
		case "dropdata":
			_ = ss.conn.Conn().Close()
			return nil
		case "slow":
			time.Sleep(ss.s.SlowDelay)
		}
	}
	ss.s.mu.Lock()
	ss.s.received = append(ss.s.received, Received{From: ss.from, To: append([]string(nil), ss.to...), Data: string(raw)})
	ss.s.mu.Unlock()
	return nil
}
