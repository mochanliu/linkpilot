# NAT 下的隧道自动建立：四步阶梯（方案，待拍，未写代码）

日期：2026-11-03。作者：Qoder agent。状态：**方案待用户拍板，一行产品代码都没动**。
唯一一条已经拍完并且落地的除外：**P-5 拍乙**（改数据面的选路键与终点段来源），随 D-2
完成，见 §4 末尾。

## 0. 前提（已由用户定案，本方案不再讨论）

- 控制器部署在公网（客户私有化部署在公网 IP，或用我们的云控制器），所以控制器看到的
  cpe 外层地址就是它要拨的地址。
- **自动编排只按控制器看到的公网地址建隧道**；打不通就不建，不做任何中转兜底
  （中转永远是显式配置的，自动编排的隧道里永远没有中转节点）。
- **假设 NAT 不改变源端口**（用户 2026-11-03 定的前提）。本方案不讨论"端口变了怎么办"。
- TCP/UDP 之争不必讨论：控制器观测到的 cpe 公网地址在两种协议下都是对的。
- 打洞的主动首发就是 BFD 会话（`internal/agent/telemetry.go:128-149`：BFD 在 reconcile
  里无条件装上，会周期发包），不需要额外的打洞包。

## 1. 用户的阶梯，逐条落到我们代码上的动作

架设 cpe1↔cpe2 一条隧道（两端都只有控制器观测到的公网地址）：

| 步 | cpe1 的 peer | cpe2 的 peer | 判据 | 今天代码能做到吗 |
|----|-------------|-------------|------|-----------------|
| 1 | cpe2 公网 ip:port | cpe1 公网 ip:port | BFD up | 能，就是现在的路径 |
| 2 | cpe2 公网 ip:port | **只有监听端口，无对端地址** | BFD up | 不能，三处拒绝（见 §3） |
| 3 | **只有监听端口，无对端地址** | cpe1 公网 ip:port | BFD up | 同上 |
| 4 | — | — | 判为不可穿透，放弃这条隧道 | 没有这个状态 |

"放弃"必须是个看得见的状态，不是把隧道静默删掉：`internal/underlay/plan.go` 的
`Skip` 就是这个原则的先例（"静默丢对是拓扑里出现没人能解释的洞的原因"）。

## 2. VPP 侧的证据（今天新取，可复现）

取自上游 v26.06（`api.github.com/repos/FDio/vpp/contents/src/plugins/wireguard/<file>?ref=v26.06`，
2026-11-03 拉取）：

1. **peer 的外层源端口 = 本隧道接口自己的监听端口**：
   `wireguard_peer.c:341` `peer->src.port = wgi->port;`，随后
   `wg_build_rewrite(&peer->src.addr, peer->src.port, &peer->dst.addr, peer->dst.port, ...)`。
   ⇒ 我们"每隧道一个监听端口"（定案 2）意味着**外层 UDP 源端口本身就是隧道身份**，
   与对端地址无关。这是 §4 乙方案的键。
2. **endpoint 为空是被接受的**：`wg_build_rewrite` 在 `dst 为 0 或 dst_port == 0` 时返回
   `NULL`（`wireguard_send.c:99-113`），`wg_peer_can_send()` 就是
   `return peer && peer->rewrite;`（`wireguard_send.h:198-201`）。
   `wireguard_api.c:155-161` peer_add 不校验 endpoint，全零照样进池。
   ⇒ 空 endpoint 的 peer = **能收不能发**，不是错误状态，也不会崩：数据面发包时走
   `!peer->remote.r_current` 分支，发握手并丢弃本包、计 `WG_OUTPUT_ERROR_KEYPAIR`
   （`wireguard_output_tun.c:449-453`）。
3. **对端地址是从进来的握手包里学的**：`wireguard_input.c:252`（Handshake-Initiation）
   和 `:298`（Handshake-Response）都调
   `wg_peer_update_endpoint(peeri, &src_ip, udp_src_port)`，该函数会重建 rewrite 并
   刷新所有 midchain 邻接（`wireguard_peer.c:376-400`）。
   ⇒ 阶梯第 2/3 步的机理成立：只监听的一端收到对方握手包后自动拿到
   对方的**NAT 后公网 ip:port**，握手响应就能发出去，BFD 随之起来。不需要我们填地址。
