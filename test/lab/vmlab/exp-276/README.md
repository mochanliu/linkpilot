# exp-276：#276 收敛这条腿的前后实测（owner 定案：transport 选甲、形状全部照抄 FRR）

对应 `docs/queue-plan-next.md` §三 的切片 B（sr-route→sr-fib 的 SSE `/watch`）与切片 C（判据脚本 + 实测）。
owner 给这一轮的验收线是：

> 只要路由收敛性能与 frr 的长连方式不相上下就选甲

所以这一轮比较的不是"事件驱动好不好"，而是**我们这条 HTTP 事件流有没有把 sr-fib 那一拍的秒级封顶拿掉**。
下面所有数字都是 2026-11-08 在 VM 台 CPE1 上实测的，注入都是 CPE3 的 `systemctl restart frr`。

## 一、这条腿分几段（第一版量具把两段读成一段，结论翻了一次车）

场景：CPE3 的 FRR 重启一次。zebra 起来的那一刻，CPE3 的 sr-frr 按 #274 的定案把本地前缀集清空（不留乐观
窗口），于是 `172.16.3.1/32` 从 RR 撤掉、全网消失，refill 之后再回来。站在消费者 CPE1 看，从"这条前缀回来"
到"客户的包真的走我们那条隧道路径"，中间是**三段**，不是一段：

| 段 | 观测名 | 这一发采样里读到的事实 |
| --- | --- | --- |
| 1 | `in` | 本机 sr-route 的 `/status` 里有这条前缀 —— **rib 段或 pending 段任一命中都算** |
| 2 | `ok` | 它进了"可装视图"：rib 段那条带 `targets`（也就是 `next_hops` 解出来了），这正是 `viewLocked`/`AllRIB` 拿去比对的那一份 |
| 3 | `ours` | VPP 里这条前缀的转发链上真的有 `mpls via` 路径（sr-fib 写进去的那条） |

切片 B 只管 **2→3**。改前这段被 sr-fib 的轮询周期封顶（盘上 `poll_sec=1` ⇒ 平均 ~500ms、最坏 ~1s，
#262 量的"BFD 150ms 判 down、sr-fib 等到下一轮才摘路"就是它）；换成事件流之后理论上只剩 sr-route 的
10ms 合并窗 + 一次下发。**1→2 不归切片 B** —— 那是可达性输入（`path.resolvable` 只吃隧道表，#275 的地盘）。

第一版量具只有一列 `rib=grep '"prefix":"<pfx>"'`，而 `/status` 的 pending 段每条也带 `"prefix"`，于是 1 和 2
被读成同一个信号：装路边算出 873ms/887ms 这种"改了等于没改"的数，还被记到了切片 B 头上。分成三列之后
同一轮里 `1→2 知道但不可装` **一次都没有观察到**（`rsn` 列为空），2→3 的账才落回它自己头上。

**`ours` 为什么不是"VPP 里那一行在不在"**：这盒开了 linux-cp（`lcp-sync on`），内核主表的路由会镜像进 VPP
主表，署名 `lcp-rt` / `lcp-rt-dynamic`（pref=20，出口 host-enp0s8/enp0s9），而 sr-frr 正向注入的
`172.16.3.1 dev lo proto boot` 就在同一张内核主表里 ⇒ 我们撤掉自己那条 MPLS 路径之后 `show ip fib` 那一行
**还在**，只是换了署名。拿"行在不在"当输出会读到一个从来没消失的信号。所以 `line` 只用来数"第二个写手"
（`line=Y 且 ours=0` 的发数），判据一律用 `ours`。

## 二、脚本（会动状态的两把都点名）

