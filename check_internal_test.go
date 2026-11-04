package orm

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type ckRow struct {
	ID        int64
	Title     string
	Posts     int
	Status    string
	Score     float64
	Open      bool
	OpenDate  time.Time
	CloseDate time.Time
	Note      *string
	OwnerID   *int64
	Owner     *ckOwner `gorm:"foreignKey:OwnerID"`
}

type ckOwner struct {
	ID   int64
	Name string
}

// rules is ckRow's checks as registration makes them, on table ck_rows.
func rules(t *testing.T, checks ...RowCheck) ([]rule, error) {
	t.Helper()
	m, err := buildModel(reflect.TypeFor[ckRow](), "", "ck_rows")
	if err != nil {
		t.Fatal(err)
	}
	m.checks = checks
	return m.declaredRules()
}

// TestCheckSQL is each check's CHECK condition on every database it is
// made on ("" where it is made on none), and its name and message.
func TestCheckSQL(t *testing.T) {
	for _, c := range []struct {
		check                   RowCheck
		name, message           string
		postgres, mysql, sqlite string
	}{
		{CheckOf(Q{"posts__gt": 0}), "ck_rows_posts_check", "posts must be greater than 0",
			`"posts" > 0`, "`posts` > 0", `"posts" > 0`},
		{CheckOf(Q{"close_date__gte": F("open_date")}).Field("close_date").Message("closes before it opens"),
			"ck_rows_close_date_check", "closes before it opens",
			`"close_date" >= "open_date"`, "`close_date` >= `open_date`", `"close_date" >= "open_date"`},
		{CheckOf(Q{"status__in": []string{"draft", "open", "it's"}}), "ck_rows_status_check", "status must be one of draft, open, it's",
			`"status" IN ('draft', 'open', 'it''s')`, "`status` IN ('draft', 'open', 'it''s')", `"status" IN ('draft', 'open', 'it''s')`},
		{CheckOf(Q{"owner__isnull": false}).When(Q{"status": "open"}), "ck_rows_owner_check", "when status is open, owner must be set",
			`(NOT ("status" = 'open') OR "owner_id" IS NOT NULL)`, "(NOT (`status` = 'open') OR `owner_id` IS NOT NULL)", `(NOT ("status" = 'open') OR "owner_id" IS NOT NULL)`},
		{CheckOf(Or(Q{"title__istartswith": "a"}, Q{"posts__range": []int{1, 9}})).On("postgres", "mysql"), "ck_rows_title_posts_check",
			"title must start with a in any case or posts must be between 1 and 9",
			`("title" ILIKE 'a%' ESCAPE '!' OR "posts" BETWEEN 1 AND 9)`, "(LOWER(`title`) LIKE LOWER('a%') ESCAPE '!' OR `posts` BETWEEN 1 AND 9)", ""},
		{CheckOf(Not(Q{"note__contains": "x"})).On(), "ck_rows_note_check", "fails the check ck_rows_note_check", "", "", ""},
		{CheckOf(Where(Lower.Of(F("title")), "exact", "x")).Name("ck_rows_title_lower"), "ck_rows_title_lower", "fails the check ck_rows_title_lower",
			`lower("title") = 'x'`, "lower(`title`) = 'x'", `lower("title") = 'x'`},
	} {
		got, err := rules(t, c.check)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r := got[0]
		if r.name != c.name || r.message != c.message || r.sql.Postgres != c.postgres || r.sql.MySQL != c.mysql || r.sql.SQLite != c.sqlite {
			t.Errorf("%s: %q %q\n  %s\n  %s\n  %s", c.name, r.name, r.message, r.sql.Postgres, r.sql.MySQL, r.sql.SQLite)
		}
	}
}

