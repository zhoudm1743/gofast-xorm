package xormdriver

import (
	"testing"

	"xorm.io/xorm/names"
)

// ── TableName() 接口识别专项（双驱动联合测试方案新增锁定项）──────────
//
// 锁定三层表名语义（全部依赖 xorm 原生 TableInfo 解析，驱动层不得破坏）：
//  1. names.TableName() 接口优先于 mapper 推导——定义了 TableName() 的模型，
//     AutoMigrate/读/写全链路都落在 TableName() 指定的表，而不是 SnakeMapper
//     推导名（struct 名逐字小写单数）；
//  2. TablePrefix mapper 只作用于 mapper 推导的表名，不得对 TableName()
//     返回值二次加前缀；
//  3. 显式 Table() 仍为最高优先级。
//
// 覆盖：默认 xorm 与 orm 两种 tag identifier、指针接收者、无 schema 的 dest
// 兜底路径。诱饵表捕获最危险的失败模式——不报错但静默写错/查错表。

// tnBizUser 业务模型（默认 identifier）：TableName() 返回复数表名。
type tnBizUser struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`
}

func (tnBizUser) TableName() string { return "tn_biz_users" }

// tnOrmBizUser 业务模型（orm identifier）：orm tag 列定义 + TableName()。
type tnOrmBizUser struct {
	ID   string `orm:"pk varchar(16) 'id'"`
	Name string `orm:"varchar(100) 'name'"`
}

func (tnOrmBizUser) TableName() string { return "tn_orm_biz_users" }

// tnPtrUser 指针接收者 TableName()：业务代码常见写法，须与值接收者同效。
type tnPtrUser struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`
}

func (m *tnPtrUser) TableName() string { return "tn_ptr_users" }

