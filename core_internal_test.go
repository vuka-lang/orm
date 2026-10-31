package orm

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

type coreRow struct {
	Model[coreRow]
	ID   int64
	Name string
}

// A query is shared by every QuerySet derived from it, so deriving one never
// shows in another, however many there are.
func TestQuerySetsShareNothingTheyChange(t *testing.T) {
	m := For[coreRow]()
	base := m.Filter(Q{"name": "a"})
	left, right := base.Filter(Q{"id": 1}), base.Filter(Q{"id": 2})
	if len(base.q.where) != 1 || len(left.q.where) != 2 || len(right.q.where) != 2 {
		t.Fatalf("where: base %d, left %d, right %d", len(base.q.where), len(left.q.where), len(right.q.where))
	}
	if left.q.where[1].(Q)["id"] != 1 || right.q.where[1].(Q)["id"] != 2 {
		t.Errorf("derived queries wrote over each other: %v, %v", left.q.where, right.q.where)
	}
	if m.q != m.c.all || len(m.q.where) != 0 {
		t.Errorf("the manager's own query changed: %+v", m.q)
	}
}

// Building a Values statement fills in a copy of its query, not the shared one.
func TestValuesPlanLeavesTheQueryAlone(t *testing.T) {
	m := For[coreRow]()
	vq := m.Annotate("n", Count("id")).Values[map[string]any]("name", "n")
	before := *vq.q.q
	first, _, _, err := vq.q.q.valuesPlan(vq.cols, reflect.TypeFor[map[string]any](), sqlite{})
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := vq.q.q.valuesPlan(vq.cols, reflect.TypeFor[map[string]any](), sqlite{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("planned twice:\n%s\n%s", first, second)
	}
	if !slices.Equal(vq.q.q.values, before.values) || vq.q.q.grouped != before.grouped || vq.q.q.group != before.group {
		t.Errorf("planning changed the query: %+v", vq.q.q)
	}
}

// Every builder works on a QuerySet's zero value, as they did: only running
// one needs a manager.
func TestQuerySetZeroValueBuilds(t *testing.T) {
	var qs QuerySet[coreRow]
	qs = qs.Filter(Q{"id": 1}).Limit(3).Offset(1).Distinct().Unfiltered()
	if qs.q.limit != 3 || qs.q.offset != 1 || !qs.q.distinct || !qs.q.every || len(qs.q.where) != 1 {
		t.Errorf("built %+v", qs.q)
	}
}

// A row's Model is written through rowState: both must keep the same layout,
// and the manager a row is adopted by is the one it writes through.
func TestAdoptSetsTheRowsManager(t *testing.T) {
	m := Of[coreRow](DBSchema{Names: "core_adopt"})
	row := &coreRow{}
	m.c.adopt(row)
	if !row.loaded || row.m != m {
		t.Errorf("adopted: loaded %v, manager %p, want %p", row.loaded, row.m, m)
	}
	if got := row.manager(); got != m {
		t.Errorf("manager() = %p, want %p", got, m)
	}
}

// Change carries its row as a copy and its rows as T's.
func TestOnChangeTypes(t *testing.T) {
	m := Of[coreRow](DBSchema{Names: "core_change"})
	var got []Change[coreRow]
	stop := m.OnChange(func(_ context.Context, c Change[coreRow]) { got = append(got, c) })
	defer stop()
	ctx := context.Background()
	row := &coreRow{ID: 7}
	db := conn{}
	m.c.changed(ctx, db, changeEvent{kind: Updated, row: row})
	row.ID = 8
	m.c.changed(ctx, db, changeEvent{kind: BulkDeleted, count: 1, rows: []coreRow{{ID: 9}}})
	if len(got) != 2 {
		t.Fatalf("heard %d changes", len(got))
	}
	if got[0].Kind != Updated || got[0].Row.ID != 7 || got[0].Row == row {
		t.Errorf("row change: %+v", got[0])
	}
	if got[1].Kind != BulkDeleted || got[1].Count != 1 || len(got[1].Rows) != 1 || got[1].Rows[0].ID != 9 || got[1].Row != nil {
		t.Errorf("bulk change: %+v", got[1])
	}
}
