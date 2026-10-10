package ormweb_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/migration"
	"github.com/vuka-lang/orm/ormweb"
	_ "github.com/vuka-lang/orm/postgres"
	_ "github.com/vuka-lang/orm/sqlite"
	"github.com/vuka-lang/web"
)

type Visit struct {
	orm.Model[Visit]
	ID   int64
	Path string
}

func (Visit) Meta() orm.Meta { return orm.Meta{DB: "analytics"} }

var Visits = orm.For[Visit]()

type Reports struct{ orm.Conn }

var _ = orm.ConnFor[Reports]("analytics")

func file(t *testing.T, name string) string {
	return "sqlite:" + filepath.Join(t.TempDir(), name+".db")
}

// app is a web app with the databases and a route counting visits.
func app(t *testing.T, dbs *ormweb.Plugin) (*web.App, *httptest.Server) {
	t.Helper()
	a := web.New(web.Only("none"), dbs, web.Use(ormweb.UsingFrom(func(r *http.Request) string { return r.Header.Get("X-Tenant") })))
	a.Get("/visits", func(ctx context.Context) (int64, error) {
		if err := Visits.Create(ctx, &Visit{Path: "/visits"}); err != nil {
			return 0, err
		}
		return Visits.Count(ctx) // read-your-writes: the scope wrote visits
	})
	a.Get("/report", func(ctx context.Context, r *Reports, dbs *orm.Databases) (string, error) {
		n, err := orm.RawRow[int64](r.With(ctx), "SELECT COUNT(*) FROM visits")
		return r.Name() + ":" + string(rune('0'+n)), err
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return a, srv
}

func get(t *testing.T, srv *httptest.Server, path string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestApp(t *testing.T) {
	a, srv := app(t, ormweb.Databases(
		orm.Declare("main", file(t, "main"), orm.AsDefault()),
		orm.Declare("analytics", file(t, "analytics")),
		orm.Declare("tenant_a", file(t, "tenant_a")),
	))
	dbs, err := web.Resolve[*orm.Databases](a)
	if err != nil {
		t.Fatal(err)
	}
	ctx := orm.WithDatabases(context.Background(), dbs)
	for _, n := range []string{"analytics", "tenant_a"} {
		if err := orm.CreateTables(orm.Using(ctx, n), Visits); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := get(t, srv, "/visits"); code != 200 || strings.TrimSpace(body) != "1" {
		t.Fatalf("visits: %d %s", code, body)
	}
	for _, want := range []string{"1", "2"} {
		if code, body := get(t, srv, "/visits", "X-Tenant", "tenant_a"); code != 200 || strings.TrimSpace(body) != want {
			t.Fatalf("a tenant's visits: %d %s, want %s", code, body, want)
		}
	}
	if code, body := get(t, srv, "/report"); code != 200 || body != `"analytics:1"`+"\n" {
		t.Fatalf("report through the injected handle: %d %q", code, body)
	}
	code, body := get(t, srv, "/healthz")
	var rep ormweb.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil || code != 200 || rep.Status != "ok" || len(rep.Databases) != 3 {
		t.Fatalf("health: %d %s", code, body)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := dbs.Names(); len(n) != 0 {
		t.Fatalf("open after Stop: %v", n)
	}
}

// TestHealthDown: the default database not answering is a 503.
func TestHealthDown(t *testing.T) {
	_, srv := app(t, ormweb.Databases(
		orm.Declare("main", "postgres://nobody@127.0.0.1:1/x?sslmode=disable&startup_wait=100ms&pool_health=1h", orm.AsDefault()),
		orm.Declare("analytics", file(t, "analytics")),
	))
	code, body := get(t, srv, "/healthz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"down"`) || !strings.Contains(body, `"state":"down"`) {
		t.Fatalf("%d %s", code, body)
	}
}

// TestUndeclared: a model or handle naming a database the app doesn't
// declare fails the app's Build, with web's own errors.
func TestUndeclared(t *testing.T) {
	a := web.New(web.Only("none"), ormweb.Databases(orm.Declare("main", file(t, "main"), orm.AsDefault())))
	a.Get("/x", func(*NoProvider) string { return "" })
	err := a.Build()
	for _, want := range []string{`Visit's Meta.DB names database "analytics"`, "connection handle", "no provider for *ormweb_test.NoProvider"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Build: %v; want %q", err, want)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Start(ctx); err == nil || !strings.Contains(err.Error(), `"analytics"`) {
		t.Fatalf("start: %v", err)
	}
}

type NoProvider struct{}

func init() {
	migration.Register(migration.Migration{Name: "0001_marker", DB: "analytics",
		Operations: []migration.Operation{migration.RunSQL{SQL: "CREATE TABLE marker (id INTEGER)", ReverseSQL: "DROP TABLE marker"}}})
}

// TestOptions: MigrateOnStart migrates as the databases open, Health
// moves the route, and Set serves a set made elsewhere; the app's Stop
// closes it.
func TestOptions(t *testing.T) {
	set := orm.NewDatabases(orm.Declare("analytics", file(t, "analytics")))
	a, srv := app(t, ormweb.Set(set, orm.Declare("main", file(t, "main"), orm.AsDefault())).MigrateOnStart(true).Health("/up"))
	if code, _ := get(t, srv, "/healthz"); code != http.StatusNotFound {
		t.Fatalf("/healthz: %d", code)
	}
	if code, body := get(t, srv, "/up"); code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("/up: %d %s", code, body)
	}
	if got, err := web.Resolve[*orm.Databases](a); err != nil || got != set {
		t.Fatalf("Resolve: %v %v", got == set, err)
	}
	if n := set.Names(); len(n) != 2 {
		t.Fatalf("open: %v", n)
	}
	if n, err := orm.RawRow[int64](set.Conn("analytics").With(context.Background()), "SELECT COUNT(*) FROM marker"); err != nil || n != 0 {
		t.Fatalf("migrated on start: %d %v", n, err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := set.Names(); len(n) != 0 {
		t.Fatalf("open after Stop: %v", n)
	}
}

// TestToolbar: a development app's toolbar lists the ORM's statements
// under the request that ran them, an N+1 named.
func TestToolbar(t *testing.T) {
	t.Setenv(web.DevDirEnv, t.TempDir()) // as vuka run: the toolbar is on, in a test too
	a := web.New(web.Only("none"), web.Dev(), ormweb.Databases(
		orm.Declare("main", "sqlite::memory:", orm.AsDefault()),
		orm.Declare("analytics", "sqlite::memory:"),
	))
	a.Get("/visits", func(ctx context.Context) (int64, error) {
		for i := range 3 {
			if _, err := orm.RawRow[int64](orm.Using(ctx, "analytics"), "SELECT COUNT(*) FROM visits WHERE id = ?", i); err != nil { // one row at a time: an N+1
				return 0, err
			}
		}
		return Visits.Count(ctx)
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	if !a.Toolbar() {
		t.Fatal("no toolbar")
	}
	dbs, _ := web.Resolve[*orm.Databases](a)
	if err := orm.CreateTables(orm.Using(orm.WithDatabases(context.Background(), dbs), "analytics"), Visits); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/visits")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	id := res.Header.Get(web.ToolbarHeader)
	code, body := get(t, srv, "/_vuka/toolbar/r/"+id+"?panels")
	var rec struct {
		Queries int    `json:"queries"`
		HTML    string `json:"html"`
	}
	if err := json.Unmarshal([]byte(body), &rec); err != nil || code != 200 {
		t.Fatalf("the record: %d %s", code, body)
	}
	if rec.Queries != 4 || !strings.Contains(rec.HTML, "<b>N+1</b>") || !strings.Contains(rec.HTML, "visits") || !strings.Contains(rec.HTML, "on analytics") {
		t.Errorf("the SQL panel (%d statements): %s", rec.Queries, rec.HTML)
	}
}
