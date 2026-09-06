package xormdriver

import (
	"testing"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── many2many / polymorphic Preload（共享引擎，rel tag 统一表达）───────
//
// F2 引擎契约注意点：many2many 中间表连接列缺省取父/子主键"列名"，两侧主键
// 列同名（都叫 id）会让第一跳 SELECT 出现同名列歧义——测试模型两侧主键列
// 使用不同列名（student_id/course_id），rel tag 的 joinForeignKey/joinReferences
// 写 Go 字段名。

type relStudent struct {
	StudentID string      `xorm:"pk varchar(16) 'student_id'"`
	Name      string      `xorm:"varchar(64) 'name'"`
	Courses   []relCourse `xorm:"-" rel:"many2many:rel_enrollment;joinForeignKey:StudentID;joinReferences:CourseID"`
}

type relCourse struct {
	CourseID string `xorm:"pk varchar(16) 'course_id'"`
	Title    string `xorm:"varchar(64) 'title'"`
}

// polyPost 多态父模型（默认 polymorphicValue = 父表名 poly_post）。
type polyPost struct {
	ID     string    `xorm:"pk varchar(16) 'id'"`
	Title  string    `xorm:"varchar(64) 'title'"`
	Images []polyImg `xorm:"-" rel:"polymorphic:Owner"`
}

// polyVideo 显式 polymorphicValue（custom_video）。
type polyVideo struct {
	ID     string    `xorm:"pk varchar(16) 'id'"`
	Title  string    `xorm:"varchar(64) 'title'"`
	Images []polyImg `xorm:"-" rel:"polymorphic:Owner;polymorphicValue:custom_video"`
}

type polyImg struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	OwnerID   string `xorm:"varchar(16) 'owner_id'"`
	OwnerType string `xorm:"varchar(32) 'owner_type'"`
	URL       string `xorm:"varchar(120) 'url'"`
}

// newRelPreloadDriver 迁移 m2m/多态测试模型并灌入固定数据。
func newRelPreloadDriver(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&relStudent{}, &relCourse{}, &polyPost{}, &polyVideo{}, &polyImg{}); err != nil {
		t.Fatalf("自动迁移失败: %v", err)
	}
	for _, ddl := range []string{
		// 中间表无需业务模型，直接建表灌数（引擎对中间表只读）
		"CREATE TABLE rel_enrollment (student_id VARCHAR(16) NOT NULL, course_id VARCHAR(16) NOT NULL)",
		"INSERT INTO rel_student (student_id, name) VALUES ('s1', 'alice'), ('s2', 'bob'), ('s3', 'carol')",
		"INSERT INTO rel_course (course_id, title) VALUES ('c1', 'math'), ('c2', 'phys'), ('c3', 'chem')",
		"INSERT INTO rel_enrollment (student_id, course_id) VALUES " +
			"('s1', 'c1'), ('s1', 'c2'), ('s2', 'c2')",
		// 多态：p1 两张（owner_type=poly_post），v1 一张（custom_video），
		// img4 与 p1 同 owner_id 但类型列不同——类型过滤的负面用例
		"INSERT INTO poly_post (id, title) VALUES ('p1', 'post1')",
		"INSERT INTO poly_video (id, title) VALUES ('v1', 'video1')",
		"INSERT INTO poly_img (id, owner_id, owner_type, url) VALUES " +
			"('img1', 'p1', 'poly_post', 'u1'), ('img2', 'p1', 'poly_post', 'u2'), " +
			"('img3', 'v1', 'custom_video', 'u3'), ('img4', 'p1', 'custom_video', 'u4')",
	} {
		if _, err := drv.engine.Exec(ddl); err != nil {
			t.Fatalf("初始化测试数据失败 %q: %v", ddl, err)
		}
	}
	return drv
}

// courseIDs 提取预加载课程 ID 集合（查询序不做假设）。
func courseIDs(t *testing.T, courses []relCourse) map[string]string {
	t.Helper()
	out := make(map[string]string, len(courses))
	for _, c := range courses {
		out[c.CourseID] = c.Title
	}
	return out
}

