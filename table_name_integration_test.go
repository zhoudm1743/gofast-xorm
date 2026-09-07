//go:build integration

package xormdriver

import (
	"reflect"
	"testing"
)

// TableName() 接口识别真实库对照（双驱动联合测试方案专项锁定项）。
// 单元层（table_name_test.go，SQLite）已锁定语义：TableName() 优先于 mapper
// 推导、TablePrefix 不二次前缀、显式 Table() 最高优先。本文件在 PG/MySQL 上
// 对默认 xorm 与 orm 两种 tag identifier 各做一轮读写对照，证明真实方言下
// AutoMigrate/读写落表一致，诱饵表（mapper 推导名）不被触碰。

// runTableNameRealDB 单库单 identifier 的完整流程：seed 即带主键的模型实例。
// findDest 为返回全新空切片指针的闭包（与 seed 同类型）。
func runTableNameRealDB(t *testing.T, drv *XormDriver, seed any, findDest func() any, table, derived string) {
	t.Helper()
	intgXDropTables(t, drv, table, derived)
	// 诱饵表 = SnakeMapper 推导名，放一行诱饵数据。
	intgXMustExec(t, drv, "CREATE TABLE "+derived+" (id VARCHAR(16) PRIMARY KEY, name VARCHAR(100))")
	intgXMustExec(t, drv, "INSERT INTO "+derived+" VALUES ('d1', 'decoy')")
	t.Cleanup(func() { intgXDropTables(t, drv, table, derived) })

	if err := drv.AutoMigrate(seed); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	if err := drv.Query().Create(seed); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 物理锚点：行必须落在 TableName() 表，诱饵表保持 1 行。
	var n int64
	if err := drv.Query().Raw("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%s 期望 1 行, 实际 %d err %v", table, n, err)
	}
	if err := drv.Query().Raw("SELECT COUNT(*) FROM " + derived).Scan(&n); err != nil || n != 1 {
		t.Fatalf("诱饵表 %s 应保持 1 行, 实际 %d err %v", derived, n, err)
	}

	// ORM 读路径命中 TableName() 表。
	dest := findDest()
	if err := drv.Query().Model(seed).Find(dest); err != nil {
		t.Fatalf("Find 失败: %v", err)
	}
	if got := reflectSliceLen(dest); got != 1 {
		t.Fatalf("Find 应命中 %s 的 1 行, 实际 %d 行", table, got)
	}
}

// reflectSliceLen 读切片指针长度（reflect 免按类型分支）。
func reflectSliceLen(dest any) int {
	rv := reflect.ValueOf(dest)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice {
		return -1
	}
	return rv.Len()
}

func TestTableName_RealDB(t *testing.T) {
	t.Run("PG_default_identifier", func(t *testing.T) {
		runTableNameRealDB(t, newPGXorm(t),
			&tnBizUser{ID: "u1", Name: "alice"},
			func() any { return &[]tnBizUser{} },
			"tn_biz_users", "tn_biz_user")
	})
	t.Run("PG_orm_identifier", func(t *testing.T) {
		runTableNameRealDB(t, newXormOrmPG(t),
			&tnOrmBizUser{ID: "u1", Name: "bob"},
			func() any { return &[]tnOrmBizUser{} },
			"tn_orm_biz_users", "tn_orm_biz_user")
	})
	t.Run("MySQL_default_identifier", func(t *testing.T) {
		runTableNameRealDB(t, newPlainMySQLXorm(t),
			&tnBizUser{ID: "u1", Name: "alice"},
			func() any { return &[]tnBizUser{} },
			"tn_biz_users", "tn_biz_user")
	})
	t.Run("MySQL_orm_identifier", func(t *testing.T) {
		runTableNameRealDB(t, newXormOrmMySQL(t),
			&tnOrmBizUser{ID: "u1", Name: "bob"},
			func() any { return &[]tnOrmBizUser{} },
			"tn_orm_biz_users", "tn_orm_biz_user")
	})
}
