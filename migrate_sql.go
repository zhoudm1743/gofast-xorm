package xormdriver

// migrate_sql.go 实现迁移可观测性（需求文档 R4）：
//
//   - MigrateSQL：Dry-run / Diff 预览，返回 models 经 Sync2 将执行的 DDL 列表。
//     postgres / mssql：DDL 支持事务——capture 驱动记录语句后整体回滚，库结构零变化；
//     mysql：DDL 隐式提交无法回滚——把目标库克隆到临时库，在临时库上执行 Sync2
//     并捕获 DDL，结束后 DROP 临时库（需要 CREATE/DROP DATABASE 权限）；
//     sqlite：暂不支持（返回明确错误）。
//   - CheckIndexes：索引收敛体检（pg / mysql / mssql），对比 xorm 期望索引定义
//     （engine.TableInfo，索引名经 XName 规则还原为实际落库名）与库内实际定义，
//     报告 missing / mismatched（同名异构）两类差异；只读不修改。
//   - WithRebuildIndexes（migrate.go）的索引 drop 由 dropMismatchedIndexes 在
//     Sync2 事务外自动提交执行（pg 死锁规避，见 docs/migration.md §4.2），
//     drop 语句按方言生成。
//   - capture 驱动的 mssql 连接委托前由 rewriteQuestionMarks 把 `?` 改写为
//     @pN（go-mssqldb 导出 Driver 零值不处理查询文本，见 registerCaptureDrivers）。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/stdlib"
	mssqldriver "github.com/microsoft/go-mssqldb"
	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/contracts/ormtag"

	"xorm.io/xorm"
	"xorm.io/xorm/dialects"
)

// ── capture 驱动：database/sql 语句捕获 ────────────────────────────────

// capture 驱动注册名（惰性注册一次；包装对应引擎的原生驱动并在所有语句出口记录 SQL）。
const (
	captureDriverPG    = "gofast-capture-pgx"
	captureDriverMySQL = "gofast-capture-mysql"
	captureDriverMSSQL = "gofast-capture-mssql"
)

// captureOnce 保证 capture 驱动全局只注册一次（sql.Register 重复注册会 panic）。
var captureOnce sync.Once

// registerCaptureDrivers 惰性注册三引擎 capture 驱动：委托各自原生驱动
// （pgx stdlib 默认驱动 / go-sql-driver 导出类型 / go-mssqldb 导出类型），
// 在 Prepare/ExecContext/QueryContext 记录 SQL。同时向 xorm 注册驱动名 →
// 方言映射（xorm.NewEngine 只认其注册表内的驱动名）。
//
// 注意：go-mssqldb 的导出类型 Driver{} 零值 processQueryText=false（等价注册名
// "sqlserver"），不会把 `?` 占位符改写为 SQL Server 认识的 @pN；而注册名
// "mssql"（processQueryText=true）的实例未导出、无法包装。因此 capture-mssql
// 连接 rewriteQ=true，在委托前由本包 rewriteQuestionMarks 完成 `?` → @pN
// 改写（与 go-mssqldb internal/querytext 行为对齐）。
func registerCaptureDrivers() {
	captureOnce.Do(func() {
		sql.Register(captureDriverPG, &captureDriver{parent: stdlib.GetDefaultDriver()})
		sql.Register(captureDriverMySQL, &captureDriver{parent: &mysqldriver.MySQLDriver{}})
		sql.Register(captureDriverMSSQL, &captureDriver{parent: &mssqldriver.Driver{}, rewriteQ: true})
		dialects.RegisterDriver(captureDriverPG, dialects.QueryDriver("pgx"))
		dialects.RegisterDriver(captureDriverMySQL, dialects.QueryDriver("mysql"))
		dialects.RegisterDriver(captureDriverMSSQL, dialects.QueryDriver("mssql"))
	})
}

