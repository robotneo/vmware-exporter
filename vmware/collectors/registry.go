package vmwareCollectors

import (
	"log/slog"
	"sort"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
)

// Definition 描述一个 collector 的身份、构造方式与默认开关状态。
type Definition struct {
	// Name 是 collector 的规范名，同时用于：
	//   - 命令行开关 -collector.<Name>（框架注册用）
	//   - /probe 的 collect[]=<Name> / nocollect[]=<Name>
	//   - vmware_scrape_collector_duration_seconds{collector="<Name>"} 的标签值
	// 三处必须一致，否则 /metrics 与 /probe 的开关行为会分叉。
	Name string

	// Creator 构造 collector 实例。
	Creator func(*slog.Logger) (collector.Collector, error)

	// DefaultEnabled 决定未显式指定时是否启用。
	DefaultEnabled bool
}

// definitions 是全项目 collector 清单的**唯一来源**。
//
// 改动历史：此前清单散落三处 —— 各 collector 的 init() 向框架注册一份、
// 根包 vmwareCollector.Collect 硬编码一份 slice、parseCollectors 里
// "all" 分支又硬编码一份字符串列表。三份不同步时 /metrics 与 /probe
// 的可用 collector 就会不一致，新增 collector 极易漏改。
//
// 现在 Collect 与 parseCollectors 都从这里取；各 collector 的 init()
// 仍走框架的 RegisterCollector（框架的 collectorState 是私有的，无法反查），
// 两边的一致性由 TestDefinitionsMatchRegisteredFlags 保证。
var definitions = []Definition{
	// 基础 collectors，默认启用。
	{Name: "datacenter", Creator: NewdatacenterCollector, DefaultEnabled: collector.DefaultEnabled},
	{Name: "cluster", Creator: NewClusterCollector, DefaultEnabled: collector.DefaultEnabled},
	{Name: "datastore", Creator: NewdatastoreCollector, DefaultEnabled: collector.DefaultEnabled},
	{Name: "host", Creator: NewhostCollector, DefaultEnabled: collector.DefaultEnabled},
	{Name: "vm", Creator: NewvmCollector, DefaultEnabled: collector.DefaultEnabled},

	// esxcli collectors 逐主机串行发 SOAP 调用，开销显著，默认禁用。
	{Name: "esxcli.host.nic", Creator: NewesxcliHostNICCollector, DefaultEnabled: collector.DefaultDisabled},
	{Name: "esxcli.storage", Creator: NewesxcliStorageListCCollector, DefaultEnabled: collector.DefaultDisabled},
}

// Definitions 返回 collector 清单的副本，按 Name 排序以保证调度顺序稳定
// （便于日志比对与测试断言）。返回副本是为了防止调用方意外改动全局状态。
func Definitions() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

// Names 返回所有 collector 的规范名，已排序。
func Names() []string {
	defs := Definitions()

	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}

	return names
}
