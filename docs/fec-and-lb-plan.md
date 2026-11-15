# FEC 与逐包负载均衡：VPP 下有没有现成方案

> 调研时间 2026-11-12 深夜。用户指令：QoS 必做（已跑通，见 `qos-path-c-plan.md` §11）；
> DPI 先不做；**FEC 与每包负载均衡先调研"VPP 下有没有现成方案"**。
> 本文只回答"有没有、要多少、边界在哪"，不含代码。
> 取证口径：`srwg-dev` 镜像（vpp 26.06-release）+ CPE4 真机 CLI 实测，命令都列在文中。

---

## 0. 一句话结论

| feature | 现成方案 | 结论 |
|---|---|---|
| **FEC** | **没有**。镜像 103 个插件里 0 个 FEC/RS/LDPC；系统里也没有 `gf256`/`cm256`/`jerasure`/`isal` 这类有限域库 | 要自己写，但**不必从零想形状**：专利 CN119402145A 把"VPP 里做 FEC over SD-WAN"的节点划分、配置面、冗余包标识都写清楚了 |
| **每包负载均衡** | **没有开关**。VPP 的 ECMP **设计上就是按流哈希**，逐包发射得自己写节点轮转 bucket | 技术上做得到，但**不建议做**：对 TCP 有害，且我们有更便宜的替代（§2.4） |

---

## 1. FEC

### 1.1 现成方案盘点（实测）

```bash
# 1) 插件面：一个都没有
$ ls /usr/lib/x86_64-linux-gnu/vpp_plugins/*.so | wc -l
103
$ ls /usr/lib/x86_64-linux-gnu/vpp_plugins | grep -iE 'fec|gf|reed|solomon|rsc|ldpc|coding'
（空）

# 2) 库面：没有有限域/RS 实现
$ ls /usr/lib/x86_64-linux-gnu/ | grep -iE 'gf256|cm256|fec|reed|isal|jerasure'
（空）
```

也就是说：**FEC 在 VPP 生态里不是"配一下就有"的东西**。上游没有 `fec` 插件，
也没有可链接的 RS 库；真要做得自己实现 GF(256) 编解码。

### 1.2 别人怎么做的（专利给的形状，可直接照抄骨架）

专利 **CN119402145A**（基于 VPP 的 SD-WAN 前向纠错）描述的形状与我们这台盒子几乎同构：

- **节点位置**：编码放在**加密/封装之后**（外层 UDP 已成型），解码放在**解密之后、
  交付协议栈之前**；两个节点都用 `VLIB_NODE_FN` 注册，走 feature arc 挂载。
- **冗余包怎么认**：给冗余包分配**专用 UDP 端口**（或在外层头里留类型位），
  对端按端口分流到解码节点 —— 不需要改隧道封装格式的其它部分。
- **零拷贝倾向**：编码节点直接读 `vlib_buffer_t`，不额外分配（除新造冗余包本身）。
- **配置面**：`vppctl` CLI + VAPI 两条路，参数是"块大小 k / 冗余数 m / 开关 / 端口"。

对我们的直接含义：**这不是一个"要不要重写封装"的问题**，而是在现有
`sr_wg4_encap` / `sr_wg4_input` 旁边加两个节点的事 —— 与我们已经在做的
`sr-wg4-qos` 同一套手法（feature + CLI + 计数）。

### 1.3 我们自己的隧道有两条额外约束（专利里没提，但我们要面对）

1. **外层五元组是隧道判别的唯一凭据**（`qos-path-c-plan.md` §9.2 实测：出口按外层
   `udp sport` 归隧道，20000/20001/20002）。所以冗余包必须**沿用原包的 5 元组**，
   否则对端归不到隧道。
2. **端口已经被占满**：`20000/20001/20002` 是三条隧道的本端端口，不能拿来做
   "冗余包专用端口"。可行的做法只剩两种：
   - 借用 **keepalive/保留端口**（我们自己的 binding 表里有 `path_id`，可以再分一路）；
   - 在 **WG 头之后** 加一个 1 字节类型位（要改封装格式 = 双方都要升）。
   前者不动封装，后者更干净；这条是**要先拍的第一个决定**。

### 1.4 三种粒度，代价差一个数量级

| 方案 | 冗余比 | 抗丢包形状 | 实现量 | 建议 |
|---|---|---|---|---|
| **1+1 复制**（每包发两遍，对端去重） | 100% | 抗单包随机丢包，突发 ≥2 全丢 | ~150 行（去重窗口 + 复制 + 外层打标） | **先做这个**。最笨但最先能承诺"丢包率减半" |
| **XOR 块 FEC**（k 包 + 1 冗余） | 1/k | 块内丢 1 可恢复；块内丢 2 无解 | ~250 行（成块、定时器、序号、异或） | 第二步。不需要 GF(256)，代价可控 |
| **Reed-Solomon（k+m）** | m/k | 抗突发 | +600 行（GF(256) 表、矩阵求逆）或引第三方库（要自己打包进镜像） | 有客户真要抗突发再做 |

**关键取舍**：前两种**不需要 GF(256)**，所以不踩"没有现成库"这个坑。RS 才需要，
而 RS 的收益只在"长突发丢包"上，CPE 场景（单跳隧道、随机丢包为主）通常用不上。

### 1.5 落在哪（模块边界）

```
内层包 → [sr-wg4-qos 排队] → sr-wg4-encap（加 WG 头/MPLS）→ 【FEC 编码】 → af_packet 出
                                      ↑ 冗余包在这里生成（复制 buffer，改类型位）
af_packet 入 → [FEC 解码] → sr-wg4_input（解密）→ ip4-lookup
                 ↑ 去重/恢复在这里（按 (tunnel, seq) 维护窗口）
```

- 编码点必须在**加密之后**：那样冗余包就是"另一个完整的加密包"，对端不需要特殊处理
  解密；同时也避免"同一个明文被加密两次"的 nonce 问题（WG 的 nonce 是计数器，
  复制明文再加密会**破坏 nonce 唯一性**，这条必须写进代码注释）。
- 解码点必须在**解密之后**：冗余包解密完就是明文重复包，去重即可。
- 状态：每隧道一个序号空间 + 一个 64 位窗口（`clib_bitmap` 或 u64 移位）；
  窗口外/太老的包直接放行（宁可重复也不要卡住）。

### 1.6 判据（先定，再写代码）

