package vmwareCollectors

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// vsanPerfStub 是 vSAN 性能通道的替身。
//
// 与 vsan_test.go 的 vsanStub 分开而不是扩展它：那个替身的 switch 覆盖组 A
// 的 3 个方法，本 collector 用的是完全不同的 2 个。混在一起会让两组测试
// 共享一个越来越大的 switch，而它们之间没有任何共用分支。
//
// 必须有替身的理由和组 A 相同且更强：vcsim 的 vsan/simulator.go 一个性能
// 方法都没注册，直接拿 vcsim 跑本 collector 只会在第一个 SOAP 调用上失败。
type vsanPerfStub struct {
	perf     *vsantypes.VsanPerfQueryPerfResponse
	entities *vsantypes.VsanPerfGetSupportedEntityTypesResponse

	perfErr     error
	entitiesErr error

	perfCalls     int
	entitiesCalls int

	// querySpecs 记录最后一次性能查询的 spec，用于断言请求侧裁剪
	// （Labels 真的传了白名单）与实体类型协商的结果。
	querySpecs []vsantypes.VsanPerfQuerySpec
}

func (s *vsanPerfStub) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	switch body := res.(type) {
	case *vsanmethods.VsanPerfGetSupportedEntityTypesBody:
		s.entitiesCalls++
		if s.entitiesErr != nil {
			return s.entitiesErr
		}
		body.Res = s.entities

	case *vsanmethods.VsanPerfQueryPerfBody:
		s.perfCalls++

		reqBody, ok := req.(*vsanmethods.VsanPerfQueryPerfBody)
		if !ok || reqBody.Req == nil {
			return errors.New("stub: malformed performance query request")
		}
		s.querySpecs = reqBody.Req.QuerySpecs

		if s.perfErr != nil {
			return s.perfErr
		}
		body.Res = s.perf

	default:
		return errors.New("stub: unexpected request type")
	}

	return nil
}

// collectVsanPerfCluster 用替身跑一个集群的性能采集。
func collectVsanPerfCluster(t *testing.T, stub *vsanPerfStub) (
	map[string][]map[string]string, map[string][]float64, error,
) {
	t.Helper()

	c := &vsanPerfCollector{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &collector.Scrape{Namespace: "vmware", Target: "vc.example.com"}

	// 缓冲够大即可，理由同 collectVsanCluster：不能 close(ch)。
	ch := make(chan prometheus.Metric, 500)

	err := c.collectCluster(context.Background(), ch, s, stub,
		testVsanCluster("domain-c7", "vsan-cluster"))

	series := map[string][]map[string]string{}
	values := map[string][]float64{}

	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		series[name] = append(series[name], labels)
		values[name] = append(values[name], value)
	}

	return series, values, err
}

// vsanPerfEntityTypes 造一份"环境支持这些实体类型"的响应。
func vsanPerfEntityTypes(names ...string) *vsantypes.VsanPerfGetSupportedEntityTypesResponse {
	resp := &vsantypes.VsanPerfGetSupportedEntityTypesResponse{}
	for _, n := range names {
		resp.Returnval = append(resp.Returnval, vsantypes.VsanPerfEntityType{Name: n})
	}

	return resp
}

// vsanPerfSeries 造一条指标序列。values 是逗号分隔的 CSV 串。
func vsanPerfSeries(label, values string) vsantypes.VsanPerfMetricSeriesCSV {
	return vsantypes.VsanPerfMetricSeriesCSV{
		MetricId: vsantypes.VsanPerfMetricId{Label: label},
		Values:   values,
	}
}

// vsanPerfResponse 造一份性能查询响应。
func vsanPerfResponse(entities ...vsantypes.VsanPerfEntityMetricCSV) *vsantypes.VsanPerfQueryPerfResponse {
	return &vsantypes.VsanPerfQueryPerfResponse{Returnval: entities}
}

func vsanPerfEntity(refID, sampleInfo string, series ...vsantypes.VsanPerfMetricSeriesCSV) vsantypes.VsanPerfEntityMetricCSV {
	return vsantypes.VsanPerfEntityMetricCSV{
		EntityRefId: refID,
		SampleInfo:  sampleInfo,
		Value:       series,
	}
}

