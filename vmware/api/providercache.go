package vmware

import (
	"context"
	"sync"
	"time"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/sync/singleflight"
)

// ProviderSummaryCache 缓存 PerfManager.ProviderSummary 的结果（按实体类型
// 协商采样间隔用，见 vmware/collectors/targettype.go）。
//
// 为什么需要它：govmomi 的 performance.Manager.ProviderSummary 文档注释声称
// "caching the value based on entity.Type"，但 v0.56.0 的实现里根本没有缓存 ——
// 每次调用都发一次 QueryPerfProviderSummary SOAP 往返（performance/manager.go）。
// 而本项目每轮抓取都要对 HostSystem、VirtualMachine、Datastore 各协商一次采样
// 间隔；performance.Manager 又是每次登录新建的，govmomi 即便将来补了缓存也救
// 不了跨登录的重复往返。于是默认配置下每个 scrape 周期固定多花约 3 次跨网络
// 往返，这里把它收成进程级缓存。
//
// 缓存粒度刻意做到 entity.Type 而不是具体某个实体：同一目标、同一版本下，同一
// 类型实体的 ProviderSummary（CurrentSupported/SummarySupported/RefreshRate）
// 是目标级能力，不随具体主机/VM 变化 —— 这也正是 govmomi 注释里 "based on
// entity.Type" 的含义。一轮里 host/vm 两个 goroutine、datastore 各查一次，命中
// 后整轮每种类型只发一次、跨轮一个 TTL 内零往返。
//
// 边界与 CounterCache / InventoryCache 完全一致：只在单一服务级凭证的 /metrics
// 路径注入；/probe 多租户路径构造的 VMware 不带本缓存，Scrape.ProviderSummary
// 为 nil，协商每请求实时进行。
//
// TTL 由每次 Get 传入（配置快照），SIGHUP 改值立即生效。有界性同 CounterCache：
// 填充新值时顺手清掉所有过期 entry，vCenter 升级换 build 产生的新 key 不会让 map
// 无限增长。并发未命中由 singleflight 合并成一次往返。
type ProviderSummaryCache struct {
	mu      sync.Mutex
	entries map[string]providerEntry

	sf singleflight.Group
}

type providerEntry struct {
	summary *types.PerfProviderSummary
	fetched time.Time
}

// NewProviderSummaryCache 构造一个空的采样间隔协商缓存；ttl<=0 即旁路。
func NewProviderSummaryCache() *ProviderSummaryCache {
	return &ProviderSummaryCache{entries: make(map[string]providerEntry)}
}

// providerCacheKey 把 target、vCenter 版本/build 与实体类型拼成缓存键。
//
// Version/Build 必须进键：升级或打补丁后 ProviderSummary 能力可能变化（例如
// RefreshRate 或是否支持历史汇总），带上版本后升级的第一个请求天然 miss。
func providerCacheKey(target, version, build, entityType string) string {
	return target + "|" + version + "|" + build + "|" + entityType
}

// Get 返回 key 对应的 ProviderSummary。ttl<=0 时旁路缓存、实时拉取。仅成功时
// 写入缓存 —— 一次失败不能让后续抓取在一个 TTL 里持续拿到错的协商结果。
func (c *ProviderSummaryCache) Get(ctx context.Context, key string, perf *performance.Manager, entity types.ManagedObjectReference, ttl time.Duration, now time.Time) (*types.PerfProviderSummary, error) {
	if c == nil || ttl <= 0 {
		return perf.ProviderSummary(ctx, entity)
	}

	return c.getWithFetcher(key, ttl, now, func() (*types.PerfProviderSummary, error) {
		return perf.ProviderSummary(ctx, entity)
	})
}

// getWithFetcher 是 Get 的可测试内核：fetch 抽象掉 *performance.Manager，
// 使 TTL/版本键/失败不缓存/singleflight 逻辑无需真实 SOAP 即可验证。
func (c *ProviderSummaryCache) getWithFetcher(key string, ttl time.Duration, now time.Time,
	fetch func() (*types.PerfProviderSummary, error)) (*types.PerfProviderSummary, error) {
	if summary, ok := c.fresh(key, ttl, now); ok {
		return summary, nil
	}

	v, err, _ := c.sf.Do(key, func() (interface{}, error) {
		// 拿锁前可能已有别的 goroutine 刚填好，再查一次避免多余往返。
		if summary, ok := c.fresh(key, ttl, now); ok {
			return summary, nil
		}

		summary, err := fetch()
		if err != nil {
			return nil, err
		}

		c.store(key, summary, ttl, now)

		return summary, nil
	})
	if err != nil {
		return nil, err
	}

	return v.(*types.PerfProviderSummary), nil
}

// fresh 返回未过期的缓存；过期或不存在则返回 false（过期时删除该 key）。
func (c *ProviderSummaryCache) fresh(key string, ttl time.Duration, now time.Time) (*types.PerfProviderSummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	if now.Sub(e.fetched) >= ttl {
		delete(c.entries, key)
		return nil, false
	}

	return e.summary, true
}

// store 写入新值并回收全部过期 entry（覆盖升级换 build 后旧 key 永不再被查询、
// fresh 惰性删除够不到的情况）。
func (c *ProviderSummaryCache) store(key string, summary *types.PerfProviderSummary, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, e := range c.entries {
		if now.Sub(e.fetched) >= ttl {
			delete(c.entries, k)
		}
	}

	c.entries[key] = providerEntry{summary: summary, fetched: now}
}
