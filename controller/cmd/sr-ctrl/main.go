// srou controller — M-C0 skeleton.
//
// What this is: a working cloud-controller core good enough to prove the
// three mechanisms the product depends on, against real NATs:
//
//  1. zero-touch enrollment (claim code -> device cert -> tenant identity)
//  2. the endpoint registry / rendezvous table (who is where, pushed on change)
//  3. address discovery with TWO witnesses, because they answer different
//     questions: the STUN reflector says what a third party saw (enough to send
//     the first packet), and a node's own dataplane says what a peer's traffic
//     looked like when it arrived here (the only address that is actually a path
//     back under a symmetric NAT). The /v1/observe path is that second witness.
//  4. the tenant's dataplane facts: which UDP function ports to use, and the
//     hop-MAC key ring the SR header is signed with.
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
	"crypto/sha256"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/underlay"
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

	// UptimeSec is the agent process's own uptime, as of its last heartbeat.
	// Stored rather than derived because the controller has no other way to tell
	// "the box rebooted 40 seconds ago" from "the box has been up for a month";
	// a zero means the agent did not report one (the --once path), not that the
	// process just started.
	UptimeSec int64 `json:"uptime_sec,omitempty"`

	// VpnPort / TransitPort are what this node's dataplane says it listens on.
	// Seeded from the tenant's pushed values at enrollment, then overwritten by
	// the node's own report -- because a node that had to rebind is reachable
	// only at the port it actually holds.
	VpnPort     uint16 `json:"vpn_port"`
	TransitPort uint16 `json:"transit_port"`

	// LocalAddrs is what the node declared as its own interface addresses. It is
	// the lookup table for attributing an observed outer source (see
	// attributeLocked): the controller may only name a source some node has
	// claimed over its authenticated channel.
	LocalAddrs []string `json:"local_addrs,omitempty"`

	// Tunnels is the node's declared tunnel table (peer + this node's overlay
	// address on that tunnel), in canonical order. It is relayed PER READER in
	// PeerInfo.OverlayIPs: the route protocol puts no next hop on a prefix, so a
	// peer resolves the routes it learns from this node against the address this
	// node declared for the link between them.
	Tunnels []wire.TunnelEndpoint `json:"tunnels,omitempty"`

	// Binding is the device's attested X25519 key (verified before storage --
	// see applyBinding), and per the wire contract that key IS the node's
	// WireGuard static public key. The controller stores and relays it; it can
	// check it but can never produce one, because it does not hold the device's
	// identity key.
	Binding *keybinding.Binding `json:"key_binding,omitempty"`
	KeyGen  uint32              `json:"key_gen,omitempty"`

	// Config is the device's own report of the customer-side configuration it
	// runs (see wire.ConfigState). It is PERSISTED, unlike telemetry: "this box
	// is on v7" is still true after a controller restart and while the box is
	// offline, and the console must be able to show the last thing it said.
	Config *wire.ConfigState `json:"config_state,omitempty"`

	// Tunnel is the device's own report of the tunnel configuration it runs
	// (wire.TunnelState). Persisted for the same reason as Config: the last
	// thing the box said about its tunnels is still the best fact available
	// while it is offline.
	Tunnel *wire.TunnelState `json:"tunnel_state,omitempty"`

	// Ifaces is the device's interface inventory (names, L3 MTU, state, its own
	// IPv4 addresses), reported when the agent runs with -vpp-api. Persisted for
	// the same reason as Config: the controller needs it to plan a tunnel, and a
	// device that has gone offline still has the interfaces it had a minute ago.
	//
	// It is a POINTER because "never reported" and "reported an empty table" are
	// different facts. The underlay planner treats nil as "unknown" (the
	// interface-name check is skipped rather than rejecting every name) and the
	// page says 未上报 instead of drawing an empty list.
	Ifaces      *[]wire.IfaceInfo `json:"ifaces,omitempty"`
	InventoryAt int64             `json:"inventory_at_unix,omitempty"`

	// MappingAt is when the reflector's view of this device's source address
	// (UDPMapping) last arrived. An observation is a claim about a NAT binding
	// that was true a minute ago, so the underlay planner refuses to build a
	// tunnel on one older than observationTTL -- the address is still shown, with
	// its age, so the tenant can see WHY a tunnel cannot be created.
	MappingAt int64 `json:"mapping_at_unix,omitempty"`

	// SiteID is which of the tenant's sites this box physically sits in, and it is
	// the only reason the topology can be drawn on a map. Console-only: a
	// coordinate never enters a config bundle and never reaches a device. Empty
	// means the tenant never placed this device, and the topology lists it as
	// 未上报 rather than inventing a point for it.
	SiteID string `json:"site_id,omitempty"`
}

// Tenant is the customer account a device belongs to. Tenants are created in
// the console; enrollment never creates one, because a claim code always points
// at a tenant that already exists.
type Tenant struct {
	ID        string `json:"tenant_id"`
	Name      string `json:"name,omitempty"`
	CreatedAt int64  `json:"created_at_unix"`
}

// Claim is one enrollment grant: the operator's promise that a tenant may
// activate up to MaxUses devices. It carries no configuration -- its only
// authority is "bind the holder of a valid code to TenantID". Serials lists
// exactly the devices this grant activated, so len(Serials) is both the audit
// trail and the seat count the console shows.
//
// The secret is NOT stored. A tenant generates a short-lived code from the
// grant, and the controller keeps only its SHA-256 plus issue metadata. The
// plaintext exists in exactly one place: the response to the generation call.
// That is why neither State, /api/state nor the logs can leak a usable code.
type Claim struct {
	ID        string   `json:"id"`
	TenantID  string   `json:"tenant_id"`
	Name      string   `json:"name,omitempty"`
	CreatedAt int64    `json:"created_at_unix"`
	Revoked   bool     `json:"revoked,omitempty"`
	MaxUses   int      `json:"max_uses"`
	Serials   []string `json:"serials,omitempty"`

	// CodeHash is the SHA-256 of the currently issued code. Empty means "no
	// live code": never generated, already expired, or rotated out. Generating
	// a new code replaces these fields, so a grant never has two valid secrets.
	CodeHash      string `json:"code_hash,omitempty"`
	CodeIssuedAt  int64  `json:"code_issued_at_unix,omitempty"`
	CodeExpiresAt int64  `json:"code_expires_at_unix,omitempty"`
	CodeIssuedBy  string `json:"code_issued_by,omitempty"`
	CodeUsedAt    int64  `json:"code_used_at_unix,omitempty"`

	// Reserved lists the 预授权 records (see preauth.go) that hold a seat on this
	// grant but have no device yet. It is a ledger of record ids, so a seat is
	// never counted twice and revoking a record releases exactly what it took.
	Reserved []string `json:"reserved,omitempty"`

	// Legacy fields, read-only migration input. State files written before
	// codes became ephemeral keep the plaintext code (as the map key and in
	// "code"), a note, and a grant-level expiry. normalizeState turns them into
	// CodeHash / Name / CodeExpiresAt and clears them; they are never written
	// back, so a saved state no longer contains a usable code.
	LegacyCode      string `json:"code,omitempty"`
	LegacyNote      string `json:"note,omitempty"`
	LegacyExpiresAt int64  `json:"expires_at_unix,omitempty"`
}

// hashClaimCode maps a presented code to the value stored in State. SHA-256 is
// enough here: the code carries 157 bits of entropy, so there is no dictionary
// to slow down -- a slow KDF would only add cost to every enrollment.
func hashClaimCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// claimMap reads both the current record shape and the legacy
// `{"claim-a":"tenant-a"}` shape, so an existing lab state file keeps working.
// Keys are grant ids now; files written before the id field existed key the map
// by the plaintext code, which normalizeState detects and converts.
type claimMap map[string]*Claim

