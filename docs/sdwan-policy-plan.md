# SD-WAN 策略层方案 + 前台调整方案（只出方案，本轮不改任何代码）

日期：2026-11-03。执行人：Qoder。

你睡觉前给的六条，按第 7 条的要求，这里**只有方案和取证**，`controller/` 与 `console.html` 一行没动。
前台的两页我另外做了可交互原型（纯 mock、自包含单文件），路径在 §10 末尾。

取证分两类，逐条标了出处：
- **【码】** = 读代码得到，给 `文件:行号`；
- **【量】** = 今晚在那套环路实验台（`lp-*` 容器，VPP v26.06-release）上敲命令得到的原文；
- **【上游】** = FD.io VPP 源码/文档；
- **【推断】** = 我说不出它在哪、只是我觉得会这样 —— 这种一律不许当结论，全部落到 §8 的先量清单里。

---

## 0. 一页结论

**0.1 你要的"静态策略 = VPP 的策略路由"，VPP 有原生机制，而且和我们的表结构严丝合缝。**
VPP 的 PBR 就是 **ABF 插件**（`abf policy add id <n> acl <idx> via ...` + `abf attach ip4 policy <n> <接口>`），
匹配器是 **acl-plugin**，它的规则字段是**完整五元组**（src/dst 前缀、proto、sport/dport 区间、tcp flags）。
而 `via` 后面能接 `ip4-lookup-in-table <表号>` —— 也就是"换一张 FIB 表重新按目的地址查"。
我们恰好已经把每个颜色准备成一张独立 FIB 表：
`表号 = 颜色*1000 + 10000 + 主表号`，落在 10000..17255【码 `internal/fib/fib.go:73-90`】，
而且 `ColorTable()` 的注释原文写着：**"policy 阶段用策略路由把流引到指定颜色的表"**【码 `fib.go:79-80`】。
所以静态策略不是我要新造一套机制，是把 O3 预留的那个位子填上。

**0.2 今晚实测到：**这台 VPP 里 abf/acl/policer/l3xc/lb/sasc 插件都装了，命令都在，
且 `abf-input-ip4` 在 `ip4-unicast` 弧上排在 `ip4-lookup` **之前**（弧内位次 62 vs 67）【量 §1.2】。
`ip4-lookup-in-table`、`next-hop-table`、`out-labels` 这几个 `via` 令牌在这个 build 的二进制里确实存在；
**`src-lookup` 不存在**（`/usr/bin/vpp` 里搜零命中）【量 §1.3】—— 上游 26.10 有、我们这版 26.06 没有，
所以任何依赖"按源地址查表"的写法都不能用。

**0.3 你要的"Fallback 到颜色 0"，最自然的表达是一条更差 preference 的次路径。**
VPP 的 preference 原生就是主备、会自动回落（0 最优），我们已经在用它排颜色【码 `fib.go:55-58`、`Preference: PrefBase+t.Color`】。
于是"这台设备的颜色 c 允许退到颜色 0"（开关粒度是 **(设备, 颜色)**，不是每行策略，见 §4.1）
= 指向颜色 c 的 ABF policy 的 path 集里多挂一条 `ip4-lookup-in-table <颜色0表>` 且 preference 更差。
**但这条能不能真的退，取决于一个我现在答不上来的问题**：目标颜色表里没有这个前缀时（该颜色的隧道全 down 时表就是空的），
这条 DEAG 路径是算"down 所以自动走次路径"，还是算"有路径但查不到前缀所以黑洞"。→ **M-3，必须先量**。

**0.4 动态策略最大的风险不是选路，是"数据从哪来"和"会不会自己制造抖动"。**
今晚刚交付的环路报告量到：150ms BFD + **零 hold-down** 把宿主 3.4s 的冻结放大成客户可见 1.0–12.0s 黑洞【报告 §4.4】。
一个按 SLA 改下一跳的守护进程如果同样没有抑制，就是**第二个抖动放大器**。所以 §5 里抑制/最小驻留是硬要求不是调优项。
另一半更硬：报告 §4.3 量到"agent 每 10s 的两条 VPP dump 与计数停摆同源"。
**新守护进程绝不允许自己再去 dump VPP** —— 数据必须走 sr-agent 已有的那一条采样，盒内转喂（§5.2、P-20）。

**0.5 三条前置缺口：不补，第 4/5 节配了也不生效。**
- **G-2b（今晚新证）**：控制器上的 `escape_all_colors` **没有任何下发通路**。bundle 第 4 段只有
  `tunnels / vrfs / rrs` 三个键【码 `cmd/sr-ctrl/overlay.go:705-724`】，`escape` 全仓只出现在控制器模型
  （`overlay.go:88-89`、`overlay_api.go:74-97`），`apply/wire/agent` 三处零命中。而 `overlay.go:88` 的注释还写着
  "O6-b renders it" —— 注释和现实不一致。**逃生开关目前是个纯摆设**，这也意味着环路报告的**场景 E（逃生开关）走控制台根本测不出效果**（报告 §8 那条判据要改）。
- **G-2c（今晚新证）**：**颜色本身同样没有下发通路**。`underlay.Tunnel.Color`【码 `internal/underlay/state.go:136`】
  是运营方在隧道配置上填的，但 `wire`、`agent`、`sr-agent` 三处 grep `Color` 零命中【码，Agent 复核过】。
  盒子侧 `fib.json` 的隧道表确实有 `Color` 字段【码 `cmd/sr-fib/main.go:59-63`】，可那是**手工配的**
  （环路报告 §2.5a/G-2 就是这个）。**⇒ 静态策略要"指向某个颜色的隧道"，前提是颜色和隧道表都得由控制器下发。**
