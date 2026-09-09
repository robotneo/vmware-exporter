package vmwareCollectors

import (
	"flag"
	"strings"

	"github.com/prezhdarov/vmware-exporter/internal/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/types"
)

// legacyMetrics 控制是否同时导出旧命名的指标。
//
// 默认 false，也就是默认只导出规范化后的新名。这与「向后兼容优先」的直觉
// 相反，理由是本仓库的 dashboards/ 五个面板随代码一起分发、并在同一批改动里
// 更新成新名 —— 默认开启旧名只会让每个用户的 TSDB 里多一倍序列，而随附面板
// 一个都用不到它们。
//
// 自建面板或告警规则的用户显式传 -metrics.legacy=true 撑过迁移窗口。这是
// CHANGELOG 里列为 breaking change 的那一条。
var legacyMetrics = flag.Bool("metrics.legacy", false,
	"Also emit the pre-rename metric names alongside the normalised ones (default: false). "+
		"Enable this if you have dashboards or alerting rules referencing the old names.")

// emitLegacyNames 在读锁保护下取 -metrics.legacy 的当前值。
//
// 为什么不能裸读 *legacyMetrics：SIGHUP 重载通过 flag.FlagSet.Set 改写这个
// 指针指向的 bool，而 flag 包的 setter 没有任何同步原语（标准库 flag.go 里
// boolValue.Set 的最后一行就是 `*b = boolValue(v)`）。抓取协程同时在读它，
// 构成数据竞争。
//
// 调用点必须在**循环之外**：emitPerformanceMetrics 的读取位置在最内层
// 循环里，每个计数器值一次。放在那里既会让 RLock 的次数上到几十万量级，
// 也会让同一轮抓取的前半段用旧值、后半段用新值 —— 一次落在中间的重载
// 会让同一个 /metrics 响应里一部分指标带旧名、一部分不带。
func emitLegacyNames() bool {
	var v bool

	config.Snapshot(func() {
		v = *legacyMetrics
	})

	return v
}

// 本文件把 vCenter 的性能计数器名翻译成符合 Prometheus 规范的指标名，并给出
// 单位换算系数与指标类型。
//
// 为什么需要它：旧实现直接把计数器名里的 "." 换成 "_" 就当指标名用，于是
// vCenter 那边的命名风格整套穿透了过来 —— 驼峰（bytesRx）、非基础单位
// （kiloBytes、millisecond、percent 的百分之一）、以及 .summation / .average /
// .latest 这类只在 vSphere 语境下有意义的 rollup 后缀。
//
// 翻译规则**从计数器元数据推导**，而不是逐条手写 29 个映射。原因是手写清单
// 与 host.go / vm.go 里的计数器清单是两份独立的事实，加计数器时漏改一处不会
// 有任何报错，只会让新指标继续用旧的命名风格。元数据（StatsType / UnitInfo）
// 由 vCenter 自己提供，不会和实际数据脱钩。
//
// 只有推导不出来的部分才列成例外表（bytesRx → receive_bytes 之类的语义改写）。

// perfUnitRule 描述一种 vSphere 单位到 Prometheus 基础单位的换算。
type perfUnitRule struct {
	// suffix 是新指标名的单位后缀，例如 "bytes"、"seconds"、"ratio"。
	// 空串表示这个单位不需要后缀（number 类计数器）。
	suffix string

	// factor 是乘到原始值上的系数。
	factor float64
}

// perfUnitRules 按 vSphere 的 UnitInfo key 索引换算规则。
//
// key 取自 PerfCounterInfo.UnitInfo.GetElementDescription().Key，这是 vCenter
// 自己声明的单位标识，比从计数器名猜单位可靠得多 —— 例如
// datastore.read.average 的名字里没有任何单位线索，元数据里是 kiloBytesPerSecond。
var perfUnitRules = map[string]perfUnitRule{
	// kiloBytes 是 1024 字节而非 1000，vSphere 文档明确如此。
	"kiloBytes":          {suffix: "bytes", factor: 1024},
	"kiloBytesPerSecond": {suffix: "bytes_per_second", factor: 1024},
	"megaBytes":          {suffix: "bytes", factor: 1024 * 1024},

	"megaHertz": {suffix: "hertz", factor: 1e6},

	"millisecond": {suffix: "seconds", factor: 1e-3},
	"second":      {suffix: "seconds", factor: 1},

	// vSphere 的 percent 计数器以百分之一个百分点为单位：取值 100 表示 1%。
	// 换算到 Prometheus 惯用的 ratio（0..1）是 ÷10000，不是 ÷100。
	// 这一条最容易错，而错了之后的数值仍然「看起来像个合理的百分比」。
	"percent": {suffix: "ratio", factor: 1e-4},

	// number 是无量纲计数，不加单位后缀。
	"number": {suffix: "", factor: 1},

	"joule": {suffix: "joules", factor: 1},
	"watt":  {suffix: "watts", factor: 1},
}

