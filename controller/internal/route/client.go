package route

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// VRFConfig is the controller-assigned VRF on this CPE.
//
// ExportRTs are stamped onto every local route announced from this VRF.
// ImportRTs decide, per received route, whether it enters this VRF's "bgp"
// table: the route's RTs must intersect them. A VRF may legitimately have no
// import RTs (a hub that only exports); it may not export routes with no RTs.
type VRFConfig struct {
	ID        uint8
	ExportRTs []string
	ImportRTs []string
}

// LocalPrefix is one inside prefix this CPE serves through a VRF.
type LocalPrefix struct {
	VRFID  uint8
	Prefix string
}

// RRConfig is one reflector this CPE peers with. Two are expected (active +
// standby); the protocol works with any number, and the active is the lowest
// index among the synced ones.
type RRConfig struct {
	ID   string
	Addr string
}

// ClientConfig is the CPE route instance's static configuration.
//
// Tunnels is this device's VPN tunnel table as the controller assigned it: one
// entry per tunnel, naming the peer device and the remote overlay address used
// to reach it. The table is announced to the RR once per session and never
// travels with a prefix; it is what turns "this route came from device N" into
// the local paths that can reach N. Several entries may lead to the same peer
// (parallel tunnels); all of them are installed as ECMP paths.
type ClientConfig struct {
	Tenant        string
	DeviceID      uint16
	VRFs          []VRFConfig
	LocalPrefixes []LocalPrefix
	Tunnels       []Tunnel
	RRs           []RRConfig
	HoldSec       int
	// Dial opens a transport to an RR. It must already be authenticated as
	// DeviceID (mTLS), because the RR trusts the certificate for identity and
	// treats hello's device_id as a claim to cross-check.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	Logf func(format string, args ...any)
}

// SessionStats is the diagnostics view of one RR session.
type SessionStats struct {
	Addr       string `json:"addr"`
	RRID       string `json:"rr_id,omitempty"` // as reported by hello_ok
	Connected  bool   `json:"connected"`
	Synced     bool   `json:"synced"`
	Routes     int    `json:"routes"` // imported and held from this RR
	Reconnects uint64 `json:"reconnects"`
	LastError  string `json:"last_error,omitempty"`
}

// ClientStats is the whole instance's state. Counters are cumulative since
// NewClient except Routes, which is live.
type ClientStats struct {
	DeviceID          uint16         `json:"device_id"`
	ActiveRR          int            `json:"active_rr"` // index into RRs, -1 = none
	ActiveRIBRoutes   int            `json:"active_rib_routes"`
	Sessions          []SessionStats `json:"sessions"`
	Received          uint64         `json:"received"`
	Imported          uint64         `json:"imported"`
	Withdrawn         uint64         `json:"withdrawn"`
	DroppedRTMismatch uint64         `json:"dropped_rt_mismatch"`
	DroppedInvalid    uint64         `json:"dropped_invalid"`
	DroppedOwnRoute   uint64         `json:"dropped_own_route"`
	DroppedNoPath     uint64         `json:"dropped_no_path"`
}

// RIBEntry is one imported route plus the local next hops that reach its origin
// and the local VRFs it was imported into.
//
// The wire never carries next hops: they are resolved here from this device's
// own tunnel table, so one prefix is a single object at the RR and one entry per
// receiver only where it is actually installed.
//
// Targets is the answer to "本机哪些 VRF import 了这条路由的 RT"，它和 Route.VRFID
// （**来源** VPN）是两件事：导通组里 A 的 VPN 10 可以落进 B 的 VPN 20。一条路由在
// 每个目标表都可见 ⇒ 它只存一份、Targets 是一个集合，而不是每个目标一条记录：撤
// 路由是按 (来源设备, 来源 VPN, 前缀) 这个键来的（见 RouteKey），拆成多行就要在
// withdraw 那边记住"这条路由我装进了哪几张表"，那是同一份状态的第二个主人。
// 装表那边（internal/fib）拿 map 的键当目标表、拿 Route.VRFID 算标签。
type RIBEntry struct {
	Route
	Targets  []uint8  `json:"targets"`
	NextHops []string `json:"next_hops"`
}

