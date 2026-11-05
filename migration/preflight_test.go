package migration

import (
	"slices"
	"testing"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// TestUniqueIndexKeys reads the keys and WHERE of unique indexes as each
// database writes them, and refuses what it can't read.
func TestUniqueIndexKeys(t *testing.T) {
	for _, c := range []struct {
		dialect, sql string
		keys         []string
		where        string
		ok           bool
	}{
		{"postgres", `CREATE UNIQUE INDEX "users_email_lower" ON "user" ((lower("email")))`, []string{`(lower("email"))`}, "", true},
		{"mysql", "CREATE UNIQUE INDEX `users_email_lower` ON `user` ((LOWER(`email`)))", []string{"(LOWER(`email`))"}, "", true},
		{"postgres", `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "x" ON ONLY "t" USING btree ("a" DESC NULLS LAST, "b" COLLATE "C" text_pattern_ops) INCLUDE ("c") WITH (fillfactor = 70) WHERE "active" = TRUE AND "name" <> 'a,b';`,
			[]string{`"a"`, `"b" COLLATE "C"`}, `"active" = TRUE AND "name" <> 'a,b'`, true},
		{"mysql", "CREATE UNIQUE INDEX `x` ON `t` (`name`(10), `email` DESC) USING HASH", []string{"LEFT(`name`, 10)", "`email`"}, "", true},
		{"sqlite", `CREATE UNIQUE INDEX "x" ON "t" (lower(name), "a""b")`, []string{`lower(name)`, `"a""b"`}, "", true},
		{"postgres", `CREATE INDEX "x" ON "t" ("a")`, nil, "", false},
		{"postgres", `CREATE UNIQUE INDEX "x" ON "t" ("a") NULLS NOT DISTINCT`, nil, "", false},
		{"postgres", `CREATE UNIQUE INDEX "x" ON "t" ("a"`, nil, "", false},
		{"postgres", `CREATE UNIQUE INDEX "x" ON "t" ("a" + 1)`, nil, "", false},
		{"postgres", ``, nil, "", false},
	} {
		keys, where, ok := uniqueIndex(schema.For(c.dialect), c.sql)
		if ok != c.ok || !slices.Equal(keys, c.keys) || where != c.where {
			t.Errorf("%s %s:\n got %q %q %v\nwant %q %q %v", c.dialect, c.sql, keys, where, ok, c.keys, c.where, c.ok)
		}
	}
}

// TestPreflights: a unique index or constraint is checked for duplicates,
// a check for rows breaking it; not one NotValid on Postgres, nor one on
// a table the migration made or changed, nor any after its own Go or SQL.
func TestPreflights(t *testing.T) {
	lower := AddIndex{Table: "users", Index: Index{Name: "users_email_lower", SQL: Dialects{
		Postgres: `CREATE UNIQUE INDEX "users_email_lower" ON "users" ((lower("email")))`,
		MySQL:    "CREATE UNIQUE INDEX `users_email_lower` ON `users` ((LOWER(`email`)))",
		SQLite:   `CREATE UNIQUE INDEX "users_email_lower" ON "users" ((lower("email"))) WHERE "id" > 0`,
	}}}
	pair := AddConstraint{Table: "users", Constraint: Unique("users_pair", "id", "email")}
	positive := AddConstraint{Table: "users", Constraint: Check("users_id_positive", `"id" > 0`), NotValid: true}
	golden := map[string][]string{
		"postgres": {
			`SELECT (lower("email")), COUNT(*) FROM "users" WHERE ((lower("email"))) IS NOT NULL GROUP BY (lower("email")) HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`,
			`SELECT "id", "email", COUNT(*) FROM "users" WHERE ("id") IS NOT NULL AND ("email") IS NOT NULL GROUP BY "id", "email" HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`,
		},
		"mysql/8.0.36": {
			"SELECT (LOWER(`email`)), COUNT(*) FROM `users` WHERE ((LOWER(`email`))) IS NOT NULL GROUP BY (LOWER(`email`)) HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC",
			"SELECT `id`, `email`, COUNT(*) FROM `users` WHERE (`id`) IS NOT NULL AND (`email`) IS NOT NULL GROUP BY `id`, `email` HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC",
			"SELECT COUNT(*) FROM `users` WHERE NOT (\"id\" > 0)",
		},
		"sqlite": {
			`SELECT (lower("email")), COUNT(*) FROM "users" WHERE ((lower("email"))) IS NOT NULL AND ("id" > 0) GROUP BY (lower("email")) HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`,
			`SELECT "id", "email", COUNT(*) FROM "users" WHERE ("id") IS NOT NULL AND ("email") IS NOT NULL GROUP BY "id", "email" HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`,
			`SELECT COUNT(*) FROM "users" WHERE NOT ("id" > 0)`,
		},
	}
	mg := Migration{Name: "0002", Operations: []Operation{lower, pair, positive}}
	for dialect, want := range golden {
		var got []string
		for _, p := range mg.Preflights(dialect) {
			got = append(got, p.SQL)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", dialect, got, want)
		}
	}
	for name, ops := range map[string][]Operation{
		"after RunGo":          {RunGo{}, lower},
		"after RunSQL":         {RunSQL{SQL: "DELETE FROM users;"}, pair},
		"its table changed":    {AddField{Table: "users", Name: "name", Field: Text().Null()}, lower},
		"its table made":       {CreateModel{Table: "posts", Fields: []NamedField{F("id", BigAuto())}}, AddConstraint{Table: "posts", Constraint: Unique("posts_id", "id")}},
		"NotValid on Postgres": {positive},
	} {
		if got := (Migration{Name: "0002", Operations: ops}).Preflights("postgres"); len(got) != 0 {
			t.Errorf("%s: %q", name, got)
		}
	}
}
