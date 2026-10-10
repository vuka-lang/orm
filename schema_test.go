package orm_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

// One set of models over two schemas holding the same data: the default
// names, and a "legacy" schema that names its tables, columns and link
// table apart, has a column the other hasn't (phone) and lacks one (bio),
// and reads a person's first name through the profile.
type Person struct {
	ID        int64
	Email     string `legacy:"email_address"`
	Name      string `legacy:"full_name"`
	FirstName string `legacy:"profile__first_name"`
	Bio       string `legacy:"-"`
	Phone     string `gorm:"-" legacy:"phone"`
	Active    bool   `legacy:"is_active"`
	TeamID    int64  `legacy:"team_ref"`
	Team      *Team  `gorm:"foreignKey:TeamID" orm:"related:members"`
	Roles     []Role `gorm:"many2many:user_roles;joinForeignKey:user_id;joinReferences:role_id" legacy:"many2many:auth_user_groups;joinReferences:group_id"`
	Profile   *PersonProfile
}

func (Person) TableName() string       { return "users" }
func (Person) LegacyTableName() string { return "auth_user" }

type PersonProfile struct {
	ID        int64
	PersonID  int64   `gorm:"uniqueIndex" legacy:"owner"`
	FirstName string  `legacy:"fname"`
	Person    *Person `gorm:"foreignKey:PersonID" orm:"related:profile"`
}

func (PersonProfile) TableName() string       { return "profiles" }
func (PersonProfile) LegacyTableName() string { return "user_profile" }

type Team struct {
	ID      int64
	Name    string   `legacy:"team_name"`
	Members []Person `gorm:"foreignKey:TeamID"`
}

func (Team) LegacyTableName() string { return "team" }

type Role struct {
	ID   int64
	Name string `legacy:"title"`
}

func (Role) LegacyTableName() string { return "auth_group" }

var (
	mainSchema   = orm.Schema{}
	legacySchema = orm.Schema{Names: "legacy", Unmanaged: true}
)

type schemaCase struct {
	name string
	s    orm.Schema
	ctx  context.Context
	link string // the table between people and roles, and its columns
}

// schemas is both schemas, each on a fresh database with the same data.
func schemas(t *testing.T) []schemaCase {
	t.Helper()
	cases := []schemaCase{
		{name: "main", s: mainSchema, link: "user_roles (user_id, role_id)"},
		{name: "legacy", s: legacySchema, link: "auth_user_groups (user_id, group_id)"},
	}
	for i := range cases {
		c := &cases[i]
		c.ctx = ormtest.Open(t, orm.Of[Team](c.s), orm.Of[Role](c.s), orm.Of[Person](c.s), orm.Of[PersonProfile](c.s))
		ormtest.Seed(t, c.ctx, orm.Of[Team](c.s), &Team{Name: "Core"}, &Team{Name: "Ops"})
		ormtest.Seed(t, c.ctx, orm.Of[Role](c.s), &Role{Name: "admin"}, &Role{Name: "editor"}, &Role{Name: "viewer"})
		ormtest.Seed(t, c.ctx, orm.Of[Person](c.s),
			&Person{Email: "ali@x", Name: "Ali Musa", FirstName: "Ali", Bio: "go", Phone: "255-1", Active: true, TeamID: 1},
			&Person{Email: "neema@x", Name: "Neema Juma", FirstName: "Neema", Active: true, TeamID: 1},
			&Person{Email: "juma@x", Name: "Juma Ali", FirstName: "Juma", TeamID: 2})
		ormtest.Seed(t, c.ctx, orm.Of[PersonProfile](c.s),
			&PersonProfile{PersonID: 1, FirstName: "Ali"}, &PersonProfile{PersonID: 2, FirstName: "Neema"}, &PersonProfile{PersonID: 3, FirstName: "Juma"})
		if _, err := orm.ExecOn(c.ctx, c.s, "INSERT INTO "+c.link+" VALUES (?, ?), (?, ?), (?, ?)", 1, 1, 1, 2, 2, 2); err != nil {
			t.Fatal(err)
		}
	}
	return cases
}

