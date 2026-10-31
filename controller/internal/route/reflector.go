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
// There is nothing here about reachability, and that is the point: an RR only
// reflects. It never looks at tunnels, next hops, RTs or best paths -- who can
// actually use a reflected prefix is the receiving CPE's decision, made against
// its own tunnel table.
type ReflectorConfig struct {
	RRID    string
	Tenant  string
	HoldSec int
}

// Reflector is one RR instance serving one tenant.
//
// It keeps one table: the adj-rib-in of routes keyed by origin. A route is
// advertised to every session except the one it came from (GoBGP's
// filterPathFromSourcePeer), unchanged -- no next hop is added or rewritten, no
// RT is applied, no best path is chosen. That is the whole of an RR (RFC 4456).
type Reflector struct {
	rrID   string
	tenant string
	hold   time.Duration

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
	return &Reflector{
		rrID:     cfg.RRID,
		tenant:   cfg.Tenant,
		hold:     hold,
		sessions: make(map[uint16]*serverSession),
		adjIn:    make(map[uint16]map[RouteKey]Route),
	}, nil
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
	if !s.trySend(hello) {
		delete(r.sessions, s.dev)
		s.shutdown()
		return fmt.Errorf("reflector: device %d: cannot accept hello_ok", s.dev)
	}
	// Initial UPDATE burst + END-OF-RIB, exactly as a BGP speaker does right
	// after the session is established. The device's own routes are left out
	// (we never send a route back to its origin), and an empty table is still an
	// EOR so the peer knows it has the whole RIB.
	for _, env := range announceEnvelopes(r.snapshotForLocked(s.dev)) {
		if !s.trySend(env) {
			delete(r.sessions, s.dev)
			s.shutdown()
			return fmt.Errorf("reflector: device %d: session too slow to accept initial snapshot", s.dev)
		}
	}
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

// snapshotForLocked is the initial UPDATE burst for dev: every route in the
// adj-rib-in, minus the device's own. BGP does not send a peer its own routes
// back (GoBGP filterPathFromSourcePeer); doing so would only invite loops.
func (r *Reflector) snapshotForLocked(dev uint16) []Route {
	var out []Route
	for origin, m := range r.adjIn {
		if origin == dev {
			continue
		}
		for _, rt := range m {
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

// clearRoutesLocked drops one origin's adj-rib-in and tells the other sessions.
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

// fanoutRoutesLocked advertises routes learned from origin to every other
// session. BGP's filterPathFromSourcePeer: the origin is the one peer that must
// not get them back.
func (r *Reflector) fanoutRoutesLocked(origin uint16, routes []Route) {
	for dev := range r.sessions {
		if dev == origin {
			continue
		}
		r.sendRoutesLocked(dev, routes)
	}
}

func (r *Reflector) fanoutKeysLocked(origin uint16, keys []RouteKey) {
	for dev := range r.sessions {
		if dev == origin {
			continue
		}
		r.withdrawKeysLocked(dev, keys)
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
