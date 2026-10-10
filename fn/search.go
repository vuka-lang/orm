package fn

import (
	"github.com/vuka-lang/orm"
	"github.com/vuka-lang/vuka"
)

// Doc is fields of T as a document to search (SearchVector).
type Doc[T any] struct{ d orm.Document }

// SearchVector is fields as a document: Match, SearchRank read it.
func SearchVector[T any](fields ...orm.Path[T]) Doc[T] {
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = orm.FieldName(f)
	}
	return Doc[T]{orm.SearchVector(names...)}
}

// Weight, Config and Add are the Document's.
func (d Doc[T]) Weight(w string) Doc[T] { return Doc[T]{d.d.Weight(w)} }
func (d Doc[T]) Config(c string) Doc[T] { return Doc[T]{d.d.Config(c)} }
func (d Doc[T]) Add(o Doc[T]) Doc[T]    { return Doc[T]{d.d.Add(o.d)} }

// Document is the engine's document (for an index or a generated column).
func (d Doc[T]) Document() orm.Document { return d.d }

// Match holds for the rows whose document matches q (orm.SearchQuery).
func Match[T any](d Doc[T], q orm.TextQuery) vuka.Pred[T] { return orm.P[T](orm.Match(d.d, q)) }

// SearchRank is how well the document matches q.
func SearchRank[T any](d Doc[T], q orm.TextQuery) orm.OrderedTerm[T, float64] {
	return orm.OrderedTermOf[T, float64](orm.SearchRank(d.d, q))
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
