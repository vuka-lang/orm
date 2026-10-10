package migration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/internal/schema"
)

func TestOrder(t *testing.T) {
	list := []Migration{
		{Name: "0002_b", Dependencies: []string{"0001_a", "legacy:0001_x"}},
		{Name: "0001_x", DB: "legacy"},
		{Name: "0001_a"},
		{Name: "0002_y", DB: "legacy", Dependencies: []string{"0001_x", ":0002_b"}},
	}
	got, err := Order(list)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, mg := range got {
		keys = append(keys, mg.key())
	}
	if want := []string{":0001_a", "legacy:0001_x", ":0002_b", "legacy:0002_y"}; !slices.Equal(keys, want) {
		t.Fatalf("order %v, want %v", keys, want)
	}
	if l := Leaves(got, ""); !slices.Equal(l, []string{"0002_b"}) {
		t.Fatalf("leaves %v", l)
	}
	if mg, err := Find(got, "legacy", "0002"); err != nil || mg.Name != "0002_y" {
		t.Fatalf("find by prefix: %v, %v", mg.Name, err)
	}
	if _, err := Find(got, "", "0003"); err == nil {
		t.Fatal("found a migration that isn't")
	}
	if _, err := Order([]Migration{{Name: "0001", Dependencies: []string{"0000"}}}); err == nil || !strings.Contains(err.Error(), "depends on :0000") {
		t.Fatalf("an unknown dependency: %v", err)
	}
	if _, err := Order([]Migration{{Name: "a", Dependencies: []string{"b"}}, {Name: "b", Dependencies: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("a cycle: %v", err)
	}
}

// users and posts, as a model state.
func blog() *State {
	return NewState([]schema.Table{
		{Name: "users", Columns: []schema.Column{
			{Name: "id", Kind: schema.Int, PK: true, Auto: true},
			{Name: "email", Kind: schema.String, Unique: true},
			{Name: "name", Kind: schema.String, Index: true},
		}},
		{Name: "posts", Columns: []schema.Column{
			{Name: "id", Kind: schema.Int, PK: true, Auto: true},
			{Name: "user_id", Kind: schema.Int, FK: &schema.ForeignKey{Table: "users", Ref: "id", OnDelete: "cascade"}},
			{Name: "title", Kind: schema.String},
		}, Indexes: []schema.Index{{Name: "posts_title_gin", SQL: schema.Dialects{Postgres: `CREATE INDEX "posts_title_gin" ON "posts" USING gin ("title" gin_trgm_ops)`}}}},
	}, []string{"pg_trgm"})
}

func never(string) bool { return false }

func TestDiffAndReplay(t *testing.T) {
	ops, notes := Diff(NewState(nil, nil), blog(), never)
	var got []string
	for _, op := range ops {
		got = append(got, Describe(op))
	}
	if want := []string{"create extension pg_trgm", "create model users", "create model posts"}; !slices.Equal(got, want) {
		t.Fatalf("initial %v, notes %v", got, notes)
	}
	mg := Migration{Name: "0001_initial", Operations: ops}
	s := NewState(nil, nil)
	if err := mg.Apply(s); err != nil {
		t.Fatal(err)
	}
	if again, _ := Diff(s, blog(), never); len(again) != 0 {
		t.Fatalf("the replay differs from the models: %v", again)
	}
	users := s.tables[s.find("users")]
	if c, _ := users.Column("email"); c.UniqueName != "uniq_users_email" {
		t.Fatalf("replay names the constraint: %+v", c)
	}
	if err := mg.Apply(s); err == nil || !strings.Contains(err.Error(), "create model users: the table exists") {
		t.Fatalf("replaying twice: %v", err)
	}
}

func TestDiffOperations(t *testing.T) {
	from := NewState(nil, nil)
	ops, _ := Diff(from, blog(), never)
	if err := (Migration{Name: "0001", Operations: ops}).Apply(from); err != nil {
		t.Fatal(err)
	}
	// A check constraint and a default the migrations added stay.
	for _, op := range []Operation{
		AddConstraint{"users", Check("users_name_set", "name <> ''")},
		AlterField{"posts", "title", Text().DefaultSQL("'untitled'")},
	} {
		if err := op.mutate(from); err != nil {
			t.Fatal(err)
		}
	}

	to := blog()
	users, posts := &to.tables[0], &to.tables[1]
	users.Columns[1] = schema.Column{Name: "mail", Kind: schema.String, Unique: true} // email renamed
	users.Columns = append(users.Columns, schema.Column{Name: "age", Kind: schema.Int32})
	users.Columns[2].Index = false                                                  // name's index dropped
	posts.Columns[2] = schema.Column{Name: "title", Kind: schema.String, Size: 200} // altered
	posts.Constraints = []schema.Constraint{{Name: "posts_user_title", Columns: []string{"user_id", "title"}}}
	posts.Indexes = nil
	to.tables = append(to.tables, schema.Table{Name: "tags", Columns: []schema.Column{{Name: "id", Kind: schema.Int, PK: true, Auto: true}}})

	describeAll := func(ops []Operation) []string {
		var out []string
		for _, op := range ops {
			out = append(out, Describe(op))
		}
		return out
	}
	var asked []string
	yes := func(q string) bool { asked = append(asked, q); return true }
	ops, notes := Diff(from, to, yes)
	want := []string{
		"create model tags",
		"rename field users.email to mail",
		"add field users.age",
		"alter field users.name",
		"remove index posts_title_gin from posts",
		"alter field posts.title",
		"add constraint posts_user_title on posts",
	}
	if got := describeAll(ops); !slices.Equal(got, want) {
		t.Fatalf("ops\n got %q\nwant %q", got, want)
	}
	if !slices.Equal(asked, []string{"Did you rename users.email to users.mail?"}) {
		t.Fatalf("asked %q", asked)
	}
	if !strings.Contains(notes[2], "adds the NOT NULL column users.age") {
		t.Fatalf("notes %q", notes)
	}
	if f := ops[5].(AlterField).Field; f.c.Default != "'untitled'" || f.c.Size != 200 {
		t.Fatalf("an altered field keeps its default: %+v", f.c)
	}

	// No: the column is removed and another added, with a note.
	ops, notes = Diff(from, to, never)
	got := describeAll(ops)
	if !slices.Contains(got, "remove field users.email") || !slices.Contains(got, "add field users.mail") || !strings.Contains(strings.Join(notes, "\n"), "if it was renamed") {
		t.Fatalf("ops %q, notes %q", got, notes)
	}

	// A table gone where one alike appeared.
	moved := blog()
	moved.tables[0].Name = "accounts"
	moved.tables[1].Columns[1].FK = &schema.ForeignKey{Table: "accounts", Ref: "id", OnDelete: "cascade"}
	start := NewState(nil, nil)
	ops, _ = Diff(start, blog(), never)
	_ = (Migration{Operations: ops}).Apply(start)
	ops, _ = Diff(start, moved, yes)
	if got := describeAll(ops); !slices.Equal(got, []string{"rename model users to accounts"}) {
		t.Fatalf("rename model: %q", got)
	}
	ops, _ = Diff(start, moved, never)
	if got := describeAll(ops); !slices.Equal(got, []string{"create model accounts", "alter field posts.user_id", "delete model users"}) {
		t.Fatalf("no rename: %q", got)
	}
}

func TestSource(t *testing.T) {
	mg := Migration{
		Name:         "0002_shop",
		DB:           "legacy",
		Dependencies: []string{"0001_initial"},
		Operations: []Operation{
			CreateModel{
				Table: "orders",
				Fields: []NamedField{
					F("id", BigAuto()),
					F("customer_id", BigInt().FK("customers", "id").OnDelete(Cascade)),
					F("code", Varchar(32).Unique()),
					F("note", Text().Null()),
					F("total", Float().DefaultSQL("0")),
					F("search", TSVector().Null().Generated(Dialects{Postgres: `to_tsvector('simple', "note")`, MySQL: "`note`"})),
					F("embedding", Vector(3).Null()),
				},
				Constraints: []Constraint{Unique("orders_customer_code", "customer_id", "code")},
				Indexes:     []Index{{Name: "orders_search_gin", SQL: Dialects{Postgres: `CREATE INDEX "orders_search_gin" ON "orders" USING gin ("search")`}}},
			},
			AddField{"customers", "age", Int().Null().Index()},
			RenameField{"customers", "mail", "email"},
			AlterField{"customers", "email", Varchar(320)},
			RemoveField{"customers", "bio"},
			AddIndex{"customers", Index{Name: "customers_email_gin", SQL: Dialects{Postgres: "CREATE INDEX x"}}},
			RemoveIndex{"customers", "old"},
			AddConstraint{"customers", Check("age_positive", "age >= 0")},
			RemoveConstraint{"customers", "old_unique"},
			RenameModel{"carts", "baskets"},
			DeleteModel{"wishlists"},
			CreateExtension{"vector"},
			RunSQL{SQL: "UPDATE customers SET age = 0;", ReverseSQL: Noop, Dialect: "postgres"},
		},
	}
	src, err := Source("legacy", mg, []string{"", "adds a column"})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "0002_shop.go.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != string(want) {
		t.Fatalf("source differs from %s (UPDATE_GOLDEN=1 rewrites it):\n%s", golden, src)
	}
	empty, err := Source("migrations", Migration{Name: "0003_custom", Operations: []Operation{RunGo{}}}, nil)
	if err != nil || !strings.Contains(string(empty), `"context"`) || !strings.Contains(string(empty), "Forward: func(ctx context.Context, tx m.Tx) error") {
		t.Fatalf("an empty migration: %v\n%s", err, empty)
	}
}

// sqlOf is the SQL of each step of mg on dialect, forwards from s (left
// as mg leaves it) or back.
func sqlOf(t *testing.T, mg Migration, dialect string, s *State, back bool) []string {
	t.Helper()
	var steps []Step
	var err error
	if back {
		steps, err = mg.Backwards(dialect, s)
	} else {
		steps, err = mg.Forwards(dialect, s)
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, st := range steps {
		out = append(out, st.SQL)
	}
	return out
}

func TestAlterFieldSQL(t *testing.T) {
	// A key widened, a flag made boolean, a unique column made NOT NULL,
	// an index dropped from a column going back to TEXT.
	initial := Migration{Name: "0001", Operations: []Operation{CreateModel{Table: "t", Fields: []NamedField{
		F("id", Auto()), F("flag", BigInt()), F("email", Text().Unique().Null()), F("x", Text().Index()),
	}}}}
	alter := Migration{Name: "0002", Operations: []Operation{
		AlterField{"t", "id", BigAuto()},
		AlterField{"t", "flag", Bool()},
		AlterField{"t", "email", Text().Unique()},
		AlterField{"t", "x", Text()},
	}}
	for _, c := range []struct {
		dialect string
		want    []string
	}{
		{"postgres", []string{
			`ALTER TABLE "t" ALTER COLUMN "id" TYPE BIGINT USING "id"::BIGINT`,
			`ALTER SEQUENCE "t_id_seq" AS BIGINT`,
			`ALTER TABLE "t" ALTER COLUMN "flag" TYPE BOOLEAN USING "flag" <> 0`,
			`ALTER TABLE "t" ALTER COLUMN "email" SET NOT NULL`,
			`DROP INDEX "idx_t_x"`,
		}},
		{"mysql", []string{
			"ALTER TABLE `t` MODIFY COLUMN `id` BIGINT NOT NULL AUTO_INCREMENT",
			"ALTER TABLE `t` MODIFY COLUMN `flag` BOOLEAN NOT NULL",
			"ALTER TABLE `t` MODIFY COLUMN `email` VARCHAR(255) NOT NULL",
			"DROP INDEX `idx_t_x` ON `t`",
			"ALTER TABLE `t` MODIFY COLUMN `x` TEXT NOT NULL",
		}},
	} {
		s := NewState(nil, nil)
		if err := initial.Apply(s); err != nil {
			t.Fatal(err)
		}
		if got := sqlOf(t, alter, c.dialect, s, false); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.dialect, got, c.want)
		}
	}
	// SQLite rebuilds the table, rows copied.
	s := NewState(nil, nil)
	_ = initial.Apply(s)
	got := sqlOf(t, Migration{Operations: []Operation{AlterField{"t", "flag", Bool()}}}, "sqlite", s, false)
	if len(got) != 5 || !strings.HasPrefix(got[0], `CREATE TABLE "orm__new_t"`) || got[1] != `INSERT INTO "orm__new_t" ("id", "flag", "email", "x") SELECT "id", "flag", "email", "x" FROM "t"` ||
		got[2] != `DROP TABLE "t"` || got[3] != `ALTER TABLE "orm__new_t" RENAME TO "t"` || got[4] != `CREATE INDEX "idx_t_x" ON "t" ("x")` {
		t.Fatalf("sqlite rebuild:\n%s", strings.Join(got, "\n"))
	}
}

