package vppapi

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ==================== IP FIB / MPLS：VRF 表、带标签路由、本地标签 ====================
//
// 这三条消息是 FIB 加载器（RR 路由 → VPP）唯一的下发口，所以每个字段的偏移都
// 对着 srwg-node1 里 VPP 自己生成的 packed 结构算过，不是照着 JSON 猜的：
//
//	/usr/include/vnet/ip/ip_types.api_types.h   address / prefix
//	/usr/include/vnet/fib/fib_types.api_types.h fib_path / fib_mpls_label
//	/usr/include/vnet/ip/ip.api_types.h         ip_table / ip_route
//	/usr/include/vnet/mpls/mpls.api_types.h     mpls_route
//
// 几个容易算错、算错就"超时且没有错误"的地方：
//
//   - address_family 是 **packed u8 枚举**（STATIC_ASSERT 就写着 sizeof==1），
//     而 fib_path_type/flags/nh_proto 是普通枚举、各占 4 字节。把 af 当 4 字节
//     会把整个 prefix 往后推 3 个字节。
//   - fib_mpls_label 是 7 字节（is_uniform u8 + label u32 + ttl u8 + exp u8），
//     label_stack 固定 16 个，所以光标签区就是 112 字节。
//   - ip_route 是变长（paths[] 跟在 n_paths 后面），消息体不能按定长发。
//
// 语义取自 CLI 的实测回读（2026-10-25，node1）：
//
//	ip route ... table 100 via 192.168.99.2 wg0 out-labels 777
//	  → path: sw_if_index=wg0, table_id=0, type=NORMAL, proto=IP4,
//	          nh=192.168.99.2, n_labels=1, label_stack[0].label=777
//	mpls local-label add 777 eos via ip4-lookup-in-table 100
//	  → mpls route table 0, label 777, eos=1, eos_proto=IP4,
//	    path: sw_if_index=~0, table_id=100, type=NORMAL, proto=IP4, nh=0
//
// 不用 cli_inband 做下发：实测 CLI 对重复 add、删除不存在的对象一律 retval=0，
// 失败被吞掉；二进制 API 才会把 -6/-31 这类错误带回来。

const (
	// fibPathLen 是 vl_api_fib_path_t 的 packed 尺寸。
	// 4+4+4+1+1+4+4+4 + (16+4+4+4) + 1 + 16*7 = 167
	fibPathLen  = 167
	fibLabelLen = 7
	// ipRouteFixedLen 是 is_add + is_multipath + ip_route 的定长部分：
	// 1+1 + table_id(4) + stats_index(4) + prefix(18) + n_paths(1)
	ipRouteFixedLen = 29
	// mplsRouteFixedLen 是 mr_is_add + mr_is_multipath + mpls_route 的定长部分：
	// 1+1 + table_id(4) + label(4) + eos(1) + eos_proto(1) + is_multicast(1) + n_paths(1)
	mplsRouteFixedLen = 14
	// mplsRouteMaxLabels 对应 label_stack[16]。超过就是调用方拼错了栈。
	fibPathMaxLabels = 16
	// mplsLabelMax 是 20 位标签的上限（RFC 3032）。
	mplsLabelMax = 1<<20 - 1
)

// FIBPath 是一条下一跳。两种形态共用同一个 vl_api_fib_path_t：
//
//   - IP 路由：SWIfIndex=隧道口，NextHop=对端 overlay 地址，Labels=要压的 VPN 标签。
//   - MPLS 本地标签的 lookup-in-table 路径：SWIfIndex=^0，TableID=要查的表，
//     NextHop 留空。实测 CLI 生成的就是这个形状。
type FIBPath struct {
	SWIfIndex  uint32
	TableID    uint32
	NextHop    netip.Addr
	Labels     []uint32
	Preference uint8
	// Weight 是分担权重（VPP fib_path.weight，u8）。0 按协议缺省写成 1：
	// 同桶多路径等价（ECMP）就是"全部写 1"这一种情况，>1 时按比例复制
	// load-balance 桶（UCMP，按流分流）。读回（readFIBPath）取原值，不归一。
	Weight uint8
}

