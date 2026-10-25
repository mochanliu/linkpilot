package route

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCPE is the client half of the protocol, used directly against ServeConn
// over net.Pipe. It is deliberately dumb: it does no filtering and no
// reconnection, so a test can assert exactly what the RR put on the wire.
type fakeCPE struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func newTestReflector(t *testing.T, cfg ReflectorConfig) *Reflector {
	t.Helper()
	if cfg.RRID == "" {
		cfg.RRID = "rr-a"
	}
	if cfg.Tenant == "" {
		cfg.Tenant = "tenant-a"
	}
	r, err := NewReflector(cfg)
	if err != nil {
		t.Fatalf("NewReflector: %v", err)
	}
	return r
}

// dial starts a server-side session and returns the client end plus the error
// channel of ServeConn. The TCP/TLS layer is replaced by net.Pipe: the
// certificate-derived identity is passed explicitly, exactly as the server
// would after its TLS handshake.
func dial(t *testing.T, r *Reflector, dev uint16) (*fakeCPE, chan error) {
	t.Helper()
	srv, cli := net.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- r.ServeConn(srv, PeerIdentity{Tenant: r.tenant, DeviceID: dev}) }()
	c := &fakeCPE{t: t, conn: cli, br: bufio.NewReader(cli)}
	t.Cleanup(func() { cli.Close() })
	return c, errCh
}

func (c *fakeCPE) send(env Envelope) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WriteEnvelope(c.conn, env); err != nil {
		c.t.Fatalf("client write %s: %v", env.Type, err)
	}
}

func (c *fakeCPE) recv() Envelope {
	c.t.Helper()
	env, err := c.recvErr()
	if err != nil {
		c.t.Fatalf("client read: %v", err)
	}
	return env
}

func (c *fakeCPE) recvErr() (Envelope, error) {
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return ReadEnvelope(c.br)
}

// join completes the handshake and returns the routes received in the initial
// snapshot. EOR is consumed but not returned.
func (c *fakeCPE) join(dev uint16) []Route {
	c.t.Helper()
	c.send(Envelope{Type: MsgHello, Version: ProtocolVersion, DeviceID: dev})
	ok := c.recv()
	if ok.Type != MsgHelloOK {
		c.t.Fatalf("handshake: got %s, want hello_ok (%+v)", ok.Type, ok)
	}
	var routes []Route
	for {
		env := c.recv()
		switch env.Type {
		case MsgAnnounce:
			routes = append(routes, env.Routes...)
		case MsgEOR:
			return routes
		default:
			c.t.Fatalf("unexpected %s during snapshot", env.Type)
		}
	}
}

func (c *fakeCPE) expectNone(d time.Duration) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	env, err := ReadEnvelope(c.br)
	if err == nil {
		c.t.Fatalf("expected silence, got %s %+v", env.Type, env)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		c.t.Fatalf("expected read timeout, got %v", err)
	}
}

func (c *fakeCPE) expectClosed() {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		env, err := ReadEnvelope(c.br)
		if err != nil {
			return
		}
		// A keepalive or a reflected update may already be in flight; keep
		// reading until the close shows up.
		_ = env
	}
}

func waitServe(t *testing.T, errCh chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return")
		return nil
	}
}

func testRoute(dev, vrf uint16, prefix, nextHop string, rts ...string) Route {
	if len(rts) == 0 {
		rts = []string{"65000:100"}
	}
	return Route{DeviceID: dev, VRFID: uint8(vrf), Prefix: prefix, NextHops: []string{nextHop}, RTs: rts}
}

