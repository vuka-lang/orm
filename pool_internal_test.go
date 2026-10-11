package orm

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestJitteredLifetime(t *testing.T) {
	d := time.Hour
	for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
		got := jitteredLifetime(d, r)
		if got > d || got < d*4/5 {
			t.Fatalf("lifetime %v for r=%v: out of [%v, %v]", got, r, d*4/5, d)
		}
	}
	if jitteredLifetime(d, 0) != d || jitteredLifetime(d, 0.5) != d*9/10 {
		t.Fatal("jitter isn't linear in r")
	}
}

func TestBreaker(t *testing.T) {
	b := breaker{cfg: Breaker{Failures: 3, Cooldown: 30 * time.Millisecond}}
	for i := range 2 {
		if b.failure() || !b.allow() {
			t.Fatalf("open after %d failures", i+1)
		}
	}
	if !b.failure() || b.allow() {
		t.Fatal("not open at the threshold")
	}
	if st, n := b.status(); st != "open" || n != 3 {
		t.Fatalf("status %s %d", st, n)
	}
	time.Sleep(40 * time.Millisecond)
	if !b.allow() || b.allow() {
		t.Fatal("half-open lets exactly one probe through")
	}
	b.failure() // the probe failed: open again, cooldown restarted
	if b.allow() {
		t.Fatal("a failed probe reopens it")
	}
	time.Sleep(40 * time.Millisecond)
	if !b.allow() || !b.success() || !b.allow() || !b.allow() {
		t.Fatal("a probe's success closes it")
	}
}

type sqlState string

func (s sqlState) Error() string    { return "pg: " + string(s) }
func (s sqlState) SQLState() string { return string(s) }

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err  error
		want ErrorClass
	}{
		{nil, OtherError},
		{errors.New("syntax error"), OtherError},
		{driver.ErrBadConn, ConnError},
		{io.EOF, ConnError},
		{fmt.Errorf("read: %w", syscall.ECONNRESET), ConnError},
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), ConnError},
		{sqlState("40001"), RetryableError},
		{sqlState("40P01"), RetryableError},
		{sqlState("08006"), ConnError},
		{sqlState("23505"), OtherError},
		{errors.New("Error 1213 (40001): Deadlock found when trying to get lock"), RetryableError},
		{errors.New("database is locked (5) (SQLITE_BUSY)"), RetryableError},
		{errors.New("context deadline exceeded"), OtherError},
	} {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestSplitPoolParams(t *testing.T) {
	url, p, err := SplitPoolParams("postgres://u:p@h/db?sslmode=disable&pool_max=7&pool_acquire=2s&breaker_failures=3&breaker_cooldown=1s&pool_min_idle=2&startup_wait=-1s")
	if err != nil || url != "postgres://u:p@h/db?sslmode=disable" {
		t.Fatalf("%s %v", url, err)
	}
	want := Pool{MaxOpen: 7, MinIdle: 2, Acquire: 2 * time.Second, StartupWait: -time.Second, Breaker: Breaker{Failures: 3, Cooldown: time.Second}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("%+v", p)
	}
	if url, _, _ := SplitPoolParams("mysql://u:p?w@tcp(h)/db?pool_max=1"); url != "mysql://u:p?w@tcp(h)/db" {
		t.Fatal(url)
	}
	if url, _, _ := SplitPoolParams("sqlite::memory:"); url != "sqlite::memory:" {
		t.Fatal(url)
	}
	if _, _, err := SplitPoolParams("sqlite:x.db?pool_max=many"); err == nil {
		t.Fatal("a bad value passes")
	}
}

func TestSessionSQL(t *testing.T) {
	s := map[string]string{"statement_timeout": "5s", "search_path": "app", "lock_timeout": "100"}
	got, err := sessionSQL("postgres", s)
	if err != nil || strings.Join(got, "; ") != "SET lock_timeout = 100; SET search_path = app; SET statement_timeout = '5s'" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := sessionSQL("mysql", map[string]string{"sql_mode": "it's"}); got[0] != "SET SESSION sql_mode = 'it''s'" {
		t.Fatal(got)
	}
	if got, _ := sessionSQL("sqlite", map[string]string{"busy_timeout": "5000"}); got[0] != "PRAGMA busy_timeout = 5000" {
		t.Fatal(got)
	}
	if _, err := sessionSQL("mysql", map[string]string{"x; DROP": "1"}); err == nil {
		t.Fatal("a setting that is no name passes")
	}
}

type xCustomer struct {
	ID   int64
	Name string
}

type xOrder struct {
	ID         int64
	CustomerID int64
	Customer   *xCustomer
}

type xLine struct {
	ID         int64
	CustomerID int64
	Customer   *xCustomer `orm:"crossdb"`
}

type refuser struct{ BaseRouter }

func (refuser) AllowRelation(a, b ModelInfo) bool { return a.Name != "xLine" }

// TestRelationCheck: a relation between models on two databases needs
// CrossDB; a router may refuse one.
func TestRelationCheck(t *testing.T) {
	s := NewDatabases()
	cust := ModelInfo{Name: "xCustomer", Type: reflect.TypeFor[xCustomer](), Table: "x_customers", Meta: Meta{DB: "crm"}}
	order := ModelInfo{Name: "xOrder", Type: reflect.TypeFor[xOrder](), Table: "x_orders"}
	line := ModelInfo{Name: "xLine", Type: reflect.TypeFor[xLine](), Table: "x_lines"}
	homes.Store(cust.Type, homeEntry{m: infoOnly(cust)})
	defer homes.Delete(cust.Type)
	errs := s.relationCheck([]ModelInfo{order, line})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "xOrder.Customer relates xOrder on the default database to xCustomer on database crm") {
		t.Fatalf("%v", errs)
	}
	s.AddRouter(refuser{})
	if errs := s.relationCheck([]ModelInfo{line}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "refuses the relation xLine.Customer") {
		t.Fatalf("%v", errs)
	}
}

// infoOnly is a home manager stand-in giving a ModelInfo.
type infoOnly ModelInfo

func (i infoOnly) Info() ModelInfo { return ModelInfo(i) }

func TestPoolDefaults(t *testing.T) {
	p := Pool{}.withDefaults()
	if p.MaxOpen != 20 || p.MaxIdle != 5 || p.MaxLifetime != time.Hour {
		t.Fatalf("defaults: %+v", p)
	}
	p = Pool{MaxOpen: -1, MaxIdle: -1, MaxLifetime: -1, MaxIdleTime: -1}.withDefaults()
	if p.MaxOpen != 0 || p.MaxIdle != 0 || p.MaxLifetime != 0 || p.MaxIdleTime != 0 {
		t.Fatalf("negative is no limit: %+v", p)
	}
	if p := (Pool{MaxOpen: 3}).withDefaults(); p.MaxIdle != 3 {
		t.Fatalf("idle above the cap: %+v", p)
	}
	if p := (Pool{MinIdle: 8}).withDefaults(); p.MaxIdle != 8 || p.MaxOpen != 20 {
		t.Fatalf("MinIdle: %+v", p)
	}
}

func TestSessionSQLMySQLBackslash(t *testing.T) {
	got, _ := sessionSQL("mysql", map[string]string{"x": `a\' OR 1`})
	if got[0] != `SET SESSION x = 'a\\'' OR 1'` {
		t.Fatal(got)
	}
	if got, _ := sessionSQL("postgres", map[string]string{"x": `a\b`}); got[0] != `SET x = 'a\b'` {
		t.Fatal(got)
	}
}
