package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Incr atomically bumps a fixed-window counter. The window boundary is
// derived from the database clock, so every replica agrees on it.
func (s *Store) Incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	w := micros(window)
	var count, windowStart, now int64
	err := s.db.QueryRowContext(ctx, s.q(
		`INSERT INTO rate_counters (bucket, window_start, count, expires_at)
		 VALUES (?, ({now} / ?) * ?, 1, ({now} / ?) * ? + ?)
		 ON CONFLICT (bucket, window_start) DO UPDATE SET count = rate_counters.count + 1
		 RETURNING count, window_start, {now}`),
		key, w, w, w, w, w).Scan(&count, &windowStart, &now)
	if err != nil {
		return 0, 0, err
	}
	remaining := time.Duration(windowStart+w-now) * time.Microsecond
	return count, max(remaining, 0), nil
}

// AcquireSlot claims one of max global SMTP connection slots for a project.
// A slot whose lease expired (crashed replica) can be taken over.
func (s *Store) AcquireSlot(ctx context.Context, projectID string, max int, owner string, ttl time.Duration) (int, bool, error) {
	for slot := range max {
		var got int
		err := s.db.QueryRowContext(ctx, s.q(
			`INSERT INTO smtp_slots (project_id, slot, lease_owner, lease_until)
			 VALUES (?, ?, ?, {now} + ?)
			 ON CONFLICT (project_id, slot) DO UPDATE
			   SET lease_owner = excluded.lease_owner, lease_until = excluded.lease_until
			   WHERE smtp_slots.lease_until < {now}
			 RETURNING slot`),
			projectID, slot, owner, micros(ttl)).Scan(&got)
		switch {
		case err == nil:
			return got, true, nil
		case errors.Is(err, sql.ErrNoRows):
			continue // held by someone else
		default:
			return 0, false, err
		}
	}
	return 0, false, nil
}

func (s *Store) RenewSlot(ctx context.Context, projectID string, slot int, owner string, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(
		`UPDATE smtp_slots SET lease_until = {now} + ? WHERE project_id = ? AND slot = ? AND lease_owner = ?`),
		micros(ttl), projectID, slot, owner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) ReleaseSlot(ctx context.Context, projectID string, slot int, owner string) error {
	_, err := s.db.ExecContext(ctx, s.q(
		`UPDATE smtp_slots SET lease_until = 0 WHERE project_id = ? AND slot = ? AND lease_owner = ?`),
		projectID, slot, owner)
	return err
}
