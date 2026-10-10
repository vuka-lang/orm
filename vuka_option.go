package orm

import (
	"reflect"
	"strings"
	"unsafe"

	"github.com/vuka-lang/vuka"
)

// A vuka.Option[X] field is a nullable column of X: None is NULL.

var optionPkg = reflect.TypeFor[vuka.Option[int]]().PkgPath()

func isOption(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t.PkgPath() == optionPkg && strings.HasPrefix(t.Name(), "Option[")
}

// optionElem is X of an Option[X].
func optionElem(t reflect.Type) reflect.Type { return t.Field(0).Type }

// optionParts is the value and present flag of an addressable Option.
func optionParts(v reflect.Value) (val, ok reflect.Value) {
	if !v.CanAddr() {
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		v = c
	}
	f0, f1 := v.Field(0), v.Field(1)
	return reflect.NewAt(f0.Type(), unsafe.Pointer(f0.UnsafeAddr())).Elem(),
		reflect.NewAt(f1.Type(), unsafe.Pointer(f1.UnsafeAddr())).Elem()
}

// optionValue is an Option's value as a query argument: nil for None.
func optionValue(v reflect.Value) any {
	val, ok := optionParts(v)
	if !ok.Bool() {
		return nil
	}
	return value(val)
}

// assignOption sets an addressable Option from a database value.
func assignOption(dst reflect.Value, src any) error {
	if src == nil {
		dst.SetZero()
		return nil
	}
	val, ok := optionParts(dst)
	if err := assign(val, src); err != nil {
		return err
	}
	ok.SetBool(true)
	return nil
}

// setOption puts a value a user gave into an Option: an Option as is,
// nil as None, else Some(v).
func setOption(dst reflect.Value, v any) error {
	if v == nil {
		dst.SetZero()
		return nil
	}
	if reflect.TypeOf(v) == dst.Type() {
		dst.Set(reflect.ValueOf(v))
		return nil
	}
	val, ok := optionParts(dst)
	if err := setField(val, v); err != nil {
		return err
	}
	ok.SetBool(true)
	return nil
}

// argValue is v as a statement argument: an orm.FK's key, an Option's
// value (nil for None), anything else as is.
func argValue(v any) any {
	switch x := v.(type) {
	case nil, string, int, int64, bool, float64:
		return v
	case columnValuer:
		return x.columnValue()
	}
	if rv := reflect.ValueOf(v); isOption(rv.Type()) {
		return optionValue(rv)
	}
	return v
}

// columnValuer is a field type that is a column holding one value: orm.FK.
type columnValuer interface{ columnValue() any }
