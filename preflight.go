package orm

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/vuka-lang/orm/v2/migration"
)

// preflightSamples is how many duplicated key values a failed preflight
// shows.
const preflightSamples = 5

// PreflightError is a migration refused before its first statement: the
// rows of its table don't allow its unique index or check.
type PreflightError struct {
	Migration string // the migration's label: name, or db:name
	Check     migration.Preflight
	Dialect   string
	// Values and Rows are a unique check's duplicated key values and the
	// rows holding them; Samples the first of those values, most rows
	// first, each with its row count. Rows alone is a check's rows
	// breaking it.
	Values, Rows int
	Samples      []PreflightSample
}

// PreflightSample is a duplicated key value: its keys, as text, and how
// many rows hold it.
type PreflightSample struct {
	Keys []string
	Rows int
}

func (e *PreflightError) Error() string {
	var b strings.Builder
	if e.Check.Check() {
		verb := "break"
		if e.Rows == 1 {
			verb = "breaks"
		}
		fmt.Fprintf(&b, "orm: migration %s can't %s: %s of %s %s it.\n", e.Migration, e.Check.Op, plural(e.Rows, "row"), e.Check.Table, verb)
		b.WriteString("  Fix those rows first, or change the check")
		if e.Dialect == "postgres" {
			b.WriteString(" (or add it with NotValid: only rows written after are checked, ValidateConstraint checks the others later)")
		}
	} else {
		are := "are"
		if e.Values == 1 {
			are = "is"
		}
		fmt.Fprintf(&b, "orm: migration %s can't %s: %s of its keys %s duplicated in %s of %s:\n",
			e.Migration, e.Check.Op, plural(e.Values, "value"), are, plural(e.Rows, "row"), e.Check.Table)
		for _, s := range e.Samples {
			fmt.Fprintf(&b, "    %s  (%s)\n", strings.Join(s.Keys, ", "), plural(s.Rows, "row"))
		}
		if e.Values > len(e.Samples) {
			fmt.Fprintf(&b, "    … and %d more\n", e.Values-len(e.Samples))
		}
		b.WriteString("  Clean up the duplicates, or make it non-unique")
	}
	b.WriteString(".\n  The migration hasn't been applied: change the model and run orm makemigrations --update to rewrite it (when it's the latest and no one else applied it), or orm migrate --no-preflight to try it anyway.")
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// preflight runs p's query on the session's connection before mg: an
// error when rows break what mg adds.
func (s *session) preflight(ctx context.Context, mg migration.Migration, p migration.Preflight) error {
	fail := &PreflightError{Migration: label(mg), Check: p, Dialect: s.db.dialect.Name()}
	if p.Check() {
		if err := s.c.QueryRowContext(ctx, p.SQL).Scan(&fail.Rows); err != nil {
			return fmt.Errorf("orm: migration %s: checking the rows before it can %s: %w\n%s", label(mg), p.Op, err, p.SQL)
		}
		if fail.Rows == 0 {
			return nil
		}
		return fail
	}
	rows, err := s.c.QueryContext(ctx, p.SQL)
	if err != nil {
		return fmt.Errorf("orm: migration %s: checking the rows before it can %s: %w\n%s", label(mg), p.Op, err, p.SQL)
	}
	defer rows.Close()
	vals := make([]any, len(p.Keys)+1)
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		var n int
		ptrs[len(p.Keys)] = &n
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		fail.Values++
		fail.Rows += n
		if len(fail.Samples) < preflightSamples {
			keys := make([]string, len(p.Keys))
			for i, v := range vals[:len(p.Keys)] {
				keys[i] = sampleValue(v)
			}
			fail.Samples = append(fail.Samples, PreflightSample{Keys: keys, Rows: n})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if fail.Values == 0 {
		return nil
	}
	return fail
}

// sampleValue is a key's value as a failed preflight shows it: a string
// quoted, cut at 60 characters.
func sampleValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		v = string(x)
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	if utf8.RuneCountInString(s) > 60 {
		s = string([]rune(s)[:60]) + "…"
	}
	return fmt.Sprintf("%q", s)
}
