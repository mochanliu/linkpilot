package main

// The seams added in O5 are thin, but they are the ones an operator types into,
// and a whole-replacement PUT makes the cost of a wrong read shape large: a
// misspelled field or a body that decodes as an empty table would silently
// withdraw every tunnel. So these tests drive the real route.Client over httptest
// rather than a fake -- the interesting behaviour is what the client does to its
// own tables, and a fake could only restate the handler.
//
// None of them runs the client: with no RR session the active table is empty by
// design, which is exactly the state these endpoints have to be usable in.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sdwan/controller/internal/route"
)

func testClient(t *testing.T, cfg route.ClientConfig) *route.Client {
	t.Helper()
	if cfg.Tenant == "" {
		cfg.Tenant = "tenant-a"
	}
	if cfg.DeviceID == 0 {
		cfg.DeviceID = 10
	}
	if len(cfg.RRs) == 0 {
		cfg.RRs = []route.RRConfig{{ID: "rr-a", Addr: "127.0.0.1:1"}}
	}
	if cfg.Dial == nil {
		// Never reached: these tests do not call Run. A real error instead of a
		// nil Dial keeps NewClient's validation honest.
		cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("no rr in this test")
		}
	}
	c, err := route.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func newServer(t *testing.T, c *route.Client) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(statusMux(c))
	t.Cleanup(srv.Close)
	return srv
}

// call returns the status code and the raw body. It never assumes 200: the
// error text is part of what an operator reads.
func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func getStatus(t *testing.T, srv *httptest.Server) statusDoc {
	t.Helper()
	code, raw := call(t, srv, http.MethodGet, "/status", "")
	if code != http.StatusOK {
		t.Fatalf("GET /status = %d: %s", code, raw)
	}
	var doc statusDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode /status: %v\n%s", err, raw)
	}
	return doc
}

func TestStatusReportsTheTablesInForce(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		VRFs: []route.VRFConfig{
			{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
			{ID: 6, ExportRTs: []string{"65000:6"}}, // a hub VRF: exports only
		},
		Tunnels: []route.Tunnel{
			{Peer: 22, NextHop: "192.168.100.2"},
			{Peer: 20, NextHop: "192.168.99.2"},
		},
		LocalPrefixes: []route.LocalPrefix{{VRFID: 5, Prefix: "10.10.0.0/16"}},
	})
	srv := newServer(t, c)

	// No session was ever run, so the table is empty and nothing is pending --
	// but every list still has to be present as an empty array, or a console
	// cannot tell "nothing held back" from "this build does not report it".
	doc := getStatus(t, srv)
	if doc.ActiveRR != -1 || doc.ActiveRIBRoutes != 0 || doc.ActivePending != 0 {
		t.Fatalf("idle stats = %+v", doc.ClientStats)
	}
	if len(doc.Tunnels) != 2 || doc.Tunnels[0].Peer != 20 || doc.Tunnels[1].Peer != 22 {
		t.Fatalf("tunnels = %+v", doc.Tunnels)
	}
	if len(doc.VRFs) != 2 || doc.VRFs[0].ID != 5 || len(doc.VRFs[1].ImportRTs) != 0 {
		t.Fatalf("vrfs = %+v", doc.VRFs)
	}
	if len(doc.Local) != 1 || doc.Local[0].Prefix != "10.10.0.0/16" {
		t.Fatalf("local = %+v", doc.Local)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(mustGet(t, srv)), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pending", "local_skips", "tunnels", "vrfs", "local", "rib"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("/status has no %q field", key)
		}
	}
	for _, key := range []string{"pending", "local_skips"} {
		if list, ok := raw[key].([]any); !ok || len(list) != 0 {
			t.Fatalf("%s = %#v, want an empty array", key, raw[key])
		}
	}
}

func mustGet(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	code, raw := call(t, srv, http.MethodGet, "/status", "")
	if code != http.StatusOK {
		t.Fatalf("GET /status = %d", code)
	}
	return raw
}

