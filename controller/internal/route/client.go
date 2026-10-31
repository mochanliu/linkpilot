package route

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
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

// RRConfig is one reflector this CPE peers with. Two are expected; the protocol
// works with any number, and every synced one contributes to the same Loc-RIB
// (there is no "active" slot -- see Client).
//
// ID is a label: it comes from the controller's RR library and travels into
// /status so an operator can point at the row they chose. Sessions are matched
// by Addr, not by ID -- a hand-written route.json (every box today) has
// addresses with no ids, and "运营方把 rr2 的地址改了" arrives here as exactly
// "old address gone, new address appeared".
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
	ConfigID   string `json:"config_id,omitempty"` // RR-library id, when the config named one
	RRID       string `json:"rr_id,omitempty"`     // as reported by hello_ok
	Connected  bool   `json:"connected"`
	Synced     bool   `json:"synced"`
	Retiring   bool   `json:"retiring,omitempty"` // replaced, kept only until a wanted RR takes over
	Routes     int    `json:"routes"`             // in this RR's adj-RIB-in
	Reconnects uint64 `json:"reconnects"`
	LastError  string `json:"last_error,omitempty"`
}

// RRWait is one half of a rolling RR replacement that has not finished: the RR
// addresses this device wants but cannot establish yet, and the retired session
// kept alive because of them. It is reported as data, not as a sentence, because
// sr-apply's push has to be able to tell "换完了" from "还在换" without parsing
// Chinese. Nil whenever no replacement is in flight.
type RRWait struct {
	PendingAddrs  []string `json:"pending_addrs"`
	RetiringAddrs []string `json:"retiring_addrs"`
}

// ClientStats is the whole instance's state. The drop/import counters are
// cumulative and event-shaped: they count what each received message did, so a
// route that the local tunnel or VRF table later moves between installed and
// held back does not change the numbers after the fact. RibRoutes is live.
//
// There is no "active RR" any more: every session writes into one Loc-RIB, so
// the table is not "the one that RR sent me". RibReady says whether a session
// has reached EOR, which is the only moment the table may be trusted as a
// whole (see Client).
type ClientStats struct {
	DeviceID          uint16         `json:"device_id"`
	RibReady          bool           `json:"rib_ready"`
	RibRoutes         int            `json:"rib_routes"` // installed in a local table now
	Sessions          []SessionStats `json:"sessions"`
	RRWait            *RRWait        `json:"rr_wait,omitempty"`
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
	Targets  TableList `json:"targets"`
	NextHops []string  `json:"next_hops"`
}

// TableList is the set of local VRF ids a route was imported into.
//
// It needs its own marshaller for one reason only: to encoding/json a []uint8
// IS a []byte, so table 50 would be published in /status as the base64 string
// "Mg==". Nothing in Go notices -- the unit tests compared the decoded value --
// until test-refresh.sh read the real endpoint and found a table number nobody
// can read. Empty marshals as [] rather than null, same reason as the tunnel
// table: a list that is absent and a list that is empty must not be the same
// token in a document people debug by eye.
type TableList []uint8

