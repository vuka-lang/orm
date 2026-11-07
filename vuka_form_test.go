package orm_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
	"github.com/vuka-lang/vuka/v2"
	"github.com/vuka-lang/vuka/v2/field"
)

type Gadget struct {
	orm.Model[Gadget]
	ID     int64
	Name   string
	SKU    string
	Price  int64
	Status string
	Notes  vuka.Option[string]
}

// placeholder is another library's attribute: Schema keeps it for
// vuka.FieldAttrs.
type placeholder string

func (p placeholder) Describe(s *field.Spec) { s.Placeholder = string(p) }

func (g *Gadget) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "form_gadgets"})
	s.Field(&g.ID, orm.PK{})
	s.Field(&g.Name, orm.Char{Max: 100, Unique: true, IgnoreCase: true, Trim: true}, placeholder("e.g. Desk lamp"))
	s.Field(&g.SKU, orm.Char{Max: 12, Pattern: `^[A-Z0-9-]+$`, Help: "Letters, digits and dashes", Unique: true})
	s.Field(&g.Price, orm.Int{Min: 0, Max: 1000})
	s.Field(&g.Status, orm.Char{Choices: []string{"draft", "live"}, Label: "State"})
	s.Field(&g.Notes, orm.LongText{})
}

var Gadgets = orm.For[Gadget]()

// TestDescribe: a model's attributes describe its fields to a form, other
// libraries' kept beside them.
func TestDescribe(t *testing.T) {
	attrs := vuka.FieldAttrs[Gadget]()
	spec := func(name string) field.Spec { return field.Describe(attrs[name]) }
	if s := spec("ID"); !s.Key {
		t.Errorf("ID: %+v", s)
	}
	if s := spec("Name"); s.Max == nil || *s.Max != 100 || !s.Trim || s.Placeholder != "e.g. Desk lamp" {
		t.Errorf("Name: %+v", s)
	}
	if s := spec("SKU"); s.Pattern == "" || s.Help != "Letters, digits and dashes" {
		t.Errorf("SKU: %+v", s)
	}
	if s := spec("Price"); s.Min == nil || *s.Min != 0 || s.Max == nil || *s.Max != 1000 {
		t.Errorf("Price: %+v", s)
	}
	if s := spec("Status"); s.Label != "State" || !reflect.DeepEqual(s.Choices, []field.Choice{{Value: "draft", Label: "draft"}, {Value: "live", Label: "live"}}) {
		t.Errorf("Status: %+v", s)
	}
	if s := spec("Notes"); s.Widget != "textarea" {
		t.Errorf("Notes: %+v", s)
	}
	if s := field.Describe([]any{orm.Datetime{AutoNowAdd: true}}); !s.Auto {
		t.Errorf("an AutoNowAdd time: %+v", s)
	}
}

// TestCheckUnique: a Unique column's check refuses a value another row
// holds, leaving the record's own row out once it is saved; IgnoreCase
// folds case.
func TestCheckUnique(t *testing.T) {
	ctx := ormtest.Open(t, Gadgets)
	lamp := &Gadget{Name: "Desk lamp", SKU: "DL-1", Status: "live"}
	ormtest.Seed(t, ctx, Gadgets, lamp, &Gadget{Name: "Kettle", SKU: "KT-1", Status: "live"})
	check := func(attr field.Checker, rec *Gadget, name string, v any) error {
		return attr.Check(ctx, field.Value{Name: name, Value: v, Record: rec})
	}
	name := orm.Char{Unique: true, IgnoreCase: true}
	sku := orm.Char{Unique: true}
	var invalid *field.Invalid
	if err := check(name, &Gadget{}, "Name", "desk LAMP"); !errors.As(err, &invalid) || invalid.Rule != "unique" {
		t.Errorf("a new row with a taken name, case folded: %v", err)
	}
	if err := check(sku, &Gadget{}, "SKU", "dl-1"); err != nil {
		t.Errorf("a new row with a free SKU (case kept): %v", err)
	}
	if err := check(name, lamp, "Name", "Desk lamp"); err != nil {
		t.Errorf("the row its own name: %v", err)
	}
	if err := check(sku, lamp, "SKU", "KT-1"); !errors.As(err, &invalid) {
		t.Errorf("the row another's SKU: %v", err)
	}
	if err := check(orm.Char{}, &Gadget{}, "SKU", "KT-1"); err != nil {
		t.Errorf("a column that isn't unique: %v", err)
	}
	if err := check(orm.Unique{}, &Gadget{}, "SKU", "KT-1"); !errors.As(err, &invalid) {
		t.Errorf("orm.Unique: %v", err)
	}
}
