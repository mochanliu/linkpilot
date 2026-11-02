// The VRRP virtual router is configured through the binary API, and in this VPP
// build that is the only way in: `vrrp`, `show vrrp` and `show vrrp brief` all
// answer "unknown input" while the plugin itself is loaded (measured 2026-11-02
// on lab-cpe1, VPP 26.06-release, vrrp_plugin.so present in `show plugin`).
//
// Byte layout is not guessed. Every offset below is gcc-printed from the header
// the box installs -- /usr/include/vpp_plugins/vrrp/vrrp.api_types.h -- and the
// structs are __attribute__((packed)), so a u8 next to a u32 has no padding:
//
//	vrrp_vr_conf    12 B  = sw_if_index(4) vr_id(1) priority(1) interval(2) flags(4)
//	vrrp_vr_runtime 21 B  = state(4) master_adv_int(2) skew(2) master_down_int(2)
//	                        mac(6) tracking{interfaces_dec(4) priority(1)}
//	vrrp_vr_details 40 B + 17*n_addrs   (the fixed part: header 6 + conf + runtime + n_addrs)
//
// Three consequences worth stating because they shape the callers. All three were
// measured on lab-cpe1 with tools/vrrp-probe (three rounds, 2026-11-02); the
// per-case numbers live in the note at the top of that file.
//
//   - `vrrp_vr_details` carries **no vrrp_index**. Only `vrrp_vr_update_reply`
//     returns one, and the probe showed it is a slot in a global pool that is
//     freed and handed to the next added VR, so it is not an identity a
//     reconciler could store. Delete therefore goes through `vrrp_vr_add_del`
//     with is_add=0, keyed by (sw_if_index, vr_id) -- exactly the key the dump
//     does give us.
//   - `interval` is in **centiseconds**, not ms (the api comment says "interval
//     between advertisements in centiseconds"). Whoever holds milliseconds must
//     divide here and say so in the name; passing ms would advertise 100x too fast.
//   - The VR's key is set at add and cannot be rewritten in place: a second add
//     on an existing (sw_if_index, vr_id) returns ENTRY_ALREADY_EXISTS and leaves
//     the original untouched. So "change a parameter" is delete-then-add, which
//     costs an election -- see VRRPUpdate.
//
// retvals are named from FDio/govpp's copy of vpp/src/vnet/error.h (the box
// installs no error header): -6 NO_SUCH_ENTRY, -7 INVALID_VALUE,
// -73 INVALID_ARGUMENT, -116 ENTRY_ALREADY_EXISTS.
//
// These messages are deliberately NOT in requiredVersions: vrrp is a plugin, and
// a box that does not load it must still pass Dial (same precedent as the
// sr_wg_plugin-optional messages). A box without it fails at the first call with
// "VPP 消息表里没有 vrrp_vr_add_del", which is the true statement.

package vppapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// VRRP flag bits (vl_api_vrrp_vr_flags_t). IPV6 is not exposed: this project has
// no customer IPv6 anywhere, and a VR's family is decided by the addresses it is
// given plus this flag -- carrying a bit nobody can set would be dead code.
const (
	VRRPFlagPreempt = 1 << 0
	VRRPFlagAccept  = 1 << 1
	VRRPFlagUnicast = 1 << 2
)

// VRRPState is vl_api_vrrp_vr_state_t. The numbers are the enum's values, so a
// caller can compare against the RFC 5798 name it knows.
type VRRPState uint32

const (
	VRRPStateInit     VRRPState = 0
	VRRPStateBackup   VRRPState = 1
	VRRPStateMaster   VRRPState = 2
	VRRPStateIntfDown VRRPState = 3
)

func (s VRRPState) String() string {
	switch s {
	case VRRPStateInit:
		return "INIT"
	case VRRPStateBackup:
		return "BACKUP"
	case VRRPStateMaster:
		return "MASTER"
	case VRRPStateIntfDown:
		return "INTF_DOWN"
	}
	return fmt.Sprintf("state(%d)", uint32(s))
}

