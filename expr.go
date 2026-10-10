package orm

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Expr is a SQL expression a query computes: a field (F), a function call
// (a typed Func called, a Function's Of), a template (SQL), a Case or a Switch. Exprs go where
// values go — a Q's value, Where, Annotate, Update's Set, Aggregate — and
// nest.
type Expr interface {
	exprSQL(b *builder) (string, error)
}

// F is a field of the model, or an annotation, as an expression:
// Q{"age__gt": orm.F("min_age")} compares two columns, and
// Set{"views": orm.SQL("{0} + 1", orm.F("views"))} adds to one.
func F(name string) Expr { return fieldRef(name) }

type fieldRef string

func (f fieldRef) exprSQL(b *builder) (string, error) {
	col, err := b.ref(string(f))
	if err != nil {
		return "", err
	}
	return col, nil
}

// Value is v as an expression, Django's Value(): a constant column
// ('PLACEMENT' AS category) or a value passed where an expression goes
// (Case results, Coalesce) — sent as a bound argument, never pasted into
// the SQL.
//
//	Letters.Annotate("category", orm.Value("PLACEMENT"))
func Value(v any) Expr { return valueExpr{v} }

type valueExpr struct{ v any }

func (e valueExpr) exprSQL(b *builder) (string, error) { return b.arg(e.v), nil }

// SQL is an expression written in SQL: {0}, {1}, … stand for args, each
// an Expr or a value sent as an argument ({{ and }} are literal braces).
// It is the escape hatch for what a Func can't say; never build the
// template from user input.
//
//	orm.SQL("{0} + {1}", orm.F("views"), 1)
func SQL(template string, args ...any) Expr { return tmplExpr{template, args} }

type tmplExpr struct {
	tmpl string
	args []any
}

func (t tmplExpr) exprSQL(b *builder) (string, error) { return render(b, t.tmpl, t.args) }

// render fills a template's {n} (and {*}, every argument joined by commas)
// with its arguments.
func render(b *builder, tmpl string, args []any) (string, error) {
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{' {
			out.WriteByte('{')
			i++
			continue
		}
		if c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}' {
			out.WriteByte('}')
			i++
			continue
		}
		if c != '{' {
			out.WriteByte(c)
			continue
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("orm: unclosed { in %q", tmpl)
		}
		key := tmpl[i+1 : i+end]
		i += end
		if key == "*" {
			parts := make([]string, len(args))
			for j, a := range args {
				s, err := b.operand(a)
				if err != nil {
					return "", err
				}
				parts[j] = s
			}
			out.WriteString(strings.Join(parts, ", "))
			continue
		}
		n, err := strconv.Atoi(key)
		if err != nil || n < 0 || n >= len(args) {
			return "", fmt.Errorf("orm: %q has no argument {%s}", tmpl, key)
		}
		s, err := b.operand(args[n])
		if err != nil {
			return "", err
		}
		out.WriteString(s)
	}
	return out.String(), nil
}

// operand is v in SQL: an Expr rendered, anything else an argument.
func (b *builder) operand(v any) (string, error) {
	if e, ok := v.(Expr); ok {
		return e.exprSQL(b)
	}
	return b.arg(v), nil
}

// Where is a condition on an expression: Where(Lower.Of(F("name")),
// "exact", "ali"); any Q lookup.
func Where(e Expr, lookup string, value any) Cond { return exprCond{e, lookup, value} }

type exprCond struct {
	e      Expr
	lookup string
	value  any
}

func (c exprCond) sql(b *builder) (string, error) {
	if !isLookup(c.lookup) {
		return "", unknownLookup(c.lookup)
	}
	col, err := c.e.exprSQL(b)
	if err != nil {
		return "", err
	}
	return b.compare(col, c.lookup, c.value, c.lookup)
}

// Case is the first of its branches whose condition holds, Django's
// Case: each When's result, else Else's, else NULL.
//
//	orm.Case(
//		orm.When(orm.Q{"orders__total__gte": 1000}, "gold"),
//		orm.When(orm.Q{"age__gte": 18}, "silver"),
//	).Else("bronze")
func Case(whens ...WhenClause) *CaseExpr { return &CaseExpr{whens: whens} }

