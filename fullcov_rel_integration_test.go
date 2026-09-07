//go:build integration

package xormdriver

// fullcov_rel_integration_test.go —— 关联功能域（dual-driver-test-plan.md §5.6）
// 真实库（PG/MySQL）补强包：has-one / 复合外键 / polymorphic 缺省值回退 /
// 自引用树 / references 非主键 / many2many conds / 错误路径。
// belongs-to、has-many 基础、many2many 两跳在 SQLite 单测与既有集成用例
// （pg_integration_test.go 的 Preload 全套）已覆盖，本文件不重复。
//
// 运行方式（docker-compose.yml 环境）：
//
//	GOFAST_TEST_PG_DSN="postgres://gofast:gofast123@127.0.0.1:5432/gofast_test?sslmode=disable" \
//	  go test -tags integration -run 'TestFullCovRel_PG$' -v .
//	GOFAST_TEST_MYSQL_DSN="gofast:gofast123@tcp(127.0.0.1:3306)/gofast_test" \
//	  go test -tags integration -run 'TestFullCovRel_MySQL$' -v .
//
// 覆盖清单（§5.6 编号）：
//   - REL-H1：has-one 回填——有子行回填、无子行保持 nil（不 panic 空对象）、
//     conds 过滤子行（命中回填/未命中置 nil 两侧）
//   - REL-H1 嵌套：has-one 二层（用户→档案→头像），第二层无子行保持 nil
//   - REL-C1：复合外键（tag 式 foreignKey 两列）——元组 IN 查询（引擎契约 1，
//     PG/MySQL 行值构造语法均支持，差异固化：双方言行为一致）+ 多父行数据
//     不错位 + 半匹配陷阱行（region 对 id 错）不落任何父行
//   - REL-B2：references 指向父表非主键唯一列（email）——按引用列（非主键）
//     取父行键值与分组回填
//   - REL-P1：polymorphic——显式 polymorphicValue 与缺省值回退父表名
//     （preload/relation.go resolvePolymorphic：polyValue 为空取
//     meta.TableName(parentType)，即显式 TableName() 的表名）+ 类型隔离负例
//   - REL-S1：自引用树（parent_id 自引用）+ 嵌套预加载两层
//     （Node→Children→GrandChildren，引擎契约 2 以本层回填子行为下层父行）
//   - REL-M2：many2many conds——中间表无模型，conds 作用于子表（第二跳，
//     引擎契约 3）；无匹配回填非 nil 空切片（契约 5）
//   - REL-E1：错误路径——未知关联字段 / 非关联字段 / 缺忽略标记（契约 6 →
//     ErrUnsupported）/ 非法 conds 类型（链上校验）
//
// 方言说明：本文件全部断言 PG/MySQL 共享（无行为差异），差异固化点以
// "差异固化" 注释就地标注（元组 IN 行值语法、NULL→零值扫描、缺忽略标记
// 字段的 TEXT 列映射等双方言一致的落库形态）。

import (
	"errors"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 测试模型（fcr 前缀，显式 TableName + 显式列 tag）──────────────────

// fcrUser has-one 父模型：Profile 单 struct 指针字段。方向探测（preload
// DefaultResolver.resolveDirectional）对非切片字段先试 belongs-to——父模型
// 不含 UserID 字段即 belongs-to 不成立，落 has 方向（外键在子表）为 HasOne。
type fcrUser struct {
	ID      string      `xorm:"pk varchar(16) 'id'"`
	Name    string      `xorm:"varchar(64) 'name'"`
	Profile *fcrProfile `xorm:"-" rel:"foreignKey:UserID;references:ID"`
}

func (fcrUser) TableName() string { return "fcr_rel_users" }

// fcrProfile has-one 子表（外键 user_id 在本表）+ 二层父模型（Avatar）。
type fcrProfile struct {
	ID     string     `xorm:"pk varchar(16) 'id'"`
	UserID string     `xorm:"varchar(16) 'user_id'"`
	Bio    string     `xorm:"varchar(64) 'bio'"`
	Avatar *fcrAvatar `xorm:"-" rel:"foreignKey:ProfileID;references:ID"`
}

func (fcrProfile) TableName() string { return "fcr_rel_profiles" }

// fcrAvatar has-one 第三层（头像）。
type fcrAvatar struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	ProfileID string `xorm:"varchar(16) 'profile_id'"`
	URL       string `xorm:"varchar(120) 'url'"`
}

