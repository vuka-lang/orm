package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	_ "github.com/vuka-lang/orm/v2/sqlite"
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
	t.Setenv("ORM_CACHE", t.TempDir())
	sum, err := os.ReadFile(filepath.Join(repo, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":  "module example.com/shop\n\ngo 1.27\n\nrequire github.com/vuka-lang/orm/v2 v2.0.0\n\nreplace github.com/vuka-lang/orm/v2 => " + repo + "\n" + vukaReplace(repo),
		"go.sum":  string(sum),
		"main.go": "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\nfunc main() {}\n",
		"models/user.go": `package models

import "github.com/vuka-lang/orm/v2"

type User struct {
	orm.Model[User]
	ID    int64
	Email string
}
`,
		"blog/post.go": `package blog

import (
	"github.com/vuka-lang/orm/v2"

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
	// run returns a command's stdout; its stderr, the progress, is in
	// stderr after.
	var stderr string
	run := func(what string, stdin io.Reader, f func(stdin io.Reader, stdout, stderr io.Writer) error) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := f(stdin, &out, &errb); err != nil {
			t.Fatalf("%s: %v\n%s\n%s", what, err, out.String(), errb.String())
		}
		stderr = errb.String()
		return out.String()
	}
	mk := func(stdin io.Reader, args ...string) string {
		return run("makemigrations", stdin, func(in io.Reader, out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root}, args...), in, out, errb)
		})
	}
	migrateIn := func(stdin io.Reader, args ...string) string {
		return run("migrate", stdin, func(in io.Reader, out, errb io.Writer) error {
			return runMigrate(append([]string{"--root", root}, args...), in, out, errb)
		})
	}
	migrate := func(args ...string) string { return migrateIn(nil, args...) }
	show := func() string {
		return run("showmigrations", nil, func(_ io.Reader, out, errb io.Writer) error {
			return runShowMigrations([]string{"--root", root}, out, errb)
		})
	}

	if out := mk(nil); !strings.Contains(out, "migrations/0001_initial.go") {
		t.Fatalf("makemigrations: %s", out)
	}
	// Not a terminal: a line as each step starts and one as it ends.
	for _, want := range []string{"Generating the ORM code…\n", "\n✓ Generated the ORM code  ", "Compiling the migration tool…\n", "\n✓ Compiled the migration tool  "} {
		if !strings.Contains(stderr, want) || strings.ContainsAny(stderr, "\r\x1b") {
			t.Fatalf("makemigrations' progress, no %q:\n%s", want, stderr)
		}
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

	migrate("-q")
	if stderr != "" {
		t.Fatalf("migrate -q printed progress:\n%s", stderr)
	}
	migrate("zero", "--noinput")
	migrate()
	for _, want := range []string{
		"Applying 0001_initial on the default database…\n", "\n✓ Applied 0001_initial on the default database  ",
		"\n✓ Applied 0002_rename_field_users_email_to_mail on the default database  ", "\n✓ Applied 2 migrations on the default database in ",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("migrate, no %q:\n%s", want, stderr)
		}
	}
	if migrate(); !strings.Contains(stderr, "No migrations to apply.") || !strings.Contains(stderr, "✓ Migration tool up to date") || strings.Contains(stderr, "Compiling") {
		t.Fatalf("migrate with nothing to do, the tool built before:\n%s", stderr)
	}
	if out := show(); !strings.Contains(out, " [X] 0001_initial") || !strings.Contains(out, " [X] 0002_rename") {
		t.Fatalf("showmigrations: %s", out)
	}
	sqlOut := run("sqlmigrate", nil, func(_ io.Reader, out, errb io.Writer) error {
		return runSQLMigrate([]string{"--root", root, "0002"}, out, errb)
	})
	if !strings.Contains(sqlOut, `ALTER TABLE "users" RENAME COLUMN "email" TO "mail";`) || strings.Contains(sqlOut, "migration tool") {
		t.Fatalf("sqlmigrate: %s", sqlOut)
	}
	// Unapplying asks on a terminal: no keeps everything applied.
	var mout, merr bytes.Buffer
	err = runMigrate([]string{"--root", root, "0001"}, strings.NewReader("n\n"), &mout, &merr)
	if err == nil || !strings.Contains(err.Error()+merr.String(), "cancelled") || !strings.Contains(merr.String(), "0002_rename") {
		t.Fatalf("migrate 0001, answered no: %v\n%s\n%s", err, mout.String(), merr.String())
	}
	if out := show(); !strings.Contains(out, " [X] 0002_rename") {
		t.Fatalf("showmigrations after a no: %s", out)
	}
	// The tool is the models and migrations alone: the program's main
	// package isn't built, so a broken one doesn't stop it.
	if want := "example.com/shop/zz_ormtool"; toolMainPath(t) != want {
		t.Fatalf("the tool's main package is %s, want %s", toolMainPath(t), want)
	}
	// -v: go build -v names the main package, changed so it compiles again
	// (the whole program is the tool: ORM_TOOL=full).
	t.Setenv("ORM_TOOL", "full")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\n// changed at "+strconv.FormatInt(time.Now().UnixNano(), 10)+"\nfunc main() {}\n")
	if migrateIn(strings.NewReader("y\n"), "0001", "-v"); !strings.Contains(stderr, "✓ Unapplied 0002_rename_field_users_email_to_mail on the default database  ") ||
		!strings.Contains(stderr, `    ALTER TABLE "users" RENAME COLUMN "mail" TO "email"`) || !strings.Contains(stderr, "\nexample.com/shop\n") {
		t.Fatalf("migrate 0001 -v: %s", stderr)
	}
	// A program that doesn't compile: go build's errors, shown without -v.
	writeFile(t, filepath.Join(root, "broken.go"), "package main\n\nvar _ = undefinedName\n")
	var bout, berr bytes.Buffer
	err = runMigrate([]string{"--root", root}, nil, &bout, &berr)
	os.Remove(filepath.Join(root, "broken.go"))
	if err == nil || !strings.Contains(berr.String(), "undefined: undefinedName") || !strings.Contains(berr.String(), "✗ ") {
		t.Fatalf("migrate of a broken program: %v\n%s", err, berr.String())
	}
	if out := show(); !strings.Contains(out, " [ ] 0002_rename") {
		t.Fatalf("showmigrations after going back: %s", out)
	}
	t.Setenv("ORM_TOOL", "")
	migrate("zero", "--noinput")
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
	if !strings.Contains(string(gen), "orm.RegisterModel((*User)(nil))") {
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
		if strings.HasPrefix(line, "replace github.com/vuka-lang/vuka/v2 => ") {
			dir := strings.TrimPrefix(line, "replace github.com/vuka-lang/vuka/v2 => ")
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(repo, dir)
			}
			return "replace github.com/vuka-lang/vuka/v2 => " + dir + "\n"
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
	t.Setenv("ORM_CACHE", t.TempDir())
	sum, _ := os.ReadFile(filepath.Join(repo, "go.sum"))
	files := map[string]string{
		"go.mod": "module example.com/notes\n\ngo 1.27\n\nrequire (\n\tgithub.com/vuka-lang/orm/v2 v2.0.0\n\tgithub.com/vuka-lang/vuka/v2 " + vukaRequire(repo) + "\n)\n\nreplace github.com/vuka-lang/orm/v2 => " + repo + "\n" + vukaReplace(repo),
		"go.sum": string(sum),
		"main.vuka": `package main

import (
	"time"

	"github.com/vuka-lang/orm/v2"
	_ "github.com/vuka-lang/orm/v2/sqlite"
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
	Status string     @orm.Char{Max: 20, Default: "draft", Choices: []string{"draft", "done"}, Comment: "the workflow's state"}
	Rank   int        @orm.Int{Size: orm.Small, Min: 0, Default: 0}
	Due    time.Time  @orm.DateField{Null: true}
	Seen   time.Time  @orm.Datetime{DefaultNow: true, Precision: 3}
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
		`USING hnsw ("vec" vector_cosine_ops) WITH (m = 8)`, `USING gin ("title" gin_trgm_ops)`,
		`m.F("status", m.Varchar(20).Comment("the workflow's state").DefaultSQL("'draft'"))`, `m.F("rank", m.SmallInt().DefaultSQL("0"))`,
		`m.F("due", m.Date().Null())`, `m.F("seen", m.Time().Precision(3).DefaultSQL("CURRENT_TIMESTAMP"))`} {
		if err != nil || !strings.Contains(string(src), want) {
			t.Fatalf("0001_initial.go lacks %s: %v\n%s", want, err, src)
		}
	}
	out.Reset()
	if err := runMigrate([]string{"--root", root}, nil, &out, &errb); err != nil || !strings.Contains(errb.String(), "✓ Applied 0001_initial on the default database") || !strings.Contains(errb.String(), "✓ Generated the Go code  ") {
		t.Fatalf("migrate: %v\n%s\n%s", err, out.String(), errb.String())
	}
	// A column attribute changed is an AlterField.
	main := filepath.Join(root, "main.vuka")
	old, _ := os.ReadFile(main)
	writeFile(t, main, strings.Replace(string(old), `Default: "draft"`, `Default: "new"`, 1))
	out.Reset()
	if err := runMakeMigrations([]string{"--root", root, "status"}, nil, &out, &errb); err != nil {
		t.Fatalf("makemigrations: %v\n%s\n%s", err, out.String(), errb.String())
	}
	src, err = os.ReadFile(filepath.Join(root, "migrations", "0002_status.go"))
	if want := `m.AlterField{Table: "notes", Name: "status", Field: m.Varchar(20).Comment("the workflow's state").DefaultSQL("'new'")}`; err != nil || !strings.Contains(string(src), want) {
		t.Fatalf("0002_status.go lacks %s: %v\n%s", want, err, src)
	}
	out.Reset()
	if err := runMigrate([]string{"--root", root}, nil, &out, &errb); err != nil || !strings.Contains(errb.String(), "✓ Applied 0002_status on the default database") {
		t.Fatalf("migrate: %v\n%s\n%s", err, out.String(), errb.String())
	}
}

// TestMultiDBMigrations: makemigrations writes each database's
// migrations, with the models routed there (Meta.DB), into migrations/
// for the default and migrations/<db> for another; migrate applies every
// database's. A CrossDB key has no foreign key.
func TestMultiDBMigrations(t *testing.T) {
	root := modelsApp(t)
	writeFile(t, filepath.Join(root, "shop", "shop.go"), `package shop

import "github.com/vuka-lang/orm/v2"

type Event struct {
	orm.Model[Event]
	ID   int64
	Kind string
}

func (Event) Meta() orm.Meta { return orm.Meta{DB: "analytics"} }

type Customer struct {
	orm.Model[Customer]
	ID   int64
	Name string
}

func (Customer) Meta() orm.Meta { return orm.Meta{DB: "crm"} }

type Order struct {
	orm.Model[Order]
	ID         int64
	CustomerID int64
	Customer   *Customer `+"`orm:\"crossdb\"`"+`
}
`)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\t_ \"example.com/shop/shop\"\n\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\nfunc main() {}\n")
	t.Setenv("GOWORK", "off")
	dbs := map[string]string{"": "app.db", "analytics": "analytics.db", "crm": "crm.db"}
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, dbs[""]))
	t.Setenv("DATABASE_URL_ANALYTICS", "sqlite:"+filepath.Join(root, dbs["analytics"]))
	t.Setenv("DATABASE_URL_CRM", "sqlite:"+filepath.Join(root, dbs["crm"]))
	for _, db := range []string{"", "analytics", "crm"} {
		var out, errb bytes.Buffer
		args := []string{"--root", root, "--noinput"}
		if db != "" {
			args = append(args, "--db", db)
		}
		if err := runMakeMigrations(args, nil, &out, &errb); err != nil {
			t.Fatalf("makemigrations %s: %v\n%s%s", db, err, out.String(), errb.String())
		}
	}
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	def, analytics, crm := read("migrations/0001_initial.go"), read("migrations/analytics/0001_initial.go"), read("migrations/crm/0001_initial.go")
	switch {
	case !strings.Contains(def, `"orders"`) || !strings.Contains(def, `"posts"`) || strings.Contains(def, `"events"`) || strings.Contains(def, `"customers"`):
		t.Fatalf("the default's migration:\n%s", def)
	case strings.Contains(def, "References(") || strings.Contains(def, `"customers"`):
		t.Fatalf("a CrossDB key with a foreign key:\n%s", def)
	case !strings.Contains(analytics, `"events"`) || strings.Contains(analytics, `"orders"`) || !strings.Contains(analytics, "package analytics"):
		t.Fatalf("analytics' migration:\n%s", analytics)
	case !strings.Contains(crm, `"customers"`) || strings.Contains(crm, `"events"`):
		t.Fatalf("crm's migration:\n%s", crm)
	}
	var out, errb bytes.Buffer
	if err := runMigrate([]string{"--root", root}, nil, &out, &errb); err != nil {
		t.Fatalf("migrate: %v\n%s%s", err, out.String(), errb.String())
	}
	for db, table := range map[string]string{"": "orders", "analytics": "events", "crm": "customers"} {
		d, err := orm.ConnectURL("sqlite:" + filepath.Join(root, dbs[db]))
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = d.SQL().QueryRow("SELECT name FROM sqlite_master WHERE name = ?", table).Scan(&got)
		d.SQL().Close()
		if err != nil || got != table {
			t.Errorf("%s has no table %s: %v", dbs[db], table, err)
		}
	}
}