1. `show sr-wg fec` 两个计数：`encoded` / `recovered`，且
   `recovered / encoded_loss ≤ 1`（不会凭空恢复出包）；
2. 注入 10% 随机丢包（容器里用 `tc netem loss 10%`），业务 ping 丢包率从 10% 降到 <1%
   （1+1）或 <2%（XOR k=8）；
3. **带宽代价可测**：`tx_bytes` 增量 ≈ 1+冗余比；
4. 关闭 FEC 后一切回到基线（`rate 0` 同款 fail-open）。

### 1.7 要拍的问题

1. 冗余包标识走"保留端口"还是"WG 头类型位"？（前者不动封装，后者更干净）
2. 先只做 **1+1 复制**吗？（能最快兑现"丢包率减半"）
3. 目标丢包形状是什么（随机 / 突发）？这决定要不要 RS。

---

## 2. 每包负载均衡

### 2.1 VPP 的 ECMP 是按流哈希（这是设计，不是缺省值）

结构证据（镜像里的头文件 + 盒子上的 CLI）：

- `load_balance_t` 带 `flow_hash_config_t lb_hash_config`（`vnet/dpo/load_balance.h:158`）；
  bucket 选择 = `hash % lb_n_buckets`，hash 由 `ip4-lookup` 用
  `ip4_compute_flow_hash()` 按 `lb_hash_config` 算出来，并缓存在
  `vnet_buffer(b)->ip.flow_hash` 里（同一条流后续包不再重算）。
- 可配字段实测（CPE4）：

  ```
  $ vppctl show ip fib | head -1
  ipv4-VRF:0, fib_index:0, flow hash:[src dst sport dport proto flowlabel ] ...
  $ vppctl set ip flow-hash src dst
  $ vppctl show ip fib | head -1
  ipv4-VRF:0, fib_index:0, flow hash:[src dst sport dport proto ] ...
  ```
  即 `set ip flow-hash <字段...>` 直接改全局 ECMP 哈希输入；API 是
  `set_ip_flow_hash`（message 1128，另有 V2/V3，见 `vnet/ip/ip.api.h`）。

**推论**：VPP 里没有"逐包轮转"这种模式可以打开。`lb_hash_config` 只能决定
"哪些字段参与哈希"，永远不能让它"每包不同"——除非我们自己往 hash 里注入变量。

### 2.2 真要做逐包，得写代码（两条路）

1. **自建发射节点**：在 encap 之前，自己持有 N 条隧道的 `sw_if_index`，每包
   `next = (next + 1) % N`，直接把包递给对应隧道的 encap。位置与我们 QoS 节点
   同一个弧，**但会和 FIB 打架**：路由是 FIB 装的，节点自己选隧道等于绕开 FIB，
   必须同时接管"哪条隧道属于哪个前缀"的配置。
2. **改 hash 输入**：给每个包塞一个变化的字段（比如复制一份改过的 `ip.flow_hash`），
   让 `lb_n_buckets>1` 时逐包散开。可行但要改 `vnet_buffer` 语义，且
   `ip4-lookup` 会用缓存值 —— 等于每包清缓存，代价比方案 1 还高。

两条都不是"配置"级别的活，是**几百行数据面代码 + 一套新配置模型**。

### 2.3 为什么不建议：逐包对 TCP 是负优化

同一条 TCP 流的两段包走不同隧道 ⇒ 到达乱序 ⇒ 接收端三重重复 ACK、发送端
拥塞窗减半（Reno/CUBIC 都是这个反应）。在 1% 丢包就能让吞吐掉一半的链路上，
"聚合带宽"的收益通常抵不上乱序的损失。这也是为什么 VPP/Cisco/Juniper 的 ECMP
默认全是 per-flow，而不是 per-packet。

### 2.4 更值得做的三件事（都能在现有代码里落地）

1. **per-flow ECMP + 质量探测**：我们已经有 `sr_wg_probe`（BFD 风格探测），
   把"探测结果"折进路径选择（坏链路降权/剔除），这比逐包更能提升可用带宽。
2. **加权 ECMP**：`ip route add <pfx> via ... weight N` —— 让 100M 的链路比 20M 的
   链路多分到流。零代码（FIB 原生支持），只改 agent 的装表逻辑。
3. **按 DSCP/端口分流**（策略路由）：把"大流量批量"钉到便宜链路、"实时"钉到低延迟
   链路 —— 和 QoS 的队列模型天然对齐，而且**确定性**比哈希更好。

### 2.5 判据（如果还要做逐包）

1. 两条隧道 1:1 逐包时，`show interface` 两侧 tx 包数比例在 50%±5%；
2. 同一条 TCP 流（iperf3 单流）吞吐**不得低于**单隧道基线的 90% —— 这一条大概率
   过不了，它就是这个 feature 的否决判据；
3. 关掉后立刻回到 per-flow。

### 2.6 要拍的问题

1. 逐包 LB 的目标是什么？（"聚合带宽"还是"抗单链路劣化"）
   - 若是后者 → 做 §2.4 的 1+2，**不做逐包**；
   - 若是前者且能接受 TCP 掉速 → 才写 §2.2 的方案 1。
2. 加权 ECMP 要不要现在就进 agent 的装表逻辑？（零数据面代码，只影响路径权重）

---

## 3. 相对 QoS 的优先级建议

1. **QoS 收尾**（已在跑通状态：`qos-path-c-plan.md` §11）—— 补 bundle 段 + 控制台 +
   回归脚本进仓库；
2. **FEC 1+1**（最先能兑现的可靠性 feature，不动封装格式的前提是先拍 §1.7-1）；
3. **加权 ECMP / 探测联动**（零数据面代码，改 agent）；
4. **XOR 块 FEC**（带宽换丢包，需要成块定时器）；
5. **逐包 LB**（最后，且大概率不做）；
6. **RS FEC**（有客户真提"抗突发"再说）。

---

## 4. 这份文档不承诺的东西

- 没有写任何 FEC/LB 代码，也没有改 bundle 段。
- 专利 CN119402145A 只读到摘要与权利要求层面的形状，**没有逐条比对实现细节**；
  真要照做之前应该把权利要求读全（尤其"冗余包端口"的具体做法）。