- **G-4**：探针遥测现在全是垃圾（`tx=47740 rx=0 loss=100.0%`，今晚又当场复现了一次【量 §1.6】）。
  **动态策略的 SLA 判据在 G-4 修好之前无数据可用。**

**0.6 VPP 26.06 的 QoS：能限速、能打标、不能按五元组排队。**
`qos ?` 在这台机器上只有四条命令：`egress map / mark / record / store`【量 §1.4】，分类键是 DSCP/TTL/EXP/PCP，
**不是五元组**【上游】；旧的那套 class map / queue map / `set qos interface ... out` 与 HQF/HFSC 在 26.x 已不在树里
【上游；本机 `qos ?` 只有四条，双向对上】。要做"按策略的带宽保障"，原生只剩
**policer（CAR，可 `mark-and-transmit <dscp>`）+ classify session**，或在我们自己的 `sr_wg` 插件里加 per-policy 队列 ——
后者要重编 VPP 插件，按硬规则属于"必然中断"的操作，必须点名并给时长（→ P-25）。

**0.7 需要你拍的：P-17..P-28（§9）。**其中 P-26（要不要把 G-2b/G-2c 当下发缺口当 S0 前置）和
P-19（静态策略用哪三种表达里的哪一种）不拍，后面全都动不了。

---

## 1. 今晚在 lab 里取的证据原文

环境：`sdwan-loop` 项目、容器 `lp-cpe1`（此刻仍在跑）。全部是只读 `show` 与 `?` 帮助，
**没有下任何一条 add/del/set**，所以流量与表项没被动过。时刻 2026-11-03 23:2x CST。

**1.1 版本与插件（磁盘 + 运行时都看了）**

```
Version: v26.06-release   Compiled: 2026-06-24T09:29:33   Clang/LLVM 14.0.0
运行时已加载（编号是 vppctl 的序号）：
  1. abf_plugin.so      26.06-release  Access Control List (ACL) Based Forwarding
  2. acl_plugin.so      26.06-release  Access Control Lists (ACL)
 36. l3xc_plugin.so     26.06-release  L3 Cross-Connect (L3XC)
 38. lb_plugin.so       26.06-release  Load Balancer (LB)
 59. policer_plugin.so  26.06-release  Policer
 67. sasc_plugin.so     26.06-release  Session Aware Service Chaining (sasc)
 90. vrrp_plugin.so     26.06-release  VRRP v3 (RFC 5798)
 94. sr_wg_plugin.so    1.0.0          SR-WG: Segment Routing over WireGuard transit/encap/decap
```
磁盘上另有 `flowprobe_plugin.so`、`srv6*`、`wireguard_plugin.so`；**没有 `qos_plugin.so`** —— QoS 在 core 里，不在插件目录。

**1.2 命令面与弧的位置**

```
abf attach    abf attach <ip4|ip6> [del] policy <value> <interface>
abf policy    abf policy [add|del] id <index> acl <index> via ...

set acl-plugin acl        set acl-plugin acl [index <idx>] <permit|deny|permit+reflect>
                          src <PREFIX> dst <PREFIX> [proto X] [sport X[-Y]] [dport X[-Y]]
                          [tcpflags <int> mask <int>] [tag FOO] {多规则逗号分隔}
set acl-plugin interface  set acl-plugin interface <interface> <input|output> <acl INDEX> [del]
```

`show features` 的 `ip4-unicast` 弧（共 130 条路径）里，相关项的行序是：

```
22: policer-input（更靠前）… acl-plugin-in-ip4-fa … 45: ip4-flow-classify
47: ip4-policer-classify … 62: abf-input-ip4 … 67: ip4-lookup
```

`ip4-output` 弧里：

```
sr-wg4-encap（列首）… acl-plugin-out-ip4-fa … policer-output …
tcp-mss-clamping-ip4-out … flowprobe-output-ip4 … ip4-qos-mark … vlan-ip4-qos-mark
```

按接口看当前挂了什麼：

```
show interface features host-eth2  →  ip4-unicast: none configured      （cpe1 的客户侧入口，今晚没挂任何入向 feature）
show interface features host-eth0  →  ip4-output:   sr-wg4-encap         （WAN 出口挂的是我们自己的封装节点）
```

**1.3 `via` 令牌在这个 build 里存不存在**（`grep -c` on `/usr/bin/vpp`，非命中=不能用）

```
ip4-lookup-in-table   1     ip6-lookup-in-table  1     next-hop-table  1     out-labels  1
src-lookup            0     ← 26.06 没有，依赖它的写法全部作废
```

**1.4 QoS / policer 的命令面**

```
qos egress map   qos egress map id %d [delete] {[SOURCE][INPUT]=OUTPUT}
qos mark         qos mark <SOURCE> <INTERFACE> id <MAP>
qos record       qos record <record-source> <INTERFACE> [disable]
qos store        qos store <store-source> <INTERFACE> [disable]
                 ↑ 只有这四条，没有 class map / queue map / set qos interface

policer add      policer add name <name> [type 1r2c|1r3c|2r3c-2698|2r3c-4115] [color-aware]
                 [cir <cir>] [cb <cb>] [eir <eir>] [eb <eb>] [rate kbps|pps]
                 [conform-action drop|transmit|mark-and-transmit <dscp>] [exceed-action ...] [violate-action ...]
policer input    policer input [unapply] [name|index] <interface>
policer output   policer output [unapply] [name|index] <interface>
```

