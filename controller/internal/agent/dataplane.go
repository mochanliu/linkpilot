package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// 这一层是"CPE 的数据面"：控制器说"有个对端"，这里就把它变成 VPP 里的三样东西
// —— 一条 SR 路径、一个隧道绑定、一个 WireGuard peer。
//
// 七条规则贯穿全文件，都能追溯到实测或插件源码，而不是设计偏好：
//
//  1. **声明式，且每次读真值**。VPP 才是事实来源，本层不缓存 peer/path 状态，
//     每个动作前先 dump 一遍。缓存换来的那点性能，代价是"agent 以为自己配过了"
//     —— 那种故障的表现是隧道不通而日志干净。
//  2. **下发必须成对**。真机验过：同 path_id 再 AddPath 是原地替换段列表，但隧道
//     绑定记的 endpoint 不会跟着段走（internal/vppapi 的 live gate 锁住了这条）。
//     所以换地址=AddPath+SetTunnel 一起做，少做一个就是"配上了但不通"。
//  3. **绝不先删路径**。DelPath 一条被隧道引用的路径会级联把绑定一起清掉，而且
//     retval=0 完全静默。所以 reconcile 用 AddPath 的原地替换，不用"删了重建"；
//     删路径时还得先看有没有**别人**在引用它。
//  4. **动手前先确认这张绑定表装得下**。插件的绑定表按 sw_if_index 建索引、
//     每个接口只有一条（见 ErrSingleBind），所以并发的 reconcile 与"第二个 peer"
//     都必须显式处理，否则会静默覆盖前一个人的外层地址。
//  5. **一条隧道一个监听端口，端口是集合不是标量**（定案 2）。所以建接口之后
//     必须先把这个端口 AddPort 进 vpn 集合，再 RebindPorts —— 插件的集合里
//     默认只有 51820，51830 那条隧道不登记就永远没人给它加 SR 头。
//  6. **两个 MTU 都由数据面执行，但执行的是"分片"，不是"拒发"**。2026-10-22 实测
//     （node1 `ping size 1400` ⇒ 内层 1436 > wg0 记的 1360）：VPP 在 WG 出口按
//     隧道 MTU 把**内层**切成 1356+100 两片，两片各自独立封 WG 头 + SR 头，外层
//     1452 / 196 都不超 underlay 的 1500 —— 于是 sr-wg4-input 永远只见到完整包
//     （收端 Decap_hmac_fail 增量 0，内层由 ip4-local-full-reassembly 拼回 1436）。
//     它既不回 ICMP frag-needed 也不丢包，所以这个值仍是"预算基准 + 上报 + 让
//     LAN 侧按它配置"，只是不能再当成"数据面不生效的数"。此前记的"wg0 记 1360
//     照样放过 1436"量的是**手建 WG 接口的出生状态**：四个槽全 0 = 没有 MTU
//     记录，无从执行 —— 正是 applyMTUs 必须写一个非 0 值的理由。
//     underlay 那一条才是硬约束：VPP 给 af-packet 口默认记 9000，内核网卡 1500，
//     于是 VPP 不分片、直接把超长外层帧交给 sendto()，内核静默失败，这张网卡从此
//     只收不发。2026-10-18 的带宽基线那次卡死就是这个，本轮做 MTU 测量时又在
//     node2 上复现了一次（host-eth0-tx tx sendto failure ×5）。
//  7. **写完要读回来对一遍**。retval=0 在这条链上只说明"请求被收下"：encap 挂到
//     隧道接口自己身上照样回成功，MTU 写完了表里却可能是别数（操作员手改同一台
//     节点是明确支持的用法）。所以 Open/adopt 结尾用 core 的 sw_interface_dump
//     核对出口是否存在、是不是隧道自己、两条 MTU 记录（verifyLanded）。
type DataPlane interface {
	SetPKey(ctx context.Context, hmacID uint16, key []byte) error
	AddPort(ctx context.Context, kind uint8, port uint16) error
	DelPort(ctx context.Context, kind uint8, port uint16) error
	Ports(ctx context.Context) ([]vppapi.PortInfo, error)
	RebindPorts(ctx context.Context) error

	CreateInterface(ctx context.Context, p vppapi.InterfaceParams) (uint32, error)
	Interfaces(ctx context.Context, showPrivate bool) ([]vppapi.InterfaceInfo, error)
	SetInterfaceMTU(ctx context.Context, swIfIndex uint32, l3MTU uint16) error
	DumpInterfaces(ctx context.Context) ([]vppapi.SwInterface, error)
	EnableEncap(ctx context.Context, swIfIndex uint32, enable bool) error

	AddPeer(ctx context.Context, p vppapi.PeerParams) (uint32, error)
	RemovePeer(ctx context.Context, peerIndex uint32) error
	Peers(ctx context.Context) ([]vppapi.WgPeer, error)

	AddPath(ctx context.Context, p vppapi.PathParams) error
	DelPath(ctx context.Context, pathID uint16) error
	SetTunnel(ctx context.Context, b vppapi.TunnelBinding) error
	ClearTunnel(ctx context.Context, swIfIndex uint32) error
	Tunnels(ctx context.Context, swIfIndex uint32) ([]vppapi.TunnelInfo, error)

	Observations(ctx context.Context) ([]vppapi.Observation, error)
	ClearObservations(ctx context.Context) error
}

// 编译期确认自研客户端确实满足这个接口 —— 不满足的话单测会全绿而真机跑不起来。
var _ DataPlane = (*vppapi.Client)(nil)

// defaultKeepaliveSec 是 WG 的 persistent keepalive 间隔。
//
// 定案 1 里这条 keepalive **就是**探针：它让对端的 VPP 看到一个带已知身份的外层
// 包，从而把"我看到的他的地址"记进观测表。所以它不能是 0（实测 VPP 允许 0，
// 但 0 就没有探针了），也要小于常见 NAT 的 UDP 映射超时 —— 25 秒与实验室手工
// 配置一致。
const defaultKeepaliveSec = 25

// 隧道 MTU 的合法区间用契约包里的 wire.MinTunnelMTU / MaxTunnelMTU，本层不另立
// 一套数：控制器下发前与 agent 落地前必须拿同一把尺子，否则"控制器能发、agent
// 拒收"这个缺口会在 enroll 之后才暴露。VPP 自己什么都不校验（实测连 `packet 0`
// 都照单收下），所以校验只能在这两处做。

// Key 是一条 hop-MAC 密钥（对应 wire.PKey 的字节形态；hex 解析留给调用方）。
type Key struct {
	HmacID uint16
	Key    []byte
}

