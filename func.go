package orm

import (
	"cmp"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FuncDef is a database function's definition: its name, its SQL on each
// dialect, how many arguments it takes, whether it aggregates. Function
// makes an untyped one, called with Of; Func, OrderedFunc and TextFunc
// make typed ones, called directly.
//
//	var Unaccent = orm.Function("unaccent")                 // unaccent(…)
//	var Year = orm.Function("year",
//		orm.Template("postgres", "CAST(EXTRACT(YEAR FROM {0}) AS INTEGER)"),
//		orm.Template("sqlite", "CAST(strftime('%Y', {0}) AS INTEGER)"))  // YEAR(…) on MySQL
//
//	Users.Filter(orm.Where(Unaccent.Of(orm.F("name")), "icontains", "jose"))
//
// A FuncDef registered with Transform is also a step of Q keys:
// Q{"name__unaccent__icontains": "jose"}.
type FuncDef struct {
	name  string
	per   map[string]string // dialect → template; "" for every dialect
	arity int               // -1: any number
	agg   bool
	trans bool
	loose bool // Function's: a dialect no Template names calls name(args…)
}

// FuncOption shapes a function's definition.
type FuncOption func(*FuncDef)

// Template is the function's SQL on a dialect ("postgres", "mysql",
// "sqlite"; "" for all of them), {0}, {1}, … for its arguments and {*}
// for all of them. A typed function with Templates is written only where
// one applies: on another dialect its query fails, naming the function
// and the dialect.
func Template(dialect, template string) FuncOption {
	return func(f *FuncDef) { f.per[strings.ToLower(dialect)] = template }
}

// Arity is how many arguments the function takes: a call with another
// number fails its query.
func Arity(n int) FuncOption { return func(f *FuncDef) { f.arity = n } }

// IsAggregate makes the function an aggregate, as Count and Sum are: an
// annotation of it groups a Values listing it by the other names, and a
// condition on it is HAVING's.
func IsAggregate() FuncOption { return func(f *FuncDef) { f.agg = true } }

func newFuncDef(name string, loose bool, opts []FuncOption) *FuncDef {
	f := &FuncDef{name: name, per: map[string]string{}, arity: -1, loose: loose}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Function defines an untyped database function, called with Of. Without
// a Template for the dialect a query runs on it is called as name(args…).
func Function(name string, opts ...FuncOption) *FuncDef { return newFuncDef(name, true, opts) }

// Name is the function's name.
func (f *FuncDef) Name() string { return f.name }

// Of calls the function: each argument an Expr (F for a field) or a value,
// sent as a bound argument, never written into the SQL.
func (f *FuncDef) Of(args ...any) Expr { return &call{f: f, args: args} }

var transforms sync.Map // lowercased name → *FuncDef

// Transform makes a one-argument function a step of Q keys, by its name,
// between a field and its lookup: Q{"created_at__year__gte": 2026}. It
// returns f, so a declaration can end with it.
func (f *FuncDef) Transform() *FuncDef {
	if f.arity >= 0 && f.arity != 1 {
		panic(fmt.Sprintf("orm: %s takes %d arguments: a transform takes one", f.name, f.arity))
	}
	f.trans = true
	transforms.Store(strings.ToLower(f.name), f)
	return f
}

func transform(name string) (*FuncDef, bool) {
	v, ok := transforms.Load(strings.ToLower(name))
	if !ok {
		return nil, false
	}
	return v.(*FuncDef), true
}

type call struct {
	f    *FuncDef
	args []any
}

func (c *call) exprSQL(b *builder) (string, error) {
	f := c.f
	if f.arity >= 0 && len(c.args) != f.arity {
		return "", fmt.Errorf("orm: %s takes %d argument(s), got %d", f.name, f.arity, len(c.args))
	}
	tmpl, ok := f.per[b.d.Name()]
	if !ok {
		tmpl, ok = f.per[""]
	}
	if !ok {
		if len(f.per) > 0 && !f.loose {
			return "", fmt.Errorf("orm: function %s has no SQL on %s: give it a Template(%q, …)", f.name, b.d.Name(), b.d.Name())
		}
		tmpl = f.name + "({*})"
	}
	return render(b, tmpl, c.args)
}

func (c *call) isAggregate() bool { return c.f.agg }
func (c *call) funcDef() *FuncDef { return c.f }

// aggregator is an expression that may aggregate: Agg, an aggregate
// function's call, a typed expression over one.
type aggregator interface{ isAggregate() bool }

func (Agg) isAggregate() bool { return true }

// isAggregate is whether e aggregates rows.
func isAggregate(e Expr) bool {
	a, ok := e.(aggregator)
	return ok && a.isAggregate()
}

// aggExpr is whether e aggregates, or names an annotation that does.
func (b *builder) aggExpr(e Expr) bool {
	if isAggregate(e) {
		return true
	}
	switch e := e.(type) {
	case fieldRef:
		return isAggregate(b.ann[string(e)])
	case interface{ unwrap() Expr }:
		return b.aggExpr(e.unwrap())
	}
	return false
}

// Orderable is a type whose values are ordered: numbers, strings, times.
type Orderable interface{ cmp.Ordered | time.Time }

// TypedExpr is an expression whose value is an R: a typed function's
// call, or any Expr given its type with Typed. Its comparisons take R, so
// a condition on it type-checks; it is an Expr, so it goes in Annotate,
// Values (through an annotation), Update's Set, a Case, and Where with
// any lookup.
//
// Go can't give a generic type's methods to some of its type arguments
// only, so the comparisons a type has come with the constructor:
// TypedExpr (Func) has those of every type, OrderedExpr (OrderedFunc)
// adds Lt, Le, Gt, Ge and Between, and TextExpr (TextFunc) adds the
// pattern matches. Contains on a number doesn't compile.
type TypedExpr[R any] struct{ e Expr }

// OrderedExpr is a TypedExpr whose values are ordered.
type OrderedExpr[R Orderable] struct{ TypedExpr[R] }

// TextExpr is an OrderedExpr of text, matched by patterns too.
type TextExpr struct{ OrderedExpr[string] }

// Typed is e as an expression of R values.
//
//	orm.Typed[bool](orm.F("active")).Eq(true)
func Typed[R any](e Expr) TypedExpr[R] { return TypedExpr[R]{e} }

// Ordered is e as an expression of ordered R values.
//
//	orm.Ordered[int](orm.F("age")).Between(18, 30)
func Ordered[R Orderable](e Expr) OrderedExpr[R] { return OrderedExpr[R]{TypedExpr[R]{e}} }

// Text is e as an expression of text.
//
//	orm.Text(orm.F("title")).IContains("go")
func Text(e Expr) TextExpr { return TextExpr{Ordered[string](e)} }

func (t TypedExpr[R]) exprSQL(b *builder) (string, error) {
	if t.e == nil {
		return "", fmt.Errorf("orm: an empty %T", t)
	}
	return t.e.exprSQL(b)
}

func (t TypedExpr[R]) unwrap() Expr      { return t.e }
func (t TypedExpr[R]) isAggregate() bool { return isAggregate(t.e) }

func (t TypedExpr[R]) funcDef() *FuncDef {
	if c, ok := t.e.(*call); ok {
		return c.f
	}
	return nil
}

// Eq holds where the expression equals v.
func (t TypedExpr[R]) Eq(v R) Cond { return Where(t, "exact", v) }

// Ne holds where the expression doesn't equal v: NOT (… = v), unknown
// where the expression is NULL, as SQL has it.
func (t TypedExpr[R]) Ne(v R) Cond { return Not(Where(t, "exact", v)) }

// In holds where the expression equals one of vs; none matches nothing.
func (t TypedExpr[R]) In(vs ...R) Cond { return Where(t, "in", vs) }

// IsNull holds where the expression is NULL.
func (t TypedExpr[R]) IsNull() Cond { return Where(t, "isnull", true) }

// NotNull holds where the expression isn't NULL.
func (t TypedExpr[R]) NotNull() Cond { return Where(t, "isnull", false) }

// Asc orders by the expression, smallest first: Order's term.
func (t TypedExpr[R]) Asc() ExprOrder { return Asc(t) }

// Desc orders by the expression, largest first.
func (t TypedExpr[R]) Desc() ExprOrder { return Desc(t) }

// As names the expression: AnnotateAs's annotation.
func (t TypedExpr[R]) As(alias string) Alias { return Alias{alias, t} }

// Over makes the expression a window function's: computed per row over
// the rows of its partition, in its order — an aggregate over a window no
// longer groups.
//
//	RowNumber := orm.OrderedFunc[int64]("row_number", orm.Arity(0))
//	Users.AnnotateAs(RowNumber().Over(orm.Window{PartitionBy: []string{"team_id"}, OrderBy: []string{"-age"}}).As("rank"))
func (t TypedExpr[R]) Over(w Window) TypedExpr[R] { return TypedExpr[R]{overExpr{t.e, w}} }

// Lt holds where the expression is less than v.
func (t OrderedExpr[R]) Lt(v R) Cond { return Where(t, "lt", v) }

// Le holds where the expression is at most v.
func (t OrderedExpr[R]) Le(v R) Cond { return Where(t, "lte", v) }

// Gt holds where the expression is more than v.
func (t OrderedExpr[R]) Gt(v R) Cond { return Where(t, "gt", v) }

// Ge holds where the expression is at least v.
func (t OrderedExpr[R]) Ge(v R) Cond { return Where(t, "gte", v) }

// Between holds where the expression is from lo to hi, both included.
func (t OrderedExpr[R]) Between(lo, hi R) Cond { return Where(t, "range", []R{lo, hi}) }

// Over is TypedExpr's Over, keeping the expression ordered.
func (t OrderedExpr[R]) Over(w Window) OrderedExpr[R] {
	return OrderedExpr[R]{t.TypedExpr.Over(w)}
}

// Contains holds where the text contains s, matched literally.
func (t TextExpr) Contains(s string) Cond { return Where(t, "contains", s) }

// IContains is Contains ignoring case.
func (t TextExpr) IContains(s string) Cond { return Where(t, "icontains", s) }

// StartsWith holds where the text starts with s.
func (t TextExpr) StartsWith(s string) Cond { return Where(t, "startswith", s) }

// EndsWith holds where the text ends with s.
func (t TextExpr) EndsWith(s string) Cond { return Where(t, "endswith", s) }

// Like holds where the text matches a LIKE pattern of your own: % any
// run of characters, _ any one (case-sensitive or not as the database's
// LIKE is). The pattern is a bound argument.
func (t TextExpr) Like(pattern string) Cond { return Where(t, "like", pattern) }

// Over is TypedExpr's Over, keeping the expression text.
func (t TextExpr) Over(w Window) TextExpr { return TextExpr{t.OrderedExpr.Over(w)} }

// Fn is a typed function: called with its arguments — each an Expr (F
// for a field) or a value, sent as a bound argument — it is an expression
// of type X. Func, OrderedFunc and TextFunc make one.
type Fn[X interface {
	Expr
	funcDef() *FuncDef
}] func(args ...any) X

// FuncOf is a function of R values, made by Func.
type FuncOf[R any] = Fn[TypedExpr[R]]

// OrderedFuncOf is a function of ordered R values, made by OrderedFunc.
type OrderedFuncOf[R Orderable] = Fn[OrderedExpr[R]]

// TextFuncOf is a function of text, made by TextFunc.
type TextFuncOf = Fn[TextExpr]

// Def is the function's definition.
func (f Fn[X]) Def() *FuncDef { return f().funcDef() }

// Name is the function's name.
func (f Fn[X]) Name() string { return f.Def().name }

// Of is the call as an untyped Expr, as a Function's Of.
func (f Fn[X]) Of(args ...any) Expr { return f(args...) }

// Transform makes the function a step of Q keys, as FuncDef's Transform.
func (f Fn[X]) Transform() Fn[X] {
	f.Def().Transform()
	return f
}

// Func defines a database function whose value is an R, called directly:
//
//	var Levenshtein = orm.Func[int]("levenshtein",
//		orm.Template("postgres", "levenshtein({0}, {1})"),   // fuzzystrmatch
//		orm.Template("sqlite", "editdist({0}, {1})"))         // a Go function: see orm/sqlite's Func
//
//	Users.Filter(Levenshtein(orm.F("name"), "jon").Eq(1))
//
// OrderedFunc and TextFunc make functions whose values compare by order
// and by pattern too. Without a Template it is called as name(args…);
// with some, on their dialects only.
func Func[R any](name string, opts ...FuncOption) FuncOf[R] {
	d := newFuncDef(name, false, opts)
	return func(args ...any) TypedExpr[R] { return TypedExpr[R]{&call{d, args}} }
}

// OrderedFunc is Func of ordered values: numbers, strings, times.
//
//	var Similarity = orm.OrderedFunc[float64]("similarity")
//	Users.Filter(Similarity(orm.F("name"), "amna").Gt(0.3)).Order(Similarity(orm.F("name"), "amna").Desc())
func OrderedFunc[R Orderable](name string, opts ...FuncOption) OrderedFuncOf[R] {
	d := newFuncDef(name, false, opts)
	return func(args ...any) OrderedExpr[R] { return OrderedExpr[R]{TypedExpr[R]{&call{d, args}}} }
}

// TextFunc is Func of text.
//
//	var Unaccent = orm.TextFunc("unaccent", orm.Arity(1)).Transform()
//	Users.Filter(Unaccent(orm.F("name")).IContains("jose"))   // or Q{"name__unaccent__icontains": "jose"}
func TextFunc(name string, opts ...FuncOption) TextFuncOf {
	d := newFuncDef(name, false, opts)
	return func(args ...any) TextExpr { return TextExpr{OrderedExpr[string]{TypedExpr[string]{&call{d, args}}}} }
}

// Aggregate is Func of an aggregate, IsAggregate's: an annotation of it
// groups, a condition on it is HAVING's, and AggOf takes it to Aggregate.
// OrderedFunc and TextFunc take IsAggregate for ordered and text
// aggregates.
//
//	var StringAgg = orm.TextFunc("string_agg", orm.IsAggregate(),
//		orm.Template("postgres", "string_agg({0}, {1})"),
//		orm.Template("sqlite", "group_concat({0}, {1})"))
//	Users.Annotate("names", StringAgg(orm.F("name"), ", ")).Values[[]any]("team_id", "names")
func Aggregate[R any](name string, opts ...FuncOption) FuncOf[R] {
	return Func[R](name, append(opts[:len(opts):len(opts)], IsAggregate())...)
}

// Functions every model can use, transforms all: Q{"name__lower": "ali"},
// Q{"created_at__year": 2026}, Q{"created_at__date": "2026-10-06"}.
// Called, each is typed: orm.Lower(orm.F("name")).StartsWith("a"); Of
// gives the untyped call. Date's value is the date as text, YYYY-MM-DD,
// which every database compares with a date.
var (
	Lower  = TextFunc("lower", Arity(1)).Transform()
	Upper  = TextFunc("upper", Arity(1)).Transform()
	Trim   = TextFunc("trim", Arity(1)).Transform()
	Length = OrderedFunc[int64]("length", Arity(1),
		Template("mysql", "CHAR_LENGTH({0})"), Template("", "length({0})")).Transform()
	Abs  = OrderedFunc[float64]("abs", Arity(1)).Transform()
	Year = OrderedFunc[int]("year", Arity(1),
		Template("postgres", "CAST(EXTRACT(YEAR FROM {0}) AS INTEGER)"),
		Template("mysql", "year({0})"),
		Template("sqlite", "CAST(strftime('%Y', {0}) AS INTEGER)")).Transform()
	Month = OrderedFunc[int]("month", Arity(1),
		Template("postgres", "CAST(EXTRACT(MONTH FROM {0}) AS INTEGER)"),
		Template("mysql", "month({0})"),
		Template("sqlite", "CAST(strftime('%m', {0}) AS INTEGER)")).Transform()
	Day = OrderedFunc[int]("day", Arity(1),
		Template("postgres", "CAST(EXTRACT(DAY FROM {0}) AS INTEGER)"),
		Template("mysql", "day({0})"),
		Template("sqlite", "CAST(strftime('%d', {0}) AS INTEGER)")).Transform()
	dateLegacy = OrderedFunc[string]("date", Arity(1),
		Template("postgres", "CAST({0} AS DATE)"),
		Template("mysql", "DATE({0})"),
		Template("sqlite", "date({0})")).Transform()
	Coalesce = Func[any]("coalesce")
)

// Window is the rows a window function (Over) computes over: those
// sharing the PartitionBy names' values (all rows without), in OrderBy's
// order (a leading - for descending). Names are fields, paths through
// relations or annotations.
type Window struct {
	PartitionBy []string
	OrderBy     []string
}

type overExpr struct {
	e Expr
	w Window
}

func (o overExpr) exprSQL(b *builder) (string, error) {
	s, err := o.e.exprSQL(b)
	if err != nil {
		return "", err
	}
	var parts []string
	if len(o.w.PartitionBy) > 0 {
		cols := make([]string, len(o.w.PartitionBy))
		for i, n := range o.w.PartitionBy {
			if cols[i], err = b.ref(n); err != nil {
				return "", err
			}
		}
		parts = append(parts, "PARTITION BY "+strings.Join(cols, ", "))
	}
	if len(o.w.OrderBy) > 0 {
		cols := make([]string, len(o.w.OrderBy))
		for i, n := range o.w.OrderBy {
			dir := " ASC"
			if strings.HasPrefix(n, "-") {
				n, dir = n[1:], " DESC"
			}
			col, err := b.ref(n)
			if err != nil {
				return "", err
			}
			cols[i] = col + dir
		}
		parts = append(parts, "ORDER BY "+strings.Join(cols, ", "))
	}
	return s + " OVER (" + strings.Join(parts, " ") + ")", nil
}

// Alias is an expression under a name, As's: AnnotateAs annotates the
// query with it.
type Alias struct {
	name string
	e    Expr
}

// Name is the alias.
func (a Alias) Name() string { return a.name }

func (a Alias) exprSQL(b *builder) (string, error) { return a.e.exprSQL(b) }
func (a Alias) unwrap() Expr                       { return a.e }
func (a Alias) isAggregate() bool                  { return isAggregate(a.e) }

// OrderTerm is a term of Order: an Ordering (a field path) or an
// expression's Asc or Desc.
type OrderTerm interface{ sortKey() sortKey }

// sortKey is an ORDER BY term: a name (a field, a path, an annotation) or
// an expression, and its direction.
type sortKey struct {
	name string
	e    Expr
	desc bool
}

// keyOf is an OrderBy string as a term: a leading - for descending.
func keyOf(s string) sortKey {
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		return sortKey{name: rest, desc: true}
	}
	return sortKey{name: s}
}

func keysOf(names []string) []sortKey {
	if names == nil {
		return nil
	}
	out := make([]sortKey, len(names))
	for i, n := range names {
		out[i] = keyOf(n)
	}
	return out
}

func (o Ordering) sortKey() sortKey { return sortKey{name: strings.Join(o.Path, "__"), desc: o.Desc} }

// ExprOrder is an expression to order by and its direction.
type ExprOrder struct {
	e    Expr
	desc bool
}

func (o ExprOrder) sortKey() sortKey { return sortKey{e: o.e, desc: o.desc} }

// Asc orders by e, smallest first.
func Asc(e Expr) ExprOrder { return ExprOrder{e, false} }

// Desc orders by e, largest first.
func Desc(e any) ExprOrder {
	if name, ok := e.(string); ok {
		return ExprOrder{F(name), true}
	}
	return ExprOrder{ExprOf(e), true}
}
