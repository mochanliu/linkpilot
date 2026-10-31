# O7：把 BGP 的 Loc-RIB 这一步补上

用户定案（2026-10-31，两条，都是硬约束）：

1. **我们的"BGP"与真 BGP 只有两条差异**：① 信道是 mTLS；② RIB→VPP FIB 的落法。
   **其余流程照抄 BGP。** 我在设计任何一步时必须回答"这在 GoBGP 里叫什么"，答不上来
   就是我在发明机制，要先问，不许写进代码。
2. **不改 GoBGP 源码、不引它当依赖**（实测：`internal/pkg/table/{path,destination,adj,table,vrf}.go`
   全在 `internal/` 下，别的模块 import 不了；master 的 `go.mod` 要 go 1.25.0 + 30 个直接依赖，
   lab 容器是 go1.22.5）。⇒ **用自己的代码照搬它的实现方式。**

顺带作废我自己提过的两样东西，都是"发明机制"：

- **`rr_disagree`**：BGP 里没有这一步。同一 `RouteKey` 被两台 RR 各反射一份，就是同一个
  path 的两份相同拷贝，并集天然去重；观测都不必单列。
- **ORIGINATOR_ID / CLUSTER_LIST**：反射环的前提是 iBGP 不追加自己的 AS、同 AS 内路由能
  从第二个 RR 绕回源头。我们 `DeviceID` 被 RR 强制等于证书身份、CPE 之间不建会话、RR 不
  反射回来源，三条腿一条都没有；underlay 没有路由协议、只认直连下一跳，转发面也环不起来。

## 现状与缺陷（读代码读出来的，不是推的）

`internal/route/client.go` 今天的样子：每条会话各有一份 `adjIn map[RouteKey]Route`，
`deriveSessionLocked`(:958) 从它派生 `rib`/`pending`，`pickActiveLocked`(:1331) 从若干会话里
挑**一条** `up && synced` 的，`RIB(vrf)`(:1441)/`AllRIB()`(:1461)/`pendingOf(c.active)`(:900)
只读那一条的派生表。

对照 GoBGP：adj-RIBs-in（`peer.adjRibIn`）✓ 有；Loc-RIB（`s.globalRib`，一台路由器一张，
每个前缀一个 `destination` 带 `knownPathList`，插入即排序，best = 第 0 条）✗ **没有**。
我用"挑一条会话"顶替了"合并所有会话的路径再选优"。后果不是"双 RR 冗余没做到"这么简单，
而是**只要被挑中那条会话的内容不全，本机表就不全**：RR 重启后的快照中途、RR 刚收到某台
盒子新隧道还没反射、控制器发布扇出期间 —— 另一条会话明明拿着这条前缀，读路径看不见。

## 目标结构（逐件指到 GoBGP 的标识符）