// LinkConfig 是建这条链路所需的全部输入，一次给全，避免"配到一半发现少个参数"。
type LinkConfig struct {
	VPP    DataPlane
	Tenant string
	// CAPool 是"我信任哪个租户 CA"的唯一来源（Identity.CAPool）。对端证书只能
	// 链到它，控制器口头说"这是可信节点"不算数。
	CAPool *x509.CertPool

	// Instance 是 WG 的 user_instance；用 node_id 最合适，因为它 enroll 之后才
	// 存在、租户内唯一、且和 VPP 的 sw_if_index 无关（那个会被别的接口占掉）。
	// 一台设备上多开一条隧道就要再让一个号（例如 node_id*16 + 隧道序号）——
	// VPP 对重复的 user_instance 实测回 -31，不是覆盖。
	Instance uint32
	// ListenPort 是**这条隧道自己的** vpn_port —— 在这个数据面里 WG 的监听端口就是
	// 功能+隧道分用键（定案 2），不是两个独立参数。同一对 CPE 开第二条隧道就是再建
	// 一个 Link，换一个端口；Open 会把它 AddPort 进数据面的 vpn 端口集合。
	ListenPort uint16
	// SrcIP 是 underlay 上发包用的源地址（agent 从本地地址集里选出来那个）。
	// 只能是 IPv4：SR 头里的段地址就是 4 字节。
	SrcIP net.IP
	// PrivateKey 是 agent 自己生成的 32 字节 X25519（定案 3：私钥不出设备，
	// 控制器只中继公钥与担保，永不持有私钥）。
	PrivateKey []byte

	// EncapIfIndex 是 **underlay 出口接口**的 sw_if_index：sr-wg4-encap 这个
	// feature 只有挂在真正的出口网卡上才会改写包。挂在 WG 接口上是**静默无效**的
	// —— wg4-output-tun 不在 ip4-output 的 feature 链里，EnableEncap 照样回 retval=0，
	// 但一个 SR 头都不会加（vpp-plugin/sr_wg_cli.c 的 set sr-wg encap 注释记了同一条）。
	// 这个号仍然由调用方给：本层没有 core 的 FIB dump，算不出"默认路由走哪张口"，
	// 而且出口该由谁定是控制面的事（agent 不猜）。
	//
	// 变化在于**填错不再是静默的**：Open/adopt 末尾的 verifyLanded 会用 core 的
	// sw_interface_dump 读回整张接口表，号不存在、或者正好是隧道自己，都在开跑之前
	// 报错退出。以前这两条只在"顺带写了 underlay MTU"时才被 retval=-2 兜住一半。
	EncapIfIndex uint32

	// TunnelMTU 是这条隧道在 overlay 上允许的 IP 包长度，来自控制器下发的
	// wire.EnrollResponse.TunnelMTU；0 = 用 wire.DefaultTunnelMTU（1360）。
	// 数据面**会**执行它，执行方式是按它切内层分片而不是拒发（见本文件规则 6）；
	// 不执行的是"超长就丢"，所以它同时还是"预算基准 + 上报 + 让 LAN 侧按它配置"。
	// 手建的 WG 接口四个 MTU 槽全是 0，也就是根本没有记录可执行 —— 这一格必须写。
	TunnelMTU uint16

	// UnderlayMTU 是 EncapIfIndex 那张网卡真实的链路 MTU，0 = 调用方不知道。
	// 它做两件事：把 underlay 接口的 MTU 写对（VPP 给 af-packet 口默认记 9000，
	// 真实网卡常是 1500，这个差值就是超长外层帧打到 sendto 上静默失败的原因），
	// 以及把 TunnelMTU 卡在 wire.TunnelMTUBudget 之内。
	//
	// 这个值必须由调用方给，**不能**改成从 VPP 读回来自己定：VPP 那张表记的就是
	// 那个可能错的 9000，读它等于把自己的病灶当体检报告。真值只有内核知道
	// （/sys/class/net/<if>/mtu，test/scripts/setup-node.sh 就是这么取的）。
	// agent 现在会读回记录并核对是否等于写进去的数（verifyLanded），但记录"是不是
	// 网卡的真值"这一层，本层无法知道。
	UnderlayMTU uint16

	KeepaliveSec uint16
	// PathIDBase 把 agent 用的 path_id 整体抬高，避开操作者手敲 CLI 时从 1
	// 开始的那批。用户明确要"我手动敲配置也可以做一些测试"，而路径表是**全局**
	// 的：node_id=1 的 agent 若直接用 1，就会覆盖同节点上手配的 path_id=1。
	//
	// 同一台 CPE 上的**每条隧道都要一个自己的 base**：path_id 只由 (base, 对端
	// node_id) 决定，两条隧道到同一个人若共用 base 就会抢同一条路径 —— 后写的
	// 把前一条的段列表换掉，症状是"第一条隧道忽然不中转了"。
	PathIDBase uint16
}

// Link 是这台 CPE 的数据面句柄：一个 WG 接口，加上它上面的密钥、路径与 peers。
//
// 顺序是硬的：NewLink → InstallKeys → Open → ApplyPeer。InstallKeys 必须在
// Open 之前或之后都行，但**一定在任何 AddPath 之前**：路径引用一个没装过的
// hmac_id 时插件直接拒绝（真机验过），所以密钥不由 ApplyPeer 顺手装，
// 而由 hmacID 未装好时的 ApplyPeer 明确报错。
//
// 一把 mu 同时干两件事：护住下面的可变字段，以及把一次 reconcile 整体串行化。
// 后者不是洁癖 —— 数据面的绑定表每个接口只有一条（ErrSingleBind），两次并发的
// ApplyPeer 交错读写同一条 bind，结果取决于谁先被 VPP 调度，这种"偶尔不通"最难查。
type Link struct {
	dp  DataPlane
	cfg LinkConfig
	// wgPub 由 PrivateKey 推出，NewLink 之后不再变，所以读它不用锁。
	wgPub []byte

	mu        sync.Mutex
	keepalive uint16
	hmacID    uint16
	hmacSet   bool
	swIfIndex uint32
	adopted   bool
	ready     bool
}

