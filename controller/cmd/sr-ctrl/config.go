// Basic configuration for one CPE: the customer-facing half of the box.
//
// Ownership is decided by measurement, not taste (lab-cpe1, 2026-10-27):
// FRR's zebra/staticd writes reach the kernel and linux-nl mirrors them into
// VPP, in both directions. So this layer owns hostname, customer interface
// addresses, static routes, BGP and OSPF, and emits them as FRR config. VPP
// keeps only what lcp-sync cannot carry: the table an interface lives in, MTU,
// admin state and the lcp TAP itself -- all runtime state that VPP loses on
// restart, which is why renderBoot() exists and why the on-box applier (not
// the controller) must replay it at boot.
//
// The VPN/tunnel side (WAN address, tunnels, keys, MTU budget) is underlay and
// never appears here.
package main

import (
	"net/netip"
	"sort"
	"strings"
)

const (
	vpnReservedOverlay = 250 // overlay table, owned by the route plane
	vpnMaxID           = 65535

	cfgMaxVPNs      = 64
	cfgMaxIfaces    = 64
	cfgMaxStatics   = 512
	cfgMaxNeighbors = 64
	cfgMaxBody      = 1 << 20
)

// BasicConfig is one device's customer-side configuration. VPN 0 always
// exists; a VPN is also the VPP table number it maps to.
type BasicConfig struct {
	Hostname string         `json:"hostname"`
	VPNs     []ConfigVPN    `json:"vpns"`
	Ifaces   []ConfigIface  `json:"ifaces"`
	Statics  []ConfigStatic `json:"statics"`
}

// ConfigVPN is a routing instance. BGP and OSPF are scoped per VPN because
// that is how FRR models them (`router bgp ASN vrf X`, `router ospf vrf X`).
type ConfigVPN struct {
	ID   uint32     `json:"id"`
	Name string     `json:"name,omitempty"`
	BGP  BGPConfig  `json:"bgp"`
	OSPF OSPFConfig `json:"ospf"`
}

type BGPConfig struct {
	On        bool      `json:"on"`
	ASN       uint32    `json:"asn"`
	RouterID  string    `json:"router_id"`
	Neighbors []BGPPeer `json:"neighbors"`
}

// BGPPeer is one customer-side session. RemoteAS equal to the local ASN means
// iBGP: reachability is no longer restricted to the directly connected subnet
// (TTL 255), and the customer usually needs next-hop-self from this box.
type BGPPeer struct {
	IP          string `json:"ip"`
	RemoteAS    uint32 `json:"remote_as"`
	Description string `json:"description,omitempty"`
	NextHopSelf bool   `json:"next_hop_self,omitempty"`
}

// OSPFConfig has no process number on purpose: FRR runs exactly one OSPF
// instance per VRF, so a process id would be a field that never reaches the
// config.
type OSPFConfig struct {
	On       bool   `json:"on"`
	RouterID string `json:"router_id"`
	Area     uint32 `json:"area"`
}

// ConfigIface is a customer port: a VPP interface the box owns, mirrored into
// the kernel as an lcp TAP so FRR can address it.
type ConfigIface struct {
	VPP       string `json:"vpp"`
	LCP       string `json:"lcp"`
	IP        string `json:"ip"`
	PrefixLen uint8  `json:"prefix_len"`
	VPN       uint32 `json:"vpn"`
	MTU       uint32 `json:"mtu"`
	ArpIgnore bool   `json:"arp_ignore"`
	Up        bool   `json:"up"`
}

type ConfigStatic struct {
	Prefix  string `json:"prefix"`
	NextHop string `json:"next_hop"`
	VPN     uint32 `json:"vpn"`
}

// DeviceConfig is the persisted draft for one (tenant, node) plus, once the
// tenant publishes it, the frozen bundle the device is told to install.
//
// The two are deliberately separate values. A draft is whatever was saved last
// -- editing after a publish must not silently change what the device is being
// asked to run, because "the console says v5 and the box installed v5 with
// different content" is unpublishable evidence. Publishing snapshots the
// rendered artifacts; saving a new draft only moves Version.
type DeviceConfig struct {
	TenantID string `json:"tenant_id"`
	NodeID   uint16 `json:"node_id"`
	// PreAuthID is set only while the draft hangs off a 预授权 record: there is no
	// node yet, so NodeID stays 0. The binding clears it when the draft moves into
	// Configs, which is also what makes the move provable in the state file.
	PreAuthID string      `json:"preauth_id,omitempty"`
	Draft     BasicConfig `json:"draft"`
	// Leak is the 路由泄露 rows of this device, both directions. It deliberately
	// lives here and not inside BasicConfig: the 基础配置 page saves a whole
	// BasicConfig, and a leak row stored in it would be wiped by every save from
	// that page.
	Leak      LeakSet `json:"leak"`
	Version   int     `json:"version"`
	UpdatedAt int64   `json:"updated_at_unix"`
	UpdatedBy string  `json:"updated_by,omitempty"`

	Published *PublishedBundle `json:"published,omitempty"`
}

