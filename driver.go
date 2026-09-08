package xormdriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
	"xorm.io/xorm/names"
)

// 编译期断言：XormDriver 满足框架驱动契约。
var _ contracts.Driver = (*XormDriver)(nil)

// NewXormDriver 根据连接配置创建 xorm 驱动实例。
// 各引擎对应的 xorm 驱动名（底层 database/sql 驱动经 imports.go blank import 注册）：
//
//	mysql    → "mysql"（go-sql-driver/mysql）
//	postgres → "pgx"（jackc/pgx/v5 stdlib）
//	sqlite   → "sqlite"（glebarez/go-sqlite，纯 Go）
//	mssql    → "mssql"（microsoft/go-mssqldb）
//
// opts 为可选功能行为（迁移安全模式 / 索引收敛，见 migrate.go）；不传保持原行为。
// 自建短生命周期驱动执行 AutoMigrate 是受支持用法（schema-per-tenant 批量迁移），
// 迁移场景推荐 NewMigrateDriver / Migrate（migrate-safe 模式）。
func NewXormDriver(cfg contracts.ConnectionConfig, log contracts.Log, opts ...DriverOption) (*XormDriver, error) {
	cfg.ApplyDefaults()

	var options driverOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	dsn := cfg.BuildDSN()
	if options.migrateSafe {
		if cfg.Engine == "postgres" {
			dsn = migrateSafeDSN(dsn)
		} else if log != nil {
			log.Info("[GoFast] xormdriver driver: WithMigrateSafe 仅支持 postgres，已忽略")
		}
	}

	var engine *xorm.Engine
	var err error

	switch cfg.Engine {
	case "mysql":
		engine, err = newConfiguredEngine(cfg, "mysql", dsn)
	case "postgres":
		// pgx stdlib 的 stdlib.OpenDB 兼容 keyword=value 形式的 DSN，
		// BuildDSN 生成的 "host=... search_path=..." 关键字串可直接使用
		// （未知关键字作为运行时参数下发服务端）。
		engine, err = newConfiguredEngine(cfg, "pgx", dsn)
	case "sqlite", "sqlite3":
		// glebarez/go-sqlite 不识别 BuildDSN 生成中的 mattn 风格参数
		// （_journal_mode/_busy_timeout 等为 mattn/go-sqlite3 专属），
		// 故不走 DSN，直接取文件路径建库；路径为空时使用内存数据库（多用于测试），
		// 带 "file:" 前缀时剥掉以统一为 glebarez 认可的裸路径格式。
		dbPath := cfg.Database
		if dbPath == "" {
			dbPath = ":memory:"
		}
		dbPath = strings.TrimPrefix(dbPath, "file:")
		// 自动创建数据库文件的父目录，避免 "unable to open database file"。
		if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, fmt.Errorf("[GoFast] xormdriver driver: cannot create dir %q: %w", dir, mkErr)
			}
		}
		engine, err = newConfiguredEngine(cfg, "sqlite", dbPath)
	case "mssql":
		engine, err = newConfiguredEngine(cfg, "mssql", dsn)
	default:
		return nil, fmt.Errorf("[GoFast] xormdriver driver: unsupported engine %q", cfg.Engine)
	}

	if err != nil {
		return nil, fmt.Errorf("[GoFast] xormdriver driver: connection failed: %w", err)
	}
	// 有效 identifier（空配置归一为 "xorm"）：AutoMigrate 启动期校验（§10.3 陷阱
	// 告警）与 Preload 忽略标记判定均按它走。
	tagIdentifier := cfg.TagIdentifier
	if tagIdentifier == "" {
		tagIdentifier = "xorm"
	}

	// 桥接框架日志器：SQL 执行日志、慢查询与 xorm 内部日志统一走框架日志器，
	// 保证与其他驱动（gormdriver）的日志级别、输出目标一致。
	// nil 防护：作为公开 API 允许传 nil log（X-09），降级为丢弃输出的空实现。
	if log == nil {
		log = discardLog{}
	}
	engine.SetLogger(newFastLogger(log, cfg.LogLevel, time.Duration(cfg.SlowThreshold)*time.Millisecond))

	// 配置连接池（engine.DB() 内嵌 *sql.DB，单值返回，无 error）。
	sqlDB := engine.DB()
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Minute)
	sqlDB.SetConnMaxIdleTime(time.Duration(cfg.ConnMaxIdleTime) * time.Minute)

	// 验证数据库连通性；失败时关闭已创建的引擎，避免泄漏连接。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.PingContext(ctx); err != nil {
		_ = engine.Close()
		// 连接失败归一（L4 故障矩阵 ERR-06，2026-09-08 此前原样透出为驱动缺陷）：
		// 多 %w 包装（Go 1.20+）同时保留底层错误链，errors.Is 可命中两个错误
		return nil, fmt.Errorf("[GoFast] xormdriver driver: database ping failed: %w: %w", err, contracts.ErrConnFailed)
	}

	return &XormDriver{engine: engine, schema: cfg.Schema, tablePrefix: cfg.TablePrefix, tagIdentifier: tagIdentifier, rebuildIndexes: options.rebuildIndexes, log: log}, nil
}

