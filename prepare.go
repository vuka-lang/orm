package orm

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"sync/atomic"
)

// Param is the n-th argument of a prepared query, from 0: a value its
// execution gives, standing where a filter's value goes.
//
//	var userByEmail = Users.Filter(orm.Q{"email__iexact": orm.Param(0), "active": true}).Prepare()
//	u, err := userByEmail.Get(ctx, email)
//
// It stands for one value: of exact, iexact, the comparisons, the LIKE
// lookups (contains, startswith… its value escaped as theirs is) and a
// registered lookup, or of an expression (orm.Value(orm.Param(1))). A list
// (in, range), isnull and the text searches take a value the SQL is
// written around, and refuse it. A query holding one runs only through
// Prepare.
func Param(n int) Placeholder { return Placeholder{n} }

// Placeholder is an argument of a prepared query, as Param makes it.
type Placeholder struct{ n int }

// slot is a Placeholder among a statement's arguments: conv turns the value an
// execution gives into the argument, as the builder turns a value (a
// field's codec, a LIKE pattern), nil to take it as it is.
type slot struct {
	n    int
	conv func(any) (any, error)
}

// Value refuses a slot reaching the driver: its query ran without Prepare.
func (s slot) Value() (driver.Value, error) {
	return nil, fmt.Errorf("orm: orm.Param(%d) is an argument of a prepared query: run the query through Prepare", s.n)
}

// slotOf is v as a slot, when it is one: a Placeholder, or a slot already.
func slotOf(v any) (slot, bool) {
	switch x := v.(type) {
	case Placeholder:
		return slot{n: x.n}, true
	case slot:
		return x, true
	}
	return slot{}, false
}

// then is s with f after its conversion.
func (s slot) then(f func(any) (any, error)) slot {
	if prev := s.conv; prev != nil {
		s.conv = func(v any) (any, error) {
			x, err := prev(v)
			if err != nil {
				return nil, err
			}
			return f(x)
		}
		return s
	}
	s.conv = f
	return s
}

// slotLookup is whether lookup compares a column to one value, which a
// Param may stand for.
func slotLookup(lookup string) bool {
	switch lookup {
	case "exact", "iexact", "gt", "gte", "lt", "lte",
		"contains", "icontains", "startswith", "istartswith", "endswith", "iendswith":
		return true
	case "in", "range", "isnull", "search", "trigram_similar":
		return false
	}
	_, custom := customLookups.Load(lookup)
	return custom
}

func noSlot(key, lookup string, n int) error {
	return fmt.Errorf("orm: %s: orm.Param(%d) stands for one value, and %s writes its SQL around the value: build that query per call", key, n, lookup)
}

// Prepare is the query compiled once: its SQL is written on its first run
// on each kind of database and kept, and each run only binds the arguments
// its Param slots stand for. Keep one in a package variable; it is immutable
// and safe for concurrent use.
//
//	var postsOf = Posts.Filter(orm.Q{"author_id": orm.Param(0)}).OrderBy("-id").Limit(20).Prepare()
//	rows, err := postsOf.All(ctx, authorID)
//
// A query that can't be written fails each run with its error.
// PrefetchRelated's queries are written per run, as a QuerySet's are, and
// take no Param.
func (qs QuerySet[T]) Prepare() Prepared[T] {
	q := qs.q
	if q == nil {
		q = qs.m.all
	}
	p := &prepared{m: qs.m, prefetch: q.prefetch}
	p.shapes[shapeAll].q = q.ordered()
	p.shapes[shapeFirst].q = q.forFirst().ordered()
	p.shapes[shapeGet].q = q.forGet(nil).ordered()
	return Prepared[T]{p}
}

// Prepared is a query Prepare compiled: run it with the values of its Param
// slots, in order.
type Prepared[T any] struct{ p *prepared }

const (
	shapeAll = iota
	shapeFirst
	shapeGet
)

type prepared struct {
	m        *managerCore
	prefetch []PrefetchSpec
	shapes   [3]preparedShape // by shapeAll, shapeFirst, shapeGet
}

// preparedShape is one terminal's query and its statements, by
// dialectIndex: written on the first run on each.
type preparedShape struct {
	q     query
	stmts [3]atomic.Pointer[compiled]
}

