// Tests for the underlay HTTP surface and its persistence.
//
// The properties here are the ones a page cannot check for itself: the gate that
// refuses every tunnel before a global configuration is published, tenant
// isolation of the whole layer (the operator sees none of it), the rule that a
// re-publish never moves a value that is already in use, the refusal to build a
// tunnel from an observation the devices never re-confirmed, and the promise
// that a preview allocates on a clone and commits nothing.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

// underlayDo drives one handler with an optional tenant session. POST always
// carries a JSON content type because the session gate demands one, even when
// the endpoint (publish) has no body.
func underlayDo(t *testing.T, h http.HandlerFunc, sid, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost || body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// underlayFixture is tenant "t" with three enrolled, observed devices that each
// report one interface owning their observed address, plus tenant "t2" with one
// device. The store is real (a temp file) because every handler saves, and the
// persistence test reloads it.
func underlayFixture(t *testing.T) (*server, string, string) {
	t.Helper()
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	now := time.Now().Unix()
	for i, ip := range []string{"198.51.100.11", "198.51.100.12", "198.51.100.13"} {
		n := addNode(s, "t", uint16(i+1), nil, ip+":40000")
		n.Serial = fmt.Sprintf("cpe-%d", i+1)
		n.MappingAt = now
		ifaces := []wire.IfaceInfo{{
			Name: "wan0", SWIfIndex: uint32(i + 1), MTU: 1500,
			AdminUp: true, LinkUp: true, Addrs: []string{ip + "/24"},
		}}
		n.Ifaces = &ifaces
		n.InventoryAt = now
	}
	other := addNode(s, "t2", 9, nil, "203.0.113.9:40000")
	other.Serial = "other"
	other.MappingAt = now
	other.Ifaces = &[]wire.IfaceInfo{{
		Name: "wan0", SWIfIndex: 9, MTU: 1500, Addrs: []string{"203.0.113.9/24"},
	}}
	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	two := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	return s, ten, two
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func settingsBody(t *testing.T, edit func(*underlay.Settings)) string {
	t.Helper()
	set := underlay.DefaultSettings()
	if edit != nil {
		edit(&set)
	}
	return mustJSON(t, set)
}

func saveUnderlaySettings(t *testing.T, s *server, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminUnderlaySettings, sid, http.MethodPut, "/api/underlay/settings", body)
}

func publishUnderlay(t *testing.T, s *server, sid string) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminUnderlayPublish, sid, http.MethodPost, "/api/underlay/settings/publish", "")
}

// publishDefault publishes the built-in settings, failing the test if the save
// or the publish does not go through.
func publishDefault(t *testing.T, s *server, sid string) {
	t.Helper()
	if rec := saveUnderlaySettings(t, s, sid, settingsBody(t, nil)); rec.Code != http.StatusOK {
		t.Fatalf("save settings = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := publishUnderlay(t, s, sid); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body.String())
	}
}

func getUnderlayDoc(t *testing.T, s *server, sid string) underlayView {
	t.Helper()
	rec := underlayDo(t, s.handleAdminUnderlay, sid, http.MethodGet, "/api/underlay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/underlay = %d (%s)", rec.Code, rec.Body.String())
	}
	var v underlayView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("underlay document: %v (%s)", err, rec.Body.String())
	}
	return v
}

func getTopology(t *testing.T, s *server, sid string) underlayTopology {
	t.Helper()
	rec := underlayDo(t, s.handleAdminUnderlayTopology, sid, http.MethodGet, "/api/underlay/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET topology = %d (%s)", rec.Code, rec.Body.String())
	}
	var v underlayTopology
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("topology document: %v (%s)", err, rec.Body.String())
	}
	return v
}

func nodeByID(t *testing.T, nodes []underlayNodeView, id uint16) underlayNodeView {
	t.Helper()
	for _, n := range nodes {
		if n.NodeID == id {
			return n
		}
	}
	t.Fatalf("device %d is not in the document (%+v)", id, nodes)
	return underlayNodeView{}
}

