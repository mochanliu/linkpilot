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

背景是产品要求（每台盒子目标 3000 条隧道、软件层 4096）。**同两台设备之间的隧道上限是 8 条，
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

**待拍（10.7）**：槽号的 `key_gen` 是每槽一个计数器还是设备级共用一个。我的倾向是**每槽一个**：
设备级一个计数器意味着"换第 3 槽的密钥"会把另外 7 槽的代次一起推进，
对端就没法区分"哪一把变了"（现在 `applyBinding` 的 :2017-2018 防重放判据是按单一计数器写的）。

### 10.5 改动清单（按落点，一处一行）

| 落点 | 现在是什么 | 要改成 |
|---|---|---|
| [identity.go:65-76](../controller/internal/agent/identity.go) `Identity.wgPriv/wgPub` | 一个槽位 | 8 把（下标 = 槽），加载时缺哪槽补哪槽，槽 0 沿用 `wg_x25519.key` |
| [identity.go:155-192](../controller/internal/agent/identity.go) `loadOrCreateX25519`/`setWGKey`/`WGPrivateKey`/`WGPublicKey` | 单把读写 | 加 slot 参数；新增"8 把公钥两两不同"自检 |
| [identity.go:310-336](../controller/internal/agent/identity.go) `Binding()`/`RotateWGKey()` | 签那把、换那把 | `Binding(slot)`/`RotateWGKey(slot)`，`key_gen` 按槽 |
| [binding.go:63-67,132-147,195-214](../controller/internal/keybinding/binding.go) | 主语无槽，域串 v1 | `Binding.Slot`、`Message` 带槽字节、域串 v2 |
| [sr-agent/main.go:271,328,441-450](../controller/cmd/sr-agent/main.go)、[report.go:47-53](../controller/cmd/sr-agent/report.go)、[client.go](../controller/internal/agent/client.go) | 心跳/mapping 带**一把** binding | 带 8 把（一次报完，控制器按槽落位；旧格式在迁移期允许只带槽 0） |
| [main.go:127-133,2003-2032](../controller/cmd/sr-ctrl/main.go) `NodeRecord.Binding`/`applyBinding` | 每设备一把 + 单一计数器 | 每设备 8 槽数组、按槽 diff 并返回"变了哪几槽"、state.json 旧格式迁移（单把 → 槽 0） |
| [underlay_delivery.go:113-136](../controller/cmd/sr-ctrl/underlay_delivery.go) `tunnelSpecLocked` | `PeerBinding: prec.Binding` | `prec.Bindings[t.Slot]`；该槽没上报 ⇒ 沿用现有"没上报公钥担保"跳过文案 |
| [wire.go:414,611,726,536](../controller/internal/wire/wire.go) | 四处单把字段 | `MappingReport`/`HeartbeatRequest`/`PeerInfo` 变数组；`TunnelSpec.PeerBinding` **保持单把**（它本来就是一条隧道一份，天然带槽） |
| [tunnel.go:422,612](../controller/internal/agent/tunnel.go) `priv := t.id.WGPrivateKey()` | 一次取出传给**每一条** Link | 按这条隧道的槽取；`LinkConfig.PrivateKey` 已是 per-link（[dataplane.go:172-174](../controller/internal/agent/dataplane.go)），不动语义 |
| [tunnel.go:100-103,305-326](../controller/internal/underlay/tunnel.go) `create`/`pairTunnel` | 撞设备对就拒 | `pairSlots(a,b)` 分配最低空闲槽；满了 ⇒ `ErrPairFull`；**错误文案里"VPP 按公钥跨接口去重"改成"VPP 插件的 peer 池全局按对端公钥去重"**（见 §9.1 第 14 条） |
| [plan.go:105-108,311-321](../controller/internal/underlay/plan.go) `HasPair`、[fan.go:41-96](../controller/internal/underlay/fan.go) `Fan` | 每对只发一条 | 批量/扇出带条数，>8 才拒 |
| [state.go:20-24,119-121](../controller/internal/underlay/state.go)、[console.html:5146-5148](../controller/cmd/sr-ctrl/console.html) | "一对一条"文案 | 同上新口径 |

**顺序硬约束**：`Slot` 落地前必须先做 M-1（path_id 每隧道唯一，§9.2 第 4 条）。
否则同一个设备对的第 2 条隧道会和对端抢同一个 `PathID = base + PeerNodeID`，
后写的段列表静默覆盖前一条、retval 全 0——那是真会改在线转发的那一条。

### 10.6 判据（做完怎么算过，前后两组数字都要有）

- 单测：8 把公钥两两不同；对内槽号不重复；槽满报 `ErrPairFull`；v1 形状的签名在 v2 域下**必须**验不过；
  `state.json` 旧格式迁移后槽 0 公钥与迁移前逐字节相同。
- **只用新建的一次性私有环境**（`srwg-*` / `osc-*` / 环路台都是别的会话的活现场，不许动）。
- 环境里同一对设备建 2 条隧道（**故意同色 0**），逐项看：
  ① 两口各自的 peer 公钥确实是不同槽（用我们的 vppapi dump 读公钥，
  **不要用 `show wireguard interface`——它打印私钥，输出不许进仓库、报告或日志**）；
  ② 两条各自握手 UP；③ BFD / 探针各自成会话、各自有遥测行；
  ④ `tools/fib-dump` 读回两个不同 path_id 且段列表各是各的；
  ⑤ 打流互不串台（隧道 1 的内层包不能被隧道 2 解出，反向亦然）——这就是 10.2 末尾欠的那条实测。
- 升级不中断：升 agent 前后同一口径打流（丢包数、时延）、邻居 uptime、VPP FIB/RIB 条数、进程 pid，
  两组数字写进提交说明。

### 10.7 三点要你拍

1. **8 把密钥的生成时机与落盘形态**：我的默认是 agent 启动时一次性补齐 8 个独立文件、
   槽 0 复用现有 `wg_x25519.key` 字节不动（理由见 10.3）。要不要改成"用一个文件存 8×32 字节"随你。
2. **`key_gen` 的粒度**：每槽一个计数器（我推荐，理由见 10.4 末），还是设备级共用一个。
3. **协议 v2 的兼容读路径留不留**：不留（一次换掉，控制器 + 全部 agent 在同一个维护窗口里升完，
   窗口内"建隧道"这个动作会失败、已在跑的流量不掉，理由和实测要求见 10.4），
   还是留一条"旧设备只带槽 0 的 v1 binding 仍然接受"。留的代价是校验分支和一份永远删不掉的兼容层。

### 10.8 已知未决

- VPP 的 `MAX_PEERS` 上限值还没量到（`wireguard_peer.c` 里那个检查，§9.1 第 2 条），
  它关系到"每盒 3000 条隧道"的产品目标能不能站住，与本次改动正交，但要在 M-2 实施期间一起量。
- 8 把 × 每台设备 = 每台盒子 8 把公钥要上报、存储、验签；`keybinding` 现在是每心跳重验一把，
  改成 8 把后心跳开销要复测（300ms 遥测周期那条链路上别加出新的抖动源）。
