package vmware

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/session/cache"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

var (
	// 全局默认凭证（用于默认 /metrics 端点）
	vmwUser       = flag.String("vmware.username", "", "Username to login to vCenter server")
	vmwPasswd     = flag.String("vmware.password", "", "Password for the user above")
	vCenter       = flag.String("vmware.vcenter", "", "vCenter server address in host:port format. This is not the vCenter Management Console")
	vmwSchema     = flag.String("vmware.schema", "https", "Use HTTP or HTTPS")
	vmwTLS        = flag.Bool("vmware.insecureTLS", false, "Trust insecure vCenter TLS (true) or verify (default)")
	vmwInterval   = flag.Int("vmware.interval", 20, "PerfManager sampling window in seconds. Default is 20s.")
	vmGranularity = flag.Int("vmware.granularity", 20, "The frequency of the sampled data. Default is 20s")
	vmwTimeout    = flag.Int("vmware.timeout", 60, "Overall timeout in seconds for a single scrape (login, property retrieval and performance sampling). Independent from -vmware.interval.")
)

// logoutTimeout 是 SOAP Logout 单独使用的超时。抓取用的 ctx 在 Logout 时
// 很可能已经超时或被取消，复用它会导致登出请求直接失败、会话继续泄漏，
// 因此这里必须用一个独立的、短的超时。
const logoutTimeout = 10 * time.Second

// ValidateFlags 在启动阶段校验 vmware.* 参数组合，避免把非法值带进运行期
// 引发除零 panic 或永远拿不到采样数据。必须在 flag 解析之后、HTTP 服务
// 启动之前调用。
func ValidateFlags() error {
	if *vmGranularity <= 0 {
		return fmt.Errorf("-vmware.granularity must be greater than 0, got %d", *vmGranularity)
	}

	if *vmwInterval <= 0 {
		return fmt.Errorf("-vmware.interval must be greater than 0, got %d", *vmwInterval)
	}

	if *vmwInterval < *vmGranularity {
		return fmt.Errorf("-vmware.interval (%d) must be greater than or equal to -vmware.granularity (%d), otherwise no sample would ever be collected", *vmwInterval, *vmGranularity)
	}

	if *vmwTimeout <= 0 {
		return fmt.Errorf("-vmware.timeout must be greater than 0, got %d", *vmwTimeout)
	}

	if *vmwSchema != "http" && *vmwSchema != "https" {
		return fmt.Errorf(`-vmware.schema must be either "http" or "https", got %q`, *vmwSchema)
	}

	return nil
}

type VMware struct {
	//logger log.Logger
}

// Credentials 存储每个 target 的凭证
type Credentials struct {
	Username string
	Password string
	Target   string
	Schema   string
	Insecure bool
}

// NewAPI 构造一个 VMware API 客户端工厂。
//
// 改动前本包的 init() 会调用 collector.RegisterAPI(NewAPI())，把实例存进
// 框架的一个包级私有变量。现在 CollectorSet 通过 Options.Login 显式接收
// 实现 —— 依赖关系从「藏在 init() 里的全局副作用」变成了构造参数。
func NewAPI() *VMware {
	return &VMware{}
}

func Load(logger *slog.Logger) {
	logger.Info("Loading VMware vSphere API")
}

// Login 实现 internal/collector.Login，是 CollectorSet 建立会话的入口。
//
// 与旧的 Login(target, logger) 有两处关键差别：
//
//  1. ctx 由调用方传入，而不是内部 context.Background()。改动前
//     govmomiLoginWithCreds 无条件用 Background 派生 scrape context，于是
//     客户端断连、Prometheus 抓取超时都无法取消已发出的 SOAP 调用 ——
//     它们会一直跑到 -vmware.timeout 自然到期，期间继续占着 vCenter 的
//     会话与连接。现在 timeout 是上限而非唯一依据：谁先到期以谁为准。
//
//  2. 返回 *collector.Scrape 与一个 cleanup，而不是 map[string]interface{}。
//     cleanup 合并了原来的 Logout，调用方 defer 一次即可，不需要知道
//     「先 SOAP 登出再 cancel」这个顺序约束。
func (vm *VMware) Login(ctx context.Context, target string) (*collector.Scrape, func(), error) {
	noop := func() {}

	if target == "" {
		target = *vCenter
	}

	if *vmwUser == "" || *vmwPasswd == "" {
		return nil, noop, fmt.Errorf("default credentials not configured. Please set -vmware.username and -vmware.password flags")
	}

	if target == "" {
		return nil, noop, fmt.Errorf("target not specified and -vmware.vcenter flag not set")
	}

	return vm.LoginWithCredentials(ctx, Credentials{
		Username: *vmwUser,
		Password: *vmwPasswd,
		Target:   target,
		Schema:   *vmwSchema,
		Insecure: *vmwTLS,
	}, slog.Default())
}

