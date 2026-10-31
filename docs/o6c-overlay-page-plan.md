# O6-c 方案：控制台「Overlay 配置」两页 + 运营方的 RR 库

状态：**E0-E5 已全部拍完（2026-10-31）**，按 §5 的 C-a → C-b → C-c 开工。
E1 先拍了 (b)，我查完机制说 (b) 做不到"逐台换 RR 不断业务"，改拍 **(c) make-before-break**，设计在 §6。

## 0. 一句话

把现在只能用手敲接口的那套 overlay 配置搬到页面上：运营方维护一份 **RR 库**（手填地址 + 位置），
租户在「Overlay 全局配置」里从库里选主/备，在「Overlay 导通」里建/改/删导通组；
控制器把选中的 RR 对和导通结果渲染进配置包第 4 段，盒子照 O6-b 那条路落盘。

## 1. 今天的事实（读码 + 实测，不是推测）

> 这一节是**写方案那天（2026-10-30）**的事实，行号当时逐条核对过。C-a 落地以后，
> §1.1 里"第 4 段只有 `tunnels`/`vrfs`"和"白名单两个键"两处已经变了（现在是三个键，
> `route.go:46`），§1.2 的"后端没有 RR 库"也不成立了；以 §2/§5 为准。

### 1.1 后端已经有的

| 东西 | 在哪 | 形状 |
| --- | --- | --- |
| 导通组 CRUD | `cmd/sr-ctrl/overlay_api.go:31/49/84`，路由表 `admin.go:358-360` | `GET /api/overlay`、`PUT /api/overlay/settings`（body 只有 `escape_all_colors`）、`POST/PUT/DELETE /api/overlay/group`；全部 `requireTenant`（`underlay_api.go:28`） |
| 租户口径 | `OverlayConfig{TenantID, EscapeAllColors, Groups, NextRT, NextGroup, ...}` `overlay.go:70-86`，存在 `State.Overlays map[string]*OverlayConfig`（`main.go:293`，按租户 id 为键） | RT 由 `allocateRT()` 分配（`overlay.go:164`，ASN 65000 起、1000 起号，`route.CanonicalRT` 规范化）；上限 256 组 / 64 成员（`overlay.go:42-43`） |
| 页面要的数据 | `overlayView` `overlay.go:456-471` | 一次 `GET /api/overlay` 就返回 `groups / checks / nodes[].vrfs[].{export,import}_rts / max_* / rt_note`。**刚才我用租户 `o6b-admin` 登录实测过：`og2`、RT `65000:1001`、两台成员各自的 import/export 全在里面** ⇒ 这一页不缺接口，只缺页面 |
| 配置包第 4 段 | `overlayRouteArtifactLocked` `overlay.go:583-608`，挂在 `configArtifacts.Route`（`config_render.go:266-269`） | `overlayRouteDoc{Tunnels json:"tunnels,omitempty", VRFs json:"vrfs"}` —— `vrfs` 恒在、`tunnels` 只在租户 underlay 已发布时才有（键在不在就是指令） |
| 设备侧合并 | `internal/apply/route.go:41` `overlayRouteKeys = {"tunnels","vrfs"}` | 白名单外的键**丢弃并记日志**（写进 sr-route 不认识的键会让它**拒绝启动**）；盘上没有 `route.json` ⇒ 硬错误（已拍：统一行为） |

### 1.2 后端没有的

- **RR 库不存在**，全仓库零命中。`wire.RRAddress{ID, Addr}`（`internal/wire/wire.go:80-83`）只有一处 Go 引用：`cmd/sr-route/main.go:45` 那份设备自己的配置文件。`sr-ctrl` 里对 `rrs` 只有注释。
- **运营方今天没有任何"基础设施清单"这种东西**：`State` 里运营方拥有的只有 `Tenants`/`Claims`/`Accounts`（`main.go:165/181/273`），租户私有的有 `Sites`/`PreAuths`（`main.go:299/306`）。RR 库要新开一个 `State` 顶层字段。好消息：`State` 没有 schema 版本号，加字段不需要迁移——缺字段解成零值后由 `normalizeState()`（`main.go:433`）就地补好。

### 1.3 前端今天长什么样

