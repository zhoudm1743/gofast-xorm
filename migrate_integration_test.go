//go:build integration

package xormdriver

// migrate_integration_test.go 迁移能力的 PostgreSQL 集成测试（需求文档 R1/R2/R4
// 的框架侧固化），依赖真实数据库：设置 GOFAST_TEST_PG_DSN 后
// `go test -tags integration -run PGXormMigrate ./`。
// 每个用例使用独立 schema 并 DROP SCHEMA ... CASCADE 清理，互不干扰。

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// ── 测试模型 ─────────────────────────────────────────────────────────

// migAlterV16/migAlterV32：同一张表（mig_alter）的两个版本，仅列宽不同，
// 用于构造 ALTER COLUMN TYPE 场景（需求 R1：0A000 验证）。
type migAlterV16 struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(16) notnull 'code'"`
}

func (migAlterV16) TableName() string { return "mig_alter" }

type migAlterV32 struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(32) notnull 'code'"`
}

func (migAlterV32) TableName() string { return "mig_alter" }

// MigDdlBase/migDdlOrder：R2 DDL 等价性代表模型，覆盖需求清单：
// string 主键、extends 嵌入、created/updated 时间戳、varchar(n) notnull default、
// 普通索引、唯一索引、复合索引、jsonb 列、comment、orm:"-"。
type MigDdlBase struct {
	ID        string    `orm:"pk varchar(16) 'id'"`
	CreatedAt time.Time `orm:"created 'created_at'"`
	UpdatedAt time.Time `orm:"updated 'updated_at'"`
}

type migDdlOrder struct {
	MigDdlBase `orm:"extends"`
	TenantID   string `orm:"varchar(16) notnull index 'tenant_id'"`
	Code       string `orm:"varchar(32) notnull default('none') unique(uk_code) 'code'"`
	Meta       []byte `orm:"jsonb 'meta'"`
	Memo       string `orm:"varchar(100) null comment('备注') 'memo'"`
	Region     string `orm:"index(grp_region) 'region'"`
	Shop       string `orm:"index(grp_region) 'shop'"`
	Ignored    string `orm:"-"`
}

func (migDdlOrder) TableName() string { return "mig_ddl_order" }

// migIdxOrder：R4 索引收敛模型（复合索引 grp = (region, shop)）。
type migIdxOrder struct {
	ID     string `orm:"pk varchar(16) 'id'"`
	Region string `orm:"index(grp) 'region'"`
	Shop   string `orm:"index(grp) 'shop'"`
}

func (migIdxOrder) TableName() string { return "mig_idx_order" }

// ── 基建 ─────────────────────────────────────────────────────────────

