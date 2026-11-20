package orm

import (
	"errors"
	"testing"

	"github.com/vuka-lang/orm/v2/migration"
)

func TestNarrowCheck(t *testing.T) {
	ops := []migration.Operation{migration.CreateExtension{Name: "x"}}
	removing := append(ops, migration.DeleteModel{Table: "notes"}, migration.RemoveIndex{Table: "t", Name: "i"})

	if err := narrowCheck(removing); err != nil {
		t.Errorf("a whole tool refused: %v", err)
	}
	narrowTool = true
	defer func() { narrowTool = false }()
	if err := narrowCheck(ops); err != nil {
		t.Errorf("a plan that removes nothing was refused: %v", err)
	}
	err := narrowCheck(removing)
	var ne *narrowError
	if !errors.As(err, &ne) || len(ne.removes) != 2 {
		t.Errorf("a plan removing a table and an index: %v", err)
	}
}
