package xormdriver

import (
	"errors"
	"sort"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// query_builder_adv_test.go 覆盖聚合与作用域链式能力（Distinct/Group/Having/Scopes/Joins），
// 风格对齐 gormdriver query_regression_test.go 的 TestDistinctGroupHaving/TestScopes。

// seedNames 批量插入 (id, name) 测试数据，任一失败即终止测试（数据是后续断言的前提）。
func seedNames(t *testing.T, q contracts.Query, data ...[2]string) {
	t.Helper()
	for _, d := range data {
		if err := q.Create(&XormTestModel{ID: d[0], Name: d[1]}); err != nil {
			t.Fatalf("插入测试数据 %v 失败: %v", d, err)
		}
	}
}

// ── Distinct + Pluck ───────────────────────────────────────────────

// TestXormAdvDistinctPluck 同 name 记录去重后计数：
// 3 行记录只有 2 个不同 name，Distinct("name") + Pluck 应返回 2 个值。
// DB 无 ORDER BY 保证顺序，断言前先排序以剔除顺序因素。
func TestXormAdvDistinctPluck(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	seedNames(t, q, [2]string{"dp1", "x"}, [2]string{"dp2", "x"}, [2]string{"dp3", "y"})

	var names []string
	if err := q.Model(&XormTestModel{}).Distinct("name").Pluck("name", &names); err != nil {
		t.Fatalf("Distinct Pluck: %v", err)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "x" || names[1] != "y" {
		t.Errorf("Distinct Pluck 期望 [x y], 实际 %v", names)
	}
}

// ── Group + Count ──────────────────────────────────────────────────

// TestXormAdvGroupCount Group("name") 后 Count 的语义结论：xorm v1.4.1 的
// GenCountSQL 在 GroupByStr 非空时生成子查询
//
//	SELECT count(*) FROM (SELECT name FROM xorm_test_model GROUP BY name) sub
//
// 因此 Count 返回的是分组数而非总行数（与 gorm Count+Group 语义一致），
// 无需退回 Scan/ScanMap 断言。3 行记录（x/x/y）共 2 组，Count 应为 2。
// 另以 Scan 聚合交叉验证每组的实际行数（x→2、y→1），钉死"组数"结论不被
// 总行数恰好相等的巧合掩盖。
func TestXormAdvGroupCount(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	seedNames(t, q, [2]string{"g1", "x"}, [2]string{"g2", "x"}, [2]string{"g3", "y"})

	var n int64
	if err := q.Model(&XormTestModel{}).Group("name").Count(&n); err != nil {
		t.Fatalf("Group Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Group 后 Count 期望分组数 2, 实际 %d", n)
	}

	// 交叉验证：每组的 count(*)（总行数 3 ≠ 分组数 2，区分两种语义）
	type agg struct {
		Name  string
		Count int64
	}
	var aggs []agg
	if err := q.Model(&XormTestModel{}).Select("name, count(*) as count").Group("name").Scan(&aggs); err != nil {
		t.Fatalf("Group Scan 聚合: %v", err)
	}
	if len(aggs) != 2 {
		t.Fatalf("Group Scan 期望 2 组, 实际 %+v", aggs)
	}
	byName := map[string]int64{}
	for _, a := range aggs {
		byName[a.Name] = a.Count
	}
	if byName["x"] != 2 || byName["y"] != 1 {
		t.Errorf("分组计数期望 {x:2, y:1}, 实际 %v", byName)
	}
}

// ── Having ─────────────────────────────────────────────────────────

// TestXormAdvHaving 分组后聚合过滤：参照 gormdriver 用 Scan 聚合（name + count(*)）
// 断言 Having 生效。xorm Session.Having 精确签名仅接受无参 string
// （驱动对携带占位符参数的调用在链上报错），故条件写作内联的 "count(*) > 1"，
// 而非 gorm 风格的 "count(*) > ?" + 参数。
func TestXormAdvHaving(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	seedNames(t, q, [2]string{"hv1", "x"}, [2]string{"hv2", "x"}, [2]string{"hv3", "y"})

	type agg struct {
		Name  string
		Count int64
	}
	var aggs []agg
	if err := q.Model(&XormTestModel{}).
		Select("name, count(*) as count").
		Group("name").
		Having("count(*) > 1").
		Scan(&aggs); err != nil {
		t.Fatalf("Group/Having Scan: %v", err)
	}
	if len(aggs) != 1 || aggs[0].Name != "x" || aggs[0].Count != 2 {
		t.Errorf("Having count(*) > 1 期望 [{x 2}], 实际 %+v", aggs)
	}
}

// ── Scopes ─────────────────────────────────────────────────────────

// TestXormAdvScopes 两个 scope 函数串联过滤（name LIKE 与 Limit），断言叠加生效：
// 全量 3 行 → 仅 LIKE scope 2 行 → LIKE + Limit 叠加后 1 行且为排序后首条。
// 逐级断言可区分"两个 scope 都生效"与"最后一个 scope 覆盖前者"两种情况。
// scope 内返回非 *XormQuery 的异常分支属 Scopes 内部防御，无法从 contracts.Query
// 接口构造，不在测试范围。
func TestXormAdvScopes(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	seedNames(t, q, [2]string{"sc0", "apple"}, [2]string{"sc1", "apricot"}, [2]string{"sc2", "banana"})

	nameLikeA := func(q contracts.Query) contracts.Query {
		return q.Where("name LIKE ?", "a%")
	}
	limitOne := func(q contracts.Query) contracts.Query {
		return q.Limit(1)
	}

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Scopes(nameLikeA).Find(&rows); err != nil {
		t.Fatalf("单 scope Find: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("LIKE scope 期望过滤出 2 条, 实际 %d", len(rows))
	}

	rows = nil
	if err := q.Model(&XormTestModel{}).Order("name asc").Scopes(nameLikeA, limitOne).Find(&rows); err != nil {
		t.Fatalf("串联 scope Find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("串联 scope 期望 1 条, 实际 %d", len(rows))
	}
	if rows[0].Name != "apple" {
		t.Errorf("串联 scope 期望排序后首条 apple, 实际 %q", rows[0].Name)
	}
}

// ── Joins ──────────────────────────────────────────────────────────

// TestXormAdvJoins 联表冒烟：xorm SnakeMapper 单数表名下自连接最稳定
// （两侧列名与值一致，规避方言对歧义结果列的差异），LEFT JOIN 自身按主键
// 一一对应，行数应与原表相同。非法 JOIN 串（缺 JOIN/ON 结构）在链上解析
// 失败，终结时经 done 返回 ErrUnsupported 包装错误。
func TestXormAdvJoins(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	seedNames(t, q, [2]string{"j1", "n1"}, [2]string{"j2", "n2"}, [2]string{"j3", "n3"})

	var rows []XormTestModel
	if err := q.Joins("LEFT JOIN xorm_test_model AS t2 ON t2.id = xorm_test_model.id").Find(&rows); err != nil {
		t.Fatalf("自连接 Find: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("自连接一一对应期望 3 行, 实际 %d", len(rows))
	}

	// 非法 JOIN 串：既无 JOIN 关键字也无 ON 条件，解析失败记入链上错误
	err := q.Joins("FOO BAR").Model(&XormTestModel{}).Find(&rows)
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("非法 Joins 串期望 ErrUnsupported, 实际: %v", err)
	}
}
