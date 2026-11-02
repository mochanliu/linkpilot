// Tests for the VRRP half of the basic config: what normalisation does to a row,
// which rows the rule set refuses, and what the 6th bundle segment looks like on
// the wire.
//
// Each rule here earns its place by being invisible on the box: VPP's API accepts
// a virtual router hung on a port the box does not own, and a VIP outside the
// subnet it is supposed to be the gateway of. Both add without an error and then
// never answer an ARP, so the customer's first hop is broken and every table on
// the box still looks configured. The controller is the only place that can see
// both halves at once.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func vrrpValidConfig() BasicConfig {
	c := baseValidConfig()
	c.VRRP = []ConfigVRRP{{
		VPP: "host-eth2", VRID: 1, Priority: 120,
		IntervalMS: vrrpDefaultIntervalMS, Preempt: true,
		VirtualAddr: "10.79.1.1",
	}}
	return c
}

func TestValidateBasicConfigVRRP(t *testing.T) {
	cfg := vrrpValidConfig()
	checks := validateBasicConfig(&cfg)
	if n := countSeverity(checks, "error"); n != 0 {
		t.Fatalf("a legal VR row must not add errors: %+v", checks)
	}
	for _, c := range checks {
		if strings.Contains(c.Text, "VRRP") {
			t.Fatalf("legal VR row produced a VRRP note: %+v", c)
		}
	}

	cases := []struct {
		name     string
		mutate   func(*BasicConfig)
		severity string
		substr   string
	}{
		{
			name:   "vr_id 0 is not a VRRP id",
			mutate: func(c *BasicConfig) { c.VRRP[0].VRID = 0 },
			substr: "VRRP host-eth2 vr_id=0 的 vr_id 需在 1–255 之间",
		},
		{
			name:   "priority 0 means stop advertising, not a backup",
			mutate: func(c *BasicConfig) { c.VRRP[0].Priority = 0 },
			substr: "priority 需在 1–254 之间",
		},
		{
			name:   "priority 255 is reserved for the real owner",
			mutate: func(c *BasicConfig) { c.VRRP[0].Priority = 255 },
			substr: "priority 需在 1–254 之间",
		},
		{
			name:   "interval below the floor",
			mutate: func(c *BasicConfig) { c.VRRP[0].IntervalMS = 50 },
			substr: "通告间隔需在 100–60000 ms 之间",
		},
		{
			name:   "interval above the ceiling",
			mutate: func(c *BasicConfig) { c.VRRP[0].IntervalMS = 60001 },
			substr: "通告间隔需在 100–60000 ms 之间",
		},
		{
			name:   "interval that VPP would round",
			mutate: func(c *BasicConfig) { c.VRRP[0].IntervalMS = 1005 },
			substr: "只能到 10 ms 一档，1005 会被 VPP 取整",
		},
		{
			name:   "vip is not an address",
			mutate: func(c *BasicConfig) { c.VRRP[0].VirtualAddr = "10.79.1" },
			substr: "虚拟地址「10.79.1」不是合法 IPv4 地址",
		},
		{
			name:   "vip equals the port's own address",
			mutate: func(c *BasicConfig) { c.VRRP[0].VirtualAddr = "10.79.1.2" },
			substr: "虚拟地址等于客户口 host-eth2 自己的地址 10.79.1.2",
		},
		{
			name:   "vip outside the port's subnet",
			mutate: func(c *BasicConfig) { c.VRRP[0].VirtualAddr = "10.79.9.1" },
			substr: "虚拟地址 10.79.9.1 不在客户口 host-eth2 的直连网段 10.79.1.0/24 内",
		},
		{
			name:   "vip is the network address",
			mutate: func(c *BasicConfig) { c.VRRP[0].VirtualAddr = "10.79.1.0" },
			substr: "是网段 10.79.1.0/24 的网络地址",
		},
		{
			name:   "vip is the broadcast address",
			mutate: func(c *BasicConfig) { c.VRRP[0].VirtualAddr = "10.79.1.255" },
			substr: "是网段 10.79.1.0/24 的广播地址",
		},
		{
			name: "two rows with the same port and vr_id",
			mutate: func(c *BasicConfig) {
				c.VRRP = append(c.VRRP, ConfigVRRP{VPP: "host-eth2", VRID: 1, Priority: 100,
					IntervalMS: vrrpDefaultIntervalMS, VirtualAddr: "10.79.1.5"})
			},
			substr: "VRRP host-eth2 vr_id=1 重复定义",
		},
		{
			name: "one vip owned by two rows",
			mutate: func(c *BasicConfig) {
				c.VRRP = append(c.VRRP, ConfigVRRP{VPP: "host-eth3", VRID: 1, Priority: 100,
					IntervalMS: vrrpDefaultIntervalMS, VirtualAddr: "10.79.1.1"})
			},
			substr: "虚拟地址 10.79.1.1 同时属于",
		},
		{
			name: "vip already a port address in the same vpn",
			mutate: func(c *BasicConfig) {
				c.Ifaces = append(c.Ifaces, ConfigIface{VPP: "host-eth4", LCP: "lcp-x2",
					IP: "10.79.1.1", PrefixLen: 32, VPN: 0, MTU: 1500, Up: true})
			},
			substr: "已被同一 VPN 0 的客户口 host-eth4 占用",
		},
		{
			name:   "port the box has no config for",
			mutate: func(c *BasicConfig) { c.VRRP[0].VPP = "host-eth9" },
			substr: "挂的客户口 host-eth9 不在这台设备的接口列表里",
		},
		{
			name:   "no port name at all",
			mutate: func(c *BasicConfig) { c.VRRP[0].VPP = "" },
			substr: "存在没有 VPP 接口名的 VRRP 条目",
		},
		{
			name: "port is down",
			mutate: func(c *BasicConfig) {
				c.Ifaces[0].Up = false
				// host-eth2 down is not a VRRP-only change: drop the BGP neighbor
				// that the iface loop would otherwise complain about, so the only
				// note left standing is the one under test.
				c.VPNs[0].BGP = BGPConfig{}
			},
			severity: "warning", substr: "挂的客户口 host-eth2 没有置为 up",
		},
		{
			name: "port has no usable address",
			mutate: func(c *BasicConfig) {
				c.Ifaces[0].IP = ""
				c.Ifaces[0].PrefixLen = 0
				c.VPNs[0].BGP = BGPConfig{}
				c.Statics = nil
			},
			severity: "warning", substr: "host-eth2 还没有合法的地址与掩码",
		},
		{
			name: "more rows than a box may carry",
			mutate: func(c *BasicConfig) {
				c.VRRP = nil
				for i := 1; i <= cfgMaxVRRP+1; i++ {
					c.VRRP = append(c.VRRP, ConfigVRRP{VPP: "host-eth2", VRID: uint8(i),
						Priority: 100, IntervalMS: vrrpDefaultIntervalMS,
						VirtualAddr: fmt.Sprintf("10.79.1.%d", i+10)})
				}
			},
			substr: "VRRP 条目数量超过上限 64",
		},
	}

	for _, tc := range cases {
		if tc.severity == "" {
			tc.severity = "error"
		}
		t.Run(tc.name, func(t *testing.T) {
			cfg := vrrpValidConfig()
			tc.mutate(&cfg)
			got := validateBasicConfig(&cfg)
			if !hasCheck(got, tc.severity, tc.substr) {
				t.Fatalf("want %s containing %q, got %+v", tc.severity, tc.substr, got)
			}
		})
	}
}

