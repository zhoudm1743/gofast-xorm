package xormdriver

// migrate_converge.go 实现"列收敛"Pass（需求 R1/R2 的关键差异消除）：
//
// xorm v1.4.1 Sync2 的列类型比对按基类型判等（columnTypesMatch 对
// VARCHAR(16) 与 VARCHAR(32) 视为相同），不会修改已有列的类型/长度/时区，
// 也不会改 NOT NULL 约束。gofast-xorm 在 Sync2 之后追加本 Pass，按
// information_schema 实际定义与模型期望逐项比对，生成 ALTER 收敛：
//
//   - 列类型/长度/数值精度（information_schema.data_type +
//     character_maximum_length + numeric_precision/scale）
//   - NOT NULL 约束（is_nullable）
//   - 列注释（pg：COMMENT ON COLUMN，仅"模型有注释且与库内不同"时生成，
//     绝不清空已有注释——见 BUG-3 说明；mysql：注释随 MODIFY 渲染，纯注释
//     差异不收敛；mssql：extended_properties 不收敛）
//
// 默认值差异不在本 Pass 收敛（两方言默认值表达式形态差异过大，属文档化
// 白名单，见 docs/migration.md）。支持 postgres / mysql / mssql。
//
// ALTER 语句与 Sync2 在同一事务内执行（schema 模式的 sync2 事务），
// MigrateSQL Dry-run 同样捕获本 Pass 的语句。

import (
	"fmt"
	"strconv"
	"strings"

	"xorm.io/xorm"
	"xorm.io/xorm/dialects"
	"xorm.io/xorm/schemas"
)

// colExpect 列期望（information_schema 口径，小写 data_type）。
type colExpect struct {
	dataType string // 期望 data_type；"" 表示该类型跳过比对（exotic/方言不支持）
	charLen  *int64 // character_maximum_length 期望；nil 不比对
	numPrec  *int64 // numeric_precision 期望；nil 不比对
	numScale *int64 // numeric_scale 期望；nil 不比对
	nullable bool
	comment  string // 期望列注释；空串表示模型未声明
}

// int64Ptr 便捷构造 *int64。
func int64Ptr(v int64) *int64 { return &v }

// infoSchemaTypeNames 各引擎 xorm SQLType 名 → information_schema.data_type
// （小写）。未列出的类型收敛时跳过（exotic：enum/set/自定义 TYPE token 原文等）。
var infoSchemaTypeNames = map[string]map[string]string{
	"pgx": {
		schemas.Varchar:       "character varying",
		schemas.NVarchar:      "character varying",
		schemas.Char:          "character",
		schemas.NChar:         "character",
		schemas.Text:          "text",
		schemas.TinyText:      "text",
		schemas.MediumText:    "text",
		schemas.LongText:      "text",
		schemas.Bool:          "boolean",
		schemas.Boolean:       "boolean",
		schemas.DateTime:      "timestamp without time zone",
		schemas.TimeStamp:     "timestamp without time zone",
		schemas.TimeStampz:    "timestamp with time zone",
		schemas.SmallDateTime: "timestamp without time zone",
		schemas.Date:          "date",
		schemas.Time:          "time without time zone",
		schemas.Int:           "integer",
		schemas.Integer:       "integer",
		schemas.MediumInt:     "integer",
		schemas.Serial:        "integer",
		schemas.BigInt:        "bigint",
		schemas.BigSerial:     "bigint",
		schemas.SmallInt:      "smallint",
		schemas.TinyInt:       "smallint",
		schemas.Float:         "real",
		schemas.Double:        "double precision",
		schemas.Decimal:       "numeric",
		schemas.Numeric:       "numeric",
		schemas.Json:          "json",
		schemas.Jsonb:         "jsonb",
		schemas.Uuid:          "uuid",
		schemas.Bytea:         "bytea",
		schemas.Binary:        "bytea",
		schemas.VarBinary:     "bytea",
		schemas.Blob:          "bytea",
		schemas.TinyBlob:      "bytea",
		schemas.MediumBlob:    "bytea",
		schemas.LongBlob:      "bytea",
	},
	"mysql": {
		schemas.Varchar:    "varchar",
		schemas.NVarchar:   "varchar",
		schemas.Char:       "char",
		schemas.NChar:      "char",
		schemas.Text:       "text",
		schemas.TinyText:   "tinytext",
		schemas.MediumText: "mediumtext",
		schemas.LongText:   "longtext",
		schemas.Bool:       "tinyint", // TINYINT(1)，display width 不参与比对
		schemas.Boolean:    "tinyint",
		schemas.DateTime:   "datetime",
		schemas.TimeStamp:  "timestamp",
		schemas.Date:       "date",
		schemas.Time:       "time",
		schemas.Int:        "int",
		schemas.Integer:    "int",
		schemas.MediumInt:  "mediumint",
		schemas.Serial:     "int",
		schemas.BigInt:     "bigint",
		schemas.BigSerial:  "bigint",
		schemas.SmallInt:   "smallint",
		schemas.TinyInt:    "tinyint",
		schemas.Float:      "float",
		schemas.Double:     "double",
		schemas.Decimal:    "decimal",
		schemas.Numeric:    "decimal",
		schemas.Json:       "json",
		schemas.Jsonb:      "json",
		schemas.Uuid:       "char", // CHAR(36)，长度不参与比对
		schemas.Bytea:      "blob",
		schemas.Blob:       "blob",
		schemas.TinyBlob:   "tinyblob",
		schemas.MediumBlob: "mediumblob",
		schemas.LongBlob:   "longblob",
	},
	"mssql": {
		schemas.Varchar:       "varchar",
		schemas.NVarchar:      "nvarchar",
		schemas.Char:          "char",
		schemas.NChar:         "nchar",
		schemas.Bool:          "bit",
		schemas.Boolean:       "bit",
		schemas.DateTime:      "datetime2",
		schemas.TimeStamp:     "datetime2",
		schemas.SmallDateTime: "datetime2",
		schemas.TimeStampz:    "datetimeoffset",
		schemas.Date:          "date",
		schemas.Time:          "time",
		schemas.Int:           "int",
		schemas.Integer:       "int",
		schemas.Serial:        "int",
		schemas.BigInt:        "bigint",
		schemas.BigSerial:     "bigint",
		schemas.SmallInt:      "smallint",
		schemas.TinyInt:       "tinyint",
		schemas.Float:         "real",
		schemas.Double:        "float",
		schemas.Decimal:       "decimal",
		schemas.Numeric:       "numeric",
		schemas.Uuid:          "varchar", // VARCHAR(40)，长度不参与比对
		schemas.Binary:        "varbinary",
		schemas.VarBinary:     "varbinary",
		schemas.Bytea:         "varbinary",
		schemas.Blob:          "varbinary",
	},
}

