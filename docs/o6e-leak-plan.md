# O6-e：路由泄露（SDWAN-CPE / CPE-SDWAN）方案

> 状态：**方案已拍**（本文只出方案，代码按 §9 逐步落地）。日期 2026-11-02。
> 依据：2026-11-02 lab-cpe1 实测（K 路由 / 只进内核 / 同前缀覆盖）+ 现有代码。
> 已拍：① route-map / prefix-list 名字按 VPN 派生（`SDWAN2CPE-vpn<id>`）；
> ② 同前缀冲突**接受 K 覆盖**（转发仍由 VPP 的 source priority 决定，见 §1）；
> ③ 批量下发 = 一步写草稿并发布；④ prefix-list 只做 permit；
> ⑤ 发布入口放在泄露页本身（设备行 + prefix-list 行），见 §6.2；
> ⑥ prefix-list 是**每个租户自己的**配置，不跨租户共享（跨租户共享属于运营商视图，等你点名）。

## 0. 一句话

"路由泄露"拆成两个互不依赖的模块，各一页：

- **CPE-SDWAN**：客户 → SD-WAN。每个（设备, VPN）选**来源**（`bgp/ospf/static/connected`）
  与**白名单 prefix-list**，sr-frr 只把选中的前缀上报给 sr-route。
- **SDWAN-CPE**：SD-WAN → 客户。sr-route 的 RIB 已经按 RT 命中**目标 VPN**
  （`/status` 的 `rib` 键就是 0..255 的 VPN 号），sr-frr 直接在该 VPN 的**内核表**里注入
  `K` 路由；该 VPN 的 BGP/OSPF 用 `redistribute kernel route-map SDWAN2CPE-vpn<id>` 发给客户。

table 1250、`sr-frr.json` 的 `vrfs/table_id`、`redistribute table-direct 1250` 整条旧路径退役。

## 1. 实验结论（2026-11-02，lab-cpe1，全部实测）

| 注入方式（前缀 → 内核表） | `show ip route vrf vpn100` | VPP table 100 |
| --- | --- | --- |
| `dev vpn100`（VRF 设备本身） | `K>* 198.51.102.0/24 [0/0] is directly connected, vpn100` | **不出现** |
| table 254 `dev lo` | `K>*` | **不出现**（VPP 主表也没有） |
| `dev dum100`（未配对的 dummy） | `K>*` | **不出现** |
| `dev lcp-x0`（lcp 配对口） | `K>*` | 出现（via 10.79.1.3） |
| blackhole | `K>* … unreachable (blackhole)` | 出现（dpo-drop） |

由此得到三条要写进代码的规则：

1. **linux-nl 只镜像"出口是 lcp 配对口"或"没有出口"（blackhole）的内核路由。**
   出口是 VRF 设备 / `lo` / 未配对 dummy 的路由停在纯内核，FRR 标记 `K`，VPP 不可见。
   → SDWAN-CPE 就用这个：VPN 0 走 `lo`，非 0 VPN 走 `vpn<N>` 设备。
2. **K 路由的 distance 是 0**（FRR 给的），同前缀会压过 BGP(20)/OSPF(110) 的本地路由。
   你已拍"接受覆盖"；转发不受影响（VPP 里 `lcp-rt` 来源高于 `API`，本地转发仍赢）。
3. **配置里新加 `redistribute kernel` 时，已经在表里的 K 路由不会被重新计算**
   （BGP 与 OSPF 都实测到），必须有一次路由更新。→ §5.3 的 touch。

另两条实测结论（本次为写方案补测）：

- **FRR 的 `vrf_id` 是内核 VRF 设备的 ifindex，不是 VPN 号/表号**：
  2026-11-02 复测 `vpn100 → id 10, table 100`，`vpn101 → id 11, table 101`；默认 VRF 是 0。
  → CPE-SDWAN 多 VPN 归属靠这个映射（`/sys/class/net/vpn<N>/ifindex` 或 netlink link dump）。
- BGP 侧 `redistribute kernel route-map` + prefix-list 已验证（4 条 → 只留 1 条）；
  **OSPF 的 route-map + prefix-list 在 S-0 已用真配置验成**（见下）。

### 1.1 S-0 spike 结论（2026-11-02，lab-cpe1，FRR 10.7.1）

**① ZAPI 通知头 `vrf_id` 可用，但订阅必须按 VRF 发。**

- 收到的每条 `ZEBRA_REDISTRIBUTE_ROUTE_ADD/DEL` 头里 `vrf_id` = **路由所在 VRF 的 ifindex**
  （`vpn100` 的路由是 10，`vpn101` 是 11），不是常量 0；默认 VRF 的路由是 0。
- 但 `ZEBRA_REDISTRIBUTE_ADD` 的订阅是按头里的 `vrf_id` 注册的（zebra 的
  `client->redist[afi][type]` 是按 vrf 的位图）。**只发一个默认订阅只会收到默认 VRF 的路由**，
  其它 VPN 一条都不来。所以 sr-frr 要按每个 VPN 的 ifindex 各发一组
  `REDISTRIBUTE_ADD`（一条连接可以订阅多个 VRF），收到后用头里的 vrf_id 反查 VPN。
