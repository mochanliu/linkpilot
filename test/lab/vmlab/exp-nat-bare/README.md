# exp-nat-bare：NAT "整机假死"的裁决实验（2026-11-12）

## 裁决结论

**不是 VPP 26.06 的发行版缺陷，是配置形状问题。** 26.10 升级路线对 NAT 不再必要
（升级是否仍要做，回归到纯粹的产品动机，与 NAT 解耦）。

三条实测支柱（全部在 CPE4，af_packet + linux-cp + 单 worker 形状不变）：

1. **裸 VPP + 纯 CLI + 全 stock 插件（禁 sr_wg、无 sr-agent）：客户数据面完全正常。**
   cust4 → 网关 192.168.1.1 ping 8/8 回包 0% 丢，会话表建立
   `172.31.4.2: 1 dynamic translations`（raw/probe-bare-datapath.log）。
   "发行版里不可能有完全不能用的 bug" 这一质疑被证实。

2. **"整机假死"的真实机制 = out2in 的标准 EI 语义，不是崩溃。**
   `nat44-ei-out2in / no translation`（数千次）是唯一的丢包节点：**入向包目的地址
   是 NAT pool 地址且没有会话匹配 → 丢**。产品配置把 pool 设成外口自身地址
   （= 盒子 WAN IP = 管理通道地址），于是所有入向管理流量（ssh/控制器）无一例外
   被丢。VPP 全程活着，vppctl 本地一直可答。

3. **正确形状是"普通 outside + 专用 pool + 外口地址身份映射"（形状 G）**，
   最终不是 of4 那版的 output-interface 形状（见文末"形状 D → G"）：
   - 内口 host-enp0s8：普通 `IF_INSIDE` 注册（入向 in2out）
   - 外口 host-enp0s9：普通 `IF_OUTSIDE` 注册（入向 out2in）
   - **专用 pool 地址** 192.168.1.200（外口 secondary 地址），不用外口自身地址
   - **外口自身地址 192.168.1.104 加 address-only 身份映射**（1:1、端口不变）
   - 结果：cust4 → 网关 8/8；盒子 → 网关 3/3；ssh 全程 50/50 存活；
     `no translation` 只多 6 次（对照形状 E 的 +1795）。

## 关键机制账本（上游 v26.06 源码逐一印证）

| 现象 | 机制 |
|---|---|
| pool=外口自身地址时管理面死 | out2in 对"目的=pool 地址且无会话"一律丢（EI 标准行为） |
| 盒子自身发起的流量回不来 | 盒子流量不经 inside 口入向 in2out，无会话；回程到 pool 地址被丢 |
| output interface 是什么 | `nat44_ei_add_del_output_interface`：postrouting in2out（盒子流量出口翻译、建会话）+ out2in（入向反翻译），flag 置 inside+outside 双向 |
| 普通口注册与 output 口互斥 | `nat44_ei_add_output_interface` 遇到接口已在 `nm->interfaces` 回 -81（VALUE_EXIST），同一口不能两种都注册 |
| CLI 组合形式踩坑 | `set interface nat44 ei in X out Y output-feature` 把**两个口都**转成 output interface → 内口入向 in2out 被顶掉 → 数据面死（of-cli-both-ifaces.log，cust4 0/8） |
| pool=接口自身地址的残余问题 | `nat44-ei-in2out-output-slowpath / out of ports`（of3 里 907 次、of4 里 1603 次）：盒子流量的动态端口分配大量失败；专用 pool 地址后客户流量无此问题，**盒子自身流量经 NAT 仍不干净** |

## ssh 时间线（每 5s 一拍）

- 无 output-feature：`dead` 直到 NAT 关闭（raw/ssh-timeline-*.txt）
- output-interface + 专用 pool：dead ~30s → ALIVE 并保持
  （起始 30s 死亡窗口 = NAT 使能时旧 TCP 连接的回程包无会话被丢；新连接建立后存活。
  盒子自身流量的翻译成功率仍未到 100%，见 out of ports 条目）

## 实验过程事故（已恢复）

- 两次把 CPE4 楔死：ssh 断开时远端 shell 被 SIGHUP 杀掉导致多命令链条只跑了第一条；
  期间一次还误杀了看门狗。两次均 VBoxManage `controlvm CPE4 reset` 恢复
  （NAT 配置不持久化，开机自动回来）。
