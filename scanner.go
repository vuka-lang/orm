package orm

import (
	"database/sql/driver"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// RowScanner reads a model's rows without reflection: generated per model
// by orm generate (or ormgen, from go:generate), never written by hand.
type RowScanner[T any] interface {
	// Dest is the scan destinations, one per column; the same slice for
	// every row.
	Dest() []any
	// Bind points the destinations at row's fields.
	Bind(row *T)
}

// RowWriter is a generated scanner's other half: a row's column values
// for an INSERT, in the scanner's column order, without reflection.
// ormgen writes it beside the scanner of a model with no pointer embeds.
type RowWriter[T any] interface {
	// Values appends row's column values to dst[:0].
	Values(row *T, dst []any) []any
}

// PtrValue is a pointer field's value for the driver, for generated
// writers: nil for nil, the pointer for a driver.Valuer, else what it
// points to.
func PtrValue[V any](p *V) any {
	if p == nil {
		return nil
	}
	if v, ok := any(p).(driver.Valuer); ok {
		return v
	}
	return *p
}

type scannerEntry struct {
	cols []string
	make any // func() RowScanner[T]
}

var (
	scanners   sync.Map // reflect.Type → scannerEntry
	scannerVer atomic.Int64
	// reflectOnly ignores the registered scanners, as a program without
	// orm generate's files reads rows: for the fallback's tests.
	reflectOnly atomic.Bool
)

// RegisterScanner installs a generated scanner for T, reading cols in
// order. A scanner whose columns aren't the model's (the model changed
// after the code was generated) is ignored and rows are read by
// reflection.
func RegisterScanner[T any](cols []string, make func() RowScanner[T]) {
	scanners.Store(reflect.TypeFor[T](), scannerEntry{cols, make})
	scannerVer.Add(1)
}

// scannerHit is what rowScanner found for a model, as of scannerVer ver.
type scannerHit struct {
	ver  int64
	make any // func() RowScanner[T], nil for none
}

// rowScanner is T's generated scanner, when it reads the model's columns;
// found once per model while no scanner is registered since.
func rowScanner[T any](m *model) (func() RowScanner[T], bool) {
	ver := scannerVer.Load()
	h := m.scanner.Load()
	if h == nil || h.ver != ver {
		h = &scannerHit{ver: ver}
		if v, ok := scanners.Load(reflect.TypeFor[T]()); ok && !reflectOnly.Load() {
			if e := v.(scannerEntry); len(m.via) == 0 && slices.Equal(e.cols, m.columns()) {
				h.make = e.make
			}
		}
		m.scanner.Store(h)
	}
	mk, ok := h.make.(func() RowScanner[T])
	return mk, ok
}

// Cell is a scan destination for a field of type V, for generated
// scanners: the common types convert without reflection, any other
// through the ORM's own conversion, NULL to the zero value.
type Cell[V any] struct{ P *V }

func (c *Cell[V]) Scan(src any) error {
	switch p := any(c.P).(type) {
	case *string:
		switch s := src.(type) {
		case string:
			*p = s
			return nil
		case []byte:
			*p = string(s)
			return nil
		case nil:
			*p = ""
			return nil
		}
	case *int64:
		switch s := src.(type) {
		case int64:
			*p = s
			return nil
		case nil:
			*p = 0
			return nil
		}
	case *int:
		switch s := src.(type) {
		case int64:
			*p = int(s)
			return nil
		case nil:
			*p = 0
			return nil
		}
	case *int32:
		switch s := src.(type) {
		case int64:
			*p = int32(s)
			return nil
		case nil:
			*p = 0
			return nil
		}
	case *float64:
		switch s := src.(type) {
		case float64:
			*p = s
			return nil
		case int64:
			*p = float64(s)
			return nil
		case nil:
			*p = 0
			return nil
		}
	case *bool:
		switch s := src.(type) {
		case bool:
			*p = s
			return nil
		case int64:
			*p = s != 0
			return nil
		case nil:
			*p = false
			return nil
		}
	case *time.Time:
		switch s := src.(type) {
		case time.Time:
			*p = s
			return nil
		case nil:
			*p = time.Time{}
			return nil
		}
	case *[]byte:
		switch s := src.(type) {
		case []byte:
			*p = append((*p)[:0:0], s...)
			return nil
		case nil:
			*p = nil
			return nil
		}
	}
	return assign(reflect.ValueOf(c.P).Elem(), src)
}

// PtrCell is a scan destination for a nullable field of type *V: nil for
// NULL, else a new V read as Cell reads it.
type PtrCell[V any] struct{ P **V }

func (c *PtrCell[V]) Scan(src any) error {
	if src == nil {
		*c.P = nil
		return nil
	}
	v := new(V)
	if err := (&Cell[V]{P: v}).Scan(src); err != nil {
		return err
	}
	*c.P = v
	return nil
}

// Generated is whether T's rows are read by a generated scanner: one is
// registered and reads the model's columns.
func Generated[T any]() bool {
	m, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return false
	}
	_, ok := rowScanner[T](m)
	return ok
}
