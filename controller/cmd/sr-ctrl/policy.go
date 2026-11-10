package main

// SD-WAN 静态策略（配置包第 7 段）：把"哪类流"钉到"哪个颜色"。
//
// 落地方式不是"指定下一跳"，而是"指定查哪张表"：颜色 c 在 VPN v 里对应一张
// 独立的 VPP FIB 表（表号 = c*1000 + v，见 internal/fib.ColorTable），那张表里的
// 路径由 sr-fib 按隧道/BFD 状态自己收敛。于是链路抖动不需要重写策略，策略也不必
// 知道隧道现在通不通（owner 2026-11-10）。
//
// 三个决定，都写在数据前面：
//
//  1. **入口口不在这里选**。一行策略挂到"这个 VPN 的所有客户口"上（owner
//     2026-11-10 拍）。理由：颜色策略按 VPN 做，而一个 VPN 的入口口本来就是那几张
//     口；让每行再挑一遍口，会造出一个"同一 VPN 内一半入口有策略、一半没有"的状态，
//     这种状态没人预期得到。所以 attach 是**渲染时从基础配置派生**的，不是用户输入。
//
//  2. **行号就是匹配顺序**。VPP 里 ABF 的挂点优先级 = 1000 + 行号，小的先匹配；
//     ACL 的 tag（sr-pol-<行号>）也带行号。所以页面上第几行、下发后第几个匹配，
//     是同一件事，不存在"顺序"这个需要额外维护的字段。代价是插一行会让后面的行号
//     整体后移，那一批 ACL/policy 会被重建一次 —— 每次重建都是 delete+add，因为
//     同 id 换 ACL 的 add 会被 VPP 拒成 INVALID_VALUE(-7)（实测 2026-11-10）。
//
//  3. **兜底是每 VRF 每颜色一个开关**（owner 2026-11-10 拍），不是每行一个。
//     勾上 = "这个颜色查不到就退到颜色 0"，落地方式是颜色表里的一条
//     0.0.0.0/0 → 主表 的 deag 路由。它不是这条段的执行内容：装它的是 sr-fib
//     （它是颜色表的 owner，见 internal/fib.PlanFrom 的 fallbacks 参数），这一段
//     只把这份清单原样带上，sr-agent 从配置包里把它派生给 sr-fib。同一个开关只有
//     一个来源（这一页），两个执行者各取自己那一半。
//
// 这一段和 VRRP（第 6 段）一样是**对账指令**，不是一个盒子要保存的文件：ACL 与
// ABF 只存在于 VPP 内存里。空数组必须照发 —— "一条策略都没有" 是一条指令
// （把现存的清掉），"没有这一段" 是另一件事（老配置包，什么都不要动）。

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/route"
	"sdwan/controller/internal/underlay"
)

const (
	// policyMaxRows 是行号上限：行号要进 ACL tag 与 ABF policy_id，盒子上按
	// 1000 + 行号 折算，999 之后会和下一个用途撞上。
	policyMaxRows = 999

	// 协议只用这四个值：盒子的 aclRulesFor 也只认这四个（0=任意 / 1=icmp /
	// 6=tcp / 17=udp）。端口只在 tcp/udp 上有意义 —— icmp 的那两个字段是
	// type/code，本系统不按它匹配。
	policyProtoAny  uint8 = 0
	policyProtoICMP uint8 = 1
	policyProtoTCP  uint8 = 6
	policyProtoUDP  uint8 = 17

	// policyModeDynamic 是动态行的 Mode 值。空串是静态行 —— 老草稿里没有这个
	// 字段，语义必须保持"钉死颜色"不变。
	policyModeDynamic = "dynamic"
)

