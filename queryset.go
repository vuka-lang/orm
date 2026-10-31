package orm

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"github.com/vuka-lang/vuka/v2"
)

// QuerySet is a lazy, immutable query of a model: each method returns a
// new QuerySet, and nothing runs until a terminal (All, First, Get,
// Count, Exists, Iter, Delete, Update, Aggregate). Keep one in a package
// variable and share it freely.
type QuerySet[T any] struct{ querySetCore }

// querySetCore is a QuerySet without its Go type, and those of its methods
// that neither mention T nor take a query's columns (vukavet checks a query's
// chain by the methods declared on QuerySet, so a method taking columns stays
// there). Keep the method set minimal: the Go compiler writes a wrapper of each
// per model, in every package using one.
type querySetCore struct {
	m *managerCore
	q *query // never changed once a QuerySet holds it: each method makes a new one
}

func (qs querySetCore) prefetchQuery() query { return *qs.q }

// Annotate adds a computed column, named: Filter, OrderBy and Values use
// it by name, and a field of the model tagged orm:"computed" with the
// same name receives it.
//
//	Users.Annotate("joined", orm.Year.Of(orm.F("created_at"))).Filter(orm.Q{"joined__gte": 2025})
func (qs QuerySet[T]) Annotate(value any, es ...Expr) QuerySet[T] {
	qs.q = qs.q.annotate(value, es)
	return qs
}

// AnnotateAs is Annotate of expressions named by their As.
//
//	Users.AnnotateAs(orm.Length(orm.F("name")).As("name_len")).Filter(orm.Q{"name_len__gt": 3})
func (qs QuerySet[T]) AnnotateAs(as ...Alias) QuerySet[T] {
	qs.q = qs.q.annotateAs(as)
	return qs
}

// Filter keeps the rows matching every condition. Keys may follow
// relations: author__name (a join), posts__title__icontains (a related
// row exists).
func (qs QuerySet[T]) Filter(conds ...Cond) QuerySet[T] {
	qs.q = qs.q.filter(conds)
	return qs
}

// Exclude drops the rows matching all the conditions.
func (qs QuerySet[T]) Exclude(conds ...Cond) QuerySet[T] {
	qs.q = qs.q.exclude(conds)
	return qs
}

// GroupBy groups the rows by names (fields, paths through relations,
// annotations): the GROUP BY of a Values listing aggregates, over the
// implicit rule (every other name Values lists). Grouping by a primary
// key lets Values read the columns depending on it without grouping by
// them, as Postgres and MySQL allow:
//
//	Letters.Annotate("n", orm.Count("placements__id")).GroupBy("id").
//		Values[[]any]("id", "reference_number", "status", "n")
//
// Typed fields name them too: GroupBy(CategoryColumns.ID).
func (qs QuerySet[T]) GroupBy(names ...any) QuerySet[T] {
	qs.q = qs.q.groupedBy(names)
	return qs
}

// Having keeps the groups matching every condition: HAVING, run after
// grouping, where the aggregates exist. A grouped query only (Values
// listing an aggregate, or GroupBy); Filter on an aggregate annotation
// routes to HAVING too, as Django's does.
//
//	Letters.Annotate("n", orm.Count("placements__id")).Having(orm.Q{"n__gt": 0}).Values[…]
func (qs QuerySet[T]) Having(conds ...Cond) QuerySet[T] {
	qs.q = qs.q.havingConds(conds)
	return qs
}

// OrderBy sorts by fields, a leading - for descending: OrderBy("-age",
// "author__name"); typed fields, ascending unless Desc (OrderBy(
// CategoryColumns.Name, ProductColumns.Price.Desc())); or expressions and
// their Asc and Desc. A query that sets no order reads rows in the
// model's Meta Ordering; OrderBy() sets none, that one included.
func (qs QuerySet[T]) OrderBy(fields ...any) QuerySet[T] {
	qs.q = qs.q.orderBy(fields)
	return qs
}

// Order sorts by terms, as OrderBy: an expression's Asc or Desc, or an
// Ordering (a field path) — mixed as needed.
//
//	Users.Order(Levenshtein(orm.F("name"), "jon").Asc(), orm.Ordering{Path: []string{"age"}, Desc: true})
func (qs QuerySet[T]) Order(terms ...OrderTerm) QuerySet[T] {
	qs.q = qs.q.orderTerms(terms)
	return qs
}

// Limit keeps at most n rows.
func (qs QuerySet[T]) Limit(n int) QuerySet[T] { qs.q = qs.q.limited(n); return qs }

// Offset skips the first n rows.
func (qs QuerySet[T]) Offset(n int) QuerySet[T] { qs.q = qs.q.offsetBy(n); return qs }

// Distinct drops duplicate rows.
func (qs QuerySet[T]) Distinct() QuerySet[T] { qs.q = qs.q.distinctRows(); return qs }

// SelectRelated reads foreign keys in the same query, one LEFT JOIN each:
// SelectRelated("author", "author__profile"). A missing related row
// leaves the field nil (or zero).
func (qs QuerySet[T]) SelectRelated(paths ...any) QuerySet[T] {
	qs.q = qs.q.selectRelated(paths)
	return qs
}

// PrefetchRelated loads relations of any kind after the rows, one query
// each (per thousand keys): names, paths (posts__comments) or Prefetch
// with a query of your own. Only All, First and Get load them; Iter
// streams rows without.
func (qs QuerySet[T]) PrefetchRelated(specs ...any) QuerySet[T] {
	qs.q = qs.q.prefetchRelated(specs)
	return qs
}

func appendSpecs(to []PrefetchSpec, specs []any) []PrefetchSpec {
	for _, s := range specs {
		switch v := s.(type) {
		case string:
			to = append(to, PrefetchSpec{path: v})
		case PrefetchSpec:
			to = append(to, v)
		case fmt.Stringer: // a typed relation: PostColumns.Tags
			to = append(to, PrefetchSpec{path: v.String()})
		default:
			to = append(to, PrefetchSpec{path: fmt.Sprintf("%v", s), qs: badPrefetch{s}})
		}
	}
	return to
}

type badPrefetch struct{ v any }

func (b badPrefetch) prefetchQuery() query { return query{} }

