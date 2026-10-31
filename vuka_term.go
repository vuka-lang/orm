package orm

import (
	"cmp"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/vuka-lang/vuka/v2"
)

// Term is a typed SQL expression over the model T, of Go type V: what the
// functions of package orm/fn build from field references
// (fn.Lower(Post.Title), fn.Count(Post.ID)). Its methods are predicates
// (vuka.Pred[T], so they go in Filter beside field references' own) and
// orderings; As names it, for Annotate and Values. It is an Expr, so the
// engine's string-free APIs take it too. OrderedTerm (numbers),
// StringTerm (strings) and CompareTerm (times) add what their type has,
// as vuka's references do.
type Term[T, V any] struct {
	e    Expr
	name string // the annotation it is, once As named it
}

// OrderedTerm is a Term of a number (or any cmp.Ordered type).
type OrderedTerm[T any, V cmp.Ordered] struct{ Term[T, V] }

// StringTerm is a Term of a string.
type StringTerm[T any, V ~string] struct{ OrderedTerm[T, V] }

// CompareTerm is a Term of a type ordering itself (time.Time).
type CompareTerm[T any, V interface{ Compare(V) int }] struct{ Term[T, V] }

// TermOf, OrderedTermOf, StringTermOf and CompareTermOf type an
// expression: the way to give a function of your own a result type.
func TermOf[T, V any](e any) Term[T, V] { return Term[T, V]{e: unterm(ExprOf(e))} }

// unterm is the engine expression under unnamed terms.
func unterm(e Expr) Expr {
	for {
		t, ok := e.(interface {
			inner() Expr
			annName() string
		})
		if !ok || t.annName() != "" {
			return e
		}
		e = t.inner()
	}
}

func (t Term[T, V]) inner() Expr { return t.e }

func OrderedTermOf[T any, V cmp.Ordered](e any) OrderedTerm[T, V] {
	return OrderedTerm[T, V]{TermOf[T, V](e)}
}

func StringTermOf[T any, V ~string](e any) StringTerm[T, V] {
	return StringTerm[T, V]{OrderedTermOf[T, V](e)}
}

func CompareTermOf[T any, V interface{ Compare(V) int }](e any) CompareTerm[T, V] {
	return CompareTerm[T, V]{TermOf[T, V](e)}
}

func (t Term[T, V]) exprSQL(b *builder) (string, error) {
	if t.name != "" {
		return b.ref(t.name)
	}
	if t.e == nil {
		return "", fmt.Errorf("orm: an empty %s term", reflect.TypeFor[V]())
	}
	return t.e.exprSQL(b)
}

// Expr is the term as the engine's expression.
func (t Term[T, V]) Expr() Expr { return t }

// Get makes a Term an Arg; an expression is computed by the database, so
// it panics.
func (t Term[T, V]) Get(T) V { panic("orm: a term is computed by the database, not read from a row") }

func (t Term[T, V]) annName() string { return t.name }

func (t Term[T, V]) annotation(*T) (string, Expr) { return t.name, t.e }

func (t Term[T, V]) as(name string) Term[T, V] { return Term[T, V]{e: t.e, name: name} }

// As names the term: Annotate(t) adds it to the query under that name,
// and the named term then reads it (in Filter, OrderBy, Values).
func (t Term[T, V]) As(name string) Term[T, V] { return t.as(name) }

func (t Term[T, V]) where(lookup string, v any) vuka.Pred[T] {
	return t.cmp(lookup, argValue(v))
}

// cmp is the term compared by lookup: an annotation by its name, as the
// engine's Q keys name it (so an aggregate's goes to HAVING).
func (t Term[T, V]) cmp(lookup string, v any) vuka.Pred[T] {
	if t.name != "" {
		return P[T](Q{t.name + "__" + lookup: v})
	}
	return P[T](Where(t, lookup, v))
}

func (t Term[T, V]) isAggregate() bool { return isAggregate(t.e) }

// Eq, Ne and In compare the term with values; EqF and NeF with another
// field or term (Django's F).
func (t Term[T, V]) Eq(v V) vuka.Pred[T] { return t.where("exact", v) }
func (t Term[T, V]) Ne(v V) vuka.Pred[T] { return vuka.Not(t.Eq(v)) }
func (t Term[T, V]) In(vs ...V) vuka.Pred[T] {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = argValue(v)
	}
	return t.cmp("in", out)
}
func (t Term[T, V]) EqF(o Arg[T, V]) vuka.Pred[T] { return t.cmp("exact", o) }
func (t Term[T, V]) NeF(o Arg[T, V]) vuka.Pred[T] { return vuka.Not(t.EqF(o)) }

