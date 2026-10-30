# O6 方案：overlay 配置进控制器（控制台「配置 > overlay」+ 重启后能工作）

2026-10-31。配套读物：`docs/overlay-design-draft.md`（模型与定案都在那里，本文不重复）。
本文只回答一件事：**要把 overlay 配置从"人手写在设备上"变成"控制器发布、盒子重启自带"，
需要动哪几处，以及哪几处需要你拍。**

## 0. 先更正我早上说错的一句

早上我说"隧道表磁盘上没有那份，所以重启后恢复不了"。这话**只对了一半**：

- 隧道表（wg 口的期望值）**在控制器上，而且是持久化的** —— 落地走的是另一条通道：
  心跳里带 `tunnel_version`，`sr-agent -tunnels` 调 `UnderlayBundle` 取整份表、用 vppapi 建口
  （[cmd/sr-agent/main.go:186-197, 246-251](../controller/cmd/sr-agent/main.go)）。
  开机清单也已经能把这两个开关带上（`SR_BOX_AGENT_VPP_API` / `SR_BOX_AGENT_TUNNELS`，
  [start.sh:311-314](../controller/deploy/sr-box/start.sh)）。
  ⇒ **控制器可达时，盒子重启后隧道自己回来，今天就已经成立**，不需要"进配置包"。
- 判据脚本量到的"重启后 0 条"是我**故意**造成的：那一段把控制器指向 127.0.0.1:18799（没人监听），
  量的就是"离线开机"这一格。

所以"进配置包"真正要补的不是隧道，而是**下面这些今天只存在于设备上、人手写的那几份**：

| 数据 | 今天在哪 | 控制器有没有 |
| --- | --- | --- |
| 隧道（对端设备 + overlay /32 + 颜色） | 控制器 | **有**，`internal/underlay/state.go:122-152`（`Endpoint.NodeID` + `Endpoint.Overlay` + `Color`） |
| VPN 号本身 | 控制器 | **有**，`ConfigVPN{ID,Name,BGP,OSPF}`（[cmd/sr-ctrl/config.go:44-50](../controller/cmd/sr-ctrl/config.go)） |
| 每个 VPN 的 import/export RT | 设备上的 `sr-route.json` | **没有** |
| 本端宣告的前缀（`local_prefixes`） | 设备上的 `sr-route.json` | **没有**（内核侧的 `Statics` 有，overlay 侧那份没有） |
| 逃生开关 `escape_all_colors` | 设备上的 `sr-fib.json` | **没有** |
| sr-fib 的 `vrfs` + `tunnels`（带颜色） | 设备上的 `sr-fib.json` | **没有**（隧道数据控制器有，只是没渲染） |
| sr-frr 的 `vrfs` + `import`（反向导入） | 设备上的 `/etc/sr-frr.json` | **没有** |

证据：`export_rts` / `import_rts` / `escape_all_colors` 这几个键在控制器代码里**只出现在设备侧
二进制自己的配置结构里**（[cmd/sr-route/main.go:59-63](../controller/cmd/sr-route/main.go)、
[cmd/sr-fib/main.go:32-42](../controller/cmd/sr-fib/main.go)），`internal/apply` 里 grep `Tunnel` 是**零命中**；
bundle 只有三段文本：`Artifacts{FRR,VPP,Boot}`（[internal/apply/apply.go:53-67](../controller/internal/apply/apply.go)），
渲染函数是 `renderFRR/renderVPP/renderBoot`（[cmd/sr-ctrl/config_render.go:15,134,187](../controller/cmd/sr-ctrl/config_render.go)）。
控制台的「Overlay 配置」是个死槽位（`console.html:318-319`，`class="child dis"` + 「路由 · 尚未规划」，
**租户角色也看得见这个空槽**，2026-10-31 真浏览器确认）。

## 1. 目标形状（一句话）

**控制器多一个 overlay 模型（导通组 + leak + 逃生开关），发布时把它和已有的隧道表一起渲染成
每台设备一份的 `route.json`，进 bundle 第 4 段；设备侧由 `sr-apply` 落盘、由 `sr-agent` 推给
正在跑的 `sr-route`。** 磁盘上那份 bundle 里就带着这些，所以重启不依赖控制器可达。

### 1.1 控制器侧：新增什么

```
OverlayGroup（租户级，导通规则）
  id, name, rt（控制器分配，不进 UI —— 定案 6）
  members: [(node_id, vpn_id)]        ← 他的原话模型：多选设备，每行一台带 vpnID 下拉
EscapeAllColors bool（租户级，定案：租户级粒度）
```

