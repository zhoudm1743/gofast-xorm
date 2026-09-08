package xormdriver

// migrate_test.go 迁移能力的单元测试（sqlite 内存/文件库，无 PG 依赖）。
// PG 相关行为（0A000 场景、DDL 等价性、MigrateSQL 产物、索引收敛）固化在
// migrate_integration_test.go（-tags integration，GOFAST_TEST_PG_DSN 门控）。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// ── migrateSafeDSN ────────────────────────────────────────────────────

func TestMigrateSafeDSN_Keyword(t *testing.T) {
	dsn := "host=localhost port=5432 user=u password=p dbname=d sslmode=disable"
	got := migrateSafeDSN(dsn)
	if !strings.HasSuffix(got, " default_query_exec_mode=exec") {
		t.Fatalf("keyword DSN 未追加 exec 参数: %q", got)
	}
	if !strings.Contains(got, "host=localhost") {
		t.Fatalf("keyword DSN 原有参数丢失: %q", got)
	}
}

func TestMigrateSafeDSN_KeywordReplaceExisting(t *testing.T) {
	dsn := "host=h dbname=d default_query_exec_mode=cache_statement"
	got := migrateSafeDSN(dsn)
	if strings.Count(got, "default_query_exec_mode") != 1 {
		t.Fatalf("既有 default_query_exec_mode 未被替换为单一实例: %q", got)
	}
	if !strings.Contains(got, "default_query_exec_mode=exec") {
		t.Fatalf("未重写为 exec: %q", got)
	}
}

func TestMigrateSafeDSN_URL(t *testing.T) {
	got := migrateSafeDSN("postgres://u:p@localhost:5432/d?sslmode=disable")
	if !strings.Contains(got, "default_query_exec_mode=exec") {
		t.Fatalf("URL DSN 未设置 exec query 参数: %q", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("URL DSN 原有参数丢失: %q", got)
	}
}

func TestMigrateSafeDSN_Idempotent(t *testing.T) {
	once := migrateSafeDSN("host=h dbname=d")
	if twice := migrateSafeDSN(once); twice != once {
		t.Fatalf("幂等性破坏: 一次=%q 两次=%q", once, twice)
	}
}

// ── 非 PG 引擎的选项降级 ──────────────────────────────────────────────

func TestWithMigrateSafe_NonPGNoop(t *testing.T) {
	log := newCaptureLog()
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver: "xorm", Engine: "sqlite",
	}, log, WithMigrateSafe(), WithRebuildIndexes())
	if err != nil {
		t.Fatalf("非 PG 引擎 + WithMigrateSafe 不应失败: %v", err)
	}
	defer func() { _ = drv.Close() }()
	if !log.has("info", "WithMigrateSafe 仅支持 postgres") {
		t.Fatalf("非 PG 引擎未记录降级 Info 日志")
	}
}

// ── Migrate 一站式函数（sqlite 冒烟）───────────────────────────────────

