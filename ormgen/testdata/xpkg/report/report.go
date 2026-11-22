// Package report declares no models and queries the models of two
// packages, one relating to the other.
package report

import (
	"github.com/vuka-lang/orm/v2"

	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/accounts"
	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/shop"
)

// Large is the orders over total of customers named name.
func Large(name string, total int64) orm.QuerySet[shop.Order] {
	return orm.For[shop.Order]().Filter(shop.OrderColumns.Customer().Name.Eq(name), shop.OrderColumns.Total.Gt(total))
}

// NameIs is a condition on the users of package accounts, which this
// package reads without querying them.
func NameIs(name string) orm.Cond {
	return accounts.UserColumns.Name.Eq(name)
}
