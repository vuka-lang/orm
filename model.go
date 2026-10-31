package orm

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unsafe"
)

// Model is a model's base, Django's models.Model: embedded by value in
// the model it is of, it gives each row the writes of its own and the
// model its manager.
//
//	type User struct {
//		orm.Model[User]
//		ID    int64
//		Email string
//		Roles []Role `gorm:"many2many:user_roles"`
//	}
//
//	u := User{Email: e}
//	err := u.Save(ctx)                                    // INSERT: u.ID set
//	u.Email = e2
//	err = u.Save(ctx)                                     // UPDATE
//	err = u.Add(ctx, "roles", admin)                      // a user_roles row
//	users, err := User{}.Objects().Filter(orm.Q{"roles__name": "admin"}).All(ctx)
//
// A row remembers whether it was read from (or written to) the database,
// Django's _state.adding, and the schema it was: rows read through a
// QuerySet, Raw, Get or Create come back loaded, related rows
// SelectRelated and PrefetchRelated load too. Its state is unexported, so
// GORM and JSON ignore it. Its methods find the row from the embedded
// field, so embed it exactly once, by value, in the type it names
// (directly or through an embedded struct); Register and boot fail on
// anything else.
type Model[T any] struct {
	m      *Manager[T] // the manager of the schema the row is of; nil: the model's own
	loaded bool
}

// vukaDeclares, vukaSchema and vukaFields mark a type embedding Model as
// declaring a model, as Base's do.
func (Model[T]) vukaDeclares()                           {}
func (Model[T]) vukaSchema(interface{ Schema(*Schema) }) {}
func (Model[T]) vukaFields()                             {}

// stater is a Model's state, as code that doesn't know T reaches it.
type stater interface {
	adopt(s DBSchema)
	schema() DBSchema
}

func (s *Model[T]) adopt(sc DBSchema) { s.m, s.loaded = managerFor[T](sc), true }

func (s *Model[T]) schema() DBSchema { return s.manager().c.schema() }

// manager is the manager the row writes through.
func (s *Model[T]) manager() *Manager[T] {
	if s.m != nil {
		return s.m
	}
	return home[T]()
}

// row is the model value s is embedded in.
func (s *Model[T]) row() (*T, error) {
	m, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return nil, err
	}
	return (*T)(unsafe.Add(unsafe.Pointer(s), -int(m.stateOff))), nil
}

// Objects is the model's manager, Django's Model.objects: Objects() its
// own (the package-level orm.For[T]() without Names it has, else one made
// from its Meta), Objects(s) the one on schema s (orm.Of[T](s)).
//
//	n, err := User{}.Objects().Filter(orm.Q{"active": true}).Count(ctx)
func (Model[T]) Objects(s ...DBSchema) *Manager[T] { return Objects[T](s...) }

// Using binds the row to schema s and returns it: its writes go there.
// A row loaded from another schema is new to s, so Save inserts it (a
// copy of the row there).
func (s *Model[T]) Using(sc DBSchema) *T {
	row, err := s.row()
	if err != nil {
		panic(err)
	}
	m := managerFor[T](sc)
	if m != s.manager() {
		s.loaded = false
	}
	s.m = m
	return row
}

// Save writes the row: an INSERT when it is new (its key set, and the row
// loaded, after), else an UPDATE of every field by its primary key.
func (s *Model[T]) Save(ctx context.Context) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	if s.loaded {
		return s.manager().c.saveRow(ctx, row)
	}
	return s.manager().c.create(ctx, row)
}

// SaveFields updates only the fields named, and the auto_now ones, of a
// loaded row: Django's save(update_fields=…). A field is a typed field
// (PostColumns.Title), a Go name or a column.
func (s *Model[T]) SaveFields(ctx context.Context, fields ...any) error {
	names, err := pathNames(fields)
	if err != nil {
		return err
	}
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.saveFields(ctx, row, s.loaded, names)
}

func (m *managerCore) saveFields(ctx context.Context, row any, loaded bool, names []string) error {
	if m.err != nil {
		return m.err
	}
	if !loaded {
		return fmt.Errorf("orm: SaveFields of a %s not saved yet: Save it first", m.meta.Name)
	}
	fs := make([]*field, len(names))
	for i, name := range names {
		f, ok := m.meta.field(name)
		if !ok {
			return fmt.Errorf("orm: %s has no field %q", m.meta.Name, name)
		}
		fs[i] = f
	}
	return m.save(ctx, row, fs)
}