**1.5 表号实证**（`show ip fib summary`，cpe1）

```
ipv4-VRF:0,     fib_index:0, flow hash:[src dst sport dport proto flowlabel] locks:[adjacency:1, recursive-resolution:3, default-route:1, lcp-rt:1]
ipv4-VRF:250,   fib_index:1, ...                                            locks:[interface:3, API:1, CLI:1]
ipv4-VRF:10000, fib_index:2, ...                                            locks:[API:1]
```
`10000` = 颜色 0 + 主表 0，和 `ColorTable()` 的公式对得上【码 `fib.go:73-90`】。
**注意：此刻这台只有颜色 0 的表**（cpe1 的两条隧道 wg1001/wg1002 都是颜色 0），
所以"把流引到颜色 1"在这个实验台上现在还配不出来 —— §8 M-4 要先造一张非空的颜色表。

**1.6 探针现场（G-4 仍在）**

```
SR-WG probe: udp port 51829, 2 session(s), echoed=0 unknown_replies=0 alloc_fail=0
  wg1001 (sw_if_index=4) 10.250.0.1 -> 10.250.0.2  interval=300ms timeout=900ms enabled
    tx=47740 rx=0 lost=47736 late=0  loss=100.0%  rtt last=0.000ms min=0.000ms avg=0.000ms max=0.000ms  jitter=0.000ms
  wg1002 (sw_if_index=5) 10.250.0.3 -> 10.250.0.4  interval=300ms timeout=900ms enabled
    tx=47740 rx=0 lost=47736 late=0  loss=100.0%  ...
```

**1.7 一条我自己要用的提醒**：`show feature arc <name>` 这个命令在 26.06 里会无视参数、
把所有弧都倒出来；`show features` 同理。想知道某个弧只有哪些项，用 `show features | awk` 取段。
弧列表的**行序不等于运行时优先级**，只有 `show interface features <intf>` 里真正 applied 的顺序才是。→ M-6。

---

## 2. 前台调整①：把拓扑图搬进总览（Dashboard）

**2.1 现状**【码】

| 东西 | 在哪 |
|---|---|
| 总览页 | `console.html:316` 导航项、`VIEWS:654-658`、`viewDashboard() console.html:923`；内容是「需要关注」清单（`:954`），**它明确不展示隧道遥测** |
| 拓扑页 | key `underlay-topology`，`console.html:328`、`VIEWS:691-695`、`viewUnderlayTopology() :5096` |
| 底图 | `go:embed` 两幅预投影世界地图 JS：`assets/world-110m.js` 132KB、`world-50m.js` 681KB，`console_assets.go:26-30` 嵌、`:57-85` 无会话提供；前端 `ensureWorld() :4506-4529` 懒加载，放大 4.5× 才拉 50m |
| 数据 | `GET /api/underlay/topology` 的 `has_location/lon/lat/site_id`【码 `underlay_view.go:121-130`】；投影 `mapProject() :4550`、摆放 `undMapPos :4596-4600`、三级缩放 `undMapLevel :4574`、低倍下同设备对隧道并线标"N 条" `:4626-4652` |
| 刷新 | 全局 `/api/state` 10s 轮询 + `/api/events` SSE，`node.underlay` 事件触发刷新（`:628`） |
| 角色 | 拓扑页 **tenant-only**；`/api/underlay/topology` 服务端 `requireTenant`【码 `underlay_api.go:702-705`】；总览页是双角色 |

**2.2 方案（推荐 A，纯移动）**

- **A｜整体搬进总览**：把地图渲染面板放到 `viewDashboard()` 顶部当主视图，导航「监控与管理」组里的
  `underlay-topology` 项**删掉**（你说的是"移动"，不是"复制"）。
  代价与要处理的三件事：
  1. 进总览就得拉 132KB 底图（原来只有点进拓扑页才拉）—— 建议总览的地图面板默认折叠/`懒加载 + 记住展开状态`，别把首屏拖慢。
  2. **角色不一致**：总览是运营方也能进的页，而拓扑数据是 tenant-only。第一版建议**运营方总览不放地图面板**
     （放一行说明"地图仅租户可见"），要跨租户地图得单开一个运营方端点，那是另一个决定（P-17 附带）。
  3. 地图的视口状态（缩放/平移）现在只在这一个页有，搬过去后仍要"不重绘就跳回原位"，沿用 `:2640-2648` 的存状态手法。
- **B｜总览放紧凑图 + 拓扑页保留全屏**：多存一份视图状态、两条代码路径，不推荐（你说的原话是"移动到"）。

要动的地方（只列位置，本轮不动）：`console.html:316`/`:328` 导航、`VIEWS:654-658`/`:691-695`、
`render() :741-785` 分支、`viewDashboard() :923` 里插面板、`viewUnderlayTopology() :5096` 的渲染函数改造成可复用组件
（`ensureWorld :4506`、`mapProject :4550`、`undMapPos/Level :4574-4600` 这些本来就是函数级的，能整体搬）。
**风险很低：纯前端，不碰数据面、不碰任何 daemon。**

---

## 3. 前台调整②：新增「SDWAN策略配置」

**3.1 位置**：导航里 Overlay 配置组（`console.html:321-325`，末项 `leak-to-cpe :325`）与
「监控与管理」组（`:326`，`monitor :327`）之间插入 —— 就是你说的"监控上边、overlay 下边"。

