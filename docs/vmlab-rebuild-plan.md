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

**判据① —— 形状成立，收口在 `check-selfheal.sh`（当时七条，现已扩到 11 条，任一 FAIL 整体退出码非 0）**

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

### 5.6 S4-B / S4-C / S4-D 实测：管理口接管成了（0 中断），但一次 `restart vpp` 每次都丢掉第一条 exec 的接管（2026-11-04）

取证目录：`CPE4:/root/vmlab/out/{s4b2.210700, s4c.211317, s4c2.132045, s4d.133306}/`；
Mac 侧独立观测在 `~/srvmlab/{out-s4b2.210700, out-s4c, out-s4c2}/`；
客户侧打流在 `cust4:/root/s4{b,c,c2,d}_ping.log`。VM 与 Mac 的时钟差实测 −1.92 秒
（`out-s4c2/clock-offset.txt`：`vm_minus_mac=-1.923267`），跨侧对齐时间线时要扣掉。

**(1) 判据② 那一下：管理口 `enp0s9` 走运行期增量 CLI 接管，实测 0 中断**

场景是 §5.3 判据② 原文问的那一步 —— 把 `192.168.1.104/24` 从物理口 `enp0s9` flush 掉、
搬到 VPP 的 TAP `vpp0` 上，我这条 SSH 会不会断。这一枪**没有走 restart**，四条动作全是运行期增量：
`create host-interface name enp0s9 hw-addr 08:00:27:9c:b6:b3`（1.07 秒）、
`lcp create host-enp0s9 host-if vpp0`（57ms）、`set interface state host-enp0s9 up`（10ms），
然后内核侧 flush + 上地址 + 默认路由 `metric 20`。发射前先坐实客户口旁证基线
（经 `lcp-x0` 打得通 `172.31.4.2`）并声明这一枪不许弄坏它，终态确实没弄坏。

读数：`create` 发射到 `ping -I vpp0 192.168.1.1` 通 **+1.724 秒**。Mac 侧三路独立观测
（`ping -i 0.1`、每 100ms 一次 TCP 建连、一条常驻 SSH 会话每 0.5 秒打点）：
ICMP `icmp_seq` 0..1338 **连续无缺号，0 个包丢**；TCP **1295 次建连 0 次失败**（140.1 秒窗口）；
常驻会话 284 个打点之间**没有 >1.5 秒的空隙**。⇒ 判据② 落在"会话不掉"那一支 (a)：
地址搬家这一下对管理会话没有可测中断。

**(2) 这一枪顺手把 §5.5 那条 netplan 规则在管理口上量实了，代价是 1065 个重复应答**

`create host-interface` 会让内核给 `enp0s9` 产生一组链路事件，networkd 手里那份还没更新的内存配置
就在这个事件上把 `192.168.1.104/24` 补回物理口 —— 和客户口 S4-A 那次同一个机理。
客户可见的后果这次也量出来了：从 `icmp_seq` 274 起（takeover 之后约 27 秒）到观测停止（seq 1338），
Mac 每条 ping **收到两条应答**，`DUP!` 共 **1065 条**。
修法就是 §5.5 写下的那条，两处都得做：磁盘上注释掉 `/etc/netplan/60-lab.yaml` 里 `enp0s9` 的
`addresses:`/`routes:`（mtime 实测 13:10:39），**再 `systemctl restart systemd-networkd`**
（journal 实测 13:11:03）。只做前一半不算数。13:11:03 之后的三枪（S4-C、S4-C-2、S4-D）里
`DUP!` 行数都是 **0**（`cust4` 与 Mac 两侧口径各自数过）。

有一条取证口径要记下来：S4-B 脚本里那句"enp0s9 上还有全局地址吗：没有（对）"跑在 13:08:06，
networkd 的补地址落在 13:08:07 —— **终态检查比它早了一秒，于是报了"对"**。
往后装包脚本的终态判定必须在动作之后留几秒延迟窗口再读，不然这条判据是假绿。

**(3) 判据③ 双口 restart：两枪都丢掉 startup 里第一条 exec 的接管，我上一轮给的修法被否证**

场景：盒子上的 VPP 因为任何原因退一次（真机的 `Restart=always` 就是这个场景）。S4-B 之后两个口都在
设计形状上，所以这一枪同时测两口。两枪的读数：

| 事件 | S4-C（13:13:36 发射，无修复） | S4-C-2（13:22:24 发射，带 ExecStartPre 修复） |
| --- | --- | --- |
| VPP 新实例 pid | +0.332s（28786 → 32111） | +0.332s（同一次形状） |
| 管理口 `host-enp0s9` / `vpp0` | TAP +2.880s、地址 +3.911s、功能 +3.724s | 同左 |
| 客户口 `host-enp0s8` | 没回来（+1.177s 旧实例退出后，新实例那条 create 失败） | **同样没回来** |
| `vmlab-tap-lcp-x0` | 15 秒门超时 → `state=failed` | 同样 failed，之后没人再管 |
| Mac 侧管理口 | 34 个 `icmp_seq` 缺号（≈3.4 秒） | 34 个缺号、TCP **5 次建连失败**（−0.05s→+2.36s），RTT 中位数不变 |
| 常驻 SSH 会话 | 无 >1.5 秒空隙（TCP 重传扛住） | 同左（260 个打点连续） |
| 客户口彻底恢复 | 靠我手工补 | 手工补完到打通 = restart 发射后 **374.9 秒**（1793798544.361 → 1793798919.280） |

S4-C-2 是带着我上一轮给的修法复测的：两个口的 `ExecStartPre=+ip link set <IF> up` 都装上了，
`systemctl show vpp.service -p ExecStartPre --value` 里三条都在，两条 `ip link set` 都返回 0
（返回非 0 的话 systemd 会把单元直接判失败，所以它们确实跑了）。journal 里
`af_packet: fd 15 reason af_packet_fd_error: Network is down` **照样出现**，`host-enp0s8` 照样没建出来。
**⇒ 我上一轮那句"旧实例退出把口置 DOWN，ExecStartPre 拉起链路就能修好"被否证，这个修法无效。**
同一次认错还有一条：我那套采样器读的是 `ip -o link` 里的 `state` 词（那是 IFLA_OPERSTATE，载波态），
不是 `<…>` 里的 IFF_UP 标志位，所以"那一刻口是 UP"这句话本来就没有证据撑着。
这两条已经写进 `s4d-restart-monitor.sh` 的文件头，不留成"下次再说"。

**(4) S4-D：换两件仪器，客户侧从 374.9 秒压到 13.6 秒；根因还差最后一步没钉住**

仪器一是 `ip -ts monitor link` 事件流（内核 netlink，没有采样空隙），外加一个 ~20ms 一拍、只跑前 20 秒的
`<…>` 标志位紧循环当旁证。仪器二是原型自愈器：restart 之后到第 12 秒查 `show lcp`，
没有 `host-enp0s8` 就用运行期增量 CLI 补一次接管（四步，和 §5.2 定稿形状一致），
再 `systemctl reset-failed` + 重启搬地址的那个单元。读数（1793799220.508 发射，old pid 32111 → 53169）：

| 时刻 | 发生了什么 |
| --- | --- |
| +3.192s | `host-enp0s9` 回来（startup 里**第二条** exec 自己成了） |
| +4.587s | 管理口功能达成（`ping -I vpp0 192.168.1.1`） |
| +12.412s | 自愈器判"没有 `host-enp0s8`"，四步补完，`systemctl restart vmlab-tap-lcp-x0.service` rc=0 |
| +12.533s | `host-enp0s8` 出现 —— **这条是自愈器补的，不是 startup 自己成的** |
| +13.882s | 客户口功能达成（`ping -I lcp-x0 172.31.4.2`） |
| 终态 | `itf-pair=[host-enp0s9 tap4096 vpp0; host-enp0s8 tap4097 lcp-x0]`，两个 `vmlab-tap-*` 都 active |

客户侧独立口径对得上：`cust4:/root/s4d_ping.log` 703 个包里 `icmp_seq` **42..115 共 74 个无应答**，
按这一段实测节奏 0.184 秒/包折算 **13.6 秒**，`DUP!` 0 条、`Destination Host Unreachable` 0 条。
⇒ +13.882 秒不是机制的下限，`GATE=12` 是我拍的判据点；同一台的管理口路径已经证明这条路的下限在
**4~5 秒**（+3.192s 出现、+4.587s 通），重试密度做成"每 0.5 秒查一次直到出现"就能贴到这个数。

事件流钉住的三条（现场读数，不是推理）：

1. 旧实例退出时两个 TAP 是被 `Deleted` 的（13:33:40.591 `vpp0`、13:33:40.663 `lcp-x0`），
   而**两条物理网口在整个 restart 窗口里一次链路事件都没有** —— `enp0s8` 的 IFF_UP 全程没动过。
2. 第二条 exec 自己成的那一次，`enp0s9` 在 13:33:43.570770 丢了 UP、43.572163 带着 NO-CARRIER 回来、
   43.609692 恢复，**共 39 毫秒**。这个翻转发生在 TAP `vpp0` 出生（43.556）**之后**，
   所以它是 `lcp-sync` 往物理口同步 MAC 造成的，不是 `create` 那一句。
   ⇒ `create host-interface … hw-addr` 这条形状在运行期会自己把内核口 DOWN→设 MAC→UP 翻一次，
   任何"它不动链路"的假设都不成立（这一条也解释了 §5.5 里 networkd 为什么总在被接管那一刻补地址）。
3. 自愈器补 `enp0s8` 那次（13:33:52.7）同样没有任何 `enp0s8` 链路事件 —— 因为 MAC 早就等值，不需要翻。

**还没钉住的那一步**：af_packet 的 `bind()` 只会因为网口 `IFF_UP` 没置而返 `ENETDOWN`，
而事件流说 `enp0s8` 在失败的 13:33:43 前后 UP 位一直没动，**两条读数互相矛盾**。
原本给那个 ~20ms 紧循环留的旁证整条作废：`flags` 串本身含逗号，我却把逗号当成了 CSV 分隔符，
234 拍全被误判成"丢 UP"（`s4d.133306/flags-tight.csv` 保留但不许引用）。
另一个可能是那条 `Network is down` 根本不属于 `enp0s8` 的 create —— journal 里没打印接口名，
`fd 15` 我也还没对上号；只是"每枪少的都是第一条 exec 的 `host-enp0s8`"这个重复性（两枪相同）
让我暂时把它记在 `enp0s8` 头上。源码侧同样没取到凭证：这台是 `v26.06-release`（2026-06-24 构建），
上游 master 的 `src/plugins/af_packet/*` grep 不到这条错误串、`release/v2606` 的 ref 我拿不到（404），
所以"bind 之前 VPP 还做了哪一步"这句话我现在没有代码依据，不写成结论。

**(5) 这一轮能定的与要拍板的**

能定：判据② 过（数字在 (1)）；**判据③ 没过** —— 双口 restart 稳定丢掉 startup 里第一条 exec 的接管，
两枪复现、位置相同，掉的那一口 `vmlab-tap-*` 门超时后 `failed` 且**没有任何人再管它**，
S4-C-2 客户侧因此断 374.9 秒。⇒ **把接管写进 VPP 的 `unix{exec}` 这条路不幂等**，
也不是"先把链路拉起来"能修好的（运行期同一条命令在两枪里都成功）。
修法已定并落地（不再等点头）：**接管不放在 VPP `exec` 里，改成 vpp.service 起来之后的后置单元**
`vmlab-underlay.service`（oneshot + `RemainAfterExit` + `After=vpp.service`，故意不写 `Requires`），
由 `vpp.service` 的 `ExecStartPost=+systemctl --no-block restart vmlab-underlay.service` 每次 VPP 起来后拉起；
清单 `/root/vmlab/underlay.list`（TSV 6 列：物理口 / TAP / netdev 真实 MAC / CIDR / 网关(可空) / 真实 MTU，
第 6 列与 `sync_mtu()` 见 §5.10）交给 `underlay.sh` → `takeover-if.sh`
有界重试（40 拍 × 0.5 秒），再搬地址并复检；失败就如实非 0 退出，不给 restart 兜底。
这个形状 S4-D 已经跑通过一遍（代价 13.6 秒），S4-E 复测压到 0.8～2.4 秒，数字在 §5.7。

甲/乙那两条**作废**：你 2026-11-04 的话定了调子——「这虚拟机就在本机上，就算网断了你都能管理」，
逐毫秒钉根因属于浪费。乙（换 exec 顺序再打一枪）本来只为分开"输第一条"还是"输 `enp0s8`"，
而两条假设的修法同一个（都不许写在 `exec` 里），分开它对铺 lab 没有增量，所以不打这一枪。
甲的那半个改动是有效的仪器修正，已经并进 `takeover-if.sh` 的 `netdev_up()`：
标志位读 `/sys/class/net/<口>/flags` 的 0 号位（IFF_UP），不用 `ip -o link` 的 `state`（那是 IFLA_OPERSTATE，载波态）。

**(6) 我这几枪里自己造成的缺陷（记账，免得下次当成事实引用）**

- `s4c-restart-two.sh` 的 `link_state()` 用 `grep -o 'state [A-Z]*'`，读的是载波态不是 IFF_UP；
  它给出的"内核 admin UP"这句话不作证（已在 S4-D 换仪器，脚本里那个函数还没删）。
- `s4d-restart-monitor.sh` 的 `flags-tight.csv` 用逗号做分隔符，与 flags 串本身冲突，整路证据作废。
- `s4c-restart-two.sh` 里 `MAXT=90` 实际是 900 拍 ≈171 秒（一拍不止 0.1 秒），标签误导，
  读数必须以每行的绝对时间戳为准，不能拿 `MAXT` 当秒数。
- S4-C 第一枪的 `old pid` 是在 restart 之后才取的（S3-e 遗留），导致"换新 pid"这条判据永不触发；
  S4-C-2 已修，读数是 +0.332s。
- **S4-E 的采样循环是坏的**：四个"直到成功才退出"的探针在 restart **之前**就已经命中，
  于是把发射前的稳态记成了功能时间戳（`addr.done` 那条读数是 `+−1.998s`，负号就是它露馅的）。
  这一枪的功能读数因此改用两条独立打流（缺号数换算），下一条仪器要改成**固定窗口 + 每拍记 ok/no**。

