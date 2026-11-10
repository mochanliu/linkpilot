# exp-288：三件事一起收口（sr-fib 惰性断连 / G-2 隧道表 / 重接管脏 MAC）

日期：2026-11-10（CST）。
对象：VirtualBox 四台 CPE（CPE1–4）+ Mac 侧控制面（sr-ctrl / 双 sr-rr）。
本轮不是复测，是**修完三张缺陷票之后的落地与验证**。三个缺陷都来自 exp-286 的现场读数；
它们的共同形状是"进程在跑、日志安静、数据面缺一块"。

| 票 | 缺陷 | 根因 | 修在哪 |
|---|---|---|---|
| ① | sr-fib 惰性断连检测 | 事件驱动只在"有人说话"时唤醒；VPP 重启后 BFD 订阅随连接死掉，若 RIB 静默就没人唤醒收敛 | `cmd/sr-fib`：`convergeLoop` 加兜底心跳收敛（10s），`dropDead` 当场叫醒重连 |
| ② | G-2：sr-fib 隧道表没人填 | `{next_hop, interface, color}` 三个键分属三个地方，产品里没有进程同时有它们 | `internal/wire` 隧道段加 `color`（指针，区分"颜色 0"与"老控制器没发"）+ `internal/agent/fibtable.go` 派生器 + `cmd/sr-agent` 心跳挂载 |
| ③ | 重接管用脏 MAC | `lcp-sync` 把 tap 的随机 MAC 反写内核 netdev；产品路径从不建 af_packet 口、也从不校验 MAC | `internal/apply`：`runVPP` 预扫描 `lcp create host-*`，建口前把净 netdev MAC 改回 `ethtool -P` 的烧录值，建口时 `hw-addr` 用同一个值 |

一张表看三件事各自对应的"以前会发生什么"：

| 场景 | 修之前 | 修之后 |
|---|---|---|
| VPP 重启且 RIB 无变化 | sr-fib 抱死句柄静默待着，FIB 空，无日志（exp-286 现场：203 行 broken pipe） | 断连当秒立刻重连；之后每 10s 一拍兜底重试（连"重连成功但当时口还没建好"这种半失败也兜） |
| 盒子开机 / 隧道增删 / 改色 | sr-fib 的隧道表只有 lab 脚本 `vm-fib-sync.sh` 手工填，产品里为空 → 所有路径被跳过、FIB 一条 overlay 路由都没有 | sr-agent 每拍心跳派生一次，和 sr-fib 现在那份不同就 PUT；不落盘（磁盘 `fib.json` 始终 0 条） |
| VPP 重启后重接管客户口 | af_packet 口只有 lab 的 `takeover-if.sh` 建（MAC 靠 VBoxManage 手工传）；产品路径只 replay `lcp create`，第一句 `set interface mtu host-ethX` 就以 unknown interface 失败 | `sr-apply` 预扫描建口；netdev 被写脏时先 `down`→`address`→`up` 改回烧录值，再 `create ... hw-addr` |

## 1. 部署

产物在 `~/srvmlab/out-288/{bin,gz}`；铺送走 `push-par.sh`（~6 路并发 ssh，1MB gz 分片），
装件走 `install-288.sh <BOX>`（先 `sr-agent` 后 `sr-fib`，中间不重启 `sr-route`/`sr-frr`）。

顺序上有一条硬约束：**先换 Mac 侧的 sr-ctrl，再换盒子**。新 agent 的派生器看到隧道段没有
`color` 字段会整轮拒绝（方案 B），所以控制器必须先会发颜色。

控制器（Mac）：`sr-ctrl 0c92cb0154c0894e`，重启后 8444/8088 在听，双 sr-rr 未动
（`sessions=5 synced=[True×5] peers=[('rr2',True)]`）。

四台盒子的换件中断时长（sr-agent / sr-fib 从 SIGTERM 到状态口回话）：

| 盒 | sr-agent | sr-fib | 新 pid（agent / fib） |
|---|---|---|---|
| CPE1 | 3.55 s | 3.76 s | 892159 / 892536 |
| CPE2 | 3.20 s | 3.24 s | 692578 / 692965 |
| CPE3 | 3.67 s | 3.78 s | 1050190 / 1050578 |
| CPE4 | 3.69 s | 3.64 s | 532676 / 533056 |

装完 `sha256` 三处同值（`run` = `/proc/<pid>/exe` 的哈希），agent `ec107eb43d455465`、
`sr-apply a608e98ee76352a7`、`sr-fib d6ccda1d5a594995`。

