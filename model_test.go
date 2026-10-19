package orm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
)

// Customer, Group and Order are models of their own orm.Model, with no
// package-level manager: Customer has a "legacy" names set too.
type Customer struct {
	orm.Model[Customer]
	ID        int64
	Email     string `legacy:"mail"`
	Visits    int
	UpdatedAt time.Time
	Groups    []Group `gorm:"many2many:customer_groups;joinForeignKey:customer_id;joinReferences:group_id"`
	Orders    []Order
}

func (Customer) LegacyTableName() string { return "clients" }

type Group struct {
	orm.Model[Group]
	ID   int64
	Name string
}

func (Group) TableName() string { return "groups_unused" }

func (Group) Meta() orm.Meta { return orm.Meta{Table: "crowds", Ordering: []string{"name"}} }

type Order struct {
	orm.Model[Order]
	ID         int64
	CustomerID int64
	Customer   *Customer
	Total      int
}

func (Order) Meta() orm.Meta { return orm.Meta{Ordering: []string{"-total"}} }

func modelOpen(t *testing.T) context.Context {
	t.Helper()
	return ormtest.Open(t, Customer{}.Objects(), Group{}.Objects(), orm.Objects[Order]())
}

func emails(cs []Customer) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Email
	}
	return out
}

func TestModelSave(t *testing.T) {
	ctx := modelOpen(t)
	orm.Clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	t.Cleanup(func() { orm.Clock = time.Now })

	c := Customer{Email: "ali@x"}
	if err := c.Save(ctx); err != nil || c.ID == 0 {
		t.Fatalf("insert: id %d, %v", c.ID, err)
	}
	c.Email = "ali@y"
	if err := c.Save(ctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 1 {
		t.Fatalf("a loaded row's Save inserted: %d rows", n)
	}

	// Rows read through a query are loaded: Save updates them.
	all, err := Customer{}.Objects().All(ctx)
	if err != nil || len(all) != 1 || all[0].Email != "ali@y" {
		t.Fatalf("read back %v, %v", all, err)
	}
	got := all[0]
	got.Visits = 3
	if err := got.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 1 {
		t.Fatalf("a read row's Save inserted: %d rows", n)
	}

	// SaveFields writes the fields named, and auto_now.
	got.Visits, got.Email = 9, "ignored@x"
	orm.Clock = func() time.Time { return time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC) }
	if err := got.SaveFields(ctx, "visits"); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Visits != 9 || c.Email != "ali@y" || !c.UpdatedAt.Equal(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after SaveFields: %+v", c)
	}
	if err := (&Customer{Email: "new@x"}).SaveFields(ctx, "email"); err == nil || !strings.Contains(err.Error(), "not saved yet") {
		t.Fatalf("SaveFields of a new row: %v", err)
	}
	if err := got.SaveFields(ctx, "nope"); err == nil || !strings.Contains(err.Error(), `no field "nope"`) {
		t.Fatalf("SaveFields of no field: %v", err)
	}

	// Delete removes the row; the row is new again after.
	if err := c.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 0 {
		t.Fatalf("Delete left %d rows", n)
	}
	if err := c.Refresh(ctx); !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("Refresh of a deleted row: %v", err)
	}
	if err := c.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 1 {
		t.Fatalf("Save after Delete: %d rows", n)
	}

	// Get, First, Raw and GetOrCreate rows are loaded.
	g, err := Customer{}.Objects().Get(ctx, orm.Q{"id": c.ID})
	if err != nil {
		t.Fatal(err)
	}
	g.Visits = 1
	r, err := Customer{}.Objects().Raw("SELECT * FROM customers WHERE id = ?", c.ID).First(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.Visits = 2
	oc, created, err := Customer{}.Objects().GetOrCreate(ctx, orm.Q{"email": "o@x"}, nil)
	if err != nil || !created {
		t.Fatal(err)
	}
	oc.Visits = 5
	for _, row := range []*Customer{&g, &r, &oc} {
		if err := row.Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 2 {
		t.Fatalf("saving loaded rows: %d rows, want 2", n)
	}
}

func TestModelSchemaMemory(t *testing.T) {
	legacy := orm.DBSchema{Names: "legacy"}
	ctx := ormtest.Open(t, Customer{}.Objects(), Customer{}.Objects(legacy))

	// A row read on a schema writes back there, in its names.
	ormtest.Exec(t, ctx, "INSERT INTO clients (id, mail, visits, updated_at) VALUES (1, 'old@x', 0, '2026-01-01 00:00:00')")
	c, err := Customer{}.Objects(legacy).Get(ctx, orm.Q{"email": "old@x"})
	if err != nil {
		t.Fatal(err)
	}
	c.Email = "new@x"
	if err := c.Save(ctx); err != nil {
		t.Fatal(err)
	}
	mails, err := orm.Raw[string](ctx, "SELECT mail FROM clients")
	if err != nil || !slices.Equal(mails, []string{"new@x"}) {
		t.Fatalf("legacy table: %v, %v", mails, err)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 0 {
		t.Fatalf("the default table has %d rows", n)
	}

	// A row loaded from one schema and bound to another is new there:
	// Save copies it.
	if err := c.Using(orm.DBSchema{}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	// A new row is the model's own schema's, unless bound with Using.
	if err := (&Customer{Email: "home@x"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (&Customer{Email: "there@x"}).Using(legacy).Save(ctx); err != nil {
		t.Fatal(err)
	}
	home, _ := Customer{}.Objects().OrderBy("email").All(ctx)
	there, _ := Customer{}.Objects(legacy).OrderBy("email").All(ctx)
	if !slices.Equal(emails(home), []string{"home@x", "new@x"}) || !slices.Equal(emails(there), []string{"new@x", "there@x"}) {
		t.Fatalf("home %v, legacy %v", emails(home), emails(there))
	}
	if (Customer{}).Objects() != orm.Objects[Customer]() || (Customer{}).Objects(legacy) != orm.Objects[Customer](legacy) {
		t.Fatal("Objects made a second manager")
	}
}

func TestModelRelations(t *testing.T) {
	ctx := modelOpen(t)
	c := Customer{Email: "ali@x"}
	if err := c.Save(ctx); err != nil {
		t.Fatal(err)
	}
	var gs []*Group
	for _, n := range []string{"staff", "admins", "beta"} {
		g := &Group{Name: n}
		if err := g.Save(ctx); err != nil {
			t.Fatal(err)
		}
		gs = append(gs, g)
	}
	staff, admins, beta := gs[0], gs[1], gs[2]
	for _, total := range []int{10, 30, 20} {
		if err := (&Order{CustomerID: c.ID, Total: total}).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}

	names := func() []string {
		t.Helper()
		got, err := orm.Related[Group](&c, "groups").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(got))
		for i, g := range got {
			out[i] = g.Name
		}
		return out
	}
	// Add by row, pointer or key; adding twice links once.
	if err := c.Add(ctx, "groups", *staff, admins, admins.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(ctx, "groups", staff); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"admins", "staff"}) {
		t.Fatalf("after Add: %v (in Meta's order, by name)", got)
	}
	if err := c.Remove(ctx, "groups", admins); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"staff"}) {
		t.Fatalf("after Remove: %v", got)
	}
	if err := c.Set(ctx, "groups", beta, admins); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"admins", "beta"}) {
		t.Fatalf("after Set: %v", got)
	}
	if err := c.Add(ctx, "orders", 1); err == nil || !strings.Contains(err.Error(), "not many-to-many") {
		t.Fatalf("Add to a reverse foreign key: %v", err)
	}
	if err := (&Customer{}).Add(ctx, "groups", staff); err == nil || !strings.Contains(err.Error(), "not saved yet") {
		t.Fatalf("Add of a new row: %v", err)
	}

	// Related: rows holding its key, a foreign key's row, and the inverse
	// of a many-to-many no field holds.
	orders, err := orm.Related[Order](c, "orders").All(ctx)
	if err != nil || len(orders) != 3 || orders[0].Total != 30 {
		t.Fatalf("orders (by Meta's -total): %v, %v", orders, err)
	}
	owner, err := orm.Related[Customer](&orders[0], "customer").Get(ctx)
	if err != nil || owner.Email != "ali@x" {
		t.Fatalf("an order's customer: %v, %v", owner, err)
	}
	members, err := orm.Related[Customer](beta, "customers").All(ctx)
	if err != nil || !slices.Equal(emails(members), []string{"ali@x"}) {
		t.Fatalf("beta's customers: %v, %v", members, err)
	}
	if _, err := orm.Related[Order](&c, "nope").All(ctx); err == nil || !strings.Contains(err.Error(), `no relation "nope"`) {
		t.Fatalf("Related of no relation: %v", err)
	}
	if _, err := orm.Related[Group](&c, "orders").All(ctx); err == nil || !strings.Contains(err.Error(), "holds Order, not Group") {
		t.Fatalf("Related of the wrong model: %v", err)
	}

	// Load onto one row; the related rows load too: saving them updates.
	if err := c.Load(ctx, "groups", orm.Prefetch("orders", orm.Objects[Order]().Filter(orm.Q{"total__gte": 20}))); err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || len(c.Orders) != 2 {
		t.Fatalf("Load: %d groups, %d orders", len(c.Groups), len(c.Orders))
	}
	c.Groups[0].Name = "admins2"
	if err := c.Groups[0].Save(ctx); err != nil {
		t.Fatal(err)
	}
	withCustomer, err := orm.Objects[Order]().SelectRelated("customer").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withCustomer[0].Customer.Visits = 7
	if err := withCustomer[0].Customer.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := (Group{}).Objects().Count(ctx); n != 3 {
		t.Fatalf("saving a prefetched row inserted: %d groups", n)
	}
	if n, _ := (Customer{}).Objects().Count(ctx); n != 1 {
		t.Fatalf("saving a selected row inserted: %d customers", n)
	}
	if err := c.Refresh(ctx); err != nil || c.Visits != 7 || c.Groups != nil {
		t.Fatalf("Refresh: %+v, %v", c, err)
	}
}