### 5.7 S4-E 实测：接管改成后置单元后，判据③ 过了（2026-11-04，CPE4）

场景同 S4-C：一次 `systemctl restart vpp`，两个口都在清单里。前后同一口径。

| 项 | 旧形状（写进 VPP `exec`） | 新形状（`vmlab-underlay.service` 后置 + 有界重试） |
|---|---|---|
| 2 条 `itf-pair` 回来 | 稳定丢第一条，客户侧 374.9 秒（靠原型自愈压到 13.6 秒） | **+1.886 秒** |
| `lcp-x0`（客户口）地址回来 | 没回来 | **+1.777 秒** |
| `vpp0`（管理口）地址回来 | +2 秒级 | **+2.584 秒** |
| 物理口全局地址 | 被 networkd 补回 ⇒ 双应答 | 全程 **0** 个 |
| 客户侧可见中断 | 374.9 秒 / 13.6 秒 | **≈0.8 秒**（0.2 秒间隔缺 4 包） |
| 管理侧可见中断 | ≈3.4 秒 | **≈2.4 秒**（0.1 秒间隔缺 24 包） |
| 重复应答（DUP） | 1065 条 | **0** |

journal 里能看见是 `ExecStartPost` 把 underlay 拉起来的，重试真的在干活：`enp0s8` 用了 3 拍、
`enp0s9` 用了 2 拍（一拍 0.5 秒）。顺带否证了 §5.6(5) 里"每次必输第一条 exec"那个假设——
新形状下清单顺序里的 `enp0s8` 反而先回来，所以那条实验（乙）连做都不用做。

幂等性也量了：形状已经在位时 `underlay.sh` **0.384 秒**短路返回，不碰任何在跑的连接和地址。
⇒ 这一条是"后置单元"能被反复触发的依据，不是"跑一次就作废"的一次性脚本。

`check-selfheal.sh` 因此从 11 条涨到 **13 条**（C7 换成查新单元 + 查 `ExecStartPost` + 查 `startup` 里不许有 `exec` 行），
CPE4 两个口 restart 前后各跑一遍：13 PASS / 0 FAIL。

### 5.8 S4-F 实测：照新形状铺 CPE1/2/3，装包与收口各踩了一条（2026-11-04）

三台都走 `test/lab/vmlab/bringup-mac.sh` → 盒内 `vm-bringup.sh` 的 A(装包)→B(落产物)→C(管理通道)→D(netplan 收口)，
每台末尾双口闸门 **13 PASS / 0 FAIL**，WAN 侧 `ping` 通 192.168.1.1、客户侧 `ping` 通各自对端。
两条只在"第二台、第三台"才暴露出来的缺陷，都已修进脚本：

1. **装包会把盒子饿死（CPE1）**。`vm-bringup.sh` 原来是「先装 VPP，再写 `vm.nr_hugepages=0`」。
   vpp 包的 postinst 会把 `vpp.service` 拉起来，这时发行版 `80-vpp.conf` 已经抢下 **818 页 = 1636MB**，
   1967MB 的盒子只剩 6MB `available`，apt 的 Post-Invoke 钩子 `apt-check` 直接僵住（1 分 44 秒没进展），
   `apt-get install` 永不返回。改成**装 VPP 之前**就写清单 + `systemctl stop vpp` + `sysctl -w vm.nr_hugepages=0`。
2. **netplan 收口只做了一半（CPE1、CPE3 各一种表现）**。
   - CPE1：只改磁盘 yaml + `systemctl restart systemd-networkd` 不算数。networkd 读的是
     `/run/systemd/network/10-netplan-<口>.network` 这份**生成物**，restart 不会重新生成它，
     旧文件里的 `Address=192.168.1.101/24` 被原封不动再配一遍 ⇒ 阶段 D 的判据 FAIL。
     修法：写 yaml 后 **`netplan generate`** 先断言生成物里 `Address=` 为 0 条，再 restart networkd，
     然后连续观察 20 秒（networkd 补地址不是立刻发生，会跟着接管产生的链路事件再来一次）。
   - CPE3：阶段 C 的闸门在 `enp0s9` 报「物理口还有地址 + 3 秒内新增 1 条 l3 mac mismatch」。
     这不是形状坏，是**判据放错了阶段**：那一刻 networkd 还握着旧地址，underlay 刚 flush 完它就补回来。
     地址归属这条判据只有在阶段 D 收口之后才可判 ⇒ 双口闸门从 C 挪到 D 末尾（D4）。

`vm-bringup.sh` 的判停也修了三处（都是"失败了但没停下来"那类）：
`vpp.service.d` 目录不存在时要先 `mkdir -p`（新盒子没有这个目录，第一次跑 C 直接写空）；
`systemctl restart vpp` 返回时 pid 和 `cli.sock` 都还没就绪（`vppctl` 报 `connect: Connection refused`），
所以 pid 和 CLI 两步都改成有界等待，等不到就打印 journal 并停手；
`vppctl show lcp` 不认 `lcp` = `linux_cp` 没加载 = drop-in 没生效，现在直接判停而不是往下跑出一片连带 FAIL。

### 5.9 S4 形状收口实测：CPE4 补齐 ⇒ 四台形状一致，外加一条必须点名的性质（2026-11-04）

CPE4 是 S3 的打样盒，形状是从手工实验一路改出来的，所以这一枪让它回到和另外三台同一条流水线上：
`bringup-mac.sh CPE4 A/B/C/D` 全走脚本，末尾双口闸门 **13 PASS / 0 FAIL**。四台再逐台复扫一遍
（不是拿各自装机时的旧结论交差）：

| 盒子 | vpp pid | enp0s9→vpp0（WAN） | enp0s8→lcp-x0（CUST） |
|---|---|---|---|
| CPE1 | 12575 | 13 PASS / 0 FAIL | 13 PASS / 0 FAIL |
| CPE2 | 5925 | 13 PASS / 0 FAIL | 13 PASS / 0 FAIL |
| CPE3 | 6103 | 13 PASS / 0 FAIL | 13 PASS / 0 FAIL |
| CPE4 | 66098 | 13 PASS / 0 FAIL | 13 PASS / 0 FAIL |

这一枪踩到的两条：

1. **钥匙块的 bash 优先级把 CPE4 卡在装包第一步**。原来写成
   `[ -f 钥匙 ] || curl … && { 转成 keyring; } || fail "拿不到钥匙"`，
   bash 解析成 `((A || B) && C) || D` —— 钥匙**已经存在**时 A 为真、B 不跑，但 C 照样跑，
   而 C 里那句 `curl …/gpg` 是 404 的旧 URL，于是整块落到 D 报"拿不到 fdio/release 的 GPG 钥匙"。
   CPE1/2/3 是空钥匙所以没暴露。改成显式 `if [ ! -f 钥匙 ]; then … fi`，每步单独判停。
   （同一个 bug 的另一半：CPE4 磁盘只剩 115M 时 `apt-get` 会僵，脚本 A0 先清 `/var/lib/apt/lists`
   与 cache，实测腾到 549M，装完 frr 10.7.1-0~ubuntu24.04.1 仍剩 464M。）
2. **接管后的管理口批量吞吐崩塌，这条要当性质记住，不是当 bug 修**。
   同一个 45MB 产物包：接管前走内核路径 **3.16 秒**（≈14MB/s），管理口被 VPP 接管后
   `ssh cat > /dev/null` **超过 400 秒**（<25KB/s），差约 500 倍。
   心跳、JSON、ssh 交互这类小包不受影响（四台的 ssh 一直稳），受影响的只有**批量传输**。
   后果是 `bringup-mac.sh` 原来每个 phase 无条件重推 45MB ⇒ phase C 之后的每一枪都会卡在传输上
   （实测跑到 10 分钟被我停掉）。改成先 `sha256sum` 盒子上的产物、只对不上的才推
   （`test/lab/vmlab/bringup-mac.sh:51-71`），复跑 D 用 **51 秒**、显示"产物新推 0 份"、闸门仍 13/0。
   停 scp 之后逐份核过哈希，8 份产物与 Mac 侧全等，没留半截文件。
   真机也是这个形状，所以以后要给接管后的盒子传大件，只有两条路：先解管再传，或在接管前传完。

判据本身也收了一处口径：`check-selfheal.sh` 的 C7 原来硬编码
`/root/vmlab/startup-vmlab.conf /root/vmlab/startup-s3.conf` 两个文件名去 grep `exec` 行，
而 VPP 实际跑哪份是 `vpp.service.d/50-vmlab.conf` 的 `ExecStart=-c` 决定的；CPE4 上还躺着
S3 时代的 `startup-s3.conf` 和 5 个 `.bak-*` 死文件。现在 C7 从 `systemctl cat vpp.service`
取出在跑的那份再判，死文件移进 `CPE4:/root/vmlab/retired-s3/`（盒子上的 mv，没进仓库）。
改完四台 8 次复扫仍全 PASS。

### 5.10 S4 尾巴三件实测：MTU 压回、MPLS 默认表、G 段开机清单（2026-11-04，提交 474ab69）

- **MTU 压回**：af-packet 口在 VPP 里出生记 9000，内核 netdev 真实是 1500。控制器
  `tunnelMTU()` 按设备上报那格算预算 ⇒ 算出假的 8868，agent 按真实链路
  （1500−8−124=1368）硬拒装。这个数只能显式传：VPP 起来后 `set interface mtu packet 1500`
  会把内核 netdev 反写成 **1544**，从活口读就读到脏值 ⇒ 清单加第 6 列"出生 MTU"，
  `underlay.sh` 的 `sync_mtu()` 在每条口搬完地址后压回真实值，失败只置 rc 不 continue
  （管理口是唯一能登进去的路）。判据 C3b 读数是 VPP `host-enp0s9`=1500 = 清单期望，
  内核那侧 1544 是 VPP 反写的、不参与判定。
- **MPLS 默认表**：`sw_interface_set_mpls_enable` 的 `retval=-3` 在源码里含义唯一 ——
  VPP 26.06 `src/vnet/mpls/interface.c` 第一步 `fib_table_find(FIB_PROTOCOL_MPLS,
  MPLS_FIB_DEFAULT_TABLE_ID)` 找不到就回 `VNET_API_ERROR_NO_SUCH_FIB`，什么都不动；
  VPP 不自动建这张表。容器台一直是 lab 脚本补的（`setup-node.sh`/`bringup.sh`/
  `loop-bringup.sh` 的 P-3），VM 台按 S7 把配置退回控制台后没有这一笔，所以踩穿。
  前后数字：CPE1 基线 3 次"只装上一部分 … retval=-3"、agent 退避 40 秒到第 4 次才收敛，
  且收敛那一刻表 locks 里是 `CLI:1`（我手工建的那张，属路径上的手工补丁）；
  CPE2/3/4 走脚本后首次 apply **0 次 -3**、与 agent 启动同秒收敛。
  现在四台 wg 口都是 `1368/0/0/0`、`show interface features` 的 mpls-input 段全是
  none configured、`show mpls fib` 每台数到 1 张 `MPLS-VRF:0`。
- **G 段（开机清单）**：单台 16.1 秒装完并 enable，装的是产品那份
  `controller/deploy/sr-box/{start.sh,sr-box.service,sr-box-watch.service,sr-box-watch.timer}`，
  不是 lab 自己写的第六套启动逻辑。装完四台 `start.sh check` 一致：vpp/agent/frr/fib 有、
  tunnels 2/2/3/3 条、route 与 bridge 是"配置在，进程没跑"（那时 rrs 还没发、
  sr-frr 的两个键还空）。闸门：四台 × 双口共 8 次 `check-selfheal.sh` 全 PASS
  （每台 12 条判据、0 FAIL），C5 功能 8 个口各打 20 包 0% loss，
  WAN rtt 最差 25.357ms、CUST 最差 37.123ms（VM 跑在 Mac 宿主上的形状）。
- **留下的产品缺口（那时没动 Go 代码）**：`controller/internal/vppapi/messages.go` 没注册
  `mpls_table_add_del`、`sr-box/start.sh` 不建 MPLS 默认表 ⇒ 任何全新真机盒子第一次装隧道
  都会吃同样的 -3，只能靠 agent 退避重试 + 外部建表。

S5 与 S6 的实测数字、以及这一轮新抓到的三条缺陷在 **§9**。

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
→ 2026-11-04 再补：第一件**做完了，判据② 过**（+1.724 秒、Mac 侧 0 缺号 / 0 建连失败，数字在 §5.6(1)）；
第三件的"必须 restart networkd 才算数"在管理口上又量实一遍，代价是 1065 个重复应答（§5.6(2)）。
**但判据③ 没过**：一次 `systemctl restart vpp` 稳定丢掉 startup 里第一条 exec 的接管，
我给的 `ExecStartPre` 修法复测被否证；原型自愈器能把客户侧从 374.9 秒压到 13.6 秒（§5.6(3)(4)）。
→ 2026-11-04 这条已经收掉：接管从 VPP `exec` 挪进后置单元 `vmlab-underlay.service`
（由 `vpp.service` 的 `ExecStartPost` 每次拉起），判据③ 过，数字在 §5.7；§5.6(5) 的甲/乙**作废**。
→ 同一天照新形状铺完 CPE1/2/3（§5.8）并把打样盒 CPE4 拉回同一条流水线，
四台逐台复扫 **8 次（每台双口）全 PASS**，形状一致（§5.9）。
`check-selfheal.sh` 现在是 **13 条**判据（§5.7：C7 拆成「查新单元在位」+「查 `ExecStartPost`」
+「查 startup 里不许有 `exec` 行」；本次再把第三查的口径从硬编码文件名改成读实际在跑的那份）。
**S4 只剩最后一件**：四台入网（预授权 → enroll → 心跳），然后 `start.sh` 清单起满、冷开复现一次并记秒表。

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

## 9. S5 / S6 实测与缺陷（2026-11-05：①②③ 已收口；S6 §9.5–§9.7、缺口 G-2 §9.6、
周期性停摆归因 §9.8、缺陷⑥ 已量化待拍 §9.9）

### 9.1 S5 客户侧：三台都到位