// perfNameOverrides 把计数器名里推导不出来的部分改写成规范说法。
//
// key 是计数器名去掉 rollup 后缀后的部分（例如 "net.bytesRx"），value 是替换
// 后的下划线形式。只列真正需要语义改写的：驼峰、方向词、以及 vSphere 自造的
// 缩写。纯粹的 a.b → a_b 由通用逻辑处理，不必列在这里。
var perfNameOverrides = map[string]string{
	// Rx/Tx 是驼峰，且 Prometheus 生态惯用 receive/transmit。
	"net.bytesRx":   "net_receive",
	"net.bytesTx":   "net_transmit",
	"net.errorsRx":  "net_receive_errors",
	"net.errorsTx":  "net_transmit_errors",
	"net.droppedRx": "net_receive_dropped",
	"net.droppedTx": "net_transmit_dropped",

	// numberReadAveraged 的 "Averaged" 是 vSphere 的说法，它实际是 IOPS。
	"datastore.numberReadAveraged":  "datastore_read_operations",
	"datastore.numberWriteAveraged": "datastore_write_operations",

	// totalReadLatency 里的 "total" 指「包含内核与设备两段」，不是累加量，
	// 直译成 total_read_latency 会被误读成 counter。
	"datastore.totalReadLatency":  "datastore_read_latency",
	"datastore.totalWriteLatency": "datastore_write_latency",

	// usagemhz 把单位编进了名字里，而单位后缀由 perfUnitRules 统一加。
	"cpu.usagemhz": "cpu_usage",

	// vmmemctl 是 balloon 驱动的内部名，对外惯称 balloon。
	"mem.vmmemctl": "mem_balloon",
}

// perfMetricSpec 是一个性能计数器翻译后的完整结果。
type perfMetricSpec struct {
	// Name 是新的指标名（不含 namespace / subsystem 前缀）。
	Name string

	// Factor 是值的换算系数。
	Factor float64

	// ValueType 是 Prometheus 指标类型。
	ValueType prometheus.ValueType

	// Delta 表示这个计数器的每个样本是「该采样区间内的增量」。
	//
	// 这个字段决定多样本时的聚合方式，是本次修正的核心：delta 计数器必须
	// 求和（把各区间的增量加起来），而旧实现对所有计数器一律求平均。
	Delta bool
}

// translatePerfCounter 把一个 vCenter 性能计数器翻译成 Prometheus 指标规格。
//
// 第二个返回值为 false 表示这个计数器的单位不在已知规则里 —— 调用方应当按
// 旧命名导出并留下日志，而不是猜一个后缀。猜错单位比不改名有害得多：数值
// 会带着一个错误的单位后缀进入 TSDB，而没有任何东西会报错。
func translatePerfCounter(counter string, info *types.PerfCounterInfo) (perfMetricSpec, bool) {
	if info == nil {
		return perfMetricSpec{}, false
	}

	unitKey := info.UnitInfo.GetElementDescription().Key

	rule, ok := perfUnitRules[unitKey]
	if !ok {
		return perfMetricSpec{}, false
	}

	// vSphere 计数器名的形式是 group.name.rollup，例如 cpu.ready.summation。
	// rollup 后缀（summation / average / latest / maximum / minimum）描述的是
	// vCenter 内部的聚合方式，不属于指标语义，一律去掉。
	base := counter
	if i := strings.LastIndex(counter, "."); i > 0 {
		base = counter[:i]
	}

	name, ok := perfNameOverrides[base]
	if !ok {
		name = strings.ReplaceAll(base, ".", "_")
	}

	// StatsType 是 vCenter 声明的取值语义，不是从名字猜的：
	//   delta    —— 每个样本是区间内的增量
	//   absolute —— 每个样本是瞬时绝对值
	//   rate     —— 每个样本是每秒速率
	//
	// 只有 delta 会被导出成 counter。absolute 与 rate 都是瞬时量，导成
	// counter 会让 rate() 对一条上下跳动的序列算出垃圾。
	delta := info.StatsType == types.PerfStatsTypeDelta

	spec := perfMetricSpec{
		Name:      name,
		Factor:    rule.factor,
		ValueType: prometheus.GaugeValue,
		Delta:     delta,
	}

	if rule.suffix != "" {
		spec.Name = spec.Name + "_" + rule.suffix
	}

	if delta {
		// delta 计数器求和之后是「这个采样窗口内累计了多少」，语义上是
		// counter 的增量，因此加 _total 并用 CounterValue。
		spec.Name = spec.Name + "_total"
		spec.ValueType = prometheus.CounterValue
	}

	return spec, true
}