func (t TableList) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, len(t)*4+2)
	buf = append(buf, '[')
	for i, id := range t {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendUint(buf, uint64(id), 10)
	}
	return append(buf, ']'), nil
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
//   - Every session writes into one Loc-RIB (loc), exactly as BGP does: the
//     adj-RIBs-in of all peers merge into one table, and each prefix picks one
//     best path from the copies all sessions sent. Two RRs carrying the same
//     route are two copies of one path, so losing one session never shrinks the
//     table -- the other copy takes over.
//
//   - An RR that is up but has not sent EOR contributes nothing. Serving a
//     snapshot that is still arriving would install a partial table and then
//     silently "lose" routes at EOR; empty is honest, partial is not.
//
//   - What an RR sent is kept even when it cannot be installed. Each session
//     holds the routes exactly as received (adj-RIBs-in); the tunnel table
//     decides whether a path's next hop resolves (a flag on the path, GoBGP's
//     IsNexthopInvalid), and the VRF table decides which local table can see it
//     (GoBGP's CanImportToVrf). Both are consulted when reading and when the
//     input changes, so a tunnel or a VPN added later deploys routes that are
//     already known -- without asking the RR again.
//
// The set of RRs is not static (C-c): SetRRs adds and retires sessions while the
// process runs, which is why a session is a pointer and the slice is reorderable.
// The rules that make a rolling RR replacement safe are in SetRRs.
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
	sessions []*clientSession // wanted ones first, in configured order, then the retired ones
	loc      *locRib
	counters counters

	// runCtx is set by Run and is the parent of every session goroutine's ctx, so
	// SetRRs can start a session after boot and still have it stop on shutdown.
	runCtx context.Context
	wg     sync.WaitGroup

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
	cfgID      string // RR-library id, a label only -- see RRConfig
	addr       string
	conn       net.Conn
	up         bool
	synced     bool
	rrID       string // as reported by hello_ok, not the config label above
	reconnects uint64
	lastErr    string

	// retire is set by SetRRs when the current RR set no longer names this
	// address. A retired session is not dropped immediately: the replacement has
	// to establish first (see SetRRs). Once it leaves the session list its
	// goroutine is still finishing, so every shared-state update re-checks
	// membership instead of trusting this flag.
	retire bool

	// cancel ends this session's reconnect loop. It belongs to the goroutine that
	// dials addr, so dropping a retired session also stops the redial that would
	// otherwise bring the RR back a second later.
	cancel context.CancelFunc

	// adjIn is what this RR told us, one copy per route key, exactly as
	// received -- the adj-RIB-in. The visible table is the shared Loc-RIB (see
	// Client.loc), never a per-session derivation: that is what keeps one
	// session's copy from disappearing when another session dies.
	adjIn map[RouteKey]Route

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
	if err := validateRRs(cfg.RRs); err != nil {
		return nil, err
	}
	hold, err := HoldDuration(cfg.HoldSec)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cfg:     cfg,
		hold:    hold,
		base:    map[localKey]LocalPrefix{},
		dynamic: map[localKey]LocalPrefix{},
		loc:     newLocRib(),
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

	for _, rr := range cfg.RRs {
		c.sessions = append(c.sessions, newSession(rr))
	}
	return c, nil
}

// validateRRs is the one gate both entry points share: a client that starts with
// a broken RR list and one that is handed a new one at runtime must refuse for
// exactly the same reason. An empty list is refused because sr-route itself
// refuses to start without a reflector -- accepting it here would let a PUT
// take the device off the network and a restart would then fail outright.
func validateRRs(rrs []RRConfig) error {
	if len(rrs) == 0 {
		return errors.New("route client: at least one RR is required")
	}
	seen := make(map[string]bool, len(rrs))
	for i, rr := range rrs {
		if rr.Addr == "" {
			return fmt.Errorf("route client: rr[%d] has no address", i)
		}
		if seen[rr.Addr] {
			return fmt.Errorf("route client: rr %s is configured twice (a second session to the same reflector is not a standby)", rr.Addr)
		}
		seen[rr.Addr] = true
	}
	return nil
}

