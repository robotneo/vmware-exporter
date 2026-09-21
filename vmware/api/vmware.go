package vmware

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prezhdarov/vmware-exporter/internal/config"
	"github.com/prezhdarov/vmware-exporter/internal/safedial"
	"github.com/prezhdarov/vmware-exporter/internal/target"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/session/cache"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
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

	// perfChunkSize 限制单次 QueryPerf SOAP 请求携带的实体数。
	//
	// vCenter 有 vpxd.stats.maxQueryMetrics 上限（约束 对象数×计数器数 的
	// 指标总量），几千台 VM 连同十几个计数器塞进一个请求会超限失败或撞单次
	// 超时；分块后多块在 -collector.max-concurrency 的有界并发下发出，结果
	// 按实体顺序合并，输出序列与单请求逐字一致。
	//
	// 默认 64 与 telegraf inputs.vsphere 的 max_query_objects 默认值同量级。
	// 0 表示不分块（v0.1.19 及更早的行为），小规模环境或 ESXi 直连可显式设 0。
	vmwPerfChunkSize = flag.Int("vmware.perf.chunk-size", 64,
		"Maximum number of entities (hosts/VMs/datastores) per QueryPerf SOAP request. Chunks run with bounded concurrency and merge in entity order. 0 disables chunking (single request, pre-v0.1.20 behavior).")

	// counterCacheTTL 控制 PerfCounterInfo 元数据在 /metrics 路径的复用时长
	// （P-03）。默认 10m；0 关闭、每次登录实时拉取。只在 /metrics 生效，
	// /probe 多租户路径永不读缓存。
	vmwCounterCacheTTL = flag.Duration("scrape.counter-cache-ttl", 10*time.Minute,
		"How long the performance-counter metadata table (counter name to id/unit) is reused on the /metrics path. The cache key includes the target plus its vCenter About version/build, so an upgrade is picked up within one TTL. 0 fetches it on every login. Never used on the multi-tenant /probe path.")

	// providerCacheTTL 控制 QueryPerfProviderSummary（按实体类型协商采样间隔）
	// 在 /metrics 路径的复用时长。govmomi 的 ProviderSummary 名义按 entity.Type
	// 缓存、实际每次都发 SOAP，而 host/vm/datastore 每轮各协商一次、Manager 又
	// 每登录新建 —— 这个缓存把默认路径每轮约 3 次跨网络往返收成按类型一次。
	// 边界同 counter/inventory 缓存：仅 /metrics，/probe 永不读缓存。
	vmwProviderCacheTTL = flag.Duration("scrape.perf-interval-cache-ttl", 10*time.Minute,
		"How long the per-entity-type QueryPerfProviderSummary result (used to negotiate the real-time/historical sampling interval) is reused on the /metrics path. govmomi's Manager.ProviderSummary is documented as cached by entity type but actually issues a SOAP call every time, and host/vm/datastore each negotiate once per scrape. Key includes target, vCenter About version/build and entity type. 0 queries it on every scrape. Never used on the multi-tenant /probe path.")

	// vmwDenyPrivate 是 S-06 拨号侧 IP 复核的加严开关。
	//
	// 默认（false）只阻断链路本地（含云元数据 169.254.169.254）与未指定地址，
	// 对跑在 RFC1918 内网的 vCenter/ESXi（最常见形态）零误伤；置 true 后再
	// 一并阻断环回与私网，适用于 exporter 与 vCenter 走可路由地址、或要求强
	// 隔离的部署。两种模式都在真正 connect(2) 前按本次实际解析出的 IP 判定，
	// 堵住 DNS rebinding。详见 internal/safedial。
	vmwDenyPrivate = flag.Bool("vmware.deny-private-addresses", false,
		"Also reject dialing loopback and RFC1918/ULA private addresses when connecting to vCenter, in addition to the always-blocked link-local (incl. 169.254.169.254 cloud metadata) and unspecified addresses. Enable only when vCenter is reached over routable addresses; do NOT enable for an on-LAN vCenter or a 127.0.0.1 sidecar proxy.")
)

