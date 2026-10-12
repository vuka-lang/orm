// Package schema is the ORM's tables as the database needs them, and the
// SQL that creates and changes them on each dialect. A table is described
// once for every dialect: what differs (a generated column's expression,
// a declared index) is held per dialect. The ORM builds it from its
// models (orm.CreateTables, the migration autodetector), and migrations
// replay their operations into it.
package schema

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kind is what a column holds, as the database types it.
type Kind string

const (
	Bool    Kind = "bool"
	Int     Kind = "int" // 64-bit
	Int32   Kind = "int32"
	Int16   Kind = "int16" // a SMALLINT
	Float   Kind = "float"
	Float32 Kind = "float32"
	String  Kind = "string"
	Time    Kind = "time"
	Date    Kind = "date" // a DATE: the day alone
	UUID    Kind = "uuid" // uuid on Postgres, CHAR(36) elsewhere
	Bytes   Kind = "bytes"
	JSON    Kind = "json"
	Custom  Kind = "custom" // a Scanner/Valuer: TEXT unless typed
)

func (k Kind) numeric() bool {
	return k == Int || k == Int32 || k == Int16 || k == Float || k == Float32
}

// Dialects is SQL written for each dialect: "" where a dialect has none.
type Dialects struct {
	Postgres, MySQL, SQLite string
}

// For is the SQL of the dialect named.
func (x Dialects) For(dialect string) string {
	switch dialect {
	case "postgres":
		return x.Postgres
	case "mysql":
		return x.MySQL
	}
	return x.SQLite
}

// Set is x with the dialect's SQL set to s.
func (x Dialects) Set(dialect, s string) Dialects {
	switch dialect {
	case "postgres":
		x.Postgres = s
	case "mysql":
		x.MySQL = s
	default:
		x.SQLite = s
	}
	return x
}

func (x Dialects) Zero() bool { return x == Dialects{} }

// Names is the dialects, in the order SQL is written for them.
var Names = []string{"postgres", "mysql", "sqlite"}

// Column is a table's column.
type Column struct {
	Name     string
	Kind     Kind
	Nullable bool
	PK       bool
	Auto     bool // an integer key the database counts
	Unique   bool
	Index    bool
	Size     int
	Type     string // a database type of the model's own
	Vector   int    // a pgvector column of so many dimensions (-1: any)
	TSVector bool   // a Postgres tsvector column
	// Default is the SQL literal a row written without the column takes.
	// CURRENT_TIMESTAMP and CURRENT_DATE are written as each dialect
	// needs them (MySQL's of the column's precision, its date in
	// parentheses).
	Default string
	// Types is the column's type on each dialect, its Go type's
	// (ColumnType): where one is given it wins over the kind's.
	Types Dialects
	// Comment is the column's comment (Postgres, MySQL; SQLite has none);
	// Collation its collation; Precision a timestamp's fractional digits
	// (0: the dialect's default); Unsigned an integer UNSIGNED on MySQL.
	Comment   string
	Collation string
	Precision int
	Unsigned  bool
	// Generated is the expression a stored generated column is computed
	// by, on each dialect.
	Generated Dialects
	FK        *ForeignKey

	// The names of the column's unique constraint and index, given when
	// the column comes to have them (see Table.Named).
	UniqueName, IndexName string
}

// Same is whether c and o define a column alike, their names and their
// constraints' aside.
func (c Column) Same(o Column) bool {
	a, b := c.bare(), o.bare()
	fa, fb := a.FK, b.FK
	a.FK, b.FK = nil, nil
	return a == b && (fa == nil) == (fb == nil) && (fa == nil || *fa == *fb)
}

func (c Column) bare() Column {
	c.Name, c.UniqueName, c.IndexName = "", "", ""
	if c.FK != nil {
		fk := *c.FK
		fk.Name = ""
		c.FK = &fk
	}
	return c
}

// ForeignKey is a column referring to another table's key.
type ForeignKey struct {
	Table    string
	Ref      string
	OnDelete string // cascade, set_null, restrict
	Name     string
}

// Constraint is a table's unique constraint over columns, or, with
// Check, its check constraint.
type Constraint struct {
	Name    string
	Columns []string
	Check   string
}

