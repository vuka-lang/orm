package orm_test

import (
	"database/sql/driver"
	"strconv"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
)

// BenchmarkFallback is the same reads and writes through the generated
// scanners and by reflection (no orm generate files): stub is the ORM's
// own cost, with no database; sqlite includes the database's.
func BenchmarkFallback(b *testing.B) {
	for _, mode := range []string{"generated", "reflect"} {
		run := func(name string, f func(b *testing.B)) {
			b.Run(name+"/"+mode, func(b *testing.B) {
				orm.SetReflectOnly(mode == "reflect")
				defer orm.SetReflectOnly(false)
				f(b)
			})
		}
		run("stub/get", func(b *testing.B) {
			e := stubOpen(b, 1)
			for i := 0; b.Loop(); i++ {
				_, err := BenchUsers.Get(e.ctx, orm.Q{"id": i})
				check(b, err)
			}
		})
		run("stub/scan1000", func(b *testing.B) {
			e := stubOpen(b, 1000)
			for b.Loop() {
				out, err := BenchUsers.All(e.ctx)
				if err != nil || len(out) != 1000 {
					b.Fatal(len(out), err)
				}
			}
		})
		run("stub/wide1000", func(b *testing.B) {
			e := stubOpen(b, 0)
			stubWides(1000)
			for b.Loop() {
				out, err := BenchWides.All(e.ctx)
				if err != nil || len(out) != 1000 {
					b.Fatal(len(out), err)
				}
			}
		})
		run("stub/bulk1000", func(b *testing.B) {
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
		run("sqlite/scan1000", func(b *testing.B) {
			e := benchOpen(b, 1000)
			for b.Loop() {
				out, err := BenchUsers.All(e.ctx)
				if err != nil || len(out) != 1000 {
					b.Fatal(len(out), err)
				}
			}
		})
		run("sqlite/wide1000", func(b *testing.B) {
			e := benchOpen(b, 0)
			seedWide(b, e, 1000)
			for b.Loop() {
				out, err := BenchWides.All(e.ctx)
				if err != nil || len(out) != 1000 {
					b.Fatal(len(out), err)
				}
			}
		})
	}
}

// stubWides answers queries with n bench_wides rows.
func stubWides(n int) {
	cols := BenchWides.Columns()
	now := time.Now()
	rows := make([][]driver.Value, n)
	for i := range rows {
		s := "v" + strconv.Itoa(i)
		r := make([]driver.Value, len(cols))
		for j, c := range cols {
			switch c {
			case "id", "age", "score", "visits":
				r[j] = int64(i)
			case "balance", "rating":
				r[j] = float64(i) / 3
			case "active", "verified", "admin":
				r[j] = i%2 == 0
			case "created_at", "updated_at", "last_seen":
				r[j] = now
			case "bio", "nick", "notes", "referrer":
				switch {
				case i%3 == 0:
					r[j] = nil
				case c == "referrer":
					r[j] = int64(i)
				default:
					r[j] = s
				}
			default:
				r[j] = s
			}
		}
		rows[i] = r
	}
	stubSet(cols, rows...)
}