- 逐包 LB 的两条实现路径都**没有做可行性验证**（没写过样机），工作量是估算。
- `ip4_compute_flow_hash` 是 VPP 内部函数（不在 `/usr/include` 里），
  本节关于"hash 缓存在 `vnet_buffer`"的说法来自 VPP 源码阅读，本轮**没有在盒子上
  用计数器逐包验证**。

---

## 5. 不等价负载均衡第一棒落地：静态路由 weight（2026-11-13）

> owner 拍板：先做不等价负载均衡（UCMP），从静态路由开始；逐包 LB 不做。
> DIA 口径同步修正：静态路由 + NAT 即 DIA，本文档之外 gap-scan 的 S0-2 **不算缺口**。

### 5.1 判别性实测（CPE4，前缀 198.18.55.0/24，测完即清）

四棒全过，UCMP 经"staticd → zebra → linux-cp → VPP"端到端成立：

1. **staticd**：`ip route <pfx> <gw> weight <n>` 关键字存在（FRR 10.7.1，vtysh 补全
   实测 "weight Set weight of nexthop"，值域 **1–65535**；0 被拒）。
2. **zebra**：同前缀两行不同下一跳聚成一组 UCMP（两个下一跳都 active +
   fib-installed）；weight 归一化到内核 8 位口径、**比例保留**（10:30 → 85:255）。
3. **linux-cp**：weight 同步进 VPP path（`show ip fib detail` 见 `weight=84` /
   `weight=254`，~1:3）。
4. **VPP load-balance 按比例复制桶**：`buckets:4`，大权重占 3 桶小权重占 1 桶
   = 75%/25% **按流**分流（flow-hash 缓存，无逐包乱序问题）。

### 5.2 实现（管理面，零 agent/数据面改动）

- `ConfigStatic.Weight`（0/1 = 默认权重，不渲染 weight 后缀——与老配置完全同形，
  frr diff 不会无谓翻行）；渲染 `ip route %s %s weight %d`（仅 weight>1）。
- 校验：weight > 65535 → error；同前缀多下一跳本来合法（去重键 = VPN|prefix|nexthop）。
- 控制台静态路由面板加 Weight 列与输入（提示语写明 UCMP 语义）。
- 测试：`TestRenderStaticWeight`（带/不带 weight 后缀）、`TestValidateStaticWeight`
  （0/1/255/65535 合法、65536 报错、UCMP 组无 error）；sr-ctrl 全套 PASS。
- sr-ctrl 已上线（md5 `bab09756a60d125bb314ae5b4464325c`，备份
  `sr-ctrl.prev-20261113ucmp`）。

### 5.3 注意点

- VPP 桶数 = 比例量化：3:1 → 4 桶；比例更悬殊时桶数更多、量化粒度更粗。
- 这只是管理面这一棒。**探测联动**（probe 劣化 → 降 weight / 摘路）与
  **多径分流比例实测**（iperf 多流验证桶分布）还没做，是 UCMP 的下一步。

---

## 6. UCMP 第二棒落地：隧道 weight 链 + 选路器自愈（2026-11-13）

> owner 拍板的三条：①丢包优先、四档非线性；②underlay 配置手填 weight + "动态"
> 复选框，配置值是**基数上限**，不回写；③迟滞 3 个周期、每周期 5 秒。
> 本节是第二棒（静态 weight 链），第三棒（动态分档）未开工。

### 6.1 链路（全部落地，`daff3a4` 之后的工作树）

```
sr-ctrl  Tunnel.Weight/Dynamic（create/update/fanout/API/console 列）
   └─ wire.TunnelSpec.Weight/Dynamic → sr-agent TunnelSync（按隧道随 bundle 下发）
        └─ agent.FibTableSync → fib.json（FibTunnelRow.Weight，0/省略 = 与老配置逐字节同形）
             └─ sr-fib tunnelConfig.Weight → fib.PlanFrom → Path.Weight
                  └─ pathIdentityEqual 把 Weight 当身份（改 weight = 改路由 = 重应用）
                       └─ vppapi FIBPath 第 12 字节（u8，0→1）→ VPP load-balance 桶复制
```

- 语义：同对端同颜色多隧道默认 ECMP（weight 全 1）；不同 weight = UCMP，
  VPP 桶数按比例量化、按流哈希（无逐包乱序）。weight 应用到该隧道出现的
  每一张表（颜色表跨设备混对端，owner 已接受）。
- 前置：同对端第二条隧道的创建门（M-3 的 409）还没抬——链路已就绪，门一抬即可用。

### 6.2 顺手修掉的实机缺陷：sla 选路器重启后永不重写（CPE1 跨站全黑）

- 现场：CPE1 的 VPP 重启（插件换装）后，apply 管道把选路表 10001 的 0/0 重种成
  drop；agent 内存里 `written=true`、决策没变、表里"有 0/0"——旧跳过条件
  `exists && written && color==current` 永远为真，选路器一行日志不再打，
  0/0 落 dpo-drop，该行动态策略的所有客户流量黑洞。
- 修法（`internal/sla/selector.go`）：**连接代数**。`planeConn` 每次成功重拨
  `gen++`；`rowState` 记 `writtenGen`；跳过条件加 `writtenGen == gen`。
  重拨必然换代，换代必然幂等重写一次（ip_route_add_del 的替换语义），盖掉 drop。
  2026-11-13 CPE1 重启实测复现了同一签名（retval=-3 连打五拍后成功
  "行 1 → 颜色 2"），修复后六方向 loopback 源互 ping 0% 丢包。
- 回归测试：`TestSeedDropRouteReplacedAfterVPPRestart`（复现实机序列：
  写 → 表被清+重种 drop + 连接断一拍 → 换代重写 → 稳定后不再写）。

### 6.3 部署与验证（2026-11-13 傍晚实测）

- 四台 sr-agent `abca7697b55144f83d7cc8cf1221a624` + sr-fib
  `d07ba51e3ff991b52d44a3fde789231d`；host sr-ctrl darwin
  `2d7911e9dbc985b0a79dca68713d3f68`（detached 脚本起，pid 会话无关）。
- 验证：cust12↔cust4、cust12↔cust3、cust3↔cust4 六方向 `ping -I 172.16.x.1`
  全 0% 丢包；四节点心跳恢复；sr-ctrl observe 行持续。
- 两个环境教训进了 agent-handover §2.1：CPE1 磁盘满导致 guest 僵死；
  客户机连通性必须绑 loopback 源（LAN 子网源不在 overlay 宣告里）。