装包走 `test/lab/vmlab/cust-nat-install.sh`（Mac 侧）+ `cust-frr.sh`（VM 侧）：三台 cust 只挂
intnet、天生无出网，临时插一块 NAT 网卡装 `frr=10.7.1-0~ubuntu24.04.1` + `frr-pythontools`，
装完拔回。VirtualBox 7.2.22 不能热改禁用槽，所以整条路是两次停机。实测（cust3）：
apt 86.33s、ACPI 关机 3.96s、开机到 ssh 回来 37.37s；三台收尾复核都只剩 enp0s8 一张网卡、
静态地址、默认路由 0 条、FRR 版本一致。

第一份配置走 `cust-config.sh`（逐条抄 `loop-bringup.sh:300-347`，数值同 `:93-98`），
差量热更、不 restart。改后复核（2026-11-05 07:1x 那一轮）：

| 台 | `frr-reload.py --test` 差量 | pid（zebra/ospfd/bgpd） | loopback 进路由表 | 邻居 |
| --- | --- | --- | --- | --- |
| cust12 | 0 行 | 823 / 850 / 843 | `172.16.12.1/32` | 那一轮两个邻居都卡 `Init`，成因与收口见 §9.3（现已 `Full`） |
| cust3  | 0 行 | 836 / 855 / 841 | `172.16.3.1/32` | `172.31.3.1 AS 65103` **Established**，PfxRcd 1 / PfxSnt 2 |
| cust4  | 0 行 | 822 / 849 / 842 | `172.16.4.1/32` | `172.31.4.1 AS 65104` **Established**，PfxRcd 1 / PfxSnt 2 |

按你拍的三条：一条 `ip route` 都没写、LAN 前缀不播（只播 loopback）、不发 VRRP。
口径修掉一处假绿：`frr-reload.py --test` 的节内是裸行、没有 `+`/`-` 前缀，
`grep '^[+-]'` 恒等于 0；改成和产品 `internal/apply/apply.go` 的 `frrDelta()` 同口径解析
两节，并加"改前差量必须 > 0"硬闸（故意多写一段 `router bgp 65099` 自检得 5 行）。

### 9.2 缺陷①：`sr-box-watch.service` 每 60 秒把刚起的进程杀掉（已修，VM 台实测）

场景：四台盒子的 overlay 面（sr-route + sr-frr）永远起不来，控制台的 v3 一直"未确认"。
代码走到哪：`controller/deploy/sr-box/sr-box-watch.service` 是 `Type=oneshot`、
**没有** `RemainAfterExit=yes`、也没写 `KillMode`（默认 `control-group`）；
`start.sh:144` 的 `launch()` 用 `nohup ... &` 把 sr-route/sr-frr 起在这个 unit 的 cgroup 里。
为什么这么写：oneshot 的语义是"跑完就结束"，主进程一退 systemd 就把这个 unit 置为
非活动并收割整个 cgroup —— `nohup` 只挡 SIGHUP，挡不住 cgroup 级的 SIGKILL。
症状（CPE1，2026-11-04 23:08 那一轮，journal + `/var/log/sr-box/*.log` 同秒对齐）：
`23:08:28 sr-route: status on 127.0.0.1:19001` → `23:08:30 sr-route: stopped`；
`23:08:29 sr-frr: 反向导入已开：1 个 VPN` → `23:08:30 sr-frr: 停止`；
`23:08:30.264 systemd: sr-box-watch.service: Deactivated successfully`。
0.5s 采样的 `/proc` 口径：sr-route 只活在 47.0–49.1s（约 2.1s）、sr-frr 只活在 48.6–49.1s
（≤1s），两者的 cgroup 都是 `/system.slice/sr-box-watch.service`；同一轮里从 ssh 会话
（`session-320.scope`）起的 sr-agent / sr-fib 活满 71.6s。`/run/sr-box/{bridge,route}.pid`
留下的是已经死了的 pid（43460 / 43437）。
要动什么：unit 加 `KillMode=process`（只杀主进程、不连坐 cgroup）。
**不加 `RemainAfterExit=yes`** —— 那样 unit 会一直是 active，timer 下一分钟的 start job
不再重跑 `ExecStart`，看门狗直接失效。备选（形状更正但改动大）：sr-route/sr-frr 各给一个
`Type=simple` 单元，看门狗退成检查器；容器 lab 没有 systemd，会多出一套并行的启动形状。

已修（unit 里 `KillMode=process` + 上面那段实测注释）。VM 台实测，四台各自独立量过：

| 盒子 | unit 落盘时间 | 修后 sr-route pid | 修后 sr-frr pid | 最近 10 分钟 `remains running` 条数 |
| --- | --- | --- | --- | --- |
| CPE1 | 11-04 23:46:59 | 47678（23:08 那轮起就没换过） | 47701 | 18 |
| CPE2 | 11-04 23:43:19 | 27428 | 27451 | 20 |
| CPE3 | 11-04 23:43:24 | 34038 | 34061 | 16 |
| CPE4 | 11-04 23:44:09 | 94739 | 94763 | 16 |

`remains running` 是 systemd 在 ExecStart 退出时对每个还活着的子进程明说的一句话；
一轮 timer 出两条（sr-route + sr-frr），10 分钟就是 ~18 条 —— 也就是说看门狗每分钟照样重跑
清单，但**不再收这两个进程**。对比修之前同一台 CPE1：sr-route 只活 2.1s、sr-frr 只活 ≤1s。

顺带纠正本节早先写的一句预测：缺陷① 修完，控制台上那条陈旧错误**不是** ≤5 分钟自己消失，
是等 agent 的下一次重试。退避从 5s 起每失败一次翻倍、到 5min 封顶
（`cmd/sr-agent/configsync.go:191`），CPE1 的 agent 日志最后四条全是 5m0s：

```
23:31:44 config v3 failed (12 in a row, next try in 5m0s): sr-frr 热更失败（…活进程还是旧表）…
23:46:50 config v3 failed (15 in a row, next try in 5m0s): sr-frr 热更失败（…活进程还是旧表）…
23:51:52 config v3 applied
```

实测四台各自 `config v3 applied` 的时刻（盒子 UTC）：CPE4 23:23:54（缺陷① 之前就已经好了）、
CPE2 23:48:19、CPE3 23:48:30、CPE1 23:51:52。CPE1 从 23:46:59 装好 unit 到 23:51:52 是
4 分 53 秒 —— 正好是它下一次退避到期的时间。也就是说"≤5 分钟"这个量级碰巧对，
但机制是"设备重试"，不是"控制器改口"；慢的根源是那 5min 的退避上限。



### 9.3 缺陷②：接管客户口时必须放开网卡多播过滤（已修，VM 台实测）

场景：cust12 能看到 CPE1/CPE2 两个邻居、永远停在 `Init/DROther`（9 分钟不动）；
CPE1 侧 `show ip ospf interface` 的 `Neighbor Count is 0`，即盒子完全听不到客户侧。

取证（改前）：`tcpdump -i lcp-x0 proto ospf` 24 秒只抓到盒子里外发的两条 hello，
`lcp-x0` 的 `rx_packets` 增量 **0** —— 内核一个从客户 LAN 来的包都没收到；
单播正常（cust12↔172.31.12.1/.2 各 3 发 3 收 0% 丢包）；
VPP `trace add af-packet-input` 20 秒里没有一条 `224.0.0.5` 的包，说明 hello 连网卡的
RX 过滤都没过。`ip -d link show enp0s8`：`allmulti 0`、mc list 里只有 `224.0.0.1` 和 IPv6 那几个
—— FRR 的组播订阅是加在 lcp TAP（`lcp-x0`）上的，物理 netdev `enp0s8` 根本不知道要收
`01:00:5e:00:00:05`，e1000 的硬件多播过滤直接丢帧。
2026-11-05 01:05 在**从没打过补丁**的 CPE2 上再取一次现场，形状完全一致：`enp0s8` 的 flags 只有
`BROADCAST,MULTICAST,UP,LOWER_UP`，`rx_packets=471`、60 秒内不动，`show ip ospf neighbor` 表体是空的，
单播 2 发 2 收 0%；cust12 侧看 `192.168.1.102` 停在 `Init/DROther` 已经 `2h01m42s`。

为什么必须在产品这一步做：VPP 26.10 的 `create host-interface` 没有任何多播/混杂选项
（`show commands` 里 grep `promisc|mcast|multicast|maddr` 零条），所以只能改 netdev 侧的标志；
而接管这个口的动作本来就在 `takeover-if.sh` 里，放开过滤必须和它同一条路，否则每次重装都会漏。
容器台没暴露这条缺陷是因为容器版客户口是 veth、没有硬件多播过滤。

改了什么：`test/lab/vmlab/takeover-if.sh:28` 加 `allmulti_on()`，`:30-46` 在四条增量 CLI 之前、
也在"这条口已经在 VPP 里就直接返回"之前放开过滤 —— 放在返回之前是因为口一进 VPP 接管就算"成功"，
漏掉的过滤没人再补，重跑脚本必须还能把它打开。三档结果（`已开` / `本次打开` / `开不了(非致命)`）
都只拼进 `mm_note` 打印一行、不改退出码，理由和 MTU 一样是结构性的：
`underlay.sh:90` 对本脚本非 0 退出的处理是 `continue`，也就是"这条口整段跳过搬地址"，
管理地址是唯一一条能登录进去的路，任何非致命校验都不许挂在这条路上。

实测（闸门五组，改前/改后同一口径）：

1. **CPE2 补上过滤就通**：`OK(1) enp0s8 已在 VPP 里，TAP=lcp-x0 allmulti=本次打开`，脚本耗时 0.09 秒；
   `rx_packets` 从 471 开始涨（01:07 是 518、01:10 是 563）；`show ip ospf neighbor` 两条都 `Full`；
   cust12 侧 `192.168.1.102` 从 `Init` 两小时不动变成 `Full/DR`；改后 cust12 的 LSDB 里确实有它发的
   `AS-external-LSA`（Link State ID `172.16.3.1`，`Advertising Router: 192.168.1.102`，seq `8000000a`）
   和 area 0 里它自己的 router-LSA —— 邻接没起来之前这两条都不可能存在。
2. **放开过滤之后邻接建立要多久**：在 CPE2 上主动 `clear ip ospf neighbor` 一次（这条链路上只有
   lab 自己的三条邻居，没有业务压着），两条邻居都在 **12 秒**到 `Full`。
3. **CPE1 撤掉手工补丁**（原反证里那条 `ip link set dev enp0s8 allmulticast on` 已删，换成产品路径）：
   关掉过滤后立刻重跑同一条脚本，关窗 **0.095 秒**、报 `allmulti=本次打开`；邻居 uptime
   `1h55m29s` → `1h55m40s` 连续没归零；`ospfd` pid 16658 与 `15:56:39` 的启动时刻都不变；
   客户可见打流 1 秒间隔 12 包 **0% 丢包**（rtt min 2.328 / avg 10.307 / max 32.342 ms）。
4. **VPP 重启会不会把过滤冲掉**：CPE2 `systemctl restart vpp`，0.2 秒一拍采 140 拍（restart 在第 20 拍发出，
   覆盖整个窗口）—— 140 拍全程 `ALLMULTI` 都在；重启后 `vmlab-underlay` 的接管日志两条口都报
   `allmulti=已开`，脚本没有重复动它（幂等）。同一次重启里 OSPF 邻接**会**随 TAP 重建而 reset
   （改前 `2m44s`，改后 `17.475s` / `22.150s`），客户可见 30 包丢 2（6.67%）—— 邻接 reset 和丢包
   都是 TAP 随 VPP 一起销毁重建的结果，接管路径本来就每次重启都会重跑（S4 把接管从 VPP 的
   `unix{exec}` 挪成了 `vpp.service` 的 `ExecStartPost`），不是本缺陷引入的新中断。
5. **铺到四台**：按各台 `underlay.list` 逐条跑脚本，八条被接管的口改后 8/8 都带 `ALLMULTI`
   （CPE1 `enp0s9`、CPE3 与 CPE4 的两条口各 `本次打开`，其余 `已开`）。
   判据进了 S3 的形状校验器：`check-selfheal.sh` 新增 **C3c「物理口的多播过滤已放开」**
   （`test/lab/vmlab/check-selfheal.sh:63-72`，容器台恒真、只有 VM 台会抓到），
   四台 × 两条口共 8 次跑完是 **8/8 全部 PASS** —— 也就是说今天这些改动没有碰坏 S3/S4 的
   任何一条形状判据（C1 三处 MAC、C2 地址归属、C3 配对、C3b MTU、C4 dpo-receive、C5 ping、
   C6 mac mismatch 无新增、C7 自愈单元）。

没测到的，点名：

- CPE3/CPE4 这两条客户链路本轮没有组播说话者 —— 盒内 `ospfd` 起来了但 `OSPF is not enabled in vrf default`，
  链路上跑的是 cust3/cust4 的 eBGP 单播（§9.1 表里 Established），所以"放开过滤 → 邻接建立"这条因果
  只在 CPE1/CPE2↔cust12 上量到；那两条口的改动只有 flags 数字，没有业务侧变化。
- VRRP 用 `224.0.0.18`，同一条硬件过滤一样会挡 —— 这一轮不发 VRRP 配置，所以那条是**预测、没测**。
- 放开过滤后 VPP 会开始收到客户 LAN 的组播帧，这些帧在 VPP 里走哪条路（punt / 丢弃 / 计数）没测。
  生产上客户 LAN 一定有 IGMP、mDNS 这类流量，需要单独定性，不能默认它无害。
- 真机 192.168.1.100 的接管是当年手工敲 CLI 做的、没有对应脚本，物理网卡是不是同一形状没测。
- 待拍一条：这条规则要不要抬进产品装机清单（`controller/deploy/sr-box/`），还是就留在 VM 台的接管脚本里。
  抬进去之前先在真机上量一次，避免把 e1000 的结论抄到别的驱动上。

### 9.4 缺陷③：全新盒子第一次 apply 必失败在第 5 段（sr-frr 热更）—— 已修，VM 台实测

场景：一台盒子从没装过配置，控制台第一次点「发布到设备」。这一次 apply 落在第 5 段
（sr-frr 那张表）就死掉，而且**永远**死在这里：盘上的配置写不进去，进程又起不来。

