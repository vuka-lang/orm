// Package postgres links the Postgres driver (pgx, through database/sql)
// for the ORM: blank-import it to Connect to "postgres". It also teaches
// the ORM the driver's connection errors and how to put orm.Credentials
// into a DSN.
//
//	import _ "github.com/vuka-lang/orm/v2/postgres"
//
//	db, err := orm.Connect("postgres", "postgres://user:pass@localhost:5432/shop?sslmode=disable")
package postgres

import (
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/vuka-lang/orm/v2"
)

func init() {
	orm.RegisterDriver("postgres", "pgx")
	orm.RegisterDriverHooks("postgres", orm.DriverHooks{
		Credentials: credentials,
		Classify: func(err error) orm.ErrorClass {
			if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
				return orm.ConnError
			}
			if _, ok := errors.AsType[*pgconn.PgError](err); !ok && pgconn.SafeToRetry(err) {
				return orm.ConnError
			}
			return orm.OtherError
		},
	})
}

// credentials puts user and password into a URL or a key=value DSN.
func credentials(dsn, user, password string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.User = url.UserPassword(user, password)
		return u.String(), nil
	}
	quote := func(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }
	return dsn + " user=" + quote(user) + " password=" + quote(password), nil
}
