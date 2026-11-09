# 接手这份项目：agent → agent 的把手

这份文件是给**另一个 AI agent** 写的，不是给人看的介绍。它假设你从来没碰过这个仓库，
目标是你在 30 分钟内能独立动手，而且不踩我们已经付过学费的那几处。

写在最前面的一条：本文所有数字都是 **2026-11-07 由前一轮 agent 实测**，每条后面都跟了复核命令。
**动手前先复核，不要相信这份文件本身** —— 它会过期，尤其是「哪一份代码在跑」那一节。
如果复核结果和这里不一致，以你测到的为准，并且把这份文件改掉（这是它的自我维护约定）。

---

## 0. 读这份文件的顺序

1. 先读仓库根的 `AGENTS.md`（50 行，全是硬规则，会在每个 agent 会话里自动加载）。它和本文冲突时，**AGENTS.md 赢**。
2. 再读本文 §2（现在这一刻的真实状态）和 §3（在跑的产物 vs 盘上的产物：11-06 那三只雷已在 11-07 收口，判据仍在这节）—— 这两节决定你第一条命令该敲什么。
3. 要改代码：读 §5（唯一合法的生效路径）+ §9（未决队列，owner 已经点名的活）。
4. 要下结论/交差：读 §6（读数口径）+ §7（测量陷阱）+ §8（验证基线）。

owner 的三条协作习惯，违反任何一条都会被退回，这里先说清楚：

- **先分析、后方案。** 他说过的原话："你不要每次都动不动就说解决方案是什么，因为你的分析得到我的认同前，
  你想的解决方案毫无意义。" 分析没被他点头之前，不要提交方案，更不要提交代码。
- **"修复"只指修软件缺陷，不指让 lab 看起来正常。** 不许用手工 `ip route`、手改 vtysh/FRR 配置、
  重启进程这类办法把现象压掉（他的原话："也就是说不允许手动方法修复"）。
- **controller 的每个增量先出方案、他点头后再写码；一次只推进一个缺陷。**

### 0.1 上车先跑这四条（全只读，跑完你就知道本文哪一句过期了）

```bash
git log --oneline -5 && git status --short                       # HEAD 与别人留下的散工
VBoxManage list runningvms                                        # 应是 7 台：CPE1-4 cust12 cust3 cust4
ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep # 控制面在不在 Mac 上（§2.1）
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do pid=$(pgrep -x $x|head -1);
  printf "%-9s run=%s disk=%s\n" "$x" "$(sha256sum /proc/$pid/exe|cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x|cut -c1-16)"; done'            # §3 那张表还成立吗
```

第四条如果哪一行 `run != disk`，说明有人换过件没装盘（或有第二 agent 在铺产物）—— 先问 owner 再动手，
别直接重启去"对齐"。第二条如果 7 台不全在跑，你的现场是别人的半成品，先声明再说。
`vm-verify.sh`（约 2 分钟）和 `exp-273/six.sh` 同样随时可跑，但跑的时候确认没人在注入故障（§10）。

---

## 1. 这个项目是什么

一套自研 SD-WAN，一个项目两半：

- **数据面**：VPP 插件 `sr_wg`（SR-over-WireGuard：隧道里再叠源路由头），C 写的，在 `vpp-plugin/`。
- **控制面**：Go 写的控制器 + 盒内一组 daemon（`sr-agent` / `sr-apply` / `sr-route` / `sr-fib` / `sr-frr`），
  在 `controller/`。

我们的"BGP"那部分**不是 FRR 的 bgpd**，是自己写的 `sr-route`（Loc-RIB 形状照抄 GoBGP）+ `sr-rr` 反射器，
走 mTLS，协议 v3 的条目是 `{DeviceID, VRFID, Prefix, RTs}`；下一跳在**本地**从隧道表解析。
盒内 FRR 只负责客户侧那一半（zebra/bgpd/ospfd），`sr-frr` 是它和 overlay 之间的双向搬运工。
定案口径：**与真 BGP 只差两条**（信道用 mTLS、RIB→VPP 自己装表），其余照抄。

一台 CPE 盒里从上到下的接缝（改路由相关代码前必须能在脑子里跑出这张图）：

```
sr-route Loc-RIB
   └─ frr.PlanFrom ──► 内核 K 路由(netlink, distance 0) ──► zebra redistribute kernel ──► 客户侧（正向）
   ▲
   └─ ZAPI 订阅(按 VPN/来源) ──► to_sdwan 白名单减去已导出 ──► PUT /local-prefixes（反向）
                        ▲
                        └── sr-route 开机 GET /local-prefixes（反向的"新客户端回说"，2026-11-09 起）
```