代码走到哪：`internal/apply/srfrr.go` 的 `applySrfrr` 按 `docs/o6e-leak-plan.md` §5.3
定的次序先 `pushSrfrr()`（PUT `/run/sr-frr/control.sock`），PUT 不成功就直接 return 错误、
后面的 `writeFileSync` 一行都不执行；`cmd/sr-frr/main.go:699` 又在 `export_vrfs` 与
`import_rules` 都为空时 `log.Fatal` 拒绝启动。两句凑在一起是个死锁：盘上那两键要进程活着才写得进去，
进程要那两键非空才肯起来。容器台从来没暴露过它，因为 `bringup.sh` 手工先把非空值写死了；
VM 台按 S7 定案把配置退回控制台、空值起步，锁就出现了。

为什么"先 PUT 后落盘"这个次序本身是对的（不能反过来）：反过来写，热更失败时盘上是新表、
活进程是旧表，而设备报给控制器的是"这个版本装好了"—— 控制台就会把没生效的东西显示成生效了。
所以失败必须硬失败、保留上一份，这条不动。

要动什么（已拍并已落地）：只在"这台盒子上 sr-frr 本来就没在跑"时豁免 ——
判据是两条**同时**成立：socket 上没人监听（`ENOENT` 或 `ECONNREFUSED`，`srfrrAbsent()`，
`internal/apply/srfrr.go:158`）**并且**盒子上没有 sr-frr 进程（`SrfrrRunning()`，
`internal/apply/apply.go:420`，默认实现 `procExists("sr-frr")`）。成立就按开机重放那条路走：
只落盘、不热更，并附一条备注交给上报链；其余情况（进程活着但 socket 连不上，
例如它正在启动、或者控制口被人删了）维持原样硬失败 —— 那种时候落盘会造出一个
"活进程是旧表、盘上是新表"的盒子，正是 `o6e-leak-plan.md` §5.3 要防的。

备注一路走到控制台，中间没有汇总也没有改写：
`applySrfrr` 的 note → `apply.Status.Notes`（`internal/apply/apply.go:334`、
`:670` 写进 status.json）→ `wire.ConfigState.Notes`（`internal/wire/wire.go:469`）→
agent 的 `readState()` 每次上报前重读盘（`cmd/sr-agent/configsync.go:87`）→ 心跳 →
控制台的「设备备注」行 + 「设备已装 vN（有未热更）」标签。

改完的判据（CPE1 / node 1，2026-11-05 00:34–00:57 UTC，脚本 `/tmp/gate3.sh A|B|C|D|E`，
取证留在 `/tmp/gate3-state/`）：

| 阶段 | 动作 | 盘上 `/etc/sr-frr.json` | 控制器发布 / 设备回报 | 备注 |
| --- | --- | --- | --- | --- |
| B 基线 | sr-frr 活着、`pl2` | sha `7f3108cb6f87` | pub v3 / 设备 v3 `source=boot` | 无 |
| C 第一段 | `kill -9` sr-frr，改成 `pl1` 并发布 | sha `80bce5f2443f`（**写进去了**） | pub v4 / 设备 v4 `ok=true source=apply` | 「已落盘 /etc/sr-frr.json、未热更…」 |
| C 反证 | 同一时刻连控制口 | — | — | `FileNotFoundError`（socket 文件确实不在） |
| D | 等看门狗那一轮把 sr-frr 起起来（00:43:18，pid 55815） | 同上 | 仍 v4、备注**还在** | 起进程不算一次 apply |
| E | 改回 `pl2` 并发布（此时 sr-frr 活着） | sha 回 `7f3108cb6f87` | pub v5 / 设备 v5 | `notes=null`（正常热更） |
| 控制台复核 | 再造一次 v6（带备注）→ v7（干净） | — | v6 有备注行 / v7 无 | 见下 |

客户可见路径：整个窗口（杀 sr-frr → 按落盘走 → 清单把 sr-frr 起起来 → 正常热更）里
CPE1 1 秒一次 ping 客户侧 `172.31.12.13`，**286 个回显、0 丢包**（收尾用了 SIGTERM，
ping 没打统计行，按 `icmp_seq` 连续性与回显行数复原），时延 min 1.26 / avg 4.79 / max 52.5 ms。
盒子里外发的包只动了 sr-frr 自己：`sr-route 47678`（11-04 23:47:28 起）、`sr-fib 21441`、
`zebra 16646`、`ospfd 16658` 三个 pid 从头到尾没换过。cust12 侧看 `192.168.1.101`
这条邻居，现在 Up Time `1h45m42s`，横跨整个 gate 没有归零。

为什么不加"备注自动过期"：它只被**下一次 apply**替换。D 段那轮看门狗把 sr-frr 起起来了、
表也确实活了，备注仍然在 —— 这不是漏，是刻意的：`internal/apply/route.go:252` 给 sr-route
发的是同一类"已落盘、未热更"备注，生命周期就是这样。只给 sr-frr 开一条自我清理的路是我发明的
机制，不是你的规则；真要让"进程起来了"消掉备注，得两条一起改。

单测（容器 `lp-ctrl`，go1.22.5，`-count=1`）：`internal/apply` / `cmd/sr-agent` /
`internal/wire` / `cmd/sr-ctrl` 四个包全过；`-run Srfrr -v` 出 9 条 PASS，其中两条是这次的判据
—— `TestApplySrfrrNoProcessWritesWithNote`（没进程 → 落盘 + 备注）和
`TestApplySrfrrAliveProcessWithUnreachableSocketStillFails`（有进程但连不上 → 仍然硬失败、
盘不动）。

铺到四台的口径（`sr-agent` sha `f2e1461226db`、`sr-apply` sha `d17f40beac08`，四台一致）：
换二进制只 kill `sr-agent`、由 `sr-box-watch.service` 那一轮把它拉回来，其余 daemon 一个都不许动。
实测 CPE3 `sr-agent` 11844→41595、CPE4 72558→102186，两台的 `sr-route`/`sr-fib`/`sr-frr`/`zebra`/`ospfd`
pid 全程不变；CPE1 那只换了 `sr-apply` 文件（`03599cca5471`→`d17f40beac08`），五个 pid 一个没动。
CPE4 的升级窗口里从 Mac 1 秒一次 ping 它的 `vpp0` 地址 `192.168.1.104`（走 VPP 接管口的管理通道）：
16 发 16 收、0.0% 丢包。CPE3/CPE4 没有"客户可见路径"可量 —— S5 只给站点 1/2 做了客户侧编址，
这两台盒子上还没有客户 LAN，所以那两台用管理通道 + pid 两条口径代替，不硬凑一个数。

控制台侧（真实浏览器 `http://127.0.0.1:8088`，租户 `vm-tenant`，展开「路由泄露 · CPE-SDWAN」
的 node-1）：v6 有备注时那一行是 `<tr><th>设备备注</th>…`，取色 `rgb(210,153,34)`（warn 色），
标签是 `tag stale :: 设备已装 v6（有未热更）`；v7 干净热更后备注行消失、标签变回
`tag online :: 设备已装 v7`。这次顺手补的是**泄露页**：它原先只给标签不给原因，
和「基础配置」页不对称，现在两处共用同一个 `cfgNotesRow()`（`console.html:1553`、
泄露页 `:6591`）。截图没成 —— in-app Browser 面板在后台（`visibilityState=hidden`）时
拿不到可视表面，DOM 取证是实际页面的内容，不是 mock。

### 9.5 S6 第一段：五条隧道全部从控制台建，四台落在隧道 v8（2026-11-05）

场景：S5 把三台盒子的客户侧编址和盒内 FRR 都铺好了，但盒子之间一条隧道也没有，
overlay 上什么都传不动。这一段要拍的是「隧道层完全由控制台产出，lab 里不再手敲 VPP CLI」，
这是 S7 把 verify 口径从 `docker exec` 迁到 `ssh` 的前提。

建法与结果（`vm-console.py status` 与 `GET /api/underlay` 两路口径一致，2026-11-05 02:4x 盒子时间）：

| 隧道 | 两端（外层 `addr:port`） | 颜色 | MTU | 中转跳 |
| --- | --- | --- | --- | --- |
| t2 | CPE1 `192.168.1.101:20000` ↔ CPE3 `192.168.1.103:20000` | 0 | 1368 | 0 |
| t3 | CPE1 `192.168.1.101:20001` ↔ CPE4 `192.168.1.104:20000` | 0 | 1368 | 0 |
| t4 | CPE3 `192.168.1.103:20001` ↔ CPE4 `192.168.1.104:20001` | 0 | 1368 | 0 |
| t6 | CPE2 `192.168.1.102:20000` ↔ CPE3 `192.168.1.103:20002` | 0 | 1368 | 0 |
| t7 | CPE2 `192.168.1.102:20001` ↔ CPE4 `192.168.1.104:20002` | 0 | 1368 | 0 |

为什么是这五条而不是 full mesh：站点 12 里 CPE1/CPE2 挂在同一个客户 LAN（cust12 那台 VM），
它们之间不需要隧道；跨站点只需要「每台盒子各连对端站点的一条」，加上 CPE3↔CPE4 这条
好让站点 3/4 也互为对端。端口是控制器分配的中转端口（第 1 段全局配置里的端口段），
不是 lab 手挑的 —— 这就是 D3 之前定下的口径：控制面分配、数据面照做。

判据（前后两组数字，同口径）：发布侧 `vm-console.py status` 出
「node1 基础配置 发布v7/设备v7、node2/3/4 发布v3/设备v3、四台隧道设备 v8」，
四台都没有 `[未确认]` 后缀，也没有 `overlay 有未发布的改动`；设备侧每台
BFD 会话数 = 它承载的隧道数（CPE1/2 各 2 条、CPE3/4 各 3 条）且 `Session state` 两列都是 Up，
`Detection Time (usec) 150000` 全一致（`Required Min Rx Interval 50000` × `Detect multiplier 3`，
和 `internal/wire/wire.go:887-889` 的三个默认值对上），wg 口名按隧道号生成
（CPE1 `wg1002 wg1003`、CPE2 `wg1006 wg1007`、CPE3 `wg1002 wg1004 wg1006`、
CPE4 `wg1003 wg1004 wg1007`）。

本轮**没有发 VRRP 配置**（S6 定案之一）。要按 §8 的规矩点名带来的缺口：站点 12 现在
是"两台盒子各跑各的"，没有 VRRP 就没有虚网关，客户侧主机只能靠 loopback 编址互相可达，
「网关冗余」这一条在 VM 台上是空白，等 S7 之后单独开一轮。

### 9.6 缺口 G-2：sr-fib 要的隧道表在产品里没有任何来源，这一段暂时由 lab 脚本填

场景：隧道建好、BFD 全 Up、sr-route 的 RIB 里也有 2/2/3/3 条路由，但 VPP 的 FIB 里
**一条 overlay 路由都没装**。CPE3 的 `/var/log/sr-box/fib.log` 把原因写得很直白：

```
2026-11-04 23:43:58 sr-fib: 警告: 172.16.12.1/32: 下一跳 10.250.0.1 不在隧道表里，该路径跳过
```

这句在同一台机器上出现 14 次，最后一次是 2026-11-05 01:14:17（在那之后才有下面的脚本）。

代码一步步走到哪：sr-fib 每轮收敛先读 sr-route 的 `/status` 拿 RIB
（`cmd/sr-fib/main.go:235` 的 `converge()`，`fetchStatus()` 在 `:314`），再读 VPP 的
BFD 会话表，然后把两样交给 `fib.PlanFrom(...)`。`PlanFrom` 里每条路径要过两道闸：
第一道是「这个 overlay 下一跳在我自己的隧道表里吗」—— `internal/fib/fib.go:302`，
不在就打上面那句 `该路径跳过`；第二道才是「它的 BFD 是不是 Up」—— `fib.go:308`。
问题在第一道：sr-fib 的隧道表（`{next_hop, interface, color}`）只从它自己的配置文件读
（`cmd/sr-fib/main.go` 的 `loadConfig`/`fibTunnels`），而控制器的配置包六段里**没有这张表**。
第 3 段把 wg 口建在 VPP 上、第 4 段只把「隧道的下一跳地址 + VPN 表」写进
`/etc/sdwan/route.json` 给 sr-route 用，谁都没告诉 sr-fib「10.250.0.2 这个下一跳
是本机的 wg1002、颜色 0」。这个映射只有盒子自己知道，产品里今天没有任何进程维护它。

D3 已经开好了接缝：sr-fib 有 `PUT /config`（`configMux()`，`cmd/sr-fib/main.go:472`），
推完立刻按新表收敛，不用重启。所以这一段的**产品缺口是"谁来调这个接缝"**，不是"接缝不存在"。
按规矩点名：要动的是二选一 —— agent 在收敛完隧道后把这张表推给 sr-fib（它手上已经有
sw_if_index、颜色和本端 overlay /32），或者 sr-fib 反过来向 agent 要一次。这是"新增一条
下发路径"的拍板，不是修 bug，本轮不动控制器代码。

lab 侧怎么补（`test/lab/vmlab/vm-fib-sync.sh`，容器台 `test/lab/loop-fib-sync.sh` 的 ssh 孪生）：
两头都取真数据，脚本不猜任何一个字段。

- 下一跳 / 本端 overlay / 颜色 ← 控制台 `GET /api/underlay`。一条隧道两端各有一个 overlay /32，
  脚本按本端摊开成「本端 overlay → 对端 overlay」，所以每台盒子只看见自己那半张表。
- 接口名 ← 盒子里 `vppctl show interface addr`：只认 `wg` 开头且 `(up)` 的口，
  以及该口上绑的那条 `L3 x.x.x.x/32`；本端 overlay 命中哪条 /32，口名就是谁。
- VPN 表号 ← 盒子里 `/etc/sdwan/route.json` 第 4 段的 `vrfs[].id`（控制台发布的）。
- 逃生开关 ← 先读 sr-fib 当前 `/status`，把 `escape_all_colors` 原样带回去，脚本不改它。

推完回读 `/status` 判定：`warnings` 非空或 `converged=false` 就退出码 1，不给自己留"应该没问题"。
VM 盒子上 sr-fib 的状态口是每台同一个 `127.0.0.1:19011`（容器台是 19010+n 错开，这里不用错开，
因为每盒一个 loopback）。

