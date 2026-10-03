package sqlstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/store"
)

// Purge deletes expired data in bounded batches. Under PostgreSQL a
// session-level advisory lock elects a single purger across replicas; if
// another node holds it the call returns Skipped. Every statement is
// idempotent, so overlapping runs would still be harmless.
func (s *Store) Purge(ctx context.Context, rules []store.RetentionRule, def store.RetentionRule, batch int) (store.PurgeResult, error) {
	var res store.PurgeResult
	if batch < 1 {
		batch = 1000
	}

	if s.d.postgres {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return res, err
		}
		defer func() { _ = conn.Close() }()
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", int64(purgeLockKey)).Scan(&locked); err != nil {
			return res, err
		}
		if !locked {
			res.Skipped = true
			return res, nil
		}
		defer func() {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", int64(purgeLockKey))
		}()
	}

	known := make([]string, 0, len(rules))
	for _, r := range rules {
		known = append(known, r.ProjectID)
		n, err := s.purgeProject(ctx, r, "project_id = ?", []any{r.ProjectID}, batch)
		res.Messages += n
		if err != nil {
			return res, err
		}
	}

	// Messages of projects that no longer exist in config use the defaults.
	where, args := "1 = 1", []any(nil)
	if len(known) > 0 {
		where = "project_id NOT IN (?" + strings.Repeat(",?", len(known)-1) + ")"
		for _, k := range known {
			args = append(args, k)
		}
	}
	n, err := s.purgeProject(ctx, def, where, args, batch)
	res.Messages += n
	if err != nil {
		return res, err
	}

	if res.Idempotency, err = s.deleteExpired(ctx, "idempotency"); err != nil {
		return res, err
	}
	if res.Counters, err = s.deleteExpired(ctx, "rate_counters"); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Store) purgeProject(ctx context.Context, r store.RetentionRule, scope string, scopeArgs []any, batch int) (int64, error) {
	var total int64
	sweep := func(cond string, ttlMicros int64) error {
		if ttlMicros <= 0 {
			return nil
		}
		for {
			args := append(append([]any{}, scopeArgs...), ttlMicros, batch)
			res, err := s.db.ExecContext(ctx, s.q(
				`DELETE FROM messages WHERE id IN (
					SELECT id FROM messages WHERE `+scope+` AND `+cond+` LIMIT ?)`), args...)
			if err != nil {
				return fmt.Errorf("purge messages: %w", err)
			}
			n, _ := res.RowsAffected()
			total += n
			if n < int64(batch) || ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	// Placeholders appear after scope args: cond uses one ? for the cutoff age.
	if err := sweep(`state = 'sent' AND sent_at < {now} - ?`, micros(r.Sent)); err != nil {
		return total, err
	}
	err := sweep(`state IN ('failed', 'canceled') AND updated_at < {now} - ?`, micros(r.Failed))
	return total, err
}

func (s *Store) deleteExpired(ctx context.Context, table string) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM `+table+` WHERE expires_at < {now}`))
	if err != nil {
		return 0, fmt.Errorf("purge %s: %w", table, err)
	}
	return res.RowsAffected()
}
