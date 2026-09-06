package xormdriver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/utils"

	"xorm.io/xorm"
)

// ── 关联 ─────────────────────────────────────────────────────────────

// joinOnRe 匹配 JOIN 串中第一处独立单词 "ON"（大小写不敏感，两侧为空白），
// 用于切分表名与连接条件；只认空白分隔的完整单词，避免命中表名/别名
// 中恰好以 on 结尾的标识符。
var joinOnRe = regexp.MustCompile(`(?i)\s+ON\s+`)

// hasWordPrefix 判断大写化后的 s 是否以词 word 开头且词后紧跟空白或串尾。
// 要求词边界，防止 "INNER_TABLE" 这类以关键字开头的标识符被误识别为 join 前缀。
func hasWordPrefix(s, word string) bool {
	if !strings.HasPrefix(s, word) {
		return false
	}
	return len(s) == len(word) || s[len(word)] == ' ' || s[len(word)] == '\t'
}

// parseJoinClause 解析 gorm 风格 JOIN 串
// "[LEFT|RIGHT|INNER|FULL|CROSS [OUTER]] JOIN <table> ON <cond>"（大小写不敏感）。
// join 前缀缺省按 INNER 处理；可选 OUTER 关键字并入 operator（xorm Join 会把
// operator 原样拼在 "JOIN" 之前，"LEFT OUTER" 即生成 "LEFT OUTER JOIN"）。
// 缺少 JOIN 关键字、缺少 ON 条件或任一部分为空均视为解析失败（返回 ok=false），
// 由调用方包装 ErrUnsupported——gorm 的关联名 Preload 式用法不在支持范围。
func parseJoinClause(query string) (op, table, cond string, ok bool) {
	rest := strings.TrimSpace(query)
	if rest == "" {
		return "", "", "", false
	}
	up := strings.ToUpper(rest)
	op = "INNER"
	for _, kw := range []string{"LEFT", "RIGHT", "INNER", "FULL", "CROSS"} {
		if hasWordPrefix(up, kw) {
			op = kw
			rest = strings.TrimSpace(rest[len(kw):])
			up = strings.ToUpper(rest)
			break
		}
	}
	if hasWordPrefix(up, "OUTER") {
		op += " OUTER"
		rest = strings.TrimSpace(rest[len("OUTER"):])
		up = strings.ToUpper(rest)
	}
	// 剥去 JOIN 关键字（"JOIN t ON ..." 无前缀时 op 已缺省 INNER）
	if !hasWordPrefix(up, "JOIN") {
		return "", "", "", false
	}
	rest = strings.TrimSpace(rest[len("JOIN"):])
	if rest == "" {
		return "", "", "", false
	}
	// 按第一处 " ON " 切分表名与条件（gorm 串的连接条件不可能早于表名出现）
	loc := joinOnRe.FindStringIndex(rest)
	if loc == nil {
		return "", "", "", false
	}
	table = strings.TrimSpace(rest[:loc[0]])
	cond = strings.TrimSpace(rest[loc[1]:])
	if table == "" || cond == "" {
		return "", "", "", false
	}
	return op, table, cond, true
}

// Joins 追加联表查询，兼容 gorm 风格 JOIN 串：
//
//	Joins("LEFT JOIN profiles ON profiles.user_id = users.id", args...)
//	Joins("JOIN orders ON orders.uid = users.id")
//
// 前缀关键字 LEFT/RIGHT/INNER/FULL/CROSS 大小写不敏感、缺省 INNER；解析结果
// 映射到 xorm 的 s.Join(operator, table, cond, args...)（operator 原样拼在
// JOIN 之前，合法取值如 "LEFT"/"INNER"/"LEFT OUTER"）。解析失败（缺 JOIN/
// 缺 ON 等）记入链上错误，终结时经 done 返回 ErrUnsupported 包装。
// 关联表名执行期经 schemaTable 读取 q.schema，保证 Schema() 后置调用时
// 关联表同样带上租户前缀。
func (q *XormQuery) Joins(query string, args ...any) contracts.Query {
	op, table, cond, ok := parseJoinClause(query)
	if !ok {
		return q.setErr(fmt.Errorf("%w: invalid join clause %q, expect \"[INNER|LEFT|RIGHT|FULL|CROSS] JOIN <table> ON <cond>\"", contracts.ErrUnsupported, query))
	}
	return q.addApplier("join:"+query, func(q *XormQuery, s *xorm.Session) error {
		s.Join(op, q.schemaTable(table), cond, args...)
		return nil
	})
}

