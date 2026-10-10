package orm

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Prefetch loads a relation's rows with a query of your own (filtered,
// ordered, selecting or prefetching further), as Django's Prefetch does:
//
//	Users.PrefetchRelated(orm.Prefetch("posts", Posts.Filter(orm.Q{"draft": false}).OrderBy("-id")))
//
// PrefetchRelated("posts") is Prefetch("posts", nil): every related row.
func Prefetch(path string, qs prefetcher) PrefetchSpec {
	return PrefetchSpec{path: path, qs: qs}
}

// PrefetchSpec is a relation PrefetchRelated loads; see Prefetch.
type PrefetchSpec struct {
	path string
	qs   prefetcher
}

// prefetcher is a QuerySet of any model, as Prefetch takes it.
type prefetcher interface{ prefetchQuery() query }

// prefetchChunk is how many keys one IN (…) of a prefetch holds.
const prefetchChunk = 1000

// prefetch loads specs' relations into parents (addressable structs of
// m), each relation in one query per prefetchChunk keys, nested paths
// (posts__comments) on the rows loaded; the rows are loaded from the
// parents' schema s.
func prefetch(ctx context.Context, c conn, m *model, s Schema, parents []reflect.Value, specs []PrefetchSpec) error {
	if len(parents) == 0 {
		return nil
	}
	type step struct {
		custom *query
		nested []PrefetchSpec
	}
	var order []string
	steps := map[string]*step{}
	for _, sp := range specs {
		if err := checkPath(sp.path); err != nil {
			return err
		}
		first, rest, _ := strings.Cut(sp.path, "__")
		s := steps[first]
		if s == nil {
			s = &step{}
			steps[first] = s
			order = append(order, first)
		}
		switch {
		case rest != "":
			s.nested = append(s.nested, PrefetchSpec{path: rest, qs: sp.qs})
		case sp.qs != nil:
			q := sp.qs.prefetchQuery()
			if q.m == nil {
				return fmt.Errorf("orm: PrefetchRelated takes relation names and orm.Prefetch, not %T", sp.qs.(badPrefetch).v)
			}
			s.custom = &q
		}
	}
	for _, name := range order {
		r, ok := m.relation(name)
		if !ok {
			return fmt.Errorf("orm: PrefetchRelated: %s has no relation %q", m.Name, name)
		}
		st := steps[name]
		if err := fetchRelated(ctx, c, m, s, r, parents, st.custom, st.nested); err != nil {
			return err
		}
	}
	return nil
}

func fetchRelated(ctx context.Context, c conn, m *model, s Schema, r *relation, parents []reflect.Value, custom *query, nested []PrefetchSpec) error {
	if err := r.held(m); err != nil {
		return err
	}
	t, _, err := r.ends()
	if err != nil {
		return err
	}
	base := query{m: t}
	if custom != nil {
		if custom.m != t {
			return fmt.Errorf("orm: Prefetch(%q) is a query of %s, not %s", r.Name, custom.m.Name, t.Name)
		}
		base = custom.clone()
		base.limit, base.offset = 0, 0
	}
	base.prefetch = append(base.prefetch, nested...)
	if r.Kind == relElements {
		return fetchElements(ctx, c, s, r, parents, base)
	}
	byKey, err := related(ctx, c, s, r, base, distinctKeys(parents, r.local))
	if err != nil {
		return err
	}
	for _, p := range parents {
		got := byKey[key(p, r.local)]
		switch {
		case !r.one():
			setMany(fieldOf(p, r.Index), r, got)
		case len(got) > 0:
			setOne(fieldOf(p, r.Index), r, got[0])
		}
	}
	return nil
}

// fetchElements loads an elements relation: the keys inside each
// parent's array read in Go, the rows fetched by them, each parent's
// slice set in its array's order.
func fetchElements(ctx context.Context, c conn, s Schema, r *relation, parents []reflect.Value, base query) error {
	elems := make([][]any, len(parents))
	var keys []any
	seen := map[any]bool{}
	for i, p := range parents {
		es, err := r.elementKeys(p)
		if err != nil {
			return err
		}
		elems[i] = es
		for _, e := range es {
			if !seen[e] {
				seen[e] = true
				keys = append(keys, e)
			}
		}
	}
	byKey, err := related(ctx, c, s, r, base, keys)
	if err != nil {
		return err
	}
	for i, p := range parents {
		var got []reflect.Value
		for _, e := range elems[i] {
			got = append(got, byKey[e]...)
		}
		setMany(fieldOf(p, r.Index), r, got)
	}
	return nil
}

