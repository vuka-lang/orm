package orm

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/vuka-lang/vuka"
)

// Schema is what a model declares beyond its Go types: attributes of the
// model and of its fields, as values. A model declares them in a Schema
// method, which the ORM calls once, on a zero value, the first time it
// maps the model:
//
//	type Post struct {
//		orm.Model[Post]
//		ID        int64
//		Title     string
//		Slug      string
//		Author    orm.FK[User]
//		Published time.Time
//	}
//
//	func (p *Post) Schema(s *orm.Schema) {
//		s.Model(orm.Meta{Table: "posts", Ordering: []string{"-published"}})
//		s.Field(&p.Title, orm.Char{Max: 200})
//		s.Field(&p.Slug, orm.Slug{Unique: true})
//		s.Field(&p.Author, orm.Rel{OnDelete: orm.Cascade, Related: "posts"})
//		s.Field(&p.Published, orm.Datetime{DefaultNow: true})
//	}
//
// A field is named by its address, so a rename is checked by the compiler;
// a field with nothing to say isn't listed, its column, NULL-ness and type
// coming from its name and Go type. Fields of embedded structs are named
// the same way (&p.Timestamps.Created, or the promoted &p.Created).
//
// The attributes are the ones the ORM declares (PK, Char, Int, Datetime,
// Rel, Column, Unique, …) and any value of another package implementing
// FieldAttr or ModelAttr: the ORM treats them alike. Attributes apply in
// order, a later one's set members over an earlier one's. A Vuka model's
// field attributes (Title string @orm.Char{Max: 200}) are the same values,
// and come before its Schema's.
type Schema struct {
	t      reflect.Type
	root   reflect.Value // the zero value Schema is called on, kept while it runs
	model  []any
	fields map[string][]any // by field index path, pathKey's
	errs   []error
}

// Model adds model attributes: orm.Meta, and any ModelAttr.
func (s *Schema) Model(attrs ...ModelAttr) {
	for _, a := range attrs {
		if a == nil {
			s.fail("E0502", "s.Model is given a nil attribute")
			continue
		}
		s.model = append(s.model, a)
	}
}

// Field adds attributes to the field ptr points to, a field of the value
// Schema is called on.
func (s *Schema) Field(ptr any, attrs ...FieldAttr) {
	v := reflect.ValueOf(ptr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		s.fail("E0501", "s.Field is given %T, not the address of one of %s's fields (s.Field(&p.Title, …))", ptr, s.t.Name())
		return
	}
	var path []int
	ok := v.Pointer() >= s.root.Pointer()
	if ok {
		path, ok = fieldAt(s.t, v.Pointer()-s.root.Pointer(), v.Type().Elem())
	}
	if !ok {
		s.fail("E0501", "s.Field is given a %s that isn't a field of %s: pass the address of the receiver's field (s.Field(&p.Title, …))", v.Type(), s.t.Name())
		return
	}
	k := pathKey(path)
	for _, a := range attrs {
		if a == nil {
			s.fail("E0502", "s.Field(&%s, …) is given a nil attribute", fieldName(s.t, path))
			continue
		}
		s.fields[k] = append(s.fields[k], a)
	}
}

func (s *Schema) fail(code, format string, args ...any) {
	s.errs = append(s.errs, &SchemaError{Code: code, Model: s.t.Name(), Msg: fmt.Sprintf(format, args...)})
}

// ModelAttrs is the model's attributes, in order.
func (s *Schema) ModelAttrs() []any { return s.model }

// FieldAttrs is the attributes of the model's field name (its Go name, as
// the model's own or promoted from a struct it embeds), in order.
func (s *Schema) FieldAttrs(name string) []any {
	sf, ok := s.t.FieldByName(name)
	if !ok {
		return nil
	}
	return s.fields[pathKey(sf.Index)]
}

// SchemaError is a mistake in a model's Schema method, reported by every
// query of the model and by Check. Code is E0501 for an argument of
// s.Field that isn't the address of one of the model's fields, E0502 for
// a nil attribute, E0503 for a Schema method that panicked.
type SchemaError struct {
	Code  string
	Model string
	Msg   string
}

