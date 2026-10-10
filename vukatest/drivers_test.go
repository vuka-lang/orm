package vukatest

// The drivers ormtest opens when ORMTEST_DRIVER names a real server:
//
//	ORMTEST_DRIVER=postgres ORMTEST_DSN=postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable vuka test ./vukatest
//	ORMTEST_DRIVER=mysql ORMTEST_DSN='root@tcp(127.0.0.1:53306)/mysql' vuka test ./vukatest
import (
	_ "github.com/vuka-lang/orm/mysql"
	_ "github.com/vuka-lang/orm/postgres"
)
