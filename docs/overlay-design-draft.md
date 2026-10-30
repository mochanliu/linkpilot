# overlay 与路由面 v4 设计（全局配置 / 导通 / 路由 leak / 隧道颜色 / 站点 HA）

2026-10-30。本文取代 10-28 那版草案：那版的 §2.3（策略落在 RR 还是设备）与"导通规则成对"
两条已被用户的模型（RT 组）和拍板结论替代。思科侧取证见 `cisco-vpn-leaking-research.md`。

**状态：方案定稿（§11 十条拍板记录，无待决项），实现路线 = 重构我们自己的 `internal/route`，
不改 GoBGP（§6）。站点 HA 在 §13（结论：不加机制、加一条同站点过滤器 + VRRP 配置项）。
开工顺序在 §12。O1 已落地（vppapi Weight/Preference + fib Tunnel Color + delete-then-add，
单测全绿 2026-10-30）。** #140 那个绑定键缺陷独立于本文，
仍单独等点头。

`R/` = 仓库根目录。本文里"实测"两个字只用于我在 lab 真机上跑过并留下输出的条目。

---

## 1. 范围与三句需求

用户要的真机语义四句，对到本文的四个章节：

| 需求 | 章节 |
|---|---|
| 配好哪些 VPN 之间能通（跨站点），RT 只在内部可见 | §3.2 导通设置 |
| 通的时候哪些前缀允许跨、往哪侧发布（静态/直连/bgp/ospf ↔ 无/bgp/ospf） | §3.3 路由 leak |
| 同一对 CPE 最多 8 条隧道，按颜色编号，供以后 SD-WAN policy 按 SLA 引流 | §4 颜色与装表 |
| 某站点买两台设备做 HA，怎么在咱们的模型里配出来 | §13 站点 HA |

已定：只要**跨站点** VPN 互通（不做"同机跨 VPN"这种本地互导）；VPN 号 **8 位够用**
（不加宽 uint16）；**RT 对用户不可见**，是控制器内部的编码；**导通规则不建隧道**；
**装表决策在设备侧**；**组网（导通）规则必须可编辑**（增减设备、改 VPN）。

---

## 2. 本轮实测到的事实（决定实现方式）

### 2.1 VPP 原生支持"按 preference 选路 + 自动回落"（逃生开关的地基）

头文件：`/usr/include/vnet/fib/fib_types.h:594-600`（lab-cpe1 内）——

```
/** A path preference. 0 is the best.
    Only paths of the best preference, that are 'up', are considered for forwarding. */
u8 frp_preference;
```

**0 最优，u8**。在 lab-cpe1 的临时表里用真隧道口（wg0→192.168.99.2、wg1→192.168.100.2）
实测，`show ip fib` 的 `buckets:` 就是结论：

| 场景（一条前缀，多条路径） | 结果 |
|---|---|
| pref 0 + pref 5，同一张新表一条命令下发 | `buckets:1`，只有 pref 0 参与转发 |
| pref 0 + pref 0 | `buckets:2`，ECMP |
| 把 pref 0 那条的接口 `set interface state down` | **立刻**变成 `[1] mpls via 192.168.100.2 wg1`，pref 5 顶上 |
| 再把接口 up | 自动回到 pref 0 那条 |

这正是用户要的逃生语义：**颜色=preference，颜色 0 最优；颜色 0 全 down 时 VPP 自己按
"存活的最小颜色"顶上，恢复后自动回落**，不需要我们在控制面算回落。

### 2.2 【2026-10-30 改】原地替换重不重算 preference，取决于走的是 CLI 还是我们的 API

原先这一节写的是"原地替换**不会**重算"，那条实测是**手工 vppctl** 量出来的，结论套到
`Applier` 上套错了。今天把两条路分开重测（node1，表 100，wg0→192.168.99.2 / wg1→
192.168.100.2，`fib-dump` 读回 preference）：

| 下发方式 | 动作 | 读回的 pref | `buckets:` |
|---|---|---|---|
| CLI 分两条 `ip route add ... via X preference 0` | 建出 ECMP | 0/0 | 2 |
| CLI 一条 `ip route add ... via A preference 0 via B preference 5` 整条覆盖 | **纯空操作** | 仍 0/0 | 仍 **2** |
| sr-fib 的 `IPRouteAddDel(is_multipath=0)` 覆盖同两条身份、只把 B 改成 233 | **重算** | 230/233 | **1**（只有 230 参与转发） |

第三行就是 §9 的 O2 判据里那次观测：`test-ecmp-color.sh` 第 7 步杀掉正在跑的 sr-fib，
改 `fib.json` 里第二条隧道的颜色，用 `-once` 重新下发。新进程的 `prev` 是空的，所以走的
是**普通 add**（不是先删后加），结果 bucket 从 2 自己变成 1、改色的那条退出转发但仍在
路由里存着 —— 这正是"备份颜色"该有的形态。

**【2026-10-30 O3-b 之后这一段的前半仍然成立，后半换了形状】**：逃生关时加载器不再把非 0 色的
路径留在主表里"当备份"，而是按 §4.3 第 5 步**搬进颜色表**（`test-ecmp-color.sh` 现在是 29 条断言，
第 7 步断言主表 100 只剩 wg0 且 `192.168.100.2` **不在主表**、颜色表 13100 里那条 pref=233 且同样
压标签 1100）。"整条替换会重算"这条结论本身没被换掉：第 8 步把颜色改回 0 重启，主表自己回到
`buckets:2` 且 wg1 那条读回 pref=230 —— 新进程一次普通 add 就把 pref 写回去了。

前两行的解释：CLI 拿到多个 `via` 时走的是**往已有 load-balance 里并路径**的分支，身份
没变就整个跳过，preference 连存都没存进去；我们发的是 `is_multipath=0`（见
`controller/internal/vppapi/fib.go:175,195`，注释写的就是"整条替换"），VPP 把整条路由
换掉，换完必然重算选路。

**所以 §11 拍板 #2 那条"改颜色必须先删后加"的前提没了** —— O1 已经按它加了规则
（`controller/internal/fib/fib.go:348` 的 `pathIdentityEqual` 分支），实测证明这条
分支在真实下发路径上是多余的，代价是改色瞬间有一个**真空窗**（路由整条消失再回来）。
**2026-10-30 用户拍：留着** —— 原话"改颜色会丢包属于正常应该接受的。实现它即可"。
⇒ 规则保留，注释已改成引用这一节的**重测结论**（不再谎称"实测必须删加"）。

不受影响的一条：路径集合本身变化（1 条 ↔ 2 条同 pref）原地替换一直是正确的，BFD 翻转
走的 #40 那条 e2e 就是这个路径。

### 2.3 vppapi 层要补的东西（已知缺口，不是未知风险）

- `FIBPath` 没有 weight/preference 字段，`appendFIBPath` 把两个字节**硬编码成 1/0**
  （`R/controller/internal/vppapi/fib.go:101-107`）；`readFIBPath` 也**不读回**
  偏移 12/13（`fib.go:336-353`）。→ 颜色落地时：加 `Weight`/`Preference` 两个 u8 字段，
  编码写实际值，解码读回来（否则核对下发结果时看不见颜色）。
- 读回不带上 preference 会让"下发成功"和"数据面真的按颜色选"这两件事分不开，
  所以核对必须走 `FIBRoutes`（§9 的负向测试依赖它）。

### 2.4 标签与表必须解耦（现有实现的真缺陷，用户已独立复述正确规则）

`R/controller/internal/fib/fib.go:186-187`：

```go
k := key{table: uint32(r.VRF), prefix: prefix}
grouped[k] = append(grouped[k], Path{... Label: LabelFor(r.VRF)})
```

表号和标签**都**取自这条路由自己的 VRF 字段。导通之后这句话不成立：
「不管是哪个表要导入这条路由，都应该压来源 VPN 的标签」——标签是**远端语义**
（对端弹栈后按标签查的是它自己那张真实所在的表，`mpls_route` 是全局表、不按本端 VPN
解释），表号是**本机语义**（可以有 N 张）。两个后果：

1. 不能靠改写 `vrf_id` 实现导通（改了就压错标签 → 对端黑洞）。
2. **同前缀不同来源 VPN 的路径不能 ECMP**：现在的 `{表,前缀}` 分组会把它们合成一条多路径
   路由，VPP 照单全收成 `buckets:N`，于是流量按比例被送进错误的远端 VPN 表——**静默投递**，
   比不通难查得多。§5 的 RD 与 §4.3 的"标签同组才能进同一个桶"就是修这一条。

### 2.5 收敛与 HA 的现状（本轮核对代码所得）

| 事实 | 出处 |
|---|---|
| RR 侧隧道变化会**立刻增量重推**（diff 出 gained 的对端后只推那些设备的路由） | `R/controller/internal/route/reflector.go:435-486` |
| CPE 侧隧道表**只在启动时读一次**；被闸丢掉的路径**不留存** | `R/controller/cmd/sr-route/main.go:46,159-160`、`client.go:726-751` |
| ⇒ 今天"后补隧道"要靠设备重启 `sr-route` + `sr-fib` 才重新学到路由（用户问的这条，答案是 RR 侧没问题、**CPE 侧不行**） | 同上 |
| 双 RR 已有：`pickActiveLocked` 只允许 **EOR 完成（synced）**的会话供表，主备切换即换活动会话 | `client.go:677-688` |
| 新会话顶替旧会话时会清掉旧会话的路由与隧道表，等新快照重来 | `reflector.go:197-208` |
| `watchRIB` 2s 只是**打日志**，不触发任何重算 | `R/controller/cmd/sr-route/main.go:190-208` |

### 2.6 其它（承接上轮）

