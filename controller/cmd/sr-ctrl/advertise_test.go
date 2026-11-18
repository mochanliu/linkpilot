package main

import (
	"net"
	"strings"
	"testing"
)

// 设备与 RR 拿到的地址必须能拨。-listen 是绑定，不是可达地址：一个 0.0.0.0
// 进了候选表，客户端就会去连自己。
func TestAdvertiseAddr(t *testing.T) {
	cases := []struct {
		name     string
		listen   string
		sans     []string
		explicit string
		want     string
	}{
		{"显式优先", "0.0.0.0:8444", []string{"10.0.0.9"}, "192.168.1.100", "192.168.1.100:8444"},
		{"显式带端口", "0.0.0.0:8444", nil, "ctrl.example:9443", "ctrl.example:9443"},
		{"取 tls-san 第一项", "0.0.0.0:8444", []string{"192.168.1.100", "203.0.113.10"}, "", "192.168.1.100:8444"},
		{"跳过 localhost", "0.0.0.0:8444", []string{"localhost", "10.0.0.1"}, "", "10.0.0.1:8444"},
		{"listen 本身可拨", "10.0.0.5:9443", nil, "", "10.0.0.5:9443"},
		{"IPv6 SAN", "[::]:8444", []string{"2001:db8::1"}, "", "[2001:db8::1]:8444"},
		{"通配 listen 不带端口", "0.0.0.0", nil, "", ""}, // 期望由下面的探针断言另行检查
	}
	for _, c := range cases {
		got := advertiseAddr(c.listen, c.sans, c.explicit)
		if c.want == "" {
			// 通配 listen 又没有任何提示时只能靠默认路由探针：断言它不是通配值，
			// 并且端口是控制面端口。探针在隔离网段可能拿不到地址，那种情况允许
			// 落回 listen 原值，但绝不能是 0.0.0.0:8444 之外的空值。
			if got == "" {
				t.Fatalf("%s: 得到空地址", c.name)
			}
			h, p, err := net.SplitHostPort(got)
			if err != nil {
				t.Fatalf("%s: %q 不是 host:port: %v", c.name, got, err)
			}
			if h == "0.0.0.0" || h == "::" {
				t.Fatalf("%s: 仍是通配地址 %q", c.name, got)
			}
			if p != "8444" {
				t.Fatalf("%s: 端口应是 8444，得到 %q", c.name, p)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: advertiseAddr(%q,%v,%q) = %q，应是 %q",
				c.name, c.listen, c.sans, c.explicit, got, c.want)
		}
	}
}

// 单机最坏的情况是 -listen 0.0.0.0 且没给任何 SAN：以前会把 0.0.0.0:8444 当
// 控制器地址下发，设备的地址簿里于是有一条"连自己"的候选。
func TestAdvertiseNeverReturnsWildcard(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8444", ":8444", "[::]:8444", "0.0.0.0:9000"} {
		got := advertiseAddr(listen, []string{"localhost", "127.0.0.1"}, "")
		host, port, err := net.SplitHostPort(got)
		if err != nil {
			t.Fatalf("%q -> %q: %v", listen, got, err)
		}
		if host == "0.0.0.0" || host == "::" || strings.HasPrefix(host, "127.") {
			t.Fatalf("%q -> %q：宣告地址不能是通配或回环", listen, got)
		}
		if _, wantPort, _ := net.SplitHostPort(listen); wantPort != "" && port != wantPort {
			t.Fatalf("%q -> %q：端口应沿用监听端口 %s", listen, got, wantPort)
		}
	}
}

func TestHostPortHelpers(t *testing.T) {
	if got := hostOf("[2001:db8::1]:8444"); got != "2001:db8::1" {
		t.Errorf("hostOf v6 = %q", got)
	}
	if got := hostOf("192.168.1.1"); got != "192.168.1.1" {
		t.Errorf("hostOf 无端口 = %q", got)
	}
	if got := portOf("0.0.0.0:9443"); got != "9443" {
		t.Errorf("portOf = %q", got)
	}
	if got := portOf("0.0.0.0"); got != "8444" {
		t.Errorf("portOf 缺省 = %q", got)
	}
	if got := hostPort("2001:db8::1", "8444"); got != "[2001:db8::1]:8444" {
		t.Errorf("hostPort v6 = %q", got)
	}
}
