package orm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"

	"github.com/vuka-lang/vuka"
)

// Query is a model's QuerySet as Vuka code reads it: conditions are
// predicates of field references (Post.Title.Contains("go"), typed by the
// model, so a User's are refused at compile time), orderings their Asc
// and Desc, relations and fields named by reference, and the terminals
// return vuka.Result — First a Result of an Option. Post.Objects is the
// Query of every Post; each method returns a new Query, and nothing runs
// until a terminal.
//
//	posts := Post.Objects.Filter(Post.Title.Contains("go"), Post.Author.Name.Eq("ada")).
//		OrderBy(Post.Views.Desc()).All(ctx)?
//	match Post.Objects.Get(ctx, Post.Slug.Eq(slug)) {
//	case Ok(p):              …
//	case Err(orm.NotFound):  …
//	case Err(e):             …
//	}
//
// QuerySet and With reach the engine's string forms.
type Query[T any] struct {
	qs  QuerySet[T]
	set bool
}

// QueryOf is qs as a Query.
func QueryOf[T any](qs QuerySet[T]) Query[T] { return Query[T]{qs: qs, set: true} }

// QuerySet is the query as the engine's QuerySet.
func (q Query[T]) QuerySet() QuerySet[T] {
	if q.set {
		return q.qs
	}
	return Objects[T]().QuerySet
}

// Manager is the model's manager (OnChange, BulkCreate, Raw, …).
func (q Query[T]) Manager() *Manager[T] { return q.QuerySet().m }

func (q Query[T]) with(f func(QuerySet[T]) QuerySet[T]) Query[T] {
	return Query[T]{qs: f(q.QuerySet()), set: true}
}

// With applies f to the QuerySet: the way to a string form of the engine.
//
//	Post.Objects.With(func(qs orm.QuerySet[Post]) orm.QuerySet[Post] { return qs.OrderBy("-views") })
func (q Query[T]) With(f func(QuerySet[T]) QuerySet[T]) Query[T] { return q.with(f) }

// On is the model's rows on schema s (a tenant's schema, another
// database): call it first, the query starts over.
func (q Query[T]) On(s Schema) Query[T] { return QueryOf(Of[T](s).QuerySet) }

// Filter keeps the rows every predicate holds for.
func (q Query[T]) Filter(ps ...vuka.Pred[T]) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Filter(condsOf(ps)...) })
}

// Exclude drops the rows all the predicates hold for.
func (q Query[T]) Exclude(ps ...vuka.Pred[T]) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Exclude(condsOf(ps)...) })
}

// Where filters by the ORM's own conditions (Q, Or, Exists, Match).
func (q Query[T]) Where(conds ...Cond) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Filter(conds...) })
}

// OrderBy orders by field references' and terms' Asc and Desc.
func (q Query[T]) OrderBy(sorts ...Sort[T]) Query[T] {
	terms := make([]OrderTerm, len(sorts))
	for i, s := range sorts {
		switch o := s.(type) {
		case vuka.Order[T]:
			terms[i] = Ordering{Path: []string{lookupPath(o.Path)}, Desc: o.Desc}
		case TermOrder[T]:
			switch {
			case o.name != "":
				terms[i] = Ordering{Path: []string{o.name}, Desc: o.desc}
			case o.desc:
				terms[i] = Desc(o.e)
			default:
				terms[i] = Asc(o.e)
			}
		default:
			err := fmt.Errorf("orm: %T is no ordering", s)
			return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Filter(errCond{err}) })
		}
	}
	return q.Order(terms...)
}

// Order orders by the engine's order terms: a typed expression's Asc or
// Desc (orm.Func's results), orm.Ordering.
func (q Query[T]) Order(terms ...OrderTerm) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Order(terms...) })
}

// AnnotateAs adds typed expressions named with their As (orm.Func's
// results).
func (q Query[T]) AnnotateAs(as ...Alias) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.AnnotateAs(as...) })
}

// Limit, Offset and Distinct are the QuerySet's.
func (q Query[T]) Limit(n int) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Limit(n) })
}
func (q Query[T]) Offset(n int) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Offset(n) })
}
func (q Query[T]) Distinct() Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Distinct() })
}

// Unfiltered marks the query as meant for every row (Delete, Update).
func (q Query[T]) Unfiltered() Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Unfiltered() })
}

