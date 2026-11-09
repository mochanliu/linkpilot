# exp-285：BFD 判 down 之后，「次优顶上」到底是谁做的

**实测日期：2026-11-09。被测盒 CPE3，前缀 172.16.12.1/32，主路经 CPE1、备路经 CPE2。**

这一枪回答的是一个具体的质疑，原话：

> 就是 bfd down 会 set wireguard tunnel down，路由自动变为失效状态，并且次优路由自动变成当前主路由，
> 这个是你以前和我说的现象。这个到底是不是现在的现象。

结论分三段，**前半句不成立，后半句成立但成因有两个，而且以前混成一个说了**：

1. **「BFD down 会 set wireguard tunnel down」——产品里没有这一环。** 全仓库唯一能下发
   `sw_interface_set_flags` 的地方是 `vppapi.Client.SetInterfaceUp`
   （[internal/vppapi/interface.go:41](../../../../controller/internal/vppapi/interface.go#L41)），
   它的生产调用点只有一个：[internal/agent/dataplane.go:814](../../../../controller/internal/agent/dataplane.go#L814)，
   而且**恒传 `up=true`**（建隧道时把口拉起来）。签名上那个 `up bool` 参数从来没有人传过 `false`。
   BFD 会话状态到接口状态之间没有任何代码路径。所以「BFD down ⇒ 口 down」不是现在的现象，
   也不是过去的现象 —— 它从来没被实现过。
2. **「路由变为失效、次优顶上」——是真的，客户可见，而且很快。** 但「失效」发生在两个不同的地方，
   取决于故障长什么样，两条链的触发者完全不同：
   - **链①（邻接级，VPP 自己干）**：本机隧道口 down / 下一跳解析没了 ⇒ VPP 把那条路径标成
     `oper-flags:` 非 resolved 并把它踢出 uPRF，**自己**把转发挪到次优那条。
     此时 BFD 还可能报 Up，path-list 里两条路径都还在。控制面一个字都没说。
   - **链②（可达性输入，sr-fib 干）**：只有对端死、本机口全程 up、邻接全程 resolved ⇒ VPP 什么都不做。
     是 sr-fib 收到 BFD 事件后重算，把那条路径**从行里删掉**，整行 `ip_route_add_del` 覆盖，
     行里只剩备路，于是备路承载。
3. **以前为什么会说成那样**：`docs/overlay-design-draft.md` §2.1 那张表第三行量的是
   「把 pref 0 那条的**接口** `set interface state down`」——那就是链①，结论没错。
   但 [internal/fib/fib.go:314-315](../../../../controller/internal/fib/fib.go#L314) 的注释把它
   概括成「down 顶上、up 回落都是原生行为」，写在逃生开关旁边，读起来就像 BFD down 也走原生回落。
   **实际上 BFD down 时 sr-fib 先把路径删了（fib.go:305-309），行里只剩一条，VPP 的原生 preference
   回落根本没机会被触发**——它只在「口/邻接已死、sr-fib 的重写还没落地」那个窗口里起作用。
   本枪把那个窗口量出来了：**108ms**（脚本按 CSV 里的全精度时间戳算；下面时间线只打三位小数，
   971.303 − 971.194 看着像 109ms，那是显示取整，不是另一组数）。

---

## 一、为什么必须重新开一枪（exp-b3 那一枪量不到这件事）

exp-b3 的前缀有两条**同权**腿：两条隧道都是颜色 0 ⇒ VPP 里 pref 都是 10 ⇒ ECMP，`buckets:2`。
摘掉一条 ECMP 腿叫「死腿摘掉、活腿继续承载」，**里面没有「次优」这个东西**。
要量主备切换，行里必须有一条 pref 更差、平时不承载的路径。

所以这一枪先把 lab 改成真主/备形状（改法见 §五，全部走产品路径）：

| 腿 | 隧道 | 颜色 | VPP preference | 起手是否承载 |
|---|---|---|---|---|
| 主 | CPE3↔CPE1（`wg1009`，下一跳 `10.250.0.0`） | 0 | 10 | **是**（`buckets:1`） |
| 备 | CPE3↔CPE2（`wg1013`，下一跳 `10.250.0.6`） | 2 | 12 | 否（挂在行里不承载） |

preference 的算法是 `PrefBase + 颜色`，`PrefBase uint8 = 10`
（[internal/fib/fib.go:56](../../../../controller/internal/fib/fib.go#L56)）。
**逃生开关必须开**：关掉时主表只收颜色 0 的路径
（[internal/fib/fib.go:316](../../../../controller/internal/fib/fib.go#L316) 的 `if t.Color == 0 || escape`），
那条颜色 2 的腿根本不进主表 —— 它不是备路，它不存在。这就是定案「颜色 0 无 Fallback」。

起手实测（CPE3，表 0）：

```
172.16.12.1/32 fib:0 index:49 locks:2
    path-list:[58] locks:2 flags:shared, uPRF-list:63 len:2 itfs:[5, 7, ]
      path:[87]  pl-index:58 ip4 weight=1 pref=10 attached-nexthop:  oper-flags:resolved,
        10.250.0.0 wg1009
      path:[103] pl-index:58 ip4 weight=1 pref=12 attached-nexthop:  oper-flags:resolved,
        10.250.0.6 wg1013
 forwarding:   unicast-ip4-chain
  [@0]: dpo-load-balance: [proto:ip4 index:52 buckets:1 uRPF:63 to:[1181:99204]]
        [@1]: mpls via 10.250.0.0 wg1009: mtu:1368 ...
```

`len:2` + `buckets:1` + 承载只有 pref=10 那条 = 主备形状成立。

---

## 二、两臂的设计：让两条链各自单独现形

两条链从客户侧看一模一样（都是「主路没了、备路顶上、丢一两包」），必须用**故障形状**把它们分开：

| 臂 | 注入点 | 本机口 | 本机邻接 | 预期触发者 |
|---|---|---|---|---|
| `MODE=local` | **被测盒自己**的主路 wg 口 down | down | 死 | 链① VPP 原生回落 |
| `MODE=peer` | **对端盒**（CPE1）上那条隧道的 wg 口 down | **全程 up** | **全程 resolved** | 链② sr-fib 重写 |

peer 臂是关键：对端口 down 之后，本盒的口还是 up、邻接还是 resolved，VPP 眼里这条路一切正常，
它不会做任何事。唯一能知道「对面死了」的是 BFD 会话，而 BFD 会话状态只喂给控制面。
**所以 peer 臂里发生的任何切换都只能是控制面做的**，这是一个干净的判别实验。

注入点不手写：peer 臂在 `PEERBOX` 上按「该口 L3 地址 == `PRIM/32`」自己查
（对端那条隧道的本端 overlay 就是 `PRIM`）；local 臂在本盒 sr-fib 活表里按 `next_hop == PRIM` 查口名。
认错口就白测一臂，所以两处都是查不到就直接退出、不猜。

---

## 三、口径：每一列能证明什么，不能证明什么

采样器每发读四样东西，写成一行 CSV。**这三列必须分开读，混起来就会误归因**：

| 列 | 来源 | 谁能改它 | 能证明的事 |
|---|---|---|---|
| `承载` | `forwarding:` 之后的 `mpls via` 下一跳 | VPP 挪 uPRF **会**改；sr-fib 重写整行**也会**改 | 客户流量真正走哪条（唯一客户可见的量） |
| `len` | `path-list … uPRF-list:N len:M` 的 M | **只有 sr-fib 重写整行会把它变小** | 行里装了几条路径 = 控制面动过没有 |
| `行内` 的 `R/U` | 每条 `path:` 的 `oper-flags:resolved` | VPP 判邻接死时翻成 U，路径**仍留在 list 里** | VPP 自己有没有判这条路死 |

判别式因此是：

- 出现「**承载已挪到备路、但 `len` 还是起手长度、主路那条标 `U`**」这一发 ⇒ **链①**，VPP 自己干的。
- 从头到尾没有那一发，`len` 变短与承载挪动落在同一发（或更早）⇒ **链②**，sr-fib 干的。

第一版只读「下一跳还在不在行里」，把这两种成因读成了同一件事 —— 因为非 resolved 的路径有时
打不出可读的下一跳，看起来就像「被删了」。加上 `len` 与 `oper` 两列之后 local 臂才变得可判。

**分辨率**：由每发的 fork 数决定，不是由 `sleep 0.02` 决定。peer 臂 3 个 fork（口态 5 发读一次）
实测中位 **90ms**；local 臂 4 个 fork（口态每发都读）实测中位 **107ms**。
**凡是两个事件落在同一发采样里，一律报 `≤分辨率`，绝不写 `0ms`** —— 那不是测出来的 0。

**锚点**：盒钟互差几小时（CPE1↔CPE3 实测差约 47 分钟，CPE3↔Mac 约 25 分钟），
跨机对齐会把几十 ms 量成几百 ms。所以：
- local 臂注入就在被测盒上，「down 返回」那一刻与采样器同钟，**可以**当锚。
- peer 臂注入在对端，**只能**用本盒自己采到的 BFD 翻转当锚。脚本按 `MODE` 自动选，不给人手误用的机会。

**时间线上 `[口down(注入)]` 这个标记的读法**：脚本把所有事件标记贴到**时间上最近的那一发采样**上
（failover.sh 的 `marks` 循环，`min(range(len(ts)), key=…abs(ts[j]-t))`），所以 local 臂那一行打的是
971.054 这一发，而注入命令真正返回是 **971.098**（逐段表里的锚就是它）。971.054 这一发当然还显示
`口=up/up` —— 它采在注入之前 44ms。**别把这一行读成「注入时口还是 up」**；口态翻转看下一发 971.194。

---

## 四、实测结果

### 链①：local 臂（本机口 down）—— VPP 原生回落，比 BFD 判 down 早 234ms

```
采样 482 发，间隔中位 107ms（=分辨率）
主路口全程：['down','up']；备路口：全是 up

1794219971.054 [口down(注入)]                          主bfd=Up    承载=[10.250.0.0] len=2 buckets=1 行内=[10=10.250.0.0:R, 12=10.250.0.6:R] 口=up/up
1794219971.194 [主退出承载+备进承载+主路非resolved+承载挪/行未变] 主bfd=Up    承载=[10.250.0.6] len=2 buckets=1 行内=[10=10.250.0.0:U, 12=10.250.0.6:R] 口=down/up
1794219971.303 [行变短]                                主bfd=Up    承载=[10.250.0.6] len=1 buckets=1 行内=[12=10.250.0.6:R]                       口=down/up
1794219971.428 [主bfd判down]                           主bfd=Down  承载=[10.250.0.6] len=1 buckets=1 行内=[12=10.250.0.6:R]                       口=down/up
1794219991.574 [主bfd判up+主回承载+行装回主路]            主bfd=Up    承载=[10.250.0.0] len=2 buckets=1 行内=[10=10.250.0.0:R, 12=10.250.0.6:R] 口=up/up
```

逐段：

| 段 | 实测 |
|---|---|
| 本盒口 down → 备路进承载链（客户可见的那一下） | **≤107ms** |
| 本盒口 down → VPP 把主路标成非 resolved | **≤107ms**（与上一项同一发） |
| 本盒口 down → path-list 变短（sr-fib 重写整行） | **205ms** |
| 承载已挪、行还是两条 → 行变短（**纯 VPP 回落独占的窗口**） | **108ms** |
| 本盒口 down → 主路 BFD 判 down（BFD 检测时间，不归任何收敛逻辑） | **330ms** |
| 主路 BFD 回到 Up → 主路重回承载链 | **≤107ms** |
| 客户探测 | **1453 发，FAIL 0 发 = 0.0%** |

**决定性的一发是 971.194**：口已经 down、承载已经挪到备路、主路已被标 `U`，
而 **BFD 还报 Up、path-list 还是 2 条**。这一发只能是 VPP 自己干的 —— 控制面此刻还没收到任何坏消息。
切换在 BFD 判 down 之前 **234ms** 就完成了。所以「BFD down 触发切换」在这个形状里是**错的因果**。

sr-fib 随后在 971.303 把行收拾成一条（`len` 2→1），那只是把已经发生的切换在路由面上记干净，
不是它打的枪。BFD 到 971.428 才判 down，sr-fib 日志对应：

```
10:26:11 sr-fib: BFD 10.250.0.0 -> down（本机 10.250.0.1，sw_if_index=4294967295）
10:26:11 sr-fib: 警告: 172.16.12.1/32: 下一跳 10.250.0.0 的 BFD 会话不是 Up，该路径跳过
10:26:11 sr-fib: 收敛 tables+0 labels+0/-0 routes+1/-0 mpls-egress=true
```

### 链②：peer 臂（只有对端死，本机口全程 up）—— sr-fib 重写整行

```
采样 558 发，间隔中位 90ms（=分辨率）
主路口全程：全是 up；备路口：全是 up

1794219898.982 [主bfd判down+主退出承载+备进承载+行变短] 主bfd=Down 备bfd=Up 承载=[10.250.0.6] len=1 buckets=1 行内=[12=10.250.0.6:R] 口=up/up
1794219919.649 [主bfd判up+主回承载+行装回主路]          主bfd=Up   备bfd=Up 承载=[10.250.0.0] len=2 buckets=1 行内=[10=10.250.0.0:R, 12=10.250.0.6:R] 口=up/up
```

| 段 | 实测 |
|---|---|
| 主路 BFD 判 down → 主路退出承载链 | **≤90ms** |
| 主路 BFD 判 down → 备路进承载链 | **≤90ms**（同一发） |
| 主路 BFD 判 down → path-list 变短 | **≤90ms**（同一发） |
| 主路 BFD 判 down → VPP 把主路标成非 resolved | **从未发生** |
| 承载已挪、行还是两条（链①的签名） | **从未发生** |
| 主路 BFD 回到 Up → 主路重回承载链 | **≤90ms** |
| 故障窗口时长 | 20.67s（注入 HOLD=20s） |
| 客户探测 | **1401 发，FAIL 2 发 = 0.1%**（两发孤立单包，不构成连续断流） |

**口全程 up、VPP 全程没判任何路径非 resolved、完全没有链①的签名那一发**，
而 `len` 2→1 与承载挪动落在同一发。sr-fib 日志逐字对上：

```
10:24:58 sr-fib: BFD 10.250.0.0 -> down（本机 10.250.0.1，sw_if_index=4294967295）
10:24:58 sr-fib: 警告: 172.16.12.1/32: 下一跳 10.250.0.0 的 BFD 会话不是 Up，该路径跳过
10:24:58 sr-fib: 收敛 tables+0 labels+0/-0 routes+1/-0 mpls-egress=true
10:25:19 sr-fib: BFD 10.250.0.0 -> init（本机 10.250.0.1，sw_if_index=4294967295）
10:25:19 sr-fib: BFD 10.250.0.0 -> up（本机 10.250.0.1，sw_if_index=4294967295）
10:25:19 sr-fib: 收敛 tables+0 labels+0/-0 routes+1/-0 mpls-egress=true
```

这一枪里「路由失效」的是 **sr-fib 的输入集合**，不是 VPP 的邻接，也不是接口状态。

### 两条链的代码路径（逐步走到哪）

**链②（BFD-only，本枪 peer 臂）**：

1. VPP 的 BFD 会话判 down，发 `bfd_udp_session_event`。sr-fib 在 dial 时订阅了它：
   [cmd/sr-fib/main.go:469](../../../../controller/cmd/sr-fib/main.go#L469) 的 `c.WatchBFDEvents`
   → [internal/vppapi/events.go:58](../../../../controller/internal/vppapi/events.go#L58)
   → [internal/vppapi/events.go:96](../../../../controller/internal/vppapi/events.go#L96) 发 `want_bfd_events`。
   回调往 `wake` 里塞一个信号（main.go:472）。**这是 B3 修的那一环**：B3 之前没有事件订阅，
   只能等 `poll_sec=1` 的轮询，摘路要 745ms；现在实测 ≤90ms。
2. 收敛器读 BFD 会话（[cmd/sr-fib/main.go:301](../../../../controller/cmd/sr-fib/main.go#L301)），
   过 [internal/fib/reach.go:20](../../../../controller/internal/fib/reach.go#L20) 的 `UnreachablePeers`。
   这个函数的口径写得很死：只看多跳会话、**只有 `Up` 算可达**（`Init` 不算，送进去和送进黑洞没区别）、
   **没有会话的对端不出现在结果里**（那是「没有信息」，不是「可达」）。
3. `PlanFrom` 逐下一跳查表，`down[nh]` 为真就 `continue`
   （[internal/fib/fib.go:305-309](../../../../controller/internal/fib/fib.go#L305)），留下那句
   「该路径跳过」的 warning。**注意这里是跳过、不是整条撤掉**：同前缀可能还有活着的腿。
4. 活下来的候选按 `Preference = PrefBase + t.Color` 造路径（fib.go:311），
   主表按 `t.Color == 0 || escape` 决定收不收（fib.go:316），颜色表恒收（fib.go:322-327）。
5. `Applier` 把整行 `ip_route_add_del(is_multipath=0)` 覆盖下去 ⇒ 新 path-list、`len` 变小、
   承载只剩备路。**`is_multipath=0` 的整条替换会重算 preference**（这一点 §2.2 已单独实测过，
   手工 CLI 多 via 的「原地替换不重算」行为不适用于我们的 API）。

**链①（口/邻接死，本枪 local 臂）**：完全不经过上面任何一步。VPP 内部把该路径的
`oper-flags` 翻成非 resolved、重算 uPRF，按 `frp_preference`「只有最优 preference 里 up 的路径参与转发」
（`/usr/include/vnet/fib/fib_types.h:594-600`，§2.1 引过）自己挪到次优。
sr-fib 的重写是**随后**才到的，它到达时切换已经发生了 108ms。

**另外还有一条「失效」，在 sr-route 里，本枪两臂都没走到它**：
Loc-RIB 的 `path.resolvable`（[internal/route/rib.go:33](../../../../controller/internal/route/rib.go#L33)，
是 GoBGP `Path.IsNexthopInvalid` 的同位物、取反）。不可解析的路径**留在表里但沉到尾部**
（`compareByReachableNexthop`，rib.go:104-105），`best()` 在头部不可解析时返回 nil（rib.go:60-61）。
它只由 `SetTunnelState` 翻（[internal/route/client.go:737](../../../../controller/internal/route/client.go#L737)），
喂它的是 agent 每 **10s** 一次的 `PUT /tunnel-state`
（[cmd/sr-agent/main.go:421](../../../../controller/cmd/sr-agent/main.go#L421) 的 `metricsInterval`、
main.go:448 的 `pusher.Push` → [internal/agent/routestate.go:123](../../../../controller/internal/agent/routestate.go#L123)），
入口在 [cmd/sr-route/main.go:519](../../../../controller/cmd/sr-route/main.go#L519)。
**这条路比 sr-fib 那条慢一个数量级（10s vs 事件驱动）**，本枪的切换不是它打的 ——
sr-fib 直接读 VPP 的 BFD 会话，不等 agent 那 10s。写在这里是为了以后有人问「路由标记失效」时，
不会把这两个不同的「失效」当成同一个。

### 定稿复量：主/备形状下客户流量六方向

两臂跑完、脚本改动全部落定之后，在**交付时的那个形状**（t13 颜色 2、四台逃生开关 true）上复量一遍，
证明这一枪没有把 lab 打坏。`TAG=p285-final bash test/lab/vmlab/exp-273/six.sh`，
每方向 `ping -c 50 -i 0.2 -W 1 -I <源 loopback>`，纯只读：

| 源 | 目的 | 丢包 | rtt min/avg/max/mdev (ms) |
|---|---|---|---|
| cust12 (172.16.12.1) | 172.16.3.1 | **0%** | 4.691/13.213/36.172/7.037 |
| cust12 (172.16.12.1) | 172.16.4.1 | **0%** | 5.719/13.429/66.461/10.798 |
| cust3 (172.16.3.1) | 172.16.12.1 | **0%** | 5.620/12.410/47.479/7.329 |
| cust3 (172.16.3.1) | 172.16.4.1 | **0%** | 6.243/13.275/54.925/10.900 |
| cust4 (172.16.4.1) | 172.16.12.1 | **0%** | 3.903/11.189/73.327/10.316 |
| cust4 (172.16.4.1) | 172.16.3.1 | **0%** | 5.064/11.397/78.703/11.319 |

退出码 0。同一轮 `vm-verify.sh` **PASS=398 FAIL=0 WARN=0**（口径变更与逐条账见 §五 末尾）。
注意 `cust3 → 172.16.12.1` 这一行正是两臂注入的那条流：它现在走的是 `pref=10` 经 CPE1 的主路，
`pref=12` 经 CPE2 的备路装着不承载 —— 六方向 0% 说明**主备形状本身不丢包**，
丢包只发生在注入故障的那 20s 窗口里（peer 臂 2 发 / local 臂 0 发，见上面两节）。

---

## 五、lab 现在被改成了什么形状（以及怎么还原）

**这一枪故意改了 lab 配置，现在的状态不等于 #284 提交时的基线。** 改动全部走产品路径
（控制台 → 发布 → agent → apply），没有手改 `ip route`、没有手改 FRR 配置、没有重启任何 daemon。

| 项 | 原值（#284 基线） | 现值 | 怎么改的 |
|---|---|---|---|
| 隧道 t13（CPE2↔CPE3，overlay `10.250.0.6`/`10.250.0.7`）颜色 | 0 | **2** | 控制台改色 → 发布 v23 → agent 已 apply（`agent.log`：`隧道配置 v23 已收敛`） |
| 逃生开关 `escape_all_colors` | 各盒原值 | **四台全 true** | `ESCAPE=true test/lab/vmlab/vm-fib-sync.sh`（本轮给脚本新加的旋钮） |

改色为什么必须走控制台：颜色是 underlay 配置的一部分，`internal/underlay/tunnel.go` 里
`Color *uint8`（指针，所以显式传 0 是「改成 0」，不传是「保持现值」）。
改色落地是 delete-then-add，O4 实测窗口 **0ms**。

CPE2 也必须开逃生开关，否则它朝 `172.16.3.1` 的唯一一条腿（颜色 2）会离开主表，反向流量断。

**还原成基线**（两步，都不重启进程）：

```bash
# 1. 控制台把 t13 的颜色改回 0，发布，等 agent 收敛（看 agent.log 的「隧道配置 vNN 已收敛」）
# 2. 把逃生开关推回去（keep=保留现值，false=显式关）
ESCAPE=false test/lab/vmlab/vm-fib-sync.sh
```

**注意 G-2 仍然没收口**：sr-fib 的隧道表只在内存里，`/etc/sdwan/fib.json` 的 `tunnels` 是 `[]`
（本轮在 CPE3 上实测确认）。`vm-fib-sync.sh` 是产品里唯一填这张表的东西，它从控制台
`GET /api/underlay` 读颜色。**所以 sr-fib 一旦重启，这张表就空了，overlay 前缀会全部按
「下一跳不在隧道表里，该路径跳过」丢掉**，必须重跑 `vm-fib-sync.sh`。逃生开关同样没有产品下发路径。

**交付时 lab 就停在这个主/备形状**（不是基线，是 owner 要自己复测的那一枪的起手形状）。
逃生开关往返取证做完后推回 `true`，再读一次 CPE3 主表确认形状没被取证过程改坏：

```
172.16.12.1/32 fib:0 index:65 locks:2
    path-list:[80] locks:2 flags:shared, uPRF-list:68 len:2 itfs:[5, 7, ]
      path:[87] pl-index:80 ip4 weight=1 pref=10 attached-nexthop:  oper-flags:resolved,
        10.250.0.0 wg1009
      path:[92] pl-index:80 ip4 weight=1 pref=12 attached-nexthop:  oper-flags:resolved,
        10.250.0.6 wg1013
  [@0]: dpo-load-balance: [proto:ip4 index:67 buckets:1 uRPF:68 to:[40:3360]]
        [@1]: mpls via 10.250.0.0 wg1009: mtu:1368 next:3 …
```

`len:2` = 两条腿都装在行里且都 resolved；`buckets:1` + `mpls via 10.250.0.0` = 只有 `pref=10` 那条承载。
**这两行就是「次优路由已经准备好、但还没顶上」的现场证据**，也是 §六 重跑前要核对的那一行。

同一轮定稿复量（全部脚本改动落定之后跑的，不是改之前的旧数）：
`test/lab/vmlab/vm-verify.sh` → **PASS=398 FAIL=0 WARN=0**，退出码 0；
`TAG=p285-final test/lab/vmlab/exp-273/six.sh` → 六方向丢包见 §四 末尾那张表。
398 与 #284 时代 392 的差额是**口径换了**，不是产品变好了 6 条，逐条账在
[docs/agent-handover.md §2.4](../../../../docs/agent-handover.md)。
台账要先重生成再跑（改色后旧台账还写着 t13 颜色 0、发布 v22）：
`test/lab/vmlab/vm-ledger.py > /tmp/vm-verify-ledger.tsv`。

---

## 六、怎么自己重跑

前提：lab 处于 §五 的形状（t13 颜色 2、四台逃生开关开），且 `vm-fib-sync.sh` 跑过。
先确认起手形状对：

```bash
ssh CPE3 'vppctl "show ip fib 172.16.12.1/32 table 0"'
# 要看到 len:2、pref=10 与 pref=12 各一条、buckets:1、承载只有 10.250.0.0
```

两臂各一枪（每枪约 60s，**会动状态**：对注入的那个口各一次 down / 一次 up，单发不循环）：

```bash
cd <repo>
# 链②：只有对端死，本机口全程 up ⇒ 期望「sr-fib 重写整行」
WORK=/tmp/p285 bash test/lab/vmlab/exp-285/failover.sh \
    CPE3 172.16.12.1/32 10.250.0.0 10.250.0.6 cust3 172.16.3.1 CPE1 20 peer

# 链①：本机口 down ⇒ 期望「VPP 原生 preference 回落」
WORK=/tmp/p285 bash test/lab/vmlab/exp-285/failover.sh \
    CPE3 172.16.12.1/32 10.250.0.0 10.250.0.6 cust3 172.16.3.1 CPE1 20 local
```

参数依次是 `BOX PFX PRIM_NH BAK_NH PB SRC PEERBOX HOLD秒 MODE`。
`PRIM_NH`/`BAK_NH` 是两条腿在**被测盒 sr-fib 隧道表里**的下一跳（= 对端那条隧道自己的 overlay /32）。
读别的表用 `TABLE=2000` 覆盖（默认主表 0）。

产物在 `$WORK`：`failover-<BOX>-<MODE>.txt`（判读）、`sample-*.csv`（原始采样）、
`probe-*.raw`（客户逐发）、`timeline-*.txt`（逐发明细，人读的那份）。

**期望读数**：

| | peer 臂 | local 臂 |
|---|---|---|
| 本机主路口 | 全程 up | down 一段 |
| 「承载挪/行未变」那一发 | **不出现** | **出现**，且 BFD 还是 Up |
| VPP 标主路非 resolved | 从不 | 口 down 后 ≤分辨率 |
| 判读行 | `sr-fib 重写整行触发的切换` | `VPP 原生 preference 回落先打的第一枪` |
| 客户丢包 | ≤0.1% | 0.0% |

**脚本会自己判废，判废了别当数据用**：

- 起手 `len < 2` ⇒ 没有次优可顶（多半是逃生开关没开，或备路隧道没发布）。
- 整段没看到主路 BFD 翻转 ⇒ 注入没生效或认错了会话。
- peer 臂在**切换窗口内**备路 BFD 也非 Up、或精确行整条消失 ⇒ 打到的不是「主路对端」而是整盒。
- 采样器没出货 ⇒ ssh/ heredoc 出了问题（`ssh -n` 配 heredoc 会一声不响输出 0 行，exp-b3 踩过）。

注入有两层收口兜底：脚本 `trap cleanup EXIT` 会在中途退出时对注入口补一次 `up`；
注入端自己也有 `trap … EXIT`。只有本枪真的看到新增的「up 返回」那一行才算收口。

---

## 七、踩过的坑（都是本轮真踩的，不是假想）

1. **`show ip fib` 是 LPM 查询，精确行不在时它回落打印覆盖它的那条。**
   peer 臂第二枪被这个污染了：三条 BFD 会话同时 down ⇒ sr-fib 把所有腿都判不可达 ⇒ 整行删掉 ⇒
   `show ip fib 172.16.12.1/32` 打印的是 `0.0.0.0/0` 的管理网默认路由
   （`pref=20 via 192.168.1.1` + 一条 drop 路径）。第一版解析器把那两条路径当成我们的行读了，
   于是「故障窗口 0.81s」「行内 `['20=192.168.1.1:R','0=?:U']`」这种鬼数据。
   **修法**：判输出第二行 `<前缀> fib:<表> index:…` 的第一列必须逐字等于 `$PFX`，
   不等就发 `ABSENT` 记号，绝不拿回落路由冒充。`ABSENT` 本身是要量的事件（= 整行被删）。
2. **`len` 要取第一个 `uPRF-list`，不是最后一个。** 回落输出里有两个 path-list，
   最后一个是 drop 那条 `len:0`；取最后一个会把「行不在」读成「行变短到 0」，正好伪造出链②的签名。
3. **只读下一跳、不读 `oper` 与 `len`，分不出两条链。** 见 §三。
4. **口态缓存会滞后。** local 臂第一版 5 发读一次口态，结果口已经 down 了 69ms 采样还报 `up/up`。
   改成 local 臂每发都读（`IFEVERY=1`），peer 臂才 5 发一次（它整枪不变，省一个 fork 换分辨率）。
5. **盒钟互差几小时，跨机对齐就是错。** 见 §三锚点。
6. **`init` 不是会话重建，是 BFD 正常恢复态。** BFD 恢复走 Down → Init → Up。
   本轮一度怀疑「每 10s 重建会话」，因为 `fib.log` 里有 202 条 `-> init`；
   实际时间戳完全不规则（09:46:24、09:51:49、09:56:59、10:02:33、10:08:44…），
   且 `apply.log` 在那些时刻没有任何条目。那是宿主停摆造成的抖动恢复，不是周期行为。
7. **备路 BFD 在这台 VM 上本来就会自己抖。** 本轮 peer 臂采到 21 发备路非 Up，
   全在切换窗口外（`10:25:23`、`10:25:34`，故障窗口 `10:24:58`–`10:25:19` 已结束）。
   这是 #261/#262 的宿主停摆噪声。闸门因此**只**在切换窗口内判备路死活，
   窗口外的抖动报数但不判废 —— 否则一枪都跑不出来。
8. **`0ms` 不是测出来的。** 同一发采样内完成的两个事件只能报 `≤分辨率`。
9. **本轮给 `vm-fib-sync.sh` 加的 `ESCAPE` 旋钮，第一版 `false` 是假的。**
   写法是 `escape_mode != "keep"` 时置 `True`，于是 `ESCAPE=false` 落进去的还是 `True` ——
   §五 那条还原配方按第一版跑等于没还原，而且脚本自己回读 `/status` 也是绿的（它读的就是它刚推的值）。
   改成显式映射 `{"true": True, "false": False}.get(mode, 现值)`，并在入口拒收 `keep|true|false` 之外的值。
   **修完在 CPE3 上做了一次往返，两头都用 VPP 的实际行数取证，不看脚本自报**：
   `ESCAPE=false` → `/status` 报 `escape_all_colors=False`，`show ip fib 172.16.12.1/32 table 0` 只剩
   `len:1`、`pref=10` 那一条（颜色 2 的备路离开主表，符合定案「颜色 0 无 Fallback」）；
   `ESCAPE=true` → `escape_all_colors=True`、`routes_set=1`、行回到 `len:2`（`pref=10` 承载、`pref=12` 装着不承载）。

---

## 八、这一枪没有回答的

- **反向流量**。两臂都只量被测盒（CPE3）朝 `172.16.12.1` 的去向。对端（CPE1/CPE2）朝
  `172.16.3.1` 的回程是它自己的主备形状决定的，要两端同时打计数器才能钉死方向归属。
  §2.1 时代那个「900ms 断流窗口」的推断（主要是反向）至今没有分向计时证据。
- **BFD 检测时间本身的构成**。local 臂量到口 down → BFD 判 down 是 330ms，
  而会话参数是 Detect mult 3 / Detection Time 150ms / Min Rx-Tx 50ms。
  330ms 比 150ms 大，差的那一段是采样分辨率（107ms）加上口 down 到 BFD 报文真的停发之间的延迟，
  本枪没有拆开。要拆得在口 down 那一刻同步抓 BFD 报文计数。
- **#262 的抑制**。本枪两臂都是 20s 的长故障，量不到「亚秒抖动被放大成 ≥1s 黑洞」那个缺陷。
  备路那些自然抖动正是它的现场，但本枪没有对它计时。