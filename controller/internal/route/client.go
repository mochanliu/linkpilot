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
	Routes     int    `json:"routes"`  // installed in the table from this RR
	Pending    int    `json:"pending"` // held from this RR, not installed
	Reconnects uint64 `json:"reconnects"`
	LastError  string `json:"last_error,omitempty"`
}

// ClientStats is the whole instance's state. The drop/import counters are
// cumulative and event-shaped: they count what each received message did, so a
// route that the local tunnel or VRF table later moves between installed and
// held back does not change the numbers after the fact. Routes is live.
type ClientStats struct {
	DeviceID          uint16         `json:"device_id"`
	ActiveRR          int            `json:"active_rr"` // index into RRs, -1 = none
	ActiveRIBRoutes   int            `json:"active_rib_routes"`
	ActivePending     int            `json:"active_pending"` // known from the active RR, not installed
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

// Why a received route is held back instead of installed. These are the two
// gates in §8; a route that fails neither is in the RIB. Both are reversible:
// changing a local table can move a route across either one, in either
// direction. Malformed and self-originated routes are not here -- they are
// rejected at intake and never reach adj-RIBs-in, so nothing can resurrect them.
const (
	// PendingRTMismatch: no local VRF imports any of its RTs (§4.3 step 1).
	PendingRTMismatch = "rt_mismatch"
	// PendingNoPath: this device has no tunnel to the origin right now. The RR
	// only reflects over mutual adjacency, so this is either a route whose
	// tunnel SetTunnels just removed, or one the RR sent before our new table
	// reached it. Either way it is transient: the next table change decides.
	PendingNoPath = "no_path"
)

// Why a configured local prefix is not being announced.
const (
	SkipVRFUnconfigured = "vrf_unconfigured"
	SkipNoExportRTs     = "no_export_rts"
)

// PendingRoute is one route held in this session's adj-RIBs-in that is not
// installed, with the gate that stopped it. It is visible so the console can
// say "this prefix is known and not deployed, because ...", which is the whole
// point of keeping the route instead of dropping it.
type PendingRoute struct {
	Route
	Reason string `json:"reason"`
}

// LocalSkip is one local prefix the device knows about but cannot announce
// under the current VRF table.
type LocalSkip struct {
	VRFID  uint8  `json:"vrf_id"`
	Prefix string `json:"prefix"`
	Reason string `json:"reason"`
}

// Client is the CPE route instance.
//
// Three design rules matter more than the code:
//
//   - The visible RIB belongs to exactly ONE RR (the lowest synced index).
//     Views are never merged: a route present only on the standby must not
//     resurrect after the active dies, and a flapping RR must not make the
//     table alternate between two partially-overlapping truths.
//
//   - An RR that is up but has not sent EOR contributes nothing. Serving a
//     snapshot that is still arriving would install a partial table and then
//     silently "lose" routes at EOR; empty is honest, partial is not.
//
//   - What an RR sent is kept even when it cannot be installed. Each session
//     holds the routes exactly as received (adj-RIBs-in) and derives the table
//     from the local tunnel and VRF tables, so a tunnel or a VPN added later
//     deploys the routes that are already known -- without asking the RR again.
type Client struct {
	cfg      ClientConfig
	hold     time.Duration
	vrfs     map[uint8]*localVRF
	vrfIDs   []uint8            // vrfs 的升序：Targets 要有稳定顺序（map 迭代是随机的）
	exports  map[uint8][]string // vrf -> canonical export RTs, stamped on local routes
	base     map[localKey]LocalPrefix
	dynamic  map[localKey]LocalPrefix
	local    []Route     // effective set, rebuilt from base ∪ dynamic + the VRF table
	skipped  []LocalSkip // the same inputs that could not become routes
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

// localKey identifies one announced prefix. It is not a RouteKey: the device id
// is always ours, and the RTs are derived from the VRF, so the pair (vrf,
// prefix) is the whole identity of an input. Keeping the inputs instead of the
// built routes is what lets a VRF change re-stamp every local route and withdraw
// the ones whose VRF is gone.
type localKey struct {
	vrfID  uint8
	prefix string
}

func (lp LocalPrefix) key() localKey { return localKey{vrfID: lp.VRFID, prefix: lp.Prefix} }

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
	reconnects uint64
	lastErr    string

	// adjIn is what this RR told us, one copy per route key, exactly as
	// received. rib and pending are derived from it plus the local tunnel and
	// VRF tables and are never written anywhere else -- so SetTunnels/SetVRFs can
	// rebuild them and nothing needs to be re-requested.
	adjIn   map[RouteKey]Route
	rib     map[RouteKey]RIBEntry
	pending map[RouteKey]PendingRoute

	// sc is the serialized writer owned by serveSession. ready is set once the
	// session's snapshot has been written; only then may local-prefix and tunnel
	// updates be streamed to it. announced/announcedTunnels are what this session
	// has been told, so a change that lands mid-snapshot is still reconciled.
	sc               *sessionConn
	ready            bool
	announced        map[RouteKey]Route
	announcedTunnels []Tunnel
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
		base:     map[localKey]LocalPrefix{},
		dynamic:  map[localKey]LocalPrefix{},
		sessions: make([]clientSession, len(cfg.RRs)),
		active:   -1,
	}
	vrfs, ids, exports, err := buildVRFs(cfg.VRFs)
	if err != nil {
		return nil, err
	}
	c.vrfs, c.vrfIDs, c.exports = vrfs, ids, exports

	tunnels, nextHops, err := normalizeTunnels(cfg.DeviceID, cfg.Tunnels)
	if err != nil {
		return nil, err
	}
	c.tunnels, c.nextHops = tunnels, nextHops

	for _, lp := range cfg.LocalPrefixes {
		if _, err := c.localRoute(lp); err != nil {
			return nil, err
		}
		c.base[lp.key()] = lp
	}
	c.rebuildLocalLocked()

	for i := range c.sessions {
		c.sessions[i] = clientSession{
			addr:    cfg.RRs[i].Addr,
			adjIn:   map[RouteKey]Route{},
			rib:     map[RouteKey]RIBEntry{},
			pending: map[RouteKey]PendingRoute{},
		}
	}
	return c, nil
}

