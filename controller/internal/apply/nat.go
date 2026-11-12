// 第 8 段的后半：NAT 出口（nat44_ei）。这一段直接对账 VPP 内存里的状态，和
// VRRP（第 6 段）、策略（第 7 段）一样没有文件可落。
//
// 形状是 2026-11-12 在 CPE4 上一轮一轮量出来的，每一个决定都对应一次实测
// （证据：test/lab/vmlab/exp-nat-bare/raw/）：
//
//	形状                                          数据面 管理面 盒子自身流量
//	A pool = 外口自身地址（.104）                 8/8    全丢   -
//	E 外口普通 outside + 专用 pool（.200）        8/8    全丢   OK
//	D 外口 output-interface + 专用 pool           8/8    存活   1603 次 out of ports
//	G 外口普通 outside + 专用 pool + 身份映射     8/8    存活   OK
//
// 所以本文件的形状就是 G：
//
//  1. 内口：普通 IF_INSIDE（入向 in2out）。
//  2. 外口：普通 IF_OUTSIDE（入向 out2in）。**不用**
//     nat44_ei_add_del_output_interface —— 它会在 ip4-output 弧上再挂一个
//     in2out 节点，把盒子自己发出的包（BFD/封装/管理回程）也拿去翻译，端口
//     分配失败就是 out of ports 全丢（形状 D 的 1603 次实测）。
//  3. pool：配置里显式写的专用地址。每个地址都要在外口网段内，由盒子自己挂成
//     外口的 secondary 地址——同一网段里上游要 ARP 它，不挂就没有回程。
//  4. 外口自己的地址（管理 IP）：加一条 **address-only 身份映射**。这是形状 G
//     的关键：out2in 对"无会话的入向包"一律丢（no translation），外口地址也
//     在被丢之列；identity mapping 让它变成"可反查的合法入向"，管理面因此存活。
//     盒子自己发出的包则根本不被翻译（没有 output 特性），端口不会被耗尽。
//
// 顺序也是实测决定的：所有会让外口开始丢包的步骤（挂特性）排在最后，且身份映射
// 先于特性——中间任何一步失败，外口都还没变成 outside，盒子还是可管理的。
package apply

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"

	"sdwan/controller/internal/vppapi"
)

// natPlane 是本段用到的 VPP 面。测试用替身实现它，真盒子上是 vppapi.Client。
type natPlane interface {
	DumpInterfaces(context.Context) ([]vppapi.SwInterface, error)
	Nat44Enable(context.Context, vppapi.Nat44Params, bool) error
	Nat44InterfaceFeature(context.Context, uint32, byte, bool) error
	Nat44AddressRange(context.Context, netip.Addr, netip.Addr, uint32, bool) error
	Nat44IdentityMapping(context.Context, netip.Addr, uint32, bool) error
	Nat44Interfaces(context.Context) ([]vppapi.Nat44Interface, error)
	Nat44PoolAddresses(context.Context) ([]vppapi.Nat44PoolAddress, error)
	Close() error
}

// 尺寸是 API 里显式给的，不用 CLI 的默认值：CLI 默认值在 256M heap 的盒子上
// 直接 OOM panic（实测两次）。这三个数在 CPE4 上跑通过整套流量测试。
const (
	natUsers        = 4096
	natUserSessions = 256
	natSessions     = 16384
)

// VPP 的 retval：-81 = VALUE_EXIST（已经配过），-170 = 已使能。对账要幂等，
// 这两种"状态已经对了"的答复不能当失败。
const (
	natRetvalExists  int32 = -81
	natRetvalEnabled int32 = -170
	// -169 = 插件本来就没使能。关插件时它不是错误（这一版删掉 NAT 行、盒子
	// 刚重启过，都是这个答复），实测 2026-11-12 CPE4。
	natRetvalNotEnabled int32 = -169
)

// natWant 是一行 NAT 解开之后的样子：盒子只需要这些东西。
type natWant struct {
	row     int // 配置里的行号（从 1 起），报错点名用
	vrf     uint32
	inside  string
	outside string
	pool    []netip.Addr
	clamp   uint16
}

