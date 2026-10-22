// Package ormgen writes the row scanners the ORM uses instead of
// reflection, registers the models embedding orm.Model, and writes each
// model's typed fields. A package that declares a model some orm.For[T]()
// names or that embeds orm.Model[T] gets one orm_scanners_gen.go, its init
// calling orm.Register[T]() for the latter; the models declared in post.go
// get their typed fields (PostFields, PostPath) in post_gen.go. orm
// generate (and the ormgen command, for go:generate) writes them to disk;
// the orm command's migration tool overlays them when they aren't there.
package ormgen

import (
	"bytes"
	"fmt"
	"go/ast"
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

	"github.com/vuka-lang/orm/v2/internal/tags"
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
	// Overlay is source the packages hold that the tree doesn't, by
	// absolute path, as go/packages takes it: what a tool generated, such
	// as the Go of .vuka files.
	Overlay map[string][]byte
	// Skipped hears each model no code is generated for, and why: it is
	// read by reflection at run time instead.
	Skipped func(model, reason string)
}

const ormPath = "github.com/vuka-lang/orm/v2"

// Generate is the scanner files of the models the packages under dir name
// with orm.For or declare embedding orm.Model, by path.
func Generate(dir string, c Config) (map[string][]byte, error) {
	patterns := c.Patterns
	if len(patterns) == 0 {
		var err error
		if patterns, err = callers(dir, c.Tests, c.Overlay); err != nil || len(patterns) == 0 {
			return map[string][]byte{}, err
		}
	}
	cfg := &packages.Config{
		Dir:     dir,
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Tests:   c.Tests,
		Overlay: c.Overlay,
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
			declFile[named] = p.Fset.PositionFor(named.Obj().Pos(), false).Filename
		}
	}
	if err := loadError(pkgs); err != nil {
		return nil, err
	}
	for _, p := range pkgs {
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
	g := &gen{models: models, registered: registered, typed: map[string]bool{}, fset: fset, skipped: c.Skipped}
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
	// Typed fields, in a file beside each model's: generated for every
	// model the scanners were considered for, whether or not it has one.
	fieldsPkg := map[string]*types.Package{}
	fieldsOf := map[string][]*types.Named{}
	for path, ms := range byPath {
		for _, n := range ms {
			name := n.Obj().Name()
			if _, ok := n.Underlying().(*types.Struct); !ok || g.declared(n.Obj().Pkg(), name+"Fields") || g.declared(n.Obj().Pkg(), name+"Path") {
				continue
			}
			g.typed[typeKey(n)] = true
			fp := filepath.Join(filepath.Dir(path), FieldsName(declFile[n]))
			key := fp + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			fieldsPkg[fp] = n.Obj().Pkg()
			fieldsOf[fp] = append(fieldsOf[fp], n)
		}
	}
	for fp, pkg := range fieldsPkg {
		if !generated(fp) {
			return nil, fmt.Errorf("ormgen: %s is in the way: it holds the typed fields of %s, and wasn't written by ormgen", fp, fieldsOf[fp][0].Obj().Name())
		}
		src, err := g.fieldsFile(pkg, fieldsOf[fp])
		if err != nil {
			return nil, err
		}
		if src != nil {
			out[fp] = src
		}
	}
	return out, nil
}

// loadError is the error that stops generation. Type errors don't: code
// naming the typed fields this writes doesn't type-check until it is written,
// and a real error is the build's to report. An import that can't be read
// does: the packages then declare no models, and a migration planned from
// that would drop every one.
func loadError(pkgs []*packages.Package) error {
	for _, p := range pkgs {
		for _, e := range p.Errors {
			// The go command reports the package's compile errors too,
			// as "# path [test]\nfile: error": the type errors again.
			if rest, ok := strings.CutPrefix(e.Msg, "# "+p.PkgPath); ok && e.Kind == packages.ListError && (strings.HasPrefix(rest, "\n") || strings.HasPrefix(rest, " [")) {
				continue
			}
			if e.Kind != packages.TypeError {
				return fmt.Errorf("ormgen: %s", e)
			}
			if unreadableImport(e.Msg) {
				return fmt.Errorf("ormgen: %s: %s\nthe orm tool cannot read this Go toolchain's compiled packages; update github.com/vuka-lang/orm/v2 (and its golang.org/x/tools) to a version that supports it", p.PkgPath, e.Msg)
			}
		}
	}
	return nil
}