// newPGMigrateDriver 读取 GOFAST_TEST_PG_DSN，按 orm tag 体系建驱动；
// safe 为 true 时使用 NewMigrateDriver（迁移安全模式）。
func newPGMigrateDriver(t *testing.T, schema string, safe bool) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_PG_DSN 环境变量，跳过 pgsql 集成测试")
	}
	cfg := contracts.ConnectionConfig{
		Driver: "xorm", Engine: "postgres", DSN: dsn,
		TagIdentifier: "orm", Schema: schema,
	}
	var (
		drv *XormDriver
		err error
	)
	if safe {
		drv, err = NewMigrateDriver(cfg, noopLog{t: t})
	} else {
		drv, err = NewXormDriver(cfg, noopLog{t: t})
	}
	if err != nil {
		t.Fatalf("创建驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// resetPGSchema 重建测试 schema（DROP ... CASCADE + CREATE），返回无 schema 绑定
// 的管理驱动（用于 reset 与断言查询），调用方负责 Close。
func resetPGSchema(t *testing.T, schema string) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_PG_DSN 环境变量，跳过 pgsql 集成测试")
	}
	admin, err := NewXormDriver(contracts.ConnectionConfig{
		Driver: "xorm", Engine: "postgres", DSN: dsn, TagIdentifier: "orm",
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建管理驱动失败: %v", err)
	}
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`,
		`CREATE SCHEMA "` + schema + `"`,
	} {
		if _, err := admin.RawEngine().Exec(ddl); err != nil {
			_ = admin.Close()
			t.Fatalf("%s 失败: %v", ddl, err)
		}
	}
	return admin
}

// pgQuery 便捷断言查询：返回首行 map。
func pgQuery(t *testing.T, drv *XormDriver, sql string, args ...any) map[string][]byte {
	t.Helper()
	rows, err := drv.RawEngine().Query(append([]any{sql}, args...)...)
	if err != nil {
		t.Fatalf("查询失败 %q: %v", sql, err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// ── R1：迁移安全模式 0A000 验证 ────────────────────────────────────────

// TestPGXormMigrate_SafeModeAlterColumn 固化需求 R1 / 验收 3：migrate-safe 驱动
// 建 varchar(16) 表 → 同连接常规查询暖机 → Sync2 改 varchar(32) → 同连接再查询，
// 断言无 "cached plan must not change result type (0A000)"。
// 连接池限制单连接，保证暖机语句的计划缓存与改类型后查询落在同一 pgx 连接上。
func TestPGXormMigrate_SafeModeAlterColumn(t *testing.T) {
	schema := "tenant_xorm_mig_safe"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	// 连接池至少 2 连接：schema 模式下 Sync2 包事务，其内部元数据查询
	// （loadTableInfo）需从池内取第二条连接，单连接会死锁（实测确认）。
	drv.RawEngine().DB().SetMaxOpenConns(2)

	if err := drv.AutoMigrate(&migAlterV16{}); err != nil {
		t.Fatalf("Sync2 建表失败: %v", err)
	}
	warm := `SELECT code FROM "` + schema + `".mig_alter WHERE id = 'warm'`
	// 双连接并发暖机：database/sql 顺序复用同一空闲连接（LIFO），并发才能让
	// 两条连接都缓存执行计划，改类型后复查才具有确定性。
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := drv.RawEngine().Exec(warm); err != nil {
				t.Errorf("暖机查询失败: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := drv.AutoMigrate(&migAlterV32{}); err != nil {
		t.Fatalf("Sync2 改列类型失败: %v", err)
	}
	// 复用暖机语句：pgx 默认 cache_statement 模式下此处即 0A000 高发点。
	if _, err := drv.RawEngine().Exec(warm); err != nil {
		if strings.Contains(err.Error(), "0A000") {
			t.Fatalf("迁移安全模式下仍触发 0A000: %v", err)
		}
		t.Fatalf("改列类型后常规查询失败: %v", err)
	}
	// 列宽确实已变更。
	row := pgQuery(t, admin,
		`SELECT character_maximum_length FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 'mig_alter' AND column_name = 'code'`, schema)
	if string(row["character_maximum_length"]) != "32" {
		t.Fatalf("列宽未变更为 32: %v", row)
	}
}

// TestPGXormMigrate_DefaultModeObserved 官方实测记录（不断言）：默认（非 safe）
// 驱动在单连接下复现 ALTER COLUMN TYPE 场景，观察是否触发 0A000，结果写入测试
// 日志供行为文档引用（需求 R1 的"官方验证"动作）。
func TestPGXormMigrate_DefaultModeObserved(t *testing.T) {
	schema := "tenant_xorm_mig_obs"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, false)
	// 同 SafeMode 用例：schema 模式 Sync2 需 ≥2 连接（事务 + 内部元数据查询）。
	drv.RawEngine().DB().SetMaxOpenConns(2)

	if err := drv.AutoMigrate(&migAlterV16{}); err != nil {
		t.Fatalf("Sync2 建表失败: %v", err)
	}
	warm := `SELECT code FROM "` + schema + `".mig_alter WHERE id = 'warm'`
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := drv.RawEngine().Exec(warm); err != nil {
				t.Errorf("暖机查询失败: %v", err)
			}
		}()
	}
	wg.Wait()
	syncErr := drv.AutoMigrate(&migAlterV32{})
	_, queryErr := drv.RawEngine().Exec(warm)
	t.Logf("默认模式实测: Sync2(改类型) err=%v, 改类型后同语句查询 err=%v（0A000 命中=%v）",
		syncErr, queryErr, queryErr != nil && strings.Contains(queryErr.Error(), "0A000"))
}

// ── R2：DDL 等价性（information_schema 断言）──────────────────────────

// TestPGXormMigrate_DDLEquivalence 需求 R2：orm tag 代表模型的 Sync2 DDL 产物
// 以 information_schema 断言固化（xorm 路径期望清单；与 gorm PatchSchema 路径
// 的已知命名差异见 docs/migration.md 对照表白名单）。
func TestPGXormMigrate_DDLEquivalence(t *testing.T) {
	schema := "tenant_xorm_mig_ddl"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migDdlOrder{}); err != nil {
		t.Fatalf("Sync2 建表失败: %v", err)
	}

	// 列级断言：data_type / 长度 / nullable / 默认值。
	colCases := []struct {
		column   string
		dtype    string
		maxLen   string // character_maximum_length，非字符型为 ""
		nullable string // YES|NO
	}{
		{"id", "character varying", "16", "NO"},
		// created/updated 列 xorm 建为可空（与 gorm 行为一致，写入由引擎填充）。
		{"created_at", "timestamp without time zone", "", "YES"},
		{"updated_at", "timestamp without time zone", "", "YES"},
		{"tenant_id", "character varying", "16", "NO"},
		{"code", "character varying", "32", "NO"},
		{"meta", "jsonb", "", "YES"},
		{"memo", "character varying", "100", "YES"},
	}
	for _, c := range colCases {
		row := pgQuery(t, admin,
			`SELECT data_type, character_maximum_length, is_nullable
			 FROM information_schema.columns
			 WHERE table_schema = ? AND table_name = 'mig_ddl_order' AND column_name = ?`,
			schema, c.column)
		if row == nil {
			t.Fatalf("列 %s 不存在", c.column)
		}
		if string(row["data_type"]) != c.dtype {
			t.Fatalf("列 %s 类型=%s，期望 %s", c.column, row["data_type"], c.dtype)
		}
		if c.maxLen != "" && string(row["character_maximum_length"]) != c.maxLen {
			t.Fatalf("列 %s 长度=%s，期望 %s", c.column, row["character_maximum_length"], c.maxLen)
		}
		if string(row["is_nullable"]) != c.nullable {
			t.Fatalf("列 %s nullable=%s，期望 %s", c.column, row["is_nullable"], c.nullable)
		}
	}

	// 默认值：code 列 default('none')。
	row := pgQuery(t, admin,
		`SELECT column_default FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 'mig_ddl_order' AND column_name = 'code'`, schema)
	def := string(row["column_default"])
	if !strings.Contains(def, "none") {
		t.Fatalf("code 默认值=%q，期望包含 'none'", def)
	}

	// orm:"-" 列不建出。
	row = pgQuery(t, admin,
		`SELECT count(*) AS n FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 'mig_ddl_order' AND column_name = 'ignored'`, schema)
	if string(row["n"]) != "0" {
		t.Fatalf("orm:\"-\" 列被建出: %v", row)
	}

	// 列注释：memo 列 comment('备注')。
	row = pgQuery(t, admin,
		`SELECT col_description(('"' || table_schema || '"."' || table_name || '"')::regclass::oid, ordinal_position) AS c
		 FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 'mig_ddl_order' AND column_name = 'memo'`, schema)
	if got := strings.TrimSpace(string(row["c"])); got != "备注" {
		t.Fatalf("memo 列注释=%q，期望 备注", got)
	}

	// 索引断言：普通/复合索引期望名按 xorm XName 规则（IDX_ 前缀）；
	// 唯一索引名以实测为准（见文档白名单），按列集合断言。
	rows, err := admin.RawEngine().Query(
		`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = ? AND tablename = 'mig_ddl_order'`, schema)
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	got := make(map[string][]string, len(rows))
	for _, r := range rows {
		got[string(r["indexname"])] = parseIndexDefCols(string(r["indexdef"]))
	}
	for _, want := range []struct {
		name string
		cols []string
	}{
		{"IDX_mig_ddl_order_tenant_id", []string{"tenant_id"}},
		{"IDX_mig_ddl_order_grp_region", []string{"region", "shop"}},
	} {
		cols, ok := got[want.name]
		if !ok {
			t.Fatalf("索引 %s 不存在，实际索引集: %v", want.name, got)
		}
		if !equalCols(cols, want.cols) {
			t.Fatalf("索引 %s 列=%v，期望 %v", want.name, cols, want.cols)
		}
	}
	// 唯一索引：存在且落在 code 列（xorm 命名 IDX_/UQE_ 前缀，允许名称差异，列集合为准）。
	foundUnique := false
	for name, cols := range got {
		if !strings.HasPrefix(name, "UQE_") {
			continue
		}
		if equalCols(cols, []string{"code"}) {
			foundUnique = true
		}
	}
	if !foundUnique {
		t.Fatalf("code 列唯一索引不存在，实际索引集: %v", got)
	}
	// 主键索引存在。
	if _, ok := got["mig_ddl_order_pkey"]; !ok {
		t.Fatalf("主键索引缺失，实际索引集: %v", got)
	}
}

// ── R4：MigrateSQL Dry-run ─────────────────────────────────────────────

// TestPGXormMigrate_MigrateSQLDryRun 需求 R4：改列类型场景下 MigrateSQL 返回
// 待执行 DDL 且事务回滚、库结构零变化；无 diff 时返回空列表。
func TestPGXormMigrate_MigrateSQLDryRun(t *testing.T) {
	schema := "tenant_xorm_mig_dry"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migAlterV16{}); err != nil {
		t.Fatalf("基线建表失败: %v", err)
	}
	cfg := contracts.ConnectionConfig{
		Driver: "xorm", Engine: "postgres",
		DSN:           os.Getenv("GOFAST_TEST_PG_DSN"),
		TagIdentifier: "orm", Schema: schema,
	}

	// 无 diff：同版本模型 → 空 DDL。
	ddls, err := MigrateSQL(cfg, &migAlterV16{})
	if err != nil {
		t.Fatalf("MigrateSQL(无 diff) 失败: %v", err)
	}
	if len(ddls) != 0 {
		t.Fatalf("无 diff 场景应返回空 DDL，实际: %v", ddls)
	}

	// 有 diff：改 varchar(32) → 返回 ALTER 语句，且未真实生效。
	ddls, err = MigrateSQL(cfg, &migAlterV32{})
	if err != nil {
		t.Fatalf("MigrateSQL(有 diff) 失败: %v", err)
	}
	found := false
	for _, ddl := range ddls {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "ALTER TABLE") &&
			strings.Contains(ddl, "32") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未捕获 ALTER COLUMN 语句: %v", ddls)
	}
	row := pgQuery(t, admin,
		`SELECT character_maximum_length FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = 'mig_alter' AND column_name = 'code'`, schema)
	if string(row["character_maximum_length"]) != "16" {
		t.Fatalf("Dry-run 泄漏：列宽被改为 %s（应仍为 16，事务未回滚）", row["character_maximum_length"])
	}
}

// ── R4：索引收敛 ──────────────────────────────────────────────────────

// TestPGXormMigrate_IndexConvergence 需求 R4：手工造同名异构索引 → CheckIndexes
// 报告 mismatched → WithRebuildIndexes 迁移后收敛；索引缺失报 missing。
func TestPGXormMigrate_IndexConvergence(t *testing.T) {
	schema := "tenant_xorm_mig_idx"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 同名异构：DROP 重建为 (shop)。
	if _, err := admin.RawEngine().Exec(`DROP INDEX "` + schema + `"."IDX_mig_idx_order_grp"`); err != nil {
		t.Fatalf("DROP 索引失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec(`CREATE INDEX "IDX_mig_idx_order_grp" ON "` + schema + `".mig_idx_order (shop)`); err != nil {
		t.Fatalf("重建异构索引失败: %v", err)
	}

	cfg := contracts.ConnectionConfig{
		Driver: "xorm", Engine: "postgres",
		DSN:           os.Getenv("GOFAST_TEST_PG_DSN"),
		TagIdentifier: "orm", Schema: schema,
	}
	mis, err := CheckIndexes(cfg, &migIdxOrder{})
	if err != nil {
		t.Fatalf("CheckIndexes 失败: %v", err)
	}
	if len(mis) != 1 || mis[0].Kind != "mismatched" {
		t.Fatalf("期望 1 条 mismatched，实际: %+v", mis)
	}
	if !equalCols(mis[0].Expected, []string{"region", "shop"}) || !equalCols(mis[0].Actual, []string{"shop"}) {
		t.Fatalf("差异详情不符: %+v", mis[0])
	}

	// WithRebuildIndexes 收敛。
	rebuild, err := NewXormDriver(cfg, noopLog{t: t}, WithMigrateSafe(), WithRebuildIndexes())
	if err != nil {
		t.Fatalf("建重建驱动失败: %v", err)
	}
	defer func() { _ = rebuild.Close() }()
	if err := rebuild.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("RebuildIndexes 迁移失败: %v", err)
	}
	mis, err = CheckIndexes(cfg, &migIdxOrder{})
	if err != nil {
		t.Fatalf("重建后 CheckIndexes 失败: %v", err)
	}
	if len(mis) != 0 {
		t.Fatalf("重建后仍有差异: %+v", mis)
	}

	// missing：手工 DROP 索引 → CheckIndexes 报 missing（Sync2 自身会补建）。
	if _, err := admin.RawEngine().Exec(`DROP INDEX "` + schema + `"."IDX_mig_idx_order_grp"`); err != nil {
		t.Fatalf("DROP 索引失败: %v", err)
	}
	mis, err = CheckIndexes(cfg, &migIdxOrder{})
	if err != nil {
		t.Fatalf("missing 场景 CheckIndexes 失败: %v", err)
	}
	if len(mis) != 1 || mis[0].Kind != "missing" {
		t.Fatalf("期望 1 条 missing，实际: %+v", mis)
	}
	// Sync2 补建后差异消失。
	if err := drv.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("Sync2 补建失败: %v", err)
	}
	mis, err = CheckIndexes(cfg, &migIdxOrder{})
	if err != nil || len(mis) != 0 {
		t.Fatalf("补建后仍有差异: %v %+v", err, mis)
	}
}

// ── R4（MySQL）：临时库克隆式 Dry-run + 索引收敛 ───────────────────────

// mysqlTestCfg 确保测试库存在并返回指向它的配置（GOFAST_TEST_MYSQL_DSN 门控）。
// DSN 可不指定库（root 权限自建 gofast_xorm_mig_test）。
func mysqlTestCfg(t *testing.T) contracts.ConnectionConfig {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_MYSQL_DSN 环境变量，跳过 mysql 集成测试")
	}
	admin, err := xorm.NewEngine("mysql", dsn)
	if err != nil {
		t.Fatalf("mysql 管理连接失败: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec("CREATE DATABASE IF NOT EXISTS `gofast_xorm_mig_test`"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	return contracts.ConnectionConfig{
		Driver: "xorm", Engine: "mysql",
		DSN:           rewriteMySQLDatabase(dsn, "gofast_xorm_mig_test"),
		TagIdentifier: "orm",
	}
}

// TestMySQLXormMigrate_MigrateSQLClone 需求 R4（mysql）：临时库克隆 Dry-run——
// 返回待执行 ALTER 且目标库结构零变化；无 diff 返回空；索引收敛全链路。
func TestMySQLXormMigrate_MigrateSQLClone(t *testing.T) {
	cfg := mysqlTestCfg(t)
	admin, err := NewXormDriver(cfg, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建驱动失败: %v", err)
	}
	defer func() { _ = admin.Close() }()
	for _, ddl := range []string{
		"DROP TABLE IF EXISTS mig_alter",
		"DROP TABLE IF EXISTS mig_idx_order",
	} {
		if _, err := admin.RawEngine().Exec(ddl); err != nil {
			t.Fatalf("%s 失败: %v", ddl, err)
		}
	}

	// 基线建表（varchar(16)）。
	if err := admin.AutoMigrate(&migAlterV16{}); err != nil {
		t.Fatalf("基线建表失败: %v", err)
	}

	// Dry-run：改 varchar(32) → 返回 ALTER，库内仍是 16。
	ddls, err := MigrateSQL(cfg, &migAlterV32{})
	if err != nil {
		t.Fatalf("MigrateSQL 失败: %v", err)
	}
	found := false
	for _, ddl := range ddls {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "ALTER TABLE") &&
			strings.Contains(strings.ToLower(ddl), "varchar(32)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未捕获 ALTER TABLE ... varchar(32): %v", ddls)
	}
	var colType string
	if err := admin.Query().Raw(
		`SELECT column_type FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'mig_alter' AND column_name = 'code'`).Scan(&colType); err != nil {
		t.Fatalf("回读列类型失败: %v", err)
	}
	if colType != "varchar(16)" {
		t.Fatalf("Dry-run 泄漏：code 列类型=%s（应仍为 varchar(16)）", colType)
	}

	// 无 diff → 空 DDL。
	ddls, err = MigrateSQL(cfg, &migAlterV16{})
	if err != nil {
		t.Fatalf("MigrateSQL(无 diff) 失败: %v", err)
	}
	if len(ddls) != 0 {
		t.Fatalf("无 diff 场景应返回空 DDL，实际: %v", ddls)
	}

	// 索引收敛：同名异构 → 报告 → 重建。
	if err := admin.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("建索引表失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec("ALTER TABLE mig_idx_order DROP INDEX `IDX_mig_idx_order_grp`"); err != nil {
		t.Fatalf("DROP 索引失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec("CREATE INDEX `IDX_mig_idx_order_grp` ON mig_idx_order (shop)"); err != nil {
		t.Fatalf("重建异构索引失败: %v", err)
	}
	mis, err := CheckIndexes(cfg, &migIdxOrder{})
	if err != nil {
		t.Fatalf("CheckIndexes 失败: %v", err)
	}
	if len(mis) != 1 || mis[0].Kind != "mismatched" {
		t.Fatalf("期望 1 条 mismatched，实际: %+v", mis)
	}
	rebuild, err := NewXormDriver(cfg, noopLog{t: t}, WithRebuildIndexes())
	if err != nil {
		t.Fatalf("建重建驱动失败: %v", err)
	}
	defer func() { _ = rebuild.Close() }()
	if err := rebuild.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("RebuildIndexes 迁移失败: %v", err)
	}
	mis, err = CheckIndexes(cfg, &migIdxOrder{})
	if err != nil || len(mis) != 0 {
		t.Fatalf("重建后仍有差异: %v %+v", err, mis)
	}
}

// ── R4（MSSQL）：事务回滚式 Dry-run ─────────────────────────────────────

// mssqlTestCfg 确保测试库存在并返回指向它的配置（GOFAST_TEST_MSSQL_DSN 门控，
// DSN 形如 "server=host;port=1433;user id=sa;password=..."，可不指定 database）。
func mssqlTestCfg(t *testing.T) contracts.ConnectionConfig {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_MSSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_MSSQL_DSN 环境变量，跳过 mssql 集成测试")
	}
	admin, err := xorm.NewEngine("mssql", dsn+";database=master")
	if err != nil {
		t.Fatalf("mssql 管理连接失败: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec("IF DB_ID('gofast_xorm_mig_test') IS NULL CREATE DATABASE gofast_xorm_mig_test"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	return contracts.ConnectionConfig{
		Driver: "xorm", Engine: "mssql",
		DSN:           dsn + ";database=gofast_xorm_mig_test",
		TagIdentifier: "orm",
	}
}

// TestMSSQLXormMigrate_MigrateSQLDryRun 需求 R4（mssql）：DDL 事务性——
// capture + 回滚实现 Dry-run，结构零变化。
func TestMSSQLXormMigrate_MigrateSQLDryRun(t *testing.T) {
	cfg := mssqlTestCfg(t)
	admin, err := NewXormDriver(cfg, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建驱动失败: %v", err)
	}
	defer func() { _ = admin.Close() }()
	for _, ddl := range []string{
		"IF OBJECT_ID('mig_alter', 'U') IS NOT NULL DROP TABLE mig_alter",
		"IF OBJECT_ID('mig_idx_order', 'U') IS NOT NULL DROP TABLE mig_idx_order",
	} {
		if _, err := admin.RawEngine().Exec(ddl); err != nil {
			t.Fatalf("%s 失败: %v", ddl, err)
		}
	}

	if err := admin.AutoMigrate(&migAlterV16{}); err != nil {
		t.Fatalf("基线建表失败: %v", err)
	}

	// Dry-run：改 varchar(32) → 返回 ALTER，库内仍是 16。
	ddls, err := MigrateSQL(cfg, &migAlterV32{})
	if err != nil {
		t.Fatalf("MigrateSQL 失败: %v", err)
	}
	found := false
	for _, ddl := range ddls {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "ALTER TABLE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未捕获 ALTER TABLE 语句: %v", ddls)
	}
	var maxLen int
	if err := admin.Query().Raw(
		`SELECT character_maximum_length FROM information_schema.columns
		 WHERE table_name = 'mig_alter' AND column_name = 'code'`).Scan(&maxLen); err != nil {
		t.Fatalf("回读列长度失败: %v", err)
	}
	if maxLen != 16 {
		t.Fatalf("Dry-run 泄漏：code 长度=%d（应仍为 16）", maxLen)
	}

	// 索引收敛：同名异构 → 报告 → 重建（验证 sys.indexes 读取与 DROP 语法）。
	if err := admin.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("建索引表失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec("DROP INDEX [IDX_mig_idx_order_grp] ON mig_idx_order"); err != nil {
		t.Fatalf("DROP 索引失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec("CREATE INDEX [IDX_mig_idx_order_grp] ON mig_idx_order (shop)"); err != nil {
		t.Fatalf("重建异构索引失败: %v", err)
	}
	mis, err := CheckIndexes(cfg, &migIdxOrder{})
	if err != nil {
		t.Fatalf("CheckIndexes 失败: %v", err)
	}
	if len(mis) != 1 || mis[0].Kind != "mismatched" {
		t.Fatalf("期望 1 条 mismatched，实际: %+v", mis)
	}
	rebuild, err := NewXormDriver(cfg, noopLog{t: t}, WithRebuildIndexes())
	if err != nil {
		t.Fatalf("建重建驱动失败: %v", err)
	}
	defer func() { _ = rebuild.Close() }()
	if err := rebuild.AutoMigrate(&migIdxOrder{}); err != nil {
		t.Fatalf("RebuildIndexes 迁移失败: %v", err)
	}
	mis, err = CheckIndexes(cfg, &migIdxOrder{})
	if err != nil || len(mis) != 0 {
		t.Fatalf("重建后仍有差异: %v %+v", err, mis)
	}
}
