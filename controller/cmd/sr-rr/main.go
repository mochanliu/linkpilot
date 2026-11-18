// sr-rr is one route-reflector instance. Two of them (one preferred, one
// standby) serve a tenant's CPEs, exactly like the "two RR neighbours" the CPE
// keeps warm.
//
// What it is NOT: a controller. It owns no device state, signs nothing and
// persists nothing. Its entire memory is the adj-rib-in of the sessions that
// are up right now -- if it restarts, every CPE reconnects and re-announces,
// and the table is rebuilt from the sources. That is why there is no database
// here and why a restart is safe.
//
// Identity: a session's peer is named by its certificate URI --
// srou://<tenant>/node/<id> for a CPE, srou://<tenant>/rr/<id> for another
// reflector. The tenant and the id used for routing come from that certificate,
// never from the hello frame -- the hello's device_id is only cross-checked
// against it, and a reflector's hello carries no device id at all.
//
// Reflection: an RR reflects. A route received from one client is advertised to
// every other session, unchanged, and a route is never sent back to the client
// that announced it. No next hop, RT or best path is applied here: the receiver
// resolves the origin in its own tunnel table and decides what to install, so a
// hub with a thousand tunnels is still one route per prefix on the wire.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"sdwan/controller/internal/ctrladdr"
	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/route"
)

type fileConfig struct {
	RRID       string            `json:"rr_id"`
	Tenant     string            `json:"tenant"`
	HoldSec    int               `json:"hold_sec"`
	Listen     string            `json:"listen"`
	StatusAddr string            `json:"status_addr"`
	TLS        tlsFiles          `json:"tls"`
	Controller *controllerConfig `json:"controller"`
}

// controllerConfig is optional: without it this RR only accepts sessions and
// its config file needs no change. With it the RR pulls its grid every
// poll_sec and dials the peers the controller lists.
type controllerConfig struct {
	Addr    string `json:"addr"`     // https://host:port of the device-facing API
	PollSec int    `json:"poll_sec"` // 0 = 30
}

type tlsFiles struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	CAFile   string `json:"ca_file"`
}

func loadConfig(path string) (*fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg fileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // a misspelled key must not silently become a default
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch {
	case cfg.RRID == "":
		return nil, errors.New("rr_id is required")
	case cfg.Tenant == "":
		return nil, errors.New("tenant is required")
	case cfg.Listen == "":
		return nil, errors.New("listen is required (host:port)")
	case cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" || cfg.TLS.CAFile == "":
		return nil, errors.New("tls.cert_file, tls.key_file and tls.ca_file are required (CPEs verify us against the tenant CA)")
	}
	if cfg.Controller != nil {
		u, err := url.Parse(cfg.Controller.Addr)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("controller.addr %q must be an https://host:port URL", cfg.Controller.Addr)
		}
		if cfg.Controller.PollSec != 0 && (cfg.Controller.PollSec < 3 || cfg.Controller.PollSec > 3600) {
			return nil, fmt.Errorf("controller.poll_sec must be 3..3600 (got %d)", cfg.Controller.PollSec)
		}
	}
	return &cfg, nil
}

// runtimeConf is what both boot paths converge to: the config-file path reads
// it out of JSON + PEM files, the single-file identity path out of one
// rr-identity.json downloaded from the console.
type runtimeConf struct {
	rrID       string
	tenant     string
	holdSec    int
	listen     string
	statusAddr string
	ctrlAddr   string // "" = inbound only (no controller configured)
	pollSec    int
	cert       tls.Certificate
	caPEM      []byte
	pool       *x509.CertPool
	stateDir   string // where the controller address book is persisted
}

func loadConfigFile(path, listenOverride, statusOverride string) (*runtimeConf, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.TLS.CAFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tls: %s has no usable certificate", cfg.TLS.CAFile)
	}
	rt := &runtimeConf{
		rrID: cfg.RRID, tenant: cfg.Tenant, holdSec: cfg.HoldSec,
		listen: cfg.Listen, statusAddr: cfg.StatusAddr, stateDir: filepath.Dir(path),
		cert: cert, caPEM: caPEM, pool: pool,
	}
	if cfg.Controller != nil {
		rt.ctrlAddr = cfg.Controller.Addr
		rt.pollSec = cfg.Controller.PollSec
	}
	applyOverrides(rt, listenOverride, statusOverride)
	return rt, nil
}