// PublishedBundle is the snapshot a device is asked to install. Checks travel
// with it for the same reason they travel in the console bundle: the device's
// applier refuses a bundle whose checks contain errors, and it must be able to
// make that decision without asking the controller again.
type PublishedBundle struct {
	Version     int             `json:"version"`
	PublishedAt int64           `json:"published_at_unix"`
	PublishedBy string          `json:"published_by,omitempty"`
	Checks      []ConfigCheck   `json:"checks"`
	Artifacts   configArtifacts `json:"artifacts"`
}

// ConfigCheck is one server-side validation result. Errors block a future
// apply; warnings do not. The page shows both, because "your eBGP neighbor is
// not on a directly connected subnet" is exactly the mistake this layer exists
// to catch before it reaches a box.
type ConfigCheck struct {
	Severity string `json:"severity"` // error | warning | info
	Text     string `json:"text"`
}

func configKey(tenantID string, nodeID uint16) string { return key(tenantID, nodeID) }

// defaultBasicConfig is what an unconfigured device shows: VPN 0 exists, empty
// everything else. Nothing is invented (no interface, no address).
func defaultBasicConfig() BasicConfig {
	return BasicConfig{
		VPNs:    []ConfigVPN{{ID: 0, Name: "default"}},
		Ifaces:  []ConfigIface{},
		Statics: []ConfigStatic{},
	}
}

func normalizeBasicConfig(c *BasicConfig) {
	c.Hostname = strings.TrimSpace(c.Hostname)
	for i := range c.VPNs {
		c.VPNs[i].Name = strings.TrimSpace(c.VPNs[i].Name)
		c.VPNs[i].BGP.RouterID = strings.TrimSpace(c.VPNs[i].BGP.RouterID)
		if c.VPNs[i].BGP.Neighbors == nil {
			c.VPNs[i].BGP.Neighbors = []BGPPeer{}
		}
		for j := range c.VPNs[i].BGP.Neighbors {
			n := &c.VPNs[i].BGP.Neighbors[j]
			n.IP = strings.TrimSpace(n.IP)
			n.Description = strings.TrimSpace(n.Description)
		}
		c.VPNs[i].OSPF.RouterID = strings.TrimSpace(c.VPNs[i].OSPF.RouterID)
	}
	for i := range c.Ifaces {
		c.Ifaces[i].VPP = strings.TrimSpace(c.Ifaces[i].VPP)
		c.Ifaces[i].LCP = strings.TrimSpace(c.Ifaces[i].LCP)
		c.Ifaces[i].IP = strings.TrimSpace(c.Ifaces[i].IP)
	}
	for i := range c.Statics {
		c.Statics[i].Prefix = strings.TrimSpace(c.Statics[i].Prefix)
		c.Statics[i].NextHop = strings.TrimSpace(c.Statics[i].NextHop)
	}
	if len(c.VPNs) == 0 {
		c.VPNs = defaultBasicConfig().VPNs
	}
	hasZero := false
	for _, v := range c.VPNs {
		if v.ID == 0 {
			hasZero = true
			break
		}
	}
	if !hasZero {
		c.VPNs = append(c.VPNs, ConfigVPN{ID: 0, Name: "default"})
	}
	sort.SliceStable(c.VPNs, func(i, j int) bool { return c.VPNs[i].ID < c.VPNs[j].ID })
	if c.Ifaces == nil {
		c.Ifaces = []ConfigIface{}
	}
	if c.Statics == nil {
		c.Statics = []ConfigStatic{}
	}
}

func parseIPv4(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.IsUnspecified() {
		return netip.Addr{}, false
	}
	return a, true
}

func ifacePrefix(it ConfigIface) (netip.Prefix, bool) {
	a, ok := parseIPv4(it.IP)
	if !ok || it.PrefixLen > 32 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, int(it.PrefixLen)).Masked(), true
}

// vpnPrefixes returns the directly connected subnets of one VPN. Reachability
// checks are per VPN: the same address in two VRFs is not a conflict.
func vpnPrefixes(c *BasicConfig, vpn uint32) []netip.Prefix {
	var out []netip.Prefix
	for _, it := range c.Ifaces {
		if it.VPN != vpn {
			continue
		}
		if p, ok := ifacePrefix(it); ok {
			out = append(out, p)
		}
	}
	return out
}

func validHostname(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func validIfaceName(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/':
		default:
			return false
		}
	}
	return true
}

// validTAPName follows Linux IFNAMSIZ (15 visible characters).
func validTAPName(s string) bool {
	if len(s) < 1 || len(s) > 15 {
		return false
	}
	for _, r := range s {
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

func validDescription(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// staticCovers reports whether a static route in this VPN makes addr
// resolvable (the iBGP recursion case).
func staticCovers(c *BasicConfig, vpn uint32, addr netip.Addr) bool {
	for _, r := range c.Statics {
		if r.VPN != vpn {
			continue
		}
		p, err := netip.ParsePrefix(r.Prefix)
		if err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}
