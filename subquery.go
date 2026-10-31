package orm

import (
	"fmt"
	"strconv"
)

// subquerier is a query that can stand inside another: a QuerySet (its
// primary keys) or Values (its columns).
type subquerier interface {
	subquerySQL(b *builder, exists bool) (string, error)
}

// Subquery is a query as a value of another: in a filter, an
// annotation or an update. A QuerySet gives its primary keys, Values its
// columns; OuterRef reads the query it stands in.
//
//	latest := Posts.Filter(orm.Q{"author_id": orm.OuterRef("id")}).OrderBy("-created_at").Limit(1)
//	Users.Annotate("latest_title", orm.Subquery(latest.Values[string]("title")))
//	Users.Filter(orm.Q{"id__in": Posts.Filter(orm.Q{"published": true}).Values[int64]("author_id")})
func Subquery(s subquerier) Expr { return subqueryExpr{s} }

type subqueryExpr struct{ s subquerier }

func (e subqueryExpr) exprSQL(b *builder) (string, error) {
	s, err := e.s.subquerySQL(b, false)
	if err != nil {
		return "", err
	}
	return "(" + s + ")", nil
}

// Exists holds when the query has a row; with OuterRef, a row related to
// the outer one.
//
//	Users.Filter(orm.Exists(Posts.Filter(orm.Q{"author_id": orm.OuterRef("id"), "published": true})))
func Exists(s subquerier) ExistsCond { return ExistsCond{s} }

// ExistsCond is Exists: a condition, and an Expr for Annotate.
type ExistsCond struct{ s subquerier }

func (e ExistsCond) sql(b *builder) (string, error) {
	s, err := e.s.subquerySQL(b, true)
	if err != nil {
		return "", err
	}
	return "EXISTS (" + s + ")", nil
}

func (e ExistsCond) exprSQL(b *builder) (string, error) { return e.sql(b) }

// OuterRef is a field of the query a Subquery or Exists stands in.
func OuterRef(name string) Expr { return outerRef(name) }

type outerRef string

func (o outerRef) exprSQL(b *builder) (string, error) {
	if b.outer == nil {
		return "", fmt.Errorf("orm: OuterRef(%q) outside a subquery", string(o))
	}
	return b.outer.ref(string(o))
}

// sub is the builder of a query standing inside b's: aliased apart (it
// may read the same table), its own joins, the statement's arguments.
func (q query) sub(b *builder) *builder {
	b.st.n++
	alias := "orm_" + strconv.Itoa(b.st.n)
	s := &builder{d: b.d, m: q.m, alias: alias, path: alias, st: b.st, joins: &[]joinClause{}, outer: b}
	s.scope = &fromScope{root: s, upto: -1, parent: b.scope}
	if len(q.ann) > 0 {
		s.ann = make(map[string]Expr, len(q.ann))
		for _, a := range q.ann {
			s.ann[a.name] = a.expr
		}
	}
	if err := q.addJoins(s); err != nil {
		s.scope.err = err
	}
	return s
}

func (qs QuerySet[T]) subquerySQL(b *builder, exists bool) (string, error) {
	q := *qs.q
	s := q.sub(b)
	cols := "1"
	if !exists {
		if q.m.PK == nil {
			return "", fmt.Errorf("orm: a query of %s stands for its primary key, and it has none: use Values", q.m.Name)
		}
		cols = s.col(q.m.PK)
	}
	return q.selectSQL(s, cols)
}

func (vq Values[T, R]) subquerySQL(b *builder, exists bool) (string, error) {
	q := *vq.q.q
	s := q.sub(b)
	if exists {
		return q.selectSQL(s, "1")
	}
	if len(vq.cols) != 1 {
		return "", fmt.Errorf("orm: a subquery reads one column; Values has %d", len(vq.cols))
	}
	s.many = true
	name, named := queryName(vq.cols[0])
	var col string
	var err error
	agg := false
	switch e, isExpr := vq.cols[0].(Expr); {
	case named:
		col, err = s.ref(name)
		agg = isAggregate(s.ann[name])
		q.values = []string{name}
	case isExpr:
		col, err = e.exprSQL(s)
		agg = isAggregate(e)
	default:
		err = fmt.Errorf("orm: a subquery's Values reads a %T: give a field or an expression", vq.cols[0])
	}
	if err != nil {
		return "", err
	}
	q.grouped = agg || q.groupBy != nil
	return q.selectSQL(s, col)
}
