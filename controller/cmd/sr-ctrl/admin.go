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
	"unicode/utf8"
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
//     user can create tenants, issue enrollment grants and read device state. It
//     can never enroll a device, sign a certificate or write key material: the
//     claim code is redeemed by the device itself, over the mTLS listener, with a
//     key the controller never sees.
//
// Enrollment is two levels. The OPERATOR issues a grant (name + how many devices
// it may activate) and never sees a code. The TENANT generates a short-lived
// code from that grant at the moment it activates a device; the controller keeps
// only the code's hash, and the plaintext appears in exactly one response.
//
// Authentication is a console account (username + password) or the operator
// token from the command line. Both produce a session that carries an IDENTITY,
// not just an expiry: a tenant session can only ever see its own tenant, and
// every mutating endpoint names the role it requires.
//
// The operator token stays on as a break-glass login for bootstrap and recovery
// (a lost admin password must not lock a private deployment out of its own
// controller). It is not an account and is not stored in state.
//
// POSTs additionally require a JSON content type, which a cross-site form
// cannot produce without a CORS preflight, so the cookie is not a CSRF lever.

const sessionCookie = "sr_ctrl_session"

const (
	roleOperator = "operator"
	roleTenant   = "tenant"
)

// consoleIdentity is who a session belongs to. TenantID is empty for operators.
type consoleIdentity struct {
	Role     string
	TenantID string
	Username string
	Via      string // "password" | "token"
	expires  time.Time
}

type adminHub struct {
	mu    sync.Mutex
	next  int
	subs  map[int]adminSub
	sess  map[string]consoleIdentity
	fails map[string]*loginFail
}

// adminSub is one SSE subscriber plus the slice of the world it is allowed to
// see. Filtering here (not in the page) matters: events carry node names, and
// leaking another tenant's node key would tell a customer that it exists.
type adminSub struct {
	ch       chan adminEvent
	role     string
	tenantID string
}

// adminEvent is what the console's SSE stream carries. It is intentionally
// thin: a console that receives an event refetches /api/state, so an event
// arriving during a refetch cannot leave the table stuck in a half-applied
// view, and a dropped event costs one refresh interval, not correctness.
type adminEvent struct {
	Type     string `json:"type"`
	At       int64  `json:"at_unix"`
	TenantID string `json:"tenant_id,omitempty"`
	Node     string `json:"node,omitempty"`
}

func newAdminHub() *adminHub {
	return &adminHub{
		subs:  map[int]adminSub{},
		sess:  map[string]consoleIdentity{},
		fails: map[string]*loginFail{},
	}
}

// publish is safe to call with s.mu held: the hub never reads controller state
// and never blocks (a full subscriber buffer drops the event). tenantID is the
// event's owner; it is what a tenant subscriber is matched against.
func (h *adminHub) publish(typ, tenantID string, rec *NodeRecord) {
	if h == nil {
		return
	}
	ev := adminEvent{Type: typ, At: time.Now().Unix(), TenantID: tenantID}
	if rec != nil {
		ev.TenantID = rec.TenantID
		ev.Node = key(rec.TenantID, rec.NodeID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.role == roleTenant && sub.tenantID != ev.TenantID {
			continue
		}
		select {
		case sub.ch <- ev:
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

const (
	claimDefaultMaxUses = 10
	claimMaxUsesLimit   = 10000

	// claimCodeTTLDefault is how long a generated code stays valid. Five
	// minutes is long enough to type it into one device and short enough that a
	// screenshot left on a desk is worthless. The tenant may stretch it to a day.
	claimCodeTTLDefault = 300
	claimCodeTTLMax     = 86400
)

// normalizeClaimCodeTTL turns the tenant's requested window into seconds. 0 is
// the wire default; a window outside 1s..24h is a client mistake, not a wish.
func normalizeClaimCodeTTL(n int) (int, error) {
	switch {
	case n == 0:
		return claimCodeTTLDefault, nil
	case n < 1 || n > claimCodeTTLMax:
		return 0, fmt.Errorf("ttl_sec must be 1..%d (0 means the default %d)", claimCodeTTLMax, claimCodeTTLDefault)
	}
	return n, nil
}

// normalizeGrantName keeps a grant's name renderable: it is what the tenant
// sees instead of a code, so it must be present, short, and not smuggled in as
// an empty string.
func normalizeGrantName(name, fallback string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}
	if utf8.RuneCountInString(name) > 60 {
		return "", fmt.Errorf("name must be at most 60 characters")
	}
	return name, nil
}

// newClaimCode is the secret a customer pastes into a device. It deliberately
// looks like an API key: an opaque, prefix-tagged, 160-bit random string with no
// tenant name in it, so it cannot be guessed from a tenant ID or recognized on
// sight as belonging to someone else. The alphabet has no vowels and no
// look-alikes: it is read off a screen and typed on a device console, and 0/O,
// 1/l/I confusion is the failure that costs a support call.
func newClaimCode() string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return "srk_" + string(out)
}

// normalizeClaimMaxUses turns the operator's requested activation limit into the
// stored one. 0 is the wire default, not "unlimited": an unbounded code is a code
// that leaks once and enrolls forever, so every code carries a finite seat count.
func normalizeClaimMaxUses(n int) (int, error) {
	switch {
	case n == 0:
		return claimDefaultMaxUses, nil
	case n < 0 || n > claimMaxUsesLimit:
		return 0, fmt.Errorf("max_uses must be 1..%d (0 means the default %d)", claimMaxUsesLimit, claimDefaultMaxUses)
	}
	return n, nil
}

// ------------------------------------------------------------------ auth

func (s *server) checkToken(presented string) bool {
	if s.adminToken == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.adminToken)) == 1
}

