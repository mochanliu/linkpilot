# DIA + NAT + DHCP/DNS 方案（并把「基础配置」拆成模块）

日期：2026-11-11。状态：**只出方案，未改代码**（按 AGENTS.md §"controller 增量先出方案"）。
本文的每条 VPP 能力都在 lab 的 CPE1 上当场敲过，标【量】；读代码得到的标【码】；判断标【推断】。

---

## 0. 一页结论

四件事的难度不是一回事：

| 件 | 难度 | 为什么 |
|---|---|---|
| **NAT44** | 小 | VPP 原生就有，`nat44 ei` 插件在跑，命令齐全【量】 |
| **DIA（本地出口）** | 小 | 本质是"VPN 表里加一条默认路由 + NAT 挂 inside/outside"，不引入新机制 |
| **DHCP** | 小–中 | VPP **没有 DHCP 服务器**，只有 client 和 **proxy（中继）**【量】；但"服务面"已实测打通（见 §5），服务器用**用户态程序直接跑**即可，工作量在服务本体，不在数据面 |
| **DNS** | 小–中 | `dns_plugin` 是 VPP 自己的解析器（`dns cache` / `dns name-server`），**不是给客户端用的 DNS 服务**【量】；同样用用户态程序直接跑，已实测 dig 通 |

一句话：**NAT 和 DIA 是配置模型的工作；DHCP/DNS 是"服务面"的工作 —— 而服务面已经不需要我们自己造了**。

【2026-11-11 23:1x 更正】本文早前版本说"内核侧服务 IP 收不到 VPP 的 unicast"，这是**错的**，
原因有两个：当时用了"客户子网里塞第二个 IP + 手工静态路由"这种错姿势，且 lab 的 lcp punt 表被我
一次 `lcp delete/create` 打坏了（§5.4 坑 a）。恢复后的实测：**用户态守护进程绑在网关 IP 上，
DNS 查询与应答、DHCP 广播的收发全部通**（F8–F10）。所以 S2 不再是"要不要先花半天验"的问题，
而是"已经验完，按用户态守护进程落地"。

工作仍是两段：**S1 先把 DIA+NAT 做完整（零风险、可独立交付）**，**S2 已由一枪实验结项**，
直接进 S3 写 `sr-svc`。

---

## 1. 今天在 lab 量到的事实（CPE1，2026-11-11 22:0x–22:4x）

