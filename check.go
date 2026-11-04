package orm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vuka-lang/orm/v2/internal/schema"
)

// RowCheck is a condition every row of a model meets, declared in its
// Checks() method. It is enforced twice: by the ORM before Create, Save
// and BulkCreate write a row (every failing check reported at once, tied
// to its field), and by the database, as a CHECK constraint that
// migrations make, which holds against raw SQL and other programs too.
// Either way the write's error carries the same Violations.
//
//	func (Vacancy) Checks() []orm.RowCheck {
//		return []orm.RowCheck{
//			orm.CheckOf(orm.Q{"close_date__gte": orm.F("open_date")}).
//				Field("close_date").Message("closes before it opens"),
//			orm.CheckOf(orm.Q{"posts__gt": 0}).Field("posts").Message("needs at least one post"),
//			orm.CheckOf(orm.Q{"status__in": []string{"draft", "open", "closed"}}),
//			orm.CheckOf(orm.Q{"employer__isnull": false}).When(orm.Q{"status": "open"}),
//		}
//	}
//
// A condition is written as a query's filter (Q, lookups, F, expressions,
// Or, Not) of the row's own columns: no relations, no subqueries. The ORM
// evaluates in Go the lookups with an exact Go meaning (exact, iexact,
// gt, gte, lt, lte, in, isnull, range, contains, startswith, endswith and
// their i forms; F comparisons; And, Or, Not) and leaves the rest, a
// database function, to the database, whose refusal is the same error.
// An Update of a query, Raw and Exec write rows the ORM doesn't load: the
// database alone checks those.
type RowCheck struct {
	cond, when Cond
	field      string
	message    string
	name       string
	on         onDBs
}

// CheckOf is a check of cond.
func CheckOf(cond Cond) RowCheck { return RowCheck{cond: cond} }

// When applies the check only to rows matching cond: the CHECK is NOT
// cond OR the check's condition.
func (c RowCheck) When(cond Cond) RowCheck { c.when = cond; return c }

// Field is the field a failure belongs to, which a form shows it under.
func (c RowCheck) Field(name string) RowCheck { c.field = name; return c }

// Message is a failure's message; a sentence made from the condition
// otherwise ("posts must be greater than 0").
func (c RowCheck) Message(text string) RowCheck { c.message = text; return c }

// Name names the constraint; <table>_<fields>_check otherwise. A check
// of no field (an expression's) must be named.
func (c RowCheck) Name(s string) RowCheck { c.name = s; return c }

// On makes the CHECK only on the databases named ("postgres", "mysql",
// "sqlite"); with none, on no database. The ORM checks rows before
// writing them on every database.
func (c RowCheck) On(databases ...DialectName) RowCheck {
	c.on = onDBs{databases, true}
	return c
}

// condition is the check's condition with its When: NOT when OR cond.
func (c RowCheck) condition() Cond {
	if c.when == nil {
		return c.cond
	}
	return Or(Not(c.when), c.cond)
}

// Violation is a check a row fails: the check's name, the field it
// belongs to ("" for none) and its message.
type Violation struct {
	Check, Field, Message string
}

// Violations is the checks a row fails: all those the ORM found before
// writing it, or the one the database refused it by. A write's error
// carries it, an Invalid Error:
//
//	var bad orm.Violations
//	if errors.As(err, &bad) { … }
//
// It is vuka web's field error: a form shows each message under its
// field, a route answers 422 with them.
type Violations []Violation

// Violated is one field's message, for a model's Validate to return.
//
//	return orm.Violated("permit_no", "the job permit has expired")
func Violated(field, message string) Violations {
	return Violations{{Field: field, Message: message}}
}

func (vs Violations) Error() string {
	msgs := make([]string, len(vs))
	for i, v := range vs {
		msgs[i] = v.Message
	}
	return strings.Join(msgs, "; ")
}

// FieldErrors is the messages by field (several of one field joined),
// as vuka web shows them under a form's fields.
func (vs Violations) FieldErrors() map[string]string {
	out := map[string]string{}
	for _, v := range vs {
		if v.Field == "" {
			continue
		}
		if have := out[v.Field]; have != "" {
			out[v.Field] = have + "; " + v.Message
		} else {
			out[v.Field] = v.Message
		}
	}
	return out
}

// StatusCode is the HTTP status a web route answers with: 422.
func (Violations) StatusCode() int { return 422 }

// PublicMessage is the messages, which a web route may show anyone.
func (vs Violations) PublicMessage() string { return vs.Error() }

// RowValidator is a model's own check of a row about to be written, for
// what a RowCheck can't say: other tables, services, the context. It runs on
// Create, Save and BulkCreate after the model's checks pass, before
// BeforeCreate and BeforeSave; its error stops the write, and Violated
// ties it to a field.
type RowValidator interface {
	Validate(ctx context.Context) error
}

