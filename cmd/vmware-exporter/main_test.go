package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/vmware/govmomi/simulator"
)

func TestWebConfigUsesListenAddress(t *testing.T) {
	addr := ":9999"

	cfg := webConfig(&addr)

	if cfg == nil {
		t.Fatal("webConfig() returned nil")
	}

	if cfg.WebListenAddresses == nil {
		t.Fatal("WebListenAddresses is nil")
	}

	if len(*cfg.WebListenAddresses) != 1 {
		t.Fatalf("expected exactly 1 listen address, got %d", len(*cfg.WebListenAddresses))
	}

	if got := (*cfg.WebListenAddresses)[0]; got != addr {
		t.Fatalf("listen address = %q, want %q", got, addr)
	}

	if cfg.WebSystemdSocket == nil {
		t.Fatal("WebSystemdSocket is nil")
	}

	if *cfg.WebSystemdSocket {
		t.Fatal("WebSystemdSocket = true, want false")
	}

	if cfg.WebConfigFile == nil {
		t.Fatal("WebConfigFile is nil")
	}

	if *cfg.WebConfigFile != "" {
		t.Fatalf("WebConfigFile = %q, want empty string", *cfg.WebConfigFile)
	}
}

// setBoolFlag 设置一个已注册的 bool flag 并在测试结束时还原。
//
// 直接改 flag.Lookup 拿到的 Value 而不是包级变量指针：collector 的开关
// 定义在 vmware/collectors 包内且未导出，根包测试碰不到。
func setBoolFlag(t *testing.T, name string, v bool) {
	t.Helper()

	f := flag.Lookup(name)
	if f == nil {
		t.Fatalf("flag -%s is not registered", name)
	}

	old := f.Value.String()
	if err := f.Value.Set(strconv.FormatBool(v)); err != nil {
		t.Fatalf("could not set -%s: %v", name, err)
	}

	t.Cleanup(func() {
		if err := f.Value.Set(old); err != nil {
			t.Errorf("could not restore -%s: %v", name, err)
		}
	})
}

// restoreVMwareTargetFlags 把 -vmware.vcenter 指向一个必定连不上的地址，
// 并在测试结束后还原。
func restoreVMwareTargetFlags(t *testing.T) {
	t.Helper()

	f := flag.Lookup("vmware.vcenter")
	if f == nil {
		t.Fatal("flag -vmware.vcenter is not registered")
	}

	old := f.Value.String()
	// 127.0.0.1:1 —— 保留端口，不会有服务在听，连接立刻被拒绝而不是超时。
	if err := f.Value.Set("127.0.0.1:1"); err != nil {
		t.Fatalf("could not set -vmware.vcenter: %v", err)
	}

	t.Cleanup(func() {
		if err := f.Value.Set(old); err != nil {
			t.Errorf("could not restore -vmware.vcenter: %v", err)
		}
	})
}

// restoreExporterFlags 存取根包 flag，避免测试之间互相污染。
// 与 api 包的 restoreVMwareFlags 同一模式。
func restoreExporterFlags(t *testing.T) {
	t.Helper()

	oldMetrics := *disableExporterMetrics
	oldTarget := *disableExporterTarget
	oldConcurrency := *maxConcurrency

	t.Cleanup(func() {
		*disableExporterMetrics = oldMetrics
		*disableExporterTarget = oldTarget
		*maxConcurrency = oldConcurrency
	})
}

