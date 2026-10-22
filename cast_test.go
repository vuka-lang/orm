package orm_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
)

func TestCast(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)

	texts, err := Users.Annotate("age_text", orm.Cast(orm.F("age"), orm.AsText)).
		Filter(orm.Q{"age_text__startswith": "2"}).Values[string]("age_text").All(ctx)
	if err != nil || !slices.Equal(texts, []string{"25"}) {
		t.Fatalf("to text: %v, %v", texts, err)
	}
	n, err := Users.Filter(orm.Where(orm.Cast("30", orm.AsInt), "lt", orm.F("age"))).Count(ctx)
	if err != nil || n != 2 {
		t.Fatalf("a value to int: %d, %v", n, err)
	}
	halves, err := Users.OrderBy("id").Annotate("half", orm.SQL("{0} / 2", orm.Cast(orm.F("age"), orm.AsFloat))).Values[float64]("half").All(ctx)
	if err != nil || len(halves) != 4 || halves[0] != 12.5 {
		t.Fatalf("to float: %v, %v", halves, err)
	}
	dec, err := Users.Filter(orm.Q{"name": "Ali"}).Annotate("d", orm.Cast(orm.F("age"), orm.AsDecimal(10, 2))).Values[float64]("d").All(ctx)
	if err != nil || !slices.Equal(dec, []float64{25}) {
		t.Fatalf("to decimal: %v, %v", dec, err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	n, err = Users.Filter(orm.Where(orm.Cast(orm.F("created_at"), orm.AsDate), "lte", orm.Cast(today, orm.AsDate))).Count(ctx)
	if err != nil || n != 4 {
		t.Fatalf("to date: %d, %v", n, err)
	}
	if _, err := Users.Annotate("x", orm.Cast(orm.F("age"), orm.AsType("INT); DROP TABLE users; --"))).Values[string]("x").All(ctx); err == nil || !strings.Contains(err.Error(), "a type is a name") {
		t.Fatalf("a type that isn't one: %v", err)
	}
	if _, err := Users.Annotate("x", orm.Cast(orm.F("name"), orm.AsType("CHAR(20)"))).Values[string]("x").All(ctx); err != nil {
		t.Fatalf("a type of the database's own: %v", err)
	}
}