// Index is an index a model declares (GIN, HNSW, FULLTEXT, …), as the
// statement creating it on each dialect: none where it has no SQL. Drop
// is the statements dropping it where DROP INDEX doesn't: SQLite's
// full-text table and the triggers keeping it.
type Index struct {
	Name string
	SQL  Dialects
	Drop Dialects
}

// Table is a table: its columns in order, its constraints, its declared
// indexes.
type Table struct {
	Name        string
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
}

// Column is t's column named name.
func (t *Table) Column(name string) (*Column, bool) {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i], true
		}
	}
	return nil, false
}

// Named is t with the constraints and indexes its columns hold named
// where they aren't: after the table and column, as the table is named
// now. Names stay with a renamed table or column.
func (t Table) Named() Table {
	t.Columns = slices.Clone(t.Columns)
	for i := range t.Columns {
		t.Columns[i].Named(t.Name)
	}
	return t
}

// Named names c's unique constraint, index and foreign key, where it has
// them unnamed, after table; and drops the names of those it hasn't.
func (c *Column) Named(table string) {
	if c.Unique && !c.PK && c.UniqueName == "" {
		c.UniqueName = Ident("uniq_" + table + "_" + c.Name)
	}
	if c.Index && !c.Unique && !c.PK && c.IndexName == "" {
		c.IndexName = Ident("idx_" + table + "_" + c.Name)
	}
	if !c.Unique || c.PK {
		c.UniqueName = ""
	}
	if !c.Index || c.Unique || c.PK {
		c.IndexName = ""
	}
	if c.FK != nil && c.FK.Name == "" {
		fk := *c.FK
		fk.Name = Ident("fk_" + table + "_" + c.Name)
		c.FK = &fk
	}
}

// Ident is name as every dialect takes an identifier: at most 63 bytes,
// a longer one cut and told apart by a hash of it.
func Ident(name string) string {
	if len(name) <= 63 {
		return name
	}
	h := sha256.Sum256([]byte(name))
	return name[:54] + "_" + hex.EncodeToString(h[:4])
}

// Dialect is how a database writes the schema.
type Dialect struct {
	Name  string // postgres, mysql, sqlite
	Quote func(string) string
	// Vectors is whether a MySQL server has the VECTOR type (MySQL 9,
	// MariaDB 11.7): without it a vector is a VARBINARY of the same bytes.
	Vectors bool
}

// typ is c's SQL type, its key clause excluded; keyed says an index or
// constraint holds it (MySQL can't key TEXT).
func (d Dialect) typ(c Column, keyed bool) string {
	t := d.baseType(c, keyed)
	if c.Unsigned && d.Name == "mysql" && c.Type == "" && c.Types.For(d.Name) == "" && (c.Kind == Int || c.Kind == Int32 || c.Kind == Int16) {
		t += " UNSIGNED"
	}
	return t
}

// fullType is c's type with its collation.
func (d Dialect) fullType(c Column, keyed bool) string {
	t := d.typ(c, keyed)
	if c.Collation != "" {
		if d.Name == "postgres" {
			t += " COLLATE " + d.Quote(c.Collation)
		} else {
			t += " COLLATE " + c.Collation
		}
	}
	return t
}

// defaultSQL is c's default as d writes it: the current time of the
// column's precision on MySQL, its current date in parentheses.
func (d Dialect) defaultSQL(c Column) string {
	if d.Name != "mysql" {
		return c.Default
	}
	switch strings.ToUpper(c.Default) {
	case "CURRENT_TIMESTAMP", "NOW()", "CURRENT_TIMESTAMP()":
		if c.Kind == Time && c.Type == "" {
			return "CURRENT_TIMESTAMP(" + strconv.Itoa(cmp.Or(c.Precision, 6)) + ")"
		}
	case "CURRENT_DATE", "CURRENT_DATE()":
		return "(CURRENT_DATE)"
	}
	return c.Default
}