// migrateSmokeModel 冒烟模型：xorm tag 显式列定义。
type migrateSmokeModel struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`
}

func TestMigrate_Sqlite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate_smoke.db")
	cfg := contracts.ConnectionConfig{Driver: "xorm", Engine: "sqlite", Database: dbPath}
	if err := Migrate(cfg, &migrateSmokeModel{}); err != nil {
		t.Fatalf("Migrate 失败: %v", err)
	}
	// 独立引擎验证表已建出。
	engine, err := xorm.NewEngine("sqlite", dbPath)
	if err != nil {
		t.Fatalf("回读建引擎失败: %v", err)
	}
	defer func() { _ = engine.Close() }()
	exist, err := engine.IsTableExist(&migrateSmokeModel{})
	if err != nil || !exist {
		t.Fatalf("Migrate 后表不存在: exist=%v err=%v", exist, err)
	}
}

// ── 非 PG 引擎的 R4 API 报错 ──────────────────────────────────────────

func TestMigrateSQL_UnsupportedEngine(t *testing.T) {
	_, err := MigrateSQL(contracts.ConnectionConfig{Driver: "xorm", Engine: "sqlite"}, &migrateSmokeModel{})
	if err == nil || !strings.Contains(err.Error(), "不支持的引擎") {
		t.Fatalf("MigrateSQL 不支持引擎应报明确错误, got: %v", err)
	}
}

func TestCheckIndexes_UnsupportedEngine(t *testing.T) {
	_, err := CheckIndexes(contracts.ConnectionConfig{Driver: "xorm", Engine: "sqlite"}, &migrateSmokeModel{})
	if err == nil || !strings.Contains(err.Error(), "不支持的引擎") {
		t.Fatalf("CheckIndexes 不支持引擎应报明确错误, got: %v", err)
	}
}

// ── rewriteMySQLDatabase ──────────────────────────────────────────────

func TestRewriteMySQLDatabase(t *testing.T) {
	cases := []struct{ dsn, db, want string }{
		{"root:p@tcp(192.168.1.1:3306)/app?charset=utf8mb4&parseTime=True", "tmp1",
			"root:p@tcp(192.168.1.1:3306)/tmp1?charset=utf8mb4&parseTime=True"},
		{"root:p@tcp(192.168.1.1:3306)/app", "tmp1", "root:p@tcp(192.168.1.1:3306)/tmp1"},
		{"root:p@tcp(192.168.1.1:3306)/", "tmp1", "root:p@tcp(192.168.1.1:3306)/tmp1"},
		{"root:p@tcp(192.168.1.1:3306)", "tmp1", "root:p@tcp(192.168.1.1:3306)/tmp1"},
	}
	for _, c := range cases {
		if got := rewriteMySQLDatabase(c.dsn, c.db); got != c.want {
			t.Fatalf("rewriteMySQLDatabase(%q, %q) = %q, want %q", c.dsn, c.db, got, c.want)
		}
	}
}

// ── isDDL 过滤 ────────────────────────────────────────────────────────

func TestIsDDL(t *testing.T) {
	cases := map[string]bool{
		"CREATE TABLE foo (id bigint)":          true,
		"  ALTER TABLE foo ALTER COLUMN a TYPE": true,
		"alter table foo add column b int":      true,
		"DROP INDEX IF EXISTS idx_foo":          true,
		"SELECT indexname FROM pg_indexes":      false,
		"INSERT INTO foo VALUES ($1)":           false,
		"SET LOCAL search_path TO \"tenant1\"":  false,
		"(SELECT 1)":                            false,
	}
	for q, want := range cases {
		if got := isDDL(q); got != want {
			t.Fatalf("isDDL(%q) = %v, want %v", q, got, want)
		}
	}
}

// ── rewriteQuestionMarks ───────────────────────────────────────────────

func TestRewriteQuestionMarks(t *testing.T) {
	cases := []struct {
		in   string
		want string
		n    int
	}{
		{"SELECT a FROM t WHERE b = ? AND c = ?", "SELECT a FROM t WHERE b = @p1 AND c = @p2", 2},
		{"SELECT '?' AS q, a FROM t WHERE b = ?", "SELECT '?' AS q, a FROM t WHERE b = @p1", 1},
		{"SELECT 'it''s ?' AS q WHERE a = ?", "SELECT 'it''s ?' AS q WHERE a = @p1", 1},
		{"SELECT [a?b] FROM t WHERE c = ?", "SELECT [a?b] FROM t WHERE c = @p1", 1},
		{"SELECT \"a?b\" FROM t WHERE c = ?", "SELECT \"a?b\" FROM t WHERE c = @p1", 1},
		{"SELECT a FROM t -- 注释 ?\nWHERE b = ?", "SELECT a FROM t -- 注释 ?\nWHERE b = @p1", 1},
		{"SELECT a FROM t /* 块 ? 注释 */ WHERE b = ?", "SELECT a FROM t /* 块 ? 注释 */ WHERE b = @p1", 1},
		{"SELECT 1", "SELECT 1", 0},
	}
	for _, c := range cases {
		got, n := rewriteQuestionMarks(c.in)
		if got != c.want || n != c.n {
			t.Fatalf("rewriteQuestionMarks(%q) = (%q, %d), want (%q, %d)", c.in, got, n, c.want, c.n)
		}
	}
}

// ── parseIndexDefCols ─────────────────────────────────────────────────

func TestParseIndexDefCols(t *testing.T) {
	cases := []struct {
		def  string
		want []string
	}{
		{`CREATE INDEX idx_a ON public.users USING btree (name)`, []string{"name"}},
		{`CREATE UNIQUE INDEX uk_a ON s.t USING btree ("tenant_id", "code" DESC)`, []string{"tenant_id", "code"}},
		{`CREATE INDEX idx_x ON t USING gin (payload jsonb_path_ops)`, []string{"payload"}},
	}
	for _, c := range cases {
		got := parseIndexDefCols(c.def)
		if !equalCols(got, c.want) {
			t.Fatalf("parseIndexDefCols(%q) = %v, want %v", c.def, got, c.want)
		}
	}
}
