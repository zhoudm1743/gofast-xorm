package xormdriver

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/builder"
	"xorm.io/xorm"
)

// ── 链式构建器 ───────────────────────────────────────────────────────
//
// 链式方法一律经 wrap/addApplier/setErr 生成新实例（不可变语义），条件在此刻不落
// session，而是记录为 applier，由终结方法经 build() 在执行期按序应用。
// applier 闭包通过参数 q 读取 schema 等执行期最新状态（保证 Schema() 后置调用
// 生效），仅捕获链上即得的常量（SQL 片段、参数、解析出的表名）；缓存键片段
// （keyPart）则在链上即席生成确定性字符串。

// validCondType 链上条件类型预校验：string / builder.Cond / map[string]any 合法。
// 在链上即失败（setErr）而非拖到执行期，让非法条件按 gorm AddError 语义在
// 终结方法返回的首个错误中被拦截。
func validCondType(query any) bool {
	switch query.(type) {
	case string, builder.Cond, map[string]any:
		return true
	default:
		return false
	}
}

// condKey 生成条件的缓存键片段：条件的确定性字符串表示，参数逐项以 \x1f 分隔。
// fmt.Sprint 对 map 按 key 排序输出，同一条件多次构造得到的片段稳定一致。
func condKey(prefix string, query any, args []any) string {
	key := prefix + fmt.Sprint(query)
	for _, a := range args {
		key += "\x1f" + fmt.Sprint(a)
	}
	return key
}

// Table 显式指定表名。schema 前缀延迟到执行期经 schemaTable(q.schema) 拼接，
// 使 Schema() 无论先于或后于 Table() 调用都能生效。
func (q *XormQuery) Table(name string) contracts.Query {
	// tableName 记录裸名：表达式 UPDATE（updateWithExpr）组装时经 q.schemaTable
	// 执行期解析 schema 前缀，Schema() 后置调用同样生效。
	nq := q.wrap(func(c *XormQuery) {
		c.explicitTable = true
		c.tableName = name
	})
	return nq.addApplier("table:"+name, func(q *XormQuery, s *xorm.Session) error {
		s.Table(q.schemaTable(name))
		return nil
	})
}

// Model 显式指定模型。链上即解析裸表名（已应用 TablePrefix mapper、尊重
// TableName() 接口），解析失败记录首个错误；explicitTable 置位后，build()
// 不再按 dest 推导表名兜底，投影结构体等 dest 不会覆盖显式模型。
func (q *XormQuery) Model(value any) contracts.Query {
	name, err := tableInfoName(q.engine, value)
	if err != nil {
		return q.setErr(fmt.Errorf("xorm: Model 解析表名失败: %w", err))
	}
	if name == "" {
		// 非 struct（或切片元素非 struct）解析不出表名，显式报错而不是
		// 留到执行期静默落到空表名查错表。
		return q.setErr(fmt.Errorf("%w: Model 入参无法解析出表名（收到 %T）", contracts.ErrUnsupported, value))
	}
	nq := q.wrap(func(c *XormQuery) {
		c.explicitTable = true
		c.modelValue = value
		c.tableName = name // 链上解析出的裸表名，供表达式 UPDATE 组装复用
	})
	return nq.addApplier("model:"+name, func(q *XormQuery, s *xorm.Session) error {
		s.Table(q.schemaTable(name))
		return nil
	})
}

// Select 指定投影列。xorm Session.Select 仅接受 string（无参数占位符语义），
// 非 string 在链上报错。
// Select("*") 对齐 gormdriver 语义：回归驱动默认的全列查询，此处不做任何
// session 写入（no-op）——显式 "*" 会破坏 Omit 等依赖"未显式投影"的路径。
func (q *XormQuery) Select(query any, args ...any) contracts.Query {
	str, ok := query.(string)
	if !ok {
		return q.setErr(fmt.Errorf("%w: Select 仅支持 string，收到 %T", contracts.ErrUnsupported, query))
	}
	if len(args) == 0 && strings.TrimSpace(str) == "*" {
		return q.addApplier("select:*", func(q *XormQuery, s *xorm.Session) error {
			return nil
		})
	}
	// gorm 语义 Select("id", "name") 为多列投影；xorm Select 仅收单个字符串，
	// 拼接为 "id, name"（xorm 原样写入 SELECT 列表，不做列名解析）。
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, str)
	for _, a := range args {
		s2, ok := a.(string)
		if !ok {
			return q.setErr(fmt.Errorf("%w: Select 变参仅支持 string，收到 %T", contracts.ErrUnsupported, a))
		}
		parts = append(parts, s2)
	}
	joined := strings.Join(parts, ", ")
	return q.addApplier(condKey("select:", joined, nil), func(q *XormQuery, s *xorm.Session) error {
		s.Select(joined)
		return nil
	})
}

