# gofast-xorm 原生迁移指南

对应需求文档 `docs/md/gofast-xorm迁移能力需求.md`（stitch-mes 提交）的框架侧落地：
R1 迁移安全模式、R2 DDL 映射对照、R3 批量租户迁移姿势、R4 可观测性（Dry-run /
索引收敛）、R5 双驱动 AutoMigrate 语义差异。

## 1. 迁移安全模式（R1）：pgx 语句缓存 0A000 的根治

**背景**：框架运行时连接统一走 prepared statement（gorm `PrepareStmt=true` /
pgx `cache_statement`）。`ALTER COLUMN TYPE` 后连接内已缓存的执行计划失效，
复用即报 `cached plan must not change result type (0A000)`；若迁移在事务内执行，
报错回滚导致列类型永远改不掉（历史 BUG 见 stitch-mes
`demand/finish/AutoMigrate-style_bom_usage缓存计划0A000.md`）。

gofast-xorm 的 `Sync2` 底层同为 pgx stdlib，默认 exec mode 下存在同一机制，
因此提供**迁移安全模式**：构造驱动时强制 `default_query_exec_mode=exec`——
pgx 全程不使用 prepared statement，连接级语句缓存被根除，0A000 无从触发。

```go
// 方式一：一站式函数（自动 migrate-safe，校验 + Sync2 + 关闭）
err := xormdriver.Migrate(cfg, models...)

// 方式二：自建短生命周期迁移驱动（schema-per-tenant 批量迁移推荐姿势）
drv, err := xormdriver.NewMigrateDriver(cfg, log) // = NewXormDriver + WithMigrateSafe()
if err != nil { ... }
defer drv.Close()
err = drv.AutoMigrate(models...)

// 方式三：选项注入既有构造入口
drv, err := xormdriver.NewXormDriver(cfg, log, xormdriver.WithMigrateSafe())
```

DSN 改写规则（`migrateSafeDSN`，幂等）：

| DSN 形态 | 处理 |
|----------|------|
| `postgres://user:pass@host/db?...`（URL） | url.Parse 后设置 query `default_query_exec_mode=exec` |
| `host=... port=... dbname=...`（keyword） | 剔除既有 `default_query_exec_mode=*` token 后追加 `=exec` |

仅 postgres 引擎生效；其他引擎降级为 no-op 并记 Info 日志。已固化为
`migrate_integration_test.go` 的 `TestPGXormMigrate_SafeModeAlterColumn`
（建 varchar(16) → 暖机查询 → Sync2 改 varchar(32) → 同连接复查，断言无 0A000）。

## 2. orm/ext 元数据 → DDL 映射对照（R2）

gorm 路径为 `PatchSchema`（orm/ext tag patch 进 gorm schema 缓存）+ `AutoMigrate`；
xorm 路径为 tag_identifier=orm 时 xorm 原生解析 orm tag + `Sync2`。
两路径产物对照（✓=一致；⚠=落库产物有差异，属白名单已知项）：

| orm/ext 元数据 | gorm PatchSchema 路径 | xorm Sync2 路径 | 结论 |
|----------------|----------------------|-----------------|------|
| `orm:"pk"` string 主键 | 单列 PRIMARY KEY | 同左 | ✓ |
| `orm:"varchar(n) notnull default('x')"` | `varchar(n) NOT NULL DEFAULT 'x'` | 同左 | ✓ |
| `orm:"created"/"updated"`（time.Time） | `timestamp with time zone`（gorm pg 默认 timestamptz） | `timestamp without time zone`（xorm pg 默认） | ⚠ 见下 |
| `orm:"index"` 普通索引 | `idx_<table>_<col>` | `IDX_<table>_<col>`（XName 规则） | ⚠ 仅命名 |
| `orm:"index(grp)"` 复合索引 | `idx_<table>_<grp>`（小写 idx 前缀） | `IDX_<table>_<grp>`（大写） | ⚠ 仅命名 |
| `orm:"unique"`/`unique(name)` | 唯一约束 `uni_<table>_<col>` / 唯一索引 `<name>` | 唯一索引 `UQE_<table>_<col>` / `UQE_<table>_<name>` | ⚠ 仅命名 |
| `orm:"jsonb"` 列 | `jsonb` | `jsonb` | ✓ |
| `orm:"extends"` 嵌入 | 展开 + 前缀 patch | 原生展开 | ✓ |
| `orm:"-"` | 忽略五联（不建列） | 忽略 | ✓ |
| `orm:"comment('备注')"` | 列注释 | 列注释（pg） | ✓ |
| ext `check(...)` | CHECK 约束 | 不支持（启动期 Warn，见 ormtag_check.go） | ⚠ xorm 侧缺失 |
| ext `migration:false` | `IgnoreMigration`（不动列） | 不支持（Warn） | ⚠ 用 `orm:"-"` 或手写 DDL 补充 |

