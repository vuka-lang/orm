package orm_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

type Profile struct {
	ID  int64
	Bio string
}

// Author's relations are found by convention: Profile by ProfileID, Books
// by Book.AuthorID.
type Author struct {
	ID        int64
	Name      string
	ProfileID *int64
	Profile   *Profile
	Books     []Book
}

// Book's are tagged: Author with GORM's foreignKey, Tags with orm's m2m.
type Book struct {
	ID        int64
	Title     string
	AuthorID  int64
	Author    *Author `gorm:"foreignKey:AuthorID"`
	Published bool
	Tags      []Tag `orm:"m2m:book_tags"`
}

type Tag struct {
	ID    int64
	Name  string
	Books []*Book `orm:"m2m:book_tags,tag_id,book_id"`
}

var (
	Profiles = orm.For[Profile]()
	Authors  = orm.For[Author]()
	Books    = orm.For[Book]()
	Tags     = orm.For[Tag]()
)

var relData = []string{
	`INSERT INTO profiles (id, bio) VALUES (1, 'a poet from Dodoma')`,
	`INSERT INTO authors (id, name, profile_id) VALUES (1, 'Ali', 1), (2, 'Neema', NULL), (3, 'Juma', NULL)`,
	`INSERT INTO books (id, title, author_id, published) VALUES (1, 'Go in Practice', 1, TRUE), (2, 'Poems', 1, FALSE), (3, 'Learning Go', 2, TRUE)`,
	`INSERT INTO tags (id, name) VALUES (1, 'golang'), (2, 'poetry'), (3, 'unused')`,
	`INSERT INTO book_tags (book_id, tag_id) VALUES (1, 1), (3, 1), (2, 2)`,
}

func relOpen(t *testing.T) context.Context {
	t.Helper()
	ctx := open(t)
	for _, s := range relData {
		ormtest.Exec(t, ctx, s)
	}
	return ctx
}

func titles(bs []Book) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Title
	}
	return out
}

func authorNames(as []Author) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name
	}
	return out
}

func TestRelationFilters(t *testing.T) {
	ctx := relOpen(t)
	for _, c := range []struct {
		name string
		got  func() ([]string, error)
		want []string
	}{
		{"fk", func() ([]string, error) {
			bs, err := Books.Filter(orm.Q{"author__name": "Ali"}).OrderBy("id").All(ctx)
			return titles(bs), err
		}, []string{"Go in Practice", "Poems"}},
		{"fk by key", func() ([]string, error) {
			bs, err := Books.Filter(orm.Q{"author": 2}).All(ctx)
			return titles(bs), err
		}, []string{"Learning Go"}},
		{"nested fk", func() ([]string, error) {
			bs, err := Books.Filter(orm.Q{"author__profile__bio__icontains": "POET"}).OrderBy("id").All(ctx)
			return titles(bs), err
		}, []string{"Go in Practice", "Poems"}},
		{"reverse fk", func() ([]string, error) {
			as, err := Authors.Filter(orm.Q{"books__title__icontains": "go"}).OrderBy("id").All(ctx)
			return authorNames(as), err
		}, []string{"Ali", "Neema"}},
		{"reverse fk, none", func() ([]string, error) {
			as, err := Authors.Filter(orm.Q{"books__isnull": true}).All(ctx)
			return authorNames(as), err
		}, []string{"Juma"}},
		{"many-to-many", func() ([]string, error) {
			bs, err := Books.Filter(orm.Q{"tags__name": "golang"}).OrderBy("id").All(ctx)
			return titles(bs), err
		}, []string{"Go in Practice", "Learning Go"}},
		{"many-to-many then fk", func() ([]string, error) {
			ts, err := Tags.Filter(orm.Q{"books__author__name": "Ali"}).OrderBy("id").Values[string]("name").All(ctx)
			return ts, err
		}, []string{"golang", "poetry"}},
		{"order and values across fk", func() ([]string, error) {
			return Books.OrderBy("author__name", "-title").Values[string]("author__name").All(ctx)
		}, []string{"Ali", "Ali", "Neema"}},
		{"exclude across reverse fk", func() ([]string, error) {
			as, err := Authors.Exclude(orm.Q{"books__published": true}).All(ctx)
			return authorNames(as), err
		}, []string{"Juma"}},
	} {
		got, err := c.got()
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}

	if n, err := Books.Filter(orm.Q{"author__profile__isnull": false}).Count(ctx); n != 2 || err != nil {
		t.Errorf("count across fk = %d, %v", n, err)
	}
	if n, err := Books.Filter(orm.Q{"author__name": "Neema"}).Update(ctx, orm.Set{"published": false}); n != 1 || err != nil {
		t.Errorf("update across fk = %d, %v", n, err)
	}
	if n, err := Authors.Filter(orm.Q{"books__isnull": true}).Delete(ctx); n != 1 || err != nil {
		t.Errorf("delete across reverse fk = %d, %v", n, err)
	}
	if _, err := Books.OrderBy("tags").All(ctx); err == nil {
		t.Error("ordered by a many-to-many")
	}
	if _, err := Books.Filter(orm.Q{"tags": 1}).All(ctx); err == nil || !strings.Contains(err.Error(), "holds many rows") {
		t.Errorf("compared a many-to-many: %v", err)
	}
}

