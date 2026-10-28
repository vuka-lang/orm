package blog

import (
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/vuka/v2"
)

type Post struct {
	orm.Model[Post]
	ID     int64
	Title  string
	Slug   string
	Body   *string
	Views  vuka.Option[int]
	Author orm.FK[User] `orm:"related:posts"`
	Tags   orm.M2M[Tag] `orm:"related:posts"`
	Stamps
}

// Stamps is when a row was made and last changed.
type Stamps struct {
	Created time.Time
	Updated time.Time
}

type Tag struct {
	orm.Model[Tag]
	ID   int64
	Name string
}

func (p *Post) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "posts", Ordering: []string{"-created"}})
	s.Field(&p.Title, orm.Char{Max: 200})
	s.Field(&p.Slug, orm.Slug{Unique: true})
	s.Field(&p.Author, orm.Rel{OnDelete: orm.Cascade, Related: "posts"})
	s.Field(&p.Created, orm.Datetime{AutoNowAdd: true})
	s.Field(&p.Updated, orm.Datetime{AutoNow: true})
}
