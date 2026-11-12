# QoS 路径 C：Cisco 的模型 → 这台盒子能兑现的那一半

出这份文档的动机与 `queue-plan-next.md` 相同：AGENTS.md §1 与「controller 增量先出方案、
点头后再写代码」（memory: `feedback-do-not-develop-controller`）要求动手前先落纸面。
owner 这一条的命令是**先读 Cisco 的实现**，所以第一节是 Cisco 的原文形状，
第二节才是盒子的实测账，第三节才开始谈我们的形状。

本轮的取证全部在 CPE4（VPP `v26.06-release`，build 2026-06-24）上跑，命令与原始输出
在 §7。**没有改产品代码，也没有在盒上留下任何东西**（测试口/测试 policer 已清，§7.4）。

---

## 0. 结论先行

1. **Cisco SD-WAN 的 QoS 是两半**：无状态的「分类 + 标记 + 限速」，和有状态的
   「队列调度（带宽%/缓冲%/WRR/LLQ/RED）」。前者是逐包动作，后者要有**真调度器**。
2. **这台盒子原生只有前半**。VPP 26.06 这版没有 DPDK、没有 hqos、没有 qos 插件，
   网卡是 af_packet 且**只有 1 个共享 TX 队列** —— 队列这一半在 VPP 里没有任何落点。
3. **后半能借到，但要付三笔账**：实测 `af_packet` 加 `qdisc-bypass-disable` 后，
   VPP 的出口包**真的会过主机内核的 qdisc**，HTB 也**真的会限速**（100 kbit 下
   500 发只过 60 发、88% 丢、backlog 201 包）。代价是：要重建 WAN 口（隧道会抖）、
   把出口数据面的一半搬进主机内核、以及管理面从此和限速器**同命运**。
4. **建议**：先只做 C1（VPP 原生的分类/标记/限速），并且**把 Cisco 的队列字段做成硬错误**
   —— 不认识就拒收，不接受之后静默忽略（那正是我们一路在避免的"假配置"）。
   C2（主机 tc 调度）单独立项，先量 §6.4 那三项再拍。
5. **§9 是关键修正**：真正的对象是 **WireGuard 隧道**，不是网卡。隧道流量不经内核，
   所以 C2 对隧道无效；外层 `udp sport` 逐隧道不同（实测 20000/20001/20002），
   三条可行路径重排在 §9.3，加密前的挂点判断在 §9.4。
6. **§10 是第二次修正，而且是坏消息**：`wg` 口的 `ip4-output` 弧**不承载转发流量**
   （只有本机自发包走它），所以 §9.4 的挂点作废。转发流量能拿到内层 DSCP 的唯一
   不用改 VPP 内部结构的位置是**入口弧 `ip4-unicast`**（O3），代价是每包多一次 FIB 查表。
7. **§10.6 是事故记录**：O3 版上盒即瘫（数据面死、重启无效），已回退到 20:12 的版本、
   盒子恢复基线。**O3 版在容器里带 LCP 复现定位之前，禁止部署到任何真机。**

---

## 1. Cisco SD-WAN 的 QoS 由什么组成

来源：Cisco「Implement QoS in Cisco SD-WAN」（vEdge 213408）与「Understand QoS Fundamentals
and Class-Default Behavior in SD-WAN」（222695）。下面是它的形状，不是我的转述发明。

### 1.1 出口方向的六段流水线

| 段 | 名字 | 做什么 |
| --- | --- | --- |
| 1 | Classify input | 按 ACL 把包归到一个**转发类**，类再映射到**出口队列** |
| 2 | Input ACL + Policer | 入向限速，超速丢弃（省上游带宽） |
| 3 | Route lookup | 查表选出口 |
| 4 | Output ACL + Policer | 出向限速；合规转发，超速降优先级或丢 |
| 5 | **Output scheduling** | 每个队列的带宽、缓冲、丢包优先级（QoS map） |
| 6 | **Rewrite output** | 重写外层 IP 头的 DSCP |

第 5、6 段是这套东西的**价值所在**，也是它跟"限速"的分界线：第 5 段决定"谁先走"，
第 6 段决定"上游/运营商看不看得见你的优先级"。

### 1.2 五个配置对象

```
# 1) 转发类 → 出口队列
policy class-map class voice         queue 0
policy class-map class critical-data queue 1
policy class-map class bulk-data     queue 2
policy class-map class best-effort   queue 3

# 2) 每类的调度参数
policy qos-scheduler voice-scheduler class voice \
       bandwidth-percent 20 buffer-percent 20 scheduling llq  drops tail-drop
policy qos-scheduler crit-scheduler  class critical-data \
       bandwidth-percent 40 buffer-percent 40 scheduling wrr  drops red-drop
policy qos-scheduler be-scheduler    class best-effort \
       bandwidth-percent 20 buffer-percent 20 scheduling wrr  drops red-drop

# 3) 把调度器组成一张 map
policy qos-map MyQoSMap qos-scheduler voice-scheduler qos-scheduler crit-scheduler ...

# 4) 应用到出口
interface ge0/1 qos-map MyQoSMap

# 5) 分类（ACL）+ 标记（set dscp）
policy access-list MyACL
 sequence 10 match dscp 46                action accept class voice
 sequence 20 match source-ip 10.1.1.0/24  action accept class bulk-data  set dscp 32
 sequence 40                              action accept class best-effort set dscp 0
 default-action drop
vpn 10 interface ge0/0 access-list MyACL in
```

### 1.3 三条硬不变量（读实现才看得到的）

1. **8 个队列，queue 0 保留给控制面 + LLQ**；映射到 queue 0 的类**必须**配 `scheduling llq`。
   所有控制流量都走 0 号队列并且一定发出去。
2. **queue 1–7 给数据**；`class-default` 自动创建并**自动落 queue 2**（所以不显式定义
   class-default 时 Manager 上根本没有它的统计）。
3. `bandwidth-percent` 是**相对接口/shaper 速率**的百分比；`buffer-percent` 是相对队列缓冲的
   百分比。两者都是"承诺"，不是"上限"——`wrr` 类在别人空闲时可以用超出自己份额的带宽
   （借带宽），`llq` 类拿的是**优先发送权 + 自己的令牌桶**。

### 1.4 哪些字段是承诺，哪些是装饰

| 字段 | 客户能不能感觉到 |
| --- | --- |
| `scheduling llq` / `wrr` | **能**。拥塞时谁先走就是这个决定的 |
| `bandwidth-percent` | **能**。这就是"给我留 20%" |
| `buffer-percent` / `drops red-drop` | **能**，但要拥塞才显形（RED 是提前丢，保 TCP） |
| `set dscp` | **能**，但要看上游认不认这个标记 |
| policer（`cir`/`eir`/超速动作） | **能**。这就是"限速到 X" |

这张表是后面所有取舍的依据：**第 5 段的字段兑现不了就不能收，收了就是骗人。**

---

## 2. 这台盒子实测能表达什么（CPE4，VPP 26.06）

### 2.1 没有调度器（决定性）

```
$ vppctl show plugins | grep -iE 'dpdk|hqos|qos'
 12. crypto_sw_scheduler_plugin.so  26.06-release  SW Scheduler Crypto Async Engine plugin
```

87 个插件里**没有 dpdk、没有 hqos、没有 qos**。HQoS 是 DPDK 专属 —— 没有 DPDK 就没有
任何"队列/调度器"原语。网卡也不是 DPDK：

```
$ vppctl show hardware-interfaces | head
host-enp0s8  1  up  host-enp0s8
  Link speed: unknown
  RX Queues: 0  main (0)  interrupt
  TX Queues: 0  no  0            ← 一个 TX 队列，shared
  Linux PACKET socket interface v3
  FEATURES: qdisc-bpass-enabled
```

**1 个共享 TX 队列**意味着即使有调度器也没有"队列"可分。

核心的 QoS 特性节点倒是在（`vppctl show features`）：`ip4-qos-mark`、`ip4-qos-record`、
`ip4-qos-store`、`ip4-policer-classify`、`policer-input`、`policer-output`、`ip4-punt-policer`。
**这些全是"标记/限速"，没有一个是"排队"。**

### 2.2 qdisc 旁路 A/B（本轮最关键的一枪）

af_packet 默认把 `PACKET_QDISC_BYPASS` 打开，所以 VPP 发的包**完全绕过主机内核的 qdisc**。
两组 veth 对照（不动生产口，理由见 §7.4）：

