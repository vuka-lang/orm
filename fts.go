package orm

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/vuka-lang/orm/internal/schema"
)

// SQLite searches text by FTS5. A model declaring a document — a
// generated TSVector column of a SearchVector, a FullTextIndex, a
// GinIndex of a SearchVector — has, on SQLite, a full-text table over
// the document's fields: <table>_fts, an external-content FTS5 table
// (content=<table>, content_rowid=<its integer key>) that triggers on the
// table keep in step with its rows. Migrations make it (rebuilt from the
// rows there are) and drop it as they do an index. Match, SearchRank,
// Headline and __search read it: the query's words as FTS5 phrases,
// ranked by bm25, marked by snippet and highlight. A model without a
// document, or with a key that isn't an integer, is searched by LIKE.

// ftsTable is a model's SQLite full-text table.
type ftsTable struct {
	name   string
	m      *model
	fields []*field // its columns, in the model's order
	// english: the document's words stemmed by FTS5's porter tokenizer,
	// as Postgres's english configuration stems them, and its stopwords
	// left out of queries as Postgres leaves them out.
	english bool
}

// fts is the model's full-text table on SQLite, nil when it declares no
// document or has no integer key.
func (m *model) fts() *ftsTable {
	if m.PK == nil || !intKind(m.PK.Type) {
		return nil
	}
	t := &ftsTable{m: m}
	add := func(name string) {
		if f, ok := m.field(name); ok && f.Type != tsvectorType && f.Gen == nil && f.Via == "" && !slices.Contains(t.fields, f) {
			t.fields = append(t.fields, f)
		}
	}
	doc := func(d Document) {
		for _, p := range d.parts {
			for _, name := range p.fields {
				add(name)
			}
		}
		t.english = t.english || d.config == "english"
	}
	for _, f := range m.Fields {
		if d, ok := f.Gen.(Document); ok {
			doc(d)
		}
	}
	for _, ix := range m.indexes {
		for _, o := range ix.on {
			switch o := o.(type) {
			case Document:
				doc(o)
			case string:
				if ix.kind == "fulltext" {
					add(o)
					t.english = t.english || ix.config == "english"
				}
			}
		}
	}
	if len(t.fields) == 0 {
		return nil
	}
	slices.SortFunc(t.fields, func(a, b *field) int { return slices.Index(m.Fields, a) - slices.Index(m.Fields, b) })
	t.name = schema.Ident(m.Table + "_fts")
	return t
}

// index is the full-text table as a declared index: the statements making
// it, its triggers and its rows, and those dropping them.
func (t *ftsTable) index() schema.Index {
	q := sqlite{}.Quote
	tq, name := q(t.m.Table), q(t.name)
	cols := make([]string, len(t.fields))
	for i, f := range t.fields {
		cols[i] = q(f.Column)
	}
	list := strings.Join(cols, ", ")
	row := func(of string) string {
		vs := []string{of + "." + q(t.m.PK.Column)}
		for _, c := range cols {
			vs = append(vs, of+"."+c)
		}
		return strings.Join(vs, ", ")
	}
	insert := "INSERT INTO " + name + "(rowid, " + list + ") VALUES (" + row("new") + ");"
	remove := "INSERT INTO " + name + "(" + name + ", rowid, " + list + ") VALUES ('delete', " + row("old") + ");"
	tokenize := "unicode61"
	if t.english {
		tokenize = "porter unicode61"
	}
	trigger := func(suffix, event, body string) string {
		return "CREATE TRIGGER IF NOT EXISTS " + q(schema.Ident(t.name+suffix)) + " AFTER " + event + " ON " + tq + " BEGIN " + body + " END"
	}
	create := []string{
		"CREATE VIRTUAL TABLE IF NOT EXISTS " + name + " USING fts5(" + list + ", content='" + strings.ReplaceAll(t.m.Table, "'", "''") + "', content_rowid='" + strings.ReplaceAll(t.m.PK.Column, "'", "''") + "', tokenize='" + tokenize + "')",
		trigger("_ai", "INSERT", insert),
		trigger("_ad", "DELETE", remove),
		trigger("_au", "UPDATE OF "+q(t.m.PK.Column)+", "+list, remove+" "+insert),
		"INSERT INTO " + name + "(" + name + ") VALUES ('rebuild')",
	}
	var drop []string
	for _, s := range []string{"_ai", "_ad", "_au"} {
		drop = append(drop, "DROP TRIGGER IF EXISTS "+q(schema.Ident(t.name+s)))
	}
	drop = append(drop, "DROP TABLE IF EXISTS "+name)
	return schema.Index{
		Name: t.name,
		SQL:  schema.Dialects{SQLite: strings.Join(create, ";\n")},
		Drop: schema.Dialects{SQLite: strings.Join(drop, ";\n")},
	}
}

