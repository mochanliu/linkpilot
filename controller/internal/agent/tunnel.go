package agent

// The device half of tunnel delivery.
//
// The controller renders ONE snapshot per device (wire.UnderlayBundle) and bumps
// a generation on every mutation of the tenant's tunnels. This type pulls that
// snapshot over the device's own certificate and makes the dataplane match it.
// Same arrangement as the config channel in cmd/sr-agent/configsync.go: the
// device reports what it actually runs, the controller never infers it.
//
// Three rules shape the reconcile, all of them forced by what the dataplane does
// when a step is skipped (see dataplane.go's rule list):
//
//  1. A FULL SNAPSHOT, never a delta. A lost fetch, a crashed agent or a VPP
//     restart must all converge to the same state from nothing but the next
//     bundle.
//  2. Tunnels the controller cannot describe (Skipped) keep running. An
//     observation that went stale for a minute is not a reason to tear down a
//     link that still carries traffic.
//  3. Interfaces the agent owns (user_instance >= wire.UnderlayInstanceBase)
//     that the bundle no longer mentions are removed. That is how a deletion
//     made while the agent was down still lands: there is no local state file
//     to compare against, VPP is the record.

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// TunnelSync owns the links and the report about them. It mirrors configSync's
// shape on purpose (Capable/Version/Applying/OK/Error), so the console can show
// both channels the same way.
type TunnelSync struct {
	// dp is guarded by dpMu, NOT by applyMu: a reconcile holds applyMu across
	// network calls, and the report side must be able to ask for the current
	// connection without waiting minutes behind one.
	dpMu sync.Mutex
	dp   DataPlane
	// dial, when set, lets the reconciler re-register after VPP restarts: every
	// api socket dies with the process it belonged to, and a dead client never
	// recovers on its own (see dropDeadConnection).
	dial  func(context.Context) (DataPlane, error)
	id    *Identity
	fetch func(context.Context) (wire.UnderlayBundle, error)
	logf  func(string, ...any)
	now   func() time.Time

	retryMin time.Duration
	retryMax time.Duration

	// applyMu serialises whole reconciles. It is held across network and API
	// calls, so it must never be mu (which only guards the report).
	applyMu sync.Mutex

	mu       sync.Mutex
	links    map[string]*Link
	peerOf   map[string]uint16
	localOf  map[string]netip.Prefix
	skipped  map[string]string
	state    wire.TunnelState
	applying bool
	fails    int
	nextTry  time.Time
}

// NewTunnelSync wires the reconciler. logf may be nil; now defaults to time.Now.
func NewTunnelSync(dp DataPlane, id *Identity,
	fetch func(context.Context) (wire.UnderlayBundle, error),
	logf func(string, ...any)) *TunnelSync {

	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := &TunnelSync{
		dp: dp, id: id, fetch: fetch, logf: logf, now: time.Now,
		retryMin: 5 * time.Second, retryMax: 5 * time.Minute,
		links:   map[string]*Link{},
		peerOf:  map[string]uint16{},
		localOf: map[string]netip.Prefix{},
		skipped: map[string]string{},
		state:   wire.TunnelState{Capable: true},
	}
	return t
}

// SetRedial installs a factory for a fresh dataplane connection. Called by the
// agent binary with vppapi.Dial; tests leave it nil and keep the fake.
func (t *TunnelSync) SetRedial(dial func(context.Context) (DataPlane, error)) {
	t.applyMu.Lock()
	defer t.applyMu.Unlock()
	t.dial = dial
}

// State is a copy of the last report, safe to attach to a heartbeat.
func (t *TunnelSync) State() wire.TunnelState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// DataPlane is the connection currently in use, or nil before the first
// successful dial. The observation report shares it on purpose: when VPP
// restarts, the reconciler redials and the reader's view follows the new
// connection instead of holding a handle to a socket that no longer exists.
func (t *TunnelSync) DataPlane() DataPlane {
	t.dpMu.Lock()
	defer t.dpMu.Unlock()
	return t.dp
}

func (t *TunnelSync) setDataPlane(dp DataPlane) {
	t.dpMu.Lock()
	t.dp = dp
	t.dpMu.Unlock()
}

