package xormdriver

// soft_delete.go 框架托管软删除（sd tag，文档 §4.5/§11.9）的 xorm 侧实现。
//
// sd 标记的模型（database.SoftDelete/SoftDeleteMilli/SoftDeleteNano/
// SoftDeleteFlag/SoftDeleteTime 或自定义 sd tag 字段）由框架驱动托管：
//   - Delete/DeleteResult 自动改写为置位 UPDATE（写 SdMeta.DeletedValue(now)），
//     不再物理删除；
//   - 默认查询（Find/First/Take/Last/Count/Pluck 与 Update/Updates/Save 更新路径）
//     自动附加 SdMeta.AliveCond 存活过滤（Unscoped 绕过）；
//   - OnlyTrashed/Restore 类型感知（SdMeta.TrashedCond/AliveValue）。
//
// 未打 sd 标记的 deleted_at 列保持旧版业务级手动语义（查询需显式
// Where("deleted_at = ?", 0)），注册表不收录、过滤不生效，完全兼容。
//
// 与 gormdriver 的口径差异固化：Scan/ScanMap/Row/Rows 原生扫描路径不过滤
// （gorm 侧 Scan 走 Rows 回调天然绕过软删 QueryClauses，双驱动对齐）。

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/zhoudm1743/go-fast-framework/contracts/ormtag"

	"xorm.io/builder"
	"xorm.io/xorm"
)

// sdFieldMeta 软删字段注册表条目（reflect.Type → 元数据，查询期惰性写入）。
type sdFieldMeta struct {
	Column string        // 软删列名
	Sd     ormtag.SdMeta // 模式/类型感知条件与值
}

// sdRegistryKey 注册表键类型（避免与可能的其他 reflect.Type 键冲突）。
type sdRegistryKey struct{ t reflect.Type }

// sdOfModel 从模型值解析软删元数据（指针/切片/数组解引用到 struct）。
// 列名经 xorm TableInfo 按字段名反查；sd 字段不是映射列时返回错误（与 gorm
// 侧 patch 期 fail-fast 对齐，此处为查询期首次使用时报错）。
func (d *XormDriver) sdOfModel(value any) (*sdFieldMeta, error) {
	if d == nil {
		return nil, nil
	}
	return sdOfModel(d.engine, &d.sdFields, value)
}

// sdOfModel 查询级入口：驱动级注册表与引擎在 Query() 创建时注入（与 qc
// 同模式，wrap 拷贝共享同一注册表）。
func (q *XormQuery) sdOfModel(value any) (*sdFieldMeta, error) {
	return sdOfModel(q.engine, q.sdFields, value)
}

// sdOfModel 解析主体：engine 提供 TableInfo 列名反查，registry 按类型缓存
// （含错误缓存）；非 sd 模型返回 (nil, nil)。
func sdOfModel(engine *xorm.Engine, registry *sync.Map, value any) (*sdFieldMeta, error) {
	if engine == nil || registry == nil || value == nil {
		return nil, nil
	}
	t := reflect.TypeOf(value)
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, nil
	}
	key := sdRegistryKey{t}
	if v, ok := registry.Load(key); ok {
		switch e := v.(type) {
		case *sdFieldMeta:
			return e, nil
		case error:
			return nil, e
		}
		return nil, nil
	}
	sd, err := resolveSd(engine, t)
	if err != nil {
		registry.Store(key, err)
		return nil, err
	}
	if sd == nil {
		return nil, nil // 非 sd 模型不缓存（ormtag.Parse 自带类型缓存，代价可忽略）
	}
	registry.Store(key, sd)
	return sd, nil
}

// resolveSd 计算单个模型类型的软删元数据；非 sd 模型返回 (nil, nil)。
func resolveSd(engine *xorm.Engine, t reflect.Type) (*sdFieldMeta, error) {
	bean := reflect.New(t).Interface()
	meta, err := ormtag.Parse(bean)
	if err != nil {
		return nil, err
	}
	if len(meta.Sd) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(meta.Sd))
	for name := range meta.Sd {
		names = append(names, name)
	}
	sort.Strings(names) // 一模型多个 sd 字段时按字段名排序取第一个（与 gorm 侧一致）
	name := names[0]
	ti, err := engine.TableInfo(bean)
	if err != nil {
		return nil, fmt.Errorf("xormdriver: 解析软删模型 %s 表信息失败: %w", t.Name(), err)
	}
	var col string
	for _, c := range ti.Columns() {
		// 嵌套匿名嵌入（extends 套 extends）时 xorm 的 FieldName 为点分全路径
		// （如 "ModelWithSoftDelete.SoftDelete.DeletedAt"），按叶子后缀匹配。
		if c.FieldName == name || strings.HasSuffix(c.FieldName, "."+name) {
			col = c.Name
			break
		}
	}
	if col == "" {
		return nil, fmt.Errorf("xormdriver: 字段 %q 带 sd 软删标记但不是映射的数据列（被忽略或缺列名），请移除 sd 标记或补充列映射", name)
	}
	return &sdFieldMeta{Column: col, Sd: meta.Sd[name]}, nil
}

// sdAliveCond 存活行过滤条件（Where 直用）：time 模式 IS NULL，其余 = 0。
func (sd *sdFieldMeta) aliveCond() (string, []any) {
	return sd.Sd.AliveCond(sd.Column)
}

// sdAliveBuilderCond 表达式 UPDATE（builder 组装 WHERE）的存活条件。
func (sd *sdFieldMeta) aliveBuilderCond() builder.Cond {
	if sd.Sd.TimeBased {
		return builder.Expr(sd.Column + " IS NULL")
	}
	return builder.Eq{sd.Column: int64(0)}
}

// applySdFilter 在会话上追加存活过滤（buildOpts 统一入口）。
func (q *XormQuery) applySdFilter(s *xorm.Session, dest any) error {
	if q.unscoped {
		return nil
	}
	target := dest
	if target == nil {
		target = q.modelValue
	}
	sd, err := q.sdOfModel(target)
	if err != nil {
		return err
	}
	if sd == nil {
		return nil
	}
	cond, args := sd.aliveCond()
	s.Where(cond, args...)
	return nil
}

// sdTrashedCond 已删行过滤条件（OnlyTrashed）：time 模式 IS NOT NULL、
// flag 模式 = 1、其余 <> 0。builder 与 Where 两种形态。
func (sd *sdFieldMeta) trashedCond() (string, []any) {
	return sd.Sd.TrashedCond(sd.Column)
}