- 一个手写单页：`cmd/sr-ctrl/console.html`，5456 行，无框架、无构建步骤。加一页要动四个地方：nav 锚点（`:313-329`）、`VIEWS` 表（`:637-681`）、`render()` 分派（`:713-724`）、`bind()` 里的事件（`:4831-5393`）。
- **位置已经给我留好了**：`Overlay 配置` 那一节现在只有一个死槽 `<a class="child dis">路由 · 尚未规划</a>`（`:318-319`）。
- 惯例可以直接照抄：请求走 `fetchText/api()`（`:411-427`）；错误是页面内联一条 `cfg-note`（`undNote` `:2233`）而不是弹层；删东西前 `confirm(...)`（`:2514`）；写接口返回整份文档 → 存 JS store → `render()`；可达性那一格的写法是 `undTunnelDeliveryCell`（`:3153-3166`）；**单选 = 折叠下拉 + 过滤框**（`dpHtml` `:3340`），多选才是列表；所有插值必须过 `esc()`（`:367`）。
- 角色：`GET /api/session` → `S.me`（只在内存里，靠 httpOnly cookie），导航用 `data-roles` 显隐，路由层再挡一次（`allowed()` `:516`）。

### 1.4 一条我必须先说的坑（它决定 §4 的 E1）

`rrs` 和 `tunnels`/`vrfs` 不一样：**sr-route 只在启动时读它**。
- `sessions: make([]clientSession, len(cfg.RRs))`（`internal/route/client.go:299`）——会话数组按配置长度一次性构造，换 RR 集合等于重建 client，不是给它加个 PUT 就完事；
- `sr-apply` 的热更只有 `pushOverlay`（`route.go:185`）推 `tunnels`/`vrfs`，它**不重启 sr-route**（`apply.go:765-780` 只重启 FRR）；
- 清单 `start.sh` 只有 `up|check|stop`（`:480-487`），**没有"重启某一步"**；而且 `run_stop` 的循环是 `bridge fib route agent`——**包含 agent**。sr-apply 是 sr-agent 起的子进程，谁在 apply 里调 `start.sh stop`，谁就把自己他爹杀了。这条要单独做一个只动 `route/fib/bridge` 的子命令。
- 还有一条更硬的：`rrs` 为空时 sr-route **拒绝启动**（`main.go:91`）。所以一台新盒子如果从没选过 RR，route 那一步本来就是起不来的——这不是"改了没反应"，是"第一次必须有人把它拉起来"。

⇒ 结论：**"租户选了 RR 之后多久真的生效"必须拍一个口径**（E1），我不替你决定。

## 2. 形状（按你三条新口径落）

### 2.1 RR 库（运营方拥有，全局一份）

```go
type RRItem struct {
    ID       string `json:"id"`       // rr1, rr2 …控制器编号，页面不改写
    Name     string `json:"name"`     // 手填，给人看的
    Addr     string `json:"addr"`     // "域名或 IP:端口"，就是你说的"盒子怎么找它"
    Location string `json:"location"` // 纯文本（E3 已拍）
    Owner    string `json:"owner"`    // "" = 所有租户共享；否则 = 租户 id（该租户独享）
    CreatedAt, UpdatedAt int64
}
```
- 存 `State.RRs *RRLibrary`（顶层一份**文档**：`{items: [], next_id: N, updated_at, updated_by}`，
  全局一张表但**每条带归属**，E2 已拍：运营方添加时决定这条是"大家共享"还是"某租户独享"）。
  - 为什么不是我上一版写的 `map[string]*RRItem`：map 的遍历顺序不确定，而这张表是**运营方手打的**，
    他敲进去的顺序就是他心里"RR 1、RR 2"的顺序；编号器 `next_id` 也得跟这些行放在一起，
    删一条不还号（租户里那条引用是纯字符串，还号就等于让旧引用突然指向另一台机器）。
- **可见性规则**（就一条，不复杂）：租户在页面上能选到的 = `Owner==""` 的 + `Owner==本租户` 的。
  独享条目对别的租户**根本不出现**在下拉里；控制器在保存选择时也按这条闸一遍
  （隐藏的接口用程序点也是 403，沿用今天权限收口的做法）。
  渲染时同样按这条闸一次：`Owner` 不匹配的引用 ⇒ 不发 `rrs` 键 + 一条 blocking check。
