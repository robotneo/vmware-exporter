package vmwareCollectors

import (
	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/vmware/govmomi/vim25/mo"
)

// hostDataPlaneEligible 报告一台主机是否可以承载「实时数据面」采集：
// perf 实时计数器与 esxcli 调用都要求主机开机、已连接且不在维护模式。
//
// 同一个判定被三处使用：host collector（perf）、esxcli.host.nic、
// esxcli.storage。收敛成一个函数，避免三处条件漂移 —— 漏掉维护模式
// 这种偏差不会编译报错，只会让 esxcli 在维护窗口里刷错误日志。
//
// 注意它只管数据面：info/容量/生命周期状态指标对全部主机无条件输出，
// 不经过这个判定（见 host.go 与 DESIGN-p1-p3-roadmap 2.1）。
func hostDataPlaneEligible(host mo.HostSystem) bool {
	return host.Runtime.PowerState == "poweredOn" &&
		host.Runtime.ConnectionState == "connected" &&
		!host.Runtime.InMaintenanceMode
}

// hostSkipReasons 汇总一台主机不满足数据面条件的全部原因。
//
// 返回预填的原因 map（未命中的原因也带 0），调用方累加进 RecordEntities，
// 于是正常状态也有稳定的 0 值序列，「有主机被跳过」告警不必用 absent()/or
// 兜底。一台主机可能同时命中多个原因（维护中断连），各原因独立计数，
// 因此跨原因求和可能大于实际跳过的主机数 —— 这是刻意的。
func hostSkipReasons(host mo.HostSystem) map[string]int {
	reasons := map[string]int{
		collector.SkipReasonPoweredOff:    0,
		collector.SkipReasonDisconnected:  0,
		collector.SkipReasonNotResponding: 0,
		collector.SkipReasonMaintenance:   0,
	}

	if host.Runtime.PowerState != "poweredOn" {
		reasons[collector.SkipReasonPoweredOff]++
	}
	switch host.Runtime.ConnectionState {
	case "disconnected":
		reasons[collector.SkipReasonDisconnected]++
	case "notResponding":
		reasons[collector.SkipReasonNotResponding]++
	}
	if host.Runtime.InMaintenanceMode {
		reasons[collector.SkipReasonMaintenance]++
	}

	return reasons
}

// addSkipReasons 把 src 中每个原因的计数累加进 dst。
//
// 多个 collector（host + 两个 esxcli）各自遍历同一批主机，它们的计数在
// EntityStats 里按 collector 名分开，但单个 collector 内部多台主机的原因
// 需要逐台累加。dst 必须是预填过的 map。
func addSkipReasons(dst, src map[string]int) {
	for reason, n := range src {
		dst[reason] += n
	}
}
