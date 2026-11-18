# 控制面发现：CPE 与 RR 各自怎么找到控制器（2026-11-18）

> 背景：交付形态讨论（deb/rpm vs image）真正难的不是"装得上"，是**装完以后 CPE 和 RR
> 各自从哪儿知道"控制器在哪"**。本文只回答这两个问题，不重构入网协议。

## 0. 先更正术语（这是我上一版写错的地方）

上一版说"sr-ctrl 是 vBond + vSmart + vManage 三合一"，**错了**：vSmart 是 RR，它一直是
独立进程 `sr-rr`，跟控制器分开部署、分开签发身份、分开升级。

| 角色 | 二进制 | 业界对应 | 今天的形态 |
|---|---|---|---|
| 控制器 | `sr-ctrl` | **vBond（入网 / NAT 观测）+ vManage（控制台 / 编排 / 配置下发）** | 一个进程，两个角色合一 |
| 路由反射器 | `sr-rr` | **vSmart** | 独立进程，可多副本，靠控制器下发网格 |
| CPE | `sr-agent` + VPP | vEdge / cEdge | 盒子内的 agent |

所以"vBond 和控制器在一起"不影响任何设计结论——它本来就是一个二进制里的两个职责。
真正要解决的只有两件事：**CPE 首接触连谁**、**RR 首接触连谁**。

## 1. 一句话根因

控制器**没有"我叫什么"这个概念**。`-listen 0.0.0.0:8444` 是绑定地址，不是可达地址；
于是所有下游产物都靠人手填 IP：CPE 的 `--ctrl`、RR 配置的 `controller.addr`、RR 行里的
`addr`、心跳应答里的 `TLSAddrs`（单机时就是 `0.0.0.0:8444`，见 §3.4）。这次 .100 迁移
要手工改 4 台设备 + 2 份 config + 成员表，根子都在这里。

## 2. 现状盘点（代码事实）

| 角色 | 首接触要什么 | 之后怎么发现 | 证据 |
|---|---|---|---|
| CPE `sr-agent` | `--ctrl host:port`（必填）、`--claim <激活码>`（首次）、`--ca <CA 文件>` | 心跳应答每次带 `TLSAddrs` + `Leader`，agent 自己轮换、换主立即切 | `cmd/sr-agent/main.go:146-160`、`cmd/sr-agent/addrs.go:50-75` |
| RR `sr-rr` | **四样**：`-config` 指向的 JSON 里要 `listen`、`status_addr`、`tls.cert_file/key_file/ca_file`（三个磁盘路径），再加 `controller.addr` | 每 `poll_sec`（默认 30）拉 `/v1/rr/config`，按返回的行拨网格 | `cmd/sr-rr/main.go:45-104` |
| 控制台 | 浏览器书签 | — | — |
| STUN | 设备侧不配 | 控制器在心跳/入网应答里给反射器地址 | `cmd/sr-ctrl/stun.go` |

三个事实：

1. **入网机制本身没问题**：控制台生成一次性激活码（默认 5 分钟有效，只存哈希），
   设备带码 redeem，控制器发证书 + node_id。这条**继续沿用，不动协议**。
2. **首接触之后已经是自发现**：地址表轮换、RR 网格都是拉模式，不需要发明新的发现协议。
3. **RR 的缺口比"少一个字段"更大**：identity 下载给的是 PEM 字符串，`sr-rr` 要的是
   三个磁盘路径 + 一份手写 JSON。也就是"装好了但不工作"的经典形态。

## 3. 设计：一个 `-advertise`，三处落地

### 3.1 新增单一事实：控制器宣告地址

`sr-ctrl` 增加 `-advertise host:port`，含义是**"别人该用这个地址找我"**（区别于 `-listen`
的"我绑在哪"）。取值顺序：

1. `-advertise` 显式给出，直接用；
2. 否则取 `-tls-san` 的第一项 + `-listen` 的端口（SAN 里本来就必须有设备用的那个名字，
   等于同一个事实只声明一次）；
3. 都没有（`-listen 0.0.0.0` 且无 SAN）→ 启动日志显式告警，并退化为"第一个非 loopback
   网卡地址"，日志写明这是猜的。

这一条同时消掉三处手工输入，因为下面的产物都从这里取值：

