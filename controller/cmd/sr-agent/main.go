// Command sr-agent is the CPE-side runtime: it enrolls a device into a tenant,
// then keeps it visible with heartbeats. That is all it does.
//
// Deliberately NOT here: the dataplane. Enrollment is tenant association only
// -- it carries no ports, no overlay address, no key ring, no RR list. Those
// arrive with the node plan once the customer configures the device in the
// console, and until that plan exists there is nothing for this process to
// install. Starting VPP work from an empty plan would only produce a device
// that looks configured and forwards nothing.
//
// The ONE read-only exception is the interface inventory (-vpp-api): the
// controller cannot identify this box's underlay port from its own data, and
// the device is the only place that knows which ports it has. It is a reading
// of VPP's interface table, never a write, so it stays on this side of the
// "agent does not configure the dataplane" line.
//
// Address discovery lives here too, and it is on this same side of that line:
// the STUN probe is a kernel UDP socket that writes nothing to VPP. It belongs
// to the agent because the fact being measured is "where does a third party see
// THIS box", which the controller cannot observe by itself and the dataplane has
// no reason to answer for.
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/apply"
	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

const agentVersion = "sr-agent/0.1 (identity+heartbeat)"

var processStart = time.Now()

func main() {
	var (
		ctrl       = flag.String("ctrl", "", "controller TLS endpoint, host:port (required)")
		claim      = flag.String("claim", "", "enrollment claim code (required only until the device has an identity)")
		caPath     = flag.String("ca", "", "pinned controller CA bundle for the FIRST enrollment (default <id-dir>/pinned_ca.pem)")
		idDir      = flag.String("id", "./agent-id", "identity directory (device key, WG key, cert, CA)")
		serial     = flag.String("serial", "", "device serial (default hostname)")
		preAuth    = flag.String("preauth", "", "claim a 预授权 record created in the console, by its device name (first enrollment only)")
		statusAddr = flag.String("status", "127.0.0.1:8089", "local status endpoint (empty disables it)")
		once       = flag.Bool("once", false, "enroll if needed, send one heartbeat, print status, exit")
		extraHB    = flag.Int("hb", 0, "override the controller's heartbeat interval in seconds (testing only)")

		// Applying configuration needs root, vppctl and FRR, so it is opt-in.
		// Without -apply the agent still heartbeats and still REPORTS its config
		// state (which is "capable: false"), because a device that silently
		// ignores published versions must look different from one that has
		// nothing to install.
		applyCfg  = flag.Bool("apply", false, "install the controller's published config (needs root, vppctl, FRR)")
		storeRoot = flag.String("store", "/var/lib/sr-cpe", "sr-apply store root (versions/, current, status.json)")
		vppctl    = flag.String("vppctl", "vppctl", "vppctl binary")
		frrConf   = flag.String("frr-conf", "/etc/frr/frr.conf", "FRR config path")
		frrReload = flag.String("frr-reload", "", "frr-reload.py used to diff FRR config through vtysh (default: /usr/lib/frr/frr-reload.py)")
		routeConf = flag.String("route-conf", "/etc/sdwan/route.json",
			"sr-route config file; the bundle's overlay segment is merged into it (never created here)")

		// The interface inventory is read-only and off by default: a box whose
		// VPP is managed by hand may not have this socket, and an agent that
		// fails to start because an optional report is unavailable is worse
		// than one that reports nothing.
		vppAPI = flag.String("vpp-api", "", "VPP api.sock for the read-only interface inventory (empty disables it)")

		// The tunnel applier WRITES to VPP, so it is a separate opt-in from the
		// read-only inventory above: an operator who gave this agent the socket
		// to report interfaces did not thereby hand over the dataplane.
		tunnels = flag.Bool("tunnels", false, "install the controller's tunnel section into VPP (needs -vpp-api)")

		// Address discovery runs on a KERNEL socket, so it needs neither root nor
		// the dataplane. The device does not learn the reflector addresses from
		// here -- the controller advertises them (see agent.StunSync) -- and this
		// number only says how many of them this process is willing to probe.
		// Two is the minimum that can produce a symmetric-NAT verdict, which is
		// why it is the default; 0 switches discovery off.
		stunN    = flag.Int("stun-reflectors", 2, "how many reflector addresses the controller may hand this agent to probe (0 disables address discovery)")
		stunBind = flag.String("stun-bind", ":0", "local UDP source for the STUN probes (\"ip:port\" to pin the egress address and port)")
	)
	flag.Parse()

	if *tunnels && strings.TrimSpace(*vppAPI) == "" {
		log.Fatal("--tunnels needs --vpp-api: the applier works through VPP's api socket")
	}
	if *stunN < 0 {
		// Negative is not "off": the cap would silently pass a nonsense number to
		// the controller, so say what the flag means instead of guessing.
		log.Fatalf("--stun-reflectors must be 0 (no discovery) or more, got %d", *stunN)
	}

	if strings.TrimSpace(*ctrl) == "" {
		log.Fatal("--ctrl is required (host:port of the controller's mTLS endpoint)")
	}
	base, err := ctrlBase(*ctrl)
	if err != nil {
		log.Fatalf("--ctrl: %v", err)
	}
	idSerial := strings.TrimSpace(*serial)
	if idSerial == "" {
		h, err := os.Hostname()
		if err != nil {
			log.Fatalf("hostname (or pass --serial): %v", err)
		}
		idSerial = h
	}

	id, err := agent.LoadOrCreate(*idDir, idSerial)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Address discovery. Off with --stun-reflectors 0, and quiet until the
	// controller names a reflector: an agent that was never given an address to
	// probe reports no observation rather than a failure, because "nobody told me
	// where to look" is not this device's fault and must not read as one.
	ss := agent.NewStunSync(*stunBind, *stunN, log.Printf)
	defer ss.Close()

	hbSec := 5
	if !id.Enrolled() {
		if *claim == "" {
			log.Fatalf("device %s has no identity yet: pass --claim (identity dir %s)", idSerial, *idDir)
		}
		roots, err := pinnedRoots(*caPath, *idDir)
		if err != nil {
			log.Fatalf("pinned CA: %v", err)
		}
		// The reflector count is this agent's own limit, not a request for a
		// service: it says how many addresses the controller may hand back for
		// this device to probe. Two is the floor for a symmetric-NAT verdict.
		req := id.EnrollRequest(*claim, agentVersion, *stunN, *preAuth)
		ectx, ecancel := context.WithTimeout(ctx, 20*time.Second)
		resp, err := agent.Enroll(ectx, base, roots, req)
		ecancel()
		if err != nil {
			log.Fatalf("enroll: %v", err)
		}
		if err := id.SetEnrolled(resp.TenantID, resp.NodeID, resp.CertPEM, resp.CAPEM); err != nil {
			log.Fatalf("enroll: storing identity: %v", err)
		}
		if resp.HeartbeatSec > 0 {
			hbSec = resp.HeartbeatSec
		}
		log.Printf("enrolled: tenant=%s node=%d serial=%s epoch=%d", resp.TenantID, resp.NodeID, idSerial, resp.Epoch)
		ss.SetReflectors(resp.Reflectors)
		// 预授权 hand-over is only finished when the controller says so. An agent
		// that asked for an index and got an empty echo did NOT get the config
		// waiting behind it -- and from a rack, that is otherwise invisible.
		if *preAuth != "" {
			if resp.PreAuthName == "" {
				log.Printf("WARNING: 控制器没有认领预授权「%s」（应答里没有回显）；这台设备按普通入网处理，控制器上等它的配置不会下发", *preAuth)
			} else {
				log.Printf("预授权「%s」已认领到 node %d；配置仍在控制器上，等租户显式发布后随 -apply 下发", resp.PreAuthName, resp.NodeID)
			}
		}
	} else {
		log.Printf("using existing identity: tenant=%s node=%d serial=%s", id.TenantID(), id.NodeID(), idSerial)
		if *preAuth != "" {
			// The index rides the enrollment request and nothing else, so saying
			// nothing here would let someone believe a re-run had claimed it.
			log.Printf("WARNING: 设备已有身份（node %d），本次没有发送 enroll，-preauth「%s」未生效；要认领这条预授权，需要一台还没有身份的盒子（或先清掉 %s）",
				id.NodeID(), *preAuth, *idDir)
		}
	}

	st := &agentStatus{
		Serial: id.Serial(), TenantID: id.TenantID(), NodeID: id.NodeID(),
		StartedAt: time.Now().Unix(), Controller: base,
	}
	if *extraHB > 0 {
		hbSec = *extraHB
	}
	st.hbInterval = hbSec

	if *statusAddr != "" {
		go serveStatus(*statusAddr, st)
	}

	client, err := agent.NewClient(base, id)
	if err != nil {
		log.Fatalf("client: %v", err)
	}

	// Read-only interface inventory. Off unless -vpp-api is given, because a box
	// whose VPP lives in another namespace may not have this socket at all --
	// and an optional report must never be a reason the agent cannot start.
	inv := agent.NewInventoryReporter(*vppAPI, "sr-agent", log.Printf)
	defer inv.Close()
	if *vppAPI != "" {
		log.Printf("interface inventory on (vpp api %s)", *vppAPI)
	}

	// Tunnel applier. Off unless -tunnels is given because, unlike the inventory
	// above, it WRITES to VPP. The connection is dialled lazily: a VPP that is
	// down at start is a state to report ("capable, nothing applied yet"), not a
	// reason to refuse to run, and the reconciler redials by itself (see
	// TunnelSync.dropDeadConnection).
	var tun *agent.TunnelSync
	if *tunnels {
		dial := func(ctx context.Context) (agent.DataPlane, error) {
			return vppapi.Dial(ctx, *vppAPI, "sr-agent-tunnel")
		}
		dp, derr := dial(ctx)
		if derr != nil {
			log.Printf("tunnel applier: no VPP connection yet (%v); the first reconcile will retry", derr)
		}
		tun = agent.NewTunnelSync(dp, id, client.UnderlayBundle, log.Printf)
		tun.SetRedial(dial)
		log.Printf("tunnel applier on (vpp api %s)", *vppAPI)
	}
	st.setChannels(tun, *vppAPI != "")
	st.setStun(ss)
	reporter := &reportSender{client: client, id: id, tun: tun, stun: ss, logf: log.Printf}
	// Address discovery runs on its own beat, not the heartbeat's: a round is two
	// reflectors times two tries, and a device must never miss a beat because it is
	// waiting for a third party to answer.
	if *stunN > 0 {
		go ss.Loop(ctx, 2*time.Second)
	}
	// 隧道内遥测有自己的节拍，不挂在心跳上：每次 Sample 报的是"上一次采样以来的
	// 增量"，心跳退避到 30s 时窗口会被拉长，抖动/丢包就糊掉了。
	if tun != nil {
		go metricsLoop(ctx, tun.Telemetry(), client, log.Printf)
	}

	var cfg *configSync
	if *applyCfg {
		store := apply.Store{Root: *storeRoot}
		applier := apply.New(store)
		applier.VPPCtl = *vppctl
		// The same socket the inventory reads from. It is not a new privilege here:
		// -apply already writes VPP through vppctl. Left empty, a bundle with a
		// virtual router fails with the reason in its text instead of half-applying.
		applier.VPPAPI = *vppAPI
		applier.FRRConf = *frrConf
		applier.FRRReload = *frrReload
		applier.RouteConf = *routeConf
		applier.Logf = log.Printf
		cfg = newConfigSync(id.NodeID(), store, client.ConfigBundle,
			func(ctx context.Context, b apply.Bundle) error { return applier.Apply(ctx, b, false) },
			log.Printf, time.Now)
		if cur := cfg.State(); cur.Version > 0 {
			log.Printf("config sync on (store %s, running v%d)", *storeRoot, cur.Version)
		} else {
			log.Printf("config sync on (store %s, nothing installed yet)", *storeRoot)
		}
		st.setConfig(cfg)
	}

	if *once {
		hbctx, hbcancel := context.WithTimeout(ctx, 15*time.Second)
		req := wire.HeartbeatRequest{Uptime: 0}
		req.Binding = signedBinding(id, log.Printf)
		req.Config = st.configReport()
		req.Tunnel = st.tunnelReport()
		req.Ifaces = readInventory(ctx, inv)
		resp, err := client.Heartbeat(hbctx, req)
		hbcancel()
		if err != nil {
			log.Fatalf("heartbeat: %v", err)
		}
		st.markOK(resp)
		log.Printf("heartbeat ok: epoch=%d peers=%d binding_err=%q config_version=%d tunnel_version=%d",
			resp.Epoch, resp.PeerCount, resp.BindingErr, resp.ConfigVersion, resp.TunnelVersion)
		if cfg != nil && resp.ConfigVersion > 0 {
			actx, acancel := context.WithTimeout(ctx, 5*time.Minute)
			err := cfg.Sync(actx, resp.ConfigVersion)
			acancel()
			if err != nil {
				log.Fatalf("config v%d: %v", resp.ConfigVersion, err)
			}
		}
		// Applied synchronously here (not Kick'ed): -once prints the state and
		// exits, so a tunnel left to a background goroutine would never land.
		if tun != nil && resp.TunnelVersion > 0 {
			tctx, tcancel := context.WithTimeout(ctx, 5*time.Minute)
			err := tun.Sync(tctx, resp.TunnelVersion)
			tcancel()
			if err != nil {
				log.Fatalf("tunnel v%d: %v", resp.TunnelVersion, err)
			}
		}
		// Reported AFTER applying, so the tunnel table the controller stores is
		// what the dataplane really has, not what the bundle asked for. -once has
		// no probe loop running, so discovery takes its one round here: a printed
		// status without a mapping must mean "the reflector did not answer" (the
		// reason is in the stun section), not "we exited before asking".
		if *stunN > 0 {
			ss.SetReflectors(resp.Reflectors)
			sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
			ss.Sync(sctx)
			scancel()
		}
		reporter.send(ctx, req.Ifaces)
		b, _ := json.MarshalIndent(st.snapshot(), "", "  ")
		fmt.Println(string(b))
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Heartbeat loop. A failure is logged and retried on the same cadence: the
	// controller has its own staleness timer, and a device that exits on the
	// first blip can never come back on its own.
	backoff := time.Duration(0)
	for {
		hbctx, hbcancel := context.WithTimeout(ctx, 10*time.Second)
		req := wire.HeartbeatRequest{Uptime: int64(time.Since(processStart).Seconds())}
		req.Binding = signedBinding(id, log.Printf)
		req.Config = st.configReport()
		req.Tunnel = st.tunnelReport()
		req.Ifaces = readInventory(ctx, inv)
		resp, err := client.Heartbeat(hbctx, req)
		hbcancel()
		if err != nil {
			st.markErr(err)
			log.Printf("heartbeat failed: %v", err)
			switch agent.StatusOf(err) {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
				// The controller no longer accepts this identity (certificate
				// rotated, node removed, registry reset). Retrying cannot fix it;
				// say so and stop instead of looping forever.
				log.Printf("identity rejected by the controller; wipe %s to re-enroll", *idDir)
				return
			}
			backoff = nextBackoff(backoff)
		} else {
			st.markOK(resp)
			backoff = 0
			// The controller may start its reflector service after this box already
			// enrolled, and a restarted agent never enrolls again -- so the list
			// rides every ack, and a changed list makes the loop take a new round.
			ss.SetReflectors(resp.Reflectors)
			if resp.BindingErr != "" {
				log.Printf("controller rejected a key binding: %s", resp.BindingErr)
			}
			// Applies run in the background: an apply restarts FRR and can take
			// tens of seconds, and a device that stops heartbeating while
			// installing its own config looks offline at the worst moment.
			if cfg != nil {
				cfg.Kick(ctx, resp.ConfigVersion)
			}
			if tun != nil {
				tun.Kick(ctx, resp.TunnelVersion)
			}
			// What this box really runs, reported every beat: the mapping table
			// (binding + tunnel endpoints + own addresses) and one window of
			// outer-source observations. Both are readings, so they are taken
			// after the reconcile was Kicked rather than before it.
			reporter.send(ctx, req.Ifaces)
		}

		wait := time.Duration(hbSec) * time.Second
		if backoff > wait {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return
		case <-sig:
			log.Printf("shutting down")
			return
		case <-time.After(wait):
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur == 0 {
		return 2 * time.Second
	}
	cur *= 2
	if cur > 30*time.Second {
		cur = 30 * time.Second
	}
	return cur
}

// metricsInterval 是上报窗口的长度。300ms 的探针在这一段里跑约 33 轮，够算出
// 有意义的抖动/丢包；再短只是把同一条隧道的行刷得更勤，再长则断链要等更久才可见。
const metricsInterval = 10 * time.Second

// metricsLoop 每 metricsInterval 读一次数据面的探针/BFD 快照并上报。
//
// 一次失败只记日志：窗口是增量（见 agent.Telemetry.row），丢一个窗口不会污染
// 下一个；而在这里退避或退出，只会让控制器上的数据变成"陈旧"而不是"报错"。
// 没有隧道时也发一次空表 —— 隧道全撤了还挂着旧行，读数比空表更误导。
func metricsLoop(ctx context.Context, tel *agent.Telemetry, client *agent.Client, logf func(string, ...any)) {
	hadRows := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(metricsInterval):
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rows, err := tel.Sample(rctx)
		if err == nil && (len(rows) > 0 || hadRows) {
			err = client.ReportMetrics(rctx, wire.MetricsReport{Tunnels: rows})
		}
		cancel()
		if err != nil {
			logf("metrics: %v", err)
			continue
		}
		hadRows = len(rows) > 0
	}
}

// readInventory takes one inventory reading for this heartbeat on its own,
// shorter deadline: a wedged VPP must not eat the heartbeat's whole budget and
// turn an optional report into "device went offline".
func readInventory(ctx context.Context, inv *agent.InventoryReporter) *[]wire.IfaceInfo {
	if inv == nil {
		return nil
	}
	ictx, icancel := context.WithTimeout(ctx, 3*time.Second)
	defer icancel()
	return inv.Report(ictx)
}

// signedBinding is the WG-key attestation carried by every heartbeat: a
// statement signed with the device identity key, not a derived value. It can
// only fail before enrollment, which main refuses earlier, so a nil here means
// the controller must treat this box as unbound rather than trust the key.
func signedBinding(id *agent.Identity, logf func(string, ...any)) *keybinding.Binding {
	b, err := id.Binding()
	if err != nil {
		logf("key binding: %v", err)
		return nil
	}
	return &b
}

// ctrlBase normalises --ctrl into an https URL. Bare host:port is the common
// case; a full URL is accepted so a test can point at a proxy.
func ctrlBase(s string) (string, error) {
	if strings.HasPrefix(s, "https://") {
		return strings.TrimSuffix(s, "/"), nil
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("only https:// is accepted, got %q", s)
	}
	return "https://" + strings.TrimSuffix(s, "/"), nil
}

// pinnedRoots is the trust anchor for the FIRST enrollment.
//
// Order matters: an already-stored CA (from a previous enrollment) wins, because
// that is the CA the device's own certificate chains to. --ca is for the very
// first boot, when the only thing on the box is what the operator put there.
func pinnedRoots(caFlag, idDir string) (*x509.CertPool, error) {
	candidates := []string{}
	if caFlag != "" {
		candidates = append(candidates, caFlag)
	}
	candidates = append(candidates, idDir+"/pinned_ca.pem")
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("%s contains no PEM certificate", p)
		}
		log.Printf("trust anchor: %s", p)
		return pool, nil
	}
	return nil, fmt.Errorf("no CA bundle: pass --ca or place the controller CA at %s/pinned_ca.pem", idDir)
}