func TestNewReflectorValidation(t *testing.T) {
	base := func() ReflectorConfig {
		return ReflectorConfig{RRID: "rr", Tenant: "t", Topology: map[uint16][]uint16{1: {2}}}
	}
	if _, err := NewReflector(ReflectorConfig{Tenant: "t"}); err == nil {
		t.Fatal("missing rr_id accepted")
	}
	if _, err := NewReflector(ReflectorConfig{RRID: "rr"}); err == nil {
		t.Fatal("missing tenant accepted")
	}
	badHold := base()
	badHold.HoldSec = 2
	if _, err := NewReflector(badHold); err == nil {
		t.Fatal("hold 2s accepted")
	}
	zero := base()
	zero.Topology = map[uint16][]uint16{0: {1}}
	if _, err := NewReflector(zero); err == nil {
		t.Fatal("device 0 in topology accepted")
	}
	self := base()
	self.Topology = map[uint16][]uint16{1: {1}}
	if _, err := NewReflector(self); err == nil {
		t.Fatal("self link accepted")
	}
	badPolicy := base()
	badPolicy.ReflectTo = map[uint16][]uint16{1: {1}}
	if _, err := NewReflector(badPolicy); err == nil {
		t.Fatal("reflect_to self target accepted")
	}
}

func TestTargetsSymmetrizedAndOverridden(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{
		Topology:  map[uint16][]uint16{1: {2, 3}}, // tunnels listed from one end only
		ReflectTo: map[uint16][]uint16{1: {9}},    // policy replaces topology for 1
	})
	if got := r.TargetsOf(1); len(got) != 1 || got[0] != 9 {
		t.Fatalf("targets of 1 = %v, want [9] (reflect_to overrides)", got)
	}
	if got := r.TargetsOf(2); len(got) != 1 || got[0] != 1 {
		t.Fatalf("targets of 2 = %v, want [1] (symmetric from 1's list)", got)
	}
	if got := r.TargetsOf(3); len(got) != 1 || got[0] != 1 {
		t.Fatalf("targets of 3 = %v, want [1]", got)
	}
	if got := r.TargetsOf(4); len(got) != 0 {
		t.Fatalf("targets of unknown device = %v, want none", got)
	}
}

func TestSnapshotOnConnectAndFanout(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2, 3}}})

	c2, _ := dial(t, r, 2)
	c2.join(2)
	rt2 := testRoute(2, 1, "10.2.0.0/16", "192.168.99.2", "65000:200")
	c2.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2}})
	c2.send(Envelope{Type: MsgEOR})

	// A late joiner must receive the existing route in its snapshot, before EOR.
	c1, _ := dial(t, r, 1)
	snap := c1.join(1)
	if len(snap) != 1 || !snap[0].Equal(rt2) {
		t.Fatalf("snapshot = %+v, want [%+v]", snap, rt2)
	}

	// Device 1's own route reaches 2 and 3 but not back to 1.
	rt1 := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt1}})
	if got := c2.recv(); got.Type != MsgAnnounce || len(got.Routes) != 1 || !got.Routes[0].Equal(rt1) {
		t.Fatalf("device 2 got %+v, want announce of %+v", got, rt1)
	}

	c3, _ := dial(t, r, 3)
	snap3 := c3.join(3)
	// 3 tunnels only to 1, so device 2's route (targets=[1]) must NOT be here.
	if len(snap3) != 1 || !snap3[0].Equal(rt1) {
		t.Fatalf("device 3 snapshot = %+v, want only device 1's route", snap3)
	}

	// A route from 2 reflects only to its tunnel peer 1.
	rt2b := testRoute(2, 1, "10.22.0.0/16", "192.168.99.2")
	c2.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2b}})
	if got := c1.recv(); got.Type != MsgAnnounce || got.Routes[0].Prefix != "10.22.0.0/16" {
		t.Fatalf("device 1 got %+v", got)
	}
	c3.expectNone(150 * time.Millisecond) // 3 is not a target of 2
}

func TestWithdrawFanoutAndIdempotence(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2, 3}}})
	c1, _ := dial(t, r, 1)
	c1.join(1)
	c2, _ := dial(t, r, 2)
	c2.join(2)
	c3, _ := dial(t, r, 3)
	c3.join(3)

	rt := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	for _, c := range []*fakeCPE{c2, c3} {
		if got := c.recv(); got.Type != MsgAnnounce {
			t.Fatalf("want announce, got %s", got.Type)
		}
	}

	c1.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{rt.Key()}})
	for _, c := range []*fakeCPE{c2, c3} {
		got := c.recv()
		if got.Type != MsgWithdraw || len(got.Keys) != 1 || got.Keys[0] != rt.Key() {
			t.Fatalf("want withdraw of %+v, got %+v", rt.Key(), got)
		}
	}
	if n := len(r.RoutesOf(1)); n != 0 {
		t.Fatalf("adj-rib-in still has %d routes", n)
	}

	// Withdrawing something that is not there is absorbed, not fanned out.
	c1.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{rt.Key()}})
	c2.expectNone(150 * time.Millisecond)
}

