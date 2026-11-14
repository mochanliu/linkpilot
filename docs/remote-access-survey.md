# 远程用户接入：现成方案调研与落点建议

2026-11-14。本文只做调研与方案对比，不含实现。结论用到的外部事实都标了出处（§7）。

## 0. 一句话结论

**没有可以直接装进 CPE 的现成方案。** 最接近的两种形态——Cisco SD-WAN Remote Access 和
FortiGate SSL VPN——和我们想要的模型几乎一模一样（角色/组 → 地址池 → 一个虚拟接口 →
放进 service VPN/VRF → 路由协议把网段发布出去），但都是封闭设备，拿不到代码，也装不进
VPP 数据面。开源侧要么只有"池 + 接口"没有"角色→段"（wg-access-server），要么有 app 和
角色但没有"池接口"（NetBird/Tailscale/Firezone），要么有池有角色但数据面在内核
（strongSwan/ocserv/OpenVPN），与我们"内核只做控制面、转发全在 VPP"的定案冲突。

**建议：用现有 VPP WireGuard + 控制器自己做接入面。** 需要新写的只有"角色→池"的对象模型、
IPAM/租约、peer 下发和客户端配置分发；"接口进 VRF、FRR 看得见、overlay 发布"这三步今天
已经具备，不需要新机制。

---

## 1. 把需求拆成六条硬约束

| # | 约束 | 现状 |
|---|---|---|
| 1 | 终端装 app，主动连到**选定的 CPE**（不是连中心网关） | 无 |
| 2 | 客户按 **role** 给终端分配**指定段**的 IP | 无 role/池概念 |
| 3 | 这个段在 CPE 上是**一个接口**（有网关地址、有前缀） | 基础配置已有"接口 + VPN"模型 |
| 4 | 接口能放进 **VRF**（VPN N） | 已实现：`vpnN` + 内核 table N |
| 5 | **FRR 能看见**（connected 路由） | 已实现：地址写在 LCP 上，zebra 下发 |
| 6 | overlay 能**发布这个网段** | 已实现：leak 的 `connected` 来源 + 前缀白名单 |

后四条是我们已经有的，所以真正要解决的是 1、2，以及"一个接口上挂很多 peer"的数据面形态。

---

## 2. 现成方案逐个对照

### 2.1 商用 SD-WAN 的同类功能（模型验证，不可复用）

| 产品 | 做法 | 与本需求的关系 |
|---|---|---|
| **Cisco Catalyst SD-WAN RA** | AnyConnect 走 IKEv2 到任意 edge；RADIUS/ISE 认证并按组授权；用户被放进某个 **service VPN**；headend 为每个用户加一条 **/32 主机路由**，由 **OMP 自动发布**到其它站点 | 模型完全一致，连"每用户 /32 而不是整段"这个细节都一致。封闭、按用户收费、必须 Cisco edge + Cisco 客户端 |
| **FortiGate SSL VPN** | `ssl.root` 虚拟接口 + 每个 portal 一个 **IP 池**；用户组 → portal 决定拿哪个池；防火墙策略按组放行；可把 `ssl.root` 放进 VRF | "组→池→接口"的教科书形态。封闭设备；FortiOS 7.6.3 起 SSL VPN 隧道模式被移除，官方指向 IPsec |
| **Palo Alto GlobalProtect** | 每个 gateway 一个地址池，tunnel 接口属于 virtual router（≈VRF），按组选池 | 同上，封闭 |

这一组证明：**"角色→段→接口→VRF→路由发布"是成熟商用形态**，不是我们在发明机制。它们
解决"每用户 /32 怎么发布"的办法也值得抄：**发布 /32，不发布整段**（§4.2）。

### 2.2 开源 VPN 服务端（有池、有角色、能进 VRF，但数据面在内核）

