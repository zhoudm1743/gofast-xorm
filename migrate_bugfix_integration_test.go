//go:build integration

package xormdriver

// migrate_bugfix_integration_test.go 固化 stitch-mes 反馈的四个阻塞 BUG 修复
//（需求文档 §7），依赖真实 PostgreSQL：设置 GOFAST_TEST_PG_DSN 后
// `go test -tags integration -run PGXormMigrateBUG ./`。
//   - BUG-1：MigrateSQL 引擎构造与 NewXormDriver 对齐（mapper/tag/schema），
//     无 GonicMapper 时列名回退 SnakeMapper 产生 user_i_d 假列；
//   - BUG-2：Sync2 对 gorm unique 建成的唯一约束发 DROP INDEX 报 2BP01 中断
//     迁移（含同列重复索引的 map 迭代随机竞态），预删 DROP CONSTRAINT 根治；
//   - BUG-3：Sync2 comment 分支对"库有注释、模型无"的列发 ModifyColumnSQL 清空
//     注释（假阳性 ALTER），中性化 + 收敛（仅新增/更新，绝不清空）；
//   - BUG-4：CheckIndexes 对 gorm 小写索引名（idx_*） vs xorm XName 大写前缀
//     （IDX_*）误报 missing，pg 侧大小写不敏感匹配。

import (
	"os"
	"strings"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── BUG-1 模型：GonicMapper 对齐（UserID → user_id，无显式列名） ────────

type migBug1Gonic struct {
	ID     string `orm:"pk varchar(16) 'id'"`
	UserID string `orm:"varchar(32) index"` // 无 'col' 名，列名完全由 mapper 决定
}

func (migBug1Gonic) TableName() string { return "mig_bug1_gonic" }

// ── BUG-2 模型：同列重复的 constraint + 普通唯一索引（stitch-mes wage_period 式） ──

type migBug2WP struct {
	ID        string `orm:"pk varchar(16) 'id'"`
	YearMonth string `orm:"varchar(8) notnull unique(uk_ym) 'year_month'"`
	Code      string `orm:"varchar(32) notnull unique(uk_code) 'code'"`
}

func (migBug2WP) TableName() string { return "mig_bug2_wp" }

// ── BUG-3 模型：注释中性化（模型无注释，库内有 legacy 注释） ─────────────

type migBug3Legacy struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(32) notnull 'code'"`
}

func (migBug3Legacy) TableName() string { return "mig_bug3_legacy" }

// migBug3LegacyCmt：同表加注释版本（收敛更新路径：模型有注释、库内是 legacy 值）。
type migBug3LegacyCmt struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(32) notnull comment('编码') 'code'"`
}

func (migBug3LegacyCmt) TableName() string { return "mig_bug3_legacy" }

// migBug3FreshCmt：新表带注释列（建表路径 comment 落库）。
type migBug3FreshCmt struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Memo string `orm:"varchar(100) null comment('备注') 'memo'"`
}

func (migBug3FreshCmt) TableName() string { return "mig_bug3_fresh" }

// ── BUG-4 模型：库内小写索引名 vs XName 大写前缀 ─────────────────────────

type migBug4Case struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Code string `orm:"varchar(32) index 'code'"`
}

func (migBug4Case) TableName() string { return "mig_bug4_case" }

// ── 基建 ─────────────────────────────────────────────────────────────

// pgBugCfg 构造带 schema 的连接配置（MigrateSQL / CheckIndexes 用）。
func pgBugCfg(t *testing.T, schema string) contracts.ConnectionConfig {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_PG_DSN 环境变量，跳过 pgsql 集成测试")
	}
	return contracts.ConnectionConfig{
		Driver: "xorm", Engine: "postgres", DSN: dsn,
		TagIdentifier: "orm", Schema: schema,
	}
}

// pgColComment 读取某列的库内注释（pg_description，不存在为空串）。
func pgColComment(t *testing.T, admin *XormDriver, schema, table, col string) string {
	t.Helper()
	row := pgQuery(t, admin,
		`SELECT col_description(c.oid, a.attnum) AS comment
		   FROM pg_class c
		   JOIN pg_namespace n ON c.relnamespace = n.oid
		   JOIN pg_attribute a ON a.attrelid = c.oid
		  WHERE n.nspname = ? AND c.relname = ? AND a.attname = ? AND a.attnum > 0`,
		schema, table, col)
	if row == nil {
		t.Fatalf("列 %s.%s.%s 不存在", schema, table, col)
	}
	return string(row["comment"])
}