type tunnelReq struct {
	ID        string       `json:"id,omitempty"`
	A         underlaySpec `json:"a"`
	B         underlaySpec `json:"b"`
	Path      *pathReq     `json:"path,omitempty"`
	ClearPath bool         `json:"clear_path,omitempty"`
}

type pathReq struct {
	TemplateID string `json:"template_id,omitempty"`
	NodeID     uint16 `json:"node_id,omitempty"`
	Addr       string `json:"addr,omitempty"`
	SaveAs     string `json:"save_as,omitempty"`
}

func manualTunnel(a, b uint16, iface string) tunnelReq {
	return tunnelReq{
		A: underlaySpec{NodeID: a, Iface: iface},
		B: underlaySpec{NodeID: b, Iface: iface},
	}
}

func createTunnel(t *testing.T, s *server, sid string, req tunnelReq) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminUnderlayTunnels, sid, http.MethodPost, "/api/underlay/tunnels", mustJSON(t, req))
}

func tunnelFrom(t *testing.T, rec *httptest.ResponseRecorder) underlayTunnelView {
	t.Helper()
	var out struct {
		Tunnel underlayTunnelView `json:"tunnel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("tunnel response: %v (%s)", err, rec.Body.String())
	}
	return out.Tunnel
}

type autoReq struct {
	Nodes     []uint16 `json:"nodes"`
	Mode      string   `json:"mode"`
	Hub       uint16   `json:"hub,omitempty"`
	SpokeMode string   `json:"spoke_mode,omitempty"`
}

func runAuto(t *testing.T, s *server, sid string, apply bool, req autoReq) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/underlay/auto/plan"
	h := s.handleAdminUnderlayAutoPlan
	if apply {
		target, h = "/api/underlay/auto/apply", s.handleAdminUnderlayAutoApply
	}
	return underlayDo(t, h, sid, http.MethodPost, target, mustJSON(t, req))
}

func autoPlanFrom(t *testing.T, rec *httptest.ResponseRecorder) underlayAutoView {
	t.Helper()
	var out struct {
		Plan underlayAutoView `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("auto response: %v (%s)", err, rec.Body.String())
	}
	return out.Plan
}

// ------------------------------------------------------------------- gate

