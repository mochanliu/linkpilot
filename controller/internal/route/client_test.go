package route

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClientConfig() ClientConfig {
	return ClientConfig{
		Tenant:   "tenant-a",
		DeviceID: 10,
		VRFs: []VRFConfig{{
			ID:        5,
			ExportRTs: []string{"65000:5"},
			ImportRTs: []string{"65000:6"},
		}},
		LocalPrefixes: []LocalPrefix{{VRFID: 5, Prefix: "10.10.0.0/16"}},
		Tunnels:       []Tunnel{{Peer: 20, NextHop: "192.168.99.2"}},
		RRs:           []RRConfig{{ID: "rr-a", Addr: "rr-a"}},
		Dial:          func(context.Context, string) (net.Conn, error) { return nil, nil },
	}
}

func startClient(t *testing.T, cfg ClientConfig) *Client {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 5*time.Second, what, cond)
}

// waitForWithin is waitFor with a chosen budget, for the cases where the thing
// being waited for is a reconnect: the backoff caps at 5 s, so "RR is back and
// carrying the new table" can legitimately take longer than the default.
func waitForWithin(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ribPrefixes(routes []RIBEntry) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Prefix+"@"+strings.Join(r.NextHops, ","))
	}
	return out
}

func TestNewClientValidation(t *testing.T) {
	base := func() ClientConfig { return testClientConfig() }
	cases := []struct {
		name string
		mut  func(*ClientConfig)
		want string
	}{
		{"no tenant", func(c *ClientConfig) { c.Tenant = "" }, "tenant"},
		{"device 0", func(c *ClientConfig) { c.DeviceID = 0 }, "device_id"},
		{"no dial", func(c *ClientConfig) { c.Dial = nil }, "Dial"},
		{"no rrs", func(c *ClientConfig) { c.RRs = nil }, "at least one RR"},
		{"rr without addr", func(c *ClientConfig) { c.RRs = []RRConfig{{ID: "x"}} }, "no address"},
		{"bad hold", func(c *ClientConfig) { c.HoldSec = 2 }, "hold_sec"},
		{"duplicate vrf", func(c *ClientConfig) {
			c.VRFs = append(c.VRFs, c.VRFs[0])
		}, "twice"},
		{"bad export rt", func(c *ClientConfig) { c.VRFs[0].ExportRTs = []string{"x"} }, "bad RT"},
		{"bad import rt", func(c *ClientConfig) { c.VRFs[0].ImportRTs = []string{"1:"} }, "bad RT"},
		{"unknown vrf", func(c *ClientConfig) { c.LocalPrefixes = []LocalPrefix{{VRFID: 9, Prefix: "10.0.0.0/8"}} }, "unconfigured vrf"},
		{"no export rts", func(c *ClientConfig) {
			c.VRFs[0].ExportRTs = nil
		}, "no export RTs"},
		{"bad local prefix", func(c *ClientConfig) { c.LocalPrefixes[0].Prefix = "10.10.1.1/16" }, "not canonical"},
		{"tunnel to this device", func(c *ClientConfig) {
			c.Tunnels = []Tunnel{{Peer: 10, NextHop: "192.168.99.9"}}
		}, "this device"},
		{"tunnel peer 0", func(c *ClientConfig) {
			c.Tunnels = []Tunnel{{Peer: 0, NextHop: "192.168.99.2"}}
		}, "peer_device_id"},
		{"bad tunnel next hop", func(c *ClientConfig) {
			c.Tunnels = []Tunnel{{Peer: 20, NextHop: "192.168.099.2"}}
		}, "bad next_hop"},
		{"duplicate tunnel", func(c *ClientConfig) {
			c.Tunnels = []Tunnel{{Peer: 20, NextHop: "192.168.99.2"}, {Peer: 20, NextHop: "192.168.99.2"}}
		}, "ascending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			_, err := NewClient(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestClientWithoutTunnelsIsValid(t *testing.T) {
	// A device that only exports (or has not been given a tunnel yet) must
	// still build: "no tunnels" is a legitimate table, not a config error.
	cfg := testClientConfig()
	cfg.Tunnels = nil
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if n := len(c.Tunnels()); n != 0 {
		t.Fatalf("tunnels = %d, want 0", n)
	}
	if n := len(c.LocalRoutes()); n != 1 {
		t.Fatalf("local routes = %d", n)
	}
}

func TestTunnelsAreCanonicalized(t *testing.T) {
	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 21, NextHop: "192.168.99.9"},
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 20, NextHop: "192.168.77.1"},
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got := c.Tunnels()
	want := []Tunnel{
		{Peer: 20, NextHop: "192.168.77.1"},
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.9"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tunnels[%d] = %+v, want %+v (whole table %+v)", i, got[i], want[i], got)
		}
	}
	if cfg.Tunnels[0].Peer != 21 {
		t.Fatal("NewClient reordered the caller's slice in place")
	}
}

