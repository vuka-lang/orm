// Package accounts is the model another package's model relates to: the
// split of TestCrossPackageFields.
package accounts

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