## 2. 验证

### 2.1 ② G-2：四台的隧道表都由 agent 派生出来了

换件后 5 分钟（不做任何手工动作，`vm-fib-sync.sh` 已退役）：

| 盒 | sr-fib 隧道表（下一跳/口/颜色） | escape | converged | 缺行警告 |
|---|---|---|---|---|
| CPE1 | 10.250.0.1→wg1009/0、10.250.0.3→wg1010/0 | false | true | 0 |
| CPE2 | 10.250.0.7→wg1013/**2**、10.250.0.9→wg1014/0 | false | true | 0 |
| CPE3 | 10.250.0.0→wg1009/0、10.250.0.5→wg1011/0、10.250.0.6→wg1013/**2** | false | true | 0 |
| CPE4 | 10.250.0.2→wg1010/0、10.250.0.4→wg1011/0、10.250.0.8→wg1014/0 | false | true | 0 |

颜色与拓扑对得上（CPE2↔CPE3 那条是颜色 2 的主备腿，两端都报 2），`escape_all_colors`
四台都保持原值 `false`（读-改-写带回，没有把运维的手工设置抹掉）。
同期数据面：四台 `BFD Up 条数 = peer established 条数 = wg 口数 = 隧道条数`（2/2/2/2、2/2/2/2、3/3/3/3、3/3/3/3）。

**换件当场的那 2 分钟就是 G-2 的窗口**（CPE2，UTC）：

```
04:34:21  sr-fib 重启 → 隧道表 0 条
04:34:21  警告: 172.16.3.1/32: 下一跳 10.250.0.7 不在隧道表里，该路径跳过; 172.16.4.1/32: …
04:38:51  sr-agent: 隧道配置 v24 已收敛              ← Mac 侧 sr-ctrl 这时才恢复，心跳才开始走
04:38:56  PUT /config：VRF [0]、隧道 2 条、逃生开关=false（只改内存，磁盘上的 fib.json 没动）
04:38:56  收敛 tables+1 labels+0/-0 routes+2/-0 mpls-egress=true
```

也就是说：agent 一拍之内把表补上，`不在隧道表里`的两条路径当场回来（routes +2）。
那段 4 分半的空窗不是派生器慢，是 Mac 侧控制面掉线、心跳根本没发出去（下面 §3.7）。

**开机也是同一条路**（CPE2 power cycle，05:07）：

```
05:07:16  sr-fib 起来：隧道表 0 条 + 两条"不在隧道表里"
05:08:11  sr-fib 隧道表: sr-fib http://127.0.0.1:19011 连不上（还没起来？下一拍再试）
05:08:16  sr-fib 隧道表已收敛：2 行、vrfs=[0]、escape=false
05:08:16  PUT /config：… 隧道 2 条 …   →  收敛、warnings 清空
```

sr-fib 状态口回话到表被补上 = **3 秒**，没有任何脚本参与。磁盘 `fib.json` 全程 0 条
（`{"vrfs":[0],"tunnels":[]}`，mtime 还是 11-04），这就是"表是运行时推来的、不是开机读盘"的反证。

### 2.2 ① VPP 重启自愈（CPE2，04:45:30）

```
04:45:31  VPP api 连接断了（broken pipe）：立刻重连，按整份计划从头收敛
04:45:31  已连上 VPP api.sock  →  本轮收敛失败: 隧道接口 wg1014 不存在
04:45:41  本轮收敛失败（同一句）                      ← 10s 兜底那一拍，无人叫它
04:45:44  BFD 10.250.0.7 up → 收敛 routes+1
04:45:45  BFD 10.250.0.9 up → 收敛 routes+1           ← 全量恢复
```

VPP 重启 → FIB 完整恢复：**15 秒**。两条断言一起成立：断连当秒就重连（`dropDead` 自己叫醒，
不再等外部事件），以及"重连后当时口还没建好"的半失败由 10s 兜底那拍接住（04:45:31 → 04:45:41）。
exp-286 的对照组是 203 行 broken pipe、FIB 空到有人手动碰它。

### 2.3 ③ 脏 MAC：现场推翻了代码里的一个假设

先把 CPE2 客户口弄脏（模拟 `lcp-sync` 的回写；`vppctl show lcp` 确认 `lcp-sync on`）：

```
netdev enp0s8 cur=02:de:ad:be:ef:01  perm=08:00:27:4c:ed:b7   ← 脏
```

再走 `ensureHostIface` 的那几步（`allmulticast on` → `up` → `create host-interface name enp0s8 hw-addr <perm>`）：

```
VPP host-enp0s8 : 用 hw-addr 的烧录值（08:00:27:4c:ed:b7）
lcp TAP lcp-x0  : 08:00:27:4c:ed:b7        ← TAP 也拿到烧录值
netdev enp0s8   : 02:de:ad:be:ef:01        ← 还是脏的
```

**结论：`hw-addr` 只决定 VPP 侧（以及镜像出来的 lcp TAP）的 MAC，不改内核 netdev。**
此前代码注释写的"hw-addr 顺带把内核那一侧改回来（VPP 翻 down、改地址、再 up）"是错的，
本轮实测推翻。脏值不是无害的，它是个地雷：lab 自己的接管脚本一见 MAC 不符就直接拒绝接管 ——

```
FAIL enp0s8 的 netdev MAC=02:de:ad:be:ef:01 与传入的 08:00:27:4c:ed:b7 不等；
     先放开 VPP 再把 netdev 修回该值
```

于是那台盒子停在"客户口没被接管"的半死状态，等人来手工修。所以修法补了一步：
**建口之前显式把 netdev 改回烧录值**（`down` → `address` → `up`，改 MAC 必须先 down；
改不动只记日志、不挡住接管，因为 VPP 侧有 `hw-addr` 已经是对的）。
对应两条新单测：`TestHostIfaceRepairsDirtyKernelMAC`（三步顺序 + 发生在建口之前 + 日志点名）、
`TestHostIfaceCleanKernelMACIsNotRewritten`（本来就是烧录值时不抖口，大小写不同不算脏）。

本条的限制写在 §3.6：VM 台上没有装客户配置，产品路径（`sr-apply` 应用第 5/6 段并执行
`lcp create`）根本不会跑到，所以现场只能验到"这条 VPP 命令 + 这条改回命令的效果"，
Go 那一层靠单测（`internal/apply` 22 条，本轮新增 2 条；`internal/agent` 10 条，全为 G-2）。

## 3. 已知边界

1. `escape_all_colors` 仍走读改写（方案 A 走乙）：运维手工 `curl` 改逃生开关与 agent 推表之间存在
   一个覆盖窗口。要彻底消掉，得把逃生开关渲进第 4 段（O6-b 的欠账）。
2. 派生器每拍都问一次 VPP 接口表（`DumpInterfaces`）。当前规模（每盒 ≤ 数条隧道）可忽略；
   §9 的 4096 条上限下要复测，不行再改成"只在隧道对账变化时问"。
3. 颜色缺失时**整轮拒绝**（方案 B 的倾向）：老控制器 + 新 agent 的组合下，sr-fib 隧道表不会被派生。
   代价是两端必须同版本升级，这一点在部署顺序上要记住：**先换控制器，再换盒子**。
4. `vm-fib-sync.sh` 已退役（删文件，调用点改成"等 agent 自己回来"）。要手工改逃生开关的话，
   直接 `PUT /config` 把整份表重发一次即可；派生器的 `sameFibTables` 只比 vrfs 与 tunnels，
   escape 不在比较里，所以手工设的值不会被下一拍覆盖。
5. **停 VPP = 这台盒子从网络上消失**（管理地址在 lcp TAP 上）：`systemctl stop vpp` 之后
   CPE2 20 分钟不可达，只能断电重启（20 秒回来）。所以 ① 只能用 `restart` 复现"断连"，
   不能用 `stop` 复现"断连且长时间没有事件"；那条路要靠单测
   `TestHealthTickRedialsDeadVPPWithoutAnyEvent`（20ms 心跳、从不等事件）。
6. VM 台上没有客户配置（`/var/lib/sr-cpe` 不存在，`start.sh` 的 apply 步一直 SKIP），
   所以 ③ 的产品分支在本台站跑不到；真机（有 bundle 的盒子）才会走完整链路。
   本轮改动的二进制已重编（`sr-apply dc5c4a8f6021351d`），随下一轮包铺。
7. Mac 侧 `sr-ctrl` 不能靠 `nohup` 从短命 shell 里起（这条 shell 一退它就没）；
   `launchctl` 在这个受限环境里报 `5: Input/output error`。本轮用的是双 fork + `setsid`
   的真守护（`/tmp/spawn_daemon.py`）。控制面一掉，四台盒子的心跳全断、派生器整轮不跑 ——
   04:34–04:38 那 4 分半的空窗就是这么来的。
