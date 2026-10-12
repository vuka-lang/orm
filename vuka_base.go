package orm

import (
	"context"
	"reflect"

	"github.com/vuka-lang/vuka"
)

// Base is a Vuka model's base: Model without its Objects method, so that
// Post.Objects is the static Vuka gives every type embedding it — the
// model's Query. Embed it as orm.Base (Vuka fills in the type):
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
// A Meta written on the embedded field is the model's (unless it has a
// Meta method). Rows have Model's methods: Save, SaveFields, Refresh,
// Delete, Load, Add, Remove, Set, Using.
type Base[Self any] Model[Self]

func (b *Base[Self]) model() *Model[Self] { return (*Model[Self])(b) }

// vukaDeclares marks a type embedding Base as declaring a model: Vuka links
// its package into the program's mains, so the ORM sees the model without an
// import naming it.
func (Base[Self]) vukaDeclares() {}

func (b *Base[Self]) adopt(sc Schema)       { b.model().adopt(sc) }
func (b *Base[Self]) schema() Schema        { return b.model().schema() }
func (*Base[Self]) of() reflect.Type        { return reflect.TypeFor[Self]() }
func (*Base[Self]) register()               { Register[Self]() }
func (b *Base[Self]) Using(sc Schema) *Self { return b.model().Using(sc) }

// Save inserts the row when it is new, else updates it.
func (b *Base[Self]) Save(ctx context.Context) error { return b.model().Save(ctx) }

// SaveFields updates only the fields named, by reference or name.
func (b *Base[Self]) SaveFields(ctx context.Context, fields ...any) error {
	names, err := pathNames(fields)
	if err != nil {
		return err
	}
	return b.model().SaveFields(ctx, names...)
}

// Refresh reads the row again; Delete removes it.
func (b *Base[Self]) Refresh(ctx context.Context) error { return b.model().Refresh(ctx) }
func (b *Base[Self]) Delete(ctx context.Context) error  { return b.model().Delete(ctx) }

// Load loads relations into the row, by reference (Post.Tags), name or
// orm.Prefetch.
func (b *Base[Self]) Load(ctx context.Context, specs ...any) error {
	return b.model().Load(ctx, prefetchSpecs(specs)...)
}

// Add, Remove and Set change the row's many-to-many links, the relation
// named by reference or name. An orm.M2M field has them itself.
func (b *Base[Self]) Add(ctx context.Context, relation any, related ...any) error {
	return b.model().Add(ctx, pathName(relation), related...)
}
func (b *Base[Self]) Remove(ctx context.Context, relation any, related ...any) error {
	return b.model().Remove(ctx, pathName(relation), related...)
}
func (b *Base[Self]) Set(ctx context.Context, relation any, related ...any) error {
	return b.model().Set(ctx, pathName(relation), related...)
}

var baseObjects vuka.Statics

// Base_Objects is the static Objects of every type embedding orm.Base:
// Post.Objects in Vuka, the model's Query over all its rows.
func Base_Objects[Self any]() *vuka.Static[Query[Self]] {
	return vuka.StaticOf[Base[Self]](&baseObjects, func() Query[Self] { return Query[Self]{} })
}
