# VM 台：整套 lab 从 Docker 搬到 VirtualBox 的方案

日期：2026-11-04　v2（按你 11-04 的四条决定改过：M1 撤掉独立管理通道、不建 term、不建中转、不配 VRRP）
状态：**开工令已拿到**（"实验环境你随便摆弄"）。本文以你**真机 192.168.1.100 的实测形状**为准，
不是我造的机制。里程碑：**VM 台跑绿并交你复核 ⇒ 你关 docker**。

## 0. 你已经拍掉的（我不当选项重提）

| # | 你的决定 | 落到本方案 |
|---|---|---|
| 1 | 探针先行 | 已跑完（A/B/C 三轮，§2） |
| 2 | 按原计划重建整套 lab，不再用容器 | 容器只做**编译**，运行全在 VM |
| 3 | 控制器跑在 Mac 192.168.1.4 | S2 |
| 4 | **不同意单加管理通道** —— 未来 CPE 也没有管理通道，也是通过 VPP 可达 | §5 整节按真机 `vpp0` 那条路重写，**不加网卡、不动 VirtualBox** |
| 5 | **不需要客户终端**，用 cust 的 loopback 之类代替客户 IP | 不建 term VM（§3、S5） |
| 6 | **不需要中转节点** | 不建 transit VM；两跳中转等以后再单独要（§3） |
| 7 | **这一轮不配 VRRP**，避免扰乱环路测试结果 | §7 边界里写死：站点 12 两台盒子只跑"共享 LAN + 各自路由"，不发 VRRP 配置 |
| 8 | 新环境搭好告诉你，你关 docker | 交付判据 = 我自己能登进去复核 + verify 全绿；**容器回收由你做**，我不 `docker rm` 别人的现场 |

## 1. 台子现状（2026-11-04 逐台取回）

7 台全在跑：Ubuntu 24.04.5 / kernel 6.8.0-147-generic / virtio / 2 vCPU / 2048MB / 根盘 2409MiB。
每台 `nic2=enp0s8` 挂 internal network（客户 LAN）、`nic3=enp0s9` 桥接 Mac 的 en0（WAN）；
`nic1/4..8` 全空 —— **这套网卡数正好就是"无管理通道"的形状，M1 拍完之后一块都不用加。**

| VM | enp0s8 | enp0s9 | 空闲盘 | MemAvailable | VPP/FRR |
|---|---|---|---|---|---|
| CPE1 | lan12 172.31.12.1/24 | 192.168.1.101/24 | 474M | 1636M | 无 / 无 |
| CPE2 | lan12 172.31.12.2/24 | 192.168.1.102/24 | 474M | 1633M | 无 / 无 |
| CPE3 | lan3 172.31.3.1/24 | 192.168.1.103/24 | 474M | 1680M | 无 / 无 |
| CPE4 | lan4 172.31.4.1/24 | 192.168.1.104/24 | **180M** | 1630M | **vpp 26.06 已装**（探针带的）/ 无 |
| cust12 | lan12 172.16.31.12/24 | ——（单网卡） | 472M | 1651M | 无 / 无 |
| cust3 | lan3 172.31.3.2/24 | —— | 486M | 1652M | 无 / 无 |
| cust4 | lan4 172.31.4.2/24 | —— | 486M | 1654M | 无 / 无 |

三条要点：

- **VRF 模块 7 台都在**：`CONFIG_NET_VRF=m`、`/lib/modules/6.8.0-147-generic/kernel/drivers/net/vrf.ko.zst`
  存在、`modinfo vrf` 可解析。当初 lab 搬 colima 就是为这个，这台子不缺。
- **站点 12 的编址现在是手工缝的**：CPE 侧 `172.31.12.0/24`、cust12 `172.16.31.0/24`，
  两边各写了一条 `scope link` 静态路由才通。S5 把它拆掉（改 cust12 到 `172.31.12.13/24`，
  客户 IP 改由 loopback 播 —— 对应你拍的第 5 条）。
- **磁盘账**（CPE4 实测掉了 295M，拿没 `apt update` 过的 CPE1 当基线分了三段）见 §6.2。

## 2. 探针三轮的结论

判据"SSH 全程不掉 = 结论成立"满足，而且绑的是 65.4 秒不是 30 秒。原始证据在
`~/.qoder-tmp/vmlab-probe/20261104-134618/PROBE-REPORT.md`。

1. **接管那一刻不中断。** `create host-interface name <口>` 是 af_packet **旁路**：
   内核照样收帧、IP 和路由原地不动。B 轮 65.4 秒里内核 rx 涨 5136 个包，
   `rx_dropped` 5.0/s（绑定前 4.7/s），Mac 侧 **1646 次 100ms 建连 0 失败**、
   1689 个 ICMP 0 丢失、常驻 SSH 会话 343 个打点最大间隔 0.53s。
2. **致命的是"松开"**：`delete host-interface` 一成功，内核口立刻 `state DOWN` + 地址全清 + 路由归零
   （A 轮 enp0s8、C 轮 enp0s9 两次独立复现）。**把 VPP 打死不会**造成这个后果（B 轮实测完好）。
