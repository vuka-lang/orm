package orm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
)

// newApp is an app's databases: name → a fresh in-memory SQLite with the
// users table, rows[name] users in it.
func newApp(t *testing.T, rows map[string]int, def string) *orm.Databases {
	t.Helper()
	s := orm.NewDatabases()
	for name, n := range rows {
		d, err := orm.Connect("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.SQL().Close() })
		var opts []orm.DatabaseOption
		if name == def {
			opts = append(opts, orm.AsDefault())
		}
		s.Add(name, d, opts...)
		ctx := orm.WithDB(context.Background(), d)
		if err := orm.CreateTables(ctx, Users); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if err := Users.Create(ctx, &User{Name: "u", Email: strings.Repeat("x", i+1)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// TestTwoApps uses one manager for two apps in a process: each request's
// context carries its app's databases.
func TestTwoApps(t *testing.T) {
	one := newApp(t, map[string]int{"main": 1}, "main")
	two := newApp(t, map[string]int{"main": 2}, "main")
	for _, c := range []struct {
		app  *orm.Databases
		want int64
	}{{one, 1}, {two, 2}, {one, 1}} {
		n, err := Users.Count(orm.WithDatabases(context.Background(), c.app))
		if err != nil || n != c.want {
			t.Fatalf("count = %d, %v, want %d", n, err, c.want)
		}
	}
}

// TestOnlyDatabase: with no AsDefault, a model finds the only database,
// and none when there are two.
func TestOnlyDatabase(t *testing.T) {
	one := newApp(t, map[string]int{"main": 0}, "")
	if n, err := Users.Count(orm.WithDatabases(context.Background(), one)); err != nil || n != 0 {
		t.Fatalf("one database: %d, %v", n, err)
	}
	two := newApp(t, map[string]int{"main": 0, "other": 0}, "")
	if _, err := Users.Count(orm.WithDatabases(context.Background(), two)); err == nil || !strings.Contains(err.Error(), "no default database") {
		t.Fatalf("two databases: %v", err)
	}
	empty := orm.NewDatabases()
	if _, err := Users.Count(orm.WithDatabases(context.Background(), empty)); err == nil || !strings.Contains(err.Error(), "orm.AddDatabase") {
		t.Fatalf("no database: %v", err)
	}
}

// TestRoutePerApp: a table's route belongs to the Databases it is set on.
// Another app's databases, without it, query their own default.
func TestRoutePerApp(t *testing.T) {
	routed := newApp(t, map[string]int{"main": 0, "routed": 1}, "main")
	routed.Route("users", orm.Route{DB: "routed"})
	plain := newApp(t, map[string]int{"main": 2}, "main")
	for _, c := range []struct {
		app  *orm.Databases
		want int64
	}{{plain, 2}, {routed, 1}} {
		n, err := Users.Count(orm.WithDatabases(context.Background(), c.app))
		if err != nil || n != c.want {
			t.Fatalf("count = %d, %v, want %d", n, err, c.want)
		}
	}
	routed.Unroute("users")
	if n, _ := Users.Count(orm.WithDatabases(context.Background(), routed)); n != 0 {
		t.Fatalf("unrouted count = %d", n)
	}
}

func TestParseURL(t *testing.T) {
	for _, c := range []struct{ url, dialect, dsn string }{
		{"postgres://u:p@h:5432/db?sslmode=disable", "postgres", "postgres://u:p@h:5432/db?sslmode=disable"},
		{"postgresql://u@h/db", "postgres", "postgres://u@h/db"},
		{"mysql://u:p@tcp(h:3306)/db", "mysql", "u:p@tcp(h:3306)/db"},
		{"sqlite::memory:", "sqlite", ":memory:"},
		{"sqlite://shop.db", "sqlite", "shop.db"},
		{"file:shop.db?mode=ro", "sqlite", "file:shop.db?mode=ro"},
	} {
		d, dsn, err := orm.ParseURL(c.url)
		if err != nil || d != c.dialect || dsn != c.dsn {
			t.Errorf("%s = %s %s %v", c.url, d, dsn, err)
		}
	}
	if _, _, err := orm.ParseURL("redis://u:secret@h"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad scheme: %v", err)
	}
	if _, _, err := orm.ParseURL("nourl"); err == nil {
		t.Fatal("no scheme")
	}
}

// A MySQL DSN gets parseTime, and utf8mb4 in the server's default
// collation unless it names its own charset or collation.
func TestMySQLDSN(t *testing.T) {
	for _, c := range [][2]string{
		{"u:p@tcp(h:3306)/db", "u:p@tcp(h:3306)/db?parseTime=true&charset=utf8mb4"},
		{"u:p?w@tcp(h)/db?loc=UTC", "u:p?w@tcp(h)/db?loc=UTC&parseTime=true&charset=utf8mb4"},
		{"u@tcp(h)/db?collation=utf8mb4_unicode_ci", "u@tcp(h)/db?collation=utf8mb4_unicode_ci&parseTime=true"},
		{"u@tcp(h)/db?charset=latin1&parseTime=false", "u@tcp(h)/db?charset=latin1&parseTime=false"},
		{"/db", "/db?parseTime=true&charset=utf8mb4"},
	} {
		if got := orm.MySQLDSN(c[0]); got != c[1] {
			t.Errorf("MySQLDSN(%s) = %s, want %s", c[0], got, c[1])
		}
	}
}
