package xormdriver

// 各数据库引擎的 database/sql 驱动注册（blank import）。
// 引擎与 xorm 驱动名的映射见 driver.go：
//
//	sqlite/sqlite3 → "sqlite"（glebarez/go-sqlite，纯 Go）
//	mysql          → "mysql"（go-sql-driver/mysql）
//	postgres       → "pgx"（jackc/pgx/v5 stdlib）
//	mssql          → "mssql"（microsoft/go-mssqldb，同时注册 "sqlserver"）
import (
	_ "github.com/glebarez/go-sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)