// NewLink 校验输入并推出 WG 公钥。校验放在这里而不是等到第一次下发，是因为
// "端口 0""私钥不是 32 字节"这类错误在 enroll 之后、控制器都连上了才炸最难查。
func NewLink(cfg LinkConfig) (*Link, error) {
	switch {
	case cfg.VPP == nil:
		return nil, errors.New("agent: LinkConfig 没有 DataPlane")
	case cfg.Tenant == "":
		return nil, errors.New("agent: LinkConfig 没有租户")
	case cfg.CAPool == nil:
		return nil, errors.New("agent: LinkConfig 没有 CA 池")
	case cfg.ListenPort == 0:
		return nil, errors.New("agent: 监听端口为 0：租户下发的 vpn_port 没拿到")
	case cfg.EncapIfIndex == 0:
		// 0 在 VPP 里是 local0，永远不会是出口，所以当"没填"用正好。
		return nil, errors.New("agent: 没给 underlay 出口接口的 sw_if_index：" +
			"encap 挂在 WG 接口上会启用成功但一个包都不改写，必须先定出口")
	}
	if len(cfg.PrivateKey) != keybinding.X25519PubLen {
		return nil, fmt.Errorf("agent: WG 私钥必须 %d 字节，给了 %d",
			keybinding.X25519PubLen, len(cfg.PrivateKey))
	}
	// 全零私钥在 Go 里是**合法**的（容器里实测：crypto/ecdh 收 32 个零字节不报错，
	// 公钥恒为 2fe57da3…b3b74）。所以"身份文件被截断/清零"这种现场不会自己炸，
	// 只会安静地装出一个谁都能推导出来的身份 —— 必须在这儿拦住。
	if bytes.Equal(cfg.PrivateKey, make([]byte, keybinding.X25519PubLen)) {
		return nil, errors.New("agent: WG 私钥是全零：身份文件被清零了？拒绝用可推导的密钥入网")
	}
	if cfg.SrcIP == nil || cfg.SrcIP.To4() == nil {
		return nil, fmt.Errorf("agent: underlay 源地址必须是 IPv4，给了 %v", cfg.SrcIP)
	}
	// 隧道 MTU：0 = 控制器没下发，落到契约里的默认值。这里就把**算出来的那个值**
	// 存回 cfg，后面 Open/adopt 与任何上报用的都是它，不再各自判一次 0。
	mtu := cfg.TunnelMTU
	if mtu == 0 {
		mtu = wire.DefaultTunnelMTU
	}
	if mtu < wire.MinTunnelMTU || mtu > wire.MaxTunnelMTU {
		return nil, fmt.Errorf("agent: 隧道 MTU %d 出界，可用区间 %d..%d",
			mtu, wire.MinTunnelMTU, wire.MaxTunnelMTU)
	}
	// 知道 underlay MTU 就必须卡住预算，因为超预算**不会报错**：这条口按自己记着的
	// MTU 切内层分片照样封装发走（2026-10-22 实测 overlay 1436 过 1360 的口 → 两片
	// 1356+100，一片不丢），所以配错的人只会看到"隧道通了，大包时好时坏"。
	// 超预算坏在哪：每个内层分片都要再背一份自己的 WG+SR 头，于是外层撑破 underlay，
	// 分片之上又套一层分片；中间设备常丢单片 UDP，而收端若不先重组就派发，本插件
	// 拿到的是半截 SR 头。实验室里 VPP 在 ip4-input 之前把外层拼回来了（这一路
	// Decap_hmac_fail 增 0），公网不保证这一点，所以宁可在这里拒绝。
	//
	// 按**最多段数**算，不是按当前段数：段列表由控制器随时重组，而 MTU 是接口级
	// 的、一条隧道只有一个值。取最坏的那个，长路径就不会在某一天把外层撑破。
	if cfg.UnderlayMTU != 0 {
		if budget := wire.TunnelMTUBudget(cfg.UnderlayMTU, wire.MaxSRSegments); mtu > budget {
			return nil, fmt.Errorf("agent: 隧道 MTU %d 超出预算：underlay %d、最多 %d 段时上限是 %d；"+
				"超出去不会被 VPP 拒绝，只会静默分片（外层还要再分一层），请把 tunnel_mtu 调小或缩短路径",
				mtu, cfg.UnderlayMTU, wire.MaxSRSegments, budget)
		}
	}
	cfg.TunnelMTU = mtu
	k, err := ecdh.X25519().NewPrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("agent: WG 私钥不合法: %w", err)
	}
	// 私钥与源地址各拷一份：调用方给的是从磁盘读出来、可能被复用的切片，
	// 它们被就地改动时，一个正在跑的 agent 不该跟着一起改身份。
	cfg.PrivateKey = append([]byte(nil), cfg.PrivateKey...)
	cfg.SrcIP = append(net.IP(nil), cfg.SrcIP...)
	keep := cfg.KeepaliveSec
	if keep == 0 {
		keep = defaultKeepaliveSec
	}
	return &Link{
		dp:        cfg.VPP,
		cfg:       cfg,
		wgPub:     k.PublicKey().Bytes(),
		keepalive: keep,
	}, nil
}

// InstallKeys 把租户的 hop-MAC 密钥环装进数据面。
//
// 今天的环里只有一条（见 wire.EnrollResponse.PKeys 的注释），所以这里记住那个
// hmac_id 给后面的路径用。**先装密钥再建路径**不是风格问题：路径签名要用它。
func (l *Link) InstallKeys(ctx context.Context, keys []Key) error {
	if len(keys) == 0 {
		return errors.New("agent: 租户没下发 hop-MAC 密钥，装不出能签名的路径")
	}
	for _, k := range keys {
		if len(k.Key) != 16 {
			return fmt.Errorf("agent: hmac_id=%d 的密钥应是 16 字节（SR_PKEY_LEN），实际 %d 字节",
				k.HmacID, len(k.Key))
		}
		if err := l.dp.SetPKey(ctx, k.HmacID, k.Key); err != nil {
			return fmt.Errorf("agent: 装 hmac_id=%d 的密钥: %w", k.HmacID, err)
		}
	}
	// 环里到今天只有一条（wire.EnrollResponse.PKeys 明说轮转还没实现）。取第 0 条
	// 只是为了**确定性**：任一条都能用，因为路径里写的就是这个 hmac_id、对端也有同一把。
	// 不写成"最后一条最新"那种推测 —— 契约没规定次序。
	l.mu.Lock()
	l.hmacID = keys[0].HmacID
	l.hmacSet = true
	l.mu.Unlock()
	return nil
}

// Open 建 WG 接口，或者**认回**上次留下的那个。返回值是"认回的"还是"新建的"。
//
// 为什么必须有"认回"：agent 重启或崩在配置中途时，VPP 里的接口并不会消失，而
// 重复的 wireguard_interface_create 实测回 -31（VNET_API_ERROR_INVALID_REGISTRATION，
// 实例号被占）。只会从零配一遍的 agent 第二次就起不来，只能人工进 vppctl 删干净 ——
// 那不能长期跑。
//
// 认回时只确认两件事：那张接口确实是本机的身份（公钥等于由我们的私钥推出来那把），
// 以及监听端口就是现在要用的那个。任一条不满足就**报错而不静默重建**：删接口会连带
// 干掉它所有 peer，等于把一条在跑的隧道拆了，这种动作得由人决定。
func (l *Link) Open(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	want := vppapi.InterfaceParams{
		UserInstance: l.cfg.Instance,
		ListenPort:   l.cfg.ListenPort,
		PrivateKeys:  l.cfg.PrivateKey,
		SrcIP:        l.cfg.SrcIP.To4(),
	}

	adopted, err := l.adopt(ctx, want)
	if err != nil {
		return false, err
	}
	if adopted {
		return true, nil
	}

	idx, err := l.dp.CreateInterface(ctx, want)
	if err != nil {
		if !isAlreadyRegistered(err) {
			return false, fmt.Errorf("agent: 建 WG 接口（实例 %d 端口 %d）: %w",
				want.UserInstance, want.ListenPort, err)
		}
		// 实例号已被占，可我 dump 时没看见它：有人在 dump 与 create 之间建了同一张
		// 接口（操作员手敲，或另一个 agent 进程）。再认回一次，认不回来才算真错误。
		adopted, err = l.adopt(ctx, want)
		if err != nil {
			return false, err
		}
		if adopted {
			return true, nil
		}
		return false, fmt.Errorf("agent: 实例 %d 被占用（VPP 回 -31），但 WG dump 里没有身份相符的接口：%w",
			want.UserInstance, err)
	}
	// 建 WG 接口会让 wireguard 插件把**这个接口的**监听端口重新注册回它自己的
	// wg4-input，必须先把这个端口登记进 vpn 集合、再把整组端口挂回本插件，
	// 否则这条 SR 隧道会静默退化成普通 WG（见 claimListenPort）。
	if err := l.claimListenPort(ctx); err != nil {
		return false, err
	}
	// 新建的接口也要过一遍 underlay 挂点：这台机器上第一个 agent 建接口时
	// feature 可能还没启用。
	if err := l.dp.EnableEncap(ctx, l.cfg.EncapIfIndex, true); err != nil {
		return false, fmt.Errorf("agent: 在 underlay 接口 %d 上开 encap: %w", l.cfg.EncapIfIndex, err)
	}
	if err := l.applyMTUs(ctx, idx); err != nil {
		return false, err
	}
	if err := l.verifyLanded(ctx, idx); err != nil {
		return false, err
	}
	l.swIfIndex = idx
	l.adopted = false
	l.ready = true
	return false, nil
}