// VRRPVR is one virtual router as VPP reports it: the config we sent plus the
// runtime the plugin maintains. A VR that was added but never started sits in
// INIT, advertises nothing, and reads back with zero timers -- but its virtual
// MAC is already filled in (measured: 00:00:5e:00:01:<vr_id>).
type VRRPVR struct {
	SWIfIndex  uint32
	VRID       uint8
	Priority   uint8
	IntervalCS uint16
	Preempt    bool
	Accept     bool
	Unicast    bool
	Addrs      []netip.Addr

	State         VRRPState
	MasterAdvCS   uint16
	SkewCS        uint16
	MasterDownCS  uint16
	MasterMAC     net.HardwareAddr
	TrackIfDown   uint32
	TrackPriority uint8
}

// VRRPParams is what a caller sends to create or update a VR. IntervalCS is
// centiseconds; a caller holding milliseconds converts, and the controller only
// accepts whole centiseconds (see cmd/sr-ctrl config_check.go).
type VRRPParams struct {
	SWIfIndex  uint32
	VRID       uint8
	Priority   uint8
	IntervalCS uint16
	Preempt    bool
	Accept     bool
	Unicast    bool
	Addrs      []netip.Addr
}

func (p VRRPParams) validate() error {
	if p.VRID == 0 {
		return errors.New("vppapi: vr_id 不能是 0")
	}
	if len(p.Addrs) == 0 {
		return errors.New("vppapi: 虚拟路由器至少要有一个虚拟地址")
	}
	if len(p.Addrs) > 255 {
		return fmt.Errorf("vppapi: 虚拟地址 %d 个，超过 n_addrs 的 u8", len(p.Addrs))
	}
	for _, a := range p.Addrs {
		// Two ways to be wrong that VPP would accept without complaint: an invalid
		// Addr encodes as 0.0.0.0 (the failure mode this package has been burned by
		// before), and an IPv6 one silently becomes a v4 VR's garbage address,
		// because the family is a flag on the *VR*, not per address.
		if !a.IsValid() || a.IsUnspecified() {
			return fmt.Errorf("vppapi: 虚拟地址 %q 无效", a)
		}
		if !a.Is4() {
			return fmt.Errorf("vppapi: 虚拟地址 %q 不是 IPv4（这条通路只配 v4：基础配置里没有客户 IPv6）", a)
		}
	}
	return nil
}

// Params turns a row read back by VRRPDump into the parameter set to send for it.
// The reconciler needs this in both directions: to delete a VR it must echo the
// fields VPP has (a delete with a zero interval is refused), and to compare a
// desired row against the live one the live one has to be in the same shape.
func (v VRRPVR) Params() VRRPParams {
	return VRRPParams{
		SWIfIndex:  v.SWIfIndex,
		VRID:       v.VRID,
		Priority:   v.Priority,
		IntervalCS: v.IntervalCS,
		Preempt:    v.Preempt,
		Accept:     v.Accept,
		Unicast:    v.Unicast,
		Addrs:      v.Addrs,
	}
}

func (p VRRPParams) flags() uint32 {
	var f uint32
	if p.Preempt {
		f |= VRRPFlagPreempt
	}
	if p.Accept {
		f |= VRRPFlagAccept
	}
	if p.Unicast {
		f |= VRRPFlagUnicast
	}
	return f
}

