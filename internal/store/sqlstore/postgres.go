package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

var postgresDialect = dialect{
	name:       "postgres",
	nowExpr:    "((extract(epoch from clock_timestamp()) * 1000000)::bigint)",
	skipLocked: "FOR UPDATE SKIP LOCKED",
	postgres:   true,
	retryable: func(err error) bool {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// serialization_failure, deadlock_detected
			return pgErr.Code == "40001" || pgErr.Code == "40P01"
		}
		return false
	},
}

// OpenPostgres opens a PostgreSQL store suitable for multiple replicas.
func OpenPostgres(ctx context.Context, dsn string, maxConns int) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if maxConns < 1 {
		maxConns = 20
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)

	s := &Store{db: db, d: postgresDialect}
	if err := db.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return s, nil
}
