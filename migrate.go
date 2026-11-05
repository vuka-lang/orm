package orm

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/orm/v2/internal/progress"
	"github.com/vuka-lang/orm/v2/internal/schema"
	"github.com/vuka-lang/orm/v2/migration"
)

// declared is every extension CreateExtension declared, for the
// migration autodetector.
var declared struct {
	mu   sync.Mutex
	exts []extension
}

type declaredModel struct {
	model     bool // T embeds orm.Model
	db        string
	unmanaged bool
	tables    func() ([]schema.Table, error)
	madeOn    func(Dialect) error
	info      func() ModelInfo
}

// migratesOn is whether the model's table is migrated on one of dbs
// (names, "" the default): the database its writes go to, as Default
// routes them, or its mirror, where every router allows it.
func (m declaredModel) migratesOn(dbs []string) bool {
	if m.info == nil {
		return slices.Contains(dbs, m.db)
	}
	mi := m.info()
	cands := []string{Default.dbFor(context.Background(), mi, true)}
	if mr, _, _ := Default.mirrorFor(mi); mr != "" {
		cands = append(cands, mr)
	}
	for _, db := range cands {
		if slices.Contains(dbs, db) && !slices.ContainsFunc(Default.routerList(), func(r Router) bool { return !r.AllowMigrate(db, mi) }) {
			return true
		}
	}
	return false
}

type extension struct{ db, name string }

// CreateExtension declares a Postgres extension the database needs —
// "vector" for Vector columns and vector indexes, "pg_trgm" for trigram
// indexes — Django's CreateExtension: orm makemigrations writes it into
// the next migration (CreateTables makes it too, with the database's
// models). Declare it package-level, beside the models, where the
// autodetector finds it; its Verify fails while the database lacks it.
// orm.On names the database.
//
//	var Vectors = orm.CreateExtension("vector")
func CreateExtension(name string, opts ...ForOption) Extension {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	declared.mu.Lock()
	declared.exts = append(declared.exts, extension{c.db, name})
	declared.mu.Unlock()
	return Extension{Name: name, DB: c.db}
}

// Extension is a Postgres extension CreateExtension declared.
type Extension struct {
	Name string
	DB   string // the database's name; "" the default
}

// Verify fails while ctx's database of the extension lacks it, waiting
// for the database to answer as Migrate does. Other databases than
// Postgres have no extensions: it passes.
func (e Extension) Verify(ctx context.Context) error {
	d, err := waitConnected(ctx, databasesOf(ctx), e.DB, MigrateWait)
	if err != nil {
		return err
	}
	return missingExtensions(ctx, d, []string{e.Name})
}

func declaredExtensions() []extension {
	declared.mu.Lock()
	defer declared.mu.Unlock()
	return slices.Clone(declared.exts)
}

// missingExtensions fails, naming how to install them, when a Postgres
// database lacks some of the extensions names.
func missingExtensions(ctx context.Context, d *DB, names []string) error {
	if d.dialect.Name() != "postgres" || len(names) == 0 {
		return nil
	}
	rows, err := d.sql.QueryContext(ctx, "SELECT extname FROM pg_extension")
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		have[n] = true
	}
	var errs []error
	for _, n := range names {
		if !have[n] {
			errs = append(errs, fmt.Errorf("orm: the database lacks the extension %q: declare orm.CreateExtension(%q), then orm makemigrations and orm migrate", n, n))
		}
	}
	return errors.Join(append(errs, rows.Err())...)
}

// plannedModels is the models the autodetector reads: the types embedding
// orm.Model the program registers, each by its own manager. orm.For and
// orm.Of managers of other types are queries over tables, not models.
func plannedModels() []declaredModel {
	var out []declaredModel
	for _, m := range registered() {
		if d := m.declaration(); d.model {
			out = append(out, d)
		}
	}
	return out
}

// declaredState is the schema the program's models of the databases dbs
// (orm.On names, "" the default) declare, and their extensions: managed
// models' tables, unmanaged models' declared indexes alone. A model
// dialect can't make fails it.
func declaredState(dbs []string, dialect string) (*migration.State, error) {
	var tables []schema.Table
	at := map[string]int{}
	for _, m := range plannedModels() {
		if !m.migratesOn(dbs) {
			continue
		}
		ts, err := m.declaredTables(DialectFor(dialect))
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			i, ok := at[t.Name]
			if !ok {
				at[t.Name] = len(tables)
				tables = append(tables, t)
				continue
			}
			if tables[i], err = mergeTable(tables[i], t); err != nil {
				return nil, err
			}
		}
	}
	var exts []string
	for _, e := range declaredExtensions() {
		if slices.Contains(dbs, e.db) && !slices.Contains(exts, e.name) {
			exts = append(exts, e.name)
		}
	}
	return migration.NewState(schema.Sort(tables), exts), nil
}

// declaredTables is what the model declares to be migrated on d: its
// tables when it is managed; when not, what is declared on its table
// (indexes, checks), which is the database's own (and so never dropped,
// even with nothing declared on it).
func (m declaredModel) declaredTables(d Dialect) ([]schema.Table, error) {
	if !m.unmanaged {
		if err := m.madeOn(d); err != nil {
			return nil, err
		}
		return m.tables()
	}
	ts, err := m.tables()
	if err != nil {
		return nil, err
	}
	checks := slices.DeleteFunc(slices.Clone(ts[0].Constraints), func(k schema.Constraint) bool { return k.Declared.Zero() })
	return []schema.Table{{Name: ts[0].Name, Constraints: checks, Indexes: ts[0].Indexes, Unmanaged: true}}, nil
}

