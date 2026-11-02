package main

// Tests for the 路由泄露 layer's model, gates and HTTP surface (L-1).
//
// The properties worth pinning down are the ones a page cannot show: a rule
// that survives as an empty row, a VPN the box does not have, a protocol the
// VPN has no instance for, a batch that half-applies, and a publish that skips
// the leak gate because the base config was fine. All of them look plausible in
// the UI and all of them are wrong.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/route"
)

func leakFixture(t *testing.T) (*server, string, string) {
	t.Helper()
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	s.state.Overlays = map[string]*OverlayConfig{}
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	addNode(s, "t", 1, nil, "203.0.113.1:40000")
	addNode(s, "t", 2, nil, "203.0.113.2:40000")
	addNode(s, "t2", 3, nil, "203.0.113.3:40000")

	bgp := BGPConfig{On: true, ASN: 65101, RouterID: "10.0.0.1"}
	ospf := OSPFConfig{On: true, RouterID: "10.0.0.1"}
	s.state.Configs[configKey("t", 1)] = &DeviceConfig{
		TenantID: "t", NodeID: 1, Version: 3,
		Draft: BasicConfig{
			Hostname: "cpe-1",
			VPNs: []ConfigVPN{
				{ID: 0, Name: "default", BGP: bgp, OSPF: ospf},
				{ID: 10, Name: "guest", BGP: BGPConfig{On: true, ASN: 65101, RouterID: "10.0.0.1"}},
				{ID: 20, Name: "idle"},
			},
			Ifaces: []ConfigIface{}, Statics: []ConfigStatic{},
		},
	}
	s.state.Configs[configKey("t", 2)] = &DeviceConfig{
		TenantID: "t", NodeID: 2, Version: 3,
		Draft: BasicConfig{
			Hostname: "cpe-2",
			VPNs: []ConfigVPN{
				{ID: 0, Name: "default", OSPF: OSPFConfig{On: true, RouterID: "10.0.0.2"}},
			},
			Ifaces: []ConfigIface{}, Statics: []ConfigStatic{},
		},
	}
	oc := s.overlayConfigLocked("t", true)
	oc.Groups = []OverlayGroup{{
		ID: "og1", Name: "g", RT: "65000:1000",
		Members: []OverlayMember{
			{NodeID: 1, VPNID: 0}, {NodeID: 1, VPNID: 10}, {NodeID: 2, VPNID: 0},
		},
	}}
	oc.NextRT = 1001

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	return s, ten, other
}

func leakDo(t *testing.T, s *server, method, target, body, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	switch r.URL.Path {
	case "/api/leak":
		s.handleAdminLeak(rec, r)
	case "/api/leak/rows":
		s.handleAdminLeakRows(rec, r)
	case "/api/leak/batch":
		s.handleAdminLeakBatch(rec, r)
	case "/api/leak/republish":
		s.handleAdminLeakRepublish(rec, r)
	case "/api/prefix-lists":
		s.handleAdminPrefixLists(rec, r)
	default:
		t.Fatalf("no leak handler for %s", r.URL.Path)
	}
	return rec
}

func leakGet(t *testing.T, s *server, sid string) leakView {
	t.Helper()
	rec := leakDo(t, s, http.MethodGet, "/api/leak", "", sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/leak = %d (%s)", rec.Code, rec.Body.String())
	}
	var v leakView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode leak view: %v", err)
	}
	return v
}

func leakVPN(t *testing.T, v leakView, nodeID uint16, vpn uint32) leakVPNRow {
	t.Helper()
	for _, n := range v.Nodes {
		if n.NodeID != nodeID {
			continue
		}
		for _, row := range n.VPNs {
			if row.ID == vpn {
				return row
			}
		}
		t.Fatalf("node %d has no VPN %d in the view", nodeID, vpn)
	}
	t.Fatalf("node %d is not in the view", nodeID)
	return leakVPNRow{}
}

func mustRuleJSON(t *testing.T, r *LeakRule) string {
	t.Helper()
	b, _ := json.Marshal(r)
	return string(b)
}

