// Package sqlstore implements store.Store on top of database/sql for both
// SQLite (single node) and PostgreSQL (multi-node). The two drivers share
// one set of queries; a small dialect adapts placeholders, the database
// clock expression and row-locking syntax.
//
// All timestamps are BIGINT microseconds since the Unix epoch produced by
// the database clock ({now}), never the application clock, so replicas with
// skewed clocks still agree on lease expiry and schedules.
package sqlstore

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/skylarng89/smtp-handler/internal/store"
)

type dialect struct {
	name       string
	nowExpr    string // SQL expression for current DB time in microseconds
	skipLocked string // row-locking suffix for claim subqueries
	postgres   bool
	retryable  func(error) bool
}

// Store is the shared implementation.
type Store struct {
	db      *sql.DB
	d       dialect
	closers []func() error
}

var _ store.Store = (*Store)(nil)

func (s *Store) Driver() string { return s.d.name }

func (s *Store) Close() error {
	var errs []error
	errs = append(errs, s.db.Close())
	for _, c := range s.closers {
		errs = append(errs, c())
	}
	return errors.Join(errs...)
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// q expands {now} and, for PostgreSQL, rewrites ? placeholders to $n.
func (s *Store) q(query string) string {
	query = strings.ReplaceAll(query, "{now}", s.d.nowExpr)
	query = strings.ReplaceAll(query, "{skip}", s.d.skipLocked)
	if !s.d.postgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func micros(d time.Duration) int64 { return d.Microseconds() }

func fromMicros(us int64) time.Time { return time.UnixMicro(us).UTC() }

func optTime(us int64) *time.Time {
	if us == 0 {
		return nil
	}
	t := fromMicros(us)
	return &t
}

func newToken() string {
	var b [16]byte
	_, _ = cryptorand.Read(b[:])
	return hex.EncodeToString(b[:])
}

const maxTxRetries = 5

// tx runs fn in a transaction, retrying on serialization failures,
// deadlocks and SQLite busy errors with jittered backoff. fn may therefore
// run more than once and must be free of external side effects.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	var err error
	for attempt := range maxTxRetries {
		err = s.runTx(ctx, fn)
		if err == nil || !s.d.retryable(err) || ctx.Err() != nil {
			return err
		}
		backoff := time.Duration(5<<attempt) * time.Millisecond
		backoff += time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("transaction failed after %d attempts: %w", maxTxRetries, err)
}

func (s *Store) runTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

const messageColumns = `id, project_id, template, key_id, state, version, attempt, max_attempts,
	next_attempt_at, deadline_at, lease_owner, lease_until, message_id_hdr, subject, from_addr, to_addrs,
	body_dropped, last_error_class, last_error_code, last_error, created_at, updated_at, sent_at`

// scanMessage scans messageColumns, followed by any extra destinations.
func scanMessage(r scanner, extra ...any) (store.Message, error) {
	var (
		m                                                    store.Message
		state                                                string
		nextAt, deadline, leaseUntil, created, updated, sent int64
		dropped                                              int
	)
	dest := append([]any{&m.ID, &m.ProjectID, &m.Template, &m.KeyID, &state, &m.Version, &m.Attempt,
		&m.MaxAttempts, &nextAt, &deadline, &m.LeaseOwner, &leaseUntil, &m.MessageIDHeader, &m.Subject,
		&m.From, &m.To, &dropped, &m.LastErrorClass, &m.LastErrorCode, &m.LastError,
		&created, &updated, &sent}, extra...)
	if err := r.Scan(dest...); err != nil {
		return store.Message{}, err
	}
	m.State = store.State(state)
	m.NextAttemptAt = fromMicros(nextAt)
	m.DeadlineAt = fromMicros(deadline)
	m.LeaseUntil = optTime(leaseUntil)
	m.BodyDropped = dropped != 0
	m.CreatedAt = fromMicros(created)
	m.UpdatedAt = fromMicros(updated)
	m.SentAt = optTime(sent)
	return m, nil
}

const maxErrorLen = 1000

func clip(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorLen], "") + "…"
}