- 报文里没有别的 VRF 归属字段；不要试图从 nexthop 猜（导入路由的 nexthop vrf 可能是 0）。

**② 四个来源的类型号（以 FRR 10.7.1 `route_types.txt` 顺序为准，已逐条实测）：**

| 来源 | type | 实测方式 |
| --- | --- | --- |
| connected | 2 | vpn100 的 `10.200.0.0/30` 收到通知 |
| static | 4 | vpn100 的 `203.0.113.0/24 blackhole` 收到通知（type 3 是 local，别用） |
| ospf | 7 | vpn100 从 OSPF 学到的 `10.201.0.1/32` 收到通知 |
| bgp | 10 | 与既有结论一致 |
| kernel | 1 | 注入 vpn100 的 K 路由收到通知 |

**③ OSPF 的 `redistribute kernel route-map` + prefix-list 支持前缀过滤（实测）。**

vpn100 里两条 K 路由（`198.51.100.0/24`、`198.51.102.0/24`），prefix-list 只 permit 前者，
`redistribute kernel route-map SDWAN2CPE-vpn100` 后在 `show ip ospf vrf vpn100 database external`
里只出现 `198.51.100.0/24`（Type-5，metric-type 2，forward-address 0.0.0.0，metric 20）。
旧 lab 失败的原因确认是临时 ospfd 没同步 route-map，不是 FRR 不支持。
同时再次证实"先有路由、后加 redistribute 不会重算"：加完配置必须先 touch 一次才出 LSA。

**④ 注入的 K 路由不会被当成 BGP 再学回来（防回环的直接证据）。**

- 只有 K 的前缀（`198.51.100.0/24` 等）：订阅 bgp 类型时**没有任何通知**。
- 人为让 K 压住一条 BGP 路由（往 vpn100 注入 `10.68.1.0/24`）：zebra 立刻发该前缀的
  BGP `DEL`（不再是选中路由）；删掉 K 后 BGP `ADD` 回来。
  → K 在表里时 zebra 不会把同前缀当 BGP 再分发。§5.4 的集合交集仍保留（不靠这个概率）。

**⑤ VPP 侧复测：** `dev vpn100` / `dev lo` 注入的 K 路由在 VPP（table 100 与主表）都不出现；
`blackhole` 路由**会**镜像进 VPP（dpo-drop）——所以注入绝不能带黑洞下一跳，出口只用
`vpn<N>` / `lo`。测试用的 VRF、BGP 导入、OSPF 实例已全部撤掉，`frr.conf` 未改。
（副作用：VPP 里留下一个只有默认表项的 `ipv4-VRF:100`，无路由、不影响转发。）


今天的基线（必须一起退役）：sr-frr 走 ZAPI `ZEBRA_ROUTE_TABLE` + 黑洞下一跳 → FRR `T>*` →
内核 table 1250（`unreachable (blackhole)`）→ VPP `ipv4-VRF:1250` 全是 dpo-drop →
客户侧 `redistribute table-direct 1250`。这条链路只在控制面里，VPP 里那张 1250 表没有任何用处。

## 2. 模块定义（按你的口径）

### 2.1 CPE-SDWAN（客户 → SD-WAN）

每个（设备, VPN）一行：

| 字段 | 取值 | 默认 |
| --- | --- | --- |
| 来源 | `bgp` / `ospf` / `static` / `connected`，可多选 | 空 = 都不进 |
| 白名单 | 本租户 prefix-list 库里的一个 | 预置 `ALL`（`0.0.0.0/0`） |

- 过滤放在 **sr-frr**：不能通过 FRR 的入方向过滤做，否则客户自己的 RIB 也会被清掉；
  我们的 overlay 邻居不是 BGP 邻居，也没有 outbound route-map 可挂。
- 输出仍走 sr-route 的 `PUT /local-prefixes`（已经支持每条带 `vrf_id`，多 VPN 不用改协议）。
- 前缀列表内容在**发布时展开**进 `sr-frr.json`，设备按冻结内容执行。

### 2.2 SDWAN-CPE（SD-WAN → 客户）

每个（设备, VPN）一行：

| 字段 | 取值 | 默认 |
| --- | --- | --- |
| 协议 | `bgp` / `ospf`，可多选；**只提供该 VPN 真的有的实例** | 空 = 都不进 |
| 白名单 | 本租户 prefix-list 库里的一个；可以不选 | 不选 = 全放过 |

盒子上三件事：

1. sr-frr 把 sr-route RIB 里**目标 VPN 等于本 VPN** 的前缀注入内核表 = VPN 号
   （VPN 0 = 内核主表），出口 `lo` / `vpn<N>`，打 `K`。
