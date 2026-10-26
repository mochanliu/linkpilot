package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed console.html
var consoleHTML []byte

// The console is the human-facing half of the controller and the ONLY place a
// tenant is created. Two properties are deliberate:
//
//   - It is a separate listener from /v1/*. Browsers cannot present a device
//     certificate, and the device API authenticates every call by certificate
//     subject. Mixing the two would mean either weakening the device API or
//     asking every operator to install mTLS material.
//
//   - Its authority is narrower than the device API's, not wider. A console
//     user can create tenants, mint claim codes and read device state. It can
//     never enroll a device, sign a certificate or write key material: the
//     claim code it hands out is redeemed by the device itself, over the mTLS
//     listener, with a key the controller never sees.
//
// Authentication is a single operator token exchanged for a session cookie.
// POSTs additionally require a JSON content type, which a cross-site form
// cannot produce without a CORS preflight, so the cookie is not a CSRF lever.

const sessionCookie = "sr_ctrl_session"

type adminSession struct {
	expires time.Time
}

type adminHub struct {
	mu   sync.Mutex
	next int
	subs map[int]chan adminEvent
	sess map[string]adminSession
}

// adminEvent is what the console's SSE stream carries. It is intentionally
// thin: a console that receives an event refetches /api/state, so an event
// arriving during a refetch cannot leave the table stuck in a half-applied
// view, and a dropped event costs one refresh interval, not correctness.
type adminEvent struct {
	Type string `json:"type"`
	At   int64  `json:"at_unix"`
	Node string `json:"node,omitempty"`
}

func newAdminHub() *adminHub {
	return &adminHub{
		subs: map[int]chan adminEvent{},
		sess: map[string]adminSession{},
	}
}

// publish is safe to call with s.mu held: the hub never reads controller state
// and never blocks (a full subscriber buffer drops the event).
func (h *adminHub) publish(typ string, rec *NodeRecord) {
	if h == nil {
		return
	}
	ev := adminEvent{Type: typ, At: time.Now().Unix()}
	if rec != nil {
		ev.Node = key(rec.TenantID, rec.NodeID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func randToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// newClaimCode is the code a customer pastes into a device. The alphabet has no
// vowels and no look-alikes: it is read off a screen and typed on a console,
// and 0/O, 1/l/I confusion is the failure that costs a support call. Entropy is
// still 26^12.
func newClaimCode(tenant string) string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return tenant + "-" + string(out)
}

// ------------------------------------------------------------------ auth

func (s *server) checkToken(presented string) bool {
	if s.adminToken == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.adminToken)) == 1
}

func (s *server) sessionValid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	se, ok := s.admin.sess[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(se.expires) {
		delete(s.admin.sess, c.Value)
		return false
	}
	return true
}

func (s *server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleConsolePage)
	mux.HandleFunc("/api/login", s.handleAdminLogin)
	mux.HandleFunc("/api/logout", s.handleAdminLogout)
	mux.HandleFunc("/api/session", s.handleAdminSession)
	mux.HandleFunc("/api/state", s.handleAdminState)
	mux.HandleFunc("/api/tenants", s.handleAdminTenants)
	mux.HandleFunc("/api/claims", s.handleAdminClaims)
	mux.HandleFunc("/api/claims/revoke", s.handleAdminClaimRevoke)
	mux.HandleFunc("/api/events", s.handleAdminEvents)
	return mux
}

func (s *server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(consoleHTML)
}

type loginRequest struct {
	Token string `json:"token"`
}

func (s *server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if !s.checkToken(strings.TrimSpace(req.Token)) {
		log.Printf("console login REJECTED")
		http.Error(w, "invalid token", 401)
		return
	}
	sid := randToken()
	s.admin.mu.Lock()
	s.admin.sess[sid] = adminSession{expires: time.Now().Add(12 * time.Hour)}
	s.admin.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 3600,
	})
	log.Printf("console login ok")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.admin.mu.Lock()
		delete(s.admin.sess, c.Value)
		s.admin.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"logged_in": s.sessionValid(r)})
}

