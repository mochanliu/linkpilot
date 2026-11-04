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

背景是产品要求（每台盒子 **4096** 条隧道 —— 2026-11-04 用户更正口径：软件要支持 4096，
这条线以前写成"目标 3000、软件层 4096"是两套数字混着说，以 4096 为准。**2026-11-04 已按这条线
实测，结论在 §9.6**）。
**同两台设备之间的隧道上限是 8 条，
颜色可以重复**——8 条全是颜色 0、或全是颜色 1 都合法（用户 2026-11-03 更正：他从未要求
"同对端的隧道颜色互不相同"，之前把它推成"8 个颜色 ⇒ 8 条"并要求判据带颜色，是我的擅自设定）。
下面每一条都我自己读到过那一行，不是转述。**修它们一律用新搭的一次性私有环境**（用户
2026-11-03 规则：修缺陷期间不许动现有 lab），改完再由我按最新代码重搭 lab。

### 9.1 硬错：第二条根本建不出来

1. **控制器上游门禁**：[internal/underlay/tunnel.go:100-103](../controller/internal/underlay/tunnel.go)
   在 `create()` 最前面按**无序设备对**查 `pairTunnel`（[:313-326](../controller/internal/underlay/tunnel.go)），
   命中就回 `ErrPairTaken`（[state.go:20-24](../controller/internal/underlay/state.go)，API 层转 409）。
   判据只认设备对。错误文案自己把原因说清了：「每台设备只有一把 WireGuard 静态密钥，
   VPP 的 peer 表按公钥跨接口去重，第二条会被数据面以 -116 拒掉」——**这句要改**：
   "peer 表按公钥跨接口去重"是 VPP 插件的实现，不是 WireGuard 协议的限制（见本条第 13 项）。
2. **根因：一条设备一把 WG keypair，撞上 VPP 插件的全局 peer 池**。私钥只有一把
   （[internal/agent/identity.go:177-192](../controller/internal/agent/identity.go)），
   对账时 `priv := t.id.WGPrivateKey()` 一次取出、传给**每一条** Link
   （[internal/agent/tunnel.go:422](../controller/internal/agent/tunnel.go) → `PrivateKey: priv` :612）。
   于是同一对端的第二条隧道要装**同一把对端公钥**，撞上 -116：
   [internal/agent/dataplane.go:1094-1112](../controller/internal/agent/dataplane.go) 那段分支就是这个。
   挡路的代码在上游 VPP 26.06 里，逐行是：`src/plugins/wireguard/wireguard_peer.c` 的
   `wg_peer_add()` 在装 peer 之前 `pool_foreach (peer, wg_peer_pool)` **只比 `peer->remote.r_public`**
   （不比 `peer->wg_sw_if_index`），命中就 `return VNET_API_ERROR_ENTRY_ALREADY_EXISTS`（=-116）——
   `wg_peer_pool` 是**全插件一个池**，所以"跨接口去重"是它的实现事实。
   这条不是随手加的校验，收包侧确实依赖它：同文件上方 `wg_remote_get()`（`wireguard_if.c`）
   是握手 initiation 到达时"这个发送者是谁"的唯一入口，它 `wg_peer_walk` 遍历**全局池**、
   只按对端公钥匹配，**报文从哪张接口进来不参与判断**；两张口共用一把对端公钥时，
   应答会被挂到池里先命中的那条 peer 上（peer 记录里的 `wg_sw_if_index` 只有一个）。
   ⇒ 结论：**在 VPP 上做同对端多条隧道，必须每隧道一把 keypair**（第 13 项 Q2 实测 0/0），
   或者改 VPP 插件把 peer 查找按接口收口（分叉上游代码 + 每台盒子重编 VPP）。
   走前一条的话，牵连三处：
   设备身份要持有多把（identity 现在只存一把）、
   密钥担保的主语要扩（[internal/keybinding/binding.go:115-195](../controller/internal/keybinding/binding.go)
   的 `Message/Verify` 只签 `(tenant, node_id, key_gen, x25519_pub)`，没有隧道维度）、
   控制器存的 `prec.Binding` 是按设备存的（[cmd/sr-ctrl/underlay_delivery.go:113-117](../controller/cmd/sr-ctrl/underlay_delivery.go)）。
3. **批量与扇出按设备对折叠**：`plan.go` 的自动批：[:105-108](../controller/internal/underlay/plan.go)
   命中 `HasPair` 就 `Skip{"这对设备之间已经有隧道，跳过"}`，`HasPair`
   同样只比设备对（[:311-321](../controller/internal/underlay/plan.go)）；
   `Fan` 一批一个颜色、每个对端一条（[:41-96](../controller/internal/underlay/fan.go)，
   单条 create 被拒就变成一条 `FanSkip`）。
   症状比硬错更难查：full-mesh / hub-spoke 永远补不出同对端的第 2..8 条，拓扑上留下没人解释的洞。

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
      挡路的是 VPP 的 peer 表以**对端公钥**为键且键里不含接口，所以"换成本地私钥不同就能救"
      是不成立的。
    - **"端口做主键"在 VPP 的配置面完全不设防**：Q4 连"同私钥同端口"都收，说明 VPP
      既不按公钥也不按端口给 WG 设备去重。所以"每隧道每端口唯一"这条不变式**只有我们的
      分配器在守**（`allocPort`，[state.go:296-339](../controller/internal/underlay/state.go)），
      放开门禁不会撞上 VPP 的端口检查，但也不会得到 VPP 的任何保护。
    - **只放宽 pair 门禁而不给每条隧道一把对端 keypair，盒子侧第二次 `wireguard_peer_add`
      仍回 -116** —— 那是把控制器里的硬错换成"控制器说已下发、agent 装不上"的静默失败。
      Q2 的 0/0 是每隧道一把 keypair 值得做的直接证据；它证明的是**配置面收得下**，
      两条隧道各自跑通的**数据面**判据要在实施时另测（握手互不串台、BFD/探针各自成会话），
      不能拿这张表当已验。