func (m *claimMap) UnmarshalJSON(b []byte) error {
	var recs map[string]*Claim
	if err := json.Unmarshal(b, &recs); err == nil && recs != nil {
		// A legacy file fails this decode ("cannot unmarshal string into ..."),
		// so reaching here means the values really are claim records.
		*m = recs
		return nil
	}
	var legacy map[string]string
	if err := json.Unmarshal(b, &legacy); err != nil {
		return err
	}
	out := make(claimMap, len(legacy))
	for code, tenant := range legacy {
		out[code] = &Claim{LegacyCode: code, TenantID: tenant}
	}
	*m = out
	return nil
}

// State is the whole persistent world. In production these become rows in
// Postgres; the shape is deliberately flat so the mapping is trivial.
type State struct {
	Nodes      map[string]*NodeRecord `json:"nodes"`
	NextNodeID map[string]uint16      `json:"next_node_id"` // per tenant
	Epoch      uint32                 `json:"epoch"`        // monotonic, survives restart
	Tenants    map[string]*Tenant     `json:"tenants"`
	Claims     claimMap               `json:"claims"` // claim id -> Claim (grant)
	// PKeyRings holds each tenant's hop-MAC key ring. This is DOMAIN material:
	// shared across the tenant on purpose, because a node has to verify headers
	// signed by nodes it never talked to (a transit hop). What is NOT here is any
	// per-pair material -- the tunnel key is negotiated by the endpoints from the
	// bindings this controller only relays, and nothing in this file could
	// compute one.
	//
	// It is NOT pushed at enrollment: the key ring is part of the node plan a
	// tenant configures in the console, so a freshly enrolled device has no key
	// material until it has been configured.
	PKeyRings map[string][]wire.PKey `json:"p_key_rings"`

	// Accounts is the console's login table, keyed by username. A tenant account
	// belongs to exactly one tenant and can only ever see that tenant's rows; an
	// operator account has no tenant and manages tenants and claim codes. The
	// operator token from the command line remains a separate break-glass login
	// and is not stored here -- it is a startup secret, not an account.
	Accounts map[string]*Account `json:"accounts"`

	// Configs holds each device's customer-side draft, keyed by "tenant|node".
	// It is configuration, not runtime: unlike telemetry it must survive a
	// restart, and it is scoped to the tenant that owns the device -- the
	// operator role never reads it.
	Configs map[string]*DeviceConfig `json:"configs"`

	// Underlays is each tenant's tunnel layer, keyed by tenant id. It is
	// configuration like Configs: the pools, the tunnels they allocated and the
	// path templates several tunnels share. Kept separate from Configs because
	// the scope is different -- a tunnel belongs to a tenant, not to one device,
	// and it references two devices that must both exist.
	Underlays map[string]*UnderlayConfig `json:"underlays"`

	// Sites is each tenant's location table, keyed by site id. Console-only:
	// nothing here is pushed to a device, because a coordinate means nothing to
	// the dataplane. It exists so several boxes in one building share one position
	// instead of each carrying its own typed-in copy.
	Sites map[string]*Site `json:"sites"`

	// PreAuths is the tenant's on-paper device list: names and site assignments
	// for boxes that have not enrolled yet, keyed by pre-authorization id. It is
	// deliberately NOT a NodeRecord with a reserved node id -- a node id only ever
	// comes into existence during enrollment, and inventing one early would put
	// the console ahead of the one fact the dataplane depends on.
	PreAuths map[string]*PreAuth `json:"preauths"`

	// PreAuthConfigs holds each pre-authorization's draft, keyed by
	// "tenant|preauth". Separate from Configs because its key has no node: the
	// draft moves into Configs at the enrollment that claims the record, and a
	// draft that never got claimed is dropped with the record.
	PreAuthConfigs map[string]*DeviceConfig `json:"preauth_configs"`
}

