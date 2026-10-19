package main

import (
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/vuka"
)

//go:generate go run github.com/vuka-lang/orm/cmd/ormgen

type Customer struct {
	orm.Model[Customer]
	ID    int64
	Name  string
	Email string
}

func (c *Customer) Schema(s *orm.Schema) {
	s.Field(&c.Email, orm.Char{Max: 254, Unique: true, Lower: true, Trim: true})
}

type Order struct {
	orm.Model[Order]
	ID       int64
	Ref      string
	Total    int64
	Paid     bool
	Note     vuka.Option[string]
	Customer orm.FK[Customer]
	PlacedAt time.Time
}

func (o *Order) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Ordering: []string{"-placed_at"}})
	s.Field(&o.Ref, orm.Char{Max: 20, Unique: true})
	s.Field(&o.Total, orm.Int{Min: 0})
	s.Field(&o.Customer, orm.Rel{OnDelete: orm.Cascade, Related: "orders"})
	s.Field(&o.PlacedAt, orm.Datetime{AutoNowAdd: true})
}
