//go:build integration

package xormdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

// ═══════════════════════════════════════════════════════════════════════
// 统一 orm tag 体系集成测试（orm-tag-design.md §11.15/§11.16/§11.18，U18）。
// 本节全部模型使用纯 orm tag（+ rel/ext），连接经 NewXormDriver 真实建连并
// 设置 tag_identifier=orm，DDL 断言读取 information_schema/pg_constraint/
// pg_indexes 等真实方言元数据。与 mysql_integration_test.go 同包编译，共享
// 模型与 runner 定义在本文件。
// ═══════════════════════════════════════════════════════════════════════

// ── 共享测试基建 ─────────────────────────────────────────────────────

// newXormOrmPG 走 NewXormDriver 真实配置链路创建 postgres 驱动（tag_identifier=orm，
// Sync2 启动期校验/ext 降级告警全部生效），未设置环境变量时跳过。
func newXormOrmPG(t *testing.T) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_PG_DSN 环境变量，跳过 pgsql 集成测试")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver:        "xorm",
		Engine:        "postgres",
		DSN:           dsn,
		TagIdentifier: "orm",
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建 xorm postgres 驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// intgXMustExec 原生 DDL/DML 执行辅助。
func intgXMustExec(t *testing.T, drv *XormDriver, sql string, values ...any) {
	t.Helper()
	if err := drv.Query().Exec(sql, values...); err != nil {
		t.Fatalf("执行 %q 失败: %v", sql, err)
	}
}

// intgXDropTables 幂等清表（用例开始前清残留 + t.Cleanup 清终态）。
func intgXDropTables(t *testing.T, drv *XormDriver, tables ...string) {
	t.Helper()
	for _, tb := range tables {
		if err := drv.Query().Exec("DROP TABLE IF EXISTS " + tb); err != nil {
			t.Fatalf("DROP TABLE %s: %v", tb, err)
		}
	}
}

// intgXMigrate 迁移模型并登记清理。
func intgXMigrate(t *testing.T, drv *XormDriver, tables []string, models ...any) {
	t.Helper()
	intgXDropTables(t, drv, tables...)
	if err := drv.AutoMigrate(models...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, tables...) })
}

// intgXContains 通用子串断言。
func intgXContains(t *testing.T, name, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s 应包含 %q\n实际: %s", name, needle, haystack)
	}
}

// intgXPGColumn 读 information_schema 单列元数据（小写键，pgx 原样返回列标签）。
func intgXPGColumn(t *testing.T, drv *XormDriver, table, column string) map[string]any {
	t.Helper()
	var rows []map[string]any
	err := drv.Query().Raw(
		`SELECT data_type, character_maximum_length, numeric_precision, numeric_scale,
		        is_nullable, column_default
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = ? AND column_name = ?`,
		table, column).ScanMap(&rows)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取列 %s.%s 元数据失败 rows=%d err=%v", table, column, len(rows), err)
	}
	return rows[0]
}