**3.2 两个子项**：`policy-static`（静态策略配置）、`policy-dynamic`（动态策略配置）。
两页**只谈自己那一层**（你的既有要求：每页只说自己的配置层）：

- 静态页只说：五元组 → 下一跳设备 + 颜色 + 该颜色是否允许 Fallback 到颜色 0。
- 动态页只说：五元组 → 默认隧道 + SLA 需求 + 盒内守护进程的判定结果。
- 两页都**不重复** underlay 的隧道清单、不重复 overlay 的导通组/leak —— 需要引用时给一个跳转链接，不搬数据。

**3.3 设备选择控件**：沿用既有的「输入过滤 + 折叠下拉」单选控件（`console.html` 里 #132 那批已经落地的形态），
不用整页列表。**先选设备再看/编辑该设备的策略**，因为策略是 per-device 的（ABF 是 per-接口挂的）。

**3.4 角色**：tenant-only（和 overlay/underlay 同侧），`data-roles="tenant"` + `VIEWS` 里 `roles:["tenant"]`。

**3.5 状态口径**（这条不能松）：策略"生效了没有"必须有真实来源，控制台不造数据。
沿用 leak 页那套：每行带 `device_state.ok` / `source` / `applied_at_unix`（环路报告 §3.1 里能看到这几列是真的），
设备没上报就显示"未上报"，不显示 0、不显示占位的"已生效"。

---

## 4. 静态策略配置（= 这台设备上 VPP 的策略路由）

**4.1 配置形状（用户视角，就是你给的那句话）**

你的原话里有**两张表**，不是一张：

1. 策略行 = `匹配（五元组） + 下一跳设备 deviceID + 指向某个颜色的隧道`。
2. 颜色开关 = `这台设备上的某个颜色的隧道 是否支持 Fallback 到颜色 0` —— 粒度是
   **(设备, 颜色)**，不是每行策略。同一台设备上所有指向颜色 c 的策略共用这一个开关。

原型里就是这么摆的：上面一张策略表，下面一张"颜色 → 是否允许退到颜色 0"的开关表（颜色 0 自己不出现，
退到自己没有意义）。改这个开关会实时改写策略表里所有指向该颜色的行的 Fallback 列。

**4.2 映射到 VPP 的三个动作**【码/量/上游】

1. **建 ACL**：一条 `permit` 规则，字段直接是五元组：
   `set acl-plugin acl index <a> permit src <pfx> dst <pfx> [proto] [sport a[-b]] [dport a[-b]] [tag sr:<policy-id>]`。
   多行策略 ⇒ 多规则；ABF 引用的是 acl index。
2. **建 ABF policy**（它的 path 集就是 fib path，支持 `preference`）：
   `abf policy add id <p> acl <a> via ... (preference <n>) ...`
3. **挂到入口接口**：`abf attach ip4 policy <p> <客户侧入口接口>`。
   注意**这一步隐含一个你没说的必填项**（下面 4.4）。

**4.3 `via` 用哪种写法 —— 三个候选，这是 P-19**

| 号 | 写法 | 语义 | 好处 | 代价 |
|---|---|---|---|---|
| 甲 | `via <对端 overlay /32 下一跳>` | 钉死这条隧道（隧道本身=一个颜色+一个对端） | 最简单，"下一跳设备+颜色"两个信息就在下一跳里 | 该隧道 down 时**没有自动备份**，必须靠守护进程改策略 |
| 乙 | `via <下一跳> ip4-lookup-in-table <颜色表>` | 既钉下一跳、又在该颜色的表里重查 | 兼顾 | **`via <nh>` 与 `ip4-lookup-in-table` 能不能同一条 path 共存，我答不了 → M-2** |
| 丙 | `via ip4-lookup-in-table <颜色表>`（不钉下一跳） | "走这个颜色的全部可用路径" | 保留多路径与 preference，动态性最好；sr-fib 改了颜色表路径，策略不用动 | 表达不了"必须是这台设备" |

**我的建议是丙 + 一句补充**：`下一跳 deviceID` 这个约束交给"这台设备的这个前缀本来有哪些路径"决定
（overlay 的 /32 下一跳就是那台设备的隧道口），策略行仍然是"颜色"级。
若你坚持要钉设备，就走甲，并且 Fallback 必须由守护进程改策略而不是靠 preference。
**这是你的决定，不是我的。**

**4.4 Fallback 到颜色 0（开关在 (设备, 颜色) 这一级）**

推荐形态（若 4.3 选丙且 M-3 通过）：对这台设备上**每一个开了 Fallback 的颜色 c**，把该颜色对应的 ABF path 集写成
`[ ip4-lookup-in-table ColorTable(c, 主表) preference 1, ip4-lookup-in-table ColorTable(0, 主表) preference 2 ]`，
开关关掉时只留第一条。VPP 的 preference 主备 + 自动回落是原生行为（0 最优）。
所以一个开关改动会**重渲染这台设备上所有指向颜色 c 的 ABF policy**，不是只改一行。
**成立的前提是 M-3**：目标表里没有这个前缀时，这条 path 是被判定不可用（那 preference 回落就有效），
还是包直接落 `no-route`（那"自动回落"根本不发生，Fallback 就得由守护进程做）。

另有一处必须讲清的覆盖关系（P-27）：**逃生开关 `escape_all_colors` 是租户级"主表收全部颜色"**【码 `fib.go:314`：
`t.Color == 0 || escape` ⇒ 关时主表只收颜色 0】，而这里的 Fallback 是 **(设备, 颜色)** 级。两者并存时谁覆盖谁，
页面上必须写死一句话，不能让人猜。

