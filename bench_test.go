package orm_test

// The benchmarks set the ORM beside database/sql by hand, the same
// queries on the same pool: what its SQL building, reflection and
// bookkeeping cost over the driver. In-memory SQLite by default; a real
// server with ORMTEST_DRIVER and ORMTEST_DSN, as the tests take them:
//
//	go test -run '^$' -bench . -benchmem
//	ORMTEST_DRIVER=postgres ORMTEST_DSN=postgres://…/orm_bench?sslmode=disable go test -run '^$' -bench .
//
// Postgres and MySQL pools take the db layer's defaults (100 open, 10
// idle) unless a benchmark says otherwise.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

type benchUser struct {
	ID        int64 `gorm:"primaryKey"`
	CreatedAt time.Time
	Name      string
	Email     string
	Age       int
	Active    bool
}

func (benchUser) TableName() string { return "bench_users" }

// benchUserR is benchUser read by reflection: only orm.Of names it, so
// ormgen writes it no scanner.
type benchUserR benchUser

func (benchUserR) TableName() string { return "bench_users" }

// WideBase is embedded by the wide model: its columns are flattened.
type WideBase struct {
	ID        int64 `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// benchWide is a 23-column row, five of them nullable.
type benchWide struct {
	WideBase
	Name, Email, Phone, City, Country, Street, Zip string
	Age, Score, Visits                             int
	Balance, Rating                                float64
	Active, Verified, Admin                        bool
	Bio, Nick, Notes                               *string
	LastSeen                                       *time.Time
	Referrer                                       *int64
}

func (benchWide) TableName() string { return "bench_wides" }

type benchWideR benchWide

func (benchWideR) TableName() string { return "bench_wides" }

type benchAuthor struct {
	ID    int64
	Name  string
	Posts []benchPost `gorm:"foreignKey:AuthorID"`
}

func (benchAuthor) TableName() string { return "bench_authors" }

type benchPost struct {
	ID       int64
	Title    string
	AuthorID int64
	Author   *benchAuthor `gorm:"foreignKey:AuthorID"`
	Tags     []benchTag   `gorm:"many2many:bench_post_tags"`
}

func (benchPost) TableName() string { return "bench_posts" }

// benchTag declares no posts: bench_posts is its inverse of
// benchPost.Tags.
type benchTag struct {
	ID   int64
	Name string
}

func (benchTag) TableName() string { return "bench_tags" }

// benchItem is an orm.Model: its rows save and refresh themselves.
type benchItem struct {
	orm.Model[benchItem]
	ID   int64
	Name string
	Qty  int
}

var (
	BenchUsers   = orm.For[benchUser]()
	BenchWides   = orm.For[benchWide]()
	BenchAuthors = orm.For[benchAuthor]()
	BenchPosts   = orm.For[benchPost]()
	BenchTags    = orm.For[benchTag]()

	benchUsersR = orm.Of[benchUserR](orm.Schema{})
	benchWidesR = orm.Of[benchWideR](orm.Schema{})
	benchAlt    = orm.Schema{Names: "alt"}
)

type benchEnv struct {
	ctx    context.Context
	sql    *sql.DB
	driver string
}

// benchOpen is a fresh database (a schema of its own on Postgres, a
// database on MySQL) with the bench tables and users rows.
func benchOpen(b *testing.B, users int) benchEnv {
	b.Helper()
	ctx := ormtest.Open(b, BenchUsers, BenchWides, BenchAuthors, BenchPosts, BenchTags, orm.Objects[benchItem]())
	d, _ := orm.DBFrom(ctx)
	s := d.SQL()
	e := benchEnv{ctx: ctx, sql: s, driver: ormtest.Driver()}
	if e.driver != "sqlite" {
		s.SetMaxOpenConns(100)
		s.SetMaxIdleConns(10)
		s.SetConnMaxLifetime(time.Hour)
		s.SetConnMaxIdleTime(30 * time.Minute)
	}
	if users > 0 {
		batch := make([]*benchUser, users)
		for i := range batch {
			batch[i] = &benchUser{Name: fmt.Sprint("user", i), Email: fmt.Sprint("u", i, "@mail.com"), Age: 18 + i%50, Active: i%2 == 0}
		}
		if err := BenchUsers.BulkCreate(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
	return e
}

// q is SQL written with ? in the dialect's placeholders.
func (e benchEnv) q(s string) string {
	if e.driver != "postgres" {
		return s
	}
	var out strings.Builder
	n := 0
	for _, c := range s {
		if c == '?' {
			n++
			out.WriteString("$" + strconv.Itoa(n))
			continue
		}
		out.WriteRune(c)
	}
	return out.String()
}

const userCols = `id, created_at, name, email, age, active`

func scanUsers(b *testing.B, rows *sql.Rows, want int) []benchUser {
	defer rows.Close()
	out := make([]benchUser, 0, want)
	for rows.Next() {
		var u benchUser
		if err := rows.Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active); err != nil {
			b.Fatal(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	if len(out) != want {
		b.Fatal(len(out), "rows, want", want)
	}
	return out
}

func check(b *testing.B, err error) {
	if err != nil {
		b.Fatal(err)
	}
}

func BenchmarkGetByID(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE id = ?`)
		for i := 0; b.Loop(); i++ {
			var u benchUser
			check(b, e.sql.QueryRowContext(e.ctx, s, 1+i%1000).Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active))
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1 + i%1000})
			check(b, err)
		}
	})
	b.Run("orm-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := benchUsersR.Get(e.ctx, orm.Q{"id": 1 + i%1000})
			check(b, err)
		}
	})
}