// compiled is a statement with its slots: the indexes of its arguments
// each run fills, and how many values a run gives.
type compiled struct {
	st    selectStmt
	slots []int
	arity int
	err   error
}

func (ps *preparedShape) compiled(d Dialect) *compiled {
	i := dialectIndex(d)
	if i >= 0 {
		if c := ps.stmts[i].Load(); c != nil {
			return c
		}
	}
	c := ps.compile(d)
	if i >= 0 {
		ps.stmts[i].CompareAndSwap(nil, c)
	}
	return c
}

func (ps *preparedShape) compile(d Dialect) *compiled {
	c := &compiled{}
	if c.st, c.err = ps.q.statement(d); c.err != nil {
		return c
	}
	seen := map[int]bool{}
	for i, a := range c.st.args {
		if s, ok := a.(slot); ok {
			c.slots = append(c.slots, i)
			seen[s.n] = true
			c.arity = max(c.arity, s.n+1)
		}
	}
	for n := range c.arity {
		if !seen[n] {
			c.err = fmt.Errorf("orm: the prepared query has orm.Param(%d) but no orm.Param(%d): number its arguments from 0", c.arity-1, n)
			break
		}
	}
	return c
}

// bind is ps's statement on d and its arguments with args in the slots.
func (ps *preparedShape) bind(d Dialect, args []any) (*selectStmt, []any, error) {
	c := ps.compiled(d)
	if c.err != nil {
		return nil, nil, c.err
	}
	if len(args) != c.arity {
		return nil, nil, fmt.Errorf("orm: the prepared query takes %d argument(s), got %d", c.arity, len(args))
	}
	if len(c.slots) == 0 {
		return &c.st, c.st.args, nil
	}
	out := make([]any, len(c.st.args))
	copy(out, c.st.args)
	for _, i := range c.slots {
		s := out[i].(slot)
		v := args[s.n]
		if _, ok := slotOf(v); ok {
			return nil, nil, fmt.Errorf("orm: argument %d of the prepared query is an orm.Param: give it the value", s.n)
		}
		if s.conv != nil {
			var err error
			if v, err = s.conv(v); err != nil {
				return nil, nil, err
			}
		}
		out[i] = dialectArg(d, argValue(v))
	}
	return &c.st, out, nil
}

// Err is what keeps the query from being written on dialect d (a field it
// doesn't have, a misplaced Param): nil when it can be, as every run on d
// would find.
func (p Prepared[T]) Err(d Dialect) error { return p.p.shapes[shapeAll].compiled(d).err }

// All runs the query with args and returns its rows, relations loaded.
func (p Prepared[T]) All(ctx context.Context, args ...any) ([]T, error) {
	return p.rows(ctx, shapeAll, args)
}

// First is the first row, as QuerySet.First reads it; ErrNotFound when
// there is none.
func (p Prepared[T]) First(ctx context.Context, args ...any) (T, error) {
	row, n, err := p.one(ctx, shapeFirst, args)
	if err == nil && n == 0 {
		err = p.p.m.notFound()
	}
	return row, err
}

// Get is the one row: ErrNotFound when there is none, ErrMultipleFound
// when there are several.
func (p Prepared[T]) Get(ctx context.Context, args ...any) (T, error) {
	row, n, err := p.one(ctx, shapeGet, args)
	if err == nil && n != 1 {
		var zero T
		row, err = zero, p.p.m.notOne(n)
	}
	return row, err
}

// one is the first of the shape's rows and how many it read; the zero T
// on an error.
func (p Prepared[T]) one(ctx context.Context, shape int, args []any) (T, int, error) {
	var zero T
	if p.p != nil && len(p.p.prefetch) > 0 {
		rows, err := p.rows(ctx, shape, args)
		if err != nil || len(rows) == 0 {
			return zero, 0, err
		}
		return rows[0], len(rows), nil
	}
	if p.p == nil {
		return zero, 0, errZeroPrepared
	}
	ps := &p.p.shapes[shape]
	var row, first T
	n := 0
	var failed error
	p.p.m.eachRowOf(ctx, &ps.q, ps, args, &row, func(err error) bool {
		if err != nil {
			failed = err
			return false
		}
		if n == 0 {
			first = row
		}
		n++
		return true
	})
	if failed != nil {
		return zero, 0, failed
	}
	return first, n, nil
}

