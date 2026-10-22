// Package orm is a Django-style ORM for Go: a model is a plain struct, its
// manager a package-level value, and its queries lazy, immutable
// QuerySets read with Django's lookups.
//
//	var Users = orm.For[User]()
//
//	adults, err := Users.Filter(orm.Q{"age__gte": 18}).OrderBy("-age").All(ctx)
//	u, err := Users.Get(ctx, orm.Q{"email": e})
//
// Connections are database/sql's: a model reads and writes through the
// database added under its name (orm.AddDatabase; the default unless the
// model names another), or the one a context carries (orm.WithDB).
package orm

import (
	"cmp"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/orm/v2/internal/tags"
)

// field is a model's column: where it sits in the struct and how it is
// written.
type field struct {
	Name       string // Go name
	Column     string
	Index      []int
	Type       reflect.Type
	PK         bool
	AutoNowAdd bool
	AutoNow    bool
	Unique     bool
	UniqueIdx  string // a named unique index: unique together with the fields sharing it
	Indexed    bool
	Size       int
	SQLType    string
	// Via is the lookup path of a field read through a relation
	// (profile__first_name): it has no column, and is never written.
	Via string
	// Gen is a generated column's expression, from the model's
	// Generated(): the database computes it, the ORM never writes it.
	Gen  Expr
	gen  bool // tagged orm:"generated"
	Dims int  // a vector's dimensions
	// Default is the column's SQL default, from SetFieldOptions.
	Default string
	Null    bool // NULL allowed though the Go type has no nil
	fk      bool // an orm.FK: the column of the relation of the same name
	fast    scanKind
	spec    *fieldSpec // what the field's options add: its checks, conversions, column details
	conv    conv       // how a value is changed on its way to the column
	// codec adapts the field's value to and from the database under this
	// model's names set, set from a SchemaField for the set; nil for none.
	// It wins over the field type's own per-schema conversion.
	codec SchemaCodec
}

// model is what a Go struct means to the database under a names set,
// computed once per type and set.
type model struct {
	Type  reflect.Type
	Name  string // the Go type's name
	Table string
	// Names is the names set the model is read under: each field's tag of
	// that key over its default naming. "" is the default.
	Names  string
	Fields []*field
	via    []*field // the fields read through relations
	PK     *field
	byName map[string]*field // Go name and default column, lowercased
	byCol  map[string]*field // column under Names, lowercased
	// computed is the fields tagged orm:"computed", by Go name lowercased
	// and snake_case: no column, filled from the annotation of that name.
	computed map[string]*field
	// rels is the relation fields, by Go name lowercased and snake_case.
	rels    map[string]*relation
	relList []*relation
	binds   []*relation // the orm.M2M fields: told their row's key as it is read or saved
	attrIdx []Index     // the indexes field attributes declare
	indexes []Index     // Meta's Indexes, else Indexes()
	meta    Meta
	// mirrorNames and mirrorTable are the model-level mirror binding a
	// ModelAttr declares (the names set and table MirrorUnder/MirrorTable
	// name): Meta has no fields for them, so they live here. For's
	// MirrorUnder/MirrorTable win over them in the manager.
	mirrorNames string
	mirrorTable string
	// fields is the field options its model attributes decide as it is
	// mapped (ModelConfig.Fields).
	fields FieldOptionsFunc
	// state is the index of the embedded Model, nil for none, and
	// stateOff its offset in the struct.
	state    []int
	stateOff uintptr

	// The inverses other models' relations imply on this one, as of a
	// version of declarers; see inverses.
	invMu sync.Mutex
	inv   atomic.Pointer[inverseSet]

	quoted  [3]atomic.Pointer[quotedNames] // by dialectIndex
	scanner atomic.Pointer[scannerHit]
}

// field is the field a lookup names: by Go name (any case) or default
// column, else by its column under the names set.
func (m *model) field(name string) (*field, bool) {
	k := strings.ToLower(name)
	if f, ok := m.byName[k]; ok {
		return f, true
	}
	f, ok := m.byCol[k]
	return f, ok
}

// quotedNames is a model's table and columns as one dialect quotes them,
// written once: what nearly every statement on the model is made of.
type quotedNames struct {
	table string
	col   map[*field]string // qualified by the table, as builder.col writes it
	bare  map[*field]string
	list  string // the columns of readFields, qualified; "" with fields read through relations
}

// dialectIndex is d's slot in a model's quoted names, -1 for none.
func dialectIndex(d Dialect) int {
	switch d.(type) {
	case postgres:
		return 0
	case mysql:
		return 1
	case sqlite:
		return 2
	}
	return -1
}

