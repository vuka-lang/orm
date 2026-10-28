package orm

import (
	"context"
	"reflect"
)

// Base is a Vuka model's base: Model without its Objects method, so that
// Post.Objects is the static Vuka gives every type embedding it, the
// model's manager (Objects[Post]()). Embed it as orm.Base (Vuka fills in
// the type):
//
//	type Post struct {
//		orm.Base @orm.Meta{Table: "posts", Ordering: []string{"-published"}}
//		ID    int64
//		Title string @orm.Char{Max: 200}
//	}
//
//	posts := Post.Objects.Filter(Post.Title.Contains("go")).All(ctx)?
//	p.Save(ctx)?
//
// The attributes become the model's Schema method; Post.Title is the
// generated typed field PostColumns.Title. Rows have Model's methods: Save,
// SaveFields, Refresh, Delete, Load, Add, Remove, Set, Using.
type Base[Self any] Model[Self]

func (b *Base[Self]) model() *Model[Self] { return (*Model[Self])(b) }

// vukaDeclares marks a type embedding Base as declaring a model: Vuka links
// its package into the program's mains, so the ORM sees the model without an
// import naming it.
func (Base[Self]) vukaDeclares() {}

// vukaSchema has Vuka write a type embedding Base's attributes into its
// Schema method, and vukaFields its field references as the typed fields
// orm generate writes, <Model>Fields.
func (Base[Self]) vukaSchema(interface{ Schema(*Schema) }) {}
func (Base[Self]) vukaFields()                             {}

// vukaPlain names what plain Go embeds in Base's place: Vuka writes an
// embedded orm.Base as orm.Model[Self], whose methods are Base's.
func (Base[Self]) vukaPlain(Model[Self]) {}

func (b *Base[Self]) adopt(sc DBSchema)       { b.model().adopt(sc) }
func (b *Base[Self]) schema() DBSchema        { return b.model().schema() }
func (*Base[Self]) of() reflect.Type          { return reflect.TypeFor[Self]() }
func (*Base[Self]) register()                 { Register[Self]() }
func (b *Base[Self]) Using(sc DBSchema) *Self { return b.model().Using(sc) }

// Save inserts the row when it is new, else updates it.
func (b *Base[Self]) Save(ctx context.Context) error { return b.model().Save(ctx) }

// SaveFields updates only the fields named: typed fields
// (PostColumns.Title), references or names.
func (b *Base[Self]) SaveFields(ctx context.Context, fields ...any) error {
	return b.model().SaveFields(ctx, fields...)
}

// Refresh reads the row again; Delete removes it.
func (b *Base[Self]) Refresh(ctx context.Context) error { return b.model().Refresh(ctx) }
func (b *Base[Self]) Delete(ctx context.Context) error  { return b.model().Delete(ctx) }

// Load loads relations into the row, by reference (Post.Tags), name or
// orm.Prefetch.
func (b *Base[Self]) Load(ctx context.Context, specs ...any) error {
	return b.model().Load(ctx, specs...)
}

// Add, Remove and Set change the row's many-to-many links, the relation
// named by reference or name. An orm.M2M field has them itself.
func (b *Base[Self]) Add(ctx context.Context, relation any, related ...any) error {
	return b.model().Add(ctx, relation, related...)
}
func (b *Base[Self]) Remove(ctx context.Context, relation any, related ...any) error {
	return b.model().Remove(ctx, relation, related...)
}
func (b *Base[Self]) Set(ctx context.Context, relation any, related ...any) error {
	return b.model().Set(ctx, relation, related...)
}
