package orm

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// AnyManager is a manager of any model, as CreateTables and ormtest take
// it.
type AnyManager interface {
	tables() ([]schema.Table, error)
	madeOn(d Dialect) error
	conn(ctx context.Context, write bool) (conn, error)
	writeDB(s *Databases) string
	database() string
	declaration() declaredModel
	Err() error
}

// CreateTables creates the models' tables, and the tables between their
// many-to-many relations, where they are not already, and the extensions
// CreateExtension declared for the models' databases: for tests and
// tools. Foreign keys are made between the tables it makes only. A
// deployed schema changes through migrations (orm.Migrate).
func CreateTables(ctx context.Context, models ...AnyManager) error {
	if len(models) == 0 {
		return nil
	}
	c, err := models[0].conn(ctx, true)
	if err != nil {
		return err
	}
	name, err := c.src.schemaDialect(ctx, c.run)
	if err != nil {
		return err
	}
	d := schema.For(name)
	for _, e := range declaredExtensions() {
		if !slices.ContainsFunc(models, func(m AnyManager) bool { return m.database() == e.db }) {
			continue
		}
		for _, s := range d.CreateExtension(e.name) {
			if _, err := c.exec(ctx, s, nil); err != nil {
				return err
			}
		}
	}
	var all []schema.Table
	seen := map[string]bool{}
	for _, m := range models {
		if err := m.madeOn(c.d); err != nil {
			return err
		}
		ts, err := m.tables()
		if err != nil {
			return err
		}
		for _, t := range ts {
			if !seen[t.Name] {
				seen[t.Name] = true
				all = append(all, t)
			}
		}
	}
	// A foreign key to a table the call doesn't make is left out: it may
	// not exist, and these tables are for tests and tools.
	for i := range all {
		all[i].Columns = slices.Clone(all[i].Columns)
		for j, col := range all[i].Columns {
			if col.FK != nil && !seen[col.FK.Table] {
				all[i].Columns[j].FK = nil
			}
		}
	}
	for _, t := range schema.Sort(all) {
		for _, stmt := range d.Create(t, true) {
			for _, s := range splitSQL(stmt, c.d.Name()) {
				if _, err := c.exec(ctx, s, nil); err != nil {
					return fmt.Errorf("orm: creating %s: %w", t.Name, err)
				}
			}
		}
	}
	return nil
}

func (m *managerCore) database() string { return m.dbName }

func (m *managerCore) tables() ([]schema.Table, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.meta.tables()
}

func (m *managerCore) madeOn(d Dialect) error {
	if m.err != nil {
		return m.err
	}
	return m.meta.madeOn(d)
}