**白名单说明**：索引/约束命名差异（`idx_` vs `IDX_`、`uni_` vs `UQE_`）与时区
差异（`timestamptz` vs `timestamp`）属结构性已知差异——老库（gorm 建成）切 xorm
Sync2 后首次迁移会产生对应的 RENAME/ALTER，属预期内无害 diff（验收标准 2 的
"预期无害 no-op"白名单）。ext 的 check/migration:false 在 xorm 侧不支持，
需要 CHECK 的列请用手写 SQL 补充 DDL。

**列差异收敛（v1.0.3 新增）**：Sync2 不会修改已存在列的类型/长度（同名即跳过，
见 §4.3），`AutoMigrate` 在 Sync2 后追加列收敛 Pass——比对
information_schema 与模型定义，类型/长度/精度/NOT NULL 差异自动生成
`ALTER TABLE ... ALTER/MODIFY COLUMN` 收敛（pg/mysql/mssql 三引擎）；**默认值
与注释差异不收敛**（不重建列，避免数据风险），仍属白名单已知项。

xorm 路径期望产物以 `migrate_integration_test.go` 的
`TestPGXormMigrate_DDLEquivalence`（information_schema 断言）为事实源。

## 3. SaaS schema-per-tenant 批量迁移（R3）

受支持用法（非逃生口）：

```go
for _, schema := range tenantSchemas {
    cfg := BuildTenantPgConfig(schema)            // ConnectionConfig{Engine:"postgres", Schema:schema, TagIdentifier:"orm", ...}
    if err := xormdriver.Migrate(cfg, models...); err != nil { ... }
    // 补充 DDL（TimescaleDB hypertable、ext check 等手写 SQL）
}
```

schema 切换语义（DDL 与 DML 落点一致性）：

- **DDL**：`NewXormDriver` 内 `engine.SetSchema(cfg.Schema)`——xorm postgres 方言的
  建表/改列/索引以 `URI().Schema` 限定表名（事务内 `SET LOCAL search_path`
  改变不了它的落点，driver.go 注释的 U18 回归结论）；`AutoMigrate` 同时在
  事务内 `SET LOCAL search_path` 兜底序列等 search_path 敏感对象。
- **DML**：查询层按 dest 推导表名后拼 `schema.` 前缀（`schemaTable`，xormdriver.go）。
- 批量迁移组合行为：每条租户 schema 用**独立短生命周期驱动**（`Migrate`/
  `NewMigrateDriver`），DDL 经 `SetSchema` 精确落点，迁移完即 `Close`，
  不与运行时连接的 search_path / 语句缓存互相污染。
- **连接池 ≥2**：schema 模式下 Sync2 在事务内运行，其内部元数据加载需从池内
  取第二条连接，单连接池会死锁（`TestPGXormMigrate_SafeModeAlterColumn` 实测确认；
  默认池 100 无此问题，自建迁移驱动若调小池需留意）。

## 4. 迁移可观测性（R4）

### 4.1 Dry-run / Diff 预览

```go
ddls, err := xormdriver.MigrateSQL(cfg, models...) // []string，待执行 DDL 列表
```

引擎策略（Dry-run 语义一致：目标库结构全程零变化）：

| 引擎 | 实现 | 说明 |
|------|------|------|
| postgres / mssql | capture 驱动 + 事务回滚 | DDL 支持事务：事务内执行 Sync2，记录语句后整体回滚 |
| mysql | 临时库克隆 | DDL 隐式提交无法回滚：目标库基表克隆到 `_gofast_mig_preview_<ts>` 执行 Sync2，捕获后 DROP 临时库（需 CREATE/DROP DATABASE 权限；只克隆 BASE TABLE，返回 DDL 中临时库名已还原为目标库名） |
| sqlite | 不支持 | 返回明确错误 |

已固化：`TestPGXormMigrate_MigrateSQLDryRun` / `TestMySQLXormMigrate_MigrateSQLClone` /
`TestMSSQLXormMigrate_MigrateSQLDryRun`（有 diff 返回 ALTER 且结构未变；无 diff 返回空）。

### 4.2 索引收敛

**Sync2 行为**：按索引名判存在（`IsIndexExist`），同名即跳过、**不校验定义**——
与 gorm AutoMigrate 同款陷阱：同名异构索引（列集合/顺序不同）永远不会被修正。

工具链（CheckIndexes / WithRebuildIndexes 支持 postgres / mysql / mssql）：