// withVsanPerfSkipVerify 临时改 flag 并在测试结束时还原。
//
// flag 是包级变量，测试之间会互相污染 —— t.Cleanup 还原是必须的，
// 不是礼貌。Go 的测试在同一个包内串行跑（除非 t.Parallel），所以
// 这个做法安全。
func withVsanPerfSkipVerify(t *testing.T, v bool) {
	t.Helper()

	old := *vsanPerfSkipVerify
	*vsanPerfSkipVerify = v
	t.Cleanup(func() { *vsanPerfSkipVerify = old })
}

// TestVsanPerfEntityTypeIntersection 锁死 D4 的 C 方案：实际查询的实体类型
// 是白名单与环境支持清单的**交集**，且顺序跟白名单而不是 API 返回顺序。
//
// 为什么要断言顺序：顺序不稳定的话，日志与后续测试的断言都无法复现 ——
// 而 map 迭代在 Go 里是刻意随机的，很容易不小心把顺序交给它。
func TestVsanPerfEntityTypeIntersection(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	// 环境支持清单刻意乱序，且含一个白名单外的类型（vsan-iscsi-target）
	// 与缺一个白名单内的类型（cache-disk 不在）。
	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes(
			"disk-group", "vsan-iscsi-target", "host-domclient", "cluster-domclient",
		),
		perf: vsanPerfResponse(),
	}

	if _, _, err := collectVsanPerfCluster(t, stub); err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if stub.entitiesCalls != 1 {
		t.Fatalf("expected exactly 1 supported entity type query, got %d", stub.entitiesCalls)
	}

	var got []string
	for _, spec := range stub.querySpecs {
		got = append(got, spec.EntityRefId)
	}

	// 白名单顺序是 cluster-domclient, host-domclient, disk-group,
	// capacity-disk, cache-disk。环境支持前三个，所以交集就是前三个，
	// 且必须按白名单顺序而不是 API 的乱序。
	want := []string{"cluster-domclient:*", "host-domclient:*", "disk-group:*"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query specs = %v, want %v", got, want)
	}
}

// TestVsanPerfQueryRequestsWhitelistedLabels 锁死请求侧裁剪。
//
// 这条断言的是基数控制的实现位置：白名单必须作为 QuerySpec.Labels 传给
// vCenter，多余的指标根本不下载。只在响应侧过滤也能得到同样的指标输出，
// 但那意味着 disk-group 的 79 个 label 全部经过网络与解析 —— 一个测试
// 看不出差别，真实环境的带宽与 CPU 看得出。
func TestVsanPerfQueryRequestsWhitelistedLabels(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf:     vsanPerfResponse(),
	}

	if _, _, err := collectVsanPerfCluster(t, stub); err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if len(stub.querySpecs) != 1 {
		t.Fatalf("expected 1 query spec, got %d", len(stub.querySpecs))
	}

	spec := stub.querySpecs[0]
	if !reflect.DeepEqual(spec.Labels, vsanPerfLabelWhitelist) {
		t.Errorf("spec.Labels = %v, want the label whitelist %v",
			spec.Labels, vsanPerfLabelWhitelist)
	}

	// StartTime 与 EndTime 都不是 omitempty（vsan/types/types.go:7574-7575），
	// nil 会被编码成空元素而服务端行为未定义。两个都必须给值。
	if spec.StartTime == nil || spec.EndTime == nil {
		t.Fatalf("spec.StartTime = %v, spec.EndTime = %v, both must be set",
			spec.StartTime, spec.EndTime)
	}

	if gap := spec.EndTime.Sub(*spec.StartTime).Seconds(); int(gap) != *vsanPerfInterval {
		t.Errorf("query window = %.0fs, want %ds (-vmware.vsan.interval)",
			gap, *vsanPerfInterval)
	}
}