// Iter runs the query and yields its rows one at a time, without holding
// them all; relations PrefetchRelated names are not loaded.
func (qs QuerySet[T]) Iter(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) { iterate(ctx, qs.querySetCore, yield) }
}

func iterate[T any](ctx context.Context, qs querySetCore, yield func(T, error) bool) {
	var zero T
	o := qs.m.open(ctx, qs.q)
	defer o.close()
	if o.ran != nil {
		next := yield
		yield = func(row T, err error) bool {
			o.refused(err)
			return next(row, err)
		}
	}
	if o.err != nil {
		yield(zero, o.err)
		return
	}
	if o.rows != nil {
		sc := o.scanner.(func() RowScanner[T])()
		dest := sc.Dest()
		// One row read into again and again, yielded by value: zeroed
		// first, so nothing of the one before shows through.
		var row, blank T
		for o.rows.Next() {
			row = blank
			sc.Bind(&row)
			if err := o.rows.Scan(dest...); err != nil {
				yield(zero, err)
				return
			}
			qs.m.adoptAt(unsafe.Pointer(&row))
			if !yield(row, nil) {
				return
			}
		}
		if err := o.rows.Err(); err != nil {
			yield(zero, err)
		}
		return
	}
	stopped := false
	err := o.scan(func(v reflect.Value) bool {
		row := v.Addr().Interface().(*T)
		qs.m.adoptAt(unsafe.Pointer(row))
		if !yield(*row, nil) {
			stopped = true
			return false
		}
		return true
	})
	if err != nil && !stopped {
		yield(zero, err)
	}
}

// opened is a query's rows, open to read: rows and scanner when a generated
// scanner reads them, else scan reads them by reflection; err what stopped
// it opening. ctx is what the query runs in and ran what says it ran.
type opened struct {
	m       *managerCore
	ctx     context.Context
	ran     *bool
	err     error
	rows    *sql.Rows
	scanner any // func() RowScanner[T]
	end     func()
	c       conn
	q       *query // the ordered query, when scan reads it
}

func (o *opened) refused(err error) { refused(o.ctx, o.m.name(), o.ran, err) }

func (o *opened) close() {
	if o.rows != nil {
		o.rows.Close()
	}
	if o.end != nil {
		o.end()
	}
}

// open starts q's rows.
func (m *managerCore) open(ctx context.Context, q *query) *opened {
	o := &opened{m: m}
	o.ctx, o.ran = runs(ctx)
	ctx = o.ctx
	c, err := m.conn(ctx, false)
	if err != nil {
		o.err = err
		return o
	}
	c, o.end, err = q.tune(ctx, c)
	if err != nil {
		o.err = err
		return o
	}
	o.c = c
	ordered := q.ordered()
	if mk := scannerMaker(ordered.m, m.rowType); mk != nil && len(ordered.computedFields()) == 0 && len(ordered.related) == 0 && !ordered.m.perSchema() {
		b := ordered.builder(c.d)
		cols, err := ordered.columns(b, nil)
		if err != nil {
			o.err = err
			return o
		}
		s, err := ordered.selectSQL(b, cols)
		if err != nil {
			o.err = err
			return o
		}
		o.rows, o.err = c.query(ctx, s, b.args())
		o.scanner = mk
		return o
	}
	o.q = new(query)
	*o.q = ordered
	return o
}

// scan reads the rows by reflection, into one row again and again.
func (o *opened) scan(each func(v reflect.Value) bool) error {
	reuse := reflect.New(o.q.m.Type).Elem()
	return o.q.scan(o.ctx, o.c, o.m.schema(), func() reflect.Value { reuse.SetZero(); return reuse }, each)
}

