package frr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
)

// The wire shapes below are the controller's bundle 第 5 段 (docs/o6e-leak-plan.md
// §3.1/§4). They are declared here, not imported from sr-ctrl, because what binds
// the two sides is the JSON body of PUT /config and the file on disk.

// PrefixEntry is one permit line. Same JSON as the controller's library entry.
type PrefixEntry struct {
	Prefix string `json:"prefix"`
	GE     int    `json:"ge,omitempty"`
	LE     int    `json:"le,omitempty"`
}

// ImportRuleSpec is one CPE-SDWAN row with its prefix-list already expanded.
type ImportRuleSpec struct {
	VPN      uint32        `json:"vpn"`
	Sources  []string      `json:"sources"`
	Prefixes []PrefixEntry `json:"prefixes"`
}

// CompileImportRules validates the expanded rows and turns them into the
// runtime shape. It is the only place source names become zebra types.
func CompileImportRules(raw []byte) ([]ImportRule, error) {
	var specs []ImportRuleSpec
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("import_rules 不是数组: %w", err)
	}
	seen := map[uint8]bool{}
	out := make([]ImportRule, 0, len(specs))
	for _, s := range specs {
		if s.VPN > 255 {
			return nil, fmt.Errorf("import_rules 里的 VPN %d 超出 0..255", s.VPN)
		}
		if s.VPN == OverlayVRF {
			return nil, fmt.Errorf("import_rules 不能包含 VPN %d（overlay 传输域）", s.VPN)
		}
		vpn := uint8(s.VPN)
		if seen[vpn] {
			return nil, fmt.Errorf("import_rules 里 VPN %d 出现了两次", vpn)
		}
		seen[vpn] = true
		r := ImportRule{VPN: vpn}
		seenSrc := map[uint8]bool{}
		for _, name := range s.Sources {
			t, ok := ParseSource(name)
			if !ok {
				return nil, fmt.Errorf("VPN %d 的来源 %q 不认识（只支持 connected/static/ospf/bgp）", vpn, name)
			}
			if seenSrc[t] {
				continue
			}
			seenSrc[t] = true
			r.Sources = append(r.Sources, t)
		}
		for _, e := range s.Prefixes {
			allow, err := compileAllow(e)
			if err != nil {
				return nil, fmt.Errorf("VPN %d 的白名单：%w", vpn, err)
			}
			r.Allow = append(r.Allow, allow)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VPN < out[j].VPN })
	return out, nil
}

func compileAllow(e PrefixEntry) (AllowEntry, error) {
	p, err := netip.ParsePrefix(e.Prefix)
	if err != nil {
		return AllowEntry{}, fmt.Errorf("前缀 %q 无效", e.Prefix)
	}
	if !p.Addr().Is4() || p.Addr().Is4In6() {
		return AllowEntry{}, fmt.Errorf("只支持 IPv4，收到 %s", p)
	}
	if p != p.Masked() {
		return AllowEntry{}, fmt.Errorf("前缀未规范（掩码外有有效位）: %s", p)
	}
	if e.GE < 0 || e.GE > 32 || e.LE < 0 || e.LE > 32 {
		return AllowEntry{}, fmt.Errorf("%s 的 ge/le 必须在 0..32", p)
	}
	if e.GE > 0 && e.GE < p.Bits() {
		return AllowEntry{}, fmt.Errorf("%s 的 ge %d 小于前缀长度", p, e.GE)
	}
	if e.LE > 0 && e.LE < p.Bits() {
		return AllowEntry{}, fmt.Errorf("%s 的 le %d 小于前缀长度", p, e.LE)
	}
	if e.GE > 0 && e.LE > 0 && e.GE > e.LE {
		return AllowEntry{}, fmt.Errorf("%s 的 ge %d 大于 le %d", p, e.GE, e.LE)
	}
	return AllowEntry{Prefix: p, GE: e.GE, LE: e.LE}, nil
}

// CompileExportVRFs validates the export list. 253/254/255 are refused because
// KernelTable maps VPN N>0 to kernel table N, and those three tables are the
// kernel's default/main/local: a VPN there would collide with the box itself.
func CompileExportVRFs(raw []byte) ([]uint32, error) {
	var vrfs []uint32
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &vrfs); err != nil {
		return nil, fmt.Errorf("export_vrfs 不是数组: %w", err)
	}
	seen := map[uint32]bool{}
	out := make([]uint32, 0, len(vrfs))
	for _, v := range vrfs {
		if v > 255 {
			return nil, fmt.Errorf("export_vrfs 里的 %d 超出 0..255", v)
		}
		if v == OverlayVRF {
			return nil, fmt.Errorf("export_vrfs 不能包含 %d（overlay 传输域，不是 VPN）", v)
		}
		if v >= 253 {
			return nil, fmt.Errorf("export_vrfs 不能包含 %d：内核表 253/254/255 是 default/main/local，会与盒子自身撞表", v)
		}
		if seen[v] {
			return nil, fmt.Errorf("export_vrfs 里 VPN %d 重复", v)
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ConfigHash covers exactly the two controller-owned values, whitespace aside.
// Both sr-apply (before writing disk) and sr-frr (after applying) compute it
// from the same compacted bytes, so a successful PUT is verifiable rather than
// assumed.
func ConfigHash(exportRaw, importRaw []byte) (string, error) {
	var buf bytes.Buffer
	for i, raw := range [][]byte{exportRaw, importRaw} {
		if i > 0 {
			buf.WriteByte('\n')
		}
		c := bytes.TrimSpace(raw)
		if len(c) == 0 {
			buf.WriteString("[]")
			continue
		}
		if err := json.Compact(&buf, c); err != nil {
			return "", fmt.Errorf("配置不是合法 JSON: %w", err)
		}
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}