4. 学习只发生在握手期（上面两处调用），不是每个数据包都刷新；所以 WG 自己的
   rekey（`REKEY_TIMEOUT`）会让 endpoint 再刷新一次。这是运行事实，本方案不依赖它。

## 3. 今天要动的第一件事：把"空 endpoint"这条路径打开

拒绝它的**全是我们自己的代码**，不是 VPP：

- [controller/internal/vppapi/messages.go:354](../controller/internal/vppapi/messages.go)
  `AddPeer`：`if !isAPIAddress(p.EndpointIP) { return ... "peer 必须有合法的外层 endpoint 地址" }`。
- [controller/internal/agent/dataplane.go:1452-1473](../controller/internal/agent/dataplane.go)
  `chooseEndpoint`：观测与反射器地址都空 ⇒ 硬错，`ApplyPeer` 走不下去。
- [controller/cmd/sr-ctrl/underlay_delivery.go:236-254](../controller/cmd/sr-ctrl/underlay_delivery.go)
  `dialEndpoint`：地址空 ⇒ "设备 N 还没有可用的外层地址"；`peer_endpoint` 因此永远非空。
- [controller/internal/wire/wire.go:525](../controller/internal/wire/wire.go)
  `TunnelSpec.PeerEndpoint string`：需要一个"本端不拨对端"的表达。
  **不许**用空串之外的哨兵值（不是 0.0.0.0、不是 0 端口、不是 "auto"）——
  用户明确要"为空"，而数据面里 0 已经另有含义（隧道绑定键 `src_port = 0` = 永不命中，
  见 `vpp-plugin/sr_wg4_encap.c` 的 `sr_wg_bind_set_port`）。
  收口办法：加一个显式布尔 `PeerEndpointOmitted bool`（名字待拍，见 P-4），
  `PeerEndpoint` 保持空串；agent 见 `Omitted` 才走空 endpoint，见空串且没这个标志
  仍然报错。这样"忘了填"和"故意不填"是两个不同的状态，后者不会伪装成前者。

## 4. SR 与 endpoint 的关系（我上一条说错了哪一半）

用户描述："SR 只配置中间路径，最后一跳永远由 WG 负责：把最外层目的地址压到标签最后一层，
路径压到其余层，第一层改写成最外层目的地址。这个机制下 endpoint 怎么变都不影响 SR。"

**段格式本来就是他说的那个格式**，这条我错在把它说成"SR 会退化"：
[sr_header.h:268-270](../vpp-plugin/sr_header.h) `segments[0] = 最终目的节点`、
[docs/sr-header-format.md:79-81](sr-header-format.md)、
[sr_wg4_encap.c:61-69](../vpp-plugin/sr_wg4_encap.c)（encap 把外层 dst 改写为
`segments[seg_count-1]`，终点留在 `segments[0]`）。终点在标签栈里、第一跳由外层承载，
正是"最后一跳由 WG 负责"的形状。

**但写这份方案时，有两处把 SR 拴在了"控制器给的 endpoint"这个值上**，不是拴在包上：

1. 选路键：`sr_wg4_encap.c` 用 `(外层 dst IP, 封装前的 dst 端口)` 去查绑定，
   决定"这一包属于哪条隧道、该套哪条路径"。这个键的两个值都是对端地址。
2. `segments[0]` 的来源：`dataplane.go` 的终点段是从控制器下发的对端地址填的，不是从包里
   读的；绑定条目也带同一份 endpoint 拷贝（注释里写明"绑定条目里的 endpoint 是绑定自己
   那份拷贝，不会跟着路径走"，真机验过）。

所以当时：**endpoint 一旦不是控制器已知的那个值（为空、或 WG 学到别的地址），
这两处就查不到 ⇒ `encap_bypass` 静默直通**。直通对直接隧道是无害的（纯 WG 是合法
形态，[sr_wg4_input.c:10-12](../vpp-plugin/sr_wg4_input.c) 明确旁路非 SR 包），
代价只是那一条流少了一层 SR 头。

### P-5 已拍：乙（本节里的"乙"= D-2 讨论中的"A 方案"，同一个改动）

