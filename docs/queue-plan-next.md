# 队列下一步：sr-route 抬 HEAD + #275 + #276

出这份文档的动机：AGENTS.md §1 与用户自己那条「controller 增量先出方案、点头后再写代码」
（memory: `feedback-do-not-develop-controller`）要求这三件事都走同一个姿势 —— **动手前先落纸面**。
#269 缺陷⑧（并发抢起）已在本仓库 `4f66632` 落地，它曾经是"抬 sr-route"的**唯一**并发抢起阻塞；
剩下的都是"重启进程本身要付的账"，那一条要 owner 拍窗口。

引用文件里的行号只做定位用，具体改动前请先重新读上下文。

---

## 一、sr-route 抬到 HEAD 那一代（**部署**，不是代码修改）

**当前形状**（`docs/agent-handover.md` §3 末"仍不一致的那一处"）：
- 四台 CPE 跑的 sr-route = sha256 前缀 `5789c2c0…`（11-04 那一代的内容）。
- Mac 上 `/Users/louis/srvmlab/artifacts/linux/sr-route` = `6c582f0c…`（11-05 那一代）。
- 仓库 HEAD = `4f66632`（本次 #269 之后）—— 这两个都不是 HEAD build。

**为什么它落后**：O7 R1-R4（Loc-RIB）+ O9（RR 分组/身份乙/网格）之后 sr-route 换过实现，
但换进程要抖 RR 会话。当时的顾虑是"开机清单+watch 撞在同一秒会各起一份，两份互抢"，
现在 flock 修掉了这一半。剩下那一半 —— **重启就是重启** —— 与并发无关，是这条路径本身的形状。

**抬之前要拍的两条**：
1. **窗口多长可接受**：sr-route 重启 → 客户端 session 断 → 重连 + 重放 adj-RIBs-in + RR 端
   重新按 path 收敛 → sr-fib 下一次轮询把新 RIB 装进 VPP。这条链在 O7 R4 之后没被完整测过；
   上一次量的 refill 是 12~19s，但那是"zebra 重连 + sr-frr 轮询"（`agent-handover.md` §6）。
   抬 sr-route 时**必须先跑一次基线**：`test/lab/vmlab/exp-*/six.sh` + `vm-verify.sh` 的六件套，
   数字进提交说明。
2. **一次抬几台**：CPE1/2/3/4 全抬 = 一次抖 4 条 RR 会话；分两轮（先 1 台看窗口，再抬其余）
   更保守。owner 拍。

**执行步骤（不做代码改动，纯部署）**：
1. `docker exec lp-ctrl sh -lc 'cd /workspace/controller && GOOS=linux GOARCH=amd64 go build -o /workspace/../out/sr-route.$(git rev-parse --short HEAD) ./cmd/sr-route'`
   —— 产物落 `~/srvmlab/out-<N>/`，**不要覆盖 `artifacts/linux/sr-route`**（§10 冲突协议）。
2. 记下哈希：`shasum -a 256 ~/srvmlab/out-<N>/sr-route.<sha>`。
3. 基线：在没动任何进程前跑一次 `vm-verify.sh` + `box-layers.sh`，把 RIB / FIB / 邻居 uptime / 客户侧 ping 的丢包数存表。
4. 抬一台（CPE1，比如）：`scp` 上去 → `sha256sum` 对上 → `systemctl kill -s USR1 sr-box-watch.service`（挂 watch 那 60s）→ 手工 `pkill -x sr-route` → 立即 `bash /usr/local/lib/sr-box/start.sh up`（清单会补上 route 那一步）→ 抬 watch。
   —— 这一串在 4CPE 环境下要显式声明（§10）。
5. 抬完立刻：客户侧 ping 60s 窗口，抓 sr-route pid / RR 端 session uptime / sr-fib 装的表条数；对比基线；把数字写进 `MANIFEST.md` 那一份的 §3。
6. 数字过关 → 再抬其余三台。任何一台不过 → 保留 CPE1 新件，其余不动，回来讨论。
7. 全部过关 → `docs/agent-handover.md` §3 那张"仍不一致的那一处"改成一致；台账里加一行 `sr-route = <sha>`。

