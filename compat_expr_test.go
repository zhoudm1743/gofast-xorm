// compat_expr_test.go —— X-08 跨驱动 SQL 表达式（contracts.Expr）xorm 驱动侧回归：
// Update/Updates 的列值表达式经 builder 组装为 "SET col = <表达式>" 原子更新；
// ExecResult 返回受影响行数；普通值路径行为不变。
package xormdriver

import (
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// XormExprModel 计数测试模型：SnakeMapper 推导表名 xorm_expr_model。
type XormExprModel struct {
	ID    int64  `xorm:"pk autoincr 'id'"`
	Count int64  `xorm:"'count'"`
	Name  string `xorm:"varchar(64) 'name'"`
}

// newExprModelDriver 内存 SQLite + 已迁移 xorm_expr_model 表，预置三行：
// a=10 / b=20 / c=30（自增 id 依序 1/2/3，测试内按 name 定位不依赖具体 id）。
func newExprModelDriver(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&XormExprModel{}); err != nil {
		t.Fatalf("自动迁移失败: %v", err)
	}
	q := drv.Query()
	for _, m := range []*XormExprModel{
		{Name: "a", Count: 10},
		{Name: "b", Count: 20},
		{Name: "c", Count: 30},
	} {
		if err := q.Create(m); err != nil {
			t.Fatalf("Create %q: %v", m.Name, err)
		}
	}
	return drv
}

// exprRowByName 按 name 回读单行（读路径复用既有 Find 链路）。
func exprRowByName(t *testing.T, drv *XormDriver, name string) XormExprModel {
	t.Helper()
	var rows []XormExprModel
	if err := drv.Query().Table("xorm_expr_model").Where("name = ?", name).Find(&rows); err != nil {
		t.Fatalf("回读 %q: %v", name, err)
	}
	if len(rows) != 1 {
		t.Fatalf("回读 %q 期望 1 行, 实际 %d", name, len(rows))
	}
	return rows[0]
}

// ── Update 单列表达式：全表原子递增 ──────────────────────────────────

func TestXormExpr_UpdateExpression(t *testing.T) {
	drv := newExprModelDriver(t)
	if err := drv.Query().Table("xorm_expr_model").Update("count", contracts.Expr("count + ?", 5)); err != nil {
		t.Fatalf("Update 表达式: %v", err)
	}
	for name, want := range map[string]int64{"a": 15, "b": 25, "c": 35} {
		if got := exprRowByName(t, drv, name).Count; got != want {
			t.Errorf("行 %q 期望 count=%d, 实际 %d", name, want, got)
		}
	}
}

// ── 链上 Where + 表达式：只影响目标行 ────────────────────────────────

