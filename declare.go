package orm

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/vuka"
)

// OpenOption shapes a Databases: the databases Declare declares, Routers,
// ReadYourWrites, Logger, OnOpen.
type OpenOption interface{ openOption(*Databases) }

type openOptionFunc func(*Databases)

func (f openOptionFunc) openOption(s *Databases) { f(s) }

// DatabaseSpec is a database Declare declared: opened, supervised, by the
// Databases' Start.
type DatabaseSpec struct {
	name, url string
	def       bool
	cfg       databaseConfig
}

func (sp *DatabaseSpec) openOption(s *Databases) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = slices.DeleteFunc(s.specs, func(o *DatabaseSpec) bool { return o.name == sp.name })
	s.specs = append(s.specs, sp)
}

// Declare declares the database name at url (as ConnectURL reads it;
// pool settings may ride as URL parameters, see Pool), for OpenDatabases
// or NewDatabases. Its pool is supervised: see Pool.
//
//	dbs, err := orm.OpenDatabases(ctx,
//		orm.Declare("main", os.Getenv("DATABASE_URL"), orm.AsDefault(),
//			orm.Replicas(os.Getenv("REPLICA_URL")), orm.Pool{MaxOpen: 20, Acquire: 2 * time.Second}),
//		orm.Declare("analytics", "postgres://…/analytics?pool_max=5", orm.ReadOnly()))
func Declare(name, url string, opts ...DatabaseOption) *DatabaseSpec {
	sp := &DatabaseSpec{name: name, url: url}
	for _, o := range opts {
		o.apply(&sp.cfg)
	}
	sp.def = sp.cfg.def
	return sp
}

// ReadOnly refuses writes to the database: a reporting copy. The ORM
// refuses its own writes (Create, Save, Update, Delete, Exec), and a
// declared database's connections are read-only on the server too, so
// raw SQL (Raw, RawRow) can't write either: MySQL's sessions run SET
// SESSION TRANSACTION READ ONLY, Postgres's set
// default_transaction_read_only, SQLite's set PRAGMA query_only. A
// statement can still undo those for its own connection; only the
// database's grants (a user allowed SELECT alone) are a boundary an
// attacker running SQL can't cross. A database added with Add gets the
// ORM's refusal alone.
func ReadOnly() DatabaseOption {
	return databaseOptionFunc(func(c *databaseConfig) { c.readOnly = true })
}

// Replicas gives the database read replicas, by URL: reads outside a
// transaction (and a Scope that wrote) go to the healthy one least in
// use, the primary when none is; a replica failing its health ping is
// left out until it answers again.
func Replicas(urls ...string) DatabaseOption {
	return databaseOptionFunc(func(c *databaseConfig) { c.replicas = append(c.replicas, urls...) })
}

// Credentials supplies the user and password of every new connection
// (a rotating secret, a cloud IAM token), asked for each one the pool
// opens; they replace the URL's.
func Credentials(fn func(ctx context.Context) (user, password string, err error)) DatabaseOption {
	return databaseOptionFunc(func(c *databaseConfig) { c.creds = fn })
}

// Session is settings each new connection takes: MySQL session
// variables (SET SESSION k = v), Postgres run-time parameters (SET k =
// v), SQLite pragmas (PRAGMA k = v). Values are plain: numbers and words
// as they are, anything else quoted.
//
//	orm.Session{"statement_timeout": "5s", "search_path": "app"}
type Session map[string]string

func (s Session) apply(c *databaseConfig) {
	if c.session == nil {
		c.session = map[string]string{}
	}
	for k, v := range s {
		c.session[k] = v
	}
}