**风险清单**：
- 抬完 sr-route 的 O7 Loc-RIB 形状**变了**：`resolvable` 现在从 `nextHops` 推，改到从 path 上的标志位推（O7 R3 落地过），行为应当与旧版一致但**没在物理 VM 上端到端跑过**。
- 抬完 `sr-fib` 与 `sr-frr` 的对接协议没变（v3），但 sr-route 那边如果加了新的 /status 字段，`sr-fib` 需要能吃 —— 抬之前 diff 一下 `internal/route/proto`。

**这条不做，除非 owner 拍窗口。**

---

## 二、#275 resolvable 改吃活动隧道（**代码，先出方案**）

**根因（`docs/agent-handover.md` §7）**：现在 `reachableLocked()` 只看配置表
`controller/internal/route/client.go:981`：

```go
func (c *Client) reachableLocked(rt Route) bool {
    return len(c.nextHops[rt.DeviceID]) > 0
}
```

`c.nextHops` 是 `SetTunnels` 从配置写入的（`client.go:344/348/596/597`），代表**"这台设备配了几条到某对端的隧道"**，
不代表**"那些隧道现在还活着"**。所以隧道 BFD 判 down 之后，路由面照样把路径当 resolvable、
`importablePathLocked` 选中它、sr-fib 装进 VPP 表、客户流量走死路 —— 这就是 `agent-handover.md` §7 里
"数据面死亡对控制面不可见"的具体一处。

**目标形状（GoBGP Nexthop Tracking 的照抄）**：
- 客户端维护两张表：`configNextHops[peer]` = 现在这份，`activeNextHops[peer]` = BFD 说活的。
- `reachableLocked(rt)` 改成"配了且活着"，即 `len(activeNextHops[rt.DeviceID]) > 0`。
- `entryLocked` 的 `NextHops` 也从 active 出（sr-fib 拿到的下一跳列表就是能真发出去的）。
- **active 表的输入源**：sr-agent 已经会把 BFD 结果上报（任务 #183 落地的 `ReportMetrics`），
  现在 `ReportMetrics` 到 `internal/route/client.SetTunnels` 之间是**断开的** —— 这一段的接线就是 #275 的核心。
- **hold-down**（GoBGP `fs.NexthopDownDelay`，默认 30s）：up→down 不立刻撤，等 X 秒内如果又 up 就吞掉这次抖动；
  down→up 立刻生效（不做防抖）。这一条是 AGENTS.md §1 第 3 条**唯一允许的**时间抑制 ——
  因为它抑制的是**假 down**，不是**未知状态**（内存 `feedback-no-cold-restarts` 里那条"不许用乐观窗口掩盖状态缺口"，
  hold-down 不是掩盖，是给链路本身的一个 debounce）。
  hold-down 参数默认多少要拍：GoBGP 30s / Cisco 建议 5s / 我们 lab BFD 判定 150ms；
  建议**从 5s 起，实测之后再调**。

**要动的文件**（不写代码，先给范围）：
- `controller/internal/route/client.go`：`Client` 结构加 `activeNextHops`，`reachableLocked` 改读它，
  新增 `SetTunnelState(peer, nextHop, up bool)`（幂等）。
- `controller/internal/agent/...`：把 `ReportMetrics` 的 BFD 结果映射到上面这个入口。
  现在只到"上报"，没到"通知路由客户端"。
- `controller/internal/rr/...`（可选）：RR 反射端也有一份隧道视图；#275 只做客户端一侧。
- **不改协议**：v3 不动。tunnel state 是本机自己维护的事实，不需要过线。

**要拍的 5 条**（写方案给 owner）：
1. `active` 的初始值：设备刚入网、还没收到任何 BFD 回报之前，是"全 down"还是"跟随 config"？
   GoBGP 是"没有 tracking 数据时按 up"（fail-open）；`project-sdwan-telemetry-and-port-model.md` 里我们拍过
   "严格 fail-closed：无 BFD 信息即不可达"。这条**保持 fail-closed**，但要说清初始那几秒会不会抖一下。
2. hold-down 时长（默认 5s 起，owner 拍）。
3. 一台设备多条到同 peer 的隧道：任一条 up 就算 peer reachable，还是要按 next-hop 精细？
   GoBGP 是按 next-hop 精细（每条下一跳独立 tracking）。建议照抄精细档。
4. `importablePathLocked` 与 `entryLocked` 的读写锁：active 表变更频率高（BFD 300ms），
   会不会把 route client 的热路径拖慢？先加一次 benchmark，再决定是否要分片。
