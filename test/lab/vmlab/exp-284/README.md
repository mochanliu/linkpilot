# #284 抬盒：把 B3/B4 那三份产物铺满四台 CPE

这一节只讲**部署**（代码在 `af5811c` 之前就进了 HEAD）。现场台账在 `docs/agent-handover.md` §3，
方案与前因在 `docs/queue-plan-next.md` 的切片 B（B3/B4）那一节。

## 0. 抬完之后是什么样

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | 说明 |
| --- | --- | --- | --- | --- |
| CPE1 | `1161534d5c91db40` | `a9f1bc8c8fd49016` | `626b126a331f65e0` | **`sr-fib` 有意不动**：事件版无 B3，是手上唯一的缺陷臂 |
| CPE2 | `1161534d5c91db40` | `c213a6caf1b2aabf` | `626b126a331f65e0` | 三件全抬 |
| CPE3 | `1161534d5c91db40` | `c213a6caf1b2aabf` | `626b126a331f65e0` | 本轮没动（B3/B4 就是在它上面开发并验过的） |
| CPE4 | `1161534d5c91db40` | `c213a6caf1b2aabf` | `626b126a331f65e0` | 三件全抬 |

十二件全部 `run==盘`，判定方式是逐台 `sha256sum /proc/<pid>/exe` 对 `/usr/local/bin/<name>`
（`snapshot284.sh` 一条命令全包）。收口判据两条都过了：

- `bash test/lab/vmlab/vm-verify.sh` → **PASS=392 FAIL=0 WARN=0**（2026-11-09 实测）；
- `TAG=284-final bash test/lab/vmlab/exp-273/six.sh` → 六方向**全 0% 丢包**，rtt avg 8.5~18.7ms（同一轮实测）。

## 1. 合法动作序列只有四步，一步不能省

```bash
# ① 推产物到 staging（只落 /root/vmlab/artifacts/linux/<name>.new，不碰 /usr/local/bin）
bash test/lab/vmlab/exp-276/push276.sh <BOX> <name> <src.gz>     # 或下面第 3 节的 LAN 中继
# ② 装盘（原子改名，运行份不动 —— 这一步之后「跑 ≠ 盘」才是对的）
bash test/lab/vmlab/exp-276/install276.sh <BOX> <name>
# ③ 换进程（SIGTERM → 产品自己的开机清单补回 → 客户侧 75 秒探测）
WORK=/tmp/p284 bash test/lab/vmlab/exp-276/swap276.sh <BOX> <name> [探测机] [源 loopback] [目的]
# ④ 只有换 sr-fib 才需要：立刻补隧道表
bash test/lab/vmlab/vm-fib-sync.sh <节点号>
```

**顺序是硬的，两条原因**：

1. **盒内 `sr-frr` 在 `sr-route` 之前。** 老 sr-frr 没有 `GET /local-prefixes`，CPE4 上实测直连
   `/run/sr-frr/control.sock` 回 **404**；只抬 sr-route 就是让它对着 404 每 2s 重试到永远，本盒客户前缀
   永久消失。抬完 sr-frr 同一条命令回 **200** 且给出 `{"prefixes":[{"vrf_id":0,"prefix":"172.16.4.1/32"}]}`
   —— 这条命令就是"前置到没到位"的现场判据，`swap276.sh` 的 sr-route 分支已经把它编进去了
   （404/503/200+local=0 各有指名动作）。
2. **`push276.sh` 不装盘。** 本轮真实踩过：推完直接跑 `swap276.sh CPE4 sr-frr`，脚本自己喊了
   `!! 跑 == 盘：这一枪不会改变任何运行事实` —— 那一枪等于拿老份做了一次重启自检（它确实会跑完 SIGTERM +
   补回，只是换不起来，而且**这一次重启是有一次 1.03s 客户可见段的**，见第 5 节）。
   `swap276.sh` 只负责"把盘上那份抬起来"，装盘必须是 `install276.sh`。

## 2. 换 sr-fib 之后为什么必须 `vm-fib-sync.sh`（缺口 G-2 在这里咬人）

新 sr-fib 一起来内存里是 **0 条隧道**（`/etc/sdwan/fib.json` 的 `tunnels` 盘上就是 0，产品没人落盘推导表），
所有远端路径按「下一跳不在隧道表里」跳过。日志上这条链在 CPE4 一眼走完：