| 方案 | 池/角色能力 | VRF | 数据面 | 结论 |
|---|---|---|---|---|
| **strongSwan (IKEv2)** | `rightsourceip` 池；RADIUS `Framed-IP-Address` 做每用户固定 IP、`Class`→`rightgroups` 做组策略；原生客户端（Win/macOS/iOS/Android 自带 IKEv2） | 官方没有一等 VRF 支持，需要 dummy 接口 + 策略路由，得实测 | 内核 xfrm；有 `kernel-vpp` 但成熟度与我们的 sr 插件不匹配 | 标准最好、客户端最省事；但"池"不是接口，要自己造一个 dummy 承载前缀；且流量走内核，和我们 `ip_forward=0` 的定案冲突 |
| **ocserv (AnyConnect 兼容)** | `ipv4-network` 全局池；**`config-per-group` 可给每个组一个 `ipv4-network`**——这是最接近"角色→段"的开源实现；per-user 静态 IP 也有 | tun 设备可以 `ip link set tun0 master vpnN`，但路由要 `route-add-cmd` 自己写进表 | 内核 tun + 用户态转发 | 角色→段最现成；性能与内核转发是硬伤，且需要 AnyConnect/OpenConnect 客户端 |
| **OpenVPN** | `server` 池 + `client-config-dir` 的 `ifconfig-push` 做每用户固定 IP；`--client-connect` 脚本可按角色算 IP | tun 可入 VRF，但 OpenVPN 默认往主表加路由，要 `--route-noexec` + 自己装 | 内核 tun + 用户态 | 同上，且客户端体验最重 |

**共同问题**：这三位都把"池"做成"服务端内存里的一堆租约 + 一条到池的汇总路由"，而不是
"CPE 上一个有前缀的接口"。要满足约束 3，得额外造 dummy 接口、把池前缀挂上去、再想办法让
它进 VRF。而且它们的解密/转发在内核，我们要么打开 `ip_forward`（破坏"内核只做控制面"），
要么在 VRF 和 VPP 之间再搭一对 veth/tap 做交接——多一条故障路径。

### 2.3 WireGuard 控制面产品（有 app、有角色，但没有"CPE 上的池接口"）

| 方案 | 做法 | 为什么不合适 |
|---|---|---|
| **wg-access-server**（Freifunk 维护，MIT） | 单二进制 = WG 服务端 + Web UI；一个 CIDR 池、每设备一个 IP；OIDC/GitLab 登录；可按 IdP 组做访问策略；支持 routed networks、内核 WG | 最接近"池在服务端接口上"：`wg0` 确实带池前缀。但**只有一个 CIDR，没有"角色→不同段"**；没有 VRF/FRR/路由发布概念；多租户要一个租户一套实例；它的策略是"谁能访问谁"，不是"谁拿哪个段" |
| **NetBird**（BSD-3，可自托管） | WG mesh + 管理面；每 peer 从 `100.64.0.0/10` 拿地址；组 + 策略；"network routes" 让某个 peer 当路由网关 | 池在**客户端**的 `wt0` 上，不在 CPE 上；CPE 只能当 routing peer 发布别人的段，自己不长池接口。控制面是它的，接不进我们的租户/VRF 模型 |
| **Tailscale / Headscale** | 同上；subnet router、exit node、tag/ACL；Headscale 可自托管控制面 | 地址固定 `100.64/10`，池不在网关；角色是 tag/ACL，不是"段" |
| **Firezone** | 零信任；客户端 + 自托管 Gateway；按 IdP 组给 Resource 授权；网关是 WG peer | 数据面 Apache-2.0、控制面 Elastic-2.0 且**生产自托管不受支持**；会话是"客户端↔网关"点对点，网关不长"角色池接口"；SaaS 控制面与私有化交付冲突 |
| **ZeroTier** | 虚拟 L2，成员地址由网络分配；managed routes | 没有"角色→段"的接口语义；L2 模型与我们的 L3/VRF 不匹配 |

这一组的共同点：**它们把"地址池"放在客户端或中心管理面，把 CPE 当"网关/路由 peer"**。
而我们要的恰恰相反——池长在 CPE 上，成为一个可进 VRF 的接口。这是产品定位差异，不是缺功能。

### 2.4 结论