- BFD 可达性是**按隧道 /32** 判的，不是按节点：`R/controller/internal/fib/reach.go:20-30`
  只收 multihop 会话、按 `s.PeerIP` 建集合，`fib.go:176-185` 据此逐路径跳过。
  → 颜色不需要动这个判据，多条同色隧道天然各自判活。
- 每隧道两个 overlay /32：`R/controller/internal/underlay/tunnel.go:145,152`（删除时
  归还两个 /32，中转端口不归还 `:273-275`）。
- lab-cpe1 当前 IPv4 表：**0 / 10 / 99 / 250 / 1250**（250=overlay，1250=FRR table-direct）。
  §4.2 的表号公式必须避开这些，实测已避开。

### 2.7 FRR 同步进来的路由 vs 我们 API 下发的路由，谁在转发（2026-10-30 实测，lab-cpe1）

**先给复现方法**（上一轮我误判成"读不到 preference"，其实能读能写，**不用改代码**）：

```
vppctl 侧   show ip fib <前缀>            ← 给的是**按来源分块**的视图，这是本节判据
vat2  读     vat2 ip_route_dump '{"table":{"table_id":0,"is_ip6":0,"name":""}}'
vat2  写     vat2 ip_route_add_del '{"is_add":true,"is_multipath":false,"route":{...}}'
                                          ← route 对象直接从 dump 的输出回灌即可
```

`table` 是 union，必须写成嵌套对象（`{"table":0}` 会报 Failed converting JSON to API）；
每条 path 里带 `weight`/`preference`/`type`/`nh`/`label_stack`。这个 build 里 **没有 `show ip route`**。

| 测的什么 | 实测结果 |
|---|---|
| 盒内 FRR 学到的路由在 VPP 里的 preference | **20**（BGP eBGP 学到的 `10.68.1.0/24`、`172.16.1.1/32`；table 1250 里 zebra 的 blackhole 也是 20） |
| 20 从哪来 | **`linux_nl_plugin` 把内核路由的 `RTA_PRIORITY` 原样抄进 `fib_path.preference`**。我用 iproute2 直接加 `metric 33 / 200 / 240` ⇒ VPP pref 依次是 33 / 200 / 240，逐一对上 |
| **改 FRR 的 distance 会不会挪动它**（用户这条假设） | **不会，否证。** 静态路由 distance 177、distance 5、`distance bgp 77 77 77` + route refresh，**内核 metric 全是 20、VPP pref 全是 20**；`/etc/frr/*.conf` 里没有任何 metric/distance 配置。⇒ 盒内 FRR 装的路由 metric 是个**与 distance 无关的常量 20**（来源我没解释清楚，但取值稳定） |
| metric > 255 | **u8 回绕，不是饱和**：内核 `metric 300` ⇒ VPP pref **44**（300 mod 256）。⇒ 我们的 `base+颜色` 必须显式断言 ≤255 |
| 同 (表, 前缀) 上两个来源谁赢 | **不比 preference，比来源优先级。** FIB entry 按来源分块，只有最高优先级那块标 `contributing,active` 在转发，其它块 `added` 挂着完全不参与。实测：我们 API 侧写 pref=230、内核侧 metric 拉到 240（比 230 更差）⇒ **仍然 lcp-rt 转发**。实测来源次序：`lcp-rt`(FRR) > `API`(我们) > `CLI` |
| FRR 那条撤了会怎样 | API 那块**立刻自动**升为 `contributing,active`，`buckets` 切到 wg 口；内核路由加回来 ⇒ lcp-rt 立刻夺回。**两个方向都不需要控制面做任何事** |

⇒ **对 ③ 的结论（用户提的 230 方案，结果对、机理要换成下面这条）**：
"本地 FRR 路由压住 SD-WAN 学到的同一前缀、FRR 一失效 SD-WAN 自动接管" = **VPP 原生的
source priority 分层**，与 preference 取值无关（230 压不住，是来源在压）。于是：

1. **不需要** loader 侧"丢弃本地来源前缀"的规则，§8 原来那条"显式拒绝并说明冲突"改成
   **允许共存 + 记 warning**（VPP 已经决定谁转发，我们不该再顶掉一份，否则 FRR 路由消失时
   没有备份顶上）。
2. `preference = 230 + 颜色` 仍然按用户口径写（§4.3），但它的**语义要如实改写**：不是"比 FRR 大"，
   而是"在我们自己这一个来源（API）内部给颜色排序留出与 metric 20 不冲突的头部空间"。
   我们所有路由都来自 API 一个来源 ⇒ preference 在这一来源内**确实生效**（§2.1 已测）。

### 2.8 隧道口被"删掉再建"之后，`set interface mpls enable` 是静默空操作（2026-10-30 实测，卡住 O2 半天）

O2 的两条并行隧道在 FIB 里形态完全正确（`buckets:2`、两条 pref 230），**真流量却不通**：
node2 的 wg1 收端把带标签的内层包全丢。根因不在我们代码里：

- VPP 26.06 `vnet_interface_mpls_enable_disable()`（`src/vnet/mpls/interface.c`）按
  `sw_if_index` 记一份引用计数，**只在 0↔1 翻转时**才动 feature 弧
  `vnet_feature_enable_disable("mpls-input", "mpls-not-enabled", ...)`。
- 这个计数在**接口删除时不清零**，而 `sw_if_index` 会被复用 ⇒ 新建的那条隧道一上来就
  "计数已经 >0"，`enable` 不再翻弧，弧上停的是丢弃路径 `mpls-not-enabled`。
- CLI 不会报错（`cmd_wg_create` 的输出式检查也抓不到），所以表现为"配置都对，流量黑洞"。
- 只有**重启 VPP** 能清掉计数；`disable` 调多少次都清不掉（我把 node2 wg1 的计数手抖到
  8，`show mpls interface` 一直在）。

判别方法（`test-ecmp-color.sh` 第 0 步已经把它做成前置断言）：

```
show interface features <wg>     # mpls-input 弧下面是 "none configured" 才对，
                                 # 出现 "mpls-not-enabled" 就是这条口是被重建的，收端必丢
```

**`show mpls interface` 不能用来判别**（2026-10-30 对照组实测）：健康的 wg0/wg1 在它那里
同样是 `MPLS enabled`，而在 `show interface features` 里弧上是 `none configured`。
⇒ 前者读的就是那份残计数，它说"开了"不等于弧真的在收；只有后者是真相。

**对照实验（同日，真 VPP 26.06，lab-cpe2）**：同一张临时 loopback 上把两件事各做一次，
差别只有"删口前有没有 disable"，其余全同（同一个 `sw_if_index` 7、同样新建后 enable）：

| 步骤 | B) 先 disable 再删 | A) 直接删 |
| --- | --- | --- |
| 新口 enable 后弧上 | `none configured`（可用） | `mpls-not-enabled`（**空操作**） |
| 同时 `show mpls interface` | 列它 enabled | **照样列它 enabled** |
| 反复 disable/enable 三次 | — | 还是 `mpls-not-enabled`（救不回来） |

两段的"基准"（第一张口 enable 之后弧上可用 = true）都过了，所以这个号在实验前是干净的 ——
这不是"号本来就坏"，是**上一任没 disable 就交出去的**。实验程序是临时的，跑完就删了。

⇒ 顺带测出**反向的同一个坑**：对一张**从没 enable 过**的口敲一次 `disable`，计数被打到 0 以下，
下一个持有者 enable 时照样不是 0↔1 翻转 ⇒ 一样静默空操作（在 lab-cpe1 上用真 WG 接口撞到过一次）。
所以"disable 只在 enable 之后成对出现"不只是整洁问题，**乱敲 vppctl 也能给下一个租户下毒**。

**证据强度要如实写**：上面 `src/vnet/mpls/interface.c` 那两条是早前一轮读 VPP 源码得到的机理
判断；本轮在实验室镜像里 `find` 过 `vpplab` / `srwg-node1` / `lcp-probe` 三个容器，
**都没有 VPP 源码树**，所以我没法在这里逐行复核。上面表格里那些是**今天实测到的症状**，与那个机理
判断一致但不由它证明。

⇒ **这不只是 lab 脚本的坑，我们的生产路径天天在踩**（2026-10-30 核对代码）：
`agent` 建完隧道在 `finishOverlay` 里无条件 `SetInterfaceMPLS(..., true)`
（`controller/internal/agent/dataplane.go:784`），
撤隧道走 `DeleteInterface`（同文件 `:1338`）**曾经没有先 disable** ⇒ 撤一条再建一条、
`sw_if_index` 被复用，新隧道就开不了 MPLS。而 `SetInterfaceMPLS` 的注释
（`controller/internal/vppapi/interface.go:101-105`）早就写了少这条的表现是
"能 ping 通、跑不了 VPN 流量" —— 正是这个坑的现场症状，overlay 纯 IPv4 不受影响所以最容易误判成好了。
现场照片（同日）：lab-cpe1 上 agent 建的 wg1001（idx 6）至今是 `show mpls interface` 列它
"MPLS enabled"、弧上挂着 `mpls-not-enabled` —— 同机跑 `tools/mpls-arc` 与 `vppctl` 对拍出来的。

⇒ **对策：(c) 两条都做，已落地（#145）**
- (b) **删口前先 disable**（`teardownTunnel`，`dataplane.go:1328-1336`）：disable 失败就**不删口** ——
  宁可这条暂时撤不掉，也不能把残计数交出去。
- (a) **开完向弧本身核验**（`finishOverlay` → `vppapi.Client.MPLSEnabled`）：弧上还挂着
  `mpls-not-enabled` 就报错、拒绝装路由，并把"这个号是从没 disable 就删掉的口继承来的，
  只有重启 VPP 能清"写进错误里，不给它变黑洞的机会。

