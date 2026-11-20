package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanNarrow(t *testing.T) {
	const (
		models = "package entities\n\nimport \"github.com/vuka-lang/orm/v2\"\n\ntype User struct {\n\torm.Base @orm.Meta{Table: \"users\"}\n\tID int64 @orm.PK\n}\n"
		goMod  = "package gomodels\n\nimport o \"github.com/vuka-lang/orm/v2\"\n\ntype Pet struct{ o.Model[Pet] }\n"
		query  = "package svc\n\nimport (\n\t\"github.com/vuka-lang/orm/v2\"\n)\n\nfunc N(db *orm.DB) int { return 0 }\n"
		main   = "package main\n\nimport (\n\t_ \"github.com/vuka-lang/orm/v2/postgres\"\n)\n\nfunc main() {}\n"
	)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                    "module example.com/app\n",
		"entities/user.vuka":        models,
		"gomodels/pet.go":           goMod,
		"svc/svc.vuka":              query,
		"pages/_parts/part.vuka":    models,
		"migrations/0001_init.go":   "package migrations\n",
		"main.vuka":                 main,
		"build/entities/user.go":    models,
		"node_modules/x/x.go":       models,
		"nested/go.mod":             "module example.com/nested\n",
		"nested/entities/user.vuka": models,
	})
	plan := planNarrow(root)
	if plan == nil {
		t.Fatal("no plan")
	}
	want := []string{"./entities", "./gomodels", "./migrations/...", "./pages/_parts"}
	if !slices.Equal(plan.patterns, want) {
		t.Errorf("patterns %v, want %v", plan.patterns, want)
	}
	if !slices.Equal(plan.drivers, []string{"github.com/vuka-lang/orm/v2/postgres"}) {
		t.Errorf("drivers %v", plan.drivers)
	}

	t.Setenv("ORM_TOOL", "full")
	if planNarrow(root) != nil {
		t.Error("ORM_TOOL=full still narrows")
	}
}

func TestPlanNarrowNot(t *testing.T) {
	const models = "package %s\n\nimport \"github.com/vuka-lang/orm/v2\"\n\ntype User struct{ orm.Model[User] }\n"
	const driver = "package main\n\nimport _ \"github.com/vuka-lang/orm/v2/sqlite\"\n"
	for name, files := range map[string]map[string]string{
		"no model":  {"main.vuka": driver},
		"no driver": {"entities/user.vuka": "package entities\n\nimport \"github.com/vuka-lang/orm/v2\"\n\ntype U struct{ orm.Model[U] }\n"},
		"a model in main": {
			"main.vuka": "package main\n\nimport (\n\t\"github.com/vuka-lang/orm/v2\"\n\t_ \"github.com/vuka-lang/orm/v2/sqlite\"\n)\n\ntype U struct{ orm.Model[U] }\n",
		},
		"orm used, no model": {
			"svc/svc.vuka": "package svc\n\nimport \"github.com/vuka-lang/orm/v2\"\n\nvar _ = orm.NewDatabases\n",
			"main.vuka":    driver,
		},
	} {
		root := t.TempDir()
		writeTree(t, root, files)
		if plan := planNarrow(root); plan != nil {
			t.Errorf("%s: planned %v", name, plan.patterns)
		}
	}
}

func TestVukaGenOverlayNarrowed(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	for name, script := range map[string]string{
		"narrows": "echo \"$@\" >> " + log + "\necho 'overlay.json: 31 files (2 of 200 packages)'\n",
		"whole":   "echo \"$@\" >> " + log + "\necho 'overlay.json: 538 files'\n",
	} {
		os.Remove(log)
		bin := filepath.Join(dir, name)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VUKA", bin)
		var stderr bytes.Buffer
		ok, narrowed, err := vukaGenOverlay(dir, filepath.Join(dir, "overlay.json"), []string{"./entities", "./migrations/..."}, &stderr)
		calls, _ := os.ReadFile(log)
		if !ok || err != nil || narrowed != (name == "narrows") {
			t.Errorf("%s: ok=%v narrowed=%v err=%v", name, ok, narrowed, err)
		}
		if want := "gen -code-only -overlay-out " + filepath.Join(dir, "overlay.json") + " ./entities ./migrations/...\n"; string(calls) != want {
			t.Errorf("%s: called %q, want %q", name, calls, want)
		}
	}
}

// TestNarrowToolRefusesWhatItCannotJudge: a model the source scan misses
// (hidden from it by the test) is in the migration history. The narrow tool
// doesn't link it, so it sees a table to drop, refuses, and the command builds
// the whole program and answers from that: no operation dropping the table.
func TestNarrowToolRefusesWhatItCannotJudge(t *testing.T) {
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
	root := t.TempDir()
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
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}
	t.Setenv("VUKA", vuka)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("DATABASE_URL", "sqlite:"+filepath.Join(root, "app.db"))

	t.Setenv("ORM_CACHE", t.TempDir())
	t.Setenv("ORM_TOOL", "full")
	var out, errb bytes.Buffer
	if err := runMakeMigrations([]string{"--root", root, "--noinput", "initial"}, nil, &out, &errb); err != nil {
		t.Fatalf("initial: %v\n%s\n%s", err, out.String(), errb.String())
	}
	mig, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
	if err != nil || !strings.Contains(string(mig), `"notes"`) || !strings.Contains(string(mig), `"persons"`) {
		t.Fatalf("0001_initial.go: %v\n%s", err, mig)
	}

	t.Setenv("ORM_TOOL", "")
	scanMiss = func(dir string) bool { return filepath.Base(dir) == "notes" }
	defer func() { scanMiss = nil }()
	if plan := planNarrow(root); plan == nil || slices.Contains(plan.patterns, "./notes") {
		t.Fatalf("the scan should miss the notes package: %v", plan)
	}
	for _, args := range [][]string{{"--dry-run"}, {"--check"}} {
		t.Setenv("ORM_CACHE", t.TempDir())
		out.Reset()
		errb.Reset()
		if err := runMakeMigrations(append([]string{"--root", root, "--noinput"}, args...), nil, &out, &errb); err != nil {
			t.Fatalf("%v: %v\n%s\n%s", args, err, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "No changes") || strings.Contains(out.String(), "DeleteModel") {
			t.Errorf("%v: want no changes, got\n%s\n%s", args, out.String(), errb.String())
		}
		t.Logf("%v: fell back to the whole module: %v", args, strings.Contains(errb.String(), "from the whole module instead"))
	}
}
