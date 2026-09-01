package vmware

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"

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

func init() {
	collector.RegisterAPI(NewAPI())
}

func NewAPI() *VMware {
	return &VMware{}
}

func Load(logger *slog.Logger) {
	logger.Info("Loading VMware vSphere API")
}

// Login 使用全局 flag 配置的默认凭证登录（默认模式）
func (vm *VMware) Login(target string, logger *slog.Logger) (map[string]interface{}, error) {
	loginData := make(map[string]interface{}, 0)

	// 如果没有指定 target，使用全局配置
	if target == "" {
		target = *vCenter
	}

	// 检查是否配置了默认凭证
	if *vmwUser == "" || *vmwPasswd == "" {
		return nil, fmt.Errorf("default credentials not configured. Please set -vmware.username and -vmware.password flags")
	}

	if target == "" {
		return nil, fmt.Errorf("target not specified and -vmware.vcenter flag not set")
	}

	loginData["target"] = target

	// 使用全局凭证
	creds := Credentials{
		Username: *vmwUser,
		Password: *vmwPasswd,
		Target:   target,
		Schema:   *vmwSchema,
		Insecure: *vmwTLS,
	}

	loginData["credentials"] = creds

	// 登录
	if err := govmomiLoginWithCreds(loginData, creds); err != nil {
		return loginData, err
	}

	logger.Info("logged in to vCenter using default credentials", "target", target)

	return loginData, nil
}

// LoginWithCredentials 使用指定凭证登录到指定 target（Probe 模式）
func (vm *VMware) LoginWithCredentials(creds Credentials, logger *slog.Logger) (map[string]interface{}, error) {
	loginData := make(map[string]interface{}, 0)

	if creds.Target == "" {
		return nil, fmt.Errorf("target is required")
	}

	if creds.Username == "" || creds.Password == "" {
		return nil, fmt.Errorf("username and password are required")
	}

	// 使用传入的凭证，如果未指定则使用默认值
	if creds.Schema == "" {
		creds.Schema = *vmwSchema
	}

	loginData["target"] = creds.Target
	loginData["credentials"] = creds

	// 使用提供的凭证登录
	if err := govmomiLoginWithCreds(loginData, creds); err != nil {
		return nil, err
	}

	logger.Info("logged in to vCenter using probe credentials", "target", creds.Target, "username", creds.Username)

	return loginData, nil
}

// Logout 先向服务端发起 SOAP 登出释放会话，然后取消 context 释放本地资源。
//
// 顺序至关重要，不要调换：cancel() 之后抓取用的 ctx 已失效，任何后续 SOAP
// 调用都会立刻失败，服务端会话就会一直挂到自然超时（vCenter 默认 30 分钟），
// 在高频抓取下会迅速堆积到会话上限。
//
// 这里所有取值都用带 ok 的类型断言：登录中途失败时 loginData 里的键是不全的，
// 裸断言会 panic。
func (vm *VMware) Logout(loginData map[string]interface{}, logger *slog.Logger) error {
	target := "unknown"
	if t, ok := loginData["target"].(string); ok {
		target = t
	}

	// 第一步：发起 SOAP 登出。用独立的短超时 context，不复用抓取的 ctx
	// （它此刻可能已经超时）。
	session, hasSession := loginData["session"].(*cache.Session)
	client, hasClient := loginData["client"].(*vim25.Client)

	if hasSession && hasClient {
		logoutCtx, logoutCancel := context.WithTimeout(context.Background(), logoutTimeout)
		if err := session.Logout(logoutCtx, client); err != nil {
			// 登出失败不阻断流程：本地资源仍然要释放，否则会同时泄漏
			// 服务端会话和本地 goroutine。
			logger.Error("SOAP logout failed, server-side session may linger until it times out", "target", target, "error", err)
		} else {
			logger.Debug("SOAP logout succeeded", "target", target)
		}
		logoutCancel()
	} else {
		logger.Debug("no cached session found in loginData, skipping SOAP logout", "target", target)
	}

	// 第二步：取消 context，释放本地连接与关联 goroutine。
	if cancel, ok := loginData["cancel"].(context.CancelFunc); ok {
		cancel()
		logger.Debug("cleaned up local resources", "target", target)
	}

	return nil
}

