package orm_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

func TestRefusedQueriesAreObserved(t *testing.T) {
	ctx := ormtest.Open(t, Tickets, OldTickets)
	var refused, ran int
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) {
		if q.Refused {
			refused++
			if q.Err == nil || !strings.Contains(q.Err.Error(), "differs per schema") || !strings.Contains(q.SQL, "Ticket query the ORM refused") || q.Model != "Ticket" {
				t.Errorf("refusal %+v", q)
			}
			return
		}
		ran++
		if q.Start.IsZero() || q.Rows != -1 {
			t.Errorf("statement %+v", q)
		}
	})

	OldTickets.Filter(orm.Q{"status": 2}).Count(ctx)                              // refused: an int for a per-schema field
	OldTickets.Values[string]("status").All(ctx)                                  // refused: read into a string
	OldTickets.Filter(orm.Q{"status": TicketOpen}).Count(ctx)                     // runs
	OldTickets.Get(ctx, orm.Q{"id": 999})                                         // runs, finds nothing: not a refusal
	OldTickets.Filter(orm.Q{"status__in": []int{6}}).Values[int64]("id").All(ctx) // refused
	if refused != 3 || ran != 2 {
		t.Fatalf("refused %d, ran %d", refused, ran)
	}
}

func TestObserve(t *testing.T) {
	ctx := ormtest.Open(t, Tickets)
	var seen atomic.Int64
	var info orm.QueryInfo
	stop := orm.Observe(func(_ context.Context, q orm.QueryInfo) {
		seen.Add(1)
		info = q
	})
	if _, err := Tickets.Filter(orm.Q{"id": 7}).Update(ctx, orm.Set{"status": TicketOpen}); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 1 || info.Rows != 0 || len(info.Args) == 0 || !strings.Contains(info.SQL, "UPDATE") {
		t.Fatalf("seen %d: %+v", seen.Load(), info)
	}
	stop()
	Tickets.Count(ctx)
	if seen.Load() != 1 {
		t.Fatal("seen after stop")
	}
}

func TestWatchRepeats(t *testing.T) {
	ctx := ormtest.Open(t, Tickets)
	var warned []string
	ctx = orm.WatchRepeats(ctx, 3, func(_ context.Context, sql string, n int) { warned = append(warned, sql) })
	for i := range 5 {
		Tickets.Filter(orm.Q{"id": i}).Count(ctx)
	}
	Tickets.Count(ctx)
	if len(warned) != 1 || !strings.Contains(warned[0], "WHERE") {
		t.Fatalf("warned %q", warned)
	}
}