**读弧为什么走 CLI 而不是 API**：`feature_is_enabled` 这条二进制 API 在这份 VPP 26.06 上
**永不回帧**（同日实测：它的 CRC 与我们注册表一致，`checkVersions` 从没报过它 —— 不是版本对不上，
可 local0/wg0/wg1/wg1003 四次请求全部 4.003s 超时；lab-cpe1 里 `vat2` 又载不上 feature 模块，
连旁证都拿不到）。所以 `MPLSEnabled` 用 `cli_inband` 读 `show interface features <idx>` 的
`mpls-input` 段，判据与 harness 第 0 步同一条；注册表里那两条 CRC 已删，拨号不该校验一条用不上的消息。
`cli_inband` 会把错误吞成 retval=0，所以"输出里没有 mpls-input 段"当**错误**返回，绝不当"没开"。

**验证（同日都过）**：`internal/vppapi` `TestMPLSEnabledReadsFeatureArc` 5 例，样例取的是 lab-cpe1
上 wg1001 的原样输出；`internal/agent/dataplane_mpls_test.go` 4 例（撤隧道里 disable 必须排在删口前、
复用同一个号重建后弧是干净的、继承了残计数就拒绝开隧、disable 失败则不删口），假数据面把 VPP 的
引用计数字性照抄进 `fakeVPP.mplsCount`（跨 `DeleteInterface` 存活、只在 0↔1 翻弧），所以这四条是真回归。
**变异测试**：把 `teardownTunnel` 里那句 disable 改成 enable，三条立刻变红，报错正是那句诊断。

**lab 里留下的坏号**（这些实验的代价，别当成新 bug）：**按"当前占着这个号的接口"读，别按号记** ——
残计数跟着 `sw_if_index` 走，号一释放再被占，坏的就是那张新接口。本轮（2026-10-30 晚）用新判据
`show interface features <idx>` 对三台 CPE 复测：

| 盒子 | 干净的隧道口 | 弧上还挂着 `mpls-not-enabled` 的 |
| --- | --- | --- |
| lab-cpe1 | wg0(4)、wg1(5)、wg1003(7) | **wg1001(6)**（就是上面那张现场照片） |
| lab-cpe2 | wg0(4)、wg1(5) | **wg1001(6)** |
| lab-cpe3 | wg0(4)、wg1(5) | 无（这台没跑 agent，隧道是 10-26 手工配的） |

实验用的临时号（lab-cpe2 的 7、lab-cpe1 的 10/11/12）此刻都已释放，`show interface features` 会回
`unknown interface index` —— 这**不等于**号被洗干净了，只是暂时没人占。上面两个 6 号只有重启该盒 VPP
才清得掉；真撞上了会看见 agent 报"隧道接口 N 开不了 MPLS" —— 那是检测在工作，不是又一个新坑。

（顺带记一条**与本坑无关**的 lab 现状：`test/lab/verify.sh` 今天 289 通过 / 15 失败，15 条全是
site3 相关的流。复测到的根因是 lab-cpe3 那台盒子过期：清掉计数后从 term2 打 4 个包到 term3，cpe3 收端
`show sr-wg stats` 是 `Decap ok=0 / hmac_fail=6`；反向 cpe1 能解封装（`Decap ok=5`）、但内层包止于
`null-node blackholed`。两边 `show sr-wg keys` 都只有 `hmac_id=1 active=1`，而 cpe3 那份是 10-26
手工配的、之后 cpe1/cpe2 由 agent 重新下发过 ⇒ **最可能是同一个 id 已不再是同一份密钥**（密钥本身
读不出来，这句是推断，不是实测）。属于 lab 供给状态问题，另开一条处理，不是 #145 的代价。）

---

### 2.9 `ip_route_dump` 是**表作用域**的，而空表里天然有 5 条 VPP 自造条目（2026-10-30 实测）

`sr-fib -reset` 要"只撤自己管的那几张表"，靠的是先 dump 再判空。判空之前必须先知道 dump 的
语义，否则计数会骗人（第一次看到 `reset 撤 routes=14` 时我以为多删了别人的路由）：

| 问的什么 | 实测结果 |
|---|---|
| dump 带 table 字段会不会串到别的表 | **不会**：表 100 有路由、主表 0 有一堆 overlay 路由时，dump 100 回的全是 `table=100` |
| 表号根本不存在 | **零帧、retval 0、不报错** |
| 表存在但没人装过路由 | **不是空的**：VPP 自动带 5 条 special（`0.0.0.0/0`、`0.0.0.0/32`、`224.0.0.0/4`、`240.0.0.0/4`、`255.255.255.255/32`），下一跳读回 invalid、`preference=0` |

⇒ `len(routes)==0` 只是"**这张表不存在**"的可靠判据（reset 就用它跳过没建过的颜色表）；
"这张表里有没有**我们的**路由"必须看 `preference`（230..237）或前缀，**不能拿条目数当数**。
所以 `reset 撤 routes=14` = 2 张表 × (5 条自动 + 2 条真装的)，不是多删；写进
`internal/vppapi/fib.go` 的 `FIBRoutes` 与 `internal/fib/reset.go` 的注释里钉住。

---

## 3. overlay 配置模型（用户的三子项，字段级）

沿用基础配置那套「草稿 → 显式发布 → 心跳拉取（agent -apply）」，不新增下发通道。

### 3.1 全局配置

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `rr_primary` | addr:port | 控制器侧默认（本租户 RR） | 主 RR |
| `rr_backup` | addr:port | 空 | 备 RR；空=单 RR，控制台必须显式写"未配置备 RR" |
| `hold_sec` | int | 30（`proto.go:53-55`） | **租户维度**可调；范围 10..900 |
| `keepalive_sec` | int | hold/3 | 快照后的增量节奏 |
| `settle_ms` | int | 500 | sr-fib 侧"一批变化攒成一次收敛"的窗口，**不可降到 0**（既有实测结论） |
| `escape_all_colors` | bool | **false** | §5.2 逃生开关；**作用域=租户级**（用户拍），不做逐规则粒度。设备侧落点 = `sr-fib` 配置文件里的同名键（O4 已实现，缺键即按 false 跑；计时见 §5.3） |
| `vrrp` | 列表（逐 LAN 口） | 空 | §3.1.1 LAN 侧首跳冗余；**2026-10-30 用户拍：做，配置项放基础配置** |
| `site_peers` | [device_id] | 空（单机站点） | §13.3 同站点过滤器要用；控制器按 `node.site_id` 自动算，**租户不用配** |

`hold/keepalive/settle` 三个都是"BGP 的 timer"这一项的展开，控制台合成一块，
说明文字写清各自管什么（心跳超时 / 增量节奏 / 收敛攒批）。

#### 3.1.1 VRRP（站点 HA 的 LAN 侧首跳，§13）

设备侧 `vrrp_plugin.so`（VPP v26.06，VRRP v3 / RFC 5798）**已装已加载**（`show plugins` 第 90 条），
之前是我们落了没接的一节 ⇒ 现在接进基础配置。

| 字段 | 类型 | 约束/默认 | 落到 API |
|---|---|---|---|
| `interface` | 设备上的 LAN 侧口名 | 必填，逐口一条 | `sw_if_index`（agent 现成的 `DumpInterfaces` 映射） |
| `vr_id` | int | 1..255 | `vr_id` u8 |
| `priority` | int | 1..254；**主 200 / 备 100 建议默认**，备必须**开 preempt** | `priority` u8 |
| `interval_ms` | int | VPP 侧 u16，ms | `interval`；先按插件默认，不自己发明 |
| `preempt` | bool | 备 true / 主 true | flags bit `VRRP_API_VR_PREEMPT=1` |
| `accept` | bool | VR 地址不在本口子网内时要 true | flags bit `VRRP_API_VR_ACCEPT=2` |
| `unicast` | bool | false（组播） | flags bit `VRRP_API_VR_UNICAST=4`；**组播能否在客户 LAN 上跑要现场确认**，不行就退 unicast |
| `virtual_addr` | addr/prefix | 客户内网网关地址 | `n_addrs + addrs[]`；IPv6 走 bit `IPV6=8` |

API 面（`/usr/include/vapi/vrrp.api.vapi.h` 实测存在）：`vrrp_vr_add_del` / `vrrp_vr_update` /
`vrrp_vr_start_stop` / `vrrp_vr_dump` / `vrrp_vr_track_if_add_del`。
**这个 build 的 VRRP CLI 不可用**（`vrrp`、`show vrrp`、`show vrrp brief/all` 全部 unknown input），
所以只能走 API，与 `internal/vppapi` 的既有做法一致。

- `vrrp_vr_add_del` 的 payload 里 `addrs[0]` 是柔性数组 ⇒ 我们自己的 vppapi 要按 `n_addrs` 编码尾部。
- **待实测**（不写进判据就不算数）：① VR 地址是否要同时进 VPN 表（VRRP 的 v4 地址与 SR/MPLS 装表共存）；
  ② 主备切换时我们的 overlay 路由是否受影响（不应该受：VRRP 只换 LAN 侧首跳 MAC）；
  ③ 双机做 VRID 相同的实验需要 lab 第二台盒（现在 lab 只有一个 CPE 在 VPN0 里有 LAN 口），排在 §13 之后。

### 3.2 导通设置（RT 组）

一条规则 = 一组设备 + 每设备一个 VPN：

```
MeshGroup {
  id        控制器生成
  name      租户内命名，可改
  rows[]    { device_id, vpn_id }   // 一个设备一行，vpn_id 是下拉菜单
  enabled
}
```

- 添加/编辑时，控制器给这一组分配**一个 RT 值**（内部编码，用户不可见、不可填），
  写进**每台设备的这个 VPN 的 import 和 export**。
