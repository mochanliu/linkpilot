# O6-e：路由泄露（overlay 第三块）方案

> 状态：**待拍**（本文只出方案，不写代码）。日期 2026-11-02。
> 触发：定 #185 时他给出方向——**"本机宣告什么路由是由路由 leak 配置上的"**，
> 盘上那份手写的静态 `local_prefixes` 根本不该存在。

## 0. 一句话

控制器给每个 VPN 一份"**宣告什么**（内侧 → SD-WAN）+ **接收什么**（SD-WAN → 内侧）"的策略，
渲染进盒子的 FRR 与 sr-frr，发布后热更、不重启任何进程；盘上那份手写静态前缀表退役。

## 1. 现状（都有代码/盘上证据）

**"本机宣告什么"今天由三样东西拼出来，没有一样是控制器定的：**

| 来源 | 今天的样子 | 证据 |
| --- | --- | --- |
| 客户侧 BGP | sr-frr 订阅 zebra 里 **BGP 类型**的 IPv4 路由，**全量**上报（无过滤、无来源选择） | `cmd/sr-frr/main.go:288-289`（`SubscribeBGP`）、`internal/frr/import.go:52-66`（集合只按 prefix 记） |
| 只报一个 VPN | `import` 段只有一个 `vrf_id` | `cmd/sr-frr/main.go:47`；lab-cpe1 的 `/etc/sr-frr.json` 只有 `vrf_id: 0` |
| 手写静态表 | `route.json` 的 `local_prefixes`，运维手写；控制器**从不**写它 | `cmd/sr-route/main.go:48`、`internal/route/client.go:353-359` |

**盘上实况（lab-cpe1，2026-11-02）**：`/etc/sdwan/route.json` **没有** `local_prefixes`
（真正的宣告全走 sr-frr 动态那条），`/etc/frr/frr.conf` 里只有 `redistribute table-direct 1250`，
没有 route-map、没有 tag。也就是说静态表在 lab 上已经名存实亡，只剩判据脚本在播种它。

**控制器这边**：FRR 渲染器把 overlay 那几行**故意留成注释**——"overlay 层生成，本页不编辑：
redistribute table-direct 1250 / CUST-IN tag 1001"（`cmd/sr-ctrl/config_render.go:100`）。
控制器的 overlay 模型只写 `tunnels` / `vrfs` / `rrs` 三个键（`internal/apply/route.go:47`），
`sr-frr.json` 整个文件**不在 bundle 里**（一直是运维手写）。

**反方向（SD-WAN → 内侧）**：sr-frr 把 overlay 路由按 tag 1001 灌进 FRR，再由 FRR 的
`redistribute` 送进客户 BGP/OSPF——这一半也**没有任何控制器生成的配置**。

## 2. 目标形状

一条**泄露策略**（租户级，按 VPN 一条）两个方向：

```
LeakPolicy
  vpn_id            # 这条策略管哪个 VPN
  in:               # 内侧 → SD-WAN（"本机宣告什么"）
    sources         # bgp | connected | static | ospf（可多选；默认空 = 不宣告）
    prefixes        # 前缀过滤（FRR prefix-list 语法：精确 / le / ge），空 = 该来源全放
  out:              # SD-WAN → 内侧
    into            # none | bgp | ospf
```

- **没有策略 = 不宣告、不接收**（默认拒绝）。今天的"全量 BGP"是旧行为，迁移时按现状给 VPN 0
  生成一条 `sources=[bgp]`，其它 VPN 生成 `sources=[]`。
- RT 仍归控制器（导通策略分配，不在这里出现）。这边决定"哪些前缀进 overlay"，
  导通策略决定"它们能被哪些 VPN 看见"——两件事，不混。
- **前缀泄露到另一个 VPN**（prefix 级 A→B）**不在这一步**：那是 RT 层面的粒度，
  今天一个本机前缀只能带它所在 VPN 的 export RTs（`internal/route/client.go:477-520`），
  要做需要 per-prefix RT。留作 O6-f，先不做。

## 3. 谁渲染什么

