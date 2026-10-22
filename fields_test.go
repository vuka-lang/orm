package orm_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/migration"
	"github.com/vuka-lang/orm/v2/ormtest"
	"github.com/vuka-lang/vuka/v2"
)

// Phone is a value type of its own: written without spaces, read back
// with a mark, a column of its type, a check of its own.
type Phone string

func (p Phone) DBValue(d orm.Dialect) (any, error) {
	return strings.ReplaceAll(string(p), " ", ""), nil
}

func (p *Phone) FromDB(src any) error {
	switch s := src.(type) {
	case nil:
		*p = ""
	case []byte:
		*p = Phone(s)
	default:
		*p = Phone(fmt.Sprint(s))
	}
	return nil
}

func (Phone) ColumnType(d orm.Dialect) string {
	if d.Name() == "sqlite" {
		return "TEXT"
	}
	return "VARCHAR(20)"
}

func (p Phone) Validate() error {
	if !strings.HasPrefix(string(p), "+") {
		return errors.New("must start with a country code")
	}
	return nil
}

// EmailAttr is a field attribute of the program's own.
type EmailAttr struct{ Unique bool }

func (e EmailAttr) Apply(o *orm.FieldOptions) {
	o.MaxLength, o.Lower, o.Trim = 254, true, true
	o.Unique = o.Unique || e.Unique
}

func (EmailAttr) Validate(v any) error {
	if !strings.Contains(v.(string), "@") {
		return errors.New("is not an email address")
	}
	return nil
}

type Invoice struct {
	ID     int64
	Ref    string
	Status string
	Qty    int
	Phone  Phone
	Email  string
	Placed time.Time
	Due    time.Time
	Note   *string
}

type Country struct {
	Code int64
	Name string
}

type Badge struct {
	ID    string
	Title string
}

func init() {
	vuka.RegisterFields[Invoice](map[string][]any{
		"Ref": {orm.Char{Max: 8, Min: 3, Pattern: `^[A-Z0-9-]+$`, Trim: true, Comment: "the customer's reference",
			Label: "Reference", Help: "printed on the invoice"}},
		"Status": {orm.Char{Max: 20, Default: "draft", Choices: []string{"draft", "paid", "void"}, Index: true}},
		"Qty":    {orm.Int{Min: 1, Max: 100, Size: orm.Small, Default: 1}},
		"Email":  {EmailAttr{Unique: true}},
		"Placed": {orm.Datetime{DefaultNow: true, Precision: 3, UTC: true}},
		"Due":    {orm.DateField{Label: "Due on"}},
		"Note":   {orm.Column("memo"), orm.Char{Max: 50}},
	})
	vuka.RegisterFields[Country](map[string][]any{"Code": {orm.PK{Manual: true}}})
	vuka.RegisterFields[Badge](map[string][]any{"ID": {orm.PK{UUID: true}}, "Title": {orm.Char{Max: 100}}})
	Invoices, Countries, Badges = orm.For[Invoice](), orm.For[Country](), orm.For[Badge]()
}

// The managers are made after the attributes are registered, as the
// code Vuka generates does.
var (
	Invoices  *orm.Manager[Invoice]
	Countries *orm.Manager[Country]
	Badges    *orm.Manager[Badge]
)

func fieldsOf(t *testing.T, err error) map[string][]string {
	t.Helper()
	var e *orm.Error
	if !errors.As(err, &e) || !errors.Is(err, orm.ErrInvalid) {
		t.Fatalf("want an Invalid error, got %v", err)
	}
	return e.Fields
}

