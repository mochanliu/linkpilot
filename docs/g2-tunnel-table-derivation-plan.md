# 缺口 G-2 方案：sr-fib 的隧道表由 sr-agent 派生并推送

状态：**方案，未动代码**。owner 已拍四条（下面第 0 节逐字引用），其余三个岔路口在第 6 节等他拍。
这份方案的存在本身记在 `docs/vmlab-rebuild-plan.md` §9.6（缺口首次量化）与 §9.18 九（#270 收口后剩下的开机洞）。

## 0. owner 拍板（原话）

> 1. a
> 2. 听你的
> 3. 所有都应该是动态的，你不能确定谁先启动这只是一，其次用户可能随时增减隧道，overlay 路由要跟着响应。
> 4. lab 就是你测试当前代码的模拟生产环境，上边不应该有任何非生产的东西，就是要通过 lab 测出所有问题。

四条落到这件事上就是：**(1) 由 sr-agent 派生这张表并推给 sr-fib；(2) 派生结果不落盘（我的决定，理由见 4.6）；
(3) 触发只能是动态的（开机、隧道增删、VPP 重启检测），不许有"先推一次试试"的静态兜底；
(4) 现在 lab 里兜这一段的手工脚本 `test/lab/vmlab/vm-fib-sync.sh` 要退役，改完必须由 lab 自己证明。**

## 1. 场景：一台盒子开机之后，一步一步走到 overlay 不通

开机 90 秒左右 `sr-box-watch.timer` 第一次触发，`/usr/local/lib/sr-box/start.sh` 把四个 sr 进程拉起来，
其中和这件事有关的两个是 sr-agent（带 `-apply -tunnels`）和 sr-fib。

1. agent 拿到控制台发布的隧道段，在 VPP 上把隧道口建出来，本端 overlay /32 绑在口上。这一步是真的，
   `boot-check.sh` 读回 `vppctl show interface addr` 能看到 wg 口和 /32。
2. sr-fib 起来时读 `/etc/sdwan/fib.json`。CPE1 上现在这份的内容是
   `{"status_url":".../status","status_addr":"127.0.0.1:19011","vrfs":[0],"tunnels":[]}` ——
   **tunnels 是空的**，这是我这次写方案时在那台盒子上直接读到的，不是推断。
   这个文件由 `test/lab/vmlab/vm-bringup.sh:349` 起那段 heredoc 生成，`"tunnels": []` 是写死的（357 行）。
3. sr-fib 每 `poll_sec`（1 秒）收敛一次：先 GET sr-route 的 `/status` 拿 RIB，那里有客户前缀和它的下一跳
   （`controller/cmd/sr-fib/main.go:357` `fetchStatus`），再调 `fib.PlanFrom(vrfs, routes, tunnels, down, escape)`
   把三者拼成一份要下发 VPP 的计划。
4. `PlanFrom` 拿着空的隧道表去解析每一条路径，命中 `controller/internal/fib/fib.go:302` 那一行：
   给这条前缀留一条 warning「下一跳 10.250.0.2 不在隧道表里，该路径跳过」，然后把这条路径丢掉。
   每一条都丢掉，所以计划里一条路由都没有。
5. 于是 VPP 的 FIB 里一条 overlay 路由也没装。控制台看到的是：隧道 Up、BFD Up、RR 在反射路由、
   sr-route 的 RIB 有前缀 —— 全部正常，但客户站点之间 ping 不通。

这就是 §9.6 量的那个缺口，也是 CPE4 真断电重起后 `boot-check.sh` 报 FAIL 的那一条
（原始读回 `test/lab/vmlab/exp-270/raw/boot-CPE4-first-readback.log:34`：`sr-fib 隧道表=0`）。

## 2. 为什么产品里会缺这一段（不是谁偷懒漏了）

配置包一共六段。第 3 段是隧道，agent 按它在 VPP 上建口；第 4 段是 overlay 的路由层，
`sr-apply` 把它的三个键（`tunnels` / `vrfs` / `rrs`）叠进盒子的 `route.json` 并热更给 **sr-route**
（`controller/internal/apply/route.go:100` 起那段合并逻辑，未知键直接丢掉并打日志）。