改完的判据（前 / 后同一口径）：

| 口径 | 推之前 | 推之后（2026-11-05 02:4x 盒子时间） |
| --- | --- | --- |
| sr-fib 隧道表（CPE3） | `tunnels: []` | 3 行：`10.250.0.1→wg1002`、`10.250.0.6→wg1004`、`10.250.0.7→wg1006` |
| `fib.log` 的闸①告警 | 14 次「不在隧道表里」 | 0 次（只剩 BFD 那一类，见 §9.9） |
| VPP FIB 里的 `172.16.x/32` | 0 条 | CPE1/2/3/4 各 **3** 条 |
| 带 MPLS 标签的出向 | 0 | CPE1 2、CPE2 2、CPE3 3、CPE4 3（`mpls-label[1000:64:0:eos]`，颜色 0 走主表 0） |
| sr-fib `/status` 最后一轮 | `converged=true` 但 routes 全 0 | `converged=true 警告=0` |

### 9.7 overlay 的业务判据：三个站点的客户 loopback 两两可达（六方向）

编址事实先说清楚，免得数字被误读：S5 只把每台客户 VM 的 **loopback** 播进 overlay，
客户 LAN 段 `172.31.x` 不在 overlay 里，所以探针必须用 loopback 当源地址；CPE 盒子自己
也 ping 不了 overlay 源（`ping -I 10.250.0.5` 回 `bind: Cannot assign requested address`，
那些 /32 活在 VPP 里，不绑在任何 netdev 上）。这两条都是编址与实现事实，不是缺陷。

判据（`ping -c 50 -i 0.2 -W 1 -I <源 loopback>`，源在客户 VM 上，2026-11-05）：

| 源 | 目的 | 丢包 | rtt min/avg/max (ms) |
| --- | --- | --- | --- |
| cust12 `172.16.12.1` | `172.16.3.1` | **8%**（4/50） | 5.288 / 17.100 / 51.600 |
| cust12 `172.16.12.1` | `172.16.4.1` | 0% | 6.633 / 13.907 / 44.297 |
| cust3 `172.16.3.1` | `172.16.12.1` | 0% | 6.321 / 19.560 / 62.524 |
| cust3 `172.16.3.1` | `172.16.4.1` | 0% | 6.057 / 12.552 / 78.535 |
| cust4 `172.16.4.1` | `172.16.12.1` | 0% | 5.578 / 13.163 / 31.829 |
| cust4 `172.16.4.1` | `172.16.3.1` | 0% | 5.633 / 13.526 / 80.030 |

六个方向全通，链路形状也对称（跨站点单程 rtt 6–80ms 抖动，min 稳定在 5–6ms，
说明转发路径没走弯路）。第一行那 8% **不是编址问题**：同一次采样里别的方向 0%，
它就是 §9.9 那条放大链撞上了一次抖动 —— 一次抖动 150ms 判 Down、下一轮（1s）才装回，
按 200ms 发包正好吃掉 4–5 个回显。这个数字要留着，等 §9.9 拍完再复测同一张表。

更早的两段长窗（1 秒一次，源同样是客户 loopback）把这条链的客户可见长度量全了：

| 方向 | 发包 | 丢 | 连续断流段 |
| --- | --- | --- | --- |
| cust3 → `172.16.12.1` | 1256 | 41（3.26%） | 12s、11s、9s、5s、4s |
| cust3 → `172.16.4.1` | 1401 | 37（2.64%） | 10s、9s、7s、6s、3s、2s |

### 9.8 周期性停摆归因收口（#261）：帧根本没到这台盒子的网口

场景：容器台当年就有一条「基线不干净」的记录（§基线那节，宿主冻结 + 产品内放大链）。
VM 台搬到 VirtualBox 之后 BFD 仍然周期性抖动，必须重新量一遍，不能把容器台的结论抄过来。
这一轮把「谁在停」拆到只剩一个可能。四段窗口，全部**在同一台机器的同一个时钟上**取证
（跨盒子对表是坑，见下面的口径清单）：

窗口 A（150s，CPE3）：只放一个 100ms 粒度的 BFD 采样器，同时量采样器自己的迭代耗时。
迭代耗时 p50=111 / p99=168 / max=241ms，>333ms 的迭代 0 次 —— 机器没有被长时间冻结。
这一段里只有 t4（CPE3↔CPE4）那条会话抖了 3 次（668–820ms），而 t2 入向最大间隔只有 142ms
（低于 150ms 的检测时间，所以不抖）。结论：抖动是**分方向、分链路**的，不是全机同时。

窗口 B（180s，CPE3 与 CPE4 双向对拍）：CPE3 侧会话 Down 3 次（1015/872/797ms），
同一时刻它的入向到达间隙 939/793/597ms；CPE4 侧 Down 3 次（792/971/601ms），
入向间隙 929/833/746ms；两边采样器迭代 max 216/255ms。两边的抖动时刻差约 55s，互不相关
—— 说明这不是"一台机器卡了、对端跟着掉"，而是各自方向上真的收不到包。

窗口 C（240s，CPE3 三个隧道口同时抓）：四次事件里三条隧道的会话**同时**转非 Up
（…976.05、…984.20、…5126.76、…5143.69，持续 387–916ms），同一时刻三条独立
发送方的入向流一起出现间隙（816/698/527、765/647/466、361/358/453、860/585ms），
采样器 max 546ms。三个不同源同时不发包的概率极低，所以嫌疑收到「送进 CPE3 的这一条路」上。

窗口 D（150s，CPE3 三层同时取证，脚本 `/tmp/cpe3-rxwin.sh`，一次性诊断脚本不入库）：
把「收端到底有没有看到包」拆三层 —— A 层内核网口计数 `/proc/net/dev`，
B 层软中断挤压 `/proc/softnet_stat`，C 层 VPP 抓包点（tcpdump 在 enp0s9 上抓
`udp port 20000/20001/20002`，也就是三条隧道的外层）+ BFD 状态。实测：

- 窗口 149.3s 内 enp0s9 内核 `rx_packets` +13591（整机 91.1pps），`rx_dropped` +750（占到达 5.2%）。
- 三条隧道流各 3321 / 3604 / 3382 个包（22.2 / 24.0 / 22.6pps，中位间隔 44–45ms，就是 BFD 自己），
  >150ms 的到达间隙 2 / 6 / 14 次，最长 932ms。
- 按中位间隔算应有包数与实到包数之差：三条流共缺 ≈196 个包（每条 0.5%–0.9%）。
  **紧接每个间隙之后的 50ms 里只多到 29 个包**，也没有同一时间戳成串放行的现象
  —— 缺的包不是"排队晚到"，是真的没送到。这一条把"压包后成串放行"排除掉了。
- 间隙期间整机计数：22 次间隙里 2 次整个网口几乎没进包（例如 t=…344.88 的 637ms 只 +13 包，
  按 91pps 期望 ≈58；t=…433.69 的 165ms +0 包），其余是"网口还在走、只有隧道流停"。
  这正好符合网口层面的投递停顿，而不是某条链路的丢包。
- B 层什么都量不到：VirtualBox 客户机里 **没有 `/proc/softnet_stat`**，`top` 的 `%st` 恒为 0.0
  —— 这两个"宿主没参与"的证据在这个台子上是**拿不到的**，早先靠 `%st=0` 下的结论作废。

结论（能站住的部分）：抖动发生在「送进这台盒子网口」这一环，帧没有被交付到
CPE3 的抓包点（抓包点在 VPP 之下，所以 VPP 内部忙不过来不会造成到达间隙，
"sr-fib 定时器 servicing 晚"这条也被排除）。同一个网口上另有一个**独立的** guest 侧信号：
`rx_dropped` 每 150 秒涨 750，且以 5 为一档成串出现 —— af_packet 环溢出才会涨这个计数，
也就是 VPP 偶尔没有及时取包；它和上面的到达间隙**不同相**（22 次间隙里 11 次 `rx_dropped`
一点没涨），所以它解释不了 BFD 抖动，但它本身是 lab 环境的负载形状：每台 VM 只有 2 个 vCPU，
VPP 的 `vpp_main` 独占其中一个跑 100%，宿主是 8P/16T、`Load Avg 17.18/18.13/18.46`、
71.9% sys / 8.4% idle。

不能站住、要点名的部分：最后一公里（VirtualBox 的宿主侧网卡模拟到底丢没丢）需要
在 Mac 的 `en0` 上抓一份对照包，`sudo -n true` 回「a password is required」，我没有口令，
这一段留给下一轮。**这条归因不改动产品**：它说明的是"这台 VM 台为什么会抖"，
而"抖一下为什么变成 1 秒以上客户黑洞"是 §9.9 的缺陷，两边都要修，但不是一件事。

口径陷阱清单（这次踩到的，写下来给下一轮省事）：

1. `vppctl` 每行结尾带 `\r`，不 `tr -d '\r'` 就切不出列；BFD 的 `Session state` 行有**两列**
   （本端 `$3`、对端 `$4`），只看 `$3` 会漏掉远端视角。
2. 这个 build 里 `show wireguard peers`、`show mpls table` 都不存在，wg 口名只能从
   `show interface addr` 拿。
3. 发送方自己的 `enp0s9` 上 tcpdump **看不到 VPP 自己发出的包**（VPP 从 af_packet 直接发），
   所以"发送侧 TX 缺失"不能用这个口径证明。
4. guest 里 `%st` 恒 0 且没有 `/proc/softnet_stat`，别拿这两个数下"宿主无关"的结论。
5. 0.35s 的单调时钟间隙阈值对 150ms 的 BFD 检测窗来说太粗，会漏掉大半事件。
6. 盒子和盒子、盒子和 Mac 之间时钟真的会漂：CPE3 比 Mac 慢约 152s，cust3 与 CPE3 之间
   实测偏移 28929.68s（5 次测量散布 28929.679–28929.695，误差界 ±0.1s，用 ssh 里
   `date +%s.%N` 对拍）。跨表只能对"时刻形状"，不能对"同一秒"。
7. Mac 的 `date` 是 BSD 版，`+%s.%N` 会把 `%N` 原样打出来；改用 `python3 -c 'import time'`。
8. `pkill -f "ping -I"` 会把执行它的 ssh shell 自己杀掉；要收 ping 用 `pgrep -x ping` 再逐个 kill。

### 9.9 缺陷⑥（已量化，方案待拍）：亚秒级抖动被产品自己放大成 1 秒以上、最长 12 秒的客户黑洞

场景：§9.8 说明了链路为什么会抖零点几秒。这一段说的是**我们的控制环把这个抖动
放大成了什么**，放大链全在产品代码里，与 lab 无关 —— 真实 WAN 上 150–900ms 的抖动
本来就是常态。

代码一步步走到哪：

1. 隧道建好时 agent 给每条隧道建一个多跳 BFD 会话，参数取
   `internal/wire/wire.go:887-889` 的三个默认值：`RequiredMinRx=50000us`、
   `DetectMult=3` ⇒ VPP 自己报的检测时间就是窗口里量到的 `150000us`（**150ms**）。
2. sr-fib 每 `poll_sec` 秒读一次这两样：VPP 的 BFD 会话 dump
   （`cmd/sr-fib/main.go:235` 的 `converge()`）和 sr-route 的 RIB。VM 台盒子上
   `/etc/sdwan/fib.json` 写的是 `poll_sec: 1`，**产品默认值是 2**（`cmd/sr-fib/main.go:99-100`）。
3. dump 回来的会话只要不是 `BFDStateUp` 就进不可达表（`internal/fib/reach.go:21` 的
   `UnreachablePeers()`），下一句就是 `internal/fib/fib.go:308` 的「不是 Up，该路径跳过」——
   **没有抑制、没有连续 N 次确认、没有 hold-down**，一次采样就把这条路径从计划里拿掉。
4. 计划变了就直接改 VPP FIB：`routes+0/-N` 摘、下一轮再 `routes+N/-0` 装回
   （日志行格式在 `cmd/sr-fib/main.go:264`）。

放大后的实测数字（CPE3 的 `fib.log`，2026-11-04 22:57 → 11-05 02:35，218.2 分钟窗口，
日志尾 4000 行）：routes 装回 196 次、摘掉 144 次；「摘一轮 → 下一轮装回」共 90 组，
**间隔 p50 / p90 / max 全 = 1s，>2s 的 0 组，连续多轮装不回的段最长 1 轮** ——
黑洞长度就等于一个轮询周期，因为除了轮询什么都没有。
按（前缀 × 下一跳）分：`172.16.4.1/32↔10.250.0.6` 被摘 96 轮（≈1380s 无该路径），
`172.16.12.1/32↔10.250.0.7` 62 轮、`↔10.250.0.1` 59 轮。

客户可见侧（各自单时钟，不做跨表对齐，见 §9.8 第 6 条）：§9.7 那两段 1 秒一次长 ping
里，`172.16.12.1` 有 5 段连续 4–12 秒全断、`172.16.4.1` 有 6 段 2–10 秒全断；
盒子侧日志同一时间尺度上能对上的是「同一秒该前缀的两条路径一起被摘」：CPE3 61 秒、
CPE4 70 秒，最长连续 6 秒 / 4 秒；单路径前缀被摘的秒数 CPE3 118 秒、CPE4 123 秒。
两边量到的都是**没有路径可用**的时长，只是口径一个是业务、一个是控制面。
另外四台各 8.5 小时的窗口里摘路次数 138 / 123 / 176 / 190，日志文件都涨到 3.5–4.5MB
（其中相当一部分是 22:41–22:57 那段「本轮收敛失败：读 RIB: … connection refused」，
那是 sr-route 口没在听，不是这条链）。

为什么这条不能算「lab 的噪声所以不管」：把 150ms 的抖动写成 1 秒（产品默认 2 秒）的黑洞，
是控制环自己的策略，不是链路的性质。真实设备上客户看到的就是 §9.7 表第一行那种 8% 丢包。

待拍的方案（要他点头才动代码，先不动）：摘路要不要有抑制窗口 / 连续几次确认才算 Down；
BFD 检测时间和 sr-fib 轮询周期要不要有约束关系（现在 150ms vs 1000ms 完全脱钩）；
以及"抖动"这件事要不要在控制台上如实显示成设备备注，而不是只涨日志。

