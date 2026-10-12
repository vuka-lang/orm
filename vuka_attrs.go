package orm

import (
	"cmp"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/vuka"
)

// The field attributes of a Vuka model: what a struct tag says, as typed
// values written after the field.
//
//	type Post struct {
//		orm.Base
//		ID        int64        @orm.PK
//		Title     string       @orm.Char{Max: 200, Label: "Title"}
//		Status    string       @orm.Char{Max: 20, Default: "draft", Choices: []string{"draft", "published"}}
//		Slug      string       @orm.Slug{Unique: true}
//		Body      string       @orm.LongText
//		Author    orm.FK[User] @orm.Rel{OnDelete: orm.Cascade, Related: "posts"}
//		Views     int          @orm.Int{Min: Some(0), Default: 0}
//		Published time.Time    @orm.Datetime{DefaultNow: true, Precision: 3}
//	}
//
// Each is a FieldAttr, applied in order: a later one's set members win, an
// empty member never undoes an earlier one. They win over the field's
// tags; SetFieldOptions wins over them.
type (
	// PK makes the field the primary key. An integer key is counted by
	// the database unless Manual says the program sets it; UUID makes a
	// string key a random UUID the ORM sets on create when it is empty
	// (uuid on Postgres, CHAR(36) elsewhere). A key is never NULL, and is
	// unique and indexed already.
	PK struct {
		Column               string
		Manual, UUID         bool
		Comment, Label, Help string
		Default              any
	}
	// Char is a VARCHAR of Max characters (a write holding more is
	// refused), unique or indexed. Min is the fewest characters a write
	// may hold, Choices the values it may take, Pattern a regular
	// expression it matches; Trim and Lower normalize it as it is
	// written; Collation is the column's (utf8mb4_bin, "C", NOCASE).
	Char struct {
		Max, Min             int
		Unique, Index, Null  bool
		Column               string
		Comment, Label, Help string
		Default              any
		Choices              []string
		Pattern              string
		Trim, Lower          bool
		Collation            string
	}
	// Int is an integer column: Min and Max bound its values, an integer
	// each, nil for no bound (@orm.Int{Min: 0, Max: 150}: 0 is a bound like
	// any other); Size is its width over its Go type's (Small, Medium,
	// Big); Unsigned makes it UNSIGNED on MySQL and refuses negative
	// values everywhere; Choices is the values it may take.
	Int struct {
		Min, Max             any
		Size                 IntSize
		Unsigned             bool
		Choices              []int64
		Unique, Index, Null  bool
		Column               string
		Comment, Label, Help string
		Default              any
	}
	// DateField is a DATE column of a time.Time field: the day, without
	// the time (orm.Date is the lookup transform). AutoNow sets it to today
	// on every save, AutoNowAdd on create, DefaultNow on create when it is
	// empty and in the table (DEFAULT CURRENT_DATE).
	DateField struct {
		AutoNow, AutoNowAdd, DefaultNow bool
		Unique, Index, Null             bool
		Column                          string
		Comment, Label, Help            string
		Default                         any
	}
	// Datetime is a timestamp column of a time.Time field. AutoNow sets
	// it to now on every save, AutoNowAdd on create, DefaultNow on create
	// when it is empty and in the table (DEFAULT CURRENT_TIMESTAMP).
	// Precision is its fractional digits, 1–6 (0: the database's default,
	// microseconds; times are cut to it as they are written); UTC writes
	// and reads it in UTC.
	Datetime struct {
		AutoNow, AutoNowAdd, DefaultNow bool
		Precision                       int
		UTC                             bool
		Unique, Index, Null             bool
		Column                          string
		Comment, Label, Help            string
		Default                         any
	}
	// LongText is an unbounded text column (what a string is by default).
	LongText struct{}
	// Slug is an indexed VARCHAR (Max, 50 by default) for URL parts.
	Slug struct {
		Max    int
		Unique bool
	}
	// Unique makes the column unique.
	Unique struct{}
	// UniqueTogether names a unique index: the fields sharing a name are
	// unique together.
	UniqueTogether string
	// Column names the column.
	Column string
	// DBType is the column's database type, written as is (jsonb, CHAR(2)).
	DBType string
	// Null lets the column hold NULL, for a field whose Go type has no nil
	// (an orm.FK that may be unset).
	Null struct{}
	// Skip leaves the field out of the table (orm:"-").
	Skip struct{}
	// AutoNow sets the field to the time on every save; AutoNowAdd on
	// create.
	AutoNow    struct{}
	AutoNowAdd struct{}
	// Rel describes a relation field (orm.FK, orm.M2M): what deleting the
	// related row does (Cascade, SetNull, Restrict), the inverse's name on
	// the related model, the foreign key's column, a many-to-many's table
	// between, and whether the key may be NULL.
	Rel struct {
		OnDelete string
		Related  string
		Column   string
		Through  string
		Null     bool
		// CrossDB: the related model is on another database (see
		// ModelInfo): no foreign key constraint, and queries prefetch
		// rather than join it.
		CrossDB bool
	}
	// DefaultValue is a column default, from DefaultTo or DefaultSQL.
	DefaultValue struct {
		SQL   string
		value any // DefaultTo's value; nil for DefaultSQL
	}
)