func TestDuplicateAnnounceIsNoOpUnlessChanged(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, _ := dial(t, r, 1)
	c1.join(1)
	c2, _ := dial(t, r, 2)
	c2.join(2)

	rt := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1", "65000:100")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c2.recv(); got.Type != MsgAnnounce {
		t.Fatalf("want announce, got %s", got.Type)
	}
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	c2.expectNone(150 * time.Millisecond)

	// Same key, different RTs: a replacement, and the peer must see it.
	changed := rt
	changed.RTs = []string{"65000:100", "65000:300"}
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{changed}})
	got := c2.recv()
	if got.Type != MsgAnnounce || len(got.Routes[0].RTs) != 2 {
		t.Fatalf("RT change not reflected: %+v", got)
	}
}

func TestReflectToPolicyReplacesTopology(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{
		Topology:  map[uint16][]uint16{1: {2, 3}},
		ReflectTo: map[uint16][]uint16{1: {3}},
	})
	c2, _ := dial(t, r, 2)
	c2.join(2)
	c3, _ := dial(t, r, 3)
	c3.join(3)
	c1, _ := dial(t, r, 1)
	c1.join(1)

	rt := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c3.recv(); got.Type != MsgAnnounce || !got.Routes[0].Equal(rt) {
		t.Fatalf("policy target 3 got %+v", got)
	}
	c2.expectNone(150 * time.Millisecond)

	// The snapshot follows the same policy for a device that joins later.
	c4, _ := dial(t, r, 2)
	if snap := c4.join(2); len(snap) != 0 {
		t.Fatalf("device 2 snapshot = %+v, want none under reflect_to policy", snap)
	}
}

func TestReconnectReplacesSessionAndWithdraws(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, _ := dial(t, r, 1)
	c1.join(1)
	c2, _ := dial(t, r, 2)
	c2.join(2)

	rt := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c2.recv(); got.Type != MsgAnnounce {
		t.Fatalf("want announce, got %s", got.Type)
	}

	c1b, _ := dial(t, r, 1)
	c1b.join(1) // duplicate session: new wins

	got := c2.recv()
	if got.Type != MsgWithdraw || len(got.Keys) != 1 || got.Keys[0] != rt.Key() {
		t.Fatalf("old session's routes not withdrawn: %+v", got)
	}
	if sn := r.Sessions(); len(sn) != 2 || sn[0].DeviceID != 1 || sn[1].DeviceID != 2 {
		t.Fatalf("sessions = %+v", sn)
	}
	if n := len(r.RoutesOf(1)); n != 0 {
		t.Fatalf("reconnect kept %d stale routes", n)
	}
	c1.expectClosed() // the replaced connection is gone
}

func TestDisconnectWithdrawsRoutes(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, errCh1 := dial(t, r, 1)
	c1.join(1)
	c2, _ := dial(t, r, 2)
	c2.join(2)

	rt := testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c2.recv(); got.Type != MsgAnnounce {
		t.Fatalf("want announce, got %s", got.Type)
	}

	c1.conn.Close()
	if got := c2.recv(); got.Type != MsgWithdraw {
		t.Fatalf("disconnect did not withdraw: %+v", got)
	}
	if err := waitServe(t, errCh1); err == nil {
		t.Fatal("ServeConn returned nil after the client closed")
	}
	if n := len(r.Sessions()); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestAnnounceForAnotherDeviceIsFatal(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, errCh := dial(t, r, 1)
	c1.join(1)

	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(2, 1, "10.2.0.0/16", "192.168.99.2")}})
	err := waitServe(t, errCh)
	if err == nil || !strings.Contains(err.Error(), "only speak for itself") {
		t.Fatalf("ServeConn error = %v", err)
	}
	if n := len(r.RoutesOf(1)) + len(r.RoutesOf(2)); n != 0 {
		t.Fatalf("%d routes stored from a bad announce", n)
	}
}