func BenchmarkFilter100(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE age >= ? ORDER BY id LIMIT 100`)
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, s, 20)
			check(b, err)
			scanUsers(b, rows, 100)
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Filter(orm.Q{"age__gte": 20}).OrderBy("id").Limit(100).All(e.ctx)
			if err != nil || len(out) != 100 {
				b.Fatal(len(out), err)
			}
		}
	})
}

func BenchmarkSelect1000(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, `SELECT `+userCols+` FROM bench_users`)
			check(b, err)
			scanUsers(b, rows, 1000)
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("orm-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := benchUsersR.All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("orm-iter", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			n := 0
			for _, err := range BenchUsers.Iter(e.ctx) {
				check(b, err)
				n++
			}
			if n != 1000 {
				b.Fatal(n)
			}
		}
	})
}

func BenchmarkCount(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`SELECT COUNT(*) FROM bench_users WHERE age >= ? AND active = ?`)
		for b.Loop() {
			var n int64
			check(b, e.sql.QueryRowContext(e.ctx, s, 30, true).Scan(&n))
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true}).Count(e.ctx)
			check(b, err)
		}
	})
}

func BenchmarkFilterCount(b *testing.B) {
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true, "name__icontains": "user1"}).Count(e.ctx)
			check(b, err)
		}
	})
}

func BenchmarkInsert(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 0)
		s := e.q(`INSERT INTO bench_users (created_at, name, email, age, active) VALUES (?, ?, ?, ?, ?)`)
		for i := 0; b.Loop(); i++ {
			_, err := e.sql.ExecContext(e.ctx, s, time.Now().UTC(), "ali", strconv.Itoa(i), 25, true)
			check(b, err)
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			u := benchUser{Name: "ali", Email: strconv.Itoa(i), Age: 25, Active: true}
			check(b, BenchUsers.Create(e.ctx, &u))
		}
	})
}

func BenchmarkBulkInsert1000(b *testing.B) {
	rows := func() []*benchUser {
		out := make([]*benchUser, 1000)
		for i := range out {
			out[i] = &benchUser{Name: "ali", Email: strconv.Itoa(i), Age: 25}
		}
		return out
	}
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 0)
		tuple := "(?, ?, ?, ?, ?)"
		s := e.q(`INSERT INTO bench_users (created_at, name, email, age, active) VALUES ` + strings.TrimSuffix(strings.Repeat(tuple+", ", 500), ", "))
		args := make([]any, 0, 2500)
		for b.Loop() {
			for _, half := range [][]*benchUser{rows()[:500], rows()[500:]} {
				args = args[:0]
				now := time.Now().UTC()
				for _, u := range half {
					args = append(args, now, u.Name, u.Email, u.Age, u.Active)
				}
				_, err := e.sql.ExecContext(e.ctx, s, args...)
				check(b, err)
			}
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for b.Loop() {
			check(b, BenchUsers.BulkCreate(e.ctx, rows()))
		}
	})
}

func BenchmarkUpdate(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`UPDATE bench_users SET age = ?, name = ? WHERE id = ?`)
		for i := 0; b.Loop(); i++ {
			_, err := e.sql.ExecContext(e.ctx, s, i%90, "renamed", 1+i%1000)
			check(b, err)
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Filter(orm.Q{"id": 1 + i%1000}).Update(e.ctx, orm.Set{"age": i % 90, "name": "renamed"})
			check(b, err)
		}
	})
}

// BenchmarkSave writes every field of a row read before.
func BenchmarkSave(b *testing.B) {
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1)
		u, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1})
		check(b, err)
		for i := 0; b.Loop(); i++ {
			u.Age = i % 90
			check(b, BenchUsers.Save(e.ctx, &u))
		}
	})
}

// BenchmarkDelete deletes by key a row that isn't there: the statement's
// cost without the table shrinking under the loop.
func BenchmarkDelete(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`DELETE FROM bench_users WHERE id = ?`)
		for i := 0; b.Loop(); i++ {
			_, err := e.sql.ExecContext(e.ctx, s, -1-i)
			check(b, err)
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Filter(orm.Q{"id": -1 - i}).Delete(e.ctx)
			check(b, err)
		}
	})
}

func seedWide(b *testing.B, e benchEnv, n int) {
	rows := make([]*benchWide, n)
	for i := range rows {
		w := &benchWide{Name: "n", Email: "e", Phone: "p", City: "c", Country: "tz", Street: "s", Zip: "z",
			Age: i % 80, Score: i, Visits: i * 2, Balance: float64(i) / 3, Rating: 4.5, Active: true, Admin: i%10 == 0}
		if i%2 == 0 {
			bio, nick, ref, seen := "bio", "nick", int64(i), time.Now()
			w.Bio, w.Nick, w.Referrer, w.LastSeen = &bio, &nick, &ref, &seen
		}
		rows[i] = w
	}
	check(b, BenchWides.BulkCreate(e.ctx, rows))
}

// BenchmarkScanWide1000 reads 1000 rows of 23 columns, an embedded
// struct's and five nullable ones among them, half of those NULL.
func BenchmarkScanWide1000(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 0)
		seedWide(b, e, 1000)
		cols := strings.Join(BenchWides.Columns(), ", ")
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, `SELECT `+cols+` FROM bench_wides`)
			check(b, err)
			out := make([]benchWide, 0, 1000)
			for rows.Next() {
				var w benchWide
				check(b, rows.Scan(&w.ID, &w.CreatedAt, &w.UpdatedAt, &w.Name, &w.Email, &w.Phone, &w.City, &w.Country, &w.Street, &w.Zip,
					&w.Age, &w.Score, &w.Visits, &w.Balance, &w.Rating, &w.Active, &w.Verified, &w.Admin, &w.Bio, &w.Nick, &w.Notes, &w.LastSeen, &w.Referrer))
				out = append(out, w)
			}
			rows.Close()
			if len(out) != 1000 {
				b.Fatal(len(out))
			}
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		seedWide(b, e, 1000)
		for b.Loop() {
			out, err := BenchWides.All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("orm-reflect", func(b *testing.B) {
		e := benchOpen(b, 0)
		seedWide(b, e, 1000)
		for b.Loop() {
			out, err := benchWidesR.All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
}

// seedRel is 100 authors of 10 posts each, 20 tags, 3 tags a post.
func seedRel(b *testing.B, e benchEnv) {
	authors := make([]*benchAuthor, 100)
	for i := range authors {
		authors[i] = &benchAuthor{Name: fmt.Sprint("author", i)}
	}
	check(b, BenchAuthors.BulkCreate(e.ctx, authors))
	var posts []*benchPost
	for _, a := range authors {
		for j := range 10 {
			posts = append(posts, &benchPost{Title: fmt.Sprint("post ", a.ID, "-", j), AuthorID: a.ID})
		}
	}
	check(b, BenchPosts.BulkCreate(e.ctx, posts))
	tags := make([]*benchTag, 20)
	for i := range tags {
		tags[i] = &benchTag{Name: fmt.Sprint("tag", i)}
	}
	check(b, BenchTags.BulkCreate(e.ctx, tags))
	var vals []string
	var args []any
	for i, p := range posts {
		for j := range 3 {
			vals = append(vals, "(?, ?)")
			args = append(args, p.ID, tags[(i+j)%20].ID)
		}
	}
	for len(vals) > 0 {
		n := min(len(vals), 500)
		_, err := e.sql.ExecContext(e.ctx, e.q(`INSERT INTO bench_post_tags (bench_post_id, bench_tag_id) VALUES `+strings.Join(vals[:n], ", ")), args[:2*n]...)
		check(b, err)
		vals, args = vals[n:], args[2*n:]
	}
}

// BenchmarkPostsWithAuthor reads 100 posts and each one's author.
func BenchmarkPostsWithAuthor(b *testing.B) {
	run := func(name string, f func(b *testing.B, e benchEnv)) {
		b.Run(name, func(b *testing.B) {
			e := benchOpen(b, 0)
			seedRel(b, e)
			for b.Loop() {
				f(b, e)
			}
		})
	}
	want := func(b *testing.B, ps []benchPost, err error) {
		if err != nil || len(ps) != 100 || ps[99].Author == nil {
			b.Fatal(len(ps), err)
		}
	}
	run("sql-join", func(b *testing.B, e benchEnv) {
		rows, err := e.sql.QueryContext(e.ctx, `SELECT p.id, p.title, p.author_id, a.id, a.name FROM bench_posts p LEFT JOIN bench_authors a ON a.id = p.author_id ORDER BY p.id LIMIT 100`)
		check(b, err)
		var out []benchPost
		for rows.Next() {
			var p benchPost
			a := &benchAuthor{}
			check(b, rows.Scan(&p.ID, &p.Title, &p.AuthorID, &a.ID, &a.Name))
			p.Author = a
			out = append(out, p)
		}
		rows.Close()
		want(b, out, nil)
	})
	run("select_related", func(b *testing.B, e benchEnv) {
		ps, err := BenchPosts.SelectRelated("author").OrderBy("id").Limit(100).All(e.ctx)
		want(b, ps, err)
	})
	run("prefetch_related", func(b *testing.B, e benchEnv) {
		ps, err := BenchPosts.PrefetchRelated("author").OrderBy("id").Limit(100).All(e.ctx)
		want(b, ps, err)
	})
	run("n+1", func(b *testing.B, e benchEnv) {
		ps, err := BenchPosts.OrderBy("id").Limit(100).All(e.ctx)
		check(b, err)
		for i := range ps {
			a, err := BenchAuthors.Get(e.ctx, orm.Q{"id": ps[i].AuthorID})
			check(b, err)
			ps[i].Author = &a
		}
		want(b, ps, nil)
	})
}

// BenchmarkAuthorsWithPosts reads 100 authors and their 10 posts each.
func BenchmarkAuthorsWithPosts(b *testing.B) {
	run := func(name string, f func(b *testing.B, e benchEnv) []benchAuthor) {
		b.Run(name, func(b *testing.B) {
			e := benchOpen(b, 0)
			seedRel(b, e)
			for b.Loop() {
				as := f(b, e)
				if len(as) != 100 || len(as[99].Posts) != 10 {
					b.Fatal(len(as))
				}
			}
		})
	}
	run("prefetch_related", func(b *testing.B, e benchEnv) []benchAuthor {
		as, err := BenchAuthors.PrefetchRelated("posts").All(e.ctx)
		check(b, err)
		return as
	})
	run("n+1", func(b *testing.B, e benchEnv) []benchAuthor {
		as, err := BenchAuthors.All(e.ctx)
		check(b, err)
		for i := range as {
			as[i].Posts, err = BenchPosts.Filter(orm.Q{"author_id": as[i].ID}).All(e.ctx)
			check(b, err)
		}
		return as
	})
}

// BenchmarkPostsWithTags reads 100 posts and their 3 tags each, through
// the table between.
func BenchmarkPostsWithTags(b *testing.B) {
	run := func(name string, f func(b *testing.B, e benchEnv) []benchPost) {
		b.Run(name, func(b *testing.B) {
			e := benchOpen(b, 0)
			seedRel(b, e)
			for b.Loop() {
				ps := f(b, e)
				if len(ps) != 100 || len(ps[99].Tags) != 3 {
					b.Fatal(len(ps))
				}
			}
		})
	}
	run("prefetch_related", func(b *testing.B, e benchEnv) []benchPost {
		ps, err := BenchPosts.PrefetchRelated("tags").OrderBy("id").Limit(100).All(e.ctx)
		check(b, err)
		return ps
	})
}

// BenchmarkInverseFilter filters tags through bench_posts, the inverse of
// benchPost.Tags no field of benchTag declares: an EXISTS through the
// table between.
func BenchmarkInverseFilter(b *testing.B) {
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		seedRel(b, e)
		for b.Loop() {
			_, err := BenchTags.Filter(orm.Q{"bench_posts__title__startswith": "post 1-"}).Count(e.ctx)
			check(b, err)
		}
	})
	b.Run("orm-parallel", func(b *testing.B) {
		e := benchOpen(b, 0)
		seedRel(b, e)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, err := BenchTags.Filter(orm.Q{"bench_posts__title__startswith": "post 1-"}).Count(e.ctx)
				check(b, err)
			}
		})
	})
}

type benchUserRow struct {
	ID   int64
	Name string
	Age  int
}

func BenchmarkValues(b *testing.B) {
	b.Run("sql-scalar", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, `SELECT name FROM bench_users`)
			check(b, err)
			out := make([]string, 0, 1000)
			for rows.Next() {
				var s string
				check(b, rows.Scan(&s))
				out = append(out, s)
			}
			rows.Close()
		}
	})
	b.Run("scalar", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Values[string]("name").All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("struct", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Values[benchUserRow]().All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("map", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Values[map[string]any]("id", "name", "age").All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("list", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Values[[]any]("id", "name", "age").All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("grouped", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Annotate("n", orm.Count("id")).Values[[]any]("age", "n").All(e.ctx)
			if err != nil || len(out) != 50 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("case", func(b *testing.B) {
		e := benchOpen(b, 1000)
		tier := orm.Case(orm.When(orm.Q{"age__gte": 60}, "senior"), orm.When(orm.Q{"age__gte": 30}, "adult")).Else("young")
		for b.Loop() {
			out, err := BenchUsers.Annotate("tier", tier).Values[string]("tier").All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
}

func BenchmarkRaw(b *testing.B) {
	b.Run("manager", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Raw("SELECT * FROM bench_users WHERE age >= ? ORDER BY id LIMIT 100", 20).All(e.ctx)
			if err != nil || len(out) != 100 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("struct", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := orm.Raw[benchUserRow](e.ctx, "SELECT id, name, age FROM bench_users WHERE age >= ? ORDER BY id LIMIT 100", 20)
			if err != nil || len(out) != 100 {
				b.Fatal(len(out), err)
			}
		}
	})
}

// BenchmarkSchemas: orm.Of under a names set is a cached manager, its
// queries the default's.
func BenchmarkSchemas(b *testing.B) {
	b.Run("of", func(b *testing.B) {
		for b.Loop() {
			_ = orm.Of[benchUser](benchAlt)
		}
	})
	b.Run("objects", func(b *testing.B) {
		for b.Loop() {
			_ = orm.Objects[benchItem]()
		}
	})
	b.Run("get-default", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1 + i%1000})
			check(b, err)
		}
	})
	b.Run("get-names", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := orm.Of[benchUser](benchAlt).Get(e.ctx, orm.Q{"id": 1 + i%1000})
			check(b, err)
		}
	})
}

// BenchmarkModel is an orm.Model's own writes beside its manager's.
func BenchmarkModel(b *testing.B) {
	items := orm.Objects[benchItem]()
	b.Run("save-insert", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			it := benchItem{Name: "pen", Qty: i}
			check(b, it.Save(e.ctx))
		}
	})
	b.Run("manager-create", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			check(b, items.Create(e.ctx, &benchItem{Name: "pen", Qty: i}))
		}
	})
	b.Run("save-update", func(b *testing.B) {
		e := benchOpen(b, 0)
		it := benchItem{Name: "pen"}
		check(b, it.Save(e.ctx))
		for i := 0; b.Loop(); i++ {
			it.Qty = i
			check(b, it.Save(e.ctx))
		}
	})
	b.Run("manager-save", func(b *testing.B) {
		e := benchOpen(b, 0)
		it := benchItem{Name: "pen"}
		check(b, items.Create(e.ctx, &it))
		for i := 0; b.Loop(); i++ {
			it.Qty = i
			check(b, items.Save(e.ctx, &it))
		}
	})
	b.Run("refresh", func(b *testing.B) {
		e := benchOpen(b, 0)
		it := benchItem{Name: "pen"}
		check(b, it.Save(e.ctx))
		for b.Loop() {
			check(b, it.Refresh(e.ctx))
		}
	})
	b.Run("manager-get", func(b *testing.B) {
		e := benchOpen(b, 0)
		it := benchItem{Name: "pen"}
		check(b, items.Create(e.ctx, &it))
		for b.Loop() {
			_, err := items.Get(e.ctx, orm.Q{"id": it.ID})
			check(b, err)
		}
	})
}

// BenchmarkBuild is the ORM alone: a QuerySet chained, no database.
func BenchmarkBuild(b *testing.B) {
	for b.Loop() {
		q := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true}).Exclude(orm.Q{"name__icontains": "x"}).OrderBy("-age", "name").Limit(20)
		_ = q
	}
}

// BenchmarkParallelGet is GetByID from every core at once: the locks of
// the metadata caches, the Databases lookup, and an observer.
func BenchmarkParallelGet(b *testing.B) {
	var id atomic.Int64
	next := func() int64 { return 1 + id.Add(1)%1000 }
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE id = ?`)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				var u benchUser
				check(b, e.sql.QueryRowContext(e.ctx, s, next()).Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active))
			}
		})
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, err := BenchUsers.Get(e.ctx, orm.Q{"id": next()})
				check(b, err)
			}
		})
	})
	b.Run("orm-observed", func(b *testing.B) {
		e := benchOpen(b, 1000)
		b.RunParallel(func(pb *testing.PB) {
			ctx := orm.WithObserver(e.ctx, func(context.Context, orm.QueryInfo) {})
			for pb.Next() {
				_, err := BenchUsers.Get(ctx, orm.Q{"id": next()})
				check(b, err)
			}
		})
	})
	for _, dflt := range []bool{true, false} {
		name := "orm-bound-default"
		if !dflt {
			name = "orm-bound-only"
		}
		b.Run(name, func(b *testing.B) {
			ctx := benchApp(b, dflt)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, err := BenchUsers.Get(ctx, orm.Q{"id": next()})
					check(b, err)
				}
			})
		})
	}
}

