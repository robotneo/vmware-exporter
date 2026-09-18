package vmware

import (
	"context"
	"sync"
	"time"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/sync/singleflight"
)

// CounterCache 缓存 PerfManager.CounterInfoByName 的结果（P-03）。
//
// 现状（无缓存）：每次登录都对全量 PerfCounterInfo 做一次 SOAP 往返，并把
// XML 解析成数千个 PerfCounterInfo、再建一次 by-name map（vcsim 基准约
// 15ms / 45KB / 577 allocs 每次登录；真实 vCenter 计数器更多、往返更慢）。
// 计数器表只随 vCenter 版本/补丁变化，一轮抓取间隔（秒级）内绝不会变，因此
// 按 target+About 版本缓存、用一个明显长于抓取间隔但有界的 TTL 是安全的。
//
// 边界与 InventoryCache 完全一致：只在使用单一服务级凭证的 /metrics 路径注入；
// /probe 多租户路径保持每请求实时（构造 VMware 时不传 *CounterCache）。
// 元数据敏感度低，但维持同一条越权读边界，避免两套标准。
//
// TTL 不存进结构而由每次 Get 传入：它来自 -scrape.counter-cache-ttl 的配置
// 快照，SIGHUP 改值立即生效，与 InventoryCache 同一套热重载语义。
//
// 有界性：/metrics 同一进程通常只有一个 target，但 vCenter 升级会换 build、
// 产生新 key。每次填充新值时顺手清掉所有已过期 entry，因此旧版本表最多在
// TTL 后被任意一次抓取回收，map 不会随升级次数无限增长。
//
// 并发：多个抓取同时撞上一个未填充/过期 key 时由 singleflight 合并成一次
// SOAP 往返；返回的 map 是同一只读实例，调用方绝不能修改（既有约定：
// Scrape.Counters 全程只读）。
type CounterCache struct {
	mu      sync.Mutex
	entries map[string]counterEntry

	sf singleflight.Group
}

type counterEntry struct {
	counters map[string]*types.PerfCounterInfo
	fetched  time.Time
}

// NewCounterCache 构造一个空的计数器缓存；TTL 由 Get 的参数决定，ttl<=0 即
// 旁路缓存。
func NewCounterCache() *CounterCache {
	return &CounterCache{entries: make(map[string]counterEntry)}
}

// counterCacheKey 把 target 与 vCenter 版本/build 拼成缓存键。
//
// Version/Build 必须进键：vCenter 升级或打补丁后计数器表可能增删改名，
// 带上版本后升级的第一个请求天然 miss、重新拉取，最多一个 TTL 内生效。
func counterCacheKey(target, version, build string) string {
	return target + "|" + version + "|" + build
}

// Get 返回 key 对应的 by-name 计数器表。ttl<=0 时旁路缓存、每次实时拉取。
// 命中且未过期直接返回缓存实例；否则（singleflight 合并下）调
// perf.CounterInfoByName，**仅成功时**写入缓存 —— 一次失败不能让后续抓取
// 在一个 TTL 里持续拿到空/错数据。
func (c *CounterCache) Get(ctx context.Context, key string, perf *performance.Manager, ttl time.Duration, now time.Time) (map[string]*types.PerfCounterInfo, error) {
	if c == nil || ttl <= 0 {
		return perf.CounterInfoByName(ctx)
	}

	return c.getWithFetcher(key, ttl, now, func() (map[string]*types.PerfCounterInfo, error) {
		return perf.CounterInfoByName(ctx)
	})
}

// getWithFetcher 是 Get 的可测试内核：fetch 抽象掉 *performance.Manager，
// 使 TTL/版本键/失败不缓存/singleflight 逻辑无需真实 SOAP 即可验证。
func (c *CounterCache) getWithFetcher(key string, ttl time.Duration, now time.Time,
	fetch func() (map[string]*types.PerfCounterInfo, error)) (map[string]*types.PerfCounterInfo, error) {
	if counters, ok := c.fresh(key, ttl, now); ok {
		return counters, nil
	}

	// singleflight：同一 key 的并发未命中只发一次 SOAP。返回值在组内共享。
	v, err, _ := c.sf.Do(key, func() (interface{}, error) {
		// 拿到锁前可能已有别的 goroutine 刚填好，再查一次避免多余往返。
		if counters, ok := c.fresh(key, ttl, now); ok {
			return counters, nil
		}

		counters, err := fetch()
		if err != nil {
			return nil, err
		}

		c.store(key, counters, ttl, now)

		return counters, nil
	})
	if err != nil {
		return nil, err
	}

	return v.(map[string]*types.PerfCounterInfo), nil
}

// fresh 返回未过期的缓存表；过期或不存在则返回 false（过期时删除该 key）。
func (c *CounterCache) fresh(key string, ttl time.Duration, now time.Time) (map[string]*types.PerfCounterInfo, bool) {
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

	return e.counters, true
}

// store 写入新值并回收全部过期 entry（覆盖升级换 build 后旧 key 永不再被
// 查询、fresh 惰性删除够不到的情况）。
func (c *CounterCache) store(key string, counters map[string]*types.PerfCounterInfo, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, e := range c.entries {
		if now.Sub(e.fetched) >= ttl {
			delete(c.entries, k)
		}
	}

	c.entries[key] = counterEntry{counters: counters, fetched: now}
}
