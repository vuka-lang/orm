package orm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Manager is a model's entry point, Django's Model.objects: a QuerySet of
// every row (its methods are the QuerySet's) and the writes of single
// rows. It queries the database its model is on (On, Meta.DB; the default
// otherwise) in the context's Databases.
//
//	var Users = orm.For[User]()
//
//	orm.AddDatabase("main", db, orm.AsDefault())
//	u, err := Users.Get(ctx, orm.Q{"email": e})
type Manager[T any] struct {
	QuerySet[T]

	meta        *model
	err         error // what is wrong with the model: every query fails with it, Check reports it
	dbName      string
	names       string
	mirror      string
	mirrorNames string // the names set the mirror binds through (MirrorUnder)
	mirrorTable string // the mirror's table, over the model's (MirrorTable)
	unmanaged   bool
	synced      sync.Map // *sql.DB → true: its key sequence is past the table's keys
	mirrorMgrs  sync.Map // "names\x00table" → *Manager[T]: the mirror's manager, built once
	ls          listeners[T]
}

// ForOption tunes a model.
type ForOption func(*forConfig)

type forConfig struct {
	db, table, mirror, names string
	mirrorNames, mirrorTable string
	unmanaged                bool
}

// On puts the model on the database added as name; the default database
// otherwise.
func On(name string) ForOption { return func(c *forConfig) { c.db = name } }

// Table names the model's table, over TableName and the plural of the
// type's name.
func Table(name string) ForOption { return func(c *forConfig) { c.table = name } }

// Unmanaged marks the model's table as the database's own: never created
// or migrated by the ORM. Every legacy table is one.
func Unmanaged() ForOption { return func(c *forConfig) { c.unmanaged = true } }

// Names reads the model under a names set (see DBSchema): each field's tag
// of that key over its default naming, and <Names>TableName (names
// "legacy": LegacyTableName) over TableName.
func Names(set string) ForOption { return func(c *forConfig) { c.names = set } }

// For is the manager of the model T. Declare it once, package-level; a
// model the ORM can't map fails every query, and Check (run it at start
// up) reports it. Its options win over the model's Meta. The first For of
// a model without Names is its own manager, the one Objects and its rows'
// writes use.
func For[T any](opts ...ForOption) *Manager[T] {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	// With no explicit Names option this is the model's home manager: bind
	// it under the primary names set a model-level attribute declares, if
	// any (the live/legacy schema), so For[T]() reads and writes that set's
	// columns. An explicit Names option (home false) wins.
	home := c.names == ""
	if home {
		c.names = defaultNames(reflect.TypeFor[T]())
	}
	m := newManager[T](c)
	if home {
		t := reflect.TypeFor[T]()
		// A For replaces the manager Objects made before it ran (Objects
		// called during another package's init).
		if v, loaded := homes.LoadOrStore(t, homeEntry{m: m}); loaded && v.(homeEntry).made {
			homes.CompareAndSwap(t, v, homeEntry{m: m})
		}
		// A model registers as orm generate's init would register it.
		if m.meta != nil && m.meta.state != nil {
			Register[T]()
		}
	}
	return m
}

func newManager[T any](c forConfig) *Manager[T] {
	m := &Manager[T]{names: c.names}
	m.meta, m.err = modelOf(reflect.TypeFor[T](), c.names, c.table)
	var meta Meta
	var modelMirrorNames, modelMirrorTable string
	if m.meta != nil {
		meta = m.meta.meta
		modelMirrorNames, modelMirrorTable = m.meta.mirrorNames, m.meta.mirrorTable
	}
	// For's options win over the model's declared routing and mirror (its
	// Meta and ModelAttr), which an empty option never clobbers. meta.DB,
	// meta.Mirror and meta.Unmanaged already fold the ModelAttr in; the
	// mirror's names set and table come from the model (Meta has no fields
	// for them).
	m.mirror = cmp.Or(c.mirror, meta.Mirror)
	m.mirrorNames = cmp.Or(c.mirrorNames, modelMirrorNames)
	m.mirrorTable = cmp.Or(c.mirrorTable, modelMirrorTable)
	m.dbName, m.unmanaged = cmp.Or(c.db, meta.DB), c.unmanaged || meta.Unmanaged
	m.QuerySet = QuerySet[T]{m: m, q: &query{m: m.meta}}
	return m
}

