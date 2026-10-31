package orm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Clock is the time auto_now and auto_now_add fields take.
var Clock = time.Now

// The hooks a model may have, promoted from embedded structs like any
// method. A Before hook's error stops the write.
type (
	BeforeCreater interface {
		BeforeCreate(ctx context.Context) error
	}
	AfterCreater interface {
		AfterCreate(ctx context.Context) error
	}
	BeforeSaver interface {
		BeforeSave(ctx context.Context) error
	}
	AfterSaver interface {
		AfterSave(ctx context.Context) error
	}
	BeforeDeleter interface {
		BeforeDelete(ctx context.Context) error
	}
	AfterDeleter interface {
		AfterDelete(ctx context.Context) error
	}
)

func stamp(f *field) any {
	now := Clock()
	switch {
	case f.Type == timeType:
		return now
	case f.Type.Kind() == reflect.Pointer && f.Type.Elem() == timeType:
		return &now
	}
	return now
}

// insertable is the fields an INSERT writes: all but a zero key the
// database counts and generated columns.
func (m *managerCore) insertable(v reflect.Value) []*field {
	var out []*field
	for _, f := range m.meta.Fields {
		if f.Gen != nil || f == m.meta.PK && m.meta.counted() && peek(v, f.Index, f.Type).IsZero() {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (m *managerCore) prepareCreate(ctx context.Context, row any) error {
	if h, ok := row.(BeforeCreater); ok {
		if err := h.BeforeCreate(ctx); err != nil {
			return err
		}
	}
	if h, ok := row.(BeforeSaver); ok {
		if err := h.BeforeSave(ctx); err != nil {
			return err
		}
	}
	v := reflect.ValueOf(row).Elem()
	for _, f := range m.meta.Fields {
		dst := fieldOf(v, f.Index)
		if f.AutoNow || (f.AutoNowAdd && dst.IsZero()) {
			if err := setField(dst, stamp(f)); err != nil {
				return err
			}
		}
	}
	if err := m.meta.prepareFields(v, true, nil); err != nil {
		return err
	}
	return m.meta.checkRow(v, nil)
}

func (m *managerCore) afterCreate(ctx context.Context, row any) error {
	if h, ok := row.(AfterCreater); ok {
		if err := h.AfterCreate(ctx); err != nil {
			return err
		}
	}
	if h, ok := row.(AfterSaver); ok {
		return h.AfterSave(ctx)
	}
	return nil
}

// Create inserts row and sets its new primary key.
func (m *Manager[T]) Create(ctx context.Context, row *T) error { return m.c.create(ctx, row) }

func (m *managerCore) create(ctx context.Context, row any) (err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	return m.bulkCreate(ctx, []any{row})
}

// BulkCreate inserts rows in as few statements as it can, setting their
// primary keys, in batches of up to 500 (fewer for a wide model).
func (m *Manager[T]) BulkCreate(ctx context.Context, rows []*T) error {
	erased := make([]any, len(rows))
	for i, row := range rows {
		erased[i] = row
	}
	return m.c.bulkCreate(ctx, erased)
}

// bulkCreate is BulkCreate of rows, each a *T.
func (m *managerCore) bulkCreate(ctx context.Context, rows []any) (err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	if len(rows) == 0 {
		return nil
	}
	c, err := m.conn(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := m.prepareCreate(ctx, row); err != nil {
			return err
		}
	}
	// Rows with and without a key of their own insert apart, so each
	// statement's columns agree.
	var keyed, unkeyed []any
	for _, row := range rows {
		if m.meta.PK != nil && m.meta.counted() && peek(reflect.ValueOf(row).Elem(), m.meta.PK.Index, m.meta.PK.Type).IsZero() {
			unkeyed = append(unkeyed, row)
		} else {
			keyed = append(keyed, row)
		}
	}
	// A batch stays under every database's limit on a statement's
	// arguments (SQLite's 32766 the lowest).
	per := max(1, min(500, 30000/max(1, len(m.meta.Fields))))
	for _, set := range [][]any{keyed, unkeyed} {
		for batch := range slices.Chunk(set, per) {
			if err := m.insert(ctx, c, batch); err != nil {
				return err
			}
		}
	}
	if _, ok := m.mirrorOf(ctx); ok {
		// The rows as written now, keys included: the caller may change
		// them before the mirror writes after the commit.
		snap := make([]any, len(rows))
		for i, row := range rows {
			snap[i] = copyRow(row)
		}
		m.mirrored(ctx, c, "create", func(ctx context.Context, mm *managerCore) error {
			mc, err := mm.conn(ctx, true)
			if err != nil {
				return err
			}
			for batch := range slices.Chunk(snap, per) {
				if err := mm.insert(ctx, mc, batch); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for _, row := range rows {
		m.adopt(row)
		if err := m.afterCreate(ctx, row); err != nil {
			return err
		}
		m.changed(ctx, c, changeEvent{kind: Created, row: row})
	}
	return nil
}

func (m *managerCore) insert(ctx context.Context, c conn, rows []any) error {
	fields := m.insertable(reflect.ValueOf(rows[0]).Elem())
	b := newBuilder(c.d, m.meta)
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = b.bare(f)
	}
	w := m.typed.writer()
	written := make([]bool, len(m.meta.Fields))
	for i, f := range m.meta.Fields {
		written[i] = slices.Contains(fields, f)
	}
	// The statement written in one buffer, its arguments in one slice:
	// a batch of 500 rows is one allocation each, not a few per row.
	b.st.args = make([]any, 0, len(rows)*len(fields))
	var sb strings.Builder
	sb.Grow(64 + len(cols)*16 + len(rows)*len(fields)*7)
	table, err := b.from(0)
	if err != nil {
		return err
	}
	sb.WriteString("INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES ")
	var vals []any
	var outErr error
	for r, row := range rows {
		if r > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		n := 0
		mark := func(f *field, v any) {
			if n > 0 {
				sb.WriteString(", ")
			}
			v, err := m.meta.out(c.d, f, v)
			if err != nil && outErr == nil {
				outErr = err
			}
			sb.WriteString(b.arg(v))
			n++
		}
		if w != nil {
			vals = w(row, vals)
			for i, f := range m.meta.Fields {
				if written[i] {
					mark(f, vals[i])
				}
			}
		} else {
			v := reflect.ValueOf(row).Elem()
			for _, f := range fields {
				mark(f, value(peek(v, f.Index, f.Type)))
			}
		}
		sb.WriteByte(')')
	}
	if outErr != nil {
		return outErr
	}
	s := sb.String()
	pk := m.meta.PK
	needKey := pk != nil && !slices.Contains(fields, pk)
	if needKey && c.d.Name() == "postgres" {
		m.syncSequence(ctx, c)
	}
	if needKey && c.d.Returning() {
		rs, err := c.query(ctx, s+" RETURNING "+b.bare(pk), b.args())
		if err != nil {
			return m.mapErr(c, err)
		}
		defer rs.Close()
		var key cell
		for i := 0; rs.Next() && i < len(rows); i++ {
			key.dst = fieldOf(reflect.ValueOf(rows[i]).Elem(), pk.Index)
			if err := rs.Scan(&key); err != nil {
				return m.mapErr(c, err)
			}
		}
		if err := rs.Err(); err != nil {
			return m.mapErr(c, err)
		}
		return nil
	}
	res, err := c.exec(ctx, s, b.args())
	if err != nil {
		return m.mapErr(c, err)
	}
	if needKey {
		first, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// MySQL gives the first row's id; the batch's follow it, a statement
		// with its rows' count reserving them together, each the session's
		// auto_increment_increment on (a Galera cluster sets it to its size).
		step := int64(1)
		if len(rows) > 1 && c.d.Name() == "mysql" {
			rs, err := c.query(ctx, "SELECT @@auto_increment_increment", nil)
			if err != nil {
				return err
			}
			for rs.Next() {
				err = rs.Scan(&step)
			}
			if err = cmp.Or(err, rs.Close(), rs.Err()); err != nil {
				return err
			}
		}
		for i, row := range rows {
			if err := assign(fieldOf(reflect.ValueOf(row).Elem(), pk.Index), first+int64(i)*step); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncSequence moves a Postgres table's key sequence past its highest
// key, once per database in the process, before the first insert that
// takes a key from it: rows written with their keys (a mirror, a copy
// from another database) don't move the sequence, so a table cut over to
// Postgres would otherwise hand out keys it already holds.
func (m *managerCore) syncSequence(ctx context.Context, c conn) {
	if _, loaded := m.synced.LoadOrStore(c.db, true); loaded {
		return
	}
	table := strings.ReplaceAll(c.d.Quote(m.meta.Table), "'", "''")
	pk := c.d.Quote(m.meta.PK.Column)
	q := "SELECT setval(pg_get_serial_sequence('" + table + "', '" + strings.ReplaceAll(m.meta.PK.Column, "'", "''") + "'), COALESCE(MAX(" + pk + "), 0) + 1, false) FROM " + c.d.Quote(m.meta.Table) +
		" WHERE pg_get_serial_sequence('" + table + "', '" + strings.ReplaceAll(m.meta.PK.Column, "'", "''") + "') IS NOT NULL"
	if _, err := c.exec(ctx, q, nil); err != nil {
		m.synced.Delete(c.db)
		slog.WarnContext(ctx, "orm: moving the key sequence past the table's keys", "table", m.meta.Table, "err", err)
	}
}

// Save writes every field of row to its row, found by primary key; auto_now
// fields are set to now.
func (m *Manager[T]) Save(ctx context.Context, row *T) error { return m.c.saveRow(ctx, row) }

func (m *managerCore) saveRow(ctx context.Context, row any) error {
	if m.err != nil {
		return m.err
	}
	return m.save(ctx, row, nil)
}

// save writes fields of row (every one for none) and the auto_now ones,
// set to now, to its row.
func (m *managerCore) save(ctx context.Context, row any, fields []*field) error {
	pk := m.meta.PK
	if pk == nil {
		return fmt.Errorf("orm: %s has no primary key to save by", m.meta.Name)
	}
	if h, ok := row.(BeforeSaver); ok {
		if err := h.BeforeSave(ctx); err != nil {
			return err
		}
	}
	v := reflect.ValueOf(row).Elem()
	if err := m.meta.prepareFields(v, false, fields); err != nil {
		return err
	}
	values := Set{}
	for _, f := range m.meta.Fields {
		if f.PK || f.Gen != nil || fields != nil && !f.AutoNow && !slices.Contains(fields, f) {
			continue
		}
		dst := fieldOf(v, f.Index)
		if f.AutoNow {
			if err := setField(dst, stamp(f)); err != nil {
				return err
			}
			f.normalize(v)
		}
		values[f.Name] = value(dst)
	}
	for _, f := range fields {
		if _, ok := values[f.Name]; !ok {
			return fmt.Errorf("orm: %s.%s can't be saved: it is the primary key, generated, or read through a relation", m.meta.Name, f.Name)
		}
	}
	key := value(peek(v, pk.Index, pk.Type))
	byKey := m.all.filter([]Cond{Q{pk.Name: key}})
	n, err := m.update(hush(ctx), byKey, values)
	if err != nil {
		return err
	}
	if n == 0 {
		// MySQL counts rows changed, not rows matched: a save that
		// changes nothing still found its row.
		ok, err := m.exists(Primary(ctx), byKey)
		if err != nil {
			return err
		}
		if !ok {
			return m.notFound()
		}
	}
	m.adopt(row)
	if h, ok := row.(AfterSaver); ok {
		if err := h.AfterSave(ctx); err != nil {
			return err
		}
	}
	if c, err := m.conn(ctx, true); err == nil {
		m.changed(ctx, c, changeEvent{kind: Updated, row: row})
	}
	return nil
}

// Remove deletes row's row, found by primary key. A row of a Model is new
// again after.
func (m *Manager[T]) Remove(ctx context.Context, row *T) error { return m.c.remove(ctx, row) }

func (m *managerCore) remove(ctx context.Context, row any) error {
	if m.err != nil {
		return m.err
	}
	pk := m.meta.PK
	if pk == nil {
		return fmt.Errorf("orm: %s has no primary key to delete by", m.meta.Name)
	}
	if h, ok := row.(BeforeDeleter); ok {
		if err := h.BeforeDelete(ctx); err != nil {
			return err
		}
	}
	key := value(peek(reflect.ValueOf(row).Elem(), pk.Index, pk.Type))
	n, err := m.delete(hush(ctx), m.all.filter([]Cond{Q{pk.Name: key}}))
	if err != nil {
		return err
	}
	if n == 0 {
		return m.notFound()
	}
	if m.meta.state != nil {
		m.stateOf(reflect.ValueOf(row).UnsafePointer()).loaded = false
	}
	if h, ok := row.(AfterDeleter); ok {
		if err := h.AfterDelete(ctx); err != nil {
			return err
		}
	}
	if c, err := m.conn(ctx, true); err == nil {
		m.changed(ctx, c, changeEvent{kind: Deleted, row: row})
	}
	return nil
}

// Defaults is the fields GetOrCreate sets on a row it creates, beyond those
// of its lookup.
type Defaults map[string]any

// GetOrCreate is the row matching lookup, or a new one made from lookup's
// exact fields and defaults; created says which. A row another request
// inserted first is read rather than duplicated.
func (m *Manager[T]) GetOrCreate(ctx context.Context, lookup Q, defaults Defaults) (row T, created bool, err error) {
	row, err = m.Get(ctx, lookup)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return row, false, err
	}
	if err := m.c.setLookup(reflect.ValueOf(&row).Elem(), lookup, defaults); err != nil {
		return row, false, err
	}
	err = m.Create(ctx, &row)
	if errors.Is(err, ErrConflict) {
		got, gerr := m.Get(Primary(ctx), lookup)
		return got, false, gerr
	}
	return row, err == nil, err
}

// setLookup sets a new row's fields from lookup's exact ones and defaults.
func (m *managerCore) setLookup(v reflect.Value, lookup Q, defaults Defaults) error {
	for k, val := range lookup {
		if strings.Contains(k, "__") && !strings.HasSuffix(k, "__exact") {
			continue
		}
		f, ok := m.meta.field(strings.TrimSuffix(k, "__exact"))
		if !ok {
			return fmt.Errorf("orm: %s has no field %q", m.meta.Name, k)
		}
		if err := setField(fieldOf(v, f.Index), val); err != nil {
			return err
		}
	}
	return m.setDefaults(v, defaults)
}

func (m *managerCore) setDefaults(v reflect.Value, defaults Defaults) error {
	for k, val := range defaults {
		f, ok := m.meta.field(k)
		if !ok {
			return fmt.Errorf("orm: %s has no field %q", m.meta.Name, k)
		}
		if err := setField(fieldOf(v, f.Index), val); err != nil {
			return err
		}
	}
	return nil
}

// mapErr is a database error as the ORM means it: a unique violation a
// Conflict on its column, a foreign key one Invalid.
func (m *managerCore) mapErr(c conn, err error) error {
	kind, col := c.d.Violation(err)
	name := col
	if f, ok := m.meta.field(col); ok {
		name = f.Column
	} else if col != "" {
		// A constraint's name (users_email_key, idx_users_email): the
		// longest column of the model it names.
		name = ""
		for _, f := range m.meta.Fields {
			if (strings.Contains(col, "_"+f.Column+"_") || strings.HasSuffix(col, "_"+f.Column)) && len(f.Column) > len(name) {
				name = f.Column
			}
		}
	}
	switch kind {
	case uniqueViolation:
		e := errorf(Conflict, m.meta.Name, "%s with this %s already exists", m.meta.Name, orWord(name, "value"))
		e.Err = err
		if name != "" {
			e.Field(name, "already exists")
		}
		return e
	case foreignKeyViolation:
		e := errorf(Invalid, m.meta.Name, "%s refers to a row that does not exist", m.meta.Name)
		e.Err = err
		if name != "" {
			e.Field(name, "does not exist")
		}
		return e
	}
	return err
}

func orWord(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// UpdateOrCreate updates the row matching lookup with defaults, or
// creates one from lookup's exact fields and defaults (as GetOrCreate),
// in a transaction; created says which.
func (m *Manager[T]) UpdateOrCreate(ctx context.Context, lookup Q, defaults Defaults) (row T, created bool, err error) {
	c, err := m.c.conn(ctx, true)
	if err != nil {
		return row, false, err
	}
	err = AtomicOn(ctx, c.src, func(ctx context.Context) error {
		row, err = m.Get(ctx, lookup)
		if errors.Is(err, ErrNotFound) {
			row, created, err = m.GetOrCreate(ctx, lookup, defaults)
			return err
		}
		if err != nil {
			return err
		}
		if err := m.c.setDefaults(reflect.ValueOf(&row).Elem(), defaults); err != nil {
			return err
		}
		return m.Save(ctx, &row)
	})
	return row, created, err
}
