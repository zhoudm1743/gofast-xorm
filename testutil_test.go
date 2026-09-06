package xormdriver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"

	"xorm.io/xorm"
)

// newXormTestDriver 创建内存 SQLite 测试驱动（"sqlite" 驱动名由包内 imports.go
// blank import 注册）。测试内不得自定义驱动构造，统一走此入口。
func newXormTestDriver(t *testing.T) *XormDriver {
	t.Helper()
	engine, err := xorm.NewEngine("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	return &XormDriver{engine: engine}
}

// newXormTestDriverWithIdentifier 创建指定 tag_identifier 的内存 SQLite 测试驱动
// （统一 orm tag 体系 U5/U7 基建）：identifier 非 "xorm"/"" 时按 NewXormDriver
// 的配置链路调用 engine.SetTagIdentifier 切换引擎读取的 tag 键，并挂接捕获型
// 日志器，返回驱动与其日志捕获器（供启动期告警断言）。
func newXormTestDriverWithIdentifier(t *testing.T, identifier string) (*XormDriver, *captureLog) {
	t.Helper()
	engine, err := xorm.NewEngine("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	if identifier != "" && identifier != "xorm" {
		engine.SetTagIdentifier(identifier)
	}
	log := newCaptureLog()
	return &XormDriver{engine: engine, tagIdentifier: identifier, log: log}, log
}

// newXormTestDriverWithModel 带普通测试模型表（XormTestModel → xorm_test_model）的驱动。
func newXormTestDriverWithModel(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&XormTestModel{}); err != nil {
		t.Fatalf("自动迁移失败: %v", err)
	}
	return drv
}

// newXormHookDriver 带钩子测试模型表（XormHookModel → xorm_hook_model）的驱动。
func newXormHookDriver(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&XormHookModel{}); err != nil {
		t.Fatalf("自动迁移失败: %v", err)
	}
	return drv
}

// XormTestModel 普通测试模型：xorm SnakeMapper 单数推导表名 xorm_test_model
// （与 gorm NamingStrategy 的复数形式不同）。
type XormTestModel struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`
}

func (m *XormTestModel) AutoGenerateID() {
	// 测试中手动设置 ID
}

// XormHookModel 钩子测试模型：实现 contracts 全部 7 个 On* 钩子，用 Called
// 标记位验证钩子触发时机（标记列由 Sync2 一并建出，不影响语义）。
type XormHookModel struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(100) 'name'"`

	BeforeCreateCalled bool
	AfterCreateCalled  bool
	BeforeUpdateCalled bool
	AfterUpdateCalled  bool
	BeforeDeleteCalled bool
	AfterDeleteCalled  bool
	AfterFindCalled    bool
}

func (m *XormHookModel) AutoGenerateID() {
	// 测试中手动设置 ID
}

func (m *XormHookModel) OnBeforeCreate(q contracts.Query) error {
	m.BeforeCreateCalled = true
	return nil
}

func (m *XormHookModel) OnAfterCreate(q contracts.Query) error {
	m.AfterCreateCalled = true
	return nil
}

func (m *XormHookModel) OnBeforeUpdate(q contracts.Query) error {
	m.BeforeUpdateCalled = true
	return nil
}

func (m *XormHookModel) OnAfterUpdate(q contracts.Query) error {
	m.AfterUpdateCalled = true
	return nil
}

func (m *XormHookModel) OnBeforeDelete(q contracts.Query) error {
	m.BeforeDeleteCalled = true
	return nil
}

func (m *XormHookModel) OnAfterDelete(q contracts.Query) error {
	m.AfterDeleteCalled = true
	return nil
}

func (m *XormHookModel) OnAfterFind(q contracts.Query) error {
	m.AfterFindCalled = true
	return nil
}

// ── Schema 显式表名不被 dest 覆盖回归基建 ─────────────────────────────
// 回归缺陷：schema 模式下若按 dest 结构体推导表名无条件覆盖调用方显式设置的
// Table()/Model()，投影结构体（推导表名与实际表不一致）场景会静默查错表。
// xorm SnakeMapper 单数：userRow → user_row、tenantUser → tenant_user
//（gorm NamingStrategy 复数为 user_rows/tenant_users，诱饵表名相应不同）。

// userRow 投影结构体：SnakeMapper 推导表名 user_row，恰为诱饵表名，
// 与显式 Table("users") 不一致（缺陷报告同款场景）。
type userRow struct {
	ID   string
	Name string
}

// tablerUserRow 实现 xorm names.TableName 接口返回裸表名 "users"：
// 引擎对该接口优先于 mapper 推导，依赖驱动按 dest 兜底拼 schema 前缀。
type tablerUserRow struct {
	ID   string
	Name string
}

