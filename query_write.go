package xormdriver

import (
	"fmt"
	"reflect"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/builder"
	"xorm.io/xorm"
	"xorm.io/xorm/schemas"
)

// ── 写终结 ───────────────────────────────────────────────────────────
//
// 钩子调用点与 gorm 驱动（gormdriver query.go）逐一对应：
//   - Create/CreateInBatches/Save(插入分支)：前 invokeBeforeCreate（框架层
//     IDAutoGenerator + BeforeCreator，替代 gorm BeforeCreate 回调）、后
//     invokeAfterCreate（对应 gorm AfterCreate）。
//   - Save(更新分支)：前 invokeBeforeUpdate、后 invokeAfterUpdate。
//   - Delete：前 invokeBeforeDelete、后 invokeAfterDelete。
//   - Update/Updates：不触发模型钩子，与 gorm 驱动一致。
//
// 错误出口统一经 q.done()（契约约定 2）：链上错误优先，其余 wrapError 归一；
// 钩子产生的业务错误不匹配任何 Sentinel，wrapError 原样透传，语义不丢失。

// createCore Create 的公共内核：钩子 → Insert → 失效缓存 → 钩子。
// 返回 Insert 的受影响行数供 CreateResult 回填；Create 丢弃行数只取错误。
// 注意 gorm 驱动的 Save 空主键分支同样先落 gorm BeforeCreate 生成 ID，
// saveOne 的插入路径复用本函数即与该语义对齐。
func (q *XormQuery) createCore(value any) (int64, error) {
	if err := invokeBeforeCreate(q, value); err != nil {
		return 0, q.done(err)
	}
	s, err := q.build(value)
	if err != nil {
		return 0, q.done(err)
	}
	n, err := s.Insert(value)
	if err != nil {
		return 0, q.done(err)
	}
	// 写操作成功后统一失效查询缓存（契约约定 5）
	q.invalidateCache()
	return n, q.done(invokeAfterCreate(q, value))
}

// Create 插入单条记录。
func (q *XormQuery) Create(value any) error {
	_, err := q.createCore(value)
	return err
}

// CreateInBatches 分批插入切片。
// gorm 的 CreateInBatches 对非切片值回落为单条 Create（default 分支），
// 这里对齐：非切片直接委托 Create，钩子因此只在单条路径触发一次。
// 仅切片走分批（数组成员不可寻址时无法按元素触发钩子，同样回落 Create）。
func (q *XormQuery) CreateInBatches(value any, batchSize int) error {
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			// nil 指针无可插入元素：no-op（与 walkValues 跳过 nil 的口径一致）
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice && !(rv.Kind() == reflect.Array && rv.CanAddr()) {
		return q.Create(value)
	}
	// batchSize 非法时兜底为不分批（gorm 对非正 batch 行为无保证）
	if batchSize <= 0 {
		batchSize = rv.Len()
	}
	// 钩子对整个切片前置触发一次（walkValues 会逐元素展开），
	// 与 gorm 每批 callbacks 触发的净效果一致：每个元素各回调一次。
	if err := invokeBeforeCreate(q, value); err != nil {
		return q.done(err)
	}
	// 每块重新 build：链上条件/表名兜底/事务会话全部复用；
	// xorm session 每条语句执行后自动重置，同一 session 可连续 Insert。
	for start := 0; start < rv.Len(); start += batchSize {
		end := start + batchSize
		if end > rv.Len() {
			end = rv.Len()
		}
		chunk := rv.Slice(start, end).Interface()
		s, err := q.build(chunk)
		if err != nil {
			return q.done(err)
		}
		// 任何一块失败立即终止：已插入块不回滚（无外层事务包裹）
		if _, err := s.Insert(chunk); err != nil {
			return q.done(err)
		}
	}
	q.invalidateCache()
	return q.done(invokeAfterCreate(q, value))
}

// save Save 的公共内核：切片逐元素路由，返回累计受影响行数。
func (q *XormQuery) save(value any) (int64, error) {
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return 0, nil
		}
		rv = rv.Elem()
	}
	// 切片：gorm Save 对切片走 upsert；xorm 无可移植 upsert，退化为
	// 逐元素"空主键插入、非空主键更新"路由（净语义最接近，钩子逐元素触发）。
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		var total int64
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			if elem.Kind() == reflect.Ptr {
				if elem.IsNil() {
					continue // 跳过 nil 元素（与 walkValues 口径一致）
				}
				n, err := q.saveOne(elem.Interface())
				if err != nil {
					return total, err
				}
				total += n
			} else if elem.CanAddr() {
				n, err := q.saveOne(elem.Addr().Interface())
				if err != nil {
					return total, err
				}
				total += n
			}
		}
		return total, nil
	}
	return q.saveOne(value)
}

