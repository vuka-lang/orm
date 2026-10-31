package orm

import "context"

// SQL builds this query and its bound arguments for the context's database.
// It does not execute the query.
func (qs QuerySet[T]) SQL(ctx context.Context) (string, []any, error) {
	return qs.m.selectStatement(ctx, qs.q)
}

func (m *managerCore) selectStatement(ctx context.Context, qq *query) (string, []any, error) {
	c, err := m.conn(ctx, false)
	if err != nil {
		return "", nil, err
	}
	q := qq.ordered()
	b := q.builder(c.d)
	plans, err := q.relatedPlans(b)
	if err != nil {
		return "", nil, err
	}
	cols, err := q.columns(b, plans)
	if err != nil {
		return "", nil, err
	}
	s, err := q.selectSQL(b, cols)
	return s, b.args(), err
}