// tables is the model's table, its generated columns and indexes written
// for each dialect that can make them (madeOn says what one can't), and
// the tables between its many-to-many relations.
func (m *model) tables() ([]schema.Table, error) {
	t := schema.Table{Name: m.Table}
	together := map[string][]string{} // a unique index's columns, by its name
	for _, f := range m.Fields {
		if f.UniqueIdx != "" {
			together[f.UniqueIdx] = append(together[f.UniqueIdx], f.Column)
		}
	}
	for _, f := range m.Fields {
		c := columnOf(f)
		if f != m.PK {
			c.Auto = false
		}
		if len(together[f.UniqueIdx]) > 1 {
			c.Unique = false
		}
		switch f.Type {
		case vectorType:
			c.Vector = cmp.Or(f.Dims, -1)
		case tsvectorType:
			c.TSVector = true
		}
		if f.Gen != nil {
			c.Nullable = true
			for _, name := range schema.Names {
				b := newBuilder(DialectFor(name), m)
				b.ddl = true
				s, err := f.Gen.exprSQL(b)
				if len(*b.joins) > 0 || len(b.args()) > 0 {
					return nil, fmt.Errorf("orm: %s: a generated column reads the row's own columns, and takes no arguments", m.Name)
				}
				if err == nil {
					c.Generated = c.Generated.Set(name, s)
				}
			}
		}
		t.Columns = append(t.Columns, c)
	}
	names := slices.Sorted(maps.Keys(together))
	for _, name := range names {
		if cols := together[name]; len(cols) > 1 {
			t.Constraints = append(t.Constraints, schema.Constraint{Name: name, Columns: cols})
		}
	}
	for n, ix := range m.indexes {
		si := schema.Index{}
		for _, name := range schema.Names {
			idx, stmt, err := m.index(DialectFor(name), ix, n)
			if err == nil {
				si.Name, si.SQL = idx, si.SQL.Set(name, stmt)
			}
		}
		if si.Name != "" {
			t.Indexes = append(t.Indexes, si)
		}
	}
	if ft := m.fts(); ft != nil {
		t.Indexes = append(t.Indexes, ft.index())
	}
	out := []schema.Table{t}
	for _, r := range m.relList {
		if r.Kind != relFK && r.Kind != relM2M {
			continue
		}
		target, rf, err := r.ends()
		if err != nil {
			return nil, err
		}
		switch r.Kind {
		case relFK:
			c, _ := out[0].Column(r.local.Column)
			if r.crossDB() {
				c.Index = c.Index || !c.Unique && !c.PK
			} else {
				c.FK = &schema.ForeignKey{Table: target.Table, Ref: rf.Column, OnDelete: r.OnDelete}
			}
			if r.self != nil { // an orm.FK: the column is of the key it holds
				ref := columnOf(rf)
				c.Kind, c.Size, c.Unsigned, c.Collation = ref.Kind, ref.Size, ref.Unsigned, ref.Collation
				if c.Type == "" {
					c.Type, c.Types = ref.Type, ref.Types
				}
			}
		case relM2M:
			local, remote := columnOf(r.local), columnOf(rf)
			local.Name, remote.Name = r.ThroughLocal, r.ThroughRemote
			local.Auto, remote.Auto = false, false
			local.Unique, remote.Unique = false, false
			local.PK, remote.PK = false, false
			local.FK = &schema.ForeignKey{Table: m.Table, Ref: r.local.Column, OnDelete: "cascade"}
			remote.FK = &schema.ForeignKey{Table: target.Table, Ref: rf.Column, OnDelete: "cascade"}
			if r.crossDB() {
				remote.FK, remote.Index = nil, true
			}
			out = append(out, schema.Table{Name: r.Through, Columns: []schema.Column{local, remote}})
		}
	}
	return out, nil
}

// madeOn is what keeps d from making the model: an index kind or a
// generated expression d has none of.
func (m *model) madeOn(d Dialect) error {
	for _, f := range m.Fields {
		if f.Gen != nil {
			b := newBuilder(d, m)
			b.ddl = true
			if _, err := f.Gen.exprSQL(b); err != nil {
				return fmt.Errorf("orm: %s.%s: %w", m.Name, f.Name, err)
			}
		}
	}
	for n, ix := range m.indexes {
		if _, _, err := m.index(d, ix, n); err != nil {
			return err
		}
	}
	return nil
}

// Index is an index a model declares in its Indexes() method, Django's
// Meta.indexes, made and dropped by migrations. It is over fields, named
// as queries name them (their columns under the schema's names), or
// expressions:
//
//	func (Post) Indexes() []orm.Index {
//		return []orm.Index{
//			orm.GinIndex(orm.SearchVector("title", "body").Config("english")).Name("posts_search"),
//			orm.GinIndex("title").Trigram(),
//			orm.HnswIndex("embedding").Ops(orm.Cosine).M(16).EfConstruction(64),
//			orm.FullTextIndex("title", "body"),
//		}
//	}
//
// MySQL makes a FullTextIndex, and no trigram or vector index (its
// trigram lookups are LIKE, and it searches no vectors); a model
// declaring another kind can't map onto it. SQLite makes none of them: a model declaring a
// document (a FullTextIndex, a GinIndex of a SearchVector, a generated
// TSVector) has its FTS5 table instead, <table>_fts, and its vector and
// trigram searches are unindexed.
type Index struct {
	kind         string
	on           []any // field names and Exprs
	name, config string
	ops          Metric
	trigram      bool
	m, ef, lists int
}

// GinIndex is a Postgres GIN index of fields or expressions: a
// SearchVector, or text fields with Trigram.
func GinIndex(on ...any) Index { return Index{kind: "gin", on: on} }