func (d Dialect) baseType(c Column, keyed bool) string {
	switch {
	case c.Type != "":
		return c.Type
	case c.Types.For(d.Name) != "":
		return c.Types.For(d.Name)
	case c.Vector != 0 && d.Name == "postgres":
		if c.Vector > 0 {
			return "vector(" + strconv.Itoa(c.Vector) + ")"
		}
		return "vector"
	case c.TSVector && d.Name == "postgres":
		return "tsvector"
	case c.Vector != 0 && d.Name == "mysql":
		switch {
		case c.Vector < 0:
			return "LONGBLOB"
		case d.Vectors:
			return "VECTOR(" + strconv.Itoa(c.Vector) + ")"
		}
		return "VARBINARY(" + strconv.Itoa(4*c.Vector) + ")"
	}
	switch c.Kind {
	case Bool:
		return "BOOLEAN"
	case Int:
		if c.Auto {
			switch d.Name {
			case "postgres":
				return "BIGSERIAL"
			case "sqlite":
				return "INTEGER"
			}
		}
		return "BIGINT"
	case Int32:
		if c.Auto && d.Name == "postgres" {
			return "SERIAL"
		}
		return "INTEGER"
	case Int16:
		switch {
		case c.Auto && d.Name == "postgres":
			return "SMALLSERIAL"
		case c.Auto && d.Name == "sqlite":
			return "INTEGER"
		}
		return "SMALLINT"
	case Float:
		switch d.Name {
		case "postgres":
			return "DOUBLE PRECISION"
		case "mysql":
			return "DOUBLE"
		}
		return "REAL"
	case Float32:
		return "REAL"
	case String:
		if c.Size > 0 {
			return "VARCHAR(" + strconv.Itoa(c.Size) + ")"
		}
		if d.Name == "mysql" && keyed {
			return "VARCHAR(255)"
		}
		return "TEXT"
	case Time:
		switch {
		case d.Name == "postgres" && c.Precision > 0:
			return "TIMESTAMP(" + strconv.Itoa(c.Precision) + ")"
		case d.Name == "postgres":
			return "TIMESTAMP"
		case d.Name == "mysql":
			return "DATETIME(" + strconv.Itoa(cmp.Or(c.Precision, 6)) + ")"
		}
		return "DATETIME"
	case Date:
		return "DATE"
	case UUID:
		if d.Name == "postgres" {
			return "uuid"
		}
		return "CHAR(36)"
	case Bytes:
		switch d.Name {
		case "postgres":
			return "BYTEA"
		case "mysql":
			return "LONGBLOB"
		}
		return "BLOB"
	case JSON:
		switch d.Name {
		case "postgres":
			return "jsonb"
		case "mysql":
			return "JSON"
		}
		return "TEXT"
	}
	return "TEXT"
}

// keyed is whether an index or constraint of t holds c.
func (t Table) keyed(c Column) bool {
	if c.PK || c.Unique || c.Index {
		return true
	}
	for _, k := range t.Constraints {
		if k.Check == "" && slices.Contains(k.Columns, c.Name) {
			return true
		}
	}
	return false
}

// Check is what keeps t from being made on d: a generated column with
// no expression for d.
func (d Dialect) Check(t Table) error {
	for _, c := range t.Columns {
		if !c.Generated.Zero() && c.Generated.For(d.Name) == "" {
			return fmt.Errorf("%s.%s: the generated column has no expression for %s", t.Name, c.Name, d.Name)
		}
	}
	return nil
}

// columnSQL is a column's definition in CREATE TABLE or ADD COLUMN.
func (d Dialect) columnSQL(t Table, c Column, singlePK bool) string {
	s := d.Quote(c.Name) + " " + d.fullType(c, t.keyed(c))
	if g := c.Generated.For(d.Name); g != "" {
		return s + " GENERATED ALWAYS AS (" + g + ") STORED" + d.inlineComment(c)
	}
	if c.PK && singlePK {
		s += " PRIMARY KEY"
		if c.Auto {
			switch d.Name {
			case "sqlite":
				s += " AUTOINCREMENT"
			case "mysql":
				s += " AUTO_INCREMENT"
			}
		}
		return s + d.inlineComment(c)
	}
	if !c.Nullable {
		s += " NOT NULL"
	}
	if c.Default != "" {
		s += " DEFAULT " + d.defaultSQL(c)
	}
	return s + d.inlineComment(c)
}

// inlineComment is c's comment as MySQL writes it in the column's
// definition; Postgres comments by a statement of its own (comment).
func (d Dialect) inlineComment(c Column) string {
	if d.Name != "mysql" || c.Comment == "" {
		return ""
	}
	return " COMMENT " + literal(c.Comment)
}

