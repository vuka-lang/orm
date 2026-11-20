package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/vuka-lang/orm/v2/internal/progress"
	"github.com/vuka-lang/orm/v2/ormgen"
)

// errNarrow says the tool can't be made from the narrow Go form vuka gen
// wrote: the build goes on with the module's whole Go form.
var errNarrow = errors.New("the narrow Go form is not enough")

// narrowPlan is what the tool is made of, found in the sources before any Go
// is generated: the packages vuka gen makes the Go of (the models', and the
// migrations'), and the ORM's drivers the program imports.
type narrowPlan struct {
	patterns []string
	drivers  []string
}

func (n *narrowPlan) patternsOrNil() []string {
	if n == nil {
		return nil
	}
	return n.patterns
}

const ormImport = `"github.com/vuka-lang/orm/v2"`

var (
	importAlias = regexp.MustCompile(`(?m)^\s*(?:import\s+)?([A-Za-z_.]\w*\s+)?"github.com/vuka-lang/orm/v2"`)
	// declaring is what in a package's source can declare a model: the
	// forms ormgen looks for in its Go (orm.For[T], embedding orm.Model[T],
	// and in Vuka the orm.Base field), under the name the file imports orm by.
	declaring = func(alias string) *regexp.Regexp {
		q := regexp.QuoteMeta(alias)
		if alias == "." {
			return regexp.MustCompile(`\b(?:For|Model|Rec)\s*\[|\bBase\b`)
		}
		return regexp.MustCompile(`\b` + q + `\.(?:For\s*\[|Model\s*\[|Rec\s*\[|Base\b)`)
	}
)

// planNarrow reads the module's sources for the packages that can declare
// models — a file importing orm and using a form that declares one — and
// those of the migrations, and for the drivers the sources import. nil when
// the tool can't be made of them: no such package, no driver imported, a
// declaring package main (nothing can import it), or ORM_TOOL=full.
func planNarrow(root string) *narrowPlan {
	if os.Getenv("ORM_TOOL") == "full" {
		return nil
	}
	dirs, drivers := map[string]bool{}, map[string]bool{}
	mainDecl := false
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root {
				if strings.HasPrefix(name, ".") || name == "build" || name == "node_modules" || name == "vendor" || name == "testdata" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(name, ".vuka") && !strings.HasSuffix(name, ".vuka.html") && !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(src)
		for _, drv := range ormDrivers {
			if strings.Contains(text, `"`+drv+`"`) {
				drivers[drv] = true
			}
		}
		if !strings.Contains(text, ormImport) {
			return nil
		}
		alias := "orm"
		if m := importAlias.FindStringSubmatch(text); m != nil && m[1] != "" {
			alias = strings.TrimSpace(m[1])
		}
		if alias == "_" || !declaring(alias).MatchString(text) {
			return nil
		}
		dirs[filepath.Dir(path)] = true
		if pkgName(text) == "main" {
			mainDecl = true
		}
		return nil
	})
	if len(dirs) == 0 || len(drivers) == 0 || mainDecl {
		return nil
	}
	plan := &narrowPlan{}
	for dir := range dirs {
		if rel, err := filepath.Rel(root, dir); err == nil {
			plan.patterns = append(plan.patterns, "./"+filepath.ToSlash(rel))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "migrations")); err == nil {
		plan.patterns = append(plan.patterns, "./migrations/...")
	}
	for drv := range drivers {
		plan.drivers = append(plan.drivers, drv)
	}
	slices.Sort(plan.patterns)
	slices.Sort(plan.drivers)
	return plan
}

var packageClause = regexp.MustCompile(`(?m)^package\s+(\w+)`)

func pkgName(src string) string {
	if m := packageClause.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return ""
}

// compileNarrow builds the small tool from the narrow overlay vuka gen wrote
// for plan: the models' and migrations' packages and the drivers, as
// compile's minimal tool is. errNarrow when anything is missing from it.
func (p *project) compileNarrow(pr *progress.Printer, root, tmp, vukaOverlay string, plan *narrowPlan) (string, error) {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	flags := devFlags(root, true)
	var c ormgen.Config
	c.BuildFlags = flags
	ov, err := readOverlay(vukaOverlay)
	if err != nil {
		return "", err
	}
	c.Overlay = ov
	declares := map[string]bool{}
	c.Declares = func(path string) { declares[path] = true }

	var gen map[string][]byte
	err = phase(pr, "Generating the ORM code", func() (string, error) {
		var err error
		if gen, err = ormgen.Missing(root, c); err != nil {
			return "", fmt.Errorf("%w: %v", errNarrow, err)
		}
		if len(gen) == 0 {
			return "ORM code up to date", nil
		}
		return "Generated the ORM code", nil
	}, slowBuild)
	if err != nil {
		return "", err
	}
	migrations, err := migrationsImports(root, vukaOverlay)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNarrow, err)
	}
	if len(declares) == 0 {
		return "", fmt.Errorf("%w: no model declared", errNarrow)
	}
	set := map[string]bool{}
	for path := range declares {
		set[path] = true
	}
	for _, path := range append(slices.Clone(migrations), plan.drivers...) {
		set[path] = true
	}
	min := minimalPlan{imports: slices.Sorted(maps.Keys(set))}

	bin := toolBin(tmp)
	err = phase(pr, "Compiling the migration tool", func() (string, error) {
		overlayPath, toolDir, err := writeOverlay(tmp, root, "", gen, vukaOverlay, &min)
		if err != nil {
			return "", err
		}
		if err := p.goBuildTool(pr, bin, flags, overlayPath, root, "./"+filepath.Base(toolDir), true); err != nil {
			return "", fmt.Errorf("%w: %v", errNarrow, err)
		}
		return "Compiled the migration tool", nil
	}, slowBuild)
	return bin, err
}
