package orm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
	m "github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// TestPreflight: a unique index on rows holding duplicates fails before
// the migration's first statement, naming the duplicated values; once
// they are cleaned up it applies. A check is checked alike.
func TestPreflight(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	users := m.Migration{Name: "0001_users", Operations: []m.Operation{
		m.CreateModel{Table: "pf_users", Fields: []m.NamedField{m.F("id", m.BigAuto()), m.F("email", m.Varchar(100).Null()), m.F("age", m.Int())}},
		m.RunSQL{SQL: "INSERT INTO pf_users (email, age) VALUES ('A@x.com', 1), ('a@x.com', 2), ('a@X.COM', 3), ('b@x.com', 4), ('B@x.com', -5), ('c@x.com', 6), (NULL, 7), (NULL, 8);", ReverseSQL: m.Noop},
	}}
	lower := m.Migration{Name: "0002_email_lower", Dependencies: []string{"0001_users"}, Operations: []m.Operation{
		m.AddIndex{Table: "pf_users", Index: m.Index{Name: "pf_users_email_lower", SQL: m.Dialects{
			Postgres: `CREATE UNIQUE INDEX "pf_users_email_lower" ON "pf_users" ((lower("email")))`,
			MySQL:    "CREATE UNIQUE INDEX `pf_users_email_lower` ON `pf_users` ((lower(`email`)))",
			SQLite:   `CREATE UNIQUE INDEX "pf_users_email_lower" ON "pf_users" ((lower("email")))`,
		}}},
	}}
	err := orm.ApplyMigrations(ctx, d, "", users, lower)
	pe, ok := errors.AsType[*orm.PreflightError](err)
	if !ok || pe.Values != 2 || pe.Rows != 5 || len(pe.Samples) != 2 || pe.Samples[0].Keys[0] != `"a@x.com"` || pe.Samples[0].Rows != 3 {
		t.Fatalf("got %v (%+v)", err, pe)
	}
	for _, want := range []string{
		"orm: migration 0002_email_lower can't add index pf_users_email_lower on pf_users: 2 values of its keys are duplicated in 5 rows of pf_users:\n",
		"    \"a@x.com\"  (3 rows)\n    \"b@x.com\"  (2 rows)\n",
		"Clean up the duplicates, or make it non-unique.",
		"makemigrations --update",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the message lacks %q:\n%s", want, err)
		}
	}
	if names, _ := orm.Raw[string](ctx, "SELECT name FROM orm_migrations ORDER BY name"); len(names) != 1 {
		t.Fatalf("applied %v", names)
	}
	ormtest.Exec(t, ctx, "DELETE FROM pf_users WHERE email IN ('a@x.com', 'a@X.COM', 'B@x.com')")
	if err := orm.ApplyMigrations(ctx, d, "", users, lower); err != nil {
		t.Fatalf("without duplicates: %v", err)
	}

	adult := m.Migration{Name: "0003_age", Dependencies: []string{"0002_email_lower"}, Operations: []m.Operation{
		m.AddConstraint{Table: "pf_users", Constraint: m.Check("pf_users_age", "age >= 0")},
	}}
	ormtest.Exec(t, ctx, "INSERT INTO pf_users (email, age) VALUES ('d@x.com', -1)")
	err = orm.ApplyMigrations(ctx, d, "", users, lower, adult)
	if pe, ok := errors.AsType[*orm.PreflightError](err); !ok || pe.Rows != 1 || !strings.Contains(err.Error(), "can't add constraint pf_users_age on pf_users: 1 row of pf_users breaks it") {
		t.Fatalf("the check: %v", err)
	}
	if ormtest.Driver() == "postgres" && !strings.Contains(err.Error(), "NotValid") {
		t.Fatalf("no NotValid hint on Postgres: %v", err)
	}
}
