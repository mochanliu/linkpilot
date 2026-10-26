// Tests for the console's monitoring read.
//
// Three properties are load-bearing here. The endpoint is tenant-only (the
// operator role is scoped to inventory, exactly like runtime in /api/state).
// A stale device stays LISTED -- "which box fell off" is the question the page
// exists to answer -- but its last window is not shown, because a window that
// stopped arriving does not describe a device that stopped answering. And a
// device that never reported gets an empty row set rather than a synthesized
// one: the page can only be honest if the API is.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

// consoleSession creates a real session in the hub and returns its cookie.
func consoleSession(t *testing.T, s *server, id consoleIdentity) string {
	t.Helper()
	if s.admin == nil {
		s.admin = newAdminHub()
	}
	rec := httptest.NewRecorder()
	s.newSession(rec, id)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("newSession did not set a cookie")
	return ""
}

func consoleMetricsReq(t *testing.T, s *server, sid, method, body string) (*httptest.ResponseRecorder, consoleMetrics) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "/api/metrics", rd)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	s.handleAdminMetrics(rec, r)
	var out consoleMetrics
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func TestConsoleMetricsRoleGate(t *testing.T) {
	s := newTestServer()
	if rec, _ := consoleMetricsReq(t, s, "", http.MethodGet, ""); rec.Code != 401 {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}

	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})
	rec, _ := consoleMetricsReq(t, s, op, http.MethodGet, "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "tenant role required") {
		t.Errorf("operator = %d %q, want 403 tenant role required", rec.Code, rec.Body.String())
	}

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "u", Via: "password"})
	if rec, _ := consoleMetricsReq(t, s, ten, http.MethodGet, ""); rec.Code != 200 {
		t.Errorf("tenant GET = %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = consoleMetricsReq(t, s, ten, http.MethodPost, `{}`)
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Errorf("tenant POST = %d Allow=%q, want 405 GET", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestConsoleMetricsListsOnlyOwnNodesWithTheirWindows(t *testing.T) {
	s := newTestServer()
	now := time.Now().Unix()
	seen := addNode(s, "t", 1, nil, "")
	seen.LastSeen = now - 7
	seen.UptimeSec = 3600
	seen.AgentVer = "sr-agent/1"
	seen.UDPMapping = "203.0.113.7:40000"
	seen.VpnPort = 51820
	pending := addNode(s, "t", 2, nil, "")
	pending.Serial = "never-heartbeated"
	other := addNode(s, "other", 1, nil, "")
	other.LastSeen = now

	s.tel[key("t", 1)] = &nodeTelemetry{rows: []wire.TunnelMetrics{rowFor(5, "192.168.99.2"), rowFor(6, "192.168.99.3")}, seenAt: now - 3}

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "u", Via: "password"})
	rec, out := consoleMetricsReq(t, s, ten, http.MethodGet, "")
	if rec.Code != 200 {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if out.MetricsTTLSec != 30 || out.Controller.HeartbeatSec != 5 {
		t.Errorf("header = %+v ttl=%d", out.Controller, out.MetricsTTLSec)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want tenant t's two nodes only", out.Nodes)
	}
	n1, n2 := out.Nodes[0], out.Nodes[1]
	if n1.NodeID != 1 || n2.NodeID != 2 {
		t.Fatalf("order = %d,%d, want 1,2", n1.NodeID, n2.NodeID)
	}
	if n1.Status != "online" || n1.UptimeSec != 3600 || n1.AgentVer != "sr-agent/1" ||
		n1.UDPMapping != "203.0.113.7:40000" || n1.VpnPort != 51820 {
		t.Errorf("node 1 = %+v", n1)
	}
	if n1.HeartbeatAgeSec < 7 || n1.HeartbeatAgeSec > 8 {
		t.Errorf("heartbeat age = %d, want 7 (or 8 across a second boundary)", n1.HeartbeatAgeSec)
	}
	if n1.TelemetrySeenAt == 0 || n1.TelemetryAgeSec < 3 || n1.TelemetryAgeSec > 4 {
		t.Errorf("window seen_at/age = %d/%d, want now-3/3", n1.TelemetrySeenAt, n1.TelemetryAgeSec)
	}
	if len(n1.Tunnels) != 2 {
		t.Errorf("node 1 rows = %d, want 2", len(n1.Tunnels))
	}
	// A node with no window is INCLUDED with an empty row set, not omitted: the
	// gap ("this device reports nothing") is what the page has to show.
	if n2.Status != "pending" || n2.HeartbeatAgeSec != 0 || n2.UptimeSec != 0 {
		t.Errorf("node 2 = %+v", n2)
	}
	if n2.TelemetrySeenAt != 0 || n2.Tunnels == nil || len(n2.Tunnels) != 0 {
		t.Errorf("node 2 window = seen_at %d tunnels %#v, want never/empty []", n2.TelemetrySeenAt, n2.Tunnels)
	}
	if !strings.Contains(rec.Body.String(), `"tunnels":[]`) {
		t.Errorf("empty row set must serialize as [] not null: %s", rec.Body.String())
	}
}

func TestConsoleMetricsStaleNodeIsListedButLosesItsWindow(t *testing.T) {
	s := newTestServer()
	now := time.Now().Unix()
	rec1 := addNode(s, "t", 1, nil, "")
	rec1.LastSeen = now - 40
	rec1.Stale = true
	rec1.UptimeSec = 7200
	// The sweeper normally deletes this; the read must not depend on winning
	// that race.
	s.tel[key("t", 1)] = &nodeTelemetry{rows: []wire.TunnelMetrics{rowFor(5, "x")}, seenAt: now - 2}

	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "u", Via: "password"})
	_, out := consoleMetricsReq(t, s, ten, http.MethodGet, "")
	if len(out.Nodes) != 1 {
		t.Fatalf("nodes = %+v, want the stale device still listed", out.Nodes)
	}
	n := out.Nodes[0]
	if n.Status != "stale" {
		t.Errorf("status = %q, want stale", n.Status)
	}
	if n.HeartbeatAgeSec != 40 {
		t.Errorf("heartbeat age = %d, want 40", n.HeartbeatAgeSec)
	}
	if n.TelemetrySeenAt != 0 || len(n.Tunnels) != 0 {
		t.Errorf("stale node still exposes a window: %+v", n)
	}
}