| # | 事实 | 取证 |
|---|---|---|
| F1 | NAT 全家桶都在：`nat44_ei_plugin`、`nat44`、`det44`、`nat64`、`nat66`、`pnat`、`cnat` | `show plugins`【量】 |
| F2 | `nat44 add interface address`、`nat44 plugin enable [inside-vrf|outside-vrf]`、`nat44 ei mss-clamping`、`show nat44 ei interfaces|sessions` 都在 | `nat44 ?` / `show nat44 ?`【量】 |
| F3 | DHCP 只有 **client + proxy（中继）**：`set dhcp proxy [del] server <ip> src-address <ip> [server-fib-id] [rx-fib-id]`；**没有 server** | `set dhcp ?` / `show dhcp ?`【量】 |
| F4 | `dns_plugin` = "Simple DNS name resolver"，命令是 `dns cache add|del`、`dns name-server`，**是 VPP 自己的解析器** | `dns ?`【量】 |
| F5 | 客户侧 **DHCP DISCOVER 广播能到内核**：在 CPE1 的 67 端口收到 cust12（172.31.12.13:68）的 249 字节 DISCOVER | 手工构造 DISCOVER + LCP tap 监听【量】 |
| F6 | ~~反方向不通~~（**已作废**，见 F8–F10）：当时把服务地址塞进客户子网 `172.31.12.2/32` 并手工加 `ip route ... via tap4096`，cust12 到 .2 全丢 | 【量】当时 |
| F7 | punt 相关命令：`punt socket register [IPV4\|ipv6] [UDP\|tcp] [ALL\|<port>] socket <path>`、`ip punt redirect [add\|del] rx <if\|all> via [<nh>] <tx_if>`、`set punt ipv4 udp <port> [del]`、`show punt client\|db\|reasons\|stats` | `punt ?`【量】 |
| **F8** | **用户态 DNS 打通**：内核里把 responder 绑在 `172.31.12.1:53`（lcp-x0 上本来就有这个地址，lcp-sync 从 VPP 镜像来的），cust12 `dig @172.31.12.1` → responder 收到 54B（src 172.31.12.13:37709）→ 回 47B → dig 显示 `ANSWER 10.9.9.9` | 双向计数 + dig 输出【量】 |
| **F9** | **DHCP 广播回程也通**：内核 `172.31.12.1:67`（`SO_BINDTODEVICE lcp-x0`）发 37B 到 `255.255.255.255:68`，cust12 的 68 端口收到 | 两端日志【量】 |
| **F10** | 客户侧 DISCOVER（249B，广播）到内核 `0.0.0.0:67`，xid 对得上 | 两端日志【量】 |
| **F11** | 上面能通不是我们配的：linux-cp 在每个 pair 建立时自动装 `ip punt redirect rx host-enp0s8 via tap4096`，forwarding = `ip4-dvr-tap4096-dpo`（CPE1/CPE3 同形）。"发给本机"的包由它交给内核 TAP，回程走同一条 TAP | `show ip punt redirect`【量】 |
| **F12** | `punt socket` 路线存在但受启动配置约束：26.06 CLI 直接 `register` 报 `punt socket register: socket is not configured`；必须先 `startup.conf` 里 `punt { socket /path }`（官方文档确认；一个 VPP 只能一个路径，SOCK_DGRAM，双向） | CLI + 文档【量】 |
| **F13** | 两个坑，已复现并写进 §5.4：①`lcp delete` + `lcp create` 在线重做 pair 后，`show ip punt redirect` 里 DVR dpo 看着正常，但数据面**不再 punt**，只有 `systemctl restart vpp` 才恢复；②服务地址塞进客户子网会抢内核到该网段的源地址选择（ProxyJump 直接断） | 【量】 |

lab 已恢复：临时服务地址/路由/监听进程全部删除，`172.31.12.0/24` 回到 connected；
CPE1 重启 VPP 后按 sr-box 清单自愈（lcp pair 重建、地址回位、sr-wg 2/2 usable），
复验 12→3 / 12→4 / 4→12 / 4→3 均 0% 丢包。

---

## 2. 「基础配置」拆模块（信息架构）

现在是一页五个面板（主机名 / VPN / 接口 / 静态路由 / VRRP）【码 `BasicConfig`】。
按 owner 的要求拆成左导航子模块，每个模块一个草稿片段、同一次发布、同一套版本/回滚：

| 模块 | 内容 | 落到哪 | 现状 |
|---|---|---|---|
| **概览** | 主机名、发布状态、最近一次 diff、检查项汇总 | — | 有（散在页首） |
| **接口** | VPP/LCP 名、IP/掩码、VPN、MTU、arp_ignore、up/down | VPP fragment + lcp | 有 |
| **VPN 与路由** | VRF 列表、BGP/OSPF 参数、（路由泄露在独立页） | FRR 段 | 有 |
| **静态路由** | 前缀/下一跳/VPN | VPP fragment | 有 |
| **高可用** | VRRP 组、优先级、抢占、VIP | 第 8 段 | 有 |
| **互联网出口（DIA）** | 每个 VPN 一个出口：WAN 口、下一跳（静态/自动）、是否 NAT、探测目标、失败动作 | **新：第 9 段** | 无 |
| **NAT** | 开关、outside 接口、地址池（v1=复用 WAN 口地址）、MSS clamp、静态端口映射、会话表 | **新：第 9 段** | 无 |
| **DHCP** | 地址池（VPN×接口）、范围、网关、DNS、租期、静态绑定（MAC→IP）、租约表（只读） | **新：第 9 段 + sr-svc** | 无 |
| **DNS** | 上游解析器列表、本地记录（A/CNAME）、缓存 TTL、是否对客户端提供服务 | **新：第 9 段 + sr-svc** | 无 |
| 发布与版本 | 保持现状（独立入口），新增段自动纳入 diff/回滚 | — | 有 |

