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
// Identity: CPEs authenticate with the device certificate the controller
// issued. The tenant and device id used for routing come from that certificate
// (srou://<tenant>/node/<id>), never from the hello frame -- the hello's
// device_id is only cross-checked against it.
//
// Reflection: a prefix carries no next hop. Each CPE announces its tunnel table
// (peer device -> next hop) separately, the RR keeps the mutual edges, and a
// prefix is sent only to peers that have a tunnel to its origin. The receiver
// looks the next hop up in its own table, so a hub with a thousand tunnels is
// one small message to the RR instead of a thousand next hops per prefix.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/route"
)

type fileConfig struct {
	RRID       string              `json:"rr_id"`
	Tenant     string              `json:"tenant"`
	HoldSec    int                 `json:"hold_sec"`
	Listen     string              `json:"listen"`
	StatusAddr string              `json:"status_addr"`
	TLS        tlsFiles            `json:"tls"`
	ReflectTo  map[string][]uint16 `json:"reflect_to"`
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
	return &cfg, nil
}

// parseDeviceMap turns the JSON string keys ("1", "2") into device ids. JSON
// has no integer keys, and accepting "01" or " 1" as the same device would
// make two configs that look different address the same node.
func parseDeviceMap(name string, in map[string][]uint16) (map[uint16][]uint16, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[uint16][]uint16, len(in))
	for k, v := range in {
		n, err := strconv.ParseUint(k, 10, 16)
		if err != nil || strconv.FormatUint(n, 10) != k {
			return nil, fmt.Errorf("%s: key %q is not a canonical device id", name, k)
		}
		if n == 0 {
			return nil, fmt.Errorf("%s: device 0 is reserved", name)
		}
		seen := map[uint16]bool{}
		list := make([]uint16, 0, len(v))
		for _, d := range v {
			if d == 0 {
				return nil, fmt.Errorf("%s[%d]: device 0 is reserved", name, n)
			}
			if seen[d] {
				return nil, fmt.Errorf("%s[%d]: device %d listed twice", name, n, d)
			}
			seen[d] = true
			list = append(list, d)
		}
		out[uint16(n)] = list
	}
	return out, nil
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
	reflectTo, err := parseDeviceMap("reflect_to", cfg.ReflectTo)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ref, err := route.NewReflector(route.ReflectorConfig{
		RRID:      cfg.RRID,
		Tenant:    cfg.Tenant,
		HoldSec:   cfg.HoldSec,
		ReflectTo: reflectTo,
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

	if cfg.StatusAddr != "" {
		go serveStatus(cfg.StatusAddr, cfg, ref)
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
	peerTenant, deviceID, err := keybinding.IdentityOf(peers[0])
	if err != nil {
		log.Printf("%s: %s: %v", rrID, conn.RemoteAddr(), err)
		return
	}
	start := time.Now()
	err = ref.ServeConn(conn, route.PeerIdentity{Tenant: peerTenant, DeviceID: deviceID})
	log.Printf("%s: device %d (%s) session ended after %s: %v",
		rrID, deviceID, conn.RemoteAddr(), time.Since(start).Round(time.Millisecond), err)
}

func serveStatus(addr string, cfg *fileConfig, ref *route.Reflector) {
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
		tunnelCounts := map[string]int{}
		for dev, table := range ref.Tunnels() {
			tunnelCounts[strconv.Itoa(int(dev))] = len(table)
		}
		writeJSON(w, struct {
			RRID     string              `json:"rr_id"`
			Tenant   string              `json:"tenant"`
			Sessions []route.SessionInfo `json:"sessions"`
			Routes   map[string]int      `json:"routes"`
			Tunnels  map[string]int      `json:"tunnels"`
		}{cfg.RRID, cfg.Tenant, ref.Sessions(), counts, tunnelCounts})
	})
	mux.HandleFunc("/tunnels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		out := map[string][]route.Tunnel{}
		for dev, table := range ref.Tunnels() {
			out[strconv.Itoa(int(dev))] = table
		}
		writeJSON(w, out)
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