// adopt 在已存在的接口里找本机的身份；找不到返回 (false, nil)。
//
// 认回之后要补的两步和新建时一样（登记端口 + 开 encap）：VPP 重启、或 wireguard
// 插件在别的接口创建过程中把端口抢回去，都会让 SR 挂点失效，而症状是"隧道在、没流量"。
// claimListenPort 是无条件执行的：它只是把"这个 vpn 端口归 sr-wg 节点"这件事再声明
// 一遍，幂等；在这个数据面里 UDP 端口就是功能分用键（定案 2），所以"把端口从 plain
// WG 输入抢回来"正是设计意图，不是副作用。
func (l *Link) adopt(ctx context.Context, want vppapi.InterfaceParams) (bool, error) {
	ifs, err := l.dp.Interfaces(ctx, false)
	if err != nil {
		return false, fmt.Errorf("agent: 读 VPP 上的 WG 接口: %w", err)
	}
	for _, ifi := range ifs {
		if ifi.UserInstance != want.UserInstance {
			continue
		}
		if !bytes.Equal(ifi.PublicKey, l.wgPub) {
			return false, fmt.Errorf(
				"agent: 实例 %d 上那张接口的公钥是 %s，不是本机身份 %s —— 私钥换过（身份目录被清？）；不自动删除，请人工确认",
				ifi.UserInstance, hex.EncodeToString(ifi.PublicKey), hex.EncodeToString(l.wgPub))
		}
		if ifi.Port != want.ListenPort {
			return false, fmt.Errorf(
				"agent: 实例 %d 上那张接口在监听 %d，而租户现在要的是 %d；改端口要删接口（会连带清掉它的 peers），请人工处理",
				ifi.UserInstance, ifi.Port, want.ListenPort)
		}
		if err := l.claimListenPort(ctx); err != nil {
			return false, err
		}
		if err := l.dp.EnableEncap(ctx, l.cfg.EncapIfIndex, true); err != nil {
			return false, fmt.Errorf("agent: 认回接口 %d 后在 underlay %d 上开 encap: %w",
				ifi.SWIfIndex, l.cfg.EncapIfIndex, err)
		}
		// 认回的接口同样要过一遍 MTU：接口是活的，可它身上的记录不一定是我们上次
		// 写的那两个数（操作员手工改过、或这张口本就没有记录）。写完由 verifyLanded
		// 读回来核对；重复设置幂等。
		if err := l.applyMTUs(ctx, ifi.SWIfIndex); err != nil {
			return false, err
		}
		if err := l.verifyLanded(ctx, ifi.SWIfIndex); err != nil {
			return false, err
		}
		l.swIfIndex = ifi.SWIfIndex
		l.adopted = true
		l.ready = true
		return true, nil
	}
	return false, nil
}

// claimListenPort 把本条隧道的监听端口登记进数据面的 vpn 端口集合，然后把
// **整个集合**重挂回 sr-wg 节点。两步都必要，而且都是幂等的，所以 Open 与 adopt
// 两条路径都无条件走一遍。
//
// AddPort 解决"这条隧道的端口插件根本没听说过"：集合里默认只有 51820，第二条隧道
// （比如 51830）不登记就永远没人给它加 SR 头 —— 症状照样是 encap_bypass，不是报错。
// RebindPorts 解决"挂点被别人抢走"：建 WG 接口时 wireguard 插件把**那个接口的**
// 监听端口注册回 wg4-input，同一台 CPE 上另一个 agent 建接口时抢走的可能是我的端口。
func (l *Link) claimListenPort(ctx context.Context) error {
	if err := l.dp.AddPort(ctx, vppapi.PortKindVPN, l.cfg.ListenPort); err != nil {
		return fmt.Errorf("agent: 把监听端口 %d 登记进 vpn 端口集合: %w", l.cfg.ListenPort, err)
	}
	if err := l.dp.RebindPorts(ctx); err != nil {
		return fmt.Errorf("agent: 重挂 UDP 端口集合: %w", err)
	}
	return nil
}

// applyMTUs 把两张口的 MTU 记录写成配置里的值。写完由 verifyLanded 读回来核对。
//
// 隧道口（tunnelSwIfIndex）写 cfg.TunnelMTU —— NewLink 已经把 0 换成了默认值，
// 这里不会再看到 0。数据面确实执行它：实测记 1360 的 wg0 发出 1436 的内层时，
// VPP 在 WG 出口把内层切成 1356+100 两片分别封装（见本文件规则 6）。执行的是
// 分片不是拒发，所以这个数同时还是"预算、上报、LAN 侧该配多大"的基准。
//
// underlay 出口只在调用方给了 UnderlayMTU 时才动，而且给多大写多大：猜一张物理
// 网卡的 MTU 比重复设置危险得多 —— 写大了 VPP 就不分片，超长外层帧照样交给
// sendto()，内核静默失败、这张口从此只收不发（本文件规则 6 的现场）；写小了
// 只是多分片，不会静默。两个设置都是幂等的，所以 Open 与 adopt 都无条件走一遍。
func (l *Link) applyMTUs(ctx context.Context, tunnelSwIfIndex uint32) error {
	if err := l.dp.SetInterfaceMTU(ctx, tunnelSwIfIndex, l.cfg.TunnelMTU); err != nil {
		return fmt.Errorf("agent: 给隧道接口 %d 设 MTU=%d: %w",
			tunnelSwIfIndex, l.cfg.TunnelMTU, err)
	}
	if l.cfg.UnderlayMTU == 0 {
		return nil
	}
	if err := l.dp.SetInterfaceMTU(ctx, l.cfg.EncapIfIndex, l.cfg.UnderlayMTU); err != nil {
		return fmt.Errorf("agent: 给 underlay 接口 %d 设 MTU=%d: %w",
			l.cfg.EncapIfIndex, l.cfg.UnderlayMTU, err)
	}
	return nil
}

