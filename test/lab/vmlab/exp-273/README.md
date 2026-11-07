# exp-273：#271 的注入闸门撤掉，环路改由配置解决（owner 定性）

对应 `docs/vmlab-rebuild-plan.md` §9.20（并更正 §9.19）。

owner 的原话是这一轮的范围：

> 那就是我说的路由环路造成，这个并不该像你现在这样处理。应该由客户通过配置来解决：
> 在 CPE1 和 CPE2 上设置只对 rr 播自己客户侧的路由。这个环路场景该由配置解决，而不是代码解决。
> ……"一、你点名的那个现象，实际是两件事"我是指把这一条回滚。你修的其他缺陷不用。

所以这一轮做的只有两件事：
1. **只回滚 A1（注入闸门）** —— `Plan.Withheld` / `Importer.OwnedByCustomer` / `/status` 的 `local_withheld`
   三处删除；**A2 链路事件乐观窗口、A3 读先于写 + 首轮冻结原样留着**（`linkRecovery` / `BeginLinkWindow` /
   `Settled` / `export_frozen` 全在）。单测从 8 条变 6 条（随闸门删掉两条）。
   > 这一句是 #273 当时的范围。**A2/A3 随后在 #274 整族撤回**（`linkRecovery`/`BeginLinkWindow`/`Settled`/
   > `export_frozen*` 现在都不在代码里了，只留 D「读先于写」的次序与 E 的 `touch` 重放），
   > 见 [../exp-274/README.md](../exp-274/README.md) 与 `docs/vmlab-rebuild-plan.md` §9.21。
2. **走产品路径把那条配置加上** —— 控制台「配置 > overlay > 路由泄露」里 node1/node2 的
   `CPE-SDWAN`（`to_sdwan`）行改指一个只含 `172.16.12.1/32` 的 prefix-list，显式发布、agent 推送、
   sr-apply 第 5 段写 `/etc/sr-frr.json`、sr-frr `PUT /config` 热更。**全程零手工补丁**：不 `ip route`、
   不改 vtysh、不重启任何进程。

## 脚本（只有两个会动状态，都点名）

| 脚本 | 量什么 | 会不会动状态 |
| --- | --- | --- |
| `box-layers.sh <盒>` | sr-frr `/status`（含 `injected/reported/skipped/frozen`，A1 撤掉后不再有 `local_withheld`）+ 注入账本 `state.json` + sr-route `local/rib/dropped_own_route` + 落盘 `/etc/sr-frr.json` 第 5 段 + 内核 main 表 + zebra 视角 | 只读 |
| `six.sh`（`TAG=xx` 跑） | §9.7 六方向 `ping -c 50 -i 0.2 -W 1 -I <源 loopback>` | 只读 |
| `window-swap.sh <盒> [探测机] [源] [目的]` | 换件这一枪的客户可见时长：秒级探测中途 SIGTERM sr-frr，由产品开机清单补回来 | **会 SIGTERM sr-frr** |
| `window-cfg.sh <标签> <条目\|RESET>` | 配置热更这一枪的客户可见时长：探测中途改 prefix-list 条目并重新发布 | 会改 lab 配置（走产品路径） |
| `apply-config.sh` | 态 C 的那一次配置：建 prefix-list → 改 node1/node2 的 `to_sdwan` 行 → 预览发布 → 真发布 | 会改 lab 配置（走产品路径） |
| `push-binary.sh [盒…]` | 一次性换件：scp gzip → sha256 核对 → `install -m 0755`，不重启 | 写文件，不重启 |
| `../exp-271/soak-271.sh` | 240s 内每 20s 断言：每台自己的客户前缀不在注入账本里、zebra 里不是 `Known via "kernel"`。A1 撤掉后这一条的口径变成「账本里出现自己的前缀 = 控制台那一行播了别人站点的前缀」，改配置不改码 | 只读 |

会话 cookie（`cj.txt`）不入仓库，`WORK`/`CJ` 环境变量指到你自己那份；逐发 ping 时间戳
`probe-*.raw`（每份 ~50KB）也不入库，可由 `window-swap.sh` / `window-cfg.sh` 重跑生成。

## 三态读数（`raw/`）

| 态 | 码 | lab 配置 | 六方向 | sr-frr 读数 | 全 lab 判据 |
| --- | --- | --- | --- | --- | --- |
| **A** 上一轮交付 | A1 闸门 + A2 + A3（`8b15abd5…`） | `to_sdwan` 白名单全放 | **6/6 0%**（`six-A.txt`） | CPE1 `local_withheld=["172.16.3.1/32"]`、`reported=2`；CPE3 `local_withheld=["172.16.3.1/32"]`（`A-before-swap.txt`） | 384 PASS / 8 FAIL（§9.19） |
| **B** 只撤 A1 | 只剩 A2 + A3（`9f28db6c…`） | 同 A，一字没改 | **4/6 100% 丢包**（`six-B.txt`） | CPE3 `injected=2 reported=0`、`local` 只有 `172.16.3.1/32`；CPE1 `reported=2`、`local` 里混进 `172.16.3.1/32`（`B-layers.txt`） | 未跑（形状已由六方向与四层锁定） |
| **C** 撤 A1 + 那条配置 | 同 B | node1/node2 `to_sdwan` → 只 `172.16.12.1/32`，发布并热更 | **6/6 0%**（`six-C.txt`；RESET 后 `C-final-six.txt` 有两次 10%，`C-reset2-six.txt` 起回到 6/6 0%，见下） | 四台 `injected=2 reported=1`，`local` 各自只有本站前缀，`import_rules` 每台 `prefixes:1`（`C-final-layers.txt`） | **392 PASS / 0 FAIL / 0 WARN**（`verify-C-final-summary.txt`） |

