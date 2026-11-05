package orm_test

import (
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// TestRowPromoted is a model's writes through its orm.Model's own methods,
// as code calling them on the embedded field (or built without the methods
// orm generate writes) does: the same as the model's generated methods,
// which go through orm.Row.
func TestRowPromoted(t *testing.T) {
	ctx := modelOpen(t)
	c := Customer{Email: "ali@x"}
	if err := c.Model.Save(ctx); err != nil || c.ID == 0 {
		t.Fatalf("insert: id %d, %v", c.ID, err)
	}
	c.Email = "ali@y"
	if err := c.Model.Save(ctx); err != nil {
		t.Fatal(err)
	}
	c.Visits = 4
	if err := c.Model.SaveFields(ctx, "visits"); err != nil {
		t.Fatal(err)
	}
	var back Customer
	back.ID = c.ID
	if err := orm.Row(&back).Refresh(ctx); err != nil || back.Email != "ali@y" || back.Visits != 4 {
		t.Fatalf("Refresh through Row: %+v, %v", back, err)
	}
	if n, _ := orm.Objects[Customer]().Count(ctx); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	if err := back.Model.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := orm.Objects[Customer]().Count(ctx); n != 0 {
		t.Fatalf("Delete left %d rows", n)
	}
}

type notModel struct{ ID int64 }

func TestRowErrors(t *testing.T) {
	ctx := modelOpen(t)
	if err := orm.Row(&notModel{}).Save(ctx); err == nil || !strings.Contains(err.Error(), "doesn't embed orm.Model") {
		t.Fatalf("Save of a struct without orm.Model: %v", err)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "a nil *orm_test.Customer") {
			t.Fatalf("Save of a nil row: %v", r)
		}
	}()
	var nilRow *Customer
	_ = nilRow.Save(ctx)
}