2026-11-03 用户定："你就按 A 改吧" —— 数据面按 §2 第 1 条的事实改成"从包上取"，
两处一起收口：

- **选路键 = 本端外层 UDP 源端口**（`sr_wg_bind_lookup(src_port)`）。它是本隧道的监听端口，
  设备内唯一、与对端地址无关。`src_port = 0` 表示"还没给出可用的键"，数据面**永不命中**，
  不是通配；`show sr-wg tunnels` 把这种绑定标成 `NO SRC PORT, never matches`。
- **终点段不进路径表**：表里只存中转跳（`relay_count`），`seg_count = relay_count + 1`，
  encap 在封装前把报文自己的外层 `dst:port` 写进 `segments[0]` 再签。`relay_count = 0` 合法
  （纯直接隧道也要那张头当"收端把外层源记成 verified 观测"的凭证），CLI 里必须显式写
  `no-relay`，否则和"忘了填路径"分不开。
- agent 侧照旧：`relays[]` 进 vppapi 时倒序（数组最后一格 = 第一跳），`SetTunnel` 带
  `SrcPort = 本端监听端口`。`sr_wg.api` 四条消息的 CRC 已随字段改名重测并写进
  `vppapi/messages.go` 的 `requiredVersions`。

**这一条的后果，用户明确免除、测试也照免除写**：同一个 `src_port` 被两条绑定登记时插件
**不去重**（点对点隧道只有"手工误配"一种来源）。代价是 `sr_wg_tunnel_set_path` 换端口时要
`hash_unset(旧键)`，于是"把冲突那条改回自己端口"这一步会把**另一条**的键也摘走 —— 那条隧道
配置面看着完全正常（`src_port` 还在、`usable` 还在），数据面已经查不到，包开始裸发。
`test/scripts/test-multitunnel.sh` 的 ASSERT H 就是把这三件事量出来：接受、配置面读不出
命中归属（所以不声称实测到命中归属）、摘冲突后必须原样重下受影响那条才恢复。

⇒ 对本阶梯的意义：只监听那一端**不再需要**"不装 SR 路径与绑定"这个特例 —— 地址漂移或为空
都不会让 SR 静默旁路，因为选路和终点都来自报文本身。§1 的四步阶梯照原计划做。


## 5. 控制台的形状（用户硬性要求）

「隧道有一端可以不配置 ip 端口号，**不是用某个默认值，而是为空**」：

- 隧道表单里对端那一列：地址、端口两个输入框允许同时留空，旁边一个显式勾选
  "本端不拨对端，只监听（等对方打进来）"。勾上 ⇒ 两框禁用且提交空值；不勾 ⇒ 现在的校验不变。
- 手工建隧道保持现状（默认两端都要地址）；勾选是把"只监听"作为显式意图表达出来。
- 隧道列表要显示这条隧道现在的真实形态：`两端互拨` / `A 拨 B` / `B 拨 A` / `放弃（不可穿透）`，
  并显示阶梯每一步的失败原因和时间。运行期 WG 学到的对端地址（可从
  `vppapi.Peers()` 读回，[messages.go:432-475](../controller/internal/vppapi/messages.go)）
  作为"事实"单列展示，标"设备学到"，不冒充控制器观测。
- 设备没上报的字段仍显示"未上报"，不填 0（AGENTS §2）。
- **`given_up` 是一条保存下来的隧道，不是一次失败记录**（P-6）：它出现在隧道列表里、
  可以选中、可以编辑，编辑面板里对端的地址与端口两个框**放开手工填写**（阶梯期间这两个框
  由控制器按观测填，人改过之后阶梯不再自动改它 —— 一旦人填过就退化成 §5 上面那条"手工的
  地址是意图"）。页面上把"没通"写成一个状态，不写成错误弹窗，也不因为没通就不保存。

## 6. 编排回路（控制器要新增的机制）

模式切换 = 改 WG peer，**不重开隧道接口**：instance / 监听端口 / overlay / 出口都不变，
`sameLinkConfig` 不会触发 rebuild（[tunnel.go:521-531](../controller/internal/agent/tunnel.go)），
只会 `ApplyPeer → PeerReplaced`（删旧 peer 装新 peer，
[messages.go:344](../controller/internal/vppapi/messages.go)：VPP 的 peer 去重键是公钥，
跨接口，改地址必须删了重装）。

