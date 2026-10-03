package sqlstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skylarng89/smtp-handler/internal/store"
	"github.com/skylarng89/smtp-handler/internal/store/sqlstore"
)

var schemaSeq atomic.Int64

// openIsolatedPostgres creates a throwaway schema and points search_path at
// it, so parallel subtests never see each other's rows.
func openIsolatedPostgres(t *testing.T, dsn string) store.Store {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("smtph_t_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := sqlstore.OpenPostgres(ctx, strings.TrimSpace(u.String()), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
