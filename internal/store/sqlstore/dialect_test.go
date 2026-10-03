package sqlstore

import "testing"

func TestPlaceholderRewriting(t *testing.T) {
	pg := &Store{d: postgresDialect}
	got := pg.q("UPDATE t SET a = ?, b = {now} + ? WHERE id = ? AND x IN (?,?) LIMIT ? {skip}")
	want := "UPDATE t SET a = $1, b = ((extract(epoch from clock_timestamp()) * 1000000)::bigint) + $2 " +
		"WHERE id = $3 AND x IN ($4,$5) LIMIT $6 FOR UPDATE SKIP LOCKED"
	if got != want {
		t.Fatalf("postgres rewrite:\n got %s\nwant %s", got, want)
	}

	lite := &Store{d: sqliteDialect}
	got = lite.q("SELECT ? WHERE t < {now} {skip}")
	want = "SELECT ? WHERE t < (CAST(unixepoch('subsec') * 1000000 AS INTEGER)) "
	if got != want {
		t.Fatalf("sqlite rewrite:\n got %q\nwant %q", got, want)
	}
}

func TestMigrationsLoad(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil || len(ms) == 0 {
		t.Fatalf("loadMigrations: %v (%d)", err, len(ms))
	}
	for _, m := range ms {
		if len(m.statements) == 0 {
			t.Fatalf("migration %s has no statements", m.name)
		}
	}
}