// Omit 排除列（查询投影与更新写入均跳过这些列）。
func (q *XormQuery) Omit(columns ...string) contracts.Query {
	return q.addApplier("omit:"+strings.Join(columns, ","), func(q *XormQuery, s *xorm.Session) error {
		s.Omit(columns...)
		return nil
	})
}

// Where 追加 AND 条件。类型校验在链上完成，applier 仅在执行期落到 session。
// 同时把条件追加到 condSpecs（X-08）：普通查询路径不消费它，仅表达式 UPDATE
// （updateWithExpr）用它组装 WHERE，两路语义一致。
func (q *XormQuery) Where(query any, args ...any) contracts.Query {
	if !validCondType(query) {
		return q.setErr(fmt.Errorf("%w: Where 仅支持 string/builder.Cond/map[string]any，收到 %T", contracts.ErrUnsupported, query))
	}
	return q.addCondSpec(condKey("where:", query, args), condWhere, query, args)
}

// OrWhere 追加 OR 条件。
func (q *XormQuery) OrWhere(query any, args ...any) contracts.Query {
	if !validCondType(query) {
		return q.setErr(fmt.Errorf("%w: OrWhere 仅支持 string/builder.Cond/map[string]any，收到 %T", contracts.ErrUnsupported, query))
	}
	return q.addCondSpec(condKey("or:", query, args), condOr, query, args)
}

// Not 追加取反条件。
func (q *XormQuery) Not(query any, args ...any) contracts.Query {
	if !validCondType(query) {
		return q.setErr(fmt.Errorf("%w: Not 仅支持 string/builder.Cond/map[string]any，收到 %T", contracts.ErrUnsupported, query))
	}
	return q.addCondSpec(condKey("not:", query, args), condNot, query, args)
}

// addCondSpec Where/OrWhere/Not 的公共入口：追加 condSpec 副本后挂上原有的
// 执行期 applier（普通查询路径行为完全不变）。addApplier 内部再次 wrap，
// condSpecs 随写时复制一并深拷贝，不可变语义不破坏。
func (q *XormQuery) addCondSpec(key string, mode condMode, query any, args []any) contracts.Query {
	nq := q.wrap(func(c *XormQuery) {
		c.condSpecs = append(c.condSpecs, condSpec{mode: mode, query: query, args: args})
	})
	return nq.addApplier(key, func(q *XormQuery, s *xorm.Session) error {
		return applyCondToSession(s, mode, query, args)
	})
}

// Order 排序。xorm OrderBy 虽接受 any，这里收窄为 string（框架 Query 接口语义），
// 其他类型在链上报错，避免隐式格式化产生意外排序串。
func (q *XormQuery) Order(value any) contracts.Query {
	order, ok := value.(string)
	if !ok {
		return q.setErr(fmt.Errorf("%w: Order 仅支持 string，收到 %T", contracts.ErrUnsupported, value))
	}
	// 不落 applier 而记录到 orderStr：由 build 末段统一应用，Count 才能整体
	// 剥离 ORDER BY（对齐 gorm Count 语义，见 X-06）。
	return q.wrap(func(c *XormQuery) {
		c.orderStr = order
		c.keyParts = append(c.keyParts, "order:"+order)
	})
}

// Limit 记录 LIMIT。不在链上写 session：分页统一由骨架 applyLimit 在执行期一次性
// 应用——xorm Statement.Limit 会无条件写入 LimitN（Limit(0) 生成 "LIMIT 0"），
// 且 offset-only 场景需要 LIMIT 上限兜底，分散设置会破坏该约定。
func (q *XormQuery) Limit(limit int) contracts.Query {
	return q.wrap(func(c *XormQuery) {
		c.limitN = limit
		c.keyParts = append(c.keyParts, fmt.Sprintf("limit:%d", limit))
	})
}

