package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	mail "github.com/wneessen/go-mail"
	"github.com/wneessen/go-mail/smtp"

	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
)

// SlotStore coordinates a cluster-wide cap on concurrent SMTP connections
// (providers limit connections per account, not per replica).
type SlotStore interface {
	AcquireSlot(ctx context.Context, projectID string, max int, owner string, ttl time.Duration) (int, bool, error)
	RenewSlot(ctx context.Context, projectID string, slot int, owner string, ttl time.Duration) (bool, error)
	ReleaseSlot(ctx context.Context, projectID string, slot int, owner string) error
}

const (
	slotTTL         = 2 * time.Minute
	slotWait        = 3 * time.Second
	closeGrace      = 2 * time.Second
	sendAbortWait   = 5 * time.Second
	minReapInterval = time.Second
)

var (
	errNoSlot   = errors.New("no SMTP connection slot available")
	errPoolBusy = errors.New("all SMTP connections on this node are busy")
)

type conn struct {
	sc       *smtp.Client
	raw      net.Conn // underlying socket, closable without go-mail's client lock
	slot     int
	lastUsed time.Time
}

// holderKey carries a *connHolder through the dial context so the custom
// dialer can hand the raw socket back to the caller.
type holderKey struct{}

type connHolder struct{ conn net.Conn }

// newDialFunc dials TCP (with an implicit-TLS handshake for tls mode) and
// publishes the raw socket. go-mail's own Close/UpdateDeadline take the
// client mutex, which an in-flight exchange holds while it waits for the
// server, so only closing the socket can abort a stalled send.
func newDialFunc(cfg config.SMTP, tlsCfg *tls.Config) mail.DialContextFunc {
	netDialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var (
			c   net.Conn
			raw net.Conn
			err error
		)
		if cfg.TLS == "tls" {
			tc, derr := (&tls.Dialer{NetDialer: netDialer, Config: tlsCfg.Clone()}).DialContext(ctx, network, addr)
			if derr != nil {
				return nil, derr
			}
			c, raw = tc, tc.(*tls.Conn).NetConn()
		} else if c, err = netDialer.DialContext(ctx, network, addr); err != nil {
			return nil, err
		} else {
			raw = c
		}
		if h, ok := ctx.Value(holderKey{}).(*connHolder); ok {
			h.conn = raw
		}
		return c, nil
	}
}

// SMTP is a pooled SMTP transport for one project. Each pooled connection is
// owned by exactly one goroutine at a time.
type SMTP struct {
	projectID string
	owner     string
	cfg       config.SMTP
	client    *mail.Client
	slots     SlotStore
	log       *slog.Logger

	sem  chan struct{} // bounds open connections on this node
	idle chan *conn

	closed   atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	reaperWG sync.WaitGroup
}

// NewSMTP builds the transport. slots may be nil when no global cap is set.
func NewSMTP(projectID, owner string, cfg config.SMTP, slots SlotStore, log *slog.Logger) (*SMTP, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	t := &SMTP{
		projectID: projectID, owner: owner, cfg: cfg, client: client, log: log,
		sem:  make(chan struct{}, cfg.MaxConnections),
		idle: make(chan *conn, cfg.MaxConnections),
		stop: make(chan struct{}),
	}
	if cfg.GlobalMaxConnections > 0 {
		t.slots = slots
	}
	t.reaperWG.Add(1)
	go t.reap()
	return t, nil
}

func newClient(cfg config.SMTP) (*mail.Client, error) {
	tlsCfg := &tls.Config{
		ServerName:         cfg.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicit operator opt-in
	}
	opts := []mail.Option{
		mail.WithPort(cfg.Port),
		mail.WithTimeout(cfg.Timeout.Std()),
		mail.WithTLSConfig(tlsCfg),
		mail.WithDialContextFunc(newDialFunc(cfg, tlsCfg)),
	}
	if cfg.HELO != "" {
		opts = append(opts, mail.WithHELO(cfg.HELO))
	}
	switch cfg.TLS {
	case "tls":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}

	if cfg.Username != "" && cfg.AuthMethod != "none" {
		auth := map[string]mail.SMTPAuthType{
			"auto": mail.SMTPAuthAutoDiscover, "plain": mail.SMTPAuthPlain,
			"login": mail.SMTPAuthLogin, "cram-md5": mail.SMTPAuthCramMD5,
		}[cfg.AuthMethod]
		if cfg.TLS == "tls" && cfg.AuthMethod == "auto" {
			// With a custom dialer go-mail cannot tell an implicit-TLS channel is
			// encrypted and would refuse PLAIN/LOGIN during discovery. PLAIN is
			// the mechanism every TLS-protected submission server must support.
			auth = mail.SMTPAuthPlain
		}
		opts = append(opts, mail.WithSMTPAuth(auth), mail.WithUsername(cfg.Username), mail.WithPassword(cfg.Password))
	}
	c, err := mail.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("smtp client for %s: %w", cfg.Host, err)
	}
	return c, nil
}

// Send delivers one message, reusing a pooled connection when possible.
func (t *SMTP) Send(ctx context.Context, m *message.Message) Result {
	msg, err := BuildMsg(m)
	if err != nil {
		return Result{Class: ClassPermanent, Detail: detail(err)}
	}

	c, err := t.acquire(ctx)
	if err != nil {
		if errors.Is(err, errNoSlot) || errors.Is(err, errPoolBusy) {
			return Result{Class: ClassBusy, Detail: err.Error()}
		}
		return ClassifyDial(err)
	}

	sendErr := t.sendOn(ctx, c, msg)
	res := ClassifySend(sendErr)
	if sendErr != nil && ctx.Err() != nil {
		// Cancelled mid-exchange: the server may or may not have the message.
		res = Result{Class: ClassUnknown, Detail: detail(sendErr)}
	}
	// A connection that just failed is not trusted for reuse.
	t.release(c, sendErr == nil)
	return res
}