```
# A 臂：默认参数建口
$ vppctl create host-interface name srq0
$ tc qdisc show dev srq0
qdisc htb 1: root ... Sent 0 bytes 0 pkt (dropped 0, overlimits 0 requeues 0)

# B 臂：建口时加 qdisc-bypass-disable
$ vppctl create host-interface name srp0 qdisc-bypass-disable
$ tc qdisc show dev srp0
qdisc htb 1: root ... Sent 11358 bytes 105 pkt (dropped 0, overlimits 0 requeues 0)
```

两臂各打了 100 个 ping。**A 臂 qdisc 一个包都没看见，B 臂 105 个包（含 ARP）全过了一遍。**
结论：默认形状下 `tc` 对 VPP 出口**完全无效**；加 flag 之后才进 qdisc。

### 2.3 HTB 真的会限速（不是只计数）

同一个 B 臂，只改 class 速率、不改别的：

| 臂 | class 速率 | 结果 |
| --- | --- | --- |
| 基线 | 100 mbit | 500 发 **499 收（0% 丢）**，514 ms |
| 限速 | 100 kbit | 500 发 **60 收（88% 丢）**，511 ms，`overlimits 58`、`backlog 201p`、`tokens -68894` |

所以主机内核的 HTB 是**真调度器**：它按 class 排队、按 rate 出队、超了要么排队要么丢。
Cisco 第 5 段那些字段（bandwidth / buffer / wrr / llq / red）在 HTB 世界里都有对应物。

### 2.4 限速（policer）对象可用，标记也在，但 CLI 不可用

policer 对象建得出来，参数语义就是我们要的：

```
$ vppctl policer add name a2 cir 1000 eir 0 cb 9000 eb 0 conform-action transmit
$ vppctl show policer
Name "a2" type 1r2c cir 1000 eir 0 cb 9000 eb 0
rate type kbps, round type closest
conform action transmit, exceed action drop, violate action drop
```

机箱上的 `policer.api.json` 给出精确契约（§7.3）：`policer_add_del` crc `0x2b31dd38`，
`conform/exceed/violate_action` 是 `{type u8, dscp u8}` 结构，枚举
`SSE2_QOS_ACTION_API_DROP=0 / TRANSMIT=1 / MARK_AND_TRANSMIT=2`。
**标记和限速是同一个对象上的两个动作** —— 这就把 Cisco 的"policer"和"set dscp"接上了。

但 **CLI 的 `qos mark` 在这版是坏的**：对 `host-enp0s9`、`host-enp0s8`、刚建的 `loop0`，
四种 token 顺序全部 `qos mark: Failed to map interface`。这与本仓已有的那条结论同形
（`vppapi/acl.go` 头注：`set acl-plugin acl` 返回成功但什么都没建）——
**CLI 把失败吞掉，二进制 API 才带回 retval**。标记只能走 API。

### 2.5 分类到 policer 的接线（消息面已确认）

`vppctl show api message-table | grep policer` 只给出 `policer_classify_set_interface` 与
dump/details，**没有** `policer_classify_add_del_table` / `_add_del_session`。
所以 VPP 的模型是：**表由核心 `classify_add_del_table` 建，会话的 `hit_next_index` 指向
policer 索引，再用 `policer_classify_set_interface` 把这张表挂到口的 policer 弧上。**

| 消息 | 来源 | CRC | 已确认 |
| --- | --- | --- | --- |
| `classify_add_del_table` | core `classify.api.json` | `0x6849e39e` | ✅ |
| `classify_add_del_session` | core `classify.api.json` | `0xf20879f0` | ✅ |
| `policer_add_del` | plugins `policer.api.json` | `0x2b31dd38` | ✅ |
| `policer_details` | plugins `policer.api.json` | `0x72d0e248` | ✅（回读对账用，含 `single_rate`/计数） |
| `policer_classify_set_interface` | 只有 message-table 里的 id 1062 | ❓ | 机箱**没装**这个 `.api` json |

`policer_classify_set_interface` 的 CRC 是 C1 落地前唯一的技术未知，取法三条（§5.6）。

### 2.6 能力表：Cisco 的每个对象落在哪

| Cisco 对象/字段 | 本平台原生（C1） | 借主机 tc（C2） |
| --- | --- | --- |
| `class-map class X queue N` | 没有队列；类只体现为"一个 DSCP/一个匹配集" | 一个 HTB class `1:N` |
| ACL `match ... class X` | ✅ classify table + session | 同左（VPP 侧仍做） |
| ACL `set dscp N` | ⚠️ 只能经 policer 的 `MARK_AND_TRANSMIT`（CLI 坏，API 可） | 同左 |
| policer `cir/eir` | ✅ `policer_add_del`（kbps/pps，1r2c/2r3c） | 同左（更便宜，不必进内核） |
| `bandwidth-percent` | ❌ 无调度器 | ✅ `htb rate = pct × shaper` |
| `buffer-percent` | ❌ | ✅ `burst/cburst` + 叶 qdisc 的 `limit` |
| `scheduling wrr` | ❌ | ✅ HTB class（DRR 式借带宽） |
| `scheduling llq` | ❌ | ✅ `htb prio 0` + `ceil` 夹住 |
| `drops red-drop` | ❌（policer 的丢不是 AQM） | ✅ 叶 qdisc `fq_codel` / `red` |
| `drops tail-drop` | ❌ | ✅ `pfifo` |
| `qos-map` / `interface X qos-map` | ❌ | ✅ 一棵 tc 树 = 一张 map |
| Rewrite output（外层 DSCP） | ⚠️ `ip4-qos-mark` + egress map，或 policer 标记 | 同左；tc 按外层 `tos` 分 class |

**一句话**：Cisco 模型里"分类/标记/限速"这半边今天就能落地且是对的；
"队列调度"这半边在这台盒子上**要么借内核，要么不做**。

---

## 3. 三个形状

| | C1 | C2 | C3 |
| --- | --- | --- | --- |
| 名字 | VPP 原生：分类 + 标记 + 限速 | VPP 标记 + 主机 tc 调度 | 只做标记，不做限速 |
| 兑现的 Cisco 字段 | policer、`set dscp`、ACL 分类 | 全部（含 bandwidth/buffer/wrr/llq/red） | 只有 `set dscp` |
| 队列 | 无 | 有（HTB，7~8 个 class） | 无 |
| 数据面归属 | 全在 VPP | 出口一半进主机内核 | 全在 VPP |
| 风险 | 低（不动接口、不动内核） | **高**：要重建 WAN 口、管理面与限速器同命运、pps 未测 | 最低 |
| 工作量 | 中（4 条消息 + 对账 + 校验） | 大（C1 + 接口重建 + tc 生成 + 兜底） | 小 |
| 能不能说"带宽保证" | **不能** | 能（但在内核里） | 不能 |

C3 单列是因为它也是一种诚实的产品：只做 DSCP 标记（让上游/运营商去排队），
CPE 自己不做任何限速。很多 SD-WAN 分支站点确实只需要这个。

---

## 4. 推荐 & 要 owner 拍的 6 条

**推荐 C1**，理由三条：
1. 它是**唯一不改变数据面归属**的形状。C2 会让"出口包过不过内核"变成一个部署参数，
   而我们现在所有排障假设（`vppctl show interface` 的计数就是真值）都建立在"不经过"上。
2. C1 的风险可控且可回退：policer 是 VPP 内存状态，跟 NAT 一样按 dump 对账，删行就退。
3. C1 兑现的是客户真会买的那部分：**"这条专线限速 20M"** 和 **"语音打 DSCP 46"**。
   至于"拥塞时语音先走"，C1 给不了，就不该在界面上出现。

**要拍的 6 条：**

1. **产品要不要承诺"带宽保证"？** 要 → 必须 C2（并接受它的三笔账）；不要 → C1 收口，
   界面上不出现 `bandwidth-percent`/`queue`/`scheduling` 任何字段。
2. **Cisco 的队列字段怎么处理？** 建议**硬错误**（发布校验直接挡），不是 warning、
   不是静默忽略。理由：`bandwidth-percent 40` 被忽略 = 客户以为有保证、实际没有，
   这是这一类里最贵的一种谎。备选：接受但要求 `on:false`。
3. **C1 的匹配维度取哪些？** 建议 `src/dst` 前缀 + `proto` + `src/dst` 端口区间 +
   入向 `dscp`，与现有 ACL 模型（`vppapi.ACLRule`）同构，不发明新匹配轴。
4. **限速的方向与挂点**：入向（客户口 in）是"省上游"、出向（客户口 out 或 WAN 口 out）
   是"控用量"。建议**两个方向都支持，按口+方向成行**，与 Cisco 的 `interface ... access-list ... in/out` 对齐。
