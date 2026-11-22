package ormgen

import (
	"os"
	"path/filepath"
	"strings"
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

// TestMissingOverlay: a model only an overlay holds, as the Go of a .vuka
// file is before anything is written, gets its typed fields beside it, and
// files already current aren't missing.
func TestMissingOverlay(t *testing.T) {
	dir, _ := filepath.Abs("testdata/blog")
	src := "package blog\n\nimport \"github.com/vuka-lang/orm/v2\"\n\ntype Note struct {\n\torm.Model[Note]\n\tID   int64\n\tText string\n}\n"
	files, err := Missing(".", Config{Overlay: map[string][]byte{filepath.Join(dir, "note_vuka.go"): []byte(src)}})
	if err != nil {
		t.Fatal(err)
	}
	got := files[filepath.Join(dir, "note_gen.go")]
	if !strings.Contains(string(got), "var NoteColumns = ") {
		t.Fatalf("note_gen.go:\n%s", got)
	}
	for _, name := range []string{"post_gen.go", "user_gen.go"} {
		if _, ok := files[filepath.Join(dir, name)]; ok {
			t.Errorf("%s is current, and missing", name)
		}
	}
}

func TestRelatedClash(t *testing.T) {
	dir, _ := filepath.Abs("testdata/blog")
	path := filepath.Join(dir, "post.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(src), "Author orm.FK[User] `orm:\"related:posts\"`", "Author orm.FK[User] `orm:\"related:posts\"`\n Editor orm.FK[User] `orm:\"related:posts\"`", 1)
	if changed == string(src) {
		t.Fatal("fixture relation not found")
	}
	_, err = Generate(".", Config{Patterns: []string{"./testdata/blog"}, Overlay: map[string][]byte{path: []byte(changed)}})
	if err == nil || !strings.Contains(err.Error(), "Post.Author") || !strings.Contains(err.Error(), "Post.Editor") {
		t.Fatalf("Related clash = %v", err)
	}
}

func TestSchemaRelated(t *testing.T) {
	dir, _ := filepath.Abs("testdata/blog")
	path := filepath.Join(dir, "post.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(src), "Author orm.FK[User] `orm:\"related:posts\"`", "Author orm.FK[User]", 1)
	files, err := Generate(".", Config{Patterns: []string{"./testdata/blog"}, Overlay: map[string][]byte{path: []byte(changed)}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files[filepath.Join(dir, "user_gen.go")]), "func (p UserFields) Posts()") {
		t.Fatal("Schema Related did not generate an inverse")
	}
}

func TestRelatedColumnClash(t *testing.T) {
	dir, _ := filepath.Abs("testdata/blog")
	path := filepath.Join(dir, "user.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(src), "Name  string", "Name  string\n Posts int", 1)
	if changed == string(src) {
		t.Fatal("fixture field not found")
	}
	_, err = Generate(".", Config{Patterns: []string{"./testdata/blog"}, Overlay: map[string][]byte{path: []byte(changed)}})
	if err == nil || !strings.Contains(err.Error(), "Post.Author") || !strings.Contains(err.Error(), "User.Posts") {
		t.Fatalf("column clash = %v", err)
	}
}

// TestSourceDirectives: each typed field, relation and model-level
// declaration names what it stands for — the field of the struct declaring
// it, an embedded one's or, for an inverse, the other model's — so an
// editor can go from it to the model's source.
func TestSourceDirectives(t *testing.T) {
	files, err := Generate(".", Config{Patterns: []string{"./testdata/blog"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs("testdata/blog")
	post, user := string(files[filepath.Join(dir, "post_gen.go")]), string(files[filepath.Join(dir, "user_gen.go")])
	for _, want := range []struct{ file, text string }{
		{post, "//orm:source Post\nvar PostColumns"},
		{post, "//orm:source Post\ntype PostFields struct"},
		{post, "\t//orm:source Post.Title\n\tTitle orm.TextField[string]"},
		{post, "\t//orm:source Stamps.Created\n\tCreated orm.TimeField"},
		{post, "//orm:source Post.Author\nfunc (p PostFields) Author()"},
		{user, "//orm:source Post.Author\nfunc (p UserFields) Posts()"},
	} {
		if !strings.Contains(want.file, want.text) {
			t.Errorf("no %q", want.text)
		}
	}
}

// TestDeclaresExtension: a package declaring no model but calling
// orm.CreateExtension is one Declares names, so a tool importing only the
// declaring packages still registers its extension.
func TestDeclaresExtension(t *testing.T) {
	var got []string
	if _, err := Generate(".", Config{Patterns: []string{"./testdata/ext"}, Declares: func(p string) { got = append(got, p) }}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "/ormgen/testdata/ext") {
		t.Fatalf("Declares heard %v, want the ext package", got)
	}
}

// TestCrossPackageFields: a relation to a model of another package of the
// module is a method of the typed fields, as one to a model of the same
// package is, whether that package's typed fields are written yet or not,
// and whichever package's run generates the relating model's.
func TestCrossPackageFields(t *testing.T) {
	dir, _ := filepath.Abs("testdata/xpkg")
	golden := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	unwritten := map[string][]byte{
		filepath.Join(dir, "accounts", "user_gen.go"):         []byte("package accounts\n"),
		filepath.Join(dir, "accounts", "orm_scanners_gen.go"): []byte("package accounts\n"),
	}
	for _, c := range []struct {
		name    string
		pattern string
		overlay map[string][]byte
		want    []string
	}{
		{"its own package", "./testdata/xpkg/shop", nil, []string{"shop/order_gen.go", "shop/" + OutName}},
		{"before the other package's fields are written", "./testdata/xpkg/shop", unwritten, []string{"shop/order_gen.go", "shop/" + OutName}},
		{"a third package importing both", "./testdata/xpkg/report", unwritten, []string{"shop/order_gen.go"}},
		{"the related package", "./testdata/xpkg/accounts", nil, []string{"accounts/user_gen.go", "accounts/" + OutName}},
		{"all of them", "./testdata/xpkg/...", nil, []string{"accounts/user_gen.go", "accounts/" + OutName, "shop/order_gen.go", "shop/" + OutName, "same/models_gen.go", "same/" + OutName}},
	} {
		t.Run(c.name, func(t *testing.T) {
			files, err := Generate(".", Config{Patterns: []string{c.pattern}, Overlay: c.overlay})
			if err != nil {
				t.Fatal(err)
			}
			for _, rel := range c.want {
				if got := string(files[filepath.Join(dir, rel)]); got != golden(rel) {
					t.Errorf("%s differs from testdata/xpkg/%s:\n%s", rel, rel, got)
				}
			}
			if len(files) != len(c.want) {
				t.Errorf("generated %d files, want %d", len(files), len(c.want))
			}
		})
	}
	if order := golden("shop/order_gen.go"); !strings.Contains(order, "func (p OrderFields) Customer() accounts.UserPath {\n\treturn accounts.UserFieldsIn(") {
		t.Errorf("order_gen.go has no Customer relation into package accounts:\n%s", order)
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo}, "./testdata/xpkg/...")
	if err != nil {
		t.Fatal(err)
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("%s: %s", p.PkgPath, e)
		}
	})
}
