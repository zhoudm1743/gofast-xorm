//go:build integration

package xormdriver

// fault_integration_test.go —— L4 并发/故障注入矩阵（方案 §5.15/§5.16、§六
// 第 10 项，与 gorm 侧同款矩阵对齐）。依赖真实 PG/MySQL，默认不运行。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -count=1 -run 'TestFault' -v .
//
// 覆盖矩阵：
//   - CON-01 并发唯一键竞态：10 方抢插同一唯一键 → 恰 1 成功、其余 ErrDuplicatedKey
//   - CON-02 并发乐观锁：xorm version 原生语义，5 方并发 Save → 恰 1 赢家
//   - LK-05 死锁映射：PG（deadlock_timeout=200ms）/ MySQL（InnoDB 即时检测）
//     两事务交叉加锁 → 恰一方 ErrDeadlock
//   - ERR-05 查询超时映射：PG statement_timeout / MySQL max_execution_time
//     → ErrQueryTimeout（驱动修复点，此前无映射）
//   - CON-03 隔离级别可见性：PG READ COMMITTED vs REPEATABLE READ 双事务对照；
//     MySQL 默认 REPEATABLE READ（差异固化）
//   - CON-04 锁等待秩序冒烟：三事务顺序等同一 FOR UPDATE 行锁，版本递增=3
//   - CON-05 连接池压力：MaxOpenConns=2 并发 20 查询全部成功
//   - ERR 哨兵对照：PG/MySQL 重复键 + 未命中（SQLite 侧在 errors_matrix_test.go）
//   - ERR-06 手动故障注入：不可达端口 DSN → ErrConnFailed（GOFAST_TEST_MANUAL_CONNFAIL 门控）

