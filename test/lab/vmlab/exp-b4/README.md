# B4：sr-route 重启后本地前缀集无人重建 —— 修法与实测（2026-11-09）

owner 定的形状是 **(b)**：不改数据面形状、不引入周期重发，只让 sr-route 起来后主动向
sr-frr 的控制口要一份全量（照 zebra 的形状：新客户端一连上，daemon 立刻 announce）。

## 1. 缺陷是什么形状（场景 → 代码走到哪 → 症状）

场景：一台正在跑业务的 CPE 盒，`sr-frr` 不动，只把 `sr-route` 换件/重启（抬版本就是这一枪）。

代码逐步走到哪：

1. 这台盒子的客户侧前缀（lab 里 CPE3 是 `172.16.3.1/32`）本来就住在 `sr-frr` 里：
   zebra 把它学到的路由交给 `internal/frr` 的 Importer，`cmd/sr-frr/main.go:769` 每轮
   `reporter.reconcile(ctx, reps)` 把集合发给了 sr-route。
2. `reconcile`（`cmd/sr-frr/main.go:1006`）在已经 `synced` 的前提下只算差量
   （`diffLocalPrefixes(r.lastSeen, next)`，`:1014`/`:1041`）。集合没变 ⇒ 差量 0 条 ⇒
   `posted=false` ⇒ **一次 HTTP 都不发**。
3. `sr-route` 那份本地前缀集只在内存里，没有盘上副本，起来时也不问任何人 ——
   它只是等着。于是 `PUT /local-prefixes` 这条路在这一代进程上永远不会再被触发，
   直到集合**恰好**因为客户侧变动（路由抖一下、FRR 重连、prefix-list 改动）而变化。
4. 集合为空 ⇒ 本盒不向 RR 宣告自己的客户前缀 ⇒ RR 按会话断开摘除 ⇒ 全网别盒都失去
   这条路由，而**流量方向上没人知道这个前缀已经换了主人**：远端还在把该前缀的包往这台
   盒子的隧道里塞（#271/#272 那一对回灌/黑洞形状），本盒却不宣称所有权。

症状就是本目录要量的那条：不自愈。这与 #276 切片 A 的事件化无关 —— 事件化解决的是
"变了要马上说"，这一条是"说了对方没收到，因为对方重启了"。

## 2. 修法

两条腿，都在原进程上热做，不涉及任何 daemon 重启（AGENTS.md §1.1）：

- **sr-frr 侧**：控制口新增 `GET /local-prefixes`（`cmd/sr-frr/main.go:1192`），复用
  已有的 `localPrefixesDoc`/`localPrefixJSON` 结构，所以返回体和 sr-route 接受的
  `PUT /local-prefixes` 逐字节同形。答之前先过一道"可信空集"闸门（`:1198`）：
  `len(rc.rules)>0 && !st.zebraConnected()` 时回 **503** 并带上它自己的话
  （"反向导入未就绪（zebra 未连接或 VRF 设备缺失），请稍后重试"）—— 还没连上 zebra 时
  "当前一条也没有"证明不了任何事，宁可不答，也不给一个假的空集（§1.3 不许用乐观窗口盖状态
  缺口 + §2 不造数据）。规则集为空是**可证明**的可信状态（没有规则 ⇒ `Report()` 永远给不出
  东西），所以那条走 200 + 空数组；否则 `ensureImport` 在 `subs` 为空时把 `st.cl` 留成 nil，
  只看 `zebraConnected()` 会让"没有泄露规则的盒子"永远 503。
- **拉完之后有序问题**：应答同时调用 `reporter.demote()`（`cmd/sr-frr/main.go:996`），把
  "sr-route 手里就是 lastSeen"这个信念作废 ⇒ 下一轮（≤poll_sec，或来一条路由时的 10ms 合并）
  必然补发一次**全量 PUT**。这就是为什么"拉"和"推"并发不会留下两边不一致的窗口：不引入版本号、
  不对时钟，靠一次注定发生的重发把顺序关掉。半途中断的拉取也无害 —— 全量重发在后面，
  而且刚重启的 sr-route 本地集按定义是空的。
- **sr-route 侧**：新文件 `cmd/sr-route/pull.go`。`pullLocalPrefixes`（`:61`）起来就打一次
  `GET`，失败每 2s 重试（`pullRetryEvery`，`:52`），**拿到第一份可信答案就退出**，之后一切
  变化仍然由 sr-frr 推 —— 不留周期重发（那是 §1.3 另一种形式的乐观窗口）。装回走
  `route.Client.SetLocalPrefixes`（`internal/route/client.go:547`，全量替换、要么全装要么不装）。
  解出来的体用 `DisallowUnknownFields`（`:111`）：`prefix` 和 `prefixes` 拼错会被当成"空集"，
  而这一次是整表替换，等于把本盒所有已宣告前缀撤掉。