// requireSession rejects anything that did not come from the console page.
func (s *server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if !s.sessionValid(r) {
		http.Error(w, "not logged in", 401)
		return false
	}
	if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", 415)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------------ state view

type consoleClaim struct {
	Code          string   `json:"code"`
	TenantID      string   `json:"tenant_id"`
	Note          string   `json:"note,omitempty"`
	CreatedAt     int64    `json:"created_at_unix"`
	ExpiresAt     int64    `json:"expires_at_unix,omitempty"`
	Revoked       bool     `json:"revoked"`
	Serials       []string `json:"serials"`
	RedeemedCount int      `json:"redeemed_count"`
}

type consoleNode struct {
	TenantID   string `json:"tenant_id"`
	NodeID     uint16 `json:"node_id"`
	Serial     string `json:"serial"`
	AgentVer   string `json:"agent_version"`
	EnrolledAt int64  `json:"enrolled_at_unix"`
	LastSeen   int64  `json:"last_seen_unix"`
	Status     string `json:"status"` // online | stale | pending
	UDPMapping string `json:"udp_mapping"`
	VpnPort    uint16 `json:"vpn_port"`
	// Configured is false for every node today: enrollment is not configuration,
	// and the node plan (ports, overlay address, keys, RR list) does not exist
	// yet. The console says so rather than implying a device is ready to tunnel.
	Configured bool `json:"configured"`
}

type consoleController struct {
	TLSAddrs     []string `json:"tls_addrs"`
	HeartbeatSec int      `json:"heartbeat_sec"`
	Epoch        uint32   `json:"epoch"`
}

type consoleState struct {
	Controller consoleController `json:"controller"`
	Tenants    []*Tenant         `json:"tenants"`
	Claims     []consoleClaim    `json:"claims"`
	Nodes      []consoleNode     `json:"nodes"`
	NowUnix    int64             `json:"now_unix"`
}

func (s *server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	var out consoleState
	s.mu.Lock()
	out.Controller = consoleController{
		TLSAddrs:     append([]string{}, s.tlsAddrs...),
		HeartbeatSec: s.hbSec,
		Epoch:        s.state.Epoch,
	}
	for _, t := range s.state.Tenants {
		cp := *t
		out.Tenants = append(out.Tenants, &cp)
	}
	for _, c := range s.state.Claims {
		out.Claims = append(out.Claims, consoleClaim{
			Code: c.Code, TenantID: c.TenantID, Note: c.Note,
			CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, Revoked: c.Revoked,
			Serials:       append([]string{}, c.Serials...),
			RedeemedCount: len(c.Serials),
		})
	}
	for _, n := range s.state.Nodes {
		out.Nodes = append(out.Nodes, consoleNode{
			TenantID: n.TenantID, NodeID: n.NodeID, Serial: n.Serial,
			AgentVer: n.AgentVer, EnrolledAt: n.EnrolledAt, LastSeen: n.LastSeen,
			Status:     nodeStatus(n),
			UDPMapping: n.UDPMapping, VpnPort: n.VpnPort,
			Configured: false,
		})
	}
	s.mu.Unlock()
	out.NowUnix = time.Now().Unix()

	sort.Slice(out.Tenants, func(i, j int) bool { return out.Tenants[i].ID < out.Tenants[j].ID })
	sort.Slice(out.Claims, func(i, j int) bool {
		if out.Claims[i].TenantID != out.Claims[j].TenantID {
			return out.Claims[i].TenantID < out.Claims[j].TenantID
		}
		return out.Claims[i].CreatedAt < out.Claims[j].CreatedAt
	})
	// Newest device first: the row the customer just enrolled is the one they
	// are looking for.
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].EnrolledAt > out.Nodes[j].EnrolledAt })
	writeJSON(w, out)
}

func nodeStatus(n *NodeRecord) string {
	switch {
	case n.LastSeen == 0:
		return "pending"
	case n.Stale:
		return "stale"
	default:
		return "online"
	}
}

// ------------------------------------------------------------------ tenants

type createTenantRequest struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

type createTenantResponse struct {
	Tenant *Tenant       `json:"tenant"`
	Claim  *consoleClaim `json:"claim"`
}