第 4 段里的 `tunnels` 每一行是 `{peer_device_id, next_hop}`
（`controller/cmd/sr-ctrl/overlay.go:701` 的 `overlayRouteTunnel`）。这个形状是给 sr-route 用的：
它只需要知道"这个下一跳属于哪台设备"，好把 RR 学到的前缀和隧道对上。它不需要接口名，也不需要颜色。

sr-fib 要的是另一张表：`{next_hop, interface, color}`（`controller/cmd/sr-fib/main.go:60` 的 `tunnelConfig`）。
颜色决定这条路径装进 VPP 时的 preference（`internal/fib/fib.go:316`：主表只收颜色 0，或者逃生开关打开时全收），
接口名决定下一跳走哪张口。**这三个键分属三个地方：`next_hop` 和 `color` 只有控制器知道，
`interface` 只有这台盒子自己知道**（VPP 重启一次，口的 sw_if_index 和名字都会变，见
`internal/fib/fib.go:545` 每次收敛都重新取接口表的那个理由）。没有任何一份现存的下发内容同时装着这三个键，
所以产品里确实没有一个进程能拼出这张表。

D3（任务 #197）只做了"开一个热更入口"这一件事：`PUT /config` 能整份替换这张表
（`controller/cmd/sr-fib/main.go:543` 起）。谁调它，没有定义。我把整个 Go 树 grep 了一遍 `19011`，
调用者全是 lab 与实验脚本（`test/lab/vmlab/vm-fib-sync.sh`、`exp-268/roll-268.sh`、
`exp-270/boot-check.sh`、`exp-270/deploy-main-core.sh`），没有一行 Go 代码往里写过东西。

于是 lab 用 `vm-fib-sync.sh` 手工填这一段。它的头注释把缺口的形状写全了，因此这份脚本正好可以当派生器的规格书用。

## 3. 派生一张表需要的四个输入，今天各自从哪来

| 这一行要填的 | 语义 | 今天 lab 脚本从哪拿 | 产品里应该由谁拿 | 现状 |
|---|---|---|---|---|
| `next_hop` | 对端的 overlay /32 | 控制台 `GET /api/underlay` | agent：隧道段每条 `TunnelSpec.PeerOverlay`（`internal/wire/wire.go:540`） | 已经有 |
| `interface` | 本端隧道口在 VPP 里的名字 | 盒子里 `vppctl show interface addr` | agent：`DataPlane.DumpInterfaces`（`internal/agent/dataplane.go:76`）按 `Link.SWIfIndex()`（同文件 858 行）查名字 | 已经有，只是没拼起来 |
| `color` | 这条隧道的颜色 | 控制台 `GET /api/underlay` | agent：隧道段 | **不在契约里**，见 4.1 |
| `vrfs` | 这台盒子服务哪些 VPN | 盒子里 `/etc/sdwan/route.json` 第 4 段 | agent：读 `sr-apply` 已经落盘的那份（`cmd/sr-agent/main.go:255` 的 `applier.RouteConf`） | 已经有 |
| `escape_all_colors` | 租户级逃生开关 | 保持 sr-fib 当前值 | 见第 6 节待拍 A | 产品里没有下发路径 |

`color` 这一条要特别说明白：控制器侧的颜色是有的，`internal/underlay/state.go:145` 的 `Tunnel.Color`（上限 7，同文件 45 行），
建隧道与批量建隧道的入口都校验它。但是渲染给设备的时候没有带上——`cmd/sr-ctrl/underlay_delivery.go:134`
构造 `wire.TunnelSpec` 时逐字段抄了一遍，`Color` 不在里面，`internal/wire/wire.go` 整个文件搜不到 color。
所以设备今天**根本不知道自己每条隧道的颜色**。这是 G-2 唯一的契约改动，也是必须先拍的一条。

## 4. 要动什么

### 4.1 契约：隧道段每条带上颜色