// Annotate adds named terms (t.As("name")) as computed columns.
func (q Query[T]) Annotate(as ...Annotation[T]) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] {
		for _, a := range as {
			name, e := a.annotation(nil)
			if name == "" {
				return qs.Filter(errCond{errors.New("orm: Annotate takes named terms: name it with As")})
			}
			qs = qs.Annotate(name, e)
		}
		return qs
	})
}

// GroupBy groups by fields (references) and annotations (named terms).
func (q Query[T]) GroupBy(groups ...any) Query[T] {
	names, err := pathNames(groups)
	return q.with(func(qs QuerySet[T]) QuerySet[T] {
		if err != nil {
			return qs.Filter(errCond{err})
		}
		return qs.GroupBy(names...)
	})
}

// Having filters the groups by predicates on aggregates.
func (q Query[T]) Having(ps ...vuka.Pred[T]) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Having(condsOf(ps)...) })
}

// SelectRelated reads foreign keys' rows in the same query (a join).
func (q Query[T]) SelectRelated(rels ...Path[T]) Query[T] {
	names := make([]string, len(rels))
	for i, r := range rels {
		names[i] = FieldName(r)
	}
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.SelectRelated(names...) })
}

// PrefetchRelated reads relations' rows in a query each: references, or
// Prefetch for a filtered one.
func (q Query[T]) PrefetchRelated(specs ...any) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.PrefetchRelated(prefetchSpecs(specs)...) })
}

// PrefetchBy is a relation's rows read by a query: a filtered or ordered
// prefetch.
//
//	Post.Objects.PrefetchRelated(orm.PrefetchBy(Post.Tags, Tag.Objects.Filter(Tag.Name.StartsWith("g"))))
func PrefetchBy[T, R any](rel Path[T], rows Query[R]) PrefetchSpec {
	return Prefetch(FieldName(rel), rows.QuerySet())
}

// Nearest orders by the distance of a vector field to v.
func (q Query[T]) Nearest(field Path[T], v Vector, m Metric) Query[T] {
	return q.with(func(qs QuerySet[T]) QuerySet[T] { return qs.Nearest(FieldName(field), v, m) })
}

// All runs the query: its rows, relations loaded.
func (q Query[T]) All(ctx context.Context) vuka.Result[[]T] { return vuka.Of(q.QuerySet().All(ctx)) }

// Iter yields the rows one at a time.
func (q Query[T]) Iter(ctx context.Context) iter.Seq2[T, error] { return q.QuerySet().Iter(ctx) }

// First is the first row, None when there is none.
func (q Query[T]) First(ctx context.Context) vuka.Result[vuka.Option[T]] {
	row, err := q.QuerySet().First(ctx)
	switch {
	case errors.Is(err, NotFound):
		return vuka.Ok(vuka.None[T]())
	case err != nil:
		return vuka.Err[vuka.Option[T]](err)
	}
	return vuka.Ok(vuka.Some(row))
}

// Get is the one row the predicates hold for. It fails with the Kind
// itself — orm.NotFound, orm.MultipleFound — so match compares it:
// case Err(orm.NotFound).
func (q Query[T]) Get(ctx context.Context, ps ...vuka.Pred[T]) vuka.Result[T] {
	row, err := q.QuerySet().Get(ctx, condsOf(ps)...)
	if err != nil {
		return vuka.Err[T](bareKind(err))
	}
	return vuka.Ok(row)
}

// bareKind is err's NotFound or MultipleFound Kind, else err.
func bareKind(err error) error {
	var e *Error
	if errors.As(err, &e) && (e.Kind == NotFound || e.Kind == MultipleFound) {
		return e.Kind
	}
	return err
}

// Count, Exists, Delete and Update are the QuerySet's.
func (q Query[T]) Count(ctx context.Context) vuka.Result[int64] {
	return vuka.Of(q.QuerySet().Count(ctx))
}
func (q Query[T]) Exists(ctx context.Context) vuka.Result[bool] {
	return vuka.Of(q.QuerySet().Exists(ctx))
}
func (q Query[T]) Delete(ctx context.Context) vuka.Result[int64] {
	return vuka.Of(q.QuerySet().Delete(ctx))
}

