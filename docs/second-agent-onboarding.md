# 第二个 agent 加入这套工程：方案

不是把 `AGENTS.md` 复制一份给对面。是回答四个具体问题：
1. 那个会话起来后要读什么、跑什么命令，才算"接上手"；
2. 两条流水线上的活怎么切，才不互相踩；
3. 需要互相知会的时候走哪条路；
4. 出事（对面死在半路、锁没释放、lab 状态被单方面改了）怎么退回干净态。

前提：`AGENTS.md` + `docs/agent-handover.md` 已经是**共同法源**。本文只补
"如何把它交给第二个具体会话"这一步，不复述规则本身。

---

## 1. 起来就先接手的三份东西（顺序，别跳）

**a) `AGENTS.md`** —— 四条硬规则。这一份不进会话上下文的话，对面写出来的代码不会听约束。
让 owner 在第二个会话的 project 目录挂载里把仓库根 `/Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/`
（含 `AGENTS.md`）纳入工作区；Qoder 会自动加载项目根 `AGENTS.md` 到会话上下文。**这一步不做，后面全废。**

**b) `docs/agent-handover.md`** —— 现场：哪份产物在跑、改动的唯一合法路径、测量陷阱、
两个 agent 的冲突协议（§10）。这份文档自己会过期，所以 §11 末尾要求**读它之前先跑它给的复核命令**：
```
ssh CPE1 'for x in sr-agent sr-route sr-fib sr-frr; do pid=$(pgrep -x $x|head -1);
  [ -n "$pid" ] && printf "%-9s run=%s disk=%s\n" "$x" \
    "$(sha256sum /proc/$pid/exe|cut -c1-16)" "$(sha256sum /usr/local/bin/$x|cut -c1-16)"; done'
cd /Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan && git log --oneline -5
```
这两条命令的输出**必须**贴到 owner 的当前会话里 —— 不是贴给自己，是让 owner 看见两边读到的现场一致。

**c) `docs/queue-plan-next.md`（新增，本次提交进来）** —— 队列里剩下三件事（sr-route 抬 HEAD + #275 + #276）
的落纸方案。这份文档就是"给对面看的下一步"。

**顺序为什么这样**：AGENTS 是法、handover 是现场、queue-plan 是任务。前两份不读完就开始改代码，
必然撞 §1 冷启动禁令或者 §10 冲突协议。**任何一条都不许先动再补读。**

---

## 2. 会话与工作区的具体开法

Qoder 侧（owner 视角）：
1. **同一个仓库工作区，再开一个 Qoder Desktop 会话**（不要另开一份 clone —— 那样两边 `state.json`、
   `~/srvmlab/artifacts/` 与 VirtualBox 是同一套但 git 树不同步，会打脏）。
2. **工作区附加目录**保持与第一个会话相同：仓库根 + `~/srvmlab/artifacts/` + `~/srvmlab/out-<N>/`
   （后者让每个 agent 用**自己任务号**目录，天然分开）。
3. 会话命名建议 `sdwan-<role>`：`sdwan-dataplane`、`sdwan-controlplane`、`sdwan-console`，
   owner 一眼能看出谁在管哪一段。名字不重要，重要的是**owner 侧留一份"这个会话现在在跑什么"的清单**。

**agent 侧第一个动作**：把 §1 (a)(b)(c) 三份读完 + 跑 §1 (b) 的两条复核命令 +
在 owner 会话里贴一句 `接手完成，HEAD=xxxxxxx，四台 run==disk：<是/否>，队列里我打算领 <#N>`。
这一句就是"打招呼"，owner 或者第一个 agent 看见这条就知道有人上车了。

**不推荐的做法**：
- 用 Agent 工具 spawn 一个 read-only 子代理做这件事 —— 那个是"临时专家"，5 分钟就散会；
  第二个 agent 加入是要**跨会话**、要能 commit push 的常驻会话。
- 让第二个 agent 直接读记忆里的 `MEMORY.md` —— 那是**第一个会话的**私有记忆，
  里面的偏好和上下文对第二条会话只是参考，不是权威。权威只有 `AGENTS.md` + `agent-handover.md` + 代码。

---

## 3. 任务怎么切（默认切法 + 什么时候合流）

**按数据面/控制面切**，两边交集只有 `controller/internal/route/proto`（v3）与 `vppapi` 结构。

