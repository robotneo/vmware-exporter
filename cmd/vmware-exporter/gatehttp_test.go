package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withScrapeInflightFlag 在测试期间临时设置 -web.max-scrape-inflight，
// 结束后还原。包级 flag 在测试间共享，不还原会让顺序靠后的用例随机失败。
func withScrapeInflightFlag(t *testing.T, v int) {
	t.Helper()
	old := *maxScrapeInflight
	*maxScrapeInflight = v
	t.Cleanup(func() { *maxScrapeInflight = old })
}

func withAllowedTargetsFlag(t *testing.T, v string) {
	t.Helper()
	old := *probeAllowedTargets
	*probeAllowedTargets = v
	t.Cleanup(func() { *probeAllowedTargets = old })
}

func TestProbeRejectsTargetOutsideAllowlist(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withAllowedTargetsFlag(t, ".example.com,10.0.0.0/8")

	// 凭证都不必给：白名单判定在凭证校验之前，403 必须先于"缺凭证"的 400。
	req := httptest.NewRequest(http.MethodGet, "/probe?target=evil.internal:443", nil)
	rec := httptest.NewRecorder()
	probeHandler(rec, req, logger)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d for a non-allowlisted target", rec.Code, http.StatusForbidden)
	}
}

func TestProbeAllowsTargetInsideAllowlist(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withAllowedTargetsFlag(t, ".example.com")

	// 命中白名单后应越过 403，进入后续流程（这里没给凭证，所以是 400，而不是
	// 403 —— 这个 400 恰好证明白名单放行了）。
	req := httptest.NewRequest(http.MethodGet, "/probe?target=vc.example.com:443", nil)
	rec := httptest.NewRecorder()
	probeHandler(rec, req, logger)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("allowlisted target must not be rejected with 403, got body: %s", rec.Body.String())
	}
}

func TestProbeReturns503WhenScrapeGateFull(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withAllowedTargetsFlag(t, "") // 显式保证默认放开，避免被其它测试残留的 flag 影响
	withScrapeInflightFlag(t, 2)

	// 预先占满进程级闸。
	if !scrapeInflightGate.tryAcquire(2) {
		t.Fatal("first pre-fill acquire failed")
	}
	if !scrapeInflightGate.tryAcquire(2) {
		t.Fatal("second pre-fill acquire failed")
	}
	t.Cleanup(func() {
		scrapeInflightGate.release()
		scrapeInflightGate.release()
	})

	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=vc.example.com:443&username=u&password=p", nil)
	rec := httptest.NewRecorder()
	probeHandler(rec, req, logger)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d when the scrape gate is full", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestProbeProceedsWhenGateHasCapacity 是上面 503 测试的反向护栏：一个"永远
// 503"的实现也能通过拒绝用例，但闸有余量时请求必须放行进抓取（这里以登录
// 失败产出的非 503 响应为信号，不依赖真实 vCenter）。
func TestProbeProceedsWhenGateHasCapacity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withAllowedTargetsFlag(t, "")
	withScrapeInflightFlag(t, 4)

	before := scrapeInflightGate.current()

	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&username=u&password=p", nil)
	rec := httptest.NewRecorder()
	probeHandler(rec, req, logger)

	if rec.Code == http.StatusServiceUnavailable {
		t.Fatal("request with available capacity must not get 503")
	}
	// handler 返回后（defer 已执行）闸应回到调用前的占用数，证明 release 配对。
	if got := scrapeInflightGate.current(); got != before {
		t.Fatalf("gate active = %d after handler returned, want %d (unbalanced acquire/release)", got, before)
	}
}

func TestMetricsReturns503WhenScrapeGateFull(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withScrapeInflightFlag(t, 1)

	// /metrics 的闸在真正抓取分支上；disable.target 时走轻量 promhttp 分支
	// 不应受闸影响。这里占满唯一名额，连真实 vCenter 的分支应直接 503
	// （在 NewCollectorSet/登录之前返回，故无需 vcsim）。
	if !scrapeInflightGate.tryAcquire(1) {
		t.Fatal("could not pre-fill the gate")
	}
	t.Cleanup(func() { scrapeInflightGate.release() })

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metricsHandler(logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d when the scrape gate is full", rec.Code, http.StatusServiceUnavailable)
	}
}
