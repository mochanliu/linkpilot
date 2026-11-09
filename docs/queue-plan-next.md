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

### 落地实况（切片 A，2026-11-08）

owner 已拍：**全部照抄 FRR**、**先起 A**。切片 A 是"sr-frr → sr-route 反向导入"那半，切片 B（sr-route → sr-fib SSE `/watch`）与切片 C（e2e refill-window 判据）尚未开始。

**代码形状**（全部在 feat/vbond-control-plane，`controller/` 下）：

| 位置 | 加了什么 | FRR 对应 |
| --- | --- | --- |
| `internal/route/client.go` `LocalPrefixDelta` + `ApplyLocalPrefixDeltas` | 批量 ADD/DEL 应用到当前 local 集，闸门与 `SetLocalPrefixes` 一致：非法 op、ADD 到未发布 VRF → 整批 400 且不落；DEL 不再校验（VRF 后被撤时仍可清，否则 reporter 无法收敛到空集） | `redistribute.c::redistribute_update` / `redistribute_delete` |
| `cmd/sr-route/main.go` `PUT /local-prefixes/delta` | 新端点，只 PUT，请求体是 `{deltas: [{vrf_id, prefix, op}]}`；GET 走 405 | zebra 对每个 daemon 单独发 `ZEBRA_REDISTRIBUTE_ROUTE_ADD/_DEL`，无 GET 侧 |
| `internal/frr/import.go` `Importer.dirty chan struct{}`（size 1）+ `Dirty()` + `markDirty()` | `Handle` 在真发生变更的分支尾部（tracked ADD / tracked DEL / Reset）非阻塞发送；被 gate 拒了（invalid prefix / unknown VRF）不发 | zebra `rib_process` 之后的 work-queue 唤醒 |
| `cmd/sr-frr/main.go` `localReporter` + `importDeltaLoop` | reporter 保留 `synced/lastSeen`：首次或 delta 失败后回退全量 PUT `/local-prefixes`；其余走 delta PUT `/local-prefixes/delta`。`importDeltaLoop` 每次 `<-imp.Dirty()` 后 sleep 10ms 再算一次 diff | 首连 `bgp_zebra_connected → bgp_zebra_announce_table` 全量、稳态 `zebra_read_route → bgp_redistribute_add` 单条；合并窗口 `ZEBRA_RIB_PROCESS_HOLD_TIME 10` (zebra_router.h:185) |

**没做的**（切片 B/C 的事，或者与 owner 拍板不同）：
- SSE `/watch` 用在 sr-fib→sr-route 方向（切片 B）。sr-frr 方向沿用 HTTP PUT —— FRR 的 zapi 是长连 socket，我们只在同盒进程间用 PUT 已够，delta 语义（同 zebra 一条消息一条 ADD/DEL）比 transport 更关键。
- refill-window 判据脚本 `test/lab/vmlab/exp-276/refill-window.sh`（切片 C）；跑之前不动 lab。
- `seq`/`since_seq` 增量重放（§三 拍板 3 的"如果真出问题再上 delta"分支）：本次采用幂等 delta + 失败即全量兜底，与 bgpd 侧 zebra 死掉后重发全量同形状。

**测过的**：
- 8 条单测（`TestApplyLocalPrefixDeltas` / `TestPutLocalPrefixDeltaReplacesAndValidates` / `TestImporterDirtyCoalesces` / `TestImporterDirtyOnlyOnTrackedMutation` / `TestDiffLocalPrefixes` / `TestLocalReporterStateMachine` + 旧 2 条 `SetLocalPrefixes` 端点）。全套 `go test ./...` 全绿。
- **没有**跑 lab / VM 台物理复测；owner 若要 e2e 数字，切片 C 的判据脚本会给。

**为什么这样对齐 FRR 而不是别的**（照抄范围）：
- 每条 ADD/DEL 一条消息 = 客户侧一条路由变化 = zebra 一条 `ZEBRA_REDISTRIBUTE_ROUTE_ADD`；这是 `redistribute.c::redistribute_update` 的形状。
- 首次全量 = `bgp_zebra_announce_table`：bgpd 在 zclient 起来时对每个 afi/safi/vrf 走一次 announce，之后走 `zebra_read_route` 单条；我们 reporter 也这样。
- 10ms 合并 = `ZEBRA_RIB_PROCESS_HOLD_TIME 10`；`rib_process()` 是 zebra 的 RIB 变更 → 客户端推送这一段的工作队列；10ms 是它的 debounce。合并的是"通知发出频率"，不是"维持旧集合" —— 到期一次算的是**当时**的完整集合，不是 10ms 之前的快照，因此 AGENTS.md §1.3 的乐观窗口禁令在这里不适用。
- Delta 失败 → 下次全量：bgpd 在 zebra socket 断流时靠 `zebra_send_announce` 全量重放，我们 reporter.synced=false 走同一 fallback。
- **没有**做 seq/last_seq 增量重放：GoBGP 没这层（它是全量 + 邻居自己比对），bgpd 也不用；这条留给真出问题再加。

