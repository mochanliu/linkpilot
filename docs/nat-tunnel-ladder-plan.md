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

## 7. 待拍（每条都是"选了会怎样"，不是术语）

- **P-1 判据**：阶梯每步"通了没"用什么判。
  甲：只看 BFD up（300ms 级检测，`Up` 字段现成）。
  乙：BFD up 且探针窗口有回包（多一个"能双向"的证据，但要多等一个窗口）。
  硬前置：`TunnelMetrics` 现在只有 `PeerNodeID` 没有 `TunnelID`
  （[wire.go:919-924](../controller/internal/wire/wire.go)），同一对设备开第二条隧道时
  归属会撞车 ⇒ 必须给 metrics 行加 `TunnelID`（agent 侧 `spec.TunnelID` 现成）。我建议 P-1 选甲 + 加 TunnelID。
- **P-2 每步等多久算失败**（绝对时间，因为这是"下发往返 + WG 握手 + BFD 起来"三件事之和，
  不是数据面一个数决定的）：甲 10s（整条阶梯最多 40s，失败得快但可能误判慢链路）／
  乙 30s（最坏 2 分钟一条，我建议）／丙 60s（最坏 4 分钟，跨大洲链路稳）。
  期间不阻塞其他隧道，也不许阻塞心跳与遥测。
- **P-3 手工隧道跑不跑阶梯**：甲 只跑第 1 步，失败就如实报错，不自动把人手填的地址降级成
  "只监听"（我建议：人显式配的地址是意图，控制器不擅自改掉它）；乙 手工也跑完整阶梯。
- **P-4 字段命名**：`PeerEndpointOmitted`（Go）/ 页面上的勾选文案
  "本端不拨对端，只监听（等对方打进来）"。要不要换名字/文案，听你的。
- **P-5 SR**：~~甲／乙~~ **已拍乙并已落地**（见 §4 末尾）。剩下的待拍是 P-1…P-4、P-6。
- **P-6 放弃之后**：甲 库里保留隧道、状态 `given_up` 并列出每步失败原因（我建议，
  删掉就是看不见的地洞）；乙 直接删。

## 8. 这一版明确不做

- 不改 WG 的 NAT 保活/`nat-refresh` 语义，不加打洞专用包。
- 不做中转兜底、不做颜色、不做非公网线路（mpls/专线）。
- 不动 `plan.go:119-137` 里"观测过期不建"的闸门（阶梯的前提是有公网地址可拨）。
- 不动 lab：本方案落地前不跑任何 lab/harness 矩阵。
