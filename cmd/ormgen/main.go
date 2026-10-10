// Command ormgen writes the ORM's row scanners into the tree, for builds
// without the nexus CLI (which overlays them instead):
//
//	//go:generate go run github.com/vuka-lang/orm/cmd/ormgen ./...
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/vuka-lang/orm/ormgen"
)

func main() {
	tests := flag.Bool("tests", false, "also the models of _test.go files")
	check := flag.Bool("check", false, "fail when a written file is out of date instead of writing it")
	flag.Parse()
	files, err := ormgen.Generate(".", ormgen.Config{Patterns: flag.Args(), Tests: *tests})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	stale := false
	for _, p := range paths {
		old, _ := os.ReadFile(p)
		if string(old) == string(files[p]) {
			continue
		}
		if *check {
			fmt.Fprintln(os.Stderr, "out of date:", p)
			stale = true
			continue
		}
		if err := os.WriteFile(p, files[p], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", p)
	}
	if stale {
		os.Exit(1)
	}
}