**四条 pin 的实际选择**（切片 A 只涉及其中两条）：
- pin A1（合并窗口时长）：10ms，与 zebra 常量同源。**没**做成"每 N 条打包"—— FRR 是按时间合并（ribq），不是按条数。
- pin A2（delta 语义）：**幂等 ADD/DEL**，接收端不记 seq。sr-frr 侧 reporter 记 lastSeen，重发同一批 delta 是安全的（`ApplyLocalPrefixDeltas` 走 set 语义）。这是 FRR 的形状：zebra 断连后 bgpd 全量重放，中间任何 delta 丢了靠下次 announce 兜住。
- pin A3（重连恢复）：由 reporter.synced 处理，见上。
- pin A4（delta 中 DEL 遇到 VRF 已消失）：**接受**。真值场景是"上一轮 ACK 了 10.10/16（当时 VRF 5 已发布）→ 后来运营方撤 VRF 5 → reporter 又观测到客户侧 10.10/16 撤了"，此时 delta 里 DEL 到未发布 VRF。sr-route 端 `ApplyLocalPrefixDeltas` 只在 ADD 上做 `localRoute` 校验，DEL 直接 delete map key。这与 FRR `redistribute_delete` 的语义一致：删除不需要重新查表存在性。

**owner 想回退怎么办**：`Importer.dirty` 那条 send 关掉，delta 端点空转；reporter 走全量 PUT。整条 reporter 只是 lastImportSig 那两行的替代，替换回来不改数据面形状。

---

### 落地实况（切片 B + C，2026-11-08）

owner 的拍板是**性能条件不是偏好**："只要路由收敛性能与 frr 的长连方式不相上下就选甲"。
甲 = SSE `/watch`、每条路由一条事件，范围"全部照抄 FRR"。这一节先给形状，再给数字，
最后给两条这次顺手抓到、必须他拍的缺口（B3/B4）。**（2026-11-09 更新：B3、B4 都已落地并实测收口 —— B4 的形状 owner 拍的是 (b)：sr-route 起来后向 sr-frr 控制口拉一份全量。）**切片 B（sr-route → sr-fib 推事件）和
切片 C（判据 + 实测时长）在同一次提交里收口；B2（三条口共用一套 transport identity）没做，理由在下面。

**代码形状**（全部在 `feat/vbond-control-plane`，`controller/` 下）：

| 位置 | 加了什么 | FRR 里的对应物 |
| --- | --- | --- |
| `internal/route/watch.go`（新文件，314 行） | `WatchHub` 一根订阅管道 + `RouteEvent{vrf,prefix,op,next_hops}` + `WatchBatch`（INIT 全量或 delta）+ `viewLocked()`（把 Loc-RIB 拍成"能装的那张视图"）+ `diffView(prev,now)`（视图变了就出一批 ADD/WITHDRAW）+ `coalesceWindow=10ms`（`:41`）+ `watchBufferDepth=1`（`:48`）+ `dropLocked`（`:288`）+ `rearmIfDirty`（`:278`，丢批后立刻重新挂表，不让脏标记睡觉） | zebra `rib_process` 之后的 work-queue：按时间合并（`ZEBRA_RIB_PROCESS_HOLD_TIME 10`，`zebra_router.h:185`），不是按条数打包 |
| `internal/route/client.go` 七个 `c.notifyWatchLocked()`：`:647 :682 :763 :1236 :1533 :1632 :1675` | 会话建立/撤销、隧道表变化、RT 导通改动、`SetLocalPrefixes`/`ApplyLocalPrefixDeltas`、RR 表更换、`SetTunnelState`（#275 那条可达性回灌）—— 每一条改 Loc-RIB 可见性的路都在这里露头 | zebra 里所有改 RIB 的入口最后都汇到 `rib_process`，没有第二条出口 |
| `cmd/sr-route/main.go` `:595` 挂 `/watch` + `serveWatch`（`:631`）+ `watchPing=5s`（`:612`） | `text/event-stream`，帧形 `event: routes\ndata: {…}\n\n`；空闲 5s 发一帧 `: ping` 让读端能判死；订阅位被占直接 **409**（`:624` 注释写了为什么不是排队） | zapi 是长连 socket，我们没有那一层，只能把"常连"落在 HTTP 流上；ping 对应 socket 的 keepalive |
| `cmd/sr-fib/watch.go`（新文件）：`watchLoop`(`:84`) / `watchOnce`(`:108`) / `nextWatchBatch`(`:152`) / `applyBatch`(`:202`) / `dropView`(`:248`) / `currentRoutes`(`:276`) / `convergeLoop`(`:296`) / `signal`(`:320`)，`watchIdleTimeout=15s`（`:49`，= 3 个 ping 周期没帧就判流死） | 起来先挂流；每收到一批就 `signal()` 触发一轮收敛；**看不认识的 op 就把视图作废**（`dropView`）改走全量 `/status`，不猜；流断 → 立即重挂，重挂后先 INIT 全量 | bgpd 的 zclient：断连后 `zebra_send_announce` 全量重放，稳态走 `zebra_read_route` 单条 |
| `cmd/sr-fib/main.go` `:505-509` | `retryGap = poll_sec` **只当作重挂间隔**；watch 在位时不再有周期性 converge。`f.signal()` 只在启动时收敛一次，之后三处触发：事件批、`PUT /config`、启动 | 这就是"去掉轮询"那一步：FRR 的收敛从来不是定时器驱动的 |

