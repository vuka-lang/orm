package orm

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vuka-lang/orm/internal/tags"
)

type relKind int

const (
	relFK       relKind = iota + 1 // this row holds the related row's key
	relRev                         // the related rows hold this row's key
	relM2M                         // a table between holds both keys
	relElements                    // this row's JSON array or comma-separated column holds their keys
)

// relation is a field holding related rows: a pointer or struct for a
// foreign key, a slice for reverse foreign keys and many-to-many. An
// inverse (see inverses) is one too, held by a field of its name or none.
type relation struct {
	Name   string // Go name; an inverse's name
	Index  []int  // nil for an inverse no field holds
	Kind   relKind
	Target reflect.Type // the related model's struct type
	Ptr    bool         // the field (or the slice's elements) is a pointer
	Slice  bool
	// Single is a reverse foreign key holding one row (GORM's has-one, a
	// unique foreign key's inverse): the related model holds this row's
	// key, at most once.
	Single   bool
	OnDelete string // a foreign key's: cascade, set_null, restrict
	// holder is a relation an orm.FK or orm.M2M field holds: its rows are
	// handed to the field (hold) rather than set; self is an FK's column.
	holder bool
	self   *field

	tag   tags.Tags
	owner *model
	// local is the owner's field the relation links on: a foreign key's
	// column, else the key the related rows (or the table between) hold.
	local *field
	// remote names the related model's field the relation links on: the
	// key a foreign key or a many-to-many refers to ("" its primary key),
	// the column of rows held by many holding local ("" <Model>ID).
	remote string
	// M2M: the table between, its column for this row's key and for the
	// related row's.
	Through, ThroughLocal, ThroughRemote string
	of                                   *relation // the relation an inverse is the other side of

	once sync.Once
	t    *model
	rf   *field
	err  error
}

// relationOf is the relation a field declares, or nil: a struct, a
// pointer to one or a slice of either, of a type that isn't a column.
func relationOf(sf reflect.StructField, index []int, t tags.Tags) *relation {
	if !sf.IsExported() {
		return nil
	}
	ft := sf.Type
	r := &relation{Name: sf.Name, Index: index, tag: t, OnDelete: t.OnDelete}
	if ft.Kind() == reflect.Slice {
		r.Slice, ft = true, ft.Elem()
	}
	if ft.Kind() == reflect.Pointer {
		r.Ptr, ft = true, ft.Elem()
	}
	if ft.Kind() != reflect.Struct || ft == timeType {
		return nil
	}
	r.Target = ft
	return r
}

func (m *model) addRelation(r *relation) {
	r.owner = m
	m.relList = append(m.relList, r)
	m.rels[strings.ToLower(r.Name)] = r
	m.rels[tags.Snake(r.Name)] = r
	if m.Table != "-" {
		declareRelation(r.Target, m.Type)
	}
}

// relation is the relation a lookup step names, by Go field name (any
// case) or snake_case: a relation of the model's own, else an inverse.
func (m *model) relation(name string) (*relation, bool) {
	r, ok := m.rels[strings.ToLower(name)]
	if !ok {
		r, ok = m.rels[name]
	}
	if ok && r.Kind != 0 {
		if _, _, err := r.ends(); err == nil {
			return r, true
		}
	}
	inv, _ := m.inverses()
	if x, found := inv[strings.ToLower(name)]; found {
		return x, true
	}
	return r, ok && r.Kind != 0
}

