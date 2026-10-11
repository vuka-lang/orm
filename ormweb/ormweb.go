// Package ormweb serves an orm.Databases in a github.com/vuka-lang/web
// app: the databases open when the app starts (migrated in development)
// and close gracefully when it stops; *orm.Databases and the connection
// handles (@orm.Database) are dependencies; each request's context
// carries the databases and a read-your-writes Scope; GET /healthz
// reports every database's state (and, in development, its pool and
// errors); a model or handle naming a database the app doesn't declare
// fails the app's Build. In development, the app's
// toolbar lists each request's statements in its SQL panel.
//
//	app := web.New(
//		ormweb.Databases(
//			orm.Declare("main", os.Getenv("DATABASE_URL"), orm.AsDefault()),
//			orm.Declare("analytics", os.Getenv("ANALYTICS_URL"), orm.ReadOnly()),
//		).MigrateOnStart(true),
//		web.Use(ormweb.UsingFrom(tenantOf)), // a tenant's database per request
//	)
//	app.Run("")
package ormweb

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"sync"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/web"
)

// Plugin is the web.Option Databases and Set return; its methods tune it
// before web.New applies it.
type Plugin struct {
	web.Option
	set     *orm.Databases
	opts    []orm.OpenOption
	migrate *bool // nil: in development
	details *bool // nil: in development
	health  string
}

// devEnv is whether the program runs in development, by VUKA_ENV in its
// real environment: read before main, so an env file the app loads
// (.env) can't turn on development behaviour in production.
var devEnv = os.Getenv("VUKA_ENV") == "development"

// Databases serves the databases opts declare (orm.Declare), with their
// routers and settings (orm.Routers, orm.Logger, orm.OnOpen, …); with
// none declared, the app's start reads DATABASE_URL and
// DATABASE_URL_<NAME>.
func Databases(opts ...orm.OpenOption) *Plugin { return Set(nil, opts...) }

// Set serves a Databases made elsewhere (orm.NewDatabases), opts applied
// to it, opened by the app's start.
func Set(s *orm.Databases, opts ...orm.OpenOption) *Plugin {
	p := &Plugin{set: s, opts: opts, health: "/healthz"}
	p.Option = web.OptionFunc(p.install)
	return p
}

// MigrateOnStart applies pending migrations once the databases open
// (orm.Migrate). By default it does only in development: VUKA_ENV=development
// in the program's real environment, not an env file. Production
// migrates when this says so, or with the orm migrate command.
func (p *Plugin) MigrateOnStart(on bool) *Plugin { p.migrate = &on; return p }

// Health serves the databases' health at path (GET /healthz by default;
// "" serves none).
func (p *Plugin) Health(path string) *Plugin { p.health = path; return p }

// HealthDetails has the health route answer each database's full Report
// (pool statistics, breaker, last error, replicas) rather than its state
// alone. By default it does only in development (VUKA_ENV=development in
// the real environment): a driver's error text and replica addresses are
// for operators, not whoever reaches the route.
func (p *Plugin) HealthDetails(on bool) *Plugin { p.details = &on; return p }

// install provides the set (a supplied value: the app's Start opens it,
// its Stop closes it) and each connection handle type, puts the set and a
// Scope on every request's context, serves the health route, and makes
// the set's Check one of the app's Build checks.
func (p *Plugin) install(app *web.App) {
	s := p.set
	if s == nil {
		s = orm.NewDatabases()
	}
	s.Apply(p.opts...)
	migrate := devEnv
	if p.migrate != nil {
		migrate = *p.migrate
	}
	s.Apply(orm.OnOpen(func(ctx context.Context, s *orm.Databases) error {
		if !migrate {
			return nil
		}
		return orm.Migrate(orm.WithDatabases(ctx, s))
	}))
	app.Provide(s)
	for _, t := range orm.Handles() {
		fn := reflect.MakeFunc(reflect.FuncOf([]reflect.Type{reflect.TypeFor[*orm.Databases]()}, []reflect.Type{reflect.PointerTo(t)}, false),
			func(in []reflect.Value) []reflect.Value {
				return []reflect.Value{orm.MakeHandle(in[0].Interface().(*orm.Databases), t)}
			})
		app.Provide(fn.Interface())
	}
	app.Use(Middleware(s))
	if p.health != "" {
		details := devEnv
		if p.details != nil {
			details = *p.details
		}
		h := HealthHandler(s)
		if details {
			h = DetailedHealthHandler(s)
		}
		app.Get(p.health, func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })
	}
	app.Check(func(*web.App) error { return s.Check() })
	var stop func()
	app.OnStart(func(context.Context) error {
		if app.Toolbar() {
			stop = observeForToolbar()
		}
		return nil
	})
	app.OnStop(func(context.Context) error {
		if stop != nil {
			stop()
		}
		return nil
	})
}

