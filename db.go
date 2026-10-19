package orm

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DB is a database the ORM queries: a *sql.DB and its dialect, and,
// for one Declare opened, the supervisor keeping its pool (see Pool).
type DB struct {
	sql      *sql.DB
	dialect  Dialect
	server   atomic.Pointer[string] // a MySQL server's VERSION(), once read
	name     string                 // the name it was added under
	readOnly bool
	sup      *supervisor // nil for one opened elsewhere (Open, Connect)
}

// Name is the name the database was added under.
func (d *DB) Name() string { return d.name }

// Open is a DB over a *sql.DB of the given dialect ("postgres", "mysql",
// "sqlite"; "pgx" and "postgresql" read as postgres). Connect opens the
// *sql.DB too.
func Open(db *sql.DB, driver string) *DB { return &DB{sql: db, dialect: DialectFor(driver)} }

// SQL is the underlying *sql.DB.
func (d *DB) SQL() *sql.DB { return d.sql }

// Dialect is how the database speaks SQL.
func (d *DB) Dialect() Dialect { return d.dialect }

// schemaDialect is the dialect the database's tables are written in, as
// schema.For reads it: MySQL's with its server's version, which says
// whether it has the VECTOR type, read once through run.
func (d *DB) schemaDialect(ctx context.Context, run runner) (string, error) {
	name := d.dialect.Name()
	if name != "mysql" {
		return name, nil
	}
	if v := d.server.Load(); v != nil {
		return name + "/" + *v, nil
	}
	rows, err := run.QueryContext(ctx, "SELECT VERSION()")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var v string
	if rows.Next() {
		err = rows.Scan(&v)
	}
	if err = cmp.Or(err, rows.Err()); err != nil {
		return "", err
	}
	d.server.Store(&v)
	return name + "/" + v, nil
}

type dbKey struct{}

// WithDB has every model in ctx's queries use db, whatever database it is
// on: for tests and tools.
func WithDB(ctx context.Context, db *DB) context.Context {
	return context.WithValue(ctx, dbKey{}, db)
}

// DBFrom is the database WithDB put in ctx.
func DBFrom(ctx context.Context) (*DB, bool) {
	d, ok := ctx.Value(dbKey{}).(*DB)
	return d, ok
}

type usingKey struct{}

// Using sends ctx's queries to the database added as name, instead of
// each model's own.
func Using(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, usingKey{}, name)
}

// runner is what runs a statement: the database, or a transaction on it.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// conn is a resolved database for one statement: where to send it, and
// how it speaks.
type conn struct {
	run  runner
	d    Dialect
	db   *sql.DB
	src  *DB
	name string   // the database's name, for observers
	read bool     // a read: retried once on a dropped connection
	rep  *replica // the replica it reads, if any
}

type txKey struct{ db *sql.DB }

type txState struct {
	tx    *sql.Tx
	depth *atomic.Int64
	after *[]func(context.Context)
}

// on is db as ctx sees it: inside a transaction ctx opened on it, the
// transaction.
func on(ctx context.Context, db *DB) conn {
	c := conn{run: db.sql, d: db.dialect, db: db.sql, src: db, name: db.name}
	if t, ok := ctx.Value(txKey{db.sql}).(*txState); ok {
		c.run = t.tx
	} else if f, ok := ctx.Value(atomicKey{}).(*frame); ok {
		if st := f.root.on(db); st != nil {
			c.run = st.tx
		}
	}
	return c
}

// TxOption tunes Atomic: TxOn, Retry.
type TxOption interface{ txOption(*txConfig) }

type txConfig struct {
	db    string
	on    bool
	retry int
}

// TxOn runs a transaction on the database added as name, from its start,
// instead of the database its first write picks.
type TxOn string

func (t TxOn) txOption(c *txConfig) { c.db, c.on = string(t), true }

// Retry runs a transaction again, up to n more times, when the database
// aborts it for a deadlock or a serialization failure (Postgres 40001
// and 40P01, MySQL 1213 and 1205, SQLite's busy), after a short backoff.
// Only the outermost transaction retries: fn must be safe to run again.
type Retry int

func (r Retry) txOption(c *txConfig) { c.retry = int(r) }