| 产物 | 今天 | 之后 |
|---|---|---|
| 心跳应答 `TLSAddrs`（单机） | `0.0.0.0:8444`（错，见 §3.4） | `-advertise` |
| `rr-identity.json` | 没有控制器地址 | `controller_addr` = `-advertise` |
| 控制台"入网授权"页 | 显示的是一条 **lab 脚本**命令 `test/lab/vcpe.sh up --claim …` | 显示真机命令，地址来自 `-advertise` |

集群模式下 `TLSAddrs` 仍取 `activeEndpoints()`（每台节点自己的 endpoint），`-advertise`
只在单机和无集群元数据时兜底——两者语义不冲突：宣告地址是"入口"，成员表是"候选"。

### 3.2 CPE：站点种子 + 激活码（激活码机制不变）

现场只多**一个站点级文件**，每台设备仍然只敲一条带激活码的命令。

```json
// /etc/linkpilot/site.json（一个客户现场一份，0600）
{
  "ctrl": "ctrl.acme.example.com:8444",
  "ca_pem": "-----BEGIN CERTIFICATE-----\n..."
}
```

- `sr-agent` 取值顺序：`--ctrl` / `--ca` 显式参数 > `site.json`（`--site`，默认
  `/etc/linkpilot/site.json`）> 报错，错误信息里直说"没找到站点种子，也没有 --ctrl"。
- 于是每台设备的命令回到一句话，和今天的心智模型完全一致：
  ```
  sr-agent --claim 7K2M-9QX4-3FDT-8WZP
  ```
- `site.json` 从控制台"入网授权"页下载（和激活码同一张卡片），不是手抄。
- **入网协议零改动**：激活码仍是唯一的授权凭据；`site.json` 只解决"连谁、信谁"。
- 老旗标 `--ctrl/--ca` 保留，脚本化部署不受影响。

为什么不做"激活码自带地址"（把地址+CA 编进一个长 token）：码会从 19 个字符变成 1.5 KB
的 blob，现场手抄不可能、必须走文件传，那就跟 `site.json` 一样是文件，却多了一个只能
用一次的载体。地址是**站点属性**、激活码是**设备属性**，两者分开才对。

### 3.3 RR：自描述 identity，一个文件一条命令

`rr-identity.json`（控制台已有的"签发身份"下载）增加三个字段，由控制器在签发时写入：

```json
{
  "rr_id": "rr1",
  "tenant": "vm-tenant",
  "addr": "192.168.1.100:18446",
  "controller_addr": "ctrl.acme.example.com:8444",
  "cert_pem": "...", "key_pem": "...", "ca_pem": "...",
  "listen": "0.0.0.0:18446",
  "status_addr": "127.0.0.1:18450"
}
```

`sr-rr --identity rr-identity.json`：

- PEM 直接在内存里组成 `tls.Certificate`，**不再落盘三个文件、不再要三个路径**；
- `listen` 缺省由 `addr` 的端口推（`0.0.0.0:<port>`），`status_addr` 缺省 `127.0.0.1:<port+4>`；
- `controller_addr` 缺省时回落到 `--ctrl` 旗标，再缺就只收会话、不进网格（并在日志里
  明说"没有控制器地址，网格不会建立"）；
- 老的 `-config` 路径完整保留（现网 .100 那两份 rr-a/rr-b.json 就是它）。

判据：**一个文件 + `sr-rr --identity`，起来即入网**，不再有"填错字段导致装好但不工作"。

### 3.4 顺带修掉单机地址表缺陷

单机（无集群元数据）时 `tlsAddrs` 初始化成 `[]string{*listen}`（`main.go:2568`），也就是
`https://0.0.0.0:8444` 被写进心跳应答；agent 的 `addrBook.learn` 会把它合并进候选表
（`addrs.go:63-66`）。首拍连的是种子地址，所以现在没炸；但**一旦种子地址连不上，它会去试
`0.0.0.0`**——在 Linux 上等于连本机，故障现场会表现为"控制器明明活着，设备却连自己"。

修法就是 §3.1 的第二条：单机时 `tlsAddrs` 也用 `-advertise`，永不发通配地址；顺手在
`learn` 里过滤 `0.0.0.0` / `::`（防御性，防止将来别处再漏）。

