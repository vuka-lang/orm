//go:build ignore

// Command gen writes a generated app for measuring what the ORM's generics
// cost a build: 40 models in one package and eight packages querying them.
//
//	go run testdata/sizeapp/gen.go <dir> <path to this repository>
//	cd <dir> && go run github.com/vuka-lang/orm/v2/cmd/orm generate ./... && go build ./...
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

var domains = [][]string{
	{"User", "Team", "Membership", "Invite", "Session"},
	{"Customer", "Address", "Order", "OrderLine", "Shipment"},
	{"Post", "Comment", "Tag", "Category", "Revision"},
	{"Product", "Variant", "Price", "Stock", "Supplier"},
	{"Invoice", "Payment", "Refund", "Ledger", "TaxRate"},
	{"Ticket", "Reply", "Agent", "Queue", "Macro"},
	{"Event", "Venue", "Booking", "Speaker", "Sponsor"},
	{"Report", "Metric", "Alert", "Dashboard", "Widget"},
}

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: gen <dir> <orm repository>")
	}
	dir, repo := os.Args[1], os.Args[2]
	repo, err := filepath.Abs(repo)
	if err != nil {
		log.Fatal(err)
	}
	write(dir, "go.mod", fmt.Sprintf(`module example.com/sizeapp

go 1.27

require github.com/vuka-lang/orm/v2 v2.0.0

replace github.com/vuka-lang/orm/v2 => %s
`, repo))
	write(dir, "models/models.go", models())
	var imports, calls strings.Builder
	for i := range domains {
		name := fmt.Sprintf("svc%d", i)
		write(dir, name+"/"+name+".go", service(name, i))
		fmt.Fprintf(&imports, "\t\"example.com/sizeapp/%s\"\n", name)
		fmt.Fprintf(&calls, "\tif err := %s.Run(ctx); err != nil {\n\t\tlog.Fatal(err)\n\t}\n", name)
	}
	write(dir, "main.go", fmt.Sprintf(`package main

import (
	"context"
	"log"

	_ "github.com/vuka-lang/orm/v2/postgres"

%s)

func main() {
	ctx := context.Background()
%s}
`, imports.String(), calls.String()))
}

func write(dir, name, src string) {
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		log.Fatal(err)
	}
}

func models() string {
	var b strings.Builder
	b.WriteString(`package models

import (
	"context"
	"time"

	"github.com/vuka-lang/orm/v2"
)
`)
	for d, names := range domains {
		for i, n := range names {
			fmt.Fprintf(&b, "\ntype %s struct {\n\torm.Model[%s]\n\tID int64\n\tName string\n\tSlug string\n\tActive bool\n\tViews int64\n\tScore float64\n\tCreatedAt time.Time\n", n, n)
			for k := 0; k < (d+i)%6; k++ {
				fmt.Fprintf(&b, "\tExtra%d string\n", k)
			}
			if i > 0 {
				fmt.Fprintf(&b, "\tOwner orm.FK[%s]\n", names[0])
			}
			if i == 2 {
				fmt.Fprintf(&b, "\tPeers orm.M2M[%s]\n", names[1])
			}
			b.WriteString("}\n")
			fmt.Fprintf(&b, `
func (m *%[1]s) Schema(s *orm.Schema) {
	s.Field(&m.Name, orm.Char{Max: 200})
	s.Field(&m.Slug, orm.Slug{Unique: true})
}

// Live is the active rows, newest first.
func Live%[1]s() orm.QuerySet[%[1]s] {
	return orm.Objects[%[1]s]().Filter(%[1]sColumns.Active.Eq(true)).OrderBy(%[1]sColumns.CreatedAt.Desc())
}

// Touch counts a view.
func (m *%[1]s) Touch(ctx context.Context) error {
	m.Views++
	return m.Save(ctx)
}
`, n)
		}
	}
	return b.String()
}

// service queries its own domain's models and the next domain's, so each
// model is used from two packages besides its own.
func service(name string, d int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `package %s

import (
	"context"

	"github.com/vuka-lang/orm/v2"

	"example.com/sizeapp/models"
)

var _ = orm.Q{}
`, name)
	var run strings.Builder
	for _, set := range [][]string{domains[d], domains[(d+1)%len(domains)]} {
		for i, n := range set {
			fmt.Fprintf(&b, `
func List%[1]s(ctx context.Context, q string, n int) ([]models.%[1]s, error) {
	return orm.Objects[models.%[1]s]().Filter(models.%[1]sColumns.Name.Contains(q), models.%[1]sColumns.Active.Eq(true)).
		OrderBy(models.%[1]sColumns.Views.Desc()).Limit(n).All(ctx)
}

func Find%[1]s(ctx context.Context, slug string) (models.%[1]s, error) {
	return orm.Objects[models.%[1]s]().Filter(models.%[1]sColumns.Slug.Eq(slug)).First(ctx)
}

func Get%[1]s(ctx context.Context, id int64) (models.%[1]s, error) {
	return orm.Objects[models.%[1]s]().Get(ctx, models.%[1]sColumns.ID.Eq(id))
}

func Count%[1]s(ctx context.Context) (int64, error) {
	return models.Live%[1]s().Count(ctx)
}

func Exists%[1]s(ctx context.Context, slug string) (bool, error) {
	return orm.Objects[models.%[1]s]().Filter(models.%[1]sColumns.Slug.Eq(slug)).Exists(ctx)
}

func Save%[1]s(ctx context.Context, m *models.%[1]s) error {
	m.Name += "!"
	return m.Save(ctx)
}

func Delete%[1]s(ctx context.Context, m *models.%[1]s) error {
	return m.Delete(ctx)
}

func Names%[1]s(ctx context.Context) ([]string, error) {
	return orm.Objects[models.%[1]s]().Filter(models.%[1]sColumns.Active.Eq(true)).Values[string]("name").All(ctx)
}
`, n)
			if i > 0 {
				fmt.Fprintf(&b, `
func %[1]sOfLive%[2]s(ctx context.Context) ([]models.%[1]s, error) {
	owners := orm.Objects[models.%[2]s]().Filter(models.%[2]sColumns.Active.Eq(true))
	return orm.Objects[models.%[1]s]().Filter(models.%[1]sColumns.Owner().ID.InQuery(owners)).
		SelectRelated(models.%[1]sColumns.Owner()).All(ctx)
}

func OwnerOf%[1]s(ctx context.Context, m models.%[1]s) (*models.%[2]s, error) {
	r := m.Owner.Get(ctx)
	if r.IsErr() {
		return nil, r.Err()
	}
	v := r.Value()
	return &v, nil
}

func %[1]sOwners(ctx context.Context, m *models.%[1]s) ([]models.%[2]s, error) {
	return orm.Related[models.%[2]s](m, "owner").All(ctx)
}
`, n, set[0])
			}
			fmt.Fprintf(&run, "\tif _, err := Count%s(ctx); err != nil {\n\t\treturn err\n\t}\n", n)
		}
	}
	fmt.Fprintf(&b, "\nfunc Run(ctx context.Context) error {\n%s\treturn nil\n}\n", run.String())
	return b.String()
}
