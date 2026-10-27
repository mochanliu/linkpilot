# 三站点复杂 lab（单容器盒子：VPP + 盒内 FRR + 自研控制面）

一张网里同时放进了已经落地的每一层：入网、控制面反射、VPP 转发、MPLS over
WireGuard、盒内 FRR 的两个方向联动。目标是登进任意一个终端，从 10.68.N.10
一路追到另一个站点的 10.68.M.10 或客户路由器 loopback，而不是只看单点命令
输出。

**每个 `lab-cpeN` 就是一台盒子**：VPP 独占数据面（WAN 口和客户口都在 VPP
里，**盒内核不转发任何包**），盒内 FRR 只当用户态控制面算路由，sr-route /
sr-fib / sr-frr 把两套路由体系接起来。FRR 和 VPP 的接缝是 VPP 的
`linux-cp` 插件：客户口在 VPP 里，同时被镜像成内核 TAP `lcp-x0`，BGP 就跑在
这条 TAP 上；FRR 算出的路由由 zebra 装进内核主表，`linux-nl` 插件再把内核表
1:1 镜像进 VPP，于是**转发的每一跳都由 VPP 完成**：

- **正向**：sr-route RIB → sr-frr → FRR `table 1250` → `redistribute
  table-direct 1250` → BGP → 客户路由器。
- **反向**：客户路由器 eBGP → 盒内 FRR（入方向 route-map 打 `tag 1001`）→
  sr-frr 只收 tag 1001 → `PUT /local-prefixes` → sr-route local → RR 反射 →
  远端 CPE 的 RIB / 带标签 FIB。
- **落地**：远端前缀由 sr-fib 直接装 VPP（压标签 1000 走 wg）；本地前缀由
  FRR 装内核主表（下一跳 = 客户路由器、出口 = `lcp-x0`），linux-nl 镜像成
  VPP table 0 里「via <客户路由器> <客户口>」。

**控制器只做入网，不做下发。** 2026-10-26 起控制器增量按用户逐项批准推进，
但 lab 的规矩没变：凡是由控制器下发的值（每隧道 UDP 端口、overlay /32、隧道
计划、VRF/RT、FRR 旁路表号）仍然在 [bringup.sh](bringup.sh) 里手写。
sr-ctrl 现在有两件事——签发设备证书（`/v1/*`，mTLS :8444，设备用）和给客户
看的控制台（明文 :8080，浏览器用）；入网应答只有身份，没有任何配置。

## 拓扑

```
站点 N（N = 1..3）：

  lab-termN           lab-custN                    lab-cpeN（单容器盒子）
  10.68.N.10 ──LAN──  10.68.N.2 ──xconn── VPP 客户口 10.79.N.2（table 0）
                      (AS 6500N)            │
                      lo 172.16.N.1         │  linux-cp 把客户口镜像成内核 TAP
                                            ├─ 盒内核 lcp-x0 10.79.N.2：只跑 BGP
                                            │    （不转发，ip_forward=0，无默认路由）
                                            │         │ zebra 装内核主表
                                            │         │ linux-nl 镜像
                                            └─────────┴── VPP table 0（转发全部数据）
                                                          wg0/wg1 ← WAN 10.88.1.1N
                                                          （wg 接口在 table 250）
                                                              └── SR-over-WG ── lab-transit 10.88.1.3
```

客户 VPN 0 与 underlay 共用 VPP **table 0**：远端前缀压标签 1000 走 wg
接口（wg 接口本身住在 overlay table 250），本站/客户前缀不压标签、下一跳是
客户路由器（`via 10.79.N.3 host-ethX`）。盒内核只是 FRR 的计算现场，不参与
任何转发。

控制面（管理网 10.99.0.0/24，不进数据面）：

```
lab-ctrl 10.99.0.200:8444   sr-ctrl（入网 / CA / 设备证书）
         127.0.0.1:8088     sr-ctrl 控制台（宿主机回环 → 容器 8080，账号或运营令牌换会话）
lab-rr   10.99.0.100:18446  sr-rr（tenant-a 的反射器）
lab-cpeN 10.99.0.1N         sr-route /status（1900N，sr-fib/sr-frr 从这里取）
lab-custN 10.99.0.2N        BGP router-id
```

隧道（每对一条、每隧道一个端口——本 lab 里两端配成同号，便于肉眼对表——
独立 overlay /32，全部经 transit 两跳）：

