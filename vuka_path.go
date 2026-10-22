package orm

import (
	"fmt"
	"strings"

	"github.com/vuka-lang/orm/v2/internal/tags"

	"github.com/vuka-lang/vuka/v2"
)

// Arg is a field reference of T, or a typed expression over T, of Go type
// V: Post.Title, Post.Author.Name, fn.Lower(Post.Title). Functions taking
// one keep V, so their results stay typed.
type Arg[T, V any] interface{ Get(T) V }

// Path is a field reference of T, whatever its type: Post.Author, Post.Tags.
type Path[T any] interface{ Asc() vuka.Order[T] }

// Sort is an ordering of Ts: a field reference's Asc or Desc
// (vuka.Order[T]), or an expression's.
type Sort[T any] interface{ Compare(a, b T) int }

// pather is a vuka field reference.
type pather interface{ FieldPath() vuka.FieldPath }

// lookupPath is a field path as the ORM's lookups write it, each step in
// snake_case: author__name.
func lookupPath(p vuka.FieldPath) string {
	names := p.Names()
	for i, n := range names {
		names[i] = tags.Snake(n)
	}
	return strings.Join(names, "__")
}

// pathName is the lookup path of a field reference, a term's annotation
// name, or a string as is.
func pathName(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case typedField:
		return x.Name()
	case pather:
		return lookupPath(x.FieldPath())
	case interface{ annName() string }:
		return x.annName()
	}
	return fmt.Sprint(v)
}

func pathNames(vs []any) ([]string, error) {
	out := make([]string, len(vs))
	for i, v := range vs {
		if out[i] = pathName(v); out[i] == "" {
			return nil, fmt.Errorf("orm: %v names no field", v)
		}
	}
	return out, nil
}

// FieldName is a field reference's lookup path (Post.Author.Name →
// "author__name"), for the ORM's string APIs.
func FieldName[T any](p Path[T]) string { return lookupPath(p.Asc().Path) }

// ExprOf is v as an expression: a typed field (PostFields.Title) or field
// reference as the field (F), a typed expression or Expr as itself,
// anything else a bound value. Expressions and conditions taking values
// (functions, SQL, Q's values, Case, Set) take typed fields and field
// references directly, the same way: orm.Lower(PostFields.Title).
func ExprOf(v any) Expr {
	switch x := v.(type) {
	case Expr:
		return x
	case typedField:
		return F(x.Name())
	case pather:
		return F(lookupPath(x.FieldPath()))
	}
	return Value(argValue(v))
}

// prefetchSpecs is specs with field references as their paths.
func prefetchSpecs(specs []any) []any {
	out := make([]any, len(specs))
	for i, s := range specs {
		if p, ok := s.(pather); ok {
			out[i] = lookupPath(p.FieldPath())
		} else {
			out[i] = s
		}
	}
	return out
}

// LookupPath is a field reference's path as lookups write it: author__name.
func LookupPath(p vuka.FieldPath) string { return lookupPath(p) }

// PathName is a field reference's lookup path, a named term's name, or a
// string as is; "" for an unnamed term.
func PathName(v any) string {
	if _, ok := v.(interface{ annName() string }); !ok {
		if _, ok := v.(Expr); ok {
			return ""
		}
	}
	return pathName(v)
}

// Operand is v as the engine takes a value that may be an expression: a
// typed field or field reference as the field, an Expr as is, an orm.FK its key, an
// Option its value, anything else as is (bound, and typed by the engine
// where it must be, as in Case results on Postgres).
func Operand(v any) any {
	switch x := v.(type) {
	case Expr:
		return x
	case typedField:
		return F(x.Name())
	case pather:
		return F(lookupPath(x.FieldPath()))
	}
	return argValue(v)
}