// verifyLanded 读回接口表，确认这几件事真的落了地：出口号在 VPP 里存在、它不是
// 隧道接口自己、两张口的 MTU 记录就是我们刚写进去的那两个数。
//
// 为什么值得多发一次 dump：retval=0 在这两处都不代表配对了。EnableEncap 把 encap
// 挂到 WG 接口上照样回成功，而一个 SR 头都不会加（EncapIfIndex 的注释记的就是这条
// 坑）；SetInterfaceMTU 只说"收下"，不说"记成了什么" —— 手敲配置的操作员在同一条
// 链上写另一个值，agent 完全不晓得，症状照旧是"隧道在、小包通、大包没了"。
// 这三个症状没有一个会自己冒出来，所以只能在这里主动核对。
func (l *Link) verifyLanded(ctx context.Context, tunnelSwIfIndex uint32) error {
	ifs, err := l.dp.DumpInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("agent: 读回接口表（核对出口与 MTU 记录）: %w", err)
	}

	egress, ok := vppapi.FindInterface(ifs, l.cfg.EncapIfIndex)
	if !ok {
		return fmt.Errorf("agent: 配置里的 underlay 出口 %d 在 VPP 的接口表里不存在（现有 %s）",
			l.cfg.EncapIfIndex, interfaceSummary(ifs))
	}
	if egress.SWIfIndex == tunnelSwIfIndex {
		return fmt.Errorf("agent: underlay 出口 %d 就是隧道接口自己（%s）："+
			"encap 挂在 WG 接口上会启用成功但一个包都不改写，请填真正的出口网卡",
			l.cfg.EncapIfIndex, egress.Name)
	}
	tun, ok := vppapi.FindInterface(ifs, tunnelSwIfIndex)
	if !ok {
		return fmt.Errorf("agent: 隧道接口 %d 在 VPP 的接口表里读不到了（现有 %s）",
			tunnelSwIfIndex, interfaceSummary(ifs))
	}
	if tun.L3MTU != uint32(l.cfg.TunnelMTU) {
		return fmt.Errorf("agent: 隧道接口 %s(%d) 的 MTU 记录是 %d，我们要的是 %d："+
			"数据面按这个数切内层分片，记错了就是大包黑洞（有别的人在改这张表？）",
			tun.Name, tunnelSwIfIndex, tun.L3MTU, l.cfg.TunnelMTU)
	}
	// 没给 UnderlayMTU 就压根没写 underlay，读回来的记录自然不该由我们负责。
	if l.cfg.UnderlayMTU != 0 && egress.L3MTU != uint32(l.cfg.UnderlayMTU) {
		return fmt.Errorf("agent: underlay 出口 %s(%d) 的 MTU 记录是 %d，我们要的是 %d："+
			"记大了 VPP 就不分片，超长外层帧在 sendto 上把这张口的发送侧卡死（规则 6）",
			egress.Name, l.cfg.EncapIfIndex, egress.L3MTU, l.cfg.UnderlayMTU)
	}
	return nil
}

// interfaceSummary 把接口表压成一行，只用于报错时让操作员看见"VPP 到底有哪些口"。
func interfaceSummary(ifs []vppapi.SwInterface) string {
	out := make([]string, 0, len(ifs))
	for _, i := range ifs {
		out = append(out, fmt.Sprintf("%d:%s/%d", i.SWIfIndex, i.Name, i.L3MTU))
	}
	return strings.Join(out, " ")
}

// SWIfIndex 是本机的 WG 接口号，Open 之后才有意义。
func (l *Link) SWIfIndex() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.swIfIndex
}

// WGPublicKey 是装进数据面的那把私钥对应的公钥（要交给控制器与对端的就是它）。
func (l *Link) WGPublicKey() []byte { return append([]byte(nil), l.wgPub...) }

// Endpoint 是"该往哪个外层地址发包"的结论，连同这个结论是谁给的。
//
// Port 是**包在物理上落地的那个端口**：NAT 后面就是它的映射端口，公网直连就是
// 对端的监听端口（实测 VPP 的 WG 发包源端口=接口监听端口，所以无 NAT 时两者本来就相等）。
// 它只进 SR 段列表，**不进** WG peer 的 endpoint —— 后者必须是租户的 vpn_port，
// 原因见 ApplyPeer。
type Endpoint struct {
	IP     net.IP
	Port   uint16
	Source string // "observed"（我自己看到的）| "reflector"（反射器看到的）
}

// String 把"往哪发"和"这个地址是谁给的"一起打出来：排查对称 NAT 时，
// 后者往往比前者更能说明问题。
func (e Endpoint) String() string {
	return net.JoinHostPort(e.IP.String(), strconv.Itoa(int(e.Port))) + "(" + e.Source + ")"
}

// PeerTarget 是"请把这个人配起来"所需的全部信息，形状跟着 wire.PeerInfo 走，
// 但**不直接收 wire.PeerInfo**：这一层需要的是已认证的公钥与选定后的地址，
// 让调用方把决定做出来再传进来，规则才集中在一个能测的地方。
type PeerTarget struct {
	NodeID  uint16
	Tenant  string // 必须与本机同租户；绑定消息里就含它，写错了验不过
	CertPEM string
	Binding *keybinding.Binding

	// 两个证人（见 wire.MappingReport 的同一说明）。**我自己看到的**优先：
	// 对称 NAT 下反射器给的映射回不到那个人，只有我收到他的包时他用的那个外层
	// 源地址是条真路。反射器的映射负责第一个包。
	ObservedAddr     string // "ip:port"
	ObservedVerified bool   // 这条观测的 SR 头通过了 hop-MAC 校验
	ReflectorAddr    string // "ip:port"

	// AllowedIPs 是这条隧道在 overlay 上该转发的地址。
	// **不能为空**：实测 VPP 对 0 条 allowed_ip 的 peer 直接回 -7 INVALID_VALUE。
	// 契约里现在还没有 overlay 地址（wire.PeerInfo 没有任何隧道 IP 字段），
	// 所以这是入网之后、"GUI 一键批量配隧道"之前必须补上的那一项。
	AllowedIPs []vppapi.Prefix
}

// PeerChange 描述一次下发到底动了什么，调用方用它决定要不要记日志、要不要
// 重新回报可达性。
type PeerChange string

const (
	PeerInstalled PeerChange = "installed" // 第一次配上
	PeerReplaced  PeerChange = "replaced"  // 换了地址或密钥，删了重装
	PeerUnchanged PeerChange = "unchanged" // 数据面已经是这个样子
)

// ErrSingleBind：插件的隧道绑定表**每个 sw_if_index 只有一条**，装不下第二个 peer。
//
// 收包侧确实是按外层目的地址优先查的（sr_wg4_encap.c 的 sr_wg_bind_lookup：先查
// endpoint_path，再退回 tunnel_path），可**配置侧不是**：sr_wg_tunnel_set_path
// 先按 sw_if_index 查 tunnel_path，命中就走"更新"分支 —— 改的是同一条 bind 的
// path_id 与 endpoint，并把它的旧 (endpoint, dst_port) 键从 endpoint_path 里
// hash_unset 掉。所以一张 WG 接口上的第二个 peer 不是"多一条绑定"，而是把第一个
// 人的外层地址整条删掉再换上自己的：症状是"配了 B，A 突然不通"，而且 retval=0。
//
// 这里选择**在动手写之前**就报错，而不是写下去再靠日志猜。多 peer 要能用，得先
// 改插件让配置侧也按 (endpoint, dst_port) 建槽 —— 收包侧已经是这个键了，配置侧
// 仍是一接口一槽。那是数据面的活儿，不该由 agent 假装修好了。
var ErrSingleBind = errors.New("agent: 这台 CPE 的隧道绑定表每个接口只有一条，装不下第二个 peer")