// Refresh reads the row again by its primary key, Django's
// refresh_from_db: every field, and relations emptied.
func (s *Model[T]) Refresh(ctx context.Context) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	m := s.manager()
	key, err := m.c.refreshKey(row)
	if err != nil {
		return err
	}
	got, err := m.Get(ctx, key)
	if err != nil {
		return err
	}
	*row = got
	return nil
}

// refreshKey is the condition of row's own row.
func (m *managerCore) refreshKey(row any) (Q, error) {
	if m.err != nil {
		return nil, m.err
	}
	pk := m.meta.PK
	if pk == nil {
		return nil, fmt.Errorf("orm: %s has no primary key to refresh by", m.meta.Name)
	}
	return Q{pk.Name: value(peek(reflect.ValueOf(row).Elem(), pk.Index, pk.Type))}, nil
}

// Delete deletes the row by its primary key. The row is new again after:
// Save inserts it.
func (s *Model[T]) Delete(ctx context.Context) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.remove(ctx, row)
}

// Load loads relations into the row, as PrefetchRelated loads them into
// a query's rows: typed fields (UserColumns.Roles), references, names,
// paths (posts__comments) or orm.Prefetch.
//
//	err := u.Load(ctx, "roles", "profile")
func (s *Model[T]) Load(ctx context.Context, specs ...any) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.loadInto(ctx, row, specs)
}

func (m *managerCore) loadInto(ctx context.Context, row any, specs []any) error {
	if m.err != nil {
		return m.err
	}
	c, err := m.conn(ctx, false)
	if err != nil {
		return err
	}
	return prefetch(ctx, c, m.meta, m.schema(), []reflect.Value{reflect.ValueOf(row).Elem()}, appendSpecs(nil, prefetchSpecs(specs)))
}

// Add links the row to related rows through a many-to-many relation,
// Django's related manager add(): a row of the table between for each
// not linked yet. The relation is named by typed field (UserColumns.Roles),
// reference or name; related is rows of the relation's model (values or
// pointers) or their keys.
//
//	err := u.Add(ctx, "roles", admin, editor)
func (s *Model[T]) Add(ctx context.Context, relation any, related ...any) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.link(ctx, row, s.loaded, pathName(relation), related, addLinks)
}

// Remove unlinks the row from related rows through a many-to-many
// relation, Django's remove(): their rows of the table between go.
func (s *Model[T]) Remove(ctx context.Context, relation any, related ...any) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.link(ctx, row, s.loaded, pathName(relation), related, removeLinks)
}

// Set makes related the rows the row is linked to through a many-to-many
// relation, Django's set(): links to others are removed, missing ones
// added, in one transaction.
func (s *Model[T]) Set(ctx context.Context, relation any, related ...any) error {
	row, err := s.row()
	if err != nil {
		return err
	}
	return s.manager().c.link(ctx, row, s.loaded, pathName(relation), related, setLinks)
}

func addLinks(ctx context.Context, l links, keys []any) error {
	have, err := l.linked(ctx, keys)
	if err != nil {
		return err
	}
	return l.insert(ctx, slices.DeleteFunc(keys, func(k any) bool { return have[k] }))
}

func removeLinks(ctx context.Context, l links, keys []any) error {
	return l.delete(ctx, keys, false)
}

func setLinks(ctx context.Context, l links, keys []any) error {
	return AtomicOn(ctx, l.db, func(ctx context.Context) error {
		if err := l.delete(ctx, keys, true); err != nil {
			return err
		}
		have, err := l.linked(ctx, keys)
		if err != nil {
			return err
		}
		return l.insert(ctx, slices.DeleteFunc(keys, func(k any) bool { return have[k] }))
	})
}

