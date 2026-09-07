//go:build integration

package xormdriver

// fullcov_chain_integration_test.go —— contracts.Query 链式条件构建方法
// （分组 1：Table / Model / Select / Omit / Where / OrWhere / Not / Order /
// Limit / Offset / Group / Having / Distinct）的真实库全覆盖集成测试。
//
// 运行方式（docker-compose.yml 环境，表名前缀 fc1_ 避免与其他分组撞表）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovChain_PG' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovChain_MySQL' -v .
//
// 覆盖要点：
//   - 每种用法形态（含边界）都以真实数据回读断言，不做「不报错即过」；
//   - 非法条件/非法列参在链上报错（gorm AddError 语义），经终结方法透出
//     errors.Is(err, contracts.ErrUnsupported)；
//   - Where/OrWhere/Not 支持 string / builder.Cond / map[string]any 三种条件
//     类型 × AND/OR/NOT 组合语义；IN ?/IN (?)/空切片展开与普通占位符参数
//     索引对齐；链式方法不可变（原链复用互不影响）。

import (
	"errors"
	"sort"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/builder"
)

// ── 测试模型（表名固定，列名显式，全部默认 xorm tag identifier）────────

// fc1cUser 主表模型：id 主键 + 全列数据，供投影/条件/排序/分页断言。
type fc1cUser struct {
	ID    string `xorm:"pk varchar(32) 'id'"`
	Name  string `xorm:"varchar(64) 'name'"`
	Age   int    `xorm:"int 'age'"`
	City  string `xorm:"varchar(32) 'city'"`
	Email string `xorm:"varchar(64) 'email'"`
	Score int64  `xorm:"bigint 'score'"`
}

func (fc1cUser) TableName() string { return "fc1c_chain_users" }

// fc1cOrder 分组聚合表：category 分组、amount 聚合。
type fc1cOrder struct {
	ID       string `xorm:"pk varchar(32) 'id'"`
	Category string `xorm:"varchar(32) 'category'"`
	Amount   int64  `xorm:"bigint 'amount'"`
}

func (fc1cOrder) TableName() string { return "fc1c_chain_orders" }

// fc1cAgg Group 聚合的投影结构体：列名与 Select 别名一一对应。
type fc1cAgg struct {
	Category string `xorm:"'category'"`
	Cnt      int64  `xorm:"'cnt'"`
}

// ── 种子数据 ─────────────────────────────────────────────────────────
//
// 固定 5 行用户（年龄/城市构造区分 AND/OR/NOT 语义的命中集合）：
//
//	u1 alice 20 beijing    u2 alice 21 beijing
//	u3 bob   30 shanghai   u4 carol 25 beijing
//	u5 bob   30 guangzhou
//
// 固定 6 行订单（3 个分组）：
//	a: o1/o2/o3（amount 10/20/30，共 3 行）
//	b: o4/o5（amount 5/15，共 2 行）
//	c: o6（amount 100，共 1 行）

const (
	fc1cUsersTable  = "fc1c_chain_users"
	fc1cOrdersTable = "fc1c_chain_orders"
)

func fc1cSeedUsers(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{fc1cUsersTable}, &fc1cUser{})
	q := drv.Query()
	for _, u := range []fc1cUser{
		{ID: "u1", Name: "alice", Age: 20, City: "beijing", Email: "u1@x.com", Score: 100},
		{ID: "u2", Name: "alice", Age: 21, City: "beijing", Email: "u2@x.com", Score: 200},
		{ID: "u3", Name: "bob", Age: 30, City: "shanghai", Email: "u3@x.com", Score: 300},
		{ID: "u4", Name: "carol", Age: 25, City: "beijing", Email: "u4@x.com", Score: 400},
		{ID: "u5", Name: "bob", Age: 30, City: "guangzhou", Email: "u5@x.com", Score: 500},
	} {
		if err := q.Create(&u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}
}