func TestLeakLibrarySeedsAllAndReseedsAfterDeleteAll(t *testing.T) {
	lc := &LeakConfig{TenantID: "t"}
	lc.ensure()
	if len(lc.PrefixLists) != 1 {
		t.Fatalf("library = %d lists, want the seeded ALL", len(lc.PrefixLists))
	}
	first := lc.PrefixLists[0]
	if first.Name != leakDefaultListName || len(first.Entries) != 1 || first.Entries[0].Prefix != "0.0.0.0/0" {
		t.Fatalf("seeded list = %+v, want ALL with 0.0.0.0/0", first)
	}
	// Deleting every list is allowed; the next read brings ALL back under a new
	// id, because ids are never reused (rules and logs quote them).
	lc.PrefixLists = nil
	lc.ensure()
	if len(lc.PrefixLists) != 1 {
		t.Fatalf("library = %d lists after delete-all, want a fresh ALL", len(lc.PrefixLists))
	}
	if lc.PrefixLists[0].ID == first.ID {
		t.Fatalf("re-seeded list reused id %s", first.ID)
	}
}

func TestNormalizeLeakRuleAndSet(t *testing.T) {
	r := LeakRule{Protocols: []string{" OSPF ", "bgp", "BGP", ""}, PrefixList: " pl1 "}
	normalizeLeakRule(&r)
	if strings.Join(r.Protocols, ",") != "bgp,ospf" || r.PrefixList != "pl1" {
		t.Fatalf("normalized rule = %+v, want bgp,ospf + pl1", r)
	}

	set := LeakSet{ToCPE: []LeakRule{
		{VPN: 10, Protocols: []string{"bgp"}},
		{VPN: 0, Protocols: []string{"ospf"}},
		{VPN: 10, Protocols: []string{"ospf"}},
	}}
	normalizeLeakSet(&set)
	if len(set.ToCPE) != 2 || set.ToCPE[0].VPN != 0 || set.ToCPE[1].VPN != 10 {
		t.Fatalf("normalized set = %+v, want VPN 0 then 10", set.ToCPE)
	}
	if strings.Join(set.ToCPE[1].Protocols, ",") != "ospf" {
		t.Fatalf("duplicate VPN row did not keep the last value: %+v", set.ToCPE[1])
	}
	if set.ToSDWAN == nil {
		t.Fatal("empty direction became nil; JSON would say null")
	}
}

