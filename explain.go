package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Statement is a query's SQL for one database and the arguments bound to
// its placeholders, built and not run: CountSQL, ExistsSQL, AggregateSQL,
// UpdateSQL and DeleteSQL give the statement Count, Exists, Aggregate,
// Update and Delete would run. Its Explain asks the database for its
// plan.
type Statement struct {
	Text string
	Args []any

	m     modelConn // the model whose database runs it; nil for one made by hand
	write bool
}

type modelConn interface {
	conn(ctx context.Context, write bool) (conn, error)
}

// String is the text, then the arguments in a trailing comment: for
// reading, never for running.
//
//	SELECT … WHERE "views" >= ?
//	-- args: 100
func (s Statement) String() string {
	if len(s.Args) == 0 {
		return s.Text
	}
	out := make([]string, len(s.Args))
	for i, a := range s.Args {
		out[i] = argString(a)
	}
	return s.Text + "\n-- args: " + strings.Join(out, ", ")
}

func argString(a any) string {
	switch v := a.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return fmt.Sprintf("x'%x'", v)
	case time.Time:
		return "'" + v.Format(time.RFC3339Nano) + "'"
	case fmt.Stringer:
		return "'" + strings.ReplaceAll(v.String(), "'", "''") + "'"
	}
	return fmt.Sprint(a)
}

// Plan is a database's plan for a statement: Text as the database writes
// it, JSON with Format(PlanJSON), and the database's dialect name.
type Plan struct {
	Text     string
	JSON     []byte
	Database string
}

func (p Plan) String() string { return p.Text }

// ExplainOption tunes Explain: Analyze, Format.
type ExplainOption interface{ explainOption(*explainConfig) }

type explainConfig struct {
	analyze bool
	format  PlanFormat
}

type explainOpt func(*explainConfig)

func (f explainOpt) explainOption(c *explainConfig) { f(c) }

// Analyze runs the statement to report what it did, not only what the
// database expects: Postgres's EXPLAIN (ANALYZE, BUFFERS), MySQL's
// EXPLAIN ANALYZE. It runs inside a transaction that is always rolled
// back, so an Update or Delete changes nothing.
func Analyze() ExplainOption { return explainOpt(func(c *explainConfig) { c.analyze = true }) }

// PlanFormat is how Explain asks for the plan: PlanText or PlanJSON.
type PlanFormat string

const (
	PlanText PlanFormat = "text"
	PlanJSON PlanFormat = "json"
)

// Format asks for the plan as f: with PlanJSON, Plan.JSON holds it.
func Format(f PlanFormat) ExplainOption {
	return explainOpt(func(c *explainConfig) { c.format = f })
}

// Explain is the database's plan for s, sent with its arguments. A
// Statement made by hand runs on ctx's default database, and with
// Analyze it is rolled back as a write would be.
func (s Statement) Explain(ctx context.Context, opts ...ExplainOption) (Plan, error) {
	cfg := explainConfig{format: PlanText}
	for _, o := range opts {
		o.explainOption(&cfg)
	}
	if s.Text == "" {
		return Plan{}, errors.New("orm: Explain of an empty statement")
	}
	var c conn
	var err error
	if s.m != nil {
		c, err = s.m.conn(ctx, s.write)
	} else {
		c, err = dbConn(ctx, "", true)
	}
	if err != nil {
		return Plan{}, err
	}
	d := c.d.Name()
	text, err := explainSQL(d, cfg, s.write)
	if err != nil {
		return Plan{}, err
	}
	text += s.Text
	var rows [][]string
	var cols []string
	run := func(c conn) error {
		cols, rows, err = readPlan(ctx, c, text, s.Args)
		return err
	}
	if cfg.analyze {
		err = rolledBack(ctx, c, run)
	} else {
		err = run(c)
	}
	if err != nil {
		return Plan{}, err
	}
	return planOf(d, cfg.format, cols, rows)
}

