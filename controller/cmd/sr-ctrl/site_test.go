// Tests for the site entity: the only thing that gives the topology map a
// position, and therefore the place where "we do not know where this box is" has
// to stay knowable.
//
// The properties here are the ones a map cannot recover from a bug. A device
// drawn at a guessed coordinate looks exactly like a device drawn at its real
// one, so every case involving an absent or broken location asserts the counter
// and the flag rather than a point. The second group is tenant scoping: a site id
// is a bearer token for somebody's office address, so another tenant's site must
// be indistinguishable from one that never existed -- 404, never 403, and never
// listed.
package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sitesReply is the shape every site write answers with.
type sitesReply struct {
	OK       bool               `json:"ok"`
	Sites    []underlaySiteView `json:"sites"`
	Unplaced int                `json:"unplaced_nodes"`
	NodeID   uint16             `json:"node_id,omitempty"`
	SiteID   string             `json:"site_id,omitempty"`
}

var siteTargets = map[string]string{
	"sites":  "/api/underlay/sites",
	"delete": "/api/underlay/sites/delete",
	"node":   "/api/underlay/node-site",
}

// siteDo drives one of the three site endpoints with a session.
func siteDo(t *testing.T, s *server, kind, sid, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var h http.HandlerFunc
	switch kind {
	case "sites":
		h = s.handleAdminUnderlaySites
	case "delete":
		h = s.handleAdminUnderlaySiteDelete
	case "node":
		h = s.handleAdminUnderlayNodeSite
	default:
		t.Fatalf("unknown site endpoint %q", kind)
	}
	return underlayDo(t, h, sid, method, siteTargets[kind], body)
}

// decodeSites fails the test on any status but 200 and returns the list.
func decodeSites(t *testing.T, rec *httptest.ResponseRecorder) sitesReply {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var reply sitesReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return reply
}

func createSite(t *testing.T, s *server, sid, name string, lon, lat float64) underlaySiteView {
	t.Helper()
	rec := decodeSites(t, siteDo(t, s, "sites", sid, http.MethodPost,
		mustJSON(t, map[string]any{"name": name, "lon": lon, "lat": lat})))
	for _, si := range rec.Sites {
		if si.Name == name {
			return si
		}
	}
	t.Fatalf("site %q missing from %+v", name, rec.Sites)
	return underlaySiteView{}
}

func placeNode(t *testing.T, s *server, sid string, nodeID uint16, siteID string) sitesReply {
	t.Helper()
	return decodeSites(t, siteDo(t, s, "node", sid, http.MethodPut,
		mustJSON(t, map[string]any{"node_id": nodeID, "site_id": siteID})))
}

// topologyOf fetches the topology document through its handler.
func topologyOf(t *testing.T, s *server, sid string) underlayTopology {
	t.Helper()
	rec := underlayDo(t, s.handleAdminUnderlayTopology, sid, http.MethodGet, "/api/underlay/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("topology = %d (%s)", rec.Code, rec.Body.String())
	}
	var topo underlayTopology
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
		t.Fatalf("decode topology: %v", err)
	}
	return topo
}

func topoNode(t *testing.T, topo underlayTopology, nodeID uint16) underlayNodeView {
	t.Helper()
	for _, n := range topo.Nodes {
		if n.NodeID == nodeID {
			return n
		}
	}
	t.Fatalf("device %d missing from topology", nodeID)
	return underlayNodeView{}
}

// ------------------------------------------------------------------ writing