func (fcrAvatar) TableName() string { return "fcr_rel_avatars" }

// fcrCmpParent 复合外键父模型：引用键 = (region, id) 两列。
type fcrCmpParent struct {
	ID     string       `xorm:"pk varchar(16) 'id'"`
	Region string       `xorm:"varchar(16) 'region'"`
	Items  []fcrCmpItem `xorm:"-" rel:"foreignKey:ParentRegion,ParentID;references:Region,ID"`
}

func (fcrCmpParent) TableName() string { return "fcr_rel_cmp_parents" }

// fcrCmpItem 复合外键子表：外键 (parent_region, parent_id) 与父引用键一一对应。
type fcrCmpItem struct {
	ID           string `xorm:"pk varchar(16) 'id'"`
	ParentRegion string `xorm:"varchar(16) 'parent_region'"`
	ParentID     string `xorm:"varchar(16) 'parent_id'"`
	Sku          string `xorm:"varchar(32) 'sku'"`
}

func (fcrCmpItem) TableName() string { return "fcr_rel_cmp_items" }

// fcrRefUser references 非主键父模型：引用列 email（非主键唯一列）。
type fcrRefUser struct {
	ID    string      `xorm:"pk varchar(16) 'id'"`
	Email string      `xorm:"varchar(64) 'email' unique"`
	Msgs  []fcrRefMsg `xorm:"-" rel:"foreignKey:UserEmail;references:Email"`
}

func (fcrRefUser) TableName() string { return "fcr_rel_ref_users" }

// fcrRefMsg references 非主键子表：user_email 指向父表 email 列。
type fcrRefMsg struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	UserEmail string `xorm:"varchar(64) 'user_email'"`
	Body      string `xorm:"varchar(64) 'body'"`
}

func (fcrRefMsg) TableName() string { return "fcr_rel_ref_msgs" }

// fcrPolyPost 多态父模型（缺省 polymorphicValue）：PolyValue 回退父表名
// fcr_poly_posts（resolvePolymorphic：polyValue 空 → meta.TableName(parentType)）。
type fcrPolyPost struct {
	ID     string         `xorm:"pk varchar(16) 'id'"`
	Title  string         `xorm:"varchar(64) 'title'"`
	Images []fcrPolyImage `xorm:"-" rel:"polymorphic:Owner"`
}

func (fcrPolyPost) TableName() string { return "fcr_poly_posts" }

// fcrPolyVideo 多态父模型（显式 polymorphicValue:fcr_video）。
type fcrPolyVideo struct {
	ID     string         `xorm:"pk varchar(16) 'id'"`
	Title  string         `xorm:"varchar(64) 'title'"`
	Images []fcrPolyImage `xorm:"-" rel:"polymorphic:Owner;polymorphicValue:fcr_video"`
}

func (fcrPolyVideo) TableName() string { return "fcr_poly_videos" }

// fcrPolyImage 多态子表：owner_id/owner_type 列名按 <poly 蛇形>_id/_type 约定。
type fcrPolyImage struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	OwnerID   string `xorm:"varchar(16) 'owner_id'"`
	OwnerType string `xorm:"varchar(32) 'owner_type'"`
	URL       string `xorm:"varchar(120) 'url'"`
}

func (fcrPolyImage) TableName() string { return "fcr_poly_images" }

// fcrNode 自引用树模型：Children rel 指向自身类型（切片 → has 方向优先探测，
// 外键 parent_id 与引用 id 在同一张表）。
type fcrNode struct {
	ID       string    `xorm:"pk varchar(16) 'id'"`
	ParentID string    `xorm:"varchar(16) 'parent_id' null"`
	Name     string    `xorm:"varchar(64) 'name'"`
	Children []fcrNode `xorm:"-" rel:"foreignKey:ParentID;references:ID"`
}

func (fcrNode) TableName() string { return "fcr_nodes" }

// fcrStudent many2many 父模型：中间表 fcr_enrollments 无业务模型（引擎只读），
// 两侧主键列名不同（student_id/course_id），joinForeignKey/joinReferences 写
// Go 字段名（对齐 SQLite 套件 query_preload_rel_test.go 的同款约定）。
type fcrStudent struct {
	StudentID string      `xorm:"pk varchar(16) 'student_id'"`
	Name      string      `xorm:"varchar(64) 'name'"`
	Courses   []fcrCourse `xorm:"-" rel:"many2many:fcr_enrollments;joinForeignKey:StudentID;joinReferences:CourseID"`
}

