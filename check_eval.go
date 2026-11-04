package orm

import (
	"cmp"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
)

// truth is a condition's value as SQL has it: false, unknown (NULL) or
// true. A CHECK refuses a row only where its condition is false.
type truth int8

const (
	no truth = iota
	unknown
	yes
)

func truthOf(b bool) truth {
	if b {
		return yes
	}
	return no
}

func (t truth) and(o truth) truth { return min(t, o) }
func (t truth) or(o truth) truth  { return max(t, o) }
func (t truth) not() truth        { return yes - t }

// goLookup is a lookup's meaning in Go: col, the row's value, against
// arg, both as scalar has them (nil for NULL; arg a list for in and
// range).
type goLookup func(t textRules, col, arg any) truth

func goExact(t textRules, col, arg any) truth {
	if arg == nil {
		return truthOf(col == nil)
	}
	return t.compared(col, arg, false, func(c int) bool { return c == 0 })
}

func goIExact(t textRules, col, arg any) truth {
	c, a, ok := texts(col, arg)
	if !ok {
		return unknown
	}
	return truthOf(t.lower(c) == t.lower(a))
}

// goLike is a LIKE lookup: match of the column's text and the value's,
// both folded as the database's LIKE folds them, or as LOWER does.
func goLike(match func(s, part string) bool, ignoreCase bool) goLookup {
	return func(t textRules, col, arg any) truth {
		c, a, ok := texts(col, arg)
		if !ok {
			return unknown
		}
		fold := t.likeFold
		if ignoreCase {
			fold = t.lower
		}
		return truthOf(match(fold(c), fold(a)))
	}
}

func goIn(t textRules, col, arg any) truth {
	out := no
	for _, x := range arg.([]any) {
		out = out.or(t.compared(col, x, false, func(c int) bool { return c == 0 }))
	}
	return out
}

func goOrder(holds func(int) bool) goLookup {
	return func(t textRules, col, arg any) truth { return t.compared(col, arg, true, holds) }
}

func goRange(t textRules, col, arg any) truth {
	bounds := arg.([]any)
	if len(bounds) != 2 {
		return unknown
	}
	return t.compared(col, bounds[0], true, func(c int) bool { return c >= 0 }).
		and(t.compared(col, bounds[1], true, func(c int) bool { return c <= 0 }))
}

func goIsNull(_ textRules, col, arg any) truth {
	isNull, _ := arg.(bool)
	return truthOf((col == nil) == isNull)
}

// texts is col and arg as text: false when either is NULL or not text.
func texts(col, arg any) (string, string, bool) {
	c, ok := col.(string)
	a, ok2 := arg.(string)
	return c, a, ok && ok2
}

// textRules is how a database compares text, as far as Go follows it:
// the case its LIKE ignores, what its LOWER folds, and whether it orders
// text as Go does, byte by byte.
type textRules struct {
	likeFold, lower func(string) string
	ordered         bool
}

// textRulesOf is d's: Postgres's LIKE heeds case and its order is its
// collation's; MySQL's default collations ignore case, and order their
// way; SQLite's LIKE and LOWER fold ASCII alone, and it orders by bytes.
func textRulesOf(d Dialect) textRules {
	switch d.Name() {
	case "postgres":
		return textRules{likeFold: func(s string) string { return s }, lower: strings.ToLower}
	case "mysql":
		return textRules{likeFold: strings.ToLower, lower: strings.ToLower}
	}
	return textRules{likeFold: asciiLower, lower: asciiLower, ordered: true}
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// compared is whether holds of col compared with arg (negative, zero or
// positive): unknown where either is NULL, or Go can't compare them as
// the database does (ordered: by their order, not only equality).
func (t textRules) compared(col, arg any, ordered bool, holds func(int) bool) truth {
	c, ok := t.compare(col, arg, ordered)
	if !ok {
		return unknown
	}
	return truthOf(holds(c))
}

func (t textRules) compare(a, b any, ordered bool) (int, bool) {
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return cmp.Compare(x, y), true
		case float64:
			return cmp.Compare(float64(x), y), true
		}
	case float64:
		switch y := b.(type) {
		case int64:
			return cmp.Compare(x, float64(y)), true
		case float64:
			return cmp.Compare(x, y), true
		}
	case string:
		if y, ok := b.(string); ok && (!ordered || t.ordered) {
			return strings.Compare(x, y), true
		}
	case bool:
		if y, ok := b.(bool); ok && !ordered {
			if x == y {
				return 0, true
			}
			return 1, true
		}
	case time.Time:
		if y, ok := b.(time.Time); ok {
			return x.Compare(y), true
		}
	}
	return 0, false
}

// scalar is v as Go compares it with a column's value: nil (NULL), a
// bool, an int64, a float64, a string or a time; false for anything else.
func scalar(v any) (any, bool) {
	if dv, ok := v.(driver.Valuer); ok {
		var err error
		if v, err = dv.Value(); err != nil {
			return nil, false
		}
	}
	switch x := v.(type) {
	case nil, time.Time:
		return x, true
	case []byte:
		return string(x), true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u), true
		}
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	case reflect.String:
		return rv.String(), true
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, true
		}
		return scalar(rv.Elem().Interface())
	}
	return nil, false
}

