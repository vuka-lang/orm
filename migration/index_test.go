package migration

import (
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

var emailIdx = Index{Name: "users_email_idx", SQL: Dialects{
	Postgres: `CREATE UNIQUE INDEX "users_email_idx" ON "users" ("email")`,
	MySQL:    "CREATE UNIQUE INDEX `users_email_idx` ON `users` (`email`)",
	SQLite:   `CREATE UNIQUE INDEX "users_email_idx" ON "users" ("email")`,
}}

// A concurrent AddIndex runs outside the migration's transaction on
// Postgres, both ways; elsewhere it is an ordinary step.
func TestAddIndexConcurrently(t *testing.T) {
	op := AddIndex{Table: "users", Index: emailIdx, Concurrently: true}
	s := NewState([]schema.Table{{Name: "users", Columns: []schema.Column{{Name: "email", Kind: schema.String}}}}, nil)
	mg := Migration{Name: "0002_email", Operations: []Operation{op}}
	steps, err := mg.Forwards("postgres", s.Clone())
	if err != nil || len(steps) != 1 || !steps[0].Outside || steps[0].SQL != `CREATE UNIQUE INDEX CONCURRENTLY "users_email_idx" ON "users" ("email")` {
		t.Fatalf("forwards %+v, %v", steps, err)
	}
	steps, err = mg.Backwards("postgres", s.Clone())
	if err != nil || len(steps) != 1 || !steps[0].Outside || steps[0].SQL != `DROP INDEX CONCURRENTLY "users_email_idx"` {
		t.Fatalf("backwards %+v, %v", steps, err)
	}
	for _, d := range []string{"mysql", "sqlite"} {
		steps, err := mg.Forwards(d, s.Clone())
		if err != nil || len(steps) != 1 || steps[0].Outside || steps[0].SQL != emailIdx.SQL.For(d) {
			t.Fatalf("%s: %+v, %v", d, steps, err)
		}
	}
	src, err := Source("migrations", mg, nil)
	if err != nil || !strings.Contains(string(src), "Concurrently: true}") {
		t.Fatalf("%v\n%s", err, src)
	}
}

// legacy is a state whose users table is unmanaged: its index alone.
func legacy(ixs ...Index) *State {
	if len(ixs) == 0 {
		return NewState(nil, nil)
	}
	return NewState([]schema.Table{{Name: "users", Unmanaged: true, Indexes: ixs}}, nil)
}

// An unmanaged table's indexes are migrated; the table never is.
func TestUnmanagedIndexes(t *testing.T) {
	ops, _ := Diff(NewState(nil, nil), legacy(emailIdx), never)
	if len(ops) != 1 || ops[0] != (AddIndex{Table: "users", Index: emailIdx}) {
		t.Fatalf("made %v", ops)
	}
	s := NewState(nil, nil)
	if err := (Migration{Name: "0001", Operations: ops}).Apply(s); err != nil {
		t.Fatal(err)
	}
	if again, _ := Diff(s, legacy(emailIdx), never); len(again) != 0 {
		t.Fatalf("again %v", again)
	}
	lower := Index{Name: "users_email_lower", SQL: Dialects{SQLite: `CREATE INDEX "users_email_lower" ON "users" ((lower("email")))`}}
	ops, _ = Diff(s, legacy(lower), never)
	if want := []Operation{RemoveIndex{"users", "users_email_idx"}, AddIndex{Table: "users", Index: lower}}; !slices.Equal(ops, want) {
		t.Fatalf("changed %v", ops)
	}
	ops, _ = Diff(s, legacy(), never)
	if want := []Operation{RemoveIndex{"users", "users_email_idx"}}; !slices.Equal(ops, want) {
		t.Fatalf("removed %v", ops)
	}
	if err := (Migration{Name: "0002", Operations: ops}).Apply(s); err != nil || len(s.Tables()) != 0 {
		t.Fatalf("the last index leaves the table out of the state: %+v, %v", s.Tables(), err)
	}

	// A model that becomes managed has its table created; one that stops
	// being managed keeps its table and columns.
	managed := NewState([]schema.Table{{Name: "users", Columns: []schema.Column{{Name: "id", Kind: schema.Int, PK: true, Auto: true}}, Indexes: []Index{emailIdx}}}, nil)
	ops, _ = Diff(legacy(emailIdx), managed, never)
	if len(ops) != 2 || ops[0] != (RemoveIndex{"users", "users_email_idx"}) {
		t.Fatalf("to managed %v", ops)
	}
	if c, ok := ops[1].(CreateModel); !ok || c.Table != "users" || len(c.Indexes) != 1 {
		t.Fatalf("to managed %v", ops)
	}
	ops, _ = Diff(managed, legacy(emailIdx), never)
	if len(ops) != 0 {
		t.Fatalf("to unmanaged %v", ops)
	}
	ops, _ = Diff(managed, legacy(lower), never)
	if want := []Operation{RemoveIndex{"users", "users_email_idx"}, AddIndex{Table: "users", Index: lower}}; !slices.Equal(ops, want) {
		t.Fatalf("to unmanaged, changed %v", ops)
	}
}

// A table whose model stops being managed is never dropped, even with no
// index declared on it.
func TestUnmanagedKeepsTable(t *testing.T) {
	managed := NewState([]schema.Table{{Name: "users", Columns: []schema.Column{{Name: "id", Kind: schema.Int, PK: true, Auto: true}}}}, nil)
	bare := NewState([]schema.Table{{Name: "users", Unmanaged: true}}, nil)
	if ops, _ := Diff(managed, bare, never); len(ops) != 0 {
		t.Fatalf("%v", ops)
	}
	if ops, _ := Diff(NewState(nil, nil), bare, never); len(ops) != 0 {
		t.Fatalf("%v", ops)
	}
}
