package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/vppapi"
)

// fakeVPP 是一个按**真机实测语义**行事的假数据面，不是随便记调用：
//
//   - 实例号被占 → -31；同公钥重复 add peer → -116，**而且跨接口也 -116**
//     （srwg-node1 上两张接口装同一把公钥实测第二次 -116，去重键不含接口）；
//     0 条 allowed_ip → -7；删不存在的 peer → -6；
//   - 绑一条不存在的路径 → -6；解绑一条不存在的绑定 → -6；
//   - 同 path_id 的 AddPath 原地替换；删不存在的路径 → -6；删存在的路径会级联
//     清掉引用它的隧道绑定且 retval=0；
//   - 建接口之后 sr-wg 的 UDP 挂点丢掉，所以 AddPort + RebindPorts 必须被调用；
//   - 隧道绑定的**收包侧**键是 (外层 endpoint, 封装前 dst 端口)：两张接口抢同一个
//     身份 → -116（插件 sr_wg_tunnel_set_path 里的 ENTRY_ALREADY_EXISTS）。
//
// 这些数字全部来自 internal/vppapi 的 live gate（真 VPP 26.06）与插件源码
// （vpp-plugin/sr_wg_api.c 的 sr_wg_path_del handler、sr_wg4_encap.c 的
// sr_wg_tunnel_set_path），所以这里测的是
// "reconcile 逻辑在真实数据面的脾气下能不能反复重放"，而不是"我想象中的 VPP"。
type fakeVPP struct {
	calls   []string
	ifaces  []vppapi.InterfaceInfo
	peers   []vppapi.WgPeer
	paths   map[uint16]vppapi.PathInfo
	tunnels map[uint32]vppapi.TunnelInfo
	keys    map[uint16][]byte
	encap   map[uint32]bool
	mtus    map[uint32]uint16
	mtuErr  error
	// swIfs 是 core 接口表里**非 WG** 的那几张（真机上还有 local0 之类，本测试
	// 只关心 underlay 出口）。WG 那几张由 CreateInterface 的记账现算。
	swIfs   []vppapi.SwInterface
	dumpIfs []vppapi.SwInterface // 非空时 DumpInterfaces 原样回它：用来造"记录不符"
	dumpErr error
	ports   []vppapi.PortInfo
	rebinds int
	// transitPort 是最后一次 SetTransit 的端口（真机的 transit_enabled 是全局开关，
	// 只把端口加进集合不够，agent 必须显式打开）。
	transitPort uint16
	obs         []vppapi.Observation
	nextIdx uint32
	nextPi  uint32
	// alreadyGone 让 RemovePeer **删掉的同时**回 -6：模拟"数据面里这条其实
	// 已经没了"（上次崩在中途、或对端刚被别的路径撤掉）。
	alreadyGone bool
}

func newFakeVPP() *fakeVPP {
	return &fakeVPP{
		paths:   map[uint16]vppapi.PathInfo{},
		tunnels: map[uint32]vppapi.TunnelInfo{},
		keys:    map[uint16][]byte{},
		encap:   map[uint32]bool{},
		nextIdx: 1,
		nextPi:  1,
		// underlay 出口按真机的出生样子摆在这儿：af-packet 口 VPP 默认记 9000，
		// 容器网卡只有 1500。这样"写完读回来"测的才是读回，而不是 map 里本来就对。
		swIfs: []vppapi.SwInterface{{
			SWIfIndex: encapIfIndex, Name: "host-eth0",
			L3MTU: bornVIFMTU, AdminUp: true, LinkUp: true,
		}},
	}
}

func (f *fakeVPP) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeVPP) SetPKey(ctx context.Context, hmacID uint16, key []byte) error {
	f.log("SetPKey %d", hmacID)
	f.keys[hmacID] = append([]byte(nil), key...)
	return nil
}

func (f *fakeVPP) RebindPorts(ctx context.Context) error {
	f.log("RebindPorts")
	f.rebinds++
	return nil
}

// SetTransit 的假语义照抄插件的 sr_wg_transit_enable：把端口加进 transit 集合
// 并置启用位。
func (f *fakeVPP) SetTransit(ctx context.Context, port uint16) error {
	f.log("SetTransit port=%d", port)
	if port == 0 {
		return vppapi.APIError{Op: "sr_wg_transit_enable", Retval: -7}
	}
	f.transitPort = port
	for _, p := range f.ports {
		if p.Kind == vppapi.PortKindTransit && p.Port == port {
			return nil
		}
	}
	f.ports = append(f.ports, vppapi.PortInfo{Kind: vppapi.PortKindTransit, Port: port})
	return nil
}

// AddPort 的假语义照抄插件：幂等，并且每次都会把端口挂回 sr-wg 节点，所以
// 建 WG 接口（假数据面里就是 CreateInterface）之后必须 AddPort，否则那条隧道
// 的端口根本不在集合里、没人给它加 SR 头。
func (f *fakeVPP) AddPort(ctx context.Context, kind uint8, port uint16) error {
	f.log("AddPort kind=%d port=%d", kind, port)
	if port == 0 {
		return vppapi.APIError{Op: "sr_wg_port_add_del", Retval: -7}
	}
	for _, p := range f.ports {
		if p.Kind == kind && p.Port == port {
			return nil
		}
	}
	f.ports = append(f.ports, vppapi.PortInfo{Kind: kind, Port: port})
	return nil
}

func (f *fakeVPP) DelPort(ctx context.Context, kind uint8, port uint16) error {
	f.log("DelPort kind=%d port=%d", kind, port)
	for i, p := range f.ports {
		if p.Kind == kind && p.Port == port {
			f.ports = append(f.ports[:i], f.ports[i+1:]...)
			return nil
		}
	}
	return vppapi.APIError{Op: "sr_wg_port_add_del", Retval: -6}
}

func (f *fakeVPP) Ports(ctx context.Context) ([]vppapi.PortInfo, error) {
	f.log("Ports")
	return append([]vppapi.PortInfo(nil), f.ports...), nil
}

func (f *fakeVPP) CreateInterface(ctx context.Context, p vppapi.InterfaceParams) (uint32, error) {
	f.log("CreateInterface inst=%d port=%d", p.UserInstance, p.ListenPort)
	for _, i := range f.ifaces {
		if i.UserInstance == p.UserInstance {
			return 0, vppapi.APIError{Op: "wireguard_interface_create", Retval: -31}
		}
	}
	idx := f.nextIdx
	f.nextIdx++
	k, err := ecdh.X25519().NewPrivateKey(p.PrivateKeys)
	if err != nil {
		return 0, err
	}
	f.ifaces = append(f.ifaces, vppapi.InterfaceInfo{
		UserInstance: p.UserInstance, SWIfIndex: idx, PublicKey: k.PublicKey().Bytes(),
		Port: p.ListenPort, SrcIP: p.SrcIP,
	})
	return idx, nil
}

func (f *fakeVPP) Interfaces(ctx context.Context, showPrivate bool) ([]vppapi.InterfaceInfo, error) {
	f.log("Interfaces private=%v", showPrivate)
	out := append([]vppapi.InterfaceInfo(nil), f.ifaces...)
	// showPrivate=false 时私钥必须读不到：VPP 也是这个行为，而"接口在、私钥是我们
	// 那把"这个判据用的就是公钥，不需要私钥。
	if !showPrivate {
		for i := range out {
			out[i].PrivateKey = nil
		}
	}
	return out, nil
}

func (f *fakeVPP) EnableEncap(ctx context.Context, swIfIndex uint32, enable bool) error {
	f.log("EnableEncap %d=%v", swIfIndex, enable)
	f.encap[swIfIndex] = enable
	return nil
}

