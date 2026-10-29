package orm_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

const secret = "%zq'x%"

func statements(t *testing.T, ctx context.Context) map[string]orm.Statement {
	t.Helper()
	q := Users.Filter(orm.Q{"name__icontains": "zq'x", "age__gte": 18})
	text, args, err := q.OrderBy("-age").Limit(10).SQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]orm.Statement{"select": {Text: text, Args: args}}
	text, args, err = q.Values[string]("name").SQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out["values"] = orm.Statement{Text: text, Args: args}
	for name, build := range map[string]func() (orm.Statement, error){
		"count":     func() (orm.Statement, error) { return q.CountSQL(ctx) },
		"exists":    func() (orm.Statement, error) { return q.ExistsSQL(ctx) },
		"aggregate": func() (orm.Statement, error) { return q.AggregateSQL(ctx, orm.Max("age").As("oldest")) },
		"update":    func() (orm.Statement, error) { return q.UpdateSQL(ctx, orm.Set{"bio": "x"}) },
		"delete":    func() (orm.Statement, error) { return q.DeleteSQL(ctx) },
		"vuka":      func() (orm.Statement, error) { return orm.QueryOf(q).SQL(ctx).Get() },
		"vukacount": func() (orm.Statement, error) { return orm.QueryOf(q).CountSQL(ctx).Get() },
	} {
		st, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = st
	}
	return out
}

func TestStatementShapes(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	db, _ := orm.DBFrom(ctx)
	mark := "?"
	if db.Dialect().Name() == "postgres" {
		mark = "$1"
	}
	starts := map[string]string{
		"select": "SELECT ", "values": "SELECT ", "count": "SELECT COUNT(*) ", "exists": "SELECT 1 ",
		"aggregate": "SELECT MAX(", "update": "UPDATE ", "delete": "DELETE FROM ", "vuka": "SELECT ", "vukacount": "SELECT COUNT(*) ",
	}
	for name, st := range statements(t, ctx) {
		if !strings.HasPrefix(st.Text, starts[name]) || !strings.Contains(st.Text, mark) {
			t.Errorf("%s on %s: %s", name, db.Dialect().Name(), st.Text)
		}
		if strings.Contains(st.Text, "zq") || !slices.Contains(st.Args, any(secret)) || !slices.Contains(st.Args, any(18)) {
			t.Errorf("%s: arguments in the text or missing: %s %v", name, st.Text, st.Args)
		}
		if s := st.String(); !strings.HasPrefix(s, st.Text+"\n-- args: ") || !strings.Contains(s, "'%zq''x%'") {
			t.Errorf("%s String() = %s", name, s)
		}
	}
}

// The statements are what the terminals run: run by hand, they answer
// alike.
func TestStatementRuns(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	db, _ := orm.DBFrom(ctx)
	q := Users.Filter(orm.Q{"age__gte": 18, "active": true})
	st, err := q.CountSQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.SQL().QueryRowContext(ctx, st.Text, st.Args...).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count by hand = %d, %v", n, err)
	}
	if st, err = q.UpdateSQL(ctx, orm.Set{"age": 50}); err != nil {
		t.Fatal(err)
	}
	if res, err := db.SQL().ExecContext(ctx, st.Text, st.Args...); err != nil {
		t.Fatal(err)
	} else if k, _ := res.RowsAffected(); k != 2 {
		t.Fatalf("update by hand changed %d", k)
	}
	if got, _ := Users.Filter(orm.Q{"age": 50}).Count(ctx); got != 2 {
		t.Fatalf("after the update %d rows are 50", got)
	}
}