// Account is one console login. Passwords are stored only as PBKDF2 hashes
// (see password.go). Role "operator" may manage tenants, claim codes and
// accounts; role "tenant" may only read its own tenant's devices and codes.
type Account struct {
	Username    string `json:"username"`
	Role        string `json:"role"`                // operator | tenant
	TenantID    string `json:"tenant_id,omitempty"` // required for role=tenant
	Password    string `json:"password"`            // pbkdf2$sha256$...
	CreatedAt   int64  `json:"created_at_unix"`
	LastLogin   int64  `json:"last_login_unix,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	InitialPass bool   `json:"initial_password,omitempty"` // true until the holder changes it
}

// Store is the persistence seam. F1 (Postgres) implements this and nothing
// above it changes.
type Store interface {
	Load() (*State, error)
	Save(*State) error
}

type fileStore struct{ path string }

// newClaimID is a grant's stable, public handle. It is random only so ids do
// not invite enumeration; unlike a code it carries no authority.
func newClaimID() string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return "clm_" + string(out)
}

// newSiteID is a site's public handle. Same rule as a grant id: random so ids do
// not invite enumeration, and carrying no authority of its own.
func newSiteID() string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return "site_" + string(out)
}

func (f *fileStore) Load() (*State, error) {
	b, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return seedState(), nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if normalizeState(&s) {
		// The migration exists to remove plaintext codes from disk; leaving the
		// old file in place would keep the very secret this change eliminates.
		log.Printf("state: migrated legacy claim records (codes are now stored hashed)")
		if err := f.Save(&s); err != nil {
			return nil, fmt.Errorf("saving migrated state: %w", err)
		}
	}
	return &s, nil
}

// seedState is the demo world the harness and the lab start from: two tenants
// and three enrollment grants, so multi-tenancy is provable out of the box. The
// demo codes are the fixed strings the vBond harness types ("claim-a"...); they
// are hashed like any other code, and this file is the only place they exist in
// plaintext. Console creation appends to it; nothing here is special.
func seedState() *State {
	now := time.Now().Unix()
	s := &State{
		Nodes:      map[string]*NodeRecord{},
		NextNodeID: map[string]uint16{},
		Tenants:    map[string]*Tenant{},
		Claims:     claimMap{},
		PKeyRings:  map[string][]wire.PKey{},
		Accounts:   map[string]*Account{},
		Configs:    map[string]*DeviceConfig{},
		Underlays:  map[string]*UnderlayConfig{},
		Sites:      map[string]*Site{},
		PreAuths:   map[string]*PreAuth{},

		PreAuthConfigs: map[string]*DeviceConfig{},
	}
	for id, name := range map[string]string{"tenant-a": "演示租户 A", "tenant-b": "演示租户 B"} {
		s.Tenants[id] = &Tenant{ID: id, Name: name, CreatedAt: now}
	}
	for code, tenant := range map[string]string{"claim-a": "tenant-a", "claim-b": "tenant-a", "claim-c": "tenant-b"} {
		c := &Claim{ID: newClaimID(), TenantID: tenant, Name: "演示入网授权",
			CreatedAt: now, MaxUses: claimDefaultMaxUses,
			CodeHash: hashClaimCode(code), CodeIssuedAt: now}
		s.Claims[c.ID] = c
	}
	return s
}

// normalizeState repairs anything a hand-edited or older state file can be
// missing. A claim that names no tenant authorizes nothing and is dropped; a
// tenant referenced by a node or claim exists by definition, so it is
// backfilled instead of leaving a row the console cannot render.
//
// It reports whether it converted legacy plaintext claim codes, because that is
// the one repair the caller must persist immediately: the file on disk would
// otherwise still contain a usable secret.
func normalizeState(s *State) bool {
	if s.Nodes == nil {
		s.Nodes = map[string]*NodeRecord{}
	}
	if s.NextNodeID == nil {
		s.NextNodeID = map[string]uint16{}
	}
	if s.Tenants == nil {
		s.Tenants = map[string]*Tenant{}
	}
	if s.Claims == nil {
		s.Claims = claimMap{}
	}
	if s.PKeyRings == nil {
		s.PKeyRings = map[string][]wire.PKey{}
	}
	if s.Accounts == nil {
		s.Accounts = map[string]*Account{}
	}
	if s.Configs == nil {
		s.Configs = map[string]*DeviceConfig{}
	}
	if s.Underlays == nil {
		s.Underlays = map[string]*UnderlayConfig{}
	}
	// A tenant account whose tenant no longer exists authorizes nothing; drop it
	// rather than leave a login that can only ever see an empty world.
	for name, a := range s.Accounts {
		if a == nil || a.Username == "" || a.Password == "" {
			delete(s.Accounts, name)
			continue
		}
		if a.Role != "operator" && a.Role != "tenant" {
			delete(s.Accounts, name)
			continue
		}
		if a.Role == "tenant" {
			if _, ok := s.Tenants[a.TenantID]; !ok {
				delete(s.Accounts, name)
			}
		}
	}
	now := time.Now().Unix()
	migrated := false
	claims := make(claimMap, len(s.Claims))
	for mapKey, c := range s.Claims {
		if c == nil {
			migrated = true
			continue
		}
		if c.ID == "" {
			// Old files key the map by the plaintext code and have no id.
			c.ID = newClaimID()
			if c.LegacyCode == "" {
				c.LegacyCode = mapKey
			}
			migrated = true
		}
		// A record whose key is not its id is an old file, whichever fields it
		// happens to carry. The key is still the code that file was issued for.
		if c.LegacyCode == "" && mapKey != c.ID && c.CodeHash == "" {
			c.LegacyCode = mapKey
		}
		if c.LegacyCode != "" {
			if c.CodeHash == "" {
				c.CodeHash = hashClaimCode(c.LegacyCode)
				if c.CodeExpiresAt == 0 && c.LegacyExpiresAt > 0 {
					c.CodeExpiresAt = c.LegacyExpiresAt
				}
				if c.CodeIssuedAt == 0 {
					c.CodeIssuedAt = c.CreatedAt
				}
			}
			migrated = true
		}
		if c.Name == "" {
			c.Name = c.LegacyNote
			if c.Name == "" {
				c.Name = "入网授权"
			}
		}
		if c.LegacyCode != "" || c.LegacyNote != "" || c.LegacyExpiresAt != 0 {
			migrated = true
		}
		c.LegacyCode, c.LegacyNote, c.LegacyExpiresAt = "", "", 0
		if c.CreatedAt == 0 {
			c.CreatedAt = now
		}
		if c.TenantID == "" {
			migrated = true
			continue
		}
		// Older records reused one list for every enrollment attempt, so the
		// same serial can appear twice. Seats are distinct devices, and the
		// limit must never sit below what this grant has already activated.
		seen := map[string]bool{}
		uniq := c.Serials[:0]
		for _, serial := range c.Serials {
			if serial != "" && !seen[serial] {
				seen[serial] = true
				uniq = append(uniq, serial)
			}
		}
		c.Serials = uniq
		if c.MaxUses <= 0 {
			c.MaxUses = claimDefaultMaxUses
		}
		if c.MaxUses > claimMaxUsesLimit {
			c.MaxUses = claimMaxUsesLimit
		}
		if c.MaxUses < len(c.Serials) {
			c.MaxUses = len(c.Serials)
		}
		if claims[c.ID] != nil {
			c.ID = newClaimID()
			migrated = true
		}
		claims[c.ID] = c
		ensureTenant(s, c.TenantID, now)
	}
	s.Claims = claims
	for _, n := range s.Nodes {
		if n != nil && n.TenantID != "" {
			ensureTenant(s, n.TenantID, now)
		}
	}
	// Config drafts are keyed by tenant|node and must stay reachable: repair the
	// key from the record and drop only entries that name no tenant at all.
	configs := make(map[string]*DeviceConfig, len(s.Configs))
	for _, dc := range s.Configs {
		if dc == nil || dc.TenantID == "" || dc.NodeID == 0 {
			continue
		}
		ensureTenant(s, dc.TenantID, now)
		normalizeBasicConfig(&dc.Draft)
		if dc.Version <= 0 {
			dc.Version = 1
		}
		configs[configKey(dc.TenantID, dc.NodeID)] = dc
	}
	s.Configs = configs
	// Underlay rows are keyed by tenant and must prove it: a row whose tenant id
	// disagrees with its key (or names none) is dropped rather than rehomed, since
	// rehoming would hand one tenant another's tunnels. Everything else a decode
	// loses -- nil maps, a zero settings block -- is repaired in place.
	for tenantID, uc := range s.Underlays {
		if uc == nil || tenantID == "" || (uc.TenantID != "" && uc.TenantID != tenantID) {
			delete(s.Underlays, tenantID)
			continue
		}
		uc.TenantID = tenantID
		uc.Draft = uc.Draft.Normalize()
		if uc.State == nil {
			uc.State = underlay.New(underlay.DefaultSettings())
		}
		uc.State.Repair()
	}
	// Sites are console-only, but a stale reference is still a lie: a device
	// pointing at a site that no longer exists (or belongs to another tenant)
	// would be drawn at someone else's coordinates. Clear those and let the
	// topology say 未上报.
	if normalizeSites(s) {
		migrated = true
	}
	if normalizePreAuths(s) {
		migrated = true
	}
	return migrated
}

func ensureTenant(s *State, id string, now int64) *Tenant {
	if t, ok := s.Tenants[id]; ok && t != nil {
		return t
	}
	t := &Tenant{ID: id, Name: id, CreatedAt: now}
	s.Tenants[id] = t
	return t
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

	caCert  *x509.Certificate
	caKey   ed25519.PrivateKey
	dataDir string

	// admin is the human-facing console: a separate listener with a token
	// session, because a browser cannot present a device certificate. It reads
	// the same state under the same mutex and never takes part in enrollment
	// authorization -- a console user cannot mint a device identity, only a
	// claim code that the device itself must redeem.
	admin      *adminHub
	adminToken string

	// obs is the observation store: per (tenant, OBSERVER), the attributed outer
	// sources that observer reported. Deliberately NOT part of State, and
	// therefore not persisted: an observation is only a fact while it is fresh
	// (the dataplane table it came from ages out on its own), so resurrecting one
	// after a controller restart would advertise a dead address as current.
	// Keyed by observer rather than by target because there is no target-wide
	// answer: under a symmetric NAT every observer sees a different port.
	obs map[string]observerView

	// pendingObs holds reported sources nothing could be matched to yet, kept per
	// observer so that a node which declares its addresses AFTER the observation
	// arrived still gets attributed. Two CPEs enrolling at once makes that ordering
	// real, not hypothetical.
	pendingObs map[string]map[string]*pendingObs

	// tel holds each node's latest telemetry window, keyed by node key. Like obs
	// it is NOT part of State: a metrics window is a statement about now, and a
	// persisted one would come back after a restart claiming an "up" that stopped
	// being true when the controller died. telTTLSec bounds how long a window
	// outlives the report that produced it.
	tel       map[string]*nodeTelemetry
	telTTLSec int
}

// pendingObs is an observation the controller could not name yet, kept with the
// time it arrived (so it expires like an attributed row instead of lingering until
// some future node happens to claim the address) and the reason it is still
// unnamed, which is what the reporter was told.
type pendingObs struct {
	ob     wire.Observation
	reason string
	seenAt int64
}

// observerView is one observer's attributed rows, keyed by the TARGET node.
type observerView map[uint16]*attributedObs

// attributedObs is the controller's answer to "where did node X's traffic look
// like it came from, as seen by me".
type attributedObs struct {
	mapping  string
	dstPort  uint16
	verified bool
	hits     uint64
	ageSec   uint64
	seenAt   int64 // controller clock, at report time -- used for expiry
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

	claim := s.findClaimByCodeLocked(req.ClaimCode)
	if claim == nil || claim.TenantID == "" {
		log.Printf("enroll REJECTED serial=%s (invalid claim code)", req.Serial)
		http.Error(w, "invalid claim code", 403)
		return
	}
	if claim.Revoked {
		log.Printf("enroll REJECTED serial=%s (revoked grant %s)", req.Serial, claim.ID)
		http.Error(w, "claim code revoked", 403)
		return
	}
	if claim.CodeExpiresAt > 0 && time.Now().Unix() > claim.CodeExpiresAt {
		log.Printf("enroll REJECTED serial=%s (expired code on grant %s)", req.Serial, claim.ID)
		http.Error(w, "claim code expired", 403)
		return
	}
	tenant := claim.TenantID
	if _, ok := s.state.Tenants[tenant]; !ok {
		log.Printf("enroll REJECTED serial=%s (grant %s points at unknown tenant %s)", req.Serial, claim.ID, tenant)
		http.Error(w, "claim code references an unknown tenant", 403)
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
	// 预授权认领: the engineer at the rack named the record this box should take
	// over. D3 -- an index that does not resolve is a refusal, never a quiet
	// fallback to a plain enrollment: the alternative is a box that enrolls,
	// receives none of the configuration waiting for it, and looks fine.
	paName := strings.TrimSpace(req.PreAuth)
	var pa *PreAuth
	if paName != "" {
		pa = s.preAuthByNameLocked(tenant, paName)
		if pa == nil {
			msg := fmt.Sprintf("租户 %s 下没有名为「%s」的预授权；未认领的索引：%s",
				tenant, paName, s.unclaimedPreAuthNamesLocked(tenant))
			log.Printf("enroll REJECTED serial=%s (预授权 %s)", req.Serial, paName)
			http.Error(w, msg, 403)
			return
		}
	}
	if rec == nil {
		if pa != nil {
			if refusal := s.preAuthRefusalLocked(claim, pa, req.Serial); refusal != "" {
				log.Printf("enroll REJECTED serial=%s (预授权 %s): %s", req.Serial, pa.ID, refusal)
				http.Error(w, refusal, 403)
				return
			}
			// The seat is already reserved (D2): releasing it before the check is
			// what turns "reserved" into "activated" without charging twice.
			claim.Reserved = removeString(claim.Reserved, pa.ID)
		}
		// A seat is a device, not an enrollment: re-enrolling a serial that
		// already belongs to this tenant is free, even when the code is full.
		if claim.MaxUses > 0 && seatsTakenLocked(claim) >= claim.MaxUses {
			log.Printf("enroll REJECTED serial=%s (grant %s used %d/%d, 预授权未认领 %d)",
				req.Serial, claim.ID, len(claim.Serials), claim.MaxUses, len(claim.Reserved))
			msg := "claim code has no activations left"
			if len(claim.Reserved) > 0 {
				// Telling the site which half of the limit is paper is the only
				// way this failure becomes actionable from a console log.
				msg = fmt.Sprintf("%s（已入网 %d 台 + 预授权未认领 %d 个 = %d/%d；先在控制台撤销一条预授权或提高名额）",
					msg, len(claim.Serials), len(claim.Reserved), seatsTakenLocked(claim), claim.MaxUses)
			}
			http.Error(w, msg, 403)
			return
		}
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
		// Serials is both the audit trail and the seat ledger; only a device
		// this grant actually activated belongs in it.
		claim.Serials = append(claim.Serials, req.Serial)
		claim.CodeUsedAt = time.Now().Unix()
		if pa != nil {
			s.bindPreAuthLocked(rec, pa, claim, req.Serial, time.Now().Unix())
			log.Printf("enroll: serial=%s -> tenant=%s node=%d 认领预授权「%s」(grant %s %d/%d)",
				req.Serial, tenant, id, pa.Name, claim.ID, len(claim.Serials), claim.MaxUses)
		} else {
			log.Printf("enroll: serial=%s -> tenant=%s node=%d (grant %s %d/%d)",
				req.Serial, tenant, id, claim.ID, len(claim.Serials), claim.MaxUses)
		}
	} else {
		log.Printf("enroll: serial=%s re-enrolled as tenant=%s node=%d", req.Serial, tenant, rec.NodeID)
		if pa != nil && (pa.BoundNodeID != rec.NodeID || pa.BoundSerial != req.Serial) {
			// Re-presenting the index that already claimed THIS node is normal (an
			// agent restart replays its flags). Anything else is a mistake worth a
			// refusal: silently moving a pre-authorized config onto a device that
			// already has its own would overwrite one tenant-visible config with
			// another without anyone publishing either.
			msg := fmt.Sprintf("这台设备已入网为 node %d；预授权「%s」还没有被它认领，配置不会自动挪过来。要合并请先在控制台撤销其一",
				rec.NodeID, pa.Name)
			log.Printf("enroll REJECTED serial=%s (预授权 %s 与已入网 node %d 冲突)", req.Serial, pa.ID, rec.NodeID)
			http.Error(w, msg, 403)
			return
		}
	}
	rec.PublicKey = base64.StdEncoding.EncodeToString(pub)
	rec.AgentVer = req.AgentVersion

	// Devices do not all enroll at once, and publishing only assigned transit
	// ports to the ones that existed then. Assign this one's now so its first
	// bundle already carries the tenant's published configuration. Not fatal:
	// the identity is worth more than the port, and the heartbeat retries.
	transitAssigned := false
	if port, assigned, err := s.ensureNodeTransitPortLocked(tenant, rec.NodeID); err != nil {
		log.Printf("underlay: tenant=%s node=%d 入网后补分配中转端口失败: %v", tenant, rec.NodeID, err)
	} else if assigned {
		log.Printf("underlay: tenant=%s node=%d 入网后补分配中转端口 %d", tenant, rec.NodeID, port)
		transitAssigned = true
	}

	certPEM, err := signDevice(s.caCert, s.caKey, ed25519.PublicKey(pub), tenant, rec.NodeID)
	if err != nil {
		http.Error(w, "signing failed", 500)
		return
	}
	rec.CertPEM = certPEM
	caPEM, _ := os.ReadFile(filepath.Join(s.dataDir, "ca.pem"))
	s.save()
	s.admin.publish("node.enrolled", tenant, rec)
	if transitAssigned {
		// The 全局配置 page lists every device's transit port; it just changed.
		s.admin.publish("node.underlay", tenant, rec)
	}

	resp := s.enrollResponse(tenant, rec, certPEM, string(caPEM))
	if pa != nil {
		// The echo is the agent's proof that the configuration it asked for is
		// the one attached to this identity.
		resp.PreAuthName = pa.Name
	}
	json.NewEncoder(w).Encode(resp)
}

// findClaimByCodeLocked resolves a presented code to its grant. The caller must
// hold s.mu. Comparison is over the digest: the plaintext code is never stored,
// so there is nothing to compare it to.
func (s *server) findClaimByCodeLocked(code string) *Claim {
	if code == "" {
		return nil
	}
	want := hashClaimCode(code)
	for _, c := range s.state.Claims {
		if c != nil && c.CodeHash == want {
			return c
		}
	}
	return nil
}

// expireClaimCodesLocked retires grants whose code window has closed. The
// enroll path already checks the deadline; clearing the hash here is what makes
// "expired" true of the stored record too, so a stale row cannot be resurrected
// by a later code path that forgets the deadline. The issue metadata stays for
// the audit trail and so the console can still say "the last code expired".
func (s *server) expireClaimCodesLocked(now int64) {
	for _, c := range s.state.Claims {
		if c == nil || c.CodeHash == "" || c.CodeExpiresAt == 0 || now <= c.CodeExpiresAt {
			continue
		}
		c.CodeHash = ""
		log.Printf("ttl: code on grant %s (tenant %s) expired", c.ID, c.TenantID)
		s.admin.publish("claim.updated", c.TenantID, nil)
	}
}

// enrollResponse is IDENTITY ONLY. Enrollment connects a device to a tenant; it
// is not a configuration channel. Ports, overlay addresses, keys and RR lists
// are part of the node plan, and the plan is pushed only after the customer
// configures the device in the console.
func (s *server) enrollResponse(tenant string, rec *NodeRecord, certPEM, caPEM string) wire.EnrollResponse {
	return wire.EnrollResponse{
		TenantID:     tenant,
		NodeID:       rec.NodeID,
		CertPEM:      certPEM,
		CAPEM:        caPEM,
		TLSAddrs:     s.tlsAddrs,
		HeartbeatSec: s.hbSec,
		Epoch:        s.advanceEpoch(),
	}
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
	firstContact := rec.LastSeen == 0
	rec.LastSeen = time.Now().Unix()
	rec.Stale = false
	// Only a positive value is a report. The one-shot agent path sends 0, and
	// treating that as "just booted" would erase a real uptime from the view.
	if req.Uptime > 0 {
		rec.UptimeSec = req.Uptime
	}
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
	// The device's account of what it runs. Stored as reported: the controller
	// does not infer "applied" from "published", and an absent Config (an older
	// agent) must not be turned into capable=false here -- that would accuse a
	// device of ignoring a channel it does not implement.
	configChanged := false
	if req.Config != nil && (rec.Config == nil || *rec.Config != *req.Config) {
		st := *req.Config
		rec.Config = &st
		configChanged = true
	}
	// Interface inventory. A nil report says "this agent has no VPP API client"
	// and must NOT clear what a previous run reported: the fact that the box has
	// an eth0 with MTU 1500 does not stop being true when the flag is dropped, and
	// InventoryAt is what makes its age visible. An empty non-nil list is stored
	// as such -- "the dataplane really has no interfaces" is a real answer.
	if req.Ifaces != nil {
		st := append([]wire.IfaceInfo(nil), (*req.Ifaces)...)
		rec.Ifaces = &st
		rec.InventoryAt = time.Now().Unix()
	}
	// Same rule as Config: stored as reported, never inferred from "published".
	tunnelChanged := false
	if req.Tunnel != nil && (rec.Tunnel == nil || *rec.Tunnel != *req.Tunnel) {
		st := *req.Tunnel
		rec.Tunnel = &st
		tunnelChanged = true
	}
	// Self-heal the transit port: a device that enrolled while the pool could not
	// cover it, or one whose row predates late-enrollment assignment, is repaired
	// on this beat -- and the beat's response advertises the new version, so the
	// device pulls a bundle that finally carries its port.
	transitAssigned := false
	if port, assigned, err := s.ensureNodeTransitPortLocked(tenant, nodeID); err != nil {
		log.Printf("underlay: tenant=%s node=%d 心跳补分配中转端口失败: %v", tenant, nodeID, err)
	} else if assigned {
		log.Printf("underlay: tenant=%s node=%d 心跳补分配中转端口 %d", tenant, nodeID, port)
		transitAssigned = true
	}
	s.save()

	// Coming back from stale, or offering key material peers have not seen, are
	// both events peers need: their view of this node just changed.
	if wasStale || keyChanged {
		s.broadcastLocked(tenant, wire.WatchEvent{Type: "update", Peer: s.toPeerInfoLocked(rec, 0), SentAt: time.Now().Unix()})
	}
	if wasStale || firstContact {
		// Console-facing: the device table's online/stale column just flipped.
		s.admin.publish("node.online", rec.TenantID, rec)
	}
	if configChanged {
		// Console-facing: the config page's applied/failed/applying column moved.
		s.admin.publish("node.config", rec.TenantID, rec)
	}
	if tunnelChanged || transitAssigned {
		// Console-facing: the underlay page's applied/failed column or a device's
		// transit port just moved.
		s.admin.publish("node.underlay", rec.TenantID, rec)
	}
	json.NewEncoder(w).Encode(wire.HeartbeatResponse{
		Epoch:         s.advanceEpoch(),
		PeerCount:     len(s.peersLocked(tenant, nodeID)),
		BindingErr:    bindingErr,
		ConfigVersion: s.desiredConfigVersionLocked(tenant, nodeID),
		TunnelVersion: s.underlayVersionLocked(tenant),
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
	if rep.UDPMapping != "" {
		// The underlay planner dates its observation by this, so it is refreshed on
		// every report even when the value did not change -- the point is that the
		// reflector saw the binding AGAIN, not that it moved.
		rec.MappingAt = time.Now().Unix()
	}
	rec.LocalPort = rep.LocalPort
	// A verdict is only stored when the reporter says it measured one. An agent
	// that has not run the two-reflector comparison sends the zero value, and
	// recording that as "not symmetric" would let the planner build a tunnel
	// that cannot work.
	if rep.SymmetricKnown {
		rec.Symmetric = rep.SymmetricNAT
	}
	rec.Reflector = rep.Reflector
	rec.LastSeen = time.Now().Unix()
	rec.Reach = "unknown"

	// The node's own ports win over the pushed ones, but only when it reports
	// them: a zero means "I am on what enrollment told me", and overwriting a
	// known port with 0 would point every peer at port 0.
	if rep.VpnPort != 0 && rep.VpnPort != rec.VpnPort {
		rec.VpnPort = rep.VpnPort
		changed = true
	}
	if rep.TransitPort != 0 && rep.TransitPort != rec.TransitPort {
		rec.TransitPort = rep.TransitPort
		changed = true
	}
	// Declared addresses are the attribution table. Store the node's own sorted
	// list so a change is detectable: an unsorted compare would report "changed"
	// on every report that merely reordered the same set.
	if addrs := normaliseAddrs(rep.LocalAddrs); len(addrs) > 0 && !equalStrings(addrs, rec.LocalAddrs) {
		rec.LocalAddrs = addrs
		// Rows that arrived before this node knew its own addresses may now have
		// an owner. Re-running attribution is what makes the enroll-vs-observe
		// race resolve on its own instead of needing a fixed order.
		reattributed := s.reattributeLocked(tenant)
		if reattributed > 0 {
			changed = true
			log.Printf("mapping: %d pending observation(s) now attributable to %s", reattributed, tenant)
		}
	}

	// Declared tunnel endpoints are what peers resolve a next hop against. nil
	// means "not reported" (keep the old table); an empty list means "I have no
	// tunnels" and must clear it -- a torn-down tunnel left in the registry would
	// keep handing peers an address that no longer answers.
	if rep.Tunnels != nil {
		tunnels, terr := wire.NormaliseTunnelEndpoints(nodeID, *rep.Tunnels)
		if terr != nil {
			s.mu.Unlock()
			http.Error(w, terr.Error(), 400)
			return
		}
		if !equalTunnels(tunnels, rec.Tunnels) {
			rec.Tunnels = tunnels
			changed = true
			log.Printf("mapping: %s/%d declared %d tunnel endpoint(s)", tenant, nodeID, len(tunnels))
		}
	}

	var bindingErr string
	keyChanged, err := s.applyBinding(rec, rep.Binding)
	if err != nil {
		bindingErr = err.Error()
		log.Printf("binding REJECTED tenant=%s node=%d: %v", tenant, nodeID, err)
	} else if keyChanged {
		// A new binding is itself a reason to push. Peers must install the new
		// WireGuard key BEFORE they try to talk; discovering it by packet loss
		// would make a rotation look like a link failure.
		changed = true
		log.Printf("pushing new key binding for %s/%d to peers", tenant, nodeID)
	}
	s.save()

	if changed {
		// This is the mechanism that answers "the CPE's public IP changed":
		// the registry notices and pushes. No dataplane change is needed.
		log.Printf("mapping CHANGED tenant=%s node=%d -> %q (local_port=%d symmetric=%v)",
			tenant, nodeID, rep.UDPMapping, rep.LocalPort, rep.SymmetricNAT)
		s.broadcastLocked(tenant, wire.WatchEvent{Type: "update", Peer: s.toPeerInfoLocked(rec, 0), SentAt: time.Now().Unix()})
	}
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "binding_err": bindingErr})
}

// handleObserve is the SECOND WITNESS of a node's address: not what a reflector
// saw the node send from, but what the node's traffic looked like when it ARRIVED
// at a peer. That distinction is the entire reason this endpoint exists -- under a
// symmetric NAT the reflector's answer is not a usable return path, and only the
// receiver of the first packet can name the port the reverse direction must use.
//
// The reply is synchronous and there is deliberately no broadcast: what an
// observation changes is the REPORTER's own view of a peer, which no other node
// can use (each has its own vantage). The reporter acts on this response; its
// later registry/watch rows carry the same values re-rendered per reader.
func (s *server) handleObserve(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	var rep wire.ObserveReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if rep.NodeID != nodeID {
		// Same iron rule as the heartbeat: a certificate for node 5 may not
		// report observations as node 7.
		http.Error(w, "node id does not match certificate", 403)
		return
	}

	s.mu.Lock()
	if _, ok := s.state.Nodes[key(tenant, nodeID)]; !ok {
		s.mu.Unlock()
		http.Error(w, "unknown node", 404)
		return
	}
	viewKey := key(tenant, nodeID)
	now := time.Now()
	if rep.Reset {
		// The agent cleared its dataplane table before dumping, so a source that
		// is absent really is gone. Without this flag the controller could only
		// ever learn additions, and would keep publishing a path that stopped
		// working the first time it was reported.
		delete(s.obs, viewKey)
		delete(s.pendingObs, viewKey)
	}
	if s.obs == nil {
		s.obs = map[string]observerView{}
	}
	if s.obs[viewKey] == nil {
		s.obs[viewKey] = observerView{}
	}
	view := s.obs[viewKey]

	resp := wire.ObserveResponse{}
	for _, ob := range rep.Observations {
		mapping := net.JoinHostPort(ob.SrcIP, strconv.FormatUint(uint64(ob.SrcPort), 10))
		target, reason, retry := s.attributeLocked(tenant, nodeID, ob)
		if target == 0 {
			resp.Unattributed = append(resp.Unattributed,
				wire.Attribution{Mapping: mapping, DstPort: ob.DstPort, Reason: reason})
			if !retry {
				continue
			}
			if s.pendingObs == nil {
				s.pendingObs = map[string]map[string]*pendingObs{}
			}
			if s.pendingObs[viewKey] == nil {
				s.pendingObs[viewKey] = map[string]*pendingObs{}
			}
			s.pendingObs[viewKey][mapping] = &pendingObs{ob: ob, reason: reason, seenAt: now.Unix()}
			continue
		}
		if prev := view[target]; prev != nil && prev.mapping != mapping {
			log.Printf("observe: %s moved node %d from %s to %s (as seen by %d)",
				tenant, target, prev.mapping, mapping, nodeID)
		}
		v := ob.Verified
		if prev := view[target]; prev != nil && prev.mapping == mapping && prev.verified {
			// Upgrade-only, mirroring the dataplane's own rule: a later row that
			// happened to land on an unverified packet must not erase the fact
			// that this source once carried a validly signed header.
			v = true
		}
		view[target] = &attributedObs{
			mapping:  mapping,
			dstPort:  ob.DstPort,
			verified: v,
			hits:     ob.Hits,
			ageSec:   ob.AgeSec,
			seenAt:   now.Unix(),
		}
		delete(s.pendingObs[viewKey], mapping)
		resp.Accepted++
		resp.Attributed = append(resp.Attributed,
			wire.Attribution{Mapping: mapping, DstPort: ob.DstPort, NodeID: target, Reason: reason})
	}
	// Epoch is READ here. Observations are a per-node dataplane poll and carry no
	// tenant-wide change; burning an epoch on each one would make the counter mean
	// "somebody dumped a table" instead of "the registry view moved".
	resp.Epoch = s.epoch()
	s.mu.Unlock()

	if len(resp.Attributed) > 0 {
		log.Printf("observe: node %d (%s) reported %d row(s), %d attributed",
			nodeID, tenant, len(rep.Observations), resp.Accepted)
	}
	json.NewEncoder(w).Encode(resp)
}

// nodePortLocked answers whether a UDP port belongs to this node's dataplane.
//
// The legacy pair (the VPN port a device declared, the transit port it was
// told to bind) is not enough any more: the underlay layer allocates ONE port
// per tunnel from a pool and pushes each into the device's WG interface, so a
// node owns as many VPN ports as it has tunnels. Reading only the legacy field
// made every observation on a controller-managed tunnel "unattributed" -- which
// is how the lab run of 2026-10-28 found it.
//
// Must be called with s.mu held.
func (s *server) nodePortLocked(tenant string, nodeID uint16, port uint16) bool {
	if port == 0 {
		return false
	}
	if mine := s.state.Nodes[key(tenant, nodeID)]; mine != nil &&
		(port == mine.VpnPort || port == mine.TransitPort) {
		return true
	}
	uc := s.underlayConfigLocked(tenant, false)
	if uc == nil || uc.State == nil {
		return false
	}
	if uc.State.TransitPorts[nodeID] == port {
		return true
	}
	for _, t := range uc.State.Tunnels {
		if (t.A.NodeID == nodeID && t.A.Port == port) || (t.B.NodeID == nodeID && t.B.Port == port) {
			return true
		}
	}
	return false
}

// attributeLocked names the node a reported outer source belongs to.
//
// It is a LOOKUP, not a guess. The only evidence is an address some node declared
// about itself over its authenticated channel, plus the mapping a reflector
// observed for it. No candidate means "nobody claims that address"; more than one
// means "ambiguous" and the controller refuses to pick. The refusal is the point:
// an observed source is NOT authenticated by the SR header (sr_hmac.c covers
// ver_flags through the segments, never the outer IP), so a wrong attribution
// would not fail -- it would quietly hand a peer a path to somebody else's box.
//
// Returns the target node (0 when undetermined), a human-readable reason, and
// whether the row is worth keeping for a later retry.
//
// Must be called with s.mu held.
func (s *server) attributeLocked(tenant string, observer uint16, ob wire.Observation) (uint16, string, bool) {
	if ob.SrcIP == "" || ob.SrcPort == 0 {
		return 0, "malformed source", false
	}
	mine := s.state.Nodes[key(tenant, observer)]
	if mine == nil {
		return 0, "observer is not enrolled", false
	}
	// Role check first. A packet that arrived on a port which is neither of this
	// node's dataplane ports did not come from a tunnel this controller runs --
	// and the destination port is exactly the field that says which role answered,
	// so it gets used rather than logged and ignored.
	if !s.nodePortLocked(tenant, observer, ob.DstPort) {
		return 0, fmt.Sprintf("arrived at port %d, not this node's vpn=%d/transit=%d",
			ob.DstPort, mine.VpnPort, mine.TransitPort), false
	}
	var hits []uint16
	for _, rec := range s.state.Nodes {
		if rec.TenantID != tenant {
			continue
		}
		// A node never attributes a source to ITSELF. This is not pedantry: a
		// docker bridge that rewrites sources makes an observer's own gateway
		// address show up as a "peer", and answering that with "that was me"
		// would tell the node to open a tunnel to its own box.
		if rec.NodeID == observer {
			continue
		}
		if claimsAddr(rec, ob.SrcIP) {
			hits = append(hits, rec.NodeID)
		}
	}
	switch len(hits) {
	case 0:
		return 0, fmt.Sprintf("no node in %s claims %s", tenant, ob.SrcIP), true
	case 1:
		return hits[0], "unique", false
	default:
		return 0, fmt.Sprintf("%d nodes claim %s: refusing to guess", len(hits), ob.SrcIP), true
	}
}

// claimsAddr reports whether `ip` is one of this node's own addresses: either
// declared by the node itself, or the address a reflector saw it send from.
// Both are legitimate and they are not the same evidence -- the first is a claim
// the node made under its certificate, the second is a third party's observation
// of the node. Either is enough to name a source; neither is enough to trust the
// payload, which is what the key binding is for.
func claimsAddr(rec *NodeRecord, ip string) bool {
	for _, a := range rec.LocalAddrs {
		if a == ip {
			return true
		}
	}
	if rec.UDPMapping != "" {
		if host, _, err := net.SplitHostPort(rec.UDPMapping); err == nil && host == ip {
			return true
		}
	}
	return false
}

// reattributeLocked retries the rows that arrived before their owner declared an
// address, and returns how many found one. Must be called with s.mu held.
func (s *server) reattributeLocked(tenant string) int {
	prefix := tenant + "|"
	fixed := 0
	for viewKey, pending := range s.pendingObs {
		if !strings.HasPrefix(viewKey, prefix) {
			continue
		}
		var observer uint16
		if _, err := fmt.Sscanf(viewKey, prefix+"%d", &observer); err != nil {
			continue
		}
		for mapping, p := range pending {
			target, reason, _ := s.attributeLocked(tenant, observer, p.ob)
			if target == 0 {
				p.reason = reason // the reporter asked why; keep the latest answer
				continue
			}
			if s.obs[viewKey] == nil {
				s.obs[viewKey] = observerView{}
			}
			s.obs[viewKey][target] = &attributedObs{
				mapping: mapping, dstPort: p.ob.DstPort, verified: p.ob.Verified,
				hits: p.ob.Hits, ageSec: p.ob.AgeSec, seenAt: p.seenAt,
			}
			delete(pending, mapping)
			fixed++
			log.Printf("observe: late attribution %s -> node %d (as seen by %d)", mapping, target, observer)
		}
		if len(pending) == 0 {
			delete(s.pendingObs, viewKey)
		}
	}
	return fixed
}

// expireObservationsLocked drops rows the observers have stopped re-reporting. An
// observation is a claim about a NAT binding that was true a minute ago; the
// registry must not carry it past that. Must be called with s.mu held.
func (s *server) expireObservationsLocked() {
	cutoff := time.Now().Add(-observationTTL).Unix()
	for viewKey, view := range s.obs {
		for target, o := range view {
			if o.seenAt < cutoff {
				log.Printf("observe: dropping stale row for node %d (seen by %s)", target, viewKey)
				delete(view, target)
			}
		}
		if len(view) == 0 {
			delete(s.obs, viewKey)
		}
	}
	for viewKey, pending := range s.pendingObs {
		for mapping, p := range pending {
			if p.seenAt < cutoff {
				delete(pending, mapping)
			}
		}
		if len(pending) == 0 {
			delete(s.pendingObs, viewKey)
		}
	}
}

// forgetTargetLocked removes every observer's row about one node -- used when the
// node goes stale, because a stale node's last-seen address is the kind of thing
// a peer would happily hole-punch toward and then sit on.
//
// Must be called with s.mu held.
func (s *server) forgetTargetLocked(tenant string, target uint16) {
	prefix := tenant + "|"
	for viewKey, view := range s.obs {
		if strings.HasPrefix(viewKey, prefix) {
			delete(view, target)
		}
	}
	// A pending row has no owner yet, so it cannot be matched by node id; the best
	// available test is "is this source one of the addresses the node that just
	// lapsed declared?", which is exactly what would have attributed it.
	targetRec := s.state.Nodes[key(tenant, target)]
	if targetRec == nil {
		return
	}
	for viewKey, pending := range s.pendingObs {
		if !strings.HasPrefix(viewKey, prefix) {
			continue
		}
		for mapping, _ := range pending {
			if host, _, err := net.SplitHostPort(mapping); err == nil && claimsAddr(targetRec, host) {
				delete(pending, mapping)
			}
		}
	}
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

// normaliseAddrs turns a node's declared address list into a comparable form:
// prefixes are cut (an agent enumerates interfaces and gets "10.10.1.2/24", the
// attribution lookup needs "10.10.1.2"), blanks and duplicates are dropped, and
// the result is sorted so a report that merely reordered the same set is not
// mistaken for a change.
//
// Validation is deliberately light: the list is only ever used as a match table,
// and a malformed entry can then match nothing. Rejecting an entire report
// because one interface line parsed oddly would throw away the entries that
// would have attributed real peers.
func normaliseAddrs(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		if i := strings.IndexByte(a, '/'); i >= 0 {
			a = a[:i]
		}
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalTunnels compares two canonical tunnel tables. Both sides come out of
// wire.NormaliseTunnelEndpoints, so element-wise comparison is enough.
func equalTunnels(a, b []wire.TunnelEndpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const (
	// pKeyLen is SR_PKEY_LEN in vpp-plugin/sr_hmac.h: 128-bit hop-MAC key. The
	// two numbers must agree, and the dataplane is the one that decides.
	pKeyLen = 16
	// firstHmacID is the key identifier every path uses until rotation lands.
	// Starting at 1 rather than 0 keeps "no key selected" distinguishable in the
	// header, which is the same reason the plugin treats a 0 path id carefully.
	firstHmacID = 1

	// observationTTL is how long an attributed observation stays usable. It is a
	// controller-side deadline on TOP of the dataplane's own aging, because the
	// two clocks answer different questions: age_sec says "this node stopped
	// seeing that source at T-40s", seenAt says "and nobody has re-reported it to
	// me since". Comfortably more than a few heartbeat intervals so a normal poll
	// never looks like a disappearance; short enough that a mapping which really
	// died is not advertised for hours.
	observationTTL = 90 * time.Second
)

// ------------------------------------------------------------------ tenant facts

// tenantKeyRing returns the tenant's hop-MAC key ring, issuing it on first use.
//
// The ring is DOMAIN material: the plugin keys the SR header's per-hop MAC with
// a p-key selected by the hmac_id written in the header, and a node has to verify
// headers signed by nodes it never talked to (a transit hop signs, the endpoint
// checks). So the key must be shared tenant-wide -- that is what makes "any node
// can verify any other node's header" true with no pairwise setup, and it is why
// the controller holds it at all.
//
// Exactly one entry today. Adding a second hmac_id is how rotation starts, but
// rotation has real ordering requirements (install before any path cites it, and
// the plugin refuses to drop a key a path still references), so the ring is left
// single-entry rather than pretending.
//
// Must be called with s.mu held.
func (s *server) tenantKeyRing(tenant string) []wire.PKey {
	if r, ok := s.state.PKeyRings[tenant]; ok && len(r) > 0 {
		return r
	}
	buf := make([]byte, pKeyLen)
	if _, err := rand.Read(buf); err != nil {
		// A weak or empty key would silently weaken every MAC in the tenant, so
		// refuse rather than degrade.
		log.Fatalf("tenant key ring: entropy unavailable: %v", err)
	}
	ring := []wire.PKey{{HmacID: firstHmacID, Key: hex.EncodeToString(buf)}}
	s.state.PKeyRings[tenant] = ring
	// Logged without the key itself: this line ends up in container logs, which
	// are not where tenant domain material belongs.
	log.Printf("tenant %s: issued p-key ring (hmac_id=%d, %d bytes)", tenant, firstHmacID, pKeyLen)
	return ring
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

// toPeerInfoLocked renders one registry row AS SEEN BY A READER.
//
// Two of the fields are properties of the reader rather than of the row: IsSelf,
// and the observed address (which is "what your traffic looked like when I
// received it", and under a symmetric NAT every observer gets a different
// answer). So a fan-out to subscribers must build the row per subscriber -- a
// single row cannot carry everyone's vantage.
//
// Must be called with s.mu held.
func (s *server) toPeerInfoLocked(rec *NodeRecord, reader uint16) wire.PeerInfo {
	pi := wire.PeerInfo{
		NodeID:      rec.NodeID,
		Serial:      rec.Serial,
		UDPMapping:  rec.UDPMapping,
		VpnPort:     rec.VpnPort,
		TransitPort: rec.TransitPort,
		Reach:       rec.Reach,
		LastSeen:    rec.LastSeen,
		AgentVer:    rec.AgentVer,
		IsSelf:      rec.NodeID == reader,
		Stale:       rec.Stale,
		SymmetricNA: rec.Symmetric,
		CertPEM:     rec.CertPEM,
		Binding:     rec.Binding,
		KeyGen:      rec.KeyGen,
	}
	if o := s.obs[key(rec.TenantID, reader)][rec.NodeID]; o != nil {
		pi.ObservedMapping = o.mapping
		pi.ObservedVerified = o.verified
		// age_sec came out of the observer's dataplane, measured against the
		// observer's own uptime; the controller's wait since that dump is added so
		// a reader never sees an age that got YOUNGER by sitting in a registry.
		pi.ObservedAgeSec = o.ageSec + uint64(time.Since(time.Unix(o.seenAt, 0)).Seconds())
	}
	// Only the entries this node declared TOWARD the reader travel: those are the
	// addresses the reader may use as next hops for routes learned from it, one
	// per parallel tunnel. An entry toward somebody else is none of the reader's
	// business and would be a wrong next hop if it used it.
	if reader != 0 {
		for _, t := range rec.Tunnels {
			if t.PeerNodeID == reader {
				pi.OverlayIPs = append(pi.OverlayIPs, t.LocalIP)
			}
		}
	}
	return pi
}

func (s *server) peersLocked(tenant string, reader uint16) []wire.PeerInfo {
	out := []wire.PeerInfo{}
	for _, rec := range s.state.Nodes {
		if rec.TenantID != tenant {
			continue
		}
		out = append(out, s.toPeerInfoLocked(rec, reader))
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
// know who is reading. So is the observed address -- which is why the row is
// RE-RENDERED per subscriber instead of copied: the producer's copy carries the
// node id, and everything reader-dependent is filled from the reader's own
// observation view.
func (s *server) broadcastLocked(tenant string, ev wire.WatchEvent) {
	ev.TenantID = tenant
	for _, sub := range s.subs {
		if sub.tenant != tenant {
			continue
		}
		out := ev
		if rec, ok := s.state.Nodes[key(tenant, ev.Peer.NodeID)]; ok {
			out.Peer = s.toPeerInfoLocked(rec, sub.nodeID)
		} else {
			// The row is gone (a removal): there is nothing left to render, but
			// the reader still needs to be told it was about itself.
			out.Peer.IsSelf = (ev.Peer.NodeID == sub.nodeID)
		}
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
					// Its address stops being a fact the moment its agent stops
					// answering: whatever a peer saw 40 seconds ago describes a
					// binding that may already be gone.
					s.forgetTargetLocked(rec.TenantID, rec.NodeID)
					// Telemetry describes the same box. If its heartbeats lapsed,
					// its last window is not a fact any more either.
					delete(s.tel, key(rec.TenantID, rec.NodeID))
					s.broadcastLocked(rec.TenantID, wire.WatchEvent{
						Type: "stale", Peer: s.toPeerInfoLocked(rec, 0),
						SentAt: now, Message: "heartbeat lapsed",
					})
					// Console-facing: the health table just changed without any
					// request arriving. Waiting for the 10s poll would show a dead
					// device as online for up to that long.
					s.admin.publish("node.stale", rec.TenantID, rec)
				}
			}
			s.expireObservationsLocked()
			s.expireTelemetryLocked(now)
			s.expireClaimCodesLocked(now)
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
		hbSec    = flag.Int("hb", 5, "heartbeat interval advertised to agents")
		extraSAN = flag.String("tls-san", "", "comma-separated extra SANs for the server certificate")
		// The console is a SEPARATE, PLAIN-HTTP listener. A browser cannot present
		// a device certificate, so it cannot use the mTLS listener without making
		// every operator install one; the console therefore has its own token
		// session and is meant to be published on loopback only.
		consoleAddr = flag.String("console", "127.0.0.1:8088", "console listen address (plain HTTP; empty disables the console)")
		adminTokenF = flag.String("admin-token", "", "console login token; empty reads $SR_CTRL_ADMIN_TOKEN, else generates one and prints it once")
		// How long a telemetry window stays readable after its last report. It is
		// a read-side policy, not a dataplane setting: the probe cadence comes from
		// TunnelTelemetry, while this number only decides when the controller stops
		// showing the last good window -- the wrong answer here is a dead tunnel
		// that still looks healthy.
		metricsTTL = flag.Int("metrics-ttl", 30, "seconds a telemetry window stays readable after its last report")
	)

	flag.Parse()

	if *metricsTTL < 1 {
		log.Fatalf("--metrics-ttl must be >= 1 (got %d); zero would expire every window before it could be read", *metricsTTL)
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
		caCert:     ca,
		caKey:      caKey,
		dataDir:    *dataDir,
		obs:        map[string]observerView{},
		pendingObs: map[string]map[string]*pendingObs{},
		tel:        map[string]*nodeTelemetry{},
		telTTLSec:  *metricsTTL,
		adminToken: *adminTokenF,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.ttlSweeper(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enroll", s.handleEnroll)
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/config/bundle", s.handleConfigBundle)
	mux.HandleFunc("/v1/underlay/bundle", s.handleUnderlayBundle)
	mux.HandleFunc("/v1/mapping", s.handleMapping)
	mux.HandleFunc("/v1/observe", s.handleObserve)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
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

	var adminSrv *http.Server
	if *consoleAddr != "" {
		if s.adminToken == "" {
			s.adminToken = os.Getenv("SR_CTRL_ADMIN_TOKEN")
		}
		generated := false
		if s.adminToken == "" {
			s.adminToken = randToken()
			generated = true
		}
		s.admin = newAdminHub()
		adminSrv = &http.Server{Addr: *consoleAddr, Handler: s.adminHandler()}
		go func() {
			log.Printf("console listening on http://%s/", *consoleAddr)
			if generated {
				log.Printf("console login token (generated, shown once): %s", s.adminToken)
			}
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("console listener stopped: %v", err)
			}
		}()
	} else {
		// Publishing to a hub with no subscribers keeps s.admin.publish safe on
		// the enrollment/heartbeat paths without a nil check at every call site.
		s.admin = newAdminHub()
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
		if adminSrv != nil {
			adminSrv.Close()
		}
	}()

	log.Printf("controller listening on %s (data=%s, heartbeat=%ds)", *listen, *dataDir, *hbSec)
	if err := hs.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
