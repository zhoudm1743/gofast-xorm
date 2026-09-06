package xormdriver

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// wrapError 将 xorm 链路上的错误归一为框架级 Sentinel Error。
// xorm 不做错误翻译，底层错误以 database/sql 哨兵值与各数据库方言原文透出，
// 因此先做 errors.Is 结构化匹配，再按错误文本匹配常见方言（与 gormdriver 一致）。
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	// xorm Get/First 无记录返回 ErrNotExist，database/sql Scan 无行返回 ErrNoRows，
	// 两者对框架而言语义相同：查询无结果
	case errors.Is(err, sql.ErrNoRows) || errors.Is(err, xorm.ErrNotExist):
		return fmt.Errorf("%w: %v", contracts.ErrRecordNotFound, err)
	// 事务已提交/回滚后继续在其上操作（Commit/Rollback/SavePoint 复用场景高发）
	case errors.Is(err, sql.ErrTxDone):
		return fmt.Errorf("%w: %v", contracts.ErrInvalidTransaction, err)
	}
	msg := err.Error()
	// MySQL: "Error 1213: Deadlock found when trying to get lock"
	// PostgreSQL: "deadlock detected"
	if strings.Contains(msg, "Deadlock") || strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "Error 1213") {
		return fmt.Errorf("%w: %v", contracts.ErrDeadlock, err)
	}
	// MySQL: "Error 1062: Duplicate entry 'a' for key 'u.email'"
	// PostgreSQL: SQLSTATE 23505 "duplicate key value violates unique constraint"
	// SQLite: "UNIQUE constraint failed: users.email"
	if strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "23505") || strings.Contains(msg, "Error 1062") {
		return fmt.Errorf("%w: %v", contracts.ErrDuplicatedKey, err)
	}
	// 其余错误不属于框架语义范畴，原样透传以保留底层原始上下文
	return err
}
