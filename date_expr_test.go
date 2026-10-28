package orm

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPortableDates(t *testing.T) {
	for _, c := range []struct {
		e                       DateExpr
		mysql, postgres, sqlite string
	}{
		{Today(), "CURDATE()", "CURRENT_DATE", "date('now')"},
		{Now(), "NOW()", "now()", "datetime('now')"},
		{Today().AddDays(-3), "DATE_SUB(CURDATE(), INTERVAL 3 DAY)", "(CURRENT_DATE + -3)", "date(date('now'), '-3 day')"},
		{Today().AddMonths(2), "DATE_ADD(CURDATE(), INTERVAL 2 MONTH)", "(CURRENT_DATE + INTERVAL '2 month')", "date(date('now'), '+2 month')"},
		{Now().Add(1500 * time.Millisecond), "DATE_ADD(NOW(), INTERVAL 1.5 SECOND)", "(now() + INTERVAL '1.5 second')", "datetime(datetime('now'), '+1.5 second')"},
		{Date(FieldAt[time.Time]("created")), "DATE(`users`.`created`)", `("users"."created")::date`, `DATE("users"."created")`},
	} {
		for _, d := range []Dialect{mysql{}, postgres{}, sqlite{}} {
			m := For[dateUser](Table("users"))
			b := m.Filter().q.builder(d)
			got, err := c.e.exprSQL(b)
			want := map[string]string{"mysql": c.mysql, "postgres": c.postgres, "sqlite": c.sqlite}[d.Name()]
			if err != nil || got != want {
				t.Errorf("%s: %s, %v; want %s", d.Name(), got, err, want)
			}
		}
	}
	loc := time.FixedZone("local", 14*3600)
	e := TodayIn(loc)
	b := For[dateUser]().Filter().q.builder(sqlite{})
	_, err := e.exprSQL(b)
	if err != nil || !reflect.DeepEqual(b.args(), []any{time.Now().In(loc).Format("2006-01-02")}) {
		t.Fatalf("TodayIn = %v, %v", b.args(), err)
	}
	b = For[dateUser]().Filter().q.builder(sqlite{})
	s, err := TimeFieldAt("created").Between(Today().AddDays(-3), Now()).sql(b)
	if err != nil || !strings.Contains(s, " BETWEEN ") || len(b.args()) != 0 {
		t.Fatalf("Between = %s, %v", s, err)
	}
}

type dateUser struct {
	ID      int64
	Created time.Time
}