反向这条接缝上，**2026-11-07 起没有任何"等一拍"的窗口**：`Importer.Reset()`
（[controller/internal/frr/import.go:214](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/frr/import.go#L214)）
在与 zebra 重连的那一刻清空本地前缀集，新连接的回说什么就是什么。
**2026-11-09 起另一头同理**：反向上报是变更驱动的（集合没变就一个请求都不发），所以 `sr-route` 单独重启
原本会把本盒前缀对全网抹掉直到下一次变动 —— 现在它一起来就经 sr-frr 的控制口拉一份全量
（[controller/cmd/sr-route/pull.go:61](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-route/pull.go#L61)），
sr-frr 应答即降级自己的 `lastSeen`（`localReporter.demote()`），拉取与推送的先后次序不需要版本号也不需要对齐时钟。
曾经有过四个窗口（A `BeginResync` / B `BeginLinkWindow` / C `Settled` / F `onceWindow`），
整族在增量 #274 撤回，理由写在 AGENTS.md 第 3 条。**不要把这些窗口重新加回来**，
包括看起来无害的"重连后先维持旧值 500ms"。

---

## 2. 现在这一刻的真实状态（2026-11-07 实测）

### 2.1 控制面跑在 Mac 上

| 件 | pid | 参数要点 |
| --- | --- | --- |
| `sr-ctrl` | 40606（已跑 1d10h） | `-data /Users/louis/srvmlab/ctrl/data -listen 0.0.0.0:8444 -tls-san 192.168.1.4 -console 127.0.0.1:8088 -stun 192.168.1.4:3478,3479 -hb 5` |
| `sr-rr` ×2 | 40616 / 40618 | `-config /Users/louis/srvmlab/state/rr-a.json`、`rr-b.json`，监听 18446+19446 / 18447+19447 |

复核：`ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep` 和
`lsof -nP -iTCP -sTCP:LISTEN | grep -E "sr-ctrl|sr-rr"`。
**注意**：那条 `ps` 里能看到运营方令牌的值 —— 那个值**不许进任何文件、日志、提交信息**（AGENTS.md §3）。
需要它的时候从 `/Users/louis/srvmlab/ctrl/data/console-token` 读，或者当场 `ps` 看一眼。

Mac 的 LAN 地址是 `192.168.1.4`，控制台在 `127.0.0.1:8088`（只对 loopback 开）。

### 2.2 被测现场是 VirtualBox 那 7 台

```
VBoxManage list runningvms   →   CPE1 CPE2 CPE3 CPE4 cust12 cust3 cust4
```

- CPE1–4 = 192.168.1.101–104（Ubuntu 24.04.5 / kernel 6.8.0-147 / 2 vCPU / 2048MB，
  盒内 VPP v26.06-release + FRR 10.7.1）；
- **进盒子只能走 `~/.ssh/config` 里的别名**（`ssh CPE1`、`ssh cust12`、`ssh cust3`、`ssh cust4`），
  直连 IP 会被拒；
- 真机 `192.168.1.100` 是别人的现场，**只读**，不在任何改动范围内。

### 2.3 旧 docker lab 还在跑 —— 这是个陷阱，先认清对象

`docker ps` 现在能看到 `lp-cpe1..4`、`lp-rr`、`lp-cust3/4/12`、`lp-term3/4/12`（全部 Up 4 天，2026-11-07 复核）。
**被测现场不是这些容器**，它们是 docker 时代遗留的 lab（S8 那步"用户关 docker"至今没做完，任务 #244）。
现在 docker 只剩一个用途：

- `lp-ctrl` = **编译/单测容器**，go1.22.5，bind mount 三个目录
  `controller` / `test` / `vpp-plugin` → `/workspace/{controller,test,vpp-plugin}`。
  任何 go 构建、`go vet`、`go test` 都在这里跑，Mac 上没有 C 工具链也没有 go。

所以：**改代码在 `lp-ctrl`，量数据在 VirtualBox**。别把结论从容器里搬出来。

### 2.4 验证基线（今天复测过）

| 判据 | 今天的读数 | 命令 |
| --- | --- | --- |
| VM 台整体验证 | **PASS=392 FAIL=0 WARN=0** | `bash test/lab/vmlab/vm-verify.sh`（约 2 分钟，只读） |
| Go 单测 | 全绿，仓库内 `func Test` **749 条**（`internal/frr` 41、`internal/route` 72、`internal/underlay` 58） | `docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'` |
| §9.7 六方向客户流量 | 0% 丢包 | `TAG=xx bash test/lab/vmlab/exp-273/six.sh` |

`vm-verify.sh` 结尾自己会写明本轮没覆盖什么（独立终端机、站点 12 的 VRRP），那两行要照抄进你的结论里，
不要说成"全绿所以全通"。

### 2.5 盒内进程与 systemd 单元的真实形状

四台的 `sr-box.service` 现在都是 `failed`（CPE3 是 `inactive`），**这不是新故障，别去追**。
它是开机清单那一次 `start.sh up` 以 exit 1 收尾留下的状态，日志里原因写得很清楚：

```
tunnels     0 条：VPP 里的 wg 口是 sr-agent -tunnels 从**控制器**取期望值建的，
              磁盘上没有那份表。控制器不可达时开机就是这个形状。
```

隧道表磁盘恢复不了是已知缺口（G-2，方案在 [docs/g2-tunnel-table-derivation-plan.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/g2-tunnel-table-derivation-plan.md)）。
真正让 daemon 常驻的是 `sr-box-watch.service.timer`（60s 一轮，Type=oneshot，`KillMode=process`）。
`KillMode=process` 是硬要求：默认的控制组 kill 每 60s 把 nohup 起来的 sr-route/sr-frr SIGKILL 掉
（2026-11-05 23:08 实测 sr-route 在 72s 采样里只活着 47.0–49.1s）。复核：
`ssh CPE1 'systemctl status sr-box --no-pager | tail -12'`。

---

## 3. 曾经踩到的三只雷：在跑的产物 ≠ 盘上的产物（2026-11-07 已收口，任务 #277）

11-06 夜实测出来的结论是：**`sr-fib` 和 `sr-agent` 一重启或一开机重放，就会退回没有缺陷修复的那一份**，
而且退回去是静默的。这一条当前已经不成立 —— 四台的盘上都换成了"正在跑的那一份"，
下面的表是 2026-11-07 当场复测的。但**这一节别跳过**：它记的是"怎么发现、怎么证明、以及还剩哪一处不一致"，
下次你自己铺件时要用同一套判据。

| 件 | 在跑（`/proc/<pid>/exe`） | 盘上（`/usr/local/bin`） | 收口前盘上是哪一份 |
| --- | --- | --- | --- |
| `sr-route` | `5789c2c0f753529b` | 同值 ✅ | 本来就同值（11-04 代，见下面"仍不一致的那一处"） |
| `sr-frr` | `c8e731ddcbca7bd6` | 同值 ✅ | 本来就同值（#274 那份） |
| `sr-fib` | `bab523ff3809f57b`（含缺陷⑦ 修复） | 同值 ✅ | `41c04d57fced05c0`（修复前）—— 重启即退回"VPP 重启后不重连 API、隧道表只在内存 ⇒ 客户流量永久黑洞"（缺陷⑦，#268） |
| `sr-agent` | `d2ad8519648c031f`（含缺陷④ 修复） | 同值 ✅ | CPE2/3/4 `f2e1461226db2dcd`（修复前）、CPE1 `32a1b26ada097227`（来历不明）—— 重启即退回"VPP 重启后不重建 wg 隧道且仍报 `ok=true`"（缺陷④，#260） |
| `sr_wg_plugin.so` | —— 见下面第 4 条 | CPE1–4 全 `9267d28d8b76fb54` ✅ | CPE2/3/4 `9d611883f8741999`（11-04 代） |

**收口用的方法（一句话）：盒内直接复制 `/proc/<pid>/exe` 到新 inode 再 `mv` 换上，不传输、不换进程。**
脚本 `test/lab/vmlab/exp-277/align-disk.sh`，逐台判据与前后哈希在 `exp-277/README.md` 和 §9.22。
之所以能这么做：`/proc/<pid>/exe` 跟着内核打开的那个 inode，复制出来的字节按定义就是在跑的那一份；
再用期望哈希（`d2ad8519…`/`bab523ff…`，都是 Mac 产物树里核对过的那两件）当闸门，不符就中断不装。
装完 `run==disk` 四台四件全一致，所以**重启/开机重放不再退回旧件** —— 这条是推导出来的（字节相同 ⇒ exec 出来的就是同一份码），
我们没有为此重启任何进程，也没有实测过重启后的行为。

复核（一条命令出全表，四台各跑一次）：

```bash
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do
  pid=$(pgrep -x $x | head -1)
  printf "%-9s run=%s disk=%s\n" "$x" \
    "$(sha256sum /proc/$pid/exe | cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x | cut -c1-16)"
done'
```

**2026-11-08 复测（#276 切片 B 落地后，四台全表）** —— 上面那张 11-07 表现在只对 CPE2/3/4 成立：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | `sr-agent` | 收敛触发 |
| --- | --- | --- | --- | --- | --- |
| CPE1 | `26044f78f7969935` run==盘 | `a9f1bc8c8fd49016` run==盘（**事件版，但没带 B3**） | `c8e731ddcbca7bd6` run==盘 | `d2ad8519648c031f` run==盘 | `/watch` 事件推，`poll_sec` 只当重挂间隔；**可达性变化不触发收敛（B3 缺陷臂）** |
| CPE2/3/4 | `5789c2c0f753529b` run==盘 | `bab523ff3809f57b` run==盘（轮询版） | `c8e731dd…` run==盘 | `d2ad8519…` run==盘 | 每秒一轮 |

**2026-11-09 复测（B3 + B4 落地后，四台全表，同一套命令）** —— 上面两张表只有 CPE3 又变了：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | 收敛触发 |
| --- | --- | --- | --- | --- |
| CPE3 | `1161534d5c91db40` run==盘（**B4 版**：开机向 sr-frr 拉一份 local 全量） | `c213a6caf1b2aabf` run==盘（**B3 版**：订阅本机 VPP 的 BFD 状态事件） | `626b126a331f65e0` run==盘（**B4 版**：控制口新增 `GET /local-prefixes`） | `/watch` 事件推 **+ BFD 状态翻转** + 本机重启后自己回拉 local |

上面那段"CPE1 仍是…CPE2/CPE4 仍是轮询版"只描述到 2026-11-09 上午；#284 抬完盒之后由下面这张表取代。

**2026-11-09 复测（#284 抬盒后，四台全表，同一套命令）** —— 眼下现场就是这一张：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | 收敛触发 |
| --- | --- | --- | --- | --- |
| CPE1 | `1161534d5c91db40` run==盘（**B4 版**） | `a9f1bc8c8fd49016` run==盘（**事件版、没带 B3 —— 有意留的缺陷臂**） | `626b126a331f65e0` run==盘（**B4 版**） | `/watch` 事件推 + 本机重启后自己回拉 local；**可达性变化不触发收敛（B3 缺陷臂）** |
| CPE2 | `1161534d5c91db40` run==盘 | `c213a6caf1b2aabf` run==盘（**B3 版**） | `626b126a331f65e0` run==盘 | `/watch` 事件推 **+ BFD 状态翻转** + local 回拉 |
| CPE3 | 同上 | 同上 | 同上 | 同上 |
| CPE4 | 同上 | 同上 | 同上 | 同上 |

四台十二件 run==盘，**没有静默退回**（2026-11-09 实测，逐台 `sha256sum /proc/<pid>/exe` 对
`/usr/local/bin/<name>`）。抬盒的逐枪实测、流量归属表和回滚底在
[test/lab/vmlab/exp-284/README.md](test/lab/vmlab/exp-284/README.md)；用的仍是
`test/lab/vmlab/exp-276/install276.sh`（只装盘）+ `swap276.sh`（SIGTERM + 产品自己的开机清单补回 +
客户侧 75 秒探测）。**换 sr-fib 的那一枪之后必须立刻 `vm-fib-sync.sh <节点>` 补隧道表**（G-2，见下）。

**顺带抓到的一处真实不一致（缺口 G-2，不是本次引入）**：CPE3 的 sr-fib 运行时隧道表 3 条
（`curl -s http://127.0.0.1:19011/status` 的 `tunnels`），盘上 `/etc/sdwan/fib.json` 写的是 **`tunnels: 0`**
（CPE2/3/4 全 0，CPE1 是本轮我手工 `PUT /config` 恢复过的 2 条）。这张"下一跳 ↔ 本机 wg 口名 ↔ 颜色"
的推导表产品里没人落盘，进程一起来就靠上游重推 —— 详见台账 G-2。**后果**：这种盒子上如果 sr-fib 单独重启、
而 RR/agent 那条重推链正好没走到，就是缺陷⑦ 那一族的形状。**别把 `fib.json` 里的 `tunnels` 当成现场事实。**

**这个空窗的实际后果量出来了，比"黑洞"轻（2026-11-09 实测，CPE2 与 CPE4 各一次）**：新 sr-fib 起来到
`vm-fib-sync.sh` 补上隧道表之间，空窗分别 **124s / 79s**（这段里 `/status` 的 `tunnels=0`，所有远端路径按
「下一跳不在隧道表里」跳过）。两次窗口里的收敛行都是 `routes+0/-0` —— **它不拆已经装好的 VPP 行，只是不再收敛**。
所以形状是"从这一刻起路由变化全部失效"，不是"立刻断客户流量"：这两段窗口内客户探测 **0 FAIL**，
VPP 里 `mpls via` 条数也没变（没变是孤儿行还在，别把它当 B3 生效）。判定隧道表补没补上只认
`vm-fib-sync.sh` 那行 `隧道 N 条 … converged=True` 且没有 `警告:`。

**仍不一致的那唯一一处（有意留着，不是漏）：** CPE1 的 `sr-fib` 还是 `a9f1bc8c8fd49016` —— 事件版但**没带 B3**，
它就是 B3 那一族的缺陷臂，抬到 `c213a6ca` 会把手上唯一的对照臂杀掉，所以按 #284 的方案**不动它**。
其余十一件全部落在 HEAD 那一代。回滚底在盒上按日期存着（2026-11-09 从 `/proc/<pid>/exe` 抽出来的，
哈希与当时的运行份逐一对上）：CPE1 `sr-frr.prerun-20261109`=`c8e731dd…`、`sr-route.prerun-20261109`=`26044f78…`、
`sr-fib.prerun-20261108`=`bab523ff…`；CPE4 `sr-frr/sr-route/sr-fib` 的 `.prerun-20261109` =
`c8e731dd…` / `5789c2c0…` / `bab523ff…`。

换进程这一步本身在缺陷⑧（开机清单与 watch 撞在同一秒各起一份，已用 `flock` 修掉，见 §7 表里 #269）之后
只剩"重启会抖一下 RR 会话"这个形状，不再是并发抢起。抬盒九枪的客户侧探测合计 **25282 发**（CPE2 三枪 8606 / CPE4 四枪 11076 / CPE1 两枪 5600），
其中 **CPE4/CPE2 的 sr-route 与 sr-fib 枪出现 0 或 1~2 发孤立 FAIL（无连续段）**。要说清两件事：

1. **`sr-frr` 这一枪本来就有窗。** 它重启 = 上报连接重连 = 按 AGENTS.md §1.3 立刻重置 local 前缀集，
   重置到它重新上报之间，本盒客户前缀在别盒就是 0 条 `mpls via`。所以"抬 sr-frr"的中断上界
   = 重启环 + 重新上报 + RR 反射 + 别盒装表。**同一个盒同一个方向实测两次**（CPE4，`cust12→172.16.4.1`）：
   对老份重启那一枪连续 2 发 / **1.03s**，对新份那一枪 1 发孤立 FAIL。各只有 1 个样本，
   **不足以下"新版更短"的结论**，能下的结论是"这一枪窗宽 0~1s、动之前必须按 §1.4 点名"。
   dmesg 无 link 事件，排除了链路抖。
2. **CPE1 那两枪 0 FAIL 不代表没窗 —— 那条探测对"CPE1 自己的 local 前缀被摘"不敏感。**
   站点 12 是双归的（CPE1 + CPE2 都报 `172.16.12.1/32`），CPE1 摘了 CPE2 那份还在，别盒不会掉路。
   要覆盖这一类故障必须测**单归**那一侧（站点 4 只有 CPE4 一条腿）。逐方向的敏感度表在
   `test/lab/vmlab/exp-284/README.md`。

修复件在 Mac 上的落点（都已核对哈希）：

- `sr-fib` 缺陷⑦ 那一份：`/Users/louis/srvmlab/artifacts/linux/sr-fib.20261106-bab523ff` = `bab523ff3809f57b`；
  **注意同目录里那个不带后缀的 `sr-fib` 是 `2e1ceee1cd4c849e`，是第三份，不是它**。
- `sr-agent` 在跑的那一份：`/Users/louis/srvmlab/artifacts/linux/sr-agent` = `d2ad8519648c031f`。
- 遥测插件：`/Users/louis/srvmlab/artifacts/plugins/sr_wg_plugin.so` = `9267d28d8b76fb54`（现在 = 四台盘上那份）。
- 一处**取不回**：四台在跑的 `sr-route`（`5789c2c0…`）在 Mac 的产物树里没有任何匹配文件，
  想恢复只能从源码重编一份，那**不会**得到同一个哈希（见 §4 第 2 条）。
- CPE1 那份来历不明的 `sr-agent`（`32a1b26a…`，9,697,139 字节）原先四处无副本，
  现在按被换下来的旧件保在盒上：`/usr/local/bin/sr-agent.prerun-20261107`。
- 被这次换下来的旧件全在盒上留底（`*.prerun-20261107`）：四台 `sr-fib` = `41c04d57…`、
  CPE2/3/4 `sr-agent` = `f2e14612…`、CPE2/3/4 插件 = `9d611883…`。
  **这顺带补上了台账里"`sr-fib` 没有回滚底"那条** —— `41c04d57…` 那份 11-04 代现在四台都在盒上。

---

## 4. 怎么判定"哪一份代码在跑"（这是方法，请当工具用）

我们踩过的三个误区，按顺序讲：

1. **`sha256sum /usr/local/bin/<x>` 不等于"在跑的"。** 换件是"先落盘、再 install、再换进程"，
   三个时刻任何一步没做，盘上和跑的就分叉。唯一权威的做法是
   `sha256sum /proc/<pid>/exe`（它跟着内核打开的那个 inode，哪怕文件已被删）。
2. **不能用"重新编译一份比哈希"来证明"跑的是这份源码"。** 实测：从今天的 HEAD 在 `lp-ctrl` 里
   `-trimpath CGO_ENABLED=0` 重编，`sr-route` 得到 `3668e20d…`、`sr-frr` 得到 `c7b6527b…`，
   而四台在跑的是 `5789c2c0…` / `c8e731dd…`。构建路径、buildinfo、VCS stamp 都会进哈希。
   **哈希只认"哪一份产物"，不认"哪一份源码"** —— 想证明后者，只能靠产物目录的命名/台账或 `-ldflags` 注版本（我们现在没注入）。
3. **VPP 插件盘上那份哈希不能用来判"正在跑的是哪一代"。** 实测四台盘上 `sr_wg_plugin.so` 的 mtime
   都比各自 VPP 启动时刻晚约 6 小时 —— 加载的是上一份文件。
   还有一条更硬的：`grep sr_wg_plugin /proc/<vpp pid>/maps` 在 CPE1 上显示
   `... sr_wg_plugin.so (deleted)` —— 在跑的那一代已经没有路径名指向它了，盘上那份是新 inode。
   字符串/符号比对也没用：盘上 `9d611883…` 与 `9267d28d…` 两份里 `sr-wg probe` 这个串都有（CPE2 上实测，命中各 1 次），
   而插件的命令/符号表是运行时才和 VPP 绑定的。**唯一可用的判据是功能探针本身**：
   `vppctl show sr-wg probe` 能回话、且两次采样（隔 2s）`echoed` 在涨，才说明"这一代在跑并且真在发探针"。
   想知道当前加载的是哪一份，只能拿 VPP 启动时刻去对那一时刻盘上文件的哈希（`stat -c %Y` + `/proc/<pid>` 的 etime，同一台机器上换算）。
   2026-11-07 之后四台盘上都是 `9267d28d…`，所以**下一次重启 VPP 载入的就是带探针这一代**；但"在跑的"仍要按上面这条实测，别拿盘上值推断。

4. **要把盘上补齐成"在跑的那一份"，别走网络。** 2026-11-07 实测 Mac↔盒子这条链路只有 **7KB/s 量级**
   （`gzip+scp` 5.4MB 走了 7 分钟没走完；换成盒子主动 `curl` 从 Mac 拉 247KB 也要 36 秒，速度一样）——
   18MB 的四件按这条路要两个多小时。而盒内 `cp /proc/<pid>/exe <同目录临时件>` 再 `mv` 换上，
   零传输、字节必然等于在跑那一份，期望哈希当闸门不符就不装。这条就是 #277 用的方法，脚本在 `test/lab/vmlab/exp-277/`。
   只有盒上本来没有那份字节的东西才需要走网络（比如插件：在跑的映射是 `(deleted)` inode，取不出来，只能推 247KB）。
   **需要走网络时也不要从 Mac 走：盒↔盒在管理 LAN 上是 226KB/s，比 Mac↔盒 快一个半量级**（2026-11-09 实测：
   8446056 字节的 `sr-frr` 从 CPE3 拉到 CPE4 用 37.3s）。形状 = 在已经有那份产物的盒上
   `mkdir /tmp/xfer && cp /usr/local/bin/<name> /tmp/xfer/ && timeout <N> python3 -m http.server 8099 --directory /tmp/xfer`
   （**不要加 `--bind <管理口 IP>`** —— 绑到 `vpp0` 那个地址会周期性 `OSError: [Errno 99]`，端口就废了），
   目标盒 `curl` 完必须 `sha256sum` 对一遍。**两个自己会咬人的坑**：`timeout` 到期会掐断正在服务的连接
   （curl 退 18、半份文件，所以超时要按总字节估，别按单件估）；`pkill -f "http.server 8099"` 会匹配到
   自己那条 ssh 命令行，把 ssh 一起打死（退 255）—— 记住 pid 用 `kill` 收口，别用 pkill。
   两条硬约束：**别覆写目标名**（覆写正在执行的二进制吃 `ETXTBSY`；覆写被 mmap 的插件会把在跑的数据面写坏），
   临时件要和目标**同一个文件系统**才保证 `mv` 是 rename。

顺带记两条正确的取证命令（都实测过）：

- 隧道口条数：`vppctl show interface | grep -cE '^wg'`（CPE1=2、CPE3/4=3）。
  `vppctl sr_wg_dump`、`vppctl show table`、`vppctl show wireguard interfaces` **都不是我们的命令**。
- **别给 `vppctl` 加 `-s /run/vpp/api.sock`。** `-s` 把 vppctl 切到二进制 API 口，CLI 命令发过去没有对应消息，
  于是**不回话也不报错地一直挂着**（2026-11-07 实测 CPE1：`vppctl -s /run/vpp/api.sock show interface` 和
  `… show sr-wg probe` 两条都零输出，靠 `timeout 15` 才收回来）。`vppctl <cli 命令>` 不加 `-s` 才是对的。
  挂掉的 vppctl 客户端会作为孤儿进程留在盒上（`pgrep -a vppctl` 能看到），不影响 VPP，但会污染下一次取证，
  用 `timeout N vppctl …` 包一层最省事。
  这条是我自己今天踩的：#277 收口后复测插件那轮，我第一次加了 `-s`，四个盒子的循环卡死在第一台。
- **引号不是问题**（更正先前写在这里的说法）。2026-11-07 在 CPE1 四种组合实测：
  `vppctl "show sr-wg probe"`、`vppctl show sr-wg probe`、`vppctl "show interface"`、`vppctl show interface`
  **全部正常回话**。被更正的那条出自 `86e4b2d`（昨天写把手时加的），`vmlab-rebuild-plan.md` 里查不到它对应的实测记录，
  今天在同一套 VM 盒上又复现不出来 ⇒ 按不成立删掉。今天这次卡死可以确定是 `-s` 造成的：同一条命令去掉 `-s` 立刻回话。
  插件在跑的判据是 `vppctl show sr-wg probe` 两次采样（隔 2s）`echoed` 在涨 —— 四台现在都涨
  （2 会话的 CPE1/2 约 +14/2s，3 会话的 CPE3/4 约 +20/2s）。

---

## 5. 改动的唯一合法路径

### 5.1 配置：走产品路径，一步都不能跳

```
控制台草稿 ──► 显式发布 ──► sr-agent 拉取/推送 ──► sr-apply 写第 5 段 ──► sr-frr PUT /config 热更
（127.0.0.1:8088）                                              /etc/sr-frr.json     /run/sr-frr/control.sock
```

FRR 配置一律 `frr-reload.py` 差量，VPP 走 CLI 增量，`sr-route`/`sr-frr`/`sr-fib` 只热更。
`sr-fib` 没有 unix 控制口，`/status` 与 `/config` 都挂在 HTTP `127.0.0.1:19011`
（[controller/cmd/sr-fib/main.go:532](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-fib/main.go#L532)
和 [:543](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-fib/main.go#L543)）；
`sr-frr` 才有 `/run/sr-frr/control.sock`。**生效路径里不许重启任何 daemon**（AGENTS.md §1.1），热更失败就如实报这次 apply 失败。

### 5.2 换二进制：四步，第二步最容易漏

```bash
# ① 推产物（只落盘，不装、不重启）
WORK=/tmp/pX bash test/lab/vmlab/exp-273/push-binary.sh CPE1 CPE2 CPE3 CPE4
# ② 安装到 /usr/local/bin —— push-binary.sh 不做这一步
ssh CPE1 'install -m 0755 /root/vmlab/artifacts/linux/sr-frr /usr/local/bin/sr-frr'
# ③ 换进程：SIGTERM 走产品自己的收尾（撤掉自己下发的 K 路由），由 watch 补回新进程
ssh CPE1 'kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service'
# ④ 判据用状态对比，不用退出码
ssh CPE1 'pid=$(pgrep sr-frr); [ "$(stat -Lc %i /proc/$pid/exe)" = "$(stat -c %i /usr/local/bin/sr-frr)" ]'
```

第 ② 步漏掉的后果我在今天真实犯过一次：`push-binary.sh` 之后直接换进程，重启起来的是**旧盘上那份**，
哈希没变 —— 等于换了个寂寞。这一条已经写进 §9.21 第五节和 `exp-274/README.md`。

第 ④ 步那条 inode 判据**只在"刚从这个名字起过进程"之后有意义**（它证明这个进程确实来自盘上这个文件）。
它不是"哪一代在跑"的通用判据：#277 之后四台的 `sr-agent`/`sr-fib` 就是 `run` 与 `disk` 哈希相同、
inode 不同（rename 换上去的，进程还持有旧 inode，所以 ②③ 之间没有重启）。
**判"哪一代"只认 `sha256sum /proc/<pid>/exe` 与产物台账对哈希**，inode 只用来回答"这个进程是不是从这个文件起的"。

换件是**必然中断**操作（SIGTERM 到 watch 补回之间该进程不干活），AGENTS.md §1.4 要求点名 + 给实测时长。
#274 那次四台 sr-frr 换件的实测在 [test/lab/vmlab/exp-274/README.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-274/README.md)
（客户可见中断最长 1.0–2.0s，按方向不同）。

**换完 sr-fib 必须立刻补一次隧道表**，否则这一臂作废（2026-11-09 实测，缺口 G-2 在这里咬人）：
sr-fib 的「下一跳 → 本机 wg 口 + 颜色」那张表只在内存（`PUT /config`），产品里没有任何进程维护它，
新进程一起来就是 `0 条隧道`，于是所有远端前缀按「下一跳不在隧道表里，该路径跳过」被跳过，
而 VPP 里的老行还在（`routes+0/-0`、`mpls via` 条数不变）—— 看着像"没坏"，其实收敛根本没发生。
补法只有一条合法路径：`bash test/lab/vmlab/vm-fib-sync.sh <node>`（数据来自控制台
`GET /api/underlay` + 盒内 `show interface addr` + `/etc/sdwan/route.json`，脚本不猜）。
判据是它自己打的那行：`隧道 N 条 … converged=True` 且**没有** `警告:`。
换 sr-route 那一枪必须**与 sr-frr 同批、且 sr-frr 在前**（B4 的部署硬规则，2026-11-09 实测：
老 sr-frr 没有 `GET /local-prefixes`，实测回 404，只抬 sr-route 会让它对着 404 每 2s 重试到永远，
本盒 local 前缀集照旧对全网消失）。B4 修好之后已经**不需要**再手工换 sr-frr 补前缀了 ——
新 sr-route 一起来就自己拉一份全量（CPE3 实测 `local` 在重启后 t+1s 就是 1，1899 发探测 0 FAIL）。

### 5.3 提交

```bash
git -c user.name='louis' -c user.email='louis@users.noreply.local' commit -m '...'
git push origin feat/vbond-control-plane
```

一次性身份，不动 git config；**只推 `feat/vbond-control-plane`，不推 master，不 force**
（master 历史里有密钥材料，那条线永远不推）。HEAD 随手会往前走，别信这里写的：现取
`git log --oneline -5`（本轮收口时依次是 `fe03988` vppctl 口径更正 → `57591f0` #277 产物收口 →
`86e4b2d` 这份把手入库 → `1dc4ab2` #274 窗口撤回）。
`.out/`、`controller/.out-*` 这类散工目录留在未跟踪状态。
每个验证过的增量都提交推送，并且**结尾给他一份能自己登录复核的把手**（账号/角色；口令值只在对话里给，不进仓库）。

---

## 6. 读数口径（下结论前先对齐字段）

| 读数 | 出自哪里 | 含义 |
| --- | --- | --- |
| `local` | `curl -s http://127.0.0.1:19001/status`（sr-route）的 `local` **数组**，长度即条数 | 本盒向全网宣称"我客户侧拥有"的前缀集，经 RR 反射给全网；sr-fib 用它判断"这条是本地前缀、不要装隧道" |
| `rib_routes` / `sessions[].connected/synced/routes` | 同上 | Loc-RIB 条数与两条 RR 会话的状态 |
| `dropped_own_route` / `dropped_no_path` / `dropped_rt_mismatch` | 同上 | 三类丢弃计数；回灌类问题先看 `dropped_own_route`（CPE1 现在 244） |
| `reported` / `zebra_connected` / `injected` / `skipped` / `import_rules` / `config_hash` | `curl -s --unix-socket /run/sr-frr/control.sock http://x/status` | sr-frr 这一轮实际报了多少、是否连着 zebra、注入了几条 |
| 内核 main 表 + zebra 视角 | `ip route show table main` / `vtysh -c 'show ip route'` | K 路由落地与否 |

`local=1 frr(reported=0 zebra_connected=true)` 这种组合是有意义的信号（说明是 zebra 那边空，不是链路没连上），
#274 那节就是靠它否掉了"窗口保护住了"的说法 —— 见 [docs/vmlab-rebuild-plan.md §9.21 第四节](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vmlab-rebuild-plan.md)。

一条永不混用的区分：**"前缀集掉空"的时长 ≠ "客户 BGP 会话恢复"的时长**。
旧的 12~19s refill 是"zebra 重新收录 K 路由 + sr-frr 轮询 + sr-fib 轮询"三段的合计，不是会话恢复时长
（文档里曾经混过一次，已改掉）。**2026-11-08 切片 A+B 之后这三段拆开重测**（同一台 CPE1、
注入 `systemctl restart frr`、两臂 sr-route 同一份产物）：摘路边 ≤45ms、装路边 201~755ms，
而"整条前缀不可用"仍是 5.33~6.66s 且**改前改后同量** —— 剩下的那 5s 是 zebra 收录 + 客户侧收敛的账，
不在 #276 范围。逐枪数字见 `test/lab/vmlab/exp-276/README.md` §三与§四。

`poll_sec` 现在的含义变了，别再按"轮询周期"理解：**代码默认仍是 2**（`cmd/sr-frr/main.go:105`），
lab 盘上 `/etc/sr-frr.json` 与 `/etc/sdwan/fib.json` 仍写 **1**（2026-11-08 复测四台都是 1），
但在 CPE1 上它只作为**断线重挂的间隔**用（`cmd/sr-fib/main.go:505-508`），不再是周期性收敛的节拍；
sr-frr 那一路改成变更驱动后，`poll_sec` 对它是兜底而非常态（切片 A，提交 a8083d2）。
CPE2/3/4 还是轮询版，那些盒上 `poll_sec=1` 依然是"每秒一轮"。

---

## 7. 测量陷阱（每一条都付过学费）

1. **盒子之间、盒子与 Mac 之间没有对时**（CPE1 的钟比 UTC 快约 10.7 分钟；2026-11-09 再量，
   被测盒与探测机之间互差 0.8~8.2 小时）。
   跨机只能比**时长**，不能比绝对时刻，文件 mtime 也不能横着比。注入时刻取盒子自己的 `date +%s.%3N`；
   判据锚点一律取**被测盒自己采到的那一次状态翻转**，别去对齐对面那台敲命令的时刻——
   8 小时的差会把 48ms 量成 480ms。
2. **Mac 的 BSD `date` 没有 `%N`**；`bash 3.2` 在大括号展开上有坑。跨机脚本别用它们。
3. **并发 `scp` 会静默截断文件**。产物推送一律串行，落地后 `sha256sum` 对一遍再算数。
4. **打流窗口放在管道子 shell 里会假绿**（"0ms 中断"就是这么来的）。计时要落在真正执行的那一侧。
5. **trace 只在收侧有意义**；转发到的包在收侧会被 punt，别把 punt 当丢包。
6. **`vppctl -s /run/vpp/api.sock <CLI 命令>` 不回话也不报错地一直挂着**（§4 有实测）；取证一律 `timeout 15 vppctl …`，
   跑完 `pgrep -a vppctl` 看一眼有没有留下的孤儿客户端。
7. **边跑边改被测脚本**会得到不可解释的数；脚本要么跑前定稿，要么重跑。
8. 原始读数不进仓库，落 `/tmp/<任务号>/`（`samples-*.txt` / `probe-*.txt` / `window-*.txt`），
   文档里只放汇总数字。
9. **Mac↔盒子这条链路只有 7KB/s 量级**（2026-11-07 实测，两个方向都是；§4 第 4 条有数字）。
   想"推一份产物过去再装"在这种链路上是几十分钟到几小时，而且中途断了会留下半截文件 ——
   补齐盘上那份要改成盒内 `cp /proc/<pid>/exe`（#277 的方法），只有盒上没有那份字节时才走网络。
10. **盒子上的 VPP 进程名是 `vpp_main`，不是 `vpp`** —— `pgrep -x vpp` 在这四台上返回空（今天实测），
    取 pid 用 `pgrep vpp_main`。`sha256sum /proc//exe` 这种拼出来的空 pid 会静默给你空哈希，
    看起来像"盘上没有"。
11. **`ssh -n` 配 heredoc 一声不响输出 0 行**（2026-11-09 踩过）：`-n` 把 stdin 换成 `/dev/null`，
    `<<'X'` 送不到远端，`bash -s` 就当没有输入直接结束。远端要读 heredoc 的那几条 ssh 一律不带 `-n`。
12. **引号形式的 heredoc 里本地变量不展开**，脚本会带着 `set -u` 在远端挂掉 ——
    而且常常是**活干完之后再挂**（`install276.sh` 就是这样：改名已经成功、它自己报 unbound variable 退出码非 0）。
    要送进远端的值一律走 `bash -s -- a b c` 位置参数；同类坑：`[ "$x" = y ] && echo` 放在脚本最后一行会把退出码带出去。
13. **客户侧探测必须带 `ping -I <源 loopback>`**（2026-11-09 就是这么误判过一次「BFD 根本没起来」）：
    不带 `-I` 时源地址取路由表选出来的 underlay 源，那条地址没有回程路径，量出来是 100% 丢包，
    与可达性无关。owner 在控制台里看到的「bfd 没起来」多数是这一条。
14. **别把 ECMP 的「留着死腿」读成丢一半**：VPP 按流选桶，同一组源/目的的探测全落在同一个桶里，
    实测要么这条流 20 秒全断、要么一条不丢（见 `exp-b3/README.md` 三臂）。要量"一半"得开多条并发流。

---

## 8. 交差的门槛（缺一件就别写"已验证"）

一个增量的完整交差 = ① `lp-ctrl` 里 `go build/vet/test` 全绿；② 改前/改后**同一口径**的两组数字；
③ `bash test/lab/vmlab/vm-verify.sh` 到 392/0/0；④ `TAG=xx bash test/lab/vmlab/exp-273/six.sh` 六方向 0%；
⑤ 换过产物的话，§3 那张 run/disk 哈希表更新（**哈希对，不是 inode 对**，见 §5.2 第 ④ 步那段）；
⑥ 文档里点名必然中断与时长；
⑦ 提交推分支 + 给登录把手。

UI 类改动另加一条：**必须在真浏览器里点过并截图**，不能只看 HTML 源码下结论。
控制台是单文件 `controller/cmd/sr-ctrl/console.html`，**改完必须 `node --check` 里面的脚本**。

---

## 9. 未决队列（owner 已经点名的活）

按 owner 拍定的顺序，**一次只做一件**：

| 任务 | 状态 | 一句话现状 |
| --- | --- | --- |
| **#275** `resolvable` 改吃活动隧道 | **已落地（2026-11-08）**，单测 6 条绿；e2e 判据待跑（要注入 BFD down，属于会动 lab 现状的动作，需 owner 声明窗口） | 门换成"配置 && 活着"：`reachableLocked()` 先看 `nextHops`（配置层，未配置=不可达，同旧），再看 `peerBFD[peer]`（活动层）。活动层是 agent 每次 `metricsLoop` 上报的 BFD 结果经 sr-route 新端点 `PUT /tunnel-state` 灌进来的；`sr-agent` 端接线在 `internal/agent/routestate.go`，`RouteStatePusher` 读 `route.json` 里的 `status_addr` 定位到本机 sr-route，把原始 `wire.TunnelMetrics` 行透传过去，聚合逻辑（同 peer 多隧道 any-up-wins、absent=没有信息）全在 `internal/route` 里。细则见 `docs/queue-plan-next.md` §二 落地实况。**四条 pin 的实际选择**：pin1 采用**内部/fib/reach.go 的 fail-open 口径**（"没跑 BFD 的 CPE 不该因为本包而黑洞所有路由"），与 §二 原方案里"严格 fail-closed"那条相反 —— 若 owner 拍要改回 fail-closed，只需调 `reachableLocked` 里 `!s.Known` 那行，代价是刚入网的前几秒（agent 第一次上报之前）本机路由会全部 pending；pin3 按 **peer 粗粒度 any-up-wins**（照 reach.go 的注释"路由只带对端地址，没有本地源，挑不了更细"），不是 §二 建议的 next-hop 精细；pin2 hold-down **本次未做**，交给 #262（那条本来就管"亚秒抖动放大"）；pin5 RR 侧本次未做，交给 #272 |
| **#276** 收敛改事件驱动 | **切片 A+B+C 全部落地（2026-11-08）**：A=提交 a8083d2（sr-frr→sr-route delta + 10ms 合并 + 重连全量），B=sr-route→sr-fib 的 SSE `/watch`（每前缀一条事件、10ms 合并、背压丢读端、订阅位独占 409、断流立即重挂+INIT 全量），C=判据与实测成文于 `test/lab/vmlab/exp-276/README.md`。新增单测 25 条全绿、`go test ./...` 全绿、`vm-verify.sh` 392/0/0、`exp-273/six.sh` 六方向 0% 丢包。**实测**（同一台 CPE1、同一注入 `systemctl restart frr`、两臂 sr-route 同一份 `26044f78f7969935`）：摘路边 ≤456/593ms → **≤40/41/45ms**，装路边 868/978ms → **201/294/755ms**；sr-route 发射端延迟直接量到（摘 ≤142ms、装 ≤84ms），残余在 sr-fib 一轮收敛 + VPP 装表那侧，本轮秒级日志拆不下去，已登记。**只剩 B2（三条口共用一套 transport identity）没做**。**2026-11-09 现场复核**：B3、B4 两条硬前置都已落地并实测收口（见下面两行）；四台里 CPE3 是唯一一台"事件 + B3 + B4"的盒（`sr-fib c213a6ca`、`sr-route 1161534d`、`sr-frr 626b126a`），CPE1 是"事件版 sr-fib 但没带 B3"的对照臂（`a9f1bc8c`、`sr-route 26044f78`），CPE2/CPE4 仍是轮询版（`bab523ff` + `sr-route 5789c2c0`） | 细则见 `docs/queue-plan-next.md` §三 落地实况（切片 B+C）。**这一片顺手抓到两条要 owner 拍的缺口：B3 watch 模式下隧道/BFD 状态变化没有任何收敛触发（`client.go:1142/1169` 的隧道门 + `cmd/sr-fib/main.go:301` 才读 BFD + 三个触发点 `main.go:509`/`watch.go:215/233/253`，现场证据 poll2 那一枪靠下一轮 1s 轮询救回）；B4 sr-route 重启后本地前缀集没人重建（`cmd/sr-frr/main.go:745-776` 变更驱动，本轮实测把 vm-verify 打出 5 条 FAIL，靠再换一枪 sr-frr 才补回）。**两条均已于 2026-11-09 收口，B4 按 owner 拍的形状 (b) 落地，判据与三臂数字在 `test/lab/vmlab/exp-b4/README.md`** |
| #272 跨盒回灌 | 待拍 | 注入的 K 路由进了客户 area0 后被别的盒当 `local` 上报 |
| #262 缺陷⑥ | 待拍 | 亚秒抖动被放大成 ≥1s 客户黑洞。**2026-11-09 更新口径**：sr-fib 那"1s 轮询直接摘路"的放大在事件版盒上已经没了（切片 B：摘路边实测 ≤40~45ms；B3 之后连"隧道/BFD 抖一下、RIB 没变"这一类也会立刻触发重算，双向 ≤65ms）。剩下的抖动放大全在 BFD 判定 + 没有 hold-down 这一侧；hold-down 现在**有地方挂了** —— 就挂在 B3 那条 `signal()` 上（`cmd/sr-fib/main.go:461` 造 `wake` → `:469` 建连时订阅 BFD 事件 → `cmd/sr-fib/watch.go:320` 的 `signal()`），挂在轮询上等于挂在一个 watch 模式已经不存在的节拍里 |
| ~~#269 缺陷⑧~~ | **已修（2026-11-07）** | `controller/deploy/sr-box/start.sh` 加了一把 `flock`（`LOCK=${SR_BOX_LOCK:-$RUN/.lock}`）：`run_up`/`run_stop` 都先抢它，非阻塞拿不到就 SKIP；`launch()`/`do_vpp`/`do_apply`/`do_frr` 每个后台子进程带 `9>&-` 关掉锁 fd，否则被 systemd `TimeoutStartSec` 杀掉时锁攥在守护进程手里永不释放。沙盒 5 路并发实测：`sr-agent`/`sr-route`/`sr-fib`/`sr-frr` 各起 1 份，4 份 SKIP 退出 0。**VM 台的物理复测没跑**（要 `systemctl restart vpp` 撞那一秒，属于会影响 lab 现状的动作，等 owner 点头再做）。抬 `sr-route` 到 HEAD 的**并发抢起阻塞已解**，剩下的只是重启本身的 RR 会话抖动 |
| ~~#268 / #260 产物没装全~~ | **已收口（#277，2026-11-07）** | 四台盘上已换成在跑那一份，`run==disk` 全一致；判据与留底见 §3 和 `exp-277/README.md`。**代码没再动**，这一轮修的是产物一致性 |
| G-2 隧道表落盘 | 只出了方案（`3a09f03`） | 隧道层磁盘恢复不了 ⇒ §2.5 那个 `sr-box` exit 1 |
| NAT 阶梯 #222 + M-4/M-1/M-3/M-5、#211/#213/#244/S8 | 挂着 | 顺序 M-4→M-1→M-2→M-3 |

**#276 切片 B/C（2026-11-08）顺手抓到的 2 条缺口：B3、B4 均已于 2026-11-09 落地并实测收口（B4 的形状 owner 拍的是 (b)）**
（B3 的判据与逐枪数字在 `test/lab/vmlab/exp-b3/README.md`，B4 的判据与三臂数字在
`test/lab/vmlab/exp-b4/README.md`，两条的机制与落地实况都在 `docs/queue-plan-next.md` §三 末尾）：

| 缺口 | 场景 | 一句话现状 |
| --- | --- | --- |
| ~~**B3**~~ watch 模式下可达性变化无人触发收敛 | 隧道/BFD 抖一下、RIB 没变 | **已修（2026-11-09）**：sr-fib 在建连时订阅本机 VPP 的 `want_bfd_events`，会话状态翻转直接 `signal()` 叫醒收敛轮；订阅失败按"起不来"处理，不留半死状态。三臂实测（CPE3 / 注入在**对端** CPE1 的 wg1009）：轮询版摘路 745ms/装回 492ms；**事件版没有 B3 = 无期限**（那条死腿在 FIB 里挂了 21s、20 发探测全挂、sr-fib 一行日志都没打）；B3 版摘路与装回双向 **≤65ms**（一个采样间隔以下）、1557 发只 FAIL 1。**#262 的 hold-down 现在有这个触发点可以挂了** |
| ~~**B4**~~ 对端 sr-route 重启后 local 前缀集不重建 | 抬 sr-route 那一枪（队列第一节要干的活）之后 | **已修（2026-11-09，按 owner 拍的形状 (b)）**：sr-route 一起来就向 sr-frr 控制口 `GET /local-prefixes` 拉一份全量并整表原子应用，拿到就退出、**不做周期重发**（拉不到每 2s 重试，开机时 `do_route` 在 `do_bridge` 之前所以这条必然走到）；sr-frr 侧"应答即降级"把拉取与增量推送的先后次序关掉，"有规则但 zebra 没连上"回 503 而不是交出一张没法信的空集。三臂实测（CPE3，注入=只重启 sr-route，探测 cust12→172.16.3.1）：老 pair 那一臂 `local` t+1s 掉到 0、**150s 仍未自愈**、客户探测连续断流 **158.66s**；新 pair 这一臂 `local` **t+1s=1**、**1899 发 0 FAIL**，sr-route 新 pid 起来 1630ms（与老臂 1635ms 同量，拉没给重启加时长）。换件后 `vm-verify.sh` **392/0/0**、`exp-273/six.sh` 六方向 0%。**部署硬规则（实测）：`sr-frr` 与 `sr-route` 必须同批抬、`sr-frr` 在前** —— 老 sr-frr 没这个端点，实测 404，只抬 sr-route 就是对着 404 重试到永远 |

历史与决策的权威出处（要引结论去这里引，别信二手转述）：
[docs/vmlab-rebuild-plan.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vmlab-rebuild-plan.md)（增量台账，最近四节是 §9.19/§9.20/§9.21/§9.22；§9.21 作废了前两节里"窗口保留"的说法，§9.22 是 #277 产物收口）、
[docs/loop-test-report.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/loop-test-report.md)（客户侧成环实验，P-3 已标注否决）、
`test/lab/vmlab/exp-27{1,3,4,7}/README.md`（实验台本，每个目录的 README 就是那轮的判据与局限）。
`/Users/louis/srvmlab/MANIFEST.md` 是产物台账，但它会滞后 —— 它原来那句"在跑的进程就是盘上这一份（inode 已核对）"
是**判据本身错了**（inode 相同只能证明进程来自那个名字，不能证明没被别人再换过一次；#277 之后更是哈希相同、inode 不同）。
它 11-07 已改成 run/disk 两列，但**以 §3 为准**，读之前先跑 §3 那条复核命令。

---

## 10. 两个 agent 共用一套 lab：冲突协议

这不是假想，这是你上车时的现实状态。共享的东西只有一份，且都不支持并发：

| 共享资源 | 冲突表现 | 约定 |
| --- | --- | --- |
| VirtualBox 7 台（同一时刻只有一个被测现场） | 你的故障注入把对方的基线打脏，两边结论都作废 | **动状态前先打招呼**：在 owner 的会话里声明"要动 CPE1–4 了，窗口 xx 分钟"；只读脚本（`vm-verify.sh`、`box-layers.sh`、`six.sh`）随时可跑，但跑的时候别有人在注入故障 |
| `/run/sr-frr/control.sock` 等盒内状态 | 两个 agent 各自热更，`config_hash` 对不上 | 一次只让一个 agent 持有"写盒内"的锁；换件前先看 `stat -c %y /usr/local/bin/<x>` 是否刚被别人动过 |
| 控制器 `state.json` + 8088 控制台 | 草稿互相覆盖、发布撞上别人的 apply | 草稿是共享的，**改配置类实验前先 `GET` 当前已发布版本**，别拿自己记忆里的值当现状 |
| `lp-ctrl`（同一份源码树） | `go build` 出来的产物不是你的改动 | 编译前 `git status --short` + `git rev-parse HEAD`，记下你构建时的树；产物一律放**自己任务号的目录**（`~/srvmlab/out-<N>/`），不要覆盖 `artifacts/linux/` 里别人的文件 |
| git 分支 `feat/vbond-control-plane` | 互相 rebase/reset 丢工作 | 不 `git add -A`（会带上别人的散工）；按文件名 stage；不 amend 已推的提交；不 reset --hard，先 `git status` 看清有没有别人的活 |

一条底线：**不许为了让自己的测量好看去改 lab 现状**（比如临时加条路由把现象压掉）。
lab 的当前形状属于 owner，不属于任何一次实验。

具体如何拉起第二个会话（工作区挂载、任务切分、告示牌锁的落地形式、出事怎么退回干净态）见
[`docs/second-agent-onboarding.md`](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/second-agent-onboarding.md)。
那一节是"怎么加入"的操作步骤；本节是"加入后不许踩什么"的硬约束。

---

## 11. 把手（登录与凭据的位置，值不进仓库）

- 控制台：`http://127.0.0.1:8088`（Mac 本机）。运营方令牌文件：`/Users/louis/srvmlab/ctrl/data/console-token`。
  PKI/状态同目录：`ca.key` `ca.pem` `server.key` `server.pem` `state.json`。
  **这些值一个都不许进文件/日志/提交信息**（AGENTS.md §3）。租户账号口令向 owner 当场索取，我们只在对话里传。
  脚本用的那份租户口令在 `/Users/louis/srvmlab/state/credentials.json`（0600，仓库外，`vm-console.py` 与
  `vm-fib-sync.sh` 都从这里读）。**它和控制器对不上时不要改脚本、也不要猜口令**：
  用运营方令牌登录 → `POST /api/accounts/reset {"username": ...}`（控制器生成新口令、只在这次响应里出现一次）
  → 把返回值写回这个文件并 `chmod 600` → 再跑 `python3 -c` 走 `api_tenant()` 复检登录。
  2026-11-09 就靠这条把 `vm-fib-sync.sh` 的 `401 invalid username or password` 修好的；
  **换过一次口令必须当天在日常会话里把新口令交给 owner**（他手里那份会失效）。
  另：`sr-ctrl` 的 `-admin-token` 在 Mac 的 `ps` 里是明文可见的 —— 别把它抄进任何文件、日志或提交信息。
- 设备 API：`0.0.0.0:8444`（TLS）；STUN：`3478`/`3479`；RR：`18446/19446`（rr1）、`18447/19447`（rr2）。
- 盒内：`ssh CPE1|CPE2|CPE3|CPE4|cust12|cust3|cust4`（走 `~/.ssh/config` 别名，直连 IP 被拒）。
- 构建：`docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'`。
- 远程仓库：gitee，只推 `feat/vbond-control-plane`；HEAD 现取 `git log --oneline -5`（见 §5.3，别信写死的哈希）。

---

## 12. 术语速查

| 词 | 指什么 |
| --- | --- |
| `local` | 本盒向全网宣称的客户侧前缀集（不是"本地路由表"） |
| refill | 与 zebra 重连后，到本地前缀集重新长回原样那段时长（现由"zebra 收录 K 路由 + 1s 轮询"串成） |
| 乐观窗口 | 已被否决的那族代码：暂时看不见就先维持旧答案。AGENTS.md 第 3 条禁它 |
| 第 5 段 | `sr-apply` 写的 FRR/泄露配置段，落 `/etc/sr-frr.json`，热更经 `/run/sr-frr/control.sock` |
| `to_sdwan` 白名单 | 反向导出的过滤（prefix-list 驱动）；环路场景的正解是这条配置，不是代码 |
| 缺陷⑦ / 缺陷④ | VPP 重启后 sr-fib 不重连 API / sr-agent 不重建隧道且仍报 ok=true。代码已修，产物见 §3 |
| K 路由 | sr-frr 用 netlink 注进内核 main 表的路由（distance 0），zebra 靠 `redistribute kernel` 看见它 |
| 表号公式 | 路由表号 = 颜色×1000 + 主表号（颜色 0 就是主表；250 已彻底归 overlay） |

---

## 13. 这份文件怎么维护

你做完一个增量，如果它改变了本文任何一节的事实（尤其 §2 状态、§3 哈希表、§5 步骤、§9 队列），
**在同一个提交里改掉本文**，并在句子后面标上"（YYYY-MM-DD 实测）"。
删掉一条过期结论时，把它指向的原始出处留在原位，别把出处一起删了。