// PUT /tunnels is the entry point that makes "补一条隧道就能学到前缀" possible from
// outside the process, so the replacement semantics and the rejection paths are
// the two things worth pinning here.
func TestPutTunnelsReplacesAndRejects(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		Tunnels: []route.Tunnel{{Peer: 20, NextHop: "192.168.99.2"}},
	})
	srv := newServer(t, c)

	code, raw := call(t, srv, http.MethodPut, "/tunnels",
		`{"tunnels":[{"peer_device_id":22,"next_hop":"192.168.100.2"},{"peer_device_id":20,"next_hop":"192.168.99.2"}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /tunnels = %d: %s", code, raw)
	}
	var echoed tunnelsDoc
	if err := json.Unmarshal([]byte(raw), &echoed); err != nil {
		t.Fatalf("decode response: %v\n%s", err, raw)
	}
	if len(echoed.Tunnels) != 2 || echoed.Tunnels[0].Peer != 20 {
		t.Fatalf("echoed = %+v", echoed.Tunnels)
	}
	if len(c.Tunnels()) != 2 {
		t.Fatalf("client table = %v", c.Tunnels())
	}

	// Rejected: a tunnel to ourselves would make the data plane point at this
	// box. The table must survive untouched.
	code, raw = call(t, srv, http.MethodPut, "/tunnels",
		`{"tunnels":[{"peer_device_id":10,"next_hop":"192.168.10.2"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("self tunnel = %d: %s", code, raw)
	}
	if !strings.Contains(raw, "peer 10") {
		t.Fatalf("error text = %s", raw)
	}
	if got := getStatus(t, srv).Tunnels; len(got) != 2 || got[1].Peer != 22 {
		t.Fatalf("rejected PUT changed the table: %+v", got)
	}

	// Rejected: a non-IPv4 next hop. The RR would not be able to use it, and
	// "resolved to nothing" is not the same as "no tunnel" for the held list.
	code, raw = call(t, srv, http.MethodPut, "/tunnels",
		`{"tunnels":[{"peer_device_id":23,"next_hop":"not-an-address"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad next hop = %d: %s", code, raw)
	}

	// A misspelled field must not read as an empty table: that would withdraw
	// every tunnel through a body that looked fine.
	code, raw = call(t, srv, http.MethodPut, "/tunnels",
		`{"tunnels":[{"peer":22,"next_hop":"192.168.100.2"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "unknown field") {
		t.Fatalf("unknown field = %d: %s", code, raw)
	}
	if got := getStatus(t, srv).Tunnels; len(got) != 2 {
		t.Fatalf("rejected PUT changed the table: %+v", got)
	}

	// Clearing the table is a legitimate PUT and stays a 200 with an empty array.
	code, raw = call(t, srv, http.MethodPut, "/tunnels", `{"tunnels":[]}`)
	if code != http.StatusOK || !strings.Contains(raw, `"tunnels": []`) {
		t.Fatalf("clear = %d: %s", code, raw)
	}
	if len(c.Tunnels()) != 0 {
		t.Fatalf("table not cleared: %v", c.Tunnels())
	}
}

// The VRF table decides both which received prefixes leave the pending list and
// what local prefixes may be announced at all. Removing a VRF has to withdraw
// its prefixes rather than leave them announced under a policy nobody configured
// -- and they have to come back, and be reported the whole time.
func TestPutVrfsWithdrawsAndRestoresLocalRoutes(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		VRFs: []route.VRFConfig{
			{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
		},
		LocalPrefixes: []route.LocalPrefix{{VRFID: 5, Prefix: "10.10.0.0/16"}},
	})
	srv := newServer(t, c)
	if n := len(getStatus(t, srv).Local); n != 1 {
		t.Fatalf("local = %d, want 1", n)
	}

	code, raw := call(t, srv, http.MethodPut, "/vrfs", `{"vrfs":[]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /vrfs = %d: %s", code, raw)
	}
	doc := getStatus(t, srv)
	if len(doc.Local) != 0 {
		t.Fatalf("prefix survived its VRF: %+v", doc.Local)
	}
	if doc.LocalSkipsTotal != 1 || len(doc.LocalSkips) != 1 ||
		doc.LocalSkips[0].Reason != route.SkipVRFUnconfigured {
		t.Fatalf("skips = %+v (total %d)", doc.LocalSkips, doc.LocalSkipsTotal)
	}
	if doc.LocalSkips[0].Prefix != "10.10.0.0/16" || doc.LocalSkips[0].VRFID != 5 {
		t.Fatalf("skip = %+v", doc.LocalSkips[0])
	}

	// Back without a restart: the prefix is announced again.
	code, raw = call(t, srv, http.MethodPut, "/vrfs",
		`{"vrfs":[{"id":5,"export_rts":["65000:5"],"import_rts":["65000:6"]}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /vrfs again = %d: %s", code, raw)
	}
	doc = getStatus(t, srv)
	if len(doc.Local) != 1 || doc.LocalSkipsTotal != 0 || len(doc.LocalSkips) != 0 {
		t.Fatalf("after restore: local=%+v skips=%+v", doc.Local, doc.LocalSkips)
	}

	// Rejected: an unparseable RT must leave the table in force unchanged, or the
	// operator cannot tell which policy was applied.
	code, raw = call(t, srv, http.MethodPut, "/vrfs",
		`{"vrfs":[{"id":5,"export_rts":["65000:5"],"import_rts":["not-an-rt"]}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad rt = %d: %s", code, raw)
	}
	doc = getStatus(t, srv)
	if len(doc.VRFs) != 1 || len(doc.VRFs[0].ImportRTs) != 1 || doc.VRFs[0].ImportRTs[0] != "65000:6" {
		t.Fatalf("rejected PUT changed the table: %+v", doc.VRFs)
	}
	if len(doc.Local) != 1 {
		t.Fatalf("rejected PUT changed the announcements: %+v", doc.Local)
	}
}

// GET and PUT share one shape per table, so the body an operator reads out of
// /status is a body they can PUT back.
func TestStatusListsRoundTripThroughPut(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		VRFs: []route.VRFConfig{
			{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
			{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:5"}},
		},
		Tunnels: []route.Tunnel{{Peer: 20, NextHop: "192.168.99.2"}},
	})
	srv := newServer(t, c)
	doc := getStatus(t, srv)

	vrfBody, err := json.Marshal(vrfsDoc{VRFs: doc.VRFs})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := call(t, srv, http.MethodPut, "/vrfs", string(vrfBody))
	if code != http.StatusOK {
		t.Fatalf("PUT the read-back VRFs = %d: %s", code, raw)
	}
	tunBody, err := json.Marshal(tunnelsDoc{Tunnels: doc.Tunnels})
	if err != nil {
		t.Fatal(err)
	}
	code, raw = call(t, srv, http.MethodPut, "/tunnels", string(tunBody))
	if code != http.StatusOK {
		t.Fatalf("PUT the read-back tunnels = %d: %s", code, raw)
	}
	after := getStatus(t, srv)
	if len(after.VRFs) != len(doc.VRFs) || len(after.Tunnels) != len(doc.Tunnels) {
		t.Fatalf("round trip changed the tables: %+v vs %+v", after.Tunnels, doc.Tunnels)
	}
}

// Truncation has to be visible as truncation: a bounded list that looks complete
// is worse than a long one.
func TestHeldListIsBoundedAndSaysSo(t *testing.T) {
	const n = maxStatusList + 1
	lps := make([]route.LocalPrefix, 0, n)
	for i := 0; i < n; i++ {
		lps = append(lps, route.LocalPrefix{VRFID: 5, Prefix: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)})
	}
	c := testClient(t, route.ClientConfig{
		VRFs:          []route.VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}}},
		LocalPrefixes: lps,
	})
	srv := newServer(t, c)
	if got := getStatus(t, srv).LocalSkipsTotal; got != 0 {
		t.Fatalf("skips before = %d", got)
	}

	// Drop the only VRF: every configured prefix becomes unspeakable at once.
	code, raw := call(t, srv, http.MethodPut, "/vrfs", `{"vrfs":[]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /vrfs = %d: %s", code, raw)
	}
	doc := getStatus(t, srv)
	if doc.LocalSkipsTotal != n {
		t.Fatalf("total = %d, want %d", doc.LocalSkipsTotal, n)
	}
	if len(doc.LocalSkips) != maxStatusList || !doc.LocalSkipsTruncated {
		t.Fatalf("list = %d entries, truncated=%v", len(doc.LocalSkips), doc.LocalSkipsTruncated)
	}
	// Sorted, so the page that is shown is a deterministic one.
	if doc.LocalSkips[0].Prefix != "10.0.0.0/24" {
		t.Fatalf("first skip = %+v", doc.LocalSkips[0])
	}
	if doc.PendingTotal != 0 || len(doc.Pending) != 0 || doc.PendingTruncated {
		t.Fatalf("pending = %+v total=%d", doc.Pending, doc.PendingTotal)
	}
}