// ── BUG-1：MigrateSQL 与 NewXormDriver 命名语义对齐 ──────────────────────

// TestPGXormMigrateBUG1_MigrateSQLMapperAligned 固化 §7 BUG-1：AutoMigrate 建表后
// MigrateSQL 必须产出零 DDL——捕获引擎漏配 GonicMapper 时 UserID 会被
// SnakeMapper 解析成 user_i_d 假列（每次预览都多出 ALTER TABLE ADD COLUMN）。
func TestPGXormMigrateBUG1_MigrateSQLMapperAligned(t *testing.T) {
	schema := "tenant_xorm_bug1"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migBug1Gonic{}); err != nil {
		t.Fatalf("AutoMigrate 建表失败: %v", err)
	}
	ddl, err := MigrateSQL(pgBugCfg(t, schema), &migBug1Gonic{})
	if err != nil {
		t.Fatalf("MigrateSQL 失败: %v", err)
	}
	for _, s := range ddl {
		if strings.Contains(s, "mig_bug1_gonic") {
			t.Fatalf("MigrateSQL 出现假阳性 DDL: %s", s)
		}
	}
	// 列名必须落在 GonicMapper 的 user_id 上（无 user_i_d 假列）。
	row := pgQuery(t, admin,
		`SELECT COUNT(*) AS n FROM information_schema.columns
		  WHERE table_schema = ? AND table_name = 'mig_bug1_gonic' AND column_name = 'user_id'`,
		schema)
	if string(row["n"]) != "1" {
		t.Fatalf("user_id 列不存在（GonicMapper 未生效）: %s", row["n"])
	}
}

// ── BUG-2：唯一约束预删（2BP01 根治） ────────────────────────────────────

// TestPGXormMigrateBUG2_UniqueConstraintPreDrop 固化 §7 BUG-2：库内同列并存的
// unique constraint（gorm unique 建成）+ 普通唯一索引，模型声明同列 unique——
// Sync2 的标记竞态随机对约束发 DROP INDEX（2BP01 中断整个迁移）。预删后
// AutoMigrate 必须确定性成功：约束消失、组内恰好保留一个唯一索引、模型新
// 声明的 uk_code 唯一索引被创建。重复执行验证幂等。
func TestPGXormMigrateBUG2_UniqueConstraintPreDrop(t *testing.T) {
	schema := "tenant_xorm_bug2"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	// 手工构造 gorm 式历史遗留：constraint + 同列普通唯一索引重复。
	prefix := `CREATE TABLE "` + schema + `".mig_bug2_wp (` +
		`id varchar(16) PRIMARY KEY, year_month varchar(8) NOT NULL, code varchar(32) NOT NULL)`
	if _, err := admin.RawEngine().Exec(prefix); err != nil {
		t.Fatalf("建基表失败: %v", err)
	}
	legacy := []string{
		`ALTER TABLE "` + schema + `".mig_bug2_wp ADD CONSTRAINT uni_mig_bug2_wp_ym UNIQUE (year_month)`,
		`CREATE UNIQUE INDEX idx_mig_bug2_wp_ym ON "` + schema + `".mig_bug2_wp (year_month)`,
	}
	for _, ddl := range legacy {
		if _, err := admin.RawEngine().Exec(ddl); err != nil {
			t.Fatalf("构造遗留结构失败 %q: %v", ddl, err)
		}
	}

	drv := newPGMigrateDriver(t, schema, true)
	for round := 1; round <= 2; round++ {
		if err := drv.AutoMigrate(&migBug2WP{}); err != nil {
			t.Fatalf("第 %d 轮 AutoMigrate 失败（2BP01 未根治）: %v", round, err)
		}
	}

	// 唯一约束（contype='u'）必须清零（uni_mig_bug2_wp_ym 已预删，xorm 建的是
	// UNIQUE INDEX 不是 constraint）。
	row := pgQuery(t, admin,
		`SELECT COUNT(*) AS n FROM pg_constraint c
		   JOIN pg_class r ON c.conrelid = r.oid
		   JOIN pg_namespace ns ON r.relnamespace = ns.oid
		  WHERE ns.nspname = ? AND r.relname = 'mig_bug2_wp' AND c.contype = 'u'`,
		schema)
	if string(row["n"]) != "0" {
		t.Fatalf("唯一约束未清零: %s", row["n"])
	}
	// year_month 组内恰好保留一个唯一索引。
	row = pgQuery(t, admin,
		`SELECT COUNT(*) AS n FROM pg_indexes
		  WHERE schemaname = ? AND tablename = 'mig_bug2_wp'
		    AND indexdef LIKE 'CREATE UNIQUE INDEX%(%year_month%)%'`,
		schema)
	if string(row["n"]) != "1" {
		t.Fatalf("year_month 唯一索引数量 != 1: %s", row["n"])
	}
	// 模型新声明的 uk_code 唯一索引已创建。
	row = pgQuery(t, admin,
		`SELECT COUNT(*) AS n FROM pg_indexes
		  WHERE schemaname = ? AND tablename = 'mig_bug2_wp'
		    AND indexdef LIKE 'CREATE UNIQUE INDEX%(code)%'`,
		schema)
	if string(row["n"]) != "1" {
		t.Fatalf("code 唯一索引未创建: %s", row["n"])
	}
}

