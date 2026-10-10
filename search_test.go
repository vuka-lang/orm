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
	// ts_rank weighs Rust's and Java's matches in their bodies alike; FTS5's
	// bm25 ranks the shorter body higher.
	order := []string{"Go web servers", "Rust in practice", "Java streams"}
	if ormtest.Driver() == "sqlite" {
		order = []string{"Go web servers", "Java streams", "Rust in practice"}
	}
	if err != nil || !slices.Equal(docTitles(ranked), order) || ranked[0].Score <= ranked[1].Score {
		t.Fatalf("ranked %v %v, %v", docTitles(ranked), ranked, err)
	}
	heads, err := Docs.Filter(orm.Q{"id": 3}).Annotate("head", orm.Headline("body", orm.SearchQuery("web"))).Values[string]("head").All(ctx)
	head := "collections, not the <b>web</b>"
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

// TestVectorStorage writes and reads vectors on every database, MySQL
// included, which refuses only searching them, when the query is made.
func TestVectorStorage(t *testing.T) {
	if ormtest.Driver() == "postgres" {
		needs(t, "vector")
	}
	chunks := chunkOpen(t)
	if !slices.Equal(chunks[2].Embedding, orm.Vector{0.9, 0.1, 0}) || !slices.Equal(chunks[3].Embedding, orm.Vector{0, 0, 1}) {
		t.Fatalf("read %v", chunks)
	}
	ctx := ormtest.Open(t, Chunks)
	c := Chunk{Title: "Negative", Embedding: orm.Vector{-1.5, 1e-7, 3.25}}
	if err := Chunks.Create(ctx, &c); err != nil {
		t.Fatal(err)
	}
	empty := Chunk{Title: "Unembedded"}
	if err := Chunks.Create(ctx, &empty); err != nil {
		t.Fatal(err)
	}
	if n, err := Chunks.Filter(orm.Q{"id": c.ID}).Update(ctx, orm.Set{"embedding": orm.Vector{4, 5, 6}}); n != 1 || err != nil {
		t.Fatalf("update = %d, %v", n, err)
	}
	got, err := Chunks.OrderBy("id").All(ctx)
	if err != nil || len(got) != 2 || !slices.Equal(got[0].Embedding, orm.Vector{4, 5, 6}) || got[1].Embedding != nil {
		t.Fatalf("after update %v, %v", got, err)
	}
	got[0].Embedding = orm.Vector{-1.5, 1e-7, 3.25}
	if err := Chunks.Save(ctx, &got[0]); err != nil {
		t.Fatal(err)
	}
	if n, err := Chunks.Filter(orm.Q{"embedding__isnull": true}).Count(ctx); n != 1 || err != nil {
		t.Fatalf("isnull = %d, %v", n, err)
	}
	// Read with an argument: MySQL's binary protocol, its VECTOR type 242.
	vs, err := Chunks.Filter(orm.Q{"embedding__isnull": false, "title": "Negative"}).Values[orm.Vector]("embedding").All(ctx)
	if err != nil || len(vs) != 1 || !slices.Equal(vs[0], orm.Vector{-1.5, 1e-7, 3.25}) {
		t.Fatalf("values %v, %v", vs, err)
	}
	if ormtest.Driver() != "mysql" {
		return
	}
	// A VECTOR column on MySQL 9, else VARBINARY.
	typ, err := orm.Raw[string](ctx, "SELECT CONCAT(VERSION(), ' ', DATA_TYPE) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'chunks' AND COLUMN_NAME = 'embedding'")
	if err != nil || len(typ) != 1 || !strings.HasSuffix(typ[0], " vector") && (strings.HasPrefix(typ[0], "9.") || !strings.HasSuffix(typ[0], " varbinary")) {
		t.Fatalf("column type %v, %v", typ, err)
	}
	for name, qs := range map[string]orm.QuerySet[Chunk]{
		"nearest":  Chunks.Nearest("embedding", orm.Vector{1, 0, 0}),
		"distance": Chunks.Filter(orm.L2Distance("embedding", orm.Vector{1, 0, 0}).Lt(1)),
		"hybrid":   Chunks.Hybrid(orm.SearchVector("title"), orm.SearchQuery("go"), "embedding", orm.Vector{1, 0, 0}),
	} {
		if _, err := qs.All(ctx); err == nil || !strings.Contains(err.Error(), "MySQL stores embedding's vectors but can't search them") {
			t.Errorf("%s on MySQL: %v", name, err)
		}
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

func noteOpen(t *testing.T) context.Context {
	t.Helper()
	if ormtest.Driver() == "mysql" {
		t.Skip("MySQL's boolean mode has no exclusion-only search, and its own stopwords")
	}
	ctx := ormtest.Open(t, Notes)
	ormtest.Seed(t, ctx, Notes,
		&Note{Title: "Go web servers", Body: "net/http and routing"},
		&Note{Title: "Rust in practice", Body: "a web server in Rust"},
		&Note{Title: "Java streams", Body: "collections, not the web"},
		&Note{Title: "Gardening", Body: "tomatoes"})
	return ctx
}

// TestFullTextQueries runs the web search syntax on Postgres's tsvector
// and SQLite's FTS5 table alike: words, phrases, prefixes, exclusions,
// alternatives, the english stopwords dropped.
func TestFullTextQueries(t *testing.T) {
	ctx := noteOpen(t)
	doc := orm.SearchVector("title", "body").Config("english")
	for _, c := range []struct {
		q    string
		want []string
	}{
		{`web server`, []string{"Go web servers", "Rust in practice"}},
		{`"web server"`, []string{"Go web servers", "Rust in practice"}},
		{`"server web"`, nil},
		{`serv*`, []string{"Go web servers", "Rust in practice"}},
		{`-web`, []string{"Gardening"}},
		{`rust or tomatoes`, []string{"Rust in practice", "Gardening"}},
		{`the web`, []string{"Go web servers", "Rust in practice", "Java streams"}},
		{`web -java -rust`, []string{"Go web servers"}},
		{`garden*`, []string{"Gardening"}},
	} {
		got, err := Notes.Filter(orm.Match(doc, orm.SearchQuery(c.q))).OrderBy("id").Values[string]("title").All(ctx)
		if err != nil || !slices.Equal(got, c.want) && len(got)+len(c.want) > 0 {
			t.Errorf("%q: %q, %v", c.q, got, err)
		}
	}
	// A field's __search is its document's.
	got, err := Notes.Filter(orm.Q{"title__search": "tomatoes"}).Values[string]("title").All(ctx)
	if err != nil || !slices.Equal(got, []string{"Gardening"}) {
		t.Errorf("title__search: %q, %v", got, err)
	}

	// Writes reach the full-text table.
	n, err := Notes.Get(ctx, orm.Q{"title": "Java streams"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Notes.Filter(orm.Q{"id": n.ID}).Update(ctx, orm.Set{"body": "lazy evaluation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Notes.Filter(orm.Q{"title": "Go web servers"}).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Notes.Create(ctx, &Note{Title: "Elixir", Body: "web sockets"}); err != nil {
		t.Fatal(err)
	}
	got, err = Notes.Filter(orm.Match(doc, orm.SearchQuery("web"))).OrderBy("id").Values[string]("title").All(ctx)
	if err != nil || !slices.Equal(got, []string{"Rust in practice", "Elixir"}) {
		t.Fatalf("after writes: %q, %v", got, err)
	}
}

// TestSearchTextBound sends search text only as arguments: what would
// break SQL, or FTS5's query syntax, is searched for as words.
func TestSearchTextBound(t *testing.T) {
	ctx := noteOpen(t)
	var sqls []string
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) { sqls = append(sqls, q.SQL) })
	doc := orm.SearchVector("title", "body").Config("english")
	for _, text := range []string{`'); DROP TABLE notes; --`, `NEAR(web rust)`, `title:web`, `{title}: web`, `"unclosed`, `^web`, `(`, `***`, `-`, `" "`, `web*'`, `\`} {
		q := orm.SearchQuery(text)
		_, err := Notes.Filter(orm.Or(orm.Match(doc, q), orm.Q{"title__search": text})).
			Annotate("score", orm.SearchRank(doc, q)).Annotate("head", orm.Headline("body", q, orm.Marks("<i>", "</i>"))).
			Values[string]("head").All(ctx)
		if err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
	if slices.ContainsFunc(sqls, func(s string) bool { return strings.Contains(s, "DROP") || strings.Contains(s, "NEAR") }) {
		t.Fatalf("search text in the SQL: %q", sqls)
	}
	if n, err := Notes.Count(ctx); err != nil || n != 4 {
		t.Fatalf("notes %d, %v", n, err)
	}
}

func TestHeadlineOptions(t *testing.T) {
	needs(t, "pg_trgm")
	ctx := ormtest.Open(t, Docs)
	ormtest.Seed(t, ctx, Docs,
		&Doc{Title: "Rust in practice", Body: "a web server in Rust"},
		&Doc{Title: "Essay", Body: "one two three four five six seven eight nine ten eleven twelve web thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty"})
	q := orm.SearchQuery("web")
	head := func(id int, opts ...orm.HeadlineOption) string {
		t.Helper()
		got, err := Docs.Filter(orm.Q{"id": id}).Annotate("head", orm.Headline("body", q, opts...)).Values[string]("head").All(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("headline %v, %v", got, err)
		}
		return got[0]
	}
	if got := head(1, orm.Marks("[", "]")); got != "a [web] server in Rust" {
		t.Errorf("marks: %q", got)
	}
	if got := head(1, orm.Marks("[", "]"), orm.HighlightAll()); got != "a [web] server in Rust" {
		t.Errorf("highlight all: %q", got)
	}
	short := head(2, orm.MaxWords(6))
	if !strings.Contains(short, "<b>web</b>") || len(strings.Fields(short)) > 7 {
		t.Errorf("max words: %q", short)
	}
	if full := head(2, orm.HighlightAll()); !strings.HasPrefix(full, "one two") || !strings.HasSuffix(full, "twenty") || !strings.Contains(full, "<b>web</b>") {
		t.Errorf("highlight all: %q", full)
	}
	// A row without the words: its text unmarked.
	if got, err := Docs.Filter(orm.Q{"id": 1}).Annotate("head", orm.Headline("body", orm.SearchQuery("tomatoes"))).Values[string]("head").All(ctx); err != nil || len(got) != 1 || strings.Contains(got[0], "<b>") || !strings.HasPrefix(got[0], "a web") {
		t.Errorf("no match: %q, %v", got, err)
	}
}

func TestNearest(t *testing.T) {
	needs(t, "vector")
	chunkOpen(t)
	ctx := ormtest.Open(t, Chunks)
	ormtest.Seed(t, ctx, Chunks,
		&Chunk{Title: "Go web servers", Body: "net/http and routing", Embedding: orm.Vector{1, 0, 0}},
		&Chunk{Title: "Rust in practice", Body: "a web server in Rust", Embedding: orm.Vector{0, 1, 0}},
		&Chunk{Title: "Java streams", Body: "collections, not the web", Embedding: orm.Vector{0.9, 0.1, 0}},
		&Chunk{Title: "Gardening", Body: "tomatoes", Embedding: orm.Vector{0, 0, 1}})
	v := orm.Vector{1, 0.2, 0}
	for _, c := range []struct {
		opts []orm.SearchOption
		want []string
	}{
		{[]orm.SearchOption{orm.K(2)}, []string{"Java streams", "Go web servers"}}, // the index's metric: cosine
		{[]orm.SearchOption{orm.MaxDistance(0.1)}, []string{"Java streams", "Go web servers"}},
		{[]orm.SearchOption{orm.L2}, []string{"Java streams", "Go web servers", "Rust in practice", "Gardening"}},
		{[]orm.SearchOption{orm.IP, orm.K(3)}, []string{"Go web servers", "Java streams", "Rust in practice"}},
		{[]orm.SearchOption{orm.Cosine, orm.K(2), orm.EfSearch(100), orm.Probes(3)}, []string{"Java streams", "Go web servers"}},
	} {
		got, err := Chunks.Nearest("embedding", v, c.opts...).All(ctx)
		if err != nil || !slices.Equal(chunkTitles(got), c.want) {
			t.Errorf("%v: %v, %v", c.opts, chunkTitles(got), err)
		}
	}
	d, err := Chunks.Nearest("embedding", v, orm.L2, orm.K(1), orm.DistanceAs("d")).Values[float64]("d").All(ctx)
	if err != nil || len(d) != 1 || math.Abs(d[0]-math.Sqrt(0.02)) > 1e-6 {
		t.Fatalf("distance as d: %v, %v", d, err)
	}
	// Typed distances compare and order.
	near, err := Chunks.Filter(orm.L2Distance("embedding", v).Lt(0.5)).Order(orm.L2Distance("embedding", v).Desc()).Values[string]("title").All(ctx)
	if err != nil || !slices.Equal(near, []string{"Go web servers", "Java streams"}) {
		t.Fatalf("typed distance: %v, %v", near, err)
	}
	if ormtest.Driver() != "postgres" {
		return
	}

	// ef_search is the query's, in a transaction of its own or a savepoint
	// of the context's, and gone after it.
	setting := orm.Function("current_setting")
	for _, in := range []bool{false, true} {
		run := func(ctx context.Context) error {
			got, err := Chunks.Nearest("embedding", v, orm.EfSearch(77)).Annotate("ef", setting.Of("hnsw.ef_search")).Values[string]("ef").All(ctx)
			if err != nil || len(got) == 0 || got[0] != "77" {
				t.Errorf("in a transaction %v: ef_search %v, %v", in, got, err)
			}
			after, err := orm.Raw[string](ctx, "SELECT COALESCE(current_setting('hnsw.ef_search', true), '')")
			if err != nil || after[0] == "77" {
				t.Errorf("in a transaction %v: ef_search after %v, %v", in, after, err)
			}
			return nil
		}
		if in {
			err = orm.Atomic(ctx, run)
		} else {
			err = run(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// The ORDER BY is pgvector's operator: the HNSW index serves it.
	var sel orm.QueryInfo
	seen := orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) {
		if strings.HasPrefix(q.SQL, "SELECT") {
			sel = q
		}
	})
	if _, err := Chunks.Nearest("embedding", v, orm.K(2)).All(seen); err != nil {
		t.Fatal(err)
	}
	db, _ := orm.DBFrom(ctx)
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+sel.SQL, sel.Args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan = append(plan, line)
	}
	rows.Close()
	if !strings.Contains(strings.Join(plan, "\n"), "chunks_embedding_hnsw") {
		t.Fatalf("the HNSW index unused:\n%s\n%s", sel.SQL, strings.Join(plan, "\n"))
	}
}

// Passage is searched both ways: its text by its document (FTS5 on
// SQLite), its embedding by an HNSW index.
type Passage struct {
	ID        int64
	Title     string
	Body      string
	Search    orm.TSVector `orm:"generated"`
	Embedding orm.Vector   `orm:"vector:3"`
	Score     float64      `orm:"computed"`
}

func (Passage) Generated() map[string]orm.Expr {
	return map[string]orm.Expr{"Search": orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B")).Config("english")}
}

func (Passage) Indexes() []orm.Index {
	return []orm.Index{orm.GinIndex("search"), orm.HnswIndex("embedding").Ops(orm.Cosine)}
}

var Passages = orm.For[Passage](orm.On("vectors"))

func passageTitles(ps []Passage) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Title
	}
	return out
}

func TestHybrid(t *testing.T) {
	needs(t, "vector")
	ctx := ormtest.Open(t, Passages)
	ormtest.Seed(t, ctx, Passages,
		&Passage{Title: "Go web servers", Body: "net/http and routing", Embedding: orm.Vector{1, 0, 0}},
		&Passage{Title: "Rust in practice", Body: "a web server in Rust", Embedding: orm.Vector{0, 1, 0}},
		&Passage{Title: "Java streams", Body: "collections, not the web", Embedding: orm.Vector{0.9, 0.1, 0}},
		&Passage{Title: "Gardening", Body: "tomatoes", Embedding: orm.Vector{0, 0, 1}})
	doc := orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B")).Config("english")
	q, v := orm.SearchQuery("web server"), orm.Vector{0, 1, 0.2}
	// Vector order: Rust, Gardening, Java, Go; text matches: Go and Rust.
	for _, c := range []struct {
		opts []orm.SearchOption
		want []string
	}{
		{nil, []string{"Rust in practice", "Go web servers", "Gardening", "Java streams"}},
		{[]orm.SearchOption{orm.K(3), orm.EfSearch(64)}, []string{"Rust in practice", "Go web servers", "Gardening"}},
		{[]orm.SearchOption{orm.Candidates(1), orm.HybridWeights(1, 2)}, []string{"Rust in practice", "Go web servers"}},
		{[]orm.SearchOption{orm.MaxDistance(0.85)}, []string{"Rust in practice", "Go web servers", "Gardening"}},
		{[]orm.SearchOption{orm.HybridWeights(0, 1)}, []string{"Rust in practice", "Gardening", "Java streams", "Go web servers"}},
		// Go's match is in its title (A), Rust's in its body (B).
		{[]orm.SearchOption{orm.RRF(1), orm.HybridWeights(1, 0.1)}, []string{"Go web servers", "Rust in practice", "Gardening", "Java streams"}},
	} {
		got, err := Passages.Hybrid(doc, q, "embedding", v, c.opts...).All(ctx)
		if err != nil || !slices.Equal(passageTitles(got), c.want) {
			t.Errorf("%v: %v, %v", c.opts, passageTitles(got), err)
			continue
		}
		for i := 1; i < len(got); i++ {
			if got[i].Score > got[i-1].Score {
				t.Errorf("%v: scores out of order %v", c.opts, got)
			}
		}
	}
	// The fused parts read through Values, under names of one's own.
	type hit struct {
		Title string
		R     float64
		D     float64
		S     float64
	}
	hits, err := Passages.Hybrid(doc, q, "embedding", v, orm.RankAs("r"), orm.DistanceAs("d"), orm.ScoreAs("s"), orm.K(2)).Values[hit]("title", "r", "d", "s").All(ctx)
	if err != nil || len(hits) != 2 || hits[0].Title != "Rust in practice" || hits[0].R <= 0 || hits[0].D > 0.05 || hits[0].S <= hits[1].S {
		t.Fatalf("values %+v, %v", hits, err)
	}
}