3. **一次成功的松口：内核侧真黑 0.255 秒，到外部可达 1.14 秒，已建立会话不掉。**
   `312.779` delete rc=0 → `312.839` DOWN+空地址 → `313.034` 4 条地址+2 条路由 replace 完
   → `313.923` 首个 ping 通网关（首包 rtt 833ms）。Mac 侧 1373 次建连 0 失败、
   1386 个 ICMP 丢 1 个（seq 395 换算在松口**前** 0.5s，与窗口内本就有的 9.5ms 抖动同量级，
   **不算成松口造成的**）、一次建连被拖到 101.94ms。
4. **一条脚本写法的坑**：`systemd-run --on-active=33` 晚 32.4 秒才响（两枚定时器同一瞬间开火 =
   `AccuracySec=1min` 网格），`--property=AccuracySec=1us` 被 D-Bus 拒收。
   改成 `systemd-run --collect /bin/bash -c "sleep 25; ..."` 实测 25.06s 开火。
   **⇒ 往后所有"定时开窗/定时兜底"一律用 sleep 任务。**
5. **探针形状 ≠ 目标形状，这条必须说清**：探针是"绑定但保留内核 IP"，
   而 §5 的真机形状是"地址从物理口 flush 掉、落在 LCP 的 TAP 上"。
   **flush 那一下对 SSH 的影响我没测**（当时没有 TAP 这条路），所以 S3 的第一件事就是
   在 CPE4 上把 §5 这条路搭起来并实测 —— 不许拿探针的数字顶替。

## 3. 角色映射：容器 lab → VM（按你拍的第 5、6 条收敛）

| 容器 lab 的角色 | VM 落点 | 说明 |
|---|---|---|
| CPE ×4（VPP 独占数据面 + 盒内 FRR 当控制面） | **CPE1–CPE4** | 台数正好 |
| 客户路由器 cust（FRR，播前缀） | **cust12 / cust3 / cust4** | 单网卡 + **loopback 播客户 IP**（你的第 5 条），不再拆 xconn/LAN |
| RR（`sr-rr`） | 读码：[cmd/sr-rr/main.go](../controller/cmd/sr-rr/main.go) 只 import 标准库 + `internal/{keybinding,route}`，不碰 VPP/netlink ⇒ **Mac** | 待 S2 的 `GOOS=darwin go build` 过；过不了退回 VM，不猜 |
| 控制器（`sr-ctrl`） | **Mac 192.168.1.4**（你已拍） | 控制台仍只绑 `127.0.0.1:8088`，与容器 lab 同口径 |
| 客户终端 term | **不建**（你的第 5 条） | 打流用 cust 自己 + loopback 前缀 |
| 中转 transit | **不建**（你的第 6 条） | 环路 lab 本来就不建；两跳中转（#139、NAT 阶梯 #222）要用时再单独申请 |
| 容器 lab 的 `lab_mgmt 10.99.0.0/24` | **不要**（你的第 4 条） | VM 里没有这条，也不补 |

## 4. 环境事实（都会写进 VM 的配置，不是"注意一下"）

- **大页**：vpp 包的 `/etc/sysctl.d/80-vpp.conf` 要 `vm.nr_hugepages=1024`，
  2GB VM 上内核真划走 778 页=1.52GB、`MemAvailable` 剩 130MB ⇒ VPP 一起手就被 OOM 杀。
  加 `/etc/sysctl.d/zz-vm-lab-vpp.conf` 写 `vm.nr_hugepages=0`（实测 130M→1687M）。
- **内存**：`memory { main-heap-size 256M }`（默认 1G 在 2GB VM 上撑不住）。
- **`ip addr save/restore` 在 noble 上 rc=255** ⇒ 还原只能逐条 `ip addr replace`。
- **救回路径只有一个**：接管全是**运行时**动作，netplan `60-lab.yaml` 的静态地址没被改，
  所以 `VBoxManage controlvm <VM> poweroff && VBoxManage startvm <VM> --type headless` 一定回得来。
  这是 M1 拍完之后的**唯一逃生口**（没有带外通道了）⇒ 每一次动 WAN 口的脚本都必须自带兜底，
  停机时长在 S3 第一次实测出来写进交付。

## 5. 管理通道：照你真机的形状（逐条抄，不发明）

这是 v2 相对 v1 最大的改动。你 192.168.1.100 那台真机**现在就是这么跑的**，我只做了只读取证。

### 5.1 真机形状取证（2026-11-04 只读）

`/etc/vpp/setup.vpp`：

```
create host-interface name iface0
lcp create host-iface0 host-if vpp0
lcp lcp-sync on
exec /etc/vpp/setmac.vpp
lcp lcp-auto-subint on
set interface state host-iface0 up
```

`/etc/systemd/system/vpp-tap-config.service`（`After/Requires=vpp.service`，`enabled`）：

