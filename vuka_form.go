package orm

import (
	"cmp"
	"context"
	"reflect"
	"strconv"

	vfield "github.com/vuka-lang/vuka/v2/field"
)

// The field attributes describe their fields to a form (vfield.Describer),
// so a form editing a model (ui.FormFor) takes its labels, limits and
// choices from the model: a Char's Max is the input's maxlength and a rule,
// its Choices a select's options, a LongText a textarea; a key and the
// times the ORM sets are never form fields. A unique column's value is
// checked against the table as the form is submitted (vfield.Checker).

func (x PK) Describe(s *vfield.Spec) {
	s.Key = true
	describeText(s, x.Label, x.Help)
}

func (x Char) Describe(s *vfield.Spec) {
	if x.Max > 0 {
		s.Max = vfield.Float(float64(x.Max))
	}
	if x.Min > 0 {
		s.Min = vfield.Float(float64(x.Min))
	}
	if x.Pattern != "" {
		s.Pattern = x.Pattern
	}
	if x.Choices != nil {
		s.Choices = make([]vfield.Choice, len(x.Choices))
		for i, c := range x.Choices {
			s.Choices[i] = vfield.Choice{Value: c, Label: c}
		}
	}
	s.Trim, s.Lower = s.Trim || x.Trim, s.Lower || x.Lower
	s.Optional = s.Optional || x.Null
	describeText(s, x.Label, x.Help)
}

// Check refuses a value another row holds when the column is Unique,
// ignoring case when IgnoreCase says so.
func (x Char) Check(ctx context.Context, v vfield.Value) error {
	if !x.Unique {
		return nil
	}
	return checkUnique(ctx, v, x.IgnoreCase)
}

func (x Int) Describe(s *vfield.Spec) {
	bound := func(v any) *float64 {
		rv := reflect.ValueOf(v)
		switch k := rv.Kind(); {
		case v == nil:
		case k >= reflect.Int && k <= reflect.Int64:
			return vfield.Float(float64(rv.Int()))
		case k >= reflect.Uint && k <= reflect.Uint64:
			return vfield.Float(float64(rv.Uint()))
		}
		return nil
	}
	if n := bound(x.Min); n != nil {
		s.Min = n
	} else if x.Unsigned && s.Min == nil {
		s.Min = vfield.Float(0)
	}
	if n := bound(x.Max); n != nil {
		s.Max = n
	}
	if x.Choices != nil {
		s.Choices = make([]vfield.Choice, len(x.Choices))
		for i, c := range x.Choices {
			v := strconv.FormatInt(c, 10)
			s.Choices[i] = vfield.Choice{Value: v, Label: v}
		}
	}
	s.Optional = s.Optional || x.Null
	describeText(s, x.Label, x.Help)
}

// Check refuses a value another row holds when the column is Unique.
func (x Int) Check(ctx context.Context, v vfield.Value) error {
	if !x.Unique {
		return nil
	}
	return checkUnique(ctx, v, false)
}

func (x DateField) Describe(s *vfield.Spec) {
	s.Auto = s.Auto || x.AutoNow || x.AutoNowAdd || x.DefaultNow
	s.Optional = s.Optional || x.Null
	describeText(s, x.Label, x.Help)
}

func (x Datetime) Describe(s *vfield.Spec) {
	s.Auto = s.Auto || x.AutoNow || x.AutoNowAdd || x.DefaultNow
	s.Optional = s.Optional || x.Null
	describeText(s, x.Label, x.Help)
}

func (LongText) Describe(s *vfield.Spec) { s.Max, s.Widget = nil, "textarea" }

func (x Slug) Describe(s *vfield.Spec) { s.Max = vfield.Float(float64(cmp.Or(x.Max, 50))) }

// Check refuses a slug another row holds when it is Unique.
func (x Slug) Check(ctx context.Context, v vfield.Value) error {
	if !x.Unique {
		return nil
	}
	return checkUnique(ctx, v, false)
}

// Check refuses a value another row holds.
func (Unique) Check(ctx context.Context, v vfield.Value) error { return checkUnique(ctx, v, false) }

func (Null) Describe(s *vfield.Spec)       { s.Optional = true }
func (AutoNow) Describe(s *vfield.Spec)    { s.Auto = true }
func (AutoNowAdd) Describe(s *vfield.Spec) { s.Auto = true }

func describeText(s *vfield.Spec, label, help string) {
	if label != "" {
		s.Label = label
	}
	if help != "" {
		s.Help = help
	}
}

// checkUnique refuses v's value when a row of its record's table other
// than the record holds it: the record's own row is left out once it is
// saved, so editing a row keeps its value.
func checkUnique(ctx context.Context, v vfield.Value, fold bool) error {
	rv := reflect.ValueOf(v.Record)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct || isZeroValue(v.Value) {
		return nil
	}
	m := homeCore(rv.Type().Elem())
	if m.err != nil {
		return m.err
	}
	loaded := false
	if m.meta.state != nil {
		if st := m.stateOf(rv.UnsafePointer()); st.m != nil {
			m, loaded = st.m, st.loaded
		} else {
			loaded = st.loaded
		}
	}
	f, ok := m.meta.field(v.Name)
	if !ok {
		return nil
	}
	lookup := f.Name
	if fold {
		lookup += "__iexact"
	}
	q := m.all.filter([]Cond{Q{lookup: v.Value}})
	if pk := m.meta.PK; loaded && pk != nil {
		q = q.exclude([]Cond{Q{pk.Name: value(peek(rv.Elem(), pk.Index, pk.Type))}})
	}
	taken, err := m.exists(ctx, q)
	if err != nil {
		return err
	}
	if taken {
		return &vfield.Invalid{Rule: "unique"}
	}
	return nil
}

func isZeroValue(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }
