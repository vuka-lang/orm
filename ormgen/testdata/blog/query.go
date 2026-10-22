package blog

import (
	"context"

	"github.com/vuka-lang/orm/v2"
)

// ByDomain is the posts by authors at domain, tagged go or sql, newest first.
func ByDomain(ctx context.Context, domain string) ([]Post, error) {
	f := PostFields
	return orm.For[Post]().
		Filter(f.Author.Email.IEndsWith("@"+domain), f.Tags.Name.In("go", "sql")).
		OrderBy(f.Created.Desc()).
		All(ctx)
}
