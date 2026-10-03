package sqlstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/skylarng89/smtp-handler/internal/store"
	"github.com/skylarng89/smtp-handler/internal/store/sqlstore"
	"github.com/skylarng89/smtp-handler/internal/store/storetest"
)

func TestSQLiteContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlstore.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "q.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSQLiteSingleInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	first, err := sqlstore.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlstore.OpenSQLite(context.Background(), path); err == nil {
		t.Fatal("a second instance opened the same SQLite database")
	}
	_ = first.Close()
	again, err := sqlstore.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("lock was not released on close: %v", err)
	}
	_ = again.Close()
}

// TestPostgresContract runs against a real server when
// SMTPH_TEST_POSTGRES_DSN is set (CI provides one). Each subtest gets its
// own schema so they can run in parallel.
func TestPostgresContract(t *testing.T) {
	dsn := os.Getenv("SMTPH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SMTPH_TEST_POSTGRES_DSN not set")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		return openIsolatedPostgres(t, dsn)
	})
}