```
ExecStartPre=/bin/sh -c 'i=0; until ip link show vpp0 …; do …; [ $i -gt 15 ] && exit 1; sleep 1; done'
ExecStart=/bin/sh -c 'ip addr flush dev iface0; ip link set vpp0 up;
                      ip addr replace 192.168.1.100/24 dev vpp0;
                      ip route replace default via 192.168.1.1 dev vpp0 metric 20;
                      for i in 1 2 3 4; do ip link set vpp$i up; done'
```

取到的证据链：

- 内核侧：`iface0` **一个全局地址都没有**，`192.168.1.100/24` + `default via 192.168.1.1 metric 20`
  全在 **`vpp0`**（`ip -d link` 显示它是 `tun type tap`）。
- VPP 侧：`show lcp` 有 `itf-pair: [0] host-iface0 tap4096 vpp0 30 type tap`；
  `show ip fib` 里 `192.168.1.100/32 → dpo-receive: 192.168.1.100 on host-iface0`，
  计数 **to:[145043:11835543]** —— 也就是说我的 SSH 包确实是 VPP 收下再送进那条 TAP 的，
  不是内核直接收的（**这就是"通过 VPP 可达"的字面意思**）。
- 顺带解释了两件事：① 真机**不需要** `arp_ignore`，因为物理口上已经没有地址可替它抢答；
  ② 记忆体里那句"重启 VPP ⇒ `vpp0` 消失 ⇒ 失联"就是这条链的反面 —— `vpp.service` 是
  `Restart=always`，`setup.vpp` 由 `unix{exec}` 自带，`vpp-tap-config` 有 15 秒等 TAP 的门，
  所以 VPP 自己起来之后这条路能自愈。

### 5.2 VM 上的等价形状（每台 CPE，S3 实测后定稿）

这一段不再是"照真机配置文件抄"，是 S3 在 CPE4 上跑出来的。**次序和被删掉的那条 `set mac` 都和真机
`/etc/vpp/startup.conf` 不一致**，为什么不一致、证据数字，全在 §5.4。

管理口（`enp0s9`，真机 `iface0` + `vpp0` 的等价物）：

```
# 1) 建口的同时就把 VPP 侧 MAC 钉成这块 netdev 的真实 MAC —— 全程不发 set mac，理由见 §5.4 缺陷①
create host-interface name enp0s9 hw-addr <enp0s9 netdev 的真实 MAC>
lcp create host-enp0s9 host-if vpp0
lcp lcp-sync on
lcp lcp-auto-subint on
set interface state host-enp0s9 up

# 2) 地址从物理口搬到内核 TAP。这一步不写在 .vpp 里，由 vmlab-tap-config 那条 oneshot 干
#    （它要等 TAP 在内核里出现，最多 15 秒），因为 VPP 进程里 exec 的命令跑不到内核 netns 这一步。
ip addr flush dev enp0s9
ip link set vpp0 up
ip addr replace 192.168.1.104/24 dev vpp0
ip route replace default via 192.168.1.1 dev vpp0 metric 20
```

三条硬约束，违反任何一条都会把这条通道弄死，每一条都有实测：

1. **`hw-addr` 后面那个值只能由人显式传进来，不许脚本现读 `ip link show`。**
   上一版生成脚本读的是"当前 netdev 的 MAC"。这台机器只要有一次接管失败过，netdev 上就会留着
   VPP 自造的 `02:fe:xx`（见下面第 2 条），脚本读到它、再把它焊进下一次开机配置，从此永远起不来。
2. **不许发 `set interface mac address`。** 真机把这条放在 `lcp create` 之后；S3-h 实测这个次序下，
   `lcp-auto-subint on` + `set interface state up` 会把**内核 TAP 的旧 MAC 反写进 netdev**，
   于是内核物理口、VPP host-if、内核 TAP 三处不相等 ⇒ 对端 ARP 停在 FAILED ⇒ 100% 丢包。
   改成建口时给 `hw-addr`，整条路径不再出现这条命令，也就没有可被反写的那一步。
3. **被接管的那个口，netplan 里不许再留它的地址**（`addresses:` 那一行注释掉）。
   留着的话 networkd 会在链路事件时把地址补回物理口——S3-e 就补了一次（+2.793s）。
   **S4-A 把这条的代价量准了**：补回去的那一刻起，同一个 IP 有**两个应答者**（VPP 经 `lcp-sync`
   答一次、内核经物理口答一次），对端 `ping` 从下一个包开始**每个 echo 收到两条应答**
   （324 个 seq 全是 `DUP!`），直到把物理口地址手工 flush 掉才停。所以这条不是"形状脏、不影响功能"，
   是**客户可见的重复应答**，见 §5.5。
   ⚠️ 还有一层：注释只改磁盘上的 yaml，**networkd 手里那份是开机时读进内存的旧配置**，
   `networkctl reconfigure <口>` 实测清不掉，`systemctl restart systemd-networkd` 才真的换掉
   （改完 netplan 必须做这一步，否则下一次链路事件照样补地址）。

