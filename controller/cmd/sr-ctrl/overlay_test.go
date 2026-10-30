// Tests for the overlay layer's model: 导通组 CRUD, the RT allocator, and the
// per-device derivation the console and (in O6-b) route.json both read.
//
// The properties worth pinning down are the ones a screenshot cannot show:
//
//   - an RT is allocated once, stays unique inside the tenant, is never re-minted
//     by an edit, and is never handed back after a delete -- because a device may
//     still hold routes announced under the old value.
//   - removing a member makes its RT disappear from that device's table because
//     nothing references it any more. That is the whole reason the stored shape is
//     a group and not a per-VPN RT list, so this test would fail the day the
//     derivation grows a cleanup path instead.
//   - the gates that would otherwise only surface as a device refusing route.json
//     (VPN 号宽、保留表、别人的设备、一条路由最多带多少个 RT).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/route"
)

// overlayFixture is tenant "t" with three devices that each carry VPNs 0, 10 and
// 20 -- device 1 also carries the reserved 250 and an out-of-range 300, so the
// gates have something real to refuse -- plus tenant "t2" with one device.
func overlayFixture(t *testing.T) (*server, string, string) {
	t.Helper()
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}

	vpnRows := func(ids ...uint32) []ConfigVPN {
		out := []ConfigVPN{{ID: 0, Name: "default"}}
		for _, id := range ids {
			out = append(out, ConfigVPN{ID: id, Name: fmt.Sprintf("VPN-%d", id)})
		}
		return out
	}
	put := func(tenant string, id uint16, vpns []ConfigVPN) {
		rec := addNode(s, tenant, id, nil, fmt.Sprintf("198.51.100.%d:40000", id))
		rec.Serial = fmt.Sprintf("cpe-%d", id)
		s.state.Configs[configKey(tenant, id)] = &DeviceConfig{
			TenantID: tenant, NodeID: id,
			Draft: BasicConfig{VPNs: vpns, Ifaces: []ConfigIface{}, Statics: []ConfigStatic{}},
		}
	}
	put("t", 1, vpnRows(10, 20, vpnReservedOverlay, 300))
	put("t", 2, vpnRows(10, 20))
	put("t", 3, vpnRows(10, 20))
	put("t2", 9, vpnRows(10))

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	two := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	return s, ten, two
}

func overlayDo(t *testing.T, s *server, method, target, body, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	switch r.URL.Path {
	case "/api/overlay":
		s.handleAdminOverlay(rec, r)
	case "/api/overlay/settings":
		s.handleAdminOverlaySettings(rec, r)
	case "/api/overlay/group":
		s.handleAdminOverlayGroup(rec, r)
	default:
		t.Fatalf("no overlay handler for %s", r.URL.Path)
	}
	return rec
}

func overlayBody(name string, members ...OverlayMember) string {
	b, _ := json.Marshal(overlayGroupRequest{Name: name, Members: members})
	return string(b)
}

