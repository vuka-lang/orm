package orm

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vuka-lang/orm/internal/tags"
)

// Cond is a condition a QuerySet filters by: a Q, or Or / Not / And of
// conditions.
type Cond interface {
	sql(b *builder) (string, error)
}

// Q is conditions joined by AND, keyed Django's way: a field, then
// optionally __ and a lookup.
//
//	orm.Q{"age__gte": 18, "name__icontains": "a", "email": e}
//
// Lookups: exact (the default), iexact, contains, icontains, startswith,
// istartswith, endswith, iendswith, in, gt, gte, lt, lte, range, isnull,
// search (full text, in web search syntax; see SearchQuery) and
// trigram_similar. A nil value with exact means IS NULL.
type Q map[string]any

func (q Q) sql(b *builder) (string, error) {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		s, err := b.lookup(k, q[k])
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return join(parts, " AND "), nil
}

type group struct {
	op    string
	conds []Cond
}

func (g group) sql(b *builder) (string, error) {
	var parts []string
	for _, c := range g.conds {
		s, err := c.sql(b)
		if err != nil {
			return "", err
		}
		// An empty condition (Q{}, And()) is no condition, as Django's
		// Q(): it drops out of the combination.
		if s != "" {
			parts = append(parts, s)
		}
	}
	return join(parts, " "+g.op+" "), nil
}

// And holds when every condition does.
func And(conds ...Cond) Cond { return group{"AND", conds} }

// Or holds when any condition does.
func Or(conds ...Cond) Cond { return group{"OR", conds} }

type not struct{ c Cond }

func (n not) sql(b *builder) (string, error) {
	b.neg = !b.neg
	s, err := n.c.sql(b)
	b.neg = !b.neg
	if err != nil || s == "" {
		return s, err
	}
	return "NOT (" + s + ")", nil
}

// Not holds when c doesn't.
func Not(c Cond) Cond { return not{c} }

func join(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "(" + strings.Join(parts, sep) + ")"
}

var lookups = map[string]bool{
	"exact": true, "iexact": true, "contains": true, "icontains": true,
	"startswith": true, "istartswith": true, "endswith": true, "iendswith": true,
	"in": true, "gt": true, "gte": true, "lt": true, "lte": true,
	"range": true, "isnull": true, "search": true, "trigram_similar": true,
}

// stmt is what the builders of one statement share: its arguments and a
// counter naming its subqueries.
type stmt struct {
	args []any
	n    int
}

// join is a LEFT JOIN of a relation the statement follows, by its path
// from the queried model; on adds a FilteredRelation's condition to its
// ON, written where the FROM is.
type joinClause struct {
	path, sql string
	on        func() (string, error)
}

// builder writes SQL about one model under one alias: the queried model,
// a model a foreign key joins, or the related model of an EXISTS. Its
// joins go to the FROM it belongs to.
type builder struct {
	d     Dialect
	m     *model
	alias string
	path  string // the relation path from the queried model; "" for it
	ann   map[string]Expr
	depth int
	st    *stmt
	joins *[]joinClause
	outer *builder // the query a subquery's OuterRef reads
	neg   bool     // under an odd number of Nots
	many  bool     // names may follow relations holding many rows (Values)
	ddl   bool     // columns bare, for an index's or a generated column's expression
	// agg: an aggregate's Filter is being written: a relation holding many
	// rows that the statement already joins is the same joined row there
	// (Count("books__id").Filter(books__published) counts published
	// books), not an EXISTS over any of them.
	agg bool
}

// newBuilder is a builder of a statement on m, aliased by its table.
func newBuilder(d Dialect, m *model) *builder {
	// One allocation for the builder, its statement and its joins, with
	// room for a few arguments: most statements need no other.
	nb := &struct {
		b     builder
		st    stmt
		joins []joinClause
		args  [4]any
	}{}
	nb.st.args = nb.args[:0]
	nb.b = builder{d: d, m: m, alias: m.Table, st: &nb.st, joins: &nb.joins}
	return &nb.b
}

