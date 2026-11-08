package orm

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Rows as a form's choices. A form editing an orm.FK[T] field shows T's
// rows as its options, and a <Field>Choices method may return a query (a
// QuerySet, a Manager's, Post.Objects.Filter(…)) instead of a list: its
// rows are the options, and the allow-list a submitted key is checked
// against. The form library reads them through these methods, by their
// shape: it doesn't import the ORM, nor the ORM it.
//
// A row's label is its String() when its model has one, else its label
// field's value: the first text field named Name, Title or Label, else
// its first text field. A search (a widget searching a large table)
// matches the label field, ignoring case.

// FormChoices are the query's rows as a form's choices: each row's key as
// text, its label and the row itself (a T). search, when not empty, keeps
// the rows whose label field holds it, ignoring case; limit, when above
// 0, caps the rows read.
func (c querySetCore) FormChoices(ctx context.Context, search string, limit int) (keys, labels []string, rows []any, err error) {
	if c.m == nil || c.m.err != nil {
		if c.m != nil {
			err = c.m.err
		}
		return nil, nil, nil, cmpErr(err, fmt.Errorf("orm: FormChoices of an unset query"))
	}
	q := c.q
	if search = strings.TrimSpace(search); search != "" {
		lf := labelField(c.m.meta)
		if lf == nil {
			return nil, nil, nil, fmt.Errorf("orm: %s has no text field to search: give it a Name, Title or Label", c.m.meta.Name)
		}
		q = q.filter([]Cond{Q{lf.Name + "__icontains": search}})
	}
	if limit > 0 {
		q = q.limited(limit)
	}
	out, err := c.m.rows(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	v := reflect.ValueOf(out)
	for i := 0; i < v.Len(); i++ {
		row := v.Index(i)
		k, l := rowChoice(c.m.meta, row)
		keys, labels, rows = append(keys, k), append(labels, l), append(rows, row.Interface())
	}
	return keys, labels, rows, nil
}

// FormChoice is the query's row whose key is key, the text a form sent:
// ok false when the query holds no such row (or key isn't one), which a
// form reports as a choice not offered.
func (c querySetCore) FormChoice(ctx context.Context, key string) (label string, row any, ok bool, err error) {
	if c.m == nil || c.m.err != nil {
		if c.m != nil {
			err = c.m.err
		}
		return "", nil, false, cmpErr(err, fmt.Errorf("orm: FormChoice of an unset query"))
	}
	pk := c.m.meta.PK
	if pk == nil {
		return "", nil, false, fmt.Errorf("orm: %s has no primary key to choose its rows by", c.m.meta.Name)
	}
	k, perr := parseKey(pk.Type, key)
	if perr != nil {
		return "", nil, false, nil
	}
	out, err := c.m.rows(ctx, c.q.filter([]Cond{Q{pk.Name: k}}).limited(2))
	if err != nil {
		return "", nil, false, err
	}
	v := reflect.ValueOf(out)
	if v.Len() != 1 {
		return "", nil, false, nil
	}
	_, l := rowChoice(c.m.meta, v.Index(0))
	return l, v.Index(0).Interface(), true, nil
}

func cmpErr(err, dflt error) error {
	if err != nil {
		return err
	}
	return dflt
}

// rowChoice is a row's key as text and its label.
func rowChoice(m *model, row reflect.Value) (string, string) {
	k := ""
	if m.PK != nil {
		k = fmt.Sprint(key(row, m.PK))
	}
	if row.CanAddr() {
		if s, ok := row.Addr().Interface().(fmt.Stringer); ok {
			return k, s.String()
		}
	}
	if s, ok := row.Interface().(fmt.Stringer); ok {
		return k, s.String()
	}
	if lf := labelField(m); lf != nil {
		if v := peek(row, lf.Index, lf.Type); v.IsValid() {
			if s := fmt.Sprint(value(v)); s != "" && s != "<nil>" {
				return k, s
			}
		}
	}
	return k, k
}

// labelField is a model's label field: the first text field named Name,
// Title or Label, else its first text field; nil when it has none.
func labelField(m *model) *field {
	text := func(f *field) bool {
		t := f.Type
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if isOptionType(t) {
			t = t.Field(0).Type
		}
		return t.Kind() == reflect.String && !f.PK && f.Via == ""
	}
	for _, name := range []string{"Name", "Title", "Label"} {
		for _, f := range m.Fields {
			if f.Name == name && text(f) {
				return f
			}
		}
	}
	for _, f := range m.Fields {
		if text(f) {
			return f
		}
	}
	return nil
}

// isOptionType reports whether t is a vuka.Option.
func isOptionType(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && strings.HasPrefix(t.Name(), "Option[") && strings.HasSuffix(t.PkgPath(), "vuka/v2")
}

// parseKey is key, a key's text, as a value of t, a primary key's type.
func parseKey(t reflect.Type, key string) (any, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("orm: an empty key")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(key, 10, 64)
		return n, err
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(key, 10, 64)
		return int64(n), err
	case reflect.String:
		return key, nil
	}
	return key, nil
}