- ** socket 路径的找法**（`srfrrControlSocket`，`:149`）照 sr-apply 的三级阶梯抄：
  显式 `-srfrr-sock` > `/etc/sr-frr.json` 里的 `control_socket` > `/run/sr-frr/control.sock`。
  选"读 sr-frr 自己的文件"而不是给 sr-route 的配置加一个新键，是为了不造第二个事实来源
  （控制器从不写这个键）。配置文件不存在 ⇒ `ok=false`，与开机清单 `do_bridge` 的
  `BRIDGE_CONF` 门禁同一个判据，于是不跑 sr-frr 的盒子只会打一行实话，不会永远重试。
  `-srfrr-conf` 的默认值取 `SR_BOX_BRIDGE_CONF`，和那份清单读的是同一个环境变量。

开机顺序天然是 sr-route 先起（`controller/deploy/sr-box/start.sh` 里 `do_route` 在前、
`do_bridge` 在后），所以 socket 那一刻还不存在，这条重试路径是设计里必须有的一半。

## 3. 判据脚本

```bash
# A 臂（缺陷）：老 pair，只重启 sr-route，观察 150s
bash test/lab/vmlab/exp-b4/b4-arm.sh CPE3 150 cust12 172.16.12.1 172.16.3.1 A
# 人工把手臂：只重启 sr-frr（这就是修之前运维唯一的办法）
bash test/lab/vmlab/exp-b4/b4-heal.sh CPE3 30 cust12 172.16.12.1 172.16.3.1
# B 臂（修法）：新 pair，同样只重启 sr-route
bash test/lab/vmlab/exp-b4/b4-arm.sh CPE3 40 cust12 172.16.12.1 172.16.3.1 B
```

脚本形状只有一条：SIGTERM 老进程 → 产品自己的开机清单（`sr-box-watch.service` →
`start.sh up`）把它补回来。不 `kill -9`、不 `systemctl restart` 业务单元、不手改
FRR/VPP/内核路由。**sr-frr 全程不许动** —— 动了它 reporter 就复位、重发全量，缺陷和修法
同时治好，这一枪就量不出归因了。

换件走仓库里那三步（`exp-276/push276.sh` → `install276.sh` → 换进程），产物在容器里
`GOOS=linux` 编：`sr-route 1161534d5c91db40`、`sr-frr 626b126a331f65e0`。

## 4. 实测（2026-11-09，被测盒 CPE3，探测机 cust12）

基线：`sr-route local=1`（`{vrf_id:0, prefix:"172.16.3.1/32"}`）、`rib_routes=3`；
`sr-frr reported=1`、`zebra_connected=true`、pid 822068；`sr-route` pid 821964（跑=盘=`26044f78f7969935`）。
四台盒每台各 1 条 local，CPE1/CPE2 同前缀（同站点），CPE3/CPE4 各自唯一 —— 所以只有 CPE3/CPE4 能做这一枪。

| 臂 | 进程形状 | `local` 结果 | sr-frr | 客户探测（cust12 → 172.16.3.1） |
| --- | --- | --- | --- | --- |
| **A 缺陷**（老 pair，只重启 sr-route） | 老 pid 821964 SIGTERM → 新 pid 844498 用时 **1635ms**，跑仍是 `26044f` | **t+1s 变 0，150s 末仍 0**（不自愈） | pid 822068 未变、`reported=1` 全程 | 299 发里 **150 发 FAIL**，连续断流 158.66s 且到探针结束**没闭合** |
| **人工把手**（只重启 sr-frr） | 老 pid 822068 SIGTERM → 新 pid 846954 用时 **1402ms**，跑 `626b126a` | **t+1s 回到 1**（sr-route pid 844498 全程未变） | 新进程控制口 `GET /local-prefixes` → **HTTP 200**，体 `{"prefixes":[{"vrf_id":0,"prefix":"172.16.3.1/32"}]}` | 1547 发 2 FAIL（窗首那段是 A 臂留下的洞，不是这一枪的成本） |
| **B 修法**（新 pair，只重启 sr-route） | 老 pid 844498 SIGTERM → 新 pid 847698 用时 **1630ms**，跑 `1161534d` | **t+1s 就是 1**，40s 末仍 1 | pid 846954、`reported=1` 全程未变 | **1899 发 0 FAIL，无断流** |

