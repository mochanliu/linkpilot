// Package underlay holds the tenant-scoped tunnel layer: the pools every tunnel
// draws from, the tunnels themselves, reusable path templates and the
// allocator that hands out ports and overlay addresses.
//
// Three rules from the design (docs/underlay-design.md) are enforced here
// rather than in the UI, because a console that only *shows* a rule is one
// request away from being bypassed:
//
//   - ports are unique PER DEVICE, not per tenant: two devices may both listen
//     on 20000, and the dataplane is explicitly built to allow the two ends of
//     one tunnel to use different ports;
//   - a device needs exactly ONE transit port, however many tunnels are
//     relayed through it (sr-wg4-transit forwards by the SR header, not by
//     port), so the port is a per-device value, never per tunnel;
//   - overlay /32s are handed out FIFO and released to the BACK of the queue:
//     an overlay address ends up in peers' next hops, and reusing one
//     immediately would let a stale route silently land on a new tunnel.
package underlay

import (
	"fmt"
	"net/netip"
	"strings"

	"sdwan/controller/internal/wire"
)

// PortRange is a closed interval of UDP ports.
type PortRange struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
}

func (r PortRange) Valid() bool            { return r.Min > 0 && r.Min <= r.Max }
func (r PortRange) Contains(p uint16) bool { return r.Valid() && p >= r.Min && p <= r.Max }
func (r PortRange) Len() int {
	if !r.Valid() {
		return 0
	}
	return int(r.Max) - int(r.Min) + 1
}

// Overlaps reports whether two ranges share at least one port. Ports are the
// multiplexing key of the dataplane: a transcript port that doubles as a VPN
// listen port has no defined receive path, so the two pools must not intersect.
func (r PortRange) Overlaps(o PortRange) bool {
	return r.Valid() && o.Valid() && r.Min <= o.Max && o.Min <= r.Max
}

func (r PortRange) String() string { return fmt.Sprintf("%d-%d", r.Min, r.Max) }

// Bounds the pools may not cross. Below 1024 needs root and collides with
// system services; 32768-60999 is the kernel's ephemeral range, which would
// make an outbound socket steal a tunnel's port (or the reverse).
const (
	privilegedPortMax  = 1023
	ephemeralPortMin   = 32768
	ephemeralPortMax   = 60999
	maxOverlayHostBits = 16 // /16 pool = 65534 usable /32s; a /8 would be 16M entries
)

var ephemeralPorts = PortRange{Min: ephemeralPortMin, Max: ephemeralPortMax}

// Settings are the tenant's global underlay parameters. Defaults are built in
// (see DefaultSettings) and the tenant may change any of them.
type Settings struct {
	VPNPorts     PortRange   `json:"vpn_ports"`
	VPNAvoid     []PortRange `json:"vpn_avoid,omitempty"`
	TransitPorts PortRange   `json:"transit_ports"`
	OverlayCIDR  string      `json:"overlay_cidr"`

	ProbeIntervalMS uint32 `json:"probe_interval_ms"`
	BFDMinTxMS      uint32 `json:"bfd_min_tx_ms"`
	BFDMinRxMS      uint32 `json:"bfd_min_rx_ms"`
	BFDDetectMult   uint8  `json:"bfd_detect_mult"`

	UpdatedAt int64 `json:"updated_at_unix,omitempty"`
}

// DefaultSettings is the suite a tenant starts with: both pools sit below the
// ephemeral range and apart from each other, and the timers are the values the
// lab measured.
func DefaultSettings() Settings {
	return Settings{
		VPNPorts:        PortRange{Min: 20000, Max: 29999},
		TransitPorts:    PortRange{Min: 15000, Max: 15999},
		OverlayCIDR:     "10.250.0.0/16",
		ProbeIntervalMS: wire.DefaultProbeIntervalMS,
		BFDMinTxMS:      wire.DefaultBFDMinTxUS / 1000,
		BFDMinRxMS:      wire.DefaultBFDMinRxUS / 1000,
		BFDDetectMult:   wire.DefaultBFDDetectMult,
	}
}

// Normalize fills every zero field with its default. It is what the API calls
// on a submitted form, so a tenant that only edits one range does not silently
// zero the others.
func (s Settings) Normalize() Settings {
	d := DefaultSettings()
	if !s.VPNPorts.Valid() {
		s.VPNPorts = d.VPNPorts
	}
	if !s.TransitPorts.Valid() {
		s.TransitPorts = d.TransitPorts
	}
	if strings.TrimSpace(s.OverlayCIDR) == "" {
		s.OverlayCIDR = d.OverlayCIDR
	}
	if s.ProbeIntervalMS == 0 {
		s.ProbeIntervalMS = d.ProbeIntervalMS
	}
	if s.BFDMinTxMS == 0 {
		s.BFDMinTxMS = d.BFDMinTxMS
	}
	if s.BFDMinRxMS == 0 {
		s.BFDMinRxMS = d.BFDMinRxMS
	}
	if s.BFDDetectMult == 0 {
		s.BFDDetectMult = d.BFDDetectMult
	}
	s.OverlayCIDR = strings.TrimSpace(s.OverlayCIDR)
	return s
}

