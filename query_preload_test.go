package xormdriver

import (
	"errors"
	"testing"

	"github.com/zhoudm1743/go-fast-framework/cache"
	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── Preload 测试模型 ──────────────────────────────────────────────────
// 关联字段一律带 xorm:"-"：xorm 对 struct/slice 字段无法映射为列，
// 不打忽略标记会导致 TableInfo/Sync2 解析失败。外键列遵循约定
// <父表名>_id（preload_user_id / preload_order_id）。

// preloadUser 父模型：has-one Profile、has-many Orders/Tags。
type preloadUser struct {
	ID      string          `xorm:"pk varchar(16) 'id'"`
	Name    string          `xorm:"varchar(100) 'name'"`
	Profile *preloadProfile `xorm:"-"`
	Orders  []preloadOrder  `xorm:"-"`
	Tags    []*preloadTag   `xorm:"-"`
	Broken  []preloadBroken `xorm:"-"`
}

type preloadProfile struct {
	ID            string `xorm:"pk varchar(16) 'id'"`
	PreloadUserID string `xorm:"varchar(16) 'preload_user_id'"`
	Bio           string `xorm:"varchar(200) 'bio'"`
}

type preloadOrder struct {
	ID            string        `xorm:"pk varchar(16) 'id'"`
	PreloadUserID string        `xorm:"varchar(16) 'preload_user_id'"`
	Status        string        `xorm:"varchar(20) 'status'"`
	Items         []preloadItem `xorm:"-"`
}

type preloadItem struct {
	ID             string `xorm:"pk varchar(16) 'id'"`
	PreloadOrderID string `xorm:"varchar(16) 'preload_order_id'"`
	Sku            string `xorm:"varchar(100) 'sku'"`
}

type preloadTag struct {
	ID            string `xorm:"pk varchar(16) 'id'"`
	PreloadUserID string `xorm:"varchar(16) 'preload_user_id'"`
	Name          string `xorm:"varchar(50) 'name'"`
}

// preloadBroken 缺约定外键列（preload_user_id）的子模型，用于验证 FK 校验失败路径。
type preloadBroken struct {
	ID   string `xorm:"pk varchar(16) 'id'"`
	Name string `xorm:"varchar(50) 'name'"`
}

// ── gorm tag 外键 / 复合主键模型 ──────────────────────────────────────

// preloadGormUser 经 gorm tag 声明外键：子表外键列 owner_id（非约定列名）。
type preloadGormUser struct {
	ID     string             `xorm:"pk varchar(16) 'id'"`
	Name   string             `xorm:"varchar(100) 'name'"`
	Orders []preloadGormOrder `xorm:"-" gorm:"foreignKey:OwnerID;references:ID"`
}

type preloadGormOrder struct {
	ID      string `xorm:"pk varchar(16) 'id'"`
	OwnerID string `xorm:"varchar(16) 'owner_id'"`
	Status  string `xorm:"varchar(20) 'status'"`
}

// preloadCodeUser 经 gorm tag 引用非 id 主键（Code 列）。
type preloadCodeUser struct {
	Code   string             `xorm:"pk varchar(16) 'code'"`
	Name   string             `xorm:"varchar(100) 'name'"`
	Orders []preloadCodeOrder `xorm:"-" gorm:"foreignKey:OwnerCode;references:Code"`
}

type preloadCodeOrder struct {
	ID        string `xorm:"pk varchar(16) 'id'"`
	OwnerCode string `xorm:"varchar(16) 'owner_code'"`
	Status    string `xorm:"varchar(20) 'status'"`
}

// preloadCompositeUser 复合主键，外键走多列约定 <父表名>_<主键列>。
type preloadCompositeUser struct {
	TenantID string                  `xorm:"pk varchar(16) 'tenant_id'"`
	ID       string                  `xorm:"pk varchar(16) 'id'"`
	Name     string                  `xorm:"varchar(100) 'name'"`
	Orders   []preloadCompositeOrder `xorm:"-"`
}

type preloadCompositeOrder struct {
	ID         string `xorm:"pk varchar(16) 'id'"`
	CompTenant string `xorm:"varchar(16) 'preload_composite_user_tenant_id'"`
	CompID     string `xorm:"varchar(16) 'preload_composite_user_id'"`
	Status     string `xorm:"varchar(20) 'status'"`
}

// preloadTagCompositeUser 复合主键，外键经 gorm tag 字段名列表声明。
type preloadTagCompositeUser struct {
	TenantID string                     `xorm:"pk varchar(16) 'tenant_id'"`
	ID       string                     `xorm:"pk varchar(16) 'id'"`
	Orders   []preloadTagCompositeOrder `xorm:"-" gorm:"foreignKey:TID,UID;references:TenantID,ID"`
}

type preloadTagCompositeOrder struct {
	ID  string `xorm:"pk varchar(16) 'id'"`
	TID string `xorm:"varchar(16) 'tid'"`
	UID string `xorm:"varchar(16) 'uid'"`
}

// preloadBadTagUser gorm tag 指向子模型不存在的字段，用于验证 tag 校验失败路径。
type preloadBadTagUser struct {
	ID     string         `xorm:"pk varchar(16) 'id'"`
	Orders []preloadOrder `xorm:"-" gorm:"foreignKey:NoSuchField"`
}

// newPreloadDriver 迁移全部 Preload 测试模型并灌入固定数据。
func newPreloadDriver(t *testing.T) *XormDriver {
	t.Helper()
	drv := newXormTestDriver(t)
	if err := drv.AutoMigrate(&preloadUser{}, &preloadProfile{}, &preloadOrder{}, &preloadItem{}, &preloadTag{}, &preloadBroken{},
		&preloadGormUser{}, &preloadGormOrder{},
		&preloadCodeUser{}, &preloadCodeOrder{},
		&preloadCompositeUser{}, &preloadCompositeOrder{},
		&preloadTagCompositeUser{}, &preloadTagCompositeOrder{},
		&preloadBadTagUser{}); err != nil {
		t.Fatalf("自动迁移失败: %v", err)
	}
	for _, ddl := range []string{
		"INSERT INTO preload_user (id, name) VALUES ('u1', 'alice'), ('u2', 'bob')",
		"INSERT INTO preload_profile (id, preload_user_id, bio) VALUES ('p1', 'u1', 'bio1')",
		"INSERT INTO preload_order (id, preload_user_id, status) VALUES " +
			"('o1', 'u1', 'paid'), ('o2', 'u1', 'unpaid'), ('o3', 'u2', 'paid')",
		"INSERT INTO preload_item (id, preload_order_id, sku) VALUES " +
			"('i1', 'o1', 'sku1'), ('i2', 'o1', 'sku2'), ('i3', 'o3', 'sku3')",
		"INSERT INTO preload_tag (id, preload_user_id, name) VALUES ('t1', 'u1', 'tag1'), ('t2', 'u2', 'tag2')",
		"INSERT INTO preload_gorm_user (id, name) VALUES ('gu1', 'alice')",
		"INSERT INTO preload_gorm_order (id, owner_id, status) VALUES ('go1', 'gu1', 'paid'), ('go2', 'gu1', 'unpaid')",
		"INSERT INTO preload_code_user (code, name) VALUES ('c1', 'alice')",
		"INSERT INTO preload_code_order (id, owner_code, status) VALUES ('co1', 'c1', 'paid')",
		"INSERT INTO preload_composite_user (tenant_id, id, name) VALUES ('t1', 'cu1', 'alice'), ('t1', 'cu2', 'bob')",
		"INSERT INTO preload_composite_order (id, preload_composite_user_tenant_id, preload_composite_user_id, status) VALUES " +
			"('co1', 't1', 'cu1', 'paid'), ('co2', 't1', 'cu2', 'unpaid')",
		"INSERT INTO preload_tag_composite_user (tenant_id, id) VALUES ('t1', 'tc1'), ('t2', 'tc1')",
		"INSERT INTO preload_tag_composite_order (id, tid, uid) VALUES ('to1', 't1', 'tc1'), ('to2', 't2', 'tc1')",
		"INSERT INTO preload_bad_tag_user (id) VALUES ('b1')",
	} {
		if _, err := drv.engine.Exec(ddl); err != nil {
			t.Fatalf("初始化测试数据失败 %q: %v", ddl, err)
		}
	}
	return drv
}

// userByID 按 ID 索引查询结果。
func userByID(t *testing.T, users []preloadUser, id string) *preloadUser {
	t.Helper()
	for i := range users {
		if users[i].ID == id {
			return &users[i]
		}
	}
	t.Fatalf("查询结果缺少用户 %s", id)
	return nil
}

func orderByID(t *testing.T, orders []preloadOrder, id string) *preloadOrder {
	t.Helper()
	for i := range orders {
		if orders[i].ID == id {
			return &orders[i]
		}
	}
	t.Fatalf("预加载结果缺少订单 %s", id)
	return nil
}

// ── 正常路径 ──────────────────────────────────────────────────────────

func TestPreloadHasMany(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	if err := drv.Query().Preload("Orders").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if len(u1.Orders) != 2 {
		t.Fatalf("u1 应预加载 2 个订单，实际 %d", len(u1.Orders))
	}
	if orderByID(t, u1.Orders, "o1").Status != "paid" {
		t.Fatal("u1 订单 o1 数据错误")
	}
	if orderByID(t, u1.Orders, "o2").Status != "unpaid" {
		t.Fatal("u1 订单 o2 数据错误")
	}
	u2 := userByID(t, users, "u2")
	if len(u2.Orders) != 1 || u2.Orders[0].ID != "o3" {
		t.Fatalf("u2 应预加载 1 个订单 o3，实际 %+v", u2.Orders)
	}
	// 未预加载的关联保持零值
	if u1.Profile != nil || len(u1.Tags) != 0 {
		t.Fatal("未预加载的关联字段应保持零值")
	}
}

func TestPreloadHasManyPtrSlice(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	if err := drv.Query().Preload("Tags").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if len(u1.Tags) != 1 || u1.Tags[0] == nil || u1.Tags[0].Name != "tag1" {
		t.Fatalf("u1 应预加载指针切片标签 tag1，实际 %+v", u1.Tags)
	}
	u2 := userByID(t, users, "u2")
	if len(u2.Tags) != 1 || u2.Tags[0] == nil || u2.Tags[0].Name != "tag2" {
		t.Fatalf("u2 应预加载指针切片标签 tag2，实际 %+v", u2.Tags)
	}
}

func TestPreloadHasOne(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	if err := drv.Query().Preload("Profile").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if u1.Profile == nil || u1.Profile.Bio != "bio1" {
		t.Fatalf("u1 应预加载 Profile(bio1)，实际 %+v", u1.Profile)
	}
	u2 := userByID(t, users, "u2")
	if u2.Profile != nil {
		t.Fatalf("u2 无 Profile 记录，应保持 nil，实际 %+v", u2.Profile)
	}
}

func TestPreloadWithConditions(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	if err := drv.Query().Preload("Orders", "status = ?", "paid").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if len(u1.Orders) != 1 || u1.Orders[0].ID != "o1" {
		t.Fatalf("u1 应只预加载已支付订单 o1，实际 %+v", u1.Orders)
	}
	u2 := userByID(t, users, "u2")
	if len(u2.Orders) != 1 || u2.Orders[0].ID != "o3" {
		t.Fatalf("u2 应只预加载已支付订单 o3，实际 %+v", u2.Orders)
	}
}

func TestPreloadNested(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	if err := drv.Query().Preload("Orders.Items").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	o1 := orderByID(t, u1.Orders, "o1")
	if len(o1.Items) != 2 {
		t.Fatalf("o1 应预加载 2 个明细，实际 %d", len(o1.Items))
	}
	if len(orderByID(t, u1.Orders, "o2").Items) != 0 {
		t.Fatal("o2 无明细，应保持空")
	}
	u2 := userByID(t, users, "u2")
	if o3 := orderByID(t, u2.Orders, "o3"); len(o3.Items) != 1 || o3.Items[0].Sku != "sku3" {
		t.Fatalf("o3 应预加载明细 sku3，实际 %+v", o3.Items)
	}
}

func TestPreloadFirst(t *testing.T) {
	drv := newPreloadDriver(t)
	var user preloadUser
	if err := drv.Query().Preload("Orders").First(&user, "id = ?", "u1"); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(user.Orders) != 2 {
		t.Fatalf("First 应预加载 2 个订单，实际 %d", len(user.Orders))
	}
}

// ── 错误路径 ──────────────────────────────────────────────────────────

func TestPreloadUnknownField(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("NoSuchField").Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("预加载不存在的字段应返回 ErrUnsupported，实际 %v", err)
	}
}

