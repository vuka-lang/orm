// Package vet is models whose rows go vet's unusedwrite checks: fields set,
// then a row method called (TestUnusedWrite).
package vet

import (
	"context"
	"time"

	"github.com/vuka-lang/orm/v2"
)

// Post embeds orm.Model.
type Post struct {
	orm.Model[Post]
	ID    int64
	Title string
	Tags  []Tag `gorm:"many2many:post_tags"`
}

type Tag struct {
	orm.Model[Tag]
	ID   int64
	Name string
}

// Note embeds orm.Base, as a Vuka model does.
type Note struct {
	orm.Base[Note]
	ID   int64
	Body string
}

// Event has its orm.Model through an embedded struct.
type Event struct {
	Audit
	ID    int64
	Label string
}

type Audit struct {
	orm.Model[Event]
	At time.Time
}

// Custom writes its own Delete: ormgen leaves it.
type Custom struct {
	orm.Model[Custom]
	ID      int64
	Name    string
	Deleted bool
}

func (c *Custom) Delete(ctx context.Context) error {
	c.Deleted = true
	return orm.Row(c).SaveFields(ctx, "Deleted")
}
