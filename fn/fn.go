// Package fn is the ORM's expressions over Vuka field references, typed:
// each takes field references (Post.Title, Post.Author.Name) or other
// terms and gives an orm.Term of the Go type the database computes, so
// only the comparisons that make sense type-check.
//
//	Post.Objects.Filter(fn.Lower(Post.Title).Eq("go"), fn.Year(Post.Published).Ge(2025))
//	Post.Objects.Annotate(fn.Count(Post.Tags).As("tags")).OrderBy(fn.Length(Post.Title).Desc())
//	Post.Objects.Update(ctx, orm.AssignF(Post.Views, fn.Num(Post.Views).Add(1)))
//
// The names are the engine's (orm.Count, orm.Lower, …), which take field
// names as strings; this package is their Vuka form.
//
// Deprecated: Vuka now writes a model's field as its typed field
// (PostFields.Title), whose Expr and Name the engine's functions take:
// orm.Lower(Post.Title.Expr()). Package fn goes in the next minor release.
package fn

import (
	"cmp"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/vuka"
)

// Number is what Sum and arithmetic take.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// Num, Str, Val and Time make a field reference (or term) a term, for
// arithmetic and comparisons with other fields: fn.Num(Post.Views).Add(1),
// fn.Num(Post.Views).GtF(Post.Likes).
func Num[T any, V cmp.Ordered](a orm.Arg[T, V]) orm.OrderedTerm[T, V] {
	return orm.OrderedTermOf[T, V](a)
}
func Str[T any, V ~string](a orm.Arg[T, V]) orm.StringTerm[T, V] { return orm.StringTermOf[T, V](a) }
func Val[T, V any](a orm.Arg[T, V]) orm.Term[T, V]               { return orm.TermOf[T, V](a) }
func Time[T any](a orm.Arg[T, time.Time]) orm.CompareTerm[T, time.Time] {
	return orm.CompareTermOf[T, time.Time](a)
}

// Lit is a value as a term of the model T: fn.Lit[Post]("PLACEMENT").
func Lit[T, V any](v V) orm.Term[T, V] { return orm.TermOf[T, V](orm.Value(v)) }

// Lower, Upper and Trim are the string functions.
func Lower[T any, V ~string](a orm.Arg[T, V]) orm.StringTerm[T, V] {
	return orm.StringTermOf[T, V](orm.Lower.Of(a))
}
func Upper[T any, V ~string](a orm.Arg[T, V]) orm.StringTerm[T, V] {
	return orm.StringTermOf[T, V](orm.Upper.Of(a))
}
func Trim[T any, V ~string](a orm.Arg[T, V]) orm.StringTerm[T, V] {
	return orm.StringTermOf[T, V](orm.Trim.Of(a))
}

// Length is a string's length in characters.
func Length[T any, V ~string](a orm.Arg[T, V]) orm.OrderedTerm[T, int64] {
	return orm.OrderedTermOf[T, int64](orm.Length.Of(a))
}

// Abs is a number's absolute value.
func Abs[T any, V Number](a orm.Arg[T, V]) orm.OrderedTerm[T, V] {
	return orm.OrderedTermOf[T, V](orm.Abs.Of(a))
}

// Year, Month and Day are parts of a time (a time.Time, *time.Time or
// Option[time.Time] field); Date its date.
func Year[T, V any](a orm.Arg[T, V]) orm.OrderedTerm[T, int64] {
	return orm.OrderedTermOf[T, int64](orm.Year.Of(a))
}
func Month[T, V any](a orm.Arg[T, V]) orm.OrderedTerm[T, int64] {
	return orm.OrderedTermOf[T, int64](orm.Month.Of(a))
}
func Day[T, V any](a orm.Arg[T, V]) orm.OrderedTerm[T, int64] {
	return orm.OrderedTermOf[T, int64](orm.Day.Of(a))
}
func Date[T, V any](a orm.Arg[T, V]) orm.Term[T, string] {
	return orm.TermOf[T, string](orm.Date.Of(a))
}

// Coalesce is the first of its arguments that isn't NULL; the term is of
// the first's type.
func Coalesce[T, V any](a orm.Arg[T, V], rest ...any) orm.Term[T, V] {
	return orm.TermOf[T, V](orm.Coalesce.Of(append([]any{a}, rest...)...))
}

// Cast converts to the database type t (orm.AsInt, orm.AsText, …); R is
// the Go type it reads as: fn.Cast[int64](Post.Code, orm.AsInt).
func Cast[R, T, V any](a orm.Arg[T, V], t orm.CastType) orm.Term[T, R] {
	return orm.TermOf[T, R](orm.Cast(a, t))
}

// SQL is a template ({0}, {1}, …) of field references, terms and values,
// typed R: the escape hatch for what has no function.
func SQL[R, T, V any](template string, a orm.Arg[T, V], rest ...any) orm.Term[T, R] {
	return orm.TermOf[T, R](orm.SQL(template, append([]any{a}, rest...)...))
}

// Call is a database function — orm.Function's, or a typed orm.Func's —
// called on field references and values, as a term of T typed R:
//
//	var unaccent = orm.Function("unaccent")
//	fn.Str(fn.Call[string](unaccent, Post.Title)).IContains("jose")
func Call[R, T, V any](f interface{ Of(args ...any) orm.Expr }, a orm.Arg[T, V], rest ...any) orm.Term[T, R] {
	return orm.TermOf[T, R](f.Of(append([]any{a}, rest...)...))
}

// Agg is an aggregate term: Filter and Distinct give its variants.
type Agg[T any, V cmp.Ordered] struct {
	orm.OrderedTerm[T, V]
	agg orm.Agg
}

func aggOf[T any, V cmp.Ordered](a orm.Agg) Agg[T, V] {
	return Agg[T, V]{orm.OrderedTermOf[T, V](a), a}
}

// Filter aggregates only the rows p holds for (Django's filter=).
func (a Agg[T, V]) Filter(p vuka.Pred[T]) Agg[T, V] {
	return aggOf[T, V](a.agg.Filter(orm.CondOf(p)))
}

// Distinct aggregates each distinct value once.
func (a Agg[T, V]) Distinct() Agg[T, V] { return aggOf[T, V](a.agg.Distinct()) }

// Count, Sum, Avg, Min and Max aggregate a field: of the rows, of
// related rows through a relation (fn.Count(Post.Tags) counts tags).
func Count[T any](p orm.Path[T]) Agg[T, int64]       { return aggOf[T, int64](orm.Count(orm.FieldName(p))) }
func Sum[T any, V Number](a orm.Arg[T, V]) Agg[T, V] { return aggOf[T, V](orm.Sum(name(a))) }
func Avg[T any, V Number](a orm.Arg[T, V]) Agg[T, float64] {
	return aggOf[T, float64](orm.Avg(name(a)))
}
func Min[T any, V cmp.Ordered](a orm.Arg[T, V]) Agg[T, V] { return aggOf[T, V](orm.Min(name(a))) }
func Max[T any, V cmp.Ordered](a orm.Arg[T, V]) Agg[T, V] { return aggOf[T, V](orm.Max(name(a))) }

func name(a any) string {
	if p, ok := a.(interface{ FieldPath() vuka.FieldPath }); ok {
		return orm.LookupPath(p.FieldPath())
	}
	return ""
}