func TestPreloadNonRelationField(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Name").Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("预加载普通列字段应返回 ErrUnsupported，实际 %v", err)
	}
}

func TestPreloadMissingFKColumn(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Broken").Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("子表缺约定外键列应返回 ErrUnsupported，实际 %v", err)
	}
}

func TestPreloadInvalidConditionArgs(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Orders", 42).Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("非法条件参数应返回 ErrUnsupported，实际 %v", err)
	}
}

// ── 事务 / 缓存交互 ──────────────────────────────────────────────────

func TestPreloadInTransaction(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Transaction(func(tx contracts.Query) error {
		return tx.Preload("Orders").Find(&users)
	})
	if err != nil {
		t.Fatalf("事务内预加载查询失败: %v", err)
	}
	if len(userByID(t, users, "u1").Orders) != 2 {
		t.Fatal("事务内预加载未生效")
	}
}

func TestPreloadCacheHitStillPreloads(t *testing.T) {
	drv := newPreloadDriver(t)
	store := cache.NewMemoryStore(16, 0)
	c := &preloadTestCache{CacheStore: store}
	if err := drv.EnableCaches(c); err != nil {
		t.Fatalf("启用查询缓存失败: %v", err)
	}

	base := drv.Query().Preload("Orders").Order("id").Cache().(*XormQuery)
	var first []preloadUser
	if err := base.Find(&first); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if len(userByID(t, first, "u1").Orders) != 2 {
		t.Fatal("首次查询预加载未生效")
	}
	if store.Get(buildCacheKey(base.keyParts, &first)) == nil {
		t.Fatal("首次查询未写入缓存，后续无法验证命中路径")
	}

	// 第二次查询应命中缓存，且预加载在命中路径上仍然执行
	var second []preloadUser
	if err := base.Find(&second); err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if len(userByID(t, second, "u1").Orders) != 2 {
		t.Fatal("缓存命中后预加载未生效")
	}
}