## 4. 为什么不做别的

| 不做 | 原因 |
|---|---|
| 地址写进设备证书 | 换地址就得重签证书，等于把 DNS 问题升级成 PKI 问题 |
| mDNS / 广播发现 | 客户现场几乎一定是三层，跨不过去 |
| 让激活码承担地址分发 | 码会变成 KB 级 blob，且一次性，不如站点种子 |
| 设备侧自己维护候选列表 | 心跳应答已经带地址表，两份就是两个真相 |
| RR 手填 `controller.addr` | 就是今天"装好但不工作"的来源 |
| 强制 DNS | 小客户可能只有 IP。允许 IP，但文档写明"换 IP 要逐台改"的代价 |

## 5. 改动清单（按优先级）

| # | 改动 | 位置 | 规模 | 风险 |
|---|---|---|---|---|
| 1 | `-advertise` + 单机 `TLSAddrs` 用它 + `learn` 过滤通配 | `cmd/sr-ctrl/main.go`、`cmd/sr-agent/addrs.go` | ~40 行 | 低 |
| 2 | `site.json` + `--site`，`--ctrl/--ca` 回落 | `cmd/sr-agent/main.go` | ~50 行 + 单测 | 低 |
| 3 | `rr-identity.json` 增 `controller_addr`/`listen`/`status_addr` | `cmd/sr-ctrl/rr_api.go` | ~25 行 | 低（多三个字段） |
| 4 | `sr-rr --identity`（内存 TLS + 缺省值） | `cmd/sr-rr/main.go` | ~80 行 + 单测 | 低（`-config` 保留） |
| 5 | 控制台：入网卡片显示真机命令 + "下载站点种子"；RR 卡片提示一个文件启动 | `cmd/sr-ctrl/console.html` | ~60 行 | 低 |
| 6 | 单元文件样例、迁移/安装文档去 IP 化 | `docs/`、`packaging/` | 文档 | 无 |

1–5 合计约 250 行，含单测。不需要数据迁移、不需要改入网协议、不需要换设备侧其它件。

## 6. 与打包的关系

种子契约定了，打包只剩"把文件放到对的位置"：

- **deb/rpm**：`/etc/linkpilot/site.json`（0600）、`/etc/linkpilot/rr-identity.json`；
  单元 `ExecStart=/usr/bin/sr-agent --site /etc/linkpilot/site.json`。postinstall 只校验
  文件存在、CA 能解析，**不做任何网络行为**（安装器有网络行为是运维事故的常见来源）。
- **image**：`LINKPILOT_SITE` 环境变量或挂载 `/run/secrets/site.json`。
- **一体机**：种子预烧 → 现场零配置。

## 7. 验收判据

1. **全新 CPE**：装包（含 `site.json`）→ `sr-agent --claim <码>` → 控制台出现设备、证书
   签发、心跳稳定。全程没有人敲过 IP。
2. **全新 RR**：放一个 `rr-identity.json` → `sr-rr --identity …` → 一个 poll 周期内出现在
   网格（peer list 正常），日志无 TLS 报错。
3. **换地址试验**：控制器从 A 机搬到 B 机，只改 DNS（`-advertise` 用域名）→ CPE 下一拍
   自动切（地址表）、RR 下一轮 poll 自动切，设备侧零改动；`-advertise` 若是 IP，则要改
   每台设备的站点种子——这个代价文档必须写明。
4. **回归**：现网 .100 的两份 `rr-a/rr-b.json` 用 `-config` 继续能起（不强制迁移）。

## 8. 待 owner 拍板

1. **客户现场能否保证一条内网 DNS 记录**（`ctrl.<客户域>`）？能 → 推荐 `-advertise` 用
   域名，换机只改 DNS；不能 → 允许 IP，文档写明逐台改的代价。
2. **控制面要不要在 NAT 后**（即是否需要独立的 vBond 门厅）？需要才做"门厅 + 名册"那一层；
   不需要就 `-advertise` 直连，本文足够。
3. `site.json` 的默认路径用 `/etc/linkpilot/`（产品名）还是 `/etc/sdwan/`（现有目录惯例，
   `route.json`/`fib.json` 都在那儿）？统一成一个，别两套。
