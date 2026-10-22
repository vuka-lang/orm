package orm_test

import (
	"slices"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

type AuthorStats struct {
	ID        int64
	Name      string
	Latest    string `orm:"computed"`
	BookCount int    `orm:"computed"`
}

var AuthorStatsQ = orm.For[AuthorStats](orm.Table("authors"))

func TestSubqueries(t *testing.T) {
	ctx := relOpen(t)

	published := Books.Filter(orm.Q{"published": true}).Values[int64]("author_id")
	as, err := Authors.Filter(orm.Q{"id__in": published}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali", "Neema"}) {
		t.Fatalf("__in a subquery = %v, %v", authorNames(as), err)
	}
	as, err = Authors.Exclude(orm.Q{"id__in": Books.Filter(orm.Q{"title__icontains": "poem"}).Values[int64]("author_id")}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema", "Juma"}) {
		t.Fatalf("not in = %v, %v", authorNames(as), err)
	}

	withBooks := orm.Exists(Books.Filter(orm.Q{"author_id": orm.OuterRef("id"), "published": true}))
	as, err = Authors.Filter(withBooks).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali", "Neema"}) {
		t.Fatalf("exists = %v, %v", authorNames(as), err)
	}
	as, err = Authors.Filter(orm.Not(orm.Exists(Books.Filter(orm.Q{"author_id": orm.OuterRef("id")})))).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Juma"}) {
		t.Fatalf("not exists = %v, %v", authorNames(as), err)
	}

	latest := Books.Filter(orm.Q{"author_id": orm.OuterRef("id")}).OrderBy("-id").Limit(1).Values[string]("title")
	count := orm.Function("count_books", orm.Template("", "(SELECT COUNT(*) FROM books b WHERE b.author_id = {0})"))
	stats, err := AuthorStatsQ.Annotate("latest", orm.Subquery(latest)).
		Annotate("book_count", count.Of(orm.F("id"))).OrderBy("-book_count", "name").All(ctx)
	if err != nil || len(stats) != 3 || stats[0] != (AuthorStats{1, "Ali", "Poems", 2}) || stats[2].Latest != "" {
		t.Fatalf("subquery annotations = %+v, %v", stats, err)
	}

	// A subquery of the same table, aliased apart.
	older := Authors.Filter(orm.Q{"id__lt": orm.OuterRef("id")})
	as, err = Authors.Filter(orm.Exists(older)).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema", "Juma"}) {
		t.Fatalf("self subquery = %v, %v", authorNames(as), err)
	}
	// Following a relation inside, reading the outer row.
	ts, err := Tags.Filter(orm.Exists(Books.Filter(orm.Q{"tags__id": orm.OuterRef("id"), "author__name": "Neema"}))).Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(ts, []string{"golang"}) {
		t.Fatalf("relation inside = %v, %v", ts, err)
	}
	if _, err := Authors.Filter(orm.Q{"id": orm.OuterRef("id")}).All(ctx); err == nil {
		t.Fatal("OuterRef outside a subquery was accepted")
	}
	if _, err := Authors.Filter(orm.Q{"id__in": Books.Values[int64]("id", "author_id")}).All(ctx); err == nil {
		t.Fatal("a two-column subquery was accepted")
	}
}
