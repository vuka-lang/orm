package orm_test

// The drivers ormtest opens when ORMTEST_DRIVER names a real server:
//
//	ORMTEST_DRIVER=postgres ORMTEST_DSN=postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable go test ./...
//	ORMTEST_DRIVER=mysql ORMTEST_DSN='root@tcp(127.0.0.1:53306)/mysql' go test ./...
import (
	_ "github.com/vuka-lang/orm/v2/mysql"
	_ "github.com/vuka-lang/orm/v2/postgres"
)