`wire.TunnelSpec` 加一个 `Color uint8`，渲染点在 `cmd/sr-ctrl/underlay_delivery.go:134`，那里 `t.Color` 就在手边。
渲染时不做默认值兜底：`underlay.Tunnel.Color` 是什么就发什么，颜色 0 是合法颜色而不是"没填"。

不动第 4 段的 `route.json`：那张表归 sr-route，给它加一个它不读的字段，等于让两个进程共享一份没人负责的键。

老版本控制器不发 color 时设备会读到 0，后果不是报错而是主备消失（所有路径同优先级）。这条在第 6 节待拍 B 里拍。

### 4.2 派生器：agent 侧新增一个组件，不新增进程

放在 `internal/agent/`，和 `TunnelSync` 同一层（`internal/agent/tunnel.go:45` 那个结构里，
`links` / `peerOf` / `localOf` 在 67 到 70 行，也就是"哪条隧道、本端 /32、对端是谁"都在它手上）。
新组件做三件事：把 `TunnelSync` 的活链路摊成 `{next_hop, interface, color}` 若干行，
再从盘上的 `route.json` 拿 `vrfs`、从 sr-fib 拿 `escape_all_colors`，拼成一份 `configDoc` 推上去。

不新建 daemon、不改 `start.sh` 清单、不重启任何进程（AGENTS 规则 1）。sr-agent 这一个进程里
今天已经同时跑着隧道对账和配置落盘两条线（`cmd/sr-agent/main.go:216` 与 `246`），这张表要的输入正好全在这一个进程里，
这是它归 agent 而不是归 sr-apply 或 sr-fib 的实际理由，也和拍板 1 一致。

### 4.3 接口名只认读回，不认算法

派生器不许拼口名。今天 lab 里看到的名字是 `wg1002`、`wg1004`、`wg1006`
（§9.7 那张前后对比表，正好等于 `user_instance`），但这是 VPP 自己的命名结果，不是一条可以依赖的规则：
容器台就记过「wg 接口名按创建顺序自动生成」这一类观察，而 VPP 重启一次 sw_if_index 全部会变
（`internal/fib/fib.go:545` 每次收敛都重新取接口表，就是这个不可靠性的产品内承认）。

所以派生器的认法和 agent 接管时的认法必须同一个口径：agent 建口时传的是
`UserInstance = l.cfg.Instance`（`internal/agent/dataplane.go:446`，接管时按同一个号比对，同文件 517 行），
`Link` 建好之后手上也有 `SWIfIndex()`（同文件 858 行）。派生器拿这个 index，
在 `DumpInterfaces` 的返回里（同文件 76 行）找那一行的 `Name` —— 名字是问 VPP 要来的。

找不到（口已被删、VPP 刚重启还没重建）就把这条隧道**排除在这一轮之外并说明原因**，
而不是推一条接口名为空的行——`internal/fib/fib.go:250` 会因为"隧道没有接口名"把整份配置拒掉。
整份拒绝是对的（读错一个字段的代价不是报错，而是某个 VPN 静默不转发，见 `controller/cmd/sr-fib/main.go:120`
那段注释），但那是 sr-fib 的拒绝，不是派生器可以拿来当降级用。

同一对端多条隧道（M-3 之后）不会撞：每条隧道有自己的 overlay /32 对，`next_hop` 不同，
`internal/fib/fib.go:256` 那条"同一对端只能有一条隧道映射到同一个 overlay 地址"的重复检查照旧过。

### 4.4 `vrfs` 读盘上那一份，读不到就不推

`route.json` 的第 4 段是控制器发布的、`sr-apply` 已经落盘的，agent 只是读，不猜。
如果这台盒子还没收到过 overlay 配置（`vrfs` 空），派生器这一轮**什么都不推**并把原因报出来：
`sr-fib` 的 `validateLive`（`controller/cmd/sr-fib/main.go:124`）会拒绝空的 VPN 集合，
因为"一个 VPN 都不服务"和"还没配"是两件事，前者是租户的意图，后者不是。

### 4.5 `escape_all_colors` 走读改写，并且把多写者的风险说清楚

