// Package xormdriver 基于 xorm.io/xorm 实现框架数据库抽象层
// （contracts.Driver / contracts.Query / contracts.QueryCacher）。
//
// 接入方式（可选驱动，需显式加入应用 providers）：
//
//	import xormdriver "github.com/zhoudm1743/gofast-xorm"
//	app.SetProviders(append(providers, &xormdriver.ServiceProvider{}))
//	// 配置：database.connections.<name>.driver = "xorm"
//
// 语义说明：
//   - Schema()/Tenant() 多租户：显式 Table()/Model() 永远优先，仅当未显式指定
//     表名时才按 dest 推导表名并加 "schema." 前缀（与 gormdriver applySchema
//     修复后语义一致，投影结构体/分表不会被 dest 推导覆盖）。
//   - Query().Cache() 查询结果缓存：需先通过 dbManager.UseQueryCache 启用
//     （驱动实现 contracts.QueryCacher）；JSON 序列化语义；写操作自动失效。
//   - TxOption（隔离级别）：xorm 的 session.Begin() 无选项参数，当前降级为忽略。
//   - 统一模型标签（orm tag）：连接配置 tag_identifier 切换引擎读取的 tag 键
//     （默认 "xorm"，置 "orm" 启用统一标签，§8.1/§8.2）；AutoMigrate 启动期
//     校验禁用 token/未知裸 token（§8.4）并对迁移期陷阱/ ext 降级告警（§10.3，
//     见 ormtag_check.go）。
//   - Preload()：切换到框架级共享 Preload 引擎（database/preload，契约 §9.2），
//     驱动侧仅保留 MetaAdapter/子查询构造薄包装；关联解析覆盖 rel tag → gorm
//     tag → 约定 → 方向判定 → many2many → polymorphic 全链（query_preload.go）。
//     Debug()/Lock(LockShareMode)：xorm 无对应能力，文档化 no-op。
//   - Model(&bean).Updates/Update（X-09）：bean 主键非零时自动并入主键等值
//     条件（与 gorm 驱动一致）；主键全零/无主键不附加条件，链上无 Where 即
//     为全表更新（批量写请显式 Where）。
package xormdriver

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// ── 驱动类型 ─────────────────────────────────────────────────────────

// XormDriver 实现 contracts.Driver（方法见 driver.go / cacher.go）。
type XormDriver struct {
	engine        *xorm.Engine
	schema        string        // 连接级默认 schema（cfg.Schema）
	tablePrefix   string        // 表名前缀（cfg.TablePrefix）
	tagIdentifier string        // 引擎读取的 struct tag 键名（cfg.TagIdentifier，空值归一为 "xorm"）
	qc            *queryCache   // 查询缓存；EnableCaches 后非 nil（见 cacher.go）
	log           contracts.Log // 框架日志器（AutoMigrate 启动期校验告警用；构造时 nil 已降级 discardLog）
}

// ── 查询构建器 ───────────────────────────────────────────────────────

// applierFn 终结方法执行前应用到 *xorm.Session 的链式条件。
// 接收当前查询状态（在执行期读取 schema 等最新值，保证 Schema() 后置调用生效）
// 与目标 session；返回错误时终止执行。
type applierFn func(q *XormQuery, s *xorm.Session) error

// condSpec 链上条件的规范化记录（X-08）：与 appliers 并行保留一份 {组合方式,
// 条件, 参数}，专供表达式 UPDATE 组装 WHERE（updateWithExpr → buildCond）。
// 普通查询路径不读它——执行期仍走 appliers，行为完全不变。
type condSpec struct {
	mode  condMode
	query any
	args  []any
}

