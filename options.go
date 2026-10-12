package orm

import (
	"reflect"
	"slices"
	"sync/atomic"

	"github.com/vuka-lang/orm/internal/tags"
	"github.com/vuka-lang/vuka"
)

// FieldOptions is what a struct tag says about a field, as a value: the
// way a layer above the ORM (a language's field attributes, a schema file)
// declares a model's fields without tags. A zero member leaves the tag's
// setting; a set one wins over it.
//
// FieldOptions is a FieldAttr itself: its set members applied over
// another's, as SetFieldOptions's are over a field's attributes.
type FieldOptions struct {
	Column      string // the column; the field's name in snake_case otherwise
	Skip        bool   // no column (orm:"-")
	PK          bool
	Unique      bool
	Index       bool
	UniqueIndex string // a named unique index: fields sharing it are unique together
	MaxLength   int    // a VARCHAR's size, and the most characters a write may hold
	Type        string // a database type of the program's own
	// Default is the SQL literal a row inserted without the column takes,
	// in the table migrations make ('draft', 0, CURRENT_TIMESTAMP).
	Default string
	// Initial is the Go value Create gives the field when it is left at
	// its zero value: Default as a value (DefaultTo and the attributes'
	// Default set both).
	Initial    any
	AutoNow    bool   // set to the time on every save
	AutoNowAdd bool   // set to the time on create
	OnDelete   string // a foreign key's: cascade, set_null, restrict
	Related    string // the name of a relation's inverse on the related model
	FK         string // a relation's column holding the related row's key
	Null       bool   // the column takes NULL (an orm.FK that may be unset)
	Through    string // a many-to-many's table between
	Dims       int    // a vector's dimensions
	// CrossDB marks a relation to a model on another database: its key
	// column has no foreign key constraint, and queries never join it.
	CrossDB bool

	// Comment is the column's comment in the table (Postgres, MySQL);
	// Label and Help describe the field to forms and tools (FieldInfo).
	Comment, Label, Help string
	// Collation is a text column's collation, as the database names it
	// (utf8mb4_bin, "C", NOCASE).
	Collation string

	// The checks a write makes of the field's value (Create, Save, Update,
	// BulkCreate): an Invalid error naming the column when one fails.
	// Choices is the values it may take; MinLength the fewest characters
	// of a text; Min and Max a number's bounds; Pattern a regular
	// expression (Go's syntax) a text matches; Checks functions of the
	// value, a non-nil error refusing it.
	Choices   []any
	MinLength int
	Min, Max  vuka.Option[int64]
	Pattern   string
	Checks    []func(v any) error
	// Trim and Lower normalize a text as it is written: its surrounding
	// spaces removed, in lower case.
	Trim, Lower bool

	// Size is an integer column's width (Small, Medium, Big), over its Go
	// type's; Unsigned makes it UNSIGNED on MySQL, and refuses negative
	// values on every database.
	Size     IntSize
	Unsigned bool
	// Date makes a time.Time field a DATE column. Precision is a
	// timestamp's fractional digits (1–6; 0 is the database's default,
	// microseconds); UTC writes and reads its times in UTC.
	Date      bool
	Precision int
	UTC       bool
	// Manual is an integer primary key the program sets, not one the
	// database counts; UUID a key the ORM sets to a random UUID on create
	// when it is empty (a string field).
	Manual, UUID bool

	// Schemas overrides the field's column under named schemas (names
	// sets), keyed by the set's name: what a layer above the ORM declares
	// in place of a per-set struct tag (legacy:"column:…;type:…"). The
	// override for a set is applied only when the model is read under it
	// (orm.Names, orm.Of, a mirror under the set); the default schema is
	// untouched. A SchemaField's Codec adapts the value's format there.
	Schemas map[string]SchemaField

	err error // what an attribute couldn't apply: the model can't be mapped
}

// SchemaField is a field's column under one names set: its name and SQL
// type there (as a legacy:"column:…;type:…" tag gives them), and a Codec
// adapting its value's format for that set. A zero member leaves the
// default; Skip drops the column there (legacy:"-"). The SQL type is the
// column's documented type and what migrations would make; a legacy
// (Unmanaged) table is never migrated, so there it only documents the
// write format.
type SchemaField struct {
	Column string
	Type   string
	Skip   bool
	Codec  SchemaCodec
}

