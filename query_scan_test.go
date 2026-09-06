package xormdriver

import "testing"

// ── Raw + Scan ───────────────────────────────────────────────────────

// TestXormScanRawSlice Raw 原生 SQL 多行结果扫描进结构体切片：
// dest 为 *[]T（非单 struct 指针）走 Find 路径，xorm 在 RawSQL 非空时
// 直接以原生 SQL 执行，列按模型标签映射回字段。
func TestXormScanRawSlice(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "rs1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "rs2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var models []XormTestModel
	if err := q.Raw("SELECT * FROM xorm_test_model ORDER BY id").Scan(&models); err != nil {
		t.Fatalf("Raw+Scan 切片失败: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("期望 2 行, 实际 %d", len(models))
	}
	if models[0].ID != "rs1" || models[0].Name != "alice" {
		t.Errorf("第一行期望 {rs1 alice}, 实际 {%s %s}", models[0].ID, models[0].Name)
	}
	if models[1].ID != "rs2" || models[1].Name != "bob" {
		t.Errorf("第二行期望 {rs2 bob}, 实际 {%s %s}", models[1].ID, models[1].Name)
	}
}

// TestXormScanRawStruct Raw 原生 SQL 扫描进单个 struct 指针（Get 路径）；
// 无命中行时 Get 返回 found=false，dest 保持零值且不报错——这是文档化的
// Scan 单 struct 语义，不归入 ErrRecordNotFound。
func TestXormScanRawStruct(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "rg1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var hit XormTestModel
	if err := q.Raw("SELECT * FROM xorm_test_model WHERE id = ?", "rg1").Scan(&hit); err != nil {
		t.Fatalf("Raw+Scan 单 struct 失败: %v", err)
	}
	if hit.ID != "rg1" || hit.Name != "alice" {
		t.Errorf("期望 {rg1 alice}, 实际 {%s %s}", hit.ID, hit.Name)
	}

	// 无行：零值 + nil 错误
	var miss XormTestModel
	if err := q.Raw("SELECT * FROM xorm_test_model WHERE id = 'missing'").Scan(&miss); err != nil {
		t.Fatalf("无行 Scan 不应报错, 实际: %v", err)
	}
	if miss.ID != "" || miss.Name != "" {
		t.Errorf("无行时应保持零值, 实际 {%q %q}", miss.ID, miss.Name)
	}
}

// TestXormScanChained 链式 Model→Where→Scan 到切片：条件作为 applier 在
// 执行期落到 session，终结仍走 Find 集合路径。
func TestXormScanChained(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "sc1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "sc2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var models []XormTestModel
	if err := q.Model(&XormTestModel{}).Where("name = ?", "alice").Scan(&models); err != nil {
		t.Fatalf("链式 Scan 失败: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("期望 1 行, 实际 %d", len(models))
	}
	if models[0].ID != "sc1" || models[0].Name != "alice" {
		t.Errorf("期望 {sc1 alice}, 实际 {%s %s}", models[0].ID, models[0].Name)
	}
}

// ── Pluck ────────────────────────────────────────────────────────────

// TestXormScanPluck Pluck 单列收集：链上必须显式 Model()（build(nil) 无
// dest 可推导表名）；Order("id") 固定行序以断言列值序列。字符串与主键列
// 两种元素类型都应按查询序收集。
func TestXormScanPluck(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "pl1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "pl2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "pl3", Name: "carol"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var names []string
	if err := q.Model(&XormTestModel{}).Order("id").Pluck("name", &names); err != nil {
		t.Fatalf("Pluck(name) 失败: %v", err)
	}
	wantNames := []string{"alice", "bob", "carol"}
	if len(names) != len(wantNames) {
		t.Fatalf("Pluck(name) 期望 %d 个值, 实际 %d: %v", len(wantNames), len(names), names)
	}
	for i, want := range wantNames {
		if names[i] != want {
			t.Errorf("Pluck(name)[%d] 期望 %s, 实际 %s", i, want, names[i])
		}
	}

	var ids []string
	if err := q.Model(&XormTestModel{}).Order("id").Pluck("id", &ids); err != nil {
		t.Fatalf("Pluck(id) 失败: %v", err)
	}
	wantIDs := []string{"pl1", "pl2", "pl3"}
	if len(ids) != len(wantIDs) {
		t.Fatalf("Pluck(id) 期望 %d 个值, 实际 %d: %v", len(wantIDs), len(ids), ids)
	}
	for i, want := range wantIDs {
		if ids[i] != want {
			t.Errorf("Pluck(id)[%d] 期望 %s, 实际 %s", i, want, ids[i])
		}
	}
}

// ── ScanMap ──────────────────────────────────────────────────────────

// TestXormScanMap ScanMap 输出 []map[string]any（[]byte 已归一为 string）：
// 断言行数与 id/name 键值；空表返回空切片且不报错（追加语义下空结果
// 不动 dest 初始 nil 切片）。
func TestXormScanMap(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "sm001", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "sm002", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var rows []map[string]any
	if err := q.Model(&XormTestModel{}).Order("id").ScanMap(&rows); err != nil {
		t.Fatalf("ScanMap 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 行, 实际 %d", len(rows))
	}
	if rows[0]["id"] != "sm001" {
		t.Errorf("第一行 id 期望 sm001, 实际 %v", rows[0]["id"])
	}
	if rows[0]["name"] != "alice" {
		t.Errorf("第一行 name 期望 alice, 实际 %v", rows[0]["name"])
	}

	// 空表：不报错且不产生行
	var empty []map[string]any
	if err := drv.Query().Model(&XormTestModel{}).Where("id = ?", "nope").ScanMap(&empty); err != nil {
		t.Fatalf("空表 ScanMap 不应报错, 实际: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空表 ScanMap 期望空切片, 实际 %d 行", len(empty))
	}
}

// ── Row / Rows（仅支持 Raw 原生 SQL）─────────────────────────────────

// TestXormScanRow Raw 链 Row() 返回单行游标，Scan 出两列；非 Raw 链上
// Row() 构造不报错，错误延迟到 Scan 时透出（对齐 *sql.Row 语义）。
func TestXormScanRow(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "rw1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "rw2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var id, name string
	if err := q.Raw("SELECT id, name FROM xorm_test_model WHERE id = ?", "rw1").Row().Scan(&id, &name); err != nil {
		t.Fatalf("Row().Scan 失败: %v", err)
	}
	if id != "rw1" || name != "alice" {
		t.Errorf("期望 {rw1 alice}, 实际 {%s %s}", id, name)
	}

	// 无 Raw 链：仅支持 Raw 的文档化语义，Scan 返回错误而非 panic/静默
	if err := drv.Query().Row().Scan(&id, &name); err == nil {
		t.Error("非 Raw 链 Row().Scan 应返回错误, 实际 nil")
	}
}

// TestXormScanRows Raw 链 Rows() 返回多行游标，遍历计数并逐行 Scan 列；
// 非 Raw 链 Rows() 直接返回错误。
func TestXormScanRows(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "rm1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "rm2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	rows, err := q.Raw("SELECT id, name FROM xorm_test_model ORDER BY id").Rows()
	if err != nil {
		t.Fatalf("Rows 失败: %v", err)
	}
	defer rows.Close()

	count := 0
	var firstID, firstName string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("rows.Scan: %v", err)
		}
		if count == 0 {
			firstID, firstName = id, name
		}
		count++
	}
	if count != 2 {
		t.Errorf("Rows 期望遍历 2 条, 实际 %d", count)
	}
	if firstID != "rm1" || firstName != "alice" {
		t.Errorf("首行期望 {rm1 alice}, 实际 {%s %s}", firstID, firstName)
	}

	// 无 Raw 链：直接返回错误
	if _, err := drv.Query().Rows(); err == nil {
		t.Error("非 Raw 链 Rows 应返回错误, 实际 nil")
	}
}