// benchApp is a context finding a database (the default one, or its only
// one) in a Databases of its own, with 1000 users in it, as a request's or
// a job's: no WithDB.
func benchApp(b *testing.B, asDefault bool) context.Context {
	b.Helper()
	dialect, dsn := ormtest.Driver(), os.Getenv("ORMTEST_DSN")
	if dialect == "sqlite" {
		dsn = filepath.Join(b.TempDir(), "bench.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	d, err := orm.Connect(dialect, dsn)
	check(b, err)
	b.Cleanup(func() { _ = d.SQL().Close() })
	var opts []orm.DatabaseOption
	if asDefault {
		opts = append(opts, orm.AsDefault())
	}
	dbs := orm.NewDatabases()
	dbs.Add("main", d, opts...)
	ctx := orm.WithDatabases(context.Background(), dbs)
	setup := orm.WithDB(ctx, d)
	_, _ = orm.Exec(setup, "DROP TABLE IF EXISTS bench_users")
	check(b, orm.CreateTables(setup, BenchUsers))
	b.Cleanup(func() { _, _ = orm.Exec(setup, "DROP TABLE IF EXISTS bench_users") })
	batch := make([]*benchUser, 1000)
	for i := range batch {
		batch[i] = &benchUser{Name: fmt.Sprint("user", i), Email: fmt.Sprint("u", i), Age: 18 + i%50}
	}
	check(b, BenchUsers.BulkCreate(setup, batch))
	return ctx
}

// BenchmarkPool is GetByID from 64 goroutines a core over pools of
// several sizes: the db layer's default is 100 open, 10 idle. waits/op
// is how often a query waited for a connection, closed/op how often one
// was closed for want of an idle slot (and a new one dialled).
func BenchmarkPool(b *testing.B) {
	if ormtest.Driver() == "sqlite" {
		b.Skip("a server's pool: set ORMTEST_DRIVER")
	}
	for _, p := range []struct{ open, idle int }{{4, 4}, {16, 10}, {16, 16}, {100, 10}, {100, 100}} {
		b.Run(fmt.Sprintf("open=%d,idle=%d", p.open, p.idle), func(b *testing.B) {
			e := benchOpen(b, 1000)
			e.sql.SetMaxOpenConns(p.open)
			e.sql.SetMaxIdleConns(p.idle)
			var id atomic.Int64
			before := e.sql.Stats()
			b.SetParallelism(8)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1 + id.Add(1)%1000})
					check(b, err)
				}
			})
			b.StopTimer()
			st := e.sql.Stats()
			n := float64(b.N)
			b.ReportMetric(float64(st.WaitCount-before.WaitCount)/n, "waits/op")
			b.ReportMetric(float64(st.MaxIdleClosed-before.MaxIdleClosed)/n, "closed/op")
		})
	}
}