func (fcrStudent) TableName() string { return "fcr_students" }

// fcrCourse many2many 子表。
type fcrCourse struct {
	CourseID string `xorm:"pk varchar(16) 'course_id'"`
	Title    string `xorm:"varchar(64) 'title'"`
}

func (fcrCourse) TableName() string { return "fcr_courses" }

// fcrNoMarkUser 缺忽略标记模型（REL-E1 契约 6 负例）：Extras 带 rel tag 但无
// xorm:"-"——xorm 把无 tag 的 struct 切片字段映射为 TEXT 列（差异固化：xorm
// v1.4.1 Type2SQLType 对非字节切片落 Text，双方言一致），查询以显式投影避开
// 该列，仅验证 Resolver 前置校验报 ErrUnsupported。
type fcrNoMarkUser struct {
	ID     string         `xorm:"pk varchar(16) 'id'"`
	Name   string         `xorm:"varchar(64) 'name'"`
	Extras []fcrNoMarkRow `rel:"foreignKey:OwnerID;references:ID"`
}

func (fcrNoMarkUser) TableName() string { return "fcr_nomark_users" }

// fcrNoMarkRow 缺忽略标记用例的子模型（仅参与解析，不迁移）。
type fcrNoMarkRow struct {
	ID      string `xorm:"pk varchar(16) 'id'"`
	OwnerID string `xorm:"varchar(16) 'owner_id'"`
}

// ── 共享辅助 ─────────────────────────────────────────────────────────

// fcrMustCreate 逐行种子写入（q.Create 路径，双方言一致）。
func fcrMustCreate(t *testing.T, drv *XormDriver, rows ...any) {
	t.Helper()
	for _, r := range rows {
		if err := drv.Query().Create(r); err != nil {
			t.Fatalf("种子 %T %+v: %v", r, r, err)
		}
	}
}

// fcrByID 按字段值建索引（查询序不做假设）。
func fcrByID[T any](t *testing.T, rows []T, key func(*T) string) map[string]*T {
	t.Helper()
	out := make(map[string]*T, len(rows))
	for i := range rows {
		out[key(&rows[i])] = &rows[i]
	}
	return out
}

// ── 分组入口 ─────────────────────────────────────────────────────────

// TestFullCovRel_PG PostgreSQL 关联域覆盖入口。
func TestFullCovRel_PG(t *testing.T) {
	runFullCovRel(t, newPGXorm(t), "pg")
}

// TestFullCovRel_MySQL MySQL 关联域覆盖入口。
func TestFullCovRel_MySQL(t *testing.T) {
	runFullCovRel(t, newPlainMySQLXorm(t), "mysql")
}

// runFullCovRel 共享 runner：按 §5.6 用例编号分组，双方言共享全部断言
// （本域双方言行为一致，无分支）。
func runFullCovRel(t *testing.T, drv *XormDriver, dialect string) {
	t.Helper()
	t.Logf("关联域覆盖 dialect=%s", dialect)
	t.Run("REL-H1_has-one基础", func(t *testing.T) { runFcrHasOne(t, drv) })
	t.Run("REL-H1_has-one嵌套", func(t *testing.T) { runFcrHasOneNested(t, drv) })
	t.Run("REL-C1_复合外键", func(t *testing.T) { runFcrCompositeFK(t, drv) })
	t.Run("REL-B2_references非主键", func(t *testing.T) { runFcrReferencesNonPK(t, drv) })
	t.Run("REL-P1_polymorphic缺省值", func(t *testing.T) { runFcrPolymorphic(t, drv) })
	t.Run("REL-S1_自引用树", func(t *testing.T) { runFcrSelfRefTree(t, drv) })
	t.Run("REL-M2_many2many_conds", func(t *testing.T) { runFcrMany2ManyConds(t, drv) })
	t.Run("REL-E1_错误路径", func(t *testing.T) { runFcrErrorPaths(t, drv) })
}

// ── REL-H1：has-one 基础 ─────────────────────────────────────────────

