# 控制器迁移：Mac(192.168.1.4) → 192.168.1.100（2026-11-18）

0.9 beta 收口后把控制面从 Mac 单机迁到 192.168.1.100（原物理 CPE，VPP 还在跑但只当
Ubuntu server 用）。**单机形态**，RR 也在同一台上。全程按共识路径收缩集群，没有改文件绕过。

## 1. 现在的形态

| 项 | 值 |
|---|---|
| 主机 | 192.168.1.100，Ubuntu 22.04，**aarch64**，2C/1.8G |
| sr-ctrl | `/usr/local/bin/sr-ctrl`（静态，arm64，构建自 `feat/vbond-control-plane@8a19cff`） |
| sr-rr | `/usr/local/bin/sr-rr` ×2（rr1:18446 / rr2:18447） |
| 数据 | `/opt/sdwan/ctrl/data`（整体从 Mac `~/srvmlab/ctrl/data` 搬入） |
| RR 材料 | `/opt/sdwan/state/`（rr-a/rr-b 的 crt/key/ca.pem，**迁后重签**，SAN=192.168.1.100） |
| systemd | `sr-ctrl.service`、`sr-rr@.service`（模板：`sr-rr@a`、`sr-rr@b`），全部 `enable --now` |
| 监听 | 8444 设备 mTLS / 8443 控制台 TLS（SAN=192.168.1.100 + 公网 203.0.113.10）/ 8088 loopback 明文 / STUN 192.168.1.100:3478+3479 |
| 集群 | 单 voter（c-0a250321=mac-1 身份，`node-id` 随 data 搬入），leader、quorum=true；mac-2/mac-6 已按 §5.2 共识下线（两台都走了"自退场"流程，日志干净） |
| 租户账号 | vm-tenant 密码已重置：`<redacted>`（operator token 不变，跟单元里的 -admin-token） |

**搬数据前先删了 `data/server.pem|server.key` 和 `console-tls/`**：`loadOrCreateServerCert`
只在文件缺失时签发，重启后按 `-tls-san 192.168.1.100,192.168.1.4` 和 `-console-san
192.168.1.100,203.0.113.10` 重签。CA 没动，设备身份/pinned_ca 全部继续有效，不需要重新入网。

## 2. 迁移过程中的两个坑（下次照抄时注意）

1. **tar 解包位置**：data.tgz 解到了 `/opt/sdwan/data` 而单元指向 `/opt/sdwan/ctrl/data`
   ——控制器在空目录上自举了一套全新 CA，租户登录失败、RR 验签失败"同时"出现。
   症状迷惑性极强：RR 的报错看起来像"RR 证书是旧一代 CA"。判据：先看
   `ca.pem` 指纹和搬迁源是否一致，再怀疑别的。
2. **Mac 上 `&` 起的后台进程会随 shell 死掉**：起 mac-2/mac-6 凑多数派时第一次用
   `&`，进程随 Bash 工具退出被杀，.100 侧表现为 `connect: connection refused`。
   要用常驻方式（nohup/launchd/容器）。

## 3. 集群收缩走了共识（不是改文件）

RAID：cluster 里有 3 个 active voter（mac-1/mac-2/mac-6），单机永远拿不到多数派、
写接口一律 503「没有 leader」。处理：在 Mac 上临时拉起 mac-2、mac-6 的实例（lab7 构建，
各自原数据目录）→ 多数派复活，mac-1 当选 leader → 用 operator 会话在 console API
`POST /api/cluster {action:"remove"}` 依次下线 mac-2、mac-6 → 两台按设计自退（日志
`已被移出集群，进程退出`，rc=0）→ Mac 侧 `launchctl disable` 关掉 mac-1 的旧 launchd
项，防止下次开机抢 8444。

**遗留**：成员表里 mac-1 行的 endpoint/console 还是 `192.168.1.4:844x`（没有改成员
endpoint 的 API；单机下只是审计字段，不影响服务）。将来若要再扩 HA，走 §5.1"替换"
流程把这个 endpoint 改掉，不要直接拿 192.168.1.4 加节点。

## 4. 设备侧待办（VM 现在都没开机，起来之后做）

STUN 地址是控制器下发的（`-stun` 已指向 .100），设备本地**只有一处**要改：

1. **sr-agent 的 systemd 单元**：`-ctrl 192.168.1.4:8444` → `-ctrl 192.168.1.100:8444`，
   `systemctl restart sr-agent`。CA 是同一张，不用重新认领。
2. **RR 列表**：控制台 rr1/rr2 行已改成 192.168.1.100:18446/18447，但按接口口径
   "改动只影响下一次发布"——设备连上新控制器后要在控制台**发布一次**基础配置，
   RR 列表才会换过去。
3. 判据：四台心跳恢复（last_seen 秒级）；任一隧道 ping 双向通；rr1/rr2 的
   `peer list` 日志不再报 dial .4 失败。

## 5. 回滚

Mac 侧一切未删：launchd 项只是 disable（`launchctl enable gui/501/com.louis.sr-ctrl`
即恢复），数据快照在 `~/srvmlab/data.migrate-snap-20261118/`（迁移瞬间）和
`~/srvmlab/ctrl/data/`（迁移后的最终状态）。.100 的数据也在 `/opt/sdwan/ctrl/data`。
回滚 = enable launchd 项 + 起进程，把设备 -ctrl 改回 .4 即可。

## 6. 验证记录（2026-11-18 21:22）

- `.100`：sr-ctrl/sr-rr@a/sr-rr@b 全 active；term 1697 leader=c-0a250321 quorum=true；
  rr1/rr2 grid 拉取正常、无 TLS 报错；控制台真 Chromium 渲染登录页正常。
- 内存：sr-ctrl 16MB + 两个 sr-rr 共 9MB；VPP 仍在跑（vpp0 持有 192.168.1.100，
  杀它就丢 SSH，没动）。