// MarshalText is the FK's key as a form's control holds it: "" when unset.
func (f FK[T]) MarshalText() ([]byte, error) {
	k := f.columnValue()
	if k == nil {
		return nil, nil
	}
	return []byte(fmt.Sprint(k)), nil
}

// UnmarshalText makes the FK refer to the row whose key is text, a form's
// value: an empty text unsets it, and a text that is no key of T's type
// is an error.
func (f *FK[T]) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*f = FK[T]{}
		return nil
	}
	m, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return err
	}
	if m.PK == nil {
		return fmt.Errorf("orm: %s has no primary key", m.Name)
	}
	k, err := parseKey(m.PK.Type, s)
	if err != nil {
		return fmt.Errorf("orm: %q is no key of %s", s, m.Name)
	}
	*f = FK[T]{key: k}
	return nil
}

// FormRows is every row of T, the choices of a form's FK field that has no
// <Field>Choices method of its own.
func (FK[T]) FormRows() any { return allRows{reflect.TypeFor[T]()} }

// allRows is every row of a model, as a form's choices: it names no
// Manager[T], so a model with an FK compiles none.
type allRows struct{ t reflect.Type }

func (a allRows) core() querySetCore {
	registerType(a.t)
	m := homeCore(a.t)
	return querySetCore{m: m, q: m.all}
}

func (a allRows) FormChoices(ctx context.Context, search string, limit int) (keys, labels []string, rows []any, err error) {
	return a.core().FormChoices(ctx, search, limit)
}

func (a allRows) FormChoice(ctx context.Context, key string) (label string, row any, ok bool, err error) {
	return a.core().FormChoice(ctx, key)
}

// FormChildren are the rows of child, a model whose field (its Go name)
// is an FK to T, that refer to the T whose key is key: a form's rows of
// the record it edits (ui.Rows), in key order.
func (FK[T]) FormChildren(ctx context.Context, child reflect.Type, field string, key string) ([]any, error) {
	parent, err := modelOf(reflect.TypeFor[T](), "", "")
	if err != nil {
		return nil, err
	}
	if parent.PK == nil {
		return nil, fmt.Errorf("orm: %s has no primary key", parent.Name)
	}
	k, err := parseKey(parent.PK.Type, key)
	if err != nil {
		return nil, fmt.Errorf("orm: %q is no key of %s", key, parent.Name)
	}
	registerType(child)
	m := homeCore(child)
	if m.err != nil {
		return nil, m.err
	}
	q := m.all.filter([]Cond{Q{field: k}})
	if m.meta.PK != nil {
		q = q.orderBy([]any{m.meta.PK.Name})
	}
	out, err := m.rows(ctx, q)
	if err != nil {
		return nil, err
	}
	v := reflect.ValueOf(out)
	rows := make([]any, v.Len())
	for i := range rows {
		rows[i] = v.Index(i).Interface()
	}
	return rows, nil
}

// FormAtomic runs fn in a transaction (Atomic): a form saving its record
// and its rows.
func (FK[T]) FormAtomic(ctx context.Context, fn func(context.Context) error) error {
	return Atomic(ctx, fn)
}
