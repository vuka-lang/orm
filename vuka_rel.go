package orm

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/vuka-lang/vuka/v2"
)

// relField is a field type holding a relation: orm.FK, orm.M2M.
type relField interface {
	relTarget() reflect.Type
	relMany() bool
}

func relFieldOf(t reflect.Type) (target reflect.Type, many, ok bool) {
	if t.Kind() != reflect.Struct || t.PkgPath() != modelPkg {
		return nil, false, false
	}
	r, ok := reflect.Zero(t).Interface().(relField)
	if !ok {
		return nil, false, false
	}
	return r.relTarget(), r.relMany(), true
}

// holder is a relation field the ORM hands the related rows it read.
type holder interface{ hold(rows []reflect.Value) }

// binder is a relation field the ORM tells its row's key.
type binder interface {
	bind(r *relation, key any, s DBSchema)
}

// FK is a foreign key to a T, as one field: the column <field>_id holding
// the related row's key, and that row once loaded (SelectRelated,
// PrefetchRelated, Get). It implements vuka.Related[T], so a Vuka field
// reference goes on into T's fields: Post.Author.Name.
//
//	type Post struct {
//		orm.Base
//		ID     int64
//		Author orm.FK[User] @orm.Rel{OnDelete: orm.Cascade, Related: "posts"}
//	}
//
//	p := Post{Author: orm.FKOf(&ada)}
//	author := p.Author.Get(ctx)?                       // loaded once, then kept
//	posts := Post.Objects.Filter(Post.Author.Name.Eq("ada")).SelectRelated(Post.Author).All(ctx)?
//
// In JSON it is the key. The column is NOT NULL unless Rel{Null: true}
// (or @orm.Null): an unset FK is then NULL.
type FK[T any] struct {
	key any
	row *T
}

// FKOf is a foreign key to row: its key is read from the row when the FK
// is written, so the row may be saved after the FK is made.
func FKOf[T any](row *T) FK[T] { return FK[T]{row: row} }

// FKID is a foreign key to the T whose key is key.
func FKID[T any](key any) FK[T] { return FK[T]{key: normKey(key)} }

func (FK[T]) relTarget() reflect.Type { return reflect.TypeFor[T]() }
func (FK[T]) relMany() bool           { return false }

// Related is the related row when it is loaded, else nil.
func (f FK[T]) Related() *T { return f.row }

// Loaded is whether the related row is held.
func (f FK[T]) Loaded() bool { return f.row != nil }

// Key is the related row's key; nil when unset.
func (f FK[T]) Key() any { return f.columnValue() }

// ID is the related row's key as an integer; 0 when unset or not one.
func (f FK[T]) ID() int64 {
	n, _ := f.columnValue().(int64)
	return n
}

// IsSet is whether the FK refers to a row.
func (f FK[T]) IsSet() bool { return f.columnValue() != nil }

// Set makes the FK refer to row; SetID to the row of key key.
func (f *FK[T]) Set(row *T)    { *f = FKOf(row) }
func (f *FK[T]) SetID(key any) { *f = FKID[T](key) }

// Get is the related row: the one held, else read and then held.
// orm.NotFound when the FK is unset or the row is gone.
func (f *FK[T]) Get(ctx context.Context) vuka.Result[T] {
	if f.row != nil {
		return vuka.Ok(*f.row)
	}
	k := f.columnValue()
	if k == nil {
		return vuka.Err[T](NotFound)
	}
	m := Objects[T]()
	if m.c.err != nil {
		return vuka.Err[T](m.c.err)
	}
	row, err := m.Get(ctx, Q{m.c.meta.PK.Name: k})
	if err != nil {
		return vuka.Err[T](bareKind(err))
	}
	f.row = &row
	return vuka.Ok(row)
}

func (f FK[T]) columnValue() any {
	if f.row != nil {
		if k := rowKeyOf(reflect.ValueOf(f.row).Elem()); k != nil {
			return k
		}
	}
	return f.key
}

// rowKeyOf is row's primary key, nil when it has none or it is zero.
func rowKeyOf(row reflect.Value) any {
	if m, err := modelOf(row.Type(), "", ""); err == nil && m.PK != nil {
		k := key(row, m.PK)
		if k != nil && !reflect.ValueOf(k).IsZero() {
			return k
		}
	}
	return nil
}

func (f FK[T]) Value() (driver.Value, error) { return f.columnValue(), nil }

func (f *FK[T]) Scan(src any) error {
	if b, ok := src.([]byte); ok {
		src = string(b)
	}
	*f = FK[T]{key: normKey(src)}
	return nil
}

func (f *FK[T]) hold(rows []reflect.Value) {
	if len(rows) > 0 {
		f.row = rows[0].Interface().(*T)
	}
}

func (f FK[T]) MarshalJSON() ([]byte, error) { return json.Marshal(f.columnValue()) }