// The rules must not turn into a blanket "one VR per box": the whole point of
// this segment is two boxes in one site, and one box may run several VRs on
// several ports. These are the shapes a tenant is allowed to save.
func TestValidateBasicConfigAcceptsLegalVRShapes(t *testing.T) {
	cases := []struct {
		name string
		rows []ConfigVRRP
	}{
		{"same vr_id on two ports", []ConfigVRRP{
			{VPP: "host-eth2", VRID: 1, Priority: 120, IntervalMS: 1000, VirtualAddr: "10.79.1.1"},
			{VPP: "host-eth3", VRID: 1, Priority: 100, IntervalMS: 1000, VirtualAddr: "10.79.2.1"},
		}},
		{"two vr_ids on one port", []ConfigVRRP{
			{VPP: "host-eth2", VRID: 1, Priority: 120, IntervalMS: 1000, VirtualAddr: "10.79.1.1"},
			{VPP: "host-eth2", VRID: 2, Priority: 100, IntervalMS: 2000, VirtualAddr: "10.79.1.10"},
		}},
		{"defaults and no optional bits", []ConfigVRRP{
			{VPP: "host-eth2", VRID: 255, Priority: 1, IntervalMS: 60000, VirtualAddr: "10.79.1.200"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfig()
			cfg.VRRP = tc.rows
			got := validateBasicConfig(&cfg)
			for _, c := range got {
				if c.Severity == "error" && strings.Contains(c.Text, "VRRP") {
					t.Fatalf("legal rows refused: %+v", got)
				}
			}
		})
	}
}

func TestNormalizeVRRPRows(t *testing.T) {
	cfg := BasicConfig{VRRP: []ConfigVRRP{
		{VPP: " host-eth3 ", VRID: 2, Priority: 100, VirtualAddr: " 10.79.2.1 "},
		{VPP: "host-eth2", VRID: 7, Priority: 120, IntervalMS: 2000, VirtualAddr: "10.79.1.1"},
		{VPP: "host-eth2", VRID: 1, Priority: 120, VirtualAddr: "10.79.1.2"},
	}}
	normalizeBasicConfig(&cfg)

	if len(cfg.VRRP) != 3 {
		t.Fatalf("rows = %d", len(cfg.VRRP))
	}
	if cfg.VRRP[0].VPP != "host-eth2" || cfg.VRRP[0].VRID != 1 {
		t.Fatalf("rows not sorted by (port, vr_id): %+v", cfg.VRRP)
	}
	if cfg.VRRP[1].VPP != "host-eth2" || cfg.VRRP[1].VRID != 7 {
		t.Fatalf("second row = %+v, want host-eth2 vr_id 7", cfg.VRRP[1])
	}
	if cfg.VRRP[2].VPP != "host-eth3" || cfg.VRRP[2].VirtualAddr != "10.79.2.1" {
		t.Fatalf("third row = %+v, want host-eth3 with the address trimmed", cfg.VRRP[2])
	}
	// An empty interval field means the default, not "VPP decides": 0 is refused
	// outright (measured retval=-7), so filling it here is what makes the check's
	// "interval 0 is impossible" assumption true.
	for i, v := range cfg.VRRP {
		if v.IntervalMS != vrrpDefaultIntervalMS && v.IntervalMS != 2000 {
			t.Errorf("[%d] interval = %d ms, want the value given or the %d ms default", i, v.IntervalMS, vrrpDefaultIntervalMS)
		}
	}

	// nil must not reach the page as JSON null.
	empty := BasicConfig{}
	normalizeBasicConfig(&empty)
	b, err := json.Marshal(empty.VRRP)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("VRRP must serialize as [], got %s", b)
	}
}

