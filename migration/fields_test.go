package migration

import (
	"slices"
	"strings"
	"testing"

	"github.com/vuka-lang/orm/internal/schema"
)

// orders has a column of every property the field attributes set.
func orders() Migration {
	return Migration{Name: "0001", Operations: []Operation{CreateModel{Table: "orders", Fields: []NamedField{
		F("id", UUID().PK()),
		F("ref", Varchar(8).Collation("utf8mb4_bin").Comment("the customer's reference")),
		F("status", Varchar(20).Default("draft")),
		F("qty", SmallInt().Unsigned().Default(1)),
		F("placed", Time().Precision(3).DefaultSQL("CURRENT_TIMESTAMP")),
		F("due", Date().DefaultSQL("CURRENT_DATE")),
		F("phone", Custom().Types(Dialects{Postgres: "VARCHAR(16)", MySQL: "VARCHAR(16)", SQLite: "TEXT"})),
	}}}}
}

func TestFieldPropertiesSQL(t *testing.T) {
	for _, c := range []struct {
		dialect string
		want    []string
	}{
		{"postgres", []string{
			"CREATE TABLE \"orders\" (\n  \"id\" uuid PRIMARY KEY,\n  \"ref\" VARCHAR(8) COLLATE \"utf8mb4_bin\" NOT NULL,\n  \"status\" VARCHAR(20) NOT NULL DEFAULT 'draft',\n" +
				"  \"qty\" SMALLINT NOT NULL DEFAULT 1,\n  \"placed\" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,\n  \"due\" DATE NOT NULL DEFAULT CURRENT_DATE,\n  \"phone\" VARCHAR(16) NOT NULL\n)",
			`COMMENT ON COLUMN "orders"."ref" IS 'the customer''s reference'`,
		}},
		{"mysql", []string{
			"CREATE TABLE `orders` (\n  `id` CHAR(36) PRIMARY KEY,\n  `ref` VARCHAR(8) COLLATE utf8mb4_bin NOT NULL COMMENT 'the customer''s reference',\n  `status` VARCHAR(20) NOT NULL DEFAULT 'draft',\n" +
				"  `qty` SMALLINT UNSIGNED NOT NULL DEFAULT 1,\n  `placed` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),\n  `due` DATE NOT NULL DEFAULT (CURRENT_DATE),\n  `phone` VARCHAR(16) NOT NULL\n)",
		}},
		{"sqlite", []string{
			"CREATE TABLE \"orders\" (\n  \"id\" CHAR(36) PRIMARY KEY,\n  \"ref\" VARCHAR(8) COLLATE utf8mb4_bin NOT NULL,\n  \"status\" VARCHAR(20) NOT NULL DEFAULT 'draft',\n" +
				"  \"qty\" SMALLINT NOT NULL DEFAULT 1,\n  \"placed\" DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,\n  \"due\" DATE NOT NULL DEFAULT CURRENT_DATE,\n  \"phone\" TEXT NOT NULL\n)",
		}},
	} {
		if got := sqlOf(t, orders(), c.dialect, NewState(nil, nil), false); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.dialect, got, c.want)
		}
	}

	alter := Migration{Name: "0002", Operations: []Operation{
		AlterField{"orders", "ref", Varchar(8).Comment("printed on the invoice")},
		AlterField{"orders", "placed", Time().Precision(6).DefaultSQL("CURRENT_TIMESTAMP")},
		AlterField{"orders", "status", Varchar(20).Default("new")},
	}}
	for _, c := range []struct {
		dialect string
		want    []string
	}{
		{"postgres", []string{
			`ALTER TABLE "orders" ALTER COLUMN "ref" TYPE VARCHAR(8) USING "ref"::VARCHAR(8)`,
			`COMMENT ON COLUMN "orders"."ref" IS 'printed on the invoice'`,
			`ALTER TABLE "orders" ALTER COLUMN "placed" DROP DEFAULT`,
			`ALTER TABLE "orders" ALTER COLUMN "placed" TYPE TIMESTAMP(6) USING "placed"::TIMESTAMP(6)`,
			`ALTER TABLE "orders" ALTER COLUMN "placed" SET DEFAULT CURRENT_TIMESTAMP`,
			`ALTER TABLE "orders" ALTER COLUMN "status" SET DEFAULT 'new'`,
		}},
		{"mysql", []string{
			"ALTER TABLE `orders` MODIFY COLUMN `ref` VARCHAR(8) NOT NULL COMMENT 'printed on the invoice'",
			"ALTER TABLE `orders` MODIFY COLUMN `placed` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)",
			"ALTER TABLE `orders` MODIFY COLUMN `status` VARCHAR(20) NOT NULL DEFAULT 'new'",
		}},
	} {
		s := NewState(nil, nil)
		if err := orders().Apply(s); err != nil {
			t.Fatal(err)
		}
		if got := sqlOf(t, alter, c.dialect, s, false); !slices.Equal(got, c.want) {
			t.Errorf("%s alter:\n got %q\nwant %q", c.dialect, got, c.want)
		}
	}
}

