package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/skylarng89/smtp-handler/internal/store"
)

// Enqueue stores a message and its idempotency record atomically.
//
// The idempotency primary key is the cross-replica lock: a concurrent
// request with the same key blocks on the unique index until the first
// transaction commits, then observes the committed row and replays it.
func (s *Store) Enqueue(ctx context.Context, m store.NewMessage, idem *store.Idempotency) (store.EnqueueResult, error) {
	var result store.EnqueueResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		result = store.EnqueueResult{ID: m.ID}

		if idem != nil {
			// An expired record is treated as absent.
			if _, err := tx.ExecContext(ctx, s.q(
				`DELETE FROM idempotency WHERE project_id = ? AND idem_key = ? AND expires_at < {now}`),
				m.ProjectID, idem.Key); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, s.q(
				`INSERT INTO idempotency (project_id, idem_key, fingerprint, message_id, expires_at)
				 VALUES (?, ?, ?, ?, {now} + ?)
				 ON CONFLICT (project_id, idem_key) DO NOTHING`),
				m.ProjectID, idem.Key, idem.Fingerprint, m.ID, micros(idem.TTL))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				var fingerprint, existing string
				err := tx.QueryRowContext(ctx, s.q(
					`SELECT fingerprint, message_id FROM idempotency WHERE project_id = ? AND idem_key = ?`),
					m.ProjectID, idem.Key).Scan(&fingerprint, &existing)
				if err != nil {
					return err
				}
				if fingerprint != idem.Fingerprint {
					return store.ErrIdempotencyMismatch
				}
				result = store.EnqueueResult{ID: existing, Replayed: true}
				return nil
			}
		}

		_, err := tx.ExecContext(ctx, s.q(
			`INSERT INTO messages (id, project_id, template, key_id, state, version, attempt, max_attempts,
				max_age, next_attempt_at, deadline_at, message_id_hdr, subject, from_addr, to_addrs, payload,
				created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'queued', 1, 0, ?, ?, {now}, {now} + ?, ?, ?, ?, ?, ?, {now}, {now})`),
			m.ID, m.ProjectID, m.Template, m.KeyID, m.MaxAttempts, micros(m.MaxAge), micros(m.MaxAge),
			m.MessageIDHeader, clip(m.Subject), clip(m.From), clip(m.To), string(m.Payload))
		return err
	})
	if err != nil {
		return store.EnqueueResult{}, err
	}
	return result, nil
}