14. **2026-11-03 补测：内核 WireGuard 侧，用户的判断成立**（我原先把 VPP 的实现说成了
    WireGuard 的限制，那是我说错了）。同一台 `mtn-node1`（LinuxKit 内核 7.0.14，
    内核自带 wireguard 模块；容器里的 iproute2 不认 wireguard 参数，改装
    `wireguard-tools v1.0.20210914` 后测），号段 51977–51980、endpoint 一律 TEST-NET：

    | 问的是 | 形状 | 内核回的值 |
    |---|---|---|
    | K1 用户的原话形状 | 三张接口**各一把本地私钥**，装**同一把对端公钥**、各挂不同 endpoint | 三张**全收**，`wg show <if> peers` 每张都列出同一把公钥 |
    | K3 同接口重复 | 同一张接口再把同一把公钥 add 一次（换 endpoint） | **收**（内核把 peer 表挂在接口上，这就是"更新 endpoint"那条路径） |
    | K6 本地私钥跨接口共用 | `wgA(priv1,51977)` 与 `wgD(priv1,51980)` | **两张都建成**，内核不拦 |

    ⇒ "多台隧道共用一套公私钥、靠监听端口区分"在**内核 WireGuard 上是原生支持的**：
    peer 表是**按接口各自的**，本端接口由 UDP 目的端口选中，公钥只在"同一张接口内部"当键。
    VPP 26.06 的插件不是这个形状：一个全局 `wg_peer_pool` + 全局 `index_table`，
    握手 initiation 靠 `wg_remote_get()` 遍历全局池按公钥命中（见第 2 条），
    报文从哪张口进来不参与判断。**所以 -116 是 VPP 的实现约束，不是协议约束。**
    两条可走的路，代价差得很远，要拍一条：
    - **甲：每隧道一把 keypair**（M-2）。不动 VPP，代价是设备身份从一把扩到多把、
      密钥担保主语加隧道维度、轮换与开机重放跟着改。
    - **乙：分叉 VPP 的 wireguard 插件**，把 peer 查找收口到接口内（`wg_peer_add` 的去重
      加 `sw_if_index`、`wg_remote_get` 带上到达接口），密钥模型一行不改、且**与内核 WireGuard
      语义一致**。代价：VPP 不再是 packagecloud 的官方包，每台盒子（含 lab 镜像与物理盒）
      都要自己编译维护 VPP，升级 VPP 版本时这块要跟；改的是收包热路径，要重测握手与吞吐。

    我原先写的"VPP 的 peer 表按公钥跨接口去重"这句话本身没错（Q1 就是它），
    但我据此说成"WG 原生不支持共用一套密钥"，是把 VPP 当成了协议，收回。

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
    拓扑边折叠成 `firstRelayed || tunnels[0]` + "N 条"标签是刻意的，但边明细缺"隧道号"列
    （颜色可重复 ⇒ 只有颜色不够认，见 §9.5 M-6）。
12. **lab 脚本**：[test/lab/loop-console.py:328-333,354-358](../test/lab/loop-console.py)
    用 `frozenset(设备对)` 做集合等值断言，隐含"一对一条"；属 lab，重搭 lab 时随最新代码改。

### 9.5 修复次序（每条都要先出方案、点头后再写码）

| 序 | 内容 | 为什么这个位置 | 阶梯 #222 是否等它 |
|---|---|---|---|
| M-4 | 遥测行加 `TunnelID`（+`ReachReport` 加或删） | P-1 判据要用它，是阶梯的硬前置；改动面小、只到 wire/agent/控制器/控制台 | **是** |
| M-1 | path_id 每隧道唯一（`PathIDBase` 不再恒 1000） | 唯一会**静默改在线转发**的一条；纯编号派生，不动密钥 | 否（阶梯改的是同一条隧道的 peer） |
| M-2 | **已拍：走甲，但粒度是"每台设备固定 8 把 keypair，槽号 = 隧道在设备对内的序号"**（不是每条隧道新生一把），落地方案见 §10 | 9.1 第 2 条的根因。可行性见 9.1 第 13 条 Q2（两把对端公钥实测 0/0）；本地私钥跨口复用有内核 K6 与 VPP Q3/Q4 的支撑，但只有配置面验过 | 否 |
| M-3 | 放宽 pair 门禁：**同设备对最多 8 条，颜色可重复**（不是按 (a,b,color) 判重）；批量/扇出同一色可以连发多条 | 必须排在 M-2 之后：门禁放宽到没有数据面支撑 = 把硬错换成静默失败（Q1：不放就是 -116） | 否 |
| M-5 | 把 9.7 那三处判据显式收紧成 (local, peer) | 去掉对"/32 全局唯一"的隐式依赖，和 M-4 一批做几乎免费 | 否 |
| M-6 | 拓扑边明细补"隧道号/颜色"列；lab 断言键改"设备对 + 条数 ≤8" | 展示与测试脚本，最后 | 否 |

### 9.6 规模实测（2026-11-04，一次性私有台 `m2v-node1`）：4096 这条线第一个卡住的不是 VPP，是**我们自己的客户端**

用户 2026-11-04 口径：**软件要支持每盒 4096 条隧道**。§9.1 第 2 条说的那道 `MAX_PEERS`
检查到底是多少，以及"改成 8 把之后心跳重验的开销"（§10.8 那两条待量的），这轮都在同一台
带 VPP 的一次性私有台上量完了。环境：Docker Desktop daemon、镜像 `srwg-dev:latest`、
`--privileged`、容器名 `m2v-node1`（不碰 `lp-*` / `srwg-*` / `osc-*` 的活现场）、
VPP v26.06-release、主线程 `taskset` 到 CPU 13（避开 CPU 0 那个 4–11ms af_packet 唤醒惩罚）、
无大页、startup 里 `buffers { buffers-per-numa 8192 }`；探针与基准都用**我们自己的**
`internal/vppapi` 客户端，形状按产品形状来：**每隧道一口（各自的本地私钥与监听端口）+
该口一个 peer（各自的对端公钥）**，号段 instance 7000+、端口 41000+、地址一律 TEST-NET。
本地临时工具与日志都不进仓库（`/Users/louis/.qoder-tmp/m2v`，其中
`artifacts/2026-11-04-evidence.txt` 是这一轮现场抓的原始输出：版本、4600 口、RSS、
两段内存、三条 dump 的报错原文、基准三遍里的一遍）。

**一、4096 在 VPP 配置面是过得了的（前后两组数字）**

| 量什么 | 改前（0 条隧道） | 改后（4600 条隧道） |
|---|---|---|
| `CreateInterface` / `AddPeer` 逐条建 | — | **4200/4200 全成功，0 个 API 错误**，耗时 9–10 s |
| `show interface` 独立数 `wg*` 行数 | 0 | **4600**（400 条递增测试留下的 + 这一轮 4200） |
| VPP 主进程 VmRSS | 329248 kB | **374020 kB** |
| 统计段（stat segment）用量 | — | 总量固定 **32.00m**，used **14.54m** / free **17.46m** |
| 主堆 | — | 总量 **1.00G**，used 278.62m / free 745.38m |

单看这一轮 4200 条的增量：329900 kB → 371972 kB = **+42.1 MB ⇒ 约 10 kB/条隧道**；
按 4096 条算 ≈ **+40 MB**。统计段那条更有用：**每口计数器约 3.2 KB**（14.54 MB / 4600），
段总量 32 MB ⇒ 数学上限约 **1 万口**。

**二、真正的墙：7868 口时 VPP 直接 abort，而**不是** `MAX_PEERS` 拒绝

把 instance 一路加上去，三次跑到**同一个数**：第 **7867** 口建成，第 **7868** 口 VPP
自己 `os_panic()`（SIGABRT）退出，日志原文是 `Out of memory, calling os_panic()`，
栈在 `vlib_stats_create_counter` ← `vnet_register_interface`。逐条排除：