// When is a branch of a Case: then where cond, any Cond, holds. then is
// an Expr (a field, arithmetic, another Case) or a value sent as an
// argument.
func When(cond Cond, then any) WhenClause { return WhenClause{cond, then} }

// WhenClause is a branch of a Case.
type WhenClause struct {
	cond Cond
	then any
}

// CaseExpr is a Case.
type CaseExpr struct {
	whens []WhenClause
	els   any
}

// Else is the result when no branch's condition holds: an Expr or a
// value.
func (c *CaseExpr) Else(v any) *CaseExpr {
	n := *c
	n.els = v
	return &n
}

func (c *CaseExpr) exprSQL(b *builder) (string, error) {
	if len(c.whens) == 0 {
		return "", fmt.Errorf("orm: a Case with no When")
	}
	// A branch's condition is its own: an Exclude the CASE stands in
	// doesn't negate it.
	neg := b.neg
	b.neg = false
	defer func() { b.neg = neg }()
	s := "CASE"
	for _, w := range c.whens {
		cond, err := w.cond.sql(b)
		if err != nil {
			return "", err
		}
		then, err := b.result(w.then)
		if err != nil {
			return "", err
		}
		s += " WHEN " + cmp.Or(cond, "1 = 1") + " THEN " + then
	}
	return b.caseEnd(s, c.els)
}

// Switch is a Case comparing one expression with values: the result of
// the first Case value it equals, else Else's, else NULL.
//
//	orm.Switch(orm.F("status")).Case("paid", 1).Case("refunded", -1).Else(0)
func Switch(e Expr) *SwitchExpr { return &SwitchExpr{e: e} }

// SwitchExpr is a Switch.
type SwitchExpr struct {
	e     Expr
	cases [][2]any // value, result
	els   any
}

// Case adds a branch: then where the expression equals value. Each is an
// Expr or a value.
func (s *SwitchExpr) Case(value, then any) *SwitchExpr {
	n := *s
	n.cases = append(slices.Clip(s.cases), [2]any{value, then})
	return &n
}

// Else is the result when the expression equals no value.
func (s *SwitchExpr) Else(v any) *SwitchExpr {
	n := *s
	n.els = v
	return &n
}

func (s *SwitchExpr) exprSQL(b *builder) (string, error) {
	if len(s.cases) == 0 {
		return "", fmt.Errorf("orm: a Switch with no Case")
	}
	e, err := s.e.exprSQL(b)
	if err != nil {
		return "", err
	}
	out := "CASE " + e
	for _, c := range s.cases {
		v, err := b.operand(c[0])
		if err != nil {
			return "", err
		}
		then, err := b.result(c[1])
		if err != nil {
			return "", err
		}
		out += " WHEN " + v + " THEN " + then
	}
	return b.caseEnd(out, s.els)
}

// caseEnd closes a CASE: its ELSE, when there is one, and END.
func (b *builder) caseEnd(s string, els any) (string, error) {
	if els != nil {
		e, err := b.result(els)
		if err != nil {
			return "", err
		}
		s += " ELSE " + e
	}
	return s + " END", nil
}

// result is a CASE's result: NULL for nil. Postgres types an argument
// there as text when no branch says otherwise, so a value is cast to its
// Go type's (a driver.Valuer's own Value says it): numbers stay numbers
// to sum, compare and write.
func (b *builder) result(v any) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	if _, ok := v.(Expr); ok || b.d.Name() != "postgres" {
		return b.operand(v)
	}
	var t string
	switch rv := reflect.ValueOf(v); {
	case rv.Type() == timeType:
		t = "TIMESTAMP"
	case rv.Type().Implements(valuerType):
	case rv.Kind() == reflect.Bool:
		t = "BOOLEAN"
	case rv.CanInt() || rv.CanUint():
		t = "BIGINT"
	case rv.CanFloat():
		t = "DOUBLE PRECISION"
	case rv.Kind() == reflect.String:
		t = "TEXT"
	}
	if t == "" {
		return b.arg(v), nil
	}
	return "CAST(" + b.arg(v) + " AS " + t + ")", nil
}