// natWants 把配置行解成"要落地的行"：On 且 pool 非空才算数。半行配置（On 但
// 没 pool）不落地，但要说出来——静默跳过等于让运维以为它在工作。
func natWants(rows []svcNATRow) ([]natWant, []string) {
	var out []natWant
	var notes []string
	for i, r := range rows {
		row := i + 1
		if !r.On {
			continue
		}
		if len(r.Pool) == 0 {
			notes = append(notes, fmt.Sprintf("NAT 第 %d 行（%s→%s）已启用但没有地址池，未下发。", row, r.Inside, r.Outside))
			continue
		}
		w := natWant{row: row, vrf: r.VPN, inside: r.Inside, outside: r.Outside, clamp: r.MSSClamp}
		for _, s := range r.Pool {
			a, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil || !a.Is4() {
				notes = append(notes, fmt.Sprintf("NAT 第 %d 行：地址池里的 %q 不是合法 IPv4，该地址未下发。", row, s))
				continue
			}
			w.pool = append(w.pool, a)
		}
		if len(w.pool) == 0 {
			continue
		}
		out = append(out, w)
	}
	return out, notes
}

// natPrevPool 是上一版里每行 NAT 的（外口 → 地址池）。退场时要靠它知道哪些
// 地址是"我们挂上去的 secondary"，不能凭 VPP 当前的地址表猜。
func natPrevPool(prev Installed) map[string][]netip.Addr {
	out := map[string][]netip.Addr{}
	if strings.TrimSpace(prev.Services) == "" {
		return out
	}
	seg, err := decodeServicesSegment(prev.Services)
	if err != nil {
		return out
	}
	for _, r := range seg.NAT {
		if !r.On || r.Outside == "" {
			continue
		}
		for _, s := range r.Pool {
			if a, perr := netip.ParseAddr(strings.TrimSpace(s)); perr == nil && a.Is4() {
				out[r.Outside] = append(out[r.Outside], a)
			}
		}
	}
	return out
}

