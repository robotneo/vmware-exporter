// Package collector 是本仓库自己的采集调度层。
//
// 它取代了此前的 github.com/prezhdarov/prometheus-exporter/pkg/collector。
// 换掉框架不是为了减少一个依赖，而是因为它的四个缺陷都无法从外部绕过：
//
//  1. Collector 接口的 Update() 签名里没有 context.Context
//     （框架 pkg/collector/collector.go:24）。context 只能藏在
//     map[string]any 里偷偷传递，客户端断连时无法取消上游的 vCenter 调用 ——
//     Prometheus 抓取超时后，SOAP 请求仍在服务端跑到自然结束。
//
//  2. 登录失败时 Collect() 直接 return
//     （框架 pkg/collector/collect.go:15-19），整轮抓取产出零个指标。
//     Prometheus 看到的是一份空响应，而不是「这个 target 挂了」——
//     没有 up=0 就没法告警，只能靠 absent() 之类的间接手段。
//
//  3. collectorState 是包级私有变量（框架 collector.go:47），外部无法反查
//     「哪些 collector 被启用了」。/probe 端点因此不得不手工复刻整套并发
//     调度逻辑，两份实现随时会分叉。
//
//  4. -prom.maxRequests 被接收但从未生效：框架把它存进 eHandler.maxRequests
//     就再没用过（框架 pkg/exporter/exporter.go:20 与 handler.go:16）。
//
// 保留的部分：指标名、标签、以及自监控指标的语义与框架完全一致，
// 因此既有的 dashboard 与告警规则不需要任何改动。
package collector

import (
	"context"
	"sync"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Scrape 承载一轮抓取所需的全部 vCenter 会话状态。
//
// 它取代的是 map[string]interface{} 形态的 loginData：那份 map 有 13 个 key，
// 靠字符串约定跨三个包传递，每次取值都要写 loginData["ctx"].(context.Context)
// 这样的裸断言。改一个 key 名不会有任何编译错误，只在运行期断言失败；
// 少填一个 key 同样如此。换成结构体之后这些都变成编译期契约。
//
// 注意 ctx 刻意**不是**字段。context 应当沿调用链显式传递，塞进结构体会让
// 「这次调用用的是哪个 context」变得不可见 —— 而 context 的生命周期恰恰是
// 这次重构要修的问题。所以它作为 Update() 的首参出现。
type Scrape struct {
	// Client 是已登录的 vim25 客户端。
	Client *vim25.Client

	// View 用于创建 ContainerView 做属性检索。
	View *view.Manager

	// Perf 是性能计数器管理器，Counters 是按名索引的计数器元数据。
	Perf     *performance.Manager
	Counters map[string]*types.PerfCounterInfo

	// Target 是被抓取的 vCenter 或 ESXi 地址，同时作为 vcenter label 的值。
	Target string

	// TargetType 区分 vCenter 与 ESXi。两者的性能采样间隔语义不同：
	// ESXi 不聚合历史统计，请求 300s 间隔会静默返回空结果集。
	TargetType string

	// Interval 是期望的采样窗口（秒），Samples 是单次请求取的样本数。
	Interval int32
	Samples  int32

	// Namespace 是所有指标名的前缀（"vmware"）。
	//
	// 它在整个进程生命周期里是常量，放在这里而不是继续做 Update 的参数，
	// 是因为 5 个参数里有 3 个 string 时传错顺序不会编译报错。做成包级
	// 变量则会让测试互相干扰。
	Namespace string

	// MaxConcurrency 是整轮抓取的并发预算，由 -collector.max-concurrency 决定。
	// collector 内部的 per-host fan-out 通过 HostConcurrency() 读取它。
	MaxConcurrency int

	// hostsOnce / hosts / hostsErr 实现 HostSystem 的**请求内**共享。
	//
	// 改动前 host、esxcli.host.nic、esxcli.storage 三个 collector 各自
	// 调用 fetchProperties 检索一遍 HostSystem —— 三个 collector 全开时
	// 同一份主机清单被拉取三次。
	//
	// 这是请求内共享，不是跨请求缓存：Scrape 的生命周期就是一次抓取，
	// 下一次抓取会构造新的 Scrape。因此不存在数据陈旧问题，也不需要
	// 考虑 TTL 与失效策略 —— 那些才是缓存要面对的复杂度。
	//
	// 属性集取三个 collector 需求的并集（见 Hosts 的说明）。
	hostsOnce sync.Once
	hosts     []mo.HostSystem
	hostsErr  error
}

// IsESXi 报告本次抓取的目标是否为 ESXi 主机而非 vCenter。
func (s *Scrape) IsESXi() bool {
	return s.TargetType == TargetTypeESXi
}

// HostConcurrency 返回 collector 内部按主机 fan-out 时应使用的并发上限。
//
// 未配置时返回 defaultHostConcurrency 而不是「不限制」。这是与 collector
// 层的 SetLimit 的一处刻意差别：collector 的数量是固定的 7 个，不限制最多
// 也就 7 个 goroutine；而 per-host fan-out 的规模由 vCenter 里的主机数决定，
// 「不限制」在 500 主机的环境下就是 500 个并发 SOAP 请求。默认值必须是一个
// 有限的数。
func (s *Scrape) HostConcurrency() int {
	if s == nil || s.MaxConcurrency <= 0 {
		return defaultHostConcurrency
	}

	return s.MaxConcurrency
}

// defaultHostConcurrency 是 MaxConcurrency 未设置时 per-host fan-out 的上限。
const defaultHostConcurrency = 8

// 目标类型常量。值必须与 vmware/api 包的定义一致 —— 它们会作为
// vmware_target_info{type="..."} 的 label 值输出，是 dashboard 条件渲染的
// 唯一依据。此处重复声明是为了避免 internal/collector 反向依赖 vmware/api。
const (
	TargetTypeVCenter = "vcenter"
	TargetTypeESXi    = "esxi"
)

// hostProperties 是三个主机相关 collector 所需属性的并集。
//
// 取并集而非各取所需，是共享这份检索结果的前提。代价是 host collector 会
// 多拿 config 与 hardware 两个属性 —— 而收益是三次 ContainerView 检索变成
// 一次。ContainerView 的创建与销毁本身就是两次 SOAP 往返，属性多少对单次
// 检索的成本影响远小于往返次数。
//
// 各 collector 原本的需求：
//   - host.go:66              parent, summary, runtime
//   - esxclihostnic.go:64     runtime, name, config, hardware
//   - esxclistoragelist.go:56 runtime, name
var hostProperties = []string{"parent", "summary", "runtime", "name", "config", "hardware"}

// HostFetcher 由外部注入实际的属性检索实现。
//
// 这个间接层的存在是为了避免包依赖成环：属性检索的实现在
// vmware/collectors 包里（fetchProperties），而那个包要依赖本包的
// Collector 接口。让本包反过来 import 它就会形成环。
type HostFetcher func(ctx context.Context, s *Scrape, props []string, out *[]mo.HostSystem) error

// Hosts 返回本轮抓取的 HostSystem 列表，同一个 Scrape 上只检索一次。
//
// 错误也一并记住并重复返回：若第一个 collector 因为超时拿不到主机清单，
// 后面两个再试一次也只会同样超时，白白多花两次 SOAP 往返 —— 而此时整轮
// 抓取的时间预算大概已经耗尽了。
func (s *Scrape) Hosts(ctx context.Context, fetch HostFetcher) ([]mo.HostSystem, error) {
	s.hostsOnce.Do(func() {
		s.hostsErr = fetch(ctx, s, hostProperties, &s.hosts)
	})

	return s.hosts, s.hostsErr
}
