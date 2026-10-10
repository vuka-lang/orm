// Package ormweb serves an orm.Databases in a github.com/vuka-lang/web
// app: the databases open when the app starts (migrated in development)
// and close gracefully when it stops; *orm.Databases and the connection
// handles (@orm.Database) are dependencies; each request's context
// carries the databases and a read-your-writes Scope; GET /healthz
// reports every database; a model or handle naming a database the app
// doesn't declare fails the app's Build.
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

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/web"
)

// Plugin is the web.Option Databases and Set return; its methods tune it
// before web.New applies it.
type Plugin struct {
	web.Option
	set     *orm.Databases
	opts    []orm.OpenOption
	migrate *bool // nil: VUKA_ENV=development when the databases open
	health  string
}

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
// (orm.Migrate); by default when VUKA_ENV=development as they open (so
// a .env file the app loads counts).
func (p *Plugin) MigrateOnStart(on bool) *Plugin { p.migrate = &on; return p }

// Health serves the databases' health at path (GET /healthz by default;
// "" serves none).
func (p *Plugin) Health(path string) *Plugin { p.health = path; return p }

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
	migrate := p.migrate
	s.Apply(orm.OnOpen(func(ctx context.Context, s *orm.Databases) error {
		if migrate == nil && os.Getenv("VUKA_ENV") != "development" || migrate != nil && !*migrate {
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
		h := HealthHandler(s)
		app.Get(p.health, func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })
	}
	app.Check(func(*web.App) error { return s.Check() })
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

// HealthHandler answers with s's Report as JSON: 200, or 503 when the
// default database is down.
func HealthHandler(s *orm.Databases) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := Check(s)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if rep.Status == "down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(rep)
		}
	})
}
