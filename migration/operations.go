package migration

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/orm/internal/schema"
)

// Field is a column's definition: a kind (BigAuto, Varchar, Time, …)
// and what its methods add.
type Field struct{ c schema.Column }

// The kinds of column.
func BigAuto() Field         { return Field{schema.Column{Kind: schema.Int, PK: true, Auto: true}} } // a 64-bit key the database counts
func Auto() Field            { return Field{schema.Column{Kind: schema.Int32, PK: true, Auto: true}} }
func BigInt() Field          { return Field{schema.Column{Kind: schema.Int}} }
func Int() Field             { return Field{schema.Column{Kind: schema.Int32}} }
func Float() Field           { return Field{schema.Column{Kind: schema.Float}} }
func Float32() Field         { return Field{schema.Column{Kind: schema.Float32}} }
func Bool() Field            { return Field{schema.Column{Kind: schema.Bool}} }
func Text() Field            { return Field{schema.Column{Kind: schema.String}} }
func Varchar(size int) Field { return Field{schema.Column{Kind: schema.String, Size: size}} }
func Time() Field            { return Field{schema.Column{Kind: schema.Time}} }
func Bytes() Field           { return Field{schema.Column{Kind: schema.Bytes}} }
func Custom() Field          { return Field{schema.Column{Kind: schema.Custom}} } // a Scanner/Valuer's: TEXT unless Type says
func TSVector() Field        { return Field{schema.Column{Kind: schema.String, TSVector: true}} }
func JSON() Field            { return Field{schema.Column{Kind: schema.JSON, Nullable: true}} } // jsonb | JSON | TEXT

// Vector is a pgvector column of dims dimensions (0: any).
func Vector(dims int) Field {
	if dims == 0 {
		dims = -1
	}
	return Field{schema.Column{Kind: schema.Custom, Vector: dims}}
}

// Null lets the column hold NULL.
func (f Field) Null() Field { f.c.Nullable = true; return f }

// PK makes the column (one of) the primary key.
func (f Field) PK() Field { f.c.PK = true; return f }

// Unique makes the column unique.
func (f Field) Unique() Field { f.c.Unique = true; return f }

// Index indexes the column.
func (f Field) Index() Field { f.c.Index = true; return f }

// Type is the column's database type, over its kind's.
func (f Field) Type(sql string) Field { f.c.Type = sql; return f }

// Default is the value rows written without the column take, and rows
// that exist when it is added: a string, number, bool or nil.
func (f Field) Default(v any) Field {
	switch x := v.(type) {
	case nil:
		f.c.Default = "NULL"
	case string:
		f.c.Default = "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case bool:
		f.c.Default = strings.ToUpper(strconv.FormatBool(x))
	default:
		f.c.Default = fmt.Sprint(x)
	}
	return f
}

// DefaultSQL is Default as an SQL expression of your own.
func (f Field) DefaultSQL(expr string) Field { f.c.Default = expr; return f }

// Generated makes the column a stored generated column, computed by the
// expression of each dialect.
func (f Field) Generated(expr Dialects) Field { f.c.Generated = expr; return f }

// FK makes the column a foreign key to table's column ref.
func (f Field) FK(table, ref string) Field {
	fk := schema.ForeignKey{Table: table, Ref: ref}
	if f.c.FK != nil {
		fk.OnDelete = f.c.FK.OnDelete
	}
	f.c.FK = &fk
	return f
}

// OnDelete is what deleting the row a foreign key refers to does: one of
// Cascade, SetNull, Restrict.
func (f Field) OnDelete(action string) Field {
	fk := schema.ForeignKey{OnDelete: action}
	if f.c.FK != nil {
		fk = *f.c.FK
		fk.OnDelete = action
	}
	f.c.FK = &fk
	return f
}

// The actions of OnDelete.
const (
	Cascade  = "cascade"
	SetNull  = "set_null"
	Restrict = "restrict"
)

// NamedField is a column of CreateModel: its name and Field.
type NamedField struct {
	Name  string
	Field Field
}

// F is the column name defined by f.
func F(name string, f Field) NamedField { return NamedField{name, f} }

// Dialects is SQL written for each dialect: "" where a dialect has none.
type Dialects = schema.Dialects

// Index is an index a model declares (GIN, HNSW, IVFFlat, FULLTEXT, a
// SearchVector's), as the statement making it on each dialect: none where
// it has no SQL (SQLite makes none of them).
type Index = schema.Index

// Constraint is a table's unique constraint (Unique) or check
// constraint (Check).
type Constraint = schema.Constraint