// IPRoute 是 ip_route_details 读回来的一条（只保留本系统会写的那些字段）。
type IPRoute struct {
	TableID uint32
	Prefix  netip.Prefix
	Paths   []FIBPath
}

// MPLSRoute 是 mpls_route_details 读回来的一条。
type MPLSRoute struct {
	TableID  uint32
	Label    uint32
	EOS      bool
	EOSProto uint8
	Paths    []FIBPath
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// appendIPv4Prefix 写 vl_api_prefix_t（packed 18 字节）：address(af u8 + union[16]) + len u8。
// appendAddress 已经按这个形状写 address，这里只补前缀长度。
func appendIPv4Prefix(b []byte, p netip.Prefix) []byte {
	b = appendAddress(b, p.Masked().Addr().AsSlice())
	return append(b, byte(p.Bits()))
}

// appendFIBPath 写一条 vl_api_fib_path_t。
func appendFIBPath(b []byte, p FIBPath) []byte {
	b = appendU32(b, p.SWIfIndex)
	b = appendU32(b, p.TableID)
	b = appendU32(b, 0) // rpf_id
	// weight：0 = 调用方没填 = 协议缺省 1（ECMP）。UCMP 就是这里写 >1。
	w := p.Weight
	if w == 0 {
		w = 1
	}
	b = append(b, w)
	b = append(b, p.Preference)
	b = appendU32(b, 0) // type = FIB_API_PATH_TYPE_NORMAL
	b = appendU32(b, 0) // flags = FIB_API_PATH_FLAG_NONE
	b = appendU32(b, 0) // proto = FIB_API_PATH_NH_PROTO_IP4

	// nh.address：union 从偏移 0 开始，v4 之后补 12 个 0；没有下一跳就是全 0。
	nh := make([]byte, 16)
	if p.NextHop.Is4() {
		a := p.NextHop.As4()
		copy(nh, a[:])
	}
	b = append(b, nh...)
	b = appendU32(b, 0) // via_label
	b = appendU32(b, 0) // obj_id
	b = appendU32(b, 0) // classify_table_index

	b = append(b, byte(len(p.Labels)))
	for i := 0; i < fibPathMaxLabels; i++ {
		if i < len(p.Labels) {
			b = append(b, 0) // is_uniform：false = 标签是常量，不继承内层 TTL/TC
			b = appendU32(b, p.Labels[i])
			b = append(b, 0) // ttl：0 = 用默认值
			b = append(b, 0) // exp
		} else {
			b = append(b, make([]byte, fibLabelLen)...)
		}
	}
	return b
}

func validatePath(p FIBPath) error {
	if len(p.Labels) > fibPathMaxLabels {
		return fmt.Errorf("vppapi: 一条路径最多 %d 个标签，给了 %d 个", fibPathMaxLabels, len(p.Labels))
	}
	for _, l := range p.Labels {
		if l > mplsLabelMax {
			return fmt.Errorf("vppapi: 标签 %d 超出 20 位", l)
		}
	}
	// 普通转发路径必须给一个 IPv4 下一跳；lookup-in-table 路径（MPLS 本地标签）
	// 由 SWIfIndex=^0 + TableID 表达，不需要下一跳。
	if p.SWIfIndex != ^uint32(0) && !p.NextHop.Is4() {
		return fmt.Errorf("vppapi: 路径 sw_if_index=%d 没有 IPv4 下一跳", p.SWIfIndex)
	}
	return nil
}

// IPTableAddDel 建/删一张 VRF 表（ip_table_add_del）。重复添加是幂等的。
func (c *Client) IPTableAddDel(ctx context.Context, tableID uint32, add bool) error {
	body := make([]byte, 0, 1+4+1+64)
	body = append(body, boolByte(add))
	body = appendU32(body, tableID)
	body = append(body, 0)                   // is_ip6
	body = append(body, make([]byte, 64)...) // name：留空让 VPP 自己命名
	frame, err := c.call(ctx, "ip_table_add_del", "ip_table_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("ip_table_add_del", frame)
}

// IPRouteAddDel 在一张 VRF 表里加/删一条 IPv4 路由。
//
// is_multipath 固定 0 = **整条替换**。FIB 加载器每次从 RIB 重算完整路径集合，
// 用增量(1)会留下已经撤掉的路径，而 VPP 不会告诉你它还在。
func (c *Client) IPRouteAddDel(ctx context.Context, tableID uint32, prefix netip.Prefix, paths []FIBPath, add bool) error {
	if !prefix.Addr().Is4() {
		return fmt.Errorf("vppapi: IPRouteAddDel 只做 IPv4，收到 %s", prefix)
	}
	if len(paths) == 0 || len(paths) > 255 {
		return fmt.Errorf("vppapi: 路由 %s 需要 1..255 条路径，给了 %d", prefix, len(paths))
	}
	for _, p := range paths {
		if err := validatePath(p); err != nil {
			return err
		}
	}
	return c.ipRouteAddDel(ctx, tableID, prefix, paths, add)
}

// ipRouteAddDel 是两条公开入口共用的消息体组装。
func (c *Client) ipRouteAddDel(ctx context.Context, tableID uint32, prefix netip.Prefix, paths []FIBPath, add bool) error {
	body := make([]byte, 0, ipRouteFixedLen+len(paths)*fibPathLen)
	body = append(body, boolByte(add), 0) // is_add, is_multipath
	body = appendU32(body, tableID)
	body = appendU32(body, 0) // stats_index
	body = appendIPv4Prefix(body, prefix)
	body = append(body, byte(len(paths)))
	for _, p := range paths {
		body = appendFIBPath(body, p)
	}
	frame, err := c.call(ctx, "ip_route_add_del", "ip_route_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("ip_route_add_del", frame)
}

// IPRouteViaInterface 加/删一条"只指定出口接口、不带下一跳"的 IPv4 路由。
//
// 这就是 CLI 的 `ip route add table 250 <peer>/32 via wg0`：overlay 里对端 /32
// 是给 BFD、探针和排障用的直连条目，下一跳由 WG 自己的 allowed-ips 决定，所以
// 这条路由没有 nh 地址（nh=0.0.0.0 + sw_if_index）。它和 IPRouteAddDel 分开，
// 是因为后者刻意要求"非 ~0 的路径必须有 IPv4 下一跳"——那条校验保护的是 FIB
// 加载器，不该为了这一种合法形态被放宽。
func (c *Client) IPRouteViaInterface(ctx context.Context, tableID uint32, prefix netip.Prefix, swIfIndex uint32, add bool) error {
	if !prefix.Addr().Is4() {
		return fmt.Errorf("vppapi: IPRouteViaInterface 只做 IPv4，收到 %s", prefix)
	}
	if swIfIndex == ^uint32(0) {
		return fmt.Errorf("vppapi: IPRouteViaInterface 需要一个具体接口，收到 ~0")
	}
	return c.ipRouteAddDel(ctx, tableID, prefix, []FIBPath{{SWIfIndex: swIfIndex}}, add)
}

// IPRoutes 读回一张表里的全部 IPv4 前缀（只用来核对"这条路由在不在"）。
//
// 不带路径细节是刻意的：调用方问的是"overlay 表里有没有它"，把整个 fib_path
// 暴露出去会让"在不在"变成"路径长得对不对"，而后者是 FIB 加载器自己的事。
func (c *Client) IPRoutes(ctx context.Context, tableID uint32) ([]netip.Prefix, error) {
	rs, err := c.FIBRoutes(ctx, tableID)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, len(rs))
	for _, r := range rs {
		if r.Prefix.IsValid() {
			out = append(out, r.Prefix)
		}
	}
	return out, nil
}

// MPLSLabelAddDel 在默认 MPLS 表里加/删一条本地标签：
// "该标签 EOS 之后按 IPv4 查 lookupTable"。
//
// 这正是跨 VRF 收包侧的最后一跳：远端压着这个标签来的包，弹栈后落进
// lookupTable 那张 VRF 表，再由表里的路由决定去哪。
func (c *Client) MPLSLabelAddDel(ctx context.Context, label uint32, lookupTable uint32, add bool) error {
	if label > mplsLabelMax {
		return fmt.Errorf("vppapi: 标签 %d 超出 20 位", label)
	}
	body := make([]byte, 0, mplsRouteFixedLen+fibPathLen)
	body = append(body, boolByte(add), 0) // mr_is_add, mr_is_multipath
	body = appendU32(body, 0)             // mr_table_id：默认标签表
	body = appendU32(body, label)
	body = append(body, 1) // mr_eos
	body = append(body, 0) // mr_eos_proto = FIB_API_PATH_NH_PROTO_IP4
	body = append(body, 0) // mr_is_multicast
	body = append(body, 1) // mr_n_paths
	body = appendFIBPath(body, FIBPath{SWIfIndex: ^uint32(0), TableID: lookupTable})
	frame, err := c.call(ctx, "mpls_route_add_del", "mpls_route_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("mpls_route_add_del", frame)
}

// MPLSLabelDel 删除一条本地标签。单独一条是因为删除路径不关心 lookupTable：
// VPP 按标签删整条，多给的表号会被忽略（实测 CLI 同义命令返回 0）。
func (c *Client) MPLSLabelDel(ctx context.Context, label uint32) error {
	if label > mplsLabelMax {
		return fmt.Errorf("vppapi: 标签 %d 超出 20 位", label)
	}
	body := make([]byte, 0, mplsRouteFixedLen+fibPathLen)
	body = append(body, 0, 0) // is_add=0, is_multipath=0
	body = appendU32(body, 0)
	body = appendU32(body, label)
	body = append(body, 1, 0, 0, 1)
	body = appendFIBPath(body, FIBPath{SWIfIndex: ^uint32(0)})
	frame, err := c.call(ctx, "mpls_route_add_del", "mpls_route_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("mpls_route_add_del", frame)
}

// CLI 在 VPP 进程里执行一条 CLI 命令（cli_inband）。
//
// 只有 `sr-wg mpls-egress` 用它：那条动作没有二进制 API，而且失败也只体现在
// "邻接还是指向 tunnel-output"这种可回读的状态上。不要拿它下发 FIB —— 实测
// CLI 把错误吞成 retval=0。
func (c *Client) CLI(ctx context.Context, cmd string) (string, error) {
	// vl_api_string_t = u32 length + bytes，**不含结尾 NUL**（vpp_papi 的
	// String.pack 就是这么写的：length=len(s)，buf 原样）。
	body := make([]byte, 0, 4+len(cmd))
	body = appendU32(body, uint32(len(cmd)))
	body = append(body, cmd...)
	frame, err := c.call(ctx, "cli_inband", "cli_inband_reply", body)
	if err != nil {
		return "", err
	}
	if len(frame) < 14 {
		return "", fmt.Errorf("vppapi: cli_inband_reply 太短 (%d B)", len(frame))
	}
	rv := int32(binary.BigEndian.Uint32(frame[6:10]))
	n := binary.BigEndian.Uint32(frame[10:14])
	if len(frame) < 14+int(n) {
		return "", fmt.Errorf("vppapi: cli_inband_reply 的字符串长度 %d 超过帧长 %d", n, len(frame))
	}
	out := string(frame[14 : 14+int(n)])
	if rv != 0 {
		return out, APIError{Op: "cli_inband", Retval: rv}
	}
	return out, nil
}

// readonlyAddress 读 vl_api_address_t（17 字节），地址族不是 v4/v6 时返回无效值。
func readAddressNetip(b []byte) netip.Addr {
	switch b[0] {
	case apiAddressFamilyIP4:
		var a [4]byte
		copy(a[:], b[1:5])
		return netip.AddrFrom4(a)
	case apiAddressFamilyIP6:
		var a [16]byte
		copy(a[:], b[1:17])
		return netip.AddrFrom16(a)
	}
	return netip.Addr{}
}

func readPrefix(b []byte) netip.Prefix {
	addr := readAddressNetip(b)
	if !addr.IsValid() {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(addr, int(b[17]))
}

// readFIBPath 解析一条 vl_api_fib_path_t（167 字节）。
func readFIBPath(b []byte) FIBPath {
	p := FIBPath{
		SWIfIndex:  binary.BigEndian.Uint32(b[0:4]),
		TableID:    binary.BigEndian.Uint32(b[4:8]),
		Weight:     b[12],
		Preference: b[13],
	}
	var nh [16]byte
	copy(nh[:], b[26:42])
	if p.SWIfIndex != ^uint32(0) {
		p.NextHop = netip.AddrFrom4([4]byte{nh[0], nh[1], nh[2], nh[3]})
	}
	n := int(b[54])
	labels := b[55 : 55+fibPathMaxLabels*fibLabelLen]
	for i := 0; i < n && i < fibPathMaxLabels; i++ {
		p.Labels = append(p.Labels, binary.BigEndian.Uint32(labels[i*fibLabelLen+1:i*fibLabelLen+5]))
	}
	return p
}

// FIBRoutes 读回一张 VRF 表里的 IPv4 路由（按需用于核对下发结果）。
//
// 三件实测（决定调用方能不能拿"空"当判据）：
//   - dump 是**按表**的：表 100 里有路由、表 0 里有一堆 overlay 路由时，
//     dump 100 回的全是 table=100，一条外来表项都没有。
//   - 表**不存在**：零帧，retval 0，不报错。
//   - 表存在但没人装路由：不是空的 —— VPP 自动带 5 条 special
//     （0.0.0.0/0、0.0.0.0/32、224.0.0.0/4、240.0.0.0/4、255.255.255.255/32），
//     下一跳读回来是 invalid、preference=0，不是本加载器写的。
//
// 所以 len()==0 只表示"这张表不存在"，而"这张表里有没有我们的路由"必须看
// preference 或前缀，不能拿条目数当数。
func (c *Client) FIBRoutes(ctx context.Context, tableID uint32) ([]IPRoute, error) {
	body := make([]byte, 0, 69)
	body = appendU32(body, tableID)
	body = append(body, 0)                   // is_ip6
	body = append(body, make([]byte, 64)...) // name
	frames, err := c.dump(ctx, "ip_route_dump", "ip_route_details", body)
	if err != nil {
		return nil, err
	}
	out := make([]IPRoute, 0, len(frames))
	for _, f := range frames {
		// 帧 = msg_id(2)+context(4) + ip_route
		if len(f) < 6+27 {
			return nil, fmt.Errorf("vppapi: ip_route_details 太短 (%d B)", len(f))
		}
		b := f[6:]
		r := IPRoute{
			TableID: binary.BigEndian.Uint32(b[0:4]),
			Prefix:  readPrefix(b[8:26]),
		}
		n := int(b[26])
		at := 27
		if len(b) < at+n*fibPathLen {
			return nil, fmt.Errorf("vppapi: ip_route_details 路径区被截断（%d 条，%d B）", n, len(b))
		}
		for i := 0; i < n; i++ {
			r.Paths = append(r.Paths, readFIBPath(b[at+i*fibPathLen:at+(i+1)*fibPathLen]))
		}
		out = append(out, r)
	}
	return out, nil
}

// MPLSRoutes 读回默认 MPLS 标签表（tableID 只用于选表，本系统只用 0）。
func (c *Client) MPLSRoutes(ctx context.Context, tableID uint32) ([]MPLSRoute, error) {
	body := make([]byte, 0, 68)
	body = appendU32(body, tableID)
	body = append(body, make([]byte, 64)...) // mt_name[64]
	frames, err := c.dump(ctx, "mpls_route_dump", "mpls_route_details", body)
	if err != nil {
		return nil, err
	}
	out := make([]MPLSRoute, 0, len(frames))
	for _, f := range frames {
		if len(f) < 6+12 {
			return nil, fmt.Errorf("vppapi: mpls_route_details 太短 (%d B)", len(f))
		}
		b := f[6:]
		r := MPLSRoute{
			TableID:  binary.BigEndian.Uint32(b[0:4]),
			Label:    binary.BigEndian.Uint32(b[4:8]),
			EOS:      b[8] != 0,
			EOSProto: b[9],
		}
		n := int(b[11])
		at := 12
		if len(b) < at+n*fibPathLen {
			return nil, fmt.Errorf("vppapi: mpls_route_details 路径区被截断（%d 条，%d B）", n, len(b))
		}
		for i := 0; i < n; i++ {
			r.Paths = append(r.Paths, readFIBPath(b[at+i*fibPathLen:at+(i+1)*fibPathLen]))
		}
		out = append(out, r)
	}
	return out, nil
}
