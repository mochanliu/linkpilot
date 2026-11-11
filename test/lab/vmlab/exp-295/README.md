# exp-295 —— 远程诊断工具箱 + 版本历史/回滚（监控管理第 5、6 项）

日期：2026-11-11 · 分支 `feat/vbond-control-plane`

## 交付了什么

### 5. 远程诊断工具箱

通道复用配置下发那条"发布→设备拉取"的形状（控制器永不拨号，盒子在 NAT 后）：

```
控制台 POST /api/diag {node, tool, target/iface/count | vpp_show}
  → 控制器入队（内存，per-node FIFO，上限 8 条未执行 / 50 条记录）
  → 心跳应答带 diag 字段下发（盒子自报 diag_busy 时按住不派发）
  → 盒子执行（白名单 + argv 无 shell + 30s 超时 + 32KB 截断）
  → POST /v1/diag 回执 → 控制台页 + 台账
```

- 工具三件：`ping`（目标/次数 1-10/可选源接口）、`traceroute`（未装则如实报错）、
  `vpp-show`（固定白名单：show interface / ip fib / ip6 fib / ip tunnel / bfd session /
  acl-plugin acl / abf）。**两侧各有一份白名单**——两个二进制不同天升级，控制器只发
  今天机队能答的，盒子再核一遍（拒绝时把白名单原样报回）。
- 状态机 pending → dispatched → done；盒子收下没执行就停在 dispatched（控制台明说
  "已下发，等回执"），不发明超时结论。
- 命令与结果都进台账（diag.issue / diag.result，结果行带输出前 4KB）；审计中间件
  自动覆盖 POST /api/diag。
- 控制器重启清空内存记录（没人盯着的诊断不值得活过重启），台账不受影响。
- 页面：监控与管理 › 远程诊断（viewDiag），5s 快速轮询跟随。

### 6. 版本历史 / diff / 一键回滚

- `DeviceConfig.History`：每次发布把被替换的 bundle 压进历史（cap 20），bundle 里新增
  `Source`（当时的草稿输入：BasicConfig + LeakSet + PolicySet，JSON 深拷贝）。
- 回滚 = **恢复草稿输入 + 走同一条发布路径**（不重放旧字节）：版本号向前、渲染反映
  今天的世界、校验门禁不绕过、盒子按普通发布拉取。`POST /api/config/rollback`。
- 功能上线前发布的版本 `has_source=false`，只能看 diff 不能回滚（API 明说，不猜）。
- diff：`GET /api/config/diff?node=&a=vN|draft&b=…`，七段 artifacts 逐段统一 diff
  （自写 60 行 LCS diff.go，无第三方依赖），页面 +/- 着色。
- 页面：基础配置页新增「版本历史 · 对比 · 回滚」面板（设备才有；预授权不显示）。

## VM 台验证（实测）

| 项 | 结果 |
|---|---|
| ping 192.168.1.4（管理口，CPE1） | 4/4、0% 丢包，输出完整回显 |
| ping -I lcp-x0 172.31.12.13（客户口源） | 3/3、0% 丢包 |
| vpp-show show bfd session | 2 会话，4ms |
| 白名单/非法目标拒绝 | curl、带空格分号目标、白名单外 vpp 命令全部 HTTP 400 |
| 版本链 v19→v20→v21→回滚→v22 | diff 报 frr 段 hostname 变更；回滚后草稿与渲染恢复 v20 内容；CPE1 拉取安装 v22（来源 apply） |
| 换件客户可见时长 | CPE1/CPE3 0 发 FAIL；CPE2/CPE4 各 4.1-4.2s 一段（agent 重启） |
| 换件后流量 | cust12 -I 172.16.12.1 → 172.16.3.1 5/5、0% 丢包 |
| 台账 | diag.issue ×5、diag.result ×5、config.rollback ×1、audit 全程覆盖；ledger 默认 kind 集已补这三类 |
| agent 日志 | `diag: diag_4 (ping 172.31.12.13 @ …) 完成，ok=true` 逐条在案 |

## 换件记录

- sr-ctrl darwin `6064ba45…`（Mac 侧，`/tmp/spawn_daemon.py` 双 fork 拉起——nohup 活不过
  短命 shell 的老坑再次复演，restart-ctrl.sh 的 nohup 版本起的进程 30s 内被收走）。
- sr-agent linux `fa5bfd98…` 四台全换（push-par-295.sh 分块并行 512KB×N + 整包 sha 核验）。
- **push-par-295.sh 两个新坑（本轮踩的）**：
  1. `ssh -n` 与 heredoc 互斥：`-n` 把 stdin 指到 /dev/null，`bash -s <<EOF` 读不到脚本、
     **静默成功 exit 0**——第一轮"全部成功"其实是远端什么都没跑。改法：heredoc 调用去掉 `-n`。
  2. sha 断言要对**解压后的整包**，不对 .gz 本身（第一版对着 gz 的 sha 比对的 binary）。

## 已知边界 / 遗留

- traceroute 未装在盒子上（发出后如实回 `executable file not found`）；要用就在四台
  `apt install traceroute`（**先清磁盘**，CPE2 清完 247M）。
- 诊断命令是"尽力一次"：盒子收下但没执行（恰好断电/重启）没有自动重发，控制台重发即可。
- pending 命令不落盘：控制器重启 = 队列清空（页面看得见记录消失，台账留了发起记录）。
- 审计中间件只记"POST /api/diag"，诊断意图的可读记录靠 appendOps 的 diag.issue 行。

## 第 7 项（流量/带宽字节计数）影响面 —— 待与 owner 碰

见对话总结，未动工。