// ------------------------------------------------------------------ status

type agentStatus struct {
	mu sync.Mutex

	Serial     string `json:"serial"`
	TenantID   string `json:"tenant_id"`
	NodeID     uint16 `json:"node_id"`
	Controller string `json:"controller"`
	StartedAt  int64  `json:"started_at_unix"`

	// cfg is nil when the agent runs without -apply; the status view then says
	// capable:false rather than hiding the channel.
	cfg *configSync
	// tun is nil when the agent runs without -tunnels. hasVPPAPI is separate
	// because "this binary has the channel, it is switched off" and "this
	// binary has no dataplane at all" must not report the same thing.
	tun       *agent.TunnelSync
	hasVPPAPI bool
	// stun is the discovery loop. It guards its own state, so the snapshot below
	// reads it through its own accessor rather than copying it under s.mu.
	stun *agent.StunSync

	hbInterval int
	Heartbeats int64  `json:"heartbeats"`
	LastOK     int64  `json:"last_ok_unix"`
	LastErr    string `json:"last_err,omitempty"`
	Epoch      uint32 `json:"epoch"`
	PeerCount  int    `json:"peer_count"`
	BindingErr string `json:"binding_err,omitempty"`
}

type statusView struct {
	Serial     string `json:"serial"`
	TenantID   string `json:"tenant_id"`
	NodeID     uint16 `json:"node_id"`
	Controller string `json:"controller"`
	StartedAt  int64  `json:"started_at_unix"`
	Heartbeats int64  `json:"heartbeats"`
	HBInterval int    `json:"heartbeat_sec"`
	LastOK     int64  `json:"last_ok_unix"`
	LastErr    string `json:"last_err,omitempty"`
	Epoch      uint32 `json:"epoch"`
	PeerCount  int    `json:"peer_count"`
	BindingErr string `json:"binding_err,omitempty"`
	DataPlane  string `json:"dataplane"`

	Config *wire.ConfigState `json:"config"`
	// Tunnel is what the box runs on the underlay channel. nil = this agent has
	// no dataplane (no -vpp-api); capable:false = the channel exists but
	// -tunnels is off.
	Tunnel *wire.TunnelState `json:"tunnel"`
	// Stun is what this box's own probes measured: the reflectors the controller
	// advertised, the outer address they saw, the local source the probe left
	// from, and the symmetric verdict when there were two reflectors to compare.
	// An empty mapping means nobody answered -- it is not "no outer address".
	Stun agent.StunSnapshot `json:"stun"`
}

