package orm

import (
	"context"
	"fmt"
	"reflect"
	"unsafe"
)

// Row is the writes of row, a model embedding orm.Model (or orm.Base), as
// the row itself: Row(p).Save(ctx) is p.Save(ctx) as Model has it. It is
// what the methods orm generate writes on each model call,
//
//	func (x *Post) Save(ctx context.Context) error { return orm.Row(x).Save(ctx) }
//
// so a call p.Save(ctx) passes the whole row, not only its embedded Model:
// analyzers (go vet's unusedwrite) then see the fields set before it read.
func Row[T any](row *T) RowRef[T] { return RowRef[T]{row} }

// RowRef is a row Row names; its methods are Model's.
type RowRef[T any] struct{ row *T }

// state is the row's embedded Model.
func (r RowRef[T]) state() (*Model[T], error) {
	if r.row == nil {
		panic(fmt.Sprintf("orm: a nil *%v", reflect.TypeFor[T]()))
	}
	m, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return nil, err
	}
	if m.state == nil {
		return nil, fmt.Errorf("orm: %s doesn't embed orm.Model[%s]", m.Name, m.Name)
	}
	return (*Model[T])(unsafe.Add(unsafe.Pointer(r.row), int(m.stateOff))), nil
}

// Using is Model.Using.
func (r RowRef[T]) Using(sc DBSchema) *T {
	s, err := r.state()
	if err != nil {
		panic(err)
	}
	return s.Using(sc)
}

// Save is Model.Save.
func (r RowRef[T]) Save(ctx context.Context) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Save(ctx)
}

// SaveFields is Model.SaveFields.
func (r RowRef[T]) SaveFields(ctx context.Context, fields ...any) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.SaveFields(ctx, fields...)
}

// Refresh is Model.Refresh.
func (r RowRef[T]) Refresh(ctx context.Context) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// Delete is Model.Delete.
func (r RowRef[T]) Delete(ctx context.Context) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Delete(ctx)
}

// Load is Model.Load.
func (r RowRef[T]) Load(ctx context.Context, specs ...any) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Load(ctx, specs...)
}

// Add is Model.Add.
func (r RowRef[T]) Add(ctx context.Context, relation any, related ...any) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Add(ctx, relation, related...)
}

// Remove is Model.Remove.
func (r RowRef[T]) Remove(ctx context.Context, relation any, related ...any) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Remove(ctx, relation, related...)
}

// Set is Model.Set.
func (r RowRef[T]) Set(ctx context.Context, relation any, related ...any) error {
	s, err := r.state()
	if err != nil {
		return err
	}
	return s.Set(ctx, relation, related...)
}
