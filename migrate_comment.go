package xormdriver

// migrate_comment.go：Sync2 列注释假阳性 ALTER 的根治（stitch-mes 实测反馈 BUG-3）。
//
// 背景（真实库实测，mes schema 700 条列注释）：DB 列注释由 gorm/手写 SQL 建成，
// 模型 orm tag 大多未声明 comment。xorm v1.4.1 sync.go 的列循环在
// col.Comment != oriCol.Comment 时无条件 ModifyColumnSQL，其 PG 产物固定为
// ALTER TABLE ... ALTER COLUMN x TYPE <相同类型>; COMMENT ON COLUMN x IS ''
// ——对几乎每列产生假阳性 ALTER（449 条实测），且把已有非空注释刷成空。
//
// 根治：Sync2 之前把引擎缓存 TableInfo 中【已存在表】的列注释改写为库内实际
// 值（neutralizeComments），Sync2 注释分支恒不触发；Sync2 后恢复模型原注释，
// 由收敛 Pass 按需生成"仅新增/更新、绝不清空"的 COMMENT 语句（pg）。
// mysql 的 Sync2 注释分支同样存在（ModifyColumnSQL 整列重写），一并中性化；
// 纯注释差异在 mysql 不收敛（见 buildConvergeStmts）。mssql 无 Sync2 注释分支，跳过。

import (
	"context"
	"fmt"
	"strings"

	"xorm.io/xorm"
	"xorm.io/xorm/schemas"
)

// neutralizeComments 把引擎缓存 TableInfo 中已存在表的列注释改写为库内实际注释，
// 返回恢复函数（调用后模型原注释复原，供收敛 Pass 计算真实注释差异）。
// 只对 pgx / mysql 生效（其余引擎 Sync2 无注释分支，空操作）。
// 注意：改写的是引擎级 TableInfo 缓存（TableInfo(bean) 返回值与 Sync2 共享），
// 必须在 Sync2 之前调用；新表（库内不存在）不改写，保留模型注释供建表。
func neutralizeComments(e *xorm.Engine, engineName, schema string, models []any) (func(), error) {
	noop := func() {}
	if engineName != "pgx" && engineName != "mysql" {
		return noop, nil
	}
	type savedTable struct {
		tbl  *schemas.Table
		orig map[string]string // 列名（原大小写）→ 模型原注释
	}
	var saves []savedTable
	for _, m := range models {
		bean := tableBeanOf(m)
		if bean == nil {
			continue
		}
		t, err := e.TableInfo(bean)
		if err != nil {
			return noop, fmt.Errorf("xormdriver: 注释中性化解析模型 %T 失败: %w", m, err)
		}
		tableName := t.Name
		if idx := strings.LastIndex(tableName, "."); idx >= 0 {
			tableName = tableName[idx+1:]
		}
		exists, err := engineTableExists(e, tableName)
		if err != nil {
			return noop, err
		}
		if !exists {
			continue // 新表：保留模型注释，Sync2 建表时落注释
		}
		comments, err := loadActualComments(e, engineName, schema, tableName)
		if err != nil {
			return noop, err
		}
		orig := make(map[string]string, len(t.Columns()))
		for _, col := range t.Columns() {
			orig[col.Name] = col.Comment
			col.Comment = comments[strings.ToLower(col.Name)] // 库内无注释则为 ""
		}
		saves = append(saves, savedTable{tbl: t, orig: orig})
	}
	return func() {
		for _, s := range saves {
			for _, col := range s.tbl.Columns() {
				if v, ok := s.orig[col.Name]; ok {
					col.Comment = v
				}
			}
		}
	}, nil
}

// engineTableExists 判断表是否已存在（经方言 IsTableExist，schema 语义随引擎配置）。
func engineTableExists(e *xorm.Engine, table string) (bool, error) {
	exists, err := e.Dialect().IsTableExist(e.DB(), context.Background(), table)
	if err != nil {
		return false, fmt.Errorf("xormdriver: 判断表 %s 存在性失败: %w", table, err)
	}
	return exists, nil
}