// sendOn runs the SMTP exchange, aborting the connection if ctx ends first.
func (t *SMTP) sendOn(ctx context.Context, c *conn, msg *mail.Msg) error {
	done := make(chan error, 1)
	go func() { done <- t.client.SendWithSMTPClient(c.sc, msg) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.abort() // unblocks the in-flight exchange
		select {
		case err := <-done:
			return fmt.Errorf("send aborted: %w (%w)", ctx.Err(), err)
		case <-time.After(sendAbortWait):
			return fmt.Errorf("send aborted: %w", ctx.Err())
		}
	}
}

// abort kills the socket immediately, failing any pending read or write.
func (c *conn) abort() {
	if c.raw != nil {
		_ = c.raw.Close()
		return
	}
	go func() { _ = c.sc.Close() }()
}

func (t *SMTP) acquire(ctx context.Context) (*conn, error) {
	if t.closed.Load() {
		return nil, errors.New("transport is closed")
	}
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		// Nothing was attempted: waiting for a pooled connection is not a
		// delivery failure and must not consume one of the message's attempts.
		return nil, errPoolBusy
	}

	for {
		select {
		case c := <-t.idle:
			if t.usable(ctx, c) {
				return c, nil
			}
			t.discard(c)
			continue
		default:
		}
		break
	}

	c, err := t.dial(ctx)
	if err != nil {
		<-t.sem
		return nil, err
	}
	return c, nil
}

// usable validates an idle connection: not stale, slot still ours, and the
// server still answers NOOP.
func (t *SMTP) usable(ctx context.Context, c *conn) bool {
	if time.Since(c.lastUsed) > t.cfg.IdleTimeout.Std() {
		return false
	}
	if t.slots != nil {
		ok, err := t.slots.RenewSlot(ctx, t.projectID, c.slot, t.owner, slotTTL)
		if err != nil || !ok {
			return false
		}
	}
	return c.sc.Noop() == nil
}

func (t *SMTP) dial(ctx context.Context) (*conn, error) {
	c := &conn{slot: -1}
	if t.slots != nil {
		slot, err := t.waitForSlot(ctx)
		if err != nil {
			return nil, err
		}
		c.slot = slot
	}
	holder := &connHolder{}
	sc, err := t.client.DialToSMTPClientWithContext(context.WithValue(ctx, holderKey{}, holder))
	if err != nil {
		t.freeSlot(c)
		return nil, err
	}
	c.sc, c.raw, c.lastUsed = sc, holder.conn, time.Now()
	return c, nil
}

// waitForSlot polls briefly for a cluster-wide slot. Giving up returns
// errNoSlot, which the worker treats as "try again soon" without burning an
// attempt.
func (t *SMTP) waitForSlot(ctx context.Context) (int, error) {
	deadline := time.Now().Add(slotWait)
	for {
		slot, ok, err := t.slots.AcquireSlot(ctx, t.projectID, t.cfg.GlobalMaxConnections, t.owner, slotTTL)
		if err != nil {
			return 0, fmt.Errorf("acquire connection slot: %w", err)
		}
		if ok {
			return slot, nil
		}
		if time.Now().After(deadline) {
			return 0, errNoSlot
		}
		wait := 50*time.Millisecond + time.Duration(rand.Int64N(int64(100*time.Millisecond)))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (t *SMTP) freeSlot(c *conn) {
	if t.slots == nil || c.slot < 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeGrace)
	defer cancel()
	if err := t.slots.ReleaseSlot(ctx, t.projectID, c.slot, t.owner); err != nil {
		t.log.Warn("release smtp slot", "project", t.projectID, "error", err)
	}
}

// release returns a connection to the pool (if healthy) and frees the
// concurrency token taken by acquire.
func (t *SMTP) release(c *conn, healthy bool) {
	defer func() { <-t.sem }()
	if !healthy || t.closed.Load() {
		t.discard(c)
		return
	}
	c.lastUsed = time.Now()
	select {
	case t.idle <- c:
	default:
		t.discard(c)
	}
}

func (t *SMTP) discard(c *conn) {
	if c.sc != nil {
		done := make(chan struct{})
		go func() {
			_ = t.client.CloseWithSMTPClient(c.sc) // polite QUIT
			_ = c.sc.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeGrace):
			c.abort()
		}
	}
	t.freeSlot(c)
}

// reap closes connections that sat idle too long so slots and server
// resources are not held for nothing.
func (t *SMTP) reap() {
	defer t.reaperWG.Done()
	interval := max(t.cfg.IdleTimeout.Std()/2, minReapInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			for n := len(t.idle); n > 0; n-- {
				select {
				case c := <-t.idle:
					if time.Since(c.lastUsed) > t.cfg.IdleTimeout.Std() {
						t.discard(c)
						continue
					}
					select {
					case t.idle <- c:
					default:
						t.discard(c)
					}
				default:
				}
			}
		}
	}
}

// Close stops the reaper and closes all idle connections. In-flight sends
// finish normally and their connections are discarded on release.
func (t *SMTP) Close() error {
	t.stopOnce.Do(func() {
		t.closed.Store(true)
		close(t.stop)
		t.reaperWG.Wait()
		for {
			select {
			case c := <-t.idle:
				t.discard(c)
			default:
				return
			}
		}
	})
	return nil
}
