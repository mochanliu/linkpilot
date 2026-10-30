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
	Peers     []uint16     `json:"peers,omitempty"`
	Path      *pathReq     `json:"path,omitempty"`
	ClearPath bool         `json:"clear_path,omitempty"`
	Color     *uint8       `json:"color,omitempty"`
}

type hopReq struct {
	NodeID uint16 `json:"node_id,omitempty"`
	Addr   string `json:"addr,omitempty"`
}

type pathReq struct {
	TemplateID string   `json:"template_id,omitempty"`
	Hops       []hopReq `json:"hops,omitempty"`
	SaveAs     string   `json:"save_as,omitempty"`
}

// via names the relay chain in submission order; the API must not reorder it.
func via(nodes ...uint16) *pathReq {
	p := &pathReq{}
	for _, n := range nodes {
		p.Hops = append(p.Hops, hopReq{NodeID: n})
	}
	return p
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

// ------------------------------------------------------------------ 批量建隧道

// fanResp is what POST /api/underlay/tunnels answers for a batch: the tunnels it
// built, the peers it refused (with the reason each would have failed), and the
// batch id that labels them all.
type fanResp struct {
	OK       bool                 `json:"ok"`
	BatchID  string               `json:"batch_id"`
	Tunnels  []underlayTunnelView `json:"tunnels"`
	Skipped  []underlay.FanSkip   `json:"skipped"`
	Underlay underlayView         `json:"underlay"`
}

func fanTunnels(t *testing.T, s *server, sid string, req tunnelReq) fanResp {
	t.Helper()
	rec := createTunnel(t, s, sid, req)
	// 200, not 201: a batch that skipped a peer is a completed request. A page
	// that tested for 201 would read "5 built, 1 already existed" as a failure and
	// roll its own display back.
	if rec.Code != http.StatusOK {
		t.Fatalf("批量 = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var out fanResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("批量响应: %v (%s)", err, rec.Body.String())
	}
	if !out.OK {
		t.Fatalf("批量 ok=false: %s", rec.Body.String())
	}
	return out
}

// 本端单选、对端多选、共用一条路径 -- 「从 CPE A 经同一条路径到 B C D E F」。
// What the endpoint owes the page beyond a count: a batch id to point at, and the
// SAME chain in the SAME order on every tunnel. A per-tunnel re-sort would make
// the topology draw a route the dataplane does not take.
func TestFanEndpointBuildsOneTunnelPerPeerOnTheSharedPath(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	// 中转设备在发布之后才入网：批量建隧道要给晚到的设备补中转端口，
	// 否则整批隧道都下发不了。
	enrollExtra(t, s, 4, "198.51.100.14")
	enrollExtra(t, s, 5, "198.51.100.15")

	out := fanTunnels(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		Peers: []uint16{3, 2, 3},
		Path:  via(4, 5),
	})
	if out.BatchID == "" {
		t.Fatal("批量没有返回 batch_id，页面无法说明这几条隧道是同一批建的")
	}
	// 三个对端里有一个重复：去重后两条，按设备号排列（点选顺序不是隧道顺序）。
	if len(out.Tunnels) != 2 || len(out.Skipped) != 0 {
		t.Fatalf("built %d / skipped %+v, want 2 条且没有跳过", len(out.Tunnels), out.Skipped)
	}
	for i, wantPeer := range []uint16{2, 3} {
		tv := out.Tunnels[i]
		if tv.A.NodeID != 1 || tv.B.NodeID != wantPeer || tv.BatchID != out.BatchID {
			t.Fatalf("第 %d 条 = A %d / B %d / batch %q，本端和对端都属于这一批",
				i, tv.A.NodeID, tv.B.NodeID, tv.BatchID)
		}
		p := tv.Path
		if p == nil || tv.PathError != "" {
			t.Fatalf("第 %d 条没带上可解析的路径: %+v / %q", i, p, tv.PathError)
		}
		if len(p.Hops) != 2 || p.Hops[0].NodeID != 4 || p.Hops[0].Serial != "cpe-4" ||
			p.Hops[1].NodeID != 5 || p.Hops[1].Serial != "cpe-5" {
			t.Fatalf("第 %d 条的中转链不是 4→5: %+v", i, p.Hops)
		}
	}
	// 一台设备一个监听端口：本端是同一台设备，两条隧道必须各占一个端口和一段
	// overlay，不然第二条会把第一条顶掉。
	if a, b := out.Tunnels[0], out.Tunnels[1]; a.A.Port == b.A.Port || a.B.Overlay == b.B.Overlay {
		t.Fatalf("两条隧道共用了本端端口或对端 overlay: %+v / %+v", a, b)
	}
	// 链上每台中转设备都要拿到端口。它们可以是同一个号（每台都先取池底，
	// 靠外层地址区分），但不能是 0 —— 0 意味着这一跳没人监听。
	for _, hop := range []uint16{4, 5} {
		if p := nodeByID(t, out.Underlay.Nodes, hop).TransitPort; p == 0 {
			t.Fatalf("中转设备 %d 没有分到端口", hop)
		}
	}
	if len(out.Underlay.Tunnels) != 2 {
		t.Fatalf("文档里 %d 条隧道，批量之后应只有这两条", len(out.Underlay.Tunnels))
	}
}

// 「从 A 到 B C D」而 C 早就有隧道，是一次成功加一句解释，不是一个失败请求：
// 建成的必须留下，没建成的要指名是哪台设备、为什么。
func TestFanEndpointSkipsTakenPeersAndKeepsWhatItBuilt(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")
	if rec := createTunnel(t, s, ten, manualTunnel(1, 3, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("预置 1↔3 = %d (%s)", rec.Code, rec.Body.String())
	}

	out := fanTunnels(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		Peers: []uint16{2, 3},
		Path:  via(4),
	})
	if len(out.Tunnels) != 1 || out.Tunnels[0].B.NodeID != 2 {
		t.Fatalf("该建的没建全: %+v", out.Tunnels)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].NodeID != 3 ||
		!strings.Contains(out.Skipped[0].Reason, "只能有一条隧道") {
		t.Fatalf("跳过项要指名设备 3 并说清是一对设备只能一条: %+v", out.Skipped)
	}
	// 已有那条不会因为这一批被改动或删除。
	if len(out.Underlay.Tunnels) != 2 {
		t.Fatalf("批量后文档里 %d 条隧道，应是预置那条 + 新建那条", len(out.Underlay.Tunnels))
	}
	st := s.state.Underlays["t"].State
	var direct *underlay.Tunnel
	for _, tv := range st.Tunnels {
		if tv.B.NodeID == 3 {
			direct = tv
		}
	}
	if direct == nil || direct.Path != nil {
		t.Fatalf("预置的直连隧道被批量改了: %+v", direct)
	}
}

// 对端列表和中转链来自同一张设备表，勾选时很容易把中转也当成对端。这种对端建不
// 成（隧道的一端同时当自己的中转），但其余对端照建。
func TestFanEndpointSkipsAPeerThatIsAlsoAHop(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")

	out := fanTunnels(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		Peers: []uint16{2, 4},
		Path:  via(4),
	})
	if len(out.Tunnels) != 1 || out.Tunnels[0].B.NodeID != 2 {
		t.Fatalf("对端 2 应建成: %+v", out.Tunnels)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].NodeID != 4 ||
		!strings.Contains(out.Skipped[0].Reason, "同时是隧道的一端") {
		t.Fatalf("对端 4 的跳过要说是它同时是中转: %+v", out.Skipped)
	}
}