func TestLocalRoutesArePathFree(t *testing.T) {
	cfg := testClientConfig()
	cfg.VRFs[0].ExportRTs = []string{"65000:50", "65000:5"}
	cfg.VRFs = append(cfg.VRFs, VRFConfig{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:7"}})
	cfg.LocalPrefixes = append(cfg.LocalPrefixes, LocalPrefix{VRFID: 6, Prefix: "10.20.0.0/16"})
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got := c.LocalRoutes()
	if len(got) != 2 {
		t.Fatalf("local routes = %d, want 1 per prefix (2)", len(got))
	}
	for _, rt := range got {
		if rt.DeviceID != 10 {
			t.Fatalf("route device = %d", rt.DeviceID)
		}
		if err := rt.Validate(); err != nil {
			t.Fatalf("local route invalid: %v", err)
		}
		if rt.VRFID == 5 {
			if len(rt.RTs) != 2 || rt.RTs[0] != "65000:5" || rt.RTs[1] != "65000:50" {
				t.Fatalf("vrf 5 exported %v, want both RTs in canonical order", rt.RTs)
			}
		}
	}
}

// testRRSrv fronts a real Reflector[] over net.Pipe and can be taken down to
// simulate an RR outage.
type testRRSrv struct {
	t        *testing.T
	r        *Reflector
	node     uint16
	incoming chan net.Conn
	down     atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
}

func newTestRRSrv(t *testing.T, r *Reflector, node uint16) *testRRSrv {
	s := &testRRSrv{t: t, r: r, node: node, incoming: make(chan net.Conn, 16)}
	go func() {
		for srv := range s.incoming {
			go func(conn net.Conn) {
				_ = s.r.ServeConn(conn, PeerIdentity{Tenant: s.r.tenant, DeviceID: s.node})
			}(srv)
		}
	}()
	return s
}

func (s *testRRSrv) dial(ctx context.Context, addr string) (net.Conn, error) {
	if s.down.Load() {
		return nil, errors.New("rr is down")
	}
	srv, cli := net.Pipe()
	s.mu.Lock()
	s.conns = append(s.conns, srv)
	s.mu.Unlock()
	select {
	case s.incoming <- srv:
		return cli, nil
	case <-ctx.Done():
		srv.Close()
		cli.Close()
		return nil, ctx.Err()
	}
}

func (s *testRRSrv) kill() {
	s.down.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *testRRSrv) revive() { s.down.Store(false) }

func TestClientAnnouncesItsTable(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)

	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 20, NextHop: "192.168.77.1"},
	}
	cfg.Dial = srv.dial
	c := startClient(t, cfg)

	waitFor(t, "rr to receive the announced table", func() bool { return len(rr.RoutesOf(10)) == 1 })
	got := rr.RoutesOf(10)
	want := c.LocalRoutes()
	if len(want) != 1 {
		t.Fatalf("client local routes = %d", len(want))
	}
	if !got[0].Equal(want[0]) {
		t.Fatalf("rr stored %+v, want %+v", got[0], want[0])
	}
	// The tunnel table travels once, as its own message, not with the prefix.
	waitFor(t, "rr to see the tunnel table", func() bool {
		sn := rr.Sessions()
		return len(sn) == 1 && sn[0].Tunnels == 2
	})
	waitFor(t, "session synced", func() bool {
		st := c.Stats()
		return st.ActiveRR == 0 && st.Sessions[0].Synced && st.Sessions[0].RRID == "rr-a"
	})
}