### 6.4 第三棒待办（owner 已拍参数）

- loss-first 四档非线性映射（0~config.Weight 上限内取档），agent 端算
  effectiveWeight 写 fib.json，sr-fib 仍只做对账器（不读探针）。
- "动态"档迟滞：3 周期 × 5s/周期；配置 weight 只是基数上限，不回写。
- 控制台动态档仅展示。前置：先抬同对端多隧道门（M-3 的 409 + Slot 分配）。

## 7. UCMP 第三棒前置：同对端多隧道门已抬（M-3）+ 实机 25:75 验证（2026-11-13）

### 7.1 抬门语义（`Tunnel.Slot`）

- 同对端（无中继的第二条直连隧道）原本被 `ErrPairTaken` 409 拒绝；现在改为
  **密钥槽派生**：每台设备持 8 把已认证密钥（WGKeySlots=8），对端第 n 条
  隧道两端各占第 n 号槽，无需密钥环交换。
- `create()` 走 `allocPairSlot`（最低空闲槽，上限 8 → 新哨兵 `ErrPairFull`
  仍映射 409）；`ErrPairTaken` 降为 Fan 专用哨兵：**编排批次（Fan）每对仍
  只建一条**（跳过理由"设备对之间已有隧道 %s"），第二条是人工的、有意识的
  行为，只放手动 Create。
- 槽永不重编号：第一条恒为 0；删除中间槽后新隧道补最低空闲位。
- API/控制台：tunnel 视图新增 `slot` 字段，表格加"槽"列（槽 0 灰显），
  表单提示写明 8 槽上限与 Fan 行为。
- 回归：`TestSamePairTunnelsGetDistinctKeySlots`（0→1，删除复用）、
  `TestSamePairTunnelsCapAtKeySlots`（第 9 条 `ErrPairFull`）、
  `TestSecondTunnelOnTheSamePairGetsFreshPortsAndOverlay`（新端口 +
  overlay route.json tunnels 第 4 段新增一行）、bundle/spec 携带 Slot、
  Fan 跳过理由、409 映射。

### 7.2 实机链路（1↔4 对，颜色 3）

- t10（槽 0，weight 10）+ t16（槽 2，weight 30）→ 逐节点
  `POST /api/config/publish` 后 overlay route.json tunnels 段出现两行
  next hop（10.250.0.3 / 10.250.0.13），RIB 每隧道一个下一跳。
- CPE1 表 3000 `172.16.4.1/32`：`dpo-load-balance buckets:4`，
  **[0-2] → wg1016（w30），[3] → wg1010（w10）**，桶布局 1:3 精确。
- 注意：新隧道只进 route.json 于 re-publish 之后；sr-fib 在探针会话
  建立（BFD Up）前跳过该下一跳并告警，属预期。
- 环境坑：t15 一度建在宿主 enp0s8（客户口/LAN）上，WG peer endpoint
  172.31.12.1:20002 不可达，BFD 永不 Up——删掉重建到 enp0s9 即好。

### 7.3 分流比实测（2026-11-13，/tmp/ucmp_measure.py 方法论）

- 关键认知：**每条 UDP 五元组整流落进一个桶**（VPP 按流哈希），10 条流
  × 4 桶的量化方差极大（此前 58:42 的假象）；要测的是**大量互异五元组的
  总量比**，且探针流量用等长空闲窗基线扣除。
- 结果：50 流 × 100 包（源/目的端口互异，2ms 步进防 VBox L2 丢突发），
  14s 基线探针 delta ≈151/152 包；扣基线后
  **wg1010 1394 : wg1016 3991 = 25.9% : 74.1%**（期望 25%:75%，误差 <1pp）。
- 残余噪声：ssh 心跳与 ICMP 回包不计入 CPE1 TX 方向，扣基线后可忽略。
- 实验脚本留存：/tmp/ucmp_measure.py（基线窗 + 50 流发送 + TX 计数差减）。

## 8. UCMP 第三棒落地：动态 weight（丢包四档非线性 + 迟滞）实机验证（2026-11-13 深夜）

### 8.1 实现（agent 侧，sr-fib 仍是纯对账者）

- 全部在 `internal/agent/fibtable.go`：`lossLevel`（丢包‰ → 档 0..3）、
  `weightForLevel`（基数上限 × 档位 → 有效 weight，∈[1,base]，base 0 = 1）、
  `levelTracker`（迟滞状态机）。门槛 50/200/500‰（间距非线性），份额
  1 : ⌈⅔⌉ : ⌈⅓⌉ : 1。
- 迟滞：候选档位连续 **3 拍**成立才生效（一拍 = 一轮心跳 = 5s）；首轮从
  **观察真值**起步（不从捏造的档 0 起步——重启恰好赶上链路劣化时，不能把
  满权重压在坏路径上 15 秒）；候选跳走就重新数。
- 探针读数按 sw_if_index 对齐 FibTunnelRow；**读不到 = 档位冻结**（维持
  上一档，不推进迟滞——"测不到"既不是健康也不是故障），告警喊一次、
  恢复喊一声。纯静态盒子不多跑 Probes dump。
- 只看丢包（owner 拍板"丢包优先"）：探针的 128 槽滑窗（~38s 惯性）本身
  就是低通，叠加 15s 迟滞，瞬时抖动翻不动档位。RTT/抖动不参与降档。
- 测试 9 条：门槛边界、份额取整、迟滞状态机（含候选跳走重新数）、首轮
  立即生效、3 拍恢复、探针消失冻结、dump 失败冻结+恢复告警、动态关原样。
  `go test ./...` 容器全绿。

### 8.2 实机验证（意外但彻底：真实丢包窗口）

- 四台换 sr-agent `3f237ac9e7f7d6d50b0b59711b89b495` 后，t16 标动态
  （w30）。实验目标是人为造丢包，结果**造丢包本身成了连续六次真实验证**：
  CPE1 agent.log 完整记录——

  ```
  17:09:41 t16 档位 0→2（丢包 382‰），weight 30→10   ← CPE4 VPP 第一次崩
  17:10:27 t16 档位 2→0（丢包 0‰），  weight 10→30   ← VPP 拉起，探针恢复
  17:12:42 t16 档位 0→3（丢包 784‰），weight 30→1    ← 第二次崩
  17:15:49 t16 档位 3→0（丢包 0‰），  weight 1→30    ← 硬复位恢复
  17:16:19 t16 档位 0→1（丢包 171‰），weight 30→20   ← 隧道收敛期残余丢包
  17:16:59 t16 档位 1→0（丢包 0‰），  weight 20→30   ← 完全收敛
  ```

  非线性档 1/2/3、降档、恢复、权重回到基数上限，全链路（探针→agent 档位
  →fib.json→sr-fib→VPP 桶）都按设计走完。邻线 t18（实验隧道，见 8.3）
  起始丢包 294‰ 同样触发档 2 → weight 7，恢复后回 20。