// session returns the identity behind the request's cookie, if the session is
// live. The zero identity (Role "") means "not logged in".
func (s *server) session(r *http.Request) consoleIdentity {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return consoleIdentity{}
	}
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	id, ok := s.admin.sess[c.Value]
	if !ok {
		return consoleIdentity{}
	}
	if time.Now().After(id.expires) {
		delete(s.admin.sess, c.Value)
		return consoleIdentity{}
	}
	return id
}

func (s *server) newSession(w http.ResponseWriter, id consoleIdentity) {
	sid := randToken()
	id.expires = time.Now().Add(12 * time.Hour)
	s.admin.mu.Lock()
	s.admin.sess[sid] = id
	s.admin.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 3600,
	})
}

// dropSessions ends every session of one account. Used when a password is
// changed, reset or an account is disabled: the old cookie must not outlive the
// credential it was issued for.
func (s *server) dropSessions(username string) {
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	for sid, id := range s.admin.sess {
		if id.Username == username {
			delete(s.admin.sess, sid)
		}
	}
}

// ------------------------------------------------------------------ login throttle

type loginFail struct {
	count int
	until time.Time
}

func loginKey(username string, r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return username + "|" + host
}

func (s *server) loginBlocked(username string, r *http.Request) bool {
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	f, ok := s.admin.fails[loginKey(username, r)]
	return ok && time.Now().Before(f.until)
}

func (s *server) noteLoginFail(username string, r *http.Request) {
	k := loginKey(username, r)
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	f := s.admin.fails[k]
	if f == nil {
		f = &loginFail{}
		s.admin.fails[k] = f
	}
	now := time.Now()
	// A zero until means "never locked yet", not "the lock expired". Resetting
	// the counter on every failure made the 5-failure lock unreachable.
	if !f.until.IsZero() && now.After(f.until) {
		f.count = 0
		f.until = time.Time{}
	}
	f.count++
	if f.count >= 5 {
		f.until = now.Add(60 * time.Second)
		f.count = 0
		log.Printf("console login: %s locked for 60s after repeated failures", k)
	}
}

func (s *server) clearLoginFail(username string, r *http.Request) {
	s.admin.mu.Lock()
	delete(s.admin.fails, loginKey(username, r))
	s.admin.mu.Unlock()
}