// PolicyRow 是一行策略：五元组（可留空 = 任意）→ 颜色。
//
// ID 由控制器在保存时按数组顺序重排成 1..N，不需要页面填。存盘里带 ID 是为了
// 让"存下来的那份"和"发给设备的那份"逐字一致，好让"发布改了没有"能靠字节比较
// 判定（和 VRRP 段同一个理由）。
//
// 两种模式（owner 2026-11-10 拍）：
//
//   - Mode == ""（静态）：这一行钉死在 Color 上。落地是 ABF → 颜色表。
//   - Mode == "dynamic"（动态）：这一行交给盒子上的 sla.Selector 现场挑。它
//     盯着**到 Peer 这台 CPE** 的各条颜色隧道的探针指标（延迟/抖动/丢包，
//     插件 sr_wg_probe），在 Colors 候选里挑满足 SLA 的最好一条，把选中结果
//     写进这一行自己的**选路表**（fib.SelectorTable(ID)，表号控制器渲染成
//     selector_table）。落地是 ABF → 选路表 → 颜色表。所以动态行上 Color 没有
//     语义，normalize 会把它清成 0；SLA 三项全 0 也是合法的 —— 含义是"不设
//     门槛，永远挑延迟最低的那条"。
type PolicyRow struct {
	ID      int    `json:"id"`
	VPN     uint32 `json:"vpn"`
	Color   uint8  `json:"color,omitempty"`
	Src     string `json:"src,omitempty"` // 空 = 任意
	Dst     string `json:"dst,omitempty"` // 空 = 任意
	Proto   uint8  `json:"proto,omitempty"`
	SrcPort string `json:"src_port,omitempty"` // "80" 或 "1000-2000"，空 = 任意
	DstPort string `json:"dst_port,omitempty"`

	Mode   string     `json:"mode,omitempty"`   // "" = 静态；"dynamic" = 动态
	Peer   uint16     `json:"peer,omitempty"`   // dynamic：SLA 量的是到这台 CPE 的隧道
	Colors []uint8    `json:"colors,omitempty"` // dynamic：候选颜色；空 = 该对端全部颜色
	SLA    *PolicySLA `json:"sla,omitempty"`    // dynamic：门槛；nil / 全 0 = 不设门槛
}

// PolicySLA 是动态行的门槛，三项都是"低于/不多于"才算达标；0 表示这一项不
// 约束。不达标不丢流（owner 2026-11-10 拍）：SLA 是偏好不是准入，全部候选都
// 不达标时选最接近的一条（丢包最少 → 延迟最低），只有全部 100% 丢包才保持不动。
type PolicySLA struct {
	LatencyMS    float64 `json:"latency_ms,omitempty"`
	JitterMS     float64 `json:"jitter_ms,omitempty"`
	LossPermille uint32  `json:"loss_permille,omitempty"`
}

// PolicyFallback 是"允许 fallback 到颜色 0"的一个开关：粒度是每 VRF 每颜色。
type PolicyFallback struct {
	VPN   uint32 `json:"vpn"`
	Color uint8  `json:"color"`
}

// PolicySet 是一台设备的静态策略。它和 LeakSet 一样挂在 DeviceConfig 上、
// 不在 BasicConfig 里：基础配置页保存的是整份 BasicConfig，策略行要是存在那里，
// 每次从那一页保存都会把它们抹掉。
type PolicySet struct {
	Rows     []PolicyRow      `json:"rows"`
	Fallback []PolicyFallback `json:"fallback"`
}

// policyArtifactRow 是第 7 段里的一行。字段名和 internal/apply.policyRowDoc
// 逐字对齐（两边各自定义、靠 JSON 对齐，这是配置包这条边界的既有做法）。
// Attach 是派生的：这个 VPN 的全部客户口，按名字排序（排序是为了同一份草稿渲染
// 出同样的字节，否则"发布没改动"就不可判）。
type policyArtifactRow struct {
	ID      int      `json:"id"`
	VPN     uint32   `json:"vpn"`
	Color   uint8    `json:"color,omitempty"`
	Src     string   `json:"src,omitempty"`
	Dst     string   `json:"dst,omitempty"`
	Proto   uint8    `json:"proto,omitempty"`
	SrcPort string   `json:"src_port,omitempty"`
	DstPort string   `json:"dst_port,omitempty"`
	Attach  []string `json:"attach"`

	// 动态行专属。静态行这些键不出现（omitempty），一份全静态的策略渲染出来和
	// 老版本逐字节一致 —— "发布没改动"要靠字节比较判定。
	Mode          string     `json:"mode,omitempty"`
	Peer          uint16     `json:"peer,omitempty"`
	Colors        []uint8    `json:"colors,omitempty"`
	SLA           *PolicySLA `json:"sla,omitempty"`
	SelectorTable uint32     `json:"selector_table,omitempty"`
}

type policyArtifactFB struct {
	VPN   uint32 `json:"vpn"`
	Color uint8  `json:"color"`
}

type policyArtifactDoc struct {
	Rows     []policyArtifactRow `json:"rows"`
	Fallback []policyArtifactFB  `json:"fallback"`
}

