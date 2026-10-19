package ormgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDrift generates the ORM's own test models and compares with the
// files committed beside them: the generator works, and they are current
// (go run ./cmd/ormgen -tests . in orm/ refreshes them).
func TestDrift(t *testing.T) {
	files, err := Generate("..", Config{Tests: true})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("..")
	for _, name := range []string{OutTestName, OutXTestName, "fields_gen_test.go"} {
		if files[filepath.Join(root, name)] == nil {
			t.Errorf("%s isn't generated", name)
		}
	}
	for path, src := range files {
		if filepath.Dir(path) != root {
			continue
		}
		if !strings.HasSuffix(path, "_test.go") {
			t.Errorf("generated %s in orm/, want test files only", path)
		}
		if want, err := os.ReadFile(path); err != nil || !bytes.Equal(src, want) {
			t.Errorf("%s is out of date: go run ./cmd/ormgen -tests . in orm/", filepath.Base(path))
		}
	}
	on, _ := filepath.Glob(filepath.Join(root, "*_gen_test.go"))
	for _, path := range on {
		if b, _ := os.ReadFile(path); bytes.HasPrefix(b, []byte(header)) && files[path] == nil {
			t.Errorf("%s is no longer generated: delete it", filepath.Base(path))
		}
	}
}