import (
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型（表前缀 xflt_，显式 TableName 固定表名）──────────────────

// xfltUniqueRow CON-01 并发唯一键竞态模型。
type xfltUniqueRow struct {
	ID   int64  `xorm:"pk autoincr 'id'"`
	Code string `xorm:"unique varchar(32) 'code'"`
	Val  int64  `xorm:"'val' default(0)"`
}

func (xfltUniqueRow) TableName() string { return "xflt_unique_rows" }

// xfltOptRow CON-02 乐观锁模型：xorm 原生 version tag（插入自动置 1、
// struct 更新自增、陈旧版本 WHERE version=旧值 0 行命中）。
type xfltOptRow struct {
	ID      string `xorm:"pk varchar(16) 'id'"`
	Name    string `xorm:"varchar(32) 'name'"`
	Version int64  `xorm:"version 'version'"`
}

func (xfltOptRow) TableName() string { return "xflt_opt_rows" }

// xfltDlockRow LK-05 死锁交叉加锁模型（a/b 两行互锁）。
type xfltDlockRow struct {
	ID  string `xorm:"pk varchar(8) 'id'"`
	Cnt int64  `xorm:"'cnt' default(0)"`
}

func (xfltDlockRow) TableName() string { return "xflt_dlocks" }

// xfltIsoRow CON-03 隔离级别可见性模型。
type xfltIsoRow struct {
	ID  string `xorm:"pk varchar(8) 'id'"`
	Cnt int64  `xorm:"'cnt'"`
}

func (xfltIsoRow) TableName() string { return "xflt_iso_rows" }

// xfltQueueRow CON-04 锁等待秩序模型。
type xfltQueueRow struct {
	ID  string `xorm:"pk varchar(8) 'id'"`
	Cnt int64  `xorm:"'cnt' default(0)"`
}

func (xfltQueueRow) TableName() string { return "xflt_queue_rows" }

// xfltPoolRow CON-05 连接池压力模型。
type xfltPoolRow struct {
	ID  string `xorm:"pk varchar(8) 'id'"`
	Cnt int64  `xorm:"'cnt' default(0)"`
}

func (xfltPoolRow) TableName() string { return "xflt_pool_rows" }

// xfltSentinelRow ERR 哨兵对照模型（重复键 + 未命中）。
type xfltSentinelRow struct {
	ID   string `xorm:"pk varchar(8) 'id'"`
	Code string `xorm:"unique varchar(32) 'code'"`
}

func (xfltSentinelRow) TableName() string { return "xflt_sentinel_rows" }

// ── 入口 ─────────────────────────────────────────────────────────────

// TestFault_PG PG 侧故障/并发矩阵入口。
func TestFault_PG(t *testing.T) {
	runFaultMatrix(t, newPGXorm(t), "pg")
}

// TestFault_MySQL MySQL 侧故障/并发矩阵入口。
func TestFault_MySQL(t *testing.T) {
	runFaultMatrix(t, newPlainMySQLXorm(t), "mysql")
}

// runFaultMatrix PG/MySQL 共用的 L4 故障注入矩阵。子用例串行执行（共享连接池，
// 隔离级别/会话参数互不干扰）。
func runFaultMatrix(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()

	t.Run("CON-01_并发唯一键竞态", func(t *testing.T) {
		faultConcurrentUniqueInsert(t, drv)
	})
	t.Run("CON-02_并发乐观锁", func(t *testing.T) {
		faultOptimisticLock(t, drv)
	})
	t.Run("LK-05_死锁映射", func(t *testing.T) {
		faultDeadlock(t, drv, dialect)
	})
	t.Run("ERR-05_查询超时映射", func(t *testing.T) {
		faultQueryTimeout(t, drv, dialect)
	})
	t.Run("CON-03_隔离级别可见性", func(t *testing.T) {
		faultIsolationVisibility(t, drv, dialect)
	})
	t.Run("CON-04_锁等待秩序", func(t *testing.T) {
		faultLockQueue(t, drv)
	})
	t.Run("CON-05_连接池压力", func(t *testing.T) {
		faultPoolPressure(t, dialect)
	})
	t.Run("ERR_哨兵对照_重复键与未命中", func(t *testing.T) {
		faultSentinelMatrix(t, drv)
	})
}

// faultWaitDone 带超时的 WaitGroup 等待（并发用例防悬挂：任一 goroutine 卡死
// 时 30s t.Fatalf，不拖垮整个测试进程；复用 fc5WaitCh 的 30s 口径）。
func faultWaitDone(t *testing.T, wg *sync.WaitGroup, name string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s 30s 未完成（疑似悬挂）", name)
	}
}

// ── CON-01 并发唯一键竞态 ────────────────────────────────────────────

// faultConcurrentUniqueInsert 10 goroutine 抢插同一唯一键：恰 1 成功、
// 其余 ErrDuplicatedKey、最终行数=1。方言重复键文案差异（PG 23505 /
// MySQL 1062）由 wrapError 统一归一。
func faultConcurrentUniqueInsert(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_unique_rows"}, &xfltUniqueRow{})

	const racers = 10
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 栅栏：最大化同刻冲突窗口
			errs[i] = drv.Query().Create(&xfltUniqueRow{Code: "race-1", Val: int64(i)})
		}(i)
	}
	close(start)
	faultWaitDone(t, &wg, "CON-01 并发抢插")

	success, dup := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, contracts.ErrDuplicatedKey):
			dup++
		default:
			t.Fatalf("抢插 #%d 非预期错误（应成功或 ErrDuplicatedKey）: %v", i, err)
		}
	}
	if success != 1 || dup != racers-1 {
		t.Fatalf("应恰 1 成功 %d 失败, 实际 success=%d dup=%d", racers-1, success, dup)
	}
	var cnt int64
	if err := drv.Query().Model(&xfltUniqueRow{}).Where("code = ?", "race-1").Count(&cnt); err != nil {
		t.Fatalf("Count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("唯一键应只落 1 行, 实际 %d", cnt)
	}
}

// ── CON-02 并发乐观锁 ────────────────────────────────────────────────