A→B 是同一条锁死链原样复现（证明闸门确实在兜这件事），B→C 只动了配置、码一字未改再回来
（证明这条环路是配置问题）。

## 两个窗口的实测（AGENTS.md §1 第 4 条）

* **换件那一枪**（SIGTERM sr-frr + watch 补进程，VPP/FRR/agent/sr-route/sr-fib pid 全程不变）：
  CPE1 **0 FAIL / 2744 发**，CPE2 **0 / 2624**，CPE4 **3 / 2575（最长断流 2.0s）**；
  CPE3 **65 FAIL / 231 发，3.3s + 63.8s（探测窗口结束时没闭合）** —— 这**不是**换件造成的中断，
  是 B 态那条锁死正在复现：换新二进制不会自己解开，因为被 K 路由顶掉的那条客户路径已经不再发通知，
  闸门撤掉后没有任何代码会在下一次对账时把它认回来（§9.19 第九节第 1 条在 C 态依然成立，只是解法换了）。
* **配置热更那一枪**（`PUT /api/prefix-lists` + `republish apply:true`，hash `426e2bf8e7c8 → 3f8351920e3d → 426e2bf8e7c8`，
  两台 pid 不变）：加条目 **0 FAIL / 2699 发**，回退 **0 FAIL / 2711 发**（`window-cfg-加条目.txt`、`window-cfg-回退.txt`）。
  **这一枪的客户可见时长是 0。**

## 已知尾巴（登记，没掩盖）

1. **RESET 之后 `raw/C-final-six.txt` 里 cust12→172.16.3.1 与 →172.16.4.1 各 10%（50 发里丢 5）**，
   出现在窗口探测结束（0 FAIL）之后约 15s；`C-reset2-six.txt`（90s 后）与随后单方向手工复测都是 0%。
   **未归因**：不像配置那一枪（75s 连续探测 0 FAIL），更像 0.2s 间隔的 50 发连打撞上了缺陷⑥（#262，亚秒抖动被放大）
   或宿主调度。下一轮要取证请在同一秒同时抓 `sr-fib` 摘路事件与 BFD 翻转。
2. **CPE1 的 `dropped_own_route=230` / CPE2 `14023` 仍在涨**：两台都把 `172.16.12.1/32` 播进 overlay，
   这是"两设备同站点多源"的正常形状，不是环路。
3. **跨盒回灌（#272）没动**：CPE1/CPE2 的 ospfd 仍是 `redistribute kernel route-map SDWAN2CPE-vpn0` 且该 route-map 无 match（全放），
   任何一台注入的 K 路由都会进客户 area0。C 态之所以干净，是因为每台只播本站前缀 ⇒ 别盒学回来的都不是"自己的"，
   也就不会吃掉自己的所有权；**但只要客户在 `to_sdwan` 那一行播了别人站点的前缀，B 态就会重现**。这条策略要不要在导出侧收、
   规则归谁配，仍待拍。
4. CPE2 的 VPP 插件哈希落后（`9d611883…` vs 现产物 `9267d28d…`）与 `make test-frr` 测不到东西（§9.19 第十节、第九节第 4 条）本轮都没碰。

## 怎么重跑

```
docker exec -w /workspace/controller lp-ctrl go test -count=1 -v -run 'Gate|Window|Frozen|Settled|Plan' ./internal/frr/
                                                 # 6 条单测：链路窗口 + 读先于写/首轮冻结 + PlanFrom
WORK=/tmp/p273 BIN=/path/to/sr-frr.gz bash push-binary.sh            # 只推产物 + install（不重启任何东西）
WORK=/tmp/p273 bash window-swap.sh CPE1 cust12 172.16.12.1 172.16.4.1 # 换件那一枪的客户可见时长
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash apply-config.sh              # 态 C 的那次配置（走产品路径）
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash window-cfg.sh 加条目 192.0.2.0/30
CJ=/tmp/p273/cj.txt WORK=/tmp/p273 bash window-cfg.sh 回退 RESET     # 收口必须跑这一步
for B in CPE1 CPE2 CPE3 CPE4; do bash box-layers.sh $B; done
TAG=final bash six.sh
bash ../exp-271/soak-271.sh
bash ../vm-verify.sh                                                # 392 PASS / 0 FAIL / 0 WARN
```
