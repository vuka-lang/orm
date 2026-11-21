package orm

import (
	"context"
	"iter"
	"reflect"
)

// Core is a query of a model without its Go type: the engine a QuerySet[T]
// runs on, compiled once, in this package. orm generate writes a set over
// it for each model it scans — UserQuery and UserQueries beside User — whose
// methods are plain methods of the models' package:
//
//	users, err := models.UserQueries.Filter(models.UserColumns.Active.Eq(true)).All(ctx)
//
// A package querying through a set instantiates nothing of the ORM for the
// model, where one naming QuerySet[User] (orm.Objects[User]() included)
// compiles QuerySet's methods for User again, as Go compiles a generic
// type's methods in each package using it. Both query the same rows, with
// the same rules: a set is a QuerySet by another name, and its QuerySet
// method converts.
//
// The zero Core is no query: CoreOf makes one.
type Core struct {
	c querySetCore
	t reflect.Type // the model, while c is unresolved: the home or s's
	s *DBSchema
}

// CoreOf is the query of T's rows that Objects[T](s...) is: resolved on
// first use, so a set in a package variable binds the manager For or
// Objects made by then.
func CoreOf[T any](s ...DBSchema) Core {
	c := Core{t: reflect.TypeFor[T]()}
	if len(s) > 0 {
		c.s = &s[0]
	}
	return c
}

// CoreFrom is q's query as a Core: a QuerySet's, a Manager's, a Values'
// or a set's.
func CoreFrom(q interface{ core() querySetCore }) Core { return Core{c: q.core()} }

// QuerySetOf is c as a QuerySet[T]; it panics when c isn't a query of T.
func QuerySetOf[T any](c Core) QuerySet[T] {
	qs := c.qs()
	if qs.m != nil && qs.m.rowType != reflect.TypeFor[T]() {
		panic("orm: QuerySetOf[" + reflect.TypeFor[T]().String() + "] of a query of " + qs.m.rowType.String())
	}
	return QuerySet[T]{qs}
}

// qs is c's query, its model's home (or schema's) when it has none yet.
//
//go:noinline
func (c Core) qs() querySetCore {
	if c.c.m != nil || c.t == nil {
		return c.c
	}
	registerType(c.t)
	var m *managerCore
	if c.s == nil {
		m = homeCore(c.t)
	} else {
		m = coreFor(c.t, *c.s)
	}
	return m.core()
}

func (c Core) core() querySetCore { return c.qs() }

func (c Core) with(q *query) Core {
	qs := c.qs()
	qs.q = q
	return Core{c: qs}
}

func (c Core) query() *query {
	q := c.qs().q
	if q == nil {
		q = &query{}
	}
	return q
}

// Err is what keeps the ORM from mapping the model: nil when it can.
func (c Core) Err() error {
	if qs := c.qs(); qs.m != nil {
		return qs.m.err
	}
	return nil
}

// Info is QuerySet's Info.
func (c Core) Info() ModelInfo { return c.qs().m.Info() }

// TableName is the model's table.
func (c Core) TableName() string { return c.qs().m.TableName() }

// PKColumn is the column of the model's primary key, "" for none.
func (c Core) PKColumn() string { return c.qs().m.PKColumn() }

// Columns is the model's columns, in field order.
func (c Core) Columns() []string { return c.qs().m.Columns() }

// Fields is the model's columns, in field order, as forms and tools read
// them.
func (c Core) Fields() []FieldInfo { return c.qs().m.Fields() }

// Filter is QuerySet's Filter.
func (c Core) Filter(conds ...Cond) Core { return c.with(c.query().filter(conds)) }

// Exclude is QuerySet's Exclude.
func (c Core) Exclude(conds ...Cond) Core { return c.with(c.query().exclude(conds)) }

// Annotate is QuerySet's Annotate.
func (c Core) Annotate(value any, es ...Expr) Core { return c.with(c.query().annotate(value, es)) }

// AnnotateAs is QuerySet's AnnotateAs.
func (c Core) AnnotateAs(as ...Alias) Core { return c.with(c.query().annotateAs(as)) }

// GroupBy is QuerySet's GroupBy.
func (c Core) GroupBy(names ...any) Core { return c.with(c.query().groupedBy(names)) }

// Having is QuerySet's Having.
func (c Core) Having(conds ...Cond) Core { return c.with(c.query().havingConds(conds)) }