// createGroup posts a group and returns the response plus the group the
// controller stored (zero value on a refused request).
func createGroup(t *testing.T, s *server, sid, name string, members ...OverlayMember) (*httptest.ResponseRecorder, OverlayGroup) {
	t.Helper()
	rec := overlayDo(t, s, http.MethodPost, "/api/overlay/group", overlayBody(name, members...), sid)
	var g OverlayGroup
	if rec.Code == http.StatusCreated {
		var out struct {
			Group OverlayGroup `json:"group"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode create response: %v (%s)", err, rec.Body.String())
		}
		g = out.Group
	}
	return rec, g
}

func editGroup(t *testing.T, s *server, sid, id, name string, members ...OverlayMember) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(overlayGroupRequest{ID: id, Name: name, Members: members})
	return overlayDo(t, s, http.MethodPut, "/api/overlay/group", string(b), sid)
}

func overlayGetView(t *testing.T, s *server, sid string) overlayView {
	t.Helper()
	rec := overlayDo(t, s, http.MethodGet, "/api/overlay", "", sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/overlay = %d (%s)", rec.Code, rec.Body.String())
	}
	var v overlayView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	return v
}

// derivedRTs reads one (device, VPN)'s export/import sets back out of the view --
// the same arithmetic O6-b will serialize into route.json, rather than a private
// helper the page never sees.
func derivedRTs(t *testing.T, v overlayView, nodeID uint16, vpnID uint32) overlayVPNRow {
	t.Helper()
	for _, n := range v.Nodes {
		if n.NodeID != nodeID {
			continue
		}
		for _, row := range n.VPNs {
			if row.ID == vpnID {
				return row
			}
		}
		t.Fatalf("node %d has no VPN %d in the view", nodeID, vpnID)
	}
	t.Fatalf("node %d is not in the view", nodeID)
	return overlayVPNRow{}
}

func mustEmpty(t *testing.T, label string, n int) {
	t.Helper()
	if n != 0 {
		t.Fatalf("%s = %d entries, want none", label, n)
	}
}

func mustHave(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("set = %v, want exactly %v", got, want)
		}
	}
}

func TestOverlayCreateAllocatesRTsFromTheDocumentedFloor(t *testing.T) {
	s, sid, _ := overlayFixture(t)

	rec, g := createGroup(t, s, sid, "办公互访",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	// 65000:1000 is the floor that keeps the allocator out of lab's hand-written
	// range (65000:56/57); pinning it is the point of the constant.
	if g.RT != "65000:1000" {
		t.Fatalf("first RT = %q, want 65000:1000", g.RT)
	}
	if g.ID != "og1" {
		t.Fatalf("first id = %q, want og1", g.ID)
	}
	rec2, g2 := createGroup(t, s, sid, "生产互访",
		OverlayMember{NodeID: 2, VPNID: 20}, OverlayMember{NodeID: 3, VPNID: 20})
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second create = %d (%s)", rec2.Code, rec2.Body.String())
	}
	if g2.RT == g.RT {
		t.Fatalf("two groups share RT %q", g.RT)
	}

	v := overlayGetView(t, s, sid)
	row1 := derivedRTs(t, v, 1, 10)
	mustHave(t, row1.ExportRTs, g.RT)
	mustHave(t, row1.ImportRTs, g.RT)
	mustHave(t, row1.Groups, g.ID)

	// Device 2 is in both groups, on two different VPNs: each VPN carries only its
	// own group's RT, so the two domains stay separate inside one box.
	row2 := derivedRTs(t, v, 2, 10)
	mustHave(t, row2.ExportRTs, g.RT)
	mustHave(t, row2.Groups, g.ID)
	mustHave(t, derivedRTs(t, v, 2, 20).ExportRTs, g2.RT)

	// A VPN nobody put in a group has no RTs at all: it announces nothing
	// (route.SkipNoExportRTs), which is the honest state and not an accident.
	row3 := derivedRTs(t, v, 3, 10)
	mustHave(t, row3.ExportRTs)
	mustHave(t, row3.ImportRTs)
	mustHave(t, row3.Groups)
}

func TestOverlayTwoGroupsOnOneVPNAccumulate(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, a := createGroup(t, s, sid, "A",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	_, b := createGroup(t, s, sid, "B",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 3, VPNID: 10})
	if a.RT == b.RT {
		t.Fatalf("both groups got %q", a.RT)
	}
	v := overlayGetView(t, s, sid)
	row := derivedRTs(t, v, 1, 10)
	// Sorted by string, and 1000 < 1001, so this is also the allocation order.
	mustHave(t, row.ExportRTs, a.RT, b.RT)
	mustHave(t, row.ImportRTs, a.RT, b.RT)
	mustHave(t, row.Groups, a.ID, b.ID)
}

// TestOverlayEditDropsStaleRT is the reason the model is a group and not a list of
// RTs per VPN: the removed device must end up with nothing left, with no cleanup
// step anywhere that could forget to run.
func TestOverlayEditDropsStaleRT(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, g := createGroup(t, s, sid, "三台",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10}, OverlayMember{NodeID: 3, VPNID: 10})

	rec := editGroup(t, s, sid, g.ID, "三台", OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Group OverlayGroup `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode edit response: %v", err)
	}
	if out.Group.RT != g.RT {
		t.Fatalf("edit changed the RT: %q -> %q", g.RT, out.Group.RT)
	}
	want := []OverlayMember{{NodeID: 1, VPNID: 10}, {NodeID: 2, VPNID: 10}}
	if len(out.Group.Members) != len(want) {
		t.Fatalf("members after edit = %v, want %v", out.Group.Members, want)
	}
	for i := range want {
		if out.Group.Members[i] != want[i] {
			t.Fatalf("members after edit = %v, want %v", out.Group.Members, want)
		}
	}

	v := overlayGetView(t, s, sid)
	mustHave(t, derivedRTs(t, v, 3, 10).ExportRTs)
	kept := derivedRTs(t, v, 1, 10)
	mustHave(t, kept.ExportRTs, g.RT)
	mustHave(t, kept.Groups, g.ID)
}

// TestOverlayEditRejectsDuplicateRow: a duplicated member means the page sent what
// the user did not click. Dropping it quietly would leave a stored group whose
// row count differs from the form.
func TestOverlayEditRejectsDuplicateRow(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, g := createGroup(t, s, sid, "两台", OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	rec := editGroup(t, s, sid, g.ID, "两台",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10}, OverlayMember{NodeID: 1, VPNID: 10})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate row = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "成员重复") {
		t.Fatalf("response does not name the duplicate: %s", rec.Body.String())
	}
	if n := len(s.overlayConfigLocked("t", false).Groups[0].Members); n != 2 {
		t.Fatalf("a refused edit rewrote the group to %d members", n)
	}
}

