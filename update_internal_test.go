package orm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/migration"
)

// updateFixture writes migrations into a directory as makemigrations
// does, and is the request updating the latest of them.
func updateFixture(t *testing.T, migs ...migration.Migration) (CLIRequest, []migration.Migration) {
	t.Helper()
	dir := t.TempDir()
	for _, mg := range migs {
		src, err := migration.Source("migrations", mg, []string{"a note on the first operation"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, mg.Name+".go"), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ordered, err := migration.Order(migs)
	if err != nil {
		t.Fatal(err)
	}
	return CLIRequest{Command: "plan", Dialect: "sqlite", Package: "migrations", Update: true, Dir: dir}, ordered
}

// TestUpdateLatest: --update rewrites the latest migration as the
// operations from before it to the models — a unique index made plain is
// added once, plain — renames it when its name was made from its
// operations, keeps a name of a person's, and removes it when nothing is
// left.
func TestUpdateLatest(t *testing.T) {
	unique := IndexOf(Lower.Of(F("email"))).Unique().Name("users_email_lower")
	plain := IndexOf(Lower.Of(F("email"))).Name("users_email_lower")
	initial, _ := migration.Diff(migration.NewState(nil, nil), usersState(t), never)
	added, _ := migration.Diff(usersState(t), usersState(t, unique), never)
	first := migration.Migration{Name: "0001_initial", Operations: initial}
	latest := migration.Migration{Name: "0002_add_index_users_email_lower_on_users", Dependencies: []string{"0001_initial"}, Operations: added}

	req, ordered := updateFixture(t, first, latest)
	res, err := update(req, ordered, usersState(t, plain), never)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updates != latest.Name || res.File != latest.Name+".go" || len(res.Ops) != 1 || res.Remove {
		t.Fatalf("%+v", res)
	}
	golden := filepath.Join("testdata", "update", "0002_add_index_users_email_lower_on_users.go.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.MkdirAll(filepath.Dir(golden), 0o755)
		if err := os.WriteFile(golden, res.Source, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want, err := os.ReadFile(golden); err != nil || string(want) != string(res.Source) {
		t.Fatalf("source differs from %s (UPDATE_GOLDEN=1 rewrites it): %v\n%s", golden, err, res.Source)
	}

	// Another index too: two operations, named auto.
	res, err = update(req, ordered, usersState(t, plain, IndexOf("name")), never)
	if err != nil || res.File != "0002_auto.go" || len(res.Ops) != 2 {
		t.Fatalf("two indexes: %v %+v", err, res)
	}
	// A name of a person's stays.
	named := latest
	named.Name = "0002_email_lower"
	req, ordered = updateFixture(t, first, named)
	if res, err = update(req, ordered, usersState(t, plain), never); err != nil || res.File != "0002_email_lower.go" {
		t.Fatalf("a person's name: %v %+v", err, res)
	}
	// The models back as before it: nothing left.
	if res, err = update(req, ordered, usersState(t), never); err != nil || !res.Remove || res.Updates != "0002_email_lower" {
		t.Fatalf("nothing left: %v %+v", err, res)
	}
	// The first migration: still initial.
	req, ordered = updateFixture(t, first)
	if res, err = update(req, ordered, usersState(t, plain), never); err != nil || res.File != "0001_initial.go" || len(res.Ops) != 1 {
		t.Fatalf("the first migration: %v %+v", err, res)
	}
}

// TestUpdateRefuses: no migration, one another depends on, one holding a
// RunSQL, one whose file was edited.
func TestUpdateRefuses(t *testing.T) {
	initial, _ := migration.Diff(migration.NewState(nil, nil), usersState(t), never)
	first := migration.Migration{Name: "0001_initial", Operations: initial}
	to := usersState(t, IndexOf("name"))

	req, ordered := updateFixture(t)
	if _, err := update(req, ordered, to, never); err == nil || !strings.Contains(err.Error(), "the default database has no migration to update") {
		t.Fatalf("no migration: %v", err)
	}
	other := migration.Migration{Name: "0001_x", DB: "other", Dependencies: []string{":0001_initial"}}
	req, ordered = updateFixture(t, first, other)
	if _, err := update(req, ordered, to, never); err == nil || !strings.Contains(err.Error(), "other:0001_x depends on it") {
		t.Fatalf("depended on: %v", err)
	}
	seed := migration.Migration{Name: "0002_seed", Dependencies: []string{"0001_initial"}, Operations: []migration.Operation{migration.RunSQL{SQL: "SELECT 1;"}}}
	req, ordered = updateFixture(t, first, seed)
	if _, err := update(req, ordered, to, never); err == nil || !strings.Contains(err.Error(), "can't rewrite 0002_seed: it holds run SQL, written by hand") {
		t.Fatalf("a RunSQL: %v", err)
	}
	req, ordered = updateFixture(t, first)
	path := filepath.Join(req.Dir, "0001_initial.go")
	src, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(src), "m.Text()", "m.Text().Null()", 1)), 0o644)
	if _, err := update(req, ordered, to, never); err == nil || !strings.Contains(err.Error(), "was edited since makemigrations wrote it") {
		t.Fatalf("edited: %v", err)
	}
}