// Update sets fields of every row: Assign(Post.Views, 0),
// AssignF(Post.Views, fn.Num(Post.Views).Add(1)).
func (q Query[T]) Update(ctx context.Context, sets ...Assignment[T]) vuka.Result[int64] {
	s := Set{}
	for _, a := range sets {
		s[a.name] = a.v
	}
	return vuka.Of(q.QuerySet().Update(ctx, s))
}

// Assignment is a field set to a value or an expression, for Update.
type Assignment[T any] struct {
	name string
	v    any
}

// Assign sets field f to v; AssignF to a field or term.
func Assign[T, V any](f Arg[T, V], v V) Assignment[T] {
	return Assignment[T]{pathName(f), argValue(v)}
}
func AssignF[T, V any](f Arg[T, V], e Arg[T, V]) Assignment[T] {
	return Assignment[T]{pathName(f), ExprOf(e)}
}

// Col is a column of a Values or Aggregate result R: Into(R.Field, source).
type Col[T, R any] struct {
	dst string
	src any
}

// Into reads src — a field reference of T or a term — into the field
// dst of the result R. Both are of type V, so a count goes into an int64.
//
//	type Stats struct { Author string; Posts int64 }
//	stats := Post.Objects.Values[Stats](ctx, orm.Into(Stats.Author, Post.Author.Name), orm.Into(Stats.Posts, fn.Count(Post.ID)))?
func Into[T, R, V any](dst Arg[R, V], src Arg[T, V]) Col[T, R] {
	d := pathName(dst)
	if p, ok := dst.(pather); ok {
		names := p.FieldPath().Names()
		d = names[len(names)-1]
	}
	return Col[T, R]{dst: d, src: src}
}

// Values reads the columns into Rs, Django's values: a source that is a
// field of the same name is read as is, others are annotated; with an
// aggregate among them the rows are grouped by the rest.
func (q Query[T]) Values[R any](ctx context.Context, cols ...Col[T, R]) vuka.Result[[]R] {
	qs := q.QuerySet()
	names := make([]string, len(cols))
	for i, c := range cols {
		if n := PathName(c.src); n != "" {
			names[i] = n
			if _, ok := c.src.(pather); !ok && !annotated(qs, n) {
				a := c.src.(interface{ annotation(*T) (string, Expr) })
				_, e := a.annotation(nil)
				qs = qs.Annotate(n, e)
			}
			continue
		}
		names[i] = "v_" + strings.ToLower(c.dst)
		qs = qs.Annotate(names[i], unterm(ExprOf(c.src)))
	}
	if len(cols) == 0 {
		return vuka.Of(qs.Values[R]().All(ctx))
	}
	rows, err := qs.Values[map[string]any](names...).All(ctx)
	if err != nil {
		return vuka.Err[[]R](err)
	}
	return vuka.Of(intoStructs(rows, cols, names))
}

// Aggregate is Values of aggregates over every row: one R.
//
//	totals := Post.Objects.Aggregate[Totals](ctx, orm.Into(Totals.Views, fn.Sum(Post.Views)))?
func (q Query[T]) Aggregate[R any](ctx context.Context, cols ...Col[T, R]) vuka.Result[R] {
	rows, err := q.Values[R](ctx, cols...).Get()
	if err != nil {
		return vuka.Err[R](err)
	}
	if len(rows) == 0 {
		var zero R
		return vuka.Ok(zero)
	}
	return vuka.Ok(rows[0])
}

// Paginate is a page of the rows: PageRequest's page, size, sort and
// search over the fields SortableBy and SearchableBy allow.
func (q Query[T]) Paginate(ctx context.Context, r PageRequest, opts ...PageOption) vuka.Result[PageResult[T]] {
	return vuka.Of(Paginate(ctx, q.QuerySet(), r, opts...))
}

// SortableBy, SearchableBy and DefaultSortBy are Sortable, Searchable and
// DefaultSort by field reference.
func SortableBy[T any](fields ...Path[T]) PageOption   { return Sortable(fieldNames(fields)...) }
func SearchableBy[T any](fields ...Path[T]) PageOption { return Searchable(fieldNames(fields)...) }
func DefaultSortBy[T any](sorts ...vuka.Order[T]) PageOption {
	names := make([]string, len(sorts))
	for i, o := range sorts {
		names[i] = lookupPath(o.Path)
		if o.Desc {
			names[i] = "-" + names[i]
		}
	}
	return DefaultSort(names...)
}

