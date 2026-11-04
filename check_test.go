package orm_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	m "github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
)

type Vacancy struct {
	ID         int64
	Title      string
	Posts      int
	Status     string
	OpenDate   time.Time
	CloseDate  time.Time
	EmployerID *int64
	PermitNo   string
	hooked     bool
}

func (Vacancy) Checks() []orm.RowCheck {
	return []orm.RowCheck{
		orm.CheckOf(orm.Q{"close_date__gte": orm.F("open_date")}).Field("close_date").Message("closes before it opens"),
		orm.CheckOf(orm.Q{"posts__gt": 0}).Field("posts").Message("needs at least one post"),
		orm.CheckOf(orm.Q{"status__in": []string{"draft", "open", "closed"}}),
		orm.CheckOf(orm.Q{"employer_id__isnull": false}).When(orm.Q{"status": "open"}).Message("an open vacancy needs an employer"),
	}
}

func (v *Vacancy) Validate(context.Context) error {
	if v.PermitNo == "expired" {
		return orm.Violated("permit_no", "the job permit has expired")
	}
	return nil
}

func (v *Vacancy) BeforeCreate(context.Context) error { v.hooked = true; return nil }

var Vacancies = orm.For[Vacancy]()

func vacancy() *Vacancy {
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return &Vacancy{Title: "Clerk", Posts: 2, Status: "draft", OpenDate: day, CloseDate: day.AddDate(0, 1, 0)}
}

// violations is the Violations err carries, by check name.
func violations(t *testing.T, err error) map[string]orm.Violation {
	t.Helper()
	var bad orm.Violations
	if !errors.As(err, &bad) || !errors.Is(err, orm.ErrInvalid) {
		t.Fatalf("not refused by checks: %v", err)
	}
	out := map[string]orm.Violation{}
	for _, v := range bad {
		out[v.Check] = v
	}
	return out
}