// Preload 关联预加载实现在 query_preload.go（xorm 无关联元数据，
// 以 xorm:"-" 关联字段 + <父表名>_id 外键约定反射回填）。

// ── 分页 ─────────────────────────────────────────────────────────────

// Paginate 按页码/页大小设置分页（页码与页大小非法时归一为 1/20）。
// LIMIT/OFFSET 不在此处直接写入 session，而是记录到 limitN/startN，
// 统一由骨架 applyLimit 在执行期应用（规避 xorm Limit(0) 生成 "LIMIT 0"
// 及 offset-only 场景的方言合法性等边界问题）。
func (q *XormQuery) Paginate(page, size int) contracts.Query {
	page, size = utils.PageUtil.Normalize(page, size)
	return q.wrap(func(c *XormQuery) {
		c.startN = utils.PageUtil.Offset(page, size)
		c.limitN = size
	})
}

// ── 作用域 ───────────────────────────────────────────────────────────

// Scopes 依次应用作用域函数，用于复用查询片段。每个 fn 基于当前链的独立
// 拷贝执行（不可变语义下 fn 返回新实例），返回 *XormQuery 则作为下一轮的
// 基础链（fn 内追加的 appliers/字段自然并入），否则保持原链继续；fn 为 nil
// 时跳过，避免可变参传入 nil 导致 panic。
func (q *XormQuery) Scopes(funcs ...func(contracts.Query) contracts.Query) contracts.Query {
	merged := q.clone()
	for _, fn := range funcs {
		if fn == nil {
			continue
		}
		if res, ok := fn(merged.clone()).(*XormQuery); ok {
			merged = res
		}
	}
	return merged
}

// ── 上下文 ───────────────────────────────────────────────────────────

// WithContext 记录上下文，build 时经 s.Context 应用，查询的取消/超时随
// ctx 传播到底层连接。
func (q *XormQuery) WithContext(ctx context.Context) contracts.Query {
	return q.wrap(func(c *XormQuery) {
		c.ctx = ctx
	})
}

// ── 调试 ─────────────────────────────────────────────────────────────

// Debug 文档化 no-op：xorm 无 per-session 调试开关，SQL 日志由引擎级
// logger（newFastLogger，接框架 log 服务与慢日志阈值）统一配置，
// 无法也不必在单条查询链上切换。
func (q *XormQuery) Debug() contracts.Query {
	return q
}

// ── 查询缓存 ─────────────────────────────────────────────────────────

// Cache 对当前查询链开启结果缓存。读终结经 withCache 生效：cacheCfg 非 nil
// 且驱动已 EnableCaches（qc 非 nil）时命中直接反序列化返回，未命中回源后
// 回填；未启用缓存的连接上本方法无副作用。
func (q *XormQuery) Cache(opts ...contracts.CacheOption) contracts.Query {
	cfg := contracts.NewCacheConfig(opts...)
	return q.wrap(func(c *XormQuery) {
		c.cacheCfg = &cfg
	})
}

// ── 悲观锁 ───────────────────────────────────────────────────────────

// Lock 悲观锁。仅 LockForUpdate 可落地：执行期调 s.ForUpdate() 生成
// "FOR UPDATE"（xorm 侧 IsForUpdate 同时禁用其内建查询缓存，保证锁语义）。
// xorm 无 SHARE 锁支持，LockShareMode 等其余模式无从表达，文档化 no-op。
func (q *XormQuery) Lock(mode contracts.LockMode) contracts.Query {
	if mode != contracts.LockForUpdate {
		return q
	}
	return q.addApplier("", func(q *XormQuery, s *xorm.Session) error {
		s.ForUpdate()
		return nil
	})
}

// ── 软删除扩展 ───────────────────────────────────────────────────────