// newSession builds a session that has never connected: its adj-RIB-in starts
// empty so a reader sees "nothing yet", not "nothing ever".
func newSession(rr RRConfig) *clientSession {
	return &clientSession{
		cfgID: rr.ID,
		addr:  rr.Addr,
		adjIn: map[RouteKey]Route{},
	}
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
	// Nothing to re-derive on the received side: import RTs are consulted when
	// the table is read (GoBGP's CanImportToVrf + Table.Select), so a VRF added
	// now sees the paths already in the Loc-RIB without any rewrite.
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
	before := c.nextHops
	c.tunnels, c.nextHops = tunnels, nextHops
	for _, s := range c.sessions {
		c.pushTunnelsLocked(s)
	}
	// Nexthop Tracking, the same shape as GoBGP (zclient applyToPathList ->
	// updatePath -> globalRib.Update): only the origins whose next hops changed
	// re-enter the Loc-RIB, each path replaced by a copy with the new flag. No
	// session is touched and nothing is re-requested from the RR.
	for id := range changedOrigins(before, nextHops) {
		for _, p := range c.loc.originPaths(id) {
			c.loc.process(p.route, p.source, len(nextHops[id]) > 0)
		}
	}
	return nil
}

// changedOrigins names the origin devices whose next-hop set differs between
// two tunnel tables. An origin with no tunnels either side has no paths, so
// leaving it out costs nothing.
func changedOrigins(before, after map[uint16][]string) map[uint16]struct{} {
	changed := make(map[uint16]struct{}, len(before)+len(after))
	for id, nh := range before {
		if !equalStrings(nh, after[id]) {
			changed[id] = struct{}{}
		}
	}
	for id, nh := range after {
		if !equalStrings(nh, before[id]) {
			changed[id] = struct{}{}
		}
	}
	return changed
}

func equalStrings(a, b []string) bool {
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

// SetRRs replaces the reflector set while the process runs -- the seam C-c exists
// for, so 逐台换 RR never means restarting sr-route.
//
// make-before-break, in three rules:
//
//   - Nothing is dropped first. An address the new set no longer names is marked
//     retire and keeps its session: the goroutine is still dialling, its
//     adj-RIBs-in is still the table, and the reflector on the other end still
//     holds our prefixes.
//   - A retired session goes only once a session this device *wants* has taken
//     over (establishedLocked). Anything weaker leaves the visible table empty
//     for a moment, which makes every peer withdraw our prefixes -- the exact
//     outage the switch is supposed to avoid, and the reason a plain restart
//     could not be used here.
//   - If the new RR never comes up, the old session is kept indefinitely and the
//     state says so (Stats.RRWait). "换 RR 失败" is something to report, not a
//     licence to black-hole the tenant.
//
// Sessions are matched by address and the list is rebuilt with the wanted ones
// first, in the configured order. That order is presentation only now: the
// visible table is the Loc-RIB, and set position no longer decides which copy of
// a prefix wins (the session address does, see comparePaths).
func (c *Client) SetRRs(next []RRConfig) error {
	if err := validateRRs(next); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	byAddr := make(map[string]*clientSession, len(c.sessions))
	for _, s := range c.sessions {
		byAddr[s.addr] = s
	}
	wantedAddrs := make(map[string]bool, len(next))
	wanted := make([]*clientSession, 0, len(next))
	for _, rr := range next {
		wantedAddrs[rr.Addr] = true
		s := byAddr[rr.Addr]
		if s == nil {
			s = newSession(rr)
		} else {
			// Reused: the label may have changed even when the address did not,
			// and a session that is wanted again stops being on its way out.
			s.cfgID, s.retire = rr.ID, false
		}
		wanted = append(wanted, s)
	}
	var retiring []*clientSession
	for _, s := range c.sessions {
		if wantedAddrs[s.addr] {
			continue
		}
		// Marked, not dropped: make-before-break means this session keeps
		// feeding the Loc-RIB until a wanted one has taken over, so it stays in
		// c.sessions (after the wanted ones) and leaves only in
		// reapRetiringLocked.
		s.retire = true
		retiring = append(retiring, s)
	}
	c.sessions = append(wanted, retiring...)
	for _, s := range c.sessions {
		c.startLocked(s)
	}
	c.reapRetiringLocked()
	return nil
}

// RRs returns the reflector set this device is working towards, in the order it
// was configured. A retiring session is connected but on its way out, so it shows
// up in Stats().Sessions and not here.
func (c *Client) RRs() []RRConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RRConfig, 0, len(c.sessions))
	for _, s := range c.sessions {
		if s.retire {
			continue
		}
		out = append(out, RRConfig{ID: s.cfgID, Addr: s.addr})
	}
	return out
}

