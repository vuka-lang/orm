package orm

import (
	"context"
	"reflect"
)

// LoadRelated loads relations into rows read already, as PrefetchRelated
// loads them into a query's rows: each relation in one query for all of
// them (per 1000 keys), by typed field (PostColumns.Tags), reference, name
// or orm.Prefetch. The rows are queried on the first one's schema.
//
//	err := orm.LoadRelated(ctx, posts, "author", "tags")
//
// What a GraphQL field holding a relation resolves through: the rows of a
// level of the query, loaded together (graphql.Related).
func LoadRelated[T any](ctx context.Context, rows []*T, specs ...any) error {
	if len(rows) == 0 || len(specs) == 0 {
		return nil
	}
	mm := home[T]()
	if m, err := modelOf(reflect.TypeFor[T](), "", ""); err == nil && m.state != nil {
		mm = managerFor[T](fieldOf(reflect.ValueOf(rows[0]).Elem(), m.state).Addr().Interface().(stater).schema())
	}
	if mm.err != nil {
		return mm.err
	}
	c, err := mm.conn(ctx, false)
	if err != nil {
		return err
	}
	parents := make([]reflect.Value, len(rows))
	for i, r := range rows {
		parents[i] = reflect.ValueOf(r).Elem()
	}
	return prefetch(ctx, c, mm.meta, mm.schema(), parents, appendSpecs(nil, prefetchSpecs(specs)))
}

// LoadRelation is LoadRelated of one relation, by its Go field's name or
// its lookup name: what the code Vuka generates for a GraphQL field of an
// orm model holding a relation passes graphql.Related.
func LoadRelation[T any](ctx context.Context, rows []*T, relation string) error {
	return LoadRelated(ctx, rows, relation)
}