// saveOne 单条 Save：按主键是否全为零值路由插入/更新路径。
// 插入路径与 Create 完全一致——gorm 驱动的 Save 空主键同样先落 gorm
// BeforeCreate 生成 ID，此处复用 createCore 对齐该语义。
// 更新路径用 AllCols() 写入所有字段（含零值），对应 gorm Save "更新全部字段"
// 的行为。差异：gorm 更新命中 0 行会回落 upsert 插入，此处保持纯更新语义，
// 0 行场景由调用方经 SaveResult().IsZeroRow() 自行判定。
func (q *XormQuery) saveOne(value any) (int64, error) {
	if pkAllZero(q.engine, value) {
		return q.createCore(value)
	}
	if err := invokeBeforeUpdate(q, value); err != nil {
		return 0, q.done(err)
	}
	s, err := q.build(value)
	if err != nil {
		return 0, q.done(err)
	}
	// xorm 的 Update 不会从被更新 bean 自动推导 WHERE（仅来自显式条件或
	// condiBean），必须显式按主键列构造等值条件，否则生成无 WHERE 的全表
	// UPDATE（多行表触发主键唯一冲突，单行表静默覆盖全部数据）。
	if err := applyPKCondition(s, q.engine, value); err != nil {
		return 0, q.done(err)
	}
	n, err := s.AllCols().Update(value)
	if err != nil {
		return 0, q.done(err)
	}
	// gorm Save 语义：更新命中 0 行（行不存在）回落 INSERT（X-04 upsert）。
	// licence 菜单同步等 upsert 场景依赖该语义；行已存在时不会走到这里。
	if n == 0 {
		return q.createCore(value)
	}
	q.invalidateCache()
	return n, q.done(invokeAfterUpdate(q, value))
}

// Save 保存记录：主键全零按插入处理，否则按主键更新（0 行回落插入，gorm 语义）。
func (q *XormQuery) Save(value any) error {
	_, err := q.save(value)
	return err
}

// updateColumnCore Update 的公共内核：单列更新，返回受影响行数。
// build(nil)：更新不按 dest 推导表名，要求链上已显式 Table()/Model()
// （显式表名由 Model/Table 的 applier 应用到 session）。
// 单列用 map 传参而非 struct 字段：xorm 对 struct 默认跳过零值字段，
// map 键无条件写入，保证 gorm Update"显式指定列（含零值）"的语义。
// 不调用模型钩子，与 gorm 驱动一致。
//
// X-08：value 为 contracts.SQLExpression（contracts.Expr 返回值）时改走
// updateWithExpr，生成 "SET col = <表达式>" 的数据库端原子更新。
func (q *XormQuery) updateColumnCore(column string, value any) (int64, error) {
	if _, ok := value.(contracts.SQLExpression); ok {
		return q.updateWithExpr(q.tableName, map[string]any{column: value})
	}
	s, err := q.build(nil)
	if err != nil {
		return 0, q.done(err)
	}
	// n 为受影响行数，仅成功路径回填 Result；错误路径忽略（行数无意义）
	n, err := s.Update(map[string]any{column: value})
	if err != nil {
		return 0, q.done(err)
	}
	q.invalidateCache()
	return n, nil
}

// Update 更新单列（要求链上已显式 Table()/Model）。
func (q *XormQuery) Update(column string, value any) error {
	_, err := q.updateColumnCore(column, value)
	return err
}

// updatesCore Updates 的公共内核：map 或 struct 批量字段更新。
// X-08：map 中任一值为 contracts.SQLExpression（contracts.Expr 返回值）时改走
// updateWithExpr——整个 map（表达式键 + 普通键）作为 SET 传入，builder.Eq 混排
// 原值与 *Expression 渲染。
// struct 传参时 xorm 跳过零值字段，与 gorm Updates(struct) 语义一致；
// map 传参写入全部键。不调用模型钩子，与 gorm 驱动一致。
// 注：struct 更新路径不支持表达式字段——xorm 的 struct 写入按列绑定字段值，
// 无法承载 "col = col + ?" 形态的表达式，需要表达式请用 map 传参。
func (q *XormQuery) updatesCore(values any) (int64, error) {
	if m, ok := values.(map[string]any); ok && mapHasSQLExpr(m) {
		return q.updateWithExpr(q.tableName, m)
	}
	s, err := q.build(nil)
	if err != nil {
		return 0, q.done(err)
	}
	n, err := s.Update(values)
	if err != nil {
		return 0, q.done(err)
	}
	q.invalidateCache()
	return n, nil
}