// newConfiguredEngine 建 xorm 引擎并应用与 NewXormDriver 完全一致的命名/标签/
// schema 语义。NewXormDriver 与迁移观测路径（migrateSQLTx / 临时库克隆预览引擎）
// 共用——BUG-1（stitch-mes 实测反馈）：capture 引擎曾漏 SetColumnMapper，列名回退
// SnakeMapper（user_id→user_i_d），对已有表加列直接 42P16 崩。
// driverName/dsn 由调用方按用途给出（原生驱动名或 capture 驱动名）。
func newConfiguredEngine(cfg contracts.ConnectionConfig, driverName, dsn string) (*xorm.Engine, error) {
	engine, err := xorm.NewEngine(driverName, dsn)
	if err != nil {
		return nil, err
	}
	applyEngineNaming(cfg, engine)
	return engine, nil
}

// applyEngineNaming 应用表名前缀 / schema / 列名 mapper / tag identifier。
// 必须与 NewXormDriver 保持同步（单一代码路径，禁止各自维护）。
func applyEngineNaming(cfg contracts.ConnectionConfig, engine *xorm.Engine) {
	// 配置表名前缀映射（仅表名，列名保持 SnakeMapper）。
	// schema 前缀不并入 mapper：由查询层按 dest 推导后拼接（见 xormdriver.go 的
	// schemaTable/build），显式 Table() 永远优先，与 gormdriver NamingStrategy 语义对齐。
	if cfg.TablePrefix != "" {
		engine.SetTableMapper(names.NewPrefixMapper(names.SnakeMapper{}, cfg.TablePrefix))
	}
	// PostgreSQL 多租户（U18 集成回归发现）：xorm postgres 方言的 DDL（Sync2
	// 建表/改列/索引）以 URI().Schema 限定表名，缺省回退 "public"——事务内
	// SET LOCAL search_path 改变不了它的落点。将连接配置的 Schema 同步给引擎，
	// 保证 AutoMigrate 在正确 schema 建表；DML 侧 schemaTable 已拼前缀的表名
	// 含 "."，xorm 不会重复追加。
	if cfg.Schema != "" && cfg.Engine == "postgres" {
		engine.SetSchema(cfg.Schema)
	}
	// 列名映射（X-02）：默认 GonicMapper（xorm 原生，常用缩写不加下划线，
	// DeptID→dept_id、ID→id，与 GORM NamingStrategy 同一套缩写规则），保证
	// 同一模型在 gorm/xorm 两驱动下列名一致；表名仍为 SnakeMapper 单数。
	// naming: "snake" 回退 SnakeMapper 逐字下划线行为（DeptID→dept_i_d）。
	if cfg.Naming != "snake" {
		engine.SetColumnMapper(names.GonicMapper{})
	}

	// 统一模型标签接入（orm-tag-design.md §8.2）：tag_identifier 配置切换引擎
	// 读取的 struct tag 键名（官方公开 API，引擎级全局生效）。默认 "xorm" 保持
	// 现状；置 "orm" 后该连接所有模型的 orm tag 由 xorm 原生解析（建表/CRUD/
	// created/updated/extends/忽略全部原生行为），xorm:"..." tag 同连接失效。
	// 注意必须在首次 TableInfo/Sync2 之前设置（tagParser 决定列元数据推导）。
	if cfg.TagIdentifier != "" && cfg.TagIdentifier != "xorm" {
		engine.SetTagIdentifier(cfg.TagIdentifier)
	}
}