// Two drafts that differ only in the order the tenant clicked "add row" must
// publish identical bytes, or "this publish changed nothing" stops being
// provable -- the console compares artifact text, and so does the box.
func TestVRRPRowOrderDoesNotChangeTheArtifact(t *testing.T) {
	row1 := ConfigVRRP{VPP: "host-eth2", VRID: 1, Priority: 120, IntervalMS: 1000, VirtualAddr: "10.79.1.1"}
	row2 := ConfigVRRP{VPP: "host-eth3", VRID: 1, Priority: 100, IntervalMS: 1000, VirtualAddr: "10.79.2.1"}

	a := baseValidConfig()
	a.VRRP = []ConfigVRRP{row1, row2}
	b := baseValidConfig()
	b.VRRP = []ConfigVRRP{row2, row1}
	normalizeBasicConfig(&a)
	normalizeBasicConfig(&b)
	if renderVRRPArtifact(&a) != renderVRRPArtifact(&b) {
		t.Fatalf("row order leaked into the artifact:\n%s\n%s", renderVRRPArtifact(&a), renderVRRPArtifact(&b))
	}

	// ... and the ordering is not a no-op that collapses two different lists.
	one := baseValidConfig()
	one.VRRP = []ConfigVRRP{row1}
	normalizeBasicConfig(&one)
	if renderVRRPArtifact(&one) == renderVRRPArtifact(&a) {
		t.Fatalf("one row and two rows rendered the same bytes")
	}
}