5. **超速动作**取 `drop` 还是 `mark-and-transmit <低 DSCP>`？Cisco 两种都有。
   建议默认 `drop`（可预期），`mark` 作为显式选项。
6. **C2 是否现在就做前置测量**（§6.4 三项）？不做的话，C2 的任何承诺都是纸面的。

---

## 5. C1 落地设计（若拍）

### 5.1 配置模型（`cmd/sr-ctrl/config_qos.go`，新）

```go
// ConfigQoS 一行 = 一个口 + 一个方向 + 一组类。
// 与 NAT 行同一纪律：行挂在草稿里的口（VPP 名）上，地址/掩码在发布时冻进 bundle。
type ConfigQoS struct {
    On      bool             `json:"on"`
    VPN     uint32           `json:"vpn"`
    VPP     string           `json:"vpp"`   // 应用口
    Dir     string           `json:"dir"`   // "in" | "out"
    Classes []ConfigQoSClass `json:"classes"`
    // Default 是没命中任何类的流量。DSCP=0 且 RateKbps=0 = 不动作（透传）。
    DefaultDSCP     uint8  `json:"default_dscp"`
    DefaultRateKbps uint32 `json:"default_rate_kbps"`
}

type ConfigQoSClass struct {
    Name       string   `json:"name"`
    Match      QoSMatch `json:"match"`
    DSCP       uint8    `json:"dscp"`        // 0..63，命中后标记
    RateKbps   uint32   `json:"rate_kbps"`   // 0 = 不限速
    BurstBytes uint32   `json:"burst_bytes"` // 0 = 由盒子按 rate 推
}

type QoSMatch struct {           // 与 vppapi.ACLRule 同构，不发明新轴
    Src     string    `json:"src"`     // IPv4 前缀
    Dst     string    `json:"dst"`
    Proto   uint8     `json:"proto"`
    SrcPort [2]uint16 `json:"src_port"`
    DstPort [2]uint16 `json:"dst_port"`
    DSCP    *uint8    `json:"dscp"`    // 可选：匹配入向 DSCP
}
```

存 `DeviceConfig.Services` 旁边的 `QoS`（**不**在 `BasicConfig` 里，理由与 NAT/DHCP 相同：
基础配置页面 PUT 整个 `BasicConfig`，放在里面的行会被每次保存抹掉）。

### 5.2 bundle 第 9 段

`Artifacts.QoS string` + `qosArtifact{ Rows []qosRow }`，渲染时把口名/VRF/前缀一起解出来。
与第 8 段同样的纪律：**行清空也要带空数组**，否则"删掉最后一行"到不了盒子。

### 5.3 apply 路径（`internal/apply/qos.go`，新；形状照抄 `nat.go`）

顺序（每条都有理由，反了就是事故）：

1. `classify_add_del_table` 建/换表（mask 按 §5.1 的匹配轴拼）；返回 `table_index`。
2. 逐类 `policer_add_del`（`is_add=1`，`rate_type=KBPS`，`conform_action={MARK_AND_TRANSMIT,dscp}`，
   超速按 §4 第 5 条）。**先建 policer 再建 session**：session 的 `hit_next_index` 要指向它。
   —— 幂等：同名 policer 已存在时 `policer_add_del` 返回 `VALUE_EXIST`(-81)，按"已配置"吞掉
   （与 `nat.go` 的 `natAlready` 同一形状）。
3. `classify_add_del_session` 每类一条，`hit_next_index = policer_index`，`action = NONE`。
4. `policer_classify_set_interface`（`is_add=1`，ip4 表，方向按行）—— **最后一步**：
   binding 一挂上，这个口的包就开始被丢，前面三步没齐就挂等于自己制造黑洞
   （与 NAT 的"interface features last"是同一类不变量）。
5. `verifyQoS`：`policer_dump` 回读每个 policer 的 `cir`/动作；`policer_classify_dump` 回读
   binding。**读不回来就报错**，不假设成功。
6. 退场反序：先解 binding，再删 session，再删 policer，最后删表。全程容忍 -81/-169 两个
   良性 retval（`nat.go` 已建立的模式）。
7. 开机重放：`applyServices` 那三处调用点同样加一段，`Boot` 传 `Installed{}`
   —— VPP 刚重启，policer 表是空的，没有"上一版"可依。

### 5.4 保存期校验（`servicesChecks` 同级）

- 口必须在基础配置里；`VPN` 与口所在 VPN 一致（照抄 NAT 的两条）。
- 同一 `(口, 方向)` 只允许一行。
- `dscp ≤ 63`；`rate_kbps` 落在 `[8, 10_000_000]`（低于 8k 的桶在 VPP 里翻不出有效令牌数）。
- `burst_bytes` 若给，必须 `≥ rate_kbps/8`（一个毫秒的量），否则桶比包还小、全丢。
- 匹配前缀必须已归一（`.Masked()`），端口区间 `first ≤ last`，`proto=0` 时不许带端口
  —— 直接复用 `ACLRule.validate()` 的那套判据，别写第二份。
- **Cisco 兼容字段（`queue`/`scheduling`/`bandwidth_percent`/`buffer_percent`）：
  出现在草稿里就 `error`**，文案说清"这台盒子没有队列调度，这一行不会被兑现"。

### 5.5 与 NAT 抢 `ip4-output` 弧（必须写进代码注释）

`nat44-ei` 的 output-interface 形状和 `ip4-qos-mark` 都在 `ip4-output` 弧上。
#302 已经量过一次"往这条弧上加特性"的代价（形状 D 的 1603 次 `out of ports`）。
所以：**C1 不启用 `ip4-qos-mark`**（标记走 policer 的 `MARK_AND_TRANSMIT`，不新增弧上特性），
把这条风险整条绕开。若将来要用 egress map 改写外层 DSCP，必须先做一次 §2.2 那种 A/B，
量 `ip4-output` 弧的顺序与 `out of ports` 计数，不能直接上。

### 5.6 落地前必须打通的三个点

1. **`policer_classify_set_interface` 的 CRC**（§2.5 唯一的 ❓）。三条路：
   (a) 从 v26.06 源码树取同名 `.api` 装进 `/usr/share/vpp/api/plugins/`；
   (b) 读 `vpp_papi` 的 JSON（若机箱有 python 绑定）；
   (c) 按 VPP 的 CRC 算法从 `.api` 文本算。**没取到之前不写代码。**
2. **`classify_add_del_session{action=NONE, hit_next_index=<policer>}` 是否真的把包交给
   policer** —— 这是 C1 的核心假设，必须先用 `vppctl` 之外的二进制 API 打一枪验证，
   判据是 `show policer` 的 `conform/exceed` 计数在打流后动。
3. **一个口上同时挂 NAT 与 policer 时的先后**：两者都是 `ip4-output`/`ip4-input` 消费者，
   必须量一遍"先 NAT 后 policer"和"先 policer 后 NAT"两种顺序下的转发与会话建立。

### 5.7 判据（e2e）

- 单测：`fakeQoS` 录调用序列，断言 **policer 先于 session、session 先于 binding**；
  幂等第二遍零调用；删行后反序退场；半行（有类无 policer）不下发只出 note。
- e2e（lab）：CPE4 一个客户口限速 1 Mbit → `iperf`/`ping -s` 打 5 Mbit，判据
  **吞吐落在 0.9~1.1 Mbit**、`show policer` 的 `exceed` 计数在涨、客户侧 ping 不中断。
- e2e（标记）：客户打 `dscp 0` 的包经 box 后，WAN 口抓包看到外层/内层 DSCP 变成配置值。
- e2e（退场）：删行后 `show policer` 为空、`policer_classify_dump` 无 binding、吞吐恢复。

---

## 6. C2 落地设计（若拍"要带宽保证"）

> **§9 已修正本节的前提**：C2 只对**物理口出口**成立，隧道流量不经内核；且
> "所有隧道同一个五元组"是错的（外层 `udp sport` 逐隧道不同，见 §9.2）。本节保留原文
> 作为物理口方案的记录，隧道场景请直接看 §9。

### 6.1 前置：接口必须重建

`qdisc-bypass-disable` 只能在 `create host-interface` 时给（§2.2），而产品路径里
**唯一建口的地方**是 `internal/apply/apply.go:998` 的 `ensureHostIface`
（它只在该口不存在时才建，所以现网盒子要生效必须**删了重建**）。

要动的一行：`create host-interface name %s hw-addr %s qdisc-bypass-disable`。
代价说清楚：删/建 WAN 口 = WireGuard 隧道断、BFD 判 down、路由收敛一轮（实测那一段
是 5~6s 级，见 `queue-plan-next.md` §三 的 refill 数字）。**这是每个站点一次性的人工窗口。**

