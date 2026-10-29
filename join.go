package orm

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Source is the table a typed field reads: a model, under a name of its
// own when a query joins it more than once. Generated typed fields carry
// theirs (ProductColumns's is Product's), so a query tells a joined
// model's columns from its own; As names another.
type Source struct {
	t     reflect.Type
	alias string
	id    string
}

type sourceKey struct {
	t     reflect.Type
	alias string
}

var sources struct {
	mu   sync.Mutex
	byID sync.Map // id → *Source
	by   map[sourceKey]*Source
}

func sourceFor(t reflect.Type, alias string) *Source {
	sources.mu.Lock()
	defer sources.mu.Unlock()
	k := sourceKey{t, alias}
	if s, ok := sources.by[k]; ok {
		return s
	}
	if sources.by == nil {
		sources.by = map[sourceKey]*Source{}
	}
	s := &Source{t: t, alias: alias, id: strconv.Itoa(len(sources.by) + 1)}
	sources.by[k] = s
	sources.byID.Store(s.id, s)
	return s
}

// SourceOf is the model T's table. Generated typed fields call it.
func SourceOf[T any]() *Source { return sourceFor(reflect.TypeFor[T](), "") }

// As is the same model's table under alias: the second of a model a query
// joins twice. Generated fields' As calls it.
func (s *Source) As(alias string) *Source {
	if s == nil {
		return nil
	}
	return sourceFor(s.t, alias)
}

// Name is the model's name, and its alias after a colon.
func (s *Source) Name() string {
	if s == nil {
		return ""
	}
	if s.alias != "" {
		return s.t.Name() + " " + strconv.Quote(s.alias)
	}
	return s.t.Name()
}

// key is a lookup path read from src: the path itself without one, else
// marked with the source, which the builder resolves to the table it is
// (the query's own, or one it joins).
func sourced(src *Source, path string) string {
	if src == nil {
		return path
	}
	return "@" + src.id + "__" + path
}

// plainPath is a key without its source.
func plainPath(key string) string {
	if !strings.HasPrefix(key, "@") {
		return key
	}
	_, rest, _ := strings.Cut(key, "__")
	return rest
}

// JoinKind is how Join combines a table with the query's rows: Inner
// keeps the rows with a match, Left keeps every row, a row of NULLs for
// the joined table's columns where nothing matches.
type JoinKind int

const (
	Inner JoinKind = iota
	Left
)

// Joinable is what Join takes: a model's typed fields' On(conds…)
// (ProductColumns.On(…)), or a relation of the queried model
// (CategoryColumns.Links(), its ON from the foreign key) with or without
// an On of extra conditions.
type Joinable interface{ joinSpec() JoinOn }

// JoinOn is a table to join and the conditions its rows match on.
type JoinOn struct {
	src   *Source
	rel   string // a relation path from src instead of src's own table
	conds []Cond
}

func (j JoinOn) joinSpec() JoinOn { return j }

// On is the table of the model these fields are of, joined where conds
// hold; on a relation (CategoryColumns.Links()), the relation's table
// joined by its foreign key and conds.
//
//	Category.Objects.Join(ProductCategoryColumns.On(ProductCategoryColumns.Category().Eq(CategoryColumns.ID)), orm.Left)
func (r RelationPath) On(conds ...Cond) JoinOn {
	return JoinOn{src: r.src, rel: r.path, conds: append([]Cond(nil), conds...)}
}

func (r RelationPath) joinSpec() JoinOn { return JoinOn{src: r.src, rel: r.path} }

// Source is the table the fields read. Generated fields pass it on to the
// relations they reach.
func (r RelationPath) Source() *Source { return r.src }

// Eq holds where the relation's key equals v: a value, a typed field or
// an expression. ProductCategoryColumns.Product().Eq(ProductColumns.ID)
// compares the foreign key column with the product's id.
func (r RelationPath) Eq(v any) Cond { return Q{sourced(r.src, r.path): v} }

type joinSpec struct {
	JoinOn
	kind JoinKind
}

// Join combines the rows with another table's: Join(<model>.On(conds…))
// where conds hold, or a relation of the queried model by its foreign key
// (Join(CategoryColumns.Links(), orm.Left)). Inner unless orm.Left; the
// joined model's typed fields then name its columns in conditions, orders,
// Values and aggregates. A table joined twice takes an alias (As).
//
//	Category.Objects.
//		Join(ProductCategoryColumns.On(ProductCategoryColumns.Category().Eq(CategoryColumns.ID)), orm.Left).
//		Join(ProductColumns.On(ProductCategoryColumns.Product().Eq(ProductColumns.ID), ProductColumns.Status.In("active")), orm.Left).
//		Values[CategoryProducts](CategoryColumns.ID, CategoryColumns.Name, orm.Count(ProductColumns.ID))
func (qs QuerySet[T]) Join(j Joinable, kind ...JoinKind) QuerySet[T] {
	s := joinSpec{JoinOn: j.joinSpec()}
	if len(kind) > 0 {
		s.kind = kind[len(kind)-1]
	}
	return qs.with(func(q *query) { q.joins = append(q.joins, s) })
}

