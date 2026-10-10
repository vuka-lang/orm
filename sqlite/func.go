package sqlite

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"time"

	gosqlite "github.com/glebarez/go-sqlite"
)

// Option shapes a Func.
type Option func(*funcOptions)

type funcOptions struct{ volatile bool }

// Volatile registers a function whose result may differ for the same
// arguments (a clock, a random number): SQLite won't index it or fold its
// calls.
func Volatile() Option { return func(o *funcOptions) { o.volatile = true } }

// Func registers fn, a Go function, as the SQL function name on every
// SQLite connection opened after it — so register in an init, or before
// orm.Connect: connections already open don't get it. It is deterministic
// unless Volatile.
//
// fn's parameters are its SQL arguments, its last one variadic or not:
// string, []byte, bool, the integer and float kinds, time.Time (read from
// SQLite's text), any (what SQLite has: int64, float64, string, []byte or
// nil), or a pointer to one of them, nil for NULL. A NULL passed to a
// parameter that isn't a pointer or any makes the call NULL without
// running fn, as SQL's own functions are. It returns one value of those
// kinds (a driver.Valuer too, a nil pointer for NULL), and an error after
// it if it can fail.
//
//	func init() {
//		sqlite.MustFunc("editdist", func(a, b string) int { … })
//	}
//	var EditDist = orm.OrderedFunc[int]("editdist",
//		orm.Template("sqlite", "editdist({0}, {1})"), orm.Template("postgres", "levenshtein({0}, {1})"))
//
//	Users.Filter(EditDist(orm.F("name"), "jon").Le(1)).Order(EditDist(orm.F("name"), "jon").Asc())
//
// Postgres and MySQL run functions written in SQL: create them with the
// migration package's CreateFunction, and give the orm function a
// Template per dialect when the names differ.
func Func(name string, fn any, opts ...Option) error {
	var o funcOptions
	for _, opt := range opts {
		opt(&o)
	}
	call, n, err := adapt(name, fn)
	if err != nil {
		return err
	}
	if o.volatile {
		return gosqlite.RegisterScalarFunction(name, n, call)
	}
	return gosqlite.RegisterDeterministicScalarFunction(name, n, call)
}

// MustFunc is Func, panicking on its error.
func MustFunc(name string, fn any, opts ...Option) {
	if err := Func(name, fn, opts...); err != nil {
		panic(err)
	}
}

var (
	errType    = reflect.TypeFor[error]()
	timeType   = reflect.TypeFor[time.Time]()
	valuerType = reflect.TypeFor[driver.Valuer]()
)

// adapt is fn as the driver calls it, and its number of arguments (-1
// for any).
func adapt(name string, fn any) (func(*gosqlite.FunctionContext, []driver.Value) (driver.Value, error), int32, error) {
	fv := reflect.ValueOf(fn)
	ft := fv.Type()
	if fv.Kind() != reflect.Func || fv.IsNil() {
		return nil, 0, fmt.Errorf("sqlite: Func %q: want a func, got %T", name, fn)
	}
	if ft.NumOut() == 0 || ft.NumOut() > 2 || ft.NumOut() == 2 && ft.Out(1) != errType || !result(ft.Out(0)) {
		return nil, 0, fmt.Errorf("sqlite: Func %q: %s must return a value, and an error after it if it can fail", name, ft)
	}
	params := make([]reflect.Type, ft.NumIn())
	for i := range params {
		params[i] = ft.In(i)
		if ft.IsVariadic() && i == len(params)-1 {
			params[i] = params[i].Elem()
		}
		if !param(params[i]) {
			return nil, 0, fmt.Errorf("sqlite: Func %q: parameter %d of %s: %s isn't an SQL value", name, i+1, ft, params[i])
		}
	}
	n := int32(len(params))
	if ft.IsVariadic() {
		n = -1
	}
	call := func(_ *gosqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) < len(params)-1 || !ft.IsVariadic() && len(args) != len(params) {
			return nil, fmt.Errorf("%s takes %d arguments, got %d", name, len(params), len(args))
		}
		in := make([]reflect.Value, len(args))
		for i, a := range args {
			t := params[min(i, len(params)-1)]
			v, null, err := goValue(a, t)
			if err != nil {
				return nil, fmt.Errorf("%s: argument %d: %w", name, i+1, err)
			}
			if null {
				return nil, nil
			}
			in[i] = v
		}
		out := fv.Call(in)
		if len(out) == 2 && !out[1].IsNil() {
			return nil, out[1].Interface().(error)
		}
		return sqlValue(out[0])
	}
	return call, n, nil
}

