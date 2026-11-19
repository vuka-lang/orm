package migration

import (
	"fmt"
	"slices"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// Diff is the operations taking a database from the state from (its
// migrations replayed) to to (the models the program declares), and a
// note beside each that needs a person ("" for none). A table or column
// gone where one alike appeared may have been renamed: ask is asked
// (Did you rename users.mail to users.email?) and on yes the operation
// is a rename, else a removal and an addition. A column's default is
// the model's where it declares one, else the migrations' is kept (the
// value an AddField gave the rows that existed). A table's declared
// checks (its models' Checks) are diffed; the check constraints its
// migrations wrote by hand are theirs alone, unless a model declares one
// of the same name. An unmanaged table's declared indexes and checks are
// diffed, never the table: it is never created, altered or dropped.
func Diff(from, to *State, ask func(question string) bool) ([]Operation, []string) {
	var ops []Operation
	var notes []string
	cur := from.Clone()
	emit := func(op Operation, note string) {
		_ = op.mutate(cur)
		ops = append(ops, op)
		notes = append(notes, note)
	}
	for _, e := range to.exts {
		if !slices.Contains(cur.exts, e) {
			emit(CreateExtension{e}, "")
		}
	}
	diffUnmanaged(cur, to, emit)

	var added, removed []schema.Table
	for _, t := range to.tables {
		if !t.Unmanaged && cur.find(t.Name) < 0 {
			added = append(added, t)
		}
	}
	for _, t := range cur.tables {
		if !t.Unmanaged && to.find(t.Name) < 0 {
			removed = append(removed, t)
		}
	}
	added = slices.DeleteFunc(added, func(t schema.Table) bool {
		for i, r := range removed {
			if sameColumns(r, t) && ask(fmt.Sprintf("Did you rename the table %s to %s?", r.Name, t.Name)) {
				emit(RenameModel{r.Name, t.Name}, "")
				removed = slices.Delete(removed, i, i+1)
				return true
			}
		}
		return false
	})
	for _, t := range schema.Sort(added) {
		op := CreateModel{Table: t.Name, Constraints: t.Constraints, Indexes: t.Indexes}
		for _, c := range t.Columns {
			op.Fields = append(op.Fields, F(c.Name, field(c)))
		}
		emit(op, "")
	}

	// current is a copy of t's table as the operations so far leave it.
	current := func(name string) schema.Table {
		t := cur.tables[cur.find(name)]
		t.Columns, t.Constraints, t.Indexes = slices.Clone(t.Columns), slices.Clone(t.Constraints), slices.Clone(t.Indexes)
		return t
	}
	for _, t := range to.tables {
		if t.Unmanaged || slices.ContainsFunc(added, func(a schema.Table) bool { return a.Name == t.Name }) {
			continue
		}
		have := current(t.Name)
		var gone, come []schema.Column
		for _, c := range have.Columns {
			if _, ok := t.Column(c.Name); !ok {
				gone = append(gone, c)
			}
		}
		for _, c := range t.Columns {
			if _, ok := have.Column(c.Name); !ok {
				come = append(come, c)
			}
		}
		come = slices.DeleteFunc(come, func(c schema.Column) bool {
			for i, g := range gone {
				w := c
				w.Default = g.Default
				if w.Same(g) && ask(fmt.Sprintf("Did you rename %s.%s to %s.%s?", t.Name, g.Name, t.Name, c.Name)) {
					emit(RenameField{t.Name, g.Name, c.Name}, "")
					gone = slices.Delete(gone, i, i+1)
					return true
				}
			}
			return false
		})
		have = current(t.Name)
		for _, k := range staleConstraints(have.Constraints, t.Constraints) {
			emit(RemoveConstraint{t.Name, k.Name}, "")
		}
		for _, ix := range have.Indexes {
			if !slices.Contains(t.Indexes, ix) {
				emit(RemoveIndex{t.Name, ix.Name}, "")
			}
		}
		for _, c := range gone {
			emit(RemoveField{t.Name, c.Name}, "drops "+t.Name+"."+c.Name+" and its data: if it was renamed, write m.RenameField instead")
		}
		for _, c := range come {
			note := ""
			if !c.Nullable && !c.PK && c.Default == "" && c.Generated.Zero() {
				note = "adds the NOT NULL column " + t.Name + "." + c.Name + ": give the rows that exist a value with .Default(v)"
			}
			emit(AddField{t.Name, c.Name, field(c)}, note)
		}
		for _, c := range t.Columns {
			o, ok := have.Column(c.Name)
			if !ok {
				continue
			}
			if c.Default == "" {
				c.Default = o.Default
			}
			if !c.Same(*o) {
				emit(AlterField{t.Name, c.Name, field(c)}, "")
			}
		}
		have = current(t.Name)
		for _, k := range t.Constraints {
			if !slices.ContainsFunc(have.Constraints, func(o Constraint) bool { return o.Name == k.Name }) {
				emit(AddConstraint{Table: t.Name, Constraint: k}, "")
			}
		}
		for _, ix := range t.Indexes {
			if !slices.Contains(have.Indexes, ix) {
				emit(AddIndex{Table: t.Name, Index: ix}, "")
			}
		}
	}

	sorted := schema.Sort(removed)
	slices.Reverse(sorted)
	for _, t := range sorted {
		emit(DeleteModel{t.Name}, "drops the table "+t.Name+" and its rows: if it was renamed, write m.RenameModel instead")
	}
	return ops, notes
}

// diffUnmanaged is the operations on what unmanaged tables have that
// their migrations own: the indexes and checks declared on them, and the
// columns of their models' Meta.Managed fields. Those no model declares
// any more are removed, the new ones added, the columns' changes altered.
// A column whose field is gone is dropped (the migrations owned it); one
// whose field stays, no longer in Managed, is forgotten: the table keeps
// it, unchanged. A model gone, or that became managed, forgets its
// columns (the table keeps them) and has its table created afterwards
// when managed; one that became unmanaged keeps its table and columns as
// they are.
func diffUnmanaged(cur, to *State, emit func(Operation, string)) {
	for _, have := range slices.Clone(cur.tables) {
		if i := to.find(have.Name); have.Unmanaged && (i < 0 || !to.tables[i].Unmanaged) {
			for _, k := range slices.Clone(have.Constraints) {
				emit(RemoveConstraint{have.Name, k.Name}, "")
			}
			for _, ix := range slices.Clone(have.Indexes) {
				emit(RemoveIndex{have.Name, ix.Name}, "")
			}
			for _, c := range slices.Clone(have.Columns) {
				emit(ForgetField{have.Name, c.Name}, "leaves "+have.Name+"."+c.Name+" in the table: write m.RemoveField to drop it")
			}
		}
	}
	for _, t := range to.tables {
		if !t.Unmanaged {
			continue
		}
		diffOwned(cur, t, emit)
		for _, k := range staleConstraints(cur.constraintsOf(t.Name), t.Constraints) {
			emit(RemoveConstraint{t.Name, k.Name}, "")
		}
		for _, k := range t.Constraints {
			if !slices.ContainsFunc(cur.constraintsOf(t.Name), func(o Constraint) bool { return o.Name == k.Name }) {
				emit(AddConstraint{Table: t.Name, Constraint: k}, "")
			}
		}
		for _, ix := range slices.Clone(cur.indexesOf(t.Name)) {
			if !slices.Contains(t.Indexes, ix) {
				emit(RemoveIndex{t.Name, ix.Name}, "")
			}
		}
		for _, ix := range t.Indexes {
			if !slices.Contains(cur.indexesOf(t.Name), ix) {
				emit(AddIndex{Table: t.Name, Index: ix}, "")
			}
		}
	}
}

// diffOwned is the operations on the columns the migrations of t, an
// unmanaged table, own (see diffUnmanaged). A table that became
// unmanaged is left as its migrations made it.
func diffOwned(cur *State, t schema.Table, emit func(Operation, string)) {
	var owned []schema.Column
	if i := cur.find(t.Name); i >= 0 {
		if !cur.tables[i].Unmanaged {
			return
		}
		owned = slices.Clone(cur.tables[i].Columns)
	}
	for _, c := range owned {
		switch _, ok := t.Column(c.Name); {
		case ok:
		case slices.Contains(t.Mapped, c.Name):
			emit(ForgetField{t.Name, c.Name}, "")
		default:
			emit(RemoveField{t.Name, c.Name}, "drops "+t.Name+"."+c.Name+" and its data: to keep the column, keep its field and drop it from Meta.Managed")
		}
	}
	for _, c := range t.Columns {
		o := slices.IndexFunc(owned, func(h schema.Column) bool { return h.Name == c.Name })
		switch {
		case o < 0:
			note := ""
			if !c.Nullable && c.Default == "" && c.Generated.Zero() {
				note = "adds the NOT NULL column " + t.Name + "." + c.Name + " to a table another system writes: give it .Null() or a .Default(v)"
			}
			emit(AddField{t.Name, c.Name, field(c)}, note)
		default:
			if c.Default == "" {
				c.Default = owned[o].Default
			}
			if !c.Same(owned[o]) {
				emit(AlterField{t.Name, c.Name, field(c)}, "")
			}
		}
	}
}

// staleConstraints is the constraints of have that want, the model's,
// no longer has as they are: unique ones and declared checks it doesn't
// declare alike. A check written by hand is the migrations' own, kept
// unless want declares one of its name.
func staleConstraints(have, want []Constraint) []Constraint {
	var out []Constraint
	for _, k := range have {
		named := slices.ContainsFunc(want, func(w Constraint) bool { return w.Name == k.Name })
		switch {
		case k.Check != "" && !named:
		case !slices.ContainsFunc(want, k.Same):
			out = append(out, k)
		}
	}
	return out
}

// sameColumns is whether two tables have the same columns, named alike.
func sameColumns(a, b schema.Table) bool {
	return slices.EqualFunc(a.Columns, b.Columns, func(x, y schema.Column) bool { return x.Name == y.Name && x.Same(y) })
}

// field is c's definition, its names left to the state.
func field(c schema.Column) Field {
	c.Name, c.UniqueName, c.IndexName = "", "", ""
	if c.FK != nil {
		fk := *c.FK
		fk.Name = ""
		c.FK = &fk
	}
	return Field{c}
}

// Autodetected is whether op is one Diff writes, as it writes it: not a
// RunSQL, RunGo or CreateFunction, an index made concurrently or a check
// added NotValid, which a person writes.
func Autodetected(op Operation) bool {
	switch o := op.(type) {
	case CreateExtension, CreateModel, DeleteModel, RenameModel, AddField, RemoveField, ForgetField, AlterField, RenameField, RemoveIndex, RemoveConstraint:
		return true
	case AddIndex:
		return !o.Concurrently
	case AddConstraint:
		return !o.NotValid
	}
	return false
}
