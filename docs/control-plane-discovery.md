# 控制面发现：CPE 与 RR 怎么找到控制器（含 HA）

> 背景：交付形态讨论（deb/rpm vs image）真正难的不是"装得上"，是**装完以后 CPE 和 RR
> 各自从哪儿知道"控制器在哪"**——而且这套地址机制必须在"加控制器 / 减控制器 / 控制器
> 宕机"三种场景下都成立。本文只回答这两件事，不改入网协议。

## 0. 先更正术语（上一版写错的地方）

上一版说"sr-ctrl 是 vBond + vSmart + vManage 三合一"，**错了**：vSmart 是 RR，它一直是
独立进程 `sr-rr`，跟控制器分开部署、分开签发身份、分开升级。

| 角色 | 二进制 | 业界对应 | 今天的形态 |
|---|---|---|---|
| 控制器 | `sr-ctrl` | **vBond（入网 / NAT 观测）+ vManage（控制台 / 编排 / 配置下发）** | 一个进程，两个角色合一 |
| 路由反射器 | `sr-rr` | **vSmart** | 独立进程，可多副本，靠控制器下发网格 |
| CPE | `sr-agent` + VPP | vEdge / cEdge | 盒子内的 agent |

所以"vBond 和控制器在一起"不影响任何设计结论——它本来就是一个二进制里的两个职责。

## 1. 一句话根因

控制器**没有"我叫什么"这个概念**。`-listen 0.0.0.0:8444` 是绑定地址，不是可达地址；
于是所有下游产物都靠人手填 IP：CPE 的 `--ctrl`、RR 配置的 `controller.addr`、RR 行里的
`addr`、心跳应答里的 `tls_addrs`。这次把控制面搬到 .100，要手工改 4 台设备 + 2 份 config
+ 成员表，根子都在这里。

HA 把这个根因放大成三种故障：**加节点**时设备不知道新节点、**减节点**时设备还在试死
地址、**宕机**时设备和 RR 各自卡在原地（RR 尤其严重，见 §4.7 G-B）。

## 2. 现状盘点（代码事实）

| 角色 | 首接触要什么 | 之后怎么发现 | 证据 |
|---|---|---|---|
| CPE `sr-agent` | `--ctrl host:port`（必填）、`--claim <激活码>`（首次）、`--ca <CA 文件>` | 心跳应答带 `tls_addrs` + `leader`，agent 自己轮换、换主立即切 | `cmd/sr-agent/main.go:146-160`、`cmd/sr-agent/addrs.go:50-75` |
| RR `sr-rr` | **四样**：`-config` 里的 `listen`、`status_addr`、三个 PEM 磁盘路径，加 `controller.addr` | 每 `poll_sec`（默认 30）拉 `/v1/rr/config`，按行拨网格 | `cmd/sr-rr/main.go:45-104` |
| 控制台 | 浏览器书签 | — | — |

四个事实：

1. **入网机制本身没问题**：控制台生成一次性激活码（默认 5 分钟有效、只存哈希），设备带码
   redeem。**继续沿用，不动协议。**
2. **读路径 follower 也服务**：心跳不 307（`handleHeartbeat` 直接读本地状态），所以设备
   打到任何一台都能拿到地址表。写路径（enroll、改配置）才 307 到 leader。
3. **`tls_addrs` 已经是成员表派生**：`activeEndpoints()` 只取 `status=active` 的成员
   （`cluster.go:122-130`），所以"加/减节点"在协议层已经会自己反映——前提是成员表里的
   `Endpoint` 是设备真的能连上的地址（今天靠签发加入码时人工填，`cluster.go:844`）。
4. **RR 是钉死的**：`mesh` 只持有一个 `m.addr`，失败就退避重试同一个地址
   （`cmd/sr-rr/mesh.go:94-123`）；`/v1/rr/config` 也不返回控制器列表
   （`mesh.go:51-55`）。控制器一挂，RR 的网格就永久冻在最后一次拉到的状态。

## 3. 设计：两个地址，各管一件事