func TestFieldAttributes(t *testing.T) {
	ctx := ormtest.Open(t, Invoices, Countries, Badges)
	due := time.Date(2026, 10, 12, 15, 30, 0, 0, time.FixedZone("EAT", 3*3600))
	inv := Invoice{Ref: "  A-1 ", Phone: "+255 712 000", Email: " Ali@Mail.COM ", Due: due}
	if err := Invoices.Create(ctx, &inv); err != nil {
		t.Fatal(err)
	}
	// Defaults, normalizing, the stamp in UTC cut to milliseconds.
	if inv.Status != "draft" || inv.Qty != 1 || inv.Ref != "A-1" || inv.Email != "ali@mail.com" {
		t.Fatalf("after create: %+v", inv)
	}
	if inv.Placed.IsZero() || inv.Placed.Location() != time.UTC || inv.Placed.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("placed: %v", inv.Placed)
	}
	got, err := Invoices.Get(ctx, orm.Q{"id": inv.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Phone != "+255712000" || got.Status != "draft" || got.Qty != 1 || got.Email != "ali@mail.com" {
		t.Fatalf("read back: %+v", got)
	}
	if !got.Placed.Equal(inv.Placed) || got.Placed.Location() != time.UTC {
		t.Fatalf("placed read back %v, wrote %v", got.Placed, inv.Placed)
	}
	if y, m, d := got.Due.Date(); y != 2026 || m != 10 || d != 12 || got.Due.Hour() != 0 {
		t.Fatalf("a date reads its day: %v", got.Due)
	}
	// Conditions pass through the type's DBValue and the date's day.
	if n, err := Invoices.Filter(orm.Q{"phone": Phone("+255 712 000")}).Count(ctx); err != nil || n != 1 {
		t.Fatalf("by phone: %d, %v", n, err)
	}
	if n, err := Invoices.Filter(orm.Q{"due": due.Add(5 * time.Hour)}).Count(ctx); err != nil || n != 1 {
		t.Fatalf("by due: %d, %v", n, err)
	}
	if n, err := Invoices.Filter(orm.Q{"phone__in": []Phone{"+1 1", "+255 712 000"}}).Count(ctx); err != nil || n != 1 {
		t.Fatalf("phone in: %d, %v", n, err)
	}

	// Each check refuses, naming its column.
	for _, c := range []struct {
		name string
		edit func(*Invoice)
		col  string
		msg  string
	}{
		{"too long", func(i *Invoice) { i.Ref = "ABCDEFGHI" }, "ref", "at most 8 characters"},
		{"too short", func(i *Invoice) { i.Ref = "AB" }, "ref", "at least 3 characters"},
		{"pattern", func(i *Invoice) { i.Ref = "ab-1" }, "ref", "must match"},
		{"choice", func(i *Invoice) { i.Status = "lost" }, "status", "must be one of draft, paid, void"},
		{"below", func(i *Invoice) { i.Qty = -1 }, "qty", "at least 1"},
		{"above", func(i *Invoice) { i.Qty = 101 }, "qty", "at most 100"},
		{"type's check", func(i *Invoice) { i.Phone = "0712" }, "phone", "country code"},
		{"attribute's check", func(i *Invoice) { i.Email = "nobody" }, "email", "not an email"},
		{"bounded pointer", func(i *Invoice) { s := strings.Repeat("x", 51); i.Note = &s }, "memo", "at most 50"},
	} {
		bad := inv
		bad.ID = 0
		bad.Email = "other@mail.com"
		c.edit(&bad)
		fields := fieldsOf(t, Invoices.Create(ctx, &bad))
		if msgs := fields[c.col]; len(msgs) == 0 || !strings.Contains(msgs[0], c.msg) {
			t.Errorf("%s: %v", c.name, fields)
		}
	}
	// Save and Update check too; Update normalizes the text it sets.
	bad := got
	bad.Qty = 0
	if f := fieldsOf(t, Invoices.Save(ctx, &bad)); len(f["qty"]) == 0 {
		t.Fatalf("save: %v", f)
	}
	_, err = Invoices.Filter(orm.Q{"id": inv.ID}).Update(ctx, orm.Set{"status": "gone", "qty": 500})
	if f := fieldsOf(t, err); len(f) != 2 {
		t.Fatalf("update: %v", f)
	}
	if _, err := Invoices.Filter(orm.Q{"id": inv.ID}).Update(ctx, orm.Set{"ref": "  B-2  ", "qty": orm.SQL("{0} + {1}", orm.F("qty"), 1)}); err != nil {
		t.Fatal(err)
	}
	if got := must(Invoices.Get(ctx, orm.Q{"id": inv.ID})); got.Ref != "B-2" || got.Qty != 2 {
		t.Fatalf("after update: %+v", got)
	}
	// BulkCreate checks every row; GetOrCreate as Create.
	if err := Invoices.BulkCreate(ctx, []*Invoice{{Ref: "C-3", Phone: "+1", Email: "c@x", Due: due}, {Ref: "x", Phone: "+1", Email: "d@x", Due: due}}); !errors.Is(err, orm.ErrInvalid) {
		t.Fatalf("bulk: %v", err)
	}
	if _, _, err := Invoices.GetOrCreate(ctx, orm.Q{"ref": "D-4"}, orm.Defaults{"phone": Phone("1"), "email": "e@x", "due": due}); !errors.Is(err, orm.ErrInvalid) {
		t.Fatalf("get or create: %v", err)
	}

	// The database's defaults, for a row SQL inserts.
	ormtest.Exec(t, ctx, "INSERT INTO invoices (ref, phone, email, due) VALUES ('E-5', '+2', 'f@x', '2026-01-02')")
	raw, err := Invoices.Get(ctx, orm.Q{"ref": "E-5"})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Status != "draft" || raw.Qty != 1 || raw.Placed.IsZero() {
		t.Fatalf("the table's defaults: %+v", raw)
	}

	// Field info for forms and tools.
	info := Invoices.Fields()
	i := slices.IndexFunc(info, func(f orm.FieldInfo) bool { return f.Name == "Ref" })
	ref := info[i]
	if ref.Label != "Reference" || ref.Help != "printed on the invoice" || ref.Comment == "" || ref.MaxLength != 8 || ref.MinLength != 3 || ref.Pattern == "" {
		t.Fatalf("ref's info: %+v", ref)
	}
	i = slices.IndexFunc(info, func(f orm.FieldInfo) bool { return f.Name == "Qty" })
	if q := info[i]; q.Min.Value() != 1 || q.Max.Value() != 100 || q.Default != "1" || q.Initial != 1 {
		t.Fatalf("qty's info: %+v", q)
	}
	i = slices.IndexFunc(info, func(f orm.FieldInfo) bool { return f.Name == "Status" })
	if s := info[i]; !slices.Equal(s.Choices, []any{"draft", "paid", "void"}) || !s.Index {
		t.Fatalf("status's info: %+v", s)
	}
	i = slices.IndexFunc(info, func(f orm.FieldInfo) bool { return f.Name == "Note" })
	if n := info[i]; n.Column != "memo" || !n.Null || n.MaxLength != 50 {
		t.Fatalf("note's info: %+v", n)
	}

	// A key the program sets, zero included; a UUID key.
	for _, c := range []Country{{Code: 0, Name: "nowhere"}, {Code: 255, Name: "Tanzania"}} {
		if err := Countries.Create(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	if c := must(Countries.Get(ctx, orm.Q{"code": 0})); c.Name != "nowhere" {
		t.Fatalf("key 0: %+v", c)
	}
	tk := Badge{Title: "first"}
	if err := Badges.Create(ctx, &tk); err != nil {
		t.Fatal(err)
	}
	if len(tk.ID) != 36 || tk.ID[14] != '4' {
		t.Fatalf("a uuid key: %q", tk.ID)
	}
	if got := must(Badges.Get(ctx, orm.Q{"id": tk.ID})); got.Title != "first" || got.ID != tk.ID {
		t.Fatalf("by uuid: %+v", got)
	}
	tk.Title = "renamed"
	if err := Badges.Save(ctx, &tk); err != nil {
		t.Fatal(err)
	}
}

// Attributes apply left to right; an empty member never undoes an
// earlier one.
type Shipment struct {
	ID      int64
	A, B, C string
	Owner   orm.FK[User]
}

func init() {
	vuka.RegisterFields[Shipment](map[string][]any{
		"A":     {orm.Column("a_col"), orm.Char{Max: 5}},
		"B":     {orm.Char{Column: "b_first"}, orm.Column("b_col")},
		"C":     {orm.DefaultTo("x"), orm.Char{Max: 9, Default: "y"}, orm.Unique{}, orm.Char{}},
		"Owner": {orm.Column("owner_ref"), orm.Rel{OnDelete: orm.Cascade}},
	})
}

func TestFieldAttributeOrder(t *testing.T) {
	cols := orm.For[Shipment]().Fields()
	want := map[string]string{"A": "a_col", "B": "b_col", "C": "c", "Owner": "owner_ref"}
	for _, f := range cols {
		if w, ok := want[f.Name]; ok && f.Column != w {
			t.Errorf("%s's column %q, want %q", f.Name, f.Column, w)
		}
		if f.Name == "C" && (f.Default != "'y'" || f.MaxLength != 9 || !f.Unique || f.Initial != "y") {
			t.Errorf("C: %+v", f)
		}
	}
}

func TestFieldAttributeMistakes(t *testing.T) {
	type BadDate struct {
		ID   int64
		Name string
	}
	vuka.RegisterFields[BadDate](map[string][]any{"Name": {orm.DateField{}}})
	if err := orm.For[BadDate]().Err(); err == nil || !strings.Contains(err.Error(), "a time.Time field") {
		t.Fatalf("a date of a string: %v", err)
	}
	type BadPattern struct {
		ID   int64
		Name string
	}
	vuka.RegisterFields[BadPattern](map[string][]any{"Name": {orm.Char{Pattern: "("}}})
	if err := orm.For[BadPattern]().Err(); err == nil || !strings.Contains(err.Error(), "Pattern") {
		t.Fatalf("a bad pattern: %v", err)
	}
	type BadBound struct {
		ID  int64
		Age int
	}
	vuka.RegisterFields[BadBound](map[string][]any{"Age": {orm.Int{Min: "0"}}})
	if err := orm.For[BadBound]().Err(); err == nil || !strings.Contains(err.Error(), "Min is an integer") {
		t.Fatalf("a bound of text: %v", err)
	}
}

// TestFieldMigrations applies the column properties the attributes make
// to the database ORMTEST_DRIVER names, then changes them.
func TestFieldMigrations(t *testing.T) {
	ctx := ormtest.Open(t)
	d, _ := orm.DBFrom(ctx)
	collation := map[string]string{"postgres": "C", "mysql": "utf8mb4_bin", "sqlite": "NOCASE"}[ormtest.Driver()]
	create := migration.Migration{Name: "0001_invoices", Operations: []migration.Operation{migration.CreateModel{Table: "invoices", Fields: []migration.NamedField{
		migration.F("id", migration.BigAuto()),
		migration.F("ref", migration.Varchar(8).Collation(collation).Comment("the customer's reference")),
		migration.F("status", migration.Varchar(20).Index().Default("draft")),
		migration.F("qty", migration.SmallInt().Unsigned().Default(1)),
		migration.F("phone", migration.Varchar(20)),
		migration.F("email", migration.Varchar(254).Unique()),
		migration.F("placed", migration.Time().Precision(3).DefaultSQL("CURRENT_TIMESTAMP")),
		migration.F("due", migration.Date().DefaultSQL("CURRENT_DATE")),
		migration.F("memo", migration.Varchar(50).Null()),
	}}}}
	alter := migration.Migration{Name: "0002_alter", Dependencies: []string{"0001_invoices"}, Operations: []migration.Operation{
		migration.AlterField{Table: "invoices", Name: "ref", Field: migration.Varchar(12).Collation(collation).Comment("printed on the invoice")},
		migration.AlterField{Table: "invoices", Name: "placed", Field: migration.Time().Precision(6).DefaultSQL("CURRENT_TIMESTAMP")},
		migration.AlterField{Table: "invoices", Name: "status", Field: migration.Varchar(20).Index().Default("new")},
		migration.AddField{Table: "invoices", Name: "rank", Field: migration.SmallInt().Comment("sorts the list").Default(0)},
	}}
	if err := orm.ApplyMigrations(ctx, d, "", create, alter); err != nil {
		t.Fatal(err)
	}
	inv := Invoice{Ref: "A-1", Phone: "+1 2", Email: "a@x", Due: time.Now()}
	if err := Invoices.Create(ctx, &inv); err != nil {
		t.Fatal(err)
	}
	ormtest.Exec(t, ctx, "INSERT INTO invoices (ref, phone, email) VALUES ('B-2', '+3', 'b@x')")
	raw := must(Invoices.Get(ctx, orm.Q{"ref": "B-2"}))
	if raw.Status != "new" || raw.Qty != 1 || raw.Placed.IsZero() || raw.Due.IsZero() {
		t.Fatalf("the migrated defaults: %+v", raw)
	}
	if err := orm.ApplyMigrations(ctx, d, "zero", create, alter); err != nil {
		t.Fatalf("unapplying: %v", err)
	}
}
