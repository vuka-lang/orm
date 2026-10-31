package orm

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"
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
// MySQL searches text with MATCH … AGAINST and stores vectors without
// searching them (its VECTOR distances are HeatWave's only); SQLite
// searches a declared document's text by FTS5 (fts.go; LIKE without
// one), and computes vector distances and trigram similarity in Go
// functions the orm/sqlite driver package registers, with no index.

// TSVector is a Postgres tsvector column: a generated column holding a
// SearchVector (orm:"generated"), searched by its __search lookup.
// Elsewhere the column holds the document's text.
type TSVector string

// Vector is an embedding: a pgvector column on Postgres, its dimensions
// tagged on the field (orm:"vector:1536"); on MySQL a VECTOR column where
// the server has the type (MySQL 9, MariaDB 11.7), else a VARBINARY of
// the same bytes, so the column becomes a VECTOR in place once it does
// (ALTER TABLE … MODIFY); its text, [1,2,3], on SQLite. MySQL stores and
// reads vectors but can't search them: distances, Nearest and Hybrid are
// refused there when the query is written.
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

// bytes is the vector as MySQL holds it: little-endian float32s, what
// the ORM sends for a vector on MySQL in place of Value's text, which a
// VECTOR column refuses.
func (v Vector) bytes() []byte {
	if v == nil {
		return nil
	}
	b := make([]byte, 0, 4*len(v))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(x))
	}
	return b
}