// TestVsanPerfAveragesWindowSamples 锁死聚合语义：窗口内多个采样点求平均。
//
// 同时锁死 label 集合与传值顺序 —— cmo/vmwcluster/entity/entityid/vcenter。
// variableLabels 的顺序错配是静默故障（值配到错误的 label 上，不报错），
// 所以必须有测试盯着。
func TestVsanPerfAveragesWindowSamples(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf: vsanPerfResponse(vsanPerfEntity(
			"cluster-domclient:52a1b2c3-uuid",
			"2026-09-02 01:00:00,2026-09-02 01:05:00,2026-09-02 01:10:00",
			// 平均 = (100 + 200 + 300) / 3 = 200
			vsanPerfSeries("iops_read", "100,200,300"),
			// 平均 = (1.5 + 2.5) / 2 ... 但只有 3 个采样点时长度必须一致，
			// 所以给 3 个值：(1.0 + 2.0 + 3.0) / 3 = 2.0
			vsanPerfSeries("latency_avg_read", "1.0,2.0,3.0"),
		)),
	}

	series, values, err := collectVsanPerfCluster(t, stub)
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if got := values["vmware_vsan_perf_iops_read"]; len(got) != 1 || got[0] != 200 {
		t.Errorf("iops_read = %v, want [200]", got)
	}

	if got := values["vmware_vsan_perf_latency_avg_read"]; len(got) != 1 || got[0] != 2 {
		t.Errorf("latency_avg_read = %v, want [2]", got)
	}

	got := series["vmware_vsan_perf_iops_read"]
	if len(got) != 1 {
		t.Fatalf("expected 1 iops_read series, got %d", len(got))
	}

	want := map[string]string{
		"cmo":        "domain-c7",
		"vmwcluster": "vsan-cluster",
		"entity":     "cluster-domclient",
		"entityid":   "52a1b2c3-uuid",
		"vcenter":    "vc.example.com",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("iops_read labels = %v, want %v", got[0], want)
	}
}

// TestVsanPerfMismatchedCSVLengthDoesNotPanic 是反向验证的重点一条。
//
// telegraf 在这里按 values 的下标索引 timeStamps（vsan.go 的 timeStamps[i]），
// values 比 sampleInfo 长时直接 panic —— 而 exporter 里一次 panic 会带走
// 整个 scrape。本测试断言：长度不一致时跳过该序列并返回错误，不 panic，
// 且**同一实体的其他正常序列照常输出**。
//
// 反向验证方式：把 emitEntity 的长度校验删掉、改成按 timestamps 下标循环，
// 这个测试会 panic 而不是 fail —— panic 也算红，但更重要的是它证明了
// 校验不是装饰。
func TestVsanPerfMismatchedCSVLengthDoesNotPanic(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf: vsanPerfResponse(vsanPerfEntity(
			"cluster-domclient:uuid-1",
			// 2 个采样点。
			"2026-09-02 01:00:00,2026-09-02 01:05:00",
			// 4 个值 —— 比采样点多，正是 telegraf 会 panic 的形态。
			vsanPerfSeries("iops_read", "1,2,3,4"),
			// 长度正确的一条，必须照常输出。
			vsanPerfSeries("iops_write", "10,20"),
		)),
	}

	series, values, err := collectVsanPerfCluster(t, stub)

	// 返回错误是预期的：长度不一致是数据问题，该让用户在日志里看到。
	if err == nil {
		t.Fatal("expected an error for the mismatched series, got nil")
	}

	if _, ok := series["vmware_vsan_perf_iops_read"]; ok {
		t.Error("the mismatched series must not be emitted")
	}

	if got := values["vmware_vsan_perf_iops_write"]; len(got) != 1 || got[0] != 15 {
		t.Errorf("iops_write = %v, want [15]; a bad sibling series must not "+
			"suppress the good ones", got)
	}
}