| 隧道 | A 端 | B 端 | transit 端口 |
|------|------|------|--------------|
| T12 | cpe1 wg0 51820 / 192.168.99.1 | cpe2 wg0 51820 / 192.168.99.2 | 51821 |
| T13 | cpe1 wg1 51840 / 192.168.100.1 | cpe3 wg0 51840 / 192.168.100.2 | 51841 |
| T23 | cpe2 wg1 51860 / 192.168.101.1 | cpe3 wg1 51860 / 192.168.101.2 | 51861 |

两端端口不必相同：encap 的隧道身份来自绑定表 `(对端 endpoint, 对端 dst_port)`，
与**本机** vpn_ports 无关（vpn_ports 只决定收包进 `sr-wg4-input` 还是
`wg4-input`）。本 lab 用同号只是排版方便；非对称端口的回归在
`test/scripts/test-multitunnel.sh`（node1 51830 / node2 51840）。

路由链路：

1. 客户 VPN 0 用 **VPP table 0**（和 underlay 同表）；VPN 标签 = 基址 1000 +
   VPN ID = **1000**（16 是 IPv4 显式空标签，不能当 VPN 0）。wg 接口与
   overlay /32 在 **table 250**——250 是 SD-WAN 传输域，不是 VPN，VPN ID
   保留 250。
2. 客户路由器宣告自己的 LAN（10.68.N.0/24）和 loopback（172.16.N.1/32）。
3. 盒内 FRR 的入方向 route-map 给客户路由打 `tag 1001`；sr-frr 只收带这个
   tag 的路由，经 `PUT /local-prefixes` 变成 sr-route 的 local 集合。
4. sr-route 把 local 集合（本站 LAN + 从客户学到的前缀）经 lab-rr 反射给
   另外两个站点。
5. sr-fib 只装**远端**前缀：压 1000、走对应 wg 接口、下一跳是对端 overlay。
   本站/客户前缀归 FRR：zebra 装内核主表（`via 10.79.N.3 dev lcp-x0`），
   linux-nl 镜像进 VPP table 0，下一跳是客户路由器、不打标签。
6. 反方向：sr-frr 把控制面 RIB 导成 FRR **table 1250**（黑洞下一跳），
   `redistribute table-direct 1250` 让它们进 BGP；客户路由器于是能看到
   远端站点的 LAN 与 loopback。这张表也会被 linux-nl 镜像成 VPP table
   1250，但全是 `dpo-drop`——它只用于再分发，不参与转发。
7. 客户侧终端只需要 `default via 10.68.N.2`（客户路由器）。

防环：

- 我们自己导出的 table 1250 路由回到 BGP 时 tag 是 0，sr-frr 只收 tag 1001，
  所以不会反向再吃一遍；这些前缀也只停在 table 1250，不进 zebra 主表。
- 客户也宣告本站 LAN 时，sr-route 的 local 集合按前缀去重（静态 local 与
  动态导入合并），不会出现两条同前缀。
- linux-nl 只镜像内核表里的路由：table 0 的本地前缀来自 FRR（下一跳客户
  路由器），table 1250 是黑洞停车场的镜像，两者都在 VPP 里保持"转发由 VPP
  做"这一条边界。

## 起停

```bash
test/lab/bringup.sh            # 全量重建（约几十秒，取决于镜像本地是否已有）
test/lab/verify.sh             # 端到端体检，非 0 退出表示有失败项
test/lab/status.sh             # 只读快照
test/lab/teardown.sh           # 拆掉 lab-*，不碰 srwg-*
test/lab/teardown.sh --purge   # 连 test/lab/run/（含一次性 CA）一起删
```

## 控制台与 vCPE 入网（GUI 全流程）

控制台有两种登录方式、两个角色：

| 角色 | 登录方式 | 能做什么 | 看不到什么 |
| --- | --- | --- | --- |
| 运营方 operator | 运营令牌（应急）/ operator 账号 | 建租户、下发/吊销入网授权、调整可激活台数、管理账号 | 任何入网码明文（授权里没有码）；设备的任何配置（`/api/state` 里没有 `runtime`、端口）；租户的监控视图 |
| 客户 tenant | 租户账号（用户名+口令） | 看自己的 CPE、自己的授权与名额、生成一次性入网码、enroll 设备、看设备健康与隧道内遥测、改自己的口令 | 别家租户的一切；运营方接口一律 403 |

