// Command orm is the ORM's tool: it writes the generated code of a
// module's models and drives its migrations, Django's manage.py commands.
//
//	go run github.com/vuka-lang/orm/cmd/orm generate [dir]
//	go run github.com/vuka-lang/orm/cmd/orm makemigrations [name] [--check] [--dry-run] [--empty] [--db name]
//	go run github.com/vuka-lang/orm/cmd/orm migrate [target] [--db name]
//	go run github.com/vuka-lang/orm/cmd/orm showmigrations
//	go run github.com/vuka-lang/orm/cmd/orm sqlmigrate <name> [--backwards] [--db name]
//
// The migration commands run the program as their tool: its main package
// is built with an init overlaid that serves orm.ServeCLI before main runs
// (main never does), imports of its migrations packages, and the generated
// code orm generate would write, then run with the request in orm.CLIEnv.
// They connect by URL (orm.ConnectURL): --database name=URL, else
// DATABASE_URL for the default database and DATABASE_URL_<NAME> for
// another. The program links the drivers it opens (orm/postgres, …).
// A module with .vuka files is built through vuka gen (the vuka command,
// or $VUKA).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormgen"
)

const usage = `orm is the ORM's tool.

Usage:

	orm generate [--tests] [--check] [dir]
	orm makemigrations [flags] [name]
	orm migrate [flags] [target]
	orm showmigrations [flags]
	orm sqlmigrate [flags] <name>

Run "orm <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "generate":
		err = runGenerate(args, os.Stdout, os.Stderr)
	case "makemigrations":
		err = runMakeMigrations(args, os.Stdin, os.Stdout, os.Stderr)
	case "migrate":
		err = runMigrate(args, os.Stdin, os.Stdout, os.Stderr)
	case "showmigrations":
		err = runShowMigrations(args, os.Stdout, os.Stderr)
	case "sqlmigrate":
		err = runSQLMigrate(args, os.Stdout, os.Stderr)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "orm: no command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orm:", err)
		os.Exit(1)
	}
}

// interspersed parses args with fs, flags before or after the
// positional arguments, which it returns.
func interspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func runGenerate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orm generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tests := fs.Bool("tests", false, "also the models of _test.go files")
	check := fs.Bool("check", false, "fail when a file is out of date, writing nothing (CI)")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	changed, err := ormgen.Write(dir, ormgen.Config{Tests: *tests, Skipped: func(model, reason string) {
		fmt.Fprintf(stderr, "  %s: read by reflection (%s)\n", model, reason)
	}}, *check)
	for _, p := range changed {
		if *check {
			fmt.Fprintln(stderr, "out of date:", p)
		} else {
			fmt.Fprintln(stdout, "wrote", p)
		}
	}
	if err == nil && *check && len(changed) > 0 {
		return errors.New("generated code is out of date: run orm generate")
	}
	return err
}

// project is the flags every migration command shares: where the program
// is and how to reach its databases.
type project struct {
	root, main, def string
	dbs             dbFlags
}

type dbFlags map[string]string

func (d dbFlags) String() string { return "" }

func (d dbFlags) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || url == "" {
		return fmt.Errorf("--database takes name=URL, not %q", v)
	}
	if name == "default" {
		name = ""
	}
	d[name] = url
	return nil
}

func (p *project) flags(fs *flag.FlagSet) {
	p.dbs = dbFlags{}
	fs.StringVar(&p.root, "root", ".", "the program's module (a directory in it)")
	fs.StringVar(&p.main, "main", "", "the program's main package directory (default: the module's only one)")
	fs.StringVar(&p.def, "default", os.Getenv("ORM_DEFAULT_DB"), "the default database's name, as models name it with orm.On (env ORM_DEFAULT_DB)")
	fs.Var(p.dbs, "database", "name=URL of a database (repeatable; name default or empty for the default); else DATABASE_URL, DATABASE_URL_<NAME>")
}

// moduleRoot is the directory of the go.mod at or above dir.
func moduleRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod at or above %s", dir)
		}
	}
}

// urls is each database's URL by its name ("" the default): --database's,
// then DATABASE_URL and DATABASE_URL_<NAME> of the environment, then of the
// module's .env file.
func (p *project) urls() map[string]string {
	out := map[string]string{}
	if root, err := moduleRoot(p.root); err == nil {
		dotEnv(filepath.Join(root, ".env"), func(k, v string) {
			if _, set := os.LookupEnv(k); !set && (k == "DATABASE_URL" || strings.HasPrefix(k, "DATABASE_URL_")) {
				os.Setenv(k, v) // the program run as the tool reads them too
			}
		})
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "DATABASE_URL":
			out[""] = v
		case strings.HasPrefix(k, "DATABASE_URL_") && v != "":
			out[strings.ToLower(strings.TrimPrefix(k, "DATABASE_URL_"))] = v
		}
	}
	for n, u := range p.dbs {
		out[n] = u
	}
	if p.def != "" {
		if u, ok := out[""]; ok {
			if _, named := out[p.def]; !named {
				out[p.def] = u
			}
		} else if u, ok := out[p.def]; ok {
			out[""] = u
		}
	}
	return out
}

// dotEnv calls set for each KEY=value line of the env file at path, if
// there is one: blank lines and # comments skipped, an export prefix and
// quotes around the value removed, an unquoted value's " #" comment cut.
// No expansion: what a database URL needs.
func dotEnv(path string, set func(k, v string)) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || line[0] == '#' {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0]:
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		set(k, v)
	}
}

// target is the database a migration is for: its name ("" the default),
// dialect, and the orm.On names of its models.
type target struct {
	name, dialect string
	models        []string
}

func (t target) label() string {
	if t.name == "" {
		return "the default database"
	}
	return "database " + t.name
}

func (p *project) target(db, dialect string) (target, error) {
	if db == "default" || db == p.def {
		db = ""
	}
	t := target{name: db, dialect: dialect}
	if t.dialect == "" {
		url, ok := p.urls()[db]
		if !ok {
			return t, fmt.Errorf("no dialect for %s: pass --dialect, or its URL (--database, DATABASE_URL)", t.label())
		}
		d, _, err := orm.ParseURL(url)
		if err != nil {
			return t, err
		}
		t.dialect = d
	}
	// A MySQL server's version may follow (mysql/9.3.0): it says whether
	// vectors are VECTOR columns.
	switch base, _, versioned := strings.Cut(t.dialect, "/"); {
	case !versioned && (base == "postgres" || base == "postgresql" || base == "pgx"):
		t.dialect = "postgres"
	case base == "mysql", base == "sqlite" && !versioned:
	default:
		return t, fmt.Errorf("dialect %q: postgres, mysql (or mysql/<server version>) or sqlite", t.dialect)
	}
	if db == "" {
		t.models = append(t.models, "")
		if p.def != "" {
			t.models = append(t.models, p.def)
		}
	} else {
		t.models = []string{db}
	}
	return t, nil
}

func runMakeMigrations(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orm makemigrations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p project
	p.flags(fs)
	db := fs.String("db", "", "the database (default: the default database)")
	dialect := fs.String("dialect", "", "postgres | mysql[/<server version>] | sqlite (default: the database URL's)")
	dir := fs.String("dir", "", "migrations directory (default migrations, migrations/<db> for a database that isn't the default)")
	check := fs.Bool("check", false, "fail when the models have changes no migration holds, writing nothing (CI)")
	empty := fs.Bool("empty", false, "write a migration with an empty RunGo, for a data migration")
	dryRun := fs.Bool("dry-run", false, "print the migration instead of writing it")
	noinput := fs.Bool("noinput", false, "ask nothing: a rename is written as a removal and an addition")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: orm makemigrations [flags] [name]

Write the next migration of the program's models as Go. The program is
built and run as a tool (its main never runs): it replays the migrations
its migrations package registers into the schema they leave, compares it
with its models (the types embedding orm.Model; managed ones, of the
database), and writes the operations between as <dir>/NNNN_<name>.go.
Import the package in the program so orm.Migrate applies them.

A table or column gone where one alike appeared may have been renamed: on
a terminal makemigrations asks; --noinput answers no, writing a removal
and an addition with a note. Read the file before committing it.

`)
		fs.PrintDefaults()
	}
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	if name != "" && !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(name) {
		return fmt.Errorf("a migration name is letters, digits and _: %q", name)
	}
	root, err := moduleRoot(p.root)
	if err != nil {
		return err
	}
	t, err := p.target(*db, *dialect)
	if err != nil {
		return err
	}
	out := *dir
	switch {
	case out == "" && t.name == "":
		out = filepath.Join(root, "migrations")
	case out == "":
		out = filepath.Join(root, "migrations", t.name)
	case !filepath.IsAbs(out):
		out = filepath.Join(root, out)
	}
	next, err := nextMigration(out)
	if err != nil {
		return err
	}
	pkg, err := migrationsPackage(out, t.name)
	if err != nil {
		return err
	}
	req := orm.CLIRequest{Command: "plan", DB: t.name, Models: t.models, Dialect: t.dialect,
		Number: next, Name: name, Package: pkg, Empty: *empty, Interactive: !*noinput && isTerminal(stdin)}
	var res orm.PlanResult
	if err := runTool(root, p.main, req, stdin, stdout, stderr, &res); err != nil {
		return err
	}
	if len(res.Ops) == 0 {
		fmt.Fprintf(stdout, "No changes in the models of %s.\n", t.label())
		return nil
	}
	if *check {
		fmt.Fprintf(stderr, "The models of %s have changes no migration holds:\n", t.label())
		for _, op := range res.Ops {
			fmt.Fprintf(stderr, "  - %s\n", op)
		}
		return errors.New("run orm makemigrations")
	}
	path := filepath.Join(out, res.File)
	if *dryRun {
		fmt.Fprintf(stdout, "// %s\n%s", path, res.Source)
		return nil
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, res.Source, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	for _, op := range res.Ops {
		fmt.Fprintf(stdout, "  - %s\n", op)
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Fprintf(stderr, "  ! %s\n", n)
		}
	}
	return nil
}

