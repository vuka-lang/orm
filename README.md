# orm

A Django-style ORM for Go: a model is a plain struct, its manager a
package-level value, its queries lazy, immutable QuerySets read with
Django's lookups (`"age__gte"`, `"roles__name"`). Relations and
prefetching, aggregates with `GroupBy`/`Having`, `Case`/`When`, subqueries,
text and vector search, raw SQL, schema-per-tenant names sets, mirrored
dual writes, and Go migration files written by `makemigrations`.

Connections are plain `database/sql`.

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

//go:generate go run github.com/vuka-lang/orm/cmd/ormgen

type User struct {
	orm.Model[User]
	ID        int64
	Name      string
	Email     string
	Age       int
	CreatedAt time.Time
}

func (u *User) Schema(s *orm.Schema) {
	s.Field(&u.Email, orm.Char{Max: 254, Unique: true})
	s.Field(&u.CreatedAt, orm.Datetime{AutoNowAdd: true})
}

type Post struct {
	orm.Model[Post]
	ID     int64
	Title  string
	Author orm.FK[User]
	Tags   orm.M2M[Tag]
}

func (p *Post) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Ordering: []string{"-id"}})
	s.Field(&p.Title, orm.Char{Max: 200})
	s.Field(&p.Author, orm.Rel{OnDelete: orm.Cascade, Related: "posts"})
}

type Tag struct {
	orm.Model[Tag]
	ID   int64
	Name string
}

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

	users, posts := orm.For[User](), orm.For[Post]()
	u := User{Name: "Ali", Email: "ali@example.com", Age: 30}
	if err := users.Create(ctx, &u); err != nil {
		log.Fatal(err)
	}
	adults, err := users.Filter(UserFields.Age.Gte(18)).OrderBy(UserFields.Age.Desc()).All(ctx)
	ali, err := users.Get(ctx, UserFields.Email.Eq("ali@example.com"))
	gophers, err := posts.Filter(PostFields.Tags.Name.Eq("go"), PostFields.Author.Email.IEndsWith("@example.com")).
		SelectRelated("author").PrefetchRelated(PostFields.Tags).All(ctx)
	n, err := users.Filter(orm.Or(UserFields.Name.IStartsWith("a"), orm.Not(UserFields.Age.Lt(18)))).Count(ctx)

	ali.Age++
	err = ali.Save(ctx) // a model embedding orm.Model saves itself
	err = orm.Transaction(ctx, func(ctx context.Context) error {
		_, err := users.Filter(UserFields.Age.Lt(18)).Delete(ctx)
		return err
	})
	_, _, _, _ = adults, gophers, n, err
}
```

### Schema

A model declares what its Go types don't say in a `Schema` method: model
attributes with `s.Model`, field attributes with `s.Field`, the field named
by its address. The ORM calls it once, on a zero value; a field with
nothing to say isn't listed, its column (the name in snake_case), NULL-ness
(a pointer or `vuka.Option`) and type coming from the Go field.

```go
func (o *Order) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "tbl_orders", Unmanaged: true}, mirror.Legacy{})
	s.Field(&o.ID, orm.PK{}, mirror.Column{Name: "order_id"})
	s.Field(&o.PlacedAt, orm.Datetime{Column: "placed_at", Null: true}, mirror.Omit{})
	s.Field(&o.Customer, orm.Rel{Column: "customer_id", Related: "orders"})
	s.Field(&o.Paid, orm.Column("is_paid"))
}
```

- The attributes are values: `orm.PK{}`, `orm.Char{Max: 50}`,
  `orm.Column("x")`, `orm.DefaultTo(0)`, … (the table in
  [Models](#models) lists them). Any value with `Apply(*orm.FieldOptions)`
  is a field attribute (`orm.FieldAttr`; a `Validate(v any) error` method
  adds a check of written values), any with `ApplyModel(*orm.ModelConfig)`
  a model attribute (`orm.ModelAttr`): a package of the app's own
  (`mirror.Column`, `mirror.Legacy` above) plugs in the same way.
- A field of an embedded struct is named through the model, `&a.Created`
  or `&a.Stamps.Created`. A pointer to no field of the receiver, a nil
  attribute, or a Schema that panics (a field behind a nil pointer embed)
  is an `*orm.SchemaError` (codes `E0501`, `E0502`, `E0503`) every query
  of the model returns and `orm.Check` reports.
- `orm.SchemaOf(t)` is what a type declares, for packages reading their
  own attributes back (`FieldAttrs("Email")`, `ModelAttrs()`).
- A `Meta()` method and `orm.SetFieldOptions` still work, deprecated for
  `Schema`; `orm:` struct tags are still read, under the Schema's
  attributes.

### Typed fields

`ormgen` (`//go:generate go run github.com/vuka-lang/orm/cmd/ormgen`, or
`orm generate`) writes, beside the models of `post.go`, a `post_gen.go`
with each model's typed fields: `PostFields`, an `orm.Field[V]` (an
`orm.TextField[V]` for strings, an `orm.NumberField[V]` for numbers) per column and a field per relation, and
`PostPath`, the same reached through a relation (`UserFields.Posts`),
whose own relations are methods (`PostFields.Author.Posts().Title`: a type
can't hold itself).

```go
// Code generated by ormgen. DO NOT EDIT.

var PostFields = struct {
	PostPath
	Author UserPath
	Tags   TagPath
}{
	PostPath: PostPathAt(""),
	Author:   UserPathAt("author__"),
	Tags:     TagPathAt("tags__"),
}

type PostPath struct {
	prefix string
	ID     orm.NumberField[int64]
	Title  orm.TextField[string]
}

func PostPathAt(prefix string) PostPath {
	return PostPath{
		prefix: prefix,
		ID:     orm.NumberFieldAt[int64](prefix + "id"),
		Title:  orm.TextFieldAt[string](prefix + "title"),
	}
}
```

They build the same conditions as `orm.Q` strings (`PostFields.Title.IContains("go")`
is `orm.Q{"title__icontains": "go"}`), orderings for `OrderBy`
(`PostFields.ID.Desc()`), arithmetic for `Update` (`orm.Set{"views":
PostFields.Views.Add(1)}`, `Add`/`Sub`/`Mul`/`Div` chaining as
`orm.NumExpr`s), fields for `SaveFields(ctx, PostFields.Title)`, and
relation paths for `Load` and `PrefetchRelated` (`PostFields.Tags`). A pointer or a `vuka.Option[V]`
field is an `orm.Field[V]`, with `IsNull` for its NULLs. They are a
convenience: without them every query works with strings, and an unknown
lookup is a runtime error. `examples/shop` is a program written this way.

`orm.WithDB(ctx, db)` sends a context's queries to one database (tests,
tools); `orm.Using(ctx, "name")` to another registered one. A model names
its database with `orm.On("name")` or `Meta.DB`.

