package orm

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"
	"time"
	"unsafe"
)

// RawQuerySet is a model's rows read by SQL of your own, Django's
// RawQuerySet: see Manager.Raw.
type RawQuerySet[T any] struct {
	m        *Manager[T]
	sql      string
	args     []any
	prefetch []PrefetchSpec
}

// Raw is the rows sql reads, as the model's: each column fills the field
// of that column under the manager's names set (else of that name, or the
// computed field), and other columns are dropped. ? marks each argument,
// ?? is a literal ?. The SQL is the schema's own: written for the schema
// the manager is on.
//
//	users, err := orm.Of[User](Legacy).Raw("SELECT * FROM auth_user WHERE tel_no LIKE ?", "255%").All(ctx)
func (m *Manager[T]) Raw(sql string, args ...any) RawQuerySet[T] {
	return RawQuerySet[T]{m: m, sql: sql, args: args}
}

// PrefetchRelated loads relations of the rows after them, as a QuerySet's
// does.
func (r RawQuerySet[T]) PrefetchRelated(specs ...any) RawQuerySet[T] {
	r.prefetch = appendSpecs(slices.Clip(r.prefetch), specs)
	return r
}

// Iter runs the SQL and yields its rows one at a time; relations
// PrefetchRelated names are not loaded.
func (r RawQuerySet[T]) Iter(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		o, err := r.m.openRaw(ctx, r.sql, r.args)
		if err != nil {
			yield(zero, err)
			return
		}
		defer o.rows.Close()
		var row, blank T // read into again and again, yielded by value
		v := reflect.ValueOf(&row).Elem()
		for o.rows.Next() {
			row = blank
			if err := o.scan(v); err != nil {
				yield(zero, err)
				return
			}
			r.m.adoptAt(unsafe.Pointer(&row))
			if !yield(row, nil) {
				return
			}
		}
		if err := o.rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

// rawRows is the rows of raw SQL, and where each column goes.
type rawRows struct {
	rows  *sql.Rows
	index [][]int
	cells []cell
	dest  []any
}

func (m *managerCore) openRaw(ctx context.Context, text string, args []any) (*rawRows, error) {
	c, err := m.conn(ctx, false)
	if err != nil {
		return nil, err
	}
	rows, err := rawQuery(ctx, c, text, args)
	if err != nil {
		return nil, err
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, err
	}
	meta := m.meta
	o := &rawRows{rows: rows, index: make([][]int, len(cols)), cells: make([]cell, len(cols)), dest: make([]any, len(cols))}
	for i, col := range cols {
		k := strings.ToLower(col)
		f, ok := meta.byCol[k]
		if !ok {
			f, ok = meta.byName[k]
		}
		if !ok {
			f, ok = meta.computed[k]
		}
		if ok {
			o.index[i], o.cells[i].fast = f.Index, f.fast
		}
	}
	for i := range cols {
		o.dest[i] = new(any) // a column no field takes, dropped
		if o.index[i] != nil {
			o.dest[i] = &o.cells[i]
		}
	}
	return o, nil
}

// scan reads the row the rows are at into v, a row.
func (o *rawRows) scan(v reflect.Value) error {
	for i := range o.index {
		if o.index[i] != nil {
			o.cells[i].dst = fieldOf(v, o.index[i])
		}
	}
	return o.rows.Scan(o.dest...)
}

// All runs the SQL and returns its rows, relations loaded.
func (r RawQuerySet[T]) All(ctx context.Context) ([]T, error) {
	out, err := collectRows(r.Iter(ctx))
	if err != nil {
		return nil, err
	}
	if len(r.prefetch) > 0 && len(out) > 0 {
		err = r.m.prefetchRows(ctx, reflect.ValueOf(out), r.prefetch)
	}
	return out, err
}

// First is the first row the SQL reads, relations loaded; ErrNotFound
// when there is none.
func (r RawQuerySet[T]) First(ctx context.Context) (T, error) {
	for row, err := range r.Iter(ctx) {
		if err != nil {
			return row, err
		}
		rows := []T{row}
		if len(r.prefetch) > 0 {
			if err := r.m.prefetchRows(ctx, reflect.ValueOf(rows), r.prefetch); err != nil {
				return row, err
			}
		}
		return rows[0], nil
	}
	var zero T
	return zero, r.m.notFound()
}

// Raw is the rows sql reads, read into R as Values reads them: a scalar
// for one column, a struct (a field per column, by orm path tag, db or
// column tag, name or column; Option or a pointer for a NULL), map[string]any
// keyed by column, or []any. ? marks each argument (written as the
// database's, $1 on Postgres; values are always bound), ?? is a literal
// ?. A column the struct has no field for is an error, unless Loose.
//
// It reads the default database (its replica, outside a transaction or a
// Scope that wrote it), or the one an option among args names:
// orm.On("analytics"), or a connection handle (a struct embedding
// orm.Conn); Timeout bounds it. Using, transactions and observers apply
// as to any query.
//
//	n, err := orm.Raw[int64](ctx, "SELECT COUNT(*) FROM users WHERE is_active = ?", true)
//	sales, err := orm.Raw[Sale](ctx, "SELECT day, SUM(total) AS total FROM sales GROUP BY day", orm.On("analytics"))
func Raw[R any](ctx context.Context, sql string, args ...any) ([]R, error) {
	return RawOn[R](ctx, DBSchema{}, sql, args...)
}

// RawRow is the one row Raw reads: NotFound when there is none (more are
// dropped).
func RawRow[R any](ctx context.Context, sql string, args ...any) (R, error) {
	for row, err := range RawIter[R](ctx, sql, args...) {
		return row, err
	}
	var zero R
	return zero, errorf(NotFound, "", "orm: the query returned no row")
}

// RawOn is Raw on schema s's database.
//
//	n, err := orm.RawOn[int64](ctx, Legacy, "SELECT COUNT(*) FROM auth_user WHERE is_active = ?", true)
func RawOn[R any](ctx context.Context, s DBSchema, sql string, args ...any) ([]R, error) {
	return collectRows(RawIterOn[R](ctx, s, sql, args...))
}

// RawIter is Raw one row at a time.
func RawIter[R any](ctx context.Context, sql string, args ...any) iter.Seq2[R, error] {
	return RawIterOn[R](ctx, DBSchema{}, sql, args...)
}

// RawIterOn is RawOn one row at a time.
func RawIterOn[R any](ctx context.Context, s DBSchema, sql string, args ...any) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		args, o := rawOptions(ctx, s, args)
		c, err := dbConn(o.ctx, o.db, false)
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := rawQuery(o.ctx, c, sql, args)
		if err != nil {
			yield(zero, err)
			return
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			yield(zero, err)
			return
		}
		rd, err := newReader(reflect.TypeFor[R](), cols, nil, s.Names, o.loose)
		if err != nil {
			rows.Close()
			yield(zero, err)
			return
		}
		readRows(rows, rd, yield)
	}
}