```
11:05:09 sr-fib: 1 个 VRF、0 条隧道，从 http://127.0.0.1:19001/status 的事件流收敛（断流/占用时先全量补一次，每 1s 再挂一次；逃生开关=false）
11:05:09 sr-fib: 已订阅本机 VPP 的 BFD 状态变化事件，可达性一变就唤醒收敛（真值仍以下一次 bfd_udp_session_dump 为准）
11:05:09 sr-fib: 警告: 172.16.12.1/32: 下一跳 10.250.0.2 不在隧道表里，该路径跳过; …
11:05:09 sr-fib: 收敛 tables+0 labels+0/-0 routes+0/-0 mpls-egress=true      ← 空窗开始
11:06:28 sr-fib: PUT /config：VRF [0]、隧道 3 条、逃生开关=false（只改内存，磁盘上的 fib.json 没动）  ← vm-fib-sync.sh 那一枪
11:06:28 sr-fib: 收敛 tables+0 labels+0/-0 routes+2/-0 mpls-egress=true      ← 空窗结束，两条远端前缀装回
```

**空窗有多长、后果是什么，两次都量了**：CPE2 = 124s、CPE4 = 79s（上面 11:05:09→11:06:28）。
两次的收敛行都是 `routes+0/-0` —— **sr-fib 在空窗里不拆已经装好的 VPP 行，只是不再收敛**。
所以后果是"从这一刻起路由变化全部失效"，不是"立刻断客户流量"：这两段窗口内客户探测 **0 FAIL**，
`show ip fib` 的 `mpls via` 条数也没变（**没变是孤儿行还在，别把它当成 B3 生效的证据**）。
补没补上只认 `vm-fib-sync.sh` 那一行 `隧道 N 条 … converged=True` 且没有 `警告:`。

## 3. 传输：别再从 Mac 推，走盒↔盒 LAN

Mac↔盒子实测 **7~14KB/s**，一件 8.4MB 要十几分钟，四台十二件是纯传输一小时以上。
本轮改成：**已经持有那份产物的盒当源**，管理 LAN 上 http 直传，实测 **226KB/s**
（`sr-frr` 8446056 字节 CPE3→CPE4 用 37.3s，`sha256sum` 与 Mac 上的参照一致）。

```bash
# 源盒（它 /usr/local/bin 里跑的就是目标版本）
ssh CPE3 'mkdir -p /tmp/xfer && cp -f /usr/local/bin/sr-frr /usr/local/bin/sr-route /tmp/xfer/'
ssh CPE3 'setsid timeout 420 python3 -m http.server 8099 --directory /tmp/xfer >/dev/null 2>&1 & echo "中继 pid=$!"'
# 目标盒逐个 curl，落 staging，哈希对完才算数
ssh CPE4 'curl -s --max-time 180 -o /root/vmlab/artifacts/linux/sr-frr.new \
            http://192.168.1.103:8099/sr-frr && sha256sum /root/vmlab/artifacts/linux/sr-frr.new'
# 收口中继：用记下来的 pid，别用 pkill
ssh CPE3 'kill <pid>; rm -rf /tmp/xfer'
```

三个坑都是本轮真撞上的：

- **不要加 `--bind <管理口 IP>`。** `vpp0` 那个地址（`192.168.1.103`）绑上去会周期性
  `OSError: [Errno 99]` —— af-packet 口不能 bind/listen，报的错还是误导性的 "Cannot assign requested address"。
  默认绑 `0.0.0.0` 即可。
- **`timeout` 会掐断正在服务的连接。** 第一次给 sr-fib 设 120s，第三个文件传到一半被切，
  目标盒 curl 退 **18（CURLE_PARTIAL_FILE）**、拿到的就是半份。超时按**总字节**估，别按单件。
- **`pkill -f "http.server 8099"` 会打死自己。** 这个模式匹配到发起它的那条 ssh 命令行本身，
  ssh 退 **255**（不是"没匹配到"的 1）。要么记住 pid 用 `kill`，要么把模式写成不匹配自身的形式。
  半路被杀时 `rm -rf /tmp/xfer` 可能没执行到，补一次并核 `ss -ltn | grep 8099` 归零。

半份产物不会自动生效：`install276.sh` 只认 `$D/<name>.new`，而它的哈希核对在 `push`/本文这条 curl 里做，
**所以核哈希这一步不能省**。

## 4. 每条探测覆盖到谁（不写这条，"0 FAIL"就是运气不是证据）

归属用内核自己的 ECMP 哈希问出来，读且可复跑：

