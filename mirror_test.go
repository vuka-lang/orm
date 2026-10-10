package orm_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

func TestMirror(t *testing.T) {
	primary := open(t)
	ctx, mirror := ormtest.Mirror(t, primary, Users, Posts)
	both := func(t *testing.T, what string) {
		t.Helper()
		a, err := Users.OrderBy("id").All(primary)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Users.OrderBy("id").All(mirror)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != len(b) {
			t.Fatalf("%s: primary %v, mirror %v", what, names(a), names(b))
		}
		for i := range a {
			a[i].CreatedAt, b[i].CreatedAt = a[i].CreatedAt.UTC().Round(1e3), b[i].CreatedAt.UTC().Round(1e3)
			a[i].UpdatedAt, b[i].UpdatedAt = a[i].UpdatedAt.UTC().Round(1e3), b[i].UpdatedAt.UTC().Round(1e3)
			if a[i].ID != b[i].ID || a[i].Name != b[i].Name || a[i].Age != b[i].Age || !a[i].UpdatedAt.Equal(b[i].UpdatedAt) {
				t.Fatalf("%s: row %d primary %+v, mirror %+v", what, i, a[i], b[i])
			}
		}
	}

	u := User{Name: "Ali", Email: "ali@x", Age: 20}
	if err := Users.Create(ctx, &u); err != nil {
		t.Fatal(err)
	}
	if err := Users.BulkCreate(ctx, []*User{{Name: "Neema", Email: "n@x"}, {Name: "Juma", Email: "j@x"}}); err != nil {
		t.Fatal(err)
	}
	both(t, "create")
	u.Age = 21
	if err := Users.Save(ctx, &u); err != nil {
		t.Fatal(err)
	}
	both(t, "save")
	if _, err := Users.Filter(orm.Q{"age": 0}).Update(ctx, orm.Set{"age": orm.SQL("{0} + 5", orm.F("id"))}); err != nil {
		t.Fatal(err)
	}
	both(t, "update with an expression")
	if _, err := Users.Filter(orm.Q{"name": "Juma"}).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	both(t, "delete")

	boom := errors.New("boom")
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		_ = Users.Create(ctx, &User{Name: "Rolled", Email: "r@x"})
		return boom
	})
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		if err := Users.Create(ctx, &User{Name: "Kept", Email: "k@x"}); err != nil {
			return err
		}
		if n, _ := Users.Count(mirror); n != 2 {
			t.Errorf("mirrored before the commit: %d rows", n)
		}
		return Users.Remove(ctx, &u)
	})
	both(t, "transactions")
	if got := names(must(Users.OrderBy("id").All(mirror))); !slices.Equal(got, []string{"Neema", "Kept"}) {
		t.Fatalf("mirror = %v", got)
	}

	var mu sync.Mutex
	var failed []orm.MirrorError
	stop := orm.OnMirrorError(func(_ context.Context, e orm.MirrorError) {
		mu.Lock()
		failed = append(failed, e)
		mu.Unlock()
	})
	defer stop()
	ormtest.Exec(t, mirror, "DROP TABLE articles")
	if err := Posts.Create(ctx, &Post{Title: "kept on the primary", AuthorID: 1}); err != nil {
		t.Fatalf("a failing mirror failed the write: %v", err)
	}
	if n, _ := Posts.Count(primary); n != 1 || len(failed) != 1 || failed[0].Table != "articles" || failed[0].Op != "create" {
		t.Fatalf("primary rows %d, failures %+v", n, failed)
	}
}

func TestTimesRoundTrip(t *testing.T) {
	ctx := open(t)
	u := User{Name: "Ali", Email: "ali@x"}
	if err := Users.Create(ctx, &u); err != nil {
		t.Fatal(err)
	}
	got, err := Users.Get(ctx, orm.Q{"id": u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if d := got.CreatedAt.Sub(u.CreatedAt); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("created_at came back %v off: wrote %v, read %v", d, u.CreatedAt, got.CreatedAt)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestMirrorRoute routes a table: written to main and mirrored to other,
// read from main.
func TestMirrorRoute(t *testing.T) {
	main, other := useDB(t, "main", true), useDB(t, "other", false)
	orm.Default.Route("users", orm.Route{DB: "main", Mirror: "other"})
	t.Cleanup(func() { orm.Default.Unroute("users") })
	on := func(d *orm.DB) context.Context { return orm.WithDB(context.Background(), d) }
	for _, d := range []*orm.DB{main, other} {
		if err := orm.CreateTables(on(d), Users); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := Users.Create(ctx, &User{Name: "Ali", Email: "ali@x"}); err != nil {
		t.Fatal(err)
	}
	ormtest.Exec(t, on(other), "UPDATE users SET name = 'only on the mirror'")
	got, err := Users.Get(ctx, orm.Q{"email": "ali@x"})
	if err != nil || got.Name != "Ali" {
		t.Fatalf("read from %q: %v", got.Name, err)
	}
	if n, _ := Users.Count(on(other)); n != 1 {
		t.Fatalf("mirror rows: %d", n)
	}
}

// TestCutoverKeys writes rows with their keys (as a mirror or a copy
// does), then inserts as the primary: the new key comes after them.
func TestCutoverKeys(t *testing.T) {
	ctx := open(t)
	ormtest.Exec(t, ctx, "INSERT INTO users (id, created_at, updated_at, name, email, age, active) VALUES (41, '2026-01-01', '2026-01-01', 'Copied', 'c@x', 1, true)")
	u := User{Name: "New", Email: "n@x"}
	if err := Users.Create(ctx, &u); err != nil || u.ID != 42 {
		t.Fatalf("new key %d, %v", u.ID, err)
	}
}