| 约束 | Cisco RA | FortiGate | strongSwan | ocserv | wg-access-server | NetBird/Tailscale | 自建 VPP WG |
|---|---|---|---|---|---|---|---|
| 1 终端连选定 CPE | ✅ | ✅ | ✅ | ✅ | ✅（单点） | ⚠️（mesh，可指定网关） | ✅ |
| 2 角色→指定段 | ✅ | ✅ | ✅（RADIUS 组） | ✅（组配置） | ❌（单池） | ⚠️（策略非段） | ✅（要写） |
| 3 段=CPE 上的接口 | ✅ | ✅ | ❌（要 dummy） | ⚠️（tun 多子网） | ✅ | ❌ | ✅ |
| 4 接口进 VRF | ✅ | ✅ | ⚠️ | ⚠️ | ❌ | ❌ | ✅（已有） |
| 5 FRR 可见 | ✅ | ✅ | ⚠️ | ⚠️ | ❌ | ❌ | ✅（已有） |
| 6 overlay 发布 | ✅（OMP） | ✅ | ⚠️ | ⚠️ | ❌ | ⚠️ | ✅（已有 leak） |
| 可嵌入/私有化 | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |

**没有一个全绿。** 自建 VPP WG 是唯一能同时满足六条的路线，代价是 1、2 和 peer 管理要自己写。

---

## 3. 与现有架构的接法

### 3.1 对象模型

```text
Role（租户级）
  name           "employee" / "contractor" / "finance"
  vpn            107                     # 这个角色落在哪个 VPN（VRF）
  pool           10.200.107.0/24         # 每 CPE 一段（见 §4.2）
  dns / split    可选：下发给客户端
  policy         可选：允许访问哪些 VPN/前缀（后续接 leak/policy）

SiteBinding（每 CPE 一条）
  node_id
  role_id
  iface          ra-employee             # CPE 上的接口名
  gateway        10.200.107.1/24         # 接口地址 = 池的网关
  listen_port    51830                   # 每 CPE 一个 RA 端口（复用现有端口池规则）

Lease（每终端一条）
  user_id / device_id
  role_id, node_id
  ip             10.200.107.23/32
  pubkey         X25519
  state          active / expired / revoked
```

"段"的粒度是 **(CPE, role)**，不是全局 role。这样接口地址、connected 路由、overlay 发布
都是天然的 site-local，不需要跨站点协调同一个前缀。

### 3.2 数据面：一张 WG 接口 + 一个 tun 对 + 一张表

现有机制已经够用，不需要新协议：

```text
VPP:  wireguard 接口 ra-employee（listen 51830，私钥由 agent 生成）
       └─ peer: 每终端一把公钥，allowed-ips = 10.200.107.23/32，table 107
       └─ 接口地址 10.200.107.1/24（由内核 zebra 下发，lcp-sync 复制进 VPP）
       └─ lcp create ra-employee host-if ra-employee tun
内核: ra-employee（tun）→ ip link set ra-employee master vpn107
FRR:  interface ra-employee vrf vpn107 / ip address 10.200.107.1/24
      → connected 10.200.107.0/24 进 table 107
      → leak：来源 connected，前缀白名单含 10.200.107.0/24
overlay: 现有 route/RT 机制把该前缀发布到允许的站点
```

关键点：**"接口地址"这一步不用新写**。基础配置今天就是"地址写在 LCP 上，zebra 下发，
lcp-sync 同步进 VPP 同一张表"（`config_render.go` §3 的注释与 `ip link set ... master vpnN`）。
远程接入接口只要走同一个渲染路径，VRF/FRR/leak 三件事自动成立。VPP 的 WG 接口用
`lcp create ... tun` 建 L3 配对，社区实测可以让 FRR 在该接口上跑 OSPF，说明"WG 接口 ↔
内核 tun ↔ 路由协议"这条路是通的（§7）。

### 3.3 控制面：控制器多三件事

1. **IPAM/租约**：按 (node, role) 从池里分配 /32，持久化，终端重连尽量拿回同一个 IP；
   租约过期/撤销要能回收，并有一段隔离期防止"刚释放就被别人拿到"。
2. **peer 下发**：新增增量接口（不要塞进 bundle），内容 = pubkey、allowed-ip /32、table、
   sw_if_index、keepalive；删除同理。参考现有 underlay 的"状态 + 增量"而不是整包重发。
