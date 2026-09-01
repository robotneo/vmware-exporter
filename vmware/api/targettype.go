package vmware

import (
	"log/slog"

	"github.com/vmware/govmomi/vim25"
)

// 目标类型常量。判定结果写入 loginData["targetType"]，供全部下游 collector 读取。
const (
	// TargetTypeVCenter 表示目标是 vCenter Server，对象树完整。
	TargetTypeVCenter = "vcenter"
	// TargetTypeESXi 表示目标是直连的单台 ESXi 主机：无真实
	// Datacenter / Cluster，且不聚合历史性能统计。
	TargetTypeESXi = "esxi"
)

// ApiType 的两个取值，由服务端在 ServiceContent.About 中返回。
const (
	apiTypeVirtualCenter = "VirtualCenter"
	apiTypeHostAgent     = "HostAgent"
)

// detectTargetType 依据 ServiceContent.About.ApiType 判定目标类型。
//
// 选这个字段而不是探测对象树，是因为它随 ServiceContent 在登录时一并返回，
// 不需要额外的 API 往返；而对象树探测（例如「查不到 Datacenter 就认为是
// ESXi」）在权限受限的账号下会误判 —— 看不到对象和对象不存在是两件事。
//
// 未知取值按 vCenter 处理并记 warn：这是既有行为，保守选择不会让现有部署变差。
func detectTargetType(client *vim25.Client, logger *slog.Logger) string {
	apiType := client.ServiceContent.About.ApiType

	switch apiType {
	case apiTypeHostAgent:
		return TargetTypeESXi
	case apiTypeVirtualCenter:
		return TargetTypeVCenter
	default:
		logger.Warn("unrecognised ServiceContent.About.ApiType, assuming vCenter",
			"api_type", apiType)
		return TargetTypeVCenter
	}
}
