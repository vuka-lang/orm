package orm

import (
	"reflect"
	"strings"
	"testing"
)

type aggregateCategory struct {
	ID       int64
	Name     string
	Products []aggregateProduct
}
type aggregateProduct struct {
	ID                  int64
	AggregateCategoryID int64
	Status              int
}

func TestRelationSQL(t *testing.T) {
	m := For[aggregateCategory](Table("categories"))
	rel := RelationAt("products")
	for _, d := range []Dialect{mysql{}, postgres{}, sqlite{}} {
		q := m.Annotate("n", rel.Where(Q{"products__status__gte": 2}).Count()).q
		b := q.builder(d)
		col, err := b.ref("n")
		if err != nil {
			t.Fatal(err)
		}
		s, err := q.selectSQL(b, col)
		quote := d.Quote
		p := quote("products") + "." + quote("id")
		condition := quote("products") + "." + quote("status") + " >= " + d.Placeholder(1)
		count := "COUNT(DISTINCT CASE WHEN " + condition + " THEN " + p + " END)"
		if d.Name() == "postgres" {
			count = "COUNT(DISTINCT " + p + ") FILTER (WHERE " + condition + ")"
		}
		want := "SELECT (" + count + ") FROM " + quote("categories") + " LEFT JOIN " + quote("aggregate_products") + " AS " + quote("products") + " ON " + quote("products") + "." + quote("aggregate_category_id") + " = " + quote("categories") + "." + quote("id") + " GROUP BY " + quote("categories") + "." + quote("id") + ", " + quote("categories") + "." + quote("name")
		if err != nil || s != want || !reflect.DeepEqual(b.args(), []any{2}) {
			t.Errorf("%s: %s %v %v; want %s", d.Name(), s, b.args(), err, want)
		}
		b = m.Filter().q.builder(d)
		s, err = rel.Exists().exprSQL(b)
		if err != nil || !strings.HasPrefix(s, "EXISTS (SELECT 1 FROM ") || !strings.Contains(s, " IS NOT NULL") {
			t.Errorf("Exists: %s %v", s, err)
		}
	}
}

func TestRelationPlannerSQL(t *testing.T) {
	m := For[aggregateCategory](Table("categories"))
	rel := RelationAt("products")
	for _, d := range []Dialect{mysql{}, postgres{}, sqlite{}} {
		q := m.Annotate("n", rel.Count()).Annotate("sum", rel.Sum(FieldAt[int]("products__status"))).q
		b := q.builder(d)
		n, err := b.ref("n")
		if err != nil {
			t.Fatal(err)
		}
		sum, err := b.ref("sum")
		if err != nil {
			t.Fatal(err)
		}
		s, err := q.selectSQL(b, n+", "+sum)
		if err != nil || strings.Count(s, "SELECT COUNT(DISTINCT") != 1 || strings.Count(s, "SELECT SUM(") != 1 || !strings.Contains(s, "SELECT DISTINCT") {
			t.Errorf("%s: %s, %v", d.Name(), s, err)
		}
		// Both aggregate joins belong to their own subqueries.
		outer := s[strings.LastIndex(s, ") FROM ")+7:]
		if strings.Contains(outer, "LEFT JOIN") {
			t.Errorf("%s: aggregate leaked join: %s", d.Name(), outer)
		}
	}
}
