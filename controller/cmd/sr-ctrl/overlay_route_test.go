// Tests for the overlay segment of a bundle (O6-b): what the controller renders,
// and the two version rules the device side depends on.
//
// The properties worth pinning are the ones the page cannot show:
//
//   - `vrfs` is ALWAYS an array (never null, never absent) because sr-route reads a
//     missing tunnel key as "withdraw everything" -- so the presence of a key is the
//     instruction, not a formatting detail.
//   - `tunnels` appears only once the tenant has actually built tunnels. A tenant
//     that configures 导通 before building tunnels gets the RTs it asked for and no
//     half-built tunnel table.
//   - a 导通组 edit does not touch the draft version, yet the box's store refuses
//     "same version, different content". Publish therefore mints a new version, or
//     the edit would never reach any device.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/apply"
)

// overlayRouteFixture is tenant "t" with three devices that report a wan0, plus
// saved drafts on nodes 1 and 2 (VPNs 0 and 100) so a publish has something to
// freeze and a 导通组 has members whose VPN really exists.
func overlayRouteFixture(t *testing.T) (*server, string) {
	t.Helper()
	s, ten, _ := underlayFixture(t)
	for _, node := range []uint16{1, 2} {
		cfg := baseValidConfig()
		cfg.Hostname = fmt.Sprintf("edge-%d", node)
		body, _ := json.Marshal(saveConfigRequest{NodeID: node, Config: cfg})
		rec := configRequest(t, s, ten, http.MethodPut, "/api/config", string(body))
		if v := decodeConfigView(t, rec); v.Version != 1 {
			t.Fatalf("save draft for node %d = %+v", node, v)
		}
	}
	return s, ten
}

func routeArtifact(t *testing.T, s *server, tenant string, node uint16) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overlayRouteArtifactLocked(tenant, node)
}

func decodeRouteDoc(t *testing.T, doc string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("第 4 段不是合法 JSON: %v\n%s", err, doc)
	}
	return out
}

type routeVRF struct {
	ID        uint8    `json:"id"`
	ExportRTs []string `json:"export_rts"`
	ImportRTs []string `json:"import_rts"`
}

type routeTunnel struct {
	Peer    uint16 `json:"peer_device_id"`
	NextHop string `json:"next_hop"`
}

func TestOverlayRouteSegmentIsTheTwoTablesSRRouteReads(t *testing.T) {
	s, ten := overlayRouteFixture(t)
	grec, g := createGroup(t, s, ten, "VPN0 互通", OverlayMember{NodeID: 1, VPNID: 0}, OverlayMember{NodeID: 2, VPNID: 0})
	if grec.Code != http.StatusCreated {
		t.Fatalf("create group = %d (%s)", grec.Code, grec.Body.String())
	}

	// No underlay state yet: vrfs is an empty-of-tunnels-but-present array, and
	// tunnels is not in the document at all.
	doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))
	if _, ok := doc["tunnels"]; ok {
		t.Errorf("租户还没建隧道就发了 tunnels 键: %s", doc["tunnels"])
	}
	var vrfs []routeVRF
	if err := json.Unmarshal(doc["vrfs"], &vrfs); err != nil {
		t.Fatalf("vrfs: %v", err)
	}
	if len(vrfs) != 1 || vrfs[0].ID != 0 || len(vrfs[0].ExportRTs) != 1 || vrfs[0].ExportRTs[0] != g.RT {
		t.Fatalf("node 1 vrfs = %+v, want VPN0 带 %s", vrfs, g.RT)
	}
	if len(vrfs[0].ImportRTs) != 1 || vrfs[0].ImportRTs[0] != g.RT {
		t.Fatalf("同组成员要互导: %+v", vrfs[0])
	}

	publishDefault(t, s, ten)
	trec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0"))
	if trec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", trec.Code, trec.Body.String())
	}
	tv := tunnelFrom(t, trec)

	doc = decodeRouteDoc(t, routeArtifact(t, s, "t", 1))
	if _, ok := doc["tunnels"]; !ok {
		t.Fatal("有隧道了却没有 tunnels 键")
	}
	var tunnels []routeTunnel
	if err := json.Unmarshal(doc["tunnels"], &tunnels); err != nil {
		t.Fatalf("tunnels: %v", err)
	}
	if len(tunnels) != 1 || tunnels[0].Peer != 2 || tunnels[0].NextHop != tv.B.Overlay {
		t.Fatalf("node 1 tunnels = %+v, want 对端 2 的 overlay %s", tunnels, tv.B.Overlay)
	}
	// The other end of the same tunnel must name the other peer.
	doc = decodeRouteDoc(t, routeArtifact(t, s, "t", 2))
	if err := json.Unmarshal(doc["tunnels"], &tunnels); err != nil {
		t.Fatalf("node 2 tunnels: %v", err)
	}
	if len(tunnels) != 1 || tunnels[0].Peer != 1 || tunnels[0].NextHop != tv.A.Overlay {
		t.Fatalf("node 2 tunnels = %+v, want 对端 1 的 overlay %s", tunnels, tv.A.Overlay)
	}
}

func TestOverlayRouteSegmentArrayIsNeverNull(t *testing.T) {
	s, _ := overlayRouteFixture(t)
	// Node 3 has no draft and no group, so the honest answer is "no VRFs" -- which
	// sr-route must be able to tell apart from "said nothing".
	doc := routeArtifact(t, s, "t", 3)
	if !strings.Contains(doc, `"vrfs": []`) {
		t.Fatalf("没有组的设备应该收到空数组，实际: %s", doc)
	}
	if strings.Contains(doc, "null") {
		t.Fatalf("渲染出了 null: %s", doc)
	}
}

