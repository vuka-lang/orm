package orm

import (
	"fmt"
	"strings"
)

// RelationPath is a model's fields rooted at a relation. Generated paths
// embed it, so conditions and aggregates use the same lookup prefix.
type RelationPath struct {
	path       string
	conditions []Cond
}

// RelationAt constructs a relation path for generated fields.
func RelationAt(path string) RelationPath { return RelationPath{path: path} }

// Where narrows the rows used by an aggregate, preserving the parent rows.
func (r RelationPath) Where(conditions ...Cond) RelationPath {
	r.conditions = append(append([]Cond(nil), r.conditions...), conditions...)
	return r
}

// Count counts distinct related primary keys.
func (r RelationPath) Count() Expr { return relationAggregate{r, "COUNT", "", false} }

// CountRows counts raw joined rows, including their duplicates.
func (r RelationPath) CountRows() Expr { return relationAggregate{r, "COUNT", "*", false} }

// Exists reports whether the relation has a matching row.
func (r RelationPath) Exists() Expr { return relationAggregate{r, "EXISTS", "", false} }

// Sum, Avg, Min and Max aggregate each related row once, even when a
// join chain reaches the same primary key more than once.
func (r RelationPath) Sum(field any) Expr { return r.aggregate("SUM", field) }
func (r RelationPath) Avg(field any) Expr { return r.aggregate("AVG", field) }
func (r RelationPath) Min(field any) Expr { return r.aggregate("MIN", field) }
func (r RelationPath) Max(field any) Expr { return r.aggregate("MAX", field) }
func (r RelationPath) aggregate(fn string, field any) Expr {
	name := ""
	switch f := field.(type) {
	case string:
		name = f
	case typedField:
		name = f.Name()
	}
	return relationAggregate{r, fn, name, true}
}

type relationAggregate struct {
	rel        RelationPath
	fn, field  string
	correlated bool
}

func (a relationAggregate) isAggregate() bool { return a.fn != "EXISTS" }

func (a relationAggregate) exprSQL(b *builder) (string, error) {
	if a.rel.path == "" {
		return "", fmt.Errorf("orm: an aggregate needs a to-many relation path")
	}
	for _, c := range a.rel.conditions {
		if err := relationCondition(c, a.rel.path); err != nil {
			return "", err
		}
	}
	// A correlated EXISTS uses an isolated copy of the root table. The
	// relation joins remain LEFT JOINs; the target key excludes empty rows.
	if a.fn == "EXISTS" || a.correlated {
		return a.subquery(b)
	}
	return a.joined(b)
}

func (a relationAggregate) target(b *builder) (*builder, error) {
	cur := b
	many := false
	for part := range strings.SplitSeq(a.rel.path, "__") {
		r, ok := cur.m.relation(part)
		if !ok {
			return nil, fmt.Errorf("orm: %s has no relation %q", cur.m.Name, part)
		}
		many = many || !r.one()
		var err error
		cur, err = cur.follow(r)
		if err != nil {
			return nil, err
		}
	}
	if !many {
		return nil, fmt.Errorf("orm: %s is a to-one path; aggregate a to-many relation instead", a.rel.path)
	}
	return cur, nil
}

func (a relationAggregate) joined(b *builder) (string, error) {
	oldMany, oldAgg := b.many, b.agg
	b.many, b.agg = true, true
	defer func() { b.many, b.agg = oldMany, oldAgg }()
	target, err := a.target(b)
	if err != nil {
		return "", err
	}
	col := target.col(target.m.PK)
	distinct := "DISTINCT "
	if a.field == "*" {
		col, distinct = "*", ""
	}
	cond, err := And(a.rel.conditions...).sql(b)
	if err != nil {
		return "", err
	}
	if cond == "" {
		return a.fn + "(" + distinct + col + ")", nil
	}
	if b.d.Name() == "postgres" {
		return a.fn + "(" + distinct + col + ") FILTER (WHERE " + cond + ")", nil
	}
	if col == "*" {
		col = "1"
	}
	return a.fn + "(" + distinct + "CASE WHEN " + cond + " THEN " + col + " END)", nil
}