// TestVsanPerfDropsLabelsOutsideWhitelist 锁死响应侧过滤。
//
// 请求侧已经传了 Labels，但 API 不保证严格遵守。这条测试喂一个白名单外的
// label，断言它不会变成指标 —— 否则基数上限就只是"希望 vCenter 配合"。
func TestVsanPerfDropsLabelsOutsideWhitelist(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("disk-group"),
		perf: vsanPerfResponse(vsanPerfEntity(
			"disk-group:uuid-dg",
			"2026-09-02 01:00:00",
			vsanPerfSeries("iops_read", "50"),
			// resync 分类计数是白名单刻意排除的那一大类（telegraf 里共 24 个）。
			vsanPerfSeries("iops_resync_read_policy", "999"),
			// 空 label 也要挡住：它会生成 vmware_vsan_perf 这种没有后缀的名字。
			vsanPerfSeries("", "1"),
		)),
	}

	series, values, err := collectVsanPerfCluster(t, stub)
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if got := values["vmware_vsan_perf_iops_read"]; len(got) != 1 || got[0] != 50 {
		t.Errorf("iops_read = %v, want [50]", got)
	}

	if _, ok := series["vmware_vsan_perf_iops_resync_read_policy"]; ok {
		t.Error("a label outside the whitelist must not be emitted")
	}

	if _, ok := series["vmware_vsan_perf"]; ok {
		t.Error("an empty label must not produce a suffix-less metric name")
	}
}

// TestVsanPerfSkipsUnparsableSamples 锁死单点容错。
//
// 一个畸形采样点只跳过它自己，窗口里其余点照常参与平均；全部不可解析时
// 不输出指标而不是输出 0 —— 0 会把"没数据"伪装成"值是零"，而 IOPS 为 0
// 与 IOPS 未知在告警上完全不同。
func TestVsanPerfSkipsUnparsableSamples(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf: vsanPerfResponse(vsanPerfEntity(
			"cluster-domclient:uuid-1",
			"t1,t2,t3",
			// 中间一个畸形、一个空：平均应为 (10 + 30) / 2 = 20。
			vsanPerfSeries("iops_read", "10,n/a,30"),
			vsanPerfSeries("iops_write", "10,,30"),
			// 全部不可解析 —— 这条指标必须完全不输出。
			vsanPerfSeries("congestion", "x,y,z"),
		)),
	}

	series, values, err := collectVsanPerfCluster(t, stub)
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if got := values["vmware_vsan_perf_iops_read"]; len(got) != 1 || got[0] != 20 {
		t.Errorf("iops_read = %v, want [20]: one bad sample must not "+
			"discard the whole window", got)
	}

	if got := values["vmware_vsan_perf_iops_write"]; len(got) != 1 || got[0] != 20 {
		t.Errorf("iops_write = %v, want [20]", got)
	}

	if _, ok := series["vmware_vsan_perf_congestion"]; ok {
		t.Error("a series with no parsable sample must not be emitted as 0")
	}
}

// TestVsanPerfEmptyIntersectionSkipsQuery 锁死空交集的处置：不发性能查询。
//
// 空交集时发查询是无害的（vCenter 只会返回空），但那是每轮一次白花的 SOAP
// 往返，与组 A 的降级 2 同一个道理。
func TestVsanPerfEmptyIntersectionSkipsQuery(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		// 环境只支持白名单外的类型。
		entities: vsanPerfEntityTypes("vsan-iscsi-target", "host-memory-heap"),
		perf:     vsanPerfResponse(),
	}

	series, _, err := collectVsanPerfCluster(t, stub)
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if stub.perfCalls != 0 {
		t.Errorf("expected no performance query on an empty intersection, got %d calls",
			stub.perfCalls)
	}

	if len(series) != 0 {
		t.Errorf("expected no metrics, got %v", series)
	}
}

// TestVsanPerfSkipVerifyBypassesNegotiation 锁死逃生门。
//
// 开启后不问 vCenter 支持什么，直接用整份白名单查询。这个 flag 存在的理由
// 不是"用户想跳过检查"，而是 API 本身不申报全部可查实体类型
// （telegraf README 明说），取交集会漏掉真实可用的数据。
func TestVsanPerfSkipVerifyBypassesNegotiation(t *testing.T) {
	withVsanPerfSkipVerify(t, true)

	stub := &vsanPerfStub{
		// 刻意给一个"什么都不支持"的响应。skip-verify 生效时它不该被读到。
		entities: vsanPerfEntityTypes(),
		perf:     vsanPerfResponse(),
	}

	if _, _, err := collectVsanPerfCluster(t, stub); err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if stub.entitiesCalls != 0 {
		t.Errorf("expected no entity type negotiation with skip-verify, got %d calls",
			stub.entitiesCalls)
	}

	if len(stub.querySpecs) != len(vsanPerfEntityWhitelist) {
		t.Fatalf("got %d query specs, want %d (the full whitelist)",
			len(stub.querySpecs), len(vsanPerfEntityWhitelist))
	}

	for i, spec := range stub.querySpecs {
		want := vsanPerfEntityWhitelist[i] + ":*"
		if spec.EntityRefId != want {
			t.Errorf("spec[%d].EntityRefId = %q, want %q", i, spec.EntityRefId, want)
		}
	}
}