func TestMalformedRouteIsFatal(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, errCh := dial(t, r, 1)
	c1.join(1)

	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(1, 1, "10.1.2.3/16", "192.168.99.1")}})
	if err := waitServe(t, errCh); err == nil {
		t.Fatal("non-canonical prefix accepted and session kept")
	}
}

func TestHandshakeRejections(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})

	t.Run("wrong tenant", func(t *testing.T) {
		srv, cli := net.Pipe()
		defer cli.Close()
		errCh := make(chan error, 1)
		go func() { errCh <- r.ServeConn(srv, PeerIdentity{Tenant: "other", DeviceID: 1}) }()
		c := &fakeCPE{t: t, conn: cli, br: bufio.NewReader(cli)}
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "tenant") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("wrong tenant accepted")
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		c, errCh := dial(t, r, 1)
		c.send(Envelope{Type: MsgHello, Version: ProtocolVersion + 1, DeviceID: 1})
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "version") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("version mismatch accepted")
		}
	})

	t.Run("device id disagrees with certificate", func(t *testing.T) {
		c, errCh := dial(t, r, 1)
		c.send(Envelope{Type: MsgHello, Version: ProtocolVersion, DeviceID: 9})
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "certificate") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("spoofed device id accepted")
		}
	})

	t.Run("first message is not hello", func(t *testing.T) {
		c, errCh := dial(t, r, 1)
		c.send(Envelope{Type: MsgKeepalive})
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "hello") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("keepalive-before-hello accepted")
		}
	})
}

func TestKeepaliveIsSent(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{HoldSec: 3, Topology: map[uint16][]uint16{1: {2}}})
	c1, _ := dial(t, r, 1)
	c1.join(1)
	c1.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	env, err := ReadEnvelope(c1.br)
	if err != nil {
		t.Fatalf("no keepalive within one hold/3 period: %v", err)
	}
	if env.Type != MsgKeepalive {
		t.Fatalf("got %s, want keepalive", env.Type)
	}
}

func TestSessionInfoTracksSyncAndRoutes(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}}})
	c1, _ := dial(t, r, 1)
	c1.join(1)
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(1, 1, "10.1.0.0/16", "192.168.99.1")}})
	c1.send(Envelope{Type: MsgEOR})
	c1.send(Envelope{Type: MsgKeepalive}) // round-trip: server applied everything before this

	var sn []SessionInfo
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sn = r.Sessions()
		if len(sn) == 1 && sn[0].Synced && sn[0].Routes == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(sn) != 1 || !sn[0].Synced || sn[0].Routes != 1 {
		t.Fatalf("session info = %+v", sn)
	}
}

// concurrent fanout: several origins announcing while a target is attached must
// never leave the reflector's map half-updated or panic under the race detector.
func TestConcurrentAnnounces(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{Topology: map[uint16][]uint16{1: {2}, 3: {2}, 4: {2}}})
	c2, _ := dial(t, r, 2)
	c2.join(2)

	var wg sync.WaitGroup
	clients := map[uint16]*fakeCPE{}
	for _, dev := range []uint16{1, 3, 4} {
		c, _ := dial(t, r, dev)
		c.join(dev)
		clients[dev] = c
	}
	stop := make(chan struct{})
	close(stop)

	for dev, c := range clients {
		dev, c := dev, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				rt := testRoute(dev, 1, "10."+itoa(int(dev))+"."+itoa(i)+".0/24", "192.168.99."+itoa(int(dev)))
				c.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
			}
			c.send(Envelope{Type: MsgEOR})
		}()
	}
	// Drain c2 so the reflector's writers never block.
	drained := make(chan int, 1)
	go func() {
		n := 0
		c2.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			env, err := ReadEnvelope(c2.br)
			if err != nil {
				drained <- n
				return
			}
			n += len(env.Routes)
			if n >= 60 {
				drained <- n
				return
			}
		}
	}()
	wg.Wait()
	select {
	case n := <-drained:
		if n < 60 {
			t.Fatalf("device 2 received %d reflected routes, want 60", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining device 2")
	}
}