// ApplyPeer 配好一个对端：验身份 → 选地址 → 建路径 → 绑隧道 → 装 WG peer。
//
// 幂等是设计目标，不是副产品：watch 流会把同一个人推很多次（心跳、重连后的
// snapshot、NAT 重绑），每次都要能安全地重放一遍。
func (l *Link) ApplyPeer(ctx context.Context, p PeerTarget) (PeerChange, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.ready {
		return "", errors.New("agent: 还没 Open，配不了对端")
	}
	if !l.hmacSet {
		return "", errors.New("agent: 还没 InstallKeys，路径没有可用的 hop-MAC 密钥")
	}
	if len(p.AllowedIPs) == 0 {
		return "", fmt.Errorf("agent: 节点 %d 没有 overlay 地址，VPP 会拒收这个 peer（实测 -7）；"+
			"控制器契约里还没有隧道地址这一项", p.NodeID)
	}
	wgPub, err := l.authenticate(p)
	if err != nil {
		return "", err
	}
	ep, err := chooseEndpoint(p)
	if err != nil {
		return "", err
	}
	pathID, err := l.PathID(p.NodeID)
	if err != nil {
		return "", err
	}
	// 先查表再写：一条会被静默覆盖的绑定，写下去就收不回来了。
	if err := l.checkSingleBind(ctx, p.NodeID, pathID); err != nil {
		return "", err
	}
	if err := l.checkObservation(ctx, ep); err != nil {
		return "", fmt.Errorf("agent: 节点 %d: %w", p.NodeID, err)
	}

	// 路径与隧道成对下发：绑定条目里的 endpoint 是绑定自己那份拷贝，不会跟着
	// 路径的段列表走（真机验过），所以只 AddPath 会让包发往旧地址。
	if err := l.dp.AddPath(ctx, vppapi.PathParams{
		PathID:   pathID,
		HmacID:   l.hmacID,
		Active:   true,
		Segments: []vppapi.Segment{{IP: ep.IP, Port: ep.Port}},
	}); err != nil {
		return "", fmt.Errorf("agent: 节点 %d 的路径（path_id=%d → %s）: %w", p.NodeID, pathID, ep, err)
	}
	if err := l.dp.SetTunnel(ctx, vppapi.TunnelBinding{
		SWIfIndex:  l.swIfIndex,
		PathID:     pathID,
		EndpointIP: ep.IP,
		// 绑定身份是 (外层 dst IP, 封装前的 dst 端口)。同一对 CPE 开第二条隧道时
		// 两条流的目的 IP 是同一个，只有端口能分清"这一包属于哪条隧道"；漏填
		// （=0 通配）会把第二条隧道也拽进这条路径，而第二条隧道的 agent 再来一次
		// 就把它抢走 —— 全是静默的 encap_bypass。
		DstPort: l.cfg.ListenPort,
	}); err != nil {
		// -116：同一个 (外层 IP, 本隧道端口) 已经被另一张接口占了。两张接口抢一个
		// 身份意味着"这个包到底走哪条路径"要看谁后写，插件选择直接拒绝，所以这里
		// 不能重试，只能由人看出是哪一张（show sr-wg tunnels 全表可查）。
		if hasRetval(err, -116) {
			return "", fmt.Errorf("agent: 节点 %d：到 %s 的端口 %d 这个隧道身份已被别的接口占着：%w；"+
				"本接口 %d 不能再抢，请用 show sr-wg tunnels 查出占着它的是哪一条",
				p.NodeID, ep.IP, l.cfg.ListenPort, err, l.swIfIndex)
		}
		return "", fmt.Errorf("agent: 节点 %d 的隧道绑定（interface %d → path %d, 端口 %d）: %w",
			p.NodeID, l.swIfIndex, pathID, l.cfg.ListenPort, err)
	}

	want := vppapi.PeerParams{
		PublicKey:  wgPub,
		EndpointIP: ep.IP,
		// **这里必须是本条隧道的 vpn_port，不是观测到的映射端口。**
		// encap 节点在 Step 4 只处理 udp 目的端口命中 vpn_ports 集合的包，Step 12
		// 才把目的端口改写成段端口（vpp-plugin/sr_wg4_encap.c）。所以 peer 端口填
		// 映射端口的话，包在出口上根本不会被加 SR 头，直接以普通 WG 发出去 ——
		// 计数器上是 encap_bypass，隧道"通"但没走源路由。映射端口只进段列表。
		Port:       l.cfg.ListenPort,
		Keepalive:  l.keepalive,
		SWIfIndex:  l.swIfIndex,
		AllowedIPs: canonPrefixes(p.AllowedIPs),
	}
	return l.installPeer(ctx, p.NodeID, ep, want)
}

// installPeer 把一个 peer 装成 want 的样子，并报告它到底动没动。
//
// VPP 没有"改 peer endpoint"这条消息，去重键又是公钥 —— 而且是**跨接口**的：
// 在 srwg-node1 上用两张接口装同一把公钥，第二次实测回 -116。所以地址变了只能
// 先删后加，而"别的接口上已经有这个人"是一种本层无权自愈的冲突（删别人的 peer
// 等于拆别人的隧道），只能把那个人指出来。
func (l *Link) installPeer(ctx context.Context, nodeID uint16, ep Endpoint,
	want vppapi.PeerParams) (PeerChange, error) {

	peers, err := l.dumpPeers(ctx)
	if err != nil {
		return "", err
	}
	cur := findPeer(peers, l.swIfIndex, want.PublicKey)
	if cur != nil && samePeer(*cur, want) {
		return PeerUnchanged, nil
	}
	if cur != nil {
		if err := l.dp.RemovePeer(ctx, cur.PeerIndex); err != nil && !isNoSuchEntry(err) {
			return "", fmt.Errorf("agent: 删节点 %d 的旧 peer（index %d）: %w", nodeID, cur.PeerIndex, err)
		}
	}
	if _, err := l.dp.AddPeer(ctx, want); err != nil {
		if !hasRetval(err, -116) {
			return "", fmt.Errorf("agent: 装节点 %d 的 peer → %s: %w", nodeID, ep, err)
		}
		peers, dumpErr := l.dumpPeers(ctx)
		if dumpErr != nil {
			return "", fmt.Errorf("agent: 节点 %d 的 peer 回 -116，重读全表又失败: %w（%v）",
				nodeID, dumpErr, err)
		}
		for i := range peers {
			if bytes.Equal(peers[i].PublicKey, want.PublicKey) {
				return "", fmt.Errorf(
					"agent: 节点 %d 的 WG 公钥 %s 已经装在接口 %d 上，而本 agent 用接口 %d；"+
						"VPP 的 peer 去重键是公钥且跨接口（真机实测 -116）。删别人的 peer 不是本层的权利，请人工决定留哪张接口",
					nodeID, hex.EncodeToString(want.PublicKey), peers[i].SWIfIndex, l.swIfIndex)
			}
		}
		return "", fmt.Errorf("agent: 节点 %d 的 peer 回 -116，可全表里查不到这把公钥（并发？重试一次即可）: %w",
			nodeID, err)
	}
	if cur != nil {
		return PeerReplaced, nil
	}
	return PeerInstalled, nil
}

