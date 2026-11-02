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
	id   PeerIdentity
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

// dial starts a server-side session for a client device and returns the client
// end plus the error channel of ServeConn. The TCP/TLS layer is replaced by
// net.Pipe: the certificate-derived identity is passed explicitly, exactly as
// the server would after its TLS handshake.
func dial(t *testing.T, r *Reflector, dev uint16) (*fakeCPE, chan error) {
	t.Helper()
	return dialAs(t, r, PeerIdentity{Tenant: r.tenant, Role: RoleClient, DeviceID: dev})
}

// dialRR starts a session that speaks as another reflector.
func dialRR(t *testing.T, r *Reflector, rrID string) (*fakeCPE, chan error) {
	t.Helper()
	return dialAs(t, r, PeerIdentity{Tenant: r.tenant, Role: RoleRR, RRID: rrID})
}

func dialAs(t *testing.T, r *Reflector, id PeerIdentity) (*fakeCPE, chan error) {
	t.Helper()
	srv, cli := net.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- r.ServeConn(srv, id) }()
	c := &fakeCPE{t: t, id: id, conn: cli, br: bufio.NewReader(cli)}
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

// join completes the handshake and returns the routes of the initial snapshot.
// The snapshot follows hello_ok immediately (BGP's initial UPDATE burst + EOR),
// so this is the whole of what a fresh session receives without announcing
// anything first. A client hello claims its device id; a reflector hello claims
// none, because a reflector is named by its certificate.
func (c *fakeCPE) join() []Route {
	c.t.Helper()
	c.send(Envelope{Type: MsgHello, Version: ProtocolVersion, DeviceID: c.id.DeviceID})
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

func testRoute(dev, vrf uint16, prefix string, rts ...string) Route {
	if len(rts) == 0 {
		rts = []string{"65000:100"}
	}
	return Route{DeviceID: dev, VRFID: uint8(vrf), Prefix: prefix, RTs: rts}
}

func TestNewReflectorValidation(t *testing.T) {
	if _, err := NewReflector(ReflectorConfig{Tenant: "t"}); err == nil {
		t.Fatal("missing rr_id accepted")
	}
	if _, err := NewReflector(ReflectorConfig{RRID: "rr"}); err == nil {
		t.Fatal("missing tenant accepted")
	}
	if _, err := NewReflector(ReflectorConfig{RRID: "rr", Tenant: "t", HoldSec: 2}); err == nil {
		t.Fatal("hold 2s accepted")
	}
}

func TestSnapshotOnConnectAndFanout(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})

	c2, _ := dial(t, r, 2)
	if snap := c2.join(); len(snap) != 0 {
		t.Fatalf("first session's snapshot = %+v, want empty", snap)
	}
	rt2 := testRoute(2, 1, "10.2.0.0/16", "65000:200")
	c2.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2}})
	c2.send(Envelope{Type: MsgEOR})

	// A late joiner must receive the existing route in its snapshot, before EOR.
	c1, _ := dial(t, r, 1)
	snap := c1.join()
	if len(snap) != 1 || !snap[0].Equal(rt2) {
		t.Fatalf("snapshot = %+v, want [%+v]", snap, rt2)
	}

	// Device 1's own route reaches 2 and never comes back to 1.
	rt1 := testRoute(1, 1, "10.1.0.0/16")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt1}})
	if got := c2.recv(); got.Type != MsgAnnounce || len(got.Routes) != 1 || !got.Routes[0].Equal(rt1) {
		t.Fatalf("device 2 got %+v, want announce of %+v", got, rt1)
	}
	c1.expectNone(150 * time.Millisecond)

	// No adjacency gate: a new session gets every route in the RR, except its
	// own. Whether it can use one is its own tunnel table's decision.
	c3, _ := dial(t, r, 3)
	snap3 := c3.join()
	if len(snap3) != 2 || !snap3[0].Equal(rt1) || !snap3[1].Equal(rt2) {
		t.Fatalf("device 3 snapshot = %+v, want device 1's and device 2's routes", snap3)
	}

	// A route from 2 reaches every other session, not just one neighbour.
	rt2b := testRoute(2, 1, "10.22.0.0/16")
	c2.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2b}})
	for _, c := range []*fakeCPE{c1, c3} {
		if got := c.recv(); got.Type != MsgAnnounce || got.Routes[0].Prefix != "10.22.0.0/16" {
			t.Fatalf("got %+v, want the 10.22.0.0/16 announce", got)
		}
	}
}