| 脚本 | 量什么 | 动不动状态 |
| --- | --- | --- |
| `sample-leg.sh [秒=70] [前缀]` | 盒内一发采 `in/ok/rsn/ours/line` 五列 —— **这份是解释性量具**，头部写了为什么必须分列；每发多一条 `grep -o … \| sed` 捕获管道，中位 102ms | 只读 |
| `sample-tight.sh [秒=70] [前缀]` | 同一套五列，热路径除 date/curl/tr/vppctl 外不 fork（字符串判断全留在 bash），中位 60~61ms。对 `/status` 的字面形状有两个假设（rib 条目含 `"targets"`、pending 条目含 `"reason"`），头部带两条核验命令 | 只读 |
| `leg-window.sh` | 驱动：落哈希/`poll_sec`/sr-fib 起手状态 → 采样 + 探测同时起 → 第 8 秒注入 → 出前后两边的账（摘路边、装路边确认窗口、1→2 门、第二个写手、收口、fib.log 旁证） | **会 restart frr**（被测故障，见下） |
| `stamp-watch.sh [URL] [秒]` | 占住 sr-route 那唯一的订阅位，逐毫秒打戳每条 SSE 帧 —— 用来直接量"sr-route 什么时候把事件发出来" | **会占订阅位**（sr-fib 在此期间挂不上，退化成全量兜底） |
| `watch-timeline.sh` | 上面那把的驱动：请 sr-fib 让位 → 采样 + 打戳同时跑 → 注入 → 用产品自己的开机清单把它补回来 → 把两边时间轴对齐 | **会让 sr-fib 让位 ~67s**（本节"必然中断"第 5 行） |
| `push276.sh <盒> <name> <src.gz>` | 换件第一步：gzip → scp → 落地核对哈希。**只落盘，不 install、不换进程** | 写文件 |
| `install276.sh <盒> <name>` | 第二步：`cp -a` 临时件 + `chmod 0755` + `mv -f`（同盘 rename，老进程抱着老 inode 继续跑）到 `/usr/local/bin`，打印新/老盘哈希 + 跑着的那份 + 字节数（push 那步若被截断，字节数会当场露出来）；空产物直接拒绝 | 写文件，不换进程 |
| `swap276.sh <盒> <name> [探测机] [源] [目的]` | 第三步：SIGTERM → 产品自己的 `sr-box-watch.service` 补回 → 新 pid 的 run==disk、盯的前缀 `mpls via` 条数（默认取探测目的 `/32`，并标明它是不是本盒 local）、换 sr-route 时检查 `local` 集是否被 B4 掏空，全程 75 秒客户探测跟着 | **会 SIGTERM 该进程** |

**注入的口径**：`systemctl restart frr` 是这一枪选的被测故障（等价于"控制面接缝断一次 + 客户侧会话重建"），
不是配置生效路径 —— AGENTS.md 第 1 条禁的是"用重启让配置生效"，不是禁拿重启当实验（同 exp-274）。

**A/B 的控制变量**：两臂用的是**同一份 sr-route**（`26044f78f7969935`，已经带 `/watch` 和那 7 个挂钩点），
唯一差别是 sr-fib 的产物：`bab523ff3809f57b` = 每 1s 全量轮询那一版，`a9f1bc8c8fd49016` = 事件驱动那一版。
所以下面表里的差只可能是"谁触发收敛"造成的，不掺 sr-route 自己的改动。

## 三、实测：改前（轮询）vs 改后（事件），同一台、同一前缀、同一量具口径

摘路边给的是**保守上界**（"最后一次看见它还在"到"第一次看出路径没了"，两个观测点之间才是真值）；
装路边给的是**确认窗口**（同一发里 `ok=Y 且 ours=0` 同时成立的最长连续段 + 一个分辨率），不是推算。

| 臂 | sr-fib 产物 | 摘路边 `ok` 还在 → 路径下架 | 装路边 `ok=Y` → `ours=1`（最长连续段） | 分辨率 |
| --- | --- | --- | --- | --- |
| 改前（轮询 1s） | `bab523ff` | ≤456ms | 868ms | 76ms |
| 改前（轮询 1s） | `bab523ff` | ≤593ms | 978ms | 76ms |
| 改后（事件流） | `a9f1bc8c` | ≤40ms | 201ms | 61ms |
| 改后（事件流） | `a9f1bc8c` | ≤41ms | 755ms | 60ms |
| 改后（事件流） | `a9f1bc8c` | ≤45ms | 294ms | 60ms |
| 改后（事件流，解释版量具） | `a9f1bc8c` | ≤79ms | 272ms | 102ms |

两条边都比改前快了，摘路边快了一个数量级（≤456/593ms → ≤40/41/45ms，剩下的正好是 10ms 合并窗 + 一次
HTTP 投递 + 一轮收敛）。装路边从"最坏接近 1s 的周期封顶"变成 201~755ms，**残余不再由轮询周期封顶**，
但也没有到分辨率以下 —— 第四节说这笔账在谁头上，第五节说它现在拆不到多细。