// Links exposes the installed tunnels for the telemetry/observation loops
// (they need a WG public key per link to attribute what the dataplane saw).
func (t *TunnelSync) Links() map[string]*Link {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]*Link, len(t.links))
	for id, l := range t.links {
		out[id] = l
	}
	return out
}

// The map accessors exist because the reconcile holds applyMu across network
// calls, while Links() only takes mu. Every read or write of t.links/t.skipped
// goes through one of these so the two never race.
func (t *TunnelSync) getLink(id string) *Link {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.links[id]
}

func (t *TunnelSync) setLink(id string, l *Link, peer uint16, local netip.Prefix) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.links[id] = l
	t.peerOf[id] = peer
	t.localOf[id] = local
}

func (t *TunnelSync) delLink(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.links, id)
	delete(t.peerOf, id)
	delete(t.localOf, id)
}

func (t *TunnelSync) takeLinks() map[string]*Link {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]*Link, len(t.links))
	for id, l := range t.links {
		out[id] = l
	}
	return out
}

func (t *TunnelSync) setSkip(id, reason string) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed = t.skipped[id] != reason
	t.skipped[id] = reason
	return changed
}

func (t *TunnelSync) pruneSkips(keep map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.skipped {
		if !keep[id] {
			delete(t.skipped, id)
		}
	}
}

// Endpoints is the tunnel table this device can declare to the registry: one
// entry per INSTALLED link, carrying this node's own overlay /32 on it. The
// controller turns those into the next-hop list peers resolve learned routes
// against, so an entry must be a reading of the dataplane, not of the bundle --
// a tunnel that is skipped or never came up has no next hop to offer.
//
// An EMPTY non-nil slice means "I run no tunnels" (a real state after a
// teardown); nil means "this agent has no tunnel applier at all".
func (t *TunnelSync) Endpoints() *[]wire.TunnelEndpoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]wire.TunnelEndpoint, 0, len(t.links))
	for id, l := range t.links {
		peer, ok := t.peerOf[id]
		if !ok || !l.Ready() {
			continue
		}
		local, ok := t.localOf[id]
		if !ok || !local.IsValid() {
			continue
		}
		out = append(out, wire.TunnelEndpoint{PeerNodeID: peer, LocalIP: local.Addr().String()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerNodeID != out[j].PeerNodeID {
			return out[i].PeerNodeID < out[j].PeerNodeID
		}
		return out[i].LocalIP < out[j].LocalIP
	})
	return &out
}

// dropDeadConnection resets the dataplane handle after the api socket died.
//
// A VPP restart closes every api connection and there is nothing to tear down
// (its config was memory-only), so the links are dropped without a teardown and
// the next reconcile adopts whatever survived and rebuilds the rest on a fresh
// connection. Doing nothing would strand the agent on a dead client: every
// retry fails the same way and the box stays configured-but-not-running.
func (t *TunnelSync) dropDeadConnection(err error) {
	if err == nil || !errors.Is(err, vppapi.ErrClosed) {
		return
	}
	t.applyMu.Lock()
	defer t.applyMu.Unlock()

	for id := range t.takeLinks() {
		t.delLink(id)
	}
	if c, ok := t.DataPlane().(interface{ Close() error }); ok {
		c.Close()
	}
	t.setDataPlane(nil)
	t.logf("VPP api 连接断了（%v）：下一次重连，按快照从头收敛", err)
}

