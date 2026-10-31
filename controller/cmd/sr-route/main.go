// sr-route is the CPE's route-protocol instance: it peers with the two RRs the
// controller named, announces this device's inside prefixes, and keeps a "bgp"
// table per VRF built from the routes whose RTs the VRF imports.
//
// It does not touch the forwarding table. FIB is the control plane's output and
// installing it is a separate, explicit step -- the seam between this table and
// the dataplane is deliberately not hidden here.
//
// Identity is the device certificate from enrollment (the same one sr-agent
// uses). The RR trusts that certificate for the device id, so this binary
// refuses to start if the certificate says a different (tenant, node) than the
// config.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/route"
	"sdwan/controller/internal/wire"
)

type fileConfig struct {
	Tenant        string           `json:"tenant"`
	DeviceID      uint16           `json:"device_id"`
	IdentityDir   string           `json:"identity_dir"`
	Serial        string           `json:"serial"`
	HoldSec       int              `json:"hold_sec"`
	TLSServerName string           `json:"tls_server_name"`
	RRs           []wire.RRAddress `json:"rrs"`
	Tunnels       []tunnelConfig   `json:"tunnels"`
	VRFs          []vrfConfig      `json:"vrfs"`
	LocalPrefixes []prefixConfig   `json:"local_prefixes"`
	StatusAddr    string           `json:"status_addr"`
}

// tunnelConfig 是控制器分配的隧道表：对端设备 + 对端 overlay 地址。本端接口名
// 只需要 sr-fib 知道，路由协议不关心。
type tunnelConfig struct {
	Peer    uint16 `json:"peer_device_id"`
	NextHop string `json:"next_hop"`
}

type vrfConfig struct {
	ID        uint8    `json:"id"`
	ExportRTs []string `json:"export_rts"`
	ImportRTs []string `json:"import_rts"`
}

type prefixConfig struct {
	VRFID  uint8  `json:"vrf_id"`
	Prefix string `json:"prefix"`
}

func loadConfig(path string) (*fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg fileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch {
	case cfg.Tenant == "":
		return nil, errors.New("tenant is required")
	case cfg.DeviceID == 0:
		return nil, errors.New("device_id is required")
	case cfg.IdentityDir == "":
		return nil, errors.New("identity_dir is required (the device certificate comes from enrollment)")
	case cfg.Serial == "":
		return nil, errors.New("serial is required (it names the identity in identity_dir)")
	case len(cfg.RRs) == 0:
		return nil, errors.New("rrs is required (the controller pushes these at enrollment)")
	}
	for i, rr := range cfg.RRs {
		if rr.Addr == "" {
			return nil, fmt.Errorf("rrs[%d] has no addr", i)
		}
	}
	return &cfg, nil
}

func main() {
	configPath := flag.String("config", "", "path to the CPE route config JSON")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("-config is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	id, err := agent.LoadOrCreate(cfg.IdentityDir, cfg.Serial)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	if !id.Enrolled() {
		log.Fatalf("identity: %s holds no certificate; enroll this device first", cfg.IdentityDir)
	}
	if id.TenantID() != cfg.Tenant || id.NodeID() != cfg.DeviceID {
		// The certificate is the authoritative identity on the RR side; a
		// config that disagrees would just make every session fail its hello
		// check, so refuse at startup where the message is readable.
		log.Fatalf("identity: certificate is %s/node/%d but config says %s/node/%d",
			id.TenantID(), id.NodeID(), cfg.Tenant, cfg.DeviceID)
	}

	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		name := cfg.TLSServerName
		if name == "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("rr address %q: %w", addr, err)
			}
			name = host
		}
		tcfg, err := id.TLSConfig(name)
		if err != nil {
			return nil, err
		}
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
			Config:    tcfg,
		}
		return d.DialContext(ctx, "tcp", addr)
	}

	clientCfg := route.ClientConfig{
		Tenant:   cfg.Tenant,
		DeviceID: cfg.DeviceID,
		HoldSec:  cfg.HoldSec,
		RRs:      make([]route.RRConfig, 0, len(cfg.RRs)),
		Dial:     dial,
		Logf:     log.Printf,
	}
	for _, rr := range cfg.RRs {
		clientCfg.RRs = append(clientCfg.RRs, route.RRConfig{ID: rr.ID, Addr: rr.Addr})
	}
	for _, t := range cfg.Tunnels {
		clientCfg.Tunnels = append(clientCfg.Tunnels, route.Tunnel{Peer: t.Peer, NextHop: t.NextHop})
	}
	for _, v := range cfg.VRFs {
		clientCfg.VRFs = append(clientCfg.VRFs, route.VRFConfig{ID: v.ID, ExportRTs: v.ExportRTs, ImportRTs: v.ImportRTs})
	}
	for _, p := range cfg.LocalPrefixes {
		clientCfg.LocalPrefixes = append(clientCfg.LocalPrefixes, route.LocalPrefix{VRFID: p.VRFID, Prefix: p.Prefix})
	}

	c, err := route.NewClient(clientCfg)
	if err != nil {
		log.Fatalf("route client: %v", err)
	}
	log.Printf("sr-route: device %d (tenant %s), %d local prefixes, %d tunnels, %d RRs",
		cfg.DeviceID, cfg.Tenant, len(c.LocalRoutes()), len(c.Tunnels()), len(cfg.RRs))

	if cfg.StatusAddr != "" {
		go serveStatus(cfg.StatusAddr, c)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go watchRIB(c, ctx.Done())

	if err := c.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
	log.Printf("sr-route: stopped")
}

