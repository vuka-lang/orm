// Package postgres links the Postgres driver (pgx, through database/sql)
// for the ORM: blank-import it to Connect to "postgres".
//
//	import _ "github.com/vuka-lang/orm/postgres"
//
//	db, err := orm.Connect("postgres", "postgres://user:pass@localhost:5432/shop?sslmode=disable")
package postgres

import (
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/vuka-lang/orm"
)

func init() { orm.RegisterDriver("postgres", "pgx") }