建租户时可以同时开该租户的第一个账号；不给口令就由服务端生成一次性口令
（`XXXX-XXXX-XXXX-XXXX`，只在创建应答里出现一次，登录后页面提示改密）。
权限在服务端收口：`/api/state` 按租户过滤，SSE 订阅也按租户过滤；页面只是
按角色显隐导航，隐藏的接口用程序点也是 403。

入网是两级模型：

1. **授权**由运营方下发，只有名称（如「现场设备」）和可激活台数，**没有码**。
   默认 10 台、上限 10000；台数只在新设备激活时消耗，同一台设备重入（换证书、
   重启、恢复出厂）不占名额——所以名额用满后仍可生成码，否则恢复出厂就再也进不来。
2. **码**由租户登录后自己点「生成」得到：先选窗口（默认 5 分钟，可 1 小时 / 24 小时），
   响应里给出 `srk_` + 32 位无歧义字符（约 157 bit），明文只出现这一次，落库只有
   SHA-256。码不含租户名，无法从租户 ID 推出。

同一授权同时只有一个有效码：重新生成会立刻让旧码失效；吊销授权同样清掉当前码。
过期由服务端清扫（默认 300 秒、上限 86400 秒），入网时也会再查一次截止时间。
运营方在页面上只看到「有效 / 已过期 / 未生成」和剩余时间，看不到码本身，也不能
替客户生成（`POST /api/claims/code` 对 operator 是 403）。

台数可以随时调大，但不能调到已激活数以下——那些设备的证书已经发出去了。旧版
`state.json` 里以明文码为键的记录会在加载时迁移成授权（哈希后落库），旧码仍可
入网，磁盘上的明文会被立即重写掉。页面右上角有「修改密码」（账号登录时）和「退出登录」。

控制台是给客户的界面：运营方建租户并下发授权 → 客户登录看到授权 → 点「生成」拿码 →
设备用码入网 → 设备出现在列表里。入网**不下发任何配置**，所以设备行上写的
「未配置」是真实状态。

```bash
test/lab/console.sh            # 打印地址 http://127.0.0.1:8088/ 和运营令牌
test/lab/console.sh restart    # 重启 sr-ctrl（控制台 + 设备面同一个进程）

# 运营方建租户后会下发授权和一个租户账号；客户登录后点「生成」拿码，然后：
test/lab/vcpe.sh up --claim <入网码>   # 起一台只做入网+心跳的 vCPE（10.99.0.60）
test/lab/vcpe.sh status               # 本地身份：tenant / node_id / 心跳次数
test/lab/vcpe.sh logs                 # agent 日志
test/lab/vcpe.sh down                 # 停容器，身份目录保留（下次 up 不用 claim）
test/lab/vcpe.sh wipe                 # 连身份一起删（下次必须重新入网）
```

vCPE 跑的是 `controller/cmd/sr-agent`：入网一次、然后每 5 秒心跳，数据面
状态直接写 "none: enrollment carries no configuration"。它和 `lab-cpeN` 的区别
只是没有 VPP —— 盒子里的 agent 以后会在这里接数据面。

运营令牌是 break-glass：长期运行的生产控制器应该用 operator 账号（口令可改、
可禁用），令牌只留给"账号进不去"的场合。令牌落在
`test/lab/run/console-token`，容器里也有同一份
（`/var/lib/sr-ctrl/console-token`）；容器重建后第一次 bringup 会重新生成。
账号和口令哈希在 `state.json` 里（PBKDF2-HMAC-SHA256），控制台会话在内存里，
重启 sr-ctrl 只会让人重新登录。

## 控制台监控页（租户）

「监控」是租户专用页面，运营方读 `/api/metrics` 会被 403——和 `/api/state`
里不下发 `runtime` 是同一条规则：设备怎么组网、链路质量如何属于客户，不属于运营方。

页面上的每个数都来自控制器真实持有的状态，没有占位数据：

- **设备健康**：状态（online / stale / pending）、最后心跳年龄、agent 上次报告的
  进程运行时长、Agent 版本、反射器观测到的外层映射、VPN 端口。存活由心跳判定：
  `max(3 × 心跳间隔, 6 秒)` 没有心跳即失联；失联设备仍留在表里（「哪台掉了」正是
  这张表要回答的问题），但它的遥测窗口同时作废。