// IsNull and NotNull test for NULL.
func (t Term[T, V]) IsNull() vuka.Pred[T]  { return t.cmp("isnull", true) }
func (t Term[T, V]) NotNull() vuka.Pred[T] { return t.cmp("isnull", false) }

// Asc and Desc order by the term.
func (t Term[T, V]) Asc() TermOrder[T]  { return TermOrder[T]{e: t, name: t.name} }
func (t Term[T, V]) Desc() TermOrder[T] { return TermOrder[T]{e: t, name: t.name, desc: true} }

func (t OrderedTerm[T, V]) As(name string) OrderedTerm[T, V] { return OrderedTerm[T, V]{t.as(name)} }

func (t OrderedTerm[T, V]) Lt(v V) vuka.Pred[T] { return t.where("lt", v) }
func (t OrderedTerm[T, V]) Le(v V) vuka.Pred[T] { return t.where("lte", v) }
func (t OrderedTerm[T, V]) Gt(v V) vuka.Pred[T] { return t.where("gt", v) }
func (t OrderedTerm[T, V]) Ge(v V) vuka.Pred[T] { return t.where("gte", v) }

// Range holds from lo to hi, both included.
func (t OrderedTerm[T, V]) Range(lo, hi V) vuka.Pred[T] {
	return t.cmp("range", []any{argValue(lo), argValue(hi)})
}

func (t OrderedTerm[T, V]) LtF(o Arg[T, V]) vuka.Pred[T] { return t.cmp("lt", o) }
func (t OrderedTerm[T, V]) LeF(o Arg[T, V]) vuka.Pred[T] { return t.cmp("lte", o) }
func (t OrderedTerm[T, V]) GtF(o Arg[T, V]) vuka.Pred[T] { return t.cmp("gt", o) }
func (t OrderedTerm[T, V]) GeF(o Arg[T, V]) vuka.Pred[T] { return t.cmp("gte", o) }

// Add, Sub, Mul and Div are arithmetic with a value, a field or a term.
func (t OrderedTerm[T, V]) Add(x any) OrderedTerm[T, V] { return t.op("+", x) }
func (t OrderedTerm[T, V]) Sub(x any) OrderedTerm[T, V] { return t.op("-", x) }
func (t OrderedTerm[T, V]) Mul(x any) OrderedTerm[T, V] { return t.op("*", x) }
func (t OrderedTerm[T, V]) Div(x any) OrderedTerm[T, V] { return t.op("/", x) }

func (t OrderedTerm[T, V]) op(o string, x any) OrderedTerm[T, V] {
	return OrderedTermOf[T, V](SQL("({0} "+o+" {1})", t, x))
}

func (t StringTerm[T, V]) As(name string) StringTerm[T, V] {
	return StringTerm[T, V]{OrderedTerm[T, V]{t.as(name)}}
}

func (t StringTerm[T, V]) Contains(s V) vuka.Pred[T]    { return t.where("contains", s) }
func (t StringTerm[T, V]) IContains(s V) vuka.Pred[T]   { return t.where("icontains", s) }
func (t StringTerm[T, V]) StartsWith(s V) vuka.Pred[T]  { return t.where("startswith", s) }
func (t StringTerm[T, V]) IStartsWith(s V) vuka.Pred[T] { return t.where("istartswith", s) }
func (t StringTerm[T, V]) EndsWith(s V) vuka.Pred[T]    { return t.where("endswith", s) }
func (t StringTerm[T, V]) IEndsWith(s V) vuka.Pred[T]   { return t.where("iendswith", s) }
func (t StringTerm[T, V]) IExact(s V) vuka.Pred[T]      { return t.where("iexact", s) }

func (t CompareTerm[T, V]) As(name string) CompareTerm[T, V] { return CompareTerm[T, V]{t.as(name)} }