func TestClientImportsOnlyMatchingRTs(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "client synced", func() bool { return c.Stats().ActiveRR == 0 })

	peer, _ := dial(t, rr, 20)
	peer.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	good := testRoute(20, 5, "10.20.0.0/16", "65000:6")
	badRT := testRoute(20, 5, "10.30.0.0/16", "65000:7")
	// 来源 VPN 9 本机没有，但 RT 命中 VRF 5 的 import —— 这正是跨站点导通的形状：
	// 准入只看 RT，来源 VPN 决定的是发端压哪个标签（Route.VRFID 原样留着给 fib）。
	foreign := testRoute(20, 9, "10.40.0.0/16", "65000:6")
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{good, badRT, foreign}})

	waitFor(t, "both matching routes imported", func() bool { return len(c.RIB(5)) == 2 })
	if got := ribPrefixes(c.RIB(5)); len(got) != 2 ||
		got[0] != "10.20.0.0/16@192.168.99.2" || got[1] != "10.40.0.0/16@192.168.99.2" {
		t.Fatalf("rib = %v", got)
	}
	for _, e := range c.RIB(5) {
		if e.Prefix == "10.40.0.0/16" && e.VRFID != 9 {
			t.Fatalf("foreign route lost its source VPN: %+v", e)
		}
		if len(e.Targets) != 1 || e.Targets[0] != 5 {
			t.Fatalf("targets = %v, want [5]", e.Targets)
		}
	}
	st := c.Stats()
	if st.DroppedRTMismatch != 1 || st.DroppedInvalid != 0 {
		t.Fatalf("drop counters = %+v", st)
	}

	// A withdraw removes it from the "bgp" table.
	peer.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{good.Key()}})
	waitFor(t, "route withdrawn", func() bool { return len(c.RIB(5)) == 1 })
}

// TableList exists for one reason, and it is this test: encoding/json treats a
// []uint8 as a []byte, so a plain []uint8 target list leaves the process in
// /status as the base64 string "Mg==" instead of 50. Every Go-side assertion in
// this file passed while that was happening -- test-refresh.sh read the real
// endpoint and caught it, so the wire shape is pinned here too.
func TestRibEntryTargetsMarshalAsNumbers(t *testing.T) {
	raw, err := json.Marshal(RIBEntry{
		Route:    Route{DeviceID: 20, VRFID: 9, Prefix: "10.40.0.0/16", RTs: []string{"65000:6"}},
		Targets:  TableList{5, 50},
		NextHops: []string{"192.168.99.2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"targets":[5,50]`) {
		t.Fatalf("targets must be a list of numbers, got %s", raw)
	}
	// And an empty list is still a list: a console must not have to decode null
	// as "empty" the way the tunnel table learned it must not.
	empty, err := json.Marshal(struct {
		Targets TableList `json:"targets"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"targets":[]}` {
		t.Fatalf("empty targets = %s", empty)
	}
}

func TestClientResolvesEveryTunnelToOrigin(t *testing.T) {
	// The prefix arrives without a next hop; the receiver installs every local
	// tunnel to the origin (parallel tunnels are ECMP, not a tie to break).
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 20, NextHop: "192.168.77.1"},
	}
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "client synced", func() bool { return c.Stats().ActiveRR == 0 })

	peer, _ := dial(t, rr, 20)
	peer.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	rt := testRoute(20, 5, "10.20.0.0/16", "65000:6")
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})

	waitFor(t, "route imported", func() bool { return len(c.RIB(5)) == 1 })
	got := c.RIB(5)[0]
	if len(got.NextHops) != 2 || got.NextHops[0] != "192.168.77.1" || got.NextHops[1] != "192.168.99.2" {
		t.Fatalf("next hops = %v, want both tunnels in canonical order", got.NextHops)
	}
	if !got.Route.Equal(rt) {
		t.Fatalf("route = %+v, want %+v", got.Route, rt)
	}
}

func TestClientReimportsOnRTChange(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "client synced", func() bool { return c.Stats().ActiveRR == 0 })

	peer, _ := dial(t, rr, 20)
	peer.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	rt := testRoute(20, 5, "10.20.0.0/16", "65000:7") // not imported
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	waitFor(t, "mismatch counted", func() bool { return c.Stats().DroppedRTMismatch == 1 })
	if n := len(c.RIB(5)); n != 0 {
		t.Fatalf("rib = %d", n)
	}

	// Same key, RT list now intersects the import policy: the replacement must
	// be installed even though the key was already seen.
	rt.RTs = []string{"65000:7", "65000:6"}
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	waitFor(t, "replacement imported", func() bool { return len(c.RIB(5)) == 1 })

	rt.RTs = []string{"65000:7"}
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	waitFor(t, "import revoked by RT change", func() bool { return len(c.RIB(5)) == 0 })
}

