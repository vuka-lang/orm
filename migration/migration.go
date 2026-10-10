// Package migration is the ORM's schema migrations, Django's: Go files
// orm makemigrations writes into the program (migrations/NNNN_name.go, a
// package of their own), each registering a Migration of operations from
// its init. orm.Migrate applies the registered migrations at start up,
// orm migrate on demand, forwards or back.
//
//	import m "github.com/vuka-lang/orm/migration"
//
//	func init() {
//		m.Register(m.Migration{
//			Name:         "0002_users_email",
//			Dependencies: []string{"0001_initial"},
//			Operations: []m.Operation{
//				m.AddField{Table: "users", Name: "email", Field: m.Varchar(255).Unique().Null()},
//			},
//		})
//	}
//
// The schema a database has is the replay of its migrations: no snapshot
// file is kept, and makemigrations compares the replay with the models
// the program declares.
package migration

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/vuka-lang/orm/internal/schema"
)

// Migration is one step of a database's schema: its operations, applied
// in order, after the migrations it depends on.
type Migration struct {
	Name string // NNNN_words, unique within its database
	DB   string // the database's name (orm.AddDatabase); "" the default
	// Dependencies is the migrations applied before it: names of its
	// database's, or "<db>:<name>" of another's (":<name>" the default's).
	Dependencies []string
	Operations   []Operation
}

// Tx is where a RunGo runs: the migration's transaction (MySQL, which
// can't roll DDL back, its connection).
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Step is a statement (or several, a RunSQL's) applying an operation, or
// Go code a RunGo runs.
type Step struct {
	SQL string
	Go  func(ctx context.Context, tx Tx) error
}

var registry struct {
	mu   sync.Mutex
	list []Migration
}

// Register adds a migration to the program's: a migrations package calls
// it from each file's init. A second migration of one database and name
// panics.
func Register(mg Migration) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if slices.ContainsFunc(registry.list, func(o Migration) bool { return o.DB == mg.DB && o.Name == mg.Name }) {
		panic(fmt.Sprintf("migration: %s registered twice", mg.key()))
	}
	registry.list = append(registry.list, mg)
}

func (mg Migration) key() string { return mg.DB + ":" + mg.Name }

// dependency is the key of a dependency of mg.
func (mg Migration) dependency(d string) string {
	if strings.Contains(d, ":") {
		return d
	}
	return mg.DB + ":" + d
}

// Ordered is every registered migration, each after its dependencies (by
// database and name where none orders them).
func Ordered() ([]Migration, error) {
	registry.mu.Lock()
	list := slices.Clone(registry.list)
	registry.mu.Unlock()
	return Order(list)
}