func policyProtoValid(p uint8) bool {
	switch p {
	case policyProtoAny, policyProtoICMP, policyProtoTCP, policyProtoUDP:
		return true
	}
	return false
}

func policyProtoName(p uint8) string {
	switch p {
	case policyProtoAny:
		return "任意"
	case policyProtoICMP:
		return "ICMP"
	case policyProtoTCP:
		return "TCP"
	case policyProtoUDP:
		return "UDP"
	}
	return fmt.Sprintf("协议 %d", p)
}

// policyRowLabel 是检查里指名道姓用的行标签：静态行报颜色，动态行报对端 ——
// 动态行上颜色没有语义，报"颜色 0"只会把人引去查一个不存在的配置项。
func policyRowLabel(r PolicyRow, i int) string {
	if r.Mode == policyModeDynamic {
		return fmt.Sprintf("第 %d 行（VPN %d → 动态·对端 %d）", i+1, r.VPN, r.Peer)
	}
	return fmt.Sprintf("第 %d 行（VPN %d → 颜色 %d）", i+1, r.VPN, r.Color)
}

// policyColorList 把颜色清单说成人话："2, 3"。
func policyColorList(cs []uint8) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, strconv.Itoa(int(c)))
	}
	return strings.Join(parts, ", ")
}

// normalizePolicySet 整理一份策略集。它**不**做校验（校验是 policyChecksLocked
// 的事，要报给人看），只做三件让"同一份内容渲染出同一份字节"成立的事：
//
//   - 行号按数组顺序重排成 1..N。行号即匹配顺序，所以数组顺序就是唯一顺序，
//     不需要再排一次序（这一点和 VRRP 行相反：VR 行没有顺序语义才要排序）。
//   - 兜底清单按 (VPN, 颜色) 排序去重（重复勾一遍按一遍算）。
//   - 空切片变成非 nil，好让 JSON 里出现 "rows": [] 而不是 "rows": null。
//
// 非 tcp/udp 上残留的端口字符串**不**在这里清掉：清掉会让"填了端口却说协议是
// ICMP"这个错误悄悄消失，校验就看不见它了。
//
// 动态行在这里做两件归一化：Color 清成 0（动态行没有固定颜色，留着一个残值会让
// "改了没有"的比较误报）；候选颜色排序去重。Mode 不在这里纠正 —— 拼错的 mode
// 要让校验报出来，而不是被悄悄当成静态。
func normalizePolicySet(set *PolicySet) {
	for i := range set.Rows {
		r := &set.Rows[i]
		r.ID = i + 1
		r.Src = strings.TrimSpace(r.Src)
		r.Dst = strings.TrimSpace(r.Dst)
		r.SrcPort = strings.TrimSpace(r.SrcPort)
		r.DstPort = strings.TrimSpace(r.DstPort)
		if r.Mode == policyModeDynamic {
			r.Color = 0
			seenC := map[uint8]bool{}
			out := make([]uint8, 0, len(r.Colors))
			for _, c := range r.Colors {
				if seenC[c] {
					continue
				}
				seenC[c] = true
				out = append(out, c)
			}
			sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
			r.Colors = out
		} else {
			r.Colors = nil
			r.SLA = nil
		}
	}
	if set.Rows == nil {
		set.Rows = []PolicyRow{}
	}
	seen := map[[2]uint32]bool{}
	out := make([]PolicyFallback, 0, len(set.Fallback))
	for _, f := range set.Fallback {
		k := [2]uint32{f.VPN, uint32(f.Color)}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].VPN != out[j].VPN {
			return out[i].VPN < out[j].VPN
		}
		return out[i].Color < out[j].Color
	})
	set.Fallback = out
}

func policySetCopy(in PolicySet) PolicySet {
	out := PolicySet{
		Rows:     make([]PolicyRow, len(in.Rows)),
		Fallback: append([]PolicyFallback(nil), in.Fallback...),
	}
	// 行要深拷：Colors 与 SLA 是引用类型，浅拷会让两份 PolicySet 共享底层数组，
	// 改一份等于改两份，"改了没有"的比较就再也测不出差异。
	for i, r := range in.Rows {
		out.Rows[i] = r
		if r.Colors != nil {
			out.Rows[i].Colors = append([]uint8(nil), r.Colors...)
		}
		if r.SLA != nil {
			s := *r.SLA
			out.Rows[i].SLA = &s
		}
	}
	if out.Fallback == nil {
		out.Fallback = []PolicyFallback{}
	}
	return out
}