- **位置不借 `Site`**（这里更正我上一版给的默认）：`Site` 是**租户私有**的（`site.go:26-34` 带 `TenantID`），运营方没有租户身份，借它会造出一条跨租户读站点的通道，还得改鉴权。库里就存一个自由文本 `location`——你原话也是"手填即可"（E3）。
- 校验只做一条硬闸：`addr` 必须是 `host:port`（`net.SplitHostPort` + 端口 1..65535 + host 是 IP 或域名），因为设备侧 `sr-route` 是 `DisallowUnknownFields` 的严格解析（`main.go:91-96`），一个错地址会毁掉这台盒子的路由面。**不探活**（E4）。
- API：`/api/rrs` 的 POST/PUT/DELETE = `requireOperator`；`GET /api/rrs` 也是运营方页要的那份列表。

### 2.2 租户的选择（进 overlay 的全局设置）

`OverlayConfig` 加两个字段：`RRPrimary`、`RRBackup`（存的是**库里的 id**，不是地址——和 RT 一样，值在库、引用在租户）。
写在已有的 `PUT /api/overlay/settings` 上（body 多两个键），不新开端点。`GET /api/overlay` 的 `overlayView` 里多带一份 `rr_library`（`id/name/addr/location`）+ `rr_primary`/`rr_backup`，让页面一次拿全。

### 2.3 渲染进第 4 段

`overlayRouteDoc` 加 `RRs []wire.RRAddress json:"rrs,omitempty"`：
- **选了才发**：没选 → **整个键不出现**，盒子继续用盘上自己那份。**永远不发 `"rrs": []`**——`main.go:91` 会让 sr-route 拒绝启动，"清空"这个动作在设备侧等于砖机。
- 发的时候是**一个集合**，不是优先级（E0：设备两条会话同时在跑，顺序不起作用）；主备都选了 ⇒ 两条，
  只选了一条 ⇒ 一条。数组顺序只是稳定顺序（按选中的槽位固定排），页面不许把它讲成"备用了才切"。
- 引用解不出来（库里的 id 不见了）→ 不发这个键，同时在 `checks` 里给一条 blocking：「选中的 RR 已不在库里」，让发布按钮按既有闸门挡下来，而不是悄悄发一份没有 RR 的配置。
- 设备侧：**C-a 只做"落盘"这一半**。`overlayRouteKeys` 加 `"rrs"`（`route.go:46`），
  整张替换盒子里手抄的那份（和 `tunnels` 同规则），盘上其余键逐字不动；
  热推那一步 `pushOverlay` **一次都不碰 `/rrs`**（今天没有这个入口），
  但只要这段带了 `rrs` 就在 status 里落一条 note：
  「rrs 已落盘，要 sr-route 下次启动才生效（这一版没有热更换 RR 的入口）」，
  开机重放（`push=false`）不产生这条 note——那时盘上那份就是新列表。
  **C-c 才把它变成 make-before-break 的热推**（§6）。

### 2.4 页面

**运营方 → 资产 → RR 库**（新增，`data-roles="operator"`）：一张表（名称 / 地址 / 位置 / 归属 / **被哪些租户选着** / 更新），就地新建 + 编辑 + 删除（删前 `confirm`）。
被引用时能不能删 = **E5**，默认「不让删，提示是哪几个租户」。

> 写页面时改的一处：这里原计划是「被哪些租户选为主 / 备」两列。后端 `rrReferencesLocked`
> 只回租户号，而 E0 已经拍了「两格在设备上等价」——把它们再拆成主/备两列，等于在页面上
> 重新发明一个已经不存在的区别。所以是一列「被哪些租户选着」。
> 同理，租户那两格在页面上叫 **RR 1 / RR 2**，不叫主/备。

**租户 → Overlay 配置 → 全局配置**（填掉那个死槽）：
- 主 RR / 备 RR：两个**折叠下拉 + 过滤框**（`dpHtml` 单选形状），选项就是库里的条目，显示成「名称 — 地址 — 位置」；
- 逃生开关（`escape_all_colors`）已在同一个 settings 里，一并放这页；
- `overlay_cidr` **这一页连值都不显示**，只写一句「它在 Underlay 全局配置里改」——它是 underlay 的地址池（`underlay/settings.go:70`），按你定的"每页只说自己那一层"，不在这里开第二个编辑口。原计划是"只读显示"，实现时改成完全不复制：两处显示同一个数就有两处可能说错，而 GET /api/overlay 根本不回这个字段，页面要显示就得自己再去拉一次 underlay。

