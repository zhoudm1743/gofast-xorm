# gofast-xorm

GoFast 框架的 [xorm](https://xorm.io) 数据库驱动插件。

框架核心 `go-fast-framework` 不再内置 ORM 驱动，需显式安装并注册本插件。

## 安装

```bash
go get github.com/zhoudm1743/gofast-xorm@latest
```

## 接入

```go
import (
    "github.com/zhoudm1743/go-fast-framework/database"
    "github.com/zhoudm1743/go-fast-framework/foundation"
    xormdriver "github.com/zhoudm1743/gofast-xorm"
)

app.SetProviders([]foundation.ServiceProvider{
    // ...
    &database.ServiceProvider{},
    &xormdriver.ServiceProvider{},
})
```

```yaml
database:
  connections:
    main:
      driver: xorm
      engine: mysql   # mysql | postgres | sqlite | mssql
      # ...
```

## 语义说明

- `Model(&bean).Updates(...)` / `Update(col, val)`：bean 主键含非零值时，
  主键等值条件自动并入更新（与链上 `Where` 取 AND 交集），与 gorm 驱动 /
  GORM v2 行为一致。主键全零或无主键时不附加条件，更新范围完全由链上
  `Where` 决定——**无 `Where` 即为全表更新**，批量写请务必显式 `Where`
  或使用 `Table()` 并确认影响范围。
- `Save(value)`：主键全零插入；非零按主键更新（行不存在回落插入）。
  注：MySQL 按"已修改行"计数 affected rows——行存在但写入值无变化时
  `SaveResult.RowsAffected` 为 0（不报错、不误插；gorm 驱动在 MySQL 上
  同为 0，PG 上两驱动均按命中行计 1），需要区分"未命中"时请结合主键
  存在性判断而非只看行数。
- `Exists` / `FirstOrCreate` / `FirstOrInit` 与 `First`/`Last`/`Take`/`Scan`
  口径一致：dest 的非零字段**不会**并入查询条件，查询范围只由链上
  `Where`/conds 决定；`FirstOrCreate` 未命中时按 dest 原值插入。
- `Having` 带参占位符：链上拒绝（`ErrUnsupported`）——gorm 驱动支持，
  双驱动差异固化（fullcov_chain `Having过滤`）。
- `Joins` 非法串：链上报 `ErrUnsupported`——gorm 驱动透传数据库原始错误。
- Save 双分支钩子：空主键走 Create 系钩子、非空主键走 Update 系钩子
  （gorm 驱动统一只触发 Update 系——双驱动差异固化，hooks_integration_test）。
- 超时/连接失败哨兵：statement_timeout(57014)/max_execution_time(3024) →
  `ErrQueryTimeout`；连接失败 → `ErrConnFailed`（fault_integration_test 实测锁定）。

双驱动联合测试方案与执行报告：`../docs/md/dual-driver-test-plan.md`。

## 依赖

- `github.com/zhoudm1743/go-fast-framework` >= v0.8.2
- `xorm.io/xorm` 及各 `database/sql` 驱动

## License

Apache-2.0
