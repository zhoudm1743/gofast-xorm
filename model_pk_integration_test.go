//go:build integration

package xormdriver

// model_pk_integration_test.go —— X-09 真实库集成验证（stitch-mes 全表更新事故
// 报告 2026-09-07 的 PG/MySQL 落地确认）：Model(&bean).Updates/Update 在 bean
// 主键非零时必须只命中主键行，不再退化为全表 UPDATE。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'ModelPK' -v .
//
// 覆盖场景：
//   - 事故同款路径：First 回填主键 → Model(&bean).Updates(map) 无显式 Where
//   - Update(col, val) 单列、Updates(struct)、Result 变体 RowsAffected
//   - 表达式 UPDATE（X-08 通道）：Update Expr / Updates 混合 map
//   - Model 主键与链上显式 Where 取 AND 交集（不交集 → 0 行不改数据）
//   - 复合主键：双列全非零 → 双列条件；仅一列非零 → 只收敛该列
//   - 零主键 Model 现状锁定：不附加主键条件，范围由链上 Where 决定
//   - PG 专属：schema-per-tenant（Schema + Model(&bean).Updates），
//     即 stitch-mes 事故发生的真实部署形态

import (
	"os"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型 ─────────────────────────────────────────────────────────

// intgXPKUser 单主键模型（默认 xorm tag identifier；TableName 固定表名，
// 避免跨方言 mapper 推导差异）。
type intgXPKUser struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(64) 'name'"`
	Cnt  int64  `xorm:"'cnt'"`
}

func (intgXPKUser) TableName() string { return "intg_xpk_users" }

// intgXPKPair 复合主键模型：验证部分非零主键的收敛行为。
type intgXPKPair struct {
	TenantID string `xorm:"pk varchar(16) 'tenant_id'"`
	UserID   string `xorm:"pk varchar(16) 'user_id'"`
	Name     string `xorm:"varchar(64) 'name'"`
}

func (intgXPKPair) TableName() string { return "intg_xpk_pairs" }

// ── 共用基建 ─────────────────────────────────────────────────────────