func (b *builder) arg(v any) string {
	// Times go as UTC: Postgres keeps a TIMESTAMP's wall clock and reads
	// it back as UTC, so a local time would come back shifted; MySQL's
	// driver converts to its loc either way.
	switch t := v.(type) {
	case time.Time:
		v = t.UTC()
	case *time.Time:
		if t != nil {
			u := t.UTC()
			v = &u
		}
	// A vector goes to MySQL as the bytes its columns hold.
	case Vector:
		if b.d.Name() == "mysql" {
			v = t.bytes()
		}
	case *Vector:
		if t != nil && b.d.Name() == "mysql" {
			v = t.bytes()
		}
	}
	b.st.args = append(b.st.args, v)
	return b.d.Placeholder(len(b.st.args))
}

// args is the statement's arguments, in the order the SQL names them.
func (b *builder) args() []any { return b.st.args }

// col is a column of b's model, qualified by its alias.
func (b *builder) col(f *field) string {
	if b.ddl {
		return b.bare(f)
	}
	if n := b.m.names(b.d); n != nil && b.alias == b.m.Table {
		if s, ok := n.col[f]; ok {
			return s
		}
	}
	return b.d.Quote(b.alias) + "." + b.d.Quote(f.Column)
}

// bare is a column unqualified, as UPDATE's SET names it.
func (b *builder) bare(f *field) string {
	if n := b.m.names(b.d); n != nil {
		if s, ok := n.bare[f]; ok {
			return s
		}
	}
	return b.d.Quote(f.Column)
}

// from is the FROM of b's statement: its table and the joins its
// conditions, orders and columns follow. at is where the FROM stands
// among the statement's arguments (see joinSQL).
func (b *builder) from(at int) (string, error) {
	var s string
	if n := b.m.names(b.d); n != nil {
		s = n.table
	} else {
		s = b.d.Quote(b.m.Table)
	}
	if b.alias != b.m.Table {
		s += " AS " + b.d.Quote(b.alias)
	}
	j, err := b.joinSQL(at)
	return s + j, err
}

// joinSQL is b's joins. A FilteredRelation's condition is written now,
// after the conditions that named it: its arguments are moved to at, the
// FROM's place among them, which positional ? marks need ($1 marks are
// numbered as written and need nothing).
func (b *builder) joinSQL(at int) (string, error) {
	var s strings.Builder
	n := len(b.st.args)
	for _, j := range *b.joins {
		s.WriteString(j.sql)
		if j.on == nil {
			continue
		}
		c, err := j.on()
		if err != nil {
			return "", err
		}
		if c != "" {
			s.WriteString(" AND (" + c + ")")
		}
	}
	if args := b.st.args; len(args) > n && at < n && b.d.Placeholder(1) == "?" {
		moved := slices.Clone(args[n:])
		copy(args[at+len(moved):], args[at:n])
		copy(args[at:], moved)
	}
	return s.String(), nil
}

// follow is the builder of the model a relation leads to, LEFT JOINed
// once per path: a foreign key's row, or, for Values, rows held by many
// (through the table between, for a many-to-many).
func (b *builder) follow(r *relation) (*builder, error) {
	return b.joinTo(r, strings.TrimPrefix(b.path+"__"+tags.Snake(r.Name), "__"), nil)
}