### 9.10 S7 收口实测：verify / status 的口径从 docker exec 迁到 ssh（2026-11-05）

场景：容器台的判据脚本 `test/lab/verify.sh` 每一条检查都是 `docker exec lab-cpeN …`。
VM 台从 S2 起没有容器（控制面跑在 Mac 上，设备是 7 台 VirtualBox 虚拟机），所以 S7 要的
是同一套口径、但走 ssh 的脚本。这一轮落地的是四个文件：

- `test/lab/vmlab/vm-verify.sh`：判红绿，有退出码。容器台 verify.sh 的孪生。
- `test/lab/vmlab/vm-status.sh`：只读快照，给人读数字、不判分（十段，四台盒子 + RR + 三台客户机）。
- `test/lab/vmlab/vm-ledger.py`：期望值台账。隧道 / 端口 / 颜色 / MTU / overlay /32 全部取自
  控制台 `GET /api/underlay`（S6 之后隧道只在页面上配，脚本里硬抄必然过期）；只有客户侧
  loopback、cust 虚拟机 ssh 别名、盒内客户口地址是文件顶部的手工表（拍板 5：lab 值手工配）。
- `test/lab/vmlab/vm-state.py`：把盒子里 sr-route(:19001) / sr-fib(:19011) 和 Mac 上 RR 的
  `/status` JSON 与台账逐条对账。判据不等的直接 `SystemExit` 打差异；相等时打一行**数字**旁证，
  `vm-status.sh` 原样打印这一行 —— 所以体检和快照看到的是同一个数。

两份 shell 脚本共用 `VM_LEDGER`（默认 `/tmp/vm-verify-ledger.tsv`，本轮 36 行：node 4 / box 4 /
tun 10 / rib 8 / local 4 / ping 6）和同一个 `ssh_run`。台账重跑一次 `python3 vm-ledger.py`
与在用的那份逐行相同，说明期望值里没有当场拍出来的数。

#### 三类假绿（都真实发生过，不是预防性改动）

**类一：不带 `-n` 的 ssh 把循环的 stdin 读走了。**
`while IFS=$'\t' read -r …; do ssh …; done < <(grep '^box' "$LEDGER")` 这种写法里，
stdin 就是台账。ssh 自己会消耗 stdin，于是循环只跑第一行就结束 —— 后面的判据**根本没执行过**，
退出码却照旧是 0。症状实测：§9 的六方向互 ping 只打出来一条。改法是 `ssh_run` 里固定
`ssh -n`（`vm-verify.sh:85-88`、`vm-status.sh:63-66`）；本脚本没有任何地方靠 stdin 往盒子里喂数据
（远端的 `<` 在引号里，那是盒子自己读的）。

**类二：`python3 -` 的程序体没落进去，执行的是空程序。**
§5 那十条 JSON 对账原本是 `python3 - <参数> > note`，程序体本该用 heredoc 从 stdin 灌进来。
但 heredoc 要占的 stdin 在类一那个循环里就是台账 —— 少程序体的那一版退出码 0、`note` 文件
0 字节，十条判据全部成立（假绿）。改法是程序体放进 `vm-state.py`，参数走 argv：stdin 不再是
接口，这类空转没有地方发生。文件头部把这条教训写死了，就是为了下一次别再改回 heredoc。

**类三：ssh 的 known_hosts 警告会混进数据里，而且压不住。**
第一次连一台盒子会打 `Warning: Permanently added '192.168.1.101' (ED25519) to the list of known hosts.`。
2026-11-05 实测：这一句**不受 `-o LogLevel=ERROR` 也不受 `LogLevel=quiet` 控制**，照打。
`ssh_run` 里是 `2>&1`，所以它会进 stdout 的捕获串：第一版快照里 cust 的 `lo：` 那格值前面就粘着
整行警告（引号里正好是盒子自己的 IP，肉眼很容易当成数据）。到了 §5，同一行会把紧凑 JSON
打断 —— 解析器报错是**假红**（比假绿好，但同样是噪声）。改法是按行滤掉那一句
（`grep -v '^Warning: Permanently added'`），同时把 ssh 自己的退出码 `return $rc` 出去，
两份脚本开头都加了 `set -u -o pipefail`：没有 pipefail，`ssh … | tr -d '\r'` 的退出码是 `tr` 的 0，
ssh 连不上、远端命令失败都会被当成成功（AGENTS.md §2「不许靠退出码猜状态」的反面）。

#### 快照的十处字段位错读：假数比 0 更危险

`vm-status.sh` 第一版的绝大多数错，都是**没量输出形状就按猜的字段位取数**。每一处都补了实测：

| 打出来的样子 | 根因 | 改法与实测值 |
| --- | --- | --- |
| `WG peers 0 个，flags: 2 的 0 个` | 命令名写成 `show wireguard peers`（复数），VPP 里没有这条 ⇒ 空输出被当成 0 | `show wireguard peer`；四台实测 2/2、2/2、3/3、3/3，全部 flags=2 |
| `linux-cp 配对 0 组` | `grep 'lcp itf'`，`show lcp` 里根本没有这个字串 | `grep 'itf-pair:'`；每台实测 2 组（host-enp0s8→lcp-x0、host-enp0s9→vpp0） |
| `检测时间 (usec)` | BFD 摘要行取 `$3`，那是单位不是数 | 取 `$NF`；实测 `150000us` = 50ms×3（缺陷⑥ #262 的判定窗口） |
| `内核 mtu qdisc` | `ip -o link` 的 mtu 在 `$5`，旧版取 `$6` | 实测 1544 |
| `table 0 有 172.16 前缀 0 条` | 前缀行的正则要求行尾有空格，VPP 输出是裸行 | `^172\.16\.[0-9.]+/[0-9]+$`；实测每台 3 条 |
| 路径列成 `via[mpls ipv4]` | `awk '/via /{print $2}'` 把标题也当路径，且同一条路径在 path-list 段和转发段各出现一次 | 跳过 `stacked-on` 之后再用 sed 提 `mpls\|ipv4 via <nh> <口>`，加 `!seen[$0]++` 去重 |
| OSPF 邻居状态是数字 | 状态取 `$2`（那是 priority） | 取 `$3`；实测 `Full/DR`、`Full/DROther`、`Full/Backup` |
| BGP 邻居多出一行 | `/^Neighbor/{h=1;next} h&&NF` 会把 `Total number of neighbors …` 当成邻居 | 只收 `/^[0-9]+\./` 行，字段取对端 AS/收/发/uptime（实测 `172.31.3.2 AS 65003 收到 1 条 / 播 4 条`） |
| `异常计数行 4 条` | `grep -ciE 'fail\|error\|drop'` 数的是计数器**标签行**，恒等于标签数 | 只列非零计数项；实测四台 `no_path/hmac_fail/drop_total` 全为 0（`bypass` 单列并注明不是异常） |
| 报告里混进原始 VM 行 | `awk -F'"'` 写在 `"$( … )"` 里，bash 3.2 报 `awk: syntax error` | `VBoxManage list runningvms \| cut -d'"' -f2`；实测 7 台在跑 |
| `MTU 1544 = netdev 真实值` | 归因写反了：1544 是 VPP `set interface mtu packet 1500` **反写**给 netdev 的脏值（§5.10 已记） | 快照并列三格：VPP `host-enp0s8` MTU(L3/IP4/IP6/MPLS)=`1500/0/0/0`、期望（`underlay.list` 第 6 列）`1500`、内核 netdev `1544` 并注明不参与判定 |

还有两处是**文案在造假**，不是解析错：`sr-box=inactive` 一开始读起来像服务挂了 —— 实测
`systemctl status` 后写明它是 oneshot（开机清单跑完就该是 dead），看门狗是 timer 常驻 +
oneshot 每 60 秒跑一次，快照改成 `常驻 vpp=active / 开机清单 sr-box=…（oneshot，dead 为正常）/
timer=active`，并把看门狗最近一次退出码与时刻打出来（实测四台 `退出码=0`）；`未上报` 也一度
用在「合法为空」的地方（没有非零异常计数、客户机没有默认路由），现在分别写成
`全为 0（no_path/hmac_fail/drop_total 等）` 和 `无（本站靠上面的 /32 明细互通）`，
`未上报` 只留给真的没数据（AGENTS.md §2：设备没上报就显示未上报，不显示 0 或占位值）。

#### 改完的前后数字（AGENTS.md §2）

- 基线（迁移第一版）：§9 六方向只打出 1 条、§5 十条 JSON 判据的 note 全是 0 字节、快照 11 处
  字段错读。这些不是"数字难看"，是判据没跑或跑了假的。
- 改完复跑两次 `bash test/lab/vmlab/vm-verify.sh`（ssh_run 换实现 + `pipefail` 之前、之后各一次）：
  **PASS=388 FAIL=0 WARN=0**（日志 `/tmp/vm-verify-run.log`）与
  **PASS=385 FAIL=0 WARN=3，退出码 0**（日志 `/tmp/vm-verify-run2.log`）。
  两组数只差在 §9 的三条客户 ping：这一轮它们各有 1–4 个包没回来，同一窗口四台 `fib.log`
  确实记了摘路（`cust12→172.16.3.1` 20%、CPE 侧 1/1/1/2 条；`cust4→172.16.12.1` 20%、2/2/2/4 条；
  `cust4→172.16.3.1` 5%、3/2/2/4 条）⇒ 按设计算 WARN 不算 FAIL，归因缺陷⑥（#262）。
  摘不到路的丢包仍然是 FAIL，这一轮 FAIL=0。
- 改完：`bash test/lab/vmlab/vm-status.sh` → 退出码 0，**277 行 / 十段**全部有实测值，
  无一处 `未上报` 是解析失败造成的（日志 `/tmp/vm-status-run3.log`）。
- 缺陷⑥ 的可观测面继续在同口径增长：本轮四台 `fib.log` 摘路 397 / 401 / 515 / 533 条
  （日志文件 26.5k–28.0k 行），最近一条都在 04:52–05:00 UTC（盒子本地时钟，不与 Mac 对齐）。
- 宿主仍是超订状态：load 15.75 / 17.11 / 17.43（16 核），Mac 上 12 个 Docker 容器 + 7 台虚拟机
  同时在跑 —— 这是 §9.8 那条链路抖动的来源，不是 lab 配置问题。

#### 怎么跑（两份脚本都是只读）

    python3 test/lab/vmlab/vm-ledger.py > /tmp/vm-verify-ledger.tsv   # 期望值台账：控制台 GET /api/underlay + 两张手工表
    bash test/lab/vmlab/vm-verify.sh     # 判红绿，无 FAIL 才退出 0；--quiet 只打结论，--no-shape 跳过 §2（8 次 × ~15s）
    bash test/lab/vmlab/vm-status.sh     # 十段快照，给人读数字、不判分

`vm-verify.sh` 在台账缺失时直接停下来要你先跑第一条（不自己生成，避免"用的是当场拍出来的期望值"）；
`vm-status.sh` 会自己调 `vm-ledger.py` 补一份。前提只有两条：Mac 上 sr-ctrl 和双 RR 在跑，
`~/.ssh/config` 里有 CPE1..CPE4 / cust12 / cust3 / cust4（cust 走 ProxyJump 到自家盒子）。
两份脚本都不改配置、不重启任何进程（AGENTS.md §1）；唯一副作用是 §2 里 `check-selfheal.sh`
自己的探测 ping 与 `clear errors`，以及 §9 的 ping 流量。

#### S7 没覆盖的，点名不静默

1. **`arp_ignore` 在 VM 台只是显示，没进判据。** 容器台 `test/lab/verify.sh:217-222` 判
   `arp_ignore=1`，`vm-verify.sh` 现在只有 `vm-status.sh` §4 打出数值（实测四台都 =1）。
   要不要照抄成判据，等他点头（控制器渲染路径在 `controller/cmd/sr-ctrl/config_render.go:303`，
   属于产品该保证的，不是 lab 自己配的）。
2. 没有独立终端机（容器台的 lab-termN）：客户视角用 cust 的 loopback 代打，链路里少一跳。
3. 站点 12 没配 VRRP（S6 拍板这轮不发），主备切换不在判据里。
4. 缺陷④（#260）已在 §9.12 收口（VM 台三段实测），缺陷⑥（#262）还没动代码。④的**显示口径**
   在 verify 里封住了（`vm-verify.sh:370-377`：回执 `ok=true` 必须和 §3 实测的口名/绑定计数同时成立，
   两个口径并排打出来），触发场景（单独重启 VPP 后 agent 不重建 wg 口）现在也有判据外的实测数字。
   ⑥这一轮抓到 3 条 WARN，就是它在客户视角的复现，方案（抑制窗口 / 检测时间与轮询周期的约束关系）还等他拍。

### 9.11 S8 把手：lab 现在怎么自己复核、怎么登进去、断电之后怎么拉起来（2026-11-05）

#### 登录（复核用这一个账号就够）

- 控制台 `http://127.0.0.1:8088`（sr-ctrl 只绑 127.0.0.1，与容器台同口径）。
- 账号 `vm-tenant`，角色**租户**，租户 `tenant-vm`。明文口令不进仓库、不进日志
  （AGENTS.md §3）；它落在他本机的 `~/srvmlab/state/credentials.json`（0600），
  要读就 `python3 -c "import json;print(json.load(open('/Users/louis/srvmlab/state/credentials.json')))"`。
- 这一轮实测过登录本身：`POST /api/login` 返回 `{"ok":true}`（HTTP 200），同一会话
  `GET /api/state` 打出 4 台设备全 `online`（`sr-agent/0.1 (identity+heartbeat)`，
  site 名在 `runtime.site_name`），`GET /api/underlay` 打出 5 条隧道（t2/t3/t4/t6/t7，
  颜色 0、MTU 1368）。页面侧当前会话的租户视图：设备 4/4、在线 4、失联 0、未定位 0。
- 运营方视角（建租户/授权/RR 库那几页）没有账号，用启动参数 `-admin-token` 那份
  token 换会话；token 在 `~/srvmlab/state/console-token`（0600），同样不进仓库。

