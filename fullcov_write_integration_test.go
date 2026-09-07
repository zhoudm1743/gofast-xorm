//go:build integration

package xormdriver

// fullcov_write_integration_test.go —— 分组 3：contracts.Query 写终结 + Result
// 变体 + 高级创建 真实库全覆盖（PG/MySQL 双方言矩阵）。
//
// 覆盖方法：Create / CreateInBatches / Save / Update / Updates / Delete /
// CreateResult / UpdateResult / UpdatesResult / DeleteResult / SaveResult /
// FirstOrCreate / FirstOrInit / FindInBatches。
//
// 语义对齐契约：
//   - Update/Updates 一律带显式 Where 限定范围（Model(&bean) 主键并入更新条件
//     属 X-09，已有 model_pk_integration_test.go 覆盖，此处不重复）；
//   - map 传参全键写入（含零值）、struct 传参跳零值（与 gorm/xorm 语义一致）；
//   - Update/Updates 列值支持 contracts.Expr 表达式（X-08，参照 compat_expr_test.go
//     的用法形态，落在本文件自己的 fc3_ 表上）；
//   - Save：主键全零插入 / 非零主键全列更新（AllCols，含零值覆盖）/ 更新 0 行
//     回落插入（X-04 upsert）/ 切片逐元素路由；
//   - FirstOrCreate/FirstOrInit 按条件 Get（命中回填、未命中按 dest 原值插入/
//     保持原值不落库，驱动文档语义）；
//   - Result 变体仅回填 RowsAffected + 错误（错误路径行数保持零值，IsZeroRow
//     判定 命中/未命中）。
//
// 运行（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovWrite' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovWrite' -v .

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型 ─────────────────────────────────────────────────────────
//
// 默认 xorm tag identifier；显式列名 tag + TableName() 固定表名（表名前缀 fc3_，
// 本分组专用，避免与其他分组/任务撞表）。