// alwaysAsk has makemigrations ask on any stdin, not only a terminal: for
// tests.
var alwaysAsk bool

func isTerminal(r io.Reader) bool {
	if alwaysAsk {
		return true
	}
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runMigrate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orm migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p project
	p.flags(fs)
	db := fs.String("db", "", "the database a target is of (default: the default database)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: orm migrate [flags] [target]

Apply the program's migrations: every database's pending ones, each after
its dependencies, as orm.Migrate does. With a target (a migration's name
or number), take one database (--db) to it: the migrations up to it
applied, those after it unapplied, last first; "zero" unapplies them all.

`)
		fs.PrintDefaults()
	}
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	req := orm.CLIRequest{Command: "migrate"}
	if len(pos) > 0 {
		req.Target = pos[0]
	}
	if *db != "default" && *db != p.def {
		req.DB = *db
	}
	return p.runConnected(req, stdin, stdout, stderr)
}

func runShowMigrations(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orm showmigrations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p project
	p.flags(fs)
	if _, err := interspersed(fs, args); err != nil {
		return err
	}
	return p.runConnected(orm.CLIRequest{Command: "show"}, nil, stdout, stderr)
}

func runSQLMigrate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("orm sqlmigrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p project
	p.flags(fs)
	db := fs.String("db", "", "the database (default: the default database)")
	dialect := fs.String("dialect", "", "postgres | mysql[/<server version>] | sqlite (default: the database URL's)")
	backwards := fs.Bool("backwards", false, "the SQL unapplying the migration")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: orm sqlmigrate [flags] <name>")
	}
	root, err := moduleRoot(p.root)
	if err != nil {
		return err
	}
	t, err := p.target(*db, *dialect)
	if err != nil {
		return err
	}
	req := orm.CLIRequest{Command: "sql", DB: t.name, Dialect: t.dialect, Target: pos[0], Backwards: *backwards}
	return runTool(root, p.main, req, nil, stdout, stderr, nil)
}

// runConnected runs a command that connects to the databases.
func (p *project) runConnected(req orm.CLIRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	root, err := moduleRoot(p.root)
	if err != nil {
		return err
	}
	req.Conns = p.urls()
	if len(req.Conns) == 0 {
		return errors.New("no database: pass --database name=URL or set DATABASE_URL")
	}
	return runTool(root, p.main, req, stdin, stdout, stderr, nil)
}

var migrationFileRE = regexp.MustCompile(`^(\d+)_[A-Za-z0-9_]+\.go$`)

// nextMigration is the number after the highest in dir.
func nextMigration(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	high := 0
	for _, e := range entries {
		if m := migrationFileRE.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			high = max(high, n)
		}
	}
	return high + 1, nil
}

// migrationsPackage is the package of the migrations in dir: the one its
// files declare, else migrations for the default database's and the
// database's name (as a Go name) for another's.
func migrationsPackage(dir, db string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.PackageClauseOnly)
			if err == nil {
				return f.Name.Name, nil
			}
		}
	}
	if db == "" {
		return "migrations", nil
	}
	name := regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(strings.ToLower(db), "_")
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "m" + name
	}
	return name, nil
}

// goList is the packages go list reports for patterns in dir: import path,
// name and directory.
type listed struct{ ImportPath, Name, Dir string }

func goList(dir string, patterns ...string) ([]listed, error) {
	cmd := exec.Command("go", append([]string{"list", "-e", "-f", "{{.ImportPath}}\t{{.Name}}\t{{.Dir}}"}, patterns...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("go list: %s", ee.Stderr)
		}
		return nil, err
	}
	var pkgs []listed
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 3)
		if len(f) == 3 && f[1] != "" {
			pkgs = append(pkgs, listed{f[0], f[1], f[2]})
		}
	}
	return pkgs, sc.Err()
}

// mainDir is the program's main package: main's (relative to root), else
// the module's only one.
func mainDir(root, main string) (string, error) {
	if main != "" {
		if !filepath.IsAbs(main) {
			main = filepath.Join(root, main)
		}
		return main, nil
	}
	pkgs, err := goList(root, "./...")
	if err != nil {
		return "", err
	}
	var mains []string
	for _, p := range pkgs {
		if p.Name == "main" && !strings.Contains(p.ImportPath, "/cmd/orm") {
			mains = append(mains, p.Dir)
		}
	}
	switch len(mains) {
	case 0:
		return "", errors.New("no main package: the migration tool runs the program, so it needs one (or pass --main)")
	case 1:
		return mains[0], nil
	}
	sort.Strings(mains)
	return "", fmt.Errorf("several main packages (%s): pass --main", strings.Join(mains, ", "))
}

// migrationsImports is the import paths of the program's migrations
// packages: migrations and the packages under it.
func migrationsImports(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "migrations")); err != nil {
		return nil, nil
	}
	pkgs, err := goList(root, "./migrations/...")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pkgs {
		out = append(out, p.ImportPath)
	}
	sort.Strings(out)
	return out, nil
}

// toolFileName is the tool's file in the main package, named to sort
// first so its init runs before the program's own.
const toolFileName = "0_orm_tool.go"

// lateToolFileName is the tool's file in a main package declaring models,
// named to sort last: its init runs after the inits registering them.
const lateToolFileName = "zz_orm_tool.go"

// toolFile is the tool's source: orm.ServeCLI, and imports of the
// migrations packages so their migrations register.
func toolFile(imports []string) []byte {
	var b strings.Builder
	b.WriteString("// Code generated by the orm command; DO NOT EDIT.\n\npackage main\n\nimport (\n\tormtool \"github.com/vuka-lang/orm\"\n")
	for _, p := range imports {
		b.WriteString("\t_ " + strconv.Quote(p) + "\n")
	}
	b.WriteString(")\n\nfunc init() { ormtool.ServeCLI() }\n")
	return []byte(b.String())
}

// runTool builds the program as the ORM's tool and runs it for req: its
// output to stdout and stderr, questions read from stdin, and plan's
// result into res.
func runTool(root, main string, req orm.CLIRequest, stdin io.Reader, stdout, stderr io.Writer, res *orm.PlanResult) error {
	tmp, err := os.MkdirTemp("", "orm-tool-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	runDir := root
	if hasVuka(root) {
		// Vuka models: build the module's Go form, vuka gen's mirror.
		if main != "" && !filepath.IsAbs(main) {
			main = filepath.Join(root, main)
		}
		mirror := filepath.Join(tmp, "src")
		gen := exec.Command(vukaCommand(), "gen", "-o", mirror)
		gen.Dir, gen.Stdout, gen.Stderr = root, io.Discard, stderr
		if err := gen.Run(); err != nil {
			return fmt.Errorf("vuka gen (the module has .vuka files; set VUKA to the vuka command): %w", err)
		}
		if main != "" {
			rel, err := filepath.Rel(root, main)
			if err != nil {
				return err
			}
			main = filepath.Join(mirror, rel)
		}
		root = mirror
	}
	dir, err := mainDir(root, main)
	if err != nil {
		return err
	}

	overlay := struct{ Replace map[string]string }{Replace: map[string]string{}}
	gen, err := ormgen.Missing(root, ormgen.Config{})
	if err != nil {
		return err
	}
	i := 0
	for path, content := range gen {
		shadow := filepath.Join(tmp, fmt.Sprintf("gen%d.go", i))
		i++
		if err := os.WriteFile(shadow, content, 0o644); err != nil {
			return err
		}
		overlay.Replace[path] = shadow
	}
	imports, err := migrationsImports(root)
	if err != nil {
		return err
	}
	// Models the main package declares itself register in its own inits
	// (generated code, Vuka's field attributes): the tool runs after them.
	name := toolFileName
	vukaGo, _ := filepath.Glob(filepath.Join(dir, "*_vuka.go"))
	for path := range gen {
		if filepath.Dir(path) == dir {
			vukaGo = append(vukaGo, path)
		}
	}
	if len(vukaGo) > 0 {
		name = lateToolFileName
	}
	shadow := filepath.Join(tmp, name)
	if err := os.WriteFile(shadow, toolFile(imports), 0o644); err != nil {
		return err
	}
	overlay.Replace[filepath.Join(dir, name)] = shadow
	overlayPath := filepath.Join(tmp, "overlay.json")
	raw, _ := json.Marshal(overlay)
	if err := os.WriteFile(overlayPath, raw, 0o644); err != nil {
		return err
	}

	bin := filepath.Join(tmp, "tool")
	build := exec.Command("go", "build", "-overlay", overlayPath, "-o", bin, ".")
	build.Dir = dir
	build.Stdout, build.Stderr = stderr, stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building the program as the migration tool: %w", err)
	}
	if res != nil {
		req.Out = filepath.Join(tmp, "plan.json")
	}
	reqJSON, _ := json.Marshal(req)
	run := exec.Command(bin)
	run.Dir = runDir
	run.Env = append(os.Environ(), orm.CLIEnv+"="+string(reqJSON))
	run.Stdin, run.Stdout, run.Stderr = stdin, stdout, stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("running the migration tool: %w", err)
	}
	if res == nil {
		return nil
	}
	out, err := os.ReadFile(req.Out)
	if err != nil {
		return fmt.Errorf("the tool wrote no plan (does the program's main package link the orm?): %w", err)
	}
	return json.Unmarshal(out, res)
}

// hasVuka is whether the module at root has .vuka files (outside hidden,
// _, build and nested modules' directories, as vuka skips them).
func hasVuka(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "build" || name == "node_modules") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		found = strings.HasSuffix(d.Name(), ".vuka")
		return nil
	})
	return found
}

// vukaCommand is the vuka command: $VUKA, else vuka on the PATH.
func vukaCommand() string {
	if v := os.Getenv("VUKA"); v != "" {
		return v
	}
	return "vuka"
}
