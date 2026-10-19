package orm_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	m "github.com/vuka-lang/orm/migration"
	"github.com/vuka-lang/orm/ormtest"
)

// Visit lives on database "visits", made by the migration this file
// registers, which TestMigrate applies.
type Visit struct {
	orm.Model[Visit]
	ID   int64
	Path string
}

func (Visit) Meta() orm.Meta { return orm.Meta{DB: "visits"} }

func init() {
	m.Register(m.Migration{Name: "0001_initial", DB: "visits", Operations: []m.Operation{
		m.CreateModel{Table: "visits", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("path", m.Text())}},
	}})
}

func TestMigrate(t *testing.T) {
	useDB(t, "visits", false)
	ctx := context.Background()
	if err := orm.Migrate(ctx, orm.MigrateOn("visits")); err != nil {
		t.Fatal(err)
	}
	if err := (&Visit{Path: "/"}).Save(ctx); err != nil {
		t.Fatalf("the migrated table: %v", err)
	}
	names, err := orm.RawOn[string](ctx, orm.DBSchema{DB: "visits"}, "SELECT name FROM orm_migrations")
	if err != nil || !slices.Equal(names, []string{"0001_initial"}) {
		t.Fatalf("recorded %v, %v", names, err)
	}
	// Applied again, nothing is pending.
	if err := orm.Migrate(ctx, orm.MigrateOn("visits")); err != nil {
		t.Fatal(err)
	}
}

// library is three migrations of authors and books, the second with a
// data migration, applied and unapplied on a real database.
func library(log *[]string) []m.Migration {
	return []m.Migration{
		{Name: "0001_initial", Operations: []m.Operation{
			m.CreateModel{Table: "authors", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("name", m.Varchar(100).Unique())}},
			m.CreateModel{Table: "books", Fields: []m.NamedField{
				m.F("id", m.BigAuto()),
				m.F("author_id", m.BigInt().FK("authors", "id").OnDelete(m.Cascade)),
				m.F("title", m.Varchar(200)),
			}, Constraints: []m.Constraint{m.Unique("books_author_title", "author_id", "title")}},
		}},
		{Name: "0002_books", Dependencies: []string{"0001_initial"}, Operations: []m.Operation{
			m.AddField{Table: "books", Name: "pages", Field: m.Int().Default(0)},
			m.RenameField{Table: "books", Old: "title", New: "name"},
			m.AlterField{Table: "authors", Name: "name", Field: m.Varchar(200).Unique()},
			m.AddField{Table: "authors", Name: "bio", Field: m.Text().Null()},
			m.AddField{Table: "authors", Name: "country", Field: m.Varchar(2).Null().Index()},
			m.RunGo{
				Forward: func(ctx context.Context, tx m.Tx) error {
					if _, err := tx.ExecContext(ctx, "INSERT INTO authors (name, bio) VALUES ('Ali', 'a poet')"); err != nil {
						return err
					}
					// The ORM's queries on ctx run in the migration.
					n, err := orm.Raw[int64](ctx, "SELECT COUNT(*) FROM authors")
					*log = append(*log, "forward", strings.Repeat("x", int(n[0])))
					return err
				},
				Backward: func(ctx context.Context, tx m.Tx) error {
					*log = append(*log, "backward")
					_, err := tx.ExecContext(ctx, "DELETE FROM authors WHERE name = 'Ali'")
					return err
				},
			},
		}},
		{Name: "0003_tidy", Dependencies: []string{"0002_books"}, Operations: []m.Operation{
			m.RemoveField{Table: "books", Name: "pages"},
			m.RemoveConstraint{Table: "books", Name: "books_author_title"},
			m.AddConstraint{Table: "authors", Constraint: m.Check("authors_name_set", "name <> ''")},
			m.RenameModel{Old: "books", New: "volumes"},
		}},
	}
}

