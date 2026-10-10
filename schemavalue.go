package orm

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
)

// SchemaValuer is a column's Go type whose database value depends on the
// schema it is written under: a status kept as a code on a legacy table
// and as its name on the new one. The ORM calls ValueFor with the names
// set of the query ("" the default) for every value it sends of the type
// — conditions, Update and Save, Create — so one query serves both.
// Reading is the type's own Scan, which takes either form.
//
//	func (s LetterStatus) ValueFor(names string) (driver.Value, error) {
//		if names == "legacy" {
//			return int64(codeOf[s]), nil
//		}
//		return string(s), nil
//	}
//
// A value of another type sent to such a column (an int where the field
// is a LetterStatus) is refused: it would be right on one schema only.
type SchemaValuer interface {
	ValueFor(names string) (driver.Value, error)
}

// SchemaScanner is SchemaValuer's reading side: ScanFor is given the
// names set the value was read under ("" the default), so a type never
// guesses which form it got. The ORM calls it for model rows, Values
// (into the type, a map or a list) and Raw on a schema; a type without it
// reads through its Scan.
//
//	func (s *LetterStatus) ScanFor(names string, src any) error {
//		if names == "legacy" {
//			*s = LetterStatus(EmployerLetterStatusFromCode[int(src.(int64))])
//			return nil
//		}
//		*s = LetterStatus(fmt.Sprint(src))
//		return nil
//	}
type SchemaScanner interface {
	ScanFor(names string, src any) error
}

var (
	schemaValuerType  = reflect.TypeFor[SchemaValuer]()
	schemaScannerType = reflect.TypeFor[SchemaScanner]()
)

// perSchema is t (or what it points to) when its values differ per
// schema: a SchemaValuer or a SchemaScanner. Else nil.
func perSchema(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	p := reflect.PointerTo(t)
	if t.Implements(schemaValuerType) || p.Implements(schemaValuerType) || p.Implements(schemaScannerType) {
		return t
	}
	return nil
}

// scanFor sets dst from src through its type's ScanFor under names: NULL
// is nil for a pointer, ScanFor(nil) else.
func scanFor(dst reflect.Value, names string, src any) error {
	if dst.Kind() == reflect.Pointer {
		if src == nil {
			dst.SetZero()
			return nil
		}
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		dst = dst.Elem()
	}
	return dst.Addr().Interface().(SchemaScanner).ScanFor(names, src)
}

// schemaValued is the type f holds when it is a SchemaValuer, else nil.
func schemaValued(f *field) reflect.Type {
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(schemaValuerType) || reflect.PointerTo(t).Implements(schemaValuerType) {
		return t
	}
	return nil
}

// out is v as f's column takes it on m's names set: a SchemaValuer's
// value there. Expressions, subqueries and nil pass as they are.
func (m *model) out(f *field, v any) (any, error) {
	t := schemaValued(f)
	if t == nil || v == nil {
		return v, nil
	}
	switch v.(type) {
	case Expr, subquerier:
		return v, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	if rv.Type() != t {
		return nil, fmt.Errorf("orm: %s.%s is %s, whose value differs per schema: pass %s, not %T (%v)", m.Name, f.Name, t, t, v, v)
	}
	p := reflect.New(t)
	p.Elem().Set(rv)
	dv, err := p.Interface().(SchemaValuer).ValueFor(m.Names)
	if err != nil {
		return nil, fmt.Errorf("orm: %s.%s: %w", m.Name, f.Name, err)
	}
	return dv, nil
}

// outCond is a condition's value as f's column takes it: each item of a
// list (__in, __range), for the lookups that compare the column itself.
func (m *model) outCond(f *field, lookup string, v any) (any, error) {
	if schemaValued(f) == nil {
		return v, nil
	}
	switch lookup {
	case "exact", "gt", "gte", "lt", "lte":
		return m.out(f, v)
	case "in", "range":
		if _, ok := v.(subquerier); ok {
			return v, nil
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return m.out(f, v)
		}
		items := make([]any, rv.Len())
		for i := range items {
			x, err := m.out(f, rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			items[i] = x
		}
		return items, nil
	}
	return v, nil
}

// perSchema is whether a field of m reads per schema: its rows are read
// by the ORM, not a generated scanner, which knows no names set.
func (m *model) perSchema() bool {
	return slices.ContainsFunc(m.Fields, func(f *field) bool { return f.fast == scanSchema })
}

// readsAs refuses reading f, a field whose values differ per schema, into
// a type other than its own (or a pointer to it): a string would read
// "6" on one schema and "WAITING" on the other.
func readsAs(name string, f *field, into reflect.Type) error {
	t := perSchema(f.Type)
	if t == nil {
		return nil
	}
	base := into
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base == t {
		return nil
	}
	return fmt.Errorf("orm: Values reads %q into %s, but the field is %s, whose value differs per schema: read it into %s", name, into, t, t)
}