// OrderBy is QuerySet's OrderBy.
func (c Core) OrderBy(fields ...any) Core { return c.with(c.query().orderBy(fields)) }

// Order is QuerySet's Order.
func (c Core) Order(terms ...OrderTerm) Core { return c.with(c.query().orderTerms(terms)) }

// Limit is QuerySet's Limit.
func (c Core) Limit(n int) Core { return c.with(c.query().limited(n)) }

// Offset is QuerySet's Offset.
func (c Core) Offset(n int) Core { return c.with(c.query().offsetBy(n)) }

// Distinct is QuerySet's Distinct.
func (c Core) Distinct() Core { return c.with(c.query().distinctRows()) }

// SelectRelated is QuerySet's SelectRelated.
func (c Core) SelectRelated(paths ...any) Core { return c.with(c.query().selectRelated(paths)) }

// PrefetchRelated is QuerySet's PrefetchRelated.
func (c Core) PrefetchRelated(specs ...any) Core {
	return c.with(c.query().prefetchRelated(specs))
}

// Join is QuerySet's Join.
func (c Core) Join(j Joinable, kind ...JoinKind) Core { return c.with(c.query().join(j, kind)) }

// Unfiltered is QuerySet's Unfiltered.
func (c Core) Unfiltered() Core { return c.with(c.query().unfiltered()) }

// Nearest is QuerySet's Nearest.
func (c Core) Nearest(field string, v Vector, opts ...SearchOption) Core {
	return c.with(c.query().nearest(field, v, opts))
}

// Hybrid is QuerySet's Hybrid.
func (c Core) Hybrid(doc Document, text TextQuery, field string, v Vector, opts ...SearchOption) Core {
	return c.with(c.query().hybrid(doc, text, field, v, opts))
}

// Count is QuerySet's Count.
func (c Core) Count(ctx context.Context) (int64, error) { return c.qs().Count(ctx) }

// Exists is QuerySet's Exists.
func (c Core) Exists(ctx context.Context) (bool, error) { return c.qs().Exists(ctx) }

// Delete is QuerySet's Delete.
func (c Core) Delete(ctx context.Context) (int64, error) { return c.qs().Delete(ctx) }

// Update is QuerySet's Update.
func (c Core) Update(ctx context.Context, values Set) (int64, error) {
	qs := c.qs()
	return qs.m.update(ctx, qs.q, values)
}

// Aggregate is QuerySet's Aggregate.
func (c Core) Aggregate(ctx context.Context, aggs ...Agg) (Result, error) {
	qs := c.qs()
	return qs.m.aggregate(ctx, qs.q, aggs)
}

// SQL is QuerySet's SQL.
func (c Core) SQL(ctx context.Context) (string, []any, error) { return c.qs().SQL(ctx) }

// Explain is QuerySet's Explain.
func (c Core) Explain(ctx context.Context, opts ...ExplainOption) (Plan, error) {
	return c.qs().Explain(ctx, opts...)
}

// CountSQL is QuerySet's CountSQL.
func (c Core) CountSQL(ctx context.Context) (Statement, error) { return c.qs().CountSQL(ctx) }

// ExistsSQL is QuerySet's ExistsSQL.
func (c Core) ExistsSQL(ctx context.Context) (Statement, error) { return c.qs().ExistsSQL(ctx) }

// DeleteSQL is QuerySet's DeleteSQL.
func (c Core) DeleteSQL(ctx context.Context) (Statement, error) { return c.qs().DeleteSQL(ctx) }

// UpdateSQL is QuerySet's UpdateSQL.
func (c Core) UpdateSQL(ctx context.Context, values Set) (Statement, error) {
	qs := c.qs()
	return qs.m.updateStatement(ctx, qs.q, values)
}

// AggregateSQL is QuerySet's AggregateSQL.
func (c Core) AggregateSQL(ctx context.Context, aggs ...Agg) (Statement, error) {
	qs := c.qs()
	return qs.m.aggregateStatement(ctx, qs.q, aggs)
}

// Rows runs the query, reading each row into rowp (a *T, zeroed first) and
// calling add after it; the first error stops it. A set's All appends in
// add, then calls Prefetch.
func (c Core) Rows(ctx context.Context, rowp any, add func()) error {
	qs := c.qs()
	var failed error
	qs.m.eachRow(ctx, qs.q, rowp, func(err error) bool {
		if err != nil {
			failed = err
			return false
		}
		add()
		return true
	})
	return failed
}