// GistIndex is a Postgres GiST index of fields or expressions.
func GistIndex(on ...any) Index { return Index{kind: "gist", on: on} }

// FullTextIndex is the fields' full-text index: MySQL's FULLTEXT, which
// MATCH needs, and on Postgres a GIN index of their tsvector (by Config,
// simple when not given), which the __search lookup of a field indexed
// alone uses.
func FullTextIndex(fields ...string) Index {
	ix := Index{kind: "fulltext"}
	for _, f := range fields {
		ix.on = append(ix.on, f)
	}
	return ix
}

// HnswIndex is pgvector's HNSW index of a vector field.
func HnswIndex(field string) Index { return Index{kind: "hnsw", on: []any{field}} }

// IvfflatIndex is pgvector's IVFFlat index of a vector field.
func IvfflatIndex(field string) Index { return Index{kind: "ivfflat", on: []any{field}} }

// Name names the index; <table>_<fields>_<kind> otherwise.
func (i Index) Name(s string) Index { i.name = s; return i }

// Ops is the metric a vector index serves (L2 when not given): only
// queries by that metric's distance use it.
func (i Index) Ops(m Metric) Index { i.ops = m; return i }

// M is an HNSW index's connections per layer; EfConstruction its
// candidate list while building; Lists an IVFFlat index's lists.
func (i Index) M(n int) Index              { i.m = n; return i }
func (i Index) EfConstruction(n int) Index { i.ef = n; return i }
func (i Index) Lists(n int) Index          { i.lists = n; return i }

// Trigram indexes text fields by pg_trgm's trigrams (gin_trgm_ops,
// gist_trgm_ops), for trigram_similar, Similarity and LIKE.
func (i Index) Trigram() Index { i.trigram = true; return i }

// Config is a FullTextIndex's text search configuration on Postgres.
func (i Index) Config(c string) Index { i.config = c; return i }

// index is the model's n-th declared index on dialect d: its name, and
// the statement making it (none on SQLite).
func (m *model) index(d Dialect, ix Index, n int) (name, stmt string, err error) {
	b := newBuilder(d, m)
	b.ddl = true
	var fields, names []string
	for _, o := range ix.on {
		if f, ok := o.(string); ok {
			fields, names = append(fields, f), append(names, strings.ReplaceAll(f, "__", "_"))
		}
	}
	on := ix.on
	// TSVector fields are tsvectors already: indexed as they are.
	tsv := !slices.ContainsFunc(fields, func(f string) bool {
		h, ok := m.field(f)
		return !ok || h.Type != tsvectorType
	})
	if ix.kind == "fulltext" && d.Name() == "postgres" && !tsv {
		on = []any{SearchVector(fields...).Config(cmp.Or(ix.config, "simple"))}
	}
	var parts []string
	for _, o := range on {
		var s string
		var err error
		switch v := o.(type) {
		case string:
			s, err = b.ref(v)
		case Expr:
			s, err = v.exprSQL(b)
			s = "(" + s + ")"
		default:
			err = fmt.Errorf("an index is of field names and Exprs, not %T", o)
		}
		if err != nil {
			return "", "", fmt.Errorf("orm: %s's index: %w", m.Name, err)
		}
		switch {
		case ix.trigram:
			s += " " + ix.kind + "_trgm_ops"
		case ix.kind == "hnsw" || ix.kind == "ivfflat":
			s += " vector_" + [...]string{L2: "l2", Cosine: "cosine", IP: "ip"}[max(ix.ops, L2)] + "_ops"
		}
		parts = append(parts, s)
	}
	if len(*b.joins) > 0 || len(b.args()) > 0 {
		return "", "", fmt.Errorf("orm: %s's index: an index is of the table's own columns, and takes no arguments", m.Name)
	}
	if len(names) == 0 {
		names = []string{strconv.Itoa(n)}
	}
	name = cmp.Or(ix.name, m.Table+"_"+strings.Join(names, "_")+"_"+ix.kind)
	q := d.Quote
	switch {
	case d.Name() == "sqlite", d.Name() == "mysql" && (ix.trigram || ix.kind == "hnsw" || ix.kind == "ivfflat"):
		return name, "", nil
	case d.Name() == "mysql" && ix.kind != "fulltext":
		return "", "", fmt.Errorf("orm: %s: MySQL has no %s index", m.Name, ix.kind)
	case d.Name() == "mysql":
		return name, "CREATE FULLTEXT INDEX " + q(name) + " ON " + q(m.Table) + " (" + strings.Join(parts, ", ") + ")", nil
	}
	using := cmp.Or(map[string]string{"fulltext": "gin"}[ix.kind], ix.kind)
	stmt = "CREATE INDEX " + q(name) + " ON " + q(m.Table) + " USING " + using + " (" + strings.Join(parts, ", ") + ")"
	var with []string
	for k, v := range map[string]int{"m": ix.m, "ef_construction": ix.ef, "lists": ix.lists} {
		if v > 0 {
			with = append(with, k+" = "+strconv.Itoa(v))
		}
	}
	if len(with) > 0 {
		slices.Sort(with)
		stmt += " WITH (" + strings.Join(with, ", ") + ")"
	}
	return name, stmt, nil
}