## Databases

`orm.Default` is the program's `*orm.Databases`: `orm.AddDatabase(name, db,
opts…)` fills it, and the database marked `orm.AsDefault()` (or the only
one) is the default. A process serving several apps gives each its own set
(`orm.NewDatabases()`) and puts it on each request's context with
`orm.WithDatabases(ctx, set)`. A database added this way is used as it is
(no supervisor): the program opened it, and closes it. To have the ORM open,
supervise and close them, declare them instead:

```go
dbs, err := orm.OpenDatabases(ctx,
	orm.Declare("main", os.Getenv("DATABASE_URL"), orm.AsDefault(),
		orm.Replicas(os.Getenv("REPLICA_URL")),
		orm.Pool{MaxOpen: 20, MinIdle: 2, Acquire: 2 * time.Second}),
	orm.Declare("analytics", "postgres://…/analytics?pool_max=5&breaker_failures=3", orm.ReadOnly()),
	orm.Routers(tenantRouter{}))
defer dbs.Close(ctx)
ctx = orm.WithDatabases(ctx, dbs)
```

`orm.Declare(name, url, opts…)` takes `orm.AsDefault()`, `orm.ReadOnly()`
(writes refused, see below), `orm.Replicas(urls…)`, `orm.Pool{…}`,
`orm.Credentials(fn)`, `orm.Session{…}`, `orm.Timeout(d)` and
`orm.SlowQuery(d)`. `orm.OpenDatabases` is `orm.NewDatabases(opts…)` then
`Start(ctx)`; with nothing declared it reads `DATABASE_URL` (as `default`)
and `DATABASE_URL_<NAME>`, as the `orm` command does. `Start` first runs
`set.Check()`: a model's `Meta` (or `orm.On`/`orm.Mirror`), a table's
`Route` or a connection handle naming a database the set doesn't declare
fails it, and so does a relation between models on two databases that
isn't `CrossDB`. `Start` and `Stop` make a `*orm.Databases` a
`Starter`/`Stopper` for frameworks; `orm.OnOpen(fn)` runs work once the
databases are open (migrations, a seed).

## Multiple databases

A model's databases come from, first to last:

1. `orm.Using(ctx, name)` — every query with that context (`orm.WithDB` still
   wins, for tests and tools);
2. the set's routers — `orm.Router` (`ReadDB`, `WriteDB`, `AllowRelation`,
   `AllowMigrate`), registered with `orm.Routers(…)` or `set.AddRouter(r)`;
   the first router naming a database wins; embed `orm.BaseRouter` for the
   methods you don't decide;
3. the model's `Meta`: `DB` (reads and writes), `Read` and `Write` (over
   `DB`), and `Mirror` (writes repeated there, see `orm.Mirror`); `orm.On`
   and `orm.Mirror` on `For` are the same, over `Meta`;
4. the table's `Route` (`set.Route("categories", orm.Route{DB: "legacy",
   Mirror: "main"})`) — configuration for tables whose models say nothing;
5. the default.

```go
func (e *Event) Schema(s *orm.Schema) { s.Model(orm.Meta{DB: "analytics"}) }
func (n *Note) Schema(s *orm.Schema)  { s.Model(orm.Meta{Read: "replica", Write: "main"}) }