// The development toolbar's SQL panel: while an app showing it runs, the
// ORM's statements are noted on the requests that ran them. One observer
// for the program, however many apps.
var toolbarObs struct {
	sync.Mutex
	apps int
	stop func()
}

func observeForToolbar() (stop func()) {
	toolbarObs.Lock()
	defer toolbarObs.Unlock()
	if toolbarObs.apps++; toolbarObs.apps == 1 {
		toolbarObs.stop = orm.Observe(func(ctx context.Context, q orm.QueryInfo) {
			web.NoteStatement(ctx, web.Statement{SQL: q.SQL, Args: q.Args, DB: q.DB, Start: q.Start, Duration: q.Duration,
				Rows: q.Rows, Err: q.Err, Refused: q.Refused})
		})
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			toolbarObs.Lock()
			defer toolbarObs.Unlock()
			if toolbarObs.apps--; toolbarObs.apps == 0 {
				toolbarObs.stop()
			}
		})
	}
}

// Middleware puts s and a read-your-writes Scope on each request's
// context.
func Middleware(s *orm.Databases) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(orm.Scope(orm.WithDatabases(r.Context(), s))))
		})
	}
}

// UsingFrom sends each request's queries to the database name gives for
// it (a tenant's, by host or header); "" leaves them routed as they are.
//
//	app.Use(ormweb.UsingFrom(func(r *http.Request) string { return "tenant_" + r.Header.Get("X-Tenant") }))
func UsingFrom(name func(*http.Request) string) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n := name(r); n != "" {
				r = r.WithContext(orm.Using(r.Context(), n))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Report is what the health endpoint answers: Status "ok", "degraded"
// (a database or replica isn't answering) or "down" (the default
// database isn't), and each database's stats.
type Report struct {
	Status    string        `json:"status"`
	Databases []orm.DBStats `json:"databases"`
}

// Check is s's health now.
func Check(s *orm.Databases) Report {
	rep := Report{Status: "ok", Databases: s.Stats()}
	if len(rep.Databases) == 0 {
		rep.Status = "down"
	}
	for _, d := range rep.Databases {
		switch {
		case d.Default && (d.State == orm.Down || d.State == orm.Connecting):
			rep.Status = "down"
		case d.State != orm.Up && rep.Status == "ok":
			rep.Status = "degraded"
		}
	}
	return rep
}

// Summary is a Report without the details: the status, and each
// database's state.
type Summary struct {
	Status    string    `json:"status"`
	Databases []DBState `json:"databases"`
}

// DBState is a database's state, as Summary reports it.
type DBState struct {
	Name    string    `json:"name"`
	Default bool      `json:"default"`
	State   orm.State `json:"state"`
}

// Summarize is rep without its details.
func Summarize(rep Report) Summary {
	sum := Summary{Status: rep.Status, Databases: make([]DBState, 0, len(rep.Databases))}
	for _, d := range rep.Databases {
		sum.Databases = append(sum.Databases, DBState{Name: d.Name, Default: d.Default, State: d.State})
	}
	return sum
}

// HealthHandler answers with s's Summary as JSON: 200, or 503 when the
// default database is down. It says nothing of errors, pools or
// replicas, so it can be public.
func HealthHandler(s *orm.Databases) http.Handler {
	return healthHandler(s, func(rep Report) any { return Summarize(rep) })
}

// DetailedHealthHandler answers with s's whole Report: the summary, and
// each database's pool, breaker, last error (the driver's text) and
// replicas (their redacted URLs). Serve it to operators only.
func DetailedHealthHandler(s *orm.Databases) http.Handler {
	return healthHandler(s, func(rep Report) any { return rep })
}

func healthHandler(s *orm.Databases, body func(Report) any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := Check(s)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if rep.Status == "down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(body(rep))
		}
	})
}