// buildVRFs validates a VRF table and returns the three views the client needs:
// the import sets, the ascending ids, and the canonical export RTs. It builds
// new maps instead of touching a running client, so a table the operator typed
// wrong is rejected without half-applying.
func buildVRFs(vs []VRFConfig) (map[uint8]*localVRF, []uint8, map[uint8][]string, error) {
	vrfs := make(map[uint8]*localVRF, len(vs))
	exports := make(map[uint8][]string, len(vs))
	ids := make([]uint8, 0, len(vs))
	for _, v := range vs {
		if _, dup := vrfs[v.ID]; dup {
			return nil, nil, nil, fmt.Errorf("route client: vrf %d configured twice", v.ID)
		}
		lv := &localVRF{imports: map[string]bool{}}
		exp := map[string]bool{}
		for _, rt := range v.ExportRTs {
			canon, err := CanonicalRT(rt)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("route client: vrf %d: %w", v.ID, err)
			}
			exp[canon] = true
		}
		for _, rt := range v.ImportRTs {
			canon, err := CanonicalRT(rt)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("route client: vrf %d: %w", v.ID, err)
			}
			lv.imports[canon] = true
		}
		vrfs[v.ID] = lv
		exports[v.ID] = sortedKeys(exp)
		ids = append(ids, v.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return vrfs, ids, exports, nil
}

// normalizeTunnels canonicalizes a tunnel table and builds the peer -> next hops
// index that resolves a received route's origin into local paths.
func normalizeTunnels(deviceID uint16, in []Tunnel) ([]Tunnel, map[uint16][]string, error) {
	for _, t := range in {
		if t.Peer == deviceID {
			return nil, nil, fmt.Errorf("route client: tunnel peer %d is this device", deviceID)
		}
	}
	tunnels := SortedTunnels(in)
	if err := ValidateTunnels(tunnels); err != nil {
		return nil, nil, fmt.Errorf("route client: tunnels: %w", err)
	}
	tunnels = copyTunnels(tunnels)
	nextHops := make(map[uint16][]string, len(tunnels))
	for _, t := range tunnels {
		nextHops[t.Peer] = append(nextHops[t.Peer], t.NextHop)
	}
	return tunnels, nextHops, nil
}

// copyTunnels returns a non-nil copy of a tunnel table. The non-nil part is
// load-bearing: &nil is a pointer, so json.Marshal writes "tunnels":null for it,
// and the RR rejects a tunnels frame that carries no table at all. "This device
// has no tunnels" has to travel as [], or clearing the last tunnel would drop
// the session instead of withdrawing the routes.
func copyTunnels(in []Tunnel) []Tunnel {
	out := make([]Tunnel, 0, len(in))
	return append(out, in...)
}

