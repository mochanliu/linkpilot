# overlay 设计草案（VPN 间导通 + prefix 过滤）— 待讨论，未开工

2026-10-28 夜。前置调研见 `cisco-vpn-leaking-research.md`（思科做法与取证，含一节 UNVERIFIED）。
本文是**方案草案**，不是已定稿的决策：第 2 节和第 8 节需要用户拍板，拍板前不写代码。
`R/` = 仓库根目录。

## 0. 目标与对标

用户诉求真机语义是两句：**"配好哪些 VPN 之间能通"** 和 **"通的时候哪些前缀允许跨"**。
对到思科：前者 = centralized control policy 的 `match route vpn → action accept export-to vpn-list`（在 vSmart 上），后者 = 同一条 sequence 里的 `prefix-list` 匹配项。RT import/export 在思科是**设备侧准入**，不是导通开关。

我们的对应物：**RR = vSmart**，**控制器/控制台 = vManage**，**CPE = vEdge**。所以导通决策按思科的样子应该落在 RR，控制器负责撰写与下发策略。

## 1. 现状（已核对，带出处）

| 事实 | 出处 |
|---|---|
| 路由协议 v3 的一条前缀只有 4 个字段：`device_id / vrf_id(uint8) / prefix / rts[]`，没有 next-hop、label、metric、site、community | `R/controller/internal/route/proto.go:124-129` |
| RR **不认识 RT**，只按"互相宣告的隧道表"决定反射对象，另有一个按来源设备的 `reflect_to` 减法白名单 | `R/controller/internal/route/reflector.go:44-45,278-305` |
| CPE 侧三道闸：**没有这个 VRF 就丢**（`noVRF`）、**RT 不交就丢**（`rtMismatch`）、**没有到来源的隧道就丢**（`noPath`） | `R/controller/internal/route/client.go:726-751` |
| sr-fib 把前缀装进 VPP 时，**表号和标签都由这条路由自己的 VRF 字段算**：`table = uint32(r.VRF)`、`Label = LabelFor(r.VRF)`；`LabelFor(v)=1000+v` | `R/controller/internal/fib/fib.go:186-187,41-53` |
| 喂给 sr-fib 的路由形状只有一个 `VRF` | `R/controller/internal/fib/fib.go:56-60` |
| 控制台的 VPN 对象只有 `id(uint32, 上限 65535) / name / bgp / ospf`，**没有 RT、没有表号、没有标签**；250 与 1250 被保留 | `R/controller/cmd/sr-ctrl/config.go:23-25,45-50` |
| VPN→RT / VPN→label 目前**只在 lab 脚本里手写**：`"vrfs":[{"id":$VPN,"export_rts":["65000:$VPN"],"import_rts":["65000:$VPN"]}]`；sr-fib/sr-frr 的表映射同样手写 | `R/test/lab/bringup.sh:743,754,835-840` |
| 本端前缀的两个来源：配置文件里的 `prefixConfig{vrf_id,prefix}`，和 sr-frr 经 `PUT /local-prefixes` 反向上报的客户 BGP 前缀（tag 1001） | `R/controller/cmd/sr-route/main.go:60-67,283-310`、`R/controller/cmd/sr-frr/main.go:283-294` |
| 数据面插件对 VPN 无感：SR 头只有 magic/path_id/segments/hmac，收端按 UDP 端口位图分派；标签的压/弹/查表全是 VPP 原生 FIB | `R/vpp-plugin/sr_header.h:9-26`、`R/vpp-plugin/sr_wg.h:100-117` |
| sr-fib 可以往**任意**表号装路由、把任意本地标签绑到任意 lookup 表（`ip_route_add_del` 带 table_id、`mpls_route_add_del` 带表） | `R/controller/internal/vppapi/fib.go:171-202,244-262` |
| 控制台"路由"页是占位（"尚未实现：路由表由 RR 反射，控制器不发布路由"），配置页第三个 tab「overlay」是一段散文，列的正是 RT import/export、双向导入、选择与 prefix 策略 | `R/controller/cmd/sr-ctrl/console.html:642,1330-1341` |
| lab 只跑过**单 VPN**（VPN 0）端到端；多 RT 只在 `test-route.sh` 里用三个节点共享 VRF 5 验过 import 不匹配与 `reflect_to`。**没有任何一次测试让一台盒子同时服务两个 VPN** | `R/test/lab/verify.sh:24-29`、`R/test/scripts/test-route.sh:160-195` |

一句话：**导通需要的三样东西——RT/前缀准入、反射、装表——各自都存在，但没有任何一处能表达"把 VPN 10 的前缀装进 VPN 20"**。