// Offset 记录 OFFSET（同 Limit，执行期由 applyLimit 统一应用）。
func (q *XormQuery) Offset(offset int) contracts.Query {
	return q.wrap(func(c *XormQuery) {
		c.startN = offset
		c.keyParts = append(c.keyParts, fmt.Sprintf("offset:%d", offset))
	})
}

// Group 指定分组列。
func (q *XormQuery) Group(name string) contracts.Query {
	return q.addApplier("group:"+name, func(q *XormQuery, s *xorm.Session) error {
		s.GroupBy(name)
		return nil
	})
}

// Having 指定分组后条件。xorm Session.Having 精确签名为 Having(conditions string)，
// 仅支持不带参数占位符的 SQL 字符串：非 string 或携带参数在链上报错，而不是
// 静默丢弃参数生成错误 SQL。
func (q *XormQuery) Having(query any, args ...any) contracts.Query {
	str, ok := query.(string)
	if !ok {
		return q.setErr(fmt.Errorf("%w: Having 仅支持 string，收到 %T", contracts.ErrUnsupported, query))
	}
	if len(args) > 0 {
		// Session.Having(conditions string) 无变参，无法绑定占位符参数；
		// 自行拼接参数会引入 SQL 注入风险，故显式拒绝。
		return q.setErr(fmt.Errorf("%w: Having 不支持参数占位符（xorm Session.Having 仅接受 string），请将条件内联到 SQL 字符串", contracts.ErrUnsupported))
	}
	return q.addApplier("having:"+str, func(q *XormQuery, s *xorm.Session) error {
		s.Having(str)
		return nil
	})
}

// Distinct 指定去重列。xorm Distinct 接受变参列名，非 string 参数在链上报错。
func (q *XormQuery) Distinct(args ...any) contracts.Query {
	cols := make([]string, 0, len(args))
	for _, a := range args {
		col, ok := a.(string)
		if !ok {
			return q.setErr(fmt.Errorf("%w: Distinct 仅支持 string 列名，收到 %T", contracts.ErrUnsupported, a))
		}
		cols = append(cols, col)
	}
	return q.addApplier("distinct:"+strings.Join(cols, ","), func(q *XormQuery, s *xorm.Session) error {
		s.Distinct(cols...)
		return nil
	})
}

// ── 条件应用（骨架 applyConds 与链式 Where/OrWhere/Not 共用）─────────

// applyCondToSession 将单个条件按组合方式应用到 session（契约冻结签名）。
// query 支持：
//   - string：SQL 占位符条件，参数原样透传；
//   - builder.Cond：直接交给 xorm 组合；
//   - map[string]any：等值条件，转 builder.Eq 后按 Cond 路径处理；
//   - 其他类型：返回 contracts.ErrUnsupported 包装错误。
//
// condNot：string 直接以 "NOT (...)" 包裹（参数透传）；builder.Cond 先经
// builder.ToSQL 展开为 SQL+参数再取反——xorm 无 Cond 级取反 API，展开是唯一
// 能保证占位符与参数对齐的取反方式。
func applyCondToSession(s *xorm.Session, mode condMode, query any, args []any) error {
	switch cond := query.(type) {
	case string:
		// gorm 语义兼容：IN ?/IN (?) 的切片参数展开为 IN (?,?,...)（X-05）。
		// gormdriver 下 Where("id IN ?", ids) 自动展开；xorm 原样绑定单参数会在
		// PG 生成 "IN $1" 语法错误或 pgx 无法编码切片，必须在驱动层对齐展开。
		cond, args = expandSlicePlaceholders(cond, args)
		switch mode {
		case condOr:
			s.Or(cond, args...)
		case condNot:
			s.Where("NOT ("+cond+")", args...)
		default:
			s.Where(cond, args...)
		}
	case builder.Cond:
		if mode == condNot {
			sqlStr, condArgs, err := builder.ToSQL(cond)
			if err != nil {
				return err
			}
			s.Where("NOT ("+sqlStr+")", condArgs...)
			return nil
		}
		if mode == condOr {
			s.Or(cond)
			return nil
		}
		s.Where(cond)
	case map[string]any:
		// builder.Eq 即 map[string]any 的命名类型，转成 Cond 复用上方分支，
		// 使 OR/NOT 组合行为与显式传入 builder.Cond 完全一致。
		return applyCondToSession(s, mode, builder.Eq(cond), args)
	default:
		return fmt.Errorf("%w: 不支持的条件类型 %T", contracts.ErrUnsupported, query)
	}
	return nil
}