var (
	settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	plainValue  = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$|^[A-Za-z_][A-Za-z0-9_]*$`)
)

// sessionSQL is the statements setting s on dialect's connections.
func sessionSQL(dialect string, s map[string]string) ([]string, error) {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		if !settingName.MatchString(k) {
			return nil, fmt.Errorf("orm: session setting %q is no name", k)
		}
		v := s[k]
		if !plainValue.MatchString(v) {
			if dialect == "mysql" {
				v = strings.ReplaceAll(v, `\`, `\\`)
			}
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		switch dialect {
		case "mysql":
			out = append(out, "SET SESSION "+k+" = "+v)
		case "postgres":
			out = append(out, "SET "+k+" = "+v)
		default:
			out = append(out, "PRAGMA "+k+" = "+v)
		}
	}
	return out, nil
}

// readOnlySQL is the statement making a dialect's connection read-only.
func readOnlySQL(dialect string) string {
	switch dialect {
	case "mysql":
		return "SET SESSION TRANSACTION READ ONLY"
	case "postgres":
		return "SET default_transaction_read_only = on"
	}
	return "PRAGMA query_only = ON"
}

// Apply applies opts to s (before Start, for the databases it declares).
func (s *Databases) Apply(opts ...OpenOption) *Databases {
	for _, o := range opts {
		o.openOption(s)
	}
	return s
}

// Routers adds routers to the Databases (see Router).
func Routers(rs ...Router) OpenOption {
	return openOptionFunc(func(s *Databases) { s.routers = append(slices.Clip(s.routers), rs...) })
}

// ReadYourWrites turns Scope's read-your-writes on (the default) or off.
func ReadYourWrites(on bool) OpenOption {
	return openOptionFunc(func(s *Databases) { s.noRYW = !on })
}

func (s *Databases) ryw() bool { return !s.noRYW }

// Logger is where the supervisors log state changes and slow queries;
// slog.Default() otherwise.
func Logger(l *slog.Logger) OpenOption {
	return openOptionFunc(func(s *Databases) { s.log = l })
}

// OnOpen runs fn once Start opened the databases: migrations, a cache
// warm-up. Its error fails Start.
func OnOpen(fn func(ctx context.Context, s *Databases) error) OpenOption {
	return openOptionFunc(func(s *Databases) { s.onOpen = append(s.onOpen, fn) })
}

// OpenDatabases is the databases opts declare, opened (NewDatabases, then
// Start). With none declared it reads DATABASE_URL (the default,
// "default") and DATABASE_URL_<NAME> (database name, lowercased), as the
// orm command does.
func OpenDatabases(ctx context.Context, opts ...OpenOption) (*Databases, error) {
	s := NewDatabases(opts...)
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv is the databases DATABASE_URL and DATABASE_URL_<NAME> name.
func FromEnv() []OpenOption {
	var out []OpenOption
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case v == "":
		case k == "DATABASE_URL":
			out = append(out, Declare("default", v, AsDefault()))
		case strings.HasPrefix(k, "DATABASE_URL_"):
			out = append(out, Declare(strings.ToLower(strings.TrimPrefix(k, "DATABASE_URL_")), v))
		}
	}
	return out
}

// Start opens the databases declared and not yet open, supervised, waits
// up to each one's StartupWait for it to answer (one that doesn't is
// opened Down, and reconnects in the background), then runs the OnOpen
// hooks. It runs Check first: a model naming a database no one declared
// fails it. With nothing declared or added it reads the environment
// (FromEnv). A framework calls it on start (it is a Starter).
func (s *Databases) Start(ctx context.Context) error {
	s.mu.Lock()
	if len(s.specs) == 0 && len(s.dbs) == 0 {
		s.mu.Unlock()
		for _, o := range FromEnv() {
			o.openOption(s)
		}
		s.mu.Lock()
	}
	var todo []*DatabaseSpec
	for _, sp := range s.specs {
		if _, ok := s.dbs[sp.name]; !ok {
			todo = append(todo, sp)
		}
	}
	log := cmp.Or(s.log, slog.Default())
	s.mu.Unlock()
	if err := s.Check(); err != nil {
		return err
	}
	var errs []error
	var opened []*DB
	for _, sp := range todo {
		d, err := openSpec(sp, log)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		opened = append(opened, d)
	}
	if err := errors.Join(errs...); err != nil {
		for _, d := range opened {
			_ = d.sup.close(ctx)
		}
		return err
	}
	var wg sync.WaitGroup
	for _, d := range opened {
		d.sup.start()
		wg.Go(func() { d.sup.waitUp(ctx) })
	}
	wg.Wait()
	for i, d := range opened {
		var opts []DatabaseOption
		if todo[i].def {
			opts = append(opts, AsDefault())
		}
		s.Add(d.name, d, opts...)
	}
	s.mu.Lock()
	s.opened = append(s.opened, opened...)
	hooks := slices.Clone(s.onOpen)
	s.mu.Unlock()
	for _, fn := range hooks {
		if err := fn(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// waitUp waits for the database to answer, up to StartupWait.
func (s *supervisor) waitUp(ctx context.Context) {
	defer func() { s.waited.Store(true); s.update(nil) }()
	if s.pool.StartupWait < 0 {
		return
	}
	t := time.NewTimer(s.pool.StartupWait)
	defer t.Stop()
	select {
	case <-s.up:
	case <-t.C:
		if s.log != nil {
			s.log.Warn("orm: database not answering; starting without it, reconnecting", "db", s.name, "waited", s.pool.StartupWait)
		}
	case <-ctx.Done():
	}
}

// Stop closes the databases Start opened (Close): a framework calls it on
// shutdown (it is a Stopper).
func (s *Databases) Stop(ctx context.Context) error { return s.Close(ctx) }

// Close closes the databases Start opened, gracefully: new statements
// fail Unavailable, those running finish until ctx's deadline, then the
// pools close. Databases added with Add are the caller's to close.
func (s *Databases) Close(ctx context.Context) error {
	s.mu.Lock()
	opened := s.opened
	s.opened = nil
	s.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, len(opened))
	for i, d := range opened {
		wg.Go(func() { errs[i] = d.sup.close(ctx) })
	}
	wg.Wait()
	for _, d := range opened {
		s.Remove(d.name)
	}
	return errors.Join(errs...)
}

// poolParams is the URL parameters Pool reads, and what each sets.
var poolParams = map[string]func(p *Pool, v string) error{
	"pool_max":           func(p *Pool, v string) (err error) { p.MaxOpen, err = strconv.Atoi(v); return },
	"pool_max_idle":      func(p *Pool, v string) (err error) { p.MaxIdle, err = strconv.Atoi(v); return },
	"pool_min_idle":      func(p *Pool, v string) (err error) { p.MinIdle, err = strconv.Atoi(v); return },
	"pool_max_lifetime":  func(p *Pool, v string) (err error) { p.MaxLifetime, err = time.ParseDuration(v); return },
	"pool_max_idle_time": func(p *Pool, v string) (err error) { p.MaxIdleTime, err = time.ParseDuration(v); return },
	"pool_acquire":       func(p *Pool, v string) (err error) { p.Acquire, err = time.ParseDuration(v); return },
	"pool_health":        func(p *Pool, v string) (err error) { p.Health, err = time.ParseDuration(v); return },
	"breaker_failures":   func(p *Pool, v string) (err error) { p.Breaker.Failures, err = strconv.Atoi(v); return },
	"breaker_cooldown":   func(p *Pool, v string) (err error) { p.Breaker.Cooldown, err = time.ParseDuration(v); return },
	"startup_wait":       func(p *Pool, v string) (err error) { p.StartupWait, err = time.ParseDuration(v); return },
}

// SplitPoolParams is url without the Pool parameters it carries, and the
// Pool they say. Its parameters are those after the last slash's first
// ?, as a MySQL DSN has them.
func SplitPoolParams(url string) (string, Pool, error) {
	var p Pool
	start := strings.LastIndexByte(url, '/') + 1
	q := strings.IndexByte(url[start:], '?')
	if q < 0 {
		return url, p, nil
	}
	base, params := url[:start+q], url[start+q+1:]
	var keep []string
	for kv := range strings.SplitSeq(params, "&") {
		k, v, _ := strings.Cut(kv, "=")
		set, ok := poolParams[k]
		if !ok {
			if kv != "" {
				keep = append(keep, kv)
			}
			continue
		}
		if err := set(&p, v); err != nil {
			return "", p, fmt.Errorf("orm: database URL parameter %s=%s: %v", k, v, err)
		}
	}
	if len(keep) > 0 {
		base += "?" + strings.Join(keep, "&")
	}
	return base, p, nil
}

// openSpec opens a declared database: its pool, supervised, and its
// replicas'.
func openSpec(sp *DatabaseSpec, log *slog.Logger) (*DB, error) {
	url, p, err := SplitPoolParams(sp.url)
	if err != nil {
		return nil, err
	}
	cfg := sp.cfg
	merged := databaseConfig{pool: p}
	cfg.pool.apply(&merged)
	pool := merged.pool.withDefaults()
	dialect, dsn, err := ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("orm: database %s: %w", sp.name, err)
	}
	sup := newSupervisor(sp.name, nil, pool, log)
	sup.slow, sup.timeout = cfg.slow, cfg.timeout
	db, err := openPool(sup, dialect, dsn, cfg)
	if err != nil {
		return nil, fmt.Errorf("orm: database %s: %w", sp.name, err)
	}
	sup.db = db
	for _, ru := range cfg.replicas {
		ru, _, err := SplitPoolParams(ru)
		if err != nil {
			return nil, err
		}
		rd, rdsn, err := ParseURL(ru)
		if err != nil {
			return nil, fmt.Errorf("orm: replica of %s: %w", sp.name, err)
		}
		if DialectFor(rd).Name() != DialectFor(dialect).Name() {
			return nil, fmt.Errorf("orm: replica %s of %s is %s, the database %s", redact(ru), sp.name, rd, dialect)
		}
		rdb, err := openPool(sup, rd, rdsn, cfg)
		if err != nil {
			return nil, fmt.Errorf("orm: replica of %s: %w", sp.name, err)
		}
		r := &replica{url: redact(ru), sql: rdb}
		r.healthy.Store(true)
		sup.replicas = append(sup.replicas, r)
	}
	d := Open(db, dialect)
	d.name, d.readOnly, d.sup = sp.name, cfg.readOnly, sup
	return d, nil
}

// openPool is a *sql.DB of dialect at dsn through the supervisor's
// connector, tuned by its Pool.
func openPool(sup *supervisor, dialect, dsn string, cfg databaseConfig) (*sql.DB, error) {
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
	if err := makeSQLiteDir(d.Name(), dsn); err != nil {
		return nil, err
	}
	probe, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	probe.Close()
	session, err := sessionSQL(d.Name(), cfg.session)
	if err != nil {
		return nil, err
	}
	if cfg.readOnly {
		session = append(session, readOnlySQL(d.Name()))
	}
	c := &connector{sup: sup, drv: drv, dsn: dsn, dialect: d.Name(), creds: cfg.creds, session: session}
	if c.creds == nil {
		if c.inner, err = openConnector(drv, dsn); err != nil {
			return nil, err
		}
	}
	db := sql.OpenDB(c)
	p := sup.pool
	db.SetMaxOpenConns(p.MaxOpen)
	db.SetMaxIdleConns(p.MaxIdle)
	db.SetConnMaxLifetime(p.MaxLifetime)
	db.SetConnMaxIdleTime(p.MaxIdleTime)
	if d.Name() == "sqlite" && (dsn == "" || strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")) {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
	}
	return db, nil
}

// Conn is a handle on one database of a Databases: embed it in a type
// of your own to inject that database by type (a framework's DI), and
// name it with @orm.Database("analytics") in Vuka or ConnFor in Go.
//
//	@orm.Database("analytics")
//	type Analytics struct{ orm.Conn }
//
//	rows := orm.Raw[Sales](a.With(ctx), "SELECT day, SUM(total) AS total FROM sales GROUP BY day")?
//
// A Conn is a context.Context too (background, with its database): pass
// it where a context goes, or With(ctx) for a request's.
type Conn struct {
	set  *Databases
	name string
}

// Conn is a handle on the database name.
func (s *Databases) Conn(name string) Conn { return Conn{set: s, name: name} }

// Name is the database's name.
func (c Conn) Name() string { return c.name }

// DB is the database.
func (c Conn) DB() (*DB, error) {
	if c.set == nil {
		return nil, errors.New("orm: a Conn of no Databases")
	}
	return c.set.Lookup(c.name)
}

// With is ctx with its queries sent to c's database (WithDatabases and
// Using).
func (c Conn) With(ctx context.Context) context.Context {
	if c.set != nil {
		ctx = WithDatabases(ctx, c.set)
	}
	return Using(ctx, c.name)
}

// Exec runs a write on c's database, as Exec does.
func (c Conn) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	return Exec(c.With(ctx), sql, args...)
}

// Ping checks c's database answers.
func (c Conn) Ping(ctx context.Context) error {
	d, err := c.DB()
	if err != nil {
		return err
	}
	return d.sql.PingContext(ctx)
}

// Stats is c's database's pool.
func (c Conn) Stats() (DBStats, error) {
	if c.set == nil {
		return DBStats{}, errors.New("orm: a Conn of no Databases")
	}
	return c.set.StatsOf(c.name)
}

func (Conn) Deadline() (time.Time, bool) { return time.Time{}, false }
func (Conn) Done() <-chan struct{}       { return nil }
func (Conn) Err() error                  { return nil }

func (c Conn) Value(key any) any {
	switch key.(type) {
	case databasesKey:
		if c.set != nil {
			return c.set
		}
	case usingKey:
		return c.name
	}
	return nil
}

type connHandle struct {
	t    reflect.Type // the struct embedding Conn
	name string
}

var handles struct {
	mu sync.Mutex
	by []connHandle
}

func addHandle(t reflect.Type, name string) {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("orm: a connection handle is a struct embedding orm.Conn; %s isn't one", t))
	}
	f, ok := t.FieldByName("Conn")
	if !ok || f.Type != reflect.TypeFor[Conn]() || !f.Anonymous {
		panic(fmt.Sprintf("orm: %s embeds no orm.Conn", t))
	}
	handles.mu.Lock()
	defer handles.mu.Unlock()
	handles.by = slices.DeleteFunc(handles.by, func(h connHandle) bool { return h.t == t })
	handles.by = append(handles.by, connHandle{t, name})
}

func connHandles() []connHandle {
	handles.mu.Lock()
	defer handles.mu.Unlock()
	return slices.Clone(handles.by)
}

// Database is a Vuka type decorator naming the database a connection
// handle (a struct embedding orm.Conn) is on; ormweb provides it, made
// from the app's Databases, to whatever takes a *T.
//
//	@orm.Database("analytics")
//	type Analytics struct{ orm.Conn }
func Database(name string) func(*vuka.Type) {
	return func(t *vuka.Type) { addHandle(t.Reflect, name) }
}

// ConnFor declares T (a struct embedding orm.Conn) the handle of the
// database name, as @orm.Database does, and returns its constructor from
// a Databases, for a DI container.
//
//	type Analytics struct{ orm.Conn }
//	app.Provide(orm.ConnFor[Analytics]("analytics"))
func ConnFor[T any](name string) func(*Databases) *T {
	addHandle(reflect.TypeFor[T](), name)
	return func(s *Databases) *T { return HandleOf[T](s) }
}

// HandleOf is the handle T on s, of the database T was declared for.
func HandleOf[T any](s *Databases) *T {
	v := MakeHandle(s, reflect.TypeFor[T]())
	return v.Interface().(*T)
}

// MakeHandle is a *T of the handle type t on s.
func MakeHandle(s *Databases, t reflect.Type) reflect.Value {
	name := ""
	for _, h := range connHandles() {
		if h.t == t {
			name = h.name
		}
	}
	v := reflect.New(t)
	v.Elem().FieldByName("Conn").Set(reflect.ValueOf(s.Conn(name)))
	return v
}

// Handles is the connection handle types declared (@orm.Database,
// ConnFor), for a framework to provide.
func Handles() []reflect.Type {
	var out []reflect.Type
	for _, h := range connHandles() {
		out = append(out, h.t)
	}
	return out
}