客户口同形状但用途不同：`create host-interface name enp0s8 hw-addr <enp0s8 真实 MAC>` +
`lcp create host-enp0s8 host-if lcp-x0`（**这个名字不改** —— 控制器渲染的配置包里写死了它，
[config_render.go:303](../controller/cmd/sr-ctrl/config_render.go)
`net.ipv4.conf.lcp-x0.arp_ignore=1`，改了要动产品代码）。
⇒ 每台 CPE 有两条 TAP：`vpp0`（管理/underlay，真机口径）和 `lcp-x0`（客户侧给 FRR 用，容器 lab 口径）。

> S3 打样时为了保住 SSH，**实际接管的是客户口 `enp0s8`，但 TAP 名字临时叫 `vpp0`**。
> 2026-11-04 S4-A 已经把它改成 `lcp-x0`，`vpp0` 这个名字现在是空的、留给管理口（数字与取证见 §5.5）。
> 管理口 `enp0s9` 的接管（含 `ip addr flush dev enp0s9` 那一下会不会把 SSH 掉一下）**还没做过**，
> 挂在 S4 第一台头上，见 §5.4 末尾的"S4 必须补的三件事"。

### 5.3 S3 的三个判据（过不去就不许往下铺）

1. `ip -o addr` 上 `enp0s9` 无全局地址、`vpp0` 有；`vppctl show lcp` 有 itf-pair；
   `vppctl show ip fib` 里本机 WAN IP 是 `dpo-receive` 且**计数在涨**（我用 SSH 打它）。
2. **执行 `ip addr flush dev enp0s9` 的那一条 SSH 会话是否存活**，Mac 侧同一套三路取证
   （100ms 建连 / `ping -A -i 0.1` / 常驻会话打点），给出实测秒数。
   预期两种结果都可接受，但必须写清是哪种：(a) 会话不掉 ⇒ 管理通道已经切到 VPP；
   (b) 会话掉 ⇒ 说明这条会话的源还在旧路径上，脚本必须 detached 跑 + 兜底重建，也能量化。
3. **一次 VPP 重启（`systemctl restart vpp`，不是 lab 配置里的路径，只为验自愈）之后**，
   `vpp-tap-config` 的 15 秒门能不能自己把管理通道恢复回来；恢复耗时实测。
   ⚠️ 这一步按 AGENTS.md 第 5 条属于"必然中断"操作，只在打样那台做、且做完就停，
   **正式生效路径里仍然不许重启 daemon**（热更失败就是失败）。

### 5.4 S3 实测结论（2026-11-04，取证在 CPE4:/root/vmlab/out/ 与 Mac:~/srvmlab/）

打样那一台是 **CPE4 的客户口 `enp0s8`**（TAP 临时名 `vpp0`，地址 `172.31.4.1/24`，对端 `cust4=172.31.4.2`）。
判据① ③ 全过；判据② 只在客户口这一侧过了，管理口那一侧还没做（下面单列）。

**判据① —— 形状成立，收口在 `check-selfheal.sh`（七条，任一 FAIL 整体退出码非 0）**

场景：包到底是不是 VPP 收的，而不是"内核还在替我答、VPP 只是挂在旁边看"。逐步量了七件事，
重启前后各一遍，两次都全 PASS（`enp0s8`/`host-enp0s8`/`vpp0` 三处 MAC 全等
`08:00:27:65:45:1d`；物理口上无全局地址、地址在 TAP；`show lcp` 里 `host-enp0s8 tap4096 vpp0`
配对在位；FIB 里 `172.31.4.1/32` 是 `dpo-receive ... on host-enp0s8` 且
`to:[1678:159370] → to:[1688:160210]` 计数在涨；功能 0% 丢包；`clear errors` 后空闲 3 秒
`l3 mac mismatch` 零新增；`vpp.service` 与 `vmlab-tap-config` 两个单元都 enabled、VPP 由常驻单元管）。

这里有一个必须写下来的读法：VPP 自己那侧的 TAP 对象（`tap4096`）MAC 每次开机都是随机的
（两次读到 `02:fe:f6:7f:07:66` 和 `02:fe:60:c2:07:b7`），**实测它不参与 L3 判定**，
所以 C1 判的"三处"是"内核 netdev / VPP host-if / 内核侧 TAP 设备"，不是把 VPP 那个随机值也算进去。

**判据③ —— 自愈成立，恢复 6.094 秒，客户侧只断约 0.8 秒**

场景：盒子上的 VPP 因为任何原因退了一次（真机的 `Restart=always` 就是这个场景），
`vmlab-tap-config` 那条 15 秒门能不能不用人管就把通道弄回来。
发射 `systemctl restart vpp`，从发射那一刻起 0.1 秒一次采形状、0.5 秒一次判功能，
`cust4` 侧同时用 `ping -i 0.2` 连续打 900 个包。实测（`restart.log`、`s3eB_ping.log`）：

