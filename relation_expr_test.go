package orm_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
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

func TestMultipleRelationAggregates(t *testing.T) {
	ctx := relOpen(t)
	books := orm.RelationAt("books")
	count := orm.Annotation[int64]("n", books.Count())
	q := Authors.Annotate(count).Annotate("total", books.Sum(orm.FieldAt[int64]("books__id"))).OrderBy("id")
	got, err := q.Values[[]any]("name", "n", "total").All(ctx)
	want := [][]any{{"Ali", int64(2), int64(3)}, {"Neema", int64(1), int64(3)}, {"Juma", int64(0), nil}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("multiple aggregates: %v, %v", got, err)
	}
	s, _, err := q.Values[[]any]("name", "n", "total").SQL(ctx)
	if err != nil || strings.Count(s, "SELECT COUNT(DISTINCT") != 1 || !strings.Contains(s, "SELECT DISTINCT") {
		t.Fatalf("planner: %s, %v", s, err)
	}
	got, err = Authors.Annotate(count).Filter(count.Gt(0)).Order(orm.Desc("n")).Values[[]any]("name", "n").All(ctx)
	if err != nil || len(got) != 2 || got[0][0] != "Ali" {
		t.Fatalf("typed HAVING: %v, %v", got, err)
	}
	_, _, err = Authors.Annotate(count).Filter(orm.Ref[int]("nn").Gt(0)).Values[string]("name").SQL(ctx)
	if err == nil || (!strings.Contains(err.Error(), "unknown annotation") || !strings.Contains(err.Error(), "did you mean")) {
		t.Fatalf("unknown annotation: %v", err)
	}
}

func (p BookFields) PublishedOnly() orm.Cond { return p.Published.Eq(true) }

func TestRelativeModelConditions(t *testing.T) {
	ctx := relOpen(t)
	root, err := Books.Filter(BookColumns.PublishedOnly()).OrderBy("id").Values[int64]("id").All(ctx)
	if err != nil || !reflect.DeepEqual(root, []int64{1, 3}) {
		t.Fatalf("root condition = %v, %v", root, err)
	}
	books := AuthorColumns.Books()
	got, err := Authors.Annotate("n", books.Where(books.PublishedOnly()).Count()).OrderBy("id").Values[int64]("n").All(ctx)
	if err != nil || !reflect.DeepEqual(got, []int64{1, 1, 0}) {
		t.Fatalf("relative condition = %v, %v", got, err)
	}
	ormtest.Exec(t, ctx, `INSERT INTO book_tags (book_id,tag_id) VALUES (2,1)`)
	// The same tag reached through two books still counts once.
	tags := orm.RelationAt("books__tags")
	got, err = Authors.Annotate("n", tags.Count()).OrderBy("id").Values[int64]("n").All(ctx)
	if err != nil || !reflect.DeepEqual(got, []int64{2, 1, 0}) {
		t.Fatalf("chain distinct = %v, %v", got, err)
	}
	// Separate relation aggregates keep sums independent of the tag join.
	rows, err := Authors.Annotate("tags", tags.Count()).Annotate("sum", books.Sum(BookFieldsAt("books__").ID)).OrderBy("id").Values[[]any]("tags", "sum").All(ctx)
	want := [][]any{{int64(2), int64(3)}, {int64(1), int64(3)}, {int64(0), nil}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("overlapping aggregates = %v, %v", rows, err)
	}
}

func TestRawRelationRows(t *testing.T) {
	ctx := relOpen(t)
	rel := orm.RelationAt("books")
	got, err := Authors.Annotate("n", rel.CountRows()).OrderBy("id").Values[int64]("n").All(ctx)
	if err != nil || !reflect.DeepEqual(got, []int64{2, 1, 1}) {
		t.Fatalf("raw joined rows = %v, %v", got, err)
	}
}

func TestRelationValueAggregates(t *testing.T) {
	ctx := relOpen(t)
	rel := orm.RelationAt("books")
	id := BookFieldsAt("books__").ID
	for _, c := range []struct {
		e    orm.Expr
		want []any
	}{
		{rel.Avg(id), []any{1.5, float64(3), nil}},
		{rel.Min(id), []any{int64(1), int64(3), nil}},
		{rel.Max(id), []any{int64(2), int64(3), nil}},
	} {
		got, err := Authors.Annotate("v", c.e).OrderBy("id").Values[any]("v").All(ctx)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("value aggregate = %v, %v; want %v", got, err, c.want)
		}
	}
	loc := time.FixedZone("local", 14*3600)
	got, err := Authors.Annotate("local_date", orm.TodayIn(loc)).Values[string]("local_date").All(ctx)
	if err != nil || len(got) != 3 || got[0] != orm.Clock().In(loc).Format("2006-01-02") {
		t.Fatalf("TodayIn = %v, %v", got, err)
	}
}