- 主堆此刻还剩 **729 MB**（见上表同一台盒子），**不是**主堆不够；
- 宿主当时可用 3.4 GB，容器 cgroup `memory.max = max`、RLIMIT 全部 unlimited，**不是**物理内存或配额；
- `wireguard_peer.c` 里那个 `pool_elts (wg_peer_pool) > MAX_PEERS → VNET_API_ERROR_LIMIT_EXCEEDED`
  一次都没回过（7867 条 peer 全都 0），**所以 §9.1 第 2 条要我"量出来的那个上限值"根本不是限制项**；
- 唯一在涨的是那 32 MB 统计段，每口 ~3.2 KB，7868 口 ≈ 25 MB 加上别的常驻计数正好顶到底。
- `user_instance` 另有一条独立的硬线：**0..16383 合法，16384 直接 `wireguard_interface_create`
  回 retval=-31**（4 倍于 4096 的余量，暂时不构成问题）。

**这条要单独点名（AGENTS.md §1 第 4 条：必然中断的操作必须写出来）**：撞墙时的失败形态是
**整个 VPP 进程 abort**，也就是**这台盒子上所有客户流量一起断**，不是"第 N 条隧道建不出来"。
⇒ 控制器/agent 必须有一个**比 VPP 更早**的隧道数闸门，并且闸门值不能贴着 7868 放。
本轮没测 7868 之下的其他资源（wg4 转发表、`sr_obs` 观测表、密钥环）在这规模下够不够，
只测了配置面能建到这儿。

**三、第一个产品上限是我们自己的代码：`detailQueueLen = 256`**

[internal/vppapi/vppapi.go:53](../controller/internal/vppapi/vppapi.go) 把每条 dump 的
details 通道做成 256 格，`c.dump()`（[:409-469](../controller/internal/vppapi/vppapi.go)）
的收法是**先等栅栏 `sr_wg_stats_get` 回来、再一次性排空通道**，通道溢出时
`dispatch`（[:270-294](../controller/internal/vppapi/vppapi.go) 里那个非阻塞 send）把
`w.full` 置位并**丢弃**多出来的帧，最后**整包报错**：
「结果多于 256 条，已被截断（这个包不该继续用它）」。二分实测的边界：**240 条隧道能收齐
（sw 行 241），260 条就报截断**；这一轮在 4600 条规模上复核，三条全红：

```
sw_interface_dump（心跳清单走的就是这条）：err=... 结果多于 256 条，已被截断
wireguard_interface_dump：err=... 已被截断
wireguard_peers_dump：err=... 已被截断
```

症状按调用点逐条说，都是**产品路径**上已经在用的：

- 心跳设备清单 [internal/agent/inventory.go:80](../controller/internal/agent/inventory.go)
  （它落到 [messages.go:908](../controller/internal/vppapi/messages.go) 的 `sw_interface_dump`）⇒
  盒子一过 256 口就**再也上报不了清单**，控制台看到的是一台"清单未上报"的盒子；
- 升级认领 `adopt` [internal/agent/dataplane.go:512](../controller/internal/agent/dataplane.go) ⇒
  清单读不齐就没法认领现成接口，§10.6 那条"0 丢包"的路径在 257 口以上直接不成立；
- peer 对账与 -116 诊断 [:1122 / :1301](../controller/internal/agent/dataplane.go)、
  隧道对账 [tunnel.go:491](../controller/internal/agent/tunnel.go)、
  接口落地核验 [tunnel.go:424](../controller/internal/agent/tunnel.go)；
- 遥测两条（探针与 BFD 会话）
  [internal/vppapi/telemetry.go:130 / :287](../controller/internal/vppapi/telemetry.go)；
- 路由两张表 [internal/vppapi/fib.go:374 / :407](../controller/internal/vppapi/fib.go)、
  观测表 / 路径表 / 隧道表 / 端口表 / 地址表
  [messages.go:274 / :612 / :700 / :765 / :970](../controller/internal/vppapi/messages.go)。

**产品代码里 13 处 `c.dump()` 调用点全都在 257 条上断**（另 1 处在 live 测试
[internal/vppapi/live_vrrp_test.go:95](../controller/internal/vppapi/live_vrrp_test.go)），
比 4096 这条线早 **16 倍**。
要动的方向（**待拍，我不动代码**）：把 dump 改成边收边排（`callStream`
[:356-375](../controller/internal/vppapi/vppapi.go) 那种并发排空的形状目前只有
`mss_clamp_get` 在用），或者把 details 直接汇聚进调用方的切片、只保留栅栏而不用固定 256 格通道。

**四、心跳重验 8 把公钥的开销（同一轮，独立跑两遍取中位）**

容器内基准 `keyringbench`（临时工具，不进仓库），**独立跑三遍取中位**
（2000×3 两遍、3000×5 一遍）：

| 量什么 | 一遍（今天） | 八把（M-2 之后） |
|---|---|---|
| 单次 `Binding.Verify`（含内部 base64 解码） | **68.0–73.7 µs** | 一台设备一轮 **559.5–607.2 µs**（倍率 7.9–8.6×，线性） |
| 纯 base64 解一把公钥 | 80–94 ns，占八把心跳的 **0.1%** | ⇒ 开销全在 Ed25519 验签本身，解码可以忽略 |
| 100 台一轮全带 | 6.8–7.4 ms | **55.9–60.7 ms** |
| 1000 台一轮全带 | 68–73.7 ms | **559–607 ms** |
| 5000 台一轮全带 | 340–368 ms | **2.80–3.04 s** |

结论不是"8 把太贵"，而是**贵的那一段被放在全局锁里**：`applyBindings`
（[cmd/sr-ctrl/main.go:2018-2064](../controller/cmd/sr-ctrl/main.go)）在 :2046 逐槽
`b.Verify(rec.TenantID, rec.NodeID, id)`，而"这槽没变、跳过"的 continue 在 **:2063**，
在它后面 ⇒ **每次心跳把 8 把全验一遍，哪怕一把都没轮换**；这段跑在 `s.mu` 里，
同一个 handler 接着还要 `s.save()`（:1447）。1000 台 ⇒ 一轮心跳约 **0.6 s 持锁做纯计算**，
这才是 300 ms 遥测那条链路上的抖动源（300 ms 比它自己还短）。
方向（**待拍**）：先比对库里存的公钥字节，只在变了时验签；以及/或者把验签挪出 `s.mu`。

**五、附带发现（不在 4096 这条线上，但坑过本轮测量）**

仓库里预编译的 `vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so` 是 **10-30 22:15**，
而 `vpp-plugin/sr_wg.api` 已经改到 **11-03 11:35** ⇒ 用预编译 .so 起盒子，`Dial` 的
CRC 交叉核对（[internal/vppapi/messages.go:139-156](../controller/internal/vppapi/messages.go)）
直接拒绝整条消息表（本轮就是这样，现场重编译插件后才通）。也就是说**这条闸门是有效的**，
但它把"预编译产物过期"变成"客户端连不上"，排障时要一眼看出来。

---

## 10. M-2 落地方案：每台设备固定 8 把 WG keypair，槽号 = 隧道在设备对内的序号