// Order is list ordered as Ordered orders the registered migrations.
func Order(list []Migration) ([]Migration, error) {
	list = slices.Clone(list)
	sort.Slice(list, func(i, j int) bool {
		if list[i].DB != list[j].DB {
			return list[i].DB < list[j].DB
		}
		return list[i].Name < list[j].Name
	})
	by := map[string]Migration{}
	for _, mg := range list {
		by[mg.key()] = mg
	}
	var out []Migration
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(mg Migration, path []string) error
	visit = func(mg Migration, path []string) error {
		switch state[mg.key()] {
		case 1:
			return fmt.Errorf("migration: a dependency cycle: %s", strings.Join(append(path, mg.key()), " → "))
		case 2:
			return nil
		}
		state[mg.key()] = 1
		for _, d := range mg.Dependencies {
			dep, ok := by[mg.dependency(d)]
			if !ok {
				return fmt.Errorf("migration: %s depends on %s, which no migrations package the program links registers", mg.key(), mg.dependency(d))
			}
			if err := visit(dep, append(path, mg.key())); err != nil {
				return err
			}
		}
		state[mg.key()] = 2
		out = append(out, mg)
		return nil
	}
	for _, mg := range list {
		if err := visit(mg, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Of is the migrations of database db among ordered, in their order.
func Of(ordered []Migration, db string) []Migration {
	var out []Migration
	for _, mg := range ordered {
		if mg.DB == db {
			out = append(out, mg)
		}
	}
	return out
}

// Leaves is the names of db's migrations none of its others depends on:
// what the next one depends on.
func Leaves(ordered []Migration, db string) []string {
	mine := Of(ordered, db)
	needed := map[string]bool{}
	for _, mg := range mine {
		for _, d := range mg.Dependencies {
			needed[mg.dependency(d)] = true
		}
	}
	var out []string
	for _, mg := range mine {
		if !needed[mg.key()] {
			out = append(out, mg.Name)
		}
	}
	return out
}

// Find is db's migration among ordered whose name is name, or starts with
// it (0002 for 0002_users_email) when one does.
func Find(ordered []Migration, db, name string) (Migration, error) {
	var hits []Migration
	for _, mg := range Of(ordered, db) {
		if mg.Name == name {
			return mg, nil
		}
		if strings.HasPrefix(mg.Name, name) {
			hits = append(hits, mg)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Migration{}, fmt.Errorf("migration: no migration %q of %s", name, dbLabel(db))
	}
	return Migration{}, fmt.Errorf("migration: %q names several migrations of %s", name, dbLabel(db))
}

func dbLabel(db string) string {
	if db == "" {
		return "the default database"
	}
	return "database " + db
}

// State is a database's schema as its migrations leave it: its tables and
// extensions.
type State struct {
	tables []schema.Table
	exts   []string
}

// NewState is a state of tables and extensions.
func NewState(tables []schema.Table, exts []string) *State {
	return &State{tables: slices.Clone(tables), exts: slices.Clone(exts)}
}

// Tables is the state's tables.
func (s *State) Tables() []schema.Table { return s.tables }

// Clone is a copy of s.
func (s *State) Clone() *State {
	c := &State{tables: make([]schema.Table, len(s.tables)), exts: slices.Clone(s.exts)}
	for i, t := range s.tables {
		t.Columns = slices.Clone(t.Columns)
		t.Constraints = slices.Clone(t.Constraints)
		t.Indexes = slices.Clone(t.Indexes)
		c.tables[i] = t
	}
	return c
}

func (s *State) find(table string) int {
	return slices.IndexFunc(s.tables, func(t schema.Table) bool { return t.Name == table })
}

// table is the state's table named.
func (s *State) table(name string) (*schema.Table, error) {
	i := s.find(name)
	if i < 0 {
		return nil, fmt.Errorf("no table %s", name)
	}
	return &s.tables[i], nil
}

// Replay is the state of database db after the migrations of it among
// ordered.
func Replay(ordered []Migration, db string) (*State, error) {
	s := &State{}
	for _, mg := range Of(ordered, db) {
		if err := mg.Apply(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Apply plays mg's operations on s.
func (mg Migration) Apply(s *State) error {
	for _, op := range mg.Operations {
		if err := op.mutate(s); err != nil {
			return fmt.Errorf("migration %s: %s: %w", mg.Name, Describe(op), err)
		}
	}
	return nil
}

// Forwards is the steps applying mg on dialect, from the state s its
// database is in; s is left as mg leaves it.
func (mg Migration) Forwards(dialect string, s *State) ([]Step, error) {
	d := schema.For(dialect)
	var out []Step
	for _, op := range mg.Operations {
		before := s.Clone()
		if err := op.mutate(s); err != nil {
			return nil, fmt.Errorf("migration %s: %s: %w", mg.Name, Describe(op), err)
		}
		steps, err := op.forwards(d, before, s)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %s: %w", mg.Name, Describe(op), err)
		}
		out = append(out, steps...)
	}
	return out, nil
}

// Backwards is the steps unapplying mg on dialect, s the state its
// database was in before mg (left as it is): its operations' reverses,
// last first.
func (mg Migration) Backwards(dialect string, s *State) ([]Step, error) {
	d := schema.For(dialect)
	states := []*State{s.Clone()}
	for _, op := range mg.Operations {
		next := states[len(states)-1].Clone()
		if err := op.mutate(next); err != nil {
			return nil, fmt.Errorf("migration %s: %s: %w", mg.Name, Describe(op), err)
		}
		states = append(states, next)
	}
	var out []Step
	for i := len(mg.Operations) - 1; i >= 0; i-- {
		steps, err := mg.Operations[i].backwards(d, states[i], states[i+1])
		if err != nil {
			return nil, fmt.Errorf("migration %s: %s: %w", mg.Name, Describe(mg.Operations[i]), err)
		}
		out = append(out, steps...)
	}
	return out, nil
}

func sqlSteps(stmts []string) []Step {
	out := make([]Step, len(stmts))
	for i, s := range stmts {
		out[i] = Step{SQL: s}
	}
	return out
}
