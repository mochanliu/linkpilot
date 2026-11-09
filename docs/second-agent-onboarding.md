# 第二个 agent 加入这套工程：方案

不是把 `AGENTS.md` 复制一份给对面。是回答四个具体问题：
1. 那个会话起来后要读什么、跑什么命令，才算"接上手"；
2. 两条流水线上的活怎么切，才不互相踩；
3. 需要互相知会的时候走哪条路；
4. 出事（对面死在半路、锁没释放、lab 状态被单方面改了）怎么退回干净态。

前提：`AGENTS.md` + `docs/agent-handover.md` 已经是**共同法源**。本文只补
"如何把它交给第二个具体会话"这一步，不复述规则本身。

**本文 2026-11-09 重写过一遍。** 上一版是 #284 抬盒之前写的，里面三处已经作废：
队列那三件事（sr-route 抬 HEAD / #275 / #276）全做完了、"动手前先申请窗口"被 owner 撤了、
运营方登录方式也变了（这套 VM 台里没有 `louis-ops` 这个账号）。下面全部按当前事实写。

---

## 0. 现在这一刻（先读这节，再读别的）

**a) lab 处于冻结令之下。** owner 2026-11-09 在 #285 交付之后的原话：
「我自己测一下再说，lab 先放那里不动。」这条写在
[`docs/agent-handover.md`](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/agent-handover.md) **§10.1**，
是那份文件里优先级最高的一条。含义要说全，不能只理解成"别写"：

- **写一律禁止**：故障注入、换件、改配置发布、`PUT /config`、跑 `vm-fib-sync.sh`、
  带 `ESCAPE=` 跑任何脚本、重启进程、重建 lab，全部不许。
- **只读也不必跑**：`vm-verify.sh`（约 4 分钟）、`exp-273/six.sh`、`exp-273/box-layers.sh`
  会占 CPU、打流量、动计数器。owner 正在自己量主备切换，**任何额外负载都会污染他的读数**。
  "只读所以随便跑"这条在冻结期不成立。
- **解冻只有 owner 明说才算。** 第二个 agent 上车第一件事是**问**这条冻结令还在不在，
  不许拿"窗口审批已撤"当解冻依据 —— 撤的是常设审批，冻结是针对这一次实验的单独命令，两件事。

**b) 队列里那三件事已经做完，别再去领。** sr-route 抬 HEAD = #284（四台十二件 run==盘）；
#275 = `resolvable` 改吃活动隧道（单测 6 条 + 随 #284 跑过 e2e）；
#276 = 事件驱动收敛（切片 A+B+C，外加 B3/B4 两条硬前置，都收口了）。
现在真正没做的是 §3 那张表里的东西。

**c) lab 现在的形状不是 #284 提交时的基线。** #285 那一枪把它改成了**有意的主/备形状**：
t13（CPE2↔CPE3）颜色 0→2、四台逃生开关全 true。这是 owner 要亲手测的现场，**不许还原**。
还原配方在 [`test/lab/vmlab/exp-285/README.md`](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-285/README.md) §五，
写在那里是为了以后有人问"怎么退回去"，**不是为了现在执行**。
判据基线也跟着换了口径：`vm-verify.sh` 现在是 **PASS=398 FAIL=0 WARN=0**（旧口径 392，
差额 +6 的逐条账在 handover §2.4 —— 不是产品变好了 6 条，是口径换了）。

---

## 1. 起来就先接手的三份东西（顺序，别跳）

三份都在同一个仓库里，绝对路径如下。**让 owner 在第二个会话的工作区挂载里把仓库根纳入进来**，
Qoder 会自动加载项目根 `AGENTS.md` 到会话上下文；这一步不做，后面全废。

