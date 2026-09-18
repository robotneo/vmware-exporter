package target

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		host      string
		authority string
	}{
		{"host port", "vc.example.com:443", "vc.example.com", "vc.example.com:443"},
		{"host no port", "vc.example.com", "vc.example.com", "vc.example.com"},
		{"ipv4 port", "10.0.0.5:443", "10.0.0.5", "10.0.0.5:443"},
		{"ipv4 no port", "127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"ipv6 port", "[2001:db8::1]:443", "2001:db8::1", "[2001:db8::1]:443"},
		{"ipv6 no port", "[::1]", "::1", "[::1]"},
		{"trim and lowercase", "  VC.Corp:443  ", "vc.corp", "vc.corp:443"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.in, err)
			}
			if got.Host != tc.host || got.Authority != tc.authority {
				t.Fatalf("Parse(%q) = %+v, want host=%q authority=%q", tc.in, got, tc.host, tc.authority)
			}
		})
	}
}

func TestParseRejectsAmbiguousTargets(t *testing.T) {
	// 这些形态曾导致白名单按 @ 前主机匹配、连接层按 @ 后主机拨号，
	// 或允许 target 携带 path/query/fragment。全部必须拒绝。
	invalid := []string{
		"",
		"   ",
		"allowed.example.com:443@169.254.169.254",
		"allowed.example.com@169.254.169.254",
		"user:pass@vc.example.com:443",
		"allowed.example.com/path",
		"allowed.example.com/sdk",
		"allowed.example.com?x=1",
		"allowed.example.com#fragment",
		"allowed.example.com#@169.254.169.254",
		"vc.example.com:notaport",
		"vc.example.com:443:8443",
	}

	for _, in := range invalid {
		t.Run(in, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) succeeded, want rejection", in)
			}
		})
	}
}
