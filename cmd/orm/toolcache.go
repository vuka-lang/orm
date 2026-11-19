package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/vuka-lang/orm/v2"
)

// toolFormat changes when how the command builds the tool changes, so no
// tool cached by an older command is run.
const toolFormat = "2"

// toolCache is where the migration tool of a module is kept between
// runs, named by the key of the inputs it was built from (toolKey):
// unchanged inputs run it again without generating or building anything.
// It is in the user's cache directory, or ORM_CACHE's; ORM_CACHE=off
// keeps none.
type toolCache struct{ dir string }

func openToolCache(root string) *toolCache {
	base := os.Getenv("ORM_CACHE")
	if strings.EqualFold(base, "off") {
		return nil
	}
	if base == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(d, "vuka-orm")
	}
	h := sha256.Sum256([]byte(root))
	return &toolCache{filepath.Join(base, "tool", filepath.Base(root)+"-"+hex.EncodeToString(h[:6]))}
}

// tool is the path of the tool built from inputs of key, and whether the
// cache has it.
func (c *toolCache) tool(key string) (string, bool) {
	bin := filepath.Join(c.dir, "tool-"+key[:32])
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	_, err := os.Stat(bin)
	return bin, err == nil
}

// keep moves the tool built at bin into the cache as the tool of key,
// removing the module's other tools (but one built in the last minute,
// which another orm command may be about to run), and returns where it
// is: bin still when it can't be moved, which only costs the next run a
// build.
func (c *toolCache) keep(bin, key string) string {
	dst, _ := c.tool(key)
	if os.MkdirAll(c.dir, 0o755) != nil || os.Rename(bin, dst) != nil {
		return bin
	}
	old, _ := filepath.Glob(filepath.Join(c.dir, "tool-*"))
	for _, p := range old {
		if st, err := os.Stat(p); err == nil && p != dst && time.Since(st.ModTime()) > time.Minute {
			os.Remove(p)
		}
	}
	return dst
}

// goEnvKeys are the go command's settings a build depends on.
var goEnvKeys = []string{"GOVERSION", "GOROOT", "GOOS", "GOARCH", "GOFLAGS", "GOWORK", "GOEXPERIMENT", "CGO_ENABLED", "GOAMD64", "GOARM64", "GOARM", "GO386", "GOMODCACHE", "GOPROXY", "GOPRIVATE", "GONOPROXY", "GOINSECURE", "CC", "CGO_CFLAGS", "CGO_LDFLAGS"}

// toolKey is a hash of what the tool of the module at root is built from:
// the module's files and those of the local modules its go.mod or go.work
// replaces or uses (by path, size and modification time), the go
// command's settings, the orm and vuka commands, and the build's own
// settings. Equal keys build the same tool. The files skip names (SQLite
// databases, which the tool writes) are left out, with their journals.
func toolKey(root string, skip []string, settings ...string) (string, error) {
	h := sha256.New()
	fmt.Fprintln(h, toolFormat)
	for _, s := range settings {
		fmt.Fprintln(h, "setting", s)
	}
	env := command("go", append([]string{"env"}, goEnvKeys...)...)
	env.Dir = root
	out, err := env.Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	h.Write(out)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VUKA") {
			fmt.Fprintln(h, "env", kv)
		}
	}
	vals := strings.Split(string(out), "\n")
	gowork := ""
	if i := indexOf(goEnvKeys, "GOWORK"); i < len(vals) {
		gowork = strings.TrimSpace(vals[i])
	}
	for _, exe := range []string{executable(), lookPath(vukaCommand())} {
		statLine(h, exe)
	}
	dirs := []string{root}
	for _, mod := range []string{filepath.Join(root, "go.mod"), gowork} {
		if mod == "" || mod == "off" {
			continue
		}
		local, err := localModules(mod)
		if err != nil {
			return "", err
		}
		dirs = append(dirs, local...)
		statLine(h, mod)
	}
	sort.Strings(dirs)
	for i, d := range dirs {
		if i > 0 && d == dirs[i-1] {
			continue
		}
		fmt.Fprintln(h, "module", d)
		if err := hashTree(h, d, d == root && hasVuka(root), skip); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return len(list)
}

func executable() string {
	exe, _ := os.Executable()
	return exe
}

func lookPath(name string) string {
	p, _ := exec.LookPath(name)
	return p
}

func statLine(w interface{ Write([]byte) (int, error) }, path string) {
	if path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(w, "missing", path)
		return
	}
	fmt.Fprintln(w, path, st.Size(), st.ModTime().UnixNano(), st.Mode())
}

// hashTree writes each file of the module at dir to h, by path, size,
// modification time and mode, as the go command (and vuka, with vuka)
// read the tree: hidden and _ entries, node_modules, nested modules, and
// vuka's build directory left out, and the files skip names.
func hashTree(h interface{ Write([]byte) (int, error) }, dir string, vuka bool, skip []string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil
		}
		name := d.Name()
		if path != dir && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			if name == "node_modules" || vuka && name == "build" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		for _, s := range skip {
			if path == s || strings.HasPrefix(path, s+"-") {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintln(h, rel, info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	})
}

// localModules is the directories a go.mod or go.work file replaces
// modules with or uses, absolute.
func localModules(file string) ([]string, error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	add := func(p string) {
		p = strings.Trim(p, `"`)
		if !strings.HasPrefix(p, ".") && !filepath.IsAbs(p) {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(file), p)
		}
		out = append(out, filepath.Clean(p))
	}
	block := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		line = strings.TrimSpace(line)
		switch {
		case line == ")":
			block = ""
			continue
		case strings.HasSuffix(line, "("):
			block = strings.TrimSpace(strings.TrimSuffix(line, "("))
			continue
		}
		verb, rest := block, line
		if verb == "" {
			verb, rest, _ = strings.Cut(line, " ")
		}
		switch verb {
		case "replace":
			if _, target, ok := strings.Cut(rest, "=>"); ok {
				if f := strings.Fields(target); len(f) > 0 {
					add(f[0])
				}
			}
		case "use":
			if f := strings.Fields(rest); len(f) > 0 {
				add(f[0])
			}
		}
	}
	return out, sc.Err()
}

// sqliteFiles is the files of the SQLite databases urls name, absolute
// (a relative one from root, where the tool runs).
func sqliteFiles(root string, urls map[string]string) []string {
	var out []string
	for _, u := range urls {
		dialect, dsn, err := orm.ParseURL(u)
		if err != nil || dialect != "sqlite" {
			continue
		}
		dsn, _, _ = strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
		if dsn == "" || strings.HasPrefix(dsn, ":memory:") {
			continue
		}
		if !filepath.IsAbs(dsn) {
			dsn = filepath.Join(root, dsn)
		}
		out = append(out, filepath.Clean(dsn))
	}
	return out
}
