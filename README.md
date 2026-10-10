# orm

A Django-style ORM for Go: a model is a plain struct, its manager a
package-level value, its queries lazy, immutable QuerySets read with
Django's lookups (`"age__gte"`, `"roles__name"`). Relations and
prefetching, aggregates with `GroupBy`/`Having`, `Case`/`When`, subqueries,
text and vector search, raw SQL, schema-per-tenant names sets, mirrored
dual writes, and Go migration files written by `makemigrations`.

It was extracted from the [nexus](https://github.com/paulmanoni/nexus)
framework's ORM (`github.com/paulmanoni/nexus/orm`). The engine is the
same; it has no dependency on nexus. Connections are plain `database/sql`.

```
go get github.com/vuka-lang/orm
```

It needs Go 1.27 (it uses generic methods: `qs.Values[R](…)`).

## Quick start

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/vuka-lang/orm"
	_ "github.com/vuka-lang/orm/postgres" // the driver this program opens
)

type User struct {
	orm.Model[User]
	ID        int64
	Name      string
	Email     string `orm:"unique"`
	Age       int
	CreatedAt time.Time
	Posts     []Post `orm:"rel:author_id"`
}

type Post struct {
	orm.Model[Post]
	ID       int64
	Title    string
	AuthorID int64
	Author   *User
	Tags     []Tag `gorm:"many2many:post_tags"` // GORM's relation tags are read too
}

type Tag struct {
	orm.Model[Tag]
	ID   int64
	Name string
}

var (
	Users = orm.For[User]()
	Posts = orm.For[Post]()
)

