package migration

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

func TestCreateFunction(t *testing.T) {
	op := CreateFunction{
		Name:    "initials",
		Params:  Dialects{Postgres: "s text", MySQL: "s VARCHAR(255)"},
		Returns: Dialects{Postgres: "text", MySQL: "VARCHAR(255)"},
		Body:    Dialects{Postgres: "SELECT upper(left(s, 1))", MySQL: "RETURN UPPER(LEFT(s, 1))"},
	}
	mg := Migration{Name: "0002_f", Operations: []Operation{op}}
	for _, c := range []struct {
		dialect   string
		fwd, back []string
	}{
		{"postgres", []string{"CREATE OR REPLACE FUNCTION initials(s text) RETURNS text LANGUAGE sql IMMUTABLE AS $orm$ SELECT upper(left(s, 1)) $orm$;"}, []string{"DROP FUNCTION IF EXISTS initials(s text);"}},
		{"mysql", []string{"CREATE FUNCTION initials(s VARCHAR(255)) RETURNS VARCHAR(255) DETERMINISTIC RETURN UPPER(LEFT(s, 1));"}, []string{"DROP FUNCTION IF EXISTS initials;"}},
		{"sqlite", nil, nil},
	} {
		if got := sqlOf(t, mg, c.dialect, NewState(nil, nil), false); !slices.Equal(got, c.fwd) {
			t.Errorf("%s forwards = %q, want %q", c.dialect, got, c.fwd)
		}
		if got := sqlOf(t, mg, c.dialect, NewState(nil, nil), true); !slices.Equal(got, c.back) {
			t.Errorf("%s backwards = %q, want %q", c.dialect, got, c.back)
		}
	}

	// A compound MySQL body runs whole, never split at its semicolons.
	op.Body.MySQL = "BEGIN DECLARE c CHAR(1); SET c = UPPER(LEFT(s, 1)); RETURN c; END"
	op.Volatile = true
	op.Language = "plpgsql"
	op.Body.Postgres = "BEGIN RETURN upper(left(s, 1)); END"
	steps, err := Migration{Name: "x", Operations: []Operation{op}}.Forwards("mysql", NewState(nil, nil))
	if err != nil || len(steps) != 1 || steps[0].Go == nil || !strings.Contains(steps[0].SQL, "NOT DETERMINISTIC BEGIN") {
		t.Fatalf("compound body = %+v, %v", steps, err)
	}
	var ran string
	if err := steps[0].Go(context.Background(), execRecorder{&ran}); err != nil || ran != steps[0].SQL {
		t.Fatalf("ran %q, %v", ran, err)
	}
	if got := sqlOf(t, Migration{Name: "x", Operations: []Operation{op}}, "postgres", NewState(nil, nil), false); !strings.Contains(got[0], "LANGUAGE plpgsql VOLATILE AS $orm$ BEGIN") {
		t.Fatalf("plpgsql = %q", got)
	}

	if src, err := Source("migrations", Migration{Name: "0002_f", Operations: []Operation{op}}, nil); err != nil ||
		!strings.Contains(string(src), `m.CreateFunction{Name: "initials", Params: m.Dialects{`) || !strings.Contains(string(src), `Language: "plpgsql", Volatile: true}`) {
		t.Fatalf("source = %s, %v", src, err)
	}
	if err := (Migration{Name: "bad", Operations: []Operation{CreateFunction{Name: "x; DROP"}}}).Apply(NewState(nil, nil)); err == nil {
		t.Fatal("a bad function name applied")
	}
}

type execRecorder struct{ got *string }

func (r execRecorder) ExecContext(_ context.Context, q string, _ ...any) (sql.Result, error) {
	*r.got = q
	return nil, nil
}
func (execRecorder) QueryContext(context.Context, string, ...any) (*sql.Rows, error) { return nil, nil }
func (execRecorder) QueryRowContext(context.Context, string, ...any) *sql.Row        { return nil }