// dumpPeers 读全表并把错误包成一句话；调用方按接口/公钥自己筛。
func (l *Link) dumpPeers(ctx context.Context) ([]vppapi.WgPeer, error) {
	peers, err := l.dp.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读现有 peer: %w", err)
	}
	return peers, nil
}

// findPeer 在 peers 里找"本接口上的这把公钥"。
//
// 筛选按接口是**有意**的：VPP 的去重键不含接口（见 installPeer），所以"别的接口上有"
// 不是找不到，而是要当成冲突单独处理，不能在这里悄悄返回它。
func findPeer(peers []vppapi.WgPeer, swIfIndex uint32, pub []byte) *vppapi.WgPeer {
	for i := range peers {
		if peers[i].SWIfIndex == swIfIndex && bytes.Equal(peers[i].PublicKey, pub) {
			return &peers[i]
		}
	}
	return nil
}

// checkSingleBind 确认本机接口上那条绑定要么没有、要么已经是这个人的。
//
// 只比 path_id：path_id 由 node_id 唯一决定（PathID），所以"等于本次的 path_id"
// 就是"这条绑定本来就属于他"，更新它是安全的；不等就是别人的，写下去会覆盖他。
func (l *Link) checkSingleBind(ctx context.Context, nodeID, pathID uint16) error {
	binds, err := l.dp.Tunnels(ctx, l.swIfIndex)
	if err != nil {
		return fmt.Errorf("agent: 读接口 %d 的隧道绑定: %w", l.swIfIndex, err)
	}
	for _, b := range binds {
		if b.SWIfIndex != l.swIfIndex || b.PathID == pathID {
			continue
		}
		return fmt.Errorf("%w：接口 %d 当前绑的是 path_id=%d（本次要给节点 %d 用 path_id=%d）。"+
			"插件的绑定表配置侧按 sw_if_index 一槽，后写的人会覆盖前一个人的外层地址；"+
			"要么先把那条绑定清掉，要么等配置侧也改成按 (endpoint, 端口) 建槽",
			ErrSingleBind, l.swIfIndex, b.PathID, nodeID, pathID)
	}
	return nil
}

// checkObservation 用**数据面自己的**证人复核"我自己看到的"那个地址。
//
// PeerTarget.ObservedVerified 是调用方填的 bool —— 而调用方是控制器中继回来的
// 转述。这一层能拿到更硬的东西：本机 VPP 的观测表里那条记录自带的 Verified 位
// （SR 头过没过 hop-MAC）。所以只做一个方向的否决：**表里现在有这条地址、
// 而它没过校验，就不配**。
//
// 表里查不到时不拦：观测是会老化的（wire.go 里 ObservedAgeSec 记的就是这个），
// 出表后唯一的证人就是控制器的转述，那是定案 1 的正常路径，不是漏洞。
func (l *Link) checkObservation(ctx context.Context, ep Endpoint) error {
	if ep.Source != "observed" {
		return nil
	}
	obs, err := l.dp.Observations(ctx)
	if err != nil {
		return fmt.Errorf("读观测表复核 %s: %w", ep, err)
	}
	for _, o := range obs {
		if o.SrcIP.Equal(ep.IP) && o.SrcPort == ep.Port && !o.Verified {
			return fmt.Errorf("观测 %s 在数据面里没过 hop-MAC 校验，而调用方声称它过了 —— 拒绝", ep)
		}
	}
	return nil
}

// DropPeer 撤掉一个对端：删 WG peer，再（在没人引用时）删他的路径。
//
// 收 PeerTarget 而不是 (nodeID, 公钥)：公钥必须由证书链证明，不能让调用方直接给
// 字节 —— 那样一个被攻破的控制器就能指使 agent 删掉任意 peer（删 peer 是可观测的
// 破坏，但比配错一个 peer 更难查）。
//
// 顺序是 peer 在前、path 在后。反过来也能成（DelPath 会级联解绑隧道），但那样
// 隧道绑定是先消失的，中间窗口里已经发出去的包会走不到对端 —— 两个动作都成功时
// 结果一样，出错时"留着一个没人指的 peer"比"留着一条指向空地址的隧道"好收拾。
func (l *Link) DropPeer(ctx context.Context, p PeerTarget) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.ready {
		return errors.New("agent: 还没 Open")
	}
	wgPub, err := l.authenticate(p)
	if err != nil {
		return err
	}
	peers, err := l.dumpPeers(ctx)
	if err != nil {
		return err
	}
	for _, pe := range peers {
		if pe.SWIfIndex != l.swIfIndex || !bytes.Equal(pe.PublicKey, wgPub) {
			continue
		}
		if err := l.dp.RemovePeer(ctx, pe.PeerIndex); err != nil && !isNoSuchEntry(err) {
			return fmt.Errorf("agent: 删 peer（index %d）: %w", pe.PeerIndex, err)
		}
	}
	pathID, err := l.PathID(p.NodeID)
	if err != nil {
		return err
	}

	// 删路径前看清谁在引用它：DelPath 会把引用它的绑定一起级联摘掉且 retval=0
	// （真机验过），而被摘的可能是操作员手配的另一张接口 —— 那不是本 agent 的权利。
	binds, err := l.dp.Tunnels(ctx, ^uint32(0))
	if err != nil {
		return fmt.Errorf("agent: 读隧道绑定表: %w", err)
	}
	var mine, others bool
	for _, b := range binds {
		if b.PathID != pathID {
			continue
		}
		if b.SWIfIndex == l.swIfIndex {
			mine = true
		} else {
			others = true
		}
	}
	if others {
		// 留着路径给那个引用者，只把自己这条绑定解掉（且只在我们确实指着它时解）。
		if mine {
			if err := l.dp.ClearTunnel(ctx, l.swIfIndex); err != nil && !isNoSuchEntry(err) {
				return fmt.Errorf("agent: 解绑接口 %d（path_id=%d 仍被别人引用）: %w",
					l.swIfIndex, pathID, err)
			}
		}
		return nil
	}
	if err := l.dp.DelPath(ctx, pathID); err != nil && !isNoSuchEntry(err) {
		return fmt.Errorf("agent: 删 path_id=%d（它上面的绑定会被一并摘掉）: %w", pathID, err)
	}
	return nil
}

// authenticate 是信任的落点：证书链到**我 pin 的这份 CA**、证书里的 srou:// URI
// 就是控制器口头声明的那个 (tenant, node_id)、且证书自己的密钥签了这份 WG 公钥。
//
// 三条各堵一个洞（见 keybinding.VerifyPeerCert）：控制器被攻破也编不出别的租户的
// 证书，中间人换公钥会让第三条失败。验不过就一律不配 —— 宁可隧道不通。
func (l *Link) authenticate(p PeerTarget) ([]byte, error) {
	if p.Binding == nil || p.CertPEM == "" {
		return nil, fmt.Errorf("agent: 节点 %d 没带证书或密钥担保，配不了", p.NodeID)
	}
	// 先比租户：一份 CA 给多个租户发证在企业里很常见，那时"链得过"并不等于
	// "和本机同租户"。跨租户的证书即使完全合法也不配 —— 那是别人的网络。
	if p.Tenant != l.cfg.Tenant {
		return nil, fmt.Errorf("agent: 节点 %d 声称属于租户 %q，而本机属于 %q：不跨租户配隧道",
			p.NodeID, p.Tenant, l.cfg.Tenant)
	}
	cert, err := keybinding.VerifyPeerCert(l.cfg.CAPool, p.CertPEM, p.Tenant, p.NodeID, *p.Binding)
	if err != nil {
		return nil, fmt.Errorf("agent: 节点 %d 的身份验不过: %w", p.NodeID, err)
	}
	id, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("agent: 节点 %d 的证书密钥类型不对", p.NodeID)
	}
	// VerifyPeerCert 内部已经验过一次签名，这里再取一次**字节**：担保里的 X25519
	// 公钥就是要塞进 VPP 的那把，取它的过程必须和验它的过程是同一条代码路径。
	wgPub, err := p.Binding.Verify(p.Tenant, p.NodeID, id)
	if err != nil {
		return nil, fmt.Errorf("agent: 节点 %d 的密钥担保取不出公钥: %w", p.NodeID, err)
	}
	return wgPub, nil
}

