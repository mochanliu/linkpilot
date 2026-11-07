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
（`cmd/sr-fib/main.go:273` 的 `converge()`，`fetchStatus()` 在 `:357`），再读 VPP 的
BFD 会话表，然后把两样交给 `fib.PlanFrom(...)`。`PlanFrom` 里每条路径要过两道闸：
第一道是「这个 overlay 下一跳在我自己的隧道表里吗」—— `internal/fib/fib.go:302`，
不在就打上面那句 `该路径跳过`；第二道才是「它的 BFD 是不是 Up」—— `fib.go:308`。
问题在第一道：sr-fib 的隧道表（`{next_hop, interface, color}`）只从它自己的配置文件读
（`cmd/sr-fib/main.go` 的 `loadConfig`/`fibTunnels`），而控制器的配置包六段里**没有这张表**。
第 3 段把 wg 口建在 VPP 上、第 4 段只把「隧道的下一跳地址 + VPN 表」写进
`/etc/sdwan/route.json` 给 sr-route 用，谁都没告诉 sr-fib「10.250.0.2 这个下一跳
是本机的 wg1002、颜色 0」。这个映射只有盒子自己知道，产品里今天没有任何进程维护它。

D3 已经开好了接缝：sr-fib 有 `PUT /config`（`configMux()`，`cmd/sr-fib/main.go:530`），
推完立刻按新表收敛，不用重启。所以这一段的**产品缺口是"谁来调这个接缝"**，不是"接缝不存在"。
按规矩点名：要动的是二选一 —— agent 在收敛完隧道后把这张表推给 sr-fib（它手上已经有
sw_if_index 和本端 overlay /32；**颜色今天不在设备侧契约里**，见下一段），或者 sr-fib 反过来向 agent 要一次。
这是"新增一条下发路径"的拍板，不是修 bug，本轮不动控制器代码。

> 2026-11-06 更正：上面这句"它手上已经有颜色"是错的。`internal/wire/wire.go` 全文没有 color，
> `cmd/sr-ctrl/underlay_delivery.go:134` 构造 `wire.TunnelSpec` 时没有抄 `underlay.Tunnel.Color`
> （`internal/underlay/state.go:145`）。所以 agent 今天不知道自己每条隧道的颜色，
> 颜色必须先进隧道段这条契约，G-2 才谈得上派生。这一条连同四条拍板写进
> `docs/g2-tunnel-table-derivation-plan.md`，代码等点头。

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

#### 现役产物是哪一份（别看错版本）

口径写在他本机的 `~/srvmlab/MANIFEST.md`（不进仓库，避免和提交历史混在一起）。2026-11-05 夜刷新的那一版里三件事要点名：

1. 在跑的只有三件新产物：`sr_wg_plugin.so`（`9267d28d…`）、`sr-agent`（`d2ad8519…`）在四台盒上，
   `sr-ctrl`（`35493b63…`）在 Mac 上。这三件的源内容 = `a5ee41a` + `d949434`。
2. `sr-route` / `sr-fib` / `sr-frr` / `sr-apply` / `sr-rr` **新编了一份但没铺**，四台盒上跑的仍是 11-04 那代
   （`5789c2c0…` / `41c04d57…` / `9114bdff…`），`sr-apply` 是 11-05 早上那份（`d17f40be…`）。
   不铺是决定不是遗漏 —— 今晚四个修复不需要它们，而换 `sr-route`/`sr-fib` 要重启这两个进程，
   一重启就抢 RR 会话，就是 §9.15 缺陷⑧ 那个机制。
3. 盒上的回滚件（`.pre-tel` / `.pre-fix4`）逐台列了哈希；CPE4 缺缺陷④之前的那份底，
   且"缺陷④那晚实际铺上去的 agent"（`29ce6114…`）在 Mac 侧没留底，要复原得从 `de156c0` 重编。

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

### 9.14 owner 点的五个问题（2026-11-05 晚）：逐条结论、动了哪些代码、量到的数字

他给的原话是五条，我按原话逐条对，不改写成"我认为他真正想问的是"。这一节的口径：
每条都写「什么场景 → 代码原来一步步走到哪 → 当初为什么这么写 → 症状 → 动了什么 → 改前/改后两组数字」，
凡是只由我推断、没有第二组数字支撑的地方，明说是推断。

#### 问题 1：overlay 地址要从 `.0` 开始分（`.0/.1`、`.2/.3` 依次类推）

**场景。** 他在控制台看隧道列表，第一对隧道拿到的不是 `10.250.0.0 / 10.250.0.1`，
而是 `10.250.0.1 / 10.250.0.2`，后面整体错位一格。他的规则是"本端 `.0`、对端 `.1`，再往后 `.2/.3`"。