// faultOptimisticLock xorm version 原生语义（差异固化：插入自动置 1；struct
// 更新 WHERE version=旧值 + SET version=version+1；版本冲突以 0 行命中表达，
// 不产生错误——与 gorm 插件式乐观锁返回错误的行为不同）。先顺序对照陈旧版本
// 拒绝，再 5 方并发 Save 断言恰 1 赢家。
func faultOptimisticLock(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_opt_rows"}, &xfltOptRow{})
	q := drv.Query()

	// xorm 原生语义：插入时 version 零值自动置 1
	doc := &xfltOptRow{ID: "opt1", Name: "v0"}
	if err := q.Create(doc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("插入后 version 应自动置 1, 实际 %d", doc.Version)
	}

	// 顺序对照：双方同读 version=1，先写方胜、后写方（陈旧版本）0 行拒绝
	var first, stale xfltOptRow
	if err := q.First(&first, "id = ?", "opt1"); err != nil {
		t.Fatalf("First(first): %v", err)
	}
	if err := q.First(&stale, "id = ?", "opt1"); err != nil {
		t.Fatalf("First(stale): %v", err)
	}
	first.Name = "first"
	if res := q.SaveResult(&first); res.Error != nil || res.RowsAffected != 1 {
		t.Fatalf("先写方应命中 1 行, 实际 rows=%d err=%v", res.RowsAffected, res.Error)
	}
	stale.Name = "stale"
	if res := q.SaveResult(&stale); res.Error != nil || res.RowsAffected != 0 {
		t.Fatalf("陈旧版本应 0 行拒绝（xorm 冲突即 0 行）, 实际 rows=%d err=%v", res.RowsAffected, res.Error)
	}

	// 5 方并发：各自加载最新 version=2 后栅栏齐发，恰 1 赢家
	const writers = 5
	beans := make([]xfltOptRow, writers)
	for i := range beans {
		if err := q.First(&beans[i], "id = ?", "opt1"); err != nil {
			t.Fatalf("并发方 #%d 加载: %v", i, err)
		}
		if beans[i].Version != 2 {
			t.Fatalf("并发方 #%d 应读到 version=2, 实际 %d", i, beans[i].Version)
		}
		beans[i].Name = "w" + string(rune('0'+i))
	}
	results := make([]contracts.Result, writers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = q.SaveResult(&beans[i])
		}(i)
	}
	close(start)
	faultWaitDone(t, &wg, "CON-02 并发乐观锁")

	winners, winnerName := 0, ""
	for i := range results {
		r := results[i]
		switch {
		case r.Error == nil && r.RowsAffected == 1:
			winners++
			winnerName = beans[i].Name
		case r.Error == nil && r.RowsAffected == 0:
			// 版本过期被拒（xorm 原生冲突语义）
		default:
			t.Fatalf("并发方 #%d 非预期结果 rows=%d err=%v", i, r.RowsAffected, r.Error)
		}
	}
	if winners != 1 {
		t.Fatalf("乐观锁应恰 1 方成功, 实际 %d", winners)
	}
	var after xfltOptRow
	if err := q.First(&after, "id = ?", "opt1"); err != nil {
		t.Fatalf("终态回读: %v", err)
	}
	if after.Version != 3 {
		t.Errorf("5 方各尝试一次（1 次成功自增 + 初始 2）终态 version 应为 3, 实际 %d", after.Version)
	}
	if after.Name != winnerName {
		t.Errorf("终态 name 应来自唯一赢家 %q, 实际 %q", winnerName, after.Name)
	}
}

// ── LK-05 死锁映射 ───────────────────────────────────────────────────

