package orm_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
)

type agePred struct{ min int }

func (a agePred) Pred() orm.Pred {
	return orm.Pred{Path: []string{"age"}, Op: orm.OpGte, Value: a.min}
}

func TestPred(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	names := func(qs orm.QuerySet[User]) []string {
		t.Helper()
		us, err := qs.All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range us {
			out = append(out, u.Name)
		}
		return out
	}
	p := orm.Pred{Op: orm.OpOr, Args: []orm.Pred{
		{Path: []string{"name"}, Op: orm.OpStartsWith, Value: "A"},
		{Op: orm.OpAnd, Args: []orm.Pred{
			{Path: []string{"age"}, Op: orm.OpGt, Value: 30},
			{Op: orm.OpNot, Args: []orm.Pred{{Path: []string{"bio"}, Op: orm.OpEq, Value: nil}}},
		}},
	}}
	q := orm.Or(orm.Q{"name__startswith": "A"}, orm.And(orm.Q{"age__gt": 30}, orm.Not(orm.Q{"bio": nil})))
	got, want := names(Users.Filter(p).OrderBy("id")), names(Users.Filter(q).OrderBy("id"))
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("pred %v, q %v", got, want)
	}
	if got, want := names(Users.Filter(orm.PredCond(agePred{30})).OrderBy(orm.Orderings(orm.Ordering{Path: []string{"age"}, Desc: true})...)),
		names(Users.Filter(orm.Q{"age__gte": 30}).OrderBy("-age")); !slices.Equal(got, want) {
		t.Fatalf("where %v, want %v", got, want)
	}
	if got, want := names(Users.Filter(orm.Pred{Path: []string{"name"}, Op: orm.OpNe, Value: "Ali"})), names(Users.Exclude(orm.Q{"name": "Ali"})); !slices.Equal(got, want) {
		t.Fatalf("ne %v, want %v", got, want)
	}
	for _, bad := range []orm.Pred{
		{Op: "like", Path: []string{"name"}},
		{Op: orm.OpEq},
		{Op: orm.OpEq, Path: []string{"name__icontains"}},
		{Op: orm.OpNot},
	} {
		if _, err := Users.Filter(bad).All(ctx); err == nil || !strings.Contains(err.Error(), "predicate") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestFieldOptions(t *testing.T) {
	type gizmo struct {
		ID    int64
		Label string
		State string
	}
	orm.SetFieldOptions(func(owner reflect.Type, f reflect.StructField) (orm.FieldOptions, bool) {
		if owner != reflect.TypeFor[gizmo]() {
			return orm.FieldOptions{}, false
		}
		switch f.Name {
		case "Label":
			return orm.FieldOptions{Column: "title", MaxLength: 40, Unique: true}, true
		case "State":
			return orm.FieldOptions{Default: "'draft'", Index: true}, true
		}
		return orm.FieldOptions{}, false
	})
	t.Cleanup(func() { orm.SetFieldOptions(nil) })
	gizmos := orm.For[gizmo]()
	if cols := gizmos.Columns(); !slices.Equal(cols, []string{"id", "title", "state"}) {
		t.Fatalf("columns %v", cols)
	}
	ctx := open(t)
	if err := orm.CreateTables(ctx, gizmos); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, "INSERT INTO gizmos (title) VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	g, err := gizmos.Get(ctx, orm.Q{"label": "a"})
	if err != nil || g.State != "draft" {
		t.Fatalf("%+v, %v", g, err)
	}
	if err := gizmos.Create(ctx, &gizmo{Label: "a"}); !errors.Is(err, orm.ErrConflict) {
		t.Fatalf("unique: %v", err)
	}
}