### 6.2 分类链：VPP 标 → 内核排

1. VPP 侧保持 C1：classify 命中 → policer `MARK_AND_TRANSMIT`（这回 DSCP 是给内核看的）。
2. WireGuard 封装后外层是 UDP/51820，**所有隧道同一个五元组**，只有 DSCP 能区分 ——
   ~~所以外层 DSCP 必须由 VPP 在封装后写~~ **（此句已被 §9.2 推翻：外层 `udp sport`
   逐隧道不同，隧道可直判别；且隧道流量不经内核，本节的 tc 落点对隧道无效）**
   这一步正是 Cisco 的第 6 段，也正好是 §5.5 那条"新增 ip4-output 弧特性"的风险点。
3. 主机侧 `tc filter ... u32 match ip tos <dscp> 0xfc flowid 1:<class>`，把包分到 HTB class。
4. HTB 树 = Cisco 的 qos-map：`bandwidth-percent` → `rate`，`llq` → `prio 0` + `ceil`，
   `buffer-percent` → `burst/cburst`，`red-drop` → 叶 qdisc `fq_codel`/`red`。

### 6.3 管理面风险与兜底（必须先写进设计）

WAN 口的 qdisc 覆盖**所有**从这台盒子出去的包，**包括 ssh 的回包**（管理地址就在 WAN 口上，
#302 那条 identity mapping 就是为它存在的）。所以：

- 默认 class 必须给足（不低于签约速率的 100%），tc 树建错的后果是**盒子失联** —— 
  这与 NAT 那次"pool 设成外口地址 → 管理面全丢"是同一族事故。
- 控制面（BFD/WireGuard/ssh）单独一个 `prio 0` class，`rate` 不小于链路实测能力的 10%
  且 `ceil = rate`（不吃别人类，但保证自己不排队）。
- 生成 tc 树时**先建树后改默认 class**，任何一步失败回滚到"无 qdisc"，不允许半边树上线。
- 兜底：一个 30s 的看门狗 —— tc 树生效后 ssh/BFD 未恢复就 `tc qdisc del dev <wan> root`。

### 6.4 C2 必须先量、现在还没量的三项

1. **pps/吞吐代价**：af_packet 已经是慢路径，再串一个 qdisc 锁，代价多少？量法：
   同一台盒、同一 `iperf` 流，`tc qdisc del`（旁路）vs `tc qdisc add ... fq_codel`（进 qdisc）
   两臂的 pps 与 CPU。
2. **单 TX 队列下 HTB 的排队行为**：一个 qdisc 一棵树能不能撑起"7 个类"的隔离，
   还是会被单队列的 `qdisc_run` 串行化到互相影响。
3. **外层 DSCP 改写对 WireGuard 的影响**：DSCP 不进 WG 的握手/密钥，但要确认
   `qos mark` 在封装**之后**生效（顺序错了就是给内层包标记，内核看不见）。

---

## 7. 本轮取证清单（CPE4，2026-11-12）

### 7.1 命令（可直接重放）

```
vppctl show version
vppctl show plugins | grep -iE 'dpdk|hqos|qos'
vppctl show hardware-interfaces            # af_packet, 1 个 shared TX 队列
vppctl show features | grep -iE 'qos|policer'
vppctl show api message-table | grep -i policer
python3 -c '...'  # 读 /usr/share/vpp/api/plugins/policer.api.json 与 core/classify.api.json
```

### 7.2 qdisc A/B 与 HTB 限速

```bash
ip link add srq0 type veth peer name srq1 ; ip link add srp0 type veth peer name srp1
ip link set srq0 up; ip link set srq1 up; ip link set srp0 up; ip link set srp1 up
vppctl create host-interface name srq0                          # 默认（旁路）
vppctl create host-interface name srp0 qdisc-bypass-disable     # 进 qdisc
vppctl set interface ip address host-srq0 10.99.0.1/24
vppctl set interface ip address host-srp0 10.98.0.1/24
ip addr add 10.99.0.2/24 dev srq1 ; ip addr add 10.98.0.2/24 dev srp1
tc qdisc add dev srq0 root handle 1: htb default 20 ; tc class add dev srq0 parent 1: classid 1:20 htb rate 100mbit
tc qdisc add dev srp0 root handle 1: htb default 20 ; tc class add dev srp0 parent 1: classid 1:20 htb rate 100mbit
vppctl ping 10.99.0.2 repeat 100 interval 0.005     # → srq0: Sent 0 bytes 0 pkt
vppctl ping 10.98.0.2 repeat 100 interval 0.005     # → srp0: Sent 11358 bytes 105 pkt
tc class change dev srp0 parent 1: classid 1:20 htb rate 100mbit
vppctl ping 10.98.0.2 repeat 500 interval 0.001     # → 499/500 收, 514ms
tc class change dev srp0 parent 1: classid 1:20 htb rate 100kbit
vppctl ping 10.98.0.2 repeat 500 interval 0.001     # → 60/500 收 (88% 丢), backlog 201p
```

**为什么用 veth 而不是生产 WAN 口**：WAN qdisc 会同时限制 ssh 回包（§6.3），
在没写兜底之前拿生产口做实验就是拿盒子连通性做赌注。af_packet 的旁路开关在
`af_packet` 插件里、不在设备里，所以 veth 上的结论对 `enp0s9` 同样成立；
但**"结论同样成立"不等于"数字同样"**，C2 的 pps 代价必须在真 WAN 口上量（§6.4）。

### 7.3 policer 契约（机箱上的 api json，逐字段）

`policer_add_del` crc `0x2b31dd38`：`is_add bool / name[64] / cir u32 / eir u32 / cb u64 /
eb u64 / rate_type u8 / round_type u8 / type u8 / color_aware bool /
conform_action{u8,u8} / exceed_action{u8,u8} / violate_action{u8,u8}`。

枚举：`SSE2_QOS_RATE_API_KBPS=0, PPS=1`；`ACTION_API_DROP=0, TRANSMIT=1, MARK_AND_TRANSMIT=2`；
`ROUND_API_TO_CLOSEST=0, UP=1, DOWN=2`。

`policer_details` crc `0x72d0e248` 带 `single_rate / scale / cir_tokens_per_period /
current_limit / current_bucket / last_update_time` —— 足够做 `verifyQoS` 的回读。

`classify_add_del_table` crc `0x6849e39e`（`table_index` 默认 `~0` = 新建）；
`classify_add_del_session` crc `0xf20879f0`（`hit_next_index`、`action`、`match_len/match`）。

### 7.4 盒面还原（已做，核对过）

```
vppctl delete host-interface name srq0 ; vppctl delete host-interface name srp0
ip link del srq0 ; ip link del srp0
vppctl policer del name p1 ; del a2 ; del a5      # 三个测试对象
vppctl delete loopback interface intfc loop0      # qos mark 语法探针用的
```

核对结果：`show interface` 回到基线（`host-enp0s8 host-enp0s9 local0 tap4096 tap4097
wg1010 wg1011 wg1014`），`show policer` 为空，无残留 veth/qdisc，ssh 全程在线。

---

## 9. 修正：QoS 的对象是隧道，不是网卡（覆盖 §3/§6 的边界）

§3 的三选一和 §6 的 tc 设计，都把"出口"当成**物理 WAN 口**来想。本轮 owner 指出真正的
对象是 **WireGuard 隧道**。这一条把前面的结论切掉一半，以下全部是 2026-11-12 18:4x 在
CPE4 上补的实测，不是推断（推断单独标出）。

### 9.1 隧道流量压根不经主机内核

WG 在 VPP 里加密，外层包从 `af_packet` 直接出去，**不经过内核网络栈**，所以
`qdisc-bypass` 这个开关对隧道流量毫无关系（§2.2 的 A/B 实验测的是物理口）。
结论：**§6 的 tc 方案对隧道只能在 WAN 口这一层落，且必须靠外层头分流**，
它拿不到内层 DSCP（已被加密），也拿不到内层五元组。

### 9.2 外层头里有逐隧道的判别符（实测，两条独立证据）

FIB 的邻接 rewrite 字节（`show ip fib | grep wg10`），逐字段拆：

| 隧道 | 外层 src | 外层 dst | 外层 sport | 外层 dport | 本端 overlay |
| --- | --- | --- | --- | --- | --- |
| wg1010 | 192.168.1.104 | 192.168.1.101 | **20000** | 20001 | 10.250.0.3/32 |
| wg1011 | 192.168.1.104 | 192.168.1.103 | **20001** | 20001 | 10.250.0.5/32 |
| wg1014 | 192.168.1.104 | 192.168.1.102 | **20002** | 20001 | 10.250.0.9/32 |