func fc1cSeedOrders(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{fc1cOrdersTable}, &fc1cOrder{})
	q := drv.Query()
	for _, o := range []fc1cOrder{
		{ID: "o1", Category: "a", Amount: 10},
		{ID: "o2", Category: "a", Amount: 20},
		{ID: "o3", Category: "a", Amount: 30},
		{ID: "o4", Category: "b", Amount: 5},
		{ID: "o5", Category: "b", Amount: 15},
		{ID: "o6", Category: "c", Amount: 100},
	} {
		if err := q.Create(&o); err != nil {
			t.Fatalf("种子 %s: %v", o.ID, err)
		}
	}
}

// fc1cIDs 提取结果行 ID 列表（保留查询返回顺序，供排序断言逐位比对）。
func fc1cIDs(rows []fc1cUser) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// fc1cAssertOrdered 断言查询返回顺序与期望一致（用于显式 Order 链）。
func fc1cAssertOrdered(t *testing.T, got []fc1cUser, want ...string) {
	t.Helper()
	ids := fc1cIDs(got)
	if len(ids) != len(want) {
		t.Fatalf("行数期望 %d（%v）, 实际 %d（%v）", len(want), want, len(ids), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("第 %d 行期望 %s, 实际 %v（完整: %v）", i, want[i], ids, ids)
		}
	}
}

// fc1cAssertSet 断言查询返回的行 ID 集合（顺序无关，剔除无 ORDER BY 的行序差异）。
func fc1cAssertSet(t *testing.T, got []fc1cUser, want ...string) {
	t.Helper()
	ids := fc1cIDs(got)
	sort.Strings(ids)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if len(ids) != len(w) {
		t.Fatalf("ID 集合期望 %v, 实际 %v", w, ids)
	}
	for i := range w {
		if ids[i] != w[i] {
			t.Fatalf("ID 集合期望 %v, 实际 %v", w, ids)
		}
	}
}

// fc1cErrUnsupported 断言终结方法错误以 ErrUnsupported 透出（含链上 setErr 路径）。
func fc1cErrUnsupported(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("%s 期望 ErrUnsupported, 实际: %v", name, err)
	}
}

// ── 共享 runner（PG/MySQL 共用）───────────────────────────────────────