// Each runs the query, reading each row into rowp (a *T, zeroed first) and
// calling each after it, or with the error that stops it; each returning
// false stops it. Relations PrefetchRelated names are not loaded, as Iter
// loads none.
func (c Core) Each(ctx context.Context, rowp any, each func(error) bool) {
	qs := c.qs()
	qs.m.eachRow(ctx, qs.q, rowp, each)
}

// Prefetch loads the relations PrefetchRelated named into rows, a []T All
// read.
func (c Core) Prefetch(ctx context.Context, rows any) error {
	qs := c.qs()
	if len(qs.q.prefetch) == 0 {
		return nil
	}
	rv := reflect.ValueOf(rows)
	if rv.Len() == 0 {
		return nil
	}
	return qs.m.prefetchRows(ctx, rv, qs.q.prefetch)
}

// First reads QuerySet's First into dst, a *T: its zero on an error.
func (c Core) First(ctx context.Context, dst any) error {
	qs := c.qs()
	return qs.m.one(ctx, qs.q.forFirst(), dst, false)
}

// Get reads QuerySet's Get into dst, a *T: its zero on an error.
func (c Core) Get(ctx context.Context, dst any, conds ...Cond) error {
	qs := c.qs()
	return qs.m.one(ctx, qs.q.forGet(conds), dst, true)
}

// one reads q's row into dst, a *T: the only one when exactly, else the
// first, relations loaded; dst's zero on an error.
func (m *managerCore) one(ctx context.Context, q *query, dst any, exactly bool) error {
	n := 0
	var failed error
	if len(q.prefetch) > 0 {
		var rows any
		if rows, failed = m.rows(ctx, q); failed == nil {
			rv := reflect.ValueOf(rows)
			if n = rv.Len(); n > 0 {
				reflect.ValueOf(dst).Elem().Set(rv.Index(0))
			}
		}
	} else {
		m.eachRow(ctx, q, dst, func(err error) bool {
			failed = err
			n++
			return err == nil && (exactly || n < 1)
		})
	}
	switch {
	case failed != nil:
	case n == 0:
		failed = m.notFound()
	case exactly && n != 1:
		failed = m.notOne(n)
	}
	if failed != nil {
		reflect.ValueOf(dst).Elem().SetZero()
	}
	return failed
}

// Create is QuerySet's Create of row, a *T.
func (c Core) Create(ctx context.Context, row any) error { return c.qs().m.create(ctx, row) }

// BulkCreate is QuerySet's BulkCreate of rows, each a *T.
func (c Core) BulkCreate(ctx context.Context, rows ...any) error {
	return c.qs().m.bulkCreate(ctx, rows)
}

// Save is QuerySet's Save of row, a *T.
func (c Core) Save(ctx context.Context, row any) error { return c.qs().m.saveRow(ctx, row) }

// Remove is QuerySet's Remove of row, a *T.
func (c Core) Remove(ctx context.Context, row any) error { return c.qs().m.remove(ctx, row) }

// GetOrCreate is QuerySet's GetOrCreate into row, a *T.
func (c Core) GetOrCreate(ctx context.Context, row any, lookup Q, defaults Defaults) (created bool, err error) {
	return c.qs().m.getOrCreate(ctx, row, lookup, defaults)
}

// UpdateOrCreate is QuerySet's UpdateOrCreate into row, a *T.
func (c Core) UpdateOrCreate(ctx context.Context, row any, lookup Q, defaults Defaults) (created bool, err error) {
	return c.qs().m.updateOrCreate(ctx, row, lookup, defaults)
}

// ValuesOf is QuerySet's Values of a set: q's fields read into R.
func ValuesOf[T, R any](c Core, fields ...any) Values[T, R] {
	qs := c.qs()
	var q query
	if qs.q != nil {
		q = *qs.q
	}
	q.proj = &projection{fields}
	return Values[T, R]{querySetCore{qs.m, &q}}
}

// IterOf is a set's Iter: the rows of c, a query of T, one at a time.
func IterOf[T any](ctx context.Context, c Core) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var row, zero T
		c.Each(ctx, &row, func(err error) bool {
			if err != nil {
				return yield(zero, err)
			}
			return yield(row, nil)
		})
	}
}
