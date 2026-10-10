module github.com/vuka-lang/orm

go 1.27

require (
	github.com/glebarez/go-sqlite v1.21.2
	github.com/go-sql-driver/mysql v1.10.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/vuka-lang/vuka v0.10.0
	github.com/vuka-lang/web v0.4.0
	golang.org/x/tools v0.49.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/a-h/templ v0.3.1020 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vuka-lang/ui v0.5.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	modernc.org/libc v1.22.5 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.23.1 // indirect
)

replace github.com/vuka-lang/web => ../vuka-web-wt-toolbar

replace github.com/vuka-lang/vuka => ../vuka-wt-toolbar

replace github.com/vuka-lang/ui => ../vuka-ui-wt-toolbar