// Kick starts a reconcile in the background when the desired version is not the
// one running and the last failure is out of its backoff window. It never blocks
// the heartbeat: a reconcile creates WG interfaces and pushes peers, and a
// device that goes quiet mid-change looks offline exactly when somebody is
// watching it.
//
// desired < 1 means the controller has no published configuration for the
// tenant (underlayVersionLocked returns 0). That is NOT "remove every tunnel":
// an unpublish or a failed load must not silently strip a working box, so the
// call is a no-op and any installed links simply keep running.
func (t *TunnelSync) Kick(ctx context.Context, desired int) {
	if desired < 1 {
		return
	}
	t.mu.Lock()
	if t.applying || (t.state.Version == desired && t.state.OK) || t.now().Before(t.nextTry) {
		t.mu.Unlock()
		return
	}
	t.applying = true
	t.mu.Unlock()

	go func() {
		actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		err := t.Sync(actx, desired)

		t.mu.Lock()
		t.applying = false
		// A per-tunnel failure is not a convergence: Sync returns nil for those,
		// but the device does not run the whole snapshot. Same backoff either way,
		// or a broken tunnel is retried -- and re-logged -- every heartbeat.
		partial := err == nil && !t.state.OK
		wait := time.Duration(0)
		fails := t.fails
		if err != nil || partial {
			t.fails++
			fails = t.fails
			wait = t.backoff()
			t.nextTry = t.now().Add(wait)
		} else {
			t.fails = 0
			t.nextTry = time.Time{}
		}
		partialErr := t.state.Error
		t.mu.Unlock()

		switch {
		case err != nil:
			t.logf("隧道配置 v%d 没装上（连续 %d 次，%s 后重试）: %v", desired, fails, wait, err)
		case partial:
			t.logf("隧道配置 v%d 只装上一部分（连续 %d 次，%s 后重试）: %s", desired, fails, wait, partialErr)
		default:
			t.logf("隧道配置 v%d 已收敛", desired)
		}
	}()
}

func (t *TunnelSync) backoff() time.Duration {
	d := t.retryMin
	for i := 1; i < t.fails && d < t.retryMax; i++ {
		d *= 2
	}
	if d > t.retryMax {
		d = t.retryMax
	}
	return d
}

// Sync fetches one snapshot and reconciles it. Per-tunnel failures are reported
// through state (OK=false + Error) rather than returned as a fetch error: other
// tunnels did converge and their success is a fact about this device.
func (t *TunnelSync) Sync(ctx context.Context, desired int) error {
	t.mu.Lock()
	t.state.Applying = true
	t.state.ApplyingVersion = desired
	t.mu.Unlock()

	ver, applied, skipped, errs, err := t.fetchAndApply(ctx, desired)
	t.dropDeadConnection(err)

	t.mu.Lock()
	t.state.Applying = false
	switch {
	case err != nil:
		t.state.OK = false
		t.state.Error = err.Error()
	default:
		t.state.Version = ver
		t.state.AppliedAt = t.now().Unix()
		t.state.Tunnels = applied
		t.state.Skipped = skipped
		t.state.OK = len(errs) == 0
		t.state.Error = strings.Join(errs, "；")
	}
	t.mu.Unlock()
	return err
}

