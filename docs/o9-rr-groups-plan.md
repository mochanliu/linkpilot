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

## 待确认一件小事（已定：一起删）

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

## O9-b1 落地状态（2026-10-31，配置面 + 控制台）

一件事：**这台设备连哪两台 RR**，被组点名的用组里的两台，没被点名的用租户那两格——一条规则，
没有第三种情况。

- 模型（`cmd/sr-ctrl/overlay.go`）：`RRGroup{id,name,device_ids,rr_primary,rr_backup}`；
  `OverlayConfig.RRGroups` + `NextRRGroup`。`ensure()` 去空白、设备 id 去重排序（存盘与页面
  不把重排当成改动）。
- 写：`PUT /api/overlay/settings {rr_groups:[…]}` 整张列表替换（删掉一组 = 不再列出它），
  `id` 由服务端发（`rg%d`），**计数随列表一起提交**：被拒的保存不烧号。
- 读：`rrsForDeviceLocked(tenant, node)` 先查组（`rrGroupOf`，第一个命中）再落租户两格；
  `overlayRouteArtifactLocked` 因此本来就按节点渲染，第 4 段 `rrs` 不用改形状。
- 闸门：`rrGroupsChecksLocked` —— 组名必填、至少一台设备、两格都走 `rrSelectionErrorLocked`
  （租户归属/不存在等）、至少一台 RR、RR 1≠RR 2、设备不重复、设备属于本租户、一台设备只在
  一组；存盘状态每次读 `/api/overlay` 再查一遍（`overlayRRGroupChecksLocked`），只报不修。
- E5 删除拒绝：`rrReferencesLocked` 现在也扫分组，删一条被组点名的 RR 仍是 409。
- 控制台（Overlay › 全局）：RR 分组表格（组 / 设备 / RR 1 / RR 2 / 编辑·删除）+ 新建/编辑
  表单（组名、可过滤多选设备、两个 RR 单选下拉）。四个控件都是草稿驱动（每次输入写 `o.rg`），
  因此带 `data-keep`——否则 `render()` 的"保住用户没提交的输入"会把刚保存完的空表单重新填回
  旧值（多选还会只剩第一台设备）。
- 判据：`rr_groups_test.go` 5 条（按设备覆盖 reflector / 五种拒绝且不写状态不烧号 / 组也是删除
  引用 / 存盘脏状态只报不修 / 规范化与重载）；`gofmt`+`go vet ./...`+`go test ./cmd/...
  ./internal/...` 全绿。
- 真实浏览器（租户 `gui-check`，临时给该租户建了一条指向 lab 在跑的 RR 的行，验证后已删）：
  建组 → 行出现在表格；编辑 → 表单回填两台设备；取消 → 表单清空；重复设备 → 行内报「设备 2
  已经在分组 rg1 里」且输入不被清掉；删除 → 确认框 + 回到 0 组；再建 → 表单在保存后**清空**。
  `GET /api/config?node=` 的 `artifacts.route`：组内 node 2/3 带 `rrs:[rr4]`，组外 node 4 没有
  `rrs` 键。

身份乙（证书 URI 角色段）在 O9-b2a 落地；RR↔RR 网格（sr-rr 拨出侧 + 双向证书 + 邻居清单）
与 RR 身份签发是 O9-b2 的 b2b/b2c，未开始。

## O9-b2 方案（身份乙 + RR↔RR 网格，未开工）

三件事，按这个顺序做，每件独立可验收：

**b2a 会话模型（internal/route + sr-rr 接入侧）**

- `keybinding`：新增 `RefOf(cert)` 解析两种身份——`srou://<tenant>/node/<n>`（客户端）与
  `srou://<tenant>/rr/<id>`（反射器）。`IdentityOf` 保持"只认 node"，设备侧/控制器侧调用点
  一行不改，也顺带保证 RR 证书当不了设备证书。
- `route.PeerIdentity` 加角色：客户端 `{Tenant, Role: client, DeviceID}`，RR
  `{Tenant, Role: rr, RRID}`；`Key()` 给出会话键 `node/<id>` / `rr/<id>`——**反射器里所有按
  设备号索引的表都换成这个键**（`sessions`、`adjIn`），因为一个 RR 会话可以替任意来源转发。
- 反射规则就是一条：**一条路由可发给某会话，当且仅当它的来源不是该会话，且（来源是客户端 或
  接收方是客户端）**。快照与扇出都走这一条（快照对客户端 = 除自己外的全部，对 RR = 只含
  客户端来源的那部分）。这就是 GoBGP filterpath 的两条规则，不需要第三条。
- 宣告校验：客户端会话仍然"路由的 device_id 必须等于自己的 id"；RR 会话不校验来源设备号
  （同一租户由证书保证），但来源仍是路由自己的属性。
- 撤销要按"并集"判：同一个 (来源, VRF, 前缀) 可能同时存在于两个会话（设备直连本 RR，而这个
  前缀又被另一台 RR 反射进来）。一条会话撤走时，只有**没有任何会话还持有它**才把撤销发出去，
  否则会把另一条会话还举着的路由从收端抹掉。
- 状态口：`/status` 的 sessions 增加 `role` 与 `key`；`routes` 仍按来源设备号聚合（对外口径不变）。

**b2b 网格（sr-rr 拨出侧 + 控制器渲染邻居清单）**

- sr-rr 配置加 `neighbours: ["10.99.0.101:18446", …]`，进程用**自己的**证书（同一份
  cert/key 作客户端证书）拨出并保持会话；拨出侧与接入侧用同一套 hello/快照/增量代码（走
  `route.Client` 的 RR 变体，或把会话状态机抽成一份两端共用）。