// applyNAT 把这一版的 NAT 意图对账到 VPP。返回的 notes 是"没法下发的部分"
// （半行配置、暂不支持的字段），不是错误。
func (a *Applier) applyNAT(ctx context.Context, rows []svcNATRow, prev Installed) ([]string, error) {
	wants, notes := natWants(rows)
	prevPool := natPrevPool(prev)
	if a.DryRun {
		for _, w := range wants {
			a.Logf("would reconcile NAT 第 %d 行：%s(in, vrf %d) → %s(out)，pool %v，外口地址加身份映射",
				w.row, w.inside, w.vrf, w.outside, w.pool)
		}
		return notes, nil
	}
	if len(wants) == 0 && len(prevPool) == 0 {
		return notes, nil // 从来没有过 NAT：不碰 VPP，也不要求 VPP 在
	}
	if a.VPPAPI == "" {
		return notes, errors.New("services 段带 NAT 行，但 apply 没配 VPP api.sock")
	}
	plane, err := a.DialNAT(ctx, a.VPPAPI)
	if err != nil {
		return notes, fmt.Errorf("NAT 面连不上 VPP: %w", err)
	}
	defer plane.Close()

	ifs, err := plane.DumpInterfaces(ctx)
	if err != nil {
		return notes, fmt.Errorf("NAT 对账要读接口表: %w", err)
	}
	byName := make(map[string]vppapi.SwInterface, len(ifs))
	for _, it := range ifs {
		byName[it.Name] = it
	}

	if len(wants) == 0 {
		if err := a.teardownNAT(ctx, plane, prevPool); err != nil {
			return notes, err
		}
		return notes, nil
	}

	// 接口地址用 CLI 读（show interface address），不用 ip_address_dump：
	// 26.06 上那条 dump 的请求体是 sw_if_index + is_ipv6(bool)，本仓库的
	// 编码在 CPE4 上一条 details 都不回（2026-11-12 实测），而 CLI 输出一直
	// 是准的，且 addIfaceAddr 本来就走 CLI，同一个来源不会出现"读的和写的
	// 不是一个地方"。
	for i := range wants {
		for _, name := range []string{wants[i].inside, wants[i].outside} {
			it := byName[name]
			if it.IPv4 != nil {
				continue
			}
			list, err := a.ifaceAddrs(ctx, name)
			if err != nil {
				return notes, fmt.Errorf("NAT 对账读 %s 的地址: %w", name, err)
			}
			it.IPv4 = list
			byName[name] = it
		}
	}

	// 1) 接口解出来（名字 → sw_if_index / 网段）。外口还必须有可用的 IPv4 网段：
	//    pool 地址要靠它挂 secondary。
	for i := range wants {
		w := &wants[i]
		if _, ok := byName[w.inside]; !ok {
			return notes, fmt.Errorf("NAT 第 %d 行：内口 %s 不在 VPP 里", w.row, w.inside)
		}
		out, ok := byName[w.outside]
		if !ok {
			return notes, fmt.Errorf("NAT 第 %d 行：外口 %s 不在 VPP 里", w.row, w.outside)
		}
		if w.inside == w.outside {
			return notes, fmt.Errorf("NAT 第 %d 行：内外口是同一个接口 %s", w.row, w.inside)
		}
		if len(out.IPv4) == 0 {
			return notes, fmt.Errorf("NAT 第 %d 行：外口 %s 没有 IPv4 地址，地址池无处挂靠", w.row, w.outside)
		}
	}

	// 2) 地址池地址挂到外口上（同一网段里上游要 ARP 它）。只挂外口网段内的地址：
	//    配置模型要求 pool 在外口网络里，越界的要么是配错，要么靠上游静态路由，
	//    两种都不该由盒子自己替它补地址。
	for _, w := range wants {
		out := byName[w.outside]
		for _, p := range w.pool {
			pfx, ok := prefixContaining(out, p)
			if !ok {
				return notes, fmt.Errorf("NAT 第 %d 行：地址池地址 %s 不在外口 %s 的网段内（外口 %v）",
					w.row, p, w.outside, prefixesOf(out))
			}
			if hasAddr(out, p) {
				continue
			}
			with := netip.PrefixFrom(p, pfx.Bits())
			if err := a.addIfaceAddr(ctx, w.outside, with); err != nil {
				return notes, fmt.Errorf("NAT 第 %d 行：地址池地址 %s 挂到 %s 失败: %w", w.row, p, w.outside, err)
			}
			a.Logf("NAT 第 %d 行：%s 挂到 %s 作为地址池地址（应答上游 ARP）", w.row, p, w.outside)
			out.IPv4 = append(out.IPv4, vppPrefix(with))
			byName[w.outside] = out
		}
	}

	// 3) 使能。nat44_ei 每对 (inside vrf, outside vrf) 一个实例；同一个 VRF 上
	//    多行（多个内口）共用它。
	params := func(vrf uint32) vppapi.Nat44Params {
		return vppapi.Nat44Params{
			InsideVRF: vrf, OutsideVRF: 0,
			Users: natUsers, UserSessions: natUserSessions, Sessions: natSessions,
		}
	}
	enabled := map[uint32]bool{}
	for _, w := range wants {
		if enabled[w.vrf] {
			continue
		}
		if err := plane.Nat44Enable(ctx, params(w.vrf), true); err != nil && !natAlready(err) {
			return notes, fmt.Errorf("NAT 使能失败（inside vrf %d）: %w", w.vrf, err)
		}
		enabled[w.vrf] = true
	}

	// 4) 外口自己地址的身份映射（形状 G 的核心）。必须在挂特性之前：外口一挂上
	//    out2in，没有身份映射的入向包就会被 no translation 丢掉，管理面当场失联。
	ident := map[netip.Addr]bool{}
	for _, w := range wants {
		out := byName[w.outside]
		for _, vp := range out.IPv4 {
			p, ok := netipPrefix(vp)
			if !ok {
				continue
			}
			addr := p.Addr()
			if containsAddr(w.pool, addr) {
				continue // 已经是池地址：它本来就能反查，不能再做身份映射
			}
			if ident[addr] {
				continue
			}
			if err := plane.Nat44IdentityMapping(ctx, addr, 0, true); err != nil && !natAlready(err) {
				return notes, fmt.Errorf("NAT 身份映射 %s 失败: %w", addr, err)
			}
			ident[addr] = true
			a.Logf("NAT：外口地址 %s 的身份映射就位（管理面/盒子自身流量靠它存活）", addr)
		}
	}

	// 5) 地址池范围。
	for _, w := range wants {
		for _, p := range w.pool {
			if err := plane.Nat44AddressRange(ctx, p, p, 0, true); err != nil && !natAlready(err) {
				return notes, fmt.Errorf("NAT 第 %d 行：地址池 %s 下发失败: %w", w.row, p, err)
			}
		}
	}

	// 6) 清掉上一版多出来、这一版不要的池地址和特性。
	if err := a.pruneNAT(ctx, plane, wants, byName, prevPool); err != nil {
		return notes, err
	}

	// 7) 最后才挂接口特性：从这一刻起外口开始按 NAT 语义丢无会话入向包，前面
	//    所有前置条件（池地址、身份映射、范围）都已就位。
	for _, w := range wants {
		in := byName[w.inside]
		out := byName[w.outside]
		if err := plane.Nat44InterfaceFeature(ctx, in.SWIfIndex, vppapi.Nat44FlagIfInside, true); err != nil && !natAlready(err) {
			return notes, fmt.Errorf("NAT 第 %d 行：内口 %s 挂 inside 失败: %w", w.row, w.inside, err)
		}
		if err := plane.Nat44InterfaceFeature(ctx, out.SWIfIndex, vppapi.Nat44FlagIfOutside, true); err != nil && !natAlready(err) {
			return notes, fmt.Errorf("NAT 第 %d 行：外口 %s 挂 outside 失败: %w", w.row, w.outside, err)
		}
	}

	// 8) 回读校验：写下去不算数，VPP 自己 dump 出来的才算。
	if err := verifyNAT(ctx, plane, wants, byName); err != nil {
		return notes, err
	}

	for _, w := range wants {
		if w.clamp != 0 {
			notes = append(notes, fmt.Sprintf("NAT 第 %d 行：MSS 钳制 %d 暂未下发（这一版没有做对外的 TCP MSS clamp）。", w.row, w.clamp))
		}
	}
	return notes, nil
}