必然中断：**从"两端互拨"降级到"A 拨 B"时，被改成只监听的那一端会断开正在跑的
客户流量**（peer 删除 = WG 会话销毁 = BFD down）。按 AGENTS §1.4，这条中断必须实测
并写在页面上，不许留"应该是瞬断"。要量的两个数：
(a) peer 删→装→握手完成→BFD 从 down 到 up 的端到端时长；
(b) 同一条隧道上另一端看到的现象与时长。测量方式：干净重建 lab、打流窗口
（注意打流窗口在管道子 shell 里会假绿 0ms 这条已知坑）。

## 7. 拍板结果（2026-11-03 用户定，本节不再是待拍）

- **P-1 判据：甲 + 加 `TunnelID`。** 阶梯每步只看 BFD `Up` 算通（300ms 级）；
  同时 `TunnelMetrics` 行必须带 `TunnelID`，理由见 §9 的 M-4：它不是阶梯的可选项，
  而是"同对端第二条隧道进来后，遥测行还能不能说清是哪条"的硬前置。
- **P-2 每步超时：甲，10 秒。** 整条阶梯最多 40s。慢链路误判的代价由"第 4 步放弃后
  仍可手工改地址重试"承担（§5、P-6），不靠拉长超时换。
- **P-3 手工隧道：完全不跑阶梯。** 人手填的地址就是意图，控制器不擅自降级成"只监听"，
  一次失败就如实报错，保持现状。阶梯只服务于控制器自己按观测建出来的隧道。
- **P-4 命名：不改。** Go 侧 `PeerEndpointOmitted`，页面勾选文案
  "本端不拨对端，只监听（等对方打进来）"。
- **P-5 SR**：~~甲／乙~~ **已拍乙并已落地**（见 §4 末尾）。
- **P-6 放弃之后：保存隧道。** 库里保留这条隧道，状态 `given_up`，语义 =
  "手动配了一个配置没通"，列出每步失败原因与时间；**并允许手工编辑地址和端口号**
  （形状见 §5）。不静默删——删掉就是看不见的地洞。

阶梯的实现排在 §9 的 M-4 之后（判据要用 `TunnelID`）；M-1/M-2/M-3 是同对端多条隧道
自身的缺陷（D-3），阶梯不会顺带解决它们。

## 8. 这一版明确不做

- 不改 WG 的 NAT 保活/`nat-refresh` 语义，不加打洞专用包。
- 不做中转兜底、不做颜色、不做非公网线路（mpls/专线）。
- 不动 `plan.go:119-137` 里"观测过期不建"的闸门（阶梯的前提是有公网地址可拨）。
- 不动 lab：本方案落地前不跑任何 lab/harness 矩阵。

## 9. D-3：两台 CPE 之间多条隧道，代码里还不支持的地方（2026-11-03 全量审计）

背景是产品要求（每台盒子目标 3000 条隧道、软件层 4096；8 个颜色 ⇒ 同对端最多 8 条）。
下面每一条都我自己读到过那一行，不是转述。**修它们一律用新搭的一次性私有环境**（用户
2026-11-03 规则：修缺陷期间不许动现有 lab），改完再由我按最新代码重搭 lab。

### 9.1 硬错：第二条根本建不出来

1. **控制器上游门禁**：[internal/underlay/tunnel.go:100-103](../controller/internal/underlay/tunnel.go)
   在 `create()` 最前面按**无序设备对**查 `pairTunnel`（[:313-326](../controller/internal/underlay/tunnel.go)），
   命中就回 `ErrPairTaken`（[state.go:20-24](../controller/internal/underlay/state.go)，API 层转 409）。
   判据里没有颜色。错误文案自己把原因说清了：「每台设备只有一把 WireGuard 静态密钥，
   VPP 的 peer 表按公钥跨接口去重，第二条会被数据面以 -116 拒掉」。
