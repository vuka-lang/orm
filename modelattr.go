package orm

import (
	"cmp"
	"reflect"
)

// ModelConfig is the model-level configuration a ModelAttr sets: the same
// routing and mirror an @orm.Meta and For's options set, in one value. A
// zero member leaves the model's setting; a set one is folded in. Later
// attributes' set members win over earlier ones, and an empty member never
// undoes one already set (cmp.Or-style), as field attributes merge.
type ModelConfig struct {
	DB          string // the model's primary database (On / Meta.DB); the default otherwise
	Names       string // the names set the model's OWN (primary) manager binds under
	Table       string // the model's table under Names (see ModelAttr)
	Read        string // the database its reads go to, over DB (Meta.Read; a replica)
	Write       string // the database its writes go to, over DB (Meta.Write)
	Mirror      string // a database its writes are repeated on (Mirror / Meta.Mirror)
	MirrorNames string // the names set the mirror binds under (MirrorUnder)
	MirrorTable string // the mirror's table, over the model's (MirrorTable)
	Unmanaged   bool   // the database's own table: never created or migrated (Unmanaged)
}

// ModelAttr is a model attribute: a value given to s.Model in the model's
// Schema method (or written on the embedded orm.Base in Vuka) that
// configures the whole model's routing and mirror the way orm.Meta and
// For's options do. The ORM calls ApplyModel on every model attribute but
// orm.Meta (read whole as the model's Meta), and folds the result into the
// model.
//
//	func (p *Placement) Schema(s *orm.Schema) {
//		s.Model(orm.Meta{Table: "placements"}, mirror.Legacy{Table: "tbl_placements"})
//		s.Field(&p.ID, orm.PK{})
//	}
//
// where an app's mirror.Legacy implements ModelAttr:
//
//	func (l Legacy) ApplyModel(c *orm.ModelConfig) {
//		c.Mirror = cmp.Or(c.Mirror, "legacy")
//		c.MirrorNames = cmp.Or(c.MirrorNames, "legacy")
//		c.MirrorTable = cmp.Or(c.MirrorTable, l.Table)
//	}
//
// A ModelConfig.Names rebinds the PRIMARY: orm.For[T]() with no Names
// option, and the model's own read/write path, build the model under that
// names set, so the live columns use the Schemas[Names] overrides and
// codecs — a model whose primary is a legacy schema and whose mirror is the
// clean one (a migration's live source), the inverse of MirrorNames.
//
// The ORM never names any specific attribute: it folds whatever implements
// ModelAttr. Precedence, highest first: a For option, an orm.Meta field,
// a ModelAttr; an empty value from a lower layer never clobbers a set one.
//
// Table names the model's table under Names, as MirrorTable names the
// mirror's under MirrorNames: wherever the model is built under that names
// set (For[T](), Of, Names(set), relations into it) it wins over
// <Names>TableName, @orm.Meta's Table and TableName; with Names empty it
// is the default-naming table, where @orm.Meta's Table wins over it.
// For's orm.Table wins over both. A ModelAttr that names a mirror but
// leaves MirrorTable empty mirrors into the model's table under
// MirrorNames (the same-named table on the mirror's database when the
// sets share it).
type ModelAttr interface{ ApplyModel(*ModelConfig) }

// ApplyModel makes Meta a model attribute: s.Model(orm.Meta{…}) is the
// model's Meta, read whole (a Meta method wins over it), so it folds
// nothing into a ModelConfig.
func (Meta) ApplyModel(*ModelConfig) {}

// defaultNames is the primary names set a model-level attribute declares
// for t, "" for none: the set For[T]() (with no Names option) and the
// model's home manager bind the model under, so its live reads and writes
// take that set's column overrides and codecs. For's own Names option wins.
func defaultNames(t reflect.Type) string {
	if c, ok := modelAttrsOf(t); ok {
		return c.Names
	}
	return ""
}

// modelAttrsOf is the configuration the type's model-level attributes
// declare, folded in order, and whether any were found. It reads the same
// attributes attrMeta does: the Schema's, and those on the embedded
// orm.Base. A Meta is the model's Meta, read whole, not folded here.
func modelAttrsOf(t reflect.Type) (ModelConfig, bool) {
	s, _ := SchemaOf(t)
	var c ModelConfig
	found := false
	for _, a := range s.model {
		if _, meta := a.(Meta); meta {
			continue
		}
		if ma, ok := a.(ModelAttr); ok {
			ma.ApplyModel(&c)
			found = true
		}
	}
	return c, found
}

// applyModelAttrs folds the type's ModelAttr configuration into the model.
// An explicit orm.Meta field (already on m.meta) wins over the ModelAttr,
// which fills only what is empty (cmp.Or); For's options win later, in the
// manager. A mirror with no table named binds the model's table under its
// names set (see mirrorAs).
func (m *model) applyModelAttrs(t reflect.Type) {
	c, ok := modelAttrsOf(t)
	if !ok {
		return
	}
	m.meta.DB = cmp.Or(m.meta.DB, c.DB)
	m.meta.Read = cmp.Or(m.meta.Read, c.Read)
	m.meta.Write = cmp.Or(m.meta.Write, c.Write)
	m.meta.Mirror = cmp.Or(m.meta.Mirror, c.Mirror)
	if c.Unmanaged {
		m.meta.Unmanaged = true
	}
	m.mirrorNames = cmp.Or(m.mirrorNames, c.MirrorNames)
	m.mirrorTable = cmp.Or(m.mirrorTable, c.MirrorTable)
}