func runFullCovChain(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	q := drv.Query()

	t.Run("Table裸表名CRUD", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// Create：裸表名写入（不依赖模型 TableName）
		for _, id := range []string{"t1", "t2"} {
			u := fc1cUser{ID: id, Name: "raw-" + id, Age: 1, City: "tokyo", Email: id + "@raw.com", Score: 7}
			if err := q.Table(fc1cUsersTable).Create(&u); err != nil {
				t.Fatalf("Table Create %s: %v", id, err)
			}
		}
		var n int64
		if err := q.Table(fc1cUsersTable).Count(&n); err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 7 {
			t.Errorf("Table Create 后期望 7 行, 实际 %d", n)
		}

		// Find：裸表名 + Where + Order 读取
		var rows []fc1cUser
		if err := q.Table(fc1cUsersTable).Where("city = ?", "tokyo").Order("id").Find(&rows); err != nil {
			t.Fatalf("Table Find: %v", err)
		}
		fc1cAssertOrdered(t, rows, "t1", "t2")

		// Update：裸表名单列更新（链上显式 Where 限定范围）
		if err := q.Table(fc1cUsersTable).Where("id = ?", "t1").Update("score", 99); err != nil {
			t.Fatalf("Table Update: %v", err)
		}
		var one fc1cUser
		if err := q.Table(fc1cUsersTable).Where("id = ?", "t1").First(&one); err != nil {
			t.Fatalf("回读 t1: %v", err)
		}
		if one.Score != 99 || one.Name != "raw-t1" {
			t.Errorf("t1 期望 score=99/name=raw-t1, 实际 %+v", one)
		}

		// Delete：裸表名 + Where 删除单行，另一行不受影响
		if err := q.Table(fc1cUsersTable).Where("id = ?", "t2").Delete(&fc1cUser{}); err != nil {
			t.Fatalf("Table Delete: %v", err)
		}
		if err := q.Table(fc1cUsersTable).Count(&n); err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 6 {
			t.Errorf("Delete 后期望 6 行, 实际 %d", n)
		}
	})

	t.Run("Model定表与错误路径", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// Model 定表：普通读 + 写共用
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Where("city = ?", "beijing").Order("id").Find(&rows); err != nil {
			t.Fatalf("Model Find: %v", err)
		}
		fc1cAssertOrdered(t, rows, "u1", "u2", "u4")

		u := fc1cUser{ID: "m1", Name: "model", Age: 9, City: "osaka", Email: "m@x.com", Score: 1}
		if err := q.Model(&fc1cUser{}).Create(&u); err != nil {
			t.Fatalf("Model Create: %v", err)
		}
		var one fc1cUser
		if err := q.Model(&fc1cUser{}).Where("id = ?", "m1").First(&one); err != nil {
			t.Fatalf("Model 回读: %v", err)
		}
		if one.Name != "model" || one.City != "osaka" {
			t.Errorf("Model Create 回读不符: %+v", one)
		}

		// 无法解析出表名：非 struct 入参（string/int/切片/nil）链上报错
		for _, bad := range []any{"users", 42, []string{"a"}, nil} {
			var r []fc1cUser
			err := q.Model(bad).Find(&r)
			fc1cErrUnsupported(t, "Model("+fc1cTypeName(bad)+").Find", err)
		}
		// 匿名结构体解析不出表名（xorm 返回空表名），同样 ErrUnsupported
		var r2 []fc1cUser
		err := q.Model(struct{ X int }{}).Find(&r2)
		fc1cErrUnsupported(t, "Model(匿名struct).Find", err)

		// 链上错误保留：Model 报错后跟合法 Table() 不覆盖
		var r3 []fc1cUser
		err = q.Model(42).Table(fc1cUsersTable).Find(&r3)
		fc1cErrUnsupported(t, "Model(42).Table().Find 首个错误保留", err)
	})

	t.Run("Select投影", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// 单列：仅 name 被投影，其余字段零值
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Select("name").Find(&rows); err != nil {
			t.Fatalf("Select(name): %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("Select(name) 应 5 行, 实际 %d", len(rows))
		}
		for _, r := range rows {
			if r.Name == "" || r.ID != "" || r.City != "" || r.Email != "" || r.Score != 0 || r.Age != 0 {
				t.Errorf("Select(name) 投影不符（仅 name 应有值）: %+v", r)
			}
		}

		// 多列变参拼接：id+name 两列有值，其余零值
		rows = nil
		if err := q.Model(&fc1cUser{}).Select("id", "name").Find(&rows); err != nil {
			t.Fatalf("Select(id,name): %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("Select(id,name) 应 5 行, 实际 %d", len(rows))
		}
		for _, r := range rows {
			if r.ID == "" || r.Name == "" || r.Email != "" || r.Score != 0 {
				t.Errorf("Select(id,name) 投影不符: %+v", r)
			}
		}

		// "*" no-op：回归默认全列
		rows = nil
		if err := q.Model(&fc1cUser{}).Select("*").Where("id = ?", "u1").Find(&rows); err != nil {
			t.Fatalf("Select(*): %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("Select(*) 应 1 行, 实际 %d", len(rows))
		}
		for _, r := range rows {
			if r.ID != "u1" || r.Name == "" || r.Email == "" || r.Score == 0 || r.Age == 0 || r.City == "" {
				t.Errorf("Select(*) 应全列回填, 实际 %+v", r)
			}
		}

		// 非 string 主参 / 变参均在链上报错
		var r4 []fc1cUser
		err := q.Model(&fc1cUser{}).Select(123).Find(&r4)
		fc1cErrUnsupported(t, "Select(int).Find", err)
		err = q.Model(&fc1cUser{}).Select("id", 42).Find(&r4)
		fc1cErrUnsupported(t, "Select(string,int).Find", err)
	})

	t.Run("Omit投影排除", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// 单列排除：email 保持零值，其余列正常回填
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Omit("email").Where("id = ?", "u1").Find(&rows); err != nil {
			t.Fatalf("Omit(email): %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("Omit 应 1 行, 实际 %d", len(rows))
		}
		if rows[0].Email != "" {
			t.Errorf("Omit(email) 后 email 应为空, 实际 %q", rows[0].Email)
		}
		if rows[0].ID != "u1" || rows[0].Name == "" || rows[0].City == "" || rows[0].Score == 0 || rows[0].Age == 0 {
			t.Errorf("Omit(email) 后其余列应回填, 实际 %+v", rows[0])
		}

		// 多列排除 + 无 Where：全表仍 5 行，被排除列全部零值
		rows = nil
		if err := q.Model(&fc1cUser{}).Omit("name", "email").Find(&rows); err != nil {
			t.Fatalf("Omit(name,email): %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("Omit 多列应 5 行, 实际 %d", len(rows))
		}
		for _, r := range rows {
			if r.Name != "" || r.Email != "" {
				t.Errorf("Omit(name,email) 后两列应为空, 实际 %+v", r)
			}
			if r.ID == "" || r.City == "" || r.Score == 0 {
				t.Errorf("Omit 后其余列应有值, 实际 %+v", r)
			}
		}
	})

	t.Run("Where条件形态", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// string+args 单条件
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Where("city = ?", "beijing").Find(&rows); err != nil {
			t.Fatalf("Where string: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u4")

		// string+args 多条件链式：AND 语义
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("city = ?", "beijing").Where("age >= ?", 21).Find(&rows); err != nil {
			t.Fatalf("Where AND 链: %v", err)
		}
		fc1cAssertSet(t, rows, "u2", "u4")

		// builder.Cond：Eq / Expr（带参表达式）
		rows = nil
		if err := q.Model(&fc1cUser{}).Where(builder.Eq{"name": "bob"}).Find(&rows); err != nil {
			t.Fatalf("Where Eq: %v", err)
		}
		fc1cAssertSet(t, rows, "u3", "u5")
		rows = nil
		if err := q.Model(&fc1cUser{}).Where(builder.Expr("age >= ?", 30)).Find(&rows); err != nil {
			t.Fatalf("Where Expr: %v", err)
		}
		fc1cAssertSet(t, rows, "u3", "u5")

		// map[string]any：多键等值 AND；键值不匹配 0 行（证明 AND 而非 OR）
		rows = nil
		if err := q.Model(&fc1cUser{}).Where(map[string]any{"city": "beijing", "name": "alice"}).Find(&rows); err != nil {
			t.Fatalf("Where map: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2")
		rows = nil
		if err := q.Model(&fc1cUser{}).Where(map[string]any{"city": "beijing", "name": "bob"}).Find(&rows); err != nil {
			t.Fatalf("Where map 不匹配: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("map 多键不匹配应 0 行, 实际 %v", fc1cIDs(rows))
		}

		// IN ? 切片展开
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("city IN ?", []string{"beijing", "shanghai"}).Find(&rows); err != nil {
			t.Fatalf("Where city IN ?: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u3", "u4")

		// IN (?) 形态同样展开
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("city IN (?)", []string{"beijing"}).Find(&rows); err != nil {
			t.Fatalf("Where city IN (?): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u4")

		// 空切片 → IN (NULL) 恒假：0 行不报错（IN ? 与 IN (?) 两种形态）
		for _, cond := range []string{"city IN ?", "city IN (?)"} {
			rows = nil
			if err := q.Model(&fc1cUser{}).Where(cond, []string{}).Find(&rows); err != nil {
				t.Fatalf("空切片 %s: %v", cond, err)
			}
			if len(rows) != 0 {
				t.Errorf("空切片 %s 应 0 行, 实际 %d", cond, len(rows))
			}
		}

		// NOT IN ? 同样展开
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("id NOT IN ?", []string{"u1", "u5"}).Find(&rows); err != nil {
			t.Fatalf("Where NOT IN ?: %v", err)
		}
		fc1cAssertSet(t, rows, "u2", "u3", "u4")

		// 普通占位符与 IN 混合：参数索引不错位（v0.8.1 回归场景）
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("name = ? AND city IN ?", "alice", []string{"beijing", "shanghai"}).Find(&rows); err != nil {
			t.Fatalf("Where 混合占位符: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2")

		// 连续两个 IN 占位符：逐位消费参数
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("city IN ? AND name IN ?", []string{"beijing"}, []string{"alice", "carol"}).Find(&rows); err != nil {
			t.Fatalf("Where 双 IN: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u4")

		// 非法类型链上报错；首个错误不被后续合法条件覆盖
		var r []fc1cUser
		err := q.Model(&fc1cUser{}).Where(3.14).Find(&r)
		fc1cErrUnsupported(t, "Where(float).Find", err)
		var n int64
		if err := q.Model(&fc1cUser{}).Where(map[string]int{"city": 1}).Count(&n); !errors.Is(err, contracts.ErrUnsupported) {
			t.Errorf("Where(map[string]int).Count 应 ErrUnsupported, 实际 %v", err)
		}
		err = q.Model(&fc1cUser{}).Where(123).Where("city = ?", "beijing").Find(&r)
		fc1cErrUnsupported(t, "Where(非法).Where(合法) 首个错误保留", err)
	})

	t.Run("OrWhere与Not组合", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// string × string：Where(a).OrWhere(b) = a OR b
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Where("city = ?", "beijing").OrWhere("city = ?", "shanghai").Find(&rows); err != nil {
			t.Fatalf("OrWhere 并集: %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u3", "u4")

		// 无前置 Where 的单独 OrWhere：展平为普通条件
		rows = nil
		if err := q.Model(&fc1cUser{}).OrWhere("city = ?", "guangzhou").Find(&rows); err != nil {
			t.Fatalf("单独 OrWhere: %v", err)
		}
		fc1cAssertSet(t, rows, "u5")

		// 三条件混合：Where(a).OrWhere(b).Where(c) = (a OR b) AND c
		// （若错误拍平成 a OR (b AND c)，将多出 u1 一行，结果可区分）
		rows = nil
		if err := q.Model(&fc1cUser{}).
			Where("city = ?", "beijing").OrWhere("city = ?", "shanghai").Where("age >= ?", 21).
			Find(&rows); err != nil {
			t.Fatalf("三条件混合: %v", err)
		}
		fc1cAssertSet(t, rows, "u2", "u3", "u4")

		// map × OrWhere：Where(string).OrWhere(map) 并集
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("city = ?", "beijing").OrWhere(map[string]any{"name": "bob"}).Find(&rows); err != nil {
			t.Fatalf("OrWhere(map): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u3", "u4", "u5")

		// builder.Cond × OrWhere：Where(Expr).OrWhere(Eq) 并集
		rows = nil
		if err := q.Model(&fc1cUser{}).
			Where(builder.Expr("age >= ?", 25)).OrWhere(builder.Eq{"name": "alice"}).
			Find(&rows); err != nil {
			t.Fatalf("OrWhere(Cond): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u3", "u4", "u5")

		// OrWhere 与 IN 切片展开组合：展开先于 OR 落会话
		rows = nil
		if err := q.Model(&fc1cUser{}).
			Where("age < ?", 21).OrWhere("city IN ?", []string{"shanghai", "guangzhou"}).
			Find(&rows); err != nil {
			t.Fatalf("OrWhere(IN ?): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u3", "u5")

		// Not(string) 单独使用：NOT (name = alice) → 排除 alice 两行
		rows = nil
		if err := q.Model(&fc1cUser{}).Not("name = ?", "alice").Find(&rows); err != nil {
			t.Fatalf("Not(string): %v", err)
		}
		fc1cAssertSet(t, rows, "u3", "u4", "u5")

		// Where 后接 Not(string)：AND NOT
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("age >= ?", 25).Not("name = ?", "bob").Find(&rows); err != nil {
			t.Fatalf("Where+Not: %v", err)
		}
		fc1cAssertSet(t, rows, "u4")

		// Not(builder.Cond)：Eq 展开后取反
		rows = nil
		if err := q.Model(&fc1cUser{}).Not(builder.Eq{"name": "bob"}).Find(&rows); err != nil {
			t.Fatalf("Not(Eq): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u4")

		// Not(builder.Cond) 带参表达式：Expr 展开参数后取反
		rows = nil
		if err := q.Model(&fc1cUser{}).Not(builder.Expr("age < ?", 25)).Find(&rows); err != nil {
			t.Fatalf("Not(Expr): %v", err)
		}
		fc1cAssertSet(t, rows, "u3", "u4", "u5")

		// Where 后接 Not(map)：AND NOT 等值
		rows = nil
		if err := q.Model(&fc1cUser{}).Where("age >= ?", 25).Not(map[string]any{"city": "beijing"}).Find(&rows); err != nil {
			t.Fatalf("Where+Not(map): %v", err)
		}
		fc1cAssertSet(t, rows, "u3", "u5")

		// Not(map) 单独使用
		rows = nil
		if err := q.Model(&fc1cUser{}).Not(map[string]any{"city": "shanghai"}).Find(&rows); err != nil {
			t.Fatalf("Not(map): %v", err)
		}
		fc1cAssertSet(t, rows, "u1", "u2", "u4", "u5")

		// Not 与 IN 切片展开组合：先展开为 NOT (id IN (?,?)) 再取反
		rows = nil
		if err := q.Model(&fc1cUser{}).Not("id IN ?", []string{"u1", "u5"}).Find(&rows); err != nil {
			t.Fatalf("Not(IN ?): %v", err)
		}
		fc1cAssertSet(t, rows, "u2", "u3", "u4")

		// 非法类型：OrWhere/Not 链上报错
		var r []fc1cUser
		err := q.Model(&fc1cUser{}).OrWhere(1.5).Find(&r)
		fc1cErrUnsupported(t, "OrWhere(float).Find", err)
		err = q.Model(&fc1cUser{}).Not([]int{1}).Find(&r)
		fc1cErrUnsupported(t, "Not(slice).Find", err)
	})

	t.Run("Order排序", func(t *testing.T) {
		fc1cSeedUsers(t, drv)
		fc1cSeedOrders(t, drv)

		// 单字段升序
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Order("name").Find(&rows); err != nil {
			t.Fatalf("Order(name): %v", err)
		}
		fc1cAssertOrdered(t, rows, "u1", "u2", "u3", "u5", "u4")

		// 多字段（单次 Order 内逗号分隔）：age 降序 + id 升序决胜
		rows = nil
		if err := q.Model(&fc1cUser{}).Order("age DESC, id ASC").Find(&rows); err != nil {
			t.Fatalf("Order(多字段): %v", err)
		}
		fc1cAssertOrdered(t, rows, "u3", "u5", "u4", "u2", "u1")

		// 连续两次 Order：以最后一次为准（orderStr 覆盖）
		rows = nil
		if err := q.Model(&fc1cUser{}).Order("age").Order("id DESC").Find(&rows); err != nil {
			t.Fatalf("Order 覆盖: %v", err)
		}
		fc1cAssertOrdered(t, rows, "u5", "u4", "u3", "u2", "u1")

		// Count 剥离 ORDER BY：带排序链 Count 不报错（PG 42803 回归）且行数正确
		var n int64
		if err := q.Table(fc1cOrdersTable).Order("category DESC").Count(&n); err != nil {
			t.Fatalf("Order+Count: %v", err)
		}
		if n != 6 {
			t.Errorf("Order+Count 期望 6, 实际 %d", n)
		}

		// 非 string 排序值链上报错
		var r []fc1cUser
		err := q.Model(&fc1cUser{}).Order(123).Find(&r)
		fc1cErrUnsupported(t, "Order(int).Find", err)
	})

	t.Run("LimitOffset分页", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// 仅 Limit
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Order("id").Limit(2).Find(&rows); err != nil {
			t.Fatalf("仅 Limit: %v", err)
		}
		fc1cAssertOrdered(t, rows, "u1", "u2")

		// 仅 Offset：LIMIT 上限兜底
		rows = nil
		if err := q.Model(&fc1cUser{}).Order("id").Offset(2).Find(&rows); err != nil {
			t.Fatalf("仅 Offset: %v", err)
		}
		fc1cAssertOrdered(t, rows, "u3", "u4", "u5")

		// Limit + Offset
		rows = nil
		if err := q.Model(&fc1cUser{}).Order("id").Limit(2).Offset(1).Find(&rows); err != nil {
			t.Fatalf("Limit+Offset: %v", err)
		}
		fc1cAssertOrdered(t, rows, "u2", "u3")

		// Offset 越界 → 空结果
		rows = nil
		if err := q.Model(&fc1cUser{}).Order("id").Offset(7).Find(&rows); err != nil {
			t.Fatalf("Offset 越界: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("Offset(7) 应 0 行, 实际 %v", fc1cIDs(rows))
		}

		// Limit(0) 与 Limit(负数)：视为未设置，返回全量
		for _, lim := range []int{0, -1, -5} {
			rows = nil
			if err := q.Model(&fc1cUser{}).Order("id").Limit(lim).Find(&rows); err != nil {
				t.Fatalf("Limit(%d): %v", lim, err)
			}
			fc1cAssertOrdered(t, rows, "u1", "u2", "u3", "u4", "u5")
		}

		// Offset(0) 与 Offset(负数)：视为未设置
		for _, off := range []int{0, -1} {
			rows = nil
			if err := q.Model(&fc1cUser{}).Order("id").Offset(off).Find(&rows); err != nil {
				t.Fatalf("Offset(%d): %v", off, err)
			}
			fc1cAssertOrdered(t, rows, "u1", "u2", "u3", "u4", "u5")
		}
	})

	t.Run("GroupHaving聚合", func(t *testing.T) {
		fc1cSeedOrders(t, drv)

		// Group + Count：xorm 分组计数返回分组数（3 组）
		var n int64
		if err := q.Table(fc1cOrdersTable).Group("category").Count(&n); err != nil {
			t.Fatalf("Group Count: %v", err)
		}
		if n != 3 {
			t.Errorf("Group 后 Count 期望 3 组, 实际 %d", n)
		}

		// Group + Select 聚合：逐组行数回读（真实数据验证分组正确性）
		var aggs []fc1cAgg
		if err := q.Model(&fc1cOrder{}).
			Select("category, count(*) AS cnt").Group("category").Scan(&aggs); err != nil {
			t.Fatalf("Group Scan: %v", err)
		}
		if len(aggs) != 3 {
			t.Fatalf("Group Scan 期望 3 组, 实际 %+v", aggs)
		}
		got := map[string]int64{}
		for _, a := range aggs {
			got[a.Category] = a.Cnt
		}
		if got["a"] != 3 || got["b"] != 2 || got["c"] != 1 {
			t.Errorf("分组行数期望 {a:3 b:2 c:1}, 实际 %v", got)
		}

		// Group + Having：聚合后过滤（仅 count > 1 的组）
		aggs = nil
		if err := q.Model(&fc1cOrder{}).
			Select("category, count(*) AS cnt").Group("category").
			Having("count(*) > 1").Scan(&aggs); err != nil {
			t.Fatalf("Group+Having Scan: %v", err)
		}
		if len(aggs) != 2 {
			t.Fatalf("Having 过滤期望 2 组, 实际 %+v", aggs)
		}
		for _, a := range aggs {
			if a.Cnt <= 1 {
				t.Errorf("Having count(*) > 1 后仍出现 cnt=%d 的组 %s", a.Cnt, a.Category)
			}
		}

		// 无分组直接 Having 组合条件（内联常量）：语义仍是分组后过滤
		aggs = nil
		if err := q.Model(&fc1cOrder{}).
			Select("category, count(*) AS cnt").Group("category").
			Having("category <> 'c'").Scan(&aggs); err != nil {
			t.Fatalf("Group+Having 列条件: %v", err)
		}
		if len(aggs) != 2 {
			t.Fatalf("Having 列条件期望 2 组, 实际 %+v", aggs)
		}

		// Having 携带参数占位符：xorm Session.Having 仅收 string，链上报错
		var a2 []fc1cAgg
		err := q.Model(&fc1cOrder{}).Group("category").Having("count(*) > ?", 1).Scan(&a2)
		fc1cErrUnsupported(t, "Having(带参数).Scan", err)
		// Having 非 string：同样链上报错
		err = q.Model(&fc1cOrder{}).Group("category").Having(100).Scan(&a2)
		fc1cErrUnsupported(t, "Having(int).Scan", err)
	})

	t.Run("Distinct去重", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// 单列去重：3 个不同 city
		var rows []fc1cUser
		if err := q.Model(&fc1cUser{}).Distinct("city").Find(&rows); err != nil {
			t.Fatalf("Distinct(city): %v", err)
		}
		if len(rows) != 3 {
			t.Errorf("Distinct(city) 期望 3 行, 实际 %d: %v", len(rows), fc1cIDs(rows))
		}
		seen := map[string]bool{}
		for _, r := range rows {
			if r.City == "" {
				t.Errorf("Distinct 后 city 应有值: %+v", r)
			}
			if seen[r.City] {
				t.Errorf("Distinct(city) 出现重复 city %q", r.City)
			}
			seen[r.City] = true
		}

		// 多列去重：4 个不同 (city,name) 组合
		rows = nil
		if err := q.Model(&fc1cUser{}).Distinct("city", "name").Find(&rows); err != nil {
			t.Fatalf("Distinct(city,name): %v", err)
		}
		if len(rows) != 4 {
			t.Errorf("Distinct(city,name) 期望 4 行, 实际 %d", len(rows))
		}
		pairs := map[[2]string]bool{}
		for _, r := range rows {
			k := [2]string{r.City, r.Name}
			if pairs[k] {
				t.Errorf("Distinct(city,name) 出现重复组合 %v", k)
			}
			pairs[k] = true
		}
		// 去重结果与 Where 组合仍生效
		rows = nil
		if err := q.Model(&fc1cUser{}).Distinct("name").Where("age < ?", 25).Find(&rows); err != nil {
			t.Fatalf("Distinct+Where: %v", err)
		}
		if len(rows) != 1 || rows[0].Name != "alice" {
			t.Errorf("Distinct(name)+age<25 期望仅 alice, 实际 %+v", rows)
		}

		// 非 string 列名链上报错
		var r []fc1cUser
		err := q.Model(&fc1cUser{}).Distinct(42).Find(&r)
		fc1cErrUnsupported(t, "Distinct(int).Find", err)
	})

	t.Run("链式不可变性", func(t *testing.T) {
		fc1cSeedUsers(t, drv)

		// 从同一基础链派生 Where 过滤链与 Limit 分页链，互不影响
		base := q.Model(&fc1cUser{}).Order("id")
		filtered := base.Where("city = ?", "beijing")
		limited := base.Limit(2)

		var all, beijing, two []fc1cUser
		if err := base.Find(&all); err != nil {
			t.Fatalf("原链 Find: %v", err)
		}
		fc1cAssertOrdered(t, all, "u1", "u2", "u3", "u4", "u5")

		if err := filtered.Find(&beijing); err != nil {
			t.Fatalf("派生 Where 链 Find: %v", err)
		}
		fc1cAssertOrdered(t, beijing, "u1", "u2", "u4")

		if err := limited.Find(&two); err != nil {
			t.Fatalf("派生 Limit 链 Find: %v", err)
		}
		fc1cAssertOrdered(t, two, "u1", "u2")

		// 原链再次执行仍为全量（派生未污染原链 appliers）
		all = nil
		if err := base.Find(&all); err != nil {
			t.Fatalf("原链复用 Find: %v", err)
		}
		fc1cAssertOrdered(t, all, "u1", "u2", "u3", "u4", "u5")

		// 派生链继续派生（Where 上再叠 Limit）：三层互不影响
		filterLimited := filtered.Limit(1)
		var one []fc1cUser
		if err := filterLimited.Find(&one); err != nil {
			t.Fatalf("二级派生链 Find: %v", err)
		}
		fc1cAssertOrdered(t, one, "u1")
		beijing = nil
		if err := filtered.Find(&beijing); err != nil {
			t.Fatalf("二级派生后原 Where 链复用: %v", err)
		}
		fc1cAssertOrdered(t, beijing, "u1", "u2", "u4")
	})
}

// fc1cTypeName 打印非法 Model 入参的类型名（错误分支辅助）。
func fc1cTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "nil"
	case string:
		return "string"
	case int:
		return "int"
	case []string:
		return "[]string"
	default:
		return "?"
	}
}

// ── 入口（PG / MySQL 成对，共享 runner）───────────────────────────────

// TestFullCovChain_PG 分组 1 链式条件构建：PostgreSQL 真实库全覆盖。
func TestFullCovChain_PG(t *testing.T) {
	runFullCovChain(t, newPGXorm(t), "pg")
}

// TestFullCovChain_MySQL 分组 1 链式条件构建：MySQL 真实库全覆盖。
func TestFullCovChain_MySQL(t *testing.T) {
	runFullCovChain(t, newPlainMySQLXorm(t), "mysql")
}
