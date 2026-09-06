package xormdriver

import (
	"context"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	xormlog "xorm.io/xorm/log"
)

// 编译期断言：fastLogger 满足 xorm 引擎要求的 ContextLogger。
var _ xormlog.ContextLogger = (*fastLogger)(nil)

// fastLogger 将 xorm 日志桥接到框架 contracts.Log：
// SQL 执行日志（BeforeSQL/AfterSQL）与 xorm 通用日志（Debugf 等）全部走框架日志器，
// 使 xorm 驱动与其他驱动（gormdriver）的日志格式、级别、输出目标保持一致。
// 字段读写遵循 xorm 内置 SimpleLogger 的无锁约定（SetLevel/ShowSQL 仅启动期配置调用）。
type fastLogger struct {
	log           contracts.Log
	level         xormlog.LogLevel
	slowThreshold time.Duration // <=0 表示不判定慢查询
	showSQL       bool          // false 时 xorm 不再回调 BeforeSQL/AfterSQL
}

// newFastLogger 根据连接配置的 LogLevel 字符串（与 gormdriver 同一套约定：
// error/warn/info/silent，空或其他值兜底 info）创建桥接日志器。
// silent 仍为 LOG_INFO 但关闭 ShowSQL，即只抑制 SQL 执行日志，
// xorm 内部 Errorf 等通用日志仍经框架日志器输出。
func newFastLogger(log contracts.Log, level string, slowThreshold time.Duration) *fastLogger {
	f := &fastLogger{
		log:           log,
		level:         xormlog.LOG_INFO,
		slowThreshold: slowThreshold,
		showSQL:       true,
	}
	switch level {
	case "error":
		f.level = xormlog.LOG_ERR
	case "warn":
		f.level = xormlog.LOG_WARNING
	case "info":
		f.level = xormlog.LOG_INFO
	case "silent":
		f.showSQL = false
	default:
		// 空值/未知值：保持兜底 LOG_INFO
	}
	return f
}

// BeforeSQL xorm 执行 SQL 前回调。LogContext 按值拷贝传入且其中的 start 字段
// 未导出，无法在两次回调间建立关联记录开始时间；ExecuteTime 已由 xorm 在
// hookCtx.End() 中计算完毕并随 AfterSQL 传入，故此处无需任何处理。
func (f *fastLogger) BeforeSQL(ctx xormlog.LogContext) {}

// AfterSQL xorm 执行 SQL 后回调：出错按 Error 输出；超过慢查询阈值按 Warn
// 输出；其余降级为 Debug 输出，避免正常查询刷屏（与 gormdriver 日志策略一致）。
func (f *fastLogger) AfterSQL(ctx xormlog.LogContext) {
	if ctx.Err != nil {
		f.log.Errorf("[xorm] SQL 执行失败: %s - error: %v", ctx.SQL, ctx.Err)
		return
	}
	if f.slowThreshold > 0 && ctx.ExecuteTime > f.slowThreshold {
		f.log.Warnf("[xorm] 慢查询: %s - 耗时 %v", ctx.SQL, ctx.ExecuteTime)
		return
	}
	f.log.Debugf("[xorm] SQL: %s - 耗时 %v", ctx.SQL, ctx.ExecuteTime)
}

// Debugf 桥接到框架日志器，不做级别门控（由框架日志自身过滤）。
func (f *fastLogger) Debugf(format string, v ...any) { f.log.Debugf(format, v...) }

// Errorf 桥接到框架日志器，不做级别门控（由框架日志自身过滤）。
func (f *fastLogger) Errorf(format string, v ...any) { f.log.Errorf(format, v...) }

// Infof 桥接到框架日志器，不做级别门控（由框架日志自身过滤）。
func (f *fastLogger) Infof(format string, v ...any) { f.log.Infof(format, v...) }

// Warnf 桥接到框架日志器，不做级别门控（由框架日志自身过滤）。
func (f *fastLogger) Warnf(format string, v ...any) { f.log.Warnf(format, v...) }

// Level 返回当前级别。
func (f *fastLogger) Level() xormlog.LogLevel { return f.level }

// SetLevel 更新级别。
func (f *fastLogger) SetLevel(l xormlog.LogLevel) { f.level = l }

// ShowSQL 维护 SQL 日志开关（与 xorm 内置日志一致：不传参数默认开启）。
func (f *fastLogger) ShowSQL(show ...bool) {
	if len(show) == 0 {
		f.showSQL = true
		return
	}
	f.showSQL = show[0]
}

// IsShowSQL 返回 SQL 日志开关。
func (f *fastLogger) IsShowSQL() bool { return f.showSQL }

// ── 空日志实现（NewXormDriver nil log 降级用，X-09）───────────────────

// discardLog 丢弃全部输出的 contracts.Log 空实现，With 系返回自身。
type discardLog struct{}

func (discardLog) Debug(args ...any)                 {}
func (discardLog) Debugf(format string, args ...any) {}
func (discardLog) Info(args ...any)                  {}
func (discardLog) Infof(format string, args ...any)  {}
func (discardLog) Warn(args ...any)                  {}
func (discardLog) Warnf(format string, args ...any)  {}
func (discardLog) Error(args ...any)                 {}
func (discardLog) Errorf(format string, args ...any) {}
func (discardLog) Fatal(args ...any)                 {}
func (discardLog) Fatalf(format string, args ...any) {}
func (discardLog) Panic(args ...any)                 {}
func (discardLog) Panicf(format string, args ...any) {}

func (l discardLog) WithField(string, any) contracts.Log       { return l }
func (l discardLog) WithFields(map[string]any) contracts.Log   { return l }
func (l discardLog) WithError(error) contracts.Log             { return l }
func (l discardLog) WithContext(context.Context) contracts.Log { return l }