// joinTo is follow under path, the related rows limited to those where
// cond holds when there is one (a FilteredRelation, path its name).
func (b *builder) joinTo(r *relation, path string, cond Cond) (*builder, error) {
	if err := r.noJoin(); err != nil {
		return nil, err
	}
	t, rf, err := r.ends()
	if err != nil {
		return nil, err
	}
	alias := path
	if alias == b.joinRoot() {
		alias += "_"
	}
	nb := &builder{d: b.d, m: t, alias: alias, path: path, st: b.st, joins: b.joins, outer: b.outer, neg: b.neg, many: b.many, agg: b.agg}
	for _, j := range *b.joins {
		if j.path == path {
			return nb, nil
		}
	}
	q := b.d.Quote
	join, on := "", nb.col(rf)+" = "+b.col(r.local)
	if r.Kind == relElements {
		through, tval := elementsFrom(b.d, b.col(r.local), alias+"_through", r.jsonLocal(), intKind(rf.Type))
		join = " LEFT JOIN " + through + " ON TRUE"
		on = nb.col(rf) + " = " + tval
	}
	if r.Kind == relM2M {
		through := q(alias + "_through")
		join = " LEFT JOIN " + q(r.Through) + " AS " + through + " ON " + through + "." + q(r.ThroughLocal) + " = " + b.col(r.local)
		on = nb.col(rf) + " = " + through + "." + q(r.ThroughRemote)
	}
	j := joinClause{path: path, sql: join + " LEFT JOIN " + q(t.Table) + " AS " + q(alias) + " ON " + on}
	if cond != nil {
		sub := *nb
		sub.joins, sub.neg = &[]joinClause{}, false
		j.on = func() (string, error) {
			s, err := cond.sql(&sub)
			if err == nil && len(*sub.joins) > 0 {
				err = fmt.Errorf("orm: FilteredRelation %q: its condition may name only %s's own fields", path, t.Name)
			}
			return s, err
		}
	}
	*b.joins = append(*b.joins, j)
	return nb, nil
}

// FilteredRelation is a relation (a path of them, posts__comments) whose
// related rows are only those where cond holds, Django's FilteredRelation:
// annotate it under a name, then name the related fields through it. cond
// names the related model's own fields. Reading through it joins with
// cond in the ON, so rows without a match keep a row of NULLs; filtering
// through a relation holding many rows asks EXISTS one matching.
//
//	Users.Annotate("active_apps", orm.FilteredRelation("applications", orm.Q{"status": "active"})).
//		Annotate("n", orm.Count("active_apps__id")).Values[[]any]("name", "n")
func FilteredRelation(relation string, cond Cond) Expr { return filteredRel{relation, cond} }

type filteredRel struct {
	relation string
	cond     Cond
}

func (f filteredRel) exprSQL(*builder) (string, error) {
	return "", fmt.Errorf("orm: FilteredRelation(%q) is a relation: name a field through it", f.relation)
}

// hop is the relation a FilteredRelation filters, by its index in the
// parts of a name read through it; at is -1 for none.
type hop struct {
	at   int
	name string
	cond Cond
}

// noField is the error of a name b's model lacks, pointing at the
// annotation it may have meant: annotations match as they are written
// (totalPlaced is not total_placed).
func (b *builder) noField(name string) error {
	first := strings.SplitN(name, "__", 2)[0]
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for a := range b.ann {
		if a != first && norm(a) == norm(first) {
			return fmt.Errorf("orm: %s has no field %q — the query annotates %q: annotation names match as written", b.m.Name, first, a)
		}
	}
	return fmt.Errorf("orm: %s has no field %q", b.m.Name, name)
}

// aggregated is whether c names an aggregate annotation, a condition
// HAVING holds.
func (b *builder) aggregated(c Cond) bool {
	switch c := c.(type) {
	case Q:
		for k := range c {
			if isAggregate(b.ann[strings.SplitN(k, "__", 2)[0]]) {
				return true
			}
		}
	case exprCond:
		return b.aggExpr(c.e)
	case group:
		return slices.ContainsFunc(c.conds, b.aggregated)
	case not:
		return b.aggregated(c.c)
	}
	return false
}

// expand is parts with a FilteredRelation's name in front replaced by its
// relation path, and the hop it filters.
func (b *builder) expand(parts []string) ([]string, hop) {
	f, ok := b.ann[parts[0]].(filteredRel)
	if !ok {
		return parts, hop{at: -1}
	}
	rel := strings.Split(f.relation, "__")
	return append(rel, parts[1:]...), hop{len(rel) - 1, parts[0], f.cond}
}