func TestSiteCreateAndUpdate(t *testing.T) {
	s, ten, _ := underlayFixture(t)

	// Nothing is placed yet: three devices, zero sites, and the count says so.
	rec := decodeSites(t, siteDo(t, s, "sites", ten, http.MethodGet, ""))
	if len(rec.Sites) != 0 || rec.Unplaced != 3 {
		t.Fatalf("fresh tenant = %d sites, %d unplaced; want 0, 3", len(rec.Sites), rec.Unplaced)
	}

	beijing := createSite(t, s, ten, "北京总部", 116.4074, 39.9042)
	if beijing.Devices != 0 || beijing.CreatedAt == 0 {
		t.Fatalf("created site = %+v", beijing)
	}
	if !strings.HasPrefix(beijing.ID, "site_") {
		t.Fatalf("site id %q is not site_-prefixed", beijing.ID)
	}

	// An update keeps the id and stamps itself.
	up := mustJSON(t, map[string]any{"id": beijing.ID, "name": "北京-望京", "lon": 116.481, "lat": 39.996})
	after := decodeSites(t, siteDo(t, s, "sites", ten, http.MethodPut, up))
	if len(after.Sites) != 1 {
		t.Fatalf("edit changed the site count: %+v", after.Sites)
	}
	if got := after.Sites[0]; got.Name != "北京-望京" || got.ID != beijing.ID ||
		got.Lon != 116.481 || got.UpdatedAt == 0 {
		t.Fatalf("edited site = %+v", got)
	}

	// Two devices in one site: that sharing is the entire reason a site exists,
	// and the count is derived, not typed.
	placeNode(t, s, ten, 1, beijing.ID)
	placed := placeNode(t, s, ten, 2, beijing.ID)
	if len(placed.Sites) != 1 || placed.Sites[0].Devices != 2 {
		t.Fatalf("device count after placing two boxes = %+v", placed.Sites)
	}
	if placed.Unplaced != 1 {
		t.Fatalf("unplaced = %d, want 1", placed.Unplaced)
	}
}

func TestSiteCreateRejectsBadInput(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"no longitude", mustJSON(t, map[string]any{"name": "x", "lat": 10.0})},
		{"no latitude", mustJSON(t, map[string]any{"name": "x", "lon": 10.0})},
		{"lon out of range", mustJSON(t, map[string]any{"name": "x", "lon": 181.0, "lat": 0.0})},
		{"lat out of range", mustJSON(t, map[string]any{"name": "x", "lon": 0.0, "lat": 91.0})},
		{"blank name", mustJSON(t, map[string]any{"name": "   ", "lon": 0.0, "lat": 0.0})},
		{"name too long", mustJSON(t, map[string]any{"name": strings.Repeat("a", 81), "lon": 0.0, "lat": 0.0})},
		{"name with control char", mustJSON(t, map[string]any{"name": "a\x01b", "lon": 0.0, "lat": 0.0})},
		{"create carries an id", mustJSON(t, map[string]any{"id": "site_preset", "name": "x", "lon": 0.0, "lat": 0.0})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := siteDo(t, s, "sites", ten, http.MethodPost, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", rec.Code, rec.Body.String())
			}
		})
	}
	// Every refusal above stored nothing.
	if got := decodeSites(t, siteDo(t, s, "sites", ten, http.MethodGet, "")); len(got.Sites) != 0 {
		t.Fatalf("rejected writes were persisted: %+v", got.Sites)
	}
}

func TestSiteEditNeedsARealSite(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	for name, id := range map[string]string{
		"no id":      "",
		"unknown id": "site_nope",
		"whitespace": "   ",
	} {
		rec := siteDo(t, s, "sites", ten, http.MethodPut,
			mustJSON(t, map[string]any{"id": id, "name": "x", "lon": 0.0, "lat": 0.0}))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d (%s), want 404", name, rec.Code, rec.Body.String())
		}
	}
}

// A zero coordinate is a real place (the Gulf of Guinea), so the API must accept
// it; the flag that decides whether a box is drawn is the site reference, not
// the number.
func TestZeroCoordinateIsAPlace(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	site := createSite(t, s, ten, "赤道零點", 0, 0)
	placed := placeNode(t, s, ten, 1, site.ID)
	if placed.Sites[0].Devices != 1 {
		t.Fatalf("(0,0) site not counted: %+v", placed.Sites)
	}
	row := topoNode(t, topologyOf(t, s, ten), 1)
	if !row.HasLocation || row.Lon != 0 || row.Lat != 0 {
		t.Fatalf("topology row at (0,0) = %+v", row)
	}
}

func TestValidSiteCoordsRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := validSiteCoords(v, 0); err == nil {
			t.Errorf("lon %v was accepted", v)
		}
		if err := validSiteCoords(0, v); err == nil {
			t.Errorf("lat %v was accepted", v)
		}
	}
	// NaN is the case a range check cannot see: both comparisons below are false,
	// which is why validSiteCoords tests for it by itself.
	if math.NaN() < -180 || math.NaN() > 180 {
		t.Fatal("NaN behaved as an out-of-range number")
	}
	for _, pair := range [][2]float64{{-180, -90}, {180, 90}, {0, 0}, {-0.0, 0.0}} {
		if err := validSiteCoords(pair[0], pair[1]); err != nil {
			t.Errorf("corner %+v rejected: %v", pair, err)
		}
	}
}

