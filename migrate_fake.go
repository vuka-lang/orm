package orm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/vuka-lang/orm/v2/internal/schema"
	"github.com/vuka-lang/orm/v2/migration"
)

// fake takes database name to target as to does, recording instead of
// running, orm migrate --fake: the migrations after target recorded
// unapplied (last first), then target recorded applied once every
// migration it depends on is; "zero" records them all unapplied. No
// migration's SQL runs.
func (r *migrator) fake(ctx context.Context, name, target string) error {
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
	var unapply []migration.Migration
	for i := len(mine) - 1; i > last; i-- {
		if s.applied[mine[i].Name] {
			unapply = append(unapply, mine[i])
		}
	}
	if len(unapply) > 0 && r.confirm != nil && !r.confirm(name, unapply) {
		return fmt.Errorf("orm: migrate cancelled: nothing recorded on %s", dbLabel(name))
	}
	for _, mg := range unapply {
		if err := s.record(ctx, mg, false); err != nil {
			return err
		}
		r.log("migration faked unapplied: %s", label(mg))
	}
	if last < 0 || s.applied[mine[last].Name] {
		return nil
	}
	mg := mine[last]
	missing, err := r.unappliedDependencies(ctx, mg)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, d := range missing {
			names[i] = label(d)
		}
		return fmt.Errorf("orm: can't fake %s: it depends on %s, not applied; apply or fake them first", label(mg), strings.Join(names, ", "))
	}
	if err := s.record(ctx, mg, true); err != nil {
		return err
	}
	r.log("migration faked: %s", label(mg))
	return nil
}

// unappliedDependencies is the migrations mg depends on, directly or
// through others, that their databases haven't applied, in their order.
func (r *migrator) unappliedDependencies(ctx context.Context, mg migration.Migration) ([]migration.Migration, error) {
	needed := map[string]bool{}
	var visit func(migration.Migration)
	visit = func(mg migration.Migration) {
		for _, d := range mg.Dependencies {
			db, name, cross := strings.Cut(d, ":")
			if !cross {
				db, name = mg.DB, d
			}
			i := slices.IndexFunc(r.ordered, func(o migration.Migration) bool { return o.DB == db && o.Name == name })
			if i >= 0 && !needed[label(r.ordered[i])] {
				needed[label(r.ordered[i])] = true
				visit(r.ordered[i])
			}
		}
	}
	visit(mg)
	var out []migration.Migration
	for _, dep := range r.ordered {
		if !needed[label(dep)] {
			continue
		}
		s, err := r.session(ctx, dep.DB)
		if err != nil {
			return nil, err
		}
		if !s.applied[dep.Name] {
			out = append(out, dep)
		}
	}
	return out, nil
}

// fakeIfMade records mg applied without running it, when fakeInitial is
// on and its tables are made already (madeAlready); from is the state
// before it.
func (r *migrator) fakeIfMade(ctx context.Context, s *session, mg migration.Migration, from *migration.State) (bool, error) {
	if !r.fakeInitial {
		return false, nil
	}
	made, err := s.madeAlready(ctx, mg, from)
	if !made || err != nil {
		return false, err
	}
	if err := s.record(ctx, mg, true); err != nil {
		return false, err
	}
	r.log("migration faked, its tables exist: %s", label(mg))
	return true, nil
}

// record records mg applied (or, back, not) without running its SQL.
func (s *session) record(ctx context.Context, mg migration.Migration, forward bool) error {
	s.faking = true
	defer func() { s.faking = false }()
	return s.run(ctx, mg, nil, forward)
}

// madeAlready is whether the tables mg creates are in the database
// already, each with the columns mg gives it, and the columns its
// AddFields add too: orm migrate --fake-initial. A migration creating no
// table isn't, nor is one none of whose tables exists. Only some of them
// there, or a column missing, fails, naming what differs: the migration
// can be neither recorded nor run. Columns the database has beyond them
// don't count.
func (s *session) madeAlready(ctx context.Context, mg migration.Migration, from *migration.State) (bool, error) {
	to := from.Clone()
	if err := mg.Apply(to); err != nil {
		return false, err
	}
	var created []string
	added := map[string][]string{}
	var addedTo []string
	for _, op := range mg.Operations {
		switch o := op.(type) {
		case migration.CreateModel:
			created = append(created, o.Table)
		case migration.AddField:
			if _, ok := added[o.Table]; !ok {
				addedTo = append(addedTo, o.Table)
			}
			added[o.Table] = append(added[o.Table], o.Name)
		}
	}
	if len(created) == 0 {
		return false, nil
	}
	have := map[string][]string{}
	for _, t := range created {
		cols, err := s.columns(ctx, t)
		if err != nil {
			return false, err
		}
		if cols != nil {
			have[t] = cols
		}
	}
	if len(have) == 0 {
		return false, nil
	}
	var differs []string
	lacks := func(table string, cols, want []string) {
		var gone []string
		for _, c := range want {
			if !slices.ContainsFunc(cols, func(h string) bool { return s.sameName(h, c) }) {
				gone = append(gone, c)
			}
		}
		if len(gone) > 0 {
			differs = append(differs, fmt.Sprintf("table %s has no column %s", table, strings.Join(gone, ", ")))
		}
	}
	for _, t := range created {
		cols, ok := have[t]
		if !ok {
			differs = append(differs, "table "+t+" doesn't exist")
			continue
		}
		var want []string
		if i := slices.IndexFunc(to.Tables(), func(tb schema.Table) bool { return tb.Name == t }); i >= 0 {
			for _, c := range to.Tables()[i].Columns {
				want = append(want, c.Name)
			}
		}
		lacks(t, cols, want)
	}
	for _, t := range addedTo {
		if slices.Contains(created, t) {
			continue
		}
		cols, err := s.columns(ctx, t)
		if err != nil {
			return false, err
		}
		lacks(t, cols, added[t])
	}
	if len(differs) > 0 {
		return false, fmt.Errorf("orm: --fake-initial: %s is only partly made on %s, so it is neither faked nor applied: %s", label(mg), dbLabel(s.name), strings.Join(differs, "; "))
	}
	return true, nil
}

// columns is the names of table's columns in the database, nil when it
// has no such table.
func (s *session) columns(ctx context.Context, table string) ([]string, error) {
	var q string
	switch d := s.db.dialect.Name(); d {
	case "sqlite":
		q = "SELECT name FROM pragma_table_info(?)"
	case "postgres":
		q = "SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1"
	case "mysql":
		q = "SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?"
	default:
		return nil, fmt.Errorf("orm: --fake-initial can't read the tables of a %s database", d)
	}
	rows, err := s.c.QueryContext(ctx, q, table)
	if err != nil {
		return nil, fmt.Errorf("orm: --fake-initial: reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// sameName is whether the database names a and b one column: MySQL and
// SQLite ignore case in column names, Postgres keeps a quoted name's.
func (s *session) sameName(a, b string) bool {
	if s.db.dialect.Name() == "postgres" {
		return a == b
	}
	return strings.EqualFold(a, b)
}
