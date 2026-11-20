package orm_test

import (
	"reflect"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// RecTag and RecPost embed orm.Rec: their row methods are the ones orm
// generate writes (rec_gen files), not Model's.
type RecTag struct {
	orm.Rec[RecTag]
	ID   int64
	Name string
}

type RecPost struct {
	orm.Rec[RecPost]
	ID    int64
	Title string
	Tags  []RecTag `gorm:"many2many:rec_post_tags;joinForeignKey:rec_post_id;joinReferences:rec_tag_id"`
}

func TestRecRowMethods(t *testing.T) {
	ctx := ormtest.Open(t, orm.Objects[RecPost](), orm.Objects[RecTag]())
	p := RecPost{Title: "one"}
	if err := p.Save(ctx); err != nil || p.ID == 0 {
		t.Fatalf("insert: id %d, %v", p.ID, err)
	}
	p.Title = "two"
	if err := p.Save(ctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := orm.Objects[RecPost]().Count(ctx); n != 1 {
		t.Fatalf("%d rows after the update, want 1", n)
	}
	p.Title = "three"
	if err := p.SaveFields(ctx, "title"); err != nil {
		t.Fatal(err)
	}
	back := RecPost{ID: p.ID}
	if err := back.Refresh(ctx); err != nil || back.Title != "three" {
		t.Fatalf("Refresh: %+v, %v", back, err)
	}

	read, err := orm.Objects[RecPost]().Get(ctx, orm.Q{"id": p.ID})
	if err != nil {
		t.Fatal(err)
	}
	read.Title = "four"
	if err := read.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := orm.Objects[RecPost]().Count(ctx); n != 1 {
		t.Fatalf("a row read is saved as an update: %d rows, want 1", n)
	}

	a, b := RecTag{Name: "a"}, RecTag{Name: "b"}
	if err := a.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := read.Add(ctx, "tags", a, b); err != nil {
		t.Fatal(err)
	}
	if err := read.Remove(ctx, "tags", a); err != nil {
		t.Fatal(err)
	}
	if err := read.Load(ctx, "tags"); err != nil || len(read.Tags) != 1 || read.Tags[0].Name != "b" {
		t.Fatalf("Load: %+v, %v", read.Tags, err)
	}
	if err := read.Set(ctx, "tags", a); err != nil {
		t.Fatal(err)
	}
	if err := read.Load(ctx, "tags"); err != nil || len(read.Tags) != 1 || read.Tags[0].Name != "a" {
		t.Fatalf("Set: %+v, %v", read.Tags, err)
	}
	if err := read.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := orm.Objects[RecPost]().Count(ctx); n != 0 {
		t.Fatalf("Delete left %d rows", n)
	}
}

// A model embedding Rec has no Model methods of its own to compile for it:
// the exported methods are the generated ones.
func TestRecHasOnlyGeneratedMethods(t *testing.T) {
	var p RecPost
	want := map[string]bool{"Using": true, "Save": true, "SaveFields": true, "Refresh": true, "Delete": true, "Load": true, "Add": true, "Remove": true, "Set": true, "DeclaredFieldAttrs": true}
	typ := reflectTypeOf(&p)
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; !want[name] {
			t.Errorf("unexpected method %s", name)
		}
	}
}

func reflectTypeOf(v any) reflect.Type { return reflect.TypeOf(v) }