// TestPreloadMany2ManyRelTag rel:"many2many:..." 两跳回填：第一跳查中间表
// 映射行，第二跳按引用键集 IN 查子表，按中间表映射不错位回填。
func TestPreloadMany2ManyRelTag(t *testing.T) {
	drv := newRelPreloadDriver(t)
	var students []relStudent
	if err := drv.Query().Preload("Courses").Find(&students); err != nil {
		t.Fatalf("many2many 预加载查询失败: %v", err)
	}
	if len(students) != 3 {
		t.Fatalf("应查到 3 个学生，实际 %d", len(students))
	}
	byID := make(map[string]*relStudent, len(students))
	for i := range students {
		byID[students[i].StudentID] = &students[i]
	}
	s1 := courseIDs(t, byID["s1"].Courses)
	if len(s1) != 2 || s1["c1"] != "math" || s1["c2"] != "phys" {
		t.Fatalf("s1 应预加载 c1/c2，实际 %v", s1)
	}
	s2 := courseIDs(t, byID["s2"].Courses)
	if len(s2) != 1 || s2["c2"] != "phys" {
		t.Fatalf("s2 应仅预加载 c2，实际 %v", s2)
	}
	// 无中间表行 → has-many 回填非 nil 空切片（契约 5）
	if byID["s3"].Courses == nil || len(byID["s3"].Courses) != 0 {
		t.Fatalf("s3 无选课应回填空切片，实际 %v", byID["s3"].Courses)
	}
	// 未建立关联的 c3 不出现在任何回填结果
	for i := range students {
		if _, hit := courseIDs(t, students[i].Courses)["c3"]; hit {
			t.Fatalf("c3 不应被回填到 %s", students[i].StudentID)
		}
	}
}

// TestPreloadMany2ManyWithConditions conds 作用于第二跳（子表查询）。
func TestPreloadMany2ManyWithConditions(t *testing.T) {
	drv := newRelPreloadDriver(t)
	var students []relStudent
	if err := drv.Query().Preload("Courses", "title = ?", "math").Find(&students); err != nil {
		t.Fatalf("带条件 many2many 预加载失败: %v", err)
	}
	byID := make(map[string]*relStudent, len(students))
	for i := range students {
		byID[students[i].StudentID] = &students[i]
	}
	s1 := courseIDs(t, byID["s1"].Courses)
	if len(s1) != 1 || s1["c1"] != "math" {
		t.Fatalf("条件过滤后 s1 应仅预加载 c1，实际 %v", s1)
	}
}

// TestPreloadPolymorphicRelTag polymorphic 类型列过滤回填：
// polymorphicValue 缺省取父表名；显式 polymorphicValue 按声明值过滤。
func TestPreloadPolymorphicRelTag(t *testing.T) {
	drv := newRelPreloadDriver(t)

	// 缺省 polymorphicValue = 父表名 poly_post：只回填 owner_type=poly_post 的行
	var posts []polyPost
	if err := drv.Query().Preload("Images").Find(&posts); err != nil {
		t.Fatalf("多态预加载查询失败: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != "p1" {
		t.Fatalf("应查到 p1，实际 %+v", posts)
	}
	if len(posts[0].Images) != 2 {
		t.Fatalf("p1 应预加载 img1/img2，实际 %+v", posts[0].Images)
	}
	for _, img := range posts[0].Images {
		if img.OwnerType != "poly_post" {
			t.Fatalf("类型列过滤失效，img4(custom_video) 不应回填: %+v", posts[0].Images)
		}
	}

	// 显式 polymorphicValue:custom_video
	var videos []polyVideo
	if err := drv.Query().Preload("Images").Find(&videos); err != nil {
		t.Fatalf("多态预加载查询失败: %v", err)
	}
	if len(videos) != 1 || videos[0].ID != "v1" {
		t.Fatalf("应查到 v1，实际 %+v", videos)
	}
	if len(videos[0].Images) != 1 || videos[0].Images[0].ID != "img3" {
		t.Fatalf("v1 应仅预加载 img3，实际 %+v", videos[0].Images)
	}
}

// TestPreloadMany2ManyInTransaction 共享引擎子查询继承事务会话：事务内 m2m
// 预加载与提交后读取结果一致（NewQuery 闭包 tx 继承的回归保护）。
func TestPreloadMany2ManyInTransaction(t *testing.T) {
	drv := newRelPreloadDriver(t)
	var inTx []relStudent
	err := drv.Query().Transaction(func(tx contracts.Query) error {
		return tx.Preload("Courses").Find(&inTx)
	})
	if err != nil {
		t.Fatalf("事务内 many2many 预加载失败: %v", err)
	}
	byID := make(map[string]*relStudent, len(inTx))
	for i := range inTx {
		byID[inTx[i].StudentID] = &inTx[i]
	}
	if len(courseIDs(t, byID["s1"].Courses)) != 2 {
		t.Fatalf("事务内 m2m 预加载未生效: %+v", byID["s1"].Courses)
	}
}