`tcpdump -i enp0s9 udp` 的入向 6 秒采样印证同一张表：
`.101.20001 > .104.20000`（11 包）、`.102.20001 > .104.20002`（10）、`.103.20001 > .104.20001`（9）。

→ **出向按外层 `udp sport` 就能把包归到某条隧道**（每条隧道一个本端端口，`show wireguard
interface` 里就是 port:20000/20001/20002），不需要解内层。这也说明 §6.2 里
"所有隧道同一个五元组、只有 DSCP 能区分"**是错的**，那句要作废。

### 9.3 三条路（取代 §3 的三选一）

| | A：WAN 口 + 主机 tc | B：VPP 内 policer | C：VPP 内自研节点 |
| --- | --- | --- | --- |
| 新代码 | 0 行 C（生成 tc 规则） | 0 行 C（二进制 API） | ~600–900 行 C |
| 排队/优先级/缓冲 | 有（HTB 7~8 class） | 无，只有丢弃 | 有（想几队列就几队列） |
| 能看见内层 DSCP | **不能**（已加密） | 能（挂内层弧） | 能（加密前） |
| 数据面归属 | 出口一半在内核 | 全在 VPP | 全在 VPP |
| 代价/风险 | 重建 WAN 口、管理面与限速器同命运、单 TX 队列隔离性未测 | 无缓冲=TCP 全局同步，只有"上限" | 自己管 buffer 生命周期与定时器精度 |
| 能不能说"带宽保证" | 能（但在内核里） | 不能 | 能 |

A 的判别方式就是 9.2 那张表（`tc filter ... match ip sport 20000 → 1:10`），
这让 A 从"按类排队"升级成"**按隧道排队**"，是 §6 原来没写的一种能力。

### 9.4 加密前的挂点：不是"挂不上"，是"不能挂在后面"

仓库里 `sr_wg4_encap.c` 的实测结论是"挂在 `wg4-output-tun` **之后**是结构性死点"
（`show node` 里 0 vectors）。但那说的是**位置**，不是这条弧不通：
`show interface features wg1010` 里 `ip4-output:` 下就是 `wg4-output-tun` 自己
——它本身就是这条 feature 弧上的一个节点。

所以一个声明 `.runs_before = VNET_FEATURES ("wg4-output-tun")` 的节点排在它**前面**，
就能在加密前拿到**明文内层包**，内层 DSCP、五元组、每隧道计数全都在。

**已实测通过（2026-11-12 19:3x，CPE4）**，不再是推断：

- `strings`/二进制 grep：`wireguard_plugin.so` 里确有 `ip4-output` 与 `wg4-output-tun`
  字面量（3 处），说明它确实是这条弧上的 feature 节点；
- 装上新插件后 `show features` 的 `ip4-output` 弧顺序：
  `sr-wg4-qos → sr-wg4-encap → ip4-sv-reassembly-output-feature → … → wg4-output-tun → …`
  —— 我们的节点排在**第一位**，`wg4-output-tun` 在第 11 位；
- 逐口启用后 `show interface features wg1010` 显示 `ip4-output: sr-wg4-qos, wg4-output-tun`
  —— 链是 `sr-wg4-qos → wg4-output-tun`，正是"明文 → 加密"的顺序；
- ~~**真实客户流量被计到**：挂上三条隧道后累计 1159 包（wg1010 273 / wg1011 51 /
  wg1014 38，随后继续增长到千级），全部落 dscp 0 桶~~ —— **这一条 2026-11-12 晚
  被推翻**：那批计数是本机自发的 probe/keepalive，**转发流量根本不走这条弧**。
  详见 §10。第一阶段的实验只证明了"节点能挂上弧"，没有证明"能看到客户流量"。

所以 4 队列整形器可以长在 `sr-wg4-qos` 节点上（分类发生在加密前、内层头可读），
但**前提是这条弧真的承载转发流量 —— 实测它不承载**。换挂点见 §10.3。

**注意**：`vppctl ping` 打到 overlay 地址走的是 main FIB（overlay 路由在
table 250），不进隧道，所以别用它做注入测试。

### 9.5 写 C 的真实代价（不美化）

- 单线程：`show threads` 只有 `vpp_main`，无 worker ⇒ 不需要跨线程 handoff，代价转移成
  **定时器精度**——drain 的唤醒间隔直接决定允许的突发量（1ms × 线速）。
- **自己持 buffer 就必须自己设上限**：队列深度要有硬 cap + 超限 drop-tail，否则是内存
  被吃光，不是丢包。
- **控制面必须默认放行或进 LLQ**：BFD/keepalive/ssh 和客户流量同一个出口，整形器写错
  的顺序是先断自己（NAT 那轮 pool 设成外口地址就是同一族事故）。
- **顺序**：整形在加密前、`sr-wg4-encap` 之前；两者相邻，`runs_before` 要一次写清并
  在 `show features` 里核对顺序。
- **构建链**：盒上没有 VPP dev headers（`/usr/include/vpp*` 不存在），只能在构建环境编
  `.so`，版本必须严格对齐 `26.06-release`；现成的 `sr_wg_plugin.so` 就是这个模式（87 个
  插件里第 87 个，`sr_wg_plugin.so 1.0.0`），不是新工程。

### 9.6 这节要拍的

1. A / B / C 选哪条？——判断标准只有一句：**要不要承诺"拥塞时语音先走"**。
   不要 ⇒ B 就够（每条隧道一个上限）；要但没有内核洁癖 ⇒ A；要且必须留在 VPP ⇒ C。
2. 若走 C：先做"每隧道单队列整形"（shaper 形状，约 C 的 1/3 工作量），还是直接
   8 队列 + WRR/LLQ + RED？
3. ~~是否接受先花半天验 §9.4 的挂点~~ —— **已验且通过**（见 §9.4 末段）。
   owner 已拍 **C + 8 队列**，挂点这一关不再是风险项。

> 落地状态（2026-11-12 19:3x）：`vpp-plugin/sr_qos.{c,h}` 已进 `sr_wg_plugin`（节点
> `sr-wg4-qos`、CLI `set sr-wg qos-hook enable|disable <if>` / `show sr-wg qos-hook`），
> 已装到 CPE4 `/usr/lib/x86_64-linux-gnu/vpp_plugins/`（旧版留 `.bak-qos-20261112`）。
> 当前是**第一阶段**：按 (隧道, DSCP) 计数后原样透传，挂点默认关闭。
> 第二阶段在同一节点上换入队 + 定时 drain + token bucket，不重写弧与 CLI。

---

## 10. 挂点错了：`wg` 的 `ip4-output` 弧不承载转发流量（2026-11-12 晚，实测）

§9.4 的结论"节点能挂上 `ip4-output` 弧、能看到明文"成立，但**"能看到客户流量"这一步被自己
的下一轮实验推翻了**。这一节只写实测，不写希望。

### 10.1 三个实测事实

**事实一：转发流量根本不走 `wg` 口的 `ip4-output` 弧。**

`cust4` 去 `172.16.12.1` 的路由是 `ipv4 [features] via 10.250.0.8 wg1014`，挂上 `wg1014`
（`rate 2000`）后：**接口计数在涨（~110 KB/s），我们的节点桶不动**。为排除"包太小/太散"，
做了只改包长的对照 —— 同样 50 包，只改 payload：

| 动作 | `sr-wg4-qos` 累计（pkt / B） | `q2 DEF` 累计（B） |
| --- | --- | --- |
| 基线 | 1491 / 165912 | — |
| 再打 50 × 200 B | 1539 / 171264（+48 / +5352） | 未动 |
| 再打 50 × 1000 B | 1580 / 175824（+41 / +4560） | 未动 |

`+5352 B / 48 包 ≈ 111 B/包`，和 200 B、1000 B 的注入量都对不上 —— 那是背景探针，
不是我们的注入。**结论：包没经过这个节点。**

`show node wg4-output-tun` 的 previous nodes 给了结构解释：

```
sr-wg4-qos (1), sr-wg4-mpls-out (6), linux-cp-xc-ip4 (254),
ip4-dvr-dpo (466), ip4-midchain (735), ip4-mcast-midchain (736),
ip4-rewrite-mcast (737), ip4-rewrite (739)
```

转发流量是从 **`ip4-midchain` / `ip4-rewrite`** 直接进 `wg4-output-tun` 的，只有本机自发流量
（`linux-cp-xc-ip4`、我们的探针）才走 `ip4-output` 弧。§9.4 里"1159 包真实客户流量"就是这些
自发包。**FIB 里的 `[features]` 标记不保证输出弧被遍历** —— midchain 邻接的下一跳是
`wg4-output-tun` 本身，不经过弧入口节点。

