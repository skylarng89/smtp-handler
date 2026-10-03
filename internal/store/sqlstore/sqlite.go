package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

var sqliteDialect = dialect{
	name:    "sqlite",
	nowExpr: "(CAST(unixepoch('subsec') * 1000000 AS INTEGER))",
	retryable: func(err error) bool {
		msg := err.Error()
		return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
	},
}

// OpenSQLite opens (creating if needed) a single-node SQLite store.
//
// SQLite cannot be shared by several processes safely over WAL on network
// filesystems, so an exclusive lock file refuses a second instance outright.
// Within the process a single connection serializes all access, which keeps
// the claim logic free of SQLITE_BUSY races.
func OpenSQLite(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite: empty path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("sqlite: create data directory: %w", err)
		}
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}

	dsn := "file:" + escapeSQLitePath(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, d: sqliteDialect, closers: []func() error{unlock}}
	if err := db.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return s, nil
}

func escapeSQLitePath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
}