// policyRowEqual 比一行的内容。SLA 是指针，不能用 == ：nil 和"全 0 的 SLA"是
// 两种写法（"没设门槛"和"门槛全 0"），但语义相同 —— 按"展开后相等"比，两种
// 写法之间来回改不会触发重下发。
func policyRowEqual(a, b PolicyRow) bool {
	if a.ID != b.ID || a.VPN != b.VPN || a.Color != b.Color ||
		a.Src != b.Src || a.Dst != b.Dst || a.Proto != b.Proto ||
		a.SrcPort != b.SrcPort || a.DstPort != b.DstPort ||
		a.Mode != b.Mode || a.Peer != b.Peer {
		return false
	}
	if len(a.Colors) != len(b.Colors) {
		return false
	}
	for i := range a.Colors {
		if a.Colors[i] != b.Colors[i] {
			return false
		}
	}
	switch {
	case (a.SLA == nil) != (b.SLA == nil):
		// 一个 nil 一个非 nil：只有非 nil 的那个全 0 才算相等。
		if a.SLA == nil {
			return *b.SLA == PolicySLA{}
		}
		return *a.SLA == PolicySLA{}
	case a.SLA != nil:
		return *a.SLA == *b.SLA
	}
	return true
}

// policyAttachIfaces 派生一行的入口口：这个 VPN 的全部客户口。
//
// 名字排序而不是按基础配置里的顺序：attach 是这份 artifact 的一部分，而基础配置
// 里客户口的顺序没有语义（normalizeBasicConfig 不排它）。同一组口排出不同字节，
// 会让一次"什么都没改"的保存看起来像改了策略，进而把整段策略无谓地重下发一遍。
func policyAttachIfaces(c *BasicConfig, vpn uint32) []string {
	out := make([]string, 0, 4)
	if c == nil {
		return out
	}
	for _, it := range c.Ifaces {
		if it.VPN == vpn && it.VPP != "" {
			out = append(out, it.VPP)
		}
	}
	sort.Strings(out)
	return out
}

// renderPolicyArtifact 渲染第 7 段。空策略集也要渲染出 rows: []（见文件头）。
func renderPolicyArtifact(c *BasicConfig, set PolicySet) string {
	set = policySetCopy(set)
	normalizePolicySet(&set)
	doc := policyArtifactDoc{
		Rows:     make([]policyArtifactRow, 0, len(set.Rows)),
		Fallback: make([]policyArtifactFB, 0, len(set.Fallback)),
	}
	for _, r := range set.Rows {
		row := policyArtifactRow{
			ID:      r.ID,
			VPN:     r.VPN,
			Color:   r.Color,
			Src:     r.Src,
			Dst:     r.Dst,
			Proto:   r.Proto,
			SrcPort: r.SrcPort,
			DstPort: r.DstPort,
			Attach:  policyAttachIfaces(c, r.VPN),
		}
		if r.Mode == policyModeDynamic {
			// 选路表号在这里定死并写进 artifact：盒子不自己算（见
			// fib.SelectorTable 的注释——表号两边各算一次是静默 bug 的形状）。
			st, err := fib.SelectorTable(r.ID)
			if err != nil {
				// 行号在 1..999 之外不可能到这里（normalize 重排 + 校验挡住）；
				// 真发生了就渲染不出这一行，宁可 panic 也不发一份会静默错路的配置。
				panic(fmt.Sprintf("renderPolicyArtifact: %v", err))
			}
			row.Mode = r.Mode
			row.Peer = r.Peer
			row.Colors = r.Colors
			row.SLA = r.SLA
			row.SelectorTable = st
			row.Color = 0 // 动态行没有固定颜色（normalize 已清；这里再钉一次）
		}
		doc.Rows = append(doc.Rows, row)
	}
	for _, f := range set.Fallback {
		doc.Fallback = append(doc.Fallback, policyArtifactFB{VPN: f.VPN, Color: f.Color})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// 只有标量和字符串切片；不可达。
		return ""
	}
	return string(b) + "\n"
}

