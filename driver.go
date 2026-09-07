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
func NewXormDriver(cfg contracts.ConnectionConfig, log contracts.Log) (*XormDriver, error) {
	cfg.ApplyDefaults()

	dsn := cfg.BuildDSN()

	var engine *xorm.Engine
	var err error

	switch cfg.Engine {
	case "mysql":
		engine, err = xorm.NewEngine("mysql", dsn)
	case "postgres":
		// pgx stdlib 的 stdlib.OpenDB 兼容 keyword=value 形式的 DSN，
		// BuildDSN 生成的 "host=... search_path=..." 关键字串可直接使用
		// （未知关键字作为运行时参数下发服务端）。
		engine, err = xorm.NewEngine("pgx", dsn)
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
		engine, err = xorm.NewEngine("sqlite", dbPath)
	case "mssql":
		engine, err = xorm.NewEngine("mssql", dsn)
	default:
		return nil, fmt.Errorf("[GoFast] xormdriver driver: unsupported engine %q", cfg.Engine)
	}

	if err != nil {
		return nil, fmt.Errorf("[GoFast] xormdriver driver: connection failed: %w", err)
	}

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

	return &XormDriver{engine: engine, schema: cfg.Schema, tablePrefix: cfg.TablePrefix, tagIdentifier: tagIdentifier, log: log}, nil
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
func (d *XormDriver) AutoMigrate(models ...any) error {
	if err := d.validateOrmTags(models); err != nil {
		return err
	}
	if d.schema == "" {
		return d.engine.Sync2(models...)
	}
	_, err := d.engine.Transaction(func(s *xorm.Session) (any, error) {
		if _, err := s.Exec(fmt.Sprintf(`SET LOCAL search_path TO "%s"`, d.schema)); err != nil {
			return nil, fmt.Errorf("set search_path failed: %w", err)
		}
		return nil, s.Sync2(models...)
	})
	return err
}

// RawEngine 逃生口：允许高级用户直接获取 *xorm.Engine，使用 xorm 原生 API
// （不推荐常规使用，绕过框架的查询语义与缓存/钩子机制）。
func (d *XormDriver) RawEngine() *xorm.Engine { return d.engine }
