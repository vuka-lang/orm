// Package models declares models whose sets orm generate writes:
// TestSetsNameNoQuerySet checks what compiling it, and a package querying
// through the sets, instantiates.
package models

import "github.com/vuka-lang/orm/v2"

type Author struct {
	orm.Rec[Author]
	ID   int64
	Name string
}

type Book struct {
	orm.Rec[Book]
	ID       int64
	Title    string
	AuthorID int64
	Pages    int64
}
