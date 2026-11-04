package orm

// Index is an index a model declares in its Indexes() method, Django's
// Meta.indexes, made and dropped by migrations on every database the
// model's table is on. It is what is indexed (its keys: fields, named as
// queries name them, expressions, or Keys of either), how (Unique,
// Where, Include, Using, …) and where (On, Else):
//
//	func (User) Indexes() []orm.Index {
//		return []orm.Index{
//			orm.IndexOf("email").Unique(),
//			orm.IndexOf(orm.Lower.Of(orm.F("email"))).Unique().Name("users_email_lower"),
//			orm.IndexOf("last_name", "first_name"),
//			orm.IndexOf(orm.Key("created_at").Desc()),
//			orm.IndexOf("email").Where(orm.Q{"active": true}).Else(orm.IndexOf("email")),
//			orm.GinIndex("bio").Trigram(),
//		}
//	}
//
// An index a database can't make as declared fails the model's
// registration, naming the database, what it lacks and the two ways out:
// On, to make it only where it can be, and Else, to make another index
// instead where it can't. No index is ever left out silently.
type Index struct {
	method   string // the access method: "" for a B-tree, gin, gist, hash, brin, spgist, hnsw, ivfflat
	fulltext bool   // FullTextIndex's: per database, see there
	keys     []IndexKey
	name     string
	unique   bool
	where    Cond
	include  []string
	with     []storageParam
	on       onDBs
	orElse   *Index
	raw      Dialects // RawIndex's statements
	config   string
	ops      Metric
	trigram  bool
	m, ef    int
	lists    int // HNSW's M and EfConstruction, IVFFlat's Lists
}

type storageParam struct {
	key   string
	value any
}

// IndexKey is a key of an index: a field (named as queries name it) or an
// expression, and how it is ordered and compared there.
//
//	orm.IndexOf(orm.Key("created_at").Desc(), "id")
type IndexKey struct {
	of      any // a field name or an Expr
	desc    bool
	nulls   string // FIRST, LAST
	collate string
	ops     string
	prefix  int
}

// Key is a field or an expression as an index's key, for its options.
func Key(fieldOrExpr any) IndexKey { return IndexKey{of: fieldOrExpr} }

// Desc orders the key descending.
func (k IndexKey) Desc() IndexKey { k.desc = true; return k }

// NullsFirst and NullsLast place the key's NULLs. Postgres orders them
// either way; SQLite only as it does by itself (first ascending, last
// descending), and MySQL neither.
func (k IndexKey) NullsFirst() IndexKey { k.nulls = "FIRST"; return k }
func (k IndexKey) NullsLast() IndexKey  { k.nulls = "LAST"; return k }

// Collate compares the key by a collation of the database's.
func (k IndexKey) Collate(name string) IndexKey { k.collate = name; return k }

// Ops is the key's operator class on Postgres (text_pattern_ops).
func (k IndexKey) Ops(class string) IndexKey { k.ops = class; return k }

// Prefix indexes the first n characters of a text key, on MySQL.
func (k IndexKey) Prefix(n int) IndexKey { k.prefix = n; return k }

// field is the key's field name, "" for an expression.
func (k IndexKey) field() string {
	s, _ := k.of.(string)
	return s
}

// IndexOf is a B-tree index of keys: field names, expressions (Exprs) or
// Keys.
func IndexOf(keys ...any) Index { return Index{keys: indexKeys(keys)} }

func indexKeys(of []any) []IndexKey {
	keys := make([]IndexKey, len(of))
	for i, o := range of {
		if k, ok := o.(IndexKey); ok {
			keys[i] = k
		} else {
			keys[i] = IndexKey{of: o}
		}
	}
	return keys
}

// GinIndex is a Postgres GIN index of fields or expressions: a
// SearchVector, or text fields with Trigram. Unless On or Else say
// otherwise it is made on Postgres alone.
func GinIndex(keys ...any) Index { return Index{method: "gin", keys: indexKeys(keys)} }

// GistIndex is a Postgres GiST index of fields or expressions, made on
// Postgres alone unless On or Else say otherwise.
func GistIndex(keys ...any) Index { return Index{method: "gist", keys: indexKeys(keys)} }

// FullTextIndex is the fields' full-text index on every database: a GIN
// index of their tsvector on Postgres (by Config, simple when not given),
// which the __search lookup of a field indexed alone uses; MySQL's
// FULLTEXT, which MATCH needs; on SQLite the model's FTS5 table.
func FullTextIndex(fields ...string) Index {
	ix := Index{fulltext: true}
	for _, f := range fields {
		ix.keys = append(ix.keys, IndexKey{of: f})
	}
	return ix
}

// HnswIndex is pgvector's HNSW index of a vector field, made on Postgres
// alone unless On or Else say otherwise.
func HnswIndex(field string) Index { return Index{method: "hnsw", keys: []IndexKey{{of: field}}} }

// IvfflatIndex is pgvector's IVFFlat index of a vector field, made on
// Postgres alone unless On or Else say otherwise.
func IvfflatIndex(field string) Index { return Index{method: "ivfflat", keys: []IndexKey{{of: field}}} }

// RawIndex is an index written by hand, one statement per database: a
// database left out gets none. Migrations track it as any declared index,
// and drop it by its name.
//
//	orm.RawIndex("users_email_prefix", orm.Dialects{orm.MySQL: "CREATE INDEX users_email_prefix ON users (email(191))"})
func RawIndex(name string, statements Dialects) Index { return Index{name: name, raw: statements} }

// Name names the index; <table>_<fields>_<kind> otherwise (idx for a
// B-tree). An index of expressions alone must be named.
func (i Index) Name(s string) Index { i.name = s; return i }

// Unique makes the index a unique one: no two rows have the same keys.
func (i Index) Unique() Index { i.unique = true; return i }

// Where makes the index a partial one, of the rows matching cond: the
// row's own columns, and values written into the statement (bools,
// numbers, strings, nil).
func (i Index) Where(cond Cond) Index { i.where = cond; return i }

// Include adds fields to the index's entries that aren't its keys (a
// covering index, on Postgres).
func (i Index) Include(fields ...string) Index { i.include = append(i.include, fields...); return i }

// With sets a storage parameter of the index on Postgres: With("fillfactor", 70).
func (i Index) With(key string, value any) Index {
	i.with = append(i.with, storageParam{key, value})
	return i
}

// Using is the index's access method: "hash" (Postgres, MySQL), "brin" or
// "spgist" (Postgres); a B-tree otherwise.
func (i Index) Using(method string) Index { i.method = method; return i }

// On makes the index only on the databases named ("postgres", "mysql",
// "sqlite"); with none, on no database: declared, and never made.
func (i Index) On(databases ...DialectName) Index {
	i.on = onDBs{databases, true}
	return i
}

// Else makes other instead of the index on the databases that can't make
// it. other is held to the same rule.
func (i Index) Else(other Index) Index { i.orElse = &other; return i }

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

// vector is whether the index is a pgvector one.
func (i Index) vector() bool { return i.method == "hnsw" || i.method == "ivfflat" }

// chain is the index and its fallbacks, Else's, in order.
func (i Index) chain() []Index {
	out := []Index{i}
	for i.orElse != nil {
		i = *i.orElse
		out = append(out, i)
	}
	return out
}

// fields is the index's keys that are field names.
func (i Index) fields() []string {
	var out []string
	for _, k := range i.keys {
		if f := k.field(); f != "" {
			out = append(out, f)
		}
	}
	return out
}