func TestOverlayDeleteBurnsTheRT(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, first := createGroup(t, s, sid, "旧的", OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	rec := overlayDo(t, s, http.MethodDelete, "/api/overlay/group?id="+first.ID, "", sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s)", rec.Code, rec.Body.String())
	}
	v := overlayGetView(t, s, sid)
	mustEmpty(t, "groups", len(v.Groups))
	mustHave(t, derivedRTs(t, v, 1, 10).ExportRTs)

	_, next := createGroup(t, s, sid, "新的", OverlayMember{NodeID: 1, VPNID: 20}, OverlayMember{NodeID: 2, VPNID: 20})
	if next.RT == first.RT {
		t.Fatalf("the allocator handed back a deleted RT (%q): a device may still hold routes under it", next.RT)
	}
	if next.ID == first.ID {
		t.Fatalf("group id reused: %q", next.ID)
	}
	if rec := overlayDo(t, s, http.MethodDelete, "/api/overlay/group?id=og-nope", "", sid); rec.Code != http.StatusNotFound {
		t.Fatalf("delete of an unknown id = %d, want 404", rec.Code)
	}
}

func TestOverlayGates(t *testing.T) {
	cases := []struct {
		name    string
		group   string
		members []OverlayMember
		want    string
	}{
		{
			name: "一台设备不算导通", group: "单台",
			members: []OverlayMember{{NodeID: 1, VPNID: 10}},
			want:    "至少要有两台",
		},
		{
			name: "没有名字", group: "  ",
			members: []OverlayMember{{NodeID: 1, VPNID: 10}, {NodeID: 2, VPNID: 10}},
			want:    "要有名字",
		},
		{
			name: "基础配置里没有的VPN", group: "不存在",
			members: []OverlayMember{{NodeID: 1, VPNID: 77}, {NodeID: 2, VPNID: 77}},
			want:    "没有 VPN 77",
		},
		{
			name: "overlay保留表不能进组", group: "保留",
			members: []OverlayMember{{NodeID: 1, VPNID: vpnReservedOverlay}, {NodeID: 2, VPNID: 10}},
			want:    "overlay 保留表",
		},
		{
			name: "超出VRFid宽度", group: "超宽",
			members: []OverlayMember{{NodeID: 1, VPNID: 300}, {NodeID: 2, VPNID: 10}},
			want:    "超出范围",
		},
		{
			name: "别的租户的设备", group: "偷设备",
			members: []OverlayMember{{NodeID: 1, VPNID: 10}, {NodeID: 9, VPNID: 10}},
			want:    "不属于这个租户",
		},
		{
			name: "还没入网的设备号", group: "空号",
			members: []OverlayMember{{NodeID: 1, VPNID: 10}, {NodeID: 4242, VPNID: 10}},
			want:    "还没入网",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sid, _ := overlayFixture(t)
			rec, _ := createGroup(t, s, sid, tc.group, tc.members...)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("response lacks %q: %s", tc.want, rec.Body.String())
			}
			if n := len(s.overlayConfigLocked("t", false).Groups); n != 0 {
				t.Fatalf("a refused group was still stored (%d groups)", n)
			}
		})
	}
}

