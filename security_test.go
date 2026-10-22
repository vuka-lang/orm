package orm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// Input a request carries into the ORM: filter values, search terms,
// field and sort names, page sizes and lists of ids. These run on SQLite
// and, with ORMTEST_DRIVER, on Postgres and MySQL.

type SecItem struct {
	ID       int64
	Name     string `orm:"unique"`
	Age      int
	ParentID *int64
	Parent   *SecItem
}

var SecItems = orm.For[SecItem]()

// Every LIKE lookup matches its value literally: %, _, the escape
// character and backslashes are text.
func TestSecurityLikeIsLiteral(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	names := []string{`a%b`, `a_b`, `axb`, `a!b`, `a\b`, `a\\b`, `a'b`, `a"b`, `100%`, `x\%y`, `x\_y`, `x!%y`, `x!!y`}
	for _, n := range names {
		ormtest.Seed(t, ctx, SecItems, &SecItem{Name: n})
	}
	for _, lk := range []string{"contains", "icontains", "startswith", "istartswith", "endswith", "iendswith", "exact", "iexact"} {
		for _, n := range names {
			got, err := SecItems.Filter(orm.Q{"name__" + lk: n}).Values[string]("name").All(ctx)
			if err != nil || !slices.Equal(got, []string{n}) {
				t.Errorf("%s %q matched %q (%v)", lk, n, got, err)
			}
		}
	}
	page, err := orm.Paginate(ctx, SecItems.Filter(), orm.PageRequest{Search: "%"}, orm.Searchable("name"), orm.DefaultSort("id"))
	if err != nil || page.Total != 4 {
		t.Errorf("search %%: %d rows, %v", page.Total, err)
	}
}

