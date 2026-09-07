//go:build integration

package xormdriver

// fullcov_rawtx_integration_test.go —— contracts.Query 真实库全覆盖（分组 4）：
// 原生 SQL + 事务 + 杂项。PG/MySQL 双真库跑同一共享 runner（runFullCovRawTx），
// 覆盖方法：Raw / Exec / ExecResult / Transaction / Begin / Commit / Rollback /
// SavePoint / RollbackTo / WithContext / Debug / Scopes / Paginate。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovRawTx' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovRawTx' -v .
//
// 覆盖形态：
//   - Raw：+Find 进模型切片、+Scan 标量（count/单列/未命中保持零值）、'?' 参数绑定
//   - Exec：原生 DDL（建表）与带参 DML（INSERT/UPDATE）落库生效
//   - ExecResult：INSERT/UPDATE/DELETE RowsAffected、未命中 0 行 IsZeroRow、
//     重复键错误映射 contracts.ErrDuplicatedKey
//   - Transaction：正常提交外部可见（含会话复用的事务内 Count）、fc 业务错误整体
//     回滚且原样透传（err == errBiz）、ORM 错误（重复键）回滚、TxOption 变体降级忽略
//   - Begin/Commit、Begin/Rollback 可见性；重复 Commit / Commit 后 Rollback /
//     Rollback 后 Commit → contracts.ErrInvalidTransaction；Begin(TxOption) 变体
//   - SavePoint/RollbackTo：保存点后插入消失、保存点前保留、回滚后事务可继续写
//   - WithContext：已取消 ctx 读/写均报错且不落库；有效 ctx 正常执行
//   - Debug：文档化 no-op，链可继续正常执行
//   - Scopes：多个作用域依次应用（AND + Order）、nil 跳过、片段复用、原链不被污染
//   - Paginate：非法 page/size 归一 1/20（先读 utils.PageUtil.Normalize 确认）、
//     中间页/尾页不足一页/越界空结果、Count+分页两阶段
//   - PG 专属：事务内 DDL 可回滚（MySQL 的 DDL 隐式提交，分支跳过）

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型 ─────────────────────────────────────────────────────────
//
// 默认 xorm tag identifier；TableName() 固定表名 fc4_users（分组 4 前缀）。
// 数值列全部用 int64（xorm 推导 BIGINT），避免跨方言 int 扫描/绑定差异。

type fc4User struct {
	ID     string `xorm:"pk varchar(16) 'id'"`
	Name   string `xorm:"varchar(64) 'name'"`
	Status int64  `xorm:"'status'"`
	Cnt    int64  `xorm:"'cnt'"`
}

func (fc4User) TableName() string { return "fc4_users" }

// ── 共用种子 / 断言辅助 ───────────────────────────────────────────────

