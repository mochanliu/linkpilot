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

### 1.2 渲染：bundle 第 4 段（O6-a 落地后重写的一节）

原话是"`Route string` 内容就是 `sr-route` 那份 `fileConfig` 的 JSON"。**这条按字面做不到**，
O6-a 之后我把两边都读了一遍：

`sr-route` 只吃一个完整文件，且 `loadConfig` 用了 `DisallowUnknownFields`，
其中四个键**控制器根本不知道**（[cmd/sr-route/main.go:38-50,70-100](../controller/cmd/sr-route/main.go)）：

| 键 | 谁能知道 | lab 现值（`lab-cpe1:/etc/sdwan/route.json`） |
| --- | --- | --- |
| `identity_dir` | 只有盒子（agent 的身份目录，清单里是 `SR_BOX_AGENT_ID`） | `/etc/sdwan/identity` |
| `status_addr` | 只有盒子（本机监听口） | `0.0.0.0:19001` |
| `hold_sec` / `tls_server_name` | 运维选，不是租户选 | `12` / `10.99.0.100` |
| `rrs` | **控制器今天完全没有这个概念**：`wire.RRAddress` 全仓库只有两处引用（类型定义 + `sr-route` 的那个字段），`sr-ctrl` 零命中 —— sr-route 配置里那句注释"the controller pushes these at enrollment"是**没实现的话** | `[{rr-a,10.99.0.100:18446}]`（我手写的） |
| `tenant` / `device_id` / `serial` | 控制器知道（入网记录） | `tenant-a` / `1` / `lab-cpe1` |
| `tunnels` / `vrfs` | 控制器知道，且这正是 O6 要接管的两份 | 手写 |
| `local_prefixes` | 不在文件里也行 —— `sr-frr` 运行时 `POST /local-prefixes` 送 | 缺 |

所以第 4 段的形状定成**控制器只发它拥有的那两个键**：

```json
{ "tunnels": [{"peer_device_id": 2, "next_hop": "192.168.99.2"}],
  "vrfs":    [{"id": 0, "export_rts": ["65000:1000"], "import_rts": ["65000:1000"]}] }
```

键名与 `sr-route` 的 `tunnelsDoc` / `vrfsDoc` 逐字相同（[cmd/sr-route/main.go:407-458](../controller/cmd/sr-route/main.go)），
所以同一份内容既能合进磁盘文件、又能直接当 `PUT` 的 body 用，不用两套编码。
`vrfs` 来自 O6-a 的 `overlayVRFsLocked`，`tunnels` 来自控制器已有的 underlay 隧道表
（`Endpoint.NodeID` + `Endpoint.Overlay`）。

出厂时那份完整的 `route.json` 从哪来，是一个独立的问题（§3 D6），不要把 O6-b 憋成它的答案。

`Checks` 里加闸门（发布前拦，不指望设备侧报错）：
① 组成员的 vpnID 在该设备的基础配置里不存在 ⇒ **error**；② 两台成员之间没有隧道 ⇒
**warning，不是 error**（O6-a 落地时改的）。「配导通 → 再建隧道」是正常工序，隧道又归另一个页面管，
把它拦成 error 等于逼租户为根本不需要互通的设备对建隧道。但它必须说出来：RR 只按双向隧道邻接反射、
完全不看 RT（O5-c 实测，草案 §8.4），所以缺这一条隧道的那对成员**不会收敛，而且哪里都不报错**。
另外 O6-a 实到三条计划外的闸门：VPN 号必须 ≤ 255（`route.MaxVRFID`，VRF id 在协议里是 uint8，
超过就表达不出来）、不能是保留表 250、一个 VPN 最多进 16 个组（`route.MaxRTsPerRoute`，
超出后 RR 直接拒绝那条 announce）。

### 1.3 设备侧：谁写盘、谁推活表

**`sr-apply` 写盘 + `sr-apply` 推活表**（原计划让 `sr-agent` 推，落地前改了）。理由：
`sr-apply` 本来就是"把这份配置变活"的那个组件——它已经写 `/etc/frr/frr.conf` 并且重启 FRR，
`sr-apply apply --file bundle.json` 是一条完整的手工路径。推口放到 `sr-agent` 里，
手工路径就少了半边，两个入口的代价也更大。放在 `Apply` 而不是 `replay`，所以
**开机重放（`Boot`）不推** —— 那时 sr-route 还没起来，它读盘上那份就是新表。

