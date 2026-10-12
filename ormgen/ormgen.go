// Package ormgen writes the row scanners the ORM uses instead of
// reflection, and registers the models embedding orm.Model: one
// orm_scanners_gen.go per package that declares a model some orm.For[T]()
// names or that embeds orm.Model[T], its init calling orm.Register[T]()
// for the latter. orm generate (and the ormgen command, for go:generate)
// writes it to disk; the orm command's migration tool overlays it when it
// isn't there.
package ormgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/vuka-lang/orm/internal/tags"
)

// The files ormgen writes in a model's package: OutTestName for models
// declared in _test.go files.
const (
	OutName      = "orm_scanners_gen.go"
	OutTestName  = "orm_scanners_gen_test.go"
	OutXTestName = "orm_scanners_x_gen_test.go" // a package_test package
)

// Config is what Generate loads: Patterns (default ./...), and Tests to
// take the models of test files too.
type Config struct {
	Patterns []string
	Tests    bool
	// Skipped hears each model no code is generated for, and why: it is
	// read by reflection at run time instead.
	Skipped func(model, reason string)
}

const ormPath = "github.com/vuka-lang/orm"

// Generate is the scanner files of the models the packages under dir name
// with orm.For or declare embedding orm.Model, by path.
func Generate(dir string, c Config) (map[string][]byte, error) {
	patterns := c.Patterns
	if len(patterns) == 0 {
		var err error
		if patterns, err = callers(dir, c.Tests); err != nil || len(patterns) == 0 {
			return map[string][]byte{}, err
		}
	}
	cfg := &packages.Config{
		Dir:   dir,
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Tests: c.Tests,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	models := map[*types.Named]bool{}
	registered := map[*types.Named]bool{}
	var order []*types.Named
	declFile := map[*types.Named]string{}
	add := func(p *packages.Package, named *types.Named) {
		if !models[named] {
			models[named] = true
			order = append(order, named)
			declFile[named] = p.Fset.Position(named.Obj().Pos()).Filename
		}
	}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			// Type errors don't stop generation: code naming the field
			// sets this writes doesn't type-check until it is written,
			// and a real error is the build's to report.
			if e.Kind != packages.TypeError {
				return nil, fmt.Errorf("ormgen: %s", e)
			}
		}
		if p.TypesInfo == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				id := forIdent(n)
				if id == nil {
					return true
				}
				inst, ok := p.TypesInfo.Instances[id]
				if !ok || inst.TypeArgs.Len() != 1 {
					return true
				}
				obj := p.TypesInfo.Uses[id]
				if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != ormPath || obj.Name() != "For" {
					return true
				}
				if named, ok := inst.TypeArgs.At(0).(*types.Named); ok {
					add(p, named)
				}
				return true
			})
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
				if named, ok := tn.Type().(*types.Named); ok && EmbedsModel(named) {
					add(p, named)
					registered[named] = true
				}
			}
		}
	}
	// A package's models declared in test files go in a test file of
	// their own; with Tests, a package loads twice (with and without its
	// tests), so its models are keyed by path, not by *types.Package.
	type target struct {
		pkg  *types.Package
		path string
	}
	byTarget := map[string]*target{}
	byPath := map[string][]*types.Named{}
	seen := map[string]bool{}
	dirs := map[string]string{}
	for _, p := range pkgs {
		if len(p.GoFiles) > 0 {
			dirs[p.Types.Path()] = filepath.Dir(p.GoFiles[0])
		}
	}
	// Models declared in packages that don't call For themselves: their
	// directories, without type-checking them again.
	var more []string
	for _, n := range order {
		if path := n.Obj().Pkg().Path(); dirs[path] == "" && !slices.Contains(more, path) {
			more = append(more, path)
		}
	}
	if len(more) > 0 {
		extra, err := packages.Load(&packages.Config{Dir: dir, Mode: packages.NeedName | packages.NeedFiles | packages.NeedModule}, more...)
		if err != nil {
			return nil, err
		}
		for _, p := range extra {
			if len(p.GoFiles) > 0 && p.Module != nil && p.Module.Main {
				dirs[p.PkgPath] = filepath.Dir(p.GoFiles[0])
			}
		}
	}
	for _, n := range order {
		pkg := n.Obj().Pkg()
		dir, local := dirs[pkg.Path()]
		if reason := ""; !local || n.TypeParams().Len() > 0 || n.Obj().Parent() != pkg.Scope() {
			switch {
			case !local:
				reason = "declared outside the module"
			case n.TypeParams().Len() > 0:
				reason = "generic"
			default:
				reason = "declared inside a function"
			}
			if c.Skipped != nil {
				c.Skipped(pkg.Path()+"."+n.Obj().Name(), reason)
			}
			continue
		}
		name := OutName
		switch {
		case strings.HasSuffix(pkg.Name(), "_test"):
			name = OutXTestName
		case strings.HasSuffix(declFile[n], "_test.go"):
			name = OutTestName
		}
		path := filepath.Join(dir, name)
		key := path + "\x00" + n.Obj().Name()
		if seen[key] {
			continue
		}
		seen[key] = true
		if byTarget[path] == nil {
			byTarget[path] = &target{pkg: pkg, path: path}
		}
		byPath[path] = append(byPath[path], n)
	}
	var fset *token.FileSet
	if len(pkgs) > 0 {
		fset = pkgs[0].Fset
	}
	g := &gen{models: models, registered: registered, fset: fset, skipped: c.Skipped}
	out := map[string][]byte{}
	for path, t := range byTarget {
		src, err := g.file(t.pkg, byPath[path])
		if err != nil {
			return nil, err
		}
		if src != nil {
			out[path] = src
		}
	}
	return out, nil
}

