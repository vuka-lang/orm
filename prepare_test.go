package orm_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

var (
	userByEmail   = Users.Filter(orm.Q{"email": orm.Param(0)}).Prepare()
	usersAgedOver = Users.Filter(orm.Q{"age__gte": orm.Param(0), "active": orm.Param(1)}).OrderBy("id").Prepare2[int, bool]()
)

func TestPrepare(t *testing.T) {
	for _, reflect := range []bool{false, true} {
		orm.SetReflectOnly(reflect)
		ctx := open(t)
		seed(t, ctx)

		for _, email := range []string{"ali@mail.com", "juma@mail.com"} {
			u, err := userByEmail.Get(ctx, email)
			if err != nil || u.Email != email || u.ID == 0 {
				t.Fatalf("Get(%q) = %+v, %v", email, u, err)
			}
		}
		if _, err := userByEmail.Get(ctx, "nobody@mail.com"); !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("Get of none: %v", err)
		}
		rows, err := usersAgedOver.All(ctx, 20, true)
		if err != nil || strings.Join(names(rows), ",") != "Ali,Juma" {
			t.Fatalf("All = %v, %v", names(rows), err)
		}
		rows, err = usersAgedOver.All(ctx, 30, false)
		if err != nil || strings.Join(names(rows), ",") != "Amina" {
			t.Fatalf("All again = %v, %v", names(rows), err)
		}
		first, err := usersAgedOver.First(ctx, 18, true)
		if err != nil || first.Name != "Ali" {
			t.Fatalf("First = %+v, %v", first, err)
		}
		n := 0
		for _, err := range usersAgedOver.Iter(ctx, 18, true) {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n != 2 {
			t.Fatalf("Iter read %d rows", n)
		}
		multi := Users.Filter(orm.Q{"age__gte": orm.Param(0)}).Prepare1[int]()
		if _, err := multi.Get(ctx, 18); !errors.Is(err, orm.ErrMultipleFound) {
			t.Fatalf("Get of several: %v", err)
		}
		none, err := Users.Filter(orm.Q{"age__gt": orm.Param(0)}).Prepare().All(ctx, 100)
		if err != nil || none == nil || len(none) != 0 {
			t.Fatalf("All of none = %#v, %v", none, err)
		}
		orm.SetReflectOnly(false)
	}
}

// A Param of a LIKE lookup is escaped as a value is, at each run.
func TestPrepareLike(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	if err := Users.Create(ctx, &User{Name: "50% off", Email: "deal@mail.com"}); err != nil {
		t.Fatal(err)
	}
	like := Users.Filter(orm.Q{"name__icontains": orm.Param(0)}).OrderBy("id").Prepare()
	for _, c := range []struct{ term, want string }{{"am", "Amina"}, {"%", "50% off"}, {"0% O", "50% off"}, {"_", ""}} {
		rows, err := like.All(ctx, c.term)
		if err != nil || strings.Join(names(rows), ",") != c.want {
			t.Fatalf("icontains %q = %v, %v", c.term, names(rows), err)
		}
	}
	expr := Users.Annotate("next", orm.SQL("{0} + {1}", orm.F("age"), orm.Param(1))).Filter(orm.Q{"next__gt": orm.Param(0)}).OrderBy("id").Prepare()
	rows, err := expr.All(ctx, 40, 10)
	if err != nil || strings.Join(names(rows), ",") != "Juma,Amina" {
		t.Fatalf("expression = %v, %v", names(rows), err)
	}
}

// A Param of a field converting its values per schema is converted at
// each run, as a value is.
func TestPrepareSchemaValue(t *testing.T) {
	ctx := ormtest.Open(t, Tickets, OldTickets)
	for _, m := range []*orm.Manager[Ticket]{Tickets, OldTickets} {
		for _, s := range []TicketStatus{TicketOpen, TicketClosed, TicketClosed} {
			if err := m.Create(ctx, &Ticket{Status: s}); err != nil {
				t.Fatal(err)
			}
		}
		byStatus := m.Filter(orm.Q{"status": orm.Param(0)}).Prepare1[TicketStatus]()
		for s, want := range map[TicketStatus]int{TicketOpen: 1, TicketClosed: 2} {
			rows, err := byStatus.All(ctx, s)
			if err != nil || len(rows) != want {
				t.Fatalf("%s: %v, %v", s, rows, err)
			}
		}
	}
}

