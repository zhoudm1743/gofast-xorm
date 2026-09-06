package xormdriver

import (
	"fmt"
	"strings"
	"testing"
)

// ── orm tag 全链路测试模型（identifier=orm 连接，U5/U6b/U7）────────────
//
// 全部列名显式书写（orm tag 强制单引号列名），DDL 断言直接对照 sqlite_schema。

// ormAccount 全功能 orm tag 模型：主键/自定义列名/非空/唯一/默认值/索引/
// created/updated 填充。
type ormAccount struct {
	ID        string `orm:"pk varchar(16) 'id'"`
	Name      string `orm:"varchar(100) 'name' notnull"`
	Email     string `orm:"varchar(120) 'email_addr' unique"`
	Amount    int    `orm:"default(0)"`
	Remark    string `orm:"varchar(100) 'remark' index"`
	CreatedAt int64  `orm:"created 'created_at'"`
	UpdatedAt int64  `orm:"updated 'updated_at'"`
}

// sqliteTableDDL 读 sqlite_schema 中建表 SQL（列名/类型/非空/默认值/唯一断言用）。
func sqliteTableDDL(t *testing.T, drv *XormDriver, table string) string {
	t.Helper()
	var ddl string
	if err := drv.Query().Raw("SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&ddl); err != nil {
		t.Fatalf("读取 %s 建表 DDL 失败: %v", table, err)
	}
	return ddl
}

// sqliteTableIndexes 读表上的索引名列表（索引断言用）。
func sqliteTableIndexes(t *testing.T, drv *XormDriver, table string) []string {
	t.Helper()
	var names []string
	if err := drv.Query().Raw("SELECT name FROM sqlite_schema WHERE type = 'index' AND tbl_name = ?", table).Scan(&names); err != nil {
		t.Fatalf("读取 %s 索引失败: %v", table, err)
	}
	return names
}

// TestOrmTagDDLWithIdentifierOrm identifier=orm 连接下 orm tag 原生驱动 DDL：
// 自定义列名/主键/非空/默认值/唯一/索引全部按 orm tag 落地。
// 说明：sqlite 方言把 VARCHAR 映射为 TEXT、把 unique 落成独立唯一索引
// （UQE_ 前缀），断言按方言实际产物对照。
func TestOrmTagDDLWithIdentifierOrm(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&ormAccount{}); err != nil {
		t.Fatalf("AutoMigrate(orm tag 模型): %v", err)
	}
	ddl := sqliteTableDDL(t, drv, "orm_account")
	// 表名：SnakeMapper 单数推导
	if !strings.Contains(ddl, "orm_account") {
		t.Fatalf("DDL 缺少表名 orm_account: %s", ddl)
	}
	// 自定义列名全部落地（email_addr 为 orm tag 声明列名，约定推导应为 email）
	for _, col := range []string{"`id`", "`name`", "`email_addr`", "`amount`", "`remark`", "`created_at`", "`updated_at`"} {
		if !strings.Contains(ddl, col) {
			t.Fatalf("DDL 缺少列 %s: %s", col, ddl)
		}
	}
	// 非空约束（notnull token）与默认值（default(0)）
	if !strings.Contains(ddl, "NOT NULL") {
		t.Fatalf("DDL 缺少 NOT NULL（name notnull）: %s", ddl)
	}
	if !strings.Contains(ddl, "DEFAULT") {
		t.Fatalf("DDL 缺少 DEFAULT（amount default(0)）: %s", ddl)
	}

	// PRAGMA 结构化断言：主键标记/非空标记/默认值
	var cols []map[string]any
	if err := drv.Query().Raw("PRAGMA table_info(orm_account)").ScanMap(&cols); err != nil {
		t.Fatalf("PRAGMA table_info 失败: %v", err)
	}
	byName := make(map[string]map[string]any, len(cols))
	for _, c := range cols {
		byName[fmt.Sprint(c["name"])] = c
	}
	if c, ok := byName["id"]; !ok || fmt.Sprint(c["pk"]) != "1" {
		t.Fatalf("id 应为主键: %v", c)
	}
	if c, ok := byName["name"]; !ok || fmt.Sprint(c["notnull"]) != "1" {
		t.Fatalf("name 应为非空: %v", c)
	}
	if c, ok := byName["amount"]; !ok || !strings.Contains(fmt.Sprint(c["dflt_value"]), "0") {
		t.Fatalf("amount 默认值应为 0: %v", c)
	}
	if _, ok := byName["email_addr"]; !ok {
		t.Fatalf("email_addr 列缺失: %v", cols)
	}

	// 索引（remark index → IDX_ 前缀）与唯一（email unique → UQE_ 前缀唯一索引）
	indexes := sqliteTableIndexes(t, drv, "orm_account")
	joined := strings.Join(indexes, ",")
	if !strings.Contains(joined, "IDX") || !strings.Contains(joined, "orm_account") {
		t.Fatalf("表 orm_account 上未发现 xorm 命名索引（remark index）: %v", indexes)
	}
	if !strings.Contains(joined, "UQE") {
		t.Fatalf("表 orm_account 上未发现唯一索引（email unique）: %v", indexes)
	}
}

