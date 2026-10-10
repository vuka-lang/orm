package orm_test

import (
	"context"
	"errors"
	"slices"
	"strings"

	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
	_ "github.com/vuka-lang/orm/sqlite"
)

type Base struct {
	ID        int64 `orm:"pk"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (b *Base) BeforeCreate(context.Context) error {
	creates++
	return nil
}

var creates int

type User struct {
	Base
	Name   string
	Email  string `db:"email" orm:"unique"`
	Age    int
	Bio    *string
	Active bool
	Posts  []Post // a relation: not a column
}

type Post struct {
	ID       int64  `gorm:"primaryKey"`
	Title    string `gorm:"column:headline"`
	AuthorID int64
	Draft    bool `gorm:"-"`
}

func (Post) TableName() string { return "articles" }

var (
	Users = orm.For[User]()
	Posts = orm.For[Post]()
)

// open is a database with the test models' tables: SQLite, or the server
// ORMTEST_DRIVER and ORMTEST_DSN name.
func open(t *testing.T) context.Context {
	t.Helper()
	return ormtest.Open(t, Users, Posts, Profiles, Authors, Books, Tags)
}

func seed(t *testing.T, ctx context.Context) {
	t.Helper()
	bio := "writes Go"
	rows := []*User{
		{Name: "Ali", Email: "ali@mail.com", Age: 25, Active: true, Bio: &bio},
		{Name: "Neema", Email: "neema@mail.com", Age: 17},
		{Name: "Juma", Email: "juma@mail.com", Age: 32, Active: true},
		{Name: "Amina", Email: "amina@mail.com", Age: 41},
	}
	if err := Users.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for i, u := range rows {
		if u.ID != int64(i+1) || u.CreatedAt.IsZero() {
			t.Fatalf("row %d after create: %+v", i, u)
		}
	}
}

func names(us []User) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return out
}

func TestModel(t *testing.T) {
	if got := Users.TableName(); got != "users" {
		t.Fatalf("table = %s", got)
	}
	want := []string{"id", "created_at", "updated_at", "name", "email", "age", "bio", "active"}
	if got := Users.Columns(); !slices.Equal(got, want) {
		t.Fatalf("columns = %v: the embedded Base flattened, the relation left out", got)
	}
	if got := Posts.TableName(); got != "articles" {
		t.Fatalf("TableName ignored: %s", got)
	}
	if got := Posts.Columns(); !slices.Equal(got, []string{"id", "headline", "author_id"}) {
		t.Fatalf("GORM tags: %v", got)
	}
}

func TestQueries(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)

	all, err := Users.OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(all), []string{"Ali", "Neema", "Juma", "Amina"}) {
		t.Fatalf("all = %v, %v", names(all), err)
	}
	if all[0].Bio == nil || *all[0].Bio != "writes Go" || all[1].Bio != nil {
		t.Fatalf("nullable: %v %v", all[0].Bio, all[1].Bio)
	}
	adults, err := Users.Filter(orm.Q{"age__gte": 18}).Exclude(orm.Q{"name__icontains": "j"}).OrderBy("-age").All(ctx)
	if err != nil || !slices.Equal(names(adults), []string{"Amina", "Ali"}) {
		t.Fatalf("adults = %v, %v", names(adults), err)
	}
	for _, c := range []struct {
		q    orm.Cond
		want []string
	}{
		{orm.Q{"name": "Juma"}, []string{"Juma"}},
		{orm.Q{"name__iexact": "juma"}, []string{"Juma"}},
		{orm.Q{"name__startswith": "A"}, []string{"Ali", "Amina"}},
		{orm.Q{"name__iendswith": "MA"}, []string{"Neema", "Juma"}},
		{orm.Q{"age__in": []int{17, 41}}, []string{"Neema", "Amina"}},
		{orm.Q{"age__in": []int{}}, []string{}},
		{orm.Q{"age__range": []int{20, 35}}, []string{"Ali", "Juma"}},
		{orm.Q{"bio__isnull": false}, []string{"Ali"}},
		{orm.Q{"bio": nil}, []string{"Neema", "Juma", "Amina"}},
		{orm.Or(orm.Q{"age__lt": 18}, orm.Q{"age__gt": 40}), []string{"Neema", "Amina"}},
		{orm.Not(orm.Q{"active": true}), []string{"Neema", "Amina"}},
		{orm.Q{"Email__contains": "%"}, []string{}},
	} {
		got, err := Users.Filter(c.q).OrderBy("id").All(ctx)
		if err != nil || !slices.Equal(names(got), c.want) {
			t.Errorf("%v: %v, %v", c.q, names(got), err)
		}
	}

	page, _ := Users.OrderBy("id").Offset(1).Limit(2).All(ctx)
	if !slices.Equal(names(page), []string{"Neema", "Juma"}) {
		t.Fatalf("page = %v", names(page))
	}
	if n, err := Users.Filter(orm.Q{"active": true}).Count(ctx); n != 2 || err != nil {
		t.Fatalf("count = %d, %v", n, err)
	}
	if n, _ := Users.OrderBy("id").Limit(3).Count(ctx); n != 3 {
		t.Fatalf("count of a sliced query = %d", n)
	}
	if ok, _ := Users.Filter(orm.Q{"name": "Nobody"}).Exists(ctx); ok {
		t.Fatal("exists")
	}
	first, err := Users.First(ctx)
	if err != nil || first.Name != "Ali" {
		t.Fatalf("first = %v, %v", first.Name, err)
	}
	var seen []string
	for u, err := range Users.OrderBy("-id").Iter(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if seen = append(seen, u.Name); len(seen) == 2 {
			break
		}
	}
	if !slices.Equal(seen, []string{"Amina", "Juma"}) {
		t.Fatalf("iter = %v", seen)
	}
}

func TestGet(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	u, err := Users.Get(ctx, orm.Q{"email": "juma@mail.com"})
	if err != nil || u.Name != "Juma" || u.ID != 3 {
		t.Fatalf("get = %+v, %v", u, err)
	}
	if _, err := Users.Get(ctx, orm.Q{"email": "nobody@mail.com"}); !errors.Is(err, orm.ErrNotFound) || err.Error() != "User matching query does not exist" {
		t.Fatalf("missing: %v", err)
	}
	if _, err := Users.Get(ctx, orm.Q{"active": true}); !errors.Is(err, orm.ErrMultipleFound) {
		t.Fatalf("two: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"nmae": "x"}).All(ctx); err == nil || err.Error() != `orm: User has no field "nmae"` {
		t.Fatalf("typo: %v", err)
	}
	if _, err := Users.OrderBy("-nmae").All(ctx); err == nil {
		t.Fatal("ordered by a field that isn't there")
	}
}

func TestWrites(t *testing.T) {
	ctx := open(t)
	before := creates
	seed(t, ctx)
	if creates-before != 4 {
		t.Fatalf("BeforeCreate promoted from Base ran %d times", creates-before)
	}

	u, _ := Users.Get(ctx, orm.Q{"name": "Neema"})
	created := u.CreatedAt
	orm.Clock = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { orm.Clock = time.Now })
	u.Age = 18
	if err := Users.Save(ctx, &u); err != nil {
		t.Fatal(err)
	}
	got, _ := Users.Get(ctx, orm.Q{"id": u.ID})
	if got.Age != 18 || !got.UpdatedAt.After(created) || !got.CreatedAt.Equal(created) {
		t.Fatalf("saved: %+v (created %v)", got, created)
	}

	n, err := Users.Filter(orm.Q{"age__lt": 30}).Update(ctx, orm.Set{"active": true})
	if n != 2 || err != nil {
		t.Fatalf("update = %d, %v", n, err)
	}
	dup := User{Name: "Ali 2", Email: "ali@mail.com"}
	err = Users.Create(ctx, &dup)
	var ne *orm.Error
	if !errors.As(err, &ne) || ne.Kind != orm.Conflict || !errors.Is(err, orm.ErrConflict) || len(ne.Fields["email"]) == 0 {
		t.Fatalf("duplicate email: %v", err)
	}

	row, made, err := Users.GetOrCreate(ctx, orm.Q{"email": "zawadi@mail.com"}, orm.Defaults{"name": "Zawadi", "age": 29})
	if err != nil || !made || row.ID == 0 || row.Name != "Zawadi" || row.Age != 29 {
		t.Fatalf("created = %+v %v %v", row, made, err)
	}
	again, made, err := Users.GetOrCreate(ctx, orm.Q{"email": "zawadi@mail.com"}, orm.Defaults{"name": "Other"})
	if err != nil || made || again.ID != row.ID {
		t.Fatalf("got = %+v %v %v", again, made, err)
	}

	if err := Users.Remove(ctx, &again); err != nil {
		t.Fatal(err)
	}
	if err := Users.Remove(ctx, &again); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if n, err := Users.Filter(orm.Q{"age__lt": 20}).Delete(ctx); n != 1 || err != nil {
		t.Fatalf("delete = %d, %v", n, err)
	}

	post := Post{Title: "Django in Go", AuthorID: 1}
	if err := Posts.Create(ctx, &post); err != nil || post.ID == 0 {
		t.Fatalf("post = %+v, %v", post, err)
	}
}

func TestValuesAndAggregate(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	got, err := Users.OrderBy("age").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(got, []string{"Neema", "Ali", "Juma", "Amina"}) {
		t.Fatalf("values = %v, %v", got, err)
	}
	type nameAge struct {
		Name string
		Age  int
	}
	pairs, err := Users.Filter(orm.Q{"active": true}).OrderBy("id").Values[nameAge]("name", "age").All(ctx)
	if err != nil || len(pairs) != 2 || pairs[1] != (nameAge{"Juma", 32}) {
		t.Fatalf("pairs = %v, %v", pairs, err)
	}
	r, err := Users.Aggregate(ctx, orm.Count("id"), orm.Avg("age"), orm.Max("age"))
	if err != nil || r.Int("id__count") != 4 || r.Float("age__avg") != 28.75 || r.Int("age__max") != 41 {
		t.Fatalf("aggregate = %v, %v", r, err)
	}
}

func TestAtomic(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	boom := errors.New("boom")
	err := orm.Atomic(ctx, func(ctx context.Context) error {
		if _, err := Users.Filter(orm.Q{"name": "Ali"}).Update(ctx, orm.Set{"age": 99}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if u, _ := Users.Get(ctx, orm.Q{"name": "Ali"}); u.Age != 25 {
		t.Fatalf("rolled back? age %d", u.Age)
	}

	err = orm.Atomic(ctx, func(ctx context.Context) error {
		if _, err := Users.Filter(orm.Q{"name": "Ali"}).Update(ctx, orm.Set{"age": 26}); err != nil {
			return err
		}
		inner := orm.Atomic(ctx, func(ctx context.Context) error {
			if _, err := Users.Filter(orm.Q{"name": "Juma"}).Update(ctx, orm.Set{"age": 99}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(inner, boom) {
			t.Fatalf("savepoint: %v", inner)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ali, _ := Users.Get(ctx, orm.Q{"name": "Ali"})
	juma, _ := Users.Get(ctx, orm.Q{"name": "Juma"})
	if ali.Age != 26 || juma.Age != 32 {
		t.Fatalf("savepoint rolled back alone? ali %d, juma %d", ali.Age, juma.Age)
	}
}

// useDB adds a fresh in-memory SQLite database to orm.Default as name
// (the default when def), removed when the test ends.
func useDB(t *testing.T, name string, def bool) *orm.DB {
	t.Helper()
	d, err := orm.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	var opts []orm.DatabaseOption
	if def {
		opts = append(opts, orm.AsDefault())
	}
	orm.AddDatabase(name, d, opts...)
	t.Cleanup(func() {
		orm.Default.Remove(name)
		_ = d.SQL().Close()
	})
	return d
}

// TestDefaultDatabase: with no WithDB, the model finds the database
// orm.AddDatabase added. That database has no users table, which is how
// the test knows the query went there.
func TestDefaultDatabase(t *testing.T) {
	useDB(t, "main", true)
	_, err := Users.Count(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no such table: users") {
		t.Fatalf("on the default database? %v", err)
	}
}

func TestBadModelFailsCheck(t *testing.T) {
	type orphan struct {
		ID    int64
		Owner *User `orm:"fk:owner_id"`
	}
	m := orm.For[orphan]()
	if err := orm.Check(m); err == nil || !strings.Contains(err.Error(), "no column") {
		t.Fatalf("check = %v", err)
	}
	if _, err := m.Count(context.Background()); err == nil || !strings.Contains(err.Error(), "no column") {
		t.Fatalf("query = %v", err)
	}
	if err := orm.Check(Users, Posts); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedScanners(t *testing.T) {
	if !orm.Generated[User]() || !orm.Generated[Post]() {
		t.Skip("no generated scanners: run go run ./cmd/ormgen -tests .")
	}
	type stale struct{ ID int64 }
	if orm.Generated[stale]() {
		t.Fatal("a model with no generated scanner reports one")
	}
}

// TestDriver checks the suite runs on the database it says: SQLite, or
// ORMTEST_DRIVER's server.
func TestDriver(t *testing.T) {
	ctx := open(t)
	d, ok := orm.DBFrom(ctx)
	if !ok || d.Dialect().Name() != ormtest.Driver() {
		t.Fatalf("running on %v, want %s", d.Dialect().Name(), ormtest.Driver())
	}
	var v string
	_ = d.SQL().QueryRowContext(ctx, map[string]string{"sqlite": "SELECT sqlite_version()", "postgres": "SELECT version()", "mysql": "SELECT version()"}[ormtest.Driver()]).Scan(&v)
	t.Logf("%s %s", ormtest.Driver(), v)
}

// TestRowsReadApart reads rows one after another into the same storage
// (each path reuses it): a NULL after a value leaves no trace of it, and
// rows already read keep their own pointers.
func TestRowsReadApart(t *testing.T) {
	ctx := relOpen(t)
	seed(t, ctx)
	if _, err := Users.Filter(orm.Q{"name": "Neema"}).Update(ctx, orm.Set{"bio": "reads"}); err != nil {
		t.Fatal(err)
	}
	type userBio struct {
		Name string
		Bio  *string
	}
	bios := map[string]func() ([]*string, error){
		"generated": func() ([]*string, error) {
			us, err := Users.OrderBy("id").All(ctx)
			return collectBios(us, func(u User) *string { return u.Bio }), err
		},
		"raw": func() ([]*string, error) {
			us, err := Users.Raw("SELECT * FROM users ORDER BY id").All(ctx)
			return collectBios(us, func(u User) *string { return u.Bio }), err
		},
		"values": func() ([]*string, error) {
			us, err := Users.OrderBy("id").Values[userBio]().All(ctx)
			return collectBios(us, func(u userBio) *string { return u.Bio }), err
		},
		"values scalar": func() ([]*string, error) {
			return Users.OrderBy("id").Values[*string]("bio").All(ctx)
		},
	}
	for name, read := range bios {
		got, err := read()
		if err != nil || len(got) != 4 || got[0] == nil || got[1] == nil || *got[0] != "writes Go" || *got[1] != "reads" || got[2] != nil || got[3] != nil {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
	// SelectRelated reads by reflection.
	books, err := Books.SelectRelated("author").OrderBy("id").All(ctx)
	if err != nil || len(books) != 3 {
		t.Fatal(books, err)
	}
	a0, a2 := books[0].Author, books[2].Author
	if a0 == nil || a2 == nil || a0 == a2 || a0.ProfileID == nil || *a0.ProfileID != 1 || a2.ProfileID != nil || a2.Name != "Neema" {
		t.Fatalf("authors read into each other: %+v %+v", a0, a2)
	}
}

func collectBios[R any](rows []R, bio func(R) *string) []*string {
	out := make([]*string, len(rows))
	for i, r := range rows {
		out[i] = bio(r)
	}
	return out
}