func TestApplyMigrations(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	var log []string
	migs := library(&log)
	exec := func(q string) error { _, err := orm.Exec(ctx, q); return err }
	applied := func() []string {
		t.Helper()
		names, err := orm.Raw[string](ctx, "SELECT name FROM orm_migrations ORDER BY name")
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	if err := orm.ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatal(err)
	}
	if got := applied(); !slices.Equal(got, []string{"0001_initial", "0002_books", "0003_tidy"}) {
		t.Fatalf("applied %v", got)
	}
	if !slices.Equal(log, []string{"forward", "x"}) {
		t.Fatalf("the data migration: %v", log)
	}
	for _, q := range []string{
		"INSERT INTO volumes (author_id, name) VALUES (1, 'Poems')",
		"INSERT INTO volumes (author_id, name) VALUES (1, 'Poems')", // no longer unique together
		"UPDATE authors SET country = 'TZ', bio = NULL",
	} {
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := orm.ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatalf("again, nothing to do: %v", err)
	}

	// Back to 0001: books as they were, the data migration undone.
	if err := exec("DELETE FROM volumes"); err != nil {
		t.Fatal(err)
	}
	if err := orm.ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if got := applied(); !slices.Equal(got, []string{"0001_initial"}) || !slices.Equal(log, []string{"forward", "x", "backward"}) {
		t.Fatalf("applied %v, log %v", got, log)
	}
	if err := exec("INSERT INTO authors (name) VALUES ('Neema')"); err != nil {
		t.Fatal(err)
	}
	if err := exec("INSERT INTO books (author_id, title) VALUES (2, 'Go')"); err != nil {
		t.Fatalf("books after unapplying: %v", err)
	}
	if err := exec("INSERT INTO books (author_id, title) VALUES (2, 'Go')"); err == nil {
		t.Fatal("unique together again after unapplying, yet a duplicate went in")
	}
	if err := exec("UPDATE authors SET bio = 'x'"); err == nil {
		t.Fatal("authors.bio outlived its migration")
	}

	// Zero, and forwards again.
	if err := orm.ApplyMigrations(ctx, d, "zero", migs...); err != nil {
		t.Fatal(err)
	}
	if err := exec("SELECT 1 FROM authors"); err == nil {
		t.Fatal("authors outlived zero")
	}
	if err := orm.ApplyMigrations(ctx, d, "0002", migs...); err != nil {
		t.Fatal(err)
	}
	if got := applied(); !slices.Equal(got, []string{"0001_initial", "0002_books"}) {
		t.Fatalf("to 0002: %v", got)
	}

	// A data migration with no Backward can't be unapplied.
	oneWay := append(migs, m.Migration{Name: "0004_one_way", Dependencies: []string{"0003_tidy"}, Operations: []m.Operation{
		m.RunGo{Forward: func(context.Context, m.Tx) error { return nil }},
	}})
	if err := orm.ApplyMigrations(ctx, d, "", oneWay...); err != nil {
		t.Fatal(err)
	}
	if err := orm.ApplyMigrations(ctx, d, "0003", oneWay...); err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("unapplying a one-way migration: %v", err)
	}
	// A failing step rolls its migration back (MySQL aside: it commits DDL).
	bad := append(oneWay, m.Migration{Name: "0005_bad", Dependencies: []string{"0004_one_way"}, Operations: []m.Operation{
		m.RunSQL{SQL: "SELECT * FROM nothing_here;", ReverseSQL: m.Noop},
	}})
	if err := orm.ApplyMigrations(ctx, d, "", bad...); err == nil || !strings.Contains(err.Error(), "0005_bad") {
		t.Fatalf("a failing migration: %v", err)
	}
	if got := applied(); slices.Contains(got, "0005_bad") {
		t.Fatalf("recorded a failed migration: %v", got)
	}
}