| 时刻 | 发生了什么 |
| --- | --- |
| +2.633s | 内核里 `vpp0` 重新出现（VPP 的 `unix{exec}` 把 host-if + lcp 自己重建了） |
| +2.793s | `172.31.4.1` 被 **networkd 补回了物理口** `enp0s8` —— 不是设计里的自愈，是硬约束 3 没做实的那一下 |
| +5.579s | 门等到了 TAP（单元自己打印"等 vpp0 用了 3 秒"，15 秒预算用了 3 秒），地址搬到 `vpp0` |
| +6.094s | 本机 `ping -I vpp0 172.31.4.2` 通 ⇒ **设计形状下的通道恢复 = 6.094 秒** |

客户侧看到的却是另一个数：900 个包只丢 4 个（`icmp_seq` 22、23、24、26，0.44%），
**没有一条 `Destination Host Unreachable`**。原因是 `hw-addr` 让 MAC 全程没变，`cust4` 的邻居表项
一直有效；而 +2.793s 到 +5.579s 之间地址虽在物理口，`lcp-sync` 把它同步进了 VPP 的
`host-enp0s8`，**VPP 照样替这个地址应答**。所以按无应答时的 0.2 秒节奏折算，
客户真正感受到的中断 **≈0.8 秒（±0.3s，有应答时 ping 的节奏被高 RTT 拉到 0.28 秒，换算不精确）**，
不是本机看到的 6 秒。这个差别要如实写：恢复"形状"慢，恢复"转发"快。

顺带钉住的机制（都做了对照实验，不是推理）：

- **oneshot 单元确实随 `systemctl restart vpp` 再跑一次**（`ExecMainStartTimestamp`
  从 08:46:01 变成 09:40:21，journal 里 `Started vpp.service` 之后紧跟 `Starting vmlab-tap-config`）。
  对照组：单独 `systemctl stop vmlab-tap-config` 之后观察 50 秒，它**不会**自己回来
  （状态一直 inactive）。所以那次再跑来自 restart 事务的传播，不是"被 multi-user.target 重新拉起"。
  地址形状没坏是因为 ExecStart 本身幂等（flush 空口 + `addr replace`），手工再 `systemctl start`
  一次实测 151ms、0 丢包 —— 也就是说这条自愈路径可以重复执行，不会自己造成中断。
- **`create` 只是旁路，`set interface state up` 之后收方向才真的归 VPP。**
  §2 第 1 条那个"内核照样收帧"的结论只覆盖"create 但不 up"的探针形状。这次 +2.793s networkd 把地址
  补回 `enp0s8` 之后，脚本里那条"回退态"分支（0.5 秒节奏，只在地址真在物理口时才试）
  **一次都没判通**，终态邻居表停在 `neigh[enp0s8]=[172.31.4.2 INCOMPLETE]` —— 内核发得出 ARP 请求，
  收不回应答，因为那个口的收方向在 VPP 的 af-packet ring 里。这正好解释了上面那个 0.8 秒：
  那一段应答是 VPP 给的（`lcp-sync` 把地址同步进了 `host-enp0s8`），不是内核给的。
- 兜底闸门（120 秒那个 sleep 任务）到点时判 A) "TAP 能打通"，什么都没动 —— 逃生口是真的，
  但这次没用上。
- `l3 mac mismatch` 累计 1 个包（重启窗口里 networkd 改配置那一下的残留），之后清零 3 秒无新增。
  成因没抓到包，只有 1 个样本，记为"观察到、未归因"。

**接管路径的时延要在交付里点名（不是缺陷修不掉，是 VirtualBox + af-packet 的账）**

同一台、同一对端、同一口径三种形状实测：
VM 内核直通 ~0.3ms 级；VPP 接管后 `min/avg/max/mdev = 0.804/13.935/140.643/15.750 ms`（900 包）。
真机同形状只有 1.867/6.323/10.787ms。差的这一截是 af-packet 轮询被 VirtualBox virtio 放大，
伴随 `af-packet-input timed out block` 计数在涨。**影响两件事**：SSH 手感，以及任何拿 RTT 当判据的
遥测口径（阈值不能沿用容器 lab 的数）。S3-d 早先报的 p50 0.64ms 是错的——那个样本里绝大部分
是接管之前的内核直通，接管后只采了十几秒。

**S4 必须补的三件事（不补就不算管理通道定稿）**

1. **管理口 `enp0s9` 的接管还没做过**，包括判据② 原文问的那一下
   （`ip addr flush dev enp0s9` + 默认路由搬到 `vpp0` 时，我这条 SSH 掉不掉）。
   做法只能是：脚本 detached 跑 + `VBoxManage controlvm` 兜底，且先在第一台做、做完看数字再铺。
2. **三处 MAC 一致是"能跑"的充要条件，不是"等于某个写死的值"。** 真机取证：
   `/etc/vpp/setmac.vpp` 里写 host-iface0=`02:fe:fa:8f:24:ee` 等 5 条，但真机现状是内核 `iface0`、
   内核 `vpp0`、VPP `host-iface0` **三处全是 `02:fe:60:d9:75:f5`** —— 那个文件在真机上根本没生效，
   真机能跑靠的是一致性。所以我们不抄那五个值，抄的是"三处一致"这个不变量。
