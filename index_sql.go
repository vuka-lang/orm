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

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// allDBs is the databases indexes are written for, in the order
// migrations write them.
var allDBs = []DialectName{Postgres, MySQL, SQLite}

var dbTitles = map[DialectName]string{Postgres: "Postgres", MySQL: "MySQL", SQLite: "SQLite"}

// indexDB is how one database makes declared indexes: what of an index it
// can't make, and the statement making the rest.
type indexDB interface {
	lacks(ix Index) (lack, bool)
	statement(w *indexWriter) (string, error)
}

var indexDBs = map[DialectName]indexDB{Postgres: postgresIndexes{}, MySQL: mysqlIndexes{}, SQLite: sqliteIndexes{}}

// lack is what a database can't make of an index: the feature, and the
// option declaring it.
type lack struct{ feature, option string }

// madeIndex is a declared index as one database makes it.
type madeIndex struct {
	name string
	db   DialectName
	stmt string
}

// declaredIndexes is the model's declared indexes as migrations make
// them: one per name, with each database's statement (an index and its
// fallback of the same name are one). An index a database can't make,
// keys that aren't the table's own, and names that clash fail it.
func (m *model) declaredIndexes() ([]schema.Index, error) {
	if err := m.checkIndexNames(); err != nil {
		return nil, err
	}
	var out []schema.Index
	for _, ix := range m.indexes {
		made, err := m.place(ix, allDBs)
		if err != nil {
			return nil, err
		}
		for _, mi := range made {
			i := slices.IndexFunc(out, func(s schema.Index) bool { return s.Name == mi.name })
			if i < 0 {
				i = len(out)
				out = append(out, schema.Index{Name: mi.name})
			}
			out[i].SQL = out[i].SQL.Set(string(mi.db), mi.stmt)
		}
	}
	return out, nil
}

// checkIndexNames fails on two declarations of one index name: a
// fallback may share its index's.
func (m *model) checkIndexNames() error {
	owner := map[string]int{}
	for n, ix := range m.indexes {
		for _, c := range ix.chain() {
			if err := m.checkIndex(c); err != nil {
				return err
			}
			name := m.indexName(c)
			if o, ok := owner[name]; ok && o != n {
				return fmt.Errorf("orm: %s declares two indexes named %s: name one of them (Name)", m.Name, name)
			}
			owner[name] = n
		}
	}
	return nil
}

// place is ix made on the databases within it is declared on: the
// statement of each that can make it, and its fallback (Else) on those
// that can't; with none, the rule's error.
func (m *model) place(ix Index, within []DialectName) ([]madeIndex, error) {
	name := m.indexName(ix)
	var made []madeIndex
	var lacking []DialectName
	var first lack
	for _, db := range ix.databases(within) {
		if l, ok := indexDBs[db].lacks(ix); ok {
			if lacking == nil {
				first = l
			}
			lacking = append(lacking, db)
			continue
		}
		stmt, err := m.indexStatement(db, ix, name)
		if err != nil {
			return nil, err
		}
		if stmt != "" {
			made = append(made, madeIndex{name, db, stmt})
		}
	}
	if len(lacking) == 0 {
		return made, nil
	}
	if ix.orElse == nil {
		return nil, m.cantMake(ix, name, ix.databases(within), lacking, first)
	}
	instead, err := m.place(*ix.orElse, lacking)
	return append(made, instead...), err
}

// onDBs is the databases a declaration on a table is made on, as On
// names them.
type onDBs struct {
	dbs []DialectName
	set bool // On was called: dbs is the whole of them, even none
}

// among is the databases of within On named; of dflt when it wasn't
// called.
func (o onDBs) among(within, dflt []DialectName) []DialectName {
	dbs := dflt
	if o.set {
		dbs = o.dbs
	}
	return slices.DeleteFunc(slices.Clone(within), func(db DialectName) bool { return !slices.Contains(dbs, db) })
}

// check fails on a name On was given that is no database's.
func (o onDBs) check() error {
	for _, db := range o.dbs {
		if dbTitles[db] == "" {
			return fmt.Errorf("On names postgres, mysql or sqlite, not %q", db)
		}
	}
	return nil
}

