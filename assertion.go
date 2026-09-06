package xormdriver

import (
	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// 编译期接口断言：保证 XormDriver / XormQuery 与框架契约完全一致。
var (
	_ contracts.Driver = (*XormDriver)(nil)
	_ contracts.Query  = (*XormQuery)(nil)
)