// callers is the packages under dir whose source says orm.For[, as
// patterns: only they are type-checked, so a save in the dev loop stays
// cheap. Nested modules, vendor, testdata, node_modules and hidden
// directories are left out.
func callers(dir string, tests bool) ([]string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || (!tests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || !callsFor(src) {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pattern := "./" + filepath.ToSlash(rel)
		if !slices.Contains(out, pattern) {
			out = append(out, pattern)
		}
		return nil
	})
	return out, err
}

// callsFor reports whether a file may call orm.For or embed orm.Model: it
// imports the orm package (under any name) and names a For[…] or
// Model[…]. Type-checking decides.
func callsFor(src []byte) bool {
	return bytes.Contains(src, []byte(`"`+ormPath+`"`)) && (bytes.Contains(src, []byte("For[")) || bytes.Contains(src, []byte("Model[")) || bytes.Contains(src, []byte("Base[")))
}

// EmbedsModel is whether n is a model of its own orm.Model: a struct
// embedding orm.Model[n], directly or through embedded structs (by
// pointer or twice too: Register then fails boot saying why). A struct
// holding another model's orm.Model (one embedding a model to add
// fields) isn't one.
func EmbedsModel(n *types.Named) bool {
	if n.TypeParams().Len() > 0 {
		return false
	}
	var walk func(st *types.Struct, seen map[*types.Struct]bool) bool
	walk = func(st *types.Struct, seen map[*types.Struct]bool) bool {
		if seen[st] {
			return false
		}
		seen[st] = true
		for i := range st.NumFields() {
			f := st.Field(i)
			if !f.Anonymous() {
				continue
			}
			t := f.Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			e, ok := t.(*types.Named)
			if !ok {
				continue
			}
			if o := e.Origin().Obj(); o.Pkg() != nil && o.Pkg().Path() == ormPath && (o.Name() == "Model" || o.Name() == "Base") {
				if e.TypeArgs().Len() == 1 && types.Identical(e.TypeArgs().At(0), n) {
					return true
				}
				continue
			}
			if sub, ok := e.Underlying().(*types.Struct); ok && walk(sub, seen) {
				return true
			}
		}
		return false
	}
	st, ok := n.Underlying().(*types.Struct)
	return ok && walk(st, map[*types.Struct]bool{})
}

// Packages is the patterns (./dir) of the packages under dir that may
// declare models with orm.For or orm.Model.
func Packages(dir string) ([]string, error) { return callers(dir, false) }

// forIdent is the For of a call orm.For[T](…), else nil.
func forIdent(n ast.Node) *ast.Ident {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil
	}
	ix, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return nil
	}
	switch f := ix.X.(type) {
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.Ident:
		return f
	}
	return nil
}