- 一台设备的一个 VRF 命中多条规则 ⇒ RT **叠加**（RT 本来就是数组，`Route.RTs []string`）。
- 天然双向：A 的 export=RTx 且 B 的 import=RTx，反之亦然 ⇒ 一条规则就够，
  不需要"反向第二条"。**编辑（增减设备、改 VPN）后必须重算 RT 集合**：
  移出的设备那一行要把它不再持有的 RT 从 import/export 里摘掉，否则残留准入。
- RT 值的分配：租户内单调计数（`<tenant-ASN>:<n>` 形式，但只作为字符串键，不对外解释）。
  控制台不提供 RT 输入框；`sr-route` 配置文件里仍然如实写 import/export RT，便于排障。

### 3.3 路由 leak 设置

| 方向 | 源 | 目标 | 过滤 |
|---|---|---|---|
| 内侧 → SD-WAN | 静态 / 直连 / bgp / ospf（多选） | — | prefix-list（选填） |
| SD-WAN → 内侧 | — | 无 / bgp / ospf（单选） | prefix-list（选填） |

- 「发布到**无**」= 只留在本机 VPP 表里，不进 FRR、不再往客户内网通告。
- 过滤只在 leak 侧（prefix-list 只 permit、只精确 CIDR；`le/ge` 不做）。
- 环路只在盒内防：SD-WAN 侧带一个 tag（§6 的 origin tag），"sdwan→内侧"发布时
  **拒收该 tag**；客户内网自己配错的环不属于本项目责任范围（用户明确）。
- 「内侧→SD-WAN」的 connected 来源今天已经天然成立（linux-cp 把 VPP 地址同步进内核，
  盒内 FRR 能看到 connected），但**未实测**：排在 UI 之后做一次实测（§9 的 O2 判据）。

---

## 4. 隧道颜色与 RIB→FIB

### 4.1 颜色规则

- 同一对 CPE **最多 8 条隧道**，颜色 **0..7**，per-pair 配置，**手动可改**。
- 批量/单条创建时**默认全是颜色 0**；颜色 0 允许多条 ⇒ 天然 ECMP。
  （放弃"1、2、3 流水排"：我们不按介质区分隧道，同介质不同路径也要能区分颜色。）
- 第 9 条一律拒绝，提示"一对 CPE 最多 8 条隧道"。
- 颜色是 **underlay 隧道属性**：`隧道配置` 加一列颜色下拉，与"有序中转跳"并列。
  协议**不为颜色加字段**——每隧道已有独立 /32 下一跳，颜色在设备侧本地查隧道表就拿到。

### 4.2 表号公式

```
表号 = 颜色 * 1000 + 10000 + 主表号        主表号 ∈ 0..255
```

- 颜色 0 → 10000..10255，颜色 1 → 11000..11255，…，颜色 7 → 17000..17255；
  色间留 1000 的空隙 ⇒ 8 个色段互不重叠。
- 与既用的 0 / 10 / 99 / 250 / 1250 **实测不冲突**，也在 FRR table-direct 之下、
  与 VPP 主表 0 无关；标签空间 `1000+vpn` 是**另一个命名空间**（MPLS 20 位标签），
  数值形似但无交叉。O3 的 lab 用例用的主表 40/41/42 也照这条公式得到 10040/10041/10042，
  与 lab 既有表号同样不撞。
- 只有真正被装进路由的 (色,主表) 组合才建表：**不是** 8×256 张表全开。
- 「主表」= `主表号`（VPN 自己的表）；"颜色 c 的表"= 上面公式。

### 4.3 装表算法（每台 CPE 本地，一次收敛）

```
对活动会话的 RIB 里每条 (来源设备 D, 来源 VPN S, prefix P, rts[]):
  1) 本机哪些 VRF 的 import_rts 命中 rts[] → 目标主表集合 T      （RT 匹配在设备侧）
  2) 对每个 t ∈ T，**不要**把本地来源（盒内 FRR）已经有的前缀顶掉：同一 (t, P) 允许两份共存，
     转发的谁由 VPP 的 source priority 决定（§2.7），我们只在计划里记一条 warning
  3) 候选路径 = 本机到 D 的隧道（每条一个下一跳，带颜色 c、BFD 活/死）
     标签恒为 LabelFor(S)                                        （远端语义，与 t 无关）
  4) 只把**同标签**的路径放进同一个桶；不同来源 VPN（不同标签）的同前缀候选
     按 §6.4 的 BGP 决策链择一（非胜者留在 adj-RIBs-in，不丢），并记 warning
                                                              （修 §2.4 的静默投递）
  5) 逃生关：主表 t 只收颜色 0 的路径；每个颜色 c 的表 t_c 只收颜色 c 的路径
     逃生开：主表 t 收全部颜色的路径，pref = 230 + 颜色；颜色表 t_c 同上
  6) 写 VPP：IPRouteAddDel(table, prefix, paths(带 pref), add)   （§2.2：整条替换会重算）
```

`preference = 230 + 颜色`（用户 2026-10-30 拍的写法）。`230` 是**固定基址**，作用是让我们这一来源
内部的颜色排序与 FRR 同步进来的 metric 20 不撞车；**它不是"压过 FRR"的手段**——压过 FRR 的是
source priority（§2.7 实测：230 vs 240 仍然是 FRR 转发）。

**硬约束（实测得出，必须写进单测）**：`frp_preference` 是 u8 且 VPP 不做饱和 ⇒
`230 + 颜色 ≤ 255`（颜色 7 时 237，安全）。任何以后想抬基址的改动都要重新算这条，
否则 `230+颜色` 会回绕成小数字（内核 `metric 300` ⇒ VPP pref 44 就是这个坑），颜色排序直接反过来。

`preference` 落在 §2.1 的原生语义上：颜色 0 全 down 时 VPP 自己抬最小颜色。

**2026-10-30 O3 实测（`test-cross-vpn.sh` 29/29、`test-ecmp-color.sh` 29/29）**：第 3 步"标签恒为
`LabelFor(S)`"、第 4 步"不同标签不能同桶 + 按 RD 择一"、第 5 步逃生关的"改色即搬表"都在真 VPP 上
成立 —— 装在表 40 的跨 VPN 路由压的是 **1041（来源）**而不是 1040（目标）；颜色 0 故意**双写**
（主表 40 与颜色表 10040 各一份同一条路径，`ColorTable(0,40)=10040`）；wg1 从颜色 0 改成 3 之后，
主表 100 里那条**消失**、只留在 13100（pref 233），主表剩下的 wg0 那条 pref 仍是 230。
负向也钉住了：`10.44.0.0/16` 由 VPN 41 与 42 同时送进来 ⇒ 主表 `buckets:1`、标签 1041、
`fib.log` 里有择一 warning，且撤掉 41 那份后同一前缀**改压 1042**、加回来又回到 1041
（赢家跟着 RD 重算，不是先到先得）。

---

## 5. 逃生开关（用户 item 5）

### 5.1 语义

| 开关 | 主表内容 | 颜色 0 全 down 后默认流量 |
|---|---|---|
| 关（**默认**） | 只有颜色 0 的路径 | **丢弃**，不去占用昂贵颜色 |
| 开 | 全部颜色，pref=230+颜色，颜色 0 最优 | 自动走**存活的最小颜色**，恢复后自动回落 |

### 5.2 实现结论：主体是 VPP 的原生 preference，不是我们的控制面循环

开关打开时把各颜色下一跳"在主表复刻一份、pref 按颜色递增"——§2.1 实测证明 VPP 原生就是这么
工作的，包括 down→备份顶上、up→自动回落。所以：

- **不要**在 `sr-fib` 里自己实现"发现颜色 0 全死再把别的颜色搬进主表"的状态机；
  我们只负责把路径集合和 preference 写对。
- down 信号仍然由现有的 BFD 过滤器提供（`fib.go:304-309` 把死路径**从计划里剔掉**）。
  这条要保留：WG 接口在 VPP 眼里始终是 up，VPP 不知道 BFD 的事。
  于是"颜色 0 全 down"的实际时序 = **sr-fib 一轮收敛**（`PollSec` + `settle_ms`），
  不是数据面亚毫秒。要在控制台如实写这个口径（§8）。**O4 实测见 §5.3，数就是这个口径的数。**
- 开关翻转属于"路径集合没变、pref 变了"那一类。**2026-10-30 实测：我们的 `is_multipath=0`
  整条替换会重算选路，普通 add 就够，不需要先删**（§2.2）—— 这一条原先写的是"必须删了重加"。
- 关掉开关是更干净的语义（昂贵颜色不被默认流量占用、也不引入"回落抖动"），所以默认 false。
- **不需要再加代价/penalty 字段**（用户确认）：VPP 的 preference 是纯排序，"颜色 7 只在 0..6
  全死时才用"由 `preference=230+颜色` 天然做到。以后的 SLA 代价属于 policy 阶段（选**进哪个颜色表**），
  不属于这里（决定**默认流量降级到哪张隧道**）。

### 5.3 O4 实测（2026-10-30，`test-escape.sh` 32/32）：两条切换各多长、终端看到什么

形态（VRF 60、标签 1060、颜色 0/3 ⇒ 颜色表 10060/13060；两端各两条多跳 BFD 会话、两个 tap 终端，
`poll_sec=1`，BFD 50 ms × detect-mult 3）：

- **开关关**（定案默认）：主表 60 里只有颜色 0 那一条；打下 wg0 → **主表这条前缀不存在**
  （`routes+0/-2`），同时**颜色表 10060 也空了**（路径是被剔出计划，不是只改主表），
  而 13060 照写 —— 开关只改主表这一条规则成立。恢复 wg0 后前缀自己回主表。