func TestHeartbeatStoresAgentUptime(t *testing.T) {
	s := newTestServer()
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	rec := addNode(s, "t", 1, nil, "")

	beat := func(uptime int) *httptest.ResponseRecorder {
		body := `{"node_id":1,"uptime_sec":` + strconv.Itoa(uptime) + `}`
		out := httptest.NewRecorder()
		s.handleHeartbeat(out, peerReq(http.MethodPost, "/v1/heartbeat", body, "t", 1))
		return out
	}

	if out := beat(3600); out.Code != 200 {
		t.Fatalf("heartbeat = %d %s", out.Code, out.Body.String())
	}
	if rec.UptimeSec != 3600 {
		t.Errorf("uptime = %d, want 3600", rec.UptimeSec)
	}
	// Zero means "not reported" (the --once path), NOT "just booted": it must
	// not erase a real uptime from the monitoring view.
	if out := beat(0); out.Code != 200 || rec.UptimeSec != 3600 {
		t.Errorf("zero uptime erased the stored value: %d %d", out.Code, rec.UptimeSec)
	}
	if out := beat(-5); out.Code != 200 || rec.UptimeSec != 3600 {
		t.Errorf("negative uptime changed the stored value: %d %d", out.Code, rec.UptimeSec)
	}
	// A smaller value really is a report: the agent restarted.
	if out := beat(5); out.Code != 200 || rec.UptimeSec != 5 {
		t.Errorf("restart uptime = %d, want 5", rec.UptimeSec)
	}
}
