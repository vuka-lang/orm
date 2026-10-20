package orm_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
	"github.com/vuka-lang/vuka"
)

// Reading is a model of Option fields: nullable columns, None as NULL.
type Reading struct {
	orm.Model[Reading]
	ID    int64
	Label vuka.Option[string]
	Value vuka.Option[float64]
	Count vuka.Option[int]
	At    vuka.Option[time.Time]
}

// TestOptionScanner reads and writes Option fields through the generated
// scanner and by reflection alike.
func TestOptionScanner(t *testing.T) {
	if !orm.Generated[Reading]() && os.Getenv("ORM_REFLECT") == "" {
		t.Fatal("no generated scanner for a model of Option fields")
	}
	readings := orm.Objects[Reading]()
	ctx := ormtest.Open(t, readings)
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	rows := []*Reading{
		{Label: vuka.Some("dawn"), Value: vuka.Some(1.5), Count: vuka.Some(0), At: vuka.Some(at)},
		{},
	}
	if err := readings.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
	read := func() []Reading {
		t.Helper()
		got, err := readings.OrderBy("id").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	gen := read()
	if l, ok := gen[0].Label.Get(); !ok || l != "dawn" || gen[0].Count.Value() != 0 || !gen[0].Count.IsSome() || !gen[0].At.Value().Equal(at) {
		t.Fatalf("first row: %+v", gen[0])
	}
	if gen[1].Label.IsSome() || gen[1].Value.IsSome() || gen[1].Count.IsSome() || gen[1].At.IsSome() {
		t.Fatalf("second row holds values: %+v", gen[1])
	}
	orm.SetReflectOnly(true)
	t.Cleanup(func() { orm.SetReflectOnly(false) })
	if refl := read(); !reflect.DeepEqual(strip(refl), strip(gen)) {
		t.Fatalf("by reflection %+v, generated %+v", refl, gen)
	}
}

func strip(rs []Reading) [][]any {
	out := make([][]any, len(rs))
	for i, r := range rs {
		out[i] = []any{r.ID, r.Label, r.Value, r.Count, r.At.Value().UTC(), r.At.IsSome()}
	}
	return out
}