| Agent A（当前这个） | Agent B（要加入的） |
| --- | --- |
| 控制面 Go：`controller/internal/**` + `controller/cmd/sr-*`（sr-route/sr-fib/sr-frr/sr-apply） | 数据面 C：VPP 插件 `vpp-plugin/**` + `vppapi` Go 客户端 |
| 控制台（`controller/web/console.html` 与配套 Go 后端） | 内核/驱动侧：linux-cp / linux-nl / af-packet 的形状 |
| 协议：route v3、config bundle 五段 | 遥测数据源：BFD/probe 在 VPP 内的实现 |

**队列里剩下的三件事按这张表落**：
- sr-route 抬 HEAD → **Agent A**（是部署 + Go 侧的兼容验证）。
- #275 resolvable 改吃活动隧道 → **Agent A**（改 `internal/route/client.go` 与 agent 接线，纯 Go 侧）。
- #276 事件驱动收敛 → **Agent A** 主导 sr-route/sr-fib/sr-frr 那三段；
  **Agent B** 补 VPP 侧 BFD 事件回调（sr_wg_probe 插件把状态变化直接推给 agent，而不是等 agent 去 `show bfd`）。
  这一件是**唯一需要两个 agent 合流**的，`proto/vppapi` 结构体加一个字段两边同时改。

**其他可以并行开的活**（不占 A 的队）：
- #272 跨盒回灌（A/B 拍板）—— 归 A，但要 owner 先拍语义。
- M-4/M-1/M-3/M-5 NAT 隧道阶梯 —— 归 B（数据面为主，控制面接一小段）。
- S8 交用户复核 docker 关不关 —— 归 owner，两个 agent 都不动。

**合流点**：只有一处 —— **协议改**。任何动 `internal/route/proto` 或 `vppapi` 的 PR 都必须在
owner 会话里点名"这条动了协议，另一边要跟着改"，让 owner 拍是否两个会话一起合入。
除此以外的文件级冲突（同一个 `.go` 都被改）按 §10 冲突协议：**先动的 agent 提交，后动的 rebase**。

---

## 4. 谁可以改 lab 现状（VirtualBox 7 台 + lp-* 容器 + 控制器 8088）

`agent-handover.md` §10 那张表说的是**规则**，这里给**流程**：

**动 lab 之前的声明模板**（发到 owner 会话，不是发到对面 agent）：
```
[Agent-A] 要动 lab：
  - 目标：CPE1（一台），抬 sr-route 到 <sha>
  - 窗口：预计 5 分钟（含基线前后各一次 vm-verify.sh）
  - 期间不要注入故障；对面如果在跑 exp-*，先停一下
  - 完事我发一条"释放"
```

对面 agent 收到（通过 owner 转发，或者 owner 直接问一句）：
- 如果**没在跑对基线敏感的活**，回一句"OK，我这边不冲突"；
- 如果**正在跑**，回一句"等 10 分钟"或者"我这轮跑完 X 之前不要动"；
- 什么都不回 = 视为"对面在动"，等 owner 仲裁。**没有隐式默认。**

**同时动 lab** 只有一种情况是合法的：只读判据（`vm-verify.sh`、`box-layers.sh`、`status.sh`、
`exp-*/six.sh` 这类"量一下不写"的）。这些随时可以并行跑。

**写路径的锁**：`/run/sr-frr/control.sock`、`/run/vpp/api.sock`、`state.json` 这三个物理上就是单点。
AGENTS.md §1 + `agent-handover.md` §10 已经写了"一次只让一个 agent 持有写盒内的锁"。
这一条我建议**加一个约定俗成的机制**：谁准备写盒内，就在 `~/srvmlab/locks/<box>.lock` 建一个文件、
里面写 `agent=<name> task=<#N> started=<iso8601>`，写完删掉。**不是** flock（flock 只在同主机有效），
就是一个"看一眼谁在里面"的告示牌。对面进盒前 `ls ~/srvmlab/locks/`，看见别人的名字就先别动。
这一条落地成本 = 一行 `echo`，收益 = 不会两边同时 PUT /config。

---

## 5. 出事怎么办（三条最常见的）

**a) 对面 agent 死了**（credit 用尽 / Qoder 崩了 / 会话被 owner 关了）
- 现场：`~/srvmlab/locks/` 里那个告示牌没人删。
- 处理：owner 或者留下的 agent 直接把 `~/srvmlab/locks/<那台>.lock` 删掉，
  同时在 owner 会话里说一句"对面死了，我接手它当时在做的 <#N>"。**不要静默接手** ——
  现场里的中间状态（比如刚 scp 了一半的产物）需要 owner 知情。
