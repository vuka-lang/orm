package orm_test

import (
	"context"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// BenchmarkCoreOps is what runs on a manager's core, by reflection, where no
// code names a Manager[T] (FK.Get, GetOrCreate, the keys and rows a write hands
// its listeners), beside the QuerySet[T] reads: generated scanner and reflection.
func BenchmarkCoreOps(b *testing.B) {
	listen := func(b *testing.B) {
		b.Cleanup(BenchUsers.OnChange(func(context.Context, orm.Change[benchUser]) {}))
	}
	b.Run("fk-get", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			f := orm.FKID[benchUser](int64(1 + i%1000))
			if r := f.Get(e.ctx); r.IsErr() {
				b.Fatal(r)
			}
		}
	})
	b.Run("fk-get-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			f := orm.FKID[benchUserR](int64(1 + i%1000))
			if r := f.Get(e.ctx); r.IsErr() {
				b.Fatal(r)
			}
		}
	})
	b.Run("get-or-create", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, _, err := BenchUsers.GetOrCreate(e.ctx, orm.Q{"id": 1 + i%1000}, nil)
			check(b, err)
		}
	})
	b.Run("get-or-create-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, _, err := benchUsersR.GetOrCreate(e.ctx, orm.Q{"id": 1 + i%1000}, nil)
			check(b, err)
		}
	})
	b.Run("update100-keys", func(b *testing.B) {
		e := benchOpen(b, 1000)
		listen(b)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Filter(orm.Q{"id__lte": 100}).Update(e.ctx, orm.Set{"age": i})
			check(b, err)
		}
	})
	b.Run("delete100-rows", func(b *testing.B) {
		e := benchOpen(b, 0)
		listen(b)
		for b.Loop() {
			b.StopTimer()
			rows := make([]*benchUser, 100)
			for j := range rows {
				rows[j] = &benchUser{Name: "n", Email: "e"}
			}
			check(b, BenchUsers.BulkCreate(e.ctx, rows))
			b.StartTimer()
			_, err := BenchUsers.Filter(orm.Q{"age": 0}).Delete(e.ctx)
			check(b, err)
		}
	})
	b.Run("all100", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Filter(orm.Q{"id__lte": 100}).All(e.ctx)
			check(b, err)
			if len(out) != 100 {
				b.Fatal(len(out))
			}
		}
	})
	b.Run("all100-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := benchUsersR.Filter(orm.Q{"id__lte": 100}).All(e.ctx)
			check(b, err)
			if len(out) != 100 {
				b.Fatal(len(out))
			}
		}
	})
	b.Run("first", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := BenchUsers.Filter(orm.Q{"id__gte": 1 + i%1000}).First(e.ctx)
			check(b, err)
		}
	})
	b.Run("first-reflect", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			_, err := benchUsersR.Filter(orm.Q{"id__gte": 1 + i%1000}).First(e.ctx)
			check(b, err)
		}
	})
}
