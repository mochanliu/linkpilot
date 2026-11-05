# exp-270 —— 问题 6：VPP 主线程落点与「VM 开机即最优」

owner 的原话是「我从主机 ping 192.168.1.103 是低延迟，其他都是高延迟……需要让所有 cpe 在开机时候
自动就是最优状态，而不是需要后续调优」，随后收窄成「你就在 vm 上，把 vm 的性能调优做好，
保证 vm 开机就是最优状态就行」（真机 192.168.1.100 他没装 VRF 内核模块，这一轮不碰）。

结论、数字和归因在 `docs/vmlab-rebuild-plan.md` **§9.18**。本目录只放可重跑的脚本和原始读数。

## 一句话结论

四台盒子里最大的一根时延开关是 **VPP 主线程落在哪个 vCPU**，而它默认没人管（内核开机随手给）。
把它写进启动配置 `cpu { main-core 0 }` 之后，真关机再开机量回来的就是最优状态。
落点写死 = 客户 overlay 往返 8~15ms（cpu1 时 22~30ms）、主机 ping 管理口 1.7~2.3ms（cpu1 时 4.2~7.3ms）。

生效路径必须重启 VPP，**必然中断客户流量**，CPE4 实测 15.3s（双向各 15.3/15.7s，丢包 0.2%，13s 自愈）。
只改运行时掩码（`taskset`）是 0ms（13,966 条守门样本、丢包行 0），所以两类操作分开报价。

## 八臂 2×2（客户 overlay 往返 avg 区间 / 隧道内探针 avg）

| | interrupt | polling |
| --- | --- | --- |
| **四台都 cpu0** | F 9.70~14.98 / 2.44, 2.29<br>F2 8.85~14.20 / 2.28, 1.56<br>K 9.65~14.09 / 1.96, 1.79 | H 8.44~11.42 / 1.52, 3.17<br>L 7.96~12.17 / 2.25, 1.70 |
| **四台都 cpu1** | G 22.07~30.41 / 6.71, 6.25<br>N 22.25~27.43 / 8.74, 5.47 | M 22.06~26.33 / 5.76, 6.33 |

cpu0 五臂无一超过 15ms，cpu1 三臂无一低于 22ms。F→G→F2 反序连摆两次，读数跟着落点来回摆。
臂 A/B（自然落点 + 只换运行时掩码）与排除项（硬中断落点、vCPU 天生不等价、盒内 loopback）
的逐行读数在 `raw/arms-270.log`。

**H、K 两臂的标签一度是错的**：描述写的「仍在 cpu1」，但 F2 已把四台钉回 cpu0，H/K 的命令里
只有 `set interface rx-mode`、没有重新钉核 ⇒ 真实形状是 cpu0。发现过程与教训见 §9.18 第五节：
摆臂的当前落点必须由读回定义（臂 M 起就是掩码 + rx-mode 一起读回）。

## 脚本

| 文件 | 做什么 | 会不会打断客户流量 |
| --- | --- | --- |
| `sample.sh` | 三把尺子同时量：主机→管理口 ping、客户 VM↔客户 VM（进隧道）、`show sr-wg probe` | 否（只发 ICMP） |
| `arm.sh` | 运行时改某台 VPP 主线程掩码，**改完立刻读回** `Cpus_allowed_list` | 否（0ms，上面那 13,966 样本就是证据） |
| `deploy-main-core.sh` | 摆配置臂：备份 `startup-vmlab.conf` → 插 `cpu { main-core $CORE }` → `systemctl restart vpp`；盒上脱离 ssh 的 60s 看门狗，`vpp_main` 没回来就还原备份并再起一次 | **是**（重启 VPP；CPE4 实测 15.3s） |
| `boot-check.sh` | 真开机判据：ACPI 关机→等 `poweroff`→headless 启动→等 ssh→按同一口径读回落点/时延/接管/守护进程/隧道表。只读，不改（除了它自己触发的这次开关机） | **是**（整台 VM 重启） |

```
cd test/lab/vmlab/exp-270
LABEL=G-全cpu1 ./sample.sh                # 自定义 ROUNDS/N/HOSTS/PROBE_BOXES/OVL
BOX=CPE4 SKIP_POWER=1 ./boot-check.sh     # 只读回，安全
BOX=CPE4 ./boot-check.sh                  # 真关机重开，会断 CPE4 上客户流量 ~15s+
CORE=0 ./deploy-main-core.sh CPE4         # 摆配置臂
bash ../vm-verify.sh | tail -30           # 全 lab 判据（含「VPP 主线程在 Core 0」那条）
```

## 原始读数（`raw/`）

| 文件 | 内容 |
| --- | --- |
| `arms-270.log` | 臂 A/B/C/F/G/F2/H/K/L/M/N + 三条排除项的逐行输出（含掩码与 IRQ 亲和性读回） |
| `boot-CPE4-first-readback.log` | 真开机后第一次读回：**PASS=7 FAIL=7**（4 条是守护进程还没起，1 条是 sr-fib 隧道表=0 = 缺口 G-2） |
| `boot-CPE4-after-fibsync.log` | lab 脚本把隧道表推回去之后：**PASS=14 FAIL=0**。两份都要留着，只留全绿那份等于把 G-2 读成"开机自己就好了" |
| `boot-CPE4-rerun-after-fix.log` | 修好 ping avg 抽取后同一台重跑（管理口 avg 3.553ms） |
| `deploy-main-core-driver.log` | 15.3s 那一档的 gate 相位与客户侧探针切窗 |

## 还没收口的两件事（不在 #270 里修）

1. **缺口 G-2**：真开机那一刻 sr-fib 的隧道表 = 0 条（`fib.json` 落盘 `"tunnels": []`，产品里没人维护这张表），
   overlay 客户前缀装不出来；lab 现在靠 `vm-fib-sync.sh` 事后 `PUT /config` 兜 —— 那是非生产的东西。
   已拍 (a)：agent 自己派生并推，触发全动态、不落盘。方案见 `docs/g2-tunnel-table-derivation-plan.md`。
   **在它收口之前，"开机即最优"只对到 VPP 落点 / 接管 / 隧道 / BFD 这一层。**
2. **缺陷⑨（#271）**：回灌链路把 `172.16.3.1` 经客户 OSPF 灌回 CPE1，RR 选中回灌路径后
   涉 cust3 的三条客户路径全黑洞。这一轮 CPE2/CPE3 的部署探针尾巴停在 LOSS，就是它，不是 VPP 重启。
   待拍的是导入/导出过滤规则与 zebra 里那条陈旧 K 路由的摘除时机，本目录不替它做决定。