// errCaptureRollback postgres/mssql Dry-run 内部哨兵：Sync2 完成后强制回滚整个
// 事务，实现"执行了 diff 计算、但库结构零变化"的 Dry-run 语义。
var errCaptureRollback = errors.New("xormdriver: capture rollback sentinel")

// captureDriver 包装原生驱动的 Dry-run 捕获驱动。
type captureDriver struct {
	parent driver.Driver
	// rewriteQ 仅 mssql：委托前把 `?` 占位符改写为 @pN（父驱动零值
	// processQueryText=false 不做改写，见 registerCaptureDrivers 注释）。
	rewriteQ bool
}

func (c *captureDriver) Open(name string) (driver.Conn, error) {
	conn, err := c.parent.Open(name)
	if err != nil {
		return nil, err
	}
	return &captureConn{parent: conn, rewriteQ: c.rewriteQ}, nil
}

// OpenConnector 覆盖 DriverContext 路径（database/sql 优先走它）。
func (c *captureDriver) OpenConnector(name string) (driver.Connector, error) {
	if dc, ok := c.parent.(driver.DriverContext); ok {
		nc, err := dc.OpenConnector(name)
		if err != nil {
			return nil, err
		}
		return &captureConnector{parent: nc, rewriteQ: c.rewriteQ}, nil
	}
	return &captureConnector{parent: dsnConnector{dsn: name, parent: c.parent}, rewriteQ: c.rewriteQ}, nil
}

// captureConnector driver.Connector 包装：Connect 返回 captureConn。
type captureConnector struct {
	parent   driver.Connector
	rewriteQ bool
}

func (cc *captureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := cc.parent.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &captureConn{parent: conn, rewriteQ: cc.rewriteQ}, nil
}

func (cc *captureConnector) Driver() driver.Driver { return cc.parent.Driver() }

// dsnConnector 适配非 DriverContext 父驱动的最小 Connector。
type dsnConnector struct {
	dsn    string
	parent driver.Driver
}

func (d dsnConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return d.parent.Open(d.dsn)
}

func (d dsnConnector) Driver() driver.Driver { return d.parent }

// captureConn driver.Conn 包装：记录语句后委托真实连接。
type captureConn struct {
	parent driver.Conn
	// rewriteQ 仅 mssql：委托语句前把 `?` 改写为 @pN（父驱动不处理查询文本）。
	rewriteQ bool
}

// delegate 记录原始语句并返回委托用的语句文本（rewriteQ 时完成 ? → @pN）。
func (c *captureConn) delegate(query string) string {
	captureRecord(query)
	if c.rewriteQ {
		rewritten, _ := rewriteQuestionMarks(query)
		return rewritten
	}
	return query
}

func (c *captureConn) Prepare(query string) (driver.Stmt, error) {
	return c.parent.Prepare(c.delegate(query))
}

func (c *captureConn) Close() error { return c.parent.Close() }

func (c *captureConn) Begin() (driver.Tx, error) { return c.parent.Begin() }

