package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestToolKey: the key of a module's tool changes with any file the build
// may read — of the module, or of a local module it replaces — and not
// with what the go command and vuka leave out.
func TestToolKey(t *testing.T) {
	root, lib := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.com/lib\n")
	writeFile(t, filepath.Join(lib, "lib.go"), "package lib\n")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\nrequire example.com/lib v0.0.0\n\nreplace (\n\texample.com/lib => "+lib+" // local\n)\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	t.Setenv("GOWORK", "off")
	key := func() string {
		t.Helper()
		k, err := toolKey(root, []string{filepath.Join(root, "app.db")}, "main ")
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	base := key()
	if key() != base {
		t.Fatal("the key of unchanged inputs changed")
	}
	if k, _ := toolKey(root, nil, "main cmd/app"); k == base {
		t.Fatal("--main left the key as it was")
	}
	later := time.Now().Add(time.Second)
	for _, path := range []string{
		filepath.Join(root, ".git", "index"),
		filepath.Join(root, "node_modules", "x.js"),
		filepath.Join(root, "_tools", "x.go"),
		filepath.Join(root, "nested", "go.mod"),
		filepath.Join(root, ".env"),
		filepath.Join(root, "app.db"),
		filepath.Join(root, "app.db-wal"),
	} {
		writeFile(t, path, "x")
		if key() != base {
			t.Fatalf("%s changed the key", path)
		}
	}
	for _, change := range []func(){
		func() { writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() { println() }\n") },
		func() { os.Chtimes(filepath.Join(root, "main.go"), later, later) },
		func() { writeFile(t, filepath.Join(root, "static", "logo.svg"), "<svg/>") },
		func() { os.Remove(filepath.Join(root, "static", "logo.svg")) },
		func() { writeFile(t, filepath.Join(lib, "lib.go"), "package lib\n\nvar X int\n") },
	} {
		before := key()
		change()
		if key() == before {
			t.Fatal("a change of an input left the key as it was")
		}
	}
}

// TestToolCache keeps one tool per module, named by its key.
func TestToolCache(t *testing.T) {
	t.Setenv("ORM_CACHE", t.TempDir())
	c := openToolCache("/src/app")
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, ok := c.tool(key); ok {
		t.Fatal("an empty cache has a tool")
	}
	for _, k := range []string{"f" + key[1:], key} {
		bin := filepath.Join(t.TempDir(), "tool")
		writeFile(t, bin, k)
		old := time.Now().Add(-2 * time.Minute)
		os.Chtimes(bin, old, old)
		if got := c.keep(bin, k); filepath.Dir(got) != c.dir {
			t.Fatalf("kept at %s", got)
		}
	}
	if p, ok := c.tool(key); !ok || filepath.Base(p) != "tool-"+key[:32] {
		t.Fatalf("tool %s, %v", p, ok)
	}
	if left, _ := filepath.Glob(filepath.Join(c.dir, "tool-*")); len(left) != 1 {
		t.Fatalf("older tools kept: %q", left)
	}
	t.Setenv("ORM_CACHE", "off")
	if openToolCache("/src/app") != nil {
		t.Fatal("ORM_CACHE=off keeps a cache")
	}
}