func TestActiveStandbyFailoverWithoutMerge(t *testing.T) {
	rrA := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
	rrB := newTestReflector(t, ReflectorConfig{RRID: "rr-b"})
	srvA := newTestRRSrv(t, rrA, 10)
	srvB := newTestRRSrv(t, rrB, 10)

	// Each RR has a different origin, so "which RR am I serving from" is visible
	// in the table itself.
	peerA, _ := dial(t, rrA, 20)
	peerA.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	peerA.send(Envelope{Type: MsgAnnounce, Routes: []Route{
		testRoute(20, 5, "10.20.0.0/16", "65000:6"),
	}})
	peerB, _ := dial(t, rrB, 21)
	peerB.join(21, Tunnel{Peer: 10, NextHop: "192.168.78.10"})
	peerB.send(Envelope{Type: MsgAnnounce, Routes: []Route{
		testRoute(21, 5, "10.21.0.0/16", "65000:6"),
	}})

	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.3"},
	}
	cfg.RRs = []RRConfig{{ID: "rr-a", Addr: "rr-a"}, {ID: "rr-b", Addr: "rr-b"}}
	var mu sync.Mutex
	dialFor := map[string]func(context.Context, string) (net.Conn, error){"rr-a": srvA.dial, "rr-b": srvB.dial}
	cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		mu.Lock()
		d := dialFor[addr]
		mu.Unlock()
		return d(ctx, addr)
	}
	c := startClient(t, cfg)

	waitFor(t, "primary active with route A", func() bool {
		st := c.Stats()
		rib := c.RIB(5)
		return st.ActiveRR == 0 && len(rib) == 1 && rib[0].Prefix == "10.20.0.0/16" &&
			len(rib[0].NextHops) == 1 && rib[0].NextHops[0] == "192.168.99.2"
	})

	srvA.kill()
	waitFor(t, "standby takes over with its own table only", func() bool {
		st := c.Stats()
		rib := c.RIB(5)
		return st.ActiveRR == 1 && len(rib) == 1 && rib[0].Prefix == "10.21.0.0/16" &&
			len(rib[0].NextHops) == 1 && rib[0].NextHops[0] == "192.168.99.3"
	})
	if st := c.Stats(); st.Sessions[0].Connected {
		t.Fatalf("dead RR still reported connected: %+v", st.Sessions[0])
	}

	srvA.revive()
	waitFor(t, "primary comes back and takes over again", func() bool {
		st := c.Stats()
		return st.ActiveRR == 0 && len(c.RIB(5)) == 1 && c.RIB(5)[0].Prefix == "10.20.0.0/16"
	})
}

func TestNoSyncedRRMeansEmptyRIB(t *testing.T) {
	rrA := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
	srvA := newTestRRSrv(t, rrA, 10)
	peerA, _ := dial(t, rrA, 20)
	peerA.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	peerA.send(Envelope{Type: MsgAnnounce, Routes: []Route{
		testRoute(20, 5, "10.20.0.0/16", "65000:6"),
	}})

	// RR B handshakes and sends a route, but withholds EOR: it must contribute
	// nothing, not a partial table.
	eorGate := make(chan struct{})
	serverDone := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-serverDone:
		default:
			close(serverDone)
		}
	})
	dialB := func(ctx context.Context, addr string) (net.Conn, error) {
		srv, cli := net.Pipe()
		go func() {
			defer srv.Close()
			br := bufio.NewReader(srv)
			if _, err := ReadEnvelope(br); err != nil {
				return
			}
			if err := WriteEnvelope(srv, Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "rr-b", HoldSec: 30}); err != nil {
				return
			}
			// Keep reading the client's tunnel table and snapshot: net.Pipe has no
			// kernel buffer, so not reading it would block the client's handshake.
			go func() {
				for {
					if _, err := ReadEnvelope(br); err != nil {
						return
					}
				}
			}()
			if err := WriteEnvelope(srv, Envelope{Type: MsgAnnounce, Routes: []Route{
				testRoute(21, 5, "10.21.0.0/16", "65000:6"),
			}}); err != nil {
				return
			}
			<-eorGate
			if err := WriteEnvelope(srv, Envelope{Type: MsgEOR}); err != nil {
				return
			}
			<-serverDone
		}()
		return cli, nil
	}

	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.3"},
	}
	cfg.RRs = []RRConfig{{ID: "rr-a", Addr: "rr-a"}, {ID: "rr-b", Addr: "rr-b"}}
	cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr == "rr-a" {
			return srvA.dial(ctx, addr)
		}
		return dialB(ctx, addr)
	}
	c := startClient(t, cfg)
	waitFor(t, "a synced", func() bool { return c.Stats().ActiveRR == 0 })
	waitFor(t, "b connected but unsynced", func() bool {
		st := c.Stats()
		return st.Sessions[1].Connected && !st.Sessions[1].Synced
	})

	srvA.kill()
	waitFor(t, "no active RR after the primary dies", func() bool { return c.Stats().ActiveRR == -1 })
	if n := len(c.RIB(5)); n != 0 {
		t.Fatalf("served %d routes from an unsynced RR", n)
	}

	close(eorGate)
	waitFor(t, "standby becomes usable at EOR", func() bool {
		st := c.Stats()
		return st.ActiveRR == 1 && len(c.RIB(5)) == 1 && c.RIB(5)[0].Prefix == "10.21.0.0/16"
	})
}

