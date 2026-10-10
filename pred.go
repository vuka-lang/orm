package orm

import (
	"fmt"
	"strings"
)

// Pred is a condition as plain data: a field path, an operator and a
// value, or And / Or / Not of predicates. It is the adapter point for a
// layer above the ORM — a language runtime's typed predicates, a filter
// decoded from a request — that builds conditions without knowing the
// ORM's lookups: it hands over a Pred (or anything with a Pred method,
// through PredCond), and the ORM writes it as it writes a Q.
//
//	orm.Pred{Op: orm.OpAnd, Args: []orm.Pred{
//		{Path: []string{"author", "name"}, Op: orm.OpEq, Value: "Ali"},
//		{Op: orm.OpNot, Args: []orm.Pred{{Path: []string{"draft"}, Op: orm.OpEq, Value: true}}},
//	}}
//
// is orm.And(orm.Q{"author__name": "Ali"}, orm.Not(orm.Q{"draft": true})).
type Pred struct {
	Op PredOp
	// Path is the field, after the relations leading to it:
	// {"author", "name"} is author__name. Each part is a Go field name or
	// a column, as a Q's key names them.
	Path  []string
	Value any
	Args  []Pred // OpAnd / OpOr: the operands; OpNot: the one it negates
}

// PredOp is a Pred's operator: a comparison of the field with the value,
// or a combination of predicates.
type PredOp string

const (
	OpEq          PredOp = "eq" // a nil value: IS NULL
	OpNe          PredOp = "ne" // a nil value: IS NOT NULL
	OpLt          PredOp = "lt"
	OpLte         PredOp = "lte"
	OpGt          PredOp = "gt"
	OpGte         PredOp = "gte"
	OpIn          PredOp = "in"    // the value is a slice
	OpRange       PredOp = "range" // the value is a two-element slice or array
	OpIsNull      PredOp = "isnull"
	OpContains    PredOp = "contains"
	OpIContains   PredOp = "icontains"
	OpIExact      PredOp = "iexact"
	OpStartsWith  PredOp = "startswith"
	OpIStartsWith PredOp = "istartswith"
	OpEndsWith    PredOp = "endswith"
	OpIEndsWith   PredOp = "iendswith"
	OpSearch      PredOp = "search"
	OpAnd         PredOp = "and"
	OpOr          PredOp = "or"
	OpNot         PredOp = "not"
)

// Predicate is a condition a layer above the ORM represents its own way
// and converts to a Pred.
type Predicate interface{ Pred() Pred }

// PredCond is p's Pred as a condition.
func PredCond(p Predicate) Cond { return p.Pred() }

// Cond is p written with the ORM's own conditions: what Filter does with
// it.
func (p Pred) Cond() (Cond, error) {
	switch p.Op {
	case OpAnd, OpOr:
		conds := make([]Cond, len(p.Args))
		for i, a := range p.Args {
			c, err := a.Cond()
			if err != nil {
				return nil, err
			}
			conds[i] = c
		}
		if p.Op == OpAnd {
			return And(conds...), nil
		}
		return Or(conds...), nil
	case OpNot:
		if len(p.Args) != 1 {
			return nil, fmt.Errorf("orm: a not predicate negates one predicate, not %d", len(p.Args))
		}
		c, err := p.Args[0].Cond()
		if err != nil {
			return nil, err
		}
		return Not(c), nil
	}
	if len(p.Path) == 0 {
		return nil, fmt.Errorf("orm: a %q predicate names no field", p.Op)
	}
	for _, part := range p.Path {
		if part == "" || strings.Contains(part, "__") {
			return nil, fmt.Errorf("orm: predicate path %q: a part is one field or relation", p.Path)
		}
	}
	key := strings.Join(p.Path, "__")
	switch p.Op {
	case OpEq:
		return Q{key: p.Value}, nil
	case OpNe:
		return Not(Q{key: p.Value}), nil
	case OpLt, OpLte, OpGt, OpGte, OpIn, OpRange, OpIsNull, OpContains, OpIContains, OpIExact,
		OpStartsWith, OpIStartsWith, OpEndsWith, OpIEndsWith, OpSearch:
		return Q{key + "__" + string(p.Op): p.Value}, nil
	}
	return nil, fmt.Errorf("orm: no predicate operator %q", p.Op)
}

func (p Pred) sql(b *builder) (string, error) {
	c, err := p.Cond()
	if err != nil {
		return "", err
	}
	return c.sql(b)
}

// Ordering is an ORDER BY term as plain data, Pred's companion: a field
// path, descending or not.
type Ordering struct {
	Path []string
	Desc bool
}

// String is the term as OrderBy takes it: author__name, or -author__name.
func (o Ordering) String() string {
	s := strings.Join(o.Path, "__")
	if o.Desc {
		return "-" + s
	}
	return s
}

// Orderings is the terms as OrderBy takes them.
//
//	qs.OrderBy(orm.Orderings(orm.Ordering{Path: []string{"age"}, Desc: true})...)
func Orderings(os ...Ordering) []string {
	out := make([]string, len(os))
	for i, o := range os {
		out[i] = o.String()
	}
	return out
}