// identityFile is the shape the console's「下载 rr-identity.json」produces.
// It is deliberately lenient about unknown keys (fingerprint, serial,
// issued_at_unix are informational), because the console owns that file and
// may add fields; sr-rr only needs the five below plus controller_addr.
type identityFile struct {
	RRID           string `json:"rr_id"`
	Tenant         string `json:"tenant"`
	Addr           string `json:"addr"` // this RR's own advertised host:port
	ControllerAddr string `json:"controller_addr"`
	CertPEM        string `json:"cert_pem"`
	KeyPEM         string `json:"key_pem"`
	CAPEM          string `json:"ca_pem"`
}

// loadIdentity boots from ONE file: certificate, key, CA and (optionally) the
// controller seed are all inside it. This is the answer to "installing an RR
// means serial-console-copying three files and hand-writing a config": the
// console hands out exactly one file, and the box needs exactly one command.
func loadIdentity(path, listenOverride, statusOverride string) (*runtimeConf, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var id identityFile
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch {
	case id.CertPEM == "" || id.KeyPEM == "":
		return nil, errors.New("cert_pem and key_pem are required")
	case id.CAPEM == "":
		return nil, errors.New("ca_pem is required (CPEs verify us against the tenant CA)")
	}
	if id.ControllerAddr != "" {
		u, perr := url.Parse(id.ControllerAddr)
		if perr != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("controller_addr %q must be an https://host:port URL", id.ControllerAddr)
		}
	}
	cert, err := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(id.CAPEM)) {
		return nil, errors.New("ca_pem has no usable certificate")
	}
	// The certificate is the authority on who this box is; the file's rr_id /
	// tenant, when present, may only confirm it. A mismatch means the file was
	// edited by hand, and edits must be loud, not silently obeyed.
	leaf, lerr := x509.ParseCertificate(cert.Certificate[0])
	if lerr != nil {
		return nil, fmt.Errorf("tls: %w", lerr)
	}
	self, refErr := keybinding.RefOf(leaf)
	if refErr == nil && self.Role == keybinding.RoleRR {
		if id.RRID != "" && id.RRID != self.RRID {
			return nil, fmt.Errorf("identity file says rr_id %q but its certificate is %q", id.RRID, self.RRID)
		}
		if id.Tenant != "" && id.Tenant != self.Tenant {
			return nil, fmt.Errorf("identity file says tenant %q but its certificate is %q", id.Tenant, self.Tenant)
		}
		id.RRID, id.Tenant = self.RRID, self.Tenant
	}
	if id.RRID == "" || id.Tenant == "" {
		return nil, errors.New("cannot derive rr_id/tenant from the certificate; the file needs them (or a srou://…/rr/<id> certificate)")
	}
	// listen comes from the file's own advertised address: its host part is
	// what other boxes dial, which may not be bindable here (a NAT address, an
	// IP from another interface), so bind on every interface at that port.
	listen := ""
	if id.Addr != "" {
		_, port, perr := net.SplitHostPort(id.Addr)
		if perr != nil {
			return nil, fmt.Errorf("addr %q is not host:port", id.Addr)
		}
		listen = ":" + port
	}
	if listen == "" && listenOverride == "" {
		return nil, errors.New("no listen address: the file's addr is empty, pass -listen host:port")
	}
	rt := &runtimeConf{
		rrID: id.RRID, tenant: id.Tenant,
		listen: listen, stateDir: filepath.Dir(path),
		cert: cert, caPEM: []byte(id.CAPEM), pool: pool,
	}
	if id.ControllerAddr != "" {
		rt.ctrlAddr = id.ControllerAddr
	}
	applyOverrides(rt, listenOverride, statusOverride)
	return rt, nil
}

func applyOverrides(rt *runtimeConf, listenOverride, statusOverride string) {
	if listenOverride != "" {
		rt.listen = listenOverride
	}
	if statusOverride != "" {
		rt.statusAddr = statusOverride
	}
}