导航上建议：`基础配置` 从"一个叶子"变成"一个分组"，子项 = 上面 8 个模块 + `发布与版本`。
这样以后加"QoS 策略"（owner 已定的位置在 SD-WAN 策略下）不会再把某一页撑爆。

### 2.1 落地（2026-11-13，owner 定的最终模块表）

实际拆成 **7 个模块**，以 owner 这一轮给的清单为准（与上面这张草表的差别：概览改名
「设备信息」并合并了发布与版本；NAT 与 DHCP/DNS 拆成两页但**共用一份服务面**；
QoS 从 SD-WAN 策略挪过来独立成页；DIA 与静态端口映射还没做）：

| 导航项（`data-view`） | 内容 | 后端 |
|---|---|---|
| 设备信息（`config`） | 设备身份、hostname、下发到设备、版本历史/diff/回滚、校验、配置包内容 | `/api/config` |
| 接口配置（`config-ifaces`） | 路由实例（VPN）、客户口、VRRP | `/api/config` |
| 静态路由（`config-static`） | 静态路由（含 UCMP weight） | `/api/config` |
| 动态路由（`config-dynamic`） | BGP / OSPF（按 VPN） | `/api/config` |
| NAT（`config-nat`） | NAT 出口行 + 暂不下发说明 | `/api/services` |
| QoS（`config-qos`） | 物理口出方向调度 | `/api/config` |
| 服务配置（`config-services`） | DHCP server、DNS 转发器、第 8 段原文 | `/api/services` |

两条不允许破的约束：

1. **模型不跟着拆**。5 个页面编辑同一份 `BasicConfig` 草稿、保存时提交整份；
   NAT 与服务配置共用同一个 `ServicesSet`，服务端只有一次 `PUT /api/services`
   —— 拆成两个 doc 会把对方的行写空。
2. **一台当前设备**。`S.cfg` 是权威，服务面跟着它走（`svFollowCfg`）。否则在
   NAT 页换了设备、回到接口配置还是上一台。

---

## 3. 数据模型（字段级，`controller/cmd/sr-ctrl/config.go`）

```go
type BasicConfig struct {
    Hostname string         `json:"hostname"`
    VPNs     []ConfigVPN    `json:"vpns"`
    Ifaces   []ConfigIface  `json:"ifaces"`
    Statics  []ConfigStatic `json:"statics"`
    VRRP     []ConfigVRRP   `json:"vrrp"`
    Egress   []ConfigEgress `json:"egress,omitempty"`   // 新
    NAT      *ConfigNAT     `json:"nat,omitempty"`      // 新
    DHCP     []ConfigPool   `json:"dhcp,omitempty"`     // 新
    DNS      *ConfigDNS     `json:"dns,omitempty"`      // 新
}

type ConfigEgress struct {          // 一个 VPN 一条出口，多条 = 主备
    VPN      uint32 `json:"vpn"`
    WANIface string `json:"wan_iface"`           // 必须是 underlay 口
    NextHop  string `json:"next_hop,omitempty"`  // 空 = 用该口 DHCP/对端给的网关
    Metric   uint32 `json:"metric,omitempty"`    // 主备次序，小的优先
    NAT      bool   `json:"nat"`                 // 出口是否做 NAT44
    Probe    string `json:"probe,omitempty"`     // 探测目标（ICMP），空=不探测
    OnFail   string `json:"on_fail,omitempty"`   // "withdraw"（撤默认路由，v1 唯一动作）
}

type ConfigNAT struct {
    Enabled    bool   `json:"enabled"`
    Outside    string `json:"outside"`             // WAN 口
    PoolStart  string `json:"pool_start,omitempty"`// 空 = 复用 outside 口地址
    PoolEnd    string `json:"pool_end,omitempty"`
    MSSClamp   uint16 `json:"mss_clamp,omitempty"` // 0=不设
    Timeouts   NATTimeouts `json:"timeouts"`
    PortForwards []PortForward `json:"port_forwards,omitempty"` // 静态映射（v1 只做 tcp/udp 单端口）
}

type ConfigPool struct {
    VPN      uint32 `json:"vpn"`
    Iface    string `json:"iface"`        // 客户口
    RangeStart string `json:"range_start"`
    RangeEnd   string `json:"range_end"`
    Gateway    string `json:"gateway"`    // 一般 = 该口 IP
    DNS        []string `json:"dns,omitempty"`
    LeaseSec   uint32   `json:"lease_sec"`
    Static     []DHCPBinding `json:"static,omitempty"`
}

type ConfigDNS struct {
    Enabled   bool     `json:"enabled"`
    Upstreams []string `json:"upstreams"`
    Records   []DNSRecord `json:"records,omitempty"`
    CacheTTLSec uint32 `json:"cache_ttl_sec,omitempty"`
}
```