// ftsFor is the full-text table of b's model holding fields on SQLite,
// and the columns a query of them is filtered to (nil: all of them); nil
// when there is none or it lacks one of them.
func (b *builder) ftsFor(fields []*field) (*ftsTable, []string) {
	if b.d.Name() != "sqlite" || len(fields) == 0 {
		return nil, nil
	}
	t := b.m.fts()
	if t == nil {
		return nil, nil
	}
	var cols []string
	for _, f := range fields {
		if !slices.Contains(t.fields, f) {
			return nil, nil
		}
		if !slices.Contains(cols, f.Column) {
			cols = append(cols, f.Column)
		}
	}
	if len(cols) == len(t.fields) {
		cols = nil
	}
	return t, cols
}

// fieldsOf is the document's fields, the model's own; false when one is
// not (a path through a relation).
func (d Document) fieldsOf(m *model) ([]*field, bool) {
	var out []*field
	for _, p := range d.parts {
		for _, name := range p.fields {
			f, ok := m.field(name)
			if !ok {
				return nil, false
			}
			out = append(out, f)
		}
	}
	return out, true
}

// ftsDoc is the full-text table holding d's fields on SQLite, with the
// columns its queries are filtered to.
func (b *builder) ftsDoc(d Document) (*ftsTable, []string) {
	fs, ok := d.fieldsOf(b.m)
	if !ok {
		return nil, nil
	}
	return b.ftsFor(fs)
}

// ftsQuery is a web search as FTS5 queries: match, every alternative
// holding words and phrases (each required, a prefix* a prefix, the
// -terms excluded), as one query; nots, each alternative of exclusions
// only, as the query of what it excludes; and any, every word and phrase
// of the search, for marking them.
type ftsQuery struct {
	match string
	nots  []string
	any   string
}

func (t *ftsTable) query(text string, cols []string) ftsQuery {
	var out ftsQuery
	var alts, anys []string
	for _, group := range webSearch(text) {
		var pos, neg []string
		for _, w := range group {
			if !strings.ContainsFunc(w.text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
				continue
			}
			if t.english && !w.prefix && !strings.ContainsFunc(w.text, unicode.IsSpace) && pgStopwords[strings.ToLower(w.text)] {
				continue
			}
			p := `"` + strings.ReplaceAll(w.text, `"`, `""`) + `"`
			if w.prefix {
				p += "*"
			}
			if w.not {
				neg = append(neg, p)
			} else {
				pos = append(pos, p)
			}
		}
		anys = append(anys, pos...)
		switch {
		case len(pos) > 0 && len(neg) > 0:
			alts = append(alts, "(("+strings.Join(pos, " AND ")+") NOT ("+strings.Join(neg, " OR ")+"))")
		case len(pos) > 0:
			alts = append(alts, "("+strings.Join(pos, " AND ")+")")
		case len(neg) > 0:
			out.nots = append(out.nots, ftsColumns(cols, strings.Join(neg, " OR ")))
		}
	}
	if len(alts) > 0 {
		out.match = ftsColumns(cols, strings.Join(alts, " OR "))
	}
	if len(anys) > 0 {
		out.any = strings.Join(anys, " OR ")
	}
	return out
}