func unreadableImport(msg string) bool {
	rest, ok := strings.CutPrefix(msg, "could not import ")
	if !ok {
		return false
	}
	path, _, _ := strings.Cut(rest, " ")
	return path == ormPath || strings.Contains(rest, "internal error in importing") || strings.Contains(rest, "export data version")
}

// callers is the packages under dir whose source says orm.For[, as
// patterns: only they are type-checked, so a save in the dev loop stays
// cheap. Nested modules, vendor, testdata, node_modules and hidden
// directories are left out.
func callers(dir string, tests bool, overlay map[string][]byte) ([]string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	add := func(path string) {
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pattern := "./" + filepath.ToSlash(rel)
		if !slices.Contains(out, pattern) {
			out = append(out, pattern)
		}
	}
	for path, src := range overlay {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") && strings.HasSuffix(path, ".go") &&
			(tests || !strings.HasSuffix(path, "_test.go")) && callsFor(src) {
			add(path)
		}
	}
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
		add(path)
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
func Packages(dir string) ([]string, error) { return callers(dir, false, nil) }

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
		if _, ok := optionElem(c.typ); ok {
			ref = o + "OptValue(" + ref + ")"
		}
		out[i] = ref
	}
	return out, true
}

// gen is one run's view across packages: every model (typed fields'
// relations reach models of other packages), and the file set to tell a
// declaration of the app's from one of a file this wrote.
type gen struct {
	models     map[*types.Named]bool
	registered map[*types.Named]bool // the models embedding orm.Model
	typed      map[string]bool       // the models with typed fields, by typeKey
	fset       *token.FileSet
	skipped    func(model, reason string)
}

// declared is whether pkg declares name outside the files ormgen writes:
// then the typed fields that would take the name are left out.
func (g *gen) declared(pkg *types.Package, name string) bool {
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return false
	}
	if g.fset == nil || !obj.Pos().IsValid() {
		return true
	}
	return !generated(g.fset.PositionFor(obj.Pos(), false).Filename)
}

// file is the scanners and registrations of a package's models, nil when
// none can be generated.
func (g *gen) file(pkg *types.Package, models []*types.Named) ([]byte, error) {
	sort.Slice(models, func(i, j int) bool { return models[i].Obj().Name() < models[j].Obj().Name() })
	imports := map[string]string{ormPath: "orm"}
	o := "orm."
	if pkg.Path() == ormPath {
		imports, o = map[string]string{}, "" // the ORM's own tests
	}
	qual := qualifier(pkg, imports)
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
				g.skipped(pkg.Path()+"."+n.Obj().Name(), "orm.FK or orm.M2M fields")
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
	}
	if inits.Len() == 0 {
		return nil, nil
	}
	return source(pkg, imports, fmt.Appendf(nil, "func init() {\n%s}\n\n%s", inits.String(), body.String()))
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
	if e, ok := optionElem(t); ok {
		return "orm.OptCell[" + types.TypeString(e, qual) + "]", ""
	}
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

// pathTail is the name an import of p declares by convention: its last
// element, or the one before a major-version suffix (orm for
// github.com/vuka-lang/orm/v2).
func pathTail(p string) string {
	i := strings.LastIndexByte(p, '/')
	last := p[i+1:]
	if n, ok := strings.CutPrefix(last, "v"); ok && i > 0 && n != "" && n != "1" && n[0] != '0' && strings.Trim(n, "0123456789") == "" {
		p = p[:i]
		return p[strings.LastIndexByte(p, '/')+1:]
	}
	return last
}

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
// reads by reflection only: orm.FK, orm.M2M.
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
			if o.Pkg().Path() == ormPath && (o.Name() == "FK" || o.Name() == "M2M") {
				return true
			}
		}
		if sub, ok := n.Underlying().(*types.Struct); ok && st.Field(i).Anonymous() && vukaFields(sub, seen) {
			return true
		}
	}
	return false
}