校验（进 `config_check.go`，拦在发布前）：WAN 口必须存在于 underlay；NAT.Outside 必须被某个 Egress 引用；
DHCP 池范围不能跨子网、不能含网关；同一 (VPN, Iface) 只能一个池；DNS 上游必须是 IP。

---

## 4. 数据面：NAT 与 DIA 怎么落（S1，全部 VPP 原生）

### 4.1 DIA 的落点

我们的 overlay 只 advertise 站点 /32 与业务前缀，**没有 0.0.0.0/0**。所以"本地出口"就是：

```
VPN 表（table = VPN 主表号）里加：0.0.0.0/0 via <WAN 网关> <WAN 口>
```

具体前缀（overlay 学到的）更长 ⇒ **天然优先走隧道**；其余全部走 DIA。不需要 PBR，也不需要新节点。

主备：`Egress.Metric` → VPP 路由 preference（复用我们在颜色上已经用熟的机制：pref 小的优先，前一条撤了自动接上）。

回退：v1 只做 `OnFail=withdraw` —— 探测失败就撤默认路由（客户流量回到"没有出口"而不是黑洞）。
探测复用现有探针通道还是新起一条，见 §7 的 S1 判据（先不引入新守护进程，用 sr-agent 已有的 metricsLoop 里加一条 ICMP 探测）。

### 4.2 NAT 的落点

```
nat44 ei plugin enable sessions <N> users <M> inside-vrf <vpn> outside-vrf 0
nat44 ei add interface address <WAN 口>          # outside
set interface nat44 in <客户口> out <WAN 口>      # 若该 build 需要显式 inside/outside 挂载
nat44 ei mss-clamping <1400>                     # PPPoE/PPTP 场景
```

【推断】`inside-vrf/outside-vrf` 的多 VPN 组合方式需要 S1 首枪实测（每个 VPN 一套 plugin 配置，还是全局一套 + 每接口 VRF 归属）。
**这一条必须在 S1 的判据里作为一个独立子项**：单 VPN 通 ≠ 多 VPN 通。

### 4.3 会话与读回

- `show nat44 ei sessions` 解析成 (inside, outside, proto, state, last-seen)，控制台 NAT 模块只读展示。
- 读回口径：**"配置里写了"不算数，`show nat44 ei interfaces` 里 outside 口在、`show nat44 ei addresses` 里有池，才算落地**。

---

## 5. DHCP / DNS：服务面已实测（结论：用户态守护进程直接跑，不改 VPP）

### 5.1 三张证据（CPE1，2026-11-11 23:1x–23:3x）

| 验的 | 做法 | 结果 |
|---|---|---|
| DNS 收 + 回 | 内核 python responder 绑 `172.31.12.1:53`；cust12 `dig @172.31.12.1 probe.example A` | responder 收到 54B、回 47B；dig 显示 `status: NOERROR` + `ANSWER: 10.9.9.9`【量】 |
| DHCP 收 | cust12 手工 DISCOVER（广播 249B） | 内核 `0.0.0.0:67` 收到，xid 一致【量】 |
| DHCP 回 | 内核 `172.31.12.1:67` 发 37B 广播到 `255.255.255.255:68` | cust12 的 `0.0.0.0:68` 收到【量】 |

也就是说：**"装个现成的用户态服务"这条路是成立的** —— 既不需要 NAT，也不需要中继，
更不需要改 C 插件。原来以为卡住的"VPP→用户态→回来"两段，VPP 已经替我们做好了。

### 5.2 为什么能通（这是 linux-cp 自带的能力，不是我们新加的）