// rowTest is a check's condition as Go tests a row about to be written.
type rowTest interface {
	test(r *rowEval) truth
}

// rowEval is a row being tested: the model's, written on d, new or saved.
type rowEval struct {
	m      *model
	d      Dialect
	text   textRules
	row    reflect.Value
	create bool
}

// column is f's value in the row as its column will hold it; false where
// Go can't tell: a generated column, or one the write fills itself.
func (r *rowEval) column(f *field) (any, bool) {
	v := peek(r.row, f.Index, f.Type)
	if f.Gen != nil || r.filled(f, v) {
		return nil, false
	}
	x, err := r.m.out(r.d, f, f.normValue(value(v)))
	if err != nil {
		return nil, false
	}
	return scalar(x)
}

// filled is whether the write gives f its value: an auto_now field, and
// on a create a zero one with a value of its own to take (a counted key,
// auto_now_add, an Initial or a new UUID).
func (r *rowEval) filled(f *field, v reflect.Value) bool {
	if f.AutoNow {
		return true
	}
	if !r.create || !v.IsZero() {
		return false
	}
	return f == r.m.PK && r.m.counted() || f.AutoNowAdd || f.spec != nil && (f.spec.initial != nil || f.spec.uuid)
}

type allOf []rowTest

func (a allOf) test(r *rowEval) truth {
	out := yes
	for _, t := range a {
		out = out.and(t.test(r))
	}
	return out
}

type anyOf []rowTest

func (a anyOf) test(r *rowEval) truth {
	out := no
	for _, t := range a {
		out = out.or(t.test(r))
	}
	return out
}

type notTest struct{ t rowTest }

func (n notTest) test(r *rowEval) truth { return n.t.test(r).not() }

// dbTest is a part of a condition only the database can answer (a
// function, a transform, a lookup of its own): Go leaves it unknown, and
// the database's CHECK decides.
type dbTest struct{}

func (dbTest) test(*rowEval) truth { return unknown }

// lookupTest is a lookup of one of the row's columns.
type lookupTest struct {
	f      *field
	lookup string
	eval   goLookup
	arg    rowArg
	// nullSafe: under a Not, a NULL column fails the lookup, as nullSafe
	// writes it.
	nullSafe bool
}

func (t lookupTest) test(r *rowEval) truth {
	col, ok := r.column(t.f)
	if !ok {
		return unknown
	}
	if t.nullSafe && col == nil {
		return no
	}
	arg, ok := t.arg.value(r, t.f, t.lookup)
	if !ok {
		return unknown
	}
	return t.eval(r.text, col, arg)
}

// rowArg is a lookup's value: a value, a column of the row (F), or what
// only the database can work out.
type rowArg struct {
	v  any
	f  *field
	db bool
}

// value is the arg as the lookup of the column of of compares it; false
// where Go can't tell.
func (a rowArg) value(r *rowEval, of *field, lookup string) (any, bool) {
	switch {
	case a.db:
		return nil, false
	case a.f != nil:
		return r.column(a.f)
	}
	v, err := r.m.outCond(r.d, of, lookup, a.v)
	if err != nil {
		return nil, false
	}
	if lookup != "in" && lookup != "range" {
		return scalar(v)
	}
	items, err := list(v)
	if err != nil {
		return nil, false
	}
	for i, x := range items {
		var ok bool
		if items[i], ok = scalar(x); !ok {
			return nil, false
		}
	}
	return items, true
}

// rowTestOf is cond as Go tests a row: nil for an empty condition (Q{},
// And()), which SQL leaves out. Under neg, an odd number of Nots, a
// lookup is null-safe as nullSafe writes it.
func (m *model) rowTestOf(cond Cond, neg bool) (rowTest, error) {
	switch c := cond.(type) {
	case Q:
		var all allOf
		for _, key := range slices.Sorted(maps.Keys(c)) {
			t, err := m.lookupTestOf(key, c[key], neg)
			if err != nil {
				return nil, err
			}
			all = append(all, t)
		}
		return joined(all, false), nil
	case group:
		var parts []rowTest
		for _, sub := range c.conds {
			t, err := m.rowTestOf(sub, neg)
			if err != nil {
				return nil, err
			}
			if t != nil {
				parts = append(parts, t)
			}
		}
		return joined(parts, c.op == "OR"), nil
	case not:
		t, err := m.rowTestOf(c.c, !neg)
		if t == nil || err != nil {
			return nil, err
		}
		return notTest{t}, nil
	case Pred:
		pc, err := c.Cond()
		if err != nil {
			return nil, err
		}
		return m.rowTestOf(pc, neg)
	case exprCond:
		// Where writes its comparison as it is, never null-safe.
		if f, ok := c.e.(fieldRef); ok {
			return m.lookupTestOf(string(f)+"__"+c.lookup, c.value, false)
		}
	case ExistsCond:
		return nil, errors.New("it asks Exists of a query: a check reads the row's own columns")
	}
	return dbTest{}, nil
}