// TestOverlayRTCapPerVPN pins a number that comes from the wire, not from this
// package: route.MaxRTsPerRoute bounds how many RTs one announce may carry, so a
// VPN cannot sit in an unlimited number of groups.
func TestOverlayRTCapPerVPN(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	for i := 0; i < route.MaxRTsPerRoute; i++ {
		rec, _ := createGroup(t, s, sid, fmt.Sprintf("组%d", i),
			OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
		if rec.Code != http.StatusCreated {
			t.Fatalf("group %d = %d (%s)", i, rec.Code, rec.Body.String())
		}
	}
	rec, _ := createGroup(t, s, sid, "溢出的一组",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 3, VPNID: 20})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("one group too many = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "最多带") {
		t.Fatalf("response does not name the RT cap: %s", rec.Body.String())
	}
	// The refused group's other member is untouched, and the view reports the cap it
	// enforced so the page does not have to guess it.
	v := overlayGetView(t, s, sid)
	mustHave(t, derivedRTs(t, v, 3, 20).ExportRTs)
	if v.MaxRTsPerVPN != route.MaxRTsPerRoute {
		t.Fatalf("view MaxRTsPerVPN = %d, want %d", v.MaxRTsPerVPN, route.MaxRTsPerRoute)
	}
}

// TestOverlayMissingTunnelIsAWarning: 配导通 and 建隧道 are two pages, and the
// normal order of work puts the group first. Blocking the save would be wrong;
// saying nothing would hide the measured fact (draft §8.4, O5-c) that the
// reflector only reflects over bidirectional tunnel adjacency and never looks at
// RTs -- so such a pair simply never converges.
func TestOverlayMissingTunnelIsAWarning(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, g := createGroup(t, s, sid, "没隧道",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	if len(g.Members) != 2 {
		t.Fatalf("group not stored: %+v", g)
	}
	warnings := func(v overlayView) int {
		n := 0
		for _, c := range v.Checks {
			if c.Severity == "error" {
				t.Fatalf("a group with no tunnel produced an error: %s", c.Text)
			}
			if c.Severity == "warning" && strings.Contains(c.Text, "没有隧道") {
				n++
			}
		}
		return n
	}
	if got := warnings(overlayGetView(t, s, sid)); got != 1 {
		t.Fatalf("warnings for one pair = %d, want 1", got)
	}
	// A triangle needs three pairs, so the count is the pairwise gap, not the
	// member count.
	if _, g3 := createGroup(t, s, sid, "三台没隧道",
		OverlayMember{NodeID: 1, VPNID: 20}, OverlayMember{NodeID: 2, VPNID: 20}, OverlayMember{NodeID: 3, VPNID: 20}); len(g3.Members) != 3 {
		t.Fatalf("second group not stored: %+v", g3)
	}
	if got := warnings(overlayGetView(t, s, sid)); got != 4 {
		t.Fatalf("warnings for a pair plus a triangle = %d, want 4", got)
	}
}

func TestOverlayEscapeSwitchRoundTrips(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	rec := overlayDo(t, s, http.MethodPut, "/api/overlay/settings", `{"escape_all_colors":true}`, sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings = %d (%s)", rec.Code, rec.Body.String())
	}
	if !s.overlayConfigLocked("t", false).EscapeAllColors {
		t.Fatal("escape switch not stored")
	}
	if !overlayGetView(t, s, sid).EscapeAllColors {
		t.Fatal("view does not report the escape switch")
	}
	// A missing field must not be read as "turn it off": the page sends the field
	// only when the checkbox changes, and a silent off here changes the data plane.
	if rec := overlayDo(t, s, http.MethodPut, "/api/overlay/settings", `{}`, sid); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT settings without the field = %d, want 400", rec.Code)
	}
	if !s.overlayConfigLocked("t", false).EscapeAllColors {
		t.Fatal("an empty body cleared the escape switch")
	}
}

