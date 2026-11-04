package orm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/internal/progress"
	"github.com/vuka-lang/orm/v2/migration"
)

func TestSplitSQL(t *testing.T) {
	got := splitSQL(`-- a comment; not split
CREATE TABLE "a;b" (x TEXT DEFAULT 'it''s; fine');
/* block; comment */ INSERT INTO t VALUES ('a\'; b');
;
SELECT 1`, "mysql")
	want := []string{
		`CREATE TABLE "a;b" (x TEXT DEFAULT 'it''s; fine')`,
		`INSERT INTO t VALUES ('a\'; b')`,
		`SELECT 1`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	got = splitSQL(`CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql;
SELECT $$;$$`, "postgres")
	if want := []string{`CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql`, `SELECT $$;$$`}; !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// Postgres: a backslash is an ordinary character in a string.
	got = splitSQL(`INSERT INTO t VALUES ('C:\'); SELECT 1;`, "postgres")
	if want := []string{`INSERT INTO t VALUES ('C:\')`, `SELECT 1`}; !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	src := "SELECT '?', \"a?\", data ?? 'k' FROM t -- a ?\nWHERE a = ? AND b IN (?, ?)"
	got, args, err := placeholders(DialectFor("postgres"), src, []any{1, "x", 3})
	if want := "SELECT '?', \"a?\", data ? 'k' FROM t \nWHERE a = $1 AND b IN ($2, $3)"; err != nil || got != want || !slices.Equal(args, []any{1, "x", 3}) {
		t.Fatalf("postgres:\n got %q, %v, %v\nwant %q", got, args, err, want)
	}
	if got, _, _ := placeholders(DialectFor("mysql"), `SELECT 'it\'s ?' WHERE a = ?`, []any{1}); got != `SELECT 'it\'s ?' WHERE a = ?` {
		t.Fatalf("mysql: %q", got)
	}
	if _, _, err := placeholders(DialectFor("sqlite"), "SELECT ?", nil); err == nil {
		t.Fatal("a ? with no argument")
	}
}

// TestMigrateDatabases applies migrations of two databases, one depending
// on the other's, in dependency order.
func TestMigrateDatabases(t *testing.T) {
	dbs := map[string]*DB{}
	for _, name := range []string{"", "other"} {
		dbs[name] = memDB(t)
	}
	table := func(name string) migration.Operation {
		return migration.CreateModel{Table: name, Fields: []migration.NamedField{migration.F("id", migration.BigAuto())}}
	}
	ordered, err := migration.Order([]migration.Migration{
		{Name: "0002_c", Dependencies: []string{"0001_a", "other:0001_b"}, Operations: []migration.Operation{table("c")}},
		{Name: "0001_b", DB: "other", Dependencies: []string{":0001_a"}, Operations: []migration.Operation{table("b")}},
		{Name: "0001_a", Operations: []migration.Operation{table("a")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	r := &migrator{
		ordered: ordered,
		open:    func(_ context.Context, name string) (*DB, error) { return dbs[name], nil },
		log:     func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) },
	}
	err = r.forwardAll(context.Background(), nil)
	r.close()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"migration applied: 0001_a", "migration applied: other:0001_b", "migration applied: 0002_c"}; !slices.Equal(log, want) {
		t.Fatalf("order %q", log)
	}
	for name, want := range map[string]string{"": "a", "other": "b"} {
		if _, err := dbs[name].sql.Exec("SELECT id FROM " + want); err != nil {
			t.Fatalf("%q has no %s: %v", name, want, err)
		}
	}
}

type planDoc struct {
	Model[planDoc]
	ID     int64
	Title  string
	Body   string
	Search TSVector `orm:"generated"`
}

func (planDoc) Meta() Meta { return Meta{DB: "search", Table: "docs"} }

func (planDoc) Generated() map[string]Expr {
	return map[string]Expr{"Search": SearchVector("title").Weight("A").Add(SearchVector("body").Weight("B")).Config("english")}
}

func (planDoc) Indexes() []Index { return []Index{GinIndex("search"), GinIndex("title").Trigram()} }

type planChunk struct {
	Model[planChunk]
	ID        int64
	Embedding Vector `orm:"vector:3"`
}

func (planChunk) Meta() Meta { return Meta{DB: "vectors", Table: "chunks"} }

func (planChunk) Indexes() []Index { return []Index{HnswIndex("embedding").Ops(Cosine)} }

// TestSearchPlan plans search models from nothing: their extensions
// (search_test.go declares them), generated column, vector and indexes,
// and nothing once replayed.
func TestSearchPlan(t *testing.T) {
	isolate(t)
	Register[planDoc]()
	Register[planChunk]()
	to, err := declaredState([]string{"search", "vectors"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	from := migration.NewState(nil, nil)
	ops, _ := migration.Diff(from, to, func(string) bool { return false })
	mg := migration.Migration{Name: "0001_initial", Operations: ops}
	steps, err := mg.Forwards("postgres", from.Clone())
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, s := range steps {
		all = append(all, s.SQL)
	}
	src := strings.Join(all, "\n")
	for _, want := range []string{`CREATE EXTENSION IF NOT EXISTS "pg_trgm"`, `CREATE EXTENSION IF NOT EXISTS "vector"`, `"search" tsvector GENERATED ALWAYS AS`, `"embedding" vector(3)`, `USING hnsw ("embedding" vector_cosine_ops)`, `USING gin ("title" gin_trgm_ops)`} {
		if !strings.Contains(src, want) {
			t.Fatalf("the migration lacks %q:\n%s", want, src)
		}
	}
	if err := mg.Apply(from); err != nil {
		t.Fatal(err)
	}
	if again, _ := migration.Diff(from, to, func(string) bool { return false }); len(again) != 0 {
		t.Fatalf("again: %v", again)
	}
	lite, err := mg.Forwards("sqlite", migration.NewState(nil, nil))
	if err != nil || slices.ContainsFunc(lite, func(s migration.Step) bool { return strings.Contains(s.SQL, "EXTENSION") }) {
		t.Fatalf("sqlite: %v", err)
	}
	// MySQL stores the vectors, unindexed.
	if my, err := declaredState([]string{"vectors"}, "mysql"); err != nil || len(my.Tables()) == 0 {
		t.Fatalf("vectors on MySQL: %v", err)
	}
	steps, err = mg.Forwards("mysql/9.3.0", migration.NewState(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var my strings.Builder
	for _, st := range steps {
		my.WriteString(st.SQL + "\n")
	}
	if !strings.Contains(my.String(), "`embedding` VECTOR(3)") || strings.Contains(my.String(), "hnsw") {
		t.Fatalf("mysql 9:\n%s", my.String())
	}
}

type plainRow struct {
	ID   int64
	Name string
}

// TestMigrateProgress shows orm migrate's migrations off a terminal: a
// line as each starts and ends, -v's statements, each database's total;
// a failing one ends failed.
func TestMigrateProgress(t *testing.T) {
	d := memDB(t)
	ordered, err := migration.Order([]migration.Migration{
		{Name: "0001_users", Operations: []migration.Operation{migration.CreateModel{Table: "users", Fields: []migration.NamedField{migration.F("id", migration.BigAuto())}}}},
		{Name: "0002_seed", Dependencies: []string{"0001_users"}, Operations: []migration.Operation{migration.RunSQL{SQL: "INSERT INTO users (id) VALUES (1); INSERT INTO users (id) VALUES (2);"}}},
		{Name: "0003_bad", Dependencies: []string{"0002_seed"}, Operations: []migration.Operation{migration.RunSQL{SQL: "INSERT INTO nowhere VALUES (1)"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w := &cliWatch{p: progress.New(&out, false, false), verbose: true}
	r := &migrator{ordered: ordered[:2], open: func(context.Context, string) (*DB, error) { return d, nil }, log: func(string, ...any) {}, watch: w}
	err = r.forwardAll(context.Background(), nil)
	r.close()
	if err != nil {
		t.Fatal(err)
	}
	w.summary()
	got := regexp.MustCompile(`\d+\.\ds`).ReplaceAllString(out.String(), "0.0s")
	want := "Applying 0001_users on the default database…\n" +
		"    CREATE TABLE \"users\" (\n      \"id\" INTEGER PRIMARY KEY AUTOINCREMENT\n    )\n" +
		"✓ Applied 0001_users on the default database  0.0s\n" +
		"Applying 0002_seed on the default database…\n" +
		"    INSERT INTO users (id) VALUES (1)\n" +
		"    INSERT INTO users (id) VALUES (2)\n" +
		"✓ Applied 0002_seed on the default database  0.0s\n" +
		"✓ Applied 2 migrations on the default database in 0.0s\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	out.Reset()
	w = &cliWatch{p: progress.New(&out, false, false)}
	r = &migrator{ordered: ordered, open: func(context.Context, string) (*DB, error) { return d, nil }, log: func(string, ...any) {}, watch: w}
	err = r.forwardAll(context.Background(), nil)
	r.close()
	if err == nil || !strings.Contains(out.String(), "Applying 0003_bad on the default database…\n✗ Applying 0003_bad on the default database  ") {
		t.Fatalf("a failing migration: %v\n%s", err, out.String())
	}

	out.Reset()
	w = &cliWatch{p: progress.New(&out, false, true)}
	w.summary()
	if out.Len() != 0 {
		t.Fatalf("quiet: %q", out.String())
	}
}

// Only types embedding Model are planned: an orm.For of a plain struct is
// a query over a table, not a model.
func TestPlanOnlyModels(t *testing.T) {
	_ = For[plainRow](On("plain"))
	to, err := declaredState([]string{"plain"}, "postgres")
	if err != nil || len(to.Tables()) != 0 {
		t.Fatalf("planned %v, %v", to.Tables(), err)
	}
}

// TestServePlan runs the planner as orm makemigrations does, for the
// models of database ledgers (model_internal_test.go).
func TestServePlan(t *testing.T) {
	isolate(t)
	Register[ledger]()
	out := filepath.Join(t.TempDir(), "plan.json")
	req, _ := json.Marshal(CLIRequest{Command: "plan", DB: "ledgers", Models: []string{"ledgers"}, Dialect: "sqlite", Number: 1, Package: "ledgers", Out: out})
	if err := serveCLI(context.Background(), string(req), strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	var res PlanResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	src := string(res.Source)
	if res.File != "0001_initial.go" || !strings.Contains(src, "package ledgers") || !strings.Contains(src, `DB:   "ledgers"`) || !strings.Contains(src, `Table: "ledger_rows"`) {
		t.Fatalf("%s\n%s", res.File, src)
	}
}