**四条不变量**（这一片的形状约束，逐条都是"照抄 FRR 的哪一件事"）：

1. **变更点只有一处出口。** 七个 `notifyWatchLocked()` 若漏挂一处，症状不是"报错"，是"那次改动没人推、
   对端只能等下一次全量对齐才发现" —— 那就是 §1 第 3 条禁的乐观窗口（靠一份已经过期的视图继续宣称可达）。
   所以单测里专门有一条反证：`TestWatchNoBatchForInvisibleChange`（视图没变就不许发空批）与
   `TestWatchLosingOneRRIsSilent`（双 RR 掉一个、最优路径不变 ⇒ 不发事件，这条是**故意**的静默，
   因为它不改变可装视图，发了反而是抖动）。
2. **合并按时间、不按条数**：10ms 窗口到期时算的是**当时**的完整视图差，不是 10ms 之前攒下的那批。
   这条和切片 A 用同一个常数，同一个理由。
3. **背压宁丢不藏**：`watchBufferDepth=1`，读端跟不上就 `dropLocked` 断它的流。排队 = 在缓冲区里
   存一串已经过期的路由事实，读出来时它比 RIB 老；FRR 同样不在 socket 里替客户端攒路由。
   断掉之后读端自己重挂 + INIT 全量，账对得回来。
4. **订阅位独占、冲突当场可见**：第二个消费者拿到 409，不是排队。现场证据 —— CPE1 上
   `stamp-watch.sh` 去占槽时，sr-fib 的日志立刻出现
   `watch: sr-route 的订阅位被占用（已有另一个消费者）`，并自动退回全量 `/status` 收敛，客户流量 0 抖。

**切片 C 的判据形状**（细节与踩坑过程在 `test/lab/vmlab/exp-276/README.md`，这里只留结论性的口径）：

一条远端前缀从"控制器知道"到"客户包走出去"要过三段，量具必须一段一段读：

- `in` —— 前缀出现在 sr-route `/status` 的任何位置（rib 段或 pending 段）。
- `ok` —— 它在**可装视图**里（rib 条目带 `"targets"`/next_hops），也就是 `viewLocked`/`AllRIB` 给 sr-fib 的那一份。
- `ours` —— `vppctl show ip fib <pfx>` 里 `mpls via` 的条数 = 我们的隧道路径确实在转发链上。

`in→ok` 是 sr-route 的可达性门（#262/#275 的账），`ok→ours` 才是切片 B 的账。第一版量具把这两段读成一段
（pending 条目也含 `"prefix"`），于是算出 873ms/887ms 这种"改了等于没改"的数并记到了切片 B 头上，翻了一次车 —— 记在 README §一，
别把它当"测量建议"。**`ours=0` 不等于这条前缀不在 FIB 里**：linux-cp 会把内核主表的路由镜像进 VPP VRF0
（`lcp-rt`，pref 20），对远端前缀那份镜像本身就是黑洞；本轮 68~125 发出现 `line=Y && ours=0`。

**实测：改前（轮询）vs 改后（事件）** —— 同一台 CPE1、同一前缀、同一注入（`systemctl restart frr`）、
两臂的 sr-route 是**同一份**产物 `26044f78f7969935`，唯一变量是"谁触发 sr-fib 收敛"：