- 教训已固化进脚本：所有 NAT 使能序列必须 systemd-run 脱离 ssh 跑 + 自带超时自关
  + 看门狗兜底（nat-bare-watchdog.sh）。
- of2 第一轮空跑：硬重启时 startup conf 还带实验加的 disable 行，sr_wg 未加载导致
  探针 API 消息表对不上 dial 失败 —— 恢复原 conf 后重跑才有效。

## 产物

- `nat-probe` 增加 `-outpostroute`（nat44_ei_add_del_output_interface 外口注册）
- `controller/internal/vppapi/nat.go` 增加 `Nat44OutputInterface`
- 实验 CPE4 最终状态：startup conf 已还原原始、NAT 关、secondary 地址已清、
  cust4 内腿 ping 3/3 通。

## 对产品的含义（已实施，见 controller/internal/apply/nat.go）

1. **NAT 行的 pool 语义**：不得用外口自身地址做 pool；配置里显式给专用地址，
   盒子把它挂成外口的 secondary（同网段上游要 ARP 它）。保存期校验已拦住
   "pool 含外口地址"与"pool 越出外口网段"两种形状。
2. **外口注册方式**：普通 `IF_OUTSIDE`（不用 output-interface）。
3. **盒子自身流量**：不翻译、也不需要翻译；用 address-only 身份映射让入向到
   外口地址的包可反查即可。这一条同时解决管理面失联与 out of ports 两个问题。
4. 之前"cust12 transit 5/5 丢且 0 会话"的取证结论要修正：当时池地址未加（使能链
   在 pool 步被打断），属于半配置状态，不能作为"NAT 数据面坏"的证据。

## 形状 D → G：为什么最终不是 output-interface

of4 之后又做了两轮对照，判据从"管理面活不活"收紧到"管理面 + 客户数据面 +
盒子自身流量三样都要对"：

| 形状 | 外口注册 | pool | 身份映射 | cust4 | ssh | 盒子自身流量 |
|---|---|---|---|---|---|---|
| A | 普通 outside | 外口地址 .104 | — | 8/8 | 全丢 | — |
| E | 普通 outside | 专用 .200 | — | 8/8 | 全丢（+1795 no translation） | 3/3 |
| D (of4) | output-interface | 专用 .200 | — | 8/8 | 存活 | 1603 次 out of ports |
| **G** | **普通 outside** | **专用 .200** | **.104 身份映射** | **8/8** | **50/50 存活** | **3/3，零 out of ports** |

- E 证明：out2in 丢的不只是 pool 地址上的包，**外口地址自己的入向包也丢**。
  所以"专用 pool"本身不够，管理面还需要一条"这个地址是合法的"的凭据。
- D 证明：output-interface 让盒子自己的包也去翻译，端口分配失败就是 out of ports
  全丢（1603 次 ≈ 盒子上 BFD/封装的心跳节奏），隧道会跟着抖。
- G 是唯一三样全对的形状：普通 outside（不碰盒子流量）+ 身份映射（入向可反查）。
  实测 50 拍 ssh 全 ALIVE、`no translation` 只多 6 次、零 out of ports。

## 产品路径验收（形状 G 由 sr-apply 自己装出来）

`run-e2e.sh`（raw/e2e-v1-*.txt、e2e-v2-*.txt）：

- v1 带一行 NAT（内 host-enp0s8 / 外 host-enp0s9 / pool 192.168.1.200）：
  `sr-apply` 自己把 pool 地址挂上外口、加身份映射、挂特性；cust4 8/8（ttl 63）、
  会话 1 条、`static mappings: identity mapping 192.168.1.104 vrf 0`、vpp0 有
  .104 与 .200；ssh 40/40 ALIVE。
- 同一 bundle 再 apply 一次：幂等，rc=0，不多挂一个地址。
- v2 删掉 NAT 行：特性/池地址/secondary 全部退掉，盒子仍可管理，rc=0。
- `run-e2e-boot.sh`（raw/e2e-boot-*.txt）：`systemctl restart vpp`（NAT 状态随
  内存消失）后 `sr-apply boot` 重放：NAT 形状原样回来、cust4 8/8、
  ssh 30/30 ALIVE。

过程坑（值得记）：e2e 前几轮一直读旧结果——`systemd-run --unit` 用固定名字，
第二次起 unit 已存在直接失败，而脚本把上一次的日志当成本次输出。修法：unit 名带
`$$` + `--collect`，并在拉起前删旧日志。
