package orm_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

type CategoryProducts struct {
	ID    int64
	Name  string
	Count int64
}

// joinOpen is the shop of shop_aggregate_test.go: four categories and
// their live (active, on_sale), recently revised products.
func joinOpen(t *testing.T) context.Context {
	t.Helper()
	ctx := ormtest.Open(t, ShopCategories, ProductCategories, ShopProducts)
	now := time.Now().UTC().Truncate(time.Second)
	day := 24 * time.Hour
	ormtest.Seed(t, ctx, ShopCategories,
		&ShopCategory{Name: "empty"}, &ShopCategory{Name: "one"}, &ShopCategory{Name: "several"}, &ShopCategory{Name: "none live"})
	ormtest.Seed(t, ctx, ShopProducts,
		&ShopProduct{Name: "kettle", Status: "active", RevisedAt: now.Add(-2 * day)},
		&ShopProduct{Name: "toaster", Status: "draft", RevisedAt: now.Add(-2 * day)},
		&ShopProduct{Name: "radio", Status: "active", RevisedAt: now.Add(-90 * day)},
		&ShopProduct{Name: "lamp", Status: "on_sale", RevisedAt: now.Add(-10 * day)},
		&ShopProduct{Name: "fan", Status: "active", RevisedAt: now.Add(-1 * day)},
		&ShopProduct{Name: "heater", Status: "archived", RevisedAt: now.Add(-1 * day)},
	)
	link := func(c, p int64) *ProductCategory { return &ProductCategory{CategoryID: c, ProductID: p} }
	ormtest.Seed(t, ctx, ProductCategories,
		link(2, 1), link(2, 2), link(2, 3),
		link(3, 1), link(3, 4), link(3, 5), link(3, 3), link(3, 6),
		link(4, 2), link(4, 3),
	)
	return ctx
}

var (
	category = ShopCategoryColumns
	link     = ProductCategoryColumns
	product  = ShopProductColumns
)

// liveProducts is the issue's query: each category's live products revised
// in the last 30 days, the conditions in the joins' ON, so a category with
// none keeps its row and counts 0.
func liveProducts() orm.Values[ShopCategory, CategoryProducts] {
	return ShopCategories.
		Join(link.On(link.CategoryID.EqCol(category.ID)), orm.Left).
		Join(product.On(
			link.Product().Eq(product.ID),
			product.Status.In("active", "on_sale"),
			product.RevisedAt.Gte(orm.Today().AddDays(-30)),
		), orm.Left).
		OrderBy(category.Name).
		Values[CategoryProducts](category.ID, category.Name, orm.Count(product.ID))
}

func TestJoinValues(t *testing.T) {
	ctx := joinOpen(t)
	q := liveProducts()
	got, err := q.All(ctx)
	want := []CategoryProducts{{1, "empty", 0}, {4, "none live", 0}, {2, "one", 1}, {3, "several", 3}}
	s, args, _ := q.SQL(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %v, %v; want %v\n%s %v", got, err, want, s, args)
	}
	today := map[string]string{
		"sqlite":   `date(date('now'), '-30 day')`,
		"postgres": `(CURRENT_DATE + -30)`,
		"mysql":    `DATE_SUB(CURDATE(), INTERVAL 30 DAY)`,
	}[ormtest.Driver()]
	ph := func(n int) string { return orm.DialectFor(ormtest.Driver()).Placeholder(n) }
	quote := orm.DialectFor(ormtest.Driver()).Quote
	c := func(table, col string) string { return quote(table) + "." + quote(col) }
	wantSQL := "SELECT " + c("categories", "id") + ", " + c("categories", "name") + ", COUNT(" + c("products", "id") + ")" +
		" FROM " + quote("categories") +
		" LEFT JOIN " + quote("product_categories") + " ON " + c("product_categories", "category_id") + " = " + c("categories", "id") +
		" LEFT JOIN " + quote("products") + " ON (" + c("product_categories", "product_id") + " = " + c("products", "id") +
		" AND " + c("products", "status") + " IN (" + ph(1) + ", " + ph(2) + ")" +
		" AND " + c("products", "revised_at") + " >= " + today + ")" +
		" GROUP BY 1, 2 ORDER BY " + c("categories", "name") + " ASC"
	t.Logf("%s %v", s, args)
	if s != wantSQL || !reflect.DeepEqual(args, []any{"active", "on_sale"}) {
		t.Errorf("SQL =\n%s %v\nwant\n%s", s, args, wantSQL)
	}

	// The relation shorthand: ON from the foreign key, the product's
	// conditions on its own join.
	short := ShopCategories.
		Join(category.Links(), orm.Left).
		Join(category.Links().Product().On(
			product.Status.In("active", "on_sale"),
			product.RevisedAt.Gte(orm.Today().AddDays(-30)),
		), orm.Left).
		OrderBy(category.Name).
		Values[CategoryProducts](category.ID, category.Name, orm.Count(product.ID))
	if got, err := short.All(ctx); err != nil || !reflect.DeepEqual(got, want) {
		s, args, _ := short.SQL(ctx)
		t.Errorf("relation join = %v, %v\n%s %v", got, err, s, args)
	}

	// Distinct counts a product a join repeats once; an inner join keeps
	// only the categories with a match.
	inner := ShopCategories.
		Join(link.On(link.CategoryID.EqCol(category.ID))).
		Join(product.On(link.Product().Eq(product.ID))).
		OrderBy(category.Name.Desc()).
		Values[[]any](category.Name, orm.Count(product.Status, orm.Distinct), orm.Max(product.Name))
	rows, err := inner.All(ctx)
	wantRows := [][]any{{"several", int64(3), "radio"}, {"one", int64(2), "toaster"}, {"none live", int64(2), "toaster"}}
	if err != nil || !reflect.DeepEqual(rows, wantRows) {
		s, args, _ := inner.SQL(ctx)
		t.Errorf("inner = %v, %v\n%s %v", rows, err, s, args)
	}
	if s, _, _ := inner.SQL(ctx); !strings.Contains(s, "COUNT(DISTINCT ") || strings.Contains(s, "LEFT JOIN") {
		t.Errorf("inner SQL = %s", s)
	}
}

