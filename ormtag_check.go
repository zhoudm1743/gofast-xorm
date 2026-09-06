package xormdriver

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/zhoudm1743/go-fast-framework/contracts/ormtag"
)

// ── AutoMigrate 启动期 orm tag 校验（orm-tag-design.md §8.4/§10.3）─────
//
// xorm 引擎对 orm tag 的原生解析是"宽容"的：未加引号的未知裸 token 会被静默
// 当作列名建出垃圾列，禁用 token（cache/nocache/deleted）会与框架缓存/软删除
// 体系冲突且不报错。本文件在 Sync2 之前统一拦截：
//
//  1. ormtag.Parse 全量校验：禁用 token、未知裸 token、非法语法、rel 复合键
//     不等长、循环嵌入等错误向上返回——启动即失败，不等 xorm 静默建列；
//  2. ext tag 降级：xorm 不支持的 gorm 专属能力（check/索引高级选项/自增步长/
//     migration:false/timePrecision/perm）识别后 Warn 日志，绝不报错——同一模型
//     在双驱动连接下均可启动，行为差异以日志固化（§8.4）；
//  3. 迁移期陷阱告警（§10.3）：连接 identifier 仍为 "xorm" 而模型带 orm tag 时
//     Warn 提示——xorm 读不到 orm tag 将静默退回命名约定推导（主键/列名/时间戳
//     可能全错）。

// validateOrmTags 遍历 AutoMigrate 的模型执行启动期校验（先于 Sync2）。
func (d *XormDriver) validateOrmTags(models []any) error {
	ident := d.tagIdentifier
	if ident == "" {
		ident = "xorm" // 直接构造 XormDriver 的场景（测试/内嵌）与空配置同义
	}
	for _, m := range models {
		if m == nil {
			continue
		}
		meta, err := ormtag.Parse(m)
		if err != nil {
			return fmt.Errorf("xormdriver: AutoMigrate 模型 %T orm tag 校验失败: %w", m, err)
		}
		if d.log != nil {
			d.warnUnsupportedExts(meta)
			if ident == "xorm" && structHasOrmTag(meta.Type) {
				d.log.Warnf("[xormdriver] 迁移期陷阱（orm-tag-design.md §10.3）: 模型 %s 声明了 orm tag，"+
					"但当前连接 tag_identifier 为 %q——orm tag 不会被 xorm 读取，将静默退回命名约定推导"+
					"（主键/列名/时间戳可能全错）；请在连接配置设置 tag_identifier: orm，或保留模型上的 xorm tag",
					meta.Type.Name(), ident)
			}
			// 反向陷阱（§10.3 陷阱 1）：已切 orm 连接但模型只有 xorm tag——
			// xorm 读不到 orm tag，同样静默退回约定推导
			if ident != "xorm" && structHasXormTagOnly(meta.Type) {
				d.log.Warnf("[xormdriver] 迁移期陷阱（orm-tag-design.md §10.3）: 模型 %s 只有 xorm tag 未声明 orm tag，"+
					"但当前连接 tag_identifier 为 %q——该模型的 xorm tag 不生效，将静默退回命名约定推导"+
					"（主键/列名/时间戳可能全错）；请为模型追加 orm tag（值可与 xorm tag 相同）",
					meta.Type.Name(), ident)
			}
		}
	}
	return nil
}

// warnUnsupportedExts ext tag 中 xorm 不支持项的降级告警（§8.4：Warn/Debug，
// 绝不报错中断）。逐 key 独立判定，一条 tag 携带多个不支持项时各告警一次。
func (d *XormDriver) warnUnsupportedExts(meta *ormtag.ModelMeta) {
	for fieldName, em := range meta.Exts {
		// check：CHECK 约束表达式——xorm Sync2 不生成 CHECK（SQLite/多方言差异），
		// 写入非法值不会失败，该差异由测试固化、由业务层校验兜底。
		if em.Check != "" {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"check:...\" 在 xorm 连接下不生效"+
				"（xorm Sync2 不支持 CHECK 约束），请改用 gormdriver 连接或在业务层校验", meta.Type.Name(), fieldName)
		}
		// index 高级选项：仅 orm token 的 index/unique 生效，gorm 风格选项串被忽略
		if em.IndexOptions != "" {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"index:...\" 高级选项在 xorm 连接下不生效"+
				"（xorm 仅支持 orm tag 的 index/unique token），索引需求请改写为 orm tag 的 index/unique",
				meta.Type.Name(), fieldName)
		}
		// 自增步长：MySQL 专属 DDL 能力，xorm 无对应 tag
		if em.AutoIncrementIncrement != 0 {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"autoIncrementIncrement\" 在 xorm 连接下不生效"+
				"（xorm 不支持自增步长 DDL）", meta.Type.Name(), fieldName)
		}
		// migration:false：xorm 侧无"参与读写但不建表"的表达，字段仍会被 Sync2 建列
		if em.IgnoreMigration {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"migration:false\" 在 xorm 连接下不生效"+
				"（Sync2 仍会为该字段建列），如需不建列请改用 orm:\"-\"", meta.Type.Name(), fieldName)
		}
		// 时间精度：xorm created/updated 精度由方言决定，无法按字段指定
		if em.TimePrecision != "" {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"timePrecision\" 在 xorm 连接下不生效"+
				"（xorm created/updated 精度由方言决定）", meta.Type.Name(), fieldName)
		}
		// 写权限限制：由消费方（写路径）自行执行，DDL/驱动层不拦截
		if em.PermCreateOnly || em.PermUpdateOnly {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext:\"perm:...\" 在 xorm 连接下由业务层自行保证"+
				"（驱动不拦截写路径）", meta.Type.Name(), fieldName)
		}
		// 未知 ext key：本驱动不识别，未来第三驱动可能消费（文档 11.16）
		for _, k := range em.UnknownKeys {
			d.log.Warnf("[xormdriver] 模型 %s 字段 %s 的 ext tag 含未知 key %q，已忽略（不报错）",
				meta.Type.Name(), fieldName, k)
		}
	}
}

// structHasOrmTag 轻量反射扫描模型（含匿名嵌入递归展开）是否声明了 orm tag。
// 仅做 tag 存在性判定，不做全量 Parse（§10.3 陷阱判定用）。
func structHasOrmTag(t reflect.Type) bool {
	return scanTag(t, map[reflect.Type]bool{}, "orm")
}

// structHasXormTagOnly 模型声明了 xorm tag 且未声明 orm tag（§10.3 陷阱 1 判定）。
func structHasXormTagOnly(t reflect.Type) bool {
	return scanTag(t, map[reflect.Type]bool{}, "xorm") && !structHasOrmTag(t)
}

func scanTag(t reflect.Type, visiting map[reflect.Type]bool, key string) bool {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct || visiting[t] {
		return false
	}
	visiting[t] = true
	defer delete(visiting, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if strings.TrimSpace(f.Tag.Get(key)) != "" {
			return true
		}
		if f.Anonymous {
			if scanTag(f.Type, visiting, key) {
				return true
			}
		}
	}
	return false
}