| 顺序 | 绝对路径 | 它是什么 | 重点读哪几节 |
| --- | --- | --- | --- |
| 1 | `/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/AGENTS.md` | **法**。四条硬规则，与任何文档冲突时它赢 | 全部（50 行） |
| 2 | `/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/agent-handover.md` | **现场**。哪份产物在跑、改动的唯一合法路径、测量陷阱、冲突协议 | **§10.1 冻结令**、§0.1 上车四条命令、§2 现在这一刻、§3 run/disk 台账、§5 合法生效路径、§7 陷阱、§9 未决队列、§10 冲突协议 |
| 3 | `/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/queue-plan-next.md` | **任务的落纸方案**。注意标题还写着那三件事，但它们已完成，看每节的"落地实况" | §二/§三 落地实况（已完成的判据长什么样）、§四（#285 的三条结论） |

第四份按需读，不是上车必读：
`/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-285/README.md`
—— owner 现在要测的就是这一枪留下的形状，读它是为了**别把它弄坏**，不是为了复现。

**顺序为什么这样**：AGENTS 是法、handover 是现场、queue-plan 是任务。前两份不读完就开始改代码，
必然撞 §1 冷启动禁令或者 §10 冲突协议。**任何一条都不许先动再补读。**

### 1.1 上车的复核命令（handover §0.1 那四条，不是 §11）

```bash
git log --oneline -5 && git status --short                       # HEAD 与别人留下的散工
VBoxManage list runningvms                                        # 应是 7 台：CPE1-4 cust12 cust3 cust4
ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep # 控制面在不在 Mac 上（§2.1）
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do pid=$(pgrep -x $x|head -1);
  printf "%-9s run=%s disk=%s\n" "$x" "$(sha256sum /proc/$pid/exe|cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x|cut -c1-16)"; done'            # §3 那张表还成立吗
```

**冻结期只跑第 1、2、3 条**：git 与 ps 是 Mac 本地，`VBoxManage` 只问宿主一句，都不进盒、不打流量、
不动计数器。**第 4 条要 ssh 进 CPE1 读 `/proc` 与算哈希，属于"动了对面的现场"**，冻结期先问 owner 再跑。

跑完把输出**贴到 owner 的当前会话里** —— 不是贴给自己，是让 owner 看见两边读到的现场一致。
第 4 条如果哪一行 `run != disk`，先别当成缺陷：CPE1 的 `sr-fib` **就是有意停在 `a9f1bc8c`**
（事件版、没带 B3），那是 #285 的对照臂，抬上去就没有对照了。详见 handover §3。

---

## 2. 会话与工作区的具体开法

Qoder 侧（owner 视角）：
1. **同一个仓库工作区，再开一个 Qoder Desktop 会话**（不要另开一份 clone —— 那样两边 `state.json`、
   `~/srvmlab/artifacts/` 与 VirtualBox 是同一套但 git 树不同步，会打脏）。
2. **工作区附加目录**保持与第一个会话相同：仓库根 + `/Users/louis/srvmlab/artifacts/` +
   `/Users/louis/srvmlab/out-<N>/`（后者让每个 agent 用**自己任务号**目录，天然分开）。
   现有目录已经有 `out-274`、`out-276` 等，新会话别复用别人的号。
3. 会话命名建议 `sdwan-<role>`：`sdwan-dataplane`、`sdwan-controlplane`、`sdwan-console`，
   owner 一眼能看出谁在管哪一段。名字不重要，重要的是**owner 侧留一份"这个会话现在在跑什么"的清单**。

**agent 侧第一个动作**：读完 §1 那三份 + 跑 §1.1 允许的那几条 + 在 owner 会话里贴这一句：

```
接手完成。
  HEAD = <git log 第一条>
  工作区散工 = <git status --short 原文，没有就写"干净（除 .out* 构建目录）">
  冻结令 = 我读到 handover §10.1 说 lab 冻结，请确认现在还在冻结期吗？
  四台 run==disk = <跑了第 4 条就贴原文；没跑就写"冻结期未进盒，待解冻后复核">
  我打算领 = <#N>，理由是这件在冻结期能靠代码 + 单测收口 / 这件必须动 lab，我等解冻
  合流点 = <这条动不动 internal/route/proto 或 vppapi；动就点名>
```