同一轮里另外几条读到的事实：

- `1→2 知道但不可装`：改前改后**一次都没有观察到**（≤1 个采样周期）。这条前缀在 lab 里只有 1 条路径、
  隧道一直是 Up，所以 sr-route 的可装门没有在这轮里当过瓶颈 —— 不能拿这个"没有"当"门不存在"的证据，
  它只说明本轮的场景不走那条门。
- 整条前缀不可用（`ours` 从掉到回）5.33~6.66s，**改前改后同量级**：这段几乎全是 CPE3 侧 zebra 重新收录
  K 路由 + sr-frr 重新上报 + RR 重新反射的 refill，不归这条腿。
- 第二个写手：`line=Y 且 ours=0` 每轮 68~125 发（60ms 一发），即这条前缀在 VPP 里被内核镜像接管过；
  探测机 cust12 侧同窗口 FAIL 7 发（1 秒一发），形状一致 —— 镜像那条不是"客户还在通"，它是黑洞本身。
- 收口：每一轮末样本都是 `in=有 ok=有 ours=1 line=有`，没有任何一轮以黑洞结束。

两个形状（CPE1 自己的墙钟，最后一列是 `ours`）：

```
改前 poll1：1794134261.232 in=N ok=N ours=1 line=Y     ← sr-route 已经不知道了
            1794134261.620 in=N ok=N ours=0 line=N     ← +388ms 才把我们的路径摘掉（等下一轮轮询）
            1794134262.260 ……                ours=0 line=Y ← 内核镜像接手
            1794134267.270 in=Y ok=Y ours=0 line=Y     ← refill 回来
            1794134268.171 in=Y ok=Y ours=1 line=Y     ← +901ms 我们的路径才回到转发链
改后 tight3：1794108297.876 in=N ok=N ours=0 line=N    ← 同一发里就掉完了
             1794108298.191 ……               line=Y
             1794108302.903 in=Y ok=Y ours=0 line=Y
             1794108303.175 in=Y ok=Y ours=1 line=Y    ← +272ms
```

## 四、事件时间轴：sr-route 发得不慢，残余在它下游

`watch-timeline.sh` 那一枪直接把 sr-fib 请出去、自己占住订阅位打毫秒戳（1106 发采样 / 分辨率 55ms）：

```
1794134012.876  INIT    本批 2 条（新订阅必吃一次全量 —— 这是拍板 B2 的兜底，不是事件）
1794134019.657  批次    1 条  op=withdraw
1794134025.674  批次    1 条  op=upsert
```

对齐同一时刻的采样：

- **摘**：最后一次看见 `ok=Y` 在 …19.515，第一次看出 `ok=N` 在 …19.598；withdraw 帧到达 19.657
  ⇒ 相对"最后一次还在"≤142ms，相对"第一次看出消失"**+59ms**。
- **装**：最后一次看见 `ok=N` 在 …25.590，第一次看出 `ok=Y` 在 …25.666；upsert 帧到达 25.674
  ⇒ ≤84ms，相对"第一次看出回来"**+8ms**。

（第一版这里算出过 -12714ms 的鬼数，因为脚本把 INIT 那批也算成事件批 —— INIT 里每条前缀都是 upsert。
`watch-timeline.sh` 现在只在非 INIT 帧里找，已修。）

所以装路边那 201~755ms **不是 sr-route 发得晚**，它落在下游：sr-fib 收到批之后的那一轮收敛（读 BFD 会话、
算 plan、下发）+ VPP 把这条路径变成"转发链上那一个"。

## 五、残余拆不到更细的地方（登记，不掩盖）

1. **盒内的日志只到秒**。tight3 那一轮：`ok=Y` 的确认窗口 ≤1794108302.903，`ours=1` 的第一发 ≥1794108303.137，
   中间 fib.log 记着"2026/11/08 03:25:02 收敛 routes+1/-0"。按秒级日志推，sr-fib 那一轮 ≤140ms、VPP 侧
   140~320ms —— 但这个拆法带着 ±1s 的日志误差，只能当形状看，不能当两个分量各自的实测值。
