# 与主流 SD-WAN 的功能差距扫描（2026-11-11）

> 动机：owner 倾向直接做 C（隧道 egress 整形）。动手前先把"我们在这张产品能力地图上的位置"钉住，
> 免得 C 做成一个孤立的限速功能，也免得高估短期竞争力。
> 口径：**【码】**=读代码/仓库文档得到，给路径；**【量】**=lab 实测；**【推断】**=没有证据，只作判断。

---

## 0. 一句话结论

- **数据面底座已经在主流水平之上**：SR-over-WG、两跳中转、BFD 事件驱动收敛（摘路 ≤40ms / 装回 ≤65ms）、
  每隧道 RTT/抖动/丢包 + 字节计数。这几件主流厂商大多用 IPsec 单跳 + 1–3s failover。
- **控制面/运营面比二线厂商完整**：多租户 + RBAC、claim code 自助入网 + 自建 CA、草稿/发布/版本/diff/回滚、
  告警 webhook + 台账/审计、远程诊断白名单。【码 `controller/cmd/sr-ctrl/`】
- **缺的是"能不能进真实客户网"的三块**，不是锦上添花的那几块：
  1. **应用识别（L7）** —— 完全没有；
  2. **本地互联网出口（DIA + NAT + DHCP/DNS）** —— 完全没有；
  3. **QoS 的队列与整形** —— 只有 policer（超出即丢），C 正在补。
- **安全/SASE 不是"能力缺口"，是"战略选择"**：补它要一条产品线，不补就要在方案里明确"交给合作方"。

---

## 1. 已经对齐（甚至领先）的部分

| 能力 | 我们的实现 | 与主流比 |
|---|---|---|
| 加密 overlay | 自研 SR-over-WireGuard，SR 头在 WG 载荷外（中转可路由、HMAC 防篡改）【码 `vpp-plugin/`】 | 主流是 IPsec/GRE；**多跳源路由中转是我们的差异化** |
| 多跳中转 | 两跳中转（3 段）已端到端验证【码 `docs/underlay-design.md` §15】 | 主流多为单跳 hub-spoke / full-mesh，多跳靠叠加隧道 |
| 多路径与颜色 | 颜色 0..7，FIB preference 自动回落【码 `controller/internal/underlay/state.go` Color 注释、`internal/fib`】 | 对齐 AAR 的"按链路质量选路"骨架 |
| 动态选路 | 每 (VPN, peer) 行：latency/jitter/loss 门限 + 3 拍迟滞 + preempt 开关 + 人话理由【码 `internal/sla/selector.go:40-85`】 | 对齐主流 AAR；**但粒度是隧道，不是应用** |
| 收敛速度 | 事件驱动，摘路 ≤40ms、装回 ≤65ms【量 #276/B3】 | 优于主流 1–3s failover 的宣传值 |
| 隧道可观测 | 每隧道 RTT/抖动/丢包 + BFD + 双向字节/包计数 + 24h bps【码 #294】 | 对齐；**缺 per-flow/per-app** |
| ZTP | claim code + 预授权 + 自建 CA 签发设备证书【码 `preauth.go`/`enroll`】 | 对齐主流 ZTP；**缺固件/镜像生命周期** |
| 配置治理 | 草稿 → 校验 → 发布 → 版本历史 → diff → 回滚（回滚走同一条发布路径）【码 `config_versions.go`/`diff.go`】 | 对齐中高端；二线厂商常没有 diff/回滚 |
| 多租户/RBAC | operator/tenant 两级，账号禁用/重置【码 `admin.go`】 | 对齐 |
| 运维 | 台账、告警 webhook、操作审计、远程诊断白名单【码 `opslog.go:250`、`diag.go:45`】 | 对齐 |
| 客户侧 L3 | VRF、静态路由、VRRP、FRR（BGP/OSPF）双向 redistribute、路由泄露白名单【码 `internal/apply/`】 | 对齐"分支路由"基本盘 |
| 组网编排 | 站点、自动编排预览、路径分配器、拓扑图、RR 分组 | 对齐 |

---

## 2. 差距清单（按"丢了会怎样"排序）

### S0：不补，进不了真实客户网络

| # | 能力 | 主流做法（代表） | 我们现状 | 差距 |
|---|---|---|---|---|
| S0-1 | **应用识别 L7** | Cisco NBAR2、PAN App-ID（能分 Zoom 视频/共享屏）、Aruba first-packet iQ、VeloCloud DPI | **零**：全仓无 DPI/签名/应用模型；策略只能按五元组【码，grep 无命中】 | 没有它，"按应用选路/限速"只能靠运维逐个填 IP:port，SaaS IP 一变就失效 |
| S0-2 | **本地互联网出口** | 所有主流：DIA + NAT44/NAT64 + DHCP server/relay + DNS proxy | **零**：控制面无 NAT/DHCP 模型；lab 里客户 LAN 只做 overlay，没有出网点【码，grep 无命中】 | 真实分支 60–80% 流量是上网/SaaS。没有 DIA，客户必须回中心，SD-WAN 的价值主张不成立 |
| S0-3 | **QoS 队列/整形** | 8 类队列 + 整形到线路速率 + DSCP 映射 + 每队列统计（全部主流） | 只有 policer（CAR，超出即丢）+ classify mark；无队列、无调度、无 shaper【码 `docs/sdwan-policy-plan.md` §0.6；量 #297】 | 见 §4：C 要补的就是这一块，但边界必须先定 |
| S0-4 | **对称 NAT 穿越** | 主流：STUN + 端口预测 + relay/TURN 兜底 | STUN 发现已有；relay/端口预测挂在 NAT 阶梯 #222（未做）【码 `docs/agent-handover.md` §9】 | 宽带接入里对称 NAT 占比不低，缺兜底就是"有些站点永远建不起隧道" |