// preloadTestCache 测试用 contracts.Cache（与 gormdriver cacher_test 同款）。
type preloadTestCache struct {
	contracts.CacheStore
}

func (c *preloadTestCache) Store(name string) contracts.CacheStore { return c.CacheStore }

// ── gorm tag 外键 ─────────────────────────────────────────────────────

func TestPreloadGormTagForeignKey(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadGormUser
	if err := drv.Query().Preload("Orders").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(users) != 1 || len(users[0].Orders) != 2 {
		t.Fatalf("gorm tag 外键预加载失败，实际 %+v", users)
	}
	if users[0].Orders[0].OwnerID != "gu1" {
		t.Fatalf("gorm tag 外键数据错误，实际 %+v", users[0].Orders)
	}
}

func TestPreloadGormTagReferences(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadCodeUser
	if err := drv.Query().Preload("Orders").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(users) != 1 || len(users[0].Orders) != 1 || users[0].Orders[0].ID != "co1" {
		t.Fatalf("references 非 id 主键预加载失败，实际 %+v", users)
	}
}

func TestPreloadBadGormTag(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadBadTagUser
	err := drv.Query().Preload("Orders").Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("gorm tag 指向不存在字段应返回 ErrUnsupported，实际 %v", err)
	}
}

// ── 复合主键 ──────────────────────────────────────────────────────────

