# exp-274：把"乐观窗口"这一族从产品里删掉（owner 定案）

对应 `docs/vmlab-rebuild-plan.md` §9.21（并更正 §9.19 的 A2/A3、§9.20 第五节）。

owner 的原话是这一轮的范围：

> 嗯，这是真正症结，把这个改成检查活动的下一跳隧道，而不只是看配置。所以你之前加的那个"乐观窗口"，
> 请 rollback 掉。我记得你不止一处加了这个乐观窗口。……咱们的路由协议是对收敛速度有要求的，
> 链路状态的变化，要几乎实时的反应在路由上，否则就会有瞬时黑洞。所以乐观窗口我理解都不应该存在的。

追问后拍定的三点：**回滚范围 A+B+C+F 全清**、**AGENTS.md 第 3 条同步改口径**、
可达性粒度**严格 fail-closed（无 BFD 信息即不可达）** —— 第三点属于 #275，本轮只做前两点的代码。

## 删掉的是哪四个窗口，留下了什么

| 编号 | 原名 | 它在哪一拍维持旧值 | 本轮 |
| --- | --- | --- | --- |
| A | `BeginResync` + `resyncQuiet=500ms` / `resyncDeadline=10s` | sr-frr 与 zebra 重连后，`stale ∪ pending` 当可见集 | **删**，换成 `Importer.Reset()`：重连即清空，新连接的回说什么就是什么 |
| B | `BeginLinkWindow` + `linkRecovery=30s` | 内核链路 down/up 事件（VPP 重启拆 lcp/vpp0）那 30s 维持旧集合 | **删**，链路事件不再开窗，`watchLinkEvents` 只剩 D-11 的 `touch` K 路由那一段 |
| C | `Settled()` + `export_frozen` / `export_frozen_held` | 首轮对账没落地时注入"只撤不加" | **删**（它是 A/B 的闸门，窗口没了就没有"落地"可言）；`/status` 少这两个字段 |
| F | `onceWindow` / `onceSettle`（`-once` 路径自己再等 300ms） | `-once` 退出前多等一拍 | **删**，`-once` 变成"跑一轮就退出" |
| D | 轮询里"先读完并上报客户侧，再决定注入"的次序 | —— 这不是窗口，是读写次序 | **留** |
| E | `linkSettle=300ms` 去抖 + `touch` K 路由重放 | —— 这是 D-11（链路删除后 zebra 丢 K 视图，导出静默失效）的修法 | **留** |

单测随窗口删掉 6 条（`import_gate_test.go` 整文件 `git rm`，`import_test.go` 里 4 条重连窗口 + 假时钟），
剩下改成"重连即重置"的判据；`go build ./...` 与 `go test ./...` 在 `lp-ctrl`（`/workspace/controller`）全绿。

## 脚本

| 脚本 | 量什么 | 会不会动状态 |
| --- | --- | --- |
| `sample-local.sh [秒=70]` | 盒子本地 200ms 一发：`local=N` = sr-route `/status` 的 local 条数（本盒对外宣称的客户侧前缀集，由 sr-frr 每轮 PUT 上去）；`frr=M <zebra_connected>` = sr-frr `/status` 的 `reported` | 只读 |
| `reconnect-window.sh [BOX=CPE1] [PB=cust12] [SRC] [DST]` | 上面两路采样 + 秒级 ping 探测，中途 `systemctl restart frr` 作为被测故障，给出偏离起止（相对盒子墙钟的注入时刻）、最低值、偏离总时长、断流段长度、收尾读数与 `bridge.log` 摘录 | **会 restart frr**（见下） |
| `../exp-273/push-binary.sh [盒…]` | gzip → scp → `sha256sum` 核对 → `install -m 0755`，不重启任何东西 | 写文件，不重启 |
| `../exp-273/window-swap.sh <盒> [探测机] [源] [目的]` | 换件这一枪的客户可见时长：探测中途 SIGTERM sr-frr，产品开机清单（`sr-box-watch.service`，Type=oneshot + `KillMode=process`）自己补回来 | **会 SIGTERM sr-frr** |

`../exp-273/six.sh`（六方向）与 `../../vm-verify.sh`（全 lab 判据）本轮复用，不新写。

