package underlay

import (
	"strings"
	"testing"
)

func TestDefaultSettingsValidate(t *testing.T) {
	s := DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Fatalf("默认设置不合格: %v", err)
	}
	if s.VPNPorts.Overlaps(ephemeralPorts) || s.TransitPorts.Overlaps(ephemeralPorts) {
		t.Fatalf("默认池落在内核临时端口段里: vpn=%s transit=%s", s.VPNPorts, s.TransitPorts)
	}
	if s.VPNPorts.Overlaps(s.TransitPorts) {
		t.Fatalf("默认的 VPN 池与中转池重叠")
	}
}

func TestSettingsRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Settings)
		want string
	}{
		{"VPN 池跨内核临时端口段", func(s *Settings) { s.VPNPorts = PortRange{30000, 40000} }, "临时端口"},
		{"中转池跨内核临时端口段", func(s *Settings) { s.TransitPorts = PortRange{32000, 33000} }, "临时端口"},
		{"端口池进了特权段", func(s *Settings) { s.VPNPorts = PortRange{80, 200} }, "1024"},
		{"两个池重叠", func(s *Settings) { s.TransitPorts = PortRange{25000, 25010} }, "重叠"},
		{"排除段在池外", func(s *Settings) { s.VPNAvoid = []PortRange{{Min: 40000, Max: 40010}} }, "不在 VPN 端口池"},
		{"overlay 不是 CIDR", func(s *Settings) { s.OverlayCIDR = "10.250.0.0" }, "不是合法 CIDR"},
		{"overlay 是 IPv6", func(s *Settings) { s.OverlayCIDR = "fd00::/64" }, "IPv4"},
		{"overlay 太大", func(s *Settings) { s.OverlayCIDR = "10.0.0.0/8" }, "太大"},
	}
	for _, tc := range cases {
		s := DefaultSettings()
		tc.mod(&s)
		err := s.Validate()
		if err == nil {
			t.Fatalf("%s: 竟然通过了", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 报错 %q，应含 %q", tc.name, err, tc.want)
		}
	}
}

func TestNormalizeKeepsSubmittedFields(t *testing.T) {
	in := Settings{VPNPorts: PortRange{Min: 21000, Max: 21999}}
	got := in.Normalize()
	if got.VPNPorts != (PortRange{21000, 21999}) {
		t.Fatalf("提交的端口池被改掉了: %s", got.VPNPorts)
	}
	if got.TransitPorts != DefaultSettings().TransitPorts {
		t.Fatalf("没提交的中转池没回落到默认值: %s", got.TransitPorts)
	}
	if got.ProbeIntervalMS != 300 {
		t.Fatalf("探针间隔默认值应是 300，实际 %d", got.ProbeIntervalMS)
	}
}

// The pool is a list of /32 endpoints, not a subnet somebody puts hosts into,
// so the first entry is the prefix address itself: the first tunnel pair is
// .0/.1, then .2/.3.
func TestOverlayHostsHandsOutTheWholePrefix(t *testing.T) {
	s := DefaultSettings()
	s.OverlayCIDR = "10.9.0.0/29"
	hosts, err := s.OverlayHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 8 {
		t.Fatalf("/29 应整段发出 8 个 /32，实际 %d", len(hosts))
	}
	if got := hosts[0].String(); got != "10.9.0.0" {
		t.Fatalf("第一个可发的地址应是 10.9.0.0，实际 %s", got)
	}
	if got := hosts[len(hosts)-1].String(); got != "10.9.0.7" {
		t.Fatalf("最后一个可发的地址应是 10.9.0.7，实际 %s", got)
	}
	n, err := s.OverlaySize()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(hosts) {
		t.Fatalf("OverlaySize=%d 和地址清单 %d 条不一致", n, len(hosts))
	}
}
