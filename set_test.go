package orm_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// TestSets runs a model's set, the one orm generate writes, beside its
// QuerySet: the same rows, the same errors, the same statements.
func TestSets(t *testing.T) {
	ctx := ormtest.Open(t, RecPostQueries, RecTagQueries) // a set is an AnyManager
	posts, qs := RecPostQueries, orm.Objects[RecPost]()

	if err := posts.Create(ctx, &RecPost{Title: "b"}); err != nil {
		t.Fatal(err)
	}
	rows := []*RecPost{{Title: "a"}, {Title: "c"}, {Title: "d"}}
	if err := posts.BulkCreate(ctx, rows); err != nil || rows[0].ID == 0 {
		t.Fatalf("BulkCreate: %v, id %d", err, rows[0].ID)
	}
	if n, err := posts.Count(ctx); err != nil || n != 4 {
		t.Fatalf("Count: %d, %v", n, err)
	}

	got, err := posts.Filter(orm.Q{"title__in": []string{"a", "b", "c"}}).Exclude(orm.Q{"title": "c"}).OrderBy("-title").All(ctx)
	want, err2 := qs.Filter(orm.Q{"title__in": []string{"a", "b", "c"}}).Exclude(orm.Q{"title": "c"}).OrderBy("-title").All(ctx)
	if err != nil || err2 != nil || !slices.EqualFunc(got, want, func(a, b RecPost) bool { return a.ID == b.ID && a.Title == b.Title }) || len(got) != 2 {
		t.Fatalf("All: %+v, %v; QuerySet %+v, %v", got, err, want, err2)
	}
	s1, a1, _ := posts.Filter(orm.Q{"title": "a"}).OrderBy("id").Limit(3).Offset(1).SQL(ctx)
	s2, a2, _ := qs.Filter(orm.Q{"title": "a"}).OrderBy("id").Limit(3).Offset(1).SQL(ctx)
	if s1 != s2 || len(a1) != len(a2) {
		t.Errorf("SQL: %q %v; QuerySet %q %v", s1, a1, s2, a2)
	}
	empty, err := posts.Filter(orm.Q{"title": "none"}).All(ctx)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("All of nothing: %#v, %v; want an empty slice", empty, err)
	}

	first, err := posts.OrderBy("title").First(ctx)
	if err != nil || first.Title != "a" {
		t.Errorf("First: %+v, %v", first, err)
	}
	if first, err := posts.First(ctx); err != nil || first.Title != "b" {
		t.Errorf("First by primary key: %+v, %v", first, err)
	}
	if p, err := posts.Filter(orm.Q{"title": "none"}).First(ctx); !errors.Is(err, orm.ErrNotFound) || p.ID != 0 {
		t.Errorf("First of nothing: %+v, %v", p, err)
	}
	if p, err := posts.Get(ctx, orm.Q{"title": "c"}); err != nil || p.ID != rows[1].ID {
		t.Errorf("Get: %+v, %v", p, err)
	}
	if p, err := posts.Get(ctx, orm.Q{"title": "none"}); !errors.Is(err, orm.ErrNotFound) || p.ID != 0 {
		t.Errorf("Get of nothing: %+v, %v", p, err)
	}
	if p, err := posts.Get(ctx); !errors.Is(err, orm.ErrMultipleFound) || p.ID != 0 {
		t.Errorf("Get of several: %+v, %v", p, err)
	}

	var titles []string
	for p, err := range posts.OrderBy("title").Iter(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		titles = append(titles, p.Title)
	}
	if !slices.Equal(titles, []string{"a", "b", "c", "d"}) {
		t.Errorf("Iter: %v", titles)
	}
	for range posts.Iter(ctx) {
		break // stopping early closes the rows
	}
	vals, err := posts.OrderBy("title").Values[string]("title").All(ctx)
	if err != nil || !slices.Equal(vals, titles) {
		t.Errorf("Values: %v, %v", vals, err)
	}
	if ok, err := posts.Filter(orm.Q{"title": "d"}).Exists(ctx); err != nil || !ok {
		t.Errorf("Exists: %v, %v", ok, err)
	}

	if n, err := posts.Filter(orm.Q{"title": "d"}).Update(ctx, orm.Set{"title": "e"}); err != nil || n != 1 {
		t.Errorf("Update: %d, %v", n, err)
	}
	if _, err := posts.Update(ctx, orm.Set{"title": "x"}); !errors.Is(err, orm.ErrUnfiltered) {
		t.Errorf("Update of every row: %v, want ErrUnfiltered", err)
	}
	p, created, err := posts.GetOrCreate(ctx, orm.Q{"title": "f"}, nil)
	if err != nil || !created || p.ID == 0 {
		t.Errorf("GetOrCreate: %+v, %v, %v", p, created, err)
	}
	p, created, err = posts.UpdateOrCreate(ctx, orm.Q{"title": "f"}, orm.Defaults{"title": "g"})
	if err != nil || created || p.Title != "g" {
		t.Errorf("UpdateOrCreate: %+v, %v, %v", p, created, err)
	}
	p.Title = "h"
	if err := posts.Save(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if back, err := qs.Get(ctx, orm.Q{"id": p.ID}); err != nil || back.Title != "h" {
		t.Errorf("Save: %+v, %v", back, err)
	}
	if err := posts.Remove(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if n, err := posts.Filter(orm.Q{"title": "e"}).Delete(ctx); err != nil || n != 1 {
		t.Errorf("Delete: %d, %v", n, err)
	}
	if n, _ := qs.Count(ctx); n != 3 {
		t.Errorf("%d rows left, want 3", n)
	}

	// Relations PrefetchRelated names: All, First and Get load them.
	a, b := RecTag{Name: "a"}, RecTag{Name: "b"}
	if err := RecTagQueries.BulkCreate(ctx, []*RecTag{&a, &b}); err != nil {
		t.Fatal(err)
	}
	if err := rows[0].Add(ctx, "tags", a, b); err != nil {
		t.Fatal(err)
	}
	withTags := posts.Filter(orm.Q{"id": rows[0].ID}).PrefetchRelated("tags")
	all, err := withTags.All(ctx)
	if err != nil || len(all) != 1 || len(all[0].Tags) != 2 {
		t.Errorf("All, prefetched: %+v, %v", all, err)
	}
	if p, err := withTags.First(ctx); err != nil || len(p.Tags) != 2 {
		t.Errorf("First, prefetched: %+v, %v", p, err)
	}
	if p, err := withTags.Get(ctx); err != nil || len(p.Tags) != 2 {
		t.Errorf("Get, prefetched: %+v, %v", p, err)
	}
	if p, err := posts.PrefetchRelated("tags").Get(ctx, orm.Q{"title": "none"}); !errors.Is(err, orm.ErrNotFound) || p.ID != 0 {
		t.Errorf("Get of nothing, prefetched: %+v, %v", p, err)
	}

	// Conversions: a set as a QuerySet and back, the query kept.
	narrowed := orm.QuerySetOf[RecPost](posts.Filter(orm.Q{"title": "a"}).Core)
	if n, err := narrowed.Count(ctx); err != nil || n != 1 {
		t.Errorf("QuerySetOf: %d, %v", n, err)
	}
	back := RecPostQuery{orm.CoreFrom(qs.Filter(orm.Q{"title": "b"}))}
	if n, err := back.Count(ctx); err != nil || n != 1 {
		t.Errorf("CoreFrom: %d, %v", n, err)
	}
	if posts.Err() != nil || posts.TableName() != qs.TableName() || !slices.Equal(posts.Columns(), qs.Columns()) {
		t.Errorf("model: %v, %q, %v", posts.Err(), posts.TableName(), posts.Columns())
	}
	defer func() {
		if recover() == nil {
			t.Error("QuerySetOf of another model's query doesn't panic")
		}
	}()
	orm.QuerySetOf[RecTag](posts.Core)
}
