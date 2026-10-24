// Tests for the telemetry ingest/read path.
//
// What they pin down is mostly what the endpoint must REFUSE or FORGET: a node
// posting under another node's id, rows that cannot be matched to a tunnel, a
// stale node's last window surviving its heartbeat, and a window appended
// rather than replaced. A view that silently merges two ends' rows would look
// fine in every happy-path test and be useless exactly when a link breaks.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

// peerReq builds a request carrying the URI SAN that peerTenantFromCert reads.
// No signature is involved: the controller handler's only identity check is
// that URI, which is exactly what these tests are exercising.
func peerReq(method, target, body, tenant string, node uint16) *http.Request {
	u, err := url.Parse(fmt.Sprintf("srou://%s/node/%d", tenant, node))
	if err != nil {
		panic(err)
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{u}}}}
	return r
}

func postMetricsReq(t *testing.T, s *server, tenant string, node uint16, rep wire.MetricsReport) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, peerReq(http.MethodPost, "/v1/metrics", string(body), tenant, node))
	return rec
}

func getMetricsReq(t *testing.T, s *server, tenant string, node uint16, query string) (*httptest.ResponseRecorder, wire.TelemetryView) {
	t.Helper()
	target := "/v1/metrics"
	if query != "" {
		target += "?" + query
	}
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, peerReq(http.MethodGet, target, "", tenant, node))
	var view wire.TelemetryView
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("view is not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, view
}

func rowFor(swIf uint32, peer string) wire.TunnelMetrics {
	return wire.TunnelMetrics{
		SwIfIndex: swIf, PeerIP: peer, Up: true, BFDState: "up",
		IntervalMS: 300, WindowSec: 10, ProbeTx: 33, ProbeRx: 33,
		RTTms: 1.2, JitterMs: 0.1,
	}
}

func TestMetricsRoundtripIsTenantScoped(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	addNode(s, "t", 3, []string{"10.10.3.2"}, "")
	addNode(s, "other", 1, []string{"10.20.1.2"}, "")

	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 1, Tunnels: []wire.TunnelMetrics{
		rowFor(5, "192.168.99.2"), rowFor(6, "192.168.99.3"),
	}}); rec.Code != 200 {
		t.Fatalf("node 1 post: %d %s", rec.Code, rec.Body)
	}
	if rec := postMetricsReq(t, s, "t", 2, wire.MetricsReport{NodeID: 2, Tunnels: []wire.TunnelMetrics{
		rowFor(5, "192.168.99.1"),
	}}); rec.Code != 200 {
		t.Fatalf("node 2 post: %d %s", rec.Code, rec.Body)
	}

	rec, view := getMetricsReq(t, s, "t", 1, "")
	if rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if view.TenantID != "t" || view.Self != 1 {
		t.Fatalf("view header = %+v", view)
	}
	if len(view.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (nodes with no window are still listed)", len(view.Nodes))
	}
	for i, want := range []uint16{1, 2, 3} {
		if view.Nodes[i].NodeID != want {
			t.Fatalf("node order = %d,%d,%d; want 1,2,3", view.Nodes[0].NodeID, view.Nodes[1].NodeID, view.Nodes[2].NodeID)
		}
	}
	if got := len(view.Nodes[0].Tunnels); got != 2 {
		t.Errorf("node 1 rows = %d, want 2", got)
	}
	if got := len(view.Nodes[1].Tunnels); got != 1 {
		t.Errorf("node 2 rows = %d, want 1", got)
	}
	if got := len(view.Nodes[2].Tunnels); got != 0 {
		t.Errorf("node 3 has no window, want 0 rows, got %d", got)
	}
	if view.Nodes[2].SeenAt != 0 || view.Nodes[2].AgeSec != 0 {
		t.Errorf("node 3 timestamps = %d/%d, want zero", view.Nodes[2].SeenAt, view.Nodes[2].AgeSec)
	}
	// The two ends of the same tunnel are KEPT APART: node 1's row says peer .2
	// and node 2's row says peer .1. A merge would have hidden that these are two
	// independent observations.
	if view.Nodes[0].Tunnels[0].PeerIP != "192.168.99.2" || view.Nodes[1].Tunnels[0].PeerIP != "192.168.99.1" {
		t.Errorf("rows were joined across nodes: %+v / %+v", view.Nodes[0].Tunnels[0], view.Nodes[1].Tunnels[0])
	}

	// A token from another tenant must not see tenant t's rows, even though both
	// tenants have a node 1.
	_, otherView := getMetricsReq(t, s, "other", 1, "")
	if otherView.TenantID != "other" {
		t.Fatalf("tenant = %q", otherView.TenantID)
	}
	if len(otherView.Nodes) != 1 || otherView.Nodes[0].NodeID != 1 {
		t.Fatalf("other tenant saw %+v", otherView.Nodes)
	}
	if len(otherView.Nodes[0].Tunnels) != 0 {
		t.Errorf("other tenant node 1 inherited tenant t's rows: %+v", otherView.Nodes[0].Tunnels)
	}
}