- **隧道内遥测**：每个节点一块，每个隧道一行，来自设备通过 `/v1/metrics` 上报的
  最新窗口（BFD 状态、RTT 当前/min/avg/max、抖动、丢包、探针 tx/rx/丢/晚）。
  同一隧道的两端**不合并**：一端说丢包、另一端说 down，这个分歧本身就是诊断信息。
  窗口 `--metrics-ttl`（默认 30 秒）没有刷新就从视图消失。

现在页面上的遥测块会显示「尚未上报遥测」，这是真实状态而不是 bug：CPE 侧还没有
采集并上报 `MetricsReport` 的 agent（`internal/agent/client.go` 有接口，`cmd/` 里
没有调用方），而且节点计划尚未下发，盒子上通常也还没有隧道。等 agent 接上数据面
后这个页面不需要改代码就会开始显示数据。

失联时控制台会收到 SSE `node.stale` 并立即提示；监控页每 5 秒刷新一次遥测，
其他页面沿用 10 秒状态轮询。

## 控制台配置页（租户）

「配置」把设备配置分成三层：基础配置（客户侧）、underlay（隧道）、overlay
（FRR 与自研路由的导入导出）。这一轮只做基础配置，另外两层只画出边界。

基础配置是**草稿**，不是已下发状态：`/api/config` GET 读、PUT 写，租户专用
（operator 403），落在 `state.json` 的 `configs` 下，键为 `tenant|node`，每次
保存 `version` 加一。页面上的「保存草稿」「重新载入」操作的就是它；保存允许
仍然带错误，响应同时给出 `checks`，因为半成品策略是正常的中间状态。

校验规则（服务端与页面预览同一套）：

- 主机名、接口名、TAP 名（≤15 字符）、IPv4/前缀长度、MTU 范围；
- 同一 VPN 内地址重复、网段相等或重叠；静态路由必须是规范网段；
- eBGP 邻居必须落在该 VPN 某个客户口的直连网段内（TTL 1，否则建不起来）；
  iBGP 允许非直连，但要求直连网段或静态路由能递归到它，未开 next-hop-self
  会给警告；BGP/OSPF router-id 跨 VPN 复用也会提示；
- VPN 250（overlay）与 1250（FRR table-direct）保留，不能配成客户 VPN。

配置归属按实测（lab-cpe1，2026-10-27）划分：FRR 管主机名、接口 IP、静态路由、
BGP/OSPF；VPP 只管接口所在 table、MTU、管理状态与 lcp TAP；内核管 VRF、
TAP 入 VRF 与 sysctl。zebra/staticd 写进内核的路由会被 linux-nl 镜像进 VPP，
两个方向都通，所以接口地址不在 VPP 片段里重复配置。页面「将下发的配置」的
三个产物（FRR / VPP / 开机脚本）就是这三份内容。

**推送通道仍然没有**：agent 现在只做入网与心跳，不消费这份配置，点「下发到设备」
会如实提示这一点。但配置包已经可以手动交付，见下一节。

## 设备侧：配置包安装与开机重放（#88）

控制器把租户的草稿渲染好后，用租户会话导出成配置包（JSON，含 `frr` / `vpp` /
`boot` 三个片段和 `checks`）：

```bash
curl -s -b "sr_ctrl_session=<租户会话>" \
     'http://127.0.0.1:8088/api/config/bundle?node=1' > bundle.json
```

租户专用（operator 403）；跨租户 404；节点还没保存过草稿时 409，因为
`version = 0` 的包一旦装上，盒子会声称自己有一份并不存在的配置。

盒子侧用 `sr-apply` 安装，不需要控制器在线：

```bash
sr-apply apply --bundle bundle.json     # 安装并立即下发
sr-apply boot                           # 开机重放当前版本（systemd 调用）
sr-apply status                         # 版本、sha256、上次 apply/boot 结果
```

`apply` 的顺序是固定的：装包（落盘）→ VPP 片段 → 等 lcp TAP 出现在内核 →
内核片段 → 写 `/etc/frr/frr.conf` → 重启 FRR。先落盘后下发，所以中途失败也
不会出现「设备在跑一份磁盘上没有的配置」。落盘是版本目录 + sha256 清单 +
`current` 符号链接的原子切换（`/var/lib/sr-cpe/versions/<n>`）；同版本同内容
重装是 no-op，同版本不同内容会被拒绝（版本号归控制器，不归盒子）。`checks`
里有 `error` 时拒绝下发，`--force` 是显式的逃生口。