func TestPreloadCompositeConvention(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadCompositeUser
	if err := drv.Query().Preload("Orders").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应查到 2 个复合主键用户，实际 %d", len(users))
	}
	byID := map[string][]preloadCompositeOrder{}
	for _, u := range users {
		byID[u.ID] = u.Orders
	}
	if len(byID["cu1"]) != 1 || byID["cu1"][0].ID != "co1" || byID["cu1"][0].Status != "paid" {
		t.Fatalf("cu1 应预加载订单 co1，实际 %+v", byID["cu1"])
	}
	if len(byID["cu2"]) != 1 || byID["cu2"][0].ID != "co2" {
		t.Fatalf("cu2 应预加载订单 co2，实际 %+v", byID["cu2"])
	}
}

func TestPreloadCompositeGormTag(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadTagCompositeUser
	if err := drv.Query().Preload("Orders").Find(&users); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应查到 2 行复合主键用户，实际 %d", len(users))
	}
	byTenant := map[string][]preloadTagCompositeOrder{}
	for _, u := range users {
		byTenant[u.TenantID] = u.Orders
	}
	if len(byTenant["t1"]) != 1 || byTenant["t1"][0].ID != "to1" {
		t.Fatalf("t1 租户应预加载订单 to1，实际 %+v", byTenant["t1"])
	}
	if len(byTenant["t2"]) != 1 || byTenant["t2"][0].ID != "to2" {
		t.Fatalf("t2 租户应预加载订单 to2，实际 %+v", byTenant["t2"])
	}
}