**代码原来怎么走。** 隧道创建时向分配器要两个 /32：`State.allocOverlay`
（[controller/internal/underlay/state.go:419](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/underlay/state.go#L419)）
按 `OverlayNext` 游标去取 `Settings.OverlayHosts()` 这张清单，清单本身是升序的 /32 数组。
问题在清单的第一格：旧版 `OverlayHosts` 把地址池当成"要住主机的网段"，只要前缀长度 ≤ /30 就
把偏移量下界设成 1、上界砍掉最后一格（跳过网络地址与广播地址），
于是 /16 池的第一格是 `10.250.0.1`，`OverlaySize` 也跟着报 `65534`。

**当初为什么这么写。** 这是把"子网可用主机数"的课本习惯直接搬进来了。但这个池子里每一条 /32
都不是拿来住主机的：它是被当作端点逐条塞进 VPP overlay 表（表 250）和下一跳位置的清单，
池子里不存在广播域，也就没有"网络地址/广播地址要留出来"这回事。`10.250.0.0` 和 `10.250.0.255`
在这里和任何其他一格完全等价。

**动了什么。** [controller/internal/underlay/settings.go:212](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/underlay/settings.go#L212)
的 `OverlayHosts` 现在从偏移 0 开始、发满 `1 << (32-bits)` 格；
`OverlaySize`（同文件 199 行）不再 `-2`；
`overlayPrefix` 的报错文案从"65534 个 /32"改成"65536 个 /32"；
[docs/underlay-design.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/underlay-design.md)
§6 的分配规则同步改成"整段都发，第一对是 `.0/.1`"。
`state_test.go` / `settings_test.go` / `plan_test.go` 里断言旧清单的三处一起改掉，
没有留兼容分支。

**改前/改后的数字（VM 台现场）。** 改前控制器返回的第一格是 `.1`，实测清单为
`10.250.0.1 … .10`（`/tmp/renum.log` 19:19:24 那一行，`overlay_used=10`）。
重排以后（19:31 复取 `/api/underlay`）：

| 隧道 | a 端 | b 端 | 颜色 | MTU |
|---|---|---|---|---|
| t9  | node-1 `10.250.0.0` | node-3 `10.250.0.1` | 0 | 1368 |
| t10 | node-1 `10.250.0.2` | node-4 `10.250.0.3` | 0 | 1368 |
| t11 | node-3 `10.250.0.4` | node-4 `10.250.0.5` | 0 | 1368 |
| t13 | node-2 `10.250.0.6` | node-3 `10.250.0.7` | 0 | 1368 |
| t14 | node-2 `10.250.0.8` | node-4 `10.250.0.9` | 0 | 1368 |

usage 报 `overlay_used 10 / overlay_total 65536`，四条 §9 口径打流的目的地全部可达。

**中断点名（规则 1.4，这一条必须写）。** 重排只能"删隧道 → 释放地址 → 重建"，产品没有别的入口
（游标复位走 `SetSettings`：把释放队列里落在新池外的地址丢掉、并对新池取模，
[state.go:567-579](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/underlay/state.go#L567)；
而池子还在被活隧道用的时候改小会被硬拒，报"先删隧道再改池"）。
删隧道的瞬间本端 overlay /32 撤销、依赖它的下一跳全消失，**这一段客户流量必断**。
实测断口：流 a `172.16.12.1→172.16.3.1` **335.0 秒**、流 b `→172.16.4.1` **343.9 秒**
（同一条流、同一台 cust 自己的时钟，比较第一轮最后一个回包与第二轮第一个回包；
第一轮用"相邻回包间隔"量出来只有 2.0s/1.5s，那是错的口径——整段黑洞里没有相邻回包可言）。

这 335 秒里绝大部分不是这套动作本身的窗口，是我漏了一步在诊断：隧道重建完（19:19:56 四台 agent
回执 `version 22 / ok=true`，wg 口 `wg1009…wg1014`，探针回声正常）之后，我**没有逐台发布**。
隧道是活对象、走 `/v1/watch` 立刻生效，但 sr-route 的隧道表来自**冻结的已发布包**
（bundle 第 4 段由 `overlayRouteArtifactLocked` 渲染，只有"发布这一台"时才写进
`dc.Published`：[config_push.go:46](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-ctrl/config_push.go#L46)）。
所以四台盒子上 sr-route 的下一跳还停在旧地址（CPE1 实测 `peer_device_id 3 → next_hop 10.250.0.2`，
而新表里 CPE3 那端是 `10.250.0.0`），RIB 里全部 `no_path`，`vm-fib-sync` 逐条报
"下一跳 … 不在隧道表里，该路径跳过"，主表 via-10.250 条数从 2/2/3/3 掉到 0/0/1/0。
19:25:05 补 `vm-console.py publish`（node1 v8、node2/3/4 v4）后约 35 秒路由装回。

按现有时间戳拆开看：删除+重建+四台装口 ≈ 18 秒（19:19:38 → 19:19:56），
发布 → 装表收敛 ≈ 35 秒，动作自带的窗口是这两段加起来的量级；
中间 19:20:06 → 19:25:05 那约 5 分钟是我的漏步骤与诊断，不该记到这套动作头上。
正确的次序写死在下面这七步（我也有一份可跑脚本 `/tmp/renum2.sh`，只做了 `bash -n` 语法检查、
**没有跑过第二遍**，因为它依赖 `/tmp/tel-sample.sh` 与 `/tmp/fix4-gap.py` 两份未入库的采样助手；
次序以这里为准）：

1. 装前采样四台（agent pid / exe 哈希 / 插件哈希 / wg 口数与名字 / BFD 会话数与 Up 数 /
   主表 `via 10.250.` 条数 / 探针行 / `/status` 的 tunnel 块），把每台的主表条数存下来当"回到装前"的判据；
2. 起四条 §9 口径打流（1Hz，300 包，源一律是站点 loopback）；
3. `DELETE /api/underlay/tunnels?id=<每一条>` 逐条删，读 `usage.overlay_used` 确认归零；
4. `PUT /api/underlay/settings`（把 `overlay_cidr` 暂时 clamp 成 `10.250.0.0/32`）
   + `POST /api/underlay/settings/publish`（必须带 `Content-Type: application/json` 和 `-d '{}'`），
   再 `PUT` 回 `10.250.0.0/16` + 再 publish —— 这一步是产品自己的复位入口：`SetSettings` 丢掉释放队列里
   落在新池之外的地址、并把游标对新池取模（state.go:567-579）；顺序不能反，池子里还有活隧道时改池会被硬拒；
5. `vm-console.py tunnels`（批量重建，走 `/api/underlay/auto/plan|apply`）→ **`vm-console.py publish`（逐台发布）**
   → 等四台 agent 回执 `"ok":true` 且 wg 口数回到 2/2/3/3；
6. `vm-fib-sync.sh 1..4` 推 sr-fib 的隧道表（缺口 G-2，产品里没有维护者；次序不能反，
   它认的是 wg 口上绑的那条 /32，而 agent 是"建口→装 peer→最后绑地址"）；
7. 等四台主表 via-overlay 条数回到第 1 步存的数字，再采一次现场、收打流。

**他没有让我再测，我没有为了拿一个更干净的窗口再切一次；**
他要的话按上面七步跑一遍，就能拿到这套受支持动作的端到端窗口。

新发现（只登记，不自己改）：控制台的 `overlay_pending` 信号确实会亮
（[config_api.go:81](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-ctrl/config_api.go#L81)
判的是"这一版第 4 段和已发布的那版不一样"），但页面文案只写"导通组改过"，
没说隧道改动同样需要逐台发布 —— 这次就是被这个盲区坑的。登记为文案待改，属功能之外的诚实信号缺口。

#### 问题 2：丢包显示 100%，还会 97%/103% 抖 —— 是两个独立缺陷叠在一起

他的原话："监控上显示丢包 100%，这个语义不对，丢包一般说的都是丢包率，没丢应该是 0%，
而且有时候会出现 103% 和 97% 抖动，应该是采样算法有问题。"

**缺陷 A：口径本身没有上界（就是他说的采样算法）。** agent 原先在两次采样之间做差再算比例，
[controller/internal/agent/telemetry.go](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/agent/telemetry.go)
旧代码是 `LossPermille = ProbeLost*1000/ProbeTx`（窗口差值）。这个比值天然能超过 1000‰：
一条探针在第 k 个窗口发出、按超时（3×间隔）要到第 k+1 个窗口才被判丢，它就只进 k+1 的**分子**、
不进 k+1 的分母；迟到回包又只加 `late` 不加 `rx`。于是同一个会话能报出 97% 与 103% 来回抖。
现在这段差值计算整个删掉，丢包率只有一个来源——插件自己的滑动窗口：
[vpp-plugin/sr_wg_probe.c:264-281](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/vpp-plugin/sr_wg_probe.c#L264)
在最近 `SR_PROBE_WINDOW`（128）个槽里数 `replied` 与 `lost`，返回 `lost*1000/(replied+lost)`，
两者皆 0 时返回 0。分子分母同窗口 ⇒ 值域钉在 0-100%，"没丢"就是 `0.0%`。
agent 侧改成直接取 `p.LossPermille`（telemetry.go:332），
`wire.TunnelMetrics.LossPermille` 的字段注释重写成"不是本窗口 lost/tx"并解释为什么（wire.go）。

**缺陷 B：那个 100% 是真的断了，断在探针自己的发送路径。** 插件发探针时把
`vnet_buffer(b)->sw_if_index[VLIB_TX]` 填成 `0`。26.06 的约定是这一格存 **fib_index**
（0、1、…），既不是 sw_if_index 也不是 VRF id，所以 `0` 等于点名查 main table；
而 overlay /32 在 main table 里也可能有条目（指向 WAN 网关）。结果探针从表 0 走出去、**根本没下隧道**，
对端 `echoed` 恒 0、`tx=216257 rx=0`、丢包钉死 100%。取证：CPE1 上表 0 那条 `10.250.0.2/32`
的计数器按探针速率增长（11 秒里 37 包 / 3344 字节，对得上 `probe_tx +43 × 76B`），
也就是**探针明文漏在 LAN 上**。改为 `~0`，让 ip4-lookup 去查 RX 那张接口所在的表
（隧道口由 agent 显式 `set interface ip table <if> 250`，于是探针与业务走同一条封装路径），
见 [sr_wg_probe.c:376](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/vpp-plugin/sr_wg_probe.c#L376)。
RX 侧仍必须填真实接口，不能也写 `~0`：隧道 admin down 时这个包会进 interface-drop，
而 `interface_drop_punt` 拿 RX 当数组下标加每接口计数器，2026-10-24 实测过一次直接 SIGSEGV。

**铺开方式。** 插件替换在四台都走 `cp … .new` + `mv -f`（VPP 持有旧 inode，ETXTBSY 时不能原地写），
重载插件需要重启 VPP —— 这一台的重启会撞上缺陷⑦（#268）的形状，所以是单独一件事，
不在"修丢包"里顺手做。

**改后数字（19:31 那一帧 `/api/metrics`，四台 10 行）。** 全部 `up=true`、`bfd_state=up`、
`loss_permille=0`（页面显示"丢包 0.0%"）、`probe_tx` 与 `probe_rx` 相等或差 1、`lost=0`、`late=0`。
真实浏览器监控页逐行复核过，不是只看接口。`console_metrics_test.go` 覆盖口径。

#### 问题 3：「BFD 的时延和抖动数据监控上都没有」

**先把口径纠正到位，再解释为什么他看不到。** 时延/抖动/丢包这三项**不可能来自 BFD**：
VPP 的 BFD 会话只回答可达性，`bfd_udp_session_details` 与会话结构体里没有 rtt、没有抖动、没有丢包
（2026-10-24 实测）。三项质量数字全部来自隧道内探针 `sr_wg_probe`。所以正确的显示门禁是
"这条隧道上有没有探针会话"，不是"这一窗口发过几个包"。

**页面原来怎么走。** 旧渲染用 `rx > 0` 当门禁：探针装了但一次回包都没有（正是缺陷 B 的形状），
时延/抖动/丢包三列全显示"—"，看上去就像"监控没这两项"。升级前的 `rtt=0.000ms` 也是同一件事——
收不到回包，不是页面无数据。

**动了什么。** `wire.TunnelMetrics` 新增 `HasProbe`
（[wire.go](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/wire/wire.go)
里字段注释写明"只有 BFD、没有探针的行必须显示未上报，0 是一个测量结果，缺席不是"），
agent 在真正取到探针行时置位（telemetry.go:319）；
控制台质量三列改按 `has_probe` 门禁：没有会话 ⇒ **未上报**；有会话但本窗口无回包 ⇒ 明写"无回包"；
第一个采样窗口（`WindowSec==0`，没有前一帧可减，报的是会话建立以来的累计值）在采样列标"累计"，
这条口径同时写进 `wire` 的注释，避免下游把无窗口差值当差值用。
另外在遥测面板加了一段口径说明，判 down 的窗口和探针节奏都取自控制器**下发**的那份策略
（[console.html:1235](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-ctrl/console.html#L1235)
`telemetryRuleHint`），页面不再写死 150 ms；从没发布过 underlay 配置时它说"定时器未知"，
不猜内置默认值。

**数字（同一帧）。** 10 行全部有值：`rtt` 当前 1.574–21.6 ms，`avg` 8.6–13.0 ms，
`max` 51.4–166.2 ms，`jitter` 2.7–8.2 ms；盒上 `show sr-wg probe` 与接口同口径一致
（CPE1 一条会话 avg=8.802 / max=116.399 / jitter=6.056）。抖动是插件里的 RFC3550 EWMA
（`jitter += (d - jitter)/16`），min/avg/max 是会话生命周期口径、不是窗口聚合——
这一点在 wire 字段注释里写明，页面上不再混称。

#### 问题 4：「BFD 是 300ms 一次，应该连丢 3 个就判 down，我不知道你是不是这么做的」

拆成三件事回答，第一件是确认，第二、三件才是改动与新缺陷。

**① 判 down 的规则：是"连丢 detect_mult 个"，但用的不是 300 ms。** 盒上 BFD 是 UDP **多跳**会话，
定时器 50 ms / 50 ms / detect_mult 3，判 down 的规则就是"detect_mult × 协商后的最小接收间隔"
这么多毫秒内没收到包，即 **3 × 50 = 150 ms 静默**。四台 `show bfd sessions` 实测
`Detection Time (usec) 150000`。他说的 300 ms 是隧道内**探针**的间隔——两个数字混在同一句话里：
可达性用 BFD（50 ms 一档），质量用探针（300 ms 一档），探针不参与判 down。

**② 墙钟比规则粗，这一条不是缺陷但是要说清。** 会话还在 Init/Down 阶段时 VPP 大约按 1 秒发一次
（CPE1 `show bfd sessions` 实测打印 `Transmit 1000000`），所以"接口真的死了"到被报出来实测约 1.5 秒
（2026-10-24）。规则值与墙钟值都写回 [wire.go](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/wire/wire.go)
遥测分段的注释里，不留成"未知"。

**③ 他的三个租户旋钮此前根本没到设备（这是真缺陷，已修）。** "全局配置"页能改
`probe_interval_ms / bfd_min_tx_ms / bfd_min_rx_ms / bfd_detect_mult`，但 `UnderlayBundle` 里没有
遥测这一段，agent 只会落回内置默认（300/50/50/3）——页面说 500 ms，盒上还是 300 ms，
属于"控制台不造数据"里最糟的一种：改了但不生效，还不告诉人。
现在新增 `wire.TelemetryPolicy`（ms 用于探针、us 用于 BFD，`omitempty` 让旧控制器的空策略仍落回默认）、
控制器在渲染 bundle 时填入（[underlay_delivery.go:71](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-ctrl/underlay_delivery.go#L71)）、
agent 每条隧道装 BFD/探针时使用（[tunnel.go:650](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/agent/tunnel.go#L650)），
ms→us 只在 [settings.go:230](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/underlay/settings.go#L230)
`TelemetryPolicy()` 一处做；`/api/metrics` 顶层回显这份**已下发**的策略（指针，未发布 ⇒ null ⇒ 页面说未知），
这样"页面上的数字"和"数据面里的数字"是同一份。

**实测（旋钮实时生效）。** `PUT /api/underlay/settings` + `POST /api/underlay/settings/publish` 把
探针间隔改成 500 ms 后 25 秒内，盒上 `show sr-wg probe` 从 `interval=300ms timeout=900ms`
变成 `500ms/1500ms`，`/api/metrics` 的 `interval_ms` 同步变 500；随后改回默认 300/50/50/3，
四台收敛（隧道版本 v10 ok）。

**新缺陷（登记，没动）：BFD 三个定时旋钮改不动已存在的会话。** 改成 100/100/5 之后四台 agent 全部进
退避循环，回执文本：`隧道配置 v9 只装上一部分（连续 1..4 次，5s→40s 后重试）: 隧道 t2: 隧道内 BFD/探针:
agent: 建 BFD 会话 sw_if=5: vppapi: bfd_udp_add 失败，VPP retval=-101`，盒上 BFD 仍是 50000/50000/3。
根因在收敛路径：agent 只用 `hasBFDSession` 比对定时器，不相等就 `AddBFDSession`，
而 `AddBFDSession` 只发 `bfd_udp_add`——既没有 `bfd_udp_mod` 也没有先删后加，VPP 对已存在的
(local, peer) 会话直接回 -101。改回 300/50/50/3 之所以能收敛，是因为新值**恰好等于**现存会话的值，
`hasBFDSession` 命中、压根没再调 add，不是"改成功了"。要不要补 `bfd_udp_mod`/先删后加，以及
"BFD 会话改动是否算必然中断"（删会话会让该隧道判 down 一次），等他拍。

#### 问题 5：隧道时延高是不是因为没绑核 —— 已收口，结论与八块数字在 §9.16

当时（2026-11-05 晚交把手前）能给的只有单点数字（19:31 帧：avg 8.6–13.0 ms、max 最高 166.2 ms、
jitter 2.7–8.2 ms），"高"要相对谁、绑核能不能压下来都给不出结论，所以那一版故意不写结论。
相关的既有测量只有一条：task #67 曾量过把 VPP 主线程钉在 CPU 0 会造成 ~10 ms 收包延迟，
说明钉法在这套形状上确实影响时延。
他给的次序是"先把五个问题修完交把手，然后继续做第 5 项对照"，
#267 因此留到交把手之后做，不在这一节里冒充已完成——**现在做完了，写在 §9.16：
三种绑法之间没有稳定方向、时延大头在隧道外面（虚拟 WAN 底价就 7–10ms），
绑核买到的是尾部可复现、不是平均值；要真动只有"给 VPP 写死 main-core"一条，而那必须重启 VPP，已登记待拍。**
（顺手更正本节当时的一句口误：宿主不是 4 核，是 16 逻辑核、常驻 load ~14；四台盒子每台 2 vCPU。）

#### 这一节的验证口径与遗留

- 单测（Linux 容器 `fix4-test`，带 VPP 头文件）：`go test -count=1`
  underlay 0.063s / agent 1.906s / wire 0.008s / cmd/sr-ctrl 3.369s，全绿。
- 控制台：真实浏览器监控页逐行复核（租户 `vm-tenant`），口径文字与每行数字一起看；
  不是只调 API 就算验证。
- 四台产物（从 `/proc/<pid>/exe` 读，不信符号链接）：agent exe `d2ad8519648c031f…`、
  插件 `9267d28d8b76fb54…`；wg 口 2/2/3/3，BFD 全 Up，agent 回执 `"tunnel":{"version":22,"ok":true}`。
- 规则 1 复核：问题 1/2A/3/4③ 四项修复都不在配置生效路径里重启任何 daemon。
  唯一需要重启 VPP 的是**换插件**这个部署动作本身（缺陷 B），它撞的是缺陷⑦（#268）的形状，
  已单独记录，没被塞进"修丢包"里顺手做。**补一条：这次 VPP 重启还在 CPE2 上引出了缺陷⑧（§9.15），
  那一处中断当时没写出来，现在写在这里。**
- 代码落点（分支 `feat/vbond-control-plane`，已推 gitee）：`a5ee41a` = 问题 1，
  `d949434` = 问题 2/3/4，`1eaf93c` = 本节的文档。产物对应关系与"编了没铺"的那五件写在
  `~/srvmlab/MANIFEST.md`（见 §9.11 的"现役产物是哪一份"）。
- 新增待拍（不自己决定）：BFD 定时旋钮改不动活会话（本节③末）、
  `overlay_pending` 文案未提隧道改动（问题 1 末）、缺陷⑥ #262、缺陷⑦ #268、缺陷⑧（§9.15）、
  以及 §9.16 里那条"要不要给 VPP 写死 `cpu { main-core }`"（改它必须重启 VPP）。
  #267（绑核对照）已做完并收口在 §9.16。

### 9.15 缺陷⑧（新登记，未修）：开机清单与 watch 在同一秒并发，把 sr-route / sr-fib 各起了两个

这一节是当晚 `vm-verify.sh` 那 14 条 FAIL 的根因。先说清楚它的定位：**不是我改的代码引入的**，
是换插件那一次 VPP 重启把一个一直存在的并发窗口暴露出来了；今晚做的是止损，不是修。

#### 场景：10:42 UTC 我在 CPE2 换 sr_wg 插件

盒子时钟就是 UTC（CPE2 `date` 与 `date -u` 相同），下面统一用它；
Mac 侧的 RR 日志打在 Mac 本地（CST，UTC+8），引用时会换算。

| 时刻（UTC） | 动作 | 证据 |
|---|---|---|
| 10:42:12 | `sr_wg_plugin.so` 落盘（缺陷 B 那份 `~0` 发送口修复） | `ls --time-style=full-iso` = `10:42:12.477`，旧文件留在 `.pre-tel`（11-04 14:45） |
| 10:42:29 | `systemctl restart vpp`（换插件必须重载） | journal：`vpp[38300]: received SIGTERM from PID 1 UID 0, exiting...` → `Starting vpp.service` |
| 10:42:29 | `sr-box.service` 跟着 vpp 停 → `start.sh stop` | journal：`start.sh[122341]: stop SIGTERM bridge (pid 27451) / fib (pid 42475) / route (pid 27428) / agent (pid 109920)` |
| 10:42:29 | vpp 起来后 `sr-box.service` 重新 `Starting` → **A 轮** `start.sh[122377]` | journal |
| 10:42:31 | **watch timer 那一轮也 `Starting`** → **B 轮** `start.sh[122559]` | journal：`Starting sr-box-watch.service`（同一秒两轮的步骤行交错打印） |

`sr-box.service` 挂 `PartOf=vpp.service`（[sr-box.service:43](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/sr-box.service#L43)），
所以重启 VPP 一定会把这份清单重跑一遍——这是设计里写明的（同文件 :16-17 那段注释）。
问题在于 `sr-box-watch.service` 是另一条独立调用点（[sr-box-watch.service:17](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/sr-box-watch.service#L17)
指向同一个 `start.sh up`），它不带 `PartOf`、也不与 `sr-box.service` 互斥，timer 又在 :31 那一秒正好到点。
**两条调用点可以在同一秒各跑一轮**，这件事此前只在注释里被默认"清单幂等所以安全"，没有谁保证过一次只跑一轮。

#### 代码逐步走到哪：清单的幂等是 check-then-act，中间没有锁

每一步的形状都在 [start.sh:327-343](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/start.sh#L327)
（`do_route`，`do_fib` 在 :346-358 同构）：

1. `gate_*`：配置文件在不在；
2. `alive route`（[:99-114](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/start.sh#L99)）：
   先看 `$RUN/route.pid`，用 `/proc/<pid>/comm` 核对它确实是这个二进制；pidfile 不可信时退到 `pgrep -x sr-route`；
3. 都不在 ⇒ `launch`（[:140-157](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/start.sh#L140)）：
   `nohup … &` → `echo $! > $RUN/route.pid` → `sleep 1` → `kill -0` 确认还活着。

第 2 步和第 3 步之间没有任何互斥。A 轮 `nohup` 已经 fork、但子进程还没 exec、pidfile 也还没写的那几毫秒里，
B 轮来查：pidfile 是空的（`stop` 刚把旧号 `rm -f` 掉，见 :460-476 那段），`pgrep -x sr-route` 也搜不到还没 exec 的孩子，
于是 B 轮也 launch。当晚实测就是这个形状：

| 步骤 | A 轮（sr-box.service，start.sh[122377]） | B 轮（sr-box-watch.service，start.sh[122559]） | pidfile 里留下的 |
|---|---|---|---|
| agent | 起了 `122480`（:30，journal 里 A 打的是"在跑，…已回话"= launch 之后那句） | `已在跑（pid 122480）` 跳过 | `agent.pid=122480`（mtime :30.706）✅ 与实际一致 |
| route | 起了 `122631`（:31，抢到 19001） | **也 launch**：起了 `122635` | `route.pid=122635`（mtime :31.794）❌ 端口不在它手上 |
| fib | 起了 `122663`（:32，抢到 19011） | **也 launch**：起了 `122667` | `fib.pid=122667`（mtime :32.953）❌ 同上 |
| sr-frr | 起了 `122695`（:33） | `已在跑` 跳过 | `bridge.pid=122695`（mtime :34.081）✅ |

这张表里"谁起了哪一个"是这样定的，不是猜的：journal 在 10:42:32 和 :34 各把 `route 状态口…已回话`、
`fib 在跑…` 这两句**打印了两遍**（两遍分别是 122377 与 122559 两个 start.sh 打的，journal 带 pid），
而这两句在脚本里都位于 `launch` 之后 —— 所以 route 与 fib 两步确实各起了两个进程。
归属到轮次靠 pidfile 的写入时刻（`launch` 是"fork 完立刻写"）：先写的那次落在低 pid 上（122631 / 122663），
后写的把账本覆盖成 122635 / 122667，mtime :31.794 与 :32.953 就是后写那次的时刻；
agent 与 sr-frr 各只有一行"launch 后"的消息，且 B 轮明说"已在跑"，所以没被起两次。

`launch` 后写 pidfile 是"后者覆盖"，所以清单的账本记住了 B 轮那两个；
先起的 A 轮那两个反倒成了**不在账上的孤儿**，但它们手里攥着 19001 / 19011。
`curl 127.0.0.1:19001/status`（`vm-verify.sh`、`vm-fib-sync.sh`、sr-frr 的 `poll` 全都打这个口）
看到的永远是那个"清单不认识的"实例。systemd 从 10:43:33 起每一轮都在点名这件事，只是那句话一直被当成噪声：
`sr-box-watch.service: Found left-over process 122631 (sr-route) in control group while starting unit. Ignoring.`
和 `Unit process 122667 (sr-fib) remains running after unit stopped.`（谁在哪个 cgroup 我不从这里推断，
这两句能确定的只是：账本和真实进程已经不一致了。同一族的还有
[sr-box-watch.service:14-30](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/sr-box-watch.service#L14)
那段 `KillMode`，即缺陷① #257。）

#### 为什么两个 sr-route 会把控制面点着：RR 一个 node 只留一条会话

这里要说的是 **RR 侧每个 node 只保留一条会话**。
两个实例都以 `srou://tenant-vm/node/2`（同一份 `/etc/sdwan/route.json`、同一台 192.168.1.102）去拨 rr1 和 rr2，
新会话一进来就把旧的顶掉，被顶掉的那个按退避重拨 ⇒ 谁都不稳定。测量（RR 日志打在 Mac 本地 CST，
换算 UTC 减 8 小时）：

- 起风时刻：18:45 CST（10:45 UTC）那一分钟 94 条，随后稳态 **227-228 条/分钟**（rr1+rr2 合计 ≈3.8 次/秒），
  一直到 20:05 CST（12:05 UTC）我收尾；两份日志里 `node/2` 的"会话结束"记录合计 **18196 条**。
  风暴期每条会话只活了 **377-560ms**（原文 `session ended after 557ms … use of closed network connection`），
  这个存活时长就是互顶的节奏。
- 11-04 22:59-23:xx UTC（日志 11-05 06:59-07:xx CST）那 92 条是**另一件事**：速率 ~1.5 条/分钟、
  会话存活 2.15-2.22s，等于"每分钟一轮清单把同一个实例顶一次"的形状，不是这次的风暴。
  我在写 §9.14 时曾拿 06:59 那条说过"抖动早就存在、不是今晚改号引起的"，**归因是错的，这里更正**：
  双实例生于 10:42:31 UTC，风暴起于 10:45 UTC，而隧道改号那一批动作在 **19:19:38-39 CST = 11:19:38-39 UTC**
  （`sr-ctrl.log`：`deleted tunnel t2/t3/t4/t6/t7` → `published global config` → `applied auto batch b8 mode=full-mesh (3 added)`
  + `b12 mode=hub-spoke (2 added)`），比风暴晚 34 分钟 —— 改号不是触发者，**换插件那次 VPP 重启才是**。

#### 放大链：会话一断就摘路，摘路把反环判据翻过来，客户侧真前缀被自己人顶掉

这一段是把 §9.14 里"防环那条是时间相关的"落到具体报文上，每一环都有采样：

1. 会话每次被顶掉，O7-R3 就按来源摘掉该 origin 的路径 ⇒ sr-route 的 Loc-RIB 闪断（不是 BFD，四台
   `show bfd sessions` 当时全 Up、`Detection Time 150000`）。
2. RIB 一 Empty，`sr-frr` 那轮的 `exp.reconcile()` 就把自己注入内核的 `dev lo` K 路由删掉
   （[cmd/sr-frr/main.go:712-760](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-frr/main.go#L712)
   一条 poll 里"先导出收敛、后算导入"，所以导出集合的抖动直接进导入的判据）。
3. K 路由一掉，CPE2 客户域里那条回灌副本（OSPF `via 172.31.12.1`）就不再落进本 VPN 的 `exportedSet`
   （[internal/frr/import.go:299-358](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/frr/import.go#L299)
   的反环判据是"当前导出集合"，可逆、不记录历史）⇒ 过滤器放行 ⇒
   **CPE2 把 `172.16.4.1/32`、`172.16.3.1/32` 当成自己的本地前缀重新宣告**（30 次采样里抓到 8 次）。
4. CPE4 收到一份 `device_id=2` 的 `172.16.4.1/32`，在它自己看来是"别人的前缀"，于是装上
   `172.16.4.1 dev lo`；这条一装，反环过滤器立刻把 CPE4 真正的 BGP 导入判成环：
   `/run/sr-frr/control.sock` 的 `/status` 采样 18 次里有 6 次 `rep=0 loop=1`，
   `PUT/GET /local-prefixes` 同期 6 次为空。
5. RR 库因此丢掉真正的主人 ⇒ cust12 / cust3 少了 `172.16.4.1/32`，两条流分别 **15% / 85%** 丢包。
   `vm-verify.sh` 表现为 14 条 FAIL，集中在 §6（隧道/MPLS）、§7（内核 `dev lo` + zebra `Known via "kernel"`）、
   §8（每台自己的客户 loopback 必须是唯一本地前缀）三族。

（这里没有引入任何新机制：第 3-4 环就是我们早就登记过的"防环 A+B 过滤规则"那条待拍项，
本次只是给它补了一个能稳定复现的触发条件。）

#### 止损做了什么（不是修）

只动那两个重复实例，其余进程一个都没重启，lab 配置一行都没手工补：

1. `kill -TERM 122635 122667` —— 杀的是 pidfile 认得、但**手里没有端口**的那两个，
   保留端口持有者 122631 / 122663。选这一对而不是反过来，是因为 survivor 不需要重新 bind、
   也不需要任何人重启：动"没抢到口的那个"才是最小中断。
2. `bash test/lab/vmlab/vm-fib-sync.sh 2` —— sr-fib 的隧道表只在内存（缺口 G-2 #268），
   幸存者 122663 是 10:42 新起的，那份表是空的，必须补：
   `node 2: 隧道 2 条（10.250.0.7→wg1013 10.250.0.9→wg1014）vrfs=[0] 逃生=False → converged=True`。

前后对照（同口径，规则 2 要的两组数字）：

| 指标 | 止损前 | 止损后 |
|---|---|---|
| RR 侧 `node/2` 会话结束 | 227-228 条/分钟，持续 1 小时 20 分 | sr-route 重连计数 15 秒内 `4549 → 4549`（不再涨） |
| RR 库（`GET /routes`） | 主人被顶掉，缺 `172.16.4.1/32` | 恰好 4 条：`1→172.16.12.1、2→172.16.12.1、3→172.16.3.1、4→172.16.4.1` |
| 四台 `/local-prefixes` | CPE2 多宣告、CPE4 为空 | 四台各自正确 |
| `vm-verify.sh` | PASS=374 **FAIL=14** WARN=0 | **PASS=388 FAIL=0 WARN=0**（`/tmp/vm-verify-run6.log`） |
| 每台实例数 | CPE2 有 sr-route×2、sr-fib×2 | 12:11 UTC 四台 `ps -eo comm`：sr-agent/sr-route/sr-fib/sr-frr **各 1** |

一条遗留状态，如实写：CPE2 的 `/run/sr-box/route.pid`、`fib.pid` 里现在是被我杀掉的那两个号
（`/proc/<pid>/comm` = GONE），真正的端口持有者 122631 / 122663 **不在清单账本上**。
之所以之后每一轮清单（10:43:34 起，每 60 秒一次，一直到 12:1x）都只打印"已在跑"而没再补一个新实例，
靠的是 `alive()` 里 `pgrep -x` 那条兜底（[:113](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/deploy/sr-box/start.sh#L113)）——
这不算修好，pidfile 与事实不符这件事还在这台盒子上。

#### 要动什么（只登记，等他拍）

一句话：**同一份 `start.sh up` 需要一把跨调用点的互斥**，让 `sr-box.service` 与 `sr-box-watch.service`
（以及任何手工/脚本起的第三、第四轮）不能并发；现在的"幂等"只保证了"不重启活着的进程"，
没保证"不会同时起两个"。具体形态（`flock` 一把锁 / `ExecStartPre` 查重 / 让 `alive()` 与 `launch()` 之间
用 pidfile 原子占位）涉及要不要新增 lab 侧依赖，也可能把 `systemd` 那层的 `Job` 语义牵进来，
**我没有动手，也没在 lab 里试任何一条**。

同时要说清的一次中断：换插件必须重启 VPP，这是缺陷 B 部署动作自带的，
这一晚它的确切代价就是上面这一串（控制面风暴 80 分钟 + 14 条 verify FAIL + 两条流 15%/85% 丢包）。
这一晚按主任务要求"停止测量"，我没有为这次 VPP 重启再打一轮 160 包窗口，
所以**没有换插件这一动作自身的丢包率**，只有上面这些计数；
要那个数字的话，用 §9.13 那四条打流方向、同一口径跑一遍即可。

### 9.16 问题 5 收口（#267）：绑核不是这条链路时延的开关（2026-11-05 夜，八块受控对照）

这一节把 §9.14 里那条"这一条没收口，缺受控对照"的账结掉。先给结论，再给仪器、原始数字、
以及我在这次对照里犯的错（第一批六块整批作废，写法在下面第四节，数字没有进任何表）。

#### 一、原本的问题和它变成的那句可测的话

owner 的原文是"隧道时延高是不是因为没绑核"。这句话里有两个词没法直接量：
"高"没有相对谁，"没绑核"没有说清是 VPP 没绑、硬中断没绑、还是我们那四个 Go 守护进程没绑。
所以我把它拆成三个能量化的命题，每条都对应一臂：

1. 如果"没绑核"指的是 VPP 主线程落点不受控 ⇒ 那就比较"完全放开"和"钉在一核"。
2. 如果指的是 WAN 网卡硬中断和 VPP 抢同一个核 ⇒ 那就比较"中断跟 VPP 同核"和"中断挪到另一个核"。
3. 如果指的是 sr-agent / sr-route / sr-fib / sr-frr 这几个用户态进程和 VPP 抢核 ⇒ 那就再加一臂把它们也赶走。

在动手之前先量了一次现场（它决定臂怎么设：另一核本来就闲着，所以不用测"多给一个核"，
只该测"谁在和 VPP 抢这一核"）：
四台盒子各 2 vCPU，VPP 主线程实际独占其中一核（CPE1/2/4 在 cpu1，CPE3 在 cpu0），
另一核 `busy` 只有 1.7%～3.1%；CPE1/2/4 的 WAN 口（enp0s9，内核侧是 virtio1，IRQ 17）
历史上全部计在 cpu1，也就是**和 VPP 同核**；CPE3 天然就是分开的。
所以"机器忙不过来"这个说法在盒内就被另一核的空闲否掉了，值得测的是"抢核"这件事。

#### 二、四臂怎么摆的，以及为什么整轮没有重启过任何东西

仪器在仓库里：[test/lab/vmlab/exp-267/](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-267)
（`arm.sh` 摆臂 / `block.sh` 一块 / `run-all.sh` 轮换 / `cpu-samp.sh` 盒内每核采样 /
`report.py`、`report2.py` 出数；脚本都按自身目录找 `raw/` 和 `state/`，盒子走 ssh 别名，
别名要指向 CPE1～CPE4）。这一轮实际是在 `/Users/louis/srvmlab/exp-267/` 那份上跑的，
原始逐包数据留在那儿的 `raw/`（八块）与 `raw-invalid-1sttry/`（作废那六块），没有入库。

四臂是：

| 臂 | VPP 主线程 | WAN 网卡硬中断（virtio1/IRQ17） | 四个 sr-* 守护进程 | 这一臂回答什么 |
|---|---|---|---|---|
| V0 | 放开 0-1 | 放开 0-1 | 放开 0-1 | 命题 1：完全不绑 |
| V1 | 钉回它开机时那一核 | 和 VPP 同核 | 放开 0-1 | 现状形状，也是命题 2 的"撞核"侧 |
| V2 | 钉回它开机时那一核 | 挪到另一个核 | 放开 0-1 | 命题 2：中断挪开能不能降 |
| V3 | 钉回它开机时那一核 | 挪到另一个核 | 全钉另一个核 | 命题 3：连用户态一起挪 |
| Z | 钉回它开机时那一核 | 放开 0-1 | 放开 0-1 | 复原，不是测量臂 |

整轮只动运行时亲和性：`taskset -pc` 改进程掩码、`/proc/irq/17/smp_affinity` 改中断落点，
**不重启任何 daemon、不写任何配置文件、不改任何内核参数**，这是 1 号硬规则要求的形状。
凭证是 VPP 的 pid：第一次摆臂（20:45 CST）读到 CPE1=167810、CPE2=122372、CPE3=190459、CPE4=252100，
最后一轮复原（21:19 CST）读回来还是这四个号，一次没变过；四个 sr-* 同理。
每次摆臂完都把生效值读回来打印（`vpp_mask` / `irq_aff` / 每个 `sr-*` 的掩码），不靠"我写了所以生效了"。

轮换是 V0→V1→V2 为一轮、跑两轮，再单独补 V3 两块（每块 90 秒）。
这样任何一段时段漂移都会同时落在三个臂上，而不是被某个臂独占。

#### 三、两把尺子

第一把是**客户视角的时延序列**：四条路径同时打 `ping -i 0.5 -c 180`，
`ovl_1_3` / `ovl_1_4` 是 cust12 打到另外两个站点的客户 loopback（进隧道、跨两台 CPE），
`ovl_3_4` 是 cust3 打到 CPE4 那边，`undl_1_3` 是 CPE1 直接 ping CPE3 的 WAN 地址
（**完全不进隧道**，是这条虚拟 WAN 自己的底价）。每包 `time=` 都留下来，按块和按臂各算一次分布。

第二把是**插件自己的探针**：`vppctl show sr-wg probe` 每秒读一次 `rtt last=`，
它量的是 CPE↔CPE 隧道内部（300ms 定时、走 SR-over-WG 全路径、不经客户 VM、不经内核 ping）。
这把尺子的作用是把"隧道里的开销"和"客户 VM 与内核这一跳的开销"分开。
按块取窗口内的样本，不是取那个累计计数器（累计值从建隧道起算，块间不可比）。

同时每块还在宿主上采了 `vm.loadavg` 和 VBoxHeadless 的 %CPU，在盒内每 10 秒采一次
`/proc/stat` 每核分项（含 `steal`）。这三样是用来判断"臂间那点差是不是时段差"的。

#### 四、第一批六块作废：仪器自己的错，写下来

第一次 `run-all.sh` 的六块（20:45～21:00）**没有进任何表**，原始目录留在
`raw-invalid-1sttry/`。原因值得单独记，因为它是一种很典型的假绿：

`arm.sh` 里"哪个核是 VPP 的、哪个是另一个"是从**当时的掩码**反推的。V0 这一臂做的事就是把
掩码放开成 `0-1`，于是 V0 跑完之后，下一臂（V1）一进来就看到 `vmask=0,1`，命中那条
`ERR: VPP 掩码不是单核` 直接退出——**摆臂失败，但 `block.sh` 没停下来**，照样打了一百八十包、
照样算了平均。所以第二批到第六批实际上全是 V0 的形状，标签却写着 V1/V2。
如果我就按那批数字写表，结论会变成"绑与不绑差不多"，而这句话是拿五块 V0 数据冒充三臂得来的。

两处改动（都不在产品代码里）：
- 参考核改用第一次进脚本时记下的那一份（盒内 `/root/exp-267-vppcore`、Mac 侧 `state/vppcore.<盒>`），
  不再从当前掩码反推；V1/V2/V3 都显式把 VPP 钉回那个核。
- `block.sh` 在摆臂后 `grep -q '^ERR'`，有错就让本块作废并 `exit 1`；
  `run-all.sh` 收到非零就中止后续块，但**复原一定执行**（不能把盒子留在摆过的形状里）。

改完先把"V0 之后接 V1/V2 再接 Z"这个失败序列单独跑通，才重跑整轮。
第二批六块 + V3 两块，八块全部有摆臂生效的读回记录。

#### 五、数字

**按臂合并（客户 ping，单位 ms，每臂 = 两块 × 180 包）**

| 路径 | 臂 | n | avg | p50 | p90 | p99 | max | sd |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| ovl_1_3（进隧道，跨两盒） | V0 | 360 | 29.40 | 28.50 | 50.10 | 75.10 | 81.50 | 15.26 |
| | V1 | 360 | **20.26** | 18.90 | **33.20** | **45.00** | **46.60** | **8.86** |
| | V2 | 359 | 23.98 | 21.70 | 40.60 | 67.80 | 78.00 | 12.60 |
| | V3 | 359 | 22.34 | 20.40 | 34.60 | 57.70 | 70.00 | 10.96 |
| ovl_1_4（进隧道，跨两盒） | V0 | 360 | 20.08 | 16.90 | 36.10 | 58.80 | 72.70 | 11.96 |
| | V1 | 360 | 27.14 | 25.60 | 39.10 | 56.30 | 69.80 | 9.42 |
| | V2 | 360 | 27.40 | 25.60 | 43.30 | 58.50 | 67.10 | 11.09 |
| | V3 | 360 | 27.33 | 26.00 | 42.50 | 62.40 | 75.10 | 11.52 |
| ovl_3_4（进隧道，跨两盒） | V0 | 360 | 19.75 | 16.10 | 36.80 | 66.90 | 75.40 | 12.46 |
| | V1 | 360 | 20.36 | 17.80 | 32.30 | 67.60 | 74.70 | 10.81 |
| | V2 | 358 | **18.61** | **15.20** | **32.10** | 69.90 | 81.00 | 12.07 |
| | V3 | 358 | 18.65 | 15.50 | 34.20 | **62.20** | **70.40** | 11.32 |
| undl_1_3（不进隧道，虚拟 WAN 底价） | V0 | 360 | 12.93 | 10.20 | 28.40 | 42.90 | 45.90 | 9.76 |
| | V1 | 360 | **8.58** | **7.35** | **17.00** | **27.20** | **38.30** | **5.57** |
| | V2 | 360 | 10.28 | 8.06 | 19.00 | 37.80 | 99.40 | 8.61 |
| | V3 | 360 | 10.86 | 9.04 | 20.70 | 50.10 | 95.90 | 9.40 |

**按臂合并（插件探针 `rtt last=`，纯数据面 CPE↔CPE）**

| 这台盒子 | 臂 | n | avg | p50 | p90 | p99 | max | sd |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| CPE1 | V0 | 352 | 10.33 | 7.08 | 23.21 | 34.68 | 44.18 | 8.34 |
| | V1 | 352 | 10.76 | 9.80 | **19.90** | **27.65** | **30.10** | **6.21** |
| | V2 | 352 | 12.35 | 10.04 | 24.27 | 39.28 | 104.13 | 9.45 |
| | V3 | 350 | 11.97 | 9.67 | 24.02 | 36.47 | 86.80 | 9.07 |
| CPE3 | V0 | 528 | 12.16 | 9.32 | 25.43 | 39.60 | 49.69 | 9.20 |
| | V1 | 531 | **8.35** | 7.46 | **14.40** | **22.56** | **38.04** | **4.71** |
| | V2 | 528 | 8.90 | 7.33 | 16.34 | 31.55 | 39.25 | 6.12 |
| | V3 | 528 | 8.52 | **6.67** | 16.54 | 31.50 | 40.38 | 6.15 |

**同一臂两轮的漂移（avg，ms）**——这张表是全部结论里最要紧的一张：

| 路径 | V0 两轮 | Δ | V1 两轮 | Δ | V2 两轮 | Δ | V3 两轮 | Δ |
|---|---|---:|---|---:|---|---:|---|---:|
| ovl_1_3 | 24.38 / 34.41 | **10.03** | 20.77 / 19.75 | 1.02 | 24.46 / 23.50 | 0.96 | 22.40 / 22.29 | 0.11 |
| ovl_1_4 | 15.75 / 24.41 | **8.66** | 26.82 / 27.46 | 0.64 | 27.87 / 26.93 | 0.94 | 27.13 / 27.53 | 0.40 |
| ovl_3_4 | 16.20 / 23.30 | **7.10** | 20.16 / 20.57 | 0.41 | 19.69 / 17.51 | 2.18 | 19.40 / 17.91 | 1.49 |
| undl_1_3 | 7.93 / 17.94 | **10.01** | 8.70 / 8.45 | 0.25 | 10.55 / 10.01 | 0.54 | 11.00 / 10.71 | 0.29 |
| 探针 CPE1 | 6.49 / 14.16 | **7.67** | 11.12 / 10.40 | 0.72 | 12.28 / 12.42 | 0.14 | 11.96 / 11.98 | 0.02 |
| 探针 CPE3 | 8.52 / 15.81 | **7.29** | 8.25 / 8.44 | 0.19 | 8.60 / 9.21 | 0.61 | 8.66 / 8.37 | 0.29 |

#### 六、三件事能从这些数字里说，一件不能说

**能说的第一件：绑不绑不改变时延的量级，三种绑法之间也没有稳定方向。**
V1/V2/V3 三个臂在同一批路径上的 avg 互差在 0.3～3.7ms；而且方向不一致：
`ovl_1_3` 上 V1 最好、`ovl_3_4` 上 V2/V3 最好、`ovl_1_4` 上三者并列。
把 WAN 中断从 VPP 那个核挪走（V2 − V1）在 `ovl_1_3` 上**反而慢 3.7ms**、在探针上慢 1.6ms（CPE1），
在 `ovl_3_4` 上快 1.7ms、探针 CPE3 慢 0.55ms；六把尺子里只有 `ovl_3_4` 这一侧变好，
其余的差全在"挪开更慢"的方向，但量级又是 0.3～3.7ms。
连用户态四个守护进程一起钉到另一核（V3）也没有额外收益（相对 V1：+2.1 / +0.2 / −1.7 / +2.3ms）。
**所以命题 2 和命题 3 的答案都是"不成立"，这条可以关掉，不必再重测。**

**能说的第二件：时延的大头在隧道外面。**
不进隧道的 `undl_1_3`（CPE1→CPE3 走虚拟 WAN）在最好的臂上 avg 也只有 8.58ms、p50 7.35ms；
纯数据面的探针（隧道内部，不经客户 VM）avg 8.35～12.4ms、p50 6.67～10.0ms。
而客户到客户的三条 overlay 路径是 avg 18.6～29.4ms。
也就是说"隧道时延高"里，先有一份是这台 Mac 上 VM↔VM 经宿主转发本身就要 7～10ms 的底价，
隧道再加的那一部分和绑核没关系。**这才是"高"的相对谁：相对物理网络，不是相对绑核。**

**能说的第三件：绑核买到的不是平均值，是可复现性。**
上一节那张漂移表里，V0 同臂两轮差 7.1～10.0ms，而 V1/V2/V3 内部只有 0.02～2.18ms——
V0 差值的最小值（7.10）比另外三臂差值的最大值（2.18）还大一倍多。
V0 的尾部也更肥，而且是同一把尺子上比：`ovl_1_3` p99 75.10 / max 81.50（V0）对 45.00 / 46.60（V1）；
探针 CPE1 p99 34.68 / max 44.18（V0）对 27.65 / 30.10（V1）。盒内每核 `busy` 的逐格采样直接拍到了机制：
V0 第一轮 CPE3 的 VPP 前 5 格在 cpu0（100.0%），12:54:34 那一格变成 cpu0=38.2 / cpu1=38.2
（一次 10 秒窗口里两核各吃一半 = 迁移正在发生），从 12:54:44 起整块落到 cpu1（100.0%）。
同一台盒子两轮之间落点不同，是因为落点由内核在开机时给、之后再没人钉住它。
V1/V2/V3 里 VPP 每格都在同一个核上 100.0%。

**不能说的那件：V0 最差那一轮（`20261105-210450`）不能算成宿主抢占。**
那一轮四条路径 + 两台探针同时抬高（`ovl_1_3` 34.41 / `undl_1_3` 17.94 / 探针 CPE3 15.81），
看着像"这八分钟里这台 Mac 在忙别的"；但它自己采到的宿主 `load1` 均值 13.25，是八块里**最低**的一块
（八块 `load1` 均值 13.25～15.29、`load5` 14.05～14.80、`load15` 14.30～14.58，基本平坦），
而且那一轮 VPP 落点是稳定的（CPE1 cpu1=100.0、CPE3 cpu1=100.0，没有迁移）。
所以这一轮的抬升我归不掉，写在这里就是"未归因"，不当成任何一条结论的支撑。

#### 七、这次对照的中断（按 1 号硬规则第 4 条点名）

八块 × 四条路径 = 5760 个 ICMP 请求，丢 6 个包，分布在两块里：
`20261105-210905-V2` 的 `ovl_1_3` 丢 1、`ovl_3_4` 丢 2；`20261105-211457-V3` 的 `ovl_1_3` 丢 1、`ovl_3_4` 丢 2。
其余六块 40 条序列全部 0 丢包。
逐条查过序列号：这 6 个都是**每条序列的第一个或前两个**（`icmp_seq` 直接从 2、从 3 起，中间没有洞），
也就是没有一次窗口中途丢包。摆臂动作全部发生在两块之间的空档（`arm.sh` 返回后才起 ping），
所以**改亲和性对客户可见流量的实测中断是 0ms**。
首包丢失最可能的解释是块与块之间空了一两分钟、客户 VM 到网关的邻居项老化后重新解析；
**这一条我没取证**（没在 CPE 上抓 `ip neigh` 的时间线），只是最合理，不当结论用。

#### 八、要动什么（只登记，等他拍，我没动）

先纠正一句容易误解的话：**现状不是"配置里绑了核"。**
CPE 上生效的 `/root/vmlab/startup-vmlab.conf` 里根本没有 `cpu` 段，
`/etc/vpp/startup.conf` 里那个 `cpu { }` 是空的（而且它已被 systemd 覆盖，不是实际用的那份）。
也就是说 VPP 只是"单线程 poll 模式 + 开机时被内核放到某一核 + 恰好没被搬走"。
这次对照里 V1 之所以能"钉回它原来的核"，是因为我在第一次进脚本时把那个随机落点记了下来。

于是真正有证据支持的改动只有一条，而且它撞硬规则：

- **给 VPP 显式写死主线程核**（VPP 自己的 `cpu { main-core N }`），买的是"尾部可复现"，
  不是"平均降几毫秒"。证据就是第六节第三件：不钉住时，同一台盒子两轮能差 7～10ms、
  同一把尺子上 `ovl_1_3` 的 p99 从 45.00（V1）抬到 75.10（V0）。**代价是改这个必须重启 VPP**，
  属于"必然中断的操作"，
  而且 §9.13 已经量过换插件那一次 VPP 重启的连锁（控制面风暴 80 分钟 + 14 条 FAIL），
  所以这一条我**没有做，也不建议顺手做**——要做得单独立项，先给中断时长和回滚姿势。
- 反过来，"把 WAN 中断挪到另一核"这种听起来最像"绑核优化"的做法，**这次被数据否掉了**，
  可以在方案里划掉，不再占用后面的测量时间。

还有一个空白要如实留着：这台是 2 vCPU 的 VirtualBox 盒，宿主 16 逻辑核上常驻 load ~14
（其中 colima 那台跑环路台的 VM 长期占 4 核以上），guest 里的 `steal` 从开机起一直是 0
（八块每 10 秒一次、全部 0.0%，VirtualBox/macOS 不把宿主抢占透给 guest）。
⇒ 这份数只能说"在这台盒子里，核间分配不是时延开关"，**不能迁移到真机**（那台 arm64/192.168.1.100
核数、网卡队列、中断分布都不同）。真机上要不要钉核，得在那台上重做同一套四臂，
仪器可以直接复用（`arm.sh` 只要认核数）。

跑完之后四台的状态：VPP 回到各自开机时那一核（CPE1/2/4=cpu1、CPE3=cpu0，pid 未变），
IRQ17 亲和性回到 `0-1`，四个 sr-* 掩码回到 `0-1`——都等于我没碰之前的设置。
`vm-verify.sh` 复跑 **PASS=388 FAIL=0 WARN=0**（`/tmp/verify-post267.log`），和交把手那份基线逐条一致。

### 9.17 缺陷⑦（#268）收口：VPP 重启后 sr-fib 自己重连、按整份计划重装；隧道表那一半是 G-2（2026-11-05 夜，CPE4 实打）

owner 的提法原话是「overlay 装表仍堵在这条上（fib.log 每秒 1 行 broken pipe）要修。
这个是只存在于 vpp 重启后的一个 bug？还是说咱们的路由协议本身机制上有什么不支持动态更新路由表的」。
一句话答：**路由协议的机制支持动态装表，这条缺陷不是机制问题，而是两个各自独立的成因叠在一起**——
① sr-fib 与 VPP 的 API 连接断了不会重连（这次修掉了，实测 1 秒自愈）；
② sr-fib 要的那张隧道表只存在进程内存里，盒子上的磁盘上根本没有（这是缺口 G-2，§9.6，这次没修，
按你拍的 (a) 方案单独立项）。下面把两半分开给数。

#### 一、代码一步步走到哪，为什么以前必然刷 broken pipe

sr-fib 的主循环每 `poll_sec`（lab 全是 1s）跑一次 `converge()`
（`controller/cmd/sr-fib/main.go:273`），顺序是：

1. `fetchStatus()` 读盒内 sr-route 的 `/status` 拿 RIB；
2. `f.dp.BFDSessions(ctx)` 读本机 VPP 里的 BFD 会话表——隧道通不通只认这一个来源；
3. `fib.PlanFrom(...)` 按当前隧道表 + VRF 集合算出**整份**目标计划。每条路径过两道闸：
   闸①「这个 overlay 下一跳在我自己的隧道表里吗」（`controller/internal/fib/fib.go:302`，
   不中就打 `下一跳 … 不在隧道表里，该路径跳过`）；闸②「它的 BFD 是不是 Up」（`fib.go:308`）；
4. `Applier.Apply(ctx, plan)`（`fib.go:422` 起）拿 next 和 **`prev`** 做差，只写差量，成功后 `a.prev = next`（`fib.go:539`）。

改前卡在第 2 步。VPP 一重启，内核里那个 `api.sock` 上的所有连接全断，`vppapi` 客户端
不会自己活过来：读循环退出后，`sendErr()` 把写失败归一成 `ErrClosed`
（`controller/internal/vppapi/vppapi.go:65`、`:501`），而 sr-fib 只在 `main()` 起来时
`Dial` 过一次，手里那只句柄从此永远是死的。于是每一轮都走到第 2 步报错、
`note()` 记下失败、下一轮再撞同一堵墙——这就是「每秒 1 行 broken pipe」
（3.5 分钟 203 行）。它不是崩溃，是**永不自愈**。

第 4 步还埋了第二颗雷，而且比"没重连"更隐蔽：就算把 socket 重新接上，`Applier.prev` 是内存里的
"上次成功下发的那份计划"。VPP 重启把表、标签、路由全带走了，`prev` 却不知道，
差一算就是「和上次一样、什么都不用写」，FIB 永久空着。**所以重连必须连 `Applier` 一起换掉**，
光 `Dial` 一次新连接修不好这条缺陷。

#### 二、动了什么（`controller/cmd/sr-fib/main.go`，四处）

- `bfdDP` 接口加上 `Close() error`（`:219`）。放在接口里是因为收敛器要能自己扔掉手里的句柄，
  `*vppapi.Conn` 本来就有这个方法（`vppapi.go:134`），不是为测试新造的。
- `fiber` 多一个 `dial func(context.Context) (bfdDP, error)` 字段，`main()` 里建那只闭包
  （`:428`）：启动时用一次，连接死了以后每轮再用一次——和缺陷④（#260）里 sr-agent 的修法同形。
- 新增 `conn(ctx)`（`:243`）：手里没连接就现连一份，**并同时换上新的 `fib.NewApplier(ndp)`**，
  日志打「已连上 VPP api.sock，按整份计划从头收敛」。它放在 `fetchStatus` 之后——
  sr-route 没起来时不该去动数据面连接。
- 新增 `dropDead(err)`（`:262`）：只在 `errors.Is(err, vppapi.ErrClosed)` 时关连接、置 nil、
  打「VPP api 连接断了…下一次重连」。超时、RIB 读不到这类错误**不进来**：那条连接还是好的，
  扔了反而自己制造一次重连抖动。
- 轮询循环改成显式持锁跑 `converge()`（`:481`）。`dp`/`applier` 现在会在轮与轮之间被换掉，
  必须和 `PUT /config`（D3 那条热更入口，`:324` 已经持锁）共用一把锁——`Applier` 只允许单线程调用。

按硬规则核对：**这条路径里没有 restart 兜底**。热更失败就是这次 apply 失败，
保留上一份成功计划（`note()` 那句「保留上次成功的计划」没改），绝不"重启试试"。

#### 三、单测（`controller/cmd/sr-fib/main_test.go`，口径是"前后状态"不是退出码）

- `TestVPPRestartSelfHealsOnNextRound`（`:468`）：假端第一轮正常收敛 → 打成死连接
  （`vppapi.ErrClosed`）→ 第二轮必须看到「重连 + 换 Applier + 按整份计划重装」，
  断言新句柄上路由条数回到 2、`Close` 恰好调用一次。
- `TestOrdinaryFailureKeepsConnection`（`:532`）：一次普通错误（不是 `ErrClosed`）之后，
  同一只连接必须还在、`Close` 一次都不调——这条是防我把「超时」也当死连接扔。

容器里跑（lab 规矩：宿主机没有 C 工具链，一律进 `srwg-dev`）：

```
docker run --rm -v "$PWD":/src -w /src/controller -e CGO_ENABLED=0 --entrypoint go \
  srwg-dev test ./cmd/sr-fib/ ./internal/fib/ -count=1
→ ok sdwan/controller/cmd/sr-fib 0.026s   ok sdwan/controller/internal/fib 0.012s
go vet ./cmd/sr-fib/ ./internal/fib/ + go build ./... 全过
```

#### 四、盒子上实打：判据分两半，别把 G-2 记到这条缺陷头上

`test/lab/vmlab/exp-268/gate.sh` 四阶段，只动 sr-fib 和 VPP，全程不碰 agent / sr-route / FRR；
客户侧同时跑 `ping-probe.sh`（loopback 互 ping，逐条记时间戳与通/不通）。
**先点名：阶段 3 是 `systemctl restart vpp`，必然中断客户流量**，中断时长见第六节。

| 阶段 | 做了什么 | 读数（盒子时钟） |
| --- | --- | --- |
| 0 | 改前现场 | sr-fib sha=`41c04d57…`、pid=252341、`r0=2 t250=3`、broken pipe 累计 **4986** |
| 1 | 同盘临时件 + `mv` 原子换二进制，**不动进程** | 运行中进程 `exe=/usr/local/bin/sr-fib (deleted)`、ino 不变、`r0` 无变化 |
| 2 | `pkill -x sr-fib` → `/usr/local/lib/sr-box/start.sh up` 带回 | 新 pid=**300421**、`1 个 VRF、0 条隧道`、三条 `不在隧道表里，该路径跳过` |
| 3 | 只 `systemctl restart vpp`，一律不干预 | 15:46:50 断连日志 → **15:46:51 「已连上 VPP api.sock」+「收敛 labels+1/-0」**；pid 仍是 300421 |
| 4 | 结论 | broken pipe **4986 → 4988（+2 之后不再涨）**；`t250` 20s 内回到 3；`r0` 到实验结束仍是 **0** |

判据A（本次修的那一半）**通过**：重连用时 1s，`sr-fib` 进程从头到尾没被重启过，
broken pipe 一次完整 VPP 重启只多 2 行然后静止（改前是每秒 1 行、永不停）。
判据B（装表齐）**未通过**，而且原因写在日志里：阶段 2 重启 sr-fib 把内存里那张隧道表丢了
（`/etc/sdwan/fib.json` 落盘的就是 `"tunnels": []`），闸① 把所有路径全跳过，
`r0` 从此回不来。这一半不是 #268，是 §9.6 的 G-2。

顺带证实了一件事：**动态更新路由表在机制上没有缺口**。第 3 步里 VPP 的表被重启清空之后，
sr-fib 重连的那一轮是按整份计划重装（`labels+1/-0`），不是靠"进程重启时顺手装一遍"。

#### 五、table 250 与主表那两条是谁装的（免得读数骗人）

- `t250`（overlay 表 250 里的对端 /32 直连条目）在 VPP 重启后 20s 回到 3，**不是 sr-fib 装的**，
  是 agent 建隧道时顺手装的：`controller/internal/agent/dataplane.go:1053 applyPeerRoute()`
  → `vppapi.IPRouteViaInterface()`（`controller/internal/vppapi/fib.go:213`，注释就写着
  「这就是 CLI 的 `ip route add table 250 <peer>/32 via wg0`」）。
- `r0`（主表里带 MPLS 标签的客户 /32）只有 sr-fib 装。所以 `t250 恢复了而 r0 永久 0`
  恰好就是"隧道层活着、overlay 装表死了"的形状，两半分开判才不会被这个读数误导。

#### 六、必然中断的实测时长（硬规则第 4 条要求点名）

探针时间戳来自客户 VM、gate 相位来自 CPE4，两把钟不在一条轴上（实测 cust4 比 Mac 快
**28679s**、CPE4 慢 220s），所以先用 `clock-anchor.sh` 量偏移再对齐（`gap-report.py --anchor`）。

| 方向 | 黑洞起点（对齐后） | 探针窗口内 | 恢复 |
| --- | --- | --- | --- |
| cust4→cust3 | 11-05 23:50:29（`restart vpp` 在 23:50:30） | **290.1s 连续丢 286 条，到探针结束仍未自愈** | 23:58:31 由 lab 脚本推隧道表才通 |
| cust3→cust4 | 11-05 23:50:27 | **294.7s 连续丢 212 条，同样未自愈** | 同上 |

端到端 **482s（8 分 02 秒）客户完全不通**，其中前 290s 是探针实测到的下限，
后面是"产品自己不会恢复、直到我干预"。恢复后双向各 5 次 ping **0% 丢包**、rtt 6.31–22.57ms。
**如果按产品现状交付，这条中断是永久的**——这就是 G-2 必须修的证据。

#### 七、我自己这把尺子上有四处读数错，都改了

写进文档是因为它们差点把结论带偏：

1. 采样器的 `converged` 列整轮都是 0，连健康时也是。原因是我 `grep '"converged": true'`，
   而 sr-fib 的 `/status` 顶层根本没有这个键，真值在 `last.converged` 里、序列化也没有空格。
   现在 `gate.sh` 改成解析 JSON，并把 `tunnels`、`warnings` 条数、`routes_set` 三列一起进采样器。
2. 上一版 `gap-report.py` 报「没有超过 0.5s 的中断：全程连续」，而同一时刻客户前缀一条都不在表里。
   错在口径：路由被摘掉时内核立刻回 `Destination Host Unreachable`，那条 LOSS 只花几十毫秒，
   "相邻两条 OK 的时间差"永远很小。真正的中断是**连续 LOSS 的那一段**，从第一条 LOSS 到它后面第一条 OK。
   现在按这个口径切窗口，并把"到探针结束仍未恢复"单独打出来。
3. **改完 1 之后那四列还是 0，而且是另一个更低级的错**：`gate.sh` 里
   `STATUS=http://$(…status_addr…)`，`status_addr` 只是 `127.0.0.1:19011`（host:port），
   而 sr-fib 只在 `/status` 这个路径上答。少了这段路径，curl 拿到的是
   `http=404 bytes=19`（"404 page not found"），`json.load` 失败走 `print(0)` ——
   于是 `converged / 隧道 / 跳过 / routes` 四列在**两轮实验里全是指向不存在的假 0**。
   判据C 那一轮我写「内存里隧道表=0 条完好」，一句话自相矛盾，才去戳这条。
   现在 URL 补上 `/status`，且解析失败打 `-1`（`0` 是"表真的空"的有效读数，不能拿来当错误码）。
   **对结论的影响**：G-2 的证据本来就不是这四列，而是 sr-fib 自己启动那行的
   `1 个 VRF、0 条隧道` + 三条 `不在隧道表里，该路径跳过` + 磁盘 `fib.json` 里 `"tunnels": []`，
   这三条都在日志/文件里，第四轮复核过（见第十一节）。
4. `roll-268.sh` 的铺件读数把两类主表 /32 混成一列：主表的 `172.16.x.y/32` 里既有 sr-fib 装的
   overlay 客户前缀，也有 agent 经 linux-cp 装的 `lcp-rt-dynamic` 直连路由。CPE4 实测
   `r_all=3` 但 `r_sr=2`（`172.16.4.1/32` 的 path 是 `src-flags:added,… lcp-rt-dynamic`、
   下一跳 `172.31.4.2 host-enp0s8`，不是 `API refs` + `via 10.250.0.x wg`）。
   读数口径改成两列：`r_sr`（按「这一前缀有一条 wg 下一跳」去重计数）与 `r_all`，
   CPE1/2 实测 `r_sr=3 r_all=3`（那三台没有 lcp 直连的 /32 撞进来）。
   早先我在台账里写的「四台 r0=3」按这把新尺子要重读，见第十节的表。


#### 八、顺手抓到一条缺陷⑥（#262）的现行证据

`/var/log/sr-box/fib.log`：

```
2026-11-05 15:56:08 sr-fib: 警告: 172.16.12.1/32: 下一跳 10.250.0.2 的 BFD 会话不是 Up，该路径跳过; …10.250.0.8 …; 172.16.3.1/32 …10.250.0.4 …
2026-11-05 15:56:08 sr-fib: 收敛 tables+0 labels+0/-0 routes+0/-2 mpls-egress=true
2026-11-05 15:56:09 sr-fib: 收敛 tables+0 labels+0/-0 routes+2/-0 mpls-egress=true
```

一轮 BFD 读数不是 Up（三条全不 Up），`PlanFrom` 就把**全部**客户前缀从计划里去掉，
`Apply` 当场删掉那 2 条（`routes+0/-2`），下一秒又装回来（`routes+2/-0`）。
这就是 #262 说的"亚秒抖动被放大成 ≥1s 黑洞"的最小闭环，删除-重装是 sr-fib 自己做的，
不是 BFD 慢。方案仍未拍，不动代码。

#### 九、G-2 怎么收（你已拍 (a)：agent 派生并推给 sr-fib；这条只登记，下一步出方案再动代码）

- 来源：agent 手上已经有 `sw_if_index`、颜色、本端 overlay /32（接口清单上报那条路径，#105 的
  `vppapi.Inventory`），派生 `{下一跳 → 本机 wg 口名, 颜色}` 后 `PUT /config` 推给 sr-fib。
- **不落盘**：推的是派生值，磁盘上存一份快照只会再养出过期的 `wg1002/wg1003`——
  隧道口名按创建顺序生成，重启后对不上是必然。
- 触发全是动态：开机、隧道增删、检测到 VPP 重启。**不许假设谁先起来**，
  sr-fib 那侧收不到表就一直按闸① 如实告警，不猜。
- 收口判据：`vm-fib-sync.sh` 退出 lab 路径（"lab 上不该有任何非生产的东西"），
  `vm-verify.sh` 加一条「隧道表来自 agent」，`/status` 的 `tunnels` 条数 = 控制台隧道数。
- 另一条同源的缺口一起记着：`start.sh` 里写「磁盘上没有那份隧道表」，
  设备断电重启后隧道层能不能从盘上恢复，是 G-2 的另一半（§9.6 只补了运行中那半）。

#### 十、修复件铺到四台的台账（2026-11-06 00:14–00:38，`exp-268/roll-268.sh`）

| 台 | 改前 pid / sha | 改后 pid / sha | `r_sr`/`r_all` | `t250` | 隧道表 | converged | broken pipe 累计 | watch 定时器 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| CPE1 | 169112 / `41c04d57…` | **232243 / `bab523ff3809`** | 3 / 3 | 2 | 2 | True | 203 → 203 | 跑前停、跑后恢复 active |
| CPE2 | 122663 / `41c04d57…` | **175596 / `bab523ff3809`** | 3 / 3 | 2 | 2 | True | 1742 → 1742 | active |
| CPE3 | 190699 / `41c04d57…` | **248325 / `bab523ff3809`** | 2 / 3 | 3 | 3 | True | 2 → 2 | active |
| CPE4 | 252341 / `41c04d57…`（11-05 夜那一轮改的） | **300421 / `bab523ff3809`** | 2 / 3 | 3 | 3 | True | 4986 → 4990（判据C 那一轮 +2） | active |

- 铺件次序是「一次一台」，每台内部：停定时器 → gzip 传件+校哈希 → 同盘临时件 `mv` 原子换（不动进程）
  → `pkill -x sr-fib` 后交产品清单 `start.sh up` 带回 → `vm-fib-sync.sh` 把隧道表推回内存 → 恢复定时器。
  哈希逐台核对一致（`bab523ff3809f57bd1a63770f1a857eb9cdd46e07c6734a55fbeadfe7665b8eb`，8,321,490 B；
  gzip 流 4,717,002 B，Mac→盒实测 ~13KB/s，每台传 5–12 分钟）。
- **件与源的对应关系要说清**：盒上这枚 `bab523ff…` 是注释重排**之前**的那份工作树编的；
  用本次提交的源码重编得到 `e8f70520ae2e7597903f29f3633a6fbc6fc060cdf673d4f7abbcd794723f0be6`
  （8,321,466 B，差 24 字节，逐段比对后差异只在 DWARF 行号表——注释换了行）。
  判据A/B/C 全是在盒上那枚上量的；没有为了对齐哈希再铺一次——换一枚只差行号表的件
  要把四台进程各重启一遍，那是一次没有必要的客户风险。
- **这一轮没测的中断**：铺件时没在客户侧挂探针，所以「`pkill` 到 `start.sh up` 带回」那 1–7 秒
  客户流量掉不掉、掉多久，我没数字。机制上不该掉（VPP 没动、FIB 里现存的条目不会被谁摘走，
  新进程起来只是"还没装"而不是"删掉重装"），但没量的就不写成结论 —— G-2 收口时补这条对照。
- 表中 `r_sr`/`r_all` 是按第七节第 4 条改过的新口径重读的；早先我记的「四台 r0=3」
  里 CPE3/CPE4 各有一条是 agent 的 `lcp-rt-dynamic` 直连路由，不是 sr-fib 装的。

#### 十一、判据C：产品形状下只重启 VPP，装表自己回来了（2026-11-06 00:43，CPE4）

这一档是 `GATE_PHASE=vpp`：**不换件、不动 `sr-fib` 进程、不跑任何 lab 脚本**，
只做 `systemctl restart vpp`。此时内存里那张隧道表是完好的（3 条）——
这才是"按产品现状交付"能走到的最好形状，也是正面回答你那个问题的唯一一把尺子：
> 这个是只存在于 vpp 重启后的一个 bug？还是说咱们的路由协议本身机制上有什么不支持动态更新路由表的

「表完好」不来自 `run.out` 里那句 `隧道=0 条` —— 那是第七节第 3 条的假 0（URL 少了 `/status`）。
它的依据是三条硬事实：① 进程没换过（fib_pid 300421 从 15:46:33 起没变，那轮 `PUT /config`
装进 3 条）；② 重启期间 sr-fib 抱怨的是「隧道接口 wg1010 不存在」而不是「下一跳不在隧道表里」，
说明闸① 是过的、它认得这张表；③ 收口后用修好的口径读 `/status`，`tunnels=3`、`converged=True`。

盒子时钟（UTC）与读数：

| 时刻 | 发生了什么 |
| --- | --- |
| 16:39:51 | 阶段 0：`r0=2 t250=3`、broken pipe 累计 4988、fib_pid=**300421**、vpp_pid=300750 |
| 16:39:53 | `systemctl restart vpp` 发出，之后不干预 |
| 16:39:54 | **判据A：2 秒**重连上 `api.sock`，日志「已连上 VPP api.sock，按整份计划从头收敛」，fib_pid 仍 300421 |
| 16:39:57–16:40:05 | 逐秒「本轮收敛失败（保留上次成功的计划）: fib: 隧道接口 wg1010 不存在」—— 隧道口还没被 agent 建回来，sr-fib 按第 1 号硬规则**不猜、不删、不清** |
| 16:40:06 | 一条「下一跳 10.250.0.2 的 BFD 会话不是 Up，该路径跳过」（闸②），同秒 `wg1014` 仍不存在 |
| 16:40:07 | 「收敛 tables+0 **labels+1/-0 routes+2/-0** mpls-egress=true」 |
| 16:40:08 | **判据B：13 秒** `r0=2 t250=3` 齐；broken pipe 整轮只 +2（4988→4990）后静止 |

- **判据C**：阶段 0 pid=300421 → 结束时 pid=300421。这条路径里没有任何 restart 兜底。
- 客户可见中断（探针口径，时间轴已按 `clock-anchor.sh` 对齐；CPE4 慢 225s、cust4 快 28675s、cust3 快 28686s）：
  cust4→cust3 **15.3s**（15 条 LOSS），cust3→cust4 **15.7s**（12 条 LOSS），全程丢包率各 0.3%，
  通的时候 rtt 中位 15.0 / 15.1ms。
- 和上一轮（同样重启 VPP，但 sr-fib 被换过进程、隧道表丢了）对照：

  | 形状 | 客户黑洞 | 是否自愈 |
  | --- | --- | --- |
  | 隧道表完好（本轮，产品现状） | 15.3s / 15.7s | **13s 自己回来**，无人干预 |
  | 隧道表丢失（上一轮，= G-2） | 290.1s / 294.7s | 探针窗口内不自愈，端到端 482s 靠 lab 脚本推表 |

**答案**：动态更新路由表在机制上没有缺口 —— 摘路、重装、按整份计划收敛，全在 1s 轮询里自己做完；
`labels+1/-0 routes+2/-0` 是重连那一轮装的，不是进程启动时顺手装的。
缺陷⑦ 只是「VPP 的 api.sock 断了不重连」，已修；
剩下的那条**永久黑洞来自缺口 G-2（隧道表只在内存、`fib.json` 落盘 `"tunnels": []`）**，
和路由协议无关。
按硬规则第 4 条点名：`systemctl restart vpp` 必然中断，实测 **15.3–15.7s**，
构成是 VPP 数据面清零 + agent 重建 wg 口 + BFD 重新 Up + sr-fib 重装四段，没有一段是缺陷。


### 9.18 问题 6 收口（#270）：VM 开机即最优 —— 主线程落点写进启动配置，八臂 2×2 对照 + CPE4 真断电重起（2026-11-06 凌晨）

owner 的原文是「vpp 性能问题还在，我从主机 ping 192.168.1.103 是低延迟，其他都是高延迟。你查一下文档，
vpp/dpdk 应该怎么调优性能，需要让所有 cpe 在开机时候自动就是最优状态，而不是需要后续调优」，
随后收窄成一句「不用管真机了，那个真机我没装 vrf 内核模块。你就在 vm 上，把 vm 的性能调优做好，
保证 vm 开机就是最优状态就行」。

一句话答案：**症状完全复现，根因是 VPP 主线程落在哪个 vCPU 由内核开机随手给（四台里三台拿到 vCPU1，
那台拿到 vCPU0 的就是 103）；把 `cpu { main-core 0 }` 写进启动配置后，真关机再开机量回来的就是最优状态，
不需要任何后置调优动作。** 开机路径上唯一还剩的不最优是 sr-fib 的隧道表（=0 条），那一条是缺口 G-2（§9.6），
不属于 #270，第八、九节把两件事分开写。

#### 一、先把 owner 那把尺子接住：管理口 ping 本身就是有效尺子（臂 A）

盒子的管理 IP `192.168.1.10x` 不在 underlay 物理口上，而在内核 `vpp0` 这个 linux-cp 设备上。
所以主机 ping 它会走「报文进 VPP → VPP punt 给内核 → 内核回 → VPP 发回」这条路径，
VPP 主线程在忙什么直接决定这条路径排多久。不必换成别的尺子，先照原样量：

| 台 | VPP 主线程掩码（`taskset -cp` 读回） | 主机→管理口 avg（40 包 ×3 轮） |
| --- | --- | --- |
| CPE1 | **1** | 7.252 / 5.333 / 5.783 ms |
| CPE2 | **1** | 5.672 / 6.122 / 5.900 ms |
| CPE3 | **0** | **1.991 / 2.311 / 2.068 ms** |
| CPE4 | **1** | 5.745 / 5.304 / 5.287 ms |

四台的 WAN 硬中断亲和性（`/proc/irq/17/smp_affinity_list`）当时都是 `0-1`，也就是没被谁钉过。
只有 CPE3 快，因为它的主线程自然落到了 cpu0 —— owner 看到的「只有 103 低延迟」就是这个。

#### 二、快的是跟着核走，不是跟着盒子走（臂 B：只改运行时掩码）

这一步不重启、不改配置，只 `taskset -pc`：CPE1→cpu0、CPE4→cpu0、CPE3→cpu1，
CPE2 不动作为对照。读数（同一口径）：

| 台 | 臂 A（改前） | 臂 B（换落点后） | 掩码读回 |
| --- | --- | --- | --- |
| CPE1（1→0） | 5.333~7.252 | **1.783~1.847** | B 后 =0 |
| CPE4（1→0） | 5.287~5.745 | **1.699~2.062** | B 后 =0 |
| CPE3（0→1） | 1.991~2.311 | 4.104~6.356 | B 后 =1 |
| CPE2（未动） | 5.672~6.122 | 6.554~8.052 | 仍 =1（复核过） |

变快的跟着 cpu0 走、变慢的跟着 cpu1 走，没动的那台维持原样。所以这不是"3 号 VM 天生快"，
落点是因、盒子是无辜的。

#### 三、三条听起来都像"绑核优化"的路，各自被自己的尺子否掉

1. **把 WAN 硬中断挪开 VPP 所在核**（臂 C）：只把 CPE2 的 IRQ17 钉到 cpu0，VPP 主线程留在 cpu1。
   结果 6.303 / 6.579 ms —— 和没动一样。这条确实动了：`/proc/irq/17/...` 计数里 CPU0 从 523561
   涨到 526927、CPU1 一动不动（8582197 → 8582197）。中断落点不是开关。
2. **vCPU 天生不等价**（盒内纯 CPU 循环 + 盒内 loopback ping）：
   同一个进程只换核跑 2 秒循环，CPE1 是 cpu0=1,624,976 / cpu1=4,176,454 iters，
   CPE2 反过来 cpu0=3,387,430 / cpu1=2,162,526 iters —— "哪个核快"在两台之间方向相反；
   而盒内 loopback ping 钉到两核都是 0.05ms 量级（CPE1 0.049/0.046，CPE2 0.052/0.060）。
   纯算力和本地路径看不出核差，所以它解释不了客户看到的 4 倍往返差。
3. **af-packet 的收包模式**（interrupt vs polling）：在两个落点上各测过，见第四节的 2×2 —— 不改变结果。

#### 四、八臂 2×2：唯一站得住的开关是主线程落点

行=VPP 主线程落点，列=af-packet 两口 `rx-mode`；单元格给的是**客户 overlay 往返**（cust4↔cust3 loopback，
各 3 轮 ×40 包的 avg 区间）和**隧道内探针**（`show sr-wg probe`，n=24）。

| | interrupt | polling |
| --- | --- | --- |
| **四台都在 cpu0** | 臂 F：9.70~14.98ms，探针 2.44/2.29<br>臂 F2：8.85~14.20ms，探针 2.28/1.56<br>臂 K：9.65~14.09ms，探针 1.96/1.79 | 臂 H：8.44~11.42ms，探针 1.52/3.17<br>臂 L：7.96~12.17ms，探针 2.25/1.70 |
| **四台都在 cpu1** | 臂 G：22.07~30.41ms，探针 6.71/6.25<br>臂 N：22.25~27.43ms，探针 8.74/5.47 | 臂 M：22.06~26.33ms，探针 5.76/6.33 |

- 落在 cpu0 的五臂（F/F2/K/H/L）没有一条超过 15ms；落在 cpu1 的三臂（G/M/N）没有一条低于 22ms。
  两个维度合起来 8 臂、0 个反例（反例的来源是我自己标错臂，见第五节第 1 条）。
- 反序复现：F→G→F2 连着摆两次，客户往返跟着落点来回摆（14.98 → 30.41 → 14.20），
  不是"时间往后就漂"。
- 同臂的管理口 ping 也一起摆：臂 G（cpu1）4.220~7.318ms，臂 F2（cpu0）1.719~2.282ms。
  owner 最初那把尺子和客户那条尺子在同一个开关上，这点很重要 —— 不是只有 ping 管理口受影响。

#### 五、我自己这把尺子上的两处错（写进文档，因为它们差点把结论写成"有残差"）

1. **臂 H、K 的标签是错的。** 我给它们的描述是「四台仍在 cpu1 + 改 rx-mode」，但摆到 H 之前那一步是臂 F2，
   F2 已经把四台 `taskset -pc 0` 过去了（读回 `CPE1 mask= 0CPE2 mask= 0...`），H 和 K 的命令里
   **只有 `set interface rx-mode`，一次都没重新钉核**。所以 H/K 的真实形状是 cpu0+polling、cpu0+interrupt。
   我是先看见"这两臂的读数和 cpu0 臂一样好，而 G/N 一样坏"，第一反应记成"cpu1 有好有坏 ⇒ 2×2 有残差"，
   然后才去补了臂 M —— M 把掩码和 rx-mode **一起读回**（`CPE1 mask=1 rx=(polling) (polling)`），
   才看清是标签错、不是数据有残差。**教训**：摆臂的"当前落点"必须由读回定义，不能由上一臂的意图继承；
   这正是 §2 证据规则说的"状态对比"，我在这条上自己摔了一次。
2. **`boot-check.sh` 把管理口 ping 的 avg 抽成了单词 "max"。** 上一版用
   `grep -E 'round-trip|rtt' | tail -1 | sed 's#.* = ##' | cut -d/ -f2`，在 ping 全丢那一刻
   抽到的是行内的单词而不是小数，打印成「管理口 avg=maxms」，而判据恰好也判 FAIL —— 假对的读数。
   现在只认 `min/avg/max` 那串小数（`grep -oE '[0-9]+\.[0-9]+/[0-9]+\.[0-9]+/[0-9]+\.[0-9]+'`），
   抽不到就打「无样本」。修完在同一台重跑：3.553ms（`raw/boot-CPE4-rerun-after-fix.log`）。

#### 六、动了什么（只有一处配置，加一条判据）

- `test/lab/vmlab/vm-bringup.sh`：生成的 `/root/vmlab/startup-vmlab.conf` 顶部加
  `cpu { main-core 0 }`，并把上面的数字和"生效必须重启 VPP"写在同一段注释里
  （`test/lab/vmlab/vm-bringup.sh:119`）。这一句必须在启动配置里：VPP 只在启动那一刻读 `cpu {}` 段。
- **没有走 systemd `CPUAffinity=` 覆盖，也没有留任何运行时 taskset 兜底**。那两条都需要有人每次开机再跑一遍，
  等于把"开机即最优"重新变成"事后调优"，正是 owner 否掉的东西。开机读回里专门验了这一点：
  `systemctl show vpp -p CPUAffinity --value` 为空。
- 铺到四台：`exp-270/deploy-main-core.sh`（备份 → 插段 → `systemctl restart vpp` →
  盒上脱离 ssh 的 60s 看门狗：如果 `vpp_main` 没回来就还原备份并再起一次）。
  盘上留的备份：CPE4 `startup-vmlab.conf.bak-270`，CPE1/2/3 `startup-vmlab.conf.270.bak`。
- **按硬规则第 4 条点名这条改动的中断**：它是启动期配置，生效路径必然重启 VPP。
  CPE4 那一档（隧道表完好、只重启 VPP、不碰 sr-fib/agent）实测客户可见黑洞 **15.3s**（双向各 15.3/15.7s，
  丢包率各 0.2%，2s 重连上 api.sock、13s 装表齐，全程自愈、sr-fib pid 不变），
  原始读数在 `exp-270/raw/deploy-main-core-driver.log`。
  CPE1 那一档实测窗口 1.5s 自愈。**CPE2/CPE3 两档不能干净归因给 #270**：摆这两台的时候缺陷⑨（#271，
  回灌）正在把 `172.16.3.1` 那条前缀打成黑洞，两份探针样本的尾巴停在 LOSS 里（8.4s / 23.1s 未收尾），
  把那段说成"重启 VPP 造成 8~23s 中断"是把 #271 的账记到 #270 头上 —— 所以这一节只报能归因的 15.3s。
- **对照：只改运行时掩码是 0ms**。覆盖 K/L/M/N 四臂切换的守门探针共 13,966 条样本、丢包行数 0
  （`/tmp/probe270b.out`）。"改配置+重启"与"只改运行时"必须分开报价，否则下一步没人敢动启动配置。

#### 七、判据从"我写了这句配置"升级到"VPP 自报在哪个核"

`vm-verify.sh` §1 新增一条（`test/lab/vmlab/vm-verify.sh:219`）：**同时**要求
① 启动配置里 `main-core 0` 行数≠0、② `vppctl show threads` 里 `vpp_main` 那行的 Core 列 = 0，
两者都成立才 PASS，否则把实际 Core 值和配置行数一起打出来。只看配置文件等于"我写了所以生效了"，
§2 证据规则不收这种验证。

顺带一条读数口径，也是踩过的：`show threads` 的 `vpp_main` 行里 Core 是**第 7 列**
（`$1`ID `$2`Name `$3`LWP `$4 $5`"other (0)" `$6`lcore `$7`Core `$8`Socket）。第一次按 `$6` 读，
在没有 DPDK 的这份构建上 lcore 恒等于 0，量什么都"通过"，是个假 PASS。

四台全量 `vm-verify.sh` 在这一次改动之后复跑：**PASS=372 FAIL=21 WARN=0**（日志 `/tmp/verify-270.log`）。
21 条 FAIL 逐条读过去，全部点名同一个前缀 `172.16.3.1/32`，一条不落在这次的改动上：
第 5 节 sr-route 的 RIB 与台账对不上（四台各差这一条）、第 6 节这条前缀在 CPE1 没压标签 1000
且路径里没有 `mpls via 10.250.0.1`、CPE2 那一节 10 秒内 FIB 没收敛、第 7 节正向导出这条不在主表 K/lo
（可能被回灌）、第 8 节 CPE1/CPE2 内核主表里这条走的是 `via 172.31.12.2 dev lcp-x0 proto ospf`
而 zebra 里 `Known via "ospf", distance 110`，CPE3 上反过来是本地前缀被压了标签走隧道、
zebra 里那条 `Known via "kernel", distance 0` 压住了 bgp 那条，第 9 节以这一条为源或为宿的四组客户互 ping
100% 丢包。这一簇就是缺陷⑨（#271）已经登记的那条：回灌链路把 `172.16.3.1` 从 CPE2 经客户侧 OSPF 灌回 CPE1，
RR 选中回灌那条路径之后三条客户路径全黑洞 —— 上面那些 zebra/kernel 读数都是它的后果。
把这一簇和 #270 分开的证据是同一轮里的另三段：#270 自己那条落点判据四台全 PASS，
第 3 节隧道层（口名 / overlay / 表 250 / BFD 全 Up）全 PASS，第 4 节 overlay 两跳 `10.250.0.x` 十组全通，
第 9 节里不牵涉 `172.16.3.1` 的两组照旧 0% 丢包（cust12→`172.16.4.1` avg 5.29ms、
cust4→`172.16.12.1` avg 6.34ms）。
上一条全绿基线是 **388/0/0**（§9.14 那张表，脚本哈希 `ee083fd…`），这次脚本新增判据后条数变了，
所以这里只做分项对账、不做减法对账：**#271 拍板并修掉之前，这 21 条不会自己回到 0。**

#### 八、真开机复测（owner 要的"开机就是最优"，CPE4 完整走一遍）

`BOX=CPE4 ./exp-270/boot-check.sh`：ACPI 按钮关机（guest 自己收口；`machinereadable` 里状态字面是
`poweroff`）→ headless 启动 → ssh 通 **35s** → 按同一口径读回，全程脚本不改任何东西。

| 项 | 读数 |
| --- | --- |
| 落点 | `show threads` 自报 **Core=0**（pid 910）、`Cpus_allowed_list=0`、配置里有 `cpu { main-core 0 }` |
| 没有后置兜底 | `vpp.service` 的 `CPUAffinity` 为空；drop-in 只有 `50-vmlab.conf`/`52-vmlab-underlay.conf`；ExecStart 就是我们生成的那份 conf，start_time=2026-11-06 02:16:35 UTC |
| 主机→管理口 | avg **2.872ms**（vCPU1 时代 5.3~7.3ms） |
| 隧道内探针 | avg **4.10ms**、max 11.10ms（n=12） |
| underlay 自愈 | host-enp0s8/9 都在 VPP 里、MTU 1500/0/0/0；内核 `vpp0` 有 192.168.1.104/24 + 默认路由 via 192.168.1.1 |
| 守护进程 | sr-agent 2520 / sr-route 2535 / sr-fib 2553 / sr-frr 2573 —— 由 `sr-box-watch.timer` 在开机后约 90s 自己起 |
| 隧道与 BFD | wg 口 3 条、BFD 会话 3 条全 Up（缺陷④ 的判据在真开机路径上成立） |

两份原始读回都要留着，不能只留全绿的那份：
- `exp-270/raw/boot-CPE4-first-readback.log`（ssh 通就读，**PASS=7 FAIL=7**）：4 条 FAIL 是"守护进程还没起"
  （定时器还没跑，时间差不是缺陷），1 条 FAIL 是隧道表=0（见第九节），另 1 条是第五节第 2 条那个坏读数。
- `exp-270/raw/boot-CPE4-after-fibsync.log`（lab 脚本把隧道表推回去之后，**PASS=14 FAIL=0**）。
  如果只留这一份，就等于把 G-2 读成"开机自己就好了"。

#### 九、开机路径上唯一剩下的不最优，就是缺口 G-2 的现行现场

真开机那一刻 sr-fib 的 `/status` 里 `tunnels=0`、上次收敛 warnings 非 0，
日志打的是闸① 那句「下一跳 … 不在隧道表里，该路径跳过」，于是 overlay 客户前缀一条都装不出来
（`/etc/sdwan/fib.json` 落盘的就是 `"tunnels": []`，产品里到今天没有任何进程维护这张表，§9.6）。
lab 里现在靠 `vm-fib-sync.sh` 事后 `PUT /config` 推 —— 那正是 owner 说的"lab 上不该有任何非生产的东西"。
已拍 (a)：agent 自己派生这张表并推给 sr-fib，触发全动态（开机 / 隧道增删 / 检测到 VPP 重启）、不落盘、
不许假设谁先起来。方案单独出（`docs/g2-tunnel-table-derivation-plan.md`），代码等他点头。
写方案时读到的一条前提要在这里留一句：**颜色今天根本不在设备侧契约里**
（`internal/wire/wire.go` 全文没有 color，`cmd/sr-ctrl/underlay_delivery.go:134` 没抄
`underlay.Tunnel.Color`），所以 §9.6 原先那句"agent 手上已经有颜色"是错的，颜色要先进隧道段。
**在 G-2 收口之前，"VM 开机即最优"只对到 VPP 落点 / underlay 接管 / 隧道 / BFD 这一层，
overlay 装表那一层开机仍不最优。**

#### 十、和 §9.16（#267）的关系：不推翻那条，是把那条留的"待拍"落掉

§9.16 的结论是「绑核买到的不是平均值、是可复现性；要真动只有'给 VPP 写死 main-core'一条，
而那必须重启 VPP，已登记待拍」。那一轮量的是**单台、运行时 taskset**；这一轮量的是**四台同时、
且把落点写进开机配置**，在客户可见的往返上是 22~30ms 对 8~15ms 的平均值差，不止尾部。
两轮的口径不同：不能拿 §9.16 那句"绑核不改变平均值"来否掉这一节，
也不能反过来把 §9.16 那八块数字说成量错了。owner 这次点的"开机就是最优"，等于拍了那条待拍。

#### 十一、怎么重跑

```
cd test/lab/vmlab/exp-270
LABEL=G-全cpu1 ./sample.sh                    # 三把尺子（管理口 / 客户 overlay / 隧道内探针）
BOX=CPE4 SKIP_POWER=1 ./boot-check.sh         # 只读回当前开机状态，不动 VM
BOX=CPE4 ./boot-check.sh                      # 真关机→开机→读回（会打断 CPE4 上的客户流量 ~15s+）
CORE=1 ./deploy-main-core.sh CPE4             # 摆配置臂（会 restart vpp，带 60s 看门狗自动还原）
bash ../vm-verify.sh | tail -30               # 全 lab 判据（含第七节那条新判据）
```

原始读数在本目录 `raw/`：`arms-270.log`（臂 A/B/C/F/G/F2/H/K/L/M/N 与排除项的逐行输出）、
两份开机读回、`deploy-main-core-driver.log`（15.3s 那一档）。

### 9.19 缺陷⑨（#271）收口：本盒客户侧拥有的前缀被自己注入的那条 K 路由顶掉，从此永久锁死 —— 三道闸门 + 四台换件前后四层落点与六方向（2026-11-06 凌晨）

owner 的原文是两句：「继续继续一直搞吧。而且我看到现在 cpe1 上在 vpp 里有 172.16.3.1 的路由。但是没有注入到 frr 里。
也查查是怎么回事，修复掉。注意所有的修复是指把软件 bug 修复，不是让 lab 看起来正常」，以及
「也就是说不允许手动方法修复」。

一句话答案：**现象是真的，因果是反的 —— CPE1 那条 `172.16.3.1` 不是"该注入没注入"，而是内核和 zebra 本来就已经
有它（客户 OSPF 学到的，distance 110），sr-frr 这一轮是刻意不再往里写第二条；真正坏掉的是 CPE3：它把自己客户侧
eBGP 学到的 `172.16.3.1` 用一条自己注入的 K 路由（distance 0）顶掉了，顶掉之后 zebra 不再发通知，这台盒子从此
"看不见"自己拥有这个前缀，远端反射回来的同前缀路由反被当成正常远端路由装上隧道 —— 状态锁死，六方向 4 条 100%
丢包。** 修复全在产品码（`controller/internal/frr` + `controller/cmd/sr-frr`），lab 里一条 `ip route`、一条
`vtysh` 配置、一个手工脚本补丁都没打过；换件生效走的是产品自己的开机清单，没有任何 daemon 冷启动（第六节
逐台量了这次操作本身的客户可见时长）。

> **2026-11-06 更正（§9.20）**：owner 把这条重新定性为**客户侧通告策略造成的路由环路**，
> 因此本节第四节的 **A1 注入闸门**（`plan.go` 的 `owned` 输入与 `Plan.Withheld`、`import.go` 的
> `OwnedByCustomer()`、`/status` 的 `local_withheld`）**已撤回**，改由控制台「路由泄露」里
> `CPE-SDWAN`（`to_sdwan`）那一行只播本站客户侧前缀来断环；**A2 链路窗口与 A3 读先于写 + 首轮冻结
> 原样保留**（那两条修的是"看不见 ≠ 没有"，与这条环路无关）。本节第四节的行号（`main.go:836-840`、
> `plan.go:61-76`、`import.go:368-392`）随之失效，A2/A3 的新行号见 §9.20 第二节；其余取证读数
> （第一、二、三、六、七、八节）**一字不改**，它们描述的是同一台 CPE3 在同一个 lab 里的真实形状，
> §9.20 用三态 A/B/C 把它们和新的修法接上。

#### 一、先把 owner 那把尺子接住：`172.16.3.1` 在 CPE1 的四层落点各是什么

换件后（`layers-after-CPE1.txt`，2026-11-06T00:27:47Z）逐层读回来：

| 层 | 读数 | 这一层为什么长这样 |
| --- | --- | --- |
| 内核 main | `172.16.3.1 nhid 999 via 172.31.12.2 dev lcp-x0 proto ospf metric 20` | 客户侧 ospfd 学到、zebra 装进内核的那条真路径。`lcp-x0` 是 VPP linux-cp 送进内核的口子，下一跳 172.31.12.2 是 cust12 那一侧 |
| zebra | `Known via "ospf", distance 110, metric 20, best` | 前缀在 FRR 里活着，而且是 best。**所以"没注入到 frr"这句在这里不成立 —— 它在 FRR 里，只是来源是 ospf 不是 kernel** |
| VPP FIB | `[@0] ipv4 via 172.31.12.2 host-enp0s8` + `[@2] ipv4 via 192.168.1.103 host-enp0s9` | 第一条是 linux-nl 把上面那条内核路由镜像进 VPP（§9 前面几轮验证过 linux-nl 就是这个语义），第二条是 WAN 侧的 underlay 直连，两者都不是隧道 |
| sr-route RIB | `rib=[{"dev": 3, "nh": ["10.250.0.1"]}]` `local=True` | overlay 里也**有**这一条（下一跳是 CPE3 的隧道 overlay 地址），`local=True` 说明本盒仍在向 RR 宣告它 |

也就是说：CPE1 上 `172.16.3.1` 三层都有，overlay 那条也装着，唯一的"没有"是 sr-frr 的注入清单里没有它 ——
而这正是这次要修的那个缺陷修好之后的正确形状。`/status` 现在把这条决定留了痕：

```
CPE1  injected=1  reported=2  local_withheld=['172.16.3.1/32']  export_frozen=false
```

另一半看起来像"没注入"的现象在 sr-route 侧：RIB 的own-route 规则会把"本盒自己上报过的（local）前缀"再从远端
收回来的那一条丢掉（计数器 `dropped_own_route`），所以 owner 如果按前缀去数 RIB 条数，会看到同一台盒子的条数
随上报抖动。这一条不是缺陷，是对称防环（第四节末）。

#### 二、杀伤链：一台盒子怎么把自己锁死的

取证快照 `before-CPE3.txt` / `layers-before-CPE3.txt`（2026-11-05T23:12 / 23:20Z），逐步走到哪：

1. **导出侧先落地。** sr-route 的 RIB 当时有 3 条（`172.16.12.1/32` dev1、`172.16.3.1/32` dev1、
   `172.16.4.1/32` dev4），`frr.PlanFrom`（`controller/internal/frr/plan.go:77`）照单全收，
   经 `internal/frr/krt` 写进内核：`RTPROT_BOOT`、VPN0 落 main(254)、`dev lo`。于是账本
   `state.json` 里出现 `172.16.3.1/32,172.16.4.1/32,172.16.12.1/32` 三条，内核里三条 `dev lo scope link`，
   `injected=3`。**注意第一条就是这台盒子自己客户侧拥有的那一条**（cust3 通过 eBGP 播给 CPE3 的 loopback）。
2. **K 路由在 zebra 里是 distance 0，把真路径顶掉。** `show ip route 172.16.3.1/32` 当时是
   `Known via "kernel", distance 0, metric 0, best`；客户那条 bgp/20 还在 BGP 表里
   （`before-CPE3.txt` [5]：`Paths: (2 available, best #1)`），但 zebra 因为 distance 0 已经不装它 —— 
   `show ip route bgp` 是空的。bgpd 侧多出一条自源、weight 32768 的路径，这是 `redistribute kernel`
   把自己那条又学了回来。
3. **顶掉之后通知就断了。** 反向导入靠 zebra 的 redistribute 通知（订阅是 `vpn0/type10@0`，type10=bgp）。
   一条路径不再被 zebra 选中，就不再有它的通知；`Importer` 的可见集里从此没有 `172.16.3.1`。
   读数：`reported=0`，PUT 给 sr-route 的 `/local-prefixes` 变成空集，sr-route 侧 `local=[]`。
4. **本盒不再宣告，远端那条反被当真。** CPE3 不再播 `172.16.3.1`，但 RR 上这一条仍由别的盒子在播
   （`rib` 里那条 `dev 1 / nh 10.250.0.0` 就是 CPE1 反射回来的）。`sr-route` 按 own-route 摘除是**按 device
   不按 prefix**，本盒不再上报之后，这条远端路径在 CPE3 眼里就是普通远端路由。
5. **闭环自持。** 下一轮 `PlanFrom` 看到的输入是「RIB 里有 `172.16.3.1`（远端那条）+ 客户侧可见集里没有它」
   ⇒ 结论仍然是"该注入"。于是重启 sr-frr 也救不回来：旧代码第一件事就是把这三条重新写进内核。
   **这就是"必须改软件、手动改 lab 只会掩盖它"的原因** —— 手工 `ip route del` 一条，下一拍又被加回来。

同一时刻 CPE1/CPE2/CPE4 都没锁死，因为它们各自客户侧拥有的那条要么本来就靠隧道（没有第二条来源），要么
`injected` 清单里没有自己那条：`before-CPE1 injected=1 reported=2`、`before-CPE2 injected=1 reported=1`、
`before-CPE4 injected=2 reported=1` —— 只有 CPE3 是 `injected=3 reported=0`。

#### 三、六方向实测：这就是"lab 看起来不正常"的那一半

§9.7 口径（`ping -c 50 -i 0.2 -W 1 -I <源 loopback>`），`six-before.txt`（2026-11-05T23:30:41Z）：

| 源 → 目的 | 丢包 | rtt min/avg/max (ms) |
| --- | --- | --- |
| cust12(172.16.12.1) → 172.16.3.1 | **100%** | — |
| cust12(172.16.12.1) → 172.16.4.1 | 0% | 5.408/16.050/49.786 |
| cust3(172.16.3.1) → 172.16.12.1 | **100%** | — |
| cust3(172.16.3.1) → 172.16.4.1 | **100%** | — |
| cust4(172.16.4.1) → 172.16.12.1 | 0% | 3.965/10.270/30.083 |
| cust4(172.16.4.1) → 172.16.3.1 | **100%** | — |

四条全灭的方向共同点是**目的或源是 `172.16.3.1`**，也就是 CPE3 锁死的那一条。两条通的方向都不经过它。

#### 四、三道闸门：改在产品码的哪一处、为什么是这个次序

`cmd/sr-frr/main.go:13-23` 的 package 注释把这轮的结论钉成了不变量：**每一轮先读完并上报客户侧（导入），
再拿这份"客户侧拥有什么"去约束注入（导出）**。反过来就是第二节的锁死链。轮询里就是这个次序：
`main.go:798-812` 先算 `exported`、连 zebra、`imp.Tick` / `imp.Report` 并 PUT，
`main.go:836-840` 才拿 `imp.OwnedByCustomer()` 去 `exp.reconcile`。

- **A1 注入闸门**（`plan.go:61-76` 规则、`plan.go:121-128` 实现、`import.go:368-392` 输入）。
  `PlanFrom` 多了一个 `owned` 输入：本盒客户侧已经有路由的前缀，**一条都不写进内核**，改判进 `Plan.Withheld`。
  输入用的是 `OwnedByCustomer()` —— 白名单与防环过滤**之前**的那份可见集，理由写在函数注释里：闸门要问的是
  "客户侧到底有没有这条路"，不是"我们打算上报哪条"；用过滤后的集合会把白名单外的真实客户路径当成"没有"。
  `Withheld` 不进内核、不进状态文件，只进日志和 `/status`（新字段 `local_withheld`），否则"少了几条"看起来像丢路由。
  与它对偶的是导入侧原有的 `LoopDropped`：`Report` 的第二个入参 `exported`（`exportState.exportedSet`，
  `main.go:250-258`）把本 VPN 正在导出的前缀从上报集合里扣掉，防止"我注入的又被我当成客户侧的上报出去"。
- **A2 链路事件乐观窗口**（`import.go:32`、`import.go:262-287`）。**（#274 已整族撤回，见 §9.21；下面这段留作历史，
  其中"实测会话 15s 才回来"没有原始读数支撑，已作废 —— 本轮实测的 refill 是 12~19s，那是"zebra 重新收录
  K 路由 + sr-frr 1s 轮询"的合计时长，不是客户 BGP 会话的恢复时长。）** VPP 重启会拆掉 lcp/vpp0，客户 eBGP 会话
  跟着掉；实测会话 15s 才回来，所以 `BeginLinkWindow()` 开 30s 窗口，窗口内 `visibleLocked()`
  （`import.go:355-366`）把 `stale`（上一份真实集合）并进可见集。这既是 AGENTS.md §1.3 "控制面重连不得让数据面
  抖动"在导出方向的应用 —— 不把"暂时看不见"当成"真的没有"，否则那一拍既会把整网抖动，又正好给 A1 喂进空集合、
  让注入抢先落地。
- **A3 读先于写 + 首轮冻结**（`main.go:297-318`、`import.go:289-298`）。**（#274：首轮冻结与 `Settled()` /
  `export_frozen*` 已删除，"读先于写"的次序本身留着，见 §9.21 第二节。）** `frozen = len(rc.rules) > 0 && !imp.Settled()`
  （`main.go:839`）：只要配了导入规则而第一轮对账还没落地，这一轮**只撤不加** —— 照常撤掉 RIB 里已经没了的，
  新的注入压下计数进 `export_frozen_held`。`Settled()` 的语义是"窗口关闭过至少一次"，即这份答复背后有一次
  我们看着它跑完的回放。没有这条，开机时 zebra 后答的那台盒子会先把自己那条注进去，然后再也看不见它。
  `-once` 用同一个判据，20s 到点仍没落地就按"只撤不加"退出并把"没读到"写进日志（`main.go:872-883`），不假装完成。

#### 五、生效姿势：换件全程零冷启动（这也是"不许手动修 lab"能落地的前提）

四台 `/usr/local/bin/sr-frr` 从旧产物 `9114bdffb5cd7d23…` 换成新产物 `8b15abd5444af276`，每台三步：

1. 产物 gzip+sha256 推到 `/root/vmlab/artifacts/linux/`（`bringup-mac.sh` 按哈希增量推送）；
2. `bash /root/vmlab/vm-bringup.sh --phase B` —— B 段只做 `install`（`vm-bringup.sh:102-115`），不重启任何东西；
3. `kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service` —— SIGTERM 走产品自己的收尾
   （日志 `sr-frr: 停止，撤掉自己下发的 2 条内核路由`），watch 是 Type=oneshot + `KillMode=process`，只补缺失进程。

VPP / FRR / sr-agent / sr-route / sr-fib 全程 pid 不变；改的是 sr-frr 一个进程，用产品开机清单补起来。
旧产物留在 Mac 侧 `~/srvmlab/artifacts/prev-20261106-271/sr-frr.old-9114`。

#### 六、这次操作本身的客户可见中断（AGENTS.md §1.4：必然中断的必须点名并给实测时长）

优雅重启 sr-frr **不是**零中断：它撤掉自己下发的 K 路由再重新落地，这期间"由这台盒子代发"的远端前缀在客户网络里
会消失。口径：`window-271.sh`，在源侧用 loopback 秒级不停 ping 一条**当时通**的方向，75s 窗口里第 6 秒 SIGTERM，
再数断流段。实际节奏 ≈32 发/秒（循环里没有 sleep，通的时候受 RTT 限制、断的时候每发吃满 1s timeout），
所以一段连续 FAIL（1 行/秒）就是硬断流，没有"半断"糊在中间。

| 台 | 探测方向（源→目的） | 总探测 | FAIL | 断流段 | 最长断流 |
| --- | --- | --- | --- | --- | --- |
| CPE1 | cust12 172.16.12.1 → 172.16.4.1 | 2444 | 9 | 00:19:26.5→00:19:34.8 | **8.3s** |
| CPE2 | cust12 172.16.12.1 → 172.16.3.1 | 2679 | 0 | 无 | **0s** |
| CPE3 | cust12 172.16.12.1 → 172.16.4.1（第一次，解锁死那一枪） | 2814 | 0 | 无 | **0s** |
| CPE3 | cust3 172.16.3.1 → 172.16.4.1（第二次复测） | 2781 | 4 | 00:20:57.5→00:21:00.5 | **3.1s** |
| CPE4 | cust4 172.16.4.1 → 172.16.12.1 | 2657 | 4 | 00:17:51.7→00:17:54.7 | **3.0s** |

CPE1 那一枪为什么长：日志逐行对得上 —— `00:19:23 停止，撤掉自己下发的 2 条内核路由` → `00:19:25` 新进程起来
→ `00:19:26` 重连对账完成、首轮把 2 条压进闸门 → `00:19:36 收敛 routes+1/-0`。也就是"撤了"到"重新落地"之间
13s，探针看到的是其中 8.3s 全断。这段是**冻结窗口 + 首轮闸门**的代价：宁可少注入几秒，也不能抢先把客户侧路径顶掉。
CPE3 第一次 0s，是因为当时那台还锁死着（本来就不通，撤掉注入反而解了锁）；第二次 3.1s 才是它正常态下的真实窗口。
CPE2 的 0s 是这条方向有冗余（cust12 的 OSPF 里 CPE1/CPE2 同站点，另一台还在播）。**要落到生产页面上说清楚的
就是这一列数字**，不是"热更无中断"。

#### 七、换件后同一口径复测

四层落点（`layers-after-CPE{1..4}.txt`，00:14–00:27Z）与 `/status`：

| 台 | injected | reported | local_withheld | 内核里自己那条 | sr-route `local` |
| --- | --- | --- | --- | --- | --- |
| CPE1 | 1 | 2 | `172.16.3.1/32` | ospf 110/20（客户路径，没被顶） | True |
| CPE2 | 2 | 1 | — | ospf（同上） | True |
| CPE3 | 2 | 1 | `172.16.3.1/32` | `via 172.31.3.2 dev lcp-x0 proto bgp metric 20`（**真路径回来了**） | True |
| CPE4 | 2 | 1 | — | bgp（同上） | True |

- CPE3 的账本从 `172.16.3.1/32,172.16.4.1/32,172.16.12.1/32` 变成 `172.16.4.1/32,172.16.12.1/32`，
  `reported 0→1`，sr-route 侧 `local []→[{vrf 0, 172.16.3.1/32}]`，`dropped_own_route 620→624`（本盒上报后
  按 device 摘除远端同前缀，计数器在动说明这条对称规则在生效）。
- CPE2 换件前有一个"幽灵 K 视图"：zebra 报 `Known via "kernel"` 但内核 `ip route` 里没有 —— 换件后由
  一次真实注入补齐（`injected 1→2`），不再是分歧。
- 六方向复测（`six-after.txt`，00:21:56Z）**6/6 全 0% 丢包**，rtt min 5.0–6.0ms / avg 10.1–17.4ms。
  中间态 `six-mid.txt`（00:11:27Z，只换完 CPE3 那一台）就已经 6/6 0%，和"锁死只发生在 CPE3"这个归因一致。
- 稳定性巡检 `soak-271.sh` 240s / 11 轮：每轮四台断言「自己的客户前缀不在 `state.json` 账本里」且
  「zebra 里它不是 `Known via "kernel"`」，11×4 全 OK，`injected/reported/withheld` 三轮以上保持同一组数字。

#### 八、全 lab 判据同一口径：FAIL 21 → 8，剩下的 8 条是同一个根

`vm-verify.sh` 换件前跑过一次基线（`/tmp/verify-270.log`，2026-11-05T19:03Z，#270 收口那一轮）：
**PASS=372 FAIL=21 WARN=0**。换件 + 四台重启 sr-frr 之后同一口径（`/tmp/p271/verify-after271.txt`，
2026-11-06T00:34Z）：**PASS=384 FAIL=8 WARN=0**。逐条对齐（行号是各自日志里的）：

| 换件前的 FAIL | 换件后 | 说明 |
| --- | --- | --- |
| 405 cust12→172.16.3.1 100% 丢包 / 407 cust3→172.16.12.1 / 408 cust3→172.16.4.1 / 410 cust4→172.16.3.1 | **消失** | 就是第三节那四条方向，同窗口四台一条摘路都没有（不是缺陷⑥ 那种能解释的丢包） |
| 313 CPE3 本地 172.16.3.1 没指向 172.31.3.2 / 392 CPE3 不是 bgp 路由 / 393 CPE3 下一跳不是 172.31.3.2 | **消失** | CPE3 解锁死：客户 eBGP 那条真路径回来了 |
| 314 CPE3 本地 172.16.3.1 被压了标签 | **消失** | 本地前缀不再走隧道，回到 §6 的正确形状 |
| 249 CPE2 rib 不符 / 288 CPE2 10s 内 FIB 没收敛 / 352 CPE2 内核主表没这条 / 390 CPE2 远端在主表不是 K/lo | **消失** | CPE2 那个"幽灵 K 视图"补齐（第七节第二条） |
| 328 CPE4 路径里没有 mpls via 10.250.0.4 | **消失** | CPE4 的隧道路径装回来了 |
| 239 CPE1 rib 不符（盒里只有 4.1） | **换成同一条判据的另一种表现**：`local 不符（盒里=['172.16.12.1/32','172.16.3.1/32'] 台账=['172.16.12.1/32']）` | CPE1 把 cust3 的 loopback 当成本地前缀上报了。**这条不是闸门造成的**：换件前的快照 `before-CPE1.txt`（23:20Z，早于任何换件动作）里 `local` 就已经含 `172.16.3.1/32`，只是 19:03Z 那次跑在这条形状出现之前 |
| 259 CPE3 rib 不符 / 269 CPE4 rib 不符 / 274+275 CPE1 172.16.3.1 没压标签 / 342+343 CPE1 主表与 zebra 不是 kernel / 386 CPE1 远端在主表不是 K/lo（可能被回灌） | **原样还在**（编号 259/269/274/275/341/342/385） | 一字未变的 7 条 |

剩下的 8 条（含那条换了形式的）共同根因只有一个，而且脚本自己就把名字写在判据文案里了 ——
**"可能被回灌"**：任何一台把远端前缀注入成 K 路由，CPE1/CPE2 的 `redistribute kernel route-map SDWAN2CPE-vpn0`
（route-map 里没有任何 match ⇒ 全放）就把它推进客户 area0；area0 里的盒子再从 ospf 学回来，
配上 CPE1 的导入白名单 `172.16.0.0/16 le 32` 全放，于是 ① 本盒把它当"客户侧拥有"上报成自己的 local
（→ `local 不符`、sr-fib 按本地前缀处理 ⇒ 不再压标签 → §6 两条），② 注入方向又被闸门压住
（→ §7/§8 三条）。**这就是第九节第 3 条待拍的实测代价，#271 不掩盖它、也不靠改判据掩盖它。**

#### 九、闸门能力的边界（不夸大，逐条有名有姓）

1. **A1 只挡"所有权还看得见"的那一次。** 一旦 K 路由已经装进去了，被它顶掉的那条客户路径就不再发通知，
   可见集里没有它，闸门也就无从判断。实测形状就在 CPE1 换件后的读数里：`172.16.4.1` 的 zebra 行是
   `Known via "ospf", distance 110, metric 20` **但没有 best 标记**，内核里是同前缀的 `dev lo scope link`
   —— 注入那条赢，ospf 那条被压住，于是 `local_withheld` 里没有它。**所以已经锁死的盒子，换新二进制不会自己
   解开，需要一次优雅重启**（本次 lab 里就是这一枪；生产上等价于 sr-box-watch 补进程那一次，实测窗口见第六节）。
2. **闸门是"可见集"的函数，不是全知。** `import.go:374-377` 的注释点名的盲区：客户如果还用了别的来源
   （OSPF 之外又跑了 isis/static），没订阅到的来源在这里就是"没有"，注入照样会顶掉它。订阅集合必须点名客户
   实际用到的每一种来源，这一条不是代码能替 lab 兜住的，是配置口径。
3. **跨盒回灌仍然开着，这条待拍。** CPE1/CPE2 的 ospfd 里是 `redistribute kernel route-map SDWAN2CPE-vpn0`，
   而 `route-map SDWAN2CPE-vpn0 permit 10` **没有任何 match** ⇒ 全放。于是任何一台注入的 K 路由都会进客户
   area0、被别的盒子当"客户侧拥有"学回来 —— 这正是 `layers-after-CPE1` 里 `172.16.3.1` 的来源，也是 CPE1 重启
   settle 期间 `172.16.4.1/32` 一度被闸门压住的原因。闸门把"自我遮蔽"挡住了，但"我的注入变成别人的客户路由"
   这条策略（导出侧要不要在回灌时过滤、过滤规则归谁配）**没有拍板，本轮没动**。
4. **`make test-frr` 现在测不到东西。** `test/scripts/test-frr-export.sh` 建立在已删除的 ZAPI/1250 导出模型上，
   配置用的是 `vrfs/distance/metric` 旧 schema；`cmd/sr-frr/main.go:56-72` 对旧 schema 的处理是"告警并把两个方向
   都置空"（绝不按旧值继续跑），所以这份脚本的断言在新码下必全灭。**本轮不改它、也不用它当判据** —— 
   是删还是按 K 路由模型重写，等 owner 拍。

#### 十、lab 完整性缺口（只登记，不动手）

CPE2 的 VPP 进程里 `sr_wg_plugin.so` 是 `9d611883f874…`，现产物是 `9267d28d8b76…`（2026-11-04 管理口被 VPP 接管后
Mac→盒子批量吞吐 <25KB/s，`bringup-mac.sh` 的按哈希增量推送当时没覆盖到运行时的插件）。对齐它需要一次 VPP 重启，
按 AGENTS.md §1 这是"必然中断"的操作 —— **待拍，本轮没做**。#271 的判据不受影响（换的是 sr-frr，不是插件）。

#### 十一、怎么重跑

```
docker exec -w /workspace/controller lp-ctrl go test -count=1 -v -run 'Gate|Withheld|Window|Frozen|Settled|Plan' ./internal/frr/
                                                  # 8 条单测：闸门/掩码匹配/链路窗口/首轮 Settled
bash test/lab/vmlab/bringup-mac.sh CPE3 B          # 只推产物 + phase B install（不重启任何东西）
ssh CPE3 'kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service'
cd test/lab/vmlab/exp-271
bash frr-forensic.sh CPE3                         # 同口径取证（换件前后各一次）
bash frr-layers.sh CPE3                           # 四层落点（内核/zebra/VPP FIB/sr-route + sr-frr 账本与状态）
bash window-271.sh CPE3 cust12 172.16.12.1 172.16.4.1   # 换件窗口的客户可见时长
bash soak-271.sh                                  # 11 轮稳定性巡检（自己的前缀不许进账本）
bash ../../vm-verify.sh                           # 全 lab 判据（换件前 21 条 FAIL，换件后 8 条，见第八节）
```

原始读数在本目录 `raw/`：`before-CPE{1..4}.txt` + `layers-before-CPE{1..4}.txt`（锁死态）、
`check-CPE3-1.txt`（解锁死那一枪之后 2 分钟）、`after-CPE{1..4}.txt` + `layers-after-CPE{1..4}.txt`、
`six-before/six-mid/six-after.txt`、`window-*.txt`、`soak-271.txt`、`verify-after271.txt`（PASS=384 FAIL=8）；
换件前的判据基线在 `/tmp/verify-270.log`（PASS=372 FAIL=21，#270 那一轮）。
逐发 ping 时间戳 `probe-*.raw`（5 份、每份 ~50KB）留在 Mac 侧 `/tmp/p271/`，可由 `window-271.sh` 重跑生成。

### 9.20 #273：同一条环路的定性更正 —— 注入闸门（A1）撤回，改由控制台那一行配置断环；三态 A/B/C 实测（2026-11-06 晚）

owner 的原文是两句，这一轮的范围完全按这两句划：

> 你把你上一轮的推送 rollback 吧。如果按你说的原因的话，那就是我说的路由环路造成，这个并不该像你现在这样处理。
> 应该由客户通过配置来解决：在 CPE1 和 CPE2 上设置只对 rr 播自己客户侧的路由。这个环路场景该由配置解决，
> 而不是代码解决。你先 rollback，然后按我说的把这个配置加上，然后再看是否路由就正常了。

> "一、你点名的那个现象，实际是两件事"我是指把这一条回滚。你修的其他缺陷不用。

所以：**只撤 A1（注入闸门），A2 链路窗口与 A3 读先于写 + 首轮冻结留着**；然后把他说的那条配置加上；
最后用同一把尺子（六方向 + 四层落点 + `vm-verify.sh`）看路由是否正常。lab 里依旧零手工补丁：
没有 `ip route`、没有 vtysh 改配置、没有重启任何 daemon。判据脚本与原始读数在
`test/lab/vmlab/exp-273/`（`raw/` 是三态的同口径快照）。

#### 一、场景：这条环路在哪两个方向上闭合

§9.19 第二节的杀伤链只讲了"一台盒子把自己顶掉"，那是环的内圈。外圈在这一轮才被量出来，它才是一句
"客户侧通告策略"能落的地方：

1. CPE3 把自己客户侧 eBGP 学到的 `172.16.3.1` 注入成内核 K 路由（distance 0）之后，客户那条真路径不再被
   zebra 装进 FIB，于是 zebra 不再对这个前缀发 redistribute 通知 —— CPE3 的可见集里没有它了，`reported` 掉到 0。
2. 同一时刻，CPE1 和 CPE2 的 ospfd 里跑着 `redistribute kernel route-map SDWAN2CPE-vpn0`，而这条 route-map
   `permit 10` **没有任何 match**（全放）。于是那台注入的 K 路由（或者任何一台注入的 K 路由）被推进客户的
   OSPF area0，area0 里的每台盒子都会学到它。
3. CPE1 的客户侧导入白名单当时是 `172.16.0.0/16 le 32`（也全放）。它从 area0 学到 `172.16.3.1`，
   按"ospf 来源 + 白名单命中"就把它当作**自己客户侧拥有的前缀**上报给 RR。
4. RR 反射回来给 CPE3 —— 而 CPE3 此刻"看不见"自己拥有 `172.16.3.1`（第 1 步），所以这条反射回来的路由在它
   眼里是正常远端路由，于是再注入一次 K 路由，回到第 1 步。

**环的闭合点是第 3 步**：CPE1/CPE2 把"别人站点的前缀"当"自己客户侧的"上报。这一步在产品里有一个明确的、
客户自己拥有的旋钮 —— 控制台「配置 > overlay > 路由泄露」里 `CPE-SDWAN`（`to_sdwan`）那一行的 prefix-list。
把它从 `172.16.0.0/16 le 32` 收成"只放本站自己客户侧的那条 `/32`"，第 3 步就断，第 4 步没有输入，
整条环停止供给。**代码不需要知道"这条前缀归谁"** —— 那是客户配置的语义，不是我们能在盒子里推出来的事实。
这就是 owner 说的"该由配置解决，而不是代码解决"，而 §9.19 的 A1 是在代码里替客户做了一个它没有依据做的判断
（只能靠"可见集"猜，猜不到就锁死，猜到了又把真实拓扑的形状藏进 `local_withheld` 里）。

#### 二、代码走到哪：撤掉了什么、留下了什么（行号是撤回后的现状）

撤回的部分：

- `internal/frr/plan.go`：`Plan` 结构回到只有 `Routes`（`plan.go:52-54`），`PlanFrom` 的签名回到三个入参
  `exportVRFs / routes / oif`（`plan.go:72`）。原来那段"本盒客户侧已经拥有的前缀一条都不写进内核"的规则
  （§9.19 第四节引用的 `plan.go:61-76`、`plan.go:121-128`）连同 `withheld` 切片一起删除；现在的函数注释里
  明确写了这一条**不在这里判断**，理由是 RIB 里既可能是远端广告、也可能是本盒自己那条被反射回来的，
  "把它挑出来不注入不是这一层的职责"，并指回本节。
- `internal/frr/import.go`：`OwnedByCustomer()` 整段删除（原 `import.go:368-392`）。留下的是
  `linkRecovery = 30 * time.Second`（`import.go:32`）、`BeginLinkWindow()`（`import.go:268`）、
  `Settled()`（`import.go:294`）、`visibleLocked()`（`import.go:346`）和 `Report(rules, exported)`
  （`import.go:372`）—— 其中 `Report` 的防环扣减（`LoopDropped`，`import.go:400`）一字未动，
  它挡的是"我这一轮正在导出的前缀不要又当成客户侧的上报出去"，是本盒自己两个方向之间的账，与跨盒环路无关。
- `cmd/sr-frr/main.go`：`exportState` 回到 `mu/dp/ex/lastSig/skipSig/plan/skipped/frozen/frozenHeld/lastErr`
  十个字段（`main.go:205-218`，原来的 `withheld` 计数删掉）；`reconcile` 的参数回到
  `(ctx, hc, cfg, statusURL, stateFile, frozen)`（`main.go:276`）；`/status`（`main.go:1054`）不再有
  `local_withheld` 这一项，其余字段（`config_hash / export_vrfs / import_rules / injected / skipped /
  export_frozen / export_frozen_held / reported / dropped_loop / dropped_unknown_vrf / subscriptions / pid`）
  原样。轮询里 A3 的判据仍在原位：`frozen := len(rc.rules) > 0 && !imp.Settled()`（`main.go:824`），
  紧接着 `exp.reconcile(..., frozen)`（`main.go:825`）。
- 包注释（`main.go:14-26`）从"三道闸门"改成"两道闸门"，并新写一段把这条定性记录下来：K 路由 distance 0 +
  客户侧 `redistribute kernel` = 路由环路，**环由客户侧通告策略断，不在注入侧偷偷少注入**。
- `internal/frr/plan_test.go`、`internal/frr/import_gate_test.go`：随闸门删掉两条单测（原
  `TestPlanFromWithholdsOwnedPrefix`、`TestOwnedByCustomer`），三处 `PlanFrom` 调用改回三个入参。
  现在这一族剩 6 条，实测全绿：链路窗口 2 条（`SurvivesSessionDrop`、`ClosesAtDeadline`）、
  首轮 `Settled` 1 条、`PlanFrom` 3 条（`TestPlanFrom` / `EmptyExport` / `Rejects`）。
- 口径脚本跟着改：`exp-271/frr-status-line.py` 不再打印 `withheld` 列；`exp-271/soak-271.sh` 顶部写明
  「账本里出现自己的前缀 = 控制台 CPE-SDWAN 那一行播了别人站点的前缀，改配置不改码」—— 断言本身没降级，
  它现在测的是配置有没有配对。

**没有动**的部分：A2 链路事件乐观窗口、A3 读先于写 + 首轮冻结、`/status` 的 `export_frozen*`、
（这三项随后在 #274 整族撤回，见 §9.21；"读先于写"的次序作为 D 留着）
控制口的 `PUT /config` 热更语义、`vm-verify.sh` 的任何判据、控制台任何页面。构建与测试在同一口径下复跑：
容器 `lp-ctrl`（go1.22.5）里 `go build ./...` + `go vet ./internal/frr ./cmd/sr-frr` + `go test ./internal/frr`
全部通过。

#### 三、为什么这样分：闸门撤掉后必须能"原样复现"，否则配置那一枪证明不了什么

这不是"代码没用所以删掉"，是一次对照实验，缺一半都不成立：

- 如果只加配置、不先撤闸门，那么"配置加上了、路由正常了"可能一直是闸门在兜着，说明不了配置的因果。
- 如果撤了闸门、加了配置，但中间没有一次"撤了闸门且配置照旧 ⇒ 环路原样复发"的读数，同样说明不了因果。

所以按三态走，每态都用同一份读数（`exp-273/box-layers.sh` 的四层 + §9.7 六方向）：

| 态 | 盒子里的码 | lab 的 `to_sdwan` 白名单 | 六方向 | 关键读数 |
| --- | --- | --- | --- | --- |
| **A**（§9.19 交付态） | A1+A2+A3，`8b15abd5…` | 全放 `172.16.0.0/16 le 32` | 6/6 0% | CPE1 `local_withheld=["172.16.3.1/32"]`、`reported=2`；CPE3 `local_withheld=["172.16.3.1/32"]`（`raw/A-before-swap.txt`） |
| **B** 只撤 A1 | A2+A3，`9f28db6c…` | 同 A，一字未改 | **4/6 100% 丢包** | CPE3 `injected=2 reported=0`，内核里 `172.16.3.1 dev lo scope link`、zebra `Known via "kernel" distance 0 best` 且 `show ip route summary` 里 **bgp 一栏为 0**；CPE1 `reported=2`、`local` 里混进 `172.16.3.1/32`（`raw/B-layers.txt`、`raw/six-B.txt`） |
| **C** 撤 A1 + 那条配置 | 同 B | node1/node2 收成只 `172.16.12.1/32`（node3/node4 仍是全放，没碰） | **6/6 0%** | 四台 `injected=2 reported=1`；每台 `local` 只有本站前缀；CPE3 内核 `172.16.3.1 nhid 981 via 172.31.3.2 dev lcp-x0 proto bgp metric 20`（`raw/C-final-layers.txt`） |

A→B 是同一条锁死链原样复现（丢包的方向、`reported=0`、zebra 里没有 bgp 路由，和 §9.19 第三节逐项一致），
B→C 码一字未改、只动配置、pid 全程不变（见第五节）⇒ 环路归配置的因果是被这两步夹出来的，不是"看起来正常了"。

#### 四、症状：B 态的读数就是这条环路的指纹

`raw/B-layers.txt`（2026-11-06T15:16–15:18Z，四台刚从 A 换成 B 的产物、配置一个字节没改）：

- 六方向 `raw/six-B.txt`（15:16:29Z）：`cust12→172.16.3.1`、`cust3→172.16.12.1`、`cust3→172.16.4.1`、
  `cust4→172.16.3.1` 四条 **100% 丢包**；只有 `cust12→172.16.4.1` 与 `cust4→172.16.12.1` 通（0%，rtt avg
  12.3 / 10.6ms）。这四条和 §9.19 第三节那四条完全同集合。
- CPE3：`reported=0`（本盒客户侧拥有的一条都没报上去）、账本里是 `172.16.4.1/32` 与 `172.16.12.1/32`，
  而内核 main 表里有三条 `dev lo scope link` 的 K 路由（`172.16.3.1`、`172.16.4.1`、`172.16.12.1`），
  zebra 对 `172.16.3.1` 的答复是 `Known via "kernel", distance 0, metric 0, best`、下一跳 `directly connected, lo`，
  `show ip route summary` 的四个来源是 kernel 4 / connected 2 / local 2，**bgp 一栏是空的** —— 客户 eBGP 那条路径
  在这台盒子的 RIB 里根本不存在，所以它既不会被上报，也不会被 redistribute。
- CPE1：`reported=2`、`local = [172.16.12.1/32, 172.16.3.1/32]`、`dropped_own_route=204` —— 它把 cust3 的
  loopback 认成了"自己客户侧拥有"，这正是第一节第 3 步。A 态里这条是被 `local_withheld` 挡掉的；闸门一撤，
  它立刻回到环里当供给方。
- **一条没归因的分歧（登记，不掩盖）**：B 态 CPE3 内核里 `172.16.3.1` 那条 K 路由**不在 sr-frr 的账本里**
  （`injected=2`、`state.json` 只有 4.1 与 12.1）。C 态同一台盒子的账本与内核是一致的（2 条对 2 条）。
  这条"账本比内核少一条"和 §9.19 第八节 CPE2 的"幽灵 K 视图"是同一族（一个方向是内核有、账本没有，
  另一个是 zebra 有、内核没有），本轮没有定位到代码。

#### 五、要动什么：只动那一行配置，而且必须走产品路径

`exp-273/apply-config.sh` 做的四步，全部是产品自己的入口，没有一步是登录盒子改文件：

1. `POST /api/prefix-lists` 建一条只含 `172.16.12.1/32` 的 prefix-list（名字"本站客户前缀"）。
2. `PUT /api/leak/rows` 把 node1、node2 的 `direction=to_sdwan`（控制台「路由泄露」的 **CPE-SDWAN** 那一行）
   指向它；node3/node4 的 `to_sdwan` **没碰**（它们本来就是 `bgp` 来源、播自己站点的 loopback，不在环的供给方位置上）。
3. `POST /api/leak/republish {prefix_list_id, apply:false}` 预览：两台都 `will_publish`，没有 blocking 检查。
4. 同接口 `apply:true` 真发布：`blocked=false`，`node1 published version=10`、`node2 published version=6`。
   之后是 agent 推送 → sr-apply 第 5 段写 `/etc/sr-frr.json` → sr-frr `PUT /config`。

生效证据（AGENTS.md §1 第 2 条：以状态对比判定，不看退出码）：

- 改前 lab 的口径是四台的 `to_sdwan` 全指向同一条 prefix-list `pl2`「客户 loopback」=
  `172.16.0.0/16 le 32`（`referencing_devices: [1,2,3,4]`），落盘读回来就是第四节 B 态那四行
  `allow=[{"prefix": "172.16.0.0/16", "le": 32}]`。
- 改后 node1/node2 落盘变成 `allow=[{"prefix":"172.16.12.1/32"}]`，`import_rules` 每台仍是
  `prefixes:1`（条目数没变，变的是条目内容 —— 只看条数会漏掉这一枪，所以判据读的是内容）；
  node3/node4 保持 `172.16.0.0/16 le 32` 原样。
- 四台 sr-frr 的 pid 在 B（15:18Z）与 C（15:39Z）两次读数里逐台相同：352167 / 299468 / 366430 / 102828
  —— **配置生效没有重启任何进程**；`/status` 的 `config_hash` 在这一枪之后是 `426e2bf8e7c8`
  （第六节配置窗口用它做了两次来回核对）。

CPE3 的自愈是这一枪的落点，而且是**在没有重启任何进程的情况下**发生的：15:39:10Z 读数里它的 pid 还是
B 态那一个（366430），内核 `172.16.3.1` 已经是 `via 172.31.3.2 dev lcp-x0 proto bgp metric 20`，
zebra 里那条的 `Last update 00:20:25 ago` —— 往前推正好是发布发生的那一刻（15:19Z），此后 20 分钟没有再抖。

> **这一条修正 §9.19 第九节第 1 条的外延。** 那条说"已经锁死的盒子，换新二进制不会自己解开，需要一次优雅
> 重启"——在"断环靠代码"的路线下它成立，因为闸门只能看可见集，而可见集已经瞎了。配置路线不需要看清：
> CPE1 停止上报之后，RR 把 `172.16.3.1` 从 CPE3 的 RIB 里摘掉，CPE3 的这一轮对账发现计划里不再有它，
> 于是撤掉自己注入的那条 K 路由，内核一撤，zebra 立刻重新装上客户的 eBGP 路径并恢复通知 —— **锁死的环
> 是从外部断供的，不需要盒子恢复视力，也不需要重启**。

收口读数（`raw/C-final-layers.txt`、`raw/six-C.txt`、`raw/C-reset2-six.txt`）：四台 `injected=2`、
`reported=1`、`subscriptions` 分别是 `vpn0/type7@0`（CPE1/2）与 `vpn0/type10@0`（CPE3/4）、
`export_frozen=false`、`last_error=""`；每台 `local` 只含本站前缀；六方向 6/6 0%（rtt min 4.4–6.3ms /
avg 9.3–16.1ms）。全 lab 判据 `vm-verify.sh`（15:44Z）：**PASS=392 FAIL=0 WARN=0** —— 对照 §9.19 第八节
换件前 21 条 FAIL、换件后 8 条 FAIL：**那 8 条不是被代码修掉的，是被这一行配置断掉的**，而且比 A1 闸门多清掉了
CPE1 `local 不符` 那条（闸门路线下它只是换了表现形式）。稳定性：`soak-271.sh` 口径改过之后 240s / 11 轮 × 4 台
= 44 条断言全 OK，`injected/reported` 三轮以上同一组数字。

#### 六、两个窗口的实测（AGENTS.md §1.4 要求点名必然中断）

这一轮只有两枪会碰到运行中的东西，都逐发量了（`timeout 1 ping` 每秒一发，链路通时约 32 发/秒，
只有一条路由挂着时才降到 1 发/秒 —— 所以"连续 FAIL 一片"是硬断流，不是采样噪声）：

**换件那一枪**（gzip+scp → sha256 核对 → `install -m 0755` → `kill -TERM $(pgrep sr-frr); sleep 2;
systemctl start sr-box-watch.service`；VPP / FRR / sr-agent / sr-route / sr-fib 的 pid 全程不变）：

| 盒 | 探测方向 | 总探测 | FAIL | 断流 |
| --- | --- | --- | --- | --- |
| CPE1 | cust12 `-I 172.16.12.1` → 172.16.4.1 | 2744 | **0** | — |
| CPE2 | cust12 → 172.16.3.1 | 2624 | **0** | — |
| CPE3 | cust3 `-I 172.16.3.1` → 172.16.12.1 | 231 | **65** | 3.3s + 63.8s（**探测结束时仍未闭合**） |
| CPE4 | cust4 `-I 172.16.4.1` → 172.16.12.1 | 2575 | 3 | 最长 2.0s |

CPE3 那一格**不能读成"换件把客户流量打断了 63.8s"**。它是 B 态那条环路正在复现：探测开始前（15:13Z）这台
已经因为上一台（CPE1）撤闸门而失去了自己的 eBGP 路径，SIGTERM 只是让这条已经断掉的方向被记进窗口。
CPE4 那 3 发（2.0s 后恢复）是 sr-frr 自己撤 K 路由再落回的正常窗口。**这一枪在 C 态是必要的一次**（要把
A1 从盒子里拿掉），不是配置路径的要求 —— 第五节那次配置生效一个进程都没碰。

**配置热更那一枪**（改 prefix-list 条目 + `republish apply:true`）：

| 标签 | 动作 | 总探测 | FAIL | `config_hash` | pid |
| --- | --- | --- | --- | --- | --- |
| 加条目 | 白名单里加一条 `192.0.2.0/30`（无害前缀） | 2699 | **0** | `426e2bf8e7c8 → 3f8351920e3d` | 两台都不变（352167 / 299468） |
| 回退 | 收回到只 `172.16.12.1/32`（这就是收口态） | 2711 | **0** | `3f8351920e3d → 426e2bf8e7c8` | 同上 |

**配置这一枪的客户可见时长实测为 0**，两个方向（加与收）都是。发布版本号 `node1 10→11`、`node2 6→7`。

#### 七、尾巴（登记，不掩盖）

1. **RESET 后一次 10% 未归因**：窗口探测 0 FAIL 结束（15:34:48Z）之后约 15s，`raw/C-final-six.txt`
   （15:35:03Z）里 `cust12→172.16.3.1` 与 `cust12→172.16.4.1` 各 10%（50 发里丢 5，rtt avg 10.5 / 12.4ms）；
   90s 后 `raw/C-reset2-six.txt` 6/6 0%，随后单方向手工 50 发复测也 0%。它不像配置那一枪（75s 连续探测 0 FAIL），
   更像 0.2s 间隔连打撞上缺陷⑥（#262：亚秒抖动被 BFD 150ms 判定 + sr-fib 1s 轮询放大成 ≥1s 黑洞）或宿主调度。
   **本轮没有取证**：下一轮要定位，请在同一秒同时抓 sr-fib 摘路事件、BFD 翻转与 `sr_wg` 探针计数。
2. **跨盒回灌（#272）没动**：CPE1/CPE2 的 ospfd 仍是 `redistribute kernel route-map SDWAN2CPE-vpn0`，
   该 route-map 无 match ⇒ 全放。C 态干净的原因是每台只播本站前缀，别盒学回来的都不是"自己的"，
   所有权不会被吃掉。**但这不是被修的，是被配置绕开的**：任何客户在 `to_sdwan` 那一行播了别人站点的前缀，
   B 态原样重现，而且现在没有任何代码会兜它（这是定性的代价，明确接受）。要不要在导出侧也收、
   规则归谁配，仍待拍。
3. **产品页面上的口径**：环路既然归配置，控制台「路由泄露」页就得让客户看得见"这一行播了什么"。
   现有页面已经显示 `to_sdwan` 行指向哪个 prefix-list 与条目内容，本轮没改页面；
   若要加"这一行的白名单覆盖了别的站点"这类提示，属于新需求，等他点。
4. `make test-frr`（`test/scripts/test-frr-export.sh`）仍是建立在已删除的 ZAPI/1250 导出模型上，本轮不改也不用它当判据（§9.19 第九节第 4 条原样有效）。
5. CPE2 的 VPP 插件哈希落后（`9d611883…` vs 现产物 `9267d28d…`）本轮没碰，对齐它要重启 VPP，属于必然中断，待拍。

#### 八、怎么重跑

```
docker exec -w /workspace/controller lp-ctrl go test -count=1 -v -run 'Gate|Window|Frozen|Settled|Plan' ./internal/frr/
                                                # 6 条单测（A1 的两条随闸门删除）
WORK=/tmp/p273 BIN=…/sr-frr.gz bash exp-273/push-binary.sh            # 只推产物 + install，不重启
WORK=/tmp/p273 bash exp-273/window-swap.sh CPE1 cust12 172.16.12.1 172.16.4.1
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash exp-273/apply-config.sh        # 态 C：那一行配置（走产品路径）
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash exp-273/window-cfg.sh 加条目 192.0.2.0/30
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash exp-273/window-cfg.sh 回退 RESET   # 收口必须跑这一步
for B in CPE1 CPE2 CPE3 CPE4; do bash exp-273/box-layers.sh $B; done
TAG=final bash exp-273/six.sh
bash exp-271/soak-271.sh                                                # 口径已改为"配置对不对"，不是"闸门有没有兜住"
bash test/lab/vmlab/vm-verify.sh                                        # 392 PASS / 0 FAIL / 0 WARN
```

原始读数在 `test/lab/vmlab/exp-273/raw/`：`A-before-swap.txt`（A 态四层）、`B-layers.txt` + `six-B.txt`
（撤闸门后原样复现）、`C-final-layers.txt` + `six-C.txt` / `C-final-six.txt` / `C-reset2-six.txt`（配置断环后）、
`swap-CPE{1..4}.txt`（换件窗口逐发）、`window-cfg-加条目.txt` / `window-cfg-回退.txt`（配置窗口逐发）、
`soak-C.txt`（11 轮 44 条断言）、`verify-C-final-summary.txt`（PASS=392 FAIL=0 WARN=0，全量 30KB 输出留在
Mac 侧 `/tmp/p273/verify-C-final.txt`）。逐发 ping 时间戳 `probe-*.raw`（每份 ~50KB）不入库，可由上面两个
window 脚本重跑生成；会话 cookie 同样不入库。

### 9.21 #274：乐观窗口这一族全部撤回（A+B+C+F），第 3 条硬规则改口径；重连一枪的改前/改后实测（2026-11-07 凌晨）

owner 的两句原文，这一轮的范围照这两句划（第三句是他的根因定性，属于 #275）：

> 嗯，这是真正症结，把这个改成检查活动的下一跳隧道，而不只是看配置。所以你之前加的那个"乐观窗口"，
> 请 rollback 掉。我记得你不止一处加了这个乐观窗口。

> 咱们的路由协议是对收敛速度有要求的，链路状态的变化，要几乎实时的反应在路由上，否则就会有瞬时黑洞。
> 所以乐观窗口我理解都不应该存在的。

追问后拍定三点：**回滚范围 = A+B+C+F 全清**（不是只删新加的那一个）；**AGENTS.md 第 3 条同步改口径**；
可达性粒度 = **严格 fail-closed，无 BFD 信息即不可达**（这条属于 #275，本轮不动代码）。

#### 一、这一枪打在哪条接缝上：从"客户前缀是谁的"倒推

`local` 这个读数是 sr-frr 每轮 PUT 给 sr-route 的那份"本盒客户侧拥有的前缀集"，它经 RR 反射给全网，
也是 sr-fib 判定"这条前缀是本地的、不要装隧道"的依据。所以它短一段，全网就跟着撤一段；它晚一拍回来，
远端就晚一拍装回隧道 —— 这一段就是瞬时黑洞的入口之一。链路走到底有四站：

1. sr-fib 用 netlink 把远端前缀注成内核 K 路由（distance 0）→ zebra `redistribute kernel` 把它通知给 sr-frr；
2. sr-frr 的 `Importer` 收到通知，按（VPN，来源，白名单，防环）算出上报集合 → `PUT /local-prefixes`；
3. 与 zebra 的连接断掉（`restart frr`、VPP 重启拆 lcp/vpp0、FRR 崩）时，第 1 站的通知会先撤销再重建；
4. **窗口一族全部活在第 3 站和第 2 站之间**：它们的共同主张是"这暂时看不见，多半不是真的没有，先把旧答案报着"。

owner 的判据把这句话否掉了：路由协议对收敛速度的要求高于对这段空窗的容忍 —— 靠一条已经死掉的会话
继续宣称前缀，本身就是黑洞，不是防黑洞。

#### 二、代码走到哪：删了什么（行号是删完之后的现状）

| 编号 | 原名 | 它在哪一拍维持旧值 | 现在 |
| --- | --- | --- | --- |
| A | `BeginResync` + `resyncQuiet=500ms` / `resyncDeadline=10s` | sr-frr 与 zebra 重连后把 `stale ∪ pending` 当可见集 | 删。换成 `Importer.Reset()`（`internal/frr/import.go:214`）：重连即清空，新连接的回说什么就是什么 |
| B | `BeginLinkWindow` + `linkRecovery=30s` | 内核链路 down/up（VPP 重启拆 lcp/vpp0）那 30s 维持旧集合 | 删。`watchLinkEvents`（`cmd/sr-frr/main.go:368`）不再开窗，只剩 D-11 那段 `touch` 重放 |
| C | `Settled()` + `export_frozen` / `export_frozen_held` | 首轮对账未落地时注入"只撤不加" | 删（它是 A/B 的闸门，没有窗口就没有"落地"可言）；`/status` 少这两个字段 |
| F | `onceWindow` / `onceSettle` | `-once` 退出前自己再等 300ms | 删，`-once` 变成"跑一轮就退出" |
| D | 轮询里"先读完并上报客户侧，再决定注入"的次序 | —— 这是读写次序，不是窗口 | **留** |
| E | `linkSettle=300ms` 去抖 + `touch` K 路由重放 | —— 这是 D-11（链路删除后 zebra 丢 K 视图、导出静默失效）的修法 | **留** |

具体改动：`Importer` 结构回到 `mu / routes / vpnOf / unknown / badVRF / Logf` 六个字段（`Now`、`stale`、
`pending`、`resync`、`seen`、`started`、`last`、`deadline`、`reason`、`settled` 全没），`Handle`
（`import.go:162`）照直写 `routes`，`Report`（`import.go:241`）第一件事是 `snapshotLocked()` 拿当前这份；
`ensureImport` 里 `imp.BeginResync()` → `imp.Reset()`（`main.go:562`，位置在订阅之前）；
`poll`（`main.go:726`）返回值从 `bool` 变回 `()`，`imp.Tick` 与 `frozen` 判定整段删除。
包注释（`main.go` 开头）把"两道闸门"改成"这条接缝上没有任何等一拍的窗口"，并把代价（重连到回放落地之间
会短报一次）写在 `Importer.Reset` 的注释里，不藏在散文里。
单测随窗口删掉 7 条：`import_gate_test.go` 整文件 `git rm`（链路窗口 2 条 + `Settled` 1 条 + 假时钟
`fakeClock` / `newClockImporter`），`import_test.go` 从 16 条减到 12 条 —— 4 条重连窗口
（`KeepsOldSetUntilReplayQuiet` / `Deadline` / `WaitsForFirstEvent` / `NestedResyncKeepsOriginalStale`）
换成一条"重连即重置"的判据（重置后必须报 0，回放到达后必须报回 1）。
容器 `lp-ctrl`（go1.22.5，`/workspace/controller`）里 `go build ./...`、`go vet ./...`、`go test ./...` 全绿。

#### 三、AGENTS.md 第 3 条换了主张（这是口径变更，不是补注）

旧第 3 条写的是"控制面重连不得让数据面抖动 ⇒ 乐观窗口：重连后先维持旧集合，等订阅回放静默 500ms
（或 10s 上限）后再一次性对账"。现在它反过来：

> **不许用"乐观窗口"掩盖状态缺口。** …… sr-frr 重连即重置本地前缀集，新连接的回说什么就是什么。
> **Why:** 路由协议对收敛速度有硬要求 —— 链路状态必须几乎实时反映到路由上，否则就是瞬时黑洞。
> **How to apply:** 数据面不抖靠**可达性信号本身**（下一跳隧道/BFD 的真实状态），不靠延迟对账。

顺带把 §9.19 第四节 A2 里那句"实测会话 15s 才回来"撤下来：它没有原始读数支撑，本轮实测的 refill 是
12~19s，但那是"zebra 重新收录 K 路由 + 1s 轮询"的合计时长，不是客户 BGP 会话的恢复时长，两件事不能混着引。

#### 四、实测：改前 9.4s / 改后 12.6s 与 15.6s —— 而这一枪里窗口一秒都没买到

故障注入 `systemctl restart frr`（CPE1；这是被测故障，不是配置生效路径，AGENTS.md 第 1 条禁的是用重启让配置生效）。
口径：盒子本地 200ms 一发读 sr-route 的 `local` 条数与 sr-frr 的 `reported`（基线 1 条 = `172.16.12.1/32`），
同时 cust12 用自己的 loopback 秒级探测一条当前通的方向。脚本 `test/lab/vmlab/exp-274/reconnect-window.sh`。

| 标签 | 产物 | `local` 掉 0 | 回到 1 | 偏离时长 | 探测 FAIL |
| --- | --- | --- | --- | --- | --- |
| `before` | `9f28db6c268aa12c`（A+B+C 都在） | +2.38s | +12.2s | 9.4s（22/160 发） | 2/2037，1 段 1.0s |
| `after` | `c8e731ddcbca7bd6`（窗口全删） | +2.57s | +15.2s | 12.6s（30/160 发） | 4/1983，2.1s + 一发 |
| `after-in` | 同上，反向（cust4 → 172.16.12.1） | +2.77s | +18.3s | 15.6s（37/161 发） | 2/2006，1 段 1.0s |

三行读下来的结论不是"代价多大"，是**改前也掉**，而且掉的时刻两边一样（+2.4~2.8s）：

- `before` 那一发的两个读数是 `local=1 frr(reported=0 zebra_connected=True)`，下一发 `local` 才变 0。
  窗口还开着时可见集是 `stale ∪ pending` = 1 条，`reported` 不可能是 0 —— 唯一能同时解释这两个读数的是
  **窗口已经按 500ms 安静关上，而那份回放的答复就是 0 条**：zebra 刚起来时自己的 RIB 还没重新收录 sr-fib
  那两条 K 路由，`redistribute kernel` 的第一份就是空的。连上约 +1.4s、+0.5s 安静、+2.0s 落地，
  与 +2.03s 这发对上（误差在一个采样间隔内）。
- 于是窗口在这个场景里买到的东西是 0：`restart frr` 后的空窗长度由 **refill** 决定（zebra 重新收录 K 路由
  之后、经 sr-frr 1s 轮询上报），改前改后都是十几秒，9.4 → 12.6/15.6s 的差是 refill 时刻的抖动，不是窗口的保护。
- 改前那份日志自己就把话说完了：`重连后 10s 没收到任何回放，按空集合收尾（撤掉旧前缀 1 条）` ——
  走到上限的结局同样是"报空"。改后是 `重连，清空本地前缀集（1 条），等新连接的回放重建`。

这同时说明：本轮删掉的东西**没有让这一枪变坏**，而 owner 要的那条真正能改变形状的路径在 #275
（可达性输入）与 #276（事件驱动收敛，把上面那 12~19s 的 refill 压成瞬时）。

#### 五、生效姿势：换件全程零冷启动

四台 `/usr/local/bin/sr-frr` 从 `9f28db6c268aa12c` 换成 `c8e731ddcbca7bd6`，每台四步，与 §9.19 第五节同一口径
（比它多一步：`push-binary.sh` 只把产物写到 `/root/vmlab/artifacts/linux/`，**不装**到 `/usr/local/bin`，
漏掉第 2 步就等于重启了个旧二进制 —— 本轮第一台 CPE2 就是这么"换了没换"的，`sha256sum` 一查才知道）：

1. gzip + `sha256sum` 推到 `/root/vmlab/artifacts/linux/`（`exp-273/push-binary.sh`，只落盘不重启）；
2. `install -m 0755 /root/vmlab/artifacts/linux/sr-frr /usr/local/bin/sr-frr`，再 `sha256sum` 核一遍；
3. `kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service` —— SIGTERM 走产品自己的收尾
   （撤掉自己下发的 K 路由），watch 是 Type=oneshot + `KillMode=process`，只补缺失进程；
4. 判据用状态对比不用退出码：`stat -Lc %i /proc/<pid>/exe == stat -c %i /usr/local/bin/sr-frr`、
   四台 `local` 条数与 `injected/reported` 与换件前一致、§9.7 六方向 0%、`vm-verify.sh` 全绿。

VPP / sr-agent / sr-route / sr-fib 全程 pid 不变，实测见下表"其它守护进程启动时间"一列 —— 四台的这几个进程
仍是 11-04~11-06 起的那一批，今天只有 sr-frr 换过。FRR 只在第四节那一枪里被 `restart frr` 打过（那是被测故障），
换件本身不碰它。

**换件这一枪的客户可见中断（AGENTS.md 第 4 条要求点名 + 给时长）**：`window-swap.sh` 探测机用自己的 loopback
1 秒一发打 75 秒（链路通时约 32 发/秒），第 6 秒 SIGTERM 该盒 sr-frr。改后（`c8e731ddcbca7bd6`，本轮）与
改前（`9f28db6c268aa12c`，§9.19/§9.20 那轮同一脚本同一方向）对照：

| 被换的盒 | 探测方向（源→目的） | 改前 总发/FAIL/最长断流 | 改后 总发/FAIL/最长断流 |
| --- | --- | --- | --- |
| CPE1 | cust12 `172.16.12.1` → `172.16.4.1` | 2744 / 0 / — | 2331 / 1 / 单发（0.0s） |
| CPE2 | cust12 `172.16.12.1` → `172.16.3.1` | 2624 / 0 / — | 2324 / 0 / 无 |
| CPE2 | cust12 `172.16.12.1` → `172.16.4.1` | 未跑该方向 | 2231 / 2 / 1.0s |
| CPE3 | cust3 `172.16.3.1` → `172.16.12.1` | 231 / 65 / 63.8s（**读数不可用**，见下） | 2252 / 3 / 1.0s |
| CPE4 | cust4 `172.16.4.1` → `172.16.12.1` | 2575 / 3 / 2.0s | 2335 / 3 / 2.0s |

- CPE3 改前那一行**不能当"改前更差"的证据**：75 秒的窗口只有 231 发（通的时候约 32 发/秒 → 全程 ~2400 发），
  说明那一发探测期间链路本身就大面积在下，63.8s 的"断流段"里混着别的故障。它只能说明那一轮的基线不干净。
- 可用读数的一致形状是**最长 1~2 秒、且只在被换的那台的方向上**：SIGTERM 到 watch 补回 sr-frr（`sleep 2` + 启动）
  之间，这台盒撤掉自己下发的 K 路由；对端站点是 CPE1+CPE2 双播，所以 cust12 侧多数方向被另一台接住（0~1 发）。
- 换件后四台状态对比（判据不是退出码）：`local=1`、sr-frr `reported=1 injected=2 zebra_connected=True
  last_error=''`、`ip route show table all` 的 K 路由各 18 条、`stat -Lc %i /proc/<pid>/exe == stat -c %i
  /usr/local/bin/sr-frr` 全部 yes、`/status` 里 `export_frozen*` 两个字段已消失。
- 同一批换件后复跑：`exp-273/six.sh` 六方向 0% 丢包（rtt avg 11.0~14.1ms），`vm-verify.sh` **392 PASS / 0 FAIL / 0 WARN**
  （与 §9.20 那次干净参照同一条数）。

#### 六、台的局限与尾巴（登记，不掩盖）

1. **反向探测体现不出空窗**：`172.16.12.1/32` 由同站点的 CPE1 与 CPE2 都在播，CPE1 那份掉下去时 CPE2 还在，
   所以 cust4→172.16.12.1 只看到 1.0s 断流。这条方向**不能当"代价小"的证据**，只有本盒 `local` 计数算数。
2. **没有量 VPP 重启那一枪**：那是窗口原本真正为它设计的一拍（链路 down → zebra 在连接还活着的时候逐条
   withdraw）。它要把 sr-route / sr-fib / BFD 一起重来，属于 #275 的现场判据；本轮判据只到"重连即重置"这条接缝。
3. **refill 12~19s 本身是缺口**，归 #276：`zebra 重新收录 K 路由` + `sr-frr 1s 轮询` 两串延时，删窗口不解决它。
4. 盒子与 Mac 之间没对时（CPE1 的钟比 UTC 快 ~10.7 分钟），所以跨机只报**时长**，注入时刻取自盒子自己的
   `date +%s.%3N`。
5. §9.19 第四节 A2/A3 两段留着当历史，但代码已经不在，本节是它们的作废声明；`docs/loop-test-report.md`
   里 P-3（sr-fib "保留上一份已装 FIB"）已按同一定案标注否决。

#### 七、怎么重跑

```
docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'
WORK=/tmp/p274 BIN=~/srvmlab/out-274/sr-frr.gz bash exp-273/push-binary.sh CPE1 CPE2 CPE3 CPE4
ssh <盒> 'install -m 0755 /root/vmlab/artifacts/linux/sr-frr /usr/local/bin/sr-frr'   # push 脚本只落盘不装，漏了这步等于换了个寂寞
WORK=/tmp/p274 TAG=before bash exp-274/reconnect-window.sh CPE1 cust12 172.16.12.1 172.16.4.1
WORK=/tmp/p274 TAG=after  bash exp-274/reconnect-window.sh CPE1 cust12 172.16.12.1 172.16.4.1
WORK=/tmp/p274 bash exp-273/window-swap.sh <盒> cust12 172.16.12.1 172.16.4.1     # 逐台换件中断时长
TAG=274 bash exp-273/six.sh
bash test/lab/vmlab/vm-verify.sh
```

原始读数在 Mac 侧 `/tmp/p274/`（`samples-*.txt` 逐发采样、`probe-*.txt` 逐发 ping、`window-*.txt` 汇总），
不入库，重跑即生成。

---

### 9.22 #277：三只「在跑的产物 ≠ 盘上的产物」收口 —— 盒内复制 `/proc/<pid>/exe`，全程不重启（2026-11-07 上午）

§9.19/§9.20/§9.21 之后，`agent-handover.md` §3 记着当前环境里最贵的一处：缺陷⑦（#268）和缺陷④（#260）
**代码修了、进程里也跑了修复件，但盘上躺的还是修复前那一份**。它坏在静默 —— 任何一次重启或开机重放就退回旧行为：
`sr-fib` 退回"VPP 重启后不重连 api.sock、隧道表只在内存 ⇒ 客户流量永久黑洞"，
`sr-agent` 退回"VPP 重启后不重建 wg 隧道且仍报 `ok=true`"。这一节把它关掉，并记下三条实测方法。

#### 一、为什么这一步不需要重启（这是判据，不是安慰）

盘上那份文件只是"下一次 exec 的内容"，**换文件本身不改任何在跑的进程**：内核按 inode 记账，
`mv`（同盘 rename）之后旧进程继续持有旧 inode，行为一个字节都不变。所以这一轮：

- 不 kill、不 restart、不 `systemctl start sr-box-watch.service`、不动任何配置发布；
- 判"有没有打扰客户流量"用状态对比：四台八个 daemon 的 pid + 隧道口条数逐一与改前相同，六方向丢包 6/6 `0%`，`vm-verify.sh` 392/0/0。

顺手排掉一个会把我装的东西吃回去的怀疑：`sr-box-watch.timer` 每 60s 跑一轮 `/usr/local/lib/sr-box/start.sh up`。
`grep -nE "install |cp -|mv |artifacts" start.sh` **零命中** —— 那份清单只 `nohup` 起进程、从不装二进制，
而它的幂等前提"在跑的进程绝不重启"是 11-05 实测过的（§9.15 那条 KillMode 的账）。

#### 二、换文件的正确姿势（`install` 直接覆写在用件上是错的）

四台踩点：

1. **覆写正在执行的二进制吃 `ETXTBSY`**；被 mmap 的插件更糟 —— 就地写会把在跑的数据面写坏。
   所以必须"同目录临时件 + `mv -f`"，`mv` 是 rename，旧 inode 由在跑的进程继续持有。
2. **临时件要和目标同一个文件系统**，否则 `mv` 退化成"复制 + unlink"，原子性没了。
3. VPP 那侧 `grep sr_wg_plugin /proc/<vpp pid>/maps` 在 CPE1 上显示 `... sr_wg_plugin.so (deleted)`：
   在跑的插件**已经没有路径名指向它**。这条同时否掉了"`/usr/local/bin` 的哈希能代表在跑的那一代"这个想法。
4. CPE1 的 `/usr/local/bin/sr-agent` 是 `32a1b26a…`（来历不明，Mac 产物树里无匹配）。改前它四处无副本，
   现在按旧件留在 `/usr/local/bin/sr-agent.prerun-20261107`。

#### 三、Mac↔盒子这条链路只有 7KB/s —— 所以别用传输来补齐

实测两路：`gzip -c sr-agent | scp` 的 5,443,837 字节走了 10 分钟没走完（4.18MB 时我停了它）；
换成**盒子主动** `curl http://192.168.1.4:8799/` 拉 247,432 字节用了 36.29s = 6,817 B/s，两个方向一样慢。
四件 18MB 走传输要两个多小时，而传输根本没必要：

```bash
pid=$(pgrep -x sr-agent | head -1)
sha256sum /proc/$pid/exe | cut -c1-16          # 必须是 d2ad8519648c031f，否则中断不装
cp /proc/$pid/exe /usr/local/bin/sr-agent.swap # 盒内复制：字节必然等于在跑那一份
chmod 0755 /usr/local/bin/sr-agent.swap && mv -f /usr/local/bin/sr-agent.swap /usr/local/bin/sr-agent
```

`/proc/<pid>/exe` 跟着内核打开的那个 inode，所以复制出来的就是"正在跑的这一段码"；
再用期望哈希当闸门（那两个期望值同时是 Mac 产物树里核对过的 `d2ad8519…` / `bab523ff…`），
一次网络都不走。插件是唯一必须推的（247KB，盒上没有那份字节）。脚本：`test/lab/vmlab/exp-277/align-disk.sh`。

#### 四、结果与留底

| 台 | `sr-agent` 盘上 | `sr-fib` 盘上 | 插件 盘上 | 在跑的（四台，全程未变） |
| --- | --- | --- | --- | --- |
| CPE1 | `32a1b26a…` → `d2ad8519…` | `41c04d57…` → `bab523ff…` | 本来就 `9267d28d…` | agent 241361 / fib 241436 / route 239318 / frr 406663 / vpp 239183 |
| CPE2 | `f2e14612…` → `d2ad8519…` | `41c04d57…` → `bab523ff…` | `9d611883…` → `9267d28d…` | agent 182805 / fib 182829 / route 122631 / frr 345863 / vpp 182261 |
| CPE3 | 同 CPE2 | 同 CPE2 | 同 CPE2 | agent 190643 / fib 248325 / route 34038 / frr 410809 / vpp 256513 |
| CPE4 | 同 CPE2 | 同 CPE2 | 同 CPE2 | agent 2520 / fib 2553 / route 2535 / frr 151532 / vpp 910 |

六方向改前 02:34Z、改后 03:0xZ 都是 6/6 `0%` 丢包（RTT 均值改前 10.4–13.5ms / 改后 9.0–14.8ms，
改后有两路最大值 72/76ms —— VM 台宿主噪声，判据是丢包，见 §9.16）；`vm-verify.sh` **392/0/0**。
被换下来的旧件全在盒上 `*.prerun-20261107`，**顺带补上了台账里"`sr-fib` 没有回滚底"那条**：
`41c04d57…` 那份 11-04 代现在四台都在盒上（#268 那次是 `mv` 换的，盒上没留 `.prev`，Mac 上也没有）。

三条要说白的局限：① "重启不再退回"是**推导**（字节相同 ⇒ exec 出来的就是同一份码），我没为验证而重启任何进程；
② 插件只对齐了"下一次重启 VPP 载入哪一份"，在跑的数据面未变，判在跑哪一代仍只能用功能探针；
③ `sr-route` 仍在 11-04 那一代的内容（run 与 disk 都是），抬它要换进程 ⇒ 撞缺陷⑧（#269 未拍），**这是有意留的分叉**。

#### 五、怎么重跑

```bash
bash test/lab/vmlab/exp-277/align-disk.sh CPE1 CPE2 CPE3 CPE4   # 逐件校哈希；不符就那台中断不装
# 只读复核（想改前先跑这个）：
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do pid=$(pgrep -x $x|head -1);
  printf "%-9s run=%s disk=%s\n" "$x" "$(sha256sum /proc/$pid/exe|cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x|cut -c1-16)"; done'
TAG=277 bash test/lab/vmlab/exp-273/six.sh
bash test/lab/vmlab/vm-verify.sh
```

原始读数在 Mac 侧 `/tmp/p277/`（`baseline.txt` 改前 run/disk+inode+pid+wg 口、`six-before.txt`/`six-after.txt`、`verify-after.txt`），
不入库，重跑即生成。