// names is m's table and columns quoted for d, nil for a dialect it keeps
// none for.
func (m *model) names(d Dialect) *quotedNames {
	i := dialectIndex(d)
	if m == nil || i < 0 {
		return nil
	}
	if n := m.quoted[i].Load(); n != nil {
		return n
	}
	n := &quotedNames{table: d.Quote(m.Table), col: make(map[*field]string, len(m.Fields)), bare: make(map[*field]string, len(m.Fields))}
	cols := make([]string, len(m.Fields))
	for j, f := range m.Fields {
		n.bare[f] = d.Quote(f.Column)
		n.col[f] = n.table + "." + n.bare[f]
		cols[j] = n.col[f]
	}
	if len(m.via) == 0 {
		n.list = strings.Join(cols, ", ")
	}
	m.quoted[i].Store(n)
	return n
}

func (m *model) columns() []string {
	out := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		out[i] = f.Column
	}
	return out
}

// readFields is the fields a row is read into: the columns, then the
// fields read through relations.
func (m *model) readFields() []*field {
	if len(m.via) == 0 {
		return m.Fields
	}
	return append(slices.Clip(m.Fields), m.via...)
}

type modelKey struct {
	t     reflect.Type
	names string
}

var models sync.Map // modelKey → *model, or error

// modelOf is the metadata of t under a names set, computed on first use;
// a table of its own makes one apart.
func modelOf(t reflect.Type, names, table string) (*model, error) {
	if table != "" {
		return buildModel(t, names, table)
	}
	k := modelKey{t, names}
	v, ok := models.Load(k)
	if !ok {
		m, err := buildModel(t, names, "")
		v = m
		if err != nil {
			v = err
		}
		v, _ = models.LoadOrStore(k, v)
	}
	if err, isErr := v.(error); isErr {
		return nil, err
	}
	return v.(*model), nil
}

type tableNamer interface{ TableName() string }

// Meta is what a model declares about itself, Django's class Meta: a model
// attribute, given to s.Model in the model's Schema method.
//
//	func (u *User) Schema(s *orm.Schema) {
//		s.Model(orm.Meta{Table: "accounts", Ordering: []string{"-created_at"}})
//	}
//
// A Meta method, func (User) Meta() orm.Meta, still declares it and wins
// over the Schema's; it is deprecated, for the Schema method.
//
// It wins over the model's TableName and Indexes methods; For's options
// (orm.Table, orm.On, orm.Unmanaged) win over it, and under a names set
// the set's <Names>TableName (LegacyTableName) does.
type Meta struct {
	Table     string   // the table; the plural of the type's name in snake_case otherwise
	DB        string   // the database added under that name, orm.On's; the default otherwise
	Read      string   // the database its reads go to, over DB (a replica's own name, a reporting copy)
	Write     string   // the database its writes go to, over DB
	Mirror    string   // a database its writes are repeated on (see orm.Mirror)
	Unmanaged bool     // the database's own table: never created or migrated, orm.Unmanaged's
	Indexes   []Index  // its indexes, as Indexes() returns them
	Ordering  []string // the order of a query that sets none, as OrderBy takes it
}

var (
	scannerType = reflect.TypeFor[sql.Scanner]()
	valuerType  = reflect.TypeFor[driver.Valuer]()
	timeType    = reflect.TypeFor[time.Time]()
)

func buildModel(t reflect.Type, names, table string) (*model, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: %v is not a struct", t)
	}
	if _, err := SchemaOf(t); err != nil {
		return nil, err
	}
	m := &model{Type: t, Name: t.Name(), Names: names, byName: map[string]*field{}, byCol: map[string]*field{}, computed: map[string]*field{}, rels: map[string]*relation{}}
	var err error
	if m.state, m.stateOff, err = modelState(t); err != nil && table != "-" {
		return nil, err
	}
	v := reflect.New(t).Interface()
	if mt, ok := v.(interface{ Meta() Meta }); ok {
		m.meta = mt.Meta()
	} else if mt, ok := attrMeta(t); ok {
		m.meta = mt
	}
	m.Table = table
	if table == "" {
		m.Table = tags.Plural(tags.Snake(t.Name()))
		if tn, ok := v.(tableNamer); ok {
			m.Table = tn.TableName()
		}
		m.Table = cmp.Or(m.meta.Table, m.Table)
		// Under a names set, <Names>TableName (LegacyTableName) wins.
		if names != "" {
			if mt := reflect.ValueOf(v).MethodByName(strings.ToUpper(names[:1]) + names[1:] + "TableName"); mt.IsValid() {
				if f, ok := mt.Interface().(func() string); ok {
					m.Table = f()
				}
			}
		}
		// A ModelAttr's Table names the table under its own Names set: over
		// everything under a names set, under @orm.Meta's Table without one.
		if c, ok := modelAttrsOf(t); ok && c.Table != "" && c.Names == names && (names != "" || m.meta.Table == "") {
			m.Table = c.Table
		}
	}
	m.applyModelAttrs(t)
	if err := collect(m, t, nil, ""); err != nil {
		return nil, err
	}
	keys := 0
	for _, f := range m.Fields {
		if f.PK {
			keys++
			m.PK = f
		}
	}
	// A composite key (a table between two others) has no one column to
	// find a row by: such a model is filtered, inserted and deleted by
	// QuerySet, never saved or removed as a row.
	if keys > 1 {
		m.PK = nil
	}
	if m.PK == nil && keys == 0 {
		if f, ok := m.byName["id"]; ok && f.Via == "" {
			f.PK = true
			m.PK = f
		}
	}
	if len(m.Fields)+len(m.via) == 0 {
		return nil, fmt.Errorf("orm: %s has no columns", m.Name)
	}
	if err := m.declared(v); err != nil {
		return nil, err
	}
	for _, r := range m.relList {
		if err := r.settle(m); err != nil {
			return nil, err
		}
		if r.holder && r.Kind == relM2M {
			m.binds = append(m.binds, r)
		}
	}
	return m, nil
}