// Scan reads pgvector's text, [1,2,3], or MySQL's bytes (bytes): a value
// that doesn't open with [ and close with ] is bytes, which only a vector
// whose last element is beyond 1e17 would.
func (v *Vector) Scan(src any) error {
	if src == nil {
		*v = nil
		return nil
	}
	if b, ok := src.([]byte); ok && !(len(b) >= 2 && b[0] == '[' && b[len(b)-1] == ']') {
		if len(b)%4 != 0 {
			return fmt.Errorf("orm: %d bytes are not a vector of float32s", len(b))
		}
		out := make(Vector, len(b)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		}
		*v = out
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
func L2Distance(field string, v Vector) OrderedExpr[float64] {
	return Ordered[float64](distance{L2, field, v})
}

// CosineDistance is the cosine distance from a vector field to v.
func CosineDistance(field string, v Vector) OrderedExpr[float64] {
	return Ordered[float64](distance{Cosine, field, v})
}

// InnerProduct is the inner product of a vector field and v, negated as
// pgvector's <#> is: smaller is nearer, as with the distances.
func InnerProduct(field string, v Vector) OrderedExpr[float64] {
	return Ordered[float64](distance{IP, field, v})
}

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
	if d.m < L2 || d.m > IP {
		return "", fmt.Errorf("orm: no metric %d: L2, Cosine or IP", d.m)
	}
	switch b.d.Name() {
	case "postgres":
		return "(" + col + " " + [...]string{L2: "<->", Cosine: "<=>", IP: "<#>"}[d.m] + " " + b.arg(d.v) + ")", nil
	case "sqlite":
		return [...]string{L2: "l2_distance(", Cosine: "cosine_distance(", IP: "-inner_product("}[d.m] + col + ", " + b.arg(d.v) + ")", nil
	}
	if b.d.Name() == "mysql" {
		return "", fmt.Errorf("orm: vector search (a distance, Nearest, Hybrid) needs Postgres (pgvector) or SQLite: MySQL stores %s's vectors but can't search them (its VECTOR distances are HeatWave's only)", d.field)
	}
	return "", fmt.Errorf("orm: vector search needs Postgres (pgvector) or SQLite, not %s", b.d.Name())
}

// metricOf is the metric a vector index of the field serves (Ops), L2
// without one.
func (m *model) metricOf(name string) Metric {
	f, ok := m.field(name)
	for _, ix := range m.indexes {
		if ix.kind != "hnsw" && ix.kind != "ivfflat" {
			continue
		}
		if g, gok := m.field(fmt.Sprint(ix.on[0])); ok && gok && g == f {
			return max(ix.ops, L2)
		}
	}
	return L2
}

// SearchOption shapes Nearest and Hybrid: a Metric, K, MaxDistance,
// DistanceAs, EfSearch, Probes; and Hybrid's HybridWeights, RRF,
// Candidates, RankAs and ScoreAs.
type SearchOption interface{ searchOption(*searchOpts) }

type searchOpts struct {
	metric                Metric
	k, candidates, rrf    int
	max                   *float64
	distance, rank, score string
	textW, vectorW        float64
	settings              []string
}

func (m Metric) searchOption(o *searchOpts) { o.metric = m }

type searchOptFunc func(*searchOpts)

func (f searchOptFunc) searchOption(o *searchOpts) { f(o) }

// K keeps the k nearest (best) rows: the query's Limit.
func K(k int) SearchOption { return searchOptFunc(func(o *searchOpts) { o.k = k }) }

// MaxDistance keeps the rows at most d from the vector: Nearest's filter,
// and the rows Hybrid ranks by distance as well as those the text matches.
func MaxDistance(d float64) SearchOption { return searchOptFunc(func(o *searchOpts) { o.max = &d }) }

// DistanceAs names the distance's annotation: "distance" otherwise.
func DistanceAs(alias string) SearchOption {
	return searchOptFunc(func(o *searchOpts) { o.distance = alias })
}

// RankAs and ScoreAs name Hybrid's text rank and fused score annotations:
// "rank" and "score" otherwise.
func RankAs(alias string) SearchOption { return searchOptFunc(func(o *searchOpts) { o.rank = alias }) }
func ScoreAs(alias string) SearchOption {
	return searchOptFunc(func(o *searchOpts) { o.score = alias })
}

// EfSearch is pgvector's hnsw.ef_search for the query, the candidates an
// HNSW scan keeps (40 by default: more finds more of the true nearest,
// slower); Probes is ivfflat.probes, the lists an IVFFlat scan reads (1
// by default). Set with SET LOCAL in a transaction of the query's own (a
// savepoint inside the context's), on Postgres only.
func EfSearch(n int) SearchOption {
	return searchOptFunc(func(o *searchOpts) { o.settings = append(o.settings, "SET LOCAL hnsw.ef_search = "+strconv.Itoa(n)) })
}

func Probes(n int) SearchOption {
	return searchOptFunc(func(o *searchOpts) { o.settings = append(o.settings, "SET LOCAL ivfflat.probes = "+strconv.Itoa(n)) })
}

// HybridWeights weighs Hybrid's text rank and vector distance in their
// fusion: 1 and 1 otherwise.
func HybridWeights(text, vector float64) SearchOption {
	return searchOptFunc(func(o *searchOpts) { o.textW, o.vectorW = text, vector })
}

// RRF is reciprocal-rank fusion's constant k: a row ranked r scores
// 1/(k + r); 60 otherwise. A smaller k favours the top ranks more.
func RRF(k int) SearchOption { return searchOptFunc(func(o *searchOpts) { o.rrf = k }) }

// Candidates is the n nearest rows Hybrid ranks by distance (all of them
// otherwise), beside those the text matches: a vector index finds them,
// where ranking every row by distance reads the table.
func Candidates(n int) SearchOption { return searchOptFunc(func(o *searchOpts) { o.candidates = n }) }

func searchOptsOf(opts []SearchOption) searchOpts {
	o := searchOpts{distance: "distance", rank: "rank", score: "score", textW: 1, vectorW: 1}
	for _, opt := range opts {
		opt.searchOption(&o)
	}
	return o
}

// Nearest orders the query by the distance of a vector field from v,
// nearest first, annotated as "distance" (DistanceAs): K keeps the k
// nearest, MaxDistance those near enough. The metric is the field's
// vector index's (Ops), L2 without one, or the option's. On Postgres the
// ORDER BY is pgvector's operator, so an HNSW or IVFFlat index of that
// metric serves it (EfSearch, Probes tune it); on SQLite the distance is
// computed in Go for every row; MySQL has no vector search.
//
//	Docs.Nearest("embedding", q, orm.K(10), orm.MaxDistance(0.4)).All(ctx)
//	Docs.Nearest("embedding", q, orm.Cosine, orm.DistanceAs("d")).Values[Hit]("id", "title", "d")
func (qs QuerySet[T]) Nearest(field string, v Vector, opts ...SearchOption) QuerySet[T] {
	qs.q = qs.q.nearest(field, v, opts)
	return qs
}

func (q *query) nearest(field string, v Vector, opts []SearchOption) *query {
	o := searchOptsOf(opts)
	d := Ordered[float64](distance{cmp.Or(o.metric, q.m.metricOf(field)), field, v})
	q = q.annotate(o.distance, []Expr{d}).orderBy([]any{o.distance})
	if o.max != nil {
		q = q.filter([]Cond{d.Le(*o.max)})
	}
	if o.k > 0 {
		q = q.limited(o.k)
	}
	return q.tuned(o.settings)
}

// Hybrid is hybrid search: the rows ranked twice, by the document's match
// of the text query (SearchRank, annotated "rank") and by the distance of
// a vector field from v (as Nearest, "distance"), the ranks fused by
// reciprocal rank (Fuse, "score": a row scores w/(k + r) for each
// ranking, from the text's only where the text matches) and ordered by
// score, best first. Every row is ranked by distance unless Candidates or
// MaxDistance narrow them; K keeps the best k. It runs where both
// searches do: Postgres, and SQLite (FTS5 and distances in Go).
//
//	Docs.Hybrid(orm.SearchVector("title", "body"), orm.SearchQuery("web server"), "embedding", v,
//		orm.K(10), orm.Candidates(100), orm.HybridWeights(1, 0.5)).All(ctx)
func (qs QuerySet[T]) Hybrid(doc Document, text TextQuery, field string, v Vector, opts ...SearchOption) QuerySet[T] {
	qs.q = qs.q.hybrid(doc, text, field, v, opts)
	return qs
}

func (q *query) hybrid(doc Document, text TextQuery, field string, v Vector, opts []SearchOption) *query {
	o := searchOptsOf(opts)
	d := Ordered[float64](distance{cmp.Or(o.metric, q.m.metricOf(field)), field, v})
	m := Match(doc, text)
	var near []Cond
	if o.candidates > 0 {
		near = append(near, nearestKeys{field, d, o.candidates})
	}
	if o.max != nil {
		near = append(near, d.Le(*o.max))
	}
	if len(near) > 0 {
		q = q.filter([]Cond{Or(m, And(near...))})
	}
	q = q.annotate(o.rank, []Expr{SearchRank(doc, text)}).annotate(o.distance, []Expr{d}).
		annotate(o.score, []Expr{Fusion{k: o.rrf, terms: []fuseTerm{{"-" + o.rank, o.textW, m}, {o.distance, o.vectorW, nil}}}}).
		orderBy([]any{"-" + o.score})
	if o.k > 0 {
		q = q.limited(o.k)
	}
	return q.tuned(o.settings)
}

// tuned is the query reading with settings SET LOCAL first.
func (q *query) tuned(settings []string) *query {
	if len(settings) == 0 {
		return q
	}
	n := q.next()
	n.settings = append(n.settings, settings...)
	return n
}

// tune is c with q's settings in force, on Postgres, for the statements
// sent on it until end: SET LOCAL in a transaction of its own, rolled
// back at the end (the query only reads), or in a savepoint of the
// context's transaction, rolled back to — so the settings end with it.
// Elsewhere, or without settings, c as it is.
func (q query) tune(ctx context.Context, c conn) (conn, func(), error) {
	if len(q.settings) == 0 || c.d.Name() != "postgres" || c.db == nil {
		return c, func() {}, nil
	}
	var end func()
	if tx, ok := c.run.(*sql.Tx); ok {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT orm_tune"); err != nil {
			return c, nil, err
		}
		end = func() {
			ctx := context.WithoutCancel(ctx)
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT orm_tune")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT orm_tune")
		}
	} else {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return c, nil, err
		}
		c.run, end = tx, func() { _ = tx.Rollback() }
	}
	for _, s := range q.settings {
		if _, err := c.exec(ctx, s, nil); err != nil {
			end()
			return c, nil, err
		}
	}
	return c, end, nil
}

