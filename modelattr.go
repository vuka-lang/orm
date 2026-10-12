package orm

import (
	"cmp"
	"reflect"

	"github.com/vuka-lang/vuka"
)

// ModelConfig is the model-level configuration a ModelAttr sets: the same
// routing and mirror an @orm.Meta and For's options set, in one value. A
// zero member leaves the model's setting; a set one is folded in. Later
// attributes' set members win over earlier ones, and an empty member never
// undoes one already set (cmp.Or-style), as field attributes merge.
type ModelConfig struct {
	DB          string // the model's primary database (On / Meta.DB); the default otherwise
	Names       string // the names set the model's OWN (primary) manager binds under
	Read        string // the database its reads go to, over DB (Meta.Read; a replica)
	Write       string // the database its writes go to, over DB (Meta.Write)
	Mirror      string // a database its writes are repeated on (Mirror / Meta.Mirror)
	MirrorNames string // the names set the mirror binds under (MirrorUnder)
	MirrorTable string // the mirror's table, over the model's (MirrorTable)
	Unmanaged   bool   // the database's own table: never created or migrated (Unmanaged)
}

// ModelAttr is a model-level Vuka attribute: an app-defined attribute,
// written on the embedded orm.Base, that configures the whole model's
// routing and mirror the way @orm.Meta and For's options do. The ORM calls
// ApplyModel on every attribute value on that field implementing it (so
// @orm.Meta and a ModelAttr on one orm.Base both apply), and folds the
// result into the model.
//
//	type Placement struct {
//		orm.Base @orm.Meta{Table: "placements"} @mirror.Legacy{Table: "tbl_placements"}
//		ID     int64 @orm.PK
//		Status string
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
// ModelAttr. Precedence, highest first: a For option, an @orm.Meta field,
// a ModelAttr; an empty value from a lower layer never clobbers a set one.
// A ModelAttr that names a mirror but leaves MirrorTable empty mirrors into
// the model's own table (the same-named table on the mirror's database).
type ModelAttr interface{ ApplyModel(*ModelConfig) }

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
// field attributes attrMeta does — those on the embedded orm.Base.
func modelAttrsOf(t reflect.Type) (ModelConfig, bool) {
	var c ModelConfig
	found := false
	for _, attrs := range vuka.FieldAttrsOf(t) {
		for _, a := range attrs {
			if ma, ok := a.(ModelAttr); ok {
				ma.ApplyModel(&c)
				found = true
			}
		}
	}
	return c, found
}

// applyModelAttrs folds the type's ModelAttr configuration into the model.
// An explicit @orm.Meta field (already on m.meta) wins over the ModelAttr,
// which fills only what is empty (cmp.Or); For's options win later, in the
// manager. A mirror with no table named defaults to the model's own table.
// Called after m.Table is resolved, so the default table is known.
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
	if c.Mirror != "" || c.MirrorNames != "" || c.MirrorTable != "" {
		m.mirrorNames = cmp.Or(m.mirrorNames, c.MirrorNames)
		// An empty MirrorTable mirrors into the model's own table.
		m.mirrorTable = cmp.Or(m.mirrorTable, c.MirrorTable, m.Table)
	}
}
