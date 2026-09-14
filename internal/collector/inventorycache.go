package collector

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// InventoryCache 是 vSphere 清单/属性检索结果的进程级 TTL 缓存。
//
// 为什么需要它：每个 collector 每轮抓取原本都做一次
// CreateContainerView + RetrieveProperties + Destroy（三次 SOAP 语义）。
// 清单拓扑（datacenter/cluster/datastore/resourcepool）与硬件配置几乎不变，
// 在数千实体的环境里，每次 20s 抓取都重放全量属性检索是主要开销。缓存命中时
// 这一整轮往返被跳过。
//
// 作用域与安全边界（刻意的设计，勿轻易放宽）：
//
//   - 进程级、跨请求存活，但按 target 分桶 —— Scrape 是每请求新建的，缓存
//     不能挂在 Scrape 上，否则永远命中不了第二次。
//   - 只缓存「属性/库存」面，绝不缓存性能计数器（perf 是实时数据）。
//   - /probe 多租户路径不注入本缓存（Scrape.InventoryCache 为 nil），否则低
//     权限凭证可能读到高权限凭证留下的对象清单 —— 那是越权读。只有使用单一
//     服务级凭证的 /metrics 路径才注入。
//   - 缓存内容对所有消费者**只读**。这与既有的 Scrape.Hosts() 请求内共享
//     同一份 []mo.HostSystem 的约定一致：各 collector 只遍历发指标，不修改
//     切片或其元素。刻意不做 gob/json 深拷贝 —— govmomi 的 mo 结构体含
//     指针与类型别名字段，序列化往返有丢字段风险，而只读共享没有这个风险。
type InventoryCache struct {
	mu sync.Mutex
	// entries 的 key 由 target + 对象类型集 + 属性集拼出，见 inventoryKey。
	entries map[string]inventoryEntry

	// sf 合并同一 key 上并发的缓存未命中：in-flight 抓取闸已限并发，但同刻
	// 多个请求对同一 vCenter 的首次（或同时过期的）检索仍应只发一次。
	sf singleflight.Group
}

type inventoryEntry struct {
	data      any // 实际类型为 []T，T 为某个 mo.* 结构体
	fetchedAt time.Time
}

// NewInventoryCache 构造一个空的进程级缓存。缓存不持有任何连接或会话 ——
// 缓存的只是上一次（已登出）会话拉回的纯数据，MoRef 在同一 vCenter 内跨会话
// 稳定，因此复用安全。
func NewInventoryCache() *InventoryCache {
	return &InventoryCache{entries: make(map[string]inventoryEntry)}
}

// FetchInventory 按 key 返回缓存的清单切片；未命中或过期时调用 load 拉取并
// 填入缓存。
//
// 泛型用顶层函数而非方法：Go（含 1.26）不支持带类型参数的方法。
//
// 任一「关闭缓存」条件成立都会直接调 load、完全不碰缓存：
//   - cache 为 nil（/probe 路径刻意不注入）；
//   - ttl <= 0（-scrape.inventory-cache-ttl=0，回到每轮实时检索）。
//
// load 返回 error 时不写缓存 —— 失败结果不被记住，下一轮立即重试，避免一次
// 瞬态超时在 TTL 内被反复重放。
func FetchInventory[T any](cache *InventoryCache, ttl time.Duration, key string, load func() ([]T, error)) ([]T, error) {
	if cache == nil || ttl <= 0 {
		return load()
	}

	if data, ok := cache.get(key, ttl); ok {
		typed, ok := data.([]T)
		if ok {
			return typed, nil
		}
		// 类型断言理论上不会失败：key 已含对象类型与属性集，同一 key 的 T 固定。
		// 真失败说明 key 拼接碰撞，落回重新拉取而不是 panic。
	}

	// singleflight.Group.Do 的回调返回 any；同一 key 的并发调用共享一次 load。
	v, err, _ := cache.sf.Do(key, func() (any, error) {
		// 拿到单飞权后再查一次：可能在等锁期间已由别的协程填好。
		if data, ok := cache.get(key, ttl); ok {
			return data, nil
		}

		fresh, err := load()
		if err != nil {
			return nil, err
		}

		cache.put(key, fresh)

		return fresh, nil
	})
	if err != nil {
		var zero []T
		return zero, err
	}

	return v.([]T), nil
}

// get 在锁内判断命中与新鲜度。命中但已过期按未命中处理。
func (c *InventoryCache) get(key string, ttl time.Duration) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	if time.Since(entry.fetchedAt) >= ttl {
		// 过期项顺手删除，避免 map 随被淘汰 key 无限增长。
		delete(c.entries, key)
		return nil, false
	}

	return entry.data, true
}

func (c *InventoryCache) put(key string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = inventoryEntry{data: data, fetchedAt: time.Now()}
}

// InventoryKey 拼出缓存键。target 区分 vCenter；kinds 与 props 进键是安全
// 必需 —— 同一个 mo 类型用不同属性集检索时，命中「属性更少」的旧结果会让
// 调用方读到一堆零值字段。各入参的次序即键的组成，调用方必须以稳定顺序传入
// （现有调用点都是包级常量切片，次序固定）。
func InventoryKey(target string, kinds, props []string) string {
	key := target + "|"
	for _, k := range kinds {
		key += k + ","
	}
	key += "|"
	for _, p := range props {
		key += p + ","
	}

	return key
}