// rule is a declared check as registration makes it: its name, the
// field its failures belong to, its message, its condition as Go tests a
// row and as each database's CHECK writes it.
type rule struct {
	name    string
	field   string
	message string
	test    rowTest
	sql     schema.Dialects
}

func (r rule) violation() Violation {
	return Violation{Check: r.name, Field: r.field, Message: r.message}
}

// constraint is the rule as migrations make it; false where it is made
// on no database.
func (r rule) constraint() (schema.Constraint, bool) {
	return schema.Constraint{Name: r.name, Declared: r.sql}, !r.sql.Zero()
}

// declaredRules is the model's checks as registration makes them: each
// one's condition the row's own columns, of values suiting them, written
// on every database it is made on; their names distinct.
func (m *model) declaredRules() ([]rule, error) {
	var out []rule
	for _, c := range m.checks {
		r, err := m.ruleOf(c)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(out, func(o rule) bool { return o.name == r.name }) {
			return nil, fmt.Errorf("orm: %s declares two checks named %s: name one of them (Name)", m.Name, r.name)
		}
		out = append(out, r)
	}
	return out, nil
}

func (m *model) ruleOf(c RowCheck) (rule, error) {
	name := m.checkName(c)
	if name == "" {
		return rule{}, fmt.Errorf("orm: %s: a check of no field needs a Name, so its name stays the same", m.Name)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("orm: %s's check %s: %s", m.Name, name, fmt.Sprintf(format, args...))
	}
	if err := c.on.check(); err != nil {
		return rule{}, fail("%v", err)
	}
	if c.field != "" {
		if _, ok := m.field(c.field); !ok {
			return rule{}, fail("Field names %s, no field of %s", c.field, m.Name)
		}
	}
	r := rule{name: name, field: c.field, message: c.message}
	if c.cond != nil {
		var err error
		if r.test, err = m.rowTestOf(c.condition(), false); err != nil {
			return rule{}, fail("%v", err)
		}
	}
	if r.test == nil {
		return rule{}, fail("it has no condition")
	}
	dbs := c.on.among(allDBs, allDBs)
	for _, db := range dbs {
		s, own, err := m.rowCond(DialectFor(string(db)), c.condition())
		if err != nil {
			return rule{}, m.cantWrite(name, db, dbs, err)
		}
		if !own {
			return rule{}, fail("its values are bools, numbers, strings or nil, written into its CHECK")
		}
		r.sql = r.sql.Set(string(db), s)
	}
	if r.message == "" {
		r.message = c.sentence()
	}
	if r.message == "" {
		r.message = "fails the check " + name
	}
	return r, nil
}

// checkName is the check's name: Name's, else <table>_<fields>_check of
// the fields its condition reads, "" for none.
func (m *model) checkName(c RowCheck) string {
	if c.name != "" {
		return c.name
	}
	var cols []string
	for _, name := range condFields(c.cond) {
		col := strings.ReplaceAll(name, "__", "_")
		if f, ok := m.field(name); ok {
			col = f.Column
		}
		if !slices.Contains(cols, col) {
			cols = append(cols, col)
		}
	}
	if len(cols) == 0 {
		return ""
	}
	return schema.Ident(m.Table + "_" + strings.Join(cols, "_") + "_check")
}

// condFields is the fields cond's lookups compare, in order: each Q
// key's first part.
func condFields(cond Cond) []string {
	switch c := cond.(type) {
	case Q:
		var out []string
		for _, key := range slices.Sorted(maps.Keys(c)) {
			out = append(out, strings.SplitN(key, "__", 2)[0])
		}
		return out
	case group:
		var out []string
		for _, sub := range c.conds {
			out = append(out, condFields(sub)...)
		}
		return out
	case not:
		return condFields(c.c)
	case Pred:
		pc, _ := c.Cond()
		return condFields(pc)
	case exprCond:
		if f, ok := c.e.(fieldRef); ok {
			return []string{string(f)}
		}
	}
	return nil
}

// cantWrite is the error of a check db can't write: the database, why,
// and the way out, On.
func (m *model) cantWrite(name string, db DialectName, dbs []DialectName, err error) error {
	return fmt.Errorf("orm: %s: %s can't write the check %s: %v.\n  %s    make it only there",
		m.Name, dbTitles[db], name, err, onWithout(dbs, db))
}

// onWithout is the On naming dbs but db.
func onWithout(dbs []DialectName, db DialectName) string {
	var on []string
	for _, d := range dbs {
		if d != db {
			on = append(on, strconv.Quote(string(d)))
		}
	}
	return ".On(" + strings.Join(on, ", ") + ")"
}

