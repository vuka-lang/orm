package orm_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
	m "github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// TestApplyManagedColumns adds, alters and drops a column migrations own
// on a table they don't, its other columns and rows left alone, and
// forgets one with no SQL. SQLite alters a column only by remaking the
// table, which isn't theirs: refused.
func TestApplyManagedColumns(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	for _, q := range []string{"CREATE TABLE mc_sessions (id BIGINT PRIMARY KEY, token VARCHAR(40) NOT NULL)", "INSERT INTO mc_sessions (id, token) VALUES (1, 'a')"} {
		if _, err := orm.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	migs := []m.Migration{
		{Name: "0001_add", Operations: []m.Operation{
			m.AddField{Table: "mc_sessions", Name: "unlocks_at", Field: m.Time().Null()},
			m.AddField{Table: "mc_sessions", Name: "note", Field: m.Text().Null()},
		}},
		{Name: "0002_alter", Dependencies: []string{"0001_add"}, Operations: []m.Operation{
			m.AlterField{Table: "mc_sessions", Name: "note", Field: m.Varchar(80).Null()},
		}},
		{Name: "0003_drop", Dependencies: []string{"0002_alter"}, Operations: []m.Operation{
			m.ForgetField{Table: "mc_sessions", Name: "unlocks_at"},
			m.RemoveField{Table: "mc_sessions", Name: "note"},
		}},
	}
	sqlite := ormtest.Driver() == "sqlite"
	asked := 0
	columns := func() []string {
		t.Helper()
		asked++ // a new statement each time: Postgres won't reuse a plan whose columns changed
		rows, err := d.SQL().Query("SELECT * FROM mc_sessions WHERE 1 = 0 AND " + strconv.Itoa(asked) + " > 0")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for i := range cols {
			cols[i] = strings.ToLower(cols[i])
		}
		return cols
	}
	if err := orm.ApplyMigrations(ctx, d, "0001", migs...); err != nil {
		t.Fatal(err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token", "unlocks_at", "note"}) {
		t.Fatalf("after 0001: %q", got)
	}
	err := orm.ApplyMigrations(ctx, d, "", migs...)
	if sqlite {
		if err == nil || !strings.Contains(err.Error(), "only by remaking the table") {
			t.Fatalf("an AlterField on SQLite: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token", "unlocks_at"}) {
		t.Fatalf("after 0003: %q", got)
	}
	if n, err := orm.Raw[int64](ctx, "SELECT count(*) FROM mc_sessions WHERE token = 'a'"); err != nil || !slices.Equal(n, []int64{1}) {
		t.Fatalf("rows: %v, %v", n, err)
	}
	if err := orm.ApplyMigrations(ctx, d, "zero", migs...); err != nil {
		t.Fatal(err)
	}
	if got := columns(); !slices.Equal(got, []string{"id", "token"}) {
		t.Fatalf("at zero: %q", got)
	}
}
