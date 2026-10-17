// srou controller — M-C0 skeleton.
//
// What this is: a working cloud-controller core good enough to prove the
// three mechanisms the product depends on, against real NATs:
//
//  1. zero-touch enrollment (claim code -> device cert -> tenant identity)
//  2. the endpoint registry / rendezvous table (who is where, pushed on change)
//  3. STUN-style mapping discovery (reflector) feeding the above
//
// What this deliberately is NOT yet (each is a named follow-up, doc 11 §6):
//
//	F1 Postgres      — file-backed store behind the Store interface below
//	F2 gRPC          — HTTP+JSON over mTLS; wire types are transport-agnostic
//	F7 epoch counter — wall clock + persisted monotonic max for now; moves to
//	                   the DB so two controller instances cannot diverge
//
// Design rules carried over from the architecture docs (do not "fix" these
// without reading 10-Controller-Plane-Architecture.md first):
//
//   - The controller is a TRUTH PROVIDER, not a decision maker. It never
//     tells an edge which path to use; it publishes what it knows.
//   - Controller outage must not break the dataplane. Nothing here is in the
//     packet path; agents keep forwarding on last-known state.
//   - Tenant identity comes from the CLIENT CERTIFICATE, never from a request
//     parameter. That is what makes the four isolation layers real rather
//     than aspirational.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/wire"
)

// ------------------------------------------------------------------ state

// NodeRecord is one enrolled device. Keyed by "tenant|nodeID".
type NodeRecord struct {
	TenantID   string `json:"tenant_id"`
	NodeID     uint16 `json:"node_id"`
	Serial     string `json:"serial"`
	PublicKey  string `json:"public_key"` // base64 ed25519
	CertPEM    string `json:"cert_pem"`
	AgentVer   string `json:"agent_version"`
	UDPMapping string `json:"udp_mapping"` // as OBSERVED BY THE REFLECTOR
	LocalPort  int    `json:"local_port"`
	Symmetric  bool   `json:"symmetric_nat"`
	Reflector  string `json:"reflector"`
	Reach      string `json:"reach"`
	LastSeen   int64  `json:"last_seen_unix"`
	EnrolledAt int64  `json:"enrolled_at_unix"`
	Stale      bool   `json:"stale"`

	// SrouIP is this node's LABEL address, assigned at enrollment from the
	// tenant's pool. Persisted so it is stable across restarts: a label that
	// changes would invalidate every peer's already-compiled paths.
	SrouIP string `json:"srou_ip"`

	// Binding is the device's attested X25519 key (verified before storage --
	// see applyBinding). The controller stores and relays it; it can check it
	// but it can never produce one, because it does not hold the device's
	// identity key.
	Binding *keybinding.Binding `json:"key_binding,omitempty"`
	KeyGen  uint32              `json:"key_gen,omitempty"`
}

// State is the whole persistent world. In production these become rows in
// Postgres; the shape is deliberately flat so the mapping is trivial.
type State struct {
	Nodes      map[string]*NodeRecord `json:"nodes"`
	NextNodeID map[string]uint16      `json:"next_node_id"` // per tenant
	Epoch      uint32                 `json:"epoch"`        // monotonic, survives restart
	Claims     map[string]string      `json:"claims"`       // claim_code -> tenant_id
	// Roots holds each tenant's E_root (hex, 16 bytes). This is the DOMAIN key:
	// the controller issues it because every node has to verify every other
	// node's hop MAC, so it cannot be per-pair. The per-pair O-key is the one
	// the controller must never see, and it is derived by the endpoints, not
	// here -- note this file contains no code that could compute one.
	Roots map[string]string `json:"tenant_roots"`
	// TenantIndex gives each tenant a stable small number, used to place its
	// label addresses in their own pool range.
	TenantIndex map[string]uint8 `json:"tenant_index"`
	NextTenant  uint8            `json:"next_tenant"`
}

// Store is the persistence seam. F1 (Postgres) implements this and nothing
// above it changes.
type Store interface {
	Load() (*State, error)
	Save(*State) error
}

