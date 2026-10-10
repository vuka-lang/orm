package orm

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/internal/schema"
	"github.com/vuka-lang/orm/migration"
)

type SQLBase struct {
	ID int64 `orm:"pk"`
}

type sqlUser struct {
	*SQLBase
	Name string
	Age  int
}

func TestSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	if m.err != nil {
		t.Fatal(m.err)
	}
	q := m.Filter(Q{"age__gte": 18, "name__icontains": "a_%"}).Exclude(Q{"id__in": []int{1, 2}}).OrderBy("-age").Limit(10).Offset(20)
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT "users"."id", "users"."name", "users"."age" FROM "users" WHERE (("users"."age" >= $1 AND "users"."name" ILIKE $2 ESCAPE '!') AND NOT ("users"."id" IN ($3, $4))) ORDER BY "users"."age" DESC LIMIT 10 OFFSET 20`},
		{mysql{}, "SELECT `users`.`id`, `users`.`name`, `users`.`age` FROM `users` WHERE ((`users`.`age` >= ? AND LOWER(`users`.`name`) LIKE LOWER(?) ESCAPE '!') AND NOT (`users`.`id` IN (?, ?))) ORDER BY `users`.`age` DESC LIMIT 10 OFFSET 20"},
	} {
		b := q.q.builder(c.d)
		cols, _ := q.q.columns(b, nil)
		got, err := q.q.selectSQL(b, cols)
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", c.d.Name(), got, c.want, err)
		}
		if want := []any{18, "%a!_!%%", 1, 2}; !reflect.DeepEqual(b.args(), want) {
			t.Errorf("%s args = %v, want %v", c.d.Name(), b.args(), want)
		}
	}
	b := newBuilder(mysql{}, m.meta)
	if got := m.Offset(5).q.pageSQL(b); got != " LIMIT 18446744073709551615 OFFSET 5" {
		t.Errorf("mysql offset alone: %q", got)
	}
}

func TestPointerEmbed(t *testing.T) {
	m, err := modelOf(reflect.TypeFor[sqlUser](), "", "")
	if err != nil || m.PK == nil || m.PK.Column != "id" {
		t.Fatalf("model = %+v, %v", m, err)
	}
	var u sqlUser
	if err := (&cell{dst: fieldOf(reflect.ValueOf(&u).Elem(), m.PK.Index)}).Scan(int64(7)); err != nil || u.SQLBase == nil || u.ID != 7 {
		t.Fatalf("scan into a nil *Base: %+v, %v", u, err)
	}
	var none sqlUser
	if got := value(peek(reflect.ValueOf(&none).Elem(), m.PK.Index, m.PK.Type)); got != int64(0) {
		t.Fatalf("read through a nil *Base = %v", got)
	}
}

func TestViolations(t *testing.T) {
	for _, c := range []struct {
		d    Dialect
		msg  string
		kind violation
		col  string
	}{
		{postgres{}, `ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE 23505) Key (email)=(a@b.c) already exists.`, uniqueViolation, "email"},
		{mysql{}, `Error 1062 (23000): Duplicate entry 'a@b.c' for key 'users.email'`, uniqueViolation, "email"},
		{sqlite{}, `constraint failed: UNIQUE constraint failed: users.email (2067)`, uniqueViolation, "email"},
		{postgres{}, `ERROR: insert or update on table "posts" violates foreign key constraint (SQLSTATE 23503)`, foreignKeyViolation, ""},
	} {
		kind, col := c.d.Violation(errString(c.msg))
		if kind != c.kind || col != c.col {
			t.Errorf("%s %q: %v %q", c.d.Name(), c.msg, kind, col)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

type hidden struct{ ID int64 }

type hiddenPtr struct {
	*hidden
	Name string
}

func TestUnexportedPointerEmbed(t *testing.T) {
	if _, err := modelOf(reflect.TypeFor[hiddenPtr](), "", ""); err == nil {
		t.Fatal("an unexported *embed was accepted")
	}
}

func TestFunctionSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT "id" FROM "users" WHERE CAST(EXTRACT(YEAR FROM ("created")) AS INTEGER) >= $1`},
		{mysql{}, "SELECT `id` FROM `users` WHERE year((`created`)) >= ?"},
		{sqlite{}, `SELECT "id" FROM "users" WHERE CAST(strftime('%Y', ("created")) AS INTEGER) >= ?`},
	} {
		b := newBuilder(c.d, m.meta)
		b.ann = map[string]Expr{"created": rawSQL(c.d.Quote("created"))}
		w, err := m.Filter(Q{"created__year__gte": 2026}).q.whereSQL(b)
		if got := `SELECT ` + c.d.Quote("id") + ` FROM ` + c.d.Quote("users") + w; err != nil || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", c.d.Name(), got, c.want, err)
		}
	}
	b := newBuilder(postgres{}, m.meta)
	s, err := SQL("{0} || '{{x}}' || {1}", F("name"), "!").exprSQL(b)
	if err != nil || s != `"users"."name" || '{x}' || $1` || b.args()[0] != "!" {
		t.Fatalf("template = %s %v %v", s, b.args(), err)
	}
	if _, err := SQL("{2}", 1).exprSQL(b); err == nil {
		t.Fatal("a missing argument was accepted")
	}
}

func TestCaseSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	q := m.Annotate("tier", Case(When(Q{"age__gte": 65}, "senior"), When(Q{"age__lt": 18}, nil)).Else("adult")).
		Annotate("points", Switch(F("name")).Case("ali", SQL("{0} * {1}", F("age"), 2)).Else(1)).
		Annotate("adults", Count("id").Filter(Q{"age__gte": 18})).
		Exclude(Q{"tier": "adult"}).OrderBy("-points").q
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT (CASE WHEN "users"."age" >= $1 THEN CAST($2 AS TEXT) WHEN "users"."age" < $3 THEN NULL ELSE CAST($4 AS TEXT) END), (CASE "users"."name" WHEN $5 THEN "users"."age" * $6 ELSE CAST($7 AS BIGINT) END), (COUNT("users"."id") FILTER (WHERE "users"."age" >= $8)) FROM "users" WHERE NOT ((CASE WHEN "users"."age" >= $9 THEN CAST($10 AS TEXT) WHEN "users"."age" < $11 THEN NULL ELSE CAST($12 AS TEXT) END) = $13) ORDER BY (CASE "users"."name" WHEN $14 THEN "users"."age" * $15 ELSE CAST($16 AS BIGINT) END) DESC`},
		{mysql{}, "SELECT (CASE WHEN `users`.`age` >= ? THEN ? WHEN `users`.`age` < ? THEN NULL ELSE ? END), (CASE `users`.`name` WHEN ? THEN `users`.`age` * ? ELSE ? END), (COUNT(CASE WHEN `users`.`age` >= ? THEN `users`.`id` END)) FROM `users` WHERE NOT ((CASE WHEN `users`.`age` >= ? THEN ? WHEN `users`.`age` < ? THEN NULL ELSE ? END) = ?) ORDER BY (CASE `users`.`name` WHEN ? THEN `users`.`age` * ? ELSE ? END) DESC"},
		{sqlite{}, `SELECT (CASE WHEN "users"."age" >= ? THEN ? WHEN "users"."age" < ? THEN NULL ELSE ? END), (CASE "users"."name" WHEN ? THEN "users"."age" * ? ELSE ? END), (COUNT("users"."id") FILTER (WHERE "users"."age" >= ?)) FROM "users" WHERE NOT ((CASE WHEN "users"."age" >= ? THEN ? WHEN "users"."age" < ? THEN NULL ELSE ? END) = ?) ORDER BY (CASE "users"."name" WHEN ? THEN "users"."age" * ? ELSE ? END) DESC`},
	} {
		b := q.builder(c.d)
		var cols []string
		for _, name := range []string{"tier", "points", "adults"} {
			col, err := b.ref(name)
			if err != nil {
				t.Fatal(err)
			}
			cols = append(cols, col)
		}
		got, err := q.selectSQL(b, strings.Join(cols, ", "))
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", c.d.Name(), got, c.want, err)
		}
		if want := []any{65, "senior", 18, "adult", "ali", 2, 1, 18, 65, "senior", 18, "adult", "adult", "ali", 2, 1}; !reflect.DeepEqual(b.args(), want) {
			t.Errorf("%s args = %v, want %v", c.d.Name(), b.args(), want)
		}
	}
}

type sqlDoc struct {
	ID        int64
	Title     string
	Body      string
	Search    TSVector `orm:"generated"`
	Embedding Vector   `orm:"vector:3"`
}

func (sqlDoc) Generated() map[string]Expr {
	return map[string]Expr{"Search": SearchVector("title").Weight("A").Add(SearchVector("body").Weight("B")).Config("english")}
}

func (sqlDoc) Indexes() []Index {
	return []Index{GinIndex("search"), GinIndex("title").Trigram(), HnswIndex("embedding").Ops(Cosine).M(16).EfConstruction(64),
		IvfflatIndex("embedding").Lists(100).Name("docs_ivf"), FullTextIndex("body").Config("english")}
}

// sqlNote is sqlDoc as MySQL can hold it: no vector, a FULLTEXT index.
type sqlNote struct {
	ID     int64
	Title  string
	Body   string
	Search TSVector `orm:"generated"`
}

func (sqlNote) Generated() map[string]Expr {
	return map[string]Expr{"Search": SearchVector("title", "body").Config("english")}
}

func (sqlNote) Indexes() []Index {
	return []Index{FullTextIndex("search"), FullTextIndex("title", "body").Name("notes_text")}
}

func searchModel(t *testing.T, of any, table string) *model {
	t.Helper()
	m, err := modelOf(reflect.TypeOf(of), "", table)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSearchSQL(t *testing.T) {
	doc := SearchVector("title").Weight("A").Add(SearchVector("body"))
	q := SearchQuery("go -java")
	for _, c := range []struct {
		m     *model
		d     Dialect
		where string
		refs  []string
	}{
		{searchModel(t, sqlDoc{}, "docs"), postgres{},
			`WHERE (("docs"."search" @@ websearch_to_tsquery('english', $1) AND "docs"."search" @@ websearch_to_tsquery('english', $2) AND "docs"."title" % $3) AND (setweight(to_tsvector(COALESCE("docs"."title", '')), 'A') || to_tsvector(COALESCE("docs"."body", ''))) @@ websearch_to_tsquery($4))`,
			[]string{
				`(ts_rank((setweight(to_tsvector(COALESCE("docs"."title", '')), 'A') || to_tsvector(COALESCE("docs"."body", ''))), websearch_to_tsquery($5)))`,
				`(ts_headline(COALESCE("docs"."body", ''), websearch_to_tsquery($6)))`,
				`(("docs"."embedding" <=> $7))`,
				`((1.0 / (60 + ROW_NUMBER() OVER (ORDER BY (ts_rank((setweight(to_tsvector(COALESCE("docs"."title", '')), 'A') || to_tsvector(COALESCE("docs"."body", ''))), websearch_to_tsquery($8))) DESC)) + 1.0 / (60 + ROW_NUMBER() OVER (ORDER BY (("docs"."embedding" <=> $9)) ASC))))`,
				`(similarity("docs"."title", $10))`,
			}},
		{searchModel(t, sqlNote{}, "notes"), mysql{},
			"WHERE ((MATCH(`notes`.`search`) AGAINST(? IN BOOLEAN MODE) AND MATCH(`notes`.`search`) AGAINST(? IN BOOLEAN MODE) AND LOWER(`notes`.`title`) LIKE LOWER(?) ESCAPE '!') AND MATCH(`notes`.`title`, `notes`.`body`) AGAINST(? IN BOOLEAN MODE))",
			[]string{
				"(MATCH(`notes`.`title`, `notes`.`body`) AGAINST(? IN BOOLEAN MODE))",
				"(SUBSTRING(`notes`.`body`, GREATEST(1, LOCATE(?, `notes`.`body`) - 40), 160))",
			}},
	} {
		qs := query{m: c.m, where: []Cond{Q{"title__search": "go", "body__search": "web", "title__trigram_similar": "gp"}, Match(doc, q)}}
		b := qs.builder(c.d)
		b.ann = map[string]Expr{"rank": SearchRank(doc, q), "head": Headline("body", q), "d": CosineDistance("embedding", Vector{1, 2}), "sim": Similarity("title", "go")}
		b.ann["score"] = Fuse("-rank", "d")
		w, err := qs.whereSQL(b)
		if err != nil || w != " "+c.where {
			t.Errorf("%s where:\n got %s\nwant %s (%v)", c.d.Name(), w, " "+c.where, err)
		}
		for i, name := range []string{"rank", "head", "d", "score", "sim"}[:len(c.refs)] {
			if s, err := b.ref(name); err != nil || s != c.refs[i] {
				t.Errorf("%s %s:\n got %s\nwant %s (%v)", c.d.Name(), name, s, c.refs[i], err)
			}
		}
	}
	b := newBuilder(mysql{}, searchModel(t, sqlNote{}, "notes"))
	b.against([]string{"x"}, `web the "in practice" -java or go rust`)
	if want := `(+"web" +"in practice" -"java") (+"rust")`; !reflect.DeepEqual(b.args(), []any{want}) {
		t.Errorf("mysql boolean search %q, want %q", b.args(), want)
	}
	b = newBuilder(mysql{}, searchModel(t, sqlNote{}, "notes"))
	b.ann = map[string]Expr{"sim": Similarity("title", "go")}
	if _, err := b.ref("sim"); err == nil {
		t.Error("Similarity on MySQL")
	}
	if got, _ := (Vector{1, 0.5, -2}).Value(); got != "[1,0.5,-2]" {
		t.Errorf("vector value %v", got)
	}
	var v Vector
	if err := v.Scan([]byte("[1, 0.5,-2]")); err != nil || !slices.Equal(v, Vector{1, 0.5, -2}) {
		t.Errorf("vector scan %v, %v", v, err)
	}
}

func TestSearchDDL(t *testing.T) {
	docs := searchModel(t, sqlDoc{}, "docs")
	notes := searchModel(t, sqlNote{}, "notes")
	for _, c := range []struct {
		m    *model
		d    Dialect
		want []string
		err  string
	}{
		{docs, postgres{}, []string{
			"CREATE TABLE \"docs\" (\n  \"id\" BIGSERIAL PRIMARY KEY,\n  \"title\" TEXT NOT NULL,\n  \"body\" TEXT NOT NULL,\n  \"search\" tsvector GENERATED ALWAYS AS ((setweight(to_tsvector('english', COALESCE(\"title\", '')), 'A') || setweight(to_tsvector('english', COALESCE(\"body\", '')), 'B'))) STORED,\n  \"embedding\" vector(3)\n)",
			`CREATE INDEX "docs_search_gin" ON "docs" USING gin ("search")`,
			`CREATE INDEX "docs_title_gin" ON "docs" USING gin ("title" gin_trgm_ops)`,
			`CREATE INDEX "docs_embedding_hnsw" ON "docs" USING hnsw ("embedding" vector_cosine_ops) WITH (ef_construction = 64, m = 16)`,
			`CREATE INDEX "docs_ivf" ON "docs" USING ivfflat ("embedding" vector_l2_ops) WITH (lists = 100)`,
			`CREATE INDEX "docs_body_fulltext" ON "docs" USING gin (((to_tsvector('english', COALESCE("body", '')))))`,
		}, ""},
		{docs, sqlite{}, []string{
			"CREATE TABLE \"docs\" (\n  \"id\" INTEGER PRIMARY KEY AUTOINCREMENT,\n  \"title\" TEXT NOT NULL,\n  \"body\" TEXT NOT NULL,\n  \"search\" TEXT GENERATED ALWAYS AS (COALESCE(\"title\", '') || ' ' || COALESCE(\"body\", '')) STORED,\n  \"embedding\" TEXT\n)",
			`CREATE VIRTUAL TABLE IF NOT EXISTS "docs_fts" USING fts5("title", "body", content='docs', content_rowid='id', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS "docs_fts_ai" AFTER INSERT ON "docs" BEGIN INSERT INTO "docs_fts"(rowid, "title", "body") VALUES (new."id", new."title", new."body"); END;
CREATE TRIGGER IF NOT EXISTS "docs_fts_ad" AFTER DELETE ON "docs" BEGIN INSERT INTO "docs_fts"("docs_fts", rowid, "title", "body") VALUES ('delete', old."id", old."title", old."body"); END;
CREATE TRIGGER IF NOT EXISTS "docs_fts_au" AFTER UPDATE OF "id", "title", "body" ON "docs" BEGIN INSERT INTO "docs_fts"("docs_fts", rowid, "title", "body") VALUES ('delete', old."id", old."title", old."body"); INSERT INTO "docs_fts"(rowid, "title", "body") VALUES (new."id", new."title", new."body"); END;
INSERT INTO "docs_fts"("docs_fts") VALUES ('rebuild')`,
		}, ""},
		{docs, mysql{}, nil, "MySQL has no vector type"},
		{notes, postgres{}, []string{
			"CREATE TABLE \"notes\" (\n  \"id\" BIGSERIAL PRIMARY KEY,\n  \"title\" TEXT NOT NULL,\n  \"body\" TEXT NOT NULL,\n  \"search\" tsvector GENERATED ALWAYS AS ((to_tsvector('english', COALESCE(\"title\", '') || ' ' || COALESCE(\"body\", '')))) STORED\n)",
			`CREATE INDEX "notes_search_fulltext" ON "notes" USING gin ("search")`,
			`CREATE INDEX "notes_text" ON "notes" USING gin (((to_tsvector('simple', COALESCE("title", '') || ' ' || COALESCE("body", '')))))`,
		}, ""},
		{notes, mysql{}, []string{
			"CREATE TABLE `notes` (\n  `id` BIGINT PRIMARY KEY AUTO_INCREMENT,\n  `title` TEXT NOT NULL,\n  `body` TEXT NOT NULL,\n  `search` TEXT GENERATED ALWAYS AS (CONCAT_WS(' ', `title`, `body`)) STORED\n)",
			"CREATE FULLTEXT INDEX `notes_search_fulltext` ON `notes` (`search`)",
			"CREATE FULLTEXT INDEX `notes_text` ON `notes` (`title`, `body`)",
		}, ""},
	} {
		err := c.m.madeOn(c.d)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v, want %q", c.d.Name(), err, c.err)
			}
			continue
		}
		ts, terr := c.m.tables()
		if err != nil || terr != nil {
			t.Fatal(err, terr)
		}
		got := schema.For(c.d.Name()).Create(ts[0], false)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.d.Name(), got, c.want)
		}
	}
	if got := docs.extensions(); !slices.Equal(got, []string{"vector", "pg_trgm"}) {
		t.Errorf("extensions %v", got)
	}

	// A changed index is dropped and made again.
	ts, _ := docs.tables()
	changed := ts[0]
	changed.Indexes = []schema.Index{changed.Indexes[0], {Name: "docs_ivf", SQL: schema.Dialects{Postgres: `CREATE INDEX "docs_ivf" ON "docs" USING ivfflat ("embedding" vector_l2_ops) WITH (lists = 200)`}}}
	from := migration.NewState(ts, nil)
	ops, _ := migration.Diff(from, migration.NewState([]schema.Table{changed}, nil), func(string) bool { return false })
	steps, err := (migration.Migration{Operations: ops}).Forwards("postgres", from)
	if err != nil {
		t.Fatal(err)
	}
	var sqls []string
	for _, s := range steps {
		sqls = append(sqls, s.SQL)
	}
	if want := []string{`DROP INDEX "docs_title_gin"`, `DROP INDEX "docs_embedding_hnsw"`, `DROP INDEX "docs_ivf"`, `DROP INDEX "docs_body_fulltext"`, changed.Indexes[1].SQL.Postgres}; !slices.Equal(sqls, want) {
		t.Errorf("diff %q", sqls)
	}
}