这一句就是"打招呼"，owner 或者第一个 agent 看见这条就知道有人上车了。
**"冻结令"那一行不许省** —— 它是本文 §0(a) 的落地形式，问一句的成本远低于把 owner 的读数打脏。

**不推荐的做法**：
- 用 Agent 工具 spawn 一个 read-only 子代理做这件事 —— 那个是"临时专家"，几分钟就散会，
  而且这类子代理是只读的；第二个 agent 加入是要**跨会话**、要能 commit push 的常驻会话。
- 让第二个 agent 直接读第一个会话的 `MEMORY.md` —— 那是**私有记忆**，
  里面的偏好和上下文对第二条会话只是参考，不是权威。权威只有 `AGENTS.md` + `agent-handover.md` + 代码。
- 让第二个 agent 一上来就跑 `vm-verify.sh` "先看看现状"。冻结期这就是污染现场，见 §0(a)。

---

## 3. 任务怎么切（默认切法 + 什么时候合流）

**按数据面/控制面切**，两边交集只有 `controller/internal/route/proto`（v3）与 `vppapi` 结构。

| Agent A（当前这个） | Agent B（要加入的） |
| --- | --- |
| 控制面 Go：`controller/internal/**` + `controller/cmd/sr-*`（sr-route/sr-fib/sr-frr/sr-apply） | 数据面 C：VPP 插件 `vpp-plugin/**` + `vppapi` Go 客户端 |
| 控制台（`controller/web/console.html` 与配套 Go 后端） | 内核/驱动侧：linux-cp / linux-nl / af-packet 的形状 |
| 协议：route v3、config bundle 五段 | 遥测数据源：BFD/probe 在 VPP 内的实现 |

这张表是**默认归属**，不是禁令。真正决定谁领哪件的是下面这张"冻结期能不能收口"表 ——
因为现在 lab 不许动，一件活的 e2e 判据跑不了，就只能停在"代码写完、单测绿、e2e 待解冻"。

### 3.1 剩下的活（权威出处：handover §9 + 各专项方案）

| 任务 | 内容 | 归谁 | 冻结期能做到哪一步 |
| --- | --- | --- | --- |
| **M-4**（#227） | 遥测行加 `TunnelID`（+`ReachReport` 加或删）。方案在 `docs/nat-tunnel-ladder-plan.md` §9.5，是 NAT 阶梯 #222 的**硬前置** | **B（推荐第一件）** | **能收口**：改动面只到 wire/agent/控制器/控制台，纯 Go + 单测 + harness，不需要动 VM。**但它是协议改**，见 §3.2 |
| **M-1**（#228） | `path_id` 每隧道唯一（`PathIDBase` 不再恒 1000） | B | 代码 + 单测能写；**判据必须动 lab**（它是唯一会静默改在线转发的一条），e2e 待解冻 |
| **B2** | 三条口共用一套 transport identity（#276 切片 B 留下的唯一缺口） | A | 代码 + 单测能写；e2e 待解冻 |
| **#262** | 亚秒抖动放大 ⇒ hold-down。挂载点已经有了：`cmd/sr-fib/watch.go:320` 的 `signal()`（B3 那条） | A | **半个能收口**：hold-down 的状态机 + 单测可以写完；抖动注入判据必须动 lab。**语义要 owner 先拍**（抑制多久、抖回来算不算一次） |
| **G-2** | 隧道表落盘（方案已出，提交 `3a09f03`）：CPE2/3/4 的 `/etc/sdwan/fib.json` `tunnels` 实测 0 条，sr-fib 一重启这张表就空 | A | **要 owner 先拍**四条待决；拍完能写代码 + 单测，落盘判据待解冻 |
| **#272** | 跨盒回灌：注入的 K 路由进客户 area0 后被别盒当 `local` 上报 | A | **要 owner 先拍语义**，拍之前不许写码 |
| **M-3 / M-5 / #222** | pair 门禁放宽（同设备对 ≤8 条、颜色可重复）/ 判据收紧成 (local, peer) / NAT 四步阶梯 | B | M-3 **必须排在 M-2 之后**（M-2 已落地）；M-5 和 M-4 一批做几乎免费；#222 等 M-4 |
| **#211 / #213** | 环路场景 C-G 故障注入矩阵 / R-5 复跑 `sr-agent SIGSTOP` 反证 | A | **冻结期完全做不了**：这两件的本体就是故障注入 |
| **#244 / S8** | 交 owner 复核 ⇒ owner 关 docker | **owner** | 两个 agent 都不动 |
| 注释修正 | `controller/internal/fib/fib.go:314-315` 那句"down 顶上、up 回落都是原生行为"过度概括了 | A | **能收口**（改注释 + 文档），已登记在 `queue-plan-next.md` §四 第 3 条 |