// unmanagedRules fails on a check made on SQLite on an unmanaged model's
// table: SQLite adds a CHECK only by making its table again, which the
// ORM never does to a table it doesn't manage.
func (m *model) unmanagedRules() error {
	for _, r := range m.rules {
		if r.sql.SQLite == "" {
			continue
		}
		var dbs []DialectName
		for _, db := range allDBs {
			if r.sql.For(string(db)) != "" {
				dbs = append(dbs, db)
			}
		}
		return fmt.Errorf("orm: %s: SQLite has no CHECK added to a table the ORM doesn't manage (Unmanaged, the check %s).\n  %s    make it only there",
			m.Name, r.name, onWithout(dbs, SQLite))
	}
	return nil
}

// violations is the checks row (new, on a create) fails, written on d.
func (m *model) violations(d Dialect, row reflect.Value, create bool) Violations {
	if len(m.rules) == 0 {
		return nil
	}
	r := &rowEval{m: m, d: d, text: textRulesOf(d), row: row, create: create}
	var out Violations
	for _, rl := range m.rules {
		if rl.test.test(r) == no {
			out = append(out, rl.violation())
		}
	}
	return out
}

// refusedBy is the Violation of the constraint named, which the database
// refused a row by: its declared check's, or one written by hand.
func (m *model) refusedBy(constraint string) Violation {
	if i := slices.IndexFunc(m.rules, func(r rule) bool { return r.name == constraint }); i >= 0 {
		return m.rules[i].violation()
	}
	return Violation{Check: constraint, Message: "fails the check " + constraint}
}

// violated is the Invalid error of vs, the checks a row fails; cause is
// the database's refusal when it was the database's.
func (m *model) violated(vs Violations, cause error) error {
	e := errorf(Invalid, m.Name, "%s is invalid: %s", m.Name, vs.Error())
	for _, v := range vs {
		if v.Field == "" {
			continue
		}
		col := v.Field
		if f, ok := m.field(v.Field); ok {
			col = f.Column
		}
		e.Field(col, v.Message)
	}
	e.Err = vs
	if cause != nil {
		e.Err = errors.Join(vs, cause)
	}
	return e
}

// sentence is what c asks of a row, in words: "posts must be greater
// than 0", "when status is open, employer must be set"; "" when it can't
// be said.
func (c RowCheck) sentence() string {
	s, ok := asks(c.cond, true)
	if !ok || c.when == nil {
		return s
	}
	if w, ok := asks(c.when, false); ok {
		return "when " + w + ", " + s
	}
	return ""
}

// asks is cond in words, each lookup as a requirement ("posts must be
// greater than 0") or, must false, a fact ("posts is greater than 0");
// false for what words can't say: a function, a Not, a transform.
func asks(cond Cond, must bool) (string, bool) {
	var parts []string
	sep := " and "
	switch c := cond.(type) {
	case Q:
		for _, key := range slices.Sorted(maps.Keys(c)) {
			s, ok := asksOf(key, c[key], must)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
	case group:
		if c.op == "OR" {
			sep = " or "
		}
		for _, sub := range c.conds {
			s, ok := asks(sub, must)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
	default:
		return "", false
	}
	return strings.Join(parts, sep), len(parts) > 0
}

func asksOf(key string, v any, must bool) (string, bool) {
	name, lookup, _ := strings.Cut(key, "__")
	if lookup == "" {
		lookup = "exact"
	}
	def, ok := lookups[lookup]
	says := def.says
	switch {
	case !ok:
		return "", false
	case lookup == "isnull":
		says = "be set"
		if isNull, _ := v.(bool); isNull {
			says = "be empty"
		}
	case lookup == "exact" && v == nil:
		says = "be empty"
	case says == "":
		return "", false
	default:
		s, ok := sayValue(lookup, v)
		if !ok {
			return "", false
		}
		says = fmt.Sprintf(says, s)
	}
	if !must {
		return name + " " + present(says), true
	}
	return name + " must " + says, true
}

// sayValue is v in words: a field by its name, a list's items.
func sayValue(lookup string, v any) (string, bool) {
	switch x := v.(type) {
	case fieldRef:
		return string(x), true
	case typedField:
		return x.Name(), true
	case pather:
		return lookupPath(x.FieldPath()), true
	case Expr:
		return "", false
	}
	if lookup != "in" && lookup != "range" {
		return fmt.Sprint(argValue(v)), true
	}
	items, err := list(v)
	if err != nil {
		return "", false
	}
	words := make([]string, len(items))
	for i, x := range items {
		var ok bool
		if words[i], ok = sayValue("", x); !ok {
			return "", false
		}
	}
	if lookup == "range" {
		return strings.Join(words, " and "), true
	}
	return strings.Join(words, ", "), true
}

// present is a requirement's words as a fact: be → is, contain →
// contains.
func present(says string) string {
	if rest, ok := strings.CutPrefix(says, "be "); ok {
		return "is " + rest
	}
	verb, rest, _ := strings.Cut(says, " ")
	return verb + "s " + rest
}