// mapHasSQLExpr 判断 map 的值中是否存在 contracts.SQLExpression（X-08 分流判定）。
func mapHasSQLExpr(m map[string]any) bool {
	for _, v := range m {
		if _, ok := v.(contracts.SQLExpression); ok {
			return true
		}
	}
	return false
}

// ── 表达式 UPDATE（X-08）─────────────────────────────────────────────
//
// xorm 的 Session.SetExpr(col, *builder.Expression) 不可用：*Expression 不匹配
// WriteArgs 的 builder 分支，会被当作单参数绑定而报错。因此带参表达式
// （contracts.Expr("count + ?", 5)）无法经 session 链式 API 渲染。
//
// 替代通道：xorm Session.Exec 支持传入单个 *builder.Builder（statement 的
// ConvertSQLOrArgs 走 builder.ToSQL 展开），且在事务 session 上原样执行
// （exec 分支按 isAutoCommit 走 tx.ExecContext），事务安全。故此处用
// builder.Update(Eq).From(table).Where(cond) 组装完整 UPDATE，经 ToSQL
// 得到 (sql, args) 后交 session.Exec——SET 中 Eq 值为 *Expression 时原生
// 渲染为 SQL 并平铺参数，占位符与参数严格对齐。
//
// 与 gorm 的差异：链上 Limit/Offset 不参与 UPDATE（gorm 仅 MySQL 方言支持
// UPDATE ... LIMIT，此处不覆盖）；ORM 钩子不触发（与普通 Update 路径一致）。

// buildCond 按序合并链上 condSpecs 为单个 builder.Cond（表达式 UPDATE 的
// WHERE 来源）。组合语义与 applyCondToSession 一致：
//   - condWhere：builder.And(累计, 当前)，首个条件直接作为累计；
//   - condOr：builder.Or(累计, 当前)；
//   - condNot：builder.And(累计, builder.Not{当前})。
//
// 条件转换：string → builder.Expr（先经 expandSlicePlaceholders 展开 IN 切片
// 占位符，与 session 路径语义一致）；map[string]any → builder.Eq；
// builder.Cond 原样。condSpecs 为空（或全部无效）返回 nil，即无 WHERE。
func (q *XormQuery) buildCond() builder.Cond {
	var acc builder.Cond
	for _, spec := range q.condSpecs {
		var cur builder.Cond
		switch cond := spec.query.(type) {
		case string:
			sqlStr, args := expandSlicePlaceholders(cond, spec.args)
			cur = builder.Expr(sqlStr, args...)
		case builder.Cond:
			cur = cond
		case map[string]any:
			cur = builder.Eq(cond)
		}
		if cur == nil || !cur.IsValid() {
			continue
		}
		if acc == nil {
			// 首个有效条件直接作为累计
			if spec.mode == condNot {
				acc = negateCond(cur)
			} else {
				acc = cur
			}
			continue
		}
		switch spec.mode {
		case condOr:
			acc = builder.Or(acc, cur)
		case condNot:
			acc = builder.And(acc, negateCond(cur))
		default:
			acc = builder.And(acc, cur)
		}
	}
	return acc
}

// negateCond 取反单个条件。builder v0.3.13 提供 Not（Cond 级取反，对
// condAnd/condOr 自动加括号），直接使用。
func negateCond(c builder.Cond) builder.Cond {
	return builder.Not{c}
}

