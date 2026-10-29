package orm_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// A shop's categories hold products through a link table: the count of
// each category's live products, recently revised, is the everyday
// per-parent count through a relation path.
type ShopCategory struct {
	ID    int64
	Name  string
	Links []ProductCategory `gorm:"foreignKey:CategoryID"`
}

func (ShopCategory) TableName() string { return "categories" }

type ProductCategory struct {
	ID         int64
	CategoryID int64
	ProductID  int64
	Product    *ShopProduct `gorm:"foreignKey:ProductID"`
}

type ShopProduct struct {
	ID        int64
	Name      string
	Status    string
	RevisedAt time.Time // not UpdatedAt: that one is set on every save
}

func (ShopProduct) TableName() string { return "products" }

var (
	ShopCategories    = orm.For[ShopCategory]()
	ProductCategories = orm.For[ProductCategory]()
	ShopProducts      = orm.For[ShopProduct]()
)

// TestPerParentRelationCounts: a relation path's Where(…).Count(), by
// name or through the generated fields, a FilteredRelation counted
// distinct, and a filtered distinct Count give each category the same
// count of its qualifying products, 0 where it has none, on every
// database.
func TestPerParentRelationCounts(t *testing.T) {
	ctx := ormtest.Open(t, ShopCategories, ProductCategories, ShopProducts)
	now := time.Now().UTC().Truncate(time.Second)
	day := 24 * time.Hour
	ormtest.Seed(t, ctx, ShopCategories,
		&ShopCategory{Name: "empty"}, &ShopCategory{Name: "one"}, &ShopCategory{Name: "several"}, &ShopCategory{Name: "none live"})
	ormtest.Seed(t, ctx, ShopProducts,
		&ShopProduct{Name: "kettle", Status: "active", RevisedAt: now.Add(-2 * day)},
		&ShopProduct{Name: "toaster", Status: "draft", RevisedAt: now.Add(-2 * day)}, // not live
		&ShopProduct{Name: "radio", Status: "active", RevisedAt: now.Add(-90 * day)}, // too old
		&ShopProduct{Name: "lamp", Status: "on_sale", RevisedAt: now.Add(-10 * day)},
		&ShopProduct{Name: "fan", Status: "active", RevisedAt: now.Add(-1 * day)},
		&ShopProduct{Name: "heater", Status: "archived", RevisedAt: now.Add(-1 * day)}, // not live
	)
	link := func(c, p int64) *ProductCategory { return &ProductCategory{CategoryID: c, ProductID: p} }
	ormtest.Seed(t, ctx, ProductCategories,
		link(2, 1), link(2, 2), link(2, 3), // one: kettle
		link(3, 1), link(3, 4), link(3, 4), link(3, 5), link(3, 3), link(3, 6), // several: kettle, lamp (linked twice), fan
		link(4, 2), link(4, 3), // none live
	)
	live := []string{"active", "on_sale"}
	since := now.Add(-30 * day)
	want := [][]any{{"empty", int64(0)}, {"one", int64(1)}, {"several", int64(3)}, {"none live", int64(0)}}

	path := orm.RelationAt("links__product")
	viaPath := ShopCategories.Annotate("n", path.Where(orm.Q{
		"links__product__status__in":      live,
		"links__product__revised_at__gte": since,
	}).Count()).OrderBy("id").Values[[]any]("name", "n")

	product := ShopCategoryColumns.Links().Product()
	viaTyped := ShopCategories.Annotate("n", product.Where(product.Status.In(live...), product.RevisedAt.Gte(since)).Count()).
		OrderBy("id").Values[[]any]("name", "n")

	viaFiltered := ShopCategories.Annotate("live", orm.FilteredRelation("links__product", orm.Q{
		"status__in":      live,
		"revised_at__gte": since,
	})).Annotate("n", orm.Count("live__id").Distinct()).GroupBy("id").OrderBy("id").Values[[]any]("name", "n")

	viaFilter := ShopCategories.Annotate("n", orm.Count("links__product__id").Distinct().Filter(orm.Q{
		"links__product__status__in":      live,
		"links__product__revised_at__gte": since,
	})).GroupBy("id").OrderBy("id").Values[[]any]("name", "n")

	for _, c := range []struct {
		name string
		q    orm.Values[ShopCategory, []any]
	}{{"relation path", viaPath}, {"typed relation path", viaTyped}, {"FilteredRelation", viaFiltered}, {"filtered Count", viaFilter}} {
		got, err := c.q.All(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			s, args, _ := c.q.SQL(ctx)
			t.Errorf("%s: counts = %v, %v; want %v\n%s %v", c.name, got, err, want, s, args)
			continue
		}
		s, _, err := c.q.SQL(ctx)
		if err != nil || !strings.Contains(s, "LEFT JOIN") || !strings.Contains(s, "COUNT(DISTINCT") {
			t.Errorf("%s: SQL = %s, %v", c.name, s, err)
		}
		// MySQL has no FILTER (WHERE …): the aggregate counts a CASE.
		if ormtest.Driver() == "mysql" && strings.Contains(s, "FILTER") {
			t.Errorf("%s: MySQL SQL uses FILTER: %s", c.name, s)
		}
	}
}
