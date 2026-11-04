package orm

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/migration"
)

type ckPost struct {
	ID     int64
	Title  string
	Posts  int
	Status string
}

// postsState is ckPost's table, ck_posts, as migrations make it when it
// declares checks.
func postsState(t *testing.T, checks ...RowCheck) *migration.State {
	t.Helper()
	m, err := buildModel(reflect.TypeFor[ckPost](), "", "ck_posts")
	if err != nil {
		t.Fatal(err)
	}
	m.checks = checks
	if m.rules, err = m.declaredRules(); err != nil {
		t.Fatal(err)
	}
	ts, err := m.tables()
	if err != nil {
		t.Fatal(err)
	}
	return migration.NewState(ts, nil)
}

// TestCheckMigrations declares checks, plans them twice (the second time
// finds nothing), then edits, adds and removes one, beside a check a
// migration wrote by hand, which planning never touches; then runs the
// migrations on SQLite.
func TestCheckMigrations(t *testing.T) {
	posts := CheckOf(Q{"posts__gt": 0})
	status := CheckOf(Q{"status__in": []string{"draft", "open"}})
	s := migration.NewState(nil, nil)
	var all []migration.Migration
	add := func(mg migration.Migration) {
		t.Helper()
		if len(all) > 0 {
			mg.Dependencies = []string{all[len(all)-1].Name}
		}
		if err := mg.Apply(s); err != nil {
			t.Fatal(err)
		}
		all = append(all, mg)
	}
	step := func(name string, want []string, checks ...RowCheck) {
		t.Helper()
		ops, _ := migration.Diff(s, postsState(t, checks...), never)
		if got := described(ops); !slices.Equal(got, want) {
			t.Fatalf("%s: %q, want %q", name, got, want)
		}
		mg := migration.Migration{Name: name, Operations: ops}
		a, err := migration.Source("migrations", mg, nil)
		b, _ := migration.Source("migrations", mg, nil)
		if err != nil || string(a) != string(b) {
			t.Fatalf("%s: %v\n%s", name, err, a)
		}
		add(mg)
		if again, _ := migration.Diff(s, postsState(t, checks...), never); len(again) != 0 {
			t.Fatalf("%s planned again: %q", name, described(again))
		}
	}
	step("0001_initial", []string{"create model ck_posts"}, posts)
	add(migration.Migration{Name: "0002_by_hand", Operations: []migration.Operation{
		migration.AddConstraint{Table: "ck_posts", Constraint: migration.Check("ck_posts_title_set", `"title" <> ''`)},
	}})
	step("0003_edit", []string{"remove constraint ck_posts_posts_check from ck_posts", "add constraint ck_posts_posts_check on ck_posts"},
		CheckOf(Q{"posts__gte": 1}))
	step("0004_status", []string{"add constraint ck_posts_status_check on ck_posts"}, CheckOf(Q{"posts__gte": 1}), status)
	step("0005_no_posts", []string{"remove constraint ck_posts_posts_check from ck_posts"}, status)

	src, _ := migration.Source("migrations", all[3], nil)
	want := "m.AddConstraint{Table: \"ck_posts\", Constraint: m.DeclaredCheck(\"ck_posts_status_check\", m.Dialects{Postgres: `\"status\" IN ('draft', 'open')`, MySQL: \"`status` IN ('draft', 'open')\", SQLite: `\"status\" IN ('draft', 'open')`})}"
	if !strings.Contains(string(src), want) {
		t.Fatalf("written:\n%s", src)
	}

	d := memDB(t)
	ctx := context.Background()
	insert := func(title string, posts int, status string) error {
		_, err := d.SQL().Exec(`INSERT INTO "ck_posts" ("title", "posts", "status") VALUES (?, ?, ?)`, title, posts, status)
		return err
	}
	if err := ApplyMigrations(ctx, d, "0004", all...); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []error{insert("a", 0, "draft"), insert("a", 1, "gone"), insert("", 1, "draft")} {
		if bad == nil || !strings.Contains(bad.Error(), "CHECK constraint failed") {
			t.Fatalf("a row breaking a check at 0004: %v", bad)
		}
	}
	if err := insert("a", 1, "open"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, d, "", all...); err != nil {
		t.Fatal(err)
	}
	if err := insert("b", 0, "draft"); err != nil {
		t.Fatalf("the posts check went: %v", err)
	}
	if err := ApplyMigrations(ctx, d, "0004", all...); err == nil || !strings.Contains(err.Error(), "ck_posts_posts_check") {
		t.Fatalf("the posts check made again over a row breaking it: %v", err)
	}
	if _, err := d.SQL().Exec(`DELETE FROM "ck_posts" WHERE "posts" = 0`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, d, "zero", all...); err != nil {
		t.Fatal(err)
	}
}