// databases is the databases among within the index is made on: On's,
// Postgres alone for its own kinds (unless Else says what the others
// make), else every one.
func (i Index) databases(within []DialectName) []DialectName {
	if i.postgresKind() && i.orElse == nil {
		return i.on.among(within, []DialectName{Postgres})
	}
	return i.on.among(within, allDBs)
}

// postgresKind is whether the index is of a kind only Postgres has, which
// is made there alone by default.
func (i Index) postgresKind() bool {
	switch i.method {
	case "gin", "gist", "hnsw", "ivfflat":
		return true
	}
	return false
}

// cantMake is the rule's error: the database that can't make ix, what it
// lacks, and the two ways out.
func (m *model) cantMake(ix Index, name string, scope, lacking []DialectName, l lack) error {
	var on []string
	for _, db := range scope {
		if !slices.Contains(lacking, db) {
			on = append(on, strconv.Quote(string(db)))
		}
	}
	instead := "orm.IndexOf(…)"
	if fields := ix.fields(); len(fields) == len(ix.keys) {
		quoted := make([]string, len(fields))
		for i, f := range fields {
			quoted[i] = strconv.Quote(f)
		}
		instead = "orm.IndexOf(" + strings.Join(quoted, ", ") + ")"
	}
	titles := make([]string, len(lacking))
	for i, db := range lacking {
		titles[i] = dbTitles[db]
	}
	fixOn, fixElse := ".On("+strings.Join(on, ", ")+")", ".Else("+instead+")"
	width := max(len(fixOn), len(fixElse)) + 8
	return fmt.Errorf("orm: %s: %s has no %s (%s on %s).\n  %-*smake it only there\n  %-*smake this instead on %s",
		m.Name, dbTitles[lacking[0]], l.feature, l.option, name, width, fixOn, width, fixElse, strings.Join(titles, " and "))
}

var (
	paramRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	collateRE = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)
	methods   = []string{"", "btree", "gin", "gist", "hash", "brin", "spgist", "hnsw", "ivfflat"}
)

// checkIndex is what is wrong with ix whatever the database: no keys, an
// unnamed index of expressions, an unknown database, method, field,
// parameter or collation, an option of another kind of index.
func (m *model) checkIndex(ix Index) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("orm: %s's index %s: %s", m.Name, m.indexName(ix), fmt.Sprintf(format, args...))
	}
	if err := ix.on.check(); err != nil {
		return fail("%v", err)
	}
	if ix.raw != nil {
		if ix.name == "" {
			return fmt.Errorf("orm: %s: a RawIndex needs a name", m.Name)
		}
		for db := range ix.raw {
			if dbTitles[db] == "" {
				return fail("its statements are for postgres, mysql or sqlite, not %q", db)
			}
		}
		return nil
	}
	switch {
	case len(ix.keys) == 0:
		return fmt.Errorf("orm: %s: an index needs keys", m.Name)
	case ix.name == "" && len(ix.fields()) == 0:
		return fmt.Errorf("orm: %s: an index of expressions alone needs a Name, so its name stays the same", m.Name)
	case !slices.Contains(methods, ix.method):
		return fail("no access method %q: hash, brin or spgist", ix.method)
	case ix.vector() && len(ix.keys) != 1:
		return fail("a vector index is of one field")
	case ix.trigram && ix.method != "gin" && ix.method != "gist":
		return fail("Trigram is a GinIndex's or a GistIndex's")
	}
	for _, f := range ix.include {
		if _, ok := m.field(f); !ok {
			return fail("Include names %s, no field of %s", f, m.Name)
		}
	}
	for _, p := range ix.with {
		if !paramRE.MatchString(p.key) {
			return fail("With(%q): no storage parameter", p.key)
		}
	}
	for _, k := range ix.keys {
		if k.collate != "" && !collateRE.MatchString(k.collate) {
			return fail("Collate(%q): no collation", k.collate)
		}
		if k.ops != "" && !paramRE.MatchString(k.ops) {
			return fail("Ops(%q): no operator class", k.ops)
		}
	}
	return nil
}

// indexName is the index's name: Name's, else <table>_<fields>_<kind>.
func (m *model) indexName(ix Index) string {
	if ix.name != "" {
		return ix.name
	}
	names := ix.fields()
	for i, f := range names {
		names[i] = strings.ReplaceAll(f, "__", "_")
	}
	kind := cmp.Or(ix.method, "idx")
	if ix.fulltext {
		kind = "fulltext"
	}
	return m.Table + "_" + strings.Join(names, "_") + "_" + kind
}