func TestJoinRows(t *testing.T) {
	ctx := joinOpen(t)
	// All without Values reads the queried model: a row per match.
	got, err := ShopCategories.
		Join(link.On(link.CategoryID.EqCol(category.ID))).
		Join(product.On(link.Product().Eq(product.ID))).
		Filter(product.Name.Eq("kettle")).
		OrderBy(category.ID).All(ctx)
	if err != nil || len(got) != 2 || got[0].Name != "one" || got[1].Name != "several" {
		t.Fatalf("rows = %v, %v", got, err)
	}
	n, err := ShopCategories.Join(link.On(link.CategoryID.EqCol(category.ID))).Filter(link.ProductID.Eq(3)).Count(ctx)
	if err != nil || n != 3 {
		t.Errorf("count = %d, %v", n, err)
	}
	// GroupBy by a typed field, an ordering by the aggregate.
	top, err := ShopCategories.
		Join(link.On(link.CategoryID.EqCol(category.ID)), orm.Left).
		GroupBy(category.ID).
		OrderBy(orm.Desc(orm.Count(link.ID)), category.ID).
		Limit(1).
		Values[string](category.Name).All(ctx)
	if err != nil || !reflect.DeepEqual(top, []string{"several"}) {
		t.Errorf("top = %v, %v", top, err)
	}
}

func TestJoinAlias(t *testing.T) {
	ctx := relOpen(t)
	other := BookColumns.As("other")
	q := Books.
		Join(other.On(other.AuthorID.EqCol(BookColumns.AuthorID), orm.SQLOf[bool]("{0} < {1}", BookColumns.ID, other.ID))).
		OrderBy(BookColumns.ID).
		Values[[]any](BookColumns.Title, other.Title)
	got, err := q.All(ctx)
	if err != nil || !reflect.DeepEqual(got, [][]any{{"Go in Practice", "Poems"}}) {
		s, args, _ := q.SQL(ctx)
		t.Fatalf("pairs = %v, %v\n%s %v", got, err, s, args)
	}
	quote := orm.DialectFor(ormtest.Driver()).Quote
	if s, _, _ := q.SQL(ctx); !strings.Contains(s, " JOIN "+quote("books")+" AS "+quote("other")+" ON ") {
		t.Errorf("SQL = %s", s)
	}
}

