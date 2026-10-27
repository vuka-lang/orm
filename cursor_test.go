package orm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
)

// pages reads every page of qs, size n, forwards then backwards, and the
// names each holds.
func pageNames(t *testing.T, ctx context.Context, qs orm.QuerySet[User], n int) (fwd, back [][]string) {
	t.Helper()
	after := ""
	for range 10 {
		rows, cursors, more, err := qs.CursorPage(ctx, after, "", n, 0)
		if err != nil {
			t.Fatal(err)
		}
		fwd = append(fwd, names(rows))
		if !more {
			break
		}
		after = cursors[len(cursors)-1]
	}
	before := ""
	for range 10 {
		rows, cursors, more, err := qs.CursorPage(ctx, "", before, 0, n)
		if err != nil {
			t.Fatal(err)
		}
		back = append(back, names(rows))
		if !more {
			break
		}
		before = cursors[0]
	}
	return fwd, back
}

func TestCursorPages(t *testing.T) {
	ctx := open(t)
	seed(t, ctx) // Ali 25, Neema 17, Juma 32, Amina 41; Ali alone has a bio
	for _, c := range []struct {
		name      string
		qs        orm.QuerySet[User]
		fwd, back string
		keyset    bool
	}{
		{"by the primary key", Users.QuerySet, "[[Ali Neema] [Juma Amina]]", "[[Juma Amina] [Ali Neema]]", true},
		{"by age, descending", Users.OrderBy("-age"), "[[Amina Juma] [Ali Neema]]", "[[Ali Neema] [Amina Juma]]", true},
		{"filtered, by name", Users.Filter(orm.Q{"age__gt": 18}).OrderBy("name"), "[[Ali Amina] [Juma]]", "[[Amina Juma] [Ali]]", true},
		{"by a nullable column: offsets", Users.OrderBy("bio", "id"), "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stmts := captureSQL(t)
			fwd, back := pageNames(t, ctx, c.qs, 2)
			if c.fwd != "" && (fmtList(fwd) != c.fwd || fmtList(back) != c.back) {
				t.Errorf("forwards %s, backwards %s; want %s and %s", fmtList(fwd), fmtList(back), c.fwd, c.back)
			}
			if !c.keyset {
				all, _ := c.qs.All(ctx)
				var got []string
				for _, p := range fwd {
					got = append(got, p...)
				}
				if !slices.Equal(got, names(all)) {
					t.Errorf("offset pages %v, want %v", fwd, names(all))
				}
			}
			sql := strings.Join(stmts(), "\n")
			if c.keyset == strings.Contains(sql, "OFFSET") {
				t.Errorf("keyset %v, but the SQL:\n%s", c.keyset, sql)
			}
		})
	}
}

func TestCursorRefused(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	_, cursors, _, err := Users.OrderBy("-age").CursorPage(ctx, "", "", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Users.OrderBy("name").CursorPage(ctx, cursors[0], "", 2, 0); !errors.Is(err, orm.ErrCursor) {
		t.Errorf("a cursor of another ordering: %v", err)
	}
	if _, _, _, err := Users.CursorPage(ctx, "nonsense!", "", 2, 0); !errors.Is(err, orm.ErrCursor) {
		t.Errorf("a malformed cursor: %v", err)
	}
	if _, _, _, err := Users.CursorPage(ctx, "", "", 2, 2); err == nil {
		t.Error("first and last together")
	}
}

// captureSQL records the statements run from here to the test's end.
func captureSQL(t *testing.T) func() []string {
	var mu sync.Mutex
	var out []string
	stop := orm.Observe(func(ctx context.Context, q orm.QueryInfo) {
		mu.Lock()
		out = append(out, q.SQL)
		mu.Unlock()
	})
	t.Cleanup(stop)
	return func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(out) }
}

func fmtList(pages [][]string) string {
	var b strings.Builder
	b.WriteString("[")
	for i, p := range pages {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("[" + strings.Join(p, " ") + "]")
	}
	return b.String() + "]"
}

func TestLoadRelated(t *testing.T) {
	ctx := relOpen(t)
	books, err := Books.OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ptrs := make([]*Book, len(books))
	for i := range books {
		ptrs[i] = &books[i]
	}
	ctx, count := ormtest.CountQueries(ctx)
	if err := orm.LoadRelation(ctx, ptrs, "Author"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d queries for the books' authors, want 1", n)
	}
	var got []string
	for _, b := range books {
		got = append(got, b.Author.Name)
	}
	if !slices.Equal(got, []string{"Ali", "Ali", "Neema"}) {
		t.Errorf("authors %v", got)
	}
	authors, _ := Authors.OrderBy("id").All(ctx)
	aptrs := []*Author{&authors[0], &authors[1], &authors[2]}
	if err := orm.LoadRelated(ctx, aptrs, "books"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Errorf("%d queries in all, want 3: the authors, then their books", n)
	}
	if len(authors[0].Books) != 2 || len(authors[1].Books) != 1 || authors[2].Books == nil || len(authors[2].Books) != 0 {
		t.Errorf("books: %d %d %v", len(authors[0].Books), len(authors[1].Books), authors[2].Books)
	}
}
