package orm

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// fieldOf is the field at index in v, allocating the nil pointers it
// passes through (an embedded *Base) so it can be written.
func fieldOf(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

// peek is the field at index in v without allocating: the zero value when
// a pointer on the way is nil.
func peek(v reflect.Value, index []int, t reflect.Type) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Zero(t)
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

// cell receives one column of a row whatever the driver hands it, NULL
// included, and converts it into the field.
type cell struct {
	dst   reflect.Value
	fast  scanKind // dst's kind when the driver's common values set it directly
	names string   // the names set read under, for a SchemaScanner
}

func (c *cell) Scan(src any) error {
	if c.fast == scanSchema {
		return scanFor(c.dst, c.names, src)
	}
	// What assign does for the driver's usual values, without its checks:
	// fast is only set for a type that is no Scanner nor pointer.
	switch c.fast {
	case scanString:
		switch s := src.(type) {
		case string:
			c.dst.SetString(s)
			return nil
		case []byte:
			c.dst.SetString(string(s))
			return nil
		}
	case scanInt:
		if n, ok := src.(int64); ok {
			c.dst.SetInt(n)
			return nil
		}
	case scanBool:
		switch s := src.(type) {
		case bool:
			c.dst.SetBool(s)
			return nil
		case int64:
			c.dst.SetBool(s != 0)
			return nil
		}
	case scanFloat:
		switch s := src.(type) {
		case float64:
			c.dst.SetFloat(s)
			return nil
		case int64:
			c.dst.SetFloat(float64(s))
			return nil
		}
	case scanTime:
		if t, ok := src.(time.Time); ok {
			*(*time.Time)(c.dst.Addr().UnsafePointer()) = t
			return nil
		}
	}
	return assign(c.dst, src)
}

// scanKind is how a cell of a type may be set without assign's checks.
type scanKind uint8

const (
	scanSlow scanKind = iota
	scanString
	scanInt
	scanBool
	scanFloat
	scanTime
	scanSchema // a SchemaScanner: its ScanFor with the names set
)

func scanKindOf(t reflect.Type) scanKind {
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if reflect.PointerTo(base).Implements(schemaScannerType) {
		return scanSchema
	}
	if reflect.PointerTo(t).Implements(scannerType) {
		return scanSlow
	}
	if t == timeType {
		return scanTime
	}
	switch t.Kind() {
	case reflect.String:
		return scanString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return scanInt
	case reflect.Bool:
		return scanBool
	case reflect.Float32, reflect.Float64:
		return scanFloat
	}
	return scanSlow
}

// assign sets dst from a database value: a NULL zeroes it, a Scanner
// scans itself, and the driver's int64, float64, bool, []byte, string
// and time.Time convert to dst's kind.
func assign(dst reflect.Value, src any) error {
	if dst.Kind() != reflect.Pointer && dst.CanAddr() {
		if s, ok := dst.Addr().Interface().(sql.Scanner); ok {
			return s.Scan(src)
		}
	}
	if src == nil {
		dst.SetZero()
		return nil
	}
	if dst.Kind() == reflect.Pointer {
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		return assign(dst.Elem(), src)
	}
	if dst.Type() == timeType {
		t, err := asTime(src)
		if err != nil {
			return err
		}
		if dst.CanAddr() {
			*dst.Addr().Interface().(*time.Time) = t // no boxing of t
		} else {
			dst.Set(reflect.ValueOf(t))
		}
		return nil
	}
	switch dst.Kind() {
	case reflect.String:
		switch s := src.(type) {
		case string:
			dst.SetString(s)
		case []byte:
			dst.SetString(string(s))
		case time.Time:
			dst.SetString(s.Format(time.RFC3339Nano))
		default:
			dst.SetString(fmt.Sprint(s))
		}
		return nil
	case reflect.Bool:
		switch s := src.(type) {
		case bool:
			dst.SetBool(s)
		case int64:
			dst.SetBool(s != 0)
		default:
			b, err := strconv.ParseBool(text(s))
			if err != nil {
				return fmt.Errorf("orm: %v into bool: %w", src, err)
			}
			dst.SetBool(b)
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch s := src.(type) {
		case int64:
			dst.SetInt(s)
		case float64:
			dst.SetInt(int64(s))
		case bool:
			dst.SetInt(map[bool]int64{true: 1}[s])
		default:
			n, err := strconv.ParseInt(text(s), 10, 64)
			if err != nil {
				return fmt.Errorf("orm: %v into %s: %w", src, dst.Type(), err)
			}
			dst.SetInt(n)
		}
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch s := src.(type) {
		case int64:
			dst.SetUint(uint64(s))
		default:
			n, err := strconv.ParseUint(text(s), 10, 64)
			if err != nil {
				return fmt.Errorf("orm: %v into %s: %w", src, dst.Type(), err)
			}
			dst.SetUint(n)
		}
		return nil
	case reflect.Float32, reflect.Float64:
		switch s := src.(type) {
		case float64:
			dst.SetFloat(s)
		case int64:
			dst.SetFloat(float64(s))
		default:
			n, err := strconv.ParseFloat(text(s), 64)
			if err != nil {
				return fmt.Errorf("orm: %v into %s: %w", src, dst.Type(), err)
			}
			dst.SetFloat(n)
		}
		return nil
	case reflect.Slice:
		if dst.Type().Elem().Kind() == reflect.Uint8 {
			switch s := src.(type) {
			case []byte:
				dst.SetBytes(append([]byte(nil), s...))
				return nil
			case string:
				dst.SetBytes([]byte(s))
				return nil
			}
		}
	}
	v := reflect.ValueOf(src)
	if v.Type().ConvertibleTo(dst.Type()) {
		dst.Set(v.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("orm: can't put %T into %s", src, dst.Type())
}

func text(v any) string {
	switch s := v.(type) {
	case []byte:
		return string(s)
	case string:
		return s
	}
	return fmt.Sprint(v)
}

var timeLayouts = []string{
	time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02",
}

// asTime reads a time the way SQLite and MySQL may hand it over: as text.
func asTime(src any) (time.Time, error) {
	switch s := src.(type) {
	case time.Time:
		return s, nil
	case int64:
		return time.Unix(s, 0).UTC(), nil
	}
	str := text(src)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, str); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("orm: %q is not a time", str)
}

// value is a field as a query argument: its Valuer if it has one, nil for
// a nil pointer, else the value itself.
func value(v reflect.Value) any {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		if _, ok := v.Interface().(driver.Valuer); ok {
			return v.Interface()
		}
		return value(v.Elem())
	}
	return v.Interface()
}

// setField puts a value a user gave (Defaults, Set) into a field,
// converting what can be converted.
func setField(dst reflect.Value, val any) error {
	if val == nil {
		dst.SetZero()
		return nil
	}
	v := reflect.ValueOf(val)
	switch {
	case v.Type().AssignableTo(dst.Type()):
		dst.Set(v)
		return nil
	case dst.Kind() == reflect.Pointer && v.Type().AssignableTo(dst.Type().Elem()):
		p := reflect.New(dst.Type().Elem())
		p.Elem().Set(v)
		dst.Set(p)
		return nil
	case v.Type().ConvertibleTo(dst.Type()) && v.Kind() != reflect.String:
		dst.Set(v.Convert(dst.Type()))
		return nil
	}
	return assign(dst, val)
}
