package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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