func TestUnderlayGateAndTenantIsolation(t *testing.T) {
	s, ten, other := underlayFixture(t)
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})

	if rec := underlayDo(t, s.handleAdminUnderlay, "", http.MethodGet, "/api/underlay", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}
	// The operator sees device inventory, never how a device is configured.
	if rec := underlayDo(t, s.handleAdminUnderlay, op, http.MethodGet, "/api/underlay", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET = %d, want 403", rec.Code)
	}
	if rec := saveUnderlaySettings(t, s, op, settingsBody(t, nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("operator PUT = %d, want 403", rec.Code)
	}

	// Nothing can be built before a global configuration is published: the port
	// every device listens on at the relay does not exist yet.
	rec := runAuto(t, s, ten, false, autoReq{Nodes: []uint16{1, 2}, Mode: "full-mesh"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "全局配置") {
		t.Fatalf("auto plan before publish = %d (%s), want 409 naming the missing global config", rec.Code, rec.Body.String())
	}
	rec = createTunnel(t, s, ten, manualTunnel(1, 2, "wan0"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "还没有发布全局配置") {
		t.Fatalf("tunnel before publish = %d (%s)", rec.Code, rec.Body.String())
	}

	// The overlay pool lives in its own table; overlapping a customer subnet makes
	// the next hop resolve out of the wrong table, so it is refused at save.
	s.state.Configs[configKey("t", 1)] = &DeviceConfig{
		TenantID: "t", NodeID: 1,
		Draft: BasicConfig{Ifaces: []ConfigIface{{
			VPP: "lan0", LCP: "tap0", IP: "10.250.5.1", PrefixLen: 24, VPN: 0, MTU: 1500, Up: true,
		}}},
	}
	rec = saveUnderlaySettings(t, s, ten, settingsBody(t, nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "重叠") {
		t.Fatalf("overlapping overlay pool = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if uc := s.state.Underlays["t"]; uc != nil && uc.DraftAt != 0 {
		t.Fatalf("a refused save wrote a draft anyway: %+v", uc)
	}
	delete(s.state.Configs, configKey("t", 1))

	// A valid save is a draft; nothing is published until publish is clicked.
	rec = saveUnderlaySettings(t, s, ten, settingsBody(t, func(set *underlay.Settings) {
		set.OverlayCIDR = "10.99.0.0/16"
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("save settings = %d (%s)", rec.Code, rec.Body.String())
	}
	var saved struct {
		Changes  []string     `json:"changes"`
		Underlay underlayView `json:"underlay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save response: %v", err)
	}
	if len(saved.Changes) == 0 || saved.Underlay.Configured || saved.Underlay.DraftAt == 0 {
		t.Fatalf("saved draft = %+v changes=%v", saved.Underlay, saved.Changes)
	}

	rec = publishUnderlay(t, s, ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body.String())
	}
	var pub struct {
		Underlay underlayView `json:"underlay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("publish response: %v", err)
	}
	if !pub.Underlay.Configured || pub.Underlay.PublishedAt == 0 || pub.Underlay.PublishedBy != "alice" {
		t.Fatalf("published header = %+v", pub.Underlay)
	}
	// Publishing is the moment every enrolled device gets its transit port.
	for _, id := range []uint16{1, 2, 3} {
		if n := nodeByID(t, pub.Underlay.Nodes, id); n.TransitPort == 0 {
			t.Fatalf("device %d has no transit port after publish", id)
		}
	}
	if pub.Underlay.Usage.TransitAssigned != 3 || pub.Underlay.Usage.OverlayUsed != 0 {
		t.Fatalf("usage after publish = %+v", pub.Underlay.Usage)
	}

	// Tenant isolation: t2's document is t2's world, and t's tunnel identity is
	// not addressable from it.
	rec = createTunnel(t, s, ten, manualTunnel(1, 2, "wan0"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	tun := tunnelFrom(t, rec)

	doc2 := getUnderlayDoc(t, s, other)
	if doc2.Configured || len(doc2.Tunnels) != 0 || len(doc2.Nodes) != 1 || doc2.Nodes[0].NodeID != 9 {
		t.Fatalf("t2 sees t's underlay: %+v", doc2)
	}
	rec = underlayDo(t, s.handleAdminUnderlayTunnels, other, http.MethodDelete, "/api/underlay/tunnels?id="+tun.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete = %d, want 404", rec.Code)
	}
	rec = createTunnel(t, s, other, manualTunnel(9, 1, "wan0"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("t2 referenced t's device: %d (%s)", rec.Code, rec.Body.String())
	}

	// Re-publishing may not move a transit port that is already in use. The draft
	// saves (it only affects future assignments) but the publish is refused.
	rec = saveUnderlaySettings(t, s, ten, settingsBody(t, func(set *underlay.Settings) {
		set.OverlayCIDR = "10.99.0.0/16"
		set.TransitPorts = underlay.PortRange{Min: 15001, Max: 15099}
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("save narrowed transit pool = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = publishUnderlay(t, s, ten)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "中转端口") {
		t.Fatalf("re-publish moving an assigned port = %d (%s)", rec.Code, rec.Body.String())
	}
	uc := s.state.Underlays["t"]
	if uc.State.TransitPorts[1] != 15000 || uc.State.Settings.TransitPorts.Min != 15000 {
		t.Fatalf("refused publish moved assigned values: %+v", uc.State.TransitPorts)
	}
}

// -------------------------------------------------- inventory and observations

func TestUnderlayOnlyUsesReportedInventoryAndFreshObservations(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)

	doc := getUnderlayDoc(t, s, ten)
	n1 := nodeByID(t, doc.Nodes, 1)
	if n1.Observed != "198.51.100.11" || n1.Mapping != "198.51.100.11:40000" {
		t.Fatalf("observation = %q / %q; the address half is the endpoint, the raw mapping is kept", n1.Observed, n1.Mapping)
	}
	if n1.ObservedStale || n1.ObservedAgeSec != 0 {
		t.Fatalf("fresh observation marked stale: %+v", n1)
	}
	if !n1.InventoryReported || n1.DefaultIface != "wan0" || len(n1.Ifaces) != 1 {
		t.Fatalf("inventory/default iface = %+v", n1)
	}

	// An interface whose MTU the dataplane has not reported yet is inventory, not
	// a usable egress port: nothing can be computed from MTU 0.
	ifaces := append(append([]wire.IfaceInfo(nil), (*s.state.Nodes[key("t", 1)].Ifaces)...),
		wire.IfaceInfo{Name: "wan9", MTU: 0, Addrs: []string{"198.51.100.11/24"}})
	s.state.Nodes[key("t", 1)].Ifaces = &ifaces
	// Two interfaces owning one address is not a fact to act on: no default is
	// better than a coin flip.
	s.state.Nodes[key("t", 3)].Ifaces = &[]wire.IfaceInfo{
		{Name: "wan0", MTU: 1500, Addrs: []string{"198.51.100.13/24"}},
		{Name: "wan1", MTU: 1400, Addrs: []string{"198.51.100.13/24"}},
	}

	doc = getUnderlayDoc(t, s, ten)
	if got := nodeByID(t, doc.Nodes, 3); got.DefaultIface != "" {
		t.Fatalf("ambiguous egress port chose %q", got.DefaultIface)
	}
	rec := createTunnel(t, s, ten, tunnelReq{
		A: underlaySpec{NodeID: 1, Iface: "wan0"},
		B: underlaySpec{NodeID: 3},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "需要选择物理端口") {
		t.Fatalf("no default iface = %d (%s)", rec.Code, rec.Body.String())
	}
	// An inventory that exists but does not include the named port is refused: the
	// agent's report is the authority, not a typo that happens to look plausible.
	rec = createTunnel(t, s, ten, tunnelReq{
		A: underlaySpec{NodeID: 1, Iface: "wan9"},
		B: underlaySpec{NodeID: 2, Iface: "wan0"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "没有接口") {
		t.Fatalf("MTU-0 interface accepted = %d (%s)", rec.Code, rec.Body.String())
	}

	// A stale observation stays visible with its age -- it is still the last thing
	// the reflector saw -- but it may not become a tunnel endpoint.
	s.state.Nodes[key("t", 2)].MappingAt = time.Now().Unix() - 600
	doc = getUnderlayDoc(t, s, ten)
	n2 := nodeByID(t, doc.Nodes, 2)
	if !n2.ObservedStale || n2.Observed != "198.51.100.12" || n2.ObservedAgeSec < 599 {
		t.Fatalf("stale observation = %+v", n2)
	}
	rec = createTunnel(t, s, ten, manualTunnel(1, 2, "wan0"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "已过期") {
		t.Fatalf("tunnel on a stale observation = %d (%s)", rec.Code, rec.Body.String())
	}
	// An explicitly typed address is the documented way out.
	addr := "198.51.100.12"
	req := manualTunnel(1, 2, "wan0")
	req.B.Addr = &addr
	rec = createTunnel(t, s, ten, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("explicit address over a stale observation = %d (%s)", rec.Code, rec.Body.String())
	}
	if tun := tunnelFrom(t, rec); tun.B.Addr != addr || tun.B.MatchesObservation {
		t.Fatalf("endpoint = %+v; a typed address is not an observation match", tun.B)
	}
}

// ------------------------------------------------------------------ auto batch

func TestUnderlayAutoPreviewCommitsNothingAndApplyDedups(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	st := s.state.Underlays["t"].State

	req := autoReq{Nodes: []uint16{1, 2, 3}, Mode: "full-mesh"}
	rec := runAuto(t, s, ten, false, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto plan = %d (%s)", rec.Code, rec.Body.String())
	}
	plan := autoPlanFrom(t, rec)
	if len(plan.Add) != 3 || len(plan.Skip) != 0 || plan.Pairs != 3 {
		t.Fatalf("plan = %d add, %d skip, %d pairs", len(plan.Add), len(plan.Skip), plan.Pairs)
	}
	if plan.Usage.OverlayUsed != 0 || plan.OverlayAfter != 6 {
		t.Fatalf("preview usage = %d -> %d, want 0 -> 6", plan.Usage.OverlayUsed, plan.OverlayAfter)
	}
	for _, tun := range plan.Add {
		if tun.A.Overlay == "" || tun.B.Overlay == "" || tun.State != "unknown" {
			t.Fatalf("preview tunnel = %+v", tun)
		}
	}
	for _, n := range plan.PerNode {
		if n.Tunnels != 2 {
			t.Fatalf("preview per-node count = %+v; full mesh gives every device 2", n)
		}
	}
	// The preview ran on a clone: not one port or /32 was committed.
	if len(st.Tunnels) != 0 || st.OverlayNext != 0 {
		t.Fatalf("preview committed state: %d tunnels, overlay_next %d", len(st.Tunnels), st.OverlayNext)
	}

	rec = runAuto(t, s, ten, true, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto apply = %d (%s)", rec.Code, rec.Body.String())
	}
	plan = autoPlanFrom(t, rec)
	if len(plan.Add) != 3 || plan.Pairs != 3 {
		t.Fatalf("apply plan = %d add", len(plan.Add))
	}
	if len(st.Tunnels) != 3 {
		t.Fatalf("apply stored %d tunnels", len(st.Tunnels))
	}
	var batch string
	for _, tun := range st.Tunnels {
		if tun.BatchID == "" {
			t.Fatalf("tunnel %s has no batch id; a batch must be traceable", tun.ID)
		}
		if batch == "" {
			batch = tun.BatchID
		}
		if tun.BatchID != batch {
			t.Fatalf("one batch stamped several ids: %s vs %s", tun.BatchID, batch)
		}
	}

	// Running the same batch again adds nothing: the pair is the dedup key, and a
	// second tunnel between two devices is a manual act.
	rec = runAuto(t, s, ten, true, req)
	plan = autoPlanFrom(t, rec)
	if len(plan.Add) != 0 || len(plan.Skip) != 3 || !strings.Contains(plan.Skip[0].Reason, "已经有隧道") {
		t.Fatalf("second apply = %d add, %d skip (%+v)", len(plan.Add), len(plan.Skip), plan.Skip)
	}
	// The apply result must not count the batch twice.
	if plan.Usage.OverlayUsed != 6 || plan.OverlayAfter != 6 {
		t.Fatalf("apply usage = %d -> %d, want 6 -> 6", plan.Usage.OverlayUsed, plan.OverlayAfter)
	}

	doc := getUnderlayDoc(t, s, ten)
	if len(doc.Tunnels) != 3 {
		t.Fatalf("document lists %d tunnels", len(doc.Tunnels))
	}
	for _, tun := range doc.Tunnels {
		if tun.State != "unknown" {
			t.Fatalf("tunnel state %q: nothing is delivered yet, so no measurement exists", tun.State)
		}
	}
	if doc.StateNote == "" || !strings.Contains(doc.StateNote, "未知") {
		t.Fatalf("missing state note: %q", doc.StateNote)
	}
}

func TestUnderlayRelayPreviewNamesTheTemplateItWillCreate(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	st := s.state.Underlays["t"].State

	req := autoReq{Nodes: []uint16{1, 2, 3}, Mode: "hub-spoke", Hub: 1, SpokeMode: "via-hub"}
	rec := runAuto(t, s, ten, false, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay plan = %d (%s)", rec.Code, rec.Body.String())
	}
	plan := autoPlanFrom(t, rec)
	if len(plan.Add) != 3 {
		t.Fatalf("via-hub plan = %d tunnels", len(plan.Add))
	}
	var relayed *underlayTunnelView
	for i := range plan.Add {
		if plan.Add[i].Path != nil {
			relayed = &plan.Add[i]
		}
	}
	if relayed == nil {
		t.Fatalf("no tunnel in the preview carries the hub path: %+v", plan.Add)
	}
	if relayed.Path.TemplateName != "经 cpe-1 中转" || relayed.PathError != "" {
		t.Fatalf("preview relay = %+v / error %q; the batch's own template must resolve", relayed.Path, relayed.PathError)
	}
	// The preview did not create the template; the apply does.
	if len(st.Paths) != 0 {
		t.Fatalf("preview created %d template(s)", len(st.Paths))
	}

	rec = runAuto(t, s, ten, true, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay apply = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(st.Tunnels) != 3 || len(st.Paths) != 1 {
		t.Fatalf("after apply: %d tunnels, %d templates", len(st.Tunnels), len(st.Paths))
	}
	var relayedID, templateID string
	for _, tun := range st.Tunnels {
		if tun.Path != nil {
			relayedID, templateID = tun.ID, tun.Path.TemplateID
		}
	}
	if relayedID == "" || templateID == "" {
		t.Fatalf("stored relay tunnel lost its path: %+v", st.Tunnels)
	}

	// A template in use cannot be deleted: that would silently turn every
	// referring tunnel back into a direct one. The refusal names the users.
	rec = underlayDo(t, s.handleAdminUnderlayPaths, ten, http.MethodDelete, "/api/underlay/paths?id="+templateID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in-use template = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	var refused struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || len(refused.Users) != 1 || refused.Users[0] != relayedID {
		t.Fatalf("refusal users = %+v (%v)", refused.Users, err)
	}

	rec = underlayDo(t, s.handleAdminUnderlayTunnels, ten, http.MethodDelete, "/api/underlay/tunnels?id="+relayedID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete relay tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = underlayDo(t, s.handleAdminUnderlayPaths, ten, http.MethodDelete, "/api/underlay/paths?id="+templateID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete unused template = %d (%s)", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------------ topology

// One edge per device pair. A second tunnel on the same pair is refused with a
// conflict rather than drawn as a parallel edge: one static WG key per device
// makes it uninstallable in the dataplane (-116).
func TestUnderlayTopologyBundlesOneTunnelPerPairAndShowsRelays(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)

	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := createTunnel(t, s, ten, manualTunnel(2, 1, "wan0"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("同一对设备的第二条隧道应回 409，实际 %d (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "只能有一条隧道") {
		t.Fatalf("拒绝里没说清原因: %s", body)
	}
	rec = createTunnel(t, s, ten, tunnelReq{
		A:    underlaySpec{NodeID: 2, Iface: "wan0"},
		B:    underlaySpec{NodeID: 3, Iface: "wan0"},
		Path: &pathReq{NodeID: 1},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("relayed tunnel = %d (%s)", rec.Code, rec.Body.String())
	}

	topo := getTopology(t, s, ten)
	if !topo.Configured || topo.StateNote == "" {
		t.Fatalf("topology header = configured %v, note %q", topo.Configured, topo.StateNote)
	}
	if len(topo.Nodes) != 3 {
		t.Fatalf("topology has %d nodes", len(topo.Nodes))
	}
	if len(topo.Bundles) != 2 {
		t.Fatalf("topology has %d bundles; two pairs are two edges", len(topo.Bundles))
	}
	bundle := topo.Bundles[0]
	if bundle.A != 1 || bundle.B != 2 || bundle.Count != 1 || len(bundle.Tunnels) != 1 {
		t.Fatalf("first bundle = %+v", bundle)
	}
	if bundle.State != "unknown" {
		t.Fatalf("bundle state = %q", bundle.State)
	}
	relayBundle := topo.Bundles[1]
	if relayBundle.A != 2 || relayBundle.B != 3 || relayBundle.Tunnels[0].Path == nil ||
		relayBundle.Tunnels[0].Path.NodeID != 1 || relayBundle.Tunnels[0].Path.Serial != "cpe-1" {
		t.Fatalf("relay bundle = %+v", relayBundle)
	}

	// A relay-only device has no edge of its own, so it is visible on the graph
	// only through these per-node counts.
	n1 := nodeByID(t, topo.Nodes, 1)
	if n1.TunnelCount != 1 || n1.RelayFor != 1 || n1.TransitPort != 15000 || len(n1.Ports) != 1 {
		t.Fatalf("relay device row = %+v", n1)
	}
	if n2 := nodeByID(t, topo.Nodes, 2); n2.TunnelCount != 2 {
		t.Fatalf("device 2 tunnel count = %d, want 2", n2.TunnelCount)
	}
}

// --------------------------------------------------------------- persistence

func TestUnderlayPersistsThroughAnAtomicReload(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)

	rec := createTunnel(t, s, ten, tunnelReq{
		A:    underlaySpec{NodeID: 1, Iface: "wan0"},
		B:    underlaySpec{NodeID: 2, Iface: "wan0"},
		Path: &pathReq{NodeID: 3, SaveAs: "经 cpe-3 中转"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create relayed tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	tun := tunnelFrom(t, rec)

	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	uc := st.Underlays["t"]
	if uc == nil || uc.State == nil {
		t.Fatalf("underlay row did not survive: %+v", uc)
	}
	if uc.PublishedBy != "alice" || !uc.State.Configured() || uc.Draft.OverlayCIDR != underlay.DefaultSettings().OverlayCIDR {
		t.Fatalf("published header did not survive: %+v", uc)
	}
	if len(uc.State.Tunnels) != 1 || len(uc.State.Paths) != 1 {
		t.Fatalf("reloaded %d tunnels and %d templates", len(uc.State.Tunnels), len(uc.State.Paths))
	}
	saved, ok := uc.State.Tunnels[tun.ID]
	if !ok || saved.A.Port != tun.A.Port || saved.B.Overlay != tun.B.Overlay || saved.Path == nil {
		t.Fatalf("reloaded tunnel = %+v", saved)
	}
	if _, ok := uc.State.Paths[saved.Path.TemplateID]; !ok {
		t.Fatalf("the tunnel's template reference broke across a reload: %+v", saved.Path)
	}
	for _, id := range []uint16{1, 2, 3} {
		if uc.State.TransitPorts[id] != 15000 {
			t.Fatalf("transit port of device %d = %d after reload", id, uc.State.TransitPorts[id])
		}
	}
	if uc.State.OverlayNext != 2 {
		t.Fatalf("overlay cursor = %d after reload, want 2", uc.State.OverlayNext)
	}

	// A file written before this layer existed has no "underlays" key at all; it
	// must load as an empty map, not as a nil map that panics on the first save.
	legacy := &fileStore{path: filepath.Join(t.TempDir(), "legacy.json")}
	if err := os.WriteFile(legacy.path, []byte(`{"tenants":{},"nodes":{}}`), 0600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	old, err := legacy.Load()
	if err != nil {
		t.Fatalf("load legacy state: %v", err)
	}
	if old.Underlays == nil || len(old.Underlays) != 0 {
		t.Fatalf("legacy state loaded with underlays %+v", old.Underlays)
	}

	// A row whose tenant id disagrees with its key would hand one tenant another's
	// tunnels; it is dropped, while a well-keyed row is repaired in place.
	mixed := &fileStore{path: filepath.Join(t.TempDir(), "mixed.json")}
	body := `{"underlays":{"t":{"tenant_id":"t2"},"x":{"tenant_id":"x"}}}`
	if err := os.WriteFile(mixed.path, []byte(body), 0600); err != nil {
		t.Fatalf("write mixed state: %v", err)
	}
	repaired, err := mixed.Load()
	if err != nil {
		t.Fatalf("load mixed state: %v", err)
	}
	if _, ok := repaired.Underlays["t"]; ok {
		t.Fatalf("a row naming another tenant was kept: %+v", repaired.Underlays["t"])
	}
	x := repaired.Underlays["x"]
	if x == nil || x.State == nil || x.Draft.OverlayCIDR == "" || x.State.Tunnels == nil {
		t.Fatalf("a well-keyed row was not repaired: %+v", x)
	}
}
