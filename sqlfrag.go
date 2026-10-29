package orm

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// DialectName names a database's SQL: MySQL, Postgres, SQLite.
type DialectName string

const (
	MySQL    DialectName = "mysql"
	Postgres DialectName = "postgres"
	SQLite   DialectName = "sqlite"
)

// Dialects is a SQL fragment per database, for SQLFor.
type Dialects map[DialectName]string

// SQLExpr is a SQL fragment of T values, written in a query where the
// built-ins say nothing: SQLOf's or SQLFor's. It is an expression (Values,
// OrderBy, Update's Set, a comparison's value), compared by its own
// methods, and a SQLExpr[bool] is a condition (On, Filter, Exclude, Having).
type SQLExpr[T any] struct {
	per  Dialects // "" for every database
	args []any
}

// SQLOf is a SQL fragment of T values: ? binds the next argument as a
// parameter, {0}, {1}, … write the arguments they number as SQL — a typed
// field as its column (of the queried model or a model it joins), an
// aggregate or another expression as itself; '…' literals are left as
// written, and ?? is a literal ?. The template must be a constant: values
// only reach the database bound.
//
//	threeDaysAgo := orm.SQLOf[time.Time]("DATE_SUB(CURDATE(), INTERVAL ? DAY)", 3).For(orm.MySQL)
//	ProductColumns.UpdatedAt.Gte(threeDaysAgo)
//	orm.SQLOf[bool]("{0} >= DATE_SUB(CURDATE(), INTERVAL ? DAY)", ProductColumns.UpdatedAt, 3) // a condition
func SQLOf[T any](template string, args ...any) SQLExpr[T] {
	return SQLExpr[T]{per: Dialects{"": template}, args: args}
}

// SQLFor is SQLOf of a fragment written per database; a query on a
// database it has none for fails, naming it.
//
//	orm.SQLFor[time.Time](orm.Dialects{
//		orm.MySQL:    "DATE_SUB(CURDATE(), INTERVAL ? DAY)",
//		orm.Postgres: "CURRENT_DATE - ?::int",
//		orm.SQLite:   "date('now', '-' || ? || ' days')",
//	}, 3)
func SQLFor[T any](per Dialects, args ...any) SQLExpr[T] {
	return SQLExpr[T]{per: per, args: args}
}

// For marks the fragment as d's SQL: a query on another database fails,
// naming both, instead of with a syntax error.
func (e SQLExpr[T]) For(d DialectName) SQLExpr[T] {
	if t, ok := e.per[""]; ok {
		e.per = Dialects{d: t}
	}
	return e
}

func (e SQLExpr[T]) template(b *builder) (string, error) {
	if t, ok := e.per[DialectName(b.d.Name())]; ok {
		return t, nil
	}
	if t, ok := e.per[""]; ok {
		return t, nil
	}
	var has []string
	for d := range e.per {
		has = append(has, string(d))
	}
	slices.Sort(has)
	return "", fmt.Errorf("orm: a SQL fragment has no SQL for %s (it has %s)", b.d.Name(), strings.Join(has, ", "))
}

func (e SQLExpr[T]) exprSQL(b *builder) (string, error) {
	t, err := e.template(b)
	if err != nil {
		return "", err
	}
	return renderFragment(b, t, e.args)
}

func (e SQLExpr[T]) sql(b *builder) (string, error) {
	if reflect.TypeFor[T]().Kind() != reflect.Bool {
		return "", fmt.Errorf("orm: a SQLOf[%v] is no condition: a condition is a SQLOf[bool]", reflect.TypeFor[T]())
	}
	s, err := e.exprSQL(b)
	if err != nil || s == "" {
		return s, err
	}
	return "(" + s + ")", nil
}

func (SQLExpr[T]) column(T) {}

func (e SQLExpr[T]) isAggregate() bool {
	return slices.ContainsFunc(e.args, func(a any) bool { x, ok := a.(Expr); return ok && isAggregate(x) })
}

// Eq, Ne, Gt, Gte, Lt and Lte compare the fragment with v: a T, a typed
// field or another expression.
func (e SQLExpr[T]) Eq(v any) Cond  { return Where(e, "exact", v) }
func (e SQLExpr[T]) Ne(v any) Cond  { return Not(Where(e, "exact", v)) }
func (e SQLExpr[T]) Gt(v any) Cond  { return Where(e, "gt", v) }
func (e SQLExpr[T]) Gte(v any) Cond { return Where(e, "gte", v) }
func (e SQLExpr[T]) Lt(v any) Cond  { return Where(e, "lt", v) }
func (e SQLExpr[T]) Lte(v any) Cond { return Where(e, "lte", v) }

// Asc and Desc order by the fragment.
func (e SQLExpr[T]) Asc() ExprOrder  { return Asc(e) }
func (e SQLExpr[T]) Desc() ExprOrder { return ExprOrder{e, true} }

// renderFragment writes a SQLOf template: ? the next argument no {n} names,
// bound; {n} argument n as SQL.
func renderFragment(b *builder, tmpl string, args []any) (string, error) {
	named := map[int]bool{}
	var marks int
	if err := scanSQL(tmpl, func(n int) { named[n] = true }, func() { marks++ }); err != nil {
		return "", err
	}
	var free []int
	for i := range args {
		if !named[i] {
			free = append(free, i)
		}
	}
	for n := range named {
		if n >= len(args) {
			return "", fmt.Errorf("orm: SQL %q has no argument {%d}: it is given %d", tmpl, n, len(args))
		}
	}
	if marks != len(free) {
		return "", fmt.Errorf("orm: SQL %q has %d ? for %d arguments no {n} names", tmpl, marks, len(free))
	}
	var out strings.Builder
	next := 0
	var err error
	write := func(s string) { out.WriteString(s) }
	err = walkSQL(tmpl, write, func(n int) error {
		s, err := b.operand(args[n])
		write(s)
		return err
	}, func() error {
		v := args[free[next]]
		next++
		if e, ok := v.(Expr); ok {
			return fmt.Errorf("orm: SQL %q binds %T with ?: write an expression with {n}", tmpl, e)
		}
		write(b.arg(argValue(v)))
		return nil
	})
	return out.String(), err
}

// scanSQL reports a template's {n} and ? marks, or why it is malformed.
func scanSQL(tmpl string, ref func(int), mark func()) error {
	return walkSQL(tmpl, func(string) {}, func(n int) error { ref(n); return nil }, func() error { mark(); return nil })
}

// walkSQL walks a template: its text to text, each {n} to ref and each ?
// to mark; quoted literals and identifiers are text, ?? and {{ }} literal.
func walkSQL(tmpl string, text func(string), ref func(int) error, mark func() error) error {
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			end := strings.IndexByte(tmpl[i+1:], c)
			if end < 0 {
				return fmt.Errorf("orm: SQL %q: unclosed %c", tmpl, c)
			}
			text(tmpl[i : i+end+2])
			i += end + 1
		case c == '?' && i+1 < len(tmpl) && tmpl[i+1] == '?',
			c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{',
			c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}':
			text(string(c))
			i++
		case c == '?':
			if err := mark(); err != nil {
				return err
			}
		case c == '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return fmt.Errorf("orm: SQL %q: unclosed {", tmpl)
			}
			n, err := strconv.Atoi(tmpl[i+1 : i+end])
			if err != nil || n < 0 {
				return fmt.Errorf("orm: SQL %q: {%s} is no argument number", tmpl, tmpl[i+1:i+end])
			}
			if err := ref(n); err != nil {
				return err
			}
			i += end
		default:
			text(string(c))
		}
	}
	return nil
}