// joined is whether the statement already joins the relation r, step i
// of a name read through hop h.
func (b *builder) joined(r *relation, i int, h hop) bool {
	path := strings.TrimPrefix(b.path+"__"+tags.Snake(r.Name), "__")
	if i == h.at {
		path = h.name
	}
	return slices.ContainsFunc(*b.joins, func(j joinClause) bool { return j.path == path })
}

// via is the builder of the model r leads to: through h's join when r is
// the hop h filters.
func (b *builder) via(r *relation, i int, h hop) (*builder, error) {
	if i == h.at {
		return b.joinTo(r, h.name, h.cond)
	}
	return b.follow(r)
}

// joinRoot is the alias of the FROM's own table, which no join may take.
func (b *builder) joinRoot() string {
	if b.path == "" {
		return b.alias
	}
	return ""
}

// ref is a name as SQL: an annotation's expression, a field's column, or
// a path through foreign keys to one (author__name).
func (b *builder) ref(name string) (string, error) {
	s, _, err := b.refField(name)
	return s, err
}

// maxPath is the most parts a name may have: its fields, relations,
// transforms and lookup, joined by __. A name read from a request can't
// make a statement of thousands of joins or nested subqueries, whose SQL
// takes time quadratic in its length to write.
const maxPath = 32

// checkPath refuses a name of more than maxPath parts.
func checkPath(name string) error {
	if strings.Count(name, "__") < maxPath {
		return nil
	}
	if len(name) > 64 {
		name = name[:64] + "…"
	}
	return fmt.Errorf("orm: %q has more than %d parts", name, maxPath)
}

// refField is ref and the field it reads, nil for an annotation.
func (b *builder) refField(name string) (string, *field, error) {
	if err := checkPath(name); err != nil {
		return "", nil, err
	}
	parts, h := b.expand(strings.Split(name, "__"))
	if e, ok := b.ann[name]; ok && h.at < 0 {
		if b.depth > 16 {
			return "", nil, fmt.Errorf("orm: annotation %q refers to itself", name)
		}
		b.depth++
		defer func() { b.depth-- }()
		s, err := e.exprSQL(b)
		if err != nil {
			return "", nil, err
		}
		return "(" + s + ")", nil, nil
	}
	cur := b
	for i, part := range parts {
		last := i == len(parts)-1
		if f, ok := cur.m.field(part); ok && last {
			if f.Via != "" {
				return cur.refField(f.Via)
			}
			return cur.col(f), f, nil
		}
		r, ok := cur.m.relation(part)
		if !ok {
			break
		}
		if !r.one() && !cur.many {
			return "", nil, fmt.Errorf("orm: %s.%s holds many rows: it can't be read as one value", cur.m.Name, r.Name)
		}
		if (last || r.keyStep(parts[i+1:])) && r.Kind == relFK && i != h.at {
			return cur.col(r.local), r.local, nil
		}
		next, err := cur.via(r, i, h)
		if err != nil {
			return "", nil, err
		}
		if last {
			return next.col(next.m.PK), next.m.PK, nil
		}
		cur = next
	}
	return "", nil, b.noField(name)
}

