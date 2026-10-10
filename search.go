package orm

import (
	"cmp"
	"database/sql/driver"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Text search, Django's contrib.postgres.search, and vector search,
// pgvector's, each written for the database the query runs on: Postgres
// does both natively (pg_trgm and pgvector for trigrams and vectors);
// MySQL searches text with MATCH … AGAINST and has no vectors; SQLite
// matches text by LIKE, and computes vector distances and trigram
// similarity in Go functions the orm/sqlite driver package registers,
// with no index.

// TSVector is a Postgres tsvector column: a generated column holding a
// SearchVector (orm:"generated"), searched by its __search lookup.
// Elsewhere the column holds the document's text.
type TSVector string

// Vector is an embedding: a pgvector column on Postgres, its dimensions
// tagged on the field (orm:"vector:1536"), and its text, [1,2,3],
// elsewhere.
type Vector []float32

// Value writes the vector as pgvector's text.
func (v Vector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	b := []byte{'['}
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(x), 'g', -1, 32)
	}
	return string(append(b, ']')), nil
}

// Scan reads pgvector's text.
func (v *Vector) Scan(src any) error {
	if src == nil {
		*v = nil
		return nil
	}
	s := strings.Trim(strings.TrimSpace(text(src)), "[]")
	out := Vector{}
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		f, err := strconv.ParseFloat(part, 32)
		if err != nil {
			return fmt.Errorf("orm: %q is not a vector: %w", text(src), err)
		}
		out = append(out, float32(f))
	}
	*v = out
	return nil
}

var (
	vectorType   = reflect.TypeFor[Vector]()
	tsvectorType = reflect.TypeFor[TSVector]()
)

// Metric is how vectors are compared: their Euclidean distance (L2), the
// cosine distance, or the inner product.
type Metric int

const (
	L2 Metric = iota + 1
	Cosine
	IP
)

// L2Distance is the Euclidean distance from a vector field to v.
func L2Distance(field string, v Vector) Expr { return distance{L2, field, v} }

// CosineDistance is the cosine distance from a vector field to v.
func CosineDistance(field string, v Vector) Expr { return distance{Cosine, field, v} }

// InnerProduct is the inner product of a vector field and v, negated as
// pgvector's <#> is: smaller is nearer, as with the distances.
func InnerProduct(field string, v Vector) Expr { return distance{IP, field, v} }

type distance struct {
	m     Metric
	field string
	v     Vector
}

func (d distance) exprSQL(b *builder) (string, error) {
	col, err := b.ref(d.field)
	if err != nil {
		return "", err
	}
	v := b.arg(d.v)
	switch b.d.Name() {
	case "postgres":
		return "(" + col + " " + [...]string{L2: "<->", Cosine: "<=>", IP: "<#>"}[d.m] + " " + v + ")", nil
	case "sqlite":
		return [...]string{L2: "l2_distance(", Cosine: "cosine_distance(", IP: "-inner_product("}[d.m] + col + ", " + v + ")", nil
	}
	return "", fmt.Errorf("orm: vector distances need Postgres (pgvector) or SQLite, not %s", b.d.Name())
}

// Nearest orders the query by the distance of a vector field from v,
// nearest first, annotated as "distance".
//
//	Docs.Nearest("embedding", q, orm.Cosine).Limit(10).All(ctx)
func (qs QuerySet[T]) Nearest(field string, v Vector, m Metric) QuerySet[T] {
	return qs.Annotate("distance", distance{m, field, v}).OrderBy("distance")
}

// Fuse is reciprocal-rank fusion of orderings, for hybrid search: each
// name (a field or an annotation, a leading - for descending) ranks the
// rows, and a row scores the sum of 1/(60 + its rank) — order by it
// descending.
//
//	Docs.Annotate("rank", orm.SearchRank(doc, q)).
//		Annotate("distance", orm.CosineDistance("embedding", v)).
//		Annotate("score", orm.Fuse("-rank", "distance")).OrderBy("-score")
func Fuse(orders ...string) Expr { return fuse(orders) }

type fuse []string

func (f fuse) exprSQL(b *builder) (string, error) {
	parts := make([]string, len(f))
	for i, o := range f {
		dir := " ASC"
		if strings.HasPrefix(o, "-") {
			o, dir = o[1:], " DESC"
		}
		col, err := b.ref(o)
		if err != nil {
			return "", err
		}
		parts[i] = "1.0 / (60 + ROW_NUMBER() OVER (ORDER BY " + col + dir + "))"
	}
	return "(" + strings.Join(parts, " + ") + ")", nil
}

