package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/store"
)

func (s *Store) Get(ctx context.Context, id string) (*store.Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+messageColumns+` FROM messages WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) GetPayload(ctx context.Context, id string) ([]byte, error) {
	var (
		payload string
		dropped int
	)
	err := s.db.QueryRowContext(ctx, s.q(`SELECT payload, body_dropped FROM messages WHERE id = ?`), id).Scan(&payload, &dropped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if dropped != 0 || payload == "" {
		return nil, store.ErrPayloadGone
	}
	return []byte(payload), nil
}

func (s *Store) Attempts(ctx context.Context, id string) ([]store.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		`SELECT attempt, node, started_at, finished_at, outcome, smtp_code, enhanced_code, error
		 FROM attempts WHERE message_id = ? ORDER BY started_at, attempt`), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.Attempt
	for rows.Next() {
		var (
			a                 store.Attempt
			started, finished int64
		)
		if err := rows.Scan(&a.Attempt, &a.Node, &started, &finished, &a.Outcome, &a.SMTPCode, &a.EnhancedCode, &a.Error); err != nil {
			return nil, err
		}
		a.MessageID = id
		a.StartedAt, a.FinishedAt = fromMicros(started), fromMicros(finished)
		out = append(out, a)
	}
	return out, rows.Err()
}

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Store) List(ctx context.Context, f store.ListFilter) ([]store.Message, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)

	var (
		where []string
		args  []any
	)
	if f.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN (?"+strings.Repeat(",?", len(f.States)-1)+")")
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		pattern := "%" + likeEscaper.Replace(strings.ToLower(q)) + "%"
		where = append(where,
			`(LOWER(subject) LIKE ? ESCAPE '\' OR LOWER(to_addrs) LIKE ? ESCAPE '\' OR LOWER(from_addr) LIKE ? ESCAPE '\' OR id = ? OR message_id_hdr LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern, strings.ToUpper(q), pattern)
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}

	query := `SELECT ` + messageColumns + ` FROM messages`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]store.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context) ([]store.Count, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, state, COUNT(*) FROM messages GROUP BY project_id, state ORDER BY project_id, state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.Count
	for rows.Next() {
		var (
			c     store.Count
			state string
		)
		if err := rows.Scan(&c.ProjectID, &state, &c.Count); err != nil {
			return nil, err
		}
		c.State = store.State(state)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Retry and Cancel are single guarded UPDATEs: the allowed source states and
// the optimistic version live in the WHERE clause, so there is no
// check-then-act window between replicas, workers and operators.

func (s *Store) Retry(ctx context.Context, id string, version int64) (*store.Message, error) {
	return s.transition(ctx, id, version,
		`state = 'queued', attempt = 0, next_attempt_at = {now}, deadline_at = {now} + max_age,
		 last_error_class = '', last_error_code = 0, last_error = '', `+clearLease,
		[]store.State{store.StateFailed, store.StateCanceled, store.StateRetryScheduled},
		true)
}

func (s *Store) Cancel(ctx context.Context, id string, version int64) (*store.Message, error) {
	return s.transition(ctx, id, version,
		`state = 'canceled', `+clearLease,
		[]store.State{store.StateQueued, store.StateRetryScheduled, store.StateFailed},
		false)
}

func (s *Store) transition(ctx context.Context, id string, version int64, set string, from []store.State, needsBody bool) (*store.Message, error) {
	args := make([]any, 0, len(from)+3)
	args = append(args, id)
	for _, st := range from {
		args = append(args, string(st))
	}
	cond := `id = ? AND state IN (?` + strings.Repeat(",?", len(from)-1) + `)`
	if needsBody {
		cond += ` AND body_dropped = 0`
	}
	if version >= 0 {
		cond += ` AND version = ?`
		args = append(args, version)
	}

	m, err := scanMessage(s.db.QueryRowContext(ctx,
		s.q(`UPDATE messages SET `+set+` WHERE `+cond+` RETURNING `+messageColumns), args...))
	if err == nil {
		return &m, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Nothing matched: work out why so the caller can respond precisely.
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, st := range from {
		allowed = allowed || cur.State == st
	}
	switch {
	case !allowed:
		return nil, store.ErrConflict
	case needsBody && cur.BodyDropped:
		return nil, store.ErrPayloadGone
	case version >= 0 && cur.Version != version:
		return nil, store.ErrVersionMismatch
	default:
		// The row changed between the UPDATE and the diagnosis read.
		return nil, store.ErrConflict
	}
}