用户 2026-11-03 拍板，原话：「咱们既然定死了同一对儿设备最多 8 条隧道，那咱们就每台设备有 8 个私钥，
只有同一对儿设备才用不同私钥，不同对儿设备可以复用。就这样实现。」
本节把这句话翻译成能照着改的清单，并点名里面含协议变更与升级顺序。**这一节点头之前不写代码。**

### 10.1 规则本身

1. 每台设备固定 8 把 X25519 keypair，下标 0..7，下面叫**密钥槽（slot）**。
2. 同一对设备之间的第 n 条隧道，**两端各自用自己的第 n 槽**。所以隧道记录里要新增 `Slot`，
   它是"这条隧道在这个设备对里的第几号"，由控制器在建隧道时分配，建好不再变。
3. 不同设备对**可以**撞同一个槽号：A-B 的第 1 条和 A-C 的第 1 条都用 A 的第 1 把私钥。
4. 一个设备对最多 8 条 ⇒ 槽分配失败（0..7 全占）是新的硬错 `ErrPairFull`，替换今天的 `ErrPairTaken`。
5. 槽与颜色无关，颜色可重复（见 §9 开头）。

### 10.2 为什么这条规则刚好堵住 VPP 那个洞（论证 + 它的两个前提）

场景：盒子 A 上会同时挂 N 张 WG 口，每张口装一条 peer，peer 的键是**对端那把公钥**；
VPP 的 peer 池是全局一张、只比对端公钥（§9.1 第 2 条）。所以只要满足下面这句就不会撞 -116：

> **A 的 peer 池里，任何两条记录的"对端公钥"彼此不同。**

逐条对上 10.1：

- 同一对 (A,B) 的第 n 条 ⇒ 装的是 B 的第 n 把；同对内的第 m 条装 B 的第 m 把。
  一台设备的 8 把公钥彼此不同（前提①），所以 n≠m ⇒ 两把不同。
- 不同对 (A,C) 即使同槽 ⇒ 装的是 C 的公钥，和 B 的任何一把都不同（不同设备密钥彼此独立，前提②）。
- ⇒ 两条隧道不会在 A 上撞键。这正是用户说的"只有同一对设备才需要不同私钥"为什么够用。

反过来的那一半——**本地私钥跨口复用**（A 的第 n 把同时是通往 B 和通往 C 两张口的本端密钥）——
VPP 收（Q3/Q4，§9.1 第 13 条），内核 WireGuard 也收（K6，第 14 条）。
但这两组实测的只是"配置面能不能装上"，**不是**"两口号共用一把私钥时握手和转发真的不串"。
所以 10.6 里有一条专门量它。

要写进校验的两个前提：① 一台设备的 8 把公钥两两不同（生成后自检）；
② 同一设备对内槽号不重复（由 10.1 第 2 条的分配器保证，`ErrPairFull` 兜底）。

### 10.3 存量隧道一律不动：槽 0 就是今天那把私钥

**场景**：升级这一条之前，lab/真机上已经在跑的隧道用的是设备上唯一那把 keypair。
如果新代码给"第 1 条隧道"发一把新私钥，两端 peer 都要换 ⇒ WG 要重握手 ⇒
隧道口是 delete-then-add ⇒ **客户流量会掉一下**。这是 AGENTS.md §1 第 6 条要拦的事。

**做法**：磁盘上现有的 `wg_x25519.key`（[identity.go:28-36](../controller/internal/agent/identity.go)
那个 32 字节文件，`loadOrCreateX25519` 读写）直接**当成槽 0**，字节一个不改；
新增的是槽 1..7（`wg_x25519.slot1.key` … `slot7.key`，agent 启动时缺哪个补哪个）。
迁移时把存量隧道全部记为 `Slot=0`（照 [plan.go](../controller/internal/underlay/plan.go) 里
`#135` 那次"旧数据迁移"的现成写法）。
⇒ 槽 0 的公钥和今天完全一致 ⇒ 对端 peer 不用动 ⇒ `Link.adopt`
（[dataplane.go:441-502](../controller/internal/agent/dataplane.go)，它按
`ifi.PublicKey == l.wgPub` 认领现成接口）认领得上 ⇒ **升级本身 0 丢包**。
只有新建的第 2..8 条才会用到新槽。

### 10.4 担保（keybinding）要加槽号：这是协议变更，必须点名

**现状逐层看**：签名的字节是 `(域串, tenant, node_id, key_gen, x25519_pub)`
（[binding.go:132-147](../controller/internal/keybinding/binding.go)），结构体里只有**一把**公钥
（`Binding{X25519Pub, KeyGen, Sig}`，:63-67），控制器按设备存一把
（`NodeRecord.Binding`/`KeyGen`，[main.go:127-133](../controller/cmd/sr-ctrl/main.go)），
下发的隧道规格里也只塞一把（`TunnelSpec.PeerBinding`，
[wire.go:534-536](../controller/internal/wire/wire.go) ← `prec.Binding`，
[underlay_delivery.go:113-136](../controller/cmd/sr-ctrl/underlay_delivery.go)）。
**主语里没有隧道/槽的位置**——这就是"一台设备一把密钥"这件事在担保层的固化。

**改法**：`Binding` 加 `Slot uint8`；`Message` 在 `key_gen` 之后追加 1 字节槽号（定宽、
自定界，符合 :119-128 那条"每个字段要么定长要么带长度前缀"的规则）；
`DomainSeparator` 从 `srou-keybinding-v1` 升 `srou-keybinding-v2`。

**为什么必须换域串**：域串存在的理由就是"一份签名不能覆盖另一种含义的字节"（:41-45 注释，
`TestWhyFieldsAreLengthPrefixed` 就是钉这个的）。签名覆盖的字节变了，域串不换，
就等于允许"旧形状的签名"在新校验里被接受。
⇒ **代价：旧 agent 签的 v1 binding，在新控制器上验不过**（`b.Verify` 直接 return err，
[main.go:2011](../controller/cmd/sr-ctrl/main.go)）。这不是可以忽略的小事，见下面的升级顺序。

**升级顺序（把中断算清楚再动）**：
1. v1 签名在新控制器上验不过时，`applyBinding` 是**直接返回错误、不覆盖已存的那把**
   （[main.go:2011-2013](../controller/cmd/sr-ctrl/main.go)），所以那台设备的存量担保还在，
   `tunnelSpecLocked` 的"没上报公钥担保"跳过分支（[underlay_delivery.go:113-118](../controller/cmd/sr-ctrl/underlay_delivery.go)，
   行为由 [underlay_delivery_test.go:96](../controller/cmd/sr-ctrl/underlay_delivery_test.go) 钉着）
   不会因为版本换代的这一步被触发。
2. 反过来（新控制器 + 旧 agent）也一样：旧 agent 报的 v1 会被拒，存量值同样保留。
3. ⇒ **真正的窗口代价是"建隧道"这个动作**：agent 与控制器没有都换到 v2 之前，
   新建或修改的隧道会拿到一份对端验不过的担保（`VerifyPeerCert` 用 v2 域串重建消息，
   [binding.go:239-272](../controller/internal/keybinding/binding.go)），装不上 peer。
   所以这必须是**同一个维护窗口里一次性升完**（控制器 + 全部 agent 同一批），不能拖。