// The module's .env names the databases when the environment doesn't.
func TestDotEnvURLs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n")
	writeFile(t, filepath.Join(root, ".env"), `# settings
APP_NAME=App
DATABASE_URL=sqlite://data/app.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)   # the default
export DATABASE_URL_ANALYTICS="postgres://u:p@h/a"
DATABASE_URL_SET=from-the-file
`)
	t.Setenv("DATABASE_URL_SET", "from-the-environment")
	for _, k := range []string{"DATABASE_URL", "DATABASE_URL_ANALYTICS"} {
		t.Setenv(k, "") // restored after the test
		os.Unsetenv(k)
	}
	p := &project{root: filepath.Join(root)}
	got := p.urls()
	for name, want := range map[string]string{
		"":          "sqlite://data/app.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		"analytics": "postgres://u:p@h/a",
		"set":       "from-the-environment",
	} {
		if got[name] != want {
			t.Errorf("%q: %q, want %q", name, got[name], want)
		}
	}
	if _, ok := got["app_name"]; ok {
		t.Error("a key other than DATABASE_URL… was read")
	}
}

// vukaRequire is the vuka version the orm's go.mod requires.
func vukaRequire(repo string) string {
	mod, _ := os.ReadFile(filepath.Join(repo, "go.mod"))
	for line := range strings.SplitSeq(string(mod), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "github.com/vuka-lang/vuka/v2" {
			return f[1]
		}
	}
	return "v2.0.0"
}