| 边 | 改前 `bab523ff`（poll_sec=1，76ms 分辨率） | 改后 `a9f1bc8c`（事件，60~61ms 分辨率） |
| --- | --- | --- |
| 摘路边（可达 → 我们的路径下架） | ≤456ms / ≤593ms | **≤40ms / ≤41ms / ≤45ms**（另一臂 102ms 分辨率下 ≤79ms） |
| 装路边（可装 → 我们的路径上架） | 868ms / 978ms | **201ms / 294ms / 755ms**（102ms 分辨率臂 ≤272ms） |
| `in→ok` 那道可达性门 | 未观察到 1→2 单独成段 | 同样一次都没观察到 —— 它不归切片 B，归 #262/#275 |
| 整条前缀不可用的总时长 | 5.33~6.66s | 5.33~6.66s（**两臂同量**：那半是 zebra 收录 + 客户侧收敛的账，切片 B 压不动它） |

**发射延迟是直接量的，不是推出来的**：用 `stamp-watch.sh` 占住订阅位，逐毫秒打戳每一帧 ——
摘路边事件比"外部第一次观测到变化"晚 59ms（≤142ms 内发出），装路边晚 8ms（≤84ms）。
所以残余不在 sr-route 的发射端，在下游：sr-fib 一轮收敛 + VPP 装表。这一段本轮拆不干净
（`fib.log`/`bridge.log` 只有秒级时间戳），README §五 给了怎么拆（给 converge 加毫秒戳 +
在 ours=0 窗口里打一份完整 `show ip fib`），登记为残余，不写成结论。

**测过的**：
- 单测 25 条新增：`internal/route/watch_test.go` 12（INIT 全量 / announce+withdraw / 下一跳集合变化算一条事件 /
  RT 改动算一条事件 / 不可见改动不发批 / 隧道 down 撤、up 重装 / 丢一个 RR 静默 / 突发合并 /
  背压丢读端 / 订阅位独占 / 关停收尾 / `viewLocked` 与 `AllRIB` 逐条相等）、
  `cmd/sr-route/watch_test.go` 3（真编辑能走完整条流 / 槽互斥 / 客户端断开收尾）、
  `cmd/sr-fib/watch_test.go` 10（URL 换算 / 一帧只读一帧 / 不认识的帧拒收 / applyBatch 给收敛器一张可信视图 /
  没快照时只请求全量 / 未知 op 作废视图 / 视图可证时用它、不可证时回全量 / 断流回退全量 RIB /
  watchLoop 真装表 / 槽被占仍从全量 RIB 收敛）。`go test ./...` 全绿，`vm-verify.sh` **392 PASS / 0 FAIL / 0 WARN**，
  `exp-273/six.sh` 六方向全 **0% 丢包**（2026-11-08 实测）。
- lab 双臂 A/B 各若干枪，逐枪的客户可见时长写在 README §六 的中断表里；本轮新增两枪
  `swap276.sh CPE1 sr-fib`（同版本换件，脚本自检）：新 pid 1831ms / 1804ms 起来，
  **0/2561 与 0/2544 发 FAIL**。

**两条新缺口的现状（2026-11-09 更新：B3、B4 均已落地并实测收口，抬 sr-route 只剩"与 sr-frr 同批"这条部署规则）**：