func TestClientDropsOwnAndMalformedRoutes(t *testing.T) {
	// A scripted RR (not the real reflector, which would reject these at the
	// source) checks the client's own defensive filtering.
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		srv, cli := net.Pipe()
		go func() {
			defer srv.Close()
			br := bufio.NewReader(srv)
			if _, err := ReadEnvelope(br); err != nil {
				return
			}
			if err := WriteEnvelope(srv, Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "script", HoldSec: 30}); err != nil {
				return
			}
			go func() {
				for {
					if _, err := ReadEnvelope(br); err != nil {
						return
					}
				}
			}()
			if err := WriteEnvelope(srv, Envelope{Type: MsgAnnounce, Routes: []Route{
				{DeviceID: 10, VRFID: 5, Prefix: "10.99.0.0/16", RTs: []string{"65000:6"}},
				{DeviceID: 21, VRFID: 5, Prefix: "10.21.2.3/16", RTs: []string{"65000:6"}},
				{DeviceID: 21, VRFID: 5, Prefix: "10.21.0.0/16", RTs: []string{"65000:6"}},
				{DeviceID: 22, VRFID: 5, Prefix: "10.22.0.0/16", RTs: []string{"65000:6"}},
			}}); err != nil {
				return
			}
			if err := WriteEnvelope(srv, Envelope{Type: MsgEOR}); err != nil {
				return
			}
			<-ctx.Done()
		}()
		return cli, nil
	}
	cfg := testClientConfig()
	cfg.Tunnels = []Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.3"},
	}
	cfg.Dial = dial
	c := startClient(t, cfg)

	waitFor(t, "valid route imported", func() bool { return len(c.RIB(5)) == 1 })
	st := c.Stats()
	if st.DroppedOwnRoute != 1 || st.DroppedInvalid != 1 || st.DroppedNoPath != 1 {
		t.Fatalf("counters = %+v", st)
	}
	if st.Received != 2 || st.Imported != 1 {
		t.Fatalf("counters = %+v", st)
	}
	if got := c.RIB(5); len(got) != 1 || got[0].Prefix != "10.21.0.0/16" {
		t.Fatalf("rib = %v", ribPrefixes(got))
	}
}

func TestAllRIBAndEmptyWhenStopped(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.VRFs = append(cfg.VRFs, VRFConfig{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:8"}})
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "client synced", func() bool { return c.Stats().ActiveRR == 0 })

	peer, _ := dial(t, rr, 20)
	peer.join(20, Tunnel{Peer: 10, NextHop: "192.168.77.10"})
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{
		testRoute(20, 5, "10.20.0.0/16", "65000:6"),
		testRoute(20, 6, "10.30.0.0/16", "65000:8"),
		// 来源 VRF 6、RT 65000:6 —— 命中的是 VRF 5 的 import：跨 VPN 导通。
		// 它不再因为"本机有 6"或"本机没有 5"而被丢掉，准入只看 RT。
		testRoute(20, 6, "10.31.0.0/16", "65000:6"),
	}})
	waitFor(t, "both VRFs populated", func() bool {
		all := c.AllRIB()
		return len(all[5]) == 2 && len(all[6]) == 1
	})
	if got := ribPrefixes(c.AllRIB()[6]); got[0] != "10.30.0.0/16@192.168.99.2" {
		t.Fatalf("vrf 6 = %v", got)
	}
	if got := ribPrefixes(c.AllRIB()[5]); len(got) != 2 ||
		got[0] != "10.20.0.0/16@192.168.99.2" || got[1] != "10.31.0.0/16@192.168.99.2" {
		t.Fatalf("vrf 5 = %v", got)
	}
	for _, e := range c.AllRIB()[5] {
		if e.Prefix == "10.31.0.0/16" && e.VRFID != 6 {
			t.Fatalf("cross-VPN route lost its source VPN (the label depends on it): %+v", e)
		}
	}

	srv.kill()
	waitFor(t, "table cleared when no RR is usable", func() bool {
		st := c.Stats()
		return st.ActiveRR == -1 && len(c.AllRIB()) == 0
	})
}