| GoBGP (internal/pkg/table, master) | 我们落点 | 动作 |
| --- | --- | --- |
| `AdjRib.table map[Family]*Table`，每 peer 一份，只做存储不做选优 | `clientSession.adjIn` | 保持原样（我们无 ADD-PATH，一个 key 一份 path，`Route` 即 path） |
| `TableManager.globalRib` / `NewTable` → `destination{nlri, knownPathList}` | **新增** `Client.locRib map[RouteKey]*destination` | 每个前缀一个 destination，`paths []*path`（一条会话一份），**始终按最优先排序** |
| `Table.update` → `dst.Calculate(logger, newPath, selectionOptions) (*Update, *Path)`，内部 `explicitWithdraw` / `implicitWithdraw` + `insertSort`，返回 `{KnownPathList, OldKnownPathList}` | **新增** `Client.updateLocRib(p *path) *ribUpdate` | 写 destination 的唯一入口；`Changed` 语义照抄（新增/替换/撤回才算变） |
| 选优顺序 `compareByLLGRStaleCommunity → compareByReachableNexthop → compareByLocalPref → compareByLocalOrigin → compareByASPath → compareByOrigin → compareByMED → compareByASNumber → compareByAge → compareByRouterID → compareByNeighborAddress` | **新增** `comparePaths(a, b) int`，逐步注明"本系统无此属性，跳过" | 实际生效只有两步：**`compareByReachableNexthop` 的同位物**（下一跳能不能解析，见下）+ **`compareByNeighborAddress` 的同位物 = RR 地址字典序最小**。用 `s.addr`（跨重连稳定）而不是会话下标，`SetRRs` 换顺序不会让 best 抖。`compareByWeight`/`compareByIGPCost` GoBGP 自己都删了，我们不补 |
| Nexthop Tracking：`Path.IsNexthopInvalid` 翻转 ⇒ `GetChanges` 发 withdraw/重宣告（`updateNexthopState`） | `path.resolvable` 标志 + `SetTunnels`(:623) 里遍历受影响 path | **`no_path` 从"派生出来的表"变成"path 身上的标志位"**，对齐 `IsNexthopInvalid`：path 留在 RIB 里，不参与选优，隧道回来了自动重新当选 |
| `Vrf.ImportRt` + `CanImportToVrf` + `Table.Select(VRF)`（读视图按 RT 过滤，path 本身不复制） | `localVRF.imports` + `importTargets` + `RIB(vrf)` | **`rt_mismatch` 对齐这个**：RT 不命中不是"这条路由不属于本机 RIB"，是"这张表看不见它"。它继续留在 destination 里，改 import 立刻可见，无需重发 |
| `GetBestPath` = 第 0 条非 `rsFilter` 且非 invalid；`MultiBests`/`getMultiBestPath` = 顶部等值平台 | `RIB(vrf)` / `AllRIB()` 改为读 `locRib` 每个 destination 的最优 | 每前缀出一个赢家；多条下一跳本来就在 `RIBEntry.NextHops` 里，不是多 path |
| `dropAdjRIBIn` → `AdjRib.Drop` 逐条产 `Clone(true)` 撤回 → `propagateUpdate` 喂回 globalRib，**按来源摘，不扫全表** | 会话断开 / `retire` 摘除：遍历**该会话的 `adjIn`** 把对应 path 从 destination 摘掉，重新排序 | 这是这次修复的核心机制：另一条会话的同一 path 顶上，**表不缩水** |
| `peer.handleUpdate` → `adjRibIn.Update` → `propagateUpdate` → `globalRib.Update` → `propagateUpdateToNeighbors` | `applyAnnounce`(:1342) / `applyWithdraw` 的写入次序 | 顺序照抄：先落 adj-RIB（每会话一份真副本），再喂 Loc-RIB |
| `ownRoute`：GoBGP 在 intake 做 own-nexthop / loop 检查，命中 `SetRejected(true)`，**不进 adj-RIB** | 保留 `ownRoute` 计数、保留"不进 adjIn" | 这条不是发明，是它的同位物；并在判据里钉死"自己起源的前缀绝不进本机表" |
| `adj-RIB-out` / `advertisedRoutes` | `reflector.go` 的 adj-rib-in + `fanout*` | RR 侧这次不动，本增量只改 CPE 读路径 |

差异 ① ② 保持不动：`Route` 仍不带下一跳（这本来就是 RFC 4364 的递归解析形状），颜色→
`preference=230+颜色`、表号公式、标签跟来源 VPN 仍在 `internal/fib`，GoBGP 的对应物是
"Nexthop Tracking + 多路径等值平台"，位置对，不动。

## 删掉与新增的对外字段

- 删：`ClientStats.ActiveRR`、`ActiveRRID`、`ActiveRIBRoutes`、`ActivePending` 和 `pickActiveLocked`。
- 新增：`rib_ready`（有任一 `up && synced` 会话过 EOR —— 这才是"表可信"的真信号）。
- 每个 destination 只留一份最优；待决清单从 Loc-RIB 读，不再从某条会话读。
- 会话明细继续逐条报（`sessions[].addr/up/synced/reconnects`），**观测**用，不参与决策。
- sr-route `/status` 的键跟着改，sr-fib 与判据脚本一起换。

## 判据（改完必须全绿）

1. 单测（对齐 GoBGP 的语义，不测我们的想当然）：
   - 两条会话对同一 `RouteKey` 都给出相同 path ⇒ destination 里是一份拷贝，不是合成第三条；
   - 未过 EOR 的会话贡献 0 条（对齐 `rsFilter` + "快照中途不算"）；
   - **一条会话撤走 / 被摘掉 ⇒ 表不缩水**（这条是这次修复的靶心）；
   - 隧道回来 ⇒ `no_path` 的 path 自己重新当选，不需要 RR 重发（对齐 Nexthop Tracking）；
   - 改 import RT ⇒ `rt_mismatch` 的 path 立刻可见，不需要 RR 重发（对齐 `CanImportToVrf`）；
   - 两条会话地址交换顺序 ⇒ 选出的最优不变（对齐 `compareByNeighborAddress` 用的是地址不是下标）。
2. `test-rr-switch.sh` 加一条硬指标：**节点 2 可见表在这 1375ms 的窗口里缺口必须为 0**。
   这条不绿，本增量不算完。
3. 读 `active_rr` 的脚本一起换：`test/lab/verify.sh:99`、`test/lab/bringup.sh:776,780`、
   `controller/test/scripts/test-refresh.sh:492`、`test-route.sh`（D 段现在把错误模型钉成
   "杀掉首选 RR 后切到 rr-b 且表不变"，要改成"任意一条会话断了表都不变"）、`test-rr-switch.sh`。