func scalar(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return t == timeType
}

func param(t reflect.Type) bool {
	if t.Kind() == reflect.Interface {
		return t.NumMethod() == 0
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return scalar(t)
}

func result(t reflect.Type) bool {
	return t.Implements(valuerType) || param(t)
}

// goValue is an SQLite value as a t; null when it is NULL and t can't
// hold that.
func goValue(a driver.Value, t reflect.Type) (_ reflect.Value, null bool, _ error) {
	switch {
	case t.Kind() == reflect.Interface:
		if a == nil {
			return reflect.Zero(t), false, nil
		}
		return reflect.ValueOf(a), false, nil
	case a == nil && t.Kind() == reflect.Pointer:
		return reflect.Zero(t), false, nil
	case a == nil:
		return reflect.Value{}, true, nil
	case t.Kind() == reflect.Pointer:
		v, null, err := goValue(a, t.Elem())
		if err != nil || null {
			return v, null, err
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(v)
		return p, false, nil
	}
	v := reflect.New(t).Elem()
	s, isText := text(a).(string)
	switch {
	case t == timeType:
		if !isText {
			return v, false, fmt.Errorf("%v (%T) is not a time", a, a)
		}
		for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999 -0700 MST", time.DateOnly} {
			if tm, err := time.Parse(layout, s); err == nil {
				v.Set(reflect.ValueOf(tm))
				return v, false, nil
			}
		}
		return v, false, fmt.Errorf("%q is not a time", s)
	case t.Kind() == reflect.String:
		v.SetString(fmt.Sprint(text(a)))
	case t.Kind() == reflect.Slice:
		if isText {
			v.SetBytes([]byte(s))
		} else if b, ok := a.([]byte); ok {
			v.SetBytes(b)
		} else {
			v.SetBytes([]byte(fmt.Sprint(a)))
		}
	case t.Kind() == reflect.Bool:
		switch x := a.(type) {
		case int64:
			v.SetBool(x != 0)
		case float64:
			v.SetBool(x != 0)
		default:
			b, err := strconv.ParseBool(fmt.Sprint(text(a)))
			if err != nil {
				return v, false, fmt.Errorf("%q is not a bool", fmt.Sprint(text(a)))
			}
			v.SetBool(b)
		}
	case v.CanInt() || v.CanUint():
		var n int64
		switch x := a.(type) {
		case int64:
			n = x
		case float64:
			if x != float64(int64(x)) {
				return v, false, fmt.Errorf("%v is not a whole number", x)
			}
			n = int64(x)
		default:
			var err error
			if n, err = strconv.ParseInt(fmt.Sprint(text(a)), 10, 64); err != nil {
				return v, false, fmt.Errorf("%q is not an integer", fmt.Sprint(text(a)))
			}
		}
		if v.CanInt() {
			if v.OverflowInt(n) {
				return v, false, fmt.Errorf("%d overflows %s", n, t)
			}
			v.SetInt(n)
		} else {
			if n < 0 || v.OverflowUint(uint64(n)) {
				return v, false, fmt.Errorf("%d overflows %s", n, t)
			}
			v.SetUint(uint64(n))
		}
	case v.CanFloat():
		switch x := a.(type) {
		case int64:
			v.SetFloat(float64(x))
		case float64:
			v.SetFloat(x)
		default:
			f, err := strconv.ParseFloat(fmt.Sprint(text(a)), 64)
			if err != nil {
				return v, false, fmt.Errorf("%q is not a number", fmt.Sprint(text(a)))
			}
			v.SetFloat(f)
		}
	}
	return v, false, nil
}

// sqlValue is a Go function's result as SQLite takes it.
func sqlValue(v reflect.Value) (driver.Value, error) {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return nil, nil
	}
	if vr, ok := v.Interface().(driver.Valuer); ok {
		return vr.Value()
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return sqlValue(v.Elem())
	case reflect.Bool:
		if v.Bool() {
			return int64(1), nil
		}
		return int64(0), nil
	case reflect.String:
		return v.String(), nil
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Bytes(), nil
		}
	case reflect.Float32, reflect.Float64:
		return v.Float(), nil
	}
	switch {
	case v.CanInt():
		return v.Int(), nil
	case v.CanUint():
		return int64(v.Uint()), nil
	case v.Type() == timeType:
		return v.Interface().(time.Time).UTC().Format("2006-01-02 15:04:05.999999999-07:00"), nil
	}
	return nil, fmt.Errorf("%s is not an SQL value", v.Type())
}
