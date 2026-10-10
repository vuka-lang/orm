package orm

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Dialect is what differs between the databases the ORM speaks to.
type Dialect interface {
	Name() string
	// Placeholder is the n-th argument's marker, from 1.
	Placeholder(n int) string
	Quote(ident string) string
	// ILike matches col against a LIKE pattern, ignoring case.
	ILike(col, pattern string) string
	// Returning is whether INSERT … RETURNING gives back the new key.
	Returning() bool
	// NoLimit is the LIMIT to put before an OFFSET with no limit; empty
	// when OFFSET may stand alone.
	NoLimit() string
	// Violation reads a constraint error: unique or foreign key, and the
	// column it names when it names one.
	Violation(err error) (kind violation, column string)
}

type violation int

const (
	noViolation violation = iota
	uniqueViolation
	foreignKeyViolation
)

// DialectFor is the dialect of a driver name: "postgres" (or
// "postgresql", "pgx"), "mysql" or "sqlite"; a server version after a
// slash (mysql/9.3.0, as migrations take it) is left out.
func DialectFor(driver string) Dialect {
	driver, _, _ = strings.Cut(driver, "/")
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx":
		return postgres{}
	case "mysql":
		return mysql{}
	}
	return sqlite{}
}

type postgres struct{}

func (postgres) Name() string                     { return "postgres" }
func (postgres) Quote(s string) string            { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (postgres) ILike(col, pattern string) string { return col + " ILIKE " + pattern + ` ESCAPE '!'` }
func (postgres) Returning() bool                  { return true }
func (postgres) NoLimit() string                  { return "" }

func (postgres) Placeholder(n int) string {
	if n > 0 && n <= len(pgMarks) {
		return pgMarks[n-1]
	}
	return "$" + strconv.Itoa(n)
}

// pgMarks is $1…$1024, so a statement's markers cost no allocation.
var pgMarks = func() []string {
	out := make([]string, 1024)
	for i := range out {
		out[i] = "$" + strconv.Itoa(i+1)
	}
	return out
}()

var (
	pgDetail     = regexp.MustCompile(`Key \(([^)]+)\)`)
	pgConstraint = regexp.MustCompile(`constraint "([^"]+)"`)
)

func (postgres) Violation(err error) (violation, string) {
	msg := err.Error()
	col := ""
	if m := pgDetail.FindStringSubmatch(msg); m != nil {
		col = m[1]
	} else if m := pgConstraint.FindStringSubmatch(msg); m != nil {
		col = m[1] // a constraint name, users_email_key: mapErr finds the column in it
	}
	// The driver's SQLSTATE when it gives one (pgx, lib/pq): the message
	// may quote a value from the request (invalid input syntax for type
	// uuid: "…duplicate key…"), which mustn't pass for a violation.
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		switch coded.SQLState() {
		case "23505":
			return uniqueViolation, col
		case "23503":
			return foreignKeyViolation, col
		}
		return noViolation, ""
	}
	switch {
	case strings.Contains(msg, "23505"), strings.Contains(msg, "duplicate key"):
		return uniqueViolation, col
	case strings.Contains(msg, "23503"), strings.Contains(msg, "foreign key"):
		return foreignKeyViolation, col
	}
	return noViolation, ""
}

type mysql struct{}

func (mysql) Name() string           { return "mysql" }
func (mysql) Placeholder(int) string { return "?" }
func (mysql) Quote(s string) string  { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysql) ILike(col, pattern string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + pattern + ") ESCAPE '!'"
}
func (mysql) Returning() bool { return false }
func (mysql) NoLimit() string { return "18446744073709551615" }

// The driver's error is "Error 1062 (23000): Duplicate entry '<value>'
// for key '<table>.<key>'": the code is the first, ahead of any value it
// quotes, and the key the last, after it.
var (
	mysqlCode = regexp.MustCompile(`Error (\d+)\b`)
	mysqlKey  = regexp.MustCompile(`for key '(?:[^.']*\.)?([^']+)'$`)
)

func (mysql) Violation(err error) (violation, string) {
	msg := err.Error()
	code := ""
	if m := mysqlCode.FindStringSubmatch(msg); m != nil {
		code = m[1]
	}
	switch code {
	case "1062":
		col := ""
		if m := mysqlKey.FindStringSubmatch(msg); m != nil {
			col = m[1]
		}
		return uniqueViolation, col
	case "1452", "1451":
		return foreignKeyViolation, ""
	}
	return noViolation, ""
}

type sqlite struct{}

func (sqlite) Name() string           { return "sqlite" }
func (sqlite) Placeholder(int) string { return "?" }
func (sqlite) Quote(s string) string  { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (sqlite) ILike(col, pattern string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + pattern + ") ESCAPE '!'"
}
func (sqlite) Returning() bool { return true }
func (sqlite) NoLimit() string { return "-1" }

var sqliteCol = regexp.MustCompile(`constraint failed: [\w"]+\.(\w+)`)

func (sqlite) Violation(err error) (violation, string) {
	msg := err.Error()
	col := ""
	if m := sqliteCol.FindStringSubmatch(msg); m != nil {
		col = m[1]
	}
	// The driver's extended result code when it gives one.
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() {
		case 2067, 1555: // SQLITE_CONSTRAINT_UNIQUE, _PRIMARYKEY
			return uniqueViolation, col
		case 787: // SQLITE_CONSTRAINT_FOREIGNKEY
			return foreignKeyViolation, ""
		}
		return noViolation, ""
	}
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return uniqueViolation, col
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return foreignKeyViolation, ""
	}
	return noViolation, ""
}

// likeEscape makes s match literally inside a LIKE pattern escaped with
// '!'.
func likeEscape(s string) string { return likeEscaper.Replace(s) }

var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