VPP 的 CLI 出错也返回 0，所以每条命令的输出都会被检查；已经存在的 lcp pair
和接口所在 table 会先用 `show lcp` / `show interface address` 问清楚再决定
跳过，而不是忽略报错。`boot` 只重放，不联系控制器；FRR 的配置在安装时已经
写好，开机时 FRR 自己读它。systemd 单元模板在
`controller/deploy/sr-apply.service`：`After=vpp.service`、`Before=frr.service`、
`ExecStartPre` 等 vppctl 的 CLI socket 可用（vppctl 连不上时退出码是 1），
没有安装过版本时 `ConditionPathExists` 直接跳过而不是报失败。

实测（2026-10-27，一次性容器 `apply-probe`，srwg-lab 镜像 + linux-cp/nl）：
重启后 VPP 的 MTU 回到 9000、lcp pair 和 TAP 全部消失；`sr-apply boot` 之后
MTU 1400、pair、`lcp-sync on`、两个 TAP 与 sysctl 都恢复，随后启动 FRR 时
地址和静态路由按已安装的 `frr.conf` 生效，并被 lcp-sync 镜像回 VPP。
第二次 `apply` 会打印 `already paired ... skip` 并成功返回。

**多 VPN 在本 lab 内核上无法验证**：这个 Docker Desktop 的 LinuxKit 内核
（7.0.14-linuxkit）没有 `vrf` 模块（`ip link add name vpn100 type vrf table 100`
报 `Unknown device type`，没有 `/lib/modules`，也没有 `modprobe`）。带 VPN 100
的包会把 VPP 部分做完（`ip table add 100`、接口移进 table 100、lcp pair），
然后在内核片段第一行失败；`sr-apply` 如实返回非 0、`status.ok=false`，并且
不会覆盖 `/etc/frr/frr.conf`。也就是说：自动推送还没做，而多 VPN 的真机路径
（内核 VRF 设备 + 入 VRF + FRR vrf 段）需要在有 `vrf` 模块的宿主上再验一次。

现阶段的设备仍然按手工配置运行。

分阶段调试（阶段之间可反复重跑，配置是幂等的）：

```bash
test/lab/bringup.sh --stage containers   # 建容器/网络、FRR 配置、客户路由器
test/lab/bringup.sh --stage vpp          # 4 个 VPP（3 CPE + transit，含 linux-cp/linux-nl）
test/lab/bringup.sh --stage enroll       # 编译二进制、sr-ctrl（含控制台）、三台入网、RR 证书
test/lab/bringup.sh --stage dataplane    # 客户口交给 VPP + linux-cp、WG、端口、SR 路径、peer
test/lab/bringup.sh --stage control      # sr-route + sr-fib（VPN table 0 / 标签 1000）
test/lab/bringup.sh --stage frr          # sr-frr 正向导出 + 反向导入
test/lab/bringup.sh --stage terms        # 终端默认路由
```

可选：`--bfd` 在隧道内建**多跳** BFD（绑接口的会话会把 overlay 路由打成
dpo-drop）；`--keep` 不先拆现有容器（只用于调试，状态可能是脏的）。

## 登进去测

所有容器都是 `lab-*` 前缀，互不干扰已有的 `srwg-*` 实验环境。

