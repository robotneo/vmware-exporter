package vmware

import (
	"context"
	"errors"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/safedial"

	"github.com/vmware/govmomi/simulator"
)

// TestDialGuardRejectsLoopbackUnderStrictPolicy 是 S-06 的端到端验收：它不只测
// safedial 包本身，而是证明护栏确实被装进了 govmomi 的登录链路，并且对 vCenter
// 实际使用的 HTTPS（而非仅明文 http）生效。
//
// vcsim 的 NewServer() 是一个监听 127.0.0.1 的 TLS 服务。govmomi 对 https 自设
// DialTLSContext（内部 tls.Dial，绕过 transport.DialContext）；若我们只覆盖
// DialContext，这个测试会登录成功、护栏形同虚设。Strict 策略连环比都拦，因此
// 登录必须在 connect 前失败，错误文本带有 safedial 的 blocked 标记。
func TestDialGuardRejectsLoopbackUnderStrictPolicy(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60
	*vmwDenyPrivate = true // 启用 Strict：环回也在拦截范围
	t.Cleanup(func() { *vmwDenyPrivate = false })

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme, // vcsim 默认 https
		Insecure: true,
	}

	s, cleanup, err := NewAPI().LoginWithCredentials(context.Background(), creds, discardLogger())
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected login to 127.0.0.1 vcsim to be blocked under strict dial policy, but it succeeded")
	}
	if s != nil {
		t.Fatalf("login returned a non-nil Scrape despite error: %v", err)
	}
	if !errors.Is(err, safedial.ErrBlockedAddress) {
		t.Fatalf("expected a safedial blocked-address error, got: %v", err)
	}
}

// TestDialGuardAllowsLoopbackByDefault 钉住默认策略的零误伤承诺：vCenter 内网/
// vcsim/127.0.0.1 sidecar 代理在默认配置下必须能正常登录（只拦链路本地/未指定）。
func TestDialGuardAllowsLoopbackByDefault(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60
	// *vmwDenyPrivate 保持默认 false。

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme,
		Insecure: true,
	}

	s, cleanup, err := NewAPI().LoginWithCredentials(context.Background(), creds, discardLogger())
	if err != nil {
		t.Fatalf("default dial policy must allow a loopback vcsim login, got: %v", err)
	}
	if s == nil {
		t.Fatal("login returned nil Scrape without error")
	}
	cleanup()
}
