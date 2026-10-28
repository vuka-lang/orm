package orm_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vuka-lang/orm/v2"
	"github.com/vuka-lang/orm/v2/ormtest"
	"github.com/vuka-lang/vuka/v2"
)

type Writer struct {
	orm.Model[Writer]
	ID    int64
	Name  string
	Email string
}

func (w *Writer) Schema(s *orm.Schema) {
	s.Model(orm.Meta{Table: "writers", Ordering: []string{"name"}})
	s.Field(&w.Email, orm.Char{Max: 120, Unique: true}, lowered{})
}

// Stamp is embedded: its fields are named through the model that embeds it.
type Stamp struct{ Created time.Time }

type Story struct {
	orm.Model[Story]
	ID     int64
	Title  string
	Slug   string
	Rating vuka.Option[int]
	Writer orm.FK[Writer]
	Stamp
}

func (st *Story) Schema(s *orm.Schema) {
	s.Model(tableIn("tales"))
	s.Field(&st.Title, orm.Char{Max: 12})
	s.Field(&st.Slug, orm.Slug{Unique: true}, orm.Column("url_slug"))
	s.Field(&st.Writer, orm.Rel{OnDelete: orm.Cascade, Related: "stories"})
	s.Field(&st.Created, orm.Datetime{AutoNowAdd: true})
}

// Every attribute is a value of its minimal spelling.
var (
	_ = []orm.FieldAttr{orm.PK{}, orm.Char{}, orm.Int{}, orm.DateField{}, orm.Datetime{}, orm.LongText{}, orm.Slug{},
		orm.Unique{}, orm.UniqueTogether("u"), orm.Column("c"), orm.DBType("jsonb"), orm.Null{}, orm.Skip{}, orm.AutoNow{},
		orm.AutoNowAdd{}, orm.Rel{}, orm.DefaultTo(0), orm.DefaultSQL("CURRENT_TIMESTAMP"), orm.Elements{}, orm.Index{},
		orm.FieldOptions{}, orm.Dims(3), orm.HNSW{}, orm.IVFFlat{}, orm.Trigram{}, orm.FullText{}, orm.SearchOf{}}
	_ = []orm.ModelAttr{orm.Meta{}}
)

// lowered is a field attribute of a package of the app's own.
type lowered struct{}

func (lowered) Apply(o *orm.FieldOptions) { o.Lower, o.Trim = true, true }

func (lowered) Validate(v any) error {
	if !strings.Contains(fmt.Sprint(v), "@") {
		return errors.New("is no email")
	}
	return nil
}

// tableIn is a model attribute of the app's own.
type tableIn string

func (t tableIn) ApplyModel(c *orm.ModelConfig) { c.Table = string(t) }

var (
	Writers = orm.For[Writer]()
	Stories = orm.For[Story]()
)

func schemaOpen(t *testing.T) context.Context {
	t.Helper()
	ctx := ormtest.Open(t, Writers, Stories)
	ada := &Writer{Name: "Ada", Email: "  ADA@Example.com "}
	bob := &Writer{Name: "Bob", Email: "bob@other.org"}
	ormtest.Seed(t, ctx, Writers, bob, ada)
	ormtest.Seed(t, ctx, Stories,
		&Story{Title: "Go at work", Slug: "go-at-work", Writer: orm.FKOf(ada), Rating: vuka.Some(4)},
		&Story{Title: "SQL by hand", Slug: "sql-by-hand", Writer: orm.FKOf(bob)},
		&Story{Title: "Go, gently", Slug: "go-gently", Writer: orm.FKOf(bob), Rating: vuka.Some(5)})
	return ctx
}

