package xormdriver

// migrate.go 提供 gofast-xorm 原生迁移能力（需求文档 R1/R3/R4）：
//
//   - 迁移安全模式（R1）：WithMigrateSafe / NewMigrateDriver，PG 下强制
//     default_query_exec_mode=exec——pgx 不建 prepared statement，连接级语句缓存
//     无从谈起，从根源杜绝 ALTER COLUMN TYPE 后复用缓存计划触发
//     "cached plan must not change result type (0A000)" 的问题（gorm 路径历史 BUG：
//     见需求文档 §2，框架运行时连接 PrepareStmt/cache_statement 与迁移共用即踩坑）。
//   - 一站式迁移（R3）：Migrate(cfg, models...) 建专用短生命周期迁移驱动
//     （migrate-safe + orm tag 启动期校验 + Sync2），用完即关；
//     NewXormDriver 自建短生命周期驱动执行 AutoMigrate 同为受支持用法。
//   - 索引收敛（R4）：WithRebuildIndexes（PG）在 Sync2 前 drop 同名异构索引，
//     规避 Sync2 "按索引名判存在、同名即跳过、不校验定义" 的收敛陷阱
//     （与 gorm AutoMigrate 同款行为，见 docs/migration.md）。

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// driverOptions 驱动构造的可选行为（NewXormDriver 变参）。
type driverOptions struct {
	migrateSafe    bool // PG 迁移安全模式：default_query_exec_mode=exec
	rebuildIndexes bool // PG：Sync2 前 drop 同名异构索引（须配合 CheckIndexes 语义）
}

// DriverOption NewXormDriver / NewMigrateDriver 的功能选项。
type DriverOption func(*driverOptions)

// WithMigrateSafe 启用迁移安全模式：postgres 引擎下将连接 DSN 重写为
// default_query_exec_mode=exec（pgx 不再使用 prepared statement，连接级语句
// 缓存被根除，ALTER COLUMN TYPE 场景不可能再触发 0A000）。非 postgres 引擎
// 降级为 no-op（记 Info 日志）。仅影响本次构造的驱动，与运行时连接配置彻底隔离。
func WithMigrateSafe() DriverOption {
	return func(o *driverOptions) { o.migrateSafe = true }
}

// WithRebuildIndexes 启用索引收敛：AutoMigrate 执行 Sync2 之前，先按 xorm
// 期望定义（engine.TableInfo）对比库内实际索引，对同名异构索引执行 DROP INDEX，
// 使 Sync2 得以按模型定义重建。仅 postgres 引擎生效。
func WithRebuildIndexes() DriverOption {
	return func(o *driverOptions) { o.rebuildIndexes = true }
}

// migrateSafeDSN 将 PG DSN 重写为 default_query_exec_mode=exec（幂等）。
// 支持两种形态：
//   - URL 形态（postgres://...）：url.Parse 后设置 query 参数；
//   - keyword 形态（host=... port=...）：空格分隔 token，替换或追加关键字。
//
// 已是 exec 时原样返回；无法识别形态时原样返回（调用方引擎报错兜底）。
func migrateSafeDSN(dsn string) string {
	if strings.Contains(dsn, "default_query_exec_mode=exec") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("default_query_exec_mode", "exec")
		u.RawQuery = q.Encode()
		return u.String()
	}
	// keyword 形态：剔除既有 default_query_exec_mode token 后追加 exec。
	tokens := strings.Fields(dsn)
	kept := tokens[:0]
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "default_query_exec_mode=") {
			continue
		}
		kept = append(kept, tok)
	}
	return strings.Join(append(kept, "default_query_exec_mode=exec"), " ")
}

// NewMigrateDriver 按连接配置创建迁移专用驱动：migrate-safe 模式 +
// 完整驱动能力（orm tag 启动期校验、多 schema Sync2），供一次性迁移流程使用，
// 调用方负责 Close（或直接使用下方 Migrate 一站式函数）。
// schema-per-tenant 批量迁移的受支持用法：BuildTenantPgConfig(schema) →
// NewMigrateDriver → AutoMigrate → Close。
func NewMigrateDriver(cfg contracts.ConnectionConfig, log contracts.Log) (*XormDriver, error) {
	return NewXormDriver(cfg, log, WithMigrateSafe())
}

// Migrate 一站式迁移（R3）：按连接配置建专用迁移驱动（含迁移安全模式），
// 执行 orm tag 启动期校验（ormtag_check.go）与 Sync2（schema 非空时事务内
// SET LOCAL search_path），用完即关。任一步失败即关闭引擎并返回错误。
// 需要额外驱动行为（如 WithRebuildIndexes）时，改用 NewMigrateDriver 自建驱动：
//
//	drv, err := xormdriver.NewMigrateDriver(cfg, log)
//	if err != nil { ... }
//	defer drv.Close()
//	err = drv.AutoMigrate(models...)
func Migrate(cfg contracts.ConnectionConfig, models ...any) error {
	drv, err := NewMigrateDriver(cfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
	return drv.AutoMigrate(models...)
}

// sync2 执行 Sync2 + 列收敛（migrate_converge.go；Sync2 按基类型判等不改已有列
// 类型/长度/nullable，收敛 Pass 按 information_schema 逐项比对生成 ALTER）。
// schema 非空时包一层事务 + SET LOCAL search_path（与 xorm postgres 方言 DDL
// 以 URI().Schema 限定落点的机制配合）。
//
// 列注释中性化（BUG-3）：Sync2 前把已存在表的 TableInfo 列注释改写为库内实际值
// （消除 Sync2 comment 分支的假阳性 ALTER / 注释清空），Sync2 后恢复模型原注释，
// 由收敛 Pass 生成仅新增/更新的 COMMENT 语句。见 migrate_comment.go。
//
// 注意（实测确认）：schema 模式下 Sync2 在事务内运行，其内部元数据加载
// （loadTableInfo）会从连接池取第二条连接——迁移驱动连接池必须 ≥2，
// 单连接池会死锁（生产配置默认 100 无此问题，仅短生命周期自建驱动需留意）。
func (d *XormDriver) sync2(models ...any) error {
	engineName := d.engine.DriverName()
	restore, err := neutralizeComments(d.engine, engineName, d.schema, models)
	if err != nil {
		return err
	}
	defer restore() // 幂等：Sync2 成功路径已显式恢复，失败路径兜底

	run := func(s *xorm.Session) error {
		if d.schema != "" {
			if _, err := s.Exec(fmt.Sprintf(`SET LOCAL search_path TO "%s"`, d.schema)); err != nil {
				return fmt.Errorf("set search_path failed: %w", err)
			}
		}
		if err := s.Sync2(models...); err != nil {
			return err
		}
		restore() // 恢复模型原注释，供收敛 Pass 计算真实注释差异
		return d.execConverge(s, models)
	}
	if d.schema == "" {
		if err := d.engine.Sync2(models...); err != nil {
			return err
		}
		restore()
		sess := d.engine.NewSession()
		defer sess.Close()
		return d.execConverge(sess, models)
	}
	_, err = d.engine.Transaction(func(s *xorm.Session) (any, error) {
		return nil, run(s)
	})
	return err
}