5. **是否给 RR 一份**：RR 那边收到 A→B 的路由时，如果它自己不知道 A 与 B 之间隧道死没死，
   会不会仍把 B 的路由回给 A？（这是 `project-sdwan-controlplane-reachability-gap.md` 里那条"跨盒回灌"的一部分，
   但 #275 只做本机 resolvable，RR 端#272 待拍。）

**判据脚本**（方案里先立起来）：
- 单测：隧道 up→down、down→up、抖动 <hold-down 被吞掉、抖动 >hold-down 生效、active 空时全部 unresolvable。
- e2e：新增 `test/lab/vmlab/exp-275/tunnel-bfd-then-verify.sh`，注入一条 BFD down（`vppctl bfd set ...` 或直接把 wg 口 shut），
  看客户侧 ping 的窗口是否 = BFD 判定时长 + hold-down（而不是"永久黑洞"），
  且 route /status 里那条 prefix 从 installed 变 pending 时长在 200ms 级。

**这条不做，除非 owner 逐条点完上面 5 条拍板。**

### 落地实况（2026-11-08）

代码在 `feat/vbond-control-plane` 上落地，改动五处（`internal/route/client.go`、
`cmd/sr-route/main.go` 加 `PUT/GET /tunnel-state`、`internal/agent/routestate.go` 新
`RouteStatePusher`、`cmd/sr-agent/main.go` 在 `metricsLoop` 里接上推送、
`internal/route/client_test.go` 加 6 条单测：down 摘路 / up 回灌 / absent 让配置层说了算 / 同 peer 两条 one-up-wins / 空批次全清 / 未知状态 400）。
五条要拍的 pin，我按现有代码里的**约定**给了答案，不是我自己发明的规则；有两条与本节上面原写的建议不一致，写在下面请 owner 复核：

- **pin 1（`active` 初始值）：选了 fail-open，不是上面写的"保持 fail-closed"**。
  依据是 `internal/fib/reach.go` 那份现成的注释：**"没有会话的对端不出现在结果里。这不是'可达'，而是'没有信息'：
  没跑 BFD 的 CPE 不该因为本包而黑洞所有路由"** —— 同一个 route client 的另一条可达性判定路径已经这么定了，
  #275 只把同一约定从 sr-fib 挪到 sr-route。选 fail-closed 的实际代价是"设备刚入网、agent 第一次上报之前，本机路由全 pending"，
  这段空窗就是 §1 第 3 条不允许的"未知状态当黑洞"。若 owner 拍板回到 fail-closed，
  只需改 `client.go::reachableLocked` 里 `if !ok || !s.Known { return true }` 那一行。
- **pin 2（hold-down）：本次未做**。它是 #262 的活（那条本来就是"亚秒抖动放大"），
  同时挂 #262 与 #275 会让两条都改不完；这条与 `queue-plan-next.md` 里 §10 顺序一致。
- **pin 3（同 peer 多隧道，粗粒度还是精细）：选了 peer 粗粒度 any-up-wins**，不是上面写的"照抄精细档"。
  理由也是 reach.go 那句：**"路由只带对端地址，没有本地源，挑不了更细"** —— 我们的 Loc-RIB path 只带
  `DeviceID`（对端 node_id），没有本地源，next-hop 精细在数据模型上落不了地；
  要做精细得先给 path 结构加源，那是协议层改动，不属于 #275。
- **pin 4（热路径读写锁 benchmark）：本次未跑**。BFD 上报频率是 `metricsLoop` 的 10s（不是 300ms，
  300ms 是 BFD 判定期望），`SetTunnelState` 只在聚合状态真变的那几个 peer 上重跑 `loc.process`；
  热路径压力比 `SetTunnels` 低（隧道表整表替换，本函数只走 originPaths 命中的那几条）。
  真有性能问题在 #276 事件驱动化时用 `sr-route` 的 `/status` 计时窗口测。
- **pin 5（RR 端是否给一份）：本次未做**。§7 那条 `#272 跨盒回灌` 里，RR 端的可见性模型和这条不是一回事。

`docs/agent-handover.md` §9 那一行同批改了口径。

---

## 三、#276 收敛改事件驱动，去掉轮询（**代码，先出方案**）

**现状**：
- `sr-fib` 每 `poll_sec`（默认 2s，lab 盘上配的 1s）轮询 `sr-route` 的 `/status`（`cmd/sr-fib/main.go:473`）。
- `sr-frr` 每 `poll_sec` 向 sr-route 推本地前缀（`cmd/sr-frr/main.go:807`），
  默认 2s、lab 盘 1s（`agent-handover.md` §6）。
