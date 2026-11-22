// Package same is package shop's models with the related model beside
// them: the SQL their queries make is the split's.
package same

import "github.com/vuka-lang/orm/v2"

type User struct {
	orm.Model[User]
	ID    int64
	Name  string
	Email string
}

func (u *User) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "users"})
}

type Order struct {
	orm.Model[Order]
	ID       int64
	Total    int64
	Customer orm.FK[User]
}

func (o *Order) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "orders"})
}

// ByEmail is shop.ByEmail on the models of one package.
func ByEmail(email string) orm.QuerySet[Order] {
	return orm.For[Order]().Filter(OrderColumns.Customer().Email.Eq(email))
}

// Large is report.Large on the models of one package.
func Large(name string, total int64) orm.QuerySet[Order] {
	return orm.For[Order]().Filter(OrderColumns.Customer().Name.Eq(name), OrderColumns.Total.Gt(total))
}
