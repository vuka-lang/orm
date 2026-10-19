package ormgen

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestLoadError(t *testing.T) {
	pkg := func(msgs ...string) []*packages.Package {
		p := &packages.Package{PkgPath: "example.com/app/models"}
		for _, m := range msgs {
			p.Errors = append(p.Errors, packages.Error{Msg: m, Kind: packages.TypeError})
		}
		return []*packages.Package{p}
	}
	newer := `could not import fmt (reading /cache/e8-d: internal error in importing "fmt" (cannot decode "fmt", export data version 5 is greater than maximum supported version 4); please report an issue)`
	for _, msgs := range [][]string{
		{"undefined: UserFields"},
		{"could not import example.com/app/models (no export data file)"},
	} {
		if err := loadError(pkg(msgs...)); err != nil {
			t.Errorf("%q: %v, want generation to go on", msgs, err)
		}
	}
	for _, msgs := range [][]string{
		{"undefined: UserFields", newer},
		{"could not import github.com/vuka-lang/orm (no export data file)"},
	} {
		err := loadError(pkg(msgs...))
		if err == nil || !strings.Contains(err.Error(), "example.com/app/models") {
			t.Errorf("%q: %v, want a load error naming the package", msgs, err)
		}
	}
}