// runFcrHasOne has-one 三断言：有子行回填、无子行保持 nil（不 panic 空对象）、
// conds 过滤子行（命中/未命中两侧）。
func runFcrHasOne(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_rel_users", "fcr_rel_profiles", "fcr_rel_avatars"},
		&fcrUser{}, &fcrProfile{}, &fcrAvatar{})
	fcrMustCreate(t, drv,
		&fcrUser{ID: "u1", Name: "alice"},
		&fcrUser{ID: "u2", Name: "bob"}, // 无档案行
		&fcrProfile{ID: "pr1", UserID: "u1", Bio: "bio-u1"},
	)

	// 基础回填：u1 命中、u2 保持 nil
	var users []fcrUser
	if err := drv.Query().Preload("Profile").Find(&users); err != nil {
		t.Fatalf("has-one 预加载失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应查到 2 个用户, 实际 %d", len(users))
	}
	byID := fcrByID(t, users, func(u *fcrUser) string { return u.ID })
	if p := byID["u1"].Profile; p == nil || p.Bio != "bio-u1" {
		t.Errorf("u1 has-one Profile 应回填 bio-u1, 实际 %+v", byID["u1"].Profile)
	}
	if p := byID["u2"].Profile; p != nil {
		t.Errorf("u2 无档案行 Profile 应保持 nil（不回填空对象）, 实际 %+v", p)
	}

	// conds 命中：条件成立时回填
	var hit []fcrUser
	if err := drv.Query().Preload("Profile", "bio = ?", "bio-u1").Find(&hit); err != nil {
		t.Fatalf("conds 命中预加载失败: %v", err)
	}
	byID = fcrByID(t, hit, func(u *fcrUser) string { return u.ID })
	if p := byID["u1"].Profile; p == nil || p.Bio != "bio-u1" {
		t.Errorf("conds 命中时 u1 Profile 应回填, 实际 %+v", byID["u1"].Profile)
	}

	// conds 未命中：子行被过滤 → 关联保持 nil
	var miss []fcrUser
	if err := drv.Query().Preload("Profile", "bio = ?", "no-such-bio").Find(&miss); err != nil {
		t.Fatalf("conds 未命中预加载失败: %v", err)
	}
	byID = fcrByID(t, miss, func(u *fcrUser) string { return u.ID })
	if p := byID["u1"].Profile; p != nil {
		t.Errorf("conds 过滤后 u1 Profile 应保持 nil, 实际 %+v", p)
	}
	if p := byID["u2"].Profile; p != nil {
		t.Errorf("conds 过滤后 u2 Profile 应保持 nil, 实际 %+v", p)
	}
}

// ── REL-H1：has-one 嵌套 ─────────────────────────────────────────────

// runFcrHasOneNested has-one 二层（用户→档案→头像）：第一层 nil 时第二层
// 不下探；第一层有值而第二层无子行时第二层保持 nil（引擎契约 2/5）。
func runFcrHasOneNested(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_rel_users", "fcr_rel_profiles", "fcr_rel_avatars"},
		&fcrUser{}, &fcrProfile{}, &fcrAvatar{})
	fcrMustCreate(t, drv,
		&fcrUser{ID: "u1", Name: "alice"},
		&fcrUser{ID: "u2", Name: "bob"},
		&fcrProfile{ID: "pr1", UserID: "u1", Bio: "bio-u1"},
		&fcrProfile{ID: "pr2", UserID: "u2", Bio: "bio-u2"}, // 无头像
		&fcrAvatar{ID: "av1", ProfileID: "pr1", URL: "avatar-u1"},
	)

	var users []fcrUser
	if err := drv.Query().Preload("Profile.Avatar").Find(&users); err != nil {
		t.Fatalf("has-one 嵌套预加载失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应查到 2 个用户, 实际 %d", len(users))
	}
	byID := fcrByID(t, users, func(u *fcrUser) string { return u.ID })

	u1 := byID["u1"]
	if u1.Profile == nil || u1.Profile.Avatar == nil || u1.Profile.Avatar.URL != "avatar-u1" {
		t.Errorf("u1 二层头像应回填 avatar-u1, 实际 %+v", u1.Profile)
	}
	u2 := byID["u2"]
	if u2.Profile == nil || u2.Profile.Bio != "bio-u2" {
		t.Errorf("u2 第一层档案应回填 bio-u2, 实际 %+v", u2.Profile)
	}
	if u2.Profile != nil && u2.Profile.Avatar != nil {
		t.Errorf("u2 二层无头像应保持 nil, 实际 %+v", u2.Profile.Avatar)
	}
}

// ── REL-C1：复合外键 ─────────────────────────────────────────────────

