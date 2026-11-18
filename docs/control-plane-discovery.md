# 控制面发现：CPE 与 RR 怎么找到控制器（含 HA）

> 状态：**已实现**（2026-11-18：`internal/ctrladdr` + `sr-ctrl -advertise` + RR
> `controllers[]`）。本文是这套机制的最终口径；早期版本里的「入口地址 `-entry`」
> 「`ctrl_fallback`」「证书 SAN 改造」都**不在**这套设计里，理由见 §7。

## 0. 角色与术语

| 角色 | 二进制 | 业界对应 | 形态 |
| --- | --- | --- | --- |
| 控制器 | `sr-ctrl` | vBond（入网/NAT 观测）+ vManage（控制台/编排） | 一个进程，两个职责 |
| 路由反射器 | `sr-rr` | vSmart | 独立进程，可多副本，网格由控制器给 |
| CPE | `sr-agent` + VPP | vEdge / cEdge | 盒子内的 agent |

vSmart 就是 `sr-rr`，它一直是独立进程、独立身份、独立升级——早期文档把它写成
"sr-ctrl 三合一"是错的。本文只回答两件事：**CPE 和 RR 各自怎么找到控制器**，以及
**加/减控制器、控制器宕机时这套机制怎么收敛**。入网协议（激活码）不变。

## 1. 契约：一个种子，一张表

| | 种子（配置里只有一个） | 连上之后 | 落在哪儿 |
| --- | --- | --- | --- |
| **CPE** | 激活命令里的控制器地址（`--ctrl`，今天的形态不变） | 每拍心跳应答带 `tls_addrs` + `leader` | `<id-dir>/ctrl-addrs.json` |
| **RR** | 配置里的 `controller.addr`（一个地址） | 每次 poll 的 `/v1/rr/config` 带 `controllers[]` + `leader` | `<config 同目录>/ctrl-addrs-<rr_id>.json` |

两个角色共用同一份实现（`internal/ctrladdr`），因为它们是同一类角色：都被给了一个
种子，然后靠应答里带回的全表跟着集群走。**没有"主控制器"这个配置项**：leadership
会漂，而且每台节点都能应答首接触（心跳由 follower 直接答，写操作 307 到 leader），
所以种子写哪一台都行——写"当前主"只是让配置跟着一个会变的状态走。

规则（是切换语义，不是性能优化）：

1. **leader 变了立刻切过去**，不等失败。写只在 leader 上成立，跟着它少一次 307。
2. **连不上立刻试下一个，不退避**；整张表都试过一轮才退避。否则热切换名义上自动、
   实际要等一个周期。
3. **种子永远留在候选里**，哪怕某次应答的成员表里没有它。
4. **通配地址永远不进候选**（`0.0.0.0`、`::`）——它只会让客户端去连自己。
5. **落盘，重启先试上次成功的那个**，而不是回到可能已经下线的种子。

第 5 条解决的是"重启就掉线"：候选表以前只在内存里，agent 一重启就回落到配置里的
种子；如果那一刻种子节点正好在换机，本来正常的设备会卡在死地址上，日志里只有
connection refused。

## 2. 根因：控制器没有"我叫什么"

`-listen 0.0.0.0:8444` 只是**绑定**，不是任何人能拨的地址。缺了这个语义，四个地方
只能手填 IP：CPE 的 `--ctrl`、RR 的 `controller.addr`、RR 行的 `addr`、以及单机心跳
里的 `tls_addrs`（以前直接是 `-listen` 原值，于是 `0.0.0.0:8444` 被当成控制器地址
发下去，设备的地址簿里就多一条"连自己"的候选）。

2026-11-18 把控制面从 Mac（192.168.1.4）迁到 192.168.1.100，要手工改 4 台设备 + 2 份
RR 配置 + 成员表——这就是根因的账单。

## 3. 控制器侧：`-advertise`