**租户 → Overlay 配置 → 导通**：组列表 + 组编辑（名称、成员多选、每台一个 VPN 选择），RT 只读显示并沿用接口里那句 `rt_note`；建组/改组/删组都直接吃已有端点。leak 那一块留死槽位（照今天"尚未规划"的写法），O6-e 再说。

## 3. 判据（这一版怎么算绿）

**C-a（后端，2026-10-31 已绿）** —— 下面 1、2 两条，加一条这一版才看得见的：
**运营方改一个 RR 地址 = 一次配置改动**，设备页必须说"这一版还没发布"，
而在那之前设备拉到的还是**冻结的那一版**（旧地址），再发布才另起版本号。

1. 单测：库的校验（负向：无端口、带路径、端口 0、带 scheme、裸 IPv6）；租户选了不存在的 id ⇒ `checks` 有 blocking 且渲染出的第 4 段**没有** `rrs` 键；选了 ⇒ 恰好两条、主在前；没选 ⇒ 没有这个键、也不产生 `rrs: []`。
   - 另外钉住的几条（都是页面看不见的）：独享条目对别的租户**不出现**在下拉里、
     且拒绝消息里说不出归属是谁；`next_id` 删了不还号；被引用的条目删不掉并且 409 里点名是哪几个租户；
     引用悬空（手改 state.json 才会出现）是**报出来**而不是被 normalize 悄悄修好；
     只有 `rrs` 的一版热推次数为 0，但 status 里有那条 note；开机重放不产生 note。
2. 编译 + `gofmt -l` 空 + `go vet` + `go test -count=1 -race ./...` 全绿（14 个包，2026-10-31）、
   `test-vbond.sh` 全绿（**196 passed / 0 failed**，2026-10-31 复跑）。
3. **真浏览器**（不是只看接口）：运营方建两条 RR → 租户登录，下拉里选到它们 → 保存 → 刷新还在；`GET /api/config/bundle?node=1` 里能读到那两条；导通页建组 → 两行 RT 自动写入。截图存档。**（C-b）**
4. 设备侧（lab `lab-o6b1`/`lab-o6b2`）：发布一次 ⇒ 两台 `route.json` 的 `rrs` 变成选中的那两条，**盘上其余键逐字没动**（`tenant/device_id/serial/hold_sec/local_prefixes/status_addr/identity_dir`）；**`sr-route` 的 pid 不许变**——O6-b 那条"改导通不重启任何进程"的判据不能被这一步破坏。**（C-b 之后才能整条走：要有一个能选 RR 的页面；C-a 已单测覆盖合并语义本身）**
5. 反证 E1 的那条：重启盒子（`docker restart` + 一条 `start.sh up`）后 `/status` 里连的 RR 就是页面上选的那对。**（C-c 做完 make-before-break 之后，这条要改成"不重启盒子也能换"）**

> 判据里的 VPN：这一版仍按 **VPN 0** 走（O6-b 那条约束还在）。补充一条今天查到的环境事实：
> 这台机器上有**两个 docker daemon**——lab 全套 fixture 在 Docker Desktop（内核 `7.0.14-linuxkit`，
> 复核过 `ip link add … type vrf` 仍然 `Unknown device type.`），另一个是 colima 的 Ubuntu 24.04 VM
> （内核 `6.8.0-117-generic`，2026-10-28 在那台上一次装成过 VPN 0 + VPN 100，见
> [overlay-console-plan.md](overlay-console-plan.md) 记的 #98；那五个 `vrf-*` 探针容器今天
> 是 `Exited (137) 3 days ago`，VM 还在跑）。⇒ **每条 docker 命令都要显式 `export DOCKER_HOST=`**，
> 否则同名的 `srwg-node1` 会打到另一台上。要把 VPN≠0 也纳入 overlay 判据，路是 `docker start vrf-cpe …`，
> 但那是把 fixture 换一台机器，这一版不做。

