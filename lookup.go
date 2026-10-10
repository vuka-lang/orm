package orm

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// LookupFunc writes a custom lookup's condition on dialect d: lhs is the
// left side's SQL (a column, through any transforms of its key), rhs
// writes the value — a bound argument's marker, or an Expr's SQL — each
// call binding it once more, so a lookup naming its value twice calls rhs
// twice and one ignoring its value binds nothing. ok is false where the
// lookup has no SQL on d: the query fails, naming the lookup and the
// dialect.
type LookupFunc func(lhs string, rhs func() string, d Dialect) (sql string, ok bool)

// LookupOption shapes a custom lookup.
type LookupOption func(*customLookup)

// LookupValue maps a lookup's value before it is bound (an Expr is never
// passed through it): LikePattern's, or a value of your own.
func LookupValue(fn func(v any) any) LookupOption {
	return func(l *customLookup) { l.value = fn }
}

// LikePattern is a LookupValue making the value a LIKE pattern matching
// it literally, between before and after (% for any run): its own %, _
// and ! escaped by !, so the lookup's SQL ends with ESCAPE '!'.
func LikePattern(before, after string) func(any) any {
	return func(v any) any { return before + likeEscape(fmt.Sprint(v)) + after }
}

type customLookup struct {
	fn    LookupFunc
	value func(any) any
}

var (
	customLookups sync.Map // name → *customLookup
	lookupNameRE  = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
)

// RegisterLookup adds a lookup to Q keys and Where, Django's
// register_lookup: Q{"title__unaccent_icontains": "cafe"},
// orm.Where(e, "unaccent_icontains", "cafe"). Its name is lowercase
// letters, digits and single underscores; registering a name again
// replaces it, and a built-in lookup's name panics. Transforms (a
// function's Transform) go before it as before a built-in lookup:
// Q{"title__lower__unaccent_icontains": "cafe"}.
//
//	orm.RegisterLookup("unaccent_icontains", func(lhs string, rhs func() string, d orm.Dialect) (string, bool) {
//		if d.Name() != "postgres" {
//			return "", false
//		}
//		return "unaccent(" + lhs + ") ILIKE unaccent(" + rhs() + ") ESCAPE '!'", true
//	}, orm.LookupValue(orm.LikePattern("%", "%")))
func RegisterLookup(name string, fn LookupFunc, opts ...LookupOption) {
	if !lookupNameRE.MatchString(name) {
		panic(fmt.Sprintf("orm: lookup name %q: lowercase letters, digits and single underscores", name))
	}
	if lookups[name] {
		panic(fmt.Sprintf("orm: %q is a built-in lookup", name))
	}
	if fn == nil {
		panic(fmt.Sprintf("orm: lookup %q has no LookupFunc", name))
	}
	l := &customLookup{fn: fn}
	for _, o := range opts {
		o(l)
	}
	customLookups.Store(name, l)
}

func init() {
	// like: a LIKE pattern of the caller's own, TextExpr's Like.
	RegisterLookup("like", func(lhs string, rhs func() string, _ Dialect) (string, bool) {
		return lhs + " LIKE " + rhs(), true
	})
}

// isLookup is whether name is a lookup, built in or registered.
func isLookup(name string) bool {
	if lookups[name] {
		return true
	}
	_, ok := customLookups.Load(name)
	return ok
}

// custom is the lookup registered as name: col against v.
func (b *builder) custom(col, name string, v any) (string, error) {
	l, ok := customLookups.Load(name)
	if !ok {
		return "", unknownLookup(name)
	}
	cl := l.(*customLookup)
	if _, isExpr := v.(Expr); !isExpr && cl.value != nil {
		v = cl.value(v)
	}
	var err error
	rhs := func() string {
		s, e := b.operand(v)
		if err == nil {
			err = e
		}
		return s
	}
	s, ok := cl.fn(col, rhs, b.d)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", fmt.Errorf("orm: lookup %q has no SQL on %s", name, b.d.Name())
	}
	return s, nil
}

// lookupNames is every lookup's name and every transform's.
func lookupNames(transforms bool) []string {
	names := slices.Collect(maps.Keys(lookups))
	customLookups.Range(func(k, _ any) bool {
		names = append(names, k.(string))
		return true
	})
	if transforms {
		names = append(names, transformNames()...)
	}
	slices.Sort(names)
	return names
}

func transformNames() []string {
	var out []string
	transforms.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	return out
}

func unknownLookup(name string) error {
	return fmt.Errorf("orm: unknown lookup %q%s", name, didYouMean(name, lookupNames(false)))
}

// didYouMean is a hint naming the candidate closest to name, when one is
// close enough to be a typo of it: ` — did you mean "icontains"?`.
func didYouMean(name string, candidates []string) string {
	best, bestD := "", len(name)/3+2
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(name), c); d < bestD {
			best, bestD = c, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" — did you mean %q?", best)
}

// editDistance is the Levenshtein distance of a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
