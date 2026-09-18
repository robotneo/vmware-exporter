package vmwareCollectors

import (
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// esxcliDescCache 按 (fqName, const label 组合) 复用 *prometheus.Desc。
//
// 为什么需要它（P-01）：host/vm/perf 当年把「实体循环内 NewDesc + 把实体值
// 塞进 constLabels」改造成了 Desc 复用（见 descs.go 顶部 P2-4），但 esxcli 两个
// collector 没纳入那次改造。它们把逐实体的 mo/host 连同驱动/型号信息一起塞进
// constLabels，于是即便全环境是同一型号网卡/同一驱动版本，每台主机仍要 NewDesc
// 一次（mo/host 必不同），500 台主机就是 500 次 fqName 拼接、label 校验与 map
// 拷贝，纯属 GC 压力。
//
// 修法与 P2-4 相同：逐实体的标识（mo/host）做成 variableLabels，在发指标时传
// 值；真正决定「这是哪一种驱动/固件」的描述性 label 留在 constLabels。输出的
// 时间序列 label 名/值/基数与改前逐字一致 —— constLabels 与 variableLabels 只
// 影响 Desc 复用，不影响 exposition。缓存键因此只含 fqName + const 组合，跨
// 主机命中率高：同型号硬件共用一个 Desc。
//
// 生命周期：缓存挂在 collector 实例上，而 CollectorSet 每轮抓取新建实例，因此
// 缓存自然随一轮抓取回收，不会被罕见硬件组合长期堆积 key。esxcli fan-out 是多
// goroutine 并发，读写需要同一把锁。
type esxcliDescCache struct {
	mu    sync.Mutex
	descs map[string]*prometheus.Desc
}

func newEsxcliDescCache() *esxcliDescCache {
	return &esxcliDescCache{descs: make(map[string]*prometheus.Desc)}
}

// get 返回 fqName + variableLabels + constLabels 组合对应的 Desc，缺失时构造
// 并缓存。variableLabels 对同一指标名是固定的，不进缓存键；键只含 fqName 与
// 每个 const label 的名/值，按名排序后拼接，不依赖 map 迭代顺序。
func (c *esxcliDescCache) get(fqName, help string, variableLabels []string, constLabels map[string]string) *prometheus.Desc {
	key := esxcliDescKey(fqName, constLabels)

	c.mu.Lock()
	defer c.mu.Unlock()

	if d, ok := c.descs[key]; ok {
		return d
	}

	d := prometheus.NewDesc(fqName, help, variableLabels, constLabels)
	c.descs[key] = d

	return d
}

func esxcliDescKey(fqName string, labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}

	sort.Strings(names)

	var b strings.Builder
	b.WriteString(fqName)
	for _, k := range names {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}

	return b.String()
}
