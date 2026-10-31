# O9：RR 只做反射（删掉我加的扇出闸）+ RR 分组

用户定案（2026-11-01，原话）：

> 「砍掉你 rr 里这个机制，标准的 rr 不是这么实现的，你又自己发挥了。标准的 rr 就是收到
> rr client 路由发给其他所有，并且不改变任何属性，收到非 rr client 路由只反射给 rr client」

对齐结果：这与 GoBGP `filterpath`（`pkg/server/server.go`，iBGP 段）的三条判断一致 ——
默认不把 iBGP 学到的路发给 iBGP 邻居；`info.RouteReflectorClient` ⇒ 放开（客户的路发给所有人）；
`peer.isRouteReflectorClient()` ⇒ 放开（可以发给自己的客户）。唯一的标准追加是 RFC 4456 的
ORIGINATOR_ID / CLUSTER_LIST，而防环已被用户三次否决（overlay 无环）⇒ "不改变任何属性"在我们
这里是字面成立的，不再讨论。

另一条已拍：**RR↔RR 的身份用证书 URI 角色段**（乙）——`srou://<tenant>/rr/<id>`，
不用设备号槽、不做特判分支。

## 他问过的那件事，答案记在这里

「RR 只是传递路由，和隧道表有什么关系？设备不是本地有自己的隧道表吗？」—— 对。设备本地那份
（`Client.tunnels` / `Client.nextHops`，client.go:232/447-450）负责第 3 步解析下一跳，**与 RR 无关**。
RR 手里那份隧道表（`Reflector.tunnels`）只被**一处**用到：我加的扇出闸，即
`mutualPeersLocked`（reflector.go:268）"双向都宣告过才反射"，理由是 reflector.go:23-26 写明的
"别让一千隧道的 hub 的前缀被反射成一千个下一跳"。这是 BGP 里没有的东西，是我发挥的。
它语义上是冗余的（收端解析不出来会挂 `no_path`），所以删掉不损失正确性，损失的是扇出。

⇒ 分组不需要"同步隧道表"。需要的是把这道闸删掉，RR 回到"只反射"。

## 要删的东西（逐处）

| 位置 | 内容 | 处理 |
| --- | --- | --- |
| reflector.go:21-30 | 头注释里"由隧道图推导邻接"的说明、`ReflectTo` 注释 | 重写为"RR 只反射" |
| reflector.go `ReflectorConfig.ReflectTo` / `buildPolicy` / `Reflector.policy` | 建立在邻接之上的策略 | **删**（见下面"待确认"） |
| reflector.go `r.tunnels` | session 替换(:206)、detach(:598)、`applyTunnels`(:435)、`Tunnels()`(:102)、`Sessions()` 的 `Tunnels` 字段(:672) | 全删 |
| reflector.go:254-305 | `peersLocked` / `mutualPeersLocked` / `targetsLocked` / `sendersLocked` | 全删 |
| reflector.go:326-335 | `case MsgTunnels` | 删 |
| reflector.go:241-249 | `snapshotForLocked` | **保留**，改成"所有会话的路由，去掉来源设备自己的那条"（这就是标准的"不回发来源"，= GoBGP `filterPathFromSourcePeer`） |
| proto.go:85、:364 | `MsgTunnels` 常量与合法类型表 | 删 |
| client.go:979-989、:1148、:348 | `pushTunnelsLocked`、建会话时的首帧隧道表、`announcedTunnels` | 删线上部分 |
| client.go:232/436-451 | `c.tunnels` / `c.nextHops` / `normalizeTunnels` | **保留**（本地解析下一跳，与 RR 无关） |
| cmd/sr-rr | `/status` 的 `tunnels` 字段、`GET /tunnels`(:239)、`reflect_to` 配置项(:49,:126,:135) | 删 |
| 测试 | reflector_test.go 的邻接/策略用例；test-route.sh C 段（reflect_to）与"没隧道不反射"断言；test-rr-switch.sh / test-fib.sh 里同类断言 | 换成"照发、收端挂 no_path"的口径 |

## 要加的东西（只有一处）

**会话 established 后立刻发本侧全表 + EOR。** 今天这个动作藏在 `applyTunnels` 的 `first` 分支里
（reflector.go:457-466），闸一删就没人触发它。落点：回完 `MsgHelloOK` 之后直接调
`announceEnvelopes(r.snapshotForLocked(dev))`（`announceEnvelopes` 末尾本来就补 EOR，:235）。
等价于 BGP 的 initial UPDATE 突发 + END-OF-RIB。

如实记副作用：盒子启动时可能"先收到路由、后把自己的隧道表 PUT 给 sr-route"，那几条先挂
`no_path`，`SetTunnels` 重算后自己进表（client.go:621-624）。BGP 里就是"路由先到、下一跳后解析"，
不是缺陷。

## 分组：配置面

- 模型：租户加 `rr_groups: [{id, name, device_ids[], rr_primary, rr_backup}]`；
  **没被任何组选中的设备继续用顶上那两个槽**（一条规则，不加"分组/非分组"分支）。