新增 `sr-ctrl -advertise host:port`，语义是"设备与 RR 该用哪个地址找我"。缺省按下面
顺序推（`cmd/sr-ctrl/advertise.go`，带单测）：

1. `-advertise`（放在 NAT 后、做过端口映射时必须用它）；
2. `-tls-san` 的第一项——证书里设备要校验的名字本来就得是这个地址，声明一次两处生效；
3. `-listen` 的主机部分（当它不是通配值）；
4. 默认路由出口的本机地址（UDP dial 只做路由查找，不发包）；
5. 兜底用 `-listen` 原值，并把这个值写进启动日志。

`localhost` / 回环 / 通配一律跳过：它们对另一台机器上的设备没有意义。

**成员表里自己那条是入群时填的记录，可能已经过时。** 今天 .100 的成员表里 `mac-1`
仍写着 `192.168.1.4:8444`（旧 Mac）。所以对外地址表（`memberEndpointsLocked`）把
自己那一条换成 `-advertise` 的值，别的成员照旧用成员表——那才是它们各自宣告的地址。
启动时若两者不一致，日志会写明：

```
cluster: 成员表里本机地址是 192.168.1.4:8444，本机宣告的是 192.168.1.100:8444；
设备与 RR 会用后者，节点间拨号仍按前者（建议在控制台把成员地址改过来）
```

不自动改共识：成员变更是运维动作，静默重写会让"谁改了成员表"没有痕迹。

## 4. CPE 侧（2026-11-19 起激活码自带地址与 CA）

出码时控制器把 **tenant CA PEM + 对外地址表（`tlsAddrs`）** 包进激活码信封：

```
srs1-<base64url(JSON{code, ctrl[], ca})>
```

- 信封里只有内层 `code`（`srk_…`，形状不变）是秘密；ctrl/CA 是公开事实，**不参与
  哈希**。控制器存 `/v1` 侧的仍是内层 secret 的 SHA-256。
- **TTL、吊销、名额完全不动**：到期判定、`expireClaimCodesLocked`、座位记账全部
  以授权记录为准，与码的形状无关（owner 明确要求保住 TTL 这条安全底线）。
- 现场装机只剩一条命令、零拷文件（CA 与地址随码走）：

```
sr-agent -claim <整段信封>
```

取值顺序：`--ctrl` 显式参数 > 信封内嵌端点（取第一项，其余随后续应答进候选表）；
CA：`--ca` 文件 > 信封内嵌 CA > `<id-dir>/pinned_ca.pem`。
- **向后兼容**：不带 `srs1-` 前缀的旧码按原样 enroll，已有测试钉住
  （`cmd/sr-ctrl/claim_test.go` 的 `TestPlainLegacyCodeStillEnrolls`）。
- 站点级 `site.json` 这一层仍未落地——信封已覆盖了它想解决的场景，优先级更低。

## 5. RR 侧

两条启动路径，最终汇进同一份运行时结构：

- **`-config`**（原有）：JSON 里有 `listen`、`status_addr`、三个 PEM 路径与
  `controller.addr`（种子）。
- **`-identity`**（2026-11-19 新增）：控制台"下载 rr-identity.json"的文件里现在
  带 `controller_addr` 字段，一文件即全部输入：

```
sr-rr -identity rr-identity-<rr_id>.json
```

证书是权威：文件里的 `rr_id`/`tenant` 只能证实证书、不能改写它（矛盾即拒绝）。
listen 取文件 `addr` 的端口绑所有接口（host 部分是别人拨的地址，未必本机可绑），
`-listen`/`-status` 可覆盖。候选表落盘在 identity 文件旁边
（`ctrl-addrs-<rr_id>.json`），与 `-config` 路径同一规则。

两条路径连上控制器之后的协议完全一样：

```
GET /v1/rr/config          （用 RR 自己的客户端证书认证）
→ { tenant, rr_id, peers:[{id,addr}], controllers:[...], leader:"..." }
```