// A name of more parts than any model needs (a key, an order, a Values or
// SelectRelated path a request names) fails before any SQL is written:
// thousands of nested joins or EXISTS took seconds to write and exhausted
// the server's memory.
func TestSecurityPathDepth(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	ormtest.Seed(t, ctx, SecItems, &SecItem{Name: "p"})
	ctx, queries := ormtest.CountQueries(ctx)
	deep := strings.Repeat("parent__", 40) + "name"
	many := strings.Repeat("sec_items__", 5000) + "name"
	for name, run := range map[string]func() error{
		"filter": func() error { _, err := SecItems.Filter(orm.Q{deep: "x"}).Count(ctx); return err },
		"exists": func() error { _, err := SecItems.Filter(orm.Q{many: "x"}).Count(ctx); return err },
		"order":  func() error { _, err := SecItems.OrderBy("-" + deep).All(ctx); return err },
		"values": func() error { _, err := SecItems.Values[string](deep).All(ctx); return err },
		"F":      func() error { _, err := SecItems.Filter(orm.Q{"name": orm.F(deep)}).All(ctx); return err },
		"select": func() error {
			_, err := SecItems.SelectRelated(strings.TrimSuffix(deep, "__name")).All(ctx)
			return err
		},
		"prefetch": func() error {
			_, err := SecItems.PrefetchRelated(strings.Repeat("sec_items__", 40) + "sec_items").All(ctx)
			return err
		},
	} {
		start := time.Now()
		err := run()
		if err == nil || !strings.Contains(err.Error(), "has more than 32 parts") {
			t.Errorf("%s: %v", name, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s took %v", name, d)
		}
	}
	if n := queries(); n > 1 { // the prefetch's parents
		t.Errorf("%d statements ran", n)
	}
	// A deep path that is still a path works.
	if _, err := SecItems.Filter(orm.Q{strings.Repeat("parent__", 5) + "name__icontains": "x"}).Count(ctx); err != nil {
		t.Fatal(err)
	}
}

// An __in of a list longer than one statement can carry fails with the
// reason, without the statement reaching the database.
func TestSecurityArgumentLimit(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	ctx, queries := ormtest.CountQueries(ctx)
	ids := make([]int64, 70000)
	for i := range ids {
		ids[i] = int64(i)
	}
	_, err := SecItems.Filter(orm.Q{"id__in": ids}).Count(ctx)
	if err == nil || !strings.Contains(err.Error(), "takes at most") || queries() != 0 {
		t.Fatalf("%v, %d statements", err, queries())
	}
	if _, err := SecItems.Filter(orm.Q{"id__in": ids[:30000]}).Count(ctx); err != nil {
		t.Fatal(err)
	}
}

// A unique violation names the field it is on, whatever the value quotes.
func TestSecurityViolationNamesItsField(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	v := "x' for key 'age"
	ormtest.Seed(t, ctx, SecItems, &SecItem{Name: v})
	e, ok := errors.AsType[*orm.Error](SecItems.Create(ctx, &SecItem{Name: v}))
	if !ok || e.Kind != orm.Conflict || len(e.Fields) != 1 || e.Fields["name"] == nil {
		t.Fatalf("%+v", e)
	}
}

type SecRef struct {
	ID  int64
	Ref string
}

var SecRefs = orm.For[SecRef]()

// A database error quoting a value from the request (one that names a
// violation's code or words) is not taken for a violation.
func TestSecurityViolationNotFromValue(t *testing.T) {
	ctx := ormtest.Open(t)
	switch ormtest.Driver() {
	case "postgres":
		ormtest.Exec(t, ctx, "CREATE TABLE sec_refs (id BIGSERIAL PRIMARY KEY, ref UUID NOT NULL)")
	case "mysql":
		ormtest.Exec(t, ctx, "CREATE TABLE sec_refs (id BIGINT AUTO_INCREMENT PRIMARY KEY, ref DATETIME NOT NULL)")
	default:
		t.Skip("SQLite quotes no value in an error")
	}
	err := SecRefs.Create(ctx, &SecRef{Ref: "1062 23505 duplicate key Duplicate entry"})
	if err == nil || orm.KindOf(err) != 0 {
		t.Fatalf("%v: %v", orm.KindOf(err), err)
	}
}

// Linking a row to a long list of ids (one a request sent) costs the
// list's length, not its square.
func TestSecurityLinkManyKeys(t *testing.T) {
	ctx := modelOpen(t)
	c := Customer{Email: "a@x"}
	if err := c.Save(ctx); err != nil {
		t.Fatal(err)
	}
	ids := make([]any, 100000)
	for i := range ids {
		ids[i] = int64(i % 50000)
	}
	start := time.Now()
	err := c.Add(ctx, "groups", ids...)
	if err == nil || !strings.Contains(err.Error(), "takes at most") {
		t.Errorf("Add: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Add of %d keys took %v", len(ids), d)
	}
}

// Paginate takes a misconfigured size, and a sort naming one field again
// and again orders by it once.
func TestSecurityPaginate(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	ormtest.Seed(t, ctx, SecItems, &SecItem{Name: "a"}, &SecItem{Name: "b"})
	page, err := orm.Paginate(ctx, SecItems.Filter(), orm.PageRequest{Size: 5}, orm.MaxSize(0), orm.DefaultSize(0))
	if err != nil || page.Size != 1 || page.Pages != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	var sqls []string
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) { sqls = append(sqls, q.SQL) })
	sort := strings.Repeat("name,-name,", 5000) + "age"
	if _, err := orm.Paginate(ctx, SecItems.Filter(), orm.PageRequest{Sort: sort}, orm.Sortable("name", "age")); err != nil {
		t.Fatal(err)
	}
	last := sqls[len(sqls)-1]
	if strings.Count(last, " ASC")+strings.Count(last, " DESC") != 2 {
		t.Fatalf("order of %s", last)
	}
}

// Count of a sliced or distinct query sends its conditions' arguments
// once (they were sent twice, failing on Postgres and MySQL).
func TestCountSlicedFiltered(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	ormtest.Seed(t, ctx, SecItems, &SecItem{Name: "a", Age: 3}, &SecItem{Name: "b", Age: 3}, &SecItem{Name: "c", Age: 4})
	for _, c := range []struct {
		qs   orm.QuerySet[SecItem]
		want int64
	}{
		{SecItems.Filter(orm.Q{"age": 3}).Distinct(), 2},
		{SecItems.Filter(orm.Q{"age": 3}).Limit(1), 1},
		{SecItems.Filter(orm.Q{"age__gte": 3}).OrderBy("id").Offset(1), 2},
	} {
		if n, err := c.qs.Count(ctx); err != nil || n != c.want {
			t.Errorf("count %d, %v; want %d", n, err, c.want)
		}
	}
}

