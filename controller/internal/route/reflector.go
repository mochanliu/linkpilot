package route

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// PeerIdentity is who a session speaks for. It is read from the peer's mTLS
// certificate by the caller (keybinding.IdentityOf), never from the hello: the
// hello's device_id is only a claim, and the check is that it agrees with this.
type PeerIdentity struct {
	Tenant   string
	DeviceID uint16
}

// ReflectorConfig is the RR's static input for v1.
//
// Topology is the VPN tunnel graph (undirected): an entry 1:[2,3] means device 1
// has tunnels to 2 and 3. NewReflector symmetrizes it, so listing only one
// direction is enough. ReflectTo is a per-origin policy that REPLACES the
// topology-derived target list for that origin -- that is the "配置策略指定
// 设备ID" escape hatch, not an addition, so an operator can stop reflecting
// routes from a device to its tunnels without changing the tunnel graph.
//
// Both maps are read-only after construction. A topology change is a restart
// today; sessions re-snapshot on reconnect, so no route can go stale across it.
type ReflectorConfig struct {
	RRID      string
	Tenant    string
	HoldSec   int
	Topology  map[uint16][]uint16
	ReflectTo map[uint16][]uint16
}

// Reflector is one RR instance serving one tenant.
//
// Route flow: a CPE session announces its own routes (adj-rib-in, keyed by
// origin), the reflector fans them out to that origin's targets. It never
// rewrites a next hop, never computes a best path and never applies RTs: RT
// import is the receiving CPE's decision, and the RR storing RTs verbatim is
// what lets both ends agree after a failover.
type Reflector struct {
	rrID    string
	tenant  string
	hold    time.Duration
	targets map[uint16][]uint16 // origin -> sorted targets, origin excluded

	mu       sync.Mutex
	sessions map[uint16]*serverSession
	adjIn    map[uint16]map[RouteKey]Route
}

// SessionInfo is the admin/tests view of one live session.
type SessionInfo struct {
	DeviceID uint16    `json:"device_id"`
	Since    time.Time `json:"since"`
	Synced   bool      `json:"synced"` // peer sent EOR after its first snapshot
	Routes   int       `json:"routes"` // adj-rib-in size for this origin
}

func NewReflector(cfg ReflectorConfig) (*Reflector, error) {
	if cfg.RRID == "" {
		return nil, errors.New("reflector: rr_id is required (it is sent in hello_ok so logs on both ends can name the session)")
	}
	if cfg.Tenant == "" {
		return nil, errors.New("reflector: tenant is required")
	}
	hold, err := HoldDuration(cfg.HoldSec)
	if err != nil {
		return nil, err
	}
	targets, err := buildTargets(cfg.Topology, cfg.ReflectTo)
	if err != nil {
		return nil, err
	}
	return &Reflector{
		rrID:     cfg.RRID,
		tenant:   cfg.Tenant,
		hold:     hold,
		targets:  targets,
		sessions: make(map[uint16]*serverSession),
		adjIn:    make(map[uint16]map[RouteKey]Route),
	}, nil
}

// TargetsOf reports the effective reflection targets of one origin, after
// symmetrizing topologies and applying ReflectTo overrides.
func (r *Reflector) TargetsOf(dev uint16) []uint16 {
	return append([]uint16(nil), r.targets[dev]...)
}

func buildTargets(topology, override map[uint16][]uint16) (map[uint16][]uint16, error) {
	adj := make(map[uint16]map[uint16]bool)
	add := func(a, b uint16) {
		if adj[a] == nil {
			adj[a] = make(map[uint16]bool)
		}
		adj[a][b] = true
	}
	for a, peers := range topology {
		if a == 0 {
			return nil, errors.New("reflector: topology has device 0 (reserved)")
		}
		for _, b := range peers {
			switch {
			case b == 0:
				return nil, fmt.Errorf("reflector: topology device %d links to device 0", a)
			case b == a:
				return nil, fmt.Errorf("reflector: topology device %d links to itself", a)
			}
			add(a, b)
			add(b, a) // an operator may list a tunnel from one end only
		}
	}

	out := make(map[uint16][]uint16, len(adj))
	for a, set := range adj {
		list := make([]uint16, 0, len(set))
		for b := range set {
			list = append(list, b)
		}
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		out[a] = list
	}

	for a, peers := range override {
		if a == 0 {
			return nil, errors.New("reflector: reflect_to has device 0 (reserved)")
		}
		set := make(map[uint16]bool, len(peers))
		for _, b := range peers {
			if b == 0 {
				return nil, fmt.Errorf("reflector: reflect_to[%d] targets device 0", a)
			}
			if b == a {
				return nil, fmt.Errorf("reflector: reflect_to[%d] targets itself", a)
			}
			set[b] = true
		}
		list := make([]uint16, 0, len(set))
		for b := range set {
			list = append(list, b)
		}
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		out[a] = list
	}
	return out, nil
}