// faultDeadlock 两事务交叉行锁（t1 持 a 等 b，t2 持 b 等 a）→ 恰一方 ErrDeadlock。
// 差异固化：PG 死锁探测周期由 deadlock_timeout 控制（默认 1s，SET LOCAL 压到
// 200ms 加速）；MySQL InnoDB 即时检测（错误 1213）无需设置。
// 事务体全程不调用 t.*（goroutine 限制），结果经带超时的 errCh 收取防悬挂；
// ready 信号保证双方各自持有首行锁后再交叉加锁，稳定成环。
func faultDeadlock(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_dlocks"}, &xfltDlockRow{})
	intgXMustExec(t, drv, `INSERT INTO xflt_dlocks (id, cnt) VALUES ('a', 0), ('b', 0)`)

	setTimeout := func(tx contracts.Query) error {
		if dialect != "pg" {
			return nil
		}
		return tx.Exec("SET LOCAL deadlock_timeout = '200ms'")
	}

	t1Ready := make(chan struct{})
	t2Ready := make(chan struct{})
	errT1 := make(chan error, 1)
	errT2 := make(chan error, 1)

	// 事务一：锁 a → 等 t2 就绪 → 交叉锁 b（被 t2 阻塞）
	go func() {
		errT1 <- drv.Query().Transaction(func(tx contracts.Query) error {
			if err := setTimeout(tx); err != nil {
				return err
			}
			if err := tx.Model(&xfltDlockRow{}).Where("id = ?", "a").Update("cnt", 1); err != nil {
				return err
			}
			close(t1Ready)
			<-t2Ready
			return tx.Model(&xfltDlockRow{}).Where("id = ?", "b").Update("cnt", 10)
		})
	}()
	// 事务二：锁 b → 等 t1 就绪 → 交叉锁 a（被 t1 阻塞 → 成环）
	go func() {
		errT2 <- drv.Query().Transaction(func(tx contracts.Query) error {
			if err := setTimeout(tx); err != nil {
				return err
			}
			if err := tx.Model(&xfltDlockRow{}).Where("id = ?", "b").Update("cnt", 1); err != nil {
				return err
			}
			close(t2Ready)
			<-t1Ready
			return tx.Model(&xfltDlockRow{}).Where("id = ?", "a").Update("cnt", 10)
		})
	}()

	fc5WaitCh(t, t1Ready, "事务一持有 a 行锁")
	fc5WaitCh(t, t2Ready, "事务二持有 b 行锁")

	e1 := fc5WaitErrCh(t, errT1, "事务一死锁结果")
	e2 := fc5WaitErrCh(t, errT2, "事务二死锁结果")
	if (e1 == nil) == (e2 == nil) {
		t.Fatalf("交叉加锁应恰一方失败（死锁牺牲者）, 实际 e1=%v e2=%v", e1, e2)
	}
	victim := e1
	if victim == nil {
		victim = e2
	}
	if !errors.Is(victim, contracts.ErrDeadlock) {
		t.Fatalf("死锁应映射 ErrDeadlock, 实际: %v", victim)
	}
}

// ── ERR-05 查询超时映射 ──────────────────────────────────────────────

// faultQueryTimeout 超时注入：PG 事务级 SET statement_timeout + pg_sleep；
// MySQL SET SESSION max_execution_time + SELECT SLEEP。差异固化：PG 超时参数
// 可用 SET LOCAL 限定事务（同连接确定性生效）；MySQL 无事务级超时，SET SESSION
// 随连接回池保留（仅影响 >200ms 查询，池内其余用例均为快查询），结束后尽力
// 恢复默认值。
func faultQueryTimeout(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	var err error
	if dialect == "pg" {
		err = drv.Query().Transaction(func(tx contracts.Query) error {
			if err := tx.Exec("SET LOCAL statement_timeout = 200"); err != nil {
				return err
			}
			return tx.Exec("SELECT pg_sleep(0.5)")
		})
	} else {
		defer func() {
			_ = drv.Query().Exec("SET SESSION max_execution_time = 0")
		}()
		err = drv.Query().Transaction(func(tx contracts.Query) error {
			if err := tx.Exec("SET SESSION max_execution_time = 200"); err != nil {
				return err
			}
			// 注意不用 SELECT SLEEP(1)：SLEEP 会被 max_execution_time 打断并提前
			// 返回 1，但语句本身不向客户端报错（实测 MySQL 8）；重只读 SELECT
			// （information_schema.columns 三表笛卡尔积）才会以 Error 3024 终止。
			return tx.Exec("SELECT COUNT(*) FROM information_schema.columns a, information_schema.columns b, information_schema.columns c")
		})
	}
	if err == nil {
		t.Fatalf("超时查询应失败（pg_sleep(0.5)/重只读 SELECT 超出 200ms 限制）")
	}
	if !errors.Is(err, contracts.ErrQueryTimeout) {
		t.Fatalf("查询超时应映射 ErrQueryTimeout, 实际: %v", err)
	}
}