// mergeTable is one table two models map: the columns, constraints and
// indexes of both, which must agree where they overlap. It is managed
// when one of them is.
func mergeTable(a, b schema.Table) (schema.Table, error) {
	a.Unmanaged = a.Unmanaged && b.Unmanaged
	a.Columns = slices.Clone(a.Columns)
	for _, c := range b.Columns {
		have, ok := a.Column(c.Name)
		if !ok {
			a.Columns = append(a.Columns, c)
			continue
		}
		if !have.Same(c) {
			return a, fmt.Errorf("orm: two models map table %s and disagree on its column %s: make one orm.Unmanaged()", a.Name, c.Name)
		}
	}
	for _, k := range b.Constraints {
		i := slices.IndexFunc(a.Constraints, func(o schema.Constraint) bool { return o.Name == k.Name })
		switch {
		case i < 0:
			a.Constraints = append(a.Constraints, k)
		case !k.Declared.Zero() && !a.Constraints[i].Same(k):
			return a, fmt.Errorf("orm: two models map table %s and disagree on its check %s: declare it on one of them", a.Name, k.Name)
		}
	}
	for _, ix := range b.Indexes {
		i := slices.IndexFunc(a.Indexes, func(have schema.Index) bool { return have.Name == ix.Name })
		switch {
		case i < 0:
			a.Indexes = append(a.Indexes, ix)
		case a.Indexes[i] != ix:
			return a, fmt.Errorf("orm: two models map table %s and disagree on its index %s: declare it on one of them", a.Name, ix.Name)
		}
	}
	return a, nil
}

// MigrateOption tunes Migrate.
type MigrateOption func(*migrateConfig)

type migrateConfig struct {
	dbs []string
	log *slog.Logger
}

// MigrateOn applies the migrations of the database added as name ("" the
// default) only; repeat it for several. Every database with migrations
// otherwise.
func MigrateOn(name string) MigrateOption {
	return func(c *migrateConfig) { c.dbs = append(c.dbs, name) }
}

// MigrateLogger has Migrate say what it applies on l; slog.Default()
// otherwise.
func MigrateLogger(l *slog.Logger) MigrateOption {
	return func(c *migrateConfig) { c.log = l }
}

// Migrate applies the registered migrations to ctx's databases (Default,
// or WithDatabases's), at start up before the program serves: those not
// yet recorded in each database's MigrationsTable, each after its
// dependencies. The program's migrations package registers them: import
// it.
//
//	import _ "example.com/shop/migrations"
//
//	if err := orm.Migrate(ctx); err != nil {
//		log.Fatal(err)
//	}
//
// It waits up to MigrateWait for each database to answer. A migration runs
// in a transaction where the database can roll DDL back (Postgres, SQLite;
// MySQL commits each statement). Replicas starting together take turns
// under a lock (Postgres advisory, MySQL GET_LOCK). orm migrate applies
// them (or unapplies them, back to a migration) on demand.
func Migrate(ctx context.Context, opts ...MigrateOption) error {
	c := migrateConfig{log: slog.Default()}
	for _, o := range opts {
		o(&c)
	}
	ordered, err := migration.Ordered()
	if err != nil {
		return err
	}
	dbs := databasesOf(ctx)
	r := &migrator{
		ordered: ordered,
		open: func(ctx context.Context, name string) (*DB, error) {
			return waitConnected(ctx, dbs, name, MigrateWait)
		},
		log: func(format string, args ...any) {
			c.log.InfoContext(ctx, "orm: "+fmt.Sprintf(format, args...))
		},
	}
	defer r.close()
	return r.forwardAll(ctx, c.dbs)
}

// Check is what keeps the ORM from mapping the program's models — those
// Register added, and models — joined: run it at start up, so a model it
// can't map fails there rather than on its first query.
func Check(models ...AnyManager) error {
	var errs []error
	for _, m := range append(registered(), models...) {
		errs = append(errs, m.Err())
	}
	return errors.Join(errs...)
}

// ApplyMigrations takes d through migrations (one database's) as orm
// migrate does: to target (a name, or its leading number), applying those
// up to it and unapplying those after; "zero" unapplies them all, ""
// applies them all. For tools and tests.
func ApplyMigrations(ctx context.Context, d *DB, target string, migrations ...migration.Migration) error {
	ordered, err := migration.Order(migrations)
	if err != nil || len(ordered) == 0 {
		return err
	}
	r := &migrator{
		ordered: ordered,
		open:    func(context.Context, string) (*DB, error) { return d, nil },
		log:     func(string, ...any) {},
	}
	defer r.close()
	if target == "" {
		return r.forwardAll(ctx, nil)
	}
	return r.to(ctx, ordered[0].DB, target)
}

// MigrateWait is how long Migrate (and Verify) wait for a database to
// answer.
var MigrateWait = 30 * time.Second

// MigrationsTable is the table recording each database's applied
// migrations. Set it before migrating to keep a history recorded under
// another name.
var MigrationsTable = "orm_migrations"