**给 B 的建议顺序**：M-4 → M-5（一批）→ M-1 → M-3 → #222。
理由：M-4 是阶梯的硬前置且冻结期能真收口，M-5 搭它的车几乎免费，
M-1/M-3 的判据都要动 lab，排在解冻之后正好。

### 3.2 合流点：只有一处 —— 协议改

任何动 `controller/internal/route/proto` 或 `internal/vppapi` 结构体的增量，都必须在 owner 会话里点名
"这条动了协议，另一边要跟着改"，让 owner 拍是否两个会话一起合入。
**M-4 就是这种**（`TunnelID` 要进 wire 的遥测行），所以 B 的第一条消息里就要把它标出来，
不能等写完了才说。A 这边同期在动 `internal/fib` 与 `internal/route`，撞车面是真实存在的。

除此以外的文件级冲突（同一个 `.go` 都被改）按 handover §10：**先动的 agent 提交，后动的 rebase**。

---

## 4. 谁可以改 lab 现状（VirtualBox 7 台 + 控制器 8088）

`agent-handover.md` §10 那张表说的是**规则**，这里给**流程**。但流程分两个状态，别混：

### 4.1 冻结期（现在）

**没有流程可走，因为答案是"不许"。** 想动 lab 的唯一正确动作是问 owner 一句"还在冻结期吗"，
然后等回答。不要发知会模板、不要建锁文件、不要"我先只改一台应该没事"。
冻结期能做的活见 §3.1 最后一列：读代码、写方案、写单测、`lp-ctrl` 容器里编译、改文档。

### 4.2 解冻之后（owner 明说解冻才切到这一节）

owner 2026-11-09 的原话把**常设窗口审批撤了**：「lab 是你研发测试用的，不存在窗口概念，
你随时想动都可以动，除非我明确说不动。」所以不需要申请时间窗。仍然要做的是三件事：
① 改动走产品路径（控制台 → 发布 → agent → apply），不许手工 `ip route` / 手改 vtysh；
② 在实验 README 里写清"改了什么 / 怎么还原"（exp-285 §五是样板）；
③ 有第二个 agent 在场时别打脏对方正在量的基线。**生产盒与别人现场的 lab 不在这条豁免里。**

**动手前的知会模板**（发到 owner 会话，不是发到对面 agent。这不是审批，是让对方有机会说"我正在量"）：
```
[Agent-B] 要动 lab：
  - 目标：CPE3（一台），做什么、为什么
  - 判据：前后各一次 vm-verify.sh + exp-273/six.sh，数字会写进提交说明
  - 还原：改了什么、怎么退回去（会写进实验 README）
  - 对面如果在跑 exp-*，先停一下
  - 完事我发一条"释放"
```

对面 agent 收到（通过 owner 转发，或者 owner 直接问一句）：
- 没在跑对基线敏感的活 ⇒ 回"OK，我这边不冲突"；
- 正在跑 ⇒ 回"我这轮跑完 X 之前不要动"；
- 什么都不回 = 视为"对面在动"，等 owner 仲裁。**没有隐式默认。**

