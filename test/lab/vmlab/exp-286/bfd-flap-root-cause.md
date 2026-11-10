# BFD 自发抖动：根因取证与结论（#286 第二轮）

日期：2026-11-09 22:36–00:40 CST
执行：第二个 agent
前置：owner 三条授权 —— 环路交给 prefix-list、四台刷到最新、主备形状随便改

---

## 0. 一句话

**抖动不是我们的 daemon 造成的，是 BFD 的 150 ms 检测窗口在这套单线程 VPP + 桥接 WAN 的
虚机上没有余量**：包到了、对端在发，但同一条主线程上还压着 WG 加解密、tap punting 和 CLI，
任何一次 ≥150 ms 的忙碌都会被判成「对端死」。停掉 `sr-agent` 与 `sr-box-watch.timer` 之后
抖动照旧，这是本轮最硬的一条反证。

---

## 1. 这一轮先把四台刷成同一代（原来只差 `sr-agent`）

### 1.1 换代前的实测（不是看文件时间，是看二进制里的特征串）

| 件 | 盘上（四台一致） | 与 HEAD 新编对比 | 判据 |
| --- | --- | --- | --- |
| `sr-agent` | `d2ad8519` | `7a11844b` | `grep -a tunnel-state`：盘上 **0** 命中，新编 **2** —— 真漂移 |
| `sr-route` | `1161534d` | `01862ba1` | 特征串一致，哈希差异来自构建参数，仍按「同代」处理 |
| `sr-frr` | `626b126a` | `11ed0b62` | 同上 |
| `sr-fib` | `c213a6ca`（CPE1 是 `a9f1bc8c` 缺陷对照臂） | `c213a6ca` | 与新编**逐字节相同**；只有 CPE1 是有意留的对照臂 |

> 教训：**哈希对比不能单独当「哪一代」的判据**，`-trimpath` 与否就会让同一份源码产出不同哈希。
> 这一轮先用特征串定位（`tunnel-state`），再谈哈希。

### 1.2 下发过程（管理面只有 ~13.7 KB/s，这条也要单独立项）

管理流量走的是盒子的 `vpp0`（VPP 的 tap 口），实测：

- 单连接 **13.7 KB/s**，1 MB 固定 76 s；三台并行、每台 4 条连接时每连接仍是 13.7 KB/s
  ⇒ **per-connection 限制**，不是总带宽；
- 反向（盒→宿主）1 MB/s；
- 宿主一次突发 5000 个 UDP 包到盒子，盒内 VPP 的 `punt` 计数**只涨 5**、`drops` 涨 0
  ⇒ 4995 个包在到达 VPP 之前就没了。

所以 `scp`/`rsync` 在这个 lab 上不可用。改成 gz + 切块 + 6 路并行（`~/srvmlab/out-286/push-par.sh`），
每台 4–7 分钟落地，落地后逐件核 `sha256`。

### 1.3 换件（必然中断操作，逐台实测）

`~/srvmlab/out-286/install-restart.sh`：install → `SIGTERM` → `systemctl start sr-box-watch.service` 补回。
顺序按 B4 的部署硬规则：**`sr-frr` → `sr-route` → `sr-agent`**，CPE1 最后补 `sr-fib`。

| 台 | 单件 SIGTERM 收尾 | 单件中断（到底新 pid 起来） | 备注 |
| --- | --- | --- | --- |
| CPE2 | 0.79 s | **3.10–3.19 s** | 3 件 |
| CPE3 | 0.76–0.78 s | **3.11–3.21 s** | 3 件 |
| CPE4 | 0.80–0.88 s | **3.23–3.47 s** | 3 件 |
| CPE1 | 0.82–0.97 s | **3.18–3.45 s** | 4 件，含 `sr-fib` |

CPE1 换完 `sr-fib` 立刻补了隧道表：`隧道 2 条 … converged=True`（G-2 缺口，必须补）。
换完四台 `run == disk`（对的是本轮新编的那四个哈希），客户侧六方向 **0% 丢包**。

### 1.4 #275 这条链现在通了

- 日志：`route-state pusher on (sr-route /etc/sdwan/route.json)`；
- 抓包：15 秒内 `PUT /tunnel-state` 出现 **2 次**（10 s 一次）。

也就是说上一轮 S4 那个「隧道断了 `rib` 却一动不动」的现象，现在应当消失 —— 这是下一枪要量的。

---

## 2. 抖动取证：五条互相独立的证据

### 2.1 不是宿主全局停顿（跨盒子不同时）

取三台最近 400 条 down 的秒级时刻（CPE4 的日志是 UTC+8，先归一），两两交集：

