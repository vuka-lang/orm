package orm_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

func TestRelationAggregates(t *testing.T) {
	ctx := relOpen(t)
	books := orm.RelationAt("books")
	q := Authors.Annotate("n", books.Where(orm.Q{"books__published": true}).Count()).GroupBy("id").OrderBy("id")
	got, err := q.Values[[]any]("name", "n").All(ctx)
	want := [][]any{{"Ali", int64(1)}, {"Neema", int64(1)}, {"Juma", int64(0)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %v, %v", got, err)
	}
	exists, err := Authors.Annotate("has", books.Exists()).OrderBy("id").Values[bool]("has").All(ctx)
	if err != nil || !reflect.DeepEqual(exists, []bool{true, true, false}) {
		t.Fatalf("exists = %v, %v", exists, err)
	}
	s, args, err := q.Values[[]any]("name", "n").SQL(ctx)
	if err != nil || !strings.Contains(s, "COUNT(DISTINCT CASE WHEN") || len(args) != 1 {
		t.Fatalf("SQL = %s %v %v", s, args, err)
	}
	for _, c := range []struct {
		e       orm.Expr
		message string
	}{
		{orm.RelationAt("profile").Count(), "to-one"},
		{books.Where(orm.Q{"name": "Ali"}).Count(), "another model"},
	} {
		_, _, err := Authors.Annotate("n", c.e).Values[int64]("n").SQL(ctx)
		if err == nil || !strings.Contains(err.Error(), c.message) {
			t.Errorf("error = %v, want %s", err, c.message)
		}
	}
	type Row struct {
		Name    string
		Missing string
	}
	_, _, err = Authors.Values[Row]("name").SQL(ctx)
	if err == nil || !strings.Contains(err.Error(), "Missing has no selected column") {
		t.Fatalf("mapping error = %v", err)
	}
}
