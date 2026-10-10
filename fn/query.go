package fn

import (
	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/vuka"
)

// Branch is a Case's: a predicate and the result where it holds.
type Branch[T, V any] struct {
	when orm.WhenClause
}

// When is a Case branch: then where p holds.
func When[T, V any](p vuka.Pred[T], then V) Branch[T, V] {
	return Branch[T, V]{orm.When(orm.CondOf(p), orm.ExprOf(then))}
}

// CaseTerm is a Case: the result of its first branch whose predicate
// holds, else Else's, else NULL.
type CaseTerm[T, V any] struct {
	orm.Term[T, V]
	c *orm.CaseExpr
}

// Case is Django's Case/When, typed by its results:
//
//	fn.Case(fn.When(Post.Views.Gt(100), "hot"), fn.When(Post.Views.Gt(10), "warm")).Else("cold")
func Case[T, V any](branches ...Branch[T, V]) CaseTerm[T, V] {
	ws := make([]orm.WhenClause, len(branches))
	for i, b := range branches {
		ws[i] = b.when
	}
	c := orm.Case(ws...)
	return CaseTerm[T, V]{orm.TermOf[T, V](c), c}
}

// Else is the result where no branch holds.
func (c CaseTerm[T, V]) Else(v V) orm.Term[T, V] { return orm.TermOf[T, V](c.c.Else(orm.ExprOf(v))) }

// Switcher is a Switch before its first Case.
type Switcher[T, V any] struct{ e orm.Expr }

// Switch compares a field or term with values, Case by Case:
//
//	fn.Switch(Post.Status).Case("paid", 1).Case("refunded", -1).Else(0)
func Switch[T, V any](a orm.Arg[T, V]) Switcher[T, V] { return Switcher[T, V]{orm.ExprOf(a)} }

// Case is the first branch: then, of type R, where the switch equals v.
func (s Switcher[T, V]) Case[R any](v V, then R) SwitchTerm[T, V, R] {
	return SwitchTerm[T, V, R]{}.with(orm.Switch(s.e).Case(orm.ExprOf(v), orm.ExprOf(then)))
}

// SwitchTerm is a Switch: a term of its results' type R.
type SwitchTerm[T, V, R any] struct {
	orm.Term[T, R]
	s *orm.SwitchExpr
}

func (s SwitchTerm[T, V, R]) with(x *orm.SwitchExpr) SwitchTerm[T, V, R] {
	return SwitchTerm[T, V, R]{orm.TermOf[T, R](x), x}
}

// Case adds a branch; Else gives the result where no value equals.
func (s SwitchTerm[T, V, R]) Case(v V, then R) SwitchTerm[T, V, R] {
	return s.with(s.s.Case(orm.ExprOf(v), orm.ExprOf(then)))
}
func (s SwitchTerm[T, V, R]) Else(r R) orm.Term[T, R] {
	return orm.TermOf[T, R](s.s.Else(orm.ExprOf(r)))
}

// Subquery is a column of another model's query as a term of T: its
// first row's (order and limit the query).
//
//	latest := Post.Objects.Filter(fn.OuterEq(Post.Author, User.ID)).OrderBy(Post.Published.Desc()).Limit(1)
//	User.Objects.Annotate(fn.Subquery[User](latest, Post.Title).As("latest"))
func Subquery[T, R, V any](q orm.Query[R], col orm.Arg[R, V]) orm.Term[T, V] {
	return orm.TermOf[T, V](orm.Subquery(column(q, col)))
}

// In holds where a field of T is among a column of another model's query.
func In[T, R, V any](a orm.Arg[T, V], q orm.Query[R], col orm.Arg[R, V]) vuka.Pred[T] {
	return orm.P[T](orm.Q{orm.PathName(a) + "__in": column(q, col)})
}

func column[R, V any](q orm.Query[R], col orm.Arg[R, V]) orm.Values[R, any] {
	qs := q.QuerySet()
	n := orm.PathName(col)
	if _, ok := col.(orm.Expr); ok && n == "" {
		n = "_sub"
		qs = qs.Annotate(n, orm.ExprOf(col))
	}
	return qs.Values[any](n)
}

// Exists holds for the rows of T another model's query has a row for
// (with OuterEq, a related row).
func Exists[T, R any](q orm.Query[R]) vuka.Pred[T] { return orm.P[T](orm.Exists(q.QuerySet())) }

// OuterEq is the predicate of a subquery that its field equals a field of
// the query it stands in, Django's OuterRef:
// Post.Objects.Filter(fn.OuterEq(Post.Author, User.ID)).
func OuterEq[T, U, V, W any](inner orm.Arg[T, V], outer orm.Arg[U, W]) vuka.Pred[T] {
	return orm.P[T](orm.Q{orm.PathName(inner): orm.OuterRef(orm.PathName(outer))})
}

// OuterRef is a field of the query a subquery stands in, as an
// expression for templates and functions.
func OuterRef[U, W any](outer orm.Arg[U, W]) orm.Expr { return orm.OuterRef(orm.PathName(outer)) }

// FilteredRel is a relation whose rows are only those a predicate holds
// for, Django's FilteredRelation. Name it with As, Annotate it, then read
// through it with Field and Count.
type FilteredRel[T, R any] struct {
	rel  string
	cond orm.Cond
	name string
}

// FilteredRelation is rel (a relation of T to R) with the rows p holds for.
//
//	g := fn.FilteredRelation(Post.Tags, Tag.Name.StartsWith("g")).As("gtags")
//	Post.Objects.Annotate(g, g.Count().As("n"))
func FilteredRelation[T, R any](rel orm.Path[T], p vuka.Pred[R]) FilteredRel[T, R] {
	return FilteredRel[T, R]{rel: orm.FieldName(rel), cond: orm.CondOf(p)}
}

// As names the relation.
func (f FilteredRel[T, R]) As(name string) FilteredRel[T, R] { f.name = name; return f }

// Annotation makes it something Annotate takes.
func (f FilteredRel[T, R]) Annotation() orm.Term[T, R] {
	return orm.TermOf[T, R](orm.FilteredRelation(f.rel, f.cond)).As(f.name)
}

// Field is a field of the related rows, read through the relation.
func (f FilteredRel[T, R]) Field[V any](a orm.Arg[R, V]) orm.Term[T, V] {
	return orm.TermOf[T, V](orm.F(f.name + "__" + orm.PathName(a)))
}

// Count counts the related rows.
func (f FilteredRel[T, R]) Count() Agg[T, int64] { return aggOf[T, int64](orm.Count(f.name)) }