**故障注入的口径要说清**：`systemctl restart frr` 是这一枪选的被测故障（它等价于"控制面接缝断一次 +
客户侧会话重建"），不是配置生效路径 —— AGENTS.md 第 1 条禁止的是"用重启让配置生效"，不是禁止拿重启当实验。
lab 里 restart frr 会把 sr-fib 注入的那两条 K 路由从 zebra 的 RIB 里冲掉再重新学回来，正是窗口本来要掩盖的那一拍。

## 实测：这一枪里窗口没买到任何东西（改前 9.4s vs 改后 12.6s / 15.6s）

三次 `restart frr`（CPE1，探测机 cust12 → 172.16.4.1，`local` 基线 1 条）：

| 标签 | 产物 | `local` 掉到 0 的时刻 | 回到 1 的时刻 | 偏离总时长 | 探测 FAIL |
| --- | --- | --- | --- | --- | --- |
| `before` | `9f28db6c268aa12c`（A+B+C 都在） | +2.38s | +12.2s | 9.4s（22/160 发） | 2/2037，1 段 1.0s |
| `after` | `c8e731ddcbca7bd6`（窗口全删） | +2.57s | +15.2s | 12.6s（30/160 发） | 4/1983，2.1s + 一发 |
| `after-in` | 同上，反方向（cust4 172.16.4.1 → 172.16.12.1） | +2.77s | +18.3s | 15.6s（37/161 发） | 2/2006，1 段 1.0s（见"台的局限"） |

三行读下来，真正的结论不是"代价多大"，而是**改前也掉**：

- 改前的 `local` 同样在 +2.4s 变 0，`frr(reported)` 在同一发就已经是 0。所以那次空窗不是"重连把旧集合撤了"，
  而是**窗口按 500ms 安静提前关上了，而那份回放本来就是空的** —— zebra 刚起来时自己 RIB 里还没有那两条
  K 路由，`redistribute kernel` 的第一份答复就是 0 条，窗口把"回放到齐了，答案是 0"当成新事实接受了。
  后面 `sr-fib` 的 K 路由被 zebra 重新学回来（+12~18s）才把 1 条补回去。
  这一步是时序推断，不是日志原文：`reported=0` 那一发（+2.03s）`zebra_connected` 仍是 true，而窗口还开着时
  可见集是 `stale ∪ pending` = 1 条 —— 只有"已关窗"能同时解释这两个读数；关窗要到 `seen` 之后安静 500ms，
  对应 +1.4s 连上、+2.0s 落地，误差在一个采样间隔（200ms）内。
- 改后 `Reset()` 走的是同一条时间线：连接一断就清空，等新的回放把前缀带回来。
- 也就是说 9.4s → 12.6/15.6s 这个差**不是窗口保护出来的差**，是 refill 时刻的抖动
  （zebra 重启后重新收录 K 路由、再经 1s 轮询落地，本来就有好几秒的分散）。窗口唯一可能买到的东西，
  在这个场景里它一秒都没买到。

改前那一份的日志原话（盒子时间，CPE1 的钟比 UTC 快 ~10.7 分钟，所以只有时长可比）：

```
sr-frr: 与 zebra 的连接断开（zebra 连接已关闭: EOF），重连
sr-frr: 反向导入未连上 zebra: ... connect: connection refused
sr-frr: 重连后 10s 没收到任何回放，按空集合收尾（撤掉旧前缀 1 条）      # 改前：到 10s 上限也还是空
sr-frr: 重连，清空本地前缀集（1 条），等新连接的回放重建              # 改后：新码的日志（同一条前缀集）
```

## 换件：四台从 `9f28db6c268aa12c` 到 `c8e731ddcbca7bd6`，零冷启动 + 实测中断

三步（`push-binary.sh` 只落盘到 `/root/vmlab/artifacts/linux/`，**不会**装到 `/usr/local/bin`，
所以每台要自己 `install -m 0755`，再 SIGTERM 让产品自己的 `sr-box-watch.service` 补回来）：

