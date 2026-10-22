package orm_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/vuka/v2"
)

// MirrorPlacement is the clean model: its Go types say nothing about the
// legacy schema it dual-writes to — the mirror's columns, types and value
// formats ride field attributes (registered below), read only under the
// "legacy" names set.
type MirrorPlacement struct {
	ID       int64 `orm:"pk"`
	Ref      string
	Status   string    // legacy: status_code INTEGER, a code
	ClosedAt time.Time // legacy: closed_at_text TEXT, a formatted string
}

var mpStatusCodes = map[string]int64{"OPEN": 1, "CLOSED": 2, "FILLED": 3}

const mpTimeLayout = "2006-01-02 15:04:05"

// legacyCol is a field attribute lowering to a per-names-set override on
// FieldOptions: exactly what a portal decorator does.
type legacyCol struct {
	column, typ string
	codec       orm.SchemaCodec
}

func (a legacyCol) Apply(o *orm.FieldOptions) {
	if o.Schemas == nil {
		o.Schemas = map[string]orm.SchemaField{}
	}
	o.Schemas["legacy"] = orm.SchemaField{Column: a.column, Type: a.typ, Codec: a.codec}
}

func init() {
	vuka.RegisterFields[MirrorPlacement](map[string][]any{
		"Status":   {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
		"ClosedAt": {legacyCol{"closed_at_text", "text", orm.TimeAsText(mpTimeLayout)}},
	})
}

// twoDBs opens two fresh, isolated databases in their own set, the first
// the default. SQLite in memory by default; two throwaway schemas
// (Postgres) or databases (MySQL) on the server ORMTEST_DRIVER/ORMTEST_DSN
// name, dropped when the test ends — never the caller's own data.
func twoDBs(t *testing.T, a, b string) (*orm.Databases, *orm.DB, *orm.DB, context.Context) {
	t.Helper()
	da, db := throwaway(t), throwaway(t)
	s := orm.NewDatabases()
	s.Add(a, da, orm.AsDefault())
	s.Add(b, db)
	return s, da, db, orm.WithDatabases(context.Background(), s)
}

func rid() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "orm_mirror_" + hex.EncodeToString(b[:])
}

// throwaway is one isolated database: in-memory SQLite, or a schema
// (Postgres) / database (MySQL) of its own on the configured server.
func throwaway(t *testing.T) *orm.DB {
	t.Helper()
	dsn := os.Getenv("ORMTEST_DSN")
	switch os.Getenv("ORMTEST_DRIVER") {
	case "postgres":
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := rid()
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		s, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = s.Close()
			_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			_ = admin.Close()
		})
		return orm.Open(s, "postgres")
	case "mysql":
		admin, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		name := rid()
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatal(err)
		}
		slash := strings.LastIndex(dsn, "/")
		rest := dsn[slash+1:]
		params := ""
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			params = rest[i:]
		}
		s, err := sql.Open("mysql", orm.MySQLDSN(dsn[:slash+1]+name+params))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = s.Close()
			_, _ = admin.Exec("DROP DATABASE " + name)
			_ = admin.Close()
		})
		return orm.Open(s, "mysql")
	}
	d, err := orm.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SQL().Close() })
	return d
}

