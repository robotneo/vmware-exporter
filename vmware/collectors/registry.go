package vmwareCollectors

import (
	"sort"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
)

// Definition 是 internal/collector.Definition 的别名。
//
// 别名而非重新声明：清单的消费方（根包的 /probe 处理、首页文档生成）拿到的
// 就是调度层认识的同一个类型，不需要任何转换代码。
type Definition = collector.Definition

// definitions 是全项目 collector 清单的**唯一来源**。
//
// 改动历史：此前清单散落三处 —— 各 collector 的 init() 向框架注册一份、
// 根包 vmwareCollector.Collect 硬编码一份 slice、parseCollectors 里
// "all" 分支又硬编码一份字符串列表。三份不同步时 /metrics 与 /probe
// 的可用 collector 就会不一致，新增 collector 极易漏改。
//
// 现在这份清单同时驱动 /metrics 与 /probe：两条路径共用
// internal/collector.CollectorSet，不再有第二份调度实现。
// 各 collector 的 init() 仍额外注册一个 -collector.<name> 命令行开关，
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
