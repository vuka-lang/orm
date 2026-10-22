// Package blog is the models TestFields generates typed fields for.
package blog

import "github.com/vuka-lang/orm/v2"

type User struct {
	orm.Model[User]
	ID    int64
	Name  string
	Email string
}

func (u *User) Schema(s *orm.Schema) {
	s.Field(&u.Email, orm.Char{Max: 254, Unique: true})
}