func (s *server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleConsolePage)
	// The world basemap. More specific than the "/" catch-all above, and the only
	// console URL that is not behind a session -- it carries no tenant data, and
	// a map with no points on it says nothing about anybody's network.
	mux.HandleFunc("/assets/", s.handleConsoleAsset)
	mux.HandleFunc("/api/login", s.handleAdminLogin)
	mux.HandleFunc("/api/logout", s.handleAdminLogout)
	mux.HandleFunc("/api/session", s.handleAdminSession)
	mux.HandleFunc("/api/password", s.handleAdminPassword)
	mux.HandleFunc("/api/state", s.handleAdminState)
	mux.HandleFunc("/api/metrics", s.handleAdminMetrics)
	mux.HandleFunc("/api/config", s.handleAdminConfig)
	mux.HandleFunc("/api/config/bundle", s.handleAdminConfigBundle)
	mux.HandleFunc("/api/config/publish", s.handleAdminConfigPublish)
	// 预授权: devices the tenant registers before they exist. Tenant-only, like the
	// config layer -- the operator hands out seats and never names a customer's
	// boxes; it sees a grant's reserved count, not the names behind it.
	mux.HandleFunc("/api/preauth", s.handleAdminPreAuth)
	mux.HandleFunc("/api/preauth/revoke", s.handleAdminPreAuthRevoke)
	mux.HandleFunc("/api/preauth/config", s.handleAdminPreAuthConfig)
	// Underlay is tenant-only and never appears in /api/state; the operator role
	// sees inventory, not a customer's network layout.
	mux.HandleFunc("/api/underlay", s.handleAdminUnderlay)
	mux.HandleFunc("/api/underlay/settings", s.handleAdminUnderlaySettings)
	mux.HandleFunc("/api/underlay/settings/publish", s.handleAdminUnderlayPublish)
	mux.HandleFunc("/api/underlay/tunnels", s.handleAdminUnderlayTunnels)
	mux.HandleFunc("/api/underlay/auto/plan", s.handleAdminUnderlayAutoPlan)
	mux.HandleFunc("/api/underlay/auto/apply", s.handleAdminUnderlayAutoApply)
	mux.HandleFunc("/api/underlay/paths", s.handleAdminUnderlayPaths)
	mux.HandleFunc("/api/underlay/topology", s.handleAdminUnderlayTopology)
	// Sites are the map's only input and are tenant-only like the rest of the
	// underlay layer: a location is where the customer's building is, which the
	// operator has no business editing.
	mux.HandleFunc("/api/underlay/sites", s.handleAdminUnderlaySites)
	mux.HandleFunc("/api/underlay/sites/delete", s.handleAdminUnderlaySiteDelete)
	mux.HandleFunc("/api/underlay/node-site", s.handleAdminUnderlayNodeSite)
	mux.HandleFunc("/api/tenants", s.handleAdminTenants)
	mux.HandleFunc("/api/claims", s.handleAdminClaims)
	mux.HandleFunc("/api/claims/code", s.handleAdminClaimCode)
	mux.HandleFunc("/api/claims/revoke", s.handleAdminClaimRevoke)
	mux.HandleFunc("/api/claims/update", s.handleAdminClaimUpdate)
	mux.HandleFunc("/api/accounts", s.handleAdminAccounts)
	mux.HandleFunc("/api/accounts/reset", s.handleAdminAccountReset)
	mux.HandleFunc("/api/accounts/disable", s.handleAdminAccountDisable)
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
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
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

	// Break-glass: the operator token from the command line.
	if tok := strings.TrimSpace(req.Token); tok != "" {
		if !s.checkToken(tok) {
			log.Printf("console login REJECTED (bad operator token)")
			http.Error(w, "invalid token", 401)
			return
		}
		s.newSession(w, consoleIdentity{Role: roleOperator, Username: "operator", Via: "token"})
		log.Printf("console login ok via operator token")
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		http.Error(w, "username and password required", 400)
		return
	}
	if s.loginBlocked(username, r) {
		http.Error(w, "too many failed logins, retry in a minute", 429)
		return
	}

	s.mu.Lock()
	var acc *Account
	if a := s.state.Accounts[username]; a != nil {
		cp := *a
		acc = &cp
	}
	s.mu.Unlock()

	if acc == nil || acc.Disabled {
		// Same work, same message: whether the username exists is not something
		// an unauthenticated caller gets to learn.
		burnPasswordWork(req.Password)
		s.noteLoginFail(username, r)
		log.Printf("console login REJECTED username=%s", username)
		http.Error(w, "invalid username or password", 401)
		return
	}
	if !verifyPassword(acc.Password, req.Password) {
		s.noteLoginFail(username, r)
		log.Printf("console login REJECTED username=%s", username)
		http.Error(w, "invalid username or password", 401)
		return
	}
	if acc.Role == roleTenant {
		s.mu.Lock()
		_, ok := s.state.Tenants[acc.TenantID]
		s.mu.Unlock()
		if !ok {
			s.noteLoginFail(username, r)
			http.Error(w, "invalid username or password", 401)
			return
		}
	}

	s.clearLoginFail(username, r)
	s.mu.Lock()
	if a := s.state.Accounts[username]; a != nil {
		a.LastLogin = time.Now().Unix()
	}
	s.save()
	s.mu.Unlock()

	s.newSession(w, consoleIdentity{
		Role: acc.Role, TenantID: acc.TenantID, Username: acc.Username, Via: "password",
	})
	log.Printf("console login ok username=%s role=%s tenant=%s", acc.Username, acc.Role, acc.TenantID)
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
	id := s.session(r)
	if id.Role == "" {
		writeJSON(w, map[string]any{"logged_in": false})
		return
	}
	out := map[string]any{
		"logged_in": true,
		"role":      id.Role,
		"username":  id.Username,
		"via":       id.Via,
		"tenant_id": id.TenantID,
	}
	if id.Via == "password" {
		s.mu.Lock()
		if a := s.state.Accounts[id.Username]; a != nil {
			out["initial_password"] = a.InitialPass
		}
		if t := s.state.Tenants[id.TenantID]; t != nil {
			out["tenant_name"] = t.Name
		}
		s.mu.Unlock()
	}
	writeJSON(w, out)
}

