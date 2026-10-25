package route

import (
	"bufio"
	"context"
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
	deadline := time.Now().Add(5 * time.Second)
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
	badVRF := testRoute(20, 9, "10.40.0.0/16", "65000:6")
	peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{good, badRT, badVRF}})

	waitFor(t, "matching route imported", func() bool { return len(c.RIB(5)) == 1 })
	if got := ribPrefixes(c.RIB(5)); len(got) != 1 || got[0] != "10.20.0.0/16@192.168.99.2" {
		t.Fatalf("rib = %v", got)
	}
	st := c.Stats()
	if st.DroppedRTMismatch != 1 || st.DroppedNoVRF != 1 || st.DroppedInvalid != 0 {
		t.Fatalf("drop counters = %+v", st)
	}

	// A withdraw removes it from the "bgp" table.
	peer.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{good.Key()}})
	waitFor(t, "route withdrawn", func() bool { return len(c.RIB(5)) == 0 })
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
		testRoute(20, 6, "10.31.0.0/16", "65000:6"), // RT does not match VRF 6
	}})
	waitFor(t, "both VRFs populated", func() bool {
		all := c.AllRIB()
		return len(all[5]) == 1 && len(all[6]) == 1
	})
	if got := ribPrefixes(c.AllRIB()[6]); got[0] != "10.30.0.0/16@192.168.99.2" {
		t.Fatalf("vrf 6 = %v", got)
	}

	srv.kill()
	waitFor(t, "table cleared when no RR is usable", func() bool {
		st := c.Stats()
		return st.ActiveRR == -1 && len(c.AllRIB()) == 0
	})
}
