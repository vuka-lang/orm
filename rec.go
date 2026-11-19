package orm

import "reflect"

// Rec is Model's state without its methods: a model embedding Rec[T] is a
// model as one embedding Model[T] is, but gets Save, Using and the rest
// from methods orm generate writes on it (RowFor(x).Save(ctx)), and
// instantiates nothing for them, where each Model[T] method is compiled
// again for every model and its package grows with them. Vuka embeds it
// for orm.Base.
//
//	type User struct {
//		orm.Rec[User]
//		ID    int64
//		Email string
//	}
//
// A plain Go package uses Model; Rec is for a package whose models'
// generated code is written before it is built.
type Rec[T any] struct {
	_ [0]*T
	rowState
}

// vukaDeclares, vukaSchema and vukaFields mark a type embedding Rec as
// declaring a model, as Model's do.
func (rowState) vukaDeclares()                           {}
func (rowState) vukaSchema(interface{ Schema(*Schema) }) {}
func (rowState) vukaFields()                             {}

// DeclaredFieldAttrs is Model's, found from t: the model an embedded Rec
// is of.
func (*rowState) DeclaredFieldAttrs(t reflect.Type, registered map[string][]any) map[string][]any {
	if all := stateEmbeds(t); len(all) != 1 || all[0].of != t {
		return registered // a struct embedding the model
	}
	return schemaFrom(t, registered).byName
}