type fileStore struct{ path string }

func (f *fileStore) Load() (*State, error) {
	b, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return &State{
			Nodes:       map[string]*NodeRecord{},
			NextNodeID:  map[string]uint16{},
			Roots:       map[string]string{},
			TenantIndex: map[string]uint8{},
			// Seeded so the harness proves multi-tenancy out of the box.
			Claims: map[string]string{
				"claim-a": "tenant-a",
				"claim-b": "tenant-a",
				"claim-c": "tenant-b",
			},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Nodes == nil {
		s.Nodes = map[string]*NodeRecord{}
	}
	if s.NextNodeID == nil {
		s.NextNodeID = map[string]uint16{}
	}
	if s.Claims == nil {
		s.Claims = map[string]string{}
	}
	if s.Roots == nil {
		s.Roots = map[string]string{}
	}
	if s.TenantIndex == nil {
		s.TenantIndex = map[string]uint8{}
	}
	return &s, nil
}

func (f *fileStore) Save(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path) // atomic: a crash never leaves a torn file
}

var _ Store = (*fileStore)(nil) // compile-time guard on the seam

// ------------------------------------------------------------------ server

type subscriber struct {
	tenant string
	nodeID uint16
	ch     chan wire.WatchEvent
}

type server struct {
	mu       sync.Mutex
	state    *State
	store    Store
	subs     map[int]*subscriber
	nextSub  int
	hbSec    int
	tlsAddrs []string
	udpAddrs []string

	caCert  *x509.Certificate
	caKey   ed25519.PrivateKey
	dataDir string

	// opcodeBase is the tenant-wide instruction-port base (doc 23). Pushed to
	// every node rather than configured per node, so a label can be composed
	// from a peer's label address alone.
	opcodeBase uint16
}

func (s *server) save() {
	if err := s.store.Save(s.state); err != nil {
		log.Printf("store save failed: %v", err)
	}
}

func (s *server) epoch() uint32 { return s.state.Epoch }

// advanceEpoch bumps the controller-assigned epoch counter. Devices never
// compute epochs themselves — that removes an entire class of clock-skew
// bugs (including the one found in the POC's license logic).
func (s *server) advanceEpoch() uint32 {
	now := uint32(time.Now().Unix() / 60) // one epoch per minute
	if now > s.state.Epoch {
		s.state.Epoch = now
	}
	return s.state.Epoch
}

// ---------------------------------------------------------------- TLS / PKI

func newSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
}

func loadOrCreateCA(dir string) (*x509.Certificate, ed25519.PrivateKey, *x509.CertPool, error) {
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	if _, err := os.Stat(certPath); err == nil {
		cb, err := os.ReadFile(certPath)
		if err != nil {
			return nil, nil, nil, err
		}
		kb, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, nil, err
		}
		cblk, _ := pem.Decode(cb)
		cert, err := x509.ParseCertificate(cblk.Bytes)
		if err != nil {
			return nil, nil, nil, err
		}
		kblk, _ := pem.Decode(kb)
		key, err := x509.ParsePKCS8PrivateKey(kblk.Bytes)
		if err != nil {
			return nil, nil, nil, err
		}
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		return cert, key.(ed25519.PrivateKey), pool, nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "srou-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return nil, nil, nil, err
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(priv)
	if err := writePEM(keyPath, "PRIVATE KEY", kb, 0600); err != nil {
		return nil, nil, nil, err
	}
	log.Printf("generated new CA at %s", certPath)

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return cert, priv, pool, nil
}