// SetInterfaceMTU 照抄真机行为：**不做任何合理性判断**，只把数记下来。实测
// `set interface mtu packet 0` 也照样接受（等于取消记录），所以这里对 0 与出界的
// 值都不拦 —— 拦是 agent 在 NewLink 里做的（wire.MinTunnelMTU/MaxTunnelMTU +
// 预算），本 fake 的意义正是"别让测试通过靠的是数据面替我们把关"。
func (f *fakeVPP) SetInterfaceMTU(ctx context.Context, swIfIndex uint32, l3MTU uint16) error {
	f.log("SetInterfaceMTU %d=%d", swIfIndex, l3MTU)
	if f.mtuErr != nil {
		return f.mtuErr
	}
	if f.mtus == nil {
		f.mtus = map[uint32]uint16{}
	}
	f.mtus[swIfIndex] = l3MTU
	return nil
}

// bornVIFMTU 是 VPP 给 af-packet 口的默认 MTU 记录（实测 9000），而实验室的容器
// 网卡只有 1500。规则 6 那条发送侧卡死就是这两个数不一致造成的。
const bornVIFMTU = 9000

// DumpInterfaces 回读"VPP 眼下的接口表"：非 WG 的那几张来自 swIfs，WG 的那几张由
// CreateInterface 的记账现算，两张都叠上 SetInterfaceMTU 写过的值。
//
// 没写过的口保留出生值 —— 新建的 WG 接口四槽全 0（真机实测），underlay 是 9000。
// 这样 verifyLanded 读到什么取决于 agent 有没有真的去写，而不是取决于 map 的初值。
func (f *fakeVPP) DumpInterfaces(ctx context.Context) ([]vppapi.SwInterface, error) {
	f.log("DumpInterfaces")
	if f.dumpErr != nil {
		return nil, f.dumpErr
	}
	if len(f.dumpIfs) > 0 {
		return append([]vppapi.SwInterface(nil), f.dumpIfs...), nil
	}
	out := make([]vppapi.SwInterface, 0, len(f.swIfs)+len(f.ifaces))
	for _, i := range f.swIfs {
		if m, ok := f.mtus[i.SWIfIndex]; ok {
			i.L3MTU = uint32(m)
		}
		out = append(out, i)
	}
	for _, i := range f.ifaces {
		si := vppapi.SwInterface{
			SWIfIndex: i.SWIfIndex, Name: fmt.Sprintf("wg%d", i.UserInstance),
			AdminUp: true, LinkUp: true,
		}
		if m, ok := f.mtus[i.SWIfIndex]; ok {
			si.L3MTU = uint32(m)
		}
		out = append(out, si)
	}
	return out, nil
}

func (f *fakeVPP) AddPeer(ctx context.Context, p vppapi.PeerParams) (uint32, error) {
	f.log("AddPeer %s→%s:%d ka=%d allowed=%d", hex.EncodeToString(p.PublicKey)[:8],
		p.EndpointIP, p.Port, p.Keepalive, len(p.AllowedIPs))
	if len(p.AllowedIPs) == 0 {
		return 0, vppapi.APIError{Op: "wireguard_peer_add", Retval: -7}
	}
	// 去重键**只有公钥**：不看接口（真机实测跨接口也 -116）。
	for _, pe := range f.peers {
		if hex.EncodeToString(pe.PublicKey) == hex.EncodeToString(p.PublicKey) {
			return 0, vppapi.APIError{Op: "wireguard_peer_add", Retval: -116}
		}
	}
	idx := f.nextPi
	f.nextPi++
	f.peers = append(f.peers, vppapi.WgPeer{
		PeerIndex: idx, PublicKey: append([]byte(nil), p.PublicKey...),
		EndpointIP: p.EndpointIP, Port: p.Port, Keepalive: p.Keepalive,
		TableID: p.TableID, SWIfIndex: p.SWIfIndex,
		AllowedIPs: canonPrefixesForTest(p.AllowedIPs),
	})
	return idx, nil
}

func (f *fakeVPP) RemovePeer(ctx context.Context, peerIndex uint32) error {
	f.log("RemovePeer %d", peerIndex)
	for i, pe := range f.peers {
		if pe.PeerIndex == peerIndex {
			f.peers = append(f.peers[:i], f.peers[i+1:]...)
			if f.alreadyGone {
				return vppapi.APIError{Op: "wireguard_peer_remove", Retval: -6}
			}
			return nil
		}
	}
	return vppapi.APIError{Op: "wireguard_peer_remove", Retval: -6}
}

func (f *fakeVPP) Peers(ctx context.Context) ([]vppapi.WgPeer, error) {
	f.log("Peers")
	return append([]vppapi.WgPeer(nil), f.peers...), nil
}

func (f *fakeVPP) AddPath(ctx context.Context, p vppapi.PathParams) error {
	segs := make([]string, 0, len(p.Segments))
	for _, s := range p.Segments {
		segs = append(segs, fmt.Sprintf("%s:%d", s.IP, s.Port))
	}
	f.log("AddPath %d hmac=%d [%s]", p.PathID, p.HmacID, strings.Join(segs, ","))
	if _, ok := f.keys[p.HmacID]; !ok {
		// 真插件就是这么拒的：路径引用了一个没装过的 hmac_id。
		return fmt.Errorf("vppapi: sr_wg_path_add_del 失败，hmac_id=%d 没装过密钥", p.HmacID)
	}
	// 原地替换（实测）
	f.paths[p.PathID] = vppapi.PathInfo{PathID: p.PathID, HmacID: p.HmacID,
		Active: p.Active, Segments: append([]vppapi.Segment(nil), p.Segments...)}
	return nil
}

func (f *fakeVPP) DelPath(ctx context.Context, pathID uint16) error {
	f.log("DelPath %d", pathID)
	if _, ok := f.paths[pathID]; !ok {
		// 实测语义：sr_path_del 找不到就回 -1，handler 翻成
		// VNET_API_ERROR_NO_SUCH_ENTRY（vpp-plugin/sr_wg_api.c 的 sr_wg_path_add_del handler）。
		return vppapi.APIError{Op: "sr_wg_path_add_del", Retval: -6}
	}
	delete(f.paths, pathID)
	// 级联解绑（实测，且这一步 retval 静默为 0）
	for sw, t := range f.tunnels {
		if t.PathID == pathID {
			delete(f.tunnels, sw)
		}
	}
	return nil
}

func (f *fakeVPP) SetTunnel(ctx context.Context, b vppapi.TunnelBinding) error {
	f.log("SetTunnel iface=%d path=%d ep=%s:%d", b.SWIfIndex, b.PathID, b.EndpointIP, b.DstPort)
	if _, ok := f.paths[b.PathID]; !ok {
		// sr_wg4_encap.c 的 sr_wg_tunnel_set_path：绑一条不存在的路径回 NO_SUCH_ENTRY。
		return vppapi.APIError{Op: "sr_wg_tunnel_set_path", Retval: -6}
	}
	// 配置侧的键仍是 sw_if_index：**每个接口只有一条**，第二次写会把第一条覆盖掉。
	// 这是 ErrSingleBind 那个限制在假数据面上的忠实复刻，不许"顺手"改成多槽。
	// 收包侧的键是 (endpoint, dst_port)：别的接口已经占着这个身份就 -116，
	// 同一条 bind 自己改 endpoint/端口不算抢（下面 own 的判据就是这个）。
	if b.EndpointIP != nil {
		for sw, t := range f.tunnels {
			if sw != b.SWIfIndex && t.EndpointIP.Equal(b.EndpointIP) && t.DstPort == b.DstPort {
				return vppapi.APIError{Op: "sr_wg_tunnel_set_path", Retval: -116}
			}
		}
	}
	f.tunnels[b.SWIfIndex] = vppapi.TunnelInfo{
		SWIfIndex: b.SWIfIndex, PathID: b.PathID, EndpointIP: b.EndpointIP,
		DstPort: b.DstPort, Effective: true,
	}
	return nil
}

