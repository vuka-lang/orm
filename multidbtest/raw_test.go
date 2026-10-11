package multidbtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/vuka"
)

type Analytics struct{ orm.Conn }

var newAnalytics = orm.ConnFor[Analytics]("analytics")

type eventRow struct {
	ID    int64
	Kind  string `db:"kind"`
	Note  *string
	Score vuka.Option[int]
}

// TestRaw reads raw SQL into structs on the database an option names:
// NULLs into pointers and Options, unmapped columns refused unless
// Loose, ? written as each dialect's marks.
func TestRaw(t *testing.T) {
	s, ctx := open(t, map[string][]orm.AnyManager{"analytics": {Events}, "main": {Articles}})
	for _, k := range []string{"click", "view"} {
		if err := Events.Create(ctx, &Event{Kind: k}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var seen []orm.QueryInfo
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) { mu.Lock(); seen = append(seen, q); mu.Unlock() })

	rows, err := orm.Raw[eventRow](ctx, "SELECT id, kind, NULL AS note, CASE WHEN kind = ? THEN 7 END AS score FROM events ORDER BY id", "view", orm.On("analytics"))
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].Kind != "click" || rows[0].Note != nil || rows[0].Score.IsSome() || rows[1].Score.Unwrap() != 7 {
		t.Fatalf("%+v", rows)
	}
	last := seen[len(seen)-1]
	if last.DB != "analytics" || (driver() == "postgres") != strings.Contains(last.SQL, "$1") || len(last.Args) != 1 {
		t.Fatalf("observed %+v", last)
	}

	if _, err := orm.Raw[eventRow](ctx, "SELECT id, kind, 1 AS extra FROM events", orm.On("analytics")); err == nil || !strings.Contains(err.Error(), `no field for "extra"`) {
		t.Fatalf("unmapped column: %v", err)
	}
	if rows, err := orm.Raw[eventRow](ctx, "SELECT id, kind, 1 AS extra FROM events", orm.On("analytics"), orm.Loose()); err != nil || len(rows) != 2 {
		t.Fatalf("Loose: %v %v", rows, err)
	}

	a := newAnalytics(s)
	n, err := orm.RawRow[int64](a.With(ctx), "SELECT COUNT(*) FROM events WHERE kind = ?", "click")
	if err != nil || n != 1 {
		t.Fatalf("RawRow through a handle: %d %v", n, err)
	}
	if n, err := orm.RawRow[int64](a, "SELECT COUNT(*) FROM events"); err != nil || n != 2 {
		t.Fatalf("a handle as the context: %d %v", n, err)
	}
	if n, err := orm.RawRow[int64](ctx, "SELECT COUNT(*) FROM events", a); err != nil || n != 2 {
		t.Fatalf("a handle as an option: %d %v", n, err)
	}
	if _, err := orm.RawRow[int64](ctx, "SELECT id FROM events WHERE kind = ?", "none", orm.On("analytics")); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("no row: %v", err)
	}
	if n, err := orm.Exec(ctx, "UPDATE events SET kind = ? WHERE kind = ?", "tap", "click", orm.On("analytics")); err != nil || n != 1 {
		t.Fatalf("Exec On: %d %v", n, err)
	}
	if n, err := a.Exec(ctx, "DELETE FROM events WHERE kind = ?", "tap"); err != nil || n != 1 {
		t.Fatalf("handle Exec: %d %v", n, err)
	}
	if _, err := orm.Raw[int64](ctx, "SELECT COUNT(*) FROM events"); err == nil {
		t.Fatal("events read on main, which has no such table")
	}
	// The model's own raw query reads its database.
	if evs, err := Events.Raw("SELECT * FROM events").All(ctx); err != nil || len(evs) != 1 {
		t.Fatalf("Manager.Raw: %v %v", evs, err)
	}
	st, err := a.Stats()
	if err != nil || st.Name != "analytics" || !st.Supervised {
		t.Fatalf("handle stats %+v %v", st, err)
	}
}

// TestReadOnly refuses writes to a read-only database.
func TestReadOnly(t *testing.T) {
	_, ctx := open(t, nil, orm.Declare("analytics", dbURL(t, "analytics"), orm.ReadOnly()))
	if _, err := orm.Exec(ctx, "CREATE TABLE x (id INT)", orm.On("analytics")); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("%v", err)
	}
	if _, err := orm.Raw[int](ctx, "SELECT 1", orm.On("analytics")); err != nil {
		t.Fatal(err)
	}
}

// TestReadOnlyRefusesRawWrites: a ReadOnly database's connections are
// read-only on the server, so raw SQL, routed as a read, can't write.
func TestReadOnlyRefusesRawWrites(t *testing.T) {
	url := dbURL(t, "ro")
	_, rw := open(t, map[string][]orm.AnyManager{"main": {Articles}}, orm.Declare("main", url, orm.AsDefault()))
	if err := Articles.Create(rw, &Article{Title: "kept"}); err != nil {
		t.Fatal(err)
	}
	_, ctx := open(t, nil, orm.Declare("main", url, orm.AsDefault(), orm.ReadOnly()))
	if err := Articles.Create(ctx, &Article{Title: "no"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Create: %v", err)
	}
	type row struct{ ID int64 }
	for _, q := range []string{
		"UPDATE articles SET title = 'pwned'",
		"DELETE FROM articles",
		"INSERT INTO articles (title) VALUES ('pwned')",
	} {
		if _, err := orm.Raw[row](ctx, q, orm.Loose()); err == nil {
			t.Errorf("Raw %q ran on a read-only database", q)
		}
		if _, err := orm.RawRow[int64](ctx, q); err == nil {
			t.Errorf("RawRow %q ran on a read-only database", q)
		}
	}
	if n, err := orm.RawRow[int64](ctx, "SELECT COUNT(*) FROM articles"); err != nil || n != 1 {
		t.Fatalf("reads: %v %v", n, err)
	}
	got, err := Articles.All(rw)
	if err != nil || len(got) != 1 || got[0].Title != "kept" {
		t.Fatalf("the data changed: %+v %v", got, err)
	}
}