func (f *FK[T]) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if n, ok := v.(float64); ok && n == float64(int64(n)) {
		v = int64(n)
	}
	*f = FK[T]{key: v}
	return nil
}

func (f FK[T]) String() string {
	if k := f.columnValue(); k != nil {
		return fmt.Sprintf("%s(%v)", reflect.TypeFor[T]().Name(), k)
	}
	return reflect.TypeFor[T]().Name() + "(nil)"
}

// M2M is a many-to-many relation to T, as one field: a table between
// (<table>_<field>, or Rel's Through) holding both keys. Its rows are read
// with All (held once PrefetchRelated loaded them) or queried with Query;
// Add, Remove, Set and Clear change the links of a saved row. A field
// reference goes on into T's fields: Post.Tags.Name.
//
//	p.Tags.Add(ctx, &golang, &db)?
//	tags := p.Tags.All(ctx)?
//	posts := Post.Objects.Filter(Post.Tags.Name.Eq("go")).PrefetchRelated(Post.Tags).All(ctx)?
type M2M[T any] struct {
	r      *relation
	key    any
	sc     DBSchema
	rows   []T
	loaded bool
}

func (M2M[T]) relTarget() reflect.Type { return reflect.TypeFor[T]() }
func (M2M[T]) relMany() bool           { return true }

// Related is the first related row held, else nil: what a field reference
// through the relation reads in memory.
func (m M2M[T]) Related() *T {
	if len(m.rows) == 0 {
		return nil
	}
	return &m.rows[0]
}

// Loaded is whether the related rows are held; Rows is them.
func (m M2M[T]) Loaded() bool { return m.loaded }
func (m M2M[T]) Rows() []T    { return m.rows }

func (m *M2M[T]) bind(r *relation, key any, s DBSchema) { m.r, m.key, m.sc = r, key, s }

func (m *M2M[T]) hold(rows []reflect.Value) {
	m.rows = make([]T, len(rows))
	for i, r := range rows {
		m.rows[i] = *r.Interface().(*T)
	}
	m.loaded = true
}

func (m M2M[T]) MarshalJSON() ([]byte, error) {
	if m.rows == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(m.rows)
}

var errUnsaved = errors.New("orm: the row is not saved yet: Save it before reaching its many-to-many rows")

// Query is the related rows as a query.
func (m M2M[T]) Query() Query[T] {
	qs := managerFor[T](m.sc).QuerySet
	if m.r == nil || m.key == nil {
		return QueryOf(qs.Filter(errCond{errUnsaved}))
	}
	_, rf, err := m.r.ends()
	if err != nil {
		return QueryOf(qs.Filter(errCond{err}))
	}
	return QueryOf(qs.Filter(throughCond{m.r, rf, m.key}))
}

// All is the related rows: those held, else read (and not held).
func (m M2M[T]) All(ctx context.Context) vuka.Result[[]T] {
	if m.loaded {
		return vuka.Ok(m.rows)
	}
	return m.Query().All(ctx)
}

// Add links the row to related rows (rows or keys), Remove unlinks them,
// Set makes them its only links, Clear removes every link.
func (m *M2M[T]) Add(ctx context.Context, related ...any) error {
	return m.link(ctx, related, addLinks)
}

func (m *M2M[T]) Remove(ctx context.Context, related ...any) error {
	return m.link(ctx, related, removeLinks)
}

func (m *M2M[T]) Set(ctx context.Context, related ...any) error {
	return m.link(ctx, related, setLinks)
}

func (m *M2M[T]) Clear(ctx context.Context) error { return m.Set(ctx) }

func (m *M2M[T]) link(ctx context.Context, related []any, do func(context.Context, links, []any) error) error {
	l, keys, err := m2mLinks(ctx, m.r, m.key, m.sc, related)
	if err != nil {
		return err
	}
	m.rows, m.loaded = nil, false
	return do(ctx, l, keys)
}

// m2mLinks is the table between of the row of key local's relation r on
// schema sc, and related's keys, distinct.
func m2mLinks(ctx context.Context, r *relation, local any, sc DBSchema, related []any) (links, []any, error) {
	if r == nil || local == nil {
		return links{}, nil, errUnsaved
	}
	_, rf, err := r.ends()
	if err != nil {
		return links{}, nil, err
	}
	var keys []any
	seen := map[any]bool{}
	for _, x := range related {
		v := reflect.ValueOf(x)
		for v.Kind() == reflect.Pointer && v.Type().Elem() == r.Target {
			v = v.Elem()
		}
		k := normKey(argValue(x))
		if v.Type() == r.Target {
			k = key(v, rf)
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	mi := infoOf(r.owner.Type)
	if sc.DB != "" {
		mi.Meta.DB = sc.DB
	}
	c, err := resolve(ctx, databasesOf(ctx), &mi, "", true)
	if err != nil {
		return links{}, nil, err
	}
	return links{db: c.src, r: r, local: local}, keys, nil
}
