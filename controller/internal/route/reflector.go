package route

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PeerRole tells the two kinds of session an RR serves apart. Both authenticate
// with a tenant certificate; the role is what that certificate's URI says, not
// something the session may claim in the hello.
type PeerRole string

const (
	// RoleClient is a CPE: srou://<tenant>/node/<n>.
	RoleClient PeerRole = "client"
	// RoleRR is another reflector: srou://<tenant>/rr/<id>.
	RoleRR PeerRole = "rr"
)

// PeerIdentity is who a session speaks for. It is read from the peer's mTLS
// certificate by the caller (keybinding.RefOf), never from the hello: the
// hello's device_id is only a claim, and the check is that it agrees with this.
type PeerIdentity struct {
	Tenant   string
	Role     PeerRole
	DeviceID uint16 // RoleClient: the node the certificate names
	RRID     string // RoleRR: the reflector the certificate names
}

// Key is the reflector's session key: "node/<n>" for a CPE, "rr/<id>" for a
// reflector. Every table in the reflector is keyed by it, because a reflector
// session can carry routes for any origin device -- the session is the peer,
// the device id inside a route is only what that route says about itself.
func (p PeerIdentity) Key() string {
	if p.Role == RoleRR {
		return "rr/" + p.RRID
	}
	return "node/" + strconv.Itoa(int(p.DeviceID))
}

// Validate refuses an identity that cannot name a session. A missing role gets
// its own message: defaulting it to "client" would let a certificate that says
// nothing about what it is be treated as a device.
func (p PeerIdentity) Validate() error {
	switch p.Role {
	case RoleClient:
		if p.DeviceID == 0 {
			return errors.New("client identity has device id 0 (reserved)")
		}
	case RoleRR:
		if p.RRID == "" {
			return errors.New("rr identity has an empty rr id")
		}
		if strings.ContainsRune(p.RRID, '/') {
			return fmt.Errorf("rr id %q contains a path separator", p.RRID)
		}
	default:
		return fmt.Errorf("unknown session role %q", p.Role)
	}
	if p.Tenant == "" {
		return errors.New("identity has no tenant")
	}
	return nil
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
// It keeps one table per session: that session's Adj-RIB-In, keyed by session
// key, plus the role it spoke in. A route is advertised under one rule, copied
// from RFC 4456 / GoBGP's filterPathFromSourcePeer:
//
//	a route may go to a session iff it did not come from that session, and
//	( it came from a client, or the receiver is a client ).
//
// No route is rewritten on the way out: no next hop, no RT, no best path. That
// is the whole of a reflector.
type Reflector struct {
	rrID   string
	tenant string
	hold   time.Duration

	mu       sync.Mutex
	sessions map[string]*serverSession
	adjIn    map[string]*adjRibIn
}

// adjRibIn is one session's Adj-RIB-In. The role is stored next to the routes
// because the fan-out rule depends on where a route ENTERED, and the session
// that entry belongs to can disappear while its routes are still being
// withdrawn (clearRoutesLocked has to know what it is withdrawing from).
type adjRibIn struct {
	role   PeerRole
	routes map[RouteKey]Route
}

// SessionInfo is the admin/tests view of one live session.
type SessionInfo struct {
	DeviceID uint16    `json:"device_id"` // clients only; 0 for an rr session
	Role     PeerRole  `json:"role"`
	Key      string    `json:"key"`
	Since    time.Time `json:"since"`
	Synced   bool      `json:"synced"` // peer sent EOR after its first snapshot
	Routes   int       `json:"routes"` // this session's adj-rib-in size
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
		sessions: make(map[string]*serverSession),
		adjIn:    make(map[string]*adjRibIn),
	}, nil
}

