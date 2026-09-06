package xormdriver

import (
	"reflect"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 模型钩子调用框架 ─────────────────────────────────────────────────

// walkValues 将 value 展开为可寻址对象指针，对每个元素调用 fn。
// 支持 *T、T、[]T、*[]T、[]*T、*[]*T；切片逐元素执行，非切片单次执行。
// 若 fn 返回错误，立即终止遍历并返回该错误。
func walkValues(value any, fn func(iface any) error) error {
	rv := reflect.ValueOf(value)
	// 解引用外层指针
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Slice:
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			var iface any
			if elem.Kind() == reflect.Ptr {
				if elem.IsNil() {
					continue
				}
				iface = elem.Interface()
			} else if elem.CanAddr() {
				iface = elem.Addr().Interface()
			} else {
				continue
			}
			if err := fn(iface); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if !rv.CanAddr() {
			return nil
		}
		return fn(rv.Addr().Interface())
	default:
		return nil
	}
}

// invokeBeforeCreate 对 value 依次调用 IDAutoGenerator 和 BeforeCreator。
func invokeBeforeCreate(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if ag, ok := iface.(contracts.IDAutoGenerator); ok {
			ag.AutoGenerateID()
		}
		if bc, ok := iface.(contracts.BeforeCreator); ok {
			return bc.OnBeforeCreate(q)
		}
		return nil
	})
}

// invokeAfterCreate 对 value 调用 AfterCreator。
func invokeAfterCreate(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if ac, ok := iface.(contracts.AfterCreator); ok {
			return ac.OnAfterCreate(q)
		}
		return nil
	})
}

// invokeBeforeUpdate 对 value 调用 BeforeUpdater。
func invokeBeforeUpdate(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if bu, ok := iface.(contracts.BeforeUpdater); ok {
			return bu.OnBeforeUpdate(q)
		}
		return nil
	})
}

// invokeAfterUpdate 对 value 调用 AfterUpdater。
func invokeAfterUpdate(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if au, ok := iface.(contracts.AfterUpdater); ok {
			return au.OnAfterUpdate(q)
		}
		return nil
	})
}

// invokeBeforeDelete 对 value 调用 BeforeDeleter。
func invokeBeforeDelete(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if bd, ok := iface.(contracts.BeforeDeleter); ok {
			return bd.OnBeforeDelete(q)
		}
		return nil
	})
}

// invokeAfterDelete 对 value 调用 AfterDeleter。
func invokeAfterDelete(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if ad, ok := iface.(contracts.AfterDeleter); ok {
			return ad.OnAfterDelete(q)
		}
		return nil
	})
}

// invokeAfterFind 对 value 调用 AfterFinder。
func invokeAfterFind(q contracts.Query, value any) error {
	return walkValues(value, func(iface any) error {
		if af, ok := iface.(contracts.AfterFinder); ok {
			return af.OnAfterFind(q)
		}
		return nil
	})
}
