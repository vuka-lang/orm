package orm_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

type Aisle struct {
	ID   int64
	Code string
	Name string
}

type Shelf struct {
	ID    int64
	Name  string
	Aisle orm.FK[Aisle]
}

// Sticker is a model with a String method: its label.
type Sticker struct {
	ID   int64
	Slug string
}

func (t Sticker) String() string { return "#" + t.Slug }

var (
	Aisles   = orm.For[Aisle]()
	Shelves  = orm.For[Shelf]()
	Stickers = orm.For[Sticker]()
)

// rowSource is how a form reads a query's rows as choices.
type rowSource interface {
	FormChoices(ctx context.Context, search string, limit int) (keys, labels []string, rows []any, err error)
	FormChoice(ctx context.Context, key string) (label string, row any, ok bool, err error)
}

// TestFormChoices: a query's rows as a form's choices, labelled by the
// label field or String, searched by the label field, capped; FormChoice
// finds a row only among the query's.
func TestFormChoices(t *testing.T) {
	ctx := ormtest.Open(t, Aisles, Shelves, Stickers)
	ormtest.Seed(t, ctx, Aisles, &Aisle{Code: "A1", Name: "Garden"}, &Aisle{Code: "B2", Name: "Kitchen"}, &Aisle{Code: "C3", Name: "Garage"})
	ormtest.Seed(t, ctx, Stickers, &Sticker{Slug: "new"})

	var src rowSource = Aisles.OrderBy("id")
	keys, labels, rows, err := src.FormChoices(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"1", "2", "3"}) || !reflect.DeepEqual(labels, []string{"Garden", "Kitchen", "Garage"}) {
		t.Fatalf("keys %v labels %v", keys, labels)
	}
	if r, ok := rows[1].(Aisle); !ok || r.Code != "B2" {
		t.Fatalf("row %#v", rows[1])
	}
	if _, labels, _, _ = src.FormChoices(ctx, "gar", 0); !reflect.DeepEqual(labels, []string{"Garden", "Garage"}) {
		t.Errorf("search gar: %v", labels)
	}
	if keys, _, _, _ = src.FormChoices(ctx, "", 2); len(keys) != 2 {
		t.Errorf("limit 2: %v", keys)
	}

	narrow := Aisles.Filter(orm.Q{"code__startswith": "B"})
	if l, row, ok, err := narrow.FormChoice(ctx, "2"); err != nil || !ok || l != "Kitchen" || row.(Aisle).ID != 2 {
		t.Errorf("FormChoice 2: %q %v %v %v", l, row, ok, err)
	}
	for _, k := range []string{"1", "9", "x", ""} {
		if _, _, ok, err := narrow.FormChoice(ctx, k); ok || err != nil {
			t.Errorf("FormChoice %q: %v %v", k, ok, err)
		}
	}

	if _, labels, _, _ := Stickers.QuerySet.FormChoices(ctx, "", 0); !reflect.DeepEqual(labels, []string{"#new"}) {
		t.Errorf("String labels: %v", labels)
	}
}

// TestFKText: an FK is its key as a form's text, and its FormRows every
// row of its model.
func TestFKText(t *testing.T) {
	ctx := ormtest.Open(t, Aisles, Shelves)
	ormtest.Seed(t, ctx, Aisles, &Aisle{Code: "A1", Name: "Garden"})
	var fk orm.FK[Aisle]
	if b, _ := fk.MarshalText(); len(b) != 0 {
		t.Errorf("unset: %q", b)
	}
	if err := fk.UnmarshalText([]byte("1")); err != nil || fk.ID() != 1 {
		t.Fatalf("1: %v %d", err, fk.ID())
	}
	if b, _ := fk.MarshalText(); string(b) != "1" {
		t.Errorf("text %q", b)
	}
	if err := fk.UnmarshalText([]byte("x")); err == nil {
		t.Error("x took")
	}
	if err := fk.UnmarshalText(nil); err != nil || fk.IsSet() {
		t.Errorf("empty: %v %v", err, fk.IsSet())
	}
	src, ok := fk.FormRows().(rowSource)
	if !ok {
		t.Fatalf("FormRows is %T", fk.FormRows())
	}
	if keys, _, _, err := src.FormChoices(ctx, "", 0); err != nil || !reflect.DeepEqual(keys, []string{"1"}) {
		t.Errorf("FormRows: %v %v", keys, err)
	}
}
