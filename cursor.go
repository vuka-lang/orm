package orm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"reflect"
	"slices"
	"strings"
)

// CursorPage reads one page of the query's rows by cursor, as a GraphQL
// connection pages (graphql.Paginate takes a QuerySet): the first rows
// after the row of cursor after, or (last > 0) the last rows before the
// row of cursor before. Either cursor may be "", and both bound the page.
// It returns the rows in the query's order, each row's cursor, and whether
// rows go on past the page: after it reading forwards, before it reading
// backwards.
//
// Cursors are opaque strings, keyset cursors when the query's ordering
// allows: its order (Meta's Ordering when it sets none) is the model's own
// non-nullable columns — the primary key added last when no column of the
// ordering is unique, the primary key alone without an ordering — and a
// page is the rows past the cursor's values in that order, unmoved by
// rows written before it. Any other ordering (an expression, an
// annotation, a related model's field, a nullable column) gets offset
// cursors: a row's position in the result. A cursor made for another
// ordering is refused (ErrCursor).
func (qs QuerySet[T]) CursorPage(ctx context.Context, after, before string, first, last int) (rows []T, cursors []string, more bool, err error) {
	if first > 0 && last > 0 {
		return nil, nil, false, errors.New("orm: CursorPage takes first or last, not both")
	}
	if first <= 0 && last <= 0 {
		return nil, nil, false, errors.New("orm: CursorPage takes a first or a last above zero")
	}
	q := qs.q.ordered()
	keys, keyset := q.m.cursorKeys(q.order)
	tag := orderingTag(keys, keyset)
	var a, b *cursor
	if after != "" {
		if a, err = decodeCursor(after, tag); err != nil {
			return nil, nil, false, err
		}
	}
	if before != "" {
		if b, err = decodeCursor(before, tag); err != nil {
			return nil, nil, false, err
		}
	}
	if keyset {
		return qs.keysetPage(ctx, keys, tag, a, b, first, last)
	}
	return qs.offsetPage(ctx, tag, a, b, first, last)
}

// ErrCursor is a cursor CursorPage can't read: malformed, or made for
// another ordering.
var ErrCursor = errors.New("orm: the cursor isn't one of this query's")

// cursorKey is a column of a keyset ordering.
type cursorKey struct {
	f    *field
	name string // as lookups name it
	desc bool
}

// cursorKeys are order's columns as a keyset ordering, and whether it is
// one: the model's own non-nullable scalar columns, made total with the
// primary key.
func (m *model) cursorKeys(order []sortKey) ([]cursorKey, bool) {
	var keys []cursorKey
	unique := false
	for _, o := range order {
		if o.e != nil || strings.Contains(o.name, "__") {
			return nil, false
		}
		f, ok := m.field(o.name)
		if !ok || f.Via != "" || f.Gen != nil || !keysetType(f) {
			return nil, false
		}
		keys = append(keys, cursorKey{f, f.Name, o.desc})
		unique = unique || f.PK || f.Unique && f.UniqueIdx == ""
	}
	if !unique {
		if m.PK == nil {
			return nil, false
		}
		keys = append(keys, cursorKey{m.PK, m.PK.Name, len(keys) > 0 && keys[len(keys)-1].desc})
	}
	return keys, true
}

// keysetType is whether a column's values order and compare in SQL as in
// a cursor: a non-nullable number, string, bool or time.
func keysetType(f *field) bool {
	if f.Null || f.fk {
		return false
	}
	t := f.Type
	if t == timeType {
		return true
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String, reflect.Bool:
		return true
	}
	return false
}

// orderingTag tells cursors of one ordering from another's.
func orderingTag(keys []cursorKey, keyset bool) uint32 {
	var b strings.Builder
	if keyset {
		for _, k := range keys {
			if k.desc {
				b.WriteByte('-')
			}
			b.WriteString(k.f.Column + ",")
		}
	} else {
		b.WriteString("offset")
	}
	return crc32.ChecksumIEEE([]byte(b.String()))
}