// watchRIB logs a change of active RR or table shape. Failover that nobody can
// see is indistinguishable from a table that quietly went empty.
func watchRIB(c *route.Client, stop <-chan struct{}) {
	last := ""
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			cur := summary(c)
			if cur != last {
				log.Printf("sr-route: %s", cur)
				last = cur
			}
		}
	}
}

func summary(c *route.Client) string {
	st := c.Stats()
	var synced []string
	for _, s := range st.Sessions {
		state := "down"
		if s.Connected {
			state = "connected"
			if s.Synced {
				state = "synced"
			}
		}
		synced = append(synced, fmt.Sprintf("%s=%s/%d", s.Addr, state, s.Routes))
	}
	rib := c.AllRIB()
	vrfs := make([]int, 0, len(rib))
	for v := range rib {
		vrfs = append(vrfs, int(v))
	}
	sort.Ints(vrfs)
	parts := make([]string, 0, len(vrfs))
	for _, v := range vrfs {
		parts = append(parts, strconv.Itoa(v)+":"+strconv.Itoa(len(rib[uint8(v)])))
	}
	if len(parts) == 0 {
		parts = append(parts, "empty")
	}
	base := fmt.Sprintf("active=%d sessions[%v] rib{%s} held=%d",
		st.ActiveRR, synced, strings.Join(parts, " "), st.ActivePending)
	if st.RRWait == nil {
		return base
	}
	// 换 RR 没走完必须看得见：盘上的 rrs 已经是新的，活着的反射器还是旧的，
	// 这正是"配置未完全生效"，不该只在日志里一闪而过。
	return base + fmt.Sprintf(" 换 RR 未完成：新 RR %v 未成立，旧会话 %v 保留",
		st.RRWait.PendingAddrs, st.RRWait.RetiringAddrs)
}

// localPrefixJSON is both the /status "local" shape and the /local-prefixes
// request/response body. It carries no RTs: those come from the VRF config.
type localPrefixJSON struct {
	VRFID  uint8  `json:"vrf_id"`
	Prefix string `json:"prefix"`
}

type localPrefixesDoc struct {
	Prefixes []localPrefixJSON `json:"prefixes"`
}

func localPrefixes(c *route.Client) []localPrefixJSON {
	routes := c.LocalRoutes()
	out := make([]localPrefixJSON, 0, len(routes))
	for _, rt := range routes {
		out = append(out, localPrefixJSON{VRFID: rt.VRFID, Prefix: rt.Prefix})
	}
	return out
}

// statusDoc is the /status body. ClientStats is embedded, so its fields stay at
// the top level the way sr-fib and the console read them.
//
// The two held-back lists are bounded (maxStatusList) and always come with a
// total. A device can have as many pending prefixes as the RR knows about, and
// this endpoint is polled, not streamed -- but a truncated list with no total
// would let a console say "these are all the routes being held back", which is
// the kind of number nobody can act on and nobody can disprove.
type statusDoc struct {
	route.ClientStats
	RIB                 map[string][]route.RIBEntry `json:"rib"`
	Local               []localPrefixJSON           `json:"local"`
	Tunnels             []tunnelConfig              `json:"tunnels"`
	VRFs                []vrfConfig                 `json:"vrfs"`
	Pending             []route.PendingRoute        `json:"pending"`
	PendingTotal        int                         `json:"pending_total"`
	PendingTruncated    bool                        `json:"pending_truncated"`
	LocalSkips          []route.LocalSkip           `json:"local_skips"`
	LocalSkipsTotal     int                         `json:"local_skips_total"`
	LocalSkipsTruncated bool                        `json:"local_skips_truncated"`
}