// pruneNAT 把上一版有、这一版不要的池地址/接口特性/secondary 地址摘掉。
func (a *Applier) pruneNAT(ctx context.Context, plane natPlane, wants []natWant, byName map[string]vppapi.SwInterface, prevPool map[string][]netip.Addr) error {
	wantPool := map[netip.Addr]bool{}
	wantIn := map[uint32]bool{}
	wantOut := map[uint32]bool{}
	for _, w := range wants {
		for _, p := range w.pool {
			wantPool[p] = true
		}
		wantIn[byName[w.inside].SWIfIndex] = true
		wantOut[byName[w.outside].SWIfIndex] = true
	}
	// 先摘特性，再删池地址：反过来会出现"池里有地址、特性已经不在"的中间态。
	cur, err := plane.Nat44Interfaces(ctx)
	if err != nil {
		return fmt.Errorf("NAT 对账读接口特性: %w", err)
	}
	for _, r := range cur {
		if r.Inside && !wantIn[r.SWIfIndex] {
			if err := plane.Nat44InterfaceFeature(ctx, r.SWIfIndex, vppapi.Nat44FlagIfInside, false); err != nil && !natAlready(err) {
				return fmt.Errorf("NAT：摘 inside 特性 (sw_if_index %d) 失败: %w", r.SWIfIndex, err)
			}
			a.Logf("NAT：摘掉不再使用的 inside 特性 (sw_if_index %d)", r.SWIfIndex)
		}
		if r.Outside && !wantOut[r.SWIfIndex] {
			if err := plane.Nat44InterfaceFeature(ctx, r.SWIfIndex, vppapi.Nat44FlagIfOutside, false); err != nil && !natAlready(err) {
				return fmt.Errorf("NAT：摘 outside 特性 (sw_if_index %d) 失败: %w", r.SWIfIndex, err)
			}
			a.Logf("NAT：摘掉不再使用的 outside 特性 (sw_if_index %d)", r.SWIfIndex)
		}
	}
	addrs, err := plane.Nat44PoolAddresses(ctx)
	if err != nil {
		return fmt.Errorf("NAT 对账读地址池: %w", err)
	}
	for _, p := range addrs {
		if wantPool[p.Addr] {
			continue
		}
		if err := plane.Nat44AddressRange(ctx, p.Addr, p.Addr, p.VRF, false); err != nil && !natAlready(err) {
			return fmt.Errorf("NAT：删掉不再使用的池地址 %s 失败: %w", p.Addr, err)
		}
		a.Logf("NAT：删掉不再使用的池地址 %s", p.Addr)
		// 上一版是我们把这个地址挂成 secondary 的，一并摘掉（不是池地址了，
		// 留在口上只会让上游 ARP 到一个没人用的地址）。
		for iface, list := range prevPool {
			for _, q := range list {
				if q == p.Addr {
					_ = a.delIfaceAddr(ctx, iface, q)
				}
			}
		}
	}
	return nil
}

