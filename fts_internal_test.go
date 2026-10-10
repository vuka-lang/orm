package orm

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/internal/schema"
	"github.com/vuka-lang/orm/migration"
)

// selectOf is the SELECT a query set runs on d, and its arguments.
func selectOf[T any](t *testing.T, qs QuerySet[T], d Dialect) (string, []any, error) {
	t.Helper()
	q := qs.q.ordered()
	b := q.builder(d)
	cols, err := q.columns(b, nil)
	if err != nil {
		return "", nil, err
	}
	s, err := q.selectSQL(b, cols)
	return s, b.args(), err
}

func TestFTSQuerySQL(t *testing.T) {
	m := searchModel(t, sqlNote{}, "notes")
	doc := SearchVector("title").Weight("A").Add(SearchVector("body"))
	q := SearchQuery(`web "in practice" serv* -java or the -rust`)
	b := query{m: m}.builder(sqlite{})
	b.ann = map[string]Expr{"rank": SearchRank(doc, q), "head": Headline("body", q, Marks("[", "]"), MaxWords(80))}
	w, err := query{m: m, where: []Cond{Match(doc, q), Q{"title__search": "go"}, Match(SearchVector("title"), SearchQuery("go"))}}.whereSQL(b)
	want := ` WHERE (("notes"."id" IN (SELECT rowid FROM "notes_fts" WHERE "notes_fts" MATCH ?) OR "notes"."id" NOT IN (SELECT rowid FROM "notes_fts" WHERE "notes_fts" MATCH ?))` +
		` AND ("notes"."id" IN (SELECT rowid FROM "notes_fts" WHERE "notes_fts" MATCH ?))` +
		` AND ("notes"."id" IN (SELECT rowid FROM "notes_fts" WHERE "notes_fts" MATCH ?)))`
	if err != nil || w != want {
		t.Errorf("where:\n got %s\nwant %s (%v)", w, want, err)
	}
	// The stopword "the" goes, as Postgres's english configuration drops
	// it; an alternative of exclusions only is a NOT IN of its own.
	args := []any{`(("web" AND "in practice" AND "serv"*) NOT ("java"))`, `"rust"`, `("go")`, `{"title"} : (("go"))`}
	if !reflect.DeepEqual(b.args(), args) {
		t.Errorf("args\n got %q\nwant %q", b.args(), args)
	}
	for name, want := range map[string]string{
		"rank": `(COALESCE((SELECT -bm25("notes_fts", 1.0, 0.1) FROM "notes_fts" WHERE "notes_fts" MATCH ? AND rowid = "notes"."id"), 0))`,
		"head": `(COALESCE((SELECT snippet("notes_fts", 1, ?, ?, '', 64) FROM "notes_fts" WHERE "notes_fts" MATCH ? AND rowid = "notes"."id"), substr("notes"."body", max(1, instr(LOWER("notes"."body"), ?) - 40), 160)))`,
	} {
		if s, err := b.ref(name); err != nil || s != want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", name, s, want, err)
		}
	}
	b = query{m: m}.builder(sqlite{})
	b.ann = map[string]Expr{"all": Headline("title", SearchQuery("go"), HighlightAll())}
	if s, _ := b.ref("all"); !strings.Contains(s, `highlight("notes_fts", 0, ?, ?)`) || !reflect.DeepEqual(b.args(), []any{"<b>", "</b>", `"go"`, "go"}) {
		t.Errorf("highlight %s %q", s, b.args())
	}

	// No document, no full-text table: LIKE, as before.
	b = query{m: searchModel(t, sqlChunk{}, "chunks")}.builder(sqlite{})
	if s, _ := (match{SearchVector("title"), SearchQuery("go")}).sql(b); !strings.Contains(s, "LIKE") {
		t.Errorf("no document: %s", s)
	}
	// A key that isn't an integer can't be an FTS5 rowid.
	if searchModel(t, sqlKeyed{}, "keyed").fts() != nil {
		t.Error("a full-text table of a text key")
	}

	// Prefixes on Postgres and MySQL.
	pb := query{m: m}.builder(postgres{})
	s, err := pb.tsquery(SearchQuery(`serv* "web server" -it's or rust`), "english")
	if want := `((to_tsquery('english', $1) && phraseto_tsquery('english', $2) && !!plainto_tsquery('english', $3)) || (plainto_tsquery('english', $4)))`; err != nil || s != want {
		t.Errorf("prefix tsquery\n got %s\nwant %s (%v)", s, want, err)
	}
	if want := []any{"'serv':*", "web server", "it's", "rust"}; !reflect.DeepEqual(pb.args(), want) {
		t.Errorf("prefix tsquery args %q", pb.args())
	}
	mb := query{m: m}.builder(mysql{})
	mb.against([]string{"x"}, `serv* -ja.va*`)
	if want := []any{`(+serv* -java*)`}; !reflect.DeepEqual(mb.args(), want) {
		t.Errorf("mysql prefix %q", mb.args())
	}
	pb = query{m: m}.builder(postgres{})
	pb.ann = map[string]Expr{"head": Headline("body", SearchQuery("go"), Marks("<mark>", "</mark>"), MaxWords(10), HighlightAll())}
	if s, _ := pb.ref("head"); s != `(ts_headline(COALESCE("notes"."body", ''), websearch_to_tsquery($1), $2))` ||
		pb.args()[1] != `StartSel="<mark>", StopSel="</mark>", MaxWords=10, MinWords=5, HighlightAll=true` {
		t.Errorf("postgres headline %s %q", s, pb.args())
	}
	pb.ann = map[string]Expr{"bad": Headline("body", SearchQuery("go"), Marks(`<a href="x">`, "</a>"))}
	if _, err := pb.ref("bad"); err == nil {
		t.Error("a quote in Postgres's marks")
	}
}