3. **客户口 TAP 名改成 `lcp-x0`**（产品代码写死了它），S3 打样用的 `vpp0` 是临时名。
   这条落在 S4 的装包脚本里，不是落在文档里。
   → **2026-11-04 S4-A 已做完**，装包脚本就是按口参数化的 `s4-gen-takeover.sh`，数字见 §5.5。

### 5.5 S4-A 实测：客户口 TAP 改名 `lcp-x0`，顺带把"物理口被补地址"量准（2026-11-04）

为什么要先做这一步：`vpp0` 这个名字是管理口的（真机口径），而 S3 打样时它被客户口临时占着；
管理口接管（判据② 的那一下）必须在 `vpp0` 空出来之后做。产物按口分开：
`s4-gen-takeover.sh <物理口> <CIDR> <TAP> <netdev 真实 MAC> [网关]` 一台可以装两个口，
写的是 `/root/vmlab/setup-<口>.vpp` 和 `/etc/systemd/system/vmlab-tap-<TAP>.service`；
S3 那份单口产物（`setup-s3.vpp` + `vmlab-tap-config.service`）已退役到 `/root/vmlab/retired-s3/`。

**改名窗口本身（窗口 A，`out/s4a.205025/bringup.log` + `cust4:/root/s4a_ping.log`）**

新单元是刚创建的、从没跑过，因此不在 `systemctl restart vpp` 的事务传播集合里 —— 这一轮的地址是
我 `systemctl start vmlab-tap-lcp-x0` 手工起起来的，所以**窗口 A 的耗时不算自愈判据**，只算改名窗口：
TAP 重现 +2.745s、地址落到 `lcp-x0` +2.913s、本机打通对端 +3.572s；旧名 `vpp0` 确认消失，
三处 MAC 仍全等 `08:00:27:65:45:1d`。客户侧同一时刻的 `ping -i 0.2`：`icmp_seq` **248–257 共 10 个包
无应答**（按这一段实测节奏 0.318s/包 ≈ 3.2 秒），零条 `Destination Host Unreachable`。

**改名的副产品才是这一轮最值钱的读数：那个"脏形状"会让客户看到重复应答**

改名窗口之后，`172.31.4.1` 被补回了物理口 `enp0s8`（和 S3-e 同一次现象）。这一次我把它对客户的
影响量出来了：从 `icmp_seq` 259 起，**每个 echo 收到两条应答**（`DUP!`，324 个 seq 全是），
一直持续到我手工 `ip addr flush dev enp0s8`（seq 582 处立刻停止）。
⇒ §5.2 硬约束 3 原来的写法（"没有把功能抢坏，只是形状脏"）**不成立**，已就地改成"同 IP 两个应答者"。
机制：`lcp-sync` 让 VPP 侧 `host-enp0s8` 也持有这个地址并应答一次，内核拿着物理口上那份又应答一次。

**补地址到底是谁干的：是 networkd 手里的陈旧内存配置，不是磁盘上的 netplan**

逐步排除，每步都有读数：

- `/run/systemd/network/10-netplan-enp0s8.network` 里**已经没有 `Address=` 行**（netplan 早前生成过一轮），
  可 `networkctl status enp0s8` 仍显示 `State: routable (configuring)` + `Address: 172.31.4.1`。
- `networkctl reconfigure enp0s8`：rc=0，**地址没被清掉**（试过，读数在上面那段里）。
- 手工 `ip addr flush dev enp0s8` 之后连续观察 15 秒：**没有任何东西补回来** —— 说明它只在链路事件
  （VPP 抢占/释放导致的 `Link DOWN → Gained carrier`，journal 里每次 VPP 重启各一组）时才补。
- `systemctl restart systemd-networkd` 之后：`enp0s8` 无全局地址、`lcp-x0` 保住 `172.31.4.1/24`、
  `enp0s9` 的 `192.168.1.104/24` 与默认路由原样（`ss` 里我这条 `192.168.1.4:64287` 会话还活着）。

**窗口 B —— 判据③ 在产品命名下复测（`out/s4b.205501/restart.log` + `cust4:/root/s4b_ping.log`）**

单元已经是 `active(exited)`，这一轮我没有手工干预，`systemctl restart vpp` 之后它自己再跑了一次
（`ExecMainStartTimestamp` 12:50:29 → 12:55:04）。实测：

| 时刻 | 发生了什么 |
| --- | --- |
| +2.866s | `lcp-x0` 在内核里重新出现 |
| +3.280s | `172.31.4.1` 回到 `lcp-x0`（真机形状） |
| +3.306s | `ping -I lcp-x0 172.31.4.2` 通 ⇒ 设计形状下通道恢复 **3.306 秒** |
| —— | **"地址回 enp0s8" 这一项：90 秒内没发生**；"回退态打通"同样没发生 |