#### ssh 把手（七台，密钥 `~/vm-lab/keys/id_ed25519`，root）

| 别名 | 地址 | 说明 |
| --- | --- | --- |
| `CPE1`…`CPE4` | 192.168.1.101–104 | 四台 vCPE 盒子（VPP + FRR + 四个 sr-* 进程） |
| `cust12` | 172.31.12.13，`ProxyJump CPE1` | 站点 12 的客户机，lo 上播 172.16.12.1，跑 OSPF |
| `cust3` | 172.31.3.2，`ProxyJump CPE3` | 站点 3 的客户机，lo 播 172.16.3.1，跑 BGP AS 65003 |
| `cust4` | 172.31.4.2，`ProxyJump CPE4` | 站点 4 的客户机，lo 播 172.16.4.1，跑 BGP AS 65004 |

一条自己就能看到的业务判据（客户视角，源必须是 loopback —— overlay 只承载 172.16/16 的 /32）：

    ssh cust12 "ping -c 5 -I 172.16.12.1 172.16.3.1"

2026-11-05 实测：`5 packets transmitted, 5 received, 0% packet loss`，rtt 11.49/16.69/24.31ms。
偶尔出现 1–4 个包的丢失不算回归 —— 那就是缺陷⑥（#262）在客户视角，`vm-verify.sh` 会把它
打成 WARN 并把同窗口的摘路条数一起打出来。

#### 检查这两条，别用手敲的口径

    bash test/lab/vmlab/vm-verify.sh    # 判红绿（退出码 0 = 无 FAIL）；台账缺失会先要 vm-ledger.py
    bash test/lab/vmlab/vm-status.sh    # 十段只读快照，数字与 vm-verify 同源

#### 断电/Mac 重启之后

1. 拉起七台虚拟机：`for v in CPE1 CPE2 CPE3 CPE4 cust12 cust3 cust4; do VBoxManage startvm "$v" --type headless; done`。
   实测 `Autostart Enabled: disabled`（七台都是），所以开机不会自己起 —— 要不要打开自启等他拍
   （代价：每次开 Mac 就同时跑 7 台，现在宿主 load 已经在 15–17/16 核）。
2. 盒子内部自愈不需要手工：`vpp.service` + `sr-box-watch.timer` 每 60 秒跑一次 start.sh 清单
   （本轮四台看门狗最近一次 `退出码=0`）。隧道层是磁盘恢复不了的（§5.10 / 配置分层记忆），
   所以控制面必须先起来。
3. Mac 侧控制面：`bash ~/srvmlab/restart-ctrl.sh`（幂等：已在跑的 pid 打出来不动它）。
   起 sr-ctrl（8444 设备口 / 8088 控制台 / STUN 3478+3479）+ 双 sr-rr。
   设备侧靠 `/v1/watch` 重连并重新拉包。**"控制器整个重启一遍"不在本轮判据里** ——
   这一条按 AGENTS.md §1 第 3 条（重连不许让数据面抖动）当作要求记着，但没有这轮的实测数字，
   要用之前先照 §9.10 的口径补一次前后对比。

#### 等拍的（这一轮没有替我决定任何东西；缺陷④ 已于 §9.12 收口）

1. 缺陷⑥（#262）：摘路要不要抑制窗口 / 连续确认，BFD 检测时间与 sr-fib 轮询周期要不要有约束关系。
2. ~~缺陷④（#260）~~ —— 已修并实测，见 §9.12。它留下两条**新的**待拍，写在 §9.12 末尾：
   sr-fib 要不要补 VPP API 重连、缺口 G-2（隧道表的归属）由产品接管还是让 lab 把手常驻。
3. `arp_ignore=1` 要不要像容器台那样进 VM 台判据（§9.10 未覆盖第 1 条）。
4. 七台 VM 的 VBox 自启要不要打开。

### 9.12 缺陷④（#260）收口：VPP 单独重启后，agent 现在自己否掉旧回执、换掉死句柄、第一轮重建（2026-11-05，A→D 四段实测）

#### 缺陷长什么样（场景）

CPE4 上跑着三条隧道（wg1003 / wg1004 / wg1007）、BFD 三条全 Up、六个方向的业务 ping 0% 丢包。
这时候只重启 VPP 一个进程（`systemctl restart vpp`）。VPP 的配置只在内存（§5.10「隧道层磁盘恢复
不了」），所以它一起来，三个 wg 口连同 BFD 会话一起没了。管理通道由 `vmlab-underlay.service`
补接管（S4-C 实测 6.094s 自愈），所以 ssh 还进得去 —— 但客户流量已经全断。

按 §9.10 的口径读一遍盒子：

```
pid=202309 wg=0 names=none bfd=0 bfd_up=0
"tunnel":{"capable":true,"applying_version":8,"applied_at_unix":1793860776,"ok":true,"tunnels":3}
```

控制器上看到的是「v8 装上、3 条隧道、ok=true」，数据面上是 0 个 wg 口。这就是 #260 的原句：
**不重建，还报成功**。

#### 代码一步步走到哪（为什么它会报成功）

1. `TunnelSync` 的结论存在 `t.state` 里（`internal/agent/tunnel.go` 的 `State()`），它是**上一次
   成功收敛那份回执的拷贝**。`applied_at_unix` 是上一次成功的时间，不是"现在还成立"的断言。
2. 心跳把这份 state 附到回执上（`cmd/sr-agent/main.go:335` 一带：`req.Tunnel = st.tunnelReport()`），
   控制器照它显示。
3. 重建的唯一触发点是隧道对账循环 `Sync()`（`tunnel.go:409`），它被 Kick 唤醒后先看闸：
   `version == desired && OK` 就直接 return —— VPP 重启没有带来新版本号，控制器那边的期望还是 v8，
   而 state 自己不会变。**于是现成的自愈那段代码（清句柄 → 重连 → 按快照从头收敛）一步都走不到。**
4. 也就是说这个洞没有"到期"的时候：state 永远说 v8 装好了，直到有人改隧道配置或重启 sr-agent 进程。
   阶段 A 实测到 1065 秒（约 18 分钟）它还是 `ok=true tunnels=3 / wg=0`，客户 ping 93.33% 丢包。

#### 第一半：让心跳那份接口清单来否掉回执（已提交的那次改动）

盒子上的心跳本来就带一份数据面接口清单（`wire.HeartbeatRequest.Ifaces`，由
`agent.NewInventoryReporter` 读，`cmd/sr-agent/main.go:205`），不用新读一次数据面。
`TunnelSync.VerifyDataplane`（`tunnel.go:288`，实现 `verifyDataplaneWith` `:306`）拿这份读数去核对
上一次回执声称装好的口名，按**接口名**（`wg<user_instance>`）而不是 sw_if_index 判，因为重建后
index 会变而名字是产品口径。`ifs == nil` 是"这次没上报"，不判；空清单是一次真实读数，判。

阶段 C 装的就是这一半（sr-agent md5 `4031aab871da0c0a695615290cae55a4`）。它生效了 —— 心跳一到就把
结论否掉（`/var/log/sr-agent.log`，盒子钟 07:03:46）：

```
隧道回执被数据面否掉：数据面里没有回执声称装好的 3 条隧道口（wg1003、wg1004、wg1007）：VPP 重启过？下一次收敛从头来
```

**但否掉之后盒子并没有好。** 同一个窗口里接着是 13 次一模一样的失败，退避一路涨到上限：

```
07:03:46 隧道配置 v8 没装上（连续 1 次，5s 后重试）: 读接口表（解析隧道出口）: write unix @->/run/vpp/api.sock: write: broken pipe
07:03:56 连续 2 次，10s 后重试
07:04:06 连续 3 次，20s 后重试
07:04:26 连续 4 次，40s 后重试
07:05:06 连续 5 次，1m20s 后重试
07:06:27 连续 6 次，2m40s 后重试
07:09:10 连续 7 次，5m0s 后重试   ← 从这里开始封顶
07:39:25 连续 13 次，5m0s 后重试   ← 25 分钟后还在撞同一句话
```

阶段 C 的 170 秒采样窗口里，`wg` 一直是 0、`bfd_up` 一直是 0（采 146 行，状态形状只变过两次）。
所以这一半只把"骗控制器"治住了，没把"重建"治住。

#### 第二半的根因：对端关掉的连接，vppapi 没归一成 ErrClosed

重建路径其实一直在等一个信号。`Sync()` 每轮完都会调 `dropDeadConnection(err)`
（`tunnel.go:416`），它的门是（`tunnel.go:266-269`）：

```go
if err == nil || !errors.Is(err, vppapi.ErrClosed) {
    return
}
```

这道闸的注释（`:259-265`）写的正是这件事：不动就会"拿着一条死句柄每轮撞同一个错，盒子停在
配好了但没在跑"。它当时没触发，原因在 `vppapi` 的写路径：

- `sendErr()`（改前）只在 `case <-c.closed:` 那一个分支里把错误包成 `ErrClosed`。`c.closed`
  只有在**我们自己调过 `Client.Close()`** 时才会关（`Close()` 在 `:131-139`）。
- VPP 重启是**对端**把 API 连接拆了。这时 `c.closed` 没关，`writeFrame` 写回来的是一个裸 errno，
  文本就是日志里那句 `write unix @->/run/vpp/api.sock: write: broken pipe`。
- 读循环 `readLoop()`（`:239`）确实把断开原因记进了 `c.err`（`:260` 的 `c.err.Store(&e)`），
  但它从不关 `c.closed`，所以那条信息在 `sendErr` 里根本没人看。

`errors.Is(裸 EPIPE, ErrClosed)` 恒为 false ⇒ `dropDeadConnection` 恒为 no-op ⇒ 句柄永远是那条死的。
同一条客户端还被观测表回报复用（`cmd/sr-agent/report.go:107`），所以那一分钟里日志全是同一句话的复读。
对照组很说明问题：只读那份接口清单的 `InventoryReporter` **本来就有**重连纪律
（`internal/agent/inventory.go:80-88`：读失败就 `Close()` 并置 nil，下次心跳重新注册），
所以同一次故障里它只报了一句"这次心跳不带清单"就恢复了 —— 缺口只在隧道这条写路径上没有归一。

#### 动了什么（只动一处）

`controller/internal/vppapi/vppapi.go:513` 的 `sendErr` 现在两条路都归一：自己关过的（`c.closed`）
和**对端**关掉的（`c.err` 里有读循环记的原因，或 errno 本身就是传输层的死法）。新增的判据
`connGone()` 在 `:528`，只列传输层：`net.ErrClosed` / `io.EOF` / `io.ErrClosedPipe` /
`EPIPE` / `ENOTCONN` / `ECONNRESET` / `ECONNABORTED` / `ESHUTDOWN`。
`APIError{Op,Retval}`（`:95`）、"消息表里没有这个请求"、`context.DeadlineExceeded` 这些
"连接好好的、请求本身有问题"的错误**不**在里面 —— 那些必须原样抛出去，否则调用方会拿配置错误去重连。

`tunnel.go` 一行没改：它的自愈段本来就写好了，只是从来没被允许执行。

单测（`controller/internal/vppapi/vppapi_test.go`）：
- `TestPeerCloseReportsErrClosed`（`:1598`）——假 VPP 从**服务端**关掉那条连接
  （`fakeVPP.connCh` + `killConn()` `:534`，复现 VPP 重启的真实次序：没人调 `Client.Close`，先死的是对端），
  然后一次 `SetPKey` 必须是 `errors.Is(err, ErrClosed)` 且在 2 秒内返回。
- `TestConnGoneOnlyMatchesTransportDeath`（`:1618`）——把上面两组错误逐条钉住，防止以后有人图省事写成
  "凡是错误都算断连"。

变异测试（诚实起见记下覆盖边界）：把新条件短路成 `if false` 重跑，
`TestPeerCloseReportsErrClosed` 失败并打出生产里那句原文
`write unix @->/tmp/.../api.sock: write: broken pipe`（说明这条端到端测试真的抓得住缺陷）；
`TestConnGoneOnlyMatchesTransportDeath` 仍然过 —— 它直接测判据本身，不经过那段短路，
所以"能抓住回归"这句话只对第一条测试成立。改完恢复文件，md5 与留档一致
（`7e8cb250a707f0757c07bdf7a1732b2a`）。

容器 `fix4-test`（srwg-dev / go1.22.5 / `--privileged`）里：`go build ./...` 通过，
`go test ./...` 15 个包全 ok，无失败。

#### VM 台四段数字（同一台 CPE4，同一口径，注入的都是 `systemctl restart vpp`）

采样口径 `/root/fix4-sample2.sh` 每 0.5s 一次：VPP pid / wg 口数与口名 / `show bfd sessions`
的会话数与其中 Up 的条数 / agent 自己的 `/status` 隧道回执。打流口径：cust4 上
`ping -i 1 -W1 -D -I 172.16.4.1 172.16.12.1`（源是本站 loopback，§9 口径；用 LAN 地址打源会被设计
性地黑洞，那种数不作证据 —— 我在这条上栽过一次，写在这儿防重犯）。断流分析用 cust4 自己的钟，
不跨主机对齐（盒子钟比 Mac 慢约 2.4 分钟，跨源归因一律按 `grep -n` 行序）。

| 段 | 装的是 | 回执被否掉 | 重建 | 窗口结束时 | 客户 ping |
| --- | --- | --- | --- | --- | --- |
| A | 修复前的 agent | 从不（`ok=true tunnels=3` 而 wg=0） | 无 | ≥1065s 仍未重建 | 93.33% 丢包 |
| C | 只有第一半（`4031aab…`） | 一拍内（07:03:46） | **不**：13 次撞同一 errno，退避 5s→10→20→40→80→160→300s 封顶 | 170s 窗口内 wg=0/bfd=0 | 回包停在 seq 10，之后全断 |
| D | 两半都在（`3dba22f2d66c2fb56f80c34d4f50f5ae`） | 一拍内（盒子钟 07:46:16，同一轮里就报了"连接断了"） | **是**：连续 1 次失败后按 5s 退避重试，那一次重连成功 | 三次采样形状：`wg=3/3up` → `wg=0/0up` → `wg=3/3up` | 见下 |

D 段的完整自愈弧（`/var/log/sr-box/agent.log` 第 697-705 行，按行序就是时间序）：

