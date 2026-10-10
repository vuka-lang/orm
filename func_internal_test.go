package orm

import (
	"reflect"
	"strings"
	"testing"
)

var (
	tLev = OrderedFunc[int]("lev",
		Template("postgres", "levenshtein({0}, {1})"),
		Template("sqlite", "editdist({0}, {1})"))
	tStrAgg = TextFunc("str_agg", IsAggregate(),
		Template("postgres", "string_agg({0}, {1})"),
		Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')"),
		Template("sqlite", "group_concat({0}, {1})"))
	tRowNumber = OrderedFunc[int64]("row_number", Arity(0))
	tSoundex   = TextFunc("tsoundex", Arity(1), Template("", "soundex({0})")).Transform()
)

func init() {
	RegisterLookup("tunaccent_icontains", func(lhs string, rhs func() string, d Dialect) (string, bool) {
		switch d.Name() {
		case "postgres":
			return "unaccent(" + lhs + ") ILIKE unaccent(" + rhs() + ") ESCAPE '!'", true
		case "sqlite":
			return "LOWER(" + lhs + ") LIKE LOWER(" + rhs() + ") ESCAPE '!'", true
		}
		return "", false
	}, LookupValue(LikePattern("%", "%")))
	RegisterLookup("tnear", func(lhs string, rhs func() string, _ Dialect) (string, bool) {
		return lhs + " BETWEEN " + rhs() + " - 1 AND " + rhs() + " + 1", true
	})
}

// renderSQL is q's SELECT on d and its arguments.
func renderSQL(q QuerySet[sqlUser], d Dialect) (string, []any, error) {
	b := q.q.ordered().builder(d)
	cols, err := q.q.columns(b, nil)
	if err != nil {
		return "", nil, err
	}
	s, err := q.q.ordered().selectSQL(b, cols)
	return s, b.args(), err
}

func TestTypedFuncSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	cols := map[string]string{
		"postgres": `SELECT "users"."id", "users"."name", "users"."age" FROM "users"`,
		"mysql":    "SELECT `users`.`id`, `users`.`name`, `users`.`age` FROM `users`",
		"sqlite":   `SELECT "users"."id", "users"."name", "users"."age" FROM "users"`,
	}
	for _, c := range []struct {
		name string
		q    QuerySet[sqlUser]
		want map[string]string // dialect → after the FROM; an "error: …" prefix for an error
		args []any
	}{
		{"lev", m.Filter(tLev(F("name"), "jon").Le(1)).Order(tLev(F("name"), "jon").Asc(), Ordering{Path: []string{"age"}, Desc: true}), map[string]string{
			"postgres": ` WHERE levenshtein("users"."name", $1) <= $2 ORDER BY levenshtein("users"."name", $3) ASC, "users"."age" DESC`,
			"sqlite":   ` WHERE editdist("users"."name", ?) <= ? ORDER BY editdist("users"."name", ?) ASC, "users"."age" DESC`,
			"mysql":    "error: function lev has no SQL on mysql",
		}, []any{"jon", 1, "jon"}},
		{"text", m.Filter(Lower(F("name")).StartsWith("a_"), Length(F("name")).Between(3, 5), Upper(F("name")).In("ALI", "JUMA")), map[string]string{
			"postgres": ` WHERE (lower("users"."name") LIKE $1 ESCAPE '!' AND length("users"."name") BETWEEN $2 AND $3 AND upper("users"."name") IN ($4, $5))`,
			"mysql":    " WHERE (lower(`users`.`name`) LIKE ? ESCAPE '!' AND CHAR_LENGTH(`users`.`name`) BETWEEN ? AND ? AND upper(`users`.`name`) IN (?, ?))",
		}, []any{"a!_%", int64(3), int64(5), "ALI", "JUMA"}},
		{"null/ne/like", m.Filter(Coalesce(F("name"), "x").Ne("y"), Trim(F("name")).NotNull(), Text(F("name")).Like("A%i")), map[string]string{
			"postgres": ` WHERE (NOT (coalesce("users"."name", $1) = $2) AND trim("users"."name") IS NOT NULL AND "users"."name" LIKE $3)`,
			"mysql":    " WHERE (NOT (coalesce(`users`.`name`, ?) = ?) AND trim(`users`.`name`) IS NOT NULL AND `users`.`name` LIKE ?)",
		}, []any{"x", "y", "A%i"}},
		{"lookup", m.Filter(Q{"name__tunaccent_icontains": "caf_"}, Q{"name__tsoundex__tnear": 3}), map[string]string{
			"postgres": ` WHERE (unaccent("users"."name") ILIKE unaccent($1) ESCAPE '!' AND soundex("users"."name") BETWEEN $2 - 1 AND $3 + 1)`,
			"sqlite":   ` WHERE (LOWER("users"."name") LIKE LOWER(?) ESCAPE '!' AND soundex("users"."name") BETWEEN ? - 1 AND ? + 1)`,
			"mysql":    `error: lookup "tunaccent_icontains" has no SQL on mysql`,
		}, []any{"%caf!_%", 3, 3}},
		{"where lookup", m.Filter(Where(Lower(F("name")), "tnear", F("age"))), map[string]string{
			"postgres": ` WHERE lower("users"."name") BETWEEN "users"."age" - 1 AND "users"."age" + 1`,
		}, []any{}},
		{"arity", m.Filter(tRowNumber(1).Eq(1)), map[string]string{
			"sqlite": "error: row_number takes 0 argument(s), got 1",
		}, nil},
		{"unknown lookup", m.Filter(Q{"name__icontain": "x"}), map[string]string{
			"sqlite": `error: did you mean "icontains"?`,
		}, nil},
		{"unknown where lookup", m.Filter(Where(F("name"), "startwith", "x")), map[string]string{
			"sqlite": `error: unknown lookup "startwith" — did you mean "startswith"?`,
		}, nil},
	} {
		for d, want := range c.want {
			s, args, err := renderSQL(c.q, DialectFor(d))
			if msg, ok := strings.CutPrefix(want, "error: "); ok {
				if err == nil || !strings.Contains(err.Error(), msg) {
					t.Errorf("%s on %s: err = %v, want %q", c.name, d, err, msg)
				}
				continue
			}
			full := cols[d] + want
			if err != nil || s != full {
				t.Errorf("%s on %s:\n got %s\nwant %s (%v)", c.name, d, s, full, err)
				continue
			}
			if !reflect.DeepEqual(args, c.args) {
				t.Errorf("%s on %s: args = %#v, want %#v", c.name, d, args, c.args)
			}
		}
	}
}

func TestWindowSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	q := m.AnnotateAs(tRowNumber().Over(Window{PartitionBy: []string{"age"}, OrderBy: []string{"-name"}}).As("rn"))
	b := q.q.builder(postgres{})
	s, err := b.ref("rn")
	if want := `(row_number() OVER (PARTITION BY "users"."age" ORDER BY "users"."name" DESC))`; err != nil || s != want {
		t.Fatalf("window:\n got %s\nwant %s (%v)", s, want, err)
	}
	if b.aggExpr(F("rn")) {
		t.Fatal("a window function groups")
	}
}

func TestTypedAggregateSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	q := m.Annotate("names", tStrAgg(F("name"), ", ")).GroupBy("age").Having(tStrAgg(F("name"), ", ").Contains("A"))
	q.q.grouped, q.q.values = true, []string{"age", "names"}
	for d, want := range map[string]string{
		"postgres": `SELECT "users"."age", (string_agg("users"."name", $1)) FROM "users" GROUP BY 1 HAVING string_agg("users"."name", $2) LIKE $3 ESCAPE '!'`,
		"mysql":    "SELECT `users`.`age`, (GROUP_CONCAT(`users`.`name` SEPARATOR ', ')) FROM `users` GROUP BY 1 HAVING GROUP_CONCAT(`users`.`name` SEPARATOR ', ') LIKE ? ESCAPE '!'",
	} {
		b := q.q.builder(DialectFor(d))
		a, _ := b.ref("age")
		n, _ := b.ref("names")
		s, err := q.q.selectSQL(b, a+", "+n)
		if err != nil || s != want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", d, s, want, err)
		}
	}
	// A condition on an aggregate filters groups: Filter routes it to HAVING.
	b := q.q.builder(postgres{})
	if !b.aggregated(tStrAgg(F("name"), ", ").Eq("x")) || !b.aggregated(Text(F("names")).Contains("x")) || b.aggregated(Lower(F("name")).Eq("x")) {
		t.Fatal("aggregate conditions not told apart")
	}
	if isAggregate(tStrAgg(F("name"), ", ").Over(Window{})) {
		t.Fatal("an aggregate over a window still groups")
	}
}

func TestLookupRegistry(t *testing.T) {
	for _, name := range []string{"exact", "Bad", "two__parts", "x-y", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterLookup(%q) didn't panic", name)
				}
			}()
			RegisterLookup(name, func(string, func() string, Dialect) (string, bool) { return "", true })
		}()
	}
	if !isLookup("like") || !isLookup("tnear") || isLookup("nope") {
		t.Fatal("isLookup")
	}
	if got := didYouMean("tunaccent_icontain", lookupNames(false)); got != ` — did you mean "tunaccent_icontains"?` {
		t.Fatalf("didYouMean = %q", got)
	}
	if got := didYouMean("zzzzzz", lookupNames(true)); got != "" {
		t.Fatalf("didYouMean of nothing close = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("a two-argument transform didn't panic")
		}
	}()
	Func[int]("two", Arity(2)).Transform()
}

func TestInjection(t *testing.T) {
	m := For[sqlUser](Table("users"))
	evil := "x'); DROP TABLE users; --"
	s, args, err := renderSQL(m.Filter(tLev(F("name"), evil).Eq(0), Q{"name__tunaccent_icontains": evil}, Lower(F("name")).Like(evil)), postgres{})
	if err != nil || strings.Contains(s, "DROP") || len(args) != 4 || args[0] != evil || args[3] != evil {
		t.Fatalf("values not bound: %s %v %v", s, args, err)
	}
}