单地址方案在 HA 下必然二选一失败：全用 VIP 名，成员表的 `Endpoint` 三行一模一样（还会被
`endpointConflict` 拒绝）；全用节点 IP，换机/重建后所有设备的种子作废。所以要分开：

| | 节点地址 `-advertise` | 入口地址 `-entry` |
|---|---|---|
| 含义 | "**我**这台控制器对外是 host:port" | "**这个控制面**客户该用哪个名字找我" |
| 谁用 | 成员表 `Endpoint`、`tls_addrs` 的每一项、节点证书 SAN | `site.json`、`rr-identity.json`、控制台显示的命令、证书 SAN |
| 缺省 | `-tls-san` 首项 + `-listen` 端口 | 等于 `-advertise`（单机时两者本就该相同） |
| 存哪 | 成员表（每节点一行） | 集群共识（`Cluster.Entry`，全集群一份） |
| 变更多久生效 | 加入/下线时 | 改一次，所有人下一拍/下一轮跟随 |

`-entry` 的规则：

- bootstrap 时写入 `Cluster.Entry`；之后加入的节点必须给同一个值，**不一致就拒绝加入**，
  错误信息把两边值都打出来（`-entry 甲，集群入口是乙`）。
- 只有 leader 能改（走共识），控制台提供一个"改入口地址"的动作；改完提示**所有控制器
  需重启重签证书**（见 §4.6）。
- 单机集群允许之后补设：初始没给、后来要上域名，就在控制台补一次，不用重建集群。

### 3.1 CPE：站点种子 + 激活码（激活码机制不变）

现场只多**一个站点级文件**，每台设备仍然只敲一条带激活码的命令。

```json
// /etc/linkpilot/site.json（一个客户现场一份，0600）
{
  "ctrl": "ctrl.acme.example.com:8444",
  "ca_pem": "-----BEGIN CERTIFICATE-----\n...",
  "ctrl_fallback": ["10.0.0.11:8444", "10.0.0.12:8444"]
}
```

- `sr-agent` 取值顺序：`--ctrl` / `--ca` 显式参数 > `site.json`（`--site`，默认
  `/etc/linkpilot/site.json`）> 报错，错误信息直说"没找到站点种子，也没有 --ctrl"。
- `ctrl` 必须是**入口名**，`ctrl_fallback` 是可选的"手工 VIP"（§4.5 讲为什么需要它）。
- 每台设备的命令回到一句话：
  ```
  sr-agent --claim 7K2M-9QX4-3FDT-8WZP
  ```
- 老旗标 `--ctrl/--ca` 保留，脚本化部署不受影响。

不做"激活码自带地址"：码会从 19 个字符变成 1.5 KB 的 blob，现场手抄不可能，最终还是走
文件，却多了一个只能用一次的载体。地址是**站点属性**、激活码是**设备属性**，分开才对。

### 3.2 RR：自描述 identity，一个文件一条命令

`rr-identity.json`（控制台已有的"签发身份"下载）增加字段，由控制器签发时写入：

```json
{
  "rr_id": "rr1", "tenant": "vm-tenant",
  "addr": "192.168.1.100:18446",
  "controller_addr": "ctrl.acme.example.com:8444",
  "cert_pem": "...", "key_pem": "...", "ca_pem": "...",
  "listen": "0.0.0.0:18446", "status_addr": "127.0.0.1:18450"
}
```

`sr-rr --identity rr-identity.json`：PEM 在内存里组成 `tls.Certificate`，不再要三个磁盘
路径 + 手写 JSON；`listen` 缺省由 `addr` 端口推；`controller_addr` 取的是**入口名**（不是
某个节点）。老的 `-config` 路径完整保留（现网 .100 那两份就是它）。

### 3.3 顺带修掉单机地址表缺陷

单机（无集群元数据）时 `tlsAddrs` 初始化成 `[]string{*listen}`（`main.go:2568`），即
`https://0.0.0.0:8444` 写进心跳应答，agent 的 `addrBook.learn` 会把它当候选合并
（`addrs.go:63-66`）。首拍连的是种子，所以现在没炸；一旦种子连不上，它就去试 `0.0.0.0`
——在 Linux 上等于连自己。修法：单机也用 `-advertise`，永不发通配地址。