// cursor is a decoded cursor: an ordering's tag, then a row's values (a
// keyset's) or position (an offset's).
type cursor struct {
	Tag    uint32            `json:"t"`
	Values []json.RawMessage `json:"k,omitempty"`
	Pos    int               `json:"o,omitempty"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string, tag uint32) (*cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrCursor
	}
	var c cursor
	if json.Unmarshal(b, &c) != nil || c.Tag != tag {
		return nil, ErrCursor
	}
	return &c, nil
}

// keysetPage is a page past the cursors' rows in the keys' order.
func (qs QuerySet[T]) keysetPage(ctx context.Context, keys []cursorKey, tag uint32, a, b *cursor, first, last int) ([]T, []string, bool, error) {
	conds := []Cond{}
	if a != nil {
		c, err := seek(keys, a, false)
		if err != nil {
			return nil, nil, false, err
		}
		conds = append(conds, c)
	}
	if b != nil {
		c, err := seek(keys, b, true)
		if err != nil {
			return nil, nil, false, err
		}
		conds = append(conds, c)
	}
	backwards := last > 0
	n := first
	if backwards {
		n = last
	}
	order := make([]sortKey, len(keys))
	for i, k := range keys {
		order[i] = sortKey{name: k.name, desc: k.desc != backwards}
	}
	page := qs.Filter(conds...).with(func(q *query) { q.order, q.limit, q.offset = order, n+1, 0 })
	rows, err := page.All(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	more := len(rows) > n
	if more {
		rows = rows[:n]
	}
	if backwards {
		slices.Reverse(rows)
	}
	cursors := make([]string, len(rows))
	for i := range rows {
		v := reflect.ValueOf(&rows[i]).Elem()
		c := cursor{Tag: tag}
		for _, k := range keys {
			raw, err := json.Marshal(peek(v, k.f.Index, k.f.Type).Interface())
			if err != nil {
				return nil, nil, false, fmt.Errorf("orm: a cursor of %s.%s: %w", qs.q.m.Name, k.f.Name, err)
			}
			c.Values = append(c.Values, raw)
		}
		cursors[i] = c.encode()
	}
	return rows, cursors, more, nil
}

// seek is the condition of the rows past c in keys' order (before it when
// back): (k1 > v1) OR (k1 = v1 AND k2 > v2) OR …, < for a descending key.
func seek(keys []cursorKey, c *cursor, back bool) (Cond, error) {
	if len(c.Values) != len(keys) {
		return nil, ErrCursor
	}
	vals := make([]any, len(keys))
	for i, k := range keys {
		p := reflect.New(k.f.Type)
		if err := json.Unmarshal(c.Values[i], p.Interface()); err != nil {
			return nil, ErrCursor
		}
		vals[i] = p.Elem().Interface()
	}
	var alts []Cond
	for i, k := range keys {
		q := Q{}
		for j := range i {
			q[keys[j].name] = vals[j]
		}
		op := "__gt"
		if k.desc != back {
			op = "__lt"
		}
		q[k.name+op] = vals[i]
		alts = append(alts, q)
	}
	return Or(alts...), nil
}

// offsetPage is a page by position: a cursor is a row's place in the
// query's result.
func (qs QuerySet[T]) offsetPage(ctx context.Context, tag uint32, a, b *cursor, first, last int) ([]T, []string, bool, error) {
	start := 0 // the first row past after
	if a != nil {
		start = a.Pos + 1
	}
	end := -1 // the row of before; -1 for none
	if b != nil {
		end = b.Pos
	}
	var from, n int
	more := false
	if last > 0 {
		if end < 0 {
			total, err := qs.Count(ctx)
			if err != nil {
				return nil, nil, false, err
			}
			end = int(total)
		}
		from = max(start, end-last)
		n = end - from
		more = from > start
	} else {
		from, n = start, first
		if end >= 0 {
			n = min(n, end-start)
		}
	}
	if n <= 0 {
		return nil, nil, false, nil
	}
	limit := n
	if last <= 0 {
		limit = n + 1
	}
	rows, err := qs.with(func(q *query) {
		if q.order == nil {
			q.order = keysOf(q.m.meta.Ordering)
		}
		q.limit, q.offset = limit, from
	}).All(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	if last <= 0 {
		more = len(rows) > n && (end < 0 || from+n < end)
		if len(rows) > n {
			rows = rows[:n]
		}
	}
	cursors := make([]string, len(rows))
	for i := range rows {
		cursors[i] = cursor{Tag: tag, Pos: from + i}.encode()
	}
	return rows, cursors, more, nil
}