// TestVsanPerfEmptyResponseIsNotAnError 锁死本 collector 特有的第三条降级：
// vSAN 性能服务未开启时 vCenter 返回空数据而不是报错。
//
// 这是"打开了 flag 却没有指标"的最常见原因，所以必须是 nil error + 无指标，
// 而不是把整个 collector 记为失败 —— 后者会污染 vmware_scrape_errors_total。
func TestVsanPerfEmptyResponseIsNotAnError(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf:     vsanPerfResponse(),
	}

	series, _, err := collectVsanPerfCluster(t, stub)
	if err != nil {
		t.Fatalf("an empty performance response must not be an error, got: %v", err)
	}

	if len(series) != 0 {
		t.Errorf("expected no metrics, got %v", series)
	}
}

// TestVsanPerfRejectsMalformedEntityRefID 锁死 refID 的切分。
//
// 没有冒号时报错而不是把整串当实体类型：后者会产出一条 entityid 为空的
// 序列，同一类型下所有实体的序列会互相覆盖 —— 这正是 descs.go 里
// perfEntityLabels 加 default 分支要防的那类静默故障。
func TestVsanPerfRejectsMalformedEntityRefID(t *testing.T) {
	withVsanPerfSkipVerify(t, false)

	stub := &vsanPerfStub{
		entities: vsanPerfEntityTypes("cluster-domclient"),
		perf: vsanPerfResponse(
			vsanPerfEntity("no-colon-here", "t1", vsanPerfSeries("iops_read", "1")),
			// 正常的一条必须照常输出。
			vsanPerfEntity("cluster-domclient:uuid-ok", "t1",
				vsanPerfSeries("iops_read", "7")),
		),
	}

	series, values, err := collectVsanPerfCluster(t, stub)
	if err == nil {
		t.Fatal("expected an error for the malformed entity ref id, got nil")
	}

	got := series["vmware_vsan_perf_iops_read"]
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 iops_read series (from the valid entity), got %d", len(got))
	}

	if got[0]["entityid"] != "uuid-ok" {
		t.Errorf("entityid = %q, want %q", got[0]["entityid"], "uuid-ok")
	}

	if v := values["vmware_vsan_perf_iops_read"]; len(v) != 1 || v[0] != 7 {
		t.Errorf("iops_read = %v, want [7]", v)
	}
}

// TestAverageCSVValuesKeeps64BitPrecision 锁死 ParseFloat 的位宽。
//
// telegraf 用 ParseFloat(v, 32)，32 位浮点只有约 7 位有效十进制数字。
// 大集群的 throughput 是字节/秒，轻易过亿 —— 用 32 位会丢精度，而
// Prometheus 的值本来就是 float64，没有理由先降一次精度。
//
// 取值 123456789 在 float32 下会变成 123456792，差 3。
func TestAverageCSVValuesKeeps64BitPrecision(t *testing.T) {
	avg, n := averageCSVValues([]string{"123456789"})
	if n != 1 {
		t.Fatalf("sample count = %d, want 1", n)
	}

	if avg != 123456789 {
		t.Errorf("avg = %v, want 123456789 exactly; a 32-bit parse yields %v",
			avg, float64(float32(123456789)))
	}
}

// TestSplitCSVEmptyStringYieldsNothing 锁死空串的处理。
//
// strings.Split("", ",") 返回长度 1 的 [""]，会让"没有数据"看起来像
// "有一个空值" —— 长度校验会因此把一条空 sampleInfo 和一条单值序列
// 判为匹配，从而输出一条毫无意义的指标。
func TestSplitCSVEmptyStringYieldsNothing(t *testing.T) {
	if got := splitCSV(""); len(got) != 0 {
		t.Errorf("splitCSV(\"\") = %v (len %d), want empty", got, len(got))
	}

	if got := splitCSV("1,2"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("splitCSV(\"1,2\") = %v, want [1 2]", got)
	}
}

