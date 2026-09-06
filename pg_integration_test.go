//go:build integration

package xormdriver

import (
	"context"
	"os"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// 本文件为 xorm 驱动的 PostgreSQL 多租户集成测试，依赖真实数据库，默认不运行
// （无 integration tag 时不参与编译）。
// 运行方式：设置环境变量 GOFAST_TEST_PG_DSN 后执行 `go test -tags integration`。
// 示例：
//
//	GOFAST_TEST_PG_DSN="postgres://user:pass@host:5432/db?sslmode=disable" \
//	  go test -tags integration ./database/drivers/xormdriver/

// ── 测试模型 ─────────────────────────────────────────────────────────
//
// xorm SnakeMapper 对表名与列名均为单数下划线推导（"ID" → "i_d"，与 gorm
// NamingStrategy 的复数形式不同），故各模型以 xorm tag 显式指定列名，
// 对齐原生 DDL 的列（id/name）。

// pgUserRow 投影结构体：SnakeMapper 推导表名 pg_user_row，恰为下方诱饵表名，
// 与显式 Table("users") 不一致（回归缺陷同款场景）。
type pgUserRow struct {
	ID   string `xorm:"'id'"`
	Name string `xorm:"'name'"`
}

// pgTenantUser 普通模型：SnakeMapper 推导表名 pg_tenant_user。
type pgTenantUser struct {
	ID   string `xorm:"'id'"`
	Name string `xorm:"'name'"`
}

// pgTablerUser 模拟业务模型实现 xorm names.TableName 接口返回裸表名的场景：
// 引擎对该接口优先于 mapper 推导，链上无 Table()/Model() 时依赖驱动 build()
// 的表名兜底按 dest 推导并经 schemaTable 拼上 schema 前缀。
type pgTablerUser struct {
	ID   string `xorm:"'id'"`
	Name string `xorm:"'name'"`
}

func (pgTablerUser) TableName() string { return "users" }

// ── 测试日志器 ───────────────────────────────────────────────────────

// 编译期断言：noopLog 满足框架日志契约。
var _ contracts.Log = noopLog{}

// noopLog 测试用日志器：实现 contracts.Log 全接口，把 NewXormDriver 桥接的
// xorm 日志（fastLogger 的 SQL 执行/内部日志）重定向到 t.Log——随 `go test -v`
// 输出、失败时自动附带，便于排查集成环境问题。Fatal/Panic 系列同样降级为
// 普通输出：日志层不应直接终止测试进程，连通性等致命错误由 NewXormDriver
// 返回 error 后在测试内显式 t.Fatal。With* 链式方法返回携带同一 t 的新实例
// （值语义，本实现不落任何字段状态）。
type noopLog struct{ t *testing.T }

func (l noopLog) Debug(args ...any)                 { l.t.Log(args...) }
func (l noopLog) Debugf(format string, args ...any) { l.t.Logf(format, args...) }
func (l noopLog) Info(args ...any)                  { l.t.Log(args...) }
func (l noopLog) Infof(format string, args ...any)  { l.t.Logf(format, args...) }
func (l noopLog) Warn(args ...any)                  { l.t.Log(args...) }
func (l noopLog) Warnf(format string, args ...any)  { l.t.Logf(format, args...) }
func (l noopLog) Error(args ...any)                 { l.t.Log(args...) }
func (l noopLog) Errorf(format string, args ...any) { l.t.Logf(format, args...) }
func (l noopLog) Fatal(args ...any)                 { l.t.Log(args...) }
func (l noopLog) Fatalf(format string, args ...any) { l.t.Logf(format, args...) }
func (l noopLog) Panic(args ...any)                 { l.t.Log(args...) }
func (l noopLog) Panicf(format string, args ...any) { l.t.Logf(format, args...) }

func (l noopLog) WithField(_ string, _ any) contracts.Log     { return l }
func (l noopLog) WithFields(_ map[string]any) contracts.Log   { return l }
func (l noopLog) WithError(_ error) contracts.Log             { return l }
func (l noopLog) WithContext(_ context.Context) contracts.Log { return l }

// newPGXorm 读取 GOFAST_TEST_PG_DSN 创建 xorm postgres 驱动实例（pgx stdlib）；
// 未设置环境变量时跳过测试。测试结束统一关闭连接池。
func newPGXorm(t *testing.T) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_PG_DSN 环境变量，跳过 pgsql 集成测试")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver: "xorm",
		Engine: "postgres",
		DSN:    dsn,
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建 xorm postgres 驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// TestPGXorm_ExplicitTableWithProjectionDest 回归缺陷：schema 模式下 build() 曾按
// dest 结构体推导表名无条件覆盖显式 Table()，导致 relation "<schema>.<推导表>"
// does not exist，表恰好存在时静默查错表（缺陷报告 2026-09-05，与 gormdriver
// 同名回归对齐）。
// 自建 schema tenant_pgxorm_reg：users 存正确数据；诱饵表按 pgUserRow 经
// SnakeMapper 的单数推导表名 pg_user_row 命名并存入错误数据——若表名兜底
// 错误覆盖显式 Table()，查询将静默命中诱饵表返回 "wrong-table"。覆盖
// Find/First/Create/Delete 四条路径。
func TestPGXorm_ExplicitTableWithProjectionDest(t *testing.T) {
	drv := newPGXorm(t)
	ten := "tenant_pgxorm_reg"

	if err := drv.Query().Exec("CREATE SCHEMA IF NOT EXISTS " + ten); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	defer func() {
		_ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE")
	}()
	for _, ddl := range []string{
		`CREATE TABLE ` + ten + `.users (id text PRIMARY KEY, name text)`,
		// 诱饵表名 = pgUserRow 的 SnakeMapper 推导表名（单数，非 gorm 的复数形式）
		`CREATE TABLE ` + ten + `.pg_user_row (id text PRIMARY KEY, name text)`,
		`INSERT INTO ` + ten + `.users VALUES ('u1', 'right-table')`,
		`INSERT INTO ` + ten + `.pg_user_row VALUES ('u1', 'wrong-table')`,
	} {
		if err := drv.Query().Exec(ddl); err != nil {
			t.Fatalf("初始化失败 %q: %v", ddl, err)
		}
	}

	// assertCount 原生 SQL 计数（Raw+Scan 链路），断言写操作只命中目标表
	assertCount := func(table string, want int64) {
		t.Helper()
		var n int64
		if err := drv.Query().Raw("SELECT count(*) FROM " + ten + "." + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s 期望 %d 行, 实际 %d", table, want, n)
		}
	}

	// Find：显式 Table 应命中 users 而非 pg_user_row 诱饵表
	var rows []pgUserRow
	if err := drv.Query().Schema(ten).Table("users").
		Select("id", "name").Where("id = ?", "u1").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("Find 应查 users 表, 期望 [right-table], 实际 %+v", rows)
	}

	// First：同上
	var one pgUserRow
	if err := drv.Query().Schema(ten).Table("users").First(&one); err != nil {
		t.Fatalf("First: %v", err)
	}
	if one.Name != "right-table" {
		t.Errorf("First 应查 users 表, 期望 right-table, 实际 %q", one.Name)
	}

	// Create：应写入 users，不动 pg_user_row 诱饵表
	if err := drv.Query().Schema(ten).Table("users").Create(&pgUserRow{ID: "u2", Name: "created"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertCount("users", 2)
	assertCount("pg_user_row", 1)

	// Delete：应删 users 的行，不动 pg_user_row 诱饵表
	if err := drv.Query().Schema(ten).Table("users").Where("id = ?", "u2").Delete(&pgUserRow{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertCount("users", 1)
	assertCount("pg_user_row", 1)
}

// TestPGXorm_TablerFallback 覆盖表名兜底：链上无 Table()/Model() 时，build()
// 按 dest 推导表名并经 schemaTable 拼上 schema 前缀。两种推导来源均应命中
// users：TableName() 接口（pgTablerUser → "users"）与 SnakeMapper 单数推导
// （pgTenantUser → pg_tenant_user）。
func TestPGXorm_TablerFallback(t *testing.T) {
	drv := newPGXorm(t)
	ten := "tenant_pgxorm_tabler"

	if err := drv.Query().Exec("CREATE SCHEMA IF NOT EXISTS " + ten); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	defer func() {
		_ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE")
	}()
	for _, ddl := range []string{
		`CREATE TABLE ` + ten + `.users (id text PRIMARY KEY, name text)`,
		`CREATE TABLE ` + ten + `.pg_tenant_user (id text PRIMARY KEY, name text)`,
		`INSERT INTO ` + ten + `.users VALUES ('t1', 'tabler-fallback')`,
		`INSERT INTO ` + ten + `.pg_tenant_user VALUES ('m1', 'mapper-fallback')`,
	} {
		if err := drv.Query().Exec(ddl); err != nil {
			t.Fatalf("初始化失败 %q: %v", ddl, err)
		}
	}

	// TableName() 裸表名模型：无 Table/Model 直接 Schema+Find，兜底命中 users
	var rows []pgTablerUser
	if err := drv.Query().Schema(ten).Find(&rows); err != nil {
		t.Fatalf("Tabler Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "tabler-fallback" {
		t.Errorf("Tabler 兜底应命中 users 表, 期望 [tabler-fallback], 实际 %+v", rows)
	}

	// SnakeMapper 推导模型：同样走兜底命中 pg_tenant_user
	var models []pgTenantUser
	if err := drv.Query().Schema(ten).Find(&models); err != nil {
		t.Fatalf("Mapper Find: %v", err)
	}
	if len(models) != 1 || models[0].Name != "mapper-fallback" {
		t.Errorf("Mapper 兜底应命中 pg_tenant_user 表, 期望 [mapper-fallback], 实际 %+v", models)
	}
}