// Unique is a unique constraint over columns, Django's unique_together.
func Unique(name string, columns ...string) Constraint {
	return Constraint{Name: name, Columns: columns}
}

// Check is a check constraint: rows must satisfy expr, SQL.
func Check(name, expr string) Constraint { return Constraint{Name: name, Check: expr} }

// Operation is a change of the schema: what it does to the state, and
// the SQL doing it, forwards and back.
type Operation interface {
	mutate(s *State) error
	forwards(d schema.Dialect, from, to *State) ([]Step, error)
	backwards(d schema.Dialect, from, to *State) ([]Step, error)
	describe() string
	source() string
}

// Describe is what op does, for people: "add field users.email".
func Describe(op Operation) string { return op.describe() }

// CreateModel creates a table.
type CreateModel struct {
	Table       string
	Fields      []NamedField
	Constraints []Constraint
	Indexes     []Index
}

func (o CreateModel) mutate(s *State) error {
	if s.find(o.Table) >= 0 {
		return errors.New("the table exists")
	}
	t := schema.Table{Name: o.Table, Constraints: slices.Clone(o.Constraints), Indexes: slices.Clone(o.Indexes)}
	for _, f := range o.Fields {
		c := f.Field.c
		c.Name = f.Name
		t.Columns = append(t.Columns, c)
	}
	s.tables = append(s.tables, t.Named())
	return nil
}

func (o CreateModel) forwards(d schema.Dialect, _, to *State) ([]Step, error) {
	t, _ := to.table(o.Table)
	if err := d.Check(*t); err != nil {
		return nil, err
	}
	return sqlSteps(d.Create(*t, false)), nil
}

func (o CreateModel) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.DropTable(o.Table)), nil
}

func (o CreateModel) describe() string { return "create model " + o.Table }

// DeleteModel drops a table.
type DeleteModel struct{ Table string }

func (o DeleteModel) mutate(s *State) error {
	i := s.find(o.Table)
	if i < 0 {
		return errors.New("no such table")
	}
	s.tables = slices.Delete(s.tables, i, i+1)
	return nil
}

func (o DeleteModel) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.DropTable(o.Table)), nil
}

func (o DeleteModel) backwards(d schema.Dialect, from, _ *State) ([]Step, error) {
	t, _ := from.table(o.Table)
	return sqlSteps(d.Create(*t, false)), nil
}

func (o DeleteModel) describe() string { return "delete model " + o.Table }

// RenameModel renames a table, its rows kept: the constraints and indexes
// keep their names, the foreign keys referring to it follow.
type RenameModel struct{ Old, New string }

// AlterModelTable is RenameModel, by Django's name for changing a
// model's table: here a model is its table.
type AlterModelTable = RenameModel

func (o RenameModel) mutate(s *State) error {
	t, err := s.table(o.Old)
	if err != nil {
		return err
	}
	if s.find(o.New) >= 0 {
		return fmt.Errorf("%s exists", o.New)
	}
	t.Name = o.New
	for i := range s.tables {
		for j, c := range s.tables[i].Columns {
			if c.FK != nil && c.FK.Table == o.Old {
				fk := *c.FK
				fk.Table = o.New
				s.tables[i].Columns[j].FK = &fk
			}
		}
	}
	return nil
}

func (o RenameModel) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.RenameTable(o.Old, o.New)), nil
}

func (o RenameModel) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.RenameTable(o.New, o.Old)), nil
}

func (o RenameModel) describe() string { return "rename model " + o.Old + " to " + o.New }

// AddField adds a column to a table.
type AddField struct {
	Table, Name string
	Field       Field
}

func (o AddField) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	if _, ok := t.Column(o.Name); ok {
		return errors.New("the column exists")
	}
	c := o.Field.c
	c.Name = o.Name
	c.Named(t.Name)
	t.Columns = append(t.Columns, c)
	return nil
}

func (o AddField) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return addColumn(d, from, to, o.Table, o.Name)
}

func (o AddField) backwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return dropColumn(d, to, from, o.Table, o.Name)
}

func (o AddField) describe() string { return "add field " + o.Table + "." + o.Name }

// addColumn is the steps adding column name of table, from the state
// without it to the one with.
func addColumn(d schema.Dialect, without, with *State, table, name string) ([]Step, error) {
	t, _ := with.table(table)
	if err := d.Check(*t); err != nil {
		return nil, err
	}
	c, _ := t.Column(name)
	if d.Name == "sqlite" && !schema.Simple(*c) {
		old, _ := without.table(table)
		return sqlSteps(d.Remake(*old, *t)), nil
	}
	return sqlSteps(d.AddColumn(*t, *c)), nil
}