// scriptedRR is a controllable RR: it handshakes, records everything the client
// writes, and lets the test push frames at an exact moment. The real Reflector
// cannot produce the states SetTunnels has to survive -- it never reflects a
// route over an adjacency it does not have, and "we hold a route whose tunnel the
// operator has not configured yet" is precisely the state O5 exists for.
type scriptedRR struct {
	mu     sync.Mutex
	frames []Envelope
	push   chan Envelope
}

func newScriptedRR() *scriptedRR {
	return &scriptedRR{push: make(chan Envelope, 32)}
}

func (s *scriptedRR) dial(ctx context.Context, addr string) (net.Conn, error) {
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		if _, err := ReadEnvelope(br); err != nil { // hello
			return
		}
		if err := WriteEnvelope(srv, Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "script", HoldSec: 30}); err != nil {
			return
		}
		go func() {
			for {
				env, err := ReadEnvelope(br)
				if err != nil {
					return
				}
				s.mu.Lock()
				s.frames = append(s.frames, env)
				s.mu.Unlock()
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-s.push:
				if err := WriteEnvelope(srv, env); err != nil {
					return
				}
			}
		}
	}()
	return cli, nil
}

func (s *scriptedRR) send(env Envelope) {
	s.push <- env
}

// announce hands the client a snapshot: routes then EOR. The client serves from
// one RR only after its EOR, so a script that just sends routes would leave the
// table invisible.
func (s *scriptedRR) announce(routes ...Route) {
	s.send(Envelope{Type: MsgAnnounce, Routes: routes})
	s.send(Envelope{Type: MsgEOR})
}

// tables returns the tunnel tables the client wrote, oldest first. The first one
// is the session's snapshot; more entries mean a live replacement.
func (s *scriptedRR) tables() [][]Tunnel {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]Tunnel
	for _, env := range s.frames {
		if env.Type == MsgTunnels && env.Tunnels != nil {
			out = append(out, *env.Tunnels)
		}
	}
	return out
}

func TestSetTunnelsInstallsRoutesHeldForWantOfAPath(t *testing.T) {
	rr := newScriptedRR()
	cfg := testClientConfig()
	cfg.Dial = rr.dial
	c := startClient(t, cfg)

	rt := testRoute(22, 5, "10.22.0.0/16", "65000:6")
	rr.announce(rt)
	waitFor(t, "route held as pending", func() bool {
		p := c.Pending()
		return len(p) == 1 && p[0].Reason == PendingNoPath && p[0].Prefix == "10.22.0.0/16"
	})
	if n := len(c.RIB(5)); n != 0 {
		t.Fatalf("installed a route with no path: %v", ribPrefixes(c.RIB(5)))
	}
	before := c.Stats()
	if before.DroppedNoPath != 1 || before.ActivePending != 1 {
		t.Fatalf("counters = %+v", before)
	}

	// Add the tunnel. No second announce is needed: the route is already here.
	if err := c.SetTunnels([]Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 22, NextHop: "192.168.100.2"},
	}); err != nil {
		t.Fatalf("SetTunnels: %v", err)
	}
	got := c.RIB(5)
	if len(got) != 1 || got[0].Prefix != "10.22.0.0/16" || len(got[0].NextHops) != 1 ||
		got[0].NextHops[0] != "192.168.100.2" {
		t.Fatalf("rib = %v", ribPrefixes(got))
	}
	if n := len(c.Pending()); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	// Event counters: the recompute must not count the route as newly imported.
	after := c.Stats()
	if after.Imported != before.Imported || after.DroppedNoPath != before.DroppedNoPath {
		t.Fatalf("recompute rewrote the counters: %+v -> %+v", before, after)
	}
	// The RR has to be told: it decides what to reflect to us at all.
	waitFor(t, "the RR to be told the new table", func() bool {
		tb := rr.tables()
		return len(tb) == 2 && len(tb[1]) == 2
	})
}

func TestSetTunnelsRemovesPathAndRouteLeavesTheTable(t *testing.T) {
	rr := newScriptedRR()
	cfg := testClientConfig()
	cfg.Dial = rr.dial
	c := startClient(t, cfg)

	rt := testRoute(20, 5, "10.20.0.0/16", "65000:6")
	rr.announce(rt)
	waitFor(t, "route imported", func() bool { return len(c.RIB(5)) == 1 })
	before := c.Stats()
	if before.Imported != 1 {
		t.Fatalf("counters = %+v", before)
	}

	if err := c.SetTunnels(nil); err != nil {
		t.Fatalf("SetTunnels(nil): %v", err)
	}
	if n := len(c.RIB(5)); n != 0 {
		t.Fatalf("route survived its last tunnel: %v", ribPrefixes(c.RIB(5)))
	}
	// It is not gone: it is held, and the console has to be able to say that.
	p := c.Pending()
	if len(p) != 1 || p[0].Reason != PendingNoPath {
		t.Fatalf("pending = %+v", p)
	}
	after := c.Stats()
	if after.Imported != before.Imported || after.Withdrawn != before.Withdrawn {
		t.Fatalf("recompute rewrote the counters: %+v -> %+v", before, after)
	}
	waitFor(t, "the RR to be told the table is empty", func() bool {
		tb := rr.tables()
		return len(tb) == 2 && len(tb[1]) == 0
	})
}

