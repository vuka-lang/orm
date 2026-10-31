package orm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var updateRelationSQL = flag.Bool("update-relation-sql", false, "update relation SQL goldens")

func TestRelationSQLGoldens(t *testing.T) {
	m := For[aggregateCategory](Table("categories"))
	rel := RelationAt("products")
	n := Annotation[int64]("n", rel.Count())
	cases := []struct {
		name string
		v    Values[aggregateCategory, map[string]any]
	}{
		{"count", m.Annotate("n", rel.Where(Q{"products__status__range": []int{2, 3}}).Count()).Values[map[string]any]("id", "name", "n")},
		{"exists", m.Annotate("has", rel.Where(Q{"products__status": 2}).Exists()).Values[map[string]any]("id", "has")},
		{"rows", m.Annotate("n", rel.CountRows()).Values[map[string]any]("id", "n")},
		{"multiple", m.Annotate("n", rel.Count()).Annotate("sum", rel.Sum(FieldAt[int]("products__status"))).Values[map[string]any]("id", "name", "n", "sum")},
		{"values", m.Annotate("avg", rel.Avg(FieldAt[int]("products__status"))).Annotate("min", rel.Min(FieldAt[int]("products__status"))).Annotate("max", rel.Max(FieldAt[int]("products__status"))).Values[map[string]any]("id", "avg", "min", "max")},
		{"having", m.Annotate(n).Filter(Ref[int64]("n").Gt(0)).Order(Desc("n")).Values[map[string]any]("id", "n")},
	}
	for _, d := range []Dialect{mysql{}, postgres{}, sqlite{}} {
		for _, c := range cases {
			t.Run(d.Name()+"/"+c.name, func(t *testing.T) {
				sql, args, _, err := c.v.q.q.valuesPlan(c.v.cols, reflect.TypeFor[map[string]any](), d)
				if err != nil {
					t.Fatal(err)
				}
				got := fmt.Sprintf("%s;\n-- args: %v\n", sql, args)
				path := filepath.Join("testdata", "relation-sql", d.Name()+"-"+c.name+".sql")
				if *updateRelationSQL {
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(path)
				if err != nil || string(want) != got {
					t.Fatalf("%s differs: %v\n%s", path, err, got)
				}
			})
		}
	}
}
