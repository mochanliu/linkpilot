# 三站点复杂 lab（单容器盒子：VPP + 盒内 FRR + 自研控制面）

一张网里同时放进了已经落地的每一层：入网、控制面反射、VPP 转发、MPLS over
WireGuard、盒内 FRR 的两个方向联动。目标是登进任意一个终端，从 10.68.N.10
一路追到另一个站点的 10.68.M.10 或客户路由器 loopback，而不是只看单点命令
输出。

**每个 `lab-cpeN` 就是一台盒子**：一边是 VPP 数据面，另一边是盒内 FRR 与
sr-route / sr-fib / sr-frr。盒内 FRR 的目的只有一个——和客户侧路由器跑路由
协议；自研控制面与 FRR 之间由 sr-frr 双向搬运：

- **正向**：sr-route RIB → sr-frr → FRR `table 1250` → `redistribute
  table-direct 1250` → BGP → 客户路由器。
- **反向**：客户路由器 eBGP → 盒内 FRR（入方向 route-map 打 `tag 1001`）→
  sr-frr 只收 tag 1001 → `PUT /local-prefixes` → sr-route local → RR 反射 →
  远端 CPE 的 RIB / 带标签 FIB。

**这里不开发控制器。** 按 2026-10-25 的指示，凡是由控制器下发的值（每隧道
UDP 端口、overlay /32、隧道计划、VRF/RT、FRR 旁路表号）都在
[bringup.sh](bringup.sh) 里手写；sr-ctrl 只做它已经做好的那件事——入网签发
设备证书。

## 拓扑

```
站点 N（N = 1..3）：

  lab-termN           lab-custN                      lab-cpeN（单容器盒子）
  10.68.N.10 ──LAN──  10.68.N.2 ──xconn── 10.79.N.2
                      (AS 6500N)                      盒内核
                      lo 172.16.N.1                     ├─ vti0 10.78.N.20：盒内 FRR
                                                        ├─ 默认 0.0.0.0/0 → 10.78.N.2
                                                        └─ vti1（交给 VPP）
                                                              │ 容器内 veth
                                                        ┌─────┴──────────────────────────┐
                                                        │ host-vti1 10.78.N.2 table 250  │ VPP
                                                        │ wg0/wg1 ← WAN 10.88.1.1N       │
                                                        └───────────────┬────────────────┘
                                                                        └── SR-over-WG ── lab-transit 10.88.1.3
```

盒内核把默认路由交给 VPP 的 10.78.N.2；VPP 在 table 250 里决定：远端前缀压
标签 266 走 wg 接口，本机前缀不压标签、交还给盒内 FRR（10.78.N.20）。

控制面（管理网 10.99.0.0/24，不进数据面）：

