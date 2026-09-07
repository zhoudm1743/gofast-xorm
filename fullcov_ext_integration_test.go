//go:build integration

package xormdriver

// fullcov_ext_integration_test.go —— contracts.Query 全接口真实库覆盖（分组 5）：
// Schema / GetSchema / Cache / Lock / Unscoped / OnlyTrashed / Restore /
// ForceDelete / Joins / Preload。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovExt_PG$' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovExt_MySQL$' -v .
//
// 覆盖清单：
//   - Schema/GetSchema：PG 租户 schema 全链（Create/Find/Update/Delete 只命中
//     租户表、Model→Schema 与 Schema→Model 两种顺序、public 同名表不串扰、
//     GetSchema 返回值 + 原生 SQL 拼接）；MySQL 无 schema 概念，仅断言行级
//     GetSchema 状态语义。
//   - Cache：EnableCaches(newTestCache()) 后 countCacheStore 计数断言——首次回源
//     put、二次命中（gets/hits 增长 put 不动）、绕过失效直改库后仍返回缓存旧值、
//     驱动写操作后失效回源；未启用缓存的驱动 Cache() 无副作用（unit 套件已有
//     TestXormCache_DisabledNoSideEffect，此处以真实库计数复验）。
//   - Lock：PG LockForUpdate 真实行锁互斥（事务二 lock_timeout 观察阻塞→报错、
//     释放后读到新值）；MySQL 因超时参数难以稳定控制，以普通 FOR UPDATE 不报错
//     + LockShareMode no-op 断言代替（差异说明见用例注释）。
//   - 软删除：xorm `deleted` tag 模型——Delete 后普通查询过滤、Unscoped 查回、
//     OnlyTrashed 只查已删行、Restore 恢复置 0、ForceDelete 物理删除不可恢复。
//   - Joins：缺省 INNER / LEFT JOIN（无匹配补 NULL）/ 带 args 占位符 /
//     PG schema 前缀关联表 / 非法 JOIN 串 ErrUnsupported。
//   - Preload：has-many 约定外键（父表名蛇形_id）与 rel tag 显式声明各一例、
//     belongs-to、空关联回填非 nil 空切片。

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 分组 5 测试模型（默认 xorm tag identifier，显式列 tag + TableName 固定表名）──

