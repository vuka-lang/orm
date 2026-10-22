package orm_test

import (
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/vuka-lang/orm/v2"
)

// TestReflectionFallback runs the suite again with the generated scanners
// ignored: a program without orm generate's files behaves the same.
func TestReflectionFallback(t *testing.T) {
	if os.Getenv("ORM_REFLECT") != "" {
		t.Skip("this is the run")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	cmd := exec.Command(os.Args[0], "-test.skip", "^TestReflectionFallback$")
	cmd.Env = append(os.Environ(), "ORM_REFLECT=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the suite without generated scanners: %v\n%s", err, out)
	}
}

// TestReflectionScansMatch reads and writes the same rows through the
// generated scanners and by reflection.
func TestReflectionScansMatch(t *testing.T) {
	if !orm.Generated[User]() || !orm.Generated[Post]() {
		t.Skip("no generated scanners")
	}
	ctx := open(t)
	seed(t, ctx)
	read := func() ([]User, []Post) {
		t.Helper()
		us, err := Users.OrderBy("id").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ps, err := Posts.OrderBy("id").All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return us, ps
	}
	write := func(title string) {
		t.Helper()
		if err := Posts.BulkCreate(ctx, []*Post{{Title: title, AuthorID: 1}, {Title: title + "!", AuthorID: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	write("gen")
	gu, gp := read()
	orm.SetReflectOnly(true)
	t.Cleanup(func() { orm.SetReflectOnly(false) })
	if orm.Generated[User]() {
		t.Fatal("still generated")
	}
	ru, rp := read()
	if !reflect.DeepEqual(gu, ru) || !reflect.DeepEqual(gp, rp) {
		t.Fatalf("generated %+v %+v\nreflection %+v %+v", gu, gp, ru, rp)
	}
	write("reflect")
	_, rp = read()
	if len(rp) != 4 || rp[2].Title != "reflect" || rp[3].AuthorID != 2 {
		t.Fatalf("written by reflection: %+v", rp)
	}
}