```bash
ssh cust12 'ip route get 172.16.4.1 from 172.16.12.1; ip route get 172.16.3.1 from 172.16.12.1'
# 172.16.4.1 from 172.16.12.1 via 172.31.12.2   ← CPE2
# 172.16.3.1 from 172.16.12.1 via 172.31.12.2   ← CPE2
# （不带 from 时 172.16.4.1 via 172.31.12.1 = CPE1 —— 源地址换一下就会翻到另一台）
ssh cust3 'ip route get 172.16.12.1 from 172.16.3.1'   # via 172.31.3.1 = CPE3 进
```

本轮用到的两条：

| 探测 | 正向 ingress | egress | 对哪一枪敏感 | **不**敏感于 |
| --- | --- | --- | --- | --- |
| `cust12 -I 172.16.12.1 → 172.16.4.1` | **CPE2** | **CPE4**（站点 4 单归，只有一条腿） | CPE2 与 CPE4 的全部三枪 | — |
| `cust3 -I 172.16.3.1 → 172.16.12.1` | CPE3 | CPE1 与 CPE2 之间由 VPP 多路径分流 | CPE1 作为 egress 时装不转发 | **见下** |

**CPE1 那两枪 0 FAIL 不能读成"CPE1 换件没有窗"**，两个原因叠在一起：

1. 站点 12 是双归的 —— CPE1 与 CPE2 都报 `172.16.12.1/32`，CPE1 把自己的 local 摘掉时 CPE2 那份还在，
   别盒不会掉路。这不是测量缺陷，这就是站点 HA 本来的行为；但它意味着**这类故障在这条探测上是隐形的**。
2. 那条探测的去程落在 CPE1 还是 CPE2 由 VPP 多路径的逐流哈希决定（CPE3 对 `172.16.12.1/32` 有 2 条
   `mpls via`），不是稳定的单盒路径。

要真正量到"CPE1 单盒摘 local"，得先把 CPE2 从这条路上摘出去 —— 那是动 lab 现状，本轮没做，留给 owner 定。
（对照：CPE4 是单归的，所以第 5 节那个 1.03s 在 CPE4 上看得见、在 CPE1 上结构性看不见。）

## 5. 每一枪的数字（`swap-*.txt` 原文在 `/tmp/p284/`、`/tmp/p276/`）

探测口径：`ping -I <src> -c1 -W1` 每秒一发共 75 秒；链路通时约 32~38 发/秒，只有真断流才塌成 1 发/秒的
连续 FAIL 片，所以"连续段"判的是硬断流而不是采样噪声。

| 枪 | 换之前 run | 新 pid 起来 | 探测发数 | FAIL | 最长连续断流 |
| --- | --- | --- | --- | --- | --- |
| CPE2 sr-frr | `c8e731dd` | 1501ms | 2855 | 0 | — |
| CPE2 sr-route | `5789c2c0` | 1531ms | 2871 | 0 | `local=1` + 端点 200 |
| CPE2 sr-fib | `bab523ff` | 1559ms | 2880 | 0 | 之后空窗 124s（G-2，见第 2 节） |
| CPE4 sr-frr（**误枪**：未装盘） | `c8e731dd` | — | 2690 | 4 | **1.03s** |
| CPE4 sr-frr | `c8e731dd` | 1467ms | 2805 | 1 | 0.00s（孤立） |
| CPE4 sr-route | `5789c2c0` | 1435ms | 2797 | 1 | 0.00s；`local=1` + 端点 200 |
| CPE4 sr-fib | `bab523ff` | 1646ms | 2784 | 2 | 0.00s（均孤立）；之后空窗 79s |
| CPE1 sr-frr | `c8e731dd` | 1491ms | 2794 | 0 | 敏感度见第 4 节 |
| CPE1 sr-route | `26044f78` | 1572ms | 2806 | 0 | `local=1` + 端点 200 |

合计 **25282 发**。

**"抬 sr-frr"这一枪本来就有窗，别当成偶发。** 机制是：sr-frr 重启 = 它的上报连接重连 = 按 AGENTS.md §1.3
立刻重置 local 前缀集，重置到它重新上报之间，本盒客户前缀在别盒就是 0 条 `mpls via`。
所以这一枪的中断上界 = 重启环 + 重新上报 + RR 反射 + 别盒装表。同一个盒同一个方向跑过两次
（CPE4，`cust12→172.16.4.1`）：一次连续 2 发 / **1.03s**，一次 1 发孤立 FAIL。
各只有一个样本，**不能据此说"新版比老版短"**；能说的是**窗宽 0~1s，动之前按 §1.4 点名**。
dmesg 近期无 link 事件，排除了"那 1.03s 是链路抖"。