// settle decides what a relation is from its tags, else by convention:
// Author *User with an AuthorID field is a foreign key; Posts []Post a
// reverse foreign key on Post's <Model>ID. A field that is neither stays
// out of the model, as before. GORM's foreignKey and references name Go
// fields: on a foreign key the field holding the key and the related
// model's field it refers to, on rows held by many (or a has-one) their
// field holding the key and this model's field it refers to.
func (r *relation) settle(m *model) error {
	t := r.tag
	if r.self != nil {
		r.Kind, r.local, r.remote = relFK, r.self, t.References
		return nil
	}
	key := func() error {
		r.local = m.PK
		if t.References != "" {
			f, ok := m.field(t.References)
			if !ok {
				return fmt.Errorf("orm: %s.%s: references %s, no field of %s", m.Name, r.Name, t.References, m.Name)
			}
			r.local = f
		}
		if r.local == nil {
			return fmt.Errorf("orm: %s.%s: %s has no primary key for the related rows to hold", m.Name, r.Name, m.Name)
		}
		return nil
	}
	switch {
	case t.Elements != "":
		r.Kind = relElements
		col, ref, _ := strings.Cut(t.Elements, ",")
		f, ok := m.field(strings.TrimSpace(col))
		if !ok {
			return fmt.Errorf("orm: %s.%s: elements: %s has no column %q", m.Name, r.Name, m.Name, col)
		}
		r.local, r.remote = f, strings.TrimSpace(ref)
		if !r.Slice {
			return fmt.Errorf("orm: %s.%s holds elements but is not a slice", m.Name, r.Name)
		}
	case t.M2M != "":
		r.Kind = relM2M
		r.Through = t.M2M
		r.ThroughLocal = cmp.Or(t.JoinForeignKey, tags.Snake(m.Name)+"_id")
		r.ThroughRemote = cmp.Or(t.JoinReferences, tags.Snake(r.Target.Name())+"_id")
		if !r.Slice {
			return fmt.Errorf("orm: %s.%s is many-to-many but not a slice", m.Name, r.Name)
		}
		r.local, r.remote = m.PK, t.References
		if t.GormForeignKey != "" {
			f, ok := m.field(t.GormForeignKey)
			if !ok {
				return fmt.Errorf("orm: %s.%s: foreignKey %s is no field of %s", m.Name, r.Name, t.GormForeignKey, m.Name)
			}
			r.local = f
		}
		if r.local == nil {
			return fmt.Errorf("orm: %s.%s: many-to-many needs %s's primary key", m.Name, r.Name, m.Name)
		}
	case t.FK != "" || (t.GormForeignKey != "" && !r.Slice):
		r.Kind = relFK
		f, ok := m.field(cmp.Or(t.FK, t.GormForeignKey))
		switch {
		case !ok && t.FK != "":
			return fmt.Errorf("orm: %s.%s: %s has no column %q", m.Name, r.Name, m.Name, t.FK)
		case !ok:
			// GORM's has-one: the key is on the related model.
			r.Kind, r.Single, r.remote = relRev, true, t.GormForeignKey
			return key()
		}
		r.local, r.remote = f, t.References
		if r.Slice {
			return fmt.Errorf("orm: %s.%s is a foreign key but a slice", m.Name, r.Name)
		}
	case t.Rel != "" || (t.GormForeignKey != "" && r.Slice):
		r.Kind = relRev
		r.remote = cmp.Or(t.Rel, t.GormForeignKey) // checked against the related model when it is used
		if !r.Slice {
			return fmt.Errorf("orm: %s.%s is a reverse foreign key but not a slice", m.Name, r.Name)
		}
		return key()
	case !r.Slice:
		if f, ok := m.field(r.Name + "ID"); ok {
			r.Kind, r.local = relFK, f
		}
	default:
		if m.PK != nil {
			r.Kind, r.local = relRev, m.PK // its column is found on the related model when used
		}
	}
	return nil
}

// ends is the related model and its field the relation links on (see
// remote), found once.
func (r *relation) ends() (*model, *field, error) {
	r.once.Do(func() { r.t, r.rf, r.err = r.resolve() })
	return r.t, r.rf, r.err
}

