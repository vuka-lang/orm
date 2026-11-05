package migration

import (
	"strings"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// Preflight is a query over the rows a migration's operation needs to be
// right before it runs: no two rows alike in a unique index's (or unique
// constraint's) keys, none breaking a check. orm migrate runs them before
// the migration's first statement, so it fails with the rows' story
// rather than half applied.
type Preflight struct {
	Op    string // the operation, as Describe says it
	Name  string // the index's or constraint's
	Table string
	// Keys is a unique index's key expressions; none for a check.
	Keys []string
	// SQL is the query: for unique keys each duplicated value's keys and
	// row count, most rows first; for a check the count of rows breaking
	// it.
	SQL string
}

// Check is whether p counts a check's rows rather than duplicated keys.
func (p Preflight) Check() bool { return len(p.Keys) == 0 }

// Preflights is the queries checking the rows mg's operations need, on
// dialect, before mg runs. An operation
// whose table the migration created, changed the columns of, or that
// follows Go or SQL of the migration's own (which may change the rows: a
// data migration removing duplicates) gets none: the rows it sees aren't
// there yet. So does an index whose statement isn't one it can read.
func (mg Migration) Preflights(dialect string) []Preflight {
	d := schema.For(dialect)
	changed := map[string]bool{}
	var out []Preflight
	for _, op := range mg.Operations {
		switch o := op.(type) {
		case RunSQL, RunGo, CreateFunction:
			return out
		case CreateModel:
			changed[o.Table] = true
		case RenameModel:
			changed[o.Old], changed[o.New] = true, true
		case AddField:
			changed[o.Table] = true
		case AlterField:
			changed[o.Table] = true
		case RemoveField:
			changed[o.Table] = true
		case RenameField:
			changed[o.Table] = true
		case AddIndex:
			if changed[o.Table] {
				continue
			}
			if keys, where, ok := uniqueIndex(d, o.Index.SQL.For(d.Name)); ok {
				out = append(out, uniquePreflight(d, op, o.Index.Name, o.Table, keys, where))
			}
		case AddConstraint:
			k := o.Constraint
			switch {
			case changed[o.Table] || !k.On(d.Name):
			case !k.IsCheck():
				keys := make([]string, len(k.Columns))
				for i, c := range k.Columns {
					keys[i] = d.Quote(c)
				}
				out = append(out, uniquePreflight(d, op, k.Name, o.Table, keys, ""))
			case !o.NotValid || d.Name != "postgres":
				cond := k.Check
				if cond == "" {
					cond = k.Declared.For(d.Name)
				}
				out = append(out, Preflight{Op: Describe(op), Name: k.Name, Table: o.Table,
					SQL: "SELECT COUNT(*) FROM " + d.Quote(o.Table) + " WHERE NOT (" + cond + ")"})
			}
		}
	}
	return out
}

// uniquePreflight is the query finding the values of keys more than one
// row of table holds: rows with a NULL key left out, as the database
// leaves them out of a unique index, and those where doesn't hold.
func uniquePreflight(d schema.Dialect, op Operation, name, table string, keys []string, where string) Preflight {
	list := strings.Join(keys, ", ")
	var conds []string
	for _, k := range keys {
		conds = append(conds, "("+k+") IS NOT NULL")
	}
	if where != "" {
		conds = append(conds, "("+where+")")
	}
	return Preflight{Op: Describe(op), Name: name, Table: table, Keys: keys,
		SQL: "SELECT " + list + ", COUNT(*) FROM " + d.Quote(table) + " WHERE " + strings.Join(conds, " AND ") +
			" GROUP BY " + list + " HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC"}
}

// uniqueIndex reads a CREATE UNIQUE INDEX statement: its keys as
// expressions (their order, operator class and NULLS placement dropped,
// a MySQL key prefix as LEFT) and its WHERE. ok is false for an index
// that isn't unique, ignores NULLs distinctness (NULLS NOT DISTINCT), or
// a statement it can't read.
func uniqueIndex(d schema.Dialect, stmt string) (keys []string, where string, ok bool) {
	src := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(stmt), ";"))
	toks := tokens(src)
	i := 0
	word := func(ws ...string) bool {
		for j, w := range ws {
			if i+j >= len(toks) || !strings.EqualFold(toks[i+j].text, w) {
				return false
			}
		}
		i += len(ws)
		return true
	}
	if !word("CREATE", "UNIQUE", "INDEX") {
		return nil, "", false
	}
	word("CONCURRENTLY")
	word("IF", "NOT", "EXISTS")
	if i >= len(toks) || !toks[i].name() {
		return nil, "", false
	}
	i++
	if !word("ON") {
		return nil, "", false
	}
	word("ONLY")
	if i >= len(toks) || !toks[i].name() {
		return nil, "", false
	}
	i++
	if word("USING") {
		i++
	}
	if i >= len(toks) || !toks[i].group() {
		return nil, "", false
	}
	if keys, ok = indexKeys(d, toks[i].inner()); !ok {
		return nil, "", false
	}
	for i++; i < len(toks); {
		switch {
		case word("INCLUDE"), word("WITH"):
			if i >= len(toks) || !toks[i].group() {
				return nil, "", false
			}
			i++
		case word("NULLS", "DISTINCT"):
		case word("USING"), word("TABLESPACE"):
			i++
		case strings.EqualFold(toks[i].text, "WHERE"):
			return keys, strings.TrimSpace(src[toks[i].end:]), true
		default: // NULLS NOT DISTINCT, and what it can't read
			return nil, "", false
		}
	}
	return keys, "", true
}