func (t *TunnelSync) fetchAndApply(ctx context.Context, desired int) (
	ver, applied, skipped int, errs []string, err error) {

	t.applyMu.Lock()
	defer t.applyMu.Unlock()

	dp := t.DataPlane()
	if dp == nil {
		if t.dial == nil {
			return 0, 0, 0, nil, errors.New("agent: 没有数据面连接可用了（redial 未配置）")
		}
		ndp, derr := t.dial(ctx)
		if derr != nil {
			return 0, 0, 0, nil, fmt.Errorf("重连 VPP api.sock: %w", derr)
		}
		t.setDataPlane(ndp)
		dp = ndp
	}

	b, ferr := t.fetch(ctx)
	if errors.Is(ferr, ErrNoUnderlay) {
		return 0, 0, 0, nil, fmt.Errorf(
			"心跳说有隧道版本 %d，但控制器回 409（没有可下发的隧道配置）：现有隧道保持不动", desired)
	}
	if ferr != nil {
		return 0, 0, 0, nil, fmt.Errorf("拉取隧道配置: %w", ferr)
	}
	// A bundle addressed to someone else is never installable, even if this
	// device's certificate fetched it. Refusing is the only answer that cannot
	// become a typo in someone's favour.
	if b.NodeID != 0 && b.NodeID != t.id.NodeID() {
		return 0, 0, 0, nil, fmt.Errorf("隧道配置属于 node %d，本机是 node %d", b.NodeID, t.id.NodeID())
	}
	if b.TenantID != "" && b.TenantID != t.id.TenantID() {
		return 0, 0, 0, nil, fmt.Errorf("隧道配置属于租户 %q，本机属于 %q", b.TenantID, t.id.TenantID())
	}
	ca, err := t.id.CAPool()
	if err != nil {
		return 0, 0, 0, nil, err
	}
	priv := t.id.WGPrivateKey()
	ifaces, err := dp.DumpInterfaces(ctx)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("读接口表（解析隧道出口）: %w", err)
	}

	// 中转是设备角色，不是隧道属性：`hub 仅中转`那种盒子一条隧道端点都不是，
	// 但每条经它的路径都要在它身上验签、弹一段。这两件事（密钥环 + transit 端口）
	// 原先只在建 Link 的路上做，所以零隧道的中转盒什么都不会配 —— 把它选中做中转
	// 的那些隧道变成静默黑洞，控制器这边还一切"已下发"。失败记进 errs：这条设备
	// 级的事没做成，就不算收敛。
	if b.TransitPort != 0 {
		keys, kerr := linkKeys(b.PKeys)
		if kerr != nil {
			errs = append(errs, fmt.Sprintf("中转端口 %d 要用的密钥环: %v", b.TransitPort, kerr))
		} else if err := installPKeys(ctx, dp, keys); err != nil {
			errs = append(errs, fmt.Sprintf("中转端口 %d 要用的密钥环: %v", b.TransitPort, err))
		} else if err := dp.SetTransit(ctx, b.TransitPort); err != nil {
			errs = append(errs, fmt.Sprintf("开中转端口 %d: %v", b.TransitPort, err))
		}
	}

	keep := map[string]bool{}
	keepInst := map[uint32]bool{}
	for _, spec := range b.Tunnels {
		keep[spec.TunnelID] = true
		keepInst[spec.Instance] = true
		if err := t.applySpec(ctx, b, spec, ca, priv, ifaces); err != nil {
			errs = append(errs, fmt.Sprintf("隧道 %s: %v", spec.TunnelID, err))
			continue
		}
		applied++
	}
	for _, sk := range b.Skipped {
		// A skipped tunnel keeps whatever it had; its instance must not be swept.
		keep[sk.TunnelID] = true
		if l := t.getLink(sk.TunnelID); l != nil {
			keepInst[l.cfg.Instance] = true
		}
		if t.setSkip(sk.TunnelID, sk.Reason) {
			t.logf("隧道 %s 暂不可下发（已在跑的不动）: %s", sk.TunnelID, sk.Reason)
		}
		skipped++
	}
	t.pruneSkips(keep)

	// Tunnels the controller no longer lists (neither installed nor skipped).
	for id, l := range t.takeLinks() {
		if keep[id] {
			continue
		}
		if err := l.Teardown(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("撤隧道 %s: %v", id, err))
			continue
		}
		t.delLink(id)
		t.logf("隧道 %s 已从控制器配置里删除，接口已撤掉", id)
	}

	// Orphan sweep: agent-owned interfaces the bundle does not mention at all.
	// This is the case a deleted tunnel leaves behind when the agent was down
	// while it happened -- there is no local state file, so VPP's instance
	// numbers are the only record of what we built last time.
	wgIfs, werr := dp.Interfaces(ctx, false)
	if werr != nil {
		errs = append(errs, fmt.Sprintf("读 WG 接口表（清理孤儿接口）: %v", werr))
		return b.Version, applied, skipped, errs, nil
	}
	for _, ifi := range wgIfs {
		if ifi.UserInstance < wire.UnderlayInstanceBase || keepInst[ifi.UserInstance] {
			continue
		}
		if err := teardownTunnel(ctx, dp, ifi.SWIfIndex, ifi.Port,
			b.OverlayTable, netip.Prefix{}); err != nil {
			errs = append(errs, fmt.Sprintf("清理孤儿接口 inst=%d: %v", ifi.UserInstance, err))
			continue
		}
		t.logf("隧道接口 inst=%d 已不在控制器配置里，已撤掉", ifi.UserInstance)
	}
	return b.Version, applied, skipped, errs, nil
}