// column is a field the generated scanner reads.
type column struct {
	name     string // column
	path     []string
	ptrs     []string // the pointer embeds on the way, as selectors to allocate
	typ      types.Type
	depth    int
	goName   string
	computed bool
}

// columns is a model's columns as the ORM's runtime sees them, or ok
// false when the runtime would refuse the model.
func columns(st *types.Struct) ([]column, bool) {
	var out []column
	if !collect(st, nil, nil, "", &out) {
		return nil, false
	}
	return out, len(out) > 0
}

func collect(st *types.Struct, path, ptrs []string, prefix string, out *[]column) bool {
	for i := range st.NumFields() {
		f := st.Field(i)
		tag := tags.Parse(f.Name(), reflect.StructTag(st.Tag(i)), isTime(f.Type()))
		if tag.Skip {
			continue
		}
		ft := f.Type()
		base := ft
		isPtr := false
		if p, ok := ft.(*types.Pointer); ok {
			base, isPtr = p.Elem(), true
		}
		sub, isStruct := base.Underlying().(*types.Struct)
		here := append(append([]string(nil), path...), f.Name())
		if f.Anonymous() && isStruct && !isValue(ft) {
			if isPtr && !f.Exported() {
				return false
			}
			nextPtrs := ptrs
			if isPtr {
				nextPtrs = append(append([]string(nil), ptrs...), strings.Join(here, "."))
			}
			if !collect(sub, here, nextPtrs, prefix, out) {
				return false
			}
			continue
		}
		if !f.Exported() {
			continue
		}
		if tag.Embedded && isStruct {
			nextPtrs := ptrs
			if isPtr {
				nextPtrs = append(append([]string(nil), ptrs...), strings.Join(here, "."))
			}
			if !collect(sub, here, nextPtrs, prefix+tag.Prefix, out) {
				return false
			}
			continue
		}
		if !isValue(ft) {
			continue
		}
		col := tag.Column
		if col == "" {
			col = tags.Snake(f.Name())
		}
		c := column{name: prefix + col, path: here, ptrs: ptrs, typ: ft, depth: len(here), goName: f.Name(), computed: tag.Computed}
		if c.computed {
			continue
		}
		// An outer field shadows an embedded one of the same name, as in
		// the runtime.
		key := strings.ToLower(f.Name())
		replaced := false
		for j, old := range *out {
			if strings.ToLower(old.goName) == key {
				if old.depth <= c.depth {
					replaced = true
					break
				}
				*out = append((*out)[:j], (*out)[j+1:]...)
				break
			}
		}
		if !replaced {
			*out = append(*out, c)
		}
	}
	return true
}

func isTime(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "time" && n.Obj().Name() == "Time"
}

// hasMethod is whether t's method set has name.
func hasMethod(t types.Type, name string) bool {
	return types.NewMethodSet(t).Lookup(nil, name) != nil
}

func isScanner(t types.Type) bool {
	return hasMethod(t, "Scan") || hasMethod(types.NewPointer(t), "Scan")
}

// isValue mirrors the runtime's: basic kinds, times, bytes, and types that
// scan or value themselves.
func isValue(t types.Type) bool {
	if isScanner(t) || hasMethod(t, "Value") || hasMethod(t, "DBValue") || hasMethod(types.NewPointer(t), "DBValue") || hasMethod(types.NewPointer(t), "FromDB") {
		return true
	}
	if p, ok := t.(*types.Pointer); ok {
		return isValue(p.Elem())
	}
	if isTime(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&(types.IsBoolean|types.IsInteger|types.IsFloat|types.IsString) != 0
	case *types.Slice:
		b, ok := u.Elem().Underlying().(*types.Basic)
		return ok && b.Kind() == types.Byte
	}
	return false
}

// writerValues is the INSERT values of a model's columns, as the
// runtime's reflection reads them; ok false when a column sits behind a
// pointer embed (nil reads as zero there) or is a pointer to a pointer.
func writerValues(cols []column, o string) ([]string, bool) {
	out := make([]string, len(cols))
	for i, c := range cols {
		if len(c.ptrs) > 0 {
			return nil, false
		}
		ref := "r." + strings.Join(c.path, ".")
		if p, ok := c.typ.(*types.Pointer); ok {
			if _, pp := p.Elem().(*types.Pointer); pp {
				return nil, false
			}
			ref = o + "PtrValue(" + ref + ")"
		}
		out[i] = ref
	}
	return out, true
}