// comment is the statement setting (or, empty, removing) a column's
// comment on Postgres; none elsewhere.
func (d Dialect) comment(table string, c Column) []string {
	if d.Name != "postgres" {
		return nil
	}
	v := "NULL"
	if c.Comment != "" {
		v = literal(c.Comment)
	}
	return []string{"COMMENT ON COLUMN " + d.Quote(table) + "." + d.Quote(c.Name) + " IS " + v}
}

// literal is s as a SQL string literal.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (t Table) pks() []string {
	var out []string
	for _, c := range t.Columns {
		if c.PK {
			out = append(out, c.Name)
		}
	}
	return out
}

func (d Dialect) fkSQL(col string, fk ForeignKey) string {
	s := "CONSTRAINT " + d.Quote(fk.Name) + " FOREIGN KEY (" + d.Quote(col) + ") REFERENCES " + d.Quote(fk.Table) + " (" + d.Quote(fk.Ref) + ")"
	switch fk.OnDelete {
	case "cascade":
		s += " ON DELETE CASCADE"
	case "set_null":
		s += " ON DELETE SET NULL"
	case "restrict":
		s += " ON DELETE RESTRICT"
	}
	return s
}

func (d Dialect) constraintSQL(k Constraint) string {
	if k.Check != "" {
		return "CONSTRAINT " + d.Quote(k.Name) + " CHECK (" + k.Check + ")"
	}
	return "CONSTRAINT " + d.Quote(k.Name) + " UNIQUE (" + d.list(k.Columns) + ")"
}

func (d Dialect) list(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = d.Quote(n)
	}
	return strings.Join(q, ", ")
}

