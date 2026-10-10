// Package mysql links the MySQL driver (go-sql-driver/mysql) for the ORM:
// blank-import it to Connect to "mysql". Connect reads the DSN as
// orm.MySQLDSN does: parseTime on, which the ORM's time fields need, and
// utf8mb4 in the server's default collation unless the DSN names one.
//
//	import _ "github.com/vuka-lang/orm/mysql"
//
//	db, err := orm.Connect("mysql", "user:pass@tcp(localhost:3306)/shop?charset=utf8mb4")
package mysql

import (
	_ "github.com/go-sql-driver/mysql"

	"github.com/vuka-lang/orm"
)

func init() { orm.RegisterDriver("mysql", "mysql") }