```go
// 只读体检：missing（期望索引不存在）/ mismatched（同名异构）
mis, err := xormdriver.CheckIndexes(cfg, models...)

// 收敛迁移：AutoMigrate 在 Sync2 前（事务外）自动 DROP 异构同名索引，
// 再由 Sync2 按模型定义重建
drv, err := xormdriver.NewXormDriver(cfg, log,
    xormdriver.WithMigrateSafe(), xormdriver.WithRebuildIndexes())
err = drv.AutoMigrate(models...)
```

**DROP 时机**：DROP INDEX 必须在 Sync2 事务外自动提交。pg 实测死锁场景——
事务内 DROP INDEX 持有 ACCESS EXCLUSIVE，Sync2 内部经 `pg_get_indexdef`
（pg_indexes 视图）读索引定义需 ACCESS SHARE，跨连接互相等待（连接池 ≥2 时）。
原子性取舍：DROP 与 Sync2 建索引之间崩溃会短暂缺索引（下次迁移自动补建），
换取收敛流程不死锁。

期望定义取自 `engine.TableInfo` 并按 xorm `CreateIndexSQL` 的 XName 规则还原实际
落库名（`IDX_<table>_<name>` / `UQE_<table>_<name>`；已带 `IDX_`/`UQE_` 前缀的名
字原样）。已固化 `TestPGXormMigrate_IndexConvergence`（异构→收敛→missing→补建）。

### 4.3 列类型收敛（Sync2 不改已有列类型）

**Sync2 行为**：列按名比对，同名列的类型/长度变化**静默跳过**（xorm v1.4.1
`columnTypesMatch` 只按基类型判等，`VARCHAR(16)` vs `VARCHAR(32)` 视为一致）——
改列长度/精度/NOT NULL 永远不会生效。

`AutoMigrate` 的列收敛 Pass（migrate_converge.go，pg/mysql/mssql 三引擎）：

1. 读 `information_schema.columns` 实际定义（data_type / character_maximum_length /
   numeric_precision / numeric_scale / is_nullable）；
2. 与模型期望定义逐列比对，差异生成方言 ALTER：
   - pg：`ALTER TABLE s.t ALTER COLUMN c TYPE <sqltype>` + `SET/DROP NOT NULL`；
   - mysql：`ALTER TABLE t MODIFY COLUMN <ColumnString 完整列定义>`；
   - mssql：`ALTER TABLE [t] ALTER COLUMN [c] <sqltype> NULL/NOT NULL`；
3. 语句在 Sync2 同一事务内执行（Dry-run 模式下被 capture 记录后随回滚不落库）。

**收敛范围**：类型/长度/精度/NOT NULL；**默认值与注释差异不收敛**（避免重建列
的数据风险，见 §2 白名单）。MigrateSQL 同样包含收敛语句（Dry-run 可见全部
待执行 DDL）。

## 5. contracts.Driver.AutoMigrate 双驱动语义差异（R5）

| 维度 | gormdriver | xormdriver |
|------|-----------|------------|
| orm tag 生效方式 | PatchSchema patch 进 gorm schema 缓存（连接级共享） | tag_identifier=orm 引擎原生解析 |
| 迁移事务性 | 默认非事务（依赖 migrator 逐条提交） | schema 非空时整体包事务 + SET LOCAL search_path |
| 列类型变更 | MigrateColumn，受 prepared statement 缓存影响（0A000 历史 BUG） | Sync2 + 迁移安全模式根治（§1） |
| 索引存在判断 | 按名判存在、同名跳过不校验定义 | 同左（提供 CheckIndexes/WithRebuildIndexes，§4.2） |
| 启动期校验 | patch 时 Parse 报错 | validateOrmTags：禁用/未知 token 报错，ext 不支持项 Warn |
| ext 能力 | check / migration:false / timePrecision / perm 支持 | 不支持（Warn 不报错，见 ormtag_check.go） |

**目标态（stitch-mes 侧动作）**：迁移入口改为 `xormdriver.Migrate` /
`NewMigrateDriver` 后，`go.mod` 可完整移除 `gofast-gorm` 与 `gorm.io/*`；
补充 DDL（hypertable、ext check）保留手写 SQL。

## 6. 运行集成测试

```bash
GOFAST_TEST_PG_DSN="postgres://user:pass@host:5432/db?sslmode=disable" \
  go test -tags integration -run PGXormMigrate ./
GOFAST_TEST_MYSQL_DSN="user:pass@tcp(host:3306)/" \
  go test -tags integration -run MySQLXormMigrate ./
GOFAST_TEST_MSSQL_DSN="server=host;port=1433;user id=sa;password=..." \
  go test -tags integration -run MSSQLXormMigrate ./
```
