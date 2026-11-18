# 控制面发现设计：一个种子，其余自发现（2026-11-18）

> 背景：交付形态讨论（deb/rpm vs image）真正难的不是"装得上"，是**装完以后 RR、vBond、
> CPE 各自怎么找到控制器**。本文先把契约定下来，打包只是契约的下游。

## 0. 结论（三句话）

1. 整个系统**只能有一个手工配置项**：控制面的**种子**（一个主机名 + 一张 CA）。
2. 种子之后的全部地址（leader、地址表、RR 网格、STUN 反射器）都由控制器**下发**，
   今天代码里已经实现了，不需要新机制。
3. 现在缺的是两件小事和一条纪律：RR 的身份文件里没有控制器地址（要两份手工输入）、
   CPE 要三份手工输入（ctrl / claim / CA）、交付里到处是 IP 而不是稳定名字。

## 1. 现状盘点（代码事实，不是设想）

| 角色 | 首接触要什么 | 之后怎么发现 | 代码位置 |
|---|---|---|---|
| CPE `sr-agent` | `--ctrl host:port`（必填）、`--claim`（首次）、`--ca`（首次信任锚） | 心跳应答每次带 `TLSAddrs` + `Leader`，agent 自己轮换 | `cmd/sr-agent/main.go:436-481`、`internal/agent/client.go:52-56` |
| RR `sr-rr` | `controller.addr`（https://host:port，必填才有网格）+ identity 三件套 | 每 `poll_sec`（默认 30）拉 `/v1/rr/config`，按行拨 mesh | `cmd/sr-rr/main.go:45-101` |
| 人 / 控制台 | 书签 URL | — | — |
| STUN | 不需要设备配置 | 控制器在心跳里指定反射器地址 | `-stun` 控制器侧 |

两个关键事实：

- **校验是真校验**：agent 和 RR 都用 `RootCAs + ServerName`，没有 `InsecureSkipVerify`
  （`internal/agent/client.go:140-147`、`internal/agent/identity.go:470-479`）。
  所以地址如果是主机名，证书 SAN 里必须有这个 DNS 名；`-tls-san` 已经支持 DNS 名
  （`signWithCA` 按 `net.ParseIP` 分流）。**用域名是可行的，不用改 TLS 代码。**
- **首接触之后已经是自发现**：地址表轮换和 RR 网格都是拉模式。系统不需要新的发现协议。

真正的缺口是三个：

| 缺口 | 症状 | 代价 |
|---|---|---|
| G1 RR 要两份手工输入 | identity 文件不带控制器地址，还得手写一份 config；写错一个字段就是"装好了但不工作" | 中小 |
| G2 CPE 要三份手工输入 | `--ctrl` + `--claim` + `--ca`，漏一个起不来；批量装机全靠脚本拼 | 中小 |
| G3 交付里全是 IP | 这次 .100 迁移实测：4 台设备的 `-ctrl`、2 份 RR config、成员表、证书 SAN 全要手工改 | 大（且不可持续） |

## 2. 目标契约

### 2.1 种子文件 `seed.json`

```json
{
  "ctrl": "https://ctrl.example.com:8444",
  "ca_pem": "-----BEGIN CERTIFICATE-----\n...",
  "claim": "XXXX-XXXX-XXXX-XXXX",
  "preauth": "cpe-shanghai-01"
}
```

- `ctrl`：**只写主机名**，交付文档禁止写 IP。端口固定 8444（可省）。
- `ca_pem`：首次信任锚。已入网设备以本地存的 CA 为准（`pinnedRoots` 的既有语义不改变）。
- `claim` / `preauth` 二选一，且都可省——设备已有身份时忽略。
- **同一个文件发给所有角色**：CPE 读 `ctrl`+`ca_pem`+认领字段；RR 只读 `ctrl`（身份走
  identity 文件）；vBond 只读 `ctrl`。

### 2.2 RR 身份文件自描述

`rr-identity.json` 增加 `controller_addr` 字段（签发时由控制器填入当前 `ctrl` 种子地址，
不是 RR 自己的地址），`sr-rr` 增加 `--identity <file>`：一次给定 cert/key/ca/controller，
不再需要手写 config（`listen`、`poll_sec` 可留默认）。

判据：**一个文件，一条命令，RR 起来就在网格里。**

### 2.3 名字而不是地址

- 证书 SAN 里写 `ctrl.example.com`（控制台 SAN 同理），DNS 指向 VIP / 任一节点 / 轮询。
- 集群成员表的 `endpoint` 也存名字（今天存的是 `192.168.1.4:8444`，迁移时没法自愈；
  这一条改动面较大，见 §4.4）。
- 换机 = 改 DNS 记录（或 VIP 漂移），不动任何一台设备的配置。

## 3. 分层交付（按"什么时候必须做"排序）

### L0 单一入口 + 单一信任锚（现在就该做，零代码）

- 每个客户现场只定义一个控制面主机名，DNS 指到 VIP 或任一节点。
- 证书 `-tls-san` / `-console-san` 必须包含这个名字。
- 所有交付物、安装脚本、文档里**不出现 IP**。
- 这一条就把"迁移要改 4 台设备"这类工作量从根上去掉。

### L1 自描述文件（0.9.1 建议做，工作量小）