写盘是**叠加**：把第 4 段的两个键盖到 `/etc/sdwan/route.json` 上，其余键（`identity_dir`、
`status_addr`、`rrs`…）一概不动；盘上没有这个文件时**直接报错，不猜**（猜出来的 `rrs` 比没有更坏）。
所以"盘上那份就是答案"这句话仍然成立，只是答案有两个来源，排障时要知道 `rrs` 是盒子自己的。

顺序：`replay` → `installFRR`/`reloadFRR` → 写 route.json → 推 PUT。前面的 VPP/内核没成，
就不留半张已推的路由表。

推的时候 sr-route 没在跑（或口被别人占）⇒ **记 warning 不判 apply 失败**：盘上那份已经是对的，
下次 sr-route 一起来就是新表；把一次瞬时的连不上变成"这台盒子的配置下发卡在错误里"是更大的假。
日志与 `status.json` 里必须明写"已落盘、未热更"，不写成完成。


sr-fib 那份（`vrfs` + `tunnels` + `escape_all_colors`）同样要由 `sr-apply` 写盘，但它**没有热更入口**
（sr-fib 一轮轮询自己读 RIB，配置文件只在启动时读一次）⇒ 逃生开关/VPN 集合改了要等 sr-fib
自然重启才生效。D3 已拍"照 O5-b 给 sr-fib 也开一道 `PUT`"。
**原计划把写盘那半放进 O6-b，落地时挪到 D3 那一步了**，理由不是省事：`fib.json` 的
`tunnels[].interface` 是**盒子侧才知道的键**（wg 口的名字由 `sr-agent -tunnels` 现场落地时定，
控制器今天不知道），所以那半份不能照 `route.json` 那样"控制器渲染 + 设备叠加"，得先想清楚
是控制器发还是按 next_hop 合并。同一个叠加逻辑写两遍、第二遍还建在没定的问题上，不如一步做完。
O6-b 因此只动 `route.json` 一个文件。

#### O6-b 落地后实测到的三件事（不是 bug，排障时要知道）

1. **盘上没有 `route.json` 的盒子，整次 apply 会以错误收场**，而且错误发生在其它三段
   **已经生效之后**（顺序就是 §1.3 那条：VPP → 内核 → FRR → 叠加）。lab 里 `lab-transit`
   正是这种形状（只做中转、连 `/etc/sdwan/` 都没有），而它已经在 gui-check 的导通组里
   ⇒ 一发配置它就 `ok:false` 并且按退避重试。**这是 §3 D6 的另一半**：中转-only 盒子要么由
   控制器不发第 4 段（要先有"这台盒子有没有路由面"这个字段，#68 那条），要么设备侧把它
   当"这台盒子没有路由面"跳过并明写。今天按 §1.3 的定案仍是硬报错，**待拍**。
2. **这台宿主的内核没有 `CONFIG_NET_VRF`**（linuxkit 7.0.14，`ip link add … type vrf` 直接
   `Error: Unknown device type.`）⇒ 基础配置里只要有非 0 VPN，boot 片段就失败，apply 走不到
   叠加那步。lab 判据因此用 **VPN 0 建组**（两台都在 VPN 0），推口与磁盘那两半逐字一样；
   多 VPN 的表内容另有两处证据：`overlay_route_test.go` 按 VPN 0/100 渲染，#98 在
   Ubuntu 6.8 上实测过多 VPN 的 VRF 与数据面隔离。**别再当 bug 查内核。**
3. **agent 的退避是"整台盒子"的，不是"这一版"的**：一次 apply 失败会设 `nextTry`，退避窗口内
   （5s→10s→20s→40s→…封顶 1m20s，见 `configsync.go:146`）**换一个已经修好的新版本也不会打断它**
   ⇒ 现场改完发布，若上一版还卡着失败，最长要等一分多钟才看得到新版落地。实测就是这样：
   00:22 发布 v2，盒子 00:23:15 才动手。**这条要不要改（新版本应当重置退避）是待拍**，
   不在 O6-b 里顺手改；但它必须先写进排障手册，否则"发布没生效"会被误查成下发链路坏了。

### 1.4 开机路径