type tenants struct{ orm.BaseRouter }

func (tenants) ReadDB(ctx context.Context, m orm.ModelInfo) string  { return tenantOf(ctx) }
func (tenants) WriteDB(ctx context.Context, m orm.ModelInfo) string { return tenantOf(ctx) }
```

```vuka
type Event struct {
	orm.Base @orm.Meta{DB: "analytics"}
	…
}
```

`AllowRelation` and `AllowMigrate` return bools, so they can't abstain:
every router must allow (`BaseRouter` allows everything).

**Read-your-writes.** Reads go to the read side — the `Read` database, or a
replica of the database — except inside a transaction, after a write in the
same `orm.Scope(ctx)`, or under `orm.Primary(ctx)`: then a model's reads go
where it writes. Middleware starts a scope per request (ormweb does); once a
request writes `Note`, its reads of `Note` see the write. A scope that
wrote a database also sends raw reads of it to the primary.
`orm.ReadYourWrites(false)` on the set turns it off.

**Cross-database relations.** A relation to a model on another database is
declared `CrossDB` — `@orm.Rel{CrossDB: true}` in Vuka, `orm:"crossdb"` in a
tag, `FieldOptions.CrossDB`. Its key is a plain indexed column, with no
foreign key constraint; `PrefetchRelated`, `Load` and `FK.Get` read the
related rows from their own database, in batches by key; filtering by the
key (`customer`, `customer__id`) reads the column. A query that would join
it — a filter, an ordering or `SelectRelated` through it
(`Order.Customer.Name.Eq(…)`), or through its inverse — is refused before
it is sent, naming the relation. Without `CrossDB`, a relation between
models on two databases fails `Check`, naming both; routers' `AllowRelation`
is consulted for every relation.

**Transactions.** `orm.Transaction(ctx, fn, opts…)` (`orm.Atomic`, by
another name)
begins on the database of the first write inside it — reads before it run
outside, on the models' write databases. A write to another database fails:
*a transaction spans one database*. `orm.TxOn("analytics")` names the
database from the start; `orm.Retry(n)` runs the transaction again on a
deadlock or serialization failure (Postgres 40001/40P01, MySQL 1213/1205,
SQLite busy), with a jittered backoff — fn must be safe to run twice, and
only the outermost transaction retries. Nested, `Atomic` is a savepoint
(taken when the transaction begins, if it begins inside). `orm.OnCommit(ctx,
fn)` runs once the transaction commits — never for a rolled back one or a
rolled back savepoint: an outbox's hook.

```go
err := orm.Transaction(ctx, func(ctx context.Context) error {
	return e.Save(ctx)
}, orm.TxOn("analytics"), orm.Retry(3))
```

**Migrations.** A model's table is migrated on the database its writes go
to (routed as above, by `orm.Default`'s routers — register them at init for
the `orm` command, which runs before `main`) and on its `Mirror`, where every
router's `AllowMigrate` allows it. `orm makemigrations --db analytics`
writes `migrations/analytics/` with the models routed there;
`orm migrate` applies every database's (`--db` for one). A single-database
project is unchanged: the default keeps `migrations/`.

## Raw SQL

```go
type Sale struct {
	Day   time.Time
	Total int64
	Note  vuka.Option[string] // NULL → None; a pointer works too
}

