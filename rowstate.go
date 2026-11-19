package orm

import (
	"context"
	"fmt"
	"reflect"
	"unsafe"
)

// The writes of a row, on its state and its type: what Model's methods and
// RowHandle's run without T, so a model's code is not instantiated per type.

// manager is the manager the row of type t writes through.
func rsManager(s *rowState, t reflect.Type) *managerCore {
	if s.m != nil {
		return s.m
	}
	return homeCore(t)
}

// schemaOf is the schema the row of type t is of.
func rsSchema(s *rowState, t reflect.Type) DBSchema { return rsManager(s, t).schema() }

// adoptAs marks the row of type t read under schema sc as loaded from it.
func rsAdopt(s *rowState, t reflect.Type, sc DBSchema) { s.m, s.loaded = coreFor(t, sc), true }

// row is the *t the state is embedded in.
func rsRow(s *rowState, t reflect.Type) (any, error) {
	m, err := modelOf(t, "", "")
	if err != nil {
		return nil, err
	}
	return reflect.NewAt(t, unsafe.Add(unsafe.Pointer(s), -int(m.stateOff))).Interface(), nil
}

func rsUsing(s *rowState, t reflect.Type, sc DBSchema) any {
	row, err := rsRow(s, t)
	if err != nil {
		panic(err)
	}
	m := coreFor(t, sc)
	if m != rsManager(s, t) {
		s.loaded = false
	}
	s.m = m
	return row
}

func rsSave(s *rowState, ctx context.Context, t reflect.Type) error {
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	if s.loaded {
		return rsManager(s, t).saveRow(ctx, row)
	}
	return rsManager(s, t).create(ctx, row)
}

func rsSaveFields(s *rowState, ctx context.Context, t reflect.Type, fields []any) error {
	names, err := pathNames(fields)
	if err != nil {
		return err
	}
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	return rsManager(s, t).saveFields(ctx, row, s.loaded, names)
}

func rsRefresh(s *rowState, ctx context.Context, t reflect.Type) error {
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	m := rsManager(s, t)
	key, err := m.refreshKey(row)
	if err != nil {
		return err
	}
	got := reflect.New(t)
	if err := m.get(ctx, got.Interface(), key); err != nil {
		return err
	}
	reflect.ValueOf(row).Elem().Set(got.Elem())
	return nil
}

func rsRemove(s *rowState, ctx context.Context, t reflect.Type) error {
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	return rsManager(s, t).remove(ctx, row)
}

func rsLoad(s *rowState, ctx context.Context, t reflect.Type, specs []any) error {
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	return rsManager(s, t).loadInto(ctx, row, specs)
}

func rsLink(s *rowState, ctx context.Context, t reflect.Type, relation any, related []any, do func(context.Context, links, []any) error) error {
	row, err := rsRow(s, t)
	if err != nil {
		return err
	}
	return rsManager(s, t).link(ctx, row, s.loaded, pathName(relation), related, do)
}

// RowHandle is a row, any model's *T, as Row names it without T: its
// methods are Model's. It is what the methods orm generate writes on a
// model call, so the generated code instantiates nothing per model:
//
//	func (x *Post) Save(ctx context.Context) error { return orm.RowFor(x).Save(ctx) }
type RowHandle struct{ row any }

// RowFor is the writes of row, a *T of a model embedding orm.Model,
// orm.Base or orm.Rec: RowFor(p).Save(ctx) is p.Save(ctx) as Model has it.
func RowFor(row any) RowHandle { return RowHandle{row} }

// state is the row's embedded state and its type.
func (r RowHandle) state() (*rowState, reflect.Type, error) {
	v := reflect.ValueOf(r.row)
	if v.Kind() != reflect.Pointer || v.Type().Elem().Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("orm: RowFor of %T: a pointer to a model is wanted", r.row)
	}
	if v.IsNil() {
		panic(fmt.Sprintf("orm: a nil %v", v.Type()))
	}
	t := v.Type().Elem()
	m, err := modelOf(t, "", "")
	if err != nil {
		return nil, nil, err
	}
	if m.state == nil {
		return nil, nil, fmt.Errorf("orm: %s doesn't embed orm.Model[%s]", m.Name, m.Name)
	}
	return (*rowState)(unsafe.Add(v.UnsafePointer(), m.stateOff)), t, nil
}

// Using is Model.Using; the row itself is what it returns.
func (r RowHandle) Using(sc DBSchema) {
	s, t, err := r.state()
	if err != nil {
		panic(err)
	}
	rsUsing(s, t, sc)
}

// Save is Model.Save.
func (r RowHandle) Save(ctx context.Context) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsSave(s, ctx, t)
}

// SaveFields is Model.SaveFields.
func (r RowHandle) SaveFields(ctx context.Context, fields ...any) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsSaveFields(s, ctx, t, fields)
}

// Refresh is Model.Refresh.
func (r RowHandle) Refresh(ctx context.Context) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsRefresh(s, ctx, t)
}

// Delete is Model.Delete.
func (r RowHandle) Delete(ctx context.Context) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsRemove(s, ctx, t)
}

// Load is Model.Load.
func (r RowHandle) Load(ctx context.Context, specs ...any) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsLoad(s, ctx, t, specs)
}

// Add is Model.Add.
func (r RowHandle) Add(ctx context.Context, relation any, related ...any) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsLink(s, ctx, t, relation, related, addLinks)
}

// Remove is Model.Remove.
func (r RowHandle) Remove(ctx context.Context, relation any, related ...any) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsLink(s, ctx, t, relation, related, removeLinks)
}

// Set is Model.Set.
func (r RowHandle) Set(ctx context.Context, relation any, related ...any) error {
	s, t, err := r.state()
	if err != nil {
		return err
	}
	return rsLink(s, ctx, t, relation, related, setLinks)
}