func (vm *VMware) Get(loginData, extraConfig map[string]interface{}, logger *slog.Logger) (interface{}, error) {
	creds, ok := loginData["credentials"].(Credentials)
	if !ok {
		return nil, fmt.Errorf("credentials not found in loginData")
	}

	apiPath := extraConfig["api"]
	if apiPath == nil {
		return nil, fmt.Errorf("api path not specified")
	}

	urlStr := fmt.Sprintf("%s://%s%s", creds.Schema, loginData["target"], apiPath)

	headers := make(map[string]string)
	if h, ok := loginData["headers"].(map[string]string); ok {
		headers = h
	}

	_, _, body, err := requestWithCreds("GET", urlStr, headers, creds, false)
	if err != nil {
		return nil, err
	}

	return &body, nil
}

// requestWithCreds 使用指定凭证发送 HTTP 请求
func requestWithCreds(method, urlStr string, headers map[string]string, creds Credentials, login bool) (int, string, []byte, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: creds.Insecure}

	client := &http.Client{
		Transport: transport,
		// 与 SOAP 抓取一致，用独立的 -vmware.timeout，不从采样频率推导。
		Timeout: time.Duration(*vmwTimeout) * time.Second,
	}

	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return 0, "", nil, err
	}

	if login {
		req.SetBasicAuth(creds.Username, creds.Password)
	}

	for header := range headers {
		req.Header.Add(header, headers[header])
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}

	responseHeaders := resp.Header.Get("cookie")

	// Handle read and close errors explicitly to avoid losing late I/O failures.
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()

	if readErr != nil {
		if closeErr != nil {
			return 0, "", nil, errors.Join(readErr, closeErr)
		}
		return 0, "", nil, readErr
	}

	if closeErr != nil {
		return 0, "", nil, closeErr
	}

	return resp.StatusCode, responseHeaders, body, nil
}

// govmomiLoginWithCreds 使用指定凭证登录
func govmomiLoginWithCreds(loginData map[string]interface{}, creds Credentials) error {
	// 准备 SOAP 登录 URL
	urlx, err := soap.ParseURL(fmt.Sprintf("%s://%s%s", creds.Schema, creds.Target, vim25.Path))
	if err != nil {
		return fmt.Errorf("soap url err: %s", err)
	}

	urlx.User = url.UserPassword(creds.Username, creds.Password)

	// 抓取的整体超时用独立的 -vmware.timeout 控制，不再从采样频率推导。
	// 旧实现是 (interval-2)s，默认只有 18s，大规模环境下属性检索还没跑完就被掐断。
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*vmwTimeout)*time.Second)

	session := &cache.Session{
		URL:         urlx,
		Insecure:    creds.Insecure,
		Passthrough: true,
	}

	client := new(vim25.Client)

	err = session.Login(ctx, client, nil)
	if err != nil {
		cancel()
		return fmt.Errorf("login err: %s", err)
	}

	// Property spec Manager
	loginData["view"] = view.NewManager(client)

	// Performance Manager and performance counters
	loginData["perf"] = performance.NewManager(client)
	loginData["counters"], err = loginData["perf"].(*performance.Manager).CounterInfoByName(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("perfman counters err: %s", err)
	}

	loginData["cancel"] = cancel

	// 添加 context 和 govmomi client
	loginData["ctx"] = ctx
	loginData["client"] = client

	// session 必须存进 loginData，Logout 要靠它发 SOAP 登出。
	// Passthrough=true 时 cache.Session.Logout 才会真正调用 SessionManager.Logout。
	loginData["session"] = session

	loginData["interval"] = int32(*vmwInterval)

	// granularity 已在启动时由 ValidateFlags 保证 > 0，这里不会除零。
	// 同时保证至少取 1 个采样点，避免 interval 略小于 granularity 时算出 0。
	samples := *vmwInterval / *vmGranularity
	if samples < 1 {
		samples = 1
	}
	loginData["samples"] = int32(samples)

	return nil
}