## 2. 必须先定的三个结构问题

### 2.1 VPN 号宽度：控制台的 uint32 对协议的 uint8

控制台允许 VPN ID 到 65535，协议里 `vrf_id` 是 `uint8`（`MaxVRFID=255`，`R/controller/internal/fib/fib.go:44-45`）。
两条路：
- **收口**：控制台把 VPN ID 限制在 0..249、251..255（避开 250）。改动小，但将来要放开时协议还得再动。
- **加宽**：`vrf_id`/`VRFConfig.ID`/`prefixConfig.VRFID`/`RIBRoute.VRF` 全改 `uint16`，表号跟着到 65535。标签仍安全：`1000+65535 < 2^20`。JSON 线上是纯数字，属于兼容加宽，但 canonical 校验与全部单测要一起改。

草案倾向：**加宽到 uint16**，并把思科的那套保留段（0/512/513-65525）落成控制台校验。因为"客户 VPN 数量超过 200"在真实租户里不罕见，而收口是一次以后必然要重做的约束。

### 2.2 标签耦合：导通后"装哪张表"和"压哪个标签"必须分开

这是整个 overlay 最关键的一处，也是最容易做错的一处。推演一遍（node1 的 VPN 10 里有 `10.10.1.0/24`，node2 的 VPN 20 要能到它）：

1. RR 把这条前缀以"VPN 10 来源"反射给 node2，node2 把它装进**自己的表 20**。
2. node2 发包 → 查表 20 → 下一跳是到 node1 的 wg 接口 → 压标签。
3. **标签必须是 `LabelFor(10)=1010`**，因为 node1 弹栈后按标签登记的那张表是"这个前缀真实所在的表 10"；`mpls_route` 是全局查表，不按本端 VPN 解释。
4. 若照现在的代码（表号和标签都取路由的 VRF）：把路由的 vrf 改写成 20 → 表对了、标签变 1020 → node1 弹栈查表 20 → 前缀不在那儿 → **黑洞**；不改写 → 前缀根本进不了表 20。

所以"把 vrf_id 改写成交付目标"这条路是错的。草案：**协议加一个可选字段 `install_vrf`（0/缺省 = 与 `vrf_id` 相同）**，RR 在反射导通副本时填它，CPE 装 `install_vrf` 的表、按 `vrf_id` 算标签；`sr-fib` 的 `RIBRoute` 同步加一个字段，`fib.go:186-187` 一行改成两个来源。改动小、语义直白、不动现有单 VPN 路径（`install_vrf` 缺省时行为逐字不变）。

反向：node1 要回包，得在**它的表 10** 里有 node2 的 VPN 20 前缀——就是思科要两条 sequence 的同一个道理。所以导通规则**成对**才等于"能通"，单向只是单向可达。UI 上要么给"双向"开关（落两条规则），要么明确告诉用户单向的含义。

### 2.3 策略落在谁身上：RR 侧（照思科）还是设备侧

RR 现在完全由手写 `rr.json` 起（`R/test/lab/bringup.sh:582-592`），控制器与 RR 之间**没有任何链路**。三个选项：

| 选项 | 做法 | 得什么 | 付什么 |
|---|---|---|---|
| **A 照思科：策略在 RR** | 控制器撰写导通规则+prefix-list → 新控制链路下发给 RR（带 generation，RR 只在收到完整策略后才改写/复制路由）→ 设备照旧只装收到的 | 跨站点导通；隔离最好（设备**收不到**不该收的前缀）；控制台是唯一撰写点 | 新增控制器↔RR 通道 + 策略版本/失败语义；RR 要能鉴权（复用设备证书那套？）；改动最大 |
| **B 设备侧 leak** | RR 继续只反射本 VPN 路由；每台盒子把**自己**其他 VPN 学到的前缀复制进本地另一张表（思科的 `route-replicate` / "same edge site 本地互导"） | 只做"同机跨 VPN"（共享服务、防火墙串接、一条 LAN 挂多 VPN）；策略走**已有的**配置下发通道 | **跨站点导通做不到**：不在 VPN 10 里的盒子根本收不到它的前缀（`noVRF` 闸） |
| **C 分两步：先 B 后 A** | B 作为第一阶段落地（顺带把 2.1/2.2 的字段改完并跑一次双 VPN lab），A 作为第二阶段 | 每一步都能端到端验证；B 的 lab 正好补上"一台盒子两个 VPN"从未测过的空白 | 中间态功能不完整；B 的 leak 语义以后要和 A 的规则语义在控制台上并排解释 |

