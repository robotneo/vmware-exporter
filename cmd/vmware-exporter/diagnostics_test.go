package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterDiagnosticsDisabledByDefault(t *testing.T) {
	mux := http.NewServeMux()

	registerDiagnostics(mux, false)

	// 关闭时端点必须不存在：请求 /debug/pprof/ 应走 mux 的未匹配路径，
	// 返回 404（ServeMux 对未注册路径的默认行为）。
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled pprof endpoint = %d, want 404", rec.Code)
	}
}

func TestRegisterDiagnosticsEnabledServesIndex(t *testing.T) {
	mux := http.NewServeMux()

	registerDiagnostics(mux, true)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("enabled pprof index = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	// pprof 索引页会列出各类 profile，至少应包含 goroutine 与 heap 条目。
	if !strings.Contains(body, "goroutine") || !strings.Contains(body, "heap") {
		t.Fatalf("pprof index page missing expected profiles:\n%s", body)
	}
}

func TestRegisterDiagnosticsServesAuxiliaryEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	registerDiagnostics(mux, true)

	for _, path := range []string{
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("path %s = %d, want 200", path, rec.Code)
		}
	}
}
