// query_scan.go 实现 contracts.Query 的 Scan 系读终结方法（Scan/Pluck/ScanMap/Row/Rows）。
//
// 与 gorm 驱动的差异：
//   - Scan/Pluck/ScanMap 输出语义对齐（Scan 单 struct 指针走 Get、集合走 Find；
//     Pluck/ScanMap 基于 QueryInterface 的 []map[string]any 结果做 []byte→string
//     归一，与 gormdriver.ScanMap 输出形态一致）；三者均为读终结，统一经
//     withCache 参与查询缓存（契约全局约定 5）。
//   - Row()/Rows() 仅支持 Raw() 原生 SQL 链（gorm 驱动的 Row/Rows 还可作用于
//     链式查询，xorm 无对应语句级游标出口）；实现上直接经 engine.DB() 连接池
//     执行，事务内调用同样走独立连接，不受事务约束。
package xormdriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// *sql.Row / *sql.Rows 的方法集与框架 Row/Rows 接口一一对应，天然满足，
// 无需再写适配器。
var (
	_ contracts.Row  = (*sql.Row)(nil)
	_ contracts.Rows = (*sql.Rows)(nil)
)

// ── Scan / Pluck / ScanMap ───────────────────────────────────────────

// Scan 将结果扫描进 dest：dest 为单个 struct 指针时走 Get（含 Raw SQL 单行
// 场景，xorm 的 GenGetSQL/GenFindSQL 在 statement.RawSQL 非空时直接返回原生
// SQL，故链式与 Raw 统一适用），否则（如 *[]T、*[]*T）走 Find。
// Get 返回 found=false 表示无记录，dest 保持零值属正常语义，不视为错误。
func (q *XormQuery) Scan(dest any) error {
	err := q.withCache(dest, func() error {
		s, err := q.build(dest)
		if err != nil {
			return err
		}
		// 标量 dest（*int64/*string 等，如 SELECT count(*)）：取结果集首行首列。
		// Find 无法扫描非 struct/slice/map 的 dest，必须单独经 QueryInterface 提取。
		if rv := reflect.ValueOf(dest); rv.IsValid() && rv.Kind() == reflect.Ptr {
			et := rv.Type().Elem()
			if et.Kind() != reflect.Struct && et.Kind() != reflect.Slice && et.Kind() != reflect.Map && et.Kind() != reflect.Array {
				return scanScalar(s, dest)
			}
		}
		// reflect 判定"单 struct 指针"以跳过 *[]T 等集合 dest：
		// xorm 的 Get 只接受 struct 指针（二级指针/nil 直接报错）。
		if rv := reflect.ValueOf(dest); rv.IsValid() && rv.Kind() == reflect.Ptr && rv.Type().Elem().Kind() == reflect.Struct {
			_, err = s.Get(dest)
			return err
		}
		return s.Find(dest)
	})
	return q.done(err)
}

