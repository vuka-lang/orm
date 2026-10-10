package orm

import (
	"fmt"
	"testing"
)

type pgCoded struct{ msg, code string }

func (e pgCoded) Error() string    { return e.msg }
func (e pgCoded) SQLState() string { return e.code }

type sqliteCoded struct {
	msg  string
	code int
}

func (e sqliteCoded) Error() string { return e.msg }
func (e sqliteCoded) Code() int     { return e.code }

// A violation is read from the driver's code, not from words a value
// from the request may put in the message.
func TestViolationFromCode(t *testing.T) {
	for _, c := range []struct {
		d    Dialect
		err  error
		kind violation
		col  string
	}{
		{postgres{}, pgCoded{`ERROR: invalid input syntax for type uuid: "23505 duplicate key" (SQLSTATE 22P02)`, "22P02"}, noViolation, ""},
		{postgres{}, fmt.Errorf("wrapped: %w", pgCoded{`ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE 23505)`, "23505"}), uniqueViolation, "users_email_key"},
		{postgres{}, pgCoded{`ERROR: insert or update on table "posts" violates foreign key constraint "posts_author_id_fkey" (SQLSTATE 23503)`, "23503"}, foreignKeyViolation, "posts_author_id_fkey"},
		{mysql{}, errString(`Error 1292 (22007): Incorrect datetime value: '1062 Duplicate entry' for column 'ref' at row 1`), noViolation, ""},
		{mysql{}, errString(`Error 1366 (HY000): Incorrect integer value: '1452' for column 'age' at row 1`), noViolation, ""},
		{mysql{}, errString(`Error 1062 (23000): Duplicate entry 'x' for key 'age' for key 'users.email'`), uniqueViolation, "email"},
		{mysql{}, errString(`Error 1452 (23000): Cannot add or update a child row: a foreign key constraint fails`), foreignKeyViolation, ""},
		{sqlite{}, sqliteCoded{`constraint failed: UNIQUE constraint failed: users.email (2067)`, 2067}, uniqueViolation, "email"},
		{sqlite{}, sqliteCoded{`constraint failed: CHECK constraint failed: UNIQUE constraint failed (275)`, 275}, noViolation, ""},
		{sqlite{}, sqliteCoded{`constraint failed: FOREIGN KEY constraint failed (787)`, 787}, foreignKeyViolation, ""},
	} {
		kind, col := c.d.Violation(c.err)
		if kind != c.kind || col != c.col {
			t.Errorf("%s %q: %v %q, want %v %q", c.d.Name(), c.err, kind, col, c.kind, c.col)
		}
	}
}
