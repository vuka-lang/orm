package orm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2/internal/progress"
	"github.com/vuka-lang/orm/v2/migration"
)

// The indexes the models declare are ones the preflight reads, on every
// database: a unique key of an expression, and a partial one.
func TestPreflightReadsDeclaredIndexes(t *testing.T) {
	ops, _ := migration.Diff(usersState(t), usersState(t,
		IndexOf(Lower.Of(F("email"))).Unique().Name("users_email_lower"),
		IndexOf("name", Key("email").Desc()).Unique().Where(Q{"active": true}).Else(IndexOf("name", "email").Unique()),
	), never)
	mg := migration.Migration{Name: "0002", Operations: ops}
	for dialect, want := range map[string][]string{
		"postgres": {`SELECT (lower("email")), COUNT(*)`, `SELECT "name", "email", COUNT(*) FROM "users" WHERE ("name") IS NOT NULL AND ("email") IS NOT NULL AND ("active" = TRUE) GROUP BY`},
		"mysql":    {"SELECT (lower(`email`)), COUNT(*)", "SELECT `name`, `email`, COUNT(*) FROM `users` WHERE (`name`) IS NOT NULL AND (`email`) IS NOT NULL GROUP BY"},
		"sqlite":   {`SELECT (lower("email")), COUNT(*)`, `SELECT "name", "email", COUNT(*) FROM "users" WHERE ("name") IS NOT NULL AND ("email") IS NOT NULL AND ("active" = TRUE) GROUP BY`},
	} {
		got := mg.Preflights(dialect)
		if len(got) != len(want) {
			t.Fatalf("%s: %d preflights, want %d: %+v", dialect, len(got), len(want), got)
		}
		for i, w := range want {
			if !strings.HasPrefix(got[i].SQL, w) {
				t.Errorf("%s:\n got %s\nwant %s…", dialect, got[i].SQL, w)
			}
		}
	}
}

// orm migrate shows the preflight running; --no-preflight leaves the
// duplicates to the database.
func TestPreflightProgress(t *testing.T) {
	ordered, err := migration.Order([]migration.Migration{
		{Name: "0001_users", Operations: []migration.Operation{
			migration.CreateModel{Table: "users", Fields: []migration.NamedField{migration.F("id", migration.BigAuto()), migration.F("email", migration.Text())}},
			migration.RunSQL{SQL: "INSERT INTO users (email) VALUES ('a'), ('a');"},
		}},
		{Name: "0002_email", Dependencies: []string{"0001_users"}, Operations: []migration.Operation{
			migration.AddConstraint{Table: "users", Constraint: migration.Unique("users_email", "email")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, skip := range []bool{false, true} {
		d := memDB(t)
		var out strings.Builder
		w := &cliWatch{p: progress.New(&out, false, false), verbose: true}
		r := &migrator{ordered: ordered, open: func(context.Context, string) (*DB, error) { return d, nil }, log: func(string, ...any) {}, watch: w, noPreflight: skip}
		err := r.forwardAll(context.Background(), nil)
		r.close()
		_, refused := errors.AsType[*PreflightError](err)
		switch {
		case !skip && (!refused || !strings.Contains(out.String(), `    -- preflight: SELECT "email", COUNT(*) FROM "users"`)):
			t.Fatalf("preflight: %v\n%s", err, out.String())
		case skip && (err == nil || refused || strings.Contains(out.String(), "preflight")):
			t.Fatalf("--no-preflight: %v\n%s", err, out.String())
		}
	}
}