func (f *fakeVPP) ClearTunnel(ctx context.Context, swIfIndex uint32) error {
	f.log("ClearTunnel %d", swIfIndex)
	if _, ok := f.tunnels[swIfIndex]; !ok {
		return vppapi.APIError{Op: "sr_wg_tunnel_set_path", Retval: -6}
	}
	delete(f.tunnels, swIfIndex)
	return nil
}

func (f *fakeVPP) Tunnels(ctx context.Context, swIfIndex uint32) ([]vppapi.TunnelInfo, error) {
	f.log("Tunnels %d", swIfIndex)
	out := make([]vppapi.TunnelInfo, 0, len(f.tunnels))
	for _, t := range f.tunnels {
		if swIfIndex == ^uint32(0) || t.SWIfIndex == swIfIndex {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeVPP) Observations(ctx context.Context) ([]vppapi.Observation, error) {
	f.log("Observations")
	return append([]vppapi.Observation(nil), f.obs...), nil
}

func (f *fakeVPP) ClearObservations(ctx context.Context) error {
	f.log("ClearObservations")
	f.obs = nil
	return nil
}

func (f *fakeVPP) callWith(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeVPP) wroteAnything() bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "SetPKey") || strings.HasPrefix(c, "AddPath") ||
			strings.HasPrefix(c, "SetTunnel") || strings.HasPrefix(c, "AddPeer") {
			return true
		}
	}
	return false
}

// peerOn 在**别的**接口上装一条 peer，模拟操作员手配的 wg0 或同机另一个 agent。
func (f *fakeVPP) peerOn(swIfIndex uint32, pub []byte, ep string, port uint16) {
	ip := net.ParseIP(ep).To4()
	if ip == nil {
		panic("peerOn 的地址不是 IPv4: " + ep)
	}
	f.peers = append(f.peers, vppapi.WgPeer{
		PeerIndex: f.nextPi, PublicKey: append([]byte(nil), pub...),
		EndpointIP: ip, Port: port, Keepalive: 25, SWIfIndex: swIfIndex,
		AllowedIPs: []vppapi.Prefix{{IP: net.IPv4(192, 168, 99, 200), Len: 32}},
	})
	f.nextPi++
}

// canonPrefixesForTest 把写进来的前缀归成"VPP 会回给我的样子"（4 字节 + ≤/32）。
// 真 VPP 就是这么归的（实测 dump 回来 allowed-ips 是 4 字节 /32），假数据面必须
// 同样归一，否则"写入用了 v4-mapped、读回却是 4 字节"这一路永远测不到。
func canonPrefixesForTest(pres []vppapi.Prefix) []vppapi.Prefix {
	out := make([]vppapi.Prefix, len(pres))
	for i, pre := range pres {
		if v4 := pre.IP.To4(); v4 != nil {
			if pre.Len > 32 {
				pre.Len = 32
			}
			pre.IP = v4
		}
		out[i] = pre
	}
	return out
}

// ---------------- 构造 ----------------

// encapIfIndex 是本测试假数据面里的"underlay 出口接口"号。
const encapIfIndex = 5

func testConfig(t *testing.T, f DataPlane, pki *testPKI) LinkConfig {
	t.Helper()
	return LinkConfig{
		VPP: f, Tenant: "t1", CAPool: pki.pool,
		Instance: 7, ListenPort: 51820,
		SrcIP: net.IPv4(10, 10, 1, 2), PrivateKey: mustWGKey(t),
		PathIDBase: 1000, EncapIfIndex: encapIfIndex,
	}
}

func newTestLink(t *testing.T, f *fakeVPP) (*Link, *testPKI) {
	t.Helper()
	pki := newTestPKI(t)
	l, err := NewLink(testConfig(t, f, pki))
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	return l, pki
}