// ── CON-03 隔离级别可见性 ────────────────────────────────────────────

// faultIsolationVisibility 双事务可见性对照：REPEATABLE READ 全程读快照值，
// READ COMMITTED 每条语句读最新已提交值。
// 差异固化：PG 默认 READ COMMITTED，事务内首条语句 SET TRANSACTION 切级别；
// MySQL 默认 REPEATABLE READ（RR 侧无需 SET），且 SET TRANSACTION 不允许在
// 事务进行中执行（错误 1568）——RC 侧经单连接池驱动先 SET SESSION 再 Begin，
// 保证与 Begin 落在同一物理连接。
func faultIsolationVisibility(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_iso_rows"}, &xfltIsoRow{})
	if err := drv.Query().Create(&xfltIsoRow{ID: "iso", Cnt: 1}); err != nil {
		t.Fatalf("种子: %v", err)
	}
	readCnt := func(t *testing.T, q contracts.Query) int64 {
		t.Helper()
		var row xfltIsoRow
		if err := q.Model(&xfltIsoRow{}).Where("id = ?", "iso").First(&row); err != nil {
			t.Fatalf("事务内读: %v", err)
		}
		return row.Cnt
	}
	bump := func(t *testing.T, v int64) {
		t.Helper()
		if err := drv.Query().Model(&xfltIsoRow{}).Where("id = ?", "iso").Update("cnt", v); err != nil {
			t.Fatalf("外部事务改值 cnt=%d: %v", v, err)
		}
	}

	// ── REPEATABLE READ：外部提交不可见 ──
	rr := drv.Query().Begin()
	if dialect == "pg" {
		if err := rr.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
			t.Fatalf("SET TRANSACTION REPEATABLE READ: %v", err)
		}
	}
	if got := readCnt(t, rr); got != 1 {
		t.Fatalf("RR 首读应见 1, 实际 %d", got)
	}
	bump(t, 2) // 外部事务提交新值
	if got := readCnt(t, rr); got != 1 {
		t.Fatalf("REPEATABLE READ 应持续读快照值 1（外部提交不可见）, 实际 %d", got)
	}
	if err := rr.Rollback(); err != nil {
		t.Fatalf("RR Rollback: %v", err)
	}

	// ── READ COMMITTED：每语句读最新已提交值 ──
	var rc contracts.Query
	if dialect == "pg" {
		rc = drv.Query().Begin()
		if err := rc.Exec("SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			t.Fatalf("SET TRANSACTION READ COMMITTED: %v", err)
		}
	} else {
		single := newFaultSingleConnDriver(t, "mysql", os.Getenv("GOFAST_TEST_MYSQL_DSN"))
		if err := single.Query().Exec("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			t.Fatalf("SET SESSION READ COMMITTED: %v", err)
		}
		t.Cleanup(func() {
			// 尽力恢复会话默认级别（单连接池，回池后仍复用同一连接）
			_ = single.Query().Exec("SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ")
		})
		rc = single.Query().Begin()
	}
	if got := readCnt(t, rc); got != 2 {
		t.Fatalf("RC 首读应见已提交值 2, 实际 %d", got)
	}
	bump(t, 3)
	if got := readCnt(t, rc); got != 3 {
		t.Fatalf("READ COMMITTED 应读到最新已提交值 3, 实际 %d", got)
	}
	if err := rc.Rollback(); err != nil {
		t.Fatalf("RC Rollback: %v", err)
	}
}