// same runs q on both schemas and fails unless they agree.
func same(t *testing.T, cases []schemaCase, what string, q func(c schemaCase) (any, error)) any {
	t.Helper()
	var first any
	for i, c := range cases {
		got, err := q(c)
		if err != nil {
			t.Fatalf("%s on %s: %v", what, c.name, err)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(first, got) {
			t.Fatalf("%s:\n  %s: %v\n  %s: %v", what, cases[0].name, first, c.name, got)
		}
	}
	return first
}

func people(ps []Person) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		s := fmt.Sprintf("%d %s %s %s %v team=%d", p.ID, p.Email, p.Name, p.FirstName, p.Active, p.TeamID)
		if p.Team != nil {
			s += " " + p.Team.Name
		}
		if p.Profile != nil {
			s += " profile=" + p.Profile.FirstName
		}
		for _, r := range p.Roles {
			s += " " + r.Name
		}
		out[i] = s
	}
	return out
}

func TestSchemaQueries(t *testing.T) {
	cases := schemas(t)
	got := same(t, cases, "filter through relations", func(c schemaCase) (any, error) {
		ps, err := orm.Of[Person](c.s).Filter(orm.Q{"team__name": "Core", "roles__name": "editor"}).OrderBy("id").All(c.ctx)
		return people(ps), err
	})
	if !slices.Equal(got.([]string), []string{"1 ali@x Ali Musa Ali true team=1", "2 neema@x Neema Juma Neema true team=1"}) {
		t.Fatalf("filtered %v", got)
	}
	got = same(t, cases, "SelectRelated, PrefetchRelated, order by a field read through a relation", func(c schemaCase) (any, error) {
		ps, err := orm.Of[Person](c.s).SelectRelated("team", "profile").PrefetchRelated("roles").OrderBy("-first_name").All(c.ctx)
		return people(ps), err
	})
	if want := "2 neema@x Neema Juma Neema true team=1 Core profile=Neema editor"; got.([]string)[0] != want {
		t.Fatalf("got %v", got)
	}
	same(t, cases, "a lookup on a field read through a relation", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).Filter(orm.Q{"first_name__startswith": "J"}).Exclude(orm.Q{"active": true}).Values[string]("email").All(c.ctx)
	})
	same(t, cases, "count, exists, paginate", func(c schemaCase) (any, error) {
		n, err := orm.Of[Person](c.s).Filter(orm.Q{"roles__name__in": []string{"admin", "editor"}}).Count(c.ctx)
		if err != nil {
			return nil, err
		}
		ok, err := orm.Of[Person](c.s).Filter(orm.Q{"profile__first_name": "Juma"}).Exists(c.ctx)
		if err != nil {
			return nil, err
		}
		page, err := orm.Paginate(c.ctx, orm.Of[Person](c.s).Filter(), orm.PageRequest{Page: 2, Size: 2, Sort: "first_name"}, orm.Sortable("first_name"))
		return []any{n, ok, page.Total, page.Pages, people(page.Items)}, err
	})
}

// PersonRow is a struct target whose fields say what Values reads.
type PersonRow struct {
	Email string
	Team  string `orm:"team__name"`
	First string `orm:"profile__first_name"`
}

func TestSchemaValues(t *testing.T) {
	cases := schemas(t)
	got := same(t, cases, "a scalar through a relation", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).OrderBy("id").Values[string]("team__name").All(c.ctx)
	})
	if !slices.Equal(got.([]string), []string{"Core", "Core", "Ops"}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "a struct by path tags", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).OrderBy("id").Values[PersonRow]().All(c.ctx)
	})
	if got.([]PersonRow)[2] != (PersonRow{"juma@x", "Ops", "Juma"}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "maps, a row per related row through a many-to-many", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).OrderBy("id", "roles__name").Values[map[string]any]("email", "active", "roles__name").All(c.ctx)
	})
	want := []map[string]any{
		{"email": "ali@x", "active": true, "roles__name": "admin"},
		{"email": "ali@x", "active": true, "roles__name": "editor"},
		{"email": "neema@x", "active": true, "roles__name": "editor"},
		{"email": "juma@x", "active": false, "roles__name": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "lists", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).Filter(orm.Q{"id": 1}).Values[[]any]("name", "team__id", "first_name").All(c.ctx)
	})
	if !reflect.DeepEqual(got, [][]any{{"Ali Musa", int64(1), "Ali"}}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "an aggregate groups by the other names", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).Annotate("n", orm.Count("id")).OrderBy("team__name").Values[[]any]("team__name", "n").All(c.ctx)
	})
	if !reflect.DeepEqual(got, [][]any{{"Core", int64(2)}, {"Ops", int64(1)}}) {
		t.Fatalf("got %v", got)
	}
}

