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

**这是推断，未实测。** 依据三条：① FIB 里到隧道的路由带 `[features]` 标记（说明走
ip4-output 弧）；② feature 列表里 `wg4-output-tun` 在弧上；③ "之后是死点"的既有实测。
动手写整形器之前，先花半天写一个 **40 行的计数节点**挂上去，打 10 个 ping 看 counters
是不是 10——不通过就退回 9.3 的 A 或 B，不要在没验的挂点上写 900 行。

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
3. 是否接受先花半天验 §9.4 的挂点（我认为这是 C 的**前置门槛**，不是可选项）。

---

## 8. 这份文档不承诺的东西

- **没有写代码**，也没有改 bundle 段格式。第 5 节的模型是方案，不是已落地形状。
- **C2 的三个数字一个都没量**（§6.4）。在那三项之前，C2 的任何"能保证带宽"都是纸面的。
- **`policer_classify_set_interface` 的 CRC 没取到**（§5.6），这是 C1 的第一块砖。
- **没有在 CPE1/2/3 上验**：这轮只用了 CPE4。四台的 VPP 版本/插件面同构（同一 deb），
  但 `af_packet` 的 offload 与 MAC 形状在 CPE1 上历史不同（#302），C2 落地要逐台核。