// lookup is the SQL of one Q entry: a field (after any foreign keys) and
// its transforms and lookup, or an EXISTS over a relation that holds many
// rows (posts__title__icontains).
func (b *builder) lookup(key string, v any) (string, error) {
	if err := checkPath(key); err != nil {
		return "", err
	}
	parts := strings.Split(key, "__")
	lookup := "exact"
	if len(parts) > 1 && isLookup(parts[len(parts)-1]) {
		lookup = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	parts, h := b.expand(parts)
	if _, ok := b.ann[parts[0]]; ok && h.at < 0 {
		col, err := b.ref(parts[0])
		if err != nil {
			return "", err
		}
		return b.transformed(col, parts[1:], lookup, v, key)
	}
	return b.lookupParts(parts, lookup, v, key, h)
}

// lookupParts is lookup of a key split into its parts and its lookup.
func (b *builder) lookupParts(parts []string, lookup string, v any, key string, h hop) (string, error) {
	cur := b
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if f, ok := cur.m.field(part); ok && (!f.fk || i == len(parts)-1) {
			if f.Via != "" {
				return cur.lookup(strings.Join(append(strings.Split(f.Via, "__"), parts[i+1:]...), "__")+"__"+lookup, v)
			}
			if lookup == "search" && i == len(parts)-1 {
				g, tsv, config := cur.m.searchFor(f)
				if s, ok := cur.ftsLookup(g, fmt.Sprint(v)); ok {
					return s, nil
				}
				return cur.search(cur.col(g), tsv, config, fmt.Sprint(v))
			}
			if i == len(parts)-1 {
				var err error
				if v, err = cur.m.outCond(f, lookup, v); err != nil {
					return "", err
				}
			}
			s, err := cur.transformed(cur.col(f), parts[i+1:], lookup, v, key)
			return cur.nullSafe(s, f, lookup, v), err
		}
		r, ok := cur.m.relation(part)
		if !ok {
			return "", cur.noField(part)
		}
		rest := parts[i+1:]
		if r.one() {
			if (len(rest) == 0 || r.keyStep(rest)) && r.Kind == relFK && i != h.at {
				s, err := cur.compare(cur.col(r.local), lookup, v, key)
				return cur.nullSafe(s, r.local, lookup, v), err
			}
			next, err := cur.via(r, i, h)
			if err != nil {
				return "", err
			}
			cur = next
			if len(rest) == 0 {
				s, err := cur.compare(cur.col(cur.m.PK), lookup, v, key)
				return cur.nullSafe(s, cur.m.PK, lookup, v), err
			}
			continue
		}
		if cur.agg && cur.joined(r, i, h) {
			next, err := cur.via(r, i, h)
			if err != nil {
				return "", err
			}
			cur = next
			if len(rest) == 0 {
				s, err := cur.compare(cur.col(cur.m.PK), lookup, v, key)
				return cur.nullSafe(s, cur.m.PK, lookup, v), err
			}
			continue
		}
		h.at -= i + 1
		return cur.exists(r, rest, lookup, v, key, h)
	}
	return "", b.noField(key)
}

// nullSafe keeps a negated lookup true of a NULL column, as Django's
// exclude does: NOT (bio LIKE 'a%') is unknown where bio is NULL, so a
// column that can be NULL (a nullable field, or any reached through a
// LEFT JOIN) is compared as (bio LIKE 'a%' AND bio IS NOT NULL).
func (b *builder) nullSafe(s string, f *field, lookup string, v any) string {
	if !b.neg || s == "" || lookup == "isnull" || v == nil {
		return s
	}
	if b.path == "" && f.Type.Kind() != reflect.Pointer {
		return s
	}
	return "(" + s + " AND " + b.col(f) + " IS NOT NULL)"
}

// transformed is col through the transforms named, compared by lookup.
func (b *builder) transformed(col string, steps []string, lookup string, v any, key string) (string, error) {
	for _, name := range steps {
		f, ok := transform(name)
		if !ok {
			return "", fmt.Errorf("orm: %q in %q is no field, relation, transform or lookup of %s%s", name, key, b.m.Name, didYouMean(name, lookupNames(true)))
		}
		s, err := f.Of(rawSQL(col)).exprSQL(b)
		if err != nil {
			return "", err
		}
		col = s
	}
	return b.compare(col, lookup, v, key)
}