4. **已在跑的客户流量不掉**，理由是热更路径不重配没变化的东西：隧道规格里 peer 那一段
   只在 spec 变化时重新下发，而 agent 重启走 `Link.adopt`
   （[dataplane.go:441-502](../controller/internal/agent/dataplane.go)）按
   `ifi.PublicKey == l.wgPub` 认领现成接口；槽 0 的公钥字节不变（10.3）⇒ 认领得上 ⇒
   VPP 不重启、不删口。**这句要在一次性环境实测出丢包数与时长才能当结论**，
   现在它是设计依据，不是测量结果。

**已定案（2026-11-03 用户拍，原 10.7 第 2 条）**：`key_gen` 每槽一个计数器。
设备级共用一个意味着"换第 3 槽的密钥"会把另外 7 槽的代次一起推进，
对端就没法区分"哪一把变了"；控制器侧的防重放判据（`b.KeyGen < cur.KeyGen`，
[main.go:2056-2060](../controller/cmd/sr-ctrl/main.go)）现在也是**逐槽**比的，
`TestApplyBindingsRotationAndReplayPerSlot` 钉住"槽 2 涨到 2 不许让槽 0 看起来也换了"。

### 10.5 改动清单（按落点，一处一行）

| 落点 | 现在是什么 | 要改成 |
|---|---|---|
| [identity.go:65-76](../controller/internal/agent/identity.go) `Identity.wgPriv/wgPub` | 一个槽位 | 8 把（下标 = 槽），加载时缺哪槽补哪槽，槽 0 沿用 `wg_x25519.key` |
| [identity.go:155-192](../controller/internal/agent/identity.go) `loadOrCreateX25519`/`setWGKey`/`WGPrivateKey`/`WGPublicKey` | 单把读写 | 加 slot 参数；新增"8 把公钥两两不同"自检 |
| [identity.go:310-336](../controller/internal/agent/identity.go) `Binding()`/`RotateWGKey()` | 签那把、换那把 | `Binding(slot)`/`RotateWGKey(slot)`，`key_gen` 按槽 |
| [binding.go:63-67,132-147,195-214](../controller/internal/keybinding/binding.go) | 主语无槽，域串 v1 | `Binding.Slot`、`Message` 带槽字节、域串 v2 |
| [sr-agent/main.go:271,328,441-450](../controller/cmd/sr-agent/main.go)、[report.go:47-53](../controller/cmd/sr-agent/report.go)、[client.go](../controller/internal/agent/client.go) | 心跳/mapping 带**一把** binding | 带 8 把（一次报完，控制器按槽落位）。**不留"旧设备只带槽 0"的兼容读路径**（定案见 10.7 第 3 条）：一份 v1 签名在新域串下验不过，那是真拒，不是半信 |
| [main.go:127-133,2003-2032](../controller/cmd/sr-ctrl/main.go) `NodeRecord.Binding`/`applyBinding` | 每设备一把 + 单一计数器 | 每设备 8 槽数组、逐槽验并入库（一把坏只砸那一格）、`changed` 只报真实变化。**控制器侧的旧 `key_binding` 不迁移而是丢弃**：那份签名的 v1 域串在新校验下验不过，留着就是往对端 relay 一份对方必然拒收的担保；agent 下一次心跳就重报整环 |
| [underlay_delivery.go:113-136](../controller/cmd/sr-ctrl/underlay_delivery.go) `tunnelSpecLocked` | `PeerBinding: prec.Binding` | `prec.Bindings[t.Slot]`；该槽没上报 ⇒ 沿用现有"没上报公钥担保"跳过文案 |
| [wire.go:414,611,726,536](../controller/internal/wire/wire.go) | 四处单把字段 | `MappingReport`/`HeartbeatRequest`/`PeerInfo` 变数组；`TunnelSpec.PeerBinding` **保持单把**（它本来就是一条隧道一份，天然带槽） |
| [tunnel.go:422,612](../controller/internal/agent/tunnel.go) `priv := t.id.WGPrivateKey()` | 一次取出传给**每一条** Link | 按这条隧道的槽取；`LinkConfig.PrivateKey` 已是 per-link（[dataplane.go:172-174](../controller/internal/agent/dataplane.go)），不动语义 |
| [tunnel.go:100-103,305-326](../controller/internal/underlay/tunnel.go) `create`/`pairTunnel` | 撞设备对就拒 | `pairSlots(a,b)` 分配最低空闲槽；满了 ⇒ `ErrPairFull`；**错误文案里"VPP 按公钥跨接口去重"改成"VPP 插件的 peer 池全局按对端公钥去重"**（见 §9.1 第 14 条） |
| [plan.go:105-108,311-321](../controller/internal/underlay/plan.go) `HasPair`、[fan.go:41-96](../controller/internal/underlay/fan.go) `Fan` | 每对只发一条 | 批量/扇出带条数，>8 才拒 |
| [state.go:20-24,119-121](../controller/internal/underlay/state.go)、[console.html:5146-5148](../controller/cmd/sr-ctrl/console.html) | "一对一条"文案 | 同上新口径 |

**顺序硬约束（2026-11-03 更正）**：M-1（path_id 每隧道唯一，§9.2 第 4 条）**必须先于 M-3**
（放开"同设备对 ≤8 条"那道门），而不是先于 `Slot`。理由：`PathID = base + PeerNodeID` 只按
设备对算，同设备对的第二条隧道会和对端抢同一个 path_id，后写的段列表静默覆盖前一条、retval 全 0
——那是真会改在线转发的那一条。而 `Slot` 本身不制造第二条隧道：**slice 1 落地的同时同设备对仍然
只允许一条**，槽号全程是 0，磁盘上那把旧密钥就是槽 0，所以 M-1 没做也不会撞上覆盖。把 `Slot`
拆在 M-1 之前反而更好：它把"槽"这条通路一次打通并让存量零中断，M-3 只需要改分配器和门禁。

**slice 划分与落地状态**：
- **slice 1（本增量，已落地并通过单测）**：上表前九行——agent 8 把密钥与逐槽担保、`keybinding` v2
  带槽、wire 三处数组化、控制器逐槽验存、`tunnelSpecLocked` 按 `t.Slot` 取对端担保、
  `TunnelSpec.Slot` 下发、设备侧按槽取私钥装 peer 并拒绝槽号不一致的规格。
  隧道模型里 `Tunnel.Slot` 恒 0（分配器还没放开第二条），行为与升级前一致。
- **slice 2 = M-1**：path_id 每隧道唯一。
- **slice 3 = M-3**：`pairSlots(a,b)` 分配最低空闲槽 + `ErrPairFull` + 扇出/批量带条数 +
  上表后四行的"一对一条"文案口径（含 [console.html:5146](../controller/cmd/sr-ctrl/console.html)）。

### 10.6 判据（做完怎么算过，前后两组数字都要有）