func TestWithdrawFanoutAndIdempotence(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	c2, _ := dial(t, r, 2)
	c2.join()
	c3, _ := dial(t, r, 3)
	c3.join()

	rt := testRoute(1, 1, "10.1.0.0/16")
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
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	c2, _ := dial(t, r, 2)
	c2.join()

	rt := testRoute(1, 1, "10.1.0.0/16", "65000:100")
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

func TestReconnectReplacesSessionAndWithdraws(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	c2, _ := dial(t, r, 2)
	c2.join()

	rt := testRoute(1, 1, "10.1.0.0/16")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c2.recv(); got.Type != MsgAnnounce {
		t.Fatalf("want announce, got %s", got.Type)
	}

	c1b, _ := dial(t, r, 1)
	c1b.join() // duplicate session: new wins

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
	r := newTestReflector(t, ReflectorConfig{})
	c1, errCh1 := dial(t, r, 1)
	c1.join()
	c2, _ := dial(t, r, 2)
	c2.join()

	rt := testRoute(1, 1, "10.1.0.0/16")
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

func TestSnapshotFollowsHelloOK(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c2, _ := dial(t, r, 2)
	c2.join()
	rt2 := testRoute(2, 1, "10.2.0.0/16")
	c2.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2}})
	c2.send(Envelope{Type: MsgEOR})

	// Nothing is announced by the new session first: the RR sends its table the
	// moment the session is up, which is where a BGP speaker sends its initial
	// UPDATE burst.
	c1, _ := dial(t, r, 1)
	if snap := c1.join(); len(snap) != 1 || !snap[0].Equal(rt2) {
		t.Fatalf("device 1 snapshot = %+v, want [%+v]", snap, rt2)
	}

	// An empty RR still ends the burst with EOR, so the peer knows it has the
	// whole table and not a prefix of it.
	r2 := newTestReflector(t, ReflectorConfig{})
	c7, _ := dial(t, r2, 7)
	if snap := c7.join(); len(snap) != 0 {
		t.Fatalf("snapshot from an empty RR = %+v, want none", snap)
	}
}

