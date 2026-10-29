package orm_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
)

// TestAggregateValuesReadAlike: Postgres and MySQL hand a SUM (Postgres
// an AVG too) over as NUMERIC/DECIMAL text and a date as a time.Time;
// read into []any, a map or any, they come out as SQLite gives them.
func TestAggregateValuesReadAlike(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	q := Users.Annotate("n", orm.Count("id")).Annotate("total", orm.Sum("age")).Annotate("avg", orm.Avg("age")).
		Annotate("oldest", orm.Max("age")).OrderBy("active")
	rows, err := q.Values[[]any]("active", "n", "total", "avg", "oldest").All(ctx)
	want := [][]any{{false, int64(2), int64(58), 29.0, int64(41)}, {true, int64(2), int64(57), 28.5, int64(32)}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("[]any = %#v, %v", rows, err)
	}
	maps, err := q.Values[map[string]any]("active", "total", "avg").All(ctx)
	if err != nil || len(maps) != 2 || maps[1]["total"] != int64(57) || maps[1]["avg"] != 28.5 {
		t.Fatalf("map = %#v, %v", maps, err)
	}
	sums, err := q.GroupBy("active").Values[any]("total").All(ctx)
	if err != nil || !reflect.DeepEqual(sums, []any{int64(58), int64(57)}) {
		t.Fatalf("any = %#v, %v", sums, err)
	}
	none, err := Users.Filter(orm.Q{"age__gt": 100}).Annotate("total", orm.Sum("age")).Values[any]("total").All(ctx)
	if err != nil || !reflect.DeepEqual(none, []any{nil}) {
		t.Fatalf("SUM of no rows = %#v, %v", none, err)
	}
	day := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	orm.Clock = func() time.Time { return day.Add(15 * time.Hour) }
	t.Cleanup(func() { orm.Clock = time.Now })
	dates := Users.Annotate("d", orm.TodayIn(time.UTC).AddDays(1)).OrderBy("id")
	for _, read := range []func() (any, error){
		func() (any, error) { v, err := dates.Values[string]("d").All(ctx); return first(v), err },
		func() (any, error) { v, err := dates.Values[any]("d").All(ctx); return first(v), err },
		func() (any, error) { v, err := dates.Values[[]any]("d").All(ctx); return first(first(v)), err },
	} {
		if v, err := read(); err != nil || v != "2026-03-05" {
			t.Fatalf("date = %#v, %v; want its text", v, err)
		}
	}
	at, err := dates.Values[time.Time]("d").All(ctx)
	if err != nil || !first(at).Equal(day.AddDate(0, 0, 1)) {
		t.Fatalf("date as time = %v, %v", at, err)
	}
}

func first[E any](s []E) E {
	var zero E
	if len(s) == 0 {
		return zero
	}
	return s[0]
}