// TestCollectorSetDescribeExposesScrapeMetrics 验证 Describe 不是空实现。
// 空 Describe 会让 registry 把 collector 当作 unchecked，从而跳过重复注册检测。
//
// 期望数量从 2 变成 5：Stage 9 新增了 vmware_up 与
// vmware_scrape_duration_seconds，Stage 10a 又加了 vmware_scrape_errors_total。
//   - vmware_up：不是 Prometheus 自己生成的那个 up（那个只表示 HTTP 请求
//     成功）。对多 target exporter 来说 HTTP 成功而 vCenter 登录失败是常态，
//     没有这个指标就写不出「目标不可达」的告警。
//   - vmware_scrape_duration_seconds：通用 exporter 告警规则查的是这个无标签
//     版本，框架只有带 collector="all_collectors" 标签的那个。
//   - vmware_scrape_errors_total：全库唯一的 counter。collector_success 只能
//     回答「最近一轮成不成」，答不出「过去一小时失败几次」—— 间歇性故障在
//     gauge 上会被采样间隔漏掉。
func TestCollectorSetDescribeExposesScrapeMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cs, err := collector.NewCollectorSet(context.Background(), vmwareCollectors.Definitions(), collector.Options{
		Namespace: namespace,
		Login:     vmware.NewAPI(),
		Logger:    logger,
		// Enabled 为 nil 时按各 collector 的默认开关走，这里只关心 Describe，
		// 不需要真的启用任何 collector。
		Enabled: map[string]bool{},
		Errors:  collector.NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet() returned error: %v", err)
	}

	ch := make(chan *prometheus.Desc, 16)
	cs.Describe(ch)
	close(ch)

	var descs []string
	for d := range ch {
		descs = append(descs, d.String())
	}

	want := []string{
		"vmware_up",
		"vmware_scrape_duration_seconds",
		"vmware_scrape_collector_duration_seconds",
		"vmware_scrape_collector_success",
		"vmware_scrape_errors_total",
	}

	if len(descs) != len(want) {
		t.Fatalf("Describe() emitted %d descriptors, want %d:\n%s",
			len(descs), len(want), strings.Join(descs, "\n"))
	}

	joined := strings.Join(descs, "\n")
	for _, w := range want {
		if !strings.Contains(joined, `fqName: "`+w+`"`) {
			t.Fatalf("Describe() output is missing %q, got:\n%s", w, joined)
		}
	}
}

// TestCollectorDefinitionsMatchRegisteredFlags 保证 collector 清单与命令行
// 开关一一对应。清单是 /probe 的来源，命令行开关是 /metrics 的来源；
// 两者不同步就会让同一个 collector 在两个端点上表现不一致。
func TestCollectorDefinitionsMatchRegisteredFlags(t *testing.T) {
	for _, name := range vmwareCollectors.Names() {
		flagName := "collector." + name

		if flag.Lookup(flagName) == nil {
			t.Fatalf("collector %q is listed in Definitions() but has no -%s flag registered; /metrics and /probe would diverge",
				name, flagName)
		}
	}
}

// TestScrapeAgainstESXiSimulator 是 Stage 3 的核心验收：直连一台 ESXi 时
// 全部 7 个 collector 都要能跑完且不 panic。
//
// simulator.ESX() 的对象模型与真实 ESXi 一致 —— Datacenter / Cluster /
// ClusterHost 全为零值（simulator/model.go:141），只有隐式的 ha-* 伪对象，
// ApiType 是 "HostAgent"（simulator/esx/service_content.go:27）。
// 因此这个测试不需要真实的 ESXi 主机。
//
// 之所以要跑全部 collector 而不是挑几个：ESXi 缺层的影响面很难靠推理穷举，
// 缺 Datacenter 时哪个 collector 会在解引用 Parent 时 panic，只有真跑一遍才知道。
func TestScrapeAgainstESXiSimulator(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.ESX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create ESX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=all",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// 每个 collector 都要有成功标记。success=0 说明 Update() 返回了错误，
	// 在 ESXi 上通常意味着某个 vCenter 专属假设没有被正确分支掉。
	for _, name := range vmwareCollectors.Names() {
		want := fmt.Sprintf(`vmware_scrape_collector_success{collector="%s"} 1`, name)
		if !strings.Contains(body, want) {
			t.Errorf("collector %q did not succeed against ESXi\nlooking for: %s", name, want)
		}
	}

	// 类型标识指标是全部 dashboard 条件渲染的依据。
	if !strings.Contains(body, `type="esxi"`) {
		t.Errorf(`vmware_target_info is missing type="esxi"`)
	}

	if !strings.Contains(body, "vmware_target_info{") {
		t.Errorf("vmware_target_info was not emitted at all")
	}
}