// Create is the statements creating t, its names given (Named): the
// table, its constraints and foreign keys inside it, then its indexes.
// ifNotExists leaves a table that is there alone.
func (d Dialect) Create(t Table, ifNotExists bool) []string {
	t = t.Named()
	pks := t.pks()
	var defs []string
	for _, c := range t.Columns {
		defs = append(defs, d.columnSQL(t, c, len(pks) == 1))
	}
	if len(pks) > 1 {
		defs = append(defs, "PRIMARY KEY ("+d.list(pks)+")")
	}
	for _, c := range t.Columns {
		if c.UniqueName != "" {
			defs = append(defs, d.constraintSQL(Constraint{Name: c.UniqueName, Columns: []string{c.Name}}))
		}
	}
	for _, k := range t.Constraints {
		defs = append(defs, d.constraintSQL(k))
	}
	for _, c := range t.Columns {
		if c.FK == nil {
			continue
		}
		// MySQL keys a foreign key by an index that leads with its column,
		// a unique constraint's if one does: then that constraint can't be
		// dropped. The foreign key gets its own.
		if d.Name == "mysql" {
			defs = append(defs, "KEY "+d.Quote(c.FK.Name)+" ("+d.Quote(c.Name)+")")
		}
		defs = append(defs, d.fkSQL(c.Name, *c.FK))
	}
	head := "CREATE TABLE "
	if ifNotExists {
		head += "IF NOT EXISTS "
	}
	out := []string{head + d.Quote(t.Name) + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"}
	for _, c := range t.Columns {
		if c.Comment != "" {
			out = append(out, d.comment(t.Name, c)...)
		}
	}
	for _, c := range t.Columns {
		if c.IndexName != "" {
			out = append(out, d.createIndex(t.Name, c.IndexName, c.Name, ifNotExists))
		}
	}
	for _, ix := range t.Indexes {
		if s := ix.SQL.For(d.Name); s != "" {
			if ifNotExists && d.Name != "mysql" {
				s = strings.Replace(s, "CREATE INDEX ", "CREATE INDEX IF NOT EXISTS ", 1)
			}
			out = append(out, s)
		}
	}
	return out
}

func (d Dialect) createIndex(table, name, col string, ifNotExists bool) string {
	s := "CREATE INDEX "
	if ifNotExists && d.Name != "mysql" {
		s += "IF NOT EXISTS "
	}
	return s + d.Quote(name) + " ON " + d.Quote(table) + " (" + d.Quote(col) + ")"
}

// DropTable drops a table.
func (d Dialect) DropTable(name string) []string { return []string{"DROP TABLE " + d.Quote(name)} }

// RenameTable renames a table: the constraints and indexes keep their
// names, and the foreign keys referring to it follow.
func (d Dialect) RenameTable(from, to string) []string {
	return []string{"ALTER TABLE " + d.Quote(from) + " RENAME TO " + d.Quote(to)}
}

// RenameColumn renames a column of table.
func (d Dialect) RenameColumn(table, from, to string) []string {
	return []string{"ALTER TABLE " + d.Quote(table) + " RENAME COLUMN " + d.Quote(from) + " TO " + d.Quote(to)}
}

// Simple is whether SQLite adds (or drops) c with ALTER TABLE: no key,
// constraint, foreign key or generated expression, and a value for rows
// that exist. Others rebuild the table (Remake).
func Simple(c Column) bool {
	return !c.PK && !c.Unique && c.FK == nil && c.Generated.Zero() && (c.Nullable || c.Default != "")
}

// AddColumn adds c to the table t (t as it is with c), and its
// constraint, foreign key and index. Not SQLite's: see Simple.
func (d Dialect) AddColumn(t Table, c Column) []string {
	tq := d.Quote(t.Name)
	out := []string{"ALTER TABLE " + tq + " ADD COLUMN " + d.columnSQL(t, c, false)}
	if c.Comment != "" {
		out = append(out, d.comment(t.Name, c)...)
	}
	if c.UniqueName != "" {
		out = append(out, "ALTER TABLE "+tq+" ADD "+d.constraintSQL(Constraint{Name: c.UniqueName, Columns: []string{c.Name}}))
	}
	if c.FK != nil && d.Name != "sqlite" {
		out = append(out, "ALTER TABLE "+tq+" ADD "+d.fkSQL(c.Name, *c.FK))
	}
	if c.IndexName != "" {
		out = append(out, d.createIndex(t.Name, c.IndexName, c.Name, false))
	}
	return out
}

// DropColumn drops c from table: its foreign key first on MySQL, which
// won't drop a column one holds.
func (d Dialect) DropColumn(table string, c Column) []string {
	tq := d.Quote(table)
	var out []string
	if c.FK != nil && d.Name == "mysql" {
		out = append(out, "ALTER TABLE "+tq+" DROP FOREIGN KEY "+d.Quote(c.FK.Name))
	}
	if c.IndexName != "" && d.Name == "sqlite" {
		out = append(out, d.DropIndex(table, c.IndexName)...)
	}
	return append(out, "ALTER TABLE "+tq+" DROP COLUMN "+d.Quote(c.Name))
}

// CreateIndex is the statement creating a declared index on d, none when
// d has no SQL for it.
func (d Dialect) CreateIndex(ix Index) []string {
	if s := ix.SQL.For(d.Name); s != "" {
		return []string{s}
	}
	return nil
}

// DropDeclared drops the declared index ix of table: by its Drop where it
// has one, else DROP INDEX; nothing where d has no SQL making it.
func (d Dialect) DropDeclared(table string, ix Index) []string {
	if s := ix.Drop.For(d.Name); s != "" {
		return []string{s}
	}
	if ix.SQL.For(d.Name) == "" {
		return nil
	}
	return d.DropIndex(table, ix.Name)
}

// DropAll drops t: first what of it DROP TABLE leaves behind (a declared
// index's Drop), then the table.
func (d Dialect) DropAll(t Table) []string {
	var out []string
	for _, ix := range t.Indexes {
		if s := ix.Drop.For(d.Name); s != "" {
			out = append(out, s)
		}
	}
	return append(out, d.DropTable(t.Name)...)
}

// DropIndex drops the index named of table.
func (d Dialect) DropIndex(table, name string) []string {
	if d.Name == "mysql" {
		return []string{"DROP INDEX " + d.Quote(name) + " ON " + d.Quote(table)}
	}
	return []string{"DROP INDEX " + d.Quote(name)}
}

// AddConstraint adds a unique or check constraint to table. Not
// SQLite's: it rebuilds the table.
func (d Dialect) AddConstraint(table string, k Constraint) []string {
	return []string{"ALTER TABLE " + d.Quote(table) + " ADD " + d.constraintSQL(k)}
}

// DropConstraint drops a unique or check constraint of table. Not
// SQLite's: it rebuilds the table.
func (d Dialect) DropConstraint(table string, k Constraint) []string {
	tq := d.Quote(table)
	if d.Name == "mysql" {
		if k.Check != "" {
			return []string{"ALTER TABLE " + tq + " DROP CHECK " + d.Quote(k.Name)}
		}
		return []string{"ALTER TABLE " + tq + " DROP INDEX " + d.Quote(k.Name)}
	}
	return []string{"ALTER TABLE " + tq + " DROP CONSTRAINT " + d.Quote(k.Name)}
}

func (d Dialect) dropFK(table string, fk ForeignKey) string {
	if d.Name == "mysql" {
		return "ALTER TABLE " + d.Quote(table) + " DROP FOREIGN KEY " + d.Quote(fk.Name)
	}
	return "ALTER TABLE " + d.Quote(table) + " DROP CONSTRAINT " + d.Quote(fk.Name)
}

// AlterColumn changes column o of the table t (t as it is after) to c:
// its type, nullability, default, unique constraint, index, foreign key
// and generated expression. Not SQLite's: it rebuilds the table.
func (d Dialect) AlterColumn(t Table, o, c Column) []string {
	tq, cq := d.Quote(t.Name), d.Quote(c.Name)
	var out []string
	// What holds the column goes first and comes back last: MySQL can't
	// make a keyed column TEXT, nor key a TEXT one.
	if o.FK != nil && (c.FK == nil || *o.FK != *c.FK) {
		out = append(out, d.dropFK(t.Name, *o.FK))
	}
	if o.UniqueName != "" && o.UniqueName != c.UniqueName {
		out = append(out, d.DropConstraint(t.Name, Constraint{Name: o.UniqueName})...)
	}
	if o.IndexName != "" && o.IndexName != c.IndexName {
		out = append(out, d.DropIndex(t.Name, o.IndexName)...)
	}
	if o.Generated != c.Generated {
		// A generated column changes by being made again.
		out = append(out, "ALTER TABLE "+tq+" DROP COLUMN "+cq, "ALTER TABLE "+tq+" ADD COLUMN "+d.columnSQL(t, c, false))
	} else if d.fullType(o, t.keyed(o)) != d.fullType(c, t.keyed(c)) || o.Nullable != c.Nullable || d.defaultSQL(o) != d.defaultSQL(c) || o.Auto != c.Auto ||
		d.Name == "mysql" && o.Comment != c.Comment {
		out = append(out, d.changeColumn(t, o, c)...)
	}
	if o.Comment != c.Comment && o.Generated == c.Generated {
		out = append(out, d.comment(t.Name, c)...)
	} else if o.Generated != c.Generated && c.Comment != "" {
		out = append(out, d.comment(t.Name, c)...)
	}
	if c.UniqueName != "" && o.UniqueName != c.UniqueName {
		out = append(out, d.AddConstraint(t.Name, Constraint{Name: c.UniqueName, Columns: []string{c.Name}})...)
	}
	if c.IndexName != "" && o.IndexName != c.IndexName {
		out = append(out, d.createIndex(t.Name, c.IndexName, c.Name, false))
	}
	if c.FK != nil && (o.FK == nil || *o.FK != *c.FK) {
		out = append(out, "ALTER TABLE "+tq+" ADD "+d.fkSQL(c.Name, *c.FK))
	}
	return out
}

func (d Dialect) changeColumn(t Table, o, c Column) []string {
	tq, cq := d.Quote(t.Name), d.Quote(c.Name)
	if d.Name == "mysql" {
		// The column's keys stay as they are.
		s := "ALTER TABLE " + tq + " MODIFY COLUMN " + cq + " " + d.fullType(c, t.keyed(c))
		if !c.Nullable {
			s += " NOT NULL"
		}
		if c.Default != "" {
			s += " DEFAULT " + d.defaultSQL(c)
		}
		if c.Auto {
			s += " AUTO_INCREMENT"
		}
		return []string{s + d.inlineComment(c)}
	}
	var out []string
	if d.fullType(o, false) != d.fullType(c, false) {
		// SERIAL and BIGSERIAL are no types: the column is the integer,
		// and the sequence it owns counts as far.
		plain := c
		plain.Auto = false
		typ := d.typ(plain, false)
		using := cq + "::" + typ
		full := d.fullType(plain, false)
		switch {
		case c.Kind == Bool && o.Kind.numeric():
			using = cq + " <> 0"
		case o.Kind == Bool && c.Kind.numeric():
			using = "CASE WHEN " + cq + " THEN 1 ELSE 0 END"
		}
		if o.Default != "" {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" DROP DEFAULT")
		}
		out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" TYPE "+full+" USING "+using)
		if o.Auto && c.Auto {
			out = append(out, "ALTER SEQUENCE "+d.Quote(t.Name+"_"+c.Name+"_seq")+" AS "+typ)
		}
		if c.Default != "" {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" SET DEFAULT "+d.defaultSQL(c))
		}
	} else if o.Default != c.Default {
		if c.Default == "" {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" DROP DEFAULT")
		} else {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" SET DEFAULT "+d.defaultSQL(c))
		}
	}
	if o.Nullable != c.Nullable {
		if c.Nullable {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" DROP NOT NULL")
		} else {
			out = append(out, "ALTER TABLE "+tq+" ALTER COLUMN "+cq+" SET NOT NULL")
		}
	}
	return out
}