// TestOrmTagFullChainCRUD identifier=orm 连接下 orm tag 模型完整 CRUD：
// 插入、自定义列名查询、更新、created/updated 填充、删除。
func TestOrmTagFullChainCRUD(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&ormAccount{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	q := drv.Query()

	// Create：created/updated 由 xorm 原生填充
	m := &ormAccount{ID: "a1", Name: "alice", Email: "alice@go.dev", Amount: 3, Remark: "r1"}
	if err := q.Create(m); err != nil {
		t.Fatalf("Create(orm 模型): %v", err)
	}
	if m.CreatedAt <= 0 || m.UpdatedAt <= 0 {
		t.Fatalf("created/updated 未被填充: %+v", m)
	}

	// 自定义列名查询（email_addr 为 orm tag 声明的列名）
	var got ormAccount
	if err := q.Model(&ormAccount{}).Where("email_addr = ?", "alice@go.dev").Take(&got); err != nil {
		t.Fatalf("按自定义列名查询失败: %v", err)
	}
	if got.ID != "a1" || got.Name != "alice" || got.Amount != 3 {
		t.Fatalf("查询结果错误: %+v", got)
	}

	// Update：updated 继续填充（时间戳秒级粒度，断言不回退）
	prevUpdated := got.UpdatedAt
	got.Amount = 9
	if err := q.Save(&got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got.UpdatedAt < prevUpdated {
		t.Fatalf("updated 时间戳回退: before=%d after=%d", prevUpdated, got.UpdatedAt)
	}
	var after ormAccount
	if err := q.Model(&ormAccount{}).Where("email_addr = ?", "alice@go.dev").Take(&after); err != nil {
		t.Fatalf("更新后查询失败: %v", err)
	}
	if after.Amount != 9 {
		t.Fatalf("更新未落库: %+v", after)
	}

	// Delete
	if res := q.Model(&ormAccount{}).Where("id = ?", "a1").DeleteResult(&ormAccount{}); res.Error != nil {
		t.Fatalf("DeleteResult: %v", res.Error)
	}
	if n := countRaw(t, drv, "SELECT COUNT(*) FROM orm_account"); n != 0 {
		t.Fatalf("删除后应剩 0 行，实际 %d", n)
	}
}

// ── 迁移期陷阱告警（10.3）与双 tag 并存 ──────────────────────────────

// trapOrmModel 纯 orm tag 模型（identifier=xorm 连接下 orm tag 全部失效）。
type trapOrmModel struct {
	ID    string `orm:"pk varchar(16) 'id'"`
	Email string `orm:"varchar(120) 'mail_addr'"`
}

// TestOrmTagTrapWarnWhenIdentifierXorm identifier 仍为 xorm 而模型带 orm tag：
// 启动 Warn 告警（不报错），且 orm tag 确实未被读取——列名走约定推导而非
// tag 声明的 mail_addr。
func TestOrmTagTrapWarnWhenIdentifierXorm(t *testing.T) {
	drv, log := newXormTestDriverWithIdentifier(t, "xorm")
	if err := drv.AutoMigrate(&trapOrmModel{}); err != nil {
		t.Fatalf("迁移期陷阱只告警不中断，期望 AutoMigrate 成功: %v", err)
	}
	if !log.has("warn", "迁移期陷阱") {
		t.Fatalf("未输出迁移期陷阱 Warn 告警: %v", log.lines["warn"])
	}
	if !log.has("warn", "trapOrmModel") {
		t.Fatalf("陷阱告警未包含模型名 trapOrmModel: %v", log.lines["warn"])
	}
	// xorm 读不到 orm tag：列名按 mapper 约定为 email，而非 tag 的 mail_addr
	ddl := sqliteTableDDL(t, drv, "trap_orm_model")
	if !strings.Contains(ddl, "email") {
		t.Fatalf("identifier=xorm 下应按约定推导列名 email: %s", ddl)
	}
	if strings.Contains(ddl, "mail_addr") {
		t.Fatalf("identifier=xorm 下 orm tag 列名 mail_addr 不应生效: %s", ddl)
	}
}

// dualTagModel orm 与 xorm 双 tag 并存且值不同：identifier=orm 时仅 orm 生效。
type dualTagModel struct {
	ID   string `orm:"pk varchar(16) 'orm_id'" xorm:"pk varchar(16) 'xorm_id'"`
	Name string `orm:"varchar(50) 'orm_name'" xorm:"varchar(50) 'xorm_name'"`
}

// TestDualTagOnlyOrmEffective identifier=orm 连接下双 tag 并存：仅 orm tag 生效
// （DDL 断言），xorm tag 同连接失效。
func TestDualTagOnlyOrmEffective(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&dualTagModel{}); err != nil {
		t.Fatalf("AutoMigrate(双 tag 模型): %v", err)
	}
	ddl := sqliteTableDDL(t, drv, "dual_tag_model")
	if !strings.Contains(ddl, "orm_id") || !strings.Contains(ddl, "orm_name") {
		t.Fatalf("identifier=orm 下 orm tag 列名应生效: %s", ddl)
	}
	if strings.Contains(ddl, "xorm_id") || strings.Contains(ddl, "xorm_name") {
		t.Fatalf("identifier=orm 下 xorm tag 不应生效: %s", ddl)
	}
}

// ── 启动期校验：禁用 token / 未知裸 token（8.4）───────────────────────

// badDeletedModel 禁用 token deleted（与框架业务级软删除冲突）。
type badDeletedModel struct {
	ID        string `orm:"pk varchar(16) 'id'"`
	DeletedAt int64  `orm:"deleted"`
}

// badCacheModel 禁用 token cache（与框架查询缓存体系冲突）。
type badCacheModel struct {
	ID    string `orm:"pk varchar(16) 'id'"`
	Extra string `orm:"cache"`
}

// badUnknownTokenModel 未知裸 token（xorm 会静默建垃圾列 serializer:json）。
type badUnknownTokenModel struct {
	ID      string `orm:"pk varchar(16) 'id'"`
	Payload string `orm:"serializer:json"`
}

// TestOrmTagAutoMigrateRejectsBadTags 禁用 token/未知裸 token 在 AutoMigrate
// 即报错（启动即失败，不等 xorm 静默建列），错误含字段名与替代方案。
func TestOrmTagAutoMigrateRejectsBadTags(t *testing.T) {
	cases := []struct {
		name     string
		model    any
		contains []string
	}{
		{"禁用 token deleted", &badDeletedModel{}, []string{"DeletedAt", "deleted", "OnlyTrashed"}},
		{"禁用 token cache", &badCacheModel{}, []string{"Extra", "cache", "Query().Cache()"}},
		{"未知裸 token", &badUnknownTokenModel{}, []string{"Payload", "serializer:json", "单引号"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 直接构造驱动（log 为 nil）：校验错误路径不依赖日志器
			drv := newXormTestDriver(t)
			err := drv.AutoMigrate(tc.model)
			if err == nil {
				t.Fatalf("AutoMigrate 应失败，实际成功")
			}
			for _, frag := range tc.contains {
				if !strings.Contains(err.Error(), frag) {
					t.Fatalf("错误信息缺少 %q: %v", frag, err)
				}
			}
		})
	}
}

// ── ext 降级（8.4）：Warn 绝不报错，行为差异固化 ─────────────────────

// extCheckModel check 约束在 xorm 连接下降级为日志告警（SQLite 无 CHECK 生效）。
type extCheckModel struct {
	ID     string `orm:"pk varchar(16) 'id'"`
	Amount int    `orm:"default(0)" ext:"check:amount >= 0"`
}

// TestOrmTagExtCheckDegradesWithWarn orm 模型带 ext:"check:..."：启动成功 +
// Warn 日志 + 读写正常（SQLite 不生效 CHECK，写入非法值成功——差异固化）。
func TestOrmTagExtCheckDegradesWithWarn(t *testing.T) {
	drv, log := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&extCheckModel{}); err != nil {
		t.Fatalf("ext 降级不应报错: %v", err)
	}
	if !log.has("warn", "check") {
		t.Fatalf("未输出 ext check 降级 Warn: %v", log.lines["warn"])
	}
	if !log.has("warn", "extCheckModel") {
		t.Fatalf("降级 Warn 未包含模型名 extCheckModel: %v", log.lines["warn"])
	}
	q := drv.Query()
	// 正常写入读取
	ok := &extCheckModel{ID: "e1", Amount: 10}
	if err := q.Create(ok); err != nil {
		t.Fatalf("Create(合法值): %v", err)
	}
	// CHECK 不生效：写入非法负值成功（与 gormdriver 的行为差异以日志与测试固化）
	bad := &extCheckModel{ID: "e2", Amount: -5}
	if err := q.Create(bad); err != nil {
		t.Fatalf("CHECK 降级后写入非法值应成功: %v", err)
	}
	var got extCheckModel
	if err := q.Model(&extCheckModel{}).Where("id = ?", "e2").Take(&got); err != nil {
		t.Fatalf("读取非法值行失败: %v", err)
	}
	if got.Amount != -5 {
		t.Fatalf("非法值应原样落库，实际 %+v", got)
	}
}