// mustWGKey 是一把随机的 X25519 私钥：任意 32 字节都合法，VPP 那边也一样。
func mustWGKey(t *testing.T) []byte {
	t.Helper()
	return randomBytes(t, keybinding.X25519PubLen)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// peerMaterial 造一个"合法对端"：自己的 Ed25519 身份密钥 + 一张本租户 CA 签的
// 证书 + 由身份密钥签出来的 WG 公钥担保。三件必须配套，否则验身份那关就过不去 ——
// 这正是想要的：测试里的通过必须是真的通过。
func peerMaterial(t *testing.T, pki *testPKI, tenant string, node uint16) (certPEM string, binding *keybinding.Binding, wgPub []byte) {
	t.Helper()
	idPub, idPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wgPub = randomBytes(t, keybinding.X25519PubLen)
	b, err := keybinding.Sign(deviceSigner{k: idPriv}, tenant, node, 1, wgPub)
	if err != nil {
		t.Fatal(err)
	}
	return pki.issueDevice(t, idPub, tenant, node), &b, wgPub
}

func target(node uint16, certPEM string, b *keybinding.Binding, observed string, verified bool, reflector string) PeerTarget {
	return PeerTarget{
		NodeID: node, Tenant: "t1", CertPEM: certPEM, Binding: b,
		ObservedAddr: observed, ObservedVerified: verified, ReflectorAddr: reflector,
	}
}

func openLink(t *testing.T, l *Link) {
	t.Helper()
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatalf("InstallKeys: %v", err)
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// ---------------- 测试 ----------------

func TestNewLinkRejectsBadInputs(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	base := testConfig(t, f, pki)
	if _, err := NewLink(base); err != nil {
		t.Fatalf("合法配置不该报错: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*LinkConfig)
	}{
		{"没有 DataPlane", func(c *LinkConfig) { c.VPP = nil }},
		{"没有租户", func(c *LinkConfig) { c.Tenant = "" }},
		{"没有 CA 池", func(c *LinkConfig) { c.CAPool = nil }},
		{"端口 0", func(c *LinkConfig) { c.ListenPort = 0 }},
		{"vpn 端口与 transit 端口相同", func(c *LinkConfig) { c.TransitPort = c.ListenPort }},
		{"没给 underlay 出口接口", func(c *LinkConfig) { c.EncapIfIndex = 0 }},
		{"私钥长度不对", func(c *LinkConfig) { c.PrivateKey = make([]byte, 31) }},
		// 全零私钥在 Go 里合法（实测），推出来的公钥是定值：静默用它入网等于
		// 把身份交出去，所以这一条必须由本层拦住。
		{"私钥全零", func(c *LinkConfig) { c.PrivateKey = make([]byte, 32) }},
		{"源地址是 IPv6", func(c *LinkConfig) { c.SrcIP = net.ParseIP("fe80::1") }},
	}
	for _, tc := range cases {
		cfg := base
		tc.mut(&cfg)
		if _, err := NewLink(cfg); err == nil {
			t.Errorf("%s：应当报错", tc.name)
		}
	}
}

func TestOpenCreatesThenAdopts(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	adopted, err := l.Open(ctx)
	if err != nil || adopted {
		t.Fatalf("第一次 Open 应该是新建: adopted=%v err=%v", adopted, err)
	}
	if got := f.callWith("CreateInterface"); len(got) != 1 {
		t.Errorf("建接口一次就够，实际 %v", got)
	}
	// 建接口会抢走 UDP 挂点，所以 CreateInterface 之后**紧跟着**必须把本条隧道的
	// 端口登记进 vpn 集合、再重挂整组 —— 少这一步的表现为"隧道配好了但一个包都
	// 进不来"，而且计数器上只有 encap_bypass，不会报错。
	i := indexOfCallPrefix(f.calls, "CreateInterface")
	if i < 0 || i+2 >= len(f.calls) ||
		f.calls[i+1] != "AddPort kind=0 port=51820" ||
		f.calls[i+2] != "RebindPorts" {
		t.Errorf("建接口之后没立刻登记端口并重挂端口集合，调用序列: %v", f.calls)
	}
	// encap 只能开在 **underlay 出口**上：开在 WG 接口上会返回成功却一个包都不改写
	// （wg4-output-tun 不在 feature 链里），所以这里断言的是"哪个号"，不是"有没有"。
	if !f.encap[encapIfIndex] {
		t.Errorf("没在 underlay 出口接口 %d 上开 encap: %v", encapIfIndex, f.encap)
	}
	if f.encap[l.SWIfIndex()] {
		t.Errorf("encap 开到了 WG 接口 %d 上：那是静默无效的挂点", l.SWIfIndex())
	}

	// 模拟 agent 重启：同一个 VPP、同一把落盘的 WG 私钥，再来一次。
	l2 := restartLink(t, l)
	if err := l2.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	rebindsBefore := f.rebinds
	adopted, err = l2.Open(ctx)
	if err != nil || !adopted {
		t.Fatalf("第二次 Open 应该认回接口: adopted=%v err=%v", adopted, err)
	}
	if n := len(f.callWith("CreateInterface")); n != 0 {
		t.Errorf("重启后又建了一次接口（%d 次）—— 撞 -31 只是早晚的事", n)
	}
	if l2.SWIfIndex() != l.SWIfIndex() {
		t.Errorf("认回了别的接口: %d vs %d", l2.SWIfIndex(), l.SWIfIndex())
	}
	// 认回分支也要补挂点：VPP 重启或别人建接口都会把这条隧道的端口抢回 wg4-input
	// （顺序约束记在 vpp-plugin/sr_wg.c 的 sr_wg_ports_rebind 注释里），
	// 症状是"隧道在、没流量"。AddPort 是幂等的，认回时再来一次正好把集合补齐。
	if f.rebinds == rebindsBefore {
		t.Errorf("认回接口后没重挂 UDP 端口（rebinds 仍是 %d）: %v", rebindsBefore, f.calls)
	}
	if len(f.callWith("AddPort kind=0 port=51820")) == 0 {
		t.Errorf("认回接口后没把监听端口登记回 vpn 集合: %v", f.calls)
	}
	if !f.encap[encapIfIndex] {
		t.Errorf("认回后 underlay 上的 encap 没补: %v", f.encap)
	}
	if !f.encapOnUnderlayOnly() {
		t.Errorf("encap 只应开在 underlay %d 上: %v", encapIfIndex, f.encap)
	}
}

// transit 端口由控制器下发（#17）：非 0 就必须真的挂上 sr-wg4-transit —— 只把端口
// 加进集合不够，插件的 transit_enabled 是全局开关；0 表示这台 CPE 只做端点，
// 一个包都不中转。
func TestOpenEnablesTransitOnlyWhenConfigured(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		transitPort uint16
		wantSet     bool
	}{
		{"控制器给了 transit 端口", 51821, true},
		{"纯端点（没给）", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVPP()
			pki := newTestPKI(t)
			cfg := testConfig(t, f, pki)
			cfg.TransitPort = tc.transitPort
			l, err := NewLink(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Open(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.wantSet {
				if f.transitPort != tc.transitPort {
					t.Errorf("transit 端口没挂上：SetTransit 收到 %d，期望 %d", f.transitPort, tc.transitPort)
				}
				if n := len(f.callWith("SetTransit port=51821")); n != 1 {
					t.Errorf("SetTransit 应调用 1 次，实际 %d：%v", n, f.calls)
				}
			} else if f.transitPort != 0 || len(f.callWith("SetTransit")) != 0 {
				t.Errorf("没配 transit 端口却打开了中转: %v", f.calls)
			}
		})
	}
}

func (f *fakeVPP) encapOnUnderlayOnly() bool {
	for idx, on := range f.encap {
		if on && idx != encapIfIndex {
			return false
		}
	}
	return true
}

// 接口在 dump 之后才"被别人"建出来（操作员手敲 / 第二个 agent）：撞 -31 时不能放弃，
// 要重读一次并认回 —— 否则 agent 在这台机器上永远起不来。
func TestOpenAdoptsAfterMinus31(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	cfg := testConfig(t, f, pki)
	k, err := ecdh.X25519().NewPrivateKey(cfg.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	hidden := &hideFirstDump{
		fakeVPP: f,
		iface: vppapi.InterfaceInfo{UserInstance: cfg.Instance, SWIfIndex: 42,
			Port: cfg.ListenPort, SrcIP: cfg.SrcIP.To4(), PublicKey: k.PublicKey().Bytes()},
	}
	l, err := NewLink(func(c LinkConfig) LinkConfig { c.VPP = hidden; return c }(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	adopted, err := l.Open(ctx)
	if err != nil {
		t.Fatalf("撞 -31 之后应当认回，而不是报错: %v", err)
	}
	if !adopted || l.SWIfIndex() != 42 {
		t.Errorf("adopted=%v swIfIndex=%d，期望认回 42", adopted, l.SWIfIndex())
	}
	if hidden.dumpCalls != 2 {
		t.Errorf("读接口表 %d 次：-31 之后必须重读一次", hidden.dumpCalls)
	}
}

// hideFirstDump 让第一次 Interfaces 返回空、之后返回真实表：制造
// "dump 说没有、create 却说实例被占"这个窗口。
type hideFirstDump struct {
	*fakeVPP
	iface     vppapi.InterfaceInfo
	dumpCalls int
}

func (h *hideFirstDump) Interfaces(ctx context.Context, showPrivate bool) ([]vppapi.InterfaceInfo, error) {
	h.dumpCalls++
	if h.dumpCalls == 1 {
		h.log("Interfaces private=%v (藏起来)", showPrivate)
		return nil, nil
	}
	return h.fakeVPP.Interfaces(ctx, showPrivate)
}

func (h *hideFirstDump) CreateInterface(ctx context.Context, p vppapi.InterfaceParams) (uint32, error) {
	// create 之前把那张接口放进真表，于是真表按实测语义回 -31。
	if len(h.ifaces) == 0 {
		h.ifaces = append(h.ifaces, h.iface)
	}
	return h.fakeVPP.CreateInterface(ctx, p)
}

func TestOpenRefusesForeignIdentity(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}
	// 身份目录被清过：私钥是新的，实例号上那张接口还是旧身份。
	l2, _ := newTestLink(t, f)
	_, err := l2.Open(ctx)
	if err == nil {
		t.Fatal("接口公钥不是本机身份却还 Open 成功")
	}
	if !strings.Contains(err.Error(), "公钥") {
		t.Errorf("错误要说清是身份对不上: %v", err)
	}
	if n := len(f.callWith("CreateInterface")); n != 1 {
		t.Errorf("不能靠再建一个接口绕过身份问题（%d 次）", n)
	}
}

func TestOpenRefusesPortMismatch(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}
	// 租户改了 vpn_port：删接口会连带干掉所有 peer，必须留给人决定。
	l2 := restartLinkWith(t, l, func(c *LinkConfig) { c.ListenPort = 51830 })
	if _, err := l2.Open(ctx); err == nil {
		t.Fatal("端口变了却 Open 成功")
	} else if !strings.Contains(err.Error(), "端口") {
		t.Errorf("错误要说清是端口: %v", err)
	}
}

func TestApplyPeerNeedsKeysAndOpen(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	ctx := context.Background()

	if _, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, "")); err == nil {
		t.Error("还没 Open 就该配对端，说明顺序约束没落地")
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, "")); err == nil {
		t.Error("还没装 hop-MAC 密钥就能建路径，插件会直接拒（真机验过）")
	}
	if f.wroteAnything() {
		t.Errorf("失败路径上不该有任何写入: %v", f.calls)
	}
}

func TestInstallKeysRejectsWrongKeyLength(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	err := l.InstallKeys(context.Background(), []Key{{HmacID: 1, Key: make([]byte, 15)}})
	if err == nil || !strings.Contains(err.Error(), "16 字节") {
		t.Errorf("SR_PKEY_LEN 是 16 字节，短了的密钥要当场拦住: %v", err)
	}
}

// 环里多条密钥时选第 0 条是"确定性"，不是"最后一条最新"那种推测；
// 这条测试就是为了让"取哪一条"这个决定写在代码里而不是写在运气里。
func TestInstallKeysPicksFirstHmacID(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{
		{HmacID: 7, Key: randomBytes(t, 16)},
		{HmacID: 8, Key: randomBytes(t, 16)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	if _, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	if got := f.paths[1003].HmacID; got != 7 {
		t.Errorf("路径用了 hmac_id=%d，期望第 0 条（7）", got)
	}
}

func TestApplyPeerAuthenticationGatesEveryWrite(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	before := len(f.calls)

	// 别的 CA 签的证书（链不到我 pin 的这份）
	other := newTestPKI(t)
	cert, b, _ := peerMaterial(t, other, "t1", 4)
	if _, err := l.ApplyPeer(ctx, target(4, cert, b, "10.10.2.4:51820", true, "")); err == nil {
		t.Error("链不到 pin 的 CA 却配成功了")
	}

	// 证书是给别人的（node 5 的证书配 node 4 的身份声明）
	good5, b5, _ := peerMaterial(t, pki, "t1", 5)
	if _, err := l.ApplyPeer(ctx, target(4, good5, b5, "10.10.2.4:51820", true, "")); err == nil {
		t.Error("证书身份与声称身份不一致却配成功了")
	}

	// 担保里的公钥被人换过：证书没错，绑定是用另一把 WG 私钥签的
	cert7, _, _ := peerMaterial(t, pki, "t1", 7)
	swapped, err := keybinding.Sign(deviceSigner{k: mustEd25519(t)}, "t1", 7, 1, randomBytes(t, keybinding.X25519PubLen))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplyPeer(ctx, target(7, cert7, &swapped, "10.10.2.7:51820", true, "")); err == nil {
		t.Error("担保被换过还能配成功，定案 3 就白做了")
	}

	if len(f.calls) != before {
		t.Errorf("身份验不过时一条写入都不该发出去: %v", f.calls[before:])
	}
}

// 跨租户：本机 pin 的 CA 如果同时给 t1/t2 发证（企业里常见的一份租户 CA），
// "证书链验过"就不足以说明"他和我同租户" —— 声称的租户必须等于本机自己的。
func TestApplyPeerRefusesOtherTenant(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	cert, b, _ := peerMaterial(t, pki, "t2", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")
	pt.Tenant = "t2" // 证书与声称一致，但和本机不同租户
	if _, err := l.ApplyPeer(context.Background(), pt); err == nil {
		t.Fatal("跨租户的对端配成功了")
	} else if !strings.Contains(err.Error(), "租户") {
		t.Errorf("错误要说清是租户不符: %v", err)
	}
}

// restartLink 用**同一份配置**再开一个 Link —— 磁盘上的身份没变，这就是重启。
// 用 newTestLink 是不行的：那会生成一把新的 WG 私钥，测出来的是"身份被人换过"。
func restartLink(t *testing.T, l *Link) *Link {
	t.Helper()
	return restartLinkWith(t, l, nil)
}

// restartLinkWith 同上，但可以改一个字段（比如租户换了 vpn_port）。
// 改的是**副本**：Link.cfg 是发布出去的不可变输入，测试不许伸手进去改它。
func restartLinkWith(t *testing.T, l *Link, mut func(*LinkConfig)) *Link {
	t.Helper()
	l.mu.Lock()
	cfg := l.cfg
	l.mu.Unlock()
	if mut != nil {
		mut(&cfg)
	}
	n, err := NewLink(cfg)
	if err != nil {
		t.Fatalf("重启后 NewLink: %v", err)
	}
	return n
}

// indexOfCallPrefix 返回第一条以 prefix 开头的调用记录，没有则 -1。
func indexOfCallPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func mustEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestApplyPeerEndpointPreference(t *testing.T) {
	cases := []struct {
		name     string
		observed string
		verified bool
		want     string // 期望的 path 段地址；"" 表示期望报错
	}{
		// 定案 1：我自己看到的才是回得去的那条路，反射器的映射只负责第一个包。
		{"自己的观测优先", "10.10.9.9:41000", true, "10.10.9.9:41000"},
		{"没观测就用反射器", "", false, "10.10.1.3:51820"},
		// 没过 hop-MAC 校验的观测**不能**拿来配路由：那是明文头 + MAC，
		// 没验过的条目连是谁发的都说不清。
		{"未过校验的观测要报错", "10.10.9.9:41000", false, ""},
		{"坏地址要报错而不是退回反射器", "not-an-address", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVPP()
			l, pki := newTestLink(t, f)
			openLink(t, l)
			cert, b, _ := peerMaterial(t, pki, "t1", 6)
			pt := target(6, cert, b, tc.observed, tc.verified, "10.10.1.3:51820")
			_, err := l.ApplyPeer(context.Background(), pt)
			if tc.want == "" {
				if err == nil {
					t.Fatal("这种情况必须报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyPeer: %v", err)
			}
			got := f.paths[1006]
			if seg := fmt.Sprintf("%s:%d", got.Segments[0].IP, got.Segments[0].Port); seg != tc.want {
				t.Errorf("路径段 = %s，期望 %s", seg, tc.want)
			}
			if tun := f.tunnels[l.SWIfIndex()]; !tun.EndpointIP.Equal(net.ParseIP(strings.Split(tc.want, ":")[0])) {
				t.Errorf("隧道绑定没跟上路径: %+v", tun)
			}
		})
	}
}

// NAT 场景的端口分工：观测到的**映射端口只进 SR 段**，WG peer 的目的地端口
// 永远是本条隧道的 vpn_port。填错的后果不是报错而是静默 —— encap 的 Step 4 只处理
// dst 命中 vpn_ports 集合的包（vpp-plugin/sr_wg4_encap.c），端口填成映射端口时包不
// 加 SR 头直接出去，
// 计数器上是 encap_bypass，看起来"隧道通了"其实根本没走源路由。
func TestApplyPeerSplitsPeerPortFromSegmentPort(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.7.7:42000", true, "")
	if _, err := l.ApplyPeer(context.Background(), pt); err != nil {
		t.Fatal(err)
	}
	if seg := f.paths[1003].Segments[0]; seg.Port != 42000 || !seg.IP.Equal(net.IPv4(10, 10, 7, 7)) {
		t.Errorf("SR 段应当带映射端口 42000: %+v", seg)
	}
	if p := f.peers[0]; p.Port != 51820 {
		t.Errorf("WG peer 端口 = %d，期望 vpn_port 51820（否则 encap 会静默旁路）", p.Port)
	}
	if p := f.peers[0]; !p.EndpointIP.Equal(net.IPv4(10, 10, 7, 7)) {
		t.Errorf("peer 外层地址应仍是观测到的 IP: %+v", p)
	}
}

// 数据面自己说不验，调用方说验了也不算：观测表里那条地址的 Verified 位才是硬证据。
func TestApplyPeerRefusesUnverifiedObservationInDataplane(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	f.obs = []vppapi.Observation{{SrcIP: net.IPv4(10, 10, 7, 7), SrcPort: 42000,
		DstPort: 51820, Verified: false}}
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.7.7:42000", true, "") // 调用方声称 verified=true
	_, err := l.ApplyPeer(context.Background(), pt)
	if err == nil {
		t.Fatal("数据面说这条观测没过 hop-MAC，却还是配上了")
	}
	if n := len(f.callWith("AddPath")); n != 0 {
		t.Errorf("拒绝时不该有任何路径写入（%d 次）: %v", n, f.calls)
	}
	// 反过来：表里查不到（观测老化出表是正常路径）时不该拦，否则定案 1 的
	// "控制器转述自己早先的观测"这条路就断了。
	f.obs = nil
	if _, err := l.ApplyPeer(context.Background(), pt); err != nil {
		t.Errorf("观测已出表时不该拦：那是正常路径: %v", err)
	}
}

// allowed-ips 是常量，不是调用方的输入：定案 1（2026-10-18）把它固定成
// 0.0.0.0/0，因为 wg4-input 会拿它过滤解密后的内层源地址。写对端 /32 会让
// 被转发的终端流量在收端 punt 到 null-node（2026-10-24 实测，见 #26）。
func TestApplyPeerAlwaysSendsWildcardAllowedIPs(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	cert, b, _ := peerMaterial(t, pki, "t1", 2)
	if _, err := l.ApplyPeer(context.Background(),
		target(2, cert, b, "10.10.2.2:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	got := f.peers[0].AllowedIPs
	if len(got) != 1 || got[0].Len != 0 || !got[0].IP.Equal(net.IPv4zero) || len(got[0].IP) != 4 {
		t.Fatalf("peer 的 allowed-ips 必须是唯一的 0.0.0.0/0（4 字节），实际 %+v", got)
	}
}

// 插件的绑定表**配置侧**按 sw_if_index 一槽（sr_wg4_encap.c 的
// sr_wg_tunnel_set_path：命中同接口即复用那条 bind，并把旧的 (endpoint, 端口) 键
// 从 endpoint_path 摘掉）。第二个 peer 会**静默**把第一个人的外层
// 地址换掉，所以这里必须在写之前拒绝，而不是等日志里去猜。
func TestApplyPeerRefusesSecondPeerOnSingleBind(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()

	cert3, b3, _ := peerMaterial(t, pki, "t1", 3)
	if _, err := l.ApplyPeer(ctx, target(3, cert3, b3, "10.10.2.3:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	firstTun := f.tunnels[l.SWIfIndex()]

	cert4, b4, _ := peerMaterial(t, pki, "t1", 4)
	_, err := l.ApplyPeer(ctx, target(4, cert4, b4, "10.10.2.4:51820", true, ""))
	if !errors.Is(err, ErrSingleBind) {
		t.Fatalf("第二个 peer 应当撞 ErrSingleBind，got: %v", err)
	}
	// 关键：拒绝必须是**干净**的 —— 第一个人的路径与绑定一个字都不能动。
	if got := f.tunnels[l.SWIfIndex()]; got.PathID != firstTun.PathID ||
		!got.EndpointIP.Equal(firstTun.EndpointIP) {
		t.Errorf("第二个 peer 被拒之后第一条绑定却被改了: %+v（原本 %+v）", got, firstTun)
	}
	if len(f.peers) != 1 {
		t.Errorf("被拒的 peer 不该留下 peer: %+v", f.peers)
	}
	// 同一个人重复下发**不受**这个限制影响（那是更新，不是覆盖）。
	if _, err := l.ApplyPeer(ctx, target(3, cert3, b3, "10.10.2.3:51820", true, "")); err != nil {
		t.Errorf("重复下发同一个人却被拦了: %v", err)
	}
}

// 两条隧道 = 两个 WG 接口 + 两个监听端口（定案 2：UDP 端口既分功能也分隧道）。
// 这里把两个对端放在**同一个 NAT** 后面，因为外层 IP 相同之后，只有端口能说出
// "这一包属于哪条隧道" —— 那正是绑定键加上 dst_port 才成立的事：改之前第二条
// 绑定会抢走第一条的同 IP 键，前一条隧道静默退化成普通 WG。
func TestTwoTunnelInterfacesEachOwnTheirPort(t *testing.T) {
	f := newFakeVPP()
	a, pki := newTestLink(t, f)
	openLink(t, a)

	b := restartLinkWith(t, a, func(c *LinkConfig) {
		c.Instance = 8
		c.ListenPort = 51830
		c.PathIDBase = 2000
		c.PrivateKey = mustWGKey(t) // 第二条隧道是另一把接口身份密钥
	})
	openLink(t, b)
	ctx := context.Background()

	cert3, b3, _ := peerMaterial(t, pki, "t1", 3)
	if _, err := a.ApplyPeer(ctx, target(3, cert3, b3, "10.10.9.9:41000", true, "")); err != nil {
		t.Fatalf("第一条隧道: %v", err)
	}
	cert4, b4, _ := peerMaterial(t, pki, "t1", 4)
	if _, err := b.ApplyPeer(ctx, target(4, cert4, b4, "10.10.9.9:41001", true, "")); err != nil {
		t.Fatalf("第二条隧道（端口 51830）配不上: %v", err)
	}

	// 1) 两个端口都必须在 vpn 集合里：少一个就等于那条隧道没人给它加 SR 头。
	for _, port := range []uint16{51820, 51830} {
		var found bool
		for _, p := range f.ports {
			if p.Kind == vppapi.PortKindVPN && p.Port == port {
				found = true
			}
		}
		if !found {
			t.Errorf("vpn 端口集合里没有 %d: %+v", port, f.ports)
		}
	}

	// 2) 两条绑定都得活着，各自带自己的端口。
	bindA, bindB := f.tunnels[a.SWIfIndex()], f.tunnels[b.SWIfIndex()]
	if bindA.PathID != 1003 || bindA.DstPort != 51820 {
		t.Errorf("第一条隧道绑定被改了: %+v（期望 path 1003 / 端口 51820）", bindA)
	}
	if bindB.PathID != 2004 || bindB.DstPort != 51830 {
		t.Errorf("第二条隧道绑定不对: %+v（期望 path 2004 / 端口 51830）", bindB)
	}

	// 3) peer 各打自己的 vpn_port，SR 段各打自己的映射端口。
	for _, tc := range []struct {
		iface             uint32
		peerPort, segPort uint16
		path              uint16
	}{
		{a.SWIfIndex(), 51820, 41000, 1003},
		{b.SWIfIndex(), 51830, 41001, 2004},
	} {
		var found *vppapi.WgPeer
		for i := range f.peers {
			if f.peers[i].SWIfIndex == tc.iface {
				found = &f.peers[i]
			}
		}
		if found == nil {
			t.Fatalf("接口 %d 上没有 peer: %+v", tc.iface, f.peers)
		}
		if found.Port != tc.peerPort {
			t.Errorf("接口 %d 的 peer 端口 = %d，期望本条隧道的 vpn_port %d",
				tc.iface, found.Port, tc.peerPort)
		}
		if seg := f.paths[tc.path].Segments[0]; seg.Port != tc.segPort {
			t.Errorf("path %d 的段端口 = %d，期望映射端口 %d", tc.path, seg.Port, tc.segPort)
		}
	}
}

// 两条隧道用了同一个监听端口（第二个 agent 配置抄错、或控制器把同一个 vpn_port
// 发给了两条隧道）：绑定身份 (endpoint, 端口) 撞车，插件直接 -116。错误必须点名
// "身份被别的接口占了"，不能让人以为是并发问题去重试。
func TestSecondTunnelOnSamePortIsRefused(t *testing.T) {
	f := newFakeVPP()
	a, pki := newTestLink(t, f)
	openLink(t, a)
	b := restartLinkWith(t, a, func(c *LinkConfig) {
		c.Instance = 8
		c.PathIDBase = 2000
		c.PrivateKey = mustWGKey(t)
	}) // 端口**故意**没改，仍是 51820
	openLink(t, b)
	ctx := context.Background()

	cert3, b3, _ := peerMaterial(t, pki, "t1", 3)
	if _, err := a.ApplyPeer(ctx, target(3, cert3, b3, "10.10.2.3:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	first := f.tunnels[a.SWIfIndex()]

	cert4, b4, _ := peerMaterial(t, pki, "t1", 4)
	_, err := b.ApplyPeer(ctx, target(4, cert4, b4, "10.10.2.3:51820", true, ""))
	if err == nil {
		t.Fatal("两条隧道抢同一个 (外层 IP, 端口) 身份，插件会回 -116，这里不该当成功")
	}
	if !strings.Contains(err.Error(), "隧道身份") {
		t.Errorf("错误要说清是身份撞车: %v", err)
	}
	if got := f.tunnels[a.SWIfIndex()]; got.PathID != first.PathID || got.DstPort != first.DstPort {
		t.Errorf("被拒之后第一条隧道绑定却被改了: %+v（原本 %+v）", got, first)
	}
}

func TestApplyPeerInstallsThenIdempotentThenReplaces(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, wgPub := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")

	change, err := l.ApplyPeer(ctx, pt)
	if err != nil || change != PeerInstalled {
		t.Fatalf("第一次: change=%q err=%v", change, err)
	}
	if len(f.peers) != 1 || hex.EncodeToString(f.peers[0].PublicKey) != hex.EncodeToString(wgPub) {
		t.Fatalf("peer 装的不是验过的那把公钥: %+v", f.peers)
	}
	// 写死 25，不拿代码里的常量自比：常量被人改成 0（=没有探针）时这条必须红。
	if f.peers[0].Keepalive != 25 {
		t.Errorf("keepalive=%d，期望 25（定案 1 的探针间隔，也是 NAT UDP 映射超时下的安全值）",
			f.peers[0].Keepalive)
	}

	// 同一个人再推一次（watch 的 update、重连后的 snapshot 都会这样）
	f.calls = nil
	change, err = l.ApplyPeer(ctx, pt)
	if err != nil || change != PeerUnchanged {
		t.Fatalf("重复下发应该判成 unchanged: change=%q err=%v", change, err)
	}
	if n := len(f.callWith("RemovePeer")); n != 0 {
		t.Errorf("没变化却删了 peer（%d 次）—— 每次心跳都重连一次隧道就废了", n)
	}
	// 路径与隧道仍然重发是**有意的**（声明式、原地替换），但绝不能删路径：
	// DelPath 会级联解绑隧道。
	if n := len(f.callWith("DelPath")); n != 0 {
		t.Errorf("unchanged 分支里出现了 DelPath（%d 次）: %v", n, f.calls)
	}
	if len(f.callWith("AddPath")) != 1 || len(f.callWith("SetTunnel")) != 1 {
		t.Errorf("路径/隧道应各重发一次: %v", f.calls)
	}

	// NAT 重绑：观测到的地址变了
	f.calls = nil
	pt.ObservedAddr = "10.10.7.7:42000"
	change, err = l.ApplyPeer(ctx, pt)
	if err != nil || change != PeerReplaced {
		t.Fatalf("换地址应判成 replaced: change=%q err=%v", change, err)
	}
	if len(f.peers) != 1 {
		t.Fatalf("换地址之后 peer 应该还是 1 条，实际 %d 条", len(f.peers))
	}
	if !f.peers[0].EndpointIP.Equal(net.IPv4(10, 10, 7, 7)) {
		t.Errorf("peer 还停在旧地址: %+v", f.peers[0])
	}
	// 顺序必须是先删后加：直接 add 会撞 -116（公钥去重，跨接口也一样）。
	removed := indexOfCallPrefix(f.calls, "RemovePeer")
	added := indexOfCallPrefix(f.calls, "AddPeer")
	if removed < 0 || added < 0 || removed > added {
		t.Errorf("没有先删后加: %v", f.calls)
	}
	// 换地址时路径与隧道必须**成对**跟上（实测：绑定的 endpoint 不跟着段走）
	if seg := f.paths[1003].Segments[0]; !seg.IP.Equal(net.IPv4(10, 10, 7, 7)) || seg.Port != 42000 {
		t.Errorf("路径没跟着换: %+v", seg)
	}
	if tun := f.tunnels[l.SWIfIndex()]; !tun.EndpointIP.Equal(net.IPv4(10, 10, 7, 7)) {
		t.Errorf("隧道绑定没跟着换: %+v", tun)
	}
}

// keepalive 与 allowed_ips 的比较必须真的参与"变没变"的判断：只比 endpoint 的实现
// 会让"改了探针间隔/加了 overlay 网段"被当成没变，症状是配置停在旧值上而日志一片绿。
func TestApplyPeerNoticesKeepaliveAndPrefixChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*vppapi.WgPeer)
	}{
		{"keepalive 被数据面改掉", func(p *vppapi.WgPeer) { p.Keepalive = 1 }},
		{"allowed_ips 被改成对端 /32", func(p *vppapi.WgPeer) {
			p.AllowedIPs = []vppapi.Prefix{{IP: net.IPv4(192, 168, 99, 250), Len: 32}}
		}},
		{"table_id 被改掉", func(p *vppapi.WgPeer) { p.TableID = 7 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVPP()
			l, pki := newTestLink(t, f)
			openLink(t, l)
			ctx := context.Background()
			cert, b, _ := peerMaterial(t, pki, "t1", 3)
			if _, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, "")); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&f.peers[0])
			change, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, ""))
			if err != nil {
				t.Fatal(err)
			}
			if change != PeerReplaced {
				t.Errorf("数据面和期望不一致却判成 %q（期望 replaced）", change)
			}
			if f.peers[0].Keepalive != 25 || f.peers[0].TableID != 0 {
				t.Errorf("没按期望值重装: %+v", f.peers[0])
			}
		})
	}
}

// 常量写进去是 16 字节的 net.IPv4zero，VPP dump 回来是 4 字节 + /0：不归一的话
// 每次 watch 推送都会被判成"变了"，隧道被反复删了重装。反过来，数据面上若有
// 历史遗留的 /32，比较必须判出差异并重装成常量。
func TestApplyPeerNormalizesWildcardAllowedIPs(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")
	if _, err := l.ApplyPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	if got := f.peers[0].AllowedIPs; len(got) != 1 || got[0].Len != 0 || len(got[0].IP) != 4 {
		t.Errorf("写进数据面的前缀没归一: %+v", got)
	}
	if _, err := l.ApplyPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callWith("RemovePeer")); n != 0 {
		t.Errorf("同一个常量被判成变化，重装了 peer（%d 次删除）", n)
	}

	// 旧的 /32（本 bug 的历史形态）必须被纠正，而不是被"多重集相等"放过。
	f.peers[0].AllowedIPs = []vppapi.Prefix{{IP: net.IPv4(192, 168, 99, 3), Len: 32}}
	change, err := l.ApplyPeer(ctx, pt)
	if err != nil {
		t.Fatal(err)
	}
	if change != PeerReplaced {
		t.Errorf("数据面还是 /32，却判成 %q（期望 replaced）", change)
	}
	if got := f.peers[0].AllowedIPs; len(got) != 1 || got[0].Len != 0 {
		t.Errorf("没有纠正回常量: %+v", got)
	}
}

func TestApplyPeerToleratesAlreadyGonePeer(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")
	if _, err := l.ApplyPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	// 让 reconcile 判定"地址变了、要重装"，同时让删除这一步回 -6：
	// 上一次崩在中途时 VPP 里可能已经没有这条 peer 了，那属于正常分支，
	// 不能让整个下发失败（其它错误码则必须往上报，见 TestIsNoSuchEntry…）。
	f.peers[0].EndpointIP = net.IPv4(10, 10, 2, 99)
	f.alreadyGone = true
	change, err := l.ApplyPeer(ctx, pt)
	if err != nil {
		t.Fatalf("删除回 -6 时 reconcile 不该失败: %v", err)
	}
	if change != PeerReplaced {
		t.Errorf("change=%q，期望 replaced", change)
	}
	if len(f.peers) != 1 || !f.peers[0].EndpointIP.Equal(net.IPv4(10, 10, 2, 3)) {
		t.Fatalf("没把 peer 按新地址装回来: %+v", f.peers)
	}
}

// 公钥已经被**别的接口**占了（操作员手配的 wg0，或同机另一个 agent）：VPP 会一直
// 回 -116，本层删别人的 peer 就是拆别人的隧道，所以只能把冲突说清楚。
func TestApplyPeerReportsForeignPeerHolder(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, wgPub := peerMaterial(t, pki, "t1", 3)

	// 本接口上没有，但 wg0（接口 2）上已经有同一把公钥。
	f.peerOn(2, wgPub, "10.10.2.3", 51820)

	_, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, ""))
	if err == nil {
		t.Fatal("公钥已被别的接口占用却报成功 —— VPP 实际会回 -116")
	}
	if !strings.Contains(err.Error(), "接口 2") {
		t.Errorf("错误要点名占用的那张接口: %v", err)
	}
	// 冲突处理不能动别人的 peer：数据面里那条还在接口 2 上。
	if len(f.peers) != 1 || f.peers[0].SWIfIndex != 2 {
		t.Errorf("把别的接口上的 peer 删了或加了: %+v", f.peers)
	}
}

