package orm

import (
	"reflect"
	"strings"
)

// readAs has the annotation e, read as the reader's i-th name, come out
// the same on every database. Postgres and MySQL hand over a SUM (and
// Postgres an AVG) of integers as NUMERIC/DECIMAL text and a date as a
// time.Time, where SQLite gives an int64, a float64 and the date's text:
// a map, []any or any reads a COUNT as an int64, an AVG as a float64, a SUM,
// MIN or MAX of a number as its field's kind, EXISTS as a bool, and a
// date expression, into any type, as its text.
func (rd *reader) readAs(i int, m *model, e Expr) {
	if isDateExpr(e) {
		if rd.dates == nil {
			rd.dates = make([]bool, len(rd.names))
		}
		rd.dates[i] = true
		return
	}
	switch {
	case rd.shape == scalarShape && rd.out == anyType:
		rd.anyAs = aggregateType(m, e)
	case (rd.shape == mapShape || rd.shape == listShape) && rd.types[i] == nil:
		rd.types[i] = aggregateType(m, e)
	}
}

func isDateExpr(e Expr) bool {
	switch e := e.(type) {
	case DateExpr:
		switch e.kind {
		case "today", "date", "value":
			for _, s := range e.steps {
				if s.unit == "duration" {
					return false
				}
			}
			return true
		}
	case Alias:
		return isDateExpr(e.e)
	}
	return false
}

func aggregateType(m *model, e Expr) reflect.Type {
	fn, field := "", ""
	switch e := e.(type) {
	case relationAggregate:
		fn, field = e.fn, e.field
	case Agg:
		if e.expr != nil {
			return nil
		}
		fn, field = e.fn, e.field
	case interface{ unwrap() Expr }:
		if inner := e.unwrap(); inner != nil {
			return aggregateType(m, inner)
		}
		return nil
	default:
		return nil
	}
	switch fn {
	case "COUNT":
		return reflect.TypeFor[int64]()
	case "EXISTS":
		return reflect.TypeFor[bool]()
	case "AVG":
		return reflect.TypeFor[*float64]()
	case "SUM", "MIN", "MAX":
		return numberType(m, field)
	}
	return nil
}

// numberType is *int64, *uint64 or *float64 for the number field at path
// (through to-one or to-many relations), nil for any other field.
func numberType(m *model, path string) reflect.Type {
	parts := strings.Split(path, "__")
	for _, part := range parts[:len(parts)-1] {
		r, ok := m.relation(part)
		if !ok {
			return nil
		}
		t, _, err := r.ends()
		if err != nil {
			return nil
		}
		m = t
	}
	f, ok := m.field(parts[len(parts)-1])
	if !ok || f.codec != nil || f.Via != "" {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflect.TypeFor[*int64]()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflect.TypeFor[*uint64]()
	case reflect.Float32, reflect.Float64:
		return reflect.TypeFor[*float64]()
	}
	return nil
}
