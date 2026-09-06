package xormdriver

import (
	"fmt"
	"strconv"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 事务 ─────────────────────────────────────────────────────────────

// Transaction 在独立事务会话中执行 fc：成功提交，fc 返回错误则回滚。
// txQ 复制当前查询并把事务会话注入 tx 字段，闭包内所有终结操作经 build()
// 复用该会话（xorm session 语句自动重置，可跨终结操作复用）；defer Close()
// 归还连接——若闭包 panic 既未提交也未回滚，xorm 的 Close 会兜底回滚事务。
// 注意：fc 返回错误路径显式 Rollback；panic 路径依赖 Close 兜底而非主动回滚，
// 与 gorm 驱动的错误才回滚语义保持一致。
//
// opts（隔离级别/只读）：xorm 的 session.Begin() 无 *sql.TxOptions 参数，
// 当前降级为忽略（与 gormdriver 透传隔离级别的行为差异已在包注释声明）。
func (q *XormQuery) Transaction(fc func(tx contracts.Query) error, opts ...contracts.TxOption) error {
	tx := q.engine.NewSession()
	defer tx.Close() //nolint:errcheck // Close 仅释放连接与兜底回滚，失败无意义
	if err := tx.Begin(); err != nil {
		return q.done(err)
	}
	txQ := q.clone()
	txQ.tx = tx
	if err := fc(txQ); err != nil {
		_ = tx.Rollback()
		return q.done(err)
	}
	return q.done(tx.Commit())
}

// Begin 手动开启事务，返回携带事务会话的新查询（链式方法不修改自身）。
// 开启失败时经 setErr 把包装后的 ErrInvalidTransaction 记入链上首个错误，
// 后续终结操作由 build()/done() 拦截返回该错误。
//
// opts 同 Transaction：xorm session.Begin() 无选项参数，降级忽略。
func (q *XormQuery) Begin(opts ...contracts.TxOption) contracts.Query {
	s := q.engine.NewSession()
	if err := s.Begin(); err != nil {
		return q.setErr(fmt.Errorf("%w: %v", contracts.ErrInvalidTransaction, err))
	}
	return q.wrap(func(c *XormQuery) {
		c.tx = s
	})
}

// Commit 提交事务并释放会话；无活动事务时返回 ErrInvalidTransaction
// （sentinel 直传即可，wrapError 结构化不匹配时原样透传）。
// 事务终态一次性，Commit 后直接清空 tx 字段，保证重复 Commit/Rollback
// 都走"无活动事务"分支而非复用已提交的会话。
func (q *XormQuery) Commit() error {
	if q.tx == nil {
		return q.done(contracts.ErrInvalidTransaction)
	}
	err := q.tx.Commit()
	_ = q.tx.Close()
	q.tx = nil
	return q.done(err)
}

// Rollback 回滚事务并释放会话，错误与终态语义同 Commit。
func (q *XormQuery) Rollback() error {
	if q.tx == nil {
		return q.done(contracts.ErrInvalidTransaction)
	}
	err := q.tx.Rollback()
	_ = q.tx.Close()
	q.tx = nil
	return q.done(err)
}

// SavePoint 在当前事务内创建保存点。名字用双引号引用：SQLite/PostgreSQL
// 对双引号标识符合法；MySQL 默认视双引号为字符串字面量，需开启 ANSI_QUOTES
// （或传入纯标识符）。与 gormdriver 交由 gorm 方言生成 SAVEPOINT 语句的行为
// 存在差异，此处为显式拼接。
func (q *XormQuery) SavePoint(name string) error {
	s, err := q.build(nil)
	if err != nil {
		return q.done(err)
	}
	if _, err := s.Exec("SAVEPOINT " + strconv.Quote(name)); err != nil {
		return q.done(err)
	}
	return nil
}

// RollbackTo 回滚到指定保存点，SQL 拼接与方言注意事项同 SavePoint。
func (q *XormQuery) RollbackTo(name string) error {
	s, err := q.build(nil)
	if err != nil {
		return q.done(err)
	}
	if _, err := s.Exec("ROLLBACK TO " + strconv.Quote(name)); err != nil {
		return q.done(err)
	}
	return nil
}