func (s *agentStatus) snapshot() statusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := statusView{
		Serial: s.Serial, TenantID: s.TenantID, NodeID: s.NodeID,
		Controller: s.Controller, StartedAt: s.StartedAt,
		Heartbeats: s.Heartbeats, HBInterval: s.hbInterval,
		LastOK: s.LastOK, LastErr: s.LastErr,
		Epoch: s.Epoch, PeerCount: s.PeerCount, BindingErr: s.BindingErr,
		DataPlane: s.dataPlaneDesc(),
		Tunnel:    s.tunnelReportLocked(),
	}
	// StunSync guards its own state, and it never calls back into agentStatus, so
	// reading it under s.mu has a fixed lock order. Taking its lock out here would
	// let a heartbeat write race the page it is meant to describe.
	v.Stun = s.stun.Snapshot()
	if s.cfg != nil {
		cur := s.cfg.State()
		v.Config = &cur
	} else {
		v.Config = &wire.ConfigState{Capable: false}
	}
	return v
}

// dataPlaneDesc says what this process can do to the dataplane, in the same
// honest spirit as configReport: a box with the socket but the applier off must
// not read like a box that owns the dataplane.
func (s *agentStatus) dataPlaneDesc() string {
	switch {
	case s.tun != nil:
		return "underlay tunnels (-tunnels): applies the controller's tunnel section; -apply adds the customer-side config"
	case s.hasVPPAPI:
		return "inventory only (-vpp-api): interfaces are read, nothing is installed; add -tunnels to apply the tunnel section"
	default:
		return "none: enrollment carries no configuration; -vpp-api and -apply are both off"
	}
}