`controllers` 与设备拿到的是同一份列表（`memberEndpointsLocked()`）。字段为空表示
对端是老控制器（或单机推不出地址）——RR 继续用配置里的地址，也就是旧行为，不会拿
一张空表把自己锁死。

## 6. HA 三个场景

| 场景 | 怎么传播 | 生效时延 |
| --- | --- | --- |
| 加一台控制器 | 成员表多一行 → 对外地址表多一项 → 下一拍应答带上 | 设备 ≤1 心跳（5s）；RR ≤1 poll（30s） |
| 减一台控制器 | 成员标 `decommissioned` → 立刻掉出地址表；被下线的那台自己退场 | 同上 |
| 控制器宕机（含 leader） | 不需要配置变化：设备与 RR 按候选表轮换，写操作 307 到新 leader | 设备：下一个候选（≈一次超时）；RR：同上 + 一次退避 |

三条都不需要登设备改配置。唯一没法自动覆盖的是**种子本身**：装机/启动那一刻它正好
下线，就无解（还没拿到过表）。这是已知边界，见 §8。

## 7. 代码事实：配置为什么是 pull

| 路径 | 机制 | 位置 |
| --- | --- | --- |
| 隧道/策略配置 | pull：心跳应答带 `config_version`/`tunnel_version`，变了才去拉 | `internal/wire/wire.go`、`cmd/sr-agent/main.go` |
| 控制器地址表 + leader | pull：每拍心跳应答 | `HeartbeatResponse.TLSAddrs/Leader` |
| STUN / reflector | pull：同一拍应答 | `resp.Reflectors` |
| RR 网格与控制器列表 | pull：每 `poll_sec` | `cmd/sr-rr/mesh.go` |
| 浏览器控制台 | push（SSE） | `console.html` → `/api/events` |
| peer registry | push（JSONL 长连接）`/v1/watch` | **两端都实现了，但当前没有调用方** |

配置之所以是 pull，`wire.go` 里写得很直白：*the controller never dials the box*——
设备在 NAT 后面，控制器拨不进去。所以"主控制器给 CPE/RR 下发新控制器信息"的实际形态
是"下一次应答里带上"。真要做主动推送，唯一可行通道是设备自己建的 `/v1/watch`；本轮
**没接**：5s/30s 对增删控制器够用，接推送还要处理重连与断线期间的补偿（补偿仍然得靠
pull）。

## 8. 已落地 / 明确不做 / 已知边界

**已落地**

| 文件 | 改动 |
| --- | --- |
| `internal/ctrladdr/book.go`（新） | 候选表：种子常驻、通配过滤、leader 优先、轮换、落盘（0600，内容不变不写） |
| `cmd/sr-agent/main.go` | `ctrladdr.Load(<id>/ctrl-addrs.json)`；启动接上次成功的地址；成功后 `Learn`+`Save`；删掉旧 `addrs.go` |
| `cmd/sr-ctrl/advertise.go`（新）+ `-advertise` | 宣告地址推导；单机不再下发 `0.0.0.0:8444` |
| `cmd/sr-ctrl/cluster.go` | 对外地址表里"自己"用宣告地址；启动告警成员表过期 |
| `cmd/sr-ctrl/rr_api.go` | `/v1/rr/config` 增加 `controllers[]` + `leader` |
| `cmd/sr-rr/mesh.go`、`main.go` | 同一份 `ctrladdr`；失败轮换、成功落盘；种子来自 `controller.addr` |

测试（`go test ./...` 全绿，关键包过 `-race`）：`internal/ctrladdr` 覆盖"种子不丢、
一轮只试一次、落盘往返、换种子重来、通配过滤、坏文件不阻塞启动"；`cmd/sr-rr` 覆盖
"首地址挂掉切下一台、吸收 `controllers[]` 并落盘、404 致命退出"；`cmd/sr-ctrl` 覆盖
"应答里有 `controllers[]`、没有地址时字段不出现、单机地址表用宣告值而非成员表旧值"。
其中"轮换"与"吸收"两条做过变异验证——去掉修复会变红。

