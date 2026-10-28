# 调研：思科 SD-WAN 的 VPN 间导通与 prefix 过滤

2026-10-28 做 overlay 设计前置调研。目的只有一个：搞清楚**用户要配什么**才能让两个 VPN 互通、前缀过滤配在哪一层，然后据此定我们的 overlay 配置项。
本文只记录思科的做法与取证结果，不含我们的设计（设计见 `overlay-design-draft.md`）。

取证方式：官方文档静态页 curl/WebFetch 取正文；vManage 的 Policies 交互手册页是 JS 渲染，抓不到正文，所以字段名一律从能抓到的章节页与 CLI 命令参考取证。
**每条结论标了来源；第三节是找不到官方依据的说法，单列。** 之前有过把 borrowed 架构当成客户指令的教训，所以这一节和正文同样重要。

## 0. 结论摘要（决定我们方案形态的四条）

1. 思科**跨站点的 VPN 间导通不是 route-target 开关，而是控制器上的 centralized control policy**：`match route` 命中源 VPN 的前缀 → `action accept export-to vpn-list <目标>` → `apply-policy … control-policy … in`，**只在 vSmart 上配**，双向要两条 sequence，未命中默认丢弃所以必须 `default-action accept`。这是唯一被官方文档命名为 "Route Leaking" 的机制。[S1][S2][S3]
2. **prefix 过滤和导通是同一条 sequence**：`match route` 的匹配项里就有 `prefix-list`（实例：`match route / vpn-list VPN_10_20 / prefix-list _AnyIpv4PrefixList`）。所以"哪些前缀可以跨 VPN"是一个规则里的两个 match 字段，不是两套系统。[S2][S4]
3. **route-target 在思科是设备侧的准入条件，不是导通开关**：vManage 的 service VPN 配置里是 Import/Export Route Target List 两张表（"imports routing information from the target VPN extended community"），cEdge 走 `vrf definition … address-family … route-target import/export`。[S5][S6]
4. **同一台设备上的 VPN 互导是另一条路**（不经控制器）：cEdge 的 `route-replicate from vrf <src> unicast <proto> [route-map]`（17.2.1v 起可用于 vManage CLI 模板），思科文档另提到 `redistribute leaked routes` 只作用于 "the local service VPNs at the same edge site"。[S1][S6]

## 1. 跨 VPN 的手段，按语义分四类

| 类别 | 思科对象 | 谁执行 | 粒度 |
|---|---|---|---|
| 跨站点导通 | control policy 的 `export-to` | vSmart（控制器） | 源 VPN(+prefix-list) → 目标 VPN/vpn-list，一条 sequence 一个方向 |
| 同机本地互导 | cEdge `route-replicate`、`redistribute leaked routes` | 边缘设备 | 同一 edge site 的本地 service VPN 之间 |
| VPN 准入 | Route Target import/export 列表（vManage）/ `route-target import`（cEdge VRF） | 边缘设备 | 每 VPN 一组 RT |
| 站点进哪些 VPN | **VPN membership policy** | 控制器算、设备收 | "controls whether a device can participate in a particular VPN … which VPNs are allowed and not allowed to receive routes"，不允许就 **never forwards those routes** [S3][S7] |
| Global→service | vManage VPN 配置里的 "Route leak from Global VPN" 表 | 边缘设备 | 字段 `Route Protocol`(static/connected/bgp/ospf) + `Redistribution (in service VPN) Protocol`；**只有 global→service 一个方向** [S5] |

`export-to` 的原文约束：`export-to ( vpn vpn-id | vpn-list vpn-list )` — "Direct matching routes to the specified VPN or VPN list. **You can configure this option only with match route match conditions.**" 且 control policy 只配在 vSmart。[S1]

## 2. prefix 过滤的容器（可照抄的对象名）

