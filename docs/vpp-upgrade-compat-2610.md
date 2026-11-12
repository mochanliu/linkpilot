# VPP 26.06 → 26.10 升级兼容性评估（NAT 解套方案）

owner 决策背景：nat44_ei 在 VPP 26.06-release 上判死（#302，2026-11-11 实测三连爆），
owner 倾向**升级 VPP** 解 NAT，但要求先重新核对与现有 dataplane 的兼容性。本文是评估
结论 + 落地路径；**结论先行：升级值得做（26.10 已发布、我们的用法面窄），但升级本身
不承诺修好 NAT —— 必须用 build spike + 流量实测裁决，不能只看 release notes。**

## 1. 我们在 #302 里实测到的缺陷（升级要解的对象）

CPE1 / VPP v26.06-release / 2G 内存 lab VM，2026-11-11/12：

1. CLI `nat44 plugin enable` 默认参数 → main heap OOM `os_panic()`（/var/crash 里留有
   SIGABRT core），VPP 整个进程死，接管/重放竞态把 lab VM 拖到只剩 VBoxManage reset。
2. heap 加大后 CLI enable"成功"但状态不粘（disable 答 already disabled、
   `set interface nat44` 答 add failed）。
3. **数据面**：binary API enable retval=0、dumps 回读准确，但 NAT 开启后**两个 NAT
   接口上的所有流量被 nat44 特性节点全丢**。2026-11-12 带取证复测 4/4 秒级复现
   （nat-watch.sh 2s 采样 + nat-deep.sh vppctl 深采样，见 test/lab/vmlab/）：
   - WAN 口 drops 以背景流量全速上涨（~30pps 全丢），punt/本地交付不受影响
     （ICMP echo 照答、盒内 vppctl 走 unix socket 照常、lcp pair 与内核地址完好）；
   - **会话表始终为 0**——cust12→WAN 网关的 transit ping 一个会话都没建出来；
   - 无资源异常、无 panic、无重启（load/mem/pid 全程平稳）；
   - 影响面：入向到盒子的 TCP（SSH 管理）与客户 transit 全死；盒内与本地交付幸存。
   当时的"整机假死"实为管理面失联的误称：VPP 活着，是 TCP 路径全被 NAT 丢包。
   是否 26.06 nat44-ei 真 bug、缺某个使能旋钮、还是与我们特性弧（ABF/sr_wg）的
   交互，**待 26.10 金丝雀同一杀器测试裁决**——现在测试有节点级判据（drops 全速/
   sessions==0），不再是"神秘假死"。

## 2. 上游现状（2026-11-12 调研）