`PUT /config` 是**整份替换**（`controller/cmd/sr-fib/main.go:129` 的注释逐字写了"省略 tunnels 等于空隧道表"）。
所以派生器不能只推自己那一段：必须先 `GET /config`（同文件 545 行，返回的就是同一份 `configDoc`），
把自己算出的 `tunnels`（和 `vrfs`）换进去，`escape_all_colors` 原样带回去。
今天 lab 脚本也是这么做的（它从 `/status` 读回保持），这不是妥协，是整份替换语义要求的正确姿势。

风险必须点名：这一句读改写让 sr-fib 的这张表有了第二个写者。今天写它的只有 lab 脚本和手工 `curl`；
上线之后运维如果还手工 `curl` 改逃生开关，agent 下一次推表会把它改回读到的那份吗——不会（读改写会保留它），
但只要 agent 在运维改之前 `GET`、在运维改之后 `PUT`，运维那一次就被覆盖掉。
这是这个方案里我认为唯一一处真正的并发写风险，第 6 节待拍 A 把它和逃生开关的归属一起拍。

### 4.6 触发（拍板 3）与为什么不落盘

三个触发，全部是"状态变了"，没有定时器去猜：

1. **开机**：隧道对账第一次成功之后。这一条自然覆盖"不知道谁先起来"——sr-fib 还没起就先记下"欠一次推送"，
   推送失败（连不上、答案 202）就在下一轮对账时再试。sr-fib 侧不用改：它的 1 秒轮询会按已经接受的那份自己收敛。
2. **隧道增删**：`TunnelSync` 每次对账结束都是天然的时机（bundle 的 `Version` 变了、或者某条链路建好/拆掉）。
3. **VPP 重启**：agent 已经能检测（缺陷④ 之后 `TunnelSync` 会换一条 api 连接并按 `DataPlane()` 让遥测跟着走，
   `internal/agent/tunnel.go:99-112`），换连接的这一次必须重推，因为 `interface` 那一列整个变了。

派生结果**不落盘**（拍板 2 由我决定，理由）：这张表的每一行都只在那一刻成立——口名会被 VPP 重启换掉，
下一跳会被控制台删掉。盘上存一份，开机时就必须在"用盘上那份"和"重新派生"之间选一个，
而选前者正是缺陷⑦ 那类"基线是假的"的源头。隧道本身能恢复是因为控制器会把隧道段再发一次，
这张表同理：段到了、口建起来了，表就派生得出来。盘上没有需要保留的额外信息。

### 4.7 状态口径：不造数据

派生器把三样东西报出来就够：本轮派生了几行、跳过哪几行以及原因、上一次推送的答案码与 sr-fib 回读的
`last.converged` / `last.warnings`。读不到就是"未上报"，不写 0（AGENTS 规则：控制台不造数据）。
是否给 agent 心跳加一段 `fib_table` 状态、让控制台隧道页能看出"这台盒子的 sr-fib 表还没对上"，
放在第 6 节待拍 C。

## 5. 中断口径（AGENTS 规则 4：必然中断的要点名并给实测）

先说结论：这条路径上不重启任何 daemon，也不清空任何东西。

- **重复推同一份表：0 次数据面写入。** `fib.Applier` 按上一次成功下发的计划做差分，
  sr-fib 今天本来就在每 1 秒重算同一份计划（只有 `res.Changed()` 才打日志，`controller/cmd/sr-fib/main.go:306`）。
  多一次 `PUT` 不多写一条路由。
- **开机那一次：纯增量。** 推之前 overlay 前缀一条都没装（第 1 节走的就是这个），装上去不会改动任何已在转发的路径。
- **新增隧道：纯增量。** 少一类路由变多，不涉及已有前缀。
- **删除隧道：** 这一行从表里消失之后，引用它的前缀路径按 `internal/fib/fib.go:302` 跳过。
  窗口 = 一次 `PUT` 往返 + 一轮收敛。**这一条我不写数字**：改完之后在 lab 用现成的 guard-probe 口径实测
  （和 #270 部署那次量 15.3s 用的是同一把尺子），量出来是多少就写多少。