```bash
# 客户视角：终端到终端、终端到对端客户 loopback
docker exec -it lab-term1 ping 10.68.2.10
docker exec -it lab-term1 ping 172.16.2.1

# 客户路由器（这才是真机的对端设备）
docker exec -it lab-cust1 vtysh
lab-cust1# show bgp summary
lab-cust1# show bgp ipv4 unicast      # 应看到另两站的 LAN + loopback
lab-cust1# show ip route              # 远端前缀 B>* via 10.79.1.2

# 盒内 FRR（同一个 lab-cpe1 容器里，BGP 跑在 lcp-x0 上）
docker exec -it lab-cpe1 vtysh
lab-cpe1# show ip route table 1250    # sr-frr 正向导出的远端前缀（黑洞，不转发）
lab-cpe1# show bgp ipv4 unicast       # 含客户路由（tag 1001 的那个）
lab-cpe1# show ip route 172.16.1.1/32 # 客户 loopback，带 tag 1001、via 客户路由器
lab-cpe1# show ip route               # 没有默认路由：盒内核不转发
lab-cpe1# show bgp neighbors 10.79.1.3  # Local host: 10.79.1.2（TAP 上的会话）

# 盒内核（只跑 FRR，不转发）
docker exec -it lab-cpe1 ip route     # 客户路由 via 10.79.1.3 dev lcp-x0
docker exec -it lab-cpe1 cat /proc/sys/net/ipv4/ip_forward   # 0

# CPE 数据面
docker exec -it lab-cpe1 vppctl
vppctl# show lcp                       # host-ethX ↔ lcp-x0 的配对
vppctl# show sr-wg ports
vppctl# show sr-wg paths
vppctl# show sr-wg tunnels
vppctl# show wireguard peer
vppctl# show ip fib table 0            # VPN 0：远端带标签 1000，本地 via 10.79.1.3 host-ethX
vppctl# show ip fib table 250          # overlay：对端 /32 与 wg 接口所在表
vppctl# show ip fib table 1250         # FRR 停车场镜像：全是 dpo-drop
vppctl# show sr-wg stats
vppctl# show trace                     # 先 trace add af-packet-input 100 再打流量

# CPE 控制面
docker exec -it lab-cpe1 curl -s http://127.0.0.1:19001/status | python3 -m json.tool
docker exec -it lab-cpe1 tail -f /var/log/sr-route.log
docker exec -it lab-cpe1 tail -f /var/log/sr-fib.log
docker exec -it lab-cpe1 tail -f /var/log/sr-frr.log

# 中转节点
docker exec -it lab-transit vppctl
vppctl# show sr-wg ports
vppctl# show sr-wg stats
```

抓一次真实转发路径（在 CPE 上）：

```bash
docker exec lab-cpe1 vppctl trace add af-packet-input 50
docker exec lab-term1 ping -c1 10.68.2.10
docker exec lab-cpe1 vppctl show trace
```

## 文件

| 文件 | 作用 |
|------|------|
| [lab-compose.yml](lab-compose.yml) | 12 个容器、8 张网；数据面网络全部关 masquerade |
| [Dockerfile](Dockerfile) | 在 srwg-dev 上装 FRR 10.7.1，做 `srwg-lab:latest` |
| [bringup.sh](bringup.sh) | 分阶段搭建；所有"控制器下发值"手写在这里 |
| [console.sh](console.sh) | 控制台地址/登录 token；重启 sr-ctrl |
| [vcpe.sh](vcpe.sh) | 起停一台只做入网+心跳的 vCPE（容器 `sr-lab-vcpe`） |
| [verify.sh](verify.sh) | 从容器/VPP 一直到终端到终端、双向导入的 290+ 条断言 |
| [status.sh](status.sh) | 只读状态快照 |
| [teardown.sh](teardown.sh) | 拆除 |
| `run/` | 生成的配置、取出的二进制、控制器 CA（.gitignore） |

## 已知坑（都已在脚本里处理）

- **VPP 路由语法是 `ip route add table <id> <prefix> via <nh>`**，表号在
  前缀前面；写成 `via ... table ...` 会 parse error，而 `vppctl` 对 CLI
  解析失败仍返回 0，所以脚本一律用 `show ...` 复核而不是看退出码。
- **数据面网络必须关 masquerade**：WG 的 endpoint roaming 会把被 SNAT 过的
  外层源写进 peer 的 endpoint，之后 encap 再也对不上绑定端点，整条 SR 路径
  静默旁路；BGP 的 TCP 源地址也会被改写。见 [test/docker/docker-compose.yml](../docker/docker-compose.yml)
  里同一段注释。
- **linux-cp / linux-nl 插件必须用带 `.so` 的名字启用**：`plugin
  linux_cp_plugin.so { enable }`。写成不带后缀的名字不会加载，也没有报错，
  `show lcp` 直接不存在。启动配置见
  [test/docker/vpp-startup.conf](../docker/vpp-startup.conf)。
- **客户口在 VPP 里，同时是内核 TAP 的一端**：`lcp create <vpp-if> host-if
  lcp-x0` 会把 VPP 接口的 MAC 换成 TAP 的 MAC；`lcp lcp-sync on` 把 VPP 的
  地址（以及后加的地址）复制到 TAP。FRR 只在这条 TAP 上跑 BGP，`ip_forward`
  保持 0——内核一个数据包都不转发。
