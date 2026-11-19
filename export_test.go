package orm

import (
	"context"

	"github.com/vuka-lang/orm/v2/migration"
)

// SetReflectOnly ignores the generated scanners while on.
var SetReflectOnly = setReflectOnly

// FakeMigrations takes d through migrations as orm migrate does with
// --fake (target recorded, not run) or, with target "", --fake-initial.
func FakeMigrations(ctx context.Context, d *DB, target string, migrations ...migration.Migration) error {
	ordered, err := migration.Order(migrations)
	if err != nil {
		return err
	}
	r := &migrator{
		ordered:     ordered,
		open:        func(context.Context, string) (*DB, error) { return d, nil },
		log:         func(string, ...any) {},
		fakeInitial: target == "",
	}
	defer r.close()
	if target == "" {
		return r.forwardAll(ctx, nil)
	}
	return r.fake(ctx, ordered[0].DB, target)
}
