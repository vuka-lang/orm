package orm_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
)

var (
	// Prefix is a function of the app's own: SQL of its own on every
	// database.
	Prefix = orm.Function("prefix", orm.Template("", "substr({0}, 1, {1})"))
	// Initial is one used as a Q key step: Q{"name__initial": "A"}.
	Initial = orm.Function("initial", orm.Template("", "substr({0}, 1, 1)")).Transform()
	// GroupConcat is an aggregate SQLite and MySQL have.
	GroupConcat = orm.Function("group_concat", orm.Template("postgres", "string_agg({0}, {1})"), orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')"))
)

type UserStats struct {
	ID      int64
	Name    string
	NameLen int    `orm:"computed"`
	Short   string `orm:"computed"`
}

var Stats = orm.For[UserStats](orm.Table("users"))

func TestFunctions(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)

	got, err := Users.Filter(orm.Where(Prefix.Of(orm.F("name"), 2), "exact", "Am")).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Amina"}) {
		t.Fatalf("custom function = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__initial": "A"}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Ali", "Amina"}) {
		t.Fatalf("custom transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__lower__startswith": "ne"}).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Neema"}) {
		t.Fatalf("lower transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"created_at__year": time.Now().Year()}).All(ctx)
	if err != nil || len(got) != 4 {
		t.Fatalf("year transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__length__gte": 5}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Neema", "Amina"}) {
		t.Fatalf("length transform = %v, %v", names(got), err)
	}

	long, err := Users.Annotate("name_len", orm.Length.Of(orm.F("name"))).
		Filter(orm.Q{"name_len__gte": 4}).OrderBy("-name_len", "name").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(long, []string{"Amina", "Neema", "Juma"}) {
		t.Fatalf("annotate = %v, %v", long, err)
	}
	type nameLen struct {
		Name    string
		NameLen int
	}
	pairs, err := Users.Annotate("name_len", orm.Length.Of(orm.F("name"))).OrderBy("id").Values[nameLen]("name", "name_len").All(ctx)
	if err != nil || len(pairs) != 4 || pairs[0] != (nameLen{"Ali", 3}) {
		t.Fatalf("annotation in Values = %v, %v", pairs, err)
	}

	stats, err := Stats.Annotate("name_len", orm.Length.Of(orm.F("name"))).
		Annotate("short", Prefix.Of(orm.F("name"), 2)).OrderBy("id").All(ctx)
	if err != nil || len(stats) != 4 || stats[1].NameLen != 5 || stats[1].Short != "Ne" {
		t.Fatalf("computed fields = %+v, %v", stats, err)
	}
	if plain, _ := Stats.OrderBy("id").First(ctx); plain.NameLen != 0 {
		t.Fatalf("computed field filled with no annotation: %+v", plain)
	}

	if n, err := Users.Filter(orm.Q{"age__lt": 30}).Update(ctx, orm.Set{"age": orm.SQL("{0} + {1}", orm.F("age"), 10)}); n != 2 || err != nil {
		t.Fatalf("update with an expression = %d, %v", n, err)
	}
	ali, _ := Users.Get(ctx, orm.Q{"name": "Ali"})
	if ali.Age != 35 {
		t.Fatalf("age = %d", ali.Age)
	}
	got, err = Users.Filter(orm.Q{"age__gt": orm.F("id")}).All(ctx)
	if err != nil || len(got) != 4 {
		t.Fatalf("column against column = %v, %v", names(got), err)
	}

	r, err := Users.Filter(orm.Q{"active": true}).Aggregate(ctx, orm.AggOf("names", GroupConcat.Of(orm.F("name"), ", ")), orm.Count("id"))
	if names, _ := r["names"].(string); err != nil || (names != "Ali, Juma" && names != "Juma, Ali") || r.Int("id__count") != 2 {
		t.Fatalf("custom aggregate = %v, %v", r, err)
	}
}

type UserTier struct {
	ID     int64
	Name   string
	Age    int
	Active bool
	Tier   string `orm:"computed"`
}

var UserTiers = orm.For[UserTier](orm.Table("users"))

// tier is a Case of several branches, each a condition of its own.
var tier = orm.Case(
	orm.When(orm.Q{"age__gte": 40}, "gold"),
	orm.When(orm.And(orm.Q{"active": true}, orm.Q{"age__gte": 30}), "silver"),
	orm.When(orm.Q{"active": true}, "bronze"),
).Else("none")