2. **根因：一条设备一把 WG keypair**。私钥只有一把
   （[internal/agent/identity.go:177-192](../controller/internal/agent/identity.go)），
   对账时 `priv := t.id.WGPrivateKey()` 一次取出、传给**每一条** Link
   （[internal/agent/tunnel.go:422](../controller/internal/agent/tunnel.go) → `PrivateKey: priv` :612）。
   于是同一对端的第二条隧道要装**同一把对端公钥**，撞上 VPP 跨接口去重：
   [internal/agent/dataplane.go:1094-1112](../controller/internal/agent/dataplane.go) 那段 -116 分支
   就是这个（注释写"真机实测"）。
   ⇒ 这条不是"放宽判据"能绕开的。要做的是**每隧道一把 keypair**，牵连三处：
   设备身份要持有多把（identity 现在只存一把）、
   密钥担保的主语要扩（[internal/keybinding/binding.go:115-195](../controller/internal/keybinding/binding.go)
   的 `Message/Verify` 只签 `(tenant, node_id, key_gen, x25519_pub)`，没有隧道维度）、
   控制器存的 `prec.Binding` 是按设备存的（[cmd/sr-ctrl/underlay_delivery.go:113-117](../controller/cmd/sr-ctrl/underlay_delivery.go)）。
3. **批量与扇出按设备对折叠**：`plan.go` 的自动批：[:105-108](../controller/internal/underlay/plan.go)
   命中 `HasPair` 就 `Skip{"这对设备之间已经有隧道，跳过"}`，`HasPair`
   同样只比设备对（[:311-321](../controller/internal/underlay/plan.go)）；
   `Fan` 一批一个颜色、每个对端一条（[:41-96](../controller/internal/underlay/fan.go)，
   单条 create 被拒就变成一条 `FanSkip`）。
   症状比硬错更难查：full-mesh / hub-spoke 永远补不出第 2..8 色，拓扑上留下没人解释的洞。

13. **2026-11-03 实测：把"端口是主键"和"peer 表按公钥去重"这两件事分清**。
    上面第 2 条把根因写成"VPP 的 peer 表按公钥跨接口去重（真机实测 -116）"，这条断言和
    "监听端口才是主键、每隧道每端口唯一是 WireGuard 原生支持的"并不矛盾，但必须各自量一遍
    才知道 -116 到底是谁挡的路。环境：**一次性私有树** `/tmp/mtn`（从当前工作树 rsync，
    只改 `test/docker/*` 里的容器名 `srwg-*`→`mtn-*` 与子网 `10.10.*`→`10.35.*`），
    Docker Desktop daemon、`COMPOSE_PROJECT_NAME=mtn`、只起 `mtn-node1` 一个盒子
    （VPP v26.06-release，插件 sr_wg 现场编译加载），探针走我们自己的 vppapi 客户端，
    号段 instance 9101/9102/9103、端口 51977/51978、地址一律 TEST-NET-1，跑完逐条
    `wireguard_interface_delete` + `down -v` 销毁；复核过 `srwg-*`/`osc-*` 的 uptime 未变。

    | 问的是 | 形状 | VPP 回的值 |
    |---|---|---|
    | Q1 今天这条断言本身 | 两张接口**各一把本地私钥**，把**同一把对端公钥**分别装上去 | 第一张 0，第二张 **-116** |
    | Q2 M-2 的成立判据 | 两张接口各一把本地私钥，对端公钥也**各一把** | 两张都 **0** |
    | Q3 今天"一台设备一把 keypair"的真实形状 | **同一把本地私钥**建两张接口，只换监听端口 | 两张口都建成（0） |
    | Q4 拆变量 | 同一把本地私钥 + **同一个端口**，只换 instance | 也建成（0） |

    三条结论，逐条对上层的意义：
    - **-116 与本地私钥无关**：Q1 的两张口本来就各用自己的私钥，仍然 -116。
      挡路的是 VPP 的 peer 表以**对端公钥**为键且键里不含接口，所以第 2 条的归因成立、
      "改成本地私钥不同就能救"是不成立的。
    - **"端口做主键"在 VPP 的配置面完全不设防**：Q4 连"同私钥同端口"都收，说明 VPP
      既不按公钥也不按端口给 WG 设备去重。所以"每隧道每端口唯一"这条不变式**只有我们的
      分配器在守**（`allocPort`，[state.go:296-339](../controller/internal/underlay/state.go)），
      放开门禁不会撞上 VPP 的端口检查，但也不会得到 VPP 的任何保护。
    - **M-2 是 M-3 的前置，方向不变**：只放宽 pair 门禁（M-3）而不给每条隧道一把对端
      keypair，盒子侧第二次 `wireguard_peer_add` 仍回 -116 —— 那是把控制器里的硬错换成
      "控制器说已下发、agent 装不上"的静默失败。Q2 的 0 是 M-2 值得做的直接证据；
      它证明的是**配置面收得下**，两条隧道各自跑通的**数据面**判据要在 M-2 实施时另测
      （握手互不串台、BFD/探针各自成会话），不能拿这张表当已验。