// tnPlainUser 无 TableName()：SnakeMapper 推导表名 tn_plain_user（对照模型）。
type tnPlainUser struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`
}

// tnMustExec 原生执行 DDL/DML（诱饵表准备）。
func tnMustExec(t *testing.T, drv *XormDriver, sql string) {
	t.Helper()
	if _, err := drv.engine.Exec(sql); err != nil {
		t.Fatalf("执行 %q 失败: %v", sql, err)
	}
}

// tnCount 原生计数（绕过 ORM 表名解析，作为物理真相锚点）。
func tnCount(t *testing.T, drv *XormDriver, table string) int64 {
	t.Helper()
	var n int64
	if err := drv.Query().Raw("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s 失败: %v", table, err)
	}
	return n
}

// tnTableExists 判断 sqlite_master 中表是否存在。
func tnTableExists(t *testing.T, drv *XormDriver, table string) bool {
	t.Helper()
	var n int64
	if err := drv.Query().
		Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).
		Scan(&n); err != nil {
		t.Fatalf("查 sqlite_master 失败: %v", err)
	}
	return n > 0
}

// TestTableName_InterfaceRecognized_DefaultIdentifier 默认 identifier：
// TableName() 模型全链路（AutoMigrate/Create/Find/First/Count/Update/Updates/
// Delete）落在 tn_biz_users；mapper 推导名 tn_biz_user 上放置诱饵行，
// 捕获"静默写错表/查错表"。
func TestTableName_InterfaceRecognized_DefaultIdentifier(t *testing.T) {
	drv := newXormTestDriver(t)
	// 诱饵表：SnakeMapper 推导名（struct 名 tnBizUser → tn_biz_user）。
	tnMustExec(t, drv, `CREATE TABLE tn_biz_user (id TEXT PRIMARY KEY, name TEXT)`)
	tnMustExec(t, drv, `INSERT INTO tn_biz_user VALUES ('d1', 'decoy')`)

	if err := drv.AutoMigrate(&tnBizUser{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	if !tnTableExists(t, drv, "tn_biz_users") {
		t.Fatal("AutoMigrate 应建 TableName() 指定的 tn_biz_users")
	}
	if tnTableExists(t, drv, "biz_tn_biz_users") {
		t.Fatal("不应创建带额外前缀的表")
	}

	// Create 落 TableName() 表，诱饵表行数不变。
	q := drv.Query()
	if err := q.Create(&tnBizUser{ID: "u1", Name: "alice"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := tnCount(t, drv, "tn_biz_users"); got != 1 {
		t.Fatalf("tn_biz_users 期望 1 行, 实际 %d", got)
	}
	if got := tnCount(t, drv, "tn_biz_user"); got != 1 {
		t.Fatalf("诱饵表 tn_biz_user 应保持 1 行, 实际 %d", got)
	}

	// Find/First 命中 TableName() 表且只读到本表数据。
	var rows []tnBizUser
	if err := drv.Query().Model(&tnBizUser{}).Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "alice" {
		t.Fatalf("Find 应读 tn_biz_users 的 alice, 实际 %+v", rows)
	}
	var first tnBizUser
	if err := drv.Query().Model(&tnBizUser{}).Where("id = ?", "u1").First(&first); err != nil {
		t.Fatalf("First: %v", err)
	}
	if first.Name != "alice" {
		t.Fatalf("First 回填异常: %+v", first)
	}

	// Count/Update/Updates 作用域一致。
	var n int64
	if err := drv.Query().Model(&tnBizUser{}).Count(&n); err != nil || n != 1 {
		t.Fatalf("Count 期望 1, 实际 %d err %v", n, err)
	}
	if err := drv.Query().Model(&tnBizUser{}).Where("id = ?", "u1").Update("name", "alice2"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := drv.Query().Model(&tnBizUser{}).Where("id = ?", "u1").Updates(map[string]any{"name": "alice3"}); err != nil {
		t.Fatalf("Updates: %v", err)
	}
	var after tnBizUser
	if err := drv.Query().Model(&tnBizUser{}).Where("id = ?", "u1").First(&after); err != nil {
		t.Fatalf("Update 后 First: %v", err)
	}
	if after.Name != "alice3" {
		t.Fatalf("Updates 未落 TableName() 表: %+v", after)
	}

	// 无 Model/Table 的 dest 兜底路径（buildOpts 不设表名，xorm 原生解析 dest）。
	var fallback []tnBizUser
	if err := drv.Query().Where("id = ?", "u1").Find(&fallback); err != nil {
		t.Fatalf("dest 兜底 Find: %v", err)
	}
	if len(fallback) != 1 || fallback[0].Name != "alice3" {
		t.Fatalf("dest 兜底应命中 tn_biz_users, 实际 %+v", fallback)
	}

	// Delete 后清空，诱饵表仍 1 行。
	if err := drv.Query().Delete(&tnBizUser{ID: "u1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := tnCount(t, drv, "tn_biz_users"); got != 0 {
		t.Fatalf("Delete 后 tn_biz_users 期望 0 行, 实际 %d", got)
	}
	if got := tnCount(t, drv, "tn_biz_user"); got != 1 {
		t.Fatalf("诱饵表不应被误删, 实际 %d 行", got)
	}

	// 显式 Table() 仍最高优先：dest 即 TableName() 模型本身，读诱饵表应得
	// 诱饵行——证明显式 Table() 覆盖 dest 的 TableName() 推导。
	var override []tnBizUser
	if err := drv.Query().Table("tn_biz_user").Find(&override); err != nil {
		t.Fatalf("Table 覆盖 Find: %v", err)
	}
	if len(override) != 1 || override[0].ID != "d1" || override[0].Name != "decoy" {
		t.Fatalf("显式 Table() 应覆盖 TableName(), 实际 %+v", override)
	}
}

// TestTableName_InterfaceRecognized_OrmIdentifier orm identifier（统一标签
// 体系）：TagIdentifier 切换后 TableName() 接口识别不受 tag 键名影响。
func TestTableName_InterfaceRecognized_OrmIdentifier(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	tnMustExec(t, drv, `CREATE TABLE tn_orm_biz_user (id TEXT PRIMARY KEY, name TEXT)`)
	tnMustExec(t, drv, `INSERT INTO tn_orm_biz_user VALUES ('d1', 'decoy')`)

	if err := drv.AutoMigrate(&tnOrmBizUser{}); err != nil {
		t.Fatalf("AutoMigrate(orm identifier) 失败: %v", err)
	}
	if !tnTableExists(t, drv, "tn_orm_biz_users") {
		t.Fatal("orm identifier 下 AutoMigrate 应建 TableName() 指定的表")
	}

	if err := drv.Query().Create(&tnOrmBizUser{ID: "u1", Name: "bob"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var rows []tnOrmBizUser
	if err := drv.Query().Model(&tnOrmBizUser{}).Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "bob" {
		t.Fatalf("orm identifier 下应命中 tn_orm_biz_users, 实际 %+v", rows)
	}
	if got := tnCount(t, drv, "tn_orm_biz_user"); got != 1 {
		t.Fatalf("诱饵表不应被触碰, 实际 %d 行", got)
	}
}

// TestTableName_PointerReceiver 指针接收者 TableName() 与值接收者同效。
func TestTableName_PointerReceiver(t *testing.T) {
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&tnPtrUser{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	if !tnTableExists(t, drv, "tn_ptr_users") {
		t.Fatal("指针接收者 TableName() 应被识别，建表 tn_ptr_users")
	}
	if err := drv.Query().Create(&tnPtrUser{ID: "p1", Name: "carol"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var rows []tnPtrUser
	if err := drv.Query().Model(&tnPtrUser{}).Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "carol" {
		t.Fatalf("指针接收者模型读写应落 tn_ptr_users, 实际 %+v", rows)
	}
}

// TestTableName_WithTablePrefix_NoDoublePrefix TablePrefix 与 TableName() 共存：
// 前缀 mapper 只作用于 mapper 推导名（tnPlainUser → biz_tn_plain_user），
// TableName() 返回值保持原样（tn_biz_users，不加前缀）。
func TestTableName_WithTablePrefix_NoDoublePrefix(t *testing.T) {
	drv := newXormTestDriver(t)
	drv.engine.SetTableMapper(names.NewPrefixMapper(names.SnakeMapper{}, "biz_"))
	drv.tablePrefix = "biz_"

	if err := drv.AutoMigrate(&tnBizUser{}, &tnPlainUser{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	if !tnTableExists(t, drv, "tn_biz_users") {
		t.Fatal("TableName() 表不应被加前缀，期望 tn_biz_users")
	}
	if tnTableExists(t, drv, "biz_tn_biz_users") {
		t.Fatal(" TableName() 返回值被二次加前缀（biz_tn_biz_users 不应存在）")
	}
	if !tnTableExists(t, drv, "biz_tn_plain_user") {
		t.Fatal("无 TableName() 模型应经前缀 mapper 建表 biz_tn_plain_user")
	}

	if err := drv.Query().Create(&tnBizUser{ID: "u1", Name: "dave"}); err != nil {
		t.Fatalf("Create(tabler 模型): %v", err)
	}
	if err := drv.Query().Create(&tnPlainUser{ID: "p1", Name: "erin"}); err != nil {
		t.Fatalf("Create(推导模型): %v", err)
	}
	if got := tnCount(t, drv, "tn_biz_users"); got != 1 {
		t.Fatalf("tn_biz_users 期望 1 行, 实际 %d", got)
	}
	if got := tnCount(t, drv, "biz_tn_plain_user"); got != 1 {
		t.Fatalf("biz_tn_plain_user 期望 1 行, 实际 %d", got)
	}
	var rows []tnBizUser
	if err := drv.Query().Model(&tnBizUser{}).Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "dave" {
		t.Fatalf("前缀配置下 TableName() 模型读取异常: %+v", rows)
	}
}