// fc3User 单主键写测试模型：主键显式字符串 id，非自增——所有用例主键由调用方
// 显式给定，插入路径可控（零主键即空串，两个空串主键互斥即重复键）。
type fc3User struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`
	Cnt  int64  `xorm:"'cnt'"`
}

func (fc3User) TableName() string { return "fc3_users" }

// ── 用例基建 ─────────────────────────────────────────────────────────

// fc3Reset 用例开始前 DROP 重建表；t.Cleanup 清表（防用例中断残留影响他表）。
func fc3Reset(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXDropTables(t, drv, "fc3_users")
	if err := drv.AutoMigrate(&fc3User{}); err != nil {
		t.Fatalf("AutoMigrate fc3_users: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, "fc3_users") })
}

// fc3Seed 重置并灌入基线行（经 Create 写入，顺带覆盖 Create 多行形态）。
func fc3Seed(t *testing.T, drv *XormDriver, rows ...fc3User) {
	t.Helper()
	fc3Reset(t, drv)
	for i := range rows {
		if err := drv.Query().Create(&rows[i]); err != nil {
			t.Fatalf("种子 %+v: %v", rows[i], err)
		}
	}
}

// fc3Read 按主键回读单行（驱动读链路验证落库真实数据）。
func fc3Read(t *testing.T, drv *XormDriver, id string) fc3User {
	t.Helper()
	var row fc3User
	if err := drv.Query().Model(&fc3User{}).Where("id = ?", id).First(&row); err != nil {
		t.Fatalf("回读 id=%q: %v", id, err)
	}
	return row
}

// fc3Expect 断言某行存在且字段值完全一致。
func fc3Expect(t *testing.T, drv *XormDriver, id, wantName string, wantCnt int64) {
	t.Helper()
	row := fc3Read(t, drv, id)
	if row.Name != wantName || row.Cnt != wantCnt {
		t.Errorf("行 %q 期望 (name=%q, cnt=%d), 实际 %+v", id, wantName, wantCnt, row)
	}
}

// fc3Count 原生 SQL 行数（写操作真实落库量）。
func fc3Count(t *testing.T, drv *XormDriver) int64 {
	t.Helper()
	var n int64
	if err := drv.Query().Raw("SELECT count(*) FROM fc3_users").Scan(&n); err != nil {
		t.Fatalf("count fc3_users: %v", err)
	}
	return n
}

// fc3All 全量回读（id 升序），供"未命中行不被波及"断言。
func fc3All(t *testing.T, drv *XormDriver) []fc3User {
	t.Helper()
	var rows []fc3User
	if err := drv.Query().Model(&fc3User{}).Order("id").Find(&rows); err != nil {
		t.Fatalf("全量回读: %v", err)
	}
	return rows
}

// fc3BatchItems 构造 n 个显式主键元素（id 前缀 + 两位序号，名称/计数按序号派生）。
func fc3BatchItems(prefix string, n int) []*fc3User {
	items := make([]*fc3User, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, &fc3User{
			ID:   fmt.Sprintf("%s%02d", prefix, i),
			Name: fmt.Sprintf("n-%s-%d", prefix, i),
			Cnt:  int64(i + 1),
		})
	}
	return items
}

// ── 共享 runner（PG/MySQL 双入口）─────────────────────────────────────

// runFullCovWrite 写终结全覆盖矩阵。dialect 传 "pg"/"mysql"，仅在受影响行数
// 方言语义（MySQL affected=已修改行、PG=命中行）与错误文本断言处分支。
func runFullCovWrite(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	// 顶层兜底清理（子用例各自 fc3Reset/清理，此处防中断残留）
	t.Cleanup(func() { intgXDropTables(t, drv, "fc3_users") })

	t.Run("Create_单条与显式主键不覆盖", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		u := fc3User{ID: "u1", Name: "alice", Cnt: 10}
		if err := q.Create(&u); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if u.ID != "u1" {
			t.Errorf("显式主键不应被驱动覆盖, 实际 %q", u.ID)
		}
		fc3Expect(t, drv, "u1", "alice", 10)

		// 第二条显式主键同样原样落库（互不覆盖）
		if err := q.Create(&fc3User{ID: "u2", Name: "bob", Cnt: 20}); err != nil {
			t.Fatalf("Create u2: %v", err)
		}
		fc3Expect(t, drv, "u2", "bob", 20)
		if n := fc3Count(t, drv); n != 2 {
			t.Errorf("期望 2 行, 实际 %d", n)
		}
	})

	t.Run("Create_重复主键错误映射", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()
		if err := q.Create(&fc3User{ID: "d1", Name: "first", Cnt: 1}); err != nil {
			t.Fatalf("首次 Create: %v", err)
		}
		err := q.Create(&fc3User{ID: "d1", Name: "dup", Cnt: 2})
		if err == nil {
			t.Fatal("重复主键 Create 应报错")
		}
		if !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Errorf("应映射 contracts.ErrDuplicatedKey, 实际: %v", err)
		}
		// 方言错误文本兜底核对（映射来源）
		if dialect == "mysql" {
			if !strings.Contains(err.Error(), "Duplicate entry") {
				t.Errorf("MySQL 应透出 Duplicate entry, 实际: %v", err)
			}
		} else if !strings.Contains(err.Error(), "duplicate key") {
			t.Errorf("PG 应透出 duplicate key, 实际: %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Errorf("失败插入不应落库, 期望 1 行, 实际 %d", n)
		}
	})

	t.Run("CreateInBatches_均分与尾批不足", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		items := fc3BatchItems("b", 5) // 5 条、批大小 2 → 2+2+1
		if err := q.CreateInBatches(&items, 2); err != nil {
			t.Fatalf("CreateInBatches: %v", err)
		}
		if n := fc3Count(t, drv); n != 5 {
			t.Fatalf("期望 5 行, 实际 %d", n)
		}
		// 逐元素回读：尾批（第 5 条）同样完整落库
		for i, it := range items {
			fc3Expect(t, drv, it.ID, fmt.Sprintf("n-b-%d", i), int64(i+1))
		}
	})

	t.Run("CreateInBatches_中途失败前块已提交", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		// 4 条、批大小 2：chunk1=[k1,k2] 提交；chunk2=[k1 重复, k3] 报错即停
		items := []*fc3User{
			{ID: "k1", Name: "one", Cnt: 1},
			{ID: "k2", Name: "two", Cnt: 2},
			{ID: "k1", Name: "dup", Cnt: 3},
			{ID: "k3", Name: "three", Cnt: 4},
		}
		err := q.CreateInBatches(&items, 2)
		if err == nil {
			t.Fatal("块 2 含重复主键应报错")
		}
		if !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Errorf("应映射 ErrDuplicatedKey, 实际: %v", err)
		}
		// 分批语义：已提交块不回滚，失败块原子失败
		if n := fc3Count(t, drv); n != 2 {
			t.Errorf("期望仅前块 2 行落库, 实际 %d", n)
		}
		fc3Expect(t, drv, "k1", "one", 1)
		fc3Expect(t, drv, "k2", "two", 2)
	})

	t.Run("CreateInBatches_非正批量兜底", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		// batchSize=0：兜底为不分批（整切片一次语句），全部落库
		items := fc3BatchItems("p", 3)
		if err := q.CreateInBatches(&items, 0); err != nil {
			t.Fatalf("CreateInBatches(batch=0): %v", err)
		}
		if n := fc3Count(t, drv); n != 3 {
			t.Fatalf("期望 3 行, 实际 %d", n)
		}
		// batchSize<0 同样兜底；整切片单语句含重复 → 原子失败 0 行落库
		bad := []*fc3User{
			{ID: "m1", Name: "a", Cnt: 1},
			{ID: "m1", Name: "dup", Cnt: 2},
			{ID: "m2", Name: "c", Cnt: 3},
		}
		err := q.CreateInBatches(&bad, -1)
		if err == nil || !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Fatalf("负批量整批重复应报 ErrDuplicatedKey, 实际: %v", err)
		}
		if n := fc3Count(t, drv); n != 3 {
			t.Errorf("单语句失败应原子回滚（不分批）, 期望仍 3 行, 实际 %d", n)
		}
	})

	t.Run("CreateInBatches_非切片回落单条Create", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		if err := q.CreateInBatches(&fc3User{ID: "s1", Name: "single", Cnt: 9}, 5); err != nil {
			t.Fatalf("非切片 CreateInBatches: %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Fatalf("期望回落单条 Create 落库 1 行, 实际 %d", n)
		}
		fc3Expect(t, drv, "s1", "single", 9)
		// 回落 Create 的错误语义一致：重复主键仍报 ErrDuplicatedKey
		err := q.CreateInBatches(&fc3User{ID: "s1", Name: "again", Cnt: 1}, 5)
		if err == nil || !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Errorf("回落 Create 重复主键应映射 ErrDuplicatedKey, 实际: %v", err)
		}
	})

	t.Run("Save_零主键走插入", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		u := fc3User{Name: "inserted", Cnt: 7} // 主键零值 → 插入路径
		if err := q.Save(&u); err != nil {
			t.Fatalf("Save(零主键): %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Fatalf("期望插入 1 行, 实际 %d", n)
		}
		row := fc3Read(t, drv, "") // 零主键行以空串主键落库
		if row.Name != "inserted" || row.Cnt != 7 {
			t.Errorf("插入行内容不符: %+v", row)
		}
	})

	t.Run("Save_非零主键更新全列含零值覆盖", func(t *testing.T) {
		fc3Seed(t, drv, fc3User{ID: "a", Name: "alice", Cnt: 10})
		q := drv.Query()

		// AllCols 全列覆盖：零值（name 空串 / cnt 0）也必须写库
		if err := q.Save(&fc3User{ID: "a", Name: "", Cnt: 0}); err != nil {
			t.Fatalf("Save(全列覆盖): %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Fatalf("Save 更新不应新增行, 期望 1 行, 实际 %d", n)
		}
		fc3Expect(t, drv, "a", "", 0)
	})

	t.Run("Save_更新0行回落插入upsert", func(t *testing.T) {
		fc3Seed(t, drv, fc3User{ID: "gone", Name: "x", Cnt: 1})
		q := drv.Query()
		// 先物理删除该行 → Save 更新 0 行 → 回落插入（X-04 upsert）
		if err := q.Delete(&fc3User{ID: "gone"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n := fc3Count(t, drv); n != 0 {
			t.Fatalf("删除后应 0 行, 实际 %d", n)
		}
		if err := q.Save(&fc3User{ID: "gone", Name: "reborn", Cnt: 5}); err != nil {
			t.Fatalf("Save(0 行回落): %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Fatalf("回落插入后应 1 行, 实际 %d", n)
		}
		fc3Expect(t, drv, "gone", "reborn", 5)
	})

	t.Run("Save_切片逐元素路由", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
		)
		q := drv.Query()

		// []fc3User：零主键元素 → 插入；非零主键存在行 → 全列更新
		list1 := []fc3User{
			{Name: "inserted-1", Cnt: 1},
			{ID: "a", Name: "alice-v2", Cnt: 11},
		}
		if err := q.Save(list1); err != nil {
			t.Fatalf("Save([]fc3User): %v", err)
		}
		// []*fc3User：缺失主键元素 → 0 行回落插入（upsert）；存在行 → 更新
		list2 := []*fc3User{
			{ID: "n1", Name: "inserted-2", Cnt: 2},
			{ID: "b", Name: "bob-v2", Cnt: 22},
		}
		res := q.SaveResult(list2)
		if res.Error != nil {
			t.Fatalf("SaveResult([]*fc3User): %v", res.Error)
		}
		if res.RowsAffected != 2 {
			t.Errorf("指针切片 SaveResult 期望 2 行, 实际 %d", res.RowsAffected)
		}
		if n := fc3Count(t, drv); n != 4 {
			t.Fatalf("期望 4 行（2 种子 + 2 插入）, 实际 %d", n)
		}
		fc3Expect(t, drv, "", "inserted-1", 1)
		fc3Expect(t, drv, "a", "alice-v2", 11)
		fc3Expect(t, drv, "n1", "inserted-2", 2)
		fc3Expect(t, drv, "b", "bob-v2", 22)
	})

	t.Run("Update_单列带Where与零值写入", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
			fc3User{ID: "c", Name: "carol", Cnt: 30},
		)
		q := drv.Query()

		if err := q.Model(&fc3User{}).Where("id = ?", "a").Update("name", "alice-v2"); err != nil {
			t.Fatalf("Update(name): %v", err)
		}
		fc3Expect(t, drv, "a", "alice-v2", 10)
		// 零值写入：单列 map 路径无条件写键（cnt → 0）
		if err := q.Table("fc3_users").Where("id = ?", "b").Update("cnt", int64(0)); err != nil {
			t.Fatalf("Update(cnt=0): %v", err)
		}
		fc3Expect(t, drv, "b", "bob", 0)
		// 未命中行不报错、0 行影响
		if err := q.Model(&fc3User{}).Where("id = ?", "zz").Update("name", "x"); err != nil {
			t.Fatalf("未命中 Update 不应报错: %v", err)
		}
		// 非目标行不被波及
		fc3Expect(t, drv, "c", "carol", 30)
	})

	t.Run("Updates_map全键写入含零值", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
		)
		q := drv.Query()

		if err := q.Model(&fc3User{}).Where("id = ?", "a").
			Updates(map[string]any{"name": "", "cnt": int64(0)}); err != nil {
			t.Fatalf("Updates(map 零值): %v", err)
		}
		// map 全键写入：name 空串与 cnt 0 均落库（与 struct 跳零值相反）
		fc3Expect(t, drv, "a", "", 0)
		fc3Expect(t, drv, "b", "bob", 20)

		if err := q.Model(&fc3User{}).Where("id = ?", "b").
			Updates(map[string]any{"name": "bob-v2", "cnt": int64(21)}); err != nil {
			t.Fatalf("Updates(map): %v", err)
		}
		fc3Expect(t, drv, "b", "bob-v2", 21)
	})

	t.Run("Updates_struct跳零值", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
		)
		q := drv.Query()

		// struct 传参跳零值：仅 cnt 非零 → 只写 cnt，name 保持不变
		if err := q.Model(&fc3User{}).Where("id = ?", "a").Updates(fc3User{Cnt: 99}); err != nil {
			t.Fatalf("Updates(struct 部分): %v", err)
		}
		fc3Expect(t, drv, "a", "alice", 99)
		// 仅 name 非零 → 只写 name，cnt 不被零值覆盖
		if err := q.Model(&fc3User{}).Where("id = ?", "a").Updates(&fc3User{Name: "alice-v3"}); err != nil {
			t.Fatalf("Updates(&struct): %v", err)
		}
		fc3Expect(t, drv, "a", "alice-v3", 99)
		// 指针与值两种传参形态等价；非目标行不被波及
		fc3Expect(t, drv, "b", "bob", 20)
	})

	t.Run("UpdateExpr_单列与混合map", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
			fc3User{ID: "c", Name: "carol", Cnt: 30},
		)
		q := drv.Query()

		// 单列表达式：SET cnt = cnt + ? 数据库端原子更新，只命中目标行
		if err := q.Model(&fc3User{}).Where("id = ?", "a").
			Update("cnt", contracts.Expr("cnt + ?", 5)); err != nil {
			t.Fatalf("Update 表达式: %v", err)
		}
		fc3Expect(t, drv, "a", "alice", 15)
		fc3Expect(t, drv, "b", "bob", 20)
		fc3Expect(t, drv, "c", "carol", 30)

		// 混合 map：表达式键 + 普通键同语句生效（X-08）
		if err := q.Model(&fc3User{}).Where("id = ?", "b").Updates(map[string]any{
			"cnt":  contracts.Expr("cnt * ?", 2),
			"name": "bob-x2",
		}); err != nil {
			t.Fatalf("Updates 混合表达式: %v", err)
		}
		fc3Expect(t, drv, "b", "bob-x2", 40)
		fc3Expect(t, drv, "c", "carol", 30)

		// 未命中行表达式 0 影响不报错
		if err := q.Model(&fc3User{}).Where("id = ?", "zz").
			Update("cnt", contracts.Expr("cnt + ?", 1)); err != nil {
			t.Fatalf("未命中表达式 Update 不应报错: %v", err)
		}
		fc3Expect(t, drv, "c", "carol", 30)
	})

	t.Run("Delete_bean主键与未命中", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
			fc3User{ID: "c", Name: "carol", Cnt: 30},
		)
		q := drv.Query()

		// bean 主键删除（xorm MergeConds：bean 非零字段并入 WHERE）
		if err := q.Delete(&fc3User{ID: "b"}); err != nil {
			t.Fatalf("Delete(bean): %v", err)
		}
		if n := fc3Count(t, drv); n != 2 {
			t.Fatalf("期望删除 1 行剩 2 行, 实际 %d", n)
		}
		fc3Expect(t, drv, "a", "alice", 10)
		fc3Expect(t, drv, "c", "carol", 30)

		// 未命中：0 行无错
		if err := q.Delete(&fc3User{ID: "nope"}); err != nil {
			t.Fatalf("未命中 Delete 不应报错: %v", err)
		}
		if n := fc3Count(t, drv); n != 2 {
			t.Errorf("未命中删除后应仍 2 行, 实际 %d", n)
		}
	})

	t.Run("Delete_bean加conds变参与无条件保护", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
			fc3User{ID: "c", Name: "carol", Cnt: 30},
		)
		q := drv.Query()

		// bean 主键与变参 conds 取 AND：id=a AND name=bob 无交集 → 0 行
		res := q.DeleteResult(&fc3User{ID: "a"}, "name = ?", "bob")
		if res.Error != nil {
			t.Fatalf("DeleteResult(交集为空): %v", res.Error)
		}
		if !res.IsZeroRow() {
			t.Errorf("交集为空期望 0 行, 实际 RowsAffected=%d", res.RowsAffected)
		}
		fc3Expect(t, drv, "a", "alice", 10)

		// id=b AND name=bob 交集命中 → 仅删该行
		if err := q.Delete(&fc3User{ID: "b"}, "name = ?", "bob"); err != nil {
			t.Fatalf("Delete(bean+conds): %v", err)
		}
		if n := fc3Count(t, drv); n != 2 {
			t.Fatalf("期望剩 2 行, 实际 %d", n)
		}

		// 无条件全零 bean：xorm 拒绝（防误全表删除），错误原样透出
		err := q.Delete(&fc3User{})
		if err == nil {
			t.Fatal("无任何条件的 Delete 应被拒绝")
		}
		if n := fc3Count(t, drv); n != 2 {
			t.Errorf("拒绝后行数不应变化, 实际 %d", n)
		}
	})

	t.Run("Result变体_RowsAffected与IsZeroRow", func(t *testing.T) {
		fc3Seed(t, drv,
			fc3User{ID: "a", Name: "alice", Cnt: 10},
			fc3User{ID: "b", Name: "bob", Cnt: 20},
		)
		q := drv.Query()

		// CreateResult：单条恒 1；切片为总行数
		res := q.CreateResult(&fc3User{ID: "r1", Name: "new", Cnt: 1})
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Errorf("CreateResult 单条期望 RowsAffected=1, 实际 %+v", res)
		}
		res = q.CreateResult([]*fc3User{
			{ID: "r2", Name: "n2", Cnt: 2},
			{ID: "r3", Name: "n3", Cnt: 3},
			{ID: "r4", Name: "n4", Cnt: 4},
		})
		if res.Error != nil || res.RowsAffected != 3 {
			t.Errorf("CreateResult 切片期望 RowsAffected=3, 实际 %+v", res)
		}
		if n := fc3Count(t, drv); n != 6 {
			t.Fatalf("CreateResult 后期望 6 行（2 种子+1 单条+3 切片）, 实际 %d", n)
		}

		// UpdateResult：命中 1 / 未命中 0 无错（IsZeroRow）
		res = q.Model(&fc3User{}).Where("id = ?", "a").UpdateResult("cnt", int64(11))
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Errorf("UpdateResult 命中期望 1 行, 实际 %+v", res)
		}
		fc3Expect(t, drv, "a", "alice", 11)
		res = q.Model(&fc3User{}).Where("id = ?", "zz").UpdateResult("name", "x")
		if res.Error != nil || !res.IsZeroRow() {
			t.Errorf("UpdateResult 未命中期望 IsZeroRow, 实际 %+v", res)
		}

		// UpdatesResult：命中 / 未命中
		res = q.Model(&fc3User{}).Where("id = ?", "b").
			UpdatesResult(map[string]any{"name": "bob-v2", "cnt": int64(21)})
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Errorf("UpdatesResult 命中期望 1 行, 实际 %+v", res)
		}
		fc3Expect(t, drv, "b", "bob-v2", 21)
		res = q.Model(&fc3User{}).Where("id = ?", "zz").UpdatesResult(map[string]any{"name": "x"})
		if res.Error != nil || !res.IsZeroRow() {
			t.Errorf("UpdatesResult 未命中期望 IsZeroRow, 实际 %+v", res)
		}

		// DeleteResult：命中 1 / 未命中 IsZeroRow
		res = q.DeleteResult(&fc3User{ID: "r2"})
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Errorf("DeleteResult 命中期望 1 行, 实际 %+v", res)
		}
		res = q.DeleteResult(&fc3User{ID: "r2"}) // 再删同主键 → 未命中
		if res.Error != nil || !res.IsZeroRow() {
			t.Errorf("DeleteResult 未命中期望 IsZeroRow, 实际 %+v", res)
		}
		if n := fc3Count(t, drv); n != 5 {
			t.Errorf("DeleteResult 后期望 5 行, 实际 %d", n)
		}
	})

	t.Run("SaveResult_插入更新upsert与显式条件零行", func(t *testing.T) {
		fc3Seed(t, drv, fc3User{ID: "s1", Name: "staged", Cnt: 1})
		q := drv.Query()

		// 更新路径命中（主键非零且行存在）：RowsAffected = 1
		res := q.SaveResult(&fc3User{ID: "s1", Name: "updated", Cnt: 2})
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Fatalf("SaveResult 更新命中期望 1 行, 实际 %+v", res)
		}
		fc3Expect(t, drv, "s1", "updated", 2)

		// 插入路径（主键全零）：RowsAffected = 1，空串主键行落库
		res = q.SaveResult(&fc3User{Name: "inserted", Cnt: 3})
		if res.Error != nil || res.RowsAffected != 1 || res.IsZeroRow() {
			t.Fatalf("SaveResult 插入期望 1 行, 实际 %+v", res)
		}
		fc3Expect(t, drv, "", "inserted", 3)

		// 0 行更新回落插入（upsert，X-04）：缺失主键行被重建
		res = q.SaveResult(&fc3User{ID: "ghost", Name: "reborn", Cnt: 4})
		if res.Error != nil || res.RowsAffected != 1 {
			t.Fatalf("SaveResult upsert 期望 1 行, 实际 %+v", res)
		}
		fc3Expect(t, drv, "ghost", "reborn", 4)

		// MySQL 方言 affected 计数差异：值无变化的已存在行 Save，UPDATE 返回
		// 0 affected（计数已修改行而非命中行）。驱动不得据此回落插入——否则
		// 触发主键重复错误（真实缺陷，见 query_write.go saveOne 行存在性探测）。
		cur := fc3Read(t, drv, "s1")
		res = q.SaveResult(&fc3User{ID: "s1", Name: cur.Name, Cnt: cur.Cnt})
		if res.Error != nil {
			t.Fatalf("值无变化 Save 不应报错（重复键回落回归）: %v", res.Error)
		}
		if dialect == "mysql" {
			if !res.IsZeroRow() {
				t.Errorf("MySQL 值无变化 Save 期望 0 affected 无错, 实际 RowsAffected=%d", res.RowsAffected)
			}
		} else if res.RowsAffected != 1 {
			t.Errorf("PG 值无变化 Save（命中行计数）期望 1 行, 实际 %d", res.RowsAffected)
		}
		fc3Expect(t, drv, "s1", cur.Name, cur.Cnt) // 行数据保持

		// 链上显式条件 + 行缺失：更新 0 行，回落插入被条件限定为 0 行，
		// 不报错且不落库（Result 零行语义；sqlite 同款锁定行为，见
		// query_test.go TestXormSaveResultWithExplicitCond）
		base := fc3Count(t, drv)
		res = q.Where("id = ?", "no-such").SaveResult(&fc3User{ID: "no-such", Name: "x", Cnt: 1})
		if res.Error != nil {
			t.Fatalf("显式条件未命中 SaveResult 不应报错: %v", res.Error)
		}
		if !res.IsZeroRow() {
			t.Errorf("显式条件未命中期望 IsZeroRow, 实际 RowsAffected=%d", res.RowsAffected)
		}
		if n := fc3Count(t, drv); n != base {
			t.Errorf("显式条件未命中不应新增行, 期望 %d 行, 实际 %d", base, n)
		}
	})

	t.Run("FirstOrCreate_未命中创建与命中返回", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		// 未命中：dest 原值插入（主键显式给定 → 落库即该主键行）
		dest := &fc3User{ID: "nc1", Name: "new-carol", Cnt: 5}
		if err := q.FirstOrCreate(dest, "id = ?", "nc1"); err != nil {
			t.Fatalf("FirstOrCreate(未命中): %v", err)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Fatalf("未命中应插入 1 行, 实际 %d", n)
		}
		if dest.ID != "nc1" || dest.Name != "new-carol" {
			t.Errorf("未命中后 dest 应保留待插入原值, 实际 %+v", dest)
		}
		fc3Expect(t, drv, "nc1", "new-carol", 5)

		// 命中：返回已有行（回填 dest），不新增
		dest2 := &fc3User{}
		if err := q.FirstOrCreate(dest2, "id = ?", "nc1"); err != nil {
			t.Fatalf("FirstOrCreate(命中): %v", err)
		}
		if dest2.ID != "nc1" || dest2.Name != "new-carol" || dest2.Cnt != 5 {
			t.Errorf("命中应回填库中行, 实际 %+v", dest2)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Errorf("命中后不应新增行, 期望 1 行, 实际 %d", n)
		}
	})

	t.Run("FirstOrInit_命中填充未命中不落库", func(t *testing.T) {
		fc3Seed(t, drv, fc3User{ID: "w1", Name: "alice", Cnt: 10})
		q := drv.Query()

		// 命中：查询结果回填 dest
		dest := &fc3User{}
		if err := q.FirstOrInit(dest, "id = ?", "w1"); err != nil {
			t.Fatalf("FirstOrInit(命中): %v", err)
		}
		if dest.ID != "w1" || dest.Name != "alice" || dest.Cnt != 10 {
			t.Errorf("命中应回填库中行, 实际 %+v", dest)
		}

		// 未命中：dest 保持调用方原值、不落库
		miss := &fc3User{ID: "no1", Name: "preset"}
		if err := q.FirstOrInit(miss, "id = ?", "no1"); err != nil {
			t.Fatalf("FirstOrInit(未命中): %v", err)
		}
		if miss.ID != "no1" || miss.Name != "preset" || miss.Cnt != 0 {
			t.Errorf("未命中 dest 应保持原值, 实际 %+v", miss)
		}
		if n := fc3Count(t, drv); n != 1 {
			t.Errorf("FirstOrInit 不应落库, 期望 1 行, 实际 %d", n)
		}
	})

	t.Run("FindInBatches_分批回调与尾批", func(t *testing.T) {
		rows := make([]fc3User, 0, 10)
		for i := 0; i < 10; i++ {
			rows = append(rows, fc3User{ID: fmt.Sprintf("f%02d", i), Name: fmt.Sprintf("fn%d", i), Cnt: int64(i)})
		}
		fc3Seed(t, drv, rows...)
		q := drv.Query()

		var users []fc3User
		var batches []int      // 回调序号
		var cum []int          // 回调时 dest 累计行数
		seen := map[string]bool{}
		err := q.Model(&fc3User{}).Order("id").FindInBatches(&users, 3, func(tx contracts.Query, batch int) error {
			batches = append(batches, batch)
			cum = append(cum, len(users))
			return nil
		})
		if err != nil {
			t.Fatalf("FindInBatches: %v", err)
		}
		// 10 行 / 批 3 → 4 批（3+3+3+1 尾批）
		if len(batches) != 4 {
			t.Fatalf("期望 4 次回调, 实际 %d", len(batches))
		}
		for i, b := range batches {
			if b != i+1 {
				t.Errorf("回调序号期望 %d, 实际 %d", i+1, b)
			}
		}
		wantCum := []int{3, 6, 9, 10}
		for i := range cum {
			if cum[i] != wantCum[i] {
				t.Errorf("第 %d 批后累计行数期望 %d, 实际 %d", i+1, wantCum[i], cum[i])
			}
		}
		if len(users) != 10 {
			t.Fatalf("dest 期望累积 10 行, 实际 %d", len(users))
		}
		for _, u := range users {
			seen[u.ID] = true
		}
		if len(seen) != 10 {
			t.Errorf("应无重复/遗漏批次行, 实际去重 %d", len(seen))
		}
	})

	t.Run("FindInBatches_空表无回调", func(t *testing.T) {
		fc3Reset(t, drv)
		q := drv.Query()

		var users []fc3User
		callbacks := 0
		err := q.Model(&fc3User{}).FindInBatches(&users, 2, func(tx contracts.Query, batch int) error {
			callbacks++
			return nil
		})
		if err != nil {
			t.Fatalf("空表 FindInBatches: %v", err)
		}
		if callbacks != 0 {
			t.Errorf("空表不应触发回调, 实际 %d 次", callbacks)
		}
		if len(users) != 0 {
			t.Errorf("空表 dest 应为空, 实际 %d 行", len(users))
		}
	})

	t.Run("FindInBatches_回调错误中断", func(t *testing.T) {
		rows := make([]fc3User, 0, 10)
		for i := 0; i < 10; i++ {
			rows = append(rows, fc3User{ID: fmt.Sprintf("e%02d", i), Name: fmt.Sprintf("en%d", i), Cnt: int64(i)})
		}
		fc3Seed(t, drv, rows...)
		q := drv.Query()

		interrupt := errors.New("fc3: 回调中断")
		var users []fc3User
		callbacks := 0
		err := q.Model(&fc3User{}).Order("id").FindInBatches(&users, 3, func(tx contracts.Query, batch int) error {
			callbacks++
			if batch == 2 {
				return interrupt
			}
			return nil
		})
		if err == nil || !errors.Is(err, interrupt) {
			t.Fatalf("回调错误应原样透出, 实际: %v", err)
		}
		if callbacks != 2 {
			t.Errorf("期望第 2 批后中断（共 2 次回调）, 实际 %d 次", callbacks)
		}
		if len(users) != 6 {
			t.Errorf("中断时 dest 应含前 2 批 6 行, 实际 %d", len(users))
		}
		// 库中数据不受影响（10 行全在）
		if n := fc3Count(t, drv); n != 10 {
			t.Errorf("库中行数不应受中断影响, 期望 10, 实际 %d", n)
		}
	})
}

// TestFullCovWrite_PG PG 入口。
func TestFullCovWrite_PG(t *testing.T) {
	runFullCovWrite(t, newPGXorm(t), "pg")
}

// TestFullCovWrite_MySQL MySQL 入口。
func TestFullCovWrite_MySQL(t *testing.T) {
	runFullCovWrite(t, newPlainMySQLXorm(t), "mysql")
}
