# 接手这份项目：agent → agent 的把手

这份文件是给**另一个 AI agent** 写的，不是给人看的介绍。它假设你从来没碰过这个仓库，
目标是你在 30 分钟内能独立动手，而且不踩我们已经付过学费的那几处。

写在最前面的一条：本文所有数字都是 **2026-11-07 由前一轮 agent 实测**，每条后面都跟了复核命令。
**动手前先复核，不要相信这份文件本身** —— 它会过期，尤其是「哪一份代码在跑」那一节。
如果复核结果和这里不一致，以你测到的为准，并且把这份文件改掉（这是它的自我维护约定）。

---

## 0. 读这份文件的顺序

1. 先读仓库根的 `AGENTS.md`（50 行，全是硬规则，会在每个 agent 会话里自动加载）。它和本文冲突时，**AGENTS.md 赢**。
2. 再读本文 §2（现在这一刻的真实状态）和 §3（在跑的产物 vs 盘上的产物：11-06 那三只雷已在 11-07 收口，判据仍在这节）—— 这两节决定你第一条命令该敲什么。
3. 要改代码：读 §5（唯一合法的生效路径）+ §9（未决队列，owner 已经点名的活）。
4. 要下结论/交差：读 §6（读数口径）+ §7（测量陷阱）+ §8（验证基线）。

owner 的三条协作习惯，违反任何一条都会被退回，这里先说清楚：

- **先分析、后方案。** 他说过的原话："你不要每次都动不动就说解决方案是什么，因为你的分析得到我的认同前，
  你想的解决方案毫无意义。" 分析没被他点头之前，不要提交方案，更不要提交代码。
- **"修复"只指修软件缺陷，不指让 lab 看起来正常。** 不许用手工 `ip route`、手改 vtysh/FRR 配置、
  重启进程这类办法把现象压掉（他的原话："也就是说不允许手动方法修复"）。
- **controller 的每个增量先出方案、他点头后再写码；一次只推进一个缺陷。**

### 0.1 上车先跑这四条（全只读，跑完你就知道本文哪一句过期了）

```bash
git log --oneline -5 && git status --short                       # HEAD 与别人留下的散工
VBoxManage list runningvms                                        # 应是 7 台：CPE1-4 cust12 cust3 cust4
ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep # 控制面在不在 Mac 上（§2.1）
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do pid=$(pgrep -x $x|head -1);
  printf "%-9s run=%s disk=%s\n" "$x" "$(sha256sum /proc/$pid/exe|cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x|cut -c1-16)"; done'            # §3 那张表还成立吗
```

第四条如果哪一行 `run != disk`，说明有人换过件没装盘（或有第二 agent 在铺产物）—— 先问 owner 再动手，
别直接重启去"对齐"。第二条如果 7 台不全在跑，你的现场是别人的半成品，先声明再说。
`vm-verify.sh`（约 4 分钟，2026-11-09 改口径后实测）和 `exp-273/six.sh` 同样随时可跑。
**owner 2026-11-09 已撤掉这套研发台的窗口审批**（§10 有原话），所以"跑的时候确认没人在注入故障"
不再是审批前置，只剩一条物理事实：对面正在注入时你量到的数不是基线。
**但 owner 可以对某一次实验单独下冻结令** —— 例如 2026-11-09 #285 交付后他说「lab 先放那里不动，
我自己测一下再说」，那就是**只读都不必跑、写一律禁止**，直到他解冻。冻结令优先于本节其余各条。

---

## 1. 这个项目是什么

一套自研 SD-WAN，一个项目两半：

- **数据面**：VPP 插件 `sr_wg`（SR-over-WireGuard：隧道里再叠源路由头），C 写的，在 `vpp-plugin/`。
- **控制面**：Go 写的控制器 + 盒内一组 daemon（`sr-agent` / `sr-apply` / `sr-route` / `sr-fib` / `sr-frr`），
  在 `controller/`。

我们的"BGP"那部分**不是 FRR 的 bgpd**，是自己写的 `sr-route`（Loc-RIB 形状照抄 GoBGP）+ `sr-rr` 反射器，
走 mTLS，协议 v3 的条目是 `{DeviceID, VRFID, Prefix, RTs}`；下一跳在**本地**从隧道表解析。
盒内 FRR 只负责客户侧那一半（zebra/bgpd/ospfd），`sr-frr` 是它和 overlay 之间的双向搬运工。
定案口径：**与真 BGP 只差两条**（信道用 mTLS、RIB→VPP 自己装表），其余照抄。

一台 CPE 盒里从上到下的接缝（改路由相关代码前必须能在脑子里跑出这张图）：

```
sr-route Loc-RIB
   └─ frr.PlanFrom ──► 内核 K 路由(netlink, distance 0) ──► zebra redistribute kernel ──► 客户侧（正向）
   ▲
   └─ ZAPI 订阅(按 VPN/来源) ──► to_sdwan 白名单减去已导出 ──► PUT /local-prefixes（反向）
                        ▲
                        └── sr-route 开机 GET /local-prefixes（反向的"新客户端回说"，2026-11-09 起）
```