// Unscoped 取消软删除过滤，仅对 xorm `deleted` tag 标记的字段生效
// （xorm 在查询/更新/删除时自动追加该列的未删条件，Unscoped 置位后跳过）。
// 框架业务级软删除（database.SoftDelete 的 deleted_at 列）不依赖该 tag，
// 不受本方法影响。
func (q *XormQuery) Unscoped() contracts.Query {
	return q.addApplier("unscoped", func(q *XormQuery, s *xorm.Session) error {
		s.Unscoped()
		return nil
	})
}

// OnlyTrashed 仅查询已软删除的记录（deleted_at != 0）。
// 列名 "deleted_at" 与 database.SoftDelete.DeletedAt 字段绑定，
// 若自定义软删除列名需自行实现此逻辑。
// 与 gormdriver 的 Unscoped().Where(...) 不同，这里无需叠加 Unscoped：
// 业务级 deleted_at 列并非 xorm `deleted` tag，不会被 xorm 自动过滤。
func (q *XormQuery) OnlyTrashed() contracts.Query {
	return q.addApplier("onlytrashed", func(q *XormQuery, s *xorm.Session) error {
		return applyCondToSession(s, condWhere, "deleted_at != 0", nil)
	})
}

// Restore 恢复已软删除记录（deleted_at 置 0）。要求链上已显式 Table/Model：
// build(nil) 无 dest 可作表名兜底，裸链执行时 xorm 无法定位表而报错。
// 无 Where 条件时 xorm 不拦截全表更新（与 gorm 的 ErrMissingWhereClause
// 不同），恢复范围由调用方保证。写终结成功后失效查询缓存。
func (q *XormQuery) Restore() error {
	s, err := q.build(nil)
	if err != nil {
		return q.done(err)
	}
	if _, err := s.Update(map[string]any{"deleted_at": 0}); err != nil {
		return q.done(err)
	}
	q.invalidateCache()
	return nil
}

// ForceDelete 物理删除记录（xorm Delete 默认即物理删除，等价
// gorm Unscoped().Delete 语义），conds 复用终结方法变参条件。
// 与 gormdriver 一致不触发 Before/AfterDelete 钩子——钩子面向业务级软删除
// 的 Delete 路径。写终结成功后失效查询缓存。
func (q *XormQuery) ForceDelete(value any, conds ...any) error {
	s, err := q.build(value)
	if err != nil {
		return q.done(err)
	}
	if err := applyConds(s, conds); err != nil {
		return q.done(err)
	}
	if _, err := s.Delete(value); err != nil {
		return q.done(err)
	}
	q.invalidateCache()
	return nil
}

// ── Schema ───────────────────────────────────────────────────────────

// Schema 在当前查询链上设置动态 schema（主要用于 PostgreSQL 多租户）。
// 后续 Table()/Model() 的表名与未显式指定表名时的 dest 兜底均按该前缀拼接
// （"schema.table"），连续调用以最后一次为准；空名视为不切换直接返回。
func (q *XormQuery) Schema(name string) contracts.Query {
	if name == "" {
		return q
	}
	return q.wrap(func(c *XormQuery) {
		c.schema = name
	})
}

// GetSchema 返回当前查询上下文的 schema 名称，供业务在原生 SQL 中拼接
// schema 限定的表名；无 schema 上下文时返回空字符串。
func (q *XormQuery) GetSchema() string {
	return q.schema
}

// ── 原生 SQL ─────────────────────────────────────────────────────────

// Raw 记录原生 SQL 与参数，执行期经 applier 调 s.SQL 应用到 session，
// 后续 Find/Scan/Count 等终结方法在该原生 SQL 上执行。applier 内读取
// 执行期 q 的字段：链上多次 Raw 以后一次为准。原生 SQL 中的表名不自动
// 加 schema 前缀，多租户场景请配合 GetSchema 自行拼接。
func (q *XormQuery) Raw(sql string, values ...any) contracts.Query {
	return q.wrap(func(c *XormQuery) {
		c.rawSQL = sql
		c.rawArgs = values
	}).addApplier("", func(q *XormQuery, s *xorm.Session) error {
		s.SQL(q.rawSQL, q.rawArgs...)
		return nil
	})
}