// ServeConn runs one CPE session until it dies. The caller has already
// completed the TLS handshake and must derive id from the verified client
// certificate.
//
// Blocking; run one goroutine per accepted connection. Returning an error is
// normal on disconnect -- the caller decides whether to log it.
func (r *Reflector) ServeConn(conn net.Conn, id PeerIdentity) error {
	if id.Tenant != r.tenant {
		return reject(conn, fmt.Sprintf("tenant %q is not served by this reflector", id.Tenant))
	}
	if id.DeviceID == 0 {
		return reject(conn, "device id 0 is reserved")
	}

	conn.SetDeadline(time.Now().Add(r.hold))
	br := bufio.NewReaderSize(conn, 64<<10)
	env, err := ReadEnvelope(br)
	if err != nil {
		conn.Close()
		return fmt.Errorf("reflector: device %d: read hello: %w", id.DeviceID, err)
	}
	if env.Type != MsgHello {
		return reject(conn, fmt.Sprintf("expected hello, got %q", env.Type))
	}
	if env.Version != ProtocolVersion {
		return reject(conn, fmt.Sprintf("protocol version %d unsupported (want %d)", env.Version, ProtocolVersion))
	}
	if env.DeviceID != id.DeviceID {
		return reject(conn, fmt.Sprintf("hello claims device %d but the certificate is for %d", env.DeviceID, id.DeviceID))
	}
	conn.SetDeadline(time.Time{})

	s := &serverSession{
		dev:   id.DeviceID,
		conn:  conn,
		out:   make(chan Envelope, 1024),
		done:  make(chan struct{}),
		since: time.Now(),
	}
	go r.writeLoop(s)
	if err := r.attach(s); err != nil {
		s.shutdown()
		return err
	}
	go r.keepaliveLoop(s, r.hold/3)
	defer r.detach(s)

	return r.readLoop(s, br)
}

func reject(conn net.Conn, msg string) error {
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = WriteEnvelope(conn, Envelope{Type: MsgError, Message: msg})
	conn.Close()
	return errors.New("reflector: " + msg)
}

func (r *Reflector) attach(s *serverSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if old := r.sessions[s.dev]; old != nil {
		// Newest session wins (a CPE reconnecting after its network came back
		// must not keep talking to a half-dead socket). Its routes go with it:
		// the new session sends a fresh full snapshot, and keeping the old paths
		// in the meantime would advertise reachability that nobody re-asserted.
		r.clearRoutesLocked(old.dev)
		delete(r.sessions, old.dev)
		old.shutdown()
	}
	r.sessions[s.dev] = s

	hello := Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: r.rrID, HoldSec: int(r.hold / time.Second)}
	snap := r.snapshotForLocked(s.dev)
	for _, env := range snapshotEnvelopes(hello, snap) {
		if !s.trySend(env) {
			// A peer that cannot take a 1024-deep burst while we hold the lock is
			// not going to keep up with updates either. Kill it; reconnect gives
			// it a clean snapshot rather than a table with holes.
			delete(r.sessions, s.dev)
			s.shutdown()
			return fmt.Errorf("reflector: device %d: session too slow to accept initial snapshot", s.dev)
		}
	}
	return nil
}