// ftsColumns is an FTS5 query filtered to cols, nil for every column.
func ftsColumns(cols []string, q string) string {
	if cols == nil {
		return q
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	return "{" + strings.Join(quoted, " ") + "} : (" + q + ")"
}

// ftsMatch is the condition that a row matches text in the full-text
// table's cols: its key among those the table finds, or, for an
// alternative of exclusions only, not among those it excludes.
func (b *builder) ftsMatch(t *ftsTable, cols []string, text string) string {
	fq := t.query(text, cols)
	q := b.d.Quote
	pk := b.col(t.m.PK)
	sub := " (SELECT rowid FROM " + q(t.name) + " WHERE " + q(t.name) + " MATCH "
	var alts []string
	if fq.match != "" {
		alts = append(alts, pk+" IN"+sub+b.arg(fq.match)+")")
	}
	for _, n := range fq.nots {
		alts = append(alts, pk+" NOT IN"+sub+b.arg(n)+")")
	}
	if len(alts) == 0 {
		return "1 = 0"
	}
	return "(" + strings.Join(alts, " OR ") + ")"
}

// ftsRank is the bm25 rank of a row's match of text in the full-text
// table, negated (higher better, as ts_rank), each column weighted as
// weights has it; 0 where it doesn't match.
func (b *builder) ftsRank(t *ftsTable, cols []string, weights map[*field]string, text string) string {
	fq := t.query(text, cols)
	if fq.match == "" {
		return "0"
	}
	q := b.d.Quote
	ws := make([]string, len(t.fields))
	for i, f := range t.fields {
		ws[i] = weights[f]
		if ws[i] == "" {
			ws[i] = "0"
		}
	}
	return "COALESCE((SELECT -bm25(" + q(t.name) + ", " + strings.Join(ws, ", ") + ") FROM " + q(t.name) + " WHERE " + q(t.name) + " MATCH " + b.arg(fq.match) + " AND rowid = " + b.col(t.m.PK) + "), 0)"
}

// ftsHeadline is the field's text marked where text's words are, by
// snippet (MaxWords words about them) or, with HighlightAll, highlight;
// fallback where the row has none of them.
func (b *builder) ftsHeadline(t *ftsTable, f *field, text string, o headlineOpts, fallback func() string) string {
	fq := t.query(text, nil)
	if fq.any == "" {
		return fallback()
	}
	q := b.d.Quote
	col := strconv.Itoa(slices.Index(t.fields, f))
	start, stop := b.arg(o.start), b.arg(o.stop)
	fn := "snippet(" + q(t.name) + ", " + col + ", " + start + ", " + stop + ", '', " + strconv.Itoa(min(o.words, 64)) + ")"
	if o.all {
		fn = "highlight(" + q(t.name) + ", " + col + ", " + start + ", " + stop + ")"
	}
	return "COALESCE((SELECT " + fn + " FROM " + q(t.name) + " WHERE " + q(t.name) + " MATCH " + b.arg(fq.any) + " AND rowid = " + b.col(t.m.PK) + "), " + fallback() + ")"
}

// ftsLookup is __search on SQLite by the full-text table, of what
// searchFor found: a field's own column, or the document a generated
// TSVector holds.
func (b *builder) ftsLookup(f *field, text string) (string, bool) {
	fs := []*field{f}
	if d, ok := f.Gen.(Document); ok {
		if fs, ok = d.fieldsOf(b.m); !ok {
			return "", false
		}
	}
	t, cols := b.ftsFor(fs)
	if t == nil {
		return "", false
	}
	return b.ftsMatch(t, cols, text), true
}

// pgStopwords is Postgres's english configuration's stopwords (Snowball's
// list): words its queries drop, so they're dropped from SQLite's english
// queries too, where FTS5 would require them.
var pgStopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`i me my myself we our ours ourselves you your yours yourself yourselves
		he him his himself she her hers herself it its itself they them their theirs themselves what which
		who whom this that these those am is are was were be been being have has had having do does did
		doing a an the and but if or because as until while of at by for with about against between into
		through during before after above below to from up down in out on off over under again further then
		once here there when where why how all any both each few more most other some such no nor not only
		own same so than too very s t can will just don should now`) {
		pgStopwords[w] = true
	}
}
