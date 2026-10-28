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
