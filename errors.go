package orm

import (
	"errors"
	"fmt"
)

// Kind is what an Error means to the code calling the ORM, whatever the
// database said. A Kind is an error itself, so errors.Is matches an Error
// against its kind:
//
//	if errors.Is(err, orm.ErrNotFound) { … }
//
// A web framework maps kinds onto HTTP statuses, for example:
//
//	Kind           HTTP
//	NotFound       404
//	MultipleFound  409
//	Conflict       409 (Fields: the column → "already exists")
//	Invalid        422 (Fields: the column → "does not exist", or a check's message)
//	Unavailable    503
//	(no Error)     500
type Kind uint8

const (
	_ Kind = iota
	// NotFound: no row matched a query that needs one (Get, First,
	// Refresh, Remove of a row gone).
	NotFound
	// MultipleFound: Get matched more than one row.
	MultipleFound
	// Conflict: a write broke a unique constraint.
	Conflict
	// Invalid: a write referred to a row that does not exist (a foreign
	// key violation), or gave a field a value its checks refuse (Max,
	// Choices, Pattern, a Validate method; Fields: the columns' messages).
	Invalid
	// Unavailable: the database didn't answer (Migrate gave up waiting
	// for it).
	Unavailable
)

// The kinds as error values, for errors.Is.
var (
	ErrNotFound      error = NotFound
	ErrMultipleFound error = MultipleFound
	ErrConflict      error = Conflict
	ErrInvalid       error = Invalid
	ErrUnavailable   error = Unavailable
)

func (k Kind) Error() string {
	switch k {
	case NotFound:
		return "orm: not found"
	case MultipleFound:
		return "orm: multiple found"
	case Conflict:
		return "orm: conflict"
	case Invalid:
		return "orm: invalid"
	case Unavailable:
		return "orm: unavailable"
	}
	return "orm: error"
}

func (k Kind) String() string {
	switch k {
	case NotFound:
		return "NotFound"
	case MultipleFound:
		return "MultipleFound"
	case Conflict:
		return "Conflict"
	case Invalid:
		return "Invalid"
	case Unavailable:
		return "Unavailable"
	}
	return "Kind(" + fmt.Sprint(uint8(k)) + ")"
}

// Error is an error the ORM gives a meaning: its Kind, a message for
// people, the fields (by column) it is about, and the database's error
// under it.
type Error struct {
	Kind    Kind
	Model   string // the model's name, when there is one
	Message string
	// Fields is the messages of the columns the error is about: a
	// Conflict's column "already exists", an Invalid's "does not exist".
	Fields map[string][]string
	Err    error // the cause: the driver's error, when there is one
}

func errorf(kind Kind, model, format string, args ...any) *Error {
	return &Error{Kind: kind, Model: model, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Kind.Error()
}

// Unwrap is the cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches e's kind: errors.Is(err, orm.ErrConflict).
func (e *Error) Is(target error) bool {
	k, ok := target.(Kind)
	return ok && k == e.Kind
}

// Field adds msg to the column's messages.
func (e *Error) Field(column, msg string) *Error {
	if e.Fields == nil {
		e.Fields = map[string][]string{}
	}
	e.Fields[column] = append(e.Fields[column], msg)
	return e
}

// KindOf is the Kind of err: its first *Error's, or a Kind in its chain
// (ErrNotFound and the other kinds as values), 0 when it has neither: an
// error the ORM gives no meaning (a driver's, a bad query).
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	if k, ok := errors.AsType[Kind](err); ok {
		return k
	}
	return 0
}