// vrrpSegmentRow mirrors vrrpRow in internal/apply/vrrp.go -- the struct the box
// decodes these bytes into. Keys are decoded with no omitempty on that side, so a
// field the controller forgets to send becomes a zero, not an error. That is why
// this test reads the rendered text back instead of eyeballing it.
type vrrpSegmentRow struct {
	VPP        string   `json:"vpp"`
	VRID       uint8    `json:"vr_id"`
	Priority   uint8    `json:"priority"`
	IntervalMS uint16   `json:"interval_ms"`
	Preempt    bool     `json:"preempt"`
	Accept     bool     `json:"accept"`
	Addrs      []string `json:"addrs"`
}

func decodeSegment(t *testing.T, seg string) []vrrpSegmentRow {
	t.Helper()
	var doc struct {
		VRRP *[]vrrpSegmentRow `json:"vrrp"`
	}
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		t.Fatalf("第 6 段解不开: %v\n%s", err, seg)
	}
	if doc.VRRP == nil {
		t.Fatalf("第 6 段没有 vrrp 键（设备侧会当成「这份包早于 VRRP」，一条 VR 都不动）:\n%s", seg)
	}
	return *doc.VRRP
}

func TestRenderVRRPSegment(t *testing.T) {
	cfg := vrrpValidConfig()
	cfg.VRRP = append(cfg.VRRP, ConfigVRRP{VPP: "host-eth3", VRID: 9, Priority: 90,
		IntervalMS: 2000, Accept: true, VirtualAddr: "10.79.2.9"})
	normalizeBasicConfig(&cfg)

	rows := decodeSegment(t, renderVRRPArtifact(&cfg))
	if len(rows) != 2 {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	if rows[0].VPP != "host-eth2" || rows[0].VRID != 1 || rows[0].Priority != 120 ||
		rows[0].IntervalMS != vrrpDefaultIntervalMS || !rows[0].Preempt || rows[0].Accept {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].VPP != "host-eth3" || rows[1].VRID != 9 || rows[1].Priority != 90 ||
		rows[1].IntervalMS != 2000 || rows[1].Preempt || !rows[1].Accept {
		t.Errorf("row 1 = %+v", rows[1])
	}
	// One VIP per row, as the model defines it. The box would accept more, but
	// then the page would have to say which of several addresses is the gateway.
	for i, r := range rows {
		want := cfg.VRRP[i].VirtualAddr
		if len(r.Addrs) != 1 || r.Addrs[0] != want {
			t.Errorf("row %d addrs = %v, want exactly [%s]", i, r.Addrs, want)
		}
	}

	// The port is named, never numbered: sw_if_index is box-local and is
	// reassigned when VPP restarts, which is the event this segment survives.
	if strings.Contains(renderVRRPArtifact(&cfg), "sw_if_index") {
		t.Errorf("artifact must not carry a sw_if_index:\n%s", renderVRRPArtifact(&cfg))
	}

	// "No VRs" is a statement the box must be able to act on -- it deletes what
	// VPP has. An empty list is therefore emitted as an empty array, and only a
	// bundle published before VRRP existed is the empty string.
	none := BasicConfig{}
	normalizeBasicConfig(&none)
	seg := renderVRRPArtifact(&none)
	if strings.TrimSpace(seg) == "" {
		t.Fatalf("tenant with no VR rows must still get a 6th segment, got empty text")
	}
	if got := decodeSegment(t, seg); len(got) != 0 {
		t.Fatalf("empty list decoded as %d rows", len(got))
	}
	if !strings.Contains(seg, `"vrrp": []`) {
		t.Fatalf("empty list should render as an empty array, got:\n%s", seg)
	}
	b, err := json.Marshal(configArtifacts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"vrrp"`) {
		t.Fatalf("a pre-VRRP bundle is supposed to have no such key -- that is how the box tells the two apart:\n%s", b)
	}
}