```
CPE2∩CPE3 = 2 / ~250      CPE2∩CPE4 = 2      CPE3∩CPE4 = 2
```

宿主只要有一次全局停顿，四台会在**同一墙钟秒**一起掉。没有 ⇒ 单台内部的问题。

### 2.2 是「一台盒子上所有会话一起掉」

CPE2 15:13:37 两条会话同一秒 down、15:13:38 一起 up；CPE3 15:49:39 三条会话同一秒 down。
一台盒子上的 BFD 会话共享同一个主线程、同一个定时器轮 ⇒ 这是**单点**，不是某条链路的问题。

### 2.3 把我们的进程全停掉，照旧抖

| 动作 | 窗口 | down 增量 |
| --- | --- | --- |
| 原状 | 5 min | CPE1 +4、CPE2 +4、CPE3 +4~7、CPE4 **+12** |
| 停 `sr-box-watch.timer` | 4 min | 0 / 0 / +1 / +1 |
| 停 `sr-agent` **且**停 timer | 6 min | CPE3 **+4**（10–11 s 一对的节奏） |

`sr-agent` 每 10 s 一次遥测 dump 确实是可疑点（10 s 节奏对上了），但把它停掉之后抖动仍在。
**这一条就把「我们的遥测把 BFD 打下去」这个解释否掉了。**

### 2.4 抖动窗口里，包是到的

在 CPE3 上抓 `enp0s9` 上来自 CPE1（`192.168.1.101`）的 UDP，60 秒；同时故意压 3 MB 上传：

```
包数 1709，窗口 59.4 s
最大到达间隔 59.6 ms      >100 ms 的空档：0 个
同期 CPE3 的 BFD down：+4
```

BFD 是 50 ms 一发，62 s 的窗口里最长的空档只有 59.6 ms —— **对端在发、网卡在收、内核看得见**。
而 BFD 仍然判了 4 次 down ⇒ 卡在 VPP 内部：要么没收进去（rx ring），要么主线程没跑到定时器。

### 2.5 这套数据面只有一条线程

```
$ nproc          → 2
$ vppctl show threads → 只有 vpp_main（lcore 0）
```

WG 加解密、tap 的 punting、CLI/API handler、BFD 的 process node **全在 `vpp_main` 上**。
BFD 的检测不是「收到包就更新一个时间戳」，而是**定时器轮要在 150 ms 内被跑到**；
主线程被 punting/CLI/加解密占住 150 ms，会话就被判死，哪怕包一个没丢。

---

## 3. 结论与它的边界

**结论**：在这套 lab 上，`50 ms × 3 = 150 ms` 的检测窗口没有余量。
它需要「主线程 150 ms 内必定空闲」这个前提，而这条前提在四台虚机上都不成立
（单线程 VPP + tap punting + 桥接 WAN 的突发丢包）。

**边界（我不下结论的部分）**：

- 我没有证明「主线程具体被谁占住」。上面 2.4 只证明了**包到了而 BFD 没动**，
  2.5 证明**只有一条线程**。要钉死到「punting 那一刻」，得在 VPP 里加打点（本轮没做）。
- 桥接 WAN 吞突发（5000→5）是**独立的 lab 缺陷**，它解释了管理面 13.7 KB/s，
  也可能参与抖动（业务/遥测突发会挤占同一条 LAN），但它不是抖动的充分条件 ——
  2.3 那个 6 分钟窗口里我没有做大流量，抖动照样发生。

**这不是「代码写错了」，但仍然是产品问题**：默认值把可用的环境假设写死了。
`wire.DefaultBFDMinTxUS/RequiredMinRxUS/DetectMult = 50000/50000/3`（`internal/wire/wire.go:913-915`），
而这三个字段在隧道配置里**已经是可配的**（`bfd_min_tx_us` 等，`internal/underlay/settings.go:89-91`
把 ms 换成 us 下发），所以调宽只是**一次设置变更**，不改代码。

建议的下一枪（都要 owner 点头）：

1. **把 lab 的 BFD 剖面调宽**（例如 200 ms × 5 = 1 s），走控制台 → 发布 → agent 的产品路径。
   判据：同样 10 分钟观察窗口内 `-> down` 归零，且六方向仍 0% 丢包。
   代价：真实切换时间从 ~150 ms 级变成 ~1 s 级 —— 这是要 owner 拍的取舍，不是我能定的。
2. **给 tap/punt 那条路单独立项**：管理面 13.7 KB/s、突发 5000→5，这两件事都不正常，
   而且它们污染所有「从宿主打到盒子」的测量。
3. 若最终确认抖动来自 punting：产品侧要回答的是「BFD 能不能不跟数据面抢同一条线程」，
   这属于架构题，不该在 #286 里顺手改。

