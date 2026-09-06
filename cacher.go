package xormdriver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zhoudm1743/go-fast-framework/contracts"
)

// 查询缓存相关常量。
const (
	// queryCacheTag 所有查询缓存共用的失效标签。
	// 写操作（Create/Update/Delete/Save）终结成功时按此标签整体清除，
	// 不干扰其他业务缓存（框架 Tags 机制仅删除关联 key）。
	queryCacheTag = "xorm:query:cache"
)

// EnableCaches 为连接启用查询缓存（实现 contracts.QueryCacher，由数据库管理器调用）。
// 底层使用框架 Cache 服务存储，只有显式调用 Query().Cache() 的查询才会读写缓存，
// 其余查询零副作用；写操作（Create/Update/Delete/Save）会自动失效全部查询缓存。
// 重复调用安全（仅首次启用生效）。
func (d *XormDriver) EnableCaches(cache contracts.Cache) error {
	if cache == nil {
		return fmt.Errorf("[GoFast] 启用查询缓存失败: cache 服务为空")
	}
	if d.qc != nil {
		return nil
	}
	d.qc = newQueryCache(cache)
	return nil
}

// queryCache 查询缓存实现，底层使用框架的 Cache 服务存储查询结果。
// 通过 Cache() 显式标记实现"按需缓存"：只有显式调用 Query().Cache() 的查询
// 才会读写缓存，其余查询零副作用。
// 记录已使用的存储名（stores）：写操作失效时按 store 逐个清除，避免全库 Flush。
type queryCache struct {
	cache contracts.Cache

	mu     sync.Mutex
	stores map[string]struct{} // 已使用的缓存存储名（"" 表示框架默认存储）
}

func newQueryCache(cache contracts.Cache) *queryCache {
	return &queryCache{cache: cache, stores: make(map[string]struct{})}
}

// get 读取查询缓存并归一为字节切片；未命中返回 ok=false（调用方回源查询）。
func (qc *queryCache) get(store, key string) ([]byte, bool) {
	data := qc.cache.Store(store).Get(key)
	if data == nil {
		return nil, false
	}
	return toBytes(data)
}

// put 将查询结果写入缓存并追加统一失效标签（外加用户自定义标签）。
// 写缓存失败不影响查询结果（本次已从数据库回源，仅损失下次命中机会），故忽略错误。
func (qc *queryCache) put(store, key string, val []byte, ttl time.Duration, tags []string) {
	qc.recordStore(store)
	_ = qc.cache.Store(store).Tags(append([]string{queryCacheTag}, tags...)...).Put(key, val, ttl)
}

// invalidateAll 清除所有已使用的缓存存储上的查询缓存。
// 由写终结方法（Create/Update/Delete/Save）成功后调用。
// 失效失败不阻断写操作（数据已落库，仅可能读到一次旧值），故聚合错误后仅作记录，
// 调用方不处理返回值。
func (qc *queryCache) invalidateAll() {
	qc.mu.Lock()
	storeNames := make([]string, 0, len(qc.stores))
	for name := range qc.stores {
		storeNames = append(storeNames, name)
	}
	qc.mu.Unlock()

	var errs []error
	for _, name := range storeNames {
		if err := qc.cache.Store(name).Tags(queryCacheTag).Flush(); err != nil {
			errs = append(errs, fmt.Errorf("缓存存储 %q 失效失败: %w", name, err))
		}
	}
	_ = errors.Join(errs...)
}

func (qc *queryCache) recordStore(name string) {
	qc.mu.Lock()
	qc.stores[name] = struct{}{}
	qc.mu.Unlock()
}

// buildCacheKey 将链式条件生成的键片段与 dest 类型绑定，生成最终缓存 key。
// 原因：键片段仅由 SQL 条件 + 参数组成，同一 SQL 若被不同 dest 类型复用，
// 命中时会把缓存的 JSON 反序列化进当前 dest 类型（json 复用指针类型），导致返回错误数据。
// 追加 dest 类型后，不同结构的查询天然隔离，且不影响按 tag 的失效逻辑。
// 片段以 \x1f（单元分隔符）拼接后再哈希，避免片段内容拼接后产生歧义碰撞；
// dest 为 nil 时仅用哈希（无类型维度）；parts 为空时哈希空串，结果依然稳定。
func buildCacheKey(parts []string, dest any) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	key := hex.EncodeToString(sum[:])
	if dest == nil {
		return key
	}
	return key + "#t=" + reflect.TypeOf(dest).String()
}

// toBytes 将缓存值还原为字节切片。
// 框架 memory/redis store 对 []byte 均原样返回（redis store 经 "b:" 前缀实现字节保真，
// 避免 json.Marshal 将 []byte 编码为 base64 导致双重编码）。
func toBytes(v any) ([]byte, bool) {
	switch b := v.(type) {
	case []byte:
		return b, true
	case string:
		return []byte(b), true
	default:
		return nil, false
	}
}
