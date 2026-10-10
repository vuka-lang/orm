package orm_test

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

// The search models are on databases of their own: MySQL holds neither,
// and each needs an extension a Postgres server may not have.
type Doc struct {
	ID     int64
	Title  string
	Body   string
	Search orm.TSVector `orm:"generated"`
	Score  float64      `orm:"computed"`
}

func (Doc) Generated() map[string]orm.Expr {
	return map[string]orm.Expr{"Search": orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B")).Config("english")}
}

func (Doc) Indexes() []orm.Index {
	return []orm.Index{orm.GinIndex("search"), orm.GinIndex("title").Trigram()}
}

type Chunk struct {
	ID        int64
	Title     string
	Body      string
	Embedding orm.Vector `orm:"vector:3"`
	Score     float64    `orm:"computed"`
}

func (Chunk) Indexes() []orm.Index {
	return []orm.Index{orm.HnswIndex("embedding").Ops(orm.Cosine)}
}

var (
	Docs     = orm.For[Doc](orm.On("search"))
	Trigrams = orm.CreateExtension("pg_trgm", orm.On("search"))
	Chunks   = orm.For[Chunk](orm.On("vectors"))
	Vectors  = orm.CreateExtension("vector", orm.On("vectors"))
)

// needs skips the test on a server that can't hold the search models:
// MySQL, or a Postgres without one of the extensions.
func needs(t *testing.T, exts ...string) {
	t.Helper()
	switch ormtest.Driver() {
	case "mysql":
		t.Skip("the search models are Postgres's and SQLite's: MySQL has no vectors or trigram similarity")
	case "postgres":
		ctx := ormtest.Open(t)
		d, _ := orm.DBFrom(ctx)
		for _, e := range exts {
			var ok bool
			if err := d.SQL().QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)", e).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Skipf("the server has no %s extension to install", e)
			}
			if _, err := d.SQL().ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS "+e+" SCHEMA public"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func chunkOpen(t *testing.T) []Chunk {
	t.Helper()
	ctx := ormtest.Open(t, Chunks)
	ormtest.Seed(t, ctx, Chunks,
		&Chunk{Title: "Go web servers", Body: "net/http and routing", Embedding: orm.Vector{1, 0, 0}},
		&Chunk{Title: "Rust in practice", Body: "a web server in Rust", Embedding: orm.Vector{0, 1, 0}},
		&Chunk{Title: "Java streams", Body: "collections, not the web", Embedding: orm.Vector{0.9, 0.1, 0}},
		&Chunk{Title: "Gardening", Body: "tomatoes", Embedding: orm.Vector{0, 0, 1}})
	chunks, err := Chunks.OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

func docTitles(ds []Doc) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Title
	}
	return out
}

func chunkTitles(cs []Chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Title
	}
	return out
}

// Note is a document every database searches: MySQL by its FULLTEXT
// indexes, Postgres by the GIN index of its tsvector.
type Note struct {
	ID     int64
	Title  string
	Body   string
	Search orm.TSVector `orm:"generated"`
	Score  float64      `orm:"computed"`
}

func (Note) Generated() map[string]orm.Expr {
	return map[string]orm.Expr{"Search": orm.SearchVector("title", "body").Config("english")}
}

func (Note) Indexes() []orm.Index {
	return []orm.Index{orm.FullTextIndex("search"), orm.FullTextIndex("title", "body")}
}

var Notes = orm.For[Note]()

func TestFullTextSearch(t *testing.T) {
	ctx := ormtest.Open(t, Notes)
	ormtest.Seed(t, ctx, Notes,
		&Note{Title: "Go web servers", Body: "net/http and routing"},
		&Note{Title: "Rust in practice", Body: "a web server in Rust"},
		&Note{Title: "Java streams", Body: "collections, not the web"},
		&Note{Title: "Gardening", Body: "tomatoes"})
	doc, q := orm.SearchVector("title", "body"), orm.SearchQuery("web")
	for _, c := range []struct {
		q    orm.Cond
		want []string
	}{
		{orm.Q{"search__search": "web -java"}, []string{"Go web servers", "Rust in practice"}},
		{orm.Q{"title__search": `"in practice" or tomatoes`}, []string{"Rust in practice", "Gardening"}},
		{orm.Match(doc, orm.SearchQuery("server rust")), []string{"Rust in practice"}},
		{orm.Match(doc, q), []string{"Go web servers", "Rust in practice", "Java streams"}},
	} {
		got, err := Notes.Filter(c.q).OrderBy("id").Values[string]("title").All(ctx)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, %v", c.q, got, err)
		}
	}
	ranked, err := Notes.Filter(orm.Match(doc, q)).Annotate("score", orm.SearchRank(doc, q)).OrderBy("-score", "id").All(ctx)
	if err != nil || len(ranked) != 3 || ranked[2].Score <= 0 {
		t.Fatalf("ranked %v, %v", ranked, err)
	}
}