- 单测：8 把公钥两两不同；对内槽号不重复；槽满报 `ErrPairFull`；v1 形状的签名在 v2 域下**必须**验不过；
  `state.json` 旧格式迁移后槽 0 公钥与迁移前逐字节相同。
  **slice 1 已钉住的部分**（`go test ./...` 全绿，容器内 `--cap-add=NET_ADMIN`）：
  两两不同与撞槽拒启动 [`TestLoadOrCreateKeysAndPermissions`/`TestLoadOrCreateRefusesDuplicateSlotKeys`](../controller/internal/agent/identity_test.go)、
  槽号越界五个入口 [`TestSlotIsBoundsChecked`](../controller/internal/agent/identity_test.go)、
  逐槽担保与逐槽重放 [`TestApplyBindings*`](../controller/cmd/sr-ctrl/keyring_test.go)、
  v1 签名被拒 [`TestV1SignatureIsRefused`](../controller/internal/keybinding/binding_test.go)、
  v1 标量 `key_gen` 迁进槽 0 [`TestDecodeStateMigratesV1KeyGen`](../controller/internal/agent/identity_test.go)、
  下发与落地带槽 [`TestBundleSpecCarriesTheTunnelsKeySlot`](../controller/cmd/sr-ctrl/underlay_delivery_test.go)、
  [`TestTunnelSyncInstallsKeySlotFromSpec`/`...RefusesSpecWithMismatchedBindingSlot`/`...RefusesOutOfRangeKeySlot`](../controller/internal/agent/tunnel_test.go)。
  "对内槽号不重复 / 槽满 `ErrPairFull`" 属于 slice 3（M-3），现在还没有这条路径。
- **只用新建的一次性私有环境**（`srwg-*` / `osc-*` / 环路台都是别的会话的活现场，不许动）。
- **slice 1 的控制面判据已量（2026-11-03，同一私有环境 `m2-*` / 网络 `m2net` 10.137.0.0/24，
  跑完 `m2` 残留 0 个、`lp-*` 十二个容器一台没动）**：
  `test-vbond.sh` 改前 **196 passed / 0 failed**，改后 **201 passed / 0 failed**（+5 条是
  按槽下发、槽号不一致拒收、逐槽担保与逐槽重放、越界拒绝）。日志在宿主 `/tmp/m2-priv/`。
- **①～⑤ 还没量（都不属于"改代码就能过"的范畴），⑥ 已量完（2026-11-04）**：
  ①～⑤ 那五项要求**同一对设备真的有两口隧道**，而 pair 门禁要到 slice 3（M-3）才放开 ⇒
  在 slice 3 之前这五项只能靠私有环境里手工往 VPP 塞第二口来测，测出来的不是产品路径。
  所以它们记在 slice 3 的收口判据里，不在这里假称已过。
  ⑥ "升 agent 不中断"（`adopt` 认领现成接口，10.7 第 3 条依赖的那句话）需要**带 VPP 的
  私有环境**：当时那台 `m2-*` 只有控制面（`sleep infinity` 覆盖镜像启动命令，不起数据面），
  所以那天"0 丢包"还只是**设计依据**而不是实测。
  **2026-11-04 用户批准起这台带 VPP 的台子**（容器名 `m2v-*` + 独立子网 + 换
  `COMPOSE_PROJECT_NAME`，依旧不碰 `lp-*` / `srwg-*` / `osc-*` 的活现场）；同一轮里
  把 `MAX_PEERS`（每盒 4096 的目标）和心跳重验 8 把公钥的开销一起量，三组数字都写进提交说明。
  **2026-11-04 结果：这三组都已量完。** 后两组见 §9.6 一/二/三/四 节（`m2v-node1` 上现场跑的，
  跑完容器留存到本轮收口再删）；第一组"升 agent 不中断"另起一台**两台 CPE 的**私有台
  （`m2a-cpe1` / `m2a-cpe2` / `m2a-ctrl`）量了，前后两组数字、口径和没测到的部分都在下面那节
  **「升 agent 不中断」实测**里。
- 环境里同一对设备建 2 条隧道（**故意同色 0**），逐项看：
  ① 两口各自的 peer 公钥确实是不同槽（用我们的 vppapi dump 读公钥，
  **不要用 `show wireguard interface`——它打印私钥，输出不许进仓库、报告或日志**）；
  ② 两条各自握手 UP；③ BFD / 探针各自成会话、各自有遥测行；
  ④ `tools/fib-dump` 读回两个不同 path_id 且段列表各是各的；
  ⑤ 打流互不串台（隧道 1 的内层包不能被隧道 2 解出，反向亦然）——这就是 10.2 末尾欠的那条实测。
- 升级不中断：升 agent 前后同一口径打流（丢包数、时延）、邻居 uptime、VPP FIB/RIB 条数、进程 pid，
  两组数字写进提交说明。→ **2026-11-04 已实测**，见下面那节「升 agent 不中断」实测；
  时延这一格和"客户侧 BGP 邻居 uptime"这一格**这台台子给不出**，缺在哪、什么时候补，都写在小节末尾。

#### 「升 agent 不中断」实测（判据 ⑥，2026-11-04，一次性私有台 `m2a-*`）

**这台台子长什么样**：`m2a-cpe1` 和 `m2a-cpe2` 各跑一份 VPP（26.06）加一个 `sr-agent`，
旁边一台 `m2a-ctrl` 控制器；两台盒子之间就一条颜色 0 的隧道——隧道口 `wg1001`、外层口
`host-eth1`，overlay 的 /32 只在表 250。台子整体在宿主 `~/.qoder-tmp/m2a/test/lab/`，
不进仓库，跑完删。这是上一段说的那台"两台各带 VPP 的 CPE"。

**打流口径为什么是这两样**：产品探针在这条路径上发不出去——
[sr_wg_probe.c](../vpp-plugin/sr_wg_probe.c) 发包时把转发表写死成主表 0，而对端的 /32 只在表 250。
这台台子上每个 100ms 采样点都带探针行，全程是同一个形状：`tx` 一路累加（最后一轮的
首末采样点 13677 → 14177），`rx` 却一口不动 —— cpe1 上 11 个轮次的每一个采样点都是同一个
**20**，cpe2 上恒为 **0**，整段时间里两台都没有再收到过一个探针回包；`loss_permille` 两台
恒为 **1000**（千分比拉满）。
这是另一条缺陷，改它要换 `.so`、换 `.so` 要重启 VPP、重启 VPP 就会掉隧道 ⇒ 归到下轮 lab 重建一起修。
所以**时延这一格这轮没有数字**，只有"掉不掉包"这一格。能用的是隧道里真在跑的两样：

1. **BFD 会话状态时间线**，100ms 采一次（`m2a-dump -watch` 直接读 VPP 里的会话）。
   会话是 50ms 发包 × 检测倍数 3 = **150ms 判超时**，所以"整窗没有一个采样点离开 up"
   这句读数等价于"不存在 ≥150ms 的中断"。判据里那一格"邻居 uptime"在这台台子上就是它
   （这台台子没跑 FRR，客户侧 BGP 邻居不在模型里，见本节末尾的缺口清单）。