**4.5 那个隐含必填项：接口 / VPN**

ABF 是**入向、按接口**的 feature；而颜色表号 = 颜色×1000+10000+**主表号**，主表号 = VPN 表号【码 `fib.go:51-90`】。
一台盒子可能有多个客户侧入口、多个 VPN ⇒ 同一条"策略"在 VPP 里会展开成 (入口接口 × VPN) 若干条 ABF policy + ACL。
三种做法（P-18）：
- 丙（推荐）：页面不让用户选接口，自动作用到"该设备所有客户侧入口"（设备接口清单 #105 已经上报，能拿到），
  展开规则写进提交说明；VPN 用入口接口所在表（lab 实测：客户口在 table 0 ⇒ 颜色 0 表 10000【量 §1.5】）。
- 甲：页面加一个"入口接口"单选（更精确，多一步操作）。
- 乙：策略行带 `vpn_id`（和现有 leak 页的形态一致，但把内部编号暴露给客户视角，我不推荐）。

**4.6 配置落点：bundle 第 7 段，照 VRRP 先例**

现有 6 段：`frr / vpp / boot / route / srfrr / vrrp`【码 `internal/apply/apply.go:67-74`】。
**VRRP 这段的先例正合适**：它"不是盒子存的一个文件，而是一条 reconcile 指令，
租户删掉最后一行时 sr-apply 必须发空清单"【码 `cmd/sr-ctrl/config_render.go:332-338`】。
ACL/ABF 同样是**活在 VPP 内存里**的东西，所以策略段应当：

- 第 7 段 `policy`，reconcile 语义（行集 = 期望态，全量对比 VPP 实际态）；
- 应用者是 `sr-apply`（和 VRRP 一样），**不新增进程**，静态部分不需要守护进程；
- 键用 **VPP 接口名不用 sw_if_index** —— VRRP 段注释里这条理由原样成立：
  索引是盒内本地、VPP 重启就会变【码 `config_render.go:339-343`】；
- 开机重放（`sr-apply boot`）必须能重建；
- **别踩 G-5 那个死结**：新进程/新段在"表为空"时不许 `log.Fatal` 拒绝启动，也不许"没有上一份就不写盘"
  （环路报告 §2.5c/G-5：sr-frr 拒绝空表启动 × 第 5 段热更强制 = 首次发布配不出）。

热更路径沿用既有的：控制器发布 → 推包（`/v1/watch`）→ `sr-agent -apply` → `sr-apply` 段级 reconcile；
**静态策略的生效不许重启任何 daemon**（硬规则第 1 条），失败就保留上一份并如实报错（第 5 条）。

**4.7 前置缺口**：见 §0.5 的 G-2b/G-2c。**颜色和逃生开关都没有下发通路**，
所以"S1 静态策略"要么先把颜色/隧道表下发补齐（我推荐，作为 S0），要么策略只能配在手工值的颜色上（不可接受）。

---

## 5. 动态策略配置 + 盒内策略守护进程

**5.1 配置形状**：一行 = `匹配（五元组） + 默认隧道 + SLA 需求（延迟 / 抖动 / 丢包）`。
VPP 侧落法与 §4 完全相同（ACL + ABF + attach），差别只在 **path 集由盒内守护进程按实测值改**。

**5.2 SLA 数据只能从 sr-agent 已有的那一条采样里来（硬约束）**

现状【码】：探针每会话字段是 `vppapi.ProbeStats`（`internal/vppapi/telemetry.go:50-69`）：
`Tx/Rx/Lost/LateReplies` + `LastRTTus/MinRTTus/AvgRTTus/MaxRTTus/JitterUs/LossPermille`；
默认 `IntervalMS=300`、timeout=3×interval【码 `internal/wire/wire.go:829-831`】；
BFD 不给任何质量指标【码 `vppapi/telemetry.go:22` 注释原文】；
**窗口增量在 agent 算，控制器不算**（`internal/agent/telemetry.go:337-350` 对 loss 做窗口重算；
`wire.go:911-914` 注释原文 "no controller-side delta bookkeeping"）。

⇒ 新守护进程**不许自己 dial VPP 二进制 API**：环路报告 §4.3 已经量到"sr-agent 采样的那 0.5–7s 里
四台盒子 BFD 收包计数同时停摆"，再加一个 dump 源就是自己造第二个抖动源。
推荐甲：**sr-agent 把已有的窗口结果在盒内落一份只读端点/文件**（`/run/sr-agent/metrics.json` 之类），
守护进程读它。附带好处：断网/控制器不可达时策略仍能判（本地自治，符合硬规则第 3 条）。→ P-20。

**5.3 一个必须先修的口径问题**：现在 `min/avg/max` 是**会话生命期**的极值、jitter 是当前 RFC3550 EWMA
【码 `wire.go:878-881` 注释原文】。会话不重置的话，一次历史尖峰永远留在 `max` 里 ——
**拿它当 SLA 判据会一次性把一条隧道永久判死**。所以滚动窗口口径要先定（P-21）：
要么 agent 对 RTT/jitter 也做窗口（和 loss 同口径，改动最小），要么插件侧加窗口。

**5.4 抑制（这条不是调优，是门槛）**