// charLenTypes 需要比对 character_maximum_length 的 xorm 类型名。
var charLenTypes = map[string]bool{
	schemas.Varchar: true, schemas.NVarchar: true,
	schemas.Char: true, schemas.NChar: true,
}

// numPrecTypes 需要比对 numeric_precision/numeric_scale 的 xorm 类型名。
var numPrecTypes = map[string]bool{
	schemas.Decimal: true, schemas.Numeric: true,
}

// expectColumn 计算单列期望（information_schema 口径）。
func expectColumn(engineName string, col *schemas.Column) colExpect {
	exp := colExpect{nullable: col.Nullable, comment: col.Comment}
	dataType, ok := infoSchemaTypeNames[engineName][col.SQLType.Name]
	if !ok {
		return exp // 未知类型：跳过类型比对（仍比对 nullable）
	}
	exp.dataType = dataType
	if charLenTypes[col.SQLType.Name] && col.Length > 0 {
		exp.charLen = int64Ptr(col.Length)
	}
	if numPrecTypes[col.SQLType.Name] {
		if col.Length > 0 {
			exp.numPrec = int64Ptr(col.Length)
		}
		exp.numScale = int64Ptr(col.Length2)
	}
	return exp
}

// actualColumn information_schema 口径的实际列定义。
type actualColumn struct {
	dataType string
	charLen  *int64
	numPrec  *int64
	numScale *int64
	nullable bool
	comment  string // 库内实际列注释（pg 经 pg_description；mysql 经 column_comment；mssql 恒空）
}

// convergeStatements 计算 models 的列收敛 ALTER 语句列表（不含执行）。
// engineName 为 xorm 驱动名（pgx/mysql/mssql）；schema 限定目标 schema/库。
func convergeStatements(e *xorm.Engine, engineName, schema string, models []any) ([]string, error) {
	if engineName != "pgx" && engineName != "mysql" && engineName != "mssql" {
		return nil, nil // sqlite 等：收敛不适用
	}
	var stmts []string
	for _, m := range models {
		bean := tableBeanOf(m)
		if bean == nil {
			continue
		}
		t, err := e.TableInfo(bean)
		if err != nil {
			return nil, fmt.Errorf("xormdriver: 收敛解析模型 %T 失败: %w", m, err)
		}
		tableName := t.Name
		if idx := strings.LastIndex(tableName, "."); idx >= 0 {
			tableName = tableName[idx+1:]
		}
		actual, err := loadActualColumns(e, engineName, schema, tableName)
		if err != nil {
			return nil, err
		}
		for _, col := range t.Columns() {
			act, ok := actual[col.Name]
			if !ok {
				continue // 新列由 Sync2 创建，无需收敛
			}
			stmts = append(stmts, buildConvergeStmts(e, engineName, schema, tableName, col, act)...)
		}
	}
	return stmts, nil
}

