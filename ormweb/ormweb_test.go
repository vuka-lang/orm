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

func file(t *testing.T, name string) string { return "sqlite:" + filepath.Join(t.TempDir(), name+".db") }

// app is a web app with the databases and a route counting visits.
func app(t *testing.T, opts ...ormweb.Option) (*web.App, *httptest.Server) {
	t.Helper()
	a := web.New(web.Only("none"))
	if err := ormweb.Install(a, opts...); err != nil {
		t.Fatal(err)
	}
	a.Use(ormweb.UsingFrom(func(r *http.Request) string { return r.Header.Get("X-Tenant") }))
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

// TestInstallUndeclared: a model naming a database the app doesn't
// declare fails Install, and the app's start.
func TestInstallUndeclared(t *testing.T) {
	a := web.New(web.Only("none"))
	err := ormweb.Install(a, ormweb.Databases(orm.Declare("main", file(t, "main"), orm.AsDefault())))
	if err == nil || !strings.Contains(err.Error(), `Visit's Meta.DB names database "analytics"`) || !strings.Contains(err.Error(), "connection handle") {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Start(ctx); err == nil || !strings.Contains(err.Error(), "isn't declared") {
		t.Fatalf("start: %v", err)
	}
}
