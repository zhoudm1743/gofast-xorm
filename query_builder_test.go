package xormdriver

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/builder"
)

// ── 条件与分页测试基建 ───────────────────────────────────────────────

// builderSeedNames 写入固定的三行 {w1:alice, w2:bob, w3:carol}，
// 供条件类测试以确定性的命中集合断言 AND/OR/NOT 语义。
func builderSeedNames(t *testing.T, q contracts.Query) {
	t.Helper()
	data := []struct{ id, name string }{
		{"w1", "alice"}, {"w2", "bob"}, {"w3", "carol"},
	}
	for _, d := range data {
		if err := q.Create(&XormTestModel{ID: d.id, Name: d.name}); err != nil {
			t.Fatalf("准备数据 %s: %v", d.id, err)
		}
	}
}

// builderSeedPageRows 写入 25 行（ID 为 pg00..pg24，零填充保证字典序即数值序），
// 供分页测试在不依赖插入顺序的情况下断言页大小与页偏移。
func builderSeedPageRows(t *testing.T, q contracts.Query) {
	t.Helper()
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("pg%02d", i)
		if err := q.Create(&XormTestModel{ID: id, Name: "n" + id}); err != nil {
			t.Fatalf("准备数据 %s: %v", id, err)
		}
	}
}

// builderIDs 提取结果 ID 集合并排序：SQLite 无 ORDER BY 时的返回顺序不作为契约，
// 断言只看集合内容。
func builderIDs(rows []XormTestModel) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// builderSameIDs 断言实际 ID 集合与期望一致（顺序无关）。
func builderSameIDs(t *testing.T, rows []XormTestModel, want ...string) {
	t.Helper()
	got := builderIDs(rows)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ID 集合期望 %v, 实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ID 集合期望 %v, 实际 %v", want, got)
		}
	}
}

// ── Where ────────────────────────────────────────────────────────────

// TestXormBuilderWhere 单条件与多条件链式：链上多个 Where 必须是 AND 语义。
func TestXormBuilderWhere(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	// 单条件：精确命中一行
	var single []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name = ?", "alice").Find(&single); err != nil {
		t.Fatalf("Where 单条件: %v", err)
	}
	builderSameIDs(t, single, "w1")

	// 多条件链式：AND 语义——"name LIKE a%" 与 "id != w1" 同时成立的行不存在；
	// 若驱动把链上条件错当成 OR，将多出 w2/w3 两行。
	var none []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name LIKE ?", "a%").Where("id != ?", "w1").Find(&none); err != nil {
		t.Fatalf("Where 多条件: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("多条件链式应为 AND 语义（0 行）, 实际 %d 行: %v", len(none), builderIDs(none))
	}

	// AND 语义正向验证：两个条件同时命中才返回 w1
	var one []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name LIKE ?", "a%").Where("id = ?", "w1").Find(&one); err != nil {
		t.Fatalf("Where 多条件正向: %v", err)
	}
	builderSameIDs(t, one, "w1")
}

// TestXormBuilderWhereMap map 条件：键值对为等值条件，多键之间 AND 组合。
func TestXormBuilderWhereMap(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Where(map[string]any{"name": "bob"}).Find(&rows); err != nil {
		t.Fatalf("Where(map 单键): %v", err)
	}
	builderSameIDs(t, rows, "w2")

	// 多键 map：等值 AND，同时命中才返回
	rows = nil
	if err := q.Model(&XormTestModel{}).Where(map[string]any{"id": "w1", "name": "alice"}).Find(&rows); err != nil {
		t.Fatalf("Where(map 多键): %v", err)
	}
	builderSameIDs(t, rows, "w1")

	// 键值不匹配 → 空结果（证明多键是 AND 而非 OR）
	rows = nil
	if err := q.Model(&XormTestModel{}).Where(map[string]any{"id": "w1", "name": "bob"}).Find(&rows); err != nil {
		t.Fatalf("Where(map 不匹配): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("多键不匹配应 0 行, 实际 %v", builderIDs(rows))
	}
}

// TestXormBuilderOrWhere OrWhere：与前置 Where 构成 OR 并集；无前置 Where 时
// 单独使用同样合法（xorm 空条件 Or 展平后即普通 WHERE）。
func TestXormBuilderOrWhere(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name = ?", "alice").OrWhere("name = ?", "bob").Find(&rows); err != nil {
		t.Fatalf("Where+OrWhere: %v", err)
	}
	builderSameIDs(t, rows, "w1", "w2")

	rows = nil
	if err := q.Model(&XormTestModel{}).OrWhere("name = ?", "bob").Find(&rows); err != nil {
		t.Fatalf("单独 OrWhere: %v", err)
	}
	builderSameIDs(t, rows, "w2")
}