- **开关开**：主表 60 同时收 230（颜色 0）与 233（颜色 3）；打下 wg0 → 这条前缀**一次都没消失过**，
  只是路径集合收成"只剩颜色 3 那条"；恢复后自己回到两条，且**接口计数器证明流量真的回了颜色 0**
  （`show interface wg0` 的 `mpls` 行 12 个包 +12、wg1 +0；上一轮 +46/+0）。
- 负向照旧钉住：`buckets` 在这里**不是**判据 —— 实测不同 pref 的两条路径 VPP 放同一个 dpo
  （主/备，`buckets:1`），只有同 pref 才并成 `buckets:2`（§2.1 那张表的第三行，这次是在我们
  自己的下发路径上复现）。所以开关开的形态证据是 `fib-dump` 读回的两条 pref + 计数器归因。

控制面切换计时（同一判据连跑三轮的观测区间；最后一列是 32/32 那一轮）：

| 测什么 | 区间 | 绿色轮 |
|---|---|---|
| 关：wg0 down → 主表这条前缀消失 | 342–1165 ms | 1165 ms |
| 关：wg0 up → 前缀回主表 | 449–572 ms | 449 ms |
| 开：wg0 down → 主表只剩最小存活颜色 | 724–1783 ms | 776 ms |
| 开：wg0 up → 主表回到两条路径（形态） | 891–2188 ms | 2188 ms |

区间的上界来自"故障落在 poll 周期的哪一处"（BFD 检测约 150 ms + 最多一轮 1 s），不是代码分支差异。

终端实际断流（0.1 s 间隔 × 120 包，最长连续缺失 `icmp_seq` 段，±100 ms）：

| 开关 | 实测 | 读法 |
|---|---|---|
| 关 | 120 发 10 收，**断流窗口 11 s（= 打流全程）** | 这**不是**切换耗时：脚本在打流期间一直让 wg0 保持 down（下一步才恢复）。它量的是"不回落到昂贵颜色"的字面含义 —— 颜色 0 故障期间这条前缀就没有路径 |
| 开 | 120 发 111 收，**断流窗口 900 ms** | **推断，未做分向计时**：本端 forward 应该几乎立刻由同一个 dpo 里的备路径顶上（§2.1 实测过接口 down 时 pref 5 那条即刻生效），那么这 900 ms 主要是**反向** —— 对端的 wg0 在它那侧仍然 up，回包继续发往已死的 192.168.99.1，要等对端 BFD 判 Down + 一轮 poll 把自己主表那条也搬到颜色 3 才通。这个数与本机 776 ms 同量级，方向归属要用"两端同时打计数器"才能钉死，留作后续（不影响开关的语义成立） |

结论进 §8 的口径：**开关开 ≠ 不丢包，是把"整段故障期全断"压成"约 1 s 的单向收敛窗口"**；
开关关 = 故障期间明确黑洞，不去占用昂贵颜色。

---

## 6. 路由协议 v4：借 BGP 的稳定骨架，不借它的兼容性

**定位（用户定调）：solution 完全私有，不追求与任何厂商 BGP 互通；借的是 BGP 二十年踩出来的
机制形状，用来传我们自己的消息。** 因此：

- **不**引/改 GoBGP（2026-10-30 定调：**纯重构我们自己的 `internal/route`**，GoBGP 只当参考读物）。
  三条实测理由：它的 `go.mod` 要 **Go 1.25.0**，容器里是 **Go 1.22.5**；**29 个直接依赖**
  （grpc/protobuf/viper/prometheus/netlink/sentry…）对撞本模块"零第三方依赖"的既有规矩；
  以及最贵的一条——改它源码 = **永久维护一个 fork**，每次 upstream 变动都要重放我们的私有化改动，
  这笔成本比我们重写一遍选路逻辑更高，且换来的兼容性我们**明确不要**（见 §10 第一条）。
- **不**用 FRR 当 RR：会话建在 mTLS 上（`device_id == 证书身份`），FRR 的标准 BGP 那侧
  表达不了。
- 与标准 BGP 的差别**只有两处**（用户口径）：① 传输不是 TCP/179 而是 mTLS；
  ② RIB 选完路以后的**装表阶段**（vpnid→label、deviceid→下一跳、prefix→prefix、rt→rt）。

### 6.1 要借的机制件（每一件都对应一个现存的坑）

| BGP 机制 | 我们解决的问题 | 现状 |
|---|---|---|
| adj-RIBs-in（每会话一份原始库） | 隧道后补 / 新 VPN 后补时不必重新问 RR；修 §2.5 那条"必须重启" | **O5-a 已落（2026-10-30）**：每会话一份 `adjIn` 存原样收到的路由，`rib` 与 `pending` 都从它 + 本机隧道表/VPN 表推导；`SetTunnels`/`SetVRFs` 只重算，不再问 RR |
| **Route Refresh (RFC 2918)** | `SetTunnels` 之后就地重算，不重建会话、不断表 | **不做（2026-10-30 用户拍）**：它要解决的问题已经在**收端**解决了——原始库留着，本机表一变就重算，不需要向 RR 再要一遍。⇒ O5 不动线上字段，`ProtocolVersion` 仍是 3 |
| **Graceful Restart (RFC 4724)** | RR 重启/主备切换时转发面不动 | 无：新会话顶替即清表 |
| **Add-Path (RFC 7911)** | 同一前缀的多来源路径都送下来，颜色才有得选 | 无：按 `{device,vrf,prefix}` 一条 |
| per-peer 最优路径计算 | 把装表决策收在设备侧（§4.3 步骤 2/4） | 无：RIB 直出 |
| labeled NLRI (RFC 3107) + RT ext-community (RFC 4360) | 语义对号，字段形状稳定 | 已有等价物 |
| 二进制帧 + Notification 码 + FSM/分层 timer | 错误可见、可诊断；收敛速度可调 | 帧已有，Notification 只有文本 |
| **私有 AFI/SAFI + OPEN capability** | 版本协商：v4 能力谈不拢就明确拒会话，不再靠"版本号写在 envelope 里" | 只有 `Version` 字段 |

### 6.2 明确不借的两点

- 下一跳**不**塞进 prefix 里（NLRI 不带下一跳）：下一跳来自接收方自己的隧道表，
  这是 §4.3 步骤 3 的前提，也是"没隧道就拒收"这道闸存在的原因。
- mTLS 身份绑定：`device_id` 必须等于证书身份（`proto.go:116-123` 的既有约束），
  任何"来源字段与会话身份不符"的路由照旧丢弃。

### 6.3 协议字段增项（v4，**未启用**）

O5-a 没有走到这里：adj-RIBs-in 的重算发生在收端本机，`SetTunnels`/`SetVRFs` 复用已有的
`MsgTunnels`/`MsgAnnounce`，一个字段都没加 ⇒ **`ProtocolVersion` 仍是 3**。下面这些是
Add-Path / GR / NOTIFY 结构化那一轮才需要的，先留档：

```
Route  { rd, device_id, prefix, rts[], origin_tag }   // rd = device_id:vpn_id（§7）
Tunnel { peer, nexthop, color }                        // +color，per-tunnel 已有 /32
消息   { ROUTE_REFRESH, NOTIFY(code, subcode), CAPABILITIES 协商 }
```

### 6.4 选路决策链（用户拍：「按 BGP 本身机制来」）

同前缀多来源时的择一**不用我们自创的规则**，照 BGP 的 best-path 形状走，缺属性的步骤显式标
"本系统无此属性，跳过"——这样以后加属性（metric、SLA 代价）是**插进一步**，不是改算法。

```
对 (目标表 t, 前缀 P) 的候选路径集合（RT import 已过滤）：
  1) 本系统权重          —— 暂缺；SD-WAN policy 的 SLA 代价以后落在这里
  2) 本地优先级 local-pref —— 无此属性，跳过
  3) AS_PATH 最短        —— 无 AS_PATH；类比项 = SR 段跳数（隧道配置里的中转跳数），
                            v1 不参与比较（跨 VPN 的来源通常是同一条声明），跳过并写明
  4) origin / MED        —— 无此属性，跳过
  5) eBGP 优于 iBGP      —— 不适用（我们只有一种会话），跳过
  6) IGP cost 到下一跳   —— 类比项 = 隧道彩色代价；v1 只有"颜色 0 优于其它颜色"这一条，
                            已由 §5 的 preference=230+颜色 表达，不在这里重复
  7) 确定性 tie-break    —— 比 RD：<device_id 小者胜，再比 vpn_id 小者胜>
                            （对应 BGP 的 lowest router-id，目的同一个：flap 不来回抖）
  → 胜者决定**标签**（LabelFor(来源 VPN)），于是桶内天然同标签 → §2.4 的静默投递被结构性排除
```

**这条链只管"我们这一来源（API）内部的候选"**。盒内 FRR 同步进来的同一前缀**不进这条链**：
它属于另一个来源（`lcp-rt`），VPP 用 source priority 分层，谁转发是数据面已经定了的事
（§2.7 实测，含双向自动接管）。⇒ §6.4 不需要为"本地 vs SD-WAN 同前缀"写任何择一规则，
③ 在这里自然消解，**不要**在 loader 里加丢弃逻辑（加了反而毁掉 FRR 路由撤销时的自动接管）。

胜者之外的候选**不丢**，留在 adj-RIBs-in 里（Add-Path 形状），policy 阶段可能要第二条路径。

**同组内的多下一跳 = BGP 的 `maximum-paths` 语义**：同 (前缀, 胜者 RD, 属性) 的多条隧道只差
下一跳，天然满足 multipath 条件；颜色 0 的多条就是这一组（⇒ 你要的 ECMP），
非 0 颜色在逃生开关打开时靠 preference 排序，不是 BGP 的 multipath 而是 VPP 的原生 per-path
preference（§2.1）。**两件事分开：BGP 决定"哪个属性组赢"，VPP preference 决定"组里哪个下一跳先用"。**