// Atomic runs fn in a transaction: committed when fn returns nil, rolled
// back when it returns an error or panics. Queries take part when they
// use the ctx fn is given. The first write inside it picks the database
// (each model's write database, as it routes); reads before it run
// outside, on the models' write databases, and a write to another
// database fails: a transaction spans one database. TxOn names it from
// the start; Retry retries deadlocks. Inside another Atomic it is a
// savepoint, rolled back alone. OnCommit runs work once it commits.
func Atomic(ctx context.Context, fn func(ctx context.Context) error, opts ...TxOption) error {
	var c txConfig
	for _, o := range opts {
		o.txOption(&c)
	}
	var d *DB
	if c.on {
		cn, err := resolve(ctx, databasesOf(ctx), nil, c.db, true)
		if err != nil {
			return err
		}
		d = cn.src
	}
	return atomically(ctx, d, fn, c)
}

// Transaction is Atomic: fn run in a transaction, committed when it
// returns nil, rolled back when it returns an error or panics.
//
//	err := orm.Transaction(ctx, func(ctx context.Context) error {
//		p, err := orm.For[Post]().Get(ctx, PostFields.Slug.Eq(slug))
//		if err != nil {
//			return err
//		}
//		p.Published = time.Now()
//		return p.Save(ctx)
//	}, orm.Retry(3))
func Transaction(ctx context.Context, fn func(ctx context.Context) error, opts ...TxOption) error {
	return Atomic(ctx, fn, opts...)
}

// AtomicOn is Atomic on a given database.
func AtomicOn(ctx context.Context, db *DB, fn func(ctx context.Context) error, opts ...TxOption) error {
	var c txConfig
	for _, o := range opts {
		o.txOption(&c)
	}
	return atomically(ctx, db, fn, c)
}

func atomically(ctx context.Context, d *DB, fn func(ctx context.Context) error, c txConfig) error {
	_, nested := ctx.Value(atomicKey{}).(*frame)
	for attempt := 0; ; attempt++ {
		err := runFrame(ctx, d, fn)
		if err == nil || nested || attempt >= c.retry || classify(err) != RetryableError {
			return err
		}
		wait := min(time.Second, time.Duration(20<<attempt)*time.Millisecond)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait/2 + time.Duration(rand.Int64N(int64(wait/2)+1))):
		}
	}
}

type atomicKey struct{}

// frame is an Atomic running: the transaction's root, and the savepoint
// it took once the transaction began.
type frame struct {
	parent *frame
	root   *txRoot
	sp     string
	after  []func(context.Context) // what its writes run after the commit
}

// txRoot is the outermost Atomic's transaction: begun on the database the
// first write (or TxOn) names.
type txRoot struct {
	mu  sync.Mutex
	ctx context.Context // the Atomic's own, the transaction begins with
	db  *DB
	st  *txState
	end func()
	n   int // savepoints taken
}

func (r *txRoot) on(db *DB) *txState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil && r.db.sql == db.sql {
		return r.st
	}
	return nil
}

// bind is f's transaction on d: begun when none is, with savepoints for
// the frames between it and the root; an error for another database.
func (f *frame) bind(d *DB) (*txState, error) {
	r := f.root
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.db == nil:
		tx, end, err := d.begin(r.ctx)
		if err != nil {
			return nil, err
		}
		r.db, r.end = d, end
		r.st = &txState{tx: tx, depth: new(atomic.Int64), after: new([]func(context.Context))}
	case r.db.sql != d.sql:
		return nil, fmt.Errorf("orm: a transaction spans one database: this one is on %s, and the write is to %s (run it in its own orm.Atomic, after this one)", dbLabel(r.db.name), dbLabel(d.name))
	}
	var path []*frame
	for g := f; g.parent != nil && g.sp == ""; g = g.parent {
		path = append(path, g)
	}
	for i := len(path) - 1; i >= 0; i-- {
		r.n++
		name := "orm_sp_" + strconv.Itoa(r.n)
		if _, err := r.st.tx.ExecContext(r.ctx, "SAVEPOINT "+name); err != nil {
			return nil, err
		}
		path[i].sp = name
	}
	return r.st, nil
}