// explainSQL is the EXPLAIN prefix d's database takes for cfg, or why it
// takes none.
func explainSQL(d string, cfg explainConfig, write bool) (string, error) {
	if cfg.format != PlanText && cfg.format != PlanJSON {
		return "", fmt.Errorf("orm: no plan format %q: Format takes orm.PlanText or orm.PlanJSON", cfg.format)
	}
	asJSON := cfg.format == PlanJSON
	switch d {
	case "postgres":
		var o []string
		if cfg.analyze {
			o = append(o, "ANALYZE", "BUFFERS")
		}
		if asJSON {
			o = append(o, "FORMAT JSON")
		}
		if len(o) == 0 {
			return "EXPLAIN ", nil
		}
		return "EXPLAIN (" + strings.Join(o, ", ") + ") ", nil
	case "mysql":
		switch {
		case cfg.analyze && asJSON:
			return "", errors.New("orm: mysql gives EXPLAIN ANALYZE as text only: explain with Analyze() or with Format(orm.PlanJSON), not both")
		case cfg.analyze && write:
			return "", errors.New("orm: mysql runs EXPLAIN ANALYZE for a SELECT only: explain an UPDATE or DELETE without Analyze()")
		case cfg.analyze:
			return "EXPLAIN ANALYZE ", nil
		case asJSON:
			return "EXPLAIN FORMAT=JSON ", nil
		}
		return "EXPLAIN ", nil
	case "sqlite":
		if cfg.analyze {
			return "", errors.New("orm: sqlite has no EXPLAIN ANALYZE: it gives EXPLAIN QUERY PLAN, as text or Format(orm.PlanJSON)")
		}
		return "EXPLAIN QUERY PLAN ", nil
	}
	return "", fmt.Errorf("orm: Explain doesn't know %s", d)
}

// readPlan runs an EXPLAIN and reads every cell as text.
func readPlan(ctx context.Context, c conn, text string, args []any) ([]string, [][]string, error) {
	rs, err := c.query(ctx, text, args)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	var rows [][]string
	for rs.Next() {
		cells := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rs.Scan(dest...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range cells {
			row[i] = cellString(v)
		}
		rows = append(rows, row)
	}
	return cols, rows, rs.Err()
}

func cellString(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(v)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case float64:
		if float64(float32(v)) == v { // a FLOAT column read as a double
			return strconv.FormatFloat(v, 'f', -1, 32)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// rolledBack runs fn in a transaction it rolls back: a new one, or a
// savepoint of the one ctx is in.
func rolledBack(ctx context.Context, c conn, fn func(conn) error) error {
	switch run := c.run.(type) {
	case *sql.DB:
		tx, err := run.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		c.run = tx
		return fn(c)
	case *sql.Tx:
		if _, err := run.ExecContext(ctx, "SAVEPOINT orm_explain"); err != nil {
			return err
		}
		err := fn(c)
		if _, rerr := run.ExecContext(ctx, "ROLLBACK TO SAVEPOINT orm_explain"); rerr != nil && err == nil {
			err = rerr
		}
		return err
	}
	return fmt.Errorf("orm: Analyze needs a transaction, and %T has none", c.run)
}

// planOf reads an EXPLAIN's rows as d's database writes them.
func planOf(d string, format PlanFormat, cols []string, rows [][]string) (Plan, error) {
	p := Plan{Database: d}
	if d == "sqlite" {
		type step struct {
			ID     int    `json:"id"`
			Parent int    `json:"parent"`
			Detail string `json:"detail"`
		}
		steps := make([]step, 0, len(rows))
		for _, r := range rows {
			if len(r) < 4 {
				return Plan{}, fmt.Errorf("orm: sqlite plan row of %d columns", len(r))
			}
			id, _ := strconv.Atoi(r[0])
			parent, _ := strconv.Atoi(r[1])
			steps = append(steps, step{id, parent, r[3]})
		}
		if format == PlanJSON {
			b, err := json.Marshal(steps)
			if err != nil {
				return Plan{}, err
			}
			p.JSON, p.Text = b, string(b)
			return p, nil
		}
		depth := map[int]int{}
		lines := make([]string, len(steps))
		for i, s := range steps {
			depth[s.ID] = depth[s.Parent] + 1
			lines[i] = strings.Repeat("  ", depth[s.ID]-1) + s.Detail
		}
		p.Text = strings.Join(lines, "\n")
		return p, nil
	}
	if format == PlanJSON {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r[0])
		}
		p.JSON = []byte(b.String())
		if !json.Valid(p.JSON) {
			return Plan{}, fmt.Errorf("orm: %s's JSON plan doesn't parse", d)
		}
		p.Text = b.String()
		return p, nil
	}
	if len(cols) == 1 {
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = strings.TrimRight(r[0], "\n")
		}
		p.Text = strings.Join(lines, "\n")
		return p, nil
	}
	p.Text = table(cols, rows)
	return p, nil
}

