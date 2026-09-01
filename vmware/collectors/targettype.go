package vmwareCollectors

import (
	"context"
	"log/slog"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/vim25/types"
)

// 目标类型常量。定义在 internal/collector 里，此处以常量别名转发，
// 让本包内既有的 targetTypeVCenter / targetTypeESXi 引用不必逐个改写。
const (
	targetTypeVCenter = collector.TargetTypeVCenter
	targetTypeESXi    = collector.TargetTypeESXi
)

// ESXi 上的隐式伪对象 moid。ESXi 没有真实的 Datacenter / ComputeResource，
// 但仍以固定 moid 暴露一套占位对象，让 API 的形状与 vCenter 保持一致。
//
// 依据：govmomi simulator/esx/datacenter.go:18,37-40 与
// simulator/esx/root_folder.go:19。
const (
	syntheticDatacenterMoid = "ha-datacenter"
	syntheticComputeMoid    = "ha-compute-res"
)

// historicIntervalID 是 vCenter 上最短的历史汇总间隔（5 分钟）。
// 只有 vCenter 聚合历史统计，ESXi 不做汇总。
const historicIntervalID int32 = 300

// realtimeFallbackInterval 是协商不出可用间隔时的兜底值。vSphere 的实时
// 采样周期长期固定为 20s，但这是经验值而非契约，因此只在兜底时使用。
const realtimeFallbackInterval int32 = 20

// targetType 返回本次抓取的目标类型。
//
// 改动前它从 map[string]interface{} 里取 loginData["targetType"]，并且必须
// 用带 ok 的断言兜底 —— 键可能不存在、类型可能不对，两种失败都只在运行期
// 暴露。现在 TargetType 是 Scrape 的字段，缺失即空字符串，仍然回退到
// vCenter，但「类型不对」这种可能性从此不存在。
func targetType(s *collector.Scrape) string {
	if s != nil && s.TargetType != "" {
		return s.TargetType
	}
	return targetTypeVCenter
}

// isESXi 是 targetType 的便捷形式。
func isESXi(s *collector.Scrape) bool {
	return targetType(s) == targetTypeESXi
}

// syntheticLabels 在 ESXi 模式下给伪对象指标追加 synthetic="true"。
//
// 设计取舍：ESXi 上不存在真实的 Datacenter / Cluster，但仍然输出对应指标，
// 是为了让 dashboard 里依赖 dcmo / cmo 关联的查询不断链。而 synthetic label
// 诚实标注了「这条序列来自伪对象」，用户想只统计真实数据中心时可以用
// vmware_datacenter_info{synthetic!="true"} 过滤掉。
//
// 相比静默填充伪值，多一个 label 换来的是指标语义不含糊。
func syntheticLabels(base map[string]string) map[string]string {
	base["synthetic"] = "true"
	return base
}

// resolvePerfIntervalForTarget 在 resolvePerfInterval 之上追加一层
// ESXi 专属的兜底，是 C-2 的完整修复。
//
// 为什么需要这一层：ProviderSummary 的 SummarySupported 语义是「该实体有
// 历史统计视图」，而 ESXi 上这个字段可能为真（API 形状与 vCenter 一致），
// 但 ESXi 本身并不运行统计汇总服务 —— 汇总是 vCenter 的职责。于是按
// SummarySupported 协商出的 300 秒间隔在 ESXi 上会返回空结果集。
//
// 这个偏差在 govmomi 的 simulator 里能直接看到：ESX 模型对 Datastore 返回
// historicProviderSummary（CurrentSupported=false、SummarySupported=true、
// RefreshRate=-1，见 simulator/performance_manager.go:27-31），与 VPX 模型
// 用的是同一份数据。也就是说，光看 ProviderSummary 无法区分「真的有历史
// 数据」和「只是 API 形状一致」，必须结合目标类型判断。
//
// 因此 ESXi 上一律退到实时间隔：宁可拿到粒度更细的实时数据，也不要一个
// 静默返回空值的请求。
func resolvePerfIntervalForTarget(
	ctx context.Context,
	perf *performance.Manager,
	entity types.ManagedObjectReference,
	requested, fallback int32,
	tType string,
	logger *slog.Logger,
) int32 {
	interval := resolvePerfInterval(ctx, perf, entity, requested, fallback, logger)

	if tType == targetTypeESXi && interval == historicIntervalID {
		// requested 是用户表达的期望采样窗口，在 ESXi 上它就是实时间隔的
		// 期望值；若与服务端 RefreshRate 不符，host/vm 的协商已经发过 warn。
		effective := requested
		if effective <= 0 {
			effective = realtimeFallbackInterval
		}

		logger.Debug("ESXi does not aggregate historical statistics, using the realtime interval instead",
			"entity_type", entity.Type,
			"rejected_interval", historicIntervalID,
			"interval", effective)

		return effective
	}

	return interval
}