**只读判据可以并行跑**（解冻后）：`vm-verify.sh`、`vm-status.sh`、`exp-273/box-layers.sh`、
`exp-273/six.sh`。真实绝对路径，别猜：

```
/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/vm-verify.sh
/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/vm-status.sh
/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-273/box-layers.sh
/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-273/six.sh
```

注意 `box-layers.sh` 与 `six.sh` 在 **`exp-273/` 子目录下**，不在 `vmlab/` 根 —— 上一版本文写错过。
全套重建的入口是 `test/lab/vmlab/bringup-mac.sh <盒> [phase]`（Mac 侧驱动，推产物进盒后跑
盒内 `vm-bringup.sh` 的 A–G 段），**那是 owner 的活**，中途 lab 完全不可用。

### 4.3 写路径的告示牌锁

`/run/sr-frr/control.sock`、`/run/vpp/api.sock`、控制器 `state.json` 这三个物理上就是单点。
AGENTS.md §1 + handover §10 已经写了"一次只让一个 agent 持有写盒内的锁"。落地形式建议：

谁准备写盒内，就在 **`/Users/louis/srvmlab/locks/<box>.lock`** 建一个文件，里面写
`agent=<name> task=<#N> started=<iso8601>`，写完删掉。**这个目录现在还不存在**（没人建过），
第一个要写盒内的 agent 负责 `mkdir -p` 建它。放在 Mac 侧而不是盒内，是因为两个 agent 都在同一台 Mac 上跑，
这样两边 `ls` 看到的是同一份；**不是** flock（flock 只在同主机有效，而且没人保证对面会去看）。
就是一个"看一眼谁在里面"的告示牌。进盒前 `ls /Users/louis/srvmlab/locks/`，看见别人的名字就先别动。
落地成本 = 一行 `echo`，收益 = 不会两边同时 `PUT /config`。

---

## 5. 出事怎么办（三条最常见的）

**a) 对面 agent 死了**（credit 用尽 / Qoder 崩了 / 会话被 owner 关了）
- 现场：`/Users/louis/srvmlab/locks/` 里那个告示牌没人删。
- 处理：owner 或者留下的 agent 直接把那个 `.lock` 删掉，同时在 owner 会话里说一句
  "对面死了，我接手它当时在做的 <#N>"。**不要静默接手** —— 现场里的中间状态
  （比如刚 scp 了一半的产物、`run != disk`）需要 owner 知情。
- 预防：任何 agent 领一个活的时候，第一句就是 `agent=X task=#N 现在开始，锁在 <box>`；
  owner 会话就是日志，出事回查。

**b) `docs/agent-handover.md` 被两条会话同时改**
- 现场：git 层面看是普通 merge conflict。
- 处理：谁后提交谁 rebase，rebase 时**读**对面那段而不是无脑 `--ours`。
  handover 里的每一条数字都要重新核（对面写的"sha 是 xxxxxxxx"你要么在盒上跑一次确认，
  要么改成"待复核"，不许直接采信也不许直接删）。**冻结期没法在盒上核 ⇒ 就写"待复核"**，
  别为了消冲突把别人的数字删掉。
- 预防：AGENTS.md §4 说"做完一个增量，如果它改变了那份文件的任何事实，在同一个提交里改掉它"——
  这就是让 handover 的写入跟着代码增量走，两条会话改的是**不同段落**（因为不同增量），冲突面就小了。

**c) lab 被改脏，两个 agent 的基线数字打架**
- 现场：A 刚量的 refill 12s，B 说是 19s，两边都不是错。
- 处理：**先停手，报 owner**。回到干净态的入口是 `test/lab/vmlab/bringup-mac.sh` 全套重来，
  但那是 owner 的活（重建 5–10 分钟且中途 lab 完全不可用），**agent 不许自己决定重建**。
  而且现在 lab 是 owner 要亲手测的主备形状，重建等于毁掉他的现场。
