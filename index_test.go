package orm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	m "github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
)

type Subscriber struct {
	ID        int64
	Email     string
	Name      string
	Active    bool
	CreatedAt time.Time
}

func (Subscriber) Indexes() []orm.Index {
	return []orm.Index{
		orm.IndexOf(orm.Lower.Of(orm.F("email"))).Unique().Name("subscribers_email_lower"),
		orm.IndexOf("name").Where(orm.Q{"active": true}).Else(orm.IndexOf("name")),
		orm.IndexOf(orm.Key("created_at").Desc()),
	}
}

var Subscribers = orm.For[Subscriber]()

// TestDeclaredIndexes makes a model's indexes on the test's database: a
// unique index of LOWER(email) refuses an address differing only in
// case, and serves email__iexact.
func TestDeclaredIndexes(t *testing.T) {
	ctx := ormtest.Open(t, Subscribers)
	if err := Subscribers.Create(ctx, &Subscriber{Email: "Ali@mail.com", Name: "Ali", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := Subscribers.Create(ctx, &Subscriber{Email: "ali@MAIL.com", Name: "Ali"}); !errors.Is(err, orm.ErrConflict) {
		t.Fatalf("a second address differing in case: %v", err)
	}
	plan, err := Subscribers.Filter(orm.Q{"email__iexact": "ALI@mail.com"}).Explain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s plan:\n%s", plan.Database, plan)
	if plan.Database != "mysql" && !strings.Contains(plan.Text, "subscribers_email_lower") {
		t.Fatalf("email__iexact doesn't use the LOWER(email) index on %s:\n%s", plan.Database, plan)
	}
}

// A concurrent index is made outside the migration's transaction on
// Postgres (where CREATE INDEX CONCURRENTLY can't run inside one), and as
// any other elsewhere; unapplying drops it the same way.
func TestConcurrentIndex(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	idx := m.Index{Name: "notes_title_idx", SQL: m.Dialects{
		Postgres: `CREATE INDEX "notes_title_idx" ON "notes" ("title")`,
		MySQL:    "CREATE INDEX `notes_title_idx` ON `notes` (`title`)",
		SQLite:   `CREATE INDEX "notes_title_idx" ON "notes" ("title")`,
	}}
	migrations := []m.Migration{
		{Name: "0001_initial", Operations: []m.Operation{m.CreateModel{Table: "notes", Fields: []m.NamedField{
			m.F("id", m.BigAuto()), m.F("title", m.Varchar(100)),
		}}}},
		{Name: "0002_title", Dependencies: []string{"0001_initial"}, Operations: []m.Operation{
			m.AddIndex{Table: "notes", Index: idx, Concurrently: true},
		}},
	}
	for _, target := range []string{"", "0001", ""} {
		if err := orm.ApplyMigrations(context.Background(), d, target, migrations...); err != nil {
			t.Fatalf("to %q: %v", target, err)
		}
	}
}
