package orm

// Field is a model's field as a typed lookup. ormgen writes one per column
// in each model's typed fields (UserColumns.Age, PostColumns.Author().Email),
// so a misspelled field or a value of the wrong type fails to compile
// instead of failing the query; the methods build the same conditions as
// Q:
//
//	orm.For[User]().Filter(UserColumns.Age.Gte(18), UserColumns.Name.IContains("al")).
//		OrderBy(UserColumns.Name.Desc())
//
// A pointer or an Option field is a Field of what it holds (*int and
// vuka.Option[int] are Field[int]); IsNull matches its NULLs.
type Field[V any] struct {
	path string
	src  *Source // the table the path starts from; nil: the queried model
}

// FieldAt is the field at a lookup path (a column, or relations to one:
// "author__name") of the queried model.
func FieldAt[V any](path string) Field[V] { return Field[V]{path: path} }

// FieldIn is the field at a lookup path from src's table: the queried
// model's, or a table a query joins. Generated typed fields call it.
func FieldIn[V any](src *Source, path string) Field[V] { return Field[V]{path, src} }

// key is the field as a lookup key: its path, marked with its table.
func (f Field[V]) key() string { return sourced(f.src, f.path) }

// Source is the table the field is of: nil for a field of whichever
// model a query is of.
func (f Field[V]) Source() *Source { return f.src }

// typedField is a Field of any type, which names a field where names go
// (SaveFields).
type typedField interface {
	Name() string
	typed()
	key() string
}

func (Field[V]) typed() {}

// Name is the field's lookup path.
func (f Field[V]) Name() string { return f.path }

// String is the field's lookup path.
func (f Field[V]) String() string { return f.path }

// Asc and Desc are the field for OrderBy.
func (f Field[V]) Asc() string  { return f.key() }
func (f Field[V]) Desc() string { return "-" + f.key() }

// Expr is the field as an expression: F(name).
func (f Field[V]) Expr() Expr { return F(f.key()) }

func (f Field[V]) Eq(v V) Cond  { return Q{f.key(): v} }
func (f Field[V]) Ne(v V) Cond  { return Not(Q{f.key(): v}) }
func (f Field[V]) Gt(v V) Cond  { return Q{f.key() + "__gt": v} }
func (f Field[V]) Gte(v V) Cond { return Q{f.key() + "__gte": v} }
func (f Field[V]) Lt(v V) Cond  { return Q{f.key() + "__lt": v} }
func (f Field[V]) Lte(v V) Cond { return Q{f.key() + "__lte": v} }

// In matches any of vs; none matches nothing.
func (f Field[V]) In(vs ...V) Cond { return Q{f.key() + "__in": vs} }

// Range matches from lo to hi, both included.
func (f Field[V]) Range(lo, hi V) Cond { return Q{f.key() + "__range": []V{lo, hi}} }

// IsNull matches a NULL column with true, a set one with false.
func (f Field[V]) IsNull(null bool) Cond { return Q{f.key() + "__isnull": null} }

// EqCol holds where the field equals another column or a typed
// expression of the same type: ProductColumns.Code.EqCol(LegacyItemColumns.Code).
func (f Field[V]) EqCol(c ColumnOf[V]) Cond { return Q{f.key(): c} }

// ColumnOf is a typed field or expression of V values, which EqCol compares
// with a field.
type ColumnOf[V any] interface {
	Expr
	column(V)
}

func (Field[V]) column(V) {}

func (f Field[V]) exprSQL(b *builder) (string, error) { return b.ref(f.key()) }

// InQuery matches the values a subquery selects: a QuerySet (its keys)
// or a Values of one column.
func (f Field[V]) InQuery(sub subquerier) Cond { return Q{f.key() + "__in": sub} }

// TextField is a string field: a Field with the text lookups.
type TextField[V ~string] struct{ Field[V] }

// TextFieldAt is the text field at a lookup path.
func TextFieldAt[V ~string](path string) TextField[V] { return TextField[V]{FieldAt[V](path)} }

// TextFieldIn is the text field at a lookup path from src's table.
func TextFieldIn[V ~string](src *Source, path string) TextField[V] {
	return TextField[V]{FieldIn[V](src, path)}
}

func (f TextField[V]) IExact(s string) Cond      { return Q{f.key() + "__iexact": s} }
func (f TextField[V]) Contains(s string) Cond    { return Q{f.key() + "__contains": s} }
func (f TextField[V]) IContains(s string) Cond   { return Q{f.key() + "__icontains": s} }
func (f TextField[V]) StartsWith(s string) Cond  { return Q{f.key() + "__startswith": s} }
func (f TextField[V]) IStartsWith(s string) Cond { return Q{f.key() + "__istartswith": s} }
func (f TextField[V]) EndsWith(s string) Cond    { return Q{f.key() + "__endswith": s} }
func (f TextField[V]) IEndsWith(s string) Cond   { return Q{f.key() + "__iendswith": s} }

// Number is a type a NumberField holds: an integer or a float.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// NumberField is a number field, which arithmetic takes: an Update's new
// value computed from the row's own,
//
//	Posts.Filter(f.Slug.Eq(slug)).Update(ctx, orm.Set{"views": f.Views.Add(1)})
type NumberField[V Number] struct{ Field[V] }

// NumberFieldAt is the number field at a lookup path. Generated typed
// fields call it.
func NumberFieldAt[V Number](path string) NumberField[V] { return NumberField[V]{FieldAt[V](path)} }

// NumberFieldIn is the number field at a lookup path from src's table.
func NumberFieldIn[V Number](src *Source, path string) NumberField[V] {
	return NumberField[V]{FieldIn[V](src, path)}
}

// Add, Sub, Mul and Div are the field and n combined, as an expression:
// f.Views.Add(1) is views + 1. Div divides as the database does, an
// integer column by an integer in whole numbers on SQLite and Postgres.
func (f NumberField[V]) Add(n V) NumExpr[V] { return Num[V](f.Expr()).Add(n) }
func (f NumberField[V]) Sub(n V) NumExpr[V] { return Num[V](f.Expr()).Sub(n) }
func (f NumberField[V]) Mul(n V) NumExpr[V] { return Num[V](f.Expr()).Mul(n) }
func (f NumberField[V]) Div(n V) NumExpr[V] { return Num[V](f.Expr()).Div(n) }

// NumExpr is an OrderedExpr of numbers, which arithmetic takes further:
// f.Views.Add(1).Mul(2) is (views + 1) * 2.
type NumExpr[V Number] struct{ OrderedExpr[V] }

// Num is e as an expression of numbers.
//
//	orm.Num[int](orm.F("views")).Add(1)
func Num[V Number](e Expr) NumExpr[V] { return NumExpr[V]{Ordered[V](e)} }

// Add, Sub, Mul and Div are the expression and n combined.
func (e NumExpr[V]) Add(n V) NumExpr[V] { return e.op("+", n) }
func (e NumExpr[V]) Sub(n V) NumExpr[V] { return e.op("-", n) }
func (e NumExpr[V]) Mul(n V) NumExpr[V] { return e.op("*", n) }
func (e NumExpr[V]) Div(n V) NumExpr[V] { return e.op("/", n) }

func (e NumExpr[V]) op(op string, n V) NumExpr[V] {
	return Num[V](SQL("({0} "+op+" {1})", e.e, n))
}
