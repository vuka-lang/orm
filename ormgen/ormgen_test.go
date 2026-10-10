package ormgen

import (
	"os"
	"path/filepath"
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
	for _, name := range []string{OutTestName, OutXTestName} {
		path, _ := filepath.Abs(filepath.Join("..", name))
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(files[path]) != string(want) {
			t.Errorf("%s is out of date: go run ./cmd/ormgen -tests . in orm/", name)
		}
	}
	if len(files) != 2 {
		t.Errorf("generated %d files, want the two test files", len(files))
	}
}