// loadActualColumns 读取目标表实际列定义（information_schema）。
// 占位符统一用 `?`：go-mssqldb 在 Prepare 时经 querytext.ParseParams 转换，
// pgx stdlib / go-sql-driver 原生认 `?`（xorm 不重绑定占位符）。
func loadActualColumns(e *xorm.Engine, engineName, schema, table string) (map[string]actualColumn, error) {
	query := `SELECT column_name, data_type, character_maximum_length, numeric_precision, numeric_scale, is_nullable`
	if engineName == "mysql" {
		// mysql 列注释随 information_schema.columns 主查询带出。
		query += `, column_comment`
	}
	query += `
		FROM information_schema.columns WHERE table_name = ?`
	var args []any
	switch engineName {
	case "pgx":
		if schema != "" {
			query += ` AND table_schema = ?`
			args = []any{table, schema}
		} else {
			query += ` AND table_schema = current_schema()`
			args = []any{table}
		}
	case "mysql":
		query += ` AND table_schema = DATABASE()`
		args = []any{table}
	case "mssql":
		if schema != "" {
			query += ` AND table_schema = ?`
			args = []any{table, schema}
		} else {
			query += ` AND table_schema = SCHEMA_NAME()`
			args = []any{table}
		}
	}
	rows, err := e.Query(append([]any{query}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("xormdriver: 读取 information_schema.columns 失败: %w", err)
	}
	out := make(map[string]actualColumn, len(rows))
	for _, row := range rows {
		out[strings.ToLower(string(row["column_name"]))] = actualColumn{
			dataType: strings.ToLower(string(row["data_type"])),
			charLen:  bytesToInt64Ptr(row["character_maximum_length"]),
			numPrec:  bytesToInt64Ptr(row["numeric_precision"]),
			numScale: bytesToInt64Ptr(row["numeric_scale"]),
			nullable: string(row["is_nullable"]) == "YES",
			comment:  string(row["column_comment"]), // mysql 带出；pg/mssql 为 ""
		}
	}
	// 列注释（pg：information_schema 无注释列，单独走 pg_description；
	// mysql：column_comment 随主查询带出；mssql：不收敛注释）。
	if engineName == "pgx" {
		comments, err := loadActualComments(e, engineName, schema, table)
		if err != nil {
			return nil, err
		}
		for k, c := range comments {
			if ac, ok := out[k]; ok {
				ac.comment = c
				out[k] = ac
			}
		}
	}
	return out, nil
}

// loadActualComments 读取目标表实际列注释（小写列名 → 注释；无注释列缺省 ""）。
// pg：pg_attribute + pg_description（schema 为空取 current_schema()）；
// mysql：information_schema.columns.column_comment；mssql 返回空表（不支持）。
func loadActualComments(e *xorm.Engine, engineName, schema, table string) (map[string]string, error) {
	out := map[string]string{}
	switch engineName {
	case "pgx":
		var (
			rows []map[string][]byte
			err  error
		)
		if schema != "" {
			rows, err = e.Query(
				`SELECT a.attname AS column_name, d.description AS comment
				 FROM pg_attribute a
				 JOIN pg_class c ON a.attrelid = c.oid
				 JOIN pg_namespace n ON c.relnamespace = n.oid
				 LEFT JOIN pg_description d ON d.objoid = c.oid AND d.objsubid = a.attnum
				 WHERE n.nspname = ? AND c.relname = ? AND a.attnum > 0 AND NOT a.attisdropped`,
				schema, table)
		} else {
			rows, err = e.Query(
				`SELECT a.attname AS column_name, d.description AS comment
				 FROM pg_attribute a
				 JOIN pg_class c ON a.attrelid = c.oid
				 LEFT JOIN pg_description d ON d.objoid = c.oid AND d.objsubid = a.attnum
				 WHERE c.relname = ? AND a.attnum > 0 AND NOT a.attisdropped
				   AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`,
				table)
		}
		if err != nil {
			return nil, fmt.Errorf("xormdriver: 读取 pg_description 列注释失败: %w", err)
		}
		for _, row := range rows {
			out[strings.ToLower(string(row["column_name"]))] = string(row["comment"])
		}
	case "mysql":
		rows, err := e.Query(
			`SELECT column_name, column_comment FROM information_schema.columns
			 WHERE table_schema = DATABASE() AND table_name = ?`, table)
		if err != nil {
			return nil, fmt.Errorf("xormdriver: 读取 column_comment 失败: %w", err)
		}
		for _, row := range rows {
			out[strings.ToLower(string(row["column_name"]))] = string(row["column_comment"])
		}
	}
	return out, nil
}

// bytesToInt64Ptr NULL（nil）→ nil；其余解析 int64。
func bytesToInt64Ptr(b []byte) *int64 {
	if b == nil {
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// equalInt64Ptr 值比对（nil 视为"不参与"由调用方保证）。
func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

// buildConvergeStmts 单列差异 → ALTER 语句（方言相关）。
// 注释差异：仅"模型有注释且与库内不同"时收敛（pg 生成 COMMENT ON COLUMN；
// mysql 走 MODIFY COLUMN 由 ColumnString 渲染注释）；模型无注释而库内有
// 一律不动——绝不生成清空注释的语句（BUG-3，stitch-mes 实测：Sync2 的
// ModifyColumnSQL 附 COMMENT IS ” 会清空 gorm/手工 SQL 建的 700 条注释）。
func buildConvergeStmts(e *xorm.Engine, engineName, schema, table string, col *schemas.Column, act actualColumn) []string {
	exp := expectColumn(engineName, col)
	typeDiff := exp.dataType != "" && exp.dataType != act.dataType
	lenDiff := !equalInt64Ptr(exp.charLen, act.charLen)
	precDiff := !equalInt64Ptr(exp.numPrec, act.numPrec) || !equalInt64Ptr(exp.numScale, act.numScale)
	nullableDiff := exp.nullable != act.nullable
	commentDiff := exp.comment != "" && exp.comment != act.comment

	if !typeDiff && !lenDiff && !precDiff && !nullableDiff && !commentDiff {
		return nil
	}
	switch engineName {
	case "pgx":
		qtbl := quotePGTable(schema, table)
		var out []string
		if typeDiff || lenDiff || precDiff {
			out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" TYPE %s`, qtbl, col.Name, e.Dialect().SQLType(col)))
		}
		if nullableDiff {
			kw := "DROP NOT NULL"
			if !exp.nullable {
				kw = "SET NOT NULL"
			}
			out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN "%s" %s`, qtbl, col.Name, kw))
		}
		if commentDiff {
			out = append(out, fmt.Sprintf(`COMMENT ON COLUMN %s."%s" IS '%s'`, qtbl, col.Name, escapeSQLString(exp.comment)))
		}
		return out
	case "mysql":
		// MODIFY COLUMN 需给出完整列定义（类型/nullable/默认值/注释），
		// 由 xorm ColumnString 统一渲染。纯注释差异不收敛：为重写注释而整列
		// MODIFY 有默认值漂移风险，代价大于收益（pg 的 COMMENT 语句是元数据
		// 操作，无此问题，故 pg 收敛纯注释差异）。
		if !typeDiff && !lenDiff && !precDiff && !nullableDiff {
			return nil
		}
		colDef, err := dialects.ColumnString(e.Dialect(), col, false, false)
		if err != nil {
			return nil
		}
		return []string{fmt.Sprintf("ALTER TABLE `%s` MODIFY COLUMN %s", table, colDef)}
	case "mssql":
		if commentDiff {
			return nil // mssql 注释（extended_properties）不收敛
		}
		qtbl := quoteMSSQLTable(schema, table)
		nullKW := "NULL"
		if !exp.nullable {
			nullKW = "NOT NULL"
		}
		// 类型无差异（仅 nullable 变化）时保留实际 data_type，避免无谓重写。
		sqlType := strings.ToUpper(act.dataType)
		if typeDiff || lenDiff || precDiff {
			sqlType = e.Dialect().SQLType(col)
		}
		return []string{fmt.Sprintf("ALTER TABLE %s ALTER COLUMN [%s] %s %s", qtbl, col.Name, sqlType, nullKW)}
	}
	return nil
}

// escapeSQLString SQL 字符串字面量转义（单引号加倍）。
func escapeSQLString(s string) string { return strings.ReplaceAll(s, "'", "''") }

// quotePGTable PG 表名引用（schema 非空时两段引用）。
func quotePGTable(schema, table string) string {
	if schema != "" {
		return `"` + schema + `"."` + table + `"`
	}
	return `"` + table + `"`
}

// quoteMSSQLTable MSSQL 表名引用。
func quoteMSSQLTable(schema, table string) string {
	if schema != "" {
		return "[" + schema + "].[" + table + "]"
	}
	return "[" + table + "]"
}

// execConverge 计算并执行列收敛语句（s 为 Sync2 所在事务会话，语句随事务提交/回滚）。
func (d *XormDriver) execConverge(s *xorm.Session, models []any) error {
	engineName := ""
	if d.engine != nil {
		engineName = d.engine.DriverName()
	}
	stmts, err := convergeStatements(d.engine, engineName, d.schema, models)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if _, err := s.Exec(stmt); err != nil {
			return fmt.Errorf("xormdriver: 列收敛执行失败 %q: %w", stmt, err)
		}
	}
	return nil
}
