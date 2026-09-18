package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitRules(t *testing.T) {
	got := splitRules(" .example.com , 10.0.0.0/8 ,, vc.corp ")
	want := []string{".example.com", "10.0.0.0/8", "vc.corp"}
	if len(got) != len(want) {
		t.Fatalf("splitRules = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitRules[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if len(splitRules("  ,, ")) != 0 {
		t.Error("whitespace-only config must yield no rules (=> allowlist disabled), not a single empty rule")
	}
}

func TestTargetAllowedByRules(t *testing.T) {
	// 空规则 = 不限制，这是默认兼容契约。反向：一个"空也拒绝"的实现会立刻
	// 让所有既有 /probe 部署 403，必须有测试钉死默认放开。
	if !targetAllowedByRules("anything.local", nil) {
		t.Fatal("nil rules must allow every target (backwards-compatible default)")
	}
	if !targetAllowedByRules("anything.local", splitRules("")) {
		t.Fatal("empty rule string must allow every target")
	}

	cases := []struct {
		name  string
		rules string
		host  string
		want  bool
	}{
		// 后缀规则：.example.com 命中自身与任意子域，不命中旁域。
		{"suffix base", ".example.com", "example.com", true},
		{"suffix subdomain", ".example.com", "vc.example.com", true},
		{"suffix deep subdomain", ".example.com", "a.b.example.com", true},
		{"suffix sibling rejected", ".example.com", "example.org", false},
		{"suffix lookalike rejected", ".example.com", "notexample.com", false},

		// CIDR：网段内放行，网段外拒绝，主机名不按网段匹配。
		{"cidr inside", "10.0.0.0/8", "10.20.30.40", true},
		{"cidr boundary", "10.0.0.0/8", "10.255.255.255", true},
		{"cidr outside", "10.0.0.0/8", "192.168.1.1", false},
		{"cidr does not match hostname", "10.0.0.0/8", "10.0.0.1.example.com", false},

		// 精确匹配。
		{"exact host", "vc.corp", "vc.corp", true},
		{"exact host rejected", "vc.corp", "other.corp", false},
		{"exact ip", "10.1.2.3", "10.1.2.3", true},
		{"exact case insensitive", "VC.Corp", "vc.corp", true},

		// 多规则取并集，一个命中即可。
		{"any of multiple", "10.0.0.0/8, vc.corp", "vc.corp", true},
		{"none of multiple", "10.0.0.0/8, vc.corp", "192.168.1.1", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules := splitRules(c.rules)
			if got := targetAllowedByRules(c.host, rules); got != c.want {
				t.Errorf("targetAllowedByRules(%q, %v) = %v, want %v", c.host, rules, got, c.want)
			}
		})
	}
}

// TestProbeUserinfoBypassBlocked 是 S-01 的端到端回归：白名单配置后，
// userinfo 注入等混淆 target 必须在 handler 层先被拒成 400（畸形 target），
// 而不是靠后缀匹配放行；白名单外的正常主机仍是 403。
func TestProbeUserinfoBypassBlocked(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	withAllowedTargetsFlag(t, ".example.com,10.0.0.0/8")

	malformed := []string{
		"allowed.example.com:443@169.254.169.254",
		"allowed.example.com@169.254.169.254",
		"allowed.example.com/path",
		"allowed.example.com?x=1",
		"allowed.example.com#@169.254.169.254",
	}

	for _, raw := range malformed {
		req := httptest.NewRequest(http.MethodGet, "/probe?target="+raw, nil)
		rec := httptest.NewRecorder()
		probeHandler(rec, req, logger)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("target %q: status = %d, want %d", raw, rec.Code, http.StatusBadRequest)
		}
	}
}

// TestCheckTargetNormalizesAuthority 验证同一个 target 的大小写/空白差异不会
// 形成不同的错误计数桶，且正常 host:port 能通过白名单。
func TestCheckTargetNormalizesAuthority(t *testing.T) {
	withAllowedTargetsFlag(t, ".example.com")

	ep, ok := checkTarget("  VC.Example.com:443 ")
	if !ok {
		t.Fatal("normalized allowlisted target must pass")
	}
	if ep.Host != "vc.example.com" || ep.Authority != "vc.example.com:443" {
		t.Fatalf("endpoint = %+v, want normalized host/authority", ep)
	}

	if _, ok := checkTarget("allowed.example.com:443@evil.example.org"); ok {
		t.Fatal("userinfo target must be rejected")
	}
}