var errZeroPrepared = errors.New("orm: a zero Prepared: make one with QuerySet.Prepare")

// Iter runs the query with args and yields its rows one at a time;
// relations PrefetchRelated names are not loaded.
func (p Prepared[T]) Iter(ctx context.Context, args ...any) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var row, zero T
		ps := &p.p.shapes[shapeAll]
		p.p.m.eachRowOf(ctx, &ps.q, ps, args, &row, func(err error) bool {
			if err != nil {
				return yield(zero, err)
			}
			return yield(row, nil)
		})
	}
}

func (p Prepared[T]) rows(ctx context.Context, shape int, args []any) ([]T, error) {
	if p.p == nil {
		return nil, errZeroPrepared
	}
	ps := &p.p.shapes[shape]
	var out []T
	var row T
	var failed error
	p.p.m.eachRowOf(ctx, &ps.q, ps, args, &row, func(err error) bool {
		if err != nil {
			failed = err
			return false
		}
		out = append(out, row)
		return true
	})
	if failed != nil {
		return nil, failed
	}
	if out == nil {
		out = []T{}
	}
	if len(p.p.prefetch) > 0 && len(out) > 0 {
		if err := p.p.m.prefetchRows(ctx, reflect.ValueOf(out), p.p.prefetch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Prepare1 is Prepare of a query of one Param, its type A.
func (qs QuerySet[T]) Prepare1[A any]() Prepared1[T, A] { return Prepared1[T, A]{qs.Prepare()} }

// Prepare2 is Prepare of a query of two Params, their types A and B.
func (qs QuerySet[T]) Prepare2[A, B any]() Prepared2[T, A, B] {
	return Prepared2[T, A, B]{qs.Prepare()}
}

// Prepare3 is Prepare of a query of three Params, their types A, B and C.
func (qs QuerySet[T]) Prepare3[A, B, C any]() Prepared3[T, A, B, C] {
	return Prepared3[T, A, B, C]{qs.Prepare()}
}

// Prepared1 is a Prepared of one argument, typed.
type Prepared1[T, A any] struct{ p Prepared[T] }

func (p Prepared1[T, A]) All(ctx context.Context, a A) ([]T, error) { return p.p.All(ctx, a) }
func (p Prepared1[T, A]) First(ctx context.Context, a A) (T, error) { return p.p.First(ctx, a) }
func (p Prepared1[T, A]) Get(ctx context.Context, a A) (T, error)   { return p.p.Get(ctx, a) }
func (p Prepared1[T, A]) Iter(ctx context.Context, a A) iter.Seq2[T, error] {
	return p.p.Iter(ctx, a)
}

// Prepared2 is a Prepared of two arguments, typed.
type Prepared2[T, A, B any] struct{ p Prepared[T] }

func (p Prepared2[T, A, B]) All(ctx context.Context, a A, b B) ([]T, error) {
	return p.p.All(ctx, a, b)
}
func (p Prepared2[T, A, B]) First(ctx context.Context, a A, b B) (T, error) {
	return p.p.First(ctx, a, b)
}
func (p Prepared2[T, A, B]) Get(ctx context.Context, a A, b B) (T, error) {
	return p.p.Get(ctx, a, b)
}
func (p Prepared2[T, A, B]) Iter(ctx context.Context, a A, b B) iter.Seq2[T, error] {
	return p.p.Iter(ctx, a, b)
}

// Prepared3 is a Prepared of three arguments, typed.
type Prepared3[T, A, B, C any] struct{ p Prepared[T] }

func (p Prepared3[T, A, B, C]) All(ctx context.Context, a A, b B, c C) ([]T, error) {
	return p.p.All(ctx, a, b, c)
}
func (p Prepared3[T, A, B, C]) First(ctx context.Context, a A, b B, c C) (T, error) {
	return p.p.First(ctx, a, b, c)
}
func (p Prepared3[T, A, B, C]) Get(ctx context.Context, a A, b B, c C) (T, error) {
	return p.p.Get(ctx, a, b, c)
}
func (p Prepared3[T, A, B, C]) Iter(ctx context.Context, a A, b B, c C) iter.Seq2[T, error] {
	return p.p.Iter(ctx, a, b, c)
}
