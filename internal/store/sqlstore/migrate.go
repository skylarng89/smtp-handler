package sqlstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey namespaces the PostgreSQL advisory locks used by this
// service ("smtph" in ASCII-ish hex).
const (
	migrationLockKey = 0x736d747068001
	purgeLockKey     = 0x736d747068002
)

type migration struct {
	version    int64
	name       string
	statements []string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected <version>_<name>.sql", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		raw, err := migrationFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, statements: splitStatements(string(raw))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// splitStatements splits on semicolons. Migrations are plain DDL, so no
// semicolons appear inside string literals.
func splitStatements(sql string) []string {
	var out []string
	for _, part := range strings.Split(sql, ";") {
		if stmt := strings.TrimSpace(part); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func latestVersion() (int64, error) {
	ms, err := loadMigrations()
	if err != nil || len(ms) == 0 {
		return 0, fmt.Errorf("no migrations embedded: %w", err)
	}
	return ms[len(ms)-1].version, nil
}

// Migrate applies pending migrations. It is idempotent and safe to run from
// several replicas at once: under PostgreSQL a session-level advisory lock
// serializes them, and each migration is applied in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if s.d.postgres {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(migrationLockKey)); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", int64(migrationLockKey))
		}()
	}

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int64]bool{}
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, m := range ms {
		if applied[m.version] {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range m.statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.q("INSERT INTO schema_migrations (version, applied_at) VALUES (?, {now})"), m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: record: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: commit: %w", m.name, err)
		}
	}
	return nil
}

// SchemaOK verifies the database schema is exactly what this binary expects,
// so a replica from an older or newer release never runs against it.
func (s *Store) SchemaOK(ctx context.Context) error {
	want, err := latestVersion()
	if err != nil {
		return err
	}
	var have int64
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&have)
	if err != nil {
		return fmt.Errorf("read schema version (has `smtp-handler migrate` run?): %w", err)
	}
	if have != want {
		return fmt.Errorf("database schema is at version %d but this binary expects %d; run `smtp-handler migrate`", have, want)
	}
	return nil
}