// indexStatement is the statement making ix named name on db; "" for
// none (a RawIndex without one, SQLite's full-text table).
func (m *model) indexStatement(db DialectName, ix Index, name string) (string, error) {
	if ix.raw != nil {
		return ix.raw[db], nil
	}
	return indexDBs[db].statement(&indexWriter{m: m, ix: ix, name: name, d: DialectFor(string(db))})
}

// indexWriter writes one index's statement on one database: the parts
// every database's is made of.
type indexWriter struct {
	m    *model
	ix   Index
	name string
	d    Dialect
}

func (w *indexWriter) fail(err error) error {
	return fmt.Errorf("orm: %s's index %s: %w", w.m.Name, w.name, err)
}

// create is CREATE <kind>INDEX "name" ON "table": kind UNIQUE, FULLTEXT
// or none.
func (w *indexWriter) create(kind string) string {
	if w.ix.unique {
		kind = "UNIQUE "
	}
	return "CREATE " + kind + "INDEX " + w.d.Quote(w.name) + " ON " + w.d.Quote(w.m.Table)
}

// keys is keys written by key (each a column or a parenthesised
// expression, with the database's options), joined.
func (w *indexWriter) keys(keys []IndexKey, key func(k IndexKey, sql string) string) (string, error) {
	b := newBuilder(w.d, w.m)
	b.ddl = true
	parts := make([]string, len(keys))
	for i, k := range keys {
		var s string
		var err error
		switch v := k.of.(type) {
		case string:
			s, err = b.ref(v)
		case Expr:
			s, err = v.exprSQL(b)
			s = "(" + s + ")"
		default:
			err = fmt.Errorf("an index's keys are field names, Exprs and Keys, not %T", k.of)
		}
		if err != nil {
			return "", w.fail(err)
		}
		parts[i] = key(k, s)
	}
	if len(*b.joins) > 0 || len(b.args()) > 0 {
		return "", w.fail(fmt.Errorf("its keys are the table's own columns, and take no values"))
	}
	return strings.Join(parts, ", "), nil
}

// where is " WHERE cond" of a partial index, its values written in.
func (w *indexWriter) where() (string, error) {
	if w.ix.where == nil {
		return "", nil
	}
	s, own, err := w.m.rowCond(w.d, w.ix.where)
	if err != nil {
		return "", w.fail(err)
	}
	if !own {
		return "", w.fail(fmt.Errorf("its Where reads the row's own columns, and its values are bools, numbers, strings or nil"))
	}
	return " WHERE " + s, nil
}

// rowCond is cond as a statement on the model's table writes it (a
// partial index's, a check's): its values written in. own is false when
// it reads another table, or holds a value SQL can't write in.
func (m *model) rowCond(d Dialect, cond Cond) (sql string, own bool, err error) {
	b := newBuilder(d, m)
	b.ddl, b.literal = true, true
	if sql, err = cond.sql(b); err != nil {
		return "", false, err
	}
	return sql, len(*b.joins) == 0 && len(b.args()) == 0, nil
}

// include is " INCLUDE (columns)" of a covering index.
func (w *indexWriter) include() string {
	if len(w.ix.include) == 0 {
		return ""
	}
	cols := make([]string, len(w.ix.include))
	for i, name := range w.ix.include {
		f, _ := w.m.field(name)
		cols[i] = w.d.Quote(f.Column)
	}
	return " INCLUDE (" + strings.Join(cols, ", ") + ")"
}

// storage is " WITH (k = v, …)" of the index's storage parameters,
// sorted: the vector indexes' and With's.
func (w *indexWriter) storage() string {
	var with []string
	for k, v := range map[string]int{"m": w.ix.m, "ef_construction": w.ix.ef, "lists": w.ix.lists} {
		if v > 0 {
			with = append(with, k+" = "+strconv.Itoa(v))
		}
	}
	for _, p := range w.ix.with {
		v := fmt.Sprint(p.value)
		if s, ok := p.value.(string); ok {
			v, _ = sqlLiteral(w.d, s)
		}
		with = append(with, p.key+" = "+v)
	}
	if len(with) == 0 {
		return ""
	}
	slices.Sort(with)
	return " WITH (" + strings.Join(with, ", ") + ")"
}