// What deleting a related row does to the rows holding its key (Rel's
// OnDelete).
const (
	Cascade  = "cascade"
	SetNull  = "set_null"
	Restrict = "restrict"
)

// DefaultTo is the column default v, a Go value written as a SQL literal:
// @orm.DefaultTo(0), @orm.DefaultTo("draft"), @orm.DefaultTo(false). A row
// Create writes with the field at its zero value takes v too.
func DefaultTo(v any) DefaultValue {
	d := defaultSQL(v)
	d.value = v
	return d
}

func defaultSQL(v any) DefaultValue {
	switch x := v.(type) {
	case string:
		return DefaultValue{SQL: "'" + strings.ReplaceAll(x, "'", "''") + "'"}
	case bool:
		if x {
			return DefaultValue{SQL: "TRUE"}
		}
		return DefaultValue{SQL: "FALSE"}
	case time.Time:
		return DefaultValue{SQL: "'" + x.UTC().Format("2006-01-02 15:04:05") + "'"}
	case fmt.Stringer:
		return defaultSQL(x.String())
	}
	return DefaultValue{SQL: fmt.Sprint(v)}
}

// DefaultSQL is a column default written in SQL: @orm.DefaultSQL("CURRENT_TIMESTAMP").
func DefaultSQL(sql string) DefaultValue { return DefaultValue{SQL: sql} }

// colAttrs is what every column attribute may say: its column, NULL,
// unique, indexed, a comment, a label and help, a default.
type colAttrs struct {
	name                 string
	null, unique, index  bool
	comment, label, help string
	dflt                 any
}

func (c colAttrs) apply(o *FieldOptions) {
	o.Column = cmp.Or(c.name, o.Column)
	o.Null, o.Unique, o.Index = o.Null || c.null, o.Unique || c.unique, o.Index || c.index
	o.Comment, o.Label, o.Help = cmp.Or(c.comment, o.Comment), cmp.Or(c.label, o.Label), cmp.Or(c.help, o.Help)
	if c.dflt != nil {
		DefaultTo(c.dflt).Apply(o)
	}
}

func (x PK) Apply(o *FieldOptions) {
	o.PK, o.Manual, o.UUID = true, o.Manual || x.Manual, o.UUID || x.UUID
	colAttrs{name: x.Column, comment: x.Comment, label: x.Label, help: x.Help, dflt: x.Default}.apply(o)
}

func (x Char) Apply(o *FieldOptions) {
	if x.Max > 0 {
		o.MaxLength = x.Max
	}
	if x.Min > 0 {
		o.MinLength = x.Min
	}
	if x.Choices != nil {
		o.Choices = make([]any, len(x.Choices))
		for i, c := range x.Choices {
			o.Choices[i] = c
		}
	}
	o.Pattern, o.Collation = cmp.Or(x.Pattern, o.Pattern), cmp.Or(x.Collation, o.Collation)
	o.Trim, o.Lower = o.Trim || x.Trim, o.Lower || x.Lower
	colAttrs{x.Column, x.Null, x.Unique, x.Index, x.Comment, x.Label, x.Help, x.Default}.apply(o)
}