`ConfigVPN` 不加 RT 字段 —— RT 是**组的属性**，不是 VPN 的属性。每台设备每 VPN 的
`export_rts`/`import_rts` 在**渲染时**从"我命中了哪些组"算出来。这样编辑组（减设备）
之后残留的 RT 会被自然摘掉，不需要"反向清理"这种代码。

leak 那块（第三块面板）落的是 sr-frr 的 `import` 段 + FRR 侧的 `redistribute`：
内侧→SD-WAN 选 static/connect/bgp/ospf（可过滤 prefix），SD-WAN→内侧选 none/bgp/ospf。
这一块的字段今天**完全没有对应实现**（sr-frr 只按 tag 全量反向导入，
`cmd/sr-frr/main.go:54-56`），所以它是 O6 里最贵的一块，建议单独一步、不要和前两块混。

### 1.2 渲染：bundle 第 4 段

`Artifacts` 加一个 `Route string`（内容就是 `sr-route` 那份 `fileConfig` 的 JSON，
字段名逐字照 `cmd/sr-route/main.go:40-50`）。为什么是 JSON 而不是像 vpp/frr 那样塞文本：
sr-route 只吃 JSON + HTTP，`boot.sh` 那种 vppctl 行喂不了它。

`Checks` 里加闸门（发布前拦，不指望设备侧报错）：
① 组成员的 vpnID 在该设备的基础配置里不存在 ⇒ **error**；② 两台成员之间没有隧道 ⇒
**warning，不是 error**（O6-a 落地时改的）。「配导通 → 再建隧道」是正常工序，隧道又归另一个页面管，
把它拦成 error 等于逼租户为根本不需要互通的设备对建隧道。但它必须说出来：RR 只按双向隧道邻接反射、
完全不看 RT（O5-c 实测，草案 §8.4），所以缺这一条隧道的那对成员**不会收敛，而且哪里都不报错**。
另外 O6-a 实到三条计划外的闸门：VPN 号必须 ≤ 255（`route.MaxVRFID`，VRF id 在协议里是 uint8，
超过就表达不出来）、不能是保留表 250、一个 VPN 最多进 16 个组（`route.MaxRTsPerRoute`，
超出后 RR 直接拒绝那条 announce）。

### 1.3 设备侧：谁写盘、谁推活表

**`sr-apply` 写盘**（它已经是 bundle 的安装者，`Install` + `replay` 都在它手里），
把 `route.json` 落到 `/etc/sdwan/sr-route.json`（就是 sr-route 今天读的那个路径）。
**`sr-agent` 推活表**：装完之后 `PUT /vrfs` + `PUT /tunnels` 到本机 sr-route 的状态口 ——
O5-b 开这两道缝就是为了这一刻。

这样分工的代价是"两个入口"，但各自解决一个真问题：
- 只有 agent 推、盘上没有 ⇒ sr-route 一重启就回到旧那份（**这正是今天 `PUT` 不持久那个缺陷**）；
- 只有盘、不推 ⇒ 配置变更要重启 sr-route 才生效 ⇒ **违反"配置变更永不重启别的进程"这条判据**。

sr-fib 那份（`vrfs` + `tunnels` + `escape_all_colors`）同样由 `sr-apply` 写盘，
但它**没有热更入口**（sr-fib 一轮轮询自己读 RIB，配置文件只在启动时读一次）⇒
逃生开关/VPN 集合改了要等 sr-fib 自然重启才生效。**这条要拍**（见 §3 D3）。

### 1.4 开机路径

清单不用改顺序：`apply`（重放磁盘那版，含新写的 `sr-route.json`）本来就排在
`agent → route → fib` 之前（[start.sh](../controller/deploy/sr-box/start.sh) 的顺序
`vpp → apply → frr → agent → route → fib → bridge → tunnels_check`）。
所以"重启盒子后不手敲任何命令"这一句，在 overlay 这块**由磁盘那份 bundle 保证**，
不需要控制器可达。隧道那一层仍然是"控制器可达才有"（离线开机建不出 wg 口，
因为期望值不在盘上 —— 要不要也落盘见 §3 D2）。

## 2. 开工顺序（每步一个增量、绿了再走）