---

## 4. 复现要点

```bash
# 换代（产物已在 ~/srvmlab/out-286/bin，逐件 sha256 对过）
bash ~/srvmlab/out-286/push-par.sh CPE3 sr-agent sr-route sr-frr
bash ~/srvmlab/out-286/install-restart.sh CPE3 sr-agent sr-route sr-frr

# 抖动率（10 分钟窗口，只读）
for b in CPE1 CPE2 CPE3 CPE4; do
  ssh $b 'grep -c "\-> down" /var/log/sr-box/fib.log'; sleep 60
done

# 抓到达间隔（窗口内压流量）
ssh CPE3 'timeout 60 tcpdump -i enp0s9 -nn -tt -s0 "udp and host 192.168.1.101" > /tmp/wg-gap.txt'
```

判据一律用**前后状态对比**，不看退出码（`vppctl set interface state` 出错也返回 0，见 §AGENTS.md 1.2）。

---

## 5. 后续（2026-11-10，owner 拍板之后）

### 5.1 剖面改成 300ms×3 —— 顺带挖出一个真产品缺陷

owner 更正：BFD 剖面从来就是 300ms×3（检测窗口 900ms），50ms×3 不是他的意图。
落地分三步：

1. **代码默认值**：`wire.DefaultBFDMinTxUS/DefaultBFDMinRxUS = 50000 → 300000`（`internal/wire/wire.go:913`），
   `console_metrics_test.go` 的夹具同步改为引用默认值。单测在 `fix4-test` 容器全绿
   （wire / underlay / agent / sr-ctrl / vppapi）。
2. **lab 租户设置**：store 里存的是显式 `bfd_min_tx_ms: 50`，光改默认值不落 lab。
   走产品路径改掉：console 登录 → `PUT /api/underlay/settings`（50→300，console 回显
   `BFD 最小发送间隔：50ms → 300ms`）→ `POST /api/underlay/settings/publish`。
3. **发现缺陷（#287）**：publish 之后四台的 agent 全部陷入
   `bfd_udp_add 失败，VPP retval=-101` 无限退避。根因：**租户改剖面后，已在跑的
   BFD 会话收不到新定时器** —— `Ensure` 里 `hasBFDSession` 连定时器一起比，参数变了
   就直接 Add；而真 VPP 对已存在会话的 Add 一律回 `BFD_EEXIST`（-101，"Duplicate
   BFD object"），没有原地改参数的 API。单测的 fake VPP 接受重复 Add，把这条挡住了
   （tunnel_test.go:1000 那条「改定时器」用例在真机上从来走不通）。
   **修复**：`Ensure` 先按 (local,peer) 认会话（新 `hasBFDKey`），定时器不同 → 先
   `DelBFDSession` 再 Add（代价：会话回 Init、几秒内重新 Up，这是改剖面的固有代价）。
   回归测试补上「重 Add 前必须先 Del」断言（telemetry_test.go）。sr-agent 重编
   （`059baab9…`）推四台换件，单台中断 2.7–2.9s。

### 5.2 换件后的实测

- agent.log：`隧道配置 v24 已收敛`（-101 退避消失）；
- enp0s9 抓包按包长分流：探针（114B）中位 271ms；BFD 收包计数器 20/s → **4.0/s per session**
  （20/s = 50ms 旧值；4.0 ≈ 300ms 带主线程抖动的实测值）；
- 5 分钟抖动窗口：见 §5.3。

### 5.3 punt/管理面异常的根因（当晚连查，独立于 BFD）

§1.2 那两条异常（管理面 13.7KB/s、突发 5000→5）当晚续查到根因，**都在 VPP 的
管理面转发路径上，与 BFD 无关**（BFD 多跳不跨 tap）：

拓扑：vpp0 是 enp0s9 的 LCP pair，管理 IP 192.168.1.102 在 vpp0 上。
包路：Mac → enp0s9（内核，1973/2000 到达）→ VPP af-packet → L3 punt → tap4097-tx → vpp0。

- **突发丢包的真凶：`tap4097-tx no free tx slots`**。2000 包突发一次，此计数器
  **+1494**（75%），正好对应 vpp0 只收到 ~26%。tapv2 默认 TX 环（256 槽）在突发
  瞬间被写满，Linux 侧（vpp0 NAPI，跑在被 vpp_main 挤占的机器上）腾不出来。
  这些丢包**不计入接口 drops**（error counter 单列），之前从接口计数看「drops 涨 0」
  是真话，但结论「在到达 VPP 之前就没了」是错的——包进了 VPP，死在 tap 出口。