```
07:46:16 隧道回执被数据面否掉：数据面里没有回执声称装好的 3 条隧道口（wg1003、wg1004、wg1007）：VPP 重启过？下一次收敛从头来
07:46:16 VPP api 连接断了（读接口表（解析隧道出口）: vppapi: 连接已关闭: write unix @->/run/vpp/api.sock: write: broken pipe）：下一次重连，按快照从头收敛
07:46:16 隧道配置 v8 没装上（连续 1 次，5s 后重试）: ...
07:46:21 隧道 t3 → node 1（192.168.1.101:20001）：peer installed
07:46:21 隧道 t4 → node 3（192.168.1.103:20001）：peer installed
07:46:21 隧道 t7 → node 2（192.168.1.102:20001）：peer installed
07:46:21 隧道配置 v8 已收敛
```

和阶段 C 那 13 次比：**一次失败、5 秒、然后收敛**。注意那句错误文本变了 ——
`vppapi: 连接已关闭: write ... broken pipe`，前缀就是这次归一的产物，也是闸能开门的凭证。

采样侧的数（Mac 钟）：重启命令发出 1793864911.320 → 1793864914.086 首次看到 `wg=0`
→ 1793864923.009 回执变成 `{"capable":true,"applying_version":8,"error":"读接口表（解析隧道出口）:vppapi:连接已关闭:…broken pipe","tunnels":3}`，
`version` 和 `ok` 两个字段都不再出现（`TunnelState` 上是 `omitempty`，被否掉的回执由 `verifyDataplaneWith`
把 `OK` 清成 false、`Version` 清成 0 —— `tunnel.go:329-332`）。要读准的是：**`tunnels:3` 是上一次收敛的
遗留计数，它现在必须和 `ok=false` + 那句 error 一起看**，这正是 §9.10 里 `vm-verify.sh:370-377`
把两个口径并排打出来的原因 —— 单看条数会误读成"还有 3 条"
→ 1793864928.142 `wg=3 names=wg1003,wg1004,wg1007 bfd=3/3up agent ok=true v8 tunnels=3`。
**从发出重启到隧道层整体回来 17 秒，其中前 ~11 秒是 VPP 自己起进程 + underlay 补接管（§5.9 的 6.094s 那一档）；
agent 从"发现不一致"到"重建完成"是 5 秒、一次重试。**

重建出来的是真能转发的口，不是记账：三条隧道各打一次隧道层 ping
（`vppctl ping <对端 overlay> source <载着本机 /32 的那个 wg 口>`）——
`10.250.0.3 via wg1003`、`10.250.0.5 via wg1004`、`10.250.0.9 via wg1007` 各自 **5 sent, 5 received, 0% loss**。
BFD 是收敛时按隧道口建的（`tunnel.go:641` 的 `tel.Ensure`），所以它跟着一起回来了（3/3 Up），不用单独自愈。

#### 顺手补上的那条欠账：控制面进程重启不让数据面抖动（这次有数字了）

§9.11 结尾写着"控制器整个重启一遍没有实测数字"。这轮量的不是控制器，是**盒子上的 sr-agent 进程重启**
（就是我这次部署动作本身，所以必须点名）：CPE4 健康态下 `pkill -x sr-agent` + `systemctl start
sr-box-watch.service`（盒子自己的清单单元，幂等），cust4 的 1pps 打流跨整个过程：

```
REPLIES=40 first_seq=1 last_seq=40 MISSING_SEQS=0
TOP_GAPS=1.02s(seq17→18) | 1.02s | 1.01s | 1.02s | 1.01s
40 packets transmitted, 40 received, 0% packet loss
```

1.02 秒就是 1pps 的节拍本身（打流口径的相邻两包天然差 1 秒），不是断流。进程侧：
`sr-agent 218033 → 219051`，而 `sr-route=94739 sr-fib=218389 sr-frr=94763 vpp=202309` 四个 pid 一个没动，
overlay 前缀数 3 → 3。**结论：升级 sr-agent 这个动作对客户流量是 0 丢包**（数据面没有任何一个包经过 sr-agent）。
"整个控制面（sr-ctrl + RR）重启一遍"仍然没有数字，仍然是欠账。

#### 没覆盖的边界，点名（这是两条待拍，不是这次修复的漏洞）

D 段隧道层自愈之后，业务 ping **没有**自己回来。原因不在这条路径上，是另外两件事：

1. **sr-fib 不重连 VPP API。** `cmd/sr-fib/main.go:383` 只 Dial 一次，整个进程里没有第二处拨号；
   VPP 重启后它每秒报一行，`/var/log/sr-box/fib.log` 从注入起一直刷到我看的时候：
   `本轮收敛失败（保留上次成功的计划）: 读 BFD 会话: write unix @->/run/vpp/api.sock: write: broken pipe`。
   同一个 broken pipe，同一个根因（`sendErr` 的归一）在 sr-fib 里也是死的 —— 但 sr-fib **没有**
   sr-agent 那条 `dropDeadConnection` + `redial` 的纪律，所以就算句柄报对了"连接已关闭"，
   也没有人拿着它去重连。要不要照 agent 的样子给它补一条（含补完的前后测量），这是拍板。
2. **缺口 G-2 仍然在**（§9.6）：sr-fib 的活隧道表不落盘、也不由产品维护
   （`/etc/sdwan/fib.json` 里 `"tunnels": []`）。所以重启 sr-fib 之后还要有人把表再推一次。

这轮把盒子恢复回基线用的就是这两步人工把手（顺序固定）：

```
pkill -x sr-fib && systemctl start sr-box-watch.service   # 让 sr-fib 重新 Dial
bash test/lab/vmlab/vm-fib-sync.sh 4                       # 推活隧道表 → converged=True routes_set=2
```

前后同一口径：注入后六个方向 100% 丢包（`cust12→172.16.4.1` 20/0，`cust4→172.16.12.1` 20/0），
做完这两步后 **六个方向 20/20、0% packet loss**，CPE4 的 `show ip fib table 0` 里 overlay 前缀 3 条、
BFD 会话 3 条。盒子现在留在这个状态（隧道 v8 三条、BFD 3/3 Up、六向 0% loss）。

**所以这次修复的边界要说清楚：VPP 单独重启后，隧道层 + BFD 会自己回来（5s / 一次重试），
业务流量不会 —— 业务流量还堵在 sr-fib 那两条上，那两条等他拍。**

### 9.13 缺陷④ 的修复铺到四台：部署动作本身的丢包归因（2026-11-05，含一次我自己写的测试脚本把命令打错盒子的事故）

#### 为什么要再铺一次，而不是"CPE4 验过了就算完"

§9.12 那四段实测全在 CPE4 上做，因为要注入 `systemctl restart vpp` 就得找一台敢动的盒子。
铺开前先看了四台实际在跑的二进制哈希：

```
CPE1 8afcaf618c5e9c33787f4c4eb6c1a8d3   agent pid=54041
CPE2 8afcaf618c5e9c33787f4c4eb6c1a8d3   agent pid=42450
CPE3 8afcaf618c5e9c33787f4c4eb6c1a8d3   agent pid=41595
CPE4 3dba22f2d66c2fb56f80c34d4f50f5ae   agent pid=219051（修复版）
```

也就是说，另外三台跑的还是开机时装上那份**没有修复**的 sr-agent。这时如果我只把 §9.12 交给他，
他在 CPE1 上重启一次 VPP 就会把同一个缺陷再走一遍，而他看到的会是我已经说过"修好了"的东西。
所以这一步不是新功能，是把已经验过的那一份装到剩下三台，并且每台都用同一口径量一遍。

#### 部署动作是什么（逐台一条，全程不碰 VPP、不碰其它进程）

换文件 + 只重启 sr-agent 一个进程，用的就是 §9.12 已经量过的那条接缝：

```
install -m 755 /root/sr-agent.new /usr/local/bin/sr-agent    # 先留一份 sr-agent.pre-fix4 备份
pkill -x sr-agent && systemctl start sr-box-watch.service    # 由清单把 agent 拉回来
```

量到的是三件事，缺一不可：VPP 的 pid 不能变（变了就说明我不小心动了数据面）、
wg 口名不能变（名字变了就说明 agent 把口删了重建，那才真的会打断流量）、
BFD 会话数与回执里的 `ok`/`version` 要对得上。

| | 装前 | 装后（+5s） | VPP pid | wg 口名 | 打流窗口丢包 |
|---|---|---|---|---|---|
| CPE1 | pid=54041 wg=2 bfd_up=2 v8 ok | pid=151940 wg=2 bfd_up=2 v8 ok | 18556 → 18556 | `wg1002,wg1003` 不变 | 160 包缺 2 |
| CPE2 | pid=42450 wg=2 bfd_up=2 v8 ok | pid=109920 wg=2 bfd_up=2 v8 ok | 38300 → 38300 | `wg1006,wg1007` 不变 | 160 包缺 0 |
| CPE3 | pid=41595 wg=3 bfd_up=3 v8 ok | pid=178475 wg=3 bfd_up=3 v8 ok | 9782 → 9782 | `wg1002,wg1004,wg1006` 不变 | 160 包缺 0 |

打流四条方向各 40 包 @1Hz（源一律本站 loopback，`vm-verify.sh` §9 的口径）：
`cust12→172.16.3.1`、`cust12→172.16.4.1`、`cust3→172.16.12.1`、`cust4→172.16.12.1`。
开四条是因为同站点两台（CPE1/CPE2）在 FIB 里是并列路径，一条流未必经过正在重启的那台。
每台重启前后都采 `show sr-wg stats`：CPE1 的 Encap ok 在窗口里从 2404248 涨到 2405544（+1296），
CPE2 从 1021754 涨到 1022978（+1224），所以这两台的窗口里**确实有客户流量经过**，
那个 0% 不是"没流量所以没丢"。

#### CPE1 那 2 个包怎么归因（这一步不能靠"应该无关"糊过去）

CPE1 窗口里 `cust12→172.16.4.1` 缺 seq 33、`cust4→172.16.12.1` 缺 seq 28，各一个包（都是 5%）。
把它们放回时间轴：打流在 16:32:48~16:32:55 起来，重启动作落在 16:33:00~16:33:05，
+5s 的复检在 16:33:14 就已经报回元；两个缺的包按 seq 折回本地时刻是 16:33:19 和 16:33:24，
**落在复元之后 14~19 秒**，不在进程消失的那几秒里。

为了不让这句话只是我单方面解释，补了两组同口径对照：

| 对照 | 做什么 | 样本 | 缺包 | 最长相邻回包间隔 |
|---|---|---|---|---|
| N1/N2/N3 | **什么都不做** | 480 包 | 0 | 1.6s / 1.9s / 4.6s |
| R4 | 再重启一次 CPE1 的 sr-agent（不换文件） | 160 包 | 0 | 1.3s / 1.3s / 2.8s |

结论按能站住的部分说：这个动作在 640 个包（R4 的 160 + 铺开窗口里 CPE2/CPE3 的 320 + CPE4 那轮 160）
里没有造成任何一次丢包；`cust3→172.16.12.1` 方向那种 3~4.6 秒的"回包晚到但没有缺号"的间隔，
**在什么都不做的窗口里同样出现**，所以它是这台 VM 台已有的背景抖动（就是缺陷⑥ #262 的地盘），
不是部署动作的形状。CPE1 那两个包我不能把它记在这次动作头上，但也不把它说成"零风险"——
它是这轮唯一一个没被完全解释的丢包，留在这里，复现条件（同口径四条打流 + 三轮对照）都写全了。

#### 事故：我的采样脚本把命令打到了另一台盒子上，先说这个

第一版铺开脚本跑完 CPE1 的"四台计数"之后，换二进制和重启那一段全都落在了 **CPE4** 上，
日志里能看到 `install: cannot stat '/root/sr-agent.new'` 和 `停前 pid=219051`（那是 CPE4 的 pid）。
原因不是 ssh 也不是并发，是我自己写的 bash：

```bash
allboxes() { for h in CPE1 CPE2 CPE3 CPE4; do ... done; }   # h 没声明 local
rollout()  { local host=$1 ...   # v1 里这个变量就叫 h
```

bash 是**动态作用域**：被调函数里的 `for h` 用的就是调用者那个 `local h`，不是新变量。
于是 `allboxes` 跑完，`rollout` 自己的 `$h` 已经变成循环最后一个值 `CPE4`，
后面的 install/pkill 自然打在 CPE4 上。这也是为什么同一段里"复元"一直判不出来——
它拿着 CPE1 的目标计数去比 CPE4 的输出，而 CPE4 上还没有那份采样脚本，采样是空的。

发现后立刻 `pkill` 停住整条流水线，先核四台的现场，确认破坏范围：
**没有任何一台被换过二进制**（CPE1/2/3 仍是 8afcaf61，只有 CPE4 的 sr-agent 被多重启了一次），
而"只重启 sr-agent"这个动作正是 §9.12 量过、这轮又量了 160 包 0 丢的那一条，所以现场是干净的。
第二版做了三处防呆，不是"改个变量名"就交差：

1. 所有循环变量改成独立名字（`ab`/`pf ph ps pd`），rollout 的参数叫 `host`；
2. 动手前用 `hostname` 断言一次目标是谁，断言不对就中止这台；
3. 采样为空不再当成"还没复元"往下熬 120 秒，连续三次采不到就停在那台如实报失败。

#### 四台终态与自检

铺开以后重跑 `vm-verify.sh`（四台全量）：**PASS=387 FAIL=0 WARN=1**，
唯一那条 WARN 还是 §9 里能归因到缺陷⑥的窗口（`cust12→172.16.3.1` 20% loss，
同窗口摘路条数 CPE1:2 CPE2:0 CPE3:1 CPE4:1），和铺开前同一个形状，不是新引入的。
四台现在都在 `3dba22f2…` 这份上：CPE1 隧道 2 条、CPE2 隧道 2 条、CPE3/CPE4 各 3 条，
BFD 全 Up，回执 v8 ok=true。

这一节没有引入任何新的待拍：§9.11 那几条（sr-fib 不重连、缺口 G-2 的归属、arp_ignore 进不进判据、
VBox autostart、缺陷⑥）一条也没变，还是等他点头。
