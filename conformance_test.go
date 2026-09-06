package xormdriver

// conformance_test.go 双驱动一致性套件接入（orm-tag-design.md §11.1/§11.17 U17）。
//
// 工厂构造：newXormTestDriverWithIdentifier(t, "orm")——SetTagIdentifier("orm")
// 使 xorm 原生消费统一 orm tag；日志器替换为 SQL 计数实现（engine.SetLogger →
// core.DB Logger 钩子，IsShowSQL=true 保证 BeforeSQL/AfterSQL 回调），供套件
// QueryCounter 非 N+1 断言（§11.5）。

import (
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/database/drivertest"

	xormlog "xorm.io/xorm/log"
)

// countingXormLogger xorm 计数日志器：每次 SQL 执行（AfterSQL）计数一次，
// 其余静默。
type countingXormLogger struct {
	n int64
}

var _ xormlog.ContextLogger = (*countingXormLogger)(nil)

func (l *countingXormLogger) BeforeSQL(xormlog.LogContext) {}

func (l *countingXormLogger) AfterSQL(ctx xormlog.LogContext) {
	if ctx.Err != nil {
		return // 失败 SQL 不计入查询次数（与 gorm 侧 Trace 计数口径对齐）
	}
	l.n++
}

func (l *countingXormLogger) Debugf(string, ...any) {}
func (l *countingXormLogger) Errorf(string, ...any) {}
func (l *countingXormLogger) Infof(string, ...any)  {}
func (l *countingXormLogger) Warnf(string, ...any)  {}

func (l *countingXormLogger) Level() xormlog.LogLevel   { return xormlog.LOG_ERR }
func (l *countingXormLogger) SetLevel(xormlog.LogLevel) {}

// ShowSQL/IsShowSQL：需保持 true，core.DB 的 beforeProcess/afterProcess 钩子
// 才会回调 BeforeSQL/AfterSQL（计数依赖）。
func (l *countingXormLogger) ShowSQL(show ...bool) {}
func (l *countingXormLogger) IsShowSQL() bool      { return true }

// conformanceXormDriver 套件驱动：注入 *XormDriver + QueryCounter 能力。
type conformanceXormDriver struct {
	*XormDriver
	logger *countingXormLogger
}

// QueryCount 实现 drivertest.QueryCounter（§11.5 SQL 计数断言）。
func (d *conformanceXormDriver) QueryCount() int { return int(d.logger.n) }

// newXormConformanceDriver xorm 侧套件工厂：identifier=orm + 计数日志器。
func newXormConformanceDriver(t *testing.T) contracts.Driver {
	t.Helper()
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	logger := &countingXormLogger{}
	drv.engine.SetLogger(logger)
	t.Cleanup(func() { _ = drv.Close() })
	return &conformanceXormDriver{XormDriver: drv, logger: logger}
}

// TestConformance 双驱动共享一致性套件（orm-tag-design.md §11.1）。
func TestConformance(t *testing.T) {
	drivertest.RunSuite(t, newXormConformanceDriver)
}