**明确不做**

| 不做 | 原因 |
| --- | --- |
| `-entry`（集群入口名）作为独立概念 | 种子的语义就是"一个地址"；要 DNS/VIP 就把种子写成域名，不必多一个参数 |
| 把 DNS/VIP 当前提 | 很多现场给不出来。纯 IP 方案下 HA 场景同样能正确收敛 |
| 证书 SAN 的"名字变更自动重签" | 纯 IP 方案里设备连的是各节点 `Endpoint`，SAN 本来就含它；只在用域名连时才需要 |
| ~~激活码携带地址~~ → **2026-11-19 推翻并落地** | 原判"KB 级 blob 现场抄不了"的前提是手抄；实际现场是串口粘贴，粘贴一段完全可行，且省掉 pinned_ca.pem 拷贝与 `-ctrl/-ca` 两个参数。信封见 §4，TTL 原样保留 |
| `/v1/watch` 接成配置推送 | 见 §7 末段 |
| 两级种子（门厅 + 控制器） | 运维要记两个名字，出错时判断不了该改哪个 |

**已知边界**

- 种子里只有一个地址：装机那一刻它正好下线，就没有第二次机会（已拍板接受）。
- 信封码太长，无法手抄：现场必须是复制粘贴（串口/web 控制台/ssh）。要手抄的场合
  用旧式纯 `srk_` 码（控制器仍兼容），代价是 `-ctrl`/`-ca` 要手工给。
- 种子节点被 `decommission` 后，拿它当种子的设备/RR 要改一次配置；只要候选表还在就
  不受影响（种子会被替换成存活的那台）。
- 成员表里**其他**节点的 `Endpoint` 仍是"入群时填的"：本机改 IP 会自动对外宣告新地址，
  但别的节点拨它仍按旧值。多机部署改 IP 时要在控制台更新成员地址。

## 9. 怎么验

单机（今天 .100 的形态）：

```
# 启动日志应有这一行，且不是 0.0.0.0
control plane: listening on 0.0.0.0:8444, devices and RRs should use 192.168.1.100:8444

# RR 视角：controllers[] 里是真地址
curl -s --cert <rr.crt> --key <rr.key> --cacert <ca.pem> \
  https://192.168.1.100:8444/v1/rr/config | jq '.controllers, .leader'
```

多机（lab）：

1. 3 节点起来：设备心跳应答的 `tls_addrs` 是 3 项，RR 的 `controllers` 也是 3 项。
2. 杀 leader：设备 ≤2 个心跳周期恢复（日志出现 `switching to ...`），RR ≤1 poll +
   一次退避；两者都不需要重启。
3. 加第 4 台：设备一个周期内看到 4 项，设备零改动。
4. 下线一台：一个周期内不再尝试它；RR 侧日志出现 `left the grid`。
5. 重启一台设备：日志出现 `resuming on ...`，先试上次成功的地址。

信封与单文件（2026-11-19）：

6. 控制台出码：码以 `srs1-` 开头，reveal 框是可滚动的整段文本；旧版纯 `srk_` 码
   （若有留底）仍能 enroll。
7. 新盒子只贴一条 `sr-agent -claim <信封>`（不带 `-ctrl/-ca`）：日志依次出现
   `--ctrl taken from the claim code`、`trust anchor: claim code (embedded CA)`、
   `enrolled: tenant=… node=…`。
8. RR 单文件：控制台签发身份 → 下载 rr-identity.json → 盒子上
   `sr-rr -identity rr-identity-<id>.json`，日志 `tenant … listening on :<port>`、
   `grid from https://…`；文件与证书字段矛盾时拒绝启动。