sales, err := orm.Raw[Sale](ctx, "SELECT day, SUM(total) AS total, NULL AS note FROM sales WHERE day > ? GROUP BY day", since, orm.On("analytics"))
n, err := orm.RawRow[int64](ctx, "SELECT COUNT(*) FROM sales", orm.On("analytics")) // NotFound for no row
_, err = orm.Exec(ctx, "DELETE FROM sales WHERE day < ?", cutoff, orm.On("analytics"))
posts, err := Posts.Raw("SELECT * FROM posts WHERE views > ?", 10).All(ctx)          // the model's read database
```

Columns fill struct fields by `db` tag, column or name; a column the struct
has no field for is an error unless `orm.Loose()` is among the arguments.
`?` marks arguments everywhere (written `$1`… on Postgres; `??` is a literal
`?`), and values are always bound. Without `orm.On` a raw read goes to the
default database's read side (a replica, outside a transaction or a scope
that wrote it), `Exec` to its primary. `orm.Timeout(d)` among the arguments
bounds the statement. Using, routers, transactions and observers apply as
to any query; observers see `QueryInfo.DB` (and `Replica`).

**Connection handles.** A struct embedding `orm.Conn`, named for a database,
is injected by type (ormweb provides it):

```vuka
@orm.Database("analytics")
type Analytics struct{ orm.Conn }