func TestModelMeta(t *testing.T) {
	ctx := modelOpen(t)
	for _, n := range []string{"b", "c", "a"} {
		if err := (&Group{Name: n}).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := (Group{}).Objects().TableName(); got != "crowds" {
		t.Fatalf("Meta's table over TableName: %s", got)
	}
	names := func(qs orm.QuerySet[Group]) []string {
		t.Helper()
		got, err := qs.Values[string]("name").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := Group{}.Objects().QuerySet
	if got := names(all); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Meta's ordering: %v", got)
	}
	if got := names(all.OrderBy("-name")); !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("an order of its own: %v", got)
	}
	if got, err := all.First(ctx); err != nil || got.Name != "a" {
		t.Fatalf("First by Meta's ordering: %v, %v", got, err)
	}
	rows, err := all.All(ctx)
	if err != nil || rows[0].Name != "a" {
		t.Fatalf("All by Meta's ordering: %v, %v", rows, err)
	}
	// A grouped Values has no ordering it didn't ask for.
	n, err := all.Annotate("n", orm.Count("id")).Values[int64]("n").All(ctx)
	if err != nil || !slices.Equal(n, []int64{3}) {
		t.Fatalf("aggregate under Meta's ordering: %v, %v", n, err)
	}
}

func TestEmptyConditions(t *testing.T) {
	ctx := modelOpen(t)
	for _, n := range []string{"a", "b"} {
		if err := (&Group{Name: n}).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gs := Group{}.Objects()
	for name, qs := range map[string]orm.QuerySet[Group]{
		"Q{} then Q":   gs.Filter(orm.Q{}).Filter(orm.Q{"name": "a"}),
		"Q then And()": gs.Filter(orm.Q{"name": "a"}).Filter(orm.And()),
		"Or(Q{}, Q)":   gs.Filter(orm.Or(orm.Q{}, orm.Q{"name": "a"})),
		"Not(Q{}), Q":  gs.Filter(orm.Not(orm.Q{}), orm.Q{"name": "a"}),
		"Exclude(Q{})": gs.Exclude(orm.Q{}).Filter(orm.Q{"name": "a"}),
	} {
		got, err := qs.All(ctx)
		if err != nil || len(got) != 1 || got[0].Name != "a" {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}
}