// Err is what keeps the ORM from mapping the model: nil when it can.
func (m *Manager[T]) Err() error { return m.err }

// Manager is m itself, as Query's Manager was: Post.Objects.Manager() in
// code written when Post.Objects was a Query.
func (m *Manager[T]) Manager() *Manager[T] { return m }

func (m *Manager[T]) declaration() declaredModel {
	return declaredModel{model: m.meta == nil || m.meta.state != nil, db: m.dbName, unmanaged: m.unmanaged, tables: m.tables, madeOn: m.madeOn, info: m.Info}
}

// schema is the schema the manager queries.
func (m *Manager[T]) schema() DBSchema {
	return DBSchema{DB: m.dbName, Names: m.names, Unmanaged: m.unmanaged}
}

// Info is the model as routers see it: its Meta, with For's On and
// Mirror over it.
func (m *Manager[T]) Info() ModelInfo {
	mi := ModelInfo{Type: reflect.TypeFor[T](), Name: reflect.TypeFor[T]().Name()}
	if m.meta != nil {
		mi.Table, mi.Meta = m.meta.Table, m.meta.meta
	}
	mi.Meta.DB, mi.Meta.Mirror = m.dbName, cmp.Or(m.mirror, mi.Meta.Mirror)
	return mi
}

// writeDB is the database the model's writes go to in s, outside any
// request: its migrations' database.
func (m *Manager[T]) writeDB(s *Databases) string {
	return s.dbFor(context.Background(), m.Info(), true)
}

// DBSchema is a database the models are queried on and the names they have
// there: one set of Go models, relations declared on them, over schemas
// that name their tables and columns apart.
//
//	var Legacy = orm.DBSchema{DB: "legacy", Names: "legacy", Unmanaged: true}
//
//	type User struct {
//		ID      int64
//		Phone   string   `legacy:"tel_no"`              // the column there
//		Bio     string   `legacy:"-"`                   // no such column there
//		Code    string   `gorm:"-" legacy:"code"`       // there only
//		City    string   `legacy:"profile__city"`       // read through a relation there
//		Profile *Profile `gorm:"foreignKey:UserID"`
//		Groups  []Group  `gorm:"many2many:user_groups" legacy:"many2many:auth_user_groups;joinReferences:group_id"`
//	}
//	func (User) LegacyTableName() string { return "auth_user" }
//
//	users, err := orm.Of[User](Legacy).Filter(orm.Q{"groups__name": "staff"}).All(ctx)
//
// Queries name Go fields, default names and relation paths whatever the
// schema; the ORM writes them in the schema's names. A field read through
// a relation is read-only there: Create and Save skip it, and Update
// refuses it.
type DBSchema struct {
	DB        string // the database added under that name; "" the default
	Names     string // the names set; "" the default naming
	Unmanaged bool   // the tables are the database's own: never migrated
}

type schemaKey struct {
	t reflect.Type
	s DBSchema
}

var schemaManagers sync.Map // schemaKey → *Manager[T]

// Of is the manager of the model T on schema s, made once per model and
// schema. It finds its database as For's managers do: in the context's
// Databases.
func Of[T any](s DBSchema) *Manager[T] {
	k := schemaKey{reflect.TypeFor[T](), s}
	if v, ok := schemaManagers.Load(k); ok {
		return v.(*Manager[T])
	}
	c := forConfig{db: s.DB, names: s.Names, unmanaged: s.Unmanaged}
	v, _ := schemaManagers.LoadOrStore(k, newManager[T](c))
	return v.(*Manager[T])
}