// PathID 把 node_id 映射到 path_id。
//
// 加 base 是为了和手敲 CLI 的编号隔开（见 LinkConfig.PathIDBase）；越界必须报错
// 而不是回绕 —— 回绕会让两个不同 node 抢同一条路径，症状是"某个人永远收不到包"。
func (l *Link) PathID(nodeID uint16) (uint16, error) {
	if uint32(l.cfg.PathIDBase)+uint32(nodeID) > 0xFFFF {
		return 0, fmt.Errorf("agent: path_id 溢出：base %d + node %d 超过 65535，换个 PathIDBase",
			l.cfg.PathIDBase, nodeID)
	}
	return l.cfg.PathIDBase + nodeID, nil
}

// chooseEndpoint 实现定案 1 的地址优先级。
//
// 只接受**过了 SR 头校验**的观测：观测表里的条目是明文头 + hop-MAC，没验过的
// 条目连"是谁发的"都说不清，拿它配 peer 等于让任意人改写别人的路由。
func chooseEndpoint(p PeerTarget) (Endpoint, error) {
	if p.ObservedAddr != "" {
		if !p.ObservedVerified {
			return Endpoint{}, fmt.Errorf("agent: 节点 %d 的观测 %s 没过 hop-MAC 校验，不能用",
				p.NodeID, p.ObservedAddr)
		}
		ip, port, err := parseAddr(p.ObservedAddr)
		if err == nil {
			return Endpoint{IP: ip, Port: port, Source: "observed"}, nil
		}
		// 观测地址坏了不静默退化到反射器：那是两个不同的证人，混起来就查不清了。
		return Endpoint{}, fmt.Errorf("agent: 节点 %d 的观测地址 %q 不合法: %w", p.NodeID, p.ObservedAddr, err)
	}
	if p.ReflectorAddr != "" {
		ip, port, err := parseAddr(p.ReflectorAddr)
		if err != nil {
			return Endpoint{}, fmt.Errorf("agent: 节点 %d 的反射器映射 %q 不合法: %w", p.NodeID, p.ReflectorAddr, err)
		}
		return Endpoint{IP: ip, Port: port, Source: "reflector"}, nil
	}
	return Endpoint{}, fmt.Errorf("agent: 节点 %d 没有任何可用外层地址（观测与反射器都空）", p.NodeID)
}

// parseAddr 解 "ip:port"，且只收 IPv4：SR 头里的段地址是 4 字节，VPP 的 WG peer
// 端点这里也还没走 v6。
func parseAddr(s string) (net.IP, uint16, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return nil, 0, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, 0, fmt.Errorf("%q 不是地址", host)
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, 0, fmt.Errorf("%q 不是 IPv4（SR 段地址只有 4 字节）", host)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return nil, 0, fmt.Errorf("端口 %q 读不出: %w", portStr, err)
	}
	if port <= 0 || port > 0xFFFF {
		return nil, 0, fmt.Errorf("端口 %d 出界", port)
	}
	return v4, uint16(port), nil
}

// samePeer 判"数据面已经是这个样子"。
//
// 比的是 endpoint 三元组 + table_id + keepalive + allowed_ips（多重集意义，不看
// 顺序）—— 少比任何一项都会让该重装的 peer 被判成没变，症状正是隧道停在旧地址上。
// table_id 是 VPP 用来决定"这个 peer 的 overlay 流量查哪张表"的，不同就是不同路由。
func samePeer(cur vppapi.WgPeer, want vppapi.PeerParams) bool {
	return cur.EndpointIP.Equal(want.EndpointIP) &&
		cur.Port == want.Port &&
		cur.TableID == want.TableID &&
		cur.Keepalive == want.Keepalive &&
		samePrefixes(cur.AllowedIPs, want.AllowedIPs)
}

// canonPrefix 把一个前缀归成"VPP 会回给我的那个样子"：IPv4 一律 4 字节，
// 前缀长度上限 32。
//
// 不canonicalize 会让隧道永远在"重装"：调用方常用 net.ParseIP 拿到 16 字节的
// v4-mapped 写法，::ffff:10.0.0.1/128 发出去 VPP 存成 10.0.0.1/32（实测 dump 回来
// 是 4 字节 + /32），拿 128 去比就永远不相等，于是每次 watch 推过来都删 peer 再加一次。
func canonPrefix(pre vppapi.Prefix) vppapi.Prefix {
	if v4 := pre.IP.To4(); v4 != nil {
		if pre.Len > 32 {
			pre.Len = 32
		}
		pre.IP = v4
	}
	return pre
}

func canonPrefixes(pres []vppapi.Prefix) []vppapi.Prefix {
	out := make([]vppapi.Prefix, len(pres))
	for i, pre := range pres {
		out[i] = canonPrefix(pre)
	}
	return out
}

// samePrefixes 按**多重集**比较：只比"每个 a 里都能在 b 里找到"的话，
// [10/24, 10/24] 会被判成等于 [10/24]，那条重复的路由就永远修不掉。
func samePrefixes(a, b []vppapi.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		x = canonPrefix(x)
		found := false
		for i := range b {
			if used[i] {
				continue
			}
			if y := canonPrefix(b[i]); x.Len == y.Len && x.IP.Equal(y.IP) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// hasRetval 认 VPP 的某个具体 retval（vnet/error.h 的 foreach_vnet_error）。
func hasRetval(err error, want int32) bool {
	var apiErr vppapi.APIError
	return errors.As(err, &apiErr) && apiErr.Retval == want
}

// isNoSuchEntry：-6 = VNET_API_ERROR_NO_SUCH_ENTRY（真机实测）。
//
// 只有这一种错误可以当"已经不在了、继续往下走"：删一个已经不存在的 peer 是
// reconcile 的正常分支（上次崩在中间、或对端刚被删过）。其它错误一律往上报，
// 不然故障会被这个便利逻辑吃干净。
func isNoSuchEntry(err error) bool { return hasRetval(err, -6) }

// isAlreadyRegistered：-31 = VNET_API_ERROR_INVALID_REGISTRATION，实测出现在
// wireguard_interface_create 撞到一个已被占用的 user_instance 时。
func isAlreadyRegistered(err error) bool { return hasRetval(err, -31) }
