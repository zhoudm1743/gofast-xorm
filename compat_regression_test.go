// compat_regression_test.go —— v0.8.1 兼容性修复回归（stitch-mes 全链路测试报告 2026-09-06）：
// X-02 GORM 兼容列名、X-03 Save 主键 FieldIndex 定位、X-04 Save upsert 回落、
// X-05 IN 切片展开、X-06 Count 剥离 ORDER BY、X-07 Preload belongs-to 方向判定。
package xormdriver

import (
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/database"

	"xorm.io/xorm/names"
)

// ── X-05：IN ?/IN (?) 切片参数展开 ───────────────────────────────────

func TestXormCompat_InSliceExpansion(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for _, id := range []string{"in1", "in2", "in3"} {
		if err := q.Create(&XormTestModel{ID: id, Name: "n-" + id}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	// gorm 形态 IN ?（切片自动展开）
	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("id IN ?", []string{"in1", "in3"}).Find(&rows); err != nil {
		t.Fatalf("IN ? 展开: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("IN ? 期望命中 2 行, 实际 %d", len(rows))
	}

	// IN (?) 形态同样展开
	rows = nil
	if err := q.Model(&XormTestModel{}).Where("id IN (?)", []string{"in2"}).Find(&rows); err != nil {
		t.Fatalf("IN (?) 展开: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "in2" {
		t.Errorf("IN (?) 期望命中 in2, 实际 %+v", rows)
	}

	// 空切片 → IN (NULL)（恒假），不报错返回 0 行
	rows = nil
	if err := q.Model(&XormTestModel{}).Where("id IN ?", []string{}).Find(&rows); err != nil {
		t.Fatalf("空切片 IN 展开: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("空切片 IN 期望 0 行, 实际 %d", len(rows))
	}

	// NOT IN ? 同样展开（正则词边界覆盖 NOT IN）
	rows = nil
	if err := q.Model(&XormTestModel{}).Where("id NOT IN ?", []string{"in1", "in2", "in3"}).Find(&rows); err != nil {
		t.Fatalf("NOT IN ? 展开: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("NOT IN 全排除期望 0 行, 实际 %d", len(rows))
	}

	// 非切片参数不受影响（占位符原样）
	rows = nil
	if err := q.Model(&XormTestModel{}).Where("id = ?", "in1").Find(&rows); err != nil {
		t.Fatalf("普通占位符: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("普通占位符期望 1 行, 实际 %d", len(rows))
	}
}

// TestXormCompat_InExpansionAfterPlainPlaceholder 生产缺陷回归（0.8.1 首版）：
// "type_code = ? AND value IN ?" 中 IN 前还有普通 ?，首版只扫描 IN 占位符
// 导致参数索引错位（IN 绑定到前一个参数，切片原样绑定为 $2 → PG 42601）。
func TestXormCompat_InExpansionAfterPlainPlaceholder(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for _, id := range []string{"m1", "m2", "m3"} {
		name := map[string]string{"m1": "a", "m2": "b", "m3": "a"}[id]
		if err := q.Create(&XormTestModel{ID: id, Name: name}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// 报告同款：普通 ? 在前 + IN ? 在后
	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).
		Where("name = ? AND id IN ?", "a", []string{"m1", "m3"}).Find(&rows); err != nil {
		t.Fatalf("普通 ? + IN ?: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("期望命中 2 行, 实际 %d", len(rows))
	}

	// IN (?) 括号形态混在中间：IN (?) + 尾部普通 ?
	rows = nil
	if err := q.Model(&XormTestModel{}).
		Where("id IN (?) AND name = ?", []string{"m1", "m2"}, "b").Find(&rows); err != nil {
		t.Fatalf("IN (?) + 尾部普通 ?: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "m2" {
		t.Errorf("期望命中 m2, 实际 %+v", rows)
	}

	// 双 IN：两个切片各自展开
	rows = nil
	if err := q.Model(&XormTestModel{}).
		Where("id IN ? AND name IN ?", []string{"m1"}, []string{"a", "b"}).Find(&rows); err != nil {
		t.Fatalf("双 IN: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "m1" {
		t.Errorf("双 IN 期望命中 m1, 实际 %+v", rows)
	}

	// 前置普通 ? 的空切片：IN (NULL) 恒假
	rows = nil
	if err := q.Model(&XormTestModel{}).
		Where("name = ? AND id IN ?", "a", []string{}).Find(&rows); err != nil {
		t.Fatalf("前置普通 ? 的空切片: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("空切片期望 0 行, 实际 %d", len(rows))
	}
}

// ── X-06：Count 剥离链上 ORDER BY ────────────────────────────────────

func TestXormCompat_CountStripsOrder(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for _, id := range []string{"c1", "c2", "c3"} {
		if err := q.Create(&XormTestModel{ID: id, Name: "n"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Order 先于 Count（gormdriver 下合法，xorm 驱动对齐：PG 严格模式 42803）
	var total int64
	if err := q.Model(&XormTestModel{}).Order("created_at DESC").Count(&total); err != nil {
		t.Fatalf("Order 后 Count 应剥离排序不报错: %v", err)
	}
	if total != 3 {
		t.Errorf("Count 期望 3, 实际 %d", total)
	}
	// Count 之后的链（不可变副本）Find 仍保留 Order 语义
	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Order("id DESC").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 3 || rows[0].ID != "c3" {
		t.Errorf("Order 后 Find 期望首行 c3, 实际 %+v", rows)
	}
}

// ── X-01/X-03：extends 嵌入模型 + Save 主键 FieldIndex 定位 ──────────

// XormExtModel 嵌入框架 ModelWithSoftDelete（内部自带 extends，业务侧单 tag 即展开）
type XormExtModel struct {
	database.ModelWithSoftDelete `xorm:"extends"`
	OrderNo                      string `xorm:"varchar(32) 'order_no'"`
}

func TestXormCompat_ExtendsModelCRUDAndSave(t *testing.T) {
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&XormExtModel{}); err != nil {
		t.Fatalf("AutoMigrate(extends 模型): %v", err)
	}
	q := drv.Query()

	// Create：嵌入 Model 的 AutoGenerateID 生成主键（框架钩子对 extends 生效）
	m := &XormExtModel{OrderNo: "EX-1"}
	if err := q.Create(m); err != nil {
		t.Fatalf("Create(extends 模型): %v", err)
	}
	if m.ID == "" {
		t.Fatal("extends 嵌入的 Model.ID 应被 AutoGenerateID 填充")
	}

	// 软删除列存在且默认 0（嵌套 extends 展开）
	var got XormExtModel
	if err := q.Model(&XormExtModel{}).Where("order_no = ?", "EX-1").First(&got); err != nil {
		t.Fatalf("First(extends 模型): %v", err)
	}
	if got.ID != m.ID || got.DeletedAt != 0 {
		t.Errorf("回读不符: id=%q deleted_at=%d", got.ID, got.DeletedAt)
	}

	// Save 更新路径：主键经 FieldIndex 定位（X-03，FieldName 为 "Model.ID" 点分
	// 路径时 FieldByName 恒判零值会误走 INSERT 报主键冲突）
	got.OrderNo = "EX-2"
	if err := q.Save(&got); err != nil {
		t.Fatalf("Save(extends 模型): %v", err)
	}
	var after XormExtModel
	if err := q.Model(&XormExtModel{}).Where("order_no = ?", "EX-2").First(&after); err != nil {
		t.Fatalf("Save 后查询: %v", err)
	}
	if after.ID != m.ID {
		t.Errorf("Save 更新应保持主键不变, 期望 %q 实际 %q", m.ID, after.ID)
	}

	// Save upsert 回落（X-04）：不存在的主键 0 行 → INSERT
	fresh := &XormExtModel{}
	fresh.ID = "ext-fresh"
	fresh.OrderNo = "EX-3"
	if err := q.Save(fresh); err != nil {
		t.Fatalf("Save(不存在行应回落插入): %v", err)
	}
	var n int64
	if err := q.Model(&XormExtModel{}).Count(&n); err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("回落插入后期望 2 行, 实际 %d", n)
	}
}

// ── X-07：Preload belongs-to 方向自动判定 ────────────────────────────

type compatDept struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`
}

type compatWorker struct {
	ID     string      `xorm:"pk varchar(16) 'id'"`
	Name   string      `xorm:"varchar(64) 'name'"`
	DeptID string      `xorm:"varchar(16) 'dept_id'"`
	Dept   *compatDept `xorm:"-" gorm:"foreignKey:DeptID"`
}

func TestXormCompat_PreloadBelongsTo(t *testing.T) {
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&compatDept{}, &compatWorker{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	q := drv.Query()
	if err := q.Create(&compatDept{ID: "d1", Name: "裁剪部"}); err != nil {
		t.Fatalf("Create dept: %v", err)
	}
	if err := q.Create(&compatWorker{ID: "w1", Name: "张三", DeptID: "d1"}); err != nil {
		t.Fatalf("Create worker: %v", err)
	}

	// belongs-to：foreignKey DeptID 在父行（workers）上，而非子表（depts）
	var workers []compatWorker
	if err := q.Model(&compatWorker{}).Preload("Dept").Find(&workers); err != nil {
		t.Fatalf("Preload(belongs-to): %v", err)
	}
	if len(workers) != 1 || workers[0].Dept == nil || workers[0].Dept.Name != "裁剪部" {
		t.Errorf("belongs-to 回填不符: %+v", workers)
	}
}

// ── X-02：GORM 兼容列名映射 ──────────────────────────────────────────

func TestXormCompat_GonicColumnMapper(t *testing.T) {
	// xorm 原生 GonicMapper：默认列名映射（X-02）。常用缩写整体映射且缩写表
	// 比 GORM 更全（PID→pid，GORM 推导 p_id，反而与报告中的存量列名一致）。
	m := names.GonicMapper{}
	cases := map[string]string{
		"DeptID": "dept_id",
		"PID":    "pid",
		"ID":     "id",
		"UserID": "user_id",
		"Name":   "name",
	}
	for field, want := range cases {
		if got := m.Obj2Table(field); got != want {
			t.Errorf("Obj2Table(%q) 期望 %q, 实际 %q", field, want, got)
		}
	}
	// 仍是 names.Mapper 实现（引擎 SetColumnMapper 可用）
	var _ names.Mapper = names.GonicMapper{}
}

// XormGormColModel 无显式列名 tag：DeptID 依赖默认 GORM 兼容列名映射为 dept_id
type XormGormColModel struct {
	ID     string `xorm:"pk varchar(16) 'id'"`
	DeptID string
}

func TestXormCompat_GormColumnDefault(t *testing.T) {
	// 走 NewXormDriver 全装配（列名 mapper 默认在驱动装配时注入），
	// :memory: 每个驱动实例独立，互不影响
	drv, err := NewXormDriver(contracts.ConnectionConfig{Engine: "sqlite", Database: ":memory:"}, discardLog{})
	if err != nil {
		t.Fatalf("NewXormDriver: %v", err)
	}
	defer func() { _ = drv.Close() }()
	if err := drv.AutoMigrate(&XormGormColModel{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// 列名 dept_id 直查（v0.8.0 SnakeMapper 会推导成 dept_i_d 导致 no such column）
	if err := drv.Query().Create(&XormGormColModel{ID: "g1", DeptID: "d-9"}); err != nil {
		t.Fatalf("Create(默认 GORM 列名): %v", err)
	}
	var rows []XormGormColModel
	if err := drv.Query().Model(&XormGormColModel{}).Where("dept_id = ?", "d-9").Find(&rows); err != nil {
		t.Fatalf("按 dept_id 列查询: %v", err)
	}
	if len(rows) != 1 || rows[0].DeptID != "d-9" {
		t.Errorf("期望命中 dept_id=d-9, 实际 %+v", rows)
	}
}

// Exists 空实现引用保持（contracts 钩子接口回归桩）
var _ contracts.Query = (*XormQuery)(nil)