**事实二：把挂点挪到 underlay 口，弧是过了，但内层 DSCP 没了。**

挂 `host-enp0s9`（`rate 0`，只计数）：`total=826`，`dscp 0: 809 pkt / 240941 B` ——
**确实看得到隧道包**。但注入 50 × 1000 B 让 `dscp 0` 的字节涨了 **173582 B**（≈ 3.5 倍），
说明这条弧上同一条流被多次经过（加密后 + 回程 / 多次重写），**不能直接当限速点用**。

**事实三：外层头不带内层 DSCP。**

注入 50 × DSCP 46 之后：`dscp 0` 从 7315 涨到 7750（+435），**`dscp 46` 桶一次都没出现**。
加密后外层 TOS 是 0，内层的分类信息在 underlay 那一层**不存在**。

→ 一句话：**underlay 只能按"哪条隧道"分流（外层 `udp sport` 20000/20001/20002），
拿不到"哪一类业务"。** 而 owner 要的 4 队列 LLQ 的前提正是"哪一类业务"。

### 10.2 为什么绕开（结构，非推断）

VPP IPv4 只有这几条可以挂钩的弧（`show features` 实测）：

```
ip4-unicast  → 终端节点 ip4-lookup      （入口，按**入接口**生效，包是明文）
ip4-output   → 终端节点 interface-output（出口，按**出接口**生效）
ip4-local / ip4-multicast / ip4-drop / ip4-punt / interface-output / device-input / …
```

**没有 `ip4-midchain` 弧，也没有 post-rewrite 弧。** 转发进隧道走的是
`ip4-lookup → ip4-rewrite → ip4-midchain → wg4-output-tun`，这条链上没有任何 feature
arc 的入口节点，所以"挂在出口弧上"对转发流量天然无效。

### 10.3 候选挂点（按可行性排，附证据强度）

| | O3：入口弧 `ip4-unicast` | O4：underlay `ip4-output` | O1：midchain 路径改造 |
| --- | --- | --- | --- |
| 挂在哪 | 客户口/隧道口的**入方向** | WAN 口出方向 | 改 midchain 邻接的 next 指向 |
| 能看到内层 DSCP | **能**（明文） | **不能**（实测事实三） | 能 |
| 能拿到"出哪条隧道" | 要自己查一次 FIB（邻接→`sw_if_index`） | 直接（外层 `udp sport`） | 直接（就是那条邻接） |
| 每包代价 | +1 次 FIB 查（命中 mtrie，~几十 ns） | 0 | 0 |
| 4 队列 / LLQ | **能** | 只能单队列（无分类） | 能 |
| 风险 | 分类口径必须和 `ip4-lookup` 一致（同 fib、同 VRF） | 计数被多次经过污染（实测 3.5×） | 动 VPP 邻接内部结构，升级即碎 |
| 依赖 | 全部是公开头文件（`ip4_fib_table_get_index_for_sw_if_index`、`ip4_fib_forwarding_lookup`、`adj_get`） | 无 | 无公开 API |

**补记（2026-11-13，owner 质疑"为什么不给 wg 接口做"后复查）：当时漏列了 O5。**

| | O5：做进 `sr-wg4-mpls-out` |
|---|---|
| 挂在哪 | MPLS over WG 出口链（节点已存在，`sr_wg4_mpls_out.c`，邻接 next 已指向它） |
| 内层 DSCP | **能读**（标签栈压在内层 IP 之上，`current+4` 起就是内层 IP 头） |
| 出哪条隧道 | **免费**（`adj->rewrite_header.sw_if_index` 就是隧道口，无需任何 FIB 解析） |
| 覆盖范围 | **只有带标签进隧道的流量**（CPE4 客户前缀现全为 mpls-label[1000]，实际全覆盖）；纯 IPv4 midchain 路径的流量绕过 |
| 队列/整形 | 同 4 队列模型；出队重注入本节点即可 |

O3 vs O5 的本质差别：O3 全覆盖但每包付一次（已缓存的）隧道判定；O5 零额外查表
但只认 MPLS 路径，无标签路由会静默 bypass。若产品确定隧道流量恒为 MPLS over WG，
O5 更简单（可整体删除 FIB 解析模块）；若要求"所有进隧道流量都管"，维持 O3。
**当前部署仍是 O3，未切换。**

**选 O3。** 理由不是它最省事（它多一次查表），而是它是**唯一同时满足"内层 DSCP"和
"不动 VPP 邻接内部结构"**的位置；而且出队回注天然干净 —— 入口弧的终端就是 `ip4-lookup`，
drain 时把包丢回 `ip4-lookup` 就等于"重新走一遍原来的路"，不需要碰加密节点。

O3 的确切口径（这一条必须写死，否则又是一次 §9.4）：

- 入口弧在**入接口**上生效 ⇒ 要 enable 的是 `cust4`（客户）与 `wg101X`（隧道口，用于
  hub 中转：解密后的明文再从另一条隧道出去），**不是** enable 隧道本身；
- 隧道判别：`fib = ip4_fib_table_get_index_for_sw_if_index (rx_sw_if_index)` →
  `adj = adj_get (ip4_fib_forwarding_lookup (fib, &ip->dst_address))` →
  `tun_sw_if = adj->rewrite_header.sw_if_index`；查不到已配置隧道就**原样透传**；
- 节点排在入口弧**末尾**（不加 `runs_before`），保证 NAT/ACL/ABF 都已经跑完，
  我们的 FIB 查询和随后的 `ip4-lookup` 看到同一个结果；顺序要在 `show features` 里核。

### 10.4 落地改动（相对现有 `sr_qos.c`，已实现，**禁止部署**，见 §10.6）

不需要重写调度器 —— 4 队列、token bucket、LLQ 上限、DRR、tail-drop、计数器全部不动。
变的只有三处（均已写进 `vpp-plugin/sr_qos.{c,h}`，容器内编译通过、VPP 能加载）：

1. `VNET_FEATURE_INIT` 的 `arc_name` 从 `ip4-output` 换成 `ip4-unicast`；
2. 入队时的隧道判别换成自查 FIB：`ip4_fib_lookup` → 取 load-balance 第一个桶的 DPO →
   **先验 `dpoi_type == DPO_ADJACENCY`** 再 `adj_get`，只认 `IP_LOOKUP_NEXT_MIDCHAIN`
   （不验类型直接 `adj_get` 是拿别人的索引去邻接池取值，越界读）；
3. `drain_next` 变成 `ip4-lookup`（由弧的终端自动给出，仍走"相对 next → 绝对 next"
   解析，`show sr-wg qos` 打印出来核对）。

CLI 语义随之分开，避免"enable 的是谁"再被读错：

| 命令 | 作用对象 |
| --- | --- |
| `set sr-wg qos-hook enable\|disable <if>` | **入接口**（客户口 / 隧道口），决定"哪些口进来的包参与调度" |
| `set sr-wg qos rate <tunnel-if> <kbps>` | **隧道**，决定这条隧道的整形速率 |
| `set sr-wg qos llq <tunnel-if> percent <1-100>` | **隧道**，LLQ 上限 |

容器内已核过的事实（VPP 26.06，2026-11-12 晚）：

- 节点能加载、能挂上 `ip4-unicast` 弧，CLI 正常；
- **弧内落点**：无约束时落**第 1 位**；`runs_before ip4-lookup` 仍是第 1 位
  （约束满足即可，排序器不挑最末）；`runs_after ip4-not-enabled` 也只到第 3 位。
  现版本选择无约束（第 1 位）—— 出队是把包送回弧里的下一个 feature，urpf/ACL/NAT
  照样执行，被丢的包不会因经过我们就活下来；唯一会不一致的是 ABF（本盒未用），
  这一点已写在 `VNET_FEATURE_INIT` 上方的注释里；
- ipip 隧道邻接确实是 midchain（`show ip fib` flags:[fixup-ip4o4]），FIB→邻接→
  `sw_if_index` 这条判别链在头文件层面成立；
- 打流的端到端验证**没有做完**（容器里 packet-generator 的 data 模板解析不过，
  tap 需要 /dev/net/tun）—— 分类是否真的命中，还没有实测数字。

### 10.5 这一节要拍的

1. 接受 O3（入口弧 + 自查 FIB）吗？—— 不接受的话，剩下能选的是 O4 单队列（丢掉 4 队列
   这个需求），或者回到 §9.3 的 A/B 两条路。