func TestOverlayTenantIsolation(t *testing.T) {
	s, sid, other := overlayFixture(t)
	createGroup(t, s, sid, "A 的组", OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})

	v := overlayGetView(t, s, other)
	mustEmpty(t, "groups", len(v.Groups))
	if len(v.Nodes) != 1 || v.Nodes[0].NodeID != 9 {
		t.Fatalf("tenant t2 sees the wrong devices: %v", v.Nodes)
	}
	rec, _ := createGroup(t, s, other, "偷来的组", OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 9, VPNID: 10})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant group = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "root", Via: "password"})
	if rec := overlayDo(t, s, http.MethodGet, "/api/overlay", "", op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET /api/overlay = %d, want 403", rec.Code)
	}
}

// TestOverlayPersistsAcrossRestart: the RT has to survive a restart, because the
// device reads its own copy when the controller is down (定案 D1) and the
// allocator must not hand out a value already installed somewhere.
func TestOverlayPersistsAcrossRestart(t *testing.T) {
	s, sid, _ := overlayFixture(t)
	_, g := createGroup(t, s, sid, "重启后还在",
		OverlayMember{NodeID: 1, VPNID: 10}, OverlayMember{NodeID: 2, VPNID: 10})
	reloaded, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	oc := reloaded.Overlays["t"]
	if oc == nil || len(oc.Groups) != 1 {
		t.Fatalf("groups did not persist: %+v", oc)
	}
	if oc.Groups[0].RT != g.RT {
		t.Fatalf("RT changed on reload: %q -> %q", g.RT, oc.Groups[0].RT)
	}
	if oc.NextRT <= overlayRTFirst {
		t.Fatalf("NextRT = %d after allocating %s, must be past it", oc.NextRT, g.RT)
	}
	rt, err := oc.allocateRT()
	if err != nil {
		t.Fatalf("allocate after reload: %v", err)
	}
	if rt == g.RT {
		t.Fatalf("a restarted allocator would reissue the live RT %q", rt)
	}
}

// TestOverlayStateFileWithoutOverlays is the migration half: a state file from
// before this layer exists must load, and a record with no counters must start at
// the allocator's floor rather than issuing 65000:0.
func TestOverlayStateFileWithoutOverlays(t *testing.T) {
	st := &State{Nodes: map[string]*NodeRecord{}, Tenants: map[string]*Tenant{"t": {ID: "t"}}}
	normalizeState(st)
	if st.Overlays == nil {
		t.Fatal("normalizeState left Overlays nil")
	}
	st.Overlays["t"] = &OverlayConfig{TenantID: "t", Groups: []OverlayGroup{{ID: "og0", RT: "65000:1000"}}}
	normalizeState(st)
	oc := st.Overlays["t"]
	if oc.NextRT != overlayRTFirst {
		t.Fatalf("NextRT after migration = %d, want the floor %d", oc.NextRT, overlayRTFirst)
	}
	rt, err := oc.allocateRT()
	if err != nil {
		t.Fatalf("allocate after migration: %v", err)
	}
	if rt != "65000:1001" {
		t.Fatalf("allocated %q, want 65000:1001 (the counter skips what a group already holds)", rt)
	}
}