// loadOrCreateServerCert issues the controller's own certificate. The SAN
// list is what agents validate, so every HA address must appear in it.
func loadOrCreateServerCert(dir string, ca *x509.Certificate, caKey ed25519.PrivateKey, sans []string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")

	if _, err := os.Stat(certPath); err == nil {
		return tls.LoadX509KeyPair(certPath, keyPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: "srou-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, san)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return tls.Certificate{}, err
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(priv)
	if err := writePEM(keyPath, "PRIVATE KEY", kb, 0600); err != nil {
		return tls.Certificate{}, err
	}
	log.Printf("generated server certificate (SANs: %s)", strings.Join(sans, ", "))
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// signDevice issues the device certificate. Tenant + node ID live in a URI
// SAN, so the certificate itself is the authorization: no request parameter
// can ever claim a different tenant.
func signDevice(ca *x509.Certificate, caKey ed25519.PrivateKey, pub ed25519.PublicKey, tenant string, nodeID uint16) (string, error) {
	u, err := url.Parse(fmt.Sprintf("srou://%s/node/%d", tenant, nodeID))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s-node-%d", tenant, nodeID)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// peerTenantFromCert is the ONLY place a caller's tenant identity is decided.
func peerTenantFromCert(r *http.Request) (tenant string, nodeID uint16, err error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", 0, fmt.Errorf("no client certificate")
	}
	// Parsing lives in keybinding so the controller and the devices read
	// identity out of a certificate the same way. Two implementations of "what
	// does this certificate claim" is how the two sides end up disagreeing.
	t, n, err := keybinding.IdentityOf(r.TLS.PeerCertificates[0])
	if err != nil {
		return "", 0, err
	}
	return t, n, nil
}

// ------------------------------------------------------------------ handlers

func (s *server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	// Enrollment is the ONE endpoint that accepts an unauthenticated caller:
	// the device has no certificate yet. The claim code is the authorization.
	var req wire.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.ProtocolVer != wire.ProtocolVersion {
		http.Error(w, fmt.Sprintf("protocol version %d unsupported", req.ProtocolVer), 400)
		return
	}
	if req.Serial == "" {
		http.Error(w, "serial required", 400)
		return
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		http.Error(w, "public_key must be base64 ed25519", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tenant, ok := s.state.Claims[req.ClaimCode]
	if !ok {
		log.Printf("enroll REJECTED serial=%s (unknown claim code)", req.Serial)
		http.Error(w, "invalid claim code", 403)
		return
	}

	// Idempotent enrollment: a device that already has an identity keeps it.
	// Re-enrollment after a factory reset is an explicit admin action, not
	// something a network retry can trigger silently.
	var rec *NodeRecord
	for _, n := range s.state.Nodes {
		if n.TenantID == tenant && n.Serial == req.Serial {
			rec = n
			break
		}
	}
	if rec == nil {
		id := s.state.NextNodeID[tenant]
		if id == 0 {
			id = 1
		}
		s.state.NextNodeID[tenant] = id + 1
		rec = &NodeRecord{
			TenantID:   tenant,
			NodeID:     id,
			Serial:     req.Serial,
			EnrolledAt: time.Now().Unix(),
			Reach:      "unknown",
		}
		s.state.Nodes[key(tenant, id)] = rec
		log.Printf("enroll: serial=%s -> tenant=%s node=%d", req.Serial, tenant, id)
	} else {
		log.Printf("enroll: serial=%s re-enrolled as tenant=%s node=%d", req.Serial, tenant, rec.NodeID)
	}
	rec.PublicKey = base64.StdEncoding.EncodeToString(pub)
	rec.AgentVer = req.AgentVersion
	if rec.SrouIP == "" {
		// Assigned once and kept. A label address that moved would invalidate
		// every peer's already-compiled paths, so re-enrollment must not move it.
		rec.SrouIP = s.assignSrouIP(tenant, rec.NodeID)
	}

	certPEM, err := signDevice(s.caCert, s.caKey, ed25519.PublicKey(pub), tenant, rec.NodeID)
	if err != nil {
		http.Error(w, "signing failed", 500)
		return
	}
	rec.CertPEM = certPEM
	caPEM, _ := os.ReadFile(filepath.Join(s.dataDir, "ca.pem"))
	root := s.tenantRoot(tenant)
	s.save()

	json.NewEncoder(w).Encode(wire.EnrollResponse{
		TenantID:     tenant,
		NodeID:       rec.NodeID,
		CertPEM:      certPEM,
		CAPEM:        string(caPEM),
		TLSAddrs:     s.tlsAddrs,
		UDPReflector: s.udpAddrs,
		HeartbeatSec: s.hbSec,
		Epoch:        s.advanceEpoch(),
		RootKey:      root,
		OpcodeBase:   s.opcodeBase,
		SrouIP:       rec.SrouIP,
	})
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	var req wire.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.NodeID != nodeID {
		// A certificate for node 5 may not heartbeat as node 7 — and may not
		// touch another tenant's rows at all.
		http.Error(w, "node id does not match certificate", 403)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.state.Nodes[key(tenant, nodeID)]
	if !ok {
		http.Error(w, "unknown node", 404)
		return
	}
	wasStale := rec.Stale
	rec.LastSeen = time.Now().Unix()
	rec.Stale = false
	if req.UDPMapping != "" {
		rec.UDPMapping = req.UDPMapping
	}
	var bindingErr string
	keyChanged, err := s.applyBinding(rec, req.Binding)
	if err != nil {
		// NON-FATAL on purpose: the node is still a valid rendezvous endpoint,
		// and dropping it would turn "your key material was rejected" into
		// "you vanished". The agent gets a named reason and surfaces it.
		bindingErr = err.Error()
		log.Printf("binding REJECTED tenant=%s node=%d: %v", tenant, nodeID, err)
	}
	s.save()

	// Coming back from stale, or offering key material peers have not seen, are
	// both events peers need: their view of this node just changed.
	if wasStale || keyChanged {
		s.broadcastLocked(tenant, wire.WatchEvent{Type: "update", Peer: toPeerInfo(rec, false), SentAt: time.Now().Unix()})
	}
	json.NewEncoder(w).Encode(wire.HeartbeatResponse{
		Epoch:      s.advanceEpoch(),
		PeerCount:  len(s.peersLocked(tenant, nodeID)),
		BindingErr: bindingErr,
	})
}

func (s *server) handleMapping(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	var rep wire.MappingReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	s.mu.Lock()
	rec, ok := s.state.Nodes[key(tenant, nodeID)]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown node", 404)
		return
	}
	changed := rec.UDPMapping != rep.UDPMapping
	rec.UDPMapping = rep.UDPMapping
	rec.LocalPort = rep.LocalPort
	rec.Symmetric = rep.SymmetricNAT
	rec.Reflector = rep.Reflector
	rec.LastSeen = time.Now().Unix()
	rec.Reach = "unknown"

	var bindingErr string
	keyChanged, err := s.applyBinding(rec, rep.Binding)
	if err != nil {
		bindingErr = err.Error()
		log.Printf("binding REJECTED tenant=%s node=%d: %v", tenant, nodeID, err)
	} else if keyChanged {
		// A new binding is itself a reason to push. Peers must derive the new
		// O-key BEFORE they try to talk; discovering it by packet loss would
		// make a rotation look like a link failure.
		changed = true
		log.Printf("pushing new key binding for %s/%d to peers", tenant, nodeID)
	}
	s.save()

	if changed {
		// This is the mechanism that answers "the CPE's public IP changed":
		// the registry notices and pushes. No dataplane change is needed.
		log.Printf("mapping CHANGED tenant=%s node=%d -> %q (local_port=%d symmetric=%v)",
			tenant, nodeID, rep.UDPMapping, rep.LocalPort, rep.SymmetricNAT)
		s.broadcastLocked(tenant, wire.WatchEvent{Type: "update", Peer: toPeerInfo(rec, false), SentAt: time.Now().Unix()})
	}
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "binding_err": bindingErr})
}

func (s *server) handleReach(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	var rep wire.ReachReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.state.Nodes[key(tenant, nodeID)]; ok {
		rec.Reach = rep.Result
		if rep.Result == "relay" {
			log.Printf("node %d reports relay needed for peer %d", nodeID, rep.PeerNodeID)
		}
		s.save()
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleRegistry is what the test harness asserts against — and what the
// dashboard's topology page reads. Tenant-scoped by construction.
func (s *server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"tenant_id": tenant,
		"self":      nodeID,
		"epoch":     s.epoch(),
		"peers":     s.peersLocked(tenant, nodeID),
	})
}

// handleWatch is the push channel: long-lived, one JSON object per line.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "application/jsonl")
	w.WriteHeader(200)

	s.mu.Lock()
	// Whole-tenant snapshot, including self, so the agent needs no separate
	// "discover peers" round trip.
	for _, p := range s.peersLocked(tenant, nodeID) {
		if writeEvent(w, wire.WatchEvent{Type: "snapshot", Peer: p, Epoch: s.epoch(), SentAt: time.Now().Unix()}) != nil {
			s.mu.Unlock()
			return
		}
	}
	sub := &subscriber{tenant: tenant, nodeID: nodeID, ch: make(chan wire.WatchEvent, 256)}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = sub
	s.mu.Unlock()
	flusher.Flush()

	defer func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.ch:
			if writeEvent(w, ev) != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			// Stops intermediaries from timing the stream out. Agents skip
			// event types they do not know.
			if writeEvent(w, wire.WatchEvent{Type: "ping", SentAt: time.Now().Unix()}) != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev wire.WatchEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// ------------------------------------------------------------------ helpers

func key(tenant string, nodeID uint16) string { return fmt.Sprintf("%s|%d", tenant, nodeID) }

// ------------------------------------------------------------------ tenant facts

// tenantRoot returns the tenant's E_root, generating it on first use.
//
// E_root is the D-key domain root: the key every node of the tenant derives its
// hop MAC from. It is deliberately SHARED across the tenant (unlike the O-key)
// because a node has to verify hops that were signed by nodes it never talked
// to. The controller issuing it is what makes "every node can verify every
// other node's MAC" true without any pairwise setup.
//
// Must be called with s.mu held.
func (s *server) tenantRoot(tenant string) string {
	if k, ok := s.state.Roots[tenant]; ok {
		return k
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Generating a weak or empty root key would silently weaken every MAC in
		// the tenant, so refuse rather than degrade.
		log.Fatalf("tenant root key: entropy unavailable: %v", err)
	}
	k := hex.EncodeToString(buf)
	s.state.Roots[tenant] = k
	log.Printf("tenant %s: issued E_root %s… (new)", tenant, k[:8])
	return k
}

// srouPoolBase is the label-address pool. These addresses are NAMES, not routes
// (doc 24 §3.2): they must be tenant-unique but need not be reachable, so they
// live in a range that is never routed and never collides with a customer LAN.
const srouPoolBase = "10.255."

// assignSrouIP places (tenant, nodeID) in the tenant's own /24 inside the pool.
//
// Per-tenant index instead of a global counter on purpose: with a global
// counter, decommissioning a tenant would leave its range unused, and two nodes
// of one tenant could end up in different /24s -- making the tenant's namespace
// non-contiguous for no reason. The /24 gives 254 nodes per tenant, which is
// the phase-2 envelope; the scheme is a placeholder that a future pool
// allocator replaces without touching anything else.
//
// Must be called with s.mu held.
func (s *server) assignSrouIP(tenant string, nodeID uint16) string {
	idx, ok := s.state.TenantIndex[tenant]
	if !ok {
		s.state.NextTenant++
		idx = s.state.NextTenant
		s.state.TenantIndex[tenant] = idx
	}
	return fmt.Sprintf("%s%d.%d", srouPoolBase, idx, nodeID)
}

// applyBinding authenticates and stores a node's X25519 key binding.
//
// The controller verifies too, even though the relying party verifies again.
// Two different jobs: this one stops the controller from RELAYING garbage (and
// gives the operator an immediate, named error), while the receiver's check is
// what actually protects the payload key -- because it must hold even if this
// binary is lying. A failed check here is non-fatal to registration (the node's
// mapping is still useful for rendezvous) but is reported back to the agent so
// it can surface it.
//
// Returns whether the stored binding actually changed, so the caller knows when
// peers need to be told. A silent key change would leave every peer holding an
// O-key that no longer matches, and the only symptom would be "packets arrive
// but do not decrypt".
//
// Must be called with s.mu held.
func (s *server) applyBinding(rec *NodeRecord, b *keybinding.Binding) (changed bool, err error) {
	if b == nil {
		return false, nil
	}
	pub, err := base64.StdEncoding.DecodeString(rec.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("stored device key is unusable")
	}
	if _, err := b.Verify(rec.TenantID, rec.NodeID, ed25519.PublicKey(pub)); err != nil {
		return false, err
	}
	// Going BACKWARDS in key generation is a replay of an old binding, and is
	// refused. Going forward is a rotation, which is legitimate and must be
	// announced.
	if rec.KeyGen != 0 && b.KeyGen < rec.KeyGen {
		return false, fmt.Errorf("key_gen went backwards (%d -> %d): stale binding replay", rec.KeyGen, b.KeyGen)
	}
	if rec.Binding != nil && rec.Binding.X25519Pub == b.X25519Pub && rec.Binding.KeyGen == b.KeyGen {
		return false, nil // unchanged; the common case on every heartbeat
	}
	rotated := rec.KeyGen != 0
	rec.Binding = b
	rec.KeyGen = b.KeyGen
	if rotated {
		log.Printf("node %s/%d: x25519 key ROTATED to generation %d", rec.TenantID, rec.NodeID, b.KeyGen)
	} else {
		log.Printf("node %s/%d: x25519 binding accepted (gen %d)", rec.TenantID, rec.NodeID, b.KeyGen)
	}
	return true, nil
}

func toPeerInfo(rec *NodeRecord, self bool) wire.PeerInfo {
	return wire.PeerInfo{
		NodeID:      rec.NodeID,
		Serial:      rec.Serial,
		UDPMapping:  rec.UDPMapping,
		SrouIP:      rec.SrouIP,
		Reach:       rec.Reach,
		LastSeen:    rec.LastSeen,
		AgentVer:    rec.AgentVer,
		IsSelf:      self,
		Stale:       rec.Stale,
		SymmetricNA: rec.Symmetric,
		CertPEM:     rec.CertPEM,
		Binding:     rec.Binding,
		KeyGen:      rec.KeyGen,
	}
}

func (s *server) peersLocked(tenant string, self uint16) []wire.PeerInfo {
	out := []wire.PeerInfo{}
	for _, rec := range s.state.Nodes {
		if rec.TenantID != tenant {
			continue
		}
		out = append(out, toPeerInfo(rec, rec.NodeID == self))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// broadcastLocked pushes to subscribers of ONE tenant. Must be called with
// s.mu held.
//
// The tenant is a REQUIRED ARGUMENT, taken from the caller's certificate.
// It is deliberately NOT derived from the event payload: node IDs are unique
// only within a tenant, so "look up the tenant from the node ID in the event"
// is ambiguous the moment two tenants each have a node 1 — and the first
// version of this code did exactly that, delivering one tenant's peer event
// to the other tenant's stream. Identity always flows from the credential,
// never from the message.
//
// IsSelf is also set per SUBSCRIBER: a node must recognise "this is about me"
// (that is how NAT rebinding is detected), and a producer-side flag cannot
// know who is reading.
func (s *server) broadcastLocked(tenant string, ev wire.WatchEvent) {
	ev.TenantID = tenant
	for _, sub := range s.subs {
		if sub.tenant != tenant {
			continue
		}
		out := ev
		out.Peer.IsSelf = (ev.Peer.NodeID == sub.nodeID)
		select {
		case sub.ch <- out:
		default: // slow consumer: drop rather than stall the registry
			log.Printf("watch: dropped event for tenant %s (slow consumer)", sub.tenant)
		}
	}
}

// ttlSweeper marks lapsed nodes stale and pushes that fact. Peers find out
// from the controller instead of discovering it by packet loss.
func (s *server) ttlSweeper(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ttl := int64(s.hbSec * 3)
			if ttl < 6 {
				ttl = 6
			}
			now := time.Now().Unix()
			s.mu.Lock()
			for _, rec := range s.state.Nodes {
				if rec.LastSeen == 0 || rec.Stale {
					continue
				}
				if now-rec.LastSeen > ttl {
					rec.Stale = true
					log.Printf("ttl: node %d (tenant %s) stale after %ds idle",
						rec.NodeID, rec.TenantID, now-rec.LastSeen)
					s.broadcastLocked(rec.TenantID, wire.WatchEvent{
						Type: "stale", Peer: toPeerInfo(rec, false),
						SentAt: now, Message: "heartbeat lapsed",
					})
				}
			}
			s.save()
			s.mu.Unlock()
		}
	}
}

// ------------------------------------------------------------------ main

func main() {
	var (
		dataDir  = flag.String("data", "./ctrl-data", "state directory (CA, certs, registry)")
		listen   = flag.String("listen", "0.0.0.0:8444", "TLS control-plane listen address")
		// 这两个地址只用于**公布给 agent**：reflector 由独立进程提供服务
		// （controller/test/stun_reflector.py，真 RFC 5389）。控制器自己不监听 UDP。
		udpAddr  = flag.String("udp", "0.0.0.0:3478", "advertised UDP reflector #1 address (served by stun_reflector.py)")
		udpAddr2 = flag.String("udp2", "0.0.0.0:3479", "advertised UDP reflector #2 — a SECOND address is what makes symmetric-NAT detection possible")
		hbSec    = flag.Int("hb", 5, "heartbeat interval advertised to agents")
		extraSAN = flag.String("tls-san", "", "comma-separated extra SANs for the server certificate")
		opcode   = flag.Uint("opcode-base", 51820, "tenant instruction-port base: POP=base+0, DELIVER=base+1 (doc 23)")
	)
	flag.Parse()

	if *opcode == 0 || *opcode > 65535-1 {
		// base+1 is DELIVER, so base must leave room for it. Zero is excluded
		// because a port of 0 means "any" to bind(2) and would silently become
		// a random port -- the exact class of quiet mismatch this project keeps
		// rooting out.
		log.Fatalf("--opcode-base must be in 1..65534 (got %d)", *opcode)
	}
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatal(err)
	}

	ca, caKey, caPool, err := loadOrCreateCA(*dataDir)
	if err != nil {
		log.Fatal(err)
	}

	sans := []string{"localhost", "ctrl", "host.docker.internal", "127.0.0.1", "::1", "172.18.0.10"}
	if *extraSAN != "" {
		sans = append(sans, strings.Split(*extraSAN, ",")...)
	}
	srvCert, err := loadOrCreateServerCert(*dataDir, ca, caKey, sans)
	if err != nil {
		log.Fatal(err)
	}

	store := &fileStore{path: filepath.Join(*dataDir, "state.json")}
	st, err := store.Load()
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		state:      st,
		store:      store,
		subs:       map[int]*subscriber{},
		hbSec:      *hbSec,
		tlsAddrs:   []string{*listen},
		udpAddrs:   []string{*udpAddr, *udpAddr2},
		caCert:     ca,
		caKey:      caKey,
		dataDir:    *dataDir,
		opcodeBase: uint16(*opcode),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.ttlSweeper(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enroll", s.handleEnroll)
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/mapping", s.handleMapping)
	mux.HandleFunc("/v1/reach", s.handleReach)
	mux.HandleFunc("/v1/registry", s.handleRegistry)
	mux.HandleFunc("/v1/watch", s.handleWatch)

	hs := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// VerifyClientCertIfGiven: enrollment has no cert yet; every other
		// handler enforces identity via peerTenantFromCert. TLS client auth
		// cannot vary per path at handshake time, so enforcement lives in one
		// function instead of being spread across handlers.
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{srvCert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    caPool,
			MinVersion:   tls.VersionTLS12,
		},
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("shutting down (persisting state)")
		cancel()
		s.mu.Lock()
		s.save()
		s.mu.Unlock()
		hs.Close()
	}()

	log.Printf("controller listening on %s (data=%s, heartbeat=%ds)", *listen, *dataDir, *hbSec)
	if err := hs.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