### 8.3 造丢包的三条弯路（教训）

1. **内核 tc netem 无效**：CPE 的 WAN 口被 VPP 接管（LCP 配对，内核只剩
   vpp0/lcp-x0），`tc qdisc` 作用不到 VPP 发的包。
2. **删对端探针会话无效**：探针折返是**无状态**的（sr_wg_probe.c 输入节点
   见请求就折返，不查本机会话表）——删会话照回不误。本想顺路用
   `acl_interface_set_acl_list` 挡 UDP 51829，**这台 26.06 的 acl 插件一挂
   接口就把 VPP 干崩**（acls=[0] 和 [1] 都崩，CPE4 两次硬复位救回）；
   仓库既有结论"ACL 只有二进制 API 可靠"在挂接口这步不成立。
3. **死地址端点被 WG 端点学习反杀**：t18 的 b 端点填了无人应答的
   192.168.1.250，本想造永久 100% 丢——但 CPE4 的握手 init 照常到达 CPE1，
   VPP WG 把对端发送地址学成观测到的源，隧道自愈，丢包只持续了一个窗口。
   （顺带验证：t18 起始 294‰ 照样触发了档 2。）
- 结论：这套数据面上没有"只丢探针不丢 BFD"的干净注入点；真实验证靠
  8.2 的意外窗口已完成。lab 现存演示对：t10（w10 静态）+ t16（w30 动态）。

---

## 9. FEC 落地方案评估（2026-11-13，owner 点头前）

> owner 指令：做 FEC。前端在 underlay 配置隧道处编辑隧道启用 FEC（复选框），启用后
> 下拉选比例，支持 1:1 / 1:2 / 1:3 / 1:4 四档。倾向找现成算法/现成方案。
> 本文给出方案评估 + 待拍决定；**未点头前不写代码**（AGENTS.md §1 惯例）。

### 9.1 "现成方案"到底有哪些（结论先行）

| 层面 | 现成的东西 | 用不用 |
|---|---|---|
| VPP 插件 | **没有**（§1.1 实测：103 个插件 0 个 FEC，镜像里无 GF/RS 库） | 没得选，节点自己写 |
| **XOR 块奇偶校验** | RAID-5 同款，无第三方库，纯异或，~250 行 C | ✅ **四档比例全靠它覆盖** |
| Reed-Solomon 库 | ISA-L（BSD，AVX 优化，Ceph 在用）/ Jerasure（学术，失养）/ OpenFEC（roc fork 维护中，RS+LDPC） | ❌ 本轮不引；都要打进盒子镜像，且 RS 只在"抗长突发"有额外收益 |
| RFC 6363 FECFRAME | **协议形状**（SBN/ESI 分组编号、source/repair 包语义） | ✅ 借它的头字段设计，不引它的实现 |
| 专利 CN119402145A | VPP 里做 SD-WAN FEC 的节点划分/配置面/冗余包标识骨架 | ✅ 照抄形状（§1.2 已核） |

**推荐路线：XOR-only。** 四档比例正好是"k 个数据包 + 1 个异或冗余包"的 k=1..4：
组内丢 1 个可恢复、丢 2 个无解。好处：零依赖、不需要 GF(256)、代码量可控；
坏处：抗不了组内双丢（对随机丢包 10% 量级足够，CPE 单跳隧道场景主诉就是随机丢）。
RS 留作后续扩展位（头字段里预留），真有客户要抗突发再引 ISA-L。

### 9.2 四档比例的语义（**待拍决定 ①**）

"1:n" 两种读法都成立，实现差一个数量级：

| 读法 | 1:1 | 1:2 | 1:3 | 1:4 | 带宽代价 | 恢复能力 | 实现 |
|---|---|---|---|---|---|---|---|
| **A（推荐）：1 冗余 : k 数据** | 1 数据+1 冗余 = 复制 | 2 数据+1 冗余 | 3+1 | 4+1 | 100%→50%→33%→25% | 组内丢 1 可恢复 | XOR 块，k=1 退化为复制 |
| B：1 数据 : n 冗余（n 路复制） | 发 2 份 | 发 3 份 | 发 4 份 | 发 5 份 | 100%→400% | 丢 n-1 个仍可达 | 纯复制+去重，最笨 |

A 的下拉顺序 = 保护从强到弱、带宽从贵到省，语义自然（1:1 就是最强保护）；
B 带宽代价离谱，不推荐。**若 owner 本意是 A，整条链路只用 XOR，无需任何第三方库。**

### 9.3 冗余包怎么让对端认出来（**待拍决定 ②**）

数据面包走 `sr_wg4_encap` 出去后是标准 WG 包；异或冗余包不是合法 WG 包，
必须与数据包同五元组（§1.3 约束：外层五元组是隧道判别唯一凭据）但可被对端区分：

| 机制 | 代价 | 评价 |
|---|---|---|
| **冗余包 payload 首字节 = 保留类型位（推荐）** | 双端插件 ≥1.2.0（本轮同升）；对端 input 节点首字节分流，0xF0 进解码、1-4 走原 WG 路径 | 干净、无端口争用；混版窗口内对端只是丢掉陌生类型包（无害） |
| 保留端口 | 隧道本端端口已被 20000-20002 占满，要再占一路 binding | 不动 payload 语义，但端口管理多一套 |

### 9.4 数据面形状（照抄 §1.5 + sr_qos 同款手法）

```
数据包: sr_wg4_encap(加密) → [fec-encode: 攒组, 第 k 个或 T 超时 → 异或 → 补 1 个冗余包] → 出口
冗余包: [类型位 0xF0 | 组号 u32 | 组内实际数 k' u8 | 各包长度 u16×k' | 异或体(按最长包补零)]
对端:   udp-input → 首字节分流 → 0xF0 进 fec-decode（按组号攒）
        组齐且恰好缺 1 → 异或还原出完整 WG 加密帧 → 递回 sr_wg4_input 原路解密
        （还原的是完整帧：WG nonce 在帧头里随异或一起被保护，因此还原帧可直接解密——
         这就是"编码必须在加密之后"的原因，明文复制会破坏 nonce 唯一性，见 §1.5）
```