// teardownNAT 是"这一版没有 NAT 行、上一版有"的退场：特性 → 池地址 → 身份映射
// → 关插件 → 把盒子自己挂过的 secondary 地址摘掉。
func (a *Applier) teardownNAT(ctx context.Context, plane natPlane, prevPool map[string][]netip.Addr) error {
	cur, err := plane.Nat44Interfaces(ctx)
	if err != nil {
		return fmt.Errorf("NAT 退场读接口特性: %w", err)
	}
	for _, r := range cur {
		if r.Inside {
			_ = plane.Nat44InterfaceFeature(ctx, r.SWIfIndex, vppapi.Nat44FlagIfInside, false)
		}
		if r.Outside {
			_ = plane.Nat44InterfaceFeature(ctx, r.SWIfIndex, vppapi.Nat44FlagIfOutside, false)
		}
	}
	addrs, err := plane.Nat44PoolAddresses(ctx)
	if err != nil {
		return fmt.Errorf("NAT 退场读地址池: %w", err)
	}
	for _, p := range addrs {
		if err := plane.Nat44AddressRange(ctx, p.Addr, p.Addr, p.VRF, false); err != nil && !natAlready(err) {
			return fmt.Errorf("NAT 退场：删池地址 %s 失败: %w", p.Addr, err)
		}
	}
	for iface, list := range prevPool {
		for _, p := range list {
			_ = plane.Nat44IdentityMapping(ctx, p, 0, false)
			_ = a.delIfaceAddr(ctx, iface, p)
		}
	}
	off := vppapi.Nat44Params{Users: natUsers, UserSessions: natUserSessions, Sessions: natSessions}
	if err := plane.Nat44Enable(ctx, off, false); err != nil && !natAlready(err) && !natNotEnabled(err) {
		return fmt.Errorf("NAT 关插件失败: %w", err)
	}
	a.Logf("NAT：这一版没有 NAT 行，已把上一版的 NAT 状态全部撤掉")
	return nil
}

// verifyNAT 回读：接口特性两侧都对、池地址都在。VPP 对挂错口是静默的，
// 不回读就等于没配。
func verifyNAT(ctx context.Context, plane natPlane, wants []natWant, byName map[string]vppapi.SwInterface) error {
	rows, err := plane.Nat44Interfaces(ctx)
	if err != nil {
		return fmt.Errorf("NAT 回读接口特性: %w", err)
	}
	got := map[uint32]vppapi.Nat44Interface{}
	for _, r := range rows {
		got[r.SWIfIndex] = r
	}
	for _, w := range wants {
		in := byName[w.inside].SWIfIndex
		out := byName[w.outside].SWIfIndex
		if r := got[in]; !r.Inside {
			return fmt.Errorf("NAT 第 %d 行：回读发现内口 %s 上没有 inside 特性", w.row, w.inside)
		}
		if r := got[out]; !r.Outside {
			return fmt.Errorf("NAT 第 %d 行：回读发现外口 %s 上没有 outside 特性", w.row, w.outside)
		}
	}
	pool, err := plane.Nat44PoolAddresses(ctx)
	if err != nil {
		return fmt.Errorf("NAT 回读地址池: %w", err)
	}
	have := map[netip.Addr]bool{}
	for _, p := range pool {
		have[p.Addr] = true
	}
	for _, w := range wants {
		for _, p := range w.pool {
			if !have[p] {
				return fmt.Errorf("NAT 第 %d 行：回读发现地址池里没有 %s", w.row, p)
			}
		}
	}
	return nil
}

// addIfaceAddr / delIfaceAddr 走 CLI：vppapi 这一层没有 sw_interface_add_del_address。
func (a *Applier) addIfaceAddr(ctx context.Context, iface string, p netip.Prefix) error {
	return a.vppDo(ctx, "set", "interface", "ip", "address", iface, p.String())
}

// reIfaceL3 匹配 `show interface address <if>` 里的地址行。输出形如
//
//	host-enp0s9 (up):
//	  L3 192.168.1.104/24
//	  L3 192.168.1.200/24
//
// 只认 L3 行：`show interface address` 还会打印 counters 与 table-id，那些行
// 里出现的数字不是地址。
var reIfaceL3 = regexp.MustCompile(`L3\s+([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+)`)

