// Command ormgen writes the ORM's row scanners and model registrations
// into the tree, for go:generate (orm generate does the same):
//
//	//go:generate go run github.com/vuka-lang/orm/v2/cmd/ormgen ./...
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/vuka-lang/orm/v2/ormgen"
)

func main() {
	tests := flag.Bool("tests", false, "also the models of _test.go files")
	check := flag.Bool("check", false, "fail when a written file is out of date instead of writing it")
	flag.Parse()
	changed, err := ormgen.Write(".", ormgen.Config{Patterns: flag.Args(), Tests: *tests}, *check)
	for _, p := range changed {
		if *check {
			fmt.Fprintln(os.Stderr, "out of date:", p)
		} else {
			fmt.Println("wrote", p)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check && len(changed) > 0 {
		os.Exit(1)
	}
}