2. 该 VPN 的每个选中协议实例里加
   `redistribute kernel route-map SDWAN2CPE-vpn<id>`。
3. route-map `SDWAN2CPE-vpn<id> permit 10`：不挂列表时内容为空；挂了则
   `match ip address prefix-list SDWAN2CPE-vpn<id>`，prefix-list 用同一派生名在 FRR 顶层创建。

名字按 VPN 派生是必须的：FRR 的 route-map / prefix-list 名字在**整个 daemon 内全局**，
不是 per-VRF；不派生会在第二个 VPN 上撞名。

## 3. 数据模型

### 3.1 每个租户自己的 prefix-list 库

```go
type PrefixList struct {
    ID      string        `json:"id"`      // pl1...
    Name    string        `json:"name"`
    Entries []PrefixEntry `json:"entries"` // 空 = 全放过（仅 SDWAN-CPE 允许空）
}
type PrefixEntry struct {
    Prefix string `json:"prefix"`        // 必须是规范 IPv4
    LE     int    `json:"le,omitempty"`  // 0 = 不写 le
    GE     int    `json:"ge,omitempty"`  // 0 = 不写 ge
}
```

- 存在 `State.LeakConfigs[tenantID]`（与 Overlays 并列），**只属于这个租户**：租户之间不共享，
  运营商视图里也不出现（跨租户共享的对象等点名再做）。同一租户内两个模块用的是同一份。
- 租户第一次读到时惰性建 `ALL`（`0.0.0.0/0` 一条）；删光了下次读取再建，允许引用检查之外的删除。
- 只做 **permit**，不做 deny（已拍）。
- 发布时把**展开后的条目**冻结进 bundle；之后改列表内容，已发布的设备不受影响，页面标"需重新发布"，
  发布入口见 §6.2。

### 3.2 设备上的泄露行

放在 `DeviceConfig` 上、**不进 `BasicConfig`**：

```go
type LeakSet struct {
    ToCPE   []LeakRule `json:"to_cpe"`   // SDWAN-CPE
    ToSDWAN []LeakRule `json:"to_sdwan"` // CPE-SDWAN
}
type LeakRule struct {
    VPN        uint32   `json:"vpn"`
    Protocols  []string `json:"protocols"`              // 已排序去重
    PrefixList string   `json:"prefix_list,omitempty"`  // ToCPE 可空 = 全放过
}
```

- 理由：基础配置页保存的是整份 `BasicConfig`，若把泄露行塞进去，保存基础配置就会把它们冲掉；
  两个页面各管各的层（与既有页面分层一致）。
- 每个 VPN 最多一条规则；协议为空时**不存这条规则**（空行等于没有）。
- 规则依赖该 VPN 在**至少一个导通组**里（route.json 的 `vrfs` 有它，sr-route 才认这个 VRF，
  也才有 RT 决定进/出）。不在任何组里的 VPN 在保存/发布时直接报错，页面在选择器里注明。
- VPN 只开放 0..255（路由平面 VRF id 是 8 位），且排除保留的 250。

## 4. 谁渲染什么

| 落点 | 内容 | 谁生成 |
| --- | --- | --- |
| `/etc/frr/frr.conf` | SDWAN-CPE：`ip prefix-list` / `route-map` / `redistribute kernel` | 控制器（`renderFRR`，废掉第 100 行那句占位注释） |
| `/etc/sr-frr.json` | `export_vrfs`（哪些 VPN 要注入）+ `import_rules`（每 VPN 的来源与白名单） | 控制器，**bundle 新增第 5 段**，sr-apply 叠加写盘 |
| 内核表 0..255 | `K` 路由 | sr-frr（netlink 直写） |
| sr-frr 运行态 | `export_vrfs` + `import_rules` 热替换 | sr-apply 推 `PUT /config`（Unix socket） |
| `/etc/sdwan/route.json` | 不变 | 控制器 |

第 5 段是**叠加片段**（只写控制器拥有的两个键），不是整份文件：`status_url`、`zserv_socket`、
`state_file`、`poll_sec` 仍是盒子自己的。sr-apply 写盘时删除旧的 `vrfs` / `import` /
`distance` / `metric` 四个键（一次性 schema 迁移）；上报地址不再单列，由 `status_url` 换路径得到
（同一个 sr-route HTTP 口：`/status` → `/local-prefixes`）。

## 5. 盒子上的契约

### 5.1 SDWAN-CPE 的注入（sr-frr 的 netlink 部分）

- 新包 `internal/frr/krt`（`//go:build linux`，零依赖，`syscall` 手写 rtnetlink）：
  `RTM_NEWROUTE` / `RTM_DELROUTE`，`rtm_protocol = RTPROT_BOOT`（`ip route add` 的默认值，
  也是 zebra 认成 `K` 的那个值），`rtm_scope = RT_SCOPE_LINK`，`rtm_type = RTN_UNICAST`，
  属性 `RTA_DST` + `RTA_OIF` + `RTA_TABLE`。
