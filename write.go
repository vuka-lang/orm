package orm

import (
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

// insertable is the fields an INSERT writes: all but a zero
// auto-increment key and generated columns.
func (m *Manager[T]) insertable(v reflect.Value) []*field {
	var out []*field
	for _, f := range m.meta.Fields {
		if f.Gen != nil || f == m.meta.PK && peek(v, f.Index, f.Type).IsZero() {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (m *Manager[T]) prepareCreate(ctx context.Context, row *T) error {
	if h, ok := any(row).(BeforeCreater); ok {
		if err := h.BeforeCreate(ctx); err != nil {
			return err
		}
	}
	if h, ok := any(row).(BeforeSaver); ok {
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
	return nil
}

func (m *Manager[T]) afterCreate(ctx context.Context, row *T) error {
	if h, ok := any(row).(AfterCreater); ok {
		if err := h.AfterCreate(ctx); err != nil {
			return err
		}
	}
	if h, ok := any(row).(AfterSaver); ok {
		return h.AfterSave(ctx)
	}
	return nil
}

// Create inserts row and sets its new primary key.
func (m *Manager[T]) Create(ctx context.Context, row *T) (err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	return m.BulkCreate(ctx, []*T{row})
}

// BulkCreate inserts rows in as few statements as it can, setting their
// primary keys, in batches of up to 500 (fewer for a wide model).
func (m *Manager[T]) BulkCreate(ctx context.Context, rows []*T) (err error) {
	ctx, ran := runs(ctx)
	defer func() { refused(ctx, m.name(), ran, err) }()
	if len(rows) == 0 {
		return nil
	}
	c, err := m.conn(ctx)
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
	var keyed, unkeyed []*T
	for _, row := range rows {
		if m.meta.PK != nil && peek(reflect.ValueOf(row).Elem(), m.meta.PK.Index, m.meta.PK.Type).IsZero() {
			unkeyed = append(unkeyed, row)
		} else {
			keyed = append(keyed, row)
		}
	}
	// A batch stays under every database's limit on a statement's
	// arguments (SQLite's 32766 the lowest).
	per := max(1, min(500, 30000/max(1, len(m.meta.Fields))))
	for _, set := range [][]*T{keyed, unkeyed} {
		for batch := range slices.Chunk(set, per) {
			if err := m.insert(ctx, c, batch); err != nil {
				return err
			}
		}
	}
	if _, ok := m.mirrorOf(ctx); ok {
		// The rows as written now, keys included: the caller may change
		// them before the mirror writes after the commit.
		snap := make([]*T, len(rows))
		for i, row := range rows {
			cp := *row
			snap[i] = &cp
		}
		m.mirrored(ctx, c, "create", func(ctx context.Context) error {
			mc, err := m.conn(ctx)
			if err != nil {
				return err
			}
			for batch := range slices.Chunk(snap, per) {
				if err := m.insert(ctx, mc, batch); err != nil {
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
		m.changed(ctx, c, Change[T]{Kind: Created, Row: row})
	}
	return nil
}

func (m *Manager[T]) insert(ctx context.Context, c conn, rows []*T) error {
	fields := m.insertable(reflect.ValueOf(rows[0]).Elem())
	b := newBuilder(c.d, m.meta)
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = b.bare(f)
	}
	var w RowWriter[T]
	if mk, ok := rowScanner[T](m.meta); ok {
		w, _ = mk().(RowWriter[T])
	}
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
			v, err := m.meta.out(f, v)
			if err != nil && outErr == nil {
				outErr = err
			}
			sb.WriteString(b.arg(v))
			n++
		}
		if w != nil {
			vals = w.Values(row, vals)
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
		// MySQL gives the first row's id; the batch's ids follow it.
		for i, row := range rows {
			if err := assign(fieldOf(reflect.ValueOf(row).Elem(), pk.Index), first+int64(i)); err != nil {
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
func (m *Manager[T]) syncSequence(ctx context.Context, c conn) {
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
func (m *Manager[T]) Save(ctx context.Context, row *T) error {
	if m.err != nil {
		return m.err
	}
	return m.save(ctx, row, nil)
}

// save writes fields of row (every one for none) and the auto_now ones,
// set to now, to its row.
func (m *Manager[T]) save(ctx context.Context, row *T, fields []*field) error {
	pk := m.meta.PK
	if pk == nil {
		return fmt.Errorf("orm: %s has no primary key to save by", m.meta.Name)
	}
	if h, ok := any(row).(BeforeSaver); ok {
		if err := h.BeforeSave(ctx); err != nil {
			return err
		}
	}
	v := reflect.ValueOf(row).Elem()
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
		}
		values[f.Name] = value(dst)
	}
	for _, f := range fields {
		if _, ok := values[f.Name]; !ok {
			return fmt.Errorf("orm: %s.%s can't be saved: it is the primary key, generated, or read through a relation", m.meta.Name, f.Name)
		}
	}
	key := value(peek(v, pk.Index, pk.Type))
	n, err := m.Filter(Q{pk.Name: key}).Update(hush(ctx), values)
	if err != nil {
		return err
	}
	if n == 0 {
		// MySQL counts rows changed, not rows matched: a save that
		// changes nothing still found its row.
		ok, err := m.Filter(Q{pk.Name: key}).Exists(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return m.notFound()
		}
	}
	m.adopt(row)
	if h, ok := any(row).(AfterSaver); ok {
		if err := h.AfterSave(ctx); err != nil {
			return err
		}
	}
	if c, err := m.conn(ctx); err == nil {
		m.changed(ctx, c, Change[T]{Kind: Updated, Row: row})
	}
	return nil
}

// Remove deletes row's row, found by primary key. A row of a Model is new
// again after.
func (m *Manager[T]) Remove(ctx context.Context, row *T) error {
	if m.err != nil {
		return m.err
	}
	pk := m.meta.PK
	if pk == nil {
		return fmt.Errorf("orm: %s has no primary key to delete by", m.meta.Name)
	}
	if h, ok := any(row).(BeforeDeleter); ok {
		if err := h.BeforeDelete(ctx); err != nil {
			return err
		}
	}
	key := value(peek(reflect.ValueOf(row).Elem(), pk.Index, pk.Type))
	n, err := m.Filter(Q{pk.Name: key}).Delete(hush(ctx))
	if err != nil {
		return err
	}
	if n == 0 {
		return m.notFound()
	}
	if m.meta.state != nil {
		m.state(row).loaded = false
	}
	if h, ok := any(row).(AfterDeleter); ok {
		if err := h.AfterDelete(ctx); err != nil {
			return err
		}
	}
	if c, err := m.conn(ctx); err == nil {
		m.changed(ctx, c, Change[T]{Kind: Deleted, Row: row})
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
	v := reflect.ValueOf(&row).Elem()
	for k, val := range lookup {
		if strings.Contains(k, "__") && !strings.HasSuffix(k, "__exact") {
			continue
		}
		f, ok := m.meta.field(strings.TrimSuffix(k, "__exact"))
		if !ok {
			return row, false, fmt.Errorf("orm: %s has no field %q", m.meta.Name, k)
		}
		if err := setField(fieldOf(v, f.Index), val); err != nil {
			return row, false, err
		}
	}
	for k, val := range defaults {
		f, ok := m.meta.field(k)
		if !ok {
			return row, false, fmt.Errorf("orm: %s has no field %q", m.meta.Name, k)
		}
		if err := setField(fieldOf(v, f.Index), val); err != nil {
			return row, false, err
		}
	}
	err = m.Create(ctx, &row)
	if errors.Is(err, ErrConflict) {
		got, gerr := m.Get(ctx, lookup)
		return got, false, gerr
	}
	return row, err == nil, err
}

// mapErr is a database error as the ORM means it: a unique violation a
// Conflict on its column, a foreign key one Invalid.
func (m *Manager[T]) mapErr(c conn, err error) error {
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