| 步 | 内容 | 判据 |
| --- | --- | --- |
| O6-a ✅ | 控制器 overlay 模型 + `OverlayGroup` 的 CRUD API + RT 分配与渲染（不动 UI） | 单测：建组→渲染出的每台 `export/import_rts` 逐字对；编辑组减设备⇒残留 RT 消失；两条闸门各一条负向 |
| O6-b | bundle 第 4 段 + `sr-apply` 落盘 + `sr-agent` 推 `PUT /vrfs` `/tunnels` | lab：控制台改一次导通，**不重启任何进程**，两台 sr-route 的 `/status` 里 vrfs 变了；`docker restart` 盒子后不手敲命令，`/status` 里 vrfs 还是那份（磁盘回来的） |
| O6-c | 控制台「Overlay 配置」页：全局 + 导通两块（leak 先不放） | 真浏览器：新增规则后两行 RT 自动写入、编辑减设备后残留被摘、被丢路由三态有数 |
| O6-d | 被丢路由三态上控制台（`pending` / `local_skips` / `rt_mismatch`，走心跳摘要字段） | 控制台显示的数与设备 `/status` 的数逐字相同；设备没上报时明写"未上报" |
| O6-e | leak（第三块）：sr-frr 的 prefix 过滤 + FRR redistribute 字段 | 单独出方案，本文不展开 |

O6-a/O6-b 是后端，O6-c 才见效。**最快能让他看到东西的一步是 O6-c，但它依赖 a+b**，
所以顺序不能调。

## 3. 拍板结果（2026-10-31 已定，O6-a 就按这一节写的）

- **D1｜RT 存不存**：他拍的是**"控制器分一个 RT 值，控制器里、CPE 里都得存一份，因为这个要
  租户内唯一"**。落地形状：RT 存在控制器的 `OverlayGroup.RT`（组创建时分配、编辑不改、删除不还号），
  CPE 那份走 §1.2/§1.3 的 `route.json` 落盘。⇒ 我原来建议的"不存、渲染时算"被否了，
  但**只否掉了一半**：RT 值本身确实存了，每台每 VPN 的 import/export **列表**仍然是读时从组成员关系
  推导的。这不是绕回去 —— 反过来做（把 RT 列表抄进每台设备的 `ConfigVPN`）才会带来"编辑组要反向
  清理 N 台"那个真 bug 源，而他关心的"唯一性/重启自带"两件事由**存 RT 值**这一半就够了。
- **D2｜隧道落盘**：他说"可以落盘，为了撑住控制器故障 + CPE 重启；但你评估改动大小，太大就不落"。
  评估结论是**改动小**：`wire.UnderlayBundle` 已经是一份完整快照（`internal/wire/wire.go` 里
  `PKeys` + `Tunnels []TunnelSpec` + `Skipped`，字段带对端证书/绑定/中转段/MTU），
  而 WG 私钥**今天就已经在盒子的盘上**（`internal/agent/identity.go:30` `wg_x25519.key`，
  生成一次后复用，定案 3 只要求私钥不出设备）。⇒ 估 **40–60 行 + 单测**，落成独立增量
  **O5-e「隧道快照落盘 / 离线开机」**，不塞进 O6。真正的代价有两条，都写在 O5-e 的方案里：
  ① 租户的**跳 MAC 密钥环**（`State.PKeyRings`）会跟着落到盒子磁盘，这是今天盘上没有的一份新秘密；
  ② 离线开机恢复出来的是一份**可能已过期的**对端表（对端口被改/被吊销时，盒子无从判断），
  所以磁盘那份必须带"我是哪一版、我从哪台控制器拿的"，并且 `check` 要能说出来。
- **D3｜sr-fib 热更**：按我建议 —— 给 `sr-fib` 开一道 `PUT`（照 O5-b 的形状）。**排在 O6-b 之后单独一步。**
- **D4｜"没有不跑 FRR 的盒子"** ⇒ 不加代码闸，`sr-apply` 无条件重启 FRR 保持不动；lab 那两个
  不跑 FRR 的测试盒子是**形态不一致**，修法是把 FRR 装进 dev 镜像（待做，不改判据脚本）。
- **D5｜开工**：已按 §2 顺序开工，O6-a 落地。

## 4. 明确不做

- 不改路由协议（版本仍 3，Route Refresh 你拍了先不做）。
- 不让控制器主动连设备（CPE 在 NAT 后，拨不到；所有回报走心跳）。
- 不在控制台显示任何没有真实来源的数（被丢路由三态必须来自设备 `/status`，走心跳）。
- 不动 RR 的反射范围（定案：装表决策在设备侧，RR 不看 RT）。