// TestVukaGenAsksForCodeOnly: the tool asks vuka gen for the Go alone, and
// a vuka older than -code-only builds everything as before.
func TestVukaGenAsksForCodeOnly(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	for name, script := range map[string]string{
		"new": "echo \"$@\" >> " + log + "\n",
		"old": "case \"$*\" in *-code-only*) echo 'flag provided but not defined: -code-only' >&2; exit 2;; esac\necho \"$@\" >> " + log + "\n",
	} {
		os.Remove(log)
		bin := filepath.Join(dir, name)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VUKA", bin)
		var stderr bytes.Buffer
		if err := vukaGen(dir, filepath.Join(dir, "mirror"), &stderr); err != nil {
			t.Fatalf("%s vuka: %v %s", name, err, stderr.String())
		}
		calls, _ := os.ReadFile(log)
		want := "gen -code-only -o " + filepath.Join(dir, "mirror") + "\n"
		if name == "old" {
			want = "gen -o " + filepath.Join(dir, "mirror") + "\n"
		}
		if string(calls) != want {
			t.Errorf("%s vuka was run as %q, want %q", name, calls, want)
		}
		if stderr.Len() > 0 {
			t.Errorf("%s vuka: the fallback's refusal reached stderr: %s", name, stderr.String())
		}
	}
}

