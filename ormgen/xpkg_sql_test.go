package ormgen_test

import (
	"context"
	"testing"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/report"
	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/same"
	"github.com/vuka-lang/orm/v2/ormgen/testdata/xpkg/shop"
)

type sqler interface {
	SQL(context.Context) (string, []any, error)
}

// TestCrossPackageSQL: a query filtering through a relation to a model of
// another package makes the SQL it makes with the models in one package.
func TestCrossPackageSQL(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlite"} {
		ctx := orm.WithDB(context.Background(), orm.Open(nil, driver))
		for _, c := range []struct {
			name        string
			split, same sqler
		}{
			{"two packages", shop.ByEmail("a@example.org"), same.ByEmail("a@example.org")},
			{"three packages", report.Large("Asha", 100), same.Large("Asha", 100)},
		} {
			got, gotArgs, err := c.split.SQL(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want, wantArgs, err := c.same.SQL(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != want || len(gotArgs) != len(wantArgs) {
				t.Errorf("%s, %s:\n%s %v\nwant\n%s %v", driver, c.name, got, gotArgs, want, wantArgs)
			}
			for i := range gotArgs {
				if gotArgs[i] != wantArgs[i] {
					t.Errorf("%s, %s: argument %d is %v, want %v", driver, c.name, i, gotArgs[i], wantArgs[i])
				}
			}
		}
	}
}