## 4. HA：加控制器、减控制器、控制器宕机

前提：集群建议 **3 个 voter**（2 个没有容错，缩到 2 只允许作为过渡，控制台要提示）。

### 4.1 三个场景各要满足什么

| 场景 | 设备（CPE） | RR | 人 |
|---|---|---|---|
| **加**一台 | 下一拍心跳看到 `tls_addrs` 多一项，自动成为候选（≤1 个心跳周期） | 下一轮 poll 拿到新的控制器列表，网格不中断 | 不改任何设备配置 |
| **减**一台 | 下一拍看到少一项，**不再尝试它**（≤1 个周期） | 同上 | 被下线节点自退（已有机制） |
| **宕机**（含 leader） | 下一拍失败 → 换下一个候选（follower 直接服务心跳，不 307）；写操作 307 到新 leader | 换下一个控制器继续拉网格；拉不到就保持上一份网格（会话不中断） | 无操作 |

### 4.2 加控制器（今天缺两块）

流程：新机器起 `sr-ctrl -entry <同一个入口> -advertise <本节点地址>` → 控制台签发加入码
（**地址栏由 `-advertise` 自动预填**，不再手打）→ 加入 → learner 追日志 → 提升 voter。

两块必须补，否则"加进去但设备连不上它"：

- **G-E 成员表 Endpoint 要来自加入方的 `-advertise`**，不是签发人凭记忆敲的字符串。今天
  `cluster.go:844` 强制要求填，填错就是"成员表说 A、设备连 A 连不上"。
- **G-C 节点证书 SAN 要含入口名**。今天 join 签发只放 `endpoint + console + localhost`
  （`cluster.go:544-547`）。设备经 VIP/DNS 连到这台新节点时 `ServerName` 是入口名，证书里
  没有它 → 握手失败。SAN 要改成 `entry + advertise + localhost`。

### 4.3 减控制器

- 控制台"下线"→ 走共识把成员标 `decommissioned` → `activeEndpoints()` 立刻少一条
  （`cluster.go:122-130`）→ 设备下一拍、RR 下一轮丢掉它。
- **前提是设备侧真的会丢**：今天 `addrBook.learn` 用 `merged := []string{b.cur}` 重建列表
  （`addrs.go:53`），被下线的那台如果恰好是 `cur`，它会一直留着——每次故障切换都先撞一次
  死地址。修法见 G-A。
- 被下线节点自己退出：已有（raft 非 voter 自退），日志会写"本机不在 voter 集合里"。

### 4.4 控制器宕机 / leader 挂

- 设备：请求失败 → `addrBook.next()` 换下一个候选，**不退避**（`addrs.go:77-91` 的设计
  就是为此）；新 leader 选出后，心跳应答里的 `leader` 让设备直接切过去。
- 写操作：follower 对 `/v1/enroll`、`/api/*` 返 307 到 leader；`agent.Enroll` 用默认
  redirect 策略（307 带 body 重放），入网也能被带到新 leader。
- **RR 不会自动换**（G-B）：`mesh.addr` 是常量。控制器挂掉后 RR 只是"网格冻住"，既有会话
  不受影响，但新增/删除 RR、改 `addr` 都不再生效，日志每轮报同一个错。必须补轮换。

### 4.5 首次启动的种子语义（这条决定 HA 有没有意义）

`agent.Enroll` **只认一个 base，没有 failover**（`internal/agent/client.go:119-135`），失败
即 `log.Fatalf` 退出；systemd 重启后还是同一个种子。所以：

> **种子必须是入口名（VIP / DNS），不能是某一台节点的 IP。**

否则那台节点宕机时，任何新设备都永远入不了网——而且现场看不出原因（日志只有 connection
refused）。没有 VIP/DNS 的客户，用 `site.json` 的 `ctrl_fallback` 写 1..N 个地址，等价于
"手工做的 VIP"，代价是换机要改种子；文档必须把这个代价写在明面上。