// Enough to show a console a real page of held routes without letting one VPN
// worth of them dominate the body.
const maxStatusList = 200

func statusFor(c *route.Client) statusDoc {
	all := c.AllRIB()
	doc := statusDoc{
		ClientStats: c.Stats(),
		RIB:         make(map[string][]route.RIBEntry, len(all)),
		Local:       localPrefixes(c),
		Tunnels:     tunnelsOf(c),
		VRFs:        vrfsOf(c),
	}
	for vrf, routes := range all {
		doc.RIB[strconv.Itoa(int(vrf))] = routes
	}
	doc.Pending = c.Pending()
	doc.PendingTotal = len(doc.Pending)
	if doc.PendingTotal > maxStatusList {
		doc.Pending = doc.Pending[:maxStatusList]
		doc.PendingTruncated = true
	}
	doc.LocalSkips = c.LocalSkips()
	doc.LocalSkipsTotal = len(doc.LocalSkips)
	if doc.LocalSkipsTotal > maxStatusList {
		doc.LocalSkips = doc.LocalSkips[:maxStatusList]
		doc.LocalSkipsTruncated = true
	}
	return doc
}

func tunnelsOf(c *route.Client) []tunnelConfig {
	ts := c.Tunnels()
	out := make([]tunnelConfig, 0, len(ts))
	for _, t := range ts {
		out = append(out, tunnelConfig{Peer: t.Peer, NextHop: t.NextHop})
	}
	return out
}

func vrfsOf(c *route.Client) []vrfConfig {
	vs := c.VRFs()
	out := make([]vrfConfig, 0, len(vs))
	for _, v := range vs {
		out = append(out, vrfConfig{ID: v.ID, ExportRTs: v.ExportRTs, ImportRTs: v.ImportRTs})
	}
	return out
}

type tunnelsDoc struct {
	Tunnels []tunnelConfig `json:"tunnels"`
}

type vrfsDoc struct {
	VRFs []vrfConfig `json:"vrfs"`
}

// rrsDoc is the GET/PUT /rrs body: the reflector set this device is working
// towards, plus the one thing a caller cannot infer from it -- whether the
// switch has actually finished. rr_wait is absent when nothing is in flight.
type rrsDoc struct {
	RRs    []wire.RRAddress `json:"rrs"`
	RRWait *route.RRWait    `json:"rr_wait,omitempty"`
}

func rrsOf(c *route.Client) rrsDoc {
	rrs := c.RRs()
	out := make([]wire.RRAddress, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, wire.RRAddress{ID: rr.ID, Addr: rr.Addr})
	}
	return rrsDoc{RRs: out, RRWait: c.Stats().RRWait}
}

// writeJSON is the response half of the seams below. Encoding errors are not
// reportable to the caller at that point, so log them: a silently short body
// would look like a device that has no routes.
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus exists because PUT /rrs answers 202, and a Content-Type set
// after WriteHeader is silently dropped.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("sr-route: encode status: %v", err)
	}
}