// ── 子查询回调 ────────────────────────────────────────────────────────

func TestPreloadCallbackOrder(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Orders", func(q contracts.Query) contracts.Query {
		return q.Order("id DESC")
	}).Find(&users)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if len(u1.Orders) != 2 || u1.Orders[0].ID != "o2" || u1.Orders[1].ID != "o1" {
		t.Fatalf("回调排序未生效，实际 %+v", u1.Orders)
	}
}

func TestPreloadCallbackWithConditions(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Orders", "status = ?", "paid", func(q contracts.Query) contracts.Query {
		return q.Order("id DESC")
	}).Find(&users)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	u1 := userByID(t, users, "u1")
	if len(u1.Orders) != 1 || u1.Orders[0].ID != "o1" {
		t.Fatalf("条件+回调应只留已支付订单 o1，实际 %+v", u1.Orders)
	}
	u2 := userByID(t, users, "u2")
	if len(u2.Orders) != 1 || u2.Orders[0].ID != "o3" {
		t.Fatalf("u2 应只留已支付订单 o3，实际 %+v", u2.Orders)
	}
}

func TestPreloadInvalidCallback(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	err := drv.Query().Preload("Orders", func(x int) int { return x }).Find(&users)
	if err == nil || !errors.Is(err, contracts.ErrUnsupported) {
		t.Fatalf("非法回调参数应返回 ErrUnsupported，实际 %v", err)
	}
}

// ── FindInBatches 预加载 ──────────────────────────────────────────────

func TestPreloadFindInBatches(t *testing.T) {
	drv := newPreloadDriver(t)
	var users []preloadUser
	var seen int
	err := drv.Query().Preload("Orders").Order("id").FindInBatches(&users, 1, func(bq contracts.Query, batch int) error {
		seen++
		// 每批 append 进 dest 的最后一行应已预加载
		last := users[len(users)-1]
		want := 2
		if last.ID == "u2" {
			want = 1
		}
		if len(last.Orders) != want {
			t.Fatalf("第 %d 批 %s 预加载未生效，实际 %d 个订单", batch, last.ID, len(last.Orders))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FindInBatches 失败: %v", err)
	}
	if seen != 2 || len(users) != 2 {
		t.Fatalf("应分 2 批共 2 行，实际 %d 批 %d 行", seen, len(users))
	}
	if len(userByID(t, users, "u1").Orders) != 2 || len(userByID(t, users, "u2").Orders) != 1 {
		t.Fatal("分批预加载后 dest 数据不完整")
	}
}