- **B3 —— watch 模式下，隧道/BFD 状态变化没有任何东西会触发收敛。已修（2026-11-09 落地 + 三臂实测）。**
  场景：隧道起来又塌下去，但这一段时间 RIB 没变（RR 还在发那条前缀）。
  代码走到哪：`path.resolvable` 只由隧道表决定（`internal/route/client.go:1142` 明写
  "It is NOT the source of path.resolvable: that flag is the tunnel gate alone"、`:1169`
  "This is the path.resolvable is set from" 只有 `SetTunnelState`），BFD 的真实状态是
  **收敛轮内部**才读的（`cmd/sr-fib/main.go:301` → `fib.UnreachablePeers` → `internal/fib/fib.go:306` 该路径跳过）。
  watch 在位时收敛只有三个触发点（启动 `main.go:509`、事件批 `watch.go:215/233/253`、`PUT /config`），
  `retryGap` 只是重挂间隔 ⇒ 这三个都不发生的那次抖动，**没人去重算**。
  症状（现场证据）：poll2 那一枪里 ours 1→0 之后，是下一轮 1s 轮询把它救回来的；换成 watch 就没有"下一轮"。
  为什么这条要紧：#262 的 hold-down 也要挂在同一个触发点上，挂不上就是"抑制窗口把已塌的隧道继续宣称可达"，
  正撞 §1 第 3 条。
  **落地形状（就是建议的那一条，没绕）**：订阅 VPP 的 BFD 会话状态事件，状态翻转就 `signal()`。
  - `internal/vppapi/events.go`（新）：`WatchBFDEvents(ctx, fn)` 发 `want_bfd_events`
    （body 是 `[1 个 pad + client_index=1 + pid]`，`bfdEventLen=67`），把 `bfd_udp_session_event`
    解成 `BFDEvent{SwIfIndex, LocalIP, PeerIP, State}`。**注册异步 handler 必须排在发请求之前** ——
    事件帧和应答几乎同时回来，先发后注册会丢掉头一条。
  - `internal/vppapi/vppapi.go`：`Client.asyncs map[uint16]func([]byte)`，`readLoop` 按 msg_id 派发。
    查表在 `c.mu` 里做，**回调在解锁之后调**（回调里下一步往往就是发一条请求去读真值，锁内回调就是死锁），
    并且传的是帧的副本。
  - `internal/vppapi/messages.go`：`requiredVersions` 增加 `want_bfd_events c5e2af94` /
    `want_bfd_events_reply e8d4e804` / `bfd_udp_session_event 8eaaf062`（三条 CRC 2026-11-08 从 CPE3 现网刮下来）。
  - `cmd/sr-fib/main.go:461` 先造 `wake`（容量 1 的 chan），注册做在 **dial 闭包内部**：
    VPP 侧的订阅是跟着连接一起没的，每次重连都得重新 `want_bfd_events`，放在外面就只剩第一次连接有事件。
    **订阅失败 = 这次连接失败**（`c.Close()` + 包装错误）：宁可起不来，也不要一个"看着连上了、
    但可达性变化没人叫醒收敛"的半死状态 —— 那正是修之前的形状。
  - 事件只是**叫醒**，真值仍然从 `bfd_udp_session_dump` 读（level-triggered）：这条决定了它不违反 §1 第 3 条，
    也不会有"事件丢了就一直以为 up"的乐观窗口。
  - 新增单测 5 条：投递 / **回调在锁外**（否则死锁）/ 短帧计数不投 / `retval` 透出 / dial 拒结构漂移。
  **实测（CPE3 被测，故障注入在**对端** CPE1 的 `wg1009`；台账 `test/lab/vmlab/exp-b3/README.md`）**：

  | 臂 | sr-fib | 收敛触发 | 摘路 | 装回 | 死腿在 FIB 里挂多久 | 客户侧探测 |
  | --- | --- | --- | --- | --- | --- | --- |
  | 轮询基线 | `bab523ff3809f57b` | 每秒一轮 | **745ms** | **492ms** | 0.82s / 20.30s | 1588 发 FAIL 1（0.1%） |
  | 缺陷臂（事件版无 B3） | `a9f1bc8c8fd49016` + sr-route `26044f78` | 只被路由事件叫醒 | **无期限** | 不适用 | **21s 全程没摘**（sr-fib 一行日志都没打） | 871 发 FAIL 20，**连续 20 发覆盖 19.92s** |
  | B3 | `c213a6caf1b2aabf` | 路由事件 **+ BFD 事件** | **≤65ms** | **≤65ms** | 0（Down 态 290 发 ours 恒 1） | 1557 发 FAIL 1（0.1%），无连续断流 |

  三个口径要点：① 65ms 是**量具的分辨率**（这一轮 fork 数实测 61~65ms），小于它的跳变都打成 "0ms"，
  只能读成"≤ 一个采样间隔"；② 缺陷臂不是"丢一半"，VPP 按流哈希，那条探测流是**整条 100% 黑洞且不自愈**；
  ③ 注入必须在对端盒，在本测盒注入量到的是 VPP 的接口级反应，不是 sr-fib 的决策。
  结论一句话：**摘路这一段从"1s 封顶"变成"65ms 以下"，而中间那一版是"没有期限"。**
  换件本身的中断也量了（§1 第 4 条要求点名）：`swap276.sh CPE3 sr-fib` 新 pid 1639ms 起来，
  全程 2790 发 **0 FAIL**。
  仍然留下的：`#262` 的 hold-down 现在有了触发点（挂在这条 `signal()` 上），但 hold-down 本身还没做。
  **抬 sr-route/sr-fib 到其余盒的这一条前置已经解除**；换完 sr-fib 必须立刻补一次隧道表
  （G-2：`test/lab/vmlab/vm-fib-sync.sh <node>`，判据写在 `docs/agent-handover.md` §5.2）。