// stub is a database/sql driver answering every query with the rows set
// for it, at once: the cost of the ORM and database/sql themselves,
// with no database, and their locks under RunParallel.
type stubDriver struct{}

type stubData struct {
	cols  []string
	rows  [][]driver.Value
	route map[string]*stubData // by a part of the query: rows of their own
}

var stubRowsNow atomic.Pointer[stubData]

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("stub: no prepare") }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return stubConn{}, nil }
func (stubConn) Commit() error                       { return nil }
func (stubConn) Rollback() error                     { return nil }

func (stubConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	d := stubRowsNow.Load()
	for k, r := range d.route {
		if strings.Contains(q, k) {
			return &stubRows{d: r}, nil
		}
	}
	return &stubRows{d: d}, nil
}

func (stubConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type stubRows struct {
	d *stubData
	i int
}

func (r *stubRows) Columns() []string { return r.d.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.d.rows) {
		return io.EOF
	}
	copy(dest, r.d.rows[r.i])
	r.i++
	return nil
}

func init() { sql.Register("ormbench-stub", stubDriver{}) }

// stubOpen is a stub database answering queries with n users, Postgres
// as the ORM speaks it.
func stubOpen(b *testing.B, n int) benchEnv {
	b.Helper()
	s, err := sql.Open("ormbench-stub", "")
	check(b, err)
	s.SetMaxIdleConns(100)
	b.Cleanup(func() { _ = s.Close() })
	stubUsers(n)
	return benchEnv{ctx: orm.WithDB(context.Background(), orm.Open(s, "postgres")), sql: s, driver: "postgres"}
}