// Verify checks that the models (values or pointers of their types) map
// onto the schema — no tag naming no field, relation to a model by no
// field of it, column read through no relation, or two relations claiming
// one inverse name — and, when ctx has its database, once it answers
// (waiting for it as Migrate does), that the database can make their columns, indexes and
// generated columns (a vector on MySQL can't be) and has their
// extensions. Run it at start up, after Migrate, which may install what
// it checks for.
//
//	if err := Legacy.Verify(ctx, User{}, Profile{}, Group{}); err != nil {
//		log.Fatal(err)
//	}
func (s DBSchema) Verify(ctx context.Context, models ...any) error {
	var errs []error
	var ms []*model
	for _, v := range models {
		t := reflect.TypeOf(v)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		m, err := modelOf(t, s.Names, "")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ms = append(ms, m)
		errs = append(errs, m.check()...)
	}
	d, err := waitConnected(ctx, databasesOf(ctx), s.DB, MigrateWait)
	switch {
	case errors.Is(err, ErrUnavailable):
		errs = append(errs, err)
	case err == nil:
		var exts []string
		for _, m := range ms {
			if err := m.madeOn(d.dialect); err != nil {
				errs = append(errs, err)
			}
			exts = append(exts, m.extensions()...)
		}
		slices.Sort(exts)
		errs = append(errs, missingExtensions(ctx, d, slices.Compact(exts)))
	}
	return errors.Join(errs...)
}

// waitConnected is the database name in s once it answers a ping, tried
// until wait passes: a database starting beside the program (a container)
// takes a moment. Unavailable when it never answers.
func waitConnected(ctx context.Context, s *Databases, name string, wait time.Duration) (*DB, error) {
	if d, ok := ctx.Value(dbKey{}).(*DB); ok {
		return d, nil
	}
	d, err := s.Lookup(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		perr := d.sql.PingContext(ctx)
		if perr == nil {
			return d, nil
		}
		if time.Now().After(deadline) {
			return nil, &Error{Kind: Unavailable, Message: fmt.Sprintf("orm: %s is not connected: %v", dbLabel(name), perr), Err: perr}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TableName is the model's table.
func (m *Manager[T]) TableName() string {
	if m.meta == nil {
		return ""
	}
	return m.meta.Table
}

// ColumnValues is row's columns and their values as the database is sent
// them (a nil pointer as nil): a snapshot of the row for code that speaks
// columns, such as a change feed to another system.
func (m *Manager[T]) ColumnValues(row *T) map[string]any {
	if m.meta == nil || row == nil {
		return nil
	}
	v := reflect.ValueOf(row).Elem()
	out := make(map[string]any, len(m.meta.Fields))
	for _, f := range m.meta.Fields {
		out[f.Column] = value(peek(v, f.Index, f.Type))
	}
	return out
}

// PKColumn is the column of the model's primary key, "" for none.
func (m *Manager[T]) PKColumn() string {
	if m.meta == nil || m.meta.PK == nil {
		return ""
	}
	return m.meta.PK.Column
}

// Columns is the model's columns, in field order.
func (m *Manager[T]) Columns() []string {
	if m.meta == nil {
		return nil
	}
	return m.meta.columns()
}

// conn is the database ctx's reads (or writes) of the model go to.
func (m *Manager[T]) conn(ctx context.Context, write bool) (conn, error) {
	if m.err != nil {
		return conn{}, m.err
	}
	if t, ok := ctx.Value(inMirrorKey{}).(mirrorTarget); ok {
		return m.mirrorConn(ctx, t)
	}
	mi := m.Info()
	return resolve(ctx, databasesOf(ctx), &mi, "", write)
}

// dbConn is the database name as ctx sees it, for raw SQL: WithDB's,
// Using's, or the one of ctx's Databases.
func dbConn(ctx context.Context, name string, write bool) (conn, error) {
	return resolve(ctx, databasesOf(ctx), nil, name, write)
}
