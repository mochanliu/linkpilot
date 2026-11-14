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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// SymmetricKnown is stored, not just checked on the way in: without it, a
	// node that was never measured and a node measured as non-symmetric both
	// read as Symmetric=false, and the second one is the only one a tunnel can
	// be planned from. The console and the planner both have to tell them apart.
	SymmetricKnown bool   `json:"symmetric_known,omitempty"`
	Reflector      string `json:"reflector"`
	// ProbeLocal is the source address the device's own probe socket used on the
	// way to Reflector (see wire.MappingReport.ProbeLocal). It is stored apart
	// from LocalAddrs because it is a measurement, not a declaration: LocalAddrs
	// says which addresses this box owns, this says which of them the outer path
	// actually went out through. That is the only pairing that survives a NAT --
	// the observed public address belongs to nobody's interface table.
	ProbeLocal string `json:"probe_local,omitempty"`
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

	// Bindings is the device's keyring: one attested X25519 key per WG key slot,
	// stored at its slot index (verified before storage -- see applyBindings),
	// and per the wire contract each key IS that tunnel's WireGuard static public
	// key. The controller stores and relays them; it can check them but can never
	// produce one, because it does not hold the device's identity key. A nil entry
	// means that slot has never been reported, and a tunnel whose slot is empty
	// cannot be delivered.
	//
	// Old state.json files carry a single key_binding whose signature was made
	// over the v1 message (no slot byte). It cannot verify under v2, so the
	// migration drops it instead of relaying an attestation the relying party
	// would reject: the agent re-reports the whole keyring within one heartbeat.
	Bindings []*keybinding.Binding `json:"key_bindings,omitempty"`

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

	// Overlays is each tenant's route layer, keyed by tenant id: the 导通组 whose
	// members the controller assigned RTs to, and the tenant-level escape switch.
	// Separate from Underlays because a group is not a point-to-point thing -- it
	// names a set of (device, VPN) pairs and one RT shared by all of them, and
	// nothing in it references a tunnel or a port.
	Overlays map[string]*OverlayConfig `json:"overlays"`

	// LeakConfigs is each tenant's 路由泄露 prefix-list library, keyed by tenant
	// id. It is per tenant on purpose (2026-11-02 拍板): nothing here is shared
	// across tenants, and the operator view does not show it. The rules that
	// reference a list live on DeviceConfig.Leak, not here.
	LeakConfigs map[string]*LeakConfig `json:"leak_configs"`

	// RRs is the route-reflector table. One document rather than a per-tenant map,
	// because the id counter has to be minted in one place and never rewound; but
	// every row is bound to exactly one tenant (2026-11-01 定案), that tenant is the
	// row's only editor, and the controller renders its own rows into that tenant's
	// devices' route.json.
	RRs *RRLibrary `json:"rrs,omitempty"`

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

	// Alerts is each tenant's alert settings (webhook, thresholds), keyed by
	// tenant id. Configuration, not state: it survives a restart because a
	// webhook that silently vanished after an upgrade is worse than no webhook.
	Alerts map[string]*AlertSettings `json:"alert_settings,omitempty"`

	// LedgerRetentionDays is how many days of 运维台账 to keep. One number for
	// the whole controller, not per tenant: the ledger is a directory of day
	// files that every tenant's rows share, so "tenant A keeps 7 days" would
	// physically delete tenant B's history. 0 means "never configured" and
	// falls back to opsDefaultRetentionDays.
	LedgerRetentionDays int `json:"ledger_retention_days,omitempty"`

	// Cluster is the controller cluster's membership table (cluster.go). nil =
	// 单机控制器，没有共识、没有投票权；一旦在控制台"开启集群"就有了。
	//
	// 它进共识但不进"行复制"（hardstate.go 里显式跳过）：成员变更由配置项
	// 唯一拥有，否则一次发布和一次加节点会互相覆盖。
	Cluster *ClusterConfig `json:"cluster,omitempty"`
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
		Nodes:       map[string]*NodeRecord{},
		NextNodeID:  map[string]uint16{},
		Tenants:     map[string]*Tenant{},
		Claims:      claimMap{},
		PKeyRings:   map[string][]wire.PKey{},
		Accounts:    map[string]*Account{},
		Configs:     map[string]*DeviceConfig{},
		Underlays:   map[string]*UnderlayConfig{},
		Overlays:    map[string]*OverlayConfig{},
		LeakConfigs: map[string]*LeakConfig{},
		Sites:       map[string]*Site{},
		PreAuths:    map[string]*PreAuth{},
		Alerts:      map[string]*AlertSettings{},

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
	if s.Overlays == nil {
		s.Overlays = map[string]*OverlayConfig{}
	}
	// A record from before the counters existed would allocate 65000:0 (an RT two
	// groups could share), so each one is put back on the allocator's floor.
	for _, oc := range s.Overlays {
		if oc != nil {
			oc.ensure()
		}
	}
	if s.LeakConfigs == nil {
		s.LeakConfigs = map[string]*LeakConfig{}
	}
	// Every tenant's library is repaired at load: a missing counter would mint a
	// duplicate pl id, and an empty library would leave CPE-SDWAN with nothing to
	// select. Leak rows on the drafts are normalized for the same reason -- an
	// older file has none, and a hand-edited one may carry duplicates.
	for _, lc := range s.LeakConfigs {
		if lc != nil {
			lc.ensure()
		}
	}
	for _, dc := range s.Configs {
		if dc != nil {
			normalizeLeakSet(&dc.Leak)
		}
	}
	for _, dc := range s.PreAuthConfigs {
		if dc != nil {
			normalizeLeakSet(&dc.Leak)
		}
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
	// 告警设置按租户一张表：老 state 文件没有这块，读侧 alertSettingsLocked
	// 对缺省补默认值，这里只兜 nil map。
	if s.Alerts == nil {
		s.Alerts = map[string]*AlertSettings{}
	}
	if normalizeSites(s) {
		migrated = true
	}
	if normalizePreAuths(s) {
		migrated = true
	}
	if normalizeRRs(s) {
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
	// consoleTLS 是"控制台监听器讲不讲 TLS"。集群里它要写进成员表：follower
	// 307 时必须知道 leader 的控制台是 https 还是明文，猜错就是把人送到一个
	// 只讲 TLS 的端口上。
	consoleTLS bool
	// reflectors are the STUN endpoints advertised to agents (see serveStun). Set
	// once at boot before any listener starts and never mutated, so handlers read
	// it without the mutex. Empty = this controller runs no reflector service.
	reflectors []string

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

	// rrFetch 是每台 RR 最后一次来取配置的时间与来源地址，**只在本机内存里**。
	// 它是读数不是配置：RR 每 5 秒来一次，如果把它写进硬状态，集群就会每 5 秒
	// 为一个"谁刚来过"提交一轮共识、日志因此持续增长（2026-11-14 lab 实测：
	// 空闲集群 commit 每秒 +1，全部是 RRs 这一行）。和 LastSeen 一个道理。
	rrFetch map[string]rrFetchInfo

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

	// ops is the append-only 运维台账 (opslog.go): history samples, events,
	// alerts and audits all land here. nil in tests -- every entry point is
	// nil-safe and skips.
	ops *opsLedger

	// alerts holds the ACTIVE alert records, keyed by alertKey. In-memory on
	// purpose: the rules re-derive them within one sweep after a restart, and
	// the raise/clear transitions are persisted in the ledger.
	alerts map[string]*alertRecord

	// alertStrikes counts consecutive over-threshold windows per tunnel
	// (loss alert debounce), and lastSampleAt/lastSelection throttle the
	// sample ledger and dedupe selection-change entries.
	alertStrikes  map[string]int
	lastSampleAt  map[string]int64
	lastSelection map[string]uint8

	// opsPruneDay is the last day the ledger prune ran (local date), and
	// opsPruneDays the retention it ran with. Together they let the 2s sweeper
	// do directory work once per day instead of once per tick -- and again the
	// moment someone changes the retention, so lowering it from 30 to 3 days in
	// the console takes effect now rather than at the next midnight.
	opsPruneDay  string
	opsPruneDays int

	// opsQ 是台账行的出站队列（P3：台账进共识）。leader 把 appendOps 产出的行
	// 推进来，opsShipper 攒批提议进 raft，提交后由 apply 路径写回本机文件 ——
	// 于是三台机器的台账文件逐行一致，换主后历史跟着多数派走。队列满就退回
	// 本地落盘：台账宁可"这台机器有一份别人没有的行"，也不能把写路径堵死。
	opsQ chan opsEntry

	// liveSnap 是设备活跃度表的**只读快照**（leader 搭车带出去的那份），由
	// ttlSweeper 每 2s 在持锁状态下重建。它必须是无锁读的：sendAppend 在复制
	// 热路径上取它，而写路径的 commit() 持 s.mu 等多数派 —— 热路径再去抢 s.mu
	// 就是 P1 立过的那条规矩的反面（复制等提交、提交等复制，实测把 append 的
	// 500ms ctx 全部烧完，集群直接进入选举风暴）。
	liveSnap atomic.Pointer[map[string]int64]
	// liveCh 承接对端搭车带来的活跃度表：handleAppend 的应答路径只做一次
	// 非阻塞投递，真正的合并（要拿 s.mu）在 liveApplier 协程里做 —— 应答
	// 延迟一毫秒都可能让 leader 的复制超时，所以这里宁可丢弃（下一拍还有
	// 新的表），绝不等待。
	liveCh chan map[string]int64

	// diagRecs holds the remote-diagnosis records (diag.go), keyed by node key
	// like obs/tel: a pending command that outlives a controller restart is
	// dropped on purpose (nobody is watching the console for it anymore), and
	// the durable trail lives in the ledger.
	diagRecs map[string][]*diagRecord
	// diagSeq mints diag ids. Only touched under s.mu.
	diagSeq int

	// ---- 集群（cluster.go / cluster_runtime.go / raft.go）
	//
	// raft 是共识引擎；nil = 单机模式，commit() 退回直接写 state.json。
	// 两种模式共用同一条提交路径，所以写处理器不需要知道自己跑在哪种模式下。
	raft     *raftNode
	raftDisk *raftDisk
	// hardIndex 是"已提交"的硬状态指纹表，commit() 拿它算 diff。
	hardIndex map[string]string
	// commitErr 是最近一次提交失败的原因；非空时控制台挂横幅、写请求 500。
	commitErr    string
	commitErrSeq uint64
	// clusterOps 串行化成员变更：两个控制台同时点"添加"会各自算一份成员表，
	// 后者会把前者覆盖掉（成员表是整表替换）。
	clusterOps sync.Mutex
	sweepKick  chan struct{}
	secret     string // 节点间共享密钥，也用来签加入码
	nodeID     string // 本机成员 id（cluster/node-id）
	caPool     *x509.CertPool
	// clusterCli 是节点间用的 HTTP 客户端。用原子指针：加入集群要换成集群 CA，
	// 而它每次复制都要取一次 —— 挂在 s.mu 上就会和"持锁等提交"互相卡死。
	clusterCli atomic.Pointer[http.Client]
	// cert 是当前服务端证书。用原子指针而不是启动时的固定证书：加入集群后
	// 要用集群 CA 重签一张覆盖本机地址的证书，监听器必须立刻换过去。
	cert atomic.Pointer[tls.Certificate]
	// peerAddr 是"成员 id → 地址"的一份只读快照，节点间复制每次都要查。
	// 它不能走 s.mu：写路径持着 s.mu 等多数派确认，而确认本身要发 append，
	// 在 s.mu 上取地址就是自己把自己锁死（现象：leader 一边等提交、一边看着
	// follower 因为收不到心跳而改选，两边来回换主）。
	peerAddr atomic.Pointer[peerAddrs]
	// retired 表示本机已被移出成员表（下线 / 被替换）。之后设备侧一律 503 ——
	// 一台"已经被开除、还在给设备回心跳"的控制器是最难发现的那种故障：
	// 设备以为自己好好的，运维在 leader 上也看不出异常。
	retired     atomic.Bool
	retiredNote string
	retireExit  bool
	// ctx 是进程的取消信号，共识与 sweep 都挂在它上面。
	ctx context.Context
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

// save 是"硬状态改了"的唯一出口（历史上叫 save，语义从"整份落盘"变成
// "把这几行提交给多数派"）。名字保留是为了让 60 个调用点一行不改 —— 它们
// 写的确实都是硬状态。
//
// 失败不在这里退出：控制台写请求由 commitGuard 改成 500，设备侧的关键路径
// （入网）自己检查返回值。进程退出留给 raft 里的"落盘失败"——那是另一回事。
func (s *server) save() {
	if err := s.commit(); err != nil {
		// commit() 已经记下 commitErr 并打了日志，这里只需要让控制台红起来。
		s.admin.publish("cluster.commit_failed", "", nil)
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
		// 证书在而私钥不在是新流程的常态（见 reloadCA：CA 私钥由 leader 经
		// 集群内部通道补送，加入方先有证书、后有私钥）。只装证书继续跑，
		// 签发动作等 key 到位。
		return loadClusterCA(dir)
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

// loadClusterCA 只读集群 CA：证书必须存在，私钥可以还没有（nil）。它不生成
// 任何东西 —— 生成自己的 CA 是 loadOrCreateCA（单机/首节点）的事。
func loadClusterCA(dir string) (*x509.Certificate, ed25519.PrivateKey, *x509.CertPool, error) {
	cb, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	if cblk == nil {
		return nil, nil, nil, fmt.Errorf("ca.pem 不是有效的 PEM")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		// 私钥还没送到：这台机器先以"只信、不签"的身份工作。
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		return cert, nil, pool, nil
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

// signRR issues a route reflector's certificate. It is signDevice's sibling with
// three differences that all come from what an RR is:
//
//   - the identity URI names the rr role, so keybinding.RefOf reads it as an RR
//     and a node certificate can never be mistaken for one (or vice versa);
//   - the EKU carries serverAuth as well as clientAuth: an RR both dials (the
//     controller, its peers) and is dialled (by its tenant's devices);
//   - the address host goes into the SAN, because devices and peer RRs use the
//     row's addr host as their TLS ServerName. The port is not part of a name.
func signRR(ca *x509.Certificate, caKey ed25519.PrivateKey, pub ed25519.PublicKey, tenant, rrID, host string) (string, error) {
	u, err := url.Parse(fmt.Sprintf("srou://%s/rr/%s", tenant, rrID))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s-rr-%s", tenant, rrID)},
		NotBefore:    time.Now().Add(-time.Hour),
		// Longer than a device's year: an RR is operator infrastructure, and a
		// lapsed certificate there is an outage that needs a person at the box.
		NotAfter:    time.Now().AddDate(2, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:        []*url.URL{u},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
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
	//
	// 入网是写：它会分配 node_id、签证书，这些都必须落在多数派上。落到
	// follower 上等于两台机器各发一个 node_id —— 所以先转给 leader。307
	// 会把 body 一起重放，设备端不需要改。
	if s.redirectToLeader(w, r) {
		return
	}
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
	// 入网这一步必须落盘/落多数派：设备拿着证书就再也回不来了，而 node_id
	// 是数据面的身份。提交失败就回 500，让设备重试 —— 重投一次比发一个
	// "在控制台看得见、切换后消失"的设备便宜得多。
	if err := s.commit(); err != nil {
		http.Error(w, "identity could not be committed: "+err.Error(), 500)
		return
	}
	s.admin.publish("node.enrolled", tenant, rec)
	s.appendOps("node.enrolled", tenant, rec.NodeID, "", "info",
		fmt.Sprintf("设备入网：node %d，serial %s，授权 %s", rec.NodeID, req.Serial, claim.ID))
	if transitAssigned {
		// The 全局配置 page lists every device's transit port; it just changed.
		s.admin.publish("node.underlay", tenant, rec)
	}

	resp := s.enrollResponse(tenant, rec, certPEM, string(caPEM))
	// The agent asked how many reflectors it can probe; hand it at most that
	// many. An agent built without a probe socket sends 0 and gets an empty list,
	// so it never reports "address discovery failed" for a service it has no
	// client for.
	resp.Reflectors = s.advertiseStun(req.UDPRelfectorN)
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
	keyChanged, err := s.applyBindings(rec, req.Bindings)
	if err != nil {
		// NON-FATAL on purpose: the node is still a valid rendezvous endpoint,
		// and dropping it would turn "your key material was rejected" into
		// "you vanished". The agent gets a named reason and surfaces it. A slot
		// that fails verification leaves the other slots stored; a report that
		// fails as a whole (wrong length, unusable device key) changes nothing.
		bindingErr = err.Error()
		log.Printf("binding rejected tenant=%s node=%d: %v", tenant, nodeID, err)
	}
	// The device's account of what it runs. Stored as reported: the controller
	// does not infer "applied" from "published", and an absent Config (an older
	// agent) must not be turned into capable=false here -- that would accuse a
	// device of ignoring a channel it does not implement.
	configChanged := false
	// DeepEqual rather than !=: Notes made ConfigState a non-comparable struct.
	if req.Config != nil && (rec.Config == nil || !reflect.DeepEqual(*rec.Config, *req.Config)) {
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
	// 心跳不落盘、不进共识（P0 的核心改动）。它改的全是软状态：LastSeen、
	// 设备自报的配置、隧道读数、接口清单 —— 重启后 5 秒内设备自己会再报一遍。
	// 唯一可能变硬状态的是"补分配中转端口"，那一次必须提交，否则两台控制器
	// 会给同一台设备发同一个端口。
	if transitAssigned || configChanged {
		s.saveIfLeader()
	} else {
		s.saveSoft()
	}

	// Coming back from stale, or offering key material peers have not seen, are
	// both events peers need: their view of this node just changed.
	if wasStale || keyChanged {
		s.broadcastLocked(tenant, wire.WatchEvent{Type: "update", Peer: s.toPeerInfoLocked(rec, 0), SentAt: time.Now().Unix()})
	}
	if wasStale || firstContact {
		// Console-facing: the device table's online/stale column just flipped.
		s.admin.publish("node.online", rec.TenantID, rec)
		// 台账：上线/恢复是排障时最常查的事件；失联告警在这里画上句号。
		s.appendOps("node.online", rec.TenantID, rec.NodeID, "", "info",
			fmt.Sprintf("设备上线（node %d，serial %s）", rec.NodeID, rec.Serial))
		if wasStale {
			s.clearAlertLocked("node.stale", rec.TenantID, rec.NodeID, "")
		}
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
	// 远程诊断：盒子自报在忙就不派发，命令在队列里等下一拍。
	var diagCmd *wire.DiagCommand
	if !req.DiagBusy {
		diagCmd = s.diagPopLocked(tenant, nodeID)
	}
	json.NewEncoder(w).Encode(wire.HeartbeatResponse{
		Epoch:         s.advanceEpoch(),
		PeerCount:     len(s.peersLocked(tenant, nodeID)),
		BindingErr:    bindingErr,
		ConfigVersion: s.desiredConfigVersionLocked(tenant, nodeID),
		TunnelVersion: s.underlayVersionLocked(tenant),
		// 集群地址表 + leader 提示。设备不需要 VIP：心跳应答里就有完整列表，
		// agent 自己轮换（P2）。leader 单独给一个，是因为"写"只在 leader 上
		// 成立 —— 设备连到 follower 也能活，但入网、补端口这类硬变更会被
		// 307 到 leader，多一次往返。
		TLSAddrs: s.memberEndpointsLocked(),
		Leader:   s.leaderEndpointLocked(),
		// Every beat, not just enrollment: a restarted agent already has an
		// identity and so never asks again, and a controller that started its
		// reflector after this device enrolled must still reach it. The heartbeat
		// carries no reflector offer, so the full list goes out and the agent
		// applies its own cap.
		Reflectors: s.advertiseStun(-1),
		// 远程诊断命令搭心跳下行。盒子自报在忙时（DiagBusy）不派发，命令留
		// 在队列里等下一拍；派发即标记 dispatched，盒子收下没执行就是台账里
		// 一条"无结果"，控制台重发即可。
		Diag: diagCmd,
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
	rec.ProbeLocal = rep.ProbeLocal
	// A verdict is only stored when the reporter says it measured one. An agent
	// that has not run the two-reflector comparison sends the zero value, and
	// recording that as "not symmetric" would let the planner build a tunnel
	// that cannot work.
	rec.SymmetricKnown = rep.SymmetricKnown
	if rep.SymmetricKnown {
		rec.Symmetric = rep.SymmetricNAT
	} else {
		rec.Symmetric = false
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
	keyChanged, err := s.applyBindings(rec, rep.Bindings)
	if err != nil {
		bindingErr = err.Error()
		log.Printf("binding rejected tenant=%s node=%d: %v", tenant, nodeID, err)
	}
	// Not an else-branch: applyBindings stores per slot, so a report with one
	// bad attestation can still have rotated another. Peers must install the new
	// WireGuard key BEFORE they try to talk; discovering it by packet loss would
	// make a rotation look like a link failure.
	if keyChanged {
		changed = true
		log.Printf("pushing new key bindings for %s/%d to peers", tenant, nodeID)
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

// applyBindings authenticates and stores a node's whole X25519 keyring.
//
// The controller verifies too, even though the relying party verifies again.
// Two different jobs: this one stops the controller from RELAYING garbage (and
// gives the operator an immediate, named error), while the receiver's check is
// what actually protects the payload key -- because it must hold even if this
// binary is lying. A failed check here is non-fatal to registration (the node's
// mapping is still useful for rendezvous) but is reported back to the agent so
// it can surface it.
//
// Per slot, not all-or-nothing. One bad attestation must not cost the device
// the other seven: a tunnel whose slot is stored refuses to deliver with a
// named reason, which is strictly better than every tunnel on the box going
// dark because one key rotated wrong.
//
// Returns whether any stored binding actually changed, so the caller knows when
// peers need to be told. A silent key change would leave every peer holding an
// O-key that no longer matches, and the only symptom would be "packets arrive
// but do not decrypt".
//
// Must be called with s.mu held.
func (s *server) applyBindings(rec *NodeRecord, bs []keybinding.Binding) (changed bool, err error) {
	if len(bs) == 0 {
		return false, nil
	}
	// A keyring that is not the full eight is a device reporting a stale or
	// partial set. Accepting it would silently retire slots the tunnels in the
	// registry still point at.
	if len(bs) != keybinding.WGKeySlots {
		return false, fmt.Errorf("keyring has %d entries, a device reports %d",
			len(bs), keybinding.WGKeySlots)
	}
	pub, err := base64.StdEncoding.DecodeString(rec.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("stored device key is unusable")
	}
	id := ed25519.PublicKey(pub)
	if rec.Bindings == nil {
		rec.Bindings = make([]*keybinding.Binding, keybinding.WGKeySlots)
	}
	var firstErr error
	for i := range bs {
		b := bs[i]
		if int(b.Slot) != i {
			if firstErr == nil {
				firstErr = fmt.Errorf("slot %d arrived at position %d in the keyring", b.Slot, i)
			}
			continue
		}
		if _, err := b.Verify(rec.TenantID, rec.NodeID, id); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("slot %d: %w", b.Slot, err)
			}
			continue
		}
		cur := rec.Bindings[i]
		// Going BACKWARDS in key generation is a replay of an old binding, and is
		// refused. Going forward is a rotation, which is legitimate and must be
		// announced.
		if cur != nil && b.KeyGen < cur.KeyGen {
			if firstErr == nil {
				firstErr = fmt.Errorf("slot %d: key_gen went backwards (%d -> %d): stale binding replay",
					i, cur.KeyGen, b.KeyGen)
			}
			continue
		}
		if cur != nil && cur.X25519Pub == b.X25519Pub && cur.KeyGen == b.KeyGen {
			continue // unchanged; the common case on every heartbeat
		}
		rotated := cur != nil
		cp := b
		rec.Bindings[i] = &cp
		if rotated {
			log.Printf("node %s/%d: x25519 key ROTATED in slot %d to generation %d",
				rec.TenantID, rec.NodeID, i, b.KeyGen)
		} else {
			log.Printf("node %s/%d: x25519 binding accepted in slot %d (gen %d)",
				rec.TenantID, rec.NodeID, i, b.KeyGen)
		}
		changed = true
	}
	return changed, firstErr
}

// storedBindings turns the record's per-slot pointers into the list a reader
// gets. A slot that was never reported is absent rather than a zero-value
// binding: an empty attestation in the list would claim "a key covered by
// nothing", which is a different statement from "I have not heard it yet".
func storedBindings(list []*keybinding.Binding) []keybinding.Binding {
	out := make([]keybinding.Binding, 0, len(list))
	for _, b := range list {
		if b == nil {
			continue
		}
		out = append(out, *b)
	}
	return out
}

// bindingAt picks the attestation for one key slot out of the stored keyring.
// nil means that slot has never been reported -- which is not the same thing as
// a slot reported with key_gen 0.
func bindingAt(list []*keybinding.Binding, slot uint8) *keybinding.Binding {
	if int(slot) >= len(list) {
		return nil
	}
	return list[slot]
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
		Bindings:    storedBindings(rec.Bindings),
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
			s.mu.Lock()
			// 活跃度快照每拍重建（无锁读的那份，见 liveSnap）。
			s.rebuildLivenessLocked()
			// 失联判定与告警只在 leader 上做（单一写者，Vault HA 的 standby
			// 同样不评估规则）。follower 的设备活跃度从复制热路径搭车过来
			// （appendReq.Live），控制台的显示按 last_seen 年龄推导
			// （nodeStatusForView）；但 rec.Stale、台账的 node.stale 行、
			// forgetTarget / 清遥测这些动作都只属于 leader。 follower 跑
			// 这一段的后果在 lab 里见过：last_seen 全空 → 全员"失联"。
			if s.raft == nil || s.raft.IsLeader() {
				ttl := int64(s.staleTTL())
				now := time.Now().Unix()
				for _, rec := range s.state.Nodes {
					if rec.LastSeen == 0 || rec.Stale {
						continue
					}
					if now-rec.LastSeen > ttl {
						rec.Stale = true
						log.Printf("ttl: node %d (tenant %s) stale after %ds idle",
							rec.NodeID, rec.TenantID, now-rec.LastSeen)
						s.appendOps("node.stale", rec.TenantID, rec.NodeID, "", "warn",
							fmt.Sprintf("设备失联：心跳 %d 秒没有上报", now-rec.LastSeen))
						s.raiseAlertLocked("node.stale", rec.TenantID, rec.NodeID, "", "crit",
							fmt.Sprintf("设备失联：心跳超过 %d 秒没有上报", ttl))
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
				s.sweepTelemetryGapLocked(now)
				s.pruneLedgerIfDueLocked(now)
				// saveIfLeader 而不是 save：这一轮的结论（谁失联、哪条遥测过期）本来
				// 就是 leader 定的。follower 提交只会得到"这台不是 leader"，每 2 秒
				// 往日志里写一行（lab 实测：健康集群里每台 follower 都在刷这句）。
				s.saveIfLeader()
			} else {
				// follower 也要清自己的台账旧文件（文件是每台各自一份的）。
				s.pruneLedgerIfDueLocked(time.Now().Unix())
			}
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
		// The console is a SEPARATE listener. A browser cannot present a device
		// certificate, so it cannot use the mTLS listener without making every
		// operator install one; the console therefore has its own token session.
		// It stays on loopback by default; -console-tls is what makes it safe to
		// publish further out (see console_tls.go for why the cert is self-signed).
		consoleAddr = flag.String("console", "127.0.0.1:8088", "console listen address (plain HTTP unless -console-tls; empty disables the console)")
		consoleTLS  = flag.Bool("console-tls", false, "serve the console over HTTPS (self-signed cert under <data>/console-tls unless -console-cert/-console-key)")
		consoleCert = flag.String("console-cert", "", "console TLS certificate (PEM); pair with -console-key")
		consoleKey  = flag.String("console-key", "", "console TLS private key (PEM); pair with -console-cert")
		consoleSAN  = flag.String("console-san", "", "extra console certificate SANs, comma separated (public IP or domain)")
		// A second, plain-HTTP listener for local tooling. It shares the handler
		// and the session store with the TLS one, so scripts that speak
		// http://127.0.0.1:8088 keep working while the outside world gets HTTPS.
		consoleHTTP = flag.String("console-http", "", "additional plain-HTTP console listener for loopback tooling (e.g. 127.0.0.1:8088)")
		adminTokenF = flag.String("admin-token", "", "console login token; empty reads $SR_CTRL_ADMIN_TOKEN, else generates one and prints it once")
		// How long a telemetry window stays readable after its last report. It is
		// a read-side policy, not a dataplane setting: the probe cadence comes from
		// TunnelTelemetry, while this number only decides when the controller stops
		// showing the last good window -- the wrong answer here is a dead tunnel
		// that still looks healthy.
		metricsTTL = flag.Int("metrics-ttl", 30, "seconds a telemetry window stays readable after its last report")
		// The built-in STUN reflector, for devices that must learn their own outer
		// address to be reachable at all. Off unless named, and each entry is
		// bound AND advertised verbatim -- see parseStunAddrs for why the host
		// part can never be left out.
		stunAddrs = flag.String("stun", "", "comma-separated ip:port UDP addresses to answer STUN on (empty disables address discovery; two entries are what makes the symmetric-NAT verdict measurable)")
		// 集群：初始的加入只发生在启动时（一条命令），之后的加/替换/下线都在
		// 控制台完成，不需要再碰命令行。
		joinURL  = flag.String("join", "", "join an existing controller cluster: https://leader:8443（配合 -join-token）")
		joinTok  = flag.String("join-token", "", "控制台签发的加入码（一次性，30 分钟有效）")
		nodeName = flag.String("node-name", "", "本控制器在集群里的名字（默认用主机名）")
		// 被下线之后要不要自己退出。默认退出：一台已经不在成员表里的控制器
		// 继续跑，是最难发现的那种故障（设备以为自己好好的、leader 上也看不出
		// 异常）。要留在现场看日志就 -retire-exit=false —— 它会照样停共识、
		// 照样对设备返回 503，只是进程不退。
		retireExitF = flag.Bool("retire-exit", true, "被移出集群后自动退出进程（false: 只停共识并拒绝设备请求）")
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

	ctx, cancel := context.WithCancel(context.Background())

	// The reflector binds before the listeners open, so a port already in use is
	// a boot failure with the port named -- not a device population that silently
	// grows without outer addresses.
	var reflectors []string
	if strings.TrimSpace(*stunAddrs) != "" {
		binds, err := parseStunAddrs(*stunAddrs)
		if err != nil {
			log.Fatal(err)
		}
		refs, err := serveStun(ctx, binds, log.Printf)
		if err != nil {
			log.Fatal(err)
		}
		reflectors = refs
	}

	ops, err := openOpsLedger(filepath.Join(*dataDir, "opslog"))
	if err != nil {
		log.Fatalf("opslog: %v", err)
	}

	s := &server{
		state:         st,
		store:         store,
		subs:          map[int]*subscriber{},
		hbSec:         *hbSec,
		tlsAddrs:      []string{*listen},
		consoleTLS:    *consoleTLS,
		reflectors:    reflectors,
		caCert:        ca,
		caKey:         caKey,
		caPool:        caPool,
		dataDir:       *dataDir,
		obs:           map[string]observerView{},
		rrFetch:       map[string]rrFetchInfo{},
		pendingObs:    map[string]map[string]*pendingObs{},
		tel:           map[string]*nodeTelemetry{},
		telTTLSec:     *metricsTTL,
		adminToken:    *adminTokenF,
		retireExit:    *retireExitF,
		ops:           ops,
		opsQ:          make(chan opsEntry, 1024),
		liveCh:        make(chan map[string]int64, 16),
		alerts:        map[string]*alertRecord{},
		alertStrikes:  map[string]int{},
		lastSampleAt:  map[string]int64{},
		lastSelection: map[string]uint8{},
		diagRecs:      map[string][]*diagRecord{},
		sweepKick:     make(chan struct{}, 1),
		ctx:           ctx,
	}
	// 服务端证书用原子指针挂上去：加入集群之后要用集群 CA 重签一张覆盖本机
	// 地址的证书，监听器必须立刻换过去，否则对端校验会一直失败到进程重启。
	s.cert.Store(&srvCert)

	// ---- 集群：要么加入别人，要么把已有的成员表捡起来继续跑。
	//
	// "已经在集群里"的节点，即使命令行还带着 -join 也不能再加入一次。部署时把
	// 那条加入命令顺手写进 systemd 单元是很常见的写法，机器一重启就会拿着一个
	// 已经用过的加入码重放：加入码一次性、30 分钟过期，重放必然被 leader 拒，
	// 而这台机器本来只需要接着跑 —— 结果是自己把自己弄得起不来。
	// 所以这里只警告并丢掉落参数，让它走"捡起已有成员表"那条路。
	if st.Cluster != nil && (*joinURL != "" || *joinTok != "") {
		log.Printf("cluster: 本机已经在集群 %q 里，忽略 -join/-join-token（加入码一次性，"+
			"别写进 systemd 单元）；要重新加入请先清空数据目录 %s", st.Cluster.ID, *dataDir)
		*joinURL, *joinTok = "", ""
	}
	if *joinURL != "" || *joinTok != "" {
		if *joinURL == "" || *joinTok == "" {
			log.Fatal("加入集群需要同时给 -join（leader 地址）和 -join-token（控制台签发的加入码）")
		}
		if err := s.joinCluster(*joinURL, *joinTok, ctrlVersion); err != nil {
			log.Fatalf("cluster join: %v", err)
		}
		if err := s.reloadCA(); err != nil {
			log.Fatalf("cluster join: 载入集群 CA 失败: %v", err)
		}
		ca, caKey, caPool = s.caCert, s.caKey, s.caPool
	} else {
		s.loadLocalClusterIdentity(*nodeName)
		if st.Cluster != nil {
			// 已经被下线（或被人替换掉）的那台不许再起来：它没有票、没有日志、
			// 没有任何一台会来找它，唯一的作用是继续接设备心跳 —— 一台
			// "看起来正常"的幽灵控制器。要它回来，清空数据目录换新加入码。
			if m := st.Cluster.Members[s.nodeID]; m == nil || m.Status == memberDecommissioned {
				log.Printf("cluster: 本机（%s）在集群 %q 里已被移出成员表，不再启动。"+
					"重新加入请清空数据目录 %s 后在控制台签一张新的加入码",
					s.nodeID, st.Cluster.ID, *dataDir)
				return
			}
			if err := s.startRaft(); err != nil {
				log.Fatalf("cluster: %v", err)
			}
			// 被下线的那台重启后仍然会起来（它只是不投票），但日志里必须说清楚，
			// 否则现场会以为"它回来了"，其实成员表里它还是 decommissioned。
			if s.raft != nil && !s.raft.IsVoter() {
				log.Printf("cluster: 本机不在 voter 集合里（已下线，或仍是 learner）：只接日志、不参选；" +
					"如果是被下线的那台，要重新加入请清空数据目录后换新加入码")
			}
			s.tlsAddrs = s.memberEndpoints()
		}
	}
	s.resetHardIndex()
	go s.clusterSweep(ctx)

	go s.ttlSweeper(ctx)

	go s.opsShipper(ctx)

	go s.liveApplier(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enroll", s.handleEnroll)
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/config/bundle", s.handleConfigBundle)
	mux.HandleFunc("/v1/rr/config", s.handleRRConfig)
	mux.HandleFunc("/v1/underlay/bundle", s.handleUnderlayBundle)
	mux.HandleFunc("/v1/mapping", s.handleMapping)
	mux.HandleFunc("/v1/observe", s.handleObserve)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/diag", s.handleDiagResult)
	mux.HandleFunc("/v1/reach", s.handleReach)
	mux.HandleFunc("/v1/registry", s.handleRegistry)
	mux.HandleFunc("/v1/watch", s.handleWatch)
	// 节点间 RPC：选票、日志、快照、加入。全部走共享密钥，不认设备证书。
	mux.HandleFunc("/v1/cluster/vote", s.handleClusterVote)
	mux.HandleFunc("/v1/cluster/append", s.handleClusterAppend)
	mux.HandleFunc("/v1/cluster/snapshot", s.handleClusterSnapshot)
	mux.HandleFunc("/v1/cluster/join", s.handleClusterJoin)
	mux.HandleFunc("/v1/cluster/ca", s.handleClusterCA)

	hs := &http.Server{
		Addr:    *listen,
		Handler: s.deviceGuard(mux),
		// VerifyClientCertIfGiven: enrollment has no cert yet; every other
		// handler enforces identity via peerTenantFromCert. TLS client auth
		// cannot vary per path at handshake time, so enforcement lives in one
		// function instead of being spread across handlers.
		TLSConfig: &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return s.cert.Load(), nil
			},
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  caPool,
			MinVersion: tls.VersionTLS12,
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
		var consoleTLSConf *tls.Config
		if *consoleTLS {
			info, err := loadConsoleCert(*dataDir, *consoleCert, *consoleKey, *consoleSAN)
			if err != nil {
				log.Fatalf("console TLS: %v", err)
			}
			consoleTLSConf = &tls.Config{
				Certificates: []tls.Certificate{info.cert},
				MinVersion:   tls.VersionTLS12,
			}
			state := "reused"
			if !info.reused {
				state = "generated"
			}
			// SAN 打全：操作员要靠这一行知道该把哪个名字写进浏览器、哪张证书
			// 装进钥匙串，报"证书不对"的时候也先看这里。
			log.Printf("console TLS cert %s: %s", state, strings.Join(info.sans, ", "))
		}
		handler := s.adminHandler()
		adminSrv = &http.Server{Addr: *consoleAddr, Handler: handler, TLSConfig: consoleTLSConf}
		go func() {
			if consoleTLSConf != nil {
				log.Printf("console listening on https://%s/", *consoleAddr)
			} else {
				log.Printf("console listening on http://%s/", *consoleAddr)
			}
			if generated {
				log.Printf("console login token (generated, shown once): %s", s.adminToken)
			}
			var err error
			if consoleTLSConf != nil {
				// 证书已经在 TLSConfig 里，两个空参数是 http.Server 的既有约定
				// （它会去翻 TLSConfig.Certificates）。
				err = adminSrv.ListenAndServeTLS("", "")
			} else {
				err = adminSrv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				log.Printf("console listener stopped: %v", err)
			}
		}()
		if *consoleHTTP != "" {
			plain := &http.Server{Addr: *consoleHTTP, Handler: handler}
			go func() {
				log.Printf("console (plain, for local tooling) listening on http://%s/", *consoleHTTP)
				if err := plain.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Printf("plain console listener stopped: %v", err)
				}
			}()
		}
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
