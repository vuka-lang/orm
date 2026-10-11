package orm

import (
	"os"
	"path/filepath"
	"testing"
)

// Connect and a declared database's pool (openPool) both make a SQLite
// file's directory.
func TestSQLiteDir(t *testing.T) {
	for dsn, want := range map[string]string{
		"data/app.db":                   "data",
		"data/app.db?_pragma=x(1)&a=b":  "data",
		"file:data/app.db?cache=shared": "data",
		"/abs/dir/app.db":               "/abs/dir",
		"app.db":                        "",
		":memory:":                      "",
		"":                              "",
		"file:data/x.db?mode=memory":    "",
		"file:data/x.db?mode=ro":        "",
	} {
		if got := sqliteDir(dsn); got != want {
			t.Errorf("sqliteDir(%q) = %q, want %q", dsn, got, want)
		}
	}
	dir := t.TempDir()
	sup := newSupervisor("x", nil, Pool{}.withDefaults(), nil)
	db, err := openPool(sup, "sqlite", filepath.Join(dir, "declared", "app.db"), databaseConfig{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(dir, "declared")); err != nil {
		t.Errorf("openPool didn't make the directory: %v", err)
	}
	if err := makeSQLiteDir("postgres", "data/x"); err != nil {
		t.Error(err)
	}
}