// fc5User 通用主模型：Schema/Cache/Joins 场景共用。
type fc5User struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`
	Cnt  int64  `xorm:"'cnt' default(0)"`
}

func (fc5User) TableName() string { return "fc5_users" }

// fc5Order Joins 子表（列与 fc5User 不重名，免去 SELECT 歧义）。
type fc5Order struct {
	ID     string `xorm:"pk varchar(16) 'id'"`
	UserID string `xorm:"varchar(16) 'user_id'"`
	Amount int    `xorm:"'amount'"`
	Title  string `xorm:"varchar(64) 'title'"`
}

func (fc5Order) TableName() string { return "fc5_orders" }

// fc5Lock Lock 场景模型。
type fc5Lock struct {
	ID  string `xorm:"pk varchar(16) 'id'"`
	Cnt int64  `xorm:"'cnt' default(0)"`
}

func (fc5Lock) TableName() string { return "fc5_locks" }

// fc5Soft 软删除模型：int64 + xorm deleted tag（xorm 对 deleted 列自动过滤/
// 自动软删）。
type fc5Soft struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	Name      string `xorm:"varchar(64) 'name'"`
	DeletedAt int64  `xorm:"deleted 'deleted_at'"`
}

func (fc5Soft) TableName() string { return "fc5_softs" }

// ── Preload 模型（独立表组 fc5_pre_*，避免与其他场景的表结构耦合）────────

// fc5PreUser 父模型：Orders 走 rel tag 显式声明；Posts 走约定（外键列 =
// 父表名蛇形 + "_" + 引用列蛇形 = fc5_pre_users_id）；Profile 走约定
// （belongs-to 外键列 = 字段名蛇形 + "_" + 引用列 = profile_id）。
type fc5PreUser struct {
	ID        string        `xorm:"pk varchar(16) 'id'"`
	Name      string        `xorm:"varchar(64) 'name'"`
	ProfileID string        `xorm:"varchar(16) 'profile_id' null"`
	Orders    []fc5PreOrder `xorm:"-" rel:"foreignKey:UserID;references:ID"`
	Posts     []fc5PrePost  `xorm:"-"`
	Profile   *fc5PreProfile `xorm:"-"`
}

func (fc5PreUser) TableName() string { return "fc5_pre_users" }

type fc5PreOrder struct {
	ID     string `xorm:"pk varchar(16) 'id'"`
	UserID string `xorm:"varchar(16) 'user_id'"`
	Amount int    `xorm:"'amount' default(0)"`
}

func (fc5PreOrder) TableName() string { return "fc5_pre_orders" }

// fc5PrePost 约定外键子表：列名对齐约定推断（fc5_pre_users + id）。
type fc5PrePost struct {
	ID       string `xorm:"pk varchar(16) 'id'"`
	AuthorID string `xorm:"varchar(16) 'fc5_pre_users_id'"`
	Title    string `xorm:"varchar(64) 'title'"`
}

func (fc5PrePost) TableName() string { return "fc5_pre_posts" }

type fc5PreProfile struct {
	ID  string `xorm:"pk varchar(16) 'id'"`
	Bio string `xorm:"varchar(64) 'bio'"`
}

func (fc5PreProfile) TableName() string { return "fc5_pre_profiles" }

// ── 分组入口 ───────────────────────────────────────────────────────────

// TestFullCovExt_PG PostgreSQL 全量覆盖入口。
func TestFullCovExt_PG(t *testing.T) {
	runFullCovExt(t, newPGXorm(t), "pg")
}

// TestFullCovExt_MySQL MySQL 全量覆盖入口（PG 专属场景在 runner 内跳过）。
func TestFullCovExt_MySQL(t *testing.T) {
	runFullCovExt(t, newPlainMySQLXorm(t), "mysql")
}

// runFullCovExt 共享 runner：按方法分组，dialect 区分方言分支（"pg"/"mysql"）。
func runFullCovExt(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	t.Run("Schema_GetSchema", func(t *testing.T) { runFC5Schema(t, drv, dialect) })
	t.Run("Cache", func(t *testing.T) { runFC5Cache(t, drv, dialect) })
	t.Run("Lock", func(t *testing.T) { runFC5Lock(t, drv, dialect) })
	t.Run("SoftDelete", func(t *testing.T) { runFC5SoftDelete(t, drv, dialect) })
	t.Run("Joins", func(t *testing.T) { runFC5Joins(t, drv, dialect) })
	t.Run("Preload", func(t *testing.T) { runFC5Preload(t, drv, dialect) })
}

// ── Schema / GetSchema ─────────────────────────────────────────────────

// runFC5Schema PG：租户 schema 全链 CRUD 命中 + public 同名表不串扰 +
// GetSchema 返回值；MySQL：无 schema 概念，仅断言行级状态语义。
func runFC5Schema(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	ten := "fc5_ten"

	// 行级 GetSchema 状态（两方言通用）：默认空、Schema 后为名、空名不切换。
	q := drv.Query()
	if got := q.GetSchema(); got != "" {
		t.Errorf("全新查询 GetSchema 应为空串, 实际 %q", got)
	}
	if got := q.Schema(ten).GetSchema(); got != ten {
		t.Errorf("Schema(%s) 后 GetSchema 应为 %s, 实际 %q", ten, ten, got)
	}
	if got := q.Schema(ten).Table("fc5_users").GetSchema(); got != ten {
		t.Errorf("Schema+Table 链 GetSchema 应为 %s, 实际 %q", ten, got)
	}
	// 连续调用以最后一次为准；空名视为不切换
	if got := q.Schema(ten).Schema(ten + "_2").GetSchema(); got != ten+"_2" {
		t.Errorf("连续 Schema 应取最后一次, 实际 %q", got)
	}
	if got := q.Schema(ten).Schema("").GetSchema(); got != ten {
		t.Errorf("Schema(\"\") 不应切换, 实际 %q", got)
	}
	if dialect != "pg" {
		t.Log("MySQL 无 schema 概念：Schema()/GetSchema() 仅行级状态，不参与 SQL，跳过租户全链")
		return
	}

	// PG：租户 schema 与 public 同名表各灌数据
	intgXMustExec(t, drv, "DROP SCHEMA IF EXISTS "+ten+" CASCADE")
	intgXMustExec(t, drv, "CREATE SCHEMA "+ten)
	t.Cleanup(func() { _ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE") })
	intgXDropTables(t, drv, "public.fc5_users")
	t.Cleanup(func() { intgXDropTables(t, drv, "public.fc5_users") })
	for _, ddl := range []string{
		`CREATE TABLE ` + ten + `.fc5_users (id varchar(16) PRIMARY KEY, name varchar(64), cnt bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE public.fc5_users (id varchar(16) PRIMARY KEY, name varchar(64), cnt bigint NOT NULL DEFAULT 0)`,
		`INSERT INTO ` + ten + `.fc5_users VALUES ('t1', 'tenant-alice', 10)`,
		`INSERT INTO public.fc5_users VALUES ('t1', 'public-alice', 1)`,
	} {
		intgXMustExec(t, drv, ddl)
	}

	// tenCount / pubCount 原生 SQL 计数（各自限定 schema）
	tenCount := func() int64 {
		t.Helper()
		var n int64
		if err := drv.Query().Raw("SELECT count(*) FROM " + ten + ".fc5_users").Scan(&n); err != nil {
			t.Fatalf("count %s.fc5_users: %v", ten, err)
		}
		return n
	}
	pubCount := func() int64 {
		t.Helper()
		var n int64
		if err := drv.Query().Raw("SELECT count(*) FROM public.fc5_users").Scan(&n); err != nil {
			t.Fatalf("count public.fc5_users: %v", err)
		}
		return n
	}

	// Create：Schema→Model 与 Model→Schema 两种顺序命中同一租户表
	uq := drv.Query().Schema(ten)
	if err := uq.Model(&fc5User{}).Create(&fc5User{ID: "t2", Name: "tenant-bob", Cnt: 20}); err != nil {
		t.Fatalf("Schema→Model Create: %v", err)
	}
	if tenCount() != 2 || pubCount() != 1 {
		t.Fatalf("Create 应只命中租户表: ten=%d pub=%d", tenCount(), pubCount())
	}
	if err := drv.Query().Model(&fc5User{}).Schema(ten).
		Create(&fc5User{ID: "t3", Name: "tenant-carol", Cnt: 30}); err != nil {
		t.Fatalf("Model→Schema Create: %v", err)
	}
	if tenCount() != 3 || pubCount() != 1 {
		t.Fatalf("Model→Schema Create 应只命中租户表: ten=%d pub=%d", tenCount(), pubCount())
	}

	// Find：Schema→Model 与 Model→Schema 均命中租户行
	var rowsA []fc5User
	if err := drv.Query().Schema(ten).Model(&fc5User{}).Find(&rowsA); err != nil {
		t.Fatalf("Schema→Model Find: %v", err)
	}
	if len(rowsA) != 3 {
		t.Fatalf("Schema→Model Find 应 3 行（租户表）, 实际 %d", len(rowsA))
	}
	var rowsB []fc5User
	if err := drv.Query().Model(&fc5User{}).Schema(ten).Find(&rowsB); err != nil {
		t.Fatalf("Model→Schema Find: %v", err)
	}
	if len(rowsB) != 3 || rowsB[0].Name == "public-alice" {
		t.Fatalf("Model→Schema Find 应命中租户表, 实际 %+v", rowsB)
	}

	// First 按条件回读：租户名而非 public 同名行
	var one fc5User
	if err := drv.Query().Schema(ten).Model(&fc5User{}).Where("id = ?", "t1").First(&one); err != nil {
		t.Fatalf("租户 First: %v", err)
	}
	if one.Name != "tenant-alice" {
		t.Errorf("Schema First 应命中租户行 tenant-alice, 实际 %q", one.Name)
	}

	// Update：只改租户表，public 同名行不受影响
	if err := drv.Query().Schema(ten).Model(&fc5User{}).
		Where("id = ?", "t1").Updates(map[string]any{"name": "tenant-renamed"}); err != nil {
		t.Fatalf("租户 Updates: %v", err)
	}
	var t1 fc5User
	if err := drv.Query().Schema(ten).Model(&fc5User{}).Where("id = ?", "t1").First(&t1); err != nil {
		t.Fatalf("租户回读: %v", err)
	}
	if t1.Name != "tenant-renamed" {
		t.Errorf("租户行应已改名, 实际 %q", t1.Name)
	}
	var pub fc5User
	if err := drv.Query().Model(&fc5User{}).Where("id = ?", "t1").First(&pub); err != nil {
		t.Fatalf("public 回读: %v", err)
	}
	if pub.Name != "public-alice" {
		t.Errorf("public 同名行不应被租户 Update 波及, 实际 %q", pub.Name)
	}

	// Delete：只删租户行
	if err := drv.Query().Schema(ten).Model(&fc5User{}).
		Where("id = ?", "t3").Delete(&fc5User{}); err != nil {
		t.Fatalf("租户 Delete: %v", err)
	}
	if tenCount() != 2 || pubCount() != 1 {
		t.Errorf("Delete 应只命中租户表: ten=%d pub=%d", tenCount(), pubCount())
	}

	// GetSchema 与原生 SQL 拼接：Raw 表名不自动加前缀，配合 GetSchema 拼全名
	full := drv.Query().Schema(ten)
	var name string
	if err := full.Raw("SELECT name FROM "+full.GetSchema()+".fc5_users WHERE id = ?", "t1").Scan(&name); err != nil {
		t.Fatalf("GetSchema 拼接 Raw 查询: %v", err)
	}
	if name != "tenant-renamed" {
		t.Errorf("GetSchema 拼接查询应命中租户行, 实际 %q", name)
	}
}

// ── Cache ──────────────────────────────────────────────────────────────

// runFC5Cache 真实库查询缓存：计数断言回源/命中/失效，数据新旧互相印证。
func runFC5Cache(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	intgXDropTables(t, drv, "fc5_users")
	if err := drv.AutoMigrate(&fc5User{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, "fc5_users") })

	q := drv.Query()
	// 种子在启用缓存之前写入（写路径不需要触发失效）
	for _, u := range []*fc5User{
		{ID: "w1", Name: "alice", Cnt: 1},
		{ID: "w2", Name: "bob", Cnt: 2},
	} {
		if err := q.Create(u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}

	tc := newTestCache()
	if err := drv.EnableCaches(tc); err != nil {
		t.Fatalf("EnableCaches: %v", err)
	}
	store := tc.countCacheStore
	assertCounts(t, store, "启用后零流量", 0, 0, 0)

	// cachedAlice 每次构造全新同型查询链（缓存键逐片段确定）
	cachedAlice := func() contracts.Query {
		return drv.Query().Cache().Model(&fc5User{}).Where("name = ?", "alice")
	}

	// 首次：get 未命中 → 回源查询 → put 回填
	var first []fc5User
	if err := cachedAlice().Find(&first); err != nil {
		t.Fatalf("首次缓存查询失败: %v", err)
	}
	if len(first) != 1 || first[0].ID != "w1" {
		t.Fatalf("首次查询结果错误: %+v", first)
	}
	assertCounts(t, store, "首次查询应回源", 1, 0, 1)

	// 绕过驱动写层直改库：真实 DB 已有 2 条 alice，缓存仍 1 条
	engineExec(t, drv, `INSERT INTO fc5_users (id, name, cnt) VALUES ('w3', 'alice', 3)`)
	var dbAlice int64
	if err := drv.engine.DB().QueryRow(`SELECT count(*) FROM fc5_users WHERE name = 'alice'`).Scan(&dbAlice); err != nil {
		t.Fatalf("直查数据库: %v", err)
	}
	if dbAlice != 2 {
		t.Fatalf("数据库实际应有 2 条 alice, 得到 %d", dbAlice)
	}

	// 二次同链：命中缓存返回旧结果集（w1），put 不再增长
	var second []fc5User
	if err := cachedAlice().Find(&second); err != nil {
		t.Fatalf("命中缓存查询失败: %v", err)
	}
	if len(second) != 1 || second[0].ID != "w1" {
		t.Fatalf("应命中缓存返回旧结果集 [w1], 实际 %+v", second)
	}
	assertCounts(t, store, "二次查询应命中", 2, 1, 1)

	// 驱动写操作：Update 成功后失效全部查询缓存 → 第三次回源读到最新值
	if err := drv.Query().Model(&fc5User{}).Where("id = ?", "w1").Update("name", "alice2"); err != nil {
		t.Fatalf("驱动 Update: %v", err)
	}
	var third []fc5User
	if err := cachedAlice().Find(&third); err != nil {
		t.Fatalf("失效后缓存查询失败: %v", err)
	}
	if len(third) != 1 || third[0].ID != "w3" {
		t.Fatalf("写失效后应回源返回 [w3], 实际 %+v", third)
	}
	// hits 不增长（是失效回源而非命中旧值），put 增长（回源后重新回填）
	assertCounts(t, store, "写失效后应回源", 3, 1, 2)

	// 未 Cache() 标记的同链查询零缓存流量（Cache 按查询链显式开启）
	var plain []fc5User
	if err := drv.Query().Model(&fc5User{}).Where("name = ?", "alice").Find(&plain); err != nil {
		t.Fatalf("无 Cache 标记查询失败: %v", err)
	}
	assertCounts(t, store, "未标记查询不应读写缓存", 3, 1, 2)
}

// ── Lock ───────────────────────────────────────────────────────────────

// fc5WaitCh 带超时的通道等待（持锁互斥用例防悬挂）。
func fc5WaitCh(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("等待 %s 超时", name)
	}
}

// fc5WaitErrCh 带超时的错误通道接收。
func fc5WaitErrCh(t *testing.T, ch <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("等待 %s 超时", name)
		return nil
	}
}

// runFC5Lock PG：LockForUpdate 行锁真实互斥（事务二 lock_timeout 报锁超时，
// 释放后可获锁读到新值）；MySQL：同款互斥（innodb_lock_wait_timeout=1s，
// 事务内 SET SESSION 生效）；LockShareMode 双库均为文档化 no-op。
func runFC5Lock(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	intgXDropTables(t, drv, "fc5_locks")
	if err := drv.AutoMigrate(&fc5Lock{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, "fc5_locks") })
	intgXMustExec(t, drv, `INSERT INTO fc5_locks (id, cnt) VALUES ('lock1', 1)`)

	if dialect == "pg" {
		// 事务一：FOR UPDATE 锁行并持锁，持锁期间改值
		locked := make(chan struct{})
		release := make(chan struct{})
		errCh := make(chan error, 1)
		go func() {
			errCh <- drv.Query().Transaction(func(tx contracts.Query) error {
				var row fc5Lock
				if err := tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1"); err != nil {
					return err
				}
				close(locked)
				fc5WaitCh(t, release, "事务一释放信号")
				return tx.Model(&fc5Lock{}).Where("id = ?", "lock1").UpdateResult("cnt", 99).Error
			})
		}()
		fc5WaitCh(t, locked, "事务一持锁")

		// 事务二：同行 FOR UPDATE 被互斥——SET LOCAL lock_timeout 观察阻塞
		err := drv.Query().Transaction(func(tx contracts.Query) error {
			if err := tx.Exec("SET LOCAL lock_timeout = '400ms'"); err != nil {
				return err
			}
			var row fc5Lock
			return tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1")
		})
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatalf("第二事务应在锁超时失败（FOR UPDATE 互斥）, 实际: %v", err)
		}

		// 释放后：事务一提交，第三事务可获锁并读到新值
		close(release)
		if err := fc5WaitErrCh(t, errCh, "事务一结果"); err != nil {
			t.Fatalf("事务一: %v", err)
		}
		var after fc5Lock
		if err := drv.Query().Transaction(func(tx contracts.Query) error {
			return tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&after, "id = ?", "lock1")
		}); err != nil {
			t.Fatalf("释放后 FOR UPDATE 应成功: %v", err)
		}
		if after.Cnt != 99 {
			t.Errorf("持锁事务写入应已提交, 期望 cnt=99, 实际 %d", after.Cnt)
		}
		return
	}

	// MySQL：FOR UPDATE 真实行锁互斥。innodb_lock_wait_timeout 是会话级参数，
	// 在事务二内 SET SESSION（database/sql 事务绑定单连接，随后语句同连接生效）。
	// 事务二复用共享 drv 即可——该连接回池后虽保留 1s 超时设置，但只影响
	// "锁等待 >1s" 的场景；正常读写不受影响。
	// 重置基线值，排除 PG 分支/其他用例的残留影响
	intgXMustExec(t, drv, `UPDATE fc5_locks SET cnt = 1 WHERE id = 'lock1'`)

	// 事务一：FOR UPDATE 锁行并持锁，持锁期间改值
	locked := make(chan struct{})
	release := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- drv.Query().Transaction(func(tx contracts.Query) error {
			var row fc5Lock
			if err := tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
			case <-time.After(30 * time.Second):
				return fmt.Errorf("等待事务一释放信号超时")
			}
			return tx.Model(&fc5Lock{}).Where("id = ?", "lock1").UpdateResult("cnt", 99).Error
		})
	}()
	fc5WaitCh(t, locked, "MySQL 事务一持锁")

	// 事务二：同行 FOR UPDATE 被互斥——innodb_lock_wait_timeout=1s 观察阻塞
	err := drv.Query().Transaction(func(tx contracts.Query) error {
		if err := tx.Exec("SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
			return err
		}
		var row fc5Lock
		return tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "lock1")
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "lock wait timeout") {
		t.Fatalf("第二事务应在锁等待超时失败（FOR UPDATE 互斥）, 实际: %v", err)
	}

	// 释放后：事务一提交，第三事务可获锁并读到新值
	close(release)
	if err := fc5WaitErrCh(t, errCh, "MySQL 事务一结果"); err != nil {
		t.Fatalf("事务一: %v", err)
	}
	var after fc5Lock
	if err := drv.Query().Transaction(func(tx contracts.Query) error {
		return tx.Model(&fc5Lock{}).Lock(contracts.LockForUpdate).First(&after, "id = ?", "lock1")
	}); err != nil {
		t.Fatalf("释放后 FOR UPDATE 应成功: %v", err)
	}
	if after.Cnt != 99 {
		t.Errorf("持锁事务写入应已提交, 期望 cnt=99, 实际 %d", after.Cnt)
	}

	// LockShareMode：xorm 无 SHARE 锁支持，文档化 no-op——查询不报错、返回数据
	var share []fc5Lock
	if err := drv.Query().Model(&fc5Lock{}).Lock(contracts.LockShareMode).Find(&share); err != nil {
		t.Fatalf("Lock(LockShareMode) no-op 不应报错: %v", err)
	}
	if len(share) != 1 || share[0].ID != "lock1" {
		t.Errorf("Lock(LockShareMode) 查询结果异常: %+v", share)
	}
}

// ── 软删除：Unscoped / OnlyTrashed / Restore / ForceDelete ─────────────

// runFC5SoftDelete xorm `deleted` tag 模型全链：Delete 转软删（普通查询过滤）、
// Unscoped 查回、OnlyTrashed 只查已删行、Restore 恢复置 0、ForceDelete 物理删除。
func runFC5SoftDelete(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	intgXDropTables(t, drv, "fc5_softs")
	if err := drv.AutoMigrate(&fc5Soft{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, "fc5_softs") })

	q := drv.Query()
	for _, s := range []*fc5Soft{
		{ID: "a", Name: "alice"},
		{ID: "b", Name: "bob"},
		{ID: "c", Name: "carol"},
	} {
		if err := q.Create(s); err != nil {
			t.Fatalf("种子 %s: %v", s.ID, err)
		}
	}

	// countRaw 原生计数（不含任何 xorm 过滤）
	countRaw := func() int64 {
		t.Helper()
		var n int64
		if err := drv.Query().Raw("SELECT count(*) FROM fc5_softs").Scan(&n); err != nil {
			t.Fatalf("原生计数: %v", err)
		}
		return n
	}
	// findNormal / findUnscoped / findTrashed 便捷读取
	findNormal := func() []fc5Soft {
		t.Helper()
		var rows []fc5Soft
		if err := drv.Query().Model(&fc5Soft{}).Order("id").Find(&rows); err != nil {
			t.Fatalf("普通 Find: %v", err)
		}
		return rows
	}
	findUnscoped := func() []fc5Soft {
		t.Helper()
		var rows []fc5Soft
		if err := drv.Query().Unscoped().Model(&fc5Soft{}).Order("id").Find(&rows); err != nil {
			t.Fatalf("Unscoped Find: %v", err)
		}
		return rows
	}
	findTrashed := func() []fc5Soft {
		t.Helper()
		var rows []fc5Soft
		if err := drv.Query().OnlyTrashed().Model(&fc5Soft{}).Order("id").Find(&rows); err != nil {
			t.Fatalf("OnlyTrashed Find: %v", err)
		}
		return rows
	}

	// 基线：三行全部可见
	if rows := findNormal(); len(rows) != 3 {
		t.Fatalf("基线普通查询应 3 行, 实际 %d", len(rows))
	}

	// Delete a → xorm deleted tag 转软删除（物理行保留）
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "a").Delete(&fc5Soft{}); err != nil {
		t.Fatalf("软删除 a: %v", err)
	}
	if countRaw() != 3 {
		t.Fatalf("软删除不应物理移除行, 原生计数应 3, 实际 %d", countRaw())
	}
	if rows := findNormal(); len(rows) != 2 {
		t.Errorf("Delete 后普通查询应过滤已删行（剩 2）, 实际 %d: %+v", len(rows), rows)
	}
	trashed := findTrashed()
	if len(trashed) != 1 || trashed[0].ID != "a" {
		t.Errorf("OnlyTrashed 应只查已删行 [a], 实际 %+v", trashed)
	}
	if rows := findUnscoped(); len(rows) != 3 {
		t.Fatalf("Unscoped 应查回 3 行, 实际 %d", len(rows))
	} else {
		for _, r := range rows {
			if r.ID == "a" && r.DeletedAt == 0 {
				t.Errorf("软删行 a 的 deleted_at 应非 0, 实际 %d", r.DeletedAt)
			}
			if r.ID != "a" && r.DeletedAt != 0 {
				t.Errorf("未删行 %s 的 deleted_at 应为 0, 实际 %d", r.ID, r.DeletedAt)
			}
		}
	}

	// Restore a → deleted_at 置 0，普通查询可见
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "a").Restore(); err != nil {
		t.Fatalf("Restore a: %v", err)
	}
	if rows := findNormal(); len(rows) != 3 {
		t.Errorf("Restore 后普通查询应恢复 3 行, 实际 %d", len(rows))
	}
	if trashed := findTrashed(); len(trashed) != 0 {
		t.Errorf("Restore 后 OnlyTrashed 应为空, 实际 %+v", trashed)
	}
	if rows := findUnscoped(); len(rows) == 3 {
		for _, r := range rows {
			if r.ID == "a" && r.DeletedAt != 0 {
				t.Errorf("Restore 后 a 的 deleted_at 应为 0, 实际 %d", r.DeletedAt)
			}
		}
	}

	// 再软删 b、c → OnlyTrashed 恰为 [b, c]
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "b").Delete(&fc5Soft{}); err != nil {
		t.Fatalf("软删除 b: %v", err)
	}
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "c").Delete(&fc5Soft{}); err != nil {
		t.Fatalf("软删除 c: %v", err)
	}
	if trashed := findTrashed(); len(trashed) != 2 {
		t.Errorf("OnlyTrashed 应恰为 [b c], 实际 %+v", trashed)
	}

	// ForceDelete b → 物理删除：原生计数减一、OnlyTrashed 剩 c、Unscoped 无 b
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "b").ForceDelete(&fc5Soft{}); err != nil {
		t.Fatalf("ForceDelete b: %v", err)
	}
	if countRaw() != 2 {
		t.Errorf("ForceDelete 应物理移除行, 原生计数应 2, 实际 %d", countRaw())
	}
	if trashed := findTrashed(); len(trashed) != 1 || trashed[0].ID != "c" {
		t.Errorf("ForceDelete 后 OnlyTrashed 应剩 [c], 实际 %+v", trashed)
	}
	for _, r := range findUnscoped() {
		if r.ID == "b" {
			t.Errorf("ForceDelete 后 b 不应再出现（物理删除不可恢复）: %+v", r)
		}
	}

	// Restore c → 普通查询 [a, c]
	if err := drv.Query().Model(&fc5Soft{}).Where("id = ?", "c").Restore(); err != nil {
		t.Fatalf("Restore c: %v", err)
	}
	if rows := findNormal(); len(rows) != 2 {
		t.Errorf("Restore c 后普通查询应为 [a c], 实际 %d 行", len(rows))
	}
}

// ── Joins ──────────────────────────────────────────────────────────────

// runFC5Joins INNER/LEFT/带参/非法串/PG schema 前缀关联表。
func runFC5Joins(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	// seedFC5JoinTables 重建 fc5_users/fc5_orders 并灌入两用户三订单
	seedFC5JoinTables := func() {
		t.Helper()
		intgXDropTables(t, drv, "fc5_users", "fc5_orders")
		if err := drv.AutoMigrate(&fc5User{}, &fc5Order{}); err != nil {
			t.Fatalf("AutoMigrate: %v", err)
		}
		q := drv.Query()
		for _, s := range []any{
			&fc5User{ID: "u1", Name: "alice", Cnt: 1},
			&fc5User{ID: "u2", Name: "bob", Cnt: 2},
			&fc5User{ID: "u3", Name: "carol", Cnt: 3}, // 无订单：LEFT JOIN 补 NULL
			&fc5Order{ID: "o1", UserID: "u1", Amount: 100, Title: "t100"},
			&fc5Order{ID: "o2", UserID: "u1", Amount: 250, Title: "t250"},
			&fc5Order{ID: "o3", UserID: "u2", Amount: 50, Title: "t50"},
		} {
			if err := q.Create(s); err != nil {
				t.Fatalf("种子 %T: %v", s, err)
			}
		}
	}
	// joinRows 执行带显式投影的联表查询：返回原始行（uid 与 amount 可能因
	// 方言不同以 string/[]byte/int64 形态出现，比较一律经 fc5MapStr/fc5MapNum）
	joinRows := func(chain contracts.Query) []map[string]any {
		t.Helper()
		var dest []map[string]any
		if err := chain.ScanMap(&dest); err != nil {
			t.Fatalf("联表 ScanMap: %v", err)
		}
		return dest
	}
	// groupByUser 按 uid 分组金额列表（同用户多行不折叠）。
	groupByUser := func(rows []map[string]any) map[string][]int64 {
		out := make(map[string][]int64, len(rows))
		for _, m := range rows {
			uid := fc5MapStr(m["uid"])
			out[uid] = append(out[uid], fc5MapNum(m["amount"]))
		}
		return out
	}
	// hasAll 断言金额列表包含全部期望值（INNER 无匹配不产生行）。
	hasAll := func(got []int64, want ...int64) bool {
		for _, w := range want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return len(got) == len(want)
	}
	// fc5NilLike SQL NULL 经各方言归一后的形态（nil / 空串/空字节 / 数值零——
	// 如 pgx 将 NULL INT4 归一为 int32(0)）。
	fc5NilLike := func(v any) bool {
		if v == nil {
			return true
		}
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String, reflect.Slice:
			return rv.Len() == 0
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return rv.Int() == 0
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return rv.Uint() == 0
		case reflect.Float32, reflect.Float64:
			return rv.Float() == 0
		}
		return false
	}

	// 缺省 INNER JOIN（无前缀关键字）：u1 两单(100/250)、u2 一单(50)、u3 无行
	seedFC5JoinTables()
	inner := groupByUser(joinRows(drv.Query().Table("fc5_users").
		Select("fc5_users.id as uid, fc5_orders.amount").
		Joins("JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id").
		Order("fc5_users.id")))
	if _, dup := inner["u3"]; dup {
		t.Errorf("缺省 INNER JOIN 不应含无订单用户 u3, 实际 %v", inner)
	}
	if !hasAll(inner["u1"], 100, 250) || !hasAll(inner["u2"], 50) {
		t.Errorf("缺省 INNER JOIN 结果异常: %v", inner)
	}

	// LEFT JOIN：u3 无订单 → 该行存在且 amount 为 NULL（方言归一形态）
	left := groupByUser(joinRows(drv.Query().Table("fc5_users").
		Select("fc5_users.id as uid, fc5_orders.amount").
		Joins("LEFT JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id").
		Order("fc5_users.id")))
	if !hasAll(left["u1"], 100, 250) || !hasAll(left["u2"], 50) {
		t.Errorf("LEFT JOIN 金额回读异常: %v", left)
	}
	{
		// u3 行本身必须存在（LEFT JOIN 语义），amount 值为 NULL 形态
		var u3row map[string]any
		for _, m := range joinRows(drv.Query().Table("fc5_users").
			Select("fc5_users.id as uid, fc5_orders.amount").
			Joins("LEFT JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id").
			Where("fc5_users.id = ?", "u3")) {
			u3row = m
		}
		if u3row == nil {
			t.Error("LEFT JOIN 应包含无订单用户 u3 行")
		} else if !fc5NilLike(u3row["amount"]) {
			t.Errorf("u3 无订单 amount 应为 NULL/零值形态, 实际 %#v", u3row["amount"])
		}
	}

	// JOIN 条件带 args 占位符（amount >= 100 → 命中 u1 的 100/250 两单）
	argRows := groupByUser(joinRows(drv.Query().Table("fc5_users").
		Select("fc5_users.id as uid, fc5_orders.amount").
		Joins("INNER JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id AND fc5_orders.amount >= ?", 100).
		Order("fc5_users.id")))
	if !hasAll(argRows["u1"], 100, 250) || len(argRows["u2"]) != 0 {
		t.Errorf("带参 JOIN 应命中 u1 两单(>=100), 实际 %v", argRows)
	}

	// 非法 JOIN 串 → ErrUnsupported（缺 JOIN 关键字）
	err := drv.Query().Table("fc5_users").Joins("BOGUS JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id").
		Find(&[]map[string]any{})
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("非法 JOIN 串应报 ErrUnsupported, 实际: %v", err)
	}
	// 非法 JOIN 串 → ErrUnsupported（缺 ON 条件）
	err = drv.Query().Table("fc5_users").Joins("JOIN fc5_orders").Find(&[]fc5User{})
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("缺 ON 的 JOIN 串应报 ErrUnsupported, 实际: %v", err)
	}

	if dialect == "pg" {
		// PG：Schema(ten) 下关联表名同样带租户前缀（两表均在租户 schema 内联查）
		ten := "fc5_jten"
		intgXMustExec(t, drv, "DROP SCHEMA IF EXISTS "+ten+" CASCADE")
		intgXMustExec(t, drv, "CREATE SCHEMA "+ten)
		t.Cleanup(func() { _ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE") })
		for _, ddl := range []string{
			`CREATE TABLE ` + ten + `.fc5_users (id varchar(16) PRIMARY KEY, name varchar(64), cnt bigint NOT NULL DEFAULT 0)`,
			`CREATE TABLE ` + ten + `.fc5_orders (id varchar(16) PRIMARY KEY, user_id varchar(16), amount int, title varchar(64))`,
			`INSERT INTO ` + ten + `.fc5_users VALUES ('ju1', 'tenant-alice', 1)`,
			`INSERT INTO ` + ten + `.fc5_users VALUES ('ju2', 'tenant-bob', 2)`,
			`INSERT INTO ` + ten + `.fc5_orders VALUES ('jo1', 'ju1', 100, 'a')`,
			`INSERT INTO ` + ten + `.fc5_orders VALUES ('jo2', 'ju2', 50, 'b')`,
		} {
			intgXMustExec(t, drv, ddl)
		}
		tenRows := groupByUser(joinRows(drv.Query().Schema(ten).Table("fc5_users").
			Select("fc5_users.id as uid, fc5_orders.amount").
			Joins("INNER JOIN fc5_orders ON fc5_orders.user_id = fc5_users.id").
			Order("fc5_users.id")))
		if !hasAll(tenRows["ju1"], 100) || !hasAll(tenRows["ju2"], 50) {
			t.Errorf("schema JOIN 应命中租户两表, 实际 %v", tenRows)
		}
	}
}

// fc5MapStr / fc5MapNum ScanMap 值归一（[]byte/string/int64 等方言形态 → 标量）。
func fc5MapStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

func fc5MapNum(v any) int64 {
	var out int64
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		for _, c := range n {
			if c < '0' || c > '9' {
				return out
			}
			out = out*10 + int64(c-'0')
		}
		return out
	case []byte:
		for _, c := range n {
			if c < '0' || c > '9' {
				return out
			}
			out = out*10 + int64(c-'0')
		}
	}
	return out
}

// ── Preload ────────────────────────────────────────────────────────────

// runFC5Preload 共享引擎预加载：has-many 约定外键（Posts）与 rel tag 显式
// （Orders）各一例、belongs-to（Profile）、空关联回填非 nil 空切片。
func runFC5Preload(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	tables := []string{"fc5_pre_users", "fc5_pre_orders", "fc5_pre_posts", "fc5_pre_profiles"}
	intgXDropTables(t, drv, tables...)
	if err := drv.AutoMigrate(&fc5PreUser{}, &fc5PreOrder{}, &fc5PrePost{}, &fc5PreProfile{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { intgXDropTables(t, drv, tables...) })

	q := drv.Query()
	for _, s := range []any{
		&fc5PreProfile{ID: "p1", Bio: "bio-of-alice"},
		&fc5PreUser{ID: "u1", Name: "alice", ProfileID: "p1"},
		&fc5PreUser{ID: "u2", Name: "bob"},   // 无 Profile
		&fc5PreUser{ID: "u3", Name: "carol"}, // 无订单/无帖子
		&fc5PreOrder{ID: "o1", UserID: "u1", Amount: 100},
		&fc5PreOrder{ID: "o2", UserID: "u1", Amount: 250},
		&fc5PreOrder{ID: "o3", UserID: "u2", Amount: 50},
		&fc5PrePost{ID: "po1", AuthorID: "u1", Title: "post-1"},
		&fc5PrePost{ID: "po2", AuthorID: "u1", Title: "post-2"},
		&fc5PrePost{ID: "po3", AuthorID: "u2", Title: "post-3"},
	} {
		if err := q.Create(s); err != nil {
			t.Fatalf("种子 %T %+v: %v", s, s, err)
		}
	}

	// 原生回读交叉验证种子落库（约定外键列 fc5_pre_users_id 真实存在）
	var rawPosts int64
	if err := drv.Query().Raw("SELECT count(*) FROM fc5_pre_posts").Scan(&rawPosts); err != nil {
		t.Fatalf("fc5_pre_posts 原生计数: %v", err)
	}
	if rawPosts != 3 {
		t.Fatalf("fc5_pre_posts 应有 3 行, 实际 %d", rawPosts)
	}

	var users []fc5PreUser
	if err := q.Preload("Orders").Preload("Posts").Preload("Profile").
		Order("id").Find(&users); err != nil {
		t.Fatalf("Preload 全套查询失败: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("应查到 3 个用户, 实际 %d", len(users))
	}
	byID := make(map[string]*fc5PreUser, len(users))
	for i := range users {
		byID[users[i].ID] = &users[i]
	}

	// u1：Orders（rel tag）2 单、Posts（约定外键）2 帖、Profile（belongs-to）1 条
	u1 := byID["u1"]
	if len(u1.Orders) != 2 {
		t.Errorf("u1 应预加载 2 个订单(rel tag), 实际 %d: %+v", len(u1.Orders), u1.Orders)
	}
	amountSet := map[int]bool{}
	for _, o := range u1.Orders {
		amountSet[o.Amount] = true
	}
	if !amountSet[100] || !amountSet[250] {
		t.Errorf("u1 订单金额应含 100/250, 实际 %+v", u1.Orders)
	}
	if len(u1.Posts) != 2 {
		t.Errorf("u1 应预加载 2 条帖子(约定外键 fc5_pre_users_id), 实际 %d: %+v", len(u1.Posts), u1.Posts)
	}
	if u1.Profile == nil || u1.Profile.Bio != "bio-of-alice" {
		t.Errorf("u1 belongs-to Profile 应回填 bio-of-alice, 实际 %+v", u1.Profile)
	}

	// u2：各 1 条关联；Profile 无外键值保持 nil
	u2 := byID["u2"]
	if len(u2.Orders) != 1 || u2.Orders[0].Amount != 50 {
		t.Errorf("u2 应预加载 1 个订单 o3, 实际 %+v", u2.Orders)
	}
	if len(u2.Posts) != 1 || u2.Posts[0].Title != "post-3" {
		t.Errorf("u2 应预加载 1 条帖子 post-3, 实际 %+v", u2.Posts)
	}
	if u2.Profile != nil {
		t.Errorf("u2 无 profile 外键值, Profile 应为 nil, 实际 %+v", u2.Profile)
	}

	// u3：无任何关联 → has-many 回填非 nil 空切片、belongs-to 保持 nil
	u3 := byID["u3"]
	if u3.Orders == nil || len(u3.Orders) != 0 {
		t.Errorf("u3 无订单应回填非 nil 空切片, 实际 %+v (nil=%v)", u3.Orders, u3.Orders == nil)
	}
	if u3.Posts == nil || len(u3.Posts) != 0 {
		t.Errorf("u3 无帖子应回填非 nil 空切片, 实际 %+v (nil=%v)", u3.Posts, u3.Posts == nil)
	}
	if u3.Profile != nil {
		t.Errorf("u3 无 profile, Profile 应为 nil, 实际 %+v", u3.Profile)
	}
}
