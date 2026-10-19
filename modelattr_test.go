package orm_test

import (
	"cmp"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/orm/ormtest"
	"github.com/vuka-lang/vuka"
)

// modelRoute is a generic model-level attribute: it routes and mirrors the
// WHOLE model, the way For's On/Mirror/MirrorUnder/MirrorTable and
// @orm.Meta's DB/Mirror do. An app writes it on the embedded orm.Base; the
// tests register it on a field, which is where the Vuka field-attribute
// registry keeps a Base's attributes. ApplyModel folds with cmp.Or so a
// value another layer already set is never clobbered.
type modelRoute struct {
	db, names, table, mirror, mirrorNames, mirrorTable string
	unmanaged                                          bool
}

func (a modelRoute) ApplyModel(c *orm.ModelConfig) {
	c.DB = cmp.Or(c.DB, a.db)
	c.Names = cmp.Or(c.Names, a.names)
	c.Table = cmp.Or(c.Table, a.table)
	c.Mirror = cmp.Or(c.Mirror, a.mirror)
	c.MirrorNames = cmp.Or(c.MirrorNames, a.mirrorNames)
	c.MirrorTable = cmp.Or(c.MirrorTable, a.mirrorTable)
	if a.unmanaged {
		c.Unmanaged = true
	}
}

// --- Test 1: a model-level attribute dual-writes identically to For. ---

// AttrPlacement mirrors to a legacy schema entirely through a model-level
// attribute: no Dual, no For options, no Meta. ForPlacement is the same
// shape configured through For instead — the two must behave identically.
type AttrPlacement struct {
	ID       int64 `orm:"pk"`
	Ref      string
	Status   string
	ClosedAt time.Time
}

type ForPlacement struct {
	ID       int64 `orm:"pk"`
	Ref      string
	Status   string
	ClosedAt time.Time
}