// policyMatch 是一行策略解开之后的可比较形式，只给"被前面的行完全覆盖"这条
// 检查用。解析不了的行照样进列表（后面的行还是要和它比），但标记 ok=false，
// 比较时跳过 —— 那一行本身已经报了格式错误。
type policyMatch struct {
	idx   int // 0 基下标
	ok    bool
	src   netip.Prefix
	dst   netip.Prefix
	proto uint8
	sport policySpan
	dport policySpan
}

// policySpan 是一个端口区间。anyPorts 表示这一维不参与匹配（空 = 任意）。
type policySpan struct {
	anyPorts bool
	lo, hi   uint16
}

func (s policySpan) contains(o policySpan) bool {
	if s.anyPorts {
		return true
	}
	if o.anyPorts {
		return false
	}
	return s.lo <= o.lo && s.hi >= o.hi
}

// covers 判"前面这一行比后面那行宽或相等"，也就是后面那行永远轮不到。
func (m policyMatch) covers(o policyMatch) bool {
	if !m.ok || !o.ok {
		return false
	}
	if m.proto != policyProtoAny && m.proto != o.proto {
		return false
	}
	if m.src.Bits() > o.src.Bits() || !m.src.Contains(o.src.Addr()) {
		return false
	}
	if m.dst.Bits() > o.dst.Bits() || !m.dst.Contains(o.dst.Addr()) {
		return false
	}
	return m.sport.contains(o.sport) && m.dport.contains(o.dport)
}

func policyMatchOf(c *BasicConfig, idx int, r PolicyRow) policyMatch {
	m := policyMatch{idx: idx, proto: r.Proto}
	src, err1 := policyPrefix(r.Src)
	dst, err2 := policyPrefix(r.Dst)
	sport, err3 := policySpanOf(r.SrcPort)
	dport, err4 := policySpanOf(r.DstPort)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || !policyProtoValid(r.Proto) {
		return m
	}
	m.ok = true
	m.src, m.dst, m.sport, m.dport = src, dst, sport, dport
	return m
}

// policyPrefix 与盒子上的 prefixOrAny 同一套规则：空或 "any" 是 0.0.0.0/0。
func policyPrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return netip.MustParsePrefix("0.0.0.0/0"), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("只支持 IPv4")
	}
	return p.Masked(), nil
}

// policyCheckPrefix 是校验专用的前缀解析：它**不**顺手 mask，因为"用户写的是
// 192.0.2.5/24"本身就是要报的那个错。第二个返回值是"写的是规范网段吗"。
func policyCheckPrefix(s string) (netip.Prefix, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return netip.MustParsePrefix("0.0.0.0/0"), true, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, false, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, false, fmt.Errorf("只支持 IPv4")
	}
	return p, p.Masked() == p, nil
}

func policySpanOf(s string) (policySpan, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return policySpan{anyPorts: true}, nil
	}
	lo, hi, cut := strings.Cut(s, "-")
	first, err := parsePolicyPort(lo)
	if err != nil {
		return policySpan{}, err
	}
	last := first
	if cut {
		if last, err = parsePolicyPort(hi); err != nil {
			return policySpan{}, err
		}
	}
	if last < first {
		return policySpan{}, fmt.Errorf("区间起止颠倒（%d-%d）", first, last)
	}
	return policySpan{lo: first, hi: last}, nil
}

func parsePolicyPort(s string) (uint16, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空端口号")
	}
	var n uint32
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("不是端口号")
		}
		n = n*10 + uint32(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("超出 65535")
		}
	}
	return uint16(n), nil
}

// nodeTunnelColorsLocked 返回这台设备**配置上**每种颜色有几条隧道，以及租户到底
// 有没有 underlay 配置（没有就返回 ok=false，策略的颜色检查就整体跳过 —— 一台还
// 没建隧道的设备，"颜色没有隧道"是废话，不是错误）。
//
// 只看 underlay 的配置、不看下发结果：一条隧道因为对端缺密钥被跳过，是 underlay
// 那一页自己的告警；在这里再报一次"颜色不存在"会把排查引向错的地方。
//
// Must be called with s.mu held.
func (s *server) nodeTunnelColorsLocked(tenantID string, nodeID uint16) (map[uint8]int, bool) {
	uc := s.underlayConfigLocked(tenantID, false)
	if uc == nil || uc.State == nil || !uc.State.Configured() {
		return nil, false
	}
	out := map[uint8]int{}
	for _, t := range uc.State.SortedTunnels() {
		if t.A.NodeID != nodeID && t.B.NodeID != nodeID {
			continue
		}
		out[t.Color]++
	}
	return out, true
}

