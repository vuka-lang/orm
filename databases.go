package orm

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Databases is the named databases models query: the default, and others
// a model names with On (or Meta.DB, or a Schema). A program has one,
// Default, which AddDatabase fills; a process serving several apps (tests,
// a gateway) gives each its own and puts it on the app's contexts with
// WithDatabases, so one manager queries the database of the app serving
// the request.
//
//	db, err := orm.Connect("postgres", os.Getenv("DATABASE_URL"))
//	orm.AddDatabase("main", db, orm.AsDefault())
type Databases struct {
	mu      sync.RWMutex
	dbs     map[string]*DB
	def     string
	routes  sync.Map // table → Route
	routers []Router
	noRYW   bool
	log     *slog.Logger
	specs   []*DatabaseSpec // declared, opened by Start
	onOpen  []func(context.Context, *Databases) error
	opened  []*DB // those Start opened, which Close closes
}

// NewDatabases is a set of databases: empty, or of the databases Declare
// declares (opened by Start) and the routers and settings the options
// give.
func NewDatabases(opts ...OpenOption) *Databases {
	s := &Databases{dbs: map[string]*DB{}}
	for _, o := range opts {
		o.openOption(s)
	}
	return s
}

// Default is the program's databases: those AddDatabase adds, queried by
// every context without WithDatabases.
var Default = NewDatabases()

// DatabaseOption tunes a database: one added (AsDefault) or declared
// (AsDefault, ReadOnly, Replicas, Pool, Credentials, Session, Timeout,
// SlowQuery).
type DatabaseOption interface{ apply(*databaseConfig) }

type databaseOptionFunc func(*databaseConfig)

func (f databaseOptionFunc) apply(c *databaseConfig) { f(c) }

type databaseConfig struct {
	def, readOnly bool
	replicas      []string
	pool          Pool
	creds         func(ctx context.Context) (string, string, error)
	session       map[string]string
	timeout, slow time.Duration
}

// AsDefault makes the database the default: the one models without On
// query. Without one, the only database is the default.
func AsDefault() DatabaseOption { return databaseOptionFunc(func(c *databaseConfig) { c.def = true }) }

// AddDatabase adds db to Default as name.
func AddDatabase(name string, db *DB, opts ...DatabaseOption) { Default.Add(name, db, opts...) }

// Add adds db as name, replacing a database of that name.
func (s *Databases) Add(name string, db *DB, opts ...DatabaseOption) {
	var c databaseConfig
	for _, o := range opts {
		o.apply(&c)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db.name == "" {
		db.name = name
	}
	s.dbs[name] = db
	if c.def {
		s.def = name
	}
}

// Remove removes the database name; it doesn't close it.
func (s *Databases) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dbs, name)
	if s.def == name {
		s.def = ""
	}
}