func fieldNames[T any](fields []Path[T]) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = FieldName(f)
	}
	return out
}

// Raw is the rows of a SQL query of the model's table.
func (q Query[T]) Raw(ctx context.Context, sql string, args ...any) vuka.Result[[]T] {
	return vuka.Of(q.Manager().Raw(sql, args...).All(ctx))
}

// Create inserts row (its key set after); BulkCreate inserts rows in
// batches.
func (q Query[T]) Create(ctx context.Context, row *T) error { return q.Manager().Create(ctx, row) }
func (q Query[T]) BulkCreate(ctx context.Context, rows ...*T) error {
	return q.Manager().BulkCreate(ctx, rows)
}

// Outcome is GetOrCreate's and UpdateOrCreate's: the row, and whether it
// was created.
type Outcome[T any] struct {
	Row     T
	Created bool
}

// GetOrCreate is the row whose match fields equal row's, else row created.
//
//	tag := Tag.Objects.GetOrCreate(ctx, Tag{Name: "go"}, Tag.Name)?.Row
func (q Query[T]) GetOrCreate(ctx context.Context, row T, match ...Path[T]) vuka.Result[Outcome[T]] {
	return q.orCreate(ctx, row, match, false)
}

// UpdateOrCreate updates the row whose match fields equal row's with
// row's other fields, else creates row.
func (q Query[T]) UpdateOrCreate(ctx context.Context, row T, match ...Path[T]) vuka.Result[Outcome[T]] {
	return q.orCreate(ctx, row, match, true)
}

func (q Query[T]) orCreate(ctx context.Context, row T, match []Path[T], update bool) vuka.Result[Outcome[T]] {
	m := q.Manager()
	if m.err != nil {
		return vuka.Err[Outcome[T]](m.err)
	}
	lookup, defaults := Q{}, Defaults{}
	values := m.ColumnValues(&row)
	for _, p := range match {
		f, ok := m.meta.field(FieldName(p))
		if !ok || f.Via != "" {
			return vuka.Err[Outcome[T]](fmt.Errorf("orm: %s has no column %s", m.meta.Name, FieldName(p)))
		}
		lookup[f.Column] = values[f.Column]
	}
	for _, f := range m.meta.Fields {
		if _, matched := lookup[f.Column]; !matched && f != m.meta.PK {
			defaults[f.Column] = values[f.Column]
		}
	}
	var got T
	var created bool
	var err error
	if update {
		got, created, err = m.UpdateOrCreate(ctx, lookup, defaults)
	} else {
		got, created, err = m.GetOrCreate(ctx, lookup, defaults)
	}
	return vuka.Of(Outcome[T]{got, created}, err)
}

// OnChange calls fn after each write of the model's rows.
func (q Query[T]) OnChange(fn func(ctx context.Context, c Change[T])) (stop func()) {
	return q.Manager().OnChange(fn)
}

func annotated[T any](qs QuerySet[T], name string) bool {
	return slices.ContainsFunc(qs.q.ann, func(a annotation) bool { return a.name == name })
}

// intoStructs fills Rs from rows read as maps under names.
func intoStructs[T, R any](rows []map[string]any, cols []Col[T, R], names []string) ([]R, error) {
	rt := reflect.TypeFor[R]()
	if rt.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: Values with columns reads into a struct, not %v", rt)
	}
	idx := make([][]int, len(cols))
	for i, c := range cols {
		sf, ok := rt.FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, c.dst) })
		if !ok {
			return nil, fmt.Errorf("orm: %v has no field %s", rt, c.dst)
		}
		idx[i] = sf.Index
	}
	out := make([]R, len(rows))
	for j, row := range rows {
		v := reflect.ValueOf(&out[j]).Elem()
		for i := range cols {
			x := row[names[i]]
			if p := reflect.ValueOf(x); p.Kind() == reflect.Pointer {
				x = nil
				if !p.IsNil() {
					x = p.Elem().Interface()
				}
			}
			if err := setField(v.FieldByIndex(idx[i]), x); err != nil {
				return nil, fmt.Errorf("orm: %v.%s: %w", rt, c(cols[i]), err)
			}
		}
	}
	return out, nil
}

func c[T, R any](col Col[T, R]) string { return col.dst }