// joined is parts joined by AND (or OR): nil for none, the one alone.
func joined(parts []rowTest, or bool) rowTest {
	switch {
	case len(parts) == 0:
		return nil
	case len(parts) == 1:
		return parts[0]
	case or:
		return anyOf(parts)
	}
	return allOf(parts)
}

// lookupTestOf is a Q entry as Go tests a row: its field, which must be
// one of the row's own columns, its lookup and value.
func (m *model) lookupTestOf(key string, v any, neg bool) (rowTest, error) {
	parts := strings.Split(key, "__")
	lookup := "exact"
	if len(parts) > 1 && isLookup(parts[len(parts)-1]) {
		lookup, parts = parts[len(parts)-1], parts[:len(parts)-1]
	}
	f, transformed, err := m.ownField(parts)
	if err != nil {
		return nil, fmt.Errorf("%q %w", key, err)
	}
	arg, err := m.rowArgOf(f, lookup, v, !transformed)
	if err != nil {
		return nil, fmt.Errorf("%q %w", key, err)
	}
	def := lookups[lookup]
	if transformed || def.eval == nil {
		return dbTest{}, nil
	}
	nullSafe := neg && lookup != "isnull" && v != nil && f.Type.Kind() == reflect.Pointer
	return lookupTest{f: f, lookup: lookup, eval: def.eval, arg: arg, nullSafe: nullSafe}, nil
}

// ownField is the column of the row a lookup's name reads (a field, or a
// foreign key's own column), and whether transforms follow it.
func (m *model) ownField(parts []string) (*field, bool, error) {
	if f, ok := m.field(parts[0]); ok && f.Via == "" && (!f.fk || len(parts) == 1) {
		return f, len(parts) > 1, nil
	}
	r, ok := m.relation(parts[0])
	if !ok {
		return nil, false, fmt.Errorf("names no field of %s", m.Name)
	}
	if r.Kind == relFK && (len(parts) == 1 || r.keyStep(parts[1:])) {
		return r.local, false, nil
	}
	return nil, false, fmt.Errorf("reads %s.%s, another table's rows: a check reads the row's own columns", m.Name, r.Name)
}

// rowArgOf is v as a lookup of f has it: a column of the row (F, a typed
// field), an expression the database works out, or a value, which must
// suit f's column when typed.
func (m *model) rowArgOf(f *field, lookup string, v any, typed bool) (rowArg, error) {
	switch x := v.(type) {
	case subquerier, subqueryExpr, ExistsCond:
		return rowArg{}, errors.New("compares with a query: a check reads the row's own columns")
	case fieldRef:
		return m.fieldArg(string(x))
	case typedField:
		return m.fieldArg(x.Name())
	case pather:
		return m.fieldArg(lookupPath(x.FieldPath()))
	case Expr:
		return rowArg{db: true}, nil
	}
	v = argValue(v)
	if typed {
		if err := suits(f, lookup, v); err != nil {
			return rowArg{}, err
		}
	}
	return rowArg{v: v}, nil
}

// fieldArg is a column of the row as a lookup's value.
func (m *model) fieldArg(name string) (rowArg, error) {
	f, transformed, err := m.ownField(strings.Split(name, "__"))
	if err != nil {
		return rowArg{}, fmt.Errorf("compares with %s, which %w", name, err)
	}
	return rowArg{f: f, db: transformed}, nil
}

// suits fails when v can't be what lookup compares f's column with: a
// number column with text (posts__gt "x"), isnull with anything but a
// bool, a LIKE with anything but text.
func suits(f *field, lookup string, v any) error {
	want := ""
	if f.conv == 0 {
		want = classOf(valueType(f.Type))
	}
	values := []any{v}
	switch lookup {
	case "isnull":
		want = "a bool"
	case "iexact", "contains", "icontains", "startswith", "istartswith", "endswith", "iendswith":
		want = "text"
	case "in", "range":
		items, err := list(v)
		if err != nil {
			return fmt.Errorf("takes a list of values: %w", err)
		}
		values = items
	}
	for _, x := range values {
		if x == nil || want == "" {
			continue
		}
		if _, isExpr := x.(Expr); isExpr {
			continue
		}
		if got := classOf(reflect.TypeOf(argValue(x))); got != "" && got != want {
			return fmt.Errorf("compares %s with %#v, %s: it takes %s", f.Column, x, got, want)
		}
	}
	return nil
}

// classOf is what a Go type holds as a column compares it: a number,
// text, a bool, a time; "" for another kind of value.
func classOf(t reflect.Type) string {
	switch {
	case t == timeType:
		return "a time"
	case t.Kind() == reflect.Bool:
		return "a bool"
	case t.Kind() == reflect.String:
		return "text"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Float64:
		return "a number"
	}
	return ""
}