// requireSession rejects anything that did not come from the console page.
func (s *server) requireSession(w http.ResponseWriter, r *http.Request) (consoleIdentity, bool) {
	id := s.session(r)
	if id.Role == "" {
		http.Error(w, "not logged in", 401)
		return id, false
	}
	if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", 415)
		return id, false
	}
	return id, true
}

// requireOperator is the gate on everything that changes the world: tenants,
// claim codes, accounts. A tenant account gets 403, not a hidden 404 -- it is
// allowed to know the endpoint exists and is not for it.
func (s *server) requireOperator(w http.ResponseWriter, r *http.Request) (consoleIdentity, bool) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return id, false
	}
	if id.Role != roleOperator {
		http.Error(w, "operator role required", 403)
		return id, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------------ state view

// consoleClaim is one enrollment grant as the page renders it. It is scoped to
// the grant, not to a code: the code is a short-lived secret that only its
// issuing tenant ever sees, so nothing here can be replayed against /v1/enroll.
// The code_* fields are metadata (when generated, when it dies, who asked); the
// hash never leaves the process, and the plaintext only exists in the one
// response to the tenant that generated it.
type consoleClaim struct {
	ID            string   `json:"id"`
	TenantID      string   `json:"tenant_id"`
	Name          string   `json:"name"`
	CreatedAt     int64    `json:"created_at_unix"`
	Revoked       bool     `json:"revoked"`
	MaxUses       int      `json:"max_uses"`
	Uses          int      `json:"uses"`
	Serials       []string `json:"serials"`
	RedeemedCount int      `json:"redeemed_count"`
	// Reserved is how many seats are held by 预授权 records with no device yet. The
	// operator sees the count and nothing else: which names the tenant reserved
	// and what they configured for them is not this role's view.
	Reserved int `json:"reserved"`

	CodeActive    bool   `json:"code_active"`
	CodeIssuedAt  int64  `json:"code_issued_at_unix,omitempty"`
	CodeExpiresAt int64  `json:"code_expires_at_unix,omitempty"`
	CodeIssuedBy  string `json:"code_issued_by,omitempty"`
	CodeUsedAt    int64  `json:"code_used_at_unix,omitempty"`
}

type consoleNode struct {
	TenantID   string `json:"tenant_id"`
	NodeID     uint16 `json:"node_id"`
	Serial     string `json:"serial"`
	AgentVer   string `json:"agent_version"`
	EnrolledAt int64  `json:"enrolled_at_unix"`
	LastSeen   int64  `json:"last_seen_unix"`
	Status     string `json:"status"` // online | stale | pending

	// Runtime is present only for the tenant that owns the node. The operator
	// role sees inventory (who, serial, enrolled when, online) and NOTHING about
	// how the device is configured or what it reports -- "运营方完全看不到配置"
	// is enforced here, not in the page, so a future config field cannot leak by
	// being added to this struct.
	Runtime *consoleNodeRuntime `json:"runtime,omitempty"`
}

type consoleNodeRuntime struct {
	UDPMapping string `json:"udp_mapping"`
	VpnPort    uint16 `json:"vpn_port"`
	// Site is where the box physically sits, for the map. It lives in Runtime so
	// the operator role cannot receive it by accident: a location is part of how
	// the customer drew their own network, and an empty SiteID means "未定位",
	// which is a state the page shows rather than a field it hides.
	SiteID   string `json:"site_id,omitempty"`
	SiteName string `json:"site_name,omitempty"`
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
	// PreAuths is tenant-only, like Runtime: an operator sees that a grant has
	// seats held, never which devices the customer named.
	PreAuths []consolePreAuth `json:"preauths"`
	NowUnix  int64            `json:"now_unix"`
}

func (s *server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	// A tenant session is scoped here, before anything is rendered: tenants see
	// their own tenant row, their own enrollment grants and their own devices. The
	// page never has to filter, and a page bug cannot widen the view.
	own := func(tenantID string) bool {
		return id.Role == roleOperator || tenantID == id.TenantID
	}
	// Empty collections must serialize as [] rather than null: a tenant with no
	// device yet is normal, and the page filters these slices unconditionally.
	var out = consoleState{
		Tenants:  []*Tenant{},
		Claims:   []consoleClaim{},
		Nodes:    []consoleNode{},
		PreAuths: []consolePreAuth{},
	}
	s.mu.Lock()
	out.Controller = consoleController{
		TLSAddrs:     append([]string{}, s.tlsAddrs...),
		HeartbeatSec: s.hbSec,
		Epoch:        s.state.Epoch,
	}
	for _, t := range s.state.Tenants {
		if !own(t.ID) {
			continue
		}
		cp := *t
		out.Tenants = append(out.Tenants, &cp)
	}
	for _, c := range s.state.Claims {
		if !own(c.TenantID) {
			continue
		}
		out.Claims = append(out.Claims, *claimView(c))
	}
	for _, n := range s.state.Nodes {
		if !own(n.TenantID) {
			continue
		}
		cn := consoleNode{
			TenantID: n.TenantID, NodeID: n.NodeID, Serial: n.Serial,
			AgentVer: n.AgentVer, EnrolledAt: n.EnrolledAt, LastSeen: n.LastSeen,
			Status: nodeStatus(n),
		}
		if id.Role == roleTenant {
			rt := consoleNodeRuntime{
				UDPMapping: n.UDPMapping, VpnPort: n.VpnPort,
				SiteID: n.SiteID,
			}
			// Resolve the name here rather than in the page: a site row can be
			// renamed while this response is in flight, and a stale name printed
			// next to a live id is the kind of contradiction the map already
			// refuses to draw.
			if n.SiteID != "" {
				if si := s.siteLocked(n.TenantID, n.SiteID); si != nil {
					rt.SiteName = si.Name
				}
			}
			cn.Runtime = &rt
		}
		out.Nodes = append(out.Nodes, cn)
	}
	// 预授权 rows are the tenant's own list, and they are separate from Nodes on
	// purpose: a record with no device must not be counted as an offline box, an
	// unplaced device, or a pending heartbeat anywhere in the page.
	if id.Role == roleTenant {
		out.PreAuths = s.preAuthListLocked(id.TenantID)
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
	// Account optionally creates the customer's first console login in the same
	// step. Creating a tenant and being able to log in as that tenant are one
	// intent in private deployments; the operator can also add accounts later.
	Account  string `json:"account"`
	Password string `json:"password"`
	// ClaimMaxUses is the activation limit of the tenant's first enrollment
	// grant. Every grant is bounded; 0 means "use the default", not "unlimited".
	ClaimMaxUses int `json:"claim_max_uses"`
}

type createTenantResponse struct {
	Tenant  *Tenant         `json:"tenant"`
	Claim   *consoleClaim   `json:"claim"`
	Account *consoleAccount `json:"account,omitempty"`
	// Password is the one-time initial password, returned exactly once (when it
	// was generated, i.e. the request did not supply one).
	Password string `json:"password,omitempty"`
}

func claimView(c *Claim) *consoleClaim {
	now := time.Now().Unix()
	return &consoleClaim{
		ID: c.ID, TenantID: c.TenantID, Name: c.Name,
		CreatedAt: c.CreatedAt, Revoked: c.Revoked,
		MaxUses: c.MaxUses, Uses: len(c.Serials),
		Serials: append([]string{}, c.Serials...), RedeemedCount: len(c.Serials),
		Reserved: len(c.Reserved),

		// Expiry is evaluated here as well as in the sweeper: the two-second
		// sweep interval must not show a dead code as live to the tenant.
		CodeActive:    c.CodeHash != "" && (c.CodeExpiresAt == 0 || now <= c.CodeExpiresAt),
		CodeIssuedAt:  c.CodeIssuedAt,
		CodeExpiresAt: c.CodeExpiresAt,
		CodeIssuedBy:  c.CodeIssuedBy,
		CodeUsedAt:    c.CodeUsedAt,
	}
}

// tenantClaimViewsLocked is the same row set /api/state builds, so a write that
// moves a seat (create/revoke a pre-authorization) can hand the page the fresh
// counts instead of leaving the grant table showing the pre-write numbers.
func (s *server) tenantClaimViewsLocked(tenant string) []consoleClaim {
	out := []consoleClaim{}
	for _, c := range s.state.Claims {
		if c.TenantID != tenant {
			continue
		}
		out = append(out, *claimView(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *server) handleAdminTenants(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
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
	maxUses, err := normalizeClaimMaxUses(req.ClaimMaxUses)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	username := strings.TrimSpace(req.Account)
	pw := req.Password
	generated := false
	var pwHash string
	if username != "" {
		if !validUsername(username) {
			http.Error(w, "account must be 3..40 chars of a-z 0-9 - _ .", 400)
			return
		}
		if pw == "" {
			pw = newPassword()
			generated = true
		} else if err := checkPasswordPolicy(pw); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var hashErr error
		if pwHash, hashErr = hashPassword(pw); hashErr != nil {
			http.Error(w, "cannot hash password", 500)
			return
		}
	}

	s.mu.Lock()
	if _, exists := s.state.Tenants[id]; exists {
		s.mu.Unlock()
		http.Error(w, "tenant already exists", 409)
		return
	}
	if username != "" {
		if _, exists := s.state.Accounts[username]; exists {
			s.mu.Unlock()
			http.Error(w, "account already exists", 409)
			return
		}
	}
	now := time.Now().Unix()
	t := &Tenant{ID: id, Name: name, CreatedAt: now}
	s.state.Tenants[id] = t
	// The tenant's first enrollment grant ships with the tenant. Creating a
	// tenant and being able to activate a device are one intent, one step apart;
	// making the operator discover a second button just to get a row is friction
	// with a support ticket attached. The grant carries no code: the tenant
	// generates one when it actually has a device in hand.
	c := &Claim{ID: newClaimID(), TenantID: id, Name: "初始授权", CreatedAt: now, MaxUses: maxUses}
	for s.state.Claims[c.ID] != nil {
		c.ID = newClaimID()
	}
	s.state.Claims[c.ID] = c
	var accView *consoleAccount
	if username != "" {
		a := &Account{
			Username: username, Role: roleTenant, TenantID: id, Password: pwHash,
			CreatedAt: now, InitialPass: true,
		}
		s.state.Accounts[username] = a
		accView = accountView(a)
	}
	s.save()
	s.mu.Unlock()

	log.Printf("console: tenant %s created (grant %s, account %q)", id, c.ID, username)
	s.admin.publish("tenant.created", id, nil)
	resp := createTenantResponse{Tenant: t, Claim: claimView(c), Account: accView}
	if generated {
		resp.Password = pw
	}
	writeJSON(w, resp)
}

func validUsername(u string) bool {
	if len(u) < 3 || len(u) > 40 {
		return false
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
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
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	MaxUses  int    `json:"max_uses"`
}

// handleAdminClaims issues a grant. The operator names it and sets how many
// devices it may activate; the code itself is the tenant's to generate later.
func (s *server) handleAdminClaims(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
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
	maxUses, err := normalizeClaimMaxUses(req.MaxUses)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name, err := normalizeGrantName(req.Name, "入网授权")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	if _, ok := s.state.Tenants[req.TenantID]; !ok {
		s.mu.Unlock()
		http.Error(w, "unknown tenant", 404)
		return
	}
	now := time.Now().Unix()
	c := &Claim{ID: newClaimID(), TenantID: req.TenantID, Name: name, CreatedAt: now, MaxUses: maxUses}
	for s.state.Claims[c.ID] != nil {
		c.ID = newClaimID()
	}
	s.state.Claims[c.ID] = c
	s.save()
	s.mu.Unlock()

	log.Printf("console: grant %s (%q) issued for tenant %s with %d seats", c.ID, c.Name, c.TenantID, c.MaxUses)
	s.admin.publish("claim.created", c.TenantID, nil)
	writeJSON(w, claimView(c))
}

// claimCodeRequest names a grant and asks for a code good for TTLSec seconds.
type claimCodeRequest struct {
	ID     string `json:"id"`
	TTLSec int    `json:"ttl_sec"`
}

// claimCodeResponse is the ONLY place a usable enrollment code exists outside
// the device that redeems it. It is returned once, to the tenant that asked.
// The page must let the user copy it immediately; it cannot be shown again,
// because the controller kept only its hash.
type claimCodeResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Code      string `json:"code"`
	IssuedAt  int64  `json:"issued_at_unix"`
	ExpiresAt int64  `json:"expires_at_unix"`
}

// handleAdminClaimCode mints a short-lived code for one of the session tenant's
// own grants. It is deliberately tenant-only: the operator decides WHETHER a
// tenant may activate devices (the grant's seats) and the tenant decides WHEN,
// so an operator never holds a credential it could use to bind a device to
// somebody else's tenant. An operator gets 403 rather than a hidden 404: it is
// allowed to know this endpoint exists and is not for it.
func (s *server) handleAdminClaimCode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", 403)
		return
	}
	var req claimCodeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	ttl, err := normalizeClaimCodeTTL(req.TTLSec)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	c := s.state.Claims[req.ID]
	// Another tenant's grant reports 404, not 403: whether that grant exists is
	// not something a tenant gets to learn.
	if c == nil || c.TenantID != id.TenantID {
		s.mu.Unlock()
		http.Error(w, "unknown grant", 404)
		return
	}
	if c.Revoked {
		s.mu.Unlock()
		http.Error(w, "grant revoked", 403)
		return
	}
	// A full grant may still generate a code: seats count devices, and an
	// already-activated device must be able to re-enroll (factory reset) without
	// the operator handing out another seat. The enroll path enforces the limit
	// for NEW devices.
	now := time.Now().Unix()
	code := newClaimCode()
	c.CodeHash = hashClaimCode(code)
	c.CodeIssuedAt = now
	c.CodeExpiresAt = now + int64(ttl)
	c.CodeIssuedBy = id.Username
	c.CodeUsedAt = 0
	s.save()
	name := c.Name
	s.mu.Unlock()

	log.Printf("console: tenant %s generated a %ds code for grant %s", id.TenantID, ttl, req.ID)
	s.admin.publish("claim.updated", id.TenantID, nil)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, claimCodeResponse{
		ID: req.ID, Name: name, Code: code, IssuedAt: now, ExpiresAt: now + int64(ttl),
	})
}

type revokeClaimRequest struct {
	ID string `json:"id"`
}

func (s *server) handleAdminClaimRevoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
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
	c, ok := s.state.Claims[req.ID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown grant", 404)
		return
	}
	tenantID := c.TenantID
	c.Revoked = true
	// Revoking is the operator's "stop, now". Clearing the hash means a code that
	// was screenshotted a minute ago is dead on its own terms, not merely behind
	// a flag every future enroll path must remember to check.
	c.CodeHash = ""
	s.save()
	s.mu.Unlock()

	// Revoking a grant stops FUTURE enrollment. Devices that already redeemed it
	// keep their certificates: withdrawing an identity is a different action
	// (certificate revocation), and this button must not imply it did that.
	log.Printf("console: grant %s revoked (already-enrolled devices unaffected)", req.ID)
	s.admin.publish("claim.revoked", tenantID, nil)
	writeJSON(w, map[string]any{"ok": true})
}

type updateClaimRequest struct {
	ID      string `json:"id"`
	MaxUses int    `json:"max_uses"`
}

// handleAdminClaimUpdate changes a grant's activation limit. Lowering it below
// the number of devices it already activated is refused: those devices hold
// certificates, so "used 5 of 3" is a state the operator cannot actually reach
// and the console must not pretend otherwise.
func (s *server) handleAdminClaimUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req updateClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	maxUses, err := normalizeClaimMaxUses(req.MaxUses)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	c, ok := s.state.Claims[req.ID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown grant", 404)
		return
	}
	if used := seatsTakenLocked(c); maxUses < used {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("max_uses cannot be below the %d seats already taken (%d 已入网 + %d 预授权未认领)",
			used, len(c.Serials), len(c.Reserved)), 400)
		return
	}
	c.MaxUses = maxUses
	view := claimView(c)
	s.save()
	s.mu.Unlock()

	log.Printf("console: grant %s max_uses set to %d (%d used)", req.ID, maxUses, len(view.Serials))
	s.admin.publish("claim.updated", view.TenantID, nil)
	writeJSON(w, view)
}

