package multidbtest

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
)

func sqliteURL(t testing.TB, name string) string {
	return "sqlite:" + filepath.Join(t.TempDir(), name+".db")
}

// openWith opens main at mainURL with opts, the package's other
// databases as SQLite files.
func openWith(t testing.TB, mainURL string, opts ...orm.DatabaseOption) (*orm.Databases, context.Context) {
	t.Helper()
	decl := []orm.OpenOption{orm.Declare("main", mainURL, append(opts, orm.AsDefault())...)}
	for _, n := range names[1:] {
		decl = append(decl, orm.Declare(n, sqliteURL(t, n)))
	}
	s, err := orm.OpenDatabases(context.Background(), decl...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, orm.WithDatabases(context.Background(), s)
}

var outagePool = orm.Pool{Health: 100 * time.Millisecond, Breaker: orm.Breaker{Failures: 2, Cooldown: 300 * time.Millisecond}}

// stateOf is main's state in s.
func stateOf(s *orm.Databases) orm.State {
	st, _ := s.StatsOf("main")
	return st.State
}

// failsFast checks the breaker opened: statements fail Unavailable at
// once.
func failsFast(t testing.TB, s *orm.Databases, ctx context.Context) {
	t.Helper()
	eventually(t, 5*time.Second, "open", func() bool {
		_, err := Articles.Count(ctx)
		return errors.Is(err, orm.ErrUnavailable)
	})
	start := time.Now()
	_, err := Articles.Count(ctx)
	if !errors.Is(err, orm.ErrUnavailable) || !strings.Contains(err.Error(), "circuit breaker is open") || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("an open breaker: %v after %v", err, time.Since(start))
	}
	if st := stateOf(s); st != orm.Down && st != orm.Degraded {
		t.Fatalf("state %s", st)
	}
}

func recovers(t testing.TB, s *orm.Databases, ctx context.Context, want int64) {
	t.Helper()
	eventually(t, 15*time.Second, "back up", func() bool {
		n, err := Articles.Count(ctx)
		return err == nil && n == want && stateOf(s) == orm.Up
	})
}

