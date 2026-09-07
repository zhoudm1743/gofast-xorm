package xormdriver

// errors_matrix_test.go —— L4 错误矩阵 SQLite 侧（方案 §六 第 12 项）：
// 三方言哨兵对照中落在 SQLite 的部分（无 build tag，随单测常跑）。表驱动
// 覆盖 ErrRecordNotFound / ErrDuplicatedKey / ErrUnsupported 三个哨兵，另加
// "未知错误原样透传"负对照（防过度映射吞掉底层上下文）。PG/MySQL 的重复键 +
// 未命中真实方言对照在 fault_integration_test.go（integration tag 域）补齐；
// 完整并发/故障矩阵同见该文件。

import (
	"errors"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// xfltMatrixRow 错误矩阵模型：code 唯一键用于制造重复键错误。
type xfltMatrixRow struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Code string `xorm:"unique varchar(32) 'code'"`
	Cnt  int64  `xorm:"'cnt' default(0)"`
}

func (xfltMatrixRow) TableName() string { return "xflt_matrix_rows" }

// TestFaultSQLite ERR 矩阵（SQLite 侧，表驱动）：断言 wrapError 归一后的
// 框架哨兵语义，而非 SQLite 方言原文文案。
func TestFaultSQLite(t *testing.T) {
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&xfltMatrixRow{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := drv.Query().Create(&xfltMatrixRow{ID: "m1", Code: "code-1", Cnt: 1}); err != nil {
		t.Fatalf("种子: %v", err)
	}

	// 全部 7 个哨兵：负对照（未知错误透传用例）逐一排除
	allSentinels := []error{
		contracts.ErrRecordNotFound,
		contracts.ErrDuplicatedKey,
		contracts.ErrInvalidTransaction,
		contracts.ErrDeadlock,
		contracts.ErrQueryTimeout,
		contracts.ErrConnFailed,
		contracts.ErrUnsupported,
	}

	cases := []struct {
		name string
		act  func(t *testing.T, q contracts.Query) error
		// want 期望哨兵；nil 表示特殊断言：出错但不匹配任何哨兵（原样透传）
		want error
	}{
		{
			name: "ErrRecordNotFound_未命中",
			act: func(t *testing.T, q contracts.Query) error {
				var row xfltMatrixRow
				return q.Model(&xfltMatrixRow{}).Where("id = ?", "missing").First(&row)
			},
			want: contracts.ErrRecordNotFound,
		},
		{
			name: "ErrDuplicatedKey_唯一键冲突",
			act: func(t *testing.T, q contracts.Query) error {
				// SQLite 文案 "UNIQUE constraint failed: xflt_matrix_rows.code"
				return q.Create(&xfltMatrixRow{ID: "m2", Code: "code-1"})
			},
			want: contracts.ErrDuplicatedKey,
		},
		{
			name: "ErrUnsupported_表达式更新缺表名",
			act: func(t *testing.T, q contracts.Query) error {
				// 链上无 Table()/Model() 时表达式 UPDATE 无法定位表 → ErrUnsupported
				return q.Update("cnt", contracts.Expr("cnt + ?", 1))
			},
			want: contracts.ErrUnsupported,
		},
		{
			name: "未知错误原样透传",
			act: func(t *testing.T, q contracts.Query) error {
				// 查询不存在的表：方言原文错误，不应匹配任何哨兵
				var n int64
				return q.Table("xflt_no_such_table").Count(&n)
			},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.act(t, drv.Query())
			if err == nil {
				t.Fatalf("期望错误, 实际 nil")
			}
			if c.want == nil {
				for _, s := range allSentinels {
					if errors.Is(err, s) {
						t.Fatalf("未知错误不应匹配任何哨兵（应原样透传）, 却命中 %v, 实际: %v", s, err)
					}
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("应映射 %v, 实际: %v", c.want, err)
			}
		})
	}
}
