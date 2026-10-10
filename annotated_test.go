package orm_test

import (
	"reflect"
	"testing"

	"github.com/vuka-lang/orm"
)

// A struct field reads the annotation spelt like it but for case and
// underscores.
func TestValuesMatchesAnnotationSpelling(t *testing.T) {
	ctx := relOpen(t) // Ali: 2 books, Neema: 1, Juma: none
	type row struct {
		Name      string
		BookCount int64
	}
	for _, alias := range []string{"bookCount", "book_count", "BookCount"} {
		got, err := Authors.Annotate(alias, orm.Count("books__id")).
			Filter(orm.Q{alias + "__gt": 0}).OrderBy("name").Values[row]().All(ctx)
		if err != nil || !reflect.DeepEqual(got, []row{{"Ali", 2}, {"Neema", 1}}) {
			t.Errorf("%s: %v, %v", alias, got, err)
		}
	}
}