// nearestKeys holds for the n rows nearest by d: their keys, as an index
// finds them.
type nearestKeys struct {
	field string
	d     OrderedExpr[float64]
	n     int
}

func (c nearestKeys) sql(b *builder) (string, error) {
	if b.m.PK == nil {
		return "", fmt.Errorf("orm: %s has no key to find its nearest rows by", b.m.Name)
	}
	s := query{m: b.m}.sub(b)
	dist, err := c.d.exprSQL(s)
	if err != nil {
		return "", err
	}
	return b.col(b.m.PK) + " IN (SELECT " + s.col(b.m.PK) + " FROM " + b.d.Quote(b.m.Table) + " AS " + b.d.Quote(s.alias) + " ORDER BY " + dist + " LIMIT " + strconv.Itoa(c.n) + ")", nil
}

// Fuse is reciprocal-rank fusion of orderings, for hybrid search: each
// name (a field or an annotation, a leading - for descending) ranks the
// rows, and a row scores the sum of w/(k + its rank), k 60 and each w 1
// unless K and Weights say — order by it descending. Hybrid fuses a text
// and a vector search with it.
//
//	Docs.Annotate("rank", orm.SearchRank(doc, q)).
//		Annotate("distance", orm.CosineDistance("embedding", v)).
//		Annotate("score", orm.Fuse("-rank", "distance").Weights(1, 0.5)).OrderBy("-score")
func Fuse(orders ...string) Fusion {
	f := Fusion{terms: make([]fuseTerm, len(orders))}
	for i, o := range orders {
		f.terms[i] = fuseTerm{order: o, weight: 1}
	}
	return f
}