func TestDropPeerRemovesPeerThenPath(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")
	if _, err := l.ApplyPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := l.DropPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	if len(f.peers) != 0 {
		t.Errorf("peer 没删掉: %+v", f.peers)
	}
	if _, ok := f.paths[1003]; ok {
		t.Errorf("路径没删掉")
	}
	if _, ok := f.tunnels[l.SWIfIndex()]; ok {
		t.Errorf("隧道绑定还在（DelPath 应当级联清掉）")
	}
	r, d := indexOfCallPrefix(f.calls, "RemovePeer"), indexOfCallPrefix(f.calls, "DelPath")
	if r < 0 || d < 0 || r > d {
		t.Errorf("应先删 peer 再删路径，实际序列: %v", f.calls)
	}
	// 再撤一次（对端已经不存在）：不能因为 -6 就报错
	if err := l.DropPeer(ctx, pt); err != nil {
		t.Errorf("重复撤销应容忍 -6: %v", err)
	}
}

// DropPeer 收 PeerTarget 而不是裸公钥字节：公钥必须由证书链证明。
// 这条测试就是为了让"改成收字节"这个倒退立刻红。
func TestDropPeerRequiresAuthenticatedTarget(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	if _, err := l.ApplyPeer(ctx, target(3, cert, b, "10.10.2.3:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	f.calls = nil

	// 别的 CA 的证书：链不过，就撤不了任何人。
	other := newTestPKI(t)
	badCert, badB, _ := peerMaterial(t, other, "t1", 3)
	if err := l.DropPeer(ctx, target(3, badCert, badB, "", false, "")); err == nil {
		t.Fatal("身份验不过的撤销请求成功了")
	}
	if n := len(f.callWith("RemovePeer")); n != 0 {
		t.Errorf("验不过却删了 peer（%d 次）: %v", n, f.calls)
	}
}

// 手工配置（或第二张接口）也在引用这条路径时，DelPath 会把它一起级联摘掉且 retval=0
// （实测），那不是本 agent 的权利 —— 只解自己的绑定，路径留着。
func TestDropPeerKeepsPathCitedByOtherInterface(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")
	if _, err := l.ApplyPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	// 操作员把另一张接口（sw_if_index 9）也绑到了这条路径上。
	f.tunnels[9] = vppapi.TunnelInfo{SWIfIndex: 9, PathID: 1003,
		EndpointIP: net.IPv4(10, 10, 2, 3), Effective: true}

	if err := l.DropPeer(ctx, pt); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.paths[1003]; !ok {
		t.Errorf("把别人还在用的路径删了（DelPath 会级联摘掉接口 9 的绑定）")
	}
	if _, ok := f.tunnels[9]; !ok {
		t.Errorf("接口 9 的手工绑定被牵连: %+v", f.tunnels)
	}
	if _, ok := f.tunnels[l.SWIfIndex()]; ok {
		t.Errorf("自己这条绑定没解开（peer 已删，留着会指向不存在的路径）")
	}
	if n := len(f.callWith("DelPath")); n != 0 {
		t.Errorf("出现了 DelPath（%d 次）: %v", n, f.calls)
	}
	if len(f.peers) != 0 {
		t.Errorf("peer 没删: %+v", f.peers)
	}
}

func TestPathIDBaseKeepsManualConfigsAway(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	cert, b, _ := peerMaterial(t, pki, "t1", 1)
	if _, err := l.ApplyPeer(context.Background(), target(1, cert, b, "10.10.2.1:51820", true, "")); err != nil {
		t.Fatal(err)
	}
	// node_id=1 落到 path_id=1001：手敲 CLI 习惯从 1 开始编号，而路径表是全局的，
	// 直接拿 node_id 当 path_id 会把操作者手配的 path_id=1 覆盖掉。
	if _, ok := f.paths[1001]; !ok {
		t.Errorf("path_id 没避开 1: %v", f.paths)
	}
	if _, ok := f.paths[1]; ok {
		t.Errorf("覆盖了手工配置用的 path_id=1")
	}
	if _, err := l.PathID(65535); err == nil {
		t.Error("base + node_id 越界必须报错，不能回绕")
	}
}

// watch 流与心跳重连会并发推同一个人；数据面的绑定表只有一个槽，
// 交错写出来的结果取决于调度顺序。串行化之后第二次必须判成 unchanged。
func TestConcurrentApplyPeerOfSamePeerIsSerialized(t *testing.T) {
	f := newFakeVPP()
	l, pki := newTestLink(t, f)
	openLink(t, l)
	ctx := context.Background()
	cert, b, _ := peerMaterial(t, pki, "t1", 3)
	pt := target(3, cert, b, "10.10.2.3:51820", true, "")

	var wg sync.WaitGroup
	results := make([]PeerChange, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = l.ApplyPeer(ctx, pt)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发下发 %d 号失败: %v", i, err)
		}
	}
	// 一个装了、一个发现已经是这个样子；两个都撞 -116 或都判 changed 都是回归。
	if !((results[0] == PeerInstalled && results[1] == PeerUnchanged) ||
		(results[1] == PeerInstalled && results[0] == PeerUnchanged)) {
		t.Errorf("并发结果 %q/%q，期望一个 installed 一个 unchanged（数据面 peer 共 %d 条）",
			results[0], results[1], len(f.peers))
	}
	if len(f.peers) != 1 {
		t.Errorf("装出多条 peer: %+v", f.peers)
	}
}

func TestIsNoSuchEntryOnlyAcceptsMinus6(t *testing.T) {
	if !isNoSuchEntry(vppapi.APIError{Op: "wireguard_peer_remove", Retval: -6}) {
		t.Error("-6 应当认出来")
	}
	// 其它错误码一律不能当"已经不在了"，否则真正的故障会被这个便利逻辑吞掉。
	for _, err := range []error{
		vppapi.APIError{Op: "x", Retval: -116},
		vppapi.APIError{Op: "x", Retval: -31},
		vppapi.APIError{Op: "x", Retval: -7},
		errors.New("别的错误"),
	} {
		if isNoSuchEntry(err) {
			t.Errorf("%v 不该被当成不存在", err)
		}
	}
	if !isAlreadyRegistered(vppapi.APIError{Op: "wireguard_interface_create", Retval: -31}) {
		t.Error("-31 应当认出来（实例号被占）")
	}
}
