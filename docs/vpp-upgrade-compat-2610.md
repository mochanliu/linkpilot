# VPP 26.06 → 26.10 升级兼容性评估（NAT 解套方案）

owner 决策背景：nat44_ei 在 VPP 26.06-release 上判死（#302，2026-11-11 实测三连爆），
owner 倾向**升级 VPP** 解 NAT，但要求先重新核对与现有 dataplane 的兼容性。本文是评估
结论 + 落地路径；**结论先行：升级值得做（26.10 已发布、我们的用法面窄），但升级本身
不承诺修好 NAT —— 必须用 build spike + 流量实测裁决，不能只看 release notes。**

## 1. 我们在 #302 里实测到的缺陷（升级要解的对象）

CPE1 / VPP v26.06-release / 2G 内存 lab VM，2026-11-11：

1. CLI `nat44 plugin enable` 默认参数 → main heap OOM `os_panic()`，VPP 整个进程死，
   接管/重放竞态把 lab VM 拖到只剩 VBoxManage reset。
2. heap 加大后 CLI enable"成功"但状态不粘（disable 答 already disabled、
   `set interface nat44` 答 add failed）。
3. **数据面崩**：改走 binary API（`nat.go` 显式 sizing，5 轮 retval=0、配置回读准确），
   NAT 开着后第一次真实 WAN 流量进来整机卡死（ssh EAGAIN、只有 ICMP 活）。

## 2. 上游现状（2026-11-12 调研）

- **26.10 已发布**（2026-10-28，packagecloud.io/fdio/release；462 commits，含 174 fixes）。
- nat44-ei 在上游**仍有多条未收口的数据面问题**（都是 26.06 之后提交的）：
  - [#3747](https://github.com/FDio/vpp/issues/3747) worker-handoff 特性排在
    ip4-sv-reassembly 之前 → 会话 port=0、**回复全丢**（原文："unchanged on master…
    since at least stable/2410" ⇒ 26.10 大概率没修；**仅多 worker 触发**，我们是单 worker）。
  - [#3749](https://github.com/FDio/vpp/issues/3749) `nat44 ei del user` SIGSEGV（多 worker）。
  - nat44-ed 族也有未收口 SIGSEGV（#3728 GRE/IPIP ICMP；#3743 地址-only 静态映射按包协议查）。
  - 邮件列表另有 nat44-ei ICMP error 线程索引混淆崩溃的讨论（无单一条目号）。
- **诚实结论**：我们 #302 第 3 条"首包即整机卡死"没有干净对应到某一条上游 issue ——
  可能是多个 bug 的合流（OOM 家族 + crash 后接管/重放竞态放大成假死）。26.10 的 174 个
  fix 里有没有命中我们的那条，**只有装上打流量才知道**。

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
   `vm-verify.sh`；**NAT 判据 = `nat-probe -keep` + 真实 WAN 流量打 10 分钟不假死**
   （#302 的杀手测试，retval=0 不算数）。回滚底：26.06 包留盘、恢复即降级。
4. **解封 NAT 下发**：金丝雀过了才改 `apply.servicesNatBlocked()` → false，控制台发
   NAT 行，CPE1 做 DIA+NAT 全链路（多 VPN 不串 + VPP 重启自愈，dia-nat-dhcp-dns-plan.md §4 判据）。

**若 26.10 也不过关的备选**：把上游 #3747 的两行 `runs_after` 修复 + 线程索引守卫打到
26.06 上自编（改动小但进入"自维护插件"状态，与 owner 倾向的升级路线二选一）。

## 5. 已核对无虑的事项

- 表号模型（ColorTable=color*1000+main、SelectorTable=10000+row）纯 VPP FIB 语义，不依赖版本。
- sr-fib/sr-route/sr-agent/sr-ctrl 都是用户态、走 binary API —— 只要 ABI diff 归零就透明。
- our DHCP/DNS（sr-svc）走内核 TAP punt，与 VPP 版本解耦（linux-cp 行为重测覆盖）。