// updateWithExpr 表达式 UPDATE 的组装与执行（X-08）。
// set 的值为 contracts.SQLExpression → builder.Expr（SET 渲染为 SQL 表达式），
// 否则原值绑定；WHERE 来自 buildCond（链上 Where/OrWhere/Not 副本）。
// table 为链上 Table()/Model() 记录的裸名，经 q.schemaTable 执行期解析
// schema 前缀；为空（未显式指定表）时返回 ErrUnsupported 包装错误。
func (q *XormQuery) updateWithExpr(table string, set map[string]any) (int64, error) {
	if table == "" {
		return 0, fmt.Errorf("%w: Update/Updates 表达式值要求链上已显式 Table()/Model()", contracts.ErrUnsupported)
	}
	setCond := builder.Eq{}
	for col, v := range set {
		if e, ok := v.(contracts.SQLExpression); ok {
			sqlStr, args := e.ExprSQL()
			setCond[col] = builder.Expr(sqlStr, args...)
		} else {
			setCond[col] = v
		}
	}
	b := builder.Update(setCond).From(q.schemaTable(table))
	if cond := q.buildCond(); cond != nil {
		b = b.Where(cond)
	}
	sqlStr, args, err := builder.ToSQL(b)
	if err != nil {
		return 0, q.done(err)
	}
	// build(nil) 复用事务 session（事务内安全执行）；链上错误也在此统一透出。
	s, err := q.build(nil)
	if err != nil {
		return 0, q.done(err)
	}
	execArgs := append(make([]any, 0, len(args)+1), sqlStr)
	execArgs = append(execArgs, args...)
	res, err := s.Exec(execArgs...)
	if err != nil {
		return 0, q.done(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, q.done(err)
	}
	q.invalidateCache()
	return n, nil
}

// Updates 批量更新字段（map 或 struct，要求链上已显式 Table()/Model）。
func (q *XormQuery) Updates(values any) error {
	_, err := q.updatesCore(values)
	return err
}

// deleteCore Delete 的公共内核：钩子 → 条件 → 删除 → 失效缓存 → 钩子。
// value 同时充当表定位 bean（表名兜底/主键条件来源），conds 为 gorm 语义的
// 附加 Where 条件（conds[0] 条件、其余参数，经 applyConds 应用）。
func (q *XormQuery) deleteCore(value any, conds []any) (int64, error) {
	if err := invokeBeforeDelete(q, value); err != nil {
		return 0, q.done(err)
	}
	s, err := q.build(value)
	if err != nil {
		return 0, q.done(err)
	}
	if err := applyConds(s, conds); err != nil {
		return 0, q.done(err)
	}
	n, err := s.Delete(value)
	if err != nil {
		return 0, q.done(err)
	}
	q.invalidateCache()
	return n, q.done(invokeAfterDelete(q, value))
}

// Delete 删除记录。
func (q *XormQuery) Delete(value any, conds ...any) error {
	_, err := q.deleteCore(value, conds)
	return err
}

// ── 写操作 Result 变体 ──────────────────────────────────────────────
//
// 复用与 error 变体完全相同的内核（钩子调用点、缓存失效、错误归一均一致），
// 仅额外回填 RowsAffected。错误路径行数不回填（保持零值）。

// CreateResult 插入并返回结果。RowsAffected 为 Insert 受影响行数
// （单条恒为 1；value 为切片时为总行数）。
func (q *XormQuery) CreateResult(value any) contracts.Result {
	n, err := q.createCore(value)
	return contracts.Result{RowsAffected: n, Error: err}
}

// UpdateResult 更新单列并返回结果。RowsAffected 为匹配 WHERE 的行数，
// 0 行（未命中）时 Error 为 nil，可经 IsZeroRow 判定。
// value 为 contracts.Expr 表达式时同样生效（X-08），RowsAffected 正常回填。
func (q *XormQuery) UpdateResult(column string, value any) contracts.Result {
	n, err := q.updateColumnCore(column, value)
	return contracts.Result{RowsAffected: n, Error: err}
}

// UpdatesResult 批量更新字段并返回结果。RowsAffected 语义同 UpdateResult。
// map 中含 contracts.Expr 表达式键时同样生效（X-08），RowsAffected 正常回填。
func (q *XormQuery) UpdatesResult(values any) contracts.Result {
	n, err := q.updatesCore(values)
	return contracts.Result{RowsAffected: n, Error: err}
}

// DeleteResult 删除并返回结果（含 Delete 前后钩子）。
// RowsAffected 为删除的行数。
func (q *XormQuery) DeleteResult(value any, conds ...any) contracts.Result {
	n, err := q.deleteCore(value, conds)
	return contracts.Result{RowsAffected: n, Error: err}
}

// SaveResult 保存并返回结果（含对应插入/更新钩子）。
// RowsAffected：插入路径为 Insert 受影响行数（1）；更新路径为匹配 WHERE
// 的行数（0 表示无匹配行，可经 IsZeroRow 判定；gorm 驱动同路径回落 upsert，
// 此处如上 saveOne 注释保持纯更新语义）。
func (q *XormQuery) SaveResult(value any) contracts.Result {
	n, err := q.save(value)
	return contracts.Result{RowsAffected: n, Error: err}
}

// ── 主键零值判定（Save 路由用）───────────────────────────────────────

// pkAllZero 判断 value（struct / 指针）的主键值是否全为零值。
// 表无主键、解析失败或 value 非 struct 时按"零值"处理回落插入路径：
// 无主键便无法构造更新条件，s.AllCols().Update 会退化为无 WHERE 的全表
// UPDATE，数据破坏不可逆，必须避免。
func pkAllZero(e *xorm.Engine, value any) bool {
	bean := tableBeanOf(value)
	if bean == nil {
		return true
	}
	t, err := e.TableInfo(bean)
	if err != nil {
		return true
	}
	// Indirect 解引用指针；nil 指针得到零 Value（Kind 非 Struct）→ 视为零值
	rv := reflect.Indirect(reflect.ValueOf(value))
	if rv.Kind() != reflect.Struct {
		return true
	}
	return pkStructZero(t, rv)
}

// pkStructZero 判定单个 struct 值的主键字段是否全为零值。
// xorm 的 PrimaryKeys 存放 mapper 映射后的数据库列名（如字段 ID → 列 id），
// 不能直接用列名查 struct 字段，须经 schemas.Table 列定位。
// 定位用 FieldIndex 而非 FieldName：extends 展开后 FieldName 带嵌入前缀
// （如 "Model.ID"），reflect.FieldByName 不支持点分路径会恒判零值，
// 使 Save 误走 INSERT 报主键冲突（X-03）。
func pkStructZero(t *schemas.Table, rv reflect.Value) bool {
	for _, name := range t.PrimaryKeys {
		col := t.GetColumn(name)
		if col == nil {
			continue
		}
		fv := structFieldValue(rv, col)
		// 字段不可定位（如内嵌指针为 nil 得到零 Value）时按零值处理：
		// 宁走插入路径拿到重复键错误，也不走无主键条件的全表更新。
		if fv.IsValid() && !fv.IsZero() {
			return false
		}
	}
	return true
}

// structFieldValue 按列定义定位 struct 字段值。
// 优先 FieldIndex（FieldByIndex 语义，天然支持 extends 嵌入路径），
// 逐级校验指针/结构有效性；FieldIndex 缺失时回落 FieldName。
func structFieldValue(rv reflect.Value, col *schemas.Column) reflect.Value {
	if len(col.FieldIndex) > 0 {
		cur := rv
		for _, idx := range col.FieldIndex {
			if cur.Kind() == reflect.Ptr {
				if cur.IsNil() {
					return reflect.Value{}
				}
				cur = cur.Elem()
			}
			if cur.Kind() != reflect.Struct {
				return reflect.Value{}
			}
			cur = cur.Field(idx)
		}
		return cur
	}
	if col.FieldName != "" {
		return rv.FieldByName(col.FieldName)
	}
	return reflect.Value{}
}

// ── 主键条件构造（Save 更新路径用）─────────────────────────────────────

// applyPKCondition 按主键列的当前值构造 WHERE 等值条件。
// xorm Session.Update 的 WHERE 仅来自显式条件，不会自动附加被更新 bean 的
// 主键（与 gorm 行为不同），漏加会生成无 WHERE 的全表 UPDATE。
func applyPKCondition(s *xorm.Session, e *xorm.Engine, value any) error {
	t, err := e.TableInfo(tableBeanOf(value))
	if err != nil {
		return fmt.Errorf("xorm: Save 解析表信息失败: %w", err)
	}
	rv := reflect.Indirect(reflect.ValueOf(value))
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("%w: Save 更新路径要求 struct，收到 %T", contracts.ErrUnsupported, value)
	}
	eq := builder.Eq{}
	for _, col := range t.PrimaryKeys {
		c := t.GetColumn(col)
		if c == nil {
			return fmt.Errorf("xorm: 主键列 %q 无法定位", col)
		}
		// FieldIndex 定位：兼容 extends 嵌入（FieldName 带前缀，见 X-03）
		fv := structFieldValue(rv, c)
		if !fv.IsValid() {
			return fmt.Errorf("xorm: 主键列 %q 对应字段 %q 不存在", col, c.FieldName)
		}
		eq[col] = fv.Interface()
	}
	s.Where(eq)
	return nil
}
