package orm

import (
	"reflect"
	"strings"

	"github.com/vuka-lang/vuka"
)

// The search attributes of a Vuka model's fields:
//
//	type Doc struct {
//		orm.Base
//		ID        int64
//		Title     string       @orm.Trigram
//		Body      string       @orm.FullText{Config: "english"}
//		Search    orm.TSVector @orm.SearchOf{Fields: []string{"title:A", "body:B"}, Config: "english"}
//		Embedding orm.Vector   @orm.Dims(3) @orm.HNSW{Metric: orm.Cosine, M: 16}
//	}
//
// They are what Indexes() and Generated() say in Go: indexes made by
// migrations (on Postgres; MySQL a FullText; SQLite none, its searches
// unindexed), and a generated tsvector column of the fields named.
type (
	// Dims is a vector field's dimensions.
	Dims int
	// HNSW and IVFFlat are pgvector indexes of a vector field, serving the
	// distance of their Metric (L2 when not given).
	HNSW struct {
		Metric            Metric
		M, EfConstruction int
	}
	IVFFlat struct {
		Metric Metric
		Lists  int
	}
	// Trigram is a pg_trgm index of a text field, for Similarity and LIKE.
	Trigram struct{}
	// FullText is the field's full-text index (Config: Postgres's text
	// search configuration).
	FullText struct{ Config string }
	// SearchOf makes an orm.TSVector field a generated column holding the
	// fields' document: each "field" or "field:weight" (A–D).
	SearchOf struct {
		Fields []string
		Config string
	}
)

// attrSearch is the indexes f's search attributes declare, and f made
// the generated document its SearchOf names.
func attrSearch(owner reflect.Type, sf reflect.StructField, f *field) []Index {
	var out []Index
	for _, a := range vuka.FieldAttrsOf(owner)[sf.Name] {
		switch x := a.(type) {
		case HNSW:
			ix := HnswIndex(f.Column).M(x.M).EfConstruction(x.EfConstruction)
			if x.Metric != 0 {
				ix = ix.Ops(x.Metric)
			}
			out = append(out, ix)
		case IVFFlat:
			ix := IvfflatIndex(f.Column).Lists(x.Lists)
			if x.Metric != 0 {
				ix = ix.Ops(x.Metric)
			}
			out = append(out, ix)
		case Trigram:
			out = append(out, GinIndex(f.Column).Trigram())
		case FullText:
			out = append(out, FullTextIndex(f.Column).Config(x.Config))
		case SearchOf:
			var doc Document
			for i, spec := range x.Fields {
				name, weight, _ := strings.Cut(spec, ":")
				d := SearchVector(name)
				if weight != "" {
					d = d.Weight(weight)
				}
				if i == 0 {
					doc = d
				} else {
					doc = doc.Add(d)
				}
			}
			if x.Config != "" {
				doc = doc.Config(x.Config)
			}
			f.gen, f.Gen = true, doc
		}
	}
	return out
}

// SearchDoc is fields of T searched as one document: Matches is the
// condition, Rank how well a row matches.
//
//	doc := orm.Search(Post.Title, Post.Body)
//	Post.Objects.Filter(doc.Matches("go web")).OrderBy(doc.Rank("go web").Desc())
type SearchDoc[T any] struct{ d Document }

// Search is the document of fields (a SearchOf field's own fields, or
// any text fields).
func Search[T any](fields ...Path[T]) SearchDoc[T] {
	return SearchDoc[T]{SearchVector(fieldNames(fields)...)}
}

// Weight, Config and Add are the Document's.
func (s SearchDoc[T]) Weight(w string) SearchDoc[T]    { return SearchDoc[T]{s.d.Weight(w)} }
func (s SearchDoc[T]) Config(c string) SearchDoc[T]    { return SearchDoc[T]{s.d.Config(c)} }
func (s SearchDoc[T]) Add(o SearchDoc[T]) SearchDoc[T] { return SearchDoc[T]{s.d.Add(o.d)} }
func (s SearchDoc[T]) Document() Document              { return s.d }

// Matches holds for the rows whose document matches q, a web search
// ("go web", "go -rust", `"exact phrase"`); MatchesQuery for a TextQuery.
func (s SearchDoc[T]) Matches(q string) vuka.Pred[T] { return s.MatchesQuery(SearchQuery(q)) }
func (s SearchDoc[T]) MatchesQuery(q TextQuery) vuka.Pred[T] {
	return P[T](Match(s.d, q))
}

// Rank is how well the document matches q: order by it descending.
func (s SearchDoc[T]) Rank(q string) OrderedTerm[T, float64] { return s.RankQuery(SearchQuery(q)) }
func (s SearchDoc[T]) RankQuery(q TextQuery) OrderedTerm[T, float64] {
	return OrderedTermOf[T, float64](SearchRank(s.d, q))
}