改颜色**不要求该隧道无流量**（用户拍）。原先推论"⇒ 走 §2.2 的先删后加，删加之间有窗口"，
2026-10-30 实测否证了前提：我们的整条替换（`is_multipath=0`）自己会重算，普通 add 不留窗口
（§2.2 表第三行，O2 第 7 步实测：`buckets:2 → 1`、preference 读回 230/233）。
所以"实测这个窗口"这件事变成：**如果按 O1 已落的删加规则走才有窗口**。
2026-10-30 用户拍：**规则留着，改色丢包属于正常应该接受的**（§11 拍板 #2）。
⇒ 窗口本身不再是要不要存在的问题。**时长已在 O4 量到（§5.3 第 10 步）：走"新进程 plain add
（整条替换）"这一路 = 120 发 120 收、断流 0 ms**；而 Applier 的先删后加分支要求"同一进程内
身份相同、只有 pref 变"，颜色只在进程启动时读一次 ⇒ 今天的配置流线走不到它，只有单测钉住。
⇒ 控制台文案可以用"改色不保证零丢包"这个口径，但**不必**承诺一个具体的丢失秒数；
下一次动这块代码时的依据是这组数，不是"改色一定留窗口"这个未被实测支持的前提。

`origin_tag` 就是 §3.3 的盒内防环标记，由设备在 leak 上行时打，"sdwan→内侧"下行时拒收。

---

## 7. RD：`device_id + vpn_id`（用户定）

- 直接回答 §2.4：两个来源 VPN 送同一个前缀，在 BGP 语义里是**同一个 NLRI 的两条路径**，
  于是"最优路径必须按目标表算"。加了 RD 之后它们是**不同 NLRI**，各自在自己的
  adj-RIBs-in 里存活，不再互相顶掉。
- RD 不参与转发，也不进 VPP：它只是 RIB 键的一部分。装表时仍然只按 §4.3 的
  「同标签才同桶」+ 确定性择一，**RD 不放松这条规则**（不同 RD = 不同来源 VPN 时
  仍然禁止 ECMP 混桶）。
- 宽度：RD 用 `<device_id>:<vpn_id>` 文本形式即可（device uint16、vpn uint8，
  与 §1 的"8 位够用"一致），不需要 BGP 的 8 字节 RD。

---

## 8. 归属、校验与如实呈现

| 决定 | 归属 |
|---|---|
| 导通规则、RT 分配、leak 策略、prefix-list、timer、逃生开关 | **控制器**（草稿→发布→拉取，唯一撰写点，不造数据） |
| RR 反射范围 | **RR**：只按互相宣告的隧道表决定对象，**不按 RT 过滤**（RFC 4456 的 RR 本来就不看 RT；且设备新配 VPN 后能立刻收敛，符合用户理由） |
| 装哪张表、压哪个标签、颜色→preference、同标签择一 | **设备**（`sr-route` 选路 → `sr-fib` 写 VPP） |
| 客户内网通告 | **FRR**（`redistribute table-direct N`），只碰 ZAPI，不碰主表 |
| LAN 侧 VRRP（首跳冗余） | **控制器撰写**（基础配置 §3.1.1）→ **设备执行**（`vppapi` 的 `vrrp_vr_add_del`；CLI 在这个 build 不可用） |

必须呈现的状态（数据来自设备回报的计数器，控制器不推断）：**已放行（`imported`）/ 被 RT 闸丢
（`dropped_rt_mismatch`）/ 字段非法（`dropped_invalid`）/ 是自己宣告的（`dropped_own_route`）/
被"无下一跳隧道"闸丢（`dropped_no_path`）**，外加 `sr-fib` 侧的**同前缀多来源标签被择一**
（warning，不落计数器）。
**原来这里写的"被 noVRF 闸丢"这道闸已经在 O3-a 删了**：它判的是"来源 VPN 在本机存不存在"，
而导通要解决的恰恰是"本机没有的那个 VPN 的路由落进本机有的 VPN 表"，所以它在 RT 匹配之前
就把所有跨 VPN 的导通丢掉了（`internal/route/client.go:737-741` 的注释就是这条的墓碑）。
逃生开关打开时，控制台要写清切换口径（数在 §5.3，不许写成"数据面立刻切换"，也不许写成"立刻恢复"）：
本机主表换成最小存活颜色 **0.7–1.8 s**（BFD 检测 + 一轮 poll），回到颜色 0 **0.9–2.2 s**；
终端在这一段里实际断流约 **0.9 s**，因为**反向**那条也要等对端同样一轮收敛 —— 开关开的收益是
"把整段故障期全断压成约 1 s"，不是零丢包。开关**关**时不存在切换：颜色 0 全 down ⇒ 这条前缀在主表
里没有，故障期间**全程黑洞**（实测 120 发 10 收）。

冲突规则（**2026-10-30 实测改掉，原写法是"本地来源与导通来源同前缀 ⇒ 显式拒绝"**）：
同一 (目标表, 前缀) 上本地来源（FRR，VPP 里是 `lcp-rt`）与 SD-WAN 来源（`API`）同时存在时
⇒ **两份都装、不拒绝、不覆盖**，转发的谁由 VPP source priority 定（`lcp-rt` > `API`），
FRR 那份撤销时 API 那份自动顶上（§2.7）。设备要**记一条 warning** 并在控制台的排障口径里
如实说明：`show ip fib <前缀>` 会看到**两块来源**，只有一块标 `contributing,active`，
另一块是备份 —— 这不是配错，也不按"显示成 static"那种思科属性丢失来看，我们的属性全在
`sr-route` 的 adj-RIBs-in 里，VPP 只是转发面。保留思科教训里仍然成立的那一半：
**不静默丢掉任何一份**（丢了就没有自动接管）。

规模口径（要在 §9 里被实测，不是估算）：8 条/对 × 20 台 = 160 个 WG 接口 + 160 个隧道监听端口
（中转端口按设备计、不归还）；逃生打开时 FIB 复制系数 = 可达颜色数。
**站点 HA 是 2 倍系数**：一个 2 台站点 ↔ 一个 2 台站点 = **4 个设备对 × ≤8 色 = 最多 32 条隧道**，
按每台设备计则每台多出一倍的 WG 接口与监听端口（§13）。

---

## 9. 分阶段与判据（每阶段可验证，含负向）