// Client is the CPE route instance.
//
// Two design rules matter more than the code:
//
//   - The visible RIB belongs to exactly ONE RR (the lowest synced index).
//     Views are never merged: a route present only on the standby must not
//     resurrect after the active dies, and a flapping RR must not make the
//     table alternate between two partially-overlapping truths.
//
//   - An RR that is up but has not sent EOR contributes nothing. Serving a
//     snapshot that is still arriving would install a partial table and then
//     silently "lose" routes at EOR; empty is honest, partial is not.
type Client struct {
	cfg      ClientConfig
	hold     time.Duration
	vrfs     map[uint8]*localVRF
	vrfIDs   []uint8            // vrfs 的升序：Targets 要有稳定顺序（map 迭代是随机的）
	exports  map[uint8][]string // vrf -> canonical export RTs, stamped on local routes
	base     map[RouteKey]Route // static local_prefixes from config
	dynamic  map[RouteKey]Route // replaced wholesale by SetLocalPrefixes
	local    []Route            // effective (base + dynamic), sorted
	tunnels  []Tunnel
	nextHops map[uint16][]string // peer -> ascending next hops of its tunnels

	mu       sync.Mutex
	sessions []clientSession
	active   int
	counters counters

	runOnce sync.Once
}

type localVRF struct {
	imports map[string]bool
}

type counters struct {
	received   uint64
	imported   uint64
	withdrawn  uint64
	rtMismatch uint64
	invalid    uint64
	ownRoute   uint64
	noPath     uint64
}

type clientSession struct {
	rrID       string
	addr       string
	conn       net.Conn
	up         bool
	synced     bool
	rib        map[RouteKey]RIBEntry
	reconnects uint64
	lastErr    string

	// sc is the serialized writer owned by serveSession. ready is set once the
	// session's snapshot has been written; only then may local-prefix updates
	// be streamed to it. announced is what this session has been told.
	sc        *sessionConn
	ready     bool
	announced map[RouteKey]Route
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Tenant == "" {
		return nil, errors.New("route client: tenant is required")
	}
	if cfg.DeviceID == 0 {
		return nil, errors.New("route client: device_id must be non-zero")
	}
	if cfg.Dial == nil {
		return nil, errors.New("route client: Dial is required")
	}
	if len(cfg.RRs) == 0 {
		return nil, errors.New("route client: at least one RR is required")
	}
	for i, rr := range cfg.RRs {
		if rr.Addr == "" {
			return nil, fmt.Errorf("route client: rr[%d] has no address", i)
		}
	}
	hold, err := HoldDuration(cfg.HoldSec)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cfg:      cfg,
		hold:     hold,
		vrfs:     make(map[uint8]*localVRF, len(cfg.VRFs)),
		exports:  make(map[uint8][]string, len(cfg.VRFs)),
		base:     map[RouteKey]Route{},
		dynamic:  map[RouteKey]Route{},
		sessions: make([]clientSession, len(cfg.RRs)),
		active:   -1,
	}
	exports := make(map[uint8][]string, len(cfg.VRFs))
	for _, v := range cfg.VRFs {
		if _, dup := c.vrfs[v.ID]; dup {
			return nil, fmt.Errorf("route client: vrf %d configured twice", v.ID)
		}
		lv := &localVRF{imports: map[string]bool{}}
		exp := map[string]bool{}
		for _, rt := range v.ExportRTs {
			canon, err := CanonicalRT(rt)
			if err != nil {
				return nil, fmt.Errorf("route client: vrf %d: %w", v.ID, err)
			}
			exp[canon] = true
		}
		for _, rt := range v.ImportRTs {
			canon, err := CanonicalRT(rt)
			if err != nil {
				return nil, fmt.Errorf("route client: vrf %d: %w", v.ID, err)
			}
			lv.imports[canon] = true
		}
		c.vrfs[v.ID] = lv
		exports[v.ID] = sortedKeys(exp)
		c.exports[v.ID] = exports[v.ID]
		c.vrfIDs = append(c.vrfIDs, v.ID)
	}
	sort.Slice(c.vrfIDs, func(i, j int) bool { return c.vrfIDs[i] < c.vrfIDs[j] })

	tunnels := append([]Tunnel(nil), cfg.Tunnels...)
	for _, t := range tunnels {
		if t.Peer == cfg.DeviceID {
			return nil, fmt.Errorf("route client: tunnel peer %d is this device", cfg.DeviceID)
		}
	}
	tunnels = SortedTunnels(tunnels)
	if err := ValidateTunnels(tunnels); err != nil {
		return nil, fmt.Errorf("route client: tunnels: %w", err)
	}
	c.tunnels = tunnels
	c.nextHops = make(map[uint16][]string, len(tunnels))
	for _, t := range tunnels {
		c.nextHops[t.Peer] = append(c.nextHops[t.Peer], t.NextHop)
	}

	for _, lp := range cfg.LocalPrefixes {
		rt, err := c.localRoute(lp)
		if err != nil {
			return nil, err
		}
		c.base[rt.Key()] = rt
	}
	c.local = c.mergedLocalLocked()

	for i := range c.sessions {
		c.sessions[i] = clientSession{addr: cfg.RRs[i].Addr, rib: map[RouteKey]RIBEntry{}}
	}
	return c, nil
}