// resolvePerfInterval 与服务端协商某类实体的采样间隔，而不是由客户端猜。
//
// 修复的问题：datastore.go 原先硬编码 IntervalId=300，代码注释自称
// "A dirty workaround"。这个值在 vCenter 上碰巧可用，在 ESXi 上必然查不到
// 数据 —— ESXi 不聚合历史统计，请求 300 秒间隔会得到空结果集，表现为指标
// 静默消失，且没有任何报错，是最难排查的一类故障。
//
// 做法：对一个真实存在的实体调用 QueryPerfProviderSummary，取服务端权威值。
// 必须传真实实体引用，伪造的 moid 会被服务端以 InvalidArgument 拒绝
// （见 simulator/performance_manager.go:85-90 对真实行为的复刻）。
//
// requested 是用户通过 -vmware.interval 表达的期望值。只在服务端支持实时
// 且该值与 RefreshRate 一致时才生效；不一致时以服务端为准并记 warn ——
// 间隔不是客户端可自选的，传一个服务端没有的值只会得到空数据。
//
// fallback 是协商失败时的取值，由调用方按实体类型给出。协商失败不视为
// 致命错误：采集能力可能受限，但不该让整个 collector 中断。
func resolvePerfInterval(
	ctx context.Context,
	perf *performance.Manager,
	entity types.ManagedObjectReference,
	requested, fallback int32,
	logger *slog.Logger,
) int32 {
	if perf == nil {
		logger.Warn("nil performance manager, using fallback interval",
			"entity_type", entity.Type, "fallback", fallback)
		return fallback
	}

	summary, err := perf.ProviderSummary(ctx, entity)
	if err != nil {
		logger.Debug("could not query perf provider summary, using fallback interval",
			"entity_type", entity.Type, "fallback", fallback, "error", err)
		return fallback
	}

	if summary.CurrentSupported {
		// RefreshRate 只在支持实时统计时有意义。不支持时服务端返回 -1
		// （simulator/performance_manager.go:27-31），把 -1 当作 IntervalId
		// 会构造出无效请求，因此这里显式检查 > 0。
		if summary.RefreshRate > 0 {
			if requested != summary.RefreshRate {
				logger.Warn("requested sampling interval differs from the server refresh rate, using the server value",
					"entity_type", entity.Type,
					"requested", requested,
					"server_refresh_rate", summary.RefreshRate)
			}
			return summary.RefreshRate
		}

		logger.Warn("server reports realtime support but no usable refresh rate, falling back",
			"entity_type", entity.Type,
			"refresh_rate", summary.RefreshRate,
			"fallback", realtimeFallbackInterval)
		return realtimeFallbackInterval
	}

	if summary.SummarySupported {
		logger.Debug("entity only supports historical statistics",
			"entity_type", entity.Type, "interval", historicIntervalID)
		return historicIntervalID
	}

	logger.Warn("entity supports neither realtime nor historical statistics, using fallback",
		"entity_type", entity.Type, "fallback", fallback)
	return fallback
}
