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

`Checks` 里加两条 error 级闸门（发布前拦，不指望设备侧报错）：
① 组成员的 vpnID 在该设备的基础配置里不存在；② 两台成员之间没有隧道（RR 只按双向邻接反射，
没有隧道 ⇒ 这一组导不通，这条是 O5-c 实测出来的，见草案 §9 O5-c 行）。

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
| O6-a | 控制器 overlay 模型 + `OverlayGroup` 的 CRUD API + RT 分配与渲染（不动 UI） | 单测：建组→渲染出的每台 `export/import_rts` 逐字对；编辑组减设备⇒残留 RT 消失；两条闸门各一条负向 |
| O6-b | bundle 第 4 段 + `sr-apply` 落盘 + `sr-agent` 推 `PUT /vrfs` `/tunnels` | lab：控制台改一次导通，**不重启任何进程**，两台 sr-route 的 `/status` 里 vrfs 变了；`docker restart` 盒子后不手敲命令，`/status` 里 vrfs 还是那份（磁盘回来的） |
| O6-c | 控制台「Overlay 配置」页：全局 + 导通两块（leak 先不放） | 真浏览器：新增规则后两行 RT 自动写入、编辑减设备后残留被摘、被丢路由三态有数 |
| O6-d | 被丢路由三态上控制台（`pending` / `local_skips` / `rt_mismatch`，走心跳摘要字段） | 控制台显示的数与设备 `/status` 的数逐字相同；设备没上报时明写"未上报" |
| O6-e | leak（第三块）：sr-frr 的 prefix 过滤 + FRR redistribute 字段 | 单独出方案，本文不展开 |

O6-a/O6-b 是后端，O6-c 才见效。**最快能让他看到东西的一步是 O6-c，但它依赖 a+b**，
所以顺序不能调。

## 3. 需要你拍的

- **D1｜RT 存在哪一层**：我建议**不存**，只存导通组，每台每 VPN 的 RT 列表渲染时算（§1.1）。
  另一条路是直接把 RT 写进每台设备的 `ConfigVPN`，好处是排障时"盘上就是答案"，
  代价是编辑组要反向清理 N 台设备、且残留 RT 是个真 bug 源。
- **D2｜隧道要不要也落盘**（离线开机也能把 wg 口建起来）。我建议**不要**：那等于把
  隧道私钥环和对端地址表复制到每台盒子的磁盘上，而且和"控制器是隧道期望值的唯一来源"
  这条既有定案冲突；代价是控制器不可达时盒子重启后隧道 0 条（清单会明说，不假装完成）。
- **D3｜sr-fib 没有热更入口**：逃生开关/VPN 集合改了，今天只能等 sr-fib 重启。
  三个选项：① 给 sr-fib 也开一道 `PUT`（照 O5-b 的形状，最干净，但要动 sr-fib）；
  ② 让 `sr-agent` 给 sr-fib 发 SIGHUP（它得先支持重载配置）；③ 接受，并在控制台明写
  "这一项要等设备下次重启生效"。我建议 **①**，因为它和"配置变更永不重启别的进程"这条判据一致。
- **D4｜lab 的 srwg-node1/2 不跑 FRR**（判据实测：`check` 报「frr 无：/etc/frr/frr.conf 是空的」）。
  你刚拍的"盒子软件统一、必带 FRR"意味着**这个测试盒子和交付形态不一致**。
  要不要我把 FRR 装进 dev 镜像、让 O5-d 那条判据跑在真实形态上？（不加代码闸这条我按你说的办了，
  `sr-apply` 无条件重启 FRR 不动。）
- **D5｜O6 开不开工**，以及按不按 §2 这个顺序。

## 4. 明确不做

- 不改路由协议（版本仍 3，Route Refresh 你拍了先不做）。
- 不让控制器主动连设备（CPE 在 NAT 后，拨不到；所有回报走心跳）。
- 不在控制台显示任何没有真实来源的数（被丢路由三态必须来自设备 `/status`，走心跳）。
- 不动 RR 的反射范围（定案：装表决策在设备侧，RR 不看 RT）。
