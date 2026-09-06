package xormdriver

import (
	"sync"
	"testing"
	"time"

	"github.com/zhoudm1743/go-fast-framework/cache"
	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// ── 缓存替身 ─────────────────────────────────────────────────────────
//
// 与 gormdriver cacher_test.go 同款思路：用框架内存 store 包装出 contracts.Cache。
// 额外叠加 get/put 调用计数，使命中/回源可以直接从计数断言（数据新旧是行为证据，
// 计数是实现证据，两者互为印证）。

// countCacheStore 带调用计数的 CacheStore 装饰器。queryCache.get 走 Store().Get，
// put 走 Store().Tags(...).Put 两条路径，故 Get 与 Tags 返回的 TaggedCache.Put
// 都要计数才能观测完整读写。
type countCacheStore struct {
	contracts.CacheStore

	mu   sync.Mutex
	gets int // Get 调用总次数（含未命中）
	hits int // Get 返回非 nil 的次数（命中）
	puts int // Put 写入次数（含 Tags 包装路径）
}

func (s *countCacheStore) Get(key string, def ...any) any {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	v := s.CacheStore.Get(key, def...)
	if v != nil {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
	}
	return v
}

func (s *countCacheStore) Tags(tags ...string) contracts.TaggedCache {
	return &countTaggedCache{TaggedCache: s.CacheStore.Tags(tags...), s: s}
}

// snapshot 原子读取计数（put 路径与断言可能跨 goroutine，避免数据竞争误报）。
func (s *countCacheStore) snapshot() (gets, hits, puts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.hits, s.puts
}

// countTaggedCache 对 Tags() 返回的 TaggedCache 的 Put 计数后透传。
type countTaggedCache struct {
	contracts.TaggedCache
	s *countCacheStore
}

func (t *countTaggedCache) Put(key string, value any, ttl time.Duration) error {
	t.s.mu.Lock()
	t.s.puts++
	t.s.mu.Unlock()
	return t.TaggedCache.Put(key, value, ttl)
}

// testCache 测试用 contracts.Cache 实现：所有 store 名路由到同一计数 store
// （默认 CacheConfig.Store 为空串，单 store 足够覆盖）。
type testCache struct {
	*countCacheStore
}

func (c *testCache) Store(_ string) contracts.CacheStore { return c.countCacheStore }

func newTestCache() *testCache {
	return &testCache{countCacheStore: &countCacheStore{CacheStore: cache.NewMemoryStore(16, 0)}}
}

// newXormCacheDriver 带模型表且已启用查询缓存的驱动，同时返回计数 store 供命中断言。
func newXormCacheDriver(t *testing.T) (*XormDriver, *countCacheStore) {
	t.Helper()
	drv := newXormTestDriverWithModel(t)
	tc := newTestCache()
	if err := drv.EnableCaches(tc); err != nil {
		t.Fatalf("启用查询缓存失败: %v", err)
	}
	return drv, tc.countCacheStore
}

// assertCounts 断言缓存读写计数，stage 标注断言所处阶段便于定位。
func assertCounts(t *testing.T, s *countCacheStore, stage string, wantGets, wantHits, wantPuts int) {
	t.Helper()
	gets, hits, puts := s.snapshot()
	if gets != wantGets || hits != wantHits || puts != wantPuts {
		t.Fatalf("%s: 缓存计数不符 get=%d(期望 %d) hit=%d(期望 %d) put=%d(期望 %d)",
			stage, gets, wantGets, hits, wantHits, puts, wantPuts)
	}
}

// engineExec 绕过驱动写层直接执行原生 SQL：不触发 invalidateCache，
// 用于在保持缓存完整的前提下修改底层数据（制造"缓存过期但未失效"场景）。
func engineExec(t *testing.T, drv *XormDriver, sql string) {
	t.Helper()
	if _, err := drv.engine.Exec(sql); err != nil {
		t.Fatalf("直接改库失败 %q: %v", sql, err)
	}
}

// ── 未启用缓存：Cache() 无副作用 ─────────────────────────────────────

// TestXormCache_DisabledNoSideEffect 未 EnableCaches 时 Cache() 仅是标记：
// 驱动级缓存为 nil，查询始终直查数据库并返回正确结果。
func TestXormCache_DisabledNoSideEffect(t *testing.T) {
	drv := newXormTestDriverWithModel(t) // 不调用 EnableCaches
	if drv.qc != nil {
		t.Fatalf("未 EnableCaches 时驱动级缓存应为 nil，实际 %v", drv.qc)
	}
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "d1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	// 带 Cache() 标记的查询正常执行、结果正确
	var first []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&first); err != nil {
		t.Fatalf("未启用缓存时 Cache() 查询失败: %v", err)
	}
	if len(first) != 1 || first[0].Name != "alice" {
		t.Fatalf("查询结果错误: %+v", first)
	}

	// 直接改库新增一条同名行后再查同链路：若误走缓存只会返回旧结果集，
	// 返回两行证明每次都直查数据库（无任何缓存读写）
	engineExec(t, drv, `INSERT INTO xorm_test_model (id, name) VALUES ('d2', 'alice')`)
	var second []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&second); err != nil {
		t.Fatalf("二次 Cache() 查询失败: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("未启用缓存应直查数据库返回 2 行，实际 %d 行", len(second))
	}
}