func TestConditionalExpressions(t *testing.T) {
	ctx := open(t)
	seed(t, ctx) // Ali 25 active, Neema 17, Juma 32 active, Amina 41

	got, err := Users.Annotate("tier", tier).OrderBy("id").Values[map[string]any]("name", "tier").All(ctx)
	want := []map[string]any{{"name": "Ali", "tier": "bronze"}, {"name": "Neema", "tier": "none"}, {"name": "Juma", "tier": "silver"}, {"name": "Amina", "tier": "gold"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("case = %v, %v", got, err)
	}
	minor, err := Users.Annotate("minor", orm.Case(orm.When(orm.Q{"age__lt": 18}, "yes"))).OrderBy("id").Values[[]any]("name", "minor").All(ctx)
	if err != nil || !reflect.DeepEqual(minor, [][]any{{"Ali", nil}, {"Neema", "yes"}, {"Juma", nil}, {"Amina", nil}}) {
		t.Fatalf("no Else is NULL = %v, %v", minor, err)
	}
	score := orm.Case(orm.When(orm.Q{"active": true}, orm.SQL("{0} * {1}", orm.F("age"), 2))).
		Else(orm.Case(orm.When(orm.Q{"age__lt": 18}, 0)).Else(orm.F("age")))
	scores, err := Users.Annotate("score", score).OrderBy("id").Values[int64]("score").All(ctx)
	if err != nil || !slices.Equal(scores, []int64{50, 0, 64, 41}) {
		t.Fatalf("nested, arithmetic = %v, %v", scores, err)
	}
	type tierRow struct {
		Name string
		Tier string
	}
	rows, err := Users.Annotate("tier", tier).Filter(orm.Q{"tier__in": []string{"gold", "silver"}}).OrderBy("-tier").Values[tierRow]("name", "tier").All(ctx)
	if err != nil || !slices.Equal(rows, []tierRow{{"Juma", "silver"}, {"Amina", "gold"}}) {
		t.Fatalf("filtered and ordered by the annotation = %v, %v", rows, err)
	}
	kept, err := Users.Annotate("tier", tier).Exclude(orm.Q{"tier": "none"}).OrderBy("id").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(kept, []string{"Ali", "Juma", "Amina"}) {
		t.Fatalf("excluded by the annotation = %v, %v", kept, err)
	}
	tiers, err := UserTiers.Annotate("tier", tier).OrderBy("id").All(ctx)
	if err != nil || len(tiers) != 4 || tiers[0].Tier != "bronze" || tiers[3].Tier != "gold" {
		t.Fatalf("computed field = %+v, %v", tiers, err)
	}

	points := orm.Switch(orm.F("name")).Case("Ali", 1).Case("Juma", orm.F("age")).Else(0)
	ps, err := Users.Annotate("points", points).OrderBy("-points", "id").Values[[]any]("name", "points").All(ctx)
	if err != nil || !reflect.DeepEqual(ps, [][]any{{"Juma", int64(32)}, {"Ali", int64(1)}, {"Neema", int64(0)}, {"Amina", int64(0)}}) {
		t.Fatalf("switch = %v, %v", ps, err)
	}
	if r, err := Users.Annotate("points", points).Aggregate(ctx, orm.Sum("points")); err != nil || r.Int("points__sum") != 33 {
		t.Fatalf("sum of a switch = %v, %v", r, err)
	}

	on := orm.Case(orm.When(orm.Q{"active": true}, 1)).Else(0)
	if r, err := Users.Annotate("on", on).Filter(orm.Q{"on__gte": 0}).Aggregate(ctx, orm.Sum("on")); err != nil || r.Int("on__sum") != 2 {
		t.Fatalf("sum of a case of values = %v, %v", r, err)
	}

	r, err := Users.Aggregate(ctx, orm.Count("id").Filter(orm.Q{"active": true}).As("active"),
		orm.Sum("age").Filter(orm.Q{"age__gte": 18}), orm.Avg("age").Filter(orm.Q{"name": "nobody"}), orm.Count("id"))
	if err != nil || r.Int("active") != 2 || r.Int("age__sum") != 98 || r["age__avg"] != nil || r.Int("id__count") != 4 {
		t.Fatalf("conditional aggregates = %v, %v", r, err)
	}
	grouped, err := Users.Annotate("adults", orm.Count("id").Filter(orm.Q{"age__gte": 18})).
		Annotate("n", orm.Count("id")).OrderBy("active").Values[[]any]("active", "adults", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(grouped, [][]any{{false, int64(1), int64(2)}, {true, int64(2), int64(2)}}) {
		t.Fatalf("conditional count per group = %v, %v", grouped, err)
	}

	n, err := Users.Filter(orm.Q{"age__gt": 0}).Update(ctx, orm.Set{
		"age": orm.Case(orm.When(orm.Q{"active": true}, orm.SQL("{0} + {1}", orm.F("age"), 1))).Else(orm.F("age")),
		"bio": orm.Case(orm.When(orm.Q{"age__lt": 18}, "minor"), orm.When(orm.Q{"age__gt": 40}, "senior")),
	})
	if err != nil || n != 4 {
		t.Fatalf("conditional update = %d, %v", n, err)
	}
	after, _ := Users.OrderBy("id").All(ctx)
	bios := []string{}
	for _, u := range after {
		bio := "-"
		if u.Bio != nil {
			bio = *u.Bio
		}
		bios = append(bios, fmt.Sprintf("%d %s", u.Age, bio))
	}
	if !slices.Equal(bios, []string{"26 -", "17 minor", "33 -", "41 senior"}) {
		t.Fatalf("updated %v", bios)
	}
	if _, err := Users.Filter(orm.Q{"name": "Neema"}).Update(ctx, orm.Set{"age": orm.Case(orm.When(orm.Q{"age__lt": 18}, 18)).Else(0)}); err != nil {
		t.Fatalf("an update of values into a number column: %v", err)
	}
	if neema, _ := Users.Get(ctx, orm.Q{"name": "Neema"}); neema.Age != 18 {
		t.Fatalf("age = %d", neema.Age)
	}

	if _, err := Users.Annotate("x", orm.Case()).Values[string]("x").All(ctx); err == nil || err.Error() != "orm: a Case with no When" {
		t.Fatalf("an empty Case: %v", err)
	}
	if _, err := Users.Aggregate(ctx, orm.AggOf("n", orm.SQL("COUNT(*)")).Filter(orm.Q{"active": true})); err == nil {
		t.Fatal("AggOf took a Filter")
	}
}

func TestConditionsThroughRelations(t *testing.T) {
	ctx := relOpen(t) // Ali: a poet's profile, 2 books, 1 published; Neema: 1 published; Juma: none
	kind := orm.Case(
		orm.When(orm.Q{"profile__bio__icontains": "poet"}, "poet"),
		orm.When(orm.Q{"books__published": true}, "published"),
		orm.When(orm.Where(orm.Lower.Of(orm.F("name")), "startswith", "j"), "j"),
	).Else("new")
	got, err := Authors.Annotate("kind", kind).OrderBy("id").Values[[]any]("name", "kind").All(ctx)
	if err != nil || !reflect.DeepEqual(got, [][]any{{"Ali", "poet"}, {"Neema", "published"}, {"Juma", "j"}}) {
		t.Fatalf("conditions through relations = %v, %v", got, err)
	}
	r, err := Authors.Aggregate(ctx, orm.Count("id").Filter(orm.Or(orm.Q{"books__title__icontains": "go"}, orm.Not(orm.Q{"profile__bio__isnull": true}))).As("n"))
	if err != nil || r.Int("n") != 2 {
		t.Fatalf("a filter through relations = %v, %v", r, err)
	}
	n, err := Books.Filter(orm.Q{"id__gt": 0}).Update(ctx, orm.Set{
		"title": orm.Case(orm.When(orm.Q{"tags__name": "poetry"}, orm.Upper.Of(orm.F("title")))).Else(orm.F("title")),
	})
	if err != nil || n == 0 {
		t.Fatalf("update with a condition across many rows = %d, %v", n, err)
	}
	ts, _ := Books.OrderBy("id").Values[string]("title").All(ctx)
	if !slices.Equal(ts, []string{"Go in Practice", "POEMS", "Learning Go"}) {
		t.Fatalf("updated %v", ts)
	}
}