// SearchVector is fields as a document to search, Django's SearchVector:
// Match, SearchRank and Headline read it, and an index or a generated
// column may hold it. On Postgres it is a tsvector; elsewhere the
// fields' text.
//
//	doc := orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B")).Config("english")
func SearchVector(fields ...string) Document {
	return Document{parts: []docPart{{fields: fields}}}
}

// Document is a SearchVector.
type Document struct {
	parts  []docPart
	config string
}

type docPart struct {
	fields []string
	weight string
}

// Weight ranks the document's fields: A (the highest), B, C or D.
func (d Document) Weight(w string) Document {
	d.parts = slices.Clone(d.parts)
	for i := range d.parts {
		d.parts[i].weight = w
	}
	return d
}

// Config is the Postgres text search configuration (english, simple):
// how words are stemmed. An index or a generated column needs one.
func (d Document) Config(c string) Document {
	d.config = c
	return d
}

// Add is the two documents as one, Django's +.
func (d Document) Add(o Document) Document {
	d.parts = append(slices.Clip(d.parts), o.parts...)
	d.config = cmp.Or(d.config, o.config)
	return d
}

// columns is the document's fields as SQL, by part.
func (d Document) columns(b *builder) ([][]string, error) {
	out := make([][]string, len(d.parts))
	for i, p := range d.parts {
		for _, f := range p.fields {
			col, err := b.ref(f)
			if err != nil {
				return nil, err
			}
			out[i] = append(out[i], col)
		}
	}
	return out, nil
}

// text is cols as one text: what's searched where there is no tsvector.
func (b *builder) text(cols []string) string {
	if b.d.Name() == "mysql" {
		return "CONCAT_WS(' ', " + strings.Join(cols, ", ") + ")"
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "COALESCE(" + c + ", '')"
	}
	return strings.Join(parts, " || ' ' || ")
}

func (d Document) exprSQL(b *builder) (string, error) {
	cols, err := d.columns(b)
	if err != nil {
		return "", err
	}
	if b.d.Name() != "postgres" {
		return b.text(slices.Concat(cols...)), nil
	}
	if b.ddl && d.config == "" {
		return "", fmt.Errorf("orm: a SearchVector in an index or a generated column needs a Config: Postgres must know how it was stemmed")
	}
	cfg, err := regconfig(d.config)
	if err != nil {
		return "", err
	}
	vs := make([]string, len(d.parts))
	for i, p := range d.parts {
		vs[i] = "to_tsvector(" + cfg + b.text(cols[i]) + ")"
		if p.weight != "" {
			if !strings.Contains("ABCD", p.weight) || len(p.weight) != 1 {
				return "", fmt.Errorf("orm: SearchVector weight %q: A, B, C or D", p.weight)
			}
			vs[i] = "setweight(" + vs[i] + ", '" + p.weight + "')"
		}
	}
	return "(" + strings.Join(vs, " || ") + ")", nil
}

var configRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// regconfig is a text search configuration as the first argument of a
// Postgres search function, "" for the database's default.
func regconfig(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	if !configRE.MatchString(c) {
		return "", fmt.Errorf("orm: %q is no text search configuration", c)
	}
	return "'" + c + "', ", nil
}

// SearchQuery is text to search for in web search syntax, Postgres's
// websearch_to_tsquery: every word must match, "a phrase" in order, or
// between alternatives, and -word must not.
func SearchQuery(text string) TextQuery { return TextQuery{text: text} }

// TextQuery is a SearchQuery.
type TextQuery struct{ text, config string }

// Config is the text search configuration the query's words are stemmed
// by; the document's otherwise.
func (q TextQuery) Config(c string) TextQuery {
	q.config = c
	return q
}

// tsquery is the query on Postgres.
func (b *builder) tsquery(q TextQuery, config string) (string, error) {
	cfg, err := regconfig(cmp.Or(q.config, config))
	if err != nil {
		return "", err
	}
	return "websearch_to_tsquery(" + cfg + b.arg(q.text) + ")", nil
}

// Match holds when the document matches the query, Django's
// SearchVector(…) = SearchQuery(…).
//
//	Posts.Filter(orm.Match(orm.SearchVector("title", "body"), orm.SearchQuery(`go -java "web server"`)))
func Match(d Document, q TextQuery) Cond { return match{d, q} }

type match struct {
	d Document
	q TextQuery
}

