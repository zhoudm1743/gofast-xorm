//go:build integration

package xormdriver

// hooks_integration_test.go —— 模型钩子（7 个 On*）真实库集成（双驱动测试方案
// §5.13 HK-01/HK-02、§六 第 11 项）：把 SQLite 单测（testutil_test.go
// XormHookModel 样板）与 drivertest suiteHooks 的关键用例提升到 PG/MySQL
// 双方言验证，与 gormdriver/hooks_integration_test.go 语义对齐。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -count=1 -run 'TestHooks' -v .
//
// 覆盖场景（PG/MySQL 双方言断言一致，无 dialect 分支）：
//   - HK-01：Create 触发 OnBeforeCreate/OnAfterCreate 各恰好一次，数据落库；
//   - HK-01：Save 双分支钩子（按驱动实现实测固化——xorm 驱动 saveOne 按主键
//     是否全零路由：空主键走 createCore → Create 系钩子；非空主键走
//     invokeBeforeUpdate/AllCols().Update/invokeAfterUpdate → Update 系钩子，
//     见 query_write.go saveOne）；
//   - HK-02：OnBeforeCreate 返回业务错误 → Create 中断不写库（行数不变），
//     错误经 q.done → wrapError 原样透传（errors.Is 命中且为同实例）；
//   - HK-04：链式 Update/Updates 不触发任何模型钩子（Model bean 计数位恒零）。
//
// 计数位用 int 而非 bool：可精确断言"各触发恰好一次"，捕获重复触发回归。
// （计数位沿 XormHookModel 样板不带列 tag，由 Sync2 一并建出，不影响语义。）

