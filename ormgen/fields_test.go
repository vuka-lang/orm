package ormgen

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestFields generates testdata/blog's typed fields and compares them with
// the files beside its models, which its query.go uses: they are current,
// and code naming them type-checks.
func TestFields(t *testing.T) {
	files, err := Generate(".", Config{Patterns: []string{"./testdata/blog"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs("testdata/blog")
	for _, name := range []string{"post_gen.go", "user_gen.go", OutName} {
		path := filepath.Join(dir, name)
		want, err := os.ReadFile(path)
		if err != nil || string(files[path]) != string(want) {
			t.Errorf("%s is out of date: go run ../cmd/ormgen ./testdata/blog in orm/ormgen\n%s", name, files[path])
		}
	}
	if len(files) != 3 {
		t.Errorf("generated %d files, want 3", len(files))
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo}, "./testdata/blog")
	if err != nil {
		t.Fatal(err)
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("%s: %s", p.PkgPath, e)
		}
	})
}

func TestFieldsName(t *testing.T) {
	for in, want := range map[string]string{
		"models/post.go":      "post_gen.go",
		"post_test.go":        "post_gen_test.go",
		"post_vuka.go":        "post_gen.go",
		"/app/applicant.vuka": "applicant_gen.go",
		"order_items_test.go": "order_items_gen_test.go",
	} {
		if got := FieldsName(in); got != want {
			t.Errorf("FieldsName(%q) = %s, want %s", in, got, want)
		}
	}
}