// decodeJSON reads a PUT body with the same strictness as the config file. That
// matters more here than it does for a file nobody can start the process
// without: a misspelled field would otherwise decode as "this table is empty",
// and these PUTs are whole replacements.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// statusMux is split out of serveStatus so the seams can be driven by httptest.
func statusMux(c *route.Client) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, statusFor(c))
	})
	// PUT replaces the dynamic local set (static local_prefixes are kept).
	// This is the reverse-import entry point: sr-frr watches the in-box FRR
	// for customer-learned BGP prefixes and publishes the current set here.
	mux.HandleFunc("/local-prefixes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, localPrefixesDoc{Prefixes: localPrefixes(c)})
		case http.MethodPut:
			var doc localPrefixesDoc
			if !decodeJSON(w, r, &doc) {
				return
			}
			lps := make([]route.LocalPrefix, 0, len(doc.Prefixes))
			for _, p := range doc.Prefixes {
				lps = append(lps, route.LocalPrefix{VRFID: p.VRFID, Prefix: p.Prefix})
			}
			if err := c.SetLocalPrefixes(lps); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.Printf("sr-route: 动态 local prefixes = %d 条", len(lps))
			writeJSON(w, localPrefixesDoc{Prefixes: localPrefixes(c)})
		default:
			http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		}
	})
	// PUT replaces the whole tunnel table -- the same message the RR is sent, so
	// a table posted here both re-resolves the routes already held in adj-RIBs-in
	// and tells the RR what it may reflect to us. Routes that were held back for
	// want of a path are installed by this call alone; no process restarts and no
	// prefix has to be announced twice. Omitting the key means an empty table,
	// which is a withdraw of every tunnel, so callers must send the array.
	mux.HandleFunc("/tunnels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, tunnelsDoc{Tunnels: tunnelsOf(c)})
		case http.MethodPut:
			var doc tunnelsDoc
			if !decodeJSON(w, r, &doc) {
				return
			}
			ts := make([]route.Tunnel, 0, len(doc.Tunnels))
			for _, t := range doc.Tunnels {
				ts = append(ts, route.Tunnel{Peer: t.Peer, NextHop: t.NextHop})
			}
			if err := c.SetTunnels(ts); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.Printf("sr-route: 隧道表 = %d 条（待决 %d 条）", len(c.Tunnels()), len(c.Pending()))
			writeJSON(w, tunnelsDoc{Tunnels: tunnelsOf(c)})
		default:
			http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		}
	})
	// PUT replaces the VRF table, which is half of every route decision: the
	// import RTs decide which received prefixes leave the pending list, and the
	// export RTs are stamped onto what we announce. A VRF dropped here therefore
	// withdraws its local prefixes rather than leaving them announced under an RT
	// policy nobody configured any more.
	mux.HandleFunc("/vrfs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, vrfsDoc{VRFs: vrfsOf(c)})
		case http.MethodPut:
			var doc vrfsDoc
			if !decodeJSON(w, r, &doc) {
				return
			}
			vs := make([]route.VRFConfig, 0, len(doc.VRFs))
			for _, v := range doc.VRFs {
				vs = append(vs, route.VRFConfig{ID: v.ID, ExportRTs: v.ExportRTs, ImportRTs: v.ImportRTs})
			}
			if err := c.SetVRFs(vs); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.Printf("sr-route: VPN 表 = %d 张（本地前缀 %d 条，未能宣告 %d 条）",
				len(c.VRFs()), len(c.LocalRoutes()), len(c.LocalSkips()))
			writeJSON(w, vrfsDoc{VRFs: vrfsOf(c)})
		default:
			http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		}
	})
	// PUT replaces the reflector set while this process runs -- the seam C-c exists
	// for, so 逐台换 RR never means restarting sr-route (and never means the RR
	// withdrawing our prefixes on the way, which is what a restart does).
	//
	// It answers 202, not 200: the switch is make-before-break, so a new session
	// has to be dialled, handshaken and EOR'd before the one it replaces may be
	// dropped, and sr-apply gives one hot update a 10-second budget
	// (internal/apply/route.go, routePushTimeout). Whether it finished is read
	// from rr_wait -- here and on every later GET -- not waited for in this call.
	mux.HandleFunc("/rrs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, rrsOf(c))
		case http.MethodPut:
			var doc rrsDoc
			if !decodeJSON(w, r, &doc) {
				return
			}
			rrs := make([]route.RRConfig, 0, len(doc.RRs))
			for _, rr := range doc.RRs {
				rrs = append(rrs, route.RRConfig{ID: rr.ID, Addr: rr.Addr})
			}
			if err := c.SetRRs(rrs); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			after := rrsOf(c)
			if after.RRWait != nil {
				log.Printf("sr-route: 换 RR 已开始，新 RR %v 还没成立，旧会话 %v 先保留（配置未完全生效）",
					after.RRWait.PendingAddrs, after.RRWait.RetiringAddrs)
			} else {
				log.Printf("sr-route: RR 表 = %d 条（旧会话已全部摘掉）", len(after.RRs))
			}
			writeJSONStatus(w, http.StatusAccepted, after)
		default:
			http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func serveStatus(addr string, c *route.Client) {
	log.Printf("sr-route: status on http://%s/status（运行时改表入口 PUT /tunnels、PUT /vrfs、PUT /local-prefixes、PUT /rrs）", addr)
	if err := http.ListenAndServe(addr, statusMux(c)); err != nil {
		log.Printf("sr-route: status server: %v", err)
	}
}