func TestSchemaInverses(t *testing.T) {
	cases := schemas(t)
	got := same(t, cases, "a foreign key's inverse, declared by the team too", func(c schemaCase) (any, error) {
		return orm.Of[Team](c.s).Filter(orm.Q{"members__email": "juma@x"}).Values[string]("name").All(c.ctx)
	})
	if !slices.Equal(got.([]string), []string{"Ops"}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "a many-to-many's inverse, by its default name", func(c schemaCase) (any, error) {
		return orm.Of[Role](c.s).Filter(orm.Q{"persons__active": true}).OrderBy("name").Values[string]("name").All(c.ctx)
	})
	if !slices.Equal(got.([]string), []string{"admin", "editor"}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "Values through an inverse", func(c schemaCase) (any, error) {
		return orm.Of[Role](c.s).OrderBy("name", "persons__email").Values[[]any]("name", "persons__email").All(c.ctx)
	})
	if !reflect.DeepEqual(got, [][]any{{"admin", "ali@x"}, {"editor", "ali@x"}, {"editor", "neema@x"}, {"viewer", nil}}) {
		t.Fatalf("got %v", got)
	}
	same(t, cases, "a one-to-one inverse: ordered by, selected, prefetched into the field of its name", func(c schemaCase) (any, error) {
		a, err := orm.Of[Person](c.s).SelectRelated("profile").OrderBy("-profile__first_name").All(c.ctx)
		if err != nil {
			return nil, err
		}
		b, err := orm.Of[Person](c.s).PrefetchRelated("profile").OrderBy("-profile__first_name").All(c.ctx)
		if err != nil || !slices.Equal(people(a), people(b)) || a[0].Profile.FirstName != "Neema" {
			return nil, fmt.Errorf("select %v, prefetch %v: %v", people(a), people(b), err)
		}
		return people(a), nil
	})
	same(t, cases, "an explicit has-many is the inverse", func(c schemaCase) (any, error) {
		ts, err := orm.Of[Team](c.s).PrefetchRelated("members").OrderBy("id").All(c.ctx)
		if err != nil {
			return nil, err
		}
		return []any{people(ts[0].Members), people(ts[1].Members)}, nil
	})
	for _, c := range cases {
		_, err := orm.Of[Role](c.s).PrefetchRelated("persons").All(c.ctx)
		if err == nil || !strings.Contains(err.Error(), "no field to hold persons") {
			t.Fatalf("prefetched an inverse no field holds: %v", err)
		}
	}
}