| 阶段 | 内容 | 判据 |
|---|---|---|
| O1 | vppapi 加 `Weight`/`Preference`（编码+读回）；`fib.Tunnel` 加 `Color`；`Applier` 加 §2.2 的"pref 变了先删后加" | 单测：`appendFIBPath` 字节级断言 pref 写在 13 号字节、`readFIBPath` 读回；负向：**同一路径集合改 pref 的计划**必须出现一次 `RoutesRemoved+RoutesSet` 而不是原地 add；**再一条负向：`230+颜色 > 255` 必须报错而不是回绕**（§2.7 的 metric 300→44 就是这个坑）；`go vet/test` 全绿 |
| O2 | 颜色进 underlay 配置与隧道表；lab 给 cpe1↔cpe2 加**第二条颜色 0** 隧道 | lab 实测：主表该前缀 `buckets:2`；`set interface state` 打下 wg 口后 `buckets:1`；控制台隧道配置出现颜色列并可编辑（真浏览器点击+截图）。**2026-10-30：`test-ecmp-color.sh` 18/18 全绿**（`buckets:2` 两条 pref 都是 230；打下 wg1 后 `buckets:1` **且 sr-fib 零次再下发** = VPP 原生重算；恢复回 2；两 bucket 期间真流量 ping 通）。控制台颜色列功能已核（0..7 来自后端 `max_tunnel_color`、0→3→0 改色持久化），**截图未补**。O3-b 之后同一支脚本扩到 **29/29**（第 7/8 步从"只观测"改成"断言搬表与回到 2 bucket"） |
| O2+ | 新暴露的验证缺口：**重建过的 WG 口 `set interface mpls enable` 是静默空操作**（§2.8） | 设备侧要么在装表前验 `show interface features` 的 mpls-input 弧，要么删口前先 disable；否则"隧道配好了、FIB 也装了、带标签的 VPN 流量在收端全丢"会伪装成 ECMP 故障 |
| O3 | 表号公式 + §4.3 装表算法（含同标签择一） | lab 双 VPN（10/20）+ 一条导通规则：`10.10.1.0/24` 出现在表 20 且**标签是 1000+10**；ping 通；**负向**：构造两个来源 VPN 同前缀，断言主表里该前缀 `buckets:1` 且标签是被择一那个源（这条就是 §2.4 静默投递的回归测试）。**2026-10-30 落地：`test-cross-vpn.sh` 29/29**（VPN 40/41/42 三张表，判据形状与上面一致：装在表 40 却压 **1041=来源**、颜色表 10040 双写、跨 VPN ping 通、`10.44.0.0/16` 双来源 ⇒ `buckets:1` + warning + 三轮 1041→1042→1041 证明跟着 RD）。O3-a 那半边（导入按 RT 命中目标表）另有单测 `TestClientImportsOnlyMatchingRTs`、
`TestColorTableFormula`、`TestPlanFromCrossVPNInstall`、`TestPlanFromDifferentLabelsTieBreakByRD`
与回归基线 `test-fib.sh` 36/36 |
| O4 | 逃生开关 | 打开：颜色 0 全 down 后 `show ip fib` 主表切到最小存活颜色、恢复后自动回 0；关闭：颜色 0 全 down 后主表该前缀**不存在**（丢包，不回落到昂贵颜色）。两条都要计时并写进文档，作为 §8 口径的证据。**2026-10-30 落地：`test-escape.sh` 32/32**（VRF 60，两端各两条多跳 BFD + 两个 tap 终端打 0.1s 流）。两条判据都成立且**都拿到数**：开关关 342–1165 ms 后主表没这条前缀、故障期间 120 发只有 10 收（黑洞，无回落）；开关开前缀一次没消失、724–1783 ms 收成最小存活颜色、终端断 900 ms，恢复后 891–2188 ms 回到两条并用 `show interface` 的 `mpls` 计数证明流量真回了颜色 0。全部数与读法在 §5.3，§8 用的是同一套口径。附带量到的：§11 拍板 #2 那个"改色窗口"实测 **0 ms**（120 发 120 收） |
| O5-a | `route` 包：adj-RIBs-in + 推导视图 + `SetTunnels` + `SetVRFs`（**不动协议字段，版本仍 3**，Route Refresh 已拍不做） | 单测（2026-10-30 已落，`go vet` + `-race` 全绿）：后补隧道 ⇒ 此前 `no_path` 的前缀装进表、去掉隧道 ⇒ 前缀离开表但仍在 `pending`；改 import RT ⇒ held 路由双向搬；**负向：VRF 被删 ⇒ 它上面的本地前缀必须显式撤回**（RR 那边要看到 withdraw，`LocalSkips()` 报 `vrf_unconfigured`）；被拒的变更不动表；会话断开期间的表变更要在重连时补发；空隧道表必须走 `[]` 而不是 `null`。**顺手修掉一个既有 bug**：`&nil` 序列化成 `"tunnels":null`，RR 会以"tunnels message without a table"关掉会话 ⇒ 任何"没配隧道"的设备原本一上线就掉会话 |
| O5-b | `sr-route` 开 `PUT /tunnels` + `PUT /vrfs`（照既有 `PUT /local-prefixes` 那道缝），`/status` 加待决清单（`Pending()` + `LocalSkips()`，带上总数） | curl 能改隧道表并读回；待决清单在 `/status` 可见且有条数上限 |
| O5-c | `test/scripts/test-refresh.sh`：全程**不重启任何进程** | srwg-node1/2：无隧道 ⇒ `pending/no_path`；`PUT /tunnels` ⇒ 下一次 sr-fib 轮询装表 ⇒ tap 终端 ping 通；撤隧道 ⇒ 离表且 FIB 撤；`PUT /vrfs` 改 import ⇒ 此前 RT 不匹配的前缀回来 |
| O5-d | 开机自启：`usr/local/lib/sr-box/start.sh` 一份清单（vpp → sr-apply boot → frr → sr-agent → sr-route → sr-fib，按身份门控），systemd 单元作薄壳并注明"未在 lab 实测"；容器 `--restart unless-stopped` 只在 srwg-node1/2 验 | 重启盒子后不手敲任何命令，overlay 与路由面自己在；agent 只在首次入网时 `systemctl start sr-route sr-fib`，之后配置变更**永不**重启进程 |
| O6 | 控制台「配置 > overlay」三块（全局 / 导通 / leak），导通规则可编辑 | 真浏览器：新增规则后 RT 自动写入两行、编辑减设备后残留 RT 被摘掉、被丢路由三态可见；改完 `console.html` 必须 `node --check` |
| O7 | SD-WAN policy（下一节，按 SLA 引流到颜色表） | 本文不展开，只要求 O1..O4 已经把颜色表建好 |
| O8 | 站点 HA：同站点双设备的隧道彩色规则（§13.2）+ LAN 侧 VRRP（§3.1.1）+ 控制台按站点看冗余 | lab 判据（**要先有第二台盒，见 §13.5**）：① 两台设备同颜色 0 隧道 → 主表该前缀 `buckets:2`；② 打下一台设备的 wg 口 → `buckets:1` 且流量不断；③ VRRP 主备切换后客户侧 ping 无中断（连续打流计数），且**我们这一份路由不受影响**（`show ip fib` 只是 LAN 侧首跳变了）；④ 同站点两台同时宣告同一前缀 ⇒ **不黑洞**（§13.3） |

O1..O4 是"数据面+本地选路"，O5 是"不重启进程也能收敛"，O6 是"控制台"，**O8 需要 lab 第二台 CPE 盒，排在 O6 之后**。
**O3 之前不动协议**，所以 O1..O4 可以完全在设备侧落地并单独验证。

---

## 10. 明确不做

- 与厂商 BGP 的任何互通（含 TCP/179 兼容、标准 AFI/SAFI 载荷）。
- GoBGP / FRR 作为 RR。
- 同机跨 VPN 的本地互导（用户只要跨站点）。
- prefix-list 的范围匹配（`le/ge`）、前缀聚合、重叠地址 SS-NAT。
- 客户内网自身配错导致的环路。
- 数据面 ACL / `data-prefix-list` 那套 LAN 侧兜底。
- community、按 site 做**策略分组**、service chaining。
  （**site 本身不再是"不做"**：§13 的站点 HA 要用它，后端 `node.site_id` 已经落了；
  这里排除的是"把 site 当成 policy 的匹配维度"。）
- 同站点两台设备做**控制面主备协商**（谁当选、心跳线、脑裂仲裁）——不做，见 §13.4：
  两台设备彼此不认识，冗余全部由"两个独立 device_id 各跑各的 + VPP 原生 ECMP/来源分层"承担。
- 策略自动回滚（沿用既有规矩）。

## 11. 拍板记录（2026-10-30 全部已定，无待决项）

| # | 事项 | 结论 |
|---|---|---|
| 1 | 同前缀多来源标签时怎么择一 | **按 BGP 本身机制做选择**（决策链见 §6.4，缺属性的步骤显式跳过），不自创新规则 |
| 2 | 改颜色是否要求该隧道无流量 | **不要求**；走"先删后加"。O2 实测（§2.2）把这条规则的前提否了：整条替换（`is_multipath=0`）自己会重算，删加是多余的、且是改色瞬间窗口的唯一来源。**2026-10-30 用户重拍：留着，"改颜色会丢包属于正常应该接受的，实现它即可"** ⇒ 规则保留（`fib.go:492`），单测 `TestApplyDeleteThenAddOnPrefChange` 钉住。**窗口时长已在 O4 量到（§5.3 那张打流表的最后一行）：走重启 `-once` 的整条替换 = 120 发 120 收、断流 0 ms**，也就是说"改色一定要留窗口"这句话没有被实测支持过；而先删后加那条分支要求"同一进程里身份相同、只有 pref 变"，颜色又只在进程启动时读一次 ⇒ 今天的配置流线走不到它，它目前只活在单测里。规则不动（用户拍的），但**下一次动这块时的依据是这组数**，不是"改色会丢包"这个假设 |
| 3 | 逃生开关作用域 | **租户级** |
| 4 | 是否给昂贵颜色加 penalty | **不需要做任何事**：`preference=230+颜色` 已天然做到"只在更小颜色全死时才用" |
| 5 | lab 加第二条同色隧道（O2 判据） | **允许**；按既有规矩手工配，我配之前先列要配什么 |
| — | 实现路线 | **重构我们自己的 `internal/route`，不改 GoBGP**（理由见 §6） |
| 6 | ③ 同站点两台宣告同一前缀，SD-WAN 与本地 FRR 撞车怎么办 | 用户提出用 preference 分层（230 > FRR）。**实测否证机理、但结论采纳**：`preference=230+颜色` 按用户写法保留，压制关系实际由 **VPP source priority（`lcp-rt` > `API`）** 提供，双向自动接管也是原生行为 ⇒ loader **不加**丢弃规则（§2.7、§6.4 末段） |
| 7 | FRR 的 distance 改不改得动 VPP 的 preference | **改不动**（实测：distance 5/177、`distance bgp 77` 之后内核 metric 恒 20、VPP pref 恒 20）。FRR 那份路由的 pref 只能靠**内核 metric**（=RTA_PRIORITY，LCP 原样抄）这条路上调 |
| 8 | LAN 侧首跳冗余 | **加 VPP 的 VRRP，配置项放基础配置**（用户拍：这块之前我们落了）。字段与 API 面见 §3.1.1，排在 O8 |
| 9 | HA 的默认形态 | **active-active**：两台设备的隧道默认都配颜色 0 ⇒ 主表天然 ECMP；"备"用**改颜色**表达，不做控制面主备协商（用户拍"可以！"，§13.2/§13.4） |
| 10 | HA 的隧道要不要新增机制 | **不要**：underlay 不改，客户配置时用现有 underlay 机制把两台设备的隧道建好（用户拍，§13.2） |

## 12. 开工顺序（等一句"开始"）

1. **O1**（纯 Go，无 lab 动作）：`vppapi.FIBPath` 加 `Weight`/`Preference`（编码写实际值、
   解码读回偏移 12/13）→ `fib.Tunnel` 加 `Color` → `Applier` 加"路径身份集合没变但 pref 变了
   ⇒ 先删后加"。判据：字节级单测 + 负向单测 + `go vet/test` 全绿。
2. **O2**（要动 lab 配置，我先把要手工配什么列给他）：cpe1↔cpe2 第二条颜色 0 隧道 →
   主表 `buckets:2`；打下 wg 口 → `buckets:1`；控制台颜色列可编辑（真浏览器点击+截图）。
3. O1/O2 之间不夹任何协议改动。
4. **O8（站点 HA）** 排在 O6 之后，因为要动 lab：第二台盒 + 两份入网 + VRRP 主备。
   其中**唯一不依赖第二台盒**的部分是 `site_peers` 下发与同站点过滤器（§13.3 第 1 道闸），
   它可以提前到 O3 一起做——单机站点下这个集合为空，行为完全不变，风险为零。
   VRRP 的 `vppapi` 客户端（`vrrp_vr_add_del`）也先落，因为它不需要第二台盒就能验证
   "本机加一个 VR、能 dump 回来"。
