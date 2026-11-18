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

func main() {
	configPath := flag.String("config", "", "path to the RR config JSON")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("-config is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ref, err := route.NewReflector(route.ReflectorConfig{
		RRID:    cfg.RRID,
		Tenant:  cfg.Tenant,
		HoldSec: cfg.HoldSec,
	})
	if err != nil {
		log.Fatalf("reflector: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	caPEM, err := os.ReadFile(cfg.TLS.CAFile)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Fatalf("tls: %s has no usable certificate", cfg.TLS.CAFile)
	}

	// The certificate is this RR's name on the wire. A box that joins the grid
	// must present the identity its config claims, or it would ask the
	// controller for one RR's peers and open sessions as another.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	self, refErr := keybinding.RefOf(leaf)
	if cfg.Controller != nil {
		if refErr != nil {
			log.Fatalf("identity: %v (controller is configured, so this RR needs srou://%s/rr/<id>)", refErr, cfg.Tenant)
		}
		if self.Role != keybinding.RoleRR || self.Tenant != cfg.Tenant || self.RRID != cfg.RRID {
			log.Fatalf("identity: certificate is %s but config says tenant %q rr_id %q", self, cfg.Tenant, cfg.RRID)
		}
	} else if refErr != nil || self.Role != keybinding.RoleRR {
		// An inbound-only RR never has to name itself to anyone, so a hand-made
		// certificate without a role URI keeps working -- it just cannot join a
		// grid until it is re-issued.
		log.Printf("sr-rr %s: certificate has no srou://.../rr/ identity; incoming sessions only", cfg.RRID)
	}

	// RequireAndVerifyClientCert: a CPE that cannot prove a tenant-CA identity
	// must not even complete the handshake. Identity is read from this
	// certificate below, so "verified" here is what makes PeerIdentity trusted.
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	ln, err := tls.Listen("tcp", cfg.Listen, tlsCfg)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}
	log.Printf("sr-rr %s: tenant %s listening on %s (hold=%ds)", cfg.RRID, cfg.Tenant, cfg.Listen, cfg.HoldSec)

	var grid *mesh
	if cfg.Controller != nil {
		poll := 30 * time.Second
		if cfg.Controller.PollSec > 0 {
			poll = time.Duration(cfg.Controller.PollSec) * time.Second
		}
		base := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
		}
		// 种子是配置里的 controller.addr（一个地址）；连上之后全表由控制器带回。
		// 落盘放在配置文件旁边、按 rr_id 分开：同一台机器上两个 RR 各有各的
		// 候选表，一份文件写两份会互相覆盖。
		book, berr := ctrladdr.Load(cfg.Controller.Addr,
			filepath.Join(filepath.Dir(*configPath), "ctrl-addrs-"+cfg.RRID+".json"))
		if berr != nil {
			log.Printf("sr-rr %s: controller address book: %v (starting from %s)", cfg.RRID, berr, cfg.Controller.Addr)
		}
		if cur := book.Current(); cur != ctrladdr.Norm(cfg.Controller.Addr) {
			log.Printf("sr-rr %s: resuming on %s (%d candidate(s), seed %s)",
				cfg.RRID, cur, book.Size(), book.Seed())
		}
		grid = newMesh(meshParams{
			book:    book,
			poll:    poll,
			tenant:  cfg.Tenant,
			rrID:    cfg.RRID,
			tlsBase: base,
			hc:      &http.Client{Transport: &http.Transport{TLSClientConfig: base.Clone()}},
			ref:     ref,
			logf:    log.Printf,
		})
		log.Printf("sr-rr %s: grid from %s, poll %s", cfg.RRID, book.Current(), poll)
		go func() {
			if err := grid.run(context.Background()); err != nil {
				// 404 means the row is gone; 401/403 mean this certificate is not
				// accepted. Either way serving on is worse than exiting: the
				// supervisor gets to see it instead of a silent ghost RR.
				log.Printf("sr-rr %s: %v -- stopping", cfg.RRID, err)
				ln.Close()
				os.Exit(1)
			}
		}()
	}

	if cfg.StatusAddr != "" {
		go serveStatus(cfg.StatusAddr, cfg, ref, grid)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("sr-rr %s: shutting down", cfg.RRID)
		ln.Close()
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		go serveConn(conn.(*tls.Conn), cfg.Tenant, cfg.RRID, ref)
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

func serveStatus(addr string, cfg *fileConfig, ref *route.Reflector, grid *mesh) {
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
		}{cfg.RRID, cfg.Tenant, ref.Sessions(), counts, peers})
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
	log.Printf("sr-rr %s: status on http://%s/status", cfg.RRID, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("sr-rr %s: status server: %v", cfg.RRID, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