// intgXPGIndexes 读表上全部索引定义（pg_indexes.indexdef 拼接）。
func intgXPGIndexes(t *testing.T, drv *XormDriver, table string) string {
	t.Helper()
	var defs []string
	if err := drv.Query().Raw(
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename = ?`,
		table).Scan(&defs); err != nil {
		t.Fatalf("读取 %s 索引失败: %v", table, err)
	}
	return strings.Join(defs, "\n")
}

// intgXPGComment 读列注释（col_description）。
func intgXPGComment(t *testing.T, drv *XormDriver, table, column string) string {
	t.Helper()
	var cmt string
	if err := drv.Query().Raw(
		`SELECT coalesce(col_description(?::regclass, ordinal_position), '')
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = ? AND column_name = ?`,
		table, table, column).Scan(&cmt); err != nil {
		t.Fatalf("读取 %s.%s 注释失败: %v", table, column, err)
	}
	return cmt
}

// ── DDL 全维度模型（§11.15）──────────────────────────────────────────
//
// unsigned 单独模型断言（xorm postgres 方言为容纳无符号取值域加宽为 BIGINT，
// 差异固化）；check/autoIncrementIncrement 为 ext 降级项（§8.4）。

type intgXDDLModel struct {
	ID     string  `orm:"pk varchar(16) 'id'"`
	Name   string  `orm:"varchar(100) 'name' notnull default('anon')"`
	Email  string  `orm:"varchar(100) 'email' unique"`
	Amount float64 `orm:"decimal(10,2) 'amount' notnull"`
	BigNum int64   `orm:"bigint 'big_num' default(0)"`
	// Active 不带 default：xorm 对 bool 渲染 "BOOL DEFAULT 0"，PG 严格类型
	// 报 42804（boolean ≠ integer）——默认值断言由数值列（big_num/age）覆盖。
	Active bool   `orm:"bool 'active'"`
	OrgID  string `orm:"varchar(16) 'org_id' index(idx_intg_x_org)"`
	DeptID string `orm:"varchar(16) 'dept_id' index(idx_intg_x_org)"`
	Tenant string `orm:"varchar(16) 'tenant_id' unique(uk_intg_x_tenant)"`
	TKey   string `orm:"varchar(32) 't_key' unique(uk_intg_x_tenant)"`
	Remark string `orm:"varchar(200) 'remark' comment('备注列')"`
	Age    int    `orm:"'age' default(0)" ext:"check:age >= 0"`
	Seq    int64  `orm:"'seq'" ext:"autoIncrementIncrement:5"`
}

func (intgXDDLModel) TableName() string { return "intg_xddl_models" }

// TestXormOrmTag_DDLFullMatrix（§11.15/§11.18）xorm identifier=orm 全维度 DDL：
// Sync2 后对 information_schema/pg_indexes/col_description 逐项断言。
func TestXormOrmTag_DDLFullMatrix(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xddl_models"}, &intgXDDLModel{})

	// varchar(100) → character varying(100)
	name := intgXPGColumn(t, drv, "intg_xddl_models", "name")
	if name["data_type"] != "character varying" {
		t.Errorf("varchar(100) 应映射 character varying, 实际 %v", name["data_type"])
	}
	if fmt.Sprint(name["character_maximum_length"]) != "100" {
		t.Errorf("varchar(100) 长度应为 100, 实际 %v", name["character_maximum_length"])
	}
	if name["is_nullable"] != "NO" {
		t.Errorf("notnull 列应 NOT NULL, 实际 is_nullable=%v", name["is_nullable"])
	}
	intgXContains(t, "name 默认值", fmt.Sprint(name["column_default"]), "anon")

	// decimal(10,2) → numeric 精度 10/2
	amount := intgXPGColumn(t, drv, "intg_xddl_models", "amount")
	if amount["data_type"] != "numeric" {
		t.Errorf("decimal(10,2) 应映射 numeric, 实际 %v", amount["data_type"])
	}
	if fmt.Sprint(amount["numeric_precision"]) != "10" || fmt.Sprint(amount["numeric_scale"]) != "2" {
		t.Errorf("decimal(10,2) 精度应为 (10,2), 实际 %v/%v",
			amount["numeric_precision"], amount["numeric_scale"])
	}
	if amount["is_nullable"] != "NO" {
		t.Errorf("amount notnull 应 NOT NULL, 实际 %v", amount["is_nullable"])
	}

	// bigint / bool 类型映射
	if big := intgXPGColumn(t, drv, "intg_xddl_models", "big_num"); big["data_type"] != "bigint" {
		t.Errorf("bigint 应映射 bigint, 实际 %v", big["data_type"])
	}
	if act := intgXPGColumn(t, drv, "intg_xddl_models", "active"); act["data_type"] != "boolean" {
		t.Errorf("bool 应映射 boolean, 实际 %v", act["data_type"])
	}

	// default(0) 数值默认值
	intgXContains(t, "age 默认值", fmt.Sprint(intgXPGColumn(t, drv, "intg_xddl_models", "age")["column_default"]), "0")

	// comment('备注列')：xorm postgres 方言经 COMMENT ON COLUMN 落地
	if cmt := intgXPGComment(t, drv, "intg_xddl_models", "remark"); cmt != "备注列" {
		t.Errorf("remark 注释应为 备注列, 实际 %q", cmt)
	}

	// 联合唯一 unique(uk_intg_x_tenant) 与联合索引 index(idx_intg_x_org) 落地
	idx := intgXPGIndexes(t, drv, "intg_xddl_models")
	t.Logf("intg_xddl_models indexes:\n%s", idx)
	intgXContains(t, "联合唯一", idx, "uk_intg_x_tenant")
	intgXContains(t, "联合唯一列 tenant_id", idx, "tenant_id")
	intgXContains(t, "联合唯一列 t_key", idx, "t_key")
	intgXContains(t, "联合索引", idx, "idx_intg_x_org")

	// 列级 unique：应存在覆盖 email 的唯一索引（xorm 命名不假设，按唯一性断言）
	var uniqIdx []string
	if err := drv.Query().Raw(
		`SELECT c2.relname FROM pg_index x
		 JOIN pg_class c1 ON c1.oid = x.indrelid
		 JOIN pg_class c2 ON c2.oid = x.indexrelid
		 JOIN pg_attribute a ON a.attrelid = c1.oid AND a.attnum = ANY(x.indkey)
		 WHERE c1.relname = 'intg_xddl_models' AND a.attname = 'email' AND x.indisunique`).Scan(&uniqIdx); err != nil || len(uniqIdx) == 0 {
		t.Errorf("email unique 应存在唯一索引, 实际 %v err=%v", uniqIdx, err)
	}

	// ext check：差异固化——xorm Sync2 不生成 CHECK（ormtag_check.go 降级告警），
	// pg_constraint 应无 CHECK 行；写入违反表达式的行应成功。
	var chkCount int64
	if err := drv.Query().Raw(
		`SELECT count(*) FROM pg_constraint WHERE conrelid = 'intg_xddl_models'::regclass AND contype = 'c'`).Scan(&chkCount); err != nil || chkCount != 0 {
		t.Errorf("xorm Sync2 不应生成 CHECK 约束, count=%d err=%v", chkCount, err)
	}
	if err := drv.Query().Exec(
		`INSERT INTO intg_xddl_models (id, name, amount, age) VALUES ('chk1', 'n', 1, -5)`); err != nil {
		t.Errorf("ext check 降级后写入非法值应成功: %v", err)
	}

	// ext autoIncrementIncrement:5：步长为 MySQL 会话级行为，PG 下无载体——
	// 仅固化"迁移不报错、列正常建立"（xorm 侧该 ext 整体降级告警，§8.4）。
	if seq := intgXPGColumn(t, drv, "intg_xddl_models", "seq"); seq["data_type"] != "bigint" {
		t.Errorf("seq 列应正常建立为 bigint, 实际 %v", seq["data_type"])
	}

	// 幂等：重复 AutoMigrate 不报错、不重复建索引
	if err := drv.AutoMigrate(&intgXDDLModel{}); err != nil {
		t.Errorf("重复 AutoMigrate 应幂等不报错: %v", err)
	}
}

// IntgXUnsignedModel unsigned 数值列（差异固化：PG 无无符号语义，int 照常落地）。
type IntgXUnsignedModel struct {
	ID    string `orm:"pk varchar(16) 'id'"`
	Score uint32 `orm:"unsigned int 'score' default(0)"`
}

func (IntgXUnsignedModel) TableName() string { return "intg_xunsigned" }

func TestXormOrmTag_UnsignedDegradesOnPG(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xunsigned"}, &IntgXUnsignedModel{})
	score := intgXPGColumn(t, drv, "intg_xunsigned", "score")
	// xorm postgres 方言无 UNSIGNED：为容纳无符号 32 位取值域加宽为 BIGINT
	//（差异固化；MySQL 侧见 mysql_integration_test.go 的 UNSIGNED 断言）。
	if score["data_type"] != "bigint" {
		t.Errorf("PG 下 unsigned int 应降级为 bigint, 实际 %v", score["data_type"])
	}
	if err := drv.Query().Exec(`INSERT INTO intg_xunsigned (id, score) VALUES ('u1', 7)`); err != nil {
		t.Fatalf("写入: %v", err)
	}
}

// ── 唯一冲突错误映射（§11.14/§11.18）─────────────────────────────────

type intgXDupModel struct {
	ID    string `orm:"pk varchar(16) 'id'"`
	Email string `orm:"varchar(100) 'email' unique"`
}

func (intgXDupModel) TableName() string { return "intg_xdups" }

func TestXormOrmTag_DuplicateKeyMapping(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xdups"}, &intgXDupModel{})

	q := drv.Query()
	if err := q.Create(&intgXDupModel{ID: "d1", Email: "dup@pg.dev"}); err != nil {
		t.Fatalf("首次插入: %v", err)
	}
	err := q.Create(&intgXDupModel{ID: "d2", Email: "dup@pg.dev"})
	if err == nil {
		t.Fatal("unique 列重复插入应报错（PG SQLSTATE 23505）")
	}
	if !errors.Is(err, contracts.ErrDuplicatedKey) {
		t.Errorf("唯一冲突应映射 contracts.ErrDuplicatedKey, 实际: %v", err)
	}
}

// ── FOR UPDATE 悲观锁并发互斥（§11.10/§11.18）────────────────────────

type intgXLockModel struct {
	ID  string `orm:"pk varchar(16) 'id'"`
	Cnt int    `orm:"'cnt' default(0)"`
}

func (intgXLockModel) TableName() string { return "intg_xlocks" }

func TestXormOrmTag_ForUpdateMutex(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xlocks"}, &intgXLockModel{})
	intgXMustExec(t, drv, `INSERT INTO intg_xlocks (id, cnt) VALUES ('lock1', 1)`)

	// 事务一：ForUpdate 锁定行并持锁，持锁期间改值
	locked := make(chan struct{})
	release := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- drv.Query().Transaction(func(tx contracts.Query) error {
			var row intgXLockModel
			if err := tx.Model(&intgXLockModel{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1"); err != nil {
				return err
			}
			close(locked)
			<-release // 持锁等待
			return tx.Model(&intgXLockModel{}).Where("id = ?", "lock1").UpdateResult("cnt", 99).Error
		})
	}()
	<-locked

	// 事务二：同一行 FOR UPDATE 应被互斥——用 lock_timeout 观察阻塞
	err := drv.Query().Transaction(func(tx contracts.Query) error {
		if err := tx.Exec("SET LOCAL lock_timeout = '400ms'"); err != nil {
			return err
		}
		var row intgXLockModel
		return tx.Model(&intgXLockModel{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1")
	})
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("第二事务应在锁超时失败（FOR UPDATE 互斥）, 实际: %v", err)
	}

	// 释放后：事务一提交，第三事务可获锁并读到持锁事务写入的新值
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("事务一: %v", err)
	}
	var after intgXLockModel
	if err := drv.Query().Transaction(func(tx contracts.Query) error {
		return tx.Model(&intgXLockModel{}).Lock(contracts.LockForUpdate).First(&after, "id = ?", "lock1")
	}); err != nil {
		t.Fatalf("释放后 FOR UPDATE 应成功: %v", err)
	}
	if after.Cnt != 99 {
		t.Errorf("持锁事务写入应已提交, 期望 cnt=99, 实际 %d", after.Cnt)
	}
}

// TestXormOrmTag_LockShareModeNoop（§11.10）xorm 无 SHARE 锁支持——差异固化：
// Lock(LockShareMode) 为文档化 no-op，查询不报错、返回数据。
func TestXormOrmTag_LockShareModeNoop(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xlocks"}, &intgXLockModel{})
	intgXMustExec(t, drv, `INSERT INTO intg_xlocks (id, cnt) VALUES ('s1', 7)`)

	var rows []intgXLockModel
	if err := drv.Query().Model(&intgXLockModel{}).Lock(contracts.LockShareMode).Find(&rows); err != nil {
		t.Fatalf("Lock(LockShareMode) no-op 不应报错: %v", err)
	}
	if len(rows) != 1 || rows[0].Cnt != 7 {
		t.Errorf("Lock(LockShareMode) 查询结果异常: %+v", rows)
	}
}

// ── 乐观锁真实方言冒烟（§11.18）──────────────────────────────────────

type intgXVerModel struct {
	ID      string `orm:"pk varchar(16) 'id'"`
	Title   string `orm:"varchar(64) 'title'"`
	Version int64  `orm:"version 'version'"`
}

func (intgXVerModel) TableName() string { return "intg_xvers" }

// TestXormOrmTag_OptimisticLock orm:"version" 模型（xorm 原生 version 语义）：
// 插入置 1、struct 更新自增、陈旧版本 0 行不报错。
func TestXormOrmTag_OptimisticLock(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xvers"}, &intgXVerModel{})

	q := drv.Query()
	doc := &intgXVerModel{ID: "v1", Title: "t1"}
	if err := q.Create(doc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("插入后 version 应置 1, 实际 %d", doc.Version)
	}

	got := &intgXVerModel{}
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Take(got); err != nil {
		t.Fatalf("Take: %v", err)
	}
	got.Title = "t2"
	if err := q.Save(got); err != nil {
		t.Fatalf("Save(version 更新): %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("struct 更新后 version 应回填 +1 为 2, 实际 %d", got.Version)
	}

	// 陈旧版本（库中已是 2）：0 行命中不报错，数据不变
	stale := &intgXVerModel{ID: "v1", Title: "stale", Version: 1}
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Updates(stale); err != nil {
		t.Fatalf("陈旧版本更新应 0 行不报错: %v", err)
	}
	var unchanged intgXVerModel
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Take(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != 2 || unchanged.Title != "t2" {
		t.Errorf("陈旧版本更新不应改动数据: %+v", unchanged)
	}
}

// ── ext 双驱动行为矩阵（§11.16/§11.18，xorm 侧差异固化）───────────────

type intgXExtModel struct {
	ID        string `orm:"pk varchar(16) 'id'"`
	Amount    int    `orm:"'amount' default(0)" ext:"check:amount >= 0"`
	Remark    string `orm:"varchar(100) 'remark'" ext:"migration:false"`
	CreatedAt int64  `orm:"created 'created_at'" ext:"timePrecision:milli"`
}

func (intgXExtModel) TableName() string { return "intg_xexts" }

// TestXormOrmTag_ExtDegrade xorm 侧 ext 降级（§8.4/§11.16）：启动成功 +
// CHECK 不生效（非法值写入成功）+ migration:false 字段照常建列 + created 填
// unix 秒（timePrecision 被忽略）。行为差异与 gormdriver（check 拒绝/不建列/
// 填毫秒）以用例对照固化。
func TestXormOrmTag_ExtDegrade(t *testing.T) {
	drv := newXormOrmPG(t)
	intgXMigrate(t, drv, []string{"intg_xexts"}, &intgXExtModel{})

	// migration:false 差异固化：Sync2 照常建列
	var n int64
	if err := drv.Query().Raw(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'intg_xexts' AND column_name = 'remark'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("xorm 侧 migration:false 字段应照常建列, count=%d err=%v", n, err)
	}

	// check 降级：非法值写入成功
	if err := drv.Query().Create(&intgXExtModel{ID: "bad1", Amount: -5}); err != nil {
		t.Errorf("ext check 降级后写入非法值应成功: %v", err)
	}

	// timePrecision 降级：created 填 unix 秒（量级 1e9，非毫秒 1e12）
	nowSec := time.Now().Unix()
	var got intgXExtModel
	if err := drv.Query().Model(&intgXExtModel{}).Where("id = ?", "bad1").Take(&got); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.CreatedAt < nowSec-60 || got.CreatedAt > nowSec+60 {
		t.Errorf("xorm 侧 created 应为 unix 秒（timePrecision 降级）, 实际 %d（now=%d）", got.CreatedAt, nowSec)
	}
}

// ── Preload 全套（§11.18：套件同款模型结构，orm tag + rel）───────────
//
// xorm 侧全部关联经共享 Preload 引擎（rel tag 元数据 + orm:"-" 忽略标记），
// 与 gormdriver 的 gorm 原生/引擎分流形成双路径覆盖。

type intgXUser struct {
	ID     string       `orm:"pk varchar(16) 'id'"`
	Name   string       `orm:"varchar(64) 'name'"`
	DeptID string       `orm:"varchar(16) 'dept_id' null"`
	Orders []intgXOrder `orm:"-" rel:"foreignKey:UserID;references:ID"`
	Dept   *intgXDept   `orm:"-" rel:"foreignKey:DeptID;references:ID"`
	Roles  []intgXRole  `orm:"-" rel:"many2many:intg_x_user_roles;joinForeignKey:ID;joinReferences:Code"`
	Photos []intgXPhoto `orm:"-" rel:"polymorphic:Owner;polymorphicValue:intg_x_users"`
}

func (intgXUser) TableName() string { return "intg_xusers" }

type intgXOrder struct {
	ID     string      `orm:"pk varchar(16) 'id'"`
	UserID string      `orm:"varchar(16) 'user_id'"`
	Amount int         `orm:"'amount' default(0)"`
	Items  []intgXItem `orm:"-" rel:"foreignKey:OrderID;references:ID"`
}

func (intgXOrder) TableName() string { return "intg_xorders" }

type intgXItem struct {
	ID      string `orm:"pk varchar(16) 'id'"`
	OrderID string `orm:"varchar(16) 'order_id'"`
	Sku     string `orm:"varchar(64) 'sku'"`
}

func (intgXItem) TableName() string { return "intg_xitems" }

type intgXDept struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Name string `orm:"varchar(64) 'name'"`
}

func (intgXDept) TableName() string { return "intg_xdepts" }

type intgXRole struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(16) 'code'"`
	Name string `orm:"varchar(64) 'name'"`
}

func (intgXRole) TableName() string { return "intg_xroles" }

type intgXPhoto struct {
	ID        string `orm:"pk varchar(16) 'id'"`
	OwnerID   string `orm:"varchar(16) 'owner_id'"`
	OwnerType string `orm:"varchar(32) 'owner_type'"`
	URL       string `orm:"varchar(120) 'url'"`
}

func (intgXPhoto) TableName() string { return "intg_xphotos" }

// runXormIntgPreloadMatrix PG/MySQL 共用的 Preload 全套断言（11.18）：
// has-many / 嵌套 / belongs-to / many2many / polymorphic 各一例。
func runXormIntgPreloadMatrix(t *testing.T, drv *XormDriver) {
	t.Helper()
	tables := []string{
		"intg_x_user_roles", "intg_xusers", "intg_xorders", "intg_xitems",
		"intg_xdepts", "intg_xroles", "intg_xphotos",
	}
	intgXMigrate(t, drv, tables,
		&intgXUser{}, &intgXOrder{}, &intgXItem{}, &intgXDept{}, &intgXRole{}, &intgXPhoto{})
	intgXMustExec(t, drv, `CREATE TABLE intg_x_user_roles (id varchar(16) NOT NULL, code varchar(16) NOT NULL)`)

	q := drv.Query()
	for _, s := range []any{
		&intgXDept{ID: "d1", Name: "研发部"},
		&intgXUser{ID: "u1", Name: "alice", DeptID: "d1"},
		&intgXUser{ID: "u2", Name: "bob"},
		&intgXUser{ID: "u3", Name: "carol"}, // 无订单/无照片
		&intgXOrder{ID: "o1", UserID: "u1", Amount: 100},
		&intgXOrder{ID: "o2", UserID: "u1", Amount: 250},
		&intgXOrder{ID: "o3", UserID: "u2", Amount: 50},
		&intgXItem{ID: "i1", OrderID: "o1", Sku: "sku1"},
		&intgXItem{ID: "i2", OrderID: "o2", Sku: "sku2"},
		&intgXItem{ID: "i3", OrderID: "o3", Sku: "sku3"},
		&intgXRole{ID: "r1", Code: "c1", Name: "管理员"},
		&intgXRole{ID: "r2", Code: "c2", Name: "审计"},
		&intgXPhoto{ID: "p1", OwnerID: "u1", OwnerType: "intg_x_users", URL: "u1a"},
		&intgXPhoto{ID: "p2", OwnerID: "u1", OwnerType: "intg_x_users", URL: "u1b"},
		&intgXPhoto{ID: "px", OwnerID: "u1", OwnerType: "other_type", URL: "noise"}, // 类型过滤负面用例
	} {
		if err := q.Create(s); err != nil {
			t.Fatalf("种子 %T %+v: %v", s, s, err)
		}
	}
	intgXMustExec(t, drv,
		`INSERT INTO intg_x_user_roles (id, code) VALUES ('u1','c1'), ('u1','c2'), ('u2','c2')`)

	var users []intgXUser
	if err := q.Preload("Orders.Items").Preload("Dept").Preload("Roles").Preload("Photos").Find(&users); err != nil {
		t.Fatalf("Preload 全套查询失败: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("应查到 3 个用户, 实际 %d", len(users))
	}
	byID := make(map[string]*intgXUser, len(users))
	for i := range users {
		byID[users[i].ID] = &users[i]
	}

	// has-many + 嵌套
	u1 := byID["u1"]
	if len(u1.Orders) != 2 {
		t.Fatalf("u1 应预加载 2 个订单, 实际 %d", len(u1.Orders))
	}
	skuByOrder := map[string]string{}
	for _, o := range u1.Orders {
		if len(o.Items) != 1 {
			t.Fatalf("订单 %s 应预加载 1 个明细, 实际 %d", o.ID, len(o.Items))
		}
		skuByOrder[o.ID] = o.Items[0].Sku
	}
	if skuByOrder["o1"] != "sku1" || skuByOrder["o2"] != "sku2" {
		t.Errorf("嵌套明细回填错误: %v", skuByOrder)
	}
	u2 := byID["u2"]
	if len(u2.Orders) != 1 || len(u2.Orders[0].Items) != 1 || u2.Orders[0].Items[0].Sku != "sku3" {
		t.Errorf("u2 订单 o3 明细回填错误: %+v", u2.Orders)
	}
	if byID["u3"].Orders == nil || len(byID["u3"].Orders) != 0 {
		t.Errorf("u3 无订单应回填非 nil 空切片（引擎契约 5）, 实际 %+v", byID["u3"].Orders)
	}

	// belongs-to
	if u1.Dept == nil || u1.Dept.Name != "研发部" {
		t.Errorf("u1 belongs-to 部门应回填, 实际 %+v", u1.Dept)
	}
	if u2.Dept != nil {
		t.Errorf("u2 无部门应为 nil, 实际 %+v", u2.Dept)
	}

	// many2many（两跳）
	roleCodes := map[string]bool{}
	for _, r := range u1.Roles {
		roleCodes[r.Code] = true
	}
	if len(roleCodes) != 2 || !roleCodes["c1"] || !roleCodes["c2"] {
		t.Errorf("u1 应预加载角色 c1/c2, 实际 %v", u1.Roles)
	}
	if len(u2.Roles) != 1 || u2.Roles[0].Code != "c2" {
		t.Errorf("u2 应仅预加载角色 c2, 实际 %+v", u2.Roles)
	}

	// polymorphic（类型列过滤）
	if len(u1.Photos) != 2 {
		t.Errorf("u1 应预加载 2 张照片（other_type 被过滤）, 实际 %+v", u1.Photos)
	}
	for _, p := range u1.Photos {
		if p.OwnerType != "intg_x_users" {
			t.Errorf("多态类型过滤失效: %+v", p)
		}
	}
}

func TestXormOrmTag_PreloadMatrix(t *testing.T) {
	drv := newXormOrmPG(t)
	runXormIntgPreloadMatrix(t, drv)
}

// ── 多租户 schema 全链（§11.12/§11.18，PG 专属，orm tag 模型形态）────

type intgXTenantUser struct {
	ID     string             `orm:"pk varchar(16) 'id'"`
	Name   string             `orm:"varchar(64) 'name'"`
	Orders []intgXTenantOrder `orm:"-" rel:"foreignKey:UserID;references:ID"`
}

func (intgXTenantUser) TableName() string { return "intg_xtenant_users" }

type intgXTenantOrder struct {
	ID     string `orm:"pk varchar(16) 'id'"`
	UserID string `orm:"varchar(16) 'user_id'"`
	Amount int    `orm:"'amount' default(0)"`
}

func (intgXTenantOrder) TableName() string { return "intg_xtenant_orders" }

// TestXormOrmTag_TenantFullChain：Schema("tenant_x") + orm tag 模型——
// AutoMigrate 建在租户 schema（SET LOCAL search_path 事务内执行）、
// Model/Table 与 Schema 调用顺序无关、Preload 子查询带 schema 前缀。
func TestXormOrmTag_TenantFullChain(t *testing.T) {
	drv := newXormOrmPG(t)
	ten := "tenant_xormorm"

	intgXMustExec(t, drv, "DROP SCHEMA IF EXISTS "+ten+" CASCADE")
	intgXMustExec(t, drv, "CREATE SCHEMA "+ten)
	t.Cleanup(func() { _ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE") })
	// 清理历史运行可能遗留在 public 的同名表（早期缺陷泄漏），保证断言确定性
	intgXMustExec(t, drv, "DROP TABLE IF EXISTS public.intg_xtenant_users")
	intgXMustExec(t, drv, "DROP TABLE IF EXISTS public.intg_xtenant_orders")

	tenantDrv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver:        "xorm",
		Engine:        "postgres",
		DSN:           os.Getenv("GOFAST_TEST_PG_DSN"),
		Schema:        ten,
		TagIdentifier: "orm",
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建租户驱动: %v", err)
	}
	t.Cleanup(func() { _ = tenantDrv.Close() })
	if err := tenantDrv.AutoMigrate(&intgXTenantUser{}, &intgXTenantOrder{}); err != nil {
		t.Fatalf("租户 schema AutoMigrate: %v", err)
	}

	// 表落在正确 schema（public 下不存在同名表）
	var n int64
	if err := drv.Query().Raw(
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = ? AND table_name = 'intg_xtenant_users'`, ten).Scan(&n); err != nil || n != 1 {
		t.Fatalf("intg_xtenant_users 应建在 schema %s, count=%d err=%v", ten, n, err)
	}
	if err := drv.Query().Raw(
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = 'intg_xtenant_users'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("public 下不应存在同名表, count=%d err=%v", n, err)
	}

	// 灌租户数据（经租户驱动，查询链自带 schema 前缀）
	tq := tenantDrv.Query()
	for _, s := range []any{
		&intgXTenantUser{ID: "tu1", Name: "租户用户"},
		&intgXTenantOrder{ID: "to1", UserID: "tu1", Amount: 66},
		&intgXTenantOrder{ID: "to2", UserID: "tu1", Amount: 88},
	} {
		if err := tq.Create(s); err != nil {
			t.Fatalf("灌租户数据 %T: %v", s, err)
		}
	}

	// Model/Table 与 Schema 调用顺序无关（两种顺序均命中租户表）
	var usersA []intgXTenantUser
	if err := drv.Query().Schema(ten).Model(&intgXTenantUser{}).Find(&usersA); err != nil {
		t.Fatalf("Schema→Model 顺序查询失败: %v", err)
	}
	if len(usersA) != 1 || usersA[0].Name != "租户用户" {
		t.Errorf("Schema→Model 应命中租户表, 实际 %+v", usersA)
	}
	var usersB []intgXTenantUser
	if err := drv.Query().Model(&intgXTenantUser{}).Schema(ten).Find(&usersB); err != nil {
		t.Fatalf("Model→Schema 顺序查询失败: %v", err)
	}
	if len(usersB) != 1 {
		t.Errorf("Model→Schema 应命中租户表, 实际 %+v", usersB)
	}

	// 显式 Table() 投影同样带 schema 前缀
	var names []map[string]any
	if err := drv.Query().Schema(ten).Table("intg_xtenant_users").
		Select("id", "name").Where("id = ?", "tu1").ScanMap(&names); err != nil {
		t.Fatalf("Schema→Table 投影失败: %v", err)
	}
	if len(names) != 1 {
		t.Errorf("Schema→Table 投影应命中租户表 1 行, 实际 %v", names)
	}

	// Preload 子查询带 schema 前缀（租户表关联数据正确）
	var roots []intgXTenantUser
	if err := drv.Query().Schema(ten).Model(&intgXTenantUser{}).
		Preload("Orders").Find(&roots); err != nil {
		t.Fatalf("租户 Preload: %v", err)
	}
	if len(roots) != 1 || len(roots[0].Orders) != 2 {
		t.Fatalf("租户 Preload Orders 应回填 2 单, 实际 %+v", roots)
	}
}