// TestMakeMigrationsUpdate: a unique index the rows can't take fails
// migrate's preflight before touching the schema; made plain, --update
// rewrites the migration in place and migrate applies it. --update
// refuses a migration holding a person's code.
func TestMakeMigrationsUpdate(t *testing.T) {
	root := modelsApp(t)
	t.Setenv("GOWORK", "off")
	dbPath := filepath.Join(root, "app.db")
	t.Setenv("DATABASE_URL", "sqlite:"+dbPath)
	run := func(f func(stdin io.Reader, stdout, stderr io.Writer) error) (string, string, error) {
		var out, errb bytes.Buffer
		err := f(nil, &out, &errb)
		return out.String(), errb.String(), err
	}
	mk := func(args ...string) (string, string, error) {
		return run(func(in io.Reader, out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root, "-q", "--noinput"}, args...), in, out, errb)
		})
	}
	migrate := func() (string, string, error) {
		return run(func(in io.Reader, out, errb io.Writer) error {
			return runMigrate([]string{"--root", root, "-q"}, in, out, errb)
		})
	}
	must := func(out, errs string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\n%s\n%s", err, out, errs)
		}
		return out
	}
	must(mk())
	must(migrate())
	d, err := orm.ConnectURL("sqlite:" + dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL().Exec("INSERT INTO users (email) VALUES ('a@x.com'), ('A@x.com'), ('b@x.com')"); err != nil {
		t.Fatal(err)
	}
	d.SQL().Close()

	userGo := filepath.Join(root, "models", "user.go")
	user, _ := os.ReadFile(userGo)
	withIndex := func(unique string) {
		writeFile(t, userGo, string(user)+"\nfunc (User) Indexes() []orm.Index {\n\treturn []orm.Index{orm.IndexOf(orm.Lower.Of(orm.F(\"email\")))"+unique+".Name(\"users_email_lower\")}\n}\n")
	}
	withIndex(".Unique()")
	file := filepath.Join(root, "migrations", "0002_add_index_users_email_lower_on_users.go")
	if out := must(mk()); !strings.Contains(out, "wrote "+file) {
		t.Fatalf("makemigrations: %s", out)
	}
	_, errs, err := migrate()
	if err == nil || !strings.Contains(errs, "can't add index users_email_lower on users: 1 value of its keys is duplicated in 2 rows of users:\n    \"a@x.com\"  (2 rows)") {
		t.Fatalf("migrate with duplicates: %v\n%s", err, errs)
	}

	withIndex("")
	if out := must(mk("--update", "--dry-run")); !strings.Contains(out, "// "+file+" (updated)\n") || !strings.Contains(out, `CREATE INDEX "users_email_lower"`) {
		t.Fatalf("--update --dry-run: %s", out)
	}
	if out, errs, err := mk("--update", "--check"); err == nil || !strings.Contains(errs, "--update would rewrite "+file) {
		t.Fatalf("--update --check: %v\n%s%s", err, out, errs)
	}
	if out := must(mk("--update")); !strings.Contains(out, "Updated "+file+"\n  - add index users_email_lower on users\n") {
		t.Fatalf("--update: %s", out)
	}
	if src, _ := os.ReadFile(file); strings.Contains(string(src), "UNIQUE") {
		t.Fatalf("still unique:\n%s", src)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "migrations")); len(entries) != 2 {
		t.Fatalf("migrations: %v", entries)
	}
	if out := must(mk("--update")); !strings.Contains(out, "No changes") {
		t.Fatalf("--update again: %s", out)
	}
	must(migrate())

	withIndex(".Unique()")
	again := filepath.Join(root, "migrations", "0003_auto.go")
	if out := must(mk()); !strings.Contains(out, "wrote "+again) {
		t.Fatalf("makemigrations, unique again: %s", out)
	}
	withIndex("")
	if out := must(mk("--update", "--dry-run")); !strings.Contains(out, "Would remove "+again) {
		t.Fatalf("--update --dry-run, undone: %s", out)
	}
	if out := must(mk("--update")); !strings.Contains(out, "Removed "+again+": with the models' changes it has nothing left to do.") {
		t.Fatalf("--update, undone: %s", out)
	}
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Fatalf("%s is still there", again)
	}
	if out := must(mk()); !strings.Contains(out, "No changes") {
		t.Fatalf("makemigrations after the removal: %s", out)
	}

	must(mk("--empty", "seed"))
	writeFile(t, userGo, string(user))
	if _, errs, err := mk("--update"); err == nil || !strings.Contains(errs, "can't rewrite 0003_seed: it holds run Go, written by hand") {
		t.Fatalf("--update over a RunGo: %v\n%s", err, errs)
	}
}