// peers 缺省 = 「没有要批量」，不是「批量了个空」：只写本端的请求仍然要按单条
// 隧道拒绝，否则页面上「一个对端都没勾」会变成一次静默的成功。
func TestFanEndpointRefusesAnEmptyPeerListInsteadOfBuildingNothing(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	st := s.state.Underlays["t"].State

	empty := []uint16{}
	for _, peers := range [][]uint16{nil, empty} {
		rec := createTunnel(t, s, ten, tunnelReq{
			A:     underlaySpec{NodeID: 1, Iface: "wan0"},
			Peers: peers,
			Path:  via(2),
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "两端都需要指定设备") {
			t.Fatalf("peers=%v 应回 400 说缺少对端，实际 %d (%s)", peers, rec.Code, rec.Body.String())
		}
	}
	if len(st.Tunnels) != 0 {
		t.Fatalf("被拒绝的批量留下了 %d 条隧道", len(st.Tunnels))
	}
}

// 整批都失败的原因只有两种写法：一个对所有对端都成立的错误（本端没入网），或
// 每个对端各自被拒。前者必须一次报错、什么都不写；两者都带着「存为模板」时，
// 没人用到的那个模板不能留在列表里。
func TestFanHardErrorAndEmptyResultRollBackTheTemplate(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")
	st := s.state.Underlays["t"].State

	// 本端不存在：对每个对端都会失败，所以是一次 400 而不是 N 条跳过。
	rec := createTunnel(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 9, Iface: "wan0"},
		Peers: []uint16{2, 3},
		Path:  &pathReq{Hops: []hopReq{{NodeID: 4}}, SaveAs: "整批失败"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "本端设备 9") {
		t.Fatalf("本端没入网 = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(st.Paths) != 0 || len(st.Tunnels) != 0 {
		t.Fatalf("硬错误之后残留 %d 条隧道 / %d 个模板", len(st.Tunnels), len(st.Paths))
	}

	// 每个对端各自被拒（这里把自己当成了对端）：请求是成功的，但没有任何东西
	// 用到那个新建的模板。
	out := fanTunnels(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		Peers: []uint16{1},
		Path:  &pathReq{Hops: []hopReq{{NodeID: 4}}, SaveAs: "没人用的模板"},
	})
	if len(out.Tunnels) != 0 || len(out.Skipped) != 1 {
		t.Fatalf("自连的对端应全部跳过: %+v / %+v", out.Tunnels, out.Skipped)
	}
	if len(st.Paths) != 0 {
		t.Fatalf("一条隧道都没建成，模板 %v 却留下了", st.Paths)
	}
}

// 批量和模板是同一个动作：勾 6 个对端 + 存为模板。每条隧道都要指向同一个模板 id
// ——这才是之后改一次能动到全部的前提；而改模板那次必须一次动完整批。
func TestFanSaveAsTemplateMovesTheWholeBatchOnLaterEdit(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")
	enrollExtra(t, s, 5, "198.51.100.15")
	st := s.state.Underlays["t"].State

	out := fanTunnels(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		Peers: []uint16{2, 3},
		Path:  &pathReq{Hops: []hopReq{{NodeID: 4}}, SaveAs: "主用中转"},
	})
	if len(out.Tunnels) != 2 {
		t.Fatalf("built %d", len(out.Tunnels))
	}
	tpl := out.Tunnels[0].Path.TemplateID
	if tpl == "" || out.Tunnels[0].Path.TemplateName != "主用中转" {
		t.Fatalf("批量没记住自己存的模板: %+v", out.Tunnels[0].Path)
	}
	for _, tv := range out.Tunnels[1:] {
		if tv.Path.TemplateID != tpl {
			t.Fatalf("两条隧道指向了不同模板: %q / %q", tpl, tv.Path.TemplateID)
		}
	}
	if len(st.Paths) != 1 {
		t.Fatalf("存为模板写了 %d 个，应是 1 个共享", len(st.Paths))
	}
	if len(out.Underlay.Paths) != 1 || len(out.Underlay.Paths[0].Users) != 2 {
		t.Fatalf("模板列表 = %+v，两条隧道都应挂在它下面", out.Underlay.Paths)
	}

	// 改模板 = 改这一批：链换成 4→5 之后，两条隧道同时变成两跳，顺序按提交的来。
	rec := underlayDo(t, s.handleAdminUnderlayPaths, ten, http.MethodPut, "/api/underlay/paths",
		mustJSON(t, underlayPathRequest{ID: tpl, Hops: []underlayHopSpec{{NodeID: 4}, {NodeID: 5}}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("改模板 = %d (%s)", rec.Code, rec.Body.String())
	}
	var moved struct {
		Users    []string             `json:"users"`
		Path     underlayTemplateView `json:"path"`
		Underlay underlayView         `json:"underlay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &moved); err != nil {
		t.Fatalf("改模板响应: %v (%s)", err, rec.Body.String())
	}
	if len(moved.Users) != 2 {
		t.Fatalf("改模板动到 %d 条隧道，批量建的这两条都要在里面", len(moved.Users))
	}
	if len(moved.Path.Hops) != 2 || moved.Path.Hops[0].NodeID != 4 || moved.Path.Hops[1].NodeID != 5 {
		t.Fatalf("模板自己的链不对: %+v", moved.Path.Hops)
	}
	for _, tv := range moved.Underlay.Tunnels {
		p := tv.Path
		if p == nil || len(p.Hops) != 2 || p.Hops[0].NodeID != 4 || p.Hops[1].NodeID != 5 {
			t.Fatalf("隧道 %s 没跟上模板的两跳: %+v", tv.ID, p)
		}
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
		Path: via(1),
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
	p := relayBundle.Tunnels[0].Path
	if relayBundle.A != 2 || relayBundle.B != 3 || p == nil ||
		len(p.Hops) != 1 || p.Hops[0].NodeID != 1 || p.Hops[0].Serial != "cpe-1" {
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
		Path: &pathReq{Hops: []hopReq{{NodeID: 3}}, SaveAs: "经 cpe-3 中转"},
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

// 隧道配置页的颜色列：创建时带上、编辑时能改回 0、越界报 400。
// Color 是 *uint8，因为「没填」和「填了 0」在改色时是两件事——页面对已入库的
// 隧道总是回填当前值，而把一条昂贵颜色改回 0 是一次真实操作。
func TestTunnelEndpointStoresAndEditsColor(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)

	blue := uint8(1)
	rec := createTunnel(t, s, ten, tunnelReq{
		A:     underlaySpec{NodeID: 1, Iface: "wan0"},
		B:     underlaySpec{NodeID: 2, Iface: "wan0"},
		Color: &blue,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("带颜色创建 = %d (%s)", rec.Code, rec.Body.String())
	}
	tv := tunnelFrom(t, rec)
	if tv.Color != 1 {
		t.Fatalf("响应里的颜色 = %d, want 1", tv.Color)
	}

	// 只改端口、不提交颜色：颜色必须原地不动。
	keep := putTunnelView(t, s, ten, tunnelReq{ID: tv.ID, A: underlaySpec{Iface: "wan0"}})
	if keep.Color != 1 {
		t.Fatalf("不带颜色的编辑把颜色改成了 %d, want 保持 1", keep.Color)
	}

	zero := uint8(0)
	back := putTunnelView(t, s, ten, tunnelReq{ID: tv.ID, Color: &zero})
	if back.Color != 0 {
		t.Fatalf("改回颜色 0 失败: %d", back.Color)
	}

	bad := uint8(underlay.MaxTunnelColor + 1)
	rec = putTunnel(t, s, ten, tunnelReq{ID: tv.ID, Color: &bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界颜色 = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if got := tunnelFrom(t, putTunnel(t, s, ten, tunnelReq{ID: tv.ID})); got.Color != 0 {
		t.Fatalf("被拒的改色写进了库: %d", got.Color)
	}
}

func putTunnel(t *testing.T, s *server, ten string, req tunnelReq) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminUnderlayTunnels, ten, http.MethodPut, "/api/underlay/tunnels", mustJSON(t, req))
}

func putTunnelView(t *testing.T, s *server, ten string, req tunnelReq) underlayTunnelView {
	t.Helper()
	rec := putTunnel(t, s, ten, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("编辑隧道 = %d (%s)", rec.Code, rec.Body.String())
	}
	return tunnelFrom(t, rec)
}
