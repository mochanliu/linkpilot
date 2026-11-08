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

// testClientWithLocal is testClient plus the local set sr-frr would have pushed.
func testClientWithLocal(t *testing.T, cfg route.ClientConfig, lps ...route.LocalPrefix) *route.Client {
	t.Helper()
	c := testClient(t, cfg)
	if err := c.SetLocalPrefixes(lps); err != nil {
		t.Fatalf("SetLocalPrefixes: %v", err)
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
	c := testClientWithLocal(t, route.ClientConfig{
		VRFs: []route.VRFConfig{
			{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
			{ID: 6, ExportRTs: []string{"65000:6"}}, // a hub VRF: exports only
		},
		Tunnels: []route.Tunnel{
			{Peer: 22, NextHop: "192.168.100.2"},
			{Peer: 20, NextHop: "192.168.99.2"},
		},
	}, route.LocalPrefix{VRFID: 5, Prefix: "10.10.0.0/16"})
	srv := newServer(t, c)

	// No session was ever run, so the table is empty and nothing is pending --
	// but every list still has to be present as an empty array, or a console
	// cannot tell "nothing held back" from "this build does not report it".
	doc := getStatus(t, srv)
	if doc.RibReady || doc.RibRoutes != 0 {
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
	c := testClientWithLocal(t, route.ClientConfig{
		VRFs: []route.VRFConfig{
			{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
		},
	}, route.LocalPrefix{VRFID: 5, Prefix: "10.10.0.0/16"})
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

// PUT /local-prefixes is the only way a local prefix gets announced: sr-frr
// publishes what the in-box FRR learned and the box's own config carries no
// prefix list. So the seam has to replace (not merge) and it has to refuse a
// set whose VRF nobody has published.
func TestPutLocalPrefixesReplacesAndValidates(t *testing.T) {
	c := testClientWithLocal(t, route.ClientConfig{
		VRFs: []route.VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}}},
	}, route.LocalPrefix{VRFID: 5, Prefix: "10.10.0.0/16"})
	srv := newServer(t, c)

	code, raw := call(t, srv, http.MethodPut, "/local-prefixes",
		`{"prefixes":[{"vrf_id":5,"prefix":"10.20.0.0/16"},{"vrf_id":5,"prefix":"10.30.0.0/16"}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /local-prefixes = %d: %s", code, raw)
	}
	doc := getStatus(t, srv)
	if len(doc.Local) != 2 || doc.Local[0].Prefix != "10.20.0.0/16" || doc.Local[1].Prefix != "10.30.0.0/16" {
		t.Fatalf("local = %+v, want the replaced set only", doc.Local)
	}

	// A misspelled field must not read as "withdraw everything".
	code, raw = call(t, srv, http.MethodPut, "/local-prefixes",
		`{"prefix":[{"vrf_id":5,"prefix":"10.40.0.0/16"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "unknown field") {
		t.Fatalf("unknown field = %d: %s", code, raw)
	}
	// A VRF the table does not have is a 400, and leaves the set alone.
	code, raw = call(t, srv, http.MethodPut, "/local-prefixes",
		`{"prefixes":[{"vrf_id":9,"prefix":"10.0.0.0/8"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "unconfigured vrf") {
		t.Fatalf("unknown vrf = %d: %s", code, raw)
	}
	if got := getStatus(t, srv).Local; len(got) != 2 {
		t.Fatalf("a rejected PUT changed the announcements: %+v", got)
	}

	// GET reads back the same shape, so sr-frr's own debugging and the console
	// both see the set in force.
	code, raw = call(t, srv, http.MethodGet, "/local-prefixes", "")
	if code != http.StatusOK || !strings.Contains(raw, "10.20.0.0/16") {
		t.Fatalf("GET /local-prefixes = %d: %s", code, raw)
	}
}

// Slice A #276: sr-frr's fast path is a batched delta, not a whole-table PUT.
// Endpoint has to accept a mixed add/del batch and reject one malformed line
// atomically -- the same all-or-nothing posture as /local-prefixes, because a
// half-applied delta would leave sr-route with a set that neither the last
// ack nor the next full announce describes.
func TestPutLocalPrefixDeltaReplacesAndValidates(t *testing.T) {
	c := testClientWithLocal(t, route.ClientConfig{
		VRFs: []route.VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}}},
	}, route.LocalPrefix{VRFID: 5, Prefix: "10.10.0.0/16"})
	srv := newServer(t, c)

	code, raw := call(t, srv, http.MethodPut, "/local-prefixes/delta",
		`{"deltas":[{"vrf_id":5,"prefix":"10.10.0.0/16","op":"del"},{"vrf_id":5,"prefix":"10.30.0.0/16","op":"add"}]}`)
	if code != http.StatusOK {
		t.Fatalf("delta = %d: %s", code, raw)
	}
	if got := getStatus(t, srv).Local; len(got) != 1 || got[0].Prefix != "10.30.0.0/16" {
		t.Fatalf("after delta local = %+v", got)
	}

	// One bad op rejects the whole batch. 10.40 must not have been added.
	code, raw = call(t, srv, http.MethodPut, "/local-prefixes/delta",
		`{"deltas":[{"vrf_id":5,"prefix":"10.40.0.0/16","op":"add"},{"vrf_id":5,"prefix":"10.30.0.0/16","op":"drop"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "not add/del") {
		t.Fatalf("bad op = %d: %s", code, raw)
	}
	if got := getStatus(t, srv).Local; len(got) != 1 || got[0].Prefix != "10.30.0.0/16" {
		t.Fatalf("rejected batch changed set: %+v", got)
	}

	// ADD against an unconfigured VRF is a 400 (same validation path as full).
	code, raw = call(t, srv, http.MethodPut, "/local-prefixes/delta",
		`{"deltas":[{"vrf_id":9,"prefix":"10.99.0.0/16","op":"add"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "unconfigured vrf") {
		t.Fatalf("add unknown vrf = %d: %s", code, raw)
	}

	// GET is not the delta's shape; only PUT is accepted here so the
	// whole-table endpoint stays the sole read path.
	code, _ = call(t, srv, http.MethodGet, "/local-prefixes/delta", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET delta = %d, want 405", code)
	}
}

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
	c := testClientWithLocal(t, route.ClientConfig{
		VRFs: []route.VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}}},
	}, lps...)
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

// PUT /rrs is the seam C-c exists for: 逐台换反射器 must never mean restarting
// sr-route, and must never mean the box briefly has no reflector at all. The
// switch is make-before-break, so the handler cannot wait for the new RR inside the
// request -- sr-apply budgets 10 s per hot update (internal/apply/route.go,
// routePushTimeout) -- and answers 202 with the unfinished half in rr_wait.
func TestPutRRsStartsTheSwitchAndSaysItIsUnfinished(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		RRs: []route.RRConfig{{ID: "rr1", Addr: "192.0.2.10:20200"}},
	})
	srv := newServer(t, c)

	// GET first: the ids come back, because 主 and 备 are only distinguishable by
	// the label the tenant picked in the console.
	code, raw := call(t, srv, http.MethodGet, "/rrs", "")
	if code != http.StatusOK {
		t.Fatalf("GET /rrs = %d: %s", code, raw)
	}
	var before rrsDoc
	if err := json.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatalf("decode GET /rrs: %v\n%s", err, raw)
	}
	if len(before.RRs) != 1 || before.RRs[0].ID != "rr1" || before.RRs[0].Addr != "192.0.2.10:20200" {
		t.Fatalf("GET /rrs = %+v", before.RRs)
	}
	if before.RRWait != nil {
		t.Fatalf("nothing was in flight but rr_wait is set: %+v", before.RRWait)
	}

	// Nothing can establish in this test -- Dial always fails -- so this switch
	// stays unfinished by construction, which is exactly the state that must be
	// reportable rather than look like a done deal.
	code, raw = call(t, srv, http.MethodPut, "/rrs",
		`{"rrs":[{"id":"rr1","addr":"192.0.2.11:20200"}]}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT /rrs = %d, want 202: %s", code, raw)
	}
	var after rrsDoc
	if err := json.Unmarshal([]byte(raw), &after); err != nil {
		t.Fatalf("decode PUT /rrs: %v\n%s", err, raw)
	}
	if len(after.RRs) != 1 || after.RRs[0].Addr != "192.0.2.11:20200" {
		t.Fatalf("wanted set = %+v", after.RRs)
	}
	if after.RRWait == nil {
		t.Fatalf("换 RR 没完成，PUT 体里却没有 rr_wait: %s", raw)
	}
	if len(after.RRWait.PendingAddrs) != 1 || after.RRWait.PendingAddrs[0] != "192.0.2.11:20200" {
		t.Fatalf("pending = %v", after.RRWait.PendingAddrs)
	}
	if len(after.RRWait.RetiringAddrs) != 1 || after.RRWait.RetiringAddrs[0] != "192.0.2.10:20200" {
		t.Fatalf("retiring = %v", after.RRWait.RetiringAddrs)
	}

	// /status carries the same field: that is the page sr-apply's note points an
	// operator at, and the two must not disagree about what is in flight.
	doc := getStatus(t, srv)
	if doc.RRWait == nil || doc.RRWait.RetiringAddrs[0] != "192.0.2.10:20200" {
		t.Fatalf("/status rr_wait = %+v", doc.RRWait)
	}
	// Both sessions are listed: the replacement is being tried, and the old one is
	// still standing until it succeeds.
	if len(doc.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want the new one plus the retiring old one", doc.Sessions)
	}
	if doc.Sessions[0].Addr != "192.0.2.11:20200" || !doc.Sessions[1].Retiring {
		t.Fatalf("session order/flags = %+v", doc.Sessions)
	}

	// Cancelling the switch mid-flight is a PUT of the old set. The session that was
	// on its way out is wanted again and stops retiring -- but the .11 session this
	// box started dialling a moment ago is now the one on its way out, and in this
	// test nothing can ever establish (Dial always fails), so it lingers. That is
	// make-before-break, not a leak: see internal/route SetRRs, where the same
	// rollback against a live session clears the wait at once.
	code, raw = call(t, srv, http.MethodPut, "/rrs",
		`{"rrs":[{"id":"rr1","addr":"192.0.2.10:20200"}]}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT the old set back = %d: %s", code, raw)
	}
	if err := json.Unmarshal([]byte(raw), &after); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if len(after.RRs) != 1 || after.RRs[0].Addr != "192.0.2.10:20200" {
		t.Fatalf("rrs after rollback = %+v", after.RRs)
	}
	if after.RRWait == nil ||
		len(after.RRWait.RetiringAddrs) != 1 ||
		after.RRWait.RetiringAddrs[0] != "192.0.2.11:20200" {
		t.Fatalf("rr_wait after rollback = %+v", after.RRWait)
	}
	if sn := getStatus(t, srv).Sessions; len(sn) != 2 || sn[0].Addr != "192.0.2.10:20200" ||
		sn[0].Retiring || !sn[1].Retiring {
		t.Fatalf("sessions after rollback = %+v", sn)
	}
}

// The body read out of GET /rrs is a body that can be PUT back -- the same rule the
// other two tables follow, so an operator (or a console) can copy one device's
// reflector set onto another without hand-writing JSON.
func TestRRsRoundTripThroughPut(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		RRs: []route.RRConfig{
			{ID: "rr1", Addr: "192.0.2.10:20200"},
			{ID: "rr2", Addr: "192.0.2.11:20200"},
		},
	})
	srv := newServer(t, c)
	code, raw := call(t, srv, http.MethodGet, "/rrs", "")
	if code != http.StatusOK {
		t.Fatalf("GET /rrs = %d: %s", code, raw)
	}

	code, raw = call(t, srv, http.MethodPut, "/rrs", raw)
	if code != http.StatusAccepted {
		t.Fatalf("PUT the read-back set = %d: %s", code, raw)
	}
	var after rrsDoc
	if err := json.Unmarshal([]byte(raw), &after); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if len(after.RRs) != 2 || after.RRs[0].ID != "rr1" || after.RRs[1].Addr != "192.0.2.11:20200" {
		t.Fatalf("set after round trip = %+v", after.RRs)
	}
	// Re-setting the same addresses reuses both sessions, so nothing is retiring
	// and an idling device must not look like it is mid-switch.
	if after.RRWait != nil {
		t.Fatalf("a no-op switch reported as in flight: %+v", after.RRWait)
	}
}

// An empty reflector set is the one shape this endpoint must never accept: sr-route
// refuses to START without an RR (the same file would be unreadable at next boot),
// so a PUT that emptied it would look fine right up until the box rebooted.
func TestPutRRsRejectsASetThatWouldStrandTheBox(t *testing.T) {
	c := testClient(t, route.ClientConfig{
		RRs: []route.RRConfig{{ID: "rr1", Addr: "192.0.2.10:20200"}},
	})
	srv := newServer(t, c)

	cases := []struct{ name, body, want string }{
		{"empty set", `{"rrs":[]}`, "at least one RR"},
		{"absent key", `{}`, "at least one RR"},
		{"same address twice",
			`{"rrs":[{"id":"a","addr":"192.0.2.11:20200"},{"id":"b","addr":"192.0.2.11:20200"}]}`,
			"configured twice"},
		{"no address", `{"rrs":[{"id":"a"}]}`, "no address"},
		{"misspelled field", `{"rrs":[{"id":"a","address":"192.0.2.11:20200"}]}`, "unknown field"},
	}
	for _, tc := range cases {
		code, raw := call(t, srv, http.MethodPut, "/rrs", tc.body)
		if code != http.StatusBadRequest || !strings.Contains(raw, tc.want) {
			t.Fatalf("%s = %d: %s, want 400 containing %q", tc.name, code, raw, tc.want)
		}
	}

	// A rejected PUT has to leave the working set exactly where it was. Half an
	// apply here would strand the box on a reflector nobody chose.
	code, raw := call(t, srv, http.MethodGet, "/rrs", "")
	if code != http.StatusOK {
		t.Fatalf("GET /rrs = %d: %s", code, raw)
	}
	var doc rrsDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if len(doc.RRs) != 1 || doc.RRs[0].ID != "rr1" || doc.RRs[0].Addr != "192.0.2.10:20200" {
		t.Fatalf("set after rejected PUTs = %+v", doc.RRs)
	}
	if doc.RRWait != nil {
		t.Fatalf("a rejected PUT started a switch: %+v", doc.RRWait)
	}

	// GET and PUT only: a DELETE would be the one operation this seam cannot do
	// honestly, because there is no set the box could fall back to.
	if code, _ := call(t, srv, http.MethodDelete, "/rrs", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /rrs = %d, want 405", code)
	}
}