清单不用改顺序：`apply`（重放磁盘那版，含新盖的 `route.json`）本来就排在
`agent → route → fib` 之前（[start.sh](../controller/deploy/sr-box/start.sh) 的顺序
`vpp → apply → frr → agent → route → fib → bridge → tunnels_check`）。
所以"重启盒子后不手敲任何命令"这一句，在 overlay 这块**由磁盘那份 bundle 保证**，
不需要控制器可达。隧道那一层仍然是"控制器可达才有"（离线开机建不出 wg 口，
因为期望值不在盘上 —— D2 已拍可以落盘，改动小，另列为 O5-e）。

## 2. 开工顺序（每步一个增量、绿了再走）

| 步 | 内容 | 判据 |
| --- | --- | --- |
| O6-a ✅ | 控制器 overlay 模型 + `OverlayGroup` 的 CRUD API + RT 分配与渲染（不动 UI） | 单测：建组→渲染出的每台 `export/import_rts` 逐字对；编辑组减设备⇒残留 RT 消失；两条闸门各一条负向 |
| O6-b ✅ | bundle 第 4 段（只发 `tunnels`+`vrfs`）+ `sr-apply` 叠加写盘 + `sr-apply` 推 `PUT /vrfs` `/tunnels` | lab 实测（2026-10-31，`lab-o6b1`/`lab-o6b2` 两台新盒子 + 控制器 `o6b-lab` 租户，见 [test/lab/README.md](../test/lab/README.md)）：控制台建组 65000:1000 一发布，两台 `route.json` 的第 4 段盖掉运维那份旧表（`peer 99`/`65000:999` → `peer 2`/`65000:1000`），`/status` 活表同一次心跳后跟着变，**`sr-route` 的 pid 从头到尾没换**（137 / 2421）；删组再发布 ⇒ 活表回到 `vrfs: []`，残留 RT 当场摘掉；把盒子改成"控制器不可达"再 `docker restart`，只敲一条 `start.sh up` ⇒ `apply 重放磁盘上那版 → v2 boot ok`、`/status` 里还是导通那张表（磁盘回来的，全程没有控制器）。设备独有键 `tenant/device_id/serial/hold_sec/rrs/local_prefixes/status_addr` 逐字没动；盘上没有 `route.json` 的手工负向按 §1.3 报错并 `rc=1`。单测/编译/harness：`go test -count=1 -race ./...` 全绿、`test-vbond.sh` 196 条全绿 |
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
  顺带把他反问的那句记在这里，因为它说明"盒子"这个词在我们之间是有歧义的：**盒子 ≠ 虚拟机**，
  指的是一台设备上那套**软件形态**（VPP + sr-apply + FRR + sr-agent + sr-route + sr-fib + sr-frr
  这一组进程）。lab 里同时存在两种形态：`srwg-node1/2`（`srwg-dev` 镜像，**没有 FRR**）和
  `lab-cpe1/2/3`（含 FRR 10.7.1 的镜像）。交付形态只有一种，就是带 FRR 的那种。
- **D5｜开工**：已按 §2 顺序开工，O6-a 落地。
- **D6｜出厂那台盒子第一次开机时，`route.json` 从哪来（O6-b 落地前新出的，需拍）**：
  §1.2 的实测结果是控制器今天不知道 `rrs` / `identity_dir` / `status_addr`，所以 O6-b 用的是
  "控制器发 `tunnels`+`vrfs`、`sr-apply` 把这两个键盖到盘上那份文件"的形状（**S1**）。
  它够用，但新盒子必须**先有一份手写的 `route.json`** 才谈得上被覆盖 —— lab 里这样没问题，
  出厂形态不行。两条路：
  - **S1（我建议先按这个走）**：O6-b 只做叠加，`rrs` 那一半留作独立一步
    「RR 地址进控制器 + `route.json` 出厂形状」（它要动 `State`、要动控制台、还得定"RR 是租户级
    还是运营方级"，是另一件事，混进 O6-b 会让这一步不绿）。
  - **S2**：先把 RR 搬进控制器，`route.json` 由控制器一次渲染完整、`sr-apply` 直接覆盖写，
    不要叠加。形状更干净（一个文件一个来源），代价是 O6-b 变大、O6-c 跟着往后压，
    而且"RR 归属"这条没拍之前渲染函数写不了。

## 4. 明确不做

- 不改路由协议（版本仍 3，Route Refresh 你拍了先不做）。
- 不让控制器主动连设备（CPE 在 NAT 后，拨不到；所有回报走心跳）。
- 不在控制台显示任何没有真实来源的数（被丢路由三态必须来自设备 `/status`，走心跳）。
- 不动 RR 的反射范围（定案：装表决策在设备侧，RR 不看 RT）。