// Names is the databases' names, sorted.
func (s *Databases) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.dbs))
	for n := range s.dbs {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Lookup is the database name: "" the default (the one AsDefault marked,
// else the only one).
func (s *Databases) Lookup(name string) (*DB, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name == "" {
		if s.def != "" {
			name = s.def
		} else {
			switch len(s.dbs) {
			case 0:
				return nil, errNoDB
			case 1:
				for _, d := range s.dbs {
					return d, nil
				}
			}
			if d, ok := s.dbs[""]; ok {
				return d, nil
			}
			return nil, fmt.Errorf("orm: no default database: add one with orm.AsDefault()")
		}
	}
	d, ok := s.dbs[name]
	if !ok {
		return nil, fmt.Errorf("orm: no database %q: add it with orm.AddDatabase", name)
	}
	return d, nil
}

// Route is a table's databases, over its models' own (On's, Meta's,
// Mirror's): routing a table moves it without a code change.
type Route struct {
	DB     string `toml:"db" json:"db"`
	Mirror string `toml:"mirror" json:"mirror"`
}

// Route routes table: its models read and write r.DB, and their writes are
// repeated on r.Mirror (see Mirror).
//
//	orm.Default.Route("categories", orm.Route{DB: "legacy", Mirror: "main"})
func (s *Databases) Route(table string, r Route) { s.routes.Store(table, r) }

// Unroute drops table's route.
func (s *Databases) Unroute(table string) { s.routes.Delete(table) }

func (s *Databases) route(table string) (Route, bool) {
	r, ok := s.routes.Load(table)
	if !ok {
		return Route{}, false
	}
	return r.(Route), true
}

type databasesKey struct{}

// WithDatabases has ctx's queries find their databases in s instead of
// Default.
func WithDatabases(ctx context.Context, s *Databases) context.Context {
	return context.WithValue(ctx, databasesKey{}, s)
}

// databasesOf is ctx's Databases: WithDatabases's, else Default.
func databasesOf(ctx context.Context) *Databases {
	if s, ok := ctx.Value(databasesKey{}).(*Databases); ok {
		return s
	}
	return Default
}

var drivers struct {
	mu    sync.RWMutex
	names map[string]string // dialect → database/sql driver name
}

// RegisterDriver names the database/sql driver Connect opens for a
// dialect. The driver packages (orm/postgres, orm/mysql, orm/sqlite)
// register theirs: blank-import the one the program opens.
//
//	import _ "github.com/vuka-lang/orm/postgres"
func RegisterDriver(dialect, sqlDriver string) {
	drivers.mu.Lock()
	defer drivers.mu.Unlock()
	if drivers.names == nil {
		drivers.names = map[string]string{}
	}
	drivers.names[DialectFor(dialect).Name()] = sqlDriver
}

// Connect opens dsn on dialect ("postgres", "mysql", "sqlite") through the
// driver its package registered, as the ORM reads it: MySQL as MySQLDSN
// sets it, in-memory SQLite on one connection (each would be a database
// of its own). It doesn't ping.
func Connect(dialect, dsn string) (*DB, error) {
	d := DialectFor(dialect)
	drivers.mu.RLock()
	name, ok := drivers.names[d.Name()]
	drivers.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("orm: no driver for %s is linked: blank-import it, _ \"github.com/vuka-lang/orm/%s\"", d.Name(), d.Name())
	}
	if d.Name() == "mysql" {
		dsn = MySQLDSN(dsn)
	}
	s, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	if d.Name() == "sqlite" && (dsn == "" || strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")) {
		s.SetMaxOpenConns(1)
		s.SetMaxIdleConns(1)
		s.SetConnMaxLifetime(0)
		s.SetConnMaxIdleTime(0)
	}
	return Open(s, d.Name()), nil
}

// MySQLDSN is a go-sql-driver DSN as the ORM reads MySQL: parseTime on,
// which time fields need, and, unless the DSN names a charset or a
// collation, charset=utf8mb4 with no collation. The driver otherwise
// greets the server in utf8mb4_general_ci, while MySQL 8 and later make
// tables in their default, utf8mb4_0900_ai_ci: a string the server
// derives in the connection's collation (a JSON_TABLE column) then can't
// be compared with a column (Error 1267, illegal mix of collations).
// SET NAMES utf8mb4 takes the server's default collation for utf8mb4,
// the one its tables have: MySQL 8.0, 8.4 and 9 and MariaDB alike. A
// DSN's own charset or collation is left as it is.
func MySQLDSN(dsn string) string {
	// The driver's own reading: the parameters follow the last slash.
	_, params, hasParams := strings.Cut(dsn[strings.LastIndexByte(dsn, '/')+1:], "?")
	has := func(key string) bool {
		for kv := range strings.SplitSeq(params, "&") {
			if k, _, _ := strings.Cut(kv, "="); k == key {
				return true
			}
		}
		return false
	}
	add := func(kv string) {
		if hasParams {
			dsn += "&" + kv
		} else {
			dsn, hasParams = dsn+"?"+kv, true
		}
	}
	if !has("parseTime") {
		add("parseTime=true")
	}
	if !has("charset") && !has("collation") {
		add("charset=utf8mb4")
	}
	return dsn
}

// ConnectURL opens a database by URL: postgres://… (or postgresql://),
// mysql://user:pass@tcp(host:3306)/db?… (the driver's DSN after
// mysql://), sqlite:path, sqlite://path or sqlite::memory:.
func ConnectURL(url string) (*DB, error) {
	dialect, dsn, err := ParseURL(url)
	if err != nil {
		return nil, err
	}
	return Connect(dialect, dsn)
}

// ParseURL is a database URL's dialect and the DSN its driver takes, as
// ConnectURL reads it.
func ParseURL(url string) (dialect, dsn string, err error) {
	scheme, rest, ok := strings.Cut(url, ":")
	if !ok {
		return "", "", fmt.Errorf("orm: database URL %q: no scheme (postgres://, mysql://, sqlite:)", redact(url))
	}
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql", "pgx":
		return "postgres", "postgres:" + rest, nil
	case "mysql":
		return "mysql", strings.TrimPrefix(rest, "//"), nil
	case "sqlite", "sqlite3", "file":
		if strings.ToLower(scheme) == "file" {
			return "sqlite", url, nil
		}
		return "sqlite", strings.TrimPrefix(rest, "//"), nil
	}
	return "", "", fmt.Errorf("orm: database URL scheme %q: postgres, mysql or sqlite", scheme)
}

// redact is a URL without its password, for errors.
func redact(url string) string {
	at := strings.LastIndexByte(url, '@')
	scheme := strings.Index(url, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return url
	}
	creds := url[scheme+3 : at]
	if user, _, ok := strings.Cut(creds, ":"); ok {
		return url[:scheme+3] + user + ":***" + url[at:]
	}
	return url
}
