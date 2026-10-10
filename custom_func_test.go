package orm_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/migration"
	"github.com/vuka-lang/orm/ormtest"
	"github.com/vuka-lang/orm/sqlite"
)

func init() {
	sqlite.MustFunc("go_editdist", levenshtein)
	sqlite.MustFunc("go_initial", func(s *string) *string {
		if s == nil || *s == "" {
			return nil
		}
		r := strings.ToUpper((*s)[:1])
		return &r
	})
	sqlite.MustFunc("go_sum", func(xs ...int64) (s int64) {
		for _, x := range xs {
			s += x
		}
		return s
	})
	sqlite.MustFunc("go_fail", func(string) (string, error) { return "", errors.New("go_fail always fails") })
	orm.RegisterLookup("ci_has", func(lhs string, rhs func() string, d orm.Dialect) (string, bool) {
		if d.Name() == "postgres" {
			return lhs + " ILIKE " + rhs() + " ESCAPE '!'", true
		}
		return "LOWER(" + lhs + ") LIKE LOWER(" + rhs() + ") ESCAPE '!'", true
	}, orm.LookupValue(orm.LikePattern("%", "%")))
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

var (
	// EditDist is a Go function on SQLite, fuzzystrmatch's on Postgres
	// and a stored function on MySQL (levenshteinFunc).
	EditDist = orm.OrderedFunc[int]("editdist", orm.Arity(2),
		orm.Template("sqlite", "go_editdist({0}, {1})"), orm.Template("postgres", "levenshtein({0}, {1})"),
		orm.Template("mysql", "levenshtein({0}, {1})"))
	// Initial is a Go function on SQLite and an SQL one on Postgres and
	// MySQL (initialFunc), a transform too.
	Initial2  = orm.TextFunc("go_initial", orm.Arity(1)).Transform()
	GoSum     = orm.OrderedFunc[int64]("go_sum")
	GoFail    = orm.TextFunc("go_fail")
	StringAgg = orm.TextFunc("string_agg", orm.IsAggregate(),
		orm.Template("postgres", "string_agg({0}, {1})"),
		orm.Template("sqlite", "group_concat({0}, {1})"),
		orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')"))
	RowNumber = orm.OrderedFunc[int64]("row_number", orm.Arity(0))
	MySQLOnly = orm.Func[int]("mysql_only", orm.Template("mysql", "1"))
	PGOnly    = orm.Func[int]("pg_only", orm.Template("postgres", "1"))
)

var initialFunc = migration.CreateFunction{
	Name:    "go_initial",
	Params:  migration.Dialects{Postgres: "s text", MySQL: "s VARCHAR(255)"},
	Returns: migration.Dialects{Postgres: "text", MySQL: "VARCHAR(255)"},
	Body:    migration.Dialects{Postgres: "SELECT upper(left(NULLIF(s, ''), 1))", MySQL: "RETURN UPPER(LEFT(NULLIF(s, ''), 1))"},
}

// levenshteinFunc is MySQL's edit distance, a compound body: its
// statements' semicolons must reach the server as one CREATE FUNCTION.
var levenshteinFunc = migration.CreateFunction{
	Name:    "levenshtein",
	Params:  migration.Dialects{MySQL: "a VARCHAR(255), b VARCHAR(255)"},
	Returns: migration.Dialects{MySQL: "INT"},
	Body: migration.Dialects{MySQL: `BEGIN
  DECLARE i, j INT DEFAULT 1;
  DECLARE prev, cur JSON;
  IF a IS NULL OR b IS NULL THEN RETURN NULL; END IF;
  SET prev = JSON_ARRAY(0);
  WHILE j <= CHAR_LENGTH(b) DO SET prev = JSON_ARRAY_APPEND(prev, '$', j); SET j = j + 1; END WHILE;
  WHILE i <= CHAR_LENGTH(a) DO
    SET cur = JSON_ARRAY(i), j = 1;
    WHILE j <= CHAR_LENGTH(b) DO
      SET cur = JSON_ARRAY_APPEND(cur, '$', LEAST(
        JSON_EXTRACT(prev, CONCAT('$[', j, ']')) + 1,
        JSON_EXTRACT(cur, CONCAT('$[', j - 1, ']')) + 1,
        JSON_EXTRACT(prev, CONCAT('$[', j - 1, ']')) + (BINARY SUBSTRING(a, i, 1) <> BINARY SUBSTRING(b, j, 1))));
      SET j = j + 1;
    END WHILE;
    SET prev = cur, i = i + 1;
  END WHILE;
  RETURN JSON_EXTRACT(prev, CONCAT('$[', CHAR_LENGTH(b), ']'));
END`},
}

var fns = migration.Migration{Name: "f", Operations: []migration.Operation{initialFunc, levenshteinFunc}}

// run runs a migration's steps on ctx's database.
func run(t *testing.T, ctx context.Context, steps []migration.Step) {
	t.Helper()
	d, _ := orm.DBFrom(ctx)
	for _, st := range steps {
		var err error
		if st.Go != nil {
			err = st.Go(ctx, d.SQL())
		} else {
			_, err = orm.Exec(ctx, st.SQL)
		}
		if err != nil {
			t.Fatalf("%s: %v", st.SQL, err)
		}
	}
}

func TestCustomFunctions(t *testing.T) {
	ctx := open(t)
	seed(t, ctx) // Ali 25 active (bio), Neema 17, Juma 32 active, Amina 41
	driver := ormtest.Driver()
	if driver == "postgres" {
		if _, err := orm.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS fuzzystrmatch WITH SCHEMA public"); err != nil {
			t.Skipf("fuzzystrmatch: %v", err)
		}
	}
	if driver != "sqlite" {
		// The functions made as a migration makes them: a compound body
		// is a Go step running it whole.
		steps, err := fns.Forwards(driver, migration.NewState(nil, nil))
		if err != nil || len(steps) != map[string]int{"postgres": 1, "mysql": 2}[driver] {
			t.Fatalf("CreateFunction = %v, %v", steps, err)
		}
		run(t, ctx, steps)
		defer func() {
			// Unapplied, the functions are gone.
			back, err := fns.Backwards(driver, migration.NewState(nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			run(t, ctx, back)
			if _, err := Users.Annotate("i", Initial2(orm.F("bio"))).Values[*string]("i").All(ctx); err == nil {
				t.Error("go_initial outlived its migration")
			}
		}()
	}

	// Filter, Annotate (AnnotateAs), Values and Order with a Go function.
	got, err := Users.Filter(EditDist(orm.F("name"), "Amna").Le(1)).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Amina"}) {
		t.Fatalf("filter = %v, %v", names(got), err)
	}
	got, err = Users.Order(EditDist(orm.F("name"), "Jumaa").Asc(), orm.Ordering{Path: []string{"name"}}).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Juma", "Amina", "Neema", "Ali"}) {
		t.Fatalf("order = %v, %v", names(got), err)
	}
	type dist struct {
		Name string
		D    int
	}
	ds, err := Users.AnnotateAs(EditDist(orm.F("name"), "Ali").As("d")).Filter(orm.Q{"d__lt": 4}).OrderBy("d", "name").Values[dist]("name", "d").All(ctx)
	if err != nil || !slices.Equal(ds, []dist{{"Ali", 0}, {"Amina", 3}}) {
		t.Fatalf("annotate = %v, %v", ds, err)
	}

	// NULL in, NULL out; a transform; a Case branch on a typed condition.
	inits, err := Users.Annotate("i", Initial2(orm.F("bio"))).OrderBy("id").Values[*string]("i").All(ctx)
	if err != nil || len(inits) != 4 || inits[0] == nil || *inits[0] != "W" || inits[1] != nil {
		t.Fatalf("nullable = %v, %v", inits, err)
	}
	got, err = Users.Filter(orm.Q{"bio__go_initial": "W"}).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Ali"}) {
		t.Fatalf("transform = %v, %v", names(got), err)
	}
	kinds, err := Users.Annotate("k", orm.Case(orm.When(EditDist(orm.F("name"), "Ali").Eq(0), "me")).Else("other")).
		OrderBy("id").Values[string]("k").All(ctx)
	if err != nil || !slices.Equal(kinds, []string{"me", "other", "other", "other"}) {
		t.Fatalf("case = %v, %v", kinds, err)
	}

	// Typed built-ins, a custom lookup through a transform, Exclude.
	got, err = Users.Filter(orm.Lower(orm.F("name")).StartsWith("a"), orm.Length(orm.F("name")).Between(4, 6)).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Amina"}) {
		t.Fatalf("typed built-ins = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__upper__ci_has": "M"}).Exclude(orm.Year(orm.F("created_at")).Lt(2000)).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Neema", "Juma", "Amina"}) {
		t.Fatalf("custom lookup = %v, %v", names(got), err)
	}

	// Update writes an expression of the row.
	if n, err := Users.Filter(orm.Text(orm.F("name")).In("Ali", "Juma")).Update(ctx, orm.Set{"name": orm.Upper(orm.F("name"))}); n != 2 || err != nil {
		t.Fatalf("update = %d, %v", n, err)
	}
	after, err := Users.OrderBy("id").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(after, []string{"ALI", "Neema", "JUMA", "Amina"}) {
		t.Fatalf("after update = %v, %v", after, err)
	}
	if driver != "mysql" { // whose = ignores case under its default collation
		got, err = Users.Filter(orm.Where(orm.Upper(orm.F("name")), "exact", orm.F("name"))).OrderBy("id").All(ctx)
		if err != nil || !slices.Equal(names(got), []string{"ALI", "JUMA"}) {
			t.Fatalf("expression = expression: %v, %v", names(got), err)
		}
	}

	// Aggregates: grouping, HAVING, Aggregate.
	type group struct {
		Active bool
		Names  string
	}
	gs, err := Users.Annotate("names", StringAgg(orm.F("name"), ",")).Having(StringAgg(orm.F("name"), ",").Contains("N")).
		Values[group]("active", "names").All(ctx)
	if err != nil || len(gs) != 1 || gs[0].Active || len(strings.Split(gs[0].Names, ",")) != 2 {
		t.Fatalf("grouped = %v, %v", gs, err)
	}
	r, err := Users.Filter(orm.Q{"active": true}).Aggregate(ctx, orm.AggOf("n", StringAgg(orm.F("name"), ",")))
	// MySQL's SEPARATOR is a literal: its template's is ", ".
	if s, _ := r["n"].(string); err != nil || !slices.Contains([]string{"ALI,JUMA", "JUMA,ALI"}, strings.ReplaceAll(s, " ", "")) {
		t.Fatalf("aggregate = %v, %v", r, err)
	}

	// A window function.
	type ranked struct {
		Name string
		Rn   int64
	}
	rs, err := Users.AnnotateAs(RowNumber().Over(orm.Window{PartitionBy: []string{"active"}, OrderBy: []string{"-age"}}).As("rn")).
		OrderBy("id").Values[ranked]("name", "rn").All(ctx)
	if err != nil || !reflect.DeepEqual(rs, []ranked{{"ALI", 2}, {"Neema", 2}, {"JUMA", 1}, {"Amina", 1}}) {
		t.Fatalf("window = %v, %v", rs, err)
	}

	// Values are bound, never written into the SQL.
	evil := "x'); DROP TABLE users; --"
	if got, err := Users.Filter(EditDist(orm.F("name"), evil).Lt(0)).All(ctx); err != nil || len(got) != 0 {
		t.Fatalf("injection = %v, %v", got, err)
	}
	if n, err := Users.Count(ctx); err != nil || n != 4 {
		t.Fatalf("after injection: %d, %v", n, err)
	}

	// No SQL for the dialect: the function and the dialect named.
	other, name := MySQLOnly, "mysql_only"
	if driver == "mysql" {
		other, name = PGOnly, "pg_only"
	}
	if _, err := Users.Filter(other().Eq(1)).All(ctx); err == nil || !strings.Contains(err.Error(), name+" has no SQL on "+driver) {
		t.Fatalf("no template: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"name__ci_hass": "x"}).All(ctx); err == nil || !strings.Contains(err.Error(), `did you mean "ci_has"?`) {
		t.Fatalf("unknown lookup: %v", err)
	}

	if driver != "sqlite" {
		return
	}
	// Go functions only: variadic, failing.
	sums, err := Users.Annotate("s", GoSum(orm.F("age"), orm.F("id"), 1)).OrderBy("id").Values[int64]("s").All(ctx)
	if err != nil || !slices.Equal(sums, []int64{27, 20, 36, 46}) {
		t.Fatalf("variadic = %v, %v", sums, err)
	}
	if _, err := Users.Filter(GoFail(orm.F("name")).Eq("")).All(ctx); err == nil || !strings.Contains(err.Error(), "go_fail always fails") {
		t.Fatalf("a failing Go function: %v", err)
	}
}

func TestSQLiteFuncErrors(t *testing.T) {
	for _, c := range []struct {
		fn   any
		want string
	}{
		{42, "want a func"},
		{func() {}, "must return a value"},
		{func(chan int) int { return 0 }, "isn't an SQL value"},
		{func(string) (int, int) { return 0, 0 }, "must return a value"},
	} {
		if err := sqlite.Func("go_bad", c.fn); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Func(%T) = %v, want %q", c.fn, err, c.want)
		}
	}
	if err := sqlite.Func("go_editdist", levenshtein); err == nil {
		t.Error("registering a name twice")
	}
}