- **B4 —— 对端（sr-route）重启后，本地前缀集没人重建。已修（2026-11-09 落地 + 三臂实测）。**
  场景：抬 sr-route 那一枪之后，`/status` 的 `local` 段是空的且不会自己回来 ——
  本盒客户侧前缀对全网消失，直到下一次客户侧路由变动。
  代码走到哪：sr-frr 的上报是**变更驱动**的（`cmd/sr-frr/main.go:745-776`：`reporter.reconcile` 只在
  `posted` 时打日志、只有 `imp.Dirty()` 或被唤醒才重算），sr-route 起来后不主动向 sr-frr 要。
  这条不是"晚一拍"，是**根本没有请求**：`reconcile` 算的是 `diffLocalPrefixes(lastSeen, next)`
  （`cmd/sr-frr/main.go:1006`，差量在 `:1014`/`:1041`），集合没变 ⇒ 差量 0 条 ⇒ `posted=false`
  ⇒ 一次 HTTP 都不发。本轮就踩过：我重启 CPE1 的 sr-route 之后 vm-verify 出 5 条 FAIL，根因是这条，不是切片 B。
  **修法（owner 拍的是形状 (b)，已按此落地）**：sr-route 起来后主动 GET sr-frr 控制口的 `/local-prefixes`
  要一份全量 —— 形状就是 zebra 的"新客户端一连上，daemon 立刻 announce"，只是这里把"连上"做成了
  sr-route 侧的一次拉取。新文件 `cmd/sr-route/pull.go`：`pullLocalPrefixes`（`:61`）在进程起来时
  （`cmd/sr-route/main.go:182` 的 `go pullLocalPrefixes`）拉，拿到就经 `route.Client.SetLocalPrefixes`
  （`internal/route/client.go:547`，整表原子、要么全要么不动）应用一次然后**退出**，不再有周期重发
  （AGENTS.md §1 第 3 条：不许用乐观窗口，也不许靠周期对账兜底）；拉不到就每 2s 重试
  （`pullRetryEvery` `pull.go:52`）直到第一次可信回答 —— 开机时 `do_route`(:354) 在 `do_bridge`(:383) 之前，
  socket 那时候本来就不在，重试路径是必须的。
  sr-frr 侧新增 `GET /local-prefixes`（`cmd/sr-frr/main.go:1192`），两条关键设计：
  (1) **可信空集闸门**（`:1198`）：`rules` 非空但 zebra 没连上时回 503（"我还没看到客户侧"），
  `rules` 为空时照常回 200 + 空前缀 —— 否则没配泄露的盒会因为 `ensureImport` 不建客户端而永远 503。
  (2) **serve 即降级**（`localReporter.demote()` `:996`）：被拉走一次就把 `lastSeen` 置空，
  下一轮 reconcile（≤`poll_sec`，本机 1s）必然打一次整表 PUT，把"拉取与推送谁先到"这个次序问题关掉。
  socket 路径三级台阶（`srfrrControlSocket` `pull.go:149`）：显式 `-srfrr-sock` > `/etc/sr-frr.json`
  里的 `control_socket` > `/run/sr-frr/control.sock`；解析用 `DisallowUnknownFields`（`prefix` 写成
  `prefixes` 这种拼错会被当成"空集"从而把前缀全撤了，必须硬拒）。
  **三臂实测**（CPE3，注入=只重启 sr-route，探测 cust12→172.16.3.1，判据脚本 `test/lab/vmlab/exp-b4/`）：
  老 pair 那一臂 `local` 在 t+1s 掉到 0、**150s 后仍是 0（未自愈）**，客户探测连续断流 **158.66s**；
  新 pair 这一臂 `local` **t+1s 就是 1**、1899 发探测 **0 FAIL**，sr-route 新 pid 起来用时 1630ms
  （和老臂 1635ms 同量，说明拉取没有给重启加时长）。日志因果：`开机从 sr-frr 拉到客户侧前缀 1 条
  （/run/sr-frr/control.sock），之后的变化由它推`。`vm-verify.sh` 换件后整轮 **392/0/0**，
  `exp-273/six.sh` 六方向 0% 丢包。逐枪数字与诚实口径（A 臂绿回来的那段属于 #272，不算 B4 的功劳）
  在 `test/lab/vmlab/exp-b4/README.md`。

  **实测出来的部署硬规则（改写了下面"没做的"那条）：`sr-frr` 与 `sr-route` 必须同批抬，且 `sr-frr` 在前。**
  老 sr-frr 没有这个端点，实测回 `404 page not found`；只抬 sr-route 就是让它对着 404 每 2s 重试到永远，
  空洞照旧。反过来只抬 sr-frr 是安全的。

**没做的**：
- **B2 transport identity** —— `/local-prefixes`、`/local-prefixes/delta`、`/watch` 三条口现在各自判身份，
  要收口成一套（同一条 mTLS/证书身份贯穿），这属于协议层，不该塞进切片 B。