// ── IN 切片参数展开（gorm 语义对齐）──────────────────────────────────

// inPlaceholderRe 匹配 SQL 条件串中的 IN 占位符两种写法：`IN ?` 与 `IN (?)`
// （大小写不敏感；NOT IN / not in 同样命中——词边界保证不误伤含 in 的标识符）。
// placeholderRe 匹配全部 ? 占位符（IN 上下文判定经占位符前缀文本进行，
// 保证前面还有普通 ? 时参数索引不错位）。
var placeholderRe = regexp.MustCompile(`\?`)

// expandSlicePlaceholders 将条件串中绑定到切片参数的 IN 占位符展开为
// IN (?,?,...) 并平铺参数，对齐 gormdriver 对 Where("id IN ?", ids) 的语义。
// 规则：
//   - 遍历全部 ? 占位符按序消费参数（普通 ? 消费 1 个参数），
//     避免 "type_code = ? AND value IN ?" 场景下 IN 错位到前一个参数
//     （v0.8.1 首版缺陷：只扫描 IN 占位符导致参数索引错位）；
//   - IN 占位符绑定切片参数（不含 []byte——它是单个二进制值）→ 按元素展开；
//   - 空切片 → IN (NULL)（恒假，与 gorm 生成的 SQL 一致），不产生绑定参数；
//   - 非切片参数或参数不足时占位符原样保留。
func expandSlicePlaceholders(cond string, args []any) (string, []any) {
	if len(args) == 0 {
		return cond, args
	}
	matches := placeholderRe.FindAllStringIndex(cond, -1)
	if len(matches) == 0 {
		return cond, args
	}
	var out strings.Builder
	out.Grow(len(cond) + 16)
	flat := make([]any, 0, len(args))
	argIdx := 0
	last := 0
	for _, loc := range matches {
		out.WriteString(cond[last:loc[0]])
		// IN 上下文判定：占位符之前的文本去掉尾部空白后，括号形态再剥 "("，
		// 以 "in" 结尾即命中（IN ? 与 IN (?) 两种形态，NOT IN 同样生效）
		trimmed := strings.TrimRight(cond[last:loc[0]], " \t\n\r")
		parenForm := strings.HasSuffix(trimmed, "(")
		// cutset 同时含 "(" 与空白：剥掉括号后残留的尾随空格也要去掉
		core := strings.TrimRight(trimmed, "( \t\n\r")
		isInCtx := len(core) >= 2 && strings.EqualFold(core[len(core)-2:], "in")

		// consumed 而非 nil 判定：参数本身为 nil（绑定为 NULL）时不得丢弃
		arg := any(nil)
		consumed := false
		if argIdx < len(args) {
			arg = args[argIdx]
			argIdx++
			consumed = true
		}

		if isInCtx && arg != nil {
			rv := reflect.ValueOf(arg)
			if rv.Kind() != reflect.String &&
				(rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) &&
				rv.Type().Elem().Kind() != reflect.Uint8 {
				n := rv.Len()
				switch {
				case n == 0:
					if parenForm {
						out.WriteString("NULL")
					} else {
						out.WriteString("(NULL)")
					}
				default:
					if !parenForm {
						out.WriteString("(")
					}
					for i := 0; i < n; i++ {
						if i > 0 {
							out.WriteString(",")
						}
						out.WriteString("?")
						flat = append(flat, rv.Index(i).Interface())
					}
					if !parenForm {
						out.WriteString(")")
					}
				}
				last = loc[1]
				continue
			}
		}
		// 非 IN 上下文 / 非切片 / 参数不足：占位符原样保留
		out.WriteString("?")
		if consumed {
			flat = append(flat, arg)
		}
		last = loc[1]
	}
	out.WriteString(cond[last:])
	flat = append(flat, args[argIdx:]...)
	return out.String(), flat
}