func main() {
	configPath := flag.String("config", "", "path to the RR config JSON")
	identityPath := flag.String("identity", "", "path to a single rr-identity.json downloaded from the console (replaces -config entirely)")
	listenFlag := flag.String("listen", "", "override the listen address (host:port)")
	statusFlag := flag.String("status", "", "override the status address (host:port; empty keeps the file's value)")
	flag.Parse()
	if *identityPath != "" && *configPath != "" {
		log.Fatal("-identity and -config are alternatives; pass one, not both")
	}
	if *identityPath == "" && *configPath == "" {
		log.Fatal("-config or -identity is required")
	}
	var (
		rt  *runtimeConf
		err error
	)
	if *identityPath != "" {
		rt, err = loadIdentity(*identityPath, *listenFlag, *statusFlag)
	} else {
		rt, err = loadConfigFile(*configPath, *listenFlag, *statusFlag)
	}
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ref, err := route.NewReflector(route.ReflectorConfig{
		RRID:    rt.rrID,
		Tenant:  rt.tenant,
		HoldSec: rt.holdSec,
	})
	if err != nil {
		log.Fatalf("reflector: %v", err)
	}

	// The certificate is this RR's name on the wire. A box that joins the grid
	// must present the identity its config claims, or it would ask the
	// controller for one RR's peers and open sessions as another.
	leaf, err := x509.ParseCertificate(rt.cert.Certificate[0])
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	self, refErr := keybinding.RefOf(leaf)
	if rt.ctrlAddr != "" {
		if refErr != nil {
			log.Fatalf("identity: %v (controller is configured, so this RR needs srou://%s/rr/<id>)", refErr, rt.tenant)
		}
		if self.Role != keybinding.RoleRR || self.Tenant != rt.tenant || self.RRID != rt.rrID {
			log.Fatalf("identity: certificate is %s but config says tenant %q rr_id %q", self, rt.tenant, rt.rrID)
		}
	} else if refErr != nil || self.Role != keybinding.RoleRR {
		// An inbound-only RR never has to name itself to anyone, so a hand-made
		// certificate without a role URI keeps working -- it just cannot join a
		// grid until it is re-issued.
		log.Printf("sr-rr %s: certificate has no srou://.../rr/ identity; incoming sessions only", rt.rrID)
	}

	// RequireAndVerifyClientCert: a CPE that cannot prove a tenant-CA identity
	// must not even complete the handshake. Identity is read from this
	// certificate below, so "verified" here is what makes PeerIdentity trusted.
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{rt.cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    rt.pool,
	}
	ln, err := tls.Listen("tcp", rt.listen, tlsCfg)
	if err != nil {
		log.Fatalf("listen %s: %v", rt.listen, err)
	}
	log.Printf("sr-rr %s: tenant %s listening on %s (hold=%ds)", rt.rrID, rt.tenant, rt.listen, rt.holdSec)

	var grid *mesh
	if rt.ctrlAddr != "" {
		poll := 30 * time.Second
		if rt.pollSec > 0 {
			poll = time.Duration(rt.pollSec) * time.Second
		}
		base := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{rt.cert},
			RootCAs:      rt.pool,
		}
		// 种子是身份/配置里的 controller.addr（一个地址）；连上之后全表由控制器带回。
		// 落盘放在启动文件旁边、按 rr_id 分开：同一台机器上两个 RR 各有各的
		// 候选表，一份文件写两份会互相覆盖。
		book, berr := ctrladdr.Load(rt.ctrlAddr,
			filepath.Join(rt.stateDir, "ctrl-addrs-"+rt.rrID+".json"))
		if berr != nil {
			log.Printf("sr-rr %s: controller address book: %v (starting from %s)", rt.rrID, berr, rt.ctrlAddr)
		}
		if cur := book.Current(); cur != ctrladdr.Norm(rt.ctrlAddr) {
			log.Printf("sr-rr %s: resuming on %s (%d candidate(s), seed %s)",
				rt.rrID, cur, book.Size(), book.Seed())
		}
		grid = newMesh(meshParams{
			book:    book,
			poll:    poll,
			tenant:  rt.tenant,
			rrID:    rt.rrID,
			tlsBase: base,
			hc:      &http.Client{Transport: &http.Transport{TLSClientConfig: base.Clone()}},
			ref:     ref,
			logf:    log.Printf,
		})
		log.Printf("sr-rr %s: grid from %s, poll %s", rt.rrID, book.Current(), poll)
		go func() {
			if err := grid.run(context.Background()); err != nil {
				// 404 means the row is gone; 401/403 mean this certificate is not
				// accepted. Either way serving on is worse than exiting: the
				// supervisor gets to see it instead of a silent ghost RR.
				log.Printf("sr-rr %s: %v -- stopping", rt.rrID, err)
				ln.Close()
				os.Exit(1)
			}
		}()
	}

	if rt.statusAddr != "" {
		go serveStatus(rt.statusAddr, rt, ref, grid)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("sr-rr %s: shutting down", rt.rrID)
		ln.Close()
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		go serveConn(conn.(*tls.Conn), rt.tenant, rt.rrID, ref)
	}
}

