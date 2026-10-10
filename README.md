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
searches run (unindexed) on SQLite in development and tests.
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