环路报告的教训：150ms 检测 + **零 hold-down** ⇒ 一次宿主抖动就撤前缀，放大成 1.0–12.0s 黑洞【报告 §4.4】。
所以动态策略的决策循环必须带（P-22，数值等你给）：
- 连续 N 个窗口违约才切（建议 N≥3，即 ≥900ms 的 300ms 窗口）；
- 切换后最短驻留 M 秒才允许再判；
- **回切要比出切更保守**（需要更好的余量，不是"刚好达标就回"）；
- 每次切换落一条可追溯记录（原因 = 哪个指标、实测值 vs 需求、切换时间）。
没有这四条，我拒绝把它接进客户流量路径 —— 这是我们今晚刚量出来的东西。

**5.5 候选域是个歧义（P-23，你的原话没覆盖）**

"根据 SLA 调整下一跳的隧道" —— 调到**哪些**隧道？两种读法：
- 甲：只在**同一下一跳设备**的其他颜色里换（"这台设备的隧道 SLA 不好，退到它另一条颜色的隧道"）；
- 乙：在**同一目的前缀可用的所有隧道**里换（跨设备，包含中转/多跳）。
我倾向甲（可控、可解释、不会和导通组打架），但这是你的语义，不是我该替你定的。

**5.6 动作走哪条路（不许重启任何 daemon）**

最自然：只改 ABF policy 的 path 集（`abf_policy_add_del` API，等价 CLI 的 add/del）。
**必须量的三件事**：
- 改 path 是不是 delete-then-add、有没有黑洞窗口（M-1/M-2）；我们有过先例：改色的先删后加实测 0ms【报告 §1.4/拍板 O4】，
  但 ABF 没有对应实测；
- 用 preference 排序 + make-before-break（先加新 path、确认解析成功、再删旧）能不能把窗口压到 0；
- 该操作是否只影响命中策略的流（不影响其它客户流量）。

**5.7 与既有机制的覆盖关系（必须在页面上写清）**

| 已有机制 | 粒度 | 和动态策略的关系 |
|---|---|---|
| BFD→sr-fib 撤路径（`fib.UnreachablePeers`→`PlanFrom` 的 `down`，`internal/fib/reach.go:20-47`） | 每条路径 | 它管"通不通"，策略管"好不好"；两者都会动 path 集，**必须定先后** |
| VRRP（第 6 段） | 站点第一跳 | 站点内谁 MASTER 决定流量从哪台盒子进来，策略在进来之后选隧道 |
| 逃生开关 | 租户级"主表收全部颜色" | 见 P-27 |
| preference 主备 | 每条前缀 | 策略行会**新造一层** preference（表内 vs 颜色 0 表），别和 sr-fib 写的那层混为一谈 |

**5.8 落地形态**：新增盒内二进制 `sr-policy`（延续 `sr-ctrl/sr-agent/sr-route/sr-fib/sr-frr/sr-apply` 命名），
进 `controller/deploy/sr-box/start.sh` 的开机清单（O5-d 那套），容器 `unless-stopped` 只拉容器不跑开机清单
这个坑已经在报告 §1.5 记过，清单必须自己覆盖它。
**它和 `sr-agent` 的关系要先定**（P-24）：独立进程（隔离性好、多一条 VPP API 连接要管）
vs 挂在 sr-agent 里当一个 loop（复用连接，但把策略决策和心跳/采样捆在一个进程里，故障域变大）。
我倾向独立进程 + 只读盒内端点，理由就是 §5.2。

---

## 6. VPP 对 QoS 的支持（第 6 条调研的结论）

**6.1 原生能做（本机 26.06 已验命令存在）**

| 能力 | 机制 | 分类键 | 作用点（实测弧） |
|---|---|---|---|
| 五元组→改路径/改表 | acl-plugin + **ABF** | 五元组（src/dst/proto/sport/dport/tcpflags） | `ip4-unicast` 弧，**在 `ip4-lookup` 之前**【量 §1.2】 |
| 限速/丢包/打标 | **policer**（CAR：1r2c/1r3c/2r3c-2698/2r3c-4115，cir/cb/eir/eb） | policer 本身按接口；**按流要配 classify session** | `policer-input`（ip4-unicast）/ `policer-output`（ip4-output）【量 §1.2/§1.4】 |
| 改 DSCP / 记录 | **qos mark / egress map / record / store** | DSCP、TTL、MPLS EXP、VLAN PCP（`qos_source` 只有 EXT/VLAN/MPLS/IP）【上游】 | `ip4-qos-mark` 挂在 `ip4-output` 弧（实测在列）【量 §1.2】 |
| 每流统计 | `ip4-flow-classify`（弧内位次 45）、flowprobe(IPFIX) 插件（磁盘有）、sasc（已加载，experimental） | 流 | sasc 能做每会话包长/IPG 直方图与突发检测【上游 `services/flow-quality/`】 |
| 加权多路径 | `lb_plugin` + fib path `weight`/`preference`；VRF 的 `flow hash:[src dst sport dport proto flowlabel]`【量 §1.5】 | 五元组做 hash | 转发面内 |

**6.2 原生不能做（关键，别按思科/华为的 QoS 想它）**

- **没有 class map / queue map / 输出队列**：26.x 的 `qos` 只剩 4 条命令【量 §1.4 与上游一致】，
  老的 `set qos interface <intf> out <class-map>` 那套不在树里；
- **没有 HQF/HFSC 整形**（2020-01-30 已 deprecate dpdk hqos；树内 `hfsc/hqf/shaper` grep 只命中 Marvell 驱动）【上游】；
- **QoS 分类不能按五元组**：qos 插件的分类键是 DSCP/EXP/PCP/TTL。
  ⇒ "这条策略的流量给 20% 带宽、那个队列优先"**没有原生入口**。

