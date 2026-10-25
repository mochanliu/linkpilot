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

// ReflectorConfig is the RR's static input.
//
// The tunnel graph is not configured here: every CPE announces its own table,
// and the RR derives adjacency from those tables (mutual entries only). That is
// what keeps a thousand-tunnel hub from being reflected as a thousand next hops
// per prefix -- the RR sends each peer only the prefixes it can actually reach.
//
// ReflectTo is an optional per-origin filter on top of the derived adjacency.
// It can only remove targets, never add one: a device with no mutual tunnel has
// no next hop to install, so a policy cannot create reachability.
type ReflectorConfig struct {
	RRID      string
	Tenant    string
	HoldSec   int
	ReflectTo map[uint16][]uint16
}

// Reflector is one RR instance serving one tenant.
//
// It keeps two tables: the tunnel adjacency each CPE announced, and the
// adj-rib-in of routes keyed by origin. Route flow: a CPE announces its table,
// the RR computes who is adjacent to whom, and each route is fanned out to the
// origin's adjacent peers. It never carries or rewrites a next hop, never
// computes a best path and never applies RTs: the path belongs to the receiver's
// tunnel table, and RT import is the receiving CPE's decision.
type Reflector struct {
	rrID   string
	tenant string
	hold   time.Duration
	policy map[uint16]map[uint16]bool // origin -> allowed targets (nil = no filter)

	mu       sync.Mutex
	sessions map[uint16]*serverSession
	adjIn    map[uint16]map[RouteKey]Route
	tunnels  map[uint16][]Tunnel // last table announced by each device
}

// SessionInfo is the admin/tests view of one live session.
type SessionInfo struct {
	DeviceID uint16    `json:"device_id"`
	Since    time.Time `json:"since"`
	Synced   bool      `json:"synced"`  // peer sent EOR after its first snapshot
	Routes   int       `json:"routes"`  // adj-rib-in size for this origin
	Tunnels  int       `json:"tunnels"` // entries in the table the peer last announced
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
	policy, err := buildPolicy(cfg.ReflectTo)
	if err != nil {
		return nil, err
	}
	return &Reflector{
		rrID:     cfg.RRID,
		tenant:   cfg.Tenant,
		hold:     hold,
		policy:   policy,
		sessions: make(map[uint16]*serverSession),
		adjIn:    make(map[uint16]map[RouteKey]Route),
		tunnels:  make(map[uint16][]Tunnel),
	}, nil
}

// TargetsOf reports the effective reflection targets of one origin right now:
// peers with a mutual tunnel, minus anything ReflectTo excludes.
func (r *Reflector) TargetsOf(dev uint16) []uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.targetsLocked(dev)
}

// Tunnels returns a copy of every announced tunnel table.
func (r *Reflector) Tunnels() map[uint16][]Tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint16][]Tunnel, len(r.tunnels))
	for dev, ts := range r.tunnels {
		out[dev] = append([]Tunnel(nil), ts...)
	}
	return out
}

func buildPolicy(policy map[uint16][]uint16) (map[uint16]map[uint16]bool, error) {
	if len(policy) == 0 {
		return nil, nil
	}
	out := make(map[uint16]map[uint16]bool, len(policy))
	for a, peers := range policy {
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
		out[a] = set
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
		// The old tunnel table goes too: the new session re-asserts it before it
		// can be a reflection target again.
		r.clearRoutesLocked(old.dev)
		delete(r.sessions, old.dev)
		delete(r.tunnels, old.dev)
		old.shutdown()
	}
	r.sessions[s.dev] = s

	hello := Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: r.rrID, HoldSec: int(r.hold / time.Second)}
	if !s.trySend(hello) {
		delete(r.sessions, s.dev)
		s.shutdown()
		return fmt.Errorf("reflector: device %d: cannot accept hello_ok", s.dev)
	}
	// The snapshot waits for the peer's tunnel table (applyTunnels): until then
	// the RR does not know what this device can reach, and sending an empty table
	// followed by every route as an update is worse than a short wait.
	return nil
}