import (
	"errors"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型与钩子样板 ───────────────────────────────────────────────

// xhkErrBoom 业务钩子返回的自定义错误（§11.14：原样透传，不被 Sentinel 包装吞没）。
var xhkErrBoom = errors.New("xhk boom: 业务钩子自定义错误")

// xhkHookModel 全钩子模型：实现 contracts 全部 7 个 On* 钩子 + IDAutoGenerator，
// 用 int 计数位验证钩子触发次数与时机。FailHook 非空时对应钩子返回 xhkErrBoom
//（错误中断用例）。
type xhkHookModel struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`

	FailHook string

	BeforeCreateCalled int
	AfterCreateCalled  int
	BeforeUpdateCalled int
	AfterUpdateCalled  int
	BeforeDeleteCalled int
	AfterDeleteCalled  int
	AfterFindCalled    int
}

func (xhkHookModel) TableName() string { return "xhk_hooks" }

func (m *xhkHookModel) AutoGenerateID() {
	// 测试中显式设置 ID（Save 插入分支不经 invokeBeforeCreate，ID 保持空串）
}

// hookFails 报告指定钩子是否被配置为失败（HK-02 错误中断）。
func (m *xhkHookModel) hookFails(name string) bool {
	return m.FailHook == name
}

// hookTotal 计数位总和（"全零"断言用）。
func (m *xhkHookModel) hookTotal() int {
	return m.BeforeCreateCalled + m.AfterCreateCalled +
		m.BeforeUpdateCalled + m.AfterUpdateCalled +
		m.BeforeDeleteCalled + m.AfterDeleteCalled + m.AfterFindCalled
}

// OnBeforeCreate 实现 contracts.BeforeCreator。
func (m *xhkHookModel) OnBeforeCreate(q contracts.Query) error {
	m.BeforeCreateCalled++
	if m.hookFails("before_create") {
		return xhkErrBoom
	}
	return nil
}

// OnAfterCreate 实现 contracts.AfterCreator。
func (m *xhkHookModel) OnAfterCreate(q contracts.Query) error {
	m.AfterCreateCalled++
	if m.hookFails("after_create") {
		return xhkErrBoom
	}
	return nil
}

// OnBeforeUpdate 实现 contracts.BeforeUpdater。
func (m *xhkHookModel) OnBeforeUpdate(q contracts.Query) error {
	m.BeforeUpdateCalled++
	if m.hookFails("before_update") {
		return xhkErrBoom
	}
	return nil
}

// OnAfterUpdate 实现 contracts.AfterUpdater。
func (m *xhkHookModel) OnAfterUpdate(q contracts.Query) error {
	m.AfterUpdateCalled++
	if m.hookFails("after_update") {
		return xhkErrBoom
	}
	return nil
}

// OnBeforeDelete 实现 contracts.BeforeDeleter。
func (m *xhkHookModel) OnBeforeDelete(q contracts.Query) error {
	m.BeforeDeleteCalled++
	if m.hookFails("before_delete") {
		return xhkErrBoom
	}
	return nil
}

// OnAfterDelete 实现 contracts.AfterDeleter。
func (m *xhkHookModel) OnAfterDelete(q contracts.Query) error {
	m.AfterDeleteCalled++
	if m.hookFails("after_delete") {
		return xhkErrBoom
	}
	return nil
}

// OnAfterFind 实现 contracts.AfterFinder。
func (m *xhkHookModel) OnAfterFind(q contracts.Query) error {
	m.AfterFindCalled++
	if m.hookFails("after_find") {
		return xhkErrBoom
	}
	return nil
}

// ── 入口与共享 runner ────────────────────────────────────────────────

// TestHooks_PG 钩子矩阵（pgsql 方言）。
func TestHooks_PG(t *testing.T) {
	drv := newPGXorm(t)
	runXhkHookMatrix(t, drv)
}

// TestHooks_MySQL 钩子矩阵（mysql 方言）。
func TestHooks_MySQL(t *testing.T) {
	drv := newPlainMySQLXorm(t)
	runXhkHookMatrix(t, drv)
}

// runXhkHookMatrix PG/MySQL 共用的钩子关键用例矩阵（方言无差异预期，
// 双方言断言完全一致）。
func runXhkHookMatrix(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xhk_hooks"}, &xhkHookModel{})
	q := drv.Query()

	// 回读辅助：按主键取整行（链上显式 Where，不经 Model 主键路径）。
	mustTake := func(t *testing.T, id string) xhkHookModel {
		t.Helper()
		var got xhkHookModel
		if err := q.Model(&xhkHookModel{}).Where("id = ?", id).First(&got); err != nil {
			t.Fatalf("回读 %q: %v", id, err)
		}
		return got
	}

	t.Run("HK-01_Create触发BeforeAfterCreate且落库", func(t *testing.T) {
		m := &xhkHookModel{ID: "xhk-c1", Name: "create"}
		if err := q.Create(m); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if m.BeforeCreateCalled != 1 {
			t.Errorf("OnBeforeCreate 应恰好触发一次, 实际 %d 次", m.BeforeCreateCalled)
		}
		if m.AfterCreateCalled != 1 {
			t.Errorf("OnAfterCreate 应恰好触发一次, 实际 %d 次", m.AfterCreateCalled)
		}
		// 其余钩子不应被 Create 带动
		if m.BeforeUpdateCalled+m.AfterUpdateCalled+
			m.BeforeDeleteCalled+m.AfterDeleteCalled+m.AfterFindCalled != 0 {
			t.Errorf("Create 不应触发 Create 系以外钩子: %+v", m)
		}
		// 数据真实落库（真实方言读写闭环）
		got := mustTake(t, "xhk-c1")
		if got.Name != "create" {
			t.Errorf("落库数据不一致, 期望 name=create, 实际 %q", got.Name)
		}
	})

	t.Run("HK-01_Save双分支钩子", func(t *testing.T) {
		// xorm 驱动实测语义（query_write.go saveOne）：Save 按主键是否全零
		// 路由——空主键 → createCore（Create 系钩子）；非空主键 →
		// invokeBeforeUpdate/AllCols().Update/invokeAfterUpdate（Update 系钩子）。

		// 插入分支（空主键）：触发 Create 系钩子，不触发 Update 系
		ins := &xhkHookModel{Name: "save-ins"}
		if err := q.Save(ins); err != nil {
			t.Fatalf("Save(插入分支): %v", err)
		}
		if ins.BeforeCreateCalled != 1 || ins.AfterCreateCalled != 1 {
			t.Errorf("Save 插入分支应各触发一次 Create 系钩子, 实际 before=%d after=%d",
				ins.BeforeCreateCalled, ins.AfterCreateCalled)
		}
		if ins.BeforeUpdateCalled != 0 || ins.AfterUpdateCalled != 0 {
			t.Errorf("Save 插入分支不应触发 Update 系钩子: %+v", ins)
		}
		var gotIns []xhkHookModel
		if err := q.Model(&xhkHookModel{}).Where("name = ?", "save-ins").Find(&gotIns); err != nil {
			t.Fatalf("回读 Save 插入分支: %v", err)
		}
		if len(gotIns) != 1 {
			t.Fatalf("Save 插入分支应真实落库 1 行, 实际 %d 行", len(gotIns))
		}

		// 更新分支（非空主键）：触发 Update 系钩子，不触发 Create 系
		up := &xhkHookModel{ID: "xhk-sv2", Name: "before-save"}
		if err := q.Create(up); err != nil {
			t.Fatalf("种子 Create: %v", err)
		}
		up.BeforeCreateCalled, up.AfterCreateCalled = 0, 0 // 重置 Create 阶段计数
		up.Name = "after-save"
		if err := q.Save(up); err != nil {
			t.Fatalf("Save(更新分支): %v", err)
		}
		if up.BeforeUpdateCalled != 1 || up.AfterUpdateCalled != 1 {
			t.Errorf("Save 更新分支应各触发一次 Update 系钩子, 实际 before=%d after=%d",
				up.BeforeUpdateCalled, up.AfterUpdateCalled)
		}
		if up.BeforeCreateCalled != 0 || up.AfterCreateCalled != 0 {
			t.Errorf("Save 更新分支不应触发 Create 系钩子: %+v", up)
		}
		if got := mustTake(t, "xhk-sv2"); got.Name != "after-save" {
			t.Errorf("Save 更新分支落库数据不一致, 期望 name=after-save, 实际 %q", got.Name)
		}
	})

	t.Run("HK-02_钩子错误中断Create", func(t *testing.T) {
		bad := &xhkHookModel{ID: "xhk-bad", Name: "boom", FailHook: "before_create"}
		err := q.Create(bad)
		if err == nil {
			t.Fatal("OnBeforeCreate 业务错误应中断 Create")
		}
		if !errors.Is(err, xhkErrBoom) {
			t.Errorf("业务错误应可被 errors.Is 命中, 实际: %v", err)
		}
		if err != xhkErrBoom { // q.done → wrapError 对非 Sentinel 错误原样透传（同实例）
			t.Errorf("业务错误应原样透传（同实例）, 实际: %v", err)
		}
		if bad.AfterCreateCalled != 0 {
			t.Errorf("Before 钩子中断后不应继续触发 AfterCreate: %+v", bad)
		}
		// 中断即不写库：行数不变
		var n int64
		if err := q.Model(&xhkHookModel{}).Where("id = ?", "xhk-bad").Count(&n); err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 0 {
			t.Errorf("钩子错误中断后不应落库, 实际 %d 行", n)
		}
	})

	t.Run("HK-04_UpdateUpdates不触发模型钩子", func(t *testing.T) {
		seed := &xhkHookModel{ID: "xhk-up", Name: "orig"}
		if err := q.Create(seed); err != nil {
			t.Fatalf("种子 Create: %v", err)
		}
		// 链式 Update/Updates 不触发任何模型钩子：以 Model bean 的计数位为
		// 直接观察点（驱动若误调用钩子，计数位必落在 bean 上）
		bean := &xhkHookModel{}
		if err := q.Model(bean).Where("id = ?", "xhk-up").Update("name", "u2"); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if bean.hookTotal() != 0 {
			t.Errorf("Update 不应触发模型钩子, 计数: %+v", bean)
		}
		bean2 := &xhkHookModel{}
		if err := q.Model(bean2).Where("id = ?", "xhk-up").Updates(map[string]any{"name": "u3"}); err != nil {
			t.Fatalf("Updates: %v", err)
		}
		if bean2.hookTotal() != 0 {
			t.Errorf("Updates 不应触发模型钩子, 计数: %+v", bean2)
		}
		// 数据真实更新
		if got := mustTake(t, "xhk-up"); got.Name != "u3" {
			t.Errorf("Updates 数据未落库, 期望 name=u3, 实际 %q", got.Name)
		}
	})
}