// gen is one run's view across packages: every model (a field set's
// relations reach models of other packages), and the file set to tell a
// declaration of the app's from one of a file this wrote.
type gen struct {
	models     map[*types.Named]bool
	registered map[*types.Named]bool // the models embedding orm.Model
	fset       *token.FileSet
	skipped    func(model, reason string)
}

// declared is whether pkg declares name outside the files ormgen writes:
// then the field set that would take the name is left out.
func (g *gen) declared(pkg *types.Package, name string) bool {
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return false
	}
	if g.fset == nil || !obj.Pos().IsValid() {
		return true
	}
	switch filepath.Base(g.fset.Position(obj.Pos()).Filename) {
	case OutName, OutTestName, OutXTestName:
		return false
	}
	return true
}

// file is the scanners and field sets of a package's models, nil when
// none can be generated.
func (g *gen) file(pkg *types.Package, models []*types.Named) ([]byte, error) {
	sort.Slice(models, func(i, j int) bool { return models[i].Obj().Name() < models[j].Obj().Name() })
	imports := map[string]string{ormPath: "orm"}
	o := "orm."
	if pkg.Path() == ormPath {
		imports, o = map[string]string{}, "" // the ORM's own tests
	}
	qual := func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		if name, ok := imports[p.Path()]; ok {
			return name
		}
		name := p.Name()
		for taken := true; taken; {
			taken = false
			for _, n := range imports {
				if n == name {
					name += "_"
					taken = true
				}
			}
		}
		imports[p.Path()] = name
		return name
	}
	var body bytes.Buffer
	var inits bytes.Buffer
	for _, n := range models {
		if g.registered[n] {
			fmt.Fprintf(&inits, "\t%sRegister[%s]()\n", o, n.Obj().Name())
		}
		st, ok := n.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		if vukaFields(st, map[*types.Struct]bool{}) {
			if g.skipped != nil {
				g.skipped(pkg.Path()+"."+n.Obj().Name(), "orm.FK, orm.M2M or Option fields")
			}
			continue
		}
		cols, ok := columns(st)
		if !ok {
			if g.skipped != nil {
				g.skipped(pkg.Path()+"."+n.Obj().Name(), "an unexported pointer embed, or no columns")
			}
			continue
		}
		name := n.Obj().Name()
		scan := "ormScan" + name
		var names []string
		for _, c := range cols {
			names = append(names, strconv.Quote(c.name))
		}
		fmt.Fprintf(&inits, "\t%sRegisterScanner[%s]([]string{%s}, func() %sRowScanner[%s] { return new(%s) })\n",
			o, name, strings.Join(names, ", "), o, name, scan)

		fmt.Fprintf(&body, "type %s struct {\n", scan)
		var dest, bind []string
		allocated := map[string]bool{}
		for i, c := range cols {
			for _, p := range c.ptrs {
				if !allocated[p] {
					allocated[p] = true
					t := embedType(st, strings.Split(p, "."))
					bind = append(bind, fmt.Sprintf("if r.%s == nil {\n\t\tr.%s = new(%s)\n\t}", p, p, types.TypeString(t, qual)))
				}
			}
			ref := "&r." + strings.Join(c.path, ".")
			cellType, ptr := cellFor(c.typ, qual)
			cellType = strings.Replace(cellType, "orm.", o, 1)
			fmt.Fprintf(&body, "\tc%d %s\n", i, cellType)
			dest = append(dest, fmt.Sprintf("&s.c%d", i))
			if ptr != "" {
				ref = "(" + ptr + ")(" + ref + ")"
			}
			bind = append(bind, fmt.Sprintf("s.c%d.P = %s", i, ref))
		}
		fmt.Fprintf(&body, "\tdest []any\n}\n\n")
		fmt.Fprintf(&body, "func (s *%s) Dest() []any {\n\tif s.dest == nil {\n\t\ts.dest = []any{%s}\n\t}\n\treturn s.dest\n}\n\n", scan, strings.Join(dest, ", "))
		fmt.Fprintf(&body, "func (s *%s) Bind(r *%s) {\n\t%s\n}\n\n", scan, name, strings.Join(bind, "\n\t"))
		if vals, ok := writerValues(cols, o); ok {
			fmt.Fprintf(&body, "func (s *%s) Values(r *%s, dst []any) []any {\n\treturn append(dst[:0], %s)\n}\n\n", scan, name, strings.Join(vals, ", "))
		}
		g.fieldSet(&body, pkg, n, st, cols, o, qual)
	}
	if inits.Len() == 0 {
		return nil, nil
	}
	var src bytes.Buffer
	fmt.Fprintf(&src, "// Code generated by ormgen. DO NOT EDIT.\n\npackage %s\n\n", pkg.Name())
	paths := make([]string, 0, len(imports))
	for p := range imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > 0 {
		src.WriteString("import (\n")
		for _, p := range paths {
			if imports[p] == pathTail(p) {
				fmt.Fprintf(&src, "\t%q\n", p)
			} else {
				fmt.Fprintf(&src, "\t%s %q\n", imports[p], p)
			}
		}
		src.WriteString(")\n\n")
	}
	fmt.Fprintf(&src, "func init() {\n%s}\n\n%s", inits.String(), body.String())
	out, err := format.Source(src.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ormgen: %s: %w\n%s", pkg.Path(), err, src.String())
	}
	return out, nil
}