草案倾向 **C**：B 阶段的所有改动（宽度、`install_vrf`、双 VPN lab、控制台字段）都是 A 阶段的前置，不会白做；而 A 需要的那条控制器↔RR 链路是新架构，值得单独一轮设计。
**但"客户实际要的是跨站点导通还是同机跨 VPN"是业务判断，必须由用户定。** 如果只要跨站点，B 是绕路，直接做 A。

## 3. 配置项清单（控制台要暴露的东西，字段级）

命名按思科的对象名靠拢，但只保留我们真要实现的。

**3.1 VPN（扩当前对象，不新建页面）**
| 字段 | 必填 | 说明 | 对标 |
|---|---|---|---|
| `id` | 是 | 加宽后的 1..65525，排除 0/250/512/1250；0 固定为 transport | Service VPN 范围 [见调研 §4.4] |
| `name` | 否 | 已有 | — |
| `bgp` / `ospf` | 否 | 已有 | — |
| `export_rts[]` | 是（默认自动生成 `租户ASN:id`） | 本 VPN 前缀带出去的 RT | vManage Export Route Target List |
| `import_rts[]` | 是（默认 = 自己的 export） | 本 VPN 准入的 RT | vManage Import Route Target List |
| `advertise` | 是 | 逐源开关：`connected` / `static` / `bgp`（默认前两个 on、bgp on） | `omp advertise` / `no advertise …` |
| `advertise_prefix_list` | 否 | 引用一个 prefix-list；配了就只宣告落在列表里的本地前缀 | `advertise … route-map` |

`export_rts/import_rts/advertise*` 必须由控制器生成到 `sr-route` 的配置里（今天还在 lab 手写）。

**3.2 prefix-list（新对象，租户内命名）**
```
PrefixList { name, entries[]: { cidr, le?, ge? } }
```
第一版**只 permit、只匹配精确 CIDR**（`le/ge` 是路由器语义，我们前缀量小，先不做范围匹配，等真需要再加）。被两处引用：导通规则的过滤、VPN 的 `advertise_prefix_list`。删除前检查引用数（照路径模板那条规则做）。

**3.3 导通规则（新对象，对应一条 control-policy sequence）**
```
LeakRule {
  id            控制器生成
  from_vpn      源 VPN
  to_vpn        目标 VPN
  prefix_list   选填；空 = 源 VPN 的全部前缀
  devices[]     选填；空 = 租户内所有同时具备这两个 VPN 的设备（思科用 site-list 表达这件事）
  enabled
}
```
成对语义用 UI 的"同时建反向"表达，落库仍是两条规则——这样删除、禁用、审计都是单条的。
规则本身**不参与判重到"同一对 VPN 只能一条"**（源/目标/过滤集不同就是不同规则），但 `(from,to,prefix_list)` 完全相同的重复规则要拒。

**3.4 明确在控制台可见的"准入闸门"状态**（不新增配置，只新增呈现）
`noVRF / rtMismatch / noPath` 三个计数器已经是设备的自述（`client.go:726-751`），把它们接进设备表的「路由下发」列。**被规则放行但被设备 RT 闸丢掉**这种情况必须看得见，否则用户只会看到"配了不通"。

## 4. 归属与执行点（谁算、谁装）

| 决定 | 归属 | 理由 |
|---|---|---|
| 规则撰写、prefix-list、VPN 的 RT 与 advertise | 控制器（草稿→显式发布，沿用基础配置那套版本语义） | 控制台是唯一撰写点，不能造数据 |
| 一条前缀允许被谁收到、导通副本的 `install_vrf` 填什么 | **RR**（A 阶段） | 只有控制器/RR 看得到全局；设备无法知道该不该收别人的 VPN 路由（这是思科把 export-to 放 vSmart 的原因） |
| 装进哪张 VPP 表、压哪个标签 | **CPE**（`sr-route` 收 → `sr-fib` 装） | 表号/标签是本机数据面事实 |
| 本地另一 VPN 的前缀复制（B 阶段） | **CPE**，策略来自已下发的配置包 | 不需要 RR 参与，天然只涉及本机两张表 |
| FRR | 只负责算客户侧路由，不碰导通 | 已定架构：FRR 只是用户态控制面，VPP 转发全部数据 |

## 5. 校验与如实呈现（这轮的硬规矩）

