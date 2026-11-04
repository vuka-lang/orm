package orm

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/migration"
)

func never(string) bool { return false }

// usersState is ixUser's table, users, as migrations make it when it
// declares ixs.
func usersState(t *testing.T, ixs ...Index) *migration.State {
	t.Helper()
	m, err := modelOf(reflect.TypeFor[ixUser](), "", "users")
	if err != nil {
		t.Fatal(err)
	}
	m.indexes = ixs
	if m.made, err = m.declaredIndexes(); err != nil {
		t.Fatal(err)
	}
	ts, err := m.tables()
	if err != nil {
		t.Fatal(err)
	}
	return migration.NewState(ts, nil)
}

// described is what ops do, for people.
func described(ops []migration.Operation) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = migration.Describe(op)
	}
	return out
}

// TestIndexMigrations declares indexes, plans them twice (the second time
// finds nothing), then edits, removes and adds a RawIndex, and runs the
// migrations on SQLite.
func TestIndexMigrations(t *testing.T) {
	lower := IndexOf(Lower.Of(F("email"))).Unique().Name("users_email_lower")
	s := migration.NewState(nil, nil)
	var all []migration.Migration
	step := func(name string, want []string, ixs ...Index) {
		t.Helper()
		to := usersState(t, ixs...)
		ops, _ := migration.Diff(s, to, never)
		if got := described(ops); !slices.Equal(got, want) {
			t.Fatalf("%s: %q, want %q", name, got, want)
		}
		mg := migration.Migration{Name: name, Operations: ops}
		if len(all) > 0 {
			mg.Dependencies = []string{all[len(all)-1].Name}
		}
		if _, err := migration.Source("migrations", mg, nil); err != nil {
			t.Fatal(err)
		}
		if err := mg.Apply(s); err != nil {
			t.Fatal(err)
		}
		if again, _ := migration.Diff(s, usersState(t, ixs...), never); len(again) != 0 {
			t.Fatalf("%s planned again: %q", name, described(again))
		}
		all = append(all, mg)
	}
	step("0001_initial", []string{"create model users"}, IndexOf("email"), lower)
	step("0002_unique", []string{"remove index users_email_idx from users", "add index users_email_idx on users"}, IndexOf("email").Unique(), lower)
	step("0003_no_lower", []string{"remove index users_email_lower from users"}, IndexOf("email").Unique())
	raw := RawIndex("users_name_prefix", Dialects{MySQL: "CREATE INDEX `users_name_prefix` ON `users` (`name`(20))"})
	step("0004_raw", []string{"add index users_name_prefix on users"}, IndexOf("email").Unique(), raw)

	src, err := migration.Source("migrations", all[0], nil)
	if err != nil || !strings.Contains(string(src), `SQLite: `+"`"+`CREATE UNIQUE INDEX "users_email_lower" ON "users" ((lower("email")))`+"`") {
		t.Fatalf("%v\n%s", err, src)
	}
	before, err := migration.Replay(all[:3], "")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := all[3].Forwards("mysql", before.Clone())
	if err != nil || len(steps) != 1 || steps[0].SQL != "CREATE INDEX `users_name_prefix` ON `users` (`name`(20))" {
		t.Fatalf("raw on MySQL: %+v, %v", steps, err)
	}
	if steps, _ := all[3].Backwards("mysql", before.Clone()); len(steps) != 1 || steps[0].SQL != "DROP INDEX `users_name_prefix` ON `users`" {
		t.Fatalf("raw dropped on MySQL: %+v", steps)
	}
	if steps, _ := all[3].Forwards("sqlite", before.Clone()); len(steps) != 0 {
		t.Fatalf("raw on SQLite, which it has no statement for: %+v", steps)
	}

	d := memDB(t)
	ctx := context.Background()
	if err := ApplyMigrations(ctx, d, "0002", all...); err != nil {
		t.Fatal(err)
	}
	if got := indexNamesOn(t, d, "users"); !slices.Equal(got, []string{"users_email_idx", "users_email_lower"}) {
		t.Fatalf("indexes at 0002: %v", got)
	}
	if err := ApplyMigrations(ctx, d, "", all...); err != nil {
		t.Fatal(err)
	}
	if got := indexNamesOn(t, d, "users"); !slices.Equal(got, []string{"users_email_idx"}) {
		t.Fatalf("indexes at the last: %v", got)
	}
	if err := ApplyMigrations(ctx, d, "zero", all...); err != nil {
		t.Fatal(err)
	}
}

