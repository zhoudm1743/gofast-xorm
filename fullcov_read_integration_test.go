//go:build integration

package xormdriver

// fullcov_read_integration_test.go —— contracts.Query 读终结方法真实库全覆盖
// （分组 2：Find / First / Last / Take / Count / Scan / Pluck / Row / Rows /
// ScanMap / Exists）。
//
// 与 model_pk_integration_test.go（X-09）同款方法论：每个用例 DROP 重建表、
// 断言一律回读真实数据验证。语义以驱动实现为准（query_read.go /
// query_scan.go / query_builder*.go 先读后写）：
//
//   - First/Last/Take 经 getOne 共用实现：First 按主键升序、Last 降序取首行、
//     Take 不排序；均强制 LIMIT 1，未命中映射 contracts.ErrRecordNotFound
//   - Count 要求链上显式 Table()/Model()，剥离链上 ORDER BY（X-06，PG 42803）
//   - Scan 单 struct 指针走 Get（未命中不报错、dest 保持零值）、集合走 Find、
//     标量 dest 走首行首列提取（scanScalar）
//   - Pluck/ScanMap 基于 xorm QueryInterface 的 []map[string]any 结果；
//     ScanMap 追加进 dest（非替换）；文本列经 NullString 容器归一为 string，
//     NULL 文本归一为空串（xorm convert.Interface2Interface 语义）
//   - Row/Rows 仅支持 Raw() 原生链（直接走 engine.DB() 连接池，不参与缓存）；
//     构建器链 Row() 返回 errorRow 延迟到 Scan 报错、Rows() 立即报错
//   - Exists 未命中返回 (false, nil)；LIMIT 1 短路计数，不走查询缓存
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovRead' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovRead' -v .

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型（默认 xorm tag identifier；表名 fc2_ 分组前缀）────────────