// migrator applies migrations (ordered, every database's), a session per
// database it touches.
type migrator struct {
	ordered  []migration.Migration
	open     func(ctx context.Context, name string) (*DB, error)
	log      func(format string, args ...any)
	sessions map[string]*session
	// confirm, when set, is asked before migrations are unapplied (their
	// backwards operations may drop tables and data); false cancels.
	confirm func(db string, unapply []migration.Migration) bool
	// watch, when set, is told each migration and statement as it runs.
	watch migrationWatch
	// noPreflight skips the preflights checking the rows before a
	// migration adds a unique index or a check (see migration.Preflights).
	noPreflight bool
}

// migrationWatch follows a migration running: begin before its first
// statement, checking before each preflight, statement before each
// statement (sql "" for Go code), end after.
type migrationWatch interface {
	begin(mg migration.Migration, forward bool, statements int)
	checking(p migration.Preflight)
	statement(n int, sql string)
	end(err error)
}

// session is a database being migrated: a connection holding the lock,
// and the migrations it has applied.
type session struct {
	name    string
	db      *DB
	c       *sql.Conn
	unlock  func()
	applied map[string]bool
	dialect string // the migrations' SQL is written in (DB.schemaDialect)
	watch   migrationWatch
}

func (r *migrator) session(ctx context.Context, name string) (*session, error) {
	if s, ok := r.sessions[name]; ok {
		return s, nil
	}
	d, err := r.open(ctx, name)
	if err != nil {
		return nil, err
	}
	c, err := d.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	unlock, err := lockMigrations(ctx, c, d.dialect)
	if err != nil {
		c.Close()
		return nil, err
	}
	dialect, err := d.schemaDialect(ctx, c)
	if err != nil {
		unlock()
		c.Close()
		return nil, err
	}
	s := &session{name: name, db: d, c: c, unlock: unlock, applied: map[string]bool{}, dialect: dialect, watch: r.watch}
	if r.sessions == nil {
		r.sessions = map[string]*session{}
	}
	r.sessions[name] = s
	sd := schema.For(d.dialect.Name())
	record := schema.Table{Name: MigrationsTable, Columns: []schema.Column{
		{Name: "name", Kind: schema.String, Size: 255, PK: true},
		{Name: "applied_at", Kind: schema.Time},
	}}
	if _, err := c.ExecContext(ctx, sd.Create(record, true)[0]); err != nil {
		return nil, fmt.Errorf("orm: creating %s: %w", MigrationsTable, err)
	}
	rows, err := c.QueryContext(ctx, "SELECT "+sd.Quote("name")+" FROM "+sd.Quote(MigrationsTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		s.applied[n] = true
	}
	return s, rows.Err()
}

func (r *migrator) close() {
	for _, s := range r.sessions {
		s.unlock()
		s.c.Close()
	}
}

// forwardAll applies the pending migrations of the databases dbs (every
// one with migrations for none), each after its dependencies, across
// databases.
func (r *migrator) forwardAll(ctx context.Context, dbs []string) error {
	states := map[string]*migration.State{}
	for _, mg := range r.ordered {
		if len(dbs) > 0 && !slices.Contains(dbs, mg.DB) {
			continue
		}
		s, err := r.session(ctx, mg.DB)
		if err != nil {
			return err
		}
		st := states[mg.DB]
		if st == nil {
			st = migration.NewState(nil, nil)
			states[mg.DB] = st
		}
		if s.applied[mg.Name] {
			if err := mg.Apply(st); err != nil {
				return err
			}
			continue
		}
		checks := r.preflights(mg, s.dialect)
		steps, err := mg.Forwards(s.dialect, st)
		if err != nil {
			return err
		}
		if err := s.run(ctx, mg, steps, true, checks...); err != nil {
			return err
		}
		r.log("migration applied: %s", label(mg))
	}
	return nil
}

// to takes database name to target: its migrations up to target applied,
// those after it unapplied (last first); "zero" unapplies them all.
func (r *migrator) to(ctx context.Context, name, target string) error {
	mine := migration.Of(r.ordered, name)
	last := -1
	if target != "zero" {
		mg, err := migration.Find(r.ordered, name, target)
		if err != nil {
			return err
		}
		last = slices.IndexFunc(mine, func(o migration.Migration) bool { return o.Name == mg.Name })
	}
	s, err := r.session(ctx, name)
	if err != nil {
		return err
	}
	before := make([]*migration.State, len(mine))
	st := migration.NewState(nil, nil)
	for i, mg := range mine {
		before[i] = st.Clone()
		if err := mg.Apply(st); err != nil {
			return err
		}
	}
	if r.confirm != nil {
		var unapply []migration.Migration
		for i := len(mine) - 1; i > last; i-- {
			if s.applied[mine[i].Name] {
				unapply = append(unapply, mine[i])
			}
		}
		if len(unapply) > 0 && !r.confirm(name, unapply) {
			return fmt.Errorf("orm: migrate cancelled: nothing unapplied on %s", dbLabel(name))
		}
	}
	for i := len(mine) - 1; i > last; i-- {
		if !s.applied[mine[i].Name] {
			continue
		}
		steps, err := mine[i].Backwards(s.dialect, before[i])
		if err != nil {
			return err
		}
		if err := s.run(ctx, mine[i], steps, false); err != nil {
			return err
		}
		r.log("migration unapplied: %s", label(mine[i]))
	}
	for i := 0; i <= last; i++ {
		if s.applied[mine[i].Name] {
			continue
		}
		checks := r.preflights(mine[i], s.dialect)
		steps, err := mine[i].Forwards(s.dialect, before[i])
		if err != nil {
			return err
		}
		if err := s.run(ctx, mine[i], steps, true, checks...); err != nil {
			return err
		}
		r.log("migration applied: %s", label(mine[i]))
	}
	return nil
}

// preflights is the queries checking the rows before mg runs on
// dialect: none when they are skipped.
func (r *migrator) preflights(mg migration.Migration, dialect string) []migration.Preflight {
	if r.noPreflight {
		return nil
	}
	return mg.Preflights(dialect)
}

func label(mg migration.Migration) string {
	if mg.DB == "" {
		return mg.Name
	}
	return mg.DB + ":" + mg.Name
}

// run runs a migration's steps and records it applied (or, back, not):
// in one transaction where the database rolls DDL back, Go steps and the
// ORM's queries on their ctx included, an Outside step between two (see
// batches). SQLite's foreign keys are off while it rebuilds tables. The
// checks run first: one failing fails the migration before its first
// statement.
func (s *session) run(ctx context.Context, mg migration.Migration, steps []migration.Step, forward bool, checks ...migration.Preflight) (err error) {
	dl := s.db.dialect
	sd := schema.For(dl.Name())
	// One step per statement, so each is followed as it runs.
	var stmts []migration.Step
	for _, st := range steps {
		if st.Go != nil {
			stmts = append(stmts, st)
			continue
		}
		for _, q := range splitSQL(st.SQL, dl.Name()) {
			one := st
			one.SQL = q
			stmts = append(stmts, one)
		}
	}
	total := len(stmts)
	if s.watch != nil {
		s.watch.begin(mg, forward, total)
		defer func() { s.watch.end(err) }()
	}
	for _, p := range checks {
		if s.watch != nil {
			s.watch.checking(p)
		}
		if err := s.preflight(ctx, mg, p); err != nil {
			return err
		}
	}
	if dl.Name() == "sqlite" {
		var on bool
		if err := s.c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
			return err
		}
		if on {
			if _, err := s.c.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
				return err
			}
			defer s.c.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
		}
	}
	record := "DELETE FROM " + sd.Quote(MigrationsTable) + " WHERE " + sd.Quote("name") + " = " + dl.Placeholder(1)
	args := []any{mg.Name}
	if forward {
		record = "INSERT INTO " + sd.Quote(MigrationsTable) + " (" + sd.Quote("name") + ", " + sd.Quote("applied_at") + ") VALUES (" + dl.Placeholder(1) + ", " + dl.Placeholder(2) + ")"
		args = append(args, time.Now().UTC())
	}
	stmts = append(stmts, migration.Step{Go: func(ctx context.Context, tx migration.Tx) error {
		if _, err := tx.ExecContext(ctx, record, args...); err != nil {
			return fmt.Errorf("orm: recording migration %s: %w", label(mg), err)
		}
		return nil
	}})
	done := 0
	do := func(ctx context.Context, tx migration.Tx, steps []migration.Step) error {
		for _, st := range steps {
			if done++; s.watch != nil && done <= total {
				s.watch.statement(done, st.SQL)
			}
			if st.Go != nil {
				if err := st.Go(ctx, tx); err != nil {
					return fmt.Errorf("orm: migration %s: %w", label(mg), err)
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, st.SQL); err != nil {
				return fmt.Errorf("orm: migration %s: %w\n%s", label(mg), err, st.SQL)
			}
		}
		return nil
	}
	inner := WithDB(ctx, s.db)
	for _, batch := range batches(stmts, dl.Name() != "mysql") {
		if !batch.inTx {
			if err := do(inner, s.c, batch.steps); err != nil {
				return err
			}
			continue
		}
		tx, err := s.c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := inTx(inner, s.db.sql, tx, func(ctx context.Context) error { return do(ctx, tx, batch.steps) }); err != nil {
			return err
		}
	}
	s.applied[mg.Name] = forward
	return nil
}

