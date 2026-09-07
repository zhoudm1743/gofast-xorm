// compat_model_pk_test.go —— X-09 回归（stitch-mes 全表更新事故报告 2026-09-07）：
// Model(&bean).Updates(...)/Update(col, val) 必须把 bean 的非零主键并入更新
// 条件（对齐 gorm 驱动/GORM v2 标准行为）；v1.0.0 及之前 xorm 驱动只按 Model
// 定表、不取主键，生成无 WHERE 的全表 UPDATE，业务侧（如个人中心保存资料）
// 会把整表行改写为当前 bean 的值，属于静默数据破坏。
package xormdriver

import (
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// Model(&bean) 主键非零时 Updates(map) 只命中该主键行（事故报告同款写法：
// 先 First 填充主键，再 Model(&user).Updates(map)）。
func TestXormCompat_ModelPKUpdatesMap(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "pk1", Name: "alice"}); err != nil {
		t.Fatalf("Create pk1: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "pk2", Name: "bob"}); err != nil {
		t.Fatalf("Create pk2: %v", err)
	}

	// 事故路径：First 回填主键 → Model(&bean).Updates 无显式 Where
	var bean XormTestModel
	if err := q.First(&bean, "id = ?", "pk1"); err != nil {
		t.Fatalf("First: %v", err)
	}
	if err := q.Model(&bean).Updates(map[string]any{"name": "诶嘿"}); err != nil {
		t.Fatalf("Model(&bean).Updates: %v", err)
	}

	var after1, after2 XormTestModel
	if err := q.First(&after1, "id = ?", "pk1"); err != nil {
		t.Fatalf("回读 pk1: %v", err)
	}
	if err := q.First(&after2, "id = ?", "pk2"); err != nil {
		t.Fatalf("回读 pk2: %v", err)
	}
	if after1.Name != "诶嘿" {
		t.Errorf("目标行 pk1 期望 name=诶嘿, 实际 %q", after1.Name)
	}
	if after2.Name != "bob" {
		t.Errorf("非目标行 pk2 不应被改写, 期望 bob, 实际 %q（全表更新回归）", after2.Name)
	}
}

// Updates(struct) 路径同样并入主键条件（xorm struct 更新跳过零值字段，
// 与 gorm Updates(struct) 语义一致，此处只验证行范围）。
func TestXormCompat_ModelPKUpdatesStruct(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "st1", Name: "alice"}); err != nil {
		t.Fatalf("Create st1: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "st2", Name: "bob"}); err != nil {
		t.Fatalf("Create st2: %v", err)
	}

	bean := XormTestModel{ID: "st1"}
	if err := q.Model(&bean).Updates(XormTestModel{Name: "struct-updated"}); err != nil {
		t.Fatalf("Updates(struct): %v", err)
	}
	var after2 XormTestModel
	if err := q.First(&after2, "id = ?", "st2"); err != nil {
		t.Fatalf("回读 st2: %v", err)
	}
	if after2.Name != "bob" {
		t.Errorf("非目标行 st2 不应被改写, 期望 bob, 实际 %q（全表更新回归）", after2.Name)
	}
}