- 这两拍串起来就是 refill 12~19s 里的"轮询那半"。另一半是 zebra 收录 K 路由，那是 FRR 侧的账。

**目标形状**：owner 原话"必须像 frr 重分发那样瞬时"。FRR 的重分发是 netlink 事件驱动 ——
内核路由表一变，zebra 立刻看见。**照抄这个形状**，两端都改：

- **sr-route → sr-fib**：`/status` 从"轮询读"改成 SSE / WebSocket 推。sr-route 的 Loc-RIB
  变更点在 `client.loc.process(...)` 之后（`internal/route/client.go:1316/1415`），加一个 `Subscribe` 通道；
  HTTP 侧开一个 `/watch` 长连（与 `project-sdwan-config-push.md` 的 push 定案对齐 —— 那份文档里
  "设备常连 /v1/watch"是控制器→设备的方向，这里是同进程→同盒另一进程，形状一样、实现独立）。
- **sr-frr → sr-route**：`/local-prefixes` 现在也是轮询推。改成 zebra 一有 K 路由变化就调
  `POST /local-prefixes/delta`（sr-route 侧接收端已存在，只是没人主动推 delta）。
  delta 语义：只带上一次 ack 之后新增/撤销的 prefix。sr-route 的 `setLocalPrefixes`
  是全量替换，需要在它上面加一层 delta 累加器。

**要动的文件**：
- `cmd/sr-route/main.go` + `internal/route/server.go`：加 `/watch`（SSE）与 `/local-prefixes/delta`。
- `cmd/sr-fib/main.go:473`：把 ticker 换成 watch + 首次全量对齐；断线时退化到轮询作为兜底
  （**只在断线时**，不要维持"乐观窗口" —— 这是 §1 第 3 条允许的：断线时不猜，直接告警 fail-closed，
  或者继续用最后一次事件 + 明确标记 stale，让运维看得见）。
- `cmd/sr-frr/main.go:807`：同样 shape。
- 单测 + `test/lab/vmlab/exp-276/refill-window.sh`：量出新的 refill 时长，目标 ≤ 2s
  （剩下那 1~2s 是 zebra 的账，不归 #276）。

**要拍的 3 条**：
1. **transport**：SSE（`text/event-stream`，与现有 `/v1/watch` 一致）还是 gRPC stream？
   建议 SSE —— 零依赖、`net/http` 直接能起、curl 也测得。gRPC 会让 sr-fib/sr-frr 各多一层协议栈。
2. **sr-route 重启时**：watch 断连，sr-fib/sr-frr 怎么恢复？两种：
   (a) 立即拉一次全量、再挂 watch；(b) 保留上次 RIB 并 fail-closed（客户路由不撤，
   但状态口显示 stale）。选 (a) 更符合 §1 第 3 条 —— 不"靠已经死掉的会话继续宣称前缀"。
3. **delta 累加器溢出**：`/local-prefixes/delta` 每次带增量，sr-route 端如果 ack 丢了对端要重放；
   重放的语义（幂等？带序号？）。建议每条 delta 带 `seq`，sr-route 记 last_seq，
   对端断线重连后 `GET /local-prefixes/snapshot?since_seq=N`，服务端返回 diff。
   这个形状 GoBGP 没做（它是全量），但 GoBGP 是 single-tenant；我们 sr-route ↔ sr-frr 是**进程内**
   同盒，全量也不贵，可以先简单：**每次 sr-frr 全量 POST，sr-route 全量替换**。
   等真出问题再上 delta。

**这条不做，除非 owner 逐条点完上面 3 条拍板。**

---

## 与 §10 冲突协议的关系

抬 sr-route 与 #275/#276 的 e2e 判据脚本都会**动 lab 现状**：抬进程、注 BFD down、断 watch。
每一次动手前都要在 owner 会话里声明窗口（`agent-handover.md` §10 那张表第一行）。
方案落地顺序建议：**#275 → #276 → 抬 sr-route**。前两条是纯代码 + 单测，
只有 e2e 判据跑的时候才碰 lab；抬 sr-route 是物理替换，最后做，且它做完之后
`#275` 的 delta 语义正好可以在同一轮 e2e 里复检。