// ------------------------------------------------------------------ accounts

// consoleAccount is the account row the console renders. It never carries the
// password hash.
type consoleAccount struct {
	Username    string `json:"username"`
	Role        string `json:"role"`
	TenantID    string `json:"tenant_id,omitempty"`
	CreatedAt   int64  `json:"created_at_unix"`
	LastLogin   int64  `json:"last_login_unix,omitempty"`
	Disabled    bool   `json:"disabled"`
	InitialPass bool   `json:"initial_password"`
}

func accountView(a *Account) *consoleAccount {
	return &consoleAccount{
		Username: a.Username, Role: a.Role, TenantID: a.TenantID,
		CreatedAt: a.CreatedAt, LastLogin: a.LastLogin,
		Disabled: a.Disabled, InitialPass: a.InitialPass,
	}
}

type createAccountRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	TenantID string `json:"tenant_id"`
	Password string `json:"password"`
}

func (s *server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r); !ok {
		return
	}
	if r.Method == http.MethodGet {
		s.mu.Lock()
		out := make([]*consoleAccount, 0, len(s.state.Accounts))
		for _, a := range s.state.Accounts {
			out = append(out, accountView(a))
		}
		s.mu.Unlock()
		sort.Slice(out, func(i, j int) bool {
			if out[i].Role != out[j].Role {
				return out[i].Role < out[j].Role
			}
			return out[i].Username < out[j].Username
		})
		writeJSON(w, out)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST required", 405)
		return
	}
	var req createAccountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	username := strings.TrimSpace(req.Username)
	if !validUsername(username) {
		http.Error(w, "account must be 3..40 chars of a-z A-Z 0-9 - _ .", 400)
		return
	}
	role := req.Role
	if role == "" {
		role = roleTenant
	}
	if role != roleOperator && role != roleTenant {
		http.Error(w, "role must be operator or tenant", 400)
		return
	}
	tenantID := strings.TrimSpace(req.TenantID)
	if role == roleTenant && tenantID == "" {
		http.Error(w, "tenant_id required for a tenant account", 400)
		return
	}
	if role == roleOperator {
		tenantID = ""
	}

	pw := req.Password
	generated := false
	if pw == "" {
		pw = newPassword()
		generated = true
	} else if err := checkPasswordPolicy(pw); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pwHash, err := hashPassword(pw)
	if err != nil {
		http.Error(w, "cannot hash password", 500)
		return
	}

	s.mu.Lock()
	if _, exists := s.state.Accounts[username]; exists {
		s.mu.Unlock()
		http.Error(w, "account already exists", 409)
		return
	}
	if role == roleTenant {
		if _, ok := s.state.Tenants[tenantID]; !ok {
			s.mu.Unlock()
			http.Error(w, "unknown tenant", 404)
			return
		}
	}
	a := &Account{
		Username: username, Role: role, TenantID: tenantID, Password: pwHash,
		CreatedAt: time.Now().Unix(), InitialPass: true,
	}
	s.state.Accounts[username] = a
	s.save()
	view := accountView(a)
	s.mu.Unlock()

	log.Printf("console: account %q created (role=%s tenant=%s)", username, role, tenantID)
	writeJSON(w, map[string]any{"account": view, "password": conditionalString(generated, pw)})
}