// link runs do on the table between of row's many-to-many relation, with
// the keys of related, distinct.
func (m *managerCore) link(ctx context.Context, row any, loaded bool, name string, related []any, do func(context.Context, links, []any) error) error {
	if m.err != nil {
		return m.err
	}
	r, ok := m.meta.relation(name)
	if !ok {
		return fmt.Errorf("orm: %s has no relation %q", m.meta.Name, name)
	}
	if r.Kind != relM2M {
		return fmt.Errorf("orm: %s.%s is not many-to-many: set the foreign key and Save", m.meta.Name, r.Name)
	}
	if !loaded {
		return fmt.Errorf("orm: %s is not saved yet: Save it before linking it", m.meta.Name)
	}
	_, rf, err := r.ends()
	if err != nil {
		return err
	}
	var keys []any
	seen := make(map[any]bool, len(related))
	for _, x := range related {
		v := reflect.ValueOf(x)
		for v.Kind() == reflect.Pointer && v.Type().Elem() == r.Target {
			v = v.Elem()
		}
		k := normKey(x)
		if v.Type() == r.Target {
			k = key(v, rf)
		}
		// By a set, not a scan of the keys so far: a list of ids from a
		// request costs its length, not its square.
		if k != nil && !reflect.TypeOf(k).Comparable() {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		} else if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	c, err := m.conn(ctx, true)
	if err != nil {
		return err
	}
	return do(ctx, links{db: c.src, r: r, local: key(reflect.ValueOf(row).Elem(), r.local)}, keys)
}

// links is one row's links in a many-to-many table.
type links struct {
	db    *DB
	r     *relation
	local any
}

// linked is which of keys the row is linked to.
func (l links) linked(ctx context.Context, keys []any) (map[any]bool, error) {
	have := map[any]bool{}
	if len(keys) == 0 {
		return have, nil
	}
	c := on(ctx, l.db)
	b := &builder{d: c.d, st: &stmt{}}
	q := c.d.Quote
	s := "SELECT " + q(l.r.ThroughRemote) + " FROM " + q(l.r.Through) + " WHERE " + q(l.r.ThroughLocal) + " = " + b.arg(l.local) + " AND " + q(l.r.ThroughRemote) + " IN (" + marks(b, keys) + ")"
	rows, err := c.query(ctx, s, b.args())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k any
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		have[normKey(k)] = true
	}
	return have, rows.Err()
}

func (l links) insert(ctx context.Context, keys []any) error {
	if len(keys) == 0 {
		return nil
	}
	c := on(ctx, l.db)
	b := &builder{d: c.d, st: &stmt{}}
	q := c.d.Quote
	tuples := make([]string, len(keys))
	for i, k := range keys {
		tuples[i] = "(" + b.arg(l.local) + ", " + b.arg(k) + ")"
	}
	_, err := c.exec(ctx, "INSERT INTO "+q(l.r.Through)+" ("+q(l.r.ThroughLocal)+", "+q(l.r.ThroughRemote)+") VALUES "+strings.Join(tuples, ", "), b.args())
	return err
}

// delete removes the row's links to keys, or, with others, to every row
// but them.
func (l links) delete(ctx context.Context, keys []any, others bool) error {
	c := on(ctx, l.db)
	b := &builder{d: c.d, st: &stmt{}}
	q := c.d.Quote
	s := "DELETE FROM " + q(l.r.Through) + " WHERE " + q(l.r.ThroughLocal) + " = " + b.arg(l.local)
	switch {
	case len(keys) > 0 && others:
		s += " AND " + q(l.r.ThroughRemote) + " NOT IN (" + marks(b, keys) + ")"
	case len(keys) > 0:
		s += " AND " + q(l.r.ThroughRemote) + " IN (" + marks(b, keys) + ")"
	case !others:
		return nil
	}
	_, err := c.exec(ctx, s, b.args())
	return err
}

func marks(b *builder, vals []any) string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = b.arg(v)
	}
	return strings.Join(out, ", ")
}

// Related is the rows of R related to inst (a model row, or a pointer to
// one) by its relation named: a foreign key's row, the rows holding its
// key, or those linked to it many-to-many; queried on inst's schema.
//
//	posts, err := orm.Related[Post](&u, "posts").Filter(orm.Q{"draft": false}).All(ctx)
func Related[R any](inst any, relation string) QuerySet[R] {
	own := relatedOwnerOf(inst)
	qs := managerFor[R](own.sc).QuerySet
	qs.q = own.related(qs.q, relation, reflect.TypeFor[R]())
	return qs
}

// relatedOwner is the row a Related query starts from.
type relatedOwner struct {
	v   reflect.Value
	sc  DBSchema
	m   *model
	err error
}