// batch is steps run together: in a transaction, or on the connection.
type batch struct {
	steps []migration.Step
	inTx  bool
}

// batches is a migration's steps as they run: where the database rolls
// DDL back (transactional), those between Outside steps in a transaction
// each and every Outside step on its own; else all of them on the
// connection.
func batches(steps []migration.Step, transactional bool) []batch {
	if !transactional {
		return []batch{{steps: steps}}
	}
	var out []batch
	for _, st := range steps {
		inTx := !st.Outside
		if n := len(out); n > 0 && inTx && out[n-1].inTx {
			out[n-1].steps = append(out[n-1].steps, st)
			continue
		}
		out = append(out, batch{steps: []migration.Step{st}, inTx: inTx})
	}
	return out
}

// migrationLock is the Postgres advisory lock key of the migrations:
// "NXMIGRAT" as a big-endian int64.
const migrationLock = 0x4e58_4d49_4752_4154

func lockMigrations(ctx context.Context, c *sql.Conn, d Dialect) (func(), error) {
	switch d.Name() {
	case "postgres":
		if _, err := c.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(migrationLock)); err != nil {
			return nil, fmt.Errorf("orm: locking migrations: %w", err)
		}
		return func() {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", int64(migrationLock))
		}, nil
	case "mysql":
		var got sql.NullInt64
		if err := c.QueryRowContext(ctx, "SELECT GET_LOCK(?, 600)", MigrationsTable).Scan(&got); err != nil {
			return nil, fmt.Errorf("orm: locking migrations: %w", err)
		}
		if got.Int64 != 1 {
			return nil, fmt.Errorf("orm: locking migrations: another process held the lock for 10 minutes")
		}
		return func() { _, _ = c.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", MigrationsTable) }, nil
	}
	return func() {}, nil
}