**6.3 那我们这套隧道结构里还有什么特别的坑**

- `sr-wg4-encap` 自己就挂在 `ip4-output` 弧上【量 §1.2】，和 `ip4-qos-mark`、`policer-output`、
  `acl-plugin-out-ip4-fa` **同弧** ⇒ 谁先跑决定"改的是内层 DSCP 还是外层 DSCP"。弧列表行序不是优先级，
  必须用 `show interface features <intf>` 实测 → **M-6**。
- 内层包在客户侧入口命中 ABF/ACL（那是查 FIB **之前**，符合语义）；**外层已封装的包**再命中同一 feature 时
  匹配的是外层五元组 → 出向 feature 千万别乱挂【推断，M-6 顺带验】。
- **分片**：ACL 的 hash 查找对非首片默认只匹配 L3【上游 `acl_hash_lookup_doc.rst`】。
  而我们的 MTU 1360 场景实测"执行方式是内层分片"（任务 #19 的既有结论）⇒
  **带 sport/dport 的策略对分片流可能整体失效**。这条要单独量 → **M-5**，我认为它是这个设计里最容易翻车的一条。
- 如果一定要"按策略的队列/带宽保障"：只剩自研 —— 在 `sr_wg` 插件里做 per-policy 队列。
  按硬规则，这要重编并热替换插件，属于**必然中断客户流量**的操作，方案里必须点名并给实测时长（P-25）。

**6.4 对标"按流量特性切路径"的上游现成机制**（供参照，不代表我们用）

SR Policy（SRv6-TE）`sr policy add bsid ... next ... (weight) (fib-table)` + `sr steer l3 <pfx> via bsid` —— 只按前缀/接口，无五元组、无实测触发【上游】；
BFD↔FIB 联动 `src/vnet/bfd/fib_bfd.c`：**只有 multihop 会话**直接绑 FIB entry（我们是多跳，符合）【上游】；
多路径 `ip route ... via ... weight <u8> preference <u8>`，注释明写 "lowest preference is preferred"【上游】。
⇒ **上游没有"按实测 SLA 自动换路径"的现成机制**，这正是 §5 那个守护进程要自己写的部分 —— 也意味着这块我们没有任何可以照抄的参照模型，风险最高。

---

## 7. 建议的落地顺序（每步都可验证、都不冷启动）

| 步 | 内容 | 为什么在这个位置 |
|---|---|---|
| **S0** | 补下发通路：隧道 `color` + `escape_all_colors` 进 bundle（并让 sr-fib 的隧道表由控制器下发，替掉手工 `fib.json`，即环路报告 G-2） | 静态策略的一切前提。§0.5 两条缺口不闭，策略页就是第二个"逃生开关式摆设" |
| **S1** | 前台两项：拓扑搬进总览 + 新导航项与两页（先只做模型/API/页面，盒侧不落地或只 dry-run） | 纯前端 + 后端模型，风险最低，你明天就能看效果；也是我今晚交付原型的部分 |
| **S2** | 静态策略闭环：第 7 段 + `sr-apply` reconcile + 判据脚本 + lab 实测（M-1/M-2/M-3/M-5） | 只加"引流"，不加"自动决策"，故障域可控 |
| **S3** | 动态策略：SLA 需求入库 + 滚动窗口口径 + `sr-policy` 守护进程（带 §5.4 四条抑制） | 依赖 S2 的动作面 + G-4 修好（否则无数据） |
| **S4** | QoS（若你要）：policer/classify 路线或自研队列路线 | 独立议题，且自研路线属必然中断 |

顺序不是我的偏好，是依赖：**G-4 不修 → SLA 无数据；颜色不下发 → 静态策略不生效；M-3 不量 → Fallback 语义未知。**

---

## 8. 必须先量再决定的清单（M-1..M-8）

每条给"注入动作 + 判据 + 口径"。口径一律用环路报告同一套：六条流打流、逐秒序号窗口、
`tools/fib-dump` 读回 path 与 preference、`show ip fib` 前缀集合、进程 pid 与邻居 uptime 不变。

| 号 | 要量什么 | 判据 |
|---|---|---|
| **M-1** | 在**活着的客户侧入口接口**上 enable ACL/ABF 入向 feature 的那一瞬间，客户流量掉不掉 | 打流 ≥60s，覆盖挂 feature 前后各 30s；丢包数 + 连续窗口时长。这是硬规则第 4 条要求的"点名并给时长" |
| **M-2** | `via <下一跳> ip4-lookup-in-table <表>` 这种组合是否被接受、语义是什么 | 在实验台造一条临时策略（只动 lab），看 `show ip fib`、`show abf policy`、`trace` 走位 |
| **M-3** | 目标颜色表**没有**这个前缀时，DEAG path 是被判不可用（自动走次路径）还是落 no-route | 拿一张空的颜色表（lab 里颜色 1 表天然为空【量 §1.5】）打一条命中策略的流，看是走 preference 次路径还是丢 |
| **M-4** | 造一张**非空**的颜色表（同设备两条不同颜色隧道），再验一次策略选路与 fallback | 需要先按 O2 的做法补一条非 0 颜色隧道；验完拆除 |
| **M-5** | 分片流（内层分片，我们 MTU 1360 的既有实测路径）在带 sport/dport 的策略下命中不命中 | 造 DF=false 大包流，对比首片/非首片命中数；`show acl plugin`/trace 计数 |
| **M-6** | `ip4-output` 弧上 `ip4-qos-mark` / `policer-output` / `acl-plugin-out-ip4-fa` 与 `sr-wg4-encap` 的**实际先后** | 在某 WAN 口同时挂两个 feature，`show interface features <intf>` 看 applied 顺序，再用 trace 确认包在每一跳的头部长什么样 |
| **M-7** | `sr-policy` 读盒内指标这件事会不会给 VPP 加新的 dump 压力 | 用 `controller/tools/teletiming` 同口径跑 ≥30 轮，对比基线（就是环路报告 §4.3 那个口径） |
| **M-8** | 开机重放能不能重建策略（VPP 重启后 ACL/ABF 全丢），且不踩 G-5 式死结 | 在 lab 里重启 VPP（lab 允许，生产不允许），跑 `sr-apply boot`，核对策略行全部回来、且首次发布路径不为空表而拒绝启动 |