// Join is QuerySet's Join, on the projection.
func (vq Values[T, R]) Join(j Joinable, kind ...JoinKind) Values[T, R] {
	vq.q = vq.q.Join(j, kind...)
	return vq
}

// scope is the tables of one FROM a builder's names reach: its own model
// and the models it joins, by source.
type fromScope struct {
	root   *builder
	joined []scopeJoin
	upto   int // while a join's ON is written: the joins before it, which it may name
	parent *fromScope
	err    error // a Join that can't be written
}

type scopeJoin struct {
	src *Source
	b   *builder
}

// source is the builder of the table key's source marks, and the rest of
// key: the FROM's own model's for its fields (a subquery's, then the
// query around it), a joined table's for its.
func (b *builder) source(key string) (*builder, string, error) {
	id, rest, _ := strings.Cut(key[1:], "__")
	v, ok := sources.byID.Load(id)
	if !ok {
		return nil, "", fmt.Errorf("orm: %q names no table", key)
	}
	src := v.(*Source)
	for sc := b.scope; sc != nil; sc = sc.parent {
		if src.alias == "" && sc.root.m.Type == src.t {
			return sc.root, rest, nil
		}
		for i, j := range sc.joined {
			if j.src != src {
				continue
			}
			if sc.upto >= 0 && i > sc.upto {
				return nil, "", fmt.Errorf("orm: %s is joined after this Join: join it first", src.Name())
			}
			return j.b, rest, nil
		}
	}
	return nil, "", fmt.Errorf("orm: %s isn't joined: Join(%sColumns.On(…)) first", src.Name(), src.t.Name())
}

// addJoins writes q's Joins into b's FROM, in their order, before the
// joins its names follow.
func (q query) addJoins(b *builder) error {
	if len(q.joins) == 0 {
		return nil
	}
	sc := b.scope
	quote := b.d.Quote
	taken := map[string]bool{b.alias: true}
	for _, s := range q.joins {
		if s.src == nil {
			return fmt.Errorf("orm: Join takes a model's generated typed fields (orm generate): these have no table")
		}
		kw := " JOIN "
		if s.kind == Left {
			kw = " LEFT JOIN "
		}
		if s.rel != "" {
			if err := q.joinRelation(b, s, kw, taken); err != nil {
				return err
			}
			continue
		}
		t, err := modelOf(s.src.t, q.m.Names, "")
		if err != nil {
			return err
		}
		for _, j := range sc.joined {
			if j.src == s.src {
				return fmt.Errorf("orm: %s is joined twice: join the second under a name of its own, %sColumns.As(\"…\")", s.src.Name(), t.Name)
			}
		}
		alias := t.Table
		if s.src.alias != "" {
			alias = s.src.alias
		}
		if taken[alias] {
			if s.src.alias == "" && s.src.t == q.m.Type {
				return fmt.Errorf("orm: %s is the query's own model: join it under a name of its own, %sColumns.As(\"…\")", t.Name, t.Name)
			}
			return fmt.Errorf("orm: Join: %q names two tables of the query: join %s under another, %sColumns.As(\"…\")", alias, t.Name, t.Name)
		}
		taken[alias] = true
		nb := &builder{d: b.d, m: t, alias: alias, path: "@" + alias, st: b.st, joins: b.joins, scope: sc, explicit: true}
		table := quote(t.Table)
		if alias != t.Table {
			table += " AS " + quote(alias)
		}
		at := len(sc.joined)
		sc.joined = append(sc.joined, scopeJoin{s.src, nb})
		conds := s.conds
		*b.joins = append(*b.joins, joinClause{path: nb.path, b: nb, sql: kw + table + " ON ", explicit: true, on: func() (string, error) {
			return b.joinCond(at, conds)
		}})
	}
	return nil
}