func (x Int) Apply(o *FieldOptions) {
	bound := func(dst *vuka.Option[int64], v any, name string) {
		if v == nil {
			return
		}
		rv := reflect.ValueOf(v)
		switch k := rv.Kind(); {
		case k >= reflect.Int && k <= reflect.Int64:
			*dst = vuka.Some(rv.Int())
		case k >= reflect.Uint && k <= reflect.Uint64:
			*dst = vuka.Some(int64(rv.Uint()))
		default:
			o.err = fmt.Errorf("Int's %s is an integer, not %T", name, v)
		}
	}
	bound(&o.Min, x.Min, "Min")
	bound(&o.Max, x.Max, "Max")
	if x.Size != 0 {
		o.Size = x.Size
	}
	if x.Choices != nil {
		o.Choices = make([]any, len(x.Choices))
		for i, c := range x.Choices {
			o.Choices[i] = c
		}
	}
	o.Unsigned = o.Unsigned || x.Unsigned
	colAttrs{x.Column, x.Null, x.Unique, x.Index, x.Comment, x.Label, x.Help, x.Default}.apply(o)
}

func (x DateField) Apply(o *FieldOptions) {
	o.Date = true
	stamps(o, x.AutoNow, x.AutoNowAdd, x.DefaultNow, "CURRENT_DATE")
	colAttrs{x.Column, x.Null, x.Unique, x.Index, x.Comment, x.Label, x.Help, x.Default}.apply(o)
}

func (x Datetime) Apply(o *FieldOptions) {
	if x.Precision > 0 {
		o.Precision = x.Precision
	}
	o.UTC = o.UTC || x.UTC
	stamps(o, x.AutoNow, x.AutoNowAdd, x.DefaultNow, "CURRENT_TIMESTAMP")
	colAttrs{x.Column, x.Null, x.Unique, x.Index, x.Comment, x.Label, x.Help, x.Default}.apply(o)
}

// stamps sets a time's stamps: DefaultNow is AutoNowAdd with the table's
// default the database's time (sql).
func stamps(o *FieldOptions, autoNow, autoNowAdd, defaultNow bool, sql string) {
	o.AutoNow, o.AutoNowAdd = o.AutoNow || autoNow, o.AutoNowAdd || autoNowAdd || defaultNow
	if defaultNow {
		o.Default, o.Initial = sql, nil
	}
}

func (LongText) Apply(o *FieldOptions) { o.MaxLength = 0 }

func (x Slug) Apply(o *FieldOptions) {
	o.MaxLength, o.Unique, o.Index = cmp.Or(x.Max, 50), o.Unique || x.Unique, true
}

func (Unique) Apply(o *FieldOptions)           { o.Unique = true }
func (Index) Apply(o *FieldOptions)            { o.Index = true }
func (x UniqueTogether) Apply(o *FieldOptions) { o.UniqueIndex = cmp.Or(string(x), o.UniqueIndex) }
func (x Column) Apply(o *FieldOptions)         { o.Column = cmp.Or(string(x), o.Column) }
func (x DBType) Apply(o *FieldOptions)         { o.Type = cmp.Or(string(x), o.Type) }
func (Null) Apply(o *FieldOptions)             { o.Null = true }
func (Skip) Apply(o *FieldOptions)             { o.Skip = true }
func (AutoNow) Apply(o *FieldOptions)          { o.AutoNow = true }
func (AutoNowAdd) Apply(o *FieldOptions)       { o.AutoNowAdd = true }
func (x DefaultValue) Apply(o *FieldOptions)   { o.Default, o.Initial = x.SQL, x.value }

func (x Dims) Apply(o *FieldOptions) {
	if x > 0 {
		o.Dims = int(x)
	}
}

func (x Rel) Apply(o *FieldOptions) {
	o.OnDelete, o.Related = cmp.Or(x.OnDelete, o.OnDelete), cmp.Or(x.Related, o.Related)
	o.Column, o.Through = cmp.Or(x.Column, o.Column), cmp.Or(x.Through, o.Through)
	o.Null, o.CrossDB = o.Null || x.Null, o.CrossDB || x.CrossDB
}

// fieldValidator is an attribute checking the values written to its
// field.
type fieldValidator interface{ Validate(v any) error }

// attrOptions is the options sf's Vuka field attributes give, applied in
// order; an attribute with a Validate method adds it to the checks.
func attrOptions(owner reflect.Type, sf reflect.StructField) (FieldOptions, bool) {
	attrs := vuka.FieldAttrsOf(owner)[sf.Name]
	if len(attrs) == 0 {
		return FieldOptions{}, false
	}
	var o FieldOptions
	for _, a := range attrs {
		if fa, ok := a.(FieldAttr); ok {
			fa.Apply(&o)
		}
		if v, ok := a.(fieldValidator); ok {
			o.Checks = append(o.Checks, v.Validate)
		}
	}
	return o, true
}

// typeMetas is the Meta an @orm.Entity type declares, by type.
var typeMetas sync.Map

