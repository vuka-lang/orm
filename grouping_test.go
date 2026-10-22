package orm_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

func TestGroupByHaving(t *testing.T) {
	ctx := relOpen(t) // Ali: 2 books, Neema: 1, Juma: none
	books := Authors.Annotate("n", orm.Count("books__id"))

	// Grouping by the key reads the columns depending on it ungrouped.
	got, err := books.GroupBy("id").OrderBy("id").Values[[]any]("id", "name", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(got, [][]any{{int64(1), "Ali", int64(2)}, {int64(2), "Neema", int64(1)}, {int64(3), "Juma", int64(0)}}) {
		t.Fatalf("GroupBy(id) = %v, %v", got, err)
	}
	names, err := books.GroupBy("id").Having(orm.Q{"n__gte": 1}).OrderBy("id").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(names, []string{"Ali", "Neema"}) {
		t.Fatalf("Having = %v, %v", names, err)
	}
	// A name GroupBy names that Values doesn't read.
	per, err := Books.Annotate("n", orm.Count("id")).GroupBy("author__name").OrderBy("-n").Values[int64]("n").All(ctx)
	if err != nil || !slices.Equal(per, []int64{2, 1}) {
		t.Fatalf("GroupBy a path = %v, %v", per, err)
	}
	// Having beside the implicit grouping, a plain condition in the same chain.
	kept, err := books.Exclude(orm.Q{"name": "Neema"}).Having(orm.Q{"n__gt": 0}).Values[[]any]("name", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(kept, [][]any{{"Ali", int64(2)}}) {
		t.Fatalf("Exclude + Having = %v, %v", kept, err)
	}

	// A filtered aggregate checks the row it counts: Ali's published book,
	// not "Ali has some published book".
	pub, err := Authors.Annotate("pub", orm.Count("books__id").Distinct().Filter(orm.Q{"books__published": true})).
		GroupBy("id").OrderBy("id").Values[int64]("pub").All(ctx)
	if err != nil || !slices.Equal(pub, []int64{1, 1, 0}) {
		t.Fatalf("filtered Count = %v, %v", pub, err)
	}
	// Counting the groups Having keeps: a grouped Values as a subquery.
	n, err := Authors.Filter(orm.Q{"id__in": books.GroupBy("id").Having(orm.Q{"n__gte": 1}).Values[int64]("id")}).Count(ctx)
	if err != nil || n != 2 {
		t.Fatalf("groups kept = %d, %v", n, err)
	}

	for _, c := range []struct {
		name string
		run  func() error
		want string
	}{
		{"Having on rows", func() error { _, err := Authors.Having(orm.Q{"id__gt": 0}).All(ctx); return err }, "has none"},
		{"Having on Count", func() error { _, err := books.Having(orm.Q{"n__gt": 0}).Count(ctx); return err }, "filters groups"},
		{"order folded away", func() error {
			_, err := books.OrderBy("id").Values[[]any]("name", "n").All(ctx)
			return err
		}, `ordered by "id", which the grouping folds away — this query groups by (name)`},
		{"HAVING on an ungrouped column", func() error {
			_, err := books.Having(orm.Or(orm.Q{"n__gt": 1}, orm.Q{"id": 3})).Values[[]any]("name", "n").All(ctx)
			return err
		}, `"id" no longer exists`},
		{"annotation did-you-mean", func() error {
			_, err := Authors.Annotate("total_books", orm.Count("books__id")).Filter(orm.Q{"totalBooks__gt": 0}).Values[string]("name").All(ctx)
			return err
		}, `the query annotates "total_books"`},
	} {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// TestValuesChain: the post-projection chain on Values[T,R] (Filter/Having/
// OrderBy/Limit, typed to R) matches the same ops placed before Values, and
// the result stays []R.
func TestValuesChain(t *testing.T) {
	ctx := relOpen(t) // Ali: 2 books, Neema: 1, Juma: 0
	books := Authors.Annotate("n", orm.Count("books__id"))

	// Having + OrderBy after Values == before Values.
	before, err := books.GroupBy("id").Having(orm.Q{"n__gte": 1}).OrderBy("id").Values[string]("name").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := books.GroupBy("id").Values[string]("name").Having(orm.Q{"n__gte": 1}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(after, before) {
		t.Fatalf("post-Values chain = %v, %v; want %v", after, err, before)
	}

	// Filter + OrderBy + Limit after Values, result typed []string.
	top, err := books.GroupBy("id").Values[string]("name").
		Filter(orm.Q{"n__gt": 0}).OrderBy("-n", "name").Limit(1).All(ctx)
	if err != nil || !slices.Equal(top, []string{"Ali"}) {
		t.Fatalf("Filter/OrderBy/Limit after Values = %v, %v", top, err)
	}

	// Annotate + Having after Values (the Django shape: project, then add the
	// aggregate the projection groups by and filter it).
	rows, err := Authors.GroupBy("id").Values[[]any]("name").
		Annotate("n", orm.Count("books__id")).Having(orm.Q{"n__gt": 0}).OrderBy("id").All(ctx)
	if err != nil || !reflect.DeepEqual(rows, [][]any{{"Ali"}, {"Neema"}}) {
		t.Fatalf("Annotate/Having after Values = %v, %v", rows, err)
	}
}