func TestSchemaMethod(t *testing.T) {
	if err := orm.Check(Writers, Stories); err != nil {
		t.Fatal(err)
	}
	if got := Stories.TableName(); got != "tales" {
		t.Errorf("a model attribute's table: %s", got)
	}
	if got, want := Stories.Columns(), []string{"id", "title", "url_slug", "rating", "writer_id", "created"}; !slices.Equal(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
	ctx := schemaOpen(t)

	ws, err := Writers.All(ctx)
	if err != nil || len(ws) != 2 || ws[0].Name != "Ada" || ws[0].Email != "ada@example.com" {
		t.Fatalf("Meta's ordering and a third-party attribute: %+v, %v", ws, err)
	}
	st, err := Stories.Get(ctx, orm.Q{"slug": "go-at-work"})
	if err != nil || st.Created.IsZero() {
		t.Fatalf("an embedded field's attribute (AutoNowAdd): %+v, %v", st, err)
	}

	var e *orm.Error
	err = Stories.Create(ctx, &Story{Title: "a title too long", Slug: "long", Writer: orm.FKOf(&ws[0])})
	if !errors.As(err, &e) || e.Kind != orm.Invalid {
		t.Errorf("Char's Max: %v", err)
	}
	err = Writers.Create(ctx, &Writer{Name: "Cy", Email: "nobody"})
	if !errors.As(err, &e) || e.Kind != orm.Invalid || !strings.Contains(fmt.Sprint(e.Fields), "is no email") {
		t.Errorf("a third-party attribute's Validate: %v", err)
	}
	err = Writers.Create(ctx, &Writer{Name: "Ada again", Email: "ADA@example.com"})
	if !errors.Is(err, orm.ErrConflict) {
		t.Errorf("Char's Unique, after lowering: %v", err)
	}

	s, err := orm.SchemaOf(Stories.Info().Type)
	if err != nil || len(s.FieldAttrs("Created")) != 1 || len(s.FieldAttrs("Slug")) != 2 || len(s.ModelAttrs()) != 1 {
		t.Errorf("SchemaOf: %v, %v", s, err)
	}
}

type badPointer struct {
	ID   int64
	Name string
}

var elsewhere string

func (b *badPointer) Schema(s *orm.Schema) { s.Field(&elsewhere, orm.Char{Max: 3}) }

type badValue struct {
	ID   int64
	Name string
}

func (b *badValue) Schema(s *orm.Schema) { s.Field(b.Name, orm.Char{Max: 3}) }

type badAttr struct {
	ID   int64
	Name string
}

func (b *badAttr) Schema(s *orm.Schema) { s.Field(&b.Name, nil) }

type badEmbed struct {
	*Stamp
	ID int64
}

func (b *badEmbed) Schema(s *orm.Schema) { s.Field(&b.Created, orm.AutoNow{}) }

func TestSchemaMethodErrors(t *testing.T) {
	for _, c := range []struct {
		m    orm.AnyManager
		code string
		say  string
	}{
		{orm.For[badPointer](), "E0501", "isn't a field of badPointer"},
		{orm.For[badValue](), "E0501", "is given string"},
		{orm.For[badAttr](), "E0502", "s.Field(&p.Name, …) is given a nil attribute"},
		{orm.For[badEmbed](), "E0503", "panicked"},
	} {
		err := orm.Check(c.m)
		var se *orm.SchemaError
		if !errors.As(err, &se) || se.Code != c.code || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%v, want %s saying %q", err, c.code, c.say)
		}
	}
	if _, err := orm.For[badPointer]().All(context.Background()); err == nil {
		t.Error("a query of a model whose Schema is wrong")
	}
}

