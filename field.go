package orm

// Field is a model's field as a typed lookup. ormgen writes one per column
// in each model's field set (UserFields.Age, …), so a misspelled field or
// a value of the wrong type fails to compile instead of failing the
// query; the methods build the same conditions as Q:
//
//	Users.Filter(UserFields.Age.Gte(18), UserFields.Name.IContains("al")).
//		OrderBy(UserFields.Name.Desc())
type Field[V any] struct{ path string }

// FieldAt is the field at a lookup path (a column, or relations to one:
// "author__name"). Generated field sets call it.
func FieldAt[V any](path string) Field[V] { return Field[V]{path} }

// Name is the field's lookup path.
func (f Field[V]) Name() string { return f.path }

// String is the field's lookup path.
func (f Field[V]) String() string { return f.path }

// Asc and Desc are the field for OrderBy.
func (f Field[V]) Asc() string  { return f.path }
func (f Field[V]) Desc() string { return "-" + f.path }

// Expr is the field as an expression: F(name).
func (f Field[V]) Expr() Expr { return F(f.path) }

func (f Field[V]) Eq(v V) Cond  { return Q{f.path: v} }
func (f Field[V]) Ne(v V) Cond  { return Not(Q{f.path: v}) }
func (f Field[V]) Gt(v V) Cond  { return Q{f.path + "__gt": v} }
func (f Field[V]) Gte(v V) Cond { return Q{f.path + "__gte": v} }
func (f Field[V]) Lt(v V) Cond  { return Q{f.path + "__lt": v} }
func (f Field[V]) Lte(v V) Cond { return Q{f.path + "__lte": v} }

// In matches any of vs; none matches nothing.
func (f Field[V]) In(vs ...V) Cond { return Q{f.path + "__in": vs} }

// Range matches from lo to hi, both included.
func (f Field[V]) Range(lo, hi V) Cond { return Q{f.path + "__range": []V{lo, hi}} }

// IsNull matches a NULL column with true, a set one with false.
func (f Field[V]) IsNull(null bool) Cond { return Q{f.path + "__isnull": null} }

// InQuery matches the values a subquery selects: a QuerySet (its keys)
// or a Values of one column.
func (f Field[V]) InQuery(sub subquerier) Cond { return Q{f.path + "__in": sub} }

// TextField is a string field: a Field with the text lookups.
type TextField[V ~string] struct{ Field[V] }

// TextFieldAt is the text field at a lookup path.
func TextFieldAt[V ~string](path string) TextField[V] { return TextField[V]{Field[V]{path}} }

func (f TextField[V]) IExact(s string) Cond      { return Q{f.path + "__iexact": s} }
func (f TextField[V]) Contains(s string) Cond    { return Q{f.path + "__contains": s} }
func (f TextField[V]) IContains(s string) Cond   { return Q{f.path + "__icontains": s} }
func (f TextField[V]) StartsWith(s string) Cond  { return Q{f.path + "__startswith": s} }
func (f TextField[V]) IStartsWith(s string) Cond { return Q{f.path + "__istartswith": s} }
func (f TextField[V]) EndsWith(s string) Cond    { return Q{f.path + "__endswith": s} }
func (f TextField[V]) IEndsWith(s string) Cond   { return Q{f.path + "__iendswith": s} }