@web.Get("/report")
func Report(ctx context.Context, a *Analytics) Result[[]Sale] {
	return vuka.Of(orm.Raw[Sale](a.With(ctx), "SELECT day, SUM(total) AS total FROM sales GROUP BY day"))
}
```

In Go, `orm.ConnFor[Analytics]("analytics")` declares it and returns its
constructor (`func(*orm.Databases) *Analytics`); `orm.HandleOf[T](set)`
makes one. A `Conn` is a `context.Context` too (background, carrying its
database), so `orm.Raw[R](a, …)` works; `a.With(ctx)` keeps the request's
cancellation, transaction and scope. A handle among Raw's arguments routes
it as `orm.On` does. `Exec`, `Ping`, `Stats`, `DB`, `Name` are its methods.

## Connection pool

Every declared database has a supervisor:

| `orm.Pool` field | URL parameter | Default | |
|---|---|---|---|
| `MaxOpen` | `pool_max` | 20 | connections at most; negative: no limit |
| `MaxIdle` | `pool_max_idle` | 5 | idle kept (at most `MaxOpen`); negative: none |
| `MinIdle` | `pool_min_idle` | 0 | opened at start and kept open while up |
| `MaxLifetime` | `pool_max_lifetime` | 1h | less up to 20% at random, so connections don't all reconnect at once; negative: never retired |
| `MaxIdleTime` | `pool_max_idle_time` | none | |
| `Acquire` | `pool_acquire` | wait | with `MaxOpen` in use, fail `Unavailable` after this rather than wait forever |
| `Health` | `pool_health` | 10s | ping interval (primary and replicas) |
| `Breaker{Failures, Cooldown}` | `breaker_failures`, `breaker_cooldown` | 5, 5s | circuit breaker |
| `StartupWait` | `startup_wait` | 5s | how long `Start` waits for the database; negative doesn't wait |

The URL parameters are taken off before the driver sees the URL (MySQL's
still goes through `orm.MySQLDSN`). A zero field takes the default, so
`pool_max=0` is 20 connections, not unlimited: write `pool_max=-1` for no
cap. `orm.Connect` sets the same caps on the pool it opens.

**Read-only databases.** `orm.ReadOnly()` has the ORM refuse its own writes
and makes each of the database's connections read-only on the server:
MySQL runs `SET SESSION TRANSACTION READ ONLY`, Postgres sets
`default_transaction_read_only = on`, SQLite `PRAGMA query_only = ON`. So
raw SQL (`orm.Raw`, `orm.RawRow`) can't write either. SQL that runs on the
connection can still switch those back, though: the only boundary is the
database's own grants, a user allowed to `SELECT` and nothing else. A
database added with `Add` (not declared) gets the ORM's refusal alone.

- **States**: `Connecting`, `Up`, `Degraded` (a replica down, or the breaker
  probing), `Down`. A database that doesn't answer within `StartupWait`
  starts `Down` and the program starts anyway; the supervisor reconnects in
  the background with exponential backoff and jitter (100ms to 30s). State
  changes are logged (through `orm.Logger(l)`, else `slog.Default()`) and
  reach `orm.ObserveState(fn)`; nothing else is logged but slow queries
  (`orm.SlowQuery(d)`).
- **Circuit breaker**: only connection errors count — refused, reset, timed
  out, dropped; never a query's own error (each driver package classifies its
  errors: `orm.Classify(err)`). After `Failures` in a row, statements fail at
  once with `orm.Unavailable`; after `Cooldown` one statement probes, and its
  success (or a successful health ping) closes it.
- **Retries**: a read that fails on a dropped connection is retried once, on a
  fresh connection (the older ones are retired); writes never are.
- **Replicas**: reads go to the healthy replica least in use, the primary
  when none is; a replica failing a read or a ping is ejected (the read is
  retried on the primary) until a ping succeeds again.
- **Credentials**: `orm.Credentials(func(ctx) (user, pass string, err error))`
  is asked for every new physical connection (a rotating secret, an IAM
  token); the driver packages put them into the DSN.
- **Session settings**: `orm.Session{"statement_timeout": "5s"}` runs on each
  new connection: `SET` on Postgres, `SET SESSION` on MySQL, `PRAGMA` on
  SQLite.
- **Timeouts**: `orm.Timeout(d)` on `Declare` bounds every statement;
  `orm.WithTimeout(ctx, d)` a context's.
- **Stats**: `orm.Stats(name)`, `set.StatsOf(name)`, `set.Stats()` —
  `database/sql`'s pool statistics, state, breaker, last error and each
  replica's health.
- **Shutdown**: `set.Close(ctx)` (or `Stop`) refuses new statements
  (`Unavailable`, "closing"), waits for running ones (transactions, open
  rows) until ctx's deadline, then closes the pools.

## Web integration

`github.com/vuka-lang/orm/ormweb` serves a set in a
[vuka web](https://github.com/vuka-lang/web) app (web doesn't import the
ORM; this package is in the ORM's module):

```vuka
func main() {
	app := web.New(
		ormweb.Databases(
			orm.Declare("main", os.Getenv("DATABASE_URL"), orm.AsDefault(), orm.Replicas(os.Getenv("REPLICA_URL"))),
			orm.Declare("analytics", os.Getenv("ANALYTICS_URL")),
		),
		web.Use(ormweb.UsingFrom(func(r *http.Request) string { return r.Header.Get("X-Tenant") })),
	)
	log.Fatal(app.Run(""))
}
```

`ormweb.Databases(opts…)` is one `web.Option`. It provides `*orm.Databases`
— a supplied value, so the app's start opens it (waiting `StartupWait`)
before anything built from constructors starts, and its stop closes it
gracefully after they stop — and every connection handle type; puts the set
and an `orm.Scope` on each request's context; serves `GET /healthz`: JSON
of the status and each database's state, 503 when the default database is
down (in development also each one's pool, breaker, last error and
replicas); and adds the set's `Check` to the app's `Build`, so a
model, route or handle naming a database the app doesn't declare is a Build
error, reported with web's own.

Development is `VUKA_ENV=development` in the program's real environment,
read before `main`: a `.env` file setting it doesn't count, so a file left
on a server can't turn on migrations or the details. Its methods tune it:
`.MigrateOnStart(b)` runs pending migrations as the databases open (by
default only in development; production migrates when this says so, or with
`orm migrate`), `.HealthDetails(b)` gives the health route the details (by
default only in development: a driver's error text and the replicas'
addresses are for operators), `.Health(path)` moves the health route (`""`
drops it). `ormweb.Set(s, opts…)` serves a set made elsewhere.
`ormweb.Middleware(s)`, `ormweb.HealthHandler(s)` (states) and
`ormweb.DetailedHealthHandler(s)` (the whole `Report`) are the parts for
another router;
`ormweb.UsingFrom(fn)` is middleware sending a request's queries to the
database `fn` names. web has no debug toolbar; observers carry the database
name for one.

`examples/multidb` is a whole app: products on main read from a replica,
sales on analytics holding their product by a CrossDB key, a raw report
through an injected `*Analytics`, a tenant's database per request, and
`/healthz` (`cd examples/multidb && vuka run .`).

## Drivers

Drivers are opt-in blank imports, `database/sql` style; the `orm` package
links none.

| Import                          | Driver                                   | `Connect` dialect |
|---------------------------------|------------------------------------------|-------------------|
| `github.com/vuka-lang/orm/postgres` | pgx (`github.com/jackc/pgx/v5/stdlib`)   | `postgres`        |
| `github.com/vuka-lang/orm/mysql`    | `github.com/go-sql-driver/mysql`         | `mysql` (`orm.MySQLDSN`: parseTime on, utf8mb4 in the server's collation) |
| `github.com/vuka-lang/orm/sqlite`   | pure-Go `github.com/glebarez/go-sqlite`  | `sqlite`          |

The SQLite package also registers `l2_distance`, `cosine_distance`,
`inner_product` and trigram `similarity` in Go, so vector and trigram
searches run (unindexed) on SQLite in development and tests; its text
search is FTS5 (see [Search](#search)).

On MySQL, `Connect` adds `charset=utf8mb4` to a DSN naming neither a
charset nor a collation: the driver would otherwise greet the server in
`utf8mb4_general_ci`, while MySQL 8 and 9 make tables in
`utf8mb4_0900_ai_ci`, and comparing a string the server derives in the
connection's collation (a JSON_TABLE column) with a column fails with an
illegal mix of collations. `SET NAMES utf8mb4` takes the server's default
collation, its tables' — on MySQL 8.0, 8.4, 9 and MariaDB alike. A DSN's
own `charset` or `collation` is kept; `orm.MySQLDSN(dsn)` is the DSN
`Connect` opens, for a `*sql.DB` opened elsewhere.
`orm.ConnectURL("postgres://…" | "mysql://user:pass@tcp(host)/db" |
"sqlite:file.db")` picks the dialect from the URL. A SQLite file's
directory is made when it is missing (`sqlite://data/app.db` opens on a
fresh checkout), by `Connect` and by a declared database alike. `orm.Open(sqlDB,
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
| `Unavailable`   | the database didn't answer within `MigrateWait` (Migrate, Verify); a declared database's breaker is open, no connection was free within `Acquire`, or it is closing |

`orm.KindOf(err)` is 0 for an error the ORM gives no meaning (a driver's, a
bad query). A web framework maps the kinds onto HTTP statuses, for example:

| orm Kind        | HTTP |
|-----------------|------|
| `NotFound`      | 404  |
| `MultipleFound` | 409  |
| `Conflict`      | 409 (the column → "already exists") |
| `Invalid`       | 422 (the column → "does not exist") |
| `Unavailable`   | 503  |
| none            | 500  |

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

`QueryInfo` carries the SQL, its arguments, the database's name (`DB`, and
`Replica` for a read on one), start time, duration, rows affected (an exec's; -1 for a query), the error, and `Refused`: a query
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
go run github.com/vuka-lang/orm/cmd/orm migrate [target|zero] [--db name] [--noinput]
go run github.com/vuka-lang/orm/cmd/orm showmigrations
go run github.com/vuka-lang/orm/cmd/orm sqlmigrate <name> [--backwards] [--db name]
```