// table lays rows out under their column names, aligned.
func table(cols []string, rows [][]string) string {
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len(c)
	}
	for _, r := range rows {
		for i, s := range r {
			width[i] = max(width[i], len(s))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		for i, s := range cells {
			if i == len(cells)-1 {
				b.WriteString(s)
				break
			}
			b.WriteString(s + strings.Repeat(" ", width[i]-len(s)+2))
		}
		b.WriteByte('\n')
	}
	line(cols)
	for _, r := range rows {
		line(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// statement is a Statement of m's: a write runs on its primary.
func statement(m modelConn, text string, args []any, write bool) Statement {
	return Statement{Text: text, Args: args, m: m, write: write}
}

// Explain is the database's plan for the query, as SQL builds it.
func (qs QuerySet[T]) Explain(ctx context.Context, opts ...ExplainOption) (Plan, error) {
	return qs.m.explain(ctx, qs.q, opts)
}

func (m *managerCore) explain(ctx context.Context, q *query, opts []ExplainOption) (Plan, error) {
	s, args, err := m.selectStatement(ctx, q)
	if err != nil {
		return Plan{}, err
	}
	return statement(m, s, args, false).Explain(ctx, opts...)
}

// Explain is the database's plan for the query, as SQL builds it.
func (vq Values[T, R]) Explain(ctx context.Context, opts ...ExplainOption) (Plan, error) {
	st, err := vq.statement(ctx)
	if err != nil {
		return Plan{}, err
	}
	return st.Explain(ctx, opts...)
}

func (vq Values[T, R]) statement(ctx context.Context) (Statement, error) {
	return vq.q.m.valuesStatement(ctx, vq.q.q, vq.cols, reflect.TypeFor[R]())
}

func (m *managerCore) valuesStatement(ctx context.Context, q *query, cols []any, rt reflect.Type) (Statement, error) {
	s, args, err := m.valuesSQL(ctx, q, cols, rt)
	if err != nil {
		return Statement{}, err
	}
	return statement(m, s, args, false), nil
}

// CountSQL is the statement Count runs.
func (qs QuerySet[T]) CountSQL(ctx context.Context) (Statement, error) {
	return qs.m.built(ctx, false, qs.q.countSQL)
}

// ExistsSQL is the statement Exists runs.
func (qs QuerySet[T]) ExistsSQL(ctx context.Context) (Statement, error) {
	return qs.m.built(ctx, false, qs.q.existsSQL)
}

// AggregateSQL is the statement Aggregate runs.
func (qs QuerySet[T]) AggregateSQL(ctx context.Context, aggs ...Agg) (Statement, error) {
	return qs.m.aggregateStatement(ctx, qs.q, aggs)
}

func (m *managerCore) aggregateStatement(ctx context.Context, q *query, aggs []Agg) (Statement, error) {
	return m.built(ctx, false, func(d Dialect) (string, []any, error) { return q.aggregateSQL(d, aggs) })
}

// DeleteSQL is the statement Delete runs, refused as Delete refuses it.
func (qs QuerySet[T]) DeleteSQL(ctx context.Context) (Statement, error) {
	return qs.m.built(ctx, true, qs.q.deleteSQL)
}

// UpdateSQL is the statement Update runs, refused as Update refuses it;
// fields marked auto_now are set to the time it is built.
func (qs QuerySet[T]) UpdateSQL(ctx context.Context, values Set) (Statement, error) {
	return qs.m.updateStatement(ctx, qs.q, values)
}

func (m *managerCore) updateStatement(ctx context.Context, q *query, values Set) (Statement, error) {
	return m.built(ctx, true, func(d Dialect) (string, []any, error) {
		s, args, _, err := q.updateSQL(d, values)
		if err == nil && s == "" {
			err = fmt.Errorf("orm: Update of %s sets no field: there is no statement", m.name())
		}
		return s, args, err
	})
}

func (m *managerCore) built(ctx context.Context, write bool, build func(Dialect) (string, []any, error)) (Statement, error) {
	c, err := m.conn(ctx, write)
	if err != nil {
		return Statement{}, err
	}
	s, args, err := build(c.d)
	if err != nil {
		return Statement{}, err
	}
	return statement(m, s, args, write), nil
}