// TestESXiEmitsSyntheticDatacenterAndCompute 验证伪对象被正确标注。
//
// 路线丙的核心取舍就在这里：ESXi 上仍然输出 datacenter / compute 指标（否则
// dashboard 里依赖 dcmo / cmo 的关联查询会断链），但打上 synthetic="true"，
// 让「这不是真实数据中心」这件事在指标层面可见。
func TestESXiEmitsSyntheticDatacenterAndCompute(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.ESX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create ESX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=cluster",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// datacenter 指标必须存在（不断链）且被标记（不含糊）。
	if !strings.Contains(body, "vmware_datacenter_info{") {
		t.Fatalf("vmware_datacenter_info missing on ESXi; dashboards relying on dcmo would break\nbody:\n%s", body)
	}

	if !strings.Contains(body, `dcmo="ha-datacenter"`) {
		t.Errorf("expected the implicit ha-datacenter pseudo object\nbody:\n%s", body)
	}

	// 断言必须精确到具体的指标行。只检查 body 里出现过 synthetic="true"
	// 是不够的 —— cluster 的 ha-compute-res 也带这个 label，会掩盖
	// datacenter 分支根本没生效的情况。这是反向验证时发现的：注掉
	// datacenter 的标注逻辑后，宽泛断言依然通过。
	if !metricHasLabels(body, "vmware_datacenter_info", `dcmo="ha-datacenter"`, `synthetic="true"`) {
		t.Errorf("vmware_datacenter_info for ha-datacenter is not marked synthetic; "+
			"the pseudo object would be indistinguishable from a real datacenter\nbody:\n%s", body)
	}

	if !metricHasLabels(body, "vmware_compute_info", `cmo="ha-compute-res"`, `synthetic="true"`) {
		t.Errorf("vmware_compute_info for ha-compute-res is not marked synthetic\nbody:\n%s", body)
	}

	// cluster collector 在 ESXi 上应回退到 ComputeResource。
	if !strings.Contains(body, "vmware_compute_info{") {
		t.Errorf("vmware_compute_info missing; the ComputeResource fallback did not run\nbody:\n%s", body)
	}
}

// TestVCenterDoesNotEmitSyntheticLabel 是 vCenter 侧的回归防线。
//
// synthetic 只应出现在 ESXi 的伪对象上。若它泄漏到 vCenter，用户用
// {synthetic!="true"} 过滤时会误伤真实数据中心 —— 这类错误在图上表现为
// 数据莫名消失，很难定位到 exporter。
func TestVCenterDoesNotEmitSyntheticLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create VPX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=cluster",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	if strings.Contains(body, `synthetic="true"`) {
		t.Fatalf("synthetic label leaked into vCenter output\nbody:\n%s", body)
	}

	if !strings.Contains(body, `type="vcenter"`) {
		t.Errorf(`vmware_target_info is missing type="vcenter"`)
	}

	// 既有指标必须原样保留 —— 用户的 dashboard 引用的是它，不是新指标。
	if !strings.Contains(body, "vmware_vcenter_info{") {
		t.Errorf("vmware_vcenter_info disappeared; existing dashboards would break")
	}
}

// metricHasLabels 检查指定指标族里是否存在同时带有全部给定 label 的序列。
//
// 为什么需要它：strings.Contains(body, `synthetic="true"`) 这种宽泛断言
// 只能证明整个响应里某处出现过该 label，无法定位到哪个指标。当多个 collector
// 都可能产出同一个 label 时，宽泛断言会掩盖其中一个分支失效的情况 ——
// Stage 3 的反向验证就撞上了这个坑。
func metricHasLabels(body, metricName string, labels ...string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}

		matched := true
		for _, label := range labels {
			if !strings.Contains(line, label) {
				matched = false
				break
			}
		}

		if matched {
			return true
		}
	}

	return false
}

