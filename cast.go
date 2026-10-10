package orm

import (
	"fmt"
	"regexp"
	"strconv"
)

// CastType is a type Cast converts to, spelled for each database.
type CastType struct {
	name                    string
	postgres, mysql, sqlite string // "" falls back to name
	sqliteFn                string // SQLite converts dates by function, not CAST
}

// The types Cast converts to. AsDecimal and AsType make others.
var (
	AsInt      = CastType{name: "BIGINT", mysql: "SIGNED", sqlite: "INTEGER"}
	AsFloat    = CastType{name: "DOUBLE PRECISION", mysql: "DOUBLE", sqlite: "REAL"}
	AsText     = CastType{name: "TEXT", mysql: "CHAR"}
	AsDate     = CastType{name: "DATE", sqliteFn: "date"}
	AsDateTime = CastType{name: "TIMESTAMP", mysql: "DATETIME(6)", sqliteFn: "datetime"}
)

// AsDecimal is a fixed-point number of precision digits, scale of them
// after the point (NUMERIC on SQLite, which keeps no scale).
func AsDecimal(precision, scale int) CastType {
	t := "(" + strconv.Itoa(precision) + ", " + strconv.Itoa(scale) + ")"
	return CastType{name: "NUMERIC" + t, mysql: "DECIMAL" + t, sqlite: "NUMERIC"}
}

// sqlTypeRE is what AsType takes: a type name, with a size or precision.
var sqlTypeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*(\(\s*\d+\s*(,\s*\d+\s*)?\))?$`)

// AsType is a database type written as the database spells it, the same
// on every database: AsType("CHAR(20)"), AsType("UUID"). It takes a type
// name and its size only, never anything else. MySQL's CAST knows few
// types (CHAR, SIGNED, UNSIGNED, DECIMAL, DOUBLE, DATE, DATETIME, TIME,
// JSON, BINARY): no VARCHAR, no INT.
func AsType(sqlType string) CastType {
	return CastType{name: sqlType}
}

func (t CastType) spelled(dialect string) string {
	s := t.name
	switch dialect {
	case "postgres":
		if t.postgres != "" {
			s = t.postgres
		}
	case "mysql":
		if t.mysql != "" {
			s = t.mysql
		}
	case "sqlite":
		if t.sqlite != "" {
			s = t.sqlite
		}
	}
	return s
}

// Cast converts v (a field, an expression, or a value) to type t:
//
//	Users.Annotate("age_text", orm.Cast(orm.F("age"), orm.AsText)).
//		Filter(orm.Q{"age_text__startswith": "2"})
//	Orders.Filter(orm.Where(orm.Cast(orm.F("created_at"), orm.AsDate), "exact", day))
//	orm.Cast("12.5", orm.AsDecimal(10, 2))
func Cast(v any, t CastType) Expr { return castExpr{v, t} }

type castExpr struct {
	v any
	t CastType
}

func (c castExpr) exprSQL(b *builder) (string, error) {
	if c.t.name == "" {
		return "", fmt.Errorf("orm: Cast to no type")
	}
	if !sqlTypeRE.MatchString(c.t.name) {
		return "", fmt.Errorf("orm: Cast to %q: a type is a name and its size, such as VARCHAR(20)", c.t.name)
	}
	s, err := b.operand(c.v)
	if err != nil {
		return "", err
	}
	if b.d.Name() == "sqlite" && c.t.sqliteFn != "" {
		return c.t.sqliteFn + "(" + s + ")", nil
	}
	return "CAST(" + s + " AS " + c.t.spelled(b.d.Name()) + ")", nil
}
