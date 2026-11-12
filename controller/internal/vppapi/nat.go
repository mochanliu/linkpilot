// The NAT44 EI (endpoint-independent) plane is configured through the binary
// API, and this build gives no alternative: the plugin's CLI **cannot** carry
// the two knobs that decide whether enabling it survives at all.
//
// Why the CLI path is not just inconvenient but unusable (all measured on
// lab-cpe1, VPP 26.06-release, 2026-11-11):
//
//   - `nat44 plugin enable` with the CLI's default users/user_sessions sizes
//     panics the whole VPP process: "Out-of-memory, calling os_panic()" -- the
//     per-thread user/session hash tables are allocated from the main heap and
//     the defaults do not fit a small heap. Once VPP dies, the underlay
//     takeover and the config replay race each other on the way back up; two
//     back-to-back crashes wedged the lab VM so hard only a machine reset
//     brought it back.
//   - With a bigger heap the CLI enable *succeeds* and then silently does not
//     stick: `nat44 plugin disable` right after answers "already disabled".
//     `set interface nat44` follows with bare "add <ifc> failed" lines.
//
// The API message carries every sizing knob explicitly
// (nat44_ei_plugin_enable_disable: users, user_memory, sessions,
// session_memory, user_sessions), so the caller decides the memory shape
// instead of inheriting whatever the CLI parses. That is the only path this
// file drives. Readback goes through the plugin's dumps (interfaces / addresses
// / interface addresses) -- `show nat44 ei ...` CLI output parsing is not used
// anywhere here.
//
// Byte layouts are read from /usr/share/vpp/api/plugins/nat44_ei.api.json on
// the box, not guessed. Enum vl_api_nat44_ei_config_flags_t is enumtype u8 --
// one byte, no padding: if_inside=16, if_outside=32.
package vppapi

import (
	"context"
	"fmt"
	"net/netip"
)

// Nat44 flag bits (vl_api_nat44_ei_config_flags_t, enumtype u8).
const (
	Nat44FlagIfInside  = 16
	Nat44FlagIfOutside = 32
)

// Nat44Params is the sizing half of nat44_ei_plugin_enable_disable. The zero
// memory fields mean "let the plugin compute from the counts"; Users /
// UserSessions / Sessions must all be non-zero -- an all-zero request is the
// CLI's shape, and the CLI's shape OOMs small heaps.
type Nat44Params struct {
	InsideVRF  uint32
	OutsideVRF uint32
	// Users caps concurrent NAT users per worker thread.
	Users uint32
	// UserSessions caps sessions per user per thread.
	UserSessions uint32
	// Sessions caps sessions per worker thread.
	Sessions uint32
	// UserMemory / SessionMemory override hash allocation in bytes; 0 derives
	// from the caps above.
	UserMemory    uint32
	SessionMemory uint32
}

// Nat44Enable turns the nat44_ei plugin on (enable) or off (!enable) for one
// (inside, outside) VRF pair. Disable with the same parameter set: the plugin
// validates fields on this path just like VRRP does.
func (c *Client) Nat44Enable(ctx context.Context, p Nat44Params, enable bool) error {
	body := make([]byte, 0, 28+2)
	body = appendU32(body, p.InsideVRF)
	body = appendU32(body, p.OutsideVRF)
	body = appendU32(body, p.Users)
	body = appendU32(body, p.UserMemory)
	body = appendU32(body, p.Sessions)
	body = appendU32(body, p.SessionMemory)
	body = appendU32(body, p.UserSessions)
	body = append(body, b2u8(enable)) // enable
	body = append(body, 0)            // flags: NAT44_EI_NONE
	frame, err := c.call(ctx, "nat44_ei_plugin_enable_disable", "nat44_ei_plugin_enable_disable_reply", body)
	if err != nil {
		return err
	}
	return c.retval("nat44_ei_plugin_enable_disable", frame)
}

// Nat44InterfaceFeature adds or removes the NAT44 in/out feature on one
// interface. flags carries exactly one of Nat44FlagIfInside / IfOutside -- the
// message has no direction field of its own.
func (c *Client) Nat44InterfaceFeature(ctx context.Context, swIfIndex uint32, flags byte, add bool) error {
	body := []byte{b2u8(add), flags}
	body = appendU32(body, swIfIndex)
	frame, err := c.call(ctx, "nat44_ei_interface_add_del_feature", "nat44_ei_interface_add_del_feature_reply", body)
	if err != nil {
		return err
	}
	return c.retval("nat44_ei_interface_add_del_feature", frame)
}