### S1：竞标会丢分（客户会问，问到就尴尬）

| # | 能力 | 主流做法 | 我们现状 | 差距 |
|---|---|---|---|---|
| S1-1 | 安全 | Fortinet 一体机 NGFW+IPS+URL；PAN App-ID+ZTNA；Cisco Umbrella | VPP 有 acl-plugin，但**没有建模成"防火墙策略"页面**；无 IPS/URL/AV；无 ZTNA/SASE 对接【码】 | 要么补一条安全线，要么明确"不做，对接第三方" |
| S1-2 | 链路增强 | VeloCloud DMPO（逐包）、Aruba Boost（FEC/去重/压缩）、PAN 包复制 | 无 FEC/去重/包序纠正/逐包分担；只有按流按颜色选路【码，grep 无命中】 | 弱网（丢包 1–3%）场景体验差距明显，是"贵的那部分"卖点 |
| S1-3 | 云/SaaS 接入 | Cloud OnRamp（AWS/Azure/GCP/阿里云）、SaaS 加速、全球 PoP | 无云网关、无 SaaS 加速、无 PoP；只有自建 CPE 组网【码】 | 大客户/多云场景必需；中小可后置 |
| S1-4 | 高可用 | CPE 双机（会话同步）、控制器集群、RR 多活 | CPE 侧只有 LAN 的 VRRP VIP，**无隧道/会话状态同步**；sr-ctrl 单实例（RR 有分组）【码，grep leader/standby 无命中】 | 金融/运营商招标的硬门槛 |
| S1-5 | 升级体系 | A/B 分区 + 分批灰度 + 不断流升级 | 手工换二进制，客户可见断流 ~4s；无固件包管理/版本编排【码，#293 实测 4.2s】 | 规模化运维的痛点，也是"可信交付"的门面 |
| S1-6 | 可观测性深度 | per-app/per-user/per-path、IPFIX 导出、合成拨测（HTTP/DNS/MOS）、30 天+、对接 Prometheus/SNMP | 有隧道级指标 + 24h；**无 flow/IPFIX、无 per-app、无 top talker、无合成拨测、无 SNMP/syslog 导出**【码】 | 排障时"客户说慢"我们只能给隧道 RTT，给不出"哪个应用慢" |
| S1-7 | IPv6 | 主流已双栈（含 IPv6 隧道、v6 策略、VRRPv3） | 配置模型/隧道/策略全无 v6（只有 diag 白名单里有 `ip6 fib` 只读命令）【码】 | 政企/运营商逐步强制 |

### S2：差异化/规模化才需要

| # | 能力 | 现状 |
|---|---|---|
| S2-1 | 路由广度：multicast、route-policy（community/AS-path）、OSPFv3、与 PE 的完整 BGP | 自研 sr-route 只做 overlay 那半边；客户侧靠 FRR，但没有 route-policy 建模【码】 |
| S2-2 | 编排：模板/意图化批量下发、灰度批次、合规基线/漂移检测、计划变更窗口 | 目前是逐设备草稿；有自动编排（隧道层）但不是"站点模板"【码】 |
| S2-3 | 设备健康：CPU/内存/温度/磁盘/进程 | 只有隧道 + 控制面状态；遥测无 CPU/内存【码，grep 无命中】 |
| S2-4 | 许可与计量：license、带宽分级、到期控制 | 无【码】 |
| S3-1 | LAN/Wi-Fi 管理（AP、交换机、PoE） | 无（看产品定位，可以永远不做） |
| S3-2 | AIOps：基线、异常检测、根因建议 | 无；2026 年竞标常被问，但不是门槛 |
| S3-3 | 多形态：虚拟 CPE（公有云/私有云镜像）、第三方硬件 | 只有自研盒子（VPP on Linux）；理论上可跑虚机，但没有交付形态 |

---

## 3. 三条产品路线，各自的最小补齐集

选哪条决定 S1 里哪些"必须做"：