// LocalRoutes returns the routes this CPE announces (one per VRF × prefix, no
// next hops): the static configuration plus whatever SetLocalPrefixes last
// installed, stamped with the export RTs of the VRF table as it stands. A prefix
// whose VRF is gone is not here -- see LocalSkips for why.
func (c *Client) LocalRoutes() []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Route(nil), c.local...)
}

// localRoute builds a local route from a (vrf, prefix) pair. Export RTs come
// from the VRF config, so a local prefix can never be announced without the
// policy that makes it importable somewhere.
//
// It reads the VRF table, so callers other than NewClient must hold c.mu.
func (c *Client) localRoute(lp LocalPrefix) (Route, error) {
	rt, reason := c.newLocalRoute(lp)
	switch reason {
	case "":
		return rt, nil
	case SkipVRFUnconfigured:
		return Route{}, fmt.Errorf("route client: local prefix %s references unconfigured vrf %d", lp.Prefix, lp.VRFID)
	case SkipNoExportRTs:
		return Route{}, fmt.Errorf("route client: vrf %d has local prefix %s but no export RTs (nothing could import it)",
			lp.VRFID, lp.Prefix)
	}
	return Route{}, fmt.Errorf("route client: local prefix %s: %s", lp.Prefix, reason)
}

// newLocalRoute is localRoute with the answer in gate names instead of errors:
// after a VRF change a prefix that cannot be announced is a fact to report, not
// a request to fail.
func (c *Client) newLocalRoute(lp LocalPrefix) (Route, string) {
	if c.vrfs[lp.VRFID] == nil {
		return Route{}, SkipVRFUnconfigured
	}
	rts := c.exports[lp.VRFID]
	if len(rts) == 0 {
		return Route{}, SkipNoExportRTs
	}
	rt := Route{
		DeviceID: c.cfg.DeviceID,
		VRFID:    lp.VRFID,
		Prefix:   lp.Prefix,
		RTs:      rts,
	}
	if err := rt.Validate(); err != nil {
		return Route{}, err.Error()
	}
	return rt, ""
}

// rebuildLocalLocked recomputes the announced set from the prefix inputs and the
// VRF table as it stands. SetVRFs calls it because the RTs are stamped from the
// VRF: change them and every route of that VRF has to be re-announced, delete
// the VRF and its prefixes have to be withdrawn. A prefix that no longer becomes
// a route is kept in skipped -- the console says why it stopped being advertised
// instead of the table just getting shorter.
//
// Must be called with c.mu held.
func (c *Client) rebuildLocalLocked() {
	m := make(map[localKey]LocalPrefix, len(c.base)+len(c.dynamic))
	for k, lp := range c.base {
		m[k] = lp
	}
	// A dynamic prefix identical to a static one collapses to one route: the
	// same (device, vrf, prefix) must be announced once, or a withdraw of one
	// copy would take the other with it.
	for k, lp := range c.dynamic {
		m[k] = lp
	}
	routes := make([]Route, 0, len(m))
	var skipped []LocalSkip
	for _, lp := range m {
		rt, reason := c.newLocalRoute(lp)
		if reason != "" {
			skipped = append(skipped, LocalSkip{VRFID: lp.VRFID, Prefix: lp.Prefix, Reason: reason})
			continue
		}
		routes = append(routes, rt)
	}
	c.local = SortedRoutes(routes)
	sort.Slice(skipped, func(i, j int) bool {
		if skipped[i].VRFID != skipped[j].VRFID {
			return skipped[i].VRFID < skipped[j].VRFID
		}
		return skipped[i].Prefix < skipped[j].Prefix
	})
	c.skipped = skipped
}

// SetLocalPrefixes replaces the dynamic local set (customer prefixes learned
// from the in-box FRR). The static local_prefixes from the config are always
// kept. Every live session receives the difference immediately; a session
// still streaming its snapshot reconciles against its own announced set once
// the snapshot is done.
func (c *Client) SetLocalPrefixes(lps []LocalPrefix) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[localKey]LocalPrefix, len(lps))
	for _, lp := range lps {
		if _, err := c.localRoute(lp); err != nil {
			return err
		}
		next[lp.key()] = lp
	}
	if equalLocalSets(c.dynamic, next) {
		return nil
	}
	c.dynamic = next
	c.rebuildLocalLocked()
	c.pushLocalLocked()
	return nil
}