// Remake is SQLite's change of a table it can't ALTER: the table made
// again as to is, the rows of from copied over (columns of the same name,
// generated ones aside), from dropped and the new table renamed to it,
// then to's indexes. The connection must have foreign keys off.
func (d Dialect) Remake(from, to Table) []string {
	tmp := to
	tmp.Name = "orm__new_" + to.Name
	tmp.Indexes = nil
	tmp.Columns = slices.Clone(to.Columns)
	for i := range tmp.Columns {
		tmp.Columns[i].Index = false
	}
	out := d.Create(tmp, false)[:1]
	var cols []string
	for _, c := range to.Columns {
		if oc, ok := from.Column(c.Name); ok && oc.Generated.Zero() && c.Generated.Zero() {
			cols = append(cols, d.Quote(c.Name))
		}
	}
	if len(cols) > 0 {
		list := strings.Join(cols, ", ")
		out = append(out, "INSERT INTO "+d.Quote(tmp.Name)+" ("+list+") SELECT "+list+" FROM "+d.Quote(from.Name))
	}
	out = append(out, "DROP TABLE "+d.Quote(from.Name), "ALTER TABLE "+d.Quote(tmp.Name)+" RENAME TO "+d.Quote(to.Name))
	return append(out, d.Create(to, false)[1:]...)
}