| 路线 | 对标 | 必须补 | 可以永远不做 |
|---|---|---|---|
| **A. 专线替代 / 运营商 underlay 优化** | Aryaka、运营商自建 | S0-3 QoS、S0-4 NAT 穿越、S1-4 HA、S1-6 可观测、S2-4 计量 | DPI、安全、LAN 管理 |
| **B. 企业分支一体机** | Fortinet、VeloCloud、Versa | S0-1 DPI、S0-2 DIA+NAT、S0-3 QoS、S1-1 安全、S1-2 链路增强、S1-3 云接入 | — |
| **C. Overlay 连接 / 多云互联** | Equinix Fabric、云厂商 SD-WAN 网关 | S0-3 QoS、S1-3 云 onramp、S1-6 可观测、S1-7 IPv6 | DIA、LAN 管理、FEC |

**【推断】** 我们现在的资产（自研数据面 + 多跳 SR + 强控制面）最贴 A，其次 C；走 B 等于同时补 DPI、安全、DIA 三条产品线，
不是"再做一个模块"的量级。**这条判断要 owner 拍**，因为它决定 C 之后下一步是 DPI 还是 DIA。

---

## 4. 对 C 的直接影响：QoS 的边界必须先定，否则会做成"更精细的 policer"

主流 QoS 的完整形状是四件套：**分类 → 标记 → 队列/调度 → 整形**，加上**每队列统计**。
我们 C 如果只做"每隧道一个令牌桶"，那和现在的 policer 是同一种东西，只是挂点从 underlay 挪到 wg 出口。

### 4.1 C 的最小可交付形状（建议）

1. **每隧道 egress shaper**：整形到 CIR（缓存后按速率发出），不是 policer 的直接丢弃 —— 这样 TCP 不会因为突发被整窗砍半。
2. **每隧道 8 类队列**（可先做 4 类）：strict priority（语音/信令）+ WFQ/DRR（其余），每类有最小保证与上限。
3. **DSCP 映射**：入口信任/重标记 → 内层 DSCP → **复制到外层 IP**（否则承载侧看不到，C 的一半价值丢了）。
4. **每队列统计**：tx/drop/queue-depth/排队时延，agent 上报，控制台能证明"策略生效"（否则和 #297 一样只能靠计数器猜）。
5. **配置模型**：`Tunnel.Bandwidth`（现在没有这个字段【码 `underlay/state.go` Tunnel】）+ QoS 策略（per device × per tunnel × per class）。

### 4.2 三个工程硬约束（方案里必须写清取舍）

- **多 worker**：`wg4-output` 每个 worker 各跑一份。队列若 per-worker，实际速率 = worker 数 × CIR；若 handoff 到单一 shaper 线程，
  有跨核开销与 head-of-line 风险。**这条不拍，做出来在 4 worker 盒子上就是 4 倍超发。**
- **加密后单流**：外层是一条 UDP，运营商侧只能看 DSCP。要么插件复制 DSCP（C 里做），要么承认"整条隧道一个 class"。
- **队列深度 = 延迟**：CBS/队列上限直接换算成毫秒。语音队列上限要 ≤10ms 量级，否则抖动比丢包更伤体验。

### 4.3 没有 DPI 时 QoS 的可运维性

首版只能按"隧道 / 五元组 / DSCP"分类。这够做"给某条隧道限速 + 给语音打 EF"，但做不了"给 Teams 优先"。
所以建议：**C 的接口里留 `class_id`（0..7）与分类器抽象位**，方案 3（classify 分级）落地时直接填进去，
而不是那时候再改一遍数据面和配置模型。

---

## 5. 建议顺序

| 顺序 | 事项 | 理由 |
|---|---|---|
| P0 | **C：隧道 egress shaper + 8 类队列 + DSCP 复制 + 每队列统计 + 控制台 QoS 模块** | 唯一"数据面缺口"且与现有策略模块天然一体；也是 S0-3 的补齐 |
| P1 | **方案 3：分类（先五元组/DSCP 组，后评估 nDPI/PacketLens）** | 没有分类，QoS 只能按隧道；有了分类，QoS 与动态选路共用同一套 class |
| P2 | **DIA + NAT44 + DHCP/DNS** | 商业门槛（S0-2）；比安全更容易做成"能用"的最小版 |
| P3 | 安全（先状态防火墙 + URL 过滤对接，不做 IPS）或明确"SASE 交给合作方" | 战略选择，先拍定位 |
| P4 | HA / 升级体系 / 可观测性深度 / IPv6 | 规模化与招标门槛 |

---

## 6. 动手前要 owner 拍的 4 条

1. **定位**：A（专线替代）/ B（企业一体机）/ C（overlay 多云）—— 决定后面三年补什么。
2. **QoS 队列模型**：首版 8 类还是 4 类？是否首版就做"内层 DSCP 复制到外层"（我建议做，成本低、价值高）。
3. **多 worker 模型**：per-worker 队列（实现简单、速率偏差）vs handoff 单 shaper（准确、跨核开销）。
   —— 这条需要先在 lab 量一次 4 worker 下的实际转发路径，不能靠推断。
4. **首版是否接受"只按隧道/五元组，不做应用识别"**（我建议接受，接口留好 class_id）。