// elementKeys is the related keys inside a parent's array column:
// an orm.JSON or orm.CSV value's list, or comma-separated text.
func (r *relation) elementKeys(parent reflect.Value) ([]any, error) {
	_, rf, err := r.ends()
	if err != nil {
		return nil, err
	}
	numeric := intKind(rf.Type)
	v := peek(parent, r.local.Index, r.local.Type)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		f := v.FieldByName("V")
		if !f.IsValid() {
			return nil, fmt.Errorf("orm: %s.%s: elements: %s is no list", r.owner.Name, r.Name, r.local.Name)
		}
		v = f
	}
	one := func(e reflect.Value) (any, error) {
		for e.Kind() == reflect.Pointer || e.Kind() == reflect.Interface {
			if e.IsNil() {
				return nil, nil
			}
			e = e.Elem()
		}
		switch {
		case intKind(e.Type()):
			if e.CanInt() {
				return e.Int(), nil
			}
			return int64(e.Uint()), nil
		case e.Kind() == reflect.Float64 && numeric: // a JSON[[]any] number
			return int64(e.Float()), nil
		case e.Kind() == reflect.String && numeric:
			n, err := strconv.ParseInt(strings.TrimSpace(e.String()), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("orm: %s.%s holds %q, not a key of %s", r.owner.Name, r.local.Name, e.String(), r.Target.Name())
			}
			return n, nil
		case e.Kind() == reflect.String:
			return e.String(), nil
		}
		return nil, fmt.Errorf("orm: %s.%s: elements of %s can't be keys", r.owner.Name, r.local.Name, e.Type())
	}
	switch v.Kind() {
	case reflect.String:
		var out []any
		for part := range strings.SplitSeq(v.String(), ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, err := one(reflect.ValueOf(part))
			if err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, v.Len())
		for i := range v.Len() {
			k, err := one(v.Index(i))
			if err != nil {
				return nil, err
			}
			if k != nil {
				out = append(out, k)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("orm: %s.%s: elements: %s holds %s, not a list or comma-separated text", r.owner.Name, r.Name, r.local.Name, v.Type())
}

// related is a relation's rows (pointers, loaded from schema s), read by
// base, a query of the related model, by the key of the parent holding
// them: keys, values of the parents' r.local.
func related(ctx context.Context, c conn, s Schema, r *relation, base query, keys []any) (map[any][]reflect.Value, error) {
	_, rf, err := r.ends()
	if err != nil {
		return nil, err
	}
	var pairs [][2]any
	if r.Kind == relM2M {
		if pairs, err = throughPairs(ctx, c, r, keys); err != nil {
			return nil, err
		}
		keys = nil
		seen := map[any]bool{}
		for _, pr := range pairs {
			if !seen[pr[1]] {
				seen[pr[1]] = true
				keys = append(keys, pr[1])
			}
		}
	}
	out := map[any][]reflect.Value{}
	for chunk := range slices.Chunk(keys, prefetchChunk) {
		q := base.clone()
		q.where = append(q.where, Q{rf.Name + "__in": chunk})
		rows, err := q.all(ctx, c, s)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			k := key(row.Elem(), rf)
			out[k] = append(out[k], row)
		}
	}
	if r.Kind != relM2M {
		return out, nil
	}
	byLocal := map[any][]reflect.Value{}
	for _, pr := range pairs {
		byLocal[pr[0]] = append(byLocal[pr[0]], out[pr[1]]...)
	}
	return byLocal, nil
}

// throughPairs is the (local, remote) keys of a many-to-many table for
// the local keys.
func throughPairs(ctx context.Context, c conn, r *relation, locals []any) ([][2]any, error) {
	var out [][2]any
	for chunk := range slices.Chunk(locals, prefetchChunk) {
		b := &builder{d: c.d, st: &stmt{}, joins: &[]joinClause{}}
		marks := make([]string, len(chunk))
		for i, k := range chunk {
			marks[i] = b.arg(k)
		}
		local, remote := c.d.Quote(r.ThroughLocal), c.d.Quote(r.ThroughRemote)
		s := "SELECT " + local + ", " + remote + " FROM " + c.d.Quote(r.Through) + " WHERE " + local + " IN (" + strings.Join(marks, ", ") + ")"
		rows, err := c.query(ctx, s, b.args())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l, rm any
			if err := rows.Scan(&l, &rm); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, [2]any{normKey(l), normKey(rm)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// distinctKeys is the non-null values of f over rows, once each.
func distinctKeys(rows []reflect.Value, f *field) []any {
	var out []any
	seen := map[any]bool{}
	for _, row := range rows {
		k := key(row, f)
		if k == nil || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// key is f of row as a map key both sides of a relation agree on.
func key(row reflect.Value, f *field) any { return normKey(value(peek(row, f.Index, f.Type))) }

// normKey makes keys of different Go types compare: integers as int64,
// bytes as strings, pointers by what they point to.
func normKey(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case []byte:
		return string(x)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return normKey(rv.Elem().Interface())
	}
	if rv.Kind() >= reflect.Int && rv.Kind() <= reflect.Int64 {
		return rv.Int()
	}
	if rv.Kind() == reflect.String {
		return rv.String()
	}
	return v
}

// setOne puts a related row (a pointer) into a field holding one.
func setOne(dst reflect.Value, r *relation, row reflect.Value) {
	if r.Ptr {
		dst.Set(row)
	} else {
		dst.Set(row.Elem())
	}
}

// setMany puts related rows (pointers) into a slice field, empty rather
// than nil when there are none.
func setMany(dst reflect.Value, r *relation, rows []reflect.Value) {
	s := reflect.MakeSlice(dst.Type(), len(rows), len(rows)) // set by index: Append allocates per row
	for i, row := range rows {
		if r.Ptr {
			s.Index(i).Set(row)
		} else {
			s.Index(i).Set(row.Elem())
		}
	}
	dst.Set(s)
}