- **arp_ignore=1 是数据能通的前提**：AF_PACKET host 接口让内核仍收到一份网卡
  拷贝；地址在 VPP、内核是 weak host，默认 `arp_ignore=0` 时内核会用原始网卡
  的 MAC 回答客户路由器的 ARP，客户把包发给那个 MAC，VPP 在 `ethernet-input`
  按 `l3 mac mismatch` 丢掉——表现是"BGP 通、数据不通"。地址搬进 VPP 后还要
  让客户路由器重学 ARP（`ip neigh flush`，bringup 已代做）。
- **盒内核不是路由器**：没有默认路由（docker 注入的那条要删掉），
  `ip_forward=0`，每个接口 `forwarding=0`。FRR 的客户路由经 zebra 进内核主表，
  只作为 linux-nl 的输入；转发全部在 VPP。
- **linux-nl 只镜像内核里能落地的路由**：`local`(255) 忽略；下一跳经过未配对
  接口的路由忽略；下一跳解析不到也照样装（VPP 里是 unresolved arp 条目）。
  VPP 的 table N 就是内核 table N，不存在别的映射。
- **FRR table 1250 会以 table 1250 出现在 VPP 里**：那是 linux-nl 的镜像，
  全是 `dpo-drop`；它只用于 `redistribute table-direct 1250`，不参与转发。
  别把 VPN 表也规划到 1250（本 lab：VPN 0 = table 0，overlay = 250）。
- **地址搬进 TAP 后要重启 FRR**：eBGP 的 ConnectRetryTimer 是 120 s，不重启
  会等两分钟才从新源地址重建会话。
- **建 WG 接口 → 登记端口 → `set sr-wg rebind-ports`**，顺序不能反：建接口
  会把 listen-port 抢回 wg4-input。
- **encap 挂在 underlay 出口接口上**（`host-<wan>`），不是 wg0；peer 一定
  最后加，否则握手包在 encap 就位前发出。
- **overlay /32 路由要排在 peer add 之后**，否则 FIB 里是 unresolved，转发
  落到 dpo-drop（接口 up、peer established 也一样不通）。
- **zebra 的 static 默认路由现在不需要了**：客户口在 VPP 里之后，盒子本身
  不发起经客户路由器的流量，内核也不该有默认路由（脚本会断言它是空的）。
- **反向上报只认 `tag 1001`**：客户路由必须在 FRR 入方向 route-map 里打
  tag，否则 sr-frr 视为"我们自己导出的路由"丢弃。我们经 table 1250 回到
  BGP 的路由 tag 是 0，正好被同一规则挡在门外。
- **ZAPI v6 的命令号不是"导出那条 +18"**：实测 zebra 发来的
  REDISTRIBUTE_ROUTE_ADD/DEL 是 31/32（中间夹着 CAPABILITIES/BFD_REPLAY）。
  拿错一个会静默丢掉整条反方向的路由，所以 FRR 版本必须 pinned 10.7.1。
- **Go 的 JSON 是紧凑输出**（`"prefix":"..."`，冒号后没有空格），校验状态
  口时用 JSON 解析或容忍空格的匹配，别按格式化后的样子写 grep。
- **FRR 的 frr.conf/daemons 在容器内落盘后 restart**，不用只读 bind mount：
  镜像初始化会处理 /etc/frr 的属主与 vtysh.conf。bgpd 由 `bgpd=no → yes`
  打开。
- **重启 sr-frr 要先等旧进程退出**：它会按 SIGTERM 把 local prefixes 推空，
  和下一个实例的 PUT 抢跑，导致刚导入的前缀又变 0（脚本里用 pkill -x +
  等待）。
- **VPP 不允许接口带地址改表**：`set interface ip table wg0 250` 会报
  "IP addresses are still present on wg0"；必须先摘地址、改表、再重新配地址
  （bringup.sh 的 `wg_to_overlay`）。重复执行时已经在目标表就什么都不做。
- **MPLS 收包后的标签查表恒为全局 MPLS 表**：实测把 wg0 挪到 IP table 250
  之后，VPN 标签仍按全局 MPLS 表弹栈，与接口所在的 IP 表无关——`mpls_table`
  不需要跟着 VPN 走。
- **VPN table 0 的下一跳可以指向 table 250 的 wg 接口**：`ip route add table 0
  … via <对端 overlay> wg0` 解析成 attached-nexthop，跨表 + 显式接口的
  MPLS-over-WG 转发成立（实测 3/3 ping）。
- **BFD 必须 multihop**：绑在接口上的会话会让 overlay 路由变成 dpo-drop
  （见 test/scripts/test-fib.sh 的实测记录）。
