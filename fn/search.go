package fn

import (
	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/vuka/v2"
)

// SearchVector is fields as a document: orm.Search's.
func SearchVector[T any](fields ...orm.Path[T]) orm.SearchDoc[T] { return orm.Search(fields...) }

// Match holds for the rows whose document matches q (orm.SearchQuery).
func Match[T any](d orm.SearchDoc[T], q orm.TextQuery) vuka.Pred[T] { return d.MatchesQuery(q) }

// SearchRank is how well the document matches q.
func SearchRank[T any](d orm.SearchDoc[T], q orm.TextQuery) orm.OrderedTerm[T, float64] {
	return d.RankQuery(q)
}

// Headline is a field's text with q's matches marked.
func Headline[T any, V ~string](a orm.Arg[T, V], q orm.TextQuery) orm.StringTerm[T, string] {
	return orm.StringTermOf[T, string](orm.Headline(orm.PathName(a), q))
}

// Similarity is the trigram similarity of a field's text and term.
func Similarity[T any, V ~string](a orm.Arg[T, V], term string) orm.OrderedTerm[T, float64] {
	return orm.OrderedTermOf[T, float64](orm.Similarity(orm.PathName(a), term))
}

// L2Distance, CosineDistance and InnerProduct are a vector field's
// distance to v.
func L2Distance[T any](field orm.Arg[T, orm.Vector], v orm.Vector) orm.OrderedTerm[T, float64] {
	return orm.OrderedTermOf[T, float64](orm.L2Distance(orm.PathName(field), v))
}
func CosineDistance[T any](field orm.Arg[T, orm.Vector], v orm.Vector) orm.OrderedTerm[T, float64] {
	return orm.OrderedTermOf[T, float64](orm.CosineDistance(orm.PathName(field), v))
}
func InnerProduct[T any](field orm.Arg[T, orm.Vector], v orm.Vector) orm.OrderedTerm[T, float64] {
	return orm.OrderedTermOf[T, float64](orm.InnerProduct(orm.PathName(field), v))
}

// Fuse is reciprocal-rank fusion of orderings by named terms (hybrid
// search): order by it descending.
//
//	rank := fn.SearchRank(doc, q).As("rank")
//	dist := fn.CosineDistance(Doc.Embedding, v).As("dist")
//	Doc.Objects.Annotate(rank, dist, fn.Fuse(rank.Desc(), dist.Asc()).As("score")).OrderBy(...)
func Fuse[T any](orders ...orm.TermOrder[T]) orm.OrderedTerm[T, float64] {
	names := make([]string, len(orders))
	for i, o := range orders {
		names[i] = o.Name()
	}
	return orm.OrderedTermOf[T, float64](orm.Fuse(names...))
}