func TestValidSiteNameBounds(t *testing.T) {
	if validSiteName("") || validSiteName(" \t") {
		t.Error("an empty or blank name was accepted")
	}
	if !validSiteName("a") || !validSiteName(strings.Repeat("上", siteNameMaxRunes)) {
		t.Error("a legal name was rejected")
	}
	if validSiteName(strings.Repeat("a", siteNameMaxRunes+1)) {
		t.Error("an over-long name was accepted")
	}
	// Counted in runes, not bytes: 80 Chinese characters are 240 bytes and still
	// one address label.
	if len(strings.Repeat("上", siteNameMaxRunes)) != 3*siteNameMaxRunes {
		t.Fatal("fixture is not multi-byte")
	}
	if validSiteName("上海\x7f仓库") {
		t.Error("a DEL in the middle was accepted")
	}
}

// ------------------------------------------------------------- who may write

func TestSiteEndpointsAreTenantOnly(t *testing.T) {
	s, _, _ := underlayFixture(t)
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})
	body := mustJSON(t, map[string]any{"id": "x", "name": "x", "lon": 0.0, "lat": 0.0,
		"node_id": 1, "site_id": "x"})
	for _, kind := range []string{"sites", "delete", "node"} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			rec := siteDo(t, s, kind, op, m, body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("operator %s %s = %d, want 403", kind, m, rec.Code)
			}
		}
	}
	// No session at all is a 401, before any of the parsing above.
	anon := siteDo(t, s, "sites", "", http.MethodGet, "")
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", anon.Code)
	}
}

// Tenant t2 must not be able to tell that t owns a site, let alone put its own
// device there: the answer for a foreign id is the answer for an id that never
// existed.
func TestSitesAreTenantScoped(t *testing.T) {
	s, ten, two := underlayFixture(t)
	beijing := createSite(t, s, ten, "北京总部", 116.4, 39.9)
	placeNode(t, s, ten, 1, beijing.ID)

	if got := decodeSites(t, siteDo(t, s, "sites", two, http.MethodGet, "")); len(got.Sites) != 0 {
		t.Fatalf("t2 sees t's sites: %+v", got.Sites)
	}
	if rec := siteDo(t, s, "sites", two, http.MethodPut,
		mustJSON(t, map[string]any{"id": beijing.ID, "name": "改成我的", "lon": 1.0, "lat": 1.0})); rec.Code != http.StatusNotFound {
		t.Fatalf("t2 editing t's site = %d, want 404", rec.Code)
	}
	if rec := siteDo(t, s, "delete", two, http.MethodPost,
		mustJSON(t, map[string]any{"id": beijing.ID})); rec.Code != http.StatusNotFound {
		t.Fatalf("t2 deleting t's site = %d, want 404", rec.Code)
	}
	if rec := siteDo(t, s, "node", two, http.MethodPut,
		mustJSON(t, map[string]any{"node_id": 9, "site_id": beijing.ID})); rec.Code != http.StatusNotFound {
		t.Fatalf("t2 placing its device in t's site = %d, want 404", rec.Code)
	}

	// Nothing above moved: t's device is still in Beijing and t2's is still
	// nowhere.
	t1 := topoNode(t, topologyOf(t, s, ten), 1)
	if !t1.HasLocation || t1.SiteName != "北京总部" {
		t.Fatalf("t's row damaged by the cross-tenant attempts: %+v", t1)
	}
	t2topo := topologyOf(t, s, two)
	if len(t2topo.Sites) != 0 || t2topo.UnplacedNodes != 1 {
		t.Fatalf("t2 topology = %d sites, %d unplaced", len(t2topo.Sites), t2topo.UnplacedNodes)
	}
	if row := topoNode(t, t2topo, 9); row.HasLocation || row.SiteID != "" {
		t.Fatalf("t2's device got a location it should not have: %+v", row)
	}
}