// joinCond is the ON of the join at index at: conds, which may name the
// query's own fields and the tables joined before it, and follow no
// relation (its table would be joined after it).
func (b *builder) joinCond(at int, conds []Cond) (string, error) {
	sc := b.scope
	sc.upto = at
	defer func() { sc.upto = -1 }()
	n := len(*b.joins)
	s, err := And(conds...).sql(b)
	if err != nil {
		return "", err
	}
	if len(*b.joins) > n {
		return "", fmt.Errorf("orm: a Join's condition follows a relation (%s): Join its table first and name its fields", strings.TrimPrefix((*b.joins)[n].path, "@"))
	}
	if s == "" {
		return "1 = 1", nil
	}
	return s, nil
}

// joinRelation joins a relation path of the query's model by its foreign
// key, as a name through it would, under the kind of join s asks: the
// relation's model's typed fields then name its columns.
func (q query) joinRelation(b *builder, s joinSpec, kw string, taken map[string]bool) error {
	if s.src.t != q.m.Type || s.src.alias != "" {
		return fmt.Errorf("orm: Join(%sColumns.%s): a relation joins from the queried model, %s", s.src.t.Name(), s.rel, q.m.Name)
	}
	if err := checkPath(s.rel); err != nil {
		return err
	}
	n := len(*b.joins)
	cur := b
	for part := range strings.SplitSeq(s.rel, "__") {
		r, ok := cur.m.relation(part)
		if !ok {
			return fmt.Errorf("orm: Join: %s has no relation %q", cur.m.Name, part)
		}
		next, err := cur.follow(r)
		if err != nil {
			return err
		}
		cur = next
	}
	for i := n; i < len(*b.joins); i++ {
		j := &(*b.joins)[i]
		j.sql = strings.ReplaceAll(j.sql, " LEFT JOIN ", kw)
		taken[j.b.alias] = true
	}
	src := sourceFor(cur.m.Type, "")
	for _, j := range b.scope.joined {
		if j.src == src {
			return fmt.Errorf("orm: %s is joined twice: join the second under a name of its own, %sColumns.As(\"…\")", src.Name(), cur.m.Name)
		}
	}
	b.scope.joined = append(b.scope.joined, scopeJoin{src, cur})
	if len(s.conds) > 0 {
		last := &(*b.joins)[len(*b.joins)-1]
		at := len(b.scope.joined) - 1
		conds := s.conds
		last.on = func() (string, error) { return b.joinCond(at, conds) }
	}
	return nil
}

// errExpr is an expression that fails the query it is in with err.
type errExpr struct{ err error }

func (e errExpr) exprSQL(*builder) (string, error) { return "", e.err }

// keyed is a typed relation's lookup key: RelationPath's, the foreign key
// when it is one (ProductCategoryColumns.Product() is product_id).
type keyed interface{ lookupKey() string }

func (r RelationPath) lookupKey() string { return sourced(r.src, r.path) }

// queryName is v as a name a query resolves: a string as is, a typed
// field or relation marked with its table.
func queryName(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case typedField:
		return x.key(), true
	case keyed:
		return x.lookupKey(), true
	}
	return "", false
}

func queryNames(vs []any) ([]string, error) {
	out := make([]string, len(vs))
	for i, v := range vs {
		n, ok := queryName(v)
		if !ok {
			return nil, fmt.Errorf("orm: %T names no field", v)
		}
		out[i] = n
	}
	return out, nil
}

// orderKey is an OrderBy term: a name (a leading - descending), a typed
// field ascending, an order term, or an expression ascending.
func orderKey(v any) (sortKey, error) {
	switch x := v.(type) {
	case string:
		return keyOf(x), nil
	case OrderTerm:
		return x.sortKey(), nil
	}
	if n, ok := queryName(v); ok {
		return sortKey{name: n}, nil
	}
	if e, ok := v.(Expr); ok {
		return sortKey{e: e}, nil
	}
	return sortKey{}, fmt.Errorf("orm: OrderBy(%T): order by a field, a typed field or an expression", v)
}

// kindClash is whether a column of type col can't be read into into: text
// into a number, a number into text, a time into anything else.
func kindClash(col, into reflect.Type) bool {
	base := func(t reflect.Type) reflect.Type {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if isOption(t) {
			t = optionElem(t)
		}
		return t
	}
	a, b := base(col), base(into)
	if a == b || b.Kind() == reflect.Interface || reflect.PointerTo(b).Implements(scannerType) {
		return false
	}
	kind := func(t reflect.Type) int {
		switch {
		case t == timeType:
			return 3
		case t.Kind() == reflect.String:
			return 1
		case t.Kind() >= reflect.Int && t.Kind() <= reflect.Float64:
			return 2
		}
		return 0
	}
	ka, kb := kind(a), kind(b)
	return ka != 0 && kb != 0 && ka != kb
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