// dropColumn is the steps dropping column name of table, from the state
// with it to the one without.
func dropColumn(d schema.Dialect, with, without *State, table, name string) ([]Step, error) {
	t, _ := with.table(table)
	c, _ := t.Column(name)
	if d.Name == "sqlite" && !schema.Simple(*c) {
		after, _ := without.table(table)
		return sqlSteps(d.Remake(*t, *after)), nil
	}
	return sqlSteps(d.DropColumn(table, *c)), nil
}

// RemoveField drops a column of a table, and its data.
type RemoveField struct{ Table, Name string }

func (o RemoveField) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(t.Columns, func(c schema.Column) bool { return c.Name == o.Name })
	if i < 0 {
		return errors.New("no such column")
	}
	t.Columns = slices.Delete(t.Columns, i, i+1)
	return nil
}

func (o RemoveField) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return dropColumn(d, from, to, o.Table, o.Name)
}

func (o RemoveField) backwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return addColumn(d, to, from, o.Table, o.Name)
}

func (o RemoveField) describe() string { return "remove field " + o.Table + "." + o.Name }

// AlterField changes a column's definition: its type, nullability,
// default, key, foreign key, generated expression.
type AlterField struct {
	Table, Name string
	Field       Field
}

func (o AlterField) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	old, ok := t.Column(o.Name)
	if !ok {
		return errors.New("no such column")
	}
	c := o.Field.c
	c.Name, c.UniqueName, c.IndexName = o.Name, old.UniqueName, old.IndexName
	if c.FK != nil && old.FK != nil {
		a, b := *c.FK, *old.FK
		if a.Name, b.Name = "", ""; a == b {
			c.FK = old.FK
		}
	}
	c.Named(t.Name)
	*old = c
	return nil
}

func (o AlterField) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return alter(d, from, to, o.Table, o.Name)
}

func (o AlterField) backwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return alter(d, to, from, o.Table, o.Name)
}

func alter(d schema.Dialect, from, to *State, table, name string) ([]Step, error) {
	a, _ := from.table(table)
	b, _ := to.table(table)
	if err := d.Check(*b); err != nil {
		return nil, err
	}
	if d.Name == "sqlite" {
		return sqlSteps(d.Remake(*a, *b)), nil
	}
	oc, _ := a.Column(name)
	c, _ := b.Column(name)
	if oc.PK != c.PK {
		return nil, errors.New("a primary key changes by hand: write a RunSQL")
	}
	return sqlSteps(d.AlterColumn(*b, *oc, *c)), nil
}

func (o AlterField) describe() string { return "alter field " + o.Table + "." + o.Name }

// RenameField renames a column, its data kept.
type RenameField struct{ Table, Old, New string }

func (o RenameField) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	c, ok := t.Column(o.Old)
	if !ok {
		return errors.New("no such column")
	}
	if _, taken := t.Column(o.New); taken {
		return fmt.Errorf("%s exists", o.New)
	}
	c.Name = o.New
	for i, k := range t.Constraints {
		t.Constraints[i].Columns = slices.Clone(k.Columns)
		for j, col := range k.Columns {
			if col == o.Old {
				t.Constraints[i].Columns[j] = o.New
			}
		}
	}
	for i := range s.tables {
		for j, c := range s.tables[i].Columns {
			if c.FK != nil && c.FK.Table == o.Table && c.FK.Ref == o.Old {
				fk := *c.FK
				fk.Ref = o.New
				s.tables[i].Columns[j].FK = &fk
			}
		}
	}
	return nil
}

func (o RenameField) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.RenameColumn(o.Table, o.Old, o.New)), nil
}

func (o RenameField) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.RenameColumn(o.Table, o.New, o.Old)), nil
}

func (o RenameField) describe() string {
	return "rename field " + o.Table + "." + o.Old + " to " + o.New
}

// AddIndex makes a declared index.
type AddIndex struct {
	Table string
	Index Index
}

func (o AddIndex) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(t.Indexes, func(ix Index) bool { return ix.Name == o.Index.Name }) {
		return errors.New("the index exists")
	}
	t.Indexes = append(t.Indexes, o.Index)
	return nil
}

func (o AddIndex) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.CreateIndex(o.Index)), nil
}

func (o AddIndex) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	if o.Index.SQL.For(d.Name) == "" {
		return nil, nil
	}
	return sqlSteps(d.DropIndex(o.Table, o.Index.Name)), nil
}

func (o AddIndex) describe() string { return "add index " + o.Index.Name + " on " + o.Table }