- 预防：每次动手前跑一次 `vm-verify.sh`，量出来"和上次一样"再走。当前门槛是 **398/0/0**，
  不是 392；差得远那大概率不是自己的改动，是 lab 脏了或者口径看错了（handover §2.4 有 +6 的逐条账）。

---

## 6. 提交与推送

- **一条分支**：`feat/vbond-control-plane`，两个 agent 都往这一条推。不新建分支 ——
  新建两条 agent 分支再 merge 是给自己找冲突。
- **一条底线**：`AGENTS.md` §3 说不 force push 不推 master（master 历史里有敏感材料，那条线永远不推）。
  这条对两个 agent 都成立。
- **一次一提交**：每个 agent 一个增量一个提交，不要攒。
- **不 `git add -A`**：工作区里现在有六个未跟踪的构建目录
  （`.out/`、`controller/.out-b3/`、`.out-b4/`、`.out-p273/`、`.out-p276/`、`.out-sr-frr-reverted/`），
  `-A` 会把它们和别人的散工一起带进去。**按文件名 stage**，提交前 `git status` 看清包含了什么。
- **产物放自己任务号的目录**：`/Users/louis/srvmlab/out-<N>/`，不要覆盖 `artifacts/linux/` 里别人的文件。
  编译前先 `git status --short` + `git rev-parse HEAD`，记下你构建时的树。
- **提交信息里必带**：动了什么文件、为什么、量了什么数字（改前 / 改后）、有没有影响 lab 现状。
  `agent-handover.md` 是共同法源，写"见 handover §X.Y"没问题，但不许用"照 handover"糊掉数字。
  **冻结期的增量没有 VM 台数字，就如实写"e2e 待解冻"**，不许拿旧数字冒充本轮实测，
  也不许写"应该没问题"。单测条数、`go test ./...` 结果、harness 条数这些 Mac/容器侧能拿到的照写。
- **git 身份**：都走一次性 `-c`，别改本地 config：
  ```
  git -c user.name='louis' -c user.email='louis@users.noreply.local' commit -m "..."
  ```
- **`lp-ctrl` 容器里编译是冻结安全的**（它不碰 VM 台），所以冻结期照样能出产物、跑单测。
  但**把产物 scp 进盒 = 写 lab = 禁止**。

---

## 7. 什么**不**给第二个 agent（以及登录把手怎么拿）

- **口令不进文档、不进仓库、不进日志、不进 memory。** 这是 AGENTS.md §3 与 owner 的明确要求。
  但 owner 同时要求"每次交付完主动给账号口令，让他自己能登进去点一遍" —— 这两条不矛盾：
  **口令只在对话里传**。第二个 agent 要登控制台时向 owner 要，owner 现场给；不要自己去仓库里翻，
  也不要把它写进任何文件。
- **这套 VM 台的登录事实**（2026-11-09 核过。上一版本文写的 `louis-ops` 在这里**不存在**，
  那是旧容器 lab 的账号，别再照着找）：
  - 控制台 `http://127.0.0.1:8088`（转发到 lab-ctrl:8080）。
  - `state.json` 里**只有一个账号**：`vm-tenant`，角色 tenant，租户 `tenant-vm`。
    脚本读口令的文件是 `/Users/louis/srvmlab/state/credentials.json`（仓库外，
    `test/lab/vmlab/vm-console.py:38` 的 `CRED_FILE`）。
  - **运营方没有账号口令，走令牌**：登录框的 token 字段填
    `/Users/louis/srvmlab/state/console-token` 的内容（与 `ctrl/data/console-token` 同一份，已 `cmp` 核过）。
    入口是 `controller/cmd/sr-ctrl/admin.go:418` 那条 break-glass 分支，
    成功即 `Role=operator, Username=operator, Via=token`。
  - 会话是 **Cookie**，不是 Bearer；POST 必须带 `Content-Type: application/json`，否则 415。
  - `/api/underlay` 对运营方回 **403** —— 那是租户 scoped 的接口，**不是缺陷**，别去"修"它。
