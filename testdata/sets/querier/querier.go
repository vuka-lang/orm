// Package querier queries models through their sets alone.
package querier

import (
	"context"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/testdata/sets/models"
)

func Long(ctx context.Context, author int64) ([]models.Book, error) {
	return models.BookQueries.Filter(orm.Q{"author_id": author}).Exclude(orm.Q{"pages__lt": 100}).
		OrderBy("-pages").Limit(10).All(ctx)
}

func Read(ctx context.Context, q models.BookQuery) (int64, error) {
	b, err := q.First(ctx)
	if err != nil {
		return 0, err
	}
	b.Pages++
	if err := models.BookQueries.Save(ctx, &b); err != nil {
		return 0, err
	}
	for b, err := range q.Iter(ctx) {
		if err != nil {
			return 0, err
		}
		_ = b
	}
	if _, err := models.AuthorQueries.Get(ctx, orm.Q{"id": b.AuthorID}); err != nil {
		return 0, err
	}
	titles, err := models.BookQueries.Values[string]("title").All(ctx)
	_ = titles
	if err := models.AuthorQueries.Create(ctx, &models.Author{Name: "new"}); err != nil {
		return 0, err
	}
	return q.Count(ctx)
}