3. **客户端配置分发**：终端 app 认证后拿到 `{server endpoint, 分配给自己的 /32, 服务端公钥,
   DNS, split-tunnel 列表}`，私钥在终端生成、公钥上报（沿用现有"私钥不出设备"的定案）。

---

## 4. 四个必须提前定的设计岔路

### 4.1 "角色"从哪来

| 选项 | 含义 | 代价 |
|---|---|---|
| A. 控制器本地 role | 客户在控制台建 role、把用户/设备挂进去 | 最简单，和现有租户模型一致；没有 SSO/MFA |
| B. 接 IdP（OIDC/SAML） | role 来自 IdP 的 group claim | 要引入 OIDC 客户端与回调；多租户要每租户一套 IdP 配置 |
| C. 接 RADIUS | `Framed-IP-Address` 定 IP、`Class`/`Filter-Id` 定 role | 最标准（Cisco/FortiGate 都这么干）；要部署/对接 RADIUS，且我们的认证面要能当 NAS |

建议 v1 做 A，把 role 抽象成"策略对象"；B/C 作为 role 的来源适配器后加。这样不用为了
SSO 把地址分配逻辑重写一遍。

### 4.2 同一个 role 跨多台 CPE 时，IP 怎么算

这是整个设计里唯一容易埋雷的地方。若 role 的池是全局一个 /24，而两台 CPE 都把
`10.200.107.0/24` 配成 connected 并发布，overlay 里就是**同一前缀两个源**，选路会飘。

| 方案 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| **每 CPE 切片**（建议 v1） | role 的 /24 按站点切 /26：A 站 `.0/26`、B 站 `.64/26` | 接口=段，语义干净；发布整段即可；无 /32 风暴 | 用户换 CPE 会换 IP；地址规划要留余量 |
| 全局池 + 每用户 /32（Cisco 做法） | 接口仍配 /24 但不发布整段，只为在线会话发布 /32 | 用户可在站点间漫游且保持 IP | 要抑制 connected /24、单独发布 /32；会话迁移要撤旧 /32；控制面复杂一个量级 |

建议：**v1 用每 CPE 切片**，把"跨站保持同一 IP"列为 v2 需求再评估。多数客户的"role→段"
是地址规划与策略语义，不是"同一个人换城市还不换 IP"。

### 4.3 客户端 app 从哪来

- **v1 用标准 WireGuard 客户端**（Win/macOS/Linux/iOS/Android 都有），控制器生成配置或
  QR。零客户端开发，能最快验证数据面与运维模型。
- **v2 再做品牌 app**：套 WireGuard 官方库（wireguard-go / WireGuardKit / wireguard-windows），
  加 SSO 登录、自动选 CPE、断线重连、 posture。不要在 v1 就把 app 当前置条件。

### 4.4 规模化与状态

- VPP 的 `wireguard_peer_add` 按公钥**全局去重、去重键不含接口**（现有代码实测 -116）。
  一个终端一把钥匙没问题；但"同一终端同时连两台 CPE"或"换 CPE 后旧 peer 未清"会撞。
  需要会话 GC + "一个 user 同时只有一个 active lease"的约束。
- 每 peer 的 allowed-ips 只有一条 /32，1000 个终端 = 1000 个 peer。VPP 侧要实测握手/转发
  开销与 dump 成本；控制面要避免每次变更重发全量。
- `wireguard_peer_add` 目前要求 endpoint 合法（`isAPIAddress` 拒绝空值）。终端在 NAT 后面，
  配置时不知道公网地址。需要确认 VPP 插件是否像 Linux 一样"收到认证包就更新 endpoint"；
  若不更新，就得让 app 先上报 STUN 观测地址，或扩展 API 允许空 endpoint。**这是 P0 验证项。**

---

## 5. 三条候选路线与取舍