// fc2User 主模型：文本主键（升降序语义跨方言确定）、数值列、可空文本列
// （note 供 ScanMap 的 NULL 表示断言）。
type fc2User struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`
	Cnt  int64  `xorm:"bigint 'cnt'"`
	Note string `xorm:"varchar(64) 'note' null"`
}

func (fc2User) TableName() string { return "fc2_users" }

// fc2UserProj 投影结构体：与 fc2_users 同列布局、无 TableName，
// 查询须显式 Table()（表名兜底按 mapper 推导会得到不存在的 fc2_user_proj）。
type fc2UserProj struct {
	ID   string `xorm:"'id'"`
	Name string `xorm:"'name'"`
}

// ── 种子基建 ─────────────────────────────────────────────────────────

// fc2Reset 清表重建 fc2_users（用例开始前 DROP 重建，t.Cleanup 再清一次）。
func fc2Reset(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXDropTables(t, drv, "fc2_users")
	if err := drv.AutoMigrate(&fc2User{}); err != nil {
		t.Fatalf("AutoMigrate fc2_users: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, "fc2_users") })
}

// fc2SeedUsers 灌入三行基线：插入顺序 3→1→2 与主键顺序 1<2<3 错位，
// 使 First/Last 的排序断言能区分"显式主键排序"与"存储序/插入序"。
// bob 的 note 以原生 SQL 显式写 NULL（xorm struct Insert 会把空串写为 ''，
// 无法经 Create 造出数据库 NULL）。
func fc2SeedUsers(t *testing.T, drv *XormDriver) {
	t.Helper()
	q := drv.Query()
	if err := q.Create(&fc2User{ID: "3", Name: "carol", Cnt: 30, Note: "note3"}); err != nil {
		t.Fatalf("种子 3: %v", err)
	}
	if err := q.Create(&fc2User{ID: "1", Name: "alice", Cnt: 10, Note: "note1"}); err != nil {
		t.Fatalf("种子 1: %v", err)
	}
	intgXMustExec(t, drv,
		`INSERT INTO fc2_users (id, name, cnt, note) VALUES (?, ?, ?, ?)`,
		"2", "bob", int64(20), nil)
}

// mustFC2Read 按 id 回读单行（断言辅助，链上显式 Where 不依赖主键路径）。
func mustFC2Read(t *testing.T, q contracts.Query, id string) fc2User {
	t.Helper()
	var row fc2User
	if err := q.Model(&fc2User{}).Where("id = ?", id).First(&row); err != nil {
		t.Fatalf("回读 id=%q: %v", id, err)
	}
	return row
}

// fc2NamesByID 将查询结果整理为 id→name 映射（行序无关断言辅助）。
func fc2NamesByID(rows []fc2User) map[string]string {
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.ID] = r.Name
	}
	return m
}

// ── 共享 runner：PG/MySQL 双入口均执行同一矩阵 ────────────────────────

func runFullCovReadMatrix(t *testing.T, drv *XormDriver) {
	t.Helper()

	// ── Find ──────────────────────────────────────────────────────
	t.Run("Find_无条件全量_conds变参_投影_空表", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// 无条件全量：3 行全回（行序不保证，按集合断言）
		var all []fc2User
		if err := q.Model(&fc2User{}).Find(&all); err != nil {
			t.Fatalf("Find 全量: %v", err)
		}
		names := fc2NamesByID(all)
		if len(names) != 3 || names["1"] != "alice" || names["2"] != "bob" || names["3"] != "carol" {
			t.Errorf("Find 全量应返回 3 行基线数据, 实际 %+v", all)
		}

		// 指针元素切片形态
		var allPtrs []*fc2User
		if err := q.Model(&fc2User{}).Find(&allPtrs); err != nil {
			t.Fatalf("Find []*T: %v", err)
		}
		if len(allPtrs) != 3 {
			t.Errorf("Find []*T 应返回 3 行, 实际 %d", len(allPtrs))
		}

		// conds 变参单条件
		var one []fc2User
		if err := q.Model(&fc2User{}).Find(&one, "id = ?", "2"); err != nil {
			t.Fatalf("Find conds: %v", err)
		}
		if len(one) != 1 || one[0].Name != "bob" || one[0].Cnt != 20 {
			t.Errorf("Find conds 应命中 bob, 实际 %+v", one)
		}

		// conds 与链上 Where 取 AND 交集（name=bob 且 cnt=20 → 命中；cnt>=30 → 空）
		var and []fc2User
		if err := q.Model(&fc2User{}).Where("name = ?", "bob").Find(&and, "cnt >= ?", int64(20)); err != nil {
			t.Fatalf("Find conds+Where: %v", err)
		}
		if len(and) != 1 || and[0].ID != "2" {
			t.Errorf("conds 与链上 Where 应取 AND, 实际 %+v", and)
		}
		var andEmpty []fc2User
		if err := q.Model(&fc2User{}).Where("name = ?", "bob").Find(&andEmpty, "cnt >= ?", int64(30)); err != nil {
			t.Fatalf("Find conds+Where 空集: %v", err)
		}
		if len(andEmpty) != 0 {
			t.Errorf("AND 空集应 0 行, 实际 %+v", andEmpty)
		}

		// 投影结构体切片（显式 Table + Select + Order）
		var projs []fc2UserProj
		if err := q.Table("fc2_users").Select("id", "name").
			Where("cnt >= ?", int64(20)).Order("id").Find(&projs); err != nil {
			t.Fatalf("Find 投影: %v", err)
		}
		if len(projs) != 2 || projs[0].ID != "2" || projs[0].Name != "bob" || projs[1].Name != "carol" {
			t.Errorf("投影 Find 应按 id 升序返回 bob/carol, 实际 %+v", projs)
		}

		// 空表 → len 0 不报错（xorm 对零行不预置切片，nil/空均可，断言长度）
		fc2Reset(t, drv)
		var empty []fc2User
		if err := q.Model(&fc2User{}).Find(&empty); err != nil {
			t.Fatalf("空表 Find: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("空表 Find 应 0 行, 实际 %+v", empty)
		}
		// 空表 + conds 同样 0 行
		var emptyConds []fc2User
		if err := q.Model(&fc2User{}).Find(&emptyConds, "id = ?", "x"); err != nil {
			t.Fatalf("空表 Find conds: %v", err)
		}
		if len(emptyConds) != 0 {
			t.Errorf("空表 Find conds 应 0 行, 实际 %+v", emptyConds)
		}
	})

	// ── First / Last / Take ───────────────────────────────────────
	t.Run("FirstLastTake_主键序_conds_未命中", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// 乱序插入 [3,1,2]：First 必须主键升序取 1、Last 主键降序取 3
		var first fc2User
		if err := q.Model(&fc2User{}).First(&first); err != nil {
			t.Fatalf("First: %v", err)
		}
		if first.ID != "1" || first.Name != "alice" {
			t.Errorf("First 应按主键升序取 id=1, 实际 %+v", first)
		}
		var last fc2User
		if err := q.Model(&fc2User{}).Last(&last); err != nil {
			t.Fatalf("Last: %v", err)
		}
		if last.ID != "3" || last.Name != "carol" {
			t.Errorf("Last 应按主键降序取 id=3, 实际 %+v", last)
		}

		// Take 不排序：取任意一行且数据完整
		var take fc2User
		if err := q.Model(&fc2User{}).Take(&take); err != nil {
			t.Fatalf("Take: %v", err)
		}
		switch take.ID {
		case "1", "2", "3":
		default:
			t.Errorf("Take 应取到基线三行之一, 实际 %+v", take)
		}

		// 三者带 conds：命中正确行
		if err := q.Model(&fc2User{}).First(&first, "id = ?", "2"); err != nil {
			t.Fatalf("First conds: %v", err)
		}
		if first.Name != "bob" {
			t.Errorf("First conds 应命中 bob, 实际 %+v", first)
		}
		if err := q.Model(&fc2User{}).Last(&last, "cnt = ?", int64(10)); err != nil {
			t.Fatalf("Last conds: %v", err)
		}
		if last.ID != "1" {
			t.Errorf("Last conds 应命中 alice, 实际 %+v", last)
		}
		var take2 fc2User
		if err := q.Model(&fc2User{}).Take(&take2, "id = ?", "3"); err != nil {
			t.Fatalf("Take conds: %v", err)
		}
		if take2.Name != "carol" {
			t.Errorf("Take conds 应命中 carol, 实际 %+v", take2)
		}

		// 三者未命中：均映射 contracts.ErrRecordNotFound
		for _, tc := range []struct {
			name string
			fn   func(dest any, conds ...any) error
		}{
			{"First", func(dest any, conds ...any) error { return q.Model(&fc2User{}).First(dest, conds...) }},
			{"Last", func(dest any, conds ...any) error { return q.Model(&fc2User{}).Last(dest, conds...) }},
			{"Take", func(dest any, conds ...any) error { return q.Model(&fc2User{}).Take(dest, conds...) }},
		} {
			var miss fc2User
			err := tc.fn(&miss, "id = ?", "no-such-id")
			if !errors.Is(err, contracts.ErrRecordNotFound) {
				t.Errorf("%s 未命中应返回 ErrRecordNotFound, 实际 %v", tc.name, err)
			}
		}

		// 空表 First：同样 ErrRecordNotFound（Last/Take 同实现，抽其一即可）
		fc2Reset(t, drv)
		var none fc2User
		if err := q.Model(&fc2User{}).First(&none); !errors.Is(err, contracts.ErrRecordNotFound) {
			t.Errorf("空表 First 应返回 ErrRecordNotFound, 实际 %v", err)
		}
	})

	// ── Count ─────────────────────────────────────────────────────
	t.Run("Count_无条件_链上条件_Order剥离_空表", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// 无条件（Model 链）
		var n int64
		if err := q.Model(&fc2User{}).Count(&n); err != nil {
			t.Fatalf("Count 无条件: %v", err)
		}
		if n != 3 {
			t.Errorf("Count 应 3, 实际 %d", n)
		}

		// 链上 Where string 条件
		var nCond int64
		if err := q.Model(&fc2User{}).Where("cnt >= ?", int64(20)).Count(&nCond); err != nil {
			t.Fatalf("Count 条件: %v", err)
		}
		if nCond != 2 {
			t.Errorf("Count cnt>=20 应 2, 实际 %d", nCond)
		}

		// 链上 Where map 等值条件（builder.Eq 通道）
		var nMap int64
		if err := q.Model(&fc2User{}).Where(map[string]any{"name": "bob"}).Count(&nMap); err != nil {
			t.Fatalf("Count map 条件: %v", err)
		}
		if nMap != 1 {
			t.Errorf("Count name=bob 应 1, 实际 %d", nMap)
		}

		// 无命中条件 → 0（不报错）
		var nZero int64
		if err := q.Model(&fc2User{}).Where("cnt > ?", int64(1000)).Count(&nZero); err != nil {
			t.Fatalf("Count 无命中: %v", err)
		}
		if nZero != 0 {
			t.Errorf("Count 无命中应 0, 实际 %d", nZero)
		}

		// 链上 ORDER BY：Count 必须剥离排序（PG 下聚合列不在排序列会 42803，X-06）
		var nOrder int64
		if err := q.Model(&fc2User{}).Order("name").Count(&nOrder); err != nil {
			t.Fatalf("Count Order(name): %v", err)
		}
		if nOrder != 3 {
			t.Errorf("Count Order(name) 应 3, 实际 %d", nOrder)
		}

		// Table 显式链等价
		var nTable int64
		if err := q.Table("fc2_users").Count(&nTable); err != nil {
			t.Fatalf("Count Table: %v", err)
		}
		if nTable != 3 {
			t.Errorf("Count Table 应 3, 实际 %d", nTable)
		}

		// 空表 → 0 不报错
		fc2Reset(t, drv)
		var nEmpty int64
		if err := q.Model(&fc2User{}).Count(&nEmpty); err != nil {
			t.Fatalf("空表 Count: %v", err)
		}
		if nEmpty != 0 {
			t.Errorf("空表 Count 应 0, 实际 %d", nEmpty)
		}

		// 裸链（无 Table/Model）→ 报错（驱动注释：Count 无 dest 可兜底推导表名）
		var nBare int64
		if err := q.Count(&nBare); err == nil {
			t.Error("无表 Count 应报错（要求链上显式 Table()/Model()）")
		}
	})

	// ── Scan ──────────────────────────────────────────────────────
	t.Run("Scan_标量_结构体_切片_未命中零值", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// Raw 标量（count 聚合）
		var cnt int64
		if err := q.Raw("SELECT count(*) FROM fc2_users WHERE cnt >= ?", int64(20)).Scan(&cnt); err != nil {
			t.Fatalf("Scan 标量: %v", err)
		}
		if cnt != 2 {
			t.Errorf("Scan count 应 2, 实际 %d", cnt)
		}

		// Raw 标量（文本列）
		var name string
		if err := q.Raw("SELECT name FROM fc2_users WHERE id = ?", "1").Scan(&name); err != nil {
			t.Fatalf("Scan 文本标量: %v", err)
		}
		if name != "alice" {
			t.Errorf("Scan name 应 alice, 实际 %q", name)
		}

		// 链式标量（Select 单列）
		var sel int64
		if err := q.Model(&fc2User{}).Select("cnt").Where("id = ?", "2").Scan(&sel); err != nil {
			t.Fatalf("Scan 链式标量: %v", err)
		}
		if sel != 20 {
			t.Errorf("Scan Select(cnt) 应 20, 实际 %d", sel)
		}

		// Raw 单行 → 单 struct（Get 路径，投影列缺失字段保持零值）
		var rawOne fc2User
		if err := q.Raw("SELECT id, name FROM fc2_users WHERE id = ?", "1").Scan(&rawOne); err != nil {
			t.Fatalf("Scan Raw 单行: %v", err)
		}
		if rawOne.ID != "1" || rawOne.Name != "alice" {
			t.Errorf("Scan Raw 单行应命中 alice, 实际 %+v", rawOne)
		}

		// 链式单 struct
		var chainOne fc2User
		if err := q.Model(&fc2User{}).Where("id = ?", "3").Scan(&chainOne); err != nil {
			t.Fatalf("Scan 链式单行: %v", err)
		}
		if chainOne.Name != "carol" || chainOne.Cnt != 30 {
			t.Errorf("Scan 链式单行应命中 carol, 实际 %+v", chainOne)
		}

		// dest 复用回归：Scan 前置 NoAutoCondition 后，已填充 dest 只作输出，
		// 不得把旧值并入 WHERE 导致二次查询误报/错行（修复锁定）
		var reused fc2User
		if err := q.Model(&fc2User{}).Where("id = ?", "1").Scan(&reused); err != nil {
			t.Fatalf("Scan 复用首次: %v", err)
		}
		if reused.Name != "alice" {
			t.Fatalf("Scan 复用首次应命中 alice, 实际 %+v", reused)
		}
		if err := q.Model(&fc2User{}).Where("id = ?", "2").Scan(&reused); err != nil {
			t.Fatalf("Scan 复用二次: %v", err)
		}
		if reused.Name != "bob" {
			t.Errorf("Scan 复用二次应覆盖为 bob（旧值不得作条件）, 实际 %+v", reused)
		}

		// 链式集合（有序）
		var rows []fc2User
		if err := q.Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Scan(&rows); err != nil {
			t.Fatalf("Scan 集合: %v", err)
		}
		if len(rows) != 2 || rows[0].Name != "bob" || rows[1].Name != "carol" {
			t.Errorf("Scan 集合应按 id 升序返回 bob/carol, 实际 %+v", rows)
		}

		// 单 struct 未命中：Get found=false → 不报错、dest 保持零值（gorm Scan 语义）
		var miss fc2User
		if err := q.Model(&fc2User{}).Where("id = ?", "zz").Scan(&miss); err != nil {
			t.Fatalf("Scan 未命中不应报错: %v", err)
		}
		if miss.ID != "" || miss.Name != "" || miss.Cnt != 0 {
			t.Errorf("Scan 未命中 dest 应保持零值, 实际 %+v", miss)
		}

		// 空表集合：0 行不报错
		fc2Reset(t, drv)
		var empty []fc2User
		if err := q.Model(&fc2User{}).Scan(&empty); err != nil {
			t.Fatalf("空表 Scan: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("空表 Scan 应 0 行, 实际 %+v", empty)
		}
		// 空表 Raw 标量 → 0（保持 dest 零值不报错）
		var n0 int64 = -1
		if err := q.Raw("SELECT count(*) FROM fc2_users").Scan(&n0); err != nil {
			t.Fatalf("空表 Scan 标量: %v", err)
		}
		if n0 != 0 {
			t.Errorf("空表 Scan 标量应 0, 实际 %d", n0)
		}
	})

	// ── Pluck ─────────────────────────────────────────────────────
	t.Run("Pluck_单列_条件_空结果", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// 文本列 → []string（Order 保证行序确定性）
		var names []string
		if err := q.Model(&fc2User{}).Order("id").Pluck("name", &names); err != nil {
			t.Fatalf("Pluck name: %v", err)
		}
		if len(names) != 3 || names[0] != "alice" || names[1] != "bob" || names[2] != "carol" {
			t.Errorf("Pluck name 应按 id 序返回 [alice bob carol], 实际 %v", names)
		}

		// 数值列 → []int64
		var cnts []int64
		if err := q.Model(&fc2User{}).Order("id").Pluck("cnt", &cnts); err != nil {
			t.Fatalf("Pluck cnt: %v", err)
		}
		if len(cnts) != 3 || cnts[0] != 10 || cnts[1] != 20 || cnts[2] != 30 {
			t.Errorf("Pluck cnt 应按 id 序返回 [10 20 30], 实际 %v", cnts)
		}

		// 链上 Where 条件过滤
		var filtered []string
		if err := q.Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Pluck("name", &filtered); err != nil {
			t.Fatalf("Pluck 条件: %v", err)
		}
		if len(filtered) != 2 || filtered[0] != "bob" || filtered[1] != "carol" {
			t.Errorf("Pluck cnt>=20 应按 id 序返回 [bob carol], 实际 %v", filtered)
		}

		// 无命中条件 → 空结果不报错
		var none []string
		if err := q.Model(&fc2User{}).Where("id = ?", "zz").Pluck("name", &none); err != nil {
			t.Fatalf("Pluck 无命中: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("Pluck 无命中应 0 值, 实际 %v", none)
		}

		// 空表 → 空结果不报错
		fc2Reset(t, drv)
		var empty []string
		if err := q.Model(&fc2User{}).Pluck("name", &empty); err != nil {
			t.Fatalf("空表 Pluck: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("空表 Pluck 应 0 值, 实际 %v", empty)
		}
	})

	// ── Row / Rows ────────────────────────────────────────────────
	t.Run("RowRows_Raw路径_构建器路径_游标生命周期", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		// Row()：Raw 原生链单行扫描
		var id, name string
		if err := q.Raw("SELECT id, name FROM fc2_users WHERE id = ?", "1").Row().Scan(&id, &name); err != nil {
			t.Fatalf("Row().Scan: %v", err)
		}
		if id != "1" || name != "alice" {
			t.Errorf("Row() 应扫描到 alice, 实际 %q/%q", id, name)
		}

		// Row()：Raw 未命中 → *sql.Row 的 sql.ErrNoRows 原样透出
		var missID, missName string
		if err := q.Raw("SELECT id, name FROM fc2_users WHERE id = ?", "zz").
			Row().Scan(&missID, &missName); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("Row() 未命中应返回 sql.ErrNoRows, 实际 %v", err)
		}

		// Row()：构建器链 → errorRow 延迟到 Scan 报错（*sql.Row 语义对齐）
		if err := q.Model(&fc2User{}).Where("id = ?", "1").Row().Scan(&id, &name); err == nil ||
			!strings.Contains(err.Error(), "仅支持 Raw") {
			t.Errorf("构建器链 Row().Scan 应报错提示仅支持 Raw, 实际 %v", err)
		}

		// Rows()：Raw 原生链游标迭代（Next/Scan/Close/Columns）
		rows, err := q.Raw("SELECT id, name, cnt FROM fc2_users WHERE cnt >= ? ORDER BY id", int64(10)).Rows()
		if err != nil {
			t.Fatalf("Rows(): %v", err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("Rows().Columns: %v", err)
		}
		if len(cols) != 3 || cols[0] != "id" || cols[1] != "name" || cols[2] != "cnt" {
			t.Errorf("Rows() 列名异常: %v", cols)
		}
		type rowT struct{ id, name string; cnt int64 }
		var got []rowT
		for rows.Next() {
			var r rowT
			if err := rows.Scan(&r.id, &r.name, &r.cnt); err != nil {
				t.Fatalf("Rows().Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("Rows().Close: %v", err)
		}
		if len(got) != 3 || got[0].name != "alice" || got[1].name != "bob" || got[2].name != "carol" {
			t.Errorf("Rows() 应按 id 序返回 3 行, 实际 %+v", got)
		}

		// Rows()：Raw 空结果集 → 迭代 0 行无错
		emptyRows, err := q.Raw("SELECT id FROM fc2_users WHERE id = ?", "zz").Rows()
		if err != nil {
			t.Fatalf("Rows() 空集: %v", err)
		}
		n := 0
		for emptyRows.Next() {
			n++
		}
		_ = emptyRows.Close()
		if n != 0 {
			t.Errorf("Rows() 空集应 0 行, 实际 %d", n)
		}

		// Rows()：构建器链 → 立即报错
		if _, err := q.Model(&fc2User{}).Where("id = ?", "1").Rows(); err == nil ||
			!strings.Contains(err.Error(), "仅支持 Raw") {
			t.Errorf("构建器链 Rows() 应报错提示仅支持 Raw, 实际 %v", err)
		}
	})

	// ── ScanMap ───────────────────────────────────────────────────
	t.Run("ScanMap_值归一_NULL表示_追加语义", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		var rows []map[string]any
		if err := q.Model(&fc2User{}).Order("id").ScanMap(&rows); err != nil {
			t.Fatalf("ScanMap: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("ScanMap 应 3 行, 实际 %d", len(rows))
		}
		// 行序由 Order("id") 保证：alice → bob → carol
		r0 := rows[0]
		for _, key := range []string{"id", "name", "cnt", "note"} {
			if _, ok := r0[key]; !ok {
				t.Errorf("ScanMap 行应含列 %q, 实际键 %v", key, keysOf(r0))
			}
		}
		// 文本列归一为 string（MySQL 经 []byte→string 转换、PG 原生 string）
		if s, ok := r0["name"].(string); !ok || s != "alice" {
			t.Errorf("ScanMap name 应归一为 string alice, 实际 %T %v", r0["name"], r0["name"])
		}
		// 数值列按 fmt.Sprint 断言（方言容器类型差异：int64/int32 等）
		if fmt.Sprint(r0["cnt"]) != "10" {
			t.Errorf("ScanMap cnt 应 10, 实际 %T %v", r0["cnt"], r0["cnt"])
		}
		// NULL 文本列：xorm NullString 容器 → Interface2Interface 归一为空串
		if s, ok := rows[1]["note"].(string); !ok || s != "" {
			t.Errorf("ScanMap NULL note 应归一为空串, 实际 %T %v", rows[1]["note"], rows[1]["note"])
		}
		if s, ok := rows[2]["note"].(string); !ok || s != "note3" {
			t.Errorf("ScanMap note3 归一异常, 实际 %T %v", rows[2]["note"], rows[2]["note"])
		}

		// dest 追加而非替换：预置一行后追加 3 行
		var appended = []map[string]any{{"pre": "seed"}}
		if err := q.Model(&fc2User{}).ScanMap(&appended); err != nil {
			t.Fatalf("ScanMap 追加: %v", err)
		}
		if len(appended) != 4 || appended[0]["pre"] != "seed" {
			t.Errorf("ScanMap 应追加进 dest（预置 1 + 3）, 实际 %d 行", len(appended))
		}

		// Raw 路径：列名即 SELECT 标签
		var rawRows []map[string]any
		if err := q.Raw("SELECT id, name FROM fc2_users WHERE id = ?", "2").ScanMap(&rawRows); err != nil {
			t.Fatalf("ScanMap Raw: %v", err)
		}
		if len(rawRows) != 1 || rawRows[0]["name"] != "bob" {
			t.Errorf("ScanMap Raw 应命中 bob, 实际 %v", rawRows)
		}

		// 空表 → 0 行追加、不报错
		fc2Reset(t, drv)
		var empty []map[string]any
		if err := q.Model(&fc2User{}).ScanMap(&empty); err != nil {
			t.Fatalf("空表 ScanMap: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("空表 ScanMap 应 0 行, 实际 %v", empty)
		}
	})

	// ── Exists ────────────────────────────────────────────────────
	t.Run("Exists_空表_有数据_conds命中不命中", func(t *testing.T) {
		fc2Reset(t, drv)
		q := drv.Query()

		// 空表 → false, nil
		if ok, err := q.Model(&fc2User{}).Exists(&fc2User{}); err != nil {
			t.Fatalf("空表 Exists: %v", err)
		} else if ok {
			t.Error("空表 Exists 应 false")
		}

		fc2SeedUsers(t, drv)
		// 有数据 → true
		if ok, err := q.Model(&fc2User{}).Exists(&fc2User{}); err != nil {
			t.Fatalf("Exists 有数据: %v", err)
		} else if !ok {
			t.Error("有数据 Exists 应 true")
		}

		// conds 命中 / 不命中
		if ok, err := q.Model(&fc2User{}).Exists(&fc2User{}, "id = ?", "2"); err != nil {
			t.Fatalf("Exists conds 命中: %v", err)
		} else if !ok {
			t.Error("Exists conds 命中应 true")
		}
		if ok, err := q.Model(&fc2User{}).Exists(&fc2User{}, "id = ?", "zz"); err != nil {
			t.Fatalf("Exists conds 不命中: %v", err)
		} else if ok {
			t.Error("Exists conds 不命中应 false")
		}

		// 链上 Where
		if ok, err := q.Model(&fc2User{}).Where("cnt >= ?", int64(30)).Exists(&fc2User{}); err != nil {
			t.Fatalf("Exists 链上 Where: %v", err)
		} else if !ok {
			t.Error("Exists 链上 Where 命中应 true")
		}
		if ok, err := q.Model(&fc2User{}).Where("cnt >= ?", int64(1000)).Exists(&fc2User{}); err != nil {
			t.Fatalf("Exists 链上 Where 不命中: %v", err)
		} else if ok {
			t.Error("Exists 链上 Where 不命中应 false")
		}
	})

	// ── 查询缓存与读终结（真实库 + EnableCaches）────────────────────
	// 放最后：EnableCaches 对驱动实例全局生效且不可关闭，避免影响前面用例。
	t.Run("读终结_查询缓存命中回源与写失效", func(t *testing.T) {
		fc2Reset(t, drv)
		fc2SeedUsers(t, drv)
		q := drv.Query()

		tc := newTestCache()
		if err := drv.EnableCaches(tc); err != nil {
			t.Fatalf("EnableCaches: %v", err)
		}
		store := tc.countCacheStore
		// 注意：XormQuery 在构造时快照驱动 qc，必须在 EnableCaches 之后创建查询
		q = drv.Query()

		// 首次 Find 回源回填（get=1 hit=0 put=1）
		var v1 []fc2User
		if err := q.Cache().Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Find(&v1); err != nil {
			t.Fatalf("缓存 Find 首查: %v", err)
		}
		if len(v1) != 2 || v1[0].Name != "bob" || v1[1].Name != "carol" {
			t.Fatalf("缓存 Find 结果异常: %+v", v1)
		}
		assertCounts(t, store, "Find 首查回源", 1, 0, 1)

		// 同链再查命中缓存（不回源）
		var v2 []fc2User
		if err := q.Cache().Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Find(&v2); err != nil {
			t.Fatalf("缓存 Find 再查: %v", err)
		}
		if len(v2) != 2 {
			t.Fatalf("命中缓存 Find 应 2 行, 实际 %d", len(v2))
		}
		assertCounts(t, store, "Find 再查命中", 2, 1, 1)

		// 绕开驱动直改库（不触发失效）：同链仍命中缓存返回旧结果集
		engineExec(t, drv, `INSERT INTO fc2_users (id, name, cnt, note) VALUES ('4', 'dave', 40, 'note4')`)
		var v3 []fc2User
		if err := q.Cache().Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Find(&v3); err != nil {
			t.Fatalf("缓存 Find 三查: %v", err)
		}
		if len(v3) != 2 {
			t.Errorf("直改库后应命中旧缓存（仍 2 行），实际 %d 行", len(v3))
		}
		assertCounts(t, store, "Find 命中旧值", 3, 2, 1)

		// 经驱动 Create 写终结：失效全部缓存，同链回源读到新行
		if err := q.Create(&fc2User{ID: "5", Name: "eve", Cnt: 50, Note: "note5"}); err != nil {
			t.Fatalf("缓存失效 Create: %v", err)
		}
		var v4 []fc2User
		if err := q.Cache().Model(&fc2User{}).Where("cnt >= ?", int64(20)).Order("id").Find(&v4); err != nil {
			t.Fatalf("缓存失效后 Find: %v", err)
		}
		if len(v4) != 4 || v4[0].Name != "bob" || v4[3].Name != "eve" {
			t.Errorf("写失效后应回源 4 行（bob/carol/dave/eve）, 实际 %+v", v4)
		}
		assertCounts(t, store, "写失效后回源", 4, 2, 2)

		// Count 同样走缓存（标量 dest）
		var total int64
		if err := q.Cache().Model(&fc2User{}).Count(&total); err != nil {
			t.Fatalf("缓存 Count: %v", err)
		}
		if total != 5 {
			t.Errorf("缓存 Count 应 5, 实际 %d", total)
		}
		assertCounts(t, store, "Count 回源", 5, 2, 3)
	})
}

// keysOf 输出 map 键集合（断言诊断辅助）。
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ── PG / MySQL 双入口 ────────────────────────────────────────────────

func TestFullCovRead_PG(t *testing.T) {
	runFullCovReadMatrix(t, newPGXorm(t))
}

func TestFullCovRead_MySQL(t *testing.T) {
	runFullCovReadMatrix(t, newPlainMySQLXorm(t))
}