```
lab-ctrl 10.99.0.200:8444   sr-ctrl（入网 / CA / 设备证书）
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

1. 每台 CPE 的 inside 口在 **VPP table 250**（VRF 250，VPN 标签 16+250=266）。
2. 客户路由器宣告自己的 LAN（10.68.N.0/24）和 loopback（172.16.N.1/32）。
3. 盒内 FRR 的入方向 route-map 给客户路由打 `tag 1001`；sr-frr 只收带这个
   tag 的路由，经 `PUT /local-prefixes` 变成 sr-route 的 local 集合。
4. sr-route 把 local 集合（本站 LAN + 从客户学到的前缀）经 lab-rr 反射给
   另外两个站点。
5. sr-fib 把远端前缀装成「压 266、走对应 wg 接口、下一跳是对端 overlay」；
   本机 local 前缀不压标签，下一跳是盒内 FRR（10.78.N.20）。
6. 反方向：sr-frr 把控制面 RIB 导成 FRR **table 1250**，
   `redistribute table-direct 1250` 让它们进 BGP；客户路由器于是能看到
   远端站点的 LAN 与 loopback。
7. 客户侧终端只需要 `default via 10.68.N.2`（客户路由器）。

防环：

- 我们自己导出的 table 1250 路由回到 BGP 时 tag 是 0，sr-frr 只收 tag 1001，
  所以不会反向再吃一遍；这些前缀也只停在 table 1250，不进 zebra 主表。
- 客户也宣告本站 LAN 时，sr-route 的 local 集合按前缀去重（静态 local 与
  动态导入合并），不会出现两条同前缀。

## 起停

```bash
test/lab/bringup.sh            # 全量重建（约几十秒，取决于镜像本地是否已有）
test/lab/verify.sh             # 端到端体检，非 0 退出表示有失败项
test/lab/status.sh             # 只读快照
test/lab/teardown.sh           # 拆掉 lab-*，不碰 srwg-*
test/lab/teardown.sh --purge   # 连 test/lab/run/（含一次性 CA）一起删
```

分阶段调试（阶段之间可反复重跑，配置是幂等的）：

```bash
test/lab/bringup.sh --stage containers   # 建容器/网络、veth、FRR 配置、客户路由器
test/lab/bringup.sh --stage vpp          # 4 个 VPP（3 CPE + transit）
test/lab/bringup.sh --stage enroll       # 编译二进制、sr-ctrl、三台入网、RR 证书
test/lab/bringup.sh --stage dataplane    # underlay/inside、WG、端口、SR 路径、peer
test/lab/bringup.sh --stage control      # sr-route + sr-fib + table 250
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

# 盒内 FRR（同一个 lab-cpe1 容器里）
docker exec -it lab-cpe1 vtysh
lab-cpe1# show ip route table 1250    # sr-frr 正向导出的远端前缀
lab-cpe1# show bgp ipv4 unicast       # 含客户路由（tag 1001 的那个）
lab-cpe1# show ip route 172.16.1.1/32 # 客户 loopback，带 tag 1001
lab-cpe1# show ip route 0.0.0.0/0     # 指回 VPP 10.78.1.2

# CPE 数据面
docker exec -it lab-cpe1 vppctl
vppctl# show sr-wg ports
vppctl# show sr-wg paths
vppctl# show sr-wg tunnels
vppctl# show wireguard peer
vppctl# show ip fib table 250          # 远端带标签 266，本地 via 10.78.1.20
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
| [verify.sh](verify.sh) | 从容器/VPP 一直到终端到终端、双向导入的 200+ 条断言 |
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
- **CPE 的 inside 地址用 10.78.N.2 而不是 .1**：docker 网桥自己占着子网的
  .1，会跟 VPP 抢同一个身份（ARP/ICMP）。
- **盒内核的转发开关要按接口开/关**：`start-node.sh` 启动 VPP 时会
  `ip_forward=0`（AF_PACKET host 接口仍把包投给内核，全开会重复转发并撞 WG
  重放窗）。bringup 在 VPP 起来后改回 `ip_forward=1`，但只对 xconn 与 vti0
  打开 per-interface forwarding，wan/mgmt/vti1 保持 0。
- **建 WG 接口 → 登记端口 → `set sr-wg rebind-ports`**，顺序不能反：建接口
  会把 listen-port 抢回 wg4-input。
- **encap 挂在 underlay 出口接口上**（`host-<wan>`），不是 wg0；peer 一定
  最后加，否则握手包在 encap 就位前发出。
- **overlay /32 路由要排在 peer add 之后**，否则 FIB 里是 unresolved，转发
  落到 dpo-drop（接口 up、peer established 也一样不通）。
- **zebra 的 static 默认路由只有一条且 distance 1**：docker 注入的内核默认
  路由必须删掉（`ip route del default`），否则 FRR 的默认路由装不进内核，
  盒子自己发起的流量出不去。
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
- **BFD 必须 multihop**：绑在接口上的会话会让 overlay 路由变成 dpo-drop
  （见 test/scripts/test-fib.sh 的实测记录）。