// TestPGXormMigrateBUG2_MigrateSQLPreview 固化 §7 BUG-2 的 dry-run 侧：
// MigrateSQL 预览必须包含 DROP CONSTRAINT（而非崩溃或漏报），且重复预览
// 结果确定（幽灵索引 + 2BP01 重试不改变最终语句集）。
func TestPGXormMigrateBUG2_MigrateSQLPreview(t *testing.T) {
	schema := "tenant_xorm_bug2_preview"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	if _, err := admin.RawEngine().Exec(
		`CREATE TABLE "` + schema + `".mig_bug2_wp (id varchar(16) PRIMARY KEY, year_month varchar(8) NOT NULL, code varchar(32) NOT NULL)`); err != nil {
		t.Fatalf("建基表失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec(
		`ALTER TABLE "` + schema + `".mig_bug2_wp ADD CONSTRAINT uni_mig_bug2_wp_ym UNIQUE (year_month)`); err != nil {
		t.Fatalf("构造遗留约束失败: %v", err)
	}
	// 同列普通唯一索引重复（stitch-mes wage_period 式 race 场景）：模型匹配
	// 该签名但组内重复，预删计划必须确定性选中约束。
	if _, err := admin.RawEngine().Exec(
		`CREATE UNIQUE INDEX idx_mig_bug2_wp_ym ON "` + schema + `".mig_bug2_wp (year_month)`); err != nil {
		t.Fatalf("构造重复索引失败: %v", err)
	}

	cfg := pgBugCfg(t, schema)
	var first []string
	for round := 1; round <= 3; round++ {
		ddl, err := MigrateSQL(cfg, &migBug2WP{})
		if err != nil {
			t.Fatalf("第 %d 轮 MigrateSQL 失败: %v", round, err)
		}
		var hasDrop bool
		for _, s := range ddl {
			if strings.Contains(s, `DROP CONSTRAINT IF EXISTS "uni_mig_bug2_wp_ym"`) {
				hasDrop = true
			}
		}
		if !hasDrop {
			t.Fatalf("第 %d 轮预览缺少 DROP CONSTRAINT uni_mig_bug2_wp_ym: %v", round, ddl)
		}
		if round == 1 {
			first = ddl
		} else if strings.Join(first, "\n") != strings.Join(ddl, "\n") {
			t.Fatalf("第 %d 轮预览与第 1 轮不一致:\n%v\nvs\n%v", round, first, ddl)
		}
	}
}

// ── BUG-3：注释中性化 + 收敛（仅新增/更新，绝不清空） ─────────────────────

// TestPGXormMigrateBUG3_CommentNeutralize 固化 §7 BUG-3 防护侧：legacy 库
// （gorm/手写 SQL 建成，模型 orm tag 无 comment）上的已有列注释，AutoMigrate
// 后必须原样保留，且 MigrateSQL 不得产出该表的假阳性 ALTER（stitch-mes 实测
// 曾产生 544 条）。
func TestPGXormMigrateBUG3_CommentNeutralize(t *testing.T) {
	schema := "tenant_xorm_bug3"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migBug3Legacy{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 模拟 legacy：手工 SQL 加注释（模型无 comment）。
	if _, err := admin.RawEngine().Exec(
		`COMMENT ON COLUMN "` + schema + `".mig_bug3_legacy.code IS 'legacy comment'`); err != nil {
		t.Fatalf("加 legacy 注释失败: %v", err)
	}
	// AutoMigrate（模型仍无注释）：注释绝不清空。
	if err := drv.AutoMigrate(&migBug3Legacy{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	if got := pgColComment(t, admin, schema, "mig_bug3_legacy", "code"); got != "legacy comment" {
		t.Fatalf("legacy 注释被清空/篡改: %q", got)
	}
	// MigrateSQL：该表零假阳性 DDL。
	ddl, err := MigrateSQL(pgBugCfg(t, schema), &migBug3Legacy{})
	if err != nil {
		t.Fatalf("MigrateSQL 失败: %v", err)
	}
	for _, s := range ddl {
		if strings.Contains(s, "mig_bug3_legacy") {
			t.Fatalf("注释假阳性 DDL: %s", s)
		}
	}
}

// TestPGXormMigrateBUG3_CommentConverge 固化 §7 BUG-3 收敛侧：模型新增注释时
// AutoMigrate 后必须落库（legacy 值被更新，新表/新列直接带注释）。
func TestPGXormMigrateBUG3_CommentConverge(t *testing.T) {
	schema := "tenant_xorm_bug3_conv"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	drv := newPGMigrateDriver(t, schema, true)
	if err := drv.AutoMigrate(&migBug3Legacy{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := admin.RawEngine().Exec(
		`COMMENT ON COLUMN "` + schema + `".mig_bug3_legacy.code IS 'legacy comment'`); err != nil {
		t.Fatalf("加 legacy 注释失败: %v", err)
	}
	// 模型升级为带注释版本：收敛应把注释更新为模型值（而非 Sync2 的清空路径）。
	if err := drv.AutoMigrate(&migBug3LegacyCmt{}); err != nil {
		t.Fatalf("AutoMigrate（带注释版本）失败: %v", err)
	}
	if got := pgColComment(t, admin, schema, "mig_bug3_legacy", "code"); got != "编码" {
		t.Fatalf("注释未收敛为模型值: %q", got)
	}
	// 新表 comment 列建表即落注释。
	if err := drv.AutoMigrate(&migBug3FreshCmt{}); err != nil {
		t.Fatalf("AutoMigrate（新表）失败: %v", err)
	}
	if got := pgColComment(t, admin, schema, "mig_bug3_fresh", "memo"); got != "备注" {
		t.Fatalf("新表注释未落库: %q", got)
	}
}

// ── BUG-4：CheckIndexes 大小写不敏感 ─────────────────────────────────────

// TestPGXormMigrateBUG4_CheckIndexesCaseInsensitive 固化 §7 BUG-4：库内 gorm
// 式小写索引名（idx_*）与 xorm XName 大写前缀（IDX_*）必须匹配，不得误报
// missing（stitch-mes 实测曾误报 249 条）；MigrateSQL 侧 Sync2 的 Index.Equal
// 本就不比名字，同表应零 DDL。
func TestPGXormMigrateBUG4_CheckIndexesCaseInsensitive(t *testing.T) {
	schema := "tenant_xorm_bug4"
	admin := resetPGSchema(t, schema)
	defer func() { _ = admin.Close() }()

	if _, err := admin.RawEngine().Exec(
		`CREATE TABLE "` + schema + `".mig_bug4_case (id varchar(16) PRIMARY KEY, code varchar(32))`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// gorm 式小写索引名。
	if _, err := admin.RawEngine().Exec(
		`CREATE INDEX idx_mig_bug4_case_code ON "` + schema + `".mig_bug4_case (code)`); err != nil {
		t.Fatalf("建索引失败: %v", err)
	}

	mismatches, err := CheckIndexes(pgBugCfg(t, schema), &migBug4Case{})
	if err != nil {
		t.Fatalf("CheckIndexes 失败: %v", err)
	}
	for _, mm := range mismatches {
		if mm.Table == "mig_bug4_case" {
			t.Fatalf("大小写误报 missing: %+v", mm)
		}
	}
	ddl, err := MigrateSQL(pgBugCfg(t, schema), &migBug4Case{})
	if err != nil {
		t.Fatalf("MigrateSQL 失败: %v", err)
	}
	for _, s := range ddl {
		if strings.Contains(s, "mig_bug4_case") {
			t.Fatalf("同定义索引出现多余 DDL: %s", s)
		}
	}
}