// applySpec brings one tunnel to the state the spec describes.
func (t *TunnelSync) applySpec(ctx context.Context, b wire.UnderlayBundle,
	spec wire.TunnelSpec, ca *x509.CertPool, priv []byte, ifaces []vppapi.SwInterface) error {

	if spec.Instance < wire.UnderlayInstanceBase {
		return fmt.Errorf("实例号 %d 低于 agent 的起点 %d：不接管（手配的接口归人管）",
			spec.Instance, wire.UnderlayInstanceBase)
	}
	cfg, err := t.linkConfig(ctx, b, spec, ca, priv, ifaces)
	if err != nil {
		return err
	}

	l := t.getLink(spec.TunnelID)
	if l != nil && !sameLinkConfig(l.cfg, cfg) {
		// Port, instance, egress, overlay and relay are fixed per Link on
		// purpose (each one changes the interface's identity in VPP), so a
		// change means rebuild rather than patch.
		if err := l.Teardown(ctx); err != nil {
			return fmt.Errorf("配置变了要重建，撤旧接口时: %w", err)
		}
		t.delLink(spec.TunnelID)
		l = nil
	}
	if l == nil {
		nl, err := NewLink(cfg)
		if err != nil {
			return err
		}
		l = nl
		// Recorded before it is opened: if Open or ApplyPeer fails the next
		// attempt continues from this object instead of creating a second
		// interface for the same instance.
		t.setLink(spec.TunnelID, l, spec.PeerNodeID, cfg.LocalOverlay)
	}

	keys, err := linkKeys(b.PKeys)
	if err != nil {
		return err
	}
	if err := l.InstallKeys(ctx, keys); err != nil {
		return err
	}
	if _, err := l.Open(ctx); err != nil {
		return err
	}
	change, err := l.ApplyPeer(ctx, PeerTarget{
		NodeID:  spec.PeerNodeID,
		Tenant:  b.TenantID,
		CertPEM: spec.PeerCertPEM,
		Binding: spec.PeerBinding,
		// The controller derived this address from the reflector's observation
		// (or from what the operator typed) and already paired it with the
		// peer's listen port. It is labelled as the controller's claim, not as
		// something this device saw.
		ReflectorAddr: spec.PeerEndpoint,
	})
	if err != nil {
		return err
	}
	if change != PeerUnchanged {
		t.logf("隧道 %s → node %d（%s）：peer %s", spec.TunnelID, spec.PeerNodeID,
			spec.PeerEndpoint, change)
	}
	return nil
}

// linkConfig turns one spec into the LinkConfig the dataplane needs. Every
// failure here is reported as a per-tunnel error: one bad spec must not stop
// the other links on the box from converging.
func (t *TunnelSync) linkConfig(ctx context.Context, b wire.UnderlayBundle,
	spec wire.TunnelSpec, ca *x509.CertPool, priv []byte, ifaces []vppapi.SwInterface) (LinkConfig, error) {

	egress, ok := findIfaceByName(ifaces, spec.Iface)
	if !ok {
		return LinkConfig{}, fmt.Errorf("出口接口 %q 在 VPP 里不存在（现有 %s）",
			spec.Iface, interfaceSummary(ifaces))
	}
	local, err := parseOverlayPrefix(spec.LocalOverlay, "本机 overlay")
	if err != nil {
		return LinkConfig{}, err
	}
	peer, err := parseOverlayPrefix(spec.PeerOverlay, "对端 overlay")
	if err != nil {
		return LinkConfig{}, err
	}
	src, err := t.sourceIP(ctx, spec, egress)
	if err != nil {
		return LinkConfig{}, err
	}
	cfg := LinkConfig{
		VPP: t.DataPlane(), Tenant: b.TenantID, CAPool: ca,
		Instance: spec.Instance, ListenPort: spec.ListenPort,
		TransitPort: b.TransitPort, SrcIP: src, PrivateKey: priv,
		EncapIfIndex: egress.SWIfIndex,
		TunnelMTU:    spec.MTU, UnderlayMTU: spec.UnderlayMTU,
		PathIDBase:   spec.PathIDBase,
		OverlayTable: b.OverlayTable,
		LocalOverlay: local, PeerOverlay: peer,
	}
	// 中转链按行走顺序原样进 cfg.Relays：倒序是段列表的事，这里翻了顺序就等于
	// 让控制器和盒子对"第一跳是谁"各有各的理解。
	for i, r := range spec.Relays {
		ip, port, err := parseAddr(r.Endpoint)
		if err != nil {
			return LinkConfig{}, fmt.Errorf("第 %d 跳中转 %q: %w", i+1, r.Endpoint, err)
		}
		cfg.Relays = append(cfg.Relays, RelayHop{IP: ip, Port: port})
	}
	return cfg, nil
}

