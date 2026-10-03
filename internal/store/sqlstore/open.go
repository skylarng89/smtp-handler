package sqlstore

import (
	"context"
	"fmt"

	"github.com/skylarng89/smtp-handler/internal/config"
)

// Open opens the store selected by configuration and, when autoMigrate is
// set, applies pending migrations.
func Open(ctx context.Context, c config.Store) (*Store, error) {
	var (
		s   *Store
		err error
	)
	switch c.Driver {
	case "sqlite":
		s, err = OpenSQLite(ctx, c.DSN)
	case "postgres":
		s, err = OpenPostgres(ctx, c.DSN, c.MaxOpenConns)
	default:
		return nil, fmt.Errorf("unknown store driver %q", c.Driver)
	}
	if err != nil {
		return nil, err
	}
	if c.AutoMigrate != nil && *c.AutoMigrate {
		if err := s.Migrate(ctx); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return s, nil
}
