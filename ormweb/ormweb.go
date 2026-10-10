// Package ormweb serves an orm.Databases in a github.com/vuka-lang/web
// app: the databases open when the app starts (migrated in development)
// and close gracefully when it stops; *orm.Databases and the connection
// handles (@orm.Database) are dependencies; each request's context
// carries the databases and a read-your-writes Scope; GET /healthz
// reports every database.
//
//	app := web.New()
//	if err := ormweb.Install(app, ormweb.Databases(
//		orm.Declare("main", os.Getenv("DATABASE_URL"), orm.AsDefault()),
//		orm.Declare("analytics", os.Getenv("ANALYTICS_URL"), orm.ReadOnly()),
//	)); err != nil {
//		log.Fatal(err)
//	}
//	app.Use(ormweb.UsingFrom(tenantOf)) // a tenant's database per request
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

// Option tunes Install.
type Option func(*plugin)

type plugin struct {
	set     *orm.Databases
	opts    []orm.OpenOption
	migrate bool
	health  string
}

// Databases declares the app's databases (orm.Declare) and their routers
// and settings (orm.Routers, orm.Logger, …); with none, Start reads
// DATABASE_URL and DATABASE_URL_<NAME>.
func Databases(opts ...orm.OpenOption) Option {
	return func(p *plugin) { p.opts = append(p.opts, opts...) }
}

// Set serves a Databases made elsewhere (orm.NewDatabases), opened by the
// app's start.
func Set(s *orm.Databases) Option { return func(p *plugin) { p.set = s } }

// MigrateOnStart applies pending migrations once the databases open
// (orm.Migrate); on by default when VUKA_ENV=development.
func MigrateOnStart(on bool) Option { return func(p *plugin) { p.migrate = on } }

// Health serves the databases' health at path (GET /healthz by default).
func Health(path string) Option { return func(p *plugin) { p.health = path } }

// Install adds the databases to app: provided as *orm.Databases (opened
// by the app's Start, closed by its Stop) and as each connection handle
// type, put on every request's context with a Scope, and their health
// served. Its error is the databases' Check: a model, route or handle
// naming a database the app doesn't declare. Call it before the app is
// built.
func Install(app *web.App, opts ...Option) error {
	p := &plugin{migrate: os.Getenv("VUKA_ENV") == "development", health: "/healthz"}
	for _, o := range opts {
		o(p)
	}
	if p.set == nil {
		p.set = orm.NewDatabases()
	}
	s := p.set.Apply(p.opts...)
	if p.migrate {
		s.Apply(orm.OnOpen(func(ctx context.Context, s *orm.Databases) error {
			return orm.Migrate(orm.WithDatabases(ctx, s))
		}))
	}
	err := s.Check()
	app.Provide(func() *orm.Databases { return s })
	for _, t := range orm.Handles() {
		pt := reflect.PointerTo(t)
		fn := reflect.MakeFunc(reflect.FuncOf([]reflect.Type{reflect.TypeFor[*orm.Databases]()}, []reflect.Type{pt}, false),
			func(in []reflect.Value) []reflect.Value {
				return []reflect.Value{orm.MakeHandle(in[0].Interface().(*orm.Databases), t)}
			})
		app.Provide(fn.Interface())
	}
	health := HealthHandler(s)
	app.Use(func(next http.Handler) http.Handler {
		mw := Middleware(s)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p.health != "" && r.URL.Path == p.health && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				health.ServeHTTP(w, r)
				return
			}
			mw.ServeHTTP(w, r)
		})
	})
	if p.health != "" {
		// The route needs the databases, so the app builds, starts and
		// stops them; the middleware above answers it.
		app.Get(p.health, func(*orm.Databases) error { return web.NotFound })
	}
	return err
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