func conditionalString(include bool, v string) any {
	if !include {
		return nil
	}
	return v
}

type resetAccountRequest struct {
	Username string `json:"username"`
}

// handleAdminAccountReset issues a fresh one-time password. Reset, disable and
// password change all drop that account's sessions: a new credential must not
// leave the old cookie working.
func (s *server) handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	op, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req resetAccountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	username := strings.TrimSpace(req.Username)
	pw := newPassword()
	pwHash, err := hashPassword(pw)
	if err != nil {
		http.Error(w, "cannot hash password", 500)
		return
	}
	s.mu.Lock()
	a, ok := s.state.Accounts[username]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown account", 404)
		return
	}
	a.Password = pwHash
	a.InitialPass = true
	s.save()
	view := accountView(a)
	s.mu.Unlock()
	s.dropSessions(username)

	log.Printf("console: password reset for %q by %s", username, op.Username)
	writeJSON(w, map[string]any{"account": view, "password": pw})
}

type disableAccountRequest struct {
	Username string `json:"username"`
	Disabled bool   `json:"disabled"`
}

func (s *server) handleAdminAccountDisable(w http.ResponseWriter, r *http.Request) {
	op, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req disableAccountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == op.Username {
		http.Error(w, "cannot disable the account you are logged in as", 400)
		return
	}
	s.mu.Lock()
	a, ok := s.state.Accounts[username]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown account", 404)
		return
	}
	a.Disabled = req.Disabled
	s.save()
	view := accountView(a)
	s.mu.Unlock()
	if req.Disabled {
		s.dropSessions(username)
	}

	log.Printf("console: account %q disabled=%v by %s", username, req.Disabled, op.Username)
	writeJSON(w, map[string]any{"account": view})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// handleAdminPassword is self-service, for the logged-in account only. The