// sourceIP answers "which outer address does this tunnel encapsulate from".
// The controller's value wins because it is the only one that can prove the
// address belongs to the egress port (see wire.TunnelSpec.SrcIP); otherwise the
// port's own address is used, and a port with no IPv4 is an error -- VPP would
// happily build the tunnel and send nothing.
func (t *TunnelSync) sourceIP(ctx context.Context, spec wire.TunnelSpec, egress vppapi.SwInterface) (net.IP, error) {
	if s := strings.TrimSpace(spec.SrcIP); s != "" {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("控制器给的源地址 %q: %w", s, err)
		}
		if !a.Is4() {
			return nil, fmt.Errorf("控制器给的源地址 %q 不是 IPv4", s)
		}
		return net.IP(a.AsSlice()), nil
	}
	addrs, err := t.DataPlane().IPv4Addresses(ctx, egress.SWIfIndex)
	if err != nil {
		return nil, fmt.Errorf("读出口 %s(%d) 的 IPv4 地址: %w", egress.Name, egress.SWIfIndex, err)
	}
	for _, p := range addrs {
		if p.Addr().Is4() && !p.Addr().IsLinkLocalUnicast() {
			return net.IP(p.Addr().AsSlice()), nil
		}
	}
	return nil, fmt.Errorf("控制器没给源地址，出口 %s(%d) 自己也没有可用的 IPv4 地址",
		egress.Name, egress.SWIfIndex)
}

func findIfaceByName(ifs []vppapi.SwInterface, name string) (vppapi.SwInterface, bool) {
	for _, i := range ifs {
		if i.Name == name {
			return i, true
		}
	}
	return vppapi.SwInterface{}, false
}

func parseOverlayPrefix(s, what string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s %q: %w", what, s, err)
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s %q 不是 IPv4", what, s)
	}
	return p.Masked(), nil
}

// linkKeys decodes the tenant's hop-MAC key ring. A key that is not hex is an
// error, not a skip: a link whose path signature uses a truncated key fails
// every packet without any counter that points at the key.
func linkKeys(pks []wire.PKey) ([]Key, error) {
	out := make([]Key, 0, len(pks))
	for _, pk := range pks {
		k, err := hex.DecodeString(pk.Key)
		if err != nil {
			return nil, fmt.Errorf("hop-MAC 密钥 %d 不是 hex: %w", pk.HmacID, err)
		}
		out = append(out, Key{HmacID: pk.HmacID, Key: k})
	}
	return out, nil
}

// sameLinkConfig reports whether a running Link already has every input that is
// fixed for its lifetime. Anything else (peer address, MTU record values) is
// reconciled idempotently on each pass.
func sameLinkConfig(a, b LinkConfig) bool {
	if a.Instance != b.Instance || a.ListenPort != b.ListenPort ||
		a.TransitPort != b.TransitPort || a.EncapIfIndex != b.EncapIfIndex ||
		a.PathIDBase != b.PathIDBase || a.OverlayTable != b.OverlayTable ||
		a.LocalOverlay != b.LocalOverlay || a.PeerOverlay != b.PeerOverlay ||
		!relaysEqual(a.Relays, b.Relays) || !a.SrcIP.Equal(b.SrcIP) {
		return false
	}
	amtu, bmtu := a.TunnelMTU, b.TunnelMTU
	if amtu == 0 {
		amtu = wire.DefaultTunnelMTU
	}
	if bmtu == 0 {
		bmtu = wire.DefaultTunnelMTU
	}
	return amtu == bmtu && a.UnderlayMTU == b.UnderlayMTU
}

// relaysEqual compares two relay chains element by element, order included: a
// path that only swapped its hops is a different path, and treating it as "same"
// would leave the running AddPath segment list pointing at the old first hop
// forever.
func relaysEqual(a, b []RelayHop) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Port != b[i].Port || !a[i].IP.Equal(b[i].IP) {
			return false
		}
	}
	return true
}