// ── 回源与命中 ───────────────────────────────────────────────────────

// TestXormCache_FindMissThenHit 首次 Find 回源并回填缓存；绕过失效机制改库后
// 相同查询命中缓存返回旧结果集——命中不回源的最直接证据。
func TestXormCache_FindMissThenHit(t *testing.T) {
	drv, store := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "c1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	// 首次查询：get 未命中 → 执行 SQL → put 回填
	var first []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&first); err != nil {
		t.Fatalf("首次缓存查询失败: %v", err)
	}
	if len(first) != 1 || first[0].ID != "c1" {
		t.Fatalf("首次查询结果错误: %+v", first)
	}
	assertCounts(t, store, "首次查询应回源", 1, 0, 1)

	// 绕过驱动写层插入第二条 alice 行（不触发失效），此时 DB 有 2 行、缓存 1 行
	engineExec(t, drv, `INSERT INTO xorm_test_model (id, name) VALUES ('c2', 'alice')`)

	// 相同查询命中缓存：返回旧结果集（仅 c1），未回源（put 计数不变）
	var second []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&second); err != nil {
		t.Fatalf("命中缓存查询失败: %v", err)
	}
	if len(second) != 1 || second[0].ID != "c1" {
		t.Fatalf("应命中缓存返回旧结果集 [c1]，实际 %+v", second)
	}
	assertCounts(t, store, "二次查询应命中", 2, 1, 1)
}

// TestXormCache_DifferentWhereDifferentKey 不同 Where 条件生成不同缓存键：
// 各条件独立回源/命中，互不串值；已缓存的条件在底层数据变更后仍返回旧值。
func TestXormCache_DifferentWhereDifferentKey(t *testing.T) {
	drv, _ := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "w1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if err := q.Create(&XormTestModel{ID: "w2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var as []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&as); err != nil {
		t.Fatalf("alice 条件查询失败: %v", err)
	}
	if len(as) != 1 || as[0].ID != "w1" {
		t.Fatalf("alice 条件结果错误: %+v", as)
	}

	var bs []*XormTestModel
	if err := q.Cache().Where("name = ?", "bob").Find(&bs); err != nil {
		t.Fatalf("bob 条件查询失败: %v", err)
	}
	if len(bs) != 1 || bs[0].ID != "w2" {
		t.Fatalf("不同 Where 应使用不同缓存键，各自回源，实际 %+v", bs)
	}

	// 底层数据变更：新增一条 alice、bob 改名 bobby（均绕过失效）
	engineExec(t, drv, `INSERT INTO xorm_test_model (id, name) VALUES ('w3', 'alice')`)
	engineExec(t, drv, `UPDATE xorm_test_model SET name = 'bobby' WHERE id = 'w2'`)

	// 已缓存条件命中旧结果：alice 仍 1 行（DB 实际 2 行）
	var as2 []*XormTestModel
	if err := q.Cache().Where("name = ?", "alice").Find(&as2); err != nil {
		t.Fatalf("alice 二次查询失败: %v", err)
	}
	if len(as2) != 1 || as2[0].ID != "w1" {
		t.Fatalf("同条件应命中缓存返回旧结果集，实际 %+v", as2)
	}

	// 新条件不在缓存：回源读到改名后的数据
	var bs2 []*XormTestModel
	if err := q.Cache().Where("name = ?", "bobby").Find(&bs2); err != nil {
		t.Fatalf("bobby 条件查询失败: %v", err)
	}
	if len(bs2) != 1 || bs2[0].ID != "w2" {
		t.Fatalf("新条件应回源返回新结果，实际 %+v", bs2)
	}

	// 旧条件 bob 已无匹配行，但缓存仍在：命中返回旧行——键隔离的又一佐证
	var bsOld []*XormTestModel
	if err := q.Cache().Where("name = ?", "bob").Find(&bsOld); err != nil {
		t.Fatalf("旧条件查询失败: %v", err)
	}
	if len(bsOld) != 1 || bsOld[0].ID != "w2" {
		t.Fatalf("旧条件应命中自身缓存（DB 已无 name=bob 行），实际 %+v", bsOld)
	}
}

