package orm

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2/migration"
)

// The sessions table is another system's: each version of its model
// owns more, or other, of its columns.
type sessV0 struct {
	Model[sessV0]
	ID    int64
	Token string
}

func (sessV0) Meta() Meta { return Meta{DB: "sess0", Table: "sessions", Unmanaged: true} }

type sessV1 struct {
	Model[sessV1]
	ID        int64
	Token     string
	UnlocksAt *time.Time
	Note      *string
}

func (sessV1) Meta() Meta {
	return Meta{DB: "sess1", Table: "sessions", Unmanaged: true, Managed: []string{"UnlocksAt", "Note"}}
}

type sessV2 struct {
	Model[sessV2]
	ID        int64
	Token     string
	UnlocksAt *time.Time
	Note      *string `orm:"size:80"`
}

func (sessV2) Meta() Meta {
	return Meta{DB: "sess2", Table: "sessions", Unmanaged: true, Managed: []string{"UnlocksAt", "Note"}}
}

// sessV3 keeps UnlocksAt, no longer owned, and has no Note.
type sessV3 struct {
	Model[sessV3]
	ID        int64
	Token     string
	UnlocksAt *time.Time
}

func (sessV3) Meta() Meta { return Meta{DB: "sess3", Table: "sessions", Unmanaged: true} }

// sessTypo isn't a Model: registered, its error would fail Check for
// every test.
type sessTypo struct {
	ID        int64
	UnlocksAt *time.Time
}

func (sessTypo) Meta() Meta {
	return Meta{DB: "sesstypo", Table: "sessions", Unmanaged: true, Managed: []string{"UnlockAt"}}
}

type sessOwned struct {
	Model[sessOwned]
	ID   int64
	Note string
}

func (sessOwned) Meta() Meta { return Meta{DB: "sessowned", Table: "owned", Managed: []string{"Note"}} }

// declaredOf is the state the model M declares on its database.
func declaredOf[M any](t *testing.T) (*migration.State, error) {
	t.Helper()
	var st *migration.State
	var err error
	t.Run("declare", func(t *testing.T) {
		isolate(t)
		Register[M]()
		st, err = declaredState([]string{For[M]().Info().Meta.DB}, "sqlite")
	})
	return st, err
}