func serveConn(conn *tls.Conn, tenant, rrID string, ref *route.Reflector) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := conn.Handshake(); err != nil {
		log.Printf("%s: TLS handshake from %s failed: %v", rrID, conn.RemoteAddr(), err)
		return
	}
	conn.SetDeadline(time.Time{})
	peers := conn.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		// Cannot happen with RequireAndVerifyClientCert, but the identity we
		// route on must never be guessed.
		log.Printf("%s: %s presented no client certificate", rrID, conn.RemoteAddr())
		return
	}
	peer, err := keybinding.RefOf(peers[0])
	if err != nil {
		log.Printf("%s: %s: %v", rrID, conn.RemoteAddr(), err)
		return
	}
	id, err := sessionIdentity(peer)
	if err != nil {
		log.Printf("%s: %s: %v", rrID, conn.RemoteAddr(), err)
		return
	}
	start := time.Now()
	err = ref.ServeConn(conn, id)
	log.Printf("%s: %s (%s) session ended after %s: %v",
		rrID, peer, conn.RemoteAddr(), time.Since(start).Round(time.Millisecond), err)
}

// sessionIdentity maps the identity a certificate asserts onto the session it
// may open. The two roles are not interchangeable: a node certificate opens a
// client session and a reflector certificate opens an rr session, and neither
// can be used to claim the other.
func sessionIdentity(peer keybinding.Ref) (route.PeerIdentity, error) {
	switch peer.Role {
	case keybinding.RoleNode:
		return route.PeerIdentity{Tenant: peer.Tenant, Role: route.RoleClient, DeviceID: peer.NodeID}, nil
	case keybinding.RoleRR:
		return route.PeerIdentity{Tenant: peer.Tenant, Role: route.RoleRR, RRID: peer.RRID}, nil
	default:
		return route.PeerIdentity{}, fmt.Errorf("certificate role %q cannot open a route session", peer.Role)
	}
}

func serveStatus(addr string, rt *runtimeConf, ref *route.Reflector, grid *mesh) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		counts := map[string]int{}
		for dev, routes := range ref.Routes() {
			counts[strconv.Itoa(int(dev))] = len(routes)
		}
		peers := []peerStatus{}
		if grid != nil {
			peers = grid.Status()
		}
		writeJSON(w, struct {
			RRID     string              `json:"rr_id"`
			Tenant   string              `json:"tenant"`
			Sessions []route.SessionInfo `json:"sessions"`
			Routes   map[string]int      `json:"routes"`
			Peers    []peerStatus        `json:"peers"`
		}{rt.rrID, rt.tenant, ref.Sessions(), counts, peers})
	})
	mux.HandleFunc("/routes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		out := map[string][]route.Route{}
		for dev, routes := range ref.Routes() {
			out[strconv.Itoa(int(dev))] = routes
		}
		writeJSON(w, out)
	})
	log.Printf("sr-rr %s: status on http://%s/status", rt.rrID, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("sr-rr %s: status server: %v", rt.rrID, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