func TestNodeSiteAssignmentErrors(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	site := createSite(t, s, ten, "北京总部", 116.4, 39.9)

	if rec := siteDo(t, s, "node", ten, http.MethodPut,
		mustJSON(t, map[string]any{"node_id": 99, "site_id": site.ID})); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown device = %d, want 404", rec.Code)
	}
	if rec := siteDo(t, s, "node", ten, http.MethodPost,
		mustJSON(t, map[string]any{"node_id": 1, "site_id": site.ID})); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to node-site = %d, want 405", rec.Code)
	}
	// A device of t2 addressed through t's session is simply not there.
	if rec := siteDo(t, s, "node", ten, http.MethodPut,
		mustJSON(t, map[string]any{"node_id": 9, "site_id": site.ID})); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant device = %d, want 404", rec.Code)
	}
	if n := s.state.Nodes[key("t2", 9)]; n.SiteID != "" {
		t.Fatalf("t2's node was placed by t: %q", n.SiteID)
	}
}

// The clearing half matters as much as the placing half: "the tenant took the
// location away" has to land back in 未定位, not stay on the map.
func TestNodeSiteClearAndTopology(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	site := createSite(t, s, ten, "北京总部", 116.4, 39.9)
	placeNode(t, s, ten, 1, site.ID)
	placeNode(t, s, ten, 2, site.ID)

	before := topologyOf(t, s, ten)
	if len(before.Sites) != 1 || before.Sites[0].Devices != 2 || before.UnplacedNodes != 1 {
		t.Fatalf("topology after placing = %d sites (%+v), %d unplaced",
			len(before.Sites), before.Sites, before.UnplacedNodes)
	}
	row := topoNode(t, before, 1)
	if row.SiteID != site.ID || row.SiteName != "北京总部" || !row.HasLocation ||
		row.Lon != 116.4 || row.Lat != 39.9 {
		t.Fatalf("placed row = %+v", row)
	}
	// The device nobody placed has no location and no invented zero.
	if un := topoNode(t, before, 3); un.HasLocation || un.Lon != 0 || un.Lat != 0 {
		t.Fatalf("unplaced row carries a coordinate: %+v", un)
	}

	cleared := placeNode(t, s, ten, 1, "")
	if cleared.Unplaced != 2 {
		t.Fatalf("after clearing, unplaced = %d, want 2", cleared.Unplaced)
	}
	if got := topoNode(t, topologyOf(t, s, ten), 1); got.HasLocation || got.SiteID != "" {
		t.Fatalf("clear did not reach the topology: %+v", got)
	}
}

// --------------------------------------------------------------- deleting