func main() {
	ctx := context.Background()
	db, err := orm.Connect("postgres", "postgres://localhost:5432/shop?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	orm.AddDatabase("main", db, orm.AsDefault())
	if err := orm.Check(); err != nil { // models the ORM can't map
		log.Fatal(err)
	}
	if err := orm.Migrate(ctx); err != nil { // the registered migrations
		log.Fatal(err)
	}

	u := User{Name: "Ali", Email: "ali@example.com", Age: 30}
	if err := Users.Create(ctx, &u); err != nil {
		log.Fatal(err)
	}
	adults, err := Users.Filter(orm.Q{"age__gte": 18}).OrderBy("-age").All(ctx)
	ali, err := Users.Get(ctx, orm.Q{"email": "ali@example.com"})
	posts, err := Posts.Filter(orm.Q{"tags__name": "go"}).
		SelectRelated("author").PrefetchRelated("tags").All(ctx)
	n, err := Users.Filter(orm.Or(orm.Q{"name__istartswith": "a"}, orm.Not(orm.Q{"age__lt": 18}))).Count(ctx)

	ali.Age++
	err = ali.Save(ctx) // a model embedding orm.Model saves itself
	err = orm.Atomic(ctx, func(ctx context.Context) error {
		_, err := Users.Filter(orm.Q{"age__lt": 18}).Delete(ctx)
		return err
	})
	_, _, _, _ = adults, posts, n, err
}
```

`orm.WithDB(ctx, db)` sends a context's queries to one database (tests,
tools); `orm.Using(ctx, "name")` to another registered one. A model names
its database with `orm.On("name")` or `Meta.DB`.

## Databases

`orm.Default` is the program's `*orm.Databases`: `orm.AddDatabase(name, db,
opts…)` fills it, and the database marked `orm.AsDefault()` (or the only
one) is the default. A process serving several apps gives each its own set
(`orm.NewDatabases()`) and puts it on each request's context with
`orm.WithDatabases(ctx, set)`.

A table can be routed without a code change, over its models' own
databases — the way to move a table, writing both while reading one:

```go
orm.Default.Route("categories", orm.Route{DB: "legacy", Mirror: "main"})
```

## Drivers

Drivers are opt-in blank imports, `database/sql` style; the `orm` package
links none.

| Import                          | Driver                                   | `Connect` dialect |
|---------------------------------|------------------------------------------|-------------------|
| `github.com/vuka-lang/orm/postgres` | pgx (`github.com/jackc/pgx/v5/stdlib`)   | `postgres`        |
| `github.com/vuka-lang/orm/mysql`    | `github.com/go-sql-driver/mysql`         | `mysql` (parseTime forced on) |
| `github.com/vuka-lang/orm/sqlite`   | pure-Go `github.com/glebarez/go-sqlite`  | `sqlite`          |

The SQLite package also registers `l2_distance`, `cosine_distance`,
`inner_product` and trigram `similarity` in Go, so vector and trigram
searches run (unindexed) on SQLite in development and tests; its text
search is FTS5 (see [Search](#search)).
`orm.ConnectURL("postgres://…" | "mysql://user:pass@tcp(host)/db" |
"sqlite:file.db")` picks the dialect from the URL. `orm.Open(sqlDB,
dialect)` wraps a `*sql.DB` you opened yourself.

## Errors

The ORM's errors are `*orm.Error` values with a `Kind`; `errors.Is` matches
a kind:

```go
if errors.Is(err, orm.ErrNotFound) { … }
var e *orm.Error
if errors.As(err, &e) && e.Kind == orm.Conflict { log.Print(e.Fields) } // map[email:[already exists]]
```

| Kind            | When                                                          |
|-----------------|---------------------------------------------------------------|
| `NotFound`      | `Get`, `First`, `Refresh`, `Remove` found no row              |
| `MultipleFound` | `Get` matched several rows                                    |
| `Conflict`      | a write broke a unique constraint (`Fields`: the column)      |
| `Invalid`       | a write referred to a row that doesn't exist (a foreign key)  |
| `Unavailable`   | the database didn't answer within `MigrateWait` (Migrate, Verify) |

`orm.KindOf(err)` is 0 for an error the ORM gives no meaning (a driver's, a
bad query). A framework maps the kinds onto its own model; for nexus:

| orm Kind        | nexus code      | HTTP |
|-----------------|-----------------|------|
| `NotFound`      | `NotFound`      | 404  |
| `MultipleFound` | `Conflict`      | 409  |
| `Conflict`      | `Conflict` (+ `Fields`) | 409 |
| `Invalid`       | `InvalidInput` (+ `Fields`) | 422 |
| `Unavailable`   | `Unavailable`   | 503  |
| none            | `Internal`      | 500  |

## Observing queries

Every statement passes through observers — the hook for logging, tracing,
metrics, a debug toolbar or counting queries in tests:

```go
stop := orm.Observe(func(ctx context.Context, q orm.QueryInfo) {
	slog.DebugContext(ctx, "sql", "sql", q.SQL, "args", len(q.Args), "dur", q.Duration, "rows", q.Rows, "err", q.Err)
})
defer stop()
ctx = orm.WithObserver(ctx, fn) // one context's statements only
```

`QueryInfo` carries the SQL, its arguments, start time, duration, rows
affected (an exec's; -1 for a query), the error, and `Refused`: a query
the ORM refused before sending (`Err` says why) is reported too, as a
statement that never ran. `orm.WatchRepeats(ctx, n, warn)` calls `warn`
once a context runs the same statement `n` times — an N+1 that
`SelectRelated` or `PrefetchRelated` would read in one query.

## Migrations and the `orm` command

Migrations are Go files in the program's `migrations` package
(`migrations/<db>` for a database other than the default), each
registering its operations from `init`. The schema a database has is the
replay of its migrations; `makemigrations` compares the replay with the
models the program declares. Import the package so `orm.Migrate` (and the
commands) apply them:

```go
import _ "example.com/shop/migrations"
```

```
go run github.com/vuka-lang/orm/cmd/orm generate [--tests] [--check] [dir]
go run github.com/vuka-lang/orm/cmd/orm makemigrations [name] [--check] [--dry-run] [--empty] [--noinput] [--db name]
go run github.com/vuka-lang/orm/cmd/orm migrate [target|zero] [--db name]
go run github.com/vuka-lang/orm/cmd/orm showmigrations
go run github.com/vuka-lang/orm/cmd/orm sqlmigrate <name> [--backwards] [--db name]
```

(`go get -tool github.com/vuka-lang/orm/cmd/orm` then `go tool orm …`.)

- `generate` writes `orm_scanners_gen.go` per package: an
  `orm.Register[T]()` for every type embedding `orm.Model[T]` and row
  scanners/writers that skip reflection. Without the files the ORM reads
  rows by reflection. `ormgen.Write` / `ormgen.Generate` are the library
  form; `go:generate go run github.com/vuka-lang/orm/cmd/ormgen ./...`
  works too.
- The migration commands build the program's main package (`--main` when
  the module has several) with an init overlaid that serves
  `orm.ServeCLI` before `main` runs, the migrations packages imported and
  the generated code overlaid where it isn't on disk; nothing is written
  to the tree but the migration file.
- They connect by URL: `--database name=URL` (repeatable; `default=` for
  the default), else `DATABASE_URL` and `DATABASE_URL_<NAME>`.
  `--default main` names the default database as the models' `orm.On`
  calls name it. The program links the drivers it opens.
- On a terminal `makemigrations` asks about renames ("Did you rename
  users.mail to users.email? [y/N]"); `--noinput` writes a removal and an
  addition with a note.

At run time `orm.Migrate(ctx, orm.MigrateOn("main"), orm.MigrateLogger(l))`
applies pending migrations under a lock (Postgres advisory lock, MySQL
`GET_LOCK`), recording them in `orm.MigrationsTable` (`orm_migrations`; a
program coming from nexus sets `nexus_migrations`).
`orm.ApplyMigrations(ctx, db, target, migs…)` runs a given list (tools,
tests). `orm.CreateExtension("vector")` declares a Postgres extension for
the next migration; `schema.Verify(ctx, models…)` checks models against a
database at start up.

## Testing

`github.com/vuka-lang/orm/ormtest` opens a fresh database per test with the
models' tables made — in-memory SQLite by default, or a schema/database of
its own on a real server with `ORMTEST_DRIVER=postgres|mysql` and
`ORMTEST_DSN`:

```go
ctx := ormtest.Open(t, Users, Posts)
ormtest.Seed(t, ctx, Users, &User{Name: "Ali"})
ctx, queries := ormtest.CountQueries(ctx)
```

## Search

Text search (Django's `contrib.postgres.search`) and vector search
(pgvector's), each written for the database the query runs on:

|                | Postgres                                   | MySQL                          | SQLite                                        |
|----------------|--------------------------------------------|--------------------------------|-----------------------------------------------|
| text           | `tsvector`, `websearch_to_tsquery`, GIN    | `MATCH … AGAINST`, `FULLTEXT`  | FTS5 table `<table>_fts`, kept by triggers    |
| rank           | `ts_rank`                                  | relevance                      | `-bm25(…)`, the document's weights per column |
| headline       | `ts_headline`                              | substring about the first word | `snippet()` / `highlight()`                   |
| trigrams       | `pg_trgm` (`%`, `similarity`, GIN/GiST)    | `LIKE`                         | `similarity` in Go, unindexed                 |
| vectors        | pgvector (`<->`, `<=>`, `<#>`, HNSW/IVFFlat) | none                         | distances in Go, unindexed (O(n))             |

### Declaring a document

A document is fields searched together. Declare it as a generated
`TSVector` column, a `FullTextIndex`, or a GIN index of a `SearchVector`;
each database makes what it can of the declaration:

```go
type Post struct {
	orm.Model[Post]
	ID        int64
	Title     string
	Body      string
	Search    orm.TSVector `orm:"generated"`
	Embedding orm.Vector   `orm:"vector:384"`
}

func (Post) Generated() map[string]orm.Expr {
	return map[string]orm.Expr{"Search": orm.SearchVector("title").Weight("A").
		Add(orm.SearchVector("body").Weight("B")).Config("english")}
}

func (Post) Indexes() []orm.Index {
	return []orm.Index{
		orm.GinIndex("search"),                                     // Postgres
		orm.FullTextIndex("title", "body"),                         // MySQL's FULLTEXT; GIN on Postgres
		orm.HnswIndex("embedding").Ops(orm.Cosine).M(16).EfConstruction(64),
		orm.GinIndex("title").Trigram(),
	}
}
```

On SQLite the declaration becomes an external-content FTS5 table,
`posts_fts` (`content=posts`, `content_rowid=id`), over the document's
fields, with `AFTER INSERT/UPDATE/DELETE` triggers keeping it in step and a
`rebuild` indexing the rows already there. Migrations make and drop it as
an index (`m.Index{Name: "posts_fts", SQL: …, Drop: …}`, SQLite's only), so
`makemigrations` adds it, unapplying removes the table and its triggers,
and a table SQLite rebuilds keeps it. An `english` config stems by FTS5's
`porter` tokenizer; others by `unicode61`. A model with no document, or
whose key isn't an integer, is searched by `LIKE` on SQLite, unindexed.

### Text queries

`orm.SearchQuery` is web search syntax: every word must match, `"a phrase"`
in order, `or` between alternatives, `-word` must not, and `word*` matches
words starting with it.

```go
doc, q := orm.SearchVector("title", "body").Config("english"), orm.SearchQuery(`go -java "web server" or rust*`)
Posts.Filter(orm.Match(doc, q)).
	AnnotateAs(orm.SearchRank(doc, q).As("rank"),
		orm.Headline("body", q, orm.Marks("<mark>", "</mark>"), orm.MaxWords(20)).As("head")).
	OrderBy("-rank")
Posts.Filter(orm.Q{"title__search": "go generics"})                 // the document holding title
Posts.Filter(orm.SearchRank(doc, q).Gt(0.1))                        // typed: OrderedExpr[float64]
```

The text is always a bound argument: on SQLite it is rewritten into an FTS5
query of quoted phrases (`("go" AND "web server") NOT ("java")`), never
FTS5 syntax of the user's. Where the databases differ:

- **Stopwords.** Postgres's `english` drops its stopwords from queries;
  SQLite drops the same list from an `english` document's queries. MySQL
  drops InnoDB's, and words shorter than `innodb_ft_min_token_size`.
- **Prefixes.** Postgres has no `word*` in `websearch_to_tsquery`: a query
  with one is built of `plainto_tsquery` / `phraseto_tsquery` /
  `to_tsquery('word':*)` joined by `&&`, `||`, `!!`.
- **Exclusions only.** `-java` alone matches the rows without it on
  Postgres and SQLite (a `NOT IN` of the FTS5 match); MySQL's boolean mode
  matches nothing.
- **Ranking.** `ts_rank` weighs A 1, B 0.4, C 0.2, D and unweighted 0.1;
  SQLite passes the same weights to `bm25` (0 for columns outside the
  document), but bm25 also normalises by length: a match in a short text
  ranks above one in a long text that `ts_rank` scores alike.
- **Headlines.** `Marks`, `MaxWords` (at most 64 on SQLite) and
  `HighlightAll` map onto `ts_headline`'s `StartSel`/`StopSel`/`MaxWords`/
  `HighlightAll` and onto `snippet`/`highlight`. A row without the words
  gets its first 160 characters about the first word, unmarked, on SQLite;
  MySQL always gets that substring.

### Vector search

`orm.L2Distance`, `CosineDistance` and `InnerProduct` (negated, as `<#>`:
smaller is nearer) are `OrderedExpr[float64]`, so they filter and order:

```go
Posts.Filter(orm.CosineDistance("embedding", v).Lt(0.3)).Order(orm.CosineDistance("embedding", v).Asc())
```

`Nearest` is the k-nearest-neighbour query: ordered by distance, nearest
first, annotated as `distance`:

```go
Posts.Nearest("embedding", v, orm.K(10)).All(ctx)
Posts.Nearest("embedding", v, orm.K(10), orm.MaxDistance(0.4), orm.DistanceAs("d"), orm.EfSearch(100)).
	Values[Hit]("id", "title", "d")
```

The metric is the field's vector index's (`Ops`), `L2` without one, or an
option (`orm.Cosine`). On Postgres the `ORDER BY` is pgvector's operator,
so an HNSW or IVFFlat index of that metric serves it; `orm.EfSearch(n)`
(`hnsw.ef_search`) and `orm.Probes(n)` (`ivfflat.probes`) are set with
`SET LOCAL` in a transaction of the query's own, or a savepoint of the
context's, and end with it. SQLite computes the distance in Go for every
row. MySQL has no vector search: MySQL 9's `VECTOR` type has no distance
function outside HeatWave, so a model with a `Vector` field is refused
there rather than stored half-usable.

### Hybrid search

`Hybrid` ranks rows by a text match and by a vector distance and fuses the
ranks by reciprocal rank (a row scores `w/(k + rank)` per ranking, the
text's only where the text matches), annotated `rank`, `distance` and
`score`, best first:

```go
Posts.Hybrid(doc, orm.SearchQuery("web server"), "embedding", v,
	orm.K(10), orm.Candidates(100), orm.HybridWeights(1, 0.5), orm.RRF(60)).All(ctx)
```

Every row is ranked by distance unless `Candidates(n)` (the `n` nearest,
found by the vector index) or `MaxDistance` narrow them to those plus the
text's matches. It runs on Postgres and on SQLite (FTS5 and Go distances).
`orm.Fuse("-rank", "distance").K(20).Weights(1, 0.5)` is the fusion on its
own, over any annotations.

## Custom functions

### Typed functions

A database function is defined once, with its SQL per dialect, and called
like a Go function. Its call is an expression whose comparisons take the
function's result type, so a condition on it type-checks:

```go
var (
	EditDist = orm.OrderedFunc[int]("editdist", orm.Arity(2),
		orm.Template("postgres", "levenshtein({0}, {1})"), // fuzzystrmatch
		orm.Template("sqlite", "editdist({0}, {1})"))       // a Go function, below
	Unaccent = orm.TextFunc("unaccent", orm.Arity(1),
		orm.Template("postgres", "unaccent({0})"), orm.Template("", "{0}")).Transform()
)

Users.Filter(EditDist(orm.F("name"), "jon").Le(1)).
	Order(EditDist(orm.F("name"), "jon").Asc(), orm.Ordering{Path: []string{"age"}, Desc: true})
Users.AnnotateAs(EditDist(orm.F("name"), q).As("dist")).Filter(orm.Q{"dist__lt": 3}).Values[[]any]("name", "dist")
Users.Filter(Unaccent(orm.F("name")).IContains("jose"))   // or orm.Q{"name__unaccent__icontains": "jose"}
Users.Filter(…).Update(ctx, orm.Set{"name": orm.Upper(orm.F("name"))})
```

Arguments are `orm.Expr`s (`orm.F("field")` for a field, another call, a
`Case`) or plain values, which are always bound as parameters, never written
into the SQL. `{0}`, `{1}` stand for the arguments in a `Template`, `{*}`
for all of them; `""` is the template of every dialect not named. A function
with no `Template` is called as `name(args…)`; one with templates is written
only where one applies — elsewhere its query fails with `function editdist
has no SQL on mysql`. `orm.Arity(n)` checks the number of arguments.

Go can't give a generic type's methods to some of its type arguments, so the
constructor picks the comparisons:

| Constructor | Call's type | Comparisons |
|---|---|---|
| `orm.Func[R]` | `TypedExpr[R]` | `Eq`, `Ne`, `In`, `IsNull`, `NotNull`, `Asc`, `Desc`, `As`, `Over` |
| `orm.OrderedFunc[R]` (numbers, strings, `time.Time`) | `OrderedExpr[R]` | those, and `Lt`, `Le`, `Gt`, `Ge`, `Between` |
| `orm.TextFunc` | `TextExpr` | those, and `Contains`, `IContains`, `StartsWith`, `EndsWith`, `Like` |

`Contains` on a number doesn't compile. `orm.Typed[R](e)`, `orm.Ordered[R](e)`
and `orm.Text(e)` give any expression a type (`orm.Text(orm.F("title")).Like("Go%")`);
every typed expression is an `Expr`, so `Where(e, lookup, v)` still takes any
lookup, and comparisons go in `Filter`, `Exclude`, `Having` and `When`. The
built-ins are typed the same way and keep their `Of`: `orm.Lower`, `Upper`,
`Trim` (text), `Length` (int64), `Abs` (float64), `Year`, `Month`, `Day`
(int), `Date` (the date as `YYYY-MM-DD` text, which every database compares
with a date), `Coalesce` (any). `orm.Function(name, …)` remains the untyped
form (`.Of(args…)`), falling back to `name(args…)` on a dialect without a
template.

`QuerySet.Order(terms…)` sorts by expression terms (`e.Asc()`, `e.Desc()`,
`orm.Asc(e)`, `orm.Desc(e)`) and `orm.Ordering`s, mixed; `AnnotateAs` takes
expressions named by `As`.

### Aggregates and windows

`orm.IsAggregate()` (or `orm.Aggregate[R]`) marks an aggregate: an annotation
of it groups a `Values` listing it by the other names, a condition on it is
`HAVING`'s, and `orm.AggOf` hands it to `Aggregate`.

```go
var StringAgg = orm.TextFunc("string_agg", orm.IsAggregate(),
	orm.Template("postgres", "string_agg({0}, {1})"),
	orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')"),
	orm.Template("sqlite", "group_concat({0}, {1})"))

Users.Annotate("names", StringAgg(orm.F("name"), ", ")).
	Having(StringAgg(orm.F("name"), ", ").Contains("Ali")).Values[[]any]("active", "names")
Users.Aggregate(ctx, orm.AggOf("names", StringAgg(orm.F("name"), ", ")))
```

`.Over(orm.Window{PartitionBy: []string{"team_id"}, OrderBy: []string{"-score"}})`
makes a call a window function's (`row_number`, `rank`, or an aggregate over
a window, which then no longer groups).

### Lookups and transforms

`orm.RegisterLookup` adds a lookup to `Q` keys and `Where`, Django's
`register_lookup`. Its function writes the condition per dialect from the
left side's SQL and the value (`rhs()` binds it, once per call); `ok` false
means no SQL on that dialect, and the query says so:

```go
orm.RegisterLookup("unaccent_icontains", func(lhs string, rhs func() string, d orm.Dialect) (string, bool) {
	if d.Name() != "postgres" {
		return "", false
	}
	return "unaccent(" + lhs + ") ILIKE unaccent(" + rhs() + ") ESCAPE '!'", true
}, orm.LookupValue(orm.LikePattern("%", "%")))

Posts.Filter(orm.Q{"title__unaccent_icontains": "cafe"})
Posts.Filter(orm.Where(orm.Lower(orm.F("title")), "unaccent_icontains", "cafe"))
```

`orm.LookupValue` maps the value before it is bound (`orm.LikePattern`
escapes a literal into a `LIKE` pattern). A one-argument function made a
transform with `.Transform()` is a key step before any lookup, built-in or
registered: `Q{"title__unaccent__icontains": "cafe"}`,
`Q{"title__unaccent__unaccent_icontains": "cafe"}`. An unknown lookup or transform fails
with a suggestion: `unknown lookup "icontain" — did you mean "icontains"?`.

### Go functions on SQLite

`orm/sqlite`'s `Func` registers a Go function as an SQL function on every
SQLite connection opened after it — register in an `init`, before `Connect`:

```go
func init() {
	sqlite.MustFunc("editdist", func(a, b string) int { … })      // deterministic
	sqlite.MustFunc("now_ms", func() int64 { … }, sqlite.Volatile())
	sqlite.MustFunc("initial", func(s *string) *string { … })    // pointers: NULL in and out
}
```

Parameters and results are strings, `[]byte`, bools, numbers, `time.Time`,
`any` or pointers to them (a `driver.Valuer` result too), the last parameter
variadic or not, an `error` result after the value; a NULL passed to a
non-pointer parameter makes the call NULL without running it. They run
unindexed. The driver registers scalar functions only (no Go aggregates).

### Per dialect

Postgres and MySQL run functions written in SQL. Create them in a migration
with `migration.CreateFunction` (unapplying drops them; SQLite gets nothing
— use a Go function there), or `migration.RunSQL` for anything else:

```go
m.CreateFunction{
	Name:    "initials",
	Params:  m.Dialects{Postgres: "s text", MySQL: "s VARCHAR(255)"},
	Returns: m.Dialects{Postgres: "text", MySQL: "VARCHAR(255)"},
	Body:    m.Dialects{Postgres: "SELECT upper(left(s, 1))", MySQL: "RETURN UPPER(LEFT(s, 1))"},
}
```

It is `IMMUTABLE` / `DETERMINISTIC` unless `Volatile`; `Language` picks a
Postgres language (`plpgsql`) and a MySQL `BEGIN … END` body runs as one
statement. Extension functions (`levenshtein`, `unaccent`) come with
`m.CreateExtension{Name: "fuzzystrmatch"}`.

## Extension points

Two seams let a layer above the ORM (a language runtime, a schema file)
drive it without struct tags or the `Q` map syntax:

- **Predicates as data.** `orm.Pred{Path, Op, Value, Args}` — a field
  path, an operator (`OpEq`, `OpGte`, `OpIn`, `OpIContains`, …) and a
  value, or `OpAnd`/`OpOr`/`OpNot` of predicates — is a `Cond` that
  `Filter`, `Exclude` and `Get` take. A foreign type implements
  `orm.Predicate` (`Pred() orm.Pred`) and passes `orm.PredCond(p)`.
  `orm.Ordering{Path, Desc}` and `orm.Orderings(…)` are ordering's
  counterpart for `OrderBy`.
- **Field metadata from outside the struct.** `orm.SetFieldOptions(fn)`
  supplies `orm.FieldOptions` (column, max length, index, unique, unique
  together, SQL default, primary key, `on_delete`, a relation's inverse
  name or column) per field, over the tags. Set it before the first
  query.

## What was left in nexus

The nexus integration is not part of this module: binding managers as
`nexus.Option`s at boot, reading databases and table routes from
`nexus.toml`, the dashboard's model listing, trace spans for the debug
toolbar (an `orm.Observe` hook there), and `Manager.GraphRelation` (a
batched GraphQL field over `nexus.LoadField`). Each is an adapter over the
seams above.

## License

MIT