func TestTunnelsFrameIsRejected(t *testing.T) {
	// The tunnel table left the wire when the RR stopped deriving adjacency from
	// it. A peer still speaking the old protocol must be refused loudly: silently
	// ignoring the frame is how the two sides disagree about the table forever.
	r := newTestReflector(t, ReflectorConfig{})
	c1, errCh := dial(t, r, 1)
	c1.join()
	c1.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c1.conn.Write([]byte(`{"type":"tunnels","tunnels":[]}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := waitServe(t, errCh); err == nil || !strings.Contains(err.Error(), "unknown message type") {
		t.Fatalf("ServeConn error = %v, want a retired-message-type error", err)
	}
}

func TestAnnounceForAnotherDeviceIsFatal(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, errCh := dial(t, r, 1)
	c1.join()

	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(2, 1, "10.2.0.0/16")}})
	err := waitServe(t, errCh)
	if err == nil || !strings.Contains(err.Error(), "only speak for itself") {
		t.Fatalf("ServeConn error = %v", err)
	}
	if n := len(r.RoutesOf(1)) + len(r.RoutesOf(2)); n != 0 {
		t.Fatalf("%d routes stored from a bad announce", n)
	}
}

func TestMalformedRouteIsFatal(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, errCh := dial(t, r, 1)
	c1.join()

	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(1, 1, "10.1.2.3/16")}})
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
		go func() { errCh <- r.ServeConn(srv, PeerIdentity{Tenant: "other", Role: RoleClient, DeviceID: 1}) }()
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
	r := newTestReflector(t, ReflectorConfig{HoldSec: 3})
	c1, _ := dial(t, r, 1)
	c1.join()
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
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(1, 1, "10.1.0.0/16")}})
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
	r := newTestReflector(t, ReflectorConfig{})
	c2, _ := dial(t, r, 2)
	c2.join()

	var wg sync.WaitGroup
	clients := map[uint16]*fakeCPE{}
	for _, dev := range []uint16{1, 3, 4} {
		c, _ := dial(t, r, dev)
		c.join()
		clients[dev] = c
	}

	for dev, c := range clients {
		dev, c := dev, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				rt := testRoute(dev, 1, "10."+itoa(int(dev))+"."+itoa(i)+".0/24")
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

// TestReflectorRelaysRoutesFromAReflector covers the second half of the
// fan-out rule: what an rr told us goes to clients only. A reflector is not a
// place a route is re-reflected through -- that is how a mesh becomes a loop.
func TestReflectorRelaysRoutesFromAReflector(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	rrB, _ := dialRR(t, r, "rr-b")
	rrB.join()

	// Device 9 has no session here; rr-b is relaying its route, which is
	// exactly what a reflector session is for.
	rt := testRoute(9, 1, "10.9.0.0/16")
	rrB.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c1.recv(); got.Type != MsgAnnounce || len(got.Routes) != 1 || !got.Routes[0].Equal(rt) {
		t.Fatalf("client got %+v, want the reflected route", got)
	}

	rrC, _ := dialRR(t, r, "rr-c")
	if snap := rrC.join(); len(snap) != 0 {
		t.Fatalf("second reflector's snapshot = %+v, want none (a reflector hears only clients)", snap)
	}

	sn := r.Sessions()
	if len(sn) != 3 || sn[0].Key != "node/1" || sn[0].Role != RoleClient ||
		sn[1].Key != "rr/rr-b" || sn[1].Role != RoleRR || sn[1].DeviceID != 0 {
		t.Fatalf("sessions = %+v", sn)
	}
	if n := len(r.RoutesOf(9)); n != 1 {
		t.Fatalf("RoutesOf(9) = %d routes, want 1", n)
	}
}

// TestSnapshotForAReflectorCarriesOnlyClientRoutes: what a newly attached
// reflector receives is the client-sourced part of the table, not everything.
func TestSnapshotForAReflectorCarriesOnlyClientRoutes(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1)
	c1.join()
	rt1 := testRoute(1, 1, "10.1.0.0/16")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt1}})
	c1.send(Envelope{Type: MsgEOR})

	rrB, _ := dialRR(t, r, "rr-b")
	if snap := rrB.join(); len(snap) != 1 || !snap[0].Equal(rt1) {
		t.Fatalf("reflector snapshot = %+v, want [%+v]", snap, rt1)
	}

	// rr-b relays device 8's route; a third reflector must not see it.
	rt8 := testRoute(8, 1, "10.8.0.0/16")
	rrB.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt8}})
	if got := c1.recv(); got.Type != MsgAnnounce || got.Routes[0].DeviceID != 8 {
		t.Fatalf("client got %+v, want device 8's announce", got)
	}
	rrC, _ := dialRR(t, r, "rr-c")
	snap := rrC.join()
	if len(snap) != 1 || snap[0].DeviceID != 1 {
		t.Fatalf("third reflector snapshot = %+v, want only device 1's route", snap)
	}
}

// TestWithdrawKeepsARouteAnotherSessionStillHolds: one session letting go must
// not erase a route another session still vouches for.
func TestWithdrawKeepsARouteAnotherSessionStillHolds(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{})
	c1, _ := dial(t, r, 1) // the origin
	c1.join()
	c2, _ := dial(t, r, 2) // a plain receiver
	c2.join()
	rrB, _ := dialRR(t, r, "rr-b")
	rrB.join()

	rt := testRoute(1, 1, "10.1.0.0/16")
	c1.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	for _, c := range []*fakeCPE{c2, rrB} {
		if got := c.recv(); got.Type != MsgAnnounce {
			t.Fatalf("want announce, got %s", got.Type)
		}
	}
	// rr-b says the same key back: two tables now hold it.
	rrB.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt}})
	if got := c2.recv(); got.Type != MsgAnnounce {
		t.Fatalf("want the duplicate announce, got %s", got.Type)
	}
	if got := c1.recv(); got.Type != MsgAnnounce || got.Routes[0].DeviceID != 1 {
		// A client hears its own prefix back through a reflector; dropping it is
		// the device's own job (Client.applyAnnounce counts own_route).
		t.Fatalf("origin got %+v, want its own route echoed by the reflector", got)
	}

	// The origin withdraws. c2 keeps the route: rr-b still holds it. rr-b does
	// hear a withdraw for its own copy -- per-peer, no other session vouches.
	c1.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{rt.Key()}})
	c2.expectNone(150 * time.Millisecond)
	if got := rrB.recv(); got.Type != MsgWithdraw || len(got.Keys) != 1 || got.Keys[0] != rt.Key() {
		t.Fatalf("rr-b got %+v, want the withdraw of its own copy", got)
	}

	// Now the last holder lets go, and c2 is told.
	rrB.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{rt.Key()}})
	if got := c2.recv(); got.Type != MsgWithdraw || len(got.Keys) != 1 || got.Keys[0] != rt.Key() {
		t.Fatalf("want withdraw of %+v, got %+v", rt.Key(), got)
	}
}

// TestReflectorSessionRules pins the handshake rules that keep a reflector
// session from being read as a device session and vice versa.
func TestReflectorSessionRules(t *testing.T) {
	t.Run("rr hello may not claim a device", func(t *testing.T) {
		r := newTestReflector(t, ReflectorConfig{})
		c, errCh := dialRR(t, r, "rr-b")
		c.send(Envelope{Type: MsgHello, Version: ProtocolVersion, DeviceID: 4})
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "speaks for no device") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("an rr session claiming a device was accepted")
		}
	})

	t.Run("a reflector does not peer with itself", func(t *testing.T) {
		r := newTestReflector(t, ReflectorConfig{})
		c, errCh := dialRR(t, r, "rr-a")
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "itself") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("self-peering accepted")
		}
	})

	t.Run("an identity with no role is refused", func(t *testing.T) {
		r := newTestReflector(t, ReflectorConfig{})
		srv, cli := net.Pipe()
		defer cli.Close()
		errCh := make(chan error, 1)
		go func() { errCh <- r.ServeConn(srv, PeerIdentity{Tenant: r.tenant, DeviceID: 1}) }()
		c := &fakeCPE{t: t, conn: cli, br: bufio.NewReader(cli)}
		if env := c.recv(); env.Type != MsgError || !strings.Contains(env.Message, "role") {
			t.Fatalf("got %+v", env)
		}
		if err := waitServe(t, errCh); err == nil {
			t.Fatal("a session with no role was accepted")
		}
	})
}

// fakeRR is the accepting half of a net.Pipe in the outbound tests: it reads
// what DialConn put on the wire and answers like a reflector would.
type fakeRR struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func (f *fakeRR) send(env Envelope) {
	f.t.Helper()
	f.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WriteEnvelope(f.conn, env); err != nil {
		f.t.Fatalf("fake rr write %s: %v", env.Type, err)
	}
}

func (f *fakeRR) recvErr() (Envelope, error) {
	f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return ReadEnvelope(f.br)
}

func (f *fakeRR) recv() Envelope {
	f.t.Helper()
	env, err := f.recvErr()
	if err != nil {
		f.t.Fatalf("fake rr read: %v", err)
	}
	return env
}

// TestDialConnSpeaksFirstWithoutADeviceID pins the one thing that differs
// between the two directions: the outbound side sends hello, and an rr hello
// carries no device id.
func TestDialConnSpeaksFirstWithoutADeviceID(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
	srv, cli := net.Pipe()
	defer cli.Close()
	errCh := make(chan error, 1)
	peer := PeerIdentity{Tenant: "tenant-a", Role: RoleRR, RRID: "rr-b"}
	go func() { errCh <- r.DialConn(cli, peer) }()

	f := &fakeRR{t: t, conn: srv, br: bufio.NewReader(srv)}
	env := f.recv()
	if env.Type != MsgHello {
		t.Fatalf("first frame = %s, want hello", env.Type)
	}
	if env.DeviceID != 0 {
		t.Fatalf("hello claims device %d; an rr session speaks for no device", env.DeviceID)
	}
	if env.Version != ProtocolVersion {
		t.Fatalf("hello version = %d, want %d", env.Version, ProtocolVersion)
	}

	f.send(Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "rr-b", HoldSec: int(DefaultHoldSec)})
	if env := f.recv(); env.Type != MsgEOR {
		t.Fatalf("after hello_ok got %s, want the empty snapshot's EOR", env.Type)
	}
	waitFor(t, "outbound session attached", func() bool { return len(r.Sessions()) == 1 })
	if sn := r.Sessions(); sn[0].Key != "rr/rr-b" || sn[0].Role != RoleRR {
		t.Fatalf("sessions = %+v", sn)
	}

	srv.Close()
	if err := waitServe(t, errCh); err == nil {
		t.Fatal("DialConn returned nil after the peer closed")
	}
}

// TestDialConnHonoursThePeerHold: the session's deadline and keepalive cadence
// come from the peer's hello_ok, not from our own hold_sec (the accepting side
// made the same choice in ServeConn).
func TestDialConnHonoursThePeerHold(t *testing.T) {
	r := newTestReflector(t, ReflectorConfig{RRID: "rr-a", HoldSec: 300})
	srv, cli := net.Pipe()
	defer cli.Close()
	errCh := make(chan error, 1)
	peer := PeerIdentity{Tenant: "tenant-a", Role: RoleRR, RRID: "rr-b"}
	go func() { errCh <- r.DialConn(cli, peer) }()

	f := &fakeRR{t: t, conn: srv, br: bufio.NewReader(srv)}
	f.recv()
	f.send(Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "rr-b", HoldSec: 3})
	if env := f.recv(); env.Type != MsgEOR {
		t.Fatalf("want EOR, got %s", env.Type)
	}
	// Our own hold is 300s, so a keepalive here can only come from the peer's 3s.
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if env, err := ReadEnvelope(f.br); err != nil || env.Type != MsgKeepalive {
		t.Fatalf("keepalive within 2s = %+v, %v (peer hold was ignored)", env, err)
	}
	srv.Close()
	waitServe(t, errCh)
}

// TestDialConnCarriesRoutesAcrossTheMesh runs two reflectors over net.Pipe and
// checks the same snapshot/fanout/withdraw path an inbound session uses: each
// client sees the other side's route, and a withdrawal follows.
func TestDialConnCarriesRoutesAcrossTheMesh(t *testing.T) {
	a := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
	b := newTestReflector(t, ReflectorConfig{RRID: "rr-b"})

	srv, cli := net.Pipe()
	defer cli.Close()
	aErr := make(chan error, 1)
	bErr := make(chan error, 1)
	idA := PeerIdentity{Tenant: a.tenant, Role: RoleRR, RRID: "rr-a"}
	idB := PeerIdentity{Tenant: b.tenant, Role: RoleRR, RRID: "rr-b"}
	go func() { bErr <- b.ServeConn(srv, idA) }()
	go func() { aErr <- a.DialConn(cli, idB) }()
	waitFor(t, "mesh session attached on both sides", func() bool { return len(a.Sessions()) == 1 && len(b.Sessions()) == 1 })

	ca, _ := dial(t, a, 1)
	ca.join()
	cb, _ := dial(t, b, 2)
	cb.join()

	rt1 := testRoute(1, 1, "10.1.0.0/16")
	ca.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt1}})
	if got := cb.recv(); got.Type != MsgAnnounce || len(got.Routes) != 1 || !got.Routes[0].Equal(rt1) {
		t.Fatalf("device 2 got %+v, want device 1's route through the mesh", got)
	}

	rt2 := testRoute(2, 1, "10.2.0.0/16")
	cb.send(Envelope{Type: MsgAnnounce, Routes: []Route{rt2}})
	if got := ca.recv(); got.Type != MsgAnnounce || len(got.Routes) != 1 || !got.Routes[0].Equal(rt2) {
		t.Fatalf("device 1 got %+v, want device 2's route through the mesh", got)
	}

	ca.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{rt1.Key()}})
	if got := cb.recv(); got.Type != MsgWithdraw || len(got.Keys) != 1 || got.Keys[0] != rt1.Key() {
		t.Fatalf("device 2 got %+v, want the withdraw", got)
	}

	cli.Close() // the mesh session dies; both sides drop the other's routes
	waitServe(t, aErr)
	waitServe(t, bErr)
	if n := len(b.RoutesOf(1)); n != 0 {
		t.Fatalf("b still holds %d routes for device 1 after the mesh died", n)
	}
}

// TestDialConnRejections: a peer that is not a reflector for this tenant, or
// that is ourselves, is refused before any session state exists.
func TestDialConnRejections(t *testing.T) {
	cases := []struct {
		name string
		id   PeerIdentity
		want string
	}{
		{"node identity", PeerIdentity{Tenant: "tenant-a", Role: RoleClient, DeviceID: 3}, "not a reflector"},
		{"cross tenant", PeerIdentity{Tenant: "tenant-b", Role: RoleRR, RRID: "rr-b"}, "tenant"},
		{"itself", PeerIdentity{Tenant: "tenant-a", Role: RoleRR, RRID: "rr-a"}, "itself"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
			srv, cli := net.Pipe()
			defer cli.Close()
			err := r.DialConn(srv, tc.id)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DialConn(%+v) = %v, want error containing %q", tc.id, err, tc.want)
			}
			cli.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := cli.Read(make([]byte, 1)); err == nil {
				t.Fatal("rejected connection was left open")
			}
			if n := len(r.Sessions()); n != 0 {
				t.Fatalf("%d sessions attached after a rejection", n)
			}
		})
	}
}