// collect adds t's columns to m: its own fields, and those of the
// structs it embeds, flattened as Go promotes them; an outer field
// shadows an embedded one of the same name.
func collect(m *model, t reflect.Type, index []int, prefix string) error {
	for i := range t.NumField() {
		sf := t.Field(i)
		path := append(append([]int(nil), index...), i)
		attrs := schemaFieldAttrs(m.Type, path)
		def, opts := withOptions(attrs, m.fields, t, sf, tags.Parse(sf.Name, sf.Tag, sf.Type == timeType))
		dflt := opts.Default
		tag := def
		if v, ok := sf.Tag.Lookup(m.Names); ok && m.Names != "" {
			tag = def.Under(v)
		}
		// A SchemaField (from a field attribute or SetFieldOptions) names
		// the column under a names set as the per-set struct tag does, and
		// may carry a value codec for the set.
		var codec SchemaCodec
		if m.Names != "" {
			if sfo, ok := opts.Schemas[m.Names]; ok {
				switch {
				case sfo.Skip:
					tag.Skip = true
				default:
					// An empty Column or Type inherits the field's primary
					// column and type (cmp.Or): a per-set override may retype a
					// column without renaming it, or rename it without retyping.
					if sfo.Column != "" {
						tag.Column, tag.Path = sfo.Column, ""
					}
					tag.Type = cmp.Or(sfo.Type, tag.Type)
					codec = sfo.Codec
				}
			}
		}
		if tag.Skip {
			continue
		}
		ft := sf.Type
		base := ft
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if sf.Anonymous && base.Kind() == reflect.Struct && !isValue(ft) {
			if ft.Kind() == reflect.Pointer && !sf.IsExported() {
				return fmt.Errorf("orm: %s embeds *%s, which can't be allocated while scanning: export it or embed it by value", m.Name, base.Name())
			}
			if err := collect(m, base, path, prefix); err != nil {
				return err
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		if tag.Embedded && base.Kind() == reflect.Struct {
			if err := collect(m, base, path, prefix+tag.Prefix); err != nil {
				return err
			}
			continue
		}
		if target, many, ok := relFieldOf(ft); ok {
			if err := m.addRelField(sf, path, tag, prefix, opts, target, many); err != nil {
				return err
			}
			continue
		}
		if !isValue(ft) {
			if r := relationOf(sf, path, tag); r != nil {
				m.addRelation(r)
			}
			continue // a type the database can't hold
		}
		if tag.Computed {
			f := &field{Name: sf.Name, Column: tags.Snake(sf.Name), Index: path, Type: ft}
			m.computed[strings.ToLower(sf.Name)] = f
			m.computed[f.Column] = f
			continue
		}
		f := &field{Name: sf.Name, Column: column(tag, sf.Name, prefix), Index: path, Type: ft, PK: tag.PK,
			AutoNowAdd: tag.AutoNowAdd, AutoNow: tag.AutoNow, Unique: tag.Unique, UniqueIdx: tag.UniqueIndex, Indexed: tag.Index, Size: tag.Size, SQLType: tag.Type, Via: tag.Path,
			gen: tag.Generated, Dims: tag.Vector, Default: dflt, Null: tag.Null, fast: scanKindOf(ft), codec: codec}
		if err := f.settle(m, opts); err != nil {
			return err
		}
		m.attrIdx = append(m.attrIdx, attrSearch(attrs, f)...)
		key := strings.ToLower(sf.Name)
		if old, ok := m.byName[key]; ok {
			if len(old.Index) <= len(path) {
				continue // shadowed by a shallower field
			}
			m.remove(old)
		}
		m.byName[key] = f
		if !def.Skip && def.Path == "" {
			m.byName[strings.ToLower(column(def, sf.Name, prefix))] = f
		}
		if k := strings.ToLower(prefix + tags.Snake(sf.Name)); m.byName[k] == nil {
			m.byName[k] = f // what typed fields name it by when its column is renamed
		}
		if f.Via != "" {
			f.Column = ""
			m.via = append(m.via, f)
			continue
		}
		m.Fields = append(m.Fields, f)
		m.byCol[strings.ToLower(f.Column)] = f
	}
	return nil
}

// declared reads what the model declares by method: its generated
// columns' expressions (Generated, by Go field name) and its indexes
// (Meta's, else Indexes).
func (m *model) declared(v any) error {
	if g, ok := v.(interface{ Generated() map[string]Expr }); ok {
		for name, e := range g.Generated() {
			f, ok := m.field(name)
			if !ok || !f.gen {
				return fmt.Errorf("orm: %s.Generated() names %s, no field of it tagged orm:\"generated\"", m.Name, name)
			}
			f.Gen = e
		}
	}
	for _, f := range m.Fields {
		if f.gen && f.Gen == nil {
			return fmt.Errorf("orm: %s.%s is generated: give its expression in %s's Generated()", m.Name, f.Name, m.Name)
		}
	}
	if ix, ok := v.(interface{ Indexes() []Index }); ok {
		m.indexes = ix.Indexes()
	}
	if m.meta.Indexes != nil {
		m.indexes = m.meta.Indexes
	}
	m.indexes = append(slices.Clip(m.indexes), m.attrIdx...)
	return nil
}

// column is a field's column by its tags: tagged, else its name in
// snake_case.
func column(t tags.Tags, name, prefix string) string {
	if t.Column != "" {
		return prefix + t.Column
	}
	return prefix + tags.Snake(name)
}

func (m *model) remove(old *field) {
	m.Fields = slices.DeleteFunc(m.Fields, func(f *field) bool { return f == old })
	m.via = slices.DeleteFunc(m.via, func(f *field) bool { return f == old })
	maps.DeleteFunc(m.byName, func(_ string, f *field) bool { return f == old })
	maps.DeleteFunc(m.byCol, func(_ string, f *field) bool { return f == old })
}

// isValue is whether a column can hold t: basic kinds, times, bytes, and
// types that scan or value themselves (database/sql's way or the ORM's).
func isValue(t reflect.Type) bool {
	if t.Implements(scannerType) || reflect.PointerTo(t).Implements(scannerType) || t.Implements(valuerType) || customValue(t) {
		return true
	}
	if t.Kind() == reflect.Pointer {
		return isValue(t.Elem())
	}
	if isOption(t) {
		return isValue(optionElem(t))
	}
	if t == timeType {
		return true
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return false
}

// addRelField adds an orm.FK field (a column, and the foreign key of its
// name) or an orm.M2M field (a many-to-many, through table <table>_<field>
// unless Rel names one).
func (m *model) addRelField(sf reflect.StructField, path []int, tag tags.Tags, prefix string, opts FieldOptions, target reflect.Type, many bool) error {
	r := &relation{Name: sf.Name, Index: path, tag: tag, Target: target, OnDelete: tag.OnDelete, holder: true}
	if many {
		r.Slice = true
		if r.tag.M2M == "" {
			r.tag.M2M = m.Table + "_" + tags.Snake(sf.Name)
		}
		m.addRelation(r)
		return nil
	}
	col := tag.Column
	if col == "" {
		col = tags.Snake(sf.Name) + "_id"
	}
	f := &field{Name: sf.Name, Column: prefix + col, Index: path, Type: sf.Type, Unique: tag.Unique, UniqueIdx: tag.UniqueIndex,
		Indexed: tag.Index, SQLType: tag.Type, Default: opts.Default, Null: tag.Null, fk: true, fast: scanKindOf(sf.Type)}
	if err := f.settle(m, opts); err != nil {
		return err
	}
	m.byName[strings.ToLower(sf.Name)] = f
	m.byName[strings.ToLower(f.Column)] = f
	m.byCol[strings.ToLower(f.Column)] = f
	m.Fields = append(m.Fields, f)
	r.self = f
	m.addRelation(r)
	return nil
}

// bind tells row's orm.M2M fields the row's key, read under schema s.
func (m *model) bind(row reflect.Value, s DBSchema) {
	for _, r := range m.binds {
		fieldOf(row, r.Index).Addr().Interface().(binder).bind(r, key(row, r.local), s)
	}
}