func TestMetricsFilterByNode(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")
	if rec := postMetricsReq(t, s, "t", 2, wire.MetricsReport{NodeID: 2, Tunnels: []wire.TunnelMetrics{rowFor(5, "192.168.99.1")}}); rec.Code != 200 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}

	_, view := getMetricsReq(t, s, "t", 1, "node_id=2")
	if view.Self != 1 {
		t.Errorf("Self = %d, want the READER (1)", view.Self)
	}
	if len(view.Nodes) != 1 || view.Nodes[0].NodeID != 2 || len(view.Nodes[0].Tunnels) != 1 {
		t.Fatalf("filtered view = %+v", view.Nodes)
	}

	if rec, _ := getMetricsReq(t, s, "t", 1, "node_id=99"); rec.Code != 404 {
		t.Errorf("unknown node filter = %d, want 404", rec.Code)
	}
	if rec, _ := getMetricsReq(t, s, "t", 1, "node_id=abc"); rec.Code != 400 {
		t.Errorf("bad node filter = %d, want 400", rec.Code)
	}
	// Tenant isolation applies to the FILTER too: "t" has no node 7, and
	// "other" does.
	addNode(s, "other", 7, nil, "")
	if rec, _ := getMetricsReq(t, s, "t", 1, "node_id=7"); rec.Code != 404 {
		t.Errorf("cross-tenant node filter = %d, want 404", rec.Code)
	}
}

func TestMetricsPostRefusesBadReports(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")

	// The certificate says 1; the body claims 2. Accepting this would let one
	// node rewrite a peer's reachability evidence.
	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 2, Tunnels: []wire.TunnelMetrics{rowFor(5, "192.168.99.2")}}); rec.Code != 403 {
		t.Errorf("identity mismatch = %d, want 403", rec.Code)
	}
	// Known tenant, node never enrolled.
	if rec := postMetricsReq(t, s, "t", 3, wire.MetricsReport{NodeID: 3}); rec.Code != 404 {
		t.Errorf("unknown node = %d, want 404", rec.Code)
	}
	// sw_if_index 0 is VPP's "no interface": the row could never be matched to a
	// tunnel or a probe session.
	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 1, Tunnels: []wire.TunnelMetrics{rowFor(0, "192.168.99.2")}}); rec.Code != 400 {
		t.Errorf("sw_if_index 0 = %d, want 400", rec.Code)
	}
	// One window cannot describe the same tunnel twice; a store keyed by
	// sw_if_index would have to pick a winner silently.
	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 1, Tunnels: []wire.TunnelMetrics{
		rowFor(5, "192.168.99.2"), rowFor(5, "192.168.99.9"),
	}}); rec.Code != 400 {
		t.Errorf("duplicate sw_if_index = %d, want 400", rec.Code)
	}
	if len(s.tel) != 0 {
		t.Errorf("a refused report was stored: %+v", s.tel)
	}
}

func TestMetricsReportReplacesPreviousWindow(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 1, Tunnels: []wire.TunnelMetrics{
		rowFor(5, "192.168.99.2"), rowFor(6, "192.168.99.3"),
	}}); rec.Code != 200 {
		t.Fatalf("first post: %d %s", rec.Code, rec.Body)
	}
	if rec := postMetricsReq(t, s, "t", 1, wire.MetricsReport{NodeID: 1, Tunnels: []wire.TunnelMetrics{
		rowFor(6, "192.168.99.3"),
	}}); rec.Code != 200 {
		t.Fatalf("second post: %d %s", rec.Code, rec.Body)
	}
	_, view := getMetricsReq(t, s, "t", 1, "")
	rows := view.Nodes[0].Tunnels
	if len(rows) != 1 || rows[0].SwIfIndex != 6 {
		t.Fatalf("rows = %+v, want only the second window's sw_if 6", rows)
	}
}

func TestMetricsAgeIsComputedAtReadTime(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	now := time.Now().Unix()
	s.tel[key("t", 1)] = &nodeTelemetry{rows: []wire.TunnelMetrics{rowFor(5, "x")}, seenAt: now - 12}

	view := s.telemetryLocked("t", 1, 0, now)
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %+v", view.Nodes)
	}
	// The stored value is 12s old; reading it 5s later must NOT still say 12.
	if view.Nodes[0].AgeSec != 12 {
		t.Fatalf("age = %d, want 12", view.Nodes[0].AgeSec)
	}
	view = s.telemetryLocked("t", 1, 0, now+5)
	if view.Nodes[0].AgeSec != 17 {
		t.Errorf("age after 5s sitting = %d, want 17", view.Nodes[0].AgeSec)
	}
}

func TestTelemetryExpiryAndStaleNodes(t *testing.T) {
	s := newTestServer()
	rec1 := addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")
	now := time.Now().Unix()
	s.tel[key("t", 1)] = &nodeTelemetry{rows: []wire.TunnelMetrics{rowFor(5, "x")}, seenAt: now - 10}
	s.tel[key("t", 2)] = &nodeTelemetry{rows: []wire.TunnelMetrics{rowFor(5, "x")}, seenAt: now - 31}

	s.expireTelemetryLocked(now)
	if _, ok := s.tel[key("t", 1)]; !ok {
		t.Error("fresh window expired")
	}
	if _, ok := s.tel[key("t", 2)]; ok {
		t.Error("window older than ttl survived")
	}

	// A node whose heartbeat lapsed no longer answers; its last window describes
	// a box that is gone, so the view must omit it even if the rows are fresh.
	rec1.Stale = true
	view := s.telemetryLocked("t", 1, 0, now)
	if len(view.Nodes) != 1 || view.Nodes[0].NodeID != 2 {
		t.Errorf("stale node still in view: %+v", view.Nodes)
	}
}

func TestMetricsMethodNotAllowed(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, peerReq(http.MethodDelete, "/v1/metrics", "", "t", 1))
	if rec.Code != 405 {
		t.Fatalf("DELETE = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q", allow)
	}
	// No certificate, no identity.
	rec = httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if rec.Code != 401 {
		t.Errorf("unauthenticated GET = %d, want 401", rec.Code)
	}
}