2. **接口包计数**，250ms 读一次 `show interface`，一次读两张口：隧道口 `wg1001` 和外层口
   `host-eth1`，落成 `wg.rx / wg.tx / eth1.rx / eth1.tx` 四列。稳态 ≈22pps/方向
   （BFD 占 20pps，其余零头没逐项归因，不影响空窗判据）。绝对包数缺口读"最长零增量空窗"。
   **外层口那一列是分锅用的**：隧道口停了而外层口还在涨，说明包进了 VPP 没出去，
   是我们自己的转发/加密路径；两口同时停，说明 VPP 主循环压根没被调度，是这台 VM 的问题。

**control 轮（150s，什么都不做）** —— 它唯一的用处是证明上面这套采样器自己不会造出空洞，
后面重启轮的结论才归得到重启头上。目录 `20261104-095736-control`：

| 项目 | cpe1 | cpe2 |
| --- | --- | --- |
| BFD 采样 | 1488 条，**全 up，0 个非 up** | 1488 条，全 up |
| `wg.rx` 窗口总包数 / 均速 / 最慢 / 最长零增量空窗 | 3418 / 22.18pps / 18.8pps / **0ms** | 3418 / 22.17 / 18.59 / **0ms** |
| `wg.tx` | 3416 / 22.17 / 19.01 / **0ms** | 3417 / 22.17 / 18.59 / **0ms** |
| `eth1.rx` | 3420 / 22.19 / 18.8 / **0ms** | 3420 / 22.19 / 18.59 / **0ms** |
| `eth1.tx` | 3418 / 22.18 / 19.01 / **0ms** | 3421 / 22.19 / 18.59 / **0ms** |
| 隧道身份六项（`wg_ifaces`/`peers`/`tunnels`/`paths`/`ports`/`wg_sw_ifs`） | 1488 个采样点里各只有 1 个取值 | 同 |
| VPP 表 250 前缀条数 | 7 → 7 | 7 → 7 |
| 进程 pid / 二进制 md5 | `sr-agent` 26014 不变、`vpp_main` 3325 不变 / `2a8b17c7` 不变 | 306 不变、45 不变 / `f99e3a5d` 不变 |

**restart 轮（150s，真换文件 + SIGTERM + 重新拉起，VPP 全程不动）**。目录
`20261104-100020-restart`。换进程那几步的耗时取容器内时钟（宿主是 macOS、容器在 VM 里，
跨时钟对齐会把 50ms 量级的判据糊掉）：`SIGTERM`→老进程退出 **54ms**、→新文件就位 **65ms**、
→新进程 exec **68ms**；`sr-agent` pid 26014 → 37515；`vpp_main` pid **3325 前后相同**；
md5 `2a8b17c72cd6…` → `a087cc492e9f…`（同一份源码加一个 `-buildid` 让文件哈希真的变了，
Go 的构建是可复现的，不加这一手"换了二进制"就没证据）。换完之后：

| 项目 | cpe1 | cpe2 |
| --- | --- | --- |
| BFD 采样 | 1488 条，**全 up，0 个非 up** | 1489 条，全 up |
| `wg.rx` 总包数 / 均速 / 最慢 / 最长零增量空窗 | 3484 / 22.26pps / 14.81pps / **0ms** | 3460 / 22.08 / 16.71 / **0ms** |
| `wg.tx` | 3457 / 22.09 / 16.71 / **0ms** | 3485 / 22.24 / 14.81 / **0ms** |
| `eth1.rx` | 3485 / 22.26 / 14.81 / **0ms** | 3461 / 22.09 / 16.71 / **0ms** |
| `eth1.tx` | 3458 / 22.09 / 16.71 / **0ms** | 3487 / 22.26 / 14.81 / **0ms** |
| 隧道身份六项 | 1488 个采样点里各只有 1 个取值（一个都没变过） | 同 |
| VPP 表 250 前缀条数 | 7 → 7 | 7 → 7 |
| agent 日志 | 重启后只有 `隧道配置 v2 已收敛`，**没有 `peer installed`** ⇒ 走的是认领现成接口那条路，没有重装 peer | 整轮没重启，日志无新增 |

control 和 restart 两轮的包数可以横向比：cpe1 `wg.rx` 3418 包 / 154.1s 对 3484 包 / 156.5s，
22.18pps 对 22.26pps —— **换进程那一次没有少掉任何一个包**，四列计数的最长零增量空窗都是 0ms。

**重复性**：真换了文件的 restart 轮共 **5 轮**（`093658`、`095216`、`100020`、`100515`、`100838`，
md5 逐轮 `f99e3a5d→430e43f3→2a8b17c7→a087cc49→666c7014→0d5e2d17`），合起来看：

| 每轮都量的 | 五轮的结果 |
| --- | --- |
| SIGTERM→老进程退出 | 54 / 54 / 54 / 59 / 61 ms，五轮的"等退出"轮询都只轮了 1 次就退 —— 它是自己干净退出的，没走到超时 |
| SIGTERM→新进程 exec | 67 / 68 / 69 / 96 / 109 ms |
| `vpp_main` pid 前后 | **每轮都相同**（cpe1 全程 3325，cpe2 全程 45） |
| 换进程前后 ±3s 内的 BFD 非 up 采样点 | **每轮 0 个**（五轮 × 两台） |
| 换进程前后 ±3s 内的 ≥150ms 计数空窗 | **每轮 0 个**（`093658` 那轮的采样器只有隧道口两列，后四轮四列都查） |
| 隧道身份六项 + 表 250 条数 | 每轮全窗只有一个取值 / 7→7 |

**台子的本底要如实说，这条不算升级的账**：同一个台子在**完全不动任何进程**的时候也会停，
`20261104-094933-control` 那 150s 里就出现 2 段双向停摆（BFD 时间线上 0.60～0.91s，
包计数空窗 0.52～0.78s）。全部采过的轮
（6 轮 restart，其中 1 轮没换文件、另有 1 轮 `093605` 中途放弃没留下读数；5 轮 control，60s～150s 不等）
里一共看到 **12 段**这种停摆
——其中 2 段就在 control 轮里（`094933` 那一轮从头到尾没有任何进程被动过），10 段在 restart 轮里；
每段两台同时出现、起点相差 <200ms。**没有一段落在换进程前后 ±3s 内**，
离得最近的一段在 SIGTERM 之后 6.5s，最远的一段在 128s。
归因不是猜的：在**既有外层口计数、又真的出现停摆**的两个轮里（`094933` control + `100838` restart，
共 6 段停摆，按"rx/tx × 两台"拆开是 23 个 ≥250ms 的空窗窗口），隧道口和外层口的空窗**同长度、同起点**——
隧道口 `wg.rx` 停 515ms 的那 515ms，`eth1.rx` 也一动不动，**23 个窗口全部如此，无一例外**。
包没被外层口记到，就是 VPP 主循环那段时间没拿到 CPU；两个容器的 VPP 是各自独立的进程，
能在同一时刻一起停的只有它们脚底下这台 VM。这和本仓库早就记过的那条周期性停摆
（环路台基线，宿主冻结）是同一件事，不在本判据里翻案。

