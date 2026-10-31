package orm

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
)

// DualOption tunes a Dual route. The mirror options (MirrorUnder,
// MirrorTable) and Names are the ones that carry over; it is ForOption so
// the same helpers serve For, Of and Dual.
type DualOption = ForOption

// Dual routes T for a dual write without a code change on the model: its
// models read and write primary, and every write is repeated on mirror
// after the primary commits (see Mirror). MirrorUnder and MirrorTable bind
// the mirror through a names set and table of its own — a legacy schema,
// whose columns, types and value codecs the model declares for that set —
// so the clean model stays clean and the mirror's shape rides the route.
// The table comes from T's Meta. Reads never use the mirror.
//
//	orm.Default.Apply(orm.Dual[Placement]("main", "legacy",
//		orm.MirrorUnder("legacy"), orm.MirrorTable("tbl_placements")))
//
// It is sugar for a Route; the raw Route (DB, Mirror, Names, MirrorNames,
// MirrorTable) expresses the same from configuration, and a cutover swaps
// primary and mirror with their schemas. Keep one of Dual or a Route per
// table.
func Dual[T any](primary, mirror string, opts ...DualOption) OpenOption {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	return openOptionFunc(func(s *Databases) {
		s.Route(infoOf(reflect.TypeFor[T]()).Table, Route{
			DB: primary, Mirror: mirror,
			Names: c.names, MirrorNames: c.mirrorNames, MirrorTable: c.mirrorTable,
		})
	})
}

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

// MirrorUnder binds the mirror through a names set: its writes take the
// columns, types and value codecs the model's fields declare for that set
// (a legacy schema's). Reads still never use the mirror. It pairs with
// Mirror (or a Route's) and MirrorTable.
func MirrorUnder(names string) ForOption { return func(c *forConfig) { c.mirrorNames = names } }

// MirrorTable names the mirror's table, over the model's: the legacy
// table a row is mirrored into.
func MirrorTable(name string) ForOption { return func(c *forConfig) { c.mirrorTable = name } }

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
// or one bound by name, and the names set and table the mirror binds
// through (its own, over the model's).
type mirrorTarget struct {
	name  string
	db    *DB
	names string
	table string
}

type inMirrorKey struct{}

// mirrorOf is where ctx's writes of the model are mirrored, if anywhere,
// and under which names set and table. A write already being mirrored
// isn't mirrored again.
func (m *managerCore) mirrorOf(ctx context.Context) (mirrorTarget, bool) {
	if _, in := ctx.Value(inMirrorKey{}).(mirrorTarget); in {
		return mirrorTarget{}, false
	}
	if d, ok := ctx.Value(mirrorDBKey{}).(*DB); ok {
		return mirrorTarget{db: d, names: m.mirrorNames, table: m.mirrorTable}, true
	}
	// For's Mirror wins over a Route; it carries the manager's MirrorUnder
	// and MirrorTable, a Route its own.
	if m.mirror != "" {
		return mirrorTarget{name: m.mirror, names: m.mirrorNames, table: m.mirrorTable}, true
	}
	if name, names, table := databasesOf(ctx).mirrorFor(m.Info()); name != "" {
		return mirrorTarget{name: name, names: names, table: table}, true
	}
	return mirrorTarget{}, false
}

// mirrorAs is the manager writing the mirror of m under t's names set and
// table: m itself when the mirror reuses the model's, else one built once
// per (names, table) so the write binds through the mirror's columns,
// types and value codecs.
func (m *managerCore) mirrorAs(t mirrorTarget) *managerCore {
	if t.names == m.names && (t.table == "" || t.table == m.meta.Table) {
		return m
	}
	key := t.names + "\x00" + t.table
	if v, ok := m.mirrorMgrs.Load(key); ok {
		return v.(*managerCore)
	}
	mm := m.typed.newMirror(forConfig{names: t.names, table: t.table, unmanaged: true})
	v, _ := m.mirrorMgrs.LoadOrStore(key, mm)
	return v.(*managerCore)
}

// mirrored runs write on the model's mirror once the write that ran on
// primary commits; write reaches the mirror through the ctx it's given,
// and the manager binding through the mirror's names set and table.
func (m *managerCore) mirrored(ctx context.Context, primary conn, op string, write func(ctx context.Context, mm *managerCore) error) {
	t, ok := m.mirrorOf(ctx)
	if !ok {
		return
	}
	mm := m.mirrorAs(t)
	afterCommit(ctx, primary.db, func(ctx context.Context) {
		mctx := context.WithValue(context.WithoutCancel(ctx), inMirrorKey{}, t)
		if err := write(mctx, mm); err != nil {
			reportMirror(ctx, MirrorError{Table: m.meta.Table, Op: op, Mirror: t.name, Err: err})
		}
	})
}

// mirrorConn is the database a mirrored write goes to.
func (m *managerCore) mirrorConn(ctx context.Context, t mirrorTarget) (conn, error) {
	if t.db != nil {
		return on(ctx, t.db), nil
	}
	d, err := databasesOf(ctx).Lookup(t.name)
	if err != nil {
		return conn{}, err
	}
	return on(ctx, d), nil
}