type sqlChunk struct {
	ID        int64
	Title     string
	Body      string
	Embedding Vector `orm:"vector:3"`
}

func (sqlChunk) Indexes() []Index { return []Index{IvfflatIndex("embedding").Ops(IP)} }

type sqlKeyed struct {
	Code  string `orm:"pk"`
	Title string
}

func (sqlKeyed) Indexes() []Index { return []Index{FullTextIndex("title")} }

func TestNearestSQL(t *testing.T) {
	chunks := QuerySet[sqlChunk]{q: query{m: searchModel(t, sqlChunk{}, "chunks")}}
	v := Vector{1, 0, 0}
	near := chunks.Nearest("embedding", v, K(5), MaxDistance(0.5), DistanceAs("d"), EfSearch(80), Probes(4))
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT "chunks"."id", "chunks"."title", "chunks"."body", "chunks"."embedding" FROM "chunks" WHERE ("chunks"."embedding" <#> $1) <= $2 ORDER BY (("chunks"."embedding" <#> $3)) ASC LIMIT 5`},
		{sqlite{}, `SELECT "chunks"."id", "chunks"."title", "chunks"."body", "chunks"."embedding" FROM "chunks" WHERE -inner_product("chunks"."embedding", ?) <= ? ORDER BY (-inner_product("chunks"."embedding", ?)) ASC LIMIT 5`},
	} {
		s, args, err := selectOf(t, near, c.d)
		if err != nil || s != c.want || len(args) != 3 {
			t.Errorf("%s nearest:\n got %s\nwant %s (%v)", c.d.Name(), s, c.want, err)
		}
	}
	if want := []string{"SET LOCAL hnsw.ef_search = 80", "SET LOCAL ivfflat.probes = 4"}; !slices.Equal(near.q.settings, want) {
		t.Errorf("settings %q", near.q.settings)
	}
	if _, _, err := selectOf(t, near, mysql{}); err == nil || !strings.Contains(err.Error(), "vector search needs Postgres (pgvector) or SQLite") {
		t.Errorf("mysql: %v", err)
	}
	// The metric is the index's, or the option's.
	if s, _, _ := selectOf(t, chunks.Nearest("embedding", v, Cosine), postgres{}); !strings.Contains(s, "<=>") {
		t.Errorf("cosine: %s", s)
	}
	if got := searchModel(t, sqlChunk{}, "chunks").metricOf("title"); got != L2 {
		t.Errorf("no index, metric %v", got)
	}

	hybrid := chunks.Hybrid(SearchVector("title", "body"), SearchQuery("web"), "embedding", v, Candidates(20), HybridWeights(1, 0.5), RRF(10), K(3))
	s, args, err := selectOf(t, hybrid, postgres{})
	for _, want := range []string{
		`WHERE ((to_tsvector(COALESCE("chunks"."title", '') || ' ' || COALESCE("chunks"."body", ''))) @@ websearch_to_tsquery($`,
		` OR "chunks"."id" IN (SELECT "orm_1"."id" FROM "chunks" AS "orm_1" ORDER BY ("orm_1"."embedding" <#> $`,
		`) LIMIT 20))`,
		`CASE WHEN (to_tsvector(`,
		` THEN 1.0 / (10 + ROW_NUMBER() OVER (ORDER BY CASE WHEN (to_tsvector(`,
		` THEN 0 ELSE 1 END, (ts_rank(`,
		` + 0.5 / (10 + ROW_NUMBER() OVER (ORDER BY (("chunks"."embedding" <#> $`,
		`)))) DESC LIMIT 3`,
	} {
		if err != nil || !strings.Contains(s, want) {
			t.Fatalf("hybrid lacks %q:\n%s (%v)", want, s, err)
		}
	}
	// Every placeholder has its argument, numbered in order.
	if n := strings.Count(s, "$"); n != len(args) || !strings.Contains(s, "$"+strconv.Itoa(len(args))) {
		t.Errorf("hybrid: %d placeholders, %d args", n, len(args))
	}
	if s, args, err := selectOf(t, hybrid, sqlite{}); err != nil || strings.Count(s, "?") != len(args) {
		t.Errorf("sqlite hybrid: %v %d %d", err, strings.Count(s, "?"), len(args))
	}

	b := query{m: searchModel(t, sqlChunk{}, "chunks")}.builder(postgres{})
	b.ann = map[string]Expr{"a": F("id"), "f": Fuse("a", "-a").K(5).Weights(2)}
	if s, _ := b.ref("f"); s != `((2.0 / (5 + ROW_NUMBER() OVER (ORDER BY ("chunks"."id") ASC)) + 1.0 / (5 + ROW_NUMBER() OVER (ORDER BY ("chunks"."id") DESC))))` {
		t.Errorf("weighted fuse %s", s)
	}
}