```
lcp create host-enp0s8 host-if lcp-x0
  └─ 自动装 ip punt redirect rx host-enp0s8 via tap4096   （forwarding: ip4-dvr-tap4096-dpo）
  └─ lcp-sync 把接口地址镜像到内核 TAP，于是 lcp-x0 上本来就有 172.31.12.1/24
```

- **收**：客户发到"本机地址"的包，VPP 在 `ip4-local` 判定 for-us 后按 per-interface punt 表交给 TAP，
  内核看到的就是一张普通网卡，守护进程 bind 网关地址即可（不用加地址、不用加路由）。
- **回**：守护进程从 TAP 发出，VPP 按 FIB 转发给客户；**limited broadcast（255.255.255.255）也过**（F9），
  所以 DHCP 的 OFFER/ACK 不需要特殊处理。
- 所以早前 F6 的"反方向不通"要更正：不是 punt 不行，是**姿势错了**（见 §5.4 两条红线）。

### 5.3 另一条路：punt socket（留给"要裸包"的自研进程）

`startup.conf` 里加 `punt { socket /run/vpp/punt.sock }`，再
`punt socket register ipv4 udp 53 socket /run/vpp/punt.sock`。VPP 把整包写进 SOCK_DGRAM unix socket，
**进程写回来就是注入**，等于自己接管那个端口。

- 适合：以后要做 DPI/应用识别（方案 3）这类"要看裸包、不想经过内核协议栈"的活。
- 代价：改启动配置（必然 VPP 重启）、一个 VPP 只能一个 socket 路径、注册项 CLI 不完全可 dump（F12）。
- **首版 DHCP/DNS 不用它** —— 有现成的内核栈和现成的服务实现，没必要自己搬包。

### 5.4 落地形状与两条红线

- `sr-svc`（Go，零外部依赖）：DHCPv4 server + DNS forwarder，绑在**客户口地址（= 网关 IP）**的 `:67/:53`，
  租约表落盘并上报控制器。**不引入 dnsmasq/kea/unbound**：lab 镜像里这四个一个都没装【量 `which`】，
  真机形态也不该要求客户装包。
- 不需要 `set dhcp proxy`（那是把请求中继给外部服务器）；不需要 NAT；不需要额外接口。
- **红线 a**：下发服务配置永远不要 `lcp delete` + `lcp create`。在线重建 pair 后
  `show ip punt redirect` 里 DVR dpo 看着是好的，但数据面不再 punt，**只有重启 VPP 才恢复**（今天踩过，F13）。
  服务配置只是"绑端口"，与 pair 无关，直接 SIGHUP 即可。
- **红线 b**：不要往客户口 TAP 上塞客户子网内的第二个地址（`172.31.12.2/32` 这种）。
  它会抢内核到该网段的源地址选择，把管理/ProxyJump 路径一起打断；确实需要独立服务 IP 时用 `169.254.<vpn>.1`。

### 5.5 DHCP 的验收缺口（必须提前说）

lab 现在**没有客户终端**（`cust-config.sh` 的设计：只有客户路由器，静态地址）【码】。
所以 DHCP 的 e2e 判据不能是"等客户端拿地址"，得用**手工 DISCOVER/REQUEST 脚本**（今天已经跑通发包侧），
外加一台临时 VM 做真实客户端。**要不要加这台 VM，请 owner 拍**（加 = 多一台要维护的实验件；不加 = DHCP 只能验到"服务器回了正确的 OFFER"）。

---

## 6. 下发链路

1. **渲染**：`config_render.go` 新增 `renderServicesArtifact(c)`，产出第 9 段 `services`
   （`{nat:{...}, dhcp:[...], dns:{...}}`）。段序号按 applier 的执行顺序排（现在 1–8，VRRP 是第 8）【码 `apply.go` 头注释】。
2. **apply**：`internal/apply/services.go` 新增 `applyServices`，放在 VRRP **之前**（NAT/DHCP 是数据面，先于 VIP 生效）；
   DHCP/DNS 的配置写 `/run/sr-svc/config.json` 并 `SIGHUP`（不重启守护进程，避免租约丢失）。