// LoginWithCredentials 用显式凭证登录，服务 /probe 的多 target 模式。
//
// 返回的 cleanup 永不为 nil，调用方可以无条件 defer 而不必判空。
func (vm *VMware) LoginWithCredentials(ctx context.Context, creds Credentials, logger *slog.Logger) (*collector.Scrape, func(), error) {
	noop := func() {}

	if creds.Target == "" {
		return nil, noop, fmt.Errorf("target is required")
	}

	if creds.Username == "" || creds.Password == "" {
		return nil, noop, fmt.Errorf("username and password are required")
	}

	if creds.Schema == "" {
		creds.Schema = *vmwSchema
	}

	if logger == nil {
		logger = slog.Default()
	}

	urlx, err := soap.ParseURL(fmt.Sprintf("%s://%s%s", creds.Schema, creds.Target, vim25.Path))
	if err != nil {
		return nil, noop, fmt.Errorf("soap url err: %s", err)
	}

	urlx.User = url.UserPassword(creds.Username, creds.Password)

	// -vmware.timeout 现在是上限而非唯一依据：ctx 来自 http.Request，
	// 客户端断连时它先被取消；反之若客户端很有耐心，timeout 仍然兜住。
	//
	// 旧实现是 context.WithTimeout(context.Background(), ...)，请求侧的取消
	// 完全传不进来。更早的版本还从采样频率推导超时（interval-2 秒，默认只有
	// 18s），大规模环境下属性检索还没跑完就被掐断。
	scrapeCtx, cancel := context.WithTimeout(ctx, time.Duration(*vmwTimeout)*time.Second)

	session := &cache.Session{
		URL:         urlx,
		Insecure:    creds.Insecure,
		Passthrough: true,
	}

	client := new(vim25.Client)

	// cleanup 的两步顺序至关重要，不要调换：先向服务端发 SOAP 登出释放会话，
	// 再 cancel 释放本地资源。反过来的话 scrapeCtx 已失效，登出请求会立刻
	// 失败，服务端会话就一直挂到自然超时（vCenter 默认 30 分钟），高频抓取
	// 下会迅速堆到会话上限。
	//
	// 登出用独立的短超时而非 scrapeCtx：走到 cleanup 时 scrapeCtx 很可能
	// 已经超时或被取消。
	//
	// Passthrough=true 时 cache.Session.Logout 才会真正调用
	// SessionManager.Logout，这是 session 必须被闭包捕获的原因。
	cleanup := func() {
		logoutCtx, logoutCancel := context.WithTimeout(context.Background(), logoutTimeout)
		if err := session.Logout(logoutCtx, client); err != nil {
			logger.Error("SOAP logout failed, server-side session may linger until it times out",
				"target", creds.Target, "error", err)
		} else {
			logger.Debug("SOAP logout succeeded", "target", creds.Target)
		}
		logoutCancel()

		cancel()
	}

	if err := session.Login(scrapeCtx, client, nil); err != nil {
		// 登录本身失败时没有服务端会话可登出，只释放本地资源。
		cancel()
		return nil, noop, fmt.Errorf("login err: %s", err)
	}

	perf := performance.NewManager(client)

	counters, err := perf.CounterInfoByName(scrapeCtx)
	if err != nil {
		// 这里已经登录成功了，必须走完整 cleanup 释放服务端会话。
		cleanup()
		return nil, noop, fmt.Errorf("perfman counters err: %s", err)
	}

	// granularity 已在启动时由 ValidateFlags 保证 > 0，这里不会除零。
	// 同时保证至少取 1 个采样点，避免 interval 略小于 granularity 时算出 0。
	samples := *vmwInterval / *vmGranularity
	if samples < 1 {
		samples = 1
	}

	s := &collector.Scrape{
		Client:   client,
		View:     view.NewManager(client),
		Perf:     perf,
		Counters: counters,
		Target:   creds.Target,
		// 目标类型探测必须在登录成功之后 —— ServiceContent 是登录的产物。
		// 结果供全部下游 collector 选择行为分支。
		TargetType: detectTargetType(client, logger),
		Interval:   int32(*vmwInterval),
		Samples:    int32(samples),
	}

	// 不记录 username：probe 端点的凭证来自请求参数或 Basic Auth，
	// 写进日志会让凭证随日志流出到集中式日志系统。
	logger.Info("logged in to vCenter", "target", creds.Target, "target_type", s.TargetType)

	return s, cleanup, nil
}