- **吞吐上限的真凶：`af-packet-input timed out block` ≈ `total received block`**
  （180.2万 ≈ 180.2万）——**VPP 收到的每一个 TPACKET_V3 块都是超时退休、从没装满过**，
  数据只能按块超时的节奏一坨一坨交给 VPP。单包穿过 VPP 只要 ~6ms（enp0s9→vpp0
  实测），但 TCP 在这种「突发必丢 + 按超时放行」的路径上只能爬到 ~13.7KB/s。
- rx-mode interrupt vs polling 实测不是主要杠杆（切换后突发表现 1456 vs 1746/5000），
  #270 的 2×2 结论（主线程落核主导）不因此翻案。

**定性**：这不是 bug，是 lab 的 VPP 接口配置形态（af_packet + 默认小环 tapv2 + GSO
关）天生不吃突发。它污染所有「宿主→盒子」的测量（scp/rsync/突发类），但不影响
客户数据面（WG 数据 VPP 内部闭环）和 BFD。要修属于 lab 基建立项：重建设置更大的
tap 环 / 开 tap GSO，需要动 VPP 配置并重启数据面。

### 5.4 punt 修复落地（2026-11-10 实测）：liblcp 环 256→4096 二进制补丁

上游实锤（VPP v26.06 源码 `lcp_itf_pair_create`）：LCP 配对的 tap 环
`rx_ring_sz = tx_ring_sz = 256` **硬编码**，无 CLI / startup.conf / API 出口，
也不能配对已有 tap。修法选了最小侵入：直接补丁
`/usr/lib/x86_64-linux-gnu/liblcp.so.26.06` 里那条合并存储指令——

```
779d: c7 84 24 8c 00 00 00  movl $0x1000100,0x8c(%rsp)   # rx=u16 256|tx=u16 256
  →  c7 84 24 8c 00 00 00  movl $0x10001000,0x8c(%rsp)   # rx=u16 4096|tx=u16 4096
```

（补丁文件 `~/srvmlab/out-286/punt/liblcp-4096.so`，sha256 `de62316ddcc225fb`；
每台原库备份为 `liblcp.so.26.06.orig286`。装库需 `systemctl restart vpp`，
underlay.sh 会自动重新接管、agent v24 自动重装隧道，实测恢复 ~6s/台。）

**验收（同口径突发打管理口）**：

| 盒子 | 2000 包突发到达 vpp0 | 5000 包突发 |
|------|----------------------|-------------|
| CPE2（补丁前） | ~26%（tap4097-tx no free tx slots +1494） | ~5% |
| CPE2（补丁后） | **100%** | 97.4% |
| CPE1（补丁后） | **100%** | - |

六方向客户流量不受影响（0% 丢包），BFD 会话 300ms 剖面保持。

### 5.5 VPP 重启连带恢复的三条坑（2026-11-10 实测，都是既知缺口的现场验证）

1. **sr-fib 惰性断线检测**：sr-fib 的 VPP api 连接只在「下一次写」时发现
   broken pipe。VPP 重启后如果 sr-fib 恰好空闲，它不重连、FIB 里远端路由缺失，
   且无任何日志（CPE4、CPE1 都中）。触发方式：`kill -TERM sr-fib`（watch 拉起）
   或一次 `PUT /config`。**建议立缺陷票**：sr-fib 应对 api.sock 断开做主动探测。
2. **G-2（sr-fib 隧道表产品缺口）**：sr-fib 重启后从 fib.json 启动（tunnels=[]），
   隧道表必须靠 lab 脚本 `vm-fib-sync.sh` 重推（产品里没有维护者）。重启 sr-fib
   的每台盒子都要补一发 `vm-fib-sync.sh <node>`。
3. **agent 重接管用了脏 MAC（CPE1 中招）**：lcp-sync 会把 tap MAC 反写到内核
   netdev；agent 重启 VPP 后重建 af_packet 用的是内核现值（ca:a3:04:12:a7:f8），
   不是烧录值（08:00:27:9f:f9:1f）。LAN 邻居 ARP/交换机表与新 MAC 冲突 →
   单播间歇全断、OSPF 卡 ExStart（组播 Hello 能过，DBD 单播不通）。
   修法（takeover-if.sh 头注释早有预警）：`delete host-interface name host-enp0s8`
   → `ip link set enp0s8 address <烧录值>` → 重跑 `takeover-if.sh enp0s8 lcp-x0 <烧录值>`。
   修复后 OSPF 双邻居 Full、CPE1↔LAN 0% 丢包。**建议立缺陷票**：agent 建口时
   MAC 必须取配置值并对内核做校正，而不是信内核现值。