(`go get -tool github.com/vuka-lang/orm/cmd/orm` then `go tool orm …`.)

- `generate` writes `orm_scanners_gen.go` per package: an
  `orm.Register[T]()` for every type embedding `orm.Model[T]` and row
  scanners/writers that skip reflection, and each model's typed fields in
  `<file>_gen.go` beside the file declaring it (see [Typed
  fields](#typed-fields)). Without the scanners the ORM reads rows by
  reflection. `ormgen.Write` / `ormgen.Generate` are the library
  form; `go:generate go run github.com/vuka-lang/orm/cmd/ormgen ./...`
  works too.
- The migration commands build the program's main package (`--main` when
  the module has several) with an init overlaid that serves
  `orm.ServeCLI` before `main` runs, the migrations packages imported and
  the generated code overlaid where it isn't on disk; nothing is written
  to the tree but the migration file.
- They connect by URL: `--database name=URL` (repeatable; `default=` for
  the default), else `DATABASE_URL` and `DATABASE_URL_<NAME>` from the
  environment, else from the module's `.env` file.
  `--default main` names the default database as the models' `orm.On`
  calls name it. The program links the drivers it opens.
- On a terminal `makemigrations` asks about renames ("Did you rename
  users.mail to users.email? [y/N]"); `--noinput` writes a removal and an
  addition with a note.
- On a terminal `migrate` lists the migrations a target (or `zero`) would
  unapply, whose backwards operations may drop tables and their data, and
  asks before running them; an answer other than yes changes nothing.
  `--noinput` (CI) doesn't ask; neither does a `migrate` whose input isn't
  a terminal.

At run time `orm.Migrate(ctx, orm.MigrateOn("main"), orm.MigrateLogger(l))`
applies pending migrations under a lock (Postgres advisory lock, MySQL
`GET_LOCK`), recording them in `orm.MigrationsTable` (`orm_migrations` by
default; a program whose migrations are recorded under another name sets it).
`orm.ApplyMigrations(ctx, db, target, migs…)` runs a given list (tools,
tests). `orm.CreateExtension("vector")` declares a Postgres extension for
the next migration; `schema.Verify(ctx, models…)` checks models against a
database at start up.

## Generated code is optional

`orm generate` only makes a program faster; without its files it behaves
the same:

| Generated | Without it |
|---|---|
| row scanners and writers | rows read and written by reflection |
| typed field sets | `orm.Q{"name__istartswith": w}`, `orm.F("name")`; an unknown lookup is a runtime error with a did-you-mean |
| `orm.Register[T]()` per model | a model's `orm.For[T]()`, or its first `Objects`, registers it; `orm.Check(models…)` takes others |

Each model looks for its scanner once, on its first query, and keeps the
answer; a scanner whose columns aren't the model's is ignored. The cost of
reflection (Apple M1 Pro, `go test -bench Fallback`, generated → reflection):

| | stub driver (the ORM's own cost) | SQLite |
|---|---|---|
| 1000 rows, 6 columns | 146 → 195 µs (1.34×) | 1.65 → 1.74 ms (1.05×) |
| 1000 rows, 23 columns | 525 → 875 µs (1.67×) | 5.20 → 5.59 ms (1.07×) |
| one row by id | 1.8 → 2.0 µs (1.12×) | |
| insert 1000 rows | 609 → 690 µs (1.13×) | |

`ORM_REFLECT=1 go test` runs the ORM's own suite with the generated scanners
ignored; `TestReflectionFallback` does so on every `go test`.

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
| vectors        | pgvector (`<->`, `<=>`, `<#>`, HNSW/IVFFlat) | stored (`VECTOR(n)` on 9, `VARBINARY` before), not searched | distances in Go, unindexed (O(n))             |

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
row. MySQL stores vectors but doesn't search them: MySQL 9's `VECTOR`
type has no distance function outside HeatWave. A `Vector` field is a
`VECTOR(n)` column where the server has the type (MySQL 9, MariaDB 11.7;
the ORM reads `VERSION()` once when it makes tables or runs migrations),
else a `VARBINARY(4n)` holding the same little-endian float32s, so an
upgraded server turns it into a `VECTOR` in place (`ALTER TABLE … MODIFY
… VECTOR(n)`). Vector indexes are left out, and a distance, `Nearest` or
`Hybrid` fails when the query is written, naming the field. `orm sqlmigrate
--dialect mysql` writes the `VARBINARY` form; a server's version after a
slash (`mysql/9.3.0`) writes its own.

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

## Vuka

In [Vuka](https://github.com/vuka-lang/vuka) a model's attributes are
written on its fields, and its queries are the engine's, through the same
typed fields Go code uses. Vuka writes the plain Go: Go and Vuka packages
share models and databases.

```vuka
type Post struct {
	orm.Base  @orm.Meta{Table: "posts", Ordering: []string{"-published"}}
	ID        int64             @orm.PK
	Title     string            @orm.Char{Max: 200} @orm.Trigram
	Slug      string            @orm.Slug{Unique: true}
	Author    orm.FK[User]      @orm.Rel{OnDelete: orm.Cascade, Related: "posts"}
	Tags      orm.M2M[Tag]
	Published Option[time.Time] @orm.Index
	Views     int               @orm.DefaultTo(0)
}

func Post.Live() orm.QuerySet[Post] { return Post.Objects.Filter(Post.Published.IsNull(false)) }

posts := Post.Objects.Filter(Post.Title.Contains("Go"), Post.Author.Name.Eq("ada")).
	OrderBy(Post.Views.Desc()).All(ctx)?

@orm.Transaction
func publish(ctx context.Context, slug string) error {
	p := Post.Objects.Get(ctx, Post.Slug.Eq(slug))?
	p.Published = Some(time.Now())
	return p.Save(ctx) // an error rolls back
}
```

is, in Go:

| Vuka | Go |
|---|---|
| `orm.Base` and the fields' attributes | `orm.Base[Post]` and a `Schema` method: `s.Model(orm.Meta{…})`, `s.Field(&p.Title, orm.Char{Max: 200}, orm.Trigram{})` |
| `Post.Objects` | `orm.Objects[Post]()`, the model's manager |
| `Post.Title`, `Post.Author.Name` | `PostFields.Title`, `PostFields.Author.Name`: the typed fields `orm generate` writes (the vuka command overlays them until it has) |
| `func Post.Live()` | `func PostLive()` |
| `@orm.Transaction` (`@orm.TxOn("db")`, `@orm.Retry(3)`) | `orm.Transaction(ctx, func(ctx context.Context) error { … }, orm.TxOn("db"), orm.Retry(3))` |

Everything else is the engine as Go has it: `Filter`, `OrderBy`,
`Annotate`, `Values[R]`, `Get` and `First` returning `(T, error)` (`?`
takes them; `errors.Is(err, orm.ErrNotFound)` tells none), row methods
(`Save`, `SaveFields(ctx, Post.Title)`, `Load(ctx, Post.Tags)`,
`p.Tags.Add(ctx, &tag)`), expressions through a field's `Expr()`
(`orm.Lower(Post.Title.Expr())`) and names through its `Name()`.
`vukatest` runs these on SQLite (`vuka test ./vukatest/...`), and on
Postgres or MySQL with `ORMTEST_DRIVER`.

- **`orm.Base`** is `orm.Model` without its `Objects` method, so that
  `Post.Objects` is the static Vuka gives the model. `@orm.Meta{…}
  @orm.Entity` on the type works too; the `orm.Entity` type decorator
  registers the model at start-up, so `orm.Check` knows it without `orm
  generate`.
- **Field attributes** are the values of [Schema](#schema) — `@orm.PK` is
  `orm.PK{}`, `@orm.Column("x")` is `orm.Column("x")` — and an
  attribute of the program's own is any value with `Apply(*orm.FieldOptions)`.
- **`orm.FK[T]`**, **`orm.M2M[T]`** and **`Option[T]`** fields are as in Go.

Earlier releases gave Vuka a layer of its own: `orm.Query[T]`, field
references typed by their model (`vuka.Pred[T]`), package `orm/fn`,
`orm.Into`, and terminals returning `Result`. It still compiles for code
that names it, and goes in the next minor release.

### Migrations

`orm makemigrations` and the other commands build the program through
`vuka gen` when the module has `.vuka` files (the `vuka` command on the
PATH, or `VUKA`); a main package declaring models gets the tool's init
after its own, so its registrations and field attributes are in. See
`examples/blog`: models with attributes, its migrations written by the
command, queries with field references, `match` on `Get`, `First`'s
Option, `orm.Transaction`, a typed SQLite function in Go, text, trigram
and vector search:

```
cd examples/blog && vuka run .
```

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
- **Field metadata from outside the struct.** A model's `Schema` method
  (see [Schema](#schema)) takes attributes of any package implementing
  `orm.FieldAttr` or `orm.ModelAttr`. `orm.SetFieldOptions(fn)`, which
  supplies `orm.FieldOptions` per field over the tags, is deprecated for
  it.

## License

MIT