// fieldSet writes a model's typed lookups: <Model>FieldSet, a field per
// column and a method per relation to a model with a set of its own, and
// <Model>Fields, the set from the model itself.
func (g *gen) fieldSet(w *bytes.Buffer, pkg *types.Package, n *types.Named, st *types.Struct, cols []column, o string, qual types.Qualifier) {
	name := n.Obj().Name()
	set, vr := name+"FieldSet", name+"Fields"
	if g.declared(pkg, set) || g.declared(pkg, vr) {
		return
	}
	names := map[string]int{}
	for _, c := range cols {
		names[c.goName]++
	}
	type entry struct{ field, typ, ctor, path string }
	var entries []entry
	for _, c := range cols {
		field := c.goName
		if names[field] > 1 || field == "p" {
			field = camel(c.name)
		}
		t := c.typ
		if p, ok := t.(*types.Pointer); ok && !isScanner(t) {
			t = p.Elem()
		}
		v := types.TypeString(t, qual)
		typ, ctor := o+"Field["+v+"]", o+"FieldAt["+v+"]"
		if b, ok := t.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
			typ, ctor = o+"TextField["+v+"]", o+"TextFieldAt["+v+"]"
		}
		entries = append(entries, entry{field, typ, ctor, c.name})
	}
	fmt.Fprintf(w, "// %s is %s's fields as typed lookups.\nvar %s = %s{}.Under(\"\")\n\n", vr, name, vr, set)
	fmt.Fprintf(w, "// %s is %s's fields as typed lookups: %s from the model itself, Under from a model related to it.\n", set, name, vr)
	fmt.Fprintf(w, "type %s struct {\n\tp string\n", set)
	for _, e := range entries {
		fmt.Fprintf(w, "\t%s %s\n", e.field, e.typ)
	}
	fmt.Fprintf(w, "}\n\n")
	fmt.Fprintf(w, "// Under is the set through a relation path, such as \"author__\".\nfunc (s %s) Under(prefix string) %s {\n\treturn %s{\n\t\tp: prefix,\n", set, set, set)
	for _, e := range entries {
		fmt.Fprintf(w, "\t\t%s: %s(prefix + %q),\n", e.field, e.ctor, e.path)
	}
	fmt.Fprintf(w, "\t}\n}\n\n")
	for _, r := range g.relations(st) {
		target := r.target.Obj()
		ref := target.Name() + "FieldSet"
		if target.Pkg() != pkg {
			ref = qual(target.Pkg()) + "." + ref
		}
		fmt.Fprintf(w, "// %s is the fields of %s's %s, for lookups across the relation.\nfunc (s %s) %s() %s {\n\treturn %s{}.Under(s.p + %q)\n}\n\n",
			r.goName, name, r.goName, set, r.goName, ref, ref, r.path+"__")
	}
}