func relatedOwnerOf(inst any) relatedOwner {
	v := reflect.Indirect(reflect.ValueOf(inst))
	if !v.CanAddr() {
		p := reflect.New(v.Type())
		p.Elem().Set(v)
		v = p.Elem()
	}
	var sc DBSchema
	m, err := modelOf(v.Type(), "", "")
	if err == nil && m.state != nil {
		sc = fieldOf(v, m.state).Addr().Interface().(stater).schema()
		m, err = modelOf(v.Type(), sc.Names, "")
	}
	return relatedOwner{v, sc, m, err}
}

// related is q, a query of the rows of type target, narrowed to those o's
// relation named reaches.
func (o relatedOwner) related(q *query, relation string, target reflect.Type) *query {
	refuse := func(err error) *query { return q.filter([]Cond{errCond{err}}) }
	m, err := o.m, o.err
	if err != nil {
		return refuse(err)
	}
	r, ok := m.relation(relation)
	switch {
	case !ok:
		err = fmt.Errorf("orm: %s has no relation %q", m.Name, relation)
	case r.Target != target:
		err = fmt.Errorf("orm: %s.%s holds %s, not %s", m.Name, r.Name, r.Target.Name(), target.Name())
	}
	if err != nil {
		return refuse(err)
	}
	_, rf, err := r.ends()
	if err != nil {
		return refuse(err)
	}
	if r.Kind == relElements {
		elems, err := r.elementKeys(o.v)
		if err != nil {
			return refuse(err)
		}
		return q.filter([]Cond{Q{rf.Name + "__in": elems}})
	}
	k := key(o.v, r.local)
	if r.Kind == relM2M {
		return q.filter([]Cond{throughCond{r, rf, k}})
	}
	return q.filter([]Cond{Q{rf.Name: k}})
}

// throughCond is the rows of a many-to-many's model linked to key.
type throughCond struct {
	r   *relation
	rf  *field
	key any
}

func (t throughCond) sql(b *builder) (string, error) {
	q := b.d.Quote
	return b.col(t.rf) + " IN (SELECT " + q(t.r.ThroughRemote) + " FROM " + q(t.r.Through) + " WHERE " + q(t.r.ThroughLocal) + " = " + b.arg(t.key) + ")", nil
}

// errCond is a condition that fails the query it is in with err.
type errCond struct{ err error }

func (e errCond) sql(*builder) (string, error) { return "", e.err }

// Objects is the model T's manager: Objects[T]() its own (its
// package-level orm.For[T]() without Names, else one made from its Meta
// on first use), Objects[T](s) the one on schema s (orm.Of[T](s)). It
// registers T (see Register).
func Objects[T any](s ...DBSchema) *Manager[T] {
	Register[T]()
	if len(s) == 0 {
		return home[T]()
	}
	return managerFor[T](s[0])
}

// homes is each model's own manager, by type: the first orm.For[T]()
// without Names, else one Objects made.
var homes sync.Map // reflect.Type → homeEntry

type homeEntry struct {
	m    any // *Manager[T]
	made bool
}

// home is T's own manager.
func home[T any]() *Manager[T] {
	t := reflect.TypeFor[T]()
	if v, ok := homes.Load(t); ok {
		return v.(homeEntry).m.(*Manager[T])
	}
	// Bind the home manager under the primary names set a model-level
	// attribute declares, as For[T]() does.
	m := newManager[T](forConfig{names: defaultNames(t)})
	v, _ := homes.LoadOrStore(t, homeEntry{m: m, made: true})
	return v.(homeEntry).m.(*Manager[T])
}

// managerFor is T's manager on schema s: its own when s is its own
// manager's, else orm.Of[T](s).
func managerFor[T any](s DBSchema) *Manager[T] {
	if h := home[T](); h.c.schema() == s {
		return h
	}
	return Of[T](s)
}

// registry is the models Register added: Check checks their own managers,
// and the migration planner reads their tables.
var registry struct {
	mu    sync.Mutex
	types map[reflect.Type]bool
	homes []func() AnyManager
	done  sync.Map // reflect.Type → true: types, read without the lock
}

// Register adds the model T to the program's models: Check reports when
// the ORM can't map it, and orm makemigrations plans its table without a
// package-level orm.For. orm generate writes a Register for every type
// embedding orm.Model in the module's packages (the orm command overlays
// them when it builds the program as its migration tool); Objects
// registers too.
func Register[T any]() {
	register(reflect.TypeFor[T](), func() AnyManager { return home[T]() })
}