// Ping 委托（database/sql 可选接口探测）。
func (c *captureConn) Ping(ctx context.Context) error {
	if p, ok := c.parent.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// CheckNamedValue 委托（允许 time.Time 等自定义类型直通底层驱动）。
func (c *captureConn) CheckNamedValue(nv *driver.NamedValue) error {
	if cn, ok := c.parent.(driver.NamedValueChecker); ok {
		return cn.CheckNamedValue(nv)
	}
	_, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	return err
}

func (c *captureConn) ResetSession(ctx context.Context) error {
	if rs, ok := c.parent.(driver.SessionResetter); ok {
		return rs.ResetSession(ctx)
	}
	return nil
}

// ExecContext 记录 SQL 后委托（三引擎连接均实现 ExecerContext）。
func (c *captureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	query = c.delegate(query)
	if ec, ok := c.parent.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	// 回退 Prepare 路径（delegate 已记录，直接 Prepare 不再二次记录）。
	stmt, err := c.parent.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.Exec(namedValuesToValues(args))
}

// QueryContext 记录 SQL 后委托（三引擎连接均实现 QueryerContext）。
func (c *captureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	query = c.delegate(query)
	if qc, ok := c.parent.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	stmt, err := c.parent.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.Query(namedValuesToValues(args))
}

// rewriteQuestionMarks 把语句中的 `?` 占位符改写为 SQL Server 序数参数
// @p1..@pN，返回改写后文本与参数个数。字符串字面量（” 转义）、双引号标识符
// （"" 转义）、方括号标识符（]] 转义）、-- 行注释与 /* */ 块注释内的 `?`
// 不改写。与 go-mssqldb internal/querytext.ParseParams 的序数行为对齐。
func rewriteQuestionMarks(query string) (string, int) {
	var (
		out   strings.Builder
		n     int
		inStr bool // '...'
		inDq  bool // "..."
		inBr  bool // [...]
		inLn  bool // --...\n
		inBlk bool // /*...*/
	)
	out.Grow(len(query) + 8)
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch {
		case inStr:
			out.WriteByte(ch)
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					out.WriteByte(query[i+1])
					i++
				} else {
					inStr = false
				}
			}
		case inDq:
			out.WriteByte(ch)
			if ch == '"' {
				if i+1 < len(query) && query[i+1] == '"' {
					out.WriteByte(query[i+1])
					i++
				} else {
					inDq = false
				}
			}
		case inBr:
			out.WriteByte(ch)
			if ch == ']' {
				if i+1 < len(query) && query[i+1] == ']' {
					out.WriteByte(query[i+1])
					i++
				} else {
					inBr = false
				}
			}
		case inLn:
			out.WriteByte(ch)
			if ch == '\n' {
				inLn = false
			}
		case inBlk:
			out.WriteByte(ch)
			if ch == '*' && i+1 < len(query) && query[i+1] == '/' {
				out.WriteByte(query[i+1])
				i++
				inBlk = false
			}
		default:
			switch ch {
			case '\'':
				inStr = true
				out.WriteByte(ch)
			case '"':
				inDq = true
				out.WriteByte(ch)
			case '[':
				inBr = true
				out.WriteByte(ch)
			case '-':
				if i+1 < len(query) && query[i+1] == '-' {
					inLn = true
					out.WriteByte(ch)
				} else {
					out.WriteByte(ch)
				}
			case '/':
				if i+1 < len(query) && query[i+1] == '*' {
					inBlk = true
					out.WriteByte(ch)
				} else {
					out.WriteByte(ch)
				}
			case '?':
				n++
				out.WriteString("@p")
				out.WriteString(strconv.Itoa(n))
			default:
				out.WriteByte(ch)
			}
		}
	}
	return out.String(), n
}

// namedValuesToValues NamedValue → Value（Prepare 回退路径参数转换）。
func namedValuesToValues(named []driver.NamedValue) []driver.Value {
	vals := make([]driver.Value, len(named))
	for i, nv := range named {
		vals[i] = nv.Value
	}
	return vals
}

// captureMu 串行化 MigrateSQL 调用：recorder 为进程级单例，并发调用会互相污染。
var captureMu sync.Mutex

// captureRec 当前生效的记录器（captureMu 持有期间非 nil）。
var captureRec *captureRecorder

// captureRecorder 语句收集器。
type captureRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func captureRecord(query string) {
	if captureRec == nil {
		return
	}
	captureRec.mu.Lock()
	captureRec.sqls = append(captureRec.sqls, query)
	captureRec.mu.Unlock()
}

// ddlFilter CREATE/ALTER/DROP 前缀（trim + 大写后匹配），用于从全部语句中
// 筛出 DDL。Sync2 内部会穿插 information_schema 等查询，不属于迁移产物。
var ddlFilter = regexp.MustCompile(`^(CREATE|ALTER|DROP)\b`)