// TestXormBuilderNot Not 取反：string 以 NOT (...) 包裹，builder.Cond 经
// ToSQL 展开后取反，两条路径都应生效。
func TestXormBuilderNot(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Not("name = ?", "carol").Find(&rows); err != nil {
		t.Fatalf("Not(string): %v", err)
	}
	builderSameIDs(t, rows, "w1", "w2")

	rows = nil
	if err := q.Model(&XormTestModel{}).Not(builder.Eq{"name": "alice"}).Find(&rows); err != nil {
		t.Fatalf("Not(builder.Cond): %v", err)
	}
	builderSameIDs(t, rows, "w2", "w3")
}

// TestXormBuilderWhereUnsupportedType 不支持的条件类型在链上即报错（setErr），
// 其后任何终结方法都透传该错误（gorm AddError 语义），且不被后续合法条件覆盖。
func TestXormBuilderWhereUnsupportedType(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	err := q.Model(&XormTestModel{}).Where(123).Find(&rows)
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("Where(int) 应返回 ErrUnsupported, 实际: %v", err)
	}

	// 错误链：setErr 后其余终结方法同样拦截（Count / First）
	var n int64
	if err := q.Model(&XormTestModel{}).Where(123).Count(&n); !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("Where(int) 后 Count 应返回 ErrUnsupported, 实际: %v", err)
	}
	var one XormTestModel
	if err := q.Model(&XormTestModel{}).Where(123).First(&one); !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("Where(int) 后 First 应返回 ErrUnsupported, 实际: %v", err)
	}

	// 首个错误保留：非法 Where 之后的合法条件不得覆盖/清除错误
	err = q.Model(&XormTestModel{}).Where(123).Where("name = ?", "alice").Find(&rows)
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("链上首个错误应保留, 实际: %v", err)
	}

	// OrWhere/Not/Order 的非法类型同样在链上报错
	if err := q.Model(&XormTestModel{}).OrWhere(1.5).Find(&rows); !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("OrWhere(float) 应返回 ErrUnsupported, 实际: %v", err)
	}
	if err := q.Model(&XormTestModel{}).Not([]int{1}).Find(&rows); !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("Not(slice) 应返回 ErrUnsupported, 实际: %v", err)
	}
	if err := q.Model(&XormTestModel{}).Order(123).Find(&rows); !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("Order(int) 应返回 ErrUnsupported, 实际: %v", err)
	}
}

// ── Order / Limit / Offset / Paginate ────────────────────────────────

// TestXormBuilderOrderLimitOffset Order/Limit/Offset 组合：含仅 Limit、仅 Offset
// （驱动以 LIMIT 上限兜底保证 SQL 合法）两种单边场景。
func TestXormBuilderOrderLimitOffset(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	for i := 0; i < 5; i++ {
		id := string(rune('0' + i))
		if err := q.Create(&XormTestModel{ID: id, Name: "n" + id}); err != nil {
			t.Fatalf("准备数据 %s: %v", id, err)
		}
	}

	// 组合：name 降序为 n4,n3,n2,n1,n0；跳过 1 条取 2 条 → n3,n2
	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Order("name desc").Limit(2).Offset(1).Find(&rows); err != nil {
		t.Fatalf("Order+Limit+Offset: %v", err)
	}
	builderSameIDs(t, rows, "3", "2")

	// 仅 Limit
	rows = nil
	if err := q.Model(&XormTestModel{}).Order("name").Limit(2).Find(&rows); err != nil {
		t.Fatalf("仅 Limit: %v", err)
	}
	builderSameIDs(t, rows, "0", "1")

	// 仅 Offset：跳过前 3 条剩 2 条
	rows = nil
	if err := q.Model(&XormTestModel{}).Order("name").Offset(3).Find(&rows); err != nil {
		t.Fatalf("仅 Offset: %v", err)
	}
	builderSameIDs(t, rows, "3", "4")
}