- 每隧道一组状态：发送侧组缓冲（k 个 buffer 引用 + 起始时间），接收侧按组号的
  64 位滑动窗口（`clib_bitmap`，太老直接放行——宁重复不卡流）。
- 部分 组（T 超时不足 k 个）：数据包照发，冗余包对"实际在场的 k' 个"做异或，k' 写进头。
- 探针包（sr_wg_probe）**不进 FEC 组**：探针本就是小包高频，冗余它纯浪费带宽。
- 与 QoS 的相处：fec-encode 在加密后、QoS 出向调度（interface-output 弧）之前，
  冗余包自然同队列出 Physical 口——**冗余包不该挤占 LLQ**，进队列时按 DSCP 0（BE）处理。

### 9.5 管理面与前端（完全复刻 QoS 那条链，6fa434d 的形状）

1. **模型**：Tunnel 配置加 `fec_enabled bool` + `fec_ratio uint8`（1..4）；PUT
   `/api/underlay/tunnels` 接收；校验（ratio ∈ 1..4）。
2. **渲染**：applier 照 renderVPP 的幂等 CLI 重放姿势，发
   `set sr-wg fec <隧道接口> ratio <k>` / `set sr-wg fec <隧道接口> off`；
   **版本纪律同 QoS：只推给插件 ≥1.2.0 的盒子，单条失败整体失败**。
3. **bundle**：隧道 spec 带上 fec 字段（第 4 段 tunnels 行）。
4. **前端**：控制台隧道编辑表单——复选框"启用 FEC"+ 下拉"冗余比例
   （1:1 最强 … 1:4 最省）"，语义按决定①的拍板写提示文案。
5. **可观测**：`show sr-wg fec`（per-tunnel: 组数、冗余包数、恢复数、超时半组数）；
   恢复数进遥测是后话，先不承诺。

### 9.6 验收判据（沿 §1.6，落到容器 lab）

1. 容器 lab `tc netem loss 10%`（内核口 netem 在容器 lab 可用）：1:1 时业务 ping 丢包
   10% → <1%；1:2 → <2%；
2. `tx_bytes` 增量 ≈ 1 + 冗余比（可测的带宽账）；
3. `recovered / (组内实际丢包) ≤ 1`（不许凭空恢复）；
4. `off` 后立即回基线（fail-open，与 QoS `rate 0` 同款）；
5. 实机只做无害性验证（装上后 t10/t16 六方向连通 0% 丢、CPU 无感）——实机造丢包的
   不可行已被 §8.3 证明，别再试。

### 9.7 工作量预估

| 块 | 内容 | 量级 |
|---|---|---|
| vpp-plugin | sr_fec.c/h（encode/decode 节点 + 组状态 + CLI + 计数），插件 bump 1.2.0 | ~500 行 C + lab |
| 控制面 | Tunnel 模型/校验/渲染/bundle/PUT + 测试 | ~300 行 Go |
| 控制台 | 隧道表单复选框+下拉 | ~60 行 |
| lab | 容器 netem 判据 lab + 实机部署（四台插件换件） | 半天 |

### 9.8 待拍（决定点汇总）

1. **① 比例语义**：A（1 冗余:k 数据，XOR，推荐）还是 B（n 路复制）？
2. **② 冗余包标识**：类型位（推荐）还是保留端口？
3. **③ 首版范围**：XOR-only 四档（推荐，无第三方依赖）？还是本轮就带 RS（引 ISA-L，
   +镜像打包 + 2-3 天，只在抗突发上有增量收益）？

### 9.9 落地记录（2026-11-13，全部完成）

**拍板结果**（§9.8 三点全按推荐）：① 语义 A（1 冗余 : k 数据，XOR-only）；
② 类型位 0xF0；③ XOR-only 四档，不引 RS。语义细节：1:1 退化为整包复制
（对端 WG 反回放去重）；编码点在加密之后（encap Step 14.5，异或对象 = WG
帧），因为含 SR 头的全载荷异或会被中转改写的 seg_left 破坏；冗余包逐字复制
成员的外层 [IP4|UDP|SR] 头、action 类别位与外层 ToS 清零（都在 HMAC 排除集/
免签名字段），所以中转照常转发、双跳隧道的 FEC 免费成立；接收侧计数配对
（数据包不打标不缓冲，无定时器），j==n-1 时用 len_xor 的自反性原地还原；
FEC 一切失败只丢冗余机会（fail-open）。接收侧已知限制如实记录在 sr_fec.h
（乱序错配、还原帧 QoS 类别落 BE、单线程假定）。

**插件 1.2.0**（qoslab 容器构建，md5 `5533a392…`）：`sr_fec.{c,h}` +
encap Step 14.5 + input Step 5.5/5.6 + 新 API `sr_wg_fec_set` + CLI
`set/show sr-wg fec`。**lab 带出并修掉的三处静默失效缺陷**（都是真实包才
暴露的）：

1. 冗余包外层 `ip4->length` 漏算 sr_len（下一跳按截断长度解析，冗余包全废）；
2. 还原帧 `current_length` 多报了 SR_OUTER_HDR_LEN（wg4-input 按虚高长度
   解析到垃圾，还原帧全被 WG 标签拒——还原"成功"但对端全丢）；
3. 冗余包处理失败分支没把 next_index 改回 DROP（fec 载荷被当 WG 帧送进
   wg4-input 刷 Interface error）。

**管理面**：Tunnel.FecRatio（0/1..4，越界状态层与 API 层各挡一次）→ bundle
TunnelSpec.FecRatio → agent ApplyFec（按值去重；失败只记警告不中断隧道；
"关"且从没开过的隧道不发消息——否则 1.1.0 盒子每次启动每条隧道刷一条
"消息不存在"）→ 控制台隧道表单 FEC 下拉 + 表格标签。控制面测试 5 组新增
（存取/越界/往返、bundle 携带、agent 装/去重/改档/关、老插件降级）。

**容器 lab**（`test/lab/vpp-fec/lab-fec-tunnel.sh`，双 VPP + 真 WG/SR 隧道 +
b-wan 出向 netem 10% 单向）：判据全过——

