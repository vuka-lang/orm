// Package sqlite links the pure-Go SQLite engine (glebarez/go-sqlite, on
// modernc's) for the ORM: blank-import it to Connect to "sqlite".
//
//	import _ "github.com/vuka-lang/orm/sqlite"
//
//	db, err := orm.Connect("sqlite", "file:shop.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
//
// Kept out of the orm package itself so a Postgres or MySQL program
// doesn't ship the transpiled-C SQLite engine (~5MB of binary) it never
// opens. Don't import it beside another package registering the "sqlite"
// database/sql driver (modernc.org/sqlite).
//
// It also gives SQLite the functions Postgres's pgvector and pg_trgm
// have, computed in Go, unindexed: l2_distance, cosine_distance and
// inner_product of vectors written as text ([1,2,3]), and similarity, the
// trigram similarity of two texts. The ORM's vector and trigram searches
// use them, so they run on SQLite in development and tests.
package sqlite

import (
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	gosqlite "github.com/glebarez/go-sqlite"

	"github.com/vuka-lang/orm"
)

func init() {
	orm.RegisterDriver("sqlite", "sqlite")
	vectors := func(f func(a, b []float64) float64) func(*gosqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		return func(_ *gosqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if args[0] == nil || args[1] == nil {
				return nil, nil
			}
			a, err := vector(args[0])
			if err != nil {
				return nil, err
			}
			b, err := vector(args[1])
			if err != nil {
				return nil, err
			}
			if len(a) != len(b) {
				return nil, fmt.Errorf("different vector dimensions %d and %d", len(a), len(b))
			}
			return f(a, b), nil
		}
	}
	gosqlite.MustRegisterDeterministicScalarFunction("l2_distance", 2, vectors(func(a, b []float64) float64 {
		var s float64
		for i := range a {
			s += (a[i] - b[i]) * (a[i] - b[i])
		}
		return math.Sqrt(s)
	}))
	gosqlite.MustRegisterDeterministicScalarFunction("inner_product", 2, vectors(dot))
	gosqlite.MustRegisterDeterministicScalarFunction("cosine_distance", 2, vectors(func(a, b []float64) float64 {
		return 1 - dot(a, b)/math.Sqrt(dot(a, a)*dot(b, b))
	}))
	gosqlite.MustRegisterDeterministicScalarFunction("similarity", 2, func(_ *gosqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, b := trigrams(fmt.Sprint(text(args[0]))), trigrams(fmt.Sprint(text(args[1])))
		shared := 0
		for t := range a {
			if b[t] {
				shared++
			}
		}
		if len(a)+len(b) == 0 {
			return 0.0, nil
		}
		return float64(shared) / float64(len(a)+len(b)-shared), nil
	})
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func text(v driver.Value) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// vector reads pgvector's text: [1,2,3].
func vector(v driver.Value) ([]float64, error) {
	s, ok := text(v).(string)
	if !ok {
		return nil, fmt.Errorf("%v is not a vector", v)
	}
	var out []float64
	for part := range strings.SplitSeq(strings.Trim(strings.TrimSpace(s), "[]"), ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a vector", s)
		}
		out = append(out, f)
	}
	return out, nil
}

// trigrams is pg_trgm's: each word lowercased, two spaces before and one
// after, cut into its three-character runs.
func trigrams(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r := []rune("  " + w + " ")
		for i := 0; i+3 <= len(r); i++ {
			out[string(r[i:i+3])] = true
		}
	}
	return out
}
