package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/apply"
)

func hasCheck(checks []ConfigCheck, severity, substr string) bool {
	for _, c := range checks {
		if c.Severity == severity && strings.Contains(c.Text, substr) {
			return true
		}
	}
	return false
}

func countSeverity(checks []ConfigCheck, severity string) int {
	n := 0
	for _, c := range checks {
		if c.Severity == severity {
			n++
		}
	}
	return n
}

func baseValidConfig() BasicConfig {
	return BasicConfig{
		Hostname: "edge-1",
		VPNs: []ConfigVPN{
			{
				ID: 0, Name: "default",
				BGP: BGPConfig{
					On: true, ASN: 65101, RouterID: "10.99.0.11",
					Neighbors: []BGPPeer{
						{IP: "10.79.1.3", RemoteAS: 65001, Description: "cust"},
						{IP: "10.79.1.4", RemoteAS: 65101, NextHopSelf: true},
					},
				},
				OSPF: OSPFConfig{On: true, RouterID: "10.99.0.11"},
			},
			{ID: 100, Name: "guest"},
		},
		Ifaces: []ConfigIface{
			{VPP: "host-eth2", LCP: "lcp-x0", IP: "10.79.1.2", PrefixLen: 24, VPN: 0, MTU: 1500, ArpIgnore: true, Up: true},
			{VPP: "host-eth3", LCP: "lcp-x1", IP: "10.79.2.2", PrefixLen: 24, VPN: 100, MTU: 1500, ArpIgnore: true, Up: true},
		},
		Statics: []ConfigStatic{
			{Prefix: "10.68.99.0/24", NextHop: "10.79.1.3", VPN: 0},
			{Prefix: "10.68.100.0/24", NextHop: "10.79.2.3", VPN: 100},
		},
	}
}

func TestValidateBasicConfig(t *testing.T) {
	valid := baseValidConfig()
	checks := validateBasicConfig(&valid)
	if n := countSeverity(checks, "error"); n != 0 {
		t.Fatalf("valid config has %d errors: %+v", n, checks)
	}

	cases := []struct {
		name     string
		mutate   func(*BasicConfig)
		severity string
		substr   string
	}{
		{
			name: "ebgp must be directly connected",
			mutate: func(c *BasicConfig) {
				c.VPNs[0].BGP.Neighbors[0].IP = "10.79.3.3"
			},
			severity: "error", substr: "eBGP 邻居 10.79.3.3",
		},
		{
			name: "ibgp may be multi-hop",
			mutate: func(c *BasicConfig) {
				c.VPNs[0].BGP.Neighbors[1].IP = "10.79.9.9"
				c.Statics = append(c.Statics, ConfigStatic{Prefix: "10.79.9.0/24", NextHop: "10.79.1.3", VPN: 0})
			},
			severity: "info", substr: "没有发现冲突",
		},
		{
			name: "ibgp without next-hop-self warns",
			mutate: func(c *BasicConfig) {
				c.VPNs[0].BGP.Neighbors[1].NextHopSelf = false
			},
			severity: "warning", substr: "next-hop-self",
		},
		{
			name: "reserved overlay table",
			mutate: func(c *BasicConfig) {
				c.VPNs = append(c.VPNs, ConfigVPN{ID: 250})
			},
			severity: "error", substr: "overlay 保留表",
		},
		{
			name: "reserved frr table",
			mutate: func(c *BasicConfig) {
				c.VPNs = append(c.VPNs, ConfigVPN{ID: 1250})
			},
			severity: "error", substr: "table-direct 保留表",
		},
		{
			name: "same address in two VRFs is legal",
			mutate: func(c *BasicConfig) {
				c.Ifaces[1].IP = "10.79.1.2"
				c.Ifaces[1].PrefixLen = 24
			},
			severity: "error", substr: "存在同一地址", // must NOT appear; asserted below
		},
		{
			name: "overlapping subnets in one VPN",
			mutate: func(c *BasicConfig) {
				c.Ifaces = append(c.Ifaces, ConfigIface{VPP: "host-eth4", LCP: "lcp-x2", IP: "10.79.1.130", PrefixLen: 25, VPN: 0, MTU: 1500})
			},
			severity: "error", substr: "网段重叠",
		},
		{
			name: "static in missing VPN",
			mutate: func(c *BasicConfig) {
				c.Statics[0].VPN = 7
			},
			severity: "error", substr: "不存在的 VPN 7",
		},
		{
			name: "non canonical static prefix",
			mutate: func(c *BasicConfig) {
				c.Statics[0].Prefix = "10.68.99.7/24"
			},
			severity: "error", substr: "不是规范网段",
		},
		{
			name: "tap name too long",
			mutate: func(c *BasicConfig) {
				c.Ifaces[0].LCP = "lcp-0123456789abc"
			},
			severity: "error", substr: "内核 TAP 名不合法",
		},
		{
			name: "ebgp remote-as out of range",
			mutate: func(c *BasicConfig) {
				c.VPNs[0].BGP.Neighbors[0].RemoteAS = 70000
			},
			severity: "error", substr: "对端 AS 需在 1–65535",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfig()
			tc.mutate(&cfg)
			checks := validateBasicConfig(&cfg)
			if tc.name == "same address in two VRFs is legal" {
				if hasCheck(checks, "error", tc.substr) {
					t.Fatalf("same address in different VRFs must not conflict: %+v", checks)
				}
				return
			}
			if !hasCheck(checks, tc.severity, tc.substr) {
				t.Fatalf("want %s containing %q, got %+v", tc.severity, tc.substr, checks)
			}
		})
	}
}