func (m match) sql(b *builder) (string, error) {
	cols, err := m.d.columns(b)
	if err != nil {
		return "", err
	}
	switch b.d.Name() {
	case "postgres":
		doc, err := m.d.exprSQL(b)
		if err != nil {
			return "", err
		}
		q, err := b.tsquery(m.q, m.d.config)
		return doc + " @@ " + q, err
	case "mysql":
		return b.against(slices.Concat(cols...), m.q.text), nil
	}
	return b.likeWords(b.text(slices.Concat(cols...)), m.q.text), nil
}

// against is MySQL's full-text match of cols, a FULLTEXT index's: the web
// search in boolean mode, each alternative's words required and -words
// excluded. A word InnoDB's index leaves out by default (shorter than
// innodb_ft_min_token_size, 3, or a stopword) is not required, as
// Postgres drops its stopwords: required, it would match nothing.
func (b *builder) against(cols []string, text string) string {
	var alts []string
	for _, group := range webSearch(text) {
		var words []string
		for _, t := range group {
			w := `"` + strings.ReplaceAll(t.text, `"`, "") + `"`
			switch {
			case t.not:
				words = append(words, "-"+w)
			case !strings.ContainsFunc(t.text, unicode.IsSpace) && (utf8.RuneCountInString(t.text) < 3 || innodbStopwords[strings.ToLower(t.text)]):
			default:
				words = append(words, "+"+w)
			}
		}
		alts = append(alts, "("+strings.Join(words, " ")+")")
	}
	return "MATCH(" + strings.Join(cols, ", ") + ") AGAINST(" + b.arg(strings.Join(alts, " ")) + " IN BOOLEAN MODE)"
}

// innodbStopwords is InnoDB's default full-text stopword list.
var innodbStopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a about an are as at be by com de en for from how i in is it la of on or that the this to und was what when where who will with www") {
		innodbStopwords[w] = true
	}
}

// SearchRank is how well the document matches the query, higher better:
// Postgres's ts_rank, MySQL's relevance, and elsewhere the weights of
// the fields each word is found in (A 1, B 0.4, C 0.2, D and none 0.1).
func SearchRank(d Document, q TextQuery) Expr { return rank{d, q} }

type rank struct {
	d Document
	q TextQuery
}

func (r rank) exprSQL(b *builder) (string, error) {
	cols, err := r.d.columns(b)
	if err != nil {
		return "", err
	}
	switch b.d.Name() {
	case "postgres":
		doc, err := r.d.exprSQL(b)
		if err != nil {
			return "", err
		}
		q, err := b.tsquery(r.q, r.d.config)
		return "ts_rank(" + doc + ", " + q + ")", err
	case "mysql":
		return b.against(slices.Concat(cols...), r.q.text), nil
	}
	var sum []string
	for _, group := range webSearch(r.q.text) {
		for _, t := range group {
			if t.not {
				continue
			}
			for i, p := range r.d.parts {
				w := map[string]string{"A": "1.0", "B": "0.4", "C": "0.2"}[p.weight]
				for _, c := range cols[i] {
					sum = append(sum, cmp.Or(w, "0.1")+" * (instr(LOWER("+c+"), "+b.arg(strings.ToLower(t.text))+") > 0)")
				}
			}
		}
	}
	if len(sum) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(sum, " + ") + ")", nil
}

// Headline is a field's text around the query's words: Postgres's
// ts_headline (the words marked <b>…</b>), elsewhere the 160 characters
// about the first word.
func Headline(field string, q TextQuery) Expr { return headline{field, q} }

type headline struct {
	field string
	q     TextQuery
}

func (h headline) exprSQL(b *builder) (string, error) {
	col, err := b.ref(h.field)
	if err != nil {
		return "", err
	}
	if b.d.Name() == "postgres" {
		cfg, err := regconfig(h.q.config)
		if err != nil {
			return "", err
		}
		q, err := b.tsquery(h.q, "")
		return "ts_headline(" + cfg + "COALESCE(" + col + ", ''), " + q + ")", err
	}
	first := ""
	for _, group := range webSearch(h.q.text) {
		for _, t := range group {
			if !t.not && first == "" {
				first = strings.ToLower(t.text)
			}
		}
	}
	if b.d.Name() == "mysql" {
		return "SUBSTRING(" + col + ", GREATEST(1, LOCATE(" + b.arg(first) + ", " + col + ") - 40), 160)", nil
	}
	return "substr(" + col + ", max(1, instr(LOWER(" + col + "), " + b.arg(first) + ") - 40), 160)", nil
}