// TestXormBuilderPaginate Paginate：非法页码/页大小归一为默认值（size=20），
// 指定页码/页大小时按 (page-1)*size 偏移取数。
func TestXormBuilderPaginate(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedPageRows(t, q)

	// Paginate(0,0)：归一为第 1 页、每页默认 20 条
	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Order("id").Paginate(0, 0).Find(&rows); err != nil {
		t.Fatalf("Paginate(0,0): %v", err)
	}
	if len(rows) != 20 {
		t.Fatalf("Paginate(0,0) 应默认 size=20, 实际 %d 条", len(rows))
	}
	if rows[0].ID != "pg00" || rows[len(rows)-1].ID != "pg19" {
		t.Errorf("Paginate(0,0) 应为 pg00..pg19, 实际首末: %s..%s", rows[0].ID, rows[len(rows)-1].ID)
	}

	// Paginate(2,10)：跳过前 10 条取 10 条 → pg10..pg19
	rows = nil
	if err := q.Model(&XormTestModel{}).Order("id").Paginate(2, 10).Find(&rows); err != nil {
		t.Fatalf("Paginate(2,10): %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("Paginate(2,10) 应 10 条, 实际 %d 条", len(rows))
	}
	if rows[0].ID != "pg10" || rows[len(rows)-1].ID != "pg19" {
		t.Errorf("Paginate(2,10) 应为 pg10..pg19, 实际首末: %s..%s", rows[0].ID, rows[len(rows)-1].ID)
	}
}

// ── 链式不可变性 ─────────────────────────────────────────────────────

// TestXormBuilderImmutableWhere 写时复制：Where 派生新链，原链与新链分别
// Find 的结果相互独立，原链可安全复用。
func TestXormBuilderImmutableWhere(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	base := q.Model(&XormTestModel{})
	filtered := base.Where("name = ?", "alice")

	var all, only []XormTestModel
	if err := base.Find(&all); err != nil {
		t.Fatalf("原链 Find: %v", err)
	}
	if err := filtered.Find(&only); err != nil {
		t.Fatalf("派生链 Find: %v", err)
	}
	builderSameIDs(t, all, "w1", "w2", "w3")
	builderSameIDs(t, only, "w1")

	// 原链再次执行仍不受派生链影响（appliers 写时复制，未被污染）
	var allAgain []XormTestModel
	if err := base.Find(&allAgain); err != nil {
		t.Fatalf("原链复用 Find: %v", err)
	}
	builderSameIDs(t, allAgain, "w1", "w2", "w3")
}

// TestXormBuilderImmutableOrCombo 不可变 + 条件组合冒烟：Where 后接 OrWhere
// 生成独立派生链，原链（单独 Where）执行不受影响；OR 并集对称且包含单条件结果。
func TestXormBuilderImmutableOrCombo(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	base := q.Model(&XormTestModel{}).Where("name = ?", "alice")
	combined := base.OrWhere("name = ?", "bob")

	// 组合链：Where(alice) OR Where(bob) → w1,w2
	var orRows []XormTestModel
	if err := combined.Find(&orRows); err != nil {
		t.Fatalf("Where+OrWhere 组合: %v", err)
	}
	builderSameIDs(t, orRows, "w1", "w2")

	// 原链执行不受派生影响：仍只是单独 Where(alice) 的结果
	var singleRows []XormTestModel
	if err := base.Find(&singleRows); err != nil {
		t.Fatalf("原链单独 Where: %v", err)
	}
	builderSameIDs(t, singleRows, "w1")

	// 单独 Where 的结果是 OR 并集的子集
	for _, r := range singleRows {
		found := false
		for _, c := range orRows {
			if c.ID == r.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("单独 Where 的 %s 应包含于 OR 并集", r.ID)
		}
	}

	// 对称冒烟：Where(bob).OrWhere(alice) 与上链结果集一致（OR 与条件顺序无关）
	var symRows []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name = ?", "bob").OrWhere("name = ?", "alice").Find(&symRows); err != nil {
		t.Fatalf("对称组合: %v", err)
	}
	builderSameIDs(t, symRows, builderIDs(orRows)...)
}

// ── Select / Omit 投影 ───────────────────────────────────────────────

// TestXormBuilderSelect Select 投影：Select("name") 只查该列（ID 保持零值）；
// Select("*") 对齐 gormdriver 语义回归默认全列，字段完整。
func TestXormBuilderSelect(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Select("name").Find(&rows); err != nil {
		t.Fatalf("Select(name): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Select(name) 应 3 行, 实际 %d", len(rows))
	}
	for _, r := range rows {
		if r.ID != "" {
			t.Errorf("Select(name) 后 ID 未投影应为空, 行 %s/%q", r.Name, r.ID)
		}
		if r.Name == "" {
			t.Errorf("Select(name) 后 Name 应有值, 行 %+v", r)
		}
	}

	// Select("*")：驱动不做 session 写入（no-op），等价默认全列
	var full []XormTestModel
	if err := q.Model(&XormTestModel{}).Select("*").Find(&full); err != nil {
		t.Fatalf("Select(*): %v", err)
	}
	if len(full) != 3 {
		t.Fatalf("Select(*) 应 3 行, 实际 %d", len(full))
	}
	for _, r := range full {
		if r.ID == "" || r.Name == "" {
			t.Errorf("Select(*) 后字段应完整, 实际 %+v", r)
		}
	}
}

// TestXormBuilderOmit Omit 排除列：被排除列保持零值，其余列正常回填。
func TestXormBuilderOmit(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	builderSeedNames(t, q)

	var rows []XormTestModel
	if err := q.Model(&XormTestModel{}).Omit("name").Find(&rows); err != nil {
		t.Fatalf("Omit(name): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Omit 应 3 行, 实际 %d", len(rows))
	}
	for _, r := range rows {
		if r.ID == "" {
			t.Errorf("Omit(name) 后 ID 应有值, 实际 %+v", r)
		}
		if r.Name != "" {
			t.Errorf("Omit(name) 后 Name 应为空, 行 %s Name=%q", r.ID, r.Name)
		}
	}
}