| 落点 | 内容 | 谁生成 |
| --- | --- | --- |
| 盒子 `/etc/frr/frr.conf` | 每个 VPN：`redistribute connected/static/ospf/bgp` + `route-map`（prefix-list 过滤）+ `tag 1001` 的 CUST-IN（反方向） | 控制器（`renderFRR` 里那段占位注释变成真代码） |
| 盒子 `/etc/sr-frr.json` | 每个 VPN 一段 import（替代现在单个 `vrf_id`）：来源 + 前缀过滤 | 控制器（**新增进 bundle**，今天没有） |
| 盒子 `/etc/sdwan/route.json` | 删掉 `local_prefixes` 键 | —— |

**热更路径（不许重启）**：

- FRR 侧：走已有的 `frr-reload.py` 差量（AGENTS.md 第 1 条，已实测 0 丢包、pid 不变）。
- sr-frr 侧：**sr-frr 重启会主动撤掉自己刚宣告的前缀**（`cmd/sr-frr/main.go:405-415` 那段
  退出清理，日志"停止，撤掉自己下发的 %d 条前缀"），等于全网抖动一次。所以策略变更**不能
  靠改文件重启**，要给 sr-frr 加一个与 sr-route 对称的热更口：
  `PUT /import`（新配置整份替换），sr-apply 像推 `PUT /tunnels` 那样推它。
  启动时仍从 `/etc/sr-frr.json` 读（开机重放，不依赖控制器）。

**过滤器放在 sr-frr 还是 FRR？** 建议放 **sr-frr**：
- 客户前缀是客户用 eBGP 送进来的，进了 BGP RIB 就得让客户自己能用；在 FRR 的 `in` 方向过滤
  会把这些前缀从客户的 VPN 里也拿掉，方向就错了。
- 我们的 overlay 邻居不是 BGP 邻居（是 sr-route），没有 outbound route-map 可挂。
- 但 `connected` / `static` 这两个来源只能从 FRR 的 RIB 里读（ZAPI 订阅要扩到这两类），
  sr-frr 的 importer 现在只认 BGP。

## 4. 分步（每步单独验证、单独提交）

| 步 | 内容 | 判据 |
| --- | --- | --- |
| L-1 | 控制器模型 + API + 渲染进 FRR / `sr-frr.json`（先只做 bgp 来源 + 前缀过滤） | 单测：策略变了，渲染出来的 frr.conf / sr-frr.json 逐行对得上；没策略时 VPN 不出 `redistribute` |
| L-2 | sr-frr：多 VPN import + 前缀过滤 + `PUT /import` 热更 | 判据脚本：改过滤后 `PUT` 到达，sr-route 的 `local` 集合当场变，sr-frr pid 与启动时刻不变 |
| L-3 | sr-apply：`sr-frr.json` 进 bundle、叠加写盘、推 `PUT /import`；失败不许重启 | 与 `test-overlay-bundle.sh` 同口径的十步脚本（新增第 5 段） |
| L-4 | 控制台第三块「路由泄露」页（VPN 下拉 + 来源多选 + 前缀列表 + 反方向） | 真浏览器：改一条策略 → 发布 → 页面下发状态面板显示已应用 |
| L-5 | 静态 `local_prefixes` 退役：删 `sr-route` 的键、删 sr-frr 单 `vrf_id`、lab 各盒子核对 | 全仓 grep `local_prefixes` 零命中（脚本里播种的那份改掉）；#185 随之关闭 |
| L-6 | lab 端到端：一台盒子宣告 A，另一台按导通策略看到 A，按泄露策略被挡掉 | 两台盒子 + 真 RR，路由集合前后对比 |

## 5. 需要他拍的四条

1. **来源维度**：`connected / static / ospf` 要不要一起做（要扩 ZAPI 订阅），还是先只做 `bgp`
   这一种来源（L-1 就能收口，但"盒子上直连网段要宣告"仍得靠客户 BGP 送进来）。
2. **粒度**：策略按 **租户 × VPN**（所有盒子同一套，我推荐）还是按设备单独配。
3. **默认**：没有策略 = **不宣告 / 不接收**（推荐，配置显式）还是沿用今天的"全宣告"。
4. **反方向**（SD-WAN → 内侧）要不要和正方向一起做，还是先把"宣告什么"做完再做接收。

## 6. 与 #185 的关系

#185（静态前缀挂到未发布 VPN：启动硬错、热更只跳过）不是要选"跳过还是报错"，
而是这张表本身要退役。L-5 完成时 #185 自然关闭；在那之前**不改**它的行为，
免得给一条要删的路径加新逻辑。
