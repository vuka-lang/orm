package orm_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
)

// A map's or list's values are the fields' Go types, NULL as nil, whatever
// the driver hands over; each row is the caller's own.
func TestValuesRowTypes(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	list, err := Users.Annotate("n", orm.Count("id")).GroupBy("id").OrderBy("id").
		Values[[]any]("id", "name", "age", "bio", "active", "created_at", "n").All(ctx)
	if err != nil || len(list) != 4 {
		t.Fatal(list, err)
	}
	want := []string{"int64", "string", "int", "string", "bool", "time.Time", "int64"}
	for i, v := range list[0] {
		if got := fmt.Sprintf("%T", v); got != want[i] {
			t.Errorf("item %d is %s (%v), want %s", i, got, v, want[i])
		}
	}
	if list[0][3] != "writes Go" || list[1][3] != nil || list[1][4] != false || list[0][2] != 25 {
		t.Fatalf("values %v %v", list[0], list[1])
	}
	if _, ok := list[0][5].(time.Time); !ok {
		t.Fatalf("created_at %T", list[0][5])
	}

	// Rows share no items: a write or an append to one leaves the next.
	list[0][1] = "changed"
	grown := append(list[0], "more")
	grown[2] = -1
	if list[1][1] != "Neema" || len(list[1]) != 7 || list[0][2] != 25 {
		t.Fatalf("rows changed one another: %v %v", list[0], list[1])
	}

	maps, err := Users.OrderBy("id").Values[map[string]any]("id", "name", "bio").All(ctx)
	if err != nil || len(maps) != 4 {
		t.Fatal(maps, err)
	}
	maps[0]["name"] = "changed"
	if maps[1]["name"] != "Neema" || maps[1]["bio"] != nil || maps[0]["bio"] != "writes Go" || maps[3]["id"] != int64(4) {
		t.Fatalf("maps %v", maps)
	}

	// Iter's rows are as All's: kept past the next one.
	var kept [][]any
	for row, err := range Users.OrderBy("id").Values[[]any]("name").Iter(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, row)
	}
	if len(kept) != 4 || kept[0][0] != "Ali" || kept[3][0] != "Amina" {
		t.Fatalf("Iter kept %v", kept)
	}
}