2. **`ours` 测的是"谁在转发"，不是"sr-fib 写过没有"**。让 sr-fib 让位那一枪（CPE1 的墙钟 10:33:31 `sr-fib: 停止`
   → 10:34:38 才起来，中间 67s 里没有任何 sr-fib）里，`ours` 依然 1→0→1：10:33:40 掉、10:33:46 回；
   同一秒 CPE1 的 `bridge.log` 记着 sr-frr `收敛 routes+0/-1（vpn0→table 254）` 和 `+1/-0`。
   ⇒ 这条前缀在主表里的转发链，**sr-frr 的内核注入 + linux-cp 镜像也能改它**，不需要 sr-fib 动手。
   所以装路边那 201~755ms 里有多少属于 sr-fib 的收敛轮、有多少属于"这条链换人"，本轮**没有拆开**。
   要拆的话两条：给 sr-fib 的收敛轮打毫秒时间戳（只改日志，不动生效路径），并且在 `ours=0` 的那一发
   同时 dump `show ip fib <pfx>` 全文看署名，而不是只数 `mpls via`。
3. 只量了一条前缀、一个消费者（CPE1）、2 条隧道的 lab。规模档（每盒 4096 条）的合批与下发没在这里量过。
4. CPE1/CPE3 的钟都没对时（CPE1 比 UTC 快 ~17 分钟，CPE3 差得更多），所以跨机只比时长与形状，
   所有毫秒级结论都出自 CPE1 自己那一路采样。

## 六、必然中断与实测时长（AGENTS.md §1 第 4 条）

| 这一枪 | 中断形状 | 实测 |
| --- | --- | --- |
| `swap276.sh` 换 sr-fib（poll 版 ↔ 事件版） | SIGTERM → 清单补回 0.1~1.6s | 事件版→轮询版 **0/947 发 FAIL**；轮询版→事件版 **0/959 发 FAIL** |
| 重启 sr-fib 本体（同产物） | 同上 | **0/1561 发 FAIL** |
| `swap276.sh CPE1 sr-fib` 同版本换件（脚本自检，2026-11-08） | SIGTERM → 清单补回 1831ms / 1804ms，新 pid 起来后 `mpls via`=1 | **0/2561、0/2544 发 FAIL**（两枪都 0） |
| 重启 sr-route | 订阅位空出来，sr-fib 立刻重挂并先补一次全量 | **0/1003 发 FAIL** |
| `PUT /config` 把隧道恢复回去 | 不改进程，只改表 | **0/1339 发 FAIL** |
| `watch-timeline.sh` 让 sr-fib 让位 67s（去占订阅位） | 期间没人重算，已装路径原地不动 | **没挂探测机 —— 只有形状，未测**。它的下界是"什么都不撤"，上界引用同形状的"重启 sr-fib 0/1561"。要收口就补一条 30~70s 探测（停 timer → kill → 探测 → `systemctl start` 补回） |
| 换 sr-frr（本轮为了补回 local 集打的这一枪，见第七节 B4） | SIGTERM → 清单补回 | **0/2803 发 FAIL** |

一句话：换件与重启这一族在本轮全是 0 丢包（`install276.sh`/`swap276.sh` 这两把量具本身也在 2026-11-08 用一次同版本换件验证过，0 FAIL）；唯一没测的是"故意让 sr-fib 不在位 67s"那一枪，已点名。

## 七、这一轮顺手抓到的两条新缺口（要 owner 拍，没在切片 B 里动）

**B3 —— watch 模式下可达性没有任何重收敛触发。**
`path.resolvable` 的唯一来源是隧道表：`internal/route/client.go:1142` 写着"It is NOT the source of
path.resolvable: that flag is the tunnel gate alone"，`:1169` 写着"(SetTunnelState). This is the only thing
path.resolvable is set from"。BFD 的会话状态只在**一轮收敛里面**读（`cmd/sr-fib/main.go:301`
`f.dp.BFDSessions(ctx)` → `fib.UnreachablePeers` → `internal/fib/fib.go:306` 那条"该路径跳过"警告），
而 watch 之后触发收敛的点只剩三处：开机那一次（`main.go:509`）、事件批（`watch.go:215/233/253`）、
`PUT /config`；`retryGap` 只是断流重挂的间隔，不再是周期收敛。
后果分两半：**已装路径不会因为 BFD 判 down 被撤**（客户流量继续往死隧道里灌，直到下一次别的原因触发收敛），
**因 BFD 没 Up 被跳过的路径也不会自己回来**。改前是 1s 一轮轮询误打误当了这个触发器 —— poll2 现场：
`10:39:25 警告 … BFD 会话不是 Up，该路径跳过` → `10:39:26 收敛 routes+1/-0`，靠下一轮救回来。
FRR 的形状是 BFD 状态变化异步通知 rib（`bfdd → zebra → rib` 事件），照抄过来应该是 sr-fib 订阅 VPP 的
BFD 会话状态事件、在回调里 `signal()`；#262 想加的抖动抑制（hold-down）也必须挂在这同一个触发点上，
否则抑制窗一开就把"唯一那 1s 一轮"的兜底也删掉了。**这条不修不许把 sr-route/sr-fib 抬到其余盒。**