// fc4SeedBasic 重建 fc4_users 并灌入三行基线（a/b status=1；c status=2）。
func fc4SeedBasic(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
	q := drv.Query()
	for _, u := range []*fc4User{
		{ID: "a", Name: "alice", Status: 1, Cnt: 10},
		{ID: "b", Name: "bob", Status: 1, Cnt: 20},
		{ID: "c", Name: "carol", Status: 2, Cnt: 30},
	} {
		if err := q.Create(u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}
}

// fc4SeedUsers 重建 fc4_users 并灌入 n 行（u01..u%02d，status 全 1，cnt=i*10）。
func fc4SeedUsers(t *testing.T, drv *XormDriver, n int) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
	q := drv.Query()
	for i := 1; i <= n; i++ {
		u := &fc4User{ID: fmt.Sprintf("u%02d", i), Name: fmt.Sprintf("name-%02d", i), Status: 1, Cnt: int64(i * 10)}
		if err := q.Create(u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}
}

// fc4SeedPaged 重建 fc4_users 并灌入 23 行：u01..u13 status=1，u14..u23 status=2。
// 供 Paginate 分页矩阵（筛选 status=1 共 13 行）与归一化（23 行全量）使用。
func fc4SeedPaged(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
	q := drv.Query()
	for i := 1; i <= 23; i++ {
		st := int64(2)
		if i <= 13 {
			st = 1
		}
		u := &fc4User{ID: fmt.Sprintf("u%02d", i), Name: fmt.Sprintf("name-%02d", i), Status: st, Cnt: int64(i * 10)}
		if err := q.Create(u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}
}

// fc4CountUsers 整表行数（Raw+Scan 标量链路，顺带覆盖 count 扫描）。
func fc4CountUsers(t *testing.T, drv *XormDriver) int64 {
	t.Helper()
	return countRaw(t, drv, "SELECT count(*) FROM fc4_users")
}

// fc4FindAll 全表查询并返回按 id 升序的行。
func fc4FindAll(t *testing.T, q contracts.Query) []fc4User {
	t.Helper()
	var rows []fc4User
	if err := q.Model(&fc4User{}).Order("id").Find(&rows); err != nil {
		t.Fatalf("Find 全表: %v", err)
	}
	return rows
}

// fc4FindByID 按主键回读单行（显式 Where）。
func fc4FindByID(t *testing.T, q contracts.Query, id string) fc4User {
	t.Helper()
	var row fc4User
	if err := q.Model(&fc4User{}).Where("id = ?", id).First(&row); err != nil {
		t.Fatalf("回读 %q: %v", id, err)
	}
	return row
}

// fc4AssertIDs 断言 rows 的主键序列与 want 完全一致（按序）。
func fc4AssertIDs(t *testing.T, stage string, rows []fc4User, want string) {
	t.Helper()
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != want {
		t.Errorf("%s: 主键序列应为 %q, 实际 %q", stage, want, got)
	}
}

// ── 共享 runner（PG/MySQL 双入口调用；engine 供方言差异分支）──────────

func runFullCovRawTx(t *testing.T, drv *XormDriver, engine string) {
	t.Helper()
	q := drv.Query()

	t.Run("Raw_Find模型切片_参数绑定", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		// 单参绑定 + AND 组合过滤 + 排序
		var rows []fc4User
		if err := q.Raw("SELECT id, name, status, cnt FROM fc4_users WHERE status = ? ORDER BY id", int64(1)).
			Find(&rows); err != nil {
			t.Fatalf("Raw+Find 单参: %v", err)
		}
		if len(rows) != 2 || rows[0].ID != "a" || rows[1].ID != "b" {
			t.Fatalf("Raw+Find 应命中 a,b 两行, 实际 %+v", rows)
		}
		if rows[0].Name != "alice" || rows[0].Cnt != 10 {
			t.Errorf("a 行回读值错误: %+v", rows[0])
		}

		// 多参绑定：cnt >= ? 且 status <> ?
		var rows2 []fc4User
		if err := q.Raw("SELECT id, name, status, cnt FROM fc4_users WHERE cnt >= ? AND status <> ? ORDER BY id DESC",
			int64(20), int64(2)).Find(&rows2); err != nil {
			t.Fatalf("Raw+Find 多参: %v", err)
		}
		if len(rows2) != 1 || rows2[0].ID != "b" || rows2[0].Name != "bob" {
			t.Errorf("多参 Raw 应命中 b, 实际 %+v", rows2)
		}

		// 同一 raw 链可重复终结执行（不可变语义，两次独立会话）
		var again []fc4User
		if err := q.Raw("SELECT id FROM fc4_users WHERE status = ? ORDER BY id", int64(2)).Find(&again); err != nil {
			t.Fatalf("Raw 二次执行: %v", err)
		}
		if len(again) != 1 || again[0].ID != "c" {
			t.Errorf("Raw 二次执行应命中 c, 实际 %+v", again)
		}
	})

	t.Run("Raw_Scan标量_单列与未命中", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		// count(*) → int64
		if n := fc4CountUsers(t, drv); n != 3 {
			t.Fatalf("count 应 3, 实际 %d", n)
		}
		// 单列字符串 + 参数绑定
		var name string
		if err := q.Raw("SELECT name FROM fc4_users WHERE id = ?", "a").Scan(&name); err != nil {
			t.Fatalf("Scan 单列: %v", err)
		}
		if name != "alice" {
			t.Errorf("Scan 单列应 alice, 实际 %q", name)
		}
		// 单列数值
		var cnt int64
		if err := q.Raw("SELECT cnt FROM fc4_users WHERE id = ?", "b").Scan(&cnt); err != nil {
			t.Fatalf("Scan 数值列: %v", err)
		}
		if cnt != 20 {
			t.Errorf("Scan 数值列应 20, 实际 %d", cnt)
		}
		// 未命中：无行保持零值不报错（gorm Scan 语义）
		var miss string
		if err := q.Raw("SELECT name FROM fc4_users WHERE id = ?", "no-such").Scan(&miss); err != nil {
			t.Fatalf("Scan 未命中应无错: %v", err)
		}
		if miss != "" {
			t.Errorf("Scan 未命中应保持零值, 实际 %q", miss)
		}
		// Raw+Scan 单 struct（Get 语义）
		var one fc4User
		if err := q.Raw("SELECT id, name, status, cnt FROM fc4_users WHERE id = ?", "c").Scan(&one); err != nil {
			t.Fatalf("Raw+Scan struct: %v", err)
		}
		if one.ID != "c" || one.Name != "carol" || one.Cnt != 30 {
			t.Errorf("Raw+Scan struct 值错误: %+v", one)
		}
	})

	t.Run("Exec_DDL建表_带参DML生效", func(t *testing.T) {
		intgXDropTables(t, drv, "fc4_exec_tbl")
		t.Cleanup(func() { intgXDropTables(t, drv, "fc4_exec_tbl") })
		// DDL：Exec 建表（两方言通用类型）
		if err := q.Exec(`CREATE TABLE fc4_exec_tbl (id varchar(16) PRIMARY KEY, note varchar(64), n bigint)`); err != nil {
			t.Fatalf("Exec 建表: %v", err)
		}
		// 带参 DML：INSERT
		if err := q.Exec("INSERT INTO fc4_exec_tbl (id, note, n) VALUES (?, ?, ?)", "e1", "first", int64(1)); err != nil {
			t.Fatalf("Exec 插入: %v", err)
		}
		if err := q.Exec("INSERT INTO fc4_exec_tbl (id, note, n) VALUES (?, ?, ?)", "e2", "second", int64(2)); err != nil {
			t.Fatalf("Exec 插入 2: %v", err)
		}
		if n := countRaw(t, drv, "SELECT count(*) FROM fc4_exec_tbl"); n != 2 {
			t.Fatalf("Exec 插入后应 2 行, 实际 %d", n)
		}
		// 带参 DML：UPDATE 与 DELETE
		if err := q.Exec("UPDATE fc4_exec_tbl SET note = ? WHERE id = ?", "updated", "e1"); err != nil {
			t.Fatalf("Exec 更新: %v", err)
		}
		var note string
		if err := q.Raw("SELECT note FROM fc4_exec_tbl WHERE id = ?", "e1").Scan(&note); err != nil {
			t.Fatalf("回读 note: %v", err)
		}
		if note != "updated" {
			t.Errorf("Exec UPDATE 应生效, 实际 %q", note)
		}
		if err := q.Exec("DELETE FROM fc4_exec_tbl WHERE id = ?", "e2"); err != nil {
			t.Fatalf("Exec 删除: %v", err)
		}
		if n := countRaw(t, drv, "SELECT count(*) FROM fc4_exec_tbl"); n != 1 {
			t.Errorf("Exec DELETE 后应 1 行, 实际 %d", n)
		}
	})

	t.Run("ExecResult_行数与IsZeroRow", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		// INSERT → 1 行
		r := q.ExecResult("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "d", "dora", int64(2), int64(40))
		if r.Error != nil || r.RowsAffected != 1 {
			t.Fatalf("INSERT RowsAffected 应 1, 实际 %d Error=%v", r.RowsAffected, r.Error)
		}
		// UPDATE 命中改值 → 1 行（MySQL 未改值会报 0，测试恒改新值）
		r = q.ExecResult("UPDATE fc4_users SET name = ? WHERE id = ?", "bob2", "b")
		if r.Error != nil || r.RowsAffected != 1 {
			t.Fatalf("UPDATE RowsAffected 应 1, 实际 %d Error=%v", r.RowsAffected, r.Error)
		}
		if got := fc4FindByID(t, q, "b"); got.Name != "bob2" {
			t.Errorf("UPDATE 应落库 bob2, 实际 %q", got.Name)
		}
		// UPDATE 未命中 → 0 行无错，IsZeroRow 可判定
		r = q.ExecResult("UPDATE fc4_users SET name = ? WHERE id = ?", "ghost", "zz")
		if r.Error != nil || !r.IsZeroRow() {
			t.Fatalf("未命中 UPDATE 期望 0 行无错, 实际 RowsAffected=%d Error=%v", r.RowsAffected, r.Error)
		}
		// DELETE 命中 → 1 行
		r = q.ExecResult("DELETE FROM fc4_users WHERE id = ?", "a")
		if r.Error != nil || r.RowsAffected != 1 {
			t.Fatalf("DELETE RowsAffected 应 1, 实际 %d Error=%v", r.RowsAffected, r.Error)
		}
		if n := fc4CountUsers(t, drv); n != 3 {
			t.Errorf("删除 a 后应剩 3 行(b/c/d), 实际 %d", n)
		}
	})

	t.Run("ExecResult_重复键错误映射", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		r := q.ExecResult("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "a", "dup", int64(1), int64(1))
		if r.Error == nil || !errors.Is(r.Error, contracts.ErrDuplicatedKey) {
			t.Errorf("重复键应映射 ErrDuplicatedKey, 实际 Error=%v", r.Error)
		}
		if r.RowsAffected != 0 {
			t.Errorf("错误路径 RowsAffected 应保持 0, 实际 %d", r.RowsAffected)
		}
	})

	t.Run("Transaction_提交可见_事务内Count会话复用", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		var inside int64
		err := q.Transaction(func(tx contracts.Query) error {
			if err := tx.Create(&fc4User{ID: "tx1", Name: "in-tx-1", Status: 1, Cnt: 10}); err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "tx2", "in-tx-2", int64(1), int64(20)); err != nil {
				return err
			}
			// 同一事务会话内应看到未提交的两行（session 复用）
			return tx.Model(&fc4User{}).Count(&inside)
		})
		if err != nil {
			t.Fatalf("Transaction: %v", err)
		}
		if inside != 2 {
			t.Errorf("事务内 Count 应 2, 实际 %d", inside)
		}
		if n := fc4CountUsers(t, drv); n != 2 {
			t.Fatalf("提交后应 2 行, 实际 %d", n)
		}
		rows := fc4FindAll(t, drv.Query())
		fc4AssertIDs(t, "提交后可见", rows, "tx1,tx2")
		if rows[0].Name != "in-tx-1" || rows[1].Name != "in-tx-2" {
			t.Errorf("提交后回读值错误: %+v", rows)
		}
	})

	t.Run("Transaction_业务错误回滚_原样透传", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		errBiz := errors.New("biz-fail-sentinel")
		err := q.Transaction(func(tx contracts.Query) error {
			if err := tx.Create(&fc4User{ID: "rb1", Name: "rollback-me", Status: 1, Cnt: 1}); err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "rb2", "rollback-me2", int64(1), int64(2)); err != nil {
				return err
			}
			return errBiz
		})
		if err == nil || !errors.Is(err, errBiz) {
			t.Fatalf("应返回注入的业务错误, 实际 %v", err)
		}
		if err != errBiz {
			t.Errorf("业务错误应原样透传不被包装, 实际 %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 0 {
			t.Errorf("整体回滚后应 0 行, 实际 %d", n)
		}
	})

	t.Run("Transaction_驱动错误同样回滚", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		err := q.Transaction(func(tx contracts.Query) error {
			if err := tx.Create(&fc4User{ID: "dup", Name: "first", Status: 1, Cnt: 1}); err != nil {
				return err
			}
			// 同事务内二次插入同主键：ORM 错误沿 fc 返回 → 整体回滚
			return tx.Create(&fc4User{ID: "dup", Name: "second", Status: 1, Cnt: 2})
		})
		if err == nil || !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Fatalf("应返回 ErrDuplicatedKey, 实际 %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 0 {
			t.Errorf("错误路径回滚后应 0 行, 实际 %d", n)
		}
	})

	t.Run("Transaction_TxOption变体降级忽略", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		// 隔离级别 / 只读选项：xorm session.Begin 无选项参数，降级忽略——
		// 断言不报错且数据正常提交（若被当真执行，只读事务会拒绝 INSERT）。
		err := q.Transaction(func(tx contracts.Query) error {
			return tx.Create(&fc4User{ID: "opt1", Name: "read-committed", Status: 1, Cnt: 1})
		}, contracts.TxReadCommitted)
		if err != nil {
			t.Fatalf("TxReadCommitted 变体: %v", err)
		}
		err = q.Transaction(func(tx contracts.Query) error {
			return tx.Create(&fc4User{ID: "opt2", Name: "read-only", Status: 1, Cnt: 2})
		}, contracts.TxReadOnly)
		if err != nil {
			t.Fatalf("TxReadOnly 变体（应降级忽略而非报错）: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 2 {
			t.Errorf("两笔事务提交后应 2 行, 实际 %d", n)
		}
	})

	t.Run("Begin_提交与回滚可见性", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		// Begin + Create + Commit：提交后外部可见
		tx := q.Begin()
		t.Cleanup(func() { _ = tx.Rollback() }) // 失败兜底：未终态的事务由 Cleanup 回收
		if err := tx.Create(&fc4User{ID: "bc1", Name: "commit-me", Status: 1, Cnt: 1}); err != nil {
			t.Fatalf("Begin/Create: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 1 {
			t.Errorf("提交后应 1 行, 实际 %d", n)
		}
		// Begin + Exec + Rollback：回滚后外部不可见
		tx2 := q.Begin()
		t.Cleanup(func() { _ = tx2.Rollback() })
		if err := tx2.Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "br1", "rollback-me", int64(1), int64(9)); err != nil {
			t.Fatalf("Begin/Exec: %v", err)
		}
		if err := tx2.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 1 {
			t.Errorf("回滚后应仍 1 行, 实际 %d", n)
		}
	})

	t.Run("Begin_重复Commit与终态后误用", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		// 重复 Commit → ErrInvalidTransaction
		tx := q.Begin()
		t.Cleanup(func() { _ = tx.Rollback() })
		if err := tx.Create(&fc4User{ID: "dc1", Name: "double-commit", Status: 1, Cnt: 1}); err != nil {
			t.Fatalf("Begin/Create: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("首次 Commit: %v", err)
		}
		if err := tx.Commit(); !errors.Is(err, contracts.ErrInvalidTransaction) {
			t.Errorf("重复 Commit 应 ErrInvalidTransaction, 实际 %v", err)
		}
		// Commit 后 Rollback → ErrInvalidTransaction
		if err := tx.Rollback(); !errors.Is(err, contracts.ErrInvalidTransaction) {
			t.Errorf("Commit 后 Rollback 应 ErrInvalidTransaction, 实际 %v", err)
		}
		// Rollback 后 Commit / 重复 Rollback → ErrInvalidTransaction
		tx2 := q.Begin()
		t.Cleanup(func() { _ = tx2.Rollback() })
		if err := tx2.Create(&fc4User{ID: "dc2", Name: "double-rollback", Status: 1, Cnt: 2}); err != nil {
			t.Fatalf("第二次 Begin/Create: %v", err)
		}
		if err := tx2.Rollback(); err != nil {
			t.Fatalf("首次 Rollback: %v", err)
		}
		if err := tx2.Commit(); !errors.Is(err, contracts.ErrInvalidTransaction) {
			t.Errorf("Rollback 后 Commit 应 ErrInvalidTransaction, 实际 %v", err)
		}
		if err := tx2.Rollback(); !errors.Is(err, contracts.ErrInvalidTransaction) {
			t.Errorf("重复 Rollback 应 ErrInvalidTransaction, 实际 %v", err)
		}
		// 终态链上的插入不应落库（Commit 已清空事务会话）
		if n := fc4CountUsers(t, drv); n != 1 {
			t.Errorf("仅首次事务的提交应可见, 实际 %d 行", n)
		}
	})

	t.Run("Begin_TxOption变体", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		// xorm 对 TxOption 降级忽略：Begin(变体) 不应报错、事务正常终结
		tx := q.Begin(contracts.TxRepeatableRead)
		t.Cleanup(func() { _ = tx.Rollback() })
		if err := tx.Create(&fc4User{ID: "bt1", Name: "repeatable-read", Status: 1, Cnt: 1}); err != nil {
			t.Fatalf("Begin(TxRepeatableRead)/Create: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Begin(TxRepeatableRead)/Commit: %v", err)
		}
		tx2 := q.Begin(contracts.TxSerializable)
		t.Cleanup(func() { _ = tx2.Rollback() })
		if err := tx2.Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "bt2", "serializable", int64(1), int64(2)); err != nil {
			t.Fatalf("Begin(TxSerializable)/Exec: %v", err)
		}
		if err := tx2.Rollback(); err != nil {
			t.Fatalf("Begin(TxSerializable)/Rollback: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 1 {
			t.Errorf("仅提交的 bt1 应可见, 实际 %d 行", n)
		}
	})

	t.Run("SavePoint_部分回滚", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		tx := q.Begin()
		t.Cleanup(func() { _ = tx.Rollback() })
		if err := tx.Create(&fc4User{ID: "sp-a", Name: "keep", Status: 1, Cnt: 1}); err != nil {
			t.Fatalf("保存点前插入: %v", err)
		}
		if err := tx.SavePoint("fc4_sp_1"); err != nil {
			t.Fatalf("SavePoint: %v", err)
		}
		if err := tx.Create(&fc4User{ID: "sp-b", Name: "drop", Status: 1, Cnt: 2}); err != nil {
			t.Fatalf("保存点后插入: %v", err)
		}
		if err := tx.RollbackTo("fc4_sp_1"); err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}
		// 回滚到保存点后事务仍可用：继续插入并提交
		if err := tx.Create(&fc4User{ID: "sp-c", Name: "after-rollback", Status: 1, Cnt: 3}); err != nil {
			t.Fatalf("回滚后继续插入: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 2 {
			t.Fatalf("保存点回滚后应仅 2 行(sp-a/sp-c), 实际 %d", n)
		}
		rows := fc4FindAll(t, drv.Query())
		fc4AssertIDs(t, "保存点存活行", rows, "sp-a,sp-c")
		// 保存点前的行内容未被波及
		if rows[0].Name != "keep" {
			t.Errorf("保存点前行应保持原值, 实际 %+v", rows[0])
		}
	})

	t.Run("WithContext_已取消ctx读写拒绝", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// 已取消 ctx 的查询应报错（database/sql 在取连接前短路 ctx.Err）
		var rows []fc4User
		err := q.WithContext(ctx).Model(&fc4User{}).Find(&rows)
		if err == nil {
			t.Error("已取消 ctx 的 Find 应报错")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("已取消 ctx 的 Find 错误应匹配 context.Canceled, 实际 %v", err)
		}
		// 已取消 ctx 的写入应报错且不落库
		err = q.WithContext(ctx).Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "cx1", "no-write", int64(1), int64(1))
		if err == nil {
			t.Error("已取消 ctx 的 Exec 应报错")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("已取消 ctx 的 Exec 错误应匹配 context.Canceled, 实际 %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 3 {
			t.Errorf("已取消 ctx 的写入不应落库, 实际 %d 行", n)
		}
	})

	t.Run("WithContext_有效ctx正常执行", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		ctx := context.Background()
		var rows []fc4User
		if err := q.WithContext(ctx).Model(&fc4User{}).Order("id").Find(&rows); err != nil {
			t.Fatalf("有效 ctx Find: %v", err)
		}
		fc4AssertIDs(t, "有效 ctx 查询", rows, "a,b,c")
		if err := q.WithContext(ctx).Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "ctx1", "with-ctx", int64(2), int64(50)); err != nil {
			t.Fatalf("有效 ctx Exec: %v", err)
		}
		if got := fc4FindByID(t, q, "ctx1"); got.Name != "with-ctx" {
			t.Errorf("有效 ctx 写入应落库, 实际 %q", got.Name)
		}
	})

	t.Run("Debug_NoOp链可正常执行", func(t *testing.T) {
		fc4SeedBasic(t, drv)
		// Debug 文档化 no-op：置于链头与链中均不影响执行。
		// 注意：xorm Find 向已含数据的切片追加（驱动 FindInBatches 同源语义），
		// 故每次终结使用全新 dest。
		var rows []fc4User
		if err := q.Debug().Model(&fc4User{}).Order("id").Find(&rows); err != nil {
			t.Fatalf("Debug 链 Find: %v", err)
		}
		fc4AssertIDs(t, "Debug 链头", rows, "a,b,c")
		var rows2 []fc4User
		if err := q.Model(&fc4User{}).Debug().Where("status = ?", int64(1)).Order("id").Find(&rows2); err != nil {
			t.Fatalf("Debug 链中 Find: %v", err)
		}
		fc4AssertIDs(t, "Debug 链中", rows2, "a,b")
		// Debug 后写终结同样可用
		if err := q.Debug().Exec("INSERT INTO fc4_users (id, name, status, cnt) VALUES (?, ?, ?, ?)", "dbg1", "debug-write", int64(2), int64(1)); err != nil {
			t.Fatalf("Debug 链 Exec: %v", err)
		}
		if n := fc4CountUsers(t, drv); n != 4 {
			t.Errorf("Debug 写后应 4 行, 实际 %d", n)
		}
	})

	t.Run("Scopes_多作用域依次应用_nil跳过", func(t *testing.T) {
		intgXMigrate(t, drv, []string{"fc4_users"}, &fc4User{})
		q := drv.Query()
		for _, u := range []*fc4User{
			{ID: "a", Name: "alpha", Status: 1, Cnt: 10},
			{ID: "b", Name: "bravo", Status: 1, Cnt: 20},
			{ID: "c", Name: "charlie", Status: 2, Cnt: 30},
			{ID: "d", Name: "delta", Status: 1, Cnt: 40},
			{ID: "e", Name: "echo", Status: 2, Cnt: 50},
			{ID: "f", Name: "foxtrot", Status: 3, Cnt: 60},
		} {
			if err := q.Create(u); err != nil {
				t.Fatalf("种子 %s: %v", u.ID, err)
			}
		}
		// 三个作用域 + nil：status=1 AND cnt>=20 并按 id 排序 → b,d
		rows := fc4FindAll(t, q.Scopes(fc4ScopeStatus(1), nil, fc4ScopeCntGE(20), fc4ScopeOrderID()))
		fc4AssertIDs(t, "作用域组合", rows, "b,d")
		// 原链不被污染：不带作用域的全表仍 6 行
		all := fc4FindAll(t, drv.Query())
		fc4AssertIDs(t, "原链未被污染", all, "a,b,c,d,e,f")
		// 作用域片段可复用：同一 builder 应用到全新查询
		rows2 := fc4FindAll(t, q.Scopes(fc4ScopeStatus(2), fc4ScopeOrderID()))
		fc4AssertIDs(t, "复用 status=2", rows2, "c,e")
		// 作用域链上仍可继续追加 Where（AND 组合）
		rows3 := fc4FindAll(t, q.Scopes(fc4ScopeStatus(1)).Where("cnt >= ?", int64(40)))
		fc4AssertIDs(t, "作用域后追加 Where", rows3, "d")
	})

	t.Run("Paginate_非法页码尺寸归一", func(t *testing.T) {
		fc4SeedPaged(t, drv)
		// page<=0 → 1、size<=0 → 20（utils.PageUtil.Normalize，先读实现确认）
		// 若未归一，size=0 会退化为无 LIMIT 返回全部 23 行，可分辨。
		var rows []fc4User
		if err := q.Model(&fc4User{}).Paginate(0, 0).Order("id").Find(&rows); err != nil {
			t.Fatalf("Paginate(0,0): %v", err)
		}
		if len(rows) != 20 {
			t.Fatalf("Paginate(0,0) 归一 1/20 应返回 20 行, 实际 %d", len(rows))
		}
		if rows[0].ID != "u01" {
			t.Errorf("归一首页应从 u01 开始, 实际 %q", rows[0].ID)
		}
		// 负 page/size 同样归一 1/20
		var rows2 []fc4User
		if err := q.Model(&fc4User{}).Paginate(-2, -7).Order("id").Find(&rows2); err != nil {
			t.Fatalf("Paginate(-2,-7): %v", err)
		}
		if len(rows2) != 20 || rows2[0].ID != "u01" {
			t.Errorf("Paginate(-2,-7) 归一 1/20 应返回 u01 起始的 20 行, 实际 %d 行首 %q",
				len(rows2), func() string {
					if len(rows2) > 0 {
						return rows2[0].ID
					}
					return ""
				}())
		}
	})

	t.Run("Paginate_中间页_尾页不足_越界空结果", func(t *testing.T) {
		fc4SeedPaged(t, drv)
		// 首页 1..10
		var p1 []fc4User
		if err := q.Model(&fc4User{}).Paginate(1, 10).Order("id").Find(&p1); err != nil {
			t.Fatalf("第 1 页: %v", err)
		}
		fc4AssertIDs(t, "第 1 页", p1, "u01,u02,u03,u04,u05,u06,u07,u08,u09,u10")
		// 中间页 11..20
		var p2 []fc4User
		if err := q.Model(&fc4User{}).Paginate(2, 10).Order("id").Find(&p2); err != nil {
			t.Fatalf("第 2 页: %v", err)
		}
		fc4AssertIDs(t, "第 2 页", p2, "u11,u12,u13,u14,u15,u16,u17,u18,u19,u20")
		// 尾页不足一页：3 行
		var p3 []fc4User
		if err := q.Model(&fc4User{}).Paginate(3, 10).Order("id").Find(&p3); err != nil {
			t.Fatalf("第 3 页: %v", err)
		}
		fc4AssertIDs(t, "第 3 页尾页", p3, "u21,u22,u23")
		// 越界页：空结果无错
		var p4 []fc4User
		if err := q.Model(&fc4User{}).Paginate(99, 10).Order("id").Find(&p4); err != nil {
			t.Fatalf("越界页: %v", err)
		}
		if len(p4) != 0 {
			t.Errorf("越界页应空结果, 实际 %d 行", len(p4))
		}
	})

	t.Run("Paginate_Count两阶段总数与分页", func(t *testing.T) {
		fc4SeedPaged(t, drv)
		// 两阶段用法：先 Count 总数（独立链），再同构链分页取数
		base := q.Model(&fc4User{}).Where("status = ?", int64(1)).Order("id")
		var total int64
		if err := base.Count(&total); err != nil {
			t.Fatalf("Count: %v", err)
		}
		if total != 13 {
			t.Fatalf("status=1 总数应 13, 实际 %d", total)
		}
		var pages [][]fc4User
		for page := 1; page <= 4; page++ {
			var rows []fc4User
			if err := base.Paginate(page, 5).Find(&rows); err != nil {
				t.Fatalf("第 %d 页: %v", page, err)
			}
			pages = append(pages, rows)
			for _, r := range rows {
				if r.Status != 1 {
					t.Errorf("分页结果混入 status=%d 行 %s", r.Status, r.ID)
				}
			}
		}
		fc4AssertIDs(t, "两阶段第 1 页", pages[0], "u01,u02,u03,u04,u05")
		fc4AssertIDs(t, "两阶段第 2 页", pages[1], "u06,u07,u08,u09,u10")
		fc4AssertIDs(t, "两阶段第 3 页(尾页)", pages[2], "u11,u12,u13")
		if len(pages[3]) != 0 {
			t.Errorf("两阶段第 4 页应空, 实际 %d 行", len(pages[3]))
		}
		sum := len(pages[0]) + len(pages[1]) + len(pages[2]) + len(pages[3])
		if int64(sum) != total {
			t.Errorf("分页行数总和 %d 应等于 Count 总数 %d", sum, total)
		}
	})

	t.Run("PG专属_事务内DDL可回滚", func(t *testing.T) {
		if engine != "postgres" {
			t.Skip("PG 专属：MySQL 的 DDL 隐式提交语义不同，分支跳过")
		}
		intgXDropTables(t, drv, "fc4_tx_ddl")
		t.Cleanup(func() { intgXDropTables(t, drv, "fc4_tx_ddl") })
		// 未提交的 DDL 对其他连接不可见（MVCC），可见性断言必须在同一事务
		// 会话内进行（tx 链复用会话），终态存在性用外部连接检查。
		inTxExists := func(tx contracts.Query) int64 {
			var n int64
			if err := tx.Raw(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'fc4_tx_ddl'`).Scan(&n); err != nil {
				t.Fatalf("事务内查 pg_tables: %v", err)
			}
			return n
		}
		exists := func() int64 {
			return countRaw(t, drv, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'fc4_tx_ddl'`)
		}
		// 事务内建表后回滚：事务会话内可见，提交前回滚则外部表不存在（PG DDL 事务性）
		tx := q.Begin()
		t.Cleanup(func() { _ = tx.Rollback() })
		if err := tx.Exec(`CREATE TABLE fc4_tx_ddl (id varchar(16) PRIMARY KEY)`); err != nil {
			t.Fatalf("事务内建表: %v", err)
		}
		if n := inTxExists(tx); n != 1 {
			t.Fatalf("事务内应看到未提交的建表, 实际 %d", n)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if exists() != 0 {
			t.Errorf("回滚后表应不存在, 实际 %d", exists())
		}
		// 事务内建表后提交：外部可见表保留
		tx2 := q.Begin()
		t.Cleanup(func() { _ = tx2.Rollback() })
		if err := tx2.Exec(`CREATE TABLE fc4_tx_ddl (id varchar(16) PRIMARY KEY)`); err != nil {
			t.Fatalf("第二次事务内建表: %v", err)
		}
		if err := tx2.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if exists() != 1 {
			t.Errorf("提交后表应保留, 实际 %d", exists())
		}
	})
}

// fc4ScopeStatus 构造 status 等值作用域（闭包复用查询片段）。
func fc4ScopeStatus(status int64) func(contracts.Query) contracts.Query {
	return func(qq contracts.Query) contracts.Query {
		return qq.Where("status = ?", status)
	}
}

// fc4ScopeCntGE 构造 cnt 下界作用域。
func fc4ScopeCntGE(min int64) func(contracts.Query) contracts.Query {
	return func(qq contracts.Query) contracts.Query {
		return qq.Where("cnt >= ?", min)
	}
}

// fc4ScopeOrderID 构造主键升序作用域。
func fc4ScopeOrderID() func(contracts.Query) contracts.Query {
	return func(qq contracts.Query) contracts.Query {
		return qq.Order("id")
	}
}

// ── 入口（成对，各自调共享 runner；PG/MySQL 用真实 DSN）──────────────

func TestFullCovRawTx_PG(t *testing.T) {
	runFullCovRawTx(t, newPGXorm(t), "postgres")
}

func TestFullCovRawTx_MySQL(t *testing.T) {
	runFullCovRawTx(t, newPlainMySQLXorm(t), "mysql")
}