- 选 RR 不另加校验：租户选 RR 本来就只有 `rrSelectionErrorLocked` 这一把闸（只认归本租户
  的行），分组里选 RR 走同一处，因此不需要"组内 RR 必须属于本租户"这种单独的检查。
- 查找：`rrForTenantLocked(tenantID)`（cmd/sr-ctrl/rr.go:181）→ `rrsForDevice(tenantID, nodeID)`。
- 控制台：Overlay 全局页在 RR 1/RR 2 两格上方加分组块：多选设备（复用可过滤多选列表）+
  选两台 RR（复用现有单选下拉）。
- 不动：协议形状、第 4 段 `rrs`（本来按节点渲染，overlay.go:622）、sr-route `PUT /rrs`、
  C-c 的 make-before-break、发布闸门、RR 库。

## 分组：架构面

- 会话角色：设备会话 = client，RR 会话 = 非客户。反射规则照抄上面那三条。
- RR↔RR 会话落在**同一个 Reflector 实例**内（一实例一租户，reflector.go:38、:143），
  `PeerIdentity` 不必扩成租户集合；证书 URI 加角色段，`ServeConn` 不再要求 RR 会话带 `DeviceID`。
- sr-rr 今天只会 `Accept`（cmd/sr-rr/main.go:166），网格要加**拨出侧**和双向证书。
- 邻居清单来源：控制器给 RR 也渲染一份配置（与设备第 4 段同源、不同文件），内容是"本租户
  出现过的所有 RR 地址（去重、去自己）"。
- 扇出代价（如实记）：跨组前缀会无差别发给每台客户端，由收端挂 `no_path` 消化。这是删闸
  换来的简单性，代价是每盒路由数 = 全租户前缀数。

## 待确认一件小事

`reflect_to` 的存废。它今天只能"在邻接之上做减法"（reflector.go:28-30），闸删了就没有基准。
**我建议一起删**（test-route.sh C 段随之删）；若要保留，改成"绝对白名单"（配置了就只发给这些
目标），不再是减法。这一条不影响其它部分，可以先按删准备。

## 顺序与验收

- **先跑完 O7 R1..R5**。删闸之后"同一个前缀多来源"变成常态，没有 Loc-RIB 之前
  `pickActiveLocked`（client.go:1331）只会认一条会话，会丢掉另一半。
- O9 内部两步：**O9-a 删闸**（RR 只剩反射；测试/脚本换口径）→ **O9-b 分组**（配置面 + 身份乙
  + 网格）。两步各自可独立验收、独立提交。
- 判据：删闸后"没隧道不反射"的断言反过来（照发 + 收端 `no_path`）；`test-rr-switch.sh` 的
  零缺口硬指标不变；harness 196 条；gofmt / vet / `-race` 全绿。
- 部署顺序：sr-rr 与盒子**必须同版本**（旧盒子发 `tunnels` 会被新 RR 当未知类型断连），
  lab 里一起换。协议版本号暂不升（4 仍是 3），要升你说一声。

## O9-a 落地状态（2026-10-31）

已按上面"要删的东西"全部落地；`reflect_to` 按建议**一起删**（配置项、`buildPolicy`、测试与
test-route.sh 的 C 段）。线上协议不再有 `tunnels` 帧：`MsgTunnels` 常量、`Envelope.Tunnels`
字段、每帧隧道数上限、`case MsgTunnels` 全部移除；`ValidateTunnels`/`MaxTunnelsPerDevice`
留在本地（`normalizeTunnels` 仍用）。

- `Reflector`：只剩 `sessions` + `adjIn`；`SessionInfo` 不再有 `Tunnels`；`attach` 在回完
  `hello_ok` 后立即发快照 + EOR（`announceEnvelopes(snapshotForLocked)`），快照与扇出都只做
  "除了来源之外的所有会话"。
- `Client`：`SetTunnels` 只重算本机表，不再向 RR 发任何东西；`pushTunnelsLocked`、
  `clientSession.announcedTunnels`、重连补发隧道表全部删除。
- `cmd/sr-rr`：`fileConfig` 去掉 `reflect_to`；`/status` 只返回 `{rr_id, tenant, sessions,
  routes}`；`GET /tunnels` 删除。
- 新增回归：`TestSnapshotFollowsHelloOK`（不送任何表也能收到快照）、`TestTunnelsFrameIsRejected`
  （旧帧被当未知类型、会话结束）、`TestSnapshotOnConnectAndFanout` 改成"来源永远收不到自己的
  路由、晚加入者按 RT 长度拿到全部"。
- 判据（全绿）：`gofmt`/`go vet`/`go build ./...`；`go test -race ./internal/route/`；`go test
  ./cmd/...`；`test-route.sh` 21/0（新增 E 段：收端只有到 node3 的隧道 ⇒ node1 的前缀收到但挂
  `no_path`）；`test-fib.sh` 36/0；`test-refresh.sh` 51/0（A/B/E 段改成"零隧道也收路由、
  撤隧道不动对端与 RR"）；`test-rr-switch.sh` 57/0；`test-boot.sh` 90/0；`test-vbond.sh` 196/0；
  `test-e2e.sh --keep` 46/0。

O9-b（分组：配置面 + 身份乙 + 网格）未开始。