2. 哪些口要 enable？建议 `host-enp0s8`（LAN 入）+ 三条 `wg101X`（hub 中转），
   `tap4096/4097` 是 LCP 的内核侧，不需要。

### 10.6 部署事故（2026-11-12 20:36–21:20）：O3 版本上盒即瘫，已回退，原因未明

按 §9.6 的既定流程把 O3 版部署到 CPE4（`/root/qos3-deploy.sh`，备份
`.bak-qos3-20261112`）。结果：

- 20:35:57 `systemctl restart vpp`，VPP 起来，underlay OK（20:35:59），CLI 正常
  （20:36:08 `show sr-wg qos-hook` 有日志）—— **钩子一个都没挂**；
- 之后数据面死亡：Mac 和 CPE1 对 `192.168.1.104` 的 ICMP 100% 丢，CPE1 抓包只见到
  别家发来的 WG keepalive、无任何应答；TCP 22 不通；sr-agent 连不上控制器
  （"tunnels 0 条……控制器不可达时开机就是这个形状"）；
- **整机重启也无效**（21:03、21:07 两次重启，underlay 每次 OK，但数据面同样死亡）
  —— 说明是盘上那个 `.so` 的问题，不是 VPP 一次性的状态；
- 恢复：把 CPE4 的盘挂到 CPE1（`VBoxManage storageattach CPE1 ... port 2`），
  `cp .bak-qos3-20261112 → sr_wg_plugin.so`，卸盘、开机，21:20 回到基线
  （5 口 + 3 隧道，LAN 侧 ping 通）。

**为什么可疑**：钩子从未启用，O3 版相对 20:12 那版（在盒上跑了 23 分钟、带钩子带流量、
一切正常）的唯一**加载期**差异就是把 feature 从 `ip4-output` 弧挪到 `ip4-unicast` 弧。
怀疑方向是"注册在 ip4-unicast 弧 × LCP（linux-cp-xc-ip4 也在同一条弧上）"的相互作用，
但**没有证据**，可能是完全别的原因（如进程节点/内存）。

**结论与纪律**：O3 版在能复现、能解释之前**不许再上任何一台真机**。下一步是在
特权容器里带 LCP 复现（`--device /dev/net/tun` + veth + `lcp create`，容器里
tap/LCP 才建得起来），定位后再谈部署。控制台口令已在本次恢复中重置为 vm123
（此前控制台登录失败的原因不明）。

---

## 11. O3 挂点落地：事故被推翻，隧道判别换到控制面（2026-11-12 深夜）

### 11.1 §10.6 的"上盒即瘫"不是 O3 造成的

两件事，都有实测：

1. **容器复现（6 案例）**：`test/lab/vpp-qos/lab-o3-repro.sh` 在特权容器里搭了
   与盒子同形的拓扑（`create tap` 建客户口、`lcp create ... host-if` 建管理面、
   管理面走 `linux-cp-xc-ip4`），6 个组合全部 PASS：①无插件无 LCP ②无插件有 LCP
   ③O3 插件无 LCP ④O3 插件有 LCP ⑤O3 插件有 LCP 且钩子启用（rate=0）⑥旧
   `ip4-output` 版。**"注册在 ip4-unicast 弧会把数据面搞死"不成立**。
   顺便证伪了当时的一条旁证：无插件基线也有 `null-node blackholed`，那个计数不是
   插件有罪的证据。
2. **真机二次部署**：今晚连续 4 次把 O3 版换到 CPE4（每次 `qos-guard` 看门狗
   守着，见 §11.5），4 次都恢复健康（`result.txt` 全 OK），5 口 + 3 隧道在、
   BGP/MPLS 转发在、ssh 全程不中断。

那 20:36 那次到底为什么瘫，**仍然没有根因**（盘上 .so 是变量，但同类 .so 现在
能跑）。诚实的说法是：**事故不可归因于 O3 的弧选择**；它可能是一次坏换件/坏盘写，
也可能是当时那版代码里别的东西。纪律改成"换件必须有看门狗 + 备份"，而不是
"O3 禁部署"。

### 11.2 隧道判别踩了三次坑，前两次都是"拿类型号当判据"

| 版本 | 判据 | 容器 | CPE4 | 为什么错 |
|---|---|---|---|---|
| v1 | `dpoi_type == DPO_ADJACENCY` | 失败 | 失败 | 漏掉 `DPO_ADJACENCY_MIDCHAIN`；且 CPE4 上同一个桶打出来 `type=30` |
| v2 | 加 `DPO_ADJACENCY_MIDCHAIN` / `dpoi_next_node == IP_LOOKUP_NEXT_MIDCHAIN` | 失败 | 失败 | **枚举数值随 build 变**：镜像里 `ADJ_MIDCHAIN=8 / NEXT_MIDCHAIN=6`，CPE4 上是 `type=30 next_node=14`（`raw=0x00000001000e001e`）。CPE4 的 vpp 是另一次编译（md5 `33b5611c…` vs 镜像 `32d4ef86…`，build host 也不同） |
| v3 | `adj_is_valid(dpoi_index)` + 邻接 rewrite 出接口在隧道表里 | PASS | 失败 | **MPLS over WG 的桶里根本不是邻接**：CPE4 的客户前缀转发链是 `load-balance → mpls-label[1000] → mpls via 10.250.0.8 wg1014 → stacked-on 192.168.1.102 host-enp0s9`，`dpoi_index=2` 只是**恰好命中**邻接 #2（enp0s8）。这条路在 MPLS 拓扑下原理上取不到隧道口 |
| v4 | 控制面：`fib_table_lookup` → `fib_entry_get_resolving_interface`，按 (fib,dst) 缓存 | PASS | PASS | — |

v2 的现场（CPE4，`show sr-wg qos resolve`，已固化进 CLI）：

```
bucket[0]: type=30 proto=0 next_node=14 (wg6-input-data-handoff) index=1 raw=0x00000001000e001e
  本插件编译期枚举（仅构建环境自测有效）: IP_LOOKUP_NEXT_MIDCHAIN=6 DPO_ADJACENCY=6 DPO_ADJACENCY_MIDCHAIN=8
=> 判定 why=2
```

`next_node=14` 在**这台**盒子上是 VPP wireguard 插件的 `wg6-input-data-handoff`
（`grep -a -o '[a-z0-9%-]*-input[a-z0-9%-]*' wireguard_plugin.so` 里有它）。
于是"按名字查节点索引"也不成立 —— 它压根不是 midchain 派发。

### 11.3 现在的判据（跨 build、跨 MPLS）

```
fib_table_lookup(fib_index, dst/32)          → fib_entry
fib_entry_get_resolving_interface(fib_entry) → 出口 sw_if_index
出口在我们的"配了速率的隧道表"里              → 排队；否则直通
```

CPE4 实测三种形状全对：

| 目的 | resolving_interface | 判定 |
|---|---|---|
| 172.16.12.1（客户，走隧道） | 7 `wg1014` | 排队 ✔ |
| 172.31.4.2（直连 LAN） | 1 `host-enp0s8` | 直通 ✔ |
| 8.8.8.8（默认路由 WAN） | 3 `host-enp0s9` | 直通 ✔ |

代价与对策：这两步是控制面量级（池查 + 路径解析，可能递归），**不能每包做**。
所以按 `(fib_index, dst)` 缓存进 `qm->rt_cache`（一次 `hash_get` 命中），process
节点每 2 秒整体失效一次 —— 路由改了最多 2 秒内跟上；这段时间里可能按老隧道排队，
**排队不改变转发结果**，最坏是速率算错 2 秒。

已知限制：ECMP 到多条隧道时取主路径，不按流哈希（与 §9 的"一前缀一隧道"一致）。

### 11.4 CPE4 验收数字（2026-11-12 23:20–23:45）

- **分类**：cust4（172.31.4.2）打 172.16.12.1，钩子挂在 `host-enp0s8`、
  `rate wg1014 1000`：`dscp 46: 30 pkt` → `q0 LLQ tx 30`；`dscp 0: 44 pkt` →
  `q2 DEF tx 30`；`未归隧道` 里没有这两条流（19 个未归是去 WAN 的其它流量，
  `last_dpo_type=12`，正常直通）。
- **整形**：从 cust4 灌 UDP（1400B × 数万包）到 172.16.12.1，
  `wg1014` 出口 `tx_bytes` 5 秒增量 664500 B ⇒ **1063 kbps**，配置 1000 kbps
  （差的那 6% 是外层加密头，shaper 量的是内层字节）；同时
  `q2 drop 113836 / backlog 20（峰值 128）` —— 尾丢真的在丢，队列真的在积压。
  置 `rate 0` 后立刻恢复直通、积压归零。