// VRRPAdd creates a VR. A second add on an existing (sw_if_index, vr_id) is
// refused, not replaced: measured 2026-11-02 on lab-cpe1, retval=-116
// ENTRY_ALREADY_EXISTS and the original VR came back from the dump with every
// field untouched. This function is therefore not idempotent, and the
// reconciler must read the dump and skip (or rebuild) instead of re-adding --
// see internal/apply/vrrp.go.
func (c *Client) VRRPAdd(ctx context.Context, p VRRPParams) error {
	if err := p.validate(); err != nil {
		return err
	}
	body := make([]byte, 0, 14+17*len(p.Addrs))
	body = append(body, 1) // is_add
	body = appendVRRPFields(body, p)
	frame, err := c.call(ctx, "vrrp_vr_add_del", "vrrp_vr_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("vrrp_vr_add_del", frame)
}

// VRRPDelByKey removes the VR keyed by (sw_if_index, vr_id): is_add=0 on the add
// message. It takes the whole parameter set, not just the key, because VPP
// validates fields on this path too: measured 2026-11-02 on lab-cpe1, a delete
// sent with interval=0 came back retval=-7 INVALID_VALUE. Echoing the fields read
// from VRRPDump returns ok and really removes the VR -- including a VR that is
// sitting in MASTER or BACKUP, no prior stop needed (measured, same round). So
// the reconciler deletes with the dump's own priority/interval/flags/addrs.
func (c *Client) VRRPDelByKey(ctx context.Context, p VRRPParams) error {
	if err := p.validate(); err != nil {
		return err
	}
	body := make([]byte, 0, 14+17*len(p.Addrs))
	body = append(body, 0) // is_add
	body = appendVRRPFields(body, p)
	frame, err := c.call(ctx, "vrrp_vr_add_del", "vrrp_vr_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("vrrp_vr_add_del", frame)
}

// VRRPDelByIndex removes the VR by its vrrp_index. This is the message designed
// for deletion -- 4 bytes, no fields to echo -- and it works (measured), but the
// index is a slot in a global pool: VPP frees it on delete and hands the same
// number to the next VR added, so the number says nothing about which VR it was
// unless you read it from this process's own update reply. The reconciler must
// not store or cache it across applies; VRRPDelByKey is the safe delete.
func (c *Client) VRRPDelByIndex(ctx context.Context, vrrpIndex uint32) error {
	frame, err := c.call(ctx, "vrrp_vr_del", "vrrp_vr_del_reply", appendU32(nil, vrrpIndex))
	if err != nil {
		return err
	}
	return c.retval("vrrp_vr_del", frame)
}

// VRRPUpdate rewrites a VR's parameters in place. VPP answers with the
// vrrp_index of the VR it changed -- the only place that number is readable from,
// since neither the add reply nor the dump carries one (printed from the header:
// vl_api_vrrp_vr_add_del_reply_t is retval only).
//
// Measured 2026-11-02, update finds the VR **by vrrp_index and nothing else**: a
// nonexistent index returns -6 NO_SUCH_ENTRY, and an index that belongs to a
// different VR returns -73 INVALID_ARGUMENT even when the (sw_if_index, vr_id) in
// the body is a real VR on the box. It never falls back to the key. Combined with
// the index being unreadable from the dump, that means a reconciler working from
// a dump cannot call this at all: in-place parameter change is off the table, and
// internal/apply/vrrp.go rebuilds with delete-then-add instead.
func (c *Client) VRRPUpdate(ctx context.Context, vrrpIndex uint32, p VRRPParams) (uint32, error) {
	if err := p.validate(); err != nil {
		return 0, err
	}
	body := make([]byte, 0, 17+17*len(p.Addrs))
	body = appendU32(body, vrrpIndex)
	body = appendVRRPFields(body, p)
	frame, err := c.call(ctx, "vrrp_vr_update", "vrrp_vr_update_reply", body)
	if err != nil {
		return 0, err
	}
	if err := c.retval("vrrp_vr_update", frame); err != nil {
		return 0, err
	}
	if len(frame) < 14 {
		return 0, fmt.Errorf("vppapi: vrrp_vr_update_reply 太短 (%d B)", len(frame))
	}
	return binary.BigEndian.Uint32(frame[10:14]), nil
}

// VRRPStartStop puts a VR into (or out of) the protocol. `start` moves it out of
// INIT; without it a configured VR never advertises and never becomes master.
// Repeating `start` on a running VR returns ok and leaves the state alone
// (measured), so callers need not read the state before starting.
//
// Starting does not guarantee winning an election on an empty LAN: measured
// 2026-11-02, 6 of 8 freshly added+started VRs on a single-router tap stayed in
// BACKUP past master_down, while stop-then-start or a link down/up event elected
// MASTER in 3 of 3. Whatever the reason, the applier must not treat "started" as
// "master" -- the honest statement of a VR's state is the dump.
func (c *Client) VRRPStartStop(ctx context.Context, swIfIndex uint32, vrID uint8, isIPv6, start bool) error {
	if vrID == 0 {
		return errors.New("vppapi: vr_id 不能是 0")
	}
	body := make([]byte, 0, 7)
	body = appendU32(body, swIfIndex)
	body = append(body, vrID, boolByte(isIPv6), boolByte(start))
	frame, err := c.call(ctx, "vrrp_vr_start_stop", "vrrp_vr_start_stop_reply", body)
	if err != nil {
		return err
	}
	return c.retval("vrrp_vr_start_stop", frame)
}

// VRRPDump reads back the VRs. Pass ^uint32(0) for "every interface" -- measured
// 2026-11-02 on this build, vrrp_vr_dump honours VPP's ~0 convention (0 rows for
// index 0, 1 row for the experiment port, every row for ~0), which is *not* true
// of ip_address_dump on the same box (measured 2026-10-28: it ignores ~0). So the
// reconciler can read the whole box's VRRP state in one dump; no need to
// enumerate interfaces.
//
// Whatever index was asked for, rows belonging to another interface are dropped
// here rather than returned -- the argument is a promise to the caller, not a
// hint.
func (c *Client) VRRPDump(ctx context.Context, swIfIndex uint32) ([]VRRPVR, error) {
	frames, err := c.dump(ctx, "vrrp_vr_dump", "vrrp_vr_details", appendU32(nil, swIfIndex))
	if err != nil {
		return nil, err
	}
	out := make([]VRRPVR, 0, len(frames))
	for _, f := range frames {
		vr, err := parseVRRPDetails(f)
		if err != nil {
			return nil, err
		}
		if swIfIndex != ^uint32(0) && vr.SWIfIndex != swIfIndex {
			// VPP does filter on this message (measured: dumping one port while a
			// second VR sat on another returned only the asked-for row), but the
			// filter stays -- the argument is a promise, and a plugin upgrade that
			// stops honouring it must not silently hand the caller foreign rows.
			continue
		}
		out = append(out, vr)
	}
	return out, nil
}

// vrrpDetailsFixedLen is a vrrp_vr_details frame with no addresses: the 6-byte
// reply header plus conf(12) + runtime(21) + n_addrs(1).
const vrrpDetailsFixedLen = 40

func appendVRRPFields(b []byte, p VRRPParams) []byte {
	b = appendU32(b, p.SWIfIndex)
	b = append(b, p.VRID, p.Priority)
	b = appendU16(b, p.IntervalCS)
	b = appendU32(b, p.flags())
	b = append(b, byte(len(p.Addrs)))
	for _, a := range p.Addrs {
		b = appendAddress(b, net.IP(a.AsSlice()))
	}
	return b
}

func parseVRRPDetails(f []byte) (VRRPVR, error) {
	if len(f) < vrrpDetailsFixedLen {
		return VRRPVR{}, fmt.Errorf("vppapi: vrrp_vr_details 太短 (%d B < %d B)，vrrp 插件的结构变了",
			len(f), vrrpDetailsFixedLen)
	}
	// Offsets are the gcc-printed ones: conf starts at 6 (right after msg_id+
	// context), runtime at 18, n_addrs at 39.
	vr := VRRPVR{
		SWIfIndex:     binary.BigEndian.Uint32(f[6:10]),
		VRID:          f[10],
		Priority:      f[11],
		IntervalCS:    binary.BigEndian.Uint16(f[12:14]),
		Preempt:       binary.BigEndian.Uint32(f[14:18])&VRRPFlagPreempt != 0,
		Accept:        binary.BigEndian.Uint32(f[14:18])&VRRPFlagAccept != 0,
		Unicast:       binary.BigEndian.Uint32(f[14:18])&VRRPFlagUnicast != 0,
		State:         VRRPState(binary.BigEndian.Uint32(f[18:22])),
		MasterAdvCS:   binary.BigEndian.Uint16(f[22:24]),
		SkewCS:        binary.BigEndian.Uint16(f[24:26]),
		MasterDownCS:  binary.BigEndian.Uint16(f[26:28]),
		MasterMAC:     net.HardwareAddr(append([]byte(nil), f[28:34]...)),
		TrackIfDown:   binary.BigEndian.Uint32(f[34:38]),
		TrackPriority: f[38],
	}
	n := int(f[39])
	if len(f) < vrrpDetailsFixedLen+17*n {
		return VRRPVR{}, fmt.Errorf("vppapi: vrrp_vr_details 说 %d 个地址，帧里只剩 %d B",
			n, len(f)-vrrpDetailsFixedLen)
	}
	for i := 0; i < n; i++ {
		a := readAddressNetip(f[vrrpDetailsFixedLen+17*i:])
		if !a.IsValid() {
			return VRRPVR{}, fmt.Errorf("vppapi: vrrp_vr_details 第 %d 个地址的地址族读不出来", i)
		}
		vr.Addrs = append(vr.Addrs, a)
	}
	return vr, nil
}
