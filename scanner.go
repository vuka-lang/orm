package orm

import (
	"database/sql/driver"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/vuka/v2"
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

// OptValue is an Option field's value for the driver, for generated
// writers: nil for None, else the value.
func OptValue[V any](o vuka.Option[V]) any {
	v, ok := o.Get()
	if !ok {
		return nil
	}
	return v
}

// scannerEntry is a generated scanner without its Go type: what code that
// knows the model by its reflect.Type asks of it.
type scannerEntry struct {
	cols []string
	make func() RowScan
	// pool is scanners done reading, for the next query's rows: a scanner
	// is one query's at a time.
	pool sync.Pool
	// writer is the generated RowWriter's Values, nil for none.
	writer func() func(row any, dst []any) []any
}

// get is a scanner of e's for one query's rows: put it back after.
func (e *scannerEntry) get() RowScan {
	if sc, ok := e.pool.Get().(RowScan); ok {
		return sc
	}
	return e.make()
}

func (e *scannerEntry) put(sc RowScan) { e.pool.Put(sc) }

var (
	scanners   sync.Map // reflect.Type → *scannerEntry
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
	storeScanner(reflect.TypeFor[T](), &scannerEntry{
		cols: cols,
		make: func() RowScan { return &typedScan[T]{make()} },
		writer: func() func(any, []any) []any {
			w, ok := make().(RowWriter[T])
			if !ok {
				return nil
			}
			return func(row any, dst []any) []any { return w.Values(row.(*T), dst) }
		},
	})
}

// typedScan is a RowScanner[T] as a RowScan: Bind zeroes the row first.
type typedScan[T any] struct{ sc RowScanner[T] }

func (s *typedScan[T]) Dest() []any { return s.sc.Dest() }

func (s *typedScan[T]) Bind(row any) {
	r := row.(*T)
	var blank T
	*r = blank
	s.sc.Bind(r)
}

// RowScan is RowScanner without T, for the scanners orm generate writes:
// a package of many models then holds no instantiation per model.
type RowScan interface {
	// Dest is the scan destinations, one per column; the same slice for
	// every row.
	Dest() []any
	// Bind zeroes row, a *T, and points the destinations at its fields.
	Bind(row any)
}

// RowValues is RowWriter without T: a RowScan's other half, a row's
// column values for an INSERT, in the scanner's column order.
type RowValues interface {
	// Values appends row's column values, a *T, to dst[:0].
	Values(row any, dst []any) []any
}

// RegisterScan installs a generated scanner for the model model points to,
// a (*T)(nil), reading cols in order, as RegisterScanner does.
func RegisterScan(model any, cols []string, make func() RowScan) {
	storeScanner(reflect.TypeOf(model).Elem(), &scannerEntry{
		cols: cols,
		make: make,
		writer: func() func(any, []any) []any {
			w, ok := make().(RowValues)
			if !ok {
				return nil
			}
			return w.Values
		},
	})
}

func storeScanner(t reflect.Type, e *scannerEntry) {
	scanners.Store(t, e)
	scannerVer.Add(1)
}

// scannerHit is what scannerOf found for a model, as of scannerVer ver.
type scannerHit struct {
	ver   int64
	entry *scannerEntry // nil for none
}

// scannerOf is t's generated scanner, nil for none, when it reads the
// model's columns; found once per model while no scanner is registered since.
func scannerOf(m *model, t reflect.Type) *scannerEntry {
	ver := scannerVer.Load()
	h := m.scanner.Load()
	if h == nil || h.ver != ver {
		h = &scannerHit{ver: ver}
		if v, ok := scanners.Load(t); ok && !reflectOnly.Load() {
			if e := v.(*scannerEntry); len(m.via) == 0 && slices.Equal(e.cols, m.columns()) {
				h.entry = e
			}
		}
		m.scanner.Store(h)
	}
	return h.entry
}

// writer is the generated RowWriter's Values for m's rows, nil for none.
func (m *managerCore) writer() func(row any, dst []any) []any {
	if e := scannerOf(m.meta, m.rowType); e != nil {
		return e.writer()
	}
	return nil
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

// OptCell is a scan destination for a nullable field of type
// vuka.Option[V]: None for NULL, else Some of a V read as Cell reads it.
type OptCell[V any] struct{ P *vuka.Option[V] }

func (c *OptCell[V]) Scan(src any) error {
	if src == nil {
		*c.P = vuka.None[V]()
		return nil
	}
	var v V
	if err := (&Cell[V]{P: &v}).Scan(src); err != nil {
		return err
	}
	*c.P = vuka.Some(v)
	return nil
}

// Generated is whether T's rows are read by a generated scanner: one is
// registered and reads the model's columns.
func Generated[T any]() bool {
	m, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return false
	}
	return scannerOf(m, reflect.TypeFor[T]()) != nil
}