// firstKey is the first of ix's keys that has, and whether there is one.
func firstKey(ix Index, has func(IndexKey) bool) (IndexKey, bool) {
	i := slices.IndexFunc(ix.keys, has)
	if i < 0 {
		return IndexKey{}, false
	}
	return ix.keys[i], true
}

// nullsOption is the option placing the key's NULLs: NullsFirst, NullsLast.
func (k IndexKey) nullsOption() string { return "Nulls" + k.nulls[:1] + strings.ToLower(k.nulls[1:]) }

// methodLack is the lack of a database without ix's access method.
func methodLack(ix Index) lack {
	title := map[string]string{"gin": "GIN", "gist": "GiST", "hash": "hash", "brin": "BRIN", "spgist": "SP-GiST", "hnsw": "HNSW", "ivfflat": "IVFFlat"}[ix.method]
	option := map[string]string{"gin": "GinIndex", "gist": "GistIndex", "hnsw": "HnswIndex", "ivfflat": "IvfflatIndex"}[ix.method]
	return lack{title + " index", cmp.Or(option, "Using")}
}

// sqlLiteral is v written into a statement, as an index's condition has
// it: bools, numbers, strings and nil; false for other values.
func sqlLiteral(d Dialect, v any) (string, bool) {
	if dv, ok := v.(driver.Valuer); ok {
		var err error
		if v, err = dv.Value(); err != nil {
			return "", false
		}
	}
	if v == nil {
		return "NULL", true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return strings.ToUpper(strconv.FormatBool(rv.Bool())), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10), true
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64), true
	case reflect.String:
		s := rv.String()
		if d.Name() == "mysql" {
			s = strings.ReplaceAll(s, `\`, `\\`)
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'", true
	}
	return "", false
}

// postgresIndexes makes every index but one of key prefixes, and unique
// ones only as B-trees.
type postgresIndexes struct{}

func (postgresIndexes) lacks(ix Index) (lack, bool) {
	switch {
	case ix.unique && ix.fulltext:
		return lack{"unique full-text index", "Unique"}, true
	case ix.unique && ix.method != "" && ix.method != "btree":
		return lack{"unique " + methodLack(ix).feature, "Unique"}, true
	}
	if _, ok := firstKey(ix, func(k IndexKey) bool { return k.prefix > 0 }); ok {
		return lack{"key prefix", "Prefix"}, true
	}
	return lack{}, false
}

func (postgresIndexes) statement(w *indexWriter) (string, error) {
	ix := w.ix
	keys := ix.keys
	if ix.fulltext && !w.tsvectors() {
		keys = []IndexKey{{of: SearchVector(ix.fields()...).Config(cmp.Or(ix.config, "simple"))}}
	}
	list, err := w.keys(keys, func(k IndexKey, s string) string {
		if k.collate != "" {
			s += " COLLATE " + w.d.Quote(k.collate)
		}
		if ops := w.opsOf(k); ops != "" {
			s += " " + ops
		}
		if k.desc {
			s += " DESC"
		}
		if k.nulls != "" {
			s += " NULLS " + k.nulls
		}
		return s
	})
	if err != nil {
		return "", err
	}
	where, err := w.where()
	if err != nil {
		return "", err
	}
	using := ""
	switch {
	case ix.fulltext:
		using = " USING gin"
	case ix.method != "" && ix.method != "btree":
		using = " USING " + ix.method
	}
	return w.create("") + using + " (" + list + ")" + w.include() + w.storage() + where, nil
}

// tsvectors is whether the index's fields are tsvectors already: then a
// full-text index is of them as they are.
func (w *indexWriter) tsvectors() bool {
	return !slices.ContainsFunc(w.ix.fields(), func(name string) bool {
		f, ok := w.m.field(name)
		return !ok || f.Type != tsvectorType
	})
}

// opsOf is a key's operator class on Postgres: its own, else its index's
// (a trigram or vector index's).
func (w *indexWriter) opsOf(k IndexKey) string {
	switch {
	case k.ops != "":
		return k.ops
	case w.ix.trigram:
		return w.ix.method + "_trgm_ops"
	case w.ix.vector():
		return "vector_" + [...]string{L2: "l2", Cosine: "cosine", IP: "ip"}[max(w.ix.ops, L2)] + "_ops"
	}
	return ""
}

// mysqlIndexes makes B-tree, hash and FULLTEXT indexes: no partial or
// covering ones, no NULLS order or operator classes.
type mysqlIndexes struct{}

func (mysqlIndexes) lacks(ix Index) (lack, bool) {
	switch {
	case ix.method != "" && ix.method != "btree" && ix.method != "hash":
		return methodLack(ix), true
	case ix.where != nil:
		return lack{"partial index", "Where"}, true
	case len(ix.include) > 0:
		return lack{"covering index", "Include"}, true
	case len(ix.with) > 0:
		return lack{"storage parameters", "With"}, true
	case ix.unique && ix.fulltext:
		return lack{"unique full-text index", "Unique"}, true
	}
	if k, ok := firstKey(ix, func(k IndexKey) bool { return k.nulls != "" }); ok {
		return lack{"NULLS " + k.nulls + " order", k.nullsOption()}, true
	}
	if _, ok := firstKey(ix, func(k IndexKey) bool { return k.ops != "" }); ok {
		return lack{"operator class", "Ops"}, true
	}
	return lack{}, false
}

func (mysqlIndexes) statement(w *indexWriter) (string, error) {
	list, err := w.keys(w.ix.keys, func(k IndexKey, s string) string {
		if k.collate != "" { // a key part takes no COLLATE: an expression does
			s = "(" + s + " COLLATE " + k.collate + ")"
		}
		if k.prefix > 0 {
			s += "(" + strconv.Itoa(k.prefix) + ")"
		}
		if k.desc {
			s += " DESC"
		}
		return s
	})
	if err != nil {
		return "", err
	}
	kind := ""
	if w.ix.fulltext {
		kind = "FULLTEXT "
	}
	stmt := w.create(kind) + " (" + list + ")"
	if w.ix.method == "hash" {
		stmt += " USING HASH"
	}
	return stmt, nil
}

// sqliteIndexes makes B-tree indexes, partial ones too, and full-text
// ones as the model's FTS5 table (written elsewhere): NULLs ordered only
// as SQLite orders them.
type sqliteIndexes struct{}

func (sqliteIndexes) lacks(ix Index) (lack, bool) {
	switch {
	case ix.method != "" && ix.method != "btree":
		return methodLack(ix), true
	case ix.fulltext && ix.unique:
		return lack{"unique full-text index", "Unique"}, true
	case ix.fulltext && ix.where != nil:
		return lack{"partial full-text index", "Where"}, true
	case len(ix.include) > 0:
		return lack{"covering index", "Include"}, true
	case len(ix.with) > 0:
		return lack{"storage parameters", "With"}, true
	}
	// SQLite puts NULLs first ascending and last descending, and indexes
	// them no other way.
	if k, ok := firstKey(ix, func(k IndexKey) bool { return k.nulls == "FIRST" && k.desc || k.nulls == "LAST" && !k.desc }); ok {
		order := map[bool]string{true: "a descending", false: "an ascending"}[k.desc]
		return lack{"NULLS " + k.nulls + " on " + order + " key", k.nullsOption()}, true
	}
	if _, ok := firstKey(ix, func(k IndexKey) bool { return k.ops != "" }); ok {
		return lack{"operator class", "Ops"}, true
	}
	if _, ok := firstKey(ix, func(k IndexKey) bool { return k.prefix > 0 }); ok {
		return lack{"key prefix", "Prefix"}, true
	}
	return lack{}, false
}

func (sqliteIndexes) statement(w *indexWriter) (string, error) {
	if w.ix.fulltext {
		return "", nil
	}
	list, err := w.keys(w.ix.keys, func(k IndexKey, s string) string {
		if k.collate != "" {
			s += " COLLATE " + w.d.Quote(k.collate)
		}
		if k.desc {
			s += " DESC"
		}
		return s
	})
	if err != nil {
		return "", err
	}
	where, err := w.where()
	if err != nil {
		return "", err
	}
	return w.create("") + " (" + list + ")" + where, nil
}