// announceEnvelopes builds the chunks of one route snapshot plus EOR. EOR must
// come after the last chunk or the peer starts serving a partial table.
func announceEnvelopes(routes []Route) []Envelope {
	var envs []Envelope
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

// snapshotForLocked returns every route whose origin can send to dev. The
// device's own routes are excluded: BGP speakers do not install their own
// reflected paths, and reflecting them back would only invite loops.
func (r *Reflector) snapshotForLocked(dev uint16) []Route {
	var out []Route
	for _, origin := range r.sendersLocked(dev) {
		for _, rt := range r.adjIn[origin] {
			out = append(out, rt)
		}
	}
	return SortedRoutes(out)
}

// peersLocked is the sorted, duplicate-free peer set of one device's announced
// table. Parallel tunnels to the same peer collapse to one entry here; the
// receiver still gets one path per tunnel from its own table.
func (r *Reflector) peersLocked(dev uint16) []uint16 {
	ts := r.tunnels[dev]
	out := make([]uint16, 0, len(ts))
	for i, t := range ts {
		if i > 0 && ts[i-1].Peer == t.Peer {
			continue
		}
		out = append(out, t.Peer)
	}
	return out
}

// mutualPeersLocked keeps only peers that also announce dev. An adjacency seen
// from one side only has no return path, so it is never a reflection target.
func (r *Reflector) mutualPeersLocked(dev uint16) []uint16 {
	var out []uint16
	for _, p := range r.peersLocked(dev) {
		if containsDevice(r.peersLocked(p), dev) {
			out = append(out, p)
		}
	}
	return out
}

// targetsLocked is who may receive dev's routes: mutual adjacency minus policy.
func (r *Reflector) targetsLocked(dev uint16) []uint16 {
	mutual := r.mutualPeersLocked(dev)
	pol := r.policy[dev]
	if pol == nil {
		return mutual
	}
	out := make([]uint16, 0, len(mutual))
	for _, p := range mutual {
		if pol[p] {
			out = append(out, p)
		}
	}
	return out
}

// sendersLocked is whose routes may reach dev: the same adjacency, filtered by
// each origin's own policy.
func (r *Reflector) sendersLocked(dev uint16) []uint16 {
	var out []uint16
	for _, p := range r.mutualPeersLocked(dev) {
		if pol := r.policy[p]; pol != nil && !pol[dev] {
			continue
		}
		out = append(out, p)
	}
	return out
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
		case MsgTunnels:
			if env.Tunnels == nil {
				return fmt.Errorf("reflector: device %d: tunnels message without a table", s.dev)
			}
			if err := r.applyTunnels(s, *env.Tunnels); err != nil {
				return err
			}
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

// applyTunnels replaces one device's tunnel table and re-evaluates the edges
// that changed. The table is the device's whole adjacency opinion, so a
// replacement is a diff against the previous one: a gained edge starts
// receiving routes in both directions, a lost edge stops. Routes are re-sent
// from adj-rib-in instead of being remembered per target, so there is no second
// copy of the table to drift out of sync.
func (r *Reflector) applyTunnels(s *serverSession, table []Tunnel) error {
	if err := ValidateTunnels(table); err != nil {
		return fmt.Errorf("reflector: device %d: %w", s.dev, err)
	}
	for _, t := range table {
		if t.Peer == s.dev {
			return fmt.Errorf("reflector: device %d: tunnel to itself", s.dev)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.dev] != s {
		return fmt.Errorf("reflector: device %d: session was replaced", s.dev)
	}

	beforeOut := r.targetsLocked(s.dev) // peers that receive my routes
	beforeIn := r.sendersLocked(s.dev)  // origins whose routes reach me
	r.tunnels[s.dev] = append([]Tunnel(nil), table...)
	afterOut := r.targetsLocked(s.dev)
	afterIn := r.sendersLocked(s.dev)

	first := !s.tunnelsSet
	s.tunnelsSet = true
	if first {
		for _, env := range announceEnvelopes(r.snapshotForLocked(s.dev)) {
			if !s.trySend(env) {
				s.shutdown()
				return fmt.Errorf("reflector: device %d: session too slow to accept initial snapshot", s.dev)
			}
		}
	}

	gainedOut, lostOut := diffDevices(beforeOut, afterOut)
	gainedIn, lostIn := diffDevices(beforeIn, afterIn)
	for _, p := range gainedOut {
		r.sendRoutesLocked(p, r.routesOfLocked(s.dev))
	}
	for _, p := range lostOut {
		r.withdrawKeysLocked(p, r.keysOfLocked(s.dev))
	}
	for _, p := range gainedIn {
		if first {
			continue // the initial snapshot already carried them
		}
		r.sendRoutesLocked(s.dev, r.routesOfLocked(p))
	}
	for _, p := range lostIn {
		r.withdrawKeysLocked(s.dev, r.keysOfLocked(p))
	}
	return nil
}

// clearRoutesLocked drops one origin's adj-rib-in and tells its targets. Called
// on disconnect and on reconnect, so peers do not keep serving paths whose
// origin stopped asserting them.
func (r *Reflector) clearRoutesLocked(dev uint16) {
	if len(r.adjIn[dev]) == 0 {
		delete(r.adjIn, dev)
		return
	}
	keys := r.keysOfLocked(dev)
	delete(r.adjIn, dev)
	r.fanoutKeysLocked(dev, keys)
}

func (r *Reflector) routesOfLocked(dev uint16) []Route {
	m := r.adjIn[dev]
	if len(m) == 0 {
		return nil
	}
	out := make([]Route, 0, len(m))
	for _, rt := range m {
		out = append(out, rt)
	}
	return SortedRoutes(out)
}

func (r *Reflector) keysOfLocked(dev uint16) []RouteKey {
	m := r.adjIn[dev]
	keys := make([]RouteKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return SortedKeys(keys)
}

func (r *Reflector) fanoutRoutesLocked(origin uint16, routes []Route) {
	for _, t := range r.targetsLocked(origin) {
		r.sendRoutesLocked(t, routes)
	}
}

func (r *Reflector) fanoutKeysLocked(origin uint16, keys []RouteKey) {
	for _, t := range r.targetsLocked(origin) {
		r.withdrawKeysLocked(t, keys)
	}
}

func (r *Reflector) sendRoutesLocked(to uint16, routes []Route) {
	if len(routes) == 0 {
		return
	}
	s := r.sessions[to]
	if s == nil {
		return
	}
	for len(routes) > 0 {
		n := len(routes)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		if !s.trySend(Envelope{Type: MsgAnnounce, Routes: routes[:n]}) {
			s.shutdown()
			return
		}
		routes = routes[n:]
	}
}

func (r *Reflector) withdrawKeysLocked(from uint16, keys []RouteKey) {
	if len(keys) == 0 {
		return
	}
	s := r.sessions[from]
	if s == nil {
		return
	}
	for len(keys) > 0 {
		n := len(keys)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		if !s.trySend(Envelope{Type: MsgWithdraw, Keys: keys[:n]}) {
			s.shutdown()
			return
		}
		keys = keys[n:]
	}
}

// diffDevices reports which members of after are new and which members of
// before disappeared. Both lists are sorted, so gained/lost keep that order.
func diffDevices(before, after []uint16) (gained, lost []uint16) {
	for _, d := range after {
		if !containsDevice(before, d) {
			gained = append(gained, d)
		}
	}
	for _, d := range before {
		if !containsDevice(after, d) {
			lost = append(lost, d)
		}
	}
	return gained, lost
}

func (r *Reflector) detach(s *serverSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.dev] == s {
		delete(r.sessions, s.dev)
		r.clearRoutesLocked(s.dev)
		delete(r.tunnels, s.dev)
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
			Tunnels:  len(r.tunnels[dev]),
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

	synced     bool // guarded by Reflector.mu: peer finished its initial snapshot
	tunnelsSet bool // guarded by Reflector.mu: the initial tunnel table arrived
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