| 路线 | 内容 | 适合 | 不适合 |
|---|---|---|---|
| **A. VPP WG 自建接入面**（推荐） | 复用 VPP WG/LCP/VRF/FRR/leak；控制器加 role/IPAM/peer 下发；客户端先用标准 WG | 私有化、多租户、性能、与现有架构一致 | 要自己写 peer 管理、会话、app 后续 |
| B. strongSwan sidecar | IKEv2 + RADIUS 组策略 + 原生客户端；池用 dummy 接口挂进 VRF | 客户强要求"不装 app"、要标准 IKEv2 | 内核转发与 VPP 定案冲突；池不是接口；多租户要每租户一套 charon 配置 |
| C. ocserv per-group | 每组一个 `ipv4-network`；AnyConnect/OpenConnect 客户端 | 角色→段最现成、交付最快 | 同上，且客户端体验与性能最弱；组配置与我们的租户模型要对齐 |

如果目标是"这个季度能给客户演示"，C 最快；如果目标是"成为产品的一部分"，A 是唯一不需要
以后推翻重来的路线。B 只在"必须支持原生 IKEv2 客户端"时才有意义。

---

## 6. 建议的落地顺序（每步都可独立验收）

**P0 数据面证明（lab，不碰控制台）**
1. 建 `wireguard` 接口 + `lcp create ... tun`，内核侧 `ra-test` 入 `vpn107`。
2. FRR 配 `ip address 10.200.107.1/24`，确认 VPP table 107 出现 connected。
3. 两台标准 WG 客户端拿 `.2`/`.3`，确认：
   - 能 ping 网关；
   - 能经 overlay 到另一站点（leak 白名单放行该段）；
   - 终端从 Wi-Fi 切 4G 后隧道自动恢复（验证 endpoint 学习）；
   - 100/1000 peer 下的建立时延与转发基准。

**P1 控制面最小闭环**
- Role/池/租约模型 + 控制台页面（角色、池、在线终端、撤销）；
- 生成客户端配置/QR；peer 增量下发；租约回收；
- 用现有 8 段 bundle 之外的独立增量通道，避免每次终端上下线都 bump 配置代。

**P2 生产化**
- 认证：OIDC 或 RADIUS（按 §4.1 的选择）；
- 高可用：同一 role 池在备用 CPE 上的接管方式（会话重建 or 客户端重连）；
- 可观测：在线数、握手失败、租约耗尽、每终端流量；
- 品牌 app。

---

## 7. 参考与实测线索

- Cisco Catalyst SD-WAN Remote Access：用户进 service VPN、每用户 /32 由 OMP 发布
  <https://www.cisco.com/c/en/us/solutions/collateral/enterprise-networks/sd-wan/sd-wan-remote-access-wp.pdf>
- FortiGate SSL VPN：`ssl.root` + portal IP 池 + 组→portal
  <https://packetmentor.com/topics/fortigate-ssl-vpn>
- strongSwan：池由 IKE daemon/DHCP/RADIUS/SQL 管理；`rightsourceip=%` 持久租约；RADIUS
  `Class`→`rightgroups` <https://www.strongswan.org/>、<https://docs.strongswan.org/>
- ocserv：`config-per-group` 支持 `ipv4-network`/`ipv4-netmask`/`iroute`
  <https://src.fedoraproject.org/rpms/ocserv/blob/.../ocserv.conf>
- wg-access-server：单 CIDR、OIDC、routed networks、MIT
  <https://github.com/freifunkMUC/wg-access-server>
- NetBird：`100.64.0.0/10` 每 peer 地址、组/策略、network routes
  <https://docs.netbird.io/>
- Firezone：数据面 Apache-2.0、控制面 Elastic-2.0、生产自托管不受支持
  <https://github.com/firezone/firezone>
- VPP WG 多 peer + allowed-ip：`wireguard peer add ... allowed-ip ...`
  <https://s3-docs.fd.io/vpp/24.10/developer/plugins/wireguard.html>
- VPP WG 接口 + linux-cp `tun` 跑 FRR（社区实测）
  <https://mail-archive.com/vpp-dev@lists.fd.io/msg18670.html>
- VPP 接口进表：`set interface ip table <intf> <n>`
  <https://wiki.fd.io/view/VPP/Command-line_Interface_(CLI)_Guide>
- WireGuard 无原生 VRF 绑定（内核 socket 留在创建时的命名空间，需 fwmark + 策略路由）；
  这也是"不要走内核 WG"的一个理由
  <https://blog.sdn.clinic/?p=583>、<https://www.wireguard.com/netns/>