// tunnelReport is what every heartbeat carries about the underlay channel.
// nil means "no dataplane to apply to at all"; capable:false means "the channel
// exists in this binary and was switched off" -- the controller renders the two
// differently, so collapsing them would hide a device that predates the channel.
func (s *agentStatus) tunnelReport() *wire.TunnelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tunnelReportLocked()
}

func (s *agentStatus) tunnelReportLocked() *wire.TunnelState {
	if s.tun != nil {
		cur := s.tun.State()
		return &cur
	}
	if s.hasVPPAPI {
		return &wire.TunnelState{Capable: false}
	}
	return nil
}

// setChannels records which dataplane channels this process runs. It takes the
// lock because the status listener is already serving while main is wiring
// these up.
func (s *agentStatus) setChannels(tun *agent.TunnelSync, hasVPPAPI bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tun, s.hasVPPAPI = tun, hasVPPAPI
}

func (s *agentStatus) setStun(ss *agent.StunSync) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stun = ss
}

func (s *agentStatus) setConfig(cfg *configSync) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// configReport is what every heartbeat carries. A device without -apply says
// "capable:false" rather than staying silent: silence is indistinguishable from
// an old agent, and the console would have to render it as unknown.
func (s *agentStatus) configReport() *wire.ConfigState {
	if s.cfg != nil {
		cur := s.cfg.State()
		return &cur
	}
	return &wire.ConfigState{Capable: false}
}

func (s *agentStatus) markOK(resp wire.HeartbeatResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Heartbeats++
	s.LastOK = time.Now().Unix()
	s.LastErr = ""
	s.Epoch = resp.Epoch
	s.PeerCount = resp.PeerCount
	s.BindingErr = resp.BindingErr
}

func (s *agentStatus) markErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastErr = err.Error()
}

func serveStatus(addr string, st *agentStatus) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st.snapshot())
	})
	log.Printf("status on http://%s/status", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("status listener: %v", err)
	}
}