// indexKeys is the key expressions of an index's parenthesised list.
func indexKeys(d schema.Dialect, list string) ([]string, bool) {
	var keys []string
	for _, part := range split(tokens(list)) {
		if len(part) == 0 {
			return nil, false
		}
		key, rest := part[0].text, part[1:]
		if !part[0].group() && !part[0].name() {
			return nil, false
		}
		if part[0].name() && len(rest) > 0 && rest[0].group() {
			if n := rest[0].inner(); d.Name == "mysql" && digits(n) {
				key = "LEFT(" + key + ", " + n + ")"
			} else {
				key += rest[0].text // a function call: lower("email")
			}
			rest = rest[1:]
		}
		for j := 0; j < len(rest); j++ {
			switch w := strings.ToUpper(rest[j].text); {
			case w == "COLLATE" && j+1 < len(rest):
				key += " COLLATE " + rest[j+1].text
				j++
			case w == "ASC", w == "DESC":
			case w == "NULLS" && j+1 < len(rest):
				j++
			case rest[j].name(): // an operator class
			default:
				return nil, false
			}
		}
		keys = append(keys, key)
	}
	return keys, len(keys) > 0
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// token is a word, a quoted name, a string, a parenthesised group (with
// what it holds) or a punctuation character of an SQL statement, at its
// offsets.
type token struct {
	text       string
	start, end int
}

func (t token) group() bool { return strings.HasPrefix(t.text, "(") }

func (t token) inner() string { return strings.TrimSpace(t.text[1 : len(t.text)-1]) }

// name is whether t is a word or a quoted name.
func (t token) name() bool {
	c := t.text[0]
	return c == '"' || c == '`' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tokens splits src into its top-level tokens; an unbalanced group or
// quote ends it.
func tokens(src string) []token {
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		start := i
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '"' || c == '`' || c == '\'':
			i = closeQuote(src, i)
		case c == '(':
			depth := 0
			for i < len(src) {
				switch src[i] {
				case '"', '`', '\'':
					i = closeQuote(src, i)
					continue
				case '(':
					depth++
				case ')':
					depth--
				}
				i++
				if depth == 0 {
					break
				}
			}
			if depth != 0 {
				return out
			}
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
			for i < len(src) && (src[i] == '_' || src[i] == '.' || src[i] >= 'a' && src[i] <= 'z' || src[i] >= 'A' && src[i] <= 'Z' || src[i] >= '0' && src[i] <= '9') {
				i++
			}
		default:
			i++
		}
		out = append(out, token{src[start:i], start, i})
	}
	return out
}

// closeQuote is the offset after the quote starting at i, a doubled
// quote kept inside; len(src) when it doesn't close.
func closeQuote(src string, i int) int {
	q := src[i]
	for i++; i < len(src); i++ {
		if src[i] == q {
			if i+1 < len(src) && src[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(src)
}

// split is toks at their top-level commas.
func split(toks []token) [][]token {
	var out [][]token
	var cur []token
	for _, t := range toks {
		if t.text == "," {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	return append(out, cur)
}