## 4. 拍板结果（2026-10-31，E0-E5 全部已定）

- **E1｜选了主备 RR 之后多久生效** ⇒ **最终已拍 (c) make-before-break**（见本节最后一条 E1 结论；
  下面 (a)/(b) 是拍板过程中量过的备选，(b) 已被 (c) 取代，保留是为了记下为什么重启不够）。
  - (a) **只落盘**：改动最小（约 60 行），页面上明写「这台盒子下次 sr-route 启动才会换到这对 RR」。代价：换 RR 或新盒子第一次装，route 那一步得有人重启，而清单现在没这个能力。
  - (b) **变了就自动重启 route 那一链**（~~就按这条做~~ ⇒ **作废，被 (c) 取代**）。
    当时记的改法是给 `start.sh` 加 `restart route`（只动 `route/fib/bridge`，**绝不能碰 agent**，
    理由见 §1.4），`sr-apply` 加一个 `RouteReload` 旋钮（照 `FRRReload` 的形状，`apply.go:374`），
    只在盘上 `rrs` 与新值**真的不同**时触发。这些量现在都不做了。
  - (c) **给 sr-route 开 `PUT /rrs`，运行时换会话集** ⇒ **已拍：就按这条做，而且要 make-before-break**
    （用户原话："逐台更换 rr……这种情况下是可以不断业务实现的，这才是主备 rr 的意义"）。
    (a)/(b) 都不满足这一条：重启会把这台盒子的**两条会话一起打断**，RR 随即撤掉它替本机宣告的前缀
    ⇒ **回程断**；而 sr-fib 只在"连不上"时保留上次计划（`cmd/sr-fib/main.go:248`），
    **读到一个空表它是真撤**，所以重启后"进程能应答但还没从 RR 学满"那个窗口连本地表也保不住。
    设计见 §6。
- **E0｜「主/备」这个词到底什么意思** ⇒ **已拍：照实**。设备今天是**两条同时在跑的等价会话**
  （`Run()` 给每个 RR 各起一条 goroutine 一直保持，`internal/route/client.go:784-796`；
  `active_rr` 只是显示用的下标），**数组顺序不起任何作用**。所以：
  上一版我给你的默认④「`rrs` 数组顺序即优先级」**作废**；页面上不写"主/备"这种暗示择一的词，
  写成「RR 1 / RR 2 —— 两条同时建会话，任一条失联另一条继续」；渲染进第 4 段时顺序只是稳定顺序。
  （另一半"只连主、失联才连备"要新写一套会话择一 + 冷备重学，比 make-before-break 更大且可靠性更差，已否。）
- **E2｜RR 库全局一份还是按租户隔离** ⇒ **已拍：全局一张表，每条带"归属"属性**
  （共享 / 某租户独享），运营方添加时选。可见性与保存闸见 §2.1。
- **E3｜"位置"怎么存** ⇒ **已拍：纯文本**。（这里更正我上一版给的默认"复用既有站点 `Site`"——
  `Site` 是租户私有的，`site.go:26-34` 带 `TenantID`，运营方没有租户身份，借它要多开一条跨租户读站点的口。）
- **E4｜库里的 RR 要不要探活** ⇒ **已拍：不探**。页面如实写「控制机没连过它，未知」，
  和隧道那一格口径一致，不造一个没根据的绿灯。
- **E5｜被租户选中的 RR 能不能删** ⇒ **已拍：不让删**，提示是哪几个租户。

## 5. 拆分（一个增量一个提交，绿了再走）

1. **C-a 后端**：RR 库 + `OverlayConfig` 两个字段 + `overlayView` 带库 + 渲染 `rrs` + 设备白名单加 `rrs` + 单测。（不动 UI，可独立回滚。）
   ⇒ **2026-10-31 绿并推出**：`cmd/sr-ctrl/rr.go`+`rr_api.go`+`rr_test.go`、`overlay.go`/`overlay_api.go`/`main.go`/`admin.go`/`config_render.go` 改，
   设备侧 `internal/apply/route.go`（`rrs` 只落盘 + note）与其单测。判据 1、2 见 §3。