func mustDeclare[M any](t *testing.T) *migration.State {
	t.Helper()
	st, err := declaredOf[M](t)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestManagedColumns: an unmanaged model's Meta.Managed columns are
// added, altered and dropped by its migrations, the table and its other
// columns never touched; a column dropped from Managed with its field
// kept is forgotten, left in the table.
func TestManagedColumns(t *testing.T) {
	s := migration.NewState(nil, nil)
	var migs []migration.Migration
	step := func(to *migration.State, want ...string) migration.Migration {
		t.Helper()
		ops, _ := migration.Diff(s, to, never)
		if got := described(ops); !slices.Equal(got, want) {
			t.Fatalf("%q, want %q", got, want)
		}
		mg := migration.Migration{Name: "000" + string(rune('1'+len(migs))) + "_step", DB: "sess", Operations: ops}
		if len(migs) > 0 {
			mg.Dependencies = []string{migs[len(migs)-1].Name}
		}
		if err := mg.Apply(s); err != nil {
			t.Fatal(err)
		}
		if again, _ := migration.Diff(s, to, never); len(again) != 0 {
			t.Fatalf("again: %q", described(again))
		}
		migs = append(migs, mg)
		return mg
	}
	v0, err := declaredOf[sessV0](t)
	if ops, _ := migration.Diff(s, v0, never); err != nil || len(ops) != 0 {
		t.Fatalf("an unmanaged model owning nothing: %q, %v", described(ops), err)
	}
	added := step(mustDeclare[sessV1](t), "add field sessions.unlocks_at", "add field sessions.note")
	altered := step(mustDeclare[sessV2](t), "alter field sessions.note")
	forgot := step(mustDeclare[sessV3](t), "forget field sessions.unlocks_at (the column stays)", "remove field sessions.note")

	sql := func(mg migration.Migration, dialect string, before int) string {
		t.Helper()
		st := migration.NewState(nil, nil)
		for _, m := range migs[:before] {
			m.Apply(st)
		}
		steps, err := mg.Forwards(dialect, st)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, s := range steps {
			out = append(out, s.SQL)
		}
		return strings.Join(out, ";\n")
	}
	for _, c := range []struct {
		mg            migration.Migration
		before        int
		dialect, want string
	}{
		{added, 0, "postgres", `ALTER TABLE "sessions" ADD COLUMN "unlocks_at" TIMESTAMP`},
		{altered, 1, "postgres", `ALTER TABLE "sessions" ALTER COLUMN "note" TYPE VARCHAR(80)`},
		{altered, 1, "mysql", "ALTER TABLE `sessions` MODIFY COLUMN `note` VARCHAR(80)"},
		{altered, 1, "sqlite", "error: migration 0002_step: alter field sessions.note: SQLite changes sessions.note only by remaking the table"},
		{forgot, 2, "postgres", `ALTER TABLE "sessions" DROP COLUMN "note"`},
	} {
		if got := sql(c.mg, c.dialect, c.before); !strings.Contains(got, c.want) {
			t.Errorf("%s on %s:\n%s\nwant %s", c.mg.Name, c.dialect, got, c.want)
		}
	}
	if got := sql(forgot, "postgres", 2); strings.Contains(got, "unlocks_at") || strings.Contains(got, "CREATE") {
		t.Errorf("forgetting a column ran SQL on it:\n%s", got)
	}

	// On SQLite: the columns added to the table there, its rows kept; Note
	// dropped and UnlocksAt left when they are no longer owned.
	d := memDB(t)
	for _, q := range []string{`CREATE TABLE "sessions" ("id" INTEGER PRIMARY KEY, "token" TEXT NOT NULL)`, `INSERT INTO "sessions" ("token") VALUES ('a')`} {
		if _, err := d.SQL().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	columns := func() []string {
		t.Helper()
		rows, err := d.SQL().Query(`SELECT name FROM pragma_table_info('sessions')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			out = append(out, n)
		}
		return out
	}
	ctx := context.Background()
	if err := ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token", "unlocks_at", "note"}) {
		t.Fatalf("columns after 0001: %q", got)
	}
	noAlter := slices.Clone(migs)
	noAlter[1].Operations = nil
	if err := ApplyMigrations(ctx, d, "", noAlter...); err != nil {
		t.Fatal(err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token", "unlocks_at"}) {
		t.Fatalf("columns after 0003: %q", got)
	}
	if err := ApplyMigrations(ctx, d, "zero", noAlter...); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.SQL().QueryRow(`SELECT COUNT(*) FROM "sessions"`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the table's rows: %d, %v", n, err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token"}) {
		t.Fatalf("columns at zero, 0001 unapplied: %q", got)
	}

	// The model gone, its owned columns are forgotten, never dropped.
	gone := migration.NewState(nil, nil)
	migs[0].Apply(gone)
	ops, notes := migration.Diff(gone, migration.NewState(nil, nil), never)
	if got := described(ops); !slices.Equal(got, []string{"forget field sessions.unlocks_at (the column stays)", "forget field sessions.note (the column stays)"}) || !strings.Contains(notes[0], "write m.RemoveField to drop it") {
		t.Fatalf("the model removed: %q %q", got, notes)
	}
}

func TestManagedRefused(t *testing.T) {
	if err := For[sessTypo]().Err(); err == nil || !strings.Contains(err.Error(), `Meta.Managed names "UnlockAt", no field of it with a column — did you mean "UnlocksAt"?`) {
		t.Fatalf("a name of no field: %v", err)
	}
	if _, err := declaredOf[sessOwned](t); err == nil || !strings.Contains(err.Error(), "sessOwned: Meta.Managed names the columns migrations own of an Unmanaged model's table") {
		t.Fatalf("Managed on a managed model: %v", err)
	}
}