### 4.6 证书：入口名变了就得重签

`loadOrCreateServerCert` 只在 `server.pem` 缺失时签发（`main.go:1025`）。入口名从 IP 改成
域名、或加一个 SAN，现有证书不会变，设备用新名字连就握手失败。修法（G-D）：把"期望的 SAN
集合"和现有证书比对，不一致就重签（密钥不变、不换 CA，设备侧无感）。控制台证书同理。

### 4.7 代码缺口清单（按"会不会让 HA 假成立"排序）

| 编号 | 缺口 | 症状 | 位置 |
|---|---|---|---|
| **G-B** | RR 钉死一个控制器，`/v1/rr/config` 不带控制器列表 | 控制器挂掉后网格永久冻住 | `cmd/sr-rr/mesh.go:94-123`、`mesh.go:51-55` |
| **G-A** | `addrBook.learn` 重建列表时只保留 `cur`，**种子会丢** | 设备切到某个节点后，入口名从候选表消失；入口/集群换地址后设备回不去（除非重启） | `cmd/sr-agent/addrs.go:50-75` |
| **G-C** | join 证书 SAN 不含入口名 | 加进来的节点，设备经 VIP 连它握手失败 | `cmd/sr-ctrl/cluster.go:544-547` |
| **G-D** | 证书只在缺失时签 | 改入口名后设备连不上，且没有任何提示 | `cmd/sr-ctrl/main.go:1025` |
| **G-E** | 成员表 Endpoint 靠手填 | 填错 = 成员表与可达地址不一致，`tls_addrs` 里出现连不上的地址 | `cmd/sr-ctrl/cluster.go:844` |
| **G-F** | enroll 无 failover | 种子节点宕机时新设备永远入不了网 | `internal/agent/client.go:119-135` |

G-A 和 G-B 是"HA 名义成立、实际不成立"的那两条：读路径看着能扛，实际上设备回不到入口、
RR 停在旧网格。这两条必须先修。

## 5. 为什么不做别的

| 不做 | 原因 |
|---|---|
| 让每个节点都 advertise 同一个 VIP 名 | 成员表三行 `Endpoint` 相同，`endpointConflict` 直接拒绝；设备候选集退化成一条，故障切换全靠 VIP |
| 只用节点 IP、不要入口名 | 换机/重建后所有设备的种子作废，等于今天的 .100 迁移再来一遍 |
| 地址写进设备证书 | 换地址就要重签设备证书，把 DNS 问题升级成 PKI 问题 |
| mDNS / 广播发现 | 客户现场几乎一定是三层 |
| 激活码承担地址分发 | 码变成 KB 级 blob，一次性，不如站点种子 |
| RR 手填 `controller.addr` | 就是今天"装好但不工作"的来源 |
| 两级种子（门厅一套、控制器一套） | 运维要记两个名字，出错时无法判断改哪个 |

## 6. 改动清单

| # | 改动 | 位置 | 规模 | 风险 |
|---|---|---|---|---|
| 1 | `-advertise`（节点地址）+ 单机 `tls_addrs` 用它 + `learn` 过滤通配 | `sr-ctrl/main.go`、`sr-agent/addrs.go` | ~50 行 | 低 |
| 2 | **G-A** 种子常驻：`addrBook` 单独保存 seed，`learn` 永远把它排在候选里 | `sr-agent/addrs.go` | ~25 行 + 单测 | 低 |
| 3 | **G-B** `/v1/rr/config` 增 `controllers[]`；`mesh` 持列表 + 失败轮换 | `rr_api.go`、`sr-rr/mesh.go` | ~60 行 + 单测 | 低 |
| 4 | `Cluster.Entry`：bootstrap 写入、join 校验、控制台可改（走共识） | `cluster.go`、`cluster_runtime.go` | ~70 行 | 中（多一条共识字段与校验） |
| 5 | **G-C** join 证书 SAN = entry + advertise + localhost | `cluster.go:544` | ~10 行 | 低 |
| 6 | **G-D** SAN 比对不一致即重签（入口名变更兜底） | `main.go`、`console_tls.go` | ~40 行 | 低 |
| 7 | **G-E** 加入码地址栏由 `-advertise` 预填 | `console.html`、`cluster.go` | ~40 行 | 低 |
| 8 | `site.json` + `--site`（含 `ctrl_fallback`） | `sr-agent/main.go` | ~60 行 + 单测 | 低 |
| 9 | `rr-identity.json` 增 `controller_addr`/`listen`/`status_addr`；`sr-rr --identity` | `rr_api.go`、`sr-rr/main.go` | ~100 行 + 单测 | 低（`-config` 保留） |
| 10 | 控制台：入网卡片显示真机命令 + 下载站点种子 | `console.html` | ~80 行 | 低 |