// A 导通组 edit leaves dc.Version alone, so without a new version number the box's
// store refuses the bundle ("already installed with different content") and the
// tenant's edit never lands.
func TestOverlayEditForcesANewPublishedVersion(t *testing.T) {
	s, ten := overlayRouteFixture(t)
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("publish v1 = %d (%s)", rec.Code, rec.Body.String())
	}
	dc := s.state.Configs[configKey("t", 1)]
	first := *dc.Published
	if first.Version != 1 || strings.Contains(first.Artifacts.Route, "65000:") {
		t.Fatalf("setup: 还没有组，第 4 段却有 RT: %+v", first)
	}

	grec, _ := createGroup(t, s, ten, "VPN0 互通",
		OverlayMember{NodeID: 1, VPNID: 0}, OverlayMember{NodeID: 2, VPNID: 0})
	if grec.Code != http.StatusCreated {
		t.Fatalf("create group = %d (%s)", grec.Code, grec.Body.String())
	}
	if dc.Published.Version != first.Version || dc.Published.Artifacts.Route != first.Artifacts.Route {
		t.Fatal("改组就把已发布的那版改了：发布的必须是快照")
	}

	// The page must not claim 已同步 while this difference exists.
	view := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", ""))
	if !view.OverlayPending {
		t.Fatal("导通组改了但页面说已同步")
	}

	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("publish after edit = %d (%s)", rec.Code, rec.Body.String())
	}
	if dc.Published.Version != 2 {
		t.Fatalf("只改了导通组，发布后版本 = v%d，想要 v2", dc.Published.Version)
	}
	if !strings.Contains(dc.Published.Artifacts.Route, "65000:") {
		t.Fatalf("新版本里还是没有 RT: %s", dc.Published.Artifacts.Route)
	}
	view = decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", ""))
	if view.OverlayPending {
		t.Fatal("已经发布了，页面还说 overlay 待发布")
	}

	// Re-publishing what is already published is still a no-op: same version, same
	// route segment, so no timestamp moves and the device has nothing to redo.
	before := *dc.Published
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("re-publish = %d", rec.Code)
	}
	if dc.Published.Version != before.Version || dc.Published.PublishedAt != before.PublishedAt {
		t.Fatalf("重复发布改写了快照: %+v -> %+v", before, *dc.Published)
	}

	// What the device fetches is that frozen v2, and sr-apply can decode it.
	rec := httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", "t", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d (%s)", rec.Code, rec.Body.String())
	}
	b, err := apply.DecodeBundle(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("bundle 解不开: %v", err)
	}
	if b.Version != 2 || !strings.Contains(b.Artifacts.Route, `"export_rts"`) {
		t.Fatalf("设备收到的第 4 段 = v%d %q", b.Version, b.Artifacts.Route)
	}
}

func TestOverlayPublishRefusedWhenDeviceHasNoSuchVPN(t *testing.T) {
	s, ten := overlayRouteFixture(t)
	// 建组时两台都有 VPN 100（O6-a 的闸门就在这里），所以顺序必须是
	// 先导通、后来有人把这台设备的 VPN 100 从基础配置里删掉。
	grec, _ := createGroup(t, s, ten, "guest 互通",
		OverlayMember{NodeID: 1, VPNID: 100}, OverlayMember{NodeID: 2, VPNID: 100})
	if grec.Code != http.StatusCreated {
		t.Fatalf("create group = %d (%s)", grec.Code, grec.Body.String())
	}
	cfg := baseValidConfig()
	cfg.Hostname = "edge-2"
	cfg.VPNs = []ConfigVPN{{ID: 0, Name: "default"}}
	cfg.Ifaces = []ConfigIface{cfg.Ifaces[0]}
	cfg.Statics = []ConfigStatic{cfg.Statics[0]}
	body, _ := json.Marshal(saveConfigRequest{NodeID: 2, Config: cfg})
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodPut, "/api/config", string(body))); v.Version != 2 {
		t.Fatalf("去掉 VPN 100 = %+v", v)
	}
	// The group still references a VPN device 2 no longer has. Nothing in the
	// draft says so, and sr-route would just get a VRF id the box never created.
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":2}`); rec.Code != http.StatusConflict {
		t.Fatalf("发布没有 VPN 100 的设备 = %d，想要 409 (%s)", rec.Code, rec.Body.String())
	} else if msg := rec.Body.String(); !strings.Contains(msg, "VPN 100") || !strings.Contains(msg, "基础配置") {
		t.Fatalf("拒绝的话没说清要改哪里: %s", msg)
	}
	// The device that does have the VPN still publishes.
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("发布 node 1 = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestOverlaySegmentSurvivesPersistence(t *testing.T) {
	s, ten := overlayRouteFixture(t)
	grec, _ := createGroup(t, s, ten, "VPN0 互通",
		OverlayMember{NodeID: 1, VPNID: 0}, OverlayMember{NodeID: 2, VPNID: 0})
	if grec.Code != http.StatusCreated {
		t.Fatalf("create group = %d (%s)", grec.Code, grec.Body.String())
	}
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}
	want := s.state.Configs[configKey("t", 1)].Published.Artifacts.Route
	if want == "" {
		t.Fatal("发布的第 4 段是空的")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	file := &fileStore{path: path}
	s.store = file
	s.save()
	st, err := file.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := st.Configs[configKey("t", 1)]
	if got == nil || got.Published == nil || got.Published.Artifacts.Route != want {
		t.Fatalf("重启控制器后第 4 段丢了: %+v", got)
	}
}