// Similarity is the trigram similarity of a field and term, from 0 to 1:
// pg_trgm's on Postgres, the same computed in Go on SQLite.
func Similarity(field, term string) Expr { return similarity{field, term} }

type similarity struct{ field, term string }

func (s similarity) exprSQL(b *builder) (string, error) {
	col, err := b.ref(s.field)
	if err != nil {
		return "", err
	}
	if b.d.Name() == "mysql" {
		return "", fmt.Errorf("orm: Similarity needs Postgres (pg_trgm) or SQLite")
	}
	return "similarity(" + col + ", " + b.arg(s.term) + ")", nil
}

// search is col__search: on Postgres to_tsvector(col) @@ the query, by
// config when given (as a FullTextIndex has it), or a tsvector column
// (tsv) matched as it is; MySQL's MATCH; elsewhere a LIKE per word.
func (b *builder) search(col string, tsv bool, config, text string) (string, error) {
	switch b.d.Name() {
	case "postgres":
		if !tsv {
			cfg, err := regconfig(config)
			if err != nil {
				return "", err
			}
			col = "to_tsvector(" + cfg + "COALESCE(" + col + ", ''))"
		}
		q, err := b.tsquery(TextQuery{text: text}, config)
		return col + " @@ " + q, err
	case "mysql":
		return b.against([]string{col}, text), nil
	}
	return b.likeWords("COALESCE("+col+", '')", text), nil
}

// trigramSimilar is col__trigram_similar: pg_trgm's %, the same
// threshold (0.3) on SQLite, a LIKE on MySQL.
func (b *builder) trigramSimilar(col, term string) string {
	switch b.d.Name() {
	case "postgres":
		return col + " % " + b.arg(term)
	case "sqlite":
		return "similarity(" + col + ", " + b.arg(term) + ") >= 0.3"
	}
	return b.d.ILike(col, b.arg("%"+likeEscape(term)+"%"))
}

// searchFor is what f__search matches: the generated column holding a
// SearchVector of f (or f, a TSVector column) and its configuration, else
// f under the configuration of a FullTextIndex of f alone.
func (m *model) searchFor(f *field) (col *field, tsv bool, config string) {
	if f.Type == tsvectorType {
		d, _ := f.Gen.(Document)
		return f, true, d.config
	}
	for _, g := range m.Fields {
		d, ok := g.Gen.(Document)
		if !ok {
			continue
		}
		for _, p := range d.parts {
			for _, name := range p.fields {
				if h, ok := m.field(name); ok && h == f {
					return g, true, d.config
				}
			}
		}
	}
	for _, ix := range m.indexes {
		if ix.kind != "fulltext" || len(ix.on) != 1 {
			continue
		}
		if h, ok := m.field(fmt.Sprint(ix.on[0])); ok && h == f {
			return f, false, cmp.Or(ix.config, "simple")
		}
	}
	return f, false, ""
}

// likeWords is text's web search matched by LIKE, for databases without
// full-text search: every word of one alternative, none excluded.
func (b *builder) likeWords(col, text string) string {
	var alts []string
	for _, group := range webSearch(text) {
		conds := make([]string, len(group))
		for i, t := range group {
			conds[i] = b.d.ILike(col, b.arg("%"+likeEscape(t.text)+"%"))
			if t.not {
				conds[i] = "NOT " + conds[i]
			}
		}
		alts = append(alts, "("+strings.Join(conds, " AND ")+")")
	}
	if len(alts) == 0 {
		return "1 = 0"
	}
	return "(" + strings.Join(alts, " OR ") + ")"
}

// term is a word or a phrase of a web search: not, when it must be
// absent.
type term struct {
	text string
	not  bool
}

// webSearch is web search text as alternatives (split at or), each the
// terms all of which must hold.
func webSearch(s string) [][]term {
	var groups [][]term
	var cur []term
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		not := s[0] == '-'
		if not {
			s = s[1:]
		}
		var w string
		quoted := strings.HasPrefix(s, `"`)
		if quoted {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				end = len(s) - 1
			}
			w, s = s[1:end+1], s[min(end+2, len(s)):]
		} else {
			end := strings.IndexFunc(s, unicode.IsSpace)
			if end < 0 {
				end = len(s)
			}
			w, s = s[:end], s[end:]
		}
		if !quoted && !not && strings.EqualFold(w, "or") {
			if len(cur) > 0 {
				groups, cur = append(groups, cur), nil
			}
			continue
		}
		if w = strings.TrimSpace(w); w != "" {
			cur = append(cur, term{w, not})
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}
