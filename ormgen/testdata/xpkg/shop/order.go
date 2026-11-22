// Package shop is a model with a relation to a model of another package.
package shop

import (
	"github.com/vuka-lang/orm/v2"

	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/accounts"
)

type Order struct {
	orm.Model[Order]
	ID       int64
	Total    int64
	Customer orm.FK[accounts.User]
}

func (o *Order) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "orders"})
}

// ByEmail is the orders of the customer with email, filtered through the
// relation to the other package's model.
func ByEmail(email string) orm.QuerySet[Order] {
	return orm.For[Order]().Filter(OrderColumns.Customer().Email.Eq(email))
}