反向这条接缝上，**2026-11-07 起没有任何"等一拍"的窗口**：`Importer.Reset()`
（[controller/internal/frr/import.go:214](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/internal/frr/import.go#L214)）
在与 zebra 重连的那一刻清空本地前缀集，新连接的回说什么就是什么。
**2026-11-09 起另一头同理**：反向上报是变更驱动的（集合没变就一个请求都不发），所以 `sr-route` 单独重启
原本会把本盒前缀对全网抹掉直到下一次变动 —— 现在它一起来就经 sr-frr 的控制口拉一份全量
（[controller/cmd/sr-route/pull.go:61](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-route/pull.go#L61)），
sr-frr 应答即降级自己的 `lastSeen`（`localReporter.demote()`），拉取与推送的先后次序不需要版本号也不需要对齐时钟。
曾经有过四个窗口（A `BeginResync` / B `BeginLinkWindow` / C `Settled` / F `onceWindow`），
整族在增量 #274 撤回，理由写在 AGENTS.md 第 3 条。**不要把这些窗口重新加回来**，
包括看起来无害的"重连后先维持旧值 500ms"。

---

## 2. 现在这一刻的真实状态（2026-11-07 实测）

### 2.1 控制面跑在 Mac 上

| 件 | pid | 参数要点 |
| --- | --- | --- |
| `sr-ctrl` | 52092（2026-11-13 21:06 起，HTTPS 版） | `-data /Users/louis/srvmlab/ctrl/data -listen 0.0.0.0:8444 -tls-san 192.168.1.4 -console 0.0.0.0:8443 -console-tls -console-san 203.0.113.10 -console-http 127.0.0.1:8088 -stun 192.168.1.4:3478,3479 -hb 5` |
| `sr-rr` ×2 | 40616 / 40618 | `-config /Users/louis/srvmlab/state/rr-a.json`、`rr-b.json`，监听 18446+19446 / 18447+19447 |

复核：`ps -eo pid,etime,command | grep -E "sr-ctrl|sr-rr" | grep -v grep` 和
`lsof -nP -iTCP -sTCP:LISTEN | grep -E "sr-ctrl|sr-rr"`。
**注意**：那条 `ps` 里能看到运营方令牌的值 —— 那个值**不许进任何文件、日志、提交信息**（AGENTS.md §3）。
需要它的时候从 `/Users/louis/srvmlab/ctrl/data/console-token` 读，或者当场 `ps` 看一眼。

Mac 的 LAN 地址是 `192.168.1.4`。

**2026-11-13 晚（owner 拍板）：控制台改 HTTPS，供外网访问。**

先是把 `-console` 放到 `0.0.0.0:8088`（明文），owner 随即要求上 TLS，于是同一个
晚上做成：`sr-ctrl` 自带 TLS 监听器，不引反代。启动参数（`start-sr-ctrl-detached.py`
与 `~/Library/LaunchAgents/com.louis.sr-ctrl.plist` 两处都改，plist 备份
`com.louis.sr-ctrl.plist.bak-20261113`）：

```
-console 0.0.0.0:8443  -console-tls  -console-san 203.0.113.10
-console-http 127.0.0.1:8088
```

三条监听器同时存在，各管一段：

| 监听 | 协议 | 谁用 |
| --- | --- | --- |
| `0.0.0.0:8443` | HTTPS（自签） | 外网/局域网浏览器，**唯一对外入口** |
| `127.0.0.1:8088` | 明文 HTTP | 本机脚本（`test/lab/vmlab/*.sh`、临时 curl）不用改 |
| `0.0.0.0:8444` | TLS+mTLS | 设备面，未变 |

**证书**：`<data>/console-tls/cert.pem|key.pem`（私钥 0600），首次启动自签、之后
复用（重启换指纹会把已经信任它的浏览器反复踢回警告页）。自签是唯一现实解 ——
没有域名就没有 ACME；证书带 `IsCA + KeyUsageCertSign`，**就是为了能装进钥匙串
当根证书**，装完浏览器不再报警：

```sh
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \
  /Users/louis/srvmlab/ctrl/data/console-tls/cert.pem
```

SAN 自动收本机名字：`localhost`、主机名、`127.0.0.1`、`::1`、所有网卡地址
（`192.168.1.4`、两个 `2408:8207:…` 公网 v6、`…::2`），再加 `-console-san` 点名的
公网 v4。**公网 v4 是动态的**，变了要改 `-console-san` 并删掉 `console-tls/`
重启重签（日志会明说"证书里没有 X"）；同一张证书在 IPv6 那条路上不受影响。

**会话 cookie**：`Secure` 由**每条请求的 TLS 状态**决定，不是全局开关 ——
否则 loopback 明文口写不进 cookie、或者反过来白白少一层保护。实测 HTTPS 登录
返回 `Set-Cookie: …; HttpOnly; Secure; SameSite=Strict`，走 8088 明文则没有
`Secure`（`console_tls_test.go` 两个方向都钉住）。

**验证（2026-11-13 21:06，全部通过）**

| 项 | 结果 |
| --- | --- |
| 回环 / LAN IP / 公网 IPv6 | 均 200；后两者用 `--cacert` 校验通过（SAN 正确，不是靠 `-k`） |
| 协议 | `HTTP/2`（Go 的 ListenAndServeTLS 自动开 h2） |
| 明文打 8443 | `HTTP/1.0 400 Bad Request`（Go 的"你对着 HTTPS 端口说了 HTTP"），明文服务不存在 |
| 跨机 | CPE1 → `https://192.168.1.4:8443` 200、登录 `{"ok":true}` |
| 副作用 | 控制通道断 ~5 秒后四节点重连；6 条隧道在、t10 FEC 仍 1:2（parity=seq）、抽测 0% 丢 |

**已知取舍**：① 自签证书在**没装钥匙串的客户端**上仍会报警一次，这是自签的
本质，不是配置错；② 没加 HSTS —— 自签 + 动态 IP 下 HSTS 会把"证书过期后回退
明文"这条路堵死，等有域名再上；③ 登录失败锁定按 `用户名|RemoteAddr`，直连
（当前形态）是真实客户端 IP，将来若加反代就会塌缩成同一个桶。

**2026-11-12 增量（实测）：sr-ctrl 的启动方式变了，`~/Library/LaunchAgents/com.louis.sr-ctrl.plist` 是死的。**
launchd bootstrap 这份 plist 一直报 EIO（历代 sr-ctrl 其实都是脚本 nohup 起的，launchd 从没管过它）；
agent 会话里直接 `restart-ctrl.sh`（内部 nohup）起的进程会**随 agent 会话退出被杀**（2026-11-12 实测 08:42 起的进程 2 分钟内消失）。
现在改用 **`python3 /Users/louis/srvmlab/start-sr-ctrl-detached.py`**（double-fork + setsid，token 从
`~/srvmlab/state/console-token` 读，不进命令行）。换件步骤：`install` 新二进制到 `~/srvmlab/ctrl/bin/sr-ctrl`
（先 `cp -p` 一份 `.prev-*` 备份）→ `kill -TERM $(pgrep -x sr-ctrl)` → 跑上面那个脚本 → 30 秒内四节点重连。
当前代际：sr-ctrl `c9d99da3c88f0432ad1351482f2df4a3`（2026-11-14 19:55，
台账保留期可设置：`State.LedgerRetentionDays`（全站一个数，1–365 天，0=默认 30）；
`GET/PUT /api/alerts` 带 `retention` 视图 / 可选 `retention_days`（越界 400 且整份不动）；
页面「台账与告警」新增「台账保留」面板，改小先 confirm，2s sweeper 用
`pruneLedgerIfDueLocked`（日期变了或保留期变了都清）。测试 `TestLedgerPruneFollowsSetting`、
`TestAlertsRetentionAPI`、`TestConsoleLedgerRetentionWiring`。备份 `sr-ctrl.prev-retention-20261114`。
上一代 `90f4dabfd6c233d5586d6ecd9a9509dd`（2026-11-14 10:30，
隧道表格批量配置：`GET /api/underlay/tunnels/csv` 导出当前隧道表（15 列，UTF-8 BOM），
`POST /api/underlay/tunnels/import` 上传改过的表。一行一条隧道：隧道ID 空着=新增、
填着=修改、「操作」列写删除（或勾「以文件为准」，文件里没提到的现有隧道一并删）；
先 dry 预览再确认写入；任何一行有错整份拒绝（半份落库后文件就不再描述控制器）。
列定义只在 `underlayCSVColumns()` 一处（导出表头、解析校验、页面图例三处共用，
console 测试钉住不漂移）。上传走 base64+JSON——requireSession 规定控制台 POST
必须 JSON，同时 base64 才能让控制器对着真实文件说「这不是 UTF-8」（GBK 另存会被
明说拒绝）；设备可按序列号 / node-N / 纯数字指名，中转路径三写法：空=直连、
`模板:名称`=引用共享模板（表格不创建模板）、`a>b>c`=内联链。页面在
Underlay › 隧道配置 新增「表格批量配置」面板；`underlay_csv_test.go` 十个用例
（回传不变、一文件三改、prune opt-in、坏行阻断、分配器拒绝原子性、
租户隔离、歧义设备名、模板按名、坏文件四类）。真浏览器（Playwright+chromium，
脚本 /tmp/lp-pw/csv.mjs）实测：下载 6 行 → 原样回传 6 keep → 改1删1增1 预览
逐行正确 → 取消不写。备份 `sr-ctrl.prev-csv-20261114`。上一代
`d73329f9bc167272ad7f66d6c68c1749`（2026-11-14 10:20，
选中条目画面不动：render 的焦点护栏（焦点在 #content 输入控件上整帧跳过）
吞掉了选中那一帧——条目 mousedown 的 preventDefault 让输入框一直带焦点；
两个选中回调改为先 blur 再 render（histBlurForRender）。此 bug 由 headless
Chromium + CDP 监听器检查定位，jsdom 抓不到（手动 focus 不改 activeElement）；
调试件在 /tmp/lpevt/（jsdom 事件模拟 sim.mjs + Playwright live*.mjs，
chromium 在 /tmp/pw-browsers）。上一代
`c32a8e703fd2d1c8d288dc5e4c78ec8b`（2026-11-14 09:10，选择器
卡顿三连修：loadHistory 只拉当前设备（默认全量 18MB，WAN 上堵死交互）；
选择器冻结提前到 mousedown（histHold）+ render 加 freezeAt 60s 过期安全阀；
换设备清 S.history 并显示「正在拉取」提示）。
上一代 `fbf68651e26cdee1aaf706938c08d4ba`（2026-11-14 08:35，趋势图加
第二级隧道选择器（hist-tq/hist-tpick，可搜索，匹配对端/颜色）：单设备可能上千条
隧道全画没法看；隧道清单来自当前设备采样，选中就地重画不重拉；histCharts 按
"对端/颜色" 键过滤，换设备清零隧道选择；两个选择器共享冻结位）。
上一代 `a8346c270413418e86c294f304222640`（2026-11-14 08:00，监控页
趋势图设备选择改搜索选择器 + 冻结护栏：/api/history 按台过滤本就正常，「图不变」
是图上不标设备 + 5s 轮询把展开的下拉拆掉没点完的选择；面板标题带设备名、图例
反查对端序列号、S.freeze 展开期间冻结重绘切页解冻、mousedown 选中，测试钉五处）。
上一代 `6588070e49f01dfda3bf8cfd04ccadd1`（2026-11-14 07:30，两件事：
① 基础配置第 8 个模块——VRRP 从接口配置页独立成页 `config-vrrp`，模型不拆，
接线五处照旧由 console_nav_test 钉住；② 界面文案删掉实现机制/修改过程说明
（FRR/zebra/linux-nl/VPP 内部、sr-svc/sr-fib 分工、配置包第 N 段、
preference=230+颜色等），只留客户怎么配的指导，sr-apply 设备侧命令保留。
owner 还问过中英文切换，拍的是「先不做」）。上一代
`6c0067538e9c660eb6e9fe0cfb27b2dd`（2026-11-14 06:40，NAT 页残留
的「暂不下发」说明删除——nat_pending 阻拦机制本已在 7daa66d 随形状 G 下发落地而
删除，但上一轮拆模块时误把 notes 里的死文案又接回页面（c8423ee 把显示从 doc.
nat_pending 改读 notes.nat_pending，等于把 7daa66d 已删的旧说明复活了）；本轮删掉
notes.nat_pending 条目 + 页面横幅 + config_render.go 注释里的过时定性，测试反向
钉死页面不得再引用 nat_pending。NAT 本身照常下发，与上一代功能面相同）。上一代
`320f03fc716d6dd23b084c77c7710a76`（2026-11-13 23:05，「基础配置」
拆成 7 个模块：设备信息 / 接口配置 / 静态路由 / 动态路由 / NAT / QoS / 服务配置；
服务配置从 Overlay 组移到基础配置组下，NAT 从它里面独立成页，两页共用同一份
ServicesSet 与同一台当前设备（`svFollowCfg`），侧栏改成 flex+滚动。功能面与上一代
逐字相同。上一代 `b0e7a0b8a379da9dbe6cfd39834810c8`（2026-11-13 22:47，修两件事：
① SD-WAN 策略页打不开 —— /api/policy 的 colors 是 []uint8，JSON 编成了 base64
字符串，前端 render 抛 TypeError 整页冻结；DTO 改 []int + render 加 viewCrash
兜底 + store 加 node 不匹配护栏。② 世界底图俄罗斯方向横贯一条直线 —— 生成器
用全经度矩形裁剪不切日期变更线，改回 d3 默认 antimeridian 裁剪，纬度裁剪改由
fit 框承担；gen.mjs 加了"框内不得有横向长直线"检查），备份
`sr-ctrl.prev-modules-20261113`；上一代备份 `sr-ctrl.prev-natnote-20261114`、
`sr-ctrl.prev-vrrppage-20261114`、`sr-ctrl.prev-histpick-20261114`、
`sr-ctrl.prev-picklag-20261114`、`sr-ctrl.prev-focusguard-20261114`、
`sr-ctrl.prev-csv-20261114`、`sr-ctrl.prev-retention-20261114`；再往前
`6d775b48…` 是拓扑图嵌 Dashboard 那版，备份
`sr-ctrl.prev-fix-20261113`；`5e8fe94f…`（LinkPilot 改名）备份
`sr-ctrl.prev-topo-20261113`、`f5dc4d3a…`（HTTPS）备份
`sr-ctrl.prev-brand-20261113`、`e2a787e5…`（FEC）备份
`sr-ctrl.prev-fecui-20261113` / `sr-ctrl.prev-fec-20261113`）。上一代功能面：
sr-ctrl `76f9c959f0ca0a9fba33e5e50542ebb7`（2026-11-13 深夜，M-3 抬门：
同对端第二条隧道走密钥槽派生 `allocPairSlot`（上限 8，`ErrPairFull` 409）、
Tunnel 视图/控制台新增 slot 字段、Fan 每对仍只建一条；实机 1↔4 对 t10(w10)+t16(w30)
CPE1 实测分流 25.9%:74.1%（期望 25:75），见 fec-and-lb-plan §7。备份
`sr-ctrl.prev-m3-20261113`；上一代 `2d7911e9dbc985b0a79dca68713d3f68`（隧道 weight/
动态第二棒 + sla 连接代数修复）备份 `sr-ctrl.prev-selgen-20261113`，再上一代
`bab09756a60d125bb314ae5b4464325c` 备份 `sr-ctrl.prev-20261113ucmp`；sr-ctrl 全套测试跑在
qoslab 容器里，交叉编译 `GOOS=darwin go build`）。同日新增服务面：`/api/services` GET/PUT + 控制台「服务配置」页 +
bundle 第 8 段（NAT/DHCP/DNS）；CPE1 盒内新增 daemon `sr-svc`（DHCP server + DNS 转发器，状态口 127.0.0.1:8092）。

**2026-11-13 傍晚换件（实测）**：四台 CPE 盒 sr-agent `abca7697b55144f83d7cc8cf1221a624`
+ sr-fib `d07ba51e3ff991b52d44a3fde789231d`（隧道 weight 链 + sla 选路器"连接代数"修复：
重拨换代后强制幂等重写选路表，盖掉 apply 重种的 0/0 drop——CPE1 实机那次跨站全黑就是它）。
盒上备份 `.prev-agent-20261113selgen` / `.prev-fib-20261113selgen`。换 CPE1 时撞上**磁盘满**
（2.4G 的盘被历代 .prev/.prerun 备份塞死，guest 直接僵死、连 ARP 都不回，VBox 硬复位救回），
已把 `/usr/local/bin` 里老世代备份清掉（191M）——**抬盒前先 `df -h` 看一眼**。
客户机测跨站连通性必须**绑 loopback 源**（`ping -I 172.16.x.1`）：LAN 子网源（172.31.x.x）
不在 overlay 宣告里，靠它测必然 100% 丢，那是路径不存在，不是坏了（2026-11-13 实测教训，
浪费了半小时以为数据面全黑——其实只有 CPE1 选路表那一处）。

**2026-11-13 深夜换件（实测）**：四台 CPE 盒 sr-agent `3f237ac9e7f7d6d50b0b59711b89b495`
（动态 weight 第三棒：丢包四档非线性 + 3×5s 迟滞，见 fec-and-lb-plan §8；盒上备份
`.prev-agent-20261113dyn`）。sr-fib `d07ba51e3ff991b52d44a3fde789231d` 与 host sr-ctrl
`76f9c959f0ca0a9fba33e5e50542ebb7` 未动。（当晚 FEC 管理面又换了一代：
sr-agent `8cd9e311…` + sr-ctrl `e2a787e5…`，见 §2.1 末段。）**lab 现存演示对**：t10（w10 静态）+
t16（w30 动态，slot 2）——CPE1 表 3000 实测分流 25.9%:74.1%，动态档在 CPE4 VPP 崩溃
窗口里实机走了六次降/恢复。两条硬教训：① **这台 26.06 的 acl 插件挂接口即崩 VPP**
（acl_interface_set_acl_list，acls=[0] 和 [1] 都崩，CPE4 硬复位两次）——别再试；
② 死地址端点（填 192.168.1.250 这类）造不出永久丢包：**VPP WG 有端点学习**，
对端握手一到就自愈。另：VPP 崩过之后盒上接口的 sw_if_index 会变，工具按老索引
操作前先 `show interface` 确认。

### 2.2 被测现场是 VirtualBox 那 7 台

```
VBoxManage list runningvms   →   CPE1 CPE2 CPE3 CPE4 cust12 cust3 cust4
```

- CPE1–4 = 192.168.1.101–104（Ubuntu 24.04.5 / kernel 6.8.0-147 / 2 vCPU / 2048MB，
  盒内 VPP v26.06-release + FRR 10.7.1）；
- **进盒子只能走 `~/.ssh/config` 里的别名**（`ssh CPE1`、`ssh cust12`、`ssh cust3`、`ssh cust4`），
  直连 IP 会被拒；
- 真机 `192.168.1.100` 是别人的现场，**只读**，不在任何改动范围内。

### 2.3 旧 docker lab 还在跑 —— 这是个陷阱，先认清对象

`docker ps` 现在能看到 `lp-cpe1..4`、`lp-rr`、`lp-cust3/4/12`、`lp-term3/4/12`（全部 Up 4 天，2026-11-07 复核）。
**被测现场不是这些容器**，它们是 docker 时代遗留的 lab（S8 那步"用户关 docker"至今没做完，任务 #244）。
现在 docker 只剩一个用途：

- `lp-ctrl` = **编译/单测容器**，go1.22.5，bind mount 三个目录
  `controller` / `test` / `vpp-plugin` → `/workspace/{controller,test,vpp-plugin}`。
  任何 go 构建、`go vet`、`go test` 都在这里跑，Mac 上没有 C 工具链也没有 go。

所以：**改代码在 `lp-ctrl`，量数据在 VirtualBox**。别把结论从容器里搬出来。

### 2.4 验证基线（今天复测过）

| 判据 | 今天的读数 | 命令 |
| --- | --- | --- |
| VM 台整体验证 | **PASS=398 FAIL=0 WARN=0**（2026-11-09 实测，exp-285 改口径后） | `bash test/lab/vmlab/vm-verify.sh`（约 4 分钟，只读） |
| Go 单测 | 全绿，仓库内 `func Test` **749 条**（`internal/frr` 41、`internal/route` 72、`internal/underlay` 58） | `docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'` |
| §9.7 六方向客户流量 | 0% 丢包 | `TAG=xx bash test/lab/vmlab/exp-273/six.sh` |

`vm-verify.sh` 结尾自己会写明本轮没覆盖什么（独立终端机、站点 12 的 VRRP），那两行要照抄进你的结论里，
不要说成"全绿所以全通"。

**392 → 398 这 6 条差在哪（2026-11-09 实测，逐条对过两份输出）**：§6 的「每个 RIB 下一跳都要有
`mpls via`」换成两条更严的判据 —— ①「下一跳的路径已装且 resolved」（单前缀查询，10 条）
②「承载集 == 最优 pref 的那组路径」（8 条），净 +8 条；原来那 1 条 FAIL 转成 PASS（+1）；
§3 里 `t13 颜色 0` 两条 `ok` 因为台账颜色真的变成 2 而降成 `note`（−2）。**不是产品变好了 6 条，是口径换了。**

**现在的 lab 形状不是 #284 提交时的基线**：t13（CPE2↔CPE3）颜色 **0→2**、四台逃生开关 **全 true**，
这是 exp-285 为了造出「主/备」故意改的（全走产品路径：控制台改色 → 发布 v23 → agent apply；
逃生开关走 `ESCAPE=true vm-fib-sync.sh`）。所以 CPE3 的 `172.16.12.1/32` 现在是
`len:2`、pref 10 承载 / pref 12 挂着不承载，CPE2 朝 `172.16.3.1/32` 只有 pref=12 一条腿。
**还原配方与"为什么必须开逃生开关"在 [test/lab/vmlab/exp-285/README.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-285/README.md) §五**，
两步都不重启进程。台账要跟着重生成：`test/lab/vmlab/vm-ledger.py > /tmp/vm-verify-ledger.tsv`
（旧台账里 t13 还是颜色 0，§3 会照着旧值判）。

### 2.5 盒内进程与 systemd 单元的真实形状

四台的 `sr-box.service` 现在都是 `failed`（CPE3 是 `inactive`），**这不是新故障，别去追**。
它是开机清单那一次 `start.sh up` 以 exit 1 收尾留下的状态，日志里原因写得很清楚：

```
tunnels     0 条：VPP 里的 wg 口是 sr-agent -tunnels 从**控制器**取期望值建的，
              磁盘上没有那份表。控制器不可达时开机就是这个形状。
```

隧道表磁盘恢复不了曾是已知缺口（G-2）。**产品修复已落地（`cfb3dee` ②）**：`internal/agent/fibtable.go`
的 FibTableSync 借心跳节拍每轮派生（GET /config → 只换 vrfs/tunnels → 不同才 PUT），**不落盘是有意设计**
（盘上存一份，开机就得在"用盘上那份"和"重新派生"之间选，选前者正是"基线是假的"那类缺陷的源头）。
原 lab 手工补表脚本 `vm-fib-sync.sh` 已随之退役（仓库里已删），别再找它。
真正让 daemon 常驻的是 `sr-box-watch.service.timer`（60s 一轮，Type=oneshot，`KillMode=process`）。
`KillMode=process` 是硬要求：默认的控制组 kill 每 60s 把 nohup 起来的 sr-route/sr-frr SIGKILL 掉
（2026-11-05 23:08 实测 sr-route 在 72s 采样里只活着 47.0–49.1s）。复核：
`ssh CPE1 'systemctl status sr-box --no-pager | tail -12'`。

### 2.6 VPP 插件已换件（2026-11-12，CPE4）

`sr_wg_plugin.so` 现在带 QoS 节点 **`sr-wg4-qos`**（`vpp-plugin/sr_qos.{c,h}`）：
**挂在入口弧 `ip4-unicast`**，4 队列（q0 LLQ + q1..3 DRR）+ 每隧道令牌桶整形，
按内层 DSCP 分类、每包查一次"这个包会从哪条隧道出去"（控制面解析 + 2s 缓存），
出队回注 `ip4-lookup`。**默认全关**：`set sr-wg qos-hook enable <入接口>` 才参与，
`set sr-wg qos rate <wg 口> <kbps>` 才整形（rate=0 完全直通）。
为什么不在出口弧、为什么用控制面解析：`docs/qos-path-c-plan.md` §10/§11。

**2026-11-12 21:2x 回退，23:2x 又装回去（结论已改）**：挂点改成**入口弧
`ip4-unicast`** 的版本（O3，qos3）第一次上盒后数据面瘫痪（无钩子也瘫、整机重启也瘫，
`docs/qos-path-c-plan.md` §10.6），当时把 CPE4 的盘挂到 CPE1 上拷回
`.bak-qos3-20261112`（20:12 的 `ip4-output` 版）。

**当晚深夜同一版本又部署了 4 次，全部健康**（`/var/log/qos-deploy/result.txt` 全 `OK`），
容器里 6 案例复现也全 PASS（`test/lab/vpp-qos/lab-o3-repro.sh`）。所以 §10.6 的
"上盒即瘫"**不能归因于 O3 的弧选择**，根因仍未明；纪律从"O3 禁部署"改成
**"换件必须带看门狗 + 备份"**（见下）。

**2026-11-13 已换代为 §13 版（1.1.0，qos8）——四台全部换齐（CPE1/2/3 当天上午，零失联）**：物理口统一调度
（`sr-wg4-qos` 退化成标记器 + 新增 `sr-wg4-qos-out` 挂 interface-output 弧，
CLI 全默认关）。实机精确测量与两个环境坑记在 `docs/qos-path-c-plan.md` §13.5。
CPE1/2/3 的换件用的是 CPE4 同款纪律的移植版：脚本与 `.so`（md5 `c1cec62f…`）经
CPE4→目标盒的盒间 HTTP（226KB/s）铺到 `/root/sr_wg_plugin.qos8.so` + `/root/qos-{guard,deploy}.sh`
（guard 与 CPE4 版的差异：BAK 在 sleep 180 **之后**才解析 `bak-qos4-*`——换件先起看门狗时盘上还没有备份；
锚点按盒定制：CPE1/2=192.168.1.1+172.16.12.1，CPE3=192.168.1.1+172.16.3.1，预检实测可达），
`systemd-run` 先 qosguard 后 qosdeploy，三台 result.txt 全 OK、wg 隧道自动重建、BFD 0% 丢。
四台回滚底都是盘上 `sr_wg_plugin.so.bak-qos4-*`。
**在 CPE4 上排查流量别用大容量 `trace add`（>100 会把 256M 主堆的 VPP 打崩，
栈=`vlib_add_trace` 堆耗尽）**，用 `pcap trace` 或小容量 trace；
VPP 重启后 agent 自动重建隧道（~1-2 分钟），期间转发是断的，别误判成换件事故。
四台现跑 1.1.0（md5 `c1cec62f…`，`show plugin` 版本号可验），
`show sr-wg qos-hook` / `show sr-wg qos` 确认（CPE4 已由管理面下发开启，见下）。
（2026-11-13 傍晚已被 1.2.0 换代，见下一条；本段留作换件纪律出处。）

**2026-11-13 傍晚已换代为 1.2.0（FEC，`sr_fec.{c,h}` + `sr_wg_fec_set`）——四台全部换齐**：
宿主 Mac 构建（qoslab 容器，md5 `5533a392…`）→ `scp` 到 `/root/sr_wg_plugin.fec1.so` →
`systemd-run` 先 qosguard 后 qosdeploy（CPE4 用 `qos4-deploy.sh`，其余 `qos-deploy.sh`）。
四台 `show plugin` 全 1.2.0，`show sr-wg fec` 全 off（绑定活着），t9/t10/t11/t13/t14/t16
十二方向 `vppctl ping` 全 0% 丢（换前换后各测一轮）。回滚底：四台盘上
`.bak-qos4-<换件时刻>`（= 1.1.0 `c1cec62f…`）。方案/判据/坑：`docs/fec-and-lb-plan.md` §9.9。
**两个新教训**：(1) CPE4 换件后 `restart vpp` 数据面 3 分钟内全通（看门狗 OK）但 vpp0
管理口没回来，ssh 失联——"ssh 不回来"≠"换件失败"，先看 vpp.log 是否在响应 CLI /
从别的盒 ping 它的 WAN IP，4 分钟还不行就整机 `VBoxManage reset`（30 秒恢复）；
(2) 管理面（新 sr-ctrl/sr-agent）当时还没铺：四台 agent 仍旧版，不发 `sr_wg_fec_set`，
实机 FEC 处于"插件就位、管理面待铺"。

**2026-11-13 晚 FEC 管理面铺设完毕（四台 agent + host sr-ctrl，t10 已开 1:2）**：
`sr-agent` 换到 `8cd9e311…`（含 `ApplyFec`，盒上回滚底 `.prev-agent-20261113fec`
= 动态 weight 那代 `3f237ac9…`），host `sr-ctrl` 换到 `e2a787e5…`
（`b1a4ccd1…` 是 FEC 全链那版，`e2a787e5…` 另带控制台勾选框改动；回滚底
`sr-ctrl.prev-fec-20261113` / `sr-ctrl.prev-fecui-20261113`）。四台 delivery v46 全 `ok`。
控制台隧道表单从"下拉里塞个关"改成 **勾选框「启用」+ 档位下拉 1:1…1:4**。
实机 `PUT {id:t10,fec_ratio:2}` → 5 秒内两端 `show sr-wg fec` 都是 `ratio 2`，
`parity` 与组序号严格 1:1、十二方向 0% 丢、关掉后 parity 计数立即不动（fail-open）。
**CPE1 换 agent 时又踩了 vpp0**（ssh 失联、数据面正常、看门狗 pass），复位后 20 秒
恢复；CPE2/3/4 没有复发 —— 这条脆性属 vpp0/lcp，不是换件的必然结果。
细节与"无损链路上的窗口错配（20 pps 对齐 / 200 pps 1.9%）"见
`docs/fec-and-lb-plan.md` §9.10。

**2026-11-13 QoS 配置已经管理面真实下发（CPE4，bundle v5）**：控制台 API（租户会话）
PUT /api/config 加 `qos:[{ingress:host-enp0s8, egress:host-enp0s9, rate_kbps:1000, llq_pct:0}]` →
POST /api/config/publish → agent 心跳拉取 → sr-apply 执行 → 实测 hook 挂上
host-enp0s8（按内层 DSCP 标记）、host-enp0s9 shaper 1000kbps 生效（q3 过流 0 丢），
`sr-apply boot` 重放 v5 也 ok（幂等）。CPE1/2/3 **有意不下发速率**——没有实测基线，
速率配小了会掐 BFD/业务，等 owner 拍各盒口径。**vm-verify 的 `fib_source_check` 修了一处
测试口径缺陷：agent.log 里混有二进制字节，不带 `-a` 的 grep 会静默截断匹配输出，
把「最后一推 3 行」误读成「0 行」造成假 FAIL（2026-11-13 实测，修复后 406/0/0）**。

**换件流程（2026-11-12 夜定型，4 次零失联）**

1. `scp` 新 `.so` 到 `/root/sr_wg_plugin.<tag>.so`，**不动盘上那个**；
2. 先起看门狗（脱离 ssh，180s 后判活）：
   `systemctl reset-failed qosguard; systemd-run --unit=qosguard --collect --no-block
   /bin/bash /root/qos-guard.sh`；
   判据是 ping 三个锚点（192.168.1.1 / 172.31.4.2 / 10.250.0.1），**全不通**才回滚：
   把盘上 `.so` 换回最新 `.bak-qos3-*`（已知可用的 `ip4-output` 版）→ `sync` → `reboot -f`
   → 自己 `disable`。证据写 `/var/log/qos-deploy/after-HHMM.txt`；
3. 再起部署（同样 detached）：`systemd-run --unit=qosdeploy --collect --no-block
   --setenv=SRC=/root/sr_wg_plugin.<tag>.so /bin/bash /root/qos4-deploy.sh`；
   它做：备份 `.bak-qos4-HHMMSS` → `install -m 0755` → `systemctl restart vpp` →
   等 CLI → 快照 plugins/features/interfaces/lcp/hook/errors/ip 到 `/tmp/qos4-*.txt`；
4. ssh 掉线不用管，回来先看 `/var/log/qos-deploy/{deploy.log,result.txt,after-*.txt}`，
   再 `ping` 客户侧。

**这一轮踩到的坑（都要记住）**

- **`dpo_type_t` / `ip_lookup_next_t` 的枚举值跨 build 不稳定**：镜像里
  `DPO_ADJACENCY_MIDCHAIN=8`、`IP_LOOKUP_NEXT_MIDCHAIN=6`，CPE4 的 vpp 是另一次编译
  （md5/build-host 都不同），同一个桶打出来 `type=30 next_node=14`。
  **判据里不许出现类型号**，要用运行时事实（邻接下标有效性、控制面解析出的出接口）。
- **`dpoi_index` 不一定是邻接下标**：MPLS over WG 的桶里是 `mpls-label` DPO，
  下标落到标签池，`adj_is_valid` 可能"侥幸命中"另一个邻接 —— 只看它会静默判错。
- **`vlib_get_node_by_name` 依赖 vlib 自己的 init 填充的哈希**，插件 init 早跑一步就查不到；
  要按名字找节点就扫 `vm->node_main.nodes`。
- **`fib_table_lookup` + `fib_entry_get_resolving_interface` 是唯一能穿过 MPLS/SR 栈
  拿到隧道口的办法**，但它是控制面量级的，**必须缓存**（见 `sr_qos.c` 的 `rt_cache`，2s 失效）。
- 诊断入口固定为 `show sr-wg qos resolve <ip> [from <if>]`：一次把 fib_index、
  load-balance 桶、桶的 DPO 类型/目标节点名、控制面解析结果、最终判定摊开。

- 安装位置：`/usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so`（该目录下无 `plugins{}`
  段，VPP 启动时自动装载，**没有运行时加载**）；
- 留底齐全：`.bak-qos-20261112`（最老）、`.bak-qos2-20261112`（19:28）、
  `.bak-qos3-20261112`（20:12，**当前在跑**）。回退 = 拷回原名 + `systemctl restart vpp`；
- 版本号仍是 **1.0.0**（下次连整形器一起 bump，别拿版本号判断有没有这个节点，用
  `show sr-wg qos-hook`）；
- **换插件必重启 VPP，而管理口在 VPP 里** ⇒ 换件必须 detached（`systemd-run`）+ 失败自回滚，
  照 #307 的教训做，不要在前台 ssh 里敲 `systemctl restart vpp`；
- **失联救援路径（2026-11-12 实测有效）**：CPE4 是 VirtualBox VM（`~/vm-lab/vms/CPE4/`，
  无 NAT 网卡、无快照、控制台口令 ubuntu/vm123）。数据面瘫时：
  `VBoxManage controlvm CPE4 acpipowerbutton` →
  `VBoxManage storageattach CPE1 --storagectl SATA --port 2 --device 0 --type hdd
  --medium ~/vm-lab/vms/CPE4/disk.vdi` → 在 CPE1 上 `mount /dev/sdb1 /mnt/c4`
  （脏盘用 `-o ro,noload`）修文件 → 卸盘 detach → `startvm CPE4`。
  健康判据别只看 `systemctl is-active`（VPP活着≠数据面活着）：
  从另一台盒 ping 它的 WAN IP + 抓包看有没有应答。
- 构建：`docker run --rm -v "$PWD/vpp-plugin":/workspace/vpp-plugin -w /workspace/vpp-plugin
  srwg-dev:latest` → `cmake -G Ninja && ninja`，产物在 `build-qos/lib/vpp_plugins/`。
  镜像里有 `vpp-dev 26.06-release`（`/usr/include/vnet/feature/feature.h` 是 26.06 的路径）。

---

## 3. 曾经踩到的三只雷：在跑的产物 ≠ 盘上的产物（2026-11-07 已收口，任务 #277）

11-06 夜实测出来的结论是：**`sr-fib` 和 `sr-agent` 一重启或一开机重放，就会退回没有缺陷修复的那一份**，
而且退回去是静默的。这一条当前已经不成立 —— 四台的盘上都换成了"正在跑的那一份"，
下面的表是 2026-11-07 当场复测的。但**这一节别跳过**：它记的是"怎么发现、怎么证明、以及还剩哪一处不一致"，
下次你自己铺件时要用同一套判据。

| 件 | 在跑（`/proc/<pid>/exe`） | 盘上（`/usr/local/bin`） | 收口前盘上是哪一份 |
| --- | --- | --- | --- |
| `sr-route` | `5789c2c0f753529b` | 同值 ✅ | 本来就同值（11-04 代，见下面"仍不一致的那一处"） |
| `sr-frr` | `c8e731ddcbca7bd6` | 同值 ✅ | 本来就同值（#274 那份） |
| `sr-fib` | `bab523ff3809f57b`（含缺陷⑦ 修复） | 同值 ✅ | `41c04d57fced05c0`（修复前）—— 重启即退回"VPP 重启后不重连 API、隧道表只在内存 ⇒ 客户流量永久黑洞"（缺陷⑦，#268） |
| `sr-agent` | `d2ad8519648c031f`（含缺陷④ 修复） | 同值 ✅ | CPE2/3/4 `f2e1461226db2dcd`（修复前）、CPE1 `32a1b26ada097227`（来历不明）—— 重启即退回"VPP 重启后不重建 wg 隧道且仍报 `ok=true`"（缺陷④，#260） |
| `sr_wg_plugin.so` | —— 见下面第 4 条 | CPE1–4 全 `9267d28d8b76fb54` ✅ | CPE2/3/4 `9d611883f8741999`（11-04 代） |

**收口用的方法（一句话）：盒内直接复制 `/proc/<pid>/exe` 到新 inode 再 `mv` 换上，不传输、不换进程。**
脚本 `test/lab/vmlab/exp-277/align-disk.sh`，逐台判据与前后哈希在 `exp-277/README.md` 和 §9.22。
之所以能这么做：`/proc/<pid>/exe` 跟着内核打开的那个 inode，复制出来的字节按定义就是在跑的那一份；
再用期望哈希（`d2ad8519…`/`bab523ff…`，都是 Mac 产物树里核对过的那两件）当闸门，不符就中断不装。
装完 `run==disk` 四台四件全一致，所以**重启/开机重放不再退回旧件** —— 这条是推导出来的（字节相同 ⇒ exec 出来的就是同一份码），
我们没有为此重启任何进程，也没有实测过重启后的行为。

复核（一条命令出全表，四台各跑一次）：

```bash
ssh CPE1 'for x in sr-route sr-frr sr-fib sr-agent; do
  pid=$(pgrep -x $x | head -1)
  printf "%-9s run=%s disk=%s\n" "$x" \
    "$(sha256sum /proc/$pid/exe | cut -c1-16)" \
    "$(sha256sum /usr/local/bin/$x | cut -c1-16)"
done'
```

**2026-11-08 复测（#276 切片 B 落地后，四台全表）** —— 上面那张 11-07 表现在只对 CPE2/3/4 成立：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | `sr-agent` | 收敛触发 |
| --- | --- | --- | --- | --- | --- |
| CPE1 | `26044f78f7969935` run==盘 | `a9f1bc8c8fd49016` run==盘（**事件版，但没带 B3**） | `c8e731ddcbca7bd6` run==盘 | `d2ad8519648c031f` run==盘 | `/watch` 事件推，`poll_sec` 只当重挂间隔；**可达性变化不触发收敛（B3 缺陷臂）** |
| CPE2/3/4 | `5789c2c0f753529b` run==盘 | `bab523ff3809f57b` run==盘（轮询版） | `c8e731dd…` run==盘 | `d2ad8519…` run==盘 | 每秒一轮 |

**2026-11-09 复测（B3 + B4 落地后，四台全表，同一套命令）** —— 上面两张表只有 CPE3 又变了：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | 收敛触发 |
| --- | --- | --- | --- | --- |
| CPE3 | `1161534d5c91db40` run==盘（**B4 版**：开机向 sr-frr 拉一份 local 全量） | `c213a6caf1b2aabf` run==盘（**B3 版**：订阅本机 VPP 的 BFD 状态事件） | `626b126a331f65e0` run==盘（**B4 版**：控制口新增 `GET /local-prefixes`） | `/watch` 事件推 **+ BFD 状态翻转** + 本机重启后自己回拉 local |

上面那段"CPE1 仍是…CPE2/CPE4 仍是轮询版"只描述到 2026-11-09 上午；#284 抬完盒之后由下面这张表取代。

**2026-11-09 复测（#284 抬盒后，四台全表，同一套命令）** —— 眼下现场就是这一张：

| 盒 | `sr-route` | `sr-fib` | `sr-frr` | 收敛触发 |
| --- | --- | --- | --- | --- |
| CPE1 | `1161534d5c91db40` run==盘（**B4 版**） | `a9f1bc8c8fd49016` run==盘（**事件版、没带 B3 —— 有意留的缺陷臂**） | `626b126a331f65e0` run==盘（**B4 版**） | `/watch` 事件推 + 本机重启后自己回拉 local；**可达性变化不触发收敛（B3 缺陷臂）** |
| CPE2 | `1161534d5c91db40` run==盘 | `c213a6caf1b2aabf` run==盘（**B3 版**） | `626b126a331f65e0` run==盘 | `/watch` 事件推 **+ BFD 状态翻转** + local 回拉 |
| CPE3 | 同上 | 同上 | 同上 | 同上 |
| CPE4 | 同上 | 同上 | 同上 | 同上 |

四台十二件 run==盘，**没有静默退回**（2026-11-09 实测，逐台 `sha256sum /proc/<pid>/exe` 对
`/usr/local/bin/<name>`）。抬盒的逐枪实测、流量归属表和回滚底在
[test/lab/vmlab/exp-284/README.md](test/lab/vmlab/exp-284/README.md)；用的仍是
`test/lab/vmlab/exp-276/install276.sh`（只装盘）+ `swap276.sh`（SIGTERM + 产品自己的开机清单补回 +
客户侧 75 秒探测）。~~换 sr-fib 的那一枪之后必须立刻 `vm-fib-sync.sh <节点>` 补隧道表~~
（此法已随 `cfb3dee` 退役：sr-agent 现在每拍心跳自动派生 sr-fib 隧道表，重启后一个心跳内自动回填，无需手工补）。

**2026-11-12 增量（服务面落地，只动了 CPE1 的三个件，其余盒未动）**：

| 盒 | 件 | 在跑 == 盘上 | 说明 |
| --- | --- | --- | --- |
| CPE1 | `sr-agent` | `1e8c8894e2467fcc`（两次换件：先 `6cb56276` 加 services 段，后加 FibTableSync 首拍守卫 + offload 持久化） | SIGTERM→watch 补回换件，实测心跳断 ~90s（watch 60s 一轮），数据面 0 扰动。换它的原因：bundle 里新增 services.json 段，老 agent 进程内的 apply 拷贝不认这段会静默丢掉 |
| CPE1 | `sr-apply` | `b2f40c322bd199d6`（盘上，one-shot） | agent 每次 apply 时 exec，含 offload 持久化 |
| CPE1 | `sr-svc` | `02db9629f312b2b1` | **新 daemon**：DHCP server + DNS 转发器，读 `/etc/sdwan/svc.json`（bundle 第 8 段），状态口 `127.0.0.1:8092/status`，SIGHUP 热换行不丢租约；start.sh 的 svc 步在 svc.json 存在且进程不在时拉起 |
| CPE2–4 | —— | 未动（only `/root/vmlab/underlay.sh` 已换新，含 offload-off） | 服务面只在 CPE1 验证过；配置模型/下发路径四台同构 |

**注意：CPE1 的 af_packet offload 雷（#302）已于 2026-11-12 持久化收口**：`apply.ensureHostIface`
（产品接管路径）与四台 `/root/vmlab/underlay.sh`（lab 开机清单，仓库 test/lab/vmlab/underlay.sh
同步）都会在接管时幂等 `ethtool -K <口> tso/gso/gro off`。下次重启起生效，验证法
`ethtool -k enp0s8 | grep tcp-segmentation`；VPP 升级后两处要一起重估。

**部署程序的教训（2026-11-12 三次开机挂死换来的）**：往盒上覆盖**可执行脚本**必须用
`install -m 0755 <src> /root/vmlab/underlay.sh`，不能 `scp xxx.new && mv`——scp 落盘 644，
mv 把执行位一起带走，systemd 下次开机就是 `vmlab-underlay.service ... status=203/EXEC`
（脚本没执行 = 接管没跑 = VPP 里没有 host-* 口 = sr-apply 开机重放永远卡在接口检查
= 盒子无地址失联）。`bash -n` 只查语法不查权限位，"检查通过"不代表部署成功；换完
脚本要 `ls -la` 确认 755。取证/诊断工具见 `test/lab/vmlab/nat-watch.sh`（2s 轻采样）
与 `nat-deep.sh`（vppctl 节点级深采样，走 unix socket 假死时也能答话）。

**#302 NAT 缺陷定性终版（2026-11-12 exp-nat-bare 裁决，含形状 D→G 四轮对照）**：
**不是 VPP 26.06 发行版缺陷，是配置形状问题。** 裸 VPP（禁 sr_wg、无 agent）+ 纯 CLI +
同形状 af_packet 口：客户数据面完全正常（cust4→网关 8/8 回包、会话建立）。"整机假死"
机制 = out2in 的 EI 语义：**外口上无会话的入向包一律丢（不止 pool 地址，外口地址自己也丢）**；
产品把 pool 设成外口自身地址（=管理 IP），管理面因此必死。四轮对照后定下的形状 G：
内口普通 `IF_INSIDE` + 外口普通 `IF_OUTSIDE` + 专用 pool 地址（外口 secondary）+
**外口自身地址的 address-only 身份映射** → 客户 8/8、盒子自身流量 3/3、ssh 50/50 存活、
`no translation` 只多 6 次、零 out of ports（形状 D 的 output-interface 有 1603 次
out of ports，会把 BFD/封装心跳吃掉；形状 E 的专用 pool 单独不够，管理面仍全丢）。
证据与机制账本：test/lab/vmlab/exp-nat-bare/README.md。

**产品已实现（本轮）**：`controller/internal/apply/nat.go` 按形状 G 对账 VPP（顺反：
pool 地址挂 secondary → 使能 → 身份映射 → 池范围 → 特性；退场反序），`services.go` 把
NAT 段接进 apply 流程（applyServices 现在带 prev，退场靠上一版的 pool 摘 secondary）；
`vppapi` 新增 `Nat44IdentityMapping`；保存期校验拦住"pool 含外口自身地址""pool 越出外口
网段""两行抢同一个池地址"；控制台 `nat_pending` 机制随之删除。CPE4 验收：bundle 带一行
NAT → apply 自己装出形状 G（ssh 40/40 ALIVE、cust4 8/8、会话 1 条、identity mapping 在位），
重复 apply 幂等，删行后全退；`systemctl restart vpp` 后 `sr-apply boot` 重放同样通过
（ssh 30/30 ALIVE）。26.10 升级对 NAT 不再必要。早间"cust12 transit 5/5 丢且 0 会话"系
半配置状态（使能链在 pool 步被打断），不能作数据面坏的证据。

**QoS 路径 C 方案已成文（2026-11-12，未写代码）**：`docs/qos-path-c-plan.md`。三句话记住它：
①Cisco 的 QoS 是两半 —— 无状态的分类/标记/限速，和有状态的**队列调度**（bandwidth%、buffer%、
wrr、llq、red）；②这台盒子**原生只有前半**（87 个插件里无 dpdk/hqos/qos，HQoS 是 DPDK 专属；
af_packet **只有 1 个共享 TX 队列**，`show features` 里的 qos 节点全是标记/限速，没有一个是排队）；
③后半**能借到但代价明确** —— 实测 af_packet 默认 `PACKET_QDISC_BYPASS` 让 VPP 出口包**完全绕过
主机 qdisc**（veth 上 `Sent 0 bytes 0 pkt`），建口加 `qdisc-bypass-disable` 后 105 包全过，
且 HTB **真限速**（100 kbit 下 500 发只过 60 发、88% 丢、backlog 201p）。产品建议：先做 C1
（VPP 原生 policer+标记），**把 Cisco 的队列字段做成发布期硬错误**而不是静默忽略；
C2（VPP 标记 + 主机 tc 调度）单独立项，先量 pps 代价/单队列排队行为/外层 DSCP 改写顺序三项。
C1 的第一块砖是 `policer_classify_set_interface` 的 CRC（机箱只装了 `policer.api.json` 与
core `classify.api.json`，这条消息只有 message-table 里的 id 1062）。**盒面已还原，无残留。**

**顺带抓到的一处真实不一致（缺口 G-2，不是本次引入）**：CPE3 的 sr-fib 运行时隧道表 3 条
（`curl -s http://127.0.0.1:19011/status` 的 `tunnels`），盘上 `/etc/sdwan/fib.json` 写的是 **`tunnels: 0`**
（CPE2/3/4 全 0，CPE1 是本轮我手工 `PUT /config` 恢复过的 2 条）。这张"下一跳 ↔ 本机 wg 口名 ↔ 颜色"
的推导表产品里没人落盘，进程一起来就靠上游重推 —— 详见台账 G-2。**后果**：这种盒子上如果 sr-fib 单独重启、
而 RR/agent 那条重推链正好没走到，就是缺陷⑦ 那一族的形状。**别把 `fib.json` 里的 `tunnels` 当成现场事实。**

**这个空窗的实际后果量出来了，比"黑洞"轻（2026-11-09 实测，CPE2 与 CPE4 各一次）**：新 sr-fib 起来到
`vm-fib-sync.sh` 补上隧道表之间，空窗分别 **124s / 79s**（这段里 `/status` 的 `tunnels=0`，所有远端路径按
「下一跳不在隧道表里」跳过）。两次窗口里的收敛行都是 `routes+0/-0` —— **它不拆已经装好的 VPP 行，只是不再收敛**。
所以形状是"从这一刻起路由变化全部失效"，不是"立刻断客户流量"：这两段窗口内客户探测 **0 FAIL**，
VPP 里 `mpls via` 条数也没变（没变是孤儿行还在，别把它当 B3 生效）。~~判定隧道表补没补上只认
`vm-fib-sync.sh` 那行 `隧道 N 条 … converged=True` 且没有 `警告:`~~（脚本已退役）——现在判定看两条：
agent.log 的 `sr-fib 隧道表已收敛：N 行`（写的人自报）+ `curl 127.0.0.1:19011/status` 的
`last.converged=true` 且 `warnings` 为空。

**2026-11-12 两笔 G-2 族的实况（服务面增量期间实测）**：

1. **CPE1 盘上 fib.json 里的 2 条陈年隧道已清（上面那段"手工 PUT 恢复过的 2 条"的残留，
   文件 mtime 2026-11-08）**：行内容还是 `wg1009/wg1010` + color 0 的手工时代形状，与派生纪律
   （不落盘）冲突，vm-verify 的 `fib_source_check` 因"盒里 2 条 == 磁盘 2 条"误判为非派生。
   已备份后清空 `tunnels`（`/etc/sdwan/fib.json.bak-20261112`），与 CPE2-4 的盘面（0 条）对齐；
   运行中的 sr-fib 不受影响（表在内存，agent 每拍重推）。
2. **抓到 agent 启动瞬态（新登记的已知缺口，G-2 族，非本次增量引入）**：换 sr-agent 后
   （01:04:39）FibTableSync 在 TunnelSync 还没填好 `links` 时推了一份 **0 行**的隧道表给 sr-fib，
   sr-fib 按警告「下一跳不在隧道表里」把远端路径全跳过，5 秒后（01:04:44）TunnelSync 就绪重推
   2 行、重新收敛。窗口 ~5s，窗口内客户探测 0 FAIL（§9 六方向 4/4），但它会撞上"agent 重启后
   恰好做 FIB 断言"的验证。根因：`FibTableSync.Sync()` 对 `FibRows()` 为空且 TunnelSync 从未
   成功同步过这一状态没有守卫（`fibtable.go` 在 rows 检查之后才判 dp==nil）。修法方向：首拍
   不推（等 TunnelSync 完成过一次），待 owner 拍板后另行落地。
3. **vm-verify §6 剩余 FAIL（本轮 13 条）定性：verify 口径落后于 2026-11-10 的策略引流设计，不是产品故障**。
   证据链：① §6 断言"overlay /32 全在 table 0"是策略引流（policy.go 头注，owner 2026-11-10 拍板
   "静态策略行引流到那张颜色的 FIB 表"）之前的口径；现在静态/动态策略行落地成**颜色表/选路表**
   （apply.log：`policy 1001 → 表 10001`、`policy 1002 → 表 3000`），远端 /32 的 mpls 路径物理上
   装在 2000/3000 这些颜色表里，表里还有 `0.0.0.0/0 → lookup in VRF:0` 的兜底默认 —— 这正是设计的
   数据面形状；② 四台（含**今天完全没动过**的 CPE2-4）呈同一形状 ⇒ 与服务面增量无关（服务面增量
   只加 sr-svc + services.json 段，机制上碰不到 VPP FIB）；③ 数据面全绿：§9 六方向 0% 丢包、
   客户 ping 实测 4/4、Encap 计数器在涨；④ 上一次全绿是 2026-11-09，策略引流 2026-11-10 落地，
   中间没人跑过全量 verify —— 口径在这窗口里过时了。**修法**：§6 判据要按"颜色表 + 基表"双表
   口径重写（哪个前缀该在基表、哪个该在颜色表、ABF 挂点在哪个口），待 owner 排期。
   另记一笔：CPE2 的 sr-fib 隧道表里有一行 `10.250.0.9 color=None`（手工时代 PUT 的残留，
   CPE2-4 今天没换 agent/config，老状态原封不动）—— 这行与台账对不上是 §3/§5 之外的潜在噪音，
   下次给 CPE2-4 发新 bundle（agent 派生整份替换）时会自然自愈。
4. **§6 口径已重写、回归 gate 已全绿（2026-11-12）**：vm-verify §6 改为"每个下一跳按它
   隧道颜色折表号核对"（ColorTable=color*1000+主表，颜色 0=主表），顺带全文件把
   `$var（` 形态统一 `${var}` 括护（本机 bash 把 UTF-8 标点字节并进变量名）。全量
   **PASS=398 FAIL=0 WARN=0**。瞬态①已由 `4e2c432` 的 FibTableSync 守卫收口
   （CPE1 sr-agent `1e8c8894`，换件实测首拍 hold、0 次 PUT、表未动）。
   **NAT 解套**：~~owner 拍了升级方向~~ —— **2026-11-12 exp-nat-bare 裁决后升级对 NAT
   不再必要**（NAT 是配置形状问题，见 #302 终版定性）；
   [docs/vpp-upgrade-compat-2610.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vpp-upgrade-compat-2610.md)
   的四步路径保留作纯产品动机（其他 26.10 收益）时的参考，NAT 一节已过时。

**仍不一致的那唯一一处（有意留着，不是漏）：** CPE1 的 `sr-fib` 还是 `a9f1bc8c8fd49016` —— 事件版但**没带 B3**，
它就是 B3 那一族的缺陷臂，抬到 `c213a6ca` 会把手上唯一的对照臂杀掉，所以按 #284 的方案**不动它**。
其余十一件全部落在 HEAD 那一代。回滚底在盒上按日期存着（2026-11-09 从 `/proc/<pid>/exe` 抽出来的，
哈希与当时的运行份逐一对上）：CPE1 `sr-frr.prerun-20261109`=`c8e731dd…`、`sr-route.prerun-20261109`=`26044f78…`、
`sr-fib.prerun-20261108`=`bab523ff…`；CPE4 `sr-frr/sr-route/sr-fib` 的 `.prerun-20261109` =
`c8e731dd…` / `5789c2c0…` / `bab523ff…`。

换进程这一步本身在缺陷⑧（开机清单与 watch 撞在同一秒各起一份，已用 `flock` 修掉，见 §7 表里 #269）之后
只剩"重启会抖一下 RR 会话"这个形状，不再是并发抢起。抬盒九枪的客户侧探测合计 **25282 发**（CPE2 三枪 8606 / CPE4 四枪 11076 / CPE1 两枪 5600），
其中 **CPE4/CPE2 的 sr-route 与 sr-fib 枪出现 0 或 1~2 发孤立 FAIL（无连续段）**。要说清两件事：

1. **`sr-frr` 这一枪本来就有窗。** 它重启 = 上报连接重连 = 按 AGENTS.md §1.3 立刻重置 local 前缀集，
   重置到它重新上报之间，本盒客户前缀在别盒就是 0 条 `mpls via`。所以"抬 sr-frr"的中断上界
   = 重启环 + 重新上报 + RR 反射 + 别盒装表。**同一个盒同一个方向实测两次**（CPE4，`cust12→172.16.4.1`）：
   对老份重启那一枪连续 2 发 / **1.03s**，对新份那一枪 1 发孤立 FAIL。各只有 1 个样本，
   **不足以下"新版更短"的结论**，能下的结论是"这一枪窗宽 0~1s、动之前必须按 §1.4 点名"。
   dmesg 无 link 事件，排除了链路抖。
2. **CPE1 那两枪 0 FAIL 不代表没窗 —— 那条探测对"CPE1 自己的 local 前缀被摘"不敏感。**
   站点 12 是双归的（CPE1 + CPE2 都报 `172.16.12.1/32`），CPE1 摘了 CPE2 那份还在，别盒不会掉路。
   要覆盖这一类故障必须测**单归**那一侧（站点 4 只有 CPE4 一条腿）。逐方向的敏感度表在
   `test/lab/vmlab/exp-284/README.md`。

修复件在 Mac 上的落点（都已核对哈希）：

- `sr-fib` 缺陷⑦ 那一份：`/Users/louis/srvmlab/artifacts/linux/sr-fib.20261106-bab523ff` = `bab523ff3809f57b`；
  **注意同目录里那个不带后缀的 `sr-fib` 是 `2e1ceee1cd4c849e`，是第三份，不是它**。
- `sr-agent` 在跑的那一份：`/Users/louis/srvmlab/artifacts/linux/sr-agent` = `d2ad8519648c031f`。
- 遥测插件：`/Users/louis/srvmlab/artifacts/plugins/sr_wg_plugin.so` = `9267d28d8b76fb54`（现在 = 四台盘上那份）。
- 一处**取不回**：四台在跑的 `sr-route`（`5789c2c0…`）在 Mac 的产物树里没有任何匹配文件，
  想恢复只能从源码重编一份，那**不会**得到同一个哈希（见 §4 第 2 条）。
- CPE1 那份来历不明的 `sr-agent`（`32a1b26a…`，9,697,139 字节）原先四处无副本，
  现在按被换下来的旧件保在盒上：`/usr/local/bin/sr-agent.prerun-20261107`。
- 被这次换下来的旧件全在盒上留底（`*.prerun-20261107`）：四台 `sr-fib` = `41c04d57…`、
  CPE2/3/4 `sr-agent` = `f2e14612…`、CPE2/3/4 插件 = `9d611883…`。
  **这顺带补上了台账里"`sr-fib` 没有回滚底"那条** —— `41c04d57…` 那份 11-04 代现在四台都在盒上。

---

## 4. 怎么判定"哪一份代码在跑"（这是方法，请当工具用）

我们踩过的三个误区，按顺序讲：

1. **`sha256sum /usr/local/bin/<x>` 不等于"在跑的"。** 换件是"先落盘、再 install、再换进程"，
   三个时刻任何一步没做，盘上和跑的就分叉。唯一权威的做法是
   `sha256sum /proc/<pid>/exe`（它跟着内核打开的那个 inode，哪怕文件已被删）。
2. **不能用"重新编译一份比哈希"来证明"跑的是这份源码"。** 实测：从今天的 HEAD 在 `lp-ctrl` 里
   `-trimpath CGO_ENABLED=0` 重编，`sr-route` 得到 `3668e20d…`、`sr-frr` 得到 `c7b6527b…`，
   而四台在跑的是 `5789c2c0…` / `c8e731dd…`。构建路径、buildinfo、VCS stamp 都会进哈希。
   **哈希只认"哪一份产物"，不认"哪一份源码"** —— 想证明后者，只能靠产物目录的命名/台账或 `-ldflags` 注版本（我们现在没注入）。
3. **VPP 插件盘上那份哈希不能用来判"正在跑的是哪一代"。** 实测四台盘上 `sr_wg_plugin.so` 的 mtime
   都比各自 VPP 启动时刻晚约 6 小时 —— 加载的是上一份文件。
   还有一条更硬的：`grep sr_wg_plugin /proc/<vpp pid>/maps` 在 CPE1 上显示
   `... sr_wg_plugin.so (deleted)` —— 在跑的那一代已经没有路径名指向它了，盘上那份是新 inode。
   字符串/符号比对也没用：盘上 `9d611883…` 与 `9267d28d…` 两份里 `sr-wg probe` 这个串都有（CPE2 上实测，命中各 1 次），
   而插件的命令/符号表是运行时才和 VPP 绑定的。**唯一可用的判据是功能探针本身**：
   `vppctl show sr-wg probe` 能回话、且两次采样（隔 2s）`echoed` 在涨，才说明"这一代在跑并且真在发探针"。
   想知道当前加载的是哪一份，只能拿 VPP 启动时刻去对那一时刻盘上文件的哈希（`stat -c %Y` + `/proc/<pid>` 的 etime，同一台机器上换算）。
   2026-11-07 之后四台盘上都是 `9267d28d…`，所以**下一次重启 VPP 载入的就是带探针这一代**；但"在跑的"仍要按上面这条实测，别拿盘上值推断。

4. **要把盘上补齐成"在跑的那一份"，别走网络。** 2026-11-07 实测 Mac↔盒子这条链路只有 **7KB/s 量级**
   （`gzip+scp` 5.4MB 走了 7 分钟没走完；换成盒子主动 `curl` 从 Mac 拉 247KB 也要 36 秒，速度一样）——
   18MB 的四件按这条路要两个多小时。而盒内 `cp /proc/<pid>/exe <同目录临时件>` 再 `mv` 换上，
   零传输、字节必然等于在跑那一份，期望哈希当闸门不符就不装。这条就是 #277 用的方法，脚本在 `test/lab/vmlab/exp-277/`。
   只有盒上本来没有那份字节的东西才需要走网络（比如插件：在跑的映射是 `(deleted)` inode，取不出来，只能推 247KB）。
   **需要走网络时也不要从 Mac 走：盒↔盒在管理 LAN 上是 226KB/s，比 Mac↔盒 快一个半量级**（2026-11-09 实测：
   8446056 字节的 `sr-frr` 从 CPE3 拉到 CPE4 用 37.3s）。形状 = 在已经有那份产物的盒上
   `mkdir /tmp/xfer && cp /usr/local/bin/<name> /tmp/xfer/ && timeout <N> python3 -m http.server 8099 --directory /tmp/xfer`
   （**不要加 `--bind <管理口 IP>`** —— 绑到 `vpp0` 那个地址会周期性 `OSError: [Errno 99]`，端口就废了），
   目标盒 `curl` 完必须 `sha256sum` 对一遍。**两个自己会咬人的坑**：`timeout` 到期会掐断正在服务的连接
   （curl 退 18、半份文件，所以超时要按总字节估，别按单件估）；`pkill -f "http.server 8099"` 会匹配到
   自己那条 ssh 命令行，把 ssh 一起打死（退 255）—— 记住 pid 用 `kill` 收口，别用 pkill。
   两条硬约束：**别覆写目标名**（覆写正在执行的二进制吃 `ETXTBSY`；覆写被 mmap 的插件会把在跑的数据面写坏），
   临时件要和目标**同一个文件系统**才保证 `mv` 是 rename。

顺带记两条正确的取证命令（都实测过）：

- 隧道口条数：`vppctl show interface | grep -cE '^wg'`（CPE1=2、CPE3/4=3）。
  `vppctl sr_wg_dump`、`vppctl show table`、`vppctl show wireguard interfaces` **都不是我们的命令**。
- **别给 `vppctl` 加 `-s /run/vpp/api.sock`。** `-s` 把 vppctl 切到二进制 API 口，CLI 命令发过去没有对应消息，
  于是**不回话也不报错地一直挂着**（2026-11-07 实测 CPE1：`vppctl -s /run/vpp/api.sock show interface` 和
  `… show sr-wg probe` 两条都零输出，靠 `timeout 15` 才收回来）。`vppctl <cli 命令>` 不加 `-s` 才是对的。
  挂掉的 vppctl 客户端会作为孤儿进程留在盒上（`pgrep -a vppctl` 能看到），不影响 VPP，但会污染下一次取证，
  用 `timeout N vppctl …` 包一层最省事。
  这条是我自己今天踩的：#277 收口后复测插件那轮，我第一次加了 `-s`，四个盒子的循环卡死在第一台。
- **引号不是问题**（更正先前写在这里的说法）。2026-11-07 在 CPE1 四种组合实测：
  `vppctl "show sr-wg probe"`、`vppctl show sr-wg probe`、`vppctl "show interface"`、`vppctl show interface`
  **全部正常回话**。被更正的那条出自 `86e4b2d`（昨天写把手时加的），`vmlab-rebuild-plan.md` 里查不到它对应的实测记录，
  今天在同一套 VM 盒上又复现不出来 ⇒ 按不成立删掉。今天这次卡死可以确定是 `-s` 造成的：同一条命令去掉 `-s` 立刻回话。
  插件在跑的判据是 `vppctl show sr-wg probe` 两次采样（隔 2s）`echoed` 在涨 —— 四台现在都涨
  （2 会话的 CPE1/2 约 +14/2s，3 会话的 CPE3/4 约 +20/2s）。

---

## 5. 改动的唯一合法路径

### 5.1 配置：走产品路径，一步都不能跳

```
控制台草稿 ──► 显式发布 ──► sr-agent 拉取/推送 ──► sr-apply 写第 5 段 ──► sr-frr PUT /config 热更
（127.0.0.1:8088）                                              /etc/sr-frr.json     /run/sr-frr/control.sock
```

FRR 配置一律 `frr-reload.py` 差量，VPP 走 CLI 增量，`sr-route`/`sr-frr`/`sr-fib` 只热更。
`sr-fib` 没有 unix 控制口，`/status` 与 `/config` 都挂在 HTTP `127.0.0.1:19011`
（[controller/cmd/sr-fib/main.go:532](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-fib/main.go#L532)
和 [:543](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/controller/cmd/sr-fib/main.go#L543)）；
`sr-frr` 才有 `/run/sr-frr/control.sock`。**生效路径里不许重启任何 daemon**（AGENTS.md §1.1），热更失败就如实报这次 apply 失败。

### 5.2 换二进制：四步，第二步最容易漏

```bash
# ① 推产物（只落盘，不装、不重启）
WORK=/tmp/pX bash test/lab/vmlab/exp-273/push-binary.sh CPE1 CPE2 CPE3 CPE4
# ② 安装到 /usr/local/bin —— push-binary.sh 不做这一步
ssh CPE1 'install -m 0755 /root/vmlab/artifacts/linux/sr-frr /usr/local/bin/sr-frr'
# ③ 换进程：SIGTERM 走产品自己的收尾（撤掉自己下发的 K 路由），由 watch 补回新进程
ssh CPE1 'kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service'
# ④ 判据用状态对比，不用退出码
ssh CPE1 'pid=$(pgrep sr-frr); [ "$(stat -Lc %i /proc/$pid/exe)" = "$(stat -c %i /usr/local/bin/sr-frr)" ]'
```

第 ② 步漏掉的后果我在今天真实犯过一次：`push-binary.sh` 之后直接换进程，重启起来的是**旧盘上那份**，
哈希没变 —— 等于换了个寂寞。这一条已经写进 §9.21 第五节和 `exp-274/README.md`。

第 ④ 步那条 inode 判据**只在"刚从这个名字起过进程"之后有意义**（它证明这个进程确实来自盘上这个文件）。
它不是"哪一代在跑"的通用判据：#277 之后四台的 `sr-agent`/`sr-fib` 就是 `run` 与 `disk` 哈希相同、
inode 不同（rename 换上去的，进程还持有旧 inode，所以 ②③ 之间没有重启）。
**判"哪一代"只认 `sha256sum /proc/<pid>/exe` 与产物台账对哈希**，inode 只用来回答"这个进程是不是从这个文件起的"。

换件是**必然中断**操作（SIGTERM 到 watch 补回之间该进程不干活），AGENTS.md §1.4 要求点名 + 给实测时长。
#274 那次四台 sr-frr 换件的实测在 [test/lab/vmlab/exp-274/README.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/test/lab/vmlab/exp-274/README.md)
（客户可见中断最长 1.0–2.0s，按方向不同）。

**换完 sr-fib 现在不需要手工补隧道表了**（2026-11-09 实测过 G-2 在这里咬人：sr-fib 的
「下一跳 → 本机 wg 口 + 颜色」那张表只在内存（`PUT /config`），新进程一起来就是 `0 条隧道`，
所有远端前缀按「下一跳不在隧道表里，该路径跳过」被跳过，而 VPP 里的老行还在
（`routes+0/-0`、`mpls via` 条数不变）—— 看着像"没坏"，其实收敛根本没发生）。
**2026-11-09 之后（`cfb3dee` ②）sr-agent 已在心跳节拍上自动派生并推送这张表**：
sr-fib 重启后一个心跳内自动回填，无需任何手工动作。原手工脚本 `vm-fib-sync.sh` 已退役删除。
判定自动回填完成的两条判据：agent.log 的 `sr-fib 隧道表已收敛：N 行` +
`curl 127.0.0.1:19011/status` 的 `last.converged=true` 且 `warnings` 为空。
已知残余：agent 自身刚启动的**第一拍**可能推 0 行瞬态（见上面 2026-11-12 那笔），换 sr-agent
之后若立刻做 FIB 断言，等它出「已收敛」行再判。
换 sr-route 那一枪必须**与 sr-frr 同批、且 sr-frr 在前**（B4 的部署硬规则，2026-11-09 实测：
老 sr-frr 没有 `GET /local-prefixes`，实测回 404，只抬 sr-route 会让它对着 404 每 2s 重试到永远，
本盒 local 前缀集照旧对全网消失）。B4 修好之后已经**不需要**再手工换 sr-frr 补前缀了 ——
新 sr-route 一起来就自己拉一份全量（CPE3 实测 `local` 在重启后 t+1s 就是 1，1899 发探测 0 FAIL）。

### 5.3 提交

```bash
git -c user.name='louis' -c user.email='louis@users.noreply.local' commit -m '...'
git push origin feat/vbond-control-plane
```

一次性身份，不动 git config；**只推 `feat/vbond-control-plane`，不推 master，不 force**
（master 历史里有密钥材料，那条线永远不推）。HEAD 随手会往前走，别信这里写的：现取
`git log --oneline -5`（本轮收口时依次是 `fe03988` vppctl 口径更正 → `57591f0` #277 产物收口 →
`86e4b2d` 这份把手入库 → `1dc4ab2` #274 窗口撤回）。
`.out/`、`controller/.out-*` 这类散工目录留在未跟踪状态。
每个验证过的增量都提交推送，并且**结尾给他一份能自己登录复核的把手**（账号/角色；口令值只在对话里给，不进仓库）。

---

## 6. 读数口径（下结论前先对齐字段）

| 读数 | 出自哪里 | 含义 |
| --- | --- | --- |
| `local` | `curl -s http://127.0.0.1:19001/status`（sr-route）的 `local` **数组**，长度即条数 | 本盒向全网宣称"我客户侧拥有"的前缀集，经 RR 反射给全网；sr-fib 用它判断"这条是本地前缀、不要装隧道" |
| `rib_routes` / `sessions[].connected/synced/routes` | 同上 | Loc-RIB 条数与两条 RR 会话的状态 |
| `dropped_own_route` / `dropped_no_path` / `dropped_rt_mismatch` | 同上 | 三类丢弃计数；回灌类问题先看 `dropped_own_route`（CPE1 现在 244） |
| `reported` / `zebra_connected` / `injected` / `skipped` / `import_rules` / `config_hash` | `curl -s --unix-socket /run/sr-frr/control.sock http://x/status` | sr-frr 这一轮实际报了多少、是否连着 zebra、注入了几条 |
| 内核 main 表 + zebra 视角 | `ip route show table main` / `vtysh -c 'show ip route'` | K 路由落地与否 |

`local=1 frr(reported=0 zebra_connected=true)` 这种组合是有意义的信号（说明是 zebra 那边空，不是链路没连上），
#274 那节就是靠它否掉了"窗口保护住了"的说法 —— 见 [docs/vmlab-rebuild-plan.md §9.21 第四节](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vmlab-rebuild-plan.md)。

一条永不混用的区分：**"前缀集掉空"的时长 ≠ "客户 BGP 会话恢复"的时长**。
旧的 12~19s refill 是"zebra 重新收录 K 路由 + sr-frr 轮询 + sr-fib 轮询"三段的合计，不是会话恢复时长
（文档里曾经混过一次，已改掉）。**2026-11-08 切片 A+B 之后这三段拆开重测**（同一台 CPE1、
注入 `systemctl restart frr`、两臂 sr-route 同一份产物）：摘路边 ≤45ms、装路边 201~755ms，
而"整条前缀不可用"仍是 5.33~6.66s 且**改前改后同量** —— 剩下的那 5s 是 zebra 收录 + 客户侧收敛的账，
不在 #276 范围。逐枪数字见 `test/lab/vmlab/exp-276/README.md` §三与§四。

`poll_sec` 现在的含义变了，别再按"轮询周期"理解：**代码默认仍是 2**（`cmd/sr-frr/main.go:105`），
lab 盘上 `/etc/sr-frr.json` 与 `/etc/sdwan/fib.json` 仍写 **1**（2026-11-08 复测四台都是 1），
但在 CPE1 上它只作为**断线重挂的间隔**用（`cmd/sr-fib/main.go:505-508`），不再是周期性收敛的节拍；
sr-frr 那一路改成变更驱动后，`poll_sec` 对它是兜底而非常态（切片 A，提交 a8083d2）。
CPE2/3/4 还是轮询版，那些盒上 `poll_sec=1` 依然是"每秒一轮"。

---

## 7. 测量陷阱（每一条都付过学费）

1. **盒子之间、盒子与 Mac 之间没有对时**（CPE1 的钟比 UTC 快约 10.7 分钟；2026-11-09 再量，
   被测盒与探测机之间互差 0.8~8.2 小时）。
   跨机只能比**时长**，不能比绝对时刻，文件 mtime 也不能横着比。注入时刻取盒子自己的 `date +%s.%3N`；
   判据锚点一律取**被测盒自己采到的那一次状态翻转**，别去对齐对面那台敲命令的时刻——
   8 小时的差会把 48ms 量成 480ms。
2. **Mac 的 BSD `date` 没有 `%N`**；`bash 3.2` 在大括号展开上有坑。跨机脚本别用它们。
3. **并发 `scp` 会静默截断文件**。产物推送一律串行，落地后 `sha256sum` 对一遍再算数。
4. **打流窗口放在管道子 shell 里会假绿**（"0ms 中断"就是这么来的）。计时要落在真正执行的那一侧。
5. **trace 只在收侧有意义**；转发到的包在收侧会被 punt，别把 punt 当丢包。
6. **`vppctl -s /run/vpp/api.sock <CLI 命令>` 不回话也不报错地一直挂着**（§4 有实测）；取证一律 `timeout 15 vppctl …`，
   跑完 `pgrep -a vppctl` 看一眼有没有留下的孤儿客户端。
7. **边跑边改被测脚本**会得到不可解释的数；脚本要么跑前定稿，要么重跑。
8. 原始读数不进仓库，落 `/tmp/<任务号>/`（`samples-*.txt` / `probe-*.txt` / `window-*.txt`），
   文档里只放汇总数字。
9. **Mac↔盒子这条链路只有 7KB/s 量级**（2026-11-07 实测，两个方向都是；§4 第 4 条有数字）。
   想"推一份产物过去再装"在这种链路上是几十分钟到几小时，而且中途断了会留下半截文件 ——
   补齐盘上那份要改成盒内 `cp /proc/<pid>/exe`（#277 的方法），只有盒上没有那份字节时才走网络。
10. **盒子上的 VPP 进程名是 `vpp_main`，不是 `vpp`** —— `pgrep -x vpp` 在这四台上返回空（今天实测），
    取 pid 用 `pgrep vpp_main`。`sha256sum /proc//exe` 这种拼出来的空 pid 会静默给你空哈希，
    看起来像"盘上没有"。
11. **`ssh -n` 配 heredoc 一声不响输出 0 行**（2026-11-09 踩过）：`-n` 把 stdin 换成 `/dev/null`，
    `<<'X'` 送不到远端，`bash -s` 就当没有输入直接结束。远端要读 heredoc 的那几条 ssh 一律不带 `-n`。
12. **引号形式的 heredoc 里本地变量不展开**，脚本会带着 `set -u` 在远端挂掉 ——
    而且常常是**活干完之后再挂**（`install276.sh` 就是这样：改名已经成功、它自己报 unbound variable 退出码非 0）。
    要送进远端的值一律走 `bash -s -- a b c` 位置参数；同类坑：`[ "$x" = y ] && echo` 放在脚本最后一行会把退出码带出去。
13. **客户侧探测必须带 `ping -I <源 loopback>`**（2026-11-09 就是这么误判过一次「BFD 根本没起来」）：
    不带 `-I` 时源地址取路由表选出来的 underlay 源，那条地址没有回程路径，量出来是 100% 丢包，
    与可达性无关。owner 在控制台里看到的「bfd 没起来」多数是这一条。
14. **别把 ECMP 的「留着死腿」读成丢一半**：VPP 按流选桶，同一组源/目的的探测全落在同一个桶里，
    实测要么这条流 20 秒全断、要么一条不丢（见 `exp-b3/README.md` 三臂）。要量"一半"得开多条并发流。
15. **`show ip fib table N`（整表）与 `show ip fib <前缀> table N`（单前缀）打的不是同一套东西**
    （2026-11-09 实测，exp-285 与 vm-verify 各栽一次）：整表 dump **只有承载链**
    （`unicast-ip4-chain` / `dpo-load-balance … buckets:N` / `mpls-label[…]` / `mpls via …`），
    **没有 `path:` 行、没有 `path-list`、没有 `forwarding:` 这个头**；单前缀查询才有
    `path-list:[N] … uPRF-list:M len:K`、每条 `path:[id] … pref=10 … oper-flags:resolved,` 和 `forwarding:` 段。
    所以「行里装了几条路径 / 各自 pref / resolved 没有」只能单前缀问；「客户流量真走哪条」两种都能读。
    `vm-verify.sh` 的 `fib_paths()` 就是为此加的（整表块里 awk `/path:\[/` 会一条都读不到，静默判红）。
16. **精确行被删时 `show ip fib <前缀>` 会打印覆盖它的那条（LPM 回落），不是报"没有"**
    （2026-11-09 实测回落到 `0.0.0.0/0 fib:0 index:0`，带两条 path-list：`lcp-rt` 管理网默认路由
    和 `entry-flags:drop` 的空路径）。不判前缀行就会把管理网默认路由的路径集当成我们的，
    量出「行里还剩两条、pref=20/0」这种根本不存在的形状。判别式：输出里第一条前缀行的
    `$1` 必须等于你问的前缀且 `$2 ~ /^fib:/`；取 `len`/`buckets` 时取**第一个**匹配，
    取最后一个会拿到那条 drop 路径的 `len:0`，凭空造出「行被删空」的信号。

---

## 8. 交差的门槛（缺一件就别写"已验证"）

一个增量的完整交差 = ① `lp-ctrl` 里 `go build/vet/test` 全绿；② 改前/改后**同一口径**的两组数字；
③ `bash test/lab/vmlab/vm-verify.sh` 到 398/0/0（2026-11-09 起的口径，见 §2.4）；④ `TAG=xx bash test/lab/vmlab/exp-273/six.sh` 六方向 0%；
⑤ 换过产物的话，§3 那张 run/disk 哈希表更新（**哈希对，不是 inode 对**，见 §5.2 第 ④ 步那段）；
⑥ 文档里点名必然中断与时长；
⑦ 提交推分支 + 给登录把手。

UI 类改动另加一条：**必须在真浏览器里点过并截图**，不能只看 HTML 源码下结论。
控制台是单文件 `controller/cmd/sr-ctrl/console.html`，**改完必须 `node --check` 里面的脚本**。

---

## 9. 未决队列（owner 已经点名的活）

按 owner 拍定的顺序，**一次只做一件**：

| 任务 | 状态 | 一句话现状 |
| --- | --- | --- |
| **#275** `resolvable` 改吃活动隧道 | **已落地（2026-11-08）**，单测 6 条绿；**e2e 已随 #284 抬盒跑过（2026-11-09）**，四台十二件 run==盘、`vm-verify.sh` 398/0/0、六方向 0% 丢包 | 门换成"配置 && 活着"：`reachableLocked()` 先看 `nextHops`（配置层，未配置=不可达，同旧），再看 `peerBFD[peer]`（活动层）。活动层是 agent 每次 `metricsLoop` 上报的 BFD 结果经 sr-route 新端点 `PUT /tunnel-state` 灌进来的；`sr-agent` 端接线在 `internal/agent/routestate.go`，`RouteStatePusher` 读 `route.json` 里的 `status_addr` 定位到本机 sr-route，把原始 `wire.TunnelMetrics` 行透传过去，聚合逻辑（同 peer 多隧道 any-up-wins、absent=没有信息）全在 `internal/route` 里。细则见 `docs/queue-plan-next.md` §二 落地实况。**四条 pin 的实际选择**：pin1 采用**内部/fib/reach.go 的 fail-open 口径**（"没跑 BFD 的 CPE 不该因为本包而黑洞所有路由"），与 §二 原方案里"严格 fail-closed"那条相反 —— 若 owner 拍要改回 fail-closed，只需调 `reachableLocked` 里 `!s.Known` 那行，代价是刚入网的前几秒（agent 第一次上报之前）本机路由会全部 pending；pin3 按 **peer 粗粒度 any-up-wins**（照 reach.go 的注释"路由只带对端地址，没有本地源，挑不了更细"），不是 §二 建议的 next-hop 精细；pin2 hold-down **本次未做**，交给 #262（那条本来就管"亚秒抖动放大"）；pin5 RR 侧本次未做，交给 #272 |
| **#276** 收敛改事件驱动 | **切片 A+B+C 全部落地（2026-11-08）**：A=提交 a8083d2（sr-frr→sr-route delta + 10ms 合并 + 重连全量），B=sr-route→sr-fib 的 SSE `/watch`（每前缀一条事件、10ms 合并、背压丢读端、订阅位独占 409、断流立即重挂+INIT 全量），C=判据与实测成文于 `test/lab/vmlab/exp-276/README.md`。新增单测 25 条全绿、`go test ./...` 全绿、`vm-verify.sh` 392/0/0、`exp-273/six.sh` 六方向 0% 丢包。**实测**（同一台 CPE1、同一注入 `systemctl restart frr`、两臂 sr-route 同一份 `26044f78f7969935`）：摘路边 ≤456/593ms → **≤40/41/45ms**，装路边 868/978ms → **201/294/755ms**；sr-route 发射端延迟直接量到（摘 ≤142ms、装 ≤84ms），残余在 sr-fib 一轮收敛 + VPP 装表那侧，本轮秒级日志拆不下去，已登记。**只剩 B2（三条口共用一套 transport identity）没做**。**2026-11-09 现场复核（#284 抬盒之后，以 §3 那张 run/disk 表为准）**：B3、B4 两条硬前置都已落地并实测收口（见下面两行）；**CPE2/CPE3/CPE4 三台现在都是"事件 + B3 + B4"**（`sr-route 1161534d`、`sr-fib c213a6ca`、`sr-frr 626b126a`），**CPE1 是有意留的缺陷臂**：`sr-route`/`sr-frr` 抬了，`sr-fib` 停在 `a9f1bc8c`（事件版、没带 B3），抬上去就没有对照了。抬盒之前那句"CPE2/CPE4 仍是轮询版"已作废 | 细则见 `docs/queue-plan-next.md` §三 落地实况（切片 B+C）。**这一片顺手抓到两条要 owner 拍的缺口：B3 watch 模式下隧道/BFD 状态变化没有任何收敛触发（`client.go:1142/1169` 的隧道门 + `cmd/sr-fib/main.go:301` 才读 BFD + 三个触发点 `main.go:509`/`watch.go:215/233/253`，现场证据 poll2 那一枪靠下一轮 1s 轮询救回）；B4 sr-route 重启后本地前缀集没人重建（`cmd/sr-frr/main.go:745-776` 变更驱动，本轮实测把 vm-verify 打出 5 条 FAIL，靠再换一枪 sr-frr 才补回）。**两条均已于 2026-11-09 收口，B4 按 owner 拍的形状 (b) 落地，判据与三臂数字在 `test/lab/vmlab/exp-b4/README.md`** |
| #272 跨盒回灌 | 待拍 | 注入的 K 路由进了客户 area0 后被别的盒当 `local` 上报 |
| #262 缺陷⑥ | 待拍 | 亚秒抖动被放大成 ≥1s 客户黑洞。**2026-11-09 更新口径**：sr-fib 那"1s 轮询直接摘路"的放大在事件版盒上已经没了（切片 B：摘路边实测 ≤40~45ms；B3 之后连"隧道/BFD 抖一下、RIB 没变"这一类也会立刻触发重算，双向 ≤65ms）。剩下的抖动放大全在 BFD 判定 + 没有 hold-down 这一侧；hold-down 现在**有地方挂了** —— 就挂在 B3 那条 `signal()` 上（`cmd/sr-fib/main.go:461` 造 `wake` → `:469` 建连时订阅 BFD 事件 → `cmd/sr-fib/watch.go:320` 的 `signal()`），挂在轮询上等于挂在一个 watch 模式已经不存在的节拍里 |
| ~~#269 缺陷⑧~~ | **已修（2026-11-07）** | `controller/deploy/sr-box/start.sh` 加了一把 `flock`（`LOCK=${SR_BOX_LOCK:-$RUN/.lock}`）：`run_up`/`run_stop` 都先抢它，非阻塞拿不到就 SKIP；`launch()`/`do_vpp`/`do_apply`/`do_frr` 每个后台子进程带 `9>&-` 关掉锁 fd，否则被 systemd `TimeoutStartSec` 杀掉时锁攥在守护进程手里永不释放。沙盒 5 路并发实测：`sr-agent`/`sr-route`/`sr-fib`/`sr-frr` 各起 1 份，4 份 SKIP 退出 0。**VM 台的物理复测没跑**（要 `systemctl restart vpp` 撞那一秒，属于会影响 lab 现状的动作，等 owner 点头再做）。抬 `sr-route` 到 HEAD 的**并发抢起阻塞已解**，剩下的只是重启本身的 RR 会话抖动 |
| ~~#268 / #260 产物没装全~~ | **已收口（#277，2026-11-07）** | 四台盘上已换成在跑那一份，`run==disk` 全一致；判据与留底见 §3 和 `exp-277/README.md`。**代码没再动**，这一轮修的是产物一致性 |
| ~~G-2 隧道表落盘~~ | **产品修复已落地（`cfb3dee` ②）** | sr-agent 心跳节拍自动派生 sr-fib 隧道表（`internal/agent/fibtable.go`），磁盘 0 条是有意设计；残余：agent 启动第一拍推 0 行的瞬态（2026-11-12 实测，见 §5.2/§3 附近那笔），待拍 |
| **QoS 路径 C（隧道 8 队列）** | **挂点已实测通过（2026-11-12），第一阶段节点已落地**；第二阶段（入队+drain+整形）未开工 | owner 已拍 **C**（VPP 内自研节点）+ **直接 8 队列**。对象是 **WireGuard 隧道**，不是网卡：隧道流量不经主机内核，所以 §6 的主机 tc 对隧道无效。判别符在外层 `udp sport`（wg1010/1011/1014 = 20000/20001/20002，FIB rewrite + tcpdump 双证据）。挂点实测：`sr-wg4-qos` 排在 `wg4-output-tun` **之前** ⇒ 加密前、明文、内层 DSCP 可读；三条隧道实测计到 1159 包。方案与取舍：`docs/qos-path-c-plan.md` §9；换件与回退：本文 §2.6 |
| NAT 阶梯 #222 + M-4/M-1/M-3/M-5、#211/#213/#244/S8 | 挂着 | 顺序 M-4→M-1→M-2→M-3 |

**#276 切片 B/C（2026-11-08）顺手抓到的 2 条缺口：B3、B4 均已于 2026-11-09 落地并实测收口（B4 的形状 owner 拍的是 (b)）**
（B3 的判据与逐枪数字在 `test/lab/vmlab/exp-b3/README.md`，B4 的判据与三臂数字在
`test/lab/vmlab/exp-b4/README.md`，两条的机制与落地实况都在 `docs/queue-plan-next.md` §三 末尾）：

| 缺口 | 场景 | 一句话现状 |
| --- | --- | --- |
| ~~**B3**~~ watch 模式下可达性变化无人触发收敛 | 隧道/BFD 抖一下、RIB 没变 | **已修（2026-11-09）**：sr-fib 在建连时订阅本机 VPP 的 `want_bfd_events`，会话状态翻转直接 `signal()` 叫醒收敛轮；订阅失败按"起不来"处理，不留半死状态。三臂实测（CPE3 / 注入在**对端** CPE1 的 wg1009）：轮询版摘路 745ms/装回 492ms；**事件版没有 B3 = 无期限**（那条死腿在 FIB 里挂了 21s、20 发探测全挂、sr-fib 一行日志都没打）；B3 版摘路与装回双向 **≤65ms**（一个采样间隔以下）、1557 发只 FAIL 1。**#262 的 hold-down 现在有这个触发点可以挂了** |
| ~~**B4**~~ 对端 sr-route 重启后 local 前缀集不重建 | 抬 sr-route 那一枪（队列第一节要干的活）之后 | **已修（2026-11-09，按 owner 拍的形状 (b)）**：sr-route 一起来就向 sr-frr 控制口 `GET /local-prefixes` 拉一份全量并整表原子应用，拿到就退出、**不做周期重发**（拉不到每 2s 重试，开机时 `do_route` 在 `do_bridge` 之前所以这条必然走到）；sr-frr 侧"应答即降级"把拉取与增量推送的先后次序关掉，"有规则但 zebra 没连上"回 503 而不是交出一张没法信的空集。三臂实测（CPE3，注入=只重启 sr-route，探测 cust12→172.16.3.1）：老 pair 那一臂 `local` t+1s 掉到 0、**150s 仍未自愈**、客户探测连续断流 **158.66s**；新 pair 这一臂 `local` **t+1s=1**、**1899 发 0 FAIL**，sr-route 新 pid 起来 1630ms（与老臂 1635ms 同量，拉没给重启加时长）。换件后 `vm-verify.sh` **392/0/0**、`exp-273/six.sh` 六方向 0%。**部署硬规则（实测）：`sr-frr` 与 `sr-route` 必须同批抬、`sr-frr` 在前** —— 老 sr-frr 没这个端点，实测 404，只抬 sr-route 就是对着 404 重试到永远 |

历史与决策的权威出处（要引结论去这里引，别信二手转述）：
[docs/vmlab-rebuild-plan.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/vmlab-rebuild-plan.md)（增量台账，最近四节是 §9.19/§9.20/§9.21/§9.22；§9.21 作废了前两节里"窗口保留"的说法，§9.22 是 #277 产物收口）、
[docs/loop-test-report.md](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/loop-test-report.md)（客户侧成环实验，P-3 已标注否决）、
`test/lab/vmlab/exp-27{1,3,4,7}/README.md`（实验台本，每个目录的 README 就是那轮的判据与局限）。
`/Users/louis/srvmlab/MANIFEST.md` 是产物台账，但它会滞后 —— 它原来那句"在跑的进程就是盘上这一份（inode 已核对）"
是**判据本身错了**（inode 相同只能证明进程来自那个名字，不能证明没被别人再换过一次；#277 之后更是哈希相同、inode 不同）。
它 11-07 已改成 run/disk 两列，但**以 §3 为准**，读之前先跑 §3 那条复核命令。

---

## 10. 两个 agent 共用一套 lab：冲突协议

这不是假想，这是你上车时的现实状态。共享的东西只有一份，且都不支持并发：

| 共享资源 | 冲突表现 | 约定 |
| --- | --- | --- |
| VirtualBox 7 台（同一时刻只有一个被测现场） | 你的故障注入把对方的基线打脏，两边结论都作废 | ~~动状态前先打招呼~~ **owner 2026-11-09 已撤掉窗口审批**（见本节下方原话）：这套研发台随时可动；仍要做的是「改动走产品路径 + 写清怎么还原」，以及别打脏另一个 agent 正在量的基线。只读脚本（`vm-verify.sh`、`exp-273/box-layers.sh`、`exp-273/six.sh`）随时可跑 —— **但先看 §10.1 有没有冻结令** |
| `/run/sr-frr/control.sock` 等盒内状态 | 两个 agent 各自热更，`config_hash` 对不上 | 一次只让一个 agent 持有"写盒内"的锁；换件前先看 `stat -c %y /usr/local/bin/<x>` 是否刚被别人动过 |
| 控制器 `state.json` + 8088 控制台 | 草稿互相覆盖、发布撞上别人的 apply | 草稿是共享的，**改配置类实验前先 `GET` 当前已发布版本**，别拿自己记忆里的值当现状 |
| `lp-ctrl`（同一份源码树） | `go build` 出来的产物不是你的改动 | 编译前 `git status --short` + `git rev-parse HEAD`，记下你构建时的树；产物一律放**自己任务号的目录**（`~/srvmlab/out-<N>/`），不要覆盖 `artifacts/linux/` 里别人的文件 |
| git 分支 `feat/vbond-control-plane` | 互相 rebase/reset 丢工作 | 不 `git add -A`（会带上别人的散工）；按文件名 stage；不 amend 已推的提交；不 reset --hard，先 `git status` 看清有没有别人的活 |

一条底线：**不许为了让自己的测量好看去改 lab 现状**（比如临时加条路由把现象压掉）。
lab 的当前形状属于 owner，不属于任何一次实验。

**owner 2026-11-09 的原话把上表第一行的"窗口"要求撤了**：「lab 是你研发测试用的，不存在窗口概念，
你随时想动都可以动，除非我明确说不动。」所以这套 VirtualBox 台**不需要先申请窗口**，
故障注入、换件、改配置发布都可以直接做。仍然成立的是另外两件事：
① 实验要改 lab 形状时**必须走产品路径**（控制台 → 发布 → agent → apply），并把「改了什么 / 怎么还原」
写进实验 README（exp-285 §五就是样板：t13 颜色 0→2、四台逃生开关 true，还原两步都不重启进程）；
② 有第二个 agent 在场时，别把对方正在量的基线打脏 —— 撤的是审批，不是"两边结论都作废"这个物理事实。
生产盒与别人现场的 lab 不在这条豁免里。

### 10.1 当前生效的冻结令（2026-11-09，#285 交付之后）

owner 原话：**「我自己测一下再说，lab 先放那里不动。」**

这条**优先于上面所有豁免**。含义逐条说清，别自己放宽：

- **写一律禁止**：不注故障、不换件、不改配置发布、不 `PUT /config`、不跑 `vm-fib-sync.sh`、
  不 `ESCAPE=…`、不重启任何进程、不重建 lab。
- **只读也不必跑**：`vm-verify.sh` / `six.sh` / `box-layers.sh` 这些虽然不写状态，但会占 CPU、
  打流量、动计数器，owner 正在自己测主备切换，**任何额外负载都会污染他的读数**。要读现场就读文件与 git，
  或者等他解冻。
- **冻结的形状是什么**：t13 颜色 2、四台逃生开关 true、CPE3 `172.16.12.1/32` = `len:2`（pref10 承载 /
  pref12 装着不承载）。这就是他要复测的那一枪的起手形状，**还原配方写在 exp-285 README §五，但现在不许执行**。
- **解冻条件**：只有 owner 明说。第二个 agent 上车时**第一件事就是问 owner 这条冻结令还在不在**，
  不许拿"窗口审批已撤"当解冻依据 —— 撤的是常设审批，冻结令是他针对这一次实验单独下的。
- 冻结期间能干的活：读代码、写方案、写单测、在 `lp-ctrl` 里 `go build/vet/test`（那是容器，不碰 VM 台）、
  改文档。**不能干的活**：任何需要 VM 台出数字的验证。所以冻结期间领的活必须是"代码 + 单测就能收口"的那一类。

具体如何拉起第二个会话（工作区挂载、任务切分、告示牌锁的落地形式、出事怎么退回干净态）见
[`docs/second-agent-onboarding.md`](file:///Users/louis/Documents/QoderCN/2026-10-15/chat-1/sdwan/docs/second-agent-onboarding.md)。
那一节是"怎么加入"的操作步骤；本节是"加入后不许踩什么"的硬约束。

---

## 11. 把手（登录与凭据的位置，值不进仓库）

- 控制台：`http://127.0.0.1:8088`（Mac 本机）。运营方令牌文件：`/Users/louis/srvmlab/ctrl/data/console-token`。
  PKI/状态同目录：`ca.key` `ca.pem` `server.key` `server.pem` `state.json`。
  **这些值一个都不许进文件/日志/提交信息**（AGENTS.md §3）。租户账号口令向 owner 当场索取，我们只在对话里传。
  脚本用的那份租户口令在 `/Users/louis/srvmlab/state/credentials.json`（0600，仓库外，`vm-console.py`
  从这里读；`vm-fib-sync.sh` 已退役删除，但它当年的 401 排障记录保留在下面）。**它和控制器对不上时不要改脚本、也不要猜口令**：
  用运营方令牌登录 → `POST /api/accounts/reset {"username": ...}`（控制器生成新口令、只在这次响应里出现一次）
  → 把返回值写回这个文件并 `chmod 600` → 再跑 `python3 -c` 走 `api_tenant()` 复检登录。
  2026-11-09 就靠这条把 `vm-fib-sync.sh` 的 `401 invalid username or password` 修好的；
  **换过一次口令必须当天在日常会话里把新口令交给 owner**（他手里那份会失效）。
  另：`sr-ctrl` 的 `-admin-token` 在 Mac 的 `ps` 里是明文可见的 —— 别把它抄进任何文件、日志或提交信息。
- 设备 API：`0.0.0.0:8444`（TLS）；STUN：`3478`/`3479`；RR：`18446/19446`（rr1）、`18447/19447`（rr2）。
- 盒内：`ssh CPE1|CPE2|CPE3|CPE4|cust12|cust3|cust4`（走 `~/.ssh/config` 别名，直连 IP 被拒）。
- 构建：`docker exec lp-ctrl sh -lc 'cd /workspace/controller && go build ./... && go vet ./... && go test ./...'`。
- 远程仓库：gitee，只推 `feat/vbond-control-plane`；HEAD 现取 `git log --oneline -5`（见 §5.3，别信写死的哈希）。

---

## 12. 术语速查

| 词 | 指什么 |
| --- | --- |
| `local` | 本盒向全网宣称的客户侧前缀集（不是"本地路由表"） |
| refill | 与 zebra 重连后，到本地前缀集重新长回原样那段时长（现由"zebra 收录 K 路由 + 1s 轮询"串成） |
| 乐观窗口 | 已被否决的那族代码：暂时看不见就先维持旧答案。AGENTS.md 第 3 条禁它 |
| 第 5 段 | `sr-apply` 写的 FRR/泄露配置段，落 `/etc/sr-frr.json`，热更经 `/run/sr-frr/control.sock` |
| `to_sdwan` 白名单 | 反向导出的过滤（prefix-list 驱动）；环路场景的正解是这条配置，不是代码 |
| 缺陷⑦ / 缺陷④ | VPP 重启后 sr-fib 不重连 API / sr-agent 不重建隧道且仍报 ok=true。代码已修，产物见 §3 |
| K 路由 | sr-frr 用 netlink 注进内核 main 表的路由（distance 0），zebra 靠 `redistribute kernel` 看见它 |
| 表号公式 | 路由表号 = 颜色×1000 + 主表号（颜色 0 就是主表；250 已彻底归 overlay） |

---

## 13. 这份文件怎么维护

你做完一个增量，如果它改变了本文任何一节的事实（尤其 §2 状态、§3 哈希表、§5 步骤、§9 队列），
**在同一个提交里改掉本文**，并在句子后面标上"（YYYY-MM-DD 实测）"。
删掉一条过期结论时，把它指向的原始出处留在原位，别把出处一起删了。