// newFaultSingleConnDriver 单连接池驱动（CON-03 MySQL RC 侧专用）：SET SESSION
// 与 Begin 必须落在同一物理连接，MaxOpenConns=1 保证确定性复用。
func newFaultSingleConnDriver(t *testing.T, engine, dsn string) *XormDriver {
	t.Helper()
	if dsn == "" {
		t.Skip("未设置对应 DSN 环境变量，跳过单连接池用例")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver:       "xorm",
		Engine:       engine,
		DSN:          dsn,
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建单连接驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// ── CON-04 锁等待秩序冒烟 ────────────────────────────────────────────

// faultLockQueue 三事务顺序等同一 FOR UPDATE 行锁：事务一持锁等待，事务二/三
// 阻塞排队；释放后串行获锁各 +1。断言三方锁内读到的基线互不相同（{0,1,2}，
// 证明串行化）且终值=3。
func faultLockQueue(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_queue_rows"}, &xfltQueueRow{})
	intgXMustExec(t, drv, `INSERT INTO xflt_queue_rows (id, cnt) VALUES ('q', 0)`)

	const waiters = 2
	captured := make([]int64, waiters+1)
	errCh := make(chan error, waiters+1)
	t1Locked := make(chan struct{})
	release := make(chan struct{})

	// 事务一：持锁并等待释放信号
	go func() {
		errCh <- drv.Query().Transaction(func(tx contracts.Query) error {
			var row xfltQueueRow
			if err := tx.Model(&xfltQueueRow{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "q"); err != nil {
				return err
			}
			captured[0] = row.Cnt
			close(t1Locked)
			<-release
			return tx.Model(&xfltQueueRow{}).Where("id = ?", "q").Update("cnt", row.Cnt+1)
		})
	}()
	fc5WaitCh(t, t1Locked, "事务一持锁")

	// 事务二/三：并发排队等同一行锁，锁内读旧值 +1
	for i := 0; i < waiters; i++ {
		go func(idx int) {
			errCh <- drv.Query().Transaction(func(tx contracts.Query) error {
				var row xfltQueueRow
				if err := tx.Model(&xfltQueueRow{}).Lock(contracts.LockForUpdate).First(&row, "id = ?", "q"); err != nil {
					return err
				}
				captured[idx+1] = row.Cnt
				return tx.Model(&xfltQueueRow{}).Where("id = ?", "q").Update("cnt", row.Cnt+1)
			})
		}(i)
	}
	time.Sleep(300 * time.Millisecond) // 让排队事务到达锁点
	close(release)

	for i := 0; i < waiters+1; i++ {
		if err := fc5WaitErrCh(t, errCh, "锁排队事务"); err != nil {
			t.Fatalf("排队事务 #%d: %v", i, err)
		}
	}
	sorted := append([]int64(nil), captured...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	if sorted[0] != 0 || sorted[1] != 1 || sorted[2] != 2 {
		t.Fatalf("三方锁内读到的基线应为 {0,1,2}（串行递增）, 实际 %v", sorted)
	}
	var after xfltQueueRow
	if err := drv.Query().Model(&xfltQueueRow{}).Where("id = ?", "q").First(&after); err != nil {
		t.Fatalf("终态回读: %v", err)
	}
	if after.Cnt != 3 {
		t.Fatalf("三事务串行各 +1 终值应为 3, 实际 %d", after.Cnt)
	}
}

// ── CON-05 连接池压力 ────────────────────────────────────────────────

// faultPoolPressure MaxOpenConns=2 的驱动并发 20 查询：全部成功（池排队正确、
// 无死锁/超时泄漏）。
func faultPoolPressure(t *testing.T, dialect string) {
	t.Helper()
	engineByDialect := map[string]string{"pg": "postgres", "mysql": "mysql"}
	dsnEnvByDialect := map[string]string{"pg": "GOFAST_TEST_PG_DSN", "mysql": "GOFAST_TEST_MYSQL_DSN"}
	engine, ok := engineByDialect[dialect]
	if !ok {
		t.Fatalf("未知方言 %q", dialect)
	}
	dsn := os.Getenv(dsnEnvByDialect[dialect])
	if dsn == "" {
		t.Skip("未设置 DSN 环境变量，跳过连接池压力用例")
	}
	drv, err := NewXormDriver(contracts.ConnectionConfig{
		Driver:       "xorm",
		Engine:       engine,
		DSN:          dsn,
		MaxOpenConns: 2,
		MaxIdleConns: 2,
	}, noopLog{t: t})
	if err != nil {
		t.Fatalf("创建 MaxOpenConns=2 驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })

	intgXMigrate(t, drv, []string{"xflt_pool_rows"}, &xfltPoolRow{})
	intgXMustExec(t, drv, `INSERT INTO xflt_pool_rows (id, cnt) VALUES ('p', 0)`)

	const workers = 20
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var row xfltPoolRow
			errs[i] = drv.Query().Model(&xfltPoolRow{}).Where("id = ?", "p").First(&row)
		}(i)
	}
	close(start)
	faultWaitDone(t, &wg, "CON-05 连接池压力")
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发查询 #%d 失败: %v", i, err)
		}
	}
}

// ── ERR 哨兵对照（PG/MySQL 补充，SQLite 侧见 errors_matrix_test.go）────

// faultSentinelMatrix 真实方言重复键 + 未命中的哨兵映射对照：
// 重复键 PG 23505 / MySQL 1062 → ErrDuplicatedKey；未命中 → ErrRecordNotFound。
func faultSentinelMatrix(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"xflt_sentinel_rows"}, &xfltSentinelRow{})
	q := drv.Query()

	if err := q.Create(&xfltSentinelRow{ID: "s1", Code: "dup-1"}); err != nil {
		t.Fatalf("种子: %v", err)
	}
	err := q.Create(&xfltSentinelRow{ID: "s2", Code: "dup-1"})
	if err == nil {
		t.Fatalf("重复唯一键应失败")
	}
	if !errors.Is(err, contracts.ErrDuplicatedKey) {
		t.Fatalf("重复键应映射 ErrDuplicatedKey, 实际: %v", err)
	}

	var miss xfltSentinelRow
	err = q.Model(&xfltSentinelRow{}).Where("id = ?", "missing").First(&miss)
	if err == nil {
		t.Fatalf("未命中应失败")
	}
	if !errors.Is(err, contracts.ErrRecordNotFound) {
		t.Fatalf("未命中应映射 ErrRecordNotFound, 实际: %v", err)
	}
}