// isDDL 判断语句是否为 DDL。
func isDDL(query string) bool {
	trimmed := strings.TrimSpace(strings.TrimLeft(query, "("))
	return ddlFilter.MatchString(strings.ToUpper(trimmed))
}

// ── MigrateSQL：Dry-run / Diff 预览 ────────────────────────────────────

// MigrateSQL 返回 models 经 Sync2 将执行的 DDL 列表（Dry-run / Diff 预览，
// 供 CI / 运维预审）。引擎策略：
//
//	postgres / mssql：DDL 事务性——事务内执行 Sync2，capture 驱动记录后整体回滚；
//	mysql：DDL 隐式提交——目标库克隆到临时库执行，捕获后 DROP 临时库；
//	sqlite：不支持（返回明确错误）。
//
// 与 AutoMigrate 同级做 orm tag 启动期校验（Parse 失败即报错）。
// 调用方应串行使用（进程级记录器，并发调用互斥等待）。
func MigrateSQL(cfg contracts.ConnectionConfig, models ...any) ([]string, error) {
	cfg.ApplyDefaults()

	// 启动期校验（ormtag.Parse 全量校验，失败即报错）。
	for _, m := range models {
		if m == nil {
			continue
		}
		if _, err := ormtag.Parse(m); err != nil {
			return nil, fmt.Errorf("xormdriver: AutoMigrate 模型 %T orm tag 校验失败: %w", m, err)
		}
	}

	switch cfg.Engine {
	case "postgres":
		return migrateSQLTx(cfg, captureDriverPG, models)
	case "mssql":
		return migrateSQLTx(cfg, captureDriverMSSQL, models)
	case "mysql":
		return migrateSQLMySQLClone(cfg, models)
	default:
		return nil, fmt.Errorf("xormdriver: MigrateSQL 不支持的引擎 %q（支持 postgres | mssql | mysql）", cfg.Engine)
	}
}

// migrateSQLTx 事务回滚式 Dry-run（postgres / mssql）。
func migrateSQLTx(cfg contracts.ConnectionConfig, captureName string, models []any) ([]string, error) {
	registerCaptureDrivers()

	captureMu.Lock()
	defer captureMu.Unlock()

	rec := &captureRecorder{}
	captureRec = rec
	defer func() { captureRec = nil }()

	engine, err := xorm.NewEngine(captureName, cfg.BuildDSN())
	if err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL 建引擎失败: %w", err)
	}
	defer func() { _ = engine.Close() }()

	if cfg.Schema != "" && cfg.Engine == "postgres" {
		engine.SetSchema(cfg.Schema)
	}
	if cfg.TagIdentifier != "" && cfg.TagIdentifier != "xorm" {
		engine.SetTagIdentifier(cfg.TagIdentifier)
	}

	_, txErr := engine.Transaction(func(s *xorm.Session) (any, error) {
		if cfg.Schema != "" && cfg.Engine == "postgres" {
			if _, err := s.Exec(fmt.Sprintf(`SET LOCAL search_path TO "%s"`, cfg.Schema)); err != nil {
				return nil, fmt.Errorf("set search_path failed: %w", err)
			}
		}
		if err := s.Sync2(models...); err != nil {
			return nil, err
		}
		// 列收敛语句同事务执行（被 capture 记录，随回滚不落库）。
		engineName := map[string]string{"postgres": "pgx", "mssql": "mssql"}[cfg.Engine]
		stmts, err := convergeStatements(engine, engineName, cfg.Schema, models)
		if err != nil {
			return nil, err
		}
		for _, stmt := range stmts {
			if _, err := s.Exec(stmt); err != nil {
				return nil, fmt.Errorf("xormdriver: 列收敛执行失败 %q: %w", stmt, err)
			}
		}
		return nil, errCaptureRollback
	})
	// 哨兵为预期路径；其余错误为真实失败。
	if txErr != nil && !errors.Is(txErr, errCaptureRollback) {
		return nil, txErr
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	return filterDDL(rec.sqls), nil
}