// snapshotEnvelopes builds hello_ok, the chunks of the initial table, and EOR.
// Order is the protocol: nothing below may arrive before hello_ok, and EOR must
// come after the last chunk or the peer starts serving a partial table.
func snapshotEnvelopes(hello Envelope, routes []Route) []Envelope {
	envs := make([]Envelope, 0, 2+len(routes)/MaxRoutesPerMessage)
	envs = append(envs, hello)
	for len(routes) > 0 {
		n := len(routes)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		envs = append(envs, Envelope{Type: MsgAnnounce, Routes: routes[:n]})
		routes = routes[n:]
	}
	return append(envs, Envelope{Type: MsgEOR})
}

// snapshotForLocked returns every path whose origin has dev as a target. The
// device's own routes are excluded: BGP speakers do not install their own
// reflected paths, and reflecting them back would only invite loops.
func (r *Reflector) snapshotForLocked(dev uint16) []Route {
	var out []Route
	for origin, routes := range r.adjIn {
		if origin == dev || !containsDevice(r.targets[origin], dev) {
			continue
		}
		for _, rt := range routes {
			out = append(out, rt)
		}
	}
	return SortedRoutes(out)
}

func (r *Reflector) readLoop(s *serverSession, br *bufio.Reader) error {
	for {
		s.conn.SetReadDeadline(time.Now().Add(r.hold))
		env, err := ReadEnvelope(br)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return fmt.Errorf("reflector: device %d: hold timer expired", s.dev)
			}
			return fmt.Errorf("reflector: device %d: %w", s.dev, err)
		}
		switch env.Type {
		case MsgKeepalive:
			// Nothing to do: the deadline was just pushed out by the read.
		case MsgEOR:
			r.mu.Lock()
			if cur := r.sessions[s.dev]; cur == s {
				s.synced = true
			}
			r.mu.Unlock()
		case MsgAnnounce:
			if err := r.applyAnnounce(s, env.Routes); err != nil {
				return err
			}
		case MsgWithdraw:
			if err := r.applyWithdraw(s, env.Keys); err != nil {
				return err
			}
		case MsgError:
			return fmt.Errorf("reflector: device %d: peer error: %s", s.dev, env.Message)
		default:
			return fmt.Errorf("reflector: device %d: unexpected %q after handshake", s.dev, env.Type)
		}
	}
}

func (r *Reflector) applyAnnounce(s *serverSession, routes []Route) error {
	for _, rt := range routes {
		if rt.DeviceID != s.dev {
			return fmt.Errorf("reflector: device %d: announced route for device %d (a node may only speak for itself)",
				s.dev, rt.DeviceID)
		}
		if err := rt.Validate(); err != nil {
			return fmt.Errorf("reflector: device %d: %w", s.dev, err)
		}
	}
	if len(routes) == 0 {
		return nil
	}

	// Within one frame the last copy of a key wins; announcing the same key
	// twice is a sender bug we absorb rather than reflect onwards twice.
	last := make(map[RouteKey]Route, len(routes))
	order := make([]RouteKey, 0, len(routes))
	for _, rt := range routes {
		k := rt.Key()
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = rt
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.dev] != s {
		return fmt.Errorf("reflector: device %d: session was replaced", s.dev)
	}
	m := r.adjIn[s.dev]
	if m == nil {
		m = make(map[RouteKey]Route)
		r.adjIn[s.dev] = m
	}
	changed := make([]Route, 0, len(order))
	for _, k := range order {
		rt := last[k]
		if old, ok := m[k]; ok && old.Equal(rt) {
			continue // identical re-announcement: not an update
		}
		m[k] = rt
		changed = append(changed, rt)
	}
	r.fanoutRoutesLocked(s.dev, changed)
	return nil
}

func (r *Reflector) applyWithdraw(s *serverSession, keys []RouteKey) error {
	for _, k := range keys {
		if k.DeviceID != s.dev {
			return fmt.Errorf("reflector: device %d: withdrawal for device %d", s.dev, k.DeviceID)
		}
		if err := k.Validate(); err != nil {
			return fmt.Errorf("reflector: device %d: %w", s.dev, err)
		}
	}
	if len(keys) == 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.dev] != s {
		return fmt.Errorf("reflector: device %d: session was replaced", s.dev)
	}
	m := r.adjIn[s.dev]
	var removed []RouteKey
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			continue // already gone (e.g. cleared by reconnect): idempotent
		}
		delete(m, k)
		removed = append(removed, k)
	}
	r.fanoutKeysLocked(s.dev, SortedKeys(removed))
	return nil
}