func TestClientWithNoTunnelsAnnouncesAnEmptyTable(t *testing.T) {
	// A device that has not been given a tunnel yet must still hold a session:
	// its table travels as [], not as a missing field, and the RR waits for it
	// rather than rejecting the frame.
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.Tunnels = nil
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "the RR to record an empty tunnel table", func() bool {
		sn := rr.Sessions()
		return len(sn) == 1 && sn[0].Tunnels == 0
	})
	waitFor(t, "client synced", func() bool { return c.Stats().ActiveRR == 0 })

	// And clearing the last tunnel of a running session reaches the RR the same
	// way instead of dropping the session.
	if err := c.SetTunnels([]Tunnel{{Peer: 20, NextHop: "192.168.99.2"}}); err != nil {
		t.Fatalf("SetTunnels: %v", err)
	}
	waitFor(t, "the RR to see one tunnel", func() bool {
		sn := rr.Sessions()
		return len(sn) == 1 && sn[0].Tunnels == 1
	})
	if err := c.SetTunnels(nil); err != nil {
		t.Fatalf("SetTunnels(nil): %v", err)
	}
	waitFor(t, "the RR to see the table emptied again", func() bool {
		st := c.Stats()
		sn := rr.Sessions()
		return len(sn) == 1 && sn[0].Tunnels == 0 && st.Sessions[0].Connected && st.ActiveRR == 0
	})
}