func init() {
	legacy := map[string][]any{
		"Status":   {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
		"ClosedAt": {legacyCol{"closed_at_text", "text", orm.TimeAsText(mpTimeLayout)}},
	}
	withAttr := map[string][]any{
		"ID":       {modelRoute{mirror: "legacy", mirrorNames: "legacy", mirrorTable: "legacy_placements"}},
		"Status":   legacy["Status"],
		"ClosedAt": legacy["ClosedAt"],
	}
	vuka.RegisterFields[AttrPlacement](withAttr)
	vuka.RegisterFields[ForPlacement](legacy)
}

func createLegacyPlacements(t *testing.T, ctx context.Context, d *orm.DB) {
	t.Helper()
	if _, err := d.SQL().ExecContext(ctx,
		`CREATE TABLE legacy_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER, closed_at_text TEXT)`); err != nil {
		t.Fatal(err)
	}
}

func TestModelAttrDualWrite(t *testing.T) {
	s, _, legacyDB, ctx := twoDBs(t, "main", "legacy")
	_ = s
	Placements := orm.For[AttrPlacement]() // configured only by the model attribute
	Legacy := orm.For[AttrPlacement](orm.On("legacy"), orm.Names("legacy"),
		orm.Table("legacy_placements"), orm.Unmanaged())

	if err := orm.CreateTables(ctx, Placements); err != nil {
		t.Fatal(err)
	}
	legacy := orm.WithDB(context.Background(), legacyDB)
	createLegacyPlacements(t, legacy, legacyDB)

	closed := time.Date(2026, 2, 3, 9, 30, 0, 0, time.UTC)
	if err := Placements.Create(ctx, &AttrPlacement{Ref: "P-1", Status: "OPEN", ClosedAt: closed}); err != nil {
		t.Fatal(err)
	}

	// Primary keeps the Go shape; the mirror row is translated.
	main, err := orm.Raw[string](ctx, "SELECT status FROM attr_placements WHERE id = 1")
	if err != nil || len(main) != 1 || main[0] != "OPEN" {
		t.Fatalf("primary status = %v, %v", main, err)
	}
	code, err := orm.Raw[int64](legacy, "SELECT status_code FROM legacy_placements WHERE id = 1")
	if err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("mirror status_code = %v, %v (want 1)", code, err)
	}
	at, _ := orm.Raw[string](legacy, "SELECT closed_at_text FROM legacy_placements WHERE id = 1")
	if len(at) != 1 || at[0] != "2026-02-03 09:30:00" {
		t.Fatalf("mirror closed_at_text = %v", at)
	}

	// Reads hit only the primary: move the mirror out of band, the read is unmoved.
	if _, err := legacyDB.SQL().ExecContext(legacy, "UPDATE legacy_placements SET status_code = 99"); err != nil {
		t.Fatal(err)
	}
	got, err := Placements.Get(ctx, orm.Q{"id": 1})
	if err != nil || got.Status != "OPEN" {
		t.Fatalf("read reached the mirror: %q, %v", got.Status, err)
	}
	// The codec round-trips reading the legacy schema back.
	if _, err := legacyDB.SQL().ExecContext(legacy, "UPDATE legacy_placements SET status_code = 1"); err != nil {
		t.Fatal(err)
	}
	lp, err := Legacy.Get(ctx, orm.Q{"id": 1})
	if err != nil || lp.Status != "OPEN" || !lp.ClosedAt.Equal(closed) {
		t.Fatalf("legacy read = %+v, %v", lp, err)
	}

	// A failing mirror surfaces via OnMirrorError without failing the primary.
	var failed []orm.MirrorError
	stop := orm.OnMirrorError(func(_ context.Context, e orm.MirrorError) { failed = append(failed, e) })
	defer stop()
	if _, err := legacyDB.SQL().ExecContext(legacy, "DROP TABLE legacy_placements"); err != nil {
		t.Fatal(err)
	}
	if err := Placements.Create(ctx, &AttrPlacement{Ref: "P-2", Status: "OPEN", ClosedAt: closed}); err != nil {
		t.Fatalf("a failing mirror failed the primary write: %v", err)
	}
	kept, _ := orm.Raw[int64](ctx, "SELECT count(*) FROM attr_placements")
	if kept[0] != 2 || len(failed) != 1 || failed[0].Table != "attr_placements" || failed[0].Op != "create" || failed[0].Mirror != "legacy" {
		t.Fatalf("primary rows %d, failures %+v", kept[0], failed)
	}

	// Equivalence: the same config via For produces the SAME mirror row.
	sf, _, legacyF, ctxF := twoDBs(t, "main", "legacy")
	_ = sf
	For := orm.For[ForPlacement](orm.Mirror("legacy"), orm.MirrorUnder("legacy"), orm.MirrorTable("legacy_placements"))
	legF := orm.WithDB(context.Background(), legacyF)
	if err := orm.CreateTables(ctxF, orm.For[ForPlacement]()); err != nil {
		t.Fatal(err)
	}
	createLegacyPlacements(t, legF, legacyF)
	if err := For.Create(ctxF, &ForPlacement{Ref: "P-1", Status: "OPEN", ClosedAt: closed}); err != nil {
		t.Fatal(err)
	}
	forCode, _ := orm.Raw[int64](legF, "SELECT status_code FROM legacy_placements WHERE id = 1")
	if len(forCode) != 1 || forCode[0] != code[0] {
		t.Fatalf("For mirror code %v != model-attr mirror code %v", forCode, code)
	}
}

// --- Test 2: a model-level attribute sets the primary database. ---

type AttrDBPlacement struct {
	ID  int64 `orm:"pk"`
	Ref string
}

func init() {
	vuka.RegisterFields[AttrDBPlacement](map[string][]any{"ID": {modelRoute{db: "second"}}})
}

