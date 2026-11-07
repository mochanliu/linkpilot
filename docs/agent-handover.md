# 接手这份项目：agent → agent 的把手

这份文件是给**另一个 AI agent** 写的，不是给人看的介绍。它假设你从来没碰过这个仓库，
目标是你在 30 分钟内能独立动手，而且不踩我们已经付过学费的那几处。

写在最前面的一条：本文所有数字都是 **2026-11-07 由前一轮 agent 实测**，每条后面都跟了复核命令。
**动手前先复核，不要相信这份文件本身** —— 它会过期，尤其是「哪一份代码在跑」那一节。
如果复核结果和这里不一致，以你测到的为准，并且把这份文件改掉（这是它的自我维护约定）。

---

## 0. 读这份文件的顺序

1. 先读仓库根的 `AGENTS.md`（50 行，全是硬规则，会在每个 agent 会话里自动加载）。它和本文冲突时，**AGENTS.md 赢**。
2. 再读本文 §2（现在这一刻的真实状态）和 §3（三条地雷）—— 这两节决定你第一条命令该敲什么。
3. 要改代码：读 §5（唯一合法的生效路径）+ §9（未决队列，owner 已经点名的活）。
4. 要下结论/交差：读 §6（读数口径）+ §7（测量陷阱）+ §8（验证基线）。

owner 的三条协作习惯，违反任何一条都会被退回，这里先说清楚：

- **先分析、后方案。** 他说过的原话："你不要每次都动不动就说解决方案是什么，因为你的分析得到我的认同前，
  你想的解决方案毫无意义。" 分析没被他点头之前，不要提交方案，更不要提交代码。
- **"修复"只指修软件缺陷，不指让 lab 看起来正常。** 不许用手工 `ip route`、手改 vtysh/FRR 配置、
  重启进程这类办法把现象压掉（他的原话："也就是说不允许手动方法修复"）。
- **controller 的每个增量先出方案、他点头后再写码；一次只推进一个缺陷。**

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
```

反向这条接缝上，**2026-11-07 起没有任何"等一拍"的窗口**：`Importer.Reset()`
（[controller/internal/frr/import.go:214](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/frr/import.go#L214)）
在与 zebra 重连的那一刻清空本地前缀集，新连接的回说什么就是什么。
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

`docker ps` 现在能看到 `lp-cpe1..4`、`lp-rr`、`lp-cust3/4/12`、`lp-term4`（已 Up 4 天）。
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

## 3. 三条必须先知道的地雷：在跑的产物 ≠ 盘上的产物

今天实测的四台完全一致的结论：**`sr-fib` 和 `sr-agent` 一重启或一开机重放，就会退回没有缺陷修复的那一份。**
这是当前环境里最贵的一处，因为它是静默的。

| 件 | 在跑（`/proc/<pid>/exe`） | 盘上（`/usr/local/bin`） | 后果 |
| --- | --- | --- | --- |
| `sr-route` | `5789c2c0f753529b` | 同值 ✅ | 一致 |
| `sr-frr` | `c8e731ddcbca7bd6` | 同值 ✅ | 一致（#274 那份） |
| `sr-fib` | `bab523ff3809f57b`（含缺陷⑦ 修复） | **`41c04d57fced05c0`（修复前）** | 重启即退回：VPP 重启后 sr-fib 不重连 API、隧道表只在内存 ⇒ 客户流量**永久黑洞**（缺陷⑦，任务 #268） |
| `sr-agent` | `d2ad8519648c031f`（含缺陷④ 修复） | CPE2/3/4 `f2e1461226db2dcd`（修复前）；CPE1 `32a1b26ada097227`（**来历不明**） | 重启即退回：VPP 重启后不重建 wg 隧道且仍报 `ok=true`（缺陷④，任务 #260） |
| `sr_wg_plugin.so` | VPP 只在**启动那一刻**加载盘上那一份 | CPE1 `9267d28d8b76fb54`；CPE2/3/4 `9d611883f8741999` | **盘上那份哈希对"正在跑的数据面"没有证据力**：四台盘上插件的 mtime 都比各自 VPP 的启动时刻晚 20,910–23,185 秒（约 6 小时），VPP 加载的是"上一份"文件。站得住的判据只有功能探针：`vppctl show sr-wg probe` 四台都能回话且 `echoed` 在涨（2 秒内 CPE1 +13、CPE2 +13、CPE4 +21）⇒ 在跑的那一代带探针。CPE1–3 的 `/tmp/sr_wg_plugin.so` 是 `9267d28d`（推了没装） |

复核（一条命令出全表，四台各跑一次）：

```bash
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do
  pid=$(pgrep -x $x | head -1)
  printf "%-9s run=%s disk=%s\n" "$x" \
    "$(sha256sum /proc/$pid/exe | cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x | cut -c1-16)"
