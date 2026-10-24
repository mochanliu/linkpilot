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
// OverlayAddrs are the local overlay /32s of this CPE's VPN tunnels (one per
// tunnel, controller-assigned). Every local prefix is announced once per
// overlay address, so a peer learns a path per tunnel -- that is what makes
// "下一跳是自己所有VPN隧道的本端overlay地址" true without the RR rewriting
// anything.
type ClientConfig struct {
	Tenant        string
	DeviceID      uint16
	VRFs          []VRFConfig
	LocalPrefixes []LocalPrefix
	OverlayAddrs  []string
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
	DroppedNoVRF      uint64         `json:"dropped_no_vrf"`
	DroppedRTMismatch uint64         `json:"dropped_rt_mismatch"`
	DroppedInvalid    uint64         `json:"dropped_invalid"`
	DroppedOwnRoute   uint64         `json:"dropped_own_route"`
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
	cfg   ClientConfig
	hold  time.Duration
	vrfs  map[uint8]*localVRF
	local []Route

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
	noVRF      uint64
	rtMismatch uint64
	invalid    uint64
	ownRoute   uint64
}

type clientSession struct {
	rrID       string
	addr       string
	conn       net.Conn
	up         bool
	synced     bool
	rib        map[RouteKey]Route
	reconnects uint64
	lastErr    string
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
	}

	overlays := make([]string, 0, len(cfg.OverlayAddrs))
	seenOverlay := map[string]bool{}
	for _, a := range cfg.OverlayAddrs {
		if err := validateNextHop(a); err != nil {
			return nil, fmt.Errorf("route client: overlay address: %w", err)
		}
		if seenOverlay[a] {
			return nil, fmt.Errorf("route client: overlay address %s listed twice", a)
		}
		seenOverlay[a] = true
		overlays = append(overlays, a)
	}

	for _, lp := range cfg.LocalPrefixes {
		if c.vrfs[lp.VRFID] == nil {
			return nil, fmt.Errorf("route client: local prefix %s references unconfigured vrf %d", lp.Prefix, lp.VRFID)
		}
		rts := exports[lp.VRFID]
		if len(rts) == 0 {
			return nil, fmt.Errorf("route client: vrf %d has local prefix %s but no export RTs (nothing could import it)",
				lp.VRFID, lp.Prefix)
		}
		for _, nh := range overlays {
			rt := Route{DeviceID: cfg.DeviceID, VRFID: lp.VRFID, Prefix: lp.Prefix, NextHop: nh, RTs: rts}
			if err := rt.Validate(); err != nil {
				return nil, fmt.Errorf("route client: local prefix: %w", err)
			}
			c.local = append(c.local, rt)
		}
	}
	c.local = SortedRoutes(c.local)

	for i := range c.sessions {
		c.sessions[i] = clientSession{addr: cfg.RRs[i].Addr, rib: map[RouteKey]Route{}}
	}
	return c, nil
}

// LocalRoutes returns the routes this CPE announces (device × VRF × prefix ×
// overlay). It is the input for tests and for later FIB integration, and it is
// fixed for the lifetime of this instance.
func (c *Client) LocalRoutes() []Route {
	return append([]Route(nil), c.local...)
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

	// Announce our whole table before EOR, in bounded chunks.
	for rest := c.local; len(rest) > 0; {
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
	s.rib = map[RouteKey]Route{}
	s.rrID = rrID
	s.reconnects++
	s.lastErr = ""
}

func (c *Client) markDown(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	s.up = false
	s.synced = false
	s.rib = map[RouteKey]Route{}
	s.conn = nil
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
		lv := c.vrfs[rt.VRFID]
		if lv == nil {
			c.counters.noVRF++
			continue
		}
		matched := false
		for _, rtTag := range rt.RTs {
			if lv.imports[rtTag] {
				matched = true
				break
			}
		}
		if !matched {
			c.counters.rtMismatch++
			continue
		}
		s.rib[k] = rt
		c.counters.imported++
	}
	return nil
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

// RIB returns the imported routes for one VRF from the active RR, sorted.
func (c *Client) RIB(vrf uint8) []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active < 0 {
		return nil
	}
	var out []Route
	for _, rt := range c.sessions[c.active].rib {
		if rt.VRFID == vrf {
			out = append(out, rt)
		}
	}
	return SortedRoutes(out)
}

// AllRIB returns every imported route on the active RR, grouped by VRF.
func (c *Client) AllRIB() map[uint8][]Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[uint8][]Route)
	if c.active < 0 {
		return out
	}
	for _, rt := range c.sessions[c.active].rib {
		out[rt.VRFID] = append(out[rt.VRFID], rt)
	}
	for vrf := range out {
		out[vrf] = SortedRoutes(out[vrf])
	}
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
		DroppedNoVRF:      c.counters.noVRF,
		DroppedRTMismatch: c.counters.rtMismatch,
		DroppedInvalid:    c.counters.invalid,
		DroppedOwnRoute:   c.counters.ownRoute,
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
