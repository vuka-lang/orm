package orm

import (
	"cmp"
	"reflect"
	"sync/atomic"

	"github.com/vuka-lang/orm/internal/tags"
)

// FieldOptions is what a struct tag says about a field, as a value: the
// way a layer above the ORM (a language's field attributes, a schema file)
// declares a model's fields without tags. A zero member leaves the tag's
// setting; a set one wins over it.
type FieldOptions struct {
	Column      string // the column; the field's name in snake_case otherwise
	Skip        bool   // no column (orm:"-")
	PK          bool
	Unique      bool
	Index       bool
	UniqueIndex string // a named unique index: fields sharing it are unique together
	MaxLength   int    // a VARCHAR's size
	Type        string // a database type of the program's own
	// Default is the SQL literal a row inserted without the column takes,
	// in the table migrations make ('draft', 0, CURRENT_TIMESTAMP).
	Default    string
	AutoNow    bool   // set to the time on every save
	AutoNowAdd bool   // set to the time on create
	OnDelete   string // a foreign key's: cascade, set_null, restrict
	Related    string // the name of a relation's inverse on the related model
	FK         string // a relation's column holding the related row's key
	Null       bool   // the column takes NULL (an orm.FK that may be unset)
	Through    string // a many-to-many's table between
	Dims       int    // a vector's dimensions
}

// FieldOptionsFunc supplies a model's field options: ok false for a field
// it says nothing about. owner is the struct declaring the field (an
// embedded one's own type for its fields).
type FieldOptionsFunc func(owner reflect.Type, field reflect.StructField) (opts FieldOptions, ok bool)

var fieldOptions atomic.Pointer[FieldOptionsFunc]

// SetFieldOptions installs fn as the source of field options beside the
// struct tags. Set it before the first query: a model's metadata is read
// once. orm generate reads tags only, so a model whose columns fn renames
// is read by reflection rather than by its generated scanner.
func SetFieldOptions(fn FieldOptionsFunc) {
	if fn == nil {
		fieldOptions.Store(nil)
		return
	}
	fieldOptions.Store(&fn)
}

// withOptions is t with the options of sf's Vuka field attributes, then
// those SetFieldOptions supplies, set over it, and the default they give.
func withOptions(owner reflect.Type, sf reflect.StructField, t tags.Tags) (tags.Tags, string) {
	dflt := ""
	if o, ok := attrOptions(owner, sf); ok {
		t, dflt = o.over(t), o.Default
	}
	if p := fieldOptions.Load(); p != nil {
		if o, ok := (*p)(owner, sf); ok {
			t, dflt = o.over(t), cmp.Or(o.Default, dflt)
		}
	}
	return t, dflt
}

// over is t with o's set members over it.
func (o FieldOptions) over(t tags.Tags) tags.Tags {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&t.Column, o.Column)
	set(&t.UniqueIndex, o.UniqueIndex)
	set(&t.Type, o.Type)
	set(&t.OnDelete, o.OnDelete)
	set(&t.Related, o.Related)
	set(&t.FK, o.FK)
	set(&t.M2M, o.Through)
	t.Skip = t.Skip || o.Skip
	t.PK = t.PK || o.PK
	t.Unique = t.Unique || o.Unique
	t.Index = t.Index || o.Index
	t.AutoNow = t.AutoNow || o.AutoNow
	t.AutoNowAdd = t.AutoNowAdd || o.AutoNowAdd
	t.Null = t.Null || o.Null
	if o.MaxLength > 0 {
		t.Size = o.MaxLength
	}
	if o.Dims > 0 {
		t.Vector = o.Dims
	}
	return t
}
