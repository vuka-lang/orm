package orm_test

import (
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// BenchmarkPrepared is the same queries by hand, through a QuerySet and
// through Prepare, on the stub driver: what writing the SQL per call
// costs, and what a prepared query leaves of it.
func BenchmarkPrepared(b *testing.B) {
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
	b.Run("get/prepared", func(b *testing.B) {
		e := stubOpen(b, 1)
		p := BenchUsers.Filter(orm.Q{"id": orm.Param(0)}).Prepare()
		for i := 0; b.Loop(); i++ {
			_, err := p.Get(e.ctx, i)
			check(b, err)
		}
	})
	b.Run("get/prepared-typed", func(b *testing.B) {
		e := stubOpen(b, 1)
		p := BenchUsers.Filter(orm.Q{"id": orm.Param(0)}).Prepare1[int]()
		for i := 0; b.Loop(); i++ {
			_, err := p.Get(e.ctx, i)
			check(b, err)
		}
	})
	b.Run("get/prepared-reflect", func(b *testing.B) {
		e := stubOpen(b, 1)
		p := benchUsersR.Filter(orm.Q{"id": orm.Param(0)}).Prepare()
		for i := 0; b.Loop(); i++ {
			_, err := p.Get(e.ctx, i)
			check(b, err)
		}
	})
	b.Run("filter/sql", func(b *testing.B) {
		e := stubOpen(b, 1)
		s := e.q(`SELECT ` + userCols + ` FROM bench_users WHERE age >= ? AND active = ? AND (name ILIKE ? OR email LIKE ?) ORDER BY age DESC, id LIMIT 10`)
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, s, 20, true, "%a%", "%.com")
			check(b, err)
			scanUsers(b, rows, 1)
		}
	})
	b.Run("filter/orm", func(b *testing.B) {
		e := stubOpen(b, 1)
		for b.Loop() {
			_, err := BenchUsers.Filter(orm.Q{"age__gte": 20, "active": true}, orm.Or(orm.Q{"name__icontains": "a"}, orm.Q{"email__endswith": ".com"})).OrderBy("-age", "id").Limit(10).All(e.ctx)
			check(b, err)
		}
	})
	b.Run("filter/prepared", func(b *testing.B) {
		e := stubOpen(b, 1)
		p := BenchUsers.Filter(orm.Q{"age__gte": orm.Param(0), "active": orm.Param(1)}, orm.Or(orm.Q{"name__icontains": orm.Param(2)}, orm.Q{"email__endswith": orm.Param(3)})).OrderBy("-age", "id").Limit(10).Prepare()
		for b.Loop() {
			_, err := p.All(e.ctx, 20, true, "a", ".com")
			check(b, err)
		}
	})
}
