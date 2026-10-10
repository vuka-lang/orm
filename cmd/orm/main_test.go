package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// modelsApp is a module using the orm from this repository: models in two
// packages, a main package linking them and the SQLite driver.
func modelsApp(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a module")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	root := t.TempDir()
	sum, err := os.ReadFile(filepath.Join(repo, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":  "module example.com/shop\n\ngo 1.27\n\nrequire github.com/vuka-lang/orm v0.0.0\n\nreplace github.com/vuka-lang/orm => " + repo + "\n" + vukaReplace(repo),
		"go.sum":  string(sum),
		"main.go": "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\n\t_ \"github.com/vuka-lang/orm/sqlite\"\n)\n\nfunc main() {}\n",
		"models/user.go": `package models

import "github.com/vuka-lang/orm"

type User struct {
	orm.Model[User]
	ID    int64
	Email string
}
`,
		"blog/post.go": `package blog

import (
	"github.com/vuka-lang/orm"

	"example.com/shop/models"
)

type Post struct {
	orm.Model[Post]
	ID       int64
	Title    string
	AuthorID int64
	Author   *models.User
}
`,
	}
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = root
	tidy.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return root
}

// TestMigrationCommands runs the migration commands on a program:
// makemigrations writes Go migrations (asking about a rename), migrate
// applies and unapplies them on its SQLite database, showmigrations and
// sqlmigrate read them, generate writes the models' code.
func TestMigrationCommands(t *testing.T) {
	root := modelsApp(t)
	t.Setenv("GOWORK", "off")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
	run := func(what string, stdin io.Reader, f func(stdin io.Reader, stdout, stderr io.Writer) error) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := f(stdin, &out, &errb); err != nil {
			t.Fatalf("%s: %v\n%s\n%s", what, err, out.String(), errb.String())
		}
		return out.String()
	}
	mk := func(stdin io.Reader, args ...string) string {
		return run("makemigrations", stdin, func(in io.Reader, out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root}, args...), in, out, errb)
		})
	}
	migrate := func(args ...string) string {
		return run("migrate", nil, func(in io.Reader, out, errb io.Writer) error {
			return runMigrate(append([]string{"--root", root}, args...), in, out, errb)
		})
	}
	show := func() string {
		return run("showmigrations", nil, func(_ io.Reader, out, errb io.Writer) error {
			return runShowMigrations([]string{"--root", root}, out, errb)
		})
	}

	if out := mk(nil); !strings.Contains(out, "migrations/0001_initial.go") {
		t.Fatalf("makemigrations: %s", out)
	}
	src, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
	if err != nil || !strings.Contains(string(src), "package migrations") || !strings.Contains(string(src), `m.F("email", m.Text())`) || !strings.Contains(string(src), `"posts"`) {
		t.Fatalf("0001_initial.go: %v\n%s", err, src)
	}
	if out := mk(nil, "--check"); !strings.Contains(out, "No changes") {
		t.Fatalf("--check after it: %s", out)
	}

	// users.email renamed: asked, and on yes a rename.
	userGo := filepath.Join(root, "models", "user.go")
	old, _ := os.ReadFile(userGo)
	writeFile(t, userGo, strings.Replace(string(old), "Email string", "Mail  string", 1))
	if out := mk(nil, "--dry-run", "--noinput"); !strings.Contains(out, `m.RemoveField{Table: "users", Name: "email"}`) || !strings.Contains(out, "// NOTE: drops users.email") {
		t.Fatalf("--noinput: %s", out)
	}
	alwaysAsk = true
	t.Cleanup(func() { alwaysAsk = false })
	if out := mk(strings.NewReader("y\n")); !strings.Contains(out, "0002_rename_field_users_email_to_mail.go") {
		t.Fatalf("the rename: %s", out)
	}

	out := migrate()
	if !strings.Contains(out, "migration applied: 0001_initial") || !strings.Contains(out, "migration applied: 0002_rename_field_users_email_to_mail") {
		t.Fatalf("migrate: %s", out)
	}
	if out := show(); !strings.Contains(out, " [X] 0001_initial") || !strings.Contains(out, " [X] 0002_rename") {
		t.Fatalf("showmigrations: %s", out)
	}
	sqlOut := run("sqlmigrate", nil, func(_ io.Reader, out, errb io.Writer) error {
		return runSQLMigrate([]string{"--root", root, "0002"}, out, errb)
	})
	if !strings.Contains(sqlOut, `ALTER TABLE "users" RENAME COLUMN "email" TO "mail";`) {
		t.Fatalf("sqlmigrate: %s", sqlOut)
	}
	if out := migrate("0001"); !strings.Contains(out, "migration unapplied: 0002_rename") {
		t.Fatalf("migrate 0001: %s", out)
	}
	if out := show(); !strings.Contains(out, " [ ] 0002_rename") {
		t.Fatalf("showmigrations after going back: %s", out)
	}
	migrate("zero")
	if out := show(); !strings.Contains(out, " [ ] 0001_initial") {
		t.Fatalf("showmigrations at zero: %s", out)
	}

	if out := mk(nil, "--empty", "seed"); !strings.Contains(out, "0003_seed.go") {
		t.Fatalf("--empty: %s", out)
	}
	seed, _ := os.ReadFile(filepath.Join(root, "migrations", "0003_seed.go"))
	if !strings.Contains(string(seed), "m.RunGo{") || !strings.Contains(string(seed), `Dependencies: []string{"0002_rename_field_users_email_to_mail"}`) {
		t.Fatalf("0003_seed.go:\n%s", seed)
	}
	migrate()

	// generate writes what the tool overlaid; --check passes after it.
	var gout, gerr bytes.Buffer
	if err := runGenerate([]string{root}, &gout, &gerr); err != nil || !strings.Contains(gout.String(), "orm_scanners_gen.go") {
		t.Fatalf("generate: %v\n%s%s", err, gout.String(), gerr.String())
	}
	gen, _ := os.ReadFile(filepath.Join(root, "models", "orm_scanners_gen.go"))
	if !strings.Contains(string(gen), "orm.Register[User]()") {
		t.Fatalf("generated:\n%s", gen)
	}
	gout.Reset()
	if err := runGenerate([]string{"--check", root}, &gout, &gerr); err != nil {
		t.Fatalf("generate --check: %v\n%s", err, gerr.String())
	}
}