- **抬盒已经完成（#284，2026-11-09 实测）**，不再是待办：CPE2/CPE3/CPE4 三台是"事件 + B3 + B4"
  （`sr-route 1161534d`、`sr-fib c213a6ca`、`sr-frr 626b126a`），CPE1 抬了 `sr-route`/`sr-frr` 两件、
  **`sr-fib` 按方案留在 `a9f1bc8c`**（事件版无 B3，手上唯一的缺陷臂，抬上去就没有对照了）。
  十二件 run==盘、`vm-verify.sh` 392 PASS/0 FAIL、`exp-273/six.sh` 六方向 0% 丢包。
  逐枪数字、流量归属表和回滚底在 `test/lab/vmlab/exp-284/README.md`；现场台账在 `agent-handover.md` §3。
  **抬盒踩出来的一条**：`push276.sh` 只落到 staging（`$D/<name>.new`），不装盘 —— 直接跑 `swap276.sh`
  等于拿老份做重启自检（脚本自己会喊"跑 == 盘"）。顺序只有 **push → install → swap** 一步不能省。
- `seq`/`since_seq` 增量重放：与切片 A 同一个理由 —— 幂等批 + 断线全量兜底已经覆盖了丢失，FRR 也没有这层。
- zebra 收录 K 路由那 1~2s：FRR 侧的账，#276 不压它（实测两臂 5.33~6.66s 同量，见上表）。

**owner 想回退怎么办**：`cmd/sr-fib/main.go:508` 那一行 `go f.watchLoop(...)` 去掉、把 `convergeLoop` 换回
`retryGap` 周期的 ticker，就退回轮询版；sr-route 侧 `/watch` 端点没人订阅时 `notifyWatchLocked` 是 no-op，
不产生任何副作用，可以留着。盒上的回滚底（2026-11-09 从 `/proc/<pid>/exe` 抽的，哈希对过在跑的那一份）：
CPE1 `/usr/local/bin/sr-fib.prerun-20261108` = `bab523ff3809f57b`（轮询那份，含缺陷⑦ 修复）、
`sr-frr.prerun-20261109` = `c8e731dd`、`sr-route.prerun-20261109` = `26044f78`；
CPE4 三件同名 `.prerun-20261109` = `c8e731dd` / `5789c2c0` / `bab523ff`。
换回去只需要 `install276.sh` + `swap276.sh` 两枪，全程 0 丢包。
B4 侧的回退同理是**换件**：新 sr-frr 的 `GET /local-prefixes` 没人订阅就是闲置的（`serve` 里多挂一个分支，
`demote()` 只在真被拉走时才降级），所以把 `sr-route` 单独换回旧版 = 回到"没人重建"的旧行为，不留副作用；
但**反过来只回退 sr-frr、留着新 sr-route 不行** —— 那正是上面那条 404 无限重试的形状，回退必须同批。
（抬盒时在 CPE4 又实测一遍：老 sr-frr 对这个端点回 `404 page not found`，换成新的之后回 `200` 并给出
`{"prefixes":[{"vrf_id":0,"prefix":"172.16.4.1/32"}]}`，同一条命令就是"前置到没到位"的现场判据。）

## 与 §10 冲突协议的关系

抬 sr-route 与 #275/#276 的 e2e 判据脚本都会**动 lab 现状**：抬进程、注 BFD down、断 watch。
**（2026-11-09 更新）owner 已撤掉"动手前先声明窗口"这条**：原话「lab 是你研发测试用的，不存在窗口概念，
你随时想动都可以动，除非我明确说不动。」这套 VirtualBox 台可以直接动；仍然要做的是把改动走产品路径、
在实验 README 里写清「改了什么 / 怎么还原」，以及有第二个 agent 在场时别打脏对方正在量的基线。
生产盒与别人现场的 lab 不在豁免里。
方案落地顺序建议：**#275 → #276 → 抬 sr-route**。前两条是纯代码 + 单测，
只有 e2e 判据跑的时候才碰 lab；抬 sr-route 是物理替换，最后做，且它做完之后
`#275` 的 delta 语义正好可以在同一轮 e2e 里复检。
（这三件都已完成：#275/#276 见上面两节落地实况，抬盒 = #284。）

---

## 四、#285 主/备切换到底是谁干的（**实测，已完成，2026-11-09**）

owner 点名要复核的说法是：「BFD down 会 set wireguard tunnel down，路由自动变为失效状态，
并且次优路由自动变成当前主路由。」两臂实测（`test/lab/vmlab/exp-285/README.md`，
脚本 `failover.sh`，原始读数在 `/tmp/p285/`）结论分三句：

1. **前半句是假的，而且从来没实现过。** 全仓唯一能发 `sw_interface_set_flags` 的是
   `internal/vppapi/interface.go:41` 的 `SetInterfaceUp`，唯一生产调用者是
   `internal/agent/dataplane.go:814`，**恒传 `up=true`**；那个 `up bool` 参数从没被传过 `false`。
   BFD down 不会去动隧道口。