| 阶段 | 结果 |
| --- | --- |
| 无 FEC 无损基线 | 0% 丢 |
| 1:2 无损（无害性） | 0% 丢；50 组 = complete 46 + 错配 4（tap 层乱序，还原帧是重复副本被 WG 反回放吞掉，无业务影响）|
| 1:2 + netem 10% | 丢 3-4%；recovered 增量 ≥10；parity 增量 100/200 帧（精确 1:2）；bad=0 |
| 1:1 + netem 10% | 丢 0-2%；parity ≈ 帧数 |
| off + netem 10% | 丢回 9-10%；parity 增量 0（fail-open）|

unrecovered ≈ parity 丢包率（10%）×2 组——丢一个冗余包废掉当组+下一组，
计数法的固有代价，接受并已记录。

**实机**（四台插件换件，纪律照 §2.6：看门狗 + 备份 + detached）：
`scp` → `/root/sr_wg_plugin.fec1.so` → `systemd-run` 先 `qos-guard` 后
`qos-deploy`（CPE4 用 `qos4-deploy.sh`）。四台 `show plugin` 全 1.2.0，
`show sr-wg fec` 全 off（绑定活着），t9/t10/t11/t13/t14/t16 十二方向
`vppctl ping` 全 0% 丢（换前换后各测一轮，基线一致）。备份底：
四台盘上 `.bak-qos4-<HHMMSS>`（换件前的 1.1.0，md5 `c1cec62f…`）。

**实机新坑（记牢）**：CPE4 换件后 `systemctl restart vpp` 本身成功、数据面
锚点 3 分钟内全通（看门狗判 OK），但 **vpp0（管理口 lcp 对）没有跟着回来**，
ssh 从 18:46 起失联到手工整机重启为止；干净重启 30 秒内 mgmt 回来、隧道
自动重建、一切正常。教训：(1) 换件后 ssh 不回来别急着判死——**先用另一台
盒 ping 它的 WAN IP / 看 vpp.log 是否在响应 CLI**，管理口丢 ≠ 数据面丢；
(2) 看门狗锚点从 guest 数据面出，判不了管理口死活——"OK"之后仍要人工确认
ssh；(3) ssh 4 分钟不回来就 `VBoxManage controlvm reset` 整机重启，代价只有
1-2 分钟隧道重建。vpp.log 里 18:45:29-18:45:56 的 `linux-cp/router: Failed
to delete neighbor` 刷屏是 vpp0 没回来的伴生症状，可当信号用。

**没做的**：实机不开任何 FEC（造丢包不可行，§8.3）；控制台/agent/控制器
二进制还没铺到真机——四台跑的 agent 还是旧版（不会发 `sr_wg_fec_set`），
控制器还是旧版（bundle 里没有 fec_ratio 字段），整条 FEC 链在实机处于
"插件就位、管理面待铺"状态。下一棒：发新 sr-ctrl + sr-agent 后实机开 1:2
实测（需要先解决实机造丢包的问题，或靠对端限速造拥塞丢包）。

### 9.10 实机接通：管理面铺设 + t10 开启 1:2（2026-11-13 晚）

§9.9 结尾留的"插件就位、管理面待铺"这一棒做完了。

**换件（都在既定纪律里：先 `df -h`、备份 `.prev-*`、detached 重启）**

| 件 | 新代际 | 备份 | 备注 |
| --- | --- | --- | --- |
| `sr-agent`（四台） | `8cd9e311…`（10.0 MB，含 `ApplyFec`） | `sr-agent.prev-20261113fec`（= 3f237ac9 动态 weight 那代） | CPE2/3 顺手清了 11-12 之前的老世代备份，可用空间 147M→291M / 91M→235M |
| host `sr-ctrl` | `b1a4ccd1…`（FEC 全链）→ `e2a787e5…`（页面改勾选框+档位） | `sr-ctrl.prev-fec-20261113` / `.prev-fecui-20261113` | 换完 30 秒内四节点重连，delivery v46 全 `ok=true` |

**控制台改成 owner 说的形态**：隧道表单原来是"一个下拉，里面塞一个「关」"，
现在改成**勾选框「启用」+ 档位下拉（1:1…1:4）**，没勾时下拉锁死（档位保留）、
提交值由勾选框决定（勾了没选档按 1:2）。新建/编辑两处都改了，编辑表单按
`fec_ratio` 回填"勾上 + 档位"——不回填的话，编辑任意字段保存一次就会把
FEC 关掉（表单总是提交，0 = 关），这类"静默关掉"正是要在测试里钉住的：
`console_fec_test.go` 的 `TestConsoleFecToggleIsWired` 钉四面接线（两处标记、
两处联动键、提交取值、回填）。

**实机开启 t10（CPE1↔CPE4）1:2 的判据**

| 项 | 结果 |
| --- | --- |
| 生效速度 | `PUT /api/underlay/tunnels {id:t10,fec_ratio:2}` 后 **5 秒内**两端 `show sr-wg fec` 都是 `ratio 2` |
| 冗余比例 | `parity` 与组序号 1:1（seq 2786 / parity 2786）—— 精确 1:2，没有多发 |
| 业务无损 | t10 上 200+900 次 `vppctl ping` 全 **0% 丢**；同期 t16（同对端、无 FEC）也 0%，可作为"没被连累"的对照 |
| 全局 | 十二个隧道方向换 agent 前后、开 FEC 前后各测一轮，全 0% 丢 |
| 关掉（fail-open） | `fec_ratio=0` 后 5 秒内两端 `FEC off`，再打 100 个包 `parity` 计数**一动不动**，业务 0% 丢 |
| 错误面 | `bad 0  too_big 0`（冗余包既没被当坏包丢，也没有超 MTU 的隧道） |

**无损链路上也会看到 recovered/unrecovered 小幅增长**（2786 组里 22 组，
0.8%）—— 那不是"救回了丢包"，是**配对窗口错位**：无损时数据包一个不少，
`j` 不可能等于 `n-1`，所以这两项增长只能来自冗余包与它那一组数据包的
线上顺序被打乱。实测把速率拉开就很清楚：

| 速率 | 300/1000 包的组增量 | 新增 recovered+unrecovered |
| --- | --- | --- |
| 20 pps（300 包） | 229 组 | 0（完全对齐） |
| 200 pps（1000 包） | 526 组 | 10（1.9%） |