// All runs the query and returns its rows, relations loaded.
func (qs QuerySet[T]) All(ctx context.Context) ([]T, error) {
	out := []T{}
	var failed error
	iterate(ctx, qs.querySetCore, func(row T, err error) bool {
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
	if len(qs.q.prefetch) > 0 && len(out) > 0 {
		if err := qs.m.prefetchRows(ctx, reflect.ValueOf(out), qs.q.prefetch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prefetchRows loads specs' relations into rows, a []T.
func (m *managerCore) prefetchRows(ctx context.Context, rows reflect.Value, specs []PrefetchSpec) error {
	c, err := m.conn(ctx, false)
	if err != nil {
		return err
	}
	parents := make([]reflect.Value, rows.Len())
	for i := range parents {
		parents[i] = rows.Index(i)
	}
	return prefetch(ctx, c, m.meta, m.schema(), parents, specs)
}

// First is the first row, by the order given (or Meta's) or else by primary key;
// ErrNotFound when there is none.
func (qs QuerySet[T]) First(ctx context.Context) (T, error) {
	qs.q = qs.q.forFirst()
	rows, err := qs.All(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	if len(rows) == 0 {
		var zero T
		return zero, qs.m.notFound()
	}
	return rows[0], nil
}

// forFirst is q reading its first row.
func (q *query) forFirst() *query {
	if q.ordered().order == nil && q.m != nil && q.m.PK != nil {
		q = q.orderBy([]any{q.m.PK.Name})
	}
	return q.limited(1)
}

// Optional is a row First or Get found as an Option, None when ErrNotFound
// says there is none; other errors stay errors:
//
//	p, err := orm.Optional(orm.Objects[Post]().Filter(f.Slug.Eq(slug)).First(ctx))
//	if err != nil { … }
//	if post, ok := p.Get(); ok { … }
func Optional[T any](row T, err error) (vuka.Option[T], error) {
	switch {
	case errors.Is(err, ErrNotFound):
		return vuka.None[T](), nil
	case err != nil:
		return vuka.None[T](), err
	}
	return vuka.Some(row), nil
}

// Get is the one row matching conds: ErrNotFound when there is none,
// ErrMultipleFound when there are several.
func (qs QuerySet[T]) Get(ctx context.Context, conds ...Cond) (T, error) {
	qs.q = qs.q.forGet(conds)
	rows, err := qs.All(ctx)
	if err == nil && len(rows) != 1 {
		err = qs.m.notOne(len(rows))
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return rows[0], nil
}

func (m *managerCore) name() string {
	if m.meta == nil {
		return m.rowType.Name()
	}
	return m.meta.Name
}

func (m *managerCore) notFound() error {
	return errorf(NotFound, m.name(), "%s matching query does not exist", m.name())
}

// notOne is the error of a Get that read n rows, not one.
func (m *managerCore) notOne(n int) error {
	if n == 0 {
		return m.notFound()
	}
	return errorf(MultipleFound, m.name(), "get() returned more than one %s", m.name())
}

// Count is how many rows match.
func (qs querySetCore) Count(ctx context.Context) (int64, error) { return qs.m.count(ctx, qs.q) }

func (m *managerCore) count(ctx context.Context, qq *query) (_ int64, err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	c, err := m.conn(ctx, false)
	if err != nil {
		return 0, err
	}
	s, args, err := qq.countSQL(c.d)
	if err != nil {
		return 0, err
	}
	var n int64
	err = scanOne(ctx, c, s, args, &n)
	return n, err
}

func (qq *query) countSQL(d Dialect) (string, []any, error) {
	q := qq.clone()
	q.order = nil
	b := q.builder(d)
	if q.limit > 0 || q.offset > 0 || q.distinct {
		col := q.m.Fields[0]
		if q.m.PK != nil {
			col = q.m.PK
		}
		// Written once: each writing adds the conditions' arguments.
		sub, err := q.selectSQL(b, b.col(col))
		if err != nil {
			return "", nil, err
		}
		return "SELECT COUNT(*) FROM (" + sub + ") AS orm_count", b.args(), nil
	}
	w, err := q.whereSQL(b)
	if err != nil {
		return "", nil, err
	}
	from, err := b.from(0)
	if err != nil {
		return "", nil, err
	}
	return "SELECT COUNT(*) FROM " + from + w, b.args(), nil
}

// Exists is whether any row matches.
func (qs querySetCore) Exists(ctx context.Context) (bool, error) { return qs.m.exists(ctx, qs.q) }

func (m *managerCore) exists(ctx context.Context, q *query) (_ bool, err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	c, err := m.conn(ctx, false)
	if err != nil {
		return false, err
	}
	s, args, err := q.existsSQL(c.d)
	if err != nil {
		return false, err
	}
	rows, err := c.query(ctx, s, args)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func (q *query) existsSQL(d Dialect) (string, []any, error) {
	b := q.builder(d)
	w, err := q.whereSQL(b)
	if err != nil {
		return "", nil, err
	}
	from, err := b.from(0)
	if err != nil {
		return "", nil, err
	}
	return "SELECT 1 FROM " + from + w + " LIMIT 1", b.args(), nil
}

func scanOne(ctx context.Context, c conn, s string, args []any, dst any) error {
	rows, err := c.query(ctx, s, args)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	if err := rows.Scan(&cell{dst: reflect.ValueOf(dst).Elem()}); err != nil {
		return err
	}
	return rows.Err()
}

// ErrUnfiltered is the error of an Update or Delete with no condition:
// one that would change every row of the table. Unfiltered says that is
// what is meant.
var ErrUnfiltered = errors.New("orm: no condition")

// Unfiltered lets Update and Delete reach every row the query has: without
// it, an Update or Delete whose conditions are empty (no Filter, or one of
// nothing, such as an empty Q) fails with ErrUnfiltered and runs nothing.
//
//	Sessions.Unfiltered().Delete(ctx) // every row, on purpose
func (qs QuerySet[T]) Unfiltered() QuerySet[T] {
	qs.q = qs.q.unfiltered()
	return qs
}

// keys is the primary keys of the rows q matches.
func (m *managerCore) keys(ctx context.Context, q *query) ([]any, error) {
	rows, err := m.typed.rows(ctx, q)
	if err != nil {
		return nil, err
	}
	pk := q.m.PK
	rv := reflect.ValueOf(rows)
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = value(peek(rv.Index(i), pk.Index, pk.Type))
	}
	return out, nil
}

// guardEvery refuses a write that would reach every row by accident.
func (q *query) guardEvery(op, where string) error {
	if where != "" || q.every {
		return nil
	}
	return fmt.Errorf("%w: %s of %s would reach every row; filter it, or call Unfiltered() to mean every row", ErrUnfiltered, op, q.m.Name)
}

// Delete removes the matching rows and says how many. With no condition it
// fails with ErrUnfiltered unless the query is Unfiltered.
func (qs querySetCore) Delete(ctx context.Context) (int64, error) { return qs.m.delete(ctx, qs.q) }

func (m *managerCore) delete(ctx context.Context, q *query) (_ int64, err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	c, err := m.conn(ctx, true)
	if err != nil {
		return 0, err
	}
	s, args, err := q.deleteSQL(c.d)
	if err != nil {
		return 0, err
	}
	var gone any
	if !quiet(ctx) && m.listening() {
		if gone, err = m.typed.rows(Primary(ctx), q); err != nil {
			return 0, err
		}
	}
	res, err := c.exec(ctx, s, args)
	if err != nil {
		return 0, m.mapErr(c, err)
	}
	m.mirrored(ctx, c, "delete", func(ctx context.Context, mm *managerCore) error {
		_, err := mm.delete(ctx, q.on(mm.meta))
		return err
	})
	n, err := res.RowsAffected()
	if err == nil && n > 0 && !quiet(ctx) {
		m.changed(ctx, c, changeEvent{kind: BulkDeleted, count: n, rows: gone})
	}
	return n, err
}

func (q *query) deleteSQL(d Dialect) (string, []any, error) {
	if q.limit > 0 || q.offset > 0 {
		return "", nil, fmt.Errorf("orm: can't delete a sliced %s query", q.m.Name)
	}
	b := q.builder(d)
	w, err := q.writeWhere(b)
	if err != nil {
		return "", nil, err
	}
	if err := q.guardEvery("Delete", w); err != nil {
		return "", nil, err
	}
	return "DELETE FROM " + d.Quote(q.m.Table) + w, b.args(), nil
}

// Set is the fields an Update writes, by field name.
type Set map[string]any

// Update writes values to the matching rows and says how many; fields
// marked auto_now are set to now too. With no condition it fails with
// ErrUnfiltered unless the query is Unfiltered.
func (qs QuerySet[T]) Update(ctx context.Context, values Set) (int64, error) {
	return qs.m.update(ctx, qs.q, values)
}

func (m *managerCore) update(ctx context.Context, q *query, values Set) (_ int64, err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	c, err := m.conn(ctx, true)
	if err != nil {
		return 0, err
	}
	s, args, stamped, err := q.updateSQL(c.d, values)
	if err != nil || s == "" {
		return 0, err
	}
	var touched []any
	if !quiet(ctx) && m.listening() && q.m.PK != nil {
		if touched, err = m.keys(ctx, q); err != nil {
			return 0, err
		}
	}
	res, err := c.exec(ctx, s, args)
	if err != nil {
		return 0, m.mapErr(c, err)
	}
	m.mirrored(ctx, c, "update", func(ctx context.Context, mm *managerCore) error {
		_, err := mm.update(ctx, q.on(mm.meta), stamped)
		return err
	})
	n, err := res.RowsAffected()
	if err == nil && n > 0 && !quiet(ctx) {
		m.changed(ctx, c, changeEvent{kind: BulkUpdated, count: n, keys: touched})
	}
	return n, err
}

// updateSQL is Update's statement, "" when it sets nothing, and the
// values with the auto_now fields' stamps, for a mirror to write alike.
func (qq *query) updateSQL(d Dialect, values Set) (string, []any, Set, error) {
	if qq.limit > 0 || qq.offset > 0 {
		return "", nil, nil, fmt.Errorf("orm: can't update a sliced %s query", qq.m.Name)
	}
	m := qq.m
	b := qq.builder(d)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sets []string
	seen := map[*field]bool{}
	var invalid map[string][]string
	for _, k := range keys {
		f, ok := m.field(k)
		if !ok {
			return "", nil, nil, fmt.Errorf("orm: %s has no field %q", qq.m.Name, k)
		}
		if f.Via != "" {
			parts := strings.Split(f.Via, "__")
			owner := m
			for _, p := range parts[:len(parts)-1] {
				if r, ok := owner.relation(p); ok {
					if t, _, err := r.ends(); err == nil {
						owner = t
					}
				}
			}
			return "", nil, nil, fmt.Errorf("orm: %s is read through %s: write %s", f.Name, parts[0], owner.Name)
		}
		if f.Gen != nil {
			return "", nil, nil, fmt.Errorf("orm: %s.%s is generated by the database: it can't be written", m.Name, f.Name)
		}
		seen[f] = true
		given := f.normValue(values[k])
		if msgs := f.problems(given); len(msgs) > 0 {
			if invalid == nil {
				invalid = map[string][]string{}
			}
			invalid[f.Column] = append(invalid[f.Column], msgs...)
			continue
		}
		x, err := m.out(d, f, given)
		if err != nil {
			return "", nil, nil, err
		}
		val, err := b.operand(x)
		if err != nil {
			return "", nil, nil, err
		}
		sets = append(sets, b.bare(f)+" = "+val)
	}
	if err := m.invalid(invalid); err != nil {
		return "", nil, nil, err
	}
	stamped := values
	for _, f := range m.Fields {
		if f.AutoNow && !seen[f] {
			now := stamp(f)
			x, err := m.out(d, f, now)
			if err != nil {
				return "", nil, nil, err
			}
			sets = append(sets, b.bare(f)+" = "+b.arg(x))
			if len(stamped) == len(values) {
				stamped = maps.Clone(values)
			}
			stamped[f.Name] = now
		}
	}
	if len(sets) == 0 {
		return "", nil, nil, nil
	}
	if len(*b.joins) > 0 {
		return "", nil, nil, fmt.Errorf("orm: Update of %s can't set values read through a relation", qq.m.Name)
	}
	w, err := qq.writeWhere(b)
	if err != nil {
		return "", nil, nil, err
	}
	if err := qq.guardEvery("Update", w); err != nil {
		return "", nil, nil, err
	}
	return "UPDATE " + d.Quote(m.Table) + " SET " + strings.Join(sets, ", ") + w, b.args(), stamped, nil
}

// Values is the query reading some fields into R, Django's values and
// values_list. A name is a field, an annotation, or a path through
// relations (team__name), joined in the same query; through rows held by
// many, a row per related row. R is a scalar for one name
// (Values[string]("email")); a struct, its fields matched by orm path tag
// (orm:"team__name"), name or column, and with no names its own fields
// say what is read; map[string]any keyed by the names as written; or
// []any in their order. An aggregate annotation among the names groups
// the rows by the others:
//
//	Users.Annotate("n", orm.Count("id")).Values[map[string]any]("team__name", "n")
//
// Typed columns fill R by position: its fields in order, a list's items,
// a scalar for one; a typed field (of the model or one it joins), an
// aggregate (orm.Count(ProductColumns.ID)) or any expression each:
//
//	Category.Objects.Join(…).Values[CategoryProducts](CategoryColumns.ID, CategoryColumns.Name, orm.Count(ProductColumns.ID))
func (qs QuerySet[T]) Values[R any](fields ...any) Values[T, R] {
	return Values[T, R]{q: qs, cols: fields}
}

// Values is a query of some fields of T, read into R.
type Values[T, R any] struct {
	q    QuerySet[T]
	cols []any
}

// Filter, Exclude, Having, OrderBy, Annotate, Limit, Offset and Distinct narrow
// the projection and stay typed to R — Django's post-.values() chain: project
// first, then filter/order/limit the projection, and the result is []R. Each
// delegates to the underlying QuerySet[T] and returns a Values[T, R], so the
// element type flows through the chain and execution stays on Iter/All(ctx).

// Filter keeps only rows matching every condition (QuerySet.Filter).
func (vq Values[T, R]) Filter(conds ...Cond) Values[T, R] { vq.q.q = vq.q.q.filter(conds); return vq }

// Exclude drops rows matching every condition (QuerySet.Exclude).
func (vq Values[T, R]) Exclude(conds ...Cond) Values[T, R] { vq.q.q = vq.q.q.exclude(conds); return vq }

// Having filters the grouped rows (QuerySet.Having) — a condition on an
// aggregate annotation.
func (vq Values[T, R]) Having(conds ...Cond) Values[T, R] { vq.q.q = vq.q.q.havingConds(conds); return vq }

// OrderBy sorts by the given fields (QuerySet.OrderBy); a leading "-" is descending.
func (vq Values[T, R]) OrderBy(fields ...any) Values[T, R] {
	vq.q.q = vq.q.q.orderBy(fields)
	return vq
}

// Annotate adds a computed field (QuerySet.Annotate), e.g. an aggregate the
// projection groups by.
func (vq Values[T, R]) Annotate(name any, es ...Expr) Values[T, R] {
	vq.q.q = vq.q.q.annotate(name, es)
	return vq
}

// GroupBy groups the rows by names or typed fields (QuerySet.GroupBy).
func (vq Values[T, R]) GroupBy(names ...any) Values[T, R] { vq.q.q = vq.q.q.groupedBy(names); return vq }

// Limit caps the number of rows (QuerySet.Limit).
func (vq Values[T, R]) Limit(n int) Values[T, R] { vq.q.q = vq.q.q.limited(n); return vq }

// Offset skips the first n rows (QuerySet.Offset).
func (vq Values[T, R]) Offset(n int) Values[T, R] { vq.q.q = vq.q.q.offsetBy(n); return vq }

// Distinct removes duplicate rows (QuerySet.Distinct).
func (vq Values[T, R]) Distinct() Values[T, R] { vq.q.q = vq.q.q.distinctRows(); return vq }

// Iter runs the query and yields its rows one at a time.
func (vq Values[T, R]) Iter(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) { iterValues(ctx, vq.q.m, vq.q.q, vq.cols, yield) }
}

func iterValues[R any](ctx context.Context, m *managerCore, q *query, cols []any, yield func(R, error) bool) {
	o, rd := m.openValues(ctx, q, cols, reflect.TypeFor[R]())
	defer o.close()
	if o.ran != nil {
		next := yield
		yield = func(row R, err error) bool {
			o.refused(err)
			return next(row, err)
		}
	}
	if o.err != nil {
		var zero R
		yield(zero, o.err)
		return
	}
	readRows(o.rows, rd, yield)
}

// openValues starts the rows of a Values of q, read into values of type rt.
func (m *managerCore) openValues(ctx context.Context, q *query, cols []any, rt reflect.Type) (*opened, *reader) {
	o := &opened{m: m}
	o.ctx, o.ran = runs(ctx)
	ctx = o.ctx
	c, err := m.conn(ctx, false)
	if err != nil {
		o.err = err
		return o, nil
	}
	c, o.end, err = q.tune(ctx, c)
	if err != nil {
		o.err = err
		return o, nil
	}
	s, args, rd, err := q.valuesPlan(cols, rt, c.d)
	if err != nil {
		o.err = err
		return o, nil
	}
	o.rows, o.err = c.query(ctx, s, args)
	return o, rd
}

// All runs the query and returns its rows.
func (vq Values[T, R]) All(ctx context.Context) ([]R, error) { return collectRows(vq.Iter(ctx)) }

func collectRows[R any](rows iter.Seq2[R, error]) ([]R, error) {
	out := []R{}
	for row, err := range rows {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// readRows yields rows as rd reads them, then closes them.
func readRows[R any](rows *sql.Rows, rd *reader, yield func(R, error) bool) {
	defer rows.Close()
	var row, blank R // read into again and again, yielded by value
	v := reflect.ValueOf(&row).Elem()
	for rows.Next() {
		row = blank
		if err := rd.scan(rows, v); err != nil {
			yield(row, err)
			return
		}
		if !yield(row, nil) {
			return
		}
	}
	if err := rows.Err(); err != nil {
		var zero R
		yield(zero, err)
	}
}

// reader puts a row's columns, named names, into an R: a scalar, a struct
// (a field per name, by orm path tag, name or column), map[string]any
// keyed by the names, or []any in their order.
type reader struct {
	names  []string
	types  []reflect.Type // a map's or list's value types, nil as the driver gives them
	index  [][]int        // a struct's field per name
	fast   []scanKind     // and how it scans
	codec  []SchemaCodec  // a field codec per name, nil for none
	dates  []bool         // a name reads a date: its text, as SQLite gives it
	out    reflect.Type   // R
	anyAs  reflect.Type   // the type an any R reads as, nil for the driver's
	shape  int
	schema string // the names set read under, for SchemaScanners
	// The scan destinations, made on the first row and used for every
	// one: a map's or list's cells hold values of their own, copied out.
	cells []cell
	dest  []any
}

const (
	scalarShape = iota
	structShape
	mapShape
	listShape
)

// newReader is the reader of R for names; fields, where known, are the
// model fields the names read: their Go types for maps and lists, and
// more names for a struct's fields to match.
func newReader(rt reflect.Type, names []string, fields []*field, schema string, loose bool) (*reader, error) {
	rd := &reader{names: names, types: make([]reflect.Type, len(names)), codec: make([]SchemaCodec, len(names)), schema: schema, out: rt}
	for i, f := range fields {
		if f != nil {
			rd.types[i] = f.Type
			if f.Type.Kind() != reflect.Pointer {
				rd.types[i] = reflect.PointerTo(f.Type) // NULL as nil
			}
			rd.codec[i] = f.codec
		}
	}
	switch {
	case rt == reflect.TypeFor[map[string]any]():
		rd.shape = mapShape
	case rt == reflect.TypeFor[[]any]():
		rd.shape = listShape
	case rt.Kind() == reflect.Struct && rt != timeType:
		rd.shape = structShape
	case len(names) != 1:
		return nil, fmt.Errorf("orm: %d columns can't be read into %v: read them into a struct, map[string]any or []any", len(names), rt)
	}
	if rd.shape == scalarShape && len(fields) == 1 && fields[0] != nil {
		if err := readsAs(names[0], fields[0], rt); err != nil {
			return nil, err
		}
	}
	if rd.shape != structShape {
		return rd, nil
	}
	rm, err := modelOf(rt, "", "-")
	if err != nil {
		return nil, err
	}
	for i, name := range names {
		alts := []string{name}
		if i < len(fields) && fields[i] != nil {
			alts = append(alts, fields[i].Column, fields[i].Name)
		}
		parts := strings.Split(name, "__")
		alts = append(alts, parts[len(parts)-1])
		var rf *field
		for _, f := range rm.via {
			if f.Via == name {
				rf = f
			}
		}
		for _, alt := range alts {
			if rf == nil {
				rf, _ = rm.field(alt)
			}
		}
		if rf == nil && loose {
			rd.index, rd.fast = append(rd.index, nil), append(rd.fast, 0)
			continue
		}
		if rf == nil {
			return nil, fmt.Errorf("orm: %v has no field for %q (orm.Loose() drops such columns)", rt, name)
		}
		if i < len(fields) && fields[i] != nil {
			if err := readsAs(name, fields[i], rf.Type); err != nil {
				return nil, err
			}
		}
		rd.index = append(rd.index, rf.Index)
		rd.fast = append(rd.fast, rf.fast)
		// Keep the source field's codec (set from fields[i] above, under the
		// query's names set) — the result struct's field carries one only
		// when it declares its own, so it must not blank the schema codec.
		if rf.codec != nil {
			rd.codec[i] = rf.codec
		}
	}
	return rd, nil
}

// scan reads the current row into v.
func (rd *reader) scan(rows *sql.Rows, v reflect.Value) error {
	if rd.cells == nil {
		n := len(rd.names)
		if rd.shape == scalarShape {
			n = 1
		}
		rd.cells, rd.dest = make([]cell, n), make([]any, n)
		for i := range rd.cells {
			rd.dest[i] = &rd.cells[i]
			rd.cells[i].names = rd.schema
			if i < len(rd.codec) {
				rd.cells[i].codec = rd.codec[i]
			}
			if i < len(rd.dates) {
				rd.cells[i].date = rd.dates[i]
			}
			if rd.shape == mapShape || rd.shape == listShape {
				rd.cells[i].dst = reflect.New(cmp.Or(rd.types[i], anyType)).Elem()
				if rd.types[i] != nil && scanKindOf(rd.types[i]) == scanSchema {
					rd.cells[i].fast = scanSchema
				}
			}
		}
	}
	cells := rd.cells
	for i := range cells {
		switch rd.shape {
		case scalarShape:
			if rd.anyAs != nil {
				cells[i].dst = reflect.New(rd.anyAs).Elem()
				break
			}
			cells[i].dst = v
			if scanKindOf(v.Type()) == scanSchema {
				cells[i].fast = scanSchema
			}
		case structShape:
			if rd.index[i] == nil {
				cells[i].dst, cells[i].fast = reflect.New(anyType).Elem(), 0 // a column Loose drops
				continue
			}
			cells[i].dst, cells[i].fast = fieldOf(v, rd.index[i]), rd.fast[i]
		default:
			cells[i].dst.SetZero()
		}
	}
	if err := rows.Scan(rd.dest...); err != nil {
		return err
	}
	if rd.shape == scalarShape {
		if rd.anyAs != nil {
			if x := value(cells[0].dst); x != nil {
				v.Set(reflect.ValueOf(x))
			} else {
				v.SetZero()
			}
		}
		return nil
	}
	plain := func(i int) any {
		x := value(cells[i].dst)
		if b, ok := x.([]byte); ok && rd.types[i] == nil {
			return string(b)
		}
		return x
	}
	switch rd.shape {
	case mapShape:
		out := make(map[string]any, len(cells))
		for i, name := range rd.names {
			out[name] = plain(i)
		}
		v.Set(reflect.ValueOf(out))
	case listShape:
		out := make([]any, len(cells))
		for i := range out {
			out[i] = plain(i)
		}
		v.Set(reflect.ValueOf(out))
	}
	return nil
}

var anyType = reflect.TypeFor[any]()

// Agg is an aggregate: of a field (Count, Sum, Avg, Min, Max), or any
// aggregate expression (AggOf).
type Agg struct {
	fn, field string
	key       string
	expr      Expr
	arg       Expr // the aggregated expression, when it is no field
	filter    Cond
	distinct  bool
}

// AggOf is an aggregate expression under a key of your own, for the
// aggregates the database has beyond the five:
//
//	var Names = orm.Function("names",
//		orm.Template("postgres", "string_agg({0}, ', ')"),
//		orm.Template("", "GROUP_CONCAT({0}, ', ')"),               // SQLite
//		orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')")) // a literal: MySQL takes no argument here
//	r, _ := Users.Aggregate(ctx, orm.AggOf("names", Names.Of(orm.F("name"))))
func AggOf(key string, e Expr) Agg { return Agg{key: key, expr: e} }

// Filter aggregates only the rows where cond holds, Django's filter=:
//
//	orm.Count("id").Filter(orm.Q{"active": true}).As("active")
//
// It is FILTER (WHERE …) on Postgres and SQLite, and the aggregate of a
// CASE on MySQL, NULL where cond doesn't hold.
func (a Agg) Filter(cond Cond) Agg {
	a.filter = cond
	return a
}

// Distinct aggregates each distinct value once, Django's distinct=True:
// COUNT(DISTINCT …), so a row reached twice through joins counts once.
func (a Agg) Distinct() Agg {
	a.distinct = true
	return a
}

// As names the aggregate's Key.
func (a Agg) As(key string) Agg {
	a.key = key
	return a
}

// exprSQL makes an aggregate an Expr: Annotate takes one, and Values
// listing it groups by the other names.
func (a Agg) exprSQL(b *builder) (string, error) {
	if a.expr != nil {
		if a.filter != nil || a.distinct {
			return "", fmt.Errorf("orm: AggOf(%q) takes no Filter or Distinct: put them in its expression", a.key)
		}
		return a.expr.exprSQL(b)
	}
	var col string
	var err error
	if a.arg != nil {
		col, err = a.arg.exprSQL(b)
	} else {
		col, err = b.ref(a.field)
	}
	if err != nil {
		return "", err
	}
	// MySQL averages integers and decimals as a DECIMAL of four more
	// places (div_precision_increment): a double, as elsewhere, averages
	// as a float64 reads.
	if a.fn == "AVG" && b.d.Name() == "mysql" {
		col = "(" + col + " + 0E0)"
	}
	fn := a.fn + "("
	if a.distinct {
		fn += "DISTINCT "
	}
	if a.filter == nil {
		return fn + col + ")", nil
	}
	neg, agg := b.neg, b.agg
	b.neg, b.agg = false, true
	defer func() { b.neg, b.agg = neg, agg }()
	cond, err := a.filter.sql(b)
	if err != nil || cond == "" {
		return fn + col + ")", err
	}
	if b.d.Name() == "mysql" {
		return fn + "CASE WHEN " + cond + " THEN " + col + " END)", nil
	}
	return fn + col + ") FILTER (WHERE " + cond + ")", nil
}

// AggOption shapes an aggregate: Distinct.
type AggOption int

// Distinct aggregates each distinct value once: orm.Count(ProductColumns.ID,
// orm.Distinct) is COUNT(DISTINCT …), so a row a join repeats counts once.
const Distinct AggOption = 1

// Count, Sum, Avg, Min and Max aggregate a field: a name ("age",
// "books__id"), a typed field (of the queried model or one it joins), or
// an expression. Count is SQL's COUNT(col), which skips NULLs, so a LEFT
// JOIN's missing rows count 0.
//
//	Category.Objects.Join(…).Values[CategoryProducts](CategoryColumns.Name, orm.Count(ProductColumns.ID, orm.Distinct))
func Count(field any, opts ...AggOption) Agg { return newAgg("COUNT", field, opts) }
func Sum(field any, opts ...AggOption) Agg   { return newAgg("SUM", field, opts) }
func Avg(field any, opts ...AggOption) Agg   { return newAgg("AVG", field, opts) }
func Min(field any, opts ...AggOption) Agg   { return newAgg("MIN", field, opts) }
func Max(field any, opts ...AggOption) Agg   { return newAgg("MAX", field, opts) }

func newAgg(fn string, field any, opts []AggOption) Agg {
	a := Agg{fn: fn}
	switch f := field.(type) {
	case string:
		a.field = f
	case typedField:
		a.field = f.key()
	case Expr:
		a.arg = f
	default:
		a.arg = errExpr{fmt.Errorf("orm: %s of %T: aggregate a field or an expression", fn, field)}
	}
	for _, o := range opts {
		if o == Distinct {
			a.distinct = true
		}
	}
	return a
}

// Key is the aggregate's name in a Result, Django's: age__avg.
func (a Agg) Key() string {
	if a.key != "" {
		return a.key
	}
	return plainPath(a.field) + "__" + strings.ToLower(a.fn)
}

// Result is aggregates by Key: COUNT an int64, AVG a float64, the others
// what the database gives (nil over no rows).
type Result map[string]any

// Int is an aggregate as an int64.
func (r Result) Int(key string) int64 {
	var n int64
	_ = assign(reflect.ValueOf(&n).Elem(), r[key])
	return n
}

// Float is an aggregate as a float64.
func (r Result) Float(key string) float64 {
	var f float64
	_ = assign(reflect.ValueOf(&f).Elem(), r[key])
	return f
}

// Aggregate computes aggregates over the matching rows.
func (qs QuerySet[T]) Aggregate(ctx context.Context, aggs ...Agg) (Result, error) {
	return qs.m.aggregate(ctx, qs.q, aggs)
}

func (m *managerCore) aggregate(ctx context.Context, q *query, aggs []Agg) (_ Result, err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	c, err := m.conn(ctx, false)
	if err != nil {
		return nil, err
	}
	s, args, err := q.aggregateSQL(c.d, aggs)
	if err != nil {
		return nil, err
	}
	rows, err := c.query(ctx, s, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Result{}
	if rows.Next() {
		vals := make([]any, len(aggs))
		dest := make([]any, len(aggs))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, a := range aggs {
			v := vals[i]
			switch a.fn {
			case "COUNT":
				var n int64
				_ = assign(reflect.ValueOf(&n).Elem(), v)
				v = n
			case "AVG":
				if v != nil {
					var f float64
					_ = assign(reflect.ValueOf(&f).Elem(), v)
					v = f
				}
			}
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			out[a.Key()] = v
		}
	}
	return out, rows.Err()
}

func (q *query) aggregateSQL(d Dialect, aggs []Agg) (string, []any, error) {
	b := q.builder(d)
	exprs := make([]string, len(aggs))
	for i, a := range aggs {
		s, err := a.exprSQL(b)
		if err != nil {
			return "", nil, err
		}
		exprs[i] = s
	}
	at := len(b.st.args)
	w, err := q.whereSQL(b)
	if err != nil {
		return "", nil, err
	}
	from, err := b.from(at)
	if err != nil {
		return "", nil, err
	}
	return "SELECT " + strings.Join(exprs, ", ") + " FROM " + from + w, b.args(), nil
}

// annotated is the name a target struct's field reads: name itself when
// the model or the query has it, else the annotation spelt like it but for
// case and underscores — a field TotalPlaced reads an annotation named
// totalPlaced as well as total_placed.
func (q query) annotated(name string) string {
	if _, ok := q.m.field(name); ok || strings.Contains(name, "__") {
		return name
	}
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for _, a := range q.ann {
		if a.name == name {
			return name
		}
	}
	for _, a := range q.ann {
		if norm(a.name) == norm(name) {
			return a.name
		}
	}
	return name
}

// SQL builds the projection for the context's database without executing it.
func (vq Values[T, R]) SQL(ctx context.Context) (string, []any, error) {
	return vq.q.m.valuesSQL(ctx, vq.q.q, vq.cols, reflect.TypeFor[R]())
}

func (m *managerCore) valuesSQL(ctx context.Context, q *query, cols []any, rt reflect.Type) (string, []any, error) {
	c, err := m.conn(ctx, false)
	if err != nil {
		return "", nil, err
	}
	s, args, _, err := q.valuesPlan(cols, rt, c.d)
	return s, args, err
}

// valuesPlan is the statement of a Values of qq, and what reads its rows into
// values of type rt.
func (qq *query) valuesPlan(columns []any, rt reflect.Type, d Dialect) (string, []any, *reader, error) {
	names, ok := stringNames(columns)
	if !ok {
		return qq.positional(columns, rt, d)
	}
	q := *qq
	if len(names) == 0 && rt.Kind() == reflect.Struct && rt != timeType {
		rm, err := modelOf(rt, "", "-")
		if err != nil {
			return "", nil, nil, err
		}
		for _, f := range rm.readFields() {
			names = append(names, q.annotated(cmp.Or(f.Via, f.Column)))
		}
	}
	if len(names) == 0 {
		return "", nil, nil, fmt.Errorf("orm: Values needs at least one field")
	}
	b := q.builder(d)
	b.many = true
	cols := make([]string, len(names))
	fields := make([]*field, len(names))
	var err error
	var group []string
	grouped := false
	for i, name := range names {
		if cols[i], fields[i], err = b.refField(name); err != nil {
			return "", nil, nil, err
		}
		e, ok := b.ann[name]
		if ok {
			cols[i] += " AS " + b.d.Quote(name)
		}
		if isAggregate(e) {
			grouped = true
		} else {
			group = append(group, strconv.Itoa(i+1))
		}
	}
	q.values = names
	q.grouped = grouped || q.groupBy != nil
	if grouped && q.groupBy == nil {
		for _, a := range q.ann {
			if _, ok := a.expr.(relationAggregate); ok {
				q.groupBy = []string{q.m.PK.Name}
				for _, name := range names {
					if !isAggregate(b.ann[name]) && !strings.EqualFold(name, q.m.PK.Name) && name != q.m.PK.Column {
						q.groupBy = append(q.groupBy, name)
					}
				}
				break
			}
		}
	}
	if grouped && len(group) > 0 {
		q.group = " GROUP BY " + strings.Join(group, ", ")
	}
	if !q.grouped {
		q = q.ordered()
	}
	rd, err := newReader(rt, names, fields, q.m.Names, false)
	if err != nil {
		return "", nil, nil, err
	}
	for i, name := range names {
		if e, ok := b.ann[name]; ok && fields[i] == nil {
			rd.readAs(i, q.m, e)
		}
	}
	if rd.shape == structShape {
		rm, err := modelOf(rt, "", "-")
		if err != nil {
			return "", nil, nil, err
		}
		for _, f := range rm.readFields() {
			found := false
			for _, index := range rd.index {
				if slices.Equal(index, f.Index) {
					found = true
					break
				}
			}
			if !found {
				return "", nil, nil, fmt.Errorf("orm: Values[%v] field %s has no selected column; select it or remove the field", rt, f.Name)
			}
		}
	}
	s, err := q.selectSQL(b, strings.Join(cols, ", "))
	if err != nil {
		return "", nil, nil, err
	}

	return s, b.args(), rd, nil
}

// stringNames is cols as names when every one is a string.
func stringNames(cols []any) ([]string, bool) {
	names := make([]string, len(cols))
	for i, c := range cols {
		s, ok := c.(string)
		if !ok {
			return nil, false
		}
		names[i] = s
	}
	return names, true
}

// positional is the plan of Values given typed columns: each a name, a
// typed field or relation, or an expression, read into R by position. An
// aggregate among them groups by the others.
func (qq *query) positional(columns []any, rt reflect.Type, d Dialect) (string, []any, *reader, error) {
	q := *qq
	b := q.builder(d)
	b.many = true
	n := len(columns)
	cols := make([]string, n)
	names := make([]string, n)
	fields := make([]*field, n)
	var group []string
	grouped := false
	for i, c := range columns {
		var err error
		agg := false
		if name, ok := queryName(c); ok {
			if cols[i], fields[i], err = b.refField(name); err != nil {
				return "", nil, nil, err
			}
			names[i] = name
			if e, ok := b.ann[name]; ok {
				cols[i] += " AS " + b.d.Quote(name)
				agg = isAggregate(e)
			}
		} else if e, ok := c.(Expr); ok {
			if cols[i], err = e.exprSQL(b); err != nil {
				return "", nil, nil, err
			}
			agg = isAggregate(e)
			names[i] = "col" + strconv.Itoa(i+1)
			if a, ok := c.(Agg); ok {
				names[i] = a.Key()
			}
		} else {
			return "", nil, nil, fmt.Errorf("orm: Values: column %d is a %T: give a field, a typed field or an expression", i+1, c)
		}
		if agg {
			grouped = true
		} else {
			group = append(group, strconv.Itoa(i+1))
			q.by = append(q.by, cols[i])
			q.byNames = append(q.byNames, plainPath(names[i]))
		}
	}
	q.values = names
	q.grouped = grouped || q.groupBy != nil
	if grouped && len(group) > 0 && q.groupBy == nil {
		q.group = " GROUP BY " + strings.Join(group, ", ")
	}
	if !q.grouped {
		q = q.ordered()
	}
	rd, err := positionalReader(rt, names, fields, q.m.Names)
	if err != nil {
		return "", nil, nil, err
	}
	s, err := q.selectSQL(b, strings.Join(cols, ", "))
	if err != nil {
		return "", nil, nil, err
	}
	return s, b.args(), rd, nil
}

// positionalReader reads columns into R by position: a struct's fields in
// order, a list's items, a map keyed by names, a scalar for one.
func positionalReader(rt reflect.Type, names []string, fields []*field, schema string) (*reader, error) {
	if rt.Kind() != reflect.Struct || rt == timeType {
		for i := range names {
			names[i] = plainPath(names[i])
		}
		return newReader(rt, names, fields, schema, false)
	}
	rm, err := modelOf(rt, "", "-")
	if err != nil {
		return nil, err
	}
	fs := rm.readFields()
	if len(fs) != len(names) {
		return nil, fmt.Errorf("orm: Values[%v] fills its %d fields by position, and %d columns are given", rt, len(fs), len(names))
	}
	rd := &reader{names: names, types: make([]reflect.Type, len(names)), codec: make([]SchemaCodec, len(names)), schema: schema, shape: structShape}
	for i, f := range fs {
		if fields[i] != nil {
			if err := readsAs(plainPath(names[i]), fields[i], f.Type); err != nil {
				return nil, fmt.Errorf("orm: Values[%v] field %s: %w", rt, f.Name, err)
			}
			if kindClash(fields[i].Type, f.Type) {
				return nil, fmt.Errorf("orm: Values[%v] field %s is %v, and column %d (%s) is %v", rt, f.Name, f.Type, i+1, plainPath(names[i]), fields[i].Type)
			}
			rd.codec[i] = fields[i].codec
		}
		rd.index = append(rd.index, f.Index)
		rd.fast = append(rd.fast, f.fast)
		if f.codec != nil {
			rd.codec[i] = f.codec
		}
	}
	return rd, nil
}