func TestModelAttrDB(t *testing.T) {
	_, mainDB, secondDB, ctx := twoDBs(t, "main", "second")
	Placements := orm.For[AttrDBPlacement]()
	if got := Placements.Info().Meta.DB; got != "second" {
		t.Fatalf("Info().Meta.DB = %q, want second", got)
	}
	if err := orm.CreateTables(ctx, Placements); err != nil {
		t.Fatal(err)
	}
	if err := Placements.Create(ctx, &AttrDBPlacement{Ref: "R"}); err != nil {
		t.Fatal(err)
	}
	// The write (and read) went to "second", not the default "main".
	n, _ := orm.Raw[int64](orm.WithDB(ctx, secondDB), "SELECT count(*) FROM attr_db_placements")
	if len(n) != 1 || n[0] != 1 {
		t.Fatalf("second rows = %v (want 1)", n)
	}
	got, err := Placements.Count(ctx)
	if err != nil || got != 1 {
		t.Fatalf("read from the model's database = %d, %v", got, err)
	}
	// The table was never made on the default database.
	if tables, _ := orm.Raw[int64](orm.WithDB(ctx, mainDB),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='attr_db_placements'"); len(tables) == 1 && tables[0] != 0 {
		t.Fatalf("the table was created on the default database too")
	}
}

// --- Test 3: @orm.Meta and a model attribute on one Base both apply. ---

type PrecPlacement struct {
	ID  int64 `orm:"pk"`
	Ref string
}

func init() {
	vuka.RegisterFields[PrecPlacement](map[string][]any{
		// Both on the one (Base) field: @orm.Meta sets DB, the model
		// attribute sets DB (loses to Meta) and Mirror (fills, Meta set none).
		"ID": {orm.Meta{DB: "metaDB"}, modelRoute{db: "attrDB", mirror: "attrMirror"}},
	})
}

func TestModelAttrMetaPrecedence(t *testing.T) {
	info := orm.For[PrecPlacement]().Info()
	if info.Meta.DB != "metaDB" {
		t.Fatalf("DB = %q, want metaDB (an @orm.Meta field wins over a model attribute)", info.Meta.DB)
	}
	if info.Meta.Mirror != "attrMirror" {
		t.Fatalf("Mirror = %q, want attrMirror (the model attribute fills what Meta leaves empty)", info.Meta.Mirror)
	}
	// A For option wins over both.
	if got := orm.Of[PrecPlacement](orm.DBSchema{DB: "forDB"}).Info().Meta.DB; got != "forDB" {
		t.Fatalf("For's On = %q, want forDB (a For option wins over Meta and the attribute)", got)
	}
}

// --- Test 4 (coordinator #1): empty MirrorTable → the model's own table. ---

type SameTablePlacement struct {
	ID     int64 `orm:"pk"`
	Ref    string
	Status string
}