// CreateExtension and DropExtension install and remove a Postgres
// extension; other dialects have none.
func (d Dialect) CreateExtension(name string) []string {
	if d.Name != "postgres" {
		return nil
	}
	return []string{"CREATE EXTENSION IF NOT EXISTS " + d.Quote(name)}
}

func (d Dialect) DropExtension(name string) []string {
	if d.Name != "postgres" {
		return nil
	}
	return []string{"DROP EXTENSION IF EXISTS " + d.Quote(name)}
}

// Sort orders tables so each comes after the tables its foreign keys
// refer to (a cycle keeps its order).
func Sort(tables []Table) []Table {
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	var out []Table
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string)
	visit = func(n string) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		for _, c := range byName[n].Columns {
			if c.FK != nil {
				if _, ok := byName[c.FK.Table]; ok && c.FK.Table != n {
					visit(c.FK.Table)
				}
			}
		}
		state[n] = 2
		out = append(out, byName[n])
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

// For is the dialect named (postgres, mysql, sqlite), quoting as the ORM
// does. A MySQL server's version may follow a slash, as VERSION() gives
// it (mysql/9.3.0, mysql/11.7.2-MariaDB): it says whether the server has
// the VECTOR type.
func For(name string) Dialect {
	name, version, _ := strings.Cut(name, "/")
	if name == "mysql" {
		return Dialect{Name: name, Quote: func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }, Vectors: hasVectors(version)}
	}
	return Dialect{Name: name, Quote: func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }}
}

// hasVectors is whether a MySQL server of version has the VECTOR type:
// MySQL from 9.0, MariaDB from 11.7.
func hasVectors(version string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		return false
	}
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return major > 11 || major == 11 && minor >= 7
	}
	return major >= 9
}
