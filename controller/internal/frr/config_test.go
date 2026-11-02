package frr

import (
	"net/netip"
	"testing"
)

func TestCompileImportRules(t *testing.T) {
	raw := []byte(`[
		{"vpn":0,"sources":["bgp","connected","bgp"],"prefixes":[
			{"prefix":"0.0.0.0/0"},
			{"prefix":"10.0.0.0/8","ge":16,"le":24}
		]},
		{"vpn":100,"sources":["ospf"],"prefixes":[{"prefix":"172.16.0.0/12"}]}
	]`)
	rules, err := CompileImportRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].VPN != 0 || rules[1].VPN != 100 {
		t.Fatalf("rules = %+v", rules)
	}
	// 重复来源收敛成一条。
	if got := rules[0].Sources; len(got) != 2 || got[0] != RouteBGP || got[1] != RouteConnected {
		t.Fatalf("sources = %v, want [bgp connected]", got)
	}
	if len(rules[0].Allow) != 2 {
		t.Fatalf("allow = %+v", rules[0].Allow)
	}
	e := rules[0].Allow[1]
	if e.Prefix != netip.MustParsePrefix("10.0.0.0/8") || e.GE != 16 || e.LE != 24 {
		t.Fatalf("allow[1] = %+v", e)
	}
}

func TestCompileImportRulesRejects(t *testing.T) {
	cases := map[string]string{
		"不是数组":     `{"vpn":0}`,
		"未知来源":     `[{"vpn":0,"sources":["rip"],"prefixes":[{"prefix":"10.0.0.0/8"}]}]`,
		"overlay":  `[{"vpn":250,"sources":["bgp"],"prefixes":[]}]`,
		"VPN 重复":   `[{"vpn":1,"sources":["bgp"]},{"vpn":1,"sources":["bgp"]}]`,
		"VPN 超界":   `[{"vpn":256,"sources":["bgp"]}]`,
		"前缀无效":     `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"nope"}]}]`,
		"IPv6":     `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"2001:db8::/32"}]}]`,
		"未规范前缀":    `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"10.1.2.3/24"}]}]`,
		"ge 小于前缀":  `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"10.0.0.0/16","ge":8}]}]`,
		"ge 大于 le": `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"10.0.0.0/8","ge":24,"le":16}]}]`,
		"le 超过 32": `[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"10.0.0.0/8","le":33}]}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileImportRules([]byte(raw)); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestCompileImportRulesEmpty(t *testing.T) {
	rules, err := CompileImportRules([]byte("  "))
	if err != nil || rules != nil {
		t.Fatalf("空配置 = %v, %v; want nil, nil", rules, err)
	}
}

func TestCompileExportVRFs(t *testing.T) {
	got, err := CompileExportVRFs([]byte(`[100,0,1]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{0, 1, 100}
	if len(got) != len(want) {
		t.Fatalf("vrfs = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("vrfs = %v, want 排序后的 %v", got, want)
		}
	}
}

func TestCompileExportVRFsRejects(t *testing.T) {
	for _, raw := range []string{
		`{"vpn":0}`,
		`[256]`,
		`[250]`,
		`[253]`,
		`[254]`,
		`[255]`,
		`[1,1]`,
	} {
		if _, err := CompileExportVRFs([]byte(raw)); err == nil {
			t.Errorf("%s: 期望报错", raw)
		}
	}
}

// 只要两个值的内容一样，空白的差别不能让 hash 不同：sr-apply 与 sr-frr 拿到
// 的是不同序列化器的输出，靠这一点才对得上。
func TestConfigHashWhitespaceInsensitive(t *testing.T) {
	a, err := ConfigHash([]byte(`[0,1]`), []byte(`[{"vpn":0,"sources":["bgp"],"prefixes":[{"prefix":"10.0.0.0/8"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ConfigHash([]byte(" [ 0 , 1 ] "), []byte("[\n {\"vpn\":0,\"sources\":[\"bgp\"],\n\"prefixes\":[{\"prefix\":\"10.0.0.0/8\"}]}\n]"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("hash 不一致:\n%s\n%s", a, b)
	}
	if h, err := ConfigHash(nil, nil); err != nil || h == "" {
		t.Fatalf("空配置也要有 hash: %q, %v", h, err)
	}
}