// Pluck 查询单列并按序收集进 dest（*[]基本类型）。
// 要求链上已显式 Table()/Model()：此处 build(nil) 不做 dest 表名推导，且 xorm
// GenQuerySQL 在未设置表名时直接返回 ErrTableNotFound，依赖 dest 推导不可行。
//
// 列选择经 s.Cols(column) 注入：已确认 xorm v1.4.1 中 Session.QueryInterface →
// statement.GenQuerySQL → genSelectColumnStr()，其列解析顺序为 SelectStr（显式
// Select() 设置）→ ColumnStr()（由 Cols 填充的 ColumnMap 经 Quoter Join 生成，
// 优先于按模型推导的通配列），即 Cols 对 QueryInterface 的 SELECT 列生效；
// 副作用：若链上已有显式 Select()，其 SelectStr 会覆盖 Cols，届时 row[column]
// 可能缺键（走下方首键回退）。
func (q *XormQuery) Pluck(column string, dest any) error {
	// 列名经 applier 注入而非 build 后直接调 s.Cols：addApplier 同时把列名
	// 纳入缓存键片段，避免同链不同列的 Pluck 命中同一缓存键串值。
	nq := q.addApplier("pluck:"+column, func(q *XormQuery, s *xorm.Session) error {
		s.Cols(column)
		return nil
	})
	err := nq.withCache(dest, func() error {
		s, err := nq.build(nil)
		if err != nil {
			return err
		}
		rows, err := s.QueryInterface()
		if err != nil {
			return err
		}
		rv := reflect.ValueOf(dest)
		if !rv.IsValid() || rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Slice {
			return fmt.Errorf("xormdriver: Pluck dest 必须为切片指针，得到 %T", dest)
		}
		sliceType := rv.Elem().Type()
		elemType := sliceType.Elem()
		result := reflect.MakeSlice(sliceType, 0, len(rows))
		for _, row := range rows {
			var val any
			v, ok := row[column]
			if ok {
				val = v
			} else {
				// 部分方言返回的列名与请求列存在大小写/别名差异导致键缺失，
				// 回退取该行第一个值（单列查询下即该列值，多列时顺序不保证）。
				for _, v = range row {
					val = v
					break
				}
			}
			// []byte→string 与 ScanMap 归一语义一致：既抹平 MySQL 文本列的
			// []byte 类型，也保证 dest 可被 JSON 序列化以参与查询缓存。
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			ev := reflect.ValueOf(val)
			// !ev.IsValid() 覆盖 NULL 值（nil 无类型可赋给非接口元素）
			if !ev.IsValid() || !ev.Type().AssignableTo(elemType) {
				return fmt.Errorf("xormdriver: Pluck 列 %q 的值 %T 无法写入目标切片元素类型 %s", column, val, elemType)
			}
			result = reflect.Append(result, ev)
		}
		rv.Elem().Set(result)
		return nil
	})
	return nq.done(err)
}

// ScanMap 将结果集扫描为 []map[string]any（列名 → 归一化值）并追加进 dest，
// 输出形态与 gormdriver.ScanMap 一致：[]byte（MySQL 文本/二进制列经 database/sql
// 扫描的默认类型）转为 string，其余驱动原生类型（int64/float64/time.Time 等）
// 原样保留。dest 追加而非替换，与 gorm 驱动行为对齐。
func (q *XormQuery) ScanMap(dest *[]map[string]any) error {
	err := q.withCache(dest, func() error {
		s, err := q.build(nil)
		if err != nil {
			return err
		}
		rows, err := s.QueryInterface()
		if err != nil {
			return err
		}
		// QueryInterface 返回的 map 归本查询独有，原地改写无需拷贝
		for _, row := range rows {
			for k, v := range row {
				if b, ok := v.([]byte); ok {
					row[k] = string(b)
				}
			}
			*dest = append(*dest, row)
		}
		return nil
	})
	return q.done(err)
}

// ── Row / Rows（仅支持 Raw 原生 SQL）──────────────────────────────────

// errorRow 非 Raw 链上调用 Row() 的占位返回：错误延迟到 Scan 时才报出，
// 与 *sql.Row"构造不报错、Scan 时返回错误"的语义一致，避免破坏调用方写法。
// xorm 驱动的 Row/Rows 仅支持 Raw() 链，链式查询请改用 Take/Find。
type errorRow struct{ err error }

func (r errorRow) Scan(dest ...any) error { return r.err }

// Row 执行 Raw() 记录的原生 SQL 并返回单行游标（*sql.Row 直接满足 contracts.Row）。
// 与 gorm 驱动的差异：xorm 驱动的 Row/Rows 仅支持 Raw() 原生 SQL；且此处不经
// session 而直接走 engine.DB() 连接池执行，事务内调用同样使用独立连接，不受事务
// 约束（游标生命周期跨语句，无法安全绑定事务 session）。
func (q *XormQuery) Row() contracts.Row {
	if q.rawSQL == "" {
		// 经 done 走统一错误出口：链上已有错误优先透出
		return errorRow{err: q.done(errors.New("xormdriver: Row 仅支持 Raw() 原生 SQL 查询链"))}
	}
	ctx := q.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return q.engine.DB().QueryRowContext(ctx, q.rawSQL, q.rawArgs...)
}