// SetVRFs replaces the local VRF table at runtime -- the "新 VPN 后补" half of
// O5. It is all-or-nothing: a table that fails validation changes nothing,
// because a half-applied import policy would decide which prefixes get installed
// and the operator would have no way to see which version was in force.
//
// Two derived things follow from it. Received routes are re-run through the new
// import sets, so a VRF added now picks up the prefixes already in adj-RIBs-in
// without asking the RR again. Local prefixes are re-stamped, and withdrawn if
// their VRF is gone: a route left announced with the RTs of a VRF that no longer
// exists would be imported by a policy nobody configured.
func (c *Client) SetVRFs(vs []VRFConfig) error {
	vrfs, ids, exports, err := buildVRFs(vs)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vrfs, c.vrfIDs, c.exports = vrfs, ids, exports
	c.rebuildLocalLocked()
	c.pushLocalLocked()
	for i := range c.sessions {
		c.deriveSessionLocked(&c.sessions[i])
	}
	return nil
}

// SetTunnels replaces this device's tunnel table at runtime. The table is what
// turns "this route came from device N" into local paths, so a prefix that was
// held back for want of a path is installed by this call alone -- and a prefix
// whose path was removed leaves the table again. Each live session is told the
// new table (MsgTunnels is a full replacement), which makes the RR reflect or
// withdraw on its side too.
func (c *Client) SetTunnels(ts []Tunnel) error {
	tunnels, nextHops, err := normalizeTunnels(c.cfg.DeviceID, ts)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if equalTunnels(c.tunnels, tunnels) {
		return nil
	}
	c.tunnels, c.nextHops = tunnels, nextHops
	for i := range c.sessions {
		s := &c.sessions[i]
		c.deriveSessionLocked(s)
		c.pushTunnelsLocked(s)
	}
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

func equalLocalSets(a, b map[localKey]LocalPrefix) bool {
	if len(a) != len(b) {
		return false
	}
	for k, la := range a {
		if lb, ok := b[k]; !ok || la != lb {
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

func equalTunnels(a, b []Tunnel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Tunnels returns the table this device announces, in canonical order. The copy
// is never nil: see copyTunnels.
func (c *Client) Tunnels() []Tunnel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyTunnels(c.tunnels)
}

// VRFs returns the local VRF table in force, sorted by id. RTs come back
// canonicalised and sorted, so a reader sees the same strings the routes carry
// rather than the order they were configured in -- and an empty list is empty,
// never nil, because a console has to be able to tell "no import RTs" apart
// from "not reported".
func (c *Client) VRFs() []VRFConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]VRFConfig, 0, len(c.vrfIDs))
	for _, id := range c.vrfIDs {
		out = append(out, VRFConfig{
			ID:        id,
			ExportRTs: append([]string(nil), c.exports[id]...),
			ImportRTs: sortedKeys(c.vrfs[id].imports),
		})
	}
	return out
}

// Pending returns the routes the active RR sent that are not installed, plus the
// gate each one is behind. This is the other half of "keep what you were told":
// a held-back prefix has to be reportable, or a table that is missing routes is
// indistinguishable from one that never received them.
//
// The slice is never nil, same reason as copyTunnels: these lists reach the
// console as JSON, and "[]" says "nothing held back" while "null" would only say
// "this build does not report it".
func (c *Client) Pending() []PendingRoute {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingOf(c.active)
}

func (c *Client) pendingOf(idx int) []PendingRoute {
	out := make([]PendingRoute, 0)
	if idx < 0 {
		return out
	}
	for _, p := range c.sessions[idx].pending {
		out = append(out, p)
	}
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

// LocalSkips returns the local prefixes that the current VRF table cannot turn
// into an announcement, sorted. Never nil: see Pending.
func (c *Client) LocalSkips() []LocalSkip {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LocalSkip{}, c.skipped...)
}

// classifyLocked decides where one received route belongs: the table entry it
// installs as, or the gate that keeps it in adj-RIBs-in only.
//
// Must be called with c.mu held.
func (c *Client) classifyLocked(rt Route) (RIBEntry, string) {
	// RT 匹配决定的是**这条路由装进本机哪几张表**，与来源 VPN 无关
	// （§4.3 第 1 步）。来源 VPN 只决定标签：对端弹栈后查的是它自己那张表
	// （§2.4）。所以这里不能看 rt.VRFID 在本机存不存在 —— 导通要解决的正是
	// "本机没有的那个 VPN 的路由落进本机有的 VPN 表"，旧代码那道
	// `c.vrfs[rt.VRFID] == nil` 的闸会把所有跨 VPN 的导通在 RT 匹配之前就丢掉。
	targets := c.importTargets(rt.RTs)
	if len(targets) == 0 {
		return RIBEntry{}, PendingRTMismatch
	}
	// The route is only installed if this device has a tunnel to its origin.
	// Without one there is no path to point the FIB at -- but the route itself
	// stays, because the tunnel may arrive before the next RR message does.
	nh := c.nextHops[rt.DeviceID]
	if len(nh) == 0 {
		return RIBEntry{}, PendingNoPath
	}
	return RIBEntry{Route: rt, Targets: targets, NextHops: append([]string(nil), nh...)}, ""
}

// deriveSessionLocked rebuilds one session's table from its adj-RIBs-in and the
// current tunnel and VRF tables. It never touches the counters: those record
// what each received message did, and a route that moves between installed and
// held back because the local config changed must not rewrite that history.
//
// Must be called with c.mu held.
func (c *Client) deriveSessionLocked(s *clientSession) {
	rib := make(map[RouteKey]RIBEntry, len(s.adjIn))
	pending := make(map[RouteKey]PendingRoute)
	for k, rt := range s.adjIn {
		entry, reason := c.classifyLocked(rt)
		if reason != "" {
			pending[k] = PendingRoute{Route: rt, Reason: reason}
			continue
		}
		rib[k] = entry
	}
	s.rib, s.pending = rib, pending
}

// pushTunnelsLocked re-sends the whole table to one session that already has an
// established snapshot. MsgTunnels is a replacement, not a delta, so there is
// nothing to diff on the wire; the comparison is only so a republished table
// does not make every RR rebuild its adjacency for no reason. A write failure
// closes the session: reconnect re-announces everything.
//
// Must be called with c.mu held.
func (c *Client) pushTunnelsLocked(s *clientSession) {
	if !s.ready || s.sc == nil || equalTunnels(s.announcedTunnels, c.tunnels) {
		return
	}
	table := copyTunnels(c.tunnels)
	if err := s.sc.write(Envelope{Type: MsgTunnels, Tunnels: &table}); err != nil {
		s.sc.close()
		return
	}
	s.announcedTunnels = table
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

	// From here on the session is part of the live local-prefix and tunnel
	// fanout. The snapshot may already be stale: reconcile against the current
	// sets before releasing the lock, so no update can slip between snapshot and
	// fanout.
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
	s.announcedTunnels = tunnels
	c.pushTunnelsLocked(s)
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
	// A reconnect is a fresh snapshot: everything held from the previous session
	// is gone, and the RR will send it again. Keeping the old copy would make a
	// withdrawn route survive a flap.
	s.adjIn = map[RouteKey]Route{}
	s.rib = map[RouteKey]RIBEntry{}
	s.pending = map[RouteKey]PendingRoute{}
	s.rrID = rrID
	s.reconnects++
	s.lastErr = ""
	s.sc = nil
	s.ready = false
	s.announced = nil
	s.announcedTunnels = nil
}

func (c *Client) markDown(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sessions[idx]
	s.up = false
	s.synced = false
	s.adjIn = map[RouteKey]Route{}
	s.rib = map[RouteKey]RIBEntry{}
	s.pending = map[RouteKey]PendingRoute{}
	s.conn = nil
	s.sc = nil
	s.ready = false
	s.announced = nil
	s.announcedTunnels = nil
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
		s.adjIn[k] = rt
		// Counters are event-shaped: they say what processing this frame did, not
		// what the table looks like now. A prefix that moves between installed and
		// held back every time a tunnel changes would otherwise rewrite the history
		// the console reports.
		delete(s.rib, k)
		delete(s.pending, k)
		entry, reason := c.classifyLocked(rt)
		if reason != "" {
			s.pending[k] = PendingRoute{Route: rt, Reason: reason}
			switch reason {
			case PendingRTMismatch:
				c.counters.rtMismatch++
			case PendingNoPath:
				c.counters.noPath++
			}
			continue
		}
		s.rib[k] = entry
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
		if _, ok := s.adjIn[k]; !ok {
			continue
		}
		delete(s.adjIn, k)
		// withdrawn counts routes that actually left the table. Withdrawing one
		// that was only ever held back changes nothing a console could act on.
		if _, installed := s.rib[k]; installed {
			c.counters.withdrawn++
		}
		delete(s.rib, k)
		delete(s.pending, k)
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
		st.ActivePending = len(c.sessions[c.active].pending)
	}
	for i := range c.sessions {
		s := &c.sessions[i]
		st.Sessions = append(st.Sessions, SessionStats{
			Addr:       s.addr,
			RRID:       s.rrID,
			Connected:  s.up,
			Synced:     s.synced,
			Routes:     len(s.rib),
			Pending:    len(s.pending),
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