func runFrame(ctx context.Context, d *DB, fn func(ctx context.Context) error) (err error) {
	f := &frame{}
	if p, ok := ctx.Value(atomicKey{}).(*frame); ok {
		f.parent, f.root = p, p.root
	} else {
		f.root = &txRoot{ctx: ctx}
	}
	if d == nil {
		f.root.mu.Lock()
		d = f.root.db // a transaction begun already: a savepoint of it now
		f.root.mu.Unlock()
	}
	if d != nil {
		if _, err := f.bind(d); err != nil {
			return err
		}
	}
	defer func() {
		if p := recover(); p != nil {
			f.finish(ctx, errors.New("panic"))
			panic(p)
		}
		err = f.finish(ctx, err)
	}()
	return fn(context.WithValue(ctx, atomicKey{}, f))
}

// finish ends f: a savepoint released or rolled back, its after-commit
// work handed to its parent; the root's transaction committed (then the
// work run) or rolled back.
func (f *frame) finish(ctx context.Context, err error) error {
	r := f.root
	if f.parent != nil {
		if f.sp != "" {
			r.mu.Lock()
			if err != nil {
				_, _ = r.st.tx.ExecContext(r.ctx, "ROLLBACK TO SAVEPOINT "+f.sp)
			} else if _, rerr := r.st.tx.ExecContext(r.ctx, "RELEASE SAVEPOINT "+f.sp); rerr != nil {
				err = rerr
			}
			r.mu.Unlock()
		}
		if err == nil {
			f.parent.after = append(f.parent.after, f.after...)
		}
		return err
	}
	if r.db == nil {
		if err == nil {
			for _, fn := range f.after {
				fn(ctx)
			}
		}
		return err
	}
	defer r.end()
	if err != nil {
		_ = r.st.tx.Rollback()
		return err
	}
	if err := r.db.sup.commit(r.st.tx); err != nil {
		return err
	}
	for _, fn := range append(*r.st.after, f.after...) {
		fn(ctx)
	}
	return nil
}

// inTx runs fn in tx, a transaction on db the queries of fn's ctx take
// part in: committed when fn returns nil, then what its writes asked to
// run after a commit; rolled back when it returns an error or panics.
func inTx(ctx context.Context, db *sql.DB, tx *sql.Tx, fn func(ctx context.Context) error) (err error) {
	var after []func(context.Context)
	state := &txState{tx: tx, depth: new(atomic.Int64), after: &after}
	inner := context.WithValue(ctx, txKey{db}, state)
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		if err = tx.Commit(); err == nil {
			for _, f := range after {
				f(ctx)
			}
		}
	}()
	return fn(inner)
}

// afterCommit runs f once ctx's transaction on db commits, or now when
// there is none.
func afterCommit(ctx context.Context, db *sql.DB, f func(context.Context)) {
	if t, ok := ctx.Value(txKey{db}).(*txState); ok {
		*t.after = append(*t.after, f)
		return
	}
	if fr, ok := ctx.Value(atomicKey{}).(*frame); ok {
		fr.root.mu.Lock()
		bound := fr.root.db != nil && fr.root.db.sql == db
		fr.root.mu.Unlock()
		if bound {
			fr.after = append(fr.after, f)
			return
		}
	}
	f(ctx)
}

var errNoDB = errors.New("orm: no database: add one with orm.AddDatabase, or use orm.WithDB")

// defaultDB is the database ctx names: WithDB's, else Using's or the
// default of ctx's Databases.
func defaultDB(ctx context.Context) (*DB, error) {
	if db, ok := ctx.Value(dbKey{}).(*DB); ok {
		return db, nil
	}
	name, _ := ctx.Value(usingKey{}).(string)
	return databasesOf(ctx).Lookup(name)
}

// OnCommit runs fn once ctx's transaction commits — at once outside one;
// a rolled back transaction never runs it, nor a savepoint's rolled back
// part. The way to send mail or enqueue work (an outbox) only for writes
// that stay.
func OnCommit(ctx context.Context, fn func(ctx context.Context)) error {
	if f, ok := ctx.Value(atomicKey{}).(*frame); ok {
		f.after = append(f.after, fn)
		return nil
	}
	db, err := defaultDB(ctx)
	if err != nil {
		return err
	}
	afterCommit(ctx, db.sql, fn)
	return nil
}