func TestFieldPropertiesDiffAndSource(t *testing.T) {
	s := NewState(nil, nil)
	if err := orders().Apply(s); err != nil {
		t.Fatal(err)
	}
	// The models as the migration made them: nothing to do.
	want := s.Clone()
	if ops, _ := Diff(s, want, never); len(ops) != 0 {
		t.Fatalf("a replay differs: %v", ops)
	}
	// A model's own default, comment and precision changing is an alter.
	to := s.Clone()
	t0 := &to.tables[to.find("orders")]
	c, _ := t0.Column("status")
	c.Default = "'new'"
	c, _ = t0.Column("placed")
	c.Precision = 6
	c, _ = t0.Column("ref")
	c.Comment = ""
	ops, _ := Diff(s, to, never)
	var got []string
	for _, op := range ops {
		got = append(got, op.source())
	}
	if want := []string{
		`m.AlterField{Table: "orders", Name: "ref", Field: m.Varchar(8).Collation("utf8mb4_bin")}`,
		`m.AlterField{Table: "orders", Name: "status", Field: m.Varchar(20).DefaultSQL("'new'")}`,
		`m.AlterField{Table: "orders", Name: "placed", Field: m.Time().Precision(6).DefaultSQL("CURRENT_TIMESTAMP")}`,
	}; !slices.Equal(got, want) {
		t.Fatalf("diff:\n got %q\nwant %q", got, want)
	}
	// A model with no default keeps the one a migration gave.
	keep := s.Clone()
	c, _ = keep.tables[keep.find("orders")].Column("status")
	c.Default = ""
	if ops, _ := Diff(s, keep, never); len(ops) != 0 {
		t.Fatalf("a default only the migrations have was dropped: %v", ops)
	}
	src, err := Source("migrations", orders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		`m.F("id", m.UUID().PK())`,
		`m.F("ref", m.Varchar(8).Collation("utf8mb4_bin").Comment("the customer's reference"))`,
		`m.F("qty", m.SmallInt().Unsigned().DefaultSQL("1"))`,
		`m.F("placed", m.Time().Precision(3).DefaultSQL("CURRENT_TIMESTAMP"))`,
		`m.F("due", m.Date().DefaultSQL("CURRENT_DATE"))`,
		`m.F("phone", m.Custom().Types(m.Dialects{Postgres: "VARCHAR(16)", MySQL: "VARCHAR(16)", SQLite: "TEXT"}))`,
	} {
		if !strings.Contains(string(src), w) {
			t.Errorf("the source lacks %s:\n%s", w, src)
		}
	}
	auto := Field{schema.Column{Kind: schema.Int16, PK: true, Auto: true}}
	if got := auto.source(); got != "m.SmallInt().Auto()" {
		t.Fatalf("a small counted key: %s", got)
	}
}