func (r *relation) resolve() (*model, *field, error) {
	m := r.owner
	t, err := modelOf(r.Target, m.Names, "")
	if err != nil {
		return nil, nil, err
	}
	if t.PK == nil {
		return nil, nil, fmt.Errorf("orm: %s has no primary key to relate by", t.Name)
	}
	switch {
	case r.remote != "":
		if f, ok := t.field(r.remote); ok {
			return t, f, nil
		}
		return nil, nil, fmt.Errorf("orm: %s.%s: %s has no field %q", m.Name, r.Name, t.Name, r.remote)
	case r.Kind == relRev:
		if f, ok := t.field(m.Name + "ID"); ok {
			return t, f, nil
		}
		return nil, nil, fmt.Errorf("orm: %s.%s: %s has no column holding %s's key (tag it orm:\"rel:<column>\")", m.Name, r.Name, t.Name, m.Name)
	}
	return t, t.PK, nil
}

// jsonLocal is whether an elements relation's column holds a JSON array
// rather than comma-separated text: an orm.JSON field, or a column whose
// declared type says JSON — per names set, so one field can be jsonb on
// one schema (type:jsonb) and text on another (legacy:"type:varchar(…)").
func (r *relation) jsonLocal() bool {
	t := r.local.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Implements(jsonColType) || strings.Contains(strings.ToLower(r.local.SQLType), "json")
}

// one is whether the relation holds one row: a foreign key, or a has-one.
func (r *relation) one() bool { return r.Kind == relFK || r.Single }

// held is r's error when no field holds its rows: an inverse the model
// declares no field of its name for.
func (r *relation) held(m *model) error {
	if r.Index != nil {
		return nil
	}
	return fmt.Errorf("orm: %s has no field to hold %s, the inverse of %s.%s: declare one of that name", m.Name, r.Name, r.of.owner.Name, r.of.Name)
}

// declarers is, by model type, the types with relation fields to it:
// the models whose relations imply its inverses.
var declarers struct {
	sync.Mutex
	by  map[reflect.Type][]reflect.Type
	ver atomic.Int64 // changed under the lock, read without it
}

func declareRelation(target, from reflect.Type) {
	declarers.Lock()
	defer declarers.Unlock()
	if declarers.by == nil {
		declarers.by = map[reflect.Type][]reflect.Type{}
	}
	if !slices.Contains(declarers.by[target], from) {
		declarers.by[target] = append(declarers.by[target], from)
		declarers.ver.Add(1)
	}
}

// inverses is the relations other models' foreign keys and many-to-manys
// imply on m, Django's reverse relations, by name: rows held by many (one
// for a unique foreign key), and many-to-manys back; and the names more
// than one claims, with what claims them. An inverse m declares itself
// is m's own relation, not a second one.
func (m *model) inverses() (map[string]*relation, map[string][]string) {
	// Every lookup step naming no relation of m's own asks: read without a
	// lock while no relation was declared since.
	if s := m.inv.Load(); s != nil && s.ver == declarers.ver.Load() {
		return s.inv, s.clash
	}
	declarers.Lock()
	from, ver := slices.Clone(declarers.by[m.Type]), declarers.ver.Load()
	declarers.Unlock()
	m.invMu.Lock()
	defer m.invMu.Unlock()
	if s := m.inv.Load(); s != nil && s.ver == ver {
		return s.inv, s.clash
	}
	found := map[string][]*relation{}
	for _, d := range from {
		dm, err := modelOf(d, m.Names, "")
		if err != nil {
			continue
		}
		for _, r := range dm.relList {
			if r.Target != m.Type || (r.Kind != relFK && r.Kind != relM2M) {
				continue
			}
			if x, ok := r.inverse(m); ok && !m.declares(x) {
				k := strings.ToLower(x.Name)
				found[k] = append(found[k], x)
			}
		}
	}
	inv, clash := map[string]*relation{}, map[string][]string{}
	for k, xs := range found {
		var claims []string
		if e, ok := m.rels[k]; ok && xs[0].Index == nil && e.Kind != 0 {
			if _, _, err := e.ends(); err == nil {
				claims = append(claims, "the field "+m.Name+"."+e.Name)
			}
		}
		if len(xs) == 1 && claims == nil {
			inv[k] = xs[0]
			continue
		}
		for _, x := range xs {
			claims = append(claims, x.of.owner.Name+"."+x.of.Name)
		}
		clash[k] = claims
	}
	m.inv.Store(&inverseSet{ver: ver, inv: inv, clash: clash})
	return inv, clash
}