func TestTextSearch(t *testing.T) {
	needs(t, "pg_trgm")
	ctx := ormtest.Open(t, Docs)
	ormtest.Seed(t, ctx, Docs,
		&Doc{Title: "Go web servers", Body: "net/http and routing"},
		&Doc{Title: "Rust in practice", Body: "a web server in Rust"},
		&Doc{Title: "Java streams", Body: "collections, not the web"},
		&Doc{Title: "Gardening", Body: "tomatoes"})
	want := orm.TSVector("Rust in practice a web server in Rust")
	if ormtest.Driver() == "postgres" {
		want = "'practic':3A 'rust':1A,8B 'server':6B 'web':5B"
	}
	all, err := Docs.OrderBy("id").All(ctx)
	if err != nil || all[1].Search != want {
		t.Fatalf("the generated column: %q, %v", all[1].Search, err)
	}
	for _, c := range []struct {
		q    orm.Cond
		want []string
	}{
		{orm.Q{"search__search": "web -java"}, []string{"Go web servers", "Rust in practice"}},
		{orm.Q{"title__search": `"in practice" or tomatoes`}, []string{"Rust in practice", "Gardening"}},
		{orm.Match(orm.SearchVector("title", "body"), orm.SearchQuery("server rust")), []string{"Rust in practice"}},
		{orm.Q{"title__trigram_similar": "gardenin"}, []string{"Gardening"}},
	} {
		got, err := Docs.Filter(c.q).OrderBy("id").All(ctx)
		if err != nil || !slices.Equal(docTitles(got), c.want) {
			t.Errorf("%v: %v, %v", c.q, docTitles(got), err)
		}
	}
	doc := orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B"))
	q := orm.SearchQuery("web")
	ranked, err := Docs.Filter(orm.Match(doc, q)).Annotate("score", orm.SearchRank(doc, q)).OrderBy("-score", "id").All(ctx)
	if err != nil || !slices.Equal(docTitles(ranked), []string{"Go web servers", "Rust in practice", "Java streams"}) || ranked[0].Score <= ranked[1].Score {
		t.Fatalf("ranked %v %v, %v", docTitles(ranked), ranked, err)
	}
	heads, err := Docs.Filter(orm.Q{"id": 3}).Annotate("head", orm.Headline("body", orm.SearchQuery("web"))).Values[string]("head").All(ctx)
	head := "collections, not the web"
	if ormtest.Driver() == "postgres" {
		head = "collections, not the <b>web</b>"
	}
	if err != nil || !slices.Equal(heads, []string{head}) {
		t.Fatalf("headline %v, %v", heads, err)
	}
	sim, err := Docs.Filter(orm.Q{"id": 4}).Annotate("sim", orm.Similarity("title", "gardenin")).Values[float64]("sim").All(ctx)
	if err != nil || sim[0] < 0.5 || sim[0] >= 1 {
		t.Fatalf("similarity %v, %v", sim, err)
	}
	if _, err := Docs.Filter(orm.Q{"id": 1}).Update(ctx, orm.Set{"search": "x"}); err == nil || !strings.Contains(err.Error(), "generated by the database") {
		t.Fatalf("wrote a generated column: %v", err)
	}
}

func TestVectorSearch(t *testing.T) {
	needs(t, "vector")
	chunks := chunkOpen(t)
	if !slices.Equal(chunks[2].Embedding, orm.Vector{0.9, 0.1, 0}) {
		t.Fatalf("read %v", chunks[2].Embedding)
	}
	ctx := ormtest.Open(t, Chunks)
	for _, c := range chunks {
		c.ID = 0
		if err := Chunks.Create(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	near, err := Chunks.Nearest("embedding", orm.Vector{1, 0.2, 0}, orm.Cosine).Limit(3).All(ctx)
	if err != nil || !slices.Equal(chunkTitles(near), []string{"Java streams", "Go web servers", "Rust in practice"}) {
		t.Fatalf("nearest %v, %v", chunkTitles(near), err)
	}
	for _, c := range []struct {
		e    orm.Expr
		want float64
	}{
		{orm.L2Distance("embedding", orm.Vector{0, 1, 0}), math.Sqrt(2)},
		{orm.CosineDistance("embedding", orm.Vector{2, 0, 0}), 0},
		{orm.InnerProduct("embedding", orm.Vector{3, 0, 0}), -3},
	} {
		got, err := Chunks.Filter(orm.Q{"title": "Go web servers"}).Annotate("d", c.e).Values[float64]("d").All(ctx)
		if err != nil || math.Abs(got[0]-c.want) > 1e-6 {
			t.Errorf("%v = %v, %v", c.e, got, err)
		}
	}
	q := orm.SearchQuery("web")
	doc := orm.SearchVector("title", "body")
	hybrid, err := Chunks.Filter(orm.Match(doc, q)).
		Annotate("rank", orm.SearchRank(orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body")), q)).
		Annotate("distance", orm.CosineDistance("embedding", orm.Vector{0, 1, 0})).
		Annotate("score", orm.Fuse("-rank", "distance")).OrderBy("-score", "id").All(ctx)
	if err != nil || !slices.Equal(chunkTitles(hybrid), []string{"Rust in practice", "Go web servers", "Java streams"}) {
		t.Fatalf("hybrid %v %v, %v", chunkTitles(hybrid), hybrid, err)
	}
}

func TestSearchCheck(t *testing.T) {
	useDB(t, "vectors", true)
	ctx := context.Background()
	if err := Vectors.Verify(ctx); err != nil {
		t.Fatalf("SQLite has no extensions: %v", err)
	}
	if err := (orm.Schema{DB: "vectors"}).Verify(ctx, Chunk{}); err != nil {
		t.Fatalf("SQLite searches unindexed: %v", err)
	}
}