2. **C-b 控制台**：运营方 RR 库页 + 租户 Overlay 全局/导通两页 + 真浏览器验证 + 截图。
3. **C-c**（E1 已拍 = (c) make-before-break）：`sr-route` 开 `PUT /rrs` + route client 会话生命周期改可变
   + 单测 + lab 双 RR 实测（设计见 §6）。

## 6. C-c 的设计：make-before-break（E1 已拍这条）

**为什么必须它**：换 RR 要"逐台换、业务不断"，那就不能让本机在任何一刻没有一条活会话在替它宣告前缀。
重启（(a)/(b)）做不到——两条会话一起断，RR 撤掉本机前缀 ⇒ 回程断。

**已具备的地基**（今天就能站在它上面改，不是重写）：路由**按会话记账**——每个会话持有自己的
adj-RIBs-in，`deriveSessionLocked`（`internal/route/client.go:743`）单会话派生，摘掉一个会话就是清它那份账
（这正是今天"某台 RR 挂了另一条继续"走的路径）。要改的只有"会话集合是静态的"这一点：
`sessions: make([]clientSession, len(cfg.RRs))`（`:299`）+ `Run()` 按下标起 goroutine（`:784-796`）。

**改法**
1. 会话键从**下标**换成 **RR 的 id**：`sessions []*clientSession`，每会话带自己的 `ctx/cancel`
   和 `RRConfig`。用指针是硬的：goroutine 按下标取元素、运行中集合会变长，值切片就是竞态。
   `Status()` 里 `active_rr` 那个下标字段跟着改成按 id 报告（它是设备自己的 HTTP 口，不是 CPE↔RR 协议，可以改；
   但 `sr-fib` 和控制台读它，改时要一起看）。
2. `SetRRs(new []RRConfig)` 把集合分成四类：`keep`（同 id 同 addr，不动）、`add`（新 id）、
   `restart`（同 id 换 addr）、`drop`（不在新集合里）。
   **先起 `add`/`restart` 的新会话，一条都不先摘。**
3. "成立"判据 = 该新会话握手成功**且本机前缀已经写给它**（`pushTunnelsLocked` 那条宣告路径成功，`:770-779`）。
   **只有成立的才摘对应的旧会话**；等不到的（新 RR 连不上/没回执）⇒ **旧的留着不摘**，
   `/status` 如实写「新 RR `<id>` 未成立，旧会话保留，配置未完全生效」。
   这一条我按"保业务优先"实现，不请你拍——你要是想反过来（超时也照摘），告诉我。
4. `PUT /rrs` **立刻返回**（202 + 当前会话集与各自状态），切换在后台推进。
   不能让 `sr-apply` 的推送等 20 秒——它给一次热更的预算是 10 秒（`routePushTimeout`, `internal/apply/route.go:43`）。
   所以 apply 的 note 是「已开始换 RR，进度看设备 `/status`」，而不是「换完了」。
5. 一个诚实的例外：**只选了一条 RR 的租户换地址，必然有窗口**——没有冗余是你配的形态，不是这里的 bug。
   页面上把这条讲明白（选一条时提示"没有冗余，换这台 RR 会有中断"）。

**lab 判据（要真 RR，`lab-rr` 今天就在跑；这一版要再起第二个 sr-rr 实例）**
- 硬判据在 **RR 侧**看，不看盒子侧：切换全程每 100ms 查一次 RR 的 RIB，断言"这台盒子宣告的前缀**从未消失**"；
  同时跨隧道打流，0 秒级中断。
- 负向：新 RR 地址指向一个没人听的端口 ⇒ 旧会话**不摘**、`/status` 有那句"未成立"，
  业务与本机宣告照旧。
- `sr-route` 的 pid 全程不变（这条判据从 O6-b 就一直成立，(c) 不会破坏它）。

**规模**：估 150–250 行 + 单测（`SetRRs` 的分类与"未成立不摘"是主体；goroutine 生命周期是最容易出事的地方）。
**它是 C-c，不是 C-a/C-b 的前置**：控制台和渲染可以先落地，`rrs` 在设备上"落盘 + 下次启动生效"是可用形态，
只是换 RR 那一刻还会断——所以 C-c 不能无限期排在后面。

前两步不需要您再拍别的口径——E2/E3/E4/E5 我按默认写，您改哪条我就调哪条。**E1 我停在这里等您。**
