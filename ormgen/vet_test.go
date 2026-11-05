package ormgen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/analysis/passes/unusedwrite"
	"golang.org/x/tools/go/packages"
)

// TestUnusedWrite runs go vet's unusedwrite over testdata/vet, whose
// functions set a row's fields and then call a row method: with the
// methods ormgen writes on each model the row is passed whole, and nothing
// is reported; with only orm.Model's promoted methods (the scanners file
// left out) every line marked // want is.
func TestUnusedWrite(t *testing.T) {
	dir, _ := filepath.Abs("testdata/vet")
	scanners := filepath.Join(dir, OutName)

	files, err := Generate(dir, Config{Patterns: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	for path, src := range files {
		if want, err := os.ReadFile(path); err != nil || !bytes.Equal(src, want) {
			t.Errorf("%s is out of date: go run ../cmd/ormgen ./testdata/vet in orm/ormgen", filepath.Base(path))
		}
	}
	if gen := string(files[scanners]); strings.Contains(gen, "func (x *Custom) Delete(") || !strings.Contains(gen, "func (x *Custom) Save(") {
		t.Errorf("Custom's row methods: want its own Delete left alone and Save written:\n%s", gen)
	}

	want := wantLines(t, filepath.Join(dir, "use.go"))
	before := unusedWrites(t, dir, map[string][]byte{scanners: []byte("package vet\n")})
	t.Logf("without the generated methods:\n%s", strings.Join(before, "\n"))
	if !slices.Equal(lines(before), want) {
		t.Errorf("without the generated methods, unusedwrite reports lines %v, want %v:\n%s", lines(before), want, strings.Join(before, "\n"))
	}
	if after := unusedWrites(t, dir, nil); len(after) > 0 {
		t.Errorf("with the generated methods, unusedwrite reports:\n%s", strings.Join(after, "\n"))
	}
}

// unusedWrites is what unusedwrite reports in the package at dir, as
// use.go:line: message.
func unusedWrites(t *testing.T, dir string, overlay map[string][]byte) []string {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{Dir: dir, Mode: packages.LoadAllSyntax, Overlay: overlay}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("testdata/vet: %d errors", n)
	}
	g, err := checker.Analyze([]*analysis.Analyzer{unusedwrite.Analyzer}, pkgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, act := range g.Roots {
		if act.Err != nil {
			t.Fatal(act.Err)
		}
		for _, d := range act.Diagnostics {
			p := act.Package.Fset.Position(d.Pos)
			out = append(out, fmt.Sprintf("%s:%d: %s", filepath.Base(p.Filename), p.Line, d.Message))
		}
	}
	slices.Sort(out)
	return out
}

// wantLines is the lines of the file at path marked // want.
func wantLines(t *testing.T, path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for i, l := range strings.Split(string(b), "\n") {
		if strings.HasSuffix(l, "// want") {
			out = append(out, i+1)
		}
	}
	return out
}

// lines is the distinct lines of diagnostics in use.go.
func lines(diags []string) []int {
	var out []int
	for _, d := range diags {
		var line int
		if _, err := fmt.Sscanf(strings.TrimPrefix(d, "use.go:"), "%d:", &line); err == nil && strings.HasPrefix(d, "use.go:") && !slices.Contains(out, line) {
			out = append(out, line)
		}
	}
	slices.Sort(out)
	return out
}