// newPlainMySQLXorm 默认 xorm tag identifier 的 MySQL 驱动（对照
// newXormOrmMySQL 的 orm identifier 路径）。
func newPlainMySQLXorm(t *testing.T) *XormDriver {
	t.Helper()
	dsn := os.Getenv("GOFAST_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 GOFAST_TEST_MYSQL_DSN 环境变量，跳过 mysql 集成测试")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver: "xorm",
		Engine: "mysql",
		DSN:    dsn,
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建 xorm mysql 驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// mustReadPKUser 按主键回读单行（链上显式 Where，不经 Model 主键路径）。
func mustReadPKUser(t *testing.T, q contracts.Query, id string) intgXPKUser {
	t.Helper()
	var row intgXPKUser
	if err := q.Model(&intgXPKUser{}).Where("id = ?", id).First(&row); err != nil {
		t.Fatalf("回读 %q: %v", id, err)
	}
	return row
}

// seedPKUsers 重建表并灌入三行基线数据（a=10 / b=20 / c=30）。
func seedPKUsers(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXDropTables(t, drv, "intg_xpk_users")
	if err := drv.AutoMigrate(&intgXPKUser{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	for _, u := range []*intgXPKUser{
		{ID: "a", Name: "alice", Cnt: 10},
		{ID: "b", Name: "bob", Cnt: 20},
		{ID: "c", Name: "carol", Cnt: 30},
	} {
		if err := drv.Query().Create(u); err != nil {
			t.Fatalf("种子 %s: %v", u.ID, err)
		}
	}
}

// runXormModelPKMatrix PG/MySQL 共用的 X-09 行为矩阵。
func runXormModelPKMatrix(t *testing.T, drv *XormDriver) {
	t.Helper()

	t.Run("事故路径_First回填主键_Updates只改一行", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		// 报告 §一 同款写法：First 填充主键 → Model(&user).Updates(map)，链上无 Where
		var bean intgXPKUser
		if err := q.First(&bean, "id = ?", "a"); err != nil {
			t.Fatalf("First: %v", err)
		}
		if err := q.Model(&bean).Updates(map[string]any{"name": "诶嘿"}); err != nil {
			t.Fatalf("Updates: %v", err)
		}
		if got := mustReadPKUser(t, q, "a"); got.Name != "诶嘿" {
			t.Errorf("目标行 a 期望 诶嘿, 实际 %q", got.Name)
		}
		if got := mustReadPKUser(t, q, "b"); got.Name != "bob" {
			t.Errorf("非目标行 b 被改写（全表更新回归）: %q", got.Name)
		}
		if got := mustReadPKUser(t, q, "c"); got.Name != "carol" {
			t.Errorf("非目标行 c 被改写（全表更新回归）: %q", got.Name)
		}
	})

	t.Run("单列Update与Result变体", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		if err := q.Model(&intgXPKUser{ID: "b"}).Update("name", "b-single"); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := mustReadPKUser(t, q, "a"); got.Name != "alice" {
			t.Errorf("非目标行 a 被改写（全表更新回归）: %q", got.Name)
		}

		res := q.Model(&intgXPKUser{ID: "b"}).UpdatesResult(map[string]any{"name": "b2"})
		if res.Error != nil || res.RowsAffected != 1 {
			t.Errorf("UpdatesResult 期望 1 行, 实际 RowsAffected=%d Error=%v", res.RowsAffected, res.Error)
		}
		res = q.Model(&intgXPKUser{ID: "missing"}).UpdateResult("name", "x")
		if res.Error != nil || !res.IsZeroRow() {
			t.Errorf("主键未命中期望 0 行无错, 实际 RowsAffected=%d Error=%v", res.RowsAffected, res.Error)
		}
	})

	t.Run("Updates结构体路径", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		bean := intgXPKUser{ID: "c"}
		if err := q.Model(&bean).Updates(intgXPKUser{Name: "c-struct"}); err != nil {
			t.Fatalf("Updates(struct): %v", err)
		}
		if got := mustReadPKUser(t, q, "c"); got.Name != "c-struct" {
			t.Errorf("目标行 c 期望 c-struct, 实际 %q", got.Name)
		}
		if got := mustReadPKUser(t, q, "a"); got.Name != "alice" {
			t.Errorf("非目标行 a 被改写（全表更新回归）: %q", got.Name)
		}
	})

	t.Run("表达式UPDATE通道", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		// Update 单列表达式：只命中主键行
		if err := q.Model(&intgXPKUser{ID: "b"}).Update("cnt", contracts.Expr("cnt + ?", 5)); err != nil {
			t.Fatalf("Update 表达式: %v", err)
		}
		if got := mustReadPKUser(t, q, "b"); got.Cnt != 25 {
			t.Errorf("目标行 b 期望 cnt=25, 实际 %d", got.Cnt)
		}
		if got := mustReadPKUser(t, q, "a"); got.Cnt != 10 {
			t.Errorf("非目标行 a 被表达式全表更新（回归）: cnt=%d", got.Cnt)
		}

		// Updates 混合表达式 map：主键与链上 Where 叠加（AND）
		if err := q.Model(&intgXPKUser{ID: "c"}).Where("cnt > ?", 0).
			Updates(map[string]any{"cnt": contracts.Expr("cnt * ?", 2), "name": "c-expr"}); err != nil {
			t.Fatalf("Updates 混合表达式: %v", err)
		}
		if got := mustReadPKUser(t, q, "c"); got.Cnt != 60 || got.Name != "c-expr" {
			t.Errorf("目标行 c 期望 cnt=60/name=c-expr, 实际 %+v", got)
		}
		if got := mustReadPKUser(t, q, "a"); got.Cnt != 10 {
			t.Errorf("非目标行 a 被表达式全表更新（回归）: cnt=%d", got.Cnt)
		}
	})

	t.Run("主键与显式Where取AND交集", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		// 主键 a AND name 不匹配 → 0 行，a 保持原值
		res := q.Model(&intgXPKUser{ID: "a"}).Where("name = ?", "no-such").
			UpdatesResult(map[string]any{"name": "x"})
		if res.Error != nil || !res.IsZeroRow() {
			t.Errorf("交集为空期望 0 行无错, 实际 RowsAffected=%d Error=%v", res.RowsAffected, res.Error)
		}
		if got := mustReadPKUser(t, q, "a"); got.Name != "alice" {
			t.Errorf("a 不应被改写, 实际 %q", got.Name)
		}
	})

	t.Run("复合主键", func(t *testing.T) {
		intgXDropTables(t, drv, "intg_xpk_pairs")
		if err := drv.AutoMigrate(&intgXPKPair{}); err != nil {
			t.Fatalf("AutoMigrate(pair): %v", err)
		}
		q := drv.Query()
		for _, p := range []*intgXPKPair{
			{TenantID: "t1", UserID: "u1", Name: "t1u1"},
			{TenantID: "t1", UserID: "u2", Name: "t1u2"},
			{TenantID: "t2", UserID: "u1", Name: "t2u1"},
		} {
			if err := q.Create(p); err != nil {
				t.Fatalf("种子 %+v: %v", p, err)
			}
		}

		// 双列非零：精确命中单行
		if err := q.Model(&intgXPKPair{TenantID: "t1", UserID: "u1"}).
			Updates(map[string]any{"name": "hit"}); err != nil {
			t.Fatalf("复合主键 Updates: %v", err)
		}
		var rows []intgXPKPair
		if err := q.Model(&intgXPKPair{}).Where("name = ?", "hit").Find(&rows); err != nil {
			t.Fatalf("回读 hit: %v", err)
		}
		if len(rows) != 1 || rows[0].TenantID != "t1" || rows[0].UserID != "u1" {
			t.Errorf("复合主键应只命中 t1/u1, 实际 %+v", rows)
		}

		// 仅一列非零：只收敛该列（t2 下全部行）
		if err := q.Model(&intgXPKPair{TenantID: "t2"}).
			Updates(map[string]any{"name": "t2-all"}); err != nil {
			t.Fatalf("部分主键 Updates: %v", err)
		}
		var t2rows []intgXPKPair
		if err := q.Model(&intgXPKPair{}).Where("name = ?", "t2-all").Find(&t2rows); err != nil {
			t.Fatalf("回读 t2-all: %v", err)
		}
		if len(t2rows) != 1 || t2rows[0].TenantID != "t2" {
			t.Errorf("部分主键应只命中 t2 的 1 行, 实际 %+v", t2rows)
		}
	})

	t.Run("零主键现状_范围由链上Where决定", func(t *testing.T) {
		seedPKUsers(t, drv)
		q := drv.Query()

		if err := q.Model(&intgXPKUser{}).Where("id = ?", "a").
			Updates(map[string]any{"name": "a-only"}); err != nil {
			t.Fatalf("零主键+Where Updates: %v", err)
		}
		if got := mustReadPKUser(t, q, "b"); got.Name != "bob" {
			t.Errorf("零主键+Where 不应波及其他行, b 实际 %q", got.Name)
		}
	})
}