// LocalRoutes returns the routes this CPE announces (one per VRF × prefix, no
// next hops): the static configuration plus whatever SetLocalPrefixes last
// installed.
func (c *Client) LocalRoutes() []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Route(nil), c.local...)
}

// localRoute builds a local route from a (vrf, prefix) pair. Export RTs come
// from the VRF config, so a local prefix can never be announced without the
// policy that makes it importable somewhere.
func (c *Client) localRoute(lp LocalPrefix) (Route, error) {
	if c.vrfs[lp.VRFID] == nil {
		return Route{}, fmt.Errorf("route client: local prefix %s references unconfigured vrf %d", lp.Prefix, lp.VRFID)
	}
	rts := c.exports[lp.VRFID]
	if len(rts) == 0 {
		return Route{}, fmt.Errorf("route client: vrf %d has local prefix %s but no export RTs (nothing could import it)",
			lp.VRFID, lp.Prefix)
	}
	rt := Route{
		DeviceID: c.cfg.DeviceID,
		VRFID:    lp.VRFID,
		Prefix:   lp.Prefix,
		RTs:      rts,
	}
	if err := rt.Validate(); err != nil {
		return Route{}, fmt.Errorf("route client: local prefix: %w", err)
	}
	return rt, nil
}

// mergedLocalLocked is base ∪ dynamic. A dynamic prefix identical to a static
// one collapses to one route: the same (device, vrf, prefix) key must be
// announced once, or a withdraw of one copy would take the other with it.
func (c *Client) mergedLocalLocked() []Route {
	m := make(map[RouteKey]Route, len(c.base)+len(c.dynamic))
	for k, r := range c.base {
		m[k] = r
	}
	for k, r := range c.dynamic {
		m[k] = r
	}
	out := make([]Route, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return SortedRoutes(out)
}

// SetLocalPrefixes replaces the dynamic local set (customer prefixes learned
// from the in-box FRR). The static local_prefixes from the config are always
// kept. Every live session receives the difference immediately; a session
// still streaming its snapshot reconciles against its own announced set once
// the snapshot is done.
func (c *Client) SetLocalPrefixes(lps []LocalPrefix) error {
	next := make(map[RouteKey]Route, len(lps))
	for _, lp := range lps {
		rt, err := c.localRoute(lp)
		if err != nil {
			return err
		}
		next[rt.Key()] = rt
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if equalRouteMaps(c.dynamic, next) {
		return nil
	}
	c.dynamic = next
	c.local = c.mergedLocalLocked()
	c.pushLocalLocked()
	return nil
}

// pushLocalLocked streams local-set changes to every session that has finished
// its snapshot. Must be called with c.mu held.
func (c *Client) pushLocalLocked() {
	desired := make(map[RouteKey]Route, len(c.local))
	for _, r := range c.local {
		desired[r.Key()] = r
	}
	for i := range c.sessions {
		s := &c.sessions[i]
		if !s.ready || s.sc == nil {
			continue
		}
		c.syncSessionLocalLocked(s, desired)
	}
}

// syncSessionLocalLocked writes the difference between what this session knows
// and the desired local set. Write failure closes the session: the reconnect
// path re-sends a full snapshot, which is more reliable than patching up a
// half-written stream. Must be called with c.mu held.
func (c *Client) syncSessionLocalLocked(s *clientSession, desired map[RouteKey]Route) {
	var adds []Route
	var dels []RouteKey
	for k, rt := range desired {
		if old, ok := s.announced[k]; !ok || !old.Equal(rt) {
			adds = append(adds, rt)
		}
	}
	for k := range s.announced {
		if _, ok := desired[k]; !ok {
			dels = append(dels, k)
		}
	}
	if len(adds) == 0 && len(dels) == 0 {
		return
	}
	adds = SortedRoutes(adds)
	dels = SortedKeys(dels)
	if len(adds) > 0 {
		if err := s.sc.write(Envelope{Type: MsgAnnounce, Routes: adds}); err != nil {
			s.sc.close()
			return
		}
	}
	if len(dels) > 0 {
		if err := s.sc.write(Envelope{Type: MsgWithdraw, Keys: dels}); err != nil {
			s.sc.close()
			return
		}
	}
	s.announced = cloneRoutes(desired)
}

func equalRouteMaps(a, b map[RouteKey]Route) bool {
	if len(a) != len(b) {
		return false
	}
	for k, ra := range a {
		rb, ok := b[k]
		if !ok || !ra.Equal(rb) {
			return false
		}
	}
	return true
}

func cloneRoutes(in map[RouteKey]Route) map[RouteKey]Route {
	out := make(map[RouteKey]Route, len(in))
	for k, r := range in {
		out[k] = r
	}
	return out
}

// Tunnels returns the table this device announces, in canonical order.
func (c *Client) Tunnels() []Tunnel {
	return append([]Tunnel(nil), c.tunnels...)
}

// Run keeps both RR sessions alive until ctx is cancelled. It returns nil.
func (c *Client) Run(ctx context.Context) error {
	var started bool
	c.runOnce.Do(func() { started = true })
	if !started {
		return errors.New("route client: Run may only be called once")
	}
	var wg sync.WaitGroup
	for i := range c.cfg.RRs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c.runRR(ctx, idx)
		}(i)
	}
	<-ctx.Done()
	c.mu.Lock()
	for i := range c.sessions {
		if c.sessions[i].conn != nil {
			c.sessions[i].conn.Close()
		}
	}
	c.mu.Unlock()
	wg.Wait()
	return nil
}