// fieldRel is a relation field a field set follows.
type fieldRel struct {
	goName, path string
	target       *types.Named
}

// relations is a model's relations to models with a field set: fields of
// a model type, a pointer to one, or a slice of either.
func (g *gen) relations(st *types.Struct) []fieldRel {
	var out []fieldRel
	for i := range st.NumFields() {
		f := st.Field(i)
		if !f.Exported() || f.Anonymous() || isValue(f.Type()) {
			continue
		}
		tag := tags.Parse(f.Name(), reflect.StructTag(st.Tag(i)), false)
		if tag.Skip || tag.Embedded {
			continue
		}
		t := f.Type()
		if s, ok := t.(*types.Slice); ok {
			t = s.Elem()
		}
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := t.(*types.Named)
		if !ok || !g.models[n] || n.TypeParams().Len() > 0 {
			continue
		}
		if g.declared(n.Obj().Pkg(), n.Obj().Name()+"FieldSet") || g.declared(n.Obj().Pkg(), n.Obj().Name()+"Fields") {
			continue
		}
		out = append(out, fieldRel{f.Name(), tags.Snake(f.Name()), n})
	}
	return out
}

// camel is a column as a Go name: home_city → HomeCity.
func camel(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// embedType is the struct a pointer embed at path points to.
func embedType(st *types.Struct, path []string) types.Type {
	var t types.Type = st
	for _, name := range path {
		s := t.Underlying().(*types.Struct)
		for i := range s.NumFields() {
			if s.Field(i).Name() == name {
				t = s.Field(i).Type()
				break
			}
		}
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
	}
	return t
}

// cellFor is the cell a field of type t scans through, and the pointer
// type its address is converted to when the cell reads its underlying
// type (a named string reads as a string).
func cellFor(t types.Type, qual types.Qualifier) (cell, conv string) {
	if p, ok := t.(*types.Pointer); ok && !isScanner(t) {
		return "orm.PtrCell[" + types.TypeString(p.Elem(), qual) + "]", ""
	}
	if _, named := t.(*types.Named); named && !isTime(t) && !isScanner(t) && !hasMethod(t, "Value") {
		if b, ok := t.Underlying().(*types.Basic); ok {
			u := types.TypeString(b, qual)
			return "orm.Cell[" + u + "]", "*" + u
		}
	}
	return "orm.Cell[" + types.TypeString(t, qual) + "]", ""
}

func pathTail(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

// Write writes the files Generate makes under dir that differ from what is
// on disk, and returns their paths; with check it writes nothing and
// returns the paths that are out of date. For tools: orm generate and the
// ormgen command are this.
func Write(dir string, c Config, check bool) ([]string, error) {
	files, err := Generate(dir, c)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changed []string
	for _, p := range paths {
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, files[p]) {
			continue
		}
		changed = append(changed, p)
		if check {
			continue
		}
		if err := os.WriteFile(p, files[p], 0o644); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// Missing is the files Generate makes under dir that the tree lacks or
// holds out of date: what a tool overlays onto a build so the models are
// registered and scanned without the files being written.
func Missing(dir string, c Config) (map[string][]byte, error) {
	files, err := Generate(dir, c)
	if err != nil {
		return nil, err
	}
	for p, content := range files {
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, content) {
			delete(files, p)
		}
	}
	return files, nil
}

// vukaFields is whether st (or a struct it embeds) has fields the runtime
// reads by reflection only: orm.FK, orm.M2M, vuka.Option.
func vukaFields(st *types.Struct, seen map[*types.Struct]bool) bool {
	if seen[st] {
		return false
	}
	seen[st] = true
	for i := range st.NumFields() {
		t := st.Field(i).Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := t.(*types.Named)
		if !ok {
			continue
		}
		if o := n.Origin().Obj(); o.Pkg() != nil {
			switch {
			case o.Pkg().Path() == ormPath && (o.Name() == "FK" || o.Name() == "M2M"),
				o.Pkg().Path() == "github.com/vuka-lang/vuka" && o.Name() == "Option":
				return true
			}
		}
		if sub, ok := n.Underlying().(*types.Struct); ok && st.Field(i).Anonymous() && vukaFields(sub, seen) {
			return true
		}
	}
	return false
}