客户侧：440 个包里 `icmp_seq` **104–109 共 6 个无应答**（≈1.7 秒），**`DUP!` 行数 0**。
比 S3-e 的 6.094 秒快，差别就来自上面那条修复——地址不再被补回物理口，门不用再等一轮。
`check-selfheal.sh` 七条全 PASS（C7 的口径也改了：按 TAP 找 `vmlab-tap-*.service`，
再用固定单元名 `vmlab-tap-config` 会误报 FAIL）。

**这一轮要带到 S4 后面的两条**

1. **改完 netplan 必须 `systemctl restart systemd-networkd`**（或冷开），只注释 yaml 文件不算数。
   装包脚本目前只改文件、不 apply，所以这一步得显式做在流程里；做之前先确认它不会把还在内核手里的
   管理口抖坏（这次实测 `enp0s9` 地址与默认路由不变、SSH 存活，但那是"重启 networkd"，
   不是 `netplan apply`）。
2. **判据要用客户侧的"有没有重复应答"，不只是本机能不能通。** 本轮如果没有那条 `DUP!` 计数，
   `ping` 统计里 1.67% 丢包 + 324 duplicates 很容易被当成"抖动"糊过去。


## 6. 装包与产物

### 6.1 钉版（版本必须和容器对齐，否则插件 ABI 不匹配）

| 包 | 源 | 钉死 | 为什么 |
|---|---|---|---|
| vpp / libvppinfra | `packagecloud.io/fdio/release` noble | **`26.06-release`** | 该源默认候选是 **26.10**，不钉就飘；容器里编的插件是 26.06 |
| vpp-plugin-core | 同上 | `26.06-release` | `create/delete host-interface` 是 **af_packet 插件**的 CLI，在 plugin-core 里；`vpp` 包本体装完 `show plugins` 是空的。真机那份 plugin 集合也显式 `af_packet/ linux_cp/ linux_nl enable` + `dpdk disable` |
| frr / frr-pythontools | `deb.frrouting.org/frr` noble `frr-stable` | **10.7.1-0~ubuntu24.04.1** | Ubuntu 自带只有 8.4.4；`frr-reload.py` 那套 0 丢包实测结论是 10.7.1 的 |
| sr_wg / crypto_openssl 插件 | 无 apt 源 | 从 HEAD 容器重编 | **noble 源里没有 `vpp-plugin-crypto-openssl` 包**，自研那份只能自带 |

deb 体积：vpp 4,864,160 / vpp-plugin-core 4,764,444 / libvppinfra 231,212 /
frr 6,894,898 / frr-pythontools 48,898 字节；Installed-Size：vpp 29,833KiB /
plugin-core 25,045 / libvppinfra 648 / frr 40,317 / frr-pythontools 242。

### 6.2 磁盘账（2.4G 根盘，硬约束）

| 段 | 基线(CPE1) | 装完(CPE4) | 增量 | 可回收 |
|---|---|---|---|---|
| `/var/lib/apt/lists` | 68M | 205M | **+137M** | 是（`rm -rf`） |
| `/var/cache/apt` | 135M | 230M | **+95M** | 是（`apt clean`） |
| 真装上的包 | —— | —— | **约 +63M** | 否 |

坑：**装 vpp 会顺手拖进 `linux-modules-extra-6.8.0-147-generic`（deb 113,500,352 字节）+ `wireless-regdb`**，
是依赖不是我们选的。⇒ 每台装完立刻 `apt clean && rm -rf /var/lib/apt/lists/*`，
装包前先 `du -sh /var/lib/apt/lists /var/cache/apt` 记基线。
净结论：每台 CPE 真占用 ≈ 63M(VPP) + 40M(FRR) + 5×~10M(Go) ≈ **153M**，474M 空闲够。

### 6.3 产物：编译只在容器，运行全在 VM

- 插件：`test/docker/build-plugin.sh` 在 `srwg-dev` 镜像里从 **HEAD** 重编，
  落点 `/usr/lib/x86_64-linux-gnu/vpp_plugins/`，**两侧 sha256 都记**。
  容器现有产物：`sr_wg_plugin.so` 247,432B `9d611883…5d54`、
  `crypto_openssl_plugin.so` 17,528B `14d2d9ed…8c77`；
  工作树里那份 247,472B 是 **10-30 旧版**，不许直接拷。
- Go：`sr-agent sr-route sr-fib sr-frr sr-apply` → Linux/amd64，放 VM 的 `/usr/local/bin`
  （和 [controller/deploy/sr-box/start.sh](../controller/deploy/sr-box/start.sh) 的
  `SR_BOX_BIN=/usr/local/bin` 对齐）；`sr-ctrl sr-rr` → `GOOS=darwin` 放 Mac。
- **开机形态是 VM 的第一条真收益**：VM 有 systemd，`sr-box.service` + `sr-box-watch.timer`
  自带注释写着"**未在 lab 实测**：lab 的容器里没有 systemd"。
  真机的 `vpp-tap-config` 证明这一层在真盒子上是必需的 —— 所以 VM 上把管理通道那段也做成一个
  同形状的 oneshot unit，和 `sr-box` 清单一起测（S4 判据）。