// TestApplySearchMigration makes the search models' kind of schema: an
// extension, a generated tsvector, and GIN indexes of it and of trigrams
// (MySQL: the generated text and its FULLTEXT index).
func TestApplySearchMigration(t *testing.T) {
	mysql := ormtest.Driver() == "mysql"
	if !mysql {
		needs(t, "pg_trgm")
	}
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	migs := []m.Migration{{Name: "0001_trgm", Operations: []m.Operation{m.CreateExtension{Name: "pg_trgm"}}}, {Name: "0002_pages", Dependencies: []string{"0001_trgm"}, Operations: []m.Operation{
		m.CreateModel{Table: "pages", Fields: []m.NamedField{
			m.F("id", m.BigAuto()),
			m.F("title", m.Text()),
			m.F("search", m.TSVector().Null().Generated(m.Dialects{
				Postgres: `to_tsvector('simple', COALESCE("title", ''))`,
				MySQL:    "COALESCE(`title`, '')",
				SQLite:   `COALESCE("title", '')`,
			})),
		}},
		m.AddIndex{Table: "pages", Index: m.Index{Name: "pages_search_gin", SQL: m.Dialects{Postgres: `CREATE INDEX "pages_search_gin" ON "pages" USING gin ("search")`,
			MySQL: "CREATE FULLTEXT INDEX `pages_search_gin` ON `pages` (`search`)"}}},
		m.AddIndex{Table: "pages", Index: m.Index{Name: "pages_title_gin", SQL: m.Dialects{Postgres: `CREATE INDEX "pages_title_gin" ON "pages" USING gin ("title" gin_trgm_ops)`}}},
	}}}
	if err := orm.ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, "INSERT INTO pages (title) VALUES ('go'), ('routing')"); err != nil {
		t.Fatal(err)
	}
	if mysql {
		if n, err := orm.Raw[int64](ctx, "SELECT id FROM pages WHERE MATCH(search) AGAINST (? IN BOOLEAN MODE)", "routing"); err != nil || !slices.Equal(n, []int64{2}) {
			t.Fatalf("FULLTEXT = %v, %v", n, err)
		}
	}
	// Back to 0001: the extension stays, other tables may need it.
	if err := orm.ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, "SELECT 1 FROM pages"); err == nil {
		t.Fatal("pages outlived its migration")
	}
}

// TestApplyVectorMigration applies the form makemigrations writes for a
// vector model (TestSearchPlan checks the planner writes it): on Postgres
// the extension, a vector(3) column, an HNSW index; MySQL's VECTOR(3)
// (VARBINARY before 9) and no index; SQLite's text.
func TestApplyVectorMigration(t *testing.T) {
	pg := ormtest.Driver() == "postgres"
	if pg {
		needs(t, "vector")
	}
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	migs := []m.Migration{{Name: "0001_vector", Operations: []m.Operation{m.CreateExtension{Name: "vector"}}}, {Name: "0002_items", Dependencies: []string{"0001_vector"}, Operations: []m.Operation{
		m.CreateModel{Table: "items", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("embedding", m.Vector(3))}},
		m.AddIndex{Table: "items", Index: m.Index{Name: "items_embedding_hnsw", SQL: m.Dialects{Postgres: `CREATE INDEX "items_embedding_hnsw" ON "items" USING hnsw ("embedding" vector_cosine_ops)`}}},
	}}}
	if err := orm.ApplyMigrations(ctx, d, "", migs...); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, "INSERT INTO items (embedding) VALUES (?), (?)", orm.Vector{0, 1, 0}, orm.Vector{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if vs, err := orm.Raw[orm.Vector](ctx, "SELECT embedding FROM items ORDER BY id"); err != nil || len(vs) != 2 || !slices.Equal(vs[1], orm.Vector{1, 0, 0}) {
		t.Fatalf("vectors %v, %v", vs, err)
	}
	if pg {
		near, err := orm.Raw[int64](ctx, "SELECT id FROM items ORDER BY embedding <=> ? LIMIT 1", orm.Vector{0.9, 0.1, 0})
		if err != nil || !slices.Equal(near, []int64{2}) {
			t.Fatalf("nearest %v, %v", near, err)
		}
	}
	if err := orm.ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, "SELECT 1 FROM items"); err == nil {
		t.Fatal("items outlived its migration")
	}
}