// RemoveIndex drops a declared index.
type RemoveIndex struct{ Table, Name string }

func (o RemoveIndex) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(t.Indexes, func(ix Index) bool { return ix.Name == o.Name })
	if i < 0 {
		return errors.New("no such index")
	}
	t.Indexes = slices.Delete(t.Indexes, i, i+1)
	return nil
}

func (o RemoveIndex) index(s *State) Index {
	t, _ := s.table(o.Table)
	return t.Indexes[slices.IndexFunc(t.Indexes, func(ix Index) bool { return ix.Name == o.Name })]
}

func (o RemoveIndex) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return AddIndex{o.Table, o.index(from)}.backwards(d, to, from)
}

func (o RemoveIndex) backwards(d schema.Dialect, from, _ *State) ([]Step, error) {
	return sqlSteps(d.CreateIndex(o.index(from))), nil
}

func (o RemoveIndex) describe() string { return "remove index " + o.Name + " from " + o.Table }

// AddConstraint adds a unique or check constraint to a table.
type AddConstraint struct {
	Table      string
	Constraint Constraint
}

func (o AddConstraint) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(t.Constraints, func(k Constraint) bool { return k.Name == o.Constraint.Name }) {
		return errors.New("the constraint exists")
	}
	t.Constraints = append(t.Constraints, o.Constraint)
	return nil
}

func (o AddConstraint) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return constrain(d, from, to, o.Table, d.AddConstraint(o.Table, o.Constraint))
}

func (o AddConstraint) backwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return constrain(d, to, from, o.Table, d.DropConstraint(o.Table, o.Constraint))
}

// constrain is stmts, or SQLite's rebuild of table from one state to the
// other.
func constrain(d schema.Dialect, from, to *State, table string, stmts []string) ([]Step, error) {
	if d.Name == "sqlite" {
		a, _ := from.table(table)
		b, _ := to.table(table)
		return sqlSteps(d.Remake(*a, *b)), nil
	}
	return sqlSteps(stmts), nil
}

func (o AddConstraint) describe() string {
	return "add constraint " + o.Constraint.Name + " on " + o.Table
}

// RemoveConstraint drops a unique or check constraint of a table.
type RemoveConstraint struct{ Table, Name string }

func (o RemoveConstraint) mutate(s *State) error {
	t, err := s.table(o.Table)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(t.Constraints, func(k Constraint) bool { return k.Name == o.Name })
	if i < 0 {
		return errors.New("no such constraint")
	}
	t.Constraints = slices.Delete(t.Constraints, i, i+1)
	return nil
}

func (o RemoveConstraint) constraint(s *State) Constraint {
	t, _ := s.table(o.Table)
	return t.Constraints[slices.IndexFunc(t.Constraints, func(k Constraint) bool { return k.Name == o.Name })]
}

func (o RemoveConstraint) forwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return constrain(d, from, to, o.Table, d.DropConstraint(o.Table, o.constraint(from)))
}

func (o RemoveConstraint) backwards(d schema.Dialect, from, to *State) ([]Step, error) {
	return constrain(d, to, from, o.Table, d.AddConstraint(o.Table, o.constraint(from)))
}

func (o RemoveConstraint) describe() string {
	return "remove constraint " + o.Name + " from " + o.Table
}

// CreateExtension installs a Postgres extension ("vector", "pg_trgm");
// other databases have none, and it does nothing there.
type CreateExtension struct{ Name string }

func (o CreateExtension) mutate(s *State) error {
	if !slices.Contains(s.exts, o.Name) {
		s.exts = append(s.exts, o.Name)
	}
	return nil
}

func (o CreateExtension) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.CreateExtension(o.Name)), nil
}

func (o CreateExtension) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	return sqlSteps(d.DropExtension(o.Name)), nil
}

func (o CreateExtension) describe() string { return "create extension " + o.Name }

// RunSQL runs SQL of your own (statements ending with semicolons), on
// every dialect or on Dialect's alone. ReverseSQL undoes it: empty, the
// migration can't be unapplied; Noop, there is nothing to undo.
type RunSQL struct {
	SQL, ReverseSQL string
	Dialect         string
}

// Noop is a RunSQL's ReverseSQL that does nothing.
const Noop = "-- nothing to undo"

func (o RunSQL) mutate(*State) error { return nil }

func (o RunSQL) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	if o.Dialect != "" && o.Dialect != d.Name {
		return nil, nil
	}
	return []Step{{SQL: o.SQL}}, nil
}

