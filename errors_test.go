package xormdriver

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

func TestWrapError_Nil(t *testing.T) {
	if err := wrapError(nil); err != nil {
		t.Errorf("wrapError(nil) = %v, 期望 nil", err)
	}
}

func TestWrapError_RecordNotFound(t *testing.T) {
	// database/sql 标准无行错误（经 %w 包装，模拟真实 Scan 链路）
	err := wrapError(fmt.Errorf("scan failed: %w", sql.ErrNoRows))
	if !errors.Is(err, contracts.ErrRecordNotFound) {
		t.Errorf("sql.ErrNoRows 应映射为 ErrRecordNotFound, 实际: %v", err)
	}

	// xorm Get/First 无记录时返回的 ErrNotExist，语义应归一为同一 Sentinel
	err = wrapError(fmt.Errorf("get failed: %w", xorm.ErrNotExist))
	if !errors.Is(err, contracts.ErrRecordNotFound) {
		t.Errorf("xorm.ErrNotExist 应映射为 ErrRecordNotFound, 实际: %v", err)
	}
}

func TestWrapError_InvalidTransaction(t *testing.T) {
	// 事务已提交/回滚后继续操作（sql.ErrTxDone 经 %w 包装透出）
	err := wrapError(fmt.Errorf("commit failed: %w", sql.ErrTxDone))
	if !errors.Is(err, contracts.ErrInvalidTransaction) {
		t.Errorf("sql.ErrTxDone 应映射为 ErrInvalidTransaction, 实际: %v", err)
	}
}

func TestWrapError_DuplicatedKey_StringMatch(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		// MySQL 重复键（错误码 1062 的两种文案形态）
		{"MySQL Error 1062", "Error 1062: Duplicate entry 'foo' for key 'users.email'"},
		{"MySQL Duplicate entry", "Duplicate entry 'foo' for key 'idx_name'"},
		// PostgreSQL 重复键（duplicate key 文案与 SQLSTATE 23505）
		{"PostgreSQL duplicate key", `duplicate key value violates unique constraint "users_email_key"`},
		{"PostgreSQL SQLSTATE 23505", `ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)`},
		// SQLite 唯一约束
		{"SQLite UNIQUE", "UNIQUE constraint failed: users.email"},
	}
	for _, c := range cases {
		err := wrapError(fmt.Errorf("insert failed: %w", errors.New(c.msg)))
		if !errors.Is(err, contracts.ErrDuplicatedKey) {
			t.Errorf("%s 应映射为 ErrDuplicatedKey, 实际: %v", c.name, err)
		}
	}
}

func TestWrapError_Deadlock(t *testing.T) {
	// MySQL 死锁（错误码 1213）
	err := wrapError(fmt.Errorf("exec failed: %w",
		errors.New("Error 1213: Deadlock found when trying to get lock; try restarting transaction")))
	if !errors.Is(err, contracts.ErrDeadlock) {
		t.Errorf("MySQL Error 1213 应映射为 ErrDeadlock, 实际: %v", err)
	}

	// PostgreSQL 死锁
	err = wrapError(errors.New("deadlock detected"))
	if !errors.Is(err, contracts.ErrDeadlock) {
		t.Errorf("PostgreSQL deadlock 应映射为 ErrDeadlock, 实际: %v", err)
	}
}

func TestWrapError_Unknown(t *testing.T) {
	original := errors.New("some random database error")
	err := wrapError(original)
	if err == nil {
		t.Fatal("非哨兵错误不应返回 nil")
	}
	// 未知错误应原样返回（同值），不做任何包装
	if err != original {
		t.Errorf("未知错误应原样返回, 实际: %v", err)
	}

	// %w 包装链中的未知错误同样原样透传（驱动不得吞掉链式上下文）
	wrapped := fmt.Errorf("ctx: %w", original)
	if err = wrapError(wrapped); err != wrapped {
		t.Errorf("包装的未知错误应原样返回, 实际: %v", err)
	}
}
