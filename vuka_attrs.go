package orm

import (
	"fmt"
	"reflect"
	"strconv"
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
//		ID     int64        @orm.PK
//		Title  string       @orm.Char{Max: 200}
//		Slug   string       @orm.Slug{Unique: true}
//		Body   string       @orm.Text
//		Author orm.FK[User] @orm.Rel{OnDelete: orm.Cascade, Related: "posts"}
//		Views  int          @orm.DefaultTo(0) @orm.Index
//	}
//
// They win over the field's tags; SetFieldOptions wins over them.
type (
	// PK makes the field the primary key.
	PK struct{}
	// Char is a VARCHAR of Max characters, unique or indexed.
	Char struct {
		Max           int
		Unique, Index bool
	}
	// Text is an unbounded text column (what a string is by default).
	Text struct{}
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
	}
	// DefaultValue is a column default, from DefaultTo or DefaultSQL.
	DefaultValue struct{ SQL string }
)

// What deleting a related row does to the rows holding its key (Rel's
// OnDelete).
const (
	Cascade  = "cascade"
	SetNull  = "set_null"
	Restrict = "restrict"
)

// DefaultTo is the column default v, a Go value written as a SQL literal:
// @orm.DefaultTo(0), @orm.DefaultTo("draft"), @orm.DefaultTo(false).
func DefaultTo(v any) DefaultValue {
	switch x := v.(type) {
	case string:
		return DefaultValue{"'" + strings.ReplaceAll(x, "'", "''") + "'"}
	case bool:
		if x {
			return DefaultValue{"TRUE"}
		}
		return DefaultValue{"FALSE"}
	case time.Time:
		return DefaultValue{"'" + x.UTC().Format("2006-01-02 15:04:05") + "'"}
	case fmt.Stringer:
		return DefaultTo(x.String())
	}
	return DefaultValue{fmt.Sprint(v)}
}

// DefaultSQL is a column default written in SQL: @orm.DefaultSQL("CURRENT_TIMESTAMP").
func DefaultSQL(sql string) DefaultValue { return DefaultValue{sql} }

// attrOptions is the options sf's Vuka field attributes give.
func attrOptions(owner reflect.Type, sf reflect.StructField) (FieldOptions, bool) {
	attrs := vuka.FieldAttrsOf(owner)[sf.Name]
	if len(attrs) == 0 {
		return FieldOptions{}, false
	}
	var o FieldOptions
	for _, a := range attrs {
		switch x := a.(type) {
		case PK:
			o.PK = true
		case Char:
			o.MaxLength, o.Unique, o.Index = x.Max, o.Unique || x.Unique, o.Index || x.Index
		case Text:
			o.MaxLength = 0
		case Slug:
			o.MaxLength, o.Unique, o.Index = cmpOr(x.Max, 50), o.Unique || x.Unique, true
		case Unique:
			o.Unique = true
		case Index:
			o.Index = true
		case UniqueTogether:
			o.UniqueIndex = string(x)
		case Column:
			o.Column = string(x)
		case DBType:
			o.Type = string(x)
		case Null:
			o.Null = true
		case Skip:
			o.Skip = true
		case AutoNow:
			o.AutoNow = true
		case AutoNowAdd:
			o.AutoNowAdd = true
		case DefaultValue:
			o.Default = x.SQL
		case Rel:
			o.OnDelete, o.Related, o.Column, o.Through, o.Null = x.OnDelete, x.Related, x.Column, x.Through, o.Null || x.Null
		}
	}
	return o, true
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
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

// itoa is strconv.Itoa, kept short where paths are built.
func itoa(i int) string { return strconv.Itoa(i) }