func (tablerUserRow) TableName() string { return "users" }

// tenantUser 普通模型：SnakeMapper 推导表名 tenant_user。
type tenantUser struct {
	ID   string
	Name string
}

// newXormTenantDriver 打开内存 SQLite 并 ATTACH 独立租户 schema tenant1。
// tenant1.users / tenant1.tenant_user 存正确数据，tenant1.user_row 存诱饵数据，
// 用于捕获最危险的失败模式——不报错但静默查错表。
// ATTACH 是连接级状态，必须限制连接池单连接，避免查询落到未 ATTACH 的连接上。
// engine.DB() 返回内嵌 *sql.DB 的 *core.DB（单返回值），可直接设置连接池。
func newXormTenantDriver(t *testing.T) *XormDriver {
	t.Helper()
	engine, err := xorm.NewEngine("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	engine.DB().SetMaxOpenConns(1)
	// engine.Exec 一次性执行原生 SQL（内部 session 自动关闭），适合 ATTACH/DDL/DML
	for _, ddl := range []string{
		`ATTACH DATABASE ':memory:' AS tenant1`,
		`CREATE TABLE tenant1.users (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE tenant1.user_row (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE tenant1.tenant_user (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO tenant1.users VALUES ('u1', 'right-table')`,
		`INSERT INTO tenant1.user_row VALUES ('u1', 'wrong-table')`,
		`INSERT INTO tenant1.tenant_user VALUES ('t1', 'right-table')`,
	} {
		if _, err := engine.Exec(ddl); err != nil {
			t.Fatalf("初始化租户 schema 失败 %q: %v", ddl, err)
		}
	}
	return &XormDriver{engine: engine}
}

// countRaw 原生 SQL 计数（contracts.Query 的 Raw/Scan 链路），供回归测试断言各表行数。
func countRaw(t *testing.T, drv *XormDriver, sql string) int64 {
	t.Helper()
	var n int64
	if err := drv.Query().Raw(sql).Scan(&n); err != nil {
		t.Fatalf("countRaw %q: %v", sql, err)
	}
	return n
}

// ── 捕获型日志器（orm tag 启动期校验告警断言用）──────────────────────

// captureLog 测试用 contracts.Log：按级别缓冲格式化后的日志文本，
// 供 AutoMigrate 启动期校验（ormtag_check.go）的 Warn 断言使用。
// xorm 引擎日志桥接（fastLogger）也会并发写入，故加互斥保护。
type captureLog struct {
	mu    sync.Mutex
	lines map[string][]string // 级别（debug/info/warn/error/...）→ 消息列表
}

func newCaptureLog() *captureLog {
	return &captureLog{lines: make(map[string][]string)}
}

func (l *captureLog) append(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines[level] = append(l.lines[level], msg)
}

// has 判断指定级别的日志中是否存在包含 substr 的消息。
func (l *captureLog) has(level, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines[level] {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func (l *captureLog) Debug(args ...any)                         { l.append("debug", fmt.Sprint(args...)) }
func (l *captureLog) Debugf(format string, a ...any)            { l.append("debug", fmt.Sprintf(format, a...)) }
func (l *captureLog) Info(args ...any)                          { l.append("info", fmt.Sprint(args...)) }
func (l *captureLog) Infof(format string, a ...any)             { l.append("info", fmt.Sprintf(format, a...)) }
func (l *captureLog) Warn(args ...any)                          { l.append("warn", fmt.Sprint(args...)) }
func (l *captureLog) Warnf(format string, a ...any)             { l.append("warn", fmt.Sprintf(format, a...)) }
func (l *captureLog) Error(args ...any)                         { l.append("error", fmt.Sprint(args...)) }
func (l *captureLog) Errorf(format string, a ...any)            { l.append("error", fmt.Sprintf(format, a...)) }
func (l *captureLog) Fatal(args ...any)                         { l.append("fatal", fmt.Sprint(args...)) }
func (l *captureLog) Fatalf(format string, a ...any)            { l.append("fatal", fmt.Sprintf(format, a...)) }
func (l *captureLog) Panic(args ...any)                         { l.append("panic", fmt.Sprint(args...)) }
func (l *captureLog) Panicf(format string, a ...any)            { l.append("panic", fmt.Sprintf(format, a...)) }
func (l *captureLog) WithField(string, any) contracts.Log       { return l }
func (l *captureLog) WithFields(map[string]any) contracts.Log   { return l }
func (l *captureLog) WithError(error) contracts.Log             { return l }
func (l *captureLog) WithContext(context.Context) contracts.Log { return l }