- 出口：VPN 0 → `lo`；VPN N>0 → `vpn<N>`（boot 脚本已经会建这个 VRF 设备）。
  这两个出口都满足 §1 的"不镜像"规则，所以按实测规则 **VPP 里不会出现这些路由**。
- 不写 distance / metric：内核没有 distance，`K` 的 0 是 FRR 给的；metric 留 0 即可。
  旧配置里的 `distance` / `metric` 两个键退役。
- 状态文件（`state_file`）改成 `{"mode":"krt","routes":[{table,prefix,oif}]}`；
  旧格式（无 `mode`）直接删掉并记一条日志，**不按旧内容去删 1250**（那些是 zebra 的条目）。
  退出 / 崩溃恢复时按状态逐条 `RTM_DELROUTE`；`ESRCH`（内核里已经没了，例如容器重启换了 netns）
  视为成功。
- **只建不覆盖**（§5.6）：`RouteAdd` 是 `NLM_F_CREATE|NLM_F_EXCL`，EEXIST 不由本层吞掉；
  `RouteReplace` 只用于 `prev` 里确认是我们自己的前缀（出口 ifindex 变了）。
- 已知代价：内核会认为这些前缀挂在 `lo` / `vpn<N>` 上。盒子 `ip_forward=0`、没有默认路由、
  也不主动访问客户前缀，所以不影响转发；若将来有盒子自身发起去这些前缀的流量，会落到 `lo`，
  这是这个机制的边界，写进 README。

### 5.2 SDWAN-CPE 的 FRR 渲染

以 VPN 100 挂了列表、VPN 0 没挂列表为例，生成（顶层在前）：

```
ip prefix-list SDWAN2CPE-vpn100 seq 5 permit 10.0.0.0/8 le 24
ip prefix-list SDWAN2CPE-vpn100 seq 10 permit 192.168.0.0/16
!
route-map SDWAN2CPE-vpn100 permit 10
 match ip address prefix-list SDWAN2CPE-vpn100
!
route-map SDWAN2CPE-vpn0 permit 10
!
router bgp 65101 vrf vpn100
 address-family ipv4 unicast
  redistribute kernel route-map SDWAN2CPE-vpn100
 exit-address-family
!
router ospf vrf vpn100
 redistribute kernel route-map SDWAN2CPE-vpn100
!
router bgp 65101
 address-family ipv4 unicast
  redistribute kernel route-map SDWAN2CPE-vpn0
 exit-address-family
!
```

- 没选中的协议**不出 `redistribute`**；一个协议都没选的 VPN 不出 `route-map`/`prefix-list`，
  也不进 `export_vrfs`（不做无用的注入）。
- `redistribute kernel` 写在该 VPN 的**已启用实例**里；VPN 没有这个实例时，保存/发布检查就报错
  （批量下发按你的规则直接丢弃该协议，见 §7）。

### 5.3 热更与重算顺序

- FRR 侧：`frr-reload.py` 差量，热更后 `--test` 复检 0 条差异（AGENTS.md 第 1/2 条）。
- sr-frr 侧：新增 Unix socket（默认 `/run/sr-frr/control.sock`）：
  - `PUT /config`：整份替换 `export_vrfs` + `import_rules`，返回应用后的配置 hash；
  - `GET /status`：hash、注入条数、上报条数、最近错误。
  sr-apply 的顺序固定为：**先 FRR，再 sr-frr**。先让 `redistribute kernel` 在位，再让 K 路由出现，
  zebra 才会在路由新增时触发再分发。`PUT` 成功且 hash 对上后才原子落盘；`PUT` 失败则这次 apply
  失败、盘上保留旧文件、不重启任何进程。
- **touch**：如果某个 VPN 本来就在导出，只是新加了一个协议，K 路由已经存在，`redistribute kernel`
  不会重算。sr-apply 对比上一版已安装的 srfrr 片段，把"协议集合变了的 VPN"用
  `PUT /config` 里的 `refresh_vrfs` 带过去；sr-frr 对这些 VPN 的现有前缀做一次
  delete+add（纯内核，不进 VPP），触发 zebra 重新计算再分发。
  **这一步对客户侧可见的窗口必须先量**（BGP 可能撤/发一次，OSPF 可能重发 LSA），数字写进判据；
  超过可接受范围就改成"只对新增协议的那一个 VPN touch"，仍不行则在页面点名这个中断。
  2026-11-02 实测（VPN 100 的 4 条前缀）：单条 del→add 6–174µs，客户 BGP vrf 表版本号不动，
  维持现方案（详见 §9 的「L-6 实测结果」）。
- sr-frr 的配置热更**绝不重启**：重启会主动撤掉自己上报的全部前缀（已有退出清理逻辑），
  等于全网抖动一次；老路（改文件+重启）不保留。

### 5.4 CPE-SDWAN 的导入