// exists is a lookup across a relation holding many rows: EXISTS a
// related row matching the rest of the key. With no rest, isnull asks
// whether there is none. h.at is the hop's index in rest, -1 when r is
// the relation a FilteredRelation filters.
func (b *builder) exists(r *relation, rest []string, lookup string, v any, key string, h hop) (string, error) {
	if err := r.noJoin(); err != nil {
		return "", err
	}
	t, rf, err := r.ends()
	if err != nil {
		return "", err
	}
	b.st.n++
	alias := "orm_" + strconv.Itoa(b.st.n)
	child := &builder{d: b.d, m: t, alias: alias, path: alias, st: b.st, joins: &[]joinClause{}, outer: b.outer}
	from := b.d.Quote(t.Table) + " AS " + b.d.Quote(alias)
	link := child.col(rf) + " = " + b.col(r.local)
	sel := "EXISTS (SELECT 1 FROM "
	if r.Kind == relElements {
		through, tval := elementsFrom(b.d, b.col(r.local), alias+"_through", r.jsonLocal(), intKind(rf.Type))
		from = through + ", " + from
		link = child.col(rf) + " = " + tval
		if b.d.Name() == "mysql" {
			// MySQL's semijoin rewrite loses JSON_TABLE's reference to
			// the outer row and matches nothing: keep the subquery as
			// written.
			sel = "EXISTS (SELECT /*+ NO_SEMIJOIN() */ 1 FROM "
		}
	}
	if r.Kind == relM2M {
		through := b.d.Quote(alias + "_through")
		from = b.d.Quote(r.Through) + " AS " + through + " JOIN " + from + " ON " + child.col(rf) + " = " + through + "." + b.d.Quote(r.ThroughRemote)
		link = through + "." + b.d.Quote(r.ThroughLocal) + " = " + b.col(r.local)
	}
	at := len(b.st.args)
	if h.at == -1 && h.cond != nil {
		c, err := h.cond.sql(child)
		if err != nil {
			return "", err
		}
		if c != "" {
			link += " AND (" + c + ")"
		}
	}
	if len(rest) == 0 {
		if lookup != "isnull" {
			return "", fmt.Errorf("orm: %q compares %s.%s, which holds many rows: name one of its fields", key, b.m.Name, r.Name)
		}
		j, err := child.joinSQL(at)
		if err != nil {
			return "", err
		}
		s := sel + from + j + " WHERE " + link + ")"
		if yes, _ := v.(bool); yes {
			s = "NOT " + s
		}
		return s, nil
	}
	cond, err := child.lookupParts(rest, lookup, v, key, h)
	if err != nil {
		return "", err
	}
	j, err := child.joinSQL(at)
	if err != nil {
		return "", err
	}
	return sel + from + j + " WHERE " + link + " AND " + cond + ")", nil
}

// elementsFrom is the elements of arr — a JSON array, else
// comma-separated text (spaces forgiven) — as a table the related rows
// join on: each row's array expanded once, so the join probes the
// related key by index instead of testing membership per pair. val is
// one element, typed for the related column; a NULL arr expands to
// nothing.
func elementsFrom(d Dialect, arr, alias string, json, numeric bool) (from, val string) {
	q := d.Quote(alias)
	switch d.Name() {
	case "postgres":
		if json {
			from, val = "jsonb_array_elements_text(("+arr+")::jsonb) AS "+q+"(v)", q+".v"
		} else {
			from, val = "unnest(string_to_array("+arr+", ',')) AS "+q+"(v)", "NULLIF(btrim("+q+".v), '')"
		}
		if numeric {
			val = "(" + val + ")::bigint"
		}
		return from, val
	case "mysql":
		doc, col := arr, "v BIGINT PATH '$'"
		if !numeric {
			col = "v VARCHAR(255) PATH '$'"
		}
		if !json {
			doc = "CONCAT('[', REPLACE(" + arr + ", ' ', ''), ']')"
			if !numeric {
				doc = `CONCAT('["', REPLACE(REPLACE(` + arr + `, ' ', ''), ',', '","'), '"]')`
			}
		}
		return "JSON_TABLE(" + doc + ", '$[*]' COLUMNS (" + col + ")) AS " + q, q + ".v"
	}
	doc := arr
	if !json {
		doc = "'[' || REPLACE(" + arr + ", ' ', '') || ']'"
		if !numeric {
			doc = `'["' || REPLACE(REPLACE(` + arr + `, ' ', ''), ',', '","') || '"]'`
		}
	}
	val = q + ".value"
	if numeric {
		val = "CAST(" + val + " AS INTEGER)"
	}
	return "json_each(" + doc + ") AS " + q, val
}

