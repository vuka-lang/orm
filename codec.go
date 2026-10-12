package orm

import (
	"fmt"
	"reflect"
	"time"
)

// SchemaCodec converts a single field's value to and from its database
// form under a names set, at the field level. orm.Codec is the only
// implementation: build one with orm.Codec[G, DB]{To, From} and attach it
// to a field for a names set (SchemaField.Codec, from a FieldAttr's Apply
// or SetFieldOptions). The methods are unexported so the ORM owns the
// conversion; a type that converts itself per schema uses SchemaValuer.
type SchemaCodec interface {
	encode(g any) (any, error)   // Go value → database value
	decode(src any) (any, error) // database value → Go value
}

// Codec adapts a field's value under a names set: its Go value G is
// written as DB (To) and read back as G (From), so a field keeps one Go
// type while its database form differs per schema — a status string kept
// as an integer code on a legacy table, a time kept as text. Unlike a
// SchemaValuer the field's Go type is untouched: the conversion rides the
// field, set under a names set, not the type.
//
//	orm.Codec[string, int64]{
//		To:   func(s string) (int64, error) { return codes[s], nil },
//		From: func(c int64) (string, error) { return names[c], nil },
//	}
//
// A field codec wins over the field type's own SchemaValuer/SchemaScanner
// and DBValuer: the codec is the field-level instruction, applied in place
// of the type's.
type Codec[G, DB any] struct {
	To   func(G) (DB, error)
	From func(DB) (G, error)
}

func (c Codec[G, DB]) encode(g any) (any, error) {
	gv, err := codecIn[G](g)
	if err != nil {
		return nil, err
	}
	db, err := c.To(gv)
	if err != nil {
		return nil, err
	}
	return any(db), nil
}

func (c Codec[G, DB]) decode(src any) (any, error) {
	var db DB
	if err := assign(reflect.ValueOf(&db).Elem(), src); err != nil {
		return nil, fmt.Errorf("orm: codec reading %T: %w", src, err)
	}
	g, err := c.From(db)
	if err != nil {
		return nil, err
	}
	return any(g), nil
}

// codecIn is g as the codec's Go type G: its own type, or a value
// convertible to it (a defined string from a plain string, a pointer's
// element). nil (a NULL) never reaches here — out returns early for it.
func codecIn[G any](g any) (G, error) {
	if v, ok := g.(G); ok {
		return v, nil
	}
	var zero G
	rv := reflect.ValueOf(g)
	tt := reflect.TypeFor[G]()
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return zero, nil
		}
		rv = rv.Elem()
	}
	if rv.IsValid() {
		if rv.Type() == tt {
			return rv.Interface().(G), nil
		}
		if rv.Type().ConvertibleTo(tt) {
			return rv.Convert(tt).Interface().(G), nil
		}
	}
	return zero, fmt.Errorf("orm: codec expected %s, got %T", tt, g)
}

// TimeAsText is a codec keeping a time.Time as text in layout under a
// names set (a legacy table whose timestamps are strings), reading it back
// by the same layout.
func TimeAsText(layout string) Codec[time.Time, string] {
	return Codec[time.Time, string]{
		To:   func(t time.Time) (string, error) { return t.Format(layout), nil },
		From: func(s string) (time.Time, error) { return time.Parse(layout, s) },
	}
}

// CodeMap is a codec mapping a string value to and from an integer code
// under a names set (a legacy enum kept as a number): codes gives each
// value its code, and a value or code it doesn't hold is an error.
func CodeMap[S ~string](codes map[S]int64) Codec[S, int64] {
	rev := make(map[int64]S, len(codes))
	for v, c := range codes {
		rev[c] = v
	}
	return Codec[S, int64]{
		To: func(s S) (int64, error) {
			c, ok := codes[s]
			if !ok {
				return 0, fmt.Errorf("no code for %q", s)
			}
			return c, nil
		},
		From: func(c int64) (S, error) {
			s, ok := rev[c]
			if !ok {
				return "", fmt.Errorf("unknown code %d", c)
			}
			return s, nil
		},
	}
}