func TestOperationSQL(t *testing.T) {
	initial := Migration{Name: "0001", Operations: []Operation{
		CreateModel{Table: "users", Fields: []NamedField{F("id", BigAuto()), F("email", Varchar(255).Unique())}},
		CreateModel{Table: "posts", Fields: []NamedField{F("id", BigAuto()), F("user_id", BigInt().FK("users", "id").OnDelete(Cascade)), F("title", Text())},
			Constraints: []Constraint{Unique("posts_user_title", "user_id", "title")}},
	}}
	pg := NewState(nil, nil)
	got := sqlOf(t, initial, "postgres", pg, false)
	want := []string{
		"CREATE TABLE \"users\" (\n  \"id\" BIGSERIAL PRIMARY KEY,\n  \"email\" VARCHAR(255) NOT NULL,\n  CONSTRAINT \"uniq_users_email\" UNIQUE (\"email\")\n)",
		"CREATE TABLE \"posts\" (\n  \"id\" BIGSERIAL PRIMARY KEY,\n  \"user_id\" BIGINT NOT NULL,\n  \"title\" TEXT NOT NULL,\n  CONSTRAINT \"posts_user_title\" UNIQUE (\"user_id\", \"title\"),\n  CONSTRAINT \"fk_posts_user_id\" FOREIGN KEY (\"user_id\") REFERENCES \"users\" (\"id\") ON DELETE CASCADE\n)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("create:\n got %q\nwant %q", got, want)
	}
	next := Migration{Name: "0002", Operations: []Operation{
		RenameModel{"users", "accounts"},
		AddField{"posts", "views", Int().Default(0)},
		RenameField{"posts", "title", "headline"},
		RemoveConstraint{"posts", "posts_user_title"},
		AlterField{"posts", "user_id", BigInt().Null().FK("accounts", "id").OnDelete(SetNull)},
		RunSQL{SQL: "UPDATE posts SET views = 1", ReverseSQL: Noop},
	}}
	before := pg.Clone()
	got = sqlOf(t, next, "postgres", pg, false)
	want = []string{
		`ALTER TABLE "users" RENAME TO "accounts"`,
		`ALTER TABLE "posts" ADD COLUMN "views" INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE "posts" RENAME COLUMN "title" TO "headline"`,
		`ALTER TABLE "posts" DROP CONSTRAINT "posts_user_title"`,
		`ALTER TABLE "posts" DROP CONSTRAINT "fk_posts_user_id"`,
		`ALTER TABLE "posts" ALTER COLUMN "user_id" DROP NOT NULL`,
		`ALTER TABLE "posts" ADD CONSTRAINT "fk_posts_user_id" FOREIGN KEY ("user_id") REFERENCES "accounts" ("id") ON DELETE SET NULL`,
		"UPDATE posts SET views = 1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("forwards:\n got %q\nwant %q", got, want)
	}
	got = sqlOf(t, next, "postgres", before, true)
	want = []string{
		Noop,
		`ALTER TABLE "posts" DROP CONSTRAINT "fk_posts_user_id"`,
		`ALTER TABLE "posts" ALTER COLUMN "user_id" SET NOT NULL`,
		`ALTER TABLE "posts" ADD CONSTRAINT "fk_posts_user_id" FOREIGN KEY ("user_id") REFERENCES "accounts" ("id") ON DELETE CASCADE`,
		`ALTER TABLE "posts" ADD CONSTRAINT "posts_user_title" UNIQUE ("user_id", "headline")`,
		`ALTER TABLE "posts" RENAME COLUMN "headline" TO "title"`,
		`ALTER TABLE "posts" DROP COLUMN "views"`,
		`ALTER TABLE "accounts" RENAME TO "users"`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backwards:\n got %q\nwant %q", got, want)
	}
	my := NewState(nil, nil)
	_ = initial.Apply(my)
	if got := sqlOf(t, Migration{Operations: []Operation{RemoveField{"posts", "user_id"}}}, "mysql", my, false); !slices.Equal(got, []string{
		"ALTER TABLE `posts` DROP FOREIGN KEY `fk_posts_user_id`",
		"ALTER TABLE `posts` DROP COLUMN `user_id`",
	}) {
		t.Fatalf("mysql drops the foreign key first: %q", got)
	}
	irreversible := Migration{Operations: []Operation{RunGo{Forward: func(context.Context, Tx) error { return nil }}}}
	if _, err := irreversible.Backwards("sqlite", NewState(nil, nil)); err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("a RunGo with no Backward: %v", err)
	}
	vec := Migration{Operations: []Operation{CreateModel{Table: "v", Fields: []NamedField{F("e", Vector(3))}}}}
	for server, typ := range map[string]string{"mysql": "VARBINARY(12)", "mysql/9.3.0": "VECTOR(3)"} {
		if got := sqlOf(t, vec, server, NewState(nil, nil), false); len(got) != 1 || !strings.Contains(got[0], "`e` "+typ) {
			t.Fatalf("a vector on %s: %q", server, got)
		}
	}
}
