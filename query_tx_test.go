package xormdriver

import (
	"errors"
	"strconv"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 事务与 SavePoint ─────────────────────────────────────────────────
//
// 可见性断言的位置约束：内存 SQLite 的 ":memory:" 库是连接级私有的，事务
// 会话在 Commit/Rollback 前独占一条连接，期间外部查询会拿到新连接、看到一
// 个空库。因此外部计数一律放在事务终态之后（连接归还连接池供复用）；事务
// 内的可见性验证只能经同一事务会话（tx 复用）进行。

// countTxTable 各表行数断言辅助：经驱动 Raw+ScanMap 链路取计数。不用 testutil
// 的 countRaw——glebarez/go-sqlite 对 count(*) 返回文本，其 int64 标量 dest 会
// 触发 Scan 标量类型不匹配（基建缺陷，已在 query_schema_test.go 记录同一变通）。
func countTxTable(t *testing.T, drv *XormDriver, table string) int64 {
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
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			t.Fatalf("count %s 计数 %q 解析失败: %v", table, n, err)
		}
		return v
	default:
		t.Fatalf("count %s 计数类型不支持: %T", table, rows[0]["n"])
		return 0
	}
}

// TestXormTxTransactionCommit 事务内 Create 成功后提交，外部可统计到该行。
func TestXormTxTransactionCommit(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	err := q.Transaction(func(tx contracts.Query) error {
		return tx.Create(&XormTestModel{ID: "tx1", Name: "in_tx"})
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if n := countTxTable(t, drv, "xorm_test_model"); n != 1 {
		t.Errorf("事务提交后应 1 条, 实际 %d", n)
	}
}

// TestXormTxTransactionRollback 事务内 Create 后返回注入错误触发回滚：
// 外部计数为 0；wrapError 对不匹配 Sentinel 的错误原样透传，
// errors.Is 应能匹配到闭包注入的错误本身。
func TestXormTxTransactionRollback(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	errInjected := errors.New("injected rollback")
	err := q.Transaction(func(tx contracts.Query) error {
		if err := tx.Create(&XormTestModel{ID: "tx2", Name: "rollback"}); err != nil {
			return err
		}
		return errInjected
	})
	if err == nil {
		t.Fatal("事务应返回错误")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("应匹配注入错误, 实际 %v", err)
	}
	if n := countTxTable(t, drv, "xorm_test_model"); n != 0 {
		t.Errorf("回滚后应 0 条, 实际 %d", n)
	}
}

// TestXormTxSessionReuse 事务内多终结操作复用同一 session：Create 之后的
// Count/Find 经 build() 复用 tx 会话，xorm session 语句自动重置保证 Insert
// 语句不残留；能读到未提交数据同时证明两次操作确在同一事务连接上执行
// （若误开新会话，:memory: 连接级私有库会直接报表不存在）。
func TestXormTxSessionReuse(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	err := q.Transaction(func(tx contracts.Query) error {
		if err := tx.Create(&XormTestModel{ID: "sr1", Name: "uncommitted"}); err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&XormTestModel{}).Count(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("事务内 Count 应看到未提交的 1 条, 实际 %d", n)
		}
		var rows []XormTestModel
		if err := tx.Model(&XormTestModel{}).Find(&rows); err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != "sr1" {
			t.Errorf("事务内 Find 应看到未提交数据, 实际 %+v", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务内复用 session: %v", err)
	}
}

// TestXormTxBeginCommitRollback 手动事务：Begin/Rollback 的写入回滚后外部
// 不可见，Begin/Commit 的写入提交后外部可见。
func TestXormTxBeginCommitRollback(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	tx := q.Begin()
	if err := tx.Create(&XormTestModel{ID: "bc1", Name: "rollback"}); err != nil {
		t.Fatalf("Begin/Create: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n := countTxTable(t, drv, "xorm_test_model"); n != 0 {
		t.Errorf("回滚后应 0 条, 实际 %d", n)
	}

	tx2 := q.Begin()
	if err := tx2.Create(&XormTestModel{ID: "bc2", Name: "commit"}); err != nil {
		t.Fatalf("第二次 Begin/Create: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := countTxTable(t, drv, "xorm_test_model"); n != 1 {
		t.Errorf("提交后应仅 commit 的 1 条, 实际 %d", n)
	}
}

// TestXormTxCommitWithoutBegin 无活动事务时 Commit/Rollback 统一返回
// contracts.ErrInvalidTransaction，而非驱动底层错误。
func TestXormTxCommitWithoutBegin(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	if err := q.Commit(); !errors.Is(err, contracts.ErrInvalidTransaction) {
		t.Errorf("无事务 Commit 应 ErrInvalidTransaction, 实际 %v", err)
	}
	if err := q.Rollback(); !errors.Is(err, contracts.ErrInvalidTransaction) {
		t.Errorf("无事务 Rollback 应 ErrInvalidTransaction, 实际 %v", err)
	}
}

// TestXormTxSavePoint SavePoint/RollbackTo（SQLite 原生支持 SAVEPOINT）：
// 建点后创建的第二条记录随 RollbackTo 消失，第一条与外层事务不受影响。
func TestXormTxSavePoint(t *testing.T) {
	drv := newXormTestDriverWithModel(t)
	q := drv.Query()

	tx := q.Begin()
	if err := tx.Create(&XormTestModel{ID: "sp-a", Name: "keep"}); err != nil {
		t.Fatalf("Create 第一条: %v", err)
	}
	if err := tx.SavePoint("sp1"); err != nil {
		t.Fatalf("SavePoint: %v", err)
	}
	if err := tx.Create(&XormTestModel{ID: "sp-b", Name: "drop"}); err != nil {
		t.Fatalf("Create 第二条: %v", err)
	}
	if err := tx.RollbackTo("sp1"); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := countTxTable(t, drv, "xorm_test_model"); n != 1 {
		t.Errorf("回滚到保存点后应仅剩 1 条, 实际 %d", n)
	}
	var rows []XormTestModel
	if err := drv.Query().Model(&XormTestModel{}).Find(&rows); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "sp-a" {
		t.Errorf("存活的应是保存点前创建的 sp-a, 实际 %+v", rows)
	}
}