- 邻居清单由控制器渲染：本租户库里出现过的所有 RR 地址，去重、去掉自己；与设备第 4 段同源、
  不同文件（`rr.json` 的 `neighbours` 字段）。控制器不参与连通性判断，空清单就是"没有网格"。

**b2c RR 身份签发（controller）**

- 今天 RR 的公钥/证书是手搓的（lab 里 `/etc/sr/rr.crt` 只有 IP SAN，没有任何 srou URI）。
  身份乙要求证书带 `srou://<tenant>/rr/<id>`，所以控制器要能给一条 RR 库行签发身份：运营方
  在 RR 库里点"签发身份" → 控制器用租户 CA 签一张带 URI SAN 的证书（外加这台 RR 的地址作为
  IP SAN，供 TLS 主机名校验）→ 一次性下载 cert+key（key 只在这一次返回，不落控制器）。
- 这一步之前，lab 里的 RR 只能继续用手搓证书，b2a/b2b 的联调用 harness 现生成两张 RR 证书。

**判据（b2a）**：单测 —— 客户端路由扇出到所有会话、RR 来源只扇出给客户端、RR 加入时的快照
只含客户端来源、两会话持有同一 (来源,前缀) 时撤一条不扇出、RR 会话宣告他人前缀被接受而客户端
会话宣告他人前缀仍被拒。全部 `go test -race ./internal/route/ ./internal/keybinding/` 绿；
`test-route.sh` / `test-rr-switch.sh` / `test-boot.sh` 原口径不变。

## O9-b2a 落地状态（2026-10-31，会话模型）

按上面的 b2a 落地，实现中补了三处方案里没写死的细节：

- `keybinding`：`Role`（`node`/`rr`）、`Ref{Tenant,Role,NodeID,RRID}`、`ParseRef`、`RefOf`、
  `ValidRRID`（1..64 字节 `[A-Za-z0-9._-]`，避免 URI 转义与会话键歧义）。`ParseRef` 比老的
  `ParseURI` 严：角色段之外多一个路径段、`node/` 后带非数字、`srou` 但无 host 都拒绝。
  `ParseURI`/`IdentityOf` 原样保留（设备侧与控制器侧调用点一行未改，RR 证书也因此当不了设备证书）。
- `route`：`PeerRole`（`client`/`rr`）+ `PeerIdentity{Role,DeviceID,RRID}`，`Key()` 给
  `node/<n>` / `rr/<id>`；`ServeConn` 先 `Validate`（没有角色的身份直接拒，**不默认成 client**）。
  `sessions` 与 `adjIn` 都按会话键索引；`adjIn` 的每一项带 `role`——规则问的是"这条路由从哪
  个会话进来"，不是路由里的 `device_id`。
- 会话规则：客户端 hello 的 `device_id` 必须等于证书；**RR 会话的 hello 不许带 `device_id`**
  （一个设备号只能属于一个节点）；RR 拨向自己（证书 `rr/<自己的 rr_id>`）在建会话前就拒——
  否则自己的表会以第二份拷贝回到客户端。
- 快照/扇出一条规则：`来源 ≠ 接收方 && (来源是 client || 接收方是 client)`。RR 加入时快照
  只含客户端来源；RR 收到的反射路由不再转给别的 RR。
- 撤销按并集：删掉一条会话的 key 之后，只有**没有任何别的会话还持有它**（且那个会话对接收方
  可见）才发 withdraw。同一个 key 可以同时来自"设备直连"和"另一台 RR 反射"，撤一份不能抹掉
  另一份。
- `/status`：sessions 每项加 `role`/`key`（按 key 排序），`device_id` 对 RR 会话是 0；
  `/routes` 仍按来源设备聚合，同一个 key 有多份时确定性取一份（先 client 来源，再按会话键），
  两次轮询不会给出不同的 RT 列表。
- `cmd/sr-rr`：用 `RefOf` 分类证书 → `sessionIdentity` 映射成 `PeerIdentity`；日志里会话名
  改成身份（`srou://…` 与 `node/1` 这种会话键），`test-rr-switch.sh` 那条"RR-A 有没有关闭
  记录"的断言从 grep `device 1` 改成 grep `node/1`（判据含义不变，只是名字跟着代码走）。

判据（全绿）：`gofmt`/`go vet`/`go build ./...`；`go test ./internal/keybinding/`、
`go test -race ./internal/route/`（新增 4 组：RR 来源只到客户端、RR 快照只含客户端来源、
并集撤销、会话规则/自身拨号/无角色拒绝）；`test-route.sh` 21/0；`test-rr-switch.sh` 57/0
（真二进制 + 真 mTLS，租户 o6b-lab 的设备证书走新的 `RefOf` 分类）。
`test-boot.sh` 这一步没跑：它会 `docker restart` srwg-node1/2，而那两个容器刚被别的会话重启
在用；b2a 不碰开机清单与 agent，`test-rr-switch.sh` 已经用真证书覆盖了 sr-rr 的接入侧。

b2b（sr-rr 拨出侧 + 控制器渲染 `neighbours`）与 b2c（控制器签发带 `srou://<tenant>/rr/<id>`
的 RR 身份）未开始。b2b 之前，两台 RR 之间还不能建网格：今天的 sr-rr 只有接入侧，所以
"RR 来源只扇出给客户端""并集撤销"这些规则只有单测覆盖，端到端要等 b2b。
