package orm

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var fuzzDialects = []Dialect{postgres{}, mysql{}, sqlite{}}

// FuzzPlaceholders: raw SQL's arguments are always bound, never written
// into the SQL, and on Postgres the marks are $1…$n in order, each where
// the lexer reads code.
func FuzzPlaceholders(f *testing.F) {
	for _, s := range []string{
		"SELECT ? FROM t WHERE a = ? AND b IN (?, ?)", "SELECT '?', \"a?\", data ?? 'k' FROM t -- a ?\nWHERE a = ?",
		`SELECT E'it\'s ?', ?`, "SELECT /* a /* b ? */ c ? */ ?", "SELECT $x$ ? $x$, $$?$$, a$b$, ?",
		"SELECT 5--1, ? # c ?\n", `SELECT 'C:\', ?`, "SELECT `a?b`, ?", "'unterminated ?", "/* unterminated ?", "$tag$ ?",
	} {
		f.Add(s, uint8(3))
	}
	f.Fuzz(func(t *testing.T, src string, n uint8) {
		args := make([]any, n%8)
		for i := range args {
			args[i] = "ARG" + strconv.Itoa(i) + "'; DROP TABLE x; --"
		}
		for _, d := range fuzzDialects {
			out, got, err := placeholders(d, src, args)
			if err != nil {
				continue
			}
			if len(got) != len(args) || len(args) > 0 && !reflect.DeepEqual(got, args) {
				t.Fatalf("%s: args %v, want %v", d.Name(), got, args)
			}
			if strings.Contains(out, "DROP TABLE x") && !strings.Contains(src, "DROP TABLE x") {
				t.Fatalf("%s: an argument reached the SQL: %q", d.Name(), out)
			}
			if d.Name() != "postgres" {
				continue
			}
			// The marks in the output's code are $1…$n, once each, in order.
			var marks []string
			var b strings.Builder
			lexSQL(out, "postgres", &b, func(i int) int {
				if out[i] == '$' {
					j := i + 1
					for j < len(out) && '0' <= out[j] && out[j] <= '9' {
						j++
					}
					if j > i+1 {
						marks = append(marks, out[i+1:j])
						return j
					}
				}
				return i + 1
			})
			var own int
			lexSQL(src, "postgres", &b, func(i int) int {
				if src[i] == '$' && i+1 < len(src) && '0' <= src[i+1] && src[i+1] <= '9' {
					own++
				}
				return i + 1
			})
			if own > 0 {
				continue // the SQL wrote $n marks of its own
			}
			if len(marks) != len(args) {
				t.Fatalf("%q -> %q: marks %v for %d arguments", src, out, marks, len(args))
			}
			for i, m := range marks {
				if m != strconv.Itoa(i+1) {
					t.Fatalf("%q -> %q: marks %v", src, out, marks)
				}
			}
		}
	})
}

type fuzzAuthor struct {
	ID    int64
	Name  string
	Bio   *string
	Books []fuzzBook
}

type fuzzBook struct {
	ID       int64
	Title    string
	AuthorID int64
	Author   *fuzzAuthor
	Year     int
}

// FuzzLookupKey: a Q key or an order a request names becomes SQL only
// when each of its parts is a field, relation, transform or lookup of
// the model, and its value is always an argument.
func FuzzLookupKey(f *testing.F) {
	for _, s := range []string{"title", "title__icontains", "author__name__startswith", "author__books__title",
		"year__gte", "author__bio__isnull", "title__lower__exact", "author__name\"; DROP", "Title", "author__books__author__name__in",
		"id__in", "x__y__z", "__", "title____", "-author__name"} {
		f.Add(s, "v'; DROP TABLE x; --")
	}
	books, err := modelOf(reflect.TypeFor[fuzzBook](), "", "books")
	if err != nil {
		f.Fatal(err)
	}
	authors, err := modelOf(reflect.TypeFor[fuzzAuthor](), "", "authors")
	if err != nil {
		f.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range []*model{books, authors} {
		for k := range m.byName {
			known[k] = true
		}
		for k := range m.byCol {
			known[k] = true
		}
		for k := range m.rels {
			known[strings.ToLower(k)] = true
		}
		inv, _ := m.inverses()
		for k := range inv {
			known[k] = true
		}
	}
	for k := range lookups {
		known[k] = true
	}
	transforms.Range(func(k, _ any) bool { known[k.(string)] = true; return true })
	valid := func(key string) bool {
		for _, p := range strings.Split(key, "__") {
			if !known[strings.ToLower(p)] {
				return false
			}
		}
		return true
	}
	f.Fuzz(func(t *testing.T, key, value string) {
		value += "SENTINEL'; --"
		for _, d := range fuzzDialects {
			b := newBuilder(d, books)
			s, err := b.lookup(key, value)
			if err == nil {
				if !valid(key) {
					t.Fatalf("%s: %q accepted: %s", d.Name(), key, s)
				}
				if strings.Contains(s, "SENTINEL") {
					t.Fatalf("%s: the value reached the SQL: %s", d.Name(), s)
				}
			}
			b = newBuilder(d, books)
			if s, err := b.ref(key); err == nil && !valid(key) {
				t.Fatalf("%s: order %q accepted: %s", d.Name(), key, s)
			}
		}
	})
}

// FuzzSearch: web search text is only ever arguments, and MySQL's boolean
// query made of it is required or excluded phrases, never its operators.
func FuzzSearch(f *testing.F) {
	for _, s := range []string{`go -java "web server"`, `a or b`, `+x -y "z`, `"`, `-`, `~web <x >y (a) @3 *`, `\"x\"`, "a\u00a0b", "or or -or"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		for _, g := range webSearch(text) {
			if len(g) == 0 {
				t.Fatalf("%q: an empty alternative", text)
			}
			for _, term := range g {
				if term.text == "" || strings.TrimFunc(term.text, unicode.IsSpace) != term.text {
					t.Fatalf("%q: term %q", text, term.text)
				}
			}
		}
		for _, d := range fuzzDialects {
			b := newBuilder(d, &model{Name: "x", Table: "x"})
			s, err := b.search(`"c"`, false, "", text)
			if err != nil {
				t.Fatal(err)
			}
			if text != "" && len(text) > 3 && strings.Contains(s, text) {
				t.Fatalf("%s: the text reached the SQL: %s", d.Name(), s)
			}
			if d.Name() != "mysql" || len(b.args()) == 0 {
				continue
			}
			q := b.args()[0].(string)
			// (+"…" -"…" …) groups: no quote inside a phrase, nothing outside one but signs, spaces and parens.
			inside := false
			for i := 0; i < len(q); i++ {
				c := q[i]
				switch {
				case c == '"':
					inside = !inside
				case inside:
				case c == '(' || c == ')' || c == ' ':
				case (c == '+' || c == '-') && i+1 < len(q) && q[i+1] == '"':
				default:
					t.Fatalf("%q: %q outside a phrase in %q", text, c, q)
				}
			}
			if inside {
				t.Fatalf("%q: unclosed phrase in %q", text, q)
			}
		}
	})
}