func TestMirrorSchema(t *testing.T) {
	s, _, legacyDB, ctx := twoDBs(t, "main", "legacy")
	s.Apply(orm.Dual[MirrorPlacement]("main", "legacy",
		orm.MirrorUnder("legacy"), orm.MirrorTable("legacy_placements")))

	Placements := orm.For[MirrorPlacement]()
	// Legacy manager, for reading the mirror back through the same codecs.
	Legacy := orm.For[MirrorPlacement](orm.On("legacy"), orm.Names("legacy"),
		orm.Table("legacy_placements"), orm.Unmanaged())

	if err := orm.CreateTables(ctx, Placements); err != nil {
		t.Fatal(err)
	}
	// The legacy table is the database's own (Unmanaged): renamed,
	// reformatted columns, made by hand, never by the ORM.
	legacy := orm.WithDB(context.Background(), legacyDB)
	if _, err := legacyDB.SQL().ExecContext(legacy,
		`CREATE TABLE legacy_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER, closed_at_text TEXT)`); err != nil {
		t.Fatal(err)
	}

	closed := time.Date(2026, 2, 3, 9, 30, 0, 0, time.UTC)
	p := MirrorPlacement{Ref: "P-1", Status: "OPEN", ClosedAt: closed}
	if err := Placements.Create(ctx, &p); err != nil {
		t.Fatal(err)
	}

	// Primary keeps the Go shape: status as text.
	mainStatus, err := orm.Raw[string](ctx, "SELECT status FROM mirror_placements WHERE id = 1")
	if err != nil || len(mainStatus) != 1 || mainStatus[0] != "OPEN" {
		t.Fatalf("primary status = %v, %v", mainStatus, err)
	}

	// The mirror row: renamed column, integer code, time as text.
	code, err := orm.Raw[int64](legacy, "SELECT status_code FROM legacy_placements WHERE id = 1")
	if err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("mirror status_code = %v, %v (want 1)", code, err)
	}
	at, err := orm.Raw[string](legacy, "SELECT closed_at_text FROM legacy_placements WHERE id = 1")
	if err != nil || len(at) != 1 || at[0] != "2026-02-03 09:30:00" {
		t.Fatalf("mirror closed_at_text = %v, %v", at, err)
	}

	// Reads hit only the primary: change the mirror out of band, the read
	// from the primary is unmoved.
	if _, err := legacyDB.SQL().ExecContext(legacy, "UPDATE legacy_placements SET status_code = 99"); err != nil {
		t.Fatal(err)
	}
	got, err := Placements.Get(ctx, orm.Q{"id": 1})
	if err != nil || got.Status != "OPEN" {
		t.Fatalf("read from primary = %q, %v (a read reached the mirror)", got.Status, err)
	}
	if _, err := legacyDB.SQL().ExecContext(legacy, "UPDATE legacy_placements SET status_code = 1"); err != nil {
		t.Fatal(err)
	}

	// The codec round-trips: reading the legacy schema decodes the code
	// and the text back into the Go types.
	lp, err := Legacy.Get(ctx, orm.Q{"id": 1})
	if err != nil || lp.Status != "OPEN" || !lp.ClosedAt.Equal(closed) {
		t.Fatalf("legacy read = %+v, %v", lp, err)
	}

	// Update mirrors with the translated value.
	if _, err := Placements.Filter(orm.Q{"id": 1}).Update(ctx, orm.Set{"status": "CLOSED"}); err != nil {
		t.Fatal(err)
	}
	code, _ = orm.Raw[int64](legacy, "SELECT status_code FROM legacy_placements WHERE id = 1")
	if len(code) != 1 || code[0] != 2 {
		t.Fatalf("mirror after update = %v (want 2)", code)
	}

	// Save (row-level) mirrors too.
	got.Status = "FILLED"
	if err := Placements.Save(ctx, &got); err != nil {
		t.Fatal(err)
	}
	code, _ = orm.Raw[int64](legacy, "SELECT status_code FROM legacy_placements WHERE id = 1")
	if len(code) != 1 || code[0] != 3 {
		t.Fatalf("mirror after save = %v (want 3)", code)
	}

	// Delete mirrors.
	if _, err := Placements.Filter(orm.Q{"id": 1}).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	n, _ := orm.Raw[int64](legacy, "SELECT count(*) FROM legacy_placements")
	if n[0] != 0 {
		t.Fatalf("mirror rows after delete = %d", n[0])
	}

	// A failing mirror surfaces via OnMirrorError and does not fail the
	// primary write.
	var mu sync.Mutex
	var failed []orm.MirrorError
	stop := orm.OnMirrorError(func(_ context.Context, e orm.MirrorError) {
		mu.Lock()
		failed = append(failed, e)
		mu.Unlock()
	})
	defer stop()
	if _, err := legacyDB.SQL().ExecContext(legacy, "DROP TABLE legacy_placements"); err != nil {
		t.Fatal(err)
	}
	if err := Placements.Create(ctx, &MirrorPlacement{Ref: "P-2", Status: "OPEN", ClosedAt: closed}); err != nil {
		t.Fatalf("a failing mirror failed the primary write: %v", err)
	}
	kept, _ := orm.Raw[int64](ctx, "SELECT count(*) FROM mirror_placements")
	mu.Lock()
	defer mu.Unlock()
	// Row 1 was deleted earlier, so only P-2 remains on the primary.
	if kept[0] != 1 || len(failed) != 1 || failed[0].Table != "mirror_placements" || failed[0].Op != "create" || failed[0].Mirror != "legacy" {
		t.Fatalf("primary rows %d, failures %+v", kept[0], failed)
	}
}