func TestSetVRFsImportChangeMovesHeldRoutesBothWays(t *testing.T) {
	rr := newScriptedRR()
	cfg := testClientConfig()
	cfg.Dial = rr.dial
	c := startClient(t, cfg)

	rt := testRoute(20, 5, "10.20.0.0/16", "65000:7") // vrf 5 imports 65000:6 only
	rr.announce(rt)
	waitFor(t, "route held for RT", func() bool {
		p := c.Pending()
		return len(p) == 1 && p[0].Reason == PendingRTMismatch
	})
	before := c.Stats()
	if before.DroppedRTMismatch != 1 || before.Imported != 0 {
		t.Fatalf("counters = %+v", before)
	}

	// A VPN added later picks up the prefixes that are already in adj-RIBs-in.
	if err := c.SetVRFs([]VRFConfig{
		{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
		{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:7"}},
	}); err != nil {
		t.Fatalf("SetVRFs: %v", err)
	}
	got := c.RIB(6)
	if len(got) != 1 || got[0].Prefix != "10.20.0.0/16" {
		t.Fatalf("vrf 6 = %v", ribPrefixes(got))
	}
	if n := len(c.RIB(5)); n != 0 {
		t.Fatalf("vrf 5 = %v, must not import 65000:7", ribPrefixes(c.RIB(5)))
	}
	after := c.Stats()
	if after.Imported != before.Imported || after.DroppedRTMismatch != before.DroppedRTMismatch {
		t.Fatalf("recompute rewrote the counters: %+v -> %+v", before, after)
	}

	// And removing the import again takes it out of the table, without a withdraw.
	if err := c.SetVRFs([]VRFConfig{
		{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
	}); err != nil {
		t.Fatalf("SetVRFs back: %v", err)
	}
	if n := len(c.AllRIB()[6]); n != 0 {
		t.Fatalf("vrf 6 still has %d routes after its import RTs were removed", n)
	}
	if p := c.Pending(); len(p) != 1 || p[0].Reason != PendingRTMismatch {
		t.Fatalf("pending = %+v", p)
	}
	if got := c.AllRIB(); len(got) != 0 {
		t.Fatalf("rib = %v", got)
	}
}

func TestSetVRFsRestampsAndWithdrawsLocalRoutes(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.VRFs = []VRFConfig{
		{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:6"}},
		{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:9"}},
	}
	cfg.LocalPrefixes = []LocalPrefix{
		{VRFID: 5, Prefix: "10.10.0.0/16"},
		{VRFID: 6, Prefix: "10.20.0.0/16"},
	}
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "the RR holds both prefixes", func() bool { return len(rr.RoutesOf(10)) == 2 })

	// Drop vrf 6 and change vrf 5's export RT in one step. The prefix on the
	// removed VRF must leave the RR -- a route announced with the RTs of a VRF
	// nobody configured would be imported by a policy that no longer exists.
	if err := c.SetVRFs([]VRFConfig{
		{ID: 5, ExportRTs: []string{"65000:51"}, ImportRTs: []string{"65000:6"}},
	}); err != nil {
		t.Fatalf("SetVRFs: %v", err)
	}
	waitFor(t, "the RR to hold one re-stamped prefix", func() bool {
		rs := rr.RoutesOf(10)
		return len(rs) == 1 && rs[0].Prefix == "10.10.0.0/16" &&
			len(rs[0].RTs) == 1 && rs[0].RTs[0] == "65000:51"
	})
	if lp := c.LocalSkips(); len(lp) != 1 || lp[0].VRFID != 6 ||
		lp[0].Prefix != "10.20.0.0/16" || lp[0].Reason != SkipVRFUnconfigured {
		t.Fatalf("local skips = %+v", lp)
	}

	// Put the VRF back: the prefix was kept as an input, so it comes back on its
	// own -- no restart, no re-push of the local config.
	if err := c.SetVRFs([]VRFConfig{
		{ID: 5, ExportRTs: []string{"65000:51"}, ImportRTs: []string{"65000:6"}},
		{ID: 6, ExportRTs: []string{"65000:6"}, ImportRTs: []string{"65000:9"}},
	}); err != nil {
		t.Fatalf("SetVRFs again: %v", err)
	}
	waitFor(t, "the RR holds both prefixes again", func() bool { return len(rr.RoutesOf(10)) == 2 })
	if n := len(c.LocalSkips()); n != 0 {
		t.Fatalf("local skips = %+v", c.LocalSkips())
	}
}

func TestRejectedChangeLeavesTheTablesAlone(t *testing.T) {
	cfg := testClientConfig()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"tunnel to this device", c.SetTunnels([]Tunnel{{Peer: 10, NextHop: "192.168.99.9"}}), "this device"},
		{"bad next hop", c.SetTunnels([]Tunnel{{Peer: 20, NextHop: "192.168.099.2"}}), "bad next_hop"},
		{"duplicate tunnel", c.SetTunnels([]Tunnel{{Peer: 20, NextHop: "192.168.99.2"}, {Peer: 20, NextHop: "192.168.99.2"}}), "ascending"},
		{"bad RT", c.SetVRFs([]VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"1:"}}}), "bad RT"},
		{"duplicate vrf", c.SetVRFs([]VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}}, {ID: 5, ExportRTs: []string{"65000:6"}}}), "twice"},
	}
	for _, tc := range cases {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want containing %q", tc.name, tc.err, tc.want)
		}
	}
	if got := c.Tunnels(); len(got) != 1 || got[0] != (Tunnel{Peer: 20, NextHop: "192.168.99.2"}) {
		t.Fatalf("tunnels after rejected sets = %+v", got)
	}
	if n := len(c.LocalRoutes()); n != 1 {
		t.Fatalf("local routes after rejected sets = %d", n)
	}
	if n := len(c.Pending()); n != 0 {
		t.Fatalf("pending = %d", n)
	}
}

func TestTableChangeWhileNoSessionIsAnnouncedOnReconnect(t *testing.T) {
	rr := newTestReflector(t, ReflectorConfig{})
	srv := newTestRRSrv(t, rr, 10)
	cfg := testClientConfig()
	cfg.Dial = srv.dial
	c := startClient(t, cfg)
	waitFor(t, "session synced", func() bool { return c.Stats().ActiveRR == 0 })

	srv.kill()
	waitFor(t, "session down", func() bool { return !c.Stats().Sessions[0].Connected })

	// Both changes land while nothing is connected. They still have to reach the
	// RR once it comes back -- the session's own snapshot is the only copy.
	if err := c.SetTunnels([]Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.3"},
	}); err != nil {
		t.Fatalf("SetTunnels: %v", err)
	}
	if err := c.SetLocalPrefixes([]LocalPrefix{{VRFID: 5, Prefix: "10.50.0.0/16"}}); err != nil {
		t.Fatalf("SetLocalPrefixes: %v", err)
	}
	srv.revive()
	waitForWithin(t, 20*time.Second, "the new session to carry both changes", func() bool {
		sn := rr.Sessions()
		return len(sn) == 1 && sn[0].Tunnels == 2 && len(rr.RoutesOf(10)) == 2
	})
}