// Rows 执行 Raw() 记录的原生 SQL 并返回多行游标（*sql.Rows 直接满足
// contracts.Rows：Next/Scan/Close/Columns），调用方负责 Close()。
// 与 gorm 驱动的差异：仅支持 Raw() 原生 SQL；事务内使用同样走独立连接，
// 不受事务约束。
func (q *XormQuery) Rows() (contracts.Rows, error) {
	if q.rawSQL == "" {
		return nil, q.done(errors.New("xormdriver: Rows 仅支持 Raw() 原生 SQL 查询链"))
	}
	ctx := q.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := q.engine.DB().QueryContext(ctx, q.rawSQL, q.rawArgs...)
	if err != nil {
		return nil, q.done(err)
	}
	return rows, nil
}

// ── Exec ─────────────────────────────────────────────────────────────

// Exec 执行原生 SQL 写操作（INSERT/UPDATE/DELETE/DDL 等）。
// 成功后按写终结语义失效查询缓存：原生写与 Create/Update/Delete 终结一样
// 会改变数据，缓存若不失效将读到旧值（gorm 驱动由 go-gorm/caches 插件在
// 回调层失效，此处为自研缓存的等价语义）。
func (q *XormQuery) Exec(sql string, values ...any) error {
	s, err := q.build(nil)
	if err != nil {
		return q.done(err)
	}
	args := append([]any{sql}, values...)
	if _, err := s.Exec(args...); err != nil {
		return q.done(err)
	}
	q.invalidateCache()
	return nil
}

// ExecResult 执行原生 SQL 写操作并返回受影响行数（X-08）。
// 需依据行数做业务判定（如配额原子扣减、存在性更新）时使用：
// RowsAffected 为 SQL 受影响行数；0 行（未命中）时 Error 为 nil，
// 可经 IsZeroRow 判定。事务内复用事务 session，与 Exec 同路径；
// 成功后失效查询缓存（写终结语义与 Exec 一致）。
func (q *XormQuery) ExecResult(sql string, values ...any) contracts.Result {
	s, err := q.build(nil)
	if err != nil {
		return contracts.Result{Error: q.done(err)}
	}
	args := append([]any{sql}, values...)
	res, err := s.Exec(args...)
	if err != nil {
		return contracts.Result{Error: q.done(err)}
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return contracts.Result{Error: q.done(err)}
	}
	q.invalidateCache()
	return contracts.Result{RowsAffected: ra}
}

// ── 标量 Scan 支持 ───────────────────────────────────────────────────

// scanScalar 从结果集首行提取首个标量值写入 dest（*基本类型）。
// 无行时保持 dest 零值不报错（对齐 gorm Scan 无行语义）。
// 典型用途：Raw("SELECT count(*) FROM t").Scan(&n)。
func scanScalar(s *xorm.Session, dest any) error {
	rows, err := s.QueryInterface()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	for _, v := range rows[0] {
		return assignScalar(dest, v)
	}
	return nil
}

// assignScalar 将数据库标量值（含 []byte→string 归一）写入 dest 指向的变量；
// 类型不可直接赋值时尝试可表示转换（如 int64→int），仍不匹配才报错。
func assignScalar(dest any, v any) error {
	rv := reflect.Indirect(reflect.ValueOf(dest))
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	val := reflect.ValueOf(v)
	if !val.IsValid() {
		return nil
	}
	switch {
	case val.Type().AssignableTo(rv.Type()):
		rv.Set(val)
	case val.Type().ConvertibleTo(rv.Type()):
		rv.Set(val.Convert(rv.Type()))
	default:
		// 驱动差异兜底：glebarez/go-sqlite 对 count(*) 等聚合返回 string，
		// 数值型 dest 需经 strconv 解析（reflect 的 ConvertibleTo 不含 string→数值）。
		if parsed, ok := parseNumeric(v, rv.Type()); ok {
			rv.Set(parsed)
			return nil
		}
		return fmt.Errorf("xorm: Scan 标量类型不匹配: %T → %s", v, rv.Type())
	}
	return nil
}

// parseNumeric 将字符串值按 dest 的数值类型解析（int/uint/float 全系）。
func parseNumeric(v any, t reflect.Type) (reflect.Value, bool) {
	s, ok := v.(string)
	if !ok {
		return reflect.Value{}, false
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return reflect.ValueOf(n).Convert(t), true
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return reflect.ValueOf(n).Convert(t), true
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return reflect.ValueOf(f).Convert(t), true
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(s); err == nil {
			return reflect.ValueOf(b), true
		}
	}
	return reflect.Value{}, false
}