// TestTargetInfoTargetLabelMatchesVCenterLabel 锁死 dashboard 依赖的一条不变量。
//
// 背景：Stage 3 的 dashboard 改造把 $vcenter 模板变量的取值来源从
// vmware_vcenter_info{...}的 vcenter label 换成了
// vmware_target_info{...}的 target label —— 因为只有后者带 type label，
// 换过去才能按目标类型过滤。
//
// 而所有 panel 的查询依然写着 vcenter=~"$vcenter"。也就是说：
//
//	target_info 的 target 值  ->  填进 $vcenter  ->  用来匹配业务指标的 vcenter 值
//
// 这条链要成立，两个 label 的取值必须逐字符相等。一旦哪天有人给其中一个
// 加了端口归一化、去掉了 :443、或改成小写，dashboard 会**静默**变空 ——
// 查询语法合法、指标存在、只是匹配不上，没有任何报错。
//
// 这种失败模式在 Grafana 里极难排查，所以在这里用测试钉住。
func TestTargetInfoTargetLabelMatchesVCenterLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name  string
		model *simulator.Model
	}{
		{"vcenter", simulator.VPX()},
		{"esxi", simulator.ESX()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if err := model.Create(); err != nil {
				t.Fatalf("failed to create simulator model: %v", err)
			}
			defer model.Remove()

			server := model.Service.NewServer()
			defer server.Close()

			url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=host",
				server.URL.Host, server.URL.Scheme)

			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			probeHandler(rec, req, logger)

			if rec.Code != http.StatusOK {
				t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
			}

			body := rec.Body.String()

			target := labelValue(body, "vmware_target_info", "target")
			if target == "" {
				t.Fatalf("could not extract target label from vmware_target_info\nbody:\n%s", body)
			}

			// vmware_vcenter_info 是既有指标，$vcenter 原来就取自它。
			vcenter := labelValue(body, "vmware_vcenter_info", "vcenter")
			if vcenter == "" {
				t.Fatalf("could not extract vcenter label from vmware_vcenter_info\nbody:\n%s", body)
			}

			if target != vcenter {
				t.Errorf("vmware_target_info{target=%q} != vmware_vcenter_info{vcenter=%q};\n"+
					"the bundled dashboards feed $vcenter from target_info's target label but filter\n"+
					"panels on vcenter=~\"$vcenter\", so a mismatch silently blanks every panel",
					target, vcenter)
			}

			// 业务指标侧同样必须匹配，否则 panel 过滤照样落空。
			hostVCenter := labelValue(body, "vmware_host_info", "vcenter")
			if hostVCenter == "" {
				t.Fatalf("could not extract vcenter label from vmware_host_info\nbody:\n%s", body)
			}

			if hostVCenter != target {
				t.Errorf("vmware_host_info{vcenter=%q} != vmware_target_info{target=%q}; panel filters would not match",
					hostVCenter, target)
			}
		})
	}
}

// labelValue 从 Prometheus 文本格式里取出指定指标第一条序列上某个 label 的值。
//
// 只做够用的解析：label 值本身不含逗号或引号（这里全是主机名与 moid），
// 所以按 `name="` 定位再截到下一个引号即可，不必引入完整的 expfmt 解析器。
func labelValue(body, metricName, label string) string {
	needle := label + `="`

	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}

		idx := strings.Index(line, needle)
		if idx < 0 {
			continue
		}

		rest := line[idx+len(needle):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}

		return rest[:end]
	}

	return ""
}

// TestWebConfigPassesThroughConfigFile 覆盖 -web.config.file 是否真的接到了
// exporter-toolkit。
//
// 上面那个 TestWebConfigUsesListenAddress 只断言默认值为空 —— 在 webConfig()
// 里硬编码 `configFile := ""` 的旧实现下它同样能通过，所以它对这条链路是
// 没有约束力的。这里改设 flag 再读回，才能区分「接上了」与「返回了一个恰好
// 也是空字符串的局部变量」。
//
// 这条链路断掉是静默的：exporter 会正常启动、正常服务，只是明文 HTTP，
// 而 /probe 的 URL 参数与 Basic Auth 里带着 vCenter 凭证。
func TestWebConfigPassesThroughConfigFile(t *testing.T) {
	original := *webConfigFile
	t.Cleanup(func() { *webConfigFile = original })

	const want = "/etc/vmware-exporter/web-config.yml"
	*webConfigFile = want

	addr := ":9169"
	cfg := webConfig(&addr)

	if cfg.WebConfigFile == nil {
		t.Fatal("WebConfigFile is nil")
	}

	if got := *cfg.WebConfigFile; got != want {
		t.Fatalf("WebConfigFile = %q, want %q\n"+
			"  the flag is not reaching web.FlagConfig, so TLS and basic auth "+
			"cannot be enabled at all", got, want)
	}
}