// establishedLocked is the takeover criterion: connected, EOR'd, and done writing
// our own snapshot to the RR. Synced without ready would mean the RR finished its
// snapshot while ours was still in flight, so it does not know all of our prefixes
// yet -- dropping the old session then would break the return path.
//
// Must be called with c.mu held.
func (c *Client) establishedLocked(s *clientSession) bool {
	return s != nil && !s.retire && s.up && s.synced && s.ready
}

// reapRetiringLocked drops the sessions the wanted set no longer names, but only
// after one of the wanted sessions has taken over. Called from SetRRs (the wanted
// session may already be established, e.g. when only the standby slot changed) and
// from markSynced (the instant one becomes established).
//
// Must be called with c.mu held.
func (c *Client) reapRetiringLocked() {
	var dropped []*clientSession
	kept := make([]*clientSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		if s.retire {
			dropped = append(dropped, s)
			continue
		}
		kept = append(kept, s)
	}
	if len(dropped) == 0 {
		return
	}
	if !c.anyEstablishedLocked() {
		// 新 RR 还没成立：旧的留着，宁可看起来"配置未完全生效"，也不要把这台盒子
		// 从反射器上摘下来。
		return
	}
	c.sessions = kept
	for _, s := range dropped {
		// GoBGP's dropAdjRIBIn: the session's copies leave the Loc-RIB by
		// source, one route at a time. A prefix another session also carries
		// simply keeps that copy, so the table never dips on the way out.
		c.dropSessionLocked(s)
		if s.cancel != nil {
			s.cancel()
		}
		if s.sc != nil {
			s.sc.close()
		} else if s.conn != nil {
			s.conn.Close()
		}
		c.logf("route: 新 RR 已成立，摘掉被替换的会话 %s（device %d）", s.addr, c.cfg.DeviceID)
	}
}

// anyEstablishedLocked reports whether a session this device wants has taken over.
//
// Must be called with c.mu held.
func (c *Client) anyEstablishedLocked() bool {
	for _, s := range c.sessions {
		if c.establishedLocked(s) {
			return true
		}
	}
	return false
}