| 用途 | CLI | vManage 位置 |
|---|---|---|
| 控制面前缀集合 | `policy lists prefix-list <name> ip-prefix <cidr>`，match 里引用 `prefix-list` | Centralized Policy > Control Policy > Match > Prefix List（注意某个 release 的文档明写该项 "Not available in Cisco SD-WAN Manager"，只能 CLI）[S3] |
| 数据面前缀集合 | `data-prefix-list <name> ip-prefix <cidr>`，条目上限 **8192** | Group of Interest > Data Prefix [S1][S8] |
| VPN/site/TLOC/color 集合 | `vpn-list / site-list / tloc-list / color-list` | Group of Interest 各 tab [S1] |
| `match route` 全部匹配项 | `color, color-list, omp-tag, origin protocol, originator ip-address, path-type, preference, prefix-list, region[role], site-id, site-list, tloc, tloc-list, vpn, vpn-list` | Match Condition [S1][S3] |
| `action` 全部动作 | `reject` / `accept` + `export-to vpn|vpn-list`、`set omp-tag`、`set preference(1-256)`、`set service …[vpn <id>]`、`set tloc …` | Action [S1][S3] |
| LAN 侧兜底（设备本地） | `policy access-list <name>` + match `source-data-prefix-list/destination-data-prefix-list/source-ip/…`，接口下 `vpn <n> interface <if> access-list <name> (in|out)`（name 上限 4096） | Feature Template：VPN Interface 上挂 ACL IPv4/IPv6；验证 `show policy access-list-associations` [S1][S8] |
| 设备宣告源头（每 VPN） | `omp advertise [connected|static|bgp|ospf|network prefix …] [route-map <tag>]`，可 `no advertise connected`；文档："can be issued for either a VPN or all VPNs on a device" | 设备侧模板；`show running-config vpn 1` 看 per-VPN omp advertise [S1][S9] |

`apply-policy … control-policy X (in|out)` 的语义差别（对"改写了再入库"还是"入库后改写"）：文档说明 **in** 作用于进 vSmart RIB 之前，动作影响存进 RIB 的 OMP 路由；**out** 作用在导出之后。导通用的是 `in`。[S1][S2]

## 3. UNVERIFIED（我在 cisco.com 上搜不到，别当成思科依据）

- **`route-import-service from vpn <id> <proto> route-policy <p>`（"Minimum release 20.9.1"）和 vEdge 的 `route-export/route-import {bgp|connected|ospf|static}`**：子代理给出并附了 VPN 配置页链接，但我复核时该页**没有这些命令**；`site:cisco.com` 搜 `route-import-service from vpn route-policy` **零命中**。结论：**按"未证实"处理**，同机互导我们只引用 cEdge 的 `route-replicate`（已证实）。
- **`vpn instance <n> import route-target <rt>` 这类 vEdge RT CLI**：命令参考全文零命中；RT 只在 cEdge `vrf definition` 与 vManage Route Target 表里出现。
- **extranet-VRF / extranet prefix-list 作为可配特性**：思科 SD-WAN 文档里 `extranet` 只有 `show omp …` 图例的状态位 `Ext -> extranet`，config 命令参考 **0 次**。extranet 属于传统 MVPN 语境，不是思科 SD-WAN 的配置对象。
- **`feature direct-subscriber`**：所有已取页面均无。共享服务在文档里是 data policy 的 `set service …` + `vpn service`，不是 direct-subscriber。
- **"相同 RT 不能在重叠前缀间 leak"、"重叠前缀必须 longer-match/aggregate 才能 leak"**：无官方表述。官方给的是另一件事：同一 VPN 跨站点重叠 IP 用 SS-NAT 处理，示例里才出现 `rd 1:10` + `route-target export/import 1:10`。[S10]
- **"OMP site-local vs global advertisement"**：这两个词不存在；文档实际给的是 per-VPN vs all-VPN 的 `advertise`，以及 Multi-Region Fabric 的 `region` match / Applied to Region（core/access/core-and-access，aggregate 需 vManage 20.13.1）。

## 4. 官方写明的限制与坑（影响我们把哪些东西做成校验）