// indexNamesOn is the names of the indexes d's table has, sorted.
func indexNamesOn(t *testing.T, d *DB, table string) []string {
	t.Helper()
	rows, err := d.SQL().Query("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL ORDER BY name", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

type ixLegacy struct {
	Model[ixLegacy]
	ID    int64
	Email string
	Name  string
}

func (ixLegacy) Meta() Meta {
	return Meta{DB: "ixlegacy", Table: "legacy_users", Unmanaged: true}
}

func (ixLegacy) Indexes() []Index {
	return []Index{IndexOf(Lower.Of(F("email"))).Unique().Name("legacy_users_email_lower"), IndexOf("name")}
}

// An unmanaged model's indexes are made and removed on the database's
// own table, which no operation ever touches.
func TestUnmanagedModelIndexes(t *testing.T) {
	isolate(t)
	Register[ixLegacy]()
	to, err := declaredState([]string{"ixlegacy"}, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := migration.Diff(migration.NewState(nil, nil), to, never)
	want := []string{"add index legacy_users_email_lower on legacy_users", "add index legacy_users_name_idx on legacy_users"}
	if got := described(ops); !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	made := migration.Migration{Name: "0001_initial", DB: "ixlegacy", Operations: ops}
	s := migration.NewState(nil, nil)
	if err := made.Apply(s); err != nil {
		t.Fatal(err)
	}
	if again, _ := migration.Diff(s, to, never); len(again) != 0 {
		t.Fatalf("again: %q", described(again))
	}
	ops, _ = migration.Diff(s, migration.NewState(nil, nil), never)
	if got := described(ops); !slices.Equal(got, []string{"remove index legacy_users_email_lower from legacy_users", "remove index legacy_users_name_idx from legacy_users"}) {
		t.Fatalf("removed: %q", got)
	}
	removed := migration.Migration{Name: "0002_removed", DB: "ixlegacy", Dependencies: []string{"0001_initial"}, Operations: ops}

	d := memDB(t)
	if _, err := d.SQL().Exec(`CREATE TABLE "legacy_users" ("id" INTEGER PRIMARY KEY, "email" TEXT, "name" TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL().Exec(`INSERT INTO "legacy_users" ("email", "name") VALUES ('Ali@mail.com', 'Ali')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ApplyMigrations(ctx, d, "0001", made, removed); err != nil {
		t.Fatal(err)
	}
	if got := indexNamesOn(t, d, "legacy_users"); !slices.Equal(got, []string{"legacy_users_email_lower", "legacy_users_name_idx"}) {
		t.Fatalf("made: %v", got)
	}
	if err := ApplyMigrations(ctx, d, "", made, removed); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.SQL().QueryRow(`SELECT COUNT(*) FROM "legacy_users"`).Scan(&n); err != nil || n != 1 || len(indexNamesOn(t, d, "legacy_users")) != 0 {
		t.Fatalf("the table after its indexes went: %d rows, %v, %v", n, indexNamesOn(t, d, "legacy_users"), err)
	}
}

// batches runs an Outside step between transactions of the others; a
// database that can't roll DDL back runs them all on its connection.
func TestMigrationBatches(t *testing.T) {
	steps := []migration.Step{{SQL: "a"}, {SQL: "b"}, {SQL: "c", Outside: true}, {SQL: "d"}}
	got := batches(steps, true)
	if len(got) != 3 || len(got[0].steps) != 2 || !got[0].inTx || got[1].inTx || got[1].steps[0].SQL != "c" || !got[2].inTx {
		t.Fatalf("%+v", got)
	}
	if got := batches(steps, false); len(got) != 1 || got[0].inTx || len(got[0].steps) != 4 {
		t.Fatalf("%+v", got)
	}
}