done'
```

修复件在 Mac 上的落点（都已核对哈希）：

- `sr-fib` 缺陷⑦ 那一份：`/Users/louis/srvmlab/artifacts/linux/sr-fib.20261106-bab523ff` = `bab523ff3809f57b`；
  **注意同目录里那个不带后缀的 `sr-fib` 是 `2e1ceee1cd4c849e`，是第三份，不是它**。
- `sr-agent` 在跑的那一份：`/Users/louis/srvmlab/artifacts/linux/sr-agent` = `d2ad8519648c031f`（能取回）。
- 遥测插件：`/Users/louis/srvmlab/artifacts/plugins/sr_wg_plugin.so` = `9267d28d8b76fb54`（= CPE1 盘上那份）。
- 两处**取不回**：四台在跑的 `sr-route`（`5789c2c0…`）和 CPE1 盘上的 `sr-agent`（`32a1b26a…`）
  在 Mac 的产物树里没有任何匹配文件。想恢复只能从源码重新构建一份，那**不会**得到同一个哈希（见 §4）。

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
   字符串/符号比对也没用：盘上 `9d611883…` 与 `9267d28d…` 两份里 `sr-wg probe` 这个串都有（CPE2 上实测，命中各 1 次），
   而插件的命令/符号表是运行时才和 VPP 绑定的。**唯一可用的判据是功能探针本身**：
   `vppctl show sr-wg probe` 能回话、且两次采样（隔 2s）`echoed` 在涨，才说明"这一代在跑并且真在发探针"。
   想知道当前加载的是哪一份，只能拿 VPP 启动时刻去对那一时刻盘上文件的哈希（`stat -c %Y` + `/proc/<pid>` 的 etime，同一台机器上换算）。

顺带记两条正确的取证命令（都实测过）：

- 隧道口条数：`vppctl show interface | grep -cE '^wg'`（CPE1=2、CPE3/4=3）。
  `vppctl sr_wg_dump`、`vppctl show table`、`vppctl show wireguard interfaces` **都不是我们的命令**。
- `vppctl` 的参数**不要整体加引号**：`vppctl "show sr-wg probe"` 会回 `unknown input`，
  不加引号才通。这个坑今天差点让我写下"四台都跑着遥测前的插件"。

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

换件是**必然中断**操作（SIGTERM 到 watch 补回之间该进程不干活），AGENTS.md §1.4 要求点名 + 给实测时长。
#274 那次四台 sr-frr 换件的实测在 [test/lab/vmlab/exp-274/README.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-274/README.md)
（客户可见中断最长 1.0–2.0s，按方向不同）。

### 5.3 提交

```bash
git -c user.name='louis' -c user.email='louis@users.noreply.local' commit -m '...'
git push origin feat/vbond-control-plane
```

一次性身份，不动 git config；**只推 `feat/vbond-control-plane`，不推 master，不 force**
（master 历史里有密钥材料，那条线永远不推）。当前 HEAD = `1dc4ab2`（#274）。
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
我们实测的 refill 12~19s 是"zebra 重新收录 K 路由 + sr-frr 轮询"的合计（#276 要压的就是这两拍），
不是会话恢复时长，文档里曾经混过一次已经改掉了。

轮询现状的两个数不要记错：**代码默认 `poll_sec=2`**（`cmd/sr-frr/main.go:105`），
而 lab 盘上 `/etc/sr-frr.json` 与 `/etc/sdwan/fib.json` 都写的是 **`poll_sec: 1`**（2026-11-07 在 CPE1 实测）。
owner 说"sr-frr 轮询是 2s 一次，太慢了"指的是默认值；无论哪个，#276 的定案都是**改成事件驱动**，不是把秒数调小。

---

## 7. 测量陷阱（每一条都付过学费）

1. **盒子之间、盒子与 Mac 之间没有对时**（CPE1 的钟比 UTC 快约 10.7 分钟）。
   跨机只能比**时长**，不能比绝对时刻，文件 mtime 也不能横着比。注入时刻取盒子自己的 `date +%s.%3N`。
2. **Mac 的 BSD `date` 没有 `%N`**；`bash 3.2` 在大括号展开上有坑。跨机脚本别用它们。
3. **并发 `scp` 会静默截断文件**。产物推送一律串行，落地后 `sha256sum` 对一遍再算数。
4. **打流窗口放在管道子 shell 里会假绿**（"0ms 中断"就是这么来的）。计时要落在真正执行的那一侧。
5. **trace 只在收侧有意义**；转发到的包在收侧会被 punt，别把 punt 当丢包。
6. `vppctl` 参数别整体加引号（§4）。
7. **边跑边改被测脚本**会得到不可解释的数；脚本要么跑前定稿，要么重跑。
8. 原始读数不进仓库，落 `/tmp/<任务号>/`（`samples-*.txt` / `probe-*.txt` / `window-*.txt`），
   文档里只放汇总数字。

---

## 8. 交差的门槛（缺一件就别写"已验证"）

一个增量的完整交差 = ① `lp-ctrl` 里 `go build/vet/test` 全绿；② 改前/改后**同一口径**的两组数字；
③ `bash test/lab/vmlab/vm-verify.sh` 到 392/0/0；④ `TAG=xx bash test/lab/vmlab/exp-273/six.sh` 六方向 0%；
⑤ 换过产物的话，§5.2 第 ④ 步那条 inode 判据 + §3 那张哈希表更新；⑥ 文档里点名必然中断与时长；
⑦ 提交推分支 + 给登录把手。

UI 类改动另加一条：**必须在真浏览器里点过并截图**，不能只看 HTML 源码下结论。
控制台是单文件 `controller/cmd/sr-ctrl/console.html`，**改完必须 `node --check` 里面的脚本**。

---

## 9. 未决队列（owner 已经点名的活）

按 owner 拍定的顺序，**一次只做一件**：

| 任务 | 状态 | 一句话现状 |
| --- | --- | --- |
| **#275** `resolvable` 改吃活动隧道 | 待开工，**先出方案** | 现在的门是配置的：`reachableLocked()` 只看隧道表 `controller/internal/route/client.go:981`，隧道实际死了也算可达 ⇒ 这才是根因。可达性粒度已拍**严格 fail-closed：无 BFD 信息即不可达**；接 GoBGP Nexthop Tracking 的形状，hold-down 先行 |
| **#276** 收敛改事件驱动 | 待开工，**先出方案** | 目标是把 §6 末尾那个 12~19s 的 refill 压成瞬时：`sr-fib` 与 `sr-frr` 都按 `poll_sec` 轮询（`cmd/sr-fib/main.go:473`、`cmd/sr-frr/main.go:807`；默认 2s、lab 盘上配的是 1s），owner 原话"sr-frr 轮询是 2s 一次，这个太慢了，必须像 frr 重分发那样瞬时" |
| #272 跨盒回灌 | 待拍 | 注入的 K 路由进了客户 area0 后被别的盒当 `local` 上报 |
| #262 缺陷⑥ | 待拍 | 亚秒抖动被放大成 ≥1s 客户黑洞（BFD 150ms 判定 + sr-fib 1s 轮询直接摘路，无抑制） |
| #269 缺陷⑧ | 待拍 | 开机清单与 watch 并发，同一秒把 sr-route/sr-fib 各起两个（拟加互斥） |
| #268 / #260 | **代码已修，产物没装全** | 就是 §3 那两条地雷：bab523ff 在跑、盘上是修复前。修的是产物一致性，不是代码 |
| G-2 隧道表落盘 | 只出了方案（`3a09f03`） | 隧道层磁盘恢复不了 ⇒ §2.5 那个 `sr-box` exit 1 |
| NAT 阶梯 #222 + M-4/M-1/M-3/M-5、#211/#213/#244/S8 | 挂着 | 顺序 M-4→M-1→M-2→M-3 |

历史与决策的权威出处（要引结论去这里引，别信二手转述）：
[docs/vmlab-rebuild-plan.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vmlab-rebuild-plan.md)（增量台账，§9.19/§9.20/§9.21 是最近三节，§9.21 作废了前两节里"窗口保留"的说法）、
[docs/loop-test-report.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/loop-test-report.md)（客户侧成环实验，P-3 已标注否决）、
`test/lab/vmlab/exp-27{1,3,4}/README.md`（实验台本，每个目录的 README 就是那轮的判据与局限）。
`/Users/louis/srvmlab/MANIFEST.md` 是产物台账，但它会滞后 —— 今天发现它把"在跑的进程就是盘上这一份"当成已核对过，**这条现在是假的**，以 §3 为准。

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

---

## 11. 把手（登录与凭据的位置，值不进仓库）

- 控制台：`http://127.0.0.1:8088`（Mac 本机）。运营方令牌文件：`/Users/louis/srvmlab/ctrl/data/console-token`。
  PKI/状态同目录：`ca.key` `ca.pem` `server.key` `server.pem` `state.json`。
  **这些值一个都不许进文件/日志/提交信息**（AGENTS.md §3）。租户账号口令向 owner 当场索取，我们只在对话里传。
- 设备 API：`0.0.0.0:8444`（TLS）；STUN：`3478`/`3479`；RR：`18446/19446`（rr1）、`18447/19447`（rr2）。
- 盒内：`ssh CPE1|CPE2|CPE3|CPE4|cust12|cust3|cust4`（走 `~/.ssh/config` 别名，直连 IP 被拒）。
- 构建：`docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'`。
- 远程仓库：gitee，只推 `feat/vbond-control-plane`；当前 HEAD `1dc4ab2`。

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
