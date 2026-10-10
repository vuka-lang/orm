package multidbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
)

func withParams(u, params string) string {
	if strings.Contains(u[strings.LastIndexByte(u, '/')+1:], "?") {
		return u + "&" + params
	}
	return u + "?" + params
}

// TestAcquireTimeout: with every connection in use, a statement waits
// Acquire, then fails Unavailable instead of waiting forever.
func TestAcquireTimeout(t *testing.T) {
	s, ctx := open(t, map[string][]orm.AnyManager{"main": {Articles}},
		orm.Declare("main", withParams(dbURL(t, "main"), "pool_max=1&pool_acquire=100ms"), orm.AsDefault()))
	err := orm.Atomic(ctx, func(tx context.Context) error {
		if err := Articles.Create(tx, &Article{Title: "holds the only connection"}); err != nil {
			return err
		}
		start := time.Now()
		_, err := Articles.Count(ctx) // outside the transaction: needs a second connection
		if !errors.Is(err, orm.ErrUnavailable) || !strings.Contains(err.Error(), "no free connection within 100ms") {
			t.Errorf("exhausted pool: %v", err)
		}
		if d := time.Since(start); d < 90*time.Millisecond || d > 2*time.Second {
			t.Errorf("waited %v", d)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Articles.Count(ctx); err != nil || n != 1 {
		t.Fatalf("after: %d %v", n, err)
	}
	st, _ := s.StatsOf("main")
	if st.Pool.MaxOpenConnections != 1 || st.Breaker != "closed" {
		t.Fatalf("stats %+v", st)
	}
}

// TestMinIdle: the pool opens MinIdle connections at start.
func TestMinIdle(t *testing.T) {
	s, _ := open(t, nil, orm.Declare("main", dbURL(t, "main"), orm.AsDefault(), orm.Pool{MinIdle: 3, MaxIdle: 5}))
	eventually(t, 3*time.Second, "warmed up", func() bool {
		st, _ := s.StatsOf("main")
		return st.Pool.Idle >= 3
	})
}

// TestSessionSettings: each connection takes the session settings.
func TestSessionSettings(t *testing.T) {
	var set orm.Session
	var read string
	var want string
	switch driver() {
	case "postgres":
		set, read, want = orm.Session{"statement_timeout": "1234"}, "SHOW statement_timeout", "1234ms"
	case "mysql":
		set, read, want = orm.Session{"wait_timeout": "1234"}, "SELECT CAST(@@SESSION.wait_timeout AS CHAR)", "1234"
	default:
		set, read, want = orm.Session{"cache_size": "1234"}, "PRAGMA cache_size", "1234"
	}
	_, ctx := open(t, nil, orm.Declare("main", dbURL(t, "main"), orm.AsDefault(), set, orm.Pool{MaxIdle: 3}))
	for range 3 {
		got, err := orm.RawRow[string](orm.Primary(ctx), read)
		if err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", read, got, err, want)
		}
	}
}

// TestCredentials: every new connection asks for its user and password.
func TestCredentials(t *testing.T) {
	dsn := os.Getenv("ORMTEST_DSN")
	var user, pass string
	switch driver() {
	case "postgres":
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		user = u.User.Username()
		pass, _ = u.User.Password()
	case "mysql":
		creds, _, _ := strings.Cut(dsn, "@")
		user, pass, _ = strings.Cut(creds, ":")
	default:
		t.Skip("SQLite has no users (ORMTEST_DRIVER=postgres|mysql)")
	}
	var calls atomic.Int32
	creds := orm.Credentials(func(context.Context) (string, string, error) {
		calls.Add(1)
		return user, pass, nil
	})
	wrong := strings.Replace(dbURL(t, "main"), user, "nobody_"+user, 1) // the URL's user is replaced
	s, ctx := open(t, map[string][]orm.AnyManager{"main": {Articles}},
		orm.Declare("main", wrong, orm.AsDefault(), creds, orm.Pool{MinIdle: 3, MaxIdle: 3}))
	eventually(t, 3*time.Second, "warmed up", func() bool {
		st, _ := s.StatsOf("main")
		return st.Pool.Idle >= 3
	})
	if err := Articles.Create(ctx, &Article{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.StatsOf("main")
	if n := int(calls.Load()); n < 3 || n < st.Pool.OpenConnections {
		t.Fatalf("%d calls for %d connections", n, st.Pool.OpenConnections)
	}
	failing := orm.Credentials(func(context.Context) (string, string, error) { return "", "", fmt.Errorf("vault sealed") })
	s2 := orm.NewDatabases(orm.Declare("main", dbURL(t, "main"), orm.AsDefault(), failing, orm.Pool{StartupWait: -1}))
	for _, n := range names[1:] {
		s2.Apply(orm.Declare(n, dbURL(t, n)))
	}
	if err := s2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s2.Close(context.Background())
	if _, err := orm.Raw[int](orm.WithDatabases(context.Background(), s2), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "vault sealed") {
		t.Fatalf("failing credentials: %v", err)
	}
}

// TestGracefulClose: Close refuses new statements at once, lets a running
// transaction finish, then closes.
func TestGracefulClose(t *testing.T) {
	s, ctx := open(t, map[string][]orm.AnyManager{"main": {Articles}})
	started, txErr := make(chan struct{}), make(chan error, 1)
	go func() {
		txErr <- orm.Atomic(ctx, func(ctx context.Context) error {
			if err := Articles.Create(ctx, &Article{Title: "in flight"}); err != nil {
				return err
			}
			close(started)
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	<-started
	closed := make(chan error, 1)
	start := time.Now()
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closed <- s.Close(c)
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := Events.Count(ctx); !errors.Is(err, orm.ErrUnavailable) || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("a statement while closing: %v", err)
	}
	if err := <-txErr; err != nil {
		t.Fatalf("the running transaction: %v", err)
	}
	if err := <-closed; err != nil || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("close: %v after %v", err, time.Since(start))
	}
	if names := s.Names(); len(names) != 0 {
		t.Fatalf("still has %v", names)
	}

	// A deadline that passes first: Close says so, and closes anyway.
	s2, ctx2 := open(t, map[string][]orm.AnyManager{"main": {Articles}})
	hold := make(chan struct{})
	go func() {
		_ = orm.Atomic(ctx2, func(ctx context.Context) error {
			_ = Articles.Create(ctx, &Article{Title: "slow"})
			close(hold)
			time.Sleep(500 * time.Millisecond)
			return nil
		})
	}()
	<-hold
	c, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s2.Close(c); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("close past its deadline: %v", err)
	}
}