// filterDDL 从语句列表筛出 DDL（CREATE/ALTER/DROP 前缀）。
func filterDDL(sqls []string) []string {
	var ddls []string
	for _, q := range sqls {
		if isDDL(q) {
			ddls = append(ddls, q)
		}
	}
	return ddls
}

// migrateSQLMySQLClone 临时库克隆式 Dry-run（mysql）：DDL 隐式提交无法回滚，
// 把目标库基表结构克隆到临时库 `_gofast_mig_preview_<ts>`，在临时库上执行
// Sync2 捕获 DDL，结束后 DROP 临时库——目标库结构全程零变化。
// 需要 CREATE/DROP DATABASE 权限；返回的 DDL 中临时库名已还原为目标库名。
// 注意：只克隆 BASE TABLE（视图/触发器不参与 diff）。
func migrateSQLMySQLClone(cfg contracts.ConnectionConfig, models []any) ([]string, error) {
	registerCaptureDrivers()

	captureMu.Lock()
	defer captureMu.Unlock()

	rec := &captureRecorder{}
	captureRec = rec
	defer func() { captureRec = nil }()

	// 管理连接（指向目标库，用于建/删临时库与枚举基表）。
	admin, err := xorm.NewEngine("mysql", cfg.BuildDSN())
	if err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 建管理连接失败: %w", err)
	}
	defer func() { _ = admin.Close() }()

	current := ""
	if _, err := admin.SQL("SELECT DATABASE()").Get(&current); err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 读取当前数据库失败: %w", err)
	}
	if current == "" {
		return nil, errors.New("xormdriver: MigrateSQL(mysql) 需要 DSN 指定目标数据库（DATABASE() 为空）")
	}

	tmpDB := fmt.Sprintf("_gofast_mig_preview_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + tmpDB + "`"); err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 建临时库失败（需要 CREATE DATABASE 权限）: %w", err)
	}
	defer func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + tmpDB + "`") }()

	// 克隆目标库全部基表结构（CREATE TABLE ... LIKE 连带索引与主键）。
	var tables []string
	if err := admin.SQL("SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'", current).Find(&tables); err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 枚举基表失败: %w", err)
	}
	for _, tbl := range tables {
		if _, err := admin.Exec("CREATE TABLE `" + tmpDB + "`.`" + tbl + "` LIKE `" + current + "`.`" + tbl + "`"); err != nil {
			return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 克隆表 %s 失败: %w", tbl, err)
		}
	}

	// 在临时库上执行 Sync2（无事务需求——临时库用完即弃）。
	previewDSN := rewriteMySQLDatabase(cfg.BuildDSN(), tmpDB)
	engine, err := xorm.NewEngine(captureDriverMySQL, previewDSN)
	if err != nil {
		return nil, fmt.Errorf("xormdriver: MigrateSQL(mysql) 建预览引擎失败: %w", err)
	}
	defer func() { _ = engine.Close() }()
	if cfg.TagIdentifier != "" && cfg.TagIdentifier != "xorm" {
		engine.SetTagIdentifier(cfg.TagIdentifier)
	}
	if err := engine.Sync2(models...); err != nil {
		return nil, err
	}
	// 列收敛（语句被 capture 记录，在临时库上执行无碍——用完即弃）。
	sess := engine.NewSession()
	defer sess.Close()
	stmts, err := convergeStatements(engine, "mysql", "", models)
	if err != nil {
		return nil, err
	}
	for _, stmt := range stmts {
		if _, err := sess.Exec(stmt); err != nil {
			return nil, fmt.Errorf("xormdriver: 列收敛执行失败 %q: %w", stmt, err)
		}
	}

	// 临时库名还原为目标库名（xorm mysql DDL 引用 `db`.`table` 形态时生效）。
	ddls := filterDDL(rec.sqls)
	for i := range ddls {
		ddls[i] = strings.ReplaceAll(ddls[i], "`"+tmpDB+"`.", "`"+current+"`.")
	}
	return ddls, nil
}