func TestPrepareRelated(t *testing.T) {
	ctx := ormtest.Open(t, BenchAuthors, BenchPosts, BenchTags)
	a := &benchAuthor{Name: "ali"}
	if err := BenchAuthors.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"one", "two"} {
		if err := BenchPosts.Create(ctx, &benchPost{Title: title, AuthorID: a.ID}); err != nil {
			t.Fatal(err)
		}
	}
	posts := BenchPosts.SelectRelated("author").Filter(orm.Q{"author_id": orm.Param(0)}).OrderBy("id").Prepare()
	rows, err := posts.All(ctx, a.ID)
	if err != nil || len(rows) != 2 || rows[0].Author == nil || rows[0].Author.Name != "ali" {
		t.Fatalf("SelectRelated = %+v, %v", rows, err)
	}
	authors := BenchAuthors.PrefetchRelated("posts").Filter(orm.Q{"name": orm.Param(0)}).Prepare()
	got, err := authors.Get(ctx, "ali")
	if err != nil || len(got.Posts) != 2 {
		t.Fatalf("PrefetchRelated = %+v, %v", got, err)
	}
}

func TestPrepareErrors(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	for _, c := range []struct {
		name string
		run  func() error
		want string
	}{
		{"too few", func() error { _, err := userByEmail.All(ctx); return err },
			"orm: the prepared query takes 1 argument(s), got 0"},
		{"too many", func() error { _, err := userByEmail.All(ctx, "a", "b"); return err },
			"orm: the prepared query takes 1 argument(s), got 2"},
		{"a gap", func() error {
			_, err := Users.Filter(orm.Q{"age": orm.Param(1)}).Prepare().All(ctx, 1, 2)
			return err
		}, "orm: the prepared query has orm.Param(1) but no orm.Param(0): number its arguments from 0"},
		{"in", func() error {
			_, err := Users.Filter(orm.Q{"id__in": orm.Param(0)}).Prepare().All(ctx, []int{1})
			return err
		}, "orm: id__in: orm.Param(0) stands for one value, and in writes its SQL around the value: build that query per call"},
		{"isnull", func() error {
			_, err := Users.Filter(orm.Q{"bio__isnull": orm.Param(0)}).Prepare().All(ctx, true)
			return err
		}, "orm: bio__isnull: orm.Param(0) stands for one value, and isnull writes its SQL around the value: build that query per call"},
		{"a Param given", func() error { _, err := userByEmail.All(ctx, orm.Param(0)); return err },
			"orm: argument 0 of the prepared query is an orm.Param: give it the value"},
		{"without Prepare", func() error { _, err := Users.Filter(orm.Q{"email": orm.Param(0)}).All(ctx); return err },
			"orm.Param(0) is an argument of a prepared query: run the query through Prepare"},
		{"a missing field", func() error { _, err := Users.Filter(orm.Q{"nope": orm.Param(0)}).Prepare().All(ctx, 1); return err },
			`"nope"`},
		{"zero", func() error { _, err := orm.Prepared[User]{}.All(ctx); return err },
			"orm: a zero Prepared: make one with QuerySet.Prepare"},
	} {
		err := c.run()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := userByEmail.Err(orm.DialectFor("postgres")); err != nil {
		t.Fatal(err)
	}
}

// One Prepared runs from many goroutines at once, and on several kinds of
// database: each writes its own SQL once.
func TestPrepareConcurrent(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	p := Users.Filter(orm.Q{"email": orm.Param(0)}).Prepare()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			email := []string{"ali@mail.com", "neema@mail.com"}[i%2]
			for range 20 {
				u, err := p.Get(ctx, email)
				if err != nil || u.Email != email {
					t.Errorf("Get(%q) = %+v, %v", email, u, err)
					return
				}
			}
		})
	}
	wg.Wait()
	for _, d := range []string{"postgres", "mysql", "sqlite"} {
		if err := p.Err(orm.DialectFor(d)); err != nil {
			t.Fatal(d, err)
		}
	}
}

// Observers see each run's statement with its own arguments.
func TestPrepareObserved(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	var args [][]any
	ctx = orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) { args = append(args, q.Args) })
	for _, e := range []string{"ali@mail.com", "juma@mail.com"} {
		if _, err := userByEmail.Get(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(args) != 2 || args[0][0] != "ali@mail.com" || args[1][0] != "juma@mail.com" {
		t.Fatalf("observed %v", args)
	}
}
