package multidbtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
)

// TestAtomicPicksDatabase: the first write picks the transaction's
// database; a write to another fails, and everything rolls back.
func TestAtomicPicksDatabase(t *testing.T) {
	both := []orm.AnyManager{Articles, Events}
	_, ctx := open(t, map[string][]orm.AnyManager{"main": both, "analytics": both})
	err := orm.Atomic(ctx, func(ctx context.Context) error {
		if n, err := Articles.Count(ctx); err != nil || n != 0 { // a read before: no database yet
			t.Errorf("read: %d %v", n, err)
		}
		if err := Events.Create(ctx, &Event{Kind: "first"}); err != nil {
			return err
		}
		return Articles.Create(ctx, &Article{Title: "elsewhere"})
	})
	if err == nil || !strings.Contains(err.Error(), "a transaction spans one database") {
		t.Fatalf("a write to a second database: %v", err)
	}
	if count(t, ctx, "analytics", Events) != 0 || count(t, ctx, "main", Articles) != 0 {
		t.Fatal("not rolled back")
	}

	// TxOn names the database from the start.
	err = orm.Atomic(ctx, func(ctx context.Context) error {
		return Events.Create(ctx, &Event{Kind: "x"})
	}, orm.TxOn("main"))
	if err == nil || !strings.Contains(err.Error(), "spans one database") {
		t.Fatalf("TxOn main, a write to analytics: %v", err)
	}

	// A savepoint rolls back alone; OnCommit runs after the commit only.
	var committed []string
	err = orm.Atomic(ctx, func(ctx context.Context) error {
		_ = orm.OnCommit(ctx, func(context.Context) { committed = append(committed, "outer") })
		if err := Events.Create(ctx, &Event{Kind: "kept"}); err != nil {
			return err
		}
		_ = orm.Atomic(ctx, func(ctx context.Context) error {
			_ = orm.OnCommit(ctx, func(context.Context) { committed = append(committed, "inner") })
			_ = Events.Create(ctx, &Event{Kind: "dropped"})
			return errors.New("inner fails")
		})
		if len(committed) != 0 {
			t.Error("OnCommit ran before the commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, "analytics", Events); n != 1 || len(committed) != 1 || committed[0] != "outer" {
		t.Fatalf("events %d, committed %v", n, committed)
	}

	// A savepoint inside a transaction not begun yet: its write begins it.
	err = orm.Atomic(ctx, func(ctx context.Context) error {
		if err := orm.Atomic(ctx, func(ctx context.Context) error {
			return Events.Create(ctx, &Event{Kind: "rolled back alone"})
		}); err != nil {
			return err
		}
		if err := orm.Atomic(ctx, func(ctx context.Context) error {
			_ = Events.Create(ctx, &Event{Kind: "gone"})
			return errors.New("no")
		}); err == nil {
			t.Error("inner error lost")
		}
		return nil
	})
	if err != nil || count(t, ctx, "analytics", Events) != 2 {
		t.Fatalf("lazy savepoints: %v, %d events", err, count(t, ctx, "analytics", Events))
	}
}

type deadlock struct{}

func (deadlock) Error() string    { return "deadlock detected" }
func (deadlock) SQLState() string { return "40P01" }

// TestRetry runs the transaction again on a deadlock, not on other errors.
func TestRetry(t *testing.T) {
	_, ctx := open(t, map[string][]orm.AnyManager{"main": {Accounts}})
	runs := 0
	err := orm.Atomic(ctx, func(ctx context.Context) error {
		runs++
		if err := Accounts.Create(ctx, &Account{Balance: runs}); err != nil {
			return err
		}
		if runs < 3 {
			return deadlock{}
		}
		return nil
	}, orm.Retry(3))
	if err != nil || runs != 3 || count(t, ctx, "main", Accounts) != 1 {
		t.Fatalf("runs %d, %v, %d rows", runs, err, count(t, ctx, "main", Accounts))
	}
	runs = 0
	err = orm.Atomic(ctx, func(context.Context) error { runs++; return errors.New("plain") }, orm.Retry(3))
	if err == nil || runs != 1 {
		t.Fatalf("a plain error retried: %d", runs)
	}
}

// TestRetryDeadlock locks two rows in opposite orders from two
// transactions: the database kills one, Retry runs it again, both land.
func TestRetryDeadlock(t *testing.T) {
	if driver() == "sqlite" {
		t.Skip("SQLite takes one writer at a time: no deadlock to retry (ORMTEST_DRIVER=postgres|mysql)")
	}
	_, ctx := open(t, map[string][]orm.AnyManager{"main": {Accounts}})
	a, b := Account{Balance: 100}, Account{Balance: 100}
	_ = Accounts.Create(ctx, &a)
	_ = Accounts.Create(ctx, &b)
	var ready sync.WaitGroup
	ready.Add(2)
	var attempts atomic.Int32
	move := func(from, to int64) error {
		first := true
		return orm.Atomic(ctx, func(ctx context.Context) error {
			attempts.Add(1)
			if _, err := orm.Exec(ctx, "UPDATE accounts SET balance = balance - 1 WHERE id = ?", from); err != nil {
				return err
			}
			if first {
				first = false
				ready.Done()
				ready.Wait() // both hold their first row
			}
			_, err := orm.Exec(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = ?", to)
			return err
		}, orm.Retry(3))
	}
	errs := make(chan error, 2)
	go func() { errs <- move(a.ID, b.ID) }()
	go func() { errs <- move(b.ID, a.ID) }()
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("stuck")
		}
	}
	if attempts.Load() < 3 {
		t.Fatalf("%d attempts: no deadlock was retried", attempts.Load())
	}
	for _, id := range []int64{a.ID, b.ID} {
		got, err := Accounts.Get(ctx, orm.Q{"id": id})
		if err != nil || got.Balance != 100 {
			t.Fatalf("account %d: %d %v", id, got.Balance, err)
		}
	}
}