func (o RunSQL) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	switch {
	case o.Dialect != "" && o.Dialect != d.Name:
		return nil, nil
	case o.ReverseSQL == "":
		return nil, errors.New("irreversible: it has no ReverseSQL")
	}
	return []Step{{SQL: o.ReverseSQL}}, nil
}

func (o RunSQL) describe() string { return "run SQL" }

// RunGo runs Go code, a data migration, in the migration's transaction:
// tx runs SQL in it, and the ORM's queries on ctx do too. Backward undoes
// it; nil, the migration can't be unapplied. It sees the database as the
// operations before it leave it, so query it in SQL, or with models that
// match that schema.
type RunGo struct {
	Forward, Backward func(ctx context.Context, tx Tx) error
}

func (o RunGo) mutate(*State) error { return nil }

func (o RunGo) forwards(schema.Dialect, *State, *State) ([]Step, error) {
	if o.Forward == nil {
		return nil, nil
	}
	return []Step{{Go: o.Forward}}, nil
}

func (o RunGo) backwards(schema.Dialect, *State, *State) ([]Step, error) {
	if o.Backward == nil {
		return nil, errors.New("irreversible: it has no Backward")
	}
	return []Step{{Go: o.Backward}}, nil
}

func (o RunGo) describe() string { return "run Go" }

// CreateFunction creates a function written in SQL on Postgres and MySQL,
// for an orm.Func to call; unapplying it drops it. SQLite runs no
// functions written in SQL — register a Go one with orm/sqlite's Func —
// and a dialect whose Body is empty gets nothing. Params and Returns are
// spelt as each database spells them; Body is Postgres's function body in
// Language (sql when empty: a SELECT or an expression's RETURN) and
// MySQL's routine body (RETURN …, or BEGIN … END, run whole).
//
//	m.CreateFunction{
//		Name:    "initials",
//		Params:  m.Dialects{Postgres: "s text", MySQL: "s VARCHAR(255)"},
//		Returns: m.Dialects{Postgres: "text", MySQL: "VARCHAR(255)"},
//		Body:    m.Dialects{Postgres: "SELECT upper(left(s, 1))", MySQL: "RETURN UPPER(LEFT(s, 1))"},
//	}
type CreateFunction struct {
	Name            string
	Params, Returns Dialects
	Body            Dialects
	Language        string
	Volatile        bool // the result may differ for the same arguments: not IMMUTABLE / DETERMINISTIC
}

var funcNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

func (o CreateFunction) mutate(*State) error {
	if !funcNameRE.MatchString(o.Name) {
		return fmt.Errorf("CreateFunction: %q is no function name", o.Name)
	}
	return nil
}

func (o CreateFunction) forwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	body := o.Body.For(d.Name)
	if body == "" || d.Name == "sqlite" {
		return nil, nil
	}
	params, returns := o.Params.For(d.Name), o.Returns.For(d.Name)
	if returns == "" {
		return nil, fmt.Errorf("CreateFunction %s: no Returns on %s", o.Name, d.Name)
	}
	if d.Name == "postgres" {
		if strings.Contains(body, "$orm$") {
			return nil, fmt.Errorf("CreateFunction %s: its body holds $orm$", o.Name)
		}
		vol := " IMMUTABLE"
		if o.Volatile {
			vol = " VOLATILE"
		}
		return []Step{{SQL: "CREATE OR REPLACE FUNCTION " + o.Name + "(" + params + ") RETURNS " + returns +
			" LANGUAGE " + cmp.Or(o.Language, "sql") + vol + " AS $orm$ " + body + " $orm$;"}}, nil
	}
	det := " DETERMINISTIC"
	if o.Volatile {
		det = " NOT DETERMINISTIC"
	}
	stmt := "CREATE FUNCTION " + o.Name + "(" + params + ") RETURNS " + returns + det + " " + body
	if !strings.Contains(body, ";") {
		return []Step{{SQL: stmt + ";"}}, nil
	}
	// A compound body's semicolons are its own: sent as one statement,
	// never split.
	return []Step{{SQL: stmt, Go: func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}}}, nil
}

func (o CreateFunction) backwards(d schema.Dialect, _, _ *State) ([]Step, error) {
	if o.Body.For(d.Name) == "" || d.Name == "sqlite" {
		return nil, nil
	}
	if d.Name == "postgres" {
		return []Step{{SQL: "DROP FUNCTION IF EXISTS " + o.Name + "(" + o.Params.For(d.Name) + ");"}}, nil
	}
	return []Step{{SQL: "DROP FUNCTION IF EXISTS " + o.Name + ";"}}, nil
}

func (o CreateFunction) describe() string { return "create function " + o.Name }
