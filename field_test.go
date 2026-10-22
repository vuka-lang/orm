package orm_test

import (
	"slices"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

func TestTypedFields(t *testing.T) {
	ctx := relOpen(t)
	bs, err := Books.Filter(BookFields.Author.Name.Eq("Ali"), BookFields.Title.IContains("go")).All(ctx)
	if err != nil || !slices.Equal(titles(bs), []string{"Go in Practice"}) {
		t.Fatalf("fk: %v, %v", titles(bs), err)
	}
	as, err := Authors.Filter(AuthorFields.Books.Tags().Name.In("poetry", "none")).OrderBy(AuthorFields.Name.Desc()).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali"}) {
		t.Fatalf("reverse then m2m: %v, %v", authorNames(as), err)
	}
	as, err = Authors.Filter(AuthorFields.ProfileID.IsNull(true), AuthorFields.ID.Range(1, 2)).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema"}) {
		t.Fatalf("nullable and range: %v, %v", authorNames(as), err)
	}
	as, err = Authors.Filter(AuthorFields.ID.InQuery(Books.Filter(BookFields.Published.Eq(true)).Values[int64](BookFields.AuthorID.Name()))).OrderBy(AuthorFields.ID.Asc()).All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Ali", "Neema"}) {
		t.Fatalf("in a subquery: %v, %v", authorNames(as), err)
	}
	n, err := Books.Filter(orm.Not(BookFields.Author.Profile().Bio.StartsWith("a poet"))).Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("two hops: %d, %v", n, err)
	}
	as, err = Authors.Exclude(AuthorFields.ProfileID.Eq(1)).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(authorNames(as), []string{"Neema", "Juma"}) {
		t.Fatalf("exclude keeps NULLs: %v, %v", authorNames(as), err)
	}
	if BookFields.Author.Profile().Bio.Name() != "author__profile__bio" {
		t.Fatal(BookFields.Author.Profile().Bio.Name())
	}
}