func TestSiteDeleteRefusedWhileDevicesRemain(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	site := createSite(t, s, ten, "北京总部", 116.4, 39.9)
	placeNode(t, s, ten, 1, site.ID)

	rec := siteDo(t, s, "delete", ten, http.MethodPost, mustJSON(t, map[string]any{"id": site.ID}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in use = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "1 台设备") {
		t.Fatalf("refusal does not say how many devices are in the way: %q", msg)
	}
	if s.state.Sites[site.ID] == nil {
		t.Fatal("a refused delete removed the site")
	}

	placeNode(t, s, ten, 1, "")
	after := decodeSites(t, siteDo(t, s, "delete", ten, http.MethodPost,
		mustJSON(t, map[string]any{"id": site.ID})))
	if len(after.Sites) != 0 {
		t.Fatalf("site survived its delete: %+v", after.Sites)
	}
	// Deleting an empty site leaves all three devices unplaced.
	if after.Unplaced != 3 {
		t.Fatalf("unplaced after delete = %d, want 3", after.Unplaced)
	}
}

func TestSiteDeleteRejectsUnknown(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	if rec := siteDo(t, s, "delete", ten, http.MethodPost,
		mustJSON(t, map[string]any{"id": "site_nope"})); rec.Code != http.StatusNotFound {
		t.Fatalf("delete of an unknown site = %d, want 404", rec.Code)
	}
	if rec := siteDo(t, s, "delete", ten, http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on delete = %d, want 405", rec.Code)
	}
}

// ---------------------------------------------------------------- persistence

func TestSitesPersistThroughReload(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	site := createSite(t, s, ten, "北京总部", 116.4074, 39.9042)
	placeNode(t, s, ten, 1, site.ID)

	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := st.Sites[site.ID]
	if got == nil {
		t.Fatalf("site did not survive: %+v", st.Sites)
	}
	if got.TenantID != "t" || got.Name != "北京总部" || got.Lon != 116.4074 || got.Lat != 39.9042 {
		t.Fatalf("reloaded site = %+v", got)
	}
	if n := st.Nodes[key("t", 1)]; n == nil || n.SiteID != site.ID {
		t.Fatalf("the device's reference did not survive: %+v", n)
	}
	// An unplaced device must not round-trip into a broken reference: site_id is
	// omitted when empty, and the loader has nothing to clear.
	if n := st.Nodes[key("t", 2)]; n == nil || n.SiteID != "" {
		t.Fatalf("unplaced device reloads with a site reference: %+v", n)
	}
}

// A state file written before sites existed has no "sites" key. It has to load
// as an empty map, because a nil map panics on the first create.
func TestSitesLoadFromAnOlderFile(t *testing.T) {
	store := &fileStore{path: filepath.Join(t.TempDir(), "old.json")}
	body := `{"tenants":{"t":{"id":"t"}},"nodes":{"t|1":{"tenant_id":"t","node_id":1}}}`
	if err := os.WriteFile(store.path, []byte(body), 0600); err != nil {
		t.Fatalf("write old state: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Sites == nil {
		t.Fatal("sites loaded as a nil map")
	}
	if len(st.Sites) != 0 {
		t.Fatalf("sites materialized out of nothing: %+v", st.Sites)
	}
}

func TestNormalizeSitesRepairsReferences(t *testing.T) {
	st := &State{
		Tenants: map[string]*Tenant{"t": {ID: "t"}, "t2": {ID: "t2"}},
		Nodes: map[string]*NodeRecord{
			"t|1": {TenantID: "t", NodeID: 1, SiteID: "site_ok"},
			// A reference to somebody else's site would draw this box at
			// another customer's address.
			"t|2": {TenantID: "t", NodeID: 2, SiteID: "site_other"},
			"t|3": {TenantID: "t", NodeID: 3, SiteID: "site_gone"},
			"t|4": {TenantID: "t", NodeID: 4},
		},
		Sites: map[string]*Site{
			"site_ok":      {ID: "site_ok", TenantID: "t", Name: "北京"},
			"site_other":   {ID: "site_other", TenantID: "t2", Name: "别人的"},
			"site_gone":    nil,
			"site_orphan":  {ID: "site_orphan", TenantID: "tenant_vanished", Name: "无主"},
			"site_renamed": {ID: "site_something_else", TenantID: "t", Name: "键名不一致"},
		},
	}
	if !normalizeSites(st) {
		t.Fatal("a damaged state was reported as clean")
	}
	if _, ok := st.Sites["site_gone"]; ok {
		t.Error("a nil row survived")
	}
	if _, ok := st.Sites["site_orphan"]; ok {
		t.Error("a row for a tenant that no longer exists survived")
	}
	// site_other is another tenant's real site: it stays, and only t's reference
	// to it is cleared.
	if st.Sites["site_other"] == nil {
		t.Error("a valid row of another tenant was dropped")
	}
	// Its own tenant still exists, so the row is kept and re-keyed rather than
	// dropped: every node reference was read as the key.
	if si := st.Sites["site_renamed"]; si == nil || si.ID != "site_renamed" {
		t.Fatalf("mis-keyed row not repaired: %+v", si)
	}
	for id, want := range map[uint16]string{1: "site_ok", 2: "", 3: "", 4: ""} {
		if got := st.Nodes[key("t", id)].SiteID; got != want {
			t.Errorf("device %d site = %q, want %q", id, got, want)
		}
	}
	// A clean state is not reported as repaired, so the loader does not rewrite a
	// file it did not touch.
	clean := &State{
		Tenants: map[string]*Tenant{"t": {ID: "t"}},
		Nodes:   map[string]*NodeRecord{"t|1": {TenantID: "t", NodeID: 1, SiteID: "site_ok"}},
		Sites:   map[string]*Site{"site_ok": {ID: "site_ok", TenantID: "t"}},
	}
	if normalizeSites(clean) {
		t.Error("a clean state was reported as migrated")
	}
}

// jsonKeys collects every object key that appears at any depth of the document
// a device would receive, after one round through JSON -- which is the only form
// the device actually sees.
func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("redecode: %v", err)
	}
	out := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch d := x.(type) {
		case map[string]any:
			for k, val := range d {
				out[k] = true
				walk(val)
			}
		case []any:
			for _, e := range d {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// A location is for drawing (D2). The dataplane routes on addresses and ports and
// has no use for a coordinate, so nothing about a site may ride along in the
// document a device is told to run. The keys are compared structurally rather
// than by substring because "latest" and "translation" both contain "lat".
func TestSiteNeverReachesADevice(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)
	site := createSite(t, s, ten, "北京总部", 116.4, 39.9)
	placeNode(t, s, ten, 1, site.ID)

	// Positive control: the console document does carry them, so the list below
	// is not empty by accident.
	view := decodeSites(t, siteDo(t, s, "sites", ten, http.MethodGet, ""))
	consoleKeys := jsonKeys(t, view)
	for _, k := range []string{"lon", "lat", "name", "devices", "sites"} {
		if !consoleKeys[k] {
			t.Fatalf("the sites reply lost %q: %v", k, consoleKeys)
		}
	}

	bundle := bundleOf(t, s, "t", 1)
	deviceKeys := jsonKeys(t, bundle)
	for _, k := range []string{"site", "sites", "site_id", "site_name", "lon", "lat",
		"has_location", "unplaced_nodes", "devices"} {
		if deviceKeys[k] {
			t.Errorf("the device bundle carries %q; a location must stay in the console", k)
		}
	}
	// And the placed device is still told what it was told before the site
	// existed: a location is decoration, not configuration.
	if len(bundle.Tunnels) == 0 {
		t.Fatalf("bundle rendered no tunnels: %+v", bundle)
	}
}

// ------------------------------------------------------- /api/state and sites

// stateOf reads /api/state through its handler with a session.
func stateOf(t *testing.T, s *server, sid string) consoleState {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleAdminState(rec, consoleReq(http.MethodGet, "/api/state", "", sid))
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d (%s)", rec.Code, rec.Body.String())
	}
	var out consoleState
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return out
}

func stateNode(t *testing.T, st consoleState, nodeID uint16) consoleNode {
	t.Helper()
	for _, n := range st.Nodes {
		if n.NodeID == nodeID {
			return n
		}
	}
	t.Fatalf("node %d missing from state: %+v", nodeID, st.Nodes)
	return consoleNode{}
}

// The device page's dropdown reads a node's site off /api/state, which is what
// keeps the site list one small request instead of a second table the page has
// to cross-reference. Where the location lives is a permission question: it sits
// inside Runtime, the block the operator's copy of the same node never receives,
// so "运营方看不到位置" holds because of the struct and not because of a line of
// JavaScript.
func TestStateCarriesSiteForTenantOnly(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "ops", Via: "password"})

	site := createSite(t, s, ten, "北京 · 国贸", 116.46, 39.91)
	placeNode(t, s, ten, 1, site.ID)

	placed := stateNode(t, stateOf(t, s, ten), 1)
	if placed.Runtime == nil {
		t.Fatal("tenant got no runtime for its own device")
	}
	if placed.Runtime.SiteID != site.ID || placed.Runtime.SiteName != "北京 · 国贸" {
		t.Fatalf("placed device = %+v", placed.Runtime)
	}
	// "未定位" has to be readable without a second request: the field is simply
	// absent, and the page turns that into the 未定位 option.
	if free := stateNode(t, stateOf(t, s, ten), 2); free.Runtime == nil || free.Runtime.SiteID != "" {
		t.Fatalf("unplaced device = %+v, want an empty site", free.Runtime)
	}

	// The name is resolved as the response is built, so renaming a site cannot
	// leave a stale label next to a live id.
	decodeSites(t, siteDo(t, s, "sites", ten, http.MethodPut, mustJSON(t, map[string]any{
		"id": site.ID, "name": "北京-望京", "lon": 116.481, "lat": 39.996,
	})))
	if got := stateNode(t, stateOf(t, s, ten), 1).Runtime.SiteName; got != "北京-望京" {
		t.Fatalf("site name after rename = %q", got)
	}

	for _, n := range stateOf(t, s, op).Nodes {
		if n.Runtime != nil {
			t.Fatalf("operator received runtime -- and with it a location -- for node %d", n.NodeID)
		}
	}
}