func (s *server) handleAdminTenants(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req createTenantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	id := strings.TrimSpace(req.TenantID)
	if !validTenantID(id) {
		http.Error(w, "tenant_id must be 2..40 chars of a-z 0-9 - _", 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = id
	}

	s.mu.Lock()
	if _, exists := s.state.Tenants[id]; exists {
		s.mu.Unlock()
		http.Error(w, "tenant already exists", 409)
		return
	}
	now := time.Now().Unix()
	t := &Tenant{ID: id, Name: name, CreatedAt: now}
	s.state.Tenants[id] = t
	// The tenant's first enrollment code ships with the tenant. Creating an
	// account and being able to enroll a device are one intent, one step apart;
	// making the operator discover a second button just to get a string is
	// friction with a support ticket attached.
	c := &Claim{Code: newClaimCode(id), TenantID: id, Note: "created with tenant", CreatedAt: now}
	for s.state.Claims[c.Code] != nil {
		c.Code = newClaimCode(id)
	}
	s.state.Claims[c.Code] = c
	s.save()
	s.mu.Unlock()

	log.Printf("console: tenant %s created (claim %s)", id, c.Code)
	s.admin.publish("tenant.created", nil)
	writeJSON(w, createTenantResponse{
		Tenant: t,
		Claim: &consoleClaim{
			Code: c.Code, TenantID: c.TenantID, Note: c.Note,
			CreatedAt: c.CreatedAt, Serials: []string{},
		},
	})
}

func validTenantID(id string) bool {
	if len(id) < 2 || len(id) > 40 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ claims

type createClaimRequest struct {
	TenantID   string `json:"tenant_id"`
	Note       string `json:"note"`
	ExpiresInH int    `json:"expires_in_hours"`
}

func (s *server) handleAdminClaims(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req createClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.ExpiresInH < 0 {
		http.Error(w, "expires_in_hours must be >= 0", 400)
		return
	}
	s.mu.Lock()
	if _, ok := s.state.Tenants[req.TenantID]; !ok {
		s.mu.Unlock()
		http.Error(w, "unknown tenant", 404)
		return
	}
	now := time.Now().Unix()
	c := &Claim{Code: newClaimCode(req.TenantID), TenantID: req.TenantID, Note: strings.TrimSpace(req.Note), CreatedAt: now}
	for s.state.Claims[c.Code] != nil {
		c.Code = newClaimCode(req.TenantID)
	}
	if req.ExpiresInH > 0 {
		c.ExpiresAt = now + int64(req.ExpiresInH)*3600
	}
	s.state.Claims[c.Code] = c
	s.save()
	s.mu.Unlock()

	log.Printf("console: claim %s issued for tenant %s", c.Code, c.TenantID)
	s.admin.publish("claim.created", nil)
	writeJSON(w, consoleClaim{
		Code: c.Code, TenantID: c.TenantID, Note: c.Note,
		CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, Serials: []string{},
	})
}

type revokeClaimRequest struct {
	Code string `json:"code"`
}

func (s *server) handleAdminClaimRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req revokeClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	s.mu.Lock()
	c, ok := s.state.Claims[req.Code]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown claim code", 404)
		return
	}
	c.Revoked = true
	s.save()
	s.mu.Unlock()

	// Revoking a code stops FUTURE enrollment. Devices that already redeemed it
	// keep their certificates: withdrawing an identity is a different action
	// (certificate revocation), and this button must not imply it did that.
	log.Printf("console: claim %s revoked (already-enrolled devices unaffected)", req.Code)
	s.admin.publish("claim.revoked", nil)
	writeJSON(w, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ events

func (s *server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !s.sessionValid(r) {
		http.Error(w, "not logged in", 401)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ch := make(chan adminEvent, 16)
	s.admin.mu.Lock()
	id := s.admin.next
	s.admin.next++
	s.admin.subs[id] = ch
	s.admin.mu.Unlock()
	defer func() {
		s.admin.mu.Lock()
		delete(s.admin.subs, id)
		s.admin.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprintf(w, "event: hello\ndata: {}\n\n")
	fl.Flush()

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		case <-tick.C:
			// Comment frame: keeps intermediaries from closing an idle stream
			// without delivering an event the page would have to interpret.
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}
