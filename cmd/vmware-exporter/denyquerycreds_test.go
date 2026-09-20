package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// withDenyQueryCredentials 在测试期间把 -probe.deny-query-credentials 置为给定
// 值并在结束后还原。flag 是包级变量，必须串行保护（本文件各用例不复用 flag 时
// 也显式还原，避免与其它测试文件里的裸读相互污染）。
func withDenyQueryCredentials(t *testing.T, v bool) {
	t.Helper()
	old := *probeDenyQueryCredentials
	*probeDenyQueryCredentials = v
	t.Cleanup(func() { *probeDenyQueryCredentials = old })
}

func discardProbeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestProbeDenyQueryCredentialsRejectsGet 是 S-07 的核心断言：开关开启后，凭证
// 出现在 URL 查询串里必须 400，且根本不该走到登录。
func TestProbeDenyQueryCredentialsRejectsGet(t *testing.T) {
	withDenyQueryCredentials(t, true)

	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=vcenter.example.com&username=user&password=pass", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, discardProbeLogger())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for credentials in query string", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "deny-query-credentials") {
		t.Fatalf("body = %q, want it to reference the deny-query-credentials flag", rec.Body.String())
	}
}

// 只有 username 或只有 password 落在查询串也应被拒 —— 不能因为"另一个字段在
// Basic Auth 里补齐"就放行半个凭证进 URL。
func TestProbeDenyQueryCredentialsRejectsPartialQueryCredential(t *testing.T) {
	withDenyQueryCredentials(t, true)

	for _, tc := range []struct {
		name string
		q    string
	}{
		{"username only", "/probe?target=vcenter.example.com&username=user"},
		{"password only", "/probe?target=vcenter.example.com&password=pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.q, nil)
			rec := httptest.NewRecorder()

			probeHandler(rec, req, discardProbeLogger())

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for partial credential in query (%s)", rec.Code, tc.name)
			}
		})
	}
}

// POST 把凭证放进【表单体】（URL 上不带）是开关推荐的用法之一，必须放行并真正
// 走到登录。用 127.0.0.1:1 让连接失败，靠 vmware_up 0 证明请求通过了凭证闸门。
func TestProbeDenyQueryCredentialsAllowsPostBody(t *testing.T) {
	withDenyQueryCredentials(t, true)

	form := url.Values{
		"target":   {"127.0.0.1:1"},
		"username": {"user"},
		"password": {"pass"},
		"schema":   {"http"},
	}
	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	probeHandler(rec, req, discardProbeLogger())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for POST-body credentials; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("login was never attempted, so POST-body credentials were wrongly rejected; body=%q", rec.Body.String())
	}
}

// Basic Auth 是另一个推荐通道，URL 查询串只有 target，必须放行。
func TestProbeDenyQueryCredentialsAllowsBasicAuth(t *testing.T) {
	withDenyQueryCredentials(t, true)

	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&schema=http", nil)
	req.SetBasicAuth("user", "pass")
	rec := httptest.NewRecorder()

	probeHandler(rec, req, discardProbeLogger())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for Basic Auth credentials; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("login was never attempted, so Basic Auth credentials were wrongly rejected; body=%q", rec.Body.String())
	}
}

// 防绕过：即便方法是 POST，只要凭证写在 URL 查询串里就必须被拒 —— 判定只看
// 查询串、不看方法，body 里放什么都救不了它。
func TestProbeDenyQueryCredentialsRejectsPostWithQueryCredentials(t *testing.T) {
	withDenyQueryCredentials(t, true)

	form := url.Values{"collect[]": {"vm"}}
	req := httptest.NewRequest(http.MethodPost,
		"/probe?target=127.0.0.1:1&username=user&password=pass&schema=http",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	probeHandler(rec, req, discardProbeLogger())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 even on POST when credentials are in the query string", rec.Code)
	}
}

// 默认（false）必须保持向后兼容：GET 查询串带凭证照旧工作。这条钉住开关是
// opt-in，避免把默认行为变成破坏性变更。
func TestProbeAllowsQueryCredentialsByDefault(t *testing.T) {
	// 不显式置位，依赖 flag 默认 false；仍还原以防其它用例污染。
	withDenyQueryCredentials(t, false)

	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&username=user&password=pass&schema=http&collect[]=vm", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, discardProbeLogger())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 by default for legacy GET credentials; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("login was never attempted under the default-compatible path; body=%q", rec.Body.String())
	}
}