// splitSQL is a migration's statements on a dialect: split at semicolons
// outside quotes, comments, Postgres dollar quotes and the BEGIN … END
// body of a CREATE TRIGGER (whose last statement's own END — a CASE's —
// would end it early); comments and blank statements dropped.
func splitSQL(src, dialect string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	lexSQL(src, dialect, &b, func(i int) int {
		if src[i] == ';' && !openTrigger(b.String()) {
			flush()
		} else {
			b.WriteByte(src[i])
		}
		return i + 1
	})
	flush()
	return out
}

// openTrigger is whether stmt is a CREATE TRIGGER inside its BEGIN … END
// body, where a semicolon ends one of its statements, not it.
func openTrigger(stmt string) bool {
	f := strings.Fields(strings.ToUpper(stmt))
	if len(f) < 3 || f[0] != "CREATE" {
		return false
	}
	i := 1
	if f[i] == "TEMP" || f[i] == "TEMPORARY" {
		i++
	}
	return f[i] == "TRIGGER" && slices.Contains(f, "BEGIN") && f[len(f)-1] != "END"
}

// lexSQL writes src to b: quoted strings and identifiers and Postgres
// dollar-quoted bodies whole, comments dropped (a block comment as a
// space), and each other byte through code, which writes it or what it
// stands for and returns where to go on. It reads them as dialect's
// database does, so a ? is a mark exactly where the database sees code:
// a backslash escapes a quote in MySQL's strings and Postgres's E'…',
// and is an ordinary character elsewhere; MySQL's comments are # and --
// before a space (5--1 is arithmetic); Postgres's block comments nest.
func lexSQL(src, dialect string, b *strings.Builder, code func(i int) int) {
	mysql, postgres := dialect == "mysql", dialect == "postgres"
	toEOL := func(i int) int {
		if end := strings.IndexByte(src[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(src)
	}
	for i := 0; i < len(src); {
		ch := src[i]
		switch {
		case ch == '-' && strings.HasPrefix(src[i:], "--") && (!mysql || i+2 == len(src) || src[i+2] <= ' '),
			ch == '#' && mysql:
			i = toEOL(i)
			continue
		case ch == '/' && strings.HasPrefix(src[i:], "/*"):
			depth, j := 1, i+2
			for j < len(src) && depth > 0 {
				switch {
				case strings.HasPrefix(src[j:], "*/"):
					depth, j = depth-1, j+2
				case postgres && strings.HasPrefix(src[j:], "/*"):
					depth, j = depth+1, j+2
				default:
					j++
				}
			}
			i = j
			b.WriteByte(' ')
			continue
		case ch == '\'' || ch == '"' || ch == '`':
			backslash := mysql && ch != '`' ||
				postgres && ch == '\'' && i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !identByte(src[i-2]))
			j := i + 1
			for j < len(src) {
				if src[j] == ch {
					if j+1 < len(src) && src[j+1] == ch {
						j += 2
						continue
					}
					break
				}
				if backslash && src[j] == '\\' && j+1 < len(src) {
					j++
				}
				j++
			}
			end := min(j+1, len(src))
			b.WriteString(src[i:end])
			i = end
			continue
		case ch == '$' && postgres && !inIdent(src, i):
			if tag := dollarTag(src[i:]); tag != "" {
				end := strings.Index(src[i+len(tag):], tag)
				stop := len(src)
				if end >= 0 {
					stop = i + len(tag) + end + len(tag)
				}
				b.WriteString(src[i:stop])
				i = stop
				continue
			}
		}
		i = code(i)
	}
}

// dollarTag is the dollar quote opening s: an identifier without a $
// between two, or none: $$, $body$, $fn_1$; "" when s opens none.
func dollarTag(s string) string {
	j := 1
	if j < len(s) && identByte(s[j]) && s[j] != '$' && !('0' <= s[j] && s[j] <= '9') {
		for j < len(s) && identByte(s[j]) && s[j] != '$' {
			j++
		}
	}
	if j < len(s) && s[j] == '$' {
		return s[:j+1]
	}
	return ""
}

// inIdent is whether src[i] continues an identifier: the bytes before it
// up to a space or symbol start with a letter (a$b$ is one name), not a
// digit or a $ (after 1 or $1, $$ opens a dollar quote).
func inIdent(src string, i int) bool {
	j := i
	for j > 0 && identByte(src[j-1]) {
		j--
	}
	return j < i && src[j] != '$' && !('0' <= src[j] && src[j] <= '9')
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// CLIEnv is the variable the orm command sets to run a program as its
// migration tool (makemigrations, migrate, showmigrations, sqlmigrate),
// holding a JSON CLIRequest.
const CLIEnv = "ORM_CLI_REQUEST"

// CLIRequest is what the orm command asks of a program run as its
// migration tool.
type CLIRequest struct {
	Command string `json:"command"` // plan, migrate, show, sql
	// DB is the migrations' database ("" the default).
	DB string `json:"db"`
	// Models is plan's: the databases (orm.On names) the models of DB's
	// migrations are on: "" and the default's name for the default.
	Models  []string `json:"models,omitempty"`
	Dialect string   `json:"dialect,omitempty"` // plan, sql
	// Number, Name, Package and Empty name and shape plan's migration;
	// Interactive asks about renames on stdin, and migrate's confirmation
	// before it unapplies migrations.
	Number      int    `json:"number,omitempty"`
	Name        string `json:"name,omitempty"`
	Package     string `json:"package,omitempty"`
	Empty       bool   `json:"empty,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	// Update is plan's: the changes folded into DB's latest migration,
	// whose file is in Dir, rather than a new one (makemigrations
	// --update).
	Update bool   `json:"update,omitempty"`
	Dir    string `json:"dir,omitempty"`
	// Target is migrate's: the migration to take DB to ("zero" for
	// none); every database's pending ones when empty. sql's: the
	// migration to print.
	Target    string `json:"target,omitempty"`
	Backwards bool   `json:"backwards,omitempty"` // sql
	// NoPreflight is migrate's: no checking the rows before a unique
	// index or a check is added.
	NoPreflight bool `json:"noPreflight,omitempty"`
	// Conns is the URL of each migrations database ("" the default's),
	// for migrate and show to connect (see ConnectURL).
	Conns map[string]string `json:"conns,omitempty"`
	Out   string            `json:"out,omitempty"` // plan: the file its PlanResult goes to
	// Quiet shows no progress, only errors; Verbose also each statement
	// migrate runs.
	Quiet   bool `json:"quiet,omitempty"`
	Verbose bool `json:"verbose,omitempty"`
}

// PlanResult is the migration plan wrote: its file's name and source,
// what each operation does, and the notes beside them. No operations is
// no changes. Updates is the migration an update rewrites (its file
// <Updates>.go, replaced by File); Remove says the update leaves it
// nothing to do, so its file goes.
type PlanResult struct {
	File    string   `json:"file,omitempty"`
	Source  []byte   `json:"source,omitempty"`
	Ops     []string `json:"ops"`
	Notes   []string `json:"notes,omitempty"`
	Updates string   `json:"updates,omitempty"`
	Remove  bool     `json:"remove,omitempty"`
}

// ServeCLI makes the program the orm command's migration tool when CLIEnv
// is set: it does what the request asks and exits, before main runs. The
// command calls it from an init it overlays into the program's main
// package; programs don't.
func ServeCLI() {
	raw := os.Getenv(CLIEnv)
	if raw == "" {
		return
	}
	// Ctrl-C cancels the statement running, whose transaction rolls back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := serveCLI(ctx, raw, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func serveCLI(ctx context.Context, raw string, stdin io.Reader, stdout, stderr io.Writer) error {
	var req CLIRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return err
	}
	switch req.Command {
	case "plan":
		res, err := plan(req, bufio.NewReader(stdin), stderr)
		if err != nil {
			return err
		}
		out, err := json.Marshal(res)
		if err != nil {
			return err
		}
		return os.WriteFile(req.Out, out, 0o600)
	case "sql":
		return printSQL(req, stdout)
	}
	ordered, err := migration.Ordered()
	if err != nil {
		return err
	}
	w := &cliWatch{p: progress.New(stderr, isTerminal(stderr), req.Quiet), verbose: req.Verbose}
	r := &migrator{
		ordered:     ordered,
		open:        func(ctx context.Context, name string) (*DB, error) { return openConfigured(req.Conns, name) },
		log:         func(string, ...any) {},
		watch:       w,
		noPreflight: req.NoPreflight,
	}
	defer r.close()
	if req.Interactive {
		in := bufio.NewReader(stdin)
		r.confirm = func(db string, unapply []migration.Migration) bool {
			defer w.p.Hold()()
			fmt.Fprintf(stderr, "Unapplying on %s, which may drop tables and their data:\n", dbLabel(db))
			for _, mg := range unapply {
				fmt.Fprintf(stderr, "  %s\n", label(mg))
			}
			fmt.Fprint(stderr, "Continue? [y/N] ")
			line, _ := in.ReadString('\n')
			return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
		}
	}
	switch req.Command {
	case "migrate":
		if req.Target == "" {
			err = r.forwardAll(ctx, nil)
		} else {
			err = r.to(ctx, req.DB, req.Target)
		}
		if err == nil {
			w.summary()
		}
		return err
	case "show":
		return show(ctx, r, stdout)
	}
	return fmt.Errorf("orm: no command %q", req.Command)
}

// cliWatch shows orm migrate's migrations as they run, and the totals of
// each database once they have.
type cliWatch struct {
	p       *progress.Printer
	verbose bool
	task    *progress.Task
	mg      migration.Migration
	forward bool
	n       int
	totals  []migrateTotal
}

// migrateTotal is the migrations applied (or unapplied) on a database.
type migrateTotal struct {
	db      string
	forward bool
	count   int
	took    time.Duration
	start   time.Time
}

func (w *cliWatch) begin(mg migration.Migration, forward bool, statements int) {
	w.mg, w.forward, w.n = mg, forward, statements
	verb := "Applying"
	if !forward {
		verb = "Unapplying"
	}
	w.task = w.p.Start(fmt.Sprintf("%s %s on %s", verb, mg.Name, dbLabel(mg.DB)))
	i := slices.IndexFunc(w.totals, func(t migrateTotal) bool { return t.db == mg.DB && t.forward == forward })
	if i < 0 {
		w.totals = append(w.totals, migrateTotal{db: mg.DB, forward: forward})
		i = len(w.totals) - 1
	}
	w.totals[i].start = time.Now()
}

func (w *cliWatch) checking(p migration.Preflight) {
	w.task.Detail("checking the rows for " + p.Name)
	if w.verbose {
		w.p.Println("    -- preflight: " + p.SQL)
	}
}

func (w *cliWatch) statement(n int, sql string) {
	if w.n > 1 {
		w.task.Detail(fmt.Sprintf("statement %d/%d", n, w.n))
	}
	if w.verbose {
		if sql == "" {
			sql = "-- Go code (RunGo)"
		}
		w.p.Println("    " + strings.ReplaceAll(sql, "\n", "\n    "))
	}
}

func (w *cliWatch) end(err error) {
	if err != nil {
		w.task.Fail()
		return
	}
	verb := "Applied"
	if !w.forward {
		verb = "Unapplied"
	}
	w.task.Done(fmt.Sprintf("%s %s on %s", verb, w.mg.Name, dbLabel(w.mg.DB)))
	i := slices.IndexFunc(w.totals, func(t migrateTotal) bool { return t.db == w.mg.DB && t.forward == w.forward })
	w.totals[i].count++
	w.totals[i].took += time.Since(w.totals[i].start)
}

// summary prints each database's totals, or that there was nothing to do.
func (w *cliWatch) summary() {
	if len(w.totals) == 0 {
		w.p.Println("No migrations to apply.")
	}
	for _, t := range w.totals {
		verb, noun := "Applied", "migrations"
		if !t.forward {
			verb = "Unapplied"
		}
		if t.count == 1 {
			noun = "migration"
		}
		w.p.Println(fmt.Sprintf("✓ %s %d %s on %s in %s", verb, t.count, noun, dbLabel(t.db), progress.Duration(t.took)))
	}
}

// isTerminal is whether w is a terminal, where progress redraws its line.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// openConfigured opens migrations database name by its URL.
func openConfigured(conns map[string]string, name string) (*DB, error) {
	url, ok := conns[name]
	if !ok {
		return nil, fmt.Errorf("orm: no URL for %s: pass --database or set DATABASE_URL", dbLabel(name))
	}
	return ConnectURL(url)
}

func dbLabel(name string) string {
	if name == "" {
		return "the default database"
	}
	return "database " + name
}

// plan is the next migration of req.DB: the operations from its
// migrations' state to its models', written as a Go file.
func plan(req CLIRequest, stdin *bufio.Reader, stderr io.Writer) (PlanResult, error) {
	ordered, err := migration.Ordered()
	if err != nil {
		return PlanResult{}, err
	}
	from, err := migration.Replay(ordered, req.DB)
	if err != nil {
		return PlanResult{}, err
	}
	to, err := declaredState(req.Models, req.Dialect)
	if err != nil {
		return PlanResult{}, err
	}
	ask := func(q string) bool {
		if !req.Interactive {
			return false
		}
		fmt.Fprintf(stderr, "%s [y/N] ", q)
		line, _ := stdin.ReadString('\n')
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
	}
	// An update diffs again from before the latest migration, asking
	// then; this diff only says whether there are changes.
	first := ask
	if req.Update {
		first = func(string) bool { return false }
	}
	ops, notes := migration.Diff(from, to, first)
	if req.Empty {
		ops, notes = []migration.Operation{migration.RunGo{}}, nil
	}
	res := PlanResult{Notes: notes}
	for _, op := range ops {
		res.Ops = append(res.Ops, migration.Describe(op))
	}
	if len(ops) == 0 {
		return res, nil
	}
	if req.Update {
		return update(req, ordered, to, ask)
	}
	name := req.Name
	if name == "" {
		name = autoName(ops, len(migration.Of(ordered, req.DB)) == 0)
	}
	mg := migration.Migration{Name: fmt.Sprintf("%04d_%s", req.Number, name), DB: req.DB, Dependencies: migration.Leaves(ordered, req.DB), Operations: ops}
	if _, err := mg.Forwards(req.Dialect, from); err != nil {
		return res, err
	}
	res.File = mg.Name + ".go"
	res.Source, err = migration.Source(req.Package, mg, notes)
	return res, err
}

// update is req.DB's latest migration rewritten to hold the models'
// changes too, Django's makemigrations --update: the operations from the
// state before it to the models', so an operation and its undo cancel
// out and an index changed twice is added once. It keeps its number and
// dependencies, and its name unless that was made from its operations
// (then the new ones name it). It refuses a migration another depends
// on, and one the autodetector didn't write as it is (RunSQL, RunGo, an
// edit): those are a person's. None of it checks a database: a migration
// applied anywhere must not be updated.
func update(req CLIRequest, ordered []migration.Migration, to *migration.State, ask func(string) bool) (PlanResult, error) {
	mine := migration.Of(ordered, req.DB)
	if len(mine) == 0 {
		return PlanResult{}, fmt.Errorf("orm: --update: %s has no migration to update", dbLabel(req.DB))
	}
	latest := mine[len(mine)-1]
	if err := updatable(req, ordered, latest); err != nil {
		return PlanResult{}, err
	}
	base := slices.DeleteFunc(slices.Clone(ordered), func(mg migration.Migration) bool { return mg.DB == latest.DB && mg.Name == latest.Name })
	from, err := migration.Replay(base, req.DB)
	if err != nil {
		return PlanResult{}, err
	}
	ops, notes := migration.Diff(from, to, ask)
	res := PlanResult{Notes: notes, Updates: latest.Name, Ops: []string{}}
	for _, op := range ops {
		res.Ops = append(res.Ops, migration.Describe(op))
	}
	if len(ops) == 0 {
		res.Remove = true
		return res, nil
	}
	number, name, _ := strings.Cut(latest.Name, "_")
	first := len(migration.Of(base, req.DB)) == 0
	switch {
	case req.Name != "":
		name = req.Name
	case name == autoName(latest.Operations, first):
		name = autoName(ops, first)
	}
	mg := migration.Migration{Name: number + "_" + name, DB: req.DB, Dependencies: latest.Dependencies, Operations: ops}
	if _, err := mg.Forwards(req.Dialect, from); err != nil {
		return res, err
	}
	res.File = mg.Name + ".go"
	res.Source, err = migration.Source(req.Package, mg, notes)
	return res, err
}

// updatable is why --update can't rewrite latest, nil when it can: no
// migration depends on it, and its file is what makemigrations writes of
// its operations, every one of a kind the autodetector writes.
func updatable(req CLIRequest, ordered []migration.Migration, latest migration.Migration) error {
	refuse := func(why string) error {
		return fmt.Errorf("orm: --update can't rewrite %s: %s; run makemigrations without --update for a new migration", latest.Name, why)
	}
	for _, mg := range ordered {
		for _, d := range mg.Dependencies {
			db, name, cross := strings.Cut(d, ":")
			if !cross {
				db, name = mg.DB, d
			}
			if db == latest.DB && name == latest.Name {
				return refuse(label(mg) + " depends on it")
			}
		}
	}
	for _, op := range latest.Operations {
		if !migration.Autodetected(op) {
			return refuse("it holds " + migration.Describe(op) + ", written by hand")
		}
	}
	path := filepath.Join(req.Dir, latest.Name+".go")
	have, err := os.ReadFile(path)
	if err != nil {
		return refuse(fmt.Sprintf("its file: %v", err))
	}
	want, err := migration.Source(req.Package, latest, nil)
	if err != nil {
		return err
	}
	if withoutNotes(have) != withoutNotes(want) {
		return refuse(path + " was edited since makemigrations wrote it")
	}
	return nil
}

// withoutNotes is a migration's source without makemigrations' NOTE
// comments, which it writes from the models, not from the operations.
func withoutNotes(src []byte) string {
	var b strings.Builder
	for line := range strings.Lines(string(src)) {
		if !strings.HasPrefix(strings.TrimSpace(line), "// NOTE: ") {
			b.WriteString(line)
		}
	}
	return b.String()
}

var nameRE = regexp.MustCompile(`[^a-z0-9]+`)

// autoName names a migration after what it does: initial, its one
// operation (add_field_users_email), the one table it changes, else
// auto.
func autoName(ops []migration.Operation, first bool) string {
	switch {
	case first:
		return "initial"
	case len(ops) == 1:
		return strings.Trim(nameRE.ReplaceAllString(strings.ToLower(migration.Describe(ops[0])), "_"), "_")
	}
	return "auto"
}

// show prints each database's migrations, [X] applied and [ ] not.
func show(ctx context.Context, r *migrator, w io.Writer) error {
	var dbs []string
	for _, mg := range r.ordered {
		if !slices.Contains(dbs, mg.DB) {
			dbs = append(dbs, mg.DB)
		}
	}
	if len(dbs) == 0 {
		fmt.Fprintln(w, "No migrations: does the program import its migrations package?")
	}
	for _, name := range dbs {
		s, err := r.session(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, strings.ToUpper(dbLabel(name)[:1])+dbLabel(name)[1:])
		for _, mg := range migration.Of(r.ordered, name) {
			mark := " "
			if s.applied[mg.Name] {
				mark = "X"
			}
			fmt.Fprintf(w, " [%s] %s\n", mark, mg.Name)
		}
	}
	return nil
}

// printSQL prints the SQL a migration runs on req.Dialect, forwards or
// back.
func printSQL(req CLIRequest, w io.Writer) error {
	ordered, err := migration.Ordered()
	if err != nil {
		return err
	}
	mg, err := migration.Find(ordered, req.DB, req.Target)
	if err != nil {
		return err
	}
	st := migration.NewState(nil, nil)
	for _, m := range migration.Of(ordered, req.DB) {
		if m.Name == mg.Name {
			break
		}
		if err := m.Apply(st); err != nil {
			return err
		}
	}
	var steps []migration.Step
	if req.Backwards {
		steps, err = mg.Backwards(req.Dialect, st)
	} else {
		steps, err = mg.Forwards(req.Dialect, st)
	}
	if err != nil {
		return err
	}
	for _, s := range steps {
		if s.Go != nil && s.SQL == "" {
			fmt.Fprintln(w, "-- Go code (RunGo)")
			continue
		}
		fmt.Fprintln(w, strings.TrimRight(s.SQL, ";\n ")+";")
	}
	return nil
}