1–7 是 HA 正确性（约 300 行），8–10 是易用性（约 240 行）。没有数据迁移，不改入网协议。

## 7. 与打包的关系

种子契约定了，打包只剩"把文件放到对的位置"：

- **deb/rpm**：`/etc/linkpilot/site.json`（0600）、`/etc/linkpilot/rr-identity.json`；
  单元 `ExecStart=/usr/bin/sr-agent --site /etc/linkpilot/site.json`。postinstall 只校验
  文件存在、CA 能解析，**不做任何网络行为**。
- **image**：`LINKPILOT_SITE` 环境变量或挂载 `/run/secrets/site.json`。
- **一体机**：种子预烧 → 现场零配置。

## 8. 验收判据

单机部分：

1. 全新 CPE：装包（含 `site.json`）→ `sr-agent --claim <码>` → 控制台出现设备、证书签发、
   心跳稳定。全程没有人敲过 IP。
2. 全新 RR：放一个 `rr-identity.json` → `sr-rr --identity …` → 一个 poll 周期内进入网格。
3. 现网 .100 的两份 `rr-a/rr-b.json` 继续走 `-config` 能起（不强制迁移）。

HA 部分（3 节点 lab，`-entry` 用域名，DNS 指向 VIP）：

4. **杀 leader**：设备心跳在 ≤2 个心跳周期内恢复；RR 在 ≤1 poll + 一次退避内继续更新网格；
   期间既有隧道不中断。
5. **加第 4 台**：控制台签发加入码（地址自动预填）→ 起进程 → 设备 `tls_addrs` 在一个周期
   内变成 4 项，RR 的控制器列表同步；设备侧零改动。
6. **下线第 4 台**：设备/RR 在一个周期内不再出现该地址；被下线进程自退，日志说明原因。
7. **入口名变更**：控制台把 `-entry` 从 IP 改成域名 → 各节点重启后证书 SAN 自动重签 →
   设备**不重启**、只改 DNS 就回到入口。
8. **种子节点宕机 + 新设备入网**：杀掉入口指向的那台，起一台全新 CPE，仍能入网（证明种子
   是入口而不是某台节点）。
9. **单测**：`addrBook` 在 seed→member→seed 往返中种子不丢；`mesh` 第一个地址超时后切到
   第二个并记一次日志；join 校验拒绝 entry 不一致的加入请求。

## 9. 待 owner 拍板

1. **客户现场能否保证一条内网 DNS 记录**（`ctrl.<客户域>`）？能 → `-entry` 用域名，换机
   只改 DNS；不能 → 允许 IP，`site.json` 用 `ctrl_fallback` 写多地址，文档写明换机代价。
2. **HA 默认规模**：3 台（2 台没有容错）。2 台形态是否允许作为过渡？
3. **控制面要不要在 NAT 后**（即是否需要独立的 vBond 门厅）？需要才做"门厅 + 名册"；
   不需要就 `-advertise` + `-entry` 直连，本文足够。
4. `site.json` 默认路径 `/etc/linkpilot/`（产品名）还是 `/etc/sdwan/`（现有 `route.json`
   / `fib.json` 所在）？统一成一个。
