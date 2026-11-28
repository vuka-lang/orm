package orm_test

import (
	"reflect"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// A manager's Raw reads the same rows whether its columns are the model's
// in order (the generated scanner's), another order or a few of them (a
// cell per column), or no scanner is generated.
func TestRawColumnOrders(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	want, err := Users.OrderBy("id").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, reflectOnly := range []bool{false, true} {
		orm.SetReflectOnly(reflectOnly)
		for _, sql := range []string{
			"SELECT * FROM users ORDER BY id",
			"SELECT id, created_at, updated_at, name, email, age, bio, active FROM users ORDER BY id",
			"SELECT active, bio, age, email, name, updated_at, created_at, id FROM users ORDER BY id",
			"SELECT ID, Created_At, Updated_At, NAME, Email, Age, Bio, Active, 1 AS extra FROM users ORDER BY id",
		} {
			got, err := Users.Raw(sql).All(ctx)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("reflect only %v, %s:\n%+v, %v\nwant %+v", reflectOnly, sql, got, err, want)
			}
		}
		some, err := Users.Raw("SELECT id, name FROM users ORDER BY id").All(ctx)
		if err != nil || len(some) != 4 || some[1].Name != "Neema" || some[1].ID != 2 || some[1].Email != "" {
			t.Fatalf("a few columns: %+v, %v", some, err)
		}
		first, err := Users.Raw("SELECT * FROM users WHERE age > ? ORDER BY id", 30).First(ctx)
		if err != nil || first.Name != "Juma" {
			t.Fatalf("First = %+v, %v", first, err)
		}
	}
	orm.SetReflectOnly(false)
}