// TestTypedFieldsSQL: a query through StoryColumns is the one orm.Q strings
// make, and a field the Schema renames is found by its Go name.
func TestTypedFieldsSQL(t *testing.T) {
	ctx := schemaOpen(t)
	run := func(qs orm.QuerySet[Story]) ([]string, orm.QueryInfo) {
		var last orm.QueryInfo
		rows, err := qs.All(orm.WithObserver(ctx, func(_ context.Context, q orm.QueryInfo) { last = q }))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Title)
		}
		return out, last
	}
	f := StoryColumns
	typed, tq := run(Stories.Filter(f.Writer().Email.IEndsWith("@other.org"), f.Title.IStartsWith("go"), f.Rating.Gte(1)).OrderBy(f.Created.Desc(), f.ID.Asc()))
	plain, pq := run(Stories.Filter(orm.Q{"writer__email__iendswith": "@other.org"}, orm.Q{"title__istartswith": "go"}, orm.Q{"rating__gte": 1}).OrderBy("-created", "id"))
	if tq.SQL != pq.SQL || fmt.Sprint(tq.Args) != fmt.Sprint(pq.Args) {
		t.Errorf("typed:\n%s %v\nplain:\n%s %v", tq.SQL, tq.Args, pq.SQL, pq.Args)
	}
	if !slices.Equal(typed, []string{"Go, gently"}) || !slices.Equal(typed, plain) {
		t.Errorf("rows %v, %v", typed, plain)
	}
	if got, _ := run(Stories.Filter(f.Slug.Eq("sql-by-hand"), f.Rating.IsNull(true))); !slices.Equal(got, []string{"SQL by hand"}) {
		t.Errorf("a renamed column by its Go name: %v", got)
	}
	st, err := Stories.Get(ctx, f.Slug.Eq("go-at-work"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Load(ctx, f.Writer()); err != nil || st.Writer.Related().Name != "Ada" {
		t.Errorf("Load by a typed relation: %v", err)
	}
	if f.Writer().String() != "writer" || f.Writer().Email.Name() != "writer__email" || WriterColumns.Email.Name() != "email" {
		t.Errorf("paths: %s, %s", f.Writer(), f.Writer().Email)
	}
}

func TestTransaction(t *testing.T) {
	ctx := schemaOpen(t)
	stop := errors.New("stop")
	err := orm.Transaction(ctx, func(ctx context.Context) error {
		if _, err := Writers.Filter(WriterColumns.Name.Eq("Bob")).Update(ctx, orm.Set{"name": "Rob"}); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if n, _ := Writers.Filter(WriterColumns.Name.Eq("Rob")).Count(ctx); n != 0 {
		t.Error("not rolled back")
	}
	err = orm.Transaction(ctx, func(ctx context.Context) error {
		_, err := Writers.Filter(WriterColumns.Name.Eq("Bob")).Update(ctx, orm.Set{"name": "Rob"})
		return err
	}, orm.Retry(1))
	if n, _ := Writers.Filter(WriterColumns.Name.Eq("Rob")).Count(ctx); err != nil || n != 1 {
		t.Errorf("not committed: %v", err)
	}
}

// TestDeclaredFieldAttrs is what vuka.FieldAttrsOf reads of a model: its
// registered attributes, then its Schema method's, the model's on the
// embedded Model field.
func TestDeclaredFieldAttrs(t *testing.T) {
	story := reflect.TypeFor[Story]()
	got := new(orm.Model[Story]).DeclaredFieldAttrs(story, nil)
	want := map[string][]any{
		"Model":  {tableIn("tales")},
		"Title":  {orm.Char{Max: 12}},
		"Slug":   {orm.Slug{Unique: true}, orm.Column("url_slug")},
		"Writer": {orm.Rel{OnDelete: orm.Cascade, Related: "stories"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Story's attributes %v, want %v", got, want)
	}
	registered := map[string][]any{"Name": {lowered{}}}
	if got := new(orm.Model[Story]).DeclaredFieldAttrs(reflect.TypeFor[Writer](), registered); !reflect.DeepEqual(got, registered) {
		t.Errorf("another type's attributes %v, want what was registered", got)
	}
	s, err := orm.SchemaOf(story)
	if err != nil || !reflect.DeepEqual(s.FieldAttrs("Title"), want["Title"]) || !reflect.DeepEqual(s.ModelAttrs(), want["Model"]) {
		t.Errorf("SchemaOf(Story): %v, %v, %v", s.FieldAttrs("Title"), s.ModelAttrs(), err)
	}
}