// TestCheckRules is what registration refuses: a check reading another
// table, a field it doesn't have, a value not suiting its column, names
// that clash.
func TestCheckRules(t *testing.T) {
	for _, c := range []struct {
		checks []RowCheck
		want   string
	}{
		{[]RowCheck{CheckOf(Q{"owner__name": "x"})}, `"owner__name" reads ckRow.Owner, another table's rows`},
		{[]RowCheck{CheckOf(Q{"nope__gt": 1})}, `"nope__gt" names no field of ckRow`},
		{[]RowCheck{CheckOf(Q{"posts__gt": "x"})}, `"posts__gt" compares posts with "x", text: it takes a number`},
		{[]RowCheck{CheckOf(Q{"status__in": []any{"a", 2}})}, `"status__in" compares status with 2, a number: it takes text`},
		{[]RowCheck{CheckOf(Q{"note__isnull": "no"})}, `it takes a bool`},
		{[]RowCheck{CheckOf(Q{"posts__gt": F("ownr")})}, `compares with ownr, which names no field`},
		{[]RowCheck{CheckOf(Q{"posts__gt": 0}), CheckOf(Q{"posts__lt": 9})}, "declares two checks named ck_rows_posts_check"},
		{[]RowCheck{CheckOf(Q{"posts__gt": 0}).Field("post")}, "Field names post, no field of ckRow"},
		{[]RowCheck{CheckOf(Where(Lower.Of(F("title")), "exact", "x"))}, "a check of no field needs a Name"},
		{[]RowCheck{CheckOf(Q{}).Name("c")}, "it has no condition"},
		{[]RowCheck{CheckOf(Q{"posts__gt": 0}).On("oracle")}, `On names postgres, mysql or sqlite, not "oracle"`},
		{[]RowCheck{CheckOf(Q{"open_date__gt": time.Now()})}, "its values are bools, numbers, strings or nil"},
	} {
		_, err := rules(t, c.checks...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v, want %q", err, c.want)
		}
	}
}

// TestCheckEval is each lookup Go evaluates, against SQLite's answer for
// the same rows: the rows a CHECK of the condition refuses, those for
// which it is false (NULL lets a row through).
func TestCheckEval(t *testing.T) {
	isolate(t)
	rows := For[ckRow](Table("ck_rows"))
	ctx := WithDB(context.Background(), memDB(t))
	if err := CreateTables(ctx, rows); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	note := func(s string) *string { return &s }
	seed := []*ckRow{
		{Title: "Alpha", Posts: 0, Status: "draft", Score: 1.5, OpenDate: day, CloseDate: day.AddDate(0, 0, -1), Note: note("Xray")},
		{Title: "beta", Posts: 3, Status: "Open", Score: 2, Open: true, OpenDate: day, CloseDate: day, Note: note("box")},
		{Title: "GAMMA_1", Posts: 10, Status: "open", Score: -1, Open: true, OpenDate: day, CloseDate: day.AddDate(0, 1, 0)},
		{Title: "", Posts: -2, Status: "closed", OpenDate: day.AddDate(1, 0, 0), CloseDate: day, Note: note("")},
		{Title: "100%", Posts: 5, Status: "it's", Score: 5, Note: note("x_y")},
	}
	if err := rows.BulkCreate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	all, err := rows.OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cond := range []Cond{
		Q{"posts": 3}, Q{"note": nil}, Q{"status__iexact": "OPEN"},
		Q{"posts__gt": 0}, Q{"posts__gte": 3}, Q{"posts__lt": 5}, Q{"posts__lte": 3}, Q{"score__gt": 1.5},
		Q{"posts__gt": 2.5}, Q{"open": true}, Q{"title__gt": "beta"}, Q{"title__lte": F("status")},
		Q{"close_date__gte": F("open_date")}, Q{"close_date__lt": F("open_date")},
		Q{"status__in": []string{"draft", "open"}}, Q{"status__in": []string{}}, Q{"posts__in": []int{0, 5}},
		Q{"note__isnull": true}, Q{"note__isnull": false}, Q{"posts__range": []int{0, 5}},
		Q{"note__contains": "x"}, Q{"note__icontains": "X"}, Q{"title__startswith": "al"}, Q{"title__istartswith": "AL"},
		Q{"title__endswith": "_1"}, Q{"title__iendswith": "A_1"}, Q{"title__contains": "%"}, Q{"note__startswith": "x_"},
		Not(Q{"note__startswith": "b"}), Not(Q{"note": "box"}), Or(Q{"posts__gt": 4}, Q{"note__isnull": true}),
		And(Q{"open": true}, Not(Q{"posts__lt": 4})), Or(Not(Q{"status": "open"}), Q{"note__isnull": false}),
		Where(F("posts"), "gte", 3),
	} {
		r, err := rules(t, CheckOf(cond).Name("c"))
		if err != nil {
			t.Fatal(err)
		}
		sqlFails := idsWhere(t, ctx, "NOT ("+r[0].sql.SQLite+")")
		var goFails []int64
		m := rows.c.meta
		for i := range all {
			ev := &rowEval{m: m, d: sqlite{}, text: textRulesOf(sqlite{}), row: reflect.ValueOf(&all[i]).Elem()}
			if r[0].test.test(ev) == no {
				goFails = append(goFails, all[i].ID)
			}
		}
		if !slices.Equal(goFails, sqlFails) {
			t.Errorf("%s: Go refuses %v, SQLite %v", r[0].sql.SQLite, goFails, sqlFails)
		}
	}
}