func TestJoinErrors(t *testing.T) {
	ctx := joinOpen(t)
	for _, c := range []struct {
		name string
		q    interface {
			SQL(context.Context) (string, []any, error)
		}
		want string
	}{
		{"not joined", ShopCategories.Filter(product.Status.Eq("active")).Values[string](category.Name), "ShopProduct isn't joined"},
		{"joined later", ShopCategories.Join(link.On(product.ID.Eq(1))).Join(product.On()).Values[string](category.Name), "joined after this Join"},
		{"twice", ShopCategories.Join(product.On()).Join(product.On()).Values[string](category.Name), "joined twice"},
		{"own model", ShopCategories.Join(category.On()).Values[string](category.Name), "query's own model"},
		{"fields by position", ShopCategories.Values[CategoryProducts](category.ID, category.Name), "fills its 3 fields by position"},
		{"field type", ShopCategories.Values[CategoryProducts](category.Name, category.ID, orm.Count(category.ID)), "field ID is int64, and column 1 (name) is string"},
	} {
		_, _, err := c.q.SQL(ctx)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestSQLFragments(t *testing.T) {
	ctx := joinOpen(t)
	since := orm.SQLFor[time.Time](orm.Dialects{
		orm.MySQL:    "DATE_SUB(CURDATE(), INTERVAL ? DAY)",
		orm.Postgres: "CURRENT_DATE - CAST(? AS INTEGER)",
		orm.SQLite:   "date('now', '-' || ? || ' day')",
	}, 30)
	recent := ShopProducts.Filter(product.RevisedAt.Gte(since)).OrderBy(product.ID).Values[string](product.Name)
	got, err := recent.All(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"kettle", "toaster", "lamp", "fan", "heater"}) {
		s, args, _ := recent.SQL(ctx)
		t.Fatalf("SQLFor = %v, %v\n%s %v", got, err, s, args)
	}

	// A boolean fragment is a condition; {n} writes a column, ? binds.
	live := orm.SQLOf[bool]("{0} IN (?, ?)", product.Status, "active", "on_sale")
	names, err := ShopProducts.Filter(live).Exclude(orm.SQLOf[bool]("{0} = ?", product.Name, "radio")).
		OrderBy(orm.SQLOf[int]("LENGTH({0})", product.Name), product.ID).Values[string](product.Name).All(ctx)
	if err != nil || !reflect.DeepEqual(names, []string{"fan", "lamp", "kettle"}) {
		t.Errorf("fragment condition = %v, %v", names, err)
	}

	// In Values, a fragment of an aggregate groups; in Update, it computes.
	counts, err := ShopCategories.Join(link.On(link.CategoryID.EqCol(category.ID)), orm.Left).OrderBy(category.ID).
		Values[[]any](category.ID, orm.SQLOf[string]("UPPER({0})", category.Name), orm.SQLOf[int64]("{0} * ?", orm.Count(link.ID), 10)).All(ctx)
	if err != nil || len(counts) != 4 || !reflect.DeepEqual(counts[2], []any{int64(3), "SEVERAL", int64(50)}) {
		t.Errorf("fragment values = %v, %v", counts, err)
	}
	if _, err := ShopProducts.Filter(product.Name.Eq("fan")).Update(ctx, orm.Set{"name": orm.SQLOf[string]("{0} || ?", product.Name, "!").For(orm.Postgres)}); ormtest.Driver() == "postgres" && err != nil {
		t.Errorf("update = %v", err)
	} else if ormtest.Driver() != "postgres" && (err == nil || !strings.Contains(err.Error(), "has no SQL for "+ormtest.Driver()+" (it has postgres)")) {
		t.Errorf("For(Postgres) elsewhere = %v", err)
	}
	others := orm.Dialects{orm.MySQL: "1", orm.Postgres: "1", orm.SQLite: "1"}
	delete(others, orm.DialectName(ormtest.Driver()))
	if _, _, err := ShopProducts.Values[int](orm.SQLFor[int](others)).SQL(ctx); err == nil || !strings.Contains(err.Error(), "has no SQL for "+ormtest.Driver()) {
		t.Errorf("missing dialect = %v", err)
	}
	for _, c := range []struct {
		e    orm.Expr
		want string
	}{
		{orm.SQLOf[int]("? + ?", 1), "has 2 ? for 1 arguments"},
		{orm.SQLOf[int]("{1}", product.ID), "has no argument {1}"},
		{orm.SQLOf[int]("{0} + ?", product.ID, product.ID), "binds"},
	} {
		if _, _, err := ShopProducts.Values[int](c.e).SQL(ctx); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error = %v, want %q", c.e, err, c.want)
		}
	}
	if _, _, err := ShopProducts.Filter(orm.SQLOf[int]("1")).Values[int](product.ID).SQL(ctx); err == nil || !strings.Contains(err.Error(), "is no condition") {
		t.Errorf("non-bool condition = %v", err)
	}
}

func TestTypedRelations(t *testing.T) {
	ctx := relOpen(t)
	books, err := Books.SelectRelated(BookColumns.Author()).PrefetchRelated(BookColumns.Tags()).OrderBy(BookColumns.ID).All(ctx)
	if err != nil || len(books) != 3 || books[0].Author == nil || books[0].Author.Name != "Ali" || len(books[0].Tags) != 1 {
		t.Fatalf("books = %+v, %v", books, err)
	}
	// A relation in Values reads its key; a typed relation path's field
	// reads through its join.
	rows, err := Books.OrderBy(BookColumns.ID).Values[[]any](BookColumns.Author(), BookColumns.Author().Name).All(ctx)
	if err != nil || !reflect.DeepEqual(rows[2], []any{int64(2), "Neema"}) {
		t.Errorf("rows = %v, %v", rows, err)
	}
}