// ifaceAddrs 读一张接口上的 IPv4 地址。默认实现走 CLI（见 nat.go 头部关于
// ip_address_dump 的实测记录）；IfaceAddrs 是测试缝。
func (a *Applier) ifaceAddrs(ctx context.Context, iface string) ([]vppapi.Prefix, error) {
	if a.IfaceAddrs != nil {
		return a.IfaceAddrs(ctx, iface)
	}
	if a.DryRun {
		return nil, nil
	}
	out, err := a.vppShow(ctx, "show interface address "+iface)
	if err != nil {
		return nil, err
	}
	var list []vppapi.Prefix
	for _, m := range reIfaceL3.FindAllStringSubmatch(out, -1) {
		p, perr := netip.ParsePrefix(m[1])
		if perr != nil {
			continue
		}
		list = append(list, vppPrefix(p))
	}
	return list, nil
}

func (a *Applier) delIfaceAddr(ctx context.Context, iface string, addr netip.Addr) error {
	out, err := a.vppShow(ctx, "show interface address "+iface)
	if err != nil {
		return err
	}
	pfx, ok := prefixOfAddrInShow(out, addr)
	if !ok {
		return nil // 已经不在口上了
	}
	return a.vppDo(ctx, "set", "interface", "ip", "address", "del", iface, pfx.String())
}

// vppDo 跑一条改状态的 vppctl 命令。vppctl 出错也退出 0，只能看输出。
func (a *Applier) vppDo(ctx context.Context, args ...string) error {
	out, err := a.Exec.Run(ctx, a.VPPCtl, args...)
	if err != nil {
		return fmt.Errorf("vppctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	if msg := vppError(out); msg != "" {
		return fmt.Errorf("vppctl %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// netipPrefix 把 wire 上的 Prefix 转成 netip.Prefix。
func netipPrefix(p vppapi.Prefix) (netip.Prefix, bool) {
	ip, ok := netip.AddrFromSlice(p.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		return netip.Prefix{}, false
	}
	// 不 mask：调用方要的是"这张口上的哪个地址 + 多长掩码"。mask 掉会把
	// 192.168.1.104/24 变成 192.168.1.0/24，身份映射就挂到错误的地址上了。
	return netip.PrefixFrom(ip, int(p.Len)), true
}

// vppPrefix 是 netipPrefix 的逆向，只在本地接口表里补刚挂上去的地址时用。
func vppPrefix(p netip.Prefix) vppapi.Prefix {
	return vppapi.Prefix{IP: net.IP(p.Addr().AsSlice()), Len: uint8(p.Bits())}
}

// prefixContaining 找外口上包含 addr 的网段（pool 地址必须落在里面）。
func prefixContaining(it vppapi.SwInterface, addr netip.Addr) (netip.Prefix, bool) {
	for _, vp := range it.IPv4 {
		p, ok := netipPrefix(vp)
		if ok && p.Contains(addr) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

func hasAddr(it vppapi.SwInterface, addr netip.Addr) bool {
	for _, vp := range it.IPv4 {
		if p, ok := netipPrefix(vp); ok && p.Addr() == addr {
			return true
		}
	}
	return false
}

func prefixesOf(it vppapi.SwInterface) []string {
	out := make([]string, 0, len(it.IPv4))
	for _, vp := range it.IPv4 {
		if p, ok := netipPrefix(vp); ok {
			out = append(out, p.String())
		}
	}
	return out
}

func containsAddr(list []netip.Addr, a netip.Addr) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

// natNotEnabled 判断"本来就关着"：关插件时的良性答复。
func natNotEnabled(err error) bool {
	var apiErr vppapi.APIError
	return errors.As(err, &apiErr) && apiErr.Retval == natRetvalNotEnabled
}

// natAlready 判断"状态已经是对的"：VALUE_EXIST / 已使能。
func natAlready(err error) bool {
	var apiErr vppapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retval == natRetvalExists || apiErr.Retval == natRetvalEnabled
	}
	return false
}

// prefixOfAddrInShow 从 `show interface address` 的输出里取出某个地址的写法。
func prefixOfAddrInShow(out string, addr netip.Addr) (netip.Prefix, bool) {
	for _, f := range strings.Fields(out) {
		if !strings.Contains(f, "/") {
			continue
		}
		if p, err := netip.ParsePrefix(strings.TrimRight(f, ",")); err == nil && p.Addr() == addr {
			return p, true
		}
	}
	return netip.Prefix{}, false
}