// Fusion is a Fuse.
type Fusion struct {
	terms []fuseTerm
	k     int
}

type fuseTerm struct {
	order  string
	weight float64
	when   Cond // the rows the ordering ranks, nil for all: the others score 0 by it
}

// K is the fusion's constant (60 otherwise): a smaller k favours the top
// ranks more.
func (f Fusion) K(k int) Fusion {
	f.k = k
	return f
}

// Weights weighs the orderings, in order: 1 for those not given.
func (f Fusion) Weights(ws ...float64) Fusion {
	f.terms = slices.Clone(f.terms)
	for i := range min(len(ws), len(f.terms)) {
		f.terms[i].weight = ws[i]
	}
	return f
}

func (f Fusion) exprSQL(b *builder) (string, error) {
	k := strconv.Itoa(cmp.Or(f.k, 60))
	parts := make([]string, len(f.terms))
	for i, t := range f.terms {
		o, dir := t.order, " ASC"
		if strings.HasPrefix(o, "-") {
			o, dir = o[1:], " DESC"
		}
		w := strconv.FormatFloat(t.weight, 'f', -1, 64)
		if !strings.Contains(w, ".") {
			w += ".0"
		}
		// Written in the order the SQL reads, its arguments bound so: the
		// rows the ordering ranks numbered first, 1…, the others scoring 0.
		var when, first string
		var err error
		if t.when != nil {
			if when, err = t.when.sql(b); err == nil {
				first, err = t.when.sql(b)
			}
			if err != nil {
				return "", err
			}
		}
		col, err := b.ref(o)
		if err != nil {
			return "", err
		}
		if t.when == nil {
			parts[i] = w + " / (" + k + " + ROW_NUMBER() OVER (ORDER BY " + col + dir + "))"
			continue
		}
		parts[i] = "CASE WHEN " + when + " THEN " + w + " / (" + k + " + ROW_NUMBER() OVER (ORDER BY CASE WHEN " + first + " THEN 0 ELSE 1 END, " + col + dir + ")) ELSE 0 END"
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
// how words are stemmed. An index or a generated column needs one. On
// SQLite, english stems by FTS5's porter tokenizer.
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
// between alternatives, and -word must not; and, beyond Postgres's
// syntax, word* matches the words starting with word.
func SearchQuery(text string) TextQuery { return TextQuery{text: text} }

// TextQuery is a SearchQuery.
type TextQuery struct{ text, config string }

// Config is the text search configuration the query's words are stemmed
// by; the document's otherwise.
func (q TextQuery) Config(c string) TextQuery {
	q.config = c
	return q
}

// tsquery is the query on Postgres: websearch_to_tsquery's, or, with a
// prefix* in it, websearch_to_tsquery doesn't read, the same made of a
// query per term — plainto_tsquery of a word, phraseto_tsquery of a
// phrase, to_tsquery of 'prefix':* — joined by &&, || and !!.
func (b *builder) tsquery(q TextQuery, config string) (string, error) {
	cfg, err := regconfig(cmp.Or(q.config, config))
	if err != nil {
		return "", err
	}
	groups := webSearch(q.text)
	if !slices.ContainsFunc(groups, func(g []term) bool { return slices.ContainsFunc(g, func(t term) bool { return t.prefix }) }) {
		return "websearch_to_tsquery(" + cfg + b.arg(q.text) + ")", nil
	}
	alts := make([]string, len(groups))
	for i, g := range groups {
		ts := make([]string, len(g))
		for j, t := range g {
			switch {
			case t.prefix:
				ts[j] = "to_tsquery(" + cfg + b.arg("'"+strings.NewReplacer(`\`, `\\`, "'", "''").Replace(t.text)+"':*") + ")"
			case strings.ContainsFunc(t.text, unicode.IsSpace):
				ts[j] = "phraseto_tsquery(" + cfg + b.arg(t.text) + ")"
			default:
				ts[j] = "plainto_tsquery(" + cfg + b.arg(t.text) + ")"
			}
			if t.not {
				ts[j] = "!!" + ts[j]
			}
		}
		alts[i] = "(" + strings.Join(ts, " && ") + ")"
	}
	if len(alts) == 0 {
		return "websearch_to_tsquery(" + cfg + b.arg("") + ")", nil
	}
	return "(" + strings.Join(alts, " || ") + ")", nil
}

// Match holds when the document matches the query, Django's
// SearchVector(…) = SearchQuery(…): on Postgres the tsvector @@ the
// tsquery, on MySQL MATCH … AGAINST in boolean mode, on SQLite the FTS5
// table of a model declaring the document (a LIKE per word otherwise).
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
	if t, fcols := b.ftsDoc(m.d); t != nil {
		return b.ftsMatch(t, fcols, m.q.text), nil
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
			if t.prefix {
				// A prefix is a bare word: InnoDB reads no * after a phrase.
				w = strings.Map(func(r rune) rune {
					if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
						return r
					}
					return -1
				}, t.text) + "*"
				if w == "*" {
					continue
				}
			}
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
// Postgres's ts_rank, MySQL's relevance, SQLite's FTS5 bm25 negated (a
// model declaring the document), and elsewhere the weights of the fields
// each word is found in. The weights of a document's fields are A 1, B
// 0.4, C 0.2, D and none 0.1, as ts_rank's; bm25 weighs a field the
// document doesn't hold 0, and, unlike ts_rank, ranks a match in a short
// text above one in a long.
func SearchRank(d Document, q TextQuery) OrderedExpr[float64] { return Ordered[float64](rank{d, q}) }

// rankWeights is ts_rank's weights of a document's fields, by their
// Weight.
var rankWeights = map[string]string{"A": "1.0", "B": "0.4", "C": "0.2", "D": "0.1"}

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
	if t, fcols := b.ftsDoc(r.d); t != nil {
		weights := map[*field]string{}
		for _, p := range r.d.parts {
			for _, name := range p.fields {
				f, _ := b.m.field(name)
				weights[f] = cmp.Or(rankWeights[p.weight], "0.1")
			}
		}
		return b.ftsRank(t, fcols, weights, r.q.text), nil
	}
	var sum []string
	for _, group := range webSearch(r.q.text) {
		for _, t := range group {
			if t.not {
				continue
			}
			for i, p := range r.d.parts {
				w := rankWeights[p.weight]
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

// Headline is a field's text about the query's words, marked <b>…</b>:
// Postgres's ts_headline; on SQLite, where the model declares a document
// holding the field, FTS5's snippet (highlight with HighlightAll); and
// elsewhere, or where none of the words is found, the 160 characters
// about the first word, unmarked. MySQL's is that substring always.
//
//	Posts.Annotate("head", orm.Headline("body", q, orm.Marks("<mark>", "</mark>"), orm.MaxWords(20)))
func Headline(field string, q TextQuery, opts ...HeadlineOption) TextExpr {
	h := headline{field: field, q: q, o: headlineOpts{start: "<b>", stop: "</b>", words: 35}}
	for _, o := range opts {
		o(&h.o)
		h.set = true
	}
	return Text(h)
}

// HeadlineOption shapes a Headline: Marks, MaxWords, HighlightAll.
type HeadlineOption func(*headlineOpts)

type headlineOpts struct {
	start, stop string
	words       int
	all         bool
}

// Marks are the text put before and after each word found: <b> and </b>
// otherwise. They are bound arguments, written as given: escape the
// field's text yourself where it is HTML.
func Marks(start, stop string) HeadlineOption {
	return func(o *headlineOpts) { o.start, o.stop = start, stop }
}

// MaxWords is the most words a headline has: 35 otherwise (at most 64 on
// SQLite).
func MaxWords(n int) HeadlineOption { return func(o *headlineOpts) { o.words = max(n, 1) } }

// HighlightAll marks the words in the whole text rather than cutting a
// headline from it.
func HighlightAll() HeadlineOption { return func(o *headlineOpts) { o.all = true } }

type headline struct {
	field string
	q     TextQuery
	o     headlineOpts
	set   bool // options given: Postgres gets them
}

func (h headline) exprSQL(b *builder) (string, error) {
	col, f, err := b.refField(h.field)
	if err != nil {
		return "", err
	}
	if b.d.Name() == "postgres" {
		cfg, err := regconfig(h.q.config)
		if err != nil {
			return "", err
		}
		q, err := b.tsquery(h.q, "")
		if err != nil || !h.set {
			return "ts_headline(" + cfg + "COALESCE(" + col + ", ''), " + q + ")", err
		}
		opts, err := h.o.postgres()
		if err != nil {
			return "", err
		}
		return "ts_headline(" + cfg + "COALESCE(" + col + ", ''), " + q + ", " + b.arg(opts) + ")", nil
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
	plain := func() string {
		return "substr(" + col + ", max(1, instr(LOWER(" + col + "), " + b.arg(first) + ") - 40), 160)"
	}
	if f != nil {
		if t, _ := b.ftsFor([]*field{f}); t != nil {
			return b.ftsHeadline(t, f, h.q.text, h.o, plain), nil
		}
	}
	return plain(), nil
}

// postgres is the options as ts_headline reads them.
func (o headlineOpts) postgres() (string, error) {
	if strings.ContainsAny(o.start+o.stop, `",`) {
		return "", fmt.Errorf("orm: Headline marks %q and %q: Postgres reads no \" or , in them", o.start, o.stop)
	}
	s := `StartSel="` + o.start + `", StopSel="` + o.stop + `", MaxWords=` + strconv.Itoa(o.words) + ", MinWords=" + strconv.Itoa(min(15, max(1, o.words/2)))
	if o.all {
		s += ", HighlightAll=true"
	}
	return s, nil
}

// Similarity is the trigram similarity of a field and term, from 0 to 1:
// pg_trgm's on Postgres, the same computed in Go on SQLite.
func Similarity(field, term string) OrderedExpr[float64] {
	return Ordered[float64](similarity{field, term})
}

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
// absent; prefix, a word* standing for the words it starts.
type term struct {
	text        string
	not, prefix bool
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
		prefix := false
		if !quoted && strings.HasSuffix(w, "*") {
			w, prefix = strings.TrimRight(w, "*"), true
		}
		if w = strings.TrimSpace(w); w != "" {
			cur = append(cur, term{w, not, prefix})
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}