// TestWebConfigFileFlagIsRegistered 断言 flag 以预期的名字注册。
//
// 名字是对外契约：systemd unit、compose 文件和文档都写死了它。而且
// exporter-toolkit 上游用的就是 web.config.file，跟着它走能让用户在不同
// exporter 之间复用同一套部署脚本。
func TestWebConfigFileFlagIsRegistered(t *testing.T) {
	f := flag.Lookup("web.config.file")
	if f == nil {
		t.Fatal("flag -web.config.file is not registered")
	}

	if f.DefValue != "" {
		t.Errorf("default = %q, want empty (TLS off unless explicitly configured)", f.DefValue)
	}

	// 说明文字里必须留下能查到格式的线索 —— 这个文件的 schema 不是自解释的，
	// 用户没有指引就只能猜。
	if !strings.Contains(f.Usage, "exporter-toolkit") {
		t.Errorf("usage text does not point at the exporter-toolkit docs: %q", f.Usage)
	}
}

// TestWebConfigFileAcceptsGeneratedTLSConfig 用真实的证书与配置文件走一遍
// exporter-toolkit 的加载路径。
//
// 前两个测试只证明字符串传到了结构体里，不能证明这个值最终真的被用于建立
// TLS。这里生成一张自签证书、写一份最小 web config、启一个 server，然后用
// HTTPS 客户端去访问 —— 如果配置没生效，服务端会是明文 HTTP，TLS 握手就会
// 失败。
func TestWebConfigFileAcceptsGeneratedTLSConfig(t *testing.T) {
	dir := t.TempDir()

	certPath, keyPath := writeSelfSignedCert(t, dir)

	configPath := filepath.Join(dir, "web-config.yml")
	configBody := fmt.Sprintf("tls_server_config:\n  cert_file: %s\n  key_file: %s\n",
		certPath, keyPath)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write web config: %v", err)
	}

	original := *webConfigFile
	t.Cleanup(func() { *webConfigFile = original })
	*webConfigFile = configPath

	// 监听 127.0.0.1:0 拿一个空闲端口，避免与并行测试抢固定端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, "pong"); err != nil {
			t.Errorf("write response body: %v", err)
		}
	})
	srv := &http.Server{Handler: mux}
	t.Cleanup(func() { _ = srv.Close() })

	listenAddr := addr
	cfg := webConfig(&listenAddr)

	errCh := make(chan error, 1)
	go func() {
		// ServeMultiple 会读取 WebConfigFile 并据此包上 TLS。
		errCh <- web.ServeMultiple([]net.Listener{ln}, srv, cfg,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	client := &http.Client{
		Transport: &http.Transport{
			// 自签证书，跳过校验；这里要验证的是「有没有 TLS」，
			// 不是「证书链是否可信」。
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}

	var resp *http.Response
	var lastErr error
	// server 启动是异步的，短暂重试几次。
	for i := 0; i < 20; i++ {
		resp, lastErr = client.Get("https://" + addr + "/ping")
		if lastErr == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("server exited early: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("HTTPS request failed after retries: %v\n"+
			"  the web config file was not applied, so the listener is plain HTTP", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want %q", body, "pong")
	}
	if resp.TLS == nil {
		t.Fatal("resp.TLS is nil: the connection was not encrypted")
	}
}

// writeSelfSignedCert 生成一张仅用于测试的自签证书，返回证书与私钥的路径。
func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "vmware-exporter-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatalf("close cert file: %v", err)
	}

	keyPath := filepath.Join(dir, "key.pem")
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatalf("close key file: %v", err)
	}

	return certPath, keyPath
}