// inverseSet is a model's inverses as of declarers' version ver.
type inverseSet struct {
	ver   int64
	inv   map[string]*relation
	clash map[string][]string
}

// inverseName is the name of a foreign key's or many-to-many's inverse:
// tagged related:, else the declaring model's in snake_case, plural
// unless the foreign key is unique.
func (r *relation) inverseName() string {
	name := tags.Plural(tags.Snake(r.owner.Name))
	if r.oneToOne() {
		name = tags.Snake(r.owner.Name)
	}
	return cmp.Or(r.tag.Related, name)
}

// oneToOne is whether r is a foreign key whose column is unique by itself:
// its inverse holds one row.
func (r *relation) oneToOne() bool {
	if r.Kind != relFK || !r.local.Unique {
		return false
	}
	return r.local.UniqueIdx == "" || !slices.ContainsFunc(r.owner.Fields, func(f *field) bool {
		return f != r.local && f.UniqueIdx == r.local.UniqueIdx
	})
}

// inverse is r seen from its related model m: rows of r's model holding
// m's key (one, for a unique foreign key), or a many-to-many back. A
// field of m of its name that is no relation of its own holds it.
func (r *relation) inverse(m *model) (*relation, bool) {
	_, rf, err := r.ends()
	if err != nil {
		return nil, false
	}
	local, _ := m.field(rf.Name)
	x := &relation{Name: r.inverseName(), Kind: relRev, Target: r.owner.Type, Slice: true, owner: m, local: local, of: r,
		Through: r.Through, ThroughLocal: r.ThroughRemote, ThroughRemote: r.ThroughLocal}
	if r.Kind == relM2M {
		x.Kind = relM2M
	} else if r.oneToOne() {
		x.Single, x.Slice = true, false
	}
	x.once.Do(func() { x.t, x.rf = r.owner, r.local })
	if e, ok := m.rels[strings.ToLower(x.Name)]; ok && e.Target == x.Target && e.Slice == x.Slice {
		if _, _, err := e.ends(); e.Kind == 0 || err != nil {
			x.Index, x.Ptr = e.Index, e.Ptr
		}
	}
	return x, true
}

// declares is whether m declares x itself: a relation of its own to the
// same rows by the same columns.
func (m *model) declares(x *relation) bool {
	for _, e := range m.relList {
		if e.Target != x.Target || e.Kind != x.Kind {
			continue
		}
		if x.Kind == relM2M {
			if e.Through == x.Through && e.ThroughLocal == x.ThroughLocal && e.ThroughRemote == x.ThroughRemote {
				return true
			}
			continue
		}
		if _, rf, err := e.ends(); err == nil && rf.Column == x.rf.Column && e.local.Column == x.local.Column {
			return true
		}
	}
	return false
}

// check is what keeps m from mapping onto its names set: relations to no
// model or by no field, fields read through no relation, inverse names
// claimed twice.
func (m *model) check() []error {
	var errs []error
	for _, r := range m.relList {
		if got, _ := m.relation(r.Name); r.Kind == 0 || got != r {
			continue
		}
		t, _, err := r.ends()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if r.Kind == relRev {
			continue
		}
		name := r.inverseName()
		if _, clash := t.inverses(); slices.Contains(clash[strings.ToLower(name)], m.Name+"."+r.Name) {
			errs = append(errs, fmt.Errorf("orm: %s's inverse %q is claimed by %s: give each its own orm:\"related:<name>\"", t.Name, name, strings.Join(clash[strings.ToLower(name)], " and ")))
		}
	}
	b := newBuilder(DialectFor(""), m)
	for _, f := range m.via {
		if _, err := b.ref(f.Via); err != nil {
			errs = append(errs, fmt.Errorf("orm: %s.%s is read through %s: %w", m.Name, f.Name, f.Via, err))
		}
	}
	return errs
}