## 6. 混搭会不会出事：CPE1 就是那个混搭样本

CPE1 抬完之后是 **新 sr-route（`1161534d`）+ 老 sr-fib（`a9f1bc8c`，事件版无 B3）**。
这一对在换件现场直接验过，两条证据：

```
# 老 sr-fib 在 sr-route 死掉那一刻的退路（fib.log）
sr-fib: 事件流结束（watch: 事件流在帧中间结束）：改用全量 /status，并按重连间隔再挂
sr-fib: 本轮收敛失败（保留上次成功的计划）: 读 RIB: Get ".../status": connection reset by peer
```

```
# 重挂上了（换件后 ~90s，ss -tnp）
ESTAB 127.0.0.1:39706 -> 127.0.0.1:19001  users:(("sr-fib",pid=682422,fd=7))     ← 老 sr-fib
ESTAB 127.0.0.1:19001 -> 127.0.0.1:39706  users:(("sr-route",pid=749967,fd=10))  ← 新 sr-route
```

中间那一轮"收敛失败"是对的：它按 §1.3 保留上一次成功的计划、不维持旧值再等一拍。
新版本对老版本向后兼容的两条代码根据：新 sr-route 仍服务 `GET /status`（`controller/cmd/sr-route/main.go:425`）
并新增 `/watch`（同文件 :607）；新 sr-fib 在没有 `/watch` 时退回全量 `/status`
（`controller/cmd/sr-fib/watch.go:95,255`）。**所以任何一台盒上 sr-route/sr-fib 版本不齐都不会黑洞**，
不齐的代价只是退回轮询。

## 7. 回滚

**先把回滚底做在前面**：`install276.sh` 的 `mv -f` 会覆盖 `/usr/local/bin/<name>`，老份的字节只在
**还在跑的那个进程**的 inode 里。所以装盘之前先抽（本轮四台都这么留的）：

```bash
ssh <BOX> 'for n in sr-frr sr-route sr-fib; do p=$(pgrep -x $n|head -1); B=/usr/local/bin/$n.prerun-20261109;
  [ -e $B ] || { cp /proc/$p/exe $B.inst && chmod 0755 $B.inst && mv -f $B.inst $B; }
  printf "%-9s run=%s 备份=%s\n" $n $(sha256sum /proc/$p/exe|cut -c1-8) $(sha256sum $B|cut -c1-8); done'
```

现存回滚底（哈希与当时运行份逐一对上，2026-11-09 实测）：

- CPE1：`sr-frr.prerun-20261109`=`c8e731dd`、`sr-route.prerun-20261109`=`26044f78`、`sr-fib.prerun-20261108`=`bab523ff`
- CPE4：`sr-frr/sr-route/sr-fib` 的 `.prerun-20261109` = `c8e731dd` / `5789c2c0` / `bab523ff`

回退 = 拿备份当源再走一遍装盘 + 换进程，两枪：

```bash
bash test/lab/vmlab/exp-276/install276.sh <BOX> <name> /usr/local/bin/<name>.prerun-20261109
WORK=/tmp/p284 bash test/lab/vmlab/exp-276/swap276.sh <BOX> <name> [探测机] [源] [目的]
bash test/lab/vmlab/vm-fib-sync.sh <节点>     # 回退的是 sr-fib 时同样要补隧道表
```

**B4 侧回退必须同批**：只回退 sr-frr、留着新 sr-route = 把它推回对着 404 每 2s 无限重试的形状
（就是第 1 节那条）。反过来只回退 sr-route、留新 sr-frr 是安全的 —— `GET /local-prefixes` 没人订阅就是闲置的。

## 8. 自己复核这几件事

```bash
cd <repo>
bash test/lab/vmlab/exp-284/snapshot284.sh check                 # 十二件 run/盘 哈希 + 两台 /status
bash test/lab/vmlab/vm-verify.sh                                 # 全量判据
TAG=check bash test/lab/vmlab/exp-273/six.sh                     # 六方向客户侧丢包
# B4 前置到没到位（在任意一台盒上）
ssh <BOX> 'curl -s -o /dev/null -w "%{http_code}\n' \
  '--unix-socket /run/sr-frr/control.sock http://sr-frr/local-prefixes"'   # 200=到位 404=老 sr-frr 503=反向导入未就绪
# 隧道表补没补上（换过 sr-fib 之后）
ssh <BOX> 'curl -s http://127.0.0.1:19011/status' | tr -d ' \n' | python3 -c \
  'import sys,json;print(len(json.load(sys.stdin)["tunnels"]))'
```
