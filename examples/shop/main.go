// Command shop is the ORM in plain Go: models declaring their columns in
// Schema methods, queries through the typed fields ormgen writes beside
// them (models_gen.go), and a transaction.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/vuka-lang/orm/v2"
	_ "github.com/vuka-lang/orm/v2/sqlite"
)

func main() {
	ctx := context.Background()
	db, err := orm.ConnectURL("sqlite::memory:")
	if err != nil {
		log.Fatal(err)
	}
	ctx = orm.WithDB(ctx, db)
	customers, orders := orm.For[Customer](), orm.For[Order]()
	if err := orm.CreateTables(ctx, customers, orders); err != nil {
		log.Fatal(err)
	}
	if err := seed(ctx); err != nil {
		log.Fatal(err)
	}

	f := OrderFields
	unpaid, err := orders.Filter(f.Paid.Eq(false), f.Customer.Email.IEndsWith("@example.com")).
		OrderBy(f.Total.Desc()).All(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, o := range unpaid {
		fmt.Println("unpaid:", o.Ref, o.Total)
	}

	err = orm.Transaction(ctx, func(ctx context.Context) error {
		o, err := orders.Get(ctx, f.Ref.Eq("A-2"))
		if err != nil {
			return err
		}
		o.Paid = true
		return o.Save(ctx)
	})
	if err != nil {
		log.Fatal(err)
	}
	n, err := orders.Filter(f.Paid.Eq(true)).Count(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("paid:", n)
}

func seed(ctx context.Context) error {
	ada := Customer{Name: "Ada", Email: " ADA@example.com"}
	if err := ada.Save(ctx); err != nil {
		return err
	}
	for _, o := range []Order{{Ref: "A-1", Total: 1200}, {Ref: "A-2", Total: 4500}, {Ref: "A-3", Total: 300, Paid: true}} {
		o.Customer = orm.FKOf(&ada)
		if err := o.Save(ctx); err != nil {
			return err
		}
	}
	return nil
}