// XormQuery 实现 contracts.Query。
// 链式方法不修改自身，而是返回追加了 applier 的新实例（不可变语义，与 gormdriver
// 一致）；终结方法执行时按序将 applier 应用到新建（或事务内复用）的 *xorm.Session。
type XormQuery struct {
	engine *xorm.Engine
	tx     *xorm.Session   // 事务会话（Transaction/Begin 内非 nil）；xorm session 语句自动重置，可跨终结操作复用
	schema string          // 租户/动态 schema 前缀
	ctx    context.Context // WithContext/Query(ctx) 记录的上下文，build 时应用到 session

	explicitTable bool                   // Table()/Model() 已显式指定表名
	modelValue    any                    // Model() 记录的 bean
	tableName     string                 // Table()/Model() 记录的原始表名（裸名，执行期经 schemaTable 解析 schema 前缀）
	condSpecs     []condSpec             // 链上条件副本（Where/OrWhere/Not 追加），仅表达式 UPDATE 组装 WHERE 时消费
	limitN        int                    // LIMIT，<=0 表示未设置
	startN        int                    // OFFSET，<=0 表示未设置
	orderStr      string                 // ORDER BY 子句，build 末段统一应用（Count 可剥离）
	err           error                  // 链上首个错误（gorm AddError 语义），终结时优先返回
	cacheCfg      *contracts.CacheConfig // Cache() 设置；nil 表示本查询不走缓存
	qc            *queryCache            // 驱动级缓存（未 EnableCaches 时为 nil）
	keyParts      []string               // 缓存键规范化片段

	rawSQL  string // Raw() 记录的原生 SQL（Row/Rows 原生路径使用）
	rawArgs []any

	preloads []preloadSpec // Preload() 声明的关联预加载（query_preload.go），终结成功后回填

	appliers []applierFn
}

// ── 不可变包装 ───────────────────────────────────────────────────────

// wrap 拷贝当前查询（切片深拷贝，写时复制）并应用变更，返回新实例。
func (q *XormQuery) wrap(mutate func(*XormQuery)) *XormQuery {
	c := *q
	c.appliers = append(make([]applierFn, 0, len(q.appliers)+1), q.appliers...)
	c.keyParts = append(make([]string, 0, len(q.keyParts)+1), q.keyParts...)
	c.preloads = append(make([]preloadSpec, 0, len(q.preloads)+1), q.preloads...)
	c.condSpecs = append(make([]condSpec, 0, len(q.condSpecs)+1), q.condSpecs...)
	if mutate != nil {
		mutate(&c)
	}
	return &c
}

// addApplier 追加执行期 applier；key 非空时同时追加缓存键规范化片段。
func (q *XormQuery) addApplier(key string, fn applierFn) *XormQuery {
	return q.wrap(func(c *XormQuery) {
		c.appliers = append(c.appliers, fn)
		if key != "" {
			c.keyParts = append(c.keyParts, key)
		}
	})
}

// setErr 记录链上首个错误（保留首个，后续忽略）。
func (q *XormQuery) setErr(err error) *XormQuery {
	return q.wrap(func(c *XormQuery) {
		if c.err == nil {
			c.err = err
		}
	})
}

// clone 返回完全独立的拷贝（供 Scopes/Transaction/FindInBatches 复用）。
func (q *XormQuery) clone() *XormQuery {
	return q.wrap(nil)
}

// ── 执行期构建 ───────────────────────────────────────────────────────

// build 构建（或复用事务）session，按序应用上下文、表名兜底、分页与链式条件。
// dest 用于表名兜底推导；无 dest 的场景传 nil。
func (q *XormQuery) build(dest any) (*xorm.Session, error) {
	return q.buildOpts(dest, false)
}

// buildOpts 同 build；skipOrder 用于 Count 剥离 ORDER BY——聚合列不在排序列
// 集合内时 PG 严格模式报 42803，且 gorm Count 本就丢弃排序（X-06）。
func (q *XormQuery) buildOpts(dest any, skipOrder bool) (*xorm.Session, error) {
	if q.err != nil {
		return nil, q.err
	}
	var s *xorm.Session
	if q.tx != nil {
		s = q.tx
	} else {
		s = q.engine.NewSession()
	}
	if q.ctx != nil {
		s = s.Context(q.ctx)
	}
	// 表名兜底：未显式 Table()/Model() 且设置了 schema 时，按 dest 推导表名并加前缀。
	// 显式表名永远优先，不按 dest 推导覆盖。
	if !q.explicitTable && q.schema != "" && dest != nil {
		if name, err := tableInfoName(q.engine, dest); err == nil && name != "" && !strings.Contains(name, ".") {
			s.Table(q.schemaTable(name))
		}
	}
	q.applyLimit(s)
	for _, ap := range q.appliers {
		if err := ap(q, s); err != nil {
			return nil, err
		}
	}
	if !skipOrder && q.orderStr != "" {
		s.OrderBy(q.orderStr)
	}
	return s, nil
}