func (a relationAggregate) subquery(b *builder) (string, error) {
	b.st.n++
	alias := fmt.Sprintf("orm_rel_%d", b.st.n)
	sub := newBuilder(b.d, b.m)
	sub.alias, sub.st = alias, b.st
	sub.many, sub.agg = true, true
	at := len(b.st.args)
	target, err := a.target(sub)
	if err != nil {
		return "", err
	}
	link := sub.col(sub.m.PK) + " = " + b.col(b.m.PK)
	cond, err := And(a.rel.conditions...).sql(sub)
	if err != nil {
		return "", err
	}
	if cond != "" {
		link += " AND (" + cond + ")"
	}
	if a.field != "*" {
		link += " AND " + target.col(target.m.PK) + " IS NOT NULL"
	}
	from, err := sub.from(at)
	if err != nil {
		return "", err
	}
	if a.fn == "EXISTS" {
		return "EXISTS (SELECT 1 FROM " + from + " WHERE " + link + ")", nil
	}
	col := target.col(target.m.PK)
	distinct := "DISTINCT "
	if a.field == "*" {
		col, distinct = "*", ""
	}
	if a.fn != "COUNT" {
		if !strings.HasPrefix(a.field, a.rel.path+"__") {
			return "", fmt.Errorf("orm: %s field %q must belong to relation %q", a.fn, a.field, a.rel.path)
		}
		value, err := sub.ref(a.field)
		if err != nil {
			return "", err
		}
		// Build FROM again: the value may follow another relation.
		from, err = sub.from(at)
		if err != nil {
			return "", err
		}
		q := b.d.Quote
		inner := "SELECT DISTINCT " + col + " AS " + q("orm_pk") + ", " + value + " AS " + q("orm_value") + " FROM " + from + " WHERE " + link
		value = q("orm_values") + "." + q("orm_value")
		if a.fn == "AVG" && b.d.Name() == "mysql" {
			value = "(" + value + " + 0E0)"
		}
		return "(SELECT " + a.fn + "(" + value + ") FROM (" + inner + ") AS " + q("orm_values") + ")", nil
	}
	return "(SELECT " + a.fn + "(" + distinct + col + ") FROM " + from + " WHERE " + link + ")", nil
}

func relationCondition(c Cond, path string) error {
	switch c := c.(type) {
	case Q:
		for key, value := range c {
			if !strings.HasPrefix(key, path+"__") {
				return fmt.Errorf("orm: relation %q Where condition %q belongs to another model; build it from this relation's fields", path, key)
			}
			if err := relationOperand(value, path); err != nil {
				return err
			}
		}
	case exprCond:
		if err := relationOperand(c.e, path); err != nil {
			return err
		}
		return relationOperand(c.value, path)
	case group:
		for _, child := range c.conds {
			if err := relationCondition(child, path); err != nil {
				return err
			}
		}
	case not:
		return relationCondition(c.c, path)
	default:
		return fmt.Errorf("orm: relation %q Where needs a condition on its own fields", path)
	}
	return nil
}

func relationOperand(v any, path string) error {
	check := func(name string) error {
		if !strings.HasPrefix(name, path+"__") {
			return fmt.Errorf("orm: relation %q Where field %q belongs to another model; build the condition from this relation's fields", path, name)
		}
		return nil
	}
	switch v := v.(type) {
	case fieldRef:
		return check(string(v))
	case typedField:
		return check(v.Name())
	case interface{ unwrap() Expr }:
		return relationOperand(v.unwrap(), path)
	case DateExpr:
		return relationOperand(v.base, path)
	case *call:
		for _, a := range v.args {
			if err := relationOperand(a, path); err != nil {
				return err
			}
		}
	case tmplExpr:
		for _, a := range v.args {
			if err := relationOperand(a, path); err != nil {
				return err
			}
		}
	}
	return nil
}