func init() {
	vuka.RegisterFields[SameTablePlacement](map[string][]any{
		// A mirror with no table named: it must repeat into the same-named
		// table on the mirror database.
		"ID":     {modelRoute{mirror: "legacy", mirrorNames: "legacy"}},
		"Status": {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
	})
}

func TestModelAttrMirrorTableDefault(t *testing.T) {
	_, _, legacyDB, ctx := twoDBs(t, "main", "legacy")
	Placements := orm.For[SameTablePlacement]()
	if err := orm.CreateTables(ctx, Placements); err != nil {
		t.Fatal(err)
	}
	legacy := orm.WithDB(context.Background(), legacyDB)
	// The mirror table is NAMED like the model's own table (same_table_placements),
	// but holds the legacy column + type.
	if _, err := legacyDB.SQL().ExecContext(legacy,
		`CREATE TABLE same_table_placements (id INTEGER PRIMARY KEY, ref TEXT, status_code INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := Placements.Create(ctx, &SameTablePlacement{Ref: "R", Status: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	code, err := orm.Raw[int64](legacy, "SELECT status_code FROM same_table_placements WHERE ref = 'R'")
	if err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("mirror into the same-named table = %v, %v (want 1)", code, err)
	}
}

// --- Test 5 (coordinator #2): an empty per-schema Column inherits the primary. ---

type InheritPlacement struct {
	ID     int64 `orm:"pk"`
	Ref    string
	Status string
}

func init() {
	vuka.RegisterFields[InheritPlacement](map[string][]any{
		// A per-set override that RETYPES the column (and adds a codec)
		// without RENAMING it: an empty Column must inherit the primary one.
		"Status": {legacyCol{"", "integer", orm.CodeMap(mpStatusCodes)}},
	})
}

func TestSchemaFieldInheritsColumn(t *testing.T) {
	// Read under the legacy set: the column keeps its primary name "status",
	// never a blank one.
	legacyMgr := orm.For[InheritPlacement](orm.Names("legacy"), orm.Unmanaged())
	cols := legacyMgr.Columns()
	if !slices.Contains(cols, "status") || slices.Contains(cols, "") {
		t.Fatalf("legacy columns = %v (an empty override blanked the column)", cols)
	}

	// And the override's TYPE/codec still apply: the mirror stores the int
	// code in the inherited column name.
	_, _, legacyDB, ctx := twoDBs(t, "main", "legacy")
	home := orm.For[InheritPlacement](orm.MirrorUnder("legacy"))
	if err := orm.CreateTables(ctx, orm.For[InheritPlacement]()); err != nil {
		t.Fatal(err)
	}
	legacy := orm.WithDB(context.Background(), legacyDB)
	if _, err := legacyDB.SQL().ExecContext(legacy,
		`CREATE TABLE inherit_placements (id INTEGER PRIMARY KEY, ref TEXT, status INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := home.Create(orm.WithMirror(ctx, legacyDB), &InheritPlacement{Ref: "R", Status: "CLOSED"}); err != nil {
		t.Fatal(err)
	}
	// Primary keeps the plain string in column "status"; the mirror holds the
	// code 2 in that same inherited column name.
	main, _ := orm.Raw[string](ctx, "SELECT status FROM inherit_placements WHERE ref = 'R'")
	if len(main) != 1 || main[0] != "CLOSED" {
		t.Fatalf("primary status = %v (want CLOSED)", main)
	}
	code, err := orm.Raw[int64](legacy, "SELECT status FROM inherit_placements WHERE ref = 'R'")
	if err != nil || len(code) != 1 || code[0] != 2 {
		t.Fatalf("mirror status (inherited column) = %v, %v (want 2)", code, err)
	}
}

// --- Test 6 (MySQL-first): a model attribute rebinds the PRIMARY. ---

// RebindPlacement's live primary is the legacy format (status as an int
// code); its mirror is the clean default format (status as text). The model
// attribute sets Names: "legacy" so For[T]() reads and writes the primary in
// the legacy shape, and mirrors to the default shape — the inverse of the
// usual clean-primary/legacy-mirror arrangement.
type RebindPlacement struct {
	ID     int64 `orm:"pk"`
	Ref    string
	Status string
}

func init() {
	vuka.RegisterFields[RebindPlacement](map[string][]any{
		"ID":     {modelRoute{db: "main", names: "legacy", mirror: "target"}},
		"Status": {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
	})
}

func TestModelAttrPrimaryRebind(t *testing.T) {
	_, _, targetDB, ctx := twoDBs(t, "main", "target")
	Placements := orm.For[RebindPlacement]() // no options: bound under the legacy set

	// The home manager is bound under "legacy": its column is status_code.
	if cols := Placements.Columns(); !slices.Contains(cols, "status_code") {
		t.Fatalf("primary columns = %v (the primary was not rebound under the legacy set)", cols)
	}

	// Primary (main) in the legacy shape; mirror (target) in the default shape.
	if err := orm.CreateTables(ctx, Placements); err != nil {
		t.Fatal(err)
	}
	target := orm.WithDB(context.Background(), targetDB)
	if _, err := targetDB.SQL().ExecContext(target,
		`CREATE TABLE rebind_placements (id INTEGER PRIMARY KEY, ref TEXT, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Placements.Create(ctx, &RebindPlacement{Ref: "R", Status: "OPEN"}); err != nil {
		t.Fatal(err)
	}

	// The live primary holds the int code in status_code.
	code, err := orm.Raw[int64](ctx, "SELECT status_code FROM rebind_placements WHERE ref = 'R'")
	if err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("primary status_code = %v, %v (want 1: the primary is the legacy form)", code, err)
	}
	// The mirror holds the plain string in status.
	mirror, err := orm.Raw[string](target, "SELECT status FROM rebind_placements WHERE ref = 'R'")
	if err != nil || len(mirror) != 1 || mirror[0] != "OPEN" {
		t.Fatalf("mirror status = %v, %v (want OPEN: the mirror is the default form)", mirror, err)
	}
	// Reads go through the legacy codec, so the Go value is the clean string.
	got, err := Placements.Get(ctx, orm.Q{"ref": "R"})
	if err != nil || got.Status != "OPEN" {
		t.Fatalf("read from the primary = %q, %v (the legacy codec did not round-trip)", got.Status, err)
	}
}

// --- Relation config via FieldAttr (built-in @orm.Elements and an app one). ---

// appElements is an app-defined relation decorator: it sets o.Elements
// through the public FieldAttr hook, no special ORM interface.
type appElements string

func (a appElements) Apply(o *orm.FieldOptions) { o.Elements = cmp.Or(string(a), o.Elements) }

// Three ways to the same elements relation: the raw tag, the built-in
// @orm.Elements attribute, and an app FieldAttr.
type TagElemMemo struct {
	ID    int64
	Refs  orm.CSV[int64]
	Spots []Spot `orm:"elements:refs"`
}
type AttrElemMemo struct {
	ID    int64
	Refs  orm.CSV[int64]
	Spots []Spot
}
type AppElemMemo struct {
	ID    int64
	Refs  orm.CSV[int64]
	Spots []Spot
}

func init() {
	vuka.RegisterFields[AttrElemMemo](map[string][]any{"Spots": {orm.Elements{Column: "refs"}}})
	vuka.RegisterFields[AppElemMemo](map[string][]any{"Spots": {appElements("refs")}})
}

func TestElementsAttr(t *testing.T) {
	Tag := orm.For[TagElemMemo]()
	Attr := orm.For[AttrElemMemo]()
	App := orm.For[AppElemMemo]()
	ctx := ormtest.Open(t, Spots, Tag, Attr, App)
	web1, web2, garden := &Spot{Kind: "web"}, &Spot{Kind: "web"}, &Spot{Kind: "garden"}
	ormtest.Seed(t, ctx, Spots, web1, web2, garden)
	ormtest.Seed(t, ctx, Tag, &TagElemMemo{Refs: orm.CSVOf(web2.ID, garden.ID)})
	ormtest.Seed(t, ctx, Attr, &AttrElemMemo{Refs: orm.CSVOf(web2.ID, garden.ID)})
	ormtest.Seed(t, ctx, App, &AppElemMemo{Refs: orm.CSVOf(web2.ID, garden.ID)})

	want := []int64{web2.ID, garden.ID}
	tags, err := Tag.PrefetchRelated("spots").All(ctx)
	if err != nil || len(tags) != 1 || !slices.Equal(spotIDs(tags[0].Spots), want) {
		t.Fatalf("raw tag: %v, %v", tags, err)
	}
	attrs, err := Attr.PrefetchRelated("spots").All(ctx)
	if err != nil || len(attrs) != 1 || !slices.Equal(spotIDs(attrs[0].Spots), want) {
		t.Fatalf("@orm.Elements: %v, %v (differs from the raw tag)", attrs, err)
	}
	apps, err := App.PrefetchRelated("spots").All(ctx)
	if err != nil || len(apps) != 1 || !slices.Equal(spotIDs(apps[0].Spots), want) {
		t.Fatalf("app FieldAttr: %v, %v (differs from the raw tag)", apps, err)
	}
}

// --- Acceptance: For[T]() with zero options binds the legacy set, and the
// model's relations resolve under it too. ---

// LegacyMemo's primary is the legacy schema: its key column is renamed
// (ref_list) under "legacy". A model-level attribute names that set, so
// For[LegacyMemo]() with no options binds under it — no per-query Names.
type LegacyMemo struct {
	ID    int64
	Refs  orm.CSV[int64]
	Spots []Spot
}

func init() {
	vuka.RegisterFields[LegacyMemo](map[string][]any{
		"ID":    {modelRoute{names: "legacy"}},
		"Refs":  {legacyCol{"ref_list", "text", nil}},
		"Spots": {orm.Elements{Column: "refs"}},
	})
}

func TestModelAttrNamesRelations(t *testing.T) {
	Memos := orm.For[LegacyMemo]() // ZERO options
	// Transparent: bound under the attribute's "legacy" set, so the column
	// carries its legacy name — the caller never passes orm.Names.
	if cols := Memos.Columns(); !slices.Contains(cols, "ref_list") || slices.Contains(cols, "refs") {
		t.Fatalf("columns = %v, want the legacy ref_list (For[T]() did not honor the attribute's Names)", cols)
	}
	ctx := ormtest.Open(t, Spots, Memos)
	web1, web2, garden := &Spot{Kind: "web"}, &Spot{Kind: "web"}, &Spot{Kind: "garden"}
	_ = web1
	ormtest.Seed(t, ctx, Spots, web1, web2, garden)
	// The write goes through the legacy column too.
	ormtest.Seed(t, ctx, Memos, &LegacyMemo{Refs: orm.CSVOf(web2.ID, garden.ID)})

	// The elements relation resolves under the legacy names set (it reads the
	// renamed ref_list column), with no Names on the query.
	got, err := Memos.PrefetchRelated("spots").All(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("prefetch = %v, %v", got, err)
	}
	if ids := spotIDs(got[0].Spots); !slices.Equal(ids, []int64{web2.ID, garden.ID}) {
		t.Fatalf("elements relation under legacy loaded %v, want %v", ids, []int64{web2.ID, garden.ID})
	}
}

// StaticMemo is driven through the idiomatic static path only — orm.Objects
// (what the Vuka `T.Objects` static resolves to), never For, never Names.
// Its primary is the legacy schema (status as an int code).
type StaticMemo struct {
	ID     int64
	Status string
	Refs   orm.CSV[int64]
	Spots  []Spot
}

func init() {
	vuka.RegisterFields[StaticMemo](map[string][]any{
		"ID":     {modelRoute{names: "legacy"}},
		"Status": {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
		"Spots":  {orm.Elements{Column: "refs"}},
	})
}

func TestModelAttrNamesObjects(t *testing.T) {
	// The registered default names set is honored by the static/Objects path,
	// not only For[T](): the manager binds under "legacy" with no Names.
	if cols := orm.Objects[StaticMemo]().Columns(); !slices.Contains(cols, "status_code") {
		t.Fatalf("Objects[T]() columns = %v (the static path ignored the attribute's Names)", cols)
	}
	ctx := ormtest.Open(t, Spots, orm.Objects[StaticMemo]())
	web1, web2 := &Spot{Kind: "web"}, &Spot{Kind: "web"}
	ormtest.Seed(t, ctx, Spots, web1, web2)
	ormtest.Seed(t, ctx, orm.Objects[StaticMemo](), &StaticMemo{Status: "OPEN", Refs: orm.CSVOf(web1.ID, web2.ID)})

	// The write went through the legacy int column.
	code, err := orm.Raw[int64](ctx, "SELECT status_code FROM static_memos")
	if err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("Objects write status_code = %v, %v (want 1)", code, err)
	}
	// A read and a relation through the static, filtering by the clean Go
	// value (the legacy codec encodes it): no Names anywhere.
	got, err := orm.Objects[StaticMemo]().PrefetchRelated("spots").Filter(orm.Q{"status": "OPEN"}).All(ctx)
	if err != nil || len(got) != 1 || got[0].Status != "OPEN" {
		t.Fatalf("Objects read = %+v, %v", got, err)
	}
	if ids := spotIDs(got[0].Spots); !slices.Equal(ids, []int64{web1.ID, web2.ID}) {
		t.Fatalf("relation through the static loaded %v", ids)
	}
}

// --- ModelConfig.Table: the primary's table under its names set. ---

// TableRow's live primary is a legacy table of another name, named by the
// model attribute alone (no LegacyTableName); its mirror is the clean table.
type TableRow struct {
	ID     int64 `orm:"pk"`
	Ref    string
	Status string
}

type TableRowNote struct {
	ID    int64 `orm:"pk"`
	Text  string
	RowID int64
	Row   *TableRow
}

func init() {
	vuka.RegisterFields[TableRow](map[string][]any{
		"ID":     {modelRoute{db: "main", names: "legacy", table: "legacy_rows", mirror: "target"}},
		"Status": {legacyCol{"status_code", "integer", orm.CodeMap(mpStatusCodes)}},
	})
}

func TestModelAttrTable(t *testing.T) {
	Rows := orm.For[TableRow]()
	if got := Rows.TableName(); got != "legacy_rows" {
		t.Fatalf("For[T]().TableName() = %q, want legacy_rows", got)
	}
	if got := orm.For[TableRow](orm.Names("legacy")).TableName(); got != "legacy_rows" {
		t.Fatalf("Names(legacy).TableName() = %q, want legacy_rows", got)
	}
	if got := orm.Of[TableRow](orm.DBSchema{Names: "legacy"}).TableName(); got != "legacy_rows" {
		t.Fatalf("Of(legacy).TableName() = %q, want legacy_rows", got)
	}
	if got := orm.For[TableRow](orm.Names("other")).TableName(); got != "table_rows" {
		t.Fatalf("Names(other).TableName() = %q, want table_rows (Table is the Names set's only)", got)
	}
	if got := orm.For[TableRow](orm.Table("x_rows")).TableName(); got != "x_rows" {
		t.Fatalf("orm.Table over the attribute = %q, want x_rows", got)
	}

	_, _, targetDB, ctx := twoDBs(t, "main", "target")
	if err := orm.CreateTables(ctx, Rows); err != nil {
		t.Fatal(err)
	}
	target := orm.WithDB(context.Background(), targetDB)
	if _, err := targetDB.SQL().ExecContext(target,
		`CREATE TABLE table_rows (id INTEGER PRIMARY KEY, ref TEXT, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	r := &TableRow{Ref: "R", Status: "OPEN"}
	if err := Rows.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if code, err := orm.Raw[int64](ctx, "SELECT status_code FROM legacy_rows WHERE ref = 'R'"); err != nil || len(code) != 1 || code[0] != 1 {
		t.Fatalf("primary legacy_rows = %v, %v (want 1)", code, err)
	}
	// The mirror binds the default names set, so its own table, not the primary's.
	if st, err := orm.Raw[string](target, "SELECT status FROM table_rows WHERE ref = 'R'"); err != nil || len(st) != 1 || st[0] != "OPEN" {
		t.Fatalf("mirror table_rows = %v, %v (want OPEN)", st, err)
	}
	if got, err := Rows.Get(ctx, orm.Q{"ref": "R"}); err != nil || got.Status != "OPEN" {
		t.Fatalf("read = %+v, %v", got, err)
	}

	Notes := orm.For[TableRowNote](orm.Names("legacy"))
	if err := orm.CreateTables(ctx, Notes); err != nil {
		t.Fatal(err)
	}
	if err := Notes.Create(ctx, &TableRowNote{Text: "n", RowID: r.ID}); err != nil {
		t.Fatal(err)
	}
	notes, err := Notes.Filter(orm.Q{"row__ref": "R"}).SelectRelated("row").All(ctx)
	if err != nil || len(notes) != 1 || notes[0].Row == nil || notes[0].Row.Status != "OPEN" {
		t.Fatalf("join into legacy_rows = %+v, %v", notes, err)
	}
}
