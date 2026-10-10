package orm_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vuka-lang/orm"
)

func TestPaginate(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	opts := []orm.PageOption{orm.Sortable("name", "age"), orm.Searchable("name", "email"), orm.DefaultSort("id"), orm.MaxSize(3)}

	p, err := orm.Paginate(ctx, Users.QuerySet, orm.PageFrom(url.Values{"page": {"2"}, "size": {"2"}, "sort": {"-age"}}), opts...)
	if err != nil || p.Total != 4 || p.Pages != 2 || p.Page != 2 || !slices.Equal(names(p.Items), []string{"Ali", "Neema"}) {
		t.Fatalf("page 2 by age = %+v, %v", p, err)
	}
	p, _ = orm.Paginate(ctx, Users.QuerySet, orm.PageRequest{Sort: "email;DROP TABLE users", Size: 50}, opts...)
	if p.Size != 3 || !slices.Equal(names(p.Items), []string{"Ali", "Neema", "Juma"}) {
		t.Fatalf("a sort not allowed falls back, the size capped: %+v", p)
	}
	p, _ = orm.Paginate(ctx, Users.Filter(orm.Q{"age__gt": 18}), orm.PageRequest{Search: "AM"}, opts...)
	if p.Total != 1 || p.Items[0].Name != "Amina" {
		t.Fatalf("search = %+v", p)
	}
	p, _ = orm.Paginate(ctx, Users.QuerySet, orm.PageRequest{Page: 9}, opts...)
	if len(p.Items) != 0 || p.Total != 4 {
		t.Fatalf("a page past the end = %+v", p)
	}
}

func TestOnChange(t *testing.T) {
	ctx := open(t)
	var heard []string
	stop := Users.OnChange(func(_ context.Context, c orm.Change[User]) {
		switch c.Kind {
		case orm.Created, orm.Updated, orm.Deleted:
			heard = append(heard, map[orm.ChangeKind]string{orm.Created: "created ", orm.Updated: "updated ", orm.Deleted: "deleted "}[c.Kind]+c.Row.Name)
		case orm.BulkUpdated:
			heard = append(heard, "bulk updated")
		case orm.BulkDeleted:
			heard = append(heard, "bulk deleted")
		}
	})
	defer stop()

	u := User{Name: "Ali", Email: "ali@x"}
	_ = Users.Create(ctx, &u)
	u.Age = 30
	_ = Users.Save(ctx, &u)
	_, _ = Users.Filter(orm.Q{"id": u.ID}).Update(ctx, orm.Set{"age": 31})

	boom := errors.New("boom")
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		_ = Users.Create(ctx, &User{Name: "Rolled", Email: "r@x"})
		if len(heard) != 3 {
			t.Errorf("heard before commit: %v", heard)
		}
		return boom
	})
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		_ = orm.Atomic(ctx, func(ctx context.Context) error {
			_ = Users.Create(ctx, &User{Name: "Savepoint", Email: "s@x"})
			return boom
		})
		return Users.Create(ctx, &User{Name: "Kept", Email: "k@x"})
	})
	_ = Users.Remove(ctx, &u)
	_, _ = Users.Filter(orm.Q{"name": "Kept"}).Delete(ctx)
	want := []string{"created Ali", "updated Ali", "bulk updated", "created Kept", "deleted Ali", "bulk deleted"}
	if !slices.Equal(heard, want) {
		t.Fatalf("heard %v\nwant  %v", heard, want)
	}
	stop()
	_ = Users.Create(ctx, &User{Name: "After", Email: "a@x"})
	if len(heard) != len(want) {
		t.Fatal("heard after stop")
	}
}

func TestPaginateBounds(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	p, err := orm.Paginate(ctx, Users.QuerySet, orm.PageRequest{Page: 1 << 62, Size: 50, Search: strings.Repeat("é", 500)}, orm.Searchable("name"))
	if err != nil || len(p.Items) != 0 || p.Page != 1<<62 {
		t.Fatalf("a page far past the end: %+v, %v", p, err)
	}
}

func TestBulkCreateWideBatches(t *testing.T) {
	ctx := open(t)
	rows := make([]*User, 1200)
	for i := range rows {
		rows[i] = &User{Name: "u", Email: strings.Repeat("e", 1) + string(rune('a'+i%26)) + strconv.Itoa(i)}
	}
	if err := Users.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if n, _ := Users.Count(ctx); n != 1200 || rows[1199].ID == 0 {
		t.Fatalf("count %d, last id %d", n, rows[1199].ID)
	}
}

func TestUnfilteredWrites(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	for name, run := range map[string]func() (int64, error){
		"delete":            func() (int64, error) { return Users.Delete(ctx) },
		"update":            func() (int64, error) { return Users.Update(ctx, orm.Set{"age": 1}) },
		"empty Q":           func() (int64, error) { return Users.Filter(orm.Q{}).Delete(ctx) },
		"empty And":         func() (int64, error) { return Users.Filter(orm.And()).Update(ctx, orm.Set{"age": 1}) },
		"ordered, no where": func() (int64, error) { return Users.OrderBy("name").Delete(ctx) },
	} {
		if n, err := run(); !errors.Is(err, orm.ErrUnfiltered) || n != 0 {
			t.Errorf("%s: %d, %v", name, n, err)
		}
	}
	if n, _ := Users.Count(ctx); n != 4 {
		t.Fatalf("rows touched: %d left", n)
	}
	if n, err := Users.Filter(orm.Q{"id__in": []int64{}}).Delete(ctx); err != nil || n != 0 {
		t.Fatalf("a condition matching nothing: %d, %v", n, err)
	}
	if n, err := Users.Unfiltered().Update(ctx, orm.Set{"age": 1}); err != nil || n != 4 {
		t.Fatalf("unfiltered update: %d, %v", n, err)
	}
	if n, err := Users.Unfiltered().Delete(ctx); err != nil || n != 4 {
		t.Fatalf("unfiltered delete: %d, %v", n, err)
	}
}
