// Package multidbtest tests several databases at once: routing, read
// replicas, cross-database relations, transactions, raw SQL and the
// supervised pools. Its models are its own, so every Databases it opens
// declares the names they use: main (the default), analytics, replica,
// crm and archive.
//
// SQLite by default; ORMTEST_DRIVER=postgres|mysql with ORMTEST_DSN runs
// each database as a schema (Postgres) or database (MySQL) of its own on
// that server. The outage tests need MULTIDB_PG_DSN (Postgres, behind a
// proxy they cut) and MULTIDB_MYSQLD (a mysqld binary they start and
// kill, with MULTIDB_MYSQLD_DIR for its data).
package multidbtest

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	_ "github.com/vuka-lang/orm/v2/mysql"
	_ "github.com/vuka-lang/orm/v2/postgres"
	_ "github.com/vuka-lang/orm/v2/sqlite"
)

type Article struct {
	orm.Model[Article]
	ID    int64
	Title string
}

type Event struct {
	orm.Model[Event]
	ID   int64
	Kind string
}

func (Event) Meta() orm.Meta { return orm.Meta{DB: "analytics"} }

type Note struct {
	orm.Model[Note]
	ID   int64
	Body string
}

func (Note) Meta() orm.Meta { return orm.Meta{Read: "replica", Write: "main"} }

type Ledger struct {
	orm.Model[Ledger]
	ID     int64
	Amount int
}

func (Ledger) Meta() orm.Meta { return orm.Meta{Mirror: "archive"} }

type Customer struct {
	orm.Model[Customer]
	ID   int64
	Name string
}

func (Customer) Meta() orm.Meta { return orm.Meta{DB: "crm"} }

type Order struct {
	orm.Model[Order]
	ID         int64
	Total      int
	CustomerID int64
	Customer   *Customer `orm:"crossdb"`
}

type Account struct {
	orm.Model[Account]
	ID      int64
	Balance int
}

var (
	Articles  = orm.For[Article]()
	Events    = orm.For[Event]()
	Notes     = orm.For[Note]()
	Ledgers   = orm.For[Ledger]()
	Customers = orm.For[Customer]()
	Orders    = orm.For[Order]()
	Accounts  = orm.For[Account]()
)

func driver() string { return cmp.Or(os.Getenv("ORMTEST_DRIVER"), "sqlite") }

func uniq(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// dbURL is a fresh database for name: a SQLite file, or a schema or
// database of its own on ORMTEST_DSN's server.
func dbURL(t testing.TB, name string) string {
	t.Helper()
	dsn := os.Getenv("ORMTEST_DSN")
	switch driver() {
	case "postgres":
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := uniq("md_" + name + "_")
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			admin.Close()
		})
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	case "mysql":
		admin, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db := uniq("md_" + name + "_")
		if _, err := admin.Exec("CREATE DATABASE " + db); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec("DROP DATABASE " + db)
			admin.Close()
		})
		base, params, _ := strings.Cut(dsn[strings.LastIndexByte(dsn, '/')+1:], "?")
		_ = base
		u := "mysql://" + dsn[:strings.LastIndexByte(dsn, '/')+1] + db
		if params != "" {
			u += "?" + params
		}
		return u
	}
	return "sqlite:" + filepath.Join(t.TempDir(), name+".db") + "?_pragma=busy_timeout(5000)"
}

// names is every database the package's models name.
var names = []string{"main", "analytics", "replica", "crm", "archive"}

// open opens the package's databases, each fresh, then opts (a Declare
// of one of them replaces it), and makes models' tables on the databases
// tables names for each. It closes them when the test ends.
func open(t testing.TB, tables map[string][]orm.AnyManager, opts ...orm.OpenOption) (*orm.Databases, context.Context) {
	t.Helper()
	var all []orm.OpenOption
	for _, n := range names {
		var o []orm.DatabaseOption
		if n == "main" {
			o = append(o, orm.AsDefault())
		}
		all = append(all, orm.Declare(n, dbURL(t, n), o...))
	}
	s, err := orm.OpenDatabases(context.Background(), append(all, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	ctx := orm.WithDatabases(context.Background(), s)
	for name, ms := range tables {
		if err := orm.CreateTables(orm.Using(ctx, name), ms...); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx
}

// count is how many rows of m the database name holds.
func count[T any](t testing.TB, ctx context.Context, name string, m *orm.Manager[T]) int64 {
	t.Helper()
	n, err := m.Count(orm.Using(ctx, name))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// proxy is a TCP forwarder a test can cut: the database behind it goes
// away (refused connections, reset ones) and comes back.
type proxy struct {
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]bool
	opened int
}

func newProxy(t testing.TB, target string) *proxy {
	t.Helper()
	p := &proxy{target: target, conns: map[net.Conn]bool{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.cut)
	return p
}

func (p *proxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns[c], p.conns[up] = true, true
			p.opened++
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); up.Close(); c.Close() }()
			go func() { _, _ = io.Copy(c, up); up.Close(); c.Close() }()
		}
	}()
}

// drop resets every connection through the proxy; new ones still pass.
func (p *proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		c.Close()
		delete(p.conns, c)
	}
}

// cut stops the proxy: connections reset, new ones refused.
func (p *proxy) cut() {
	p.mu.Lock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	p.mu.Unlock()
	p.drop()
}

// restore listens again on the proxy's address.
func (p *proxy) restore(t testing.TB) {
	t.Helper()
	var ln net.Listener
	var err error
	for range 50 {
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.serve(ln)
}

func (p *proxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opened
}

// eventually waits up to d for ok.
func eventually(t testing.TB, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not %s within %v", what, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
