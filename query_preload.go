package xormdriver

import (
	"fmt"
	"reflect"

	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/database/preload"

	"xorm.io/xorm"
	"xorm.io/xorm/schemas"
)

// ── 关联预加载（共享引擎薄包装）──────────────────────────────────────
//
// xorm 没有关联元数据（association tag），驱动自研的"外键解析 + 批量 IN +
// 反射回填"实现已上收为框架级共享 Preload 引擎（go-fast-framework/database/
// preload，行为契约见 orm-tag-design.md §9.2）。本文件仅保留驱动侧薄包装：
//
//   - xormMetaAdapter：把 xorm 引擎表元数据适配为 preload.MetaAdapter（表名为
//     经 TablePrefix mapper/TableName() 接口处理后的最终表名，schema 前缀由
//     NewQuery 闭包经 schemaTable baked in）；
//   - newPreloadEngine：为查询构造共享引擎（Meta + NewQuery 子查询闭包）；
//   - Preload/runPreloads：链上声明（path/conds/callbacks）的记录与执行。
//
// 关联解析（rel tag → gorm tag → 约定 → 方向判定 → many2many 两跳 →
// polymorphic 类型列过滤）与忽略标记校验（orm:"-"/xorm:"-"/gorm:"-" 任一命中
// 即可，按 §8.3 放宽，不再硬编码 xorm:"-"）全部由引擎 DefaultResolver 承担；
// 驱动侧不再自研 resolvePreloadKeys/gormAssocKeys/批量 IN/回填。

// preloadSpec 一条 Preload 声明（链上记录，终结时执行）。
type preloadSpec struct {
	path      string                                  // 关联字段路径，如 "Profile"、"Orders.Items"
	conds     []any                                   // 子查询 Where 条件（首个为条件，其余参数）
	callbacks []func(contracts.Query) contracts.Query // 子查询定制回调
}

// Preload 声明关联预加载。query 为字段路径（点号嵌套）。args 中
// func(contracts.Query) contracts.Query 类型的项提取为子查询定制回调
// （在子查询构造后、执行前应用，可排序/分页/追加条件），其余项组成首层子
// 查询的 Where 条件（与 gorm 语义一致）；条件类型在链上校验，非法即记入
// 链上错误，由终结方法返回 ErrUnsupported 包装。预加载在父查询成功装载行
// 后执行（Find/First/Last/Take/FindInBatches；缓存命中路径同样执行）。
func (q *XormQuery) Preload(query string, args ...any) contracts.Query {
	spec := preloadSpec{path: query}
	for _, a := range args {
		if cb, ok := a.(func(contracts.Query) contracts.Query); ok {
			spec.callbacks = append(spec.callbacks, cb)
			continue
		}
		spec.conds = append(spec.conds, a)
	}
	if len(spec.conds) > 0 && !validCondType(spec.conds[0]) {
		return q.setErr(fmt.Errorf("%w: Preload 条件仅支持 string/builder.Cond/map[string]any，收到 %T", contracts.ErrUnsupported, spec.conds[0]))
	}
	return q.wrap(func(c *XormQuery) {
		c.preloads = append(c.preloads, spec)
	})
}

// runPreloads 在父查询装载行成功后执行全部已声明的预加载（共享引擎）。
// dest 为 Find/getOne/FindInBatches 装载后的结果（*T / *[]T / *[]*T 等）。
// 各声明独立按序执行：字段解析/外键校验失败经 ErrUnsupported 包装，子查询
// 错误原样透传，均由终结方法经 q.done 归一为框架 Sentinel Error。
func (q *XormQuery) runPreloads(dest any) error {
	if len(q.preloads) == 0 || dest == nil {
		return nil
	}
	eng := newPreloadEngine(q)
	for _, spec := range q.preloads {
		if err := eng.Preload(dest, spec.path, spec.conds, spec.callbacks); err != nil {
			return err
		}
	}
	return nil
}

// newPreloadEngine 为当前查询构造共享 Preload 引擎（每次 runPreloads 新建，
// 元数据缓存随引擎生命周期内复用）。
func newPreloadEngine(q *XormQuery) *preload.Engine {
	return &preload.Engine{
		Meta: &xormMetaAdapter{engine: q.engine},
		NewQuery: func() contracts.Query {
			// 子查询状态继承（对齐原 preloadLevel 子查询构造清单）：engine/
			// tx/schema/ctx/查询缓存配置；不继承父链 Where 条件、显式表名/
			// 分页/排序/投影——子查询的表与条件由引擎按关联元数据重建
			//（契约 §9.2：继承 schema/ctx/tx/缓存配置，不继承父链条件）。
			return &XormQuery{
				engine:   q.engine,
				tx:       q.tx,
				schema:   q.schema,
				ctx:      q.ctx,
				qc:       q.qc,
				cacheCfg: q.cacheCfg,
			}
		},
	}
}

// ── preload.MetaAdapter 的 xorm 适配 ─────────────────────────────────

// xormMetaAdapter 把 xorm 引擎的表元数据适配为共享引擎的 MetaAdapter。
// 表名/主键/列名一律出自 engine.TableInfo（最终表名——已应用 TablePrefix
// mapper 与 TableName() 接口，与原实现 schemaTable(childTI.Name) 语义一致），
// 均为数据库裸名，不含 schema 前缀（前缀由 NewQuery 闭包经 schemaTable 拼接）。
type xormMetaAdapter struct {
	engine *xorm.Engine
}

// tableOf 解析模型类型对应的表元数据（TableInfo 缓存由 xorm 引擎维护）。
func (m *xormMetaAdapter) tableOf(modelType reflect.Type) (*schemas.Table, error) {
	return m.engine.TableInfo(reflect.New(modelType).Interface())
}

// TableName 返回模型的最终表名（含 TablePrefix 前缀，尊重 TableName() 接口）。
func (m *xormMetaAdapter) TableName(modelType reflect.Type) (string, error) {
	ti, err := m.tableOf(modelType)
	if err != nil {
		return "", err
	}
	return ti.Name, nil
}

// PrimaryKeys 返回模型的主键列名列表（ti.PrimaryKeys 存放 mapper 映射后的列名）。
func (m *xormMetaAdapter) PrimaryKeys(modelType reflect.Type) ([]string, error) {
	ti, err := m.tableOf(modelType)
	if err != nil {
		return nil, err
	}
	return ti.PrimaryKeys, nil
}

// ColumnOfField 按 Go 字段名在表元数据中定位列名（extends 展开字段以
// FieldName 前缀形态存在于 TableInfo，由 columnOfFieldName 精确匹配）。
func (m *xormMetaAdapter) ColumnOfField(modelType reflect.Type, fieldName string) (string, bool) {
	ti, err := m.tableOf(modelType)
	if err != nil {
		return "", false
	}
	col := columnOfFieldName(ti, fieldName)
	if col == nil {
		return "", false
	}
	return col.Name, true
}

// HasColumn 列名是否存在于模型对应表中。
func (m *xormMetaAdapter) HasColumn(modelType reflect.Type, columnName string) bool {
	ti, err := m.tableOf(modelType)
	if err != nil {
		return false
	}
	return ti.GetColumn(columnName) != nil
}

// columnOfFieldName 按结构体字段名在表元数据中定位列。
func columnOfFieldName(ti *schemas.Table, fieldName string) *schemas.Column {
	for _, c := range ti.Columns() {
		if c.FieldName == fieldName {
			return c
		}
	}
	return nil
}