- Importer 从"一个前缀集合"扩成 **`(vpn, prefix) → 来源集合`**：
  **按（VPN, 来源）发订阅**——`REDISTRIBUTE_ADD` 的头 `vrf_id` 填该 VPN 的设备 ifindex，
  再按来源各发一条（connected=2 / static=4 / ospf=7 / bgp=10，kernel=1，§1.1 实测）。
  只订默认 VRF 会漏掉其它 VPN 的全部路由。
- 通知归属 VPN：读 ZAPI 头的 `vrf_id`，用 ifindex→VPN 映射换算（§1.1 实测：头里就是路由所在 VRF）；
  未知 id 的通知丢弃并计数（不猜、不并到 VPN 0）。
- VPN 0 用默认 VRF（ifindex 0 的订阅），无需映射。
- 输出：只保留**该 VPN 规则里选中的来源**，且命中该 VPN 的白名单；没规则的 VPN 一条都不报。
  取消一个来源立即生效（从保留的 per-source 集合重算），不等 zebra 撤路由。
- **防回环**：同一 VPN 的 `export_vrfs` 注入集合里的前缀，永远不在上报集合里出现
  （S-0 已证 K 压过 BGP 时 zebra 不会再发 BGP 通知；这里仍用集合交集写死，不靠这个概率）。
  被压掉的条数进 `/status` 和日志。
- 上报仍是整份替换 `PUT /local-prefixes`（单写者；每条带 `vrf_id`，sr-route 现有校验要求该 VPN
  在 route.json 的 `vrfs` 里——§3.2 的"必须在导通组里"就是为它准备的）。

### 5.5 table 1250 退役与升级

- 删除：`table_id` 配置、`redistribute table-direct 1250`、ZAPI `ZEBRA_ROUTE_TABLE` 导出路径、
  `PlanFrom` 的 `Table` 校验、README 里"1250 会镜像成 VPP 表"的说明、判据脚本的播种。
- 升级动作：停旧 sr-frr（正常退出会逐条撤掉 TABLE 路由）→ 清 1250 → 换二进制 + 新配置。
  这是一次**计划内的进程重启**，不属于"配置生效路径"；中断只影响被泄露前缀的宣告，
  实测窗口写进提交说明。之后所有配置变更都不再重启。
- 新 sr-frr 遇到旧键（`vrfs`/`import`/`distance`/`metric`）时**照常启动**，但把 export 与 import
  都置空，并打一条显眼的 warning（"旧 schema，等控制器重写"）。不拒绝启动（避免先换二进制、
  还没发布时盒子起不来），也不偷偷沿用旧行为（1250 那条路径已经删了，沿用只会是假象）。
  这个识别只保留一个版本，下一次升级删掉。
- 升级顺序：控制器的新 bundle 先落盘（给 sr-apply 加一个只在升级时用的 `--persist-only`）→
  停 sr-frr、换二进制、启动 → 之后的发布全走 `PUT /config`。窗口内 SDWAN→CPE 的宣告会消失，
  与上面的进程重启是同一段中断。

### 5.6 注入的所有权安全（2026-11-02 实测）

注入曾经用 `NLM_F_CREATE|NLM_F_REPLACE`，在 lab 上踩出两个问题，现在的契约就是按这两条收的：

1. **顶掉盒子自己的 connected 路由**：`10.99.0.0/24 dev eth0` 被同前缀的 K 换成 `dev lo`，
   地址还在、路由没了，盒子到 RR 的控制面当场断。现在的规则：注入前先看
   `LocalSubnets(table)`（VPN 0 = 没挂 VRF 的接口；VPN N = `vpn<N>` 与它的从属接口），
   接口网段一律跳过；EEXIST 时用 `Owns`（proto=BOOT、scope=LINK、type=unicast、出口 ifindex 一致）
   判断归属，不是我们的一律不碰。跳过的前缀进日志（集合变化时打一行）与 `/status.skipped`。
2. **zebra 累积重复 K 条目，撤不掉**：旧路径每次对账都 replace 一次已存在的内核路由，
   zebra 每次长出一条新的 RIB 条目（实测同一前缀两条，uptime 对应两次重启，`nexthopGroupId` 相同）。
   后来删除只删掉一条，另一条仍是 `Installed`，BGP 于是**永远不向客户撤**。
   新路径只在路由不存在时写内核；杀进程留下的残留由 `Owns` 收编，不再产生任何内核写。

实测（lab-cpe1/2/3，FRR 10.7.1）：

| 场景 | 结果 |
| --- | --- |
| 5 轮 `PUT /config` 导出开/关 | 每轮 K=4~5、zebra 每前缀 1 条，重复 0 |
| `kill -9` 后重启（状态文件在/不在各一次） | 收编 4~5 条，重复 0，客户侧计数无跳变 |
| 撤回导出（客户侧 /proc/net/route 5 ms 采样） | 5 → 0 单次跳变，2.46 s（含 1 s 轮询）；收编后再撤 1.98 s |
| cpe2 从 1250 迁移到 K（该迁移必然换注入路径） | 客户 5 → 0 → 5，窗口 **5.54 s**；bgpd pid 990 不变，reload 复检 0 差异 |
| cpe1/cpe3 换二进制（`kill -9` + 收编） | 客户计数无跳变（采样零次变化） |