// logoutTimeout 是 SOAP Logout 单独使用的超时。抓取用的 ctx 在 Logout 时
// 很可能已经超时或被取消，复用它会导致登出请求直接失败、会话继续泄漏，
// 因此这里必须用一个独立的、短的超时。
const logoutTimeout = 10 * time.Second

// ValidateFlags 在启动阶段校验 vmware.* 参数组合，避免把非法值带进运行期
// 引发除零 panic 或永远拿不到采样数据。必须在 flag 解析之后、HTTP 服务
// 启动之前调用。
//
// 也在 SIGHUP 重载之后被调用，那时 HTTP 服务已经在跑，所以它同样要走快照
// 而不是裸读 flag —— 否则这个「用来防止坏配置进入运行期」的函数自己就是
// 一处数据竞争。
func ValidateFlags() error {
	cfg := currentSettings()

	if cfg.granularity <= 0 {
		return fmt.Errorf("-vmware.granularity must be greater than 0, got %d", cfg.granularity)
	}

	if cfg.interval <= 0 {
		return fmt.Errorf("-vmware.interval must be greater than 0, got %d", cfg.interval)
	}

	if cfg.interval < cfg.granularity {
		return fmt.Errorf("-vmware.interval (%d) must be greater than or equal to -vmware.granularity (%d), otherwise no sample would ever be collected", cfg.interval, cfg.granularity)
	}

	if cfg.timeout <= 0 {
		return fmt.Errorf("-vmware.timeout must be greater than 0, got %d", cfg.timeout)
	}

	// 0 是合法值（不分块），所以只拒负数；上不封顶，超大值等价于 0。
	if cfg.chunkSize < 0 {
		return fmt.Errorf("-vmware.perf.chunk-size must be greater than or equal to 0, got %d", cfg.chunkSize)
	}

	// 0 关闭计数器缓存（每次登录实时拉取），合法；拒负数。
	if cfg.counterCacheTTL < 0 {
		return fmt.Errorf("-scrape.counter-cache-ttl must be greater than or equal to 0, got %s", cfg.counterCacheTTL)
	}

	// 0 关闭采样间隔协商缓存，合法；拒负数。
	if cfg.providerCacheTTL < 0 {
		return fmt.Errorf("-scrape.perf-interval-cache-ttl must be greater than or equal to 0, got %s", cfg.providerCacheTTL)
	}

	if cfg.schema != "http" && cfg.schema != "https" {
		return fmt.Errorf(`-vmware.schema must be either "http" or "https", got %q`, cfg.schema)
	}

	return nil
}

// settings 是一轮抓取用到的全部 vmware.* 配置的快照。
//
// 存在的理由是数据竞争：SIGHUP 重载通过 flag.FlagSet.Set 改这些 flag，
// 而 flag 包的 setter 是裸写（标准库 flag.go 里 intValue.Set 的最后一行
// 就是 `*i = intValue(v)`，没有任何同步原语）。这些 flag 又全部在请求
// 路径上解引用 —— 那正是「改 flag 值即可热重载」成立的前提。于是重载
// 协程写、抓取协程读，构成数据竞争。
//
// 快照一次而不是每处加锁，还顺带修掉一个正确性问题：以前 Login 里
// 分三处解引用（timeout 在 L162、interval/granularity 在 L212 与 L226），
// 一次恰好落在中间的重载会让同一轮抓取用上两份配置的混合值 —— 例如
// interval 取新值、granularity 取旧值，算出的 samples 是任何一份配置里
// 都不存在的数。
type settings struct {
	user             string
	passwd           string
	vcenter          string
	schema           string
	insecureTLS      bool
	interval         int
	granularity      int
	timeout          int
	chunkSize        int
	counterCacheTTL  time.Duration
	providerCacheTTL time.Duration
	denyPrivate      bool
}

// currentSettings 在读锁保护下一次性拷出全部 vmware.* flag。
//
// 一次性读完是契约的一部分：分多次 RLock 会读到重载前后混合的配置，
// 那正是这个函数要消除的问题。
func currentSettings() settings {
	var s settings

	config.Snapshot(func() {
		s = settings{
			user:             *vmwUser,
			passwd:           *vmwPasswd,
			vcenter:          *vCenter,
			schema:           *vmwSchema,
			insecureTLS:      *vmwTLS,
			interval:         *vmwInterval,
			granularity:      *vmGranularity,
			timeout:          *vmwTimeout,
			chunkSize:        *vmwPerfChunkSize,
			counterCacheTTL:  *vmwCounterCacheTTL,
			providerCacheTTL: *vmwProviderCacheTTL,
			denyPrivate:      *vmwDenyPrivate,
		}
	})

	return s
}