### 9.2 静默覆盖：会改到在线转发行为，且 retval 全 0

4. **path_id 没有隧道身份**：[cmd/sr-ctrl/underlay_delivery.go:131](../controller/cmd/sr-ctrl/underlay_delivery.go)
   恒发 `PathIDBase: wire.UnderlayPathIDBase`（常量 1000，
   [internal/wire/wire.go:470-474](../controller/internal/wire/wire.go) 的注释自己写着
   "one device pair carries one tunnel"），设备侧 `PathID(nodeID) = base + PeerNodeID`
   （[internal/agent/dataplane.go:1433-1438](../controller/internal/agent/dataplane.go)）。
   两条到同一个人的隧道 ⇒ **同一个 path_id** ⇒ 后写的把前一条的段列表整条换掉，
   接口各自绑着同一个 path，两边都"配置成功"。
   这个要求代码里已经写明了却没实现：
   [dataplane.go:212-215](../controller/internal/agent/dataplane.go) 的注释就是
   「同一台 CPE 上的**每条隧道都要一个自己的 base**……症状是『第一条隧道忽然不中转了』」。
   附带要改的自我陈述：[dataplane.go:1142-1146](../controller/internal/agent/dataplane.go)
   的 `checkSingleBind` 注释写"path_id 由 node_id 唯一决定"——它描述的是今天这个错的事实，
   不是设计。
   注：D-2 之后**收包侧不受这条影响**（decap 按本端外层源端口查路，端口每隧道唯一，
   见 [vpp-plugin/sr_wg4_input.c:228-242](../vpp-plugin/sr_wg4_input.c) 里 wg4-input 按 UDP 目的端口
   定位本端接口）；坏的是发侧 接口→path_id→段列表。

### 9.3 归属说不出：污染操作员所见

5. **遥测行没有隧道身份（= P-1）**：[internal/wire/wire.go:916-919](../controller/internal/wire/wire.go)
   `TunnelMetrics` 只有 `SwIfIndex` + `PeerNodeID` + `PeerIP`；控制器落库按 `sw_if_index`
   去重（[cmd/sr-ctrl/telemetry.go:69-83](../controller/cmd/sr-ctrl/telemetry.go)）；
   控制台显示 `peer_ip` + `sw_if_index`（[console.html:1268-1271](../controller/cmd/sr-ctrl/console.html)）。
   ⇒ 同对端 8 条并排时说不出是哪条隧道/哪个颜色，且 VPP 重启后 index 漂移会让历史串台。
6. **`ReachReport` 同样只有 `PeerNodeID`**（[internal/wire/wire.go:756-760](../controller/internal/wire/wire.go)）。
   它现在没有调用者（#183 那一类"接了没人调"的同一形状）⇒ 加 `TunnelID` 或直接删，别留半个字段。

### 9.4 我核过之后**不是**缺陷的（子代理报过、我逐行自证后剔除）

7. **遥测/BFD 的键**：[internal/agent/telemetry.go:251-264](../controller/internal/agent/telemetry.go)
   的 `byPeer`/`swIfOfPeer` 只按对端 IP，`hasBFDSession`(:379-392) 按 (local, peer)，
   [internal/fib/reach.go:20-46](../controller/internal/fib/reach.go) 的 `down` 只按对端地址。
   今天不会塌，因为 **overlay /32 是全局唯一发的**：`allocOverlay` 每个隧道端取一个新地址
   （[internal/underlay/state.go:410-440](../controller/internal/underlay/state.go)，
   一次 create 里 A/B 两端各取一个并互相 reserved，:149-160），
   所以"对端 /32"本身就是隧道身份。**风险是隐式的**：哪天允许两条隧道复用同一个对端 /32，
   这三处同时塌。收紧成 (local, peer) 顺手就能和 M-4 一起做。