// ── ERR-06 手动故障注入（manual）─────────────────────────────────────

// TestFault_ManualConnFailure 连接失败 → ErrConnFailed 映射（ERR-06）。
// 对不可达端口 DSN 构造驱动（NewXormDriver 启动期 Ping 失败），断言错误链
// 包装 contracts.ErrConnFailed。不依赖真实库，但会发起真实建连（依赖连接拒绝
// 快速失败），故以 GOFAST_TEST_MANUAL_CONNFAIL 门控：
//
//	GOFAST_TEST_MANUAL_CONNFAIL=1 go test -tags integration -run TestFault_ManualConnFailure -v .
func TestFault_ManualConnFailure(t *testing.T) {
	if os.Getenv("GOFAST_TEST_MANUAL_CONNFAIL") == "" {
		t.Skip("未设置 GOFAST_TEST_MANUAL_CONNFAIL，跳过手动连接失败注入")
	}
	cases := []struct {
		name   string
		engine string
		dsn    string
	}{
		{"pg", "postgres", "postgres://gofast:gofast123@127.0.0.1:1/gofast_test?sslmode=disable"},
		{"mysql", "mysql", "gofast:gofast123@tcp(127.0.0.1:1)/gofast_test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewXormDriver(contracts.ConnectionConfig{
				Driver: "xorm",
				Engine: c.engine,
				DSN:    c.dsn,
			}, nil)
			if err == nil {
				t.Fatalf("不可达端口 DSN 应建连失败")
			}
			if !errors.Is(err, contracts.ErrConnFailed) {
				t.Fatalf("连接失败应映射 ErrConnFailed, 实际: %v", err)
			}
		})
	}
}
