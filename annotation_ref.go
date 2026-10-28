package orm

import "fmt"

// AnnotationValue is a typed name for a query annotation. Annotation
// constructs one; ModelAnnotation[T] is the model-term interface.
type AnnotationValue[T any] struct {
	TypedExpr[T]
	name  string
	value Expr
}

// Annotation names an expression and returns its typed filter/order handle.
func Annotation[T any](name string, e Expr) AnnotationValue[T] {
	return AnnotationValue[T]{Typed[T](F(name)), name, e}
}

// Ref refers to an annotation already added to the query.
func Ref[T any](name string) AnnotationValue[T]              { return Annotation[T](name, nil) }
func (v AnnotationValue[T]) namedAnnotation() (string, Expr) { return v.name, v.value }
func (v AnnotationValue[T]) Gt(x T) Cond                     { return Where(v, "gt", x) }
func (v AnnotationValue[T]) Gte(x T) Cond                    { return Where(v, "gte", x) }
func (v AnnotationValue[T]) Lt(x T) Cond                     { return Where(v, "lt", x) }
func (v AnnotationValue[T]) Lte(x T) Cond                    { return Where(v, "lte", x) }
func (v AnnotationValue[T]) Between(a, b T) Cond             { return Where(v, "range", []T{a, b}) }
func (v AnnotationValue[T]) Asc() ExprOrder                  { return Asc(v) }
func (v AnnotationValue[T]) Desc() ExprOrder                 { return Desc(v) }
func (v AnnotationValue[T]) exprSQL(b *builder) (string, error) {
	if _, ok := b.ann[v.name]; !ok {
		return "", fmt.Errorf("orm: unknown annotation %q: %w", v.name, b.noField(v.name))
	}
	return b.ref(v.name)
}

type namedAnnotation interface{ namedAnnotation() (string, Expr) }

func annotationArgs(name any, es []Expr) (string, Expr) {
	if v, ok := name.(namedAnnotation); ok && len(es) == 0 {
		n, e := v.namedAnnotation()
		if e != nil {
			return n, e
		}
		return n, invalidAnnotation{}
	}
	if n, ok := name.(string); ok && len(es) == 1 {
		return n, es[0]
	}
	return "", invalidAnnotation{}
}

type invalidAnnotation struct{}

func (invalidAnnotation) exprSQL(*builder) (string, error) {
	return "", fmt.Errorf("orm: Annotate needs a name and expression, or an Annotation handle")
}