// rewriteMySQLDatabase 重写 go-sql-driver DSN 中的数据库名（"user:pass@tcp(h:p)/db?params"
// 形态；无库名时补 "/db"），用于把连接指向临时库。参数段其余内容保持不变。
func rewriteMySQLDatabase(dsn, db string) string {
	base, query, found := strings.Cut(dsn, "?")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i+1] + db
	} else {
		base += "/" + db
	}
	if found {
		return base + "?" + query
	}
	return base
}

// ── CheckIndexes：索引收敛体检 ─────────────────────────────────────────

// IndexMismatch 索引差异条目。
type IndexMismatch struct {
	Table    string   // 表名（裸名，不含 schema/库名）
	Index    string   // 索引实际落库名（xorm XName 规则还原后）
	Kind     string   // "missing"（期望索引不存在）| "mismatched"（同名异构：列集合/顺序不同）
	Expected []string // 期望列（有序，小写）
	Actual   []string // 实际列（有序，小写；missing 时为 nil）
}

// indexDefRe 从 pg_indexes.indexdef 提取列清单：
// CREATE [UNIQUE] INDEX x ON s.t USING btree (col1, col2 [DESC], ...)。
var indexDefRe = regexp.MustCompile(`\(([^)]*)\)`)

// CheckIndexes 对比 models 的期望索引定义与库内实际索引（只读），返回
// missing / mismatched 差异列表；无差异返回空切片。
// 期望定义来自 engine.TableInfo：索引实际落库名按 xorm CreateIndexSQL 的
// XName 规则还原（IDX_<table>_<name> / UQE_<table>_<name>，已带前缀的名字原样）。
// 支持 postgres（pg_indexes）/ mysql（information_schema.statistics）/ mssql
// （sys.indexes + sys.index_columns）。
func CheckIndexes(cfg contracts.ConnectionConfig, models ...any) ([]IndexMismatch, error) {
	switch cfg.Engine {
	case "postgres", "mysql", "mssql":
	default:
		return nil, fmt.Errorf("xormdriver: CheckIndexes 不支持的引擎 %q（支持 postgres | mysql | mssql）", cfg.Engine)
	}
	drv, err := NewXormDriver(cfg, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close() }()
	return drv.indexMismatches(models)
}

// indexMismatches 差异计算核心（CheckIndexes 与 dropMismatchedIndexes 共用）。
func (d *XormDriver) indexMismatches(models []any) ([]IndexMismatch, error) {
	engineName := ""
	if d.engine != nil {
		engineName = d.engine.DriverName()
	}
	if engineName != "pgx" && engineName != "mysql" && engineName != "mssql" {
		return nil, fmt.Errorf("xormdriver: indexMismatches 不支持的引擎 %q（支持 pgx | mysql | mssql）", engineName)
	}
	var out []IndexMismatch
	for _, m := range models {
		bean := tableBeanOf(m)
		if bean == nil {
			continue
		}
		t, err := d.engine.TableInfo(bean)
		if err != nil {
			return nil, fmt.Errorf("xormdriver: 解析模型 %T 表信息失败: %w", m, err)
		}
		if len(t.Indexes) == 0 {
			continue
		}
		// 表名取最后一段（XName 对 schema 限定名同样取末段）。
		tableName := t.Name
		if idx := strings.LastIndex(tableName, "."); idx >= 0 {
			tableName = tableName[idx+1:]
		}
		actual, err := d.actualIndexes(tableName)
		if err != nil {
			return nil, err
		}
		for _, index := range t.Indexes {
			expectName := index.XName(t.Name)
			expectCols := lowerCols(index.Cols)
			// mysql 的 information_schema.statistics 与 mssql 的 sys.indexes 读取侧
			// 均统一小写键（actualIndexesMySQL/actualIndexesMSSQL），查找时按小写归一；
			// pg 索引名大小写敏感（actualIndexesPG 保留原大小写），精确匹配。
			lookupName := expectName
			if engineName == "mysql" || engineName == "mssql" {
				lookupName = strings.ToLower(expectName)
			}
			got, ok := actual[lookupName]
			if !ok {
				out = append(out, IndexMismatch{Table: tableName, Index: expectName, Kind: "missing", Expected: expectCols})
				continue
			}
			if !equalCols(expectCols, got) {
				out = append(out, IndexMismatch{Table: tableName, Index: expectName, Kind: "mismatched", Expected: expectCols, Actual: got})
			}
		}
	}
	return out, nil
}