// TestManagedColumnsCommands: makemigrations writes only the columns an
// unmanaged model's Meta.Managed names, never its table; sqlmigrate shows
// them added to the table there; dropped from Managed, the column is
// forgotten without SQL.
func TestManagedColumnsCommands(t *testing.T) {
	root := modelsApp(t)
	session := filepath.Join(root, "legacy", "session.go")
	src := `package legacy

import (
	"time"

	"github.com/vuka-lang/orm/v2"
)

type Session struct {
	orm.Model[Session]
	ID        int64
	Token     string
	UnlocksAt *time.Time
}

func (Session) Meta() orm.Meta {
	return orm.Meta{Table: "sessions", Unmanaged: true, Managed: []string{"UnlocksAt"}}
}
`
	writeFile(t, session, src)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\t_ \"example.com/shop/legacy\"\n\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\nfunc main() {}\n")
	t.Setenv("GOWORK", "off")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
	run := func(f func(stdout, stderr io.Writer) error) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := f(&out, &errb); err != nil {
			t.Fatalf("%v\n%s\n%s", err, out.String(), errb.String())
		}
		return out.String()
	}
	mk := func(args ...string) string {
		return run(func(out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root, "--noinput"}, args...), nil, out, errb)
		})
	}
	sqlmigrate := func(name string) string {
		return run(func(out, errb io.Writer) error { return runSQLMigrate([]string{"--root", root, name}, out, errb) })
	}
	mk()
	initial, _ := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
	if !strings.Contains(string(initial), `m.AddField{Table: "sessions", Name: "unlocks_at", Field: m.Time().Null()}`) || strings.Contains(string(initial), `Table: "sessions", Fields`) || strings.Contains(string(initial), `"token"`) {
		t.Fatalf("0001_initial.go:\n%s", initial)
	}
	if out := mk("--check"); !strings.Contains(out, "No changes") {
		t.Fatalf("--check: %s", out)
	}
	if out := sqlmigrate("0001"); !strings.Contains(out, `ALTER TABLE "sessions" ADD COLUMN "unlocks_at" DATETIME;`) || strings.Contains(out, `CREATE TABLE "sessions"`) {
		t.Fatalf("sqlmigrate 0001:\n%s", out)
	}
	d, err := orm.ConnectURL("sqlite:" + filepath.Join(root, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL().Exec(`CREATE TABLE "sessions" ("id" INTEGER PRIMARY KEY, "token" TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	d.SQL().Close()
	run(func(out, errb io.Writer) error { return runMigrate([]string{"--root", root}, nil, out, errb) })

	writeFile(t, session, strings.Replace(src, `, Managed: []string{"UnlocksAt"}`, "", 1))
	if out := mk(); !strings.Contains(out, "forget field sessions.unlocks_at (the column stays)") {
		t.Fatalf("makemigrations after Managed dropped: %s", out)
	}
	if out := sqlmigrate("0002"); strings.Contains(out, "unlocks_at") {
		t.Fatalf("sqlmigrate 0002 runs SQL: %s", out)
	}
	run(func(out, errb io.Writer) error { return runMigrate([]string{"--root", root}, nil, out, errb) })
	if out := mk("--check"); !strings.Contains(out, "No changes") {
		t.Fatalf("--check after: %s", out)
	}
	d, _ = orm.ConnectURL("sqlite:" + filepath.Join(root, "app.db"))
	defer d.SQL().Close()
	if _, err := d.SQL().Exec(`SELECT "unlocks_at" FROM "sessions"`); err != nil {
		t.Fatalf("the forgotten column went: %v", err)
	}
}

// TestFakeMigrate: migrate --fake records a migration by name or number
// (of the default database, or --db's) without making its tables, refuses
// one ahead of its dependencies, and --fake-initial records the
// migrations whose tables exist.
func TestFakeMigrate(t *testing.T) {
	root := modelsApp(t)
	writeFile(t, filepath.Join(root, "shop", "shop.go"), `package shop

import "github.com/vuka-lang/orm/v2"

type Event struct {
	orm.Model[Event]
	ID   int64
	Kind string
}

func (Event) Meta() orm.Meta { return orm.Meta{DB: "analytics"} }
`)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\t_ \"example.com/shop/shop\"\n\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\nfunc main() {}\n")
	t.Setenv("GOWORK", "off")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
	t.Setenv("DATABASE_URL_ANALYTICS", "sqlite:"+filepath.Join(root, "analytics.db"))
	var stderr string
	try := func(f func(stdout, stderr io.Writer) error) (string, error) {
		var out, errb bytes.Buffer
		err := f(&out, &errb)
		stderr = errb.String()
		return out.String(), err
	}
	must := func(what string, f func(stdout, stderr io.Writer) error) string {
		t.Helper()
		out, err := try(f)
		if err != nil {
			t.Fatalf("%s: %v\n%s\n%s", what, err, out, stderr)
		}
		return out
	}
	migrate := func(args ...string) error {
		_, err := try(func(out, errb io.Writer) error {
			return runMigrate(append([]string{"--root", root}, args...), nil, out, errb)
		})
		return err
	}
	show := func() string {
		return must("showmigrations", func(out, errb io.Writer) error { return runShowMigrations([]string{"--root", root}, out, errb) })
	}
	for _, args := range [][]string{{"--noinput"}, {"--noinput", "--db", "analytics"}, {"--empty", "seed"}} {
		must("makemigrations", func(out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root}, args...), nil, out, errb)
		})
	}
	tables := func(file string) []string {
		d, err := orm.ConnectURL("sqlite:" + filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		defer d.SQL().Close()
		rows, err := d.SQL().Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			out = append(out, n)
		}
		return out
	}

	if err := migrate("--fake"); err == nil || !strings.Contains(err.Error(), "name one") {
		t.Fatalf("--fake without a target: %v", err)
	}
	if err := migrate("--fake", "0002"); err == nil || !strings.Contains(stderr, "can't fake 0002_seed: it depends on 0001_initial, not applied") {
		t.Fatalf("--fake ahead of a dependency: %v\n%s", err, stderr)
	}
	if err := migrate("--fake", "0001_initial"); err != nil || !strings.Contains(stderr, "✓ Faked 0001_initial as applied on the default database  ") {
		t.Fatalf("--fake 0001_initial: %v\n%s", err, stderr)
	}
	if err := migrate("--fake", "0001", "--db", "analytics"); err != nil || !strings.Contains(stderr, "✓ Faked 0001_initial as applied on database analytics  ") {
		t.Fatalf("--fake 0001 --db analytics: %v\n%s", err, stderr)
	}
	if out := show(); !strings.Contains(out, "Database analytics\n [X] 0001_initial") || !strings.Contains(out, "The default database\n [X] 0001_initial\n [ ] 0002_seed") {
		t.Fatalf("showmigrations after --fake: %s", out)
	}
	if got := tables("app.db"); !slices.Equal(got, []string{"orm_migrations"}) {
		t.Fatalf("--fake made tables: %q", got)
	}
	if got := tables("analytics.db"); !slices.Equal(got, []string{"orm_migrations"}) {
		t.Fatalf("--fake --db analytics made tables: %q", got)
	}

	// Back to zero (recorded, nothing dropped), the tables made for real,
	// unrecorded again: --fake-initial records 0001 and applies 0002.
	if err := migrate("--fake", "zero", "--noinput"); err != nil || !strings.Contains(stderr, "✓ Faked 0001_initial as unapplied on the default database  ") {
		t.Fatalf("--fake zero: %v\n%s", err, stderr)
	}
	if err := migrate("0001"); err != nil {
		t.Fatalf("migrate 0001: %v\n%s", err, stderr)
	}
	if err := migrate("--fake", "zero", "--noinput"); err != nil {
		t.Fatalf("--fake zero: %v\n%s", err, stderr)
	}
	if got := tables("app.db"); !slices.Contains(got, "users") || !slices.Contains(got, "posts") {
		t.Fatalf("--fake zero dropped tables: %q", got)
	}
	if err := migrate("--fake-initial"); err != nil || !strings.Contains(stderr, "✓ Faked 0001_initial as applied on the default database  ") || !strings.Contains(stderr, "✓ Applied 0002_seed on the default database  ") {
		t.Fatalf("--fake-initial: %v\n%s", err, stderr)
	}
	if out := show(); !strings.Contains(out, " [X] 0001_initial\n [X] 0002_seed") {
		t.Fatalf("showmigrations after --fake-initial: %s", out)
	}
}

// toolMainPath is the import path of the main package of the migration
// tool the test's ORM_CACHE holds (go version -m reads it from the binary).
func toolMainPath(t *testing.T) string {
	t.Helper()
	tools, _ := filepath.Glob(filepath.Join(os.Getenv("ORM_CACHE"), "tool", "*", "tool-*"))
	if len(tools) == 0 {
		t.Fatal("no tool in the cache")
	}
	sort.Slice(tools, func(i, j int) bool {
		a, _ := os.Stat(tools[i])
		b, _ := os.Stat(tools[j])
		return a.ModTime().After(b.ModTime())
	})
	out, err := exec.Command("go", "version", "-m", tools[0]).Output()
	if err != nil {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "path" {
			return f[1]
		}
	}
	t.Fatalf("no path in %s", out)
	return ""
}

// TestMinimalToolFallsBack: sqlmigrate prints the same SQL from the small
// tool and from the whole program (ORM_TOOL=full), and a model declared in
// the main package, which no tool can import, keeps the whole program as
// the tool.
func TestMinimalToolFallsBack(t *testing.T) {
	root := modelsApp(t)
	t.Setenv("GOWORK", "off")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
	do := func(f func(out, errb io.Writer) error) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := f(&out, &errb); err != nil {
			t.Fatalf("%v\n%s\n%s", err, out.String(), errb.String())
		}
		return out.String()
	}
	mk := func(args ...string) string {
		return do(func(out, errb io.Writer) error {
			return runMakeMigrations(append([]string{"--root", root, "--noinput"}, args...), nil, out, errb)
		})
	}
	sql := func() string {
		return do(func(out, errb io.Writer) error { return runSQLMigrate([]string{"--root", root, "0001"}, out, errb) })
	}
	mk()
	small := sql()
	if got := toolMainPath(t); got != "example.com/shop/zz_ormtool" {
		t.Fatalf("the tool's main package is %s", got)
	}
	t.Setenv("ORM_TOOL", "full")
	if full := sql(); full != small || !strings.Contains(small, `CREATE TABLE "posts"`) {
		t.Fatalf("sqlmigrate differs between tools:\nsmall:\n%s\nfull:\n%s", small, full)
	}
	if got := toolMainPath(t); got != "example.com/shop" {
		t.Fatalf("ORM_TOOL=full built %s", got)
	}

	t.Setenv("ORM_TOOL", "")
	writeFile(t, filepath.Join(root, "main.go"), `package main

import (
	_ "example.com/shop/blog"
	"github.com/vuka-lang/orm/v2"
	_ "github.com/vuka-lang/orm/v2/sqlite"
)

type Tag struct {
	orm.Model[Tag]
	ID   int64
	Name string
}

func main() {}
`)
	if out := mk("--dry-run"); !strings.Contains(out, `"tags"`) {
		t.Fatalf("a model of the main package is missing from the plan:\n%s", out)
	}
	if got := toolMainPath(t); got != "example.com/shop" {
		t.Fatalf("a model in main: the tool is %s", got)
	}
}

// TestVukaGenOverlayProbe: the tool asks vuka gen for an overlay of the
// module's own tree; a vuka without the option (or without -code-only)
// answers with a flag error the tool takes for "build a mirror".
func TestVukaGenOverlayProbe(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	for name, script := range map[string]string{
		"new":      "echo \"$@\" >> " + log + "\n",
		"noflag":   "case \"$*\" in *-overlay-out*) echo 'flag provided but not defined: -overlay-out' >&2; exit 2;; esac\necho \"$@\" >> " + log + "\n",
		"nocode":   "case \"$*\" in *-code-only*) echo 'flag provided but not defined: -code-only' >&2; exit 2;; esac\necho \"$@\" >> " + log + "\n",
		"genfails": "echo 'type error in main.vuka' >&2; exit 1\n",
	} {
		os.Remove(log)
		bin := filepath.Join(dir, name)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VUKA", bin)
		var stderr bytes.Buffer
		ok, err := vukaGenOverlay(dir, filepath.Join(dir, "overlay.json"), &stderr)
		calls, _ := os.ReadFile(log)
		switch name {
		case "new":
			if !ok || err != nil || string(calls) != "gen -code-only -overlay-out "+filepath.Join(dir, "overlay.json")+"\n" {
				t.Errorf("new vuka: %v %v %q", ok, err, calls)
			}
		case "noflag", "nocode":
			if ok || err != nil || stderr.Len() > 0 {
				t.Errorf("%s vuka: ok=%v err=%v stderr=%q", name, ok, err, stderr.String())
			}
		case "genfails":
			if ok || err == nil || !strings.Contains(stderr.String(), "type error in main.vuka") {
				t.Errorf("a failing gen: ok=%v err=%v stderr=%q", ok, err, stderr.String())
			}
		}
	}
}

// TestVukaMinimalTool: a module with Vuka models in a package of their own
// and Go models in another, no model in main, is migrated by the small tool
// through vuka gen's overlay (a vuka with -overlay-out) and through its
// mirror (a vuka without), with the same migration and the same SQL.
func TestVukaMinimalTool(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	vuka := os.Getenv("VUKA")
	if vuka == "" {
		t.Skip("set VUKA to a vuka command with field attributes")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	sum, _ := os.ReadFile(filepath.Join(repo, "go.sum"))
	// A vuka that refuses -overlay-out, whatever the real one does.
	oldVuka := filepath.Join(t.TempDir(), "vuka-old")
	shim := "#!/bin/sh\ncase \"$*\" in *-overlay-out*) echo 'flag provided but not defined: -overlay-out' >&2; exit 2;; esac\nexec " + vuka + " \"$@\"\n"
	if err := os.WriteFile(oldVuka, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod": "module example.com/notes\n\ngo 1.27\n\nrequire (\n\tgithub.com/vuka-lang/orm/v2 v2.0.0\n\tgithub.com/vuka-lang/vuka/v2 " + vukaRequire(repo) + "\n)\n\nreplace github.com/vuka-lang/orm/v2 => " + repo + "\n" + vukaReplace(repo),
		"go.sum": string(sum),
		"main.vuka": `package main

import (
	_ "example.com/notes/notes"
	_ "example.com/notes/people"
	_ "github.com/vuka-lang/orm/v2/sqlite"
)

func main() {}
`,
		"notes/notes.vuka": `package notes

import "github.com/vuka-lang/orm/v2"

type Note struct {
	orm.Base
	ID   int64
	Text string @orm.LongText
}
`,
		"people/people.go": `package people

import "github.com/vuka-lang/orm/v2"

type Person struct {
	orm.Model[Person]
	ID   int64
	Name string
}
`,
	}
	var sqls, migs []string
	for _, v := range []struct{ name, vuka, probe string }{{"overlay", vuka, "overlay"}, {"mirror", oldVuka, "mirror"}} {
		root := t.TempDir()
		t.Setenv("ORM_CACHE", t.TempDir())
		t.Setenv("VUKA", v.vuka)
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "-mod=mod")
		t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))
		for name, content := range files {
			writeFile(t, filepath.Join(root, name), content)
		}
		var out, errb bytes.Buffer
		if err := runMakeMigrations([]string{"--root", root, "--noinput", "initial"}, nil, &out, &errb); err != nil {
			t.Fatalf("%s: makemigrations: %v\n%s\n%s", v.name, err, out.String(), errb.String())
		}
		if got := toolMainPath(t); got != "example.com/notes/zz_ormtool" {
			t.Errorf("%s: the tool's main package is %s", v.name, got)
		}
		mig, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
		if err != nil || !strings.Contains(string(mig), `"notes"`) || !strings.Contains(string(mig), `"persons"`) {
			t.Fatalf("%s: 0001_initial.go (a Vuka model and a Go model): %v\n%s", v.name, err, mig)
		}
		out.Reset()
		if err := runSQLMigrate([]string{"--root", root, "0001"}, &out, &errb); err != nil {
			t.Fatalf("%s: sqlmigrate: %v\n%s", v.name, err, errb.String())
		}
		sqls, migs = append(sqls, out.String()), append(migs, string(mig))
	}
	if sqls[0] != sqls[1] || migs[0] != migs[1] {
		t.Errorf("overlay and mirror differ:\n%s\n---\n%s", sqls[0], sqls[1])
	}
}
