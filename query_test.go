package xormdriver

import (
	"errors"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── CRUD 基础闭环 ────────────────────────────────────────────────────
//
// 覆盖 Create/Find/First/Last/Take/Count/Update/Updates/Delete 全链路：
// 插入 → 查询 → 更新 → 删除 → 计数，验证各终结方法在真实 SQLite 上的协作。

func TestXormCRUDLoop(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// 插入两条（链式方法不可变，同一 q 可安全复用）
	if err := q.Create(&XormTestModel{ID: "a1", Name: "alice"}); err != nil {
		t.Fatalf("Create a1 失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "a2", Name: "bob"}); err != nil {
		t.Fatalf("Create a2 失败: %v", err)
	}

	// Find 全量
	var rows []XormTestModel
	if err := q.Find(&rows); err != nil {
		t.Fatalf("Find 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Find 期望 2 行, 实际 %d", len(rows))
	}

	// First 主键升序取首行
	var first XormTestModel
	if err := q.First(&first); err != nil {
		t.Fatalf("First 失败: %v", err)
	}
	if first.ID != "a1" || first.Name != "alice" {
		t.Errorf("First 期望 a1/alice, 实际 %+v", first)
	}

	// Last 主键降序取末行
	var last XormTestModel
	if err := q.Last(&last); err != nil {
		t.Fatalf("Last 失败: %v", err)
	}
	if last.ID != "a2" || last.Name != "bob" {
		t.Errorf("Last 期望 a2/bob, 实际 %+v", last)
	}

	// Take 不排序，按条件命中任意一行
	var taken XormTestModel
	if err := q.Take(&taken, "id = ?", "a2"); err != nil {
		t.Fatalf("Take 失败: %v", err)
	}
	if taken.Name != "bob" {
		t.Errorf("Take 命中 a2, Name 期望 bob, 实际 %q", taken.Name)
	}

	// 计数
	var n int64
	if err := q.Model(&XormTestModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("Count 期望 2, 实际 %d", n)
	}

	// Update 单列更新（要求链上显式 Model/Table）
	if err := q.Model(&XormTestModel{}).Where("id = ?", "a1").Update("name", "alice2"); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	var afterUpdate XormTestModel
	if err := q.First(&afterUpdate, "id = ?", "a1"); err != nil {
		t.Fatalf("Update 后查询失败: %v", err)
	}
	if afterUpdate.Name != "alice2" {
		t.Errorf("Update 后 Name 期望 alice2, 实际 %q", afterUpdate.Name)
	}

	// Updates 批量字段更新
	if err := q.Model(&XormTestModel{}).Where("id = ?", "a2").Updates(map[string]any{"name": "bob2"}); err != nil {
		t.Fatalf("Updates 失败: %v", err)
	}
	var afterUpdates XormTestModel
	if err := q.First(&afterUpdates, "id = ?", "a2"); err != nil {
		t.Fatalf("Updates 后查询失败: %v", err)
	}
	if afterUpdates.Name != "bob2" {
		t.Errorf("Updates 后 Name 期望 bob2, 实际 %q", afterUpdates.Name)
	}

	// 删除后计数收敛为 1
	if err := q.Delete(&XormTestModel{ID: "a1"}); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if err := q.Model(&XormTestModel{}).Count(&n); err != nil {
		t.Fatalf("Delete 后 Count 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("Delete 后 Count 期望 1, 实际 %d", n)
	}
}

// First/Last 的排序方向由主键决定：乱序插入后 First 取主键最小行、Last 取最大行。
func TestXormFirstLastOrdering(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// 乱序插入，验证 First/Last 依据主键排序而非插入顺序
	for _, m := range []XormTestModel{
		{ID: "c3", Name: "three"},
		{ID: "c1", Name: "one"},
		{ID: "c2", Name: "two"},
	} {
		v := m
		if err := q.Create(&v); err != nil {
			t.Fatalf("Create %s 失败: %v", v.ID, err)
		}
	}

	var first XormTestModel
	if err := q.First(&first); err != nil {
		t.Fatalf("First 失败: %v", err)
	}
	if first.ID != "c1" {
		t.Errorf("First 期望主键最小的 c1, 实际 %q", first.ID)
	}

	var last XormTestModel
	if err := q.Last(&last); err != nil {
		t.Fatalf("Last 失败: %v", err)
	}
	if last.ID != "c3" {
		t.Errorf("Last 期望主键最大的 c3, 实际 %q", last.ID)
	}
}

// ── Save 双路径 ──────────────────────────────────────────────────────

// 零主键走插入路径：Save 等价 Create，行数 +1。
func TestXormSaveInsertPath(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// ID 为空 → 主键全零 → 路由到插入
	if err := q.Save(&XormTestModel{Name: "inserted"}); err != nil {
		t.Fatalf("Save(零主键) 失败: %v", err)
	}

	var rows []XormTestModel
	if err := q.Find(&rows); err != nil {
		t.Fatalf("Find 失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Save 插入路径期望 1 行, 实际 %d", len(rows))
	}
	if rows[0].Name != "inserted" {
		t.Errorf("Save 插入路径 Name 期望 inserted, 实际 %q", rows[0].Name)
	}
}

// 非零主键走全列更新路径（AllCols）：零值字段也会覆盖库中旧值。
// 用"Name 置零后再 Save"验证——若按 xorm 默认跳过零值字段，Name 会保持原值。
func TestXormSaveUpdatePathAllCols(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "s1", Name: "keep-me"}); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 同主键、Name 为零值：全列更新应把库中 Name 覆盖为空串
	if err := q.Save(&XormTestModel{ID: "s1"}); err != nil {
		t.Fatalf("Save(非零主键) 失败: %v", err)
	}

	var after XormTestModel
	if err := q.First(&after, "id = ?", "s1"); err != nil {
		t.Fatalf("Save 后查询失败: %v", err)
	}
	if after.Name != "" {
		t.Errorf("AllCols 全列更新应把 Name 覆盖为零值, 实际 %q", after.Name)
	}

	// 行数不变：确认是更新而非插入
	var n int64
	if err := q.Model(&XormTestModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("Save 更新路径不应新增行, 期望 1, 实际 %d", n)
	}
}

// ── RowsAffected 语义（Result 变体）──────────────────────────────────

func TestXormCreateResultRowsAffected(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// 单条插入恒为 1
	res := q.CreateResult(&XormTestModel{ID: "r1", Name: "one"})
	if res.Error != nil {
		t.Fatalf("CreateResult 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("CreateResult 单条 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}
	if res.IsZeroRow() {
		t.Error("成功插入不应判为零行")
	}

	// 切片插入为总行数
	res = q.CreateResult([]*XormTestModel{
		{ID: "r2", Name: "two"},
		{ID: "r3", Name: "three"},
	})
	if res.Error != nil {
		t.Fatalf("CreateResult(切片) 失败: %v", res.Error)
	}
	if res.RowsAffected != 2 {
		t.Errorf("CreateResult 切片 RowsAffected 期望 2, 实际 %d", res.RowsAffected)
	}
}

func TestXormUpdateResultRowsAffected(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "u1", Name: "before"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}

	// 命中行：RowsAffected = 1
	res := q.Model(&XormTestModel{}).Where("id = ?", "u1").UpdateResult("name", "after")
	if res.Error != nil {
		t.Fatalf("UpdateResult 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("命中行 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}
	if res.IsZeroRow() {
		t.Error("命中行不应判为零行")
	}

	// 未命中：Error 为 nil 但 RowsAffected = 0，经 IsZeroRow 判定
	res = q.Model(&XormTestModel{}).Where("id = ?", "missing").UpdateResult("name", "x")
	if res.Error != nil {
		t.Fatalf("未命中行不应返回错误: %v", res.Error)
	}
	if res.RowsAffected != 0 {
		t.Errorf("未命中行 RowsAffected 期望 0, 实际 %d", res.RowsAffected)
	}
	if !res.IsZeroRow() {
		t.Error("未命中行应判为零行")
	}
}

func TestXormUpdatesResultRowsAffected(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "us1", Name: "before"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}

	// 命中行
	res := q.Model(&XormTestModel{}).Where("id = ?", "us1").
		UpdatesResult(map[string]any{"name": "after"})
	if res.Error != nil {
		t.Fatalf("UpdatesResult 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("命中行 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}

	// 未命中 → IsZeroRow
	res = q.Model(&XormTestModel{}).Where("id = ?", "missing").
		UpdatesResult(map[string]any{"name": "x"})
	if res.Error != nil {
		t.Fatalf("未命中行不应返回错误: %v", res.Error)
	}
	if !res.IsZeroRow() {
		t.Errorf("未命中行应判为零行, 实际 RowsAffected=%d Error=%v", res.RowsAffected, res.Error)
	}
}

func TestXormDeleteResultRowsAffected(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "d1", Name: "victim"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}

	// 删除命中 1 行
	res := q.DeleteResult(&XormTestModel{ID: "d1"})
	if res.Error != nil {
		t.Fatalf("DeleteResult 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("删除命中行 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}
	if res.IsZeroRow() {
		t.Error("删除命中行不应判为零行")
	}

	// 重复删除未命中 → IsZeroRow（conds 附加条件定位不存在的行）
	res = q.DeleteResult(&XormTestModel{}, "id = ?", "d1")
	if res.Error != nil {
		t.Fatalf("未命中删除不应返回错误: %v", res.Error)
	}
	if res.RowsAffected != 0 {
		t.Errorf("未命中删除 RowsAffected 期望 0, 实际 %d", res.RowsAffected)
	}
	if !res.IsZeroRow() {
		t.Error("未命中删除应判为零行")
	}
}

func TestXormSaveResultRowsAffected(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// 铺垫一行真实数据，供更新路径命中
	if err := q.Create(&XormTestModel{ID: "sv1", Name: "staged"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}

	// 插入路径：主键全零触发 Insert，RowsAffected = 1
	res := q.SaveResult(&XormTestModel{Name: "insert"})
	if res.Error != nil {
		t.Fatalf("SaveResult(插入) 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("SaveResult 插入路径 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}

	// 更新路径命中（主键非零且库中存在该行）：RowsAffected = 1
	// 注意：本断言当前失败，暴露 query_write.go saveOne 更新分支的缺陷——
	// s.AllCols().Update(value) 未附加主键条件（xorm 仅从显式 condiBean 或
	// s.ID() 生成 UPDATE 的 WHERE），退化为无 WHERE 的全表 UPDATE：
	// 单行表静默覆盖全部行，多行表触发 UNIQUE 约束冲突。修复应在 Update
	// 前按主键列值调用 s.ID(pk...)。
	res = q.SaveResult(&XormTestModel{ID: "sv1", Name: "update"})
	if res.Error != nil {
		t.Fatalf("SaveResult(更新) 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("SaveResult 更新命中 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}

	// 更新路径未命中（主键非零但库中无此行）：对齐 gorm Save 的 upsert 语义
	// （X-04，0.8.1 起）：0 行回落 INSERT，RowsAffected=1 且新行落库
	res = q.SaveResult(&XormTestModel{ID: "ghost", Name: "nowhere"})
	if res.Error != nil {
		t.Fatalf("SaveResult 未命中回落插入不应报错: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("SaveResult 未命中回落插入 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}

	// 回落插入的行真实存在（upsert 生效）
	var n int64
	if err := q.Model(&XormTestModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 3 {
		t.Errorf("未命中 Save 应回落插入新行, 期望 3 行, 实际 %d", n)
	}
	var ghost XormTestModel
	if err := q.First(&ghost, "id = ?", "ghost"); err != nil {
		t.Fatalf("回落插入的行应可查询: %v", err)
	}
	if ghost.Name != "nowhere" {
		t.Errorf("回落插入行内容不符, 期望 nowhere, 实际 %q", ghost.Name)
	}
}

// SaveResult 在链上显式条件下的定向更新：绕开"主键自动条件"路径，单独验证
// RowsAffected 回填与 IsZeroRow 判定本身是正确的（saveOne 缺陷见上条测试注释）。
func TestXormSaveResultWithExplicitCond(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "sw1", Name: "staged"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}

	// 显式条件命中：RowsAffected = 1
	res := q.Where("id = ?", "sw1").SaveResult(&XormTestModel{ID: "sw1", Name: "update"})
	if res.Error != nil {
		t.Fatalf("SaveResult(显式条件命中) 失败: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("显式条件命中 RowsAffected 期望 1, 实际 %d", res.RowsAffected)
	}
	if res.IsZeroRow() {
		t.Error("命中行不应判为零行")
	}

	// 显式条件未命中：Error 为 nil 且 IsZeroRow
	res = q.Where("id = ?", "ghost").SaveResult(&XormTestModel{ID: "ghost", Name: "nowhere"})
	if res.Error != nil {
		t.Fatalf("SaveResult(显式条件未命中) 不应返回错误: %v", res.Error)
	}
	if !res.IsZeroRow() {
		t.Errorf("显式条件未命中应判为零行, 实际 RowsAffected=%d Error=%v", res.RowsAffected, res.Error)
	}
}

// ── CreateInBatches 分批插入 + Create 钩子 ───────────────────────────

func TestXormCreateInBatchesRowsAndHooks(t *testing.T) {
	drv := newXormHookDriver(t)
	q := drv.Query()

	// 5 条、批大小 2 → 分 3 批插入
	items := make([]*XormHookModel, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, &XormHookModel{ID: "b" + string(rune('1'+i)), Name: "batch"})
	}
	if err := q.CreateInBatches(&items, 2); err != nil {
		t.Fatalf("CreateInBatches 失败: %v", err)
	}

	// 行数断言：全部落库
	var n int64
	if err := q.Model(&XormHookModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 5 {
		t.Errorf("CreateInBatches 期望落库 5 行, 实际 %d", n)
	}

	// 钩子断言：walkValues 逐元素展开，每个元素前后各回调一次
	for _, m := range items {
		if !m.BeforeCreateCalled {
			t.Errorf("元素 %s 的 BeforeCreate 钩子未被调用", m.ID)
		}
		if !m.AfterCreateCalled {
			t.Errorf("元素 %s 的 AfterCreate 钩子未被调用", m.ID)
		}
	}
}

// ── Save / Delete / 查询钩子 ─────────────────────────────────────────

func TestXormSaveUpdateHooks(t *testing.T) {
	drv := newXormHookDriver(t)
	q := drv.Query()

	m := &XormHookModel{ID: "h1", Name: "original"}
	if err := q.Create(m); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 清掉 Create 阶段的标记，避免误判
	m.BeforeCreateCalled = false
	m.AfterCreateCalled = false

	// 非零主键 Save → 更新分支 → Update 钩子
	m.Name = "updated"
	if err := q.Save(m); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	if !m.BeforeUpdateCalled {
		t.Error("Save 更新分支应调用 BeforeUpdate 钩子")
	}
	if !m.AfterUpdateCalled {
		t.Error("Save 更新分支应调用 AfterUpdate 钩子")
	}

	// 更新真实落库
	var after XormHookModel
	if err := q.First(&after, "id = ?", "h1"); err != nil {
		t.Fatalf("Save 后查询失败: %v", err)
	}
	if after.Name != "updated" {
		t.Errorf("Save 后 Name 期望 updated, 实际 %q", after.Name)
	}
}

func TestXormDeleteHooks(t *testing.T) {
	drv := newXormHookDriver(t)
	q := drv.Query()

	m := &XormHookModel{ID: "h2", Name: "delete-me"}
	if err := q.Create(m); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	m.BeforeCreateCalled = false
	m.AfterCreateCalled = false

	if err := q.Delete(m); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}

	if !m.BeforeDeleteCalled {
		t.Error("Delete 应调用 BeforeDelete 钩子")
	}
	if !m.AfterDeleteCalled {
		t.Error("Delete 应调用 AfterDelete 钩子")
	}

	// 行确实被删除
	var n int64
	if err := q.Model(&XormHookModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("Delete 后期望 0 行, 实际 %d", n)
	}
}

func TestXormFindAfterFindHooks(t *testing.T) {
	drv := newXormHookDriver(t)
	q := drv.Query()

	if err := q.Create(&XormHookModel{ID: "h3", Name: "find-me"}); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if err := q.Create(&XormHookModel{ID: "h4", Name: "find-me-too"}); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// Find：切片逐元素触发 AfterFind
	var rows []XormHookModel
	if err := q.Find(&rows); err != nil {
		t.Fatalf("Find 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Find 期望 2 行, 实际 %d", len(rows))
	}
	for _, r := range rows {
		if !r.AfterFindCalled {
			t.Errorf("Find 后元素 %s 的 AfterFind 钩子未被调用", r.ID)
		}
	}

	// First：命中真实行后触发 AfterFind
	var first XormHookModel
	if err := q.First(&first, "id = ?", "h3"); err != nil {
		t.Fatalf("First 失败: %v", err)
	}
	if !first.AfterFindCalled {
		t.Error("First 命中行应调用 AfterFind 钩子")
	}
}

// ── 错误映射 ─────────────────────────────────────────────────────────

func TestXormDuplicateKeyError(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "dup1", Name: "first"}); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	// 重复主键 → SQLite UNIQUE 约束 → ErrDuplicatedKey
	err := q.Create(&XormTestModel{ID: "dup1", Name: "second"})
	if err == nil {
		t.Fatal("重复主键应返回错误")
	}
	if !errors.Is(err, contracts.ErrDuplicatedKey) {
		t.Errorf("重复主键应映射为 ErrDuplicatedKey, 实际: %v", err)
	}
}

func TestXormFirstRecordNotFound(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	var m XormTestModel
	err := q.First(&m, "id = ?", "nonexistent")
	if err == nil {
		t.Fatal("First 未命中应返回错误")
	}
	if !errors.Is(err, contracts.ErrRecordNotFound) {
		t.Errorf("First 未命中应映射为 ErrRecordNotFound, 实际: %v", err)
	}

	// Take 未命中走同一出口（getOne 公共路径）
	err = q.Take(&m, "id = ?", "nonexistent")
	if !errors.Is(err, contracts.ErrRecordNotFound) {
		t.Errorf("Take 未命中应映射为 ErrRecordNotFound, 实际: %v", err)
	}
}

// ── Exists ───────────────────────────────────────────────────────────

func TestXormExistsScenarios(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	// 空表：false 且无错误
	exists, err := q.Exists(&XormTestModel{})
	if err != nil {
		t.Fatalf("Exists(空表) 返回错误: %v", err)
	}
	if exists {
		t.Error("空表 Exists 应返回 false")
	}

	// 有数据：true
	if err := q.Create(&XormTestModel{ID: "e1", Name: "alice"}); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}
	exists, err = q.Exists(&XormTestModel{})
	if err != nil {
		t.Fatalf("Exists 返回错误: %v", err)
	}
	if !exists {
		t.Error("有数据时 Exists 应返回 true")
	}

	// 带条件命中：true
	exists, err = q.Exists(&XormTestModel{}, "name = ?", "alice")
	if err != nil {
		t.Fatalf("Exists(命中条件) 返回错误: %v", err)
	}
	if !exists {
		t.Error("条件命中时 Exists 应返回 true")
	}

	// 带条件不命中：false
	exists, err = q.Exists(&XormTestModel{}, "name = ?", "nonexistent")
	if err != nil {
		t.Fatalf("Exists(不命中条件) 返回错误: %v", err)
	}
	if exists {
		t.Error("条件不命中时 Exists 应返回 false")
	}
}