## 6. 控制器 API 与页面

### 6.1 API

| 方法 | 路径 | 内容 |
| --- | --- | --- |
| GET | `/api/leak` | prefix-list 库 + 每台设备的 VPN 行（含是否在导通组、有没有 bgp/ospf 实例）+ 检查 |
| PUT | `/api/leak/rows` | 单行保存（设备 + 方向 + VPN + 协议 + 列表） |
| POST | `/api/leak/batch` | 模版批量下发（§7） |
| POST | `/api/leak/republish` | prefix-list 行的一键重发：入参 `prefix_list_id`，服务端展开引用它的设备，走 §7 同一套预览/检查/发布 |
| POST/PUT/DELETE | `/api/prefix-lists[/id]` | 本租户的库（不跨租户）；被引用时删除返回 409 |

**发布复用现有端点，不新增第二条发布路径**：`POST /api/config/publish` 已经按 `node_id` 把整份
bundle（FRR / VPP / Boot / route.json / 第 5 段 srfrr）冻结并下发。泄露行存在 `DeviceConfig` 上，
所以泄露页的"发布到设备"就是带 `node_id` 调这一个端点，与「基础配置」页的发布是同一件事、
同一份版本号。`/api/leak/republish` 只是"多台设备各调一次"的编排，不落新实体。

GET 的返回**不造数据**：设备没上报就说没上报，实例有没有以基础配置草稿为准并注明"草稿"。

### 6.2 页面

导航：`Overlay 配置` 下把第 323 行那个不可点的占位换成两个子项，按你给的顺序：

- `路由泄露 · CPE-SDWAN`
- `路由泄露 · SDWAN-CPE`

两页同一套骨架：

1. 页首：prefix-list 库（**本租户的**；列表名 + 条目编辑 + 新建；两页看到的是同一份）。
   每个列表一行尾部两个动作：`编辑`、`重新发布引用它的 N 台设备`（N=0 时置灰；
   动作走 `/api/leak/republish`，先弹出预览——哪些设备、哪些行、为什么保留——确认后逐台发布）。
2. 设备列表：顶部输入框过滤（按现有规范：输入过滤 + 折叠下拉/列表，不铺全量）。
3. 每个设备展开它的 VPN 行：VPN 下拉 → 协议复选框 → prefix-list 下拉。
   - 只有该 VPN 真的有的协议才可勾；没有的置灰并写原因（"这个 VPN 没有 OSPF 实例"）。
   - VPN 不在导通组里：该行不可保存，写明"先在「导通策略配置」把这个 VPN 加进至少一个组"。
   - VPN 250 / >255 不出现。
4. 每台设备一个**「发布到设备」**按钮（不是只读状态）：调 `POST /api/config/publish`
   带该 `node_id`，与「基础配置」页的发布是同一个端点、同一份冻结包、同一个版本号；
   页脚显示已发布版本、下发确认状态、最近一次失败原因。
   - 这道按钮就是"需重新发布"的出口：行上的标记本身可点，点了等同点本设备的
     「发布到设备」。
5. 列表内容在本设备发布后被改过：该行标"prefix-list 已更新，需重新发布"（可点，见第 4 条），
   不自动下发；同一份提示也反映在 prefix-list 行的"N 台设备待重发"计数里。
6. 发布成功后该设备的已发布版本号 +1，行上的"需重新发布"消失；没有保存过草稿的设备
   （`v0`）调发布会返回 409，页面直接把原因显示在设备行上，不做静默重试。

## 7. 批量模版

两个模块各一个"模版批量下发"：

- 字段：多选设备（可过滤）、多选 VPN、多选协议（CPE-SDWAN 四个 / SDWAN-CPE 两个）、
  单选 prefix-list（SDWAN-CPE 允许"不挂"）。
- 展开规则（按你的口径）：
  - 设备没有该 VPN → 去掉该 VPN；
  - 设备该 VPN 没有该协议 → 去掉该协议（SDWAN-CPE 的 bgp/ospf；CPE-SDWAN 的 bgp/ospf；
    `static`/`connected` 不需要实例，只要求 VPN 存在）；
  - 协议全被去掉 → 这个 VPN 整行不下发；
- 交互：先"预览"（逐设备列出保留/丢弃及原因），确认后**先检查、后写入**：任何一台的发布检查
  不过就整批不写（不会一半设备生效）；全部通过则逐台写草稿并**发布**（调 `/api/config/publish`，
  与设备行的「发布到设备」同一条路径，不是另一套发布语义）。
- 模版是**一次性的批量表单**，不落成可复用实体（避免多一套生命周期）。要复用就把这次的结果
  当普通行继续编辑。

## 8. 校验闸门（保存与发布都跑）