// Raw SQL's ? marks are found where the database reads code, so each
// argument binds the mark it was written for.
func TestSecurityRawMarks(t *testing.T) {
	ctx := ormtest.Open(t)
	type rawCase struct {
		sql  string
		args []any
		want []any
	}
	var cases []rawCase
	switch ormtest.Driver() {
	case "postgres":
		cases = []rawCase{
			{`SELECT E'it\'s ?' AS a, CAST(? AS INT) AS b`, []any{5}, []any{"it's ?", int64(5)}},
			{"SELECT /* a /* nested ? */ comment ? */ CAST(? AS INT) AS b", []any{7}, []any{int64(7)}},
			{"SELECT $fn1$ ? $fn1$ AS a, CAST(? AS INT) AS b", []any{3}, []any{" ? ", int64(3)}},
			{"SELECT 1 AS a$b$, CAST(? AS INT) AS c$b$", []any{4}, []any{int64(1), int64(4)}},
			{`SELECT '{"k":1}'::jsonb ?? 'k' AS a, CAST(? AS INT) AS b`, []any{2}, []any{true, int64(2)}},
			{"SELECT CAST(? AS INT) AS a OFFSET?", []any{9, 0}, []any{int64(9)}},
		}
	case "mysql":
		cases = []rawCase{
			{"SELECT 5--1 AS a, ? AS b", []any{2}, []any{int64(6), int64(2)}},
			{"SELECT ? AS a # a comment ?\n", []any{1}, []any{int64(1)}},
			{`SELECT 'it\'s ?' AS a, "say \"?\"" AS b, ? AS c`, []any{3}, []any{"it's ?", `say "?"`, int64(3)}},
		}
	default:
		cases = []rawCase{
			{`SELECT 'C:\' AS a, ? AS b`, []any{1}, []any{`C:\`, int64(1)}},
			{"SELECT ? AS a /* ? */ -- ?\n", []any{2}, []any{int64(2)}},
		}
	}
	for _, c := range cases {
		rows, err := orm.Raw[[]any](ctx, c.sql, c.args...)
		if err != nil || len(rows) != 1 || len(rows[0]) != len(c.want) {
			t.Errorf("%q: %v %v", c.sql, rows, err)
			continue
		}
		for i, v := range rows[0] {
			switch n := v.(type) {
			case []byte:
				v = string(n)
			case int32:
				v = int64(n)
			}
			if v != c.want[i] {
				t.Errorf("%q column %d = %#v, want %#v", c.sql, i, v, c.want[i])
			}
		}
	}
}

// TestSecurityHostileValues round-trips values aimed at a driver's
// escaping (MySQL interpolates arguments into the text by default): each
// is stored and read back exactly, or refused by the database — never
// read as SQL.
func TestSecurityHostileValues(t *testing.T) {
	ctx := ormtest.Open(t, SecItems)
	ormtest.Seed(t, ctx, SecItems, &SecItem{Name: "canary"})
	for i, v := range []string{`'; DROP TABLE sec_items; --`, `\'; DROP TABLE sec_items; --`, `a\`, `\\'`, `"; SELECT 1; --`, "\xbf\x27 OR 1=1 -- ", "nul\x00byte", "¿' OR '1'='1", "/*", "?", "$1"} {
		err := SecItems.Create(ctx, &SecItem{Name: v, Age: i})
		if err == nil {
			got, err := SecItems.Filter(orm.Q{"name": v}).Values[string]("name").All(ctx)
			if err != nil || !slices.Equal(got, []string{v}) {
				t.Errorf("%q came back %q (%v)", v, got, err)
			}
		}
		if n, err := SecItems.Filter(orm.Q{"name": "canary"}).Count(ctx); err != nil || n != 1 {
			t.Fatalf("after %q the table holds %d canaries (%v)", v, n, err)
		}
	}
}