// runFcrCompositeFK 复合外键（tag 式 foreignKey 两列）：元组 IN 查询
// （引擎契约 1，差异固化：PG/MySQL 行值构造 (a,b) IN ((?,?),(?,?)) 语法均
// 支持、结果一致）；多父行不错位；半匹配陷阱行（region 与 id 恰一对错位）
// 不落任何父行——单列匹配的错误实现会被该行捕获。
func runFcrCompositeFK(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_rel_cmp_parents", "fcr_rel_cmp_items"},
		&fcrCmpParent{}, &fcrCmpItem{})
	fcrMustCreate(t, drv,
		&fcrCmpParent{ID: "cp1", Region: "r1"},
		&fcrCmpParent{ID: "cp2", Region: "r2"},
		&fcrCmpParent{ID: "cp3", Region: "r3"}, // 无子行
		// cp1 两条 (r1,cp1)、cp2 一条 (r2,cp2)、陷阱行 (r2,cp1)：
		// region 对 cp2、id 对 cp1，两列联合后不属于任何父行
		&fcrCmpItem{ID: "ci1", ParentRegion: "r1", ParentID: "cp1", Sku: "s1"},
		&fcrCmpItem{ID: "ci2", ParentRegion: "r1", ParentID: "cp1", Sku: "s2"},
		&fcrCmpItem{ID: "ci3", ParentRegion: "r2", ParentID: "cp2", Sku: "s3"},
		&fcrCmpItem{ID: "ciTrap", ParentRegion: "r2", ParentID: "cp1", Sku: "trap"},
	)

	var parents []fcrCmpParent
	if err := drv.Query().Preload("Items").Find(&parents); err != nil {
		t.Fatalf("复合外键预加载失败: %v", err)
	}
	if len(parents) != 3 {
		t.Fatalf("应查到 3 个父行, 实际 %d", len(parents))
	}
	byID := fcrByID(t, parents, func(p *fcrCmpParent) string { return p.ID })

	skusOf := func(items []fcrCmpItem) map[string]bool {
		out := make(map[string]bool, len(items))
		for _, it := range items {
			out[it.Sku] = true
		}
		return out
	}
	if got := skusOf(byID["cp1"].Items); len(got) != 2 || !got["s1"] || !got["s2"] {
		t.Errorf("cp1 复合键 (r1,cp1) 应回填 s1/s2, 实际 %v", byID["cp1"].Items)
	}
	if got := skusOf(byID["cp2"].Items); len(got) != 1 || !got["s3"] {
		t.Errorf("cp2 复合键 (r2,cp2) 应回填 s3, 实际 %v", byID["cp2"].Items)
	}
	if items := byID["cp3"].Items; items == nil || len(items) != 0 {
		t.Errorf("cp3 无子行应回填非 nil 空切片（契约 5）, 实际 %+v (nil=%v)", items, items == nil)
	}
	// 陷阱行：region/id 半匹配均不可落入任何父行
	for _, p := range parents {
		for _, it := range p.Items {
			if it.Sku == "trap" {
				t.Errorf("陷阱行 (r2,cp1) 不应回填到父行 %s(%s): %+v", p.ID, p.Region, p.Items)
			}
		}
	}
}

// ── REL-B2：references 非主键 ────────────────────────────────────────

// runFcrReferencesNonPK references 指向父表非主键唯一列（email）：父行键值
// 取引用列而非主键、子行按引用列分组回填。
func runFcrReferencesNonPK(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_rel_ref_users", "fcr_rel_ref_msgs"},
		&fcrRefUser{}, &fcrRefMsg{})
	fcrMustCreate(t, drv,
		&fcrRefUser{ID: "ru1", Email: "a@gofast.dev"},
		&fcrRefUser{ID: "ru2", Email: "b@gofast.dev"},
		&fcrRefUser{ID: "ru3", Email: "c@gofast.dev"}, // 无消息
		&fcrRefMsg{ID: "m1", UserEmail: "a@gofast.dev", Body: "hello-a1"},
		&fcrRefMsg{ID: "m2", UserEmail: "a@gofast.dev", Body: "hello-a2"},
		&fcrRefMsg{ID: "m3", UserEmail: "b@gofast.dev", Body: "hello-b1"},
	)

	var users []fcrRefUser
	if err := drv.Query().Preload("Msgs").Find(&users); err != nil {
		t.Fatalf("references 非主键预加载失败: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("应查到 3 个用户, 实际 %d", len(users))
	}
	byID := fcrByID(t, users, func(u *fcrRefUser) string { return u.ID })

	bodies := func(msgs []fcrRefMsg) map[string]bool {
		out := make(map[string]bool, len(msgs))
		for _, m := range msgs {
			out[m.Body] = true
		}
		return out
	}
	if got := bodies(byID["ru1"].Msgs); len(got) != 2 || !got["hello-a1"] || !got["hello-a2"] {
		t.Errorf("ru1 应按 email=a@ 回填两条消息, 实际 %v", byID["ru1"].Msgs)
	}
	if got := bodies(byID["ru2"].Msgs); len(got) != 1 || !got["hello-b1"] {
		t.Errorf("ru2 应按 email=b@ 回填一条消息, 实际 %v", byID["ru2"].Msgs)
	}
	// 主键 id 与引用列 email 不同值：若引擎误用主键取值将查不到任何子行
	if msgs := byID["ru3"].Msgs; msgs == nil || len(msgs) != 0 {
		t.Errorf("ru3 无子行应回填非 nil 空切片（契约 5）, 实际 %+v (nil=%v)", msgs, msgs == nil)
	}
}