- `/Users/louis/srvmlab/ctrl/data/ca.key` 与 `server.key`：在工作区附加目录之外，Qoder 默认拒读。
  **不要为了省事把它们放进仓库工作区。**
- master 分支：AGENTS.md §3 明说历史有敏感材料，永不推。第二个 agent 也一样。
- 任何 `.env`、`id_rsa`、`*.pem` 直接进 stage 的动作 —— 提交前逐行审计新增内容（AGENTS.md §3），
  对面 agent 也不例外，甚至更严。
- 真机 `192.168.1.100`：**只读不动**，两个 agent 都不许写。

---

## 8. 一句话版本

> owner 打开第二个 Qoder 会话、挂同一个仓库工作区；对面先读 `AGENTS.md` → `agent-handover.md`
> （**先看 §10.1 冻结令**）→ `queue-plan-next.md` → 跑 §0.1 里冻结期允许的那几条复核命令 →
> 把结果 + "我打算领 #N" + "这条动不动协议" 贴回 owner 会话 → 冻结期只做代码/单测/文档能收口的活
> （推荐 M-4）→ 解冻后动手前发知会、写盒内前建告示牌锁、写完释放 →
> 一个增量一个 commit 推 `feat/vbond-control-plane`，没有 VM 数字就如实写"e2e 待解冻"。

这一条落地上，第一个 agent（也就是当前这个）能配合的是：
1. 每次进入 lab 写路径前，把 `/Users/louis/srvmlab/locks/<box>.lock` 建起来（内容一行）；
2. 提交后在 handover **§9** 那张未决队列表里把已完成的那条改成"已修 + 证据"；
3. 不在 handover 里写"下一步我打算 XXX" —— 那是**对面 agent** 该读该写的段落，
   我这边只写"当前状态"。这样对面接手时读到的 handover 永远是"事实"，不是"意图"。

---

## 9. owner 直接粘给第二个会话的第一条消息

下面整段可以直接复制粘贴，作为那个会话的**第一句用户消息**。它把冻结令、必读顺序、
第一件活和合流点都带过去了，对面不需要回来问第二遍。