- **改颜色：** `delete-then-add`，O4 实测窗口 0ms，这条路径没有变。
- **VPP 重启：** 缺陷⑦（提交 `051f37e`）已经让 sr-fib 自己重连并按整份计划重装。本方案加上触发 3，
  是因为 agent 那边接口名变了；两者叠加后 VPP 重启的自愈链不再需要 lab 手工补一步。

## 6. 等他拍三条

- **A：逃生开关（`escape_all_colors`）的归属。** 它是租户级配置，控制台已经有值和入口
  （`cmd/sr-ctrl/overlay.go:89`，注释说"O6-b renders it"，但第 4 段里实际没有渲染它）。
  三个选择：(甲) 本次一起做，把它加进第 4 段并由 agent 写进整份替换的体里，读改写不再需要保持现场；
  (乙) 本次仍保持现场（和今天的 lab 脚本一样），把渲染单独列一个缺口；
  (丙) 让它归盒上的 `fib.json`（运维字段），产品永远不动它。
  我倾向甲：它本来就是租户设置，(乙) 会把"谁覆盖谁"这个风险永久留在读改写的缝里。
- **B：隧道段没有 color 时（老控制器）怎么办。** 我倾向 agent **拒绝派生并报错**，
  而不是按 0 装进去——按 0 的后果是主备关系静默消失，比装不出来更难查。代价是设备必须和控制端同版本，
  这条要他认。
- **C：控制台要不要显示这张表。** 加了就能一眼看出"隧道 Up 但 sr-fib 没收到表"，
  不加就还得 ssh 进盒子读 `127.0.0.1:19011/status`。属于界面活，可以单独排。

## 7. lab 侧收口（拍板 4）

`vm-fib-sync.sh` 今天被三处调用：`test/lab/vmlab/exp-268/roll-268.sh:73`、`vm-state.py` 的注释（不是调用）、
`exp-270/boot-check.sh` 的注释。收口顺序：

1. 代码落地并在 lab 部署之后，把 `exp-268/roll-268.sh` 里那一行调用删掉，`vm-fib-sync.sh` 整个文件删除
   ——它记录的那个缺口的形状已经完整写进本文档第 3 节和 §9.6，不会丢。
2. `exp-270/boot-check.sh:110` 那条"别读成开机自己就好了"的判据换成真判据：
   sr-fib `/status` 的 `tunnels` 条数必须等于控制台 `GET /api/underlay` 里属于这台节点的隧道条数，
   `last.warnings` 为空，`last.converged` 为 true。数字还是打印实际值，不打印"应该生效了"。
3. `vm-verify.sh` 加同一口径的判据（四台都跑），并且加一条反向判据：这台盒子的派生结果来自 agent，
   不是来自某个 `curl`。具体读法：agent 的日志里有派生器那一轮的行数与跳过原因，`/status` 的答案码与
   回读能对上。

## 8. 做完的定义

1. `controller` 单测（容器里跑，`--privileged` 那条规则照旧）：颜色进隧道段、派生器三种输入的组装、
   口名查不到时排除并报原因、整份替换时 `escape` 保持不变、`vrfs` 空时不推。
2. harness 全绿（现有条数只增不减）。
3. lab：CPE1 一次真断电重起（`VBoxManage acpipowerbutton` 那条口径），不跑任何 lab 侧补丁脚本，
   `boot-check.sh` 判据全 PASS；四台 `vm-verify.sh` 全绿。
4. 前后数字进提交说明：隧道表条数、`warnings` 条数、overlay 前缀在 VPP FIB 里的条数、
   删隧道与改色两个窗口的实测丢包/时延。

## 9. 边界与规模（一次说清，省得后面猜）

每盒 4096 条是既定规模口径。`PUT /config` 的请求体上限是 1 MiB（`controller/cmd/sr-fib/main.go:520`
的 `MaxBytesReader`），一行 `{next_hop, interface, color}` 缩进后的 JSON 约 70 字节，
4096 行约 287 KB —— 装得下，不需要为此改接口。
派生器每轮都问一次 VPP 的接口表（`DumpInterfaces`），这台盒子规模上限下是 4096 行的遍历，
一秒一轮里这点开销可以接受；如果实测不是，再把这一问改成"只在隧道对账变化时问"。