// vukaReplace is the module's own replace of the Vuka runtime (an
// unreleased one, beside the repository), for a module the tests write.
func vukaReplace(repo string) string {
	mod, _ := os.ReadFile(filepath.Join(repo, "go.mod"))
	for line := range strings.SplitSeq(string(mod), "\n") {
		if strings.HasPrefix(line, "replace github.com/vuka-lang/vuka => ") {
			dir := strings.TrimPrefix(line, "replace github.com/vuka-lang/vuka => ")
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(repo, dir)
			}
			return "replace github.com/vuka-lang/vuka => " + dir + "\n"
		}
	}
	return ""
}

// TestVukaModels runs makemigrations and migrate on a program whose
// models are Vuka, in its main package: the commands build it through
// vuka gen. It needs a vuka command with field attributes: VUKA.
func TestVukaModels(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	if os.Getenv("VUKA") == "" {
		t.Skip("set VUKA to a vuka command with field attributes")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	root := t.TempDir()
	sum, _ := os.ReadFile(filepath.Join(repo, "go.sum"))
	files := map[string]string{
		"go.mod": "module example.com/notes\n\ngo 1.27\n\nrequire (\n\tgithub.com/vuka-lang/orm v0.0.0\n\tgithub.com/vuka-lang/vuka v0.6.0\n)\n\nreplace github.com/vuka-lang/orm => " + repo + "\n" + vukaReplace(repo),
		"go.sum": string(sum),
		"main.vuka": `package main

import (
	"github.com/vuka-lang/orm"
	_ "github.com/vuka-lang/orm/sqlite"
)

type Author struct {
	orm.Base
	ID   int64
	Name string @orm.Char{Max: 80, Unique: true}
}

type Note struct {
	orm.Base @orm.Meta{Table: "notes"}
	ID     int64
	Text   string       @orm.LongText
	Author orm.FK[Author] @orm.Rel{OnDelete: orm.Cascade}
	Pinned Option[bool]
	Title  string     @orm.Char{Max: 100} @orm.Trigram
	Vec    orm.Vector @orm.Dims(3) @orm.HNSW{Metric: orm.Cosine, M: 8}
}

func main() {}
`,
	}
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
	var out, errb bytes.Buffer
	if err := runMakeMigrations([]string{"--root", root, "initial"}, nil, &out, &errb); err != nil {
		t.Fatalf("makemigrations: %v\n%s\n%s", err, out.String(), errb.String())
	}
	src, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
	for _, want := range []string{`m.F("name", m.Varchar(80).Unique())`, `m.F("author_id", m.BigInt().FK("authors", "id").OnDelete(m.Cascade))`, `m.F("pinned", m.Bool().Null())`, `Table: "notes"`, `m.F("vec", m.Vector(3).Null())`,
		`USING hnsw ("vec" vector_cosine_ops) WITH (m = 8)`, `USING gin ("title" gin_trgm_ops)`} {
		if err != nil || !strings.Contains(string(src), want) {
			t.Fatalf("0001_initial.go lacks %s: %v\n%s", want, err, src)
		}
	}
	out.Reset()
	if err := runMigrate([]string{"--root", root}, nil, &out, &errb); err != nil || !strings.Contains(out.String()+errb.String(), "migration applied: 0001_initial") {
		t.Fatalf("migrate: %v\n%s\n%s", err, out.String(), errb.String())
	}
}