3. **幂等**：NAT 用"先读回再改"（`show nat44 ei interfaces/addresses` 与期望比对），不做 `nat44 plugin disable/enable` 这种全清动作
   （会清会话，客户可见断流）。
4. **boot replay**：VPP 重启会丢 NAT 配置（和 VRRP/policy 同理），`Boot()` 里必须补第 9 段；sr-svc 自己从 `/etc/sdwan/services.json` 恢复租约表。
5. **控制台**：四个模块各自 `GET/PUT /api/config`（复用现有草稿接口，不新增端点）；新增只读 `GET /api/nat/sessions`、`GET /api/dhcp/leases`。
6. **台账**：新增事件 `config.publish` 里已经带段 diff，不用新事件；NAT 会话/租约不进台账（量太大），只进监控页。

---

## 7. 分片与判据

| 片 | 内容 | 判据（缺一条不算完成） |
|---|---|---|
| **S1** | DIA 出口 + NAT44（模型/校验/渲染/apply/读回/控制台两个模块） | ① cust12 → 192.168.1.4（当作"互联网"）经 DIA+NAT 通，0% 丢包；② CPE1 `show nat44 ei sessions` 有对应会话；③ 删掉 Egress 重新发布后**不通**且会话清空；④ 多 VPN 各自出口互不串（至少两个 VPN）；⑤ VPP 重启（`systemctl restart vpp`）后自动恢复，客户断流 ≤ 现有基线（~15s） |
| **S2** | 服务面：**已结项**（2026-11-11 23:1x，见 §5.1） | ① 守护进程收到 VPP 转来的 DISCOVER ✅（F10）；② 守护进程发的广播 OFFER 到客户 ✅（F9）；③ `dig @网关IP` 到守护进程并收到回包 ✅（F8）；④ 剩下的只有"换成 Go 的 `sr-svc` 后重复 50 次不掉"——这条并入 S3 判据① |
| **S3** | `sr-svc`：DHCP server + DNS forwarder + 租约上报 + 控制台两个模块 | ① 手工 DISCOVER/REQUEST 拿到正确 OFFER/ACK（网关/DNS/租期对）；② 静态绑定生效；③ 租约表在控制台可见；④ DNS 转发命中缓存（第二次 <1ms） |
| **S4** | 收口 | ① boot replay 之后 NAT/DHCP/DNS 全在；② 版本回滚能回到"没有 DIA"的状态；③ `vm-verify.sh` 不回归；④ 文档 + 台账 |

**S1 可以独立交付**（不依赖 S2 的结论），这也是我建议先做 S1 的原因：它零风险、可回滚、当天能验。

---

## 8. 风险

- **NAT 与 overlay 的优先级**：默认路由进 VPN 表之后，任何"本该走隧道但没有具体前缀"的流量会静默走 DIA。
  这是 DIA 的语义，但对排障不友好 ⇒ 控制台必须在 DIA 模块上直接显示"这个 VPN 当前有多少条 overlay 前缀更具体"，否则客户会以为"隧道没生效"。
- **NAT 会话清空**：任何 `plugin disable/enable` 都会清会话。apply 必须做 diff，不许整段重放。
- **DNS 单点**：sr-svc 挂了 = 全站解析挂。要么 keepalive 自愈（systemd `Restart=always`），要么 DHCP 同时下发两个 DNS（第二个是上游）。
- **地址池与 VRRP 双机冲突**：双机场景两台都跑 DHCP 会发重号地址。v1 不做双机，但模型里要预留 `active/standby` 位，别到时候改协议。

---

## 9. 要 owner 拍的 3 条

1. **DIA 语义**：per-VPN 默认路由（我建议）还是 per-VPN 前缀列表（更可控但配置量大）？
2. **NAT 地址池**：v1 复用 WAN 口地址（我建议），独立池后置？
3. **DHCP 验收集**：加一台客户终端 VM（真 dhclient），还是用手工 DISCOVER/REQUEST 脚本验到"OFFER/ACK 正确"？

原第 1 条（是否先做服务面一枪）已由 §5 的实测取消：不用验了，用户态守护进程直接跑。
原第 5 条（S1 先独立交付）按建议执行，不再占一次决策。
