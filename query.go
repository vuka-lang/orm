package orm

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// query is a QuerySet without its Go type: what prefetching runs for a
// related model, and what QuerySet[T] runs for its own.
type query struct {
	m        *model
	where    []Cond
	order    []sortKey
	limit    int
	offset   int
	distinct bool
	ann      []annotation
	related  []string
	prefetch []PrefetchSpec
	every    bool // Unfiltered: an Update or Delete may touch every row
	groupBy  []string
	having   []Cond
	// Values' grouping, decided as it builds: grouped (an aggregate is
	// listed, or GroupBy), the names it reads, and the GROUP BY of
	// positions the implicit rule groups by.
	grouped bool
	values  []string
	group   string
	// settings is what the query's reads SET LOCAL first, on Postgres
	// (EfSearch, Probes).
	settings []string
}

type annotation struct {
	name string
	expr Expr
}

// clone is q to change apart: its lists clipped, so an append copies them
// rather than writing past their end into what q shares (nothing writes
// their items in place).
func (q query) clone() query {
	q.where = slices.Clip(q.where)
	q.order = slices.Clip(q.order)
	q.ann = slices.Clip(q.ann)
	q.related = slices.Clip(q.related)
	q.prefetch = slices.Clip(q.prefetch)
	q.groupBy = slices.Clip(q.groupBy)
	q.having = slices.Clip(q.having)
	q.settings = slices.Clip(q.settings)
	return q
}

// builder is a statement of the query on dialect d.
func (q query) builder(d Dialect) *builder {
	b := newBuilder(d, q.m)
	if len(q.ann) > 0 {
		b.ann = make(map[string]Expr, len(q.ann))
		for _, a := range q.ann {
			b.ann[a.name] = a.expr
		}
	}
	return b
}

// whereSQL is the WHERE clause, empty when there is none. A condition on
// an aggregate is HAVING's, which only a SELECT of rows has.
func (q query) whereSQL(b *builder) (string, error) {
	where, having := q.split(b)
	if len(having) > 0 || len(q.having) > 0 {
		return "", fmt.Errorf("orm: a condition on an aggregate filters groups, and this reads rows: read the groups with Values(…).All, or Having on a grouped query")
	}
	return clause(b, " WHERE ", where)
}

// split is q's conditions: those of the rows (WHERE), and those naming an
// aggregate annotation (HAVING). A Q's keys and an And's members split
// apart, as Django splits them; an Or or a Not naming an aggregate can't,
// and goes to HAVING whole.
func (q query) split(b *builder) (where, having []Cond) {
	if len(b.ann) == 0 {
		return q.where, nil
	}
	for _, c := range q.where {
		w, h := b.partition(c)
		where, having = append(where, w...), append(having, h...)
	}
	return where, having
}

func (b *builder) partition(c Cond) (where, having []Cond) {
	switch c := c.(type) {
	case Q:
		wq, hq := Q{}, Q{}
		for k, v := range c {
			if isAggregate(b.ann[strings.SplitN(k, "__", 2)[0]]) {
				hq[k] = v
			} else {
				wq[k] = v
			}
		}
		if len(wq) > 0 {
			where = append(where, wq)
		}
		if len(hq) > 0 {
			having = append(having, hq)
		}
		return where, having
	case group:
		if c.op == "AND" {
			for _, m := range c.conds {
				w, h := b.partition(m)
				where, having = append(where, w...), append(having, h...)
			}
			return where, having
		}
	}
	if b.aggregated(c) {
		return nil, []Cond{c}
	}
	return []Cond{c}, nil
}

func clause(b *builder, kw string, conds []Cond) (string, error) {
	if len(conds) == 0 {
		return "", nil
	}
	s, err := And(conds...).sql(b)
	if err != nil || s == "" {
		return "", err
	}
	return kw + s, nil
}

// ordered is q in the model's Meta Ordering when it sets no order of its
// own (OrderBy() with no field sets none, Meta's included): rows read for
// their own sake, never counts, aggregates or subqueries.
func (q query) ordered() query {
	if q.order == nil && q.m != nil {
		q.order = keysOf(q.m.meta.Ordering)
	}
	return q
}