5. **O3 已落地（2026-10-30）**：O3-a 把导入改成"按 RT 命中目标表"（#147），O3-b 落了表号公式 +
   标签与表解耦 + 同标签择一（#149），判据见 §9 的 O3 行与 §4.3 末段的实测。
6. **O4 已落地（2026-10-30）**：逃生开关 `escape_all_colors` 进 `PlanFrom`（`fib.go:311-318`）
   与 `sr-fib` 配置文件（键名与 §3.1 逐字一致，读侧 `DisallowUnknownFields` 保证控制器打包时对不上
   会当场报错），判据与**四个计时数 + 三段打流**在 §5.3，控制台口径在 §8（#150）。
   §12 原先挂在这里的"顺手量改色窗口时长"也一起量完了，结果是 0 ms（§11 拍板 #2 末段）。
7. **O5-a 已落地（2026-10-30）**：`internal/route/client.go` 加每会话 adj-RIBs-in（`adjIn`）+
   两张推导视图（`rib`/`pending`）+ `SetTunnels` / `SetVRFs` + `Pending()` / `LocalSkips()`，
   计数器改成**事件形状**（记"这一帧做了什么"，本机表变动不许改写历史）。**协议没动**：
   Route Refresh 用户拍"先不做"，所以 `ProtocolVersion` 仍是 3，`reflector.go` 一行未改。
   判据、负向单测与顺手修掉的 `null` 隧道表 bug 见 §9 的 O5-a 行。
8. **下一步 O5-b**（`sr-route` 的 `PUT /tunnels` + `PUT /vrfs` 与 `/status` 待决清单），
   然后 O5-c 才是"不重启进程补隧道就学到前缀"的 lab 判据，O5-d 收开机自启。

---

## 13. 站点 HA（一个站点两台设备）

结论先行：**HA 不需要新机制，需要新过滤器。** 冗余的四层（ECMP / 颜色 / VRRP / 双 RR）全是
已落地或已拍板的东西；唯一真正新增的是"安装期别把同站点的副本装进本机"这一条显式过滤。

### 13.1 配置形状：两台设备 = 两个独立身份，站点只是挂载点

- 两次入网、两个 `device_id`、两张证书、两份基础配置草稿。**协议里没有 site**：
  `RouteKey{DeviceID,VRFID,Prefix}`（`R/controller/internal/route/proto.go:147-151`），
  `grep site R/controller/internal/route` = 零命中（实测）。⇒ HA 不改协议、不给 RR 加概念。
- 站点信息只在**控制器**里有（`node.site_id` 后端已落）。它的作用是让控制器能把"本站点有哪些
  device_id"下发给设备，其余一概不管。

### 13.2 冗余怎么来的（默认 active-active，用户拍"可以！"）

| 层 | 怎么配 | 谁做选中 | 证据 |
|---|---|---|---|
| 隧道 | 两台设备各自用**现有 underlay 机制**建到对端的隧道，默认都颜色 0（用户拍：underlay 不改） | 人工/控制台 | §4.1 |
| 路径合并 | A 和 A' 宣告同一批 LAN 前缀 ⇒ RR 看到两条不同 `RouteKey`，反射回来后本端 RIB 里**两条都在**；对端设备的隧道表里到 A、到 A' 的隧道也都在 ⇒ §4.3 步骤 3 的候选路径天然并到一个桶（**同标签**才允许，§2.4） | `sr-route` 选路 + VPP ECMP | `proto.go:147-151` + §6.4 末段 |
| 主备 | **改颜色**表达：A' 的隧道改成颜色 1 ⇒ 对端主表 pref 230(A) / 231(A') ⇒ A 全死才抬 A'；逃生**关**时主表只收颜色 0 ⇒ A' 完全不进主表 = 纯冷备 | VPP 原生 preference | §2.1、§5 |
| LAN 首跳 | **VPP VRRP**（§3.1.1），客户侧网关用 VR 地址 | VPP 插件 | 2026-10-30 用户拍"加上" |
| 控制面 | 双 RR（已有）：每台设备各自选活动会话 | `pickActiveLocked` | `client.go:677-688` |

规模代价（如实）：2 台站点 ↔ 2 台站点 = **4 个设备对 × ≤8 色 = 最多 32 条隧道**；
每台设备的 WG 接口与隧道监听端口翻倍（中转端口按设备计、不归还 `R/controller/internal/underlay/tunnel.go:273-275`）。

### 13.3 唯一真问题：同站点回注（hairpin），要一条显式过滤器

现象：A' 从 overlay 学到 **A 自己宣告的** 前缀（同一批 LAN 网段）。如果 A' 装上它，
从 A 的 LAN 进来的流量可能被 A' 塞进隧道再送回 A ⇒ 盒内绕一圈，黑洞或抖动。

两道闸，第一道是我们写的，第二道是 VPP 白送的：

1. **安装期过滤（要做，唯一新增字段）**：`sr-fib`/`sr-route` 在装表前，把
   `来源设备 ∈ 本站点其它设备` 的路由**丢掉并记 warning**，计数器口径进 §8 的多态计数。
   为此设备要知道同站点的设备清单 ⇒ 配置包里加一个 `site_peers: [device_id]`
   （控制器撰写、随基础配置下发；这是 HA 唯一新增的下发字段）。
2. **VPP 来源分层兜底（已实测）**：若该前缀在本机也有本地来源（盒内 FRR / connected，
   VPP 里 `lcp-rt` 或 `adjacency`），它**天然压住** API 那一份，且本地那份撤销时 API 自动接管
   （§2.7 双向实测）。⇒ 单臂 LAN（两台设备只有一台接了这段 LAN）也不会黑洞：
   没接到的那台若被第 1 道闸放行才会出问题，而第 1 道闸已经把它拦住了。

**不要把第 2 道当成第 1 道的替代**：第 2 道只在"同表同前缀本机有本地来源"时成立；
第 1 道是无条件的。反过来，第 1 道也不该顺手把第 2 道的自动接管能力拆掉 ——
所以 §8 的冲突规则才从"显式拒绝"改成"两份都装、记 warning"。

### 13.4 明确不做：站点内的控制面主备协商

两台设备彼此不认识：没有选主、没有心跳线、没有仲裁、没有脑裂处理。§10 已记一条。
诚实边界，逐条说清代价：

- **配置漂移**没人对齐：两台各拉各的发布包。⇒ 控制台要一个"按站点比对两份配置"的差异视图（O8 判据）。
- **一次抖动 = 多设备重推**：A 的隧道 down 时，对端主表少一个 bucket。走的是现有 diff 增量
  （`reflector.go:435-486` 实测只推 gained/lost 的那几台），不是全网重推。
- **一台重启 = 该来源路由与隧道表清空重放**（`reflector.go:197-208`）。Graceful Restart 还没做，
  也**不在 O5**：O5-a 的 adj-RIBs-in 是**收端**保留原样消息，RR 那侧重启仍然清表重放。
  HA 的另一台天然兜住这段窗口 ⇒ **在 GR 之前，"同站点双设备"就是我们最实用的 GR 替代品**，
  这句话值得如实写进控制台的排障说明。

### 13.5 lab 前置（要用户点头才动）

O8 的判据需要 lab 里**同一站点两个 CPE 盒**：现在 VPN0 只有 cpe1 有客户 LAN 口，VRRP 主备
没法验。需要的是第二台盒（或复用 lab-cpe2）加两个口进同一台客户交换机 + 两份入网。
按既有规矩，我动手前会先把"要配什么"逐条列出来给他拍；这一条不阻塞 O1..O5。

### 13.6 思科那部分

思科对冗余 WAN Edge 的做法我**没有在 cisco.com 找到逐条出处**（上一轮已如实标注）。
本节只借形状（两台独立设备 + LAN 侧 FHRP + WAN 侧各自建隧道），不借字段、不借名词，
不作为任何判据的依据。

---

### 附 A：上一轮实验残留（preference 首次测量）

lab-cpe1 的 VPP 上留下了 `loop0`/`loop1` 两个**空接口**（地址已删、无路由、无引用；这个
build 的 `delete loopback interface` CLI 不接受接口名，报 "interface not specified"）。
临时表 900/901/902/904/905 与全部测试前缀已删除，表恢复为 0/10/99/250/1250。
VPP 重启（或 lab 重放 `sr-apply`）后 loop0/loop1 自然消失。

### 附 B：本轮（§2.7、§13 依据）实验残留

全部清理并**逐条核对过**与实验前一致（`vat2 ip_route_dump` 表 0 差分 = 空）：

- 测试前缀 `198.18.77.0/24`（内核 + VPP API 两份）、`198.18.78.0/24`、`198.18.79.0/24` 已删。
- FRR 侧：三条 `ip route ... 177/5` 已 `no`，`distance bgp` 已恢复默认（`show running-config` 核对）。
- **一条清不掉的残留**：VPP 表 0 里多出 `10.79.1.1/32`（来源块 `adjacency`，pref 0，
  走 host-eth2）。它是我为 VRRP/静态路由实验下的 `ip route ... 10.79.1.1` 触发的邻接副产物；
  `ip route del 10.79.1.1/32 [table 0]` 两条都执行了但 `adjacency` 那份仍在。
  语义无害（到本网段网关的 /32，下一跳正确），VPP 重启或 `sr-apply` 重放后消失。
- 顺带记一条 CLI 口径（供排障文档用）：这个 build **没有 `show ip route`**；
  `show vrrp` / `show vrrp brief` / `show vrrp all` / `vrrp` 全部 unknown input ⇒ VRRP 只能走 API（§3.1.1）。