// ── Exec ─────────────────────────────────────────────────────────────

// TestXormScanExec Exec 执行原生 INSERT 后，行数与数据均对后续链式查询生效
// （写终结成功后失效查询缓存，保证不读到旧值）。
func TestXormScanExec(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Exec("INSERT INTO xorm_test_model (id, name) VALUES (?, ?)", "ex1", "alice"); err != nil {
		t.Fatalf("Exec INSERT 失败: %v", err)
	}
	if err := q.Exec("INSERT INTO xorm_test_model (id, name) VALUES (?, ?)", "ex2", "bob"); err != nil {
		t.Fatalf("Exec INSERT 失败: %v", err)
	}

	var n int64
	if err := q.Model(&XormTestModel{}).Count(&n); err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("Exec 后期望 2 行, 实际 %d", n)
	}

	// Exec 写入的数据对后续查询生效
	var models []XormTestModel
	if err := q.Model(&XormTestModel{}).Order("id").Scan(&models); err != nil {
		t.Fatalf("Exec 后查询失败: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("Exec 后期望查到 2 行, 实际 %d", len(models))
	}
	if models[0].ID != "ex1" || models[0].Name != "alice" {
		t.Errorf("期望 {ex1 alice}, 实际 {%s %s}", models[0].ID, models[0].Name)
	}
}