type VMware struct {
	// counterCache 非 nil 时（仅 /metrics 的单服务级凭证路径构造）在登录时
	// 按 target+About 版本缓存计数器元数据（P-03）。/probe 与裸 NewAPI()
	// 保持 nil，每请求实时拉取，与 InventoryCache 同一条越权读边界。
	counterCache *CounterCache

	// providerCache 非 nil 时（同上路径）把按实体类型协商采样间隔用的
	// QueryPerfProviderSummary 结果缓存起来（govmomi 名义缓存、实际每次发
	// SOAP）。登录成功后把绑定本缓存+key+TTL 的闭包装进 Scrape.ProviderSummary，
	// 各 collector 经它协商；/probe 与 NewAPI() 保持 nil 实时协商。
	providerCache *ProviderSummaryCache
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
//
// 不带任何缓存：/probe 多租户路径与不想要缓存的调用方用它，计数器元数据与
// 采样间隔协商都每请求实时进行。/metrics 路径用 NewAPIWithCaches。
func NewAPI() *VMware {
	return &VMware{}
}

// NewAPIWithCaches 构造带进程级计数器元数据缓存（P-03）与采样间隔协商缓存的
// 工厂。只应由使用单一服务级凭证的 /metrics 路径使用；任一 cache 为 nil 或其
// TTL<=0 时对应路径自动回退为与 NewAPI 相同的实时行为。
func NewAPIWithCaches(counterCache *CounterCache, providerCache *ProviderSummaryCache) *VMware {
	return &VMware{counterCache: counterCache, providerCache: providerCache}
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

	cfg := currentSettings()

	if target == "" {
		target = cfg.vcenter
	}

	if cfg.user == "" || cfg.passwd == "" {
		return nil, noop, fmt.Errorf("default credentials not configured. Please set -vmware.username and -vmware.password flags")
	}

	if target == "" {
		return nil, noop, fmt.Errorf("target not specified and -vmware.vcenter flag not set")
	}

	return vm.loginWithCredentials(ctx, Credentials{
		Username: cfg.user,
		Password: cfg.passwd,
		Target:   target,
		Schema:   cfg.schema,
		Insecure: cfg.insecureTLS,
	}, slog.Default(), cfg)
}

// LoginWithCredentials 用显式凭证登录，服务 /probe 的多 target 模式。
//
// 返回的 cleanup 永不为 nil，调用方可以无条件 defer 而不必判空。
func (vm *VMware) LoginWithCredentials(ctx context.Context, creds Credentials, logger *slog.Logger) (*collector.Scrape, func(), error) {
	return vm.loginWithCredentials(ctx, creds, logger, currentSettings())
}

// loginWithCredentials 是上面两个入口的共同实现，cfg 由调用方快照后传入。
//
// 把快照放在调用方而不是这里，是为了让 Login 那条路径只快照一次：
// 它需要先用 cfg 里的凭证与 vcenter 构造 Credentials，再进到这里用
// cfg 里的 timeout 与采样参数。两次快照之间发生重载就会混用两份配置。
func (vm *VMware) loginWithCredentials(ctx context.Context, creds Credentials,
	logger *slog.Logger, cfg settings) (*collector.Scrape, func(), error) {
	noop := func() {}

	if creds.Target == "" {
		return nil, noop, fmt.Errorf("target is required")
	}

	if creds.Username == "" || creds.Password == "" {
		return nil, noop, fmt.Errorf("username and password are required")
	}

	if creds.Schema == "" {
		creds.Schema = cfg.schema
	}

	if logger == nil {
		logger = slog.Default()
	}

	// 连接层再次校验 target，与 /probe 的白名单校验共用同一个解析器：
	// 这里既覆盖 /probe（纵深防御），也覆盖 /metrics 的 -vmware.vcenter。
	// 拒绝 userinfo/path/query/fragment 后，再由下面的 url.UserPassword 以
	// 受控方式附加 Basic 凭证，避免调用方把凭证或混淆主机藏进 target。
	endpoint, err := target.Parse(creds.Target)
	if err != nil {
		return nil, noop, fmt.Errorf("invalid vcenter target: %w", err)
	}
	creds.Target = endpoint.Authority

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
	scrapeCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.timeout)*time.Second)

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

	// 拨号侧 IP 复核（S-06）。govmomi 的 config 回调在它 NewClient 之后、发出
	// 任何请求之前调用一次，这里把内部 transport 的明文/TLS 两条拨号路径都换成
	// 经 safedial 校验的版本 —— 在 connect 前按「本次实际解析到的 IP」判定，
	// 堵住 DNS rebinding 与「字符串白名单放行但 IP 落在元数据/内网」的残留面。
	// 必须在 transport 层而不是预先 net.LookupIP：那两者之间存在 TOCTOU 窗口。
	dialPolicy := safedial.PolicyForFlag(cfg.denyPrivate)
	soapConfig := func(sc *soap.Client) error {
		safedial.GuardTransport(sc.DefaultTransport(), dialPolicy)
		return nil
	}

	if err := session.Login(scrapeCtx, client, soapConfig); err != nil {
		// 登录本身失败时没有服务端会话可登出，只释放本地资源。
		cancel()
		return nil, noop, fmt.Errorf("login err: %w", err)
	}

	perf := performance.NewManager(client)

	// 计数器元数据按 target+About 版本/build 缓存（P-03），仅 /metrics 注入了
	// counterCache。Version/Build 随 ServiceContent 在登录时已返回，取键零额外
	// 往返；升级换 build 后第一个请求天然 miss，最多一个 TTL 生效。/probe 与
	// NewAPI() 的 cache 为 nil，Get 直接旁路、保持每请求实时。
	cacheKey := counterCacheKey(
		creds.Target,
		client.ServiceContent.About.Version,
		client.ServiceContent.About.Build,
	)

	counters, err := vm.counterCache.Get(scrapeCtx, cacheKey, perf, cfg.counterCacheTTL, time.Now())
	if err != nil {
		// 这里已经登录成功了，必须走完整 cleanup 释放服务端会话。
		cleanup()
		return nil, noop, fmt.Errorf("perfman counters err: %s", err)
	}

	// granularity 已在启动时由 ValidateFlags 保证 > 0，这里不会除零。
	// 同时保证至少取 1 个采样点，避免 interval 略小于 granularity 时算出 0。
	samples := cfg.interval / cfg.granularity
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
		Interval:   int32(cfg.interval),
		Samples:    int32(samples),

		// 分块大小是 vmware.* 侧配置，随会话一起注入；缓存由
		// CollectorSet 在登录后注入（只有它分得清 /metrics 与 /probe）。
		PerfChunkSize: cfg.chunkSize,
	}

	// 采样间隔协商缓存（仅 /metrics：NewAPI() 的 providerCache 为 nil）。
	// 闭包按 entity.Type 补全缓存键：host/vm/datastore 在同一次登录里对同一
	// 目标+版本查询，能力是目标级的，与具体实体无关。闭包只在 collector 协商
	// 间隔时调用，那时 scrapeCtx 仍有效；cache 为 nil 或 TTL<=0 时 Get 内部
	// 旁路，直接发 SOAP。/probe 路径这里整段不安装，s.ProviderSummary 保持 nil。
	if vm.providerCache != nil {
		providerTTL := cfg.providerCacheTTL
		about := client.ServiceContent.About
		s.ProviderSummary = func(ctx context.Context, entity types.ManagedObjectReference) (*types.PerfProviderSummary, error) {
			key := providerCacheKey(creds.Target, about.Version, about.Build, entity.Type)
			return vm.providerCache.Get(ctx, key, perf, entity, providerTTL, time.Now())
		}
	}

	// 不记录 username：probe 端点的凭证来自请求参数或 Basic Auth，
	// 写进日志会让凭证随日志流出到集中式日志系统。
	logger.Info("logged in to vCenter", "target", creds.Target, "target_type", s.TargetType)

	return s, cleanup, nil
}