// nodeTunnelPeersLocked 是"到每台对端各有哪些颜色"：动态策略的 SLA 是按对端
// 量的（插针挂在到对端的每条隧道上），所以动态行的校验与页面下拉都要这份视图，
// 而不是上面那份按颜色聚合计数的。同一对端同色多条隧道按一条算 —— 探针按隧道
// 建，选路却只到颜色这一层，多了一条只是多一份样本，不改变可选集合。
func (s *server) nodeTunnelPeersLocked(tenantID string, nodeID uint16) (map[uint16][]uint8, bool) {
	uc := s.underlayConfigLocked(tenantID, false)
	if uc == nil || uc.State == nil || !uc.State.Configured() {
		return nil, false
	}
	seen := map[[2]uint32]bool{}
	out := map[uint16][]uint8{}
	for _, t := range uc.State.SortedTunnels() {
		var peer uint16
		switch {
		case t.A.NodeID == nodeID:
			peer = t.B.NodeID
		case t.B.NodeID == nodeID:
			peer = t.A.NodeID
		default:
			continue
		}
		k := [2]uint32{uint32(peer), uint32(t.Color)}
		if seen[k] {
			continue
		}
		seen[k] = true
		out[peer] = append(out[peer], t.Color)
	}
	for p := range out {
		sort.Slice(out[p], func(i, j int) bool { return out[p][i] < out[p][j] })
	}
	return out, true
}