8. **路由面已经支持并行隧道**：`route.Tunnel{Peer, NextHop}` 无 tunnel_id
   （[internal/route/proto.go:135-138](../controller/internal/route/proto.go)），
   但规范序是 (peer, next_hop) **严格升序**（:183-189），两条隧道 /32 不同 ⇒ 合法共存；
   第 4 段渲染遍历 `SortedTunnels()` 全量、每隧道一行
   （[cmd/sr-ctrl/overlay.go:728-752](../controller/cmd/sr-ctrl/overlay.go)）；
   `reachableLocked` 是"到源设备有没有隧道"的粗闸门
   （[internal/route/client.go:981-983](../controller/internal/route/client.go)），
   逐隧道摘除发生在 FIB 侧的 `down[nh]`（[internal/fib/fib.go:303-307](../controller/internal/fib/fib.go)）——
   问"隧道 #2 掉了会不会连带撤另一条的路径"，答案是**不会**。
9. **MPLS 标签只含 VPN 是对的**：`LabelFor(vrf) = LabelBase + vrf`
   （[internal/fib/fib.go:60-65](../controller/internal/fib/fib.go)）——标签是远端语义
   （对端弹栈后查自己那张表），与隧道无关。"per-tunnel 标签"是我的推测，删掉。
10. **存储/端口/实例号/配置盘上都已按隧道**：`Tunnels` map 键是 `t.id`
    （[state.go:207](../controller/internal/underlay/state.go)）、`portsOn`/`allocPort` 每设备端口唯一(:296-339)、
    WG `instance` 由隧道 ID 派生（[underlay_delivery.go:272-286](../controller/cmd/sr-ctrl/underlay_delivery.go)）、
    设备侧 keep 集合按 `TunnelID`（[agent/tunnel.go:444-466](../controller/internal/agent/tunnel.go)）、
    apply 整表替换（[apply/route.go:198-259](../controller/internal/apply/route.go)）。
11. **控制台**：对端下拉不排除已有隧道的对，硬错原文进 `undErrNote`（可接受）；
    拓扑边折叠成 `firstRelayed || tunnels[0]` + "N 条"标签是刻意的，但边明细缺"颜色"列。
12. **lab 脚本**：[test/lab/loop-console.py:328-333,354-358](../test/lab/loop-console.py)
    用 `frozenset(设备对)` 做集合等值断言，隐含"一对一条"；属 lab，重搭 lab 时随最新代码改。

### 9.5 修复次序（每条都要先出方案、点头后再写码）

| 序 | 内容 | 为什么这个位置 | 阶梯 #222 是否等它 |
|---|---|---|---|
| M-4 | 遥测行加 `TunnelID`（+`ReachReport` 加或删） | P-1 判据要用它，是阶梯的硬前置；改动面小、只到 wire/agent/控制器/控制台 | **是** |
| M-1 | path_id 每隧道唯一（`PathIDBase` 不再恒 1000） | 唯一会**静默改在线转发**的一条；纯编号派生，不动密钥 | 否（阶梯改的是同一条隧道的 peer） |
| M-2 | 每隧道一把 WG keypair（identity 多把 + keybinding 主语 + 控制器按 (node,tunnel) 存 + agent 装 peer 用隧道私钥） | 9.1 的根因，工作量最大（密钥生命周期、担保、轮换）；配置面可行性见 9.1 第 13 条 Q2（实测 0） | 否 |
| M-3 | 放宽 pair 门禁为 (a,b,color)，批量/扇出带颜色，同色重复仍拒 | 必须排在 M-2 之后：门禁放宽到没有数据面支撑 = 把硬错换成静默失败（同条 Q1：不放 keypair 就是 -116） | 否 |
| M-5 | 把 9.7 那三处判据显式收紧成 (local, peer) | 去掉对"/32 全局唯一"的隐式依赖，和 M-4 一批做几乎免费 | 否 |
| M-6 | 拓扑边明细补颜色列；lab 断言键改 (pair,color) | 展示与测试脚本，最后 | 否 |
