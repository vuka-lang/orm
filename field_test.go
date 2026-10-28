package orm_test

import (
	"slices"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

func TestTypedFields(t *testing.T) {
	ctx := relOpen(t)
	bs, err := Books.Filter(BookColumns.Author.Name.Eq("Ali"), BookColumns.Title.IContains("go")).All(ctx)
	if err != nil || !slices.Equal(titles(bs), []string{"Go in Practice"}) {
		t.Fatalf("fk: %v, %v", titles(bs), err)
	}
	as, err := Authors.Filter(AuthorColumns.Books.Tags().Name.In("poetry", "none")).OrderBy(AuthorColumns.Name.Desc()).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali"}) {
		t.Fatalf("reverse then m2m: %v, %v", authorNames(as), err)
	}
	as, err = Authors.Filter(AuthorColumns.ProfileID.IsNull(true), AuthorColumns.ID.Range(1, 2)).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema"}) {
		t.Fatalf("nullable and range: %v, %v", authorNames(as), err)
	}
	as, err = Authors.Filter(AuthorColumns.ID.InQuery(Books.Filter(BookColumns.Published.Eq(true)).Values[int64](BookColumns.AuthorID.Name()))).OrderBy(AuthorColumns.ID.Asc()).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali", "Neema"}) {
		t.Fatalf("in a subquery: %v, %v", authorNames(as), err)
	}
	n, err := Books.Filter(orm.Not(BookColumns.Author.Profile().Bio.StartsWith("a poet"))).Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("two hops: %d, %v", n, err)
	}
	as, err = Authors.Exclude(AuthorColumns.ProfileID.Eq(1)).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema", "Juma"}) {
		t.Fatalf("exclude keeps NULLs: %v, %v", authorNames(as), err)
	}
	if BookColumns.Author.Profile().Bio.Name() != "author__profile__bio" {
		t.Fatal(BookColumns.Author.Profile().Bio.Name())
	}
}