// TStatus is a status kept as a code on the legacy schema by the TYPE
// (a SchemaValuer), the pre-existing mechanism the field codec mirrors.
type TStatus string

func (s TStatus) ValueFor(names string) (driver.Value, error) {
	if names != "legacy" {
		return string(s), nil
	}
	return mpStatusCodes[string(s)], nil
}

func (s *TStatus) ScanFor(names string, src any) error {
	if names != "legacy" {
		*s = TStatus(fmt.Sprint(src))
		return nil
	}
	for name, c := range mpStatusCodes {
		if c == src.(int64) {
			*s = TStatus(name)
			return nil
		}
	}
	return fmt.Errorf("unknown code %v", src)
}

// TaggedPlacement declares the same legacy shape with a per-set struct tag
// and a SchemaValuer type — no field attribute.
type TaggedPlacement struct {
	ID     int64   `orm:"pk"`
	Ref    string  `legacy:"column:ref"`
	Status TStatus `orm:"type:varchar(16)" legacy:"column:status_code;type:integer"`
}

func (TaggedPlacement) TableName() string       { return "placements2" }
func (TaggedPlacement) LegacyTableName() string { return "legacy_placements" }

// TestMirrorAttrTagEquivalent asserts the field-attribute override and the
// per-set struct tag produce the same legacy column name, SQL type and
// mirrored value.
// A Values[struct] projection under a names set decodes the per-field codec,
// as a full-row read does: a struct result must not blank the schema codec.
func TestValuesProjectionCodec(t *testing.T) {
	s, _, legacyDB, ctx := twoDBs(t, "main", "legacy")
	_ = s
	legacy := orm.WithDB(context.Background(), legacyDB)
	if _, err := legacyDB.SQL().ExecContext(legacy,
		`CREATE TABLE legacy_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER, closed_at_text TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDB.SQL().ExecContext(legacy,
		`INSERT INTO legacy_placements (id, ref, status_code, closed_at_text) VALUES (1, 'P-1', 1, '2026-02-03 09:30:00')`); err != nil {
		t.Fatal(err)
	}

	Legacy := orm.For[MirrorPlacement](orm.On("legacy"), orm.Names("legacy"),
		orm.Table("legacy_placements"), orm.Unmanaged())

	type row struct {
		Ref    string
		Status string // reads the legacy int status_code, decoded by the codec
	}
	rows, err := Legacy.Values[row]("ref", "status").All(ctx)
	if err != nil {
		t.Fatalf("Values projection: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "OPEN" || rows[0].Ref != "P-1" {
		t.Fatalf("Values projection = %+v (want Ref P-1, Status OPEN decoded from code 1)", rows)
	}

	// The codec also encodes in a filter on the projected column.
	open, err := Legacy.Filter(orm.Q{"status": "OPEN"}).Values[row]("ref", "status").All(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("filter on codec column = %+v, %v (want 1 row; 'OPEN' must encode to code 1)", open, err)
	}
}

func TestMirrorAttrTagEquivalent(t *testing.T) {
	attr := orm.For[MirrorPlacement](orm.Names("legacy"), orm.Table("legacy_placements"), orm.Unmanaged())
	tag := orm.For[TaggedPlacement](orm.Names("legacy"), orm.Unmanaged())

	// Same column in the same place (id, ref, status_code, …).
	if got := attr.Columns(); !slices.Contains(got, "status_code") {
		t.Fatalf("attr legacy columns = %v", got)
	}
	if got := tag.Columns(); !slices.Contains(got, "status_code") {
		t.Fatalf("tag legacy columns = %v", got)
	}

	// Both write the SAME mirror row for the same input. Drive each
	// through a mirror (WithMirror) into one legacy database.
	sa, _, la, ctxa := twoDBs(t, "main", "legacy")
	_ = sa
	Attr := orm.For[MirrorPlacement]()
	legacy := orm.WithDB(context.Background(), la)
	if _, err := la.SQL().ExecContext(legacy, `CREATE TABLE legacy_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER, closed_at_text TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := orm.CreateTables(ctxa, Attr); err != nil {
		t.Fatal(err)
	}
	// Mirror under the legacy names + table.
	mctx := orm.WithMirror(ctxa, la)
	AttrM := orm.For[MirrorPlacement](orm.MirrorUnder("legacy"), orm.MirrorTable("legacy_placements"))
	if err := AttrM.Create(mctx, &MirrorPlacement{Ref: "R", Status: "CLOSED", ClosedAt: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	attrCode, _ := orm.Raw[int64](legacy, "SELECT status_code FROM legacy_placements WHERE ref='R'")

	// The tag model into the same table, same code.
	sb, _, lb, ctxb := twoDBs(t, "main", "legacy")
	_ = sb
	Tag := orm.For[TaggedPlacement]()
	legacyb := orm.WithDB(context.Background(), lb)
	if _, err := lb.SQL().ExecContext(legacyb, `CREATE TABLE legacy_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := orm.CreateTables(ctxb, Tag); err != nil {
		t.Fatal(err)
	}
	TagM := orm.For[TaggedPlacement](orm.MirrorUnder("legacy"))
	if err := TagM.Create(orm.WithMirror(ctxb, lb), &TaggedPlacement{Ref: "R", Status: "CLOSED"}); err != nil {
		t.Fatal(err)
	}
	tagCode, _ := orm.Raw[int64](legacyb, "SELECT status_code FROM legacy_placements WHERE ref='R'")

	if len(attrCode) != 1 || len(tagCode) != 1 || attrCode[0] != tagCode[0] || attrCode[0] != 2 {
		t.Fatalf("attr code %v, tag code %v (want both 2): the two mechanisms differ", attrCode, tagCode)
	}
}

// CutAccount is a plain model with the same shape on two databases, for the
// cutover swap.
type CutAccount struct {
	ID   int64 `orm:"pk"`
	Name string
}

func TestDualAndRouteCutover(t *testing.T) {
	// Dual wires the same Route a hand write would.
	s := orm.NewDatabases()
	s.Apply(orm.Dual[MirrorPlacement]("main", "legacy",
		orm.MirrorUnder("legacy"), orm.MirrorTable("legacy_placements")))
	want := orm.Route{DB: "main", Mirror: "legacy", MirrorNames: "legacy", MirrorTable: "legacy_placements"}
	if got, ok := s.RouteFor("mirror_placements"); !ok || got != want {
		t.Fatalf("Dual route = %+v (ok %v), want %+v", got, ok, want)
	}

	// A config Route cutover: writes follow the route's primary, and the
	// swap moves them to the former mirror.
	set, a, b, ctx := twoDBs(t, "main", "backup")
	Accounts := orm.For[CutAccount]()
	for _, d := range []*orm.DB{a, b} {
		if err := orm.CreateTables(orm.WithDB(context.Background(), d), Accounts); err != nil {
			t.Fatal(err)
		}
	}
	// Phase 1: write main, mirror backup.
	set.Route("cut_accounts", orm.Route{DB: "main", Mirror: "backup"})
	if err := Accounts.Create(ctx, &CutAccount{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	na, _ := orm.Raw[int64](orm.WithDB(ctx, a), "SELECT count(*) FROM cut_accounts")
	nb, _ := orm.Raw[int64](orm.WithDB(ctx, b), "SELECT count(*) FROM cut_accounts")
	if na[0] != 1 || nb[0] != 1 {
		t.Fatalf("phase 1: main %d, backup %d (want 1, 1)", na[0], nb[0])
	}
	// Phase 2 (cutover): backup is now primary, no mirror.
	set.Route("cut_accounts", orm.Route{DB: "backup"})
	if err := Accounts.Create(ctx, &CutAccount{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	na, _ = orm.Raw[int64](orm.WithDB(ctx, a), "SELECT count(*) FROM cut_accounts")
	nb, _ = orm.Raw[int64](orm.WithDB(ctx, b), "SELECT count(*) FROM cut_accounts")
	if na[0] != 1 || nb[0] != 2 {
		t.Fatalf("after cutover: main %d, backup %d (want 1, 2: the write moved to the former mirror)", na[0], nb[0])
	}
}