1. **未发布的规则不生效**，RR 只认完整策略快照 + generation；策略拉不到就沿用上一份并在控制台标"RR 仍在用 v<n>"，不假装已下发。
2. 创建/发布时静态校验：两端 VPN 都存在、`from≠to`、prefix-list 存在、VPN 0/512/250/1250 不可作为 `to_vpn`、来源设备集合里没有任何一台同时具备两个 VPN（这条规则永远不会命中，要拒绝而不是静默空转）。
3. 运行期呈现三态：**已放行 / 被设备 RT 闸丢 / 被 noVRF 闸丢**，数据来自设备回报，控制器不推断。
4. 重叠前缀：`to_vpn` 里已有同前缀（本地来源或其它规则导通）时，**不做静默覆盖**——按思科的教训（leaked 路由属性丢失、显示成 static）我们更该显式拒绝并说明冲突，让运维决定。SS-NAT 那类地址重叠不在本轮。

## 6. 分阶段（每阶段有可验证判据）

| 阶段 | 内容 | 判据 |
|---|---|---|
| O1 | VPN 加宽到 uint16 + `install_vrf` 字段 + RT/advertise 进入控制器配置与生成（替掉 lab 手写） | `go vet/test` 全绿；lab 里**一台盒子两个 VPN**（VPN 10/20）跑通：各自的表、各自的标签、互不串；`verify.sh` 扩到双 VPN |
| O2 | B 阶段：同机 leak（`sr-route` 按本机策略把 VPN A 学到的前缀复制进 VPN B 的表） | 一台盒子内 `ping` 从 VPN20 源打到 VPN10 的 LAN；VPP 表 B 里能看到该前缀且标签是 `1000+A`；关掉 leak 开关后路由消失 |
| O3 | prefix-list + per-VPN advertise 过滤 | 只宣告列表内前缀；RR 侧 `show` 口径能对上；控制台显示被过滤条数 |
| O4 | A 阶段：控制器↔RR 策略通道 + 跨站点导通规则 | 两个站点、两个不同 VPN、单向/双向各一次 ping；反向必须靠第二条规则才有回包（这条测试就是规则语义的证明） |
| O5 | 控制台「配置 > overlay」三块（VPN / 导通规则 / 前缀列表）+ 设备路由下发展现 | 真浏览器点击 + 截图，未上报一律写「未上报」 |

## 7. 明确不做（本轮）

- 数据面 ACL / `data-prefix-list` 那套 LAN 侧兜底过滤（思科 localized policy）——需要 VPP 侧分类表，属于另一件事。
- community、`region`/Multi-Region Fabric、mesh group、service chaining（`set service`）。
- 前缀范围匹配（`le/ge`）、聚合、SS-NAT 重叠地址。
- VPN 512 管理网、多租户共享 RT 空间。
- 策略回滚（沿用"没有自动回滚"的既有规矩）。

## 8. 需要用户拍板的（按重要性排序，附我的推荐）

1. **业务场景**：要的是**跨站点**的两个 VPN 互通，还是**同一台盒子**上多个 VPN 互通，还是两者？→ 决定 §2.3 走 A / B / C。我推荐 C（先 B 后 A），但若客户真实需求只有跨站点，就直接做 A，别把 B 当过渡。
2. **VPN 号宽度**：加宽到 uint16（推荐）还是把控制台限制在 8 位。
3. **导通规则粒度**：`from_vpn → to_vpn (+prefix-list) (+devices)` 这个形状够不够？要不要一开始就支持 `vpn-list → vpn-list`（思科用 list 表达，一条规则多个目标）？我推荐 v1 先一对一，UI 里"多选目标"落成多条规则，避免 RR 侧出现隐式笛卡尔积。
4. **单向/双向**：UI 给"双向"开关（落两条规则，推荐）还是只显示单向、让用户自己再建一条。
5. **RR 从哪来**：A 阶段 RR 是控制器的一个北向客户端、还是一个独立进程接新命令（`sr-rr -ctrl https://… -tenant X`）？前者要控制器能反连 RR，后者与 lab 里"盒子在 NAT 后、控制器从不主动连设备"的既有约束一致 —— 我推荐后者。
6. **RT 是否继续对用户可见**：思科可见（Import/Export Route Target List）。保留它 = 多一道设备侧闸门和一个"配了但被丢"的解释成本；藏起来 = 我们把 RT 变成内部编码、只暴露规则。我推荐**保留可见但默认自动生成**，并把被 RT 丢弃做成一条明确的告警文案。
7. **`route-import-service` 这类"vEdge 本地互导命令"**：我在 cisco.com 上搜不到（调研 §3），如果用户手上有真机文档，这是唯一需要复核的点——它关系到"同机 leak 在思科到底叫什么、字段长什么样"。
