package xormdriver

import (
	"strconv"
	"testing"

	"xorm.io/xorm/names"
)

// ── Schema 多租户回归：显式表名不被 dest 推导覆盖 ─────────────────────
//
// 对齐 gormdriver 的 TestSchema_ExplicitTable* 系列，把 xorm 驱动 build() 的
// 表名兜底契约钉死：仅当未显式 Table()/Model() 且 schema 非空时才按 dest 推导
// 表名并加前缀，显式表名永远优先。
//
// 回归缺陷（gormdriver applySchema 曾出现）：schema 模式下按 dest 结构体推导
// 表名无条件覆盖显式 Table()/Model()，投影结构体（推导表名与实际表不一致）场景
// 会 42P01 或静默查错表。本文件借 newXormTenantDriver 的诱饵布局捕获最危险的
// 失败模式——不报错但查错表：tenant1.users / tenant1.tenant_user 存 right-table，
// tenant1.user_row 存 wrong-table，任何"按 user_row 等错误表解析"的路径都会读出
// wrong-table 而非预期值。

// newXormSchemaRegressionDriver 在租户驱动上仅对齐列名映射（不影响被测的表名语义）：
// testutil 的回归模型（userRow/tenantUser/tablerUserRow）字段无 xorm tag，引擎默认
// SnakeMapper 会把 ID 映射为 i_d，与租户 DDL 的 id 列不一致——这是列映射问题，与
// 本文件被测的表名路由正交。改用 GonicMapper 后 ID→id、Name→name（仅列级），
// 表名 mapper 保持默认不变，dest 推导表名（user_row/tenant_user，即诱饵表名）与
// testutil 注释完全一致，回归语义不被削弱。
// 注意必须在首次查询前设置：TableInfo 有按类型缓存，查询后再设会读到旧映射。
func newXormSchemaRegressionDriver(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTenantDriver(t)
	drv.engine.SetColumnMapper(names.GonicMapper{})
	return drv
}

// countSchemaTable 各表行数断言辅助：经驱动自身的 Raw+ScanMap 链路取计数。
// 不用 testutil 的 countRaw——其 int64 标量 dest 在 xorm 侧 Scan 中非 struct
// 指针会走 Find（要求切片/map）而报错（基建缺陷，已另行上报），ScanMap 走
// QueryInterface 不受影响，语义等价。
func countSchemaTable(t *testing.T, drv *XormDriver, table string) int64 {
	t.Helper()
	var rows []map[string]any
	if err := drv.Query().Raw("SELECT count(*) AS n FROM " + table).ScanMap(&rows); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if len(rows) != 1 {
		t.Fatalf("count %s 应返回 1 行, 实际 %d", table, len(rows))
	}
	switch n := rows[0]["n"].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		// glebarez/go-sqlite 对 count(*) 返回文本形式，统一解析为数值
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			t.Fatalf("count %s 计数 %q 解析失败: %v", table, n, err)
		}
		return v
	case []byte:
		v, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			t.Fatalf("count %s 计数 %q 解析失败: %v", table, n, err)
		}
		return v
	default:
		t.Fatalf("count %s 计数类型不支持 %T", table, rows[0]["n"])
		return 0
	}
}

// TestXormSchema_ExplicitTableFindNotOverriddenByDest 显式 Table("users") 下，
// 投影 dest userRow 的推导表名 user_row 恰为诱饵表名：若兜底覆盖显式表名，Find
// 会静默查 user_row 返回 wrong-table 而非 users 的 right-table。
// Select 用单个逗号列串：xorm Session.Select 仅接受 string，与 gormdriver
// Select(query, args...) 的多参形态不同（xorm 侧 Select 变参不参与投影）。
func TestXormSchema_ExplicitTableFindNotOverriddenByDest(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var rows []userRow
	if err := drv.Query().Schema("tenant1").Table("users").
		Select("id, name").Where("id = ?", "u1").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("显式 Table(users) 应查 users 表, 期望 [right-table], 实际 %+v", rows)
	}
}

// TestXormSchema_ExplicitTableFirstNotOverriddenByDest 同上场景的单行终结：
// First 的 dest 仍为投影结构体，不得把查询带去诱饵表。
func TestXormSchema_ExplicitTableFirstNotOverriddenByDest(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var row userRow
	if err := drv.Query().Schema("tenant1").Table("users").
		Select("id, name").First(&row); err != nil {
		t.Fatalf("First: %v", err)
	}
	if row.Name != "right-table" {
		t.Errorf("显式 Table(users) First 应查 users 表, 期望 right-table, 实际 %q", row.Name)
	}
}