// applyLimit 统一应用 LIMIT/OFFSET。
// xorm Statement.Limit 会无条件写入 LimitN 指针（Limit(0) 生成 "LIMIT 0"），
// 因此在包装层记录、执行时一次性应用；offset-only 场景用 LIMIT 上限兜底，
// 保证 MySQL 等方言 SQL 合法。
func (q *XormQuery) applyLimit(s *xorm.Session) {
	switch {
	case q.limitN > 0 && q.startN > 0:
		s.Limit(q.limitN, q.startN)
	case q.limitN > 0:
		s.Limit(q.limitN)
	case q.startN > 0:
		s.Limit(math.MaxInt32, q.startN)
	}
}

// done 终结方法统一错误出口：链上错误优先，其余经 wrapError 映射为框架 Sentinel Error。
func (q *XormQuery) done(err error) error {
	if q.err != nil {
		return q.err
	}
	return wrapError(err)
}

// ── schema / 表名解析 ────────────────────────────────────────────────

// schemaTable schema 非空且 name 不含 "." 时自动加上 "schema." 前缀。
// xorm Quoter 对点分表名逐段引用（"tenant1.users" → "tenant1"."users"）。
func (q *XormQuery) schemaTable(name string) string {
	if q.schema != "" && !strings.Contains(name, ".") {
		return q.schema + "." + name
	}
	return name
}

// tableBeanOf 将 value（struct/指针/切片/指针切片）归一为可解析的 struct 指针 bean。
func tableBeanOf(value any) any {
	if value == nil {
		return nil
	}
	t := reflect.TypeOf(value)
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return reflect.New(t).Interface()
}

// tableInfoName 解析 value 对应表名（已应用 TablePrefix mapper，尊重 TableName() 接口）。
func tableInfoName(e *xorm.Engine, value any) (string, error) {
	bean := tableBeanOf(value)
	if bean == nil {
		return "", nil
	}
	t, err := e.TableInfo(bean)
	if err != nil {
		return "", err
	}
	return t.Name, nil
}

// pkColumnsOf 解析 value 对应表的主键列名列表（无主键或解析失败返回 nil）。
func pkColumnsOf(e *xorm.Engine, value any) []string {
	bean := tableBeanOf(value)
	if bean == nil {
		return nil
	}
	t, err := e.TableInfo(bean)
	if err != nil {
		return nil
	}
	return t.PrimaryKeys
}

// ── 条件应用（终结方法变参 conds 复用）────────────────────────────────

// condMode 条件组合方式。
type condMode int

const (
	condWhere condMode = iota // AND 组合
	condOr                    // OR 组合
	condNot                   // NOT 取反
)

// applyConds 将终结方法变参 conds 应用到 session（gorm 语义：conds[0] 为条件，
// 其余为参数，等价于一次 Where）。
func applyConds(s *xorm.Session, conds []any) error {
	if len(conds) == 0 {
		return nil
	}
	return applyCondToSession(s, condWhere, conds[0], conds[1:])
}

// ── 查询缓存挂点 ─────────────────────────────────────────────────────

// withCache 读终结方法的缓存包装：命中时反序列化进 dest 跳过 DB；未命中执行后
// 序列化回填。仅当 Cache() 显式开启（cacheCfg 非 nil）且驱动已启用缓存（qc 非 nil）
// 时生效；dest 不可 JSON 序列化时静默跳过缓存（不影响查询结果）。
func (q *XormQuery) withCache(dest any, exec func() error) error {
	if q.cacheCfg == nil || q.qc == nil || dest == nil {
		return exec()
	}
	key := buildCacheKey(q.keyParts, dest)
	if b, ok := q.qc.get(q.cacheCfg.Store, key); ok {
		if err := json.Unmarshal(b, dest); err == nil {
			return nil
		}
		// 反序列化失败（如模型结构变更）→ 回源查询
	}
	if err := exec(); err != nil {
		return err
	}
	if b, err := json.Marshal(dest); err == nil {
		q.qc.put(q.cacheCfg.Store, key, b, q.cacheCfg.TTL, q.cacheCfg.Tags)
	}
	return nil
}

// invalidateCache 写终结方法成功后调用，按统一 tag 失效全部查询缓存。
func (q *XormQuery) invalidateCache() {
	if q.qc != nil {
		q.qc.invalidateAll()
	}
}