// break-glass token session has no account and is refused.
func (s *server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	if id.Via != "password" {
		http.Error(w, "the operator token has no password to change", 400)
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if err := checkPasswordPolicy(req.NewPassword); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	a := s.state.Accounts[id.Username]
	if a == nil {
		s.mu.Unlock()
		http.Error(w, "unknown account", 404)
		return
	}
	cur := *a
	s.mu.Unlock()

	if !verifyPassword(cur.Password, req.OldPassword) {
		http.Error(w, "current password is incorrect", 403)
		return
	}
	pwHash, err := hashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, "cannot hash password", 500)
		return
	}
	s.mu.Lock()
	if a := s.state.Accounts[id.Username]; a != nil {
		a.Password = pwHash
		a.InitialPass = false
	}
	s.save()
	s.mu.Unlock()

	// Re-issue the caller's own session after dropping every session of the
	// account, so changing a password on one machine logs out the others.
	s.dropSessions(id.Username)
	s.newSession(w, consoleIdentity{
		Role: id.Role, TenantID: id.TenantID, Username: id.Username, Via: "password",
	})
	log.Printf("console: password changed for %q", id.Username)
	writeJSON(w, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ events

func (s *server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	id := s.session(r)
	if id.Role == "" {
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
	n := s.admin.next
	s.admin.next++
	s.admin.subs[n] = adminSub{ch: ch, role: id.Role, tenantID: id.TenantID}
	s.admin.mu.Unlock()
	defer func() {
		s.admin.mu.Lock()
		delete(s.admin.subs, n)
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