// TestXormSchema_ExplicitTableCreateNotOverriddenByDest 写路径同样不得被 dest
// 推导改写：插入必须落 users；用两表计数交叉验证——若落进诱饵表，users 行数
// 不变而 user_row 行数变化（或主键冲突），两种失败均可捕获。
func TestXormSchema_ExplicitTableCreateNotOverriddenByDest(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	if err := drv.Query().Schema("tenant1").Table("users").
		Create(&userRow{ID: "u2", Name: "created"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := countSchemaTable(t, drv, "tenant1.users"); got != 2 {
		t.Errorf("Create 应写入 tenant1.users（期望 2 行）, 实际 %d", got)
	}
	if got := countSchemaTable(t, drv, "tenant1.user_row"); got != 1 {
		t.Errorf("Create 不应动 user_row 诱饵表（期望仍 1 行）, 实际 %d", got)
	}
}

// TestXormSchema_ModelFindNotOverriddenByDest Model(&tenantUser{}) 显式定位
// tenant_user：投影 dest userRow（推导表名 user_row）同样不得覆盖显式模型。
func TestXormSchema_ModelFindNotOverriddenByDest(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var rows []userRow
	if err := drv.Query().Schema("tenant1").Model(&tenantUser{}).
		Select("id, name").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("Model(tenantUser) 应查 tenant_user 表, 期望 [right-table], 实际 %+v", rows)
	}
}

// TestXormSchema_TablerDestFallbackApplied 兜底正向用例（TableName() dest）：
// 无显式 Table/Model 时按 dest 解析表名——names.TableName 接口优先于 mapper，
// 解析出 users 后兜底拼前缀应命中 tenant1.users；若兜底未生效则查主库 users
// （不存在）报错，若错误改用字段名推导则命中诱饵表。
func TestXormSchema_TablerDestFallbackApplied(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var rows []tablerUserRow
	if err := drv.Query().Schema("tenant1").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("TableName() dest 应由兜底拼前缀查 users, 期望 [right-table], 实际 %+v", rows)
	}
}

// TestXormSchema_PlainDestFallbackApplied 兜底正向用例（普通 dest）：无显式
// Table/Model 时按推导表名 tenant_user 拼前缀，命中 tenant1.tenant_user 而非
// 诱饵表 user_row。
func TestXormSchema_PlainDestFallbackApplied(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var rows []tenantUser
	if err := drv.Query().Schema("tenant1").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("普通 dest 应兜底推导 tenant_user 并加前缀, 期望 [right-table], 实际 %+v", rows)
	}
}

// TestXormSchema_SchemaAfterTable Schema 后置于 Table 仍应生效：表名前缀延迟
// 到执行期由 applier 读取 q.schema 拼接，调用顺序不影响结果（命中 tenant1.users
// 返回 right-table，而非查主库 users 报 no such table）。
func TestXormSchema_SchemaAfterTable(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	var rows []userRow
	if err := drv.Query().Table("users").Schema("tenant1").
		Select("id, name").Where("id = ?", "u1").Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "right-table" {
		t.Errorf("Table 后置 Schema 应仍带 tenant1 前缀查 users, 期望 [right-table], 实际 %+v", rows)
	}
}

// TestXormSchema_GetSchema GetSchema 返回当前查询上下文的 schema，供业务在原生
// SQL 中自行拼接限定表名；空名按契约视为"不切换"，新链上 Schema("") 后仍为空。
func TestXormSchema_GetSchema(t *testing.T) {
	drv := newXormSchemaRegressionDriver(t)
	if got := drv.Query().Schema("tenant1").GetSchema(); got != "tenant1" {
		t.Errorf("Schema(tenant1) 后 GetSchema 应为 tenant1, 实际 %q", got)
	}
	if got := drv.Query().Schema("").GetSchema(); got != "" {
		t.Errorf(`Schema("") 不切换, GetSchema 应为空, 实际 %q`, got)
	}
	if got := drv.Query().GetSchema(); got != "" {
		t.Errorf("未调用 Schema 时 GetSchema 应为空, 实际 %q", got)
	}
}
