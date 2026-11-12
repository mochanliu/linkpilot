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

3. **正确形状（现网 CGNAT 标准做法）下两者兼得**（raw/of4-dedicated-pool.log）：
   - 内口 host-enp0s8：普通 `IF_INSIDE` 注册（入向 in2out）
   - 外口 host-enp0s9：**`nat44_ei_add_del_output_interface`**（output interface：
     ip4-output 弧挂 postrouting in2out + ip4-unicast 挂 out2in）
   - **专用 pool 地址** 192.168.1.200（外口 secondary 地址），不用外口自身地址
   - 结果：cust4 → 网关 8/8 回包；ssh 管理面在 ~30s 内恢复并全程存活。

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

## 对产品的含义（待 owner 拍板后实施）

1. **NAT 行的 pool 语义要改**：不得用外口自身地址做 pool；应为 WAN 口规划专用
   NAT 地址（secondary 地址），或要求配置里显式给 pool 地址。
2. **外口注册方式要改**：不用 `IF_OUTSIDE` 普通注册，改用
   `nat44_ei_add_del_output_interface`（vppapi 已加 `Nat44OutputInterface`）。
3. **盒子自身流量**（控制器心跳/管理）是否过 NAT 需要明确设计：现在的证据表明
   "过 NAT"有 out-of-ports 半失败；更稳的做法是给盒子自身服务（如控制器端口）配
   identity mapping/静态映射，或让盒子流量绕开 NAT（路由层面打标）。
4. 之前"cust12 transit 5/5 丢且 0 会话"的取证结论要修正：当时池地址未加（使能链
   在 pool 步被打断），属于半配置状态，不能作为"NAT 数据面坏"的证据。