func (s *Store) FindIdempotent(ctx context.Context, projectID, key, fingerprint string) (string, error) {
	var stored, id string
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT fingerprint, message_id FROM idempotency
		 WHERE project_id = ? AND idem_key = ? AND expires_at >= {now}`),
		projectID, key).Scan(&stored, &id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", store.ErrNotFound
	case err != nil:
		return "", err
	case stored != fingerprint:
		return "", store.ErrIdempotencyMismatch
	}
	return id, nil
}

// Claim leases due jobs to owner in one transaction.
func (s *Store) Claim(ctx context.Context, owner string, limit int, lease time.Duration, exclude []string) ([]store.Job, error) {
	if limit < 1 {
		return nil, nil
	}
	var jobs []store.Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		jobs = jobs[:0]

		// A message whose lease expired after its final attempt has either
		// crashed the worker every time or been abandoned: fail it rather
		// than looping forever.
		if _, err := tx.ExecContext(ctx, s.q(
			`UPDATE messages SET state = 'failed', last_error_class = 'lease_expired',
				last_error = 'worker lease expired during the final attempt',
				lease_owner = '', lease_token = '', lease_until = 0,
				version = version + 1, updated_at = {now}
			 WHERE state = 'sending' AND lease_until < {now} AND attempt >= max_attempts`)); err != nil {
			return err
		}

		token := newToken()
		args := []any{owner, token, micros(lease)}
		var filter strings.Builder
		if len(exclude) > 0 {
			filter.WriteString(" AND project_id NOT IN (?" + strings.Repeat(",?", len(exclude)-1) + ")")
			for _, p := range exclude {
				args = append(args, p)
			}
		}
		args = append(args, limit)

		rows, err := tx.QueryContext(ctx, s.q(
			`UPDATE messages SET state = 'sending', attempt = attempt + 1, lease_owner = ?, lease_token = ?,
				lease_until = {now} + ?, version = version + 1, updated_at = {now}
			 WHERE id IN (
				SELECT id FROM messages
				WHERE ((state IN ('queued', 'retry_scheduled') AND next_attempt_at <= {now})
					OR (state = 'sending' AND lease_until < {now}))`+filter.String()+`
				ORDER BY next_attempt_at, id
				LIMIT ? {skip})
			 RETURNING `+messageColumns+`, payload`), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var payload string
			m, err := scanMessage(rows, &payload)
			if err != nil {
				return err
			}
			jobs = append(jobs, store.Job{Message: m, Payload: []byte(payload), LeaseToken: token})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) RenewLease(ctx context.Context, id, token string, lease time.Duration) error {
	res, err := s.db.ExecContext(ctx, s.q(
		`UPDATE messages SET lease_until = {now} + ? WHERE id = ? AND lease_token = ? AND state = 'sending'`),
		micros(lease), id, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrLeaseLost
	}
	return nil
}

const clearLease = `lease_owner = '', lease_token = '', lease_until = 0, version = version + 1, updated_at = {now}`

func (s *Store) MarkSent(ctx context.Context, id, token string, a store.Attempt, dropBody bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		query := `UPDATE messages SET state = 'sent', sent_at = {now}, last_error_class = '', last_error_code = 0,
			last_error = '', ` + clearLease
		if dropBody {
			query += `, payload = '', body_dropped = 1`
		}
		res, err := tx.ExecContext(ctx, s.q(query+` WHERE id = ? AND lease_token = ? AND state = 'sending'`), id, token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrLeaseLost
		}
		return s.insertAttempt(ctx, tx, id, a)
	})
}

func (s *Store) MarkRetry(ctx context.Context, id, token string, delay time.Duration, a store.Attempt) (store.State, error) {
	var next store.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, s.q(
			`UPDATE messages SET
				state = CASE WHEN attempt >= max_attempts OR {now} + ? > deadline_at THEN 'failed' ELSE 'retry_scheduled' END,
				next_attempt_at = {now} + ?,
				last_error_class = ?, last_error_code = ?, last_error = ?, `+clearLease+`
			 WHERE id = ? AND lease_token = ? AND state = 'sending'
			 RETURNING state`),
			micros(delay), micros(delay), a.Outcome, a.SMTPCode, clip(a.Error), id, token).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		next = store.State(state)
		return s.insertAttempt(ctx, tx, id, a)
	})
	return next, err
}

func (s *Store) MarkFailed(ctx context.Context, id, token string, a store.Attempt) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(
			`UPDATE messages SET state = 'failed', last_error_class = ?, last_error_code = ?, last_error = ?, `+clearLease+`
			 WHERE id = ? AND lease_token = ? AND state = 'sending'`),
			a.Outcome, a.SMTPCode, clip(a.Error), id, token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrLeaseLost
		}
		return s.insertAttempt(ctx, tx, id, a)
	})
}

func (s *Store) Release(ctx context.Context, id, token string, delay time.Duration, a *store.Attempt) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		set := `state = 'retry_scheduled', attempt = attempt - 1, next_attempt_at = {now} + ?, `
		args := []any{micros(delay)}
		if a != nil {
			// Record why the job was deferred so operators can see it.
			set += `last_error_class = ?, last_error_code = ?, last_error = ?, `
			args = append(args, a.Outcome, a.SMTPCode, clip(a.Error))
		}
		args = append(args, id, token)

		res, err := tx.ExecContext(ctx, s.q(
			`UPDATE messages SET `+set+clearLease+` WHERE id = ? AND lease_token = ? AND state = 'sending'`), args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrLeaseLost
		}
		if a != nil {
			return s.insertAttempt(ctx, tx, id, *a)
		}
		return nil
	})
}

func (s *Store) ReleaseOwner(ctx context.Context, owner string) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(
		`UPDATE messages SET state = 'retry_scheduled', attempt = attempt - 1, next_attempt_at = {now}, `+clearLease+`
		 WHERE lease_owner = ? AND state = 'sending'`), owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) insertAttempt(ctx context.Context, tx *sql.Tx, id string, a store.Attempt) error {
	_, err := tx.ExecContext(ctx, s.q(
		`INSERT INTO attempts (message_id, attempt, node, started_at, finished_at, outcome, smtp_code, enhanced_code, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`),
		id, a.Attempt, a.Node, a.StartedAt.UnixMicro(), a.FinishedAt.UnixMicro(), a.Outcome,
		a.SMTPCode, a.EnhancedCode, clip(a.Error))
	return err
}