// Exec runs sql, a write or DDL, on the default database (or the one an
// option among args names, as Raw's do), inside ctx's transaction when
// there is one, and says how many rows it changed. A raw write is the
// database's alone: OnChange hears nothing of it and no mirror repeats
// it.
func Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	return ExecOn(ctx, DBSchema{}, sql, args...)
}

// ExecOn is Exec on schema s's database.
func ExecOn(ctx context.Context, s DBSchema, sql string, args ...any) (int64, error) {
	args, o := rawOptions(ctx, s, args)
	c, err := dbConn(o.ctx, o.db, true)
	if err != nil {
		return 0, err
	}
	q, a, err := placeholders(c.d, sql, args)
	if err != nil {
		return 0, err
	}
	res, err := c.exec(o.ctx, q, a)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RawOption tunes Raw and Exec, given among their arguments: Loose (and
// On, Timeout, a connection handle).
type RawOption interface{ rawOption(*rawConfig) }

type rawConfig struct {
	ctx   context.Context
	db    string
	loose bool
}

type looseOption struct{}

func (looseOption) rawOption(c *rawConfig) { c.loose = true }

// Loose has Raw drop the columns its struct has no field for.
func Loose() RawOption { return looseOption{} }

func (t Timeout) rawOption(c *rawConfig) { c.ctx = WithTimeout(c.ctx, time.Duration(t)) }

// rawOptions is args without the options among them, and what they say.
func rawOptions(ctx context.Context, s DBSchema, args []any) ([]any, rawConfig) {
	o := rawConfig{ctx: ctx, db: s.DB}
	var rest []any
	for _, a := range args {
		switch x := a.(type) {
		case RawOption:
			x.rawOption(&o)
		case ForOption:
			var fc forConfig
			x(&fc)
			o.db = fc.db
		case Conn:
			o.ctx, o.db = WithDatabases(o.ctx, x.set), x.name
		default:
			if c, ok := handleConn(a); ok {
				o.ctx, o.db = WithDatabases(o.ctx, c.set), c.name
				continue
			}
			rest = append(rest, a)
		}
	}
	return rest, o
}

// handleConn is the Conn a connection handle (a pointer to a struct
// embedding orm.Conn) holds.
func handleConn(a any) (Conn, bool) {
	v := reflect.ValueOf(a)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return Conn{}, false
	}
	f, ok := v.Elem().Type().FieldByName("Conn")
	if !ok || !f.Anonymous || f.Type != reflect.TypeFor[Conn]() {
		return Conn{}, false
	}
	return v.Elem().FieldByIndex(f.Index).Interface().(Conn), true
}

func rawQuery(ctx context.Context, c conn, s string, args []any) (*sql.Rows, error) {
	q, a, err := placeholders(c.d, s, args)
	if err != nil {
		return nil, err
	}
	return c.query(ctx, q, a)
}

// placeholders is raw SQL with its ? marks written as d's, each sent its
// argument as the ORM's own are (times as UTC); ?? is a literal ?, and
// marks inside quotes and comments are left.
func placeholders(d Dialect, src string, args []any) (string, []any, error) {
	b := &builder{d: d, st: &stmt{}}
	var out strings.Builder
	n := 0
	lexSQL(src, d.Name(), &out, func(i int) int {
		switch {
		case strings.HasPrefix(src[i:], "??"):
			out.WriteByte('?')
			return i + 2
		case src[i] == '?':
			if n < len(args) {
				mark := b.arg(args[n])
				// $1 against a name, a number or a $ would be read with
				// it: as a dollar quote ($$1), an identifier (OFFSET$1)
				// or another mark (?0 as $10).
				if o := out.String(); mark[0] == '$' && o != "" && identByte(o[len(o)-1]) {
					out.WriteByte(' ')
				}
				out.WriteString(mark)
				if mark[0] == '$' && i+1 < len(src) && identByte(src[i+1]) {
					out.WriteByte(' ')
				}
			}
			n++
		default:
			out.WriteByte(src[i])
		}
		return i + 1
	})
	if n != len(args) {
		return "", nil, fmt.Errorf("orm: %d arguments for the %d ? of %q", len(args), n, src)
	}
	return out.String(), b.args(), nil
}
