// Command sr-agent is the CPE-side runtime: it enrolls a device into a tenant,
// then keeps it visible with heartbeats. That is all it does.
//
// Deliberately NOT here: the dataplane. Enrollment is tenant association only
// -- it carries no ports, no overlay address, no key ring, no RR list. Those
// arrive with the node plan once the customer configures the device in the
// console, and until that plan exists there is nothing for this process to
// install. Starting VPP work from an empty plan would only produce a device
// that looks configured and forwards nothing.
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
	"sdwan/controller/internal/wire"
)

const agentVersion = "sr-agent/0.1 (identity+heartbeat)"

var processStart = time.Now()

func main() {
	var (
		ctrl    = flag.String("ctrl", "", "controller TLS endpoint, host:port (required)")
		claim   = flag.String("claim", "", "enrollment claim code (required only until the device has an identity)")
		caPath  = flag.String("ca", "", "pinned controller CA bundle for the FIRST enrollment (default <id-dir>/pinned_ca.pem)")
		idDir   = flag.String("id", "./agent-id", "identity directory (device key, WG key, cert, CA)")
		serial  = flag.String("serial", "", "device serial (default hostname)")
		statusAddr = flag.String("status", "127.0.0.1:8089", "local status endpoint (empty disables it)")
		once    = flag.Bool("once", false, "enroll if needed, send one heartbeat, print status, exit")
		extraHB = flag.Int("hb", 0, "override the controller's heartbeat interval in seconds (testing only)")

		// Applying configuration needs root, vppctl and FRR, so it is opt-in.
		// Without -apply the agent still heartbeats and still REPORTS its config
		// state (which is "capable: false"), because a device that silently
		// ignores published versions must look different from one that has
		// nothing to install.
		applyCfg   = flag.Bool("apply", false, "install the controller's published config (needs root, vppctl, FRR)")
		storeRoot  = flag.String("store", "/var/lib/sr-cpe", "sr-apply store root (versions/, current, status.json)")
		vppctl     = flag.String("vppctl", "vppctl", "vppctl binary")
		frrConf    = flag.String("frr-conf", "/etc/frr/frr.conf", "FRR config path")
		frrReload  = flag.String("frr-reload", "", "FRR restart command (default: frrinit.sh or systemctl)")
	)
	flag.Parse()

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

	hbSec := 5
	if !id.Enrolled() {
		if *claim == "" {
			log.Fatalf("device %s has no identity yet: pass --claim (identity dir %s)", idSerial, *idDir)
		}
		roots, err := pinnedRoots(*caPath, *idDir)
		if err != nil {
			log.Fatalf("pinned CA: %v", err)
		}
		// Zero reflectors requested: the agent has no dataplane to send probes
		// from, so asking the controller to pick reflectors would be asking for a
		// fact it cannot use.
		req := id.EnrollRequest(*claim, agentVersion, 0)
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
	} else {
		log.Printf("using existing identity: tenant=%s node=%d serial=%s", id.TenantID(), id.NodeID(), idSerial)
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

	var cfg *configSync
	if *applyCfg {
		store := apply.Store{Root: *storeRoot}
		applier := apply.New(store)
		applier.VPPCtl = *vppctl
		applier.FRRConf = *frrConf
		applier.FRRReload = *frrReload
		applier.Logf = log.Printf
		cfg = newConfigSync(id.NodeID(), store, client.ConfigBundle,
			func(ctx context.Context, b apply.Bundle) error { return applier.Apply(ctx, b, false) },
			log.Printf, time.Now)
		if cur := cfg.State(); cur.Version > 0 {
			log.Printf("config sync on (store %s, running v%d)", *storeRoot, cur.Version)
		} else {
			log.Printf("config sync on (store %s, nothing installed yet)", *storeRoot)
		}
		st.cfg = cfg
	}

	if *once {
		hbctx, hbcancel := context.WithTimeout(ctx, 15*time.Second)
		req := wire.HeartbeatRequest{Uptime: 0}
		req.Config = st.configReport()
		resp, err := client.Heartbeat(hbctx, req)
		hbcancel()
		if err != nil {
			log.Fatalf("heartbeat: %v", err)
		}
		st.markOK(resp)
		log.Printf("heartbeat ok: epoch=%d peers=%d binding_err=%q config_version=%d",
			resp.Epoch, resp.PeerCount, resp.BindingErr, resp.ConfigVersion)
		if cfg != nil && resp.ConfigVersion > 0 {
			actx, acancel := context.WithTimeout(ctx, 5*time.Minute)
			err := cfg.Sync(actx, resp.ConfigVersion)
			acancel()
			if err != nil {
				log.Fatalf("config v%d: %v", resp.ConfigVersion, err)
			}
		}
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
		req.Config = st.configReport()
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
			if resp.BindingErr != "" {
				log.Printf("controller rejected a key binding: %s", resp.BindingErr)
			}
			// Applies run in the background: an apply restarts FRR and can take
			// tens of seconds, and a device that stops heartbeating while
			// installing its own config looks offline at the worst moment.
			if cfg != nil {
				cfg.Kick(ctx, resp.ConfigVersion)
			}
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

	hbInterval   int
	Heartbeats   int64  `json:"heartbeats"`
	LastOK       int64  `json:"last_ok_unix"`
	LastErr      string `json:"last_err,omitempty"`
	Epoch        uint32 `json:"epoch"`
	PeerCount    int    `json:"peer_count"`
	BindingErr   string `json:"binding_err,omitempty"`
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
}

const noDataPlane = "none: enrollment carries no configuration; -apply installs only the customer-side config, tunnels/ports still wait for the underlay plan"

func (s *agentStatus) snapshot() statusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := statusView{
		Serial: s.Serial, TenantID: s.TenantID, NodeID: s.NodeID,
		Controller: s.Controller, StartedAt: s.StartedAt,
		Heartbeats: s.Heartbeats, HBInterval: s.hbInterval,
		LastOK: s.LastOK, LastErr: s.LastErr,
		Epoch: s.Epoch, PeerCount: s.PeerCount, BindingErr: s.BindingErr,
		DataPlane: noDataPlane,
	}
	if s.cfg != nil {
		cur := s.cfg.State()
		v.Config = &cur
	} else {
		v.Config = &wire.ConfigState{Capable: false}
	}
	return v
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