func register(t reflect.Type, own func() AnyManager) {
	// Objects registers on every call: a registered model takes no lock.
	if _, ok := registry.done.Load(t); ok {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.types[t] {
		return
	}
	if registry.types == nil {
		registry.types = map[reflect.Type]bool{}
	}
	registry.types[t] = true
	registry.homes = append(registry.homes, own)
	registry.done.Store(t, true)
}

func registered() []AnyManager {
	registry.mu.Lock()
	fns := slices.Clone(registry.homes)
	registry.mu.Unlock()
	out := make([]AnyManager, len(fns))
	for i, f := range fns {
		out[i] = f()
	}
	return out
}

// modelState is where t embeds Model: its index and offset, or nil for a
// model that doesn't. A Model embedded through a pointer, twice, or of
// another type is an error: its methods couldn't find the row.
func modelState(t reflect.Type) ([]int, uintptr, error) {
	type found struct {
		index []int
		off   uintptr
		ptr   bool
		of    reflect.Type
	}
	var all []found
	var walk func(t reflect.Type, index []int, off uintptr, ptr bool)
	walk = func(t reflect.Type, index []int, off uintptr, ptr bool) {
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.Anonymous {
				continue
			}
			ft, isPtr := sf.Type, false
			if ft.Kind() == reflect.Pointer {
				ft, isPtr = ft.Elem(), true
			}
			if ft.Kind() != reflect.Struct {
				continue
			}
			at := append(slices.Clip(index), i)
			if ft.PkgPath() == modelPkg && (strings.HasPrefix(ft.Name(), "Model[") || strings.HasPrefix(ft.Name(), "Base[")) {
				all = append(all, found{at, off + sf.Offset, ptr || isPtr, reflect.New(ft).Interface().(interface{ of() reflect.Type }).of()})
				continue
			}
			walk(ft, at, off+sf.Offset, ptr || isPtr)
		}
	}
	walk(t, nil, 0, false)
	switch {
	case len(all) == 0:
		return nil, 0, nil
	case len(all) > 1:
		return nil, 0, fmt.Errorf("orm: %s embeds orm.Model %d times: embed orm.Model[%s] once", t.Name(), len(all), t.Name())
	case all[0].ptr:
		return nil, 0, fmt.Errorf("orm: %s embeds orm.Model through a pointer: embed orm.Model[%s] by value", t.Name(), t.Name())
	case all[0].of != t:
		return nil, 0, fmt.Errorf("orm: %s embeds orm.Model[%s]: a model embeds orm.Model of its own type, orm.Model[%s]", t.Name(), all[0].of.Name(), t.Name())
	}
	return all[0].index, all[0].off, nil
}

func (*Model[T]) of() reflect.Type { return reflect.TypeFor[T]() }

var modelPkg = reflect.TypeFor[Model[struct{}]]().PkgPath()

// adopt marks a row of m read under schema s (an addressable struct) as
// loaded from it.
func (m *model) adopt(row reflect.Value, s DBSchema) {
	if m.state != nil {
		fieldOf(row, m.state).Addr().Interface().(stater).adopt(s)
	}
	if m.binds != nil {
		m.bind(row, s)
	}
}

// rowState is a row's Model without its Go type: a Model[T]'s memory.
type rowState struct {
	m      unsafe.Pointer // *Manager[T]
	loaded bool
}

var _ = [1]struct{}{}[unsafe.Sizeof(rowState{})-unsafe.Sizeof(Model[struct{}]{})]

// stateOf is the Model of the row at p, for a model that embeds one.
func (m *managerCore) stateOf(p unsafe.Pointer) *rowState {
	return (*rowState)(unsafe.Add(p, m.meta.stateOff))
}

// adopt marks row, a *T of a model embedding Model, loaded through m.
func (m *managerCore) adopt(row any) { m.adoptAt(reflect.ValueOf(row).UnsafePointer()) }

// adoptAt is adopt of the row at p.
func (m *managerCore) adoptAt(p unsafe.Pointer) {
	if m.meta.state != nil {
		s := m.stateOf(p)
		s.m, s.loaded = m.self, true
	}
	if m.meta.binds != nil {
		m.meta.bind(reflect.NewAt(m.rowType, p).Elem(), m.schema())
	}
}

// copyRow is a copy of the row a *T points to, as a *T.
func copyRow(row any) any {
	v := reflect.ValueOf(row).Elem()
	cp := reflect.New(v.Type())
	cp.Elem().Set(v)
	return cp.Interface()
}