// ── 写失效 ───────────────────────────────────────────────────────────

// TestXormCache_InvalidateOnCreate 写终结（Create/Update）成功后失效全部查询缓存：
// 失效后相同查询回源，命中计数不增长、回填计数增长。
func TestXormCache_InvalidateOnCreate(t *testing.T) {
	drv, store := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "i1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var v1 []*XormTestModel
	if err := q.Cache().Find(&v1); err != nil {
		t.Fatalf("首次缓存查询失败: %v", err)
	}
	if len(v1) != 1 {
		t.Fatalf("首次查询应 1 行，实际 %d", len(v1))
	}
	assertCounts(t, store, "首次查询回源", 1, 0, 1)

	// 经驱动写层 Create：成功后 invalidateCache 清空查询缓存
	if err := q.Create(&XormTestModel{ID: "i2", Name: "bob"}); err != nil {
		t.Fatalf("二次插入失败: %v", err)
	}

	var v2 []*XormTestModel
	if err := q.Cache().Find(&v2); err != nil {
		t.Fatalf("失效后查询失败: %v", err)
	}
	if len(v2) != 2 {
		t.Fatalf("写后应失效缓存并回源返回 2 行，实际 %d 行: %+v", len(v2), v2)
	}
	// hits 不增长证明是失效回源而非命中旧值；put 增长证明回源后重新回填
	assertCounts(t, store, "失效后应回源", 2, 0, 2)

	// Update 同样触发失效
	if err := q.Model(&XormTestModel{}).Where("id = ?", "i2").Update("name", "bob2"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	var v3 []*XormTestModel
	if err := q.Cache().Find(&v3); err != nil {
		t.Fatalf("更新后查询失败: %v", err)
	}
	if len(v3) != 2 {
		t.Fatalf("更新后应 2 行，实际 %d", len(v3))
	}
	for _, m := range v3 {
		if m.ID == "i2" && m.Name != "bob2" {
			t.Fatalf("更新后缓存已失效应读到新值 bob2，实际 %v", m.Name)
		}
	}
	assertCounts(t, store, "更新失效后应回源", 3, 0, 3)
}

// ── Count 的缓存与失效 ───────────────────────────────────────────────

// TestXormCache_CountCacheAndInvalidate Count 走 withCache：首次回源回填、
// 数据变更后命中旧计数值、驱动写层操作后失效回源。
func TestXormCache_CountCacheAndInvalidate(t *testing.T) {
	drv, store := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "n1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	var n1 int64
	if err := q.Cache().Model(&XormTestModel{}).Count(&n1); err != nil {
		t.Fatalf("首次 Count 失败: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("首次 Count 应为 1，实际 %d", n1)
	}
	assertCounts(t, store, "Count 首次应回源", 1, 0, 1)

	// 直接改库加一行：缓存命中旧计数 1，DB 实际 2 行
	engineExec(t, drv, `INSERT INTO xorm_test_model (id, name) VALUES ('n2', 'bob')`)
	var n2 int64
	if err := q.Cache().Model(&XormTestModel{}).Count(&n2); err != nil {
		t.Fatalf("命中 Count 失败: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("应命中缓存返回旧计数 1，实际 %d", n2)
	}
	// 绕开驱动 Scan 链路直查连接池，核实 DB 真实行数（排除"其实没查到"的假命中）
	var dbCount int64
	if err := drv.engine.DB().QueryRow("SELECT COUNT(*) FROM xorm_test_model").Scan(&dbCount); err != nil {
		t.Fatalf("直查数据库行数失败: %v", err)
	}
	if dbCount != 2 {
		t.Fatalf("数据库实际应为 2 行，得到 %d", dbCount)
	}
	assertCounts(t, store, "Count 二次应命中", 2, 1, 1)

	// 经驱动写层 Create 第三行：失效后 Count 回源返回 3
	if err := q.Create(&XormTestModel{ID: "n3", Name: "carol"}); err != nil {
		t.Fatalf("三次插入失败: %v", err)
	}
	var n3 int64
	if err := q.Cache().Model(&XormTestModel{}).Count(&n3); err != nil {
		t.Fatalf("失效后 Count 失败: %v", err)
	}
	if n3 != 3 {
		t.Fatalf("写失效后 Count 应回源为 3，实际 %d", n3)
	}
	assertCounts(t, store, "Count 失效后应回源", 3, 1, 2)
}

// ── Row/Rows 游标不受缓存影响 ────────────────────────────────────────

// TestXormCache_RowRowsBypassCache 游标类终结方法直接走连接池执行原生 SQL：
// 即使链上带 Cache() 标记也不读写查询缓存（命中会产生空游标或旧数据，必须绕开），
// 改库后立即返回最新值且不报错。
func TestXormCache_RowRowsBypassCache(t *testing.T) {
	drv, store := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "r1", Name: "eve"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	// Row() 单行扫描：正确返回且不产生任何缓存读写
	var id, name string
	if err := q.Cache().Raw("SELECT id, name FROM xorm_test_model WHERE id = ?", "r1").
		Row().Scan(&id, &name); err != nil {
		t.Fatalf("Cache().Row() 扫描失败: %v", err)
	}
	if id != "r1" || name != "eve" {
		t.Fatalf("Row() 结果错误: %s/%s", id, name)
	}
	assertCounts(t, store, "Row 不应读写缓存", 0, 0, 0)

	// 改库后再次 Row：返回新值（未走缓存）
	engineExec(t, drv, `UPDATE xorm_test_model SET name = 'eve2' WHERE id = 'r1'`)
	if err := q.Cache().Raw("SELECT name FROM xorm_test_model WHERE id = ?", "r1").
		Row().Scan(&name); err != nil {
		t.Fatalf("二次 Row() 扫描失败: %v", err)
	}
	if name != "eve2" {
		t.Fatalf("Row() 不应走缓存，期望 eve2 实际 %v", name)
	}

	// Rows() 多行遍历：游标正常工作、数据完整
	if err := q.Create(&XormTestModel{ID: "r2", Name: "bob"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	rows, err := q.Cache().Raw("SELECT id, name FROM xorm_test_model ORDER BY id").Rows()
	if err != nil {
		t.Fatalf("Cache().Rows() 失败: %v", err)
	}
	defer rows.Close()
	cnt := 0
	for rows.Next() {
		var rid, rname string
		if err := rows.Scan(&rid, &rname); err != nil {
			t.Fatalf("Rows() 扫描失败: %v", err)
		}
		cnt++
	}
	if cnt != 2 {
		t.Fatalf("Rows() 应遍历 2 行，实际 %d", cnt)
	}
	assertCounts(t, store, "Rows 不应读写缓存", 0, 0, 0)
}

// ── 缓存键的 dest 类型隔离 ───────────────────────────────────────────

// cacheProjectionRow 与 XormTestModel 同表查询的投影结构体：列 tag 对齐真实列名
// （xorm SnakeMapper 会把无 tag 的 ID 推导成 i_d 列），类型本身与 XormTestModel
// 不同才是本测试关注的维度；表推导值 cache_projection_row 与实际表不符，
// 查询须显式 Table。
type cacheProjectionRow struct {
	ID   string `xorm:"'id'"`
	Name string `xorm:"'name'"`
}

// TestXormCache_DestTypeIsolation 缓存键绑定 dest 类型（buildCacheKey 的 "#t="）：
// 同链路同条件、仅 dest 类型不同的两次查询不共享缓存，互不串值。
func TestXormCache_DestTypeIsolation(t *testing.T) {
	drv, store := newXormCacheDriver(t)
	q := drv.Query()

	if err := q.Create(&XormTestModel{ID: "t1", Name: "typed"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	// 以模型类型回源写缓存（两条链的 keyParts 完全一致：table:xorm_test_model）
	var models []XormTestModel
	if err := q.Cache().Table("xorm_test_model").Find(&models); err != nil {
		t.Fatalf("模型类型首次查询失败: %v", err)
	}
	if len(models) != 1 || models[0].Name != "typed" {
		t.Fatalf("模型类型查询结果错误: %+v", models)
	}
	assertCounts(t, store, "模型类型首次回源", 1, 0, 1)

	// 直接改库（不触发失效）
	engineExec(t, drv, `UPDATE xorm_test_model SET name = 'retyped' WHERE id = 't1'`)

	// 换投影类型同条件查询：若 key 不区分类型会命中模型的缓存返回旧值 typed；
	// 类型隔离生效时应回源读到 retyped（回源后同样回填自己的缓存键）
	var rows []cacheProjectionRow
	if err := q.Cache().Table("xorm_test_model").Find(&rows); err != nil {
		t.Fatalf("投影类型查询失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "retyped" {
		t.Fatalf("不同 dest 类型不应共享缓存，期望回源 retyped，实际 %+v", rows)
	}

	// 模型类型再查：命中自身缓存，仍为旧值 typed
	var models2 []XormTestModel
	if err := q.Cache().Table("xorm_test_model").Find(&models2); err != nil {
		t.Fatalf("模型类型二次查询失败: %v", err)
	}
	if len(models2) != 1 || models2[0].Name != "typed" {
		t.Fatalf("模型类型应命中自身缓存返回 typed，实际 %+v", models2)
	}

	// 投影类型再查：命中自己的缓存键，仍为回源时的 retyped
	var rows2 []cacheProjectionRow
	if err := q.Cache().Table("xorm_test_model").Find(&rows2); err != nil {
		t.Fatalf("投影类型二次查询失败: %v", err)
	}
	if len(rows2) != 1 || rows2[0].Name != "retyped" {
		t.Fatalf("投影类型应命中自身缓存返回 retyped，实际 %+v", rows2)
	}
	assertCounts(t, store, "类型隔离后各自命中/回源", 4, 2, 2)
}

// ── EnableCaches 守卫 ────────────────────────────────────────────────

// TestXormCache_EnableCachesGuards nil cache 报错；重复启用安全（仅首次生效，
// 后续调用不替换已生效实例）。
func TestXormCache_EnableCachesGuards(t *testing.T) {
	drv := newXormTestDriverWithModel(t)

	if err := drv.EnableCaches(nil); err == nil {
		t.Fatal("nil cache 应返回错误")
	}

	tc := newTestCache()
	if err := drv.EnableCaches(tc); err != nil {
		t.Fatalf("启用查询缓存失败: %v", err)
	}
	if drv.qc == nil {
		t.Fatal("EnableCaches 后驱动级缓存不应为 nil")
	}

	// 重复调用不报错也不替换实例：后续缓存读写仍落在第一个 store 上
	if err := drv.EnableCaches(newTestCache()); err != nil {
		t.Fatalf("重复启用应安全返回 nil: %v", err)
	}
	q := drv.Query()
	if err := q.Create(&XormTestModel{ID: "g1", Name: "alice"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	var models []*XormTestModel
	if err := q.Cache().Find(&models); err != nil {
		t.Fatalf("缓存查询失败: %v", err)
	}
	gets, _, puts := tc.countCacheStore.snapshot()
	if gets == 0 || puts == 0 {
		t.Fatalf("缓存应落在首次启用的 store 上: get=%d put=%d", gets, puts)
	}
}
