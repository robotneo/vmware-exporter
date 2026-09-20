package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	hits := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
	})

	srv := httptest.NewServer(securityHeaders(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if hits != 1 {
		t.Fatalf("inner handler invoked %d times, want 1", hits)
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d (middleware must not alter the status)", resp.StatusCode, http.StatusTeapot)
	}
	if got := resp.Header.Get(headerContentTypeOptions); got != "nosniff" {
		t.Errorf("%s = %q, want %q", headerContentTypeOptions, got, "nosniff")
	}
	if got := resp.Header.Get(headerReferrerPolicy); got != "no-referrer" {
		t.Errorf("%s = %q, want %q", headerReferrerPolicy, got, "no-referrer")
	}
}

func TestSecurityHeadersPresentWhenBodyWritten(t *testing.T) {
	// 常见路径：业务 handler 只写 body、不碰这两个头，中间件兜底加上，且不
	// 影响默认 200 与 body 内容。
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	rec := httptest.NewRecorder()
	securityHeaders(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
	if got := rec.Header().Get(headerContentTypeOptions); got != "nosniff" {
		t.Errorf("%s = %q, want nosniff", headerContentTypeOptions, got)
	}
	if got := rec.Header().Get(headerReferrerPolicy); got != "no-referrer" {
		t.Errorf("%s = %q, want no-referrer", headerReferrerPolicy, got)
	}
}

func TestListenExposedWithoutAuth(t *testing.T) {
	cases := []struct {
		name      string
		listen    string
		webConfig string
		wantWarn  bool
	}{
		{"wildcard ipv4", ":9169", "", true},
		{"wildcard zero ipv4", "0.0.0.0:9169", "", true},
		{"wildcard ipv6", "[::]:9169", "", true},
		{"non-loopback ip", "10.0.0.5:9169", "", true},
		{"hostname", "exporter.internal.example.com:9169", "", true},
		{"loopback ipv4", "127.0.0.1:9169", "", false},
		{"loopback 127/8", "127.99.0.1:9169", "", false},
		{"loopback ipv6", "[::1]:9169", "", false},
		{"localhost by name", "localhost:9169", "", false},
		{"wildcard but web.config present", ":9169", "/etc/web.yml", false},
		{"public ip but web.config present", "203.0.113.9:9169", "/etc/web.yml", false},
		{"blank web.config treated as absent", ":9169", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listenExposedWithoutAuth(tc.listen, tc.webConfig)
			if got != tc.wantWarn {
				t.Errorf("listenExposedWithoutAuth(%q, %q) = %v, want %v",
					tc.listen, tc.webConfig, got, tc.wantWarn)
			}
		})
	}
}