1. control policy 未命中即丢："If a route or TLOC matches no parameters in any of the sequences … it is, by default, **rejected and discarded**" → 导通策略必须显式 `default-action accept` 或补齐 sequence。[S3]
2. `export-to` 只能配在 `match route` 下，且 control policy 只在控制器上配。[S1]
3. 同机 route replication 只覆盖**同一 edge site 的本地 service VPN**；leaked 路由属性丢失、在表里显示成 static。[S1][S6]
4. VPN 号：vEdge 0–65530，0=transport、512=management，Service VPN 1–511/513–65525；vManage 侧 1–65525 排除 512；65526/65528/65529/65530 建议保留。[S5][S7]
5. 每个接口只属于一个 VPN；cEdge 用 VRF 取代 VPN。[S5]
6. VPN 0 上跑的服务受 `allow-service` 约束；GRE 服务隧道的源必须配在 VPN 0。[S1]
7. 容量：`data-prefix-list` 8192 条、`access-list` name 4096。[S1]
8. Multi-Region Fabric：不启用 region 就没有 `region` match；"cannot create more than one region node between the global node and a site node"；多租户部署里不能建 region；region 上限 63（ID 1–63）。[S11]
9. 控制面策略只能过滤/改写/导出路由，**不改数据转发路径**；跨 VPN 的数据面转向属于 data policy 的 `set service / set tloc / set vpn`。[S1]

## 5. 思科的最小配置集（若我们要做到等价行为，用户至少要能填这些）

1. 两个 service VPN 已建，LAN 接口挂到各自 VPN。[S5]
2. `vpn-list` 源、`vpn-list` 目标（+ 需要限前缀时一个 `prefix-list`）。[S1]
3. 一条 `control-policy`：`sequence N / match route / vpn <SRC> [+ prefix-list] / action accept / export-to vpn-list <DST>`；反向再来一条 sequence；`default-action accept`。[S1][S2][S3]
4. `apply-policy site-list <两端站点> control-policy <名字> in`。[S2]
5. 设备侧只保留想导出的前缀来源：per-VPN `omp advertise` / `no advertise connected|static` / `advertise … route-map`。[S1][S9]
6. 同机本地互导（不走控制器）时才需要 `vrf definition` + `route-replicate from vrf <src> unicast <proto> [route-map]`。[S6]
7. 站点准入（多租户边界）：`policy vpn-membership` + `apply-policy … vpn-membership`。[S3][S7]
8. 验证：`show omp routes vpn <DST>`（控制器）、`show omp routes vpn X advertised|received`、`show ip route vrf X`。[S9]

## 来源

- [S1] [Cisco SD-WAN Configuration Commands](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/command/sdwan-cr-book/config-cmd.html)
- [S2] [Configure Route Leaking for Service Chaining](https://www.cisco.com/c/en/us/support/docs/routers/sd-wan/221907-configure-route-leaking-for-service-chai.html) ／ [中文版](https://www.cisco.com/c/zh_cn/support/docs/routers/sd-wan/221907-configure-route-leaking-for-service-chai.html)
- [S3] [Policies Configuration Guide for vEdge 20.x — Centralized Policy](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/configuration/policies/vedge-20-x/policies-book/centralized-policy.html) ／ [control policies（含 VPN Membership）](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/configuration/policies/vedge/policies-book/control-policies.html)
- [S4] [Cisco SD-WAN ZBFW and route leaking](https://www.cisco.com/c/en/us/support/docs/routers/sd-wan/217758-cisco-sd-wan-zone-based-firewall-zbfw.html)
- [S5] [Configure VPNs / interfaces, vEdge 20.x](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/configuration/system-interface/vedge-20-x/systems-interfaces-book/configure-interfaces.html)
- [S6] [IOS XE Catalyst SD-WAN Qualified CLI — VRF commands](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/command/iosxe/qualified-cli-command-reference-guide/m-vrf-commands.html)
- [S7] [Cisco Catalyst SD-WAN Design Guide](https://www.cisco.com/c/en/us/td/docs/solutions/CVD/SDWAN/cisco-sdwan-design-guide.html)
- [S8] [Localized Policy, vEdge 20.x](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/configuration/policies/vedge-20-x/policies-book/localized-policy.html)
- [S9] [Operational Commands（show omp …）](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/command/sdwan-cr-book/operational-cmd.html)
- [S10] [Configure overlapping IP for same VPN across sites](https://www.cisco.com/c/en/us/support/docs/routers/xe-sd-wan-routers/220969-configure-overlapping-ip-for-same-vpn-ac.html)
- [S11] [Cisco SD-WAN Security Policy Design Guide](https://www.cisco.com/c/en/us/td/docs/solutions/CVD/SDWAN/cisco-sdwan-security-policy-design-guide.html)
- [S12] [IOS XE SD-WAN OMP commands](https://www.cisco.com/c/en/us/td/docs/routers/sdwan/command/iosxe/qualified-cli-command-reference-guide/m-omp-commands.html)