// ServeConn runs one peer session until it dies. The caller has already
// completed the TLS handshake and must derive id from the verified client
// certificate.
//
// Blocking; run one goroutine per accepted connection. Returning an error is
// normal on disconnect -- the caller decides whether to log it.
func (r *Reflector) ServeConn(conn net.Conn, id PeerIdentity) error {
	if err := id.Validate(); err != nil {
		return reject(conn, err.Error())
	}
	if id.Tenant != r.tenant {
		return reject(conn, fmt.Sprintf("tenant %q is not served by this reflector", id.Tenant))
	}
	if id.Role == RoleRR && id.RRID == r.rrID {
		// A reflector dialling itself would hand its own table back as a second
		// copy of every client route. That is a configuration error, not a
		// session to nurse along.
		return reject(conn, "a reflector does not peer with itself")
	}

	key := id.Key()
	conn.SetDeadline(time.Now().Add(r.hold))
	br := bufio.NewReaderSize(conn, 64<<10)
	env, err := ReadEnvelope(br)
	if err != nil {
		conn.Close()
		return fmt.Errorf("reflector: %s: read hello: %w", key, err)
	}
	if env.Type != MsgHello {
		return reject(conn, fmt.Sprintf("expected hello, got %q", env.Type))
	}
	if env.Version != ProtocolVersion {
		return reject(conn, fmt.Sprintf("protocol version %d unsupported (want %d)", env.Version, ProtocolVersion))
	}
	switch id.Role {
	case RoleClient:
		if env.DeviceID != id.DeviceID {
			return reject(conn, fmt.Sprintf("hello claims device %d but the certificate is for %d", env.DeviceID, id.DeviceID))
		}
	case RoleRR:
		// A reflector is named by the certificate, not by a device id. The hello
		// may not claim one: a device id belongs to a node, and one node's routes
		// must never enter through a reflector's session.
		if env.DeviceID != 0 {
			return reject(conn, fmt.Sprintf("hello claims device %d, but an rr session speaks for no device", env.DeviceID))
		}
	}
	conn.SetDeadline(time.Time{})

	s := &serverSession{
		id:    id,
		key:   key,
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

	if old := r.sessions[s.key]; old != nil {
		// Newest session wins (a CPE reconnecting after its network came back
		// must not keep talking to a half-dead socket). Its routes go with it:
		// the new session sends a fresh full snapshot, and keeping the old paths
		// in the meantime would advertise reachability that nobody re-asserted.
		r.clearRoutesLocked(old.key)
		delete(r.sessions, old.key)
		old.shutdown()
	}
	r.sessions[s.key] = s

	hello := Envelope{Type: MsgHelloOK, Version: ProtocolVersion, RRID: r.rrID, HoldSec: int(r.hold / time.Second)}
	if !s.trySend(hello) {
		delete(r.sessions, s.key)
		s.shutdown()
		return fmt.Errorf("reflector: %s: cannot accept hello_ok", s.key)
	}
	// Initial UPDATE burst + END-OF-RIB, exactly as a BGP speaker does right
	// after the session is established. The peer's own routes are left out (we
	// never send a route back to its origin), and an empty table is still an EOR
	// so the peer knows it has the whole RIB.
	for _, env := range announceEnvelopes(r.snapshotForLocked(s.id)) {
		if !s.trySend(env) {
			delete(r.sessions, s.key)
			s.shutdown()
			return fmt.Errorf("reflector: %s: session too slow to accept initial snapshot", s.key)
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

// snapshotForLocked is the initial UPDATE burst for dst: every route the
// fan-out rule lets us send it. For a client that is every other session's
// table; for a reflector it is only what clients told us, because a reflector
// is not a place a route is re-reflected through.
func (r *Reflector) snapshotForLocked(dst PeerIdentity) []Route {
	var out []Route
	for key, in := range r.adjIn {
		if key == dst.Key() {
			continue
		}
		if dst.Role != RoleClient && in.role != RoleClient {
			continue
		}
		for _, rt := range in.routes {
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
				return fmt.Errorf("reflector: %s: hold timer expired", s.key)
			}
			return fmt.Errorf("reflector: %s: %w", s.key, err)
		}
		switch env.Type {
		case MsgKeepalive:
			// Nothing to do: the deadline was just pushed out by the read.
		case MsgEOR:
			r.mu.Lock()
			if cur := r.sessions[s.key]; cur == s {
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
			return fmt.Errorf("reflector: %s: peer error: %s", s.key, env.Message)
		default:
			return fmt.Errorf("reflector: %s: unexpected %q after handshake", s.key, env.Type)
		}
	}
}

func (r *Reflector) applyAnnounce(s *serverSession, routes []Route) error {
	for _, rt := range routes {
		// A client may only speak for itself. A reflector speaks for whatever
		// it learned -- that is what it is for -- and the tenant check on its
		// certificate is what bounds "whatever".
		if s.id.Role == RoleClient && rt.DeviceID != s.id.DeviceID {
			return fmt.Errorf("reflector: %s: announced route for device %d (a node may only speak for itself)",
				s.key, rt.DeviceID)
		}
		if err := rt.Validate(); err != nil {
			return fmt.Errorf("reflector: %s: %w", s.key, err)
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
	if r.sessions[s.key] != s {
		return fmt.Errorf("reflector: %s: session was replaced", s.key)
	}
	in := r.adjIn[s.key]
	if in == nil {
		in = &adjRibIn{role: s.id.Role, routes: make(map[RouteKey]Route)}
		r.adjIn[s.key] = in
	}
	changed := make([]Route, 0, len(order))
	for _, k := range order {
		rt := last[k]
		if old, ok := in.routes[k]; ok && old.Equal(rt) {
			continue // identical re-announcement: not an update
		}
		in.routes[k] = rt
		changed = append(changed, rt)
	}
	r.fanoutRoutesLocked(s, changed)
	return nil
}

func (r *Reflector) applyWithdraw(s *serverSession, keys []RouteKey) error {
	for _, k := range keys {
		if s.id.Role == RoleClient && k.DeviceID != s.id.DeviceID {
			return fmt.Errorf("reflector: %s: withdrawal for device %d (a node may only speak for itself)",
				s.key, k.DeviceID)
		}
		if err := k.Validate(); err != nil {
			return fmt.Errorf("reflector: %s: %w", s.key, err)
		}
	}
	if len(keys) == 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.key] != s {
		return fmt.Errorf("reflector: %s: session was replaced", s.key)
	}
	in := r.adjIn[s.key]
	var removed []RouteKey
	for _, k := range keys {
		if in == nil {
			break
		}
		if _, ok := in.routes[k]; !ok {
			continue // already gone (e.g. cleared by reconnect): idempotent
		}
		delete(in.routes, k)
		removed = append(removed, k)
	}
	r.fanoutKeysLocked(s.key, s.id.Role, SortedKeys(removed))
	return nil
}

// clearRoutesLocked drops one session's adj-rib-in and tells the sessions the
// fan-out rule allows -- minus the keys some other session still holds.
func (r *Reflector) clearRoutesLocked(key string) {
	in := r.adjIn[key]
	if in == nil {
		return
	}
	delete(r.adjIn, key)
	if len(in.routes) == 0 {
		return
	}
	keys := make([]RouteKey, 0, len(in.routes))
	for k := range in.routes {
		keys = append(keys, k)
	}
	r.fanoutKeysLocked(key, in.role, SortedKeys(keys))
}

// fanoutRoutesLocked advertises routes learned on src to every session the
// fan-out rule allows (see Reflector).
func (r *Reflector) fanoutRoutesLocked(src *serverSession, routes []Route) {
	if len(routes) == 0 {
		return
	}
	for key, dst := range r.sessions {
		if key == src.key {
			continue
		}
		if src.id.Role != RoleClient && dst.id.Role != RoleClient {
			continue
		}
		r.sendRoutesLocked(dst, routes)
	}
}

// fanoutKeysLocked withdraws keys that left one session's table from every
// session the rule allows -- but only where no other session still holds the
// key. The same (origin, VRF, prefix) can sit in two tables at once (a device
// announces it to this RR directly while another RR reflects it here); taking
// it out of the receiver because one of the two let go would erase a route the
// other one still vouches for.
func (r *Reflector) fanoutKeysLocked(srcKey string, srcRole PeerRole, keys []RouteKey) {
	if len(keys) == 0 {
		return
	}
	for key, dst := range r.sessions {
		if key == srcKey {
			continue
		}
		if srcRole != RoleClient && dst.id.Role != RoleClient {
			continue
		}
		gone := make([]RouteKey, 0, len(keys))
		for _, k := range keys {
			if r.stillHeldLocked(dst, k) {
				continue
			}
			gone = append(gone, k)
		}
		r.withdrawKeysLocked(dst, SortedKeys(gone))
	}
}

// stillHeldLocked reports whether some peer other than dst still holds k, in a
// direction dst is allowed to receive from.
func (r *Reflector) stillHeldLocked(dst *serverSession, k RouteKey) bool {
	for key, in := range r.adjIn {
		if key == dst.key {
			continue // a session's own table is never reflected back to it
		}
		if dst.id.Role != RoleClient && in.role != RoleClient {
			continue
		}
		if _, ok := in.routes[k]; ok {
			return true
		}
	}
	return false
}

func (r *Reflector) sendRoutesLocked(to *serverSession, routes []Route) {
	for len(routes) > 0 {
		n := len(routes)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		if !to.trySend(Envelope{Type: MsgAnnounce, Routes: routes[:n]}) {
			to.shutdown()
			return
		}
		routes = routes[n:]
	}
}

func (r *Reflector) withdrawKeysLocked(to *serverSession, keys []RouteKey) {
	for len(keys) > 0 {
		n := len(keys)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		if !to.trySend(Envelope{Type: MsgWithdraw, Keys: keys[:n]}) {
			to.shutdown()
			return
		}
		keys = keys[n:]
	}
}

func (r *Reflector) detach(s *serverSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[s.key] == s {
		delete(r.sessions, s.key)
		r.clearRoutesLocked(s.key)
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

// Routes returns the whole adj-rib-in aggregated by origin device, sorted.
//
// Two sessions can carry the same route (one direct, one reflected); the view
// reports it once, because it answers "what does this RR know", not "which
// peer told it". The copy kept is chosen deterministically -- a client's own
// word beats a reflected one, then the lower session key -- so two polls of an
// unchanged router cannot disagree about which RT list they show.
func (r *Reflector) Routes() map[uint16][]Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	return routesByOriginLocked(r.adjIn)
}

// RoutesOf returns the routes one origin device is known to advertise, from
// every session that carries them, deduped the same way Routes does.
func (r *Reflector) RoutesOf(dev uint16) []Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	return routesByOriginLocked(r.adjIn)[dev]
}

func routesByOriginLocked(adjIn map[string]*adjRibIn) map[uint16][]Route {
	chosen := make(map[RouteKey]Route)
	from := make(map[RouteKey]string)
	for key, in := range adjIn {
		for k, rt := range in.routes {
			prev, ok := from[k]
			if ok && !preferredSource(key, in.role, prev, adjIn[prev].role) {
				continue
			}
			chosen[k] = rt
			from[k] = key
		}
	}
	out := make(map[uint16][]Route)
	for _, rt := range chosen {
		out[rt.DeviceID] = append(out[rt.DeviceID], rt)
	}
	for dev := range out {
		out[dev] = SortedRoutes(out[dev])
	}
	return out
}

// preferredSource says which of two copies of the same key is reported: the
// first-hand one, then the lower session key.
func preferredSource(aKey string, aRole PeerRole, bKey string, bRole PeerRole) bool {
	if (aRole == RoleClient) != (bRole == RoleClient) {
		return aRole == RoleClient
	}
	return aKey < bKey
}

// Sessions returns the live sessions, sorted by key.
func (r *Reflector) Sessions() []SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionInfo, 0, len(r.sessions))
	for key, s := range r.sessions {
		var n int
		if in := r.adjIn[key]; in != nil {
			n = len(in.routes)
		}
		out = append(out, SessionInfo{
			DeviceID: s.id.DeviceID,
			Role:     s.id.Role,
			Key:      key,
			Since:    s.since,
			Synced:   s.synced,
			Routes:   n,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

type serverSession struct {
	id    PeerIdentity
	key   string
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