// extensions is the Postgres extensions the model's columns and indexes
// need.
func (m *model) extensions() []string {
	var out []string
	need := func(e string) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	for _, f := range m.Fields {
		if f.Type == vectorType {
			need("vector")
		}
	}
	for _, ix := range m.indexes {
		switch {
		case ix.kind == "hnsw" || ix.kind == "ivfflat":
			need("vector")
		case ix.trigram:
			need("pg_trgm")
		}
	}
	return out
}

// columnOf is a field as a column: its kind from its Go type, NULL when
// it is a pointer.
func columnOf(f *field) schema.Column {
	t := f.Type
	c := schema.Column{Name: f.Column, PK: f.PK, Unique: f.Unique, Index: f.Indexed, Size: f.Size, Type: f.SQLType, Default: f.Default}
	if t.Kind() == reflect.Pointer {
		c.Nullable, t = true, t.Elem()
	}
	if isOption(t) {
		c.Nullable, t = true, optionElem(t)
	}
	c.Nullable = c.Nullable || f.Null
	c.Kind = kindOf(t)
	// A nil []byte, Vector, JSON or CSV is NULL, as the driver sends it.
	if c.Kind == schema.Bytes || c.Kind == schema.JSON || t == vectorType || t.Implements(csvColType) {
		c.Nullable = true
	}
	counted := true
	if s := f.spec; s != nil {
		c.Comment, c.Collation, c.Precision, c.Unsigned = s.comment, s.collation, s.precision, s.unsigned
		switch {
		case s.date:
			c.Kind = schema.Date
		case s.uuid:
			c.Kind = schema.UUID
		}
		if c.Kind == schema.Int || c.Kind == schema.Int32 || c.Kind == schema.Int16 {
			switch s.size {
			case Small:
				c.Kind = schema.Int16
			case Medium:
				c.Kind = schema.Int32
			case Big:
				c.Kind = schema.Int
			}
		}
		counted = !s.manual && !s.uuid
	}
	if implements(valueType(t), columnTyperType) {
		if ct, ok := methodsOf[ColumnTyper](reflect.New(valueType(t)).Elem()); ok {
			for _, name := range schema.Names {
				c.Types = c.Types.Set(name, ct.ColumnType(DialectFor(name)))
			}
		}
	}
	if c.PK {
		c.Nullable = false
		c.Auto = counted && (c.Kind == schema.Int || c.Kind == schema.Int32 || c.Kind == schema.Int16)
	}
	return c
}

var (
	scannerT = reflect.TypeFor[sql.Scanner]()
	valuerT  = reflect.TypeFor[driver.Valuer]()
)

func kindOf(t reflect.Type) schema.Kind {
	switch {
	case t == timeType:
		return schema.Time
	case t.Implements(jsonColType):
		return schema.JSON
	case reflect.PointerTo(t).Implements(scannerT) || t.Implements(valuerT):
		return schema.Custom
	}
	switch t.Kind() {
	case reflect.Bool:
		return schema.Bool
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return schema.Int
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint8, reflect.Uint16:
		return schema.Int32
	case reflect.Float64:
		return schema.Float
	case reflect.Float32:
		return schema.Float32
	case reflect.String:
		return schema.String
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return schema.Bytes
		}
	}
	return schema.Custom
}
