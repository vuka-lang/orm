package vet

import (
	"context"

	"github.com/vuka-lang/orm/v2"
)

func Save(ctx context.Context, p Post) error {
	p.Title = "saved" // want
	return p.Save(ctx)
}

func SaveFields(ctx context.Context, p Post) error {
	p.Title = "saved" // want
	return p.SaveFields(ctx, "Title")
}

func Refresh(ctx context.Context, p Post) error {
	p.ID = 7 // want
	return p.Refresh(ctx)
}

func Delete(ctx context.Context, p Post) error {
	p.ID = 7 // want
	return p.Delete(ctx)
}

func Load(ctx context.Context, p Post) error {
	p.ID = 7 // want
	return p.Load(ctx, "Tags")
}

func Add(ctx context.Context, p Post, t Tag) error {
	p.ID = 7 // want
	return p.Add(ctx, "Tags", t)
}

func Remove(ctx context.Context, p Post, t Tag) error {
	p.ID = 7 // want
	return p.Remove(ctx, "Tags", t)
}

func Set(ctx context.Context, p Post, t Tag) error {
	p.ID = 7 // want
	return p.Set(ctx, "Tags", t)
}

func Using(ctx context.Context, p Post, sc orm.DBSchema) error {
	p.Title = "copied" // want
	return p.Using(sc).Save(ctx)
}

func BaseSave(ctx context.Context, n Note) error {
	n.Body = "saved" // want
	return n.Save(ctx)
}

func EmbeddedSave(ctx context.Context, e Event) error {
	e.Label, e.At = "saved", e.At.Add(1) // want
	return e.Save(ctx)
}

func CustomDelete(ctx context.Context, c Custom) error {
	c.Name = "gone"
	return c.Delete(ctx)
}

func CustomSave(ctx context.Context, c Custom) error {
	c.Name = "saved" // want
	return c.Save(ctx)
}
