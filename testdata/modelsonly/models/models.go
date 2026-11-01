// Package models declares models and queries none: TestModelsNameNoQuerySet
// checks what compiling it, and a package importing it, instantiates.
package models

import "github.com/vuka-lang/orm/v2"

type User struct {
	orm.Model[User]
	ID   int64
	Name string
}

type Post struct {
	orm.Model[Post]
	ID     int64
	Title  string
	Author orm.FK[User]
}

func init() {
	orm.Register[User]()
	orm.Register[Post]()
}