func pgSchema(t testing.TB) (string, string) {
	t.Helper()
	dsn := os.Getenv("MULTIDB_PG_DSN")
	if dsn == "" {
		t.Skip("MULTIDB_PG_DSN: a Postgres to cut a proxy in front of")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := uniq("md_out_")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema, schema
}

// via is a Postgres URL through p.
func via(t testing.TB, dsn string, p *proxy) string {
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = p.addr
	return u.String()
}

func pgHost(t testing.TB, dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// TestBreakerPostgres cuts the proxy in front of Postgres: the breaker
// opens, statements fail fast; restored, the supervisor reconnects.
func TestBreakerPostgres(t *testing.T) {
	dsn, _ := pgSchema(t)
	p := newProxy(t, pgHost(t, dsn))
	var mu sync.Mutex
	var events []orm.StateEvent
	stop := orm.ObserveState(func(e orm.StateEvent) {
		if e.DB == "main" {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
	})
	defer stop()
	s, ctx := openWith(t, via(t, dsn, p), outagePool)
	if err := orm.CreateTables(orm.Using(ctx, "main"), Articles); err != nil {
		t.Fatal(err)
	}
	_ = Articles.Create(ctx, &Article{Title: "before"})
	p.cut()
	failsFast(t, s, ctx)
	p.restore(t)
	recovers(t, s, ctx, 1)
	mu.Lock()
	defer mu.Unlock()
	var path []string
	for _, e := range events {
		path = append(path, e.To.String())
	}
	if got := strings.Join(path, ","); !strings.HasPrefix(got, "up,") || !strings.Contains(got, "down") || !strings.HasSuffix(got, "up") {
		t.Fatalf("state changes %s", got)
	}
}

// TestReadRetry drops every pooled connection: the next read is retried
// on a fresh one, and succeeds.
func TestReadRetry(t *testing.T) {
	dsn, _ := pgSchema(t)
	p := newProxy(t, pgHost(t, dsn))
	s, ctx := openWith(t, via(t, dsn, p), orm.Pool{Health: time.Hour, MaxIdle: 4})
	if err := orm.CreateTables(orm.Using(ctx, "main"), Articles); err != nil {
		t.Fatal(err)
	}
	_ = Articles.Create(ctx, &Article{Title: "x"})
	before := p.connections()
	p.drop()
	if n, err := Articles.Count(ctx); err != nil || n != 1 {
		t.Fatalf("a read after the connections dropped: %d %v", n, err)
	}
	if p.connections() == before {
		t.Fatal("no fresh connection")
	}
	if st, _ := s.StatsOf("main"); st.Breaker != "closed" || st.State != orm.Up {
		t.Fatalf("one drop opened the breaker: %+v", st)
	}
}

// TestReplicaEjection: the replica (the same database behind a proxy)
// stops answering; reads fall back to the primary while it is ejected,
// and return to it once it answers.
func TestReplicaEjection(t *testing.T) {
	dsn, _ := pgSchema(t)
	p := newProxy(t, pgHost(t, dsn))
	var mu sync.Mutex
	replicaReads := 0
	stop := orm.Observe(func(_ context.Context, q orm.QueryInfo) {
		if q.Replica && strings.Contains(q.SQL, "articles") {
			mu.Lock()
			replicaReads++
			mu.Unlock()
		}
	})
	defer stop()
	s, ctx := openWith(t, dsn, orm.Replicas(via(t, dsn, p)), outagePool)
	if err := orm.CreateTables(orm.Using(ctx, "main"), Articles); err != nil {
		t.Fatal(err)
	}
	_ = Articles.Create(ctx, &Article{Title: "x"})
	reads := func() int { mu.Lock(); defer mu.Unlock(); return replicaReads }
	if n, err := Articles.Count(ctx); err != nil || n != 1 || reads() != 1 {
		t.Fatalf("read on the replica: %d %v (%d replica reads)", n, err, reads())
	}
	p.cut()
	for range 3 {
		if n, err := Articles.Count(ctx); err != nil || n != 1 {
			t.Fatalf("a read with the replica down: %d %v", n, err)
		}
	}
	st, _ := s.StatsOf("main")
	if st.Replicas[0].Healthy || st.State != orm.Degraded {
		t.Fatalf("replica not ejected: %+v", st)
	}
	n := reads()
	p.restore(t)
	eventually(t, 5*time.Second, "re-admitted", func() bool { st, _ := s.StatsOf("main"); return st.Replicas[0].Healthy && st.State == orm.Up })
	if _, err := Articles.Count(ctx); err != nil || reads() != n+1 {
		t.Fatalf("back on the replica: %v (%d replica reads)", err, reads())
	}
}

// mysqld is a throwaway MySQL server of the test's own, to kill.
type mysqld struct {
	bin, dir, sock, port string
	cmd                  *exec.Cmd
}

func newMySQLd(t testing.TB) *mysqld {
	t.Helper()
	bin, dir := os.Getenv("MULTIDB_MYSQLD"), os.Getenv("MULTIDB_MYSQLD_DIR")
	if bin == "" || dir == "" {
		t.Skip("MULTIDB_MYSQLD and MULTIDB_MYSQLD_DIR: a mysqld binary and a directory for the server the test kills")
	}
	sockDir, err := os.MkdirTemp("", "m.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	m := &mysqld{bin: bin, dir: dir, sock: filepath.Join(sockDir, "s"), port: port}
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		if out, err := exec.Command(bin, "--no-defaults", "--initialize-insecure", "--datadir="+filepath.Join(dir, "data")).CombinedOutput(); err != nil {
			t.Fatalf("initializing mysqld: %v\n%s", err, out)
		}
	}
	m.start(t)
	t.Cleanup(m.kill)
	return m
}

func (m *mysqld) start(t testing.TB) {
	t.Helper()
	m.cmd = exec.Command(m.bin, "--no-defaults", "--datadir="+filepath.Join(m.dir, "data"), "--port="+m.port, "--bind-address=127.0.0.1",
		"--socket="+m.sock, "--mysqlx=OFF", "--pid-file="+filepath.Join(m.dir, "mysqld.pid"), "--log-error="+filepath.Join(m.dir, "error.log"))
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "mysqld up", func() bool {
		db, err := sql.Open("mysql", "root@tcp(127.0.0.1:"+m.port+")/")
		if err != nil {
			return false
		}
		defer db.Close()
		return db.Ping() == nil
	})
}

func (m *mysqld) kill() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
		m.cmd = nil
	}
}

// TestBreakerMySQL kills the server: the breaker opens; restarted, the
// supervisor reconnects and statements run again.
func TestBreakerMySQL(t *testing.T) {
	m := newMySQLd(t)
	admin, err := sql.Open("mysql", "root@tcp(127.0.0.1:"+m.port+")/")
	if err != nil {
		t.Fatal(err)
	}
	db := uniq("md_")
	if _, err := admin.Exec("CREATE DATABASE " + db); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	s, ctx := openWith(t, "mysql://root@tcp(127.0.0.1:"+m.port+")/"+db, outagePool)
	if err := orm.CreateTables(orm.Using(ctx, "main"), Articles); err != nil {
		t.Fatal(err)
	}
	_ = Articles.Create(ctx, &Article{Title: "before"})
	m.kill()
	failsFast(t, s, ctx)
	m.start(t)
	recovers(t, s, ctx, 1)
}