成因在 encap 这一侧：冗余包是在**整帧处理完之后**才 `vlib_buffer_enqueue_to_next`
批量入队的，所以一帧里挤进多个包时，第 N 组的冗余包会排在第 N+1 组的数据包
后面 → 对端数出来的 `j > n-1`，当组作废。代价只是"这一组的冗余白发了"，
业务永不受影响（错配最多还原出重复帧，被 WG 反回放吞掉）。

**2026-11-15 已修（`sr_wg4_encap.c` Step 15）**：不再分两批入队，而是**边处理
边排一条输出序列** —— 成员包后面紧跟"它触发出来的那个冗余包"，整帧一次
`vlib_buffer_enqueue_to_next` 走完（`out_bufs[]` / `out_nexts[]`，容量
`2×VLIB_FRAME_SIZE+64`，+64 是 `VLIB_BUFFER_ENQUEUE_MIN_SIZE` 要求的
越界读余量）。没有用 `vlib_get_next_frame` + `vlib_validate_next_frame`：
一条序列就够，还省掉"下一帧还有没有位置"的分支。

**A/B（容器 lab，同一脚本 `SO=<.so>` 换件重跑）**

| 判据 | 旧（`fe4b3fd8`） | 新（`8ef3129e`） |
| --- | --- | --- |
| 阶段 0：200 pps、1000 包、无损 | parity 500，complete **229**，错配 **271**（recovered 125 + unrecovered 146） | parity 500，complete **500**，错配 **0** |
| 阶段 1：50 pps、无损、100 包 | complete 46，错配 4 | complete **50**，错配 **0** |
| 阶段 2：1:2 + netem 10% | 丢 1%，recovered 18 / unrecovered 12 | 丢 4%，recovered 11 / unrecovered 12 |
| 阶段 3：1:1 + netem 10% | 丢 0% | 丢 0% |
| 阶段 4：关 FEC + netem 10% | 丢 11%，parity 增 0 | 丢 10%，parity 增 0 |

容器里旧版错配 54% 远高于之前真机测到的 1.9%：容器是 tap 收包、一次
`vlib_get_buffers` 能拿到几十个包，一帧里必然跨多组；真机的 `vppctl ping`
在 200 pps 下单包成帧，所以那条缺陷在真机上只是零星露头。**真机复测
（CPE1→CPE4 的 t10，200 pps × 1000 包）旧版本来就是 0 错配**，新版同样 0 ——
这次改动在真机上的价值是消除"突发/高包速时才出现"的那一半，而不是改数字。

**同日撞出的第二只雷：换件重启后 FEC 静默失效。** 换插件必重启 VPP，
`tx_seq` 是内存态、从 0 重来，而对端 `rx_last_seq` 还停在上代那个大数上
（实测 789628）→ 接收侧"陈旧即丢"的规则把**每一个**冗余包都判成陈旧：
发 1000 包 530 个冗余包**全 bad、complete 0**，业务无感，FEC 彻底空转，
一直到对端也重启为止。两处修法：

- 发送侧：状态创建时把 `tx_seq` 的基址设成**墙上时间（秒）**而不是 0；
- 接收侧：`seq` 回退**超过 8 组**（`SR_FEC_RESEQ_SLACK`）不再是"重复投递"，
  而是"对端重启换了基址"→ 丢掉半组、把这个包当新组边界重新同步。

真机两条都验过（CPE1 单侧重启，CPE4 侧计数）：正向跳（新基址更大）→
`complete` 照常增、`bad` 不动；负向跳（旧流已跑出 600+ 组，新基址更小）→
`unrecovered +2`（一次重新同步）之后 `complete` 继续增、`bad` 仍为 0。
改前同样的动作是 `bad +530 / complete 0`。

**仍然存在的边界**：冗余包外层 ToS 与 action 类别位被故意清零（走 BE），
一旦开了 QoS 整形，成员包与冗余包可能进不同 class 队列，端口调度器仍会把
它们调换位置 —— 本次消除的是节点内部这个主因，不是"任何配置下都对齐"。

**换件记录（2026-11-15，四台全换完）**：新件 `sr_wg_plugin.so` =
`8ef3129e350dc58b`（1.2.0，含 Step 15 顺序 + seq 基址/重新同步两处修复）；
换前四台都是 `fe4b3fd8…`（与仓库 HEAD 构建逐字节相同，所以"仓库 = 在跑"这条
成立）。纪律照 §2.6：`scp` 到 `/root/sr_wg_plugin.fec2-20261115.so` → 起看门狗
（CPE1/2/3 用 `qos-guard.sh`，CPE4 用它自己的 unit + `qos4-deploy.sh`）→
`systemd-run` detached 部署。四台 `result.txt` 全 `OK`、`show plugins` 全
1.2.0、12 条绑定全 usable、十二方向 `vppctl ping` 前后两轮全 0% 丢。
CPE4 的 `qos-deploy.sh` 是**另一份**（硬编码 `/tmp/sr_wg_plugin.so.qos`、
备份写死 `.bak-qos-20261112`）—— 用它换件会静默什么都不装，必须用
`qos4-deploy.sh` + `SRC=`；这一坑这次踩了，记在这里。

**CPE1 换 agent 又踩了一次 vpp0**：`kill -TERM sr-agent` 之后数据面正常、
看门狗 60 秒内把新 agent 拉起来了，但 vpp0（管理口 lcp 对）没回来，ssh 失联；
`VBoxManage controlvm reset` 后 **20 秒** ping 通、40 秒内三条隧道全部重建。
CPE2/3/4 换同一颗 agent **没有**复发（ssh 全程在线），所以这不是换件的必然
结果，而是本代 vpp0/lcp 的既有脆性（§9.9 记的 CPE4 那次是 restart vpp 触发）。
判据仍然是：**ssh 不回来 ≠ 换件失败**，先看外部 ping / vpp.log，4 分钟再复位。

**仍然没做的**：实机造丢包（§8.3 的结论没变：ACL 挂接口即崩 VPP、死地址造不出
永久丢包）。所以实机这一轮验的是**无害性 + 机制活性 + 管理面闭环**，恢复能力
仍以容器 lab 的 10% netem 判据为准。t10 现在**开着 1:2 留着当演示态**（带宽
多 50%，lab 环境可接受），控制台里随时可以关。