// actualIndexes 读取某表的实际索引（列清单有序、小写），按引擎分发。
// postgres：pg_indexes.indexdef 解析；mysql：information_schema.statistics；
// mssql：sys.indexes + sys.index_columns（Go 侧按 key_ordinal 聚合）。
func (d *XormDriver) actualIndexes(table string) (map[string][]string, error) {
	switch d.engine.DriverName() {
	case "pgx":
		return d.actualIndexesPG(table)
	case "mysql":
		return d.actualIndexesMySQL(table)
	case "mssql":
		return d.actualIndexesMSSQL(table)
	}
	return nil, fmt.Errorf("xormdriver: actualIndexes 不支持的引擎 %q", d.engine.DriverName())
}

// actualIndexesPG pg 实际索引（schema 为空时取 current_schema()）。
func (d *XormDriver) actualIndexesPG(table string) (map[string][]string, error) {
	var (
		rows []map[string][]byte
		err  error
	)
	if d.schema != "" {
		rows, err = d.engine.Query(
			`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = ? AND tablename = ?`,
			d.schema, table)
	} else {
		rows, err = d.engine.Query(
			`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ?`,
			table)
	}
	if err != nil {
		return nil, fmt.Errorf("xormdriver: 读取 pg_indexes 失败: %w", err)
	}
	out := make(map[string][]string, len(rows))
	for _, row := range rows {
		out[string(row["indexname"])] = parseIndexDefCols(string(row["indexdef"]))
	}
	return out, nil
}

// actualIndexesMySQL mysql 实际索引（当前连接库，statistics.seq_in_index 保序）。
func (d *XormDriver) actualIndexesMySQL(table string) (map[string][]string, error) {
	rows, err := d.engine.Query(
		`SELECT index_name, GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',') AS cols
		 FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = ?
		 GROUP BY index_name`, table)
	if err != nil {
		return nil, fmt.Errorf("xormdriver: 读取 information_schema.statistics 失败: %w", err)
	}
	out := make(map[string][]string, len(rows))
	for _, row := range rows {
		cols := []string{}
		for _, c := range strings.Split(string(row["cols"]), ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				cols = append(cols, c)
			}
		}
		out[strings.ToLower(string(row["index_name"]))] = cols
	}
	return out, nil
}