## 7. 步骤（一步一判据，每步结束把前后两组数字给你）

**S1　产物**：容器里重编插件 + 交叉编译（Linux 5 个 + darwin 2 个）。
判据：`GOOS=darwin go build ./cmd/sr-ctrl ./cmd/sr-rr` 真过；插件 sha256 两侧一致。
中断：无。

**S2　控制面搬到 Mac**：`sr-ctrl`（控制台仍 `127.0.0.1:8088`）+ 双 `sr-rr`，
状态目录新建（**不迁移** `test/lab/run/ctrl-state`，那是另一套编址）。
判据：控制台能登录（运营方 + 租户）、STUN 两个 UDP 端口在听、Mac `ping` 通 4 台 CPE 的 WAN。
中断：无。

**S3　CPE4 打样管理通道**（§5.2 + §5.3 三条判据；先只做这一台，兜底全部用 sleep 任务）。
判据：§5.3。**中断：点名 S3 会实测"flush 物理口地址"对会话的影响**，这是探针没覆盖的那一下。
✅ 2026-11-04 做完：判据① ③ 过（形状七条全 PASS、VPP 重启后 6.094s 恢复、客户侧只断 ≈0.8s），
结论与实测数字在 §5.4；判据② 只在客户口这一侧过了，**管理口那侧推到 S4**。

**S4　铺到 CPE1/2/3**：装包 → 落产物 → 管理通道切到 §5.2 形状 → 入网（预授权→enroll→心跳）→
`start.sh up` 起满。判据：`start.sh check` 七步逐行绿；
**冷开复现一次**（`controlvm poweroff`→`startvm`），看清单 + `vpp-tap-config` 能不能自愈，
并记录这台 VM 从关机到我能重新 SSH 进去的秒表。
中断：每台停机一次（时长本步骤实测）。
**S4 还欠着 §5.4 那三件事**：管理口 `enp0s9` 接管（含判据② 那一下的 SSH 存活实测，detached 脚本 +
`VBoxManage` 兜底）、TAP 名落成真机/产品口径的 `vpp0` 与 `lcp-x0`、netplan 里被接管口的地址必须注释掉。
→ 2026-11-04 进度：**第二件做完**（客户口已改 `lcp-x0`，`vpp0` 空出来了，数字在 §5.5）；
第三件只做了"改文件"，**还必须补 `systemctl restart systemd-networkd`** 才算数（§5.5 那条要带到 S4 的
两条之一）；第一件（管理口接管）是 S4 剩下的第一件事，用户已点头用"带自动回滚 + 先 CPE4"的姿势做。

**S5　客户侧**：3 台 cust 装 FRR 10.7.1；**把 cust12 改到 `172.31.12.13/24` 并删掉那三条手工
`scope link` 静态路由**；客户 IP 一律用 loopback 播（你拍的第 5 条）。
判据：邻居 Established、前缀数与容器 loop lab 口径对齐；动 netplan 那一下用前后各 3 次
`ping -c 3` 量出有没有瞬断，写数字。

**S6　隧道与 overlay**：控制台建 full-mesh(1,3,4) + hub-spoke(2 为 hub)。
**不发 VRRP 配置**（你拍的第 7 条）。判据：`verify.sh` 逐条过（口径不动），
改色窗口沿用实测 0ms、`frr-reload.py` 沿用 0 丢包/pid 不变。

**S7　判据脚本这一层要新写，不是翻译**：容器版靠 `docker exec`，VM 版没有；
新写 `test/lab/vm-bringup.sh`（只做装包+落产物+管理通道+起服务+入网），
配置全部退回控制台发布（"lab 值手工配、控制器不猜客户网络"不变），
`verify.sh`/`status.sh` 里 `docker exec $C` 换成 `ssh $C`。
判据：同一份判据在 VM 上和容器上出**同样的绿**，否则说明形状没对齐。

**S8　交你复核 ⇒ 你关 docker**：给你一台台能登的把手 + 控制台地址 + 账号。

## 8. 不在这一轮里做的事

- **VRRP / 站点 HA 的双机切换**（你拍的第 7 条）：这一轮刻意排除，避免污染环路结果。
  VRRP 的代码增量（201–204，已提交）不动，只是不发这份配置。
- **两跳中转 / NAT 阶梯 #222 的验证**：需要先建 transit 节点，你拍的第 6 条说不建，等到需要时单独提。
- **4096 规模判据、心跳 8 把 keypair 开销复验**：留在一次性私有台那条线，2GB VM 装不下。
- **环路故障矩阵 #211 / SIGSTOP 反证 #213**：环境跑绿之后另开。
- **别人的现场**（`srwg-*` / `lp-*` / `osc-*` / `sr-lab-vcpe` / `lcp-probe` / `vpplab`）不碰、
  不重启它们的 VPP；**容器回收由你统一做**，我不 `docker rm`。
- **真机 192.168.1.100**：这一轮只读，不改它任何配置（`vpp-tap-config` 那套是抄的，不是动的）。
