package xormdriver

import (
	"testing"
)

// ── version 乐观锁（xorm 原生语义，orm tag 接入后行为不变，11.3 对齐）──

// versionDoc 乐观锁模型：version token 由 xorm 原生处理（无需驱动代码，§8.4）。
type versionDoc struct {
	ID      string `orm:"pk varchar(16) 'id'"`
	Title   string `orm:"varchar(64) 'title'"`
	Version int    `orm:"version"`
}

// TestOrmTagVersionOptimisticLock version 原生语义四断言：
// 插入置 1；struct 更新（Save）条件含旧版本 + 自增 + 回填；陈旧版本更新
// 0 行不报错；map 更新 version 不参与（不进 SET 也不进 WHERE）。
func TestOrmTagVersionOptimisticLock(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&versionDoc{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	q := drv.Query()

	// 1. 插入置 1 并回填 struct
	doc := &versionDoc{ID: "v1", Title: "t1"}
	if err := q.Create(doc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("插入后 version 应置 1，实际 %d", doc.Version)
	}

	// 2. struct 更新：WHERE version = 旧值，SET version = version + 1，回填 struct
	got := &versionDoc{}
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Take(got); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("读回 version 应为 1，实际 %d", got.Version)
	}
	got.Title = "t2"
	if err := q.Save(got); err != nil {
		t.Fatalf("Save(version 更新): %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("struct 更新后 version 应自增并回填为 2，实际 %d", got.Version)
	}
	var after versionDoc
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Take(&after); err != nil {
		t.Fatalf("更新后读回失败: %v", err)
	}
	if after.Version != 2 || after.Title != "t2" {
		t.Fatalf("更新未落库: %+v", after)
	}

	// 3. 陈旧版本（version=1，库中已是 2）：0 行命中不报错，数据不变
	stale := versionDoc{ID: "v1", Title: "stale", Version: 1}
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Updates(&stale); err != nil {
		t.Fatalf("陈旧版本更新应 0 行不报错: %v", err)
	}
	var unchanged versionDoc
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Take(&unchanged); err != nil {
		t.Fatalf("陈旧更新后读回失败: %v", err)
	}
	if unchanged.Version != 2 || unchanged.Title != "t2" {
		t.Fatalf("陈旧版本更新不应改动数据: %+v", unchanged)
	}

	// 4. map 更新：version 不参与（不进 SET 也不进 WHERE）
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Updates(map[string]any{"title": "m2"}); err != nil {
		t.Fatalf("map 更新失败: %v", err)
	}
	var afterMap versionDoc
	if err := q.Model(&versionDoc{}).Where("id = ?", "v1").Take(&afterMap); err != nil {
		t.Fatalf("map 更新后读回失败: %v", err)
	}
	if afterMap.Title != "m2" {
		t.Fatalf("map 更新未生效: %+v", afterMap)
	}
	if afterMap.Version != 2 {
		t.Fatalf("map 更新不应改动 version，实际 %d", afterMap.Version)
	}
}

// ── identifier=orm 下的关联预加载（共享引擎 + orm tag 忽略标记 + rel tag）──

// ormUser has-many ormOrder；外键非约定列名（user_id 而非 orm_user_id），
// 必须经 rel tag 显式声明；关联字段以 orm:"-" 忽略（identifier=orm 下原生生效）。
type ormUser struct {
	ID     string     `orm:"pk varchar(16) 'id'"`
	Name   string     `orm:"varchar(64) 'name'"`
	Orders []ormOrder `orm:"-" rel:"foreignKey:UserID;references:ID"`
}

type ormOrder struct {
	ID     string    `orm:"pk varchar(16) 'id'"`
	UserID string    `orm:"varchar(16) 'user_id'"`
	Total  int       `orm:"default(0)"`
	Items  []ormItem `orm:"-" rel:"foreignKey:OrderID;references:ID"`
}

type ormItem struct {
	ID      string `orm:"pk varchar(16) 'id'"`
	OrderID string `orm:"varchar(16) 'order_id'"`
	Sku     string `orm:"varchar(64) 'sku'"`
}

// TestOrmTagPreloadNested identifier=orm 下嵌套 Preload "Orders.Items" 走共享
// 引擎仍正确：rel tag 显式外键、orm:"-" 忽略标记、批量 IN、嵌套递归。
func TestOrmTagPreloadNested(t *testing.T) {
	drv, _ := newXormTestDriverWithIdentifier(t, "orm")
	if err := drv.AutoMigrate(&ormUser{}, &ormOrder{}, &ormItem{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	for _, ddl := range []string{
		"INSERT INTO orm_user (id, name) VALUES ('u1', 'alice'), ('u2', 'bob')",
		"INSERT INTO orm_order (id, user_id, total) VALUES " +
			"('o1', 'u1', 10), ('o2', 'u1', 20), ('o3', 'u2', 30)",
		"INSERT INTO orm_item (id, order_id, sku) VALUES " +
			"('i1', 'o1', 'sku1'), ('i2', 'o2', 'sku2'), ('i3', 'o3', 'sku3')",
	} {
		if _, err := drv.engine.Exec(ddl); err != nil {
			t.Fatalf("灌数失败 %q: %v", ddl, err)
		}
	}

	var users []ormUser
	if err := drv.Query().Preload("Orders.Items").Find(&users); err != nil {
		t.Fatalf("嵌套预加载查询失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("应查到 2 个用户，实际 %d", len(users))
	}
	byID := make(map[string]*ormUser, len(users))
	for i := range users {
		byID[users[i].ID] = &users[i]
	}
	u1 := byID["u1"]
	if len(u1.Orders) != 2 {
		t.Fatalf("u1 应预加载 2 个订单，实际 %d", len(u1.Orders))
	}
	itemSkuByOrder := make(map[string]string, len(u1.Orders))
	for _, o := range u1.Orders {
		if len(o.Items) != 1 {
			t.Fatalf("订单 %s 应预加载 1 个明细，实际 %d", o.ID, len(o.Items))
		}
		itemSkuByOrder[o.ID] = o.Items[0].Sku
	}
	if itemSkuByOrder["o1"] != "sku1" || itemSkuByOrder["o2"] != "sku2" {
		t.Fatalf("u1 订单明细回填错误: %v", itemSkuByOrder)
	}
	u2 := byID["u2"]
	if len(u2.Orders) != 1 || len(u2.Orders[0].Items) != 1 || u2.Orders[0].Items[0].Sku != "sku3" {
		t.Fatalf("u2 订单 o3 应预加载明细 sku3，实际 %+v", u2.Orders)
	}
}
