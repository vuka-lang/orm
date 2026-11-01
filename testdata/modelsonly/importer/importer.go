// Package importer uses models' rows and runs no query of its own.
package importer

import (
	"context"

	"github.com/vuka-lang/orm/v2/testdata/modelsonly/models"
)

func Rename(ctx context.Context, p *models.Post, title string) (string, error) {
	p.Title = title
	if err := p.Save(ctx); err != nil {
		return "", err
	}
	author, err := p.Author.Get(ctx).Get()
	return author.Name, err
}