1. VPN 在设备基础配置里存在；0..255；≠250。
2. VPN 至少在租户的一个导通组里（route.json 有它、RT 存在）。
3. 协议：SDWAN-CPE 只能 `bgp/ospf` 且实例已启用；CPE-SDWAN 的 `bgp/ospf` 同样要求实例，
   `static/connected` 不要求。
4. 至少一个协议；否则该行等于不存在（保存时清掉，批量时跳过并报告）。
5. prefix-list 存在、条目规范（`ge/le` 合法）；CPE-SDWAN 必须选。
6. 同一设备同一方向同一 VPN 只能一行。
7. 发布时 prefix-list 的内容按当时快照冻结；引用的列表之后被删不追溯已发布设备（但再发布会被第 5 条挡住）。

## 9. 分步与判据（每步单独验证、单独提交）

| 步 | 内容 | 判据 |
| --- | --- | --- |
| S-0 | spike：ZAPI 通知头 `vrf_id` 是否可用、四个来源的类型号、OSPF route-map 前缀匹配、注入后是否产生 BGP 自导入 | **已完成**，结论在 §1.1；不改生产代码 |
| L-1 | 控制器：prefix-list 库 + 每设备泄露行 + API + 校验 + 单测 | 单测覆盖归一化、引用检查、导通组/协议/VPN 闸门 |
| L-2 | 渲染：`renderFRR` 出 SDWAN-CPE 三件套；第 5 段 `srfrr` 出 `export_vrfs`/`import_rules` | 逐行快照测试；空规则不出任何行、不进 export_vrfs |
| L-3 | sr-frr：netlink 注入 + 状态文件 + 退出清理；退役 1250/ZAPI 导出 | lab：`K>*` 存在、VPP 对应表无新增、sr-frr 重启后按状态文件对账（ESRCH 容忍）；撤行后 K 与客户 BGP 都撤掉 |
| L-4 | sr-frr：多来源/多 VPN 导入 + 白名单 + 防回环 + `PUT /config`、`GET /status`；sr-apply 落盘+推送 | sr-frr pid/启动时刻不变；改来源/列表后 1 个轮询内上报集合变化；退出清理仍能撤干净 |
| L-5 | 控制台两页 + prefix-list 编辑器 + 批量模版 + 三处发布入口（设备行 / 列表行 / 批量） | 真浏览器点完一遍；语法检查；设备缺 VPN/协议时的丢弃原因可见；三处发布走同一个 `/api/config/publish`，版本号连续且不重复下发 |
| L-6 | lab 端到端：两台盒子 + RR；客户前缀按来源/白名单上行；远端前缀经 K 进客户 BGP；同前缀与回环 | 前后两组数字（BGP 前缀数、FRR K 条数、VPP FIB 条数、sr-frr pid、邻居 uptime）；VPP FIB 不因 K 变化 |
| L-7 ✅ | 退役收尾：本端前缀只走 `PUT /local-prefixes`，删掉配置里的那个键与旧 README/脚本/判据；#185 关闭（形态不再存在） | `git grep` 零命中；盒子重启后开机重放仍正确 |

### L-6 实测结果（2026-11-02，lab-cpe1/2/3 + lab-cust1/2）

- **基线**：`test/lab/verify.sh` 313 通过 / 0 失败；升级 sr-frr 到带链路监听的版本后仍是
  313/0，三次升级全程 sr-frr / sr-route / FRR pid 不变。
- **多 VRF（以前从没测过）**：`test/lab/test-multivrf.sh` 120 通过 / 0 失败（D3 补齐 sr-fib
  `PUT /config` 后重跑）。VPN 0/100/101
  同时在线：sr-route RIB 各 4 条、盒内核 table 100/101 各 4 条 K（出口 vpn100/vpn101）、
  zebra 的 VRF 实例各 4 条 `K>*`、BGP VRF 实例各 4 条；同一前缀 `10.200.N.0/24` 在 100/101
  里各一份且出口不同；主表 254 没有任何 100/101/200 前缀；VPP table 100 的远端路径不是
  dpo-drop（K 没有盖掉 sr-fib）。
- **touch 窗口**（§5.3 的待量项）：`PUT /config` + `refresh_vrfs:[0,100,101]` 原地重刷，
  `ip -ts monitor route` 逐条测 4 条前缀的 del→add：**211 / 5 / 6 / 5 µs**（最坏 211µs，
  2026-11-02 重跑）。
  sr-frr pid、配置指纹、客户侧 BGP vrf 表版本号全程不变（客户看不到撤/发）。
  结论：维持"只 touch 协议集合变化的 VPN"，不需要页面点名中断，也不需要额外的降级方案。
- **sr-fib 装 VPN 100/101**：D3 已补 `PUT /config`（只改内存，不写盘、不重启）。三台各自
  200、`/status` 里 `converged=true`、pid 不变；热更窗口 lab-term1 → term2 800 包
  （10ms 间隔）：**0% 丢包**。第一次实测窗口撞上 lab 的 BFD 假 down（800 里丢 90、
  seq 655–746 连续缺，与 cpe1 同一秒的 24 条路由撤离同刻），脚本重测后 0/800；
  这次也修了脚本里 `grep -o '[0-9]*% packet loss'` 把 `11.25%` 读成 `25%` 的解析 bug。