**B4 —— 对端进程重启之后，本机客户侧前缀集没人负责重建。**
sr-route 的 `local` 集只在内存里，靠 sr-frr PUT 进来；sr-frr 的 reporter 是"集合变了才发"
（`cmd/sr-frr/main.go:746-776`：`reporter.reconcile` 返回 `posted` 才打那行日志）。本轮为了做 A/B 重启过
CPE1 的 sr-route，`/status` 的 `local` 于是变成空，并且**一直空着**：`bridge.log` 上最后一次
`客户侧前缀 1 条 → sr-route` 是 11-07 01:37:03，下一次是 11-08 11:01:25（我把 sr-frr 重启才补回来，
那一次带探测 0/2803）。`vm-verify.sh` 的 5 条 FAIL 全是它的下游 —— CPE1 local 空、CPE3/CPE4 缺
`172.16.12.1/32` 的 `10.250.0.0`/`10.250.0.2` 两条路径、主表里没有对应 `mpls via`。
切片 A 的 delta 版同样只在"HTTP 失败"时退回全量，**sr-route 重启不是一次失败**，所以它也漏。
三个候选形状（要拍的就是这个岔口）：(a) reporter 定期重发全量（最简单，代价是每秒一次全量 PUT）；
(b) sr-route 起来后主动问 sr-frr 的控制口要一份（`/run/sr-frr/control.sock` 已经有 GET /status）；
(c) sr-route 给 local 集落盘当事实快照 —— 那就是 G-2 对隧道表提的同一件事，可以并一条做。
队列里"抬 sr-route 到其余盒"是下一件事，**它每一枪都会踩这条**：升完 sr-route 的那台盒子，自己的客户侧宣告
会静默消失，直到客户路由自己抖一下。

## 八、重跑

```
# 0) 产物与换件（两臂之间来回，全程不换机器不重启 VPP）
docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'
gzip -c <linux 产物> > /tmp/sr-fib.gz
bash push276.sh CPE1 sr-fib /tmp/sr-fib.gz          # 只落盘
bash install276.sh CPE1 sr-fib                       # 只装盘，不换进程
WORK=/tmp/p276 bash swap276.sh CPE1 sr-fib cust12 172.16.12.1 172.16.3.1   # 换进程 + 中断时长

# 1) 这条腿的前后（每臂各跑，标签区分）
WORK=/tmp/p276 TAG=poll1 SAMPLER=$PWD/sample-tight.sh DUR=60 bash leg-window.sh CPE1 CPE3 172.16.3.1/32 cust12 172.16.12.1
WORK=/tmp/p276 TAG=tight1 SAMPLER=$PWD/sample-tight.sh DUR=60 bash leg-window.sh CPE1 CPE3 172.16.3.1/32 cust12 172.16.12.1

# 2) 事件时间轴（会让 sr-fib 让位 ~70s，第六节第 5 行那一枪）
DUR=60 bash watch-timeline.sh CPE1 CPE3 172.16.3.1/32

# 3) 全 lab 判据
bash ../../vm-verify.sh                       # 392 PASS / 0 FAIL / 0 WARN
TAG=276 bash ../exp-273/six.sh                # 六方向 0% 丢包
```

原始读数不入仓库（`/tmp/p276/`：`leg-*.txt` 逐发采样、`probe-*.txt` 逐发 ping、`ev-*.txt` SSE 帧打戳、
`fiblog-*.txt` 收敛日志摘录、`window-*.txt` 每轮汇总、`timeline-*.txt` 时间轴对齐），重跑即生成。