// ── REL-P1：polymorphic 缺省值 ───────────────────────────────────────

// runFcrPolymorphic 多态回填：缺省 polymorphicValue 回退父表名（实测规则：
// preload/relation.go resolvePolymorphic 对未显式指定 polymorphicValue 的
// 关联取 meta.TableName(parentType)，即模型显式 TableName() 的最终表名
// fcr_poly_posts，而非结构体名或蛇形推导）；显式值按声明过滤；类型列隔离
// 负例（同 owner_id 不同 owner_type 不回填）。
func runFcrPolymorphic(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_poly_posts", "fcr_poly_videos", "fcr_poly_images"},
		&fcrPolyPost{}, &fcrPolyVideo{}, &fcrPolyImage{})
	fcrMustCreate(t, drv,
		&fcrPolyPost{ID: "pp1", Title: "post1"},
		&fcrPolyVideo{ID: "pv1", Title: "video1"},
		// pp1 两张合法（owner_type=父表名 fcr_poly_posts）+ 一张类型噪声
		&fcrPolyImage{ID: "pi1", OwnerID: "pp1", OwnerType: "fcr_poly_posts", URL: "u1"},
		&fcrPolyImage{ID: "pi2", OwnerID: "pp1", OwnerType: "fcr_poly_posts", URL: "u2"},
		&fcrPolyImage{ID: "piN", OwnerID: "pp1", OwnerType: "fcr_video", URL: "noise"},
		&fcrPolyImage{ID: "pi3", OwnerID: "pv1", OwnerType: "fcr_video", URL: "v1"},
	)

	// 缺省值 = 父表名 fcr_poly_posts：只回填 owner_type=fcr_poly_posts 的行
	var posts []fcrPolyPost
	if err := drv.Query().Preload("Images").Find(&posts); err != nil {
		t.Fatalf("多态缺省值预加载失败: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != "pp1" {
		t.Fatalf("应查到 pp1, 实际 %+v", posts)
	}
	if len(posts[0].Images) != 2 {
		t.Fatalf("缺省值应按父表名过滤出 pi1/pi2, 实际 %+v", posts[0].Images)
	}
	urls := map[string]bool{}
	for _, img := range posts[0].Images {
		urls[img.URL] = true
		if img.OwnerType != "fcr_poly_posts" {
			t.Errorf("类型隔离失效，噪声行不应回填: %+v", posts[0].Images)
		}
	}
	if !urls["u1"] || !urls["u2"] {
		t.Errorf("缺省值回填内容错误, 实际 %v", urls)
	}

	// 显式 polymorphicValue:fcr_video
	var videos []fcrPolyVideo
	if err := drv.Query().Preload("Images").Find(&videos); err != nil {
		t.Fatalf("多态显式值预加载失败: %v", err)
	}
	if len(videos) != 1 || videos[0].ID != "pv1" {
		t.Fatalf("应查到 pv1, 实际 %+v", videos)
	}
	if len(videos[0].Images) != 1 || videos[0].Images[0].URL != "v1" {
		t.Fatalf("显式值应仅回填 pi3, 实际 %+v", videos[0].Images)
	}
}

// ── REL-S1：自引用树 ─────────────────────────────────────────────────