func TestStatementRefusals(t *testing.T) {
	ctx := open(t)
	if _, err := Users.DeleteSQL(ctx); !errors.Is(err, orm.ErrUnfiltered) {
		t.Errorf("unfiltered DeleteSQL: %v", err)
	}
	if _, err := Users.UpdateSQL(ctx, orm.Set{"age": 1}); !errors.Is(err, orm.ErrUnfiltered) {
		t.Errorf("unfiltered UpdateSQL: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"id": 1}).Limit(2).DeleteSQL(ctx); err == nil || !strings.Contains(err.Error(), "sliced") {
		t.Errorf("sliced DeleteSQL: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"id": 1}).UpdateSQL(ctx, orm.Set{"nope": 1}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("UpdateSQL of no field: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"id": 1}).Explain(ctx, orm.Format("yaml")); err == nil || !strings.Contains(err.Error(), "orm.PlanText or orm.PlanJSON") {
		t.Errorf("Format(yaml): %v", err)
	}
}

func TestExplain(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	q := Users.Filter(orm.Q{"age__gte": 18}).OrderBy("name")
	plan, err := q.Explain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	driver := ormtest.Driver()
	want := map[string]string{"postgres": "Scan on users", "mysql": "users", "sqlite": "SCAN users"}[driver]
	if plan.Database != driver || !strings.Contains(plan.Text, want) || plan.JSON != nil || plan.String() != plan.Text {
		t.Fatalf("plan on %s:\n%s", driver, plan.Text)
	}
	t.Logf("%s plan:\n%s", driver, plan)

	plan, err = q.Explain(ctx, orm.Format(orm.PlanJSON))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(plan.JSON, &v); err != nil || !strings.Contains(string(plan.JSON), "users") {
		t.Fatalf("JSON plan on %s: %v\n%s", driver, err, plan.JSON)
	}
	t.Logf("%s JSON plan:\n%s", driver, plan.JSON)

	plan, err = q.Explain(ctx, orm.Analyze())
	switch driver {
	case "sqlite":
		if err == nil || !strings.Contains(err.Error(), "EXPLAIN QUERY PLAN") {
			t.Fatalf("sqlite Analyze: %v", err)
		}
	default:
		if err != nil || !strings.Contains(plan.Text, "actual") {
			t.Fatalf("Analyze on %s: %v\n%s", driver, err, plan.Text)
		}
		t.Logf("%s analyzed:\n%s", driver, plan)
	}
	_, err = q.Explain(ctx, orm.Analyze(), orm.Format(orm.PlanJSON))
	if driver == "mysql" && (err == nil || !strings.Contains(err.Error(), "as text only")) {
		t.Fatalf("mysql Analyze as JSON: %v", err)
	}
	if driver == "postgres" && err != nil {
		t.Fatal(err)
	}

	for name, st := range statements(t, ctx) {
		if _, err := st.Explain(ctx); err != nil {
			t.Errorf("Explain of %s: %v", name, err)
		}
	}
	if _, err := orm.QueryOf(q).Explain(ctx).Get(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Values[string]("name").Explain(ctx); err != nil {
		t.Fatal(err)
	}
}

// Analyze runs the statement; an Update or Delete is rolled back.
func TestExplainAnalyzeWrite(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	driver := ormtest.Driver()
	q := Users.Filter(orm.Q{"age__gte": 18})
	before, err := Users.OrderBy("id").Values[[]any]("id", "name", "age").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(when string) {
		t.Helper()
		after, err := Users.OrderBy("id").Values[[]any]("id", "name", "age").All(ctx)
		if err != nil || len(after) != len(before) {
			t.Fatalf("%s: %d rows, %v", when, len(after), err)
		}
		for i := range after {
			if !slices.Equal(after[i], before[i]) {
				t.Fatalf("%s: row %v is now %v", when, before[i], after[i])
			}
		}
	}
	update, err := q.UpdateSQL(ctx, orm.Set{"age": 99})
	if err != nil {
		t.Fatal(err)
	}
	del, err := q.DeleteSQL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []orm.Statement{update, del} {
		plan, err := st.Explain(ctx, orm.Analyze())
		switch driver {
		case "postgres":
			if err != nil || !strings.Contains(plan.Text, "actual") {
				t.Fatalf("Analyze of %s: %v\n%s", st.Text, err, plan.Text)
			}
		case "mysql":
			if err == nil || !strings.Contains(err.Error(), "SELECT only") {
				t.Fatalf("mysql Analyze of a write: %v", err)
			}
		case "sqlite":
			if err == nil {
				t.Fatal("sqlite Analyze of a write ran")
			}
		}
		unchanged("after Analyze of " + st.Text)
		if _, err := st.Explain(ctx); err != nil {
			t.Fatalf("Explain of %s: %v", st.Text, err)
		}
		unchanged("after Explain of " + st.Text)
	}
	if driver != "postgres" {
		return
	}
	err = orm.Transaction(ctx, func(ctx context.Context) error {
		if _, err := update.Explain(ctx, orm.Analyze()); err != nil {
			return err
		}
		_, err := Users.Filter(orm.Q{"name": "Ali"}).Update(ctx, orm.Set{"age": 26})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := Users.Filter(orm.Q{"age": 99}).Count(ctx); n != 0 {
		t.Fatalf("Analyze inside a transaction left %d rows changed", n)
	}
	if n, _ := Users.Filter(orm.Q{"name": "Ali", "age": 26}).Count(ctx); n != 1 {
		t.Fatal("the transaction's own write was lost")
	}
}

func TestExplainByHand(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	st := orm.Statement{Text: "SELECT name FROM users WHERE age > ?", Args: []any{20}}
	if ormtest.Driver() == "postgres" {
		st.Text = "SELECT name FROM users WHERE age > $1"
	}
	plan, err := st.Explain(ctx)
	if err != nil || !strings.Contains(plan.Text, "users") {
		t.Fatalf("%v\n%s", err, plan.Text)
	}
	if _, err := (orm.Statement{}).Explain(ctx); err == nil {
		t.Fatal("an empty statement explained")
	}
}