func (c *Client) runRR(ctx context.Context, idx int) {
	backoff := initialBackoff
	for ctx.Err() == nil {
		conn, err := c.cfg.Dial(ctx, c.cfg.RRs[idx].Addr)
		if err != nil {
			c.noteError(idx, err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		handshook, err := c.serveSession(idx, conn)
		if ctx.Err() != nil {
			return
		}
		c.noteError(idx, err)
		if handshook {
			backoff = initialBackoff
		} else {
			backoff = nextBackoff(backoff)
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serveSession runs one connection: hello, snapshot, keepalive, read loop.
// handshook reports whether hello_ok arrived, which decides whether the
// reconnect backoff starts over.
func (c *Client) serveSession(idx int, conn net.Conn) (handshook bool, err error) {
	sc := &sessionConn{conn: conn, writeTimeout: c.hold}
	sessionDone := make(chan struct{})
	defer func() {
		close(sessionDone)
		sc.close()
		c.markDown(idx)
	}()

	conn.SetDeadline(time.Now().Add(c.hold))
	br := bufio.NewReaderSize(conn, 64<<10)
	if err := sc.write(Envelope{Type: MsgHello, Version: ProtocolVersion, DeviceID: c.cfg.DeviceID}); err != nil {
		return false, fmt.Errorf("send hello: %w", err)
	}
	env, err := ReadEnvelope(br)
	if err != nil {
		return false, fmt.Errorf("read hello_ok: %w", err)
	}
	if env.Type == MsgError {
		return false, fmt.Errorf("rr rejected hello: %s", env.Message)
	}
	if env.Type != MsgHelloOK {
		return false, fmt.Errorf("expected hello_ok, got %q", env.Type)
	}
	if env.Version != ProtocolVersion {
		return false, fmt.Errorf("rr speaks protocol version %d (want %d)", env.Version, ProtocolVersion)
	}
	hold, err := HoldDuration(env.HoldSec)
	if err != nil {
		return false, fmt.Errorf("rr hello_ok: %w", err)
	}
	conn.SetDeadline(time.Time{})
	sc.writeTimeout = hold
	handshook = true

	c.markUp(idx, conn, env.RRID, hold)
	c.mu.Lock()
	c.sessions[idx].sc = sc
	c.mu.Unlock()

	// The tunnel table goes first: the RR withholds its snapshot until it knows
	// which peers this device can reach, and it must not see that as "no
	// tunnels". It is also a full replacement, so every reconnect re-asserts it.
	tunnels := c.Tunnels()
	if err := sc.write(Envelope{Type: MsgTunnels, Tunnels: &tunnels}); err != nil {
		return true, fmt.Errorf("send tunnel table: %w", err)
	}

	// Announce our whole table before EOR, in bounded chunks.
	local := c.LocalRoutes()
	for rest := local; len(rest) > 0; {
		n := len(rest)
		if n > MaxRoutesPerMessage {
			n = MaxRoutesPerMessage
		}
		if err := sc.write(Envelope{Type: MsgAnnounce, Routes: rest[:n]}); err != nil {
			return true, fmt.Errorf("send snapshot: %w", err)
		}
		rest = rest[n:]
	}
	if err := sc.write(Envelope{Type: MsgEOR}); err != nil {
		return true, fmt.Errorf("send eor: %w", err)
	}

	// From here on the session is part of the live local-prefix fanout. The
	// snapshot may already be stale: reconcile against the current set before
	// releasing the lock, so no update can slip between snapshot and fanout.
	c.mu.Lock()
	s := &c.sessions[idx]
	if !s.up {
		c.mu.Unlock()
		return true, fmt.Errorf("session went down while sending snapshot")
	}
	s.ready = true
	s.announced = make(map[RouteKey]Route, len(local))
	for _, rt := range local {
		s.announced[rt.Key()] = rt
	}
	desired := make(map[RouteKey]Route, len(c.local))
	for _, rt := range c.local {
		desired[rt.Key()] = rt
	}
	c.syncSessionLocalLocked(s, desired)
	c.mu.Unlock()

	go c.keepaliveLoop(sc, hold/3, sessionDone)

	for {
		conn.SetReadDeadline(time.Now().Add(hold))
		env, err := ReadEnvelope(br)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return true, fmt.Errorf("hold timer expired")
			}
			return true, err
		}
		switch env.Type {
		case MsgKeepalive:
		case MsgEOR:
			c.markSynced(idx)
		case MsgAnnounce:
			if err := c.applyAnnounce(idx, env.Routes); err != nil {
				return true, err
			}
		case MsgWithdraw:
			if err := c.applyWithdraw(idx, env.Keys); err != nil {
				return true, err
			}
		case MsgError:
			return true, fmt.Errorf("rr error: %s", env.Message)
		default:
			return true, fmt.Errorf("unexpected %q after handshake", env.Type)
		}
	}
}

func (c *Client) keepaliveLoop(sc *sessionConn, every time.Duration, done <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := sc.write(Envelope{Type: MsgKeepalive}); err != nil {
				sc.close()
				return
			}
		}
	}
}

func (c *Client) markUp(idx int, conn net.Conn, rrID string, hold time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	s.conn = conn
	s.up = true
	s.synced = false
	s.rib = map[RouteKey]RIBEntry{}
	s.rrID = rrID
	s.reconnects++
	s.lastErr = ""
	s.sc = nil
	s.ready = false
	s.announced = nil
}

func (c *Client) markDown(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	s.up = false
	s.synced = false
	s.rib = map[RouteKey]RIBEntry{}
	s.conn = nil
	s.sc = nil
	s.ready = false
	s.announced = nil
	c.pickActiveLocked()
}

func (c *Client) markSynced(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	if !s.up {
		return
	}
	s.synced = true
	c.pickActiveLocked()
}

func (c *Client) noteError(idx int, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.sessions[idx].lastErr = err.Error()
	c.mu.Unlock()
	if c.cfg.Logf != nil {
		c.cfg.Logf("route: rr %s (device %d): %v", c.cfg.RRs[idx].Addr, c.cfg.DeviceID, err)
	}
}

// pickActiveLocked switches the visible RIB to the lowest synced session. Only
// a session with EOR may serve: a standby in the middle of its snapshot is not
// a valid fallback, so if only it is up the table is empty.
func (c *Client) pickActiveLocked() {
	best := -1
	for i := range c.sessions {
		if c.sessions[i].up && c.sessions[i].synced {
			best = i
			break
		}
	}
	c.active = best
}

func (c *Client) applyAnnounce(idx int, routes []Route) error {
	last := make(map[RouteKey]Route, len(routes))
	order := make([]RouteKey, 0, len(routes))
	for _, rt := range routes {
		if rt.DeviceID == c.cfg.DeviceID {
			// The RR must not reflect our own path back; if it does, installing
			// it would make the data plane point our own prefix at a tunnel.
			c.mu.Lock()
			c.counters.ownRoute++
			c.mu.Unlock()
			continue
		}
		if err := rt.Validate(); err != nil {
			c.mu.Lock()
			c.counters.invalid++
			c.mu.Unlock()
			continue
		}
		k := rt.Key()
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = rt
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	if !s.up {
		return fmt.Errorf("announce on a session that is not up")
	}
	c.counters.received += uint64(len(order))
	for _, k := range order {
		rt := last[k]
		delete(s.rib, k) // RT change can revoke the import
		// RT 匹配决定的是**这条路由装进本机哪几张表**，与来源 VPN 无关
		// （§4.3 第 1 步）。来源 VPN 只决定标签：对端弹栈后查的是它自己那张表
		// （§2.4）。所以这里不能再看 rt.VRFID 在本机存不存在 —— 导通要解决的正是
		// "本机没有的那个 VPN 的路由落进本机有的 VPN 表"，旧代码那道
		// `c.vrfs[rt.VRFID] == nil` 的闸会把所有跨 VPN 的导通在 RT 匹配之前就丢掉。
		targets := c.importTargets(rt.RTs)
		if len(targets) == 0 {
			c.counters.rtMismatch++
			continue
		}
		// The route is only installed if this device actually has a tunnel to
		// its origin. The RR should never send one without adjacency; if it does,
		// the table is inconsistent and a route with no path would only make the
		// FIB warn every poll.
		nh := c.nextHops[rt.DeviceID]
		if len(nh) == 0 {
			c.counters.noPath++
			continue
		}
		s.rib[k] = RIBEntry{Route: rt, Targets: targets, NextHops: append([]string(nil), nh...)}
		c.counters.imported++
	}
	return nil
}

// importTargets 返回本机 import 了这些 RT 的 VRF，升序。
// 升序不是整理癖：Targets 会进 /status 的 JSON，也是 sr-fib 每轮重算的输入，
// 顺序不稳定就等于每轮都"看起来变了"。
func (c *Client) importTargets(rts []string) []uint8 {
	var out []uint8
	for _, id := range c.vrfIDs {
		lv := c.vrfs[id]
		for _, rtTag := range rts {
			if lv.imports[rtTag] {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func (c *Client) applyWithdraw(idx int, keys []RouteKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	if !s.up {
		return fmt.Errorf("withdraw on a session that is not up")
	}
	for _, k := range keys {
		if _, ok := s.rib[k]; ok {
			delete(s.rib, k)
			c.counters.withdrawn++
		}
	}
	return nil
}

// RIB returns the routes imported into one local VRF from the active RR, sorted.
// 这里的 vrf 是**目标**表：一条来源 VPN 不在这台盒子上的路由，只要 RT 命中了这个
// VRF 的 import，就会出现在这里（§4.3 第 1 步）。
func (c *Client) RIB(vrf uint8) []RIBEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active < 0 {
		return nil
	}
	var out []RIBEntry
	for _, rt := range c.sessions[c.active].rib {
		for _, t := range rt.Targets {
			if t == vrf {
				out = append(out, rt)
				break
			}
		}
	}
	return sortedEntries(out)
}

// AllRIB returns every imported route on the active RR, grouped by **target** VRF
// —— 也就是 sr-fib 要往哪张表装。同一个 RouteKey 可以出现在几张表下面。
func (c *Client) AllRIB() map[uint8][]RIBEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[uint8][]RIBEntry)
	if c.active < 0 {
		return out
	}
	for _, rt := range c.sessions[c.active].rib {
		for _, t := range rt.Targets {
			out[t] = append(out[t], rt)
		}
	}
	for vrf := range out {
		out[vrf] = sortedEntries(out[vrf])
	}
	return out
}

// sortedEntries orders RIB rows the same way SortedRoutes orders routes, so a
// dump does not depend on map iteration order.
func sortedEntries(in []RIBEntry) []RIBEntry {
	out := append([]RIBEntry(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		if out[i].VRFID != out[j].VRFID {
			return out[i].VRFID < out[j].VRFID
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out
}

// Stats snapshots the instance state for admin output and tests.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := ClientStats{
		DeviceID:          c.cfg.DeviceID,
		ActiveRR:          c.active,
		Received:          c.counters.received,
		Imported:          c.counters.imported,
		Withdrawn:         c.counters.withdrawn,
		DroppedRTMismatch: c.counters.rtMismatch,
		DroppedInvalid:    c.counters.invalid,
		DroppedOwnRoute:   c.counters.ownRoute,
		DroppedNoPath:     c.counters.noPath,
	}
	if c.active >= 0 {
		st.ActiveRIBRoutes = len(c.sessions[c.active].rib)
	}
	for i := range c.sessions {
		s := &c.sessions[i]
		st.Sessions = append(st.Sessions, SessionStats{
			Addr:       s.addr,
			RRID:       s.rrID,
			Connected:  s.up,
			Synced:     s.synced,
			Routes:     len(s.rib),
			Reconnects: s.reconnects,
			LastError:  s.lastErr,
		})
	}
	return st
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionConn serializes writes (snapshot vs keepalive) and makes close
// idempotent, so a keepalive timer and the reader never race on the socket.
type sessionConn struct {
	mu           sync.Mutex
	conn         net.Conn
	writeTimeout time.Duration
	closed       bool
}

func (sc *sessionConn) write(env Envelope) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return net.ErrClosed
	}
	sc.conn.SetWriteDeadline(time.Now().Add(sc.writeTimeout))
	return WriteEnvelope(sc.conn, env)
}

func (sc *sessionConn) close() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return
	}
	sc.closed = true
	sc.conn.Close()
}