func TestSplitTrigger(t *testing.T) {
	src := "CREATE TABLE a (x);\nCREATE TRIGGER IF NOT EXISTS t AFTER INSERT ON a BEGIN INSERT INTO b VALUES (new.x); DELETE FROM c; END;\nINSERT INTO a VALUES (';');\n" +
		"CREATE TRIGGER s BEFORE INSERT ON a FOR EACH ROW SET NEW.x = 1;\nSELECT 1"
	want := []string{"CREATE TABLE a (x)", "CREATE TRIGGER IF NOT EXISTS t AFTER INSERT ON a BEGIN INSERT INTO b VALUES (new.x); DELETE FROM c; END", "INSERT INTO a VALUES (';')", "CREATE TRIGGER s BEFORE INSERT ON a FOR EACH ROW SET NEW.x = 1", "SELECT 1"}
	if got := splitSQL(src, "sqlite"); !slices.Equal(got, want) {
		t.Errorf("split\n got %q\nwant %q", got, want)
	}
}

// ftsNote is sqlNote with a column besides its document.
type ftsNote struct {
	ID     int64
	Title  string
	Body   string
	Views  int
	Search TSVector `orm:"generated"`
}

func (ftsNote) Generated() map[string]Expr {
	return map[string]Expr{"Search": SearchVector("title", "body").Config("english")}
}

type ftsPlain struct {
	ID    int64
	Title string
	Body  string
	Views int
}