日志侧的因果，两边各读一次就够（盒子时钟比 Mac 慢 21 分钟，别按 Mac 时间去 grep）：

- A 臂：`route.log` 01:20:28 只有启动三行，**没有一行 `local prefixes = …`** ⇒
  sr-frr 一次都没推（0 差量 ⇒ 连 HTTP 请求都不发）⇒ 也没有"推失败所以补推"这条路。
- B 臂：`route.log` 01:34:05 `开机从 sr-frr 拉到客户侧前缀 1 条（/run/sr-frr/control.sock），之后的变化由它推`，
  01:34:06 `local prefixes = 1 条`。同一秒 `bridge.log` 也出现 `客户侧前缀 1 条 → sr-route` —— 那条**不是**
  独立的自愈，正是 GET 应答里 `demote()` 之后补发的全量；A 臂 150 秒里一条也没有，就证明
  "失败重试"这条路解释不了 B 臂。

前后状态对比（AGENTS.md §1.2，不是退出码）：

- `sr-fib` 本轮没换件，`/status` 仍是 `tunnels=3 vrfs=1 escape_all_colors=False`；
  VPP `show ip fib table 0` 里带标签路由 3 条不变，`172.16.3.1/32` 走
  `lcp-rt` 的 `attached-nexthop 172.31.3.2 host-enp0s8`（本盒自己的前缀天然不是 `mpls via`，别把这条当故障）。
- 四台的 `local` 集合回到基线（CPE1/CPE2 `172.16.12.1/32`、CPE3 `172.16.3.1/32`、CPE4 `172.16.4.1/32`），
  `rib_routes` 回到 2/2/3/3。
- `exp-273/six.sh` 六方向（标签 `b4-postfix`）**全 0% 丢包**，rtt avg 8.8–14.3ms。
- `vm-verify.sh`（三臂跑完、CPE3 换件之后整轮复跑，2026-11-09 实测）：**PASS=392 FAIL=0 WARN=0**。
  其中直接吃 B4 的判据是 §8「反向导入：客户机 loopback → 盒内 FRR → sr-frr → sr-route local」那一段
  19 条全 PASS —— CPE3 的 `172.16.3.1/32` 在 zebra 里是 bgp 路由、下一跳 `172.31.3.2`，
  并且只出现在 CPE3 自己的 `local` 段（其余三台的 `local` 各自只有自己的 loopback），
  说明重启 sr-route 之后本盒前缀是被拉回来的，不是别盒替它宣告的。

## 5. 部署形状里的硬规则（实测出来的，不是猜的）

**`sr-frr` 与 `sr-route` 必须同批抬，且 `sr-frr` 在前。** 老 sr-frr 没有这个端点，实测
`curl --unix-socket /run/sr-frr/control.sock http://sr-frr/local-prefixes` 回
`404 page not found` —— 只抬 sr-route 会让它对着 404 每 2s 重试到永远，空洞照旧（日志里
每 30 次报一行，不会刷屏）。反过来只抬 sr-frr 是安全的：它一起来就重发全量。

## 6. 这一轮**没有**在现场覆盖的部分（诚实口径）

1. **A 臂真实恢复时刻超出观察窗。** 探针在 158.66s 处被观察窗截断，之后到人工把手臂开打前
   （盒子时间 01:26:38–01:30:26）链路又绿了，而 `local` 到 01:30:30 快照仍是 0 —— 也就是说
   绿回来的那一段不是靠本盒重新宣告。所以这一臂只报"**≥158.66s 未自愈**"，不报闭合时长；
   那 4 分钟里链路为什么通，属于 #272（跨盒回灌）那条线的问题，记在这里不当作 B4 的功劳。
2. **开机顺序那条重试路径没有单独打枪。** 现场三臂里 sr-frr 都是长跑进程，socket 一直在，
   所以"sr-route 先起、socket 还不存在 → 每 2s 重试直到 sr-frr 起来"只有单测覆盖
   （`pull_test.go` 的 `TestPullLocalPrefixesWaitsForSRFrrReady` 两发 503 后成功、
   `TestPullLocalPrefixesStopsWithContext` 取消即停）。要补现场那一枪得让 sr-frr 真的晚起，
   那属于整盒重启的形状，这一轮没做。
3. **不跑 sr-frr 的盒子**（`ok=false` 那个分支）同样只有单测；lab 四台都在跑 sr-frr。
4. `SetLocalPrefixes` 被硬拒（VRF 未配 / 没有导出 RT）时按"继续重试"处理，不是致命错误 ——
   这与今天 sr-frr 自己那条 PUT 路（400 → 记日志 → 下一轮再来）同形，不是新行为。