// A transform or a function is the database's to answer: Go refuses no
// row by it, and SQLite does.
func TestCheckLeftToDatabase(t *testing.T) {
	isolate(t)
	rows := For[ckRow](Table("ck_rows"))
	ctx := WithDB(context.Background(), memDB(t))
	if err := CreateTables(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := rows.BulkCreate(ctx, []*ckRow{{Title: "Beta"}, {Title: "gamma"}}); err != nil {
		t.Fatal(err)
	}
	all, err := rows.OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cond := range []Cond{Q{"title__lower": "beta"}, Or(Q{"title__lower": "beta"}, Q{"posts__gt": 100}), Where(Lower.Of(F("title")), "exact", "beta")} {
		r, err := rules(t, CheckOf(cond).Name("c"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range all {
			ev := &rowEval{m: rows.c.meta, d: sqlite{}, text: textRulesOf(sqlite{}), row: reflect.ValueOf(&all[i]).Elem()}
			if got := r[0].test.test(ev); got != unknown {
				t.Errorf("%s of %q: %v, want it left to the database", r[0].sql.SQLite, all[i].Title, got)
			}
		}
		if got := idsWhere(t, ctx, "NOT ("+r[0].sql.SQLite+")"); !slices.Equal(got, []int64{2}) {
			t.Errorf("%s: SQLite refuses %v", r[0].sql.SQLite, got)
		}
	}
}

// idsWhere is the ids of ck_rows where cond, SQL, holds.
func idsWhere(t *testing.T, ctx context.Context, cond string) []int64 {
	t.Helper()
	d, _ := DBFrom(ctx)
	rs, err := d.SQL().QueryContext(ctx, `SELECT "id" FROM "ck_rows" WHERE `+cond+` ORDER BY "id"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// TestCheckErrors reads each database's refusal by a check: the
// constraint it names.
func TestCheckErrors(t *testing.T) {
	for _, c := range []struct {
		d    Dialect
		err  error
		want string
	}{
		{mysql{}, errors.New("Error 3819 (HY000): Check constraint 'vacancies_posts_check' is violated."), "vacancies_posts_check"},
		{sqlite{}, errors.New("constraint failed: CHECK constraint failed: vacancies_posts_check (275)"), "vacancies_posts_check"},
		{postgres{}, errors.New(`ERROR: new row for relation "vacancies" violates check constraint "vacancies_posts_check" (SQLSTATE 23514)`), "vacancies_posts_check"},
		{postgres{}, pgErr{"23514", "vacancies_posts_check"}, "vacancies_posts_check"},
	} {
		kind, name := c.d.Violation(c.err)
		if kind != checkViolation || name != c.want {
			t.Errorf("%s: %v %q, want the check %s", c.d.Name(), kind, name, c.want)
		}
	}
	if kind, _ := (postgres{}).Violation(pgErr{"22P02", ""}); kind != noViolation {
		t.Errorf("another SQLSTATE read as %v", kind)
	}
}

// pgErr is a driver's error as pgx's has its code and constraint.
type pgErr struct{ Code, ConstraintName string }

func (e pgErr) Error() string    { return "ERROR: a localized message (SQLSTATE " + e.Code + ")" }
func (e pgErr) SQLState() string { return e.Code }