// Validate checks the ranges and timers. It runs on a normalized value.
func (s Settings) Validate() error {
	if err := validatePool("VPN 端口池", s.VPNPorts); err != nil {
		return err
	}
	if err := validatePool("中转端口池", s.TransitPorts); err != nil {
		return err
	}
	if s.VPNPorts.Overlaps(s.TransitPorts) {
		return fmt.Errorf("VPN 端口池 %s 与中转端口池 %s 重叠：同一个端口不能既解 WG 又做 SR 中转",
			s.VPNPorts, s.TransitPorts)
	}
	for _, a := range s.VPNAvoid {
		if !a.Valid() {
			return fmt.Errorf("排除段 %d-%d 无效", a.Min, a.Max)
		}
		if !s.VPNPorts.Contains(a.Min) || !s.VPNPorts.Contains(a.Max) {
			return fmt.Errorf("排除段 %s 不在 VPN 端口池 %s 内，删除它或扩大端口池", a, s.VPNPorts)
		}
	}
	if _, err := s.overlayPrefix(); err != nil {
		return err
	}
	if s.ProbeIntervalMS < 50 || s.ProbeIntervalMS > 10000 {
		return fmt.Errorf("探针间隔 %dms 超出 50-10000ms", s.ProbeIntervalMS)
	}
	if s.BFDMinTxMS < 1 || s.BFDMinTxMS > 3000 {
		return fmt.Errorf("BFD 最小发送间隔 %dms 超出 1-3000ms", s.BFDMinTxMS)
	}
	if s.BFDMinRxMS < 1 || s.BFDMinRxMS > 3000 {
		return fmt.Errorf("BFD 最小接收间隔 %dms 超出 1-3000ms", s.BFDMinRxMS)
	}
	if s.BFDDetectMult < 1 || s.BFDDetectMult > 100 {
		return fmt.Errorf("BFD 检测倍数 %d 超出 1-100", s.BFDDetectMult)
	}
	return nil
}

func validatePool(what string, r PortRange) error {
	if !r.Valid() {
		return fmt.Errorf("%s %s 无效（需要 min <= max 且都大于 0）", what, r)
	}
	if r.Min <= privilegedPortMax {
		return fmt.Errorf("%s %s 含 1024 以下端口：需要 root 且会和系统服务抢", what, r)
	}
	if r.Overlaps(ephemeralPorts) {
		return fmt.Errorf("%s %s 与内核临时端口段 %s 重叠：出站连接会抢走隧道端口", what, r, ephemeralPorts)
	}
	return nil
}

// overlayPrefix parses and bounds the overlay pool. IPv4 only: the SR header's
// segment address is 4 bytes.
func (s Settings) overlayPrefix() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s.OverlayCIDR))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("overlay 地址池 %q 不是合法 CIDR: %w", s.OverlayCIDR, err)
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("overlay 地址池 %s 不是 IPv4：SR 头里的段地址只有 4 字节", p)
	}
	if p.Addr().IsMulticast() || p.Addr().IsLoopback() || p.Addr().IsLinkLocalUnicast() {
		return netip.Prefix{}, fmt.Errorf("overlay 地址池 %s 不是可用的单播网段", p)
	}
	if p.Bits() < maxOverlayHostBits {
		return netip.Prefix{}, fmt.Errorf("overlay 地址池 %s 太大：最大 /%d（65534 个 /32）",
			p, maxOverlayHostBits)
	}
	return p.Masked(), nil
}

// OverlaySize is how many /32s the pool hands out. It exists separately from
// OverlayHosts because the console asks for the total on every refresh, and
// materializing 65k addresses to count them would make a page render quadratic.
func (s Settings) OverlaySize() (int, error) {
	p, err := s.overlayPrefix()
	if err != nil {
		return 0, err
	}
	count := uint64(1) << uint(32-p.Bits())
	if p.Bits() <= 30 {
		count -= 2
	}
	return int(count), nil
}

// OverlayHosts lists every usable /32 of the pool in ascending order. Network
// and broadcast are skipped for prefixes that have them.
func (s Settings) OverlayHosts() ([]netip.Addr, error) {
	p, err := s.overlayPrefix()
	if err != nil {
		return nil, err
	}
	base := p.Masked().Addr().As4()
	first := be32(base)
	count := uint64(1) << uint(32-p.Bits())
	lo, hi := uint64(0), count
	if p.Bits() <= 30 {
		lo, hi = 1, count-1
	}
	out := make([]netip.Addr, 0, hi-lo)
	for off := lo; off < hi; off++ {
		out = append(out, fromBE32(uint32(uint64(first)+off)))
	}
	return out, nil
}

func be32(a [4]byte) uint32 {
	return uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
}

func fromBE32(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