// Nat44InterfaceAddr marks one interface's address as a NAT pool address (the
// `nat44 add interface address <ifc>` half). flags is NAT44_EI_NONE.
func (c *Client) Nat44InterfaceAddr(ctx context.Context, swIfIndex uint32, add bool) error {
	body := []byte{b2u8(add), 0} // is_add, flags
	body = appendU32(body, swIfIndex)
	frame, err := c.call(ctx, "nat44_ei_add_del_interface_addr", "nat44_ei_add_del_interface_addr_reply", body)
	if err != nil {
		return err
	}
	return c.retval("nat44_ei_add_del_interface_addr", frame)
}

// Nat44AddressRange adds or removes one pool address range (or a single
// address when first == last) in one VRF.
func (c *Client) Nat44AddressRange(ctx context.Context, first, last netip.Addr, vrf uint32, add bool) error {
	if !first.Is4() || !last.Is4() {
		return fmt.Errorf("vppapi: nat44 地址池只支持 IPv4 (%s .. %s)", first, last)
	}
	f := first.As4()
	l := last.As4()
	body := make([]byte, 0, 4+4+4+1)
	body = append(body, f[:]...)
	body = append(body, l[:]...)
	body = appendU32(body, vrf)
	body = append(body, b2u8(add))
	frame, err := c.call(ctx, "nat44_ei_add_del_address_range", "nat44_ei_add_del_address_range_reply", body)
	if err != nil {
		return err
	}
	return c.retval("nat44_ei_add_del_address_range", frame)
}

// Nat44Interface is one row of nat44_ei_interface_details: which interface
// carries the NAT feature and on which side.
type Nat44Interface struct {
	SWIfIndex uint32
	Inside    bool
	Outside   bool
}

// Nat44Interfaces dumps the interfaces the NAT feature sits on.
func (c *Client) Nat44Interfaces(ctx context.Context) ([]Nat44Interface, error) {
	frames, err := c.dump(ctx, "nat44_ei_interface_dump", "nat44_ei_interface_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]Nat44Interface, 0, len(frames))
	for _, f := range frames {
		// details = msg_id(2) context(4) flags(1) sw_if_index(4) = 11 B.
		if len(f) < 11 {
			return nil, fmt.Errorf("vppapi: nat44_ei_interface_details 太短 (%d B)，插件结构变了", len(f))
		}
		flags := f[6]
		row := Nat44Interface{SWIfIndex: readU32(f[7:11])}
		row.Inside = flags&Nat44FlagIfInside != 0
		row.Outside = flags&Nat44FlagIfOutside != 0
		out = append(out, row)
	}
	return out, nil
}

// Nat44PoolAddress is one row of nat44_ei_address_details.
type Nat44PoolAddress struct {
	Addr netip.Addr
	VRF  uint32
}

// Nat44PoolAddresses dumps the pool addresses (nat44_ei_address_details =
// msg_id(2) context(4) ip4(4) vrf(4) = 14 B).
func (c *Client) Nat44PoolAddresses(ctx context.Context) ([]Nat44PoolAddress, error) {
	frames, err := c.dump(ctx, "nat44_ei_address_dump", "nat44_ei_address_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]Nat44PoolAddress, 0, len(frames))
	for _, f := range frames {
		if len(f) < 14 {
			return nil, fmt.Errorf("vppapi: nat44_ei_address_details 太短 (%d B)，插件结构变了", len(f))
		}
		a, ok := netip.AddrFromSlice(f[6:10])
		if !ok {
			return nil, fmt.Errorf("vppapi: nat44_ei_address_details 的地址字段非法: % x", f[6:10])
		}
		out = append(out, Nat44PoolAddress{Addr: a, VRF: readU32(f[10:14])})
	}
	return out, nil
}

// Nat44InterfaceAddrRows dumps which interfaces' addresses serve as pool
// addresses (nat44_ei_interface_addr_details = msg_id(2) context(4)
// sw_if_index(4) = 10 B).
func (c *Client) Nat44InterfaceAddrRows(ctx context.Context) ([]uint32, error) {
	frames, err := c.dump(ctx, "nat44_ei_interface_addr_dump", "nat44_ei_interface_addr_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 0, len(frames))
	for _, f := range frames {
		if len(f) < 10 {
			return nil, fmt.Errorf("vppapi: nat44_ei_interface_addr_details 太短 (%d B)，插件结构变了", len(f))
		}
		out = append(out, readU32(f[6:10]))
	}
	return out, nil
}

func b2u8(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func readU32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
