package orm

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"

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
		c, err := r.m.conn(ctx)
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := rawQuery(ctx, c, r.sql, r.args)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			yield(zero, err)
			return
		}
		m := r.m.meta
		index := make([][]int, len(cols))
		cells := make([]cell, len(cols))
		for i, col := range cols {
			k := strings.ToLower(col)
			f, ok := m.byCol[k]
			if !ok {
				f, ok = m.byName[k]
			}
			if !ok {
				f, ok = m.computed[k]
			}
			if ok {
				index[i], cells[i].fast = f.Index, f.fast
			}
		}
		dest := make([]any, len(cols))
		for i := range cols {
			dest[i] = new(any) // a column no field takes, dropped
			if index[i] != nil {
				dest[i] = &cells[i]
			}
		}
		var row, blank T // read into again and again, yielded by value
		v := reflect.ValueOf(&row).Elem()
		for rows.Next() {
			row = blank
			for i := range cols {
				if index[i] != nil {
					cells[i].dst = fieldOf(v, index[i])
				}
			}
			if err := rows.Scan(dest...); err != nil {
				yield(zero, err)
				return
			}
			r.m.adopt(&row)
			if !yield(row, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

// All runs the SQL and returns its rows, relations loaded.
func (r RawQuerySet[T]) All(ctx context.Context) ([]T, error) {
	out, err := collectRows(r.Iter(ctx))
	if err != nil {
		return nil, err
	}
	return out, r.m.prefetchRows(ctx, out, r.prefetch)
}

// First is the first row the SQL reads, relations loaded; ErrNotFound
// when there is none.
func (r RawQuerySet[T]) First(ctx context.Context) (T, error) {
	for row, err := range r.Iter(ctx) {
		if err != nil {
			return row, err
		}
		rows := []T{row}
		if err := r.m.prefetchRows(ctx, rows, r.prefetch); err != nil {
			return row, err
		}
		return rows[0], nil
	}
	var zero T
	return zero, errorf(NotFound, r.m.meta.Name, "%s matching query does not exist", r.m.meta.Name)
}

// Raw is the rows sql reads on the default database, read into R as Values
// reads them: a scalar for one column, a struct (a field per column, by
// orm path tag, name or column), map[string]any keyed by column, or
// []any. ? marks each argument, ?? is a literal ?.
//
//	n, err := orm.Raw[int64](ctx, "SELECT COUNT(*) FROM users WHERE is_active = ?", true)
func Raw[R any](ctx context.Context, sql string, args ...any) ([]R, error) {
	return RawOn[R](ctx, Schema{}, sql, args...)
}

// RawOn is Raw on schema s's database.
//
//	n, err := orm.RawOn[int64](ctx, Legacy, "SELECT COUNT(*) FROM auth_user WHERE is_active = ?", true)
func RawOn[R any](ctx context.Context, s Schema, sql string, args ...any) ([]R, error) {
	return collectRows(RawIterOn[R](ctx, s, sql, args...))
}

// RawIter is Raw one row at a time.
func RawIter[R any](ctx context.Context, sql string, args ...any) iter.Seq2[R, error] {
	return RawIterOn[R](ctx, Schema{}, sql, args...)
}

// RawIterOn is RawOn one row at a time.
func RawIterOn[R any](ctx context.Context, s Schema, sql string, args ...any) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		c, err := dbConn(ctx, s.DB)
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := rawQuery(ctx, c, sql, args)
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
		rd, err := newReader(reflect.TypeFor[R](), cols, nil, s.Names)
		if err != nil {
			rows.Close()
			yield(zero, err)
			return
		}
		readRows(rows, rd, yield)
	}
}

// Exec runs sql, a write or DDL, on the default database, inside ctx's
// transaction when there is one, and says how many rows it changed. A raw
// write is the database's alone: OnChange hears nothing of it and no
// mirror repeats it.
func Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	return ExecOn(ctx, Schema{}, sql, args...)
}

// ExecOn is Exec on schema s's database.
func ExecOn(ctx context.Context, s Schema, sql string, args ...any) (int64, error) {
	c, err := dbConn(ctx, s.DB)
	if err != nil {
		return 0, err
	}
	q, a, err := placeholders(c.d, sql, args)
	if err != nil {
		return 0, err
	}
	res, err := c.exec(ctx, q, a)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