// Model(&bean).Update(col, val) 单列路径同样并入主键条件（报告 §三 指出的
// updateColumnCore 同款缺陷路径）。
func TestXormCompat_ModelPKUpdateColumn(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "uc1", Name: "alice"}); err != nil {
		t.Fatalf("Create uc1: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "uc2", Name: "bob"}); err != nil {
		t.Fatalf("Create uc2: %v", err)
	}

	bean := XormTestModel{ID: "uc1"}
	if err := q.Model(&bean).Update("name", "single-col"); err != nil {
		t.Fatalf("Model(&bean).Update: %v", err)
	}
	var after2 XormTestModel
	if err := q.First(&after2, "id = ?", "uc2"); err != nil {
		t.Fatalf("回读 uc2: %v", err)
	}
	if after2.Name != "bob" {
		t.Errorf("非目标行 uc2 不应被改写, 期望 bob, 实际 %q（全表更新回归）", after2.Name)
	}

	// Result 变体同路径：命中主键行 RowsAffected = 1
	res := q.Model(&XormTestModel{ID: "uc2"}).UpdateResult("name", "uc2-new")
	if res.Error != nil {
		t.Fatalf("UpdateResult: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("UpdateResult 期望 RowsAffected=1, 实际 %d", res.RowsAffected)
	}
	res = q.Model(&XormTestModel{ID: "uc1"}).UpdatesResult(map[string]any{"name": "uc1-new"})
	if res.Error != nil {
		t.Fatalf("UpdatesResult: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("UpdatesResult 期望 RowsAffected=1, 实际 %d", res.RowsAffected)
	}
}

// 表达式 UPDATE（X-08 的 updateWithExpr 通道）同样并入 Model 主键条件：
// builder 组装的 WHERE = 链上条件 AND 主键等值。
func TestXormCompat_ModelPKExprPath(t *testing.T) {
	drv := newExprModelDriver(t) // 预置 a=10 / b=20 / c=30（自增 id 1/2/3）
	rowB := exprRowByName(t, drv, "b")

	// Update 单列表达式：只命中主键行
	if err := drv.Query().Model(&XormExprModel{ID: rowB.ID}).
		Update("count", contracts.Expr("count + ?", 5)); err != nil {
		t.Fatalf("Model+Update 表达式: %v", err)
	}
	if got := exprRowByName(t, drv, "b").Count; got != 25 {
		t.Errorf("目标行 b 期望 count=25, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 10 {
		t.Errorf("非目标行 a 不应变更, 期望 10, 实际 %d（全表更新回归）", got)
	}
	if got := exprRowByName(t, drv, "c").Count; got != 30 {
		t.Errorf("非目标行 c 不应变更, 期望 30, 实际 %d（全表更新回归）", got)
	}

	// Updates 混合表达式 map：主键条件与链上 Where 叠加（AND）
	rowC := exprRowByName(t, drv, "c")
	if err := drv.Query().Model(&XormExprModel{ID: rowC.ID}).
		Where("count > ?", 0).
		Updates(map[string]any{"count": contracts.Expr("count * ?", 2)}); err != nil {
		t.Fatalf("Model+Updates 表达式: %v", err)
	}
	if got := exprRowByName(t, drv, "c").Count; got != 60 {
		t.Errorf("目标行 c 期望 count=60, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 10 {
		t.Errorf("非目标行 a 不应变更, 期望 10, 实际 %d（全表更新回归）", got)
	}
}

// extends 嵌入模型：主键经 FieldIndex 定位（X-03 同款路径），
// Model(&bean) 主键条件对嵌入主键同样生效。
func TestXormCompat_ModelPKExtendsModel(t *testing.T) {
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&XormExtModel{}); err != nil {
		t.Fatalf("AutoMigrate(extends 模型): %v", err)
	}
	q := drv.Query()
	m1 := &XormExtModel{OrderNo: "PK-1"}
	m2 := &XormExtModel{OrderNo: "PK-2"}
	if err := q.Create(m1); err != nil {
		t.Fatalf("Create m1: %v", err)
	}
	if err := q.Create(m2); err != nil {
		t.Fatalf("Create m2: %v", err)
	}

	// 仅传主键的 bean：嵌入主键经 FieldIndex 取出
	bean := &XormExtModel{}
	bean.ID = m1.ID
	if err := q.Model(bean).Updates(map[string]any{"order_no": "PK-1-updated"}); err != nil {
		t.Fatalf("Model(extends bean).Updates: %v", err)
	}
	var after2 XormExtModel
	if err := q.Model(&XormExtModel{}).Where("order_no = ?", "PK-2").First(&after2); err != nil {
		t.Fatalf("回读 PK-2: %v", err)
	}
	if after2.OrderNo != "PK-2" {
		t.Errorf("非目标行不应被改写, 期望 order_no=PK-2, 实际 %q（全表更新回归）", after2.OrderNo)
	}
}

// 链上显式 Where 与 Model 主键条件叠加（AND）：互不覆盖。
func TestXormCompat_ModelPKWithExplicitWhere(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for _, id := range []string{"w1", "w2"} {
		if err := q.Create(&XormTestModel{ID: id, Name: "n-" + id}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	// 主键 w1 AND 显式条件（name 不匹配）→ 0 行命中，w1 保持原值
	res := q.Model(&XormTestModel{ID: "w1"}).
		Where("name = ?", "no-such").
		UpdatesResult(map[string]any{"name": "updated"})
	if res.Error != nil {
		t.Fatalf("UpdatesResult: %v", res.Error)
	}
	if !res.IsZeroRow() {
		t.Errorf("主键+显式条件交集为空期望 0 行, 实际 %d", res.RowsAffected)
	}
	var after1 XormTestModel
	if err := q.First(&after1, "id = ?", "w1"); err != nil {
		t.Fatalf("回读 w1: %v", err)
	}
	if after1.Name != "n-w1" {
		t.Errorf("w1 不应被改写, 期望 n-w1, 实际 %q", after1.Name)
	}
}

// 现状锁定：Model 入参主键全零（或非 struct 解析不出主键）时不附加主键条件，
// 更新范围完全由链上 Where 决定——无 Where 仍为全表更新（文档已显著标注，
// 业务应显式 Where 或保证 bean 主键非零；见缺陷报告 §六 修复方向 1/3）。
func TestXormCompat_ModelZeroPKKeepsChainBehavior(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for _, id := range []string{"z1", "z2"} {
		if err := q.Create(&XormTestModel{ID: id, Name: "n"}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	// 零主键 + 显式 Where：只命中条件行
	if err := q.Model(&XormTestModel{}).Where("id = ?", "z1").
		Updates(map[string]any{"name": "z1-only"}); err != nil {
		t.Fatalf("零主键+Where Updates: %v", err)
	}
	var z2 XormTestModel
	if err := q.First(&z2, "id = ?", "z2"); err != nil {
		t.Fatalf("回读 z2: %v", err)
	}
	if z2.Name != "n" {
		t.Errorf("z2 不应被改写, 期望 n, 实际 %q", z2.Name)
	}

	// Table() 路径无 modelValue：行为不变（链上 Where 生效）
	if err := q.Table("xorm_test_model").Where("id = ?", "z2").
		Update("name", "z2-only"); err != nil {
		t.Fatalf("Table+Where Update: %v", err)
	}
	var z1 XormTestModel
	if err := q.First(&z1, "id = ?", "z1"); err != nil {
		t.Fatalf("回读 z1: %v", err)
	}
	if z1.Name != "z1-only" {
		t.Errorf("z1 期望 z1-only, 实际 %q", z1.Name)
	}
}