- **26.10 已发布**（2026-10-28，packagecloud.io/fdio/release；462 commits，含 174 fixes）。
- nat44-ei 在上游**仍有多条未收口的数据面问题**（都是 26.06 之后提交的）：
  - [#3747](https://github.com/FDio/vpp/issues/3747) worker-handoff 特性排在
    ip4-sv-reassembly 之前 → 会话 port=0、**回复全丢**（原文："unchanged on master…
    since at least stable/2410" ⇒ 26.10 大概率没修；**仅多 worker 触发**，我们是单 worker）。
  - [#3749](https://github.com/FDio/vpp/issues/3749) `nat44 ei del user` SIGSEGV（多 worker）。
  - nat44-ed 族也有未收口 SIGSEGV（#3728 GRE/IPIP ICMP；#3743 地址-only 静态映射按包协议查）。
  - 邮件列表另有 nat44-ei ICMP error 线程索引混淆崩溃的讨论（无单一条目号）。
- **诚实结论（2026-11-12 取证复测后更新）**：现象已从"神秘假死"收敛为可测形状
  （§1.3：NAT 接口全丢包 + 会话恒 0 + 无任何资源异常/panic）。它没有干净对应到上表
  任何一条 open issue——"drop-everything + 0 sessions"更像是**使能路径缺了某个前提**
  （旋钮/顺序/与我们特性弧的交互）或 26.06 ei 插件的未登记缺陷。所以判定实验保留
  原样：26.10 金丝雀跑同一杀器测试，节点级判据（drops 是否全速、sessions 是否仍 0）
  能直接分辨"版本 bug 被修了"还是"我们的用法错了"。

## 3. 兼容性核对清单（升级前必须逐项过）

| 组件 | 我们的用法 | 升级风险 | 动作 |
| --- | --- | --- | --- |
| 自研插件 `sr_wg_plugin` / `srmpls_plugin` | repo `vpp-plugin/`，对着 26.06 头文件编 | **高**：API/结构体漂移会编译失败或行为漂移 | 26.10 头文件重编（build spike 第一步），过 `vpp-plugin` 单测 |
| 上游插件 wireguard / crypto_openssl / linux_cp / linux_nl | `show plugins` 六件套判据 | 中：26.10 wireguard 有已知 open issue（#3755 outer IPv4 ID=0） | spike 后跑 vm-verify §1 六件套 |
| linux-cp | `lcp create host-*` + dirty-MAC 修回（runVPP 预扫描，26.06 实测） | 中：26.10 新增 `lcp_itf_pair_add_del_v3` API | 重测 MAC 修回 + lcp-sync 反写行为（exp-286 §5.5 的复测脚本） |
| af_packet | 全部数据面入口 | 中：#3747 复现环境就是 af_packet；offload 行为可能变 | 升级后重估 #302 的 ethtool-off workaround（apply.ensureHostIface 与 underlay.sh 两处，见代码注释） |
| binary API（Go 编码器 `internal/vppapi`） | nat44_ei/abf/acl/fib/interface/bfd/mpls/vrrp 等消息，字节布局按 26.06 的 api.json 手抄 | **高**：任何"Definition changed"都是静默错包 | spike 里脚本化 diff 26.06 vs 26.10 的 `*.api.json`（只 diff 我们编码的消息名），逐条改 Go |
| startup.conf | main-core 0 钉核、heap、插件白名单 | 低（不用 DPDK；26.10 的 DPDK 26.07 iova 坑与我们无关） | 沿用 + vm-verify §1 主线程落核判据 |
| 策略引流（ABF + 颜色表 2000/3000/10001） | §6 双表口径（2026-11-12 新判据） | 中 | 升级后全量 vm-verify 必须绿 |

## 4. 落地路径（建议四步，每步有独立判据）

1. **Build spike（不碰 VM 台）**：起一个装 VPP 26.10 dev 包的容器（srwg-dev 同款、换包
   版本），`vpp-plugin` 对 26.10 重编 + 单测；同容器跑 api.json diff，产出"我们要用的
   消息里哪些 Definition changed"清单。判据：插件编过 + diff 清单归零或有改法。
2. **ABI 修补**：按 diff 清单改 `internal/vppapi`（消息布局与大小字段），`go test ./...` 全绿。
3. **金丝雀盒**：选一台（建议 CPE2，非缺陷臂、非服务面验证机）装 26.10 全套
   （vpp + vpp-plugin-* + 我们的重编插件），`systemctl restart vpp`，跑全量
   `vm-verify.sh`；**NAT 判据 = `nat-probe -keep` + 真实 WAN 流量，对照 2026-11-12 的
   节点级判据**：WAN 口 drops 不得随背景流量全速上涨、`show nat44 ei sessions` 在
   transit 流量下必须出现会话（retval=0 不算数；取证工具 test/lab/vmlab/nat-watch.sh
   + nat-deep.sh）。回滚底：26.06 包留盘、恢复即降级。
4. **解封 NAT 下发**：金丝雀过了才改 `apply.servicesNatBlocked()` → false，控制台发
   NAT 行，CPE1 做 DIA+NAT 全链路（多 VPN 不串 + VPP 重启自愈，dia-nat-dhcp-dns-plan.md §4 判据）。

**若 26.10 也不过关的备选**：把上游 #3747 的两行 `runs_after` 修复 + 线程索引守卫打到
26.06 上自编（改动小但进入"自维护插件"状态，与 owner 倾向的升级路线二选一）。

## 5. 已核对无虑的事项

- 表号模型（ColorTable=color*1000+main、SelectorTable=10000+row）纯 VPP FIB 语义，不依赖版本。
- sr-fib/sr-route/sr-agent/sr-ctrl 都是用户态、走 binary API —— 只要 ABI diff 归零就透明。
- our DHCP/DNS（sr-svc）走内核 TAP punt，与 VPP 版本解耦（linux-cp 行为重测覆盖）。
