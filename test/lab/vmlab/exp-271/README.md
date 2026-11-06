# exp-271：本地前缀所有权被自己的注入吃掉（回灌锁死）

对应 `docs/vmlab-rebuild-plan.md` §9.19。这一轮只改产品码
（`controller/internal/frr/{plan,import}.go` + `controller/cmd/sr-frr/main.go`），
lab 里不打任何手工补丁：不 `ip route`、不改 vtysh 配置、不改隧道。

口径全部是"换件前后各量一次同一份读数"，所以这里的脚本一律只读
（`push271.sh` 是唯一会写盒子的：推 gzip 产物并 `install`，不重启任何进程）。

| 脚本 | 量什么 | 会不会动状态 |
| --- | --- | --- |
| `frr-forensic.sh <盒>` | sr-frr `/status` 全字段 + 注入账本 `state.json` + sr-route `local/rib/dropped_*` + 内核路由 + zebra 视角 + VPP FIB + 邻居 uptime/pid + 最近 30 行日志 | 只读 |
| `frr-layers.sh <盒>` | 三条前缀在四层的落点（内核 main / zebra / VPP FIB / sr-route RIB）+ sr-frr 账本与状态行 | 只读 |
| `frr-status-line.py` | 把 `/run/sr-frr/control.sock` 的 `/status` 压成一行（旧版缺新字段时打 `<旧版无此字段>`） | 只读 |
| `window-271.sh <盒> [探测机] [源] [目的]` | 优雅重启 `<盒>` 的 sr-frr，同时用客户 VM 的 loopback 秒级探测一条**当前通**的方向，算客户可见的断流时长 | 会 SIGTERM sr-frr（产品开机清单补回来）|
| `soak-271.sh` | 240s 内每 20s 断言：每台自己的客户前缀**不在**注入账本里、zebra 里它**不是** `Known via "kernel"` | 只读 |
| `push271.sh` | 一次性换件：`scp` gzip 产物 → `gunzip` → sha256 核对 → `install -m 0755` | 写文件，不重启 |

`raw/` 是这一轮的原始读数（before/after/layers/six/window/soak/verify），
逐发 ping 时间戳 `probe-*.raw` 体积大且可重跑，没入库。

## 生效姿势（零冷启动）

```
bash ../bringup-mac.sh CPE3 B                                   # 或 push271.sh：推产物 + install
ssh CPE3 'kill -TERM $(pgrep sr-frr); sleep 2; systemctl start sr-box-watch.service'
```

VPP / FRR / sr-agent / sr-route / sr-fib 全程 pid 不变。SIGTERM 走 sr-frr 自己的收尾
（日志里 `停止，撤掉自己下发的 N 条内核路由`），watch 单元是 Type=oneshot + `KillMode=process`，只补缺失进程。
这一枪的客户可见时长不是零，实测见 §9.19 第六节。