**这一格里还没测到的三条，以及什么时候补**（按硬规则第 1 条第 4 款点名）：

1. **时延**：产品探针当前发不出去（写死主表 0），要改插件、要换 `.so`、换 `.so` 必须重启 VPP，
   重启 VPP 就会掉这条隧道 —— 所以时延数字要等**下轮 lab 重建**（同一套口径重测）。
2. **终端到终端（客户口进、客户口出）的丢包与时长**：这台台子只有两台 CPE 的 overlay /32，
   没有客户路由器、没有客户前缀，打不了这条流 —— 同样归到 lab 重建轮。
3. **客户侧 BGP 邻居 uptime**：这台台子里没有 FRR（只有 VPP + agent），`vtysh` 连不上任何 daemon，
   所以邻居 uptime 这一格**这台台子给不出**；agent 进程重启这条路径本身不写 FRR 配置，
   已实测过的那条邻居数字是配置生效路径的（2026-11-01 `frr-reload.py`：0 丢包、bgpd pid 不变、
   邻居 uptime 不归零），两件事不能互相顶替。

原始读数（时间线 jsonl、四列计数、VM 负载探针、进程与 md5 快照、agent 日志）留在宿主
`~/.qoder-tmp/m2a/test/lab/run/m2a/measure/<轮次目录>/`，汇总可由
`m2a-upgrade-analyze.py <轮次目录>` 重跑复现；台子和容器按一次性规则销毁，不留在机器上。

### 10.7 三点定案（2026-11-03 用户拍，本节不再是待拍）

1. **8 把密钥的生成时机与落盘形态** —— 「开局就把 8 把钥匙生成」：`LoadOrCreate` 一次补齐
   8 个独立文件（槽 0 沿用 `wg_x25519.key` 字节不动，槽 1..7 是 `wg_x25519.slotN.key`），
   加载后立刻自检"八把两两不同"，撞了就报错拒绝启动而不是静默重生。
   不做"一个文件存 8×32 字节"：磁盘上已有七个存量文件形状的目录，多一把密钥只是多一个文件，
   存量那把的字节完全不用搬家。
2. **`key_gen` 的粒度** —— 「按你推荐」：**每槽一个计数器**。理由与判据见 10.4 末。
3. **协议 v2 的兼容读路径** —— 「不留」：控制器 + 全部 agent 同一个维护窗口一次升完。
   窗口代价与"已在跑的客户流量不掉"的论证见 10.4；那句话里"agent 重启认领现成接口"
   **2026-11-04 已在一次性私有台实测**（10.6 那节「升 agent 不中断」实测：换进程前后 ±3s 内
   BFD 非 up 采样点 0 个、四列包计数 ≥150ms 空窗 0 个，隧道身份与表 250 条数全程不变，
   VPP pid 不变；时延与终端到终端两格这台台子给不出，缺口点名在同一小节末尾）。

### 10.8 已知未决

- **（2026-11-04 用户拍完）slice 3 = M-3 的三条语义，照此实现**：
  1. 跳过条件有两条，任一命中就跳过这一条：① **批量**要建颜色 0、而这对设备已经有颜色 0 的
     隧道（批量默认全建颜色 0 ⇒ 同色重复就跳过，2026-11-03 已确认这是期望结果）；
     ② 这对设备**已经有 8 条隧道**（对内槽位用满）。
     这里有一条我自己要确认的界线：①是**批量的去重**，不是 pair 门禁的唯一性约束——
     手工单条建"同色的第二条"仍然必须能建出来，否则和他早就拍过的"8 条可以全颜色 0"直接冲突。
  2. 批量接口的返回形态是**部分成功 + 跳过清单**，不整批拒绝；跳过清单逐条带
     `TunnelID`、对端与原因，前端如实显示，不许静默。
  3. 槽号**按创建顺序递增、删一条不回收**（不做"复用最小空槽"）。
- **（第 3 条的代价，2026-11-04 我先量出来再请他确认，不擅自改）**：按字面实现是
  "对内累计发出过 8 个槽号就发不出了"，而不是"对内同时存在 8 条才满"。也就是说客户在同一对
  设备上反复建-删建-删，第八次之后这对设备**永久建不出隧道**，除非人工去改控制器里的状态。
  这条不是理论推演：`pairSlots(a,b)` 分配最低空闲槽时，"空出来的槽不许再用"和"上限 8"
  这两条规则直接冲突。要么给一个"这对设备的槽号计数复位"的显式入口（删除全部隧道后重新从 0 起），
  要么把"不回收"理解成"活着的隧道槽号不因为邻居被删而重排"（=删除后空槽可以被新隧道拿走，
  只是不许原地改号）。**M-3 的实现等这一条拍完再开工。**
- （待拍，我不动）控制台隧道页要不要显示"这对设备已用 n/8 槽"。上面第 3 条只定了分配规则，
  页面口径还没定。
- **VPP 的规模上限已量到（2026-11-04，同一台一次性私有台 `m2v-node1`，数字与逐条归因见 §9.6）**：
  4096 在配置面过得了（4200 条一次性建完、0 错误、约 **10 kB/条**），而真正的墙**不是**
  `wireguard_peer.c` 里那个 `MAX_PEERS` 检查（7867 条 peer 全回 0，从没触发），是
  **32 MB 统计段在 7868 口把 VPP 自己 abort 掉**。这条按 §1 第 4 条点名写在这里：
  **撞墙时这台盒子所有客户流量一起断**，所以闸门必须放在我们这边、且值不能贴着 7868。
  另一个更早的产品上限是**我们自己的** `detailQueueLen = 256`（§9.6 第三节）：
  257 条起心跳清单、`adopt`、peer/隧道对账、遥测、ip/mpls 路由 dump **全部**报"已被截断"。
- **8 把公钥的心跳重验开销已量到（2026-11-04，同一天同两台，见 §9.6 第四节）**：单次
  `Binding.Verify` **68–74 µs**，一台设备 8 把 **559–607 µs**（线性），1000 台一轮 **0.56–0.61 s**；
  开销全在 Ed25519 验签，base64 解码占 0.1%。**结构性问题不是倍率，是这段计算跑在 `s.mu` 里、
  而且每次心跳 8 把全验（`main.go:2046` 的验签在 :2063 的"没变就跳过"之前）** ⇒ 这就是
  300ms 遥测链路上新加的抖动源。两条修法（比对字节后再验签 / 验签挪出锁）都**待拍，我没动代码**。
- **协议 v2 的换代窗口**：用户 2026-11-04 拍"lab 可以重建" ⇒ lab 侧不存在混版本窗口，
  控制器与 agent 在同一次重建里一起升；真机 192.168.1.100 的窗口时间他还没给（升 agent 是
  热更、不动 VPP，SSH 走 VPP 的 vpp0 这条风险因此不触发），等他安排。
