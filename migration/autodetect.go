package migration

import (
	"fmt"
	"slices"

	"github.com/vuka-lang/orm/internal/schema"
)

// Diff is the operations taking a database from the state from (its
// migrations replayed) to to (the models the program declares), and a
// note beside each that needs a person ("" for none). A table or column
// gone where one alike appeared may have been renamed: ask is asked
// (Did you rename users.mail to users.email?) and on yes the operation
// is a rename, else a removal and an addition. A column's default and
// a table's check constraints are the migrations' alone: models don't
// declare them, so they are kept.
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

	var added, removed []schema.Table
	for _, t := range to.tables {
		if cur.find(t.Name) < 0 {
			added = append(added, t)
		}
	}
	for _, t := range cur.tables {
		if to.find(t.Name) < 0 {
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
		if slices.ContainsFunc(added, func(a schema.Table) bool { return a.Name == t.Name }) {
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
		for _, k := range have.Constraints {
			if k.Check == "" && !slices.ContainsFunc(t.Constraints, func(n Constraint) bool { return n.Name == k.Name && slices.Equal(n.Columns, k.Columns) }) {
				emit(RemoveConstraint{t.Name, k.Name}, "")
			}
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
			c.Default = o.Default
			if !c.Same(*o) {
				emit(AlterField{t.Name, c.Name, field(c)}, "")
			}
		}
		have = current(t.Name)
		for _, k := range t.Constraints {
			if !slices.ContainsFunc(have.Constraints, func(o Constraint) bool { return o.Name == k.Name }) {
				emit(AddConstraint{t.Name, k}, "")
			}
		}
		for _, ix := range t.Indexes {
			if !slices.Contains(have.Indexes, ix) {
				emit(AddIndex{t.Name, ix}, "")
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