func TestPrefixListChecks(t *testing.T) {
	base := PrefixList{ID: "pl2", Name: "DC", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8", LE: 24}}}
	if checks := prefixListChecks(base, []PrefixList{{ID: "pl1", Name: "ALL"}}, "pl2"); hasBlockingCheck(checks) != "" {
		t.Fatalf("valid list refused: %+v", checks)
	}

	cases := []struct {
		name   string
		list   PrefixList
		others []PrefixList
		want   string
	}{
		{"name required", PrefixList{ID: "pl2"}, nil, "要有名字"},
		{"duplicate name", base, []PrefixList{{ID: "pl1", Name: "dc"}}, "已经有一个叫"},
		{"bad prefix", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{{Prefix: "10.0.0.300/8"}}}, nil, "不是合法 IPv4"},
		{"non canonical", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{{Prefix: "10.0.0.1/8"}}}, nil, "不是规范网段"},
		{"ge below length", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8", GE: 4}}}, nil, "ge 需在 8–32"},
		{"le above 32", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8", LE: 33}}}, nil, "le 需在 8–32"},
		{"le below ge", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8", GE: 16, LE: 12}}}, nil, "不能小于 ge"},
		{"duplicate entry", PrefixList{ID: "pl2", Name: "x", Entries: []PrefixEntry{
			{Prefix: "10.0.0.0/8"}, {Prefix: "10.0.0.0/8"},
		}}, nil, "重复"},
	}
	for _, c := range cases {
		if got := hasBlockingCheck(prefixListChecks(c.list, c.others, c.list.ID)); !strings.Contains(got, c.want) {
			t.Fatalf("%s: blocking = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLeakRuleGates(t *testing.T) {
	s, _, _ := leakFixture(t)
	lc := s.leakConfigLocked("t", true)
	all := lc.PrefixLists[0].ID
	emptyList := PrefixList{ID: "pl99", Name: "EMPTY", Entries: []PrefixEntry{}}
	lc.PrefixLists = append(lc.PrefixLists, emptyList)

	cases := []struct {
		name      string
		node      uint16
		direction string
		rule      LeakRule
		want      string
	}{
		{"cpE direction valid", 1, leakToCPE, LeakRule{VPN: 10, Protocols: []string{"bgp"}}, ""},
		{"sdwan direction valid", 1, leakToSDWAN, LeakRule{VPN: 10, Protocols: []string{"bgp"}, PrefixList: all}, ""},
		{"no instance for protocol", 1, leakToCPE, LeakRule{VPN: 10, Protocols: []string{"ospf"}}, "没有启用 OSPF"},
		{"static only belongs to cpe-sdwan", 1, leakToCPE, LeakRule{VPN: 10, Protocols: []string{"static"}}, "只属于 CPE-SDWAN"},
		{"vpn not in a group", 1, leakToCPE, LeakRule{VPN: 20, Protocols: []string{"bgp"}}, "不在任何导通策略里"},
		{"vpn not in base config", 1, leakToCPE, LeakRule{VPN: 30, Protocols: []string{"bgp"}}, "没有 VPN 30"},
		{"reserved overlay table", 1, leakToCPE, LeakRule{VPN: vpnReservedOverlay, Protocols: []string{"bgp"}}, "overlay 保留表"},
		{"beyond vrf width", 1, leakToCPE, LeakRule{VPN: 300, Protocols: []string{"bgp"}}, "超出泄露规则范围"},
		{"no protocols", 1, leakToCPE, LeakRule{VPN: 10}, "至少选一个来源"},
		{"sdwan needs a list", 1, leakToSDWAN, LeakRule{VPN: 10, Protocols: []string{"bgp"}}, "必须选一个 prefix-list"},
		{"unknown list", 1, leakToSDWAN, LeakRule{VPN: 10, Protocols: []string{"bgp"}, PrefixList: "pl404"}, "不存在"},
		{"empty list for cpe-sdwan", 1, leakToSDWAN, LeakRule{VPN: 10, Protocols: []string{"bgp"}, PrefixList: emptyList.ID}, "没有条目"},
		{"no draft yet", 3, leakToCPE, LeakRule{VPN: 0, Protocols: []string{"bgp"}}, "还没有保存过基础配置草稿"},
	}
	for _, c := range cases {
		checks := s.leakRuleChecksLocked("t", c.node, c.direction, c.rule)
		got := hasBlockingCheck(checks)
		if c.want == "" {
			if got != "" {
				t.Fatalf("%s: refused: %s", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Fatalf("%s: blocking = %q, want %q (checks %+v)", c.name, got, c.want, checks)
		}
	}
}

func TestLeakViewShowsWhatTheDeviceHas(t *testing.T) {
	s, ten, _ := leakFixture(t)
	v := leakGet(t, s, ten)

	if len(v.PrefixLists) != 1 || v.PrefixLists[0].Name != "ALL" || v.PrefixLists[0].ReferencedBy != 0 {
		t.Fatalf("library view = %+v, want one unreferenced ALL", v.PrefixLists)
	}
	if len(v.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (tenant scoped)", len(v.Nodes))
	}
	if row := leakVPN(t, v, 1, 0); !row.Selectable || !row.InGroup || !row.HasBGP || !row.HasOSPF {
		t.Fatalf("VPN 0 row = %+v, want selectable with both instances", row)
	}
	if row := leakVPN(t, v, 1, 20); row.Selectable || row.InGroup {
		t.Fatalf("VPN 20 row = %+v, want not selectable (not in a group)", row)
	}
	if row := leakVPN(t, v, 2, 0); !row.Selectable || row.HasBGP || !row.HasOSPF {
		t.Fatalf("node 2 VPN 0 row = %+v, want selectable, OSPF only", row)
	}
}

func TestLeakRowsAPI(t *testing.T) {
	s, ten, _ := leakFixture(t)
	all := leakGet(t, s, ten).PrefixLists[0].ID

	put := func(node uint16, direction string, vpn uint32, protocols []string, list string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(leakRowRequest{
			NodeID: node, Direction: direction, VPN: vpn, Protocols: protocols, PrefixList: list,
		})
		return leakDo(t, s, http.MethodPut, "/api/leak/rows", string(b), ten)
	}

	if rec := put(1, leakToSDWAN, 10, []string{"bgp"}, all); rec.Code != http.StatusOK {
		t.Fatalf("save CPE-SDWAN row = %d (%s)", rec.Code, rec.Body.String())
	}
	dc := s.state.Configs[configKey("t", 1)]
	if dc.Version != 4 || len(dc.Leak.ToSDWAN) != 1 || dc.Leak.ToSDWAN[0].VPN != 10 {
		t.Fatalf("draft after save = version %d leak %+v", dc.Version, dc.Leak)
	}
	if rec := put(1, leakToCPE, 0, []string{"ospf", "bgp"}, ""); rec.Code != http.StatusOK {
		t.Fatalf("save SDWAN-CPE row = %d (%s)", rec.Code, rec.Body.String())
	}
	if dc.Leak.ToCPE[0].PrefixList != "" || strings.Join(dc.Leak.ToCPE[0].Protocols, ",") != "bgp,ospf" {
		t.Fatalf("SDWAN-CPE row = %+v", dc.Leak.ToCPE[0])
	}

	if rec := put(1, leakToCPE, 20, []string{"bgp"}, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("VPN without instance accepted: %d", rec.Code)
	}
	if rec := put(1, leakToCPE, 10, []string{"static"}, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("static accepted on SDWAN-CPE: %d", rec.Code)
	}
	if rec := put(99, leakToCPE, 0, []string{"bgp"}, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d, want 404", rec.Code)
	}
	if rec := put(3, leakToCPE, 0, []string{"bgp"}, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("other tenant's node = %d, want 404", rec.Code)
	}

	// Empty protocol set deletes the row rather than storing a row that renders
	// nothing.
	if rec := put(1, leakToSDWAN, 10, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete row = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(dc.Leak.ToSDWAN) != 0 {
		t.Fatalf("row survived an empty protocol set: %+v", dc.Leak.ToSDWAN)
	}
}

func TestLeakBatchDropsWhatTheDeviceCannotDoAndPublishesTheRest(t *testing.T) {
	s, ten, _ := leakFixture(t)
	all := leakGet(t, s, ten).PrefixLists[0].ID

	// static/connected need no instance, so a VPN with neither BGP nor OSPF still
	// keeps a static row. This pins §7's "only drop the protocol the VPN lacks".
	body, _ := json.Marshal(leakBatchRequest{
		Direction: leakToSDWAN, NodeIDs: []uint16{1}, VPNs: []uint32{10},
		Protocols: []string{"bgp", "ospf", "static"}, PrefixList: all,
	})
	rec := leakDo(t, s, http.MethodPost, "/api/leak/batch", string(body), ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("static preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var staticView leakBatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &staticView); err != nil {
		t.Fatalf("decode static preview: %v", err)
	}
	if staticView.Blocked || len(staticView.Rows) != 1 {
		t.Fatalf("static preview = %+v, want one clean row", staticView)
	}
	if row := staticView.Rows[0]; !row.Keep ||
		strings.Join(row.Rule.Protocols, ",") != "bgp,static" ||
		!strings.Contains(row.Reason, "没有启用 OSPF") {
		t.Fatalf("static preview row = %+v, want bgp+static and the OSPF drop reason", row)
	}

	body, _ = json.Marshal(leakBatchRequest{
		Direction: leakToSDWAN, NodeIDs: []uint16{1, 2}, VPNs: []uint32{0, 10, 20},
		Protocols: []string{"bgp", "ospf"}, PrefixList: all,
	})
	rec = leakDo(t, s, http.MethodPost, "/api/leak/batch", string(body), ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var preview leakBatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Blocked {
		t.Fatalf("preview blocked with no checks failing: %+v", preview)
	}
	byKey := map[[2]uint32]leakBatchRow{}
	for _, row := range preview.Rows {
		byKey[[2]uint32{uint32(row.NodeID), row.VPN}] = row
	}
	if row := byKey[[2]uint32{1, 0}]; !row.Keep || strings.Join(row.Rule.Protocols, ",") != "bgp,ospf" {
		t.Fatalf("node1 VPN0 = %+v, want bgp+ospf", row)
	}
	if row := byKey[[2]uint32{1, 10}]; !row.Keep || strings.Join(row.Rule.Protocols, ",") != "bgp" ||
		!strings.Contains(row.Reason, "没有启用 OSPF") {
		t.Fatalf("node1 VPN10 = %+v, want bgp and the drop reason", row)
	}
	if row := byKey[[2]uint32{2, 0}]; !row.Keep || strings.Join(row.Rule.Protocols, ",") != "ospf" {
		t.Fatalf("node2 VPN0 = %+v, want ospf", row)
	}
	// Every protocol dropped means the whole VPN row is not sent, not an empty rule.
	if row := byKey[[2]uint32{1, 20}]; row.Keep || row.Rule != nil ||
		!strings.Contains(row.Reason, "没有启用 BGP") || !strings.Contains(row.Reason, "没有启用 OSPF") {
		t.Fatalf("node1 VPN20 = %+v, want the row dropped with both reasons", row)
	}

	body, _ = json.Marshal(leakBatchRequest{
		Direction: leakToSDWAN, NodeIDs: []uint16{1, 2}, VPNs: []uint32{0, 10, 20},
		Protocols: []string{"bgp", "ospf"}, PrefixList: all, Apply: true,
	})
	rec = leakDo(t, s, http.MethodPost, "/api/leak/batch", string(body), ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d (%s)", rec.Code, rec.Body.String())
	}
	var applied leakBatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	if len(applied.Results) != 2 {
		t.Fatalf("results = %+v, want both devices", applied.Results)
	}
	for _, node := range []uint16{1, 2} {
		dc := s.state.Configs[configKey("t", node)]
		if dc.Version != 4 || dc.Published == nil || dc.Published.Version != 4 {
			t.Fatalf("node %d after apply: version %d published %+v", node, dc.Version, dc.Published)
		}
	}
	if rows := s.state.Configs[configKey("t", 2)].Leak.ToSDWAN; len(rows) != 1 ||
		strings.Join(rows[0].Protocols, ",") != "ospf" {
		t.Fatalf("node 2 stored rows = %+v", rows)
	}
}

func TestLeakBatchIsAllOrNothing(t *testing.T) {
	s, ten, _ := leakFixture(t)
	addNode(s, "t", 4, nil, "203.0.113.4:40000") // no draft
	all := leakGet(t, s, ten).PrefixLists[0].ID

	body, _ := json.Marshal(leakBatchRequest{
		Direction: leakToSDWAN, NodeIDs: []uint16{1, 4}, VPNs: []uint32{0},
		Protocols: []string{"bgp"}, PrefixList: all, Apply: true,
	})
	rec := leakDo(t, s, http.MethodPost, "/api/leak/batch", string(body), ten)
	if rec.Code != http.StatusConflict {
		t.Fatalf("batch with a v0 device = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if dc := s.state.Configs[configKey("t", 1)]; dc.Version != 3 || len(dc.Leak.ToSDWAN) != 0 || dc.Published != nil {
		t.Fatalf("node 1 was written by a refused batch: %+v", dc)
	}
}

func TestPrefixListCRUDAndDeleteGate(t *testing.T) {
	s, ten, _ := leakFixture(t)

	create := func(name string, entries []PrefixEntry) *httptest.ResponseRecorder {
		b, _ := json.Marshal(prefixListRequest{Name: name, Entries: entries})
		return leakDo(t, s, http.MethodPost, "/api/prefix-lists", string(b), ten)
	}
	rec := create("DC", []PrefixEntry{{Prefix: "10.0.0.0/8", LE: 24}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		List PrefixList `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.List.ID == "" || created.List.Name != "DC" {
		t.Fatalf("created list = %+v", created.List)
	}
	if rec := create("dc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name = %d, want 400", rec.Code)
	}
	if rec := create("BAD", []PrefixEntry{{Prefix: "10.0.0.0/8", GE: 24, LE: 16}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ge/le = %d, want 400", rec.Code)
	}

	body, _ := json.Marshal(prefixListRequest{ID: created.List.ID, Name: "DC-2", Entries: created.List.Entries})
	if rec := leakDo(t, s, http.MethodPut, "/api/prefix-lists", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}

	row, _ := json.Marshal(leakRowRequest{
		NodeID: 1, Direction: leakToSDWAN, VPN: 0, Protocols: []string{"bgp"}, PrefixList: created.List.ID,
	})
	if rec := leakDo(t, s, http.MethodPut, "/api/leak/rows", string(row), ten); rec.Code != http.StatusOK {
		t.Fatalf("row save = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = leakDo(t, s, http.MethodDelete, "/api/prefix-lists?id="+created.List.ID, "", ten)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "引用") {
		t.Fatalf("delete referenced = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// Clearing the row releases the list; deleting the last remaining list is
	// allowed and the library re-seeds ALL on the next read.
	row, _ = json.Marshal(leakRowRequest{NodeID: 1, Direction: leakToSDWAN, VPN: 0})
	if rec := leakDo(t, s, http.MethodPut, "/api/leak/rows", string(row), ten); rec.Code != http.StatusOK {
		t.Fatalf("row clear = %d", rec.Code)
	}
	if rec := leakDo(t, s, http.MethodDelete, "/api/prefix-lists?id="+created.List.ID, "", ten); rec.Code != http.StatusOK {
		t.Fatalf("delete unreferenced = %d (%s)", rec.Code, rec.Body.String())
	}
	allID := leakGet(t, s, ten).PrefixLists[0].ID
	if rec := leakDo(t, s, http.MethodDelete, "/api/prefix-lists?id="+allID, "", ten); rec.Code != http.StatusOK {
		t.Fatalf("delete ALL = %d (%s)", rec.Code, rec.Body.String())
	}
	v := leakGet(t, s, ten)
	if len(v.PrefixLists) != 1 || v.PrefixLists[0].Name != leakDefaultListName || v.PrefixLists[0].ID == allID {
		t.Fatalf("library after delete-all = %+v, want a fresh ALL", v.PrefixLists)
	}
}

func TestLeakRepublishExpandsReferencingDevices(t *testing.T) {
	s, ten, _ := leakFixture(t)
	all := leakGet(t, s, ten).PrefixLists[0].ID

	put := func(node uint16, protocols []string) {
		b, _ := json.Marshal(leakRowRequest{
			NodeID: node, Direction: leakToSDWAN, VPN: 0, Protocols: protocols, PrefixList: all,
		})
		if rec := leakDo(t, s, http.MethodPut, "/api/leak/rows", string(b), ten); rec.Code != http.StatusOK {
			t.Fatalf("row save node %d = %d (%s)", node, rec.Code, rec.Body.String())
		}
	}
	put(1, []string{"bgp"})
	put(2, []string{"ospf"})
	for _, node := range []uint16{1, 2} {
		dc := s.state.Configs[configKey("t", node)]
		checks := s.publishChecksLocked("t", node, dc)
		if msg := hasBlockingCheck(checks); msg != "" {
			t.Fatalf("publish gate says %q before republish", msg)
		}
		s.publishDeviceLocked("t", node, dc, checks, "alice", time.Now().Unix())
	}

	body, _ := json.Marshal(leakRepublishRequest{PrefixListID: all})
	rec := leakDo(t, s, http.MethodPost, "/api/leak/republish", string(body), ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("republish preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var preview leakRepublishView
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Blocked || len(preview.Rows) != 2 {
		t.Fatalf("preview = %+v, want both referencing devices publishable", preview)
	}
	for _, row := range preview.Rows {
		if !row.WillPublish {
			t.Fatalf("row %+v should be publishable", row)
		}
	}

	body, _ = json.Marshal(leakRepublishRequest{PrefixListID: all, Apply: true})
	rec = leakDo(t, s, http.MethodPost, "/api/leak/republish", string(body), ten)
	if rec.Code != http.StatusOK || len(rec.Body.Bytes()) == 0 {
		t.Fatalf("republish apply = %d (%s)", rec.Code, rec.Body.String())
	}
	var applied leakRepublishView
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	if len(applied.Results) != 2 {
		t.Fatalf("results = %+v, want one per referencing device", applied.Results)
	}
}

func TestPublishGateIncludesLeakRules(t *testing.T) {
	s, ten, _ := leakFixture(t)
	all := leakGet(t, s, ten).PrefixLists[0].ID
	row, _ := json.Marshal(leakRowRequest{
		NodeID: 1, Direction: leakToSDWAN, VPN: 10, Protocols: []string{"bgp"}, PrefixList: all,
	})
	if rec := leakDo(t, s, http.MethodPut, "/api/leak/rows", string(row), ten); rec.Code != http.StatusOK {
		t.Fatalf("row save = %d (%s)", rec.Code, rec.Body.String())
	}
	// The row was valid when saved. Removing the VPN from the group afterwards
	// must stop the publish, not surface as a missing RT on the box.
	oc := s.overlayConfigLocked("t", true)
	oc.Groups = []OverlayGroup{{ID: "og1", Name: "g", RT: "65000:1000",
		Members: []OverlayMember{{NodeID: 1, VPNID: 0}, {NodeID: 2, VPNID: 0}}}}

	rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "导通策略") {
		t.Fatalf("publish = %d (%s), want 409 naming the group gate", rec.Code, rec.Body.String())
	}
}

func TestPublishBumpsVersionWhenOnlyTheArtifactsChanged(t *testing.T) {
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	addNode(s, "t", 1, nil, "")
	dc := &DeviceConfig{TenantID: "t", NodeID: 1, Version: 2, Draft: baseValidConfig()}
	now := time.Now().Unix()

	if !s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("first publish reported no change")
	}
	if dc.Version != 2 || dc.Published.Version != 2 {
		t.Fatalf("first publish version = %d/%d, want 2/2", dc.Version, dc.Published.Version)
	}
	if s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("identical republish reported a change")
	}
	// Same draft version, different rendered artifact: this is the prefix-list
	// edit case -- nobody saved a draft, but the box must still get a new version.
	dc.Draft.Hostname = "edge-other"
	if !s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("artifact-only change was not published")
	}
	if dc.Version != 3 || dc.Published.Version != 3 {
		t.Fatalf("artifact-only publish version = %d/%d, want 3/3", dc.Version, dc.Published.Version)
	}
}

func TestLeakStateWithoutLeakConfigs(t *testing.T) {
	st := &State{}
	if normalizeState(st) {
		t.Fatal("normalizeState reported a secret migration for an empty state")
	}
	if st.LeakConfigs == nil {
		t.Fatal("LeakConfigs not initialized")
	}
	if got := route.MaxVRFID; got != 255 {
		t.Fatalf("route.MaxVRFID = %d, the leak gate assumes 255", got)
	}

	// A device record with v0 (no draft ever saved) still shows VPN 0, unselectable
	// with the reason, instead of an empty page with no explanation.
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	view := leakGet(t, s, ten)
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %+v, want the single device", view.Nodes)
	}
	node := view.Nodes[0]
	if node.HasDraft || len(node.VPNs) != 1 || node.VPNs[0].ID != 0 {
		t.Fatalf("v0 device rows = draft:%v %+v, want just VPN 0", node.HasDraft, node.VPNs)
	}
	if node.VPNs[0].Selectable || !strings.Contains(node.VPNs[0].Note, "v0") {
		t.Fatalf("v0 VPN 0 = %+v, want unselectable with the v0 reason", node.VPNs[0])
	}
}
