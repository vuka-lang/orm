package orm

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

type ixUser struct {
	ID        int64
	Email     string
	Name      string
	FirstName string
	LastName  string
	Active    bool
	Tags      string
	CreatedAt time.Time
}

// declare is ixUser's indexes, as table users, when it declares ixs.
func declare(t *testing.T, ixs ...Index) ([]schema.Index, error) {
	t.Helper()
	m, err := modelOf(reflect.TypeFor[ixUser](), "", "users")
	if err != nil {
		t.Fatal(err)
	}
	m.indexes = ixs
	return m.declaredIndexes()
}

// TestIndexSQL is each row of the indexes' tables, written for every
// database it is made on ("" where it is made on none).
func TestIndexSQL(t *testing.T) {
	for _, c := range []struct {
		ix                      Index
		postgres, mysql, sqlite string
	}{
		{IndexOf("email").Unique(),
			`CREATE UNIQUE INDEX "users_email_idx" ON "users" ("email")`,
			"CREATE UNIQUE INDEX `users_email_idx` ON `users` (`email`)",
			`CREATE UNIQUE INDEX "users_email_idx" ON "users" ("email")`},
		{IndexOf(Lower.Of(F("email"))).Name("users_email_lower"),
			`CREATE INDEX "users_email_lower" ON "users" ((lower("email")))`,
			"CREATE INDEX `users_email_lower` ON `users` ((lower(`email`)))",
			`CREATE INDEX "users_email_lower" ON "users" ((lower("email")))`},
		{IndexOf("last_name", "first_name"),
			`CREATE INDEX "users_last_name_first_name_idx" ON "users" ("last_name", "first_name")`,
			"CREATE INDEX `users_last_name_first_name_idx` ON `users` (`last_name`, `first_name`)",
			`CREATE INDEX "users_last_name_first_name_idx" ON "users" ("last_name", "first_name")`},
		{IndexOf(Key("name").Desc()),
			`CREATE INDEX "users_name_idx" ON "users" ("name" DESC)`,
			"CREATE INDEX `users_name_idx` ON `users` (`name` DESC)",
			`CREATE INDEX "users_name_idx" ON "users" ("name" DESC)`},
		{IndexOf("email").Where(Q{"active": true}).On("postgres", "sqlite"),
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE "active" = TRUE`,
			"",
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE "active" = TRUE`},
		{IndexOf("email").Where(Q{"name__startswith": "a'b", "id__gt": 10}).On("postgres", "sqlite"),
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE ("id" > 10 AND "name" LIKE 'a''b%' ESCAPE '!')`,
			"",
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE ("id" > 10 AND "name" LIKE 'a''b%' ESCAPE '!')`},
		{IndexOf(Key("name").NullsFirst()).On("postgres", "sqlite"),
			`CREATE INDEX "users_name_idx" ON "users" ("name" NULLS FIRST)`,
			"",
			`CREATE INDEX "users_name_idx" ON "users" ("name")`},
		{IndexOf(Key("name").Desc().NullsLast()).On("postgres", "sqlite"),
			`CREATE INDEX "users_name_idx" ON "users" ("name" DESC NULLS LAST)`,
			"",
			`CREATE INDEX "users_name_idx" ON "users" ("name" DESC)`},
		{IndexOf(Key("name").Collate("C")),
			`CREATE INDEX "users_name_idx" ON "users" ("name" COLLATE "C")`,
			"CREATE INDEX `users_name_idx` ON `users` ((`name` COLLATE C))",
			`CREATE INDEX "users_name_idx" ON "users" ("name" COLLATE "C")`},
		{IndexOf(Key("name").Ops("text_pattern_ops")).On("postgres"),
			`CREATE INDEX "users_name_idx" ON "users" ("name" text_pattern_ops)`, "", ""},
		{IndexOf(Key("email").Prefix(191)).On("mysql"),
			"", "CREATE INDEX `users_email_idx` ON `users` (`email`(191))", ""},
		{IndexOf("email").Using("hash").On("postgres", "mysql"),
			`CREATE INDEX "users_email_hash" ON "users" USING hash ("email")`,
			"CREATE INDEX `users_email_hash` ON `users` (`email`) USING HASH", ""},
		{IndexOf("created_at").Using("brin").On("postgres"),
			`CREATE INDEX "users_created_at_brin" ON "users" USING brin ("created_at")`, "", ""},
		{IndexOf("tags").Using("spgist").On("postgres"),
			`CREATE INDEX "users_tags_spgist" ON "users" USING spgist ("tags")`, "", ""},
		{IndexOf("email").Include("name", "active").On("postgres"),
			`CREATE INDEX "users_email_idx" ON "users" ("email") INCLUDE ("name", "active")`, "", ""},
		{IndexOf("email").With("fillfactor", 70).With("deduplicate_items", "off").On("postgres"),
			`CREATE INDEX "users_email_idx" ON "users" ("email") WITH (deduplicate_items = 'off', fillfactor = 70)`, "", ""},
		{GinIndex("tags"),
			`CREATE INDEX "users_tags_gin" ON "users" USING gin ("tags")`, "", ""},
		{GinIndex("tags").Trigram(),
			`CREATE INDEX "users_tags_gin" ON "users" USING gin ("tags" gin_trgm_ops)`, "", ""},
		{GistIndex("tags").Trigram(),
			`CREATE INDEX "users_tags_gist" ON "users" USING gist ("tags" gist_trgm_ops)`, "", ""},
		{FullTextIndex("name"),
			`CREATE INDEX "users_name_fulltext" ON "users" USING gin (((to_tsvector('simple', COALESCE("name", '')))))`,
			"CREATE FULLTEXT INDEX `users_name_fulltext` ON `users` (`name`)", ""},
		{RawIndex("users_email_prefix", Dialects{MySQL: "CREATE INDEX users_email_prefix ON users (email(191))"}),
			"", "CREATE INDEX users_email_prefix ON users (email(191))", ""},
		{IndexOf("email").On(), "", "", ""},
		// Else: one index, as each database can make it.
		{IndexOf("email").Where(Q{"active": true}).Else(IndexOf("email")),
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE "active" = TRUE`,
			"CREATE INDEX `users_email_idx` ON `users` (`email`)",
			`CREATE INDEX "users_email_idx" ON "users" ("email") WHERE "active" = TRUE`},
		{GinIndex("tags").Else(IndexOf("tags").Name("users_tags_gin")),
			`CREATE INDEX "users_tags_gin" ON "users" USING gin ("tags")`,
			"CREATE INDEX `users_tags_gin` ON `users` (`tags`)",
			`CREATE INDEX "users_tags_gin" ON "users" ("tags")`},
	} {
		got, err := declare(t, c.ix)
		if err != nil {
			t.Errorf("%+v: %v", c.ix, err)
			continue
		}
		want := schema.Dialects{Postgres: c.postgres, MySQL: c.mysql, SQLite: c.sqlite}
		if want.Zero() {
			if len(got) != 0 {
				t.Errorf("made %+v, want none", got)
			}
			continue
		}
		if len(got) != 1 || got[0].SQL != want {
			t.Errorf("got %+v\nwant %+v", got, want)
		}
	}
}

// An Else of another name is an index of its own, made where the first
// one isn't.
func TestIndexElseNamed(t *testing.T) {
	got, err := declare(t, GinIndex("tags").Else(IndexOf("tags")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "users_tags_gin" || got[0].SQL.MySQL != "" || got[1].Name != "users_tags_idx" ||
		got[1].SQL.Postgres != "" || got[1].SQL.MySQL == "" || got[1].SQL.SQLite == "" {
		t.Fatalf("%+v", got)
	}
}

// TestIndexRule is the one rule: an index a database can't make fails,
// naming the database, what it lacks and the ways out.
func TestIndexRule(t *testing.T) {
	_, err := declare(t, IndexOf("email").Where(Q{"active": true}))
	want := `orm: ixUser: MySQL has no partial index (Where on users_email_idx).
  .On("postgres", "sqlite")          make it only there
  .Else(orm.IndexOf("email"))        make this instead on MySQL`
	if err == nil || err.Error() != want {
		t.Fatalf("got\n%v\nwant\n%s", err, want)
	}
	for _, c := range []struct {
		ix   Index
		want string
	}{
		{IndexOf("email").Using("hash"), "SQLite has no hash index (Using on users_email_hash)"},
		{IndexOf(Key("name").NullsFirst()), "MySQL has no NULLS FIRST order (NullsFirst on users_name_idx)"},
		{IndexOf(Key("name").NullsLast()).On("sqlite"), "SQLite has no NULLS LAST on an ascending key (NullsLast on users_name_idx)"},
		{IndexOf(Key("name").Ops("text_pattern_ops")), "MySQL has no operator class (Ops on users_name_idx)"},
		{IndexOf(Key("email").Prefix(10)), "Postgres has no key prefix (Prefix on users_email_idx)"},
		{IndexOf("email").Include("name"), "MySQL has no covering index (Include on users_email_idx)"},
		{IndexOf("email").With("fillfactor", 70), "MySQL has no storage parameters (With on users_email_idx)"},
		{GinIndex("tags").On("postgres", "mysql"), "MySQL has no GIN index (GinIndex on users_tags_gin)"},
		{GinIndex("tags").Unique(), "Postgres has no unique GIN index (Unique on users_tags_gin)"},
		{FullTextIndex("name").Unique().On("mysql"), "MySQL has no unique full-text index (Unique on users_name_fulltext)"},
		// A fallback is held to the rule too.
		{IndexOf("email").Where(Q{"active": true}).Else(IndexOf(Key("email").NullsFirst())),
			"MySQL has no NULLS FIRST order (NullsFirst on users_email_idx)"},
	} {
		_, err := declare(t, c.ix)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.ix, err, c.want)
		}
	}
	// Either way out passes, and so does a fallback with its own.
	for _, ix := range []Index{
		IndexOf("email").Where(Q{"active": true}).On("postgres", "sqlite"),
		IndexOf("email").Where(Q{"active": true}).Else(IndexOf(Key("email").NullsFirst()).Else(IndexOf("email"))),
		IndexOf("email").Using("hash").Else(IndexOf("email")),
	} {
		if _, err := declare(t, ix); err != nil {
			t.Errorf("%+v: %v", ix, err)
		}
	}
	// SQLite indexes NULLs as it orders them.
	if got, err := declare(t, IndexOf(Key("name").Desc().NullsLast()).On("sqlite")); err != nil || got[0].SQL.SQLite != `CREATE INDEX "users_name_idx" ON "users" ("name" DESC)` {
		t.Errorf("%+v, %v", got, err)
	}
}

// TestIndexChecks is what registration refuses whatever the database.
func TestIndexChecks(t *testing.T) {
	for _, c := range []struct {
		ixs  []Index
		want string
	}{
		{[]Index{IndexOf(Lower.Of(F("email")))}, "an index of expressions alone needs a Name"},
		{[]Index{IndexOf("email"), IndexOf("email").Unique()}, "declares two indexes named users_email_idx"},
		{[]Index{IndexOf("email").Name("x"), IndexOf("name").Name("x")}, "declares two indexes named x"},
		{[]Index{IndexOf("email").On("oracle")}, `On names postgres, mysql or sqlite, not "oracle"`},
		{[]Index{IndexOf()}, "an index needs keys"},
		{[]Index{IndexOf("nope")}, "nope"},
		{[]Index{IndexOf(Lower.Of(Value("x"))).Name("x")}, "its keys are the table's own columns, and take no values"},
		{[]Index{IndexOf("email").Where(Q{"created_at__gt": time.Now()}).On("sqlite")}, "its values are bools, numbers, strings or nil"},
		{[]Index{IndexOf("email").Include("nope").On("postgres")}, "Include names nope"},
		{[]Index{IndexOf("email").Using("bitmap")}, `no access method "bitmap"`},
		{[]Index{IndexOf("email").With("fill factor", 1).On("postgres")}, `With("fill factor")`},
		{[]Index{IndexOf(Key("name").Collate("C'); DROP")).On("postgres")}, "no collation"},
		{[]Index{RawIndex("", Dialects{MySQL: "x"})}, "a RawIndex needs a name"},
		{[]Index{IndexOf("name").Trigram()}, "Trigram is a GinIndex's or a GistIndex's"},
	} {
		_, err := declare(t, c.ixs...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.ixs, err, c.want)
		}
	}
}

type ixBad struct {
	ID    int64
	Email string
}

func (ixBad) Indexes() []Index { return []Index{IndexOf("email").Where(Q{"id__gt": 1})} }

type ixAuthor struct {
	ID   int64
	Name string
}

type ixBook struct {
	ID       int64
	Title    string
	AuthorID int64
	Author   *ixAuthor
}

func (ixBook) Indexes() []Index {
	return []Index{IndexOf(Lower.Of(F("author__name"))).Name("books_author").On("sqlite")}
}

// A model whose index can't be made, or reads another table, fails as it
// is registered.
func TestIndexRegistration(t *testing.T) {
	if _, err := modelOf(reflect.TypeFor[ixBad](), "", ""); err == nil || !strings.Contains(err.Error(), "MySQL has no partial index") {
		t.Errorf("ixBad: %v", err)
	}
	if _, err := modelOf(reflect.TypeFor[ixBook](), "", ""); err == nil || !strings.Contains(err.Error(), "the row's own columns") {
		t.Errorf("ixBook: %v", err)
	}
}
