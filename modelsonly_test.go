package orm_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// TestModelsNameNoQuerySet: a package holding models, and one using their rows
// without querying, instantiate no Manager[T] or QuerySet[T]. Those are compiled
// anew per model in every package reaching them, what makes a large app's builds
// large; a model reaches them through a method or field naming them.
func TestModelsNameNoQuerySet(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: runs the compiler")
	}
	generics := regexp.MustCompile(`orm/v2\.(\(\*)?(Manager|QuerySet)\[.*`)
	for _, pkg := range []string{"models", "importer"} {
		a := filepath.Join(t.TempDir(), pkg+".a")
		if out, err := exec.Command("go", "build", "-o", a, "./testdata/modelsonly/"+pkg).CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
		out, err := exec.Command("go", "tool", "nm", a).CombinedOutput()
		if err != nil {
			t.Fatalf("go tool nm %s: %v\n%s", pkg, err, out)
		}
		if m := generics.FindAll(out, 5); m != nil {
			t.Errorf("%s instantiates %q", pkg, m)
		}
	}
}
