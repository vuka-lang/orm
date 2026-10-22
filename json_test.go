package orm_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

type Letter struct {
	ID       int64
	Ref      string
	Placings orm.JSON[[]int64]
	Settings orm.JSON[map[string]string]
	Spots    []Spot `orm:"elements:placings"`
}

var Letters = orm.For[Letter]()

func TestJSONColumn(t *testing.T) {
	ctx := ormtest.Open(t, Letters)
	l := &Letter{Ref: "EPL-1", Placings: orm.JSONOf([]int64{7, 9}), Settings: orm.JSONOf(map[string]string{"lang": "sw"})}
	if err := Letters.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := Letters.Create(ctx, &Letter{Ref: "EPL-2"}); err != nil {
		t.Fatal(err)
	}
	rows, err := Letters.OrderBy("id").All(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("read %v, %v", rows, err)
	}
	if !reflect.DeepEqual(rows[0].Placings.V, []int64{7, 9}) || rows[0].Settings.V["lang"] != "sw" {
		t.Fatalf("round trip %+v", rows[0])
	}
	if rows[1].Placings.V != nil || rows[1].Settings.V != nil {
		t.Fatalf("NULL read as %+v", rows[1])
	}
	nulls, err := Letters.Filter(orm.Q{"placings__isnull": true}).Values[string]("ref").All(ctx)
	if err != nil || !reflect.DeepEqual(nulls, []string{"EPL-2"}) {
		t.Fatalf("isnull = %v, %v", nulls, err)
	}

	if ormtest.Driver() == "postgres" {
		typ, err := orm.Raw[string](ctx, "SELECT data_type FROM information_schema.columns WHERE table_name = 'letters' AND column_name = 'placings'")
		if err != nil || !reflect.DeepEqual(typ, []string{"jsonb"}) {
			t.Fatalf("column type %v, %v", typ, err)
		}
	}

	out, err := json.Marshal(rows[0])
	if err != nil || !strings.Contains(string(out), `"Placings":[7,9]`) || !strings.Contains(string(out), `"Settings":{"lang":"sw"}`) {
		t.Fatalf("marshal %s, %v", out, err)
	}
	var in Letter
	if err := json.Unmarshal(out, &in); err != nil || !reflect.DeepEqual(in.Placings.V, []int64{7, 9}) {
		t.Fatalf("unmarshal %+v, %v", in, err)
	}

	rows[0].Placings.V = append(rows[0].Placings.V, 11)
	if err := rows[0].Settings.Scan(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Letters.Filter(orm.Q{"id": rows[0].ID}).Update(ctx, orm.Set{"placings": rows[0].Placings, "settings": rows[0].Settings}); err != nil {
		t.Fatal(err)
	}
	back, err := Letters.Get(ctx, orm.Q{"id": rows[0].ID})
	if err != nil || !reflect.DeepEqual(back.Placings.V, []int64{7, 9, 11}) || back.Settings.V != nil {
		t.Fatalf("updated %+v, %v", back, err)
	}
}
