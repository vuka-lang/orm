package orm

import (
	"database/sql"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

// memDB is a fresh in-memory SQLite database for a test of the package's
// internals (which can't import orm/sqlite: it imports orm).
func memDB(t testing.TB) *DB {
	t.Helper()
	s, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = s.Close() })
	return Open(s, "sqlite")
}