- **容器回归**：`test/lab/vpp-qos/lab-qos-tunnel.sh` PASS（`未归隧道=0`、
  q0=40、q2=40、出队目标 `ip4-lookup`）。

### 11.5 换件纪律（写进 handover §2.6）

管理面在 VPP 里 ⇒ `systemctl restart vpp` 会杀掉自己的 ssh。现在的流程：

1. `scp` 新 `.so` 到 `/root/*.so`（不动盘上那个）；
2. `systemd-run --unit=qosguard --collect --no-block /bin/bash /root/qos-guard.sh`
   —— 180 秒后 ping 三个锚点（192.168.1.1 / 172.31.4.2 / 10.250.0.1），全不通就
   把盘上 `.so` 换回最新的 `.bak-qos3-*`（= 已知可用的 `ip4-output` 版）、
   `reboot -f`、自己 disable；
3. `systemd-run --unit=qosdeploy --collect --no-block --setenv=SRC=... /bin/bash
   /root/qos4-deploy.sh` —— 备份、install、restart、等 CLI、快照证据到
   `/tmp/qos4-*.txt`；
4. ssh 断了也没关系，回来读 `/var/log/qos-deploy/{deploy.log,result.txt,after-*.txt}`。

### 11.6 本节遗留

- **返回方向 ping 100% 丢**（cust4 → 172.16.12.1 通到 cust12 的 `enp0s8`，回程包
  也到了 CPE4 的 `enp0s9:20002`，但没进 `lcp-x0`）。**与 QoS 无关**：钩子摘掉、
  `rate 0` 时同样丢。这是这台盒子今晚之前的既有状态（`wg4-input Peer error 1`、
  `mpls-output 90 encapsulated`），下次上盒单独查。
- 队列→DSCP 映射、bundle 段、控制台仍是纸面（§5.1–5.3）。
- 探针/看门狗脚本还是 `/root/*.sh`，没进仓库（下一步收进 `test/lab/vmlab/`）。

### 11.7 这一节要拍的

1. §10.6 的"O3 禁部署"**解除**（改成"换件必须带看门狗"）—— 是否同意？
2. 隧道判别用控制面 + 2 秒缓存，接受"路由切换后最多 2 秒按老速率排队"吗？
3. 返回方向丢包要不要现在查（不影响 QoS 验收，但会让客户面看起来是坏的）。

---

## 12. SR 头 flowID 与 MPLS TC 位评估（2026-11-13，纯调研，未改代码）

### 12.1 先说结论

| 载体 | 能否承载 QoS 类别 | 对现有 CPE4 发送端 QoS 的简化 | 兼容性代价 | 建议 |
|---|---|---|---|---|
| SR 头 `reserved[2]` 做 flowID | 能（16 bit，HMAC 保护） | **无** | 旧端 HMAC 校验失败 → 必须双端升级/版本协商 | v2 头格式再规划 |
| SR 头 `action` 高 3 bit | 能（3 bit） | **无** | **零**（数据路径没人读 action，且不在 HMAC 内） | 需要时随时可加 |
| MPLS TC/EXP（3 bit） | 能，但标签在 WG 加密**里面** | 无 | 要 per-packet 写 exp，得动 mpls DPO/自己的 mpls_out | 不做 |
| **外层 IP DSCP 复制** | 能（6 bit DSCP） | 无 | **零协议改动**（外层头是我们自己的 encap 可写） | 中转 QoS 的首选 |

核心判断：**flowID/TC 对"发送端 QoS"没有简化作用**。现在的热路径两件事——读内层 DSCP（入口弧上免费）＋判"从哪条隧道出去"（已缓存成一次 hash_get）。flowID 是封装时才写进 SR 头的，入口弧上 SR 头还不存在；而且 flowID 的语义是"业务类别"，不是"出隧道"，消不掉 FIB 解析那一步。真要消掉隧道判定，出路是把排队点挪到隧道身份已知的封装点，那是另一个设计变更，不是加字段。

### 12.2 现有 SR-WG 头格式（`sr_header.h`，唯一权威）

```
magic(1) ver_flags(1) hdr_len(1) seg_left(1) seg_count(1) action(1)
reserved[2] hmac_id(2) path_id(2) timestamp(4) segments[..] hmac(8)
```

- `reserved[2]`：16 bit 空位，**在 HMAC 覆盖内**（见 `sr_hmac.c` "显式跳过 magic/seg_left/action"）。
- `action`：1 字节，**HMAC 排除**；grep 全插件确认数据路径（`sr_wg4_input`/`sr_wg4_transit`）没有任何地方读它，只被 init 写成 FORWARD=0。
- `path_id`：已有字段，语义是"路径"不是"类别"——不要拿它当 flowID 用。

### 12.3 各载体详评

**SR 头 flowID（reserved[2]）**：中转节点唯一能读的载荷之外信息就是外层 IP/UDP＋SR 头（内层在 WG 加密里）。若产品路线要做"中转节点差分调度"，flowID 是正解之一（HMAC 保护防篡改、16 bit 还能兼做逐流统计）。但 reserved 进 HMAC ⇒ 新端写 flowID 后旧端校验必挂 ⇒ 必须版本协商（ver_flags 高 4 位就是为这个留的）。

**MPLS TC/EXP**：CPE4 FIB 里标签栈静态配置 `labels:[[1000 pipe ttl:0 exp:0]]`。"入口按 DSCP 定类别 → 封 MPLS 标签时把类别写进 3 bit TC"这个方案**技术上成立且实现点现成**：MPLS 包进 `sr-wg4-mpls-out`（我们的垫片，见 `sr_wg4_mpls_out.c`）时标签字已由 mpls-label DPO 写好，节点里按 `vnet_buffer(b)->opaque[]` 里存的类别改写标签字的 EXP 3 位即可（DSCP→3bit 一张 64 项静态映射表，RFC 3270 pipe 模式，约 20 行）。但要想清楚**给谁看**：
  - ① TC 在 WG 加密载荷里，**中转 SR 节点读不到**——中转调度指望不上它；
  - ② 读它的人只有接收端 CPE 的 mpls-input（WG 解密后、内层 IP 解析前）。对接收端 QoS 有真实价值（在 mpls 弧上就完成分类，不用等解出内层 IP）；对发送端 QoS 零增益（发送端早已用内层 DSCP 分类）；
  - ③ 当前 lab 只有一跳 MPLS（CPE4↔CPE2），没有中间 LSR。
结论：如果近期要给**接收端**做 QoS（CPE2 出口向 LAN 的调度），就按这个方案做 TC 映射；只为发送端 QoS 则不做。

**外层 IP DSCP 复制（推荐）**：`sr-wg4-encap` 挂在 underlay 出口弧、外层 IP4/UDP 头已写好且可写（encap 的三步 memmove 就在改这段）。做法：入站 QoS 钩子分类时把 3 bit 类别存进 `vnet_buffer(b)->opaque[]`（同一 buffer 走到 encap），encap 把类别映射进外层 ToS。中转节点按外层 DSCP 调度——RFC 2983 pipe 模式，协议零改动、双端零升级。注意：encap 时内层头已进密文读不到，所以类别必须经 opaque 传递，encap 自己不能重新分类。

### 12.4 什么时候才值得做

现在都不做。触发条件按顺序：
1. 出现"中转节点要做差分调度"的需求 → 先做外层 DSCP 复制（改动 ≈ encap 节点 20 行＋opaque 约定）；
2. 需要 HMAC 保护/逐流统计/控制器染色 → SR 头 v2 加 flowID（reserved[2]），带版本协商；
3. MPLS EXP 永远排最后。

---

## 8. 这份文档不承诺的东西

- ~~**没有写代码**，也没有改 bundle 段格式。第 5 节的模型是方案，不是已落地形状。~~
  —— **部分作废（2026-11-12 夜）**：路径 C 的调度器已经写出来并跑通（`vpp-plugin/sr_qos.{c,h}`，
  4 队列 + LLQ + DRR + shaper，容器内实测见 §10.4 之前的记录），但**挂点未定**（§10），
  第 5 节 C1 的 bundle 模型仍然只是方案。
- **C2 的三个数字一个都没量**（§6.4）。在那三项之前，C2 的任何"能保证带宽"都是纸面的。
- **`policer_classify_set_interface` 的 CRC 没取到**（§5.6），这是 C1 的第一块砖。
- **没有在 CPE1/2/3 上验**：这轮只用了 CPE4。四台的 VPP 版本/插件面同构（同一 deb），
  但 `af_packet` 的 offload 与 MAC 形状在 CPE1 上历史不同（#302），C2 落地要逐台核。
