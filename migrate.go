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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/orm/internal/schema"
	"github.com/vuka-lang/orm/migration"
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

// declaredState is the schema the program's managed models of the
// databases dbs (orm.On names, "" the default) declare, and their
// extensions; a model dialect can't make fails it.
func declaredState(dbs []string, dialect string) (*migration.State, error) {
	var tables []schema.Table
	at := map[string]int{}
	for _, m := range plannedModels() {
		if m.unmanaged || !slices.Contains(dbs, m.db) {
			continue
		}
		if err := m.madeOn(DialectFor(dialect)); err != nil {
			return nil, err
		}
		ts, err := m.tables()
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

// mergeTable is one table two models map: the columns, constraints and
// indexes of both, which must agree where they overlap.
func mergeTable(a, b schema.Table) (schema.Table, error) {
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
		if !slices.ContainsFunc(a.Constraints, func(o schema.Constraint) bool { return o.Name == k.Name }) {
			a.Constraints = append(a.Constraints, k)
		}
	}
	for _, ix := range b.Indexes {
		i := slices.IndexFunc(a.Indexes, func(have schema.Index) bool { return have.Name == ix.Name })
		switch {
		case i < 0:
			a.Indexes = append(a.Indexes, ix)
		case a.Indexes[i] != ix:
			return a, fmt.Errorf("orm: two models map table %s and disagree on its index %s: make one orm.Unmanaged()", a.Name, ix.Name)
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
// migrations. Set it before migrating (a program moving from nexus, whose
// ORM recorded them in nexus_migrations, sets that).
var MigrationsTable = "orm_migrations"

// migrator applies migrations (ordered, every database's), a session per
// database it touches.
type migrator struct {
	ordered  []migration.Migration
	open     func(ctx context.Context, name string) (*DB, error)
	log      func(format string, args ...any)
	sessions map[string]*session
}

// session is a database being migrated: a connection holding the lock,
// and the migrations it has applied.
type session struct {
	name    string
	db      *DB
	c       *sql.Conn
	unlock  func()
	applied map[string]bool
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
	s := &session{name: name, db: d, c: c, unlock: unlock, applied: map[string]bool{}}
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
		steps, err := mg.Forwards(s.db.dialect.Name(), st)
		if err != nil {
			return err
		}
		if err := s.run(ctx, mg, steps, true); err != nil {
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
	for i := len(mine) - 1; i > last; i-- {
		if !s.applied[mine[i].Name] {
			continue
		}
		steps, err := mine[i].Backwards(s.db.dialect.Name(), before[i])
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
		steps, err := mine[i].Forwards(s.db.dialect.Name(), before[i])
		if err != nil {
			return err
		}
		if err := s.run(ctx, mine[i], steps, true); err != nil {
			return err
		}
		r.log("migration applied: %s", label(mine[i]))
	}
	return nil
}

func label(mg migration.Migration) string {
	if mg.DB == "" {
		return mg.Name
	}
	return mg.DB + ":" + mg.Name
}

// run runs a migration's steps and records it applied (or, back, not):
// in one transaction where the database rolls DDL back, Go steps and the
// ORM's queries on their ctx included. SQLite's foreign keys are off
// while it rebuilds tables.
func (s *session) run(ctx context.Context, mg migration.Migration, steps []migration.Step, forward bool) error {
	dl := s.db.dialect
	sd := schema.For(dl.Name())
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
	do := func(ctx context.Context, tx migration.Tx) error {
		for _, st := range steps {
			if st.Go != nil {
				if err := st.Go(ctx, tx); err != nil {
					return fmt.Errorf("orm: migration %s: %w", label(mg), err)
				}
				continue
			}
			for _, q := range splitSQL(st.SQL, dl.Name()) {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return fmt.Errorf("orm: migration %s: %w\n%s", label(mg), err, q)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, record, args...); err != nil {
			return fmt.Errorf("orm: recording migration %s: %w", label(mg), err)
		}
		return nil
	}
	inner := WithDB(ctx, s.db)
	if dl.Name() == "mysql" {
		if err := do(inner, s.c); err != nil {
			return err
		}
	} else {
		tx, err := s.c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := inTx(inner, s.db.sql, tx, func(ctx context.Context) error { return do(ctx, tx) }); err != nil {
			return err
		}
	}
	s.applied[mg.Name] = forward
	return nil
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
// outside quotes, comments and Postgres dollar quotes; comments and blank
// statements dropped.
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
		if src[i] == ';' {
			flush()
		} else {
			b.WriteByte(src[i])
		}
		return i + 1
	})
	flush()
	return out
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
	// Interactive asks about renames on stdin.
	Number      int    `json:"number,omitempty"`
	Name        string `json:"name,omitempty"`
	Package     string `json:"package,omitempty"`
	Empty       bool   `json:"empty,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	// Target is migrate's: the migration to take DB to ("zero" for
	// none); every database's pending ones when empty. sql's: the
	// migration to print.
	Target    string `json:"target,omitempty"`
	Backwards bool   `json:"backwards,omitempty"` // sql
	// Conns is the URL of each migrations database ("" the default's),
	// for migrate and show to connect (see ConnectURL).
	Conns map[string]string `json:"conns,omitempty"`
	Out   string            `json:"out,omitempty"` // plan: the file its PlanResult goes to
}

// PlanResult is the migration plan wrote: its file's name and source,
// what each operation does, and the notes beside them. No operations is
// no changes.
type PlanResult struct {
	File   string   `json:"file,omitempty"`
	Source []byte   `json:"source,omitempty"`
	Ops    []string `json:"ops"`
	Notes  []string `json:"notes,omitempty"`
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
	if err := serveCLI(context.Background(), raw, os.Stdin, os.Stdout, os.Stderr); err != nil {
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
	r := &migrator{
		ordered: ordered,
		open:    func(ctx context.Context, name string) (*DB, error) { return openConfigured(req.Conns, name) },
		log:     func(format string, args ...any) { fmt.Fprintf(stdout, "  "+format+"\n", args...) },
	}
	defer r.close()
	switch req.Command {
	case "migrate":
		if req.Target == "" {
			return r.forwardAll(ctx, nil)
		}
		return r.to(ctx, req.DB, req.Target)
	case "show":
		return show(ctx, r, stdout)
	}
	return fmt.Errorf("orm: no command %q", req.Command)
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
	ops, notes := migration.Diff(from, to, ask)
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
		if s.Go != nil {
			fmt.Fprintln(w, "-- Go code (RunGo)")
			continue
		}
		fmt.Fprintln(w, strings.TrimRight(s.SQL, ";\n ")+";")
	}
	return nil
}