// TestRowChecks runs every write the checks guard: Create, Save and
// BulkCreate are refused by the ORM before they reach the database (every
// failing check at once, then Validate, then the hooks); an Update of a
// query and Exec by the database, which Update's error maps to the same
// Violation.
func TestRowChecks(t *testing.T) {
	ctx := ormtest.Open(t, Vacancies)
	ok := vacancy()
	if err := Vacancies.Create(ctx, ok); err != nil || !ok.hooked {
		t.Fatalf("a valid row: %v (hooked %v)", err, ok.hooked)
	}

	bad := vacancy()
	bad.Posts, bad.CloseDate, bad.PermitNo = 0, bad.OpenDate.AddDate(0, 0, -1), "expired"
	err := Vacancies.Create(ctx, bad)
	got := violations(t, err)
	if len(got) != 2 || got["vacancies_posts_check"] != (orm.Violation{Check: "vacancies_posts_check", Field: "posts", Message: "needs at least one post"}) ||
		got["vacancies_close_date_check"].Field != "close_date" || bad.hooked {
		t.Fatalf("two failing checks, before Validate and the hooks: %v (hooked %v)", got, bad.hooked)
	}
	var fields interface{ FieldErrors() map[string]string }
	if !errors.As(err, &fields) || !maps.Equal(fields.FieldErrors(), map[string]string{"posts": "needs at least one post", "close_date": "closes before it opens"}) {
		t.Fatalf("a form's field errors: %v", fields)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 422 {
		t.Fatalf("a route's status: %v", status)
	}
	if !strings.Contains(err.Error(), "Vacancy is invalid: closes before it opens; needs at least one post") {
		t.Fatalf("message: %v", err)
	}

	bad = vacancy()
	bad.PermitNo = "expired"
	if got := violations(t, Vacancies.Create(ctx, bad)); got[""].Field != "permit_no" {
		t.Fatalf("Validate's: %v", got)
	}
	bad = vacancy()
	bad.Status = "open"
	if got := violations(t, Vacancies.Create(ctx, bad)); got["vacancies_employer_id_check"].Message != "an open vacancy needs an employer" {
		t.Fatalf("a check When: %v", got)
	}
	if err := Vacancies.BulkCreate(ctx, []*Vacancy{vacancy(), {Title: "x", Posts: 1, Status: "gone"}}); err == nil {
		t.Fatal("a bulk create of a row breaking a check")
	} else if got := violations(t, err); got["vacancies_status_check"].Message != "status must be one of draft, open, closed" {
		t.Fatalf("a check's own message: %v", got)
	}
	ok.Posts = 0
	if got := violations(t, Vacancies.Save(ctx, ok)); len(got) != 1 {
		t.Fatalf("a save: %v", got)
	}
	if n, _ := Vacancies.Count(ctx); n != 1 {
		t.Fatalf("%d rows written, want the first alone", n)
	}

	_, err = Vacancies.Filter(orm.Q{"id": ok.ID}).Update(ctx, orm.Set{"posts": 0})
	if got := violations(t, err); got["vacancies_posts_check"].Message != "needs at least one post" || got["vacancies_posts_check"].Field != "posts" {
		t.Fatalf("an update the database refused: %v", got)
	}
	if _, err := orm.Exec(ctx, `UPDATE vacancies SET status = 'gone'`); err == nil {
		t.Fatal("Exec past the database's check")
	}
	if row, _ := Vacancies.Get(ctx, orm.Q{"id": ok.ID}); row.Posts != 2 || row.Status != "draft" {
		t.Fatalf("the row after the refused writes: %+v", row)
	}
}

// On Postgres a check added NotValid holds for rows written after it,
// and ValidateConstraint fails while older rows break it.
func TestNotValidOnPostgres(t *testing.T) {
	if ormtest.Driver() != "postgres" {
		t.Skip("NotValid is Postgres's")
	}
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	k := m.DeclaredCheck("tasks_hours_check", m.Dialects{Postgres: `"hours" > 0`})
	mgs := []m.Migration{
		{Name: "0001_initial", Operations: []m.Operation{m.CreateModel{Table: "tasks", Fields: []m.NamedField{
			m.F("id", m.BigAuto()), m.F("hours", m.Int()),
		}}}},
		{Name: "0002_hours", Dependencies: []string{"0001_initial"}, Operations: []m.Operation{
			m.AddConstraint{Table: "tasks", Constraint: k, NotValid: true},
		}},
		{Name: "0003_validate", Dependencies: []string{"0002_hours"}, Operations: []m.Operation{
			m.ValidateConstraint{Table: "tasks", Name: "tasks_hours_check"},
		}},
	}
	bg := context.Background()
	if err := orm.ApplyMigrations(bg, d, "0001", mgs...); err != nil {
		t.Fatal(err)
	}
	if _, err := orm.Exec(ctx, `INSERT INTO tasks (hours) VALUES (0)`); err != nil {
		t.Fatal(err)
	}
	if err := orm.ApplyMigrations(bg, d, "0002", mgs...); err != nil {
		t.Fatalf("NotValid over an old row breaking it: %v", err)
	}
	if _, err := orm.Exec(ctx, `INSERT INTO tasks (hours) VALUES (0)`); err == nil || !strings.Contains(err.Error(), "tasks_hours_check") {
		t.Fatalf("a new row breaking it: %v", err)
	}
	if err := orm.ApplyMigrations(bg, d, "", mgs...); err == nil || !strings.Contains(err.Error(), "tasks_hours_check") {
		t.Fatalf("validated over an old row breaking it: %v", err)
	}
	if _, err := orm.Exec(ctx, `DELETE FROM tasks`); err != nil {
		t.Fatal(err)
	}
	if err := orm.ApplyMigrations(bg, d, "", mgs...); err != nil {
		t.Fatal(err)
	}
}

type LegacyJob struct {
	ID    int64
	Hours int
}

func (LegacyJob) Checks() []orm.RowCheck {
	return []orm.RowCheck{orm.CheckOf(orm.Q{"hours__gt": 0}).Field("hours").Message("takes some hours").On("postgres", "mysql")}
}

var LegacyJobs = orm.For[LegacyJob](orm.Table("legacy_jobs"), orm.Unmanaged())

// An unmanaged model's check is made on the database's own table, as
// makemigrations writes it, and holds there as the ORM's does.
func TestUnmanagedChecksOnPostgres(t *testing.T) {
	if ormtest.Driver() != "postgres" {
		t.Skip("SQLite adds no CHECK to a table the ORM doesn't make")
	}
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	if _, err := orm.Exec(ctx, `CREATE TABLE legacy_jobs (id BIGSERIAL PRIMARY KEY, hours INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	k := m.DeclaredCheck("legacy_jobs_hours_check", m.Dialects{Postgres: `"hours" > 0`, MySQL: "`hours` > 0"})
	if err := orm.ApplyMigrations(context.Background(), d, "", m.Migration{Name: "0001_initial", Operations: []m.Operation{
		m.AddConstraint{Table: "legacy_jobs", Constraint: k},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := LegacyJobs.Create(ctx, &LegacyJob{Hours: 0}); violations(t, err)["legacy_jobs_hours_check"].Message != "takes some hours" {
		t.Fatalf("the ORM's check: %v", err)
	}
	job := &LegacyJob{Hours: 3}
	if err := LegacyJobs.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	_, err := LegacyJobs.Filter(orm.Q{"id": job.ID}).Update(ctx, orm.Set{"hours": 0})
	if violations(t, err)["legacy_jobs_hours_check"].Field != "hours" {
		t.Fatalf("the database's check: %v", err)
	}
}