// intKind is whether t (or what it points to) is an integer.
func intKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// rawSQL is SQL already written, passed back into a template.
type rawSQL string

func (r rawSQL) exprSQL(*builder) (string, error) { return string(r), nil }

// pattern is a LIKE pattern around v: a value escaped, an Expr
// concatenated.
func (b *builder) pattern(v any, before, after string) (string, error) {
	if e, ok := v.(Expr); ok {
		s, err := e.exprSQL(b)
		if err != nil {
			return "", err
		}
		concat := func(parts ...string) string {
			if b.d.Name() == "mysql" {
				return "CONCAT(" + strings.Join(parts, ", ") + ")"
			}
			return strings.Join(parts, " || ")
		}
		var parts []string
		if before != "" {
			parts = append(parts, "'"+before+"'")
		}
		parts = append(parts, s)
		if after != "" {
			parts = append(parts, "'"+after+"'")
		}
		return concat(parts...), nil
	}
	return b.arg(before + likeEscape(fmt.Sprint(v)) + after), nil
}

// compare is col against v by lookup; v an Expr or a value.
func (b *builder) compare(col, lookup string, v any, key string) (string, error) {
	like := func(before, after string, insensitive bool) (string, error) {
		p, err := b.pattern(v, before, after)
		if err != nil {
			return "", err
		}
		if insensitive {
			return b.d.ILike(col, p), nil
		}
		return col + " LIKE " + p + " ESCAPE '!'", nil
	}
	cmp := func(op string) (string, error) {
		o, err := b.operand(v)
		if err != nil {
			return "", err
		}
		return col + " " + op + " " + o, nil
	}
	switch lookup {
	case "exact":
		if v == nil {
			return col + " IS NULL", nil
		}
		return cmp("=")
	case "iexact":
		o, err := b.operand(v)
		if err != nil {
			return "", err
		}
		return "LOWER(" + col + ") = LOWER(" + o + ")", nil
	case "contains":
		return like("%", "%", false)
	case "icontains":
		return like("%", "%", true)
	case "startswith":
		return like("", "%", false)
	case "istartswith":
		return like("", "%", true)
	case "endswith":
		return like("%", "", false)
	case "iendswith":
		return like("%", "", true)
	case "gt":
		return cmp(">")
	case "gte":
		return cmp(">=")
	case "lt":
		return cmp("<")
	case "lte":
		return cmp("<=")
	case "isnull":
		if yes, _ := v.(bool); yes {
			return col + " IS NULL", nil
		}
		return col + " IS NOT NULL", nil
	case "in":
		if sq, ok := v.(subquerier); ok {
			s, err := sq.subquerySQL(b, false)
			if err != nil {
				return "", err
			}
			return col + " IN (" + s + ")", nil
		}
		items, err := list(v)
		if err != nil {
			return "", fmt.Errorf("orm: %s__in: %w", key, err)
		}
		if len(items) == 0 {
			return "1 = 0", nil
		}
		marks := make([]string, len(items))
		for i, x := range items {
			if marks[i], err = b.operand(x); err != nil {
				return "", err
			}
		}
		return col + " IN (" + strings.Join(marks, ", ") + ")", nil
	case "search":
		return b.search(col, false, "", fmt.Sprint(v))
	case "trigram_similar":
		return b.trigramSimilar(col, fmt.Sprint(v)), nil
	case "range":
		items, err := list(v)
		if err != nil || len(items) != 2 {
			return "", fmt.Errorf("orm: %s__range needs two values", key)
		}
		lo, err := b.operand(items[0])
		if err != nil {
			return "", err
		}
		hi, err := b.operand(items[1])
		if err != nil {
			return "", err
		}
		return col + " BETWEEN " + lo + " AND " + hi, nil
	}
	return b.custom(col, lookup, v)
}

// list is a slice or array's items.
func list(v any) ([]any, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, fmt.Errorf("want a slice, got %T", v)
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, nil
}