// runFcrSelfRefTree parent_id 自引用 + 嵌套预加载两层（Node→Children→
// GrandChildren）。方向探测：切片字段 has 方向优先（同表两侧列均存在），
// 判定 HasMany 而非 BelongsTo；根行 parent_id NULL 读回零值后不参与下层
// IN（契约 4 零值跳过）；叶子回填非 nil 空切片（契约 5）。
// 差异固化：根行用 NULL 字面量种子（双方言一致），NULL varchar 扫描回
// string 零值行为一致。
func runFcrSelfRefTree(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_nodes"}, &fcrNode{})
	intgXMustExec(t, drv,
		`INSERT INTO fcr_nodes (id, parent_id, name) VALUES `+
			`('n1', NULL, 'root-1'), ('n2', NULL, 'root-2'), `+
			`('n11', 'n1', 'child-1-1'), ('n12', 'n1', 'child-1-2'), ('n21', 'n2', 'child-2-1'), `+
			`('g111', 'n11', 'grand-1-1-1'), ('g112', 'n11', 'grand-1-1-2'), `+
			`('g121', 'n12', 'grand-1-2-1')`)

	var roots []fcrNode
	if err := drv.Query().Model(&fcrNode{}).
		Where("parent_id IS NULL").Order("id").
		Preload("Children.Children").Find(&roots); err != nil {
		t.Fatalf("自引用树预加载失败: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("应查到 2 个根节点, 实际 %d: %+v", len(roots), roots)
	}
	byID := fcrByID(t, roots, func(n *fcrNode) string { return n.ID })

	names := func(nodes []fcrNode) map[string]bool {
		out := make(map[string]bool, len(nodes))
		for _, n := range nodes {
			out[n.Name] = true
		}
		return out
	}

	n1 := byID["n1"]
	if n1 == nil {
		t.Fatalf("根节点 n1 缺失: %+v", roots)
	}
	if got := names(n1.Children); len(got) != 2 || !got["child-1-1"] || !got["child-1-2"] {
		t.Fatalf("n1 应预加载两个孩子, 实际 %+v", n1.Children)
	}
	// 二层：从第一层回填的子行递归（契约 2）
	byChild := fcrByID(t, n1.Children, func(n *fcrNode) string { return n.ID })
	if got := names(byChild["n11"].Children); len(got) != 2 || !got["grand-1-1-1"] || !got["grand-1-1-2"] {
		t.Errorf("n11 应预加载两个孙节点, 实际 %+v", byChild["n11"].Children)
	}
	if got := names(byChild["n12"].Children); len(got) != 1 || !got["grand-1-2-1"] {
		t.Errorf("n12 应预加载一个孙节点, 实际 %+v", byChild["n12"].Children)
	}

	n2 := byID["n2"]
	if n2 == nil {
		t.Fatalf("根节点 n2 缺失: %+v", roots)
	}
	if got := names(n2.Children); len(got) != 1 || !got["child-2-1"] {
		t.Errorf("n2 应预加载一个孩子, 实际 %+v", n2.Children)
	}
	// 叶子（孙层）无子行 → 非 nil 空切片（契约 5）
	leaf := fcrByID(t, n2.Children, func(n *fcrNode) string { return n.ID })["n21"]
	if leaf == nil || leaf.Children == nil || len(leaf.Children) != 0 {
		t.Errorf("叶子 n21 应回填非 nil 空切片, 实际 %+v (nil=%v)", leaf, leaf != nil && leaf.Children == nil)
	}
}

// ── REL-M2：many2many conds ──────────────────────────────────────────

// runFcrMany2ManyConds many2many 两跳 + conds：中间表无模型（DDL 直建），
// conds 作用于子表查询（第二跳，契约 3；对照 SQLite 套件
// TestPreloadMany2ManyWithConditions）；conds 过滤后无匹配的父行回填非 nil
// 空切片（契约 5）。
func runFcrMany2ManyConds(t *testing.T, drv *XormDriver) {
	t.Helper()
	tables := []string{"fcr_students", "fcr_courses", "fcr_enrollments"}
	intgXMigrate(t, drv, tables, &fcrStudent{}, &fcrCourse{})
	intgXMustExec(t, drv,
		`CREATE TABLE fcr_enrollments (student_id varchar(16) NOT NULL, course_id varchar(16) NOT NULL)`)
	fcrMustCreate(t, drv,
		&fcrStudent{StudentID: "s1", Name: "alice"},
		&fcrStudent{StudentID: "s2", Name: "bob"},
		&fcrCourse{CourseID: "c1", Title: "math"},
		&fcrCourse{CourseID: "c2", Title: "phys"},
		&fcrCourse{CourseID: "c3", Title: "chem"}, // 未选
	)
	intgXMustExec(t, drv,
		`INSERT INTO fcr_enrollments (student_id, course_id) VALUES `+
			`('s1', 'c1'), ('s1', 'c2'), ('s2', 'c2')`)

	// 无 conds：两跳完整回填
	var students []fcrStudent
	if err := drv.Query().Preload("Courses").Find(&students); err != nil {
		t.Fatalf("m2m 预加载失败: %v", err)
	}
	byID := fcrByID(t, students, func(s *fcrStudent) string { return s.StudentID })
	if got := fcrCourseTitles(byID["s1"].Courses); len(got) != 2 || !got["math"] || !got["phys"] {
		t.Errorf("s1 应预加载 math/phys, 实际 %v", byID["s1"].Courses)
	}

	// conds 作用于子表（第二跳）：s1 仅剩 math；s2 全被过滤 → 非 nil 空切片
	var filtered []fcrStudent
	if err := drv.Query().Preload("Courses", "title = ?", "math").Find(&filtered); err != nil {
		t.Fatalf("m2m conds 预加载失败: %v", err)
	}
	byID = fcrByID(t, filtered, func(s *fcrStudent) string { return s.StudentID })
	if got := fcrCourseTitles(byID["s1"].Courses); len(got) != 1 || !got["math"] {
		t.Errorf("conds 过滤后 s1 应仅剩 math, 实际 %v", byID["s1"].Courses)
	}
	courses := byID["s2"].Courses
	if courses == nil || len(courses) != 0 {
		t.Errorf("conds 过滤后 s2 应回填非 nil 空切片（契约 5）, 实际 %+v (nil=%v)", courses, courses == nil)
	}
}

// fcrCourseTitles 提取课程标题集合（查询序不做假设）。
func fcrCourseTitles(courses []fcrCourse) map[string]bool {
	out := make(map[string]bool, len(courses))
	for _, c := range courses {
		out[c.Title] = true
	}
	return out
}

// ── REL-E1：错误路径 ─────────────────────────────────────────────────

// runFcrErrorPaths 契约 6 错误路径（全部 ErrUnsupported 语义，经 errors.Is
// 断言 Sentinel 包装）：未知关联字段、非关联字段、关联字段缺忽略标记、
// 非法 conds 类型（链上校验）。
func runFcrErrorPaths(t *testing.T, drv *XormDriver) {
	t.Helper()
	intgXMigrate(t, drv, []string{"fcr_rel_users", "fcr_rel_profiles", "fcr_nomark_users"},
		&fcrUser{}, &fcrProfile{}, &fcrNoMarkUser{})
	fcrMustCreate(t, drv,
		&fcrUser{ID: "u1", Name: "alice"},
	)
	// 缺忽略标记模型的种子行（原生 SQL 避开未忽略切片字段的写路径；
	// 查询以显式投影避开 TEXT 列——差异固化：该字段双方言均落 TEXT 列）
	intgXMustExec(t, drv, `INSERT INTO fcr_nomark_users (id, name) VALUES ('nm1', 'nomark')`)

	// 1) 未知关联字段 → ErrUnsupported（字段在模型上不存在）
	err := drv.Query().Preload("Ghost").Find(&[]fcrUser{})
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("未知字段 Preload 应报 ErrUnsupported, 实际: %v", err)
	}

	// 2) 非关联字段 → ErrUnsupported（Name 为 string，非 struct/切片）
	err = drv.Query().Preload("Name").Find(&[]fcrUser{})
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("非关联字段 Preload 应报 ErrUnsupported, 实际: %v", err)
	}

	// 3) 关联字段缺忽略标记（契约 6 前置校验）→ ErrUnsupported
	var nomarks []fcrNoMarkUser
	err = drv.Query().Model(&fcrNoMarkUser{}).Select("id", "name").
		Where("id = ?", "nm1").Preload("Extras").Find(&nomarks)
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("缺忽略标记的关联字段 Preload 应报 ErrUnsupported, 实际: %v", err)
	}

	// 4) 非法 conds 类型（链上校验，终结方法透出）→ ErrUnsupported
	err = drv.Query().Preload("Profile", 123).Find(&[]fcrUser{})
	if !errors.Is(err, contracts.ErrUnsupported) {
		t.Errorf("非法 conds 类型应报 ErrUnsupported, 实际: %v", err)
	}
}