func (q query) orderSQL(b *builder) (string, error) {
	if len(q.order) == 0 {
		return "", nil
	}
	parts := make([]string, len(q.order))
	for i, o := range q.order {
		dir := " ASC"
		if o.desc {
			dir = " DESC"
		}
		var col string
		var err error
		if o.e != nil {
			if col, err = o.e.exprSQL(b); err != nil {
				return "", fmt.Errorf("orm: %s can't be ordered by an expression: %w", q.m.Name, err)
			}
		} else if col, err = b.ref(o.name); err != nil {
			return "", fmt.Errorf("orm: %s can't be ordered by %q: %w", q.m.Name, o.name, err)
		}
		parts[i] = col + dir
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

func (q query) pageSQL(b *builder) string {
	s := ""
	switch {
	case q.limit > 0:
		s = " LIMIT " + strconv.Itoa(q.limit)
	case q.offset > 0 && b.d.NoLimit() != "":
		s = " LIMIT " + b.d.NoLimit()
	}
	if q.offset > 0 {
		s += " OFFSET " + strconv.Itoa(q.offset)
	}
	return s
}

// selectSQL is the SELECT of cols, then the query's conditions, order and
// page: written in that order, so the arguments are; the FROM, with the
// joins they followed, last.
func (q query) selectSQL(b *builder, cols string) (string, error) {
	if !q.grouped && q.groupBy == nil && len(q.values) == 0 {
		for _, a := range q.ann {
			if isAggregate(a.expr) {
				q.grouped = true
				for _, f := range q.m.readFields() {
					q.groupBy = append(q.groupBy, f.Name)
				}
				break
			}
		}
	}
	at := len(b.st.args)
	where, having := q.split(b)
	having = append(having, q.having...)
	if len(having) > 0 && !q.grouped {
		return "", fmt.Errorf("orm: Having filters groups, and this query has none: list an aggregate in Values, or GroupBy")
	}
	if err := q.checkGrouped(b, having); err != nil {
		return "", err
	}
	w, err := clause(b, " WHERE ", where)
	if err != nil {
		return "", err
	}
	g, err := q.groupSQL(b)
	if err != nil {
		return "", err
	}
	h, err := clause(b, " HAVING ", having)
	if err != nil {
		return "", err
	}
	o, err := q.orderSQL(b)
	if err != nil {
		return "", err
	}
	from, err := b.from(at)
	if err != nil {
		return "", err
	}
	d := ""
	if q.distinct {
		d = "DISTINCT "
	}
	return "SELECT " + d + cols + " FROM " + from + w + g + h + o + q.pageSQL(b), nil
}

// groupSQL is the GROUP BY: GroupBy's names (a name Values reads by its
// position), else the positions the implicit rule groups by.
func (q query) groupSQL(b *builder) (string, error) {
	if q.groupBy == nil {
		return q.group, nil
	}
	parts := make([]string, len(q.groupBy))
	for i, name := range q.groupBy {
		if at := slices.Index(q.values, name); at >= 0 {
			parts[i] = strconv.Itoa(at + 1)
			continue
		}
		s, err := b.ref(name)
		if err != nil {
			return "", fmt.Errorf("orm: GroupBy(%q): %w", name, err)
		}
		parts[i] = s
	}
	return " GROUP BY " + strings.Join(parts, ", "), nil
}

// checkGrouped refuses, under the implicit grouping, what the grouping
// folds away: an order or a HAVING condition naming a column neither
// grouped nor aggregated — with the rule, instead of the database's
// error. GroupBy is the developer's own: the database judges it (a
// primary key carries the columns depending on it).
func (q query) checkGrouped(b *builder, having []Cond) error {
	if !q.grouped || q.groupBy != nil {
		return nil
	}
	var by []string
	for _, n := range q.values {
		if !isAggregate(b.ann[n]) {
			by = append(by, n)
		}
	}
	ok := func(name string) bool {
		return isAggregate(b.ann[name]) || slices.Contains(by, name)
	}
	why := "this query groups by (" + strings.Join(by, ", ") + ") because Values lists an aggregate"
	if len(by) == 0 {
		why = "this query is one group because Values lists only aggregates"
	}
	for _, o := range q.order {
		if name := o.name; o.e == nil && !ok(name) {
			return fmt.Errorf("orm: ordered by %q, which the grouping folds away — %s: order by a grouped name or an aggregate, add %q to Values, or GroupBy", name, why, name)
		}
	}
	for _, c := range having {
		for _, name := range condNames(c) {
			if !ok(name) {
				return fmt.Errorf("orm: the HAVING condition on %q runs after grouping, where %q no longer exists — %s: filter it in its own Filter (it runs before grouping), or add it to Values", name, name, why)
			}
		}
	}
	return nil
}

// condNames is the first step of each name a condition reads.
func condNames(c Cond) []string {
	var out []string
	switch c := c.(type) {
	case Q:
		for k := range c {
			out = append(out, strings.SplitN(k, "__", 2)[0])
		}
	case group:
		for _, m := range c.conds {
			out = append(out, condNames(m)...)
		}
	case not:
		out = condNames(c.c)
	}
	return out
}

type computedField struct {
	ann   string
	index []int
}

// computedFields is the model's computed fields the query annotates, in
// the order of the annotations.
func (q query) computedFields() []computedField {
	var out []computedField
	for _, a := range q.ann {
		if f, ok := q.m.computed[strings.ToLower(a.name)]; ok {
			out = append(out, computedField{a.name, f.Index})
		}
	}
	return out
}

// relPlan is a foreign key SelectRelated reads in the same row: the
// builder of its model, the plan of the model holding it (-1 for the
// queried one), and its relation.
type relPlan struct {
	b      *builder
	parent int
	rel    *relation
}

// relatedPlans is the plans of SelectRelated's paths and of every path
// on their way, parents before children.
func (q query) relatedPlans(b *builder) ([]relPlan, error) {
	paths := slices.Clone(q.related)
	sort.Strings(paths)
	var plans []relPlan
	index := map[string]int{}
	for _, path := range paths {
		if err := checkPath(path); err != nil {
			return nil, err
		}
		cur, parent, prefix := b, -1, ""
		for part := range strings.SplitSeq(path, "__") {
			r, ok := cur.m.relation(part)
			if !ok || !r.one() {
				return nil, fmt.Errorf("orm: SelectRelated(%q): %s has no foreign key or one-to-one %q (PrefetchRelated loads rows held by many)", path, cur.m.Name, part)
			}
			if err := r.held(cur.m); err != nil {
				return nil, err
			}
			prefix = strings.TrimPrefix(prefix+"__"+part, "__")
			if i, ok := index[prefix]; ok {
				cur, parent = plans[i].b, i
				continue
			}
			next, err := cur.follow(r)
			if err != nil {
				return nil, err
			}
			plans = append(plans, relPlan{b: next, parent: parent, rel: r})
			index[prefix] = len(plans) - 1
			cur, parent = next, len(plans)-1
		}
	}
	return plans, nil
}

// columns is the SELECT list reading q's rows: the model's fields, its
// computed annotations, and the fields of the foreign keys it selects.
func (q query) columns(b *builder, plans []relPlan) (string, error) {
	var cols []string
	read := func(b *builder) error {
		if n := b.m.names(b.d); n != nil && n.list != "" && b.alias == b.m.Table {
			cols = append(cols, n.list)
			return nil
		}
		for _, f := range b.m.readFields() {
			col := b.col(f)
			if f.Via != "" {
				var err error
				if col, err = b.ref(f.Via); err != nil {
					return err
				}
			}
			cols = append(cols, col)
		}
		return nil
	}
	if err := read(b); err != nil {
		return "", err
	}
	for _, f := range q.computedFields() {
		col, err := b.ref(f.ann)
		if err != nil {
			return "", err
		}
		cols = append(cols, col+" AS "+b.d.Quote(f.ann))
	}
	for _, p := range plans {
		if err := read(p.b); err != nil {
			return "", err
		}
	}
	return strings.Join(cols, ", "), nil
}

// scan runs the query and reads each row into a value newRow makes (an
// addressable struct), by reflection, related rows included (loaded from
// schema s); each gets the rows until it returns false.
func (q query) scan(ctx context.Context, c conn, s DBSchema, newRow func() reflect.Value, each func(reflect.Value) bool) error {
	b := q.builder(c.d)
	plans, err := q.relatedPlans(b)
	if err != nil {
		return err
	}
	cols, err := q.columns(b, plans)
	if err != nil {
		return err
	}
	sel, err := q.selectSQL(b, cols)
	if err != nil {
		return err
	}
	rows, err := c.query(ctx, sel, b.args())
	if err != nil {
		return err
	}
	defer rows.Close()
	computed := q.computedFields()
	read := q.m.readFields()
	planRead := make([][]*field, len(plans))
	n := len(read) + len(computed)
	for j, p := range plans {
		planRead[j] = p.b.m.readFields()
		n += len(planRead[j])
	}
	cells := make([]cell, n)
	dest := make([]any, n)
	for i := range cells {
		dest[i] = &cells[i]
	}
	tmps := make([]reflect.Value, len(plans))
	for rows.Next() {
		row := newRow()
		i := 0
		for _, f := range read {
			cells[i].dst, cells[i].fast, cells[i].names, cells[i].codec = fieldOf(row, f.Index), f.fast, q.m.Names, f.codec
			i++
		}
		for _, f := range computed {
			cells[i].dst = fieldOf(row, f.index)
			i++
		}
		for j, p := range plans {
			tmps[j] = reflect.New(p.b.m.Type)
			for _, f := range planRead[j] {
				cells[i].dst, cells[i].fast, cells[i].names, cells[i].codec = fieldOf(tmps[j].Elem(), f.Index), f.fast, p.b.m.Names, f.codec
				i++
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		// Children into their parents first, so a related row held by
		// value carries its own related rows.
		for j := len(plans) - 1; j >= 0; j-- {
			p := plans[j]
			if peek(tmps[j].Elem(), p.b.m.PK.Index, p.b.m.PK.Type).IsZero() {
				continue // no related row: the LEFT JOIN found none
			}
			p.b.m.adopt(tmps[j].Elem(), s)
			parent := row
			if p.parent >= 0 {
				parent = tmps[p.parent].Elem()
			}
			dst := fieldOf(parent, p.rel.Index)
			if p.rel.holder {
				dst.Addr().Interface().(holder).hold([]reflect.Value{tmps[j]})
			} else if p.rel.Ptr {
				dst.Set(tmps[j])
			} else {
				dst.Set(tmps[j].Elem())
			}
		}
		if !each(row) {
			return nil
		}
	}
	return rows.Err()
}

// all is the query's rows of its model, as pointers loaded from schema
// s, prefetches done.
func (q query) all(ctx context.Context, c conn, s DBSchema) ([]reflect.Value, error) {
	var out []reflect.Value
	err := q.ordered().scan(ctx, c, s, func() reflect.Value { return reflect.New(q.m.Type).Elem() }, func(v reflect.Value) bool {
		q.m.adopt(v, s)
		out = append(out, v.Addr())
		return true
	})
	if err != nil {
		return nil, err
	}
	if len(q.prefetch) > 0 {
		parents := make([]reflect.Value, len(out))
		for i, p := range out {
			parents[i] = p.Elem()
		}
		if err := prefetch(ctx, c, q.m, s, parents, q.prefetch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// writeWhere is the WHERE of an UPDATE or DELETE: the query's conditions,
// or, when they follow foreign keys, its rows' keys picked by a subquery
// (UPDATE and DELETE can't join portably; MySQL also needs the subquery
// wrapped to read the table it writes).
func (q query) writeWhere(b *builder) (string, error) {
	at := len(b.st.args)
	w, err := q.whereSQL(b)
	if err != nil || len(*b.joins) == 0 {
		return w, err
	}
	pk := q.m.PK
	if pk == nil {
		return "", fmt.Errorf("orm: %s has no primary key to write rows found through a relation", q.m.Name)
	}
	from, err := b.from(at)
	if err != nil {
		return "", err
	}
	inner := "SELECT " + b.col(pk) + " AS orm_pk FROM " + from + w
	return " WHERE " + b.col(pk) + " IN (SELECT orm_pk FROM (" + inner + ") AS orm_rows)", nil
}