// The page renders neighbor lists directly, so the server must never hand it
// null where the UI expects an array; VPN 0 also has to exist even if a draft
// was written without it.
func TestNormalizeBasicConfig(t *testing.T) {
	cfg := BasicConfig{
		Hostname: "  edge-1  ",
		VPNs:     []ConfigVPN{{ID: 10, Name: " office "}},
	}
	normalizeBasicConfig(&cfg)
	if len(cfg.VPNs) != 2 || cfg.VPNs[0].ID != 0 || cfg.VPNs[1].ID != 10 {
		t.Fatalf("want VPN 0 added before VPN 10, got %+v", cfg.VPNs)
	}
	if cfg.VPNs[1].Name != "office" {
		t.Fatalf("name not trimmed: %q", cfg.VPNs[1].Name)
	}
	if cfg.VPNs[1].BGP.Neighbors == nil {
		t.Fatalf("neighbors must be an empty slice, not nil")
	}
	if cfg.Ifaces == nil || cfg.Statics == nil {
		t.Fatalf("ifaces/statics must be empty slices, not nil")
	}
	b, err := json.Marshal(cfg.VPNs[1].BGP)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"neighbors":[]`) {
		t.Fatalf("neighbors must serialize as []: %s", b)
	}
}

func TestRenderArtifacts(t *testing.T) {
	cfg := baseValidConfig()
	cfg.VPNs[1].BGP = BGPConfig{On: true, ASN: 65200, RouterID: "10.99.0.12"}
	cfg.VPNs[1].OSPF = OSPFConfig{On: true, RouterID: "10.99.0.12"}
	cfg.VPNs[0].BGP.Neighbors[1].NextHopSelf = true

	frr := renderFRR(&cfg)
	for _, want := range []string{
		"hostname edge-1",
		"interface lcp-x0\n ip address 10.79.1.2/24",
		"interface lcp-x1 vrf vpn100",
		"ip address 10.79.2.2/24",
		"ip route 10.68.99.0/24 10.79.1.3",
		"vrf vpn100\n ip route 10.68.100.0/24 10.79.2.3\nexit-vrf",
		"router bgp 65101",
		"router bgp 65200 vrf vpn100",
		"neighbor 10.79.1.4 remote-as 65101",
		"neighbor 10.79.1.4 next-hop-self",
		"router ospf",
		"router ospf vrf vpn100",
		"network 10.79.2.0/24 area 0",
	} {
		if !strings.Contains(frr, want) {
			t.Errorf("FRR artifact missing %q\n%s", want, frr)
		}
	}
	if strings.Contains(frr, "set interface") {
		t.Errorf("FRR artifact must not contain VPP commands:\n%s", frr)
	}

	vpp := renderVPP(&cfg)
	for _, want := range []string{
		"ip table add 100",
		"set interface ip table host-eth3 100",
		"set interface mtu packet 1500 host-eth2",
		"set interface state host-eth2 up",
		"lcp create host-eth2 host-if lcp-x0",
		"lcp lcp-sync on",
	} {
		if !strings.Contains(vpp, want) {
			t.Errorf("VPP artifact missing %q\n%s", want, vpp)
		}
	}
	if strings.Contains(vpp, "set interface ip address") {
		t.Errorf("address must be owned by FRR, not VPP:\n%s", vpp)
	}
	if strings.Contains(vpp, "set interface ip table host-eth2 0") {
		t.Errorf("VPN 0 is the default table; no table command expected:\n%s", vpp)
	}

	boot := renderBoot(&cfg)
	for _, want := range []string{
		"ip link show vpn100 >/dev/null 2>&1 || ip link add vpn100 type vrf table 100",
		"ip link set lcp-x1 master vpn100",
		"net.ipv4.ip_forward=0",
		"net.ipv4.conf.lcp-x0.arp_ignore=1",
		"net.ipv4.conf.lcp-x1.rp_filter=1",
	} {
		if !strings.Contains(boot, want) {
			t.Errorf("boot artifact missing %q\n%s", want, boot)
		}
	}
}

func configRequest(t *testing.T, s *server, sid, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if method == http.MethodPut || method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	s.handleAdminConfig(rec, r)
	return rec
}

func decodeConfigView(t *testing.T, rec *httptest.ResponseRecorder) configView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v configView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, rec.Body.String())
	}
	return v
}

func TestConfigAPI(t *testing.T) {
	s := newTestServer()
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	s.state.Configs = map[string]*DeviceConfig{}
	addNode(s, "t", 1, nil, "")
	addNode(s, "t2", 2, nil, "")

	if rec := configRequest(t, s, "", http.MethodGet, "/api/config?node=1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})
	if rec := configRequest(t, s, op, http.MethodGet, "/api/config?node=1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403", rec.Code)
	}

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})

	// An unconfigured node is not an error: it renders an empty VPN 0, and the
	// server says so instead of inventing a plan.
	empty := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", ""))
	if empty.Version != 0 || len(empty.Config.VPNs) != 1 || empty.Config.VPNs[0].ID != 0 {
		t.Fatalf("unexpected empty config: %+v", empty)
	}
	if len(empty.Checks) == 0 || len(empty.Artifacts.FRR) == 0 {
		t.Fatalf("empty config must carry checks and artifacts: %+v", empty)
	}

	// Tenant isolation is a 404, not a filtered listing.
	if rec := configRequest(t, s, other, http.MethodGet, "/api/config?node=1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read = %d, want 404", rec.Code)
	}
	if rec := configRequest(t, s, ten, http.MethodGet, "/api/config?node=2", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("other-tenant node = %d, want 404", rec.Code)
	}

	cfg := baseValidConfig()
	body, _ := json.Marshal(saveConfigRequest{NodeID: 1, Config: cfg})
	saved := decodeConfigView(t, configRequest(t, s, ten, http.MethodPut, "/api/config", string(body)))
	if saved.Version != 1 || saved.UpdatedBy != "alice" {
		t.Fatalf("saved version/by = %d/%q, want 1/alice", saved.Version, saved.UpdatedBy)
	}
	if saved.Config.Hostname != "edge-1" || len(saved.Config.Ifaces) != 2 {
		t.Fatalf("round-trip lost fields: %+v", saved.Config)
	}
	if !strings.Contains(saved.Artifacts.FRR, "router bgp 65101") {
		t.Fatalf("artifact not generated for saved config")
	}

	// It survives a restart of the controller (state file reload), because the
	// draft is configuration, not runtime state.
	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("state reload: %v", err)
	}
	dc := st.Configs[key("t", 1)]
	if dc == nil || dc.Draft.Hostname != "edge-1" || dc.Version != 1 {
		t.Fatalf("config did not persist: %+v", dc)
	}

	// A draft that still has errors is accepted (half-written policy is a normal
	// state); the checks are what remind the tenant it cannot be applied yet.
	cfg.Hostname = "bad host!"
	body, _ = json.Marshal(saveConfigRequest{NodeID: 1, Config: cfg})
	again := decodeConfigView(t, configRequest(t, s, ten, http.MethodPut, "/api/config", string(body)))
	if again.Version != 2 || !hasCheck(again.Checks, "error", "主机名") {
		t.Fatalf("invalid draft = version %d checks %+v", again.Version, again.Checks)
	}

	if rec := configRequest(t, s, ten, http.MethodPut, "/api/config", "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", rec.Code)
	}
	if rec := configRequest(t, s, ten, http.MethodPost, "/api/config", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

func bundleRequest(t *testing.T, s *server, sid, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	// The session gate answers 415 to a POST without JSON before the handler
	// can refuse the method, so send a body-looking request to reach it.
	if method != http.MethodGet {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	s.handleAdminConfigBundle(rec, r)
	return rec
}

// The bundle is the contract with cmd/sr-apply: decode it with the real
// applier type, not with a hand-written local struct.
func TestConfigBundleAPI(t *testing.T) {
	s := newTestServer()
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	s.state.Configs = map[string]*DeviceConfig{}
	addNode(s, "t", 1, nil, "")
	addNode(s, "t2", 2, nil, "")

	if rec := bundleRequest(t, s, "", http.MethodGet, "/api/config/bundle?node=1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})
	if rec := bundleRequest(t, s, op, http.MethodGet, "/api/config/bundle?node=1"); rec.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403", rec.Code)
	}
	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	if rec := bundleRequest(t, s, other, http.MethodGet, "/api/config/bundle?node=1"); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant = %d, want 404", rec.Code)
	}
	if rec := bundleRequest(t, s, ten, http.MethodGet, "/api/config/bundle?node=1"); rec.Code != http.StatusConflict {
		t.Fatalf("unsaved node = %d, want 409", rec.Code)
	}
	if rec := bundleRequest(t, s, ten, http.MethodGet, "/api/config/bundle?node=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("node 0 = %d, want 400", rec.Code)
	}
	if rec := bundleRequest(t, s, ten, http.MethodPost, "/api/config/bundle?node=1"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}

	body, _ := json.Marshal(saveConfigRequest{NodeID: 1, Config: baseValidConfig()})
	if rec := configRequest(t, s, ten, http.MethodPut, "/api/config", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("save = %d, %s", rec.Code, rec.Body.String())
	}
	rec := bundleRequest(t, s, ten, http.MethodGet, "/api/config/bundle?node=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d, %s", rec.Code, rec.Body.String())
	}
	b, err := apply.DecodeBundle(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("bundle does not decode as apply.Bundle: %v", err)
	}
	if b.Version != 1 || b.NodeID != 1 || b.GeneratedAt == 0 {
		t.Fatalf("bundle header = %+v", b)
	}
	if !strings.Contains(b.Artifacts.FRR, "router bgp 65101") || !strings.Contains(b.Artifacts.VPP, "lcp create") {
		t.Fatalf("bundle artifacts incomplete: %+v", b.Artifacts)
	}
	if errs := b.BlockingErrors(); len(errs) != 0 {
		t.Fatalf("valid draft has blocking checks: %v", errs)
	}

	// A draft saved with errors still exports, but the applier is expected to
	// refuse it; the bundle must carry the error for that decision.
	bad := baseValidConfig()
	bad.Hostname = "bad host!"
	body, _ = json.Marshal(saveConfigRequest{NodeID: 1, Config: bad})
	if rec := configRequest(t, s, ten, http.MethodPut, "/api/config", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("save invalid = %d", rec.Code)
	}
	rec = bundleRequest(t, s, ten, http.MethodGet, "/api/config/bundle?node=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle after invalid save = %d", rec.Code)
	}
	b, err = apply.DecodeBundle(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(b.BlockingErrors()) == 0 {
		t.Fatalf("error checks did not reach the bundle: %+v", b.Checks)
	}
}