// TestFTSMigration migrates SQLite's full-text table: made with its
// table, and added to one with rows (rebuilt from them); kept by its
// triggers through inserts, updates and deletes, and through a rebuilt
// table; and gone again when the migrations are unapplied.
func TestFTSMigration(t *testing.T) {
	d := memDB(t)
	ctx := WithDB(context.Background(), d)
	tables := func(of any) []schema.Table {
		ts, err := searchModel(t, of, "notes").tables()
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	plain, withDoc := migration.NewState(tables(ftsPlain{}), nil), migration.NewState(tables(ftsNote{}), nil)
	never := func(string) bool { return false }
	first, _ := migration.Diff(migration.NewState(nil, nil), plain, never)
	second, _ := migration.Diff(plain, withDoc, never)
	altered := tables(ftsNote{})
	altered[0].Columns[3].Nullable = true // views: a rebuilt table on SQLite
	third, _ := migration.Diff(withDoc, migration.NewState(altered, nil), never)
	migs := []migration.Migration{
		{Name: "0001_notes", Operations: first},
		{Name: "0002_search", Dependencies: []string{"0001_notes"}, Operations: second},
		{Name: "0003_views", Dependencies: []string{"0002_search"}, Operations: third},
	}
	if !slices.ContainsFunc(second, func(op migration.Operation) bool {
		ix, ok := op.(migration.AddIndex)
		return ok && ix.Index.Name == "notes_fts" && strings.Contains(ix.Index.Drop.SQLite, `DROP TABLE IF EXISTS "notes_fts"`)
	}) {
		t.Fatalf("no full-text table added: %v", second)
	}
	src, err := migration.Source("migrations", migs[1], nil)
	if err != nil || !strings.Contains(string(src), "Drop: m.Dialects{SQLite: `DROP TRIGGER IF EXISTS \"notes_fts_ai\";") {
		t.Fatalf("written migration:\n%s\n%v", src, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	found := func(word string) []int64 {
		t.Helper()
		ids, err := Raw[int64](ctx, `SELECT rowid FROM notes_fts WHERE notes_fts MATCH ? ORDER BY rowid`, word)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if err := ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO notes (title, body, views) VALUES ('Go web servers', 'net/http', 1), ('Gardening', 'tomatoes', 2)`)
	if err := ApplyMigrations(ctx, d, "0002", migs...); err != nil {
		t.Fatal(err)
	}
	if got := found("web"); !slices.Equal(got, []int64{1}) {
		t.Fatalf("rows there before: %v", got)
	}
	exec(`INSERT INTO notes (title, body, views) VALUES ('Rust', 'a web server', 3)`)
	exec(`UPDATE notes SET body = 'tomatoes and the web' WHERE id = 2`)
	exec(`UPDATE notes SET views = 9 WHERE id = 1`)
	exec(`DELETE FROM notes WHERE id = 3`)
	if got, gone := found("web"), found("rust"); !slices.Equal(got, []int64{1, 2}) || len(gone) != 0 {
		t.Fatalf("triggers: web %v, rust %v", got, gone)
	}
	if err := ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO notes (title, body, views) VALUES ('Java', 'web streams', 4)`)
	if got := found("web"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("after the table was rebuilt: %v", got)
	}
	if err := ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	objects := func() []string {
		names, err := Raw[string](ctx, `SELECT name FROM sqlite_master WHERE name LIKE 'notes_fts%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if left := objects(); len(left) != 0 {
		t.Fatalf("unapplied, left %v", left)
	}
	exec(`INSERT INTO notes (title, body, views) VALUES ('After', 'web', 1)`)
	if err := ApplyMigrations(ctx, d, "zero", migs...); err != nil {
		t.Fatal(err)
	}

	// Made with its table, dropped with it.
	made, _ := migration.Diff(migration.NewState(nil, nil), withDoc, never)
	migs = []migration.Migration{{Name: "0001_all", Operations: made}}
	if err := ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatal(err)
	}
	if got := objects(); len(got) < 4 {
		t.Fatalf("made with the table: %v", got)
	}
	if err := ApplyMigrations(ctx, d, "zero", migs...); err != nil {
		t.Fatal(err)
	}
	if left := objects(); len(left) != 0 {
		t.Fatalf("dropped with the table, left %v", left)
	}
	steps, err := (migration.Migration{Operations: []migration.Operation{migration.DeleteModel{Table: "notes"}}}).Forwards("sqlite", withDoc.Clone())
	if err != nil || len(steps) != 2 || !strings.HasPrefix(steps[0].SQL, "DROP TRIGGER") {
		t.Fatalf("delete model: %v %v", steps, err)
	}
}
