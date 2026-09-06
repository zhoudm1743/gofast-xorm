package xormdriver

import (
	"database/sql"
	"fmt"
	"reflect"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 读终结方法 ───────────────────────────────────────────────────────

// Find 查询多行填充到 dest（切片指针）。
// AfterFind 钩子在查询成功后调用，缓存命中（withCache 提前返回 nil）同样触发，
// 与 gormdriver "查到数据即回调钩子" 的语义一致；钩子之后执行链上声明的
// Preload 预加载（与 gorm 回调顺序一致：AfterFind 先于 preload 回填）。
func (q *XormQuery) Find(dest any, conds ...any) error {
	// 终结方法变参 conds 不经过链式 applier，不产生缓存键片段；不同 conds 的
	// 同链查询会命中同一缓存键返回错误数据，必须显式并入缓存键。
	if len(conds) > 0 {
		q = q.clone()
		q.keyParts = append(q.keyParts, fmt.Sprintf("cond:%v|%v", conds[0], conds[1:]))
	}
	err := q.withCache(dest, func() error {
		s, err := q.build(dest)
		if err != nil {
			return err
		}
		if err := applyConds(s, conds); err != nil {
			return err
		}
		return s.Find(dest)
	})
	if err != nil {
		return q.done(err)
	}
	if err := invokeAfterFind(q, dest); err != nil {
		return q.done(err)
	}
	return q.done(q.runPreloads(dest))
}

// pkSort First/Last 的主键排序方向（Take 不排序）。
type pkSort int

const (
	pkSortNone pkSort = iota
	pkSortAsc         // First：主键升序，无显式排序时保证"第一条"语义确定
	pkSortDesc        // Last：主键降序
)

// getOne 单行查询公共实现（First/Last/Take 共用）。
// 排序与 Limit(1) 在 build 之后应用：主键排序保证 First/Last 语义确定；
// s.Limit(1) 在 applyLimit 之后调用，将用户 LIMIT 覆盖为 1、保留用户 OFFSET
// （xorm Limit 不重置已设置的 Start），与 gorm First/Last/Take 的单行语义一致。
// 未命中行统一构造 sql.ErrNoRows 交给 q.done → wrapError 映射为
// contracts.ErrRecordNotFound；仅在真实取到行后触发 AfterFind 钩子。
func (q *XormQuery) getOne(dest any, conds []any, order pkSort) error {
	s, err := q.build(dest)
	if err == nil {
		err = applyConds(s, conds)
	}
	if err == nil && order != pkSortNone {
		// dest 为投影结构体/map 等解析不出主键时跳过排序，退化为 Take 语义
		if pks := pkColumnsOf(q.engine, dest); len(pks) > 0 {
			if order == pkSortAsc {
				s.Asc(pks...)
			} else {
				s.Desc(pks...)
			}
		}
	}
	if err == nil {
		s.Limit(1)
		var found bool
		found, err = s.Get(dest)
		if err == nil && !found {
			err = sql.ErrNoRows
		}
	}
	if err != nil {
		return q.done(err)
	}
	if err := invokeAfterFind(q, dest); err != nil {
		return q.done(err)
	}
	return q.done(q.runPreloads(dest))
}

// First 取按主键升序的第一行。
func (q *XormQuery) First(dest any, conds ...any) error {
	return q.getOne(dest, conds, pkSortAsc)
}

// Last 取按主键降序的第一行。
func (q *XormQuery) Last(dest any, conds ...any) error {
	return q.getOne(dest, conds, pkSortDesc)
}

// Take 取任意一行（不追加排序）。
func (q *XormQuery) Take(dest any, conds ...any) error {
	return q.getOne(dest, conds, pkSortNone)
}

// Count 统计行数。无 dest 可供表名兜底推导，要求链上已显式 Table()/Model()
// （与 gormdriver Count 一致）；计数结果作为标量走查询缓存。
func (q *XormQuery) Count(count *int64) error {
	return q.done(q.withCache(count, func() error {
		// Count 剥离链上 ORDER BY：聚合列不在排序列集合内时 PG 报 42803，
		// 且排序对行数无意义（gorm Count 同样丢弃，X-06）。
		s, err := q.buildOpts(nil, true)
		if err != nil {
			return err
		}
		n, err := s.Count()
		if err != nil {
			return err
		}
		*count = n
		return nil
	}))
}

// Exists 判断是否存在命中行：LIMIT 1 让 DB 命中首行后即可短路，避免全量计数。
// 只关心布尔结果且无可序列化的行数据，不走查询缓存。
func (q *XormQuery) Exists(dest any, conds ...any) (bool, error) {
	s, err := q.build(dest)
	if err == nil {
		err = applyConds(s, conds)
	}
	if err != nil {
		return false, q.done(err)
	}
	s.Limit(1)
	n, err := s.Count(dest)
	return n > 0, q.done(err)
}

// FindInBatches 分批查询：以 LIMIT batchSize + OFFSET 游标逐批加载，
// 每批结果追加进 dest 后回调 fc，避免一次性载入大结果集占满内存。
// dest 必须是 *[]T 或 *[]*T；整体不走查询缓存（分批写回无法整体序列化恢复）。
// 链上声明的 Preload 逐批执行（append 前回填本批 chunk），fc 回调拿到的是
// 已预加载的批，与 gorm 的 FindInBatches 语义一致。
func (q *XormQuery) FindInBatches(dest any, batchSize int, fc func(contracts.Query, int) error) error {
	destVal := reflect.ValueOf(dest)
	if destVal.Kind() != reflect.Ptr || destVal.Elem().Kind() != reflect.Slice {
		return q.done(fmt.Errorf("%w: FindInBatches 要求 dest 为 *[]T 或 *[]*T，实际为 %T", contracts.ErrUnsupported, dest))
	}
	sliceVal := destVal.Elem()
	sliceType := sliceVal.Type()

	// reflect.MakeSlice 的容量参数不允许为负（batchSize 非法时降级为 0）
	capHint := batchSize
	if capHint < 0 {
		capHint = 0
	}

	// 每批用克隆查询携带本批 LIMIT/OFFSET 执行；bq 同时透传给 fc（gorm 语义：
	// 回调拿到的是限定本批的查询，可在回调内继续链式终结）。
	cursor, batch := 0, 0
	for {
		bq := q.clone()
		bq.limitN = batchSize
		bq.startN = cursor

		// 用可写副本承接本批结果：避免 xorm 直接在 dest 上 append 破坏追加语义
		chunk := reflect.MakeSlice(sliceType, 0, capHint)
		chunkPtr := reflect.New(chunk.Type())
		chunkPtr.Elem().Set(chunk)

		s, err := bq.build(chunkPtr.Interface())
		if err == nil {
			err = s.Find(chunkPtr.Interface())
		}
		if err != nil {
			return q.done(err)
		}

		n := chunkPtr.Elem().Len()
		if n == 0 {
			break
		}

		// 预加载必须在 append 之前执行：append 复制元素，先回填 chunk 再复制
		// 进 dest 才能保留关联数据（gorm 的 FindInBatches 同样逐批执行 preload，
		// fc 回调拿到的是已预加载的批）。
		if err := bq.runPreloads(chunkPtr.Interface()); err != nil {
			return q.done(err)
		}

		sliceVal.Set(reflect.AppendSlice(sliceVal, chunkPtr.Elem()))
		batch++

		// fc 错误即整体错误，终止后续批次
		if err := fc(bq, batch); err != nil {
			return q.done(err)
		}

		// 不足一批说明已到结果末尾，无需再查
		if n < batchSize {
			break
		}
		cursor += n
	}
	return nil
}

// ── FirstOrCreate / FirstOrInit ──────────────────────────────────────

// FirstOrCreate 按条件查询，命中返回该行；未命中将 dest 插入数据库。
// 主键自动生成由 invokeBeforeCreate 触发（AutoGenerateID），写后失效查询缓存，
// 与 Create 语义一致。
func (q *XormQuery) FirstOrCreate(dest any, conds ...any) error {
	s, err := q.build(dest)
	if err != nil {
		return q.done(err)
	}
	if err := applyConds(s, conds); err != nil {
		return q.done(err)
	}
	found, err := s.Get(dest)
	if err != nil {
		return q.done(err)
	}
	if found {
		if err := invokeAfterFind(q, dest); err != nil {
			return q.done(err)
		}
		return nil
	}
	if err := invokeBeforeCreate(q, dest); err != nil {
		return q.done(err)
	}
	s2, err := q.build(dest)
	if err != nil {
		return q.done(err)
	}
	if _, err := s2.Insert(dest); err != nil {
		return q.done(err)
	}
	q.invalidateCache()
	return q.done(invokeAfterCreate(q, dest))
}

// FirstOrInit 按条件查询，命中填充 dest 并触发 AfterFind 钩子；未命中 dest 保持
// 调用方传入的原值、不落库。
// 与 gorm 的差异：gorm 会把 struct/map 条件的属性回填进 dest，此处不做回填，
// 需要默认值时由调用方在 dest 中预置。
func (q *XormQuery) FirstOrInit(dest any, conds ...any) error {
	s, err := q.build(dest)
	if err != nil {
		return q.done(err)
	}
	if err := applyConds(s, conds); err != nil {
		return q.done(err)
	}
	found, err := s.Get(dest)
	if err != nil {
		return q.done(err)
	}
	if found {
		return q.done(invokeAfterFind(q, dest))
	}
	return nil
}