func TestSelectRelated(t *testing.T) {
	ctx := relOpen(t)
	bs, err := Books.SelectRelated("author__profile").OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bs[0].Author == nil || bs[0].Author.Name != "Ali" || bs[0].Author.Profile == nil || bs[0].Author.Profile.Bio != "a poet from Dodoma" {
		t.Fatalf("book 1: %+v", bs[0].Author)
	}
	if bs[2].Author == nil || bs[2].Author.Name != "Neema" || bs[2].Author.Profile != nil {
		t.Fatalf("book 3 (an author with no profile): %+v", bs[2].Author)
	}
	plain, _ := Books.First(ctx)
	if plain.Author != nil {
		t.Fatal("a relation loaded without SelectRelated")
	}
	if _, err := Authors.SelectRelated("books").All(ctx); err == nil {
		t.Fatal("SelectRelated took a relation holding many rows")
	}
}

func TestPrefetchRelated(t *testing.T) {
	ctx := relOpen(t)
	as, err := Authors.PrefetchRelated("books__tags", "profile").OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(as[0].Books); !slices.Equal(got, []string{"Go in Practice", "Poems"}) {
		t.Fatalf("Ali's books = %v", got)
	}
	if len(as[0].Books[0].Tags) != 1 || as[0].Books[0].Tags[0].Name != "golang" {
		t.Fatalf("nested tags = %+v", as[0].Books[0].Tags)
	}
	if as[0].Profile == nil || as[1].Profile != nil {
		t.Fatalf("profiles = %v %v", as[0].Profile, as[1].Profile)
	}
	if as[2].Books == nil || len(as[2].Books) != 0 {
		t.Fatalf("an author with no books: %#v", as[2].Books)
	}

	as, err = Authors.PrefetchRelated(orm.Prefetch("books", Books.Filter(orm.Q{"published": true}).OrderBy("-title"))).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(titles(as[0].Books), []string{"Go in Practice"}) || !slices.Equal(titles(as[1].Books), []string{"Learning Go"}) {
		t.Fatalf("custom prefetch = %v %v, %v", titles(as[0].Books), titles(as[1].Books), err)
	}

	ts, err := Tags.PrefetchRelated("books__author").OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts[0].Books) != 2 || ts[0].Books[0].Author == nil || len(ts[2].Books) != 0 {
		t.Fatalf("tags' books = %+v", ts)
	}
	bs, err := Books.SelectRelated("author").PrefetchRelated("tags").Filter(orm.Q{"id": 1}).All(ctx)
	if err != nil || bs[0].Author.Name != "Ali" || len(bs[0].Tags) != 1 {
		t.Fatalf("select and prefetch = %+v, %v", bs, err)
	}
	if _, err := Authors.PrefetchRelated("nothing").All(ctx); err == nil {
		t.Fatal("prefetched a relation that isn't there")
	}
}

func TestFilteredRelation(t *testing.T) {
	ctx := relOpen(t) // Ali: 2 books, 1 published; Neema: 1 published; Juma: none
	published := Authors.Annotate("published", orm.FilteredRelation("books", orm.Q{"published": true}))

	counts, err := published.Annotate("n", orm.Count("published__id")).OrderBy("name").Values[[]any]("name", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(counts, [][]any{{"Ali", int64(1)}, {"Juma", int64(0)}, {"Neema", int64(1)}}) {
		t.Fatalf("counts through a filtered relation = %v, %v", counts, err)
	}
	// Arguments in the columns, the join, WHERE and HAVING: positional ?
	// marks need them in the order the SQL names them.
	got, err := published.
		Annotate("label", orm.Case(orm.When(orm.Q{"name": "Ali"}, "first")).Else("other")).
		Annotate("n", orm.Count("published__id").Distinct()).
		Filter(orm.Q{"name__in": []string{"Ali", "Neema", "Juma"}}, orm.Q{"n__gte": 1}).
		OrderBy("name").Values[[]any]("name", "label", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(got, [][]any{{"Ali", "first", int64(1)}, {"Neema", "other", int64(1)}}) {
		t.Fatalf("HAVING over a filtered relation = %v, %v", got, err)
	}

	for _, c := range []struct {
		q    orm.Q
		want []string
	}{
		{orm.Q{"published__title__icontains": "go"}, []string{"Ali", "Neema"}},
		{orm.Q{"published__title": "Poems"}, nil},
		{orm.Q{"published__isnull": true}, []string{"Juma"}},
	} {
		as, err := published.Filter(c.q).OrderBy("id").All(ctx)
		if err != nil || !slices.Equal(authorNames(as), c.want) {
			t.Errorf("%v = %v, %v", c.q, authorNames(as), err)
		}
	}

	poets := Books.Annotate("poet", orm.FilteredRelation("author__profile", orm.Q{"bio__icontains": "poet"}))
	bios, err := poets.OrderBy("id").Values[[]any]("title", "poet__bio").All(ctx)
	if err != nil || !reflect.DeepEqual(bios, [][]any{{"Go in Practice", "a poet from Dodoma"}, {"Poems", "a poet from Dodoma"}, {"Learning Go", nil}}) {
		t.Fatalf("a filtered foreign key = %v, %v", bios, err)
	}
	if ts, err := poets.Filter(orm.Q{"poet__isnull": false}).OrderBy("id").Values[string]("title").All(ctx); err != nil || !slices.Equal(ts, []string{"Go in Practice", "Poems"}) {
		t.Fatalf("filtered through a filtered foreign key = %v, %v", ts, err)
	}

	if _, err := Authors.Annotate("x", orm.FilteredRelation("books", orm.Q{"author__name": "Ali"})).Values[string]("x__title").All(ctx); err == nil || !strings.Contains(err.Error(), "own fields") {
		t.Fatalf("a condition that joins: %v", err)
	}
	if _, err := published.Annotate("n", orm.Count("id")).Filter(orm.Q{"n__gt": 0}).Count(ctx); err == nil {
		t.Fatal("Count took a condition on an aggregate")
	}
}
