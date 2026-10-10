// Package mysql links the MySQL driver (go-sql-driver/mysql) for the ORM:
// blank-import it to Connect to "mysql". Connect reads the DSN as
// orm.MySQLDSN sets it: parseTime on, which the ORM's time fields need,
// and utf8mb4 in the server's default collation. It also teaches the ORM
// the driver's errors (deadlocks to retry, dropped connections) and how
// to put orm.Credentials into a DSN.
//
//	import _ "github.com/vuka-lang/orm/mysql"
//
//	db, err := orm.Connect("mysql", "user:pass@tcp(localhost:3306)/shop")
package mysql

import (
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/vuka-lang/orm"
)

func init() {
	orm.RegisterDriver("mysql", "mysql")
	orm.RegisterDriverHooks("mysql", orm.DriverHooks{
		Credentials: func(dsn, user, password string) (string, error) {
			cfg, err := mysql.ParseDSN(dsn)
			if err != nil {
				return "", err
			}
			cfg.User, cfg.Passwd = user, password
			return cfg.FormatDSN(), nil
		},
		Classify: func(err error) orm.ErrorClass {
			if errors.Is(err, mysql.ErrInvalidConn) {
				return orm.ConnError
			}
			if me, ok := errors.AsType[*mysql.MySQLError](err); ok {
				switch me.Number {
				case 1213, 1205: // deadlock, lock wait timeout
					return orm.RetryableError
				case 1053, 2006, 2013: // server shutdown, gone away, lost during query
					return orm.ConnError
				}
			}
			return orm.OtherError
		},
	})
}
