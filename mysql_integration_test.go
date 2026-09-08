//go:build integration

package xormdriver

// mysql_integration_test.go：统一 orm tag 体系的 MySQL 集成测试（设计文档
// §11.15/§11.16/§11.18，U18）。依赖真实数据库，默认不运行。
// 运行方式：设置环境变量 GOFAST_TEST_MYSQL_DSN 后执行 `go test -tags integration`。
// 示例：
//
//	GOFAST_TEST_MYSQL_DSN="user:pass@tcp(host:3306)/db" \
//	  go test -tags integration ./database/drivers/xormdriver/
//
// 与 pg_integration_test.go 同包编译：Preload 全套 runner（runXormIntgPreloadMatrix）
// 与全套 orm tag 模型复用 pg 文件中的定义；本文件专注 MySQL 方言特有断言
// （information_schema / SHOW CREATE TABLE：类型映射、unsigned、comment）与
// §11.16/§11.14/§11.18 的 MySQL 侧行为。

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// newXormOrmMySQL 走 NewXormDriver 真实配置链路创建 mysql 驱动
// （tag_identifier=orm，Sync2 启动期校验/ext 降级告警全部生效）。
func newXormOrmMySQL(t *testing.T) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_MYSQL_DSN 环境变量，跳过 mysql 集成测试")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver:        "xorm",
		Engine:        "mysql",
		DSN:           dsn,
		TagIdentifier: "orm",
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建 xorm mysql 驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// intgXMySQLColumn 读 information_schema 单列元数据（返回键归一为大写：
// 不同 MySQL 版本/驱动对 SELECT 表达式列名的大小写返回不一致，如
// column_type vs COLUMN_TYPE）。
func intgXMySQLColumn(t *testing.T, drv *XormDriver, table, column string) map[string]any {
	t.Helper()
	var rows []map[string]any
	err := drv.Query().Raw(
		`SELECT column_type, is_nullable, column_default, column_comment
		 FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, column).ScanMap(&rows)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取列 %s.%s 元数据失败 rows=%d err=%v", table, column, len(rows), err)
	}
	norm := make(map[string]any, len(rows[0]))
	for k, v := range rows[0] {
		norm[strings.ToUpper(k)] = v
	}
	return norm
}

// TestXormOrmMySQL_DDLFullMatrix（§11.15/§11.18）xorm identifier=orm 全维度 DDL
// 的 MySQL 方言断言：类型映射（varchar/decimal/bigint/bool）、notnull/default、
// comment、联合唯一/联合索引、unsigned。
func TestXormOrmMySQL_DDLFullMatrix(t *testing.T) {
	drv := newXormOrmMySQL(t)
	intgXMigrate(t, drv, []string{"intg_xddl_models"}, &intgXDDLModel{})

	// varchar(100) → varchar(100)
	name := intgXMySQLColumn(t, drv, "intg_xddl_models", "name")
	if fmt.Sprint(name["COLUMN_TYPE"]) != "varchar(100)" {
		t.Errorf("varchar(100) 应映射 varchar(100), 实际 %v", name["COLUMN_TYPE"])
	}
	if name["IS_NULLABLE"] != "NO" {
		t.Errorf("notnull 列应 NOT NULL, 实际 is_nullable=%v", name["IS_NULLABLE"])
	}
	intgXContains(t, "name 默认值", fmt.Sprint(name["COLUMN_DEFAULT"]), "anon")

	// decimal(10,2) 精度
	amount := intgXMySQLColumn(t, drv, "intg_xddl_models", "amount")
	if fmt.Sprint(amount["COLUMN_TYPE"]) != "decimal(10,2)" {
		t.Errorf("decimal(10,2) 应映射 decimal(10,2), 实际 %v", amount["COLUMN_TYPE"])
	}
	if amount["IS_NULLABLE"] != "NO" {
		t.Errorf("amount notnull 应 NOT NULL, 实际 %v", amount["IS_NULLABLE"])
	}

	// bigint / bool（MySQL bool 即 tinyint(1)；5.7 的 bigint 带显示宽度 bigint(20)，
	// 8.0.17+ 起为 bigint——按前缀断言兼容两版本）
	if big := intgXMySQLColumn(t, drv, "intg_xddl_models", "big_num"); !strings.HasPrefix(fmt.Sprint(big["COLUMN_TYPE"]), "bigint") {
		t.Errorf("bigint 应映射 bigint, 实际 %v", big["COLUMN_TYPE"])
	}
	if act := intgXMySQLColumn(t, drv, "intg_xddl_models", "active"); act["COLUMN_TYPE"] != "tinyint(1)" {
		t.Errorf("bool 应映射 tinyint(1), 实际 %v", act["COLUMN_TYPE"])
	}

	// default(0) 数值默认值
	intgXContains(t, "age 默认值", fmt.Sprint(intgXMySQLColumn(t, drv, "intg_xddl_models", "age")["COLUMN_DEFAULT"]), "0")

	// comment('备注列')：MySQL 下列注释随建表 DDL 内联落地
	if cmt := fmt.Sprint(intgXMySQLColumn(t, drv, "intg_xddl_models", "remark")["COLUMN_COMMENT"]); cmt != "备注列" {
		t.Errorf("remark 注释应为 备注列, 实际 %q", cmt)
	}

	// 联合唯一 unique(uk_intg_x_tenant) 与联合索引 index(idx_intg_x_org) 落地
	//（SHOW CREATE TABLE 去反引号后断言）
	var tbl, ddl string
	if err := drv.Query().Raw("SHOW CREATE TABLE intg_xddl_models").Row().Scan(&tbl, &ddl); err != nil {
		t.Fatalf("SHOW CREATE TABLE: %v", err)
	}
	ddl = strings.ReplaceAll(ddl, "`", "")
	t.Logf("SHOW CREATE TABLE intg_xddl_models:\n%s", ddl)
	intgXContains(t, "联合唯一", ddl, "uk_intg_x_tenant")
	intgXContains(t, "联合索引", ddl, "idx_intg_x_org")

	// 列级 unique：应存在覆盖 email 的唯一索引
	var uniqIdx []string
	if err := drv.Query().Raw(
		`SELECT index_name FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = 'intg_xddl_models'
		   AND column_name = 'email' AND non_unique = 0`).Scan(&uniqIdx); err != nil || len(uniqIdx) == 0 {
		t.Errorf("email unique 应存在唯一索引, 实际 %v err=%v", uniqIdx, err)
	}

	// unsigned：仅 MySQL 生效（差异固化：PG 侧加宽为 bigint，见 pg 文件用例）
	intgXMigrate(t, drv, []string{"intg_xunsigned"}, &IntgXUnsignedModel{})
	score := intgXMySQLColumn(t, drv, "intg_xunsigned", "score")
	if !strings.Contains(fmt.Sprint(score["COLUMN_TYPE"]), "unsigned") {
		t.Errorf("unsigned int 应带 UNSIGNED, 实际 %v", score["COLUMN_TYPE"])
	}

	// ext autoIncrementIncrement:5：步长为 MySQL 会话级变量
	//（auto_increment_increment，SHOW VARIABLES 作用域为会话且不影响建表 DDL），
	// 表选项之外的会话行为无法经表结构断言——此处仅固化"迁移不报错、读写正常"
	// （xorm 侧该 ext 整体降级告警，§8.4）。
	var n int64
	if err := drv.Query().Raw("SELECT count(*) FROM intg_xddl_models").Scan(&n); err != nil {
		t.Errorf("autoIncrementIncrement 模型读写应正常: %v", err)
	}

	// 幂等：重复 AutoMigrate 不报错
	if err := drv.AutoMigrate(&intgXDDLModel{}); err != nil {
		t.Errorf("重复 AutoMigrate 应幂等不报错: %v", err)
	}
}

// TestXormOrmMySQL_DuplicateKeyMapping（§11.14/§11.18）MySQL Error 1062 映射
// contracts.ErrDuplicatedKey。
func TestXormOrmMySQL_DuplicateKeyMapping(t *testing.T) {
	drv := newXormOrmMySQL(t)
	intgXMigrate(t, drv, []string{"intg_xdups"}, &intgXDupModel{})

	q := drv.Query()
	if err := q.Create(&intgXDupModel{ID: "d1", Email: "dup@mysql.dev"}); err != nil {
		t.Fatalf("首次插入: %v", err)
	}
	err := q.Create(&intgXDupModel{ID: "d2", Email: "dup@mysql.dev"})
	if err == nil {
		t.Fatal("unique 列重复插入应报错（MySQL Error 1062）")
	}
	if !errors.Is(err, contracts.ErrDuplicatedKey) {
		t.Errorf("唯一冲突应映射 contracts.ErrDuplicatedKey, 实际: %v", err)
	}
}

// TestXormOrmMySQL_LockClauses（§11.10）FOR UPDATE 真实可用；LockShareMode
// 为文档化 no-op（并发互斥场景由 PG 用例覆盖）。
func TestXormOrmMySQL_LockClauses(t *testing.T) {
	drv := newXormOrmMySQL(t)
	intgXMigrate(t, drv, []string{"intg_xlocks"}, &intgXLockModel{})
	intgXMustExec(t, drv, "INSERT INTO intg_xlocks (id, cnt) VALUES ('m1', 5)")

	var rows []intgXLockModel
	if err := drv.Query().Model(&intgXLockModel{}).Lock(contracts.LockForUpdate).Find(&rows); err != nil {
		t.Fatalf("Lock(LockForUpdate) 真实执行不应报错: %v", err)
	}
	if len(rows) != 1 || rows[0].Cnt != 5 {
		t.Errorf("FOR UPDATE 查询结果异常: %+v", rows)
	}
	if err := drv.Query().Model(&intgXLockModel{}).Lock(contracts.LockShareMode).Find(&rows); err != nil {
		t.Fatalf("Lock(LockShareMode) no-op 不应报错: %v", err)
	}
}

// TestXormOrmMySQL_OptimisticLock（§11.18）orm:"version" 模型 MySQL 真实方言冒烟：
// 插入置 1、struct 更新自增、陈旧版本 0 行不报错。
func TestXormOrmMySQL_OptimisticLock(t *testing.T) {
	drv := newXormOrmMySQL(t)
	intgXMigrate(t, drv, []string{"intg_xvers"}, &intgXVerModel{})

	q := drv.Query()
	doc := &intgXVerModel{ID: "v1", Title: "t1"}
	if err := q.Create(doc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("插入后 version 应置 1, 实际 %d", doc.Version)
	}

	got := &intgXVerModel{}
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Take(got); err != nil {
		t.Fatalf("Take: %v", err)
	}
	got.Title = "t2"
	if err := q.Save(got); err != nil {
		t.Fatalf("Save(version 更新): %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("struct 更新后 version 应回填 +1 为 2, 实际 %d", got.Version)
	}

	stale := &intgXVerModel{ID: "v1", Title: "stale", Version: 1}
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Updates(stale); err != nil {
		t.Fatalf("陈旧版本更新应 0 行不报错: %v", err)
	}
	var unchanged intgXVerModel
	if err := q.Model(&intgXVerModel{}).Where("id = ?", "v1").Take(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != 2 || unchanged.Title != "t2" {
		t.Errorf("陈旧版本更新不应改动数据: %+v", unchanged)
	}
}

// TestXormOrmMySQL_ExtDegrade（§11.16）xorm 侧 ext 降级（MySQL 方言对照）：
// CHECK 不生效（非法值写入成功）、migration:false 字段照常建列、created 填 unix 秒。
func TestXormOrmMySQL_ExtDegrade(t *testing.T) {
	drv := newXormOrmMySQL(t)
	intgXMigrate(t, drv, []string{"intg_xexts"}, &intgXExtModel{})

	// migration:false 差异固化：Sync2 照常建列
	var n int64
	if err := drv.Query().Raw(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'intg_xexts' AND column_name = 'remark'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("xorm 侧 migration:false 字段应照常建列, count=%d err=%v", n, err)
	}

	// check 降级：非法值写入成功
	if err := drv.Query().Create(&intgXExtModel{ID: "bad1", Amount: -5}); err != nil {
		t.Errorf("ext check 降级后写入非法值应成功: %v", err)
	}

	// timePrecision 降级：created 填 unix 秒（量级 1e9，非毫秒 1e12）
	nowSec := time.Now().Unix()
	var got intgXExtModel
	if err := drv.Query().Model(&intgXExtModel{}).Where("id = ?", "bad1").Take(&got); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.CreatedAt < nowSec-60 || got.CreatedAt > nowSec+60 {
		t.Errorf("xorm 侧 created 应为 unix 秒（timePrecision 降级）, 实际 %d（now=%d）", got.CreatedAt, nowSec)
	}
}

// TestXormOrmMySQL_PreloadMatrix（§11.18）MySQL 上 has-many / many2many 全套
// 预加载（与 PG 共用 runner）。
func TestXormOrmMySQL_PreloadMatrix(t *testing.T) {
	drv := newXormOrmMySQL(t)
	runXormIntgPreloadMatrix(t, drv)
}