// Query 创建新的查询构建器实例；可传入 context 用于超时/取消与链路追踪。
func (d *XormDriver) Query(ctx ...context.Context) contracts.Query {
	q := &XormQuery{engine: d.engine, schema: d.schema, qc: d.qc}
	if len(ctx) > 0 && ctx[0] != nil {
		q.ctx = ctx[0]
	}
	return q
}

// DriverName 返回驱动标识（与 database.RegisterDriver 的注册名一致）。
func (d *XormDriver) DriverName() string { return "xorm" }

// Ping 检查数据库连接可用性。
func (d *XormDriver) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return d.engine.PingContext(ctx)
}

// Close 关闭底层连接池。
func (d *XormDriver) Close() error { return d.engine.Close() }

// AutoMigrate 根据 struct 自动建表/迁移。
// 启动期校验先于 Sync2 执行（ormtag_check.go，orm-tag-design.md §8.4/§10.3）：
// 禁用 token/未知裸 token/非法 orm tag 语法直接报错（启动即失败），ext 中 xorm
// 不支持项与 identifier 陷阱只告警不中断。
// PostgreSQL 多租户：在事务内显式 SET LOCAL search_path，确保 DDL 在正确的 schema
// 执行，不依赖连接池的 DSN 初始化值（xorm 事务签名为 func(*Session) (any, error)）。
// 列收敛：Sync2 按基类型判等不改已有列类型/长度/nullable，AutoMigrate 在 Sync2
// 后追加收敛 Pass（migrate_converge.go，与 Sync2 同事务）。
// rebuildIndexes（WithRebuildIndexes，PG/MySQL/MSSQL）：Sync2 前在事务外
// drop 同名异构索引——不能放在事务内：Sync2 内部经 pg_indexes 读取索引定义，
// 会被本事务未提交的 DROP INDEX 锁阻塞（实测确认，IO wait 死等）。
func (d *XormDriver) AutoMigrate(models ...any) error {
	if err := d.validateOrmTags(models); err != nil {
		return err
	}
	// BUG-2（Sync2 内部变体）：Sync2 的索引 drop 循环对唯一约束（gorm unique
	// 建成，pg_constraint 支撑索引）发 DROP INDEX 报 2BP01 中断整个迁移，且
	// oriTable.Indexes 为 map、迭代随机，同列重复索引场景踩中与否不确定。
	// 默认前置按 planUniqueConstraints 的确定性计划预删（仅 pgx 生效，事务外
	// 自动提交），使 Sync2 得以按模型继续收敛重建。
	if err := d.preDropUniqueConstraints(models); err != nil {
		return err
	}
	if d.rebuildIndexes {
		if err := d.dropMismatchedIndexes(models); err != nil {
			return err
		}
	}
	return d.sync2(models...)
}

// preDropUniqueConstraints AutoMigrate 默认前置（BUG-2）：执行
// planUniqueConstraints 的唯一约束预删计划（事务外自动提交，记 Info 日志）。
// 仅 pgx 有约束/索引异构问题（mysql 的 unique 即索引、DROP INDEX 无碍；
// mssql 约束即索引、DROP INDEX 可用），其余引擎计划为空。
func (d *XormDriver) preDropUniqueConstraints(models []any) error {
	plans, err := planUniqueConstraints(d.engine, d.engine.DriverName(), d.schema, models)
	if err != nil {
		return err
	}
	for _, p := range plans {
		for _, name := range p.drops {
			var stmt string
			if d.schema != "" {
				stmt = fmt.Sprintf(`ALTER TABLE "%s"."%s" DROP CONSTRAINT IF EXISTS "%s"`, d.schema, p.table, name)
			} else {
				stmt = fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT IF EXISTS "%s"`, p.table, name)
			}
			if d.log != nil {
				d.log.Info(fmt.Sprintf("[GoFast] xormdriver driver: 唯一约束收敛：%s（%s.%s）", stmt, p.table, name))
			}
			if _, err := d.engine.Exec(stmt); err != nil {
				return fmt.Errorf("xormdriver: drop 唯一约束 %s 失败: %w", name, err)
			}
		}
	}
	return nil
}

// RawEngine 逃生口：允许高级用户直接获取 *xorm.Engine，使用 xorm 原生 API
// （不推荐常规使用，绕过框架的查询语义与缓存/钩子机制）。
func (d *XormDriver) RawEngine() *xorm.Engine { return d.engine }