- 预防：任何 agent 领一个活的时候，第一句就是 `agent=X task=#N 现在开始，锁在 <box>`；
  owner 会话就是日志，出事回查。

**b) `docs/agent-handover.md` 被两条会话同时改**
- 现场：git 层面看是普通 merge conflict。
- 处理：谁后提交谁 rebase，rebase 时**读**对面那段而不是无脑 `--ours`。
  handover 里的每一条数字都要重新核（对面写的"sha 是 bab523ff"你要么在盒上跑一次确认，
  要么改成"待复核"，不许直接采信也不许直接删）。
- 预防：AGENTS.md §4 说"做完一个增量，如果它改变了那份文件的任何事实，在同一个提交里改掉它"——
  这就是让 handover 的写入跟着代码增量走，两条会话改的是**不同段落**（因为不同增量），冲突面就小了。

**c) lab 被改脏，两个 agent 的基线数字打架**
- 现场：A 刚量的 refill 12s，B 说是 19s，两边都不是错。
- 处理：**回到干净态**：`test/lab/vmlab/bringup-mac.sh` 全套重来（这是 owner 的活，不是 agent 的 —— 
  重建 5-10 分钟且中途 lab 完全不可用）。
- 预防：每次动手前跑一次 `vm-verify.sh`，量出来"和上次一样"再走。如果和 handover §6 的
  12~19s 差得远，那大概率不是自己的改动，是 lab 脏了。

---

## 6. 提交与推送

- **一条分支**：`feat/vbond-control-plane`，两个 agent 都往这一条推。不新建分支 ——
  新建两条 agent 分支再 merge 是给自己找冲突。
- **一条底线**：`AGENTS.md` §3 说不 force push 不推 master。这条对两个 agent 都成立。
- **一次一提交**：每个 agent 一个增量一个提交（`feedback-commit-each-increment`）。不要攒。
- **提交信息里必带**：动了什么文件、为什么、量了什么数字（改前 / 改后）、有没有影响 lab 现状。
  `agent-handover.md` 是共同法源，写"见 handover §X.Y"没问题，但不许用"照 handover"糊掉数字。
- **git 身份**：都走 `git -c user.name='louis' -c user.email='louis@users.noreply.local'`
  —— 见 memory `project-sdwan-git-and-secrets`。别改本地 config，一次性 `-c` 更干净。

---

## 7. 什么**不**给第二个 agent

- 运营方口令（`louis-ops`）与租户口令。**不给**。要用的时候 owner 现场输，见 memory
  `feedback-credentials-after-commit`：口令只在对话里传，不进文档、不进 memory。
- `/Users/louis/srvmlab/ctrl/data/ca.key` 与 `server.key`。这两个文件在会话的工作区附加目录之外，
  Qoder 默认拒读；不要为了省事把它们放进仓库工作区。
- master 分支（AGENTS.md §3 明说：历史有敏感材料，永不推）。第二个 agent 也一样。
- 任何 `.env`、`id_rsa`、`*.pem` 直接进 stage 的动作 —— 提交前逐行审计新增内容（AGENTS.md §3），
  对面 agent 也不例外，甚至更严。

---

## 8. 一句话版本

> owner 打开第二个 Qoder 会话、挂同一个仓库工作区；对面先读 `AGENTS.md` →
> 跑 `agent-handover.md` §11 的复核命令 → 把结果贴回 owner 会话 → 领一件不与他人抢
> lab 写锁的活（默认 `docs/queue-plan-next.md` 里没打"合流"标签的那些）→
> 动手前发声明、写完释放 → 一个增量一个 commit 推 `feat/vbond-control-plane`。

这一条落地上，第一个 agent（也就是当前这个）能配合的是：
1. 每次进入 lab 写路径前，把 `~/srvmlab/locks/<box>.lock` 建起来（内容一行了）；
2. 提交后在 handover §7 那张表里把已完成的那条改成"已修 + 证据"；
3. 不在 handover 里写"下一步我打算 XXX" —— 那是**对面 agent** 该读该写的段落，
   我这边只写"当前状态"。这样对面接手时读到的 handover 永远是"事实"，不是"意图"。