func TestSchemaConditionals(t *testing.T) {
	cases := schemas(t)
	role := orm.Case(
		orm.When(orm.Q{"team__name": "Core", "roles__name": "admin"}, "lead"),
		orm.When(orm.Q{"first_name__startswith": "N"}, orm.F("team__name")),
	).Else(orm.Switch(orm.F("active")).Case(false, "away"))
	got := same(t, cases, "a Case through relations and a field read through one, a Switch", func(c schemaCase) (any, error) {
		return orm.Of[Person](c.s).Annotate("role", role).Filter(orm.Q{"role__in": []string{"lead", "away", "Core"}}).OrderBy("id").Values[[]any]("email", "role").All(c.ctx)
	})
	if !reflect.DeepEqual(got, [][]any{{"ali@x", "lead"}, {"neema@x", "Core"}, {"juma@x", "away"}}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "conditional aggregates, grouped and not", func(c schemaCase) (any, error) {
		per, err := orm.Of[Person](c.s).Annotate("editors", orm.Count("id").Filter(orm.Q{"active": true, "roles__name": "editor"})).
			OrderBy("team__name").Values[[]any]("team__name", "editors").All(c.ctx)
		if err != nil {
			return nil, err
		}
		r, err := orm.Of[Person](c.s).Aggregate(c.ctx, orm.Count("id").Filter(orm.Q{"first_name__startswith": "J"}).As("j"))
		return []any{per, r.Int("j")}, err
	})
	if !reflect.DeepEqual(got, []any{[][]any{{"Core", int64(2)}, {"Ops", int64(0)}}, int64(1)}) {
		t.Fatalf("got %v", got)
	}
	got = same(t, cases, "a conditional update", func(c schemaCase) (any, error) {
		_, err := orm.Of[Person](c.s).Filter(orm.Q{"id__gt": 0}).Update(c.ctx, orm.Set{
			"name": orm.Case(orm.When(orm.Q{"roles__name": "editor"}, orm.Upper.Of(orm.F("name")))).Else(orm.F("name")),
		})
		if err != nil {
			return nil, err
		}
		return orm.Of[Person](c.s).OrderBy("id").Values[string]("name").All(c.ctx)
	})
	if !slices.Equal(got.([]string), []string{"ALI MUSA", "NEEMA JUMA", "Juma Ali"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSchemaWrites(t *testing.T) {
	cases := schemas(t)
	legacy := cases[1]
	persons := orm.Of[Person](legacySchema)
	var heard []orm.Change[Person]
	stop := persons.OnChange(func(_ context.Context, c orm.Change[Person]) { heard = append(heard, c) })
	defer stop()
	p := Person{Email: "amina@x", Name: "Amina", FirstName: "ignored", Phone: "255-4", Active: true, TeamID: 2}
	if err := persons.Create(legacy.ctx, &p); err != nil {
		t.Fatal(err)
	}
	p.Name = "Amina Said"
	if err := persons.Save(legacy.ctx, &p); err != nil {
		t.Fatal(err)
	}
	rows, err := orm.RawOn[map[string]any](legacy.ctx, legacySchema, "SELECT email_address, full_name, phone, is_active, team_ref FROM auth_user WHERE id = ?", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	active := any(int64(1)) // a raw read gets the driver's value: Postgres has booleans
	if ormtest.Driver() == "postgres" {
		active = true
	}
	if want := (map[string]any{"email_address": "amina@x", "full_name": "Amina Said", "phone": "255-4", "is_active": active, "team_ref": int64(2)}); !reflect.DeepEqual(rows, []map[string]any{want}) {
		t.Fatalf("written %v", rows)
	}
	if len(heard) != 2 || heard[1].Kind != orm.Updated {
		t.Fatalf("heard %v", heard)
	}
	cols := persons.ColumnValues(heard[1].Row)
	if cols["full_name"] != "Amina Said" || cols["email_address"] != "amina@x" || cols["name"] != nil || len(cols) != 6 {
		t.Fatalf("the change feed's columns %v", cols)
	}
	if got := persons.Columns(); !slices.Equal(got, []string{"id", "email_address", "full_name", "phone", "is_active", "team_ref"}) {
		t.Fatalf("columns %v", got)
	}
	_, err = persons.Filter(orm.Q{"id": p.ID}).Update(legacy.ctx, orm.Set{"first_name": "x"})
	if err == nil || err.Error() != "orm: FirstName is read through profile: write PersonProfile" {
		t.Fatalf("updated a field read through a relation: %v", err)
	}
	if _, err := orm.Of[Person](mainSchema).Filter(orm.Q{"id": 1}).Update(cases[0].ctx, orm.Set{"first_name": "Alia"}); err != nil {
		t.Fatalf("the column of the main schema: %v", err)
	}
}

type Transfer struct {
	ID     int64
	FromID int64
	ToID   int64
	From   *Team `gorm:"foreignKey:FromID"`
	To     *Team `gorm:"foreignKey:ToID"`
}

type Misfit struct {
	ID      int64
	OwnerID int64
	Owner   *Person `gorm:"foreignKey:OwnerID;references:Nope"`
	City    string  `legacy:"owner__nowhere"`
}

func TestSchemaCheck(t *testing.T) {
	ctx := context.Background()
	if err := legacySchema.Verify(ctx, Person{}, &PersonProfile{}, Team{}, Role{}); err != nil {
		t.Fatalf("the test models: %v", err)
	}
	if err := mainSchema.Verify(ctx, Person{}, PersonProfile{}, Team{}, Role{}); err != nil {
		t.Fatalf("the test models: %v", err)
	}
	err := legacySchema.Verify(ctx, Transfer{}, Misfit{})
	for _, want := range []string{
		`Team's inverse "transfers" is claimed by Transfer.From and Transfer.To`,
		`Misfit.Owner: Person has no field "Nope"`,
		`Misfit.City is read through owner__nowhere`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in %v", want, err)
		}
	}
}

func TestRaw(t *testing.T) {
	cases := schemas(t)
	sqls := map[string]string{
		"main":   "SELECT * FROM users WHERE first_name <> ? ORDER BY id",
		"legacy": "SELECT u.*, p.fname AS first_name FROM auth_user u JOIN user_profile p ON p.owner = u.id WHERE p.fname <> ? ORDER BY u.id",
	}
	got := same(t, cases, "raw rows of a model, relations prefetched", func(c schemaCase) (any, error) {
		ps, err := orm.Of[Person](c.s).Raw(sqls[c.name], "Neema").PrefetchRelated("team", "roles").All(c.ctx)
		return people(ps), err
	})
	if want := []string{"1 ali@x Ali Musa Ali true team=1 Core admin editor", "3 juma@x Juma Ali Juma false team=2 Ops"}; !slices.Equal(got.([]string), want) {
		t.Fatalf("got %v", got)
	}
	ctx := cases[1].ctx
	first, err := orm.Of[Person](legacySchema).Raw("SELECT * FROM auth_user WHERE phone = ?", "255-1").First(ctx)
	if err != nil || first.Phone != "255-1" || first.Email != "ali@x" {
		t.Fatalf("first %+v, %v", first, err)
	}
	if _, err := orm.Of[Person](legacySchema).Raw("SELECT * FROM auth_user WHERE id = 0").First(ctx); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("no row: %v", err)
	}

	emails, err := orm.RawOn[string](ctx, legacySchema, "SELECT email_address FROM auth_user WHERE email_address LIKE '%?%' OR is_active = ? ORDER BY id", true)
	if err != nil || !slices.Equal(emails, []string{"ali@x", "neema@x"}) {
		t.Fatalf("scalars %v, %v", emails, err)
	}
	type row struct {
		Email string `orm:"column:email_address"`
		Title string
	}
	rows, err := orm.RawOn[row](ctx, legacySchema, "SELECT u.email_address, g.title FROM auth_user u JOIN auth_user_groups l ON l.user_id = u.id JOIN auth_group g ON g.id = l.group_id ORDER BY u.id, g.title")
	if err != nil || !slices.Equal(rows, []row{{"ali@x", "admin"}, {"ali@x", "editor"}, {"neema@x", "editor"}}) {
		t.Fatalf("structs %v, %v", rows, err)
	}
	lists, err := orm.RawOn[[]any](ctx, legacySchema, "SELECT title, id FROM auth_group WHERE id = ?", 3)
	if err != nil || !reflect.DeepEqual(lists, [][]any{{"viewer", int64(3)}}) {
		t.Fatalf("lists %v, %v", lists, err)
	}

	err = orm.Atomic(ctx, func(ctx context.Context) error {
		n, err := orm.ExecOn(ctx, legacySchema, "UPDATE auth_user SET is_active = ? WHERE team_ref = ?", false, 1)
		if err != nil || n != 2 {
			t.Fatalf("exec %d, %v", n, err)
		}
		left, _ := orm.RawOn[int64](ctx, legacySchema, "SELECT COUNT(*) FROM auth_user WHERE is_active")
		if left[0] != 0 {
			t.Fatalf("the transaction's own write unseen: %v", left)
		}
		return errors.New("roll back")
	})
	if active, _ := orm.RawOn[int64](ctx, legacySchema, "SELECT COUNT(*) FROM auth_user WHERE is_active"); err == nil || active[0] != 2 {
		t.Fatalf("rolled back: %v, %v", active, err)
	}
	if _, err := orm.ExecOn(ctx, legacySchema, "DELETE FROM auth_user WHERE id = ?"); err == nil {
		t.Fatal("ran with a ? and no argument")
	}
}