| 被换的盒 | 探测方向（源→目的） | 改前（§9.19/9.20 那轮同脚本同方向） | 改后（本轮） |
| --- | --- | --- | --- |
| CPE1 | cust12 `172.16.12.1` → `172.16.4.1` | 2744 发 / 0 FAIL | 2331 发 / 1 FAIL（单发，0.0s） |
| CPE2 | cust12 `172.16.12.1` → `172.16.3.1` | 2624 发 / 0 FAIL | 2324 发 / 0 FAIL |
| CPE2 | cust12 `172.16.12.1` → `172.16.4.1` | 未跑这个方向 | 2231 发 / 2 FAIL / 最长 1.0s |
| CPE3 | cust3 `172.16.3.1` → `172.16.12.1` | 231 发 / 65 FAIL / 63.8s —— **这一行不可用**：75 秒窗口只出 231 发（正常 ~2400 发），那一发探测期间链路本身就在大面积下 | 2252 发 / 3 FAIL / 最长 1.0s |
| CPE4 | cust4 `172.16.4.1` → `172.16.12.1` | 2575 发 / 3 FAIL / 最长 2.0s | 2335 发 / 3 FAIL / 最长 2.0s |

删窗口没有改变这一枪的形状：**中断来自 SIGTERM 到 watch 补回进程之间这台盒撤掉自己下发的 K 路由**，
改前改后一样是 1~2 秒；对端方向被 CPE1+CPE2 双播接住，所以 cust12 侧多数时候 0~1 发。

换件后的状态对比（判据不是退出码）：四台 `stat -Lc %i /proc/<pid>/exe == stat -c %i /usr/local/bin/sr-frr` 全 yes；
`local=1`、sr-frr `reported=1 injected=2 zebra_connected=True last_error=''`、K 路由各 18 条；
`vpp_main / sr-agent / sr-route / sr-fib` 的启动时间仍是 11-04~11-06 那一批（只有 sr-frr 是今天）；
`/status` 里 `export_frozen*` 两个字段消失。复跑：六方向 0% 丢包（rtt avg 11.0~14.1ms），
`vm-verify.sh` **392 PASS / 0 FAIL / 0 WARN**。

## 台的局限（登记，不掩盖）

1. **refill 12~18s 这件事本身是个缺口**，不属于本轮范围：它由"zebra 重新收录 K 路由" + sr-frr 1s 轮询
   两段串起来，正是 #276（收敛改事件驱动）要压掉的那两拍。本轮只删窗口，不动轮询。
2. **反向探测没能体现空窗**：`172.16.12.1/32` 是 CPE1 和 CPE2 同站点都在播的前缀，CPE1 那一份掉下去时
   CPE2 还在播，所以 cust4→172.16.12.1 只看到 1.0s 断流。这条方向不能当"代价小"的证据。
3. **没有量 VPP 重启那一枪**（窗口原本真正为它设计的那一拍：链路 down → zebra 在连接还活着的时候
   逐条发 withdraw）。它要让 sr-route / sr-fib / BFD 一起重来，是 #275 的现场判据；本轮判据只到"重连即重置"
   这条接缝，越界的那部分不在这里下结论。
4. 盒子之间、盒子与 Mac 之间没有对时，所以跨机只报**时长**不报绝对时刻（`reconnect-window.sh` 的注入时刻
   取自盒子自己的 `date +%s.%3N`）。

## 重跑

```
docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'
WORK=/tmp/p274 BIN=~/srvmlab/out-274/sr-frr.gz bash ../exp-273/push-binary.sh CPE1 CPE2 CPE3 CPE4   # 只落盘，不重启
ssh <盒> 'install -m 0755 /root/vmlab/artifacts/linux/sr-frr /usr/local/bin/sr-frr'                  # push 脚本不装，必须自己装
WORK=/tmp/p274 TAG=before bash reconnect-window.sh CPE1 cust12 172.16.12.1 172.16.4.1              # 旧产物时先量基线
WORK=/tmp/p274 TAG=after  bash reconnect-window.sh CPE1 cust12 172.16.12.1 172.16.4.1              # 换新产物后同口径再量
WORK=/tmp/p274 bash ../exp-273/window-swap.sh CPE1 cust12 172.16.12.1 172.16.4.1                    # 逐台换件 + 中断时长
TAG=274 bash ../exp-273/six.sh
bash ../../vm-verify.sh
```

原始读数不入仓库（`/tmp/p274/`：`samples-*.txt` 逐发采样、`probe-*.txt` 逐发 ping、`window-*.txt` 汇总），
重跑即生成。