func stubUsers(n int) {
	d := &stubData{cols: []string{"id", "created_at", "name", "email", "age", "active"}}
	now := time.Now()
	for i := range n {
		d.rows = append(d.rows, []driver.Value{int64(i + 1), now, "user" + strconv.Itoa(i), "u" + strconv.Itoa(i) + "@mail.com", int64(18 + i%50), i%2 == 0})
	}
	stubRowsNow.Store(d)
}

func stubRoute(route map[string]*stubData) { stubRowsNow.Store(&stubData{route: route}) }

func stubSet(cols []string, rows ...[]driver.Value) {
	stubRowsNow.Store(&stubData{cols: cols, rows: rows})
}

// BenchmarkOverhead is each layer's own cost per call, on the stub
// driver: no database time.
func BenchmarkOverhead(b *testing.B) {
	b.Run("get/sql", func(b *testing.B) {
		e := stubOpen(b, 1)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE id = ?`)
		for i := 0; b.Loop(); i++ {
			var u benchUser
			check(b, e.sql.QueryRowContext(e.ctx, s, i).Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active))
		}
	})
	b.Run("get/orm", func(b *testing.B) {
		e := stubOpen(b, 1)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Get(e.ctx, orm.Q{"id": i})
			check(b, err)
		}
	})
	b.Run("get/orm-reflect", func(b *testing.B) {
		e := stubOpen(b, 1)
		for i := 0; b.Loop(); i++ {
			_, err := benchUsersR.Get(e.ctx, orm.Q{"id": i})
			check(b, err)
		}
	})
	b.Run("filter/orm", func(b *testing.B) {
		e := stubOpen(b, 1)
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"age__gte": 20, "active": true}, orm.Or(orm.Q{"name__icontains": "a"}, orm.Q{"email__endswith": ".com"})).OrderBy("-age", "id").Limit(10).All(e.ctx)
			check(b, err)
		}
	})
	b.Run("count/sql", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"count"}, []driver.Value{int64(7)})
		s := e.q(`SELECT COUNT(*) FROM bench_users WHERE age >= ? AND active = ?`)
		for b.Loop() {
			var n int64
			check(b, e.sql.QueryRowContext(e.ctx, s, 30, true).Scan(&n))
		}
	})
	b.Run("count/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"count"}, []driver.Value{int64(7)})
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true}).Count(e.ctx)
			check(b, err)
		}
	})
	b.Run("insert/sql", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"id"}, []driver.Value{int64(1)})
		s := e.q(`INSERT INTO bench_users (created_at, name, email, age, active) VALUES (?, ?, ?, ?, ?) RETURNING id`)
		for b.Loop() {
			var id int64
			check(b, e.sql.QueryRowContext(e.ctx, s, time.Now().UTC(), "ali", "a@mail.com", 25, true).Scan(&id))
		}
	})
	b.Run("insert/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"id"}, []driver.Value{int64(1)})
		for b.Loop() {
			check(b, BenchUsers.Create(e.ctx, &benchUser{Name: "ali", Email: "a@mail.com", Age: 25, Active: true}))
		}
	})
	b.Run("bulk1000/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		ids := make([][]driver.Value, 500)
		for i := range ids {
			ids[i] = []driver.Value{int64(i + 1)}
		}
		stubSet([]string{"id"}, ids...)
		rows := make([]*benchUser, 1000)
		for b.Loop() {
			for i := range rows {
				rows[i] = &benchUser{Name: "ali", Email: "a@mail.com", Age: 25}
			}
			check(b, BenchUsers.BulkCreate(e.ctx, rows))
		}
	})
	b.Run("update/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Filter(orm.Q{"id": i}).Update(e.ctx, orm.Set{"age": 3, "name": "renamed"})
			check(b, err)
		}
	})
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("scan%d/sql", n), func(b *testing.B) {
			e := stubOpen(b, n)
			for b.Loop() {
				rows, err := e.sql.QueryContext(e.ctx, `SELECT `+userCols+` FROM bench_users`)
				check(b, err)
				scanUsers(b, rows, n)
			}
		})
		b.Run(fmt.Sprintf("scan%d/orm", n), func(b *testing.B) {
			e := stubOpen(b, n)
			for b.Loop() {
				out, err := BenchUsers.All(e.ctx)
				if err != nil || len(out) != n {
					b.Fatal(len(out), err)
				}
			}
		})
		b.Run(fmt.Sprintf("scan%d/orm-reflect", n), func(b *testing.B) {
			e := stubOpen(b, n)
			for b.Loop() {
				out, err := benchUsersR.All(e.ctx)
				if err != nil || len(out) != n {
					b.Fatal(len(out), err)
				}
			}
		})
	}
	b.Run("values1000/scalar", func(b *testing.B) {
		e := stubOpen(b, 0)
		rows := make([][]driver.Value, 1000)
		for i := range rows {
			rows[i] = []driver.Value{"user" + strconv.Itoa(i)}
		}
		stubSet([]string{"name"}, rows...)
		for b.Loop() {
			_, err := BenchUsers.Values[string]("name").All(e.ctx)
			check(b, err)
		}
	})
	b.Run("values1000/list", func(b *testing.B) {
		e := stubOpen(b, 0)
		rows := make([][]driver.Value, 1000)
		for i := range rows {
			rows[i] = []driver.Value{int64(i), "user" + strconv.Itoa(i), int64(30)}
		}
		stubSet([]string{"id", "name", "age"}, rows...)
		for b.Loop() {
			_, err := BenchUsers.Values[[]any]("id", "name", "age").All(e.ctx)
			check(b, err)
		}
	})
	b.Run("values1000/struct", func(b *testing.B) {
		e := stubOpen(b, 0)
		rows := make([][]driver.Value, 1000)
		for i := range rows {
			rows[i] = []driver.Value{int64(i), "user" + strconv.Itoa(i), int64(30)}
		}
		stubSet([]string{"id", "name", "age"}, rows...)
		for b.Loop() {
			_, err := BenchUsers.Values[benchUserRow]().All(e.ctx)
			check(b, err)
		}
	})
	b.Run("raw1000/manager", func(b *testing.B) {
		e := stubOpen(b, 1000)
		for b.Loop() {
			_, err := BenchUsers.Raw("SELECT * FROM bench_users").All(e.ctx)
			check(b, err)
		}
	})
	b.Run("icontains/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"count"}, []driver.Value{int64(7)})
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"name__icontains": "user1"}).Count(e.ctx)
			check(b, err)
		}
	})
}

// BenchmarkOverheadParallel is Overhead's calls from every core at once,
// on the stub: contention in the ORM's own locks.
func BenchmarkOverheadParallel(b *testing.B) {
	b.Run("get/sql", func(b *testing.B) {
		e := stubOpen(b, 1)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE id = ?`)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				var u benchUser
				check(b, e.sql.QueryRowContext(e.ctx, s, 1).Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active))
			}
		})
	})
	b.Run("get/orm", func(b *testing.B) {
		e := stubOpen(b, 1)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1})
				check(b, err)
			}
		})
	})
	b.Run("get/orm-observed", func(b *testing.B) {
		e := stubOpen(b, 1)
		b.RunParallel(func(pb *testing.PB) {
			ctx := orm.WithObserver(e.ctx, func(context.Context, orm.QueryInfo) {})
			for pb.Next() {
				_, err := BenchUsers.Get(ctx, orm.Q{"id": 1})
				check(b, err)
			}
		})
	})
	b.Run("inverse/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubSet([]string{"count"}, []driver.Value{int64(7)})
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, err := BenchTags.Filter(orm.Q{"bench_posts__title__startswith": "post 1-"}).Count(e.ctx)
				check(b, err)
			}
		})
	})
	b.Run("of/orm", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = orm.Of[benchUser](benchAlt)
			}
		})
	})
	b.Run("objects/orm", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = orm.Objects[benchItem]()
			}
		})
	})
	b.Run("prefetch/orm", func(b *testing.B) {
		e := stubOpen(b, 0)
		stubRoute(map[string]*stubData{
			`FROM "bench_authors"`: {cols: []string{"id", "name"}, rows: [][]driver.Value{{int64(1), "a"}, {int64(2), "b"}}},
			`FROM "bench_posts"`:   {cols: []string{"id", "title", "author_id"}, rows: [][]driver.Value{{int64(1), "p", int64(1)}, {int64(2), "q", int64(2)}}},
		})
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, err := BenchAuthors.PrefetchRelated("posts").All(e.ctx)
				check(b, err)
			}
		})
	})
}
