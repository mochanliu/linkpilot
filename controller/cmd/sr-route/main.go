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
	OverlayAddrs  []string         `json:"overlay_addrs"`
	VRFs          []vrfConfig      `json:"vrfs"`
	LocalPrefixes []prefixConfig   `json:"local_prefixes"`
	StatusAddr    string           `json:"status_addr"`
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
		Tenant:       cfg.Tenant,
		DeviceID:     cfg.DeviceID,
		HoldSec:      cfg.HoldSec,
		RRs:          make([]route.RRConfig, 0, len(cfg.RRs)),
		OverlayAddrs: cfg.OverlayAddrs,
		Dial:         dial,
		Logf:         log.Printf,
	}
	for _, rr := range cfg.RRs {
		clientCfg.RRs = append(clientCfg.RRs, route.RRConfig{ID: rr.ID, Addr: rr.Addr})
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
	log.Printf("sr-route: device %d (tenant %s), %d local paths, %d RRs",
		cfg.DeviceID, cfg.Tenant, len(c.LocalRoutes()), len(cfg.RRs))

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
	return fmt.Sprintf("active=%d sessions[%v] rib{%s}", st.ActiveRR, synced, strings.Join(parts, " "))
}

func serveStatus(addr string, c *route.Client) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		all := c.AllRIB()
		rib := make(map[string][]route.Route, len(all))
		for vrf, routes := range all {
			rib[strconv.Itoa(int(vrf))] = routes
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			route.ClientStats
			RIB map[string][]route.Route `json:"rib"`
		}{c.Stats(), rib})
	})
	log.Printf("sr-route: status on http://%s/status", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("sr-route: status server: %v", err)
	}
}