另外两条是**复跑今晚报告的遗留项**，不是新事：
**R-5**（SIGSTOP 反证，报告 §8.2）——它决定"宿主冻结 vs 盒内 dump"的归因，而归因直接决定 §5.2 那条硬约束有多硬。

---

## 9. 待拍（P-17 起，接环路报告的 P-1..P-16）

| 号 | 要你拍的 | 我的倾向（只算倾向，不算结论） |
|---|---|---|
| **P-17** | 拓扑进总览是"移动"还是"两边都放"；运营方总览要不要也放地图 | 按你原话＝移动；运营方第一版不放（数据是 tenant-only） |
| **P-18** | 静态策略作用到哪些入口：全部客户侧入口（自动）/ 页面上单选接口 / 策略行带 vpn_id | 自动，展开规则写进提交说明 |
| **P-19** | `via` 用甲（钉隧道）/ 乙（钉+换表）/ 丙（只换颜色表） | 丙；要钉设备就甲 + 由守护进程做 fallback。**乙在 M-2 之前不许选** |
| **P-20** | SLA 数据源：sr-agent 盒内转喂 / 守护进程自己 dump VPP / 换 sasc-flowprobe | 只有第一种可接受（理由见 §5.2 与今晚报告） |
| **P-21** | SLA 窗口口径：min/max 现在是会话生命期极值，要不要改成滚动窗口 | 要改；和 loss 同口径在 agent 侧算改动最小 |
| **P-22** | 抑制参数数值：N 个窗口、驻留 M 秒、回切余量 | 给我数字我就照写；没数字我不接进客户流量路径 |
| **P-23** | "调整下一跳隧道"的候选域：同一设备的其他颜色 / 该前缀所有可用隧道 | 前者 |
| **P-24** | 策略守护进程独立 `sr-policy`，还是挂在 `sr-agent` 里 | 独立 |
| **P-25** | QoS 到底要不要：只做选路（原生够）/ policer+classify 限速打标 / 自研 per-policy 队列（要重编插件＝必然中断，需点名+时长） | 先只做选路，QoS 单独立项 |
| **P-26** | **S0 下发缺口**（颜色 + escape + sr-fib 隧道表）算不算策略层的前置必修 | 算。不修的话新页面就是第二个"逃生开关" |
| **P-27** | 逃生开关（租户级）与 Fallback 开关（**(设备, 颜色)** 级）并存时的覆盖关系与页面文案 | 策略优先，页面上明写"逃生开关不参与已命中策略的流"（**这句要 M-3 过了才敢写**） |
| **P-28** | 环路报告场景 E（逃生开关）的判据要不要按 G-2b 改写：走控制台根本测不出效果 | 要改；并把这条补进报告 §9 的缺陷清单 |

---

## 10. 交付物位置

- 本文：`docs/sdwan-policy-plan.md`（仓库内，随本轮提交）。
- 前台可交互原型（**纯 mock、自包含单文件、配色写死不依赖宿主主题、中文标签、悬浮说明**）：
  `visualizations/policy-console-proto.html`，绝对路径
  `/Users/louis/.qoder-cn/app/chat-session-visualizations/34cbe6f4-665f-4ddb-8214-edfa36c9b6a3/visualizations/policy-console-proto.html`
  （183,078 字节；CSS 逐字取自 `console.html` 第 8–278 行，底图把 `assets/world-110m.js` 整段内联，投影函数逐字取自
  `console.html:4550`，所以它不是一个"像控制台的图"，而是同一套外壳。原型不进仓库，沿用 `topology-map-proto.html` 的先例。）

原型已在真实浏览器引擎里跑过一轮（CDP 注入脚本逐页操作，控制台 0 条报错）：导航顺序、拓扑项置灰、地图缩放/全景/拖动
（viewBox 随 1–14× 变化、4.5× 以上提示换 50m 明细底图）、设备过滤+单选下拉、增删策略行、颜色级 Fallback 开关实时改写策略表、
失联设备的 SLA 行一律显示"未上报"。**唯一没做到的：截图。** 内嵌 Browser 面板当时没有可见表面对象（viewport=0x0、visible=false），
截图动作被宿主拒绝，所以这一轮的"看到"仍然是结构级而非像素级 —— 你打开这个文件（或把 Browser 面板切到前台）我再补截图。

复核环境（今晚仍在跑）：http://127.0.0.1:8089/ ，角色租户，账号 `loop-tenant`。
所有 `show` 取证都可以拿它 + `docker exec lp-cpe1 vppctl ...` 复跑。