// clearRoutesLocked drops one origin's adj-rib-in and tells its targets. Called
// on disconnect and on reconnect, so peers do not keep serving paths whose
// origin stopped asserting them.
func (r *Reflector) clearRoutesLocked(dev uint16) {
	m := r.adjIn[dev]
	if len(m) == 0 {
		delete(r.adjIn, dev)
		return
	}
	keys := make([]RouteKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	delete(r.adjIn, dev)
	r.fanoutKeysLocked(dev, SortedKeys(keys))
}

func (r *Reflector) fanoutRoutesLocked(origin uint16, routes []Route) {
	if len(routes) == 0 {
		return
	}
	for _, t := range r.targets[origin] {
		if t == origin {
			continue
		}
		s := r.sessions[t]
		if s == nil {
			continue
		}
		if !s.trySend(Envelope{Type: MsgAnnounce, Routes: routes}) {
			s.shutdown()
		}
	}
}

func (r *Reflector) fanoutKeysLocked(origin uint16, keys []RouteKey) {
	if len(keys) == 0 {
		return
	}
	for _, t := range r.targets[origin] {
		if t == origin {
			continue
		}
		s := r.sessions[t]
		if s == nil {
			continue
		}
		if !s.trySend(Envelope{Type: MsgWithdraw, Keys: keys}) {
			s.shutdown()
		}
	}
}

func (r *Reflector) detach(s *serverSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.dev] == s {
		delete(r.sessions, s.dev)
		r.clearRoutesLocked(s.dev)
	}
	s.shutdown()
}

func (r *Reflector) writeLoop(s *serverSession) {
	for {
		select {
		case env := <-s.out:
			s.conn.SetWriteDeadline(time.Now().Add(r.hold))
			if err := WriteEnvelope(s.conn, env); err != nil {
				s.shutdown()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (r *Reflector) keepaliveLoop(s *serverSession, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !s.trySend(Envelope{Type: MsgKeepalive}) {
				s.shutdown()
				return
			}
		case <-s.done:
			return
		}
	}
}

// Routes returns a copy of the whole adj-rib-in, sorted, keyed by origin.
func (r *Reflector) Routes() map[uint16][]Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint16][]Route, len(r.adjIn))
	for dev, m := range r.adjIn {
		list := make([]Route, 0, len(m))
		for _, rt := range m {
			list = append(list, rt)
		}
		out[dev] = SortedRoutes(list)
	}
	return out
}

// RoutesOf returns one origin's adj-rib-in, sorted.
func (r *Reflector) RoutesOf(dev uint16) []Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.adjIn[dev]
	list := make([]Route, 0, len(m))
	for _, rt := range m {
		list = append(list, rt)
	}
	return SortedRoutes(list)
}

// Sessions returns the live sessions, sorted by device id.
func (r *Reflector) Sessions() []SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionInfo, 0, len(r.sessions))
	for dev, s := range r.sessions {
		out = append(out, SessionInfo{
			DeviceID: dev,
			Since:    s.since,
			Synced:   s.synced,
			Routes:   len(r.adjIn[dev]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

type serverSession struct {
	dev   uint16
	conn  net.Conn
	out   chan Envelope
	done  chan struct{}
	once  sync.Once
	since time.Time

	synced bool // guarded by Reflector.mu: peer finished its initial snapshot
}

func (s *serverSession) trySend(env Envelope) bool {
	select {
	case s.out <- env:
		return true
	case <-s.done:
		return false
	default:
		// Buffer full: the writer is stuck behind a full TCP window. Closing is
		// the only honest option; a silent drop would leave the peer's table
		// missing an update with no way for it to notice.
		return false
	}
}

func (s *serverSession) shutdown() {
	s.once.Do(func() {
		close(s.done)
		s.conn.Close()
	})
}

func containsDevice(list []uint16, dev uint16) bool {
	for _, d := range list {
		if d == dev {
			return true
		}
	}
	return false
}