// actualIndexesMSSQL mssql 实际索引（key_ordinal 保序，包含列不参与比对；
// schema 非空时限定 sys.schemas）。
func (d *XormDriver) actualIndexesMSSQL(table string) (map[string][]string, error) {
	query := `SELECT i.name AS indexname, c.name AS colname
		FROM sys.indexes i
		JOIN sys.index_columns ic ON i.object_id = ic.object_id AND i.index_id = ic.index_id
		JOIN sys.columns c ON ic.object_id = c.object_id AND ic.column_id = c.column_id
		JOIN sys.tables t ON i.object_id = t.object_id
		WHERE t.name = ? AND i.name IS NOT NULL AND ic.is_included_column = 0 AND i.is_hypothetical = 0`
	args := []any{table}
	if d.schema != "" {
		query += ` AND SCHEMA_NAME(t.schema_id) = ?`
		args = append(args, d.schema)
	}
	query += ` ORDER BY i.name, ic.key_ordinal`
	rows, err := d.engine.Query(append([]any{query}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("xormdriver: 读取 sys.indexes 失败: %w", err)
	}
	out := make(map[string][]string)
	for _, row := range rows {
		name := strings.ToLower(string(row["indexname"]))
		col := strings.ToLower(string(row["colname"]))
		out[name] = append(out[name], col)
	}
	return out, nil
}

// parseIndexDefCols 从 pg indexdef 提取列清单（小写、去引号、剥离 ASC/DESC/COLLATE 修饰）。
func parseIndexDefCols(def string) []string {
	m := indexDefRe.FindStringSubmatch(def)
	if m == nil {
		return nil
	}
	parts := strings.Split(m[1], ",")
	cols := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if i := strings.IndexAny(p, " \t"); i >= 0 {
			p = p[:i] // 剥离 DESC/ASC/COLLATE 等后缀
		}
		p = strings.Trim(p, `"`)
		if p != "" {
			cols = append(cols, strings.ToLower(p))
		}
	}
	return cols
}

// lowerCols 列名转小写副本。
func lowerCols(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = strings.ToLower(c)
	}
	return out
}

// equalCols 有序列清单相等。
func equalCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dropMismatchedIndexes WithRebuildIndexes 的索引 drop：先计算差异（CheckIndexes
// 同一套逻辑），将 mismatched（同名异构）索引在 Sync2 之前、事务之外逐条 DROP
// （自动提交）。missing 无需处理，Sync2 会创建。
//
// 不能放进 Sync2 的事务：Sync2 内部经 pg_indexes（pg_get_indexdef）读取索引定义，
// 其 ACCESS SHARE 会被本事务未提交的 DROP INDEX（ACCESS EXCLUSIVE）阻塞，
// 形成跨连接死等（实测确认，stack 停在 pgx BGReader IO wait）。
// 原子性取舍：DROP 已提交而 Sync2 失败时索引处于缺失态，下次迁移由 Sync2 重建
// （与 gorm AutoMigrate 的非原子行为一致）。
func (d *XormDriver) dropMismatchedIndexes(models []any) error {
	engineName := ""
	if d.engine != nil {
		engineName = d.engine.DriverName()
	}
	switch engineName {
	case "pgx", "mysql", "mssql":
	default:
		if d.log != nil {
			d.log.Info(fmt.Sprintf("[GoFast] xormdriver driver: WithRebuildIndexes 不支持引擎 %q，已忽略", engineName))
		}
		return nil
	}
	mismatches, err := d.indexMismatches(models)
	if err != nil {
		return err
	}
	for _, mm := range mismatches {
		if mm.Kind != "mismatched" {
			continue // missing：Sync2 直接创建
		}
		var stmt string
		switch engineName {
		case "pgx":
			if d.schema != "" {
				stmt = fmt.Sprintf(`DROP INDEX IF EXISTS "%s"."%s"`, d.schema, mm.Index)
			} else {
				stmt = fmt.Sprintf(`DROP INDEX IF EXISTS "%s"`, mm.Index)
			}
		case "mysql":
			stmt = fmt.Sprintf("DROP INDEX `%s` ON `%s`", mm.Index, mm.Table)
		case "mssql":
			if d.schema != "" {
				stmt = fmt.Sprintf("DROP INDEX [%s] ON [%s].[%s]", mm.Index, d.schema, mm.Table)
			} else {
				stmt = fmt.Sprintf("DROP INDEX [%s] ON [%s]", mm.Index, mm.Table)
			}
		}
		if d.log != nil {
			d.log.Info(fmt.Sprintf("[GoFast] xormdriver driver: 索引收敛：%s（%s.%s）", stmt, mm.Table, mm.Index))
		}
		if _, err := d.engine.Exec(stmt); err != nil {
			return fmt.Errorf("xormdriver: drop 异构索引 %s 失败: %w", mm.Index, err)
		}
	}
	return nil
}
