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
	vmwInterval   = flag.Int("vmware.interval", 20, "How often data will be collected. Default is every 20s.")
	vmGranularity = flag.Int("vmware.granularity", 20, "The frequency of the sampled data. Default is 20s")
)

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

// Logout 清理资源，通过 context cancel 自动清理连接
func (vm *VMware) Logout(loginData map[string]interface{}, logger *slog.Logger) error {
	target := "unknown"
	if t, ok := loginData["target"].(string); ok {
		target = t
	}

	// 清理 context - 这会自动关闭连接和清理资源
	if cancel, ok := loginData["cancel"].(context.CancelFunc); ok {
		cancel()
		logger.Debug("logged out and cleaned up resources for vCenter", "target", target)
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
		Timeout:   time.Duration(*vmwInterval-2) * time.Second,
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*vmwInterval-2)*time.Second)

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
	loginData["interval"] = int32(*vmwInterval)
	loginData["samples"] = int32(*vmwInterval / *vmGranularity)

	return nil
}
