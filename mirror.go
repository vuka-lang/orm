package orm

import (
	"context"
	"log/slog"
	"sync"
)

// Mirror has every write of the model repeated on the database added as
// name, after it succeeds on the model's own database: a
// table moving from one database to another keeps both current while
// reads stay on the first. Reads never use the mirror.
//
// The mirror is written after the write's transaction commits (at once
// outside one), with the keys the primary gave new rows. A mirror that
// fails doesn't fail the write: the primary is the truth, and the failure
// goes to OnMirrorError (and the log) so the table can be re-synced.
//
// A Route routes a table without a code change, for models naming no
// databases of their own (a program reads its routes from its
// configuration):
//
//	orm.Default.Route("categories", orm.Route{
//		DB:     "legacy", // read, and written first
//		Mirror: "main",   // written after
//	})
//
// Cutting the table over is swapping the two, then dropping the mirror.
func Mirror(name string) ForOption { return func(c *forConfig) { c.mirror = name } }

// MirrorError is a mirrored write that failed: the primary has the change,
// the mirror doesn't.
type MirrorError struct {
	Table  string
	Op     string // create, update, delete
	Mirror string // the mirror's database name ("" for one WithMirror set)
	Err    error
}

func (e MirrorError) Error() string {
	return "orm: mirroring " + e.Op + " of " + e.Table + " to " + e.Mirror + ": " + e.Err.Error()
}

var mirrorHooks struct {
	mu   sync.Mutex
	next int
	fns  map[int]func(context.Context, MirrorError)
}

// OnMirrorError calls fn for each mirrored write that fails, beside the
// log: the place to queue the table for a re-sync. It returns what stops
// it.
func OnMirrorError(fn func(ctx context.Context, e MirrorError)) (stop func()) {
	mirrorHooks.mu.Lock()
	defer mirrorHooks.mu.Unlock()
	if mirrorHooks.fns == nil {
		mirrorHooks.fns = map[int]func(context.Context, MirrorError){}
	}
	id := mirrorHooks.next
	mirrorHooks.next++
	mirrorHooks.fns[id] = fn
	return func() {
		mirrorHooks.mu.Lock()
		delete(mirrorHooks.fns, id)
		mirrorHooks.mu.Unlock()
	}
}

func reportMirror(ctx context.Context, e MirrorError) {
	slog.ErrorContext(ctx, "orm: mirror write failed", "table", e.Table, "op", e.Op, "mirror", e.Mirror, "err", e.Err)
	mirrorHooks.mu.Lock()
	fns := make([]func(context.Context, MirrorError), 0, len(mirrorHooks.fns))
	for _, fn := range mirrorHooks.fns {
		fns = append(fns, fn)
	}
	mirrorHooks.mu.Unlock()
	for _, fn := range fns {
		fn(ctx, e)
	}
}

type mirrorDBKey struct{}

// WithMirror has every model's writes in ctx repeated on db, as Mirror
// does by name: for tests and tools.
func WithMirror(ctx context.Context, db *DB) context.Context {
	return context.WithValue(ctx, mirrorDBKey{}, db)
}

// mirrorTarget is where a write is mirrored: a database WithMirror gave,
// or one bound by name.
type mirrorTarget struct {
	name string
	db   *DB
}

type inMirrorKey struct{}

// mirrorOf is where ctx's writes of the model are mirrored, if anywhere.
// A write already being mirrored isn't mirrored again.
func (m *Manager[T]) mirrorOf(ctx context.Context) (mirrorTarget, bool) {
	if _, in := ctx.Value(inMirrorKey{}).(mirrorTarget); in {
		return mirrorTarget{}, false
	}
	if d, ok := ctx.Value(mirrorDBKey{}).(*DB); ok {
		return mirrorTarget{db: d}, true
	}
	if name := databasesOf(ctx).mirrorFor(m.Info()); name != "" {
		return mirrorTarget{name: name}, true
	}
	return mirrorTarget{}, false
}

// mirrored runs write on the model's mirror once the write that ran on
// primary commits; write reaches the mirror through the ctx it's given.
func (m *Manager[T]) mirrored(ctx context.Context, primary conn, op string, write func(ctx context.Context) error) {
	t, ok := m.mirrorOf(ctx)
	if !ok {
		return
	}
	afterCommit(ctx, primary.db, func(ctx context.Context) {
		mctx := context.WithValue(context.WithoutCancel(ctx), inMirrorKey{}, t)
		if err := write(mctx); err != nil {
			reportMirror(ctx, MirrorError{Table: m.meta.Table, Op: op, Mirror: t.name, Err: err})
		}
	})
}

// mirrorConn is the database a mirrored write goes to.
func (m *Manager[T]) mirrorConn(ctx context.Context, t mirrorTarget) (conn, error) {
	if t.db != nil {
		return on(ctx, t.db), nil
	}
	d, err := databasesOf(ctx).Lookup(t.name)
	if err != nil {
		return conn{}, err
	}
	return on(ctx, d), nil
}