1. `sr-rr --identity rr-identity.json`（+控制台签发时写 `controller_addr`）≈60 行 + 单测
2. `sr-agent --enroll seed.json`（三旗标保留兼容，内部合并到同一结构）≈40 行 + 单测
3. 控制台"下载入网文件"：签发 claim 时直接给一份 `seed.json`，不再让人拼参数

判据：CPE 只放一个文件、RR 只放一个文件，都能一次性起来。

### L2 vBond 独立成"门厅"（要卖托管 / 控制器不可公网直达时）

现在的 sr-ctrl 是 **vBond + vSmart + vManage 三合一**：设备直连 8444，入网和稳态控制走同
一个口。这在单客户 on-prem 成立，在两种场景不成立：

- 控制器放在客户 DC 私网 / NAT 后，设备在公网；
- 多租户托管：一个门厅服务很多客户的控制器。

形态（只做两件事，必须无状态）：

| 接口 | 作用 |
|---|---|
| `GET /v1/bootstrap` | 返回**名册**：控制器端点表 + CA + 租户路由，带 TTL 和签名（用 CA 或专属签名单） |
| `POST /v1/enroll` | 把 claim 请求转给控制器（或 307 过去），设备只认门厅一个名字 |

硬约束：

- **vBond 不持有 CA 私钥**，不存设备状态，可多副本 + DNS/anycast。
- **vBond 自己也只读同一个 `seed.json`**，不许出现"门厅的种子"和"控制器的种子"两级。
- 名册必须有 TTL，控制器换地址时靠名册过期自愈。

代价：一个新二进制 + 名册协议 + 签名轮换；不做也能卖单客户 on-prem（L0+L1 足够）。

### L3 零接触（ZTP，按客户要求再做）

- 出厂烧 `seed.json`（含 preauth 名）→ 上电即入网，最省事、可审计。
- 客户有企业 DNS 时可选 SRV：`_linkpilot-ctrl._tcp.<domain>`（≈30 行，`net.LookupSRV`）。
- DHCP option 43 类方案留给 OEM 客户，不主动做。

## 4. 改动清单（按优先级，含规模）

| # | 改动 | 规模 | 风险 |
|---|---|---|---|
| 1 | `rr-identity.json` + `controller_addr`；`sr-rr --identity` | ~60 行 | 低（新入参，旧 config 保留） |
| 2 | `sr-agent --enroll <seed.json>`；控制台签发 seed 文件 | ~40 行 | 低（旧旗标保留） |
| 3 | 交付文档/脚本去 IP 化（含 systemd 单元样例） | 文档 | 无 |
| 4 | 成员表 `endpoint` 存名字 + `TLSAddrs` 发名字 | ~150 行 | 中（校验、替换流程都要动，建议 1.0） |
| 5 | SRV 查询（可选） | ~30 行 | 低 |

## 5. 与打包/镜像的关系

种子契约定了以后，打包就只剩"把文件放到对的位置"：

- **deb/rpm**：`/etc/linkpilot/seed.json`（0600）+ `/etc/linkpilot/rr-identity.json`；
  systemd 单元 `ExecStart=... --enroll /etc/linkpilot/seed.json`。postinstall 只校验文件
  存在、CA 能解析，**不做任何"发现"**（安装器不该有网络行为）。
- **image**：`LINKPILOT_SEED` 环境变量或挂载 `/run/secrets/seed.json`，entrypoint 渲染。
- **一体机（预装镜像）**：种子已经烧好 → 交付现场零配置，这是最简单的商业形态。

## 6. 不做的事（以及为什么）

| 不做 | 原因 |
|---|---|
| 把控制器地址写进设备证书 | 地址一变证书作废，等于把 DNS 的问题变成 PKI 的问题 |
| mDNS / 广播发现 | 跨三层不成立，客户现场几乎一定是三层 |
| 设备侧维护"控制器候选列表" | 心跳应答已经带地址表，设备侧再维护一份就是两个真相 |
| 两级种子（门厅有门厅的种子） | 运维要记两个名字，出错时无法判断该改哪个 |
| 让 RR 手工填 `controller.addr` | 就是今天的 G1，纯属让人犯错 |

## 7. 待 owner 拍板

1. **客户现场能不能保证一条内网 DNS 记录？** 能 → DNS-first（本文默认）；不能 → 种子允许
   IP，但文档要写明"换 IP 必须逐台改"的代价。
2. **vBond 是否作为独立可售组件？** 是 → L2 进 1.0 路线图；否 → 保持三合一，只用 DNS。
3. **CPE 出厂方式**：预授权名（`preauth`）还是 claim code？前者适合批量 ZTP，后者适合
   人工发货。

## 8. 验收判据（做完 L0+L1 之后）

- 一台全新 CPE：只放 `seed.json` → `systemctl start sr-agent` → 控制台出现设备、证书签发、
  心跳稳定（不需要任何 `--ctrl/--claim/--ca` 手工拼接）。
- 一台全新 RR：只放 `rr-identity.json` → 起来即在网格里（peer list 正常）。
- **换 IP 试验**：控制器换地址，只改 DNS 记录 → 设备下一拍心跳自动切（地址表轮换），
  RR 下一轮 grid 拉取自动换，全程不改任何设备配置。