// FieldAttr is a field attribute: any value written after a model's field
// that says something about its column. The ORM applies a field's
// attributes in order, each over what the ones before it set; the
// built-in ones (orm.Char, orm.Int, orm.Column, …) are FieldAttrs too.
//
//	type Email struct{ Unique bool }
//
//	func (e Email) Apply(o *orm.FieldOptions) {
//		o.MaxLength, o.Lower, o.Trim = 254, true, true
//		o.Unique = o.Unique || e.Unique
//	}
//
// An attribute with a method Validate(v any) error checks each value a
// write gives the field as well: a non-nil error refuses it (Invalid).
type FieldAttr interface{ Apply(o *FieldOptions) }

// IntSize is an integer column's width.
type IntSize uint8

// The widths of an integer column: SMALLINT, INTEGER (32 bits) and BIGINT.
const (
	Small IntSize = iota + 1
	Medium
	Big
)

// Apply sets o's set members over dst: a string, number or list that
// isn't empty, a bool that is true, a bound that is present. Checks add up.
func (o FieldOptions) Apply(dst *FieldOptions) {
	str := func(d *string, v string) {
		if v != "" {
			*d = v
		}
	}
	str(&dst.Column, o.Column)
	str(&dst.UniqueIndex, o.UniqueIndex)
	str(&dst.Type, o.Type)
	str(&dst.Default, o.Default)
	str(&dst.OnDelete, o.OnDelete)
	str(&dst.Related, o.Related)
	str(&dst.FK, o.FK)
	str(&dst.Through, o.Through)
	str(&dst.Comment, o.Comment)
	str(&dst.Label, o.Label)
	str(&dst.Help, o.Help)
	str(&dst.Collation, o.Collation)
	str(&dst.Pattern, o.Pattern)
	if o.Initial != nil {
		dst.Initial = o.Initial
	}
	if o.MaxLength > 0 {
		dst.MaxLength = o.MaxLength
	}
	if o.MinLength > 0 {
		dst.MinLength = o.MinLength
	}
	if o.Dims > 0 {
		dst.Dims = o.Dims
	}
	if o.Precision > 0 {
		dst.Precision = o.Precision
	}
	if o.Size != 0 {
		dst.Size = o.Size
	}
	if o.Choices != nil {
		dst.Choices = o.Choices
	}
	if o.Min.IsSome() {
		dst.Min = o.Min
	}
	if o.Max.IsSome() {
		dst.Max = o.Max
	}
	dst.Checks = append(slices.Clip(dst.Checks), o.Checks...)
	if o.Schemas != nil {
		if dst.Schemas == nil {
			dst.Schemas = make(map[string]SchemaField, len(o.Schemas))
		}
		for k, v := range o.Schemas {
			dst.Schemas[k] = v
		}
	}
	if o.err != nil {
		dst.err = o.err
	}
	for _, b := range []struct {
		d *bool
		v bool
	}{{&dst.Skip, o.Skip}, {&dst.PK, o.PK}, {&dst.Unique, o.Unique}, {&dst.Index, o.Index}, {&dst.AutoNow, o.AutoNow},
		{&dst.AutoNowAdd, o.AutoNowAdd}, {&dst.Null, o.Null}, {&dst.CrossDB, o.CrossDB}, {&dst.Trim, o.Trim}, {&dst.Lower, o.Lower},
		{&dst.Unsigned, o.Unsigned}, {&dst.Date, o.Date}, {&dst.UTC, o.UTC}, {&dst.Manual, o.Manual}, {&dst.UUID, o.UUID}} {
		*b.d = *b.d || b.v
	}
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
// those SetFieldOptions supplies, set over it, and the options together.
func withOptions(owner reflect.Type, sf reflect.StructField, t tags.Tags) (tags.Tags, FieldOptions) {
	o, _ := attrOptions(owner, sf)
	if p := fieldOptions.Load(); p != nil {
		if fo, ok := (*p)(owner, sf); ok {
			fo.Apply(&o)
		}
	}
	return o.over(t), o
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
	t.CrossDB = t.CrossDB || o.CrossDB
	if o.MaxLength > 0 {
		t.Size = o.MaxLength
	}
	if o.Dims > 0 {
		t.Vector = o.Dims
	}
	return t
}