2. **后半句是真的、客户可见、而且快**，但**成因有两条，取决于故障长什么样**：
   - **链①（本机口/邻接死）**：VPP 自己把该路径标 `oper-flags:` 非 resolved、踢出 uPRF，
     按 `frp_preference` 挪到次优。local 臂实测：切换那一发 **BFD 还报 Up、path-list 还是 2 条**，
     比 BFD 判 down 早 **234ms**；sr-fib 的重写 **108ms** 后才到。客户探测 **1453 发 0 FAIL**。
   - **链②（只有对端死，本机口全程 up）**：VPP 什么都不做，是 sr-fib 收到 BFD 事件后
     把那条路径**从行里删掉**、整行 `ip_route_add_del(is_multipath=0)` 覆盖。peer 臂实测：
     全程没有「承载已挪、行还是两条」那一发，`len` 2→1 与承载挪动同一发；客户探测 **1401 发 FAIL 2（0.1%，两发孤立）**。
     因为路径是被**删掉**的，**BFD-only 这一形状里 VPP 的原生 preference 回落根本没机会触发**。
3. **以前为什么会说成那样**：`docs/overlay-design-draft.md` §2.1 那张表第三行量的是
   「把 pref 0 那条的**接口** `set interface state down`」= 链①，结论没错；
   但 `internal/fib/fib.go:314-315` 的注释把它概括成「down 顶上、up 回落都是原生行为」写在逃生开关旁边，
   读起来就像 BFD down 也走原生回落。**这条注释要改**（改成"口/邻接级失效由 VPP 原生回落；
   BFD 判 down 走的是本文件 §4.3 第 3 步的删路径 + 整行重写"），本轮先记在这里，不动产品代码。

还有第三条更慢的「失效」，两臂都没走到：sr-route Loc-RIB 的 `path.resolvable`
（`internal/route/rib.go:33`，GoBGP `Path.IsNexthopInvalid` 的取反同位物），只由 agent 每 **10s** 一次的
`PUT /tunnel-state` 翻（`cmd/sr-agent/main.go:421/448` → `internal/agent/routestate.go:123`）。
sr-fib 直接读本机 VPP 的 BFD，不等这 10s。

**这一枪把 lab 改成了主/备形状，现在的现场不等于 #284 提交时的基线**：
t13（CPE2↔CPE3）颜色 **0→2**（控制台改色 → 发布 v23 → agent 已 apply），四台逃生开关 **全 true**
（`ESCAPE=true vm-fib-sync.sh`，本轮给脚本新加的旋钮）。还原两步、都不重启进程，配方在 exp-285 README §五。
**G-2 仍未收口**：CPE3 `/etc/sdwan/fib.json` 的 `tunnels` 实测仍是 0 条，sr-fib 一重启这张表就空，
必须重跑 `vm-fib-sync.sh`；逃生开关同样没有产品下发路径。

**顺手修掉的一处判据缺陷（不是产品缺陷）**：`vm-verify.sh` §6 旧口径要求「RIB 里每个下一跳都要有
`mpls via`」，那只在所有腿同 pref（ECMP）时成立 —— `mpls via` 只出现在承载链里，而承载链只装最优 preference。
颜色一分开，备路**装在 path-list 里且 resolved、但不承载**，旧口径把正确的主备形状判红（实测 FAIL 1 条）。
新口径两条、比旧的严：① 每个 RIB 下一跳都要有 **resolved 的路径装在行里**（新增 `fib_paths()` 单前缀查询，
因为整表 dump 没有 `path:` 行）；② **承载集必须恰好等于最优 pref 的那组路径**（同 pref 并列 = ECMP，全都要承载）。

新口径在三种形状上都真跑过，不是只在纸面上成立：CPE2 单条颜色 2 的腿（`pref=12`，它承载）、
CPE3 主备（`pref=10` 承载 / `pref=12` 装着不承载）、CPE4 两条同 `pref=10`（ECMP，两条都承载）。
定稿后整台复量 **`vm-verify.sh` PASS=398 FAIL=0 WARN=0**（旧口径 392，差额 +6 的逐条账在
`docs/agent-handover.md` §2.4 —— **不是产品变好了 6 条，是口径换了**：§6 新增 8 条、原来那条假 FAIL 转 PASS 1 条、
§3 两条颜色 0 的 ok 降为 note 减 2 条）。`exp-273/six.sh` 六方向 **0% 丢包**。

`ESCAPE=false` 这个值第一版是假的（`!= "keep"` 就置 `True`），会让 README 的还原配方还原不掉，
而且脚本回读自己刚推的值所以照样报绿。已改成显式映射 + 入口拒收非法值，并在 CPE3 上做了往返取证：
`false` → 主表 `len:1` 只剩 `pref=10`；`true` → `len:2`、`routes_set=1`。逐字读数在 exp-285 README §七 第 9 条。