func TestXormExpr_UpdateWithWhereTargetsOnlyRow(t *testing.T) {
	drv := newExprModelDriver(t)
	if err := drv.Query().Model(&XormExprModel{}).Where("name = ?", "a").
		Update("count", contracts.Expr("count + ?", 5)); err != nil {
		t.Fatalf("Where+Update 表达式: %v", err)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 15 {
		t.Errorf("目标行 a 期望 count=15, 实际 %d", got)
	}
	// 未命中行保持原值
	if got := exprRowByName(t, drv, "b").Count; got != 20 {
		t.Errorf("非目标行 b 期望 count=20, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "c").Count; got != 30 {
		t.Errorf("非目标行 c 期望 count=30, 实际 %d", got)
	}
}

// ── Updates 混合 map：表达式键 + 普通键同时生效 ──────────────────────

func TestXormExpr_UpdatesMixedMap(t *testing.T) {
	drv := newExprModelDriver(t)
	err := drv.Query().Table("xorm_expr_model").Where("name = ?", "b").
		Updates(map[string]any{
			"count": contracts.Expr("count * ?", 2), // 表达式：20 → 40
			"name":  "b-renamed",                    // 普通值绑定
		})
	if err != nil {
		t.Fatalf("Updates 混合 map: %v", err)
	}
	row := exprRowByName(t, drv, "b-renamed")
	if row.Count != 40 {
		t.Errorf("重命名行期望 count=40（表达式生效）, 实际 %d", row.Count)
	}
	// 其余行不受影响
	if got := exprRowByName(t, drv, "a").Count; got != 10 {
		t.Errorf("行 a 期望 count=10, 实际 %d", got)
	}
}

// ── OrWhere 组合的 WHERE 正确性：两行各 +1 ───────────────────────────

func TestXormExpr_OrWhereCombination(t *testing.T) {
	drv := newExprModelDriver(t)
	if err := drv.Query().Table("xorm_expr_model").
		Where("name = ?", "a").
		OrWhere("name = ?", "c").
		Update("count", contracts.Expr("count + ?", 1)); err != nil {
		t.Fatalf("OrWhere+Update 表达式: %v", err)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 11 {
		t.Errorf("行 a 期望 count=11, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "c").Count; got != 31 {
		t.Errorf("行 c 期望 count=31, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "b").Count; got != 20 {
		t.Errorf("行 b 不应被 OR 命中, 期望 count=20, 实际 %d", got)
	}
}

// ── Not 取反组合（builder.Not 通道，含 IN 切片展开）──────────────────

func TestXormExpr_NotCombination(t *testing.T) {
	drv := newExprModelDriver(t)
	if err := drv.Query().Table("xorm_expr_model").
		Where("count > ?", 0).
		Not("name IN ?", []string{"a", "b"}).
		Update("count", contracts.Expr("count + ?", 100)); err != nil {
		t.Fatalf("Not+Update 表达式: %v", err)
	}
	if got := exprRowByName(t, drv, "c").Count; got != 130 {
		t.Errorf("行 c 期望 count=130, 实际 %d", got)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 10 {
		t.Errorf("行 a 被 NOT 排除后不应变更, 期望 count=10, 实际 %d", got)
	}
}

// ── Result 变体：RowsAffected 回填与 IsZeroRow ───────────────────────

func TestXormExpr_UpdateResultRowsAffected(t *testing.T) {
	drv := newExprModelDriver(t)
	q := drv.Query()

	res := q.Model(&XormExprModel{}).Where("name = ?", "a").
		UpdateResult("count", contracts.Expr("count + ?", 7))
	if res.Error != nil {
		t.Fatalf("UpdateResult 表达式: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("UpdateResult 期望 RowsAffected=1, 实际 %d", res.RowsAffected)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 17 {
		t.Errorf("行 a 期望 count=17, 实际 %d", got)
	}

	// UpdatesResult 混合表达式同样回填行数
	res = q.Model(&XormExprModel{}).Where("name = ?", "b").
		UpdatesResult(map[string]any{"count": contracts.Expr("count - ?", 5)})
	if res.Error != nil {
		t.Fatalf("UpdatesResult 表达式: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("UpdatesResult 期望 RowsAffected=1, 实际 %d", res.RowsAffected)
	}

	// WHERE 未命中：成功但 0 行，IsZeroRow 判定
	res = q.Model(&XormExprModel{}).Where("name = ?", "missing").
		UpdateResult("count", contracts.Expr("count + ?", 1))
	if res.Error != nil {
		t.Fatalf("未命中 UpdateResult 不应报错: %v", res.Error)
	}
	if !res.IsZeroRow() {
		t.Errorf("未命中期望 IsZeroRow, 实际 RowsAffected=%d", res.RowsAffected)
	}
}

// ── ExecResult：INSERT 行数与 UPDATE 未命中判定 ──────────────────────

func TestXormExpr_ExecResult(t *testing.T) {
	drv := newExprModelDriver(t)
	q := drv.Query()

	res := q.ExecResult("INSERT INTO xorm_expr_model (count, name) VALUES (?, ?)", 99, "ins")
	if res.Error != nil {
		t.Fatalf("ExecResult INSERT: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("ExecResult INSERT 期望 RowsAffected=1, 实际 %d", res.RowsAffected)
	}
	if got := exprRowByName(t, drv, "ins"); got.Count != 99 {
		t.Errorf("ExecResult 插入行回读期望 count=99, 实际 %d", got.Count)
	}

	// UPDATE 未命中 0 行：Error 为 nil 且 IsZeroRow
	res = q.ExecResult("UPDATE xorm_expr_model SET count = count + 1 WHERE name = ?", "nope")
	if res.Error != nil {
		t.Fatalf("ExecResult UPDATE 未命中不应报错: %v", res.Error)
	}
	if !res.IsZeroRow() {
		t.Errorf("ExecResult 未命中期望 IsZeroRow, 实际 RowsAffected=%d", res.RowsAffected)
	}
}

// ── 非表达式值路径回归：普通 Update/Updates 行为不变 ─────────────────

func TestXormExpr_UpdatePlainValueRegression(t *testing.T) {
	drv := newExprModelDriver(t)
	q := drv.Query()

	if err := q.Table("xorm_expr_model").Where("name = ?", "a").Update("count", 42); err != nil {
		t.Fatalf("普通 Update: %v", err)
	}
	if got := exprRowByName(t, drv, "a").Count; got != 42 {
		t.Errorf("普通 Update 期望 count=42, 实际 %d", got)
	}

	if err := q.Table("xorm_expr_model").Where("name = ?", "b").
		Updates(map[string]any{"count": int64(7), "name": "b2"}); err != nil {
		t.Fatalf("普通 Updates: %v", err)
	}
	row := exprRowByName(t, drv, "b2")
	if row.Count != 7 {
		t.Errorf("普通 Updates 期望 count=7, 实际 %d", row.Count)
	}
}