```
你要加入一套已经在跑的自研 SD-WAN 工程（VPP 数据面 C 插件 + Go 控制面），
和另一个常驻 agent 共用同一个仓库、同一套 VirtualBox lab、同一个控制器。

工作区：/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/
（把这个目录挂进工作区，Qoder 会自动加载根目录的 AGENTS.md）

【最重要的一条：lab 现在是冻结的】
我（owner）正在自己测主备切换，lab 先放那里不动。
写一律禁止：不许故障注入、不许换件、不许改配置发布、不许 PUT /config、
不许跑 vm-fib-sync.sh、不许带 ESCAPE= 跑脚本、不许重启进程、不许重建 lab。
只读脚本也不要跑（vm-verify.sh / six.sh / box-layers.sh）——它们会占 CPU、打流量、
动计数器，污染我的读数。解冻只有我明说才算。
这条写在 docs/agent-handover.md §10.1，优先级高于那份文件其余各条。
注意：我 2026-11-09 撤掉的是"常设窗口审批"，不是这条冻结令，别把两者搞混。
你上车第一件事是问我一句"冻结令还在吗"，不要自己推断。

【按顺序读这三份，读完再动手】
1. /Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/AGENTS.md
   —— 四条硬规则，与任何文档冲突时它赢。核心是：生产不许冷启动、配置生效路径里
   不许重启任何 daemon（FRR 走 frr-reload.py 差量热更、VPP 走 CLI 增量、没有 restart 兜底）；
   成功以前后状态对比判定，不以退出码判定；不许用乐观窗口掩盖状态缺口；
   必然中断的操作必须点名并给实测时长；口令/密钥不进仓库；不 force push 不推 master。
2. /Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/agent-handover.md
   —— 现场。重点：§10.1 冻结令、§0.1 上车四条命令、§2 现在这一刻、§3 run/disk 台账、
   §5 唯一合法的生效路径、§7 测量陷阱、§9 未决队列、§10 冲突协议。
   这份文件自己会过期，读它之前先跑它给的复核命令；结果不一致以你测到的为准，并把它改掉。
3. /Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/queue-plan-next.md
   —— 任务的落纸方案。注意标题写的那三件事（sr-route 抬 HEAD / #275 / #276）已经全部完成，
   别再去领；看每节的"落地实况"和 §四。
按需第四份：/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-285/README.md
   —— 我现在要测的就是这一枪留下的主备形状（t13 颜色 0→2、四台逃生开关 true）。
   §五有还原配方，但现在不许执行。

另外这份是给你的加入方案本身，读它知道怎么和另一个 agent 分工：
/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/second-agent-onboarding.md

【复核命令：冻结期只跑这几条】
git log --oneline -5 && git status --short
ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep
（VBoxManage list runningvms 只问宿主，也可以跑。
 ssh 进盒算哈希那条先别跑，属于动我的现场，等我解冻。）
跑完把输出贴回来给我看，让两边读到的现场一致。
提醒：CPE1 的 sr-fib 有意停在 a9f1bc8c（缺陷对照臂），run != disk 在那一台是预期的，别去"对齐"。

【你领的第一件活：M-4（任务 #227）】
遥测行加 TunnelID（+ ReachReport 加或删）。方案在 docs/nat-tunnel-ladder-plan.md §9.5，
它是 NAT 隧道阶梯 #222 的硬前置。选它的理由：改动面只到 wire/agent/控制器/控制台，
纯 Go + 单测 + harness 就能收口，冻结期不需要动 lab。
它是协议改（TunnelID 要进 wire 的遥测行），所以你在第一条消息里就要点名这个合流点，
另一个 agent 同期在动 internal/fib 与 internal/route，撞车面是真实的。
按我的习惯：先出方案、我点头后再写代码；一次只推进一个缺陷。

【提交与推送】
分支 feat/vbond-control-plane，两个 agent 都往这一条推。不 force push，不推 master。
不 git add -A（工作区有六个未跟踪的 .out* 构建目录，还有别人的散工），按文件名 stage。
git 身份用一次性 -c：git -c user.name='louis' -c user.email='louis@users.noreply.local'
产物放 /Users/louis/srvmlab/out-<你的任务号>/，不要覆盖 artifacts/linux/ 里别人的文件。
一个增量一个提交。提交说明里写清动了什么、为什么、量了什么数字。
冻结期拿不到 VM 台数字就如实写"e2e 待解冻"，不许拿旧数字冒充本轮实测。
在 lp-ctrl 容器里编译、跑单测是冻结安全的；把产物 scp 进盒不是。

【协作纪律】
"修复"只指修软件缺陷，不指让 lab 看起来正常：不许手工 ip route、手改 vtysh/FRR 配置、
重启进程把现象压掉，一切改动走控制台 → 发布 → agent → apply 的产品路径。
先分析后方案：我的原话是"你的分析得到我的认同前，你想的解决方案毫无意义"。
不要压缩解释：按「场景 → 代码逐步走到哪 → 为什么这么写 → 症状 → 要动什么」讲全，
行号当凭证不当主语。
控制台不造数据：设备没上报就显示"未上报"，不显示 0 或占位值。
要登控制台验证时向我要账号口令，口令只在对话里传，不进任何文件。
每次交付完主动给我能登进去的账号/角色/口令，让我自己点一遍。

现在：读完那三份，跑允许的复核命令，然后贴一条"接手完成"给我，格式见
docs/second-agent-onboarding.md §2。第一条里必须包含"冻结令还在吗"这个问题。
```
