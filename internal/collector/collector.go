package collector

import (
	"context"
	"log/slog"
	"sort"

	"github.com/prezhdarov/vmware-exporter/internal/config"

	"github.com/prometheus/client_golang/prometheus"
)

// 默认开关状态的常量。名字沿用框架的 DefaultEnabled / DefaultDisabled，
// 这样 registry.go 里的清单不需要改写 —— 那份清单是全项目 collector 的
// 唯一来源，改动它的风险远大于保留两个常量名。
const (
	DefaultEnabled  = true
	DefaultDisabled = false
)

// Collector 是采集器需要实现的接口。
//
// 与框架版本的唯一实质差别是 ctx 进了签名。看起来只是加一个参数，实际是
// 这次重构的全部目的：
//
//	框架： Update(ch, namespace, clientAPI, loginData map[string]any, params map[string]string) error
//	现在： Update(ctx, ch, s *Scrape) error
//
// namespace 移入 Scrape 之外的另一处考虑：它在整个进程生命周期里是常量
// （"vmware"），每次调用都传一遍纯属噪音。但它确实参与 Desc 构造，所以
// 保留在 Scrape 里而不是做成包级变量 —— 包级可变状态在测试里会互相干扰。
//
// clientAPI 与 params 两个参数被删掉了：全仓库没有任何 collector 用过它们。
// clientAPI 在 7 个 Update 实现里全部是未使用的形参，params 同样。
type Collector interface {
	Update(ctx context.Context, ch chan<- prometheus.Metric, s *Scrape) error
}

// Definition 描述一个 collector 的身份、构造方式与默认开关状态。
type Definition struct {
	// Name 是 collector 的规范名，同时用于：
	//   - 命令行开关 -collector.<Name>
	//   - /probe 的 collect[]=<Name> / nocollect[]=<Name>
	//   - vmware_scrape_collector_duration_seconds{collector="<Name>"} 的标签值
	Name string

	// Creator 构造 collector 实例。
	Creator func(*slog.Logger) (Collector, error)

	// DefaultEnabled 决定未显式指定时是否启用。
	DefaultEnabled bool
}

// registered 是命令行开关的注册表。
//
// 与框架的 collectorState 的关键差别：这个 map 可以被 Registered() 反查。
// 框架把它设为包级私有，导致 /probe 端点无法得知「哪些 collector 启用了」，
// 只能手工复刻一份调度逻辑（改动前 vmware-exporter.go:169-219 就是那份复刻）。
// 能反查之后 /metrics 与 /probe 就能共用同一个 CollectorSet，不会再分叉。
var registered = make(map[string]*bool)

// RegisterFlag 记录某个 collector 的命令行开关指针。
//
// 由各 collector 的 init() 调用。指针而非值：flag 包在 Parse() 时才写入
// 实际值，注册发生在 Parse() 之前。
func RegisterFlag(name string, enabled *bool) {
	registered[name] = enabled
}

// Registered 返回所有已注册开关的当前状态。
//
// 走 config.Snapshot 而不是裸读：这些 *bool 是 flag 指针，SIGHUP 重载会
// 通过 flag.Set 改写它们，而 flag 包的 setter 没有任何同步原语。每轮抓取
// 都会调这个函数，于是它是重载路径最主要的读侧对手。
//
// 整个循环放进同一个 Snapshot，是为了让返回的 map 是某一时刻配置的**一致
// 切片**。逐个加锁的话，一次恰好落在中间的重载会让同一轮抓取里
// esxcli.host.nic 用新配置、esxcli.storage 用旧配置。
func Registered() map[string]bool {
	out := make(map[string]bool, len(registered))

	config.Snapshot(func() {
		for name, enabled := range registered {
			out[name] = *enabled
		}
	})

	return out
}

// RegisteredNames 返回所有已注册开关的名字，已排序。
func RegisteredNames() []string {
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