// ── PG 入口：public schema 矩阵 + schema-per-tenant 事故形态 ─────────

func TestXormModelPK_PG(t *testing.T) {
	runXormModelPKMatrix(t, newPGXorm(t))
}

// TestXormModelPK_PGTenantSchema stitch-mes 事故的真实部署形态：
// schema-per-tenant 下 Schema(ten).Model(&bean).Updates 必须带 schema 前缀
// 且只命中 bean 主键行；public 下的同名表数据不受影响。
func TestXormModelPK_PGTenantSchema(t *testing.T) {
	drv := newPGXorm(t)
	ten := "tenant_xpk"

	intgXMustExec(t, drv, "DROP SCHEMA IF EXISTS "+ten+" CASCADE")
	intgXMustExec(t, drv, "CREATE SCHEMA "+ten)
	t.Cleanup(func() { _ = drv.Query().Exec("DROP SCHEMA IF EXISTS " + ten + " CASCADE") })
	intgXDropTables(t, drv, "public.intg_xpk_users")

	// 租户表与 public 同名表各灌数据：验证互不串扰
	intgXMustExec(t, drv, `CREATE TABLE `+ten+`.intg_xpk_users (id varchar(16) PRIMARY KEY, name varchar(64), cnt bigint)`)
	intgXMustExec(t, drv, `CREATE TABLE public.intg_xpk_users (id varchar(16) PRIMARY KEY, name varchar(64), cnt bigint)`)
	intgXMustExec(t, drv, `INSERT INTO `+ten+`.intg_xpk_users VALUES ('a','t-alice',10),('b','t-bob',20)`)
	intgXMustExec(t, drv, `INSERT INTO public.intg_xpk_users VALUES ('a','p-alice',1)`)

	// 事故场景还原：租户 schema 下 First 回填 → Model(&bean).Updates
	q := drv.Query().Schema(ten)
	var bean intgXPKUser
	if err := q.First(&bean, "id = ?", "a"); err != nil {
		t.Fatalf("租户 First: %v", err)
	}
	if bean.Name != "t-alice" {
		t.Fatalf("First 应命中租户表, 实际 %q", bean.Name)
	}
	if err := q.Model(&bean).Updates(map[string]any{"name": "租户改名"}); err != nil {
		t.Fatalf("租户 Updates: %v", err)
	}

	// 租户内：只改 a，b 不变
	if got := mustReadPKUser(t, q, "a"); got.Name != "租户改名" {
		t.Errorf("租户目标行期望 租户改名, 实际 %q", got.Name)
	}
	if got := mustReadPKUser(t, q, "b"); got.Name != "t-bob" {
		t.Errorf("租户非目标行被改写（全表更新回归）: %q", got.Name)
	}
	// public 同名表不受影响
	var pub intgXPKUser
	if err := drv.Query().First(&pub, "id = ?", "a"); err != nil {
		t.Fatalf("public 回读: %v", err)
	}
	if pub.Name != "p-alice" {
		t.Errorf("public 同名表不应被租户更新波及, 实际 %q", pub.Name)
	}
}

// ── MySQL 入口 ───────────────────────────────────────────────────────

func TestXormModelPK_MySQL(t *testing.T) {
	runXormModelPKMatrix(t, newPlainMySQLXorm(t))
}