// NotValid adds a check on Postgres for the rows written after it, and
// ValidateConstraint checks the others; elsewhere the check is added as
// any is, and there is nothing to validate.
func TestNotValidCheck(t *testing.T) {
	k := migration.DeclaredCheck("ck_posts_posts_check", migration.Dialects{Postgres: `"posts" > 0`, MySQL: "`posts` > 0", SQLite: `"posts" > 0`})
	mgs := []migration.Migration{
		{Name: "0001_initial", Operations: []migration.Operation{migration.CreateModel{Table: "ck_posts", Fields: []migration.NamedField{
			migration.F("id", migration.BigAuto()), migration.F("posts", migration.Int()),
		}}}},
		{Name: "0002_posts", Dependencies: []string{"0001_initial"}, Operations: []migration.Operation{
			migration.AddConstraint{Table: "ck_posts", Constraint: k, NotValid: true},
			migration.ValidateConstraint{Table: "ck_posts", Name: "ck_posts_posts_check"},
		}},
	}
	before, err := migration.Replay(mgs[:1], "")
	if err != nil {
		t.Fatal(err)
	}
	for dialect, want := range map[string][]string{
		"postgres": {`ALTER TABLE "ck_posts" ADD CONSTRAINT "ck_posts_posts_check" CHECK ("posts" > 0) NOT VALID`, `ALTER TABLE "ck_posts" VALIDATE CONSTRAINT "ck_posts_posts_check"`},
		"mysql":    {"ALTER TABLE `ck_posts` ADD CONSTRAINT `ck_posts_posts_check` CHECK (`posts` > 0)"},
	} {
		steps, err := mgs[1].Forwards(dialect, before.Clone())
		var got []string
		for _, st := range steps {
			got = append(got, st.SQL)
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q, %v", dialect, got, err)
		}
	}
	if steps, _ := mgs[1].Backwards("mysql", before.Clone()); len(steps) != 1 || steps[0].SQL != "ALTER TABLE `ck_posts` DROP CHECK `ck_posts_posts_check`" {
		t.Errorf("unapplied on MySQL: %+v", steps)
	}
	src, _ := migration.Source("migrations", mgs[1], nil)
	if !strings.Contains(string(src), "NotValid: true}") || !strings.Contains(string(src), `m.ValidateConstraint{Table: "ck_posts", Name: "ck_posts_posts_check"}`) {
		t.Errorf("written:\n%s", src)
	}
	if err := ApplyMigrations(context.Background(), memDB(t), "", mgs...); err != nil {
		t.Fatalf("on SQLite: %v", err)
	}
}

type ckLegacy struct {
	Model[ckLegacy]
	ID     int64
	Posts  int
	Status string
}

func (ckLegacy) Meta() Meta { return Meta{DB: "cklegacy", Table: "legacy_posts", Unmanaged: true} }

func (ckLegacy) Checks() []RowCheck {
	return []RowCheck{CheckOf(Q{"posts__gt": 0}).Field("posts").Message("needs a post").On("postgres", "mysql")}
}

func (ckLegacy) Indexes() []Index { return []Index{IndexOf("status")} }

// An unmanaged model's checks are made and removed on the database's own
// table, which no operation ever touches, and the ORM checks its rows
// before writing them.
func TestUnmanagedModelChecks(t *testing.T) {
	isolate(t)
	Register[ckLegacy]()
	to, err := declaredState([]string{"cklegacy"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := migration.Diff(migration.NewState(nil, nil), to, never)
	want := []string{"add constraint legacy_posts_posts_check on legacy_posts", "add index legacy_posts_status_idx on legacy_posts"}
	if got := described(ops); !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	made := migration.Migration{Name: "0001_initial", DB: "cklegacy", Operations: ops}
	steps, err := made.Forwards("postgres", migration.NewState(nil, nil))
	if err != nil || len(steps) != 2 || steps[0].SQL != `ALTER TABLE "legacy_posts" ADD CONSTRAINT "legacy_posts_posts_check" CHECK ("posts" > 0)` {
		t.Fatalf("on Postgres: %+v, %v", steps, err)
	}
	if steps, err := made.Forwards("sqlite", migration.NewState(nil, nil)); err != nil || len(steps) != 1 {
		t.Fatalf("on SQLite, the index alone: %+v, %v", steps, err)
	}
	s := migration.NewState(nil, nil)
	if err := made.Apply(s); err != nil {
		t.Fatal(err)
	}
	if again, _ := migration.Diff(s, to, never); len(again) != 0 {
		t.Fatalf("again: %q", described(again))
	}
	ops, _ = migration.Diff(s, migration.NewState(nil, nil), never)
	if got := described(ops); !slices.Equal(got, []string{"remove constraint legacy_posts_posts_check from legacy_posts", "remove index legacy_posts_status_idx from legacy_posts"}) {
		t.Fatalf("removed: %q", got)
	}
	removed := migration.Migration{Name: "0002_removed", DB: "cklegacy", Operations: ops}
	if err := removed.Apply(s); err != nil || len(s.Tables()) != 0 {
		t.Fatalf("the state after: %v, %v", s.Tables(), err)
	}

	d := memDB(t)
	if _, err := d.SQL().Exec(`CREATE TABLE "legacy_posts" ("id" INTEGER PRIMARY KEY, "posts" INTEGER, "status" TEXT)`); err != nil {
		t.Fatal(err)
	}
	err = Objects[ckLegacy]().Create(WithDB(context.Background(), d), &ckLegacy{Posts: 0})
	var bad Violations
	if !errors.As(err, &bad) || len(bad) != 1 || bad[0].Field != "posts" || bad[0].Message != "needs a post" {
		t.Fatalf("a row breaking the check: %v", err)
	}
}

type ckLegacyEverywhere struct {
	ID    int64
	Posts int
}

func (ckLegacyEverywhere) Checks() []RowCheck { return []RowCheck{CheckOf(Q{"posts__gt": 0})} }

// SQLite can't add a CHECK to a table it doesn't make: an unmanaged
// model's check made there fails the model, naming On.
func TestUnmanagedCheckOnSQLite(t *testing.T) {
	m := newCore(forConfig{unmanaged: true}, reflect.TypeFor[ckLegacyEverywhere]())
	if m.err == nil || !strings.Contains(m.err.Error(), "SQLite has no CHECK added to a table the ORM doesn't manage") || !strings.Contains(m.err.Error(), `.On("postgres", "mysql")`) {
		t.Fatalf("%v", m.err)
	}
	if m := newCore(forConfig{}, reflect.TypeFor[ckLegacyEverywhere]()); m.err != nil {
		t.Fatalf("managed: %v", m.err)
	}
}
