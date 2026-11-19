package orm_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
	m "github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
)

func recorded(t *testing.T, ctx context.Context) []string {
	t.Helper()
	names, err := orm.Raw[string](ctx, "SELECT name FROM "+orm.MigrationsTable+" ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func tableExists(ctx context.Context, table string) bool {
	_, err := orm.Exec(ctx, "SELECT 1 FROM "+table)
	return err == nil
}

// TestFakeMigrations records migrations without running them: a target by
// name or number once what it depends on is applied, refused before;
// back to an earlier one and zero record them unapplied.
func TestFakeMigrations(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	migs := []m.Migration{
		{Name: "0001_users", Operations: []m.Operation{m.CreateModel{Table: "fk_users", Fields: []m.NamedField{m.F("id", m.BigAuto())}}}},
		{Name: "0002_posts", Dependencies: []string{"0001_users"}, Operations: []m.Operation{m.CreateModel{Table: "fk_posts", Fields: []m.NamedField{m.F("id", m.BigAuto())}}}},
		{Name: "0003_seed", Dependencies: []string{"0002_posts"}, Operations: []m.Operation{m.RunSQL{SQL: "INSERT INTO fk_posts (id) VALUES (1)"}}},
	}
	err := orm.FakeMigrations(ctx, d, "0003", migs...)
	if err == nil || !strings.Contains(err.Error(), "can't fake 0003_seed: it depends on 0001_users, 0002_posts, not applied") {
		t.Fatalf("faking ahead of its dependencies: %v", err)
	}
	if got := recorded(t, ctx); len(got) != 0 {
		t.Fatalf("recorded after a refusal: %q", got)
	}
	if err := orm.FakeMigrations(ctx, d, "0001_users", migs...); err != nil {
		t.Fatal(err)
	}
	if err := orm.FakeMigrations(ctx, d, "0002", migs...); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, ctx); !slices.Equal(got, []string{"0001_users", "0002_posts"}) {
		t.Fatalf("recorded %q", got)
	}
	if tableExists(ctx, "fk_users") || tableExists(ctx, "fk_posts") {
		t.Fatal("a faked migration ran its SQL")
	}
	// Applying from there runs 0003 alone: its insert fails, fk_posts
	// never made.
	if err := orm.ApplyMigrations(ctx, d, "", migs...); err == nil {
		t.Fatal("0003 ran on a table no migration made")
	}
	if err := orm.FakeMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, ctx); !slices.Equal(got, []string{"0001_users"}) {
		t.Fatalf("back to 0001: %q", got)
	}
	if err := orm.FakeMigrations(ctx, d, "zero", migs...); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, ctx); len(got) != 0 {
		t.Fatalf("at zero: %q", got)
	}
}

// TestFakeInitial records a migration whose tables the database has
// already, and applies the rest; a table only partly there, or a
// migration's tables only some of which are, fails it.
func TestFakeInitial(t *testing.T) {
	users := m.Migration{Name: "0001_users", Operations: []m.Operation{
		m.CreateModel{Table: "fi_users", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("email", m.Text())}},
	}}
	posts := m.Migration{Name: "0002_posts", Dependencies: []string{"0001_users"}, Operations: []m.Operation{
		m.CreateModel{Table: "fi_posts", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("title", m.Text())}},
		m.AddField{Table: "fi_users", Name: "name", Field: m.Text().Null()},
	}}
	exec := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := orm.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	exec(ctx, "CREATE TABLE fi_users (id BIGINT PRIMARY KEY, email TEXT, legacy TEXT)")
	exec(ctx, "INSERT INTO fi_users (id, email) VALUES (1, 'a@example.com')")
	if err := orm.FakeMigrations(ctx, d, "", users, posts); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, ctx); !slices.Equal(got, []string{"0001_users", "0002_posts"}) {
		t.Fatalf("recorded %q", got)
	}
	if n, err := orm.Raw[int64](ctx, "SELECT count(*) FROM fi_users"); err != nil || !slices.Equal(n, []int64{1}) {
		t.Fatalf("fi_users made again: %v, %v", n, err)
	}
	if !tableExists(ctx, "fi_posts") {
		t.Fatal("0002_posts, whose table didn't exist, wasn't applied")
	}
	if _, err := orm.Exec(ctx, "SELECT name FROM fi_users"); err != nil {
		t.Fatalf("0002's AddField didn't run: %v", err)
	}

	ctx = ormtest.Open(t)
	d, _ = orm.DBFrom(ctx)
	exec(ctx, "CREATE TABLE fi_users (id BIGINT PRIMARY KEY)")
	err := orm.FakeMigrations(ctx, d, "", users)
	if err == nil || !strings.Contains(err.Error(), "0001_users is only partly made") || !strings.Contains(err.Error(), "table fi_users has no column email") {
		t.Fatalf("a table lacking a column: %v", err)
	}
	if got := recorded(t, ctx); len(got) != 0 {
		t.Fatalf("recorded %q", got)
	}

	ctx = ormtest.Open(t)
	d, _ = orm.DBFrom(ctx)
	exec(ctx, "CREATE TABLE fi_users (id BIGINT PRIMARY KEY, email TEXT)")
	both := m.Migration{Name: "0001_initial", Operations: append(slices.Clone(users.Operations), posts.Operations[0])}
	err = orm.FakeMigrations(ctx, d, "", both)
	if err == nil || !strings.Contains(err.Error(), "table fi_posts doesn't exist") {
		t.Fatalf("one table of two: %v", err)
	}
	if tableExists(ctx, "fi_posts") {
		t.Fatal("a partly made migration ran")
	}

	// The tables there, an AddField's column not: neither faked nor run.
	ctx = ormtest.Open(t)
	d, _ = orm.DBFrom(ctx)
	exec(ctx, "CREATE TABLE fi_users (id BIGINT PRIMARY KEY, email TEXT)")
	exec(ctx, "CREATE TABLE fi_posts (id BIGINT PRIMARY KEY, title TEXT)")
	err = orm.FakeMigrations(ctx, d, "", users, posts)
	if err == nil || !strings.Contains(err.Error(), "table fi_users has no column name") {
		t.Fatalf("an AddField's column missing: %v", err)
	}
	if got := recorded(t, ctx); !slices.Equal(got, []string{"0001_users"}) {
		t.Fatalf("recorded %q", got)
	}
}