func (t CompareTerm[T, V]) Lt(v V) vuka.Pred[T] { return t.where("lt", v) }
func (t CompareTerm[T, V]) Le(v V) vuka.Pred[T] { return t.where("lte", v) }
func (t CompareTerm[T, V]) Gt(v V) vuka.Pred[T] { return t.where("gt", v) }
func (t CompareTerm[T, V]) Ge(v V) vuka.Pred[T] { return t.where("gte", v) }

// TermOrder is an ordering by a term: OrderBy takes it beside field
// references' orderings.
type TermOrder[T any] struct {
	e    Expr
	name string
	desc bool
}

// Name is the ordering as the engine's OrderBy and Fuse name it: the
// term's annotation, - first when descending ("" for an unnamed term).
func (o TermOrder[T]) Name() string {
	if o.desc && o.name != "" {
		return "-" + o.name
	}
	return o.name
}

// Compare makes a TermOrder a Sort; the database orders by it, so it
// panics.
func (TermOrder[T]) Compare(a, b T) int { panic("orm: a term's order is the database's") }

// ModelAnnotation is a named term of T: what Query.Annotate adds.
type ModelAnnotation[T any] interface{ annotation(*T) (string, Expr) }

// opCond is the vuka.Pred op of a condition the ORM wrote itself.
const opCond vuka.Op = "orm"

// P is an ORM condition as a predicate on T, beside field references'
// own: Q, Or, Exists, Match — whatever has no Vuka form.
//
//	Post.Objects.Filter(Post.Views.Gt(10), orm.P[Post](orm.Q{"title__iregex": "^go"}))
func P[T any](c Cond) vuka.Pred[T] { return vuka.Pred[T]{Op: opCond, Value: c} }

// condOf is p as the ORM's condition.
func condOf[T any](p vuka.Pred[T]) Cond {
	// T only names the model a predicate is of: Pred[T] is the same memory
	// whatever T is, and this body shouldn't be compiled once per model.
	return predCond(*(*vuka.Pred[struct{}])(unsafe.Pointer(&p)))
}

func predCond(p vuka.Pred[struct{}]) Cond {
	switch p.Op {
	case opCond:
		if c, ok := p.Value.(Cond); ok {
			return c
		}
		return errCond{fmt.Errorf("orm: %T is no condition", p.Value)}
	case vuka.OpAnd, vuka.OpOr:
		cs := make([]Cond, len(p.Preds))
		for i, q := range p.Preds {
			cs[i] = predCond(q)
		}
		if p.Op == vuka.OpAnd {
			return And(cs...)
		}
		return Or(cs...)
	case vuka.OpNot:
		if len(p.Preds) != 1 {
			return errCond{fmt.Errorf("orm: a not predicate negates one predicate, not %d", len(p.Preds))}
		}
		return Not(predCond(p.Preds[0]))
	}
	key := lookupPath(p.Path)
	if key == "" {
		return errCond{fmt.Errorf("orm: a %q predicate names no field", p.Op)}
	}
	switch p.Op {
	case vuka.OpEq:
		return Q{key: argValue(p.Value)}
	case vuka.OpNe:
		return Not(Q{key: argValue(p.Value)})
	case vuka.OpLt:
		return Q{key + "__lt": argValue(p.Value)}
	case vuka.OpLe:
		return Q{key + "__lte": argValue(p.Value)}
	case vuka.OpGt:
		return Q{key + "__gt": argValue(p.Value)}
	case vuka.OpGe:
		return Q{key + "__gte": argValue(p.Value)}
	case vuka.OpContains, vuka.OpStartsWith, vuka.OpEndsWith:
		return Q{key + "__" + string(p.Op): p.Value}
	case vuka.OpIsNil:
		return Q{key + "__isnull": true}
	case vuka.OpNotNil:
		return Q{key + "__isnull": false}
	case vuka.OpIn:
		v := reflect.ValueOf(p.Value)
		vals := make([]any, v.Len())
		for i := range vals {
			vals[i] = argValue(v.Index(i).Interface())
		}
		return Q{key + "__in": vals}
	}
	return errCond{fmt.Errorf("orm: no predicate operator %q", p.Op)}
}

func condsOf[T any](ps []vuka.Pred[T]) []Cond {
	out := make([]Cond, len(ps))
	for i, p := range ps {
		out[i] = condOf(p)
	}
	return out
}

// CondOf is a predicate as the engine's condition.
func CondOf[T any](p vuka.Pred[T]) Cond { return condOf(p) }