4. `test-vbond.sh` 196 条 + `gofmt -l` + `go vet ./...` + `go test -race -count=1 ./...` 全绿，
   真 lab 跑一遍，提交推 `feat/vbond-control-plane`。

## R1 落地时对齐到的三处细节（2026-10-31）

- `destination.best()` 就是 GoBGP `GetBestPath`：**只认队首**，队首 `IsNexthopInvalid`
  即"本前缀没有最优路"，不在读的时候往后扫。这个成立靠 `compareByReachableNexthop`
  把不可达的排到队尾，所以它是排序不变式，不是读路径的兜底。
- `compareByReachableNexthop` 在两份拷贝可达性不同时**直接定论**（GoBGP 返回
  path1/path2），不能穿到 `compareByNeighborAddress` 去比地址，否则"可解析但地址大"
  的那份会排到"不可解析但地址小"的后面，队首不可达 ⇒ 整条前缀被读成 no_path。
- `samePath` 连**来源会话**一起比：GoBGP 的 `Path.Equal` 也比 source。所以一条会话断掉、
  最优由另一条会话的同内容拷贝接手时，会照 BGP 的样子出一次 withdraw+advertise（内容
  没变，但"队首换人了"）。表不缩水这条不受影响。

## R2 落地时对齐到的三处细节（2026-10-31）

- **EOR 是进口闸门，不是合并规则**：`adjIn` 从握手起就一直写（"这条会话说过什么"的
  记录，`dropAdjRIBIn` 要靠它按来源摘），但只有 `markSynced`（收到 EOR）那一刻才把整份
  `adjIn` 灌进 Loc-RIB，之后逐条进。对齐 GoBGP：没到 Established / 快照中途的 peer 不
  贡献 path。灌入是 `process(rt, s.addr, reachable)`，来源写成会话地址 —— 之后一条会话
  断掉，按地址摘的正是自己那几份。
- **`path.resolvable` 只能等于隧道闸门**（`IsNexthopInvalid`），不能把 RT 命中折进去。
  实现里先折了一次，`TestSetVRFsImportChangeMovesHeldRoutesBothWays` 立刻红：折进去
  等于把 `CanImportToVrf` 的结论写死在 path 上，后加的 VPN 就永远捡不回已经在
  adj-RIB-in 里的路由。所以分成两个函数：`reachableLocked`（本机有没有到 `DeviceID` 的
  隧道，唯一决定 `resolvable`）与 `admitLocked`（RT + 隧道，只用来记 `rt_mismatch` /
  `no_path` 两个事件计数）。
- **摘除只按来源、事件计数看"前缀是否离开表"**：`applyWithdraw` 与
  `dropSessionLocked` 都走 `withdraw(k, s.addr)`，然后才用 `was && !installed` 决定
  `withdrawn` 加不加一。另一条会话还拿着这个前缀时它不加 —— 这也是
  `test-rr-switch.sh` (4) 的判据从"node2 会缺满一个扇出间隔"反转成"node2 缺口 0、
  withdrawn 不动"的原因：缺的那条是**拷贝**，不是路由。

## 顺序

R1 `destination` + `updateLocRib` + `comparePaths`（纯数据结构 + 单测，读路径先不接）
R2 `RIB`/`AllRIB`/`pending` 切到 Loc-RIB，删 `active_rr`，`/status` 换 `rib_ready`
R3 会话断开/`retire` 按来源摘除；`SetTunnels`/`SetVRFs` 改为翻标志位而不是重建派生表
R4 判据（单测 6 条 + `test-rr-switch.sh` 零缺口 + 5 个脚本）
R5 `rr_note` 措辞：并集落地后"两条就是两条等价活动反射器"这句才真的成立

## 状态（2026-10-31，本增量已完成）

R1 `55fa79e`；R2+R3 `aa951ff`（R3 的按来源摘除与标志位翻转与 R2 是同一份改动，分开写
只会是空提交）；R4 判据 `aa951ff` 里一起落；R5 `1edec6e`。跑绿的账：`gofmt -l` /
`go vet ./...` / `go test -race -count=1 ./...`；`test-route.sh` 19、`test-refresh.sh` 51、
`test-boot.sh` 90、`test-rr-switch.sh` 57（(4) 窗口 node2 缺口 **0**、withdrawn 不动）、
`test-vbond.sh` 196。

## 明确不在本增量里

**发布扇出口子**（改导通组时各盒子按自己的 2s 轮询装表，落后的盒子最长约 7s 黑洞：
控制器 `-hb` 默认 5s + sr-fib `PollSec` 默认 2s）。那是控制器**发布顺序**问题，与路由模型
无关，另开一条拍。本增量修的是"第二条 RR 存在但读不到"这一类，两者别混。
