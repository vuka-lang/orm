package multidbtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

type ctxKey struct{}

// eventsTo sends Event writes to the database the context names.
type eventsTo struct{ orm.BaseRouter }

func (eventsTo) WriteDB(ctx context.Context, m orm.ModelInfo) string {
	if n, _ := ctx.Value(ctxKey{}).(string); m.Name == "Event" {
		return n
	}
	return ""
}

// TestPrecedence: Using > routers > Meta > Route > the default.
func TestPrecedence(t *testing.T) {
	both := []orm.AnyManager{Articles, Events}
	s, ctx := open(t, map[string][]orm.AnyManager{"main": both, "analytics": both}, orm.Routers(eventsTo{}))
	create := func(ctx context.Context) {
		t.Helper()
		if err := Events.Create(ctx, &Event{Kind: "k"}); err != nil {
			t.Fatal(err)
		}
		if err := Articles.Create(ctx, &Article{Title: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	want := func(ev, ea, am, aa int64) {
		t.Helper()
		got := [4]int64{count(t, ctx, "main", Events), count(t, ctx, "analytics", Events), count(t, ctx, "main", Articles), count(t, ctx, "analytics", Articles)}
		if got != [4]int64{ev, ea, am, aa} {
			t.Fatalf("events main/analytics, articles main/analytics = %v, want %v", got, [4]int64{ev, ea, am, aa})
		}
	}
	create(ctx) // Meta sends events to analytics; articles go to the default
	want(0, 1, 1, 0)
	s.Route("articles", orm.Route{DB: "analytics"})
	s.Route("events", orm.Route{DB: "main"}) // Meta wins over a Route
	create(ctx)
	want(0, 2, 1, 1)
	create(context.WithValue(ctx, ctxKey{}, "main")) // a router wins over Meta
	want(1, 2, 1, 2)
	create(orm.Using(context.WithValue(ctx, ctxKey{}, "main"), "analytics")) // Using wins over all
	want(1, 3, 1, 3)
	if n, err := Events.Count(ctx); err != nil || n != 3 {
		t.Fatalf("events read from analytics: %d %v", n, err)
	}
}

// TestReadYourWrites: Note reads replica and writes main; a Scope that
// wrote it, a transaction and Primary read main.
func TestReadYourWrites(t *testing.T) {
	_, ctx := open(t, map[string][]orm.AnyManager{"main": {Notes}, "replica": {Notes}})
	if err := Notes.Create(ctx, &Note{Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := Notes.Count(ctx); n != 0 {
		t.Fatalf("read the write database outside a scope: %d", n)
	}
	if n, _ := Notes.Count(orm.Primary(ctx)); n != 1 {
		t.Fatalf("Primary: %d", n)
	}
	sc := orm.Scope(ctx)
	if n, _ := Notes.Count(sc); n != 0 {
		t.Fatalf("a scope that wrote nothing reads the replica: %d", n)
	}
	if err := Notes.Create(sc, &Note{Body: "y"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := Notes.Count(sc); n != 2 {
		t.Fatalf("read after write in a scope: %d", n)
	}
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		if n, _ := Notes.Count(ctx); n != 2 {
			t.Errorf("read in a transaction: %d", n)
		}
		return nil
	})

	_, off := open(t, map[string][]orm.AnyManager{"main": {Notes}, "replica": {Notes}}, orm.ReadYourWrites(false))
	sc = orm.Scope(off)
	if err := Notes.Create(sc, &Note{Body: "z"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := Notes.Count(sc); n != 0 {
		t.Fatalf("ReadYourWrites(false) still pins: %d", n)
	}
}

// TestReplicas: a database's reads go to its replica (here a database
// apart, to tell them), its writes and a scope's reads after one to it.
func TestReplicas(t *testing.T) {
	replicaURL := dbURL(t, "replica_of_main")
	var mu sync.Mutex
	var onReplica, onPrimary int
	stop := orm.Observe(func(ctx context.Context, q orm.QueryInfo) {
		if q.DB != "main" || !strings.Contains(q.SQL, "articles") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if q.Replica {
			onReplica++
		} else {
			onPrimary++
		}
	})
	defer stop()
	s, ctx := open(t, map[string][]orm.AnyManager{"main": {Articles}},
		orm.Declare("main", dbURL(t, "main"), orm.AsDefault(), orm.Replicas(replicaURL)))
	if err := orm.CreateTables(orm.WithDB(ctx, mustOpen(t, replicaURL)), Articles); err != nil {
		t.Fatal(err)
	}
	if err := orm.CreateTables(orm.Primary(orm.Using(ctx, "main")), Articles); err != nil {
		t.Fatal(err)
	}
	if err := Articles.Create(ctx, &Article{Title: "on the primary"}); err != nil {
		t.Fatal(err)
	}
	if n, err := Articles.Count(ctx); err != nil || n != 0 {
		t.Fatalf("a read went to the primary: %d %v", n, err)
	}
	sc := orm.Scope(ctx)
	_ = Articles.Create(sc, &Article{Title: "two"})
	if n, _ := Articles.Count(sc); n != 2 {
		t.Fatalf("a scope's read after its write: %d", n)
	}
	mu.Lock()
	if onReplica != 1 || onPrimary < 3 {
		t.Errorf("observed replica %d, primary %d", onReplica, onPrimary)
	}
	mu.Unlock()
	st, err := s.StatsOf("main")
	if err != nil || len(st.Replicas) != 1 || !st.Replicas[0].Healthy || st.State != orm.Up {
		t.Fatalf("stats %+v %v", st, err)
	}
}

func mustOpen(t testing.TB, url string) *orm.DB {
	t.Helper()
	d, err := orm.ConnectURL(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.SQL().Close() })
	return d
}

// TestMirrorMeta: Meta.Mirror repeats writes on the mirror after they
// commit.
func TestMirrorMeta(t *testing.T) {
	_, ctx := open(t, map[string][]orm.AnyManager{"main": {Ledgers}, "archive": {Ledgers}})
	if err := orm.Atomic(ctx, func(ctx context.Context) error {
		return Ledgers.Create(ctx, &Ledger{Amount: 5})
	}); err != nil {
		t.Fatal(err)
	}
	if count(t, ctx, "main", Ledgers) != 1 || count(t, ctx, "archive", Ledgers) != 1 {
		t.Fatal("not mirrored")
	}
	_ = orm.Atomic(ctx, func(ctx context.Context) error {
		_ = Ledgers.Create(ctx, &Ledger{Amount: 6})
		return errors.New("rolled back")
	})
	if count(t, ctx, "archive", Ledgers) != 1 {
		t.Fatal("a rolled back write reached the mirror")
	}
}

// TestCheckUndeclared: Start refuses a set lacking a database a model
// names.
func TestCheckUndeclared(t *testing.T) {
	var opts []orm.OpenOption
	for _, n := range names {
		if n != "crm" {
			opts = append(opts, orm.Declare(n, dbURL(t, n)))
		}
	}
	_, err := orm.OpenDatabases(context.Background(), opts...)
	if err == nil || !strings.Contains(err.Error(), `Customer's Meta.DB names database "crm", which isn't declared`) {
		t.Fatalf("%v", err)
	}
}

// TestCrossDB: Order (main) holds a Customer (crm) by key: prefetched
// from crm, never joined.
func TestCrossDB(t *testing.T) {
	_, ctx := open(t, map[string][]orm.AnyManager{"main": {Orders}, "crm": {Customers}})
	ada := Customer{Name: "Ada"}
	if err := Customers.Create(ctx, &ada); err != nil {
		t.Fatal(err)
	}
	for _, total := range []int{10, 20} {
		if err := Orders.Create(ctx, &Order{Total: total, CustomerID: ada.ID}); err != nil {
			t.Fatal(err) // no foreign key: the customer is on another database
		}
	}
	orders, err := Orders.OrderBy("total").PrefetchRelated("customer").All(ctx)
	if err != nil || len(orders) != 2 || orders[0].Customer == nil || orders[0].Customer.Name != "Ada" {
		t.Fatalf("prefetch across databases: %+v %v", orders, err)
	}
	if n, err := Orders.Filter(orm.Q{"customer__id": ada.ID}).Count(ctx); err != nil || n != 2 {
		t.Fatalf("filter by the key: %d %v", n, err)
	}
	if n, err := Orders.Filter(orm.Q{"customer": ada.ID}).Count(ctx); err != nil || n != 2 {
		t.Fatalf("filter by the relation: %d %v", n, err)
	}
	for name, q := range map[string]orm.QuerySet[Order]{
		"filter":        Orders.Filter(orm.Q{"customer__name": "Ada"}),
		"order":         Orders.OrderBy("customer__name"),
		"SelectRelated": Orders.SelectRelated("customer"),
	} {
		if _, err := q.All(ctx); err == nil || !strings.Contains(err.Error(), "Order.Customer is CrossDB") {
			t.Errorf("%s through a CrossDB relation: %v", name, err)
		}
	}
	if _, err := Customers.Filter(orm.Q{"orders__total__gt": 5}).All(ctx); err == nil || !strings.Contains(err.Error(), "is CrossDB") {
		t.Errorf("through the inverse: %v", err)
	}
	var row Order
	row, err = Orders.Get(ctx, orm.Q{"total": 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := row.Load(ctx, "customer"); err != nil || row.Customer == nil || row.Customer.ID != ada.ID {
		t.Fatalf("load: %+v %v", row.Customer, err)
	}
}
