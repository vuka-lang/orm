// Package ormtest opens a database for a test with the models' tables
// made: in-memory SQLite by default, or, with ORMTEST_DRIVER and
// ORMTEST_DSN set, a schema (Postgres) or database (MySQL) of its own on
// a real server, dropped when the test ends.
//
//	func TestSignup(t *testing.T) {
//		ctx := ormtest.Open(t, Users, Posts)
//		ormtest.Seed(t, ctx, Users, &User{Name: "Ali"})
//		…
//	}
//
// The program's tests import the driver of a real server they point it at
// (_ "github.com/vuka-lang/orm/postgres"); SQLite is linked here.
package ormtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/orm"
	_ "github.com/vuka-lang/orm/sqlite"
)

// Open is a context whose queries use a fresh database with the models'
// tables (and their many-to-many tables) made.
func Open(t testing.TB, models ...orm.AnyManager) context.Context {
	t.Helper()
	d := open(t)
	ctx := orm.WithDB(context.Background(), d)
	if err := orm.CreateTables(ctx, models...); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Driver is the database Open gives: "sqlite", or ORMTEST_DRIVER's.
func Driver() string {
	if d := os.Getenv("ORMTEST_DRIVER"); d != "" {
		return d
	}
	return "sqlite"
}

// Mirror opens a second database with the models' tables and has ctx's
// writes mirrored to it (orm.WithMirror), for testing a model moving
// between databases. It is in-memory SQLite, or ORMTEST_MIRROR_DRIVER's
// with ORMTEST_MIRROR_DSN: MySQL as Open's and Postgres as the mirror's
// is the move the ORM's mirror is for. The returned context reads the
// mirror, to check what reached it.
func Mirror(t testing.TB, ctx context.Context, models ...orm.AnyManager) (mirrored, onMirror context.Context) {
	t.Helper()
	driver := os.Getenv("ORMTEST_MIRROR_DRIVER")
	if driver == "" {
		driver = "sqlite"
	}
	d := openDriver(t, driver, os.Getenv("ORMTEST_MIRROR_DSN"))
	onMirror = orm.WithDB(context.Background(), d)
	if err := orm.CreateTables(onMirror, models...); err != nil {
		t.Fatal(err)
	}
	return orm.WithMirror(ctx, d), onMirror
}

func open(t testing.TB) *orm.DB {
	return openDriver(t, Driver(), os.Getenv("ORMTEST_DSN"))
}

func openDriver(t testing.TB, driver, dsn string) *orm.DB {
	switch driver {
	case "postgres":
		return openPostgres(t, dsn)
	case "mysql":
		return openMySQL(t, dsn)
	}
	d, err := orm.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SQL().Close() })
	return d
}

func name() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "orm_test_" + hex.EncodeToString(b)
}

// openPostgres makes a schema of the test's own and a pool whose
// connections search it, then public: an extension (a database-wide
// object) lives there, seen by every test's schema.
func openPostgres(t testing.TB, dsn string) *orm.DB {
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := name()
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := sql.Open("pgx", dsn+sep+"search_path="+schema+",public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	return orm.Open(s, "postgres")
}

// openMySQL makes a database of the test's own: the DSN's, renamed.
func openMySQL(t testing.TB, dsn string) *orm.DB {
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := name()
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	slash := strings.LastIndex(dsn, "/")
	rest := dsn[slash+1:]
	params := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		params = rest[i:]
	}
	if !strings.Contains(params, "parseTime") {
		if params == "" {
			params = "?parseTime=true"
		} else {
			params += "&parseTime=true"
		}
	}
	s, err := sql.Open("mysql", dsn[:slash+1]+dbName+params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = admin.Exec("DROP DATABASE " + dbName)
		_ = admin.Close()
	})
	return orm.Open(s, "mysql")
}

// Seed inserts rows, failing the test when it can't.
func Seed[T any](t testing.TB, ctx context.Context, m *orm.Manager[T], rows ...*T) {
	t.Helper()
	if err := m.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
}

// Exec runs SQL on ctx's database (test data the models can't write),
// failing the test when it can't.
func Exec(t testing.TB, ctx context.Context, query string, args ...any) {
	t.Helper()
	d, ok := orm.DBFrom(ctx)
	if !ok {
		t.Fatal("ormtest.Exec: ctx has no database: use the one Open returned")
	}
	if _, err := d.SQL().ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// CountQueries is ctx counting the statements its queries run (not those
// the ORM refused to send), and how many so far.
func CountQueries(ctx context.Context) (context.Context, func() int64) {
	var n atomic.Int64
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) {
		if !q.Refused {
			n.Add(1)
		}
	})
	return ctx, n.Load
}