// pushLocalLocked streams local-set changes to every session that has finished
// its snapshot. Must be called with c.mu held.
func (c *Client) pushLocalLocked() {
	desired := make(map[RouteKey]Route, len(c.local))
	for _, r := range c.local {
		desired[r.Key()] = r
	}
	for _, s := range c.sessions {
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

// Pending returns the prefixes the Loc-RIB knows and no local table installs,
// plus the gate each one is behind. This is the other half of "keep what you
// were told": a held-back prefix has to be reportable, or a table that is
// missing routes is indistinguishable from one that never received them.
//
// One row per prefix, not per session copy: the gates are properties of the
// merged table (GoBGP applies CanImportToVrf to the destination's paths and
// reads the head), so two RRs carrying the same prefix must not list it twice.
//
// The slice is never nil, same reason as copyTunnels: these lists reach the
// console as JSON, and "[]" says "nothing held back" while "null" would only say
// "this build does not report it".
func (c *Client) Pending() []PendingRoute {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PendingRoute, 0)
	c.loc.eachDest(func(d *destination) {
		p := c.importablePathLocked(d)
		if p == nil {
			// GoBGP CanImportToVrf is false for every copy: no table here cares.
			if len(d.paths) > 0 {
				out = append(out, PendingRoute{Route: d.paths[0].route, Reason: PendingRTMismatch})
			}
			return
		}
		if !p.resolvable {
			out = append(out, PendingRoute{Route: p.route, Reason: PendingNoPath})
		}
	})
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

// admitLocked applies the two intake gates to one received route: is any local
// table importing its RTs, and does a tunnel reach its origin. It answers for
// the counters only -- what the table shows is decided when it is read, because
// both gates are reversible (add a VRF, add a tunnel) and neither rewrites the
// received RIB.
//
// It is NOT the source of path.resolvable: that flag is the tunnel gate alone
// (GoBGP's IsNexthopInvalid), while RT import is a read-time question
// (CanImportToVrf). Folding them together would make a VPN added later unable to
// pick up routes that are already here.
//
// Must be called with c.mu held.
func (c *Client) admitLocked(rt Route) (reachable bool, reason string) {
	// RT 匹配决定的是**这条路由装进本机哪几张表**，与来源 VPN 无关
	// （§4.3 第 1 步）。来源 VPN 只决定标签：对端弹栈后查的是它自己那张表
	// （§2.4）。所以这里不能看 rt.VRFID 在本机存不存在 —— 导通要解决的正是
	// "本机没有的那个 VPN 的路由落进本机有的 VPN 表"，旧代码那道
	// `c.vrfs[rt.VRFID] == nil` 的闸会把所有跨 VPN 的导通在 RT 匹配之前就丢掉。
	if len(c.importTargets(rt.RTs)) == 0 {
		return false, PendingRTMismatch
	}
	// Without a tunnel to the origin there is no path to point the FIB at. The
	// path stays in the Loc-RIB as unresolvable, so the tunnel may arrive before
	// the next RR message does.
	if !c.reachableLocked(rt) {
		return false, PendingNoPath
	}
	return true, ""
}

// reachableLocked is the tunnel gate: does this device have any tunnel to the
// route's origin right now. This is the only thing path.resolvable is set from.
//
// Must be called with c.mu held.
func (c *Client) reachableLocked(rt Route) bool {
	return len(c.nextHops[rt.DeviceID]) > 0
}

// importablePathLocked is GoBGP's per-VRF view at the global level: the first
// copy in best-path order whose RTs any local table imports. nil when no copy
// can be imported at all.
//
// Must be called with c.mu held.
func (c *Client) importablePathLocked(d *destination) *path {
	if d == nil {
		return nil
	}
	for _, p := range d.paths {
		if len(c.importTargets(p.route.RTs)) > 0 {
			return p
		}
	}
	return nil
}

// installedLocked reports whether this prefix currently reaches a local table.
//
// Must be called with c.mu held.
func (c *Client) installedLocked(k RouteKey) bool {
	p := c.importablePathLocked(c.loc.dsts[k])
	return p != nil && p.resolvable
}

// entryLocked is the read-time shape of one path: local next hops belong to the
// origin's tunnel set right now, not to the announce that was received earlier.
//
// Must be called with c.mu held.
func (c *Client) entryLocked(p *path) RIBEntry {
	return RIBEntry{
		Route:    p.route,
		Targets:  c.importTargets(p.route.RTs),
		NextHops: append([]string(nil), c.nextHops[p.route.DeviceID]...),
	}
}

// dropSessionLocked removes one session's copies from the Loc-RIB, by source,
// without touching any other session's copy of the same prefixes. This is
// GoBGP's dropAdjRIBIn: a session going away withdraws exactly what that session
// had told us.
//
// Must be called with c.mu held.
func (c *Client) dropSessionLocked(s *clientSession) {
	for k := range s.adjIn {
		c.loc.withdraw(k, s.addr)
	}
	s.adjIn = map[RouteKey]Route{}
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

// Run keeps the RR sessions alive until ctx is cancelled. It returns nil.
//
// The reconnect loop belongs to a session, not to a slot in the configured list:
// SetRRs can hand this client a new reflector after boot, and that session has to
// be dialled by the same loop and stop on the same ctx.
func (c *Client) Run(ctx context.Context) error {
	var started bool
	c.runOnce.Do(func() { started = true })
	if !started {
		return errors.New("route client: Run may only be called once")
	}
	c.mu.Lock()
	c.runCtx = ctx
	for _, s := range c.sessions {
		c.startLocked(s)
	}
	c.mu.Unlock()

	<-ctx.Done()
	c.mu.Lock()
	for _, s := range c.sessions {
		if s.conn != nil {
			s.conn.Close()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

// startLocked gives one session its reconnect loop. Sessions created before Run
// are started by Run; ones created after it start here, so a PUT /rrs does not
// have to wait for a restart. Never call it twice for the same session -- the
// second goroutine would dial the same address while the first still owns s.conn.
func (c *Client) startLocked(s *clientSession) {
	if s.cancel != nil || c.runCtx == nil {
		return
	}
	sctx, cancel := context.WithCancel(c.runCtx)
	s.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runRR(sctx, s)
	}()
}

// logf is the client's only output channel, and the reason a note about a
// half-finished RR switch can live in a log line without dragging a logger in.
func (c *Client) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

func (c *Client) runRR(ctx context.Context, s *clientSession) {
	backoff := initialBackoff
	for ctx.Err() == nil {
		conn, err := c.cfg.Dial(ctx, s.addr)
		if err != nil {
			if ctx.Err() == nil {
				c.noteError(s, err)
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		handshook, err := c.serveSession(s, conn)
		if ctx.Err() != nil {
			return
		}
		c.noteError(s, err)
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
func (c *Client) serveSession(s *clientSession, conn net.Conn) (handshook bool, err error) {
	sc := &sessionConn{conn: conn, writeTimeout: c.hold}
	sessionDone := make(chan struct{})
	defer func() {
		close(sessionDone)
		sc.close()
		c.markDown(s)
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

	c.markUp(s, conn, env.RRID, hold)
	c.mu.Lock()
	s.sc = sc
	reaped := c.indexOfLocked(s) < 0
	c.mu.Unlock()
	if reaped {
		// 换 RR 在这条会话拨号的这段时间里已经完成了：新 RR 成立了，这条是来晚的
		// 旧地址。接回去会让一个不该存在的反射器重新进入本机路由面。
		return true, errors.New("session was dropped while it was connecting")
	}

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
	if !s.up || c.indexOfLocked(s) < 0 {
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
			c.markSynced(s)
		case MsgAnnounce:
			if err := c.applyAnnounce(s, env.Routes); err != nil {
				return true, err
			}
		case MsgWithdraw:
			if err := c.applyWithdraw(s, env.Keys); err != nil {
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

func (c *Client) markUp(s *clientSession, conn net.Conn, rrID string, hold time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.conn = conn
	s.up = true
	s.synced = false
	// A reconnect is a fresh snapshot: the paths this session contributed are
	// dropped from the Loc-RIB (GoBGP dropAdjRIBIn before the new session fills
	// adjRibIn) and the RR will send them again. Keeping the old copy would make
	// a withdrawn route survive a flap.
	c.dropSessionLocked(s)
	s.rrID = rrID
	s.reconnects++
	s.lastErr = ""
	s.sc = nil
	s.ready = false
	s.announced = nil
	s.announcedTunnels = nil
}

func (c *Client) markDown(s *clientSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.up = false
	s.synced = false
	c.dropSessionLocked(s)
	s.conn = nil
	s.sc = nil
	s.ready = false
	s.announced = nil
	s.announcedTunnels = nil
}

func (c *Client) markSynced(s *clientSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.indexOfLocked(s) < 0 {
		return
	}
	if !s.up {
		return
	}
	s.synced = true
	// EOR: the snapshot is complete, so this is the one moment its whole
	// adj-RIB-in may enter the Loc-RIB. Routes received from here on go in as
	// they arrive (see applyAnnounce).
	for _, rt := range s.adjIn {
		c.loc.process(rt, s.addr, c.reachableLocked(rt))
	}
	// This is the moment a replacement RR takes over: EOR means its copy of the
	// table is complete, so the session it replaced can finally be dropped.
	c.reapRetiringLocked()
}

func (c *Client) noteError(s *clientSession, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	s.lastErr = err.Error()
	managed := c.indexOfLocked(s) >= 0
	c.mu.Unlock()
	if !managed {
		// 换 RR 摘掉旧会话时是我们自己关的连接，这条"错误"不是运维要看的故障。
		return
	}
	c.logf("route: rr %s (device %d): %v", s.addr, c.cfg.DeviceID, err)
}

// indexOfLocked finds a session in the live list, or -1 once a rolling RR
// replacement has dropped it. The goroutine outlives the drop -- it can be
// mid-dial, mid-snapshot, or blocked in a read -- so every update that touches
// shared state asks this first instead of assuming it is still listed.
//
// Must be called with c.mu held.
func (c *Client) indexOfLocked(s *clientSession) int {
	for i, cur := range c.sessions {
		if cur == s {
			return i
		}
	}
	return -1
}

// applyAnnounce writes what one RR sent into this session's adj-RIB-in and then
// feeds each route into the shared Loc-RIB, where it competes with whatever the
// other sessions said about the same prefix.
//
// Only a session that has reached EOR contributes to the table: until then its
// snapshot is still arriving, and serving a partial one would install routes and
// then "lose" them at EOR. The routes are kept either way (adj-RIB-in), so
// markSynced can hand the whole set to the Loc-RIB in one go.
//
// The resolvable flag is decided at intake for the counters only. What the table
// shows is read through the local tunnel and VRF tables at read time, because
// both gates are reversible and neither rewrites what we were told.
func (c *Client) applyAnnounce(s *clientSession, routes []Route) error {
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
	if !s.up || c.indexOfLocked(s) < 0 {
		return fmt.Errorf("announce on a session that is not up")
	}
	c.counters.received += uint64(len(order))
	for _, k := range order {
		rt := last[k]
		// adj-RIB-in first: it is the record of what this session said, and
		// dropSessionLocked walks it to know which copies to take back out.
		s.adjIn[k] = rt
		// Counters are event-shaped: they say what processing this frame did, not
		// what the table looks like now. A prefix that moves between installed and
		// held back every time a tunnel changes would otherwise rewrite the history
		// the console reports.
		_, reason := c.admitLocked(rt)
		switch reason {
		case PendingRTMismatch:
			c.counters.rtMismatch++
		case PendingNoPath:
			c.counters.noPath++
		default:
			c.counters.imported++
		}
		if s.synced {
			c.loc.process(rt, s.addr, c.reachableLocked(rt))
		}
	}
	return nil
}

// importTargets 返回本机 import 了这些 RT 的 VRF，升序。
// 升序不是整理癖：Targets 会进 /status 的 JSON，也是 sr-fib 每轮重算的输入，
// 顺序不稳定就等于每轮都"看起来变了"。
func (c *Client) importTargets(rts []string) TableList {
	var out TableList
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

func (c *Client) applyWithdraw(s *clientSession, keys []RouteKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !s.up || c.indexOfLocked(s) < 0 {
		return fmt.Errorf("withdraw on a session that is not up")
	}
	for _, k := range keys {
		if _, ok := s.adjIn[k]; !ok {
			continue
		}
		was := c.installedLocked(k)
		delete(s.adjIn, k)
		// Only this session's copy leaves. If the other RR still carries the
		// prefix its copy becomes best and the route stays installed.
		c.loc.withdraw(k, s.addr)
		// withdrawn counts routes that actually left a local table. Withdrawing one
		// that was only ever held back changes nothing a console could act on.
		if was && !c.installedLocked(k) {
			c.counters.withdrawn++
		}
	}
	return nil
}

// RIB returns the routes installed into one local VRF, sorted. 这里的 vrf 是**目标**
// 表：一条来源 VPN 不在这台盒子上的路由，只要 RT 命中了这个 VRF 的 import，就会出
// 现在这里（§4.3 第 1 步）。
//
// 读的是 Loc-RIB 里每个前缀的最优路：所有 RR 会话的同一份拷贝已经在表里合成了一
// 个 destination，所以两条 RR 各送一遍不会让一条前缀出现两次。
func (c *Client) RIB(vrf uint8) []RIBEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []RIBEntry
	c.loc.eachBest(func(_ RouteKey, p *path) {
		if c.importsIntoLocked(p.route.RTs, vrf) {
			out = append(out, c.entryLocked(p))
		}
	})
	return sortedEntries(out)
}

// AllRIB returns every installed route, grouped by **target** VRF —— 也就是 sr-fib
// 要往哪张表装。同一个 RouteKey 可以出现在几张表下面。
func (c *Client) AllRIB() map[uint8][]RIBEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[uint8][]RIBEntry)
	c.loc.eachBest(func(_ RouteKey, p *path) {
		for _, t := range c.importTargets(p.route.RTs) {
			out[t] = append(out[t], c.entryLocked(p))
		}
	})
	for vrf := range out {
		out[vrf] = sortedEntries(out[vrf])
	}
	return out
}

// importsIntoLocked reports whether one local VRF imports any of these RTs.
//
// Must be called with c.mu held.
func (c *Client) importsIntoLocked(rts []string, vrf uint8) bool {
	lv := c.vrfs[vrf]
	if lv == nil {
		return false
	}
	for _, rt := range rts {
		if lv.imports[rt] {
			return true
		}
	}
	return false
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
		RibReady:          c.ribReadyLocked(),
		Received:          c.counters.received,
		Imported:          c.counters.imported,
		Withdrawn:         c.counters.withdrawn,
		DroppedRTMismatch: c.counters.rtMismatch,
		DroppedInvalid:    c.counters.invalid,
		DroppedOwnRoute:   c.counters.ownRoute,
		DroppedNoPath:     c.counters.noPath,
	}
	c.loc.eachDest(func(d *destination) {
		if c.installedLocked(d.key) {
			st.RibRoutes++
		}
	})
	st.RRWait = c.rrWaitLocked()
	for _, s := range c.sessions {
		st.Sessions = append(st.Sessions, SessionStats{
			Addr:       s.addr,
			ConfigID:   s.cfgID,
			RRID:       s.rrID,
			Connected:  s.up,
			Synced:     s.synced,
			Retiring:   s.retire,
			Routes:     len(s.adjIn),
			Reconnects: s.reconnects,
			LastError:  s.lastErr,
		})
	}
	return st
}

// ribReadyLocked reports whether any session has reached EOR, i.e. whether the
// table can be read as a whole rather than as a snapshot still arriving.
//
// Must be called with c.mu held.
func (c *Client) ribReadyLocked() bool {
	for _, s := range c.sessions {
		if s.up && s.synced {
			return true
		}
	}
	return false
}

// rrWaitLocked reports a rolling RR replacement that has not finished: the RR
// addresses this device wants but cannot establish yet, and the retired sessions
// kept alive because of them. nil when nothing is in flight -- a switch that is
// done is not a state, and a switch nobody asked for is not a warning.
//
// Must be called with c.mu held.
func (c *Client) rrWaitLocked() *RRWait {
	pending := make([]string, 0, len(c.sessions))
	retiring := make([]string, 0, len(c.sessions))
	for _, s := range c.sessions {
		if s.retire {
			retiring = append(retiring, s.addr)
			continue
		}
		if !c.establishedLocked(s) {
			pending = append(pending, s.addr)
		}
	}
	if len(retiring) == 0 {
		return nil
	}
	return &RRWait{PendingAddrs: pending, RetiringAddrs: retiring}
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