// attrMeta is the Meta t declares as Vuka attributes: on its embedded
// orm.Base (orm.Base @orm.Meta{…}) or the type (@orm.Meta{…} @orm.Entity).
func attrMeta(t reflect.Type) (Meta, bool) {
	if m, ok := typeMetas.Load(t); ok {
		return m.(Meta), true
	}
	for _, attrs := range vuka.FieldAttrsOf(t) {
		for _, a := range attrs {
			if m, ok := a.(Meta); ok {
				return m, true
			}
		}
	}
	return Meta{}, false
}

// Entity is a type decorator registering a Vuka model at start-up, so
// Check and the migration commands know it without orm generate; an
// @orm.Meta{…} on the type is its Meta.
//
//	@orm.Meta{Table: "posts", Ordering: []string{"-published"}}
//	@orm.Entity
//	type Post struct { orm.Base; … }
func Entity(t *vuka.Type) {
	var m Meta
	if t.Attr(&m) {
		typeMetas.Store(t.Reflect, m)
	}
	if r, ok := reflect.New(t.Reflect).Interface().(interface{ register() }); ok {
		r.register()
	}
}

// Where the attributes go: a model's fields; Meta on its embedded
// orm.Base field or its type. Vuka refuses them anywhere else, and an
// editor offers only these after an @ on a field. (The result types are
// written out: Vuka reads the array's length, not through an alias.)
func (PK) vukaTargets() [vuka.OnField]struct{}             { return [vuka.OnField]struct{}{} }
func (Char) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (Int) vukaTargets() [vuka.OnField]struct{}            { return [vuka.OnField]struct{}{} }
func (DateField) vukaTargets() [vuka.OnField]struct{}      { return [vuka.OnField]struct{}{} }
func (Datetime) vukaTargets() [vuka.OnField]struct{}       { return [vuka.OnField]struct{}{} }
func (LongText) vukaTargets() [vuka.OnField]struct{}       { return [vuka.OnField]struct{}{} }
func (Slug) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (Unique) vukaTargets() [vuka.OnField]struct{}         { return [vuka.OnField]struct{}{} }
func (UniqueTogether) vukaTargets() [vuka.OnField]struct{} { return [vuka.OnField]struct{}{} }
func (Column) vukaTargets() [vuka.OnField]struct{}         { return [vuka.OnField]struct{}{} }
func (DBType) vukaTargets() [vuka.OnField]struct{}         { return [vuka.OnField]struct{}{} }
func (Null) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (Skip) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (AutoNow) vukaTargets() [vuka.OnField]struct{}        { return [vuka.OnField]struct{}{} }
func (AutoNowAdd) vukaTargets() [vuka.OnField]struct{}     { return [vuka.OnField]struct{}{} }
func (Rel) vukaTargets() [vuka.OnField]struct{}            { return [vuka.OnField]struct{}{} }
func (DefaultValue) vukaTargets() [vuka.OnField]struct{}   { return [vuka.OnField]struct{}{} }
func (Index) vukaTargets() [vuka.OnField]struct{}          { return [vuka.OnField]struct{}{} }
func (FieldOptions) vukaTargets() [vuka.OnField]struct{}   { return [vuka.OnField]struct{}{} }
func (Dims) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (HNSW) vukaTargets() [vuka.OnField]struct{}           { return [vuka.OnField]struct{}{} }
func (IVFFlat) vukaTargets() [vuka.OnField]struct{}        { return [vuka.OnField]struct{}{} }
func (Trigram) vukaTargets() [vuka.OnField]struct{}        { return [vuka.OnField]struct{}{} }
func (FullText) vukaTargets() [vuka.OnField]struct{}       { return [vuka.OnField]struct{}{} }
func (SearchOf) vukaTargets() [vuka.OnField]struct{}       { return [vuka.OnField]struct{}{} }

func (Meta) vukaTargets() [vuka.OnField | vuka.OnType]struct{} {
	return [vuka.OnField | vuka.OnType]struct{}{}
}

// TxOn and Retry go on the functions and methods @orm.Transaction runs.
func (TxOn) vukaTargets() [vuka.OnFunc | vuka.OnMethod]struct{} {
	return [vuka.OnFunc | vuka.OnMethod]struct{}{}
}
func (Retry) vukaTargets() [vuka.OnFunc | vuka.OnMethod]struct{} {
	return [vuka.OnFunc | vuka.OnMethod]struct{}{}
}