- **sr-frr 升级本身**：customer BGP 侧每 15–20ms 采一次可见前缀，0 次撤/发。
- **zebra 的两个坑**（均已收口/记录，不靠重启绕过）：
  1. 接口 down、删接口、删地址会让 zebra 清掉自己 RIB 里的**全部** K 路由（内核里还在、
     sr-frr 仍报 `injected=N`），`redistribute kernel` 从此静默停发。sr-frr 现在订阅这三类
     链路事件（300ms 去抖）后自动 touch 重放，实测删 VRF 设备后 zebra 仍见 4 条 K。
  2. K 路由的 delete 不会摘掉 zebra RIB 里的条目，随后的 re-add 会多出一条非选中的
     `K *`（每个前缀最多 2 条，不累积；BGP 只宣告一条）。属观感问题，文档记录，不重启 zebra。
- **lab 环境噪声**：共享 CPU 的 VM 里 BFD 每分钟 1–3 次漏检（约 1s 自愈），
  `verify.sh` 的 FIB 断言改成最多等 10s 收敛，避免把环境噪声记成产品失败。

### L-7 实测结果（2026-11-02，lab-cpe3）

- **零命中**：`git grep` 在跟踪文件里已找不到那个键；剩下的命中只在未跟踪的构建产物
  （`controller/bin/sr-route`、`test/lab/run/bin/sr-route`）里。
- **负向**：把那个键放回 `route.json` 的副本再起新 sr-route，拒绝启动并说清原因：
  `config: parse /tmp/route-legacy-key.json: json: unknown field "local_prefixes"`，`rc=1`。
  也就是说"配置里留旧键"不会再被静默当成空集合吃掉。
- **重放**（路由面按清单顺序冷启：route → frrbridge；VPP/FRR/隧道不动——这次改动不碰
  数据面，而重启整盒要用 bringup 重建 lab）：基线本端前缀 2 条（`10.68.3.0/24`、
  `172.16.3.1/32`）；新 sr-route 起来时 `/local-prefixes` 是 **0 条**，随后从 RR 同步
  RIB 4 条；sr-frr 起来后 **1061ms**（一个轮询）把 2 条推回，控制口
  `reported=2 / injected=4 / dropped_loop=0 / last_error=""`。对端视角的撤/回：
  cpe1 在 12:11:35 看到 0 条、12:11:42 回到 2 条（窗口含人工停两个进程的时刻）；
  cpe1/cpe2 的 RIB 里两条前缀回到原下一跳，cust3 的 BGP 6 条、cpe3 内核 K 4 条。
  sr-fib 全程"保留上次成功的计划"（只打两条 connection refused，恢复后无重装），
  所以 VPP FIB 不因这次冷启抖动。
- **顺手退役 1250 的保留判据**：那条 table-direct 路径在 L-3 已拆掉，后端校验、
  控制台文案与 VPN 落表说明里的 1250 一起删掉，避免"后端允许、页面拒绝"的分叉。
- **lab 体检**：`test/lab/verify.sh` 313 通过 / 0 失败。


## 10. 不做

- 不做 prefix 级 A→B（per-prefix RT）——那是 O6-f。
- 不做 prefix-list 的 deny 条目。
- 不做"保存的模版实体"。
- 不在配置生效路径里重启任何 daemon；不保留 1250 或 ZAPI table-direct 的 fallback。
- 不把 overlay 前缀塞进 VPP 的普通转发路径（K 只给控制面看）。

## 11. 已拍与剩余待拍

已拍（2026-11-02）：

1. 批量下发 = **一步发布**：预览 → 整批检查 → 写草稿并逐台发布；任何一台不过整批不写。
2. prefix-list **只做 permit**。
3. 列表内容改了之后**不自动重发**，但在三个地方给出可见的发布入口：
   设备行的「发布到设备」、列表行的「重新发布引用它的 N 台设备」、批量模版的一次性下发（§6.2/§7）。
   "需重新发布"这个标记本身可点，点了就是发布本设备。
4. prefix-list 是**每个租户自己的**配置（`State.LeakConfigs[tenantID]`），不跨租户共享；
   跨租户共享的对象属于运营商视图，等你点名再做。租户内两个模块共用同一份。

S-0 已把原第 1 项解掉：`redistribute kernel route-map` + prefix-list 在 VRF 里过滤生效
（两条 K 只出来了被允许的那一条，见 §1.1 ③），不需要 OSPF 侧降级。

剩余待拍：

（无。原第 1 项 §5.3 的 touch 窗口已于 2026-11-02 实测：最坏 174µs、客户 BGP 版本号不动，
不需要降级也不需要页面点名。）
