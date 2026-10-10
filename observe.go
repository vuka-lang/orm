package orm

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// QueryInfo is a statement the ORM ran, or refused to, as an Observer
// sees it.
type QueryInfo struct {
	SQL      string
	Args     []any
	Start    time.Time
	Duration time.Duration
	// Rows is the rows an exec affected; -1 for a query, whose rows are
	// read after it returns, and when the driver doesn't say.
	Rows int64
	Err  error
	// Refused is a query the ORM refused before sending it (an argument
	// it can't write for the database, a field the schema lacks): SQL is
	// a comment naming the model, Err why. It never reached the database.
	Refused bool
	Model   string // a refused query's model
}

// Observer sees each statement a context's queries run: for logging,
// tracing, counting in tests, or catching repeated queries. It runs on
// the goroutine of the query, after the statement returns.
type Observer func(ctx context.Context, q QueryInfo)

type observerKey struct{}

// WithObserver has fn see every statement queries with ctx run, after
// the observers ctx already has.
func WithObserver(ctx context.Context, fn Observer) context.Context {
	if prev, ok := ctx.Value(observerKey{}).(Observer); ok {
		next := fn
		fn = func(ctx context.Context, q QueryInfo) { prev(ctx, q); next(ctx, q) }
	}
	return context.WithValue(ctx, observerKey{}, fn)
}

// observerSet is the program's observers: Observe's.
type observerSet struct {
	mu   sync.Mutex
	next int
	fns  map[int]Observer
	all  atomic.Pointer[[]Observer] // in Observe order, read without the lock
}

var globalObservers observerSet

// Observe has fn see every statement the program's queries run, whatever
// their context: the hook a framework's tracer or debug toolbar plugs
// into. It returns what stops it.
func Observe(fn Observer) (stop func()) {
	g := &globalObservers
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fns == nil {
		g.fns = map[int]Observer{}
	}
	id := g.next
	g.next++
	g.fns[id] = fn
	g.publish()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.fns, id)
		g.publish()
	}
}

func (g *observerSet) publish() {
	if len(g.fns) == 0 {
		g.all.Store(nil)
		return
	}
	ids := slices.Sorted(maps.Keys(g.fns))
	all := make([]Observer, len(ids))
	for i, id := range ids {
		all[i] = g.fns[id]
	}
	g.all.Store(&all)
}

// observersOf is what sees ctx's statements: the program's observers,
// then ctx's. Nil for none, without allocating.
func observersOf(ctx context.Context) Observer {
	own, _ := ctx.Value(observerKey{}).(Observer)
	all := globalObservers.all.Load()
	if all == nil {
		return own
	}
	fns := *all
	return func(ctx context.Context, q QueryInfo) {
		for _, fn := range fns {
			fn(ctx, q)
		}
		if own != nil {
			own(ctx, q)
		}
	}
}

// observed is whether anything sees ctx's statements.
func observed(ctx context.Context) bool {
	if globalObservers.all.Load() != nil {
		return true
	}
	_, ok := ctx.Value(observerKey{}).(Observer)
	return ok
}

// WatchRepeats is ctx warning, through warn, when its queries run the same
// statement n times: a statement reading related rows one parent at a
// time, an N+1, which SelectRelated or PrefetchRelated read in one. Give
// it a request's context; warn is called once per statement.
//
//	ctx = orm.WatchRepeats(ctx, 5, func(ctx context.Context, sql string, n int) {
//		slog.WarnContext(ctx, "orm: N+1", "sql", sql, "times", n)
//	})
func WatchRepeats(ctx context.Context, n int, warn func(ctx context.Context, sql string, times int)) context.Context {
	if n <= 0 {
		return ctx
	}
	var mu sync.Mutex
	counts := map[string]int{}
	return WithObserver(ctx, func(ctx context.Context, q QueryInfo) {
		if q.Refused {
			return
		}
		mu.Lock()
		counts[q.SQL]++
		c := counts[q.SQL]
		mu.Unlock()
		if c == n {
			warn(ctx, q.SQL, c)
		}
	})
}

// exec and query run a statement, seen by ctx's observers.
func (c conn) exec(ctx context.Context, q string, args []any) (sql.Result, error) {
	if err := argLimit(c.d, args); err != nil {
		return nil, err
	}
	w := watch(ctx, q, args)
	res, err := c.run.ExecContext(ctx, q, args...)
	if w.obs != nil {
		w.rows = -1
		if err == nil {
			if n, rerr := res.RowsAffected(); rerr == nil {
				w.rows = n
			}
		}
	}
	w.done(err)
	return res, err
}

func (c conn) query(ctx context.Context, q string, args []any) (*sql.Rows, error) {
	if err := argLimit(c.d, args); err != nil {
		return nil, err
	}
	w := watch(ctx, q, args)
	w.rows = -1
	rows, err := c.run.QueryContext(ctx, q, args...)
	w.done(err)
	return rows, err
}

// argLimit refuses a statement with more arguments than d's database
// takes in one (an __in of a list from a request, say), before it is
// sent: Postgres and MySQL take 65535, SQLite 32766.
func argLimit(d Dialect, args []any) error {
	limit := 65535
	if d.Name() == "sqlite" {
		limit = 32766
	}
	if len(args) > limit {
		return fmt.Errorf("orm: a statement of %d arguments: %s takes at most %d (an __in list that long belongs in a subquery or a table)", len(args), d.Name(), limit)
	}
	return nil
}

// watching is a statement being run, as watch started it: done ends it.
// A value, not a closure, so an unwatched statement allocates nothing.
type watching struct {
	ctx   context.Context
	q     string
	args  []any
	start time.Time
	rows  int64
	obs   Observer
}

func watch(ctx context.Context, q string, args []any) watching {
	noteRun(ctx)
	w := watching{ctx: ctx, q: q, args: args, obs: observersOf(ctx)}
	if w.obs != nil {
		w.start = time.Now()
	}
	return w
}

func (w watching) done(err error) {
	if w.obs != nil {
		w.obs(w.ctx, QueryInfo{SQL: w.q, Args: w.args, Start: w.start, Duration: time.Since(w.start), Rows: w.rows, Err: err})
	}
}

// A query the ORM refuses never reaches the database, so no statement
// reports it. While something observes ctx, each call that can send
// statements notes whether it sent one; failing without having sent any,
// it reports the refusal to the observers (Refused, with the ORM's
// error) — a debug toolbar shows it beside the statements that ran.

type runKey struct{}

// runs is ctx noting whether a statement ran under it, and the note; nil
// when nothing observes ctx, or when an enclosing call already watches
// it.
func runs(ctx context.Context) (context.Context, *bool) {
	if !observed(ctx) {
		return ctx, nil
	}
	if _, ok := ctx.Value(runKey{}).(*bool); ok {
		return ctx, nil
	}
	ran := new(bool)
	return context.WithValue(ctx, runKey{}, ran), ran
}

func noteRun(ctx context.Context) {
	if ran, ok := ctx.Value(runKey{}).(*bool); ok {
		*ran = true
	}
}

// refused reports err as a statement of model's that never ran.
func refused(ctx context.Context, model string, ran *bool, err error) {
	if ran == nil || *ran || err == nil {
		return
	}
	*ran = true
	if obs := observersOf(ctx); obs != nil {
		obs(ctx, QueryInfo{SQL: "-- not sent: a " + model + " query the ORM refused", Rows: -1, Err: err, Refused: true, Model: model, Start: time.Now()})
	}
}