func (e *SchemaError) Error() string { return "orm: " + e.Code + ": " + e.Model + ".Schema: " + e.Msg }

type schemaer interface{ Schema(*Schema) }

var schemas sync.Map // reflect.Type → *Schema

// SchemaOf is what the struct t declares: its Vuka attributes, then its
// Schema method's. A model declaring nothing has an empty Schema; one whose
// Schema method is wrong has a *SchemaError.
func SchemaOf(t reflect.Type) (*Schema, error) {
	if v, ok := schemas.Load(t); ok {
		s := v.(*Schema)
		return s, schemaErr(s)
	}
	s := &Schema{t: t, fields: map[string][]any{}}
	if t.Kind() == reflect.Struct {
		s.vukaAttrs()
		s.declared()
	}
	v, _ := schemas.LoadOrStore(t, s)
	s = v.(*Schema)
	return s, schemaErr(s)
}

func schemaErr(s *Schema) error { return errors.Join(s.errs...) }

// vukaAttrs adds the attributes a Vuka model writes on its fields: those of
// every struct t holds by value, by path, and the model attributes on its
// own fields (orm.Base @orm.Meta{…} @mirror.Legacy{}).
func (s *Schema) vukaAttrs() {
	if m, ok := typeMetas.Load(s.t); ok {
		s.model = append(s.model, m.(Meta))
	}
	for _, attrs := range vuka.FieldAttrsOf(s.t) {
		for _, a := range attrs {
			if _, ok := a.(ModelAttr); ok {
				s.model = append(s.model, a)
			}
		}
	}
	var walk func(t reflect.Type, index []int, depth int)
	walk = func(t reflect.Type, index []int, depth int) {
		attrs := vuka.FieldAttrsOf(t)
		for i := range t.NumField() {
			sf := t.Field(i)
			path := append(append([]int(nil), index...), i)
			if a := attrs[sf.Name]; len(a) > 0 {
				s.fields[pathKey(path)] = append(s.fields[pathKey(path)], a...)
			}
			ft := sf.Type
			if ft.Kind() == reflect.Pointer && sf.Anonymous {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && depth < 8 {
				walk(ft, path, depth+1)
			}
		}
	}
	walk(s.t, nil, 0)
}

// declared runs the type's Schema method on a zero value.
func (s *Schema) declared() {
	d, ok := reflect.New(s.t).Interface().(schemaer)
	if !ok {
		return
	}
	s.root = reflect.ValueOf(d)
	defer func() {
		if r := recover(); r != nil {
			s.fail("E0503", "panicked: %v (a field behind a nil pointer embed can't be named)", r)
		}
		s.root = reflect.Value{}
	}()
	d.Schema(s)
}

// fieldAt is the index path of the field of type ft at offset off in t,
// through the structs t holds by value.
func fieldAt(t reflect.Type, off uintptr, ft reflect.Type) ([]int, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if off < f.Offset || off >= f.Offset+max(f.Type.Size(), 1) {
			continue
		}
		if off == f.Offset && f.Type == ft {
			return []int{i}, true
		}
		if f.Type.Kind() == reflect.Struct {
			if p, ok := fieldAt(f.Type, off-f.Offset, ft); ok {
				return append([]int{i}, p...), true
			}
		}
	}
	return nil, false
}

func pathKey(index []int) string {
	var b strings.Builder
	for i, n := range index {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

func fieldName(t reflect.Type, index []int) string {
	names := make([]string, len(index))
	for i, n := range index {
		f := t.Field(n)
		names[i], t = f.Name, f.Type
	}
	return "p." + strings.Join(names, ".")
}

// schemaFieldAttrs is the attributes declared for the model t's field at
// index.
func schemaFieldAttrs(t reflect.Type, index []int) []any {
	s, _ := SchemaOf(t)
	return s.fields[pathKey(index)]
}

// schemaMeta is the Meta the model declares as an attribute, the last one.
func schemaMeta(t reflect.Type) (Meta, bool) {
	s, _ := SchemaOf(t)
	var m Meta
	found := false
	for _, a := range s.model {
		if x, ok := a.(Meta); ok {
			m, found = x, true
		}
	}
	return m, found
}