// policyChecksLocked 是第 7 段的校验，和基础配置一样在保存与发布两处都跑：
// 设备的客户口、隧道颜色都可能在策略写完之后变。
//
// 哪些是 error、哪些是 warning，分界线是"盒子会不会拒绝/会不会一定不通"：
//
//   - 盒子一定拒绝的（VPN 不存在、入口口一个都没有、颜色越界、颜色 0 兜底、
//     协议不认识、非 tcp/udp 带端口）→ error，发布会挡住。
//   - "可能不通但语义合法"的（颜色上没有任何隧道）→ warning。故意让某个颜色
//     黑洞是允许的配置，挡住它会逼用户去改隧道。
//
// Must be called with s.mu held.
func (s *server) policyChecksLocked(tenantID string, nodeID uint16, dc *DeviceConfig, set PolicySet) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}

	if dc == nil || dc.Version == 0 {
		add("error", "设备 %d 还没有保存过基础配置草稿（v0）：先去「配置 › 基础配置」保存一次，"+
			"策略的入口口是从那一份配置里的客户口派生出来的。", nodeID)
		return out
	}
	if len(set.Rows) > policyMaxRows {
		add("error", "策略行最多 %d 条，当前 %d 条。", policyMaxRows, len(set.Rows))
	}

	tunnelColors, hasUnderlay := s.nodeTunnelColorsLocked(tenantID, nodeID)
	peerColors, hasPeers := s.nodeTunnelPeersLocked(tenantID, nodeID)
	fbOn := map[[2]uint32]bool{}
	for _, f := range set.Fallback {
		fbOn[[2]uint32{f.VPN, uint32(f.Color)}] = true
	}

	var prior []policyMatch
	for i, r := range set.Rows {
		label := policyRowLabel(r, i)
		if r.Mode != "" && r.Mode != policyModeDynamic {
			add("error", "%s 的 mode「%s」不认识：留空 = 静态（钉死颜色），或写 dynamic（按 SLA 现场挑）。", label, r.Mode)
		}
		if r.Mode == policyModeDynamic {
			// 动态行的专有校验。它和静态行共享下面的 VPN/入口口/五元组检查；
			// "颜色上有没有隧道"那条检查只属于静态行。
			switch {
			case r.Peer == 0:
				add("error", "%s 是动态行，但没有指定对端 CPE：SLA 是按\"到这台对端的隧道\"量的，没有对端就没法量。", label)
			case r.Peer == nodeID:
				add("error", "%s 的对端是它自己（节点 %d）：到自己没有隧道，也没有 SLA 可量。", label, nodeID)
			case !hasPeers:
				add("error", "%s：这台设备还没有隧道拓扑（先在「SD-WAN 隧道」页配置并发布），动态行没有可量的隧道。", label)
			case len(peerColors[r.Peer]) == 0:
				add("error", "%s：这台设备到节点 %d 一条隧道都没有，动态行没有可量的隧道。", label, r.Peer)
			}
			if len(peerColors) > 0 && r.Peer != nodeID {
				got := map[uint8]bool{}
				for _, c := range peerColors[r.Peer] {
					got[c] = true
				}
				if len(r.Colors) == 0 {
					// 空候选 = 该对端全部颜色。这是一句现状说明，不是错误。
					if len(peerColors[r.Peer]) > 0 {
						add("info", "%s：候选颜色留空 = 到节点 %d 的全部颜色（当前 %s）。", label, r.Peer, policyColorList(peerColors[r.Peer]))
					}
				} else {
					usable := 0
					for _, c := range r.Colors {
						switch {
						case c > underlay.MaxTunnelColor:
							add("error", "%s 的候选颜色 %d 超出 0–%d。", label, c, underlay.MaxTunnelColor)
						case c == 0:
							add("error", "%s 的候选颜色里有 0：颜色 0 的落点就是主表，不是一条隧道，没有探针可量。"+
								"不达标时盒子会自动走最接近的一条，不需要把颜色 0 排进来。", label)
						case got[c]:
							usable++
						default:
							add("warning", "%s 的候选颜色里有 %d，但到节点 %d 没有这个颜色的隧道：它会被当成永远不可选。", label, c, r.Peer)
						}
					}
					if usable == 0 && len(peerColors[r.Peer]) > 0 {
						add("error", "%s：候选颜色里没有一条到节点 %d 的隧道，这一行动态选不出任何路径。", label, r.Peer)
					}
				}
			}
			if s := r.SLA; s != nil {
				switch {
				case s.LatencyMS < 0 || s.JitterMS < 0:
					add("error", "%s 的 SLA 门槛不能是负数。", label)
				case s.LatencyMS > 60000 || s.JitterMS > 60000:
					add("error", "%s 的 SLA 门槛超过 60 秒：探针的超时上限就是 60 秒，比它更宽的门槛量不出来。", label)
				case s.LossPermille > 1000:
					add("error", "%s 的丢包门槛 %d‰ 超过 1000‰。", label, s.LossPermille)
				}
				if *s == (PolicySLA{}) {
					add("info", "%s 的 SLA 三项都是 0 = 不设门槛：永远挑延迟最低的那条颜色。这是合法用法。", label)
				}
			}
		}
		switch {
		case r.VPN > uint32(route.MaxVRFID):
			add("error", "%s 的 VPN 号超出范围（0–%d）：路由平面的 VRF id 只有 8 位。", label, route.MaxVRFID)
		case r.VPN == vpnReservedOverlay:
			add("error", "%s 指向 VPN %d —— overlay 保留表，客户流量不能按它选路。", label, r.VPN)
		case kernelReservedVPN(r.VPN):
			add("error", "%s 指向 VPN %d —— 内核保留表（253/254/255），这台盒子不拿它承载客户流量。", label, r.VPN)
		case !configHasVPN(dc, r.VPN):
			add("error", "%s：这台设备的基础配置里没有 VPN %d，先在「基础配置」把它加出来。", label, r.VPN)
		}
		if r.Color > underlay.MaxTunnelColor {
			add("error", "%s 的颜色超出 0–%d：颜色表按 颜色×1000 分段，再大就和下一段重叠了。",
				label, underlay.MaxTunnelColor)
		}

		if p, canon, err := policyCheckPrefix(r.Src); err != nil {
			add("error", "%s 的源前缀「%s」不合法（应为 a.b.c.d/len 或留空表示任意）。", label, r.Src)
		} else if !canon {
			add("error", "%s 的源前缀 %s 不是规范网段，应写成 %s。", label, r.Src, p.Masked())
		}
		if p, canon, err := policyCheckPrefix(r.Dst); err != nil {
			add("error", "%s 的目的前缀「%s」不合法（应为 a.b.c.d/len 或留空表示任意）。", label, r.Dst)
		} else if !canon {
			add("error", "%s 的目的前缀 %s 不是规范网段，应写成 %s。", label, r.Dst, p.Masked())
		}

		if !policyProtoValid(r.Proto) {
			add("error", "%s 的协议 %d 不认识：只能是 任意(0) / ICMP(1) / TCP(6) / UDP(17)。", label, r.Proto)
		} else if r.Proto != policyProtoTCP && r.Proto != policyProtoUDP && (r.SrcPort != "" || r.DstPort != "") {
			add("error", "%s 是 %s，上面不该有端口：端口只在 TCP/UDP 上有意义（ICMP 的那两个字段是 type/code，本系统不按它匹配）。",
				label, policyProtoName(r.Proto))
		}
		if _, err := policySpanOf(r.SrcPort); err != nil {
			add("error", "%s 的源端口「%s」不合法（写成 80 或 1000-2000）。", label, r.SrcPort)
		}
		if _, err := policySpanOf(r.DstPort); err != nil {
			add("error", "%s 的目的端口「%s」不合法（写成 80 或 1000-2000）。", label, r.DstPort)
		}

		// 入口口：这一行的 attach 是派生的，一个口都没有就等于装了不匹配任何流量
		// 的策略（盒子上会整段拒绝，所以这里是 error）。
		if n := len(policyAttachIfaces(&dc.Draft, r.VPN)); n == 0 {
			add("error", "%s：VPN %d 在这台设备上一个客户口都没有，策略没有入口可以挂。"+
				"先在「基础配置」给它加一个客户口。", label, r.VPN)
		}

		// 颜色 0 不参与这套推理：它的落点就是这张 VPN 的主表，永远不会"没有表"。
		if r.Color != 0 && r.Color <= underlay.MaxTunnelColor && hasUnderlay {
			if tunnelColors[r.Color] == 0 {
				if fbOn[[2]uint32{r.VPN, uint32(r.Color)}] {
					add("info", "%s：这台设备没有颜色 %d 的隧道，命中的流会按兜底落到 VPN %d 的主表（颜色 0）。",
						label, r.Color, r.VPN)
				} else {
					add("warning", "%s：这台设备没有颜色 %d 的隧道，也没勾兜底 —— 命中的流会被丢掉。"+
						"给某条隧道配上这个颜色，或在下面勾「允许 fallback 到颜色 0」。", label, r.Color)
				}
			}
		}

		m := policyMatchOf(&dc.Draft, i, r)
		for _, pm := range prior {
			if pm.covers(m) {
				add("warning", "%s 被第 %d 行完全覆盖：前面的行更宽，这一行永远匹配不到（按行号顺序匹配，先命中先用）。",
					label, pm.idx+1)
				break
			}
		}
		prior = append(prior, m)
	}

	seenFB := map[[2]uint32]bool{}
	for _, f := range set.Fallback {
		label := fmt.Sprintf("兜底（VPN %d，颜色 %d）", f.VPN, f.Color)
		k := [2]uint32{f.VPN, uint32(f.Color)}
		if seenFB[k] {
			add("warning", "%s 重复勾选，按一次算。", label)
		}
		seenFB[k] = true
		if f.Color == 0 {
			add("error", "%s：颜色 0 的落点就是这张 VPN 的主表，兜底到它没有意义"+
				"（盒子上会整段拒绝这份策略）。", label)
			continue
		}
		if f.Color > underlay.MaxTunnelColor {
			add("error", "%s 的颜色超出 0–%d。", label, underlay.MaxTunnelColor)
			continue
		}
		switch {
		case f.VPN > uint32(route.MaxVRFID):
			add("error", "%s 的 VPN 号超出范围（0–%d）。", label, route.MaxVRFID)
		case f.VPN == vpnReservedOverlay:
			add("error", "%s 指向 overlay 保留表 VPN %d。", label, f.VPN)
		case kernelReservedVPN(f.VPN):
			add("error", "%s 指向内核保留表 VPN %d。", label, f.VPN)
		case !configHasVPN(dc, f.VPN):
			add("error", "%s：这台设备的基础配置里没有 VPN %d。", label, f.VPN)
		}
		used := false
		for _, r := range set.Rows {
			if r.VPN == f.VPN && r.Color == f.Color {
				used = true
				break
			}
		}
		if !used {
			add("info", "%s 勾上了，但没有任何策略行指向这个颜色：这条默认路由装上了也没人用。", label)
		}
	}
	return out
}

// policyEmptyCheckText 给"一条策略都没有"准备一句人话，页面和日志共用。
func policyEmptyCheckText(set PolicySet) string {
	if len(set.Rows) == 0 {
		return "一条策略都没有：所有流量按主表（颜色 0）选路。"
	}
	return fmt.Sprintf("%d 条策略，%d 个兜底开关。", len(set.Rows), len(set.Fallback))
}