// TestVsanPerfSkipsESXi 锁死降级 1：ESXi 直连整体跳过。
//
// 与组 A 同一个理由：ESXi 上不存在 ClusterComputeResource，而
// vsan/simulator.go:20 只在 IsVPX() 时注册 vSAN 端点，真实 ESXi 同理
// 没有 /vsanHealth —— 硬查只会得到 404。
func TestVsanPerfSkipsESXi(t *testing.T) {
	// 断言"工厂没被调用"而不是靠工厂返回错误。
	//
	// 这个差别是反向验证逼出来的：让工厂返回 error 时，缺陷版的失败形态是
	// fetchProperties 解引用 nil 的 s.View 而 panic —— panic 也算红，但报出
	// 的是段错误，完全看不出这个测试守的是"ESXi 上不该建 vSAN 通道"。
	// 改成计数标记后，缺陷版会精确报出 "created a vsan client on ESXi"。
	// 与 vsan_test.go 的 TestVsanDisabledClusterSkipsRemainingQueries 同一个
	// 教训：断言要落在主张本身上，别让更早的崩溃替它说话。
	created := 0

	c := &vsanPerfCollector{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		newClient: func(context.Context, *collector.Scrape) (vsanRoundTripper, error) {
			created++
			return &vsanPerfStub{perf: vsanPerfResponse()}, nil
		},
	}

	// 走真实的 vcsim 模型而不是手搓一个只填两个字段的 Scrape：手搓的
	// Scrape 里 View 是 nil，缺陷版会在 fetchProperties 解引用它时 panic，
	// 把断言想说的话盖掉。
	//
	// 用 **VPX** 模型而不是 ESX，这是反向验证逼出来的第二个修正：ESX 模型
	// 里没有 ClusterComputeResource，删掉 isESXi 分支后代码会落到"没有集群"
	// 的提前返回上，测试照样绿 —— 那样这个测试就只是在验证一条它没打算验证
	// 的路径。VPX 模型有集群，于是唯一能让 created 保持 0 的原因就是
	// isESXi 分支真的生效了。
	//
	// TargetType 手工改成 ESXi：模型本身是 vCenter，我们测的是 collector
	// 对 TargetType 的反应，不是 vcsim 的拓扑。
	ctx, s, teardown := setupCollectorScrapeWithModel(t, simulator.VPX())
	defer teardown()

	s.TargetType = collector.TargetTypeESXi

	ch := make(chan prometheus.Metric, 10)

	if err := c.Update(ctx, ch, s); err != nil {
		t.Fatalf("Update() on ESXi returned error: %v", err)
	}

	if created != 0 {
		t.Errorf("created a vsan client on ESXi %d time(s); the collector must "+
			"return before touching the vSAN endpoint", created)
	}

	if got := drainMetrics(ch); len(got) != 0 {
		t.Errorf("expected no metrics on a direct ESXi connection, got %d", len(got))
	}
}

// TestVsanPerfDescIsCachedPerLabel 锁死 Desc 缓存。
//
// 同一个 label 必须复用同一个 *prometheus.Desc；实体类型不进 cache key
// （它是运行时 label 值，不影响指标名与 label 集合），所以两个不同实体
// 类型取同一个 label 应当拿到**同一个**指针。
func TestVsanPerfDescIsCachedPerLabel(t *testing.T) {
	a := vsanPerfDesc("vmware", "cluster-domclient", "iops_read")
	b := vsanPerfDesc("vmware", "host-domclient", "iops_read")

	if a != b {
		t.Error("the same label must reuse one Desc regardless of entity type")
	}

	if c := vsanPerfDesc("vmware", "cluster-domclient", "iops_write"); c == a {
		t.Error("different labels must not share a Desc")
	}

	// namespace 进 key：不同 namespace 是不同的指标名。
	if d := vsanPerfDesc("other", "cluster-domclient", "iops_read"); d == a {
		t.Error("different namespaces must not share a Desc")
	}
}
