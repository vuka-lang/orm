package orm

import (
	"fmt"
	"strconv"
	"time"
)

// DateExpr is a date or timestamp, with portable calendar arithmetic.
type DateExpr struct {
	OrderedExpr[string]
	base  any
	kind  string
	steps []dateStep
}
type dateStep struct {
	n    int64
	unit string
}

// Today is the database's current date.
func Today() DateExpr { return DateExpr{kind: "today"} }

// Now is the database's current timestamp.
func Now() DateExpr { return DateExpr{kind: "now"} }

// Date truncates a field or expression to its date.
type dateFunction func(...any) DateExpr

// Date retains its function and transform API and adds calendar arithmetic.
var Date dateFunction = func(args ...any) DateExpr {
	e := dateLegacy(args...)
	if len(args) != 1 {
		return DateExpr{OrderedExpr: e, base: dateLegacy.Of(args...), kind: "base"}
	}
	return DateExpr{OrderedExpr: e, base: args[0], kind: "date"}
}

func (f dateFunction) Of(args ...any) Expr     { return dateLegacy.Of(args...) }
func (f dateFunction) Transform() dateFunction { dateLegacy.Transform(); return f }

// TodayIn computes today's local wall date in Go. It binds that date as
// a value, so the database connection's timezone does not change it.
func TodayIn(loc *time.Location) DateExpr {
	if loc == nil {
		loc = time.UTC
	}
	return DateExpr{base: Clock().In(loc).Format("2006-01-02"), kind: "value"}
}
func (e DateExpr) step(n int64, unit string) DateExpr {
	e.steps = append(append([]dateStep(nil), e.steps...), dateStep{n, unit})
	return e
}
func (e DateExpr) AddDays(n int) DateExpr   { return e.step(int64(n), "day") }
func (e DateExpr) AddMonths(n int) DateExpr { return e.step(int64(n), "month") }

// Add adds a duration, preserving fractional seconds.
func (e DateExpr) Add(d time.Duration) DateExpr { return e.step(int64(d), "duration") }
func (e DateExpr) exprSQL(b *builder) (string, error) {
	var s string
	var err error
	switch e.kind {
	case "today":
		switch b.d.Name() {
		case "mysql":
			s = "CURDATE()"
		case "postgres":
			s = "CURRENT_DATE"
		default:
			s = "date('now')"
		}
	case "now":
		switch b.d.Name() {
		case "mysql":
			s = "NOW()"
		case "postgres":
			s = "now()"
		default:
			s = "datetime('now')"
		}
	case "value":
		s = b.arg(e.base)
		if b.d.Name() == "postgres" {
			s = "CAST(" + s + " AS DATE)"
		} else {
			s = "DATE(" + s + ")"
		}
	default:
		s, err = b.operand(e.base)
		if err != nil {
			return "", err
		}
		if e.kind == "date" {
			if b.d.Name() == "postgres" {
				s = "(" + s + ")::date"
			} else {
				s = "DATE(" + s + ")"
			}
		}
	}
	isDate := e.kind == "today" || e.kind == "date" || e.kind == "value"
	for _, step := range e.steps {
		n := float64(step.n)
		unit := step.unit
		if unit == "duration" {
			n /= float64(time.Second)
			unit = "second"
		}
		amount := strconv.FormatFloat(n, 'f', -1, 64)
		switch b.d.Name() {
		case "mysql":
			fn := "DATE_ADD"
			if n < 0 {
				fn = "DATE_SUB"
				amount = strconv.FormatFloat(-n, 'f', -1, 64)
			}
			s = fn + "(" + s + ", INTERVAL " + amount + " " + map[string]string{"day": "DAY", "month": "MONTH", "second": "SECOND"}[unit] + ")"
		case "postgres":
			if unit == "day" && isDate {
				s = "(" + s + " + " + amount + ")"
			} else {
				s = "(" + s + " + INTERVAL '" + amount + " " + unit + "')"
				isDate = false
			}
		default:
			if n >= 0 {
				amount = "+" + amount
			}
			fn := "datetime"
			if unit != "second" && (e.kind == "today" || e.kind == "date" || e.kind == "value") {
				fn = "date"
			}
			s = fn + "(" + s + ", '" + amount + " " + unit + "')"
		}
	}
	return s, nil
}

// TimeField is a timestamp column. Comparisons accept time.Time values
// and date expressions; generated paths keep the field's lookup prefix.
type TimeField struct{ Field[time.Time] }

func TimeFieldAt(path string) TimeField          { return TimeField{FieldAt[time.Time](path)} }
func TimeFieldIn(src *Source, path string) TimeField {
	return TimeField{FieldIn[time.Time](src, path)}
}
func (f TimeField) Eq(v any) Cond                { return Q{f.key(): v} }
func (f TimeField) Ne(v any) Cond                { return Not(f.Eq(v)) }
func (f TimeField) Gt(v any) Cond                { return Q{f.key() + "__gt": v} }
func (f TimeField) Gte(v any) Cond               { return Q{f.key() + "__gte": v} }
func (f TimeField) Lt(v any) Cond                { return Q{f.key() + "__lt": v} }
func (f TimeField) Lte(v any) Cond               { return Q{f.key() + "__lte": v} }
func (f TimeField) AddDays(n int) DateExpr       { return DateExpr{base: f}.AddDays(n) }
func (f TimeField) AddMonths(n int) DateExpr     { return DateExpr{base: f}.AddMonths(n) }
func (f TimeField) Add(d time.Duration) DateExpr { return DateExpr{base: f}.Add(d) }
func (f TimeField) Between(a, b any) Cond        { return Where(f.Expr(), "range", []any{a, b}) }

// Between includes both endpoints, as Range does.
func (f Field[V]) Between(a, b V) Cond { return f.Range(a, b) }

// String describes a date expression for diagnostic output.
func (e DateExpr) String() string { return fmt.Sprintf("date expression (%s)", e.kind) }

func (f dateFunction) Def() *FuncDef     { return dateLegacy.Def() }
func (f dateFunction) Name() string      { return dateLegacy.Name() }
func (e DateExpr) Eq(v any) Cond         { return Where(e, "exact", v) }
func (e DateExpr) Ne(v any) Cond         { return Not(e.Eq(v)) }
func (e DateExpr) Gt(v any) Cond         { return Where(e, "gt", v) }
func (e DateExpr) Gte(v any) Cond        { return Where(e, "gte", v) }
func (e DateExpr) Ge(v any) Cond         { return e.Gte(v) }
func (e DateExpr) Lt(v any) Cond         { return Where(e, "lt", v) }
func (e DateExpr) Lte(v any) Cond        { return Where(e, "lte", v) }
func (e DateExpr) Le(v any) Cond         { return e.Lte(v) }
func (e DateExpr) Between(a, b any) Cond { return Where(e, "range", []any{a, b}) }

func (e DateExpr) Asc() ExprOrder  { return Asc(e) }
func (e DateExpr) Desc() ExprOrder { return Desc(e) }
