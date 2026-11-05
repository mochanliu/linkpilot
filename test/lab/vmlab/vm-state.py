#!/usr/bin/env python3
"""vm-state.py - 把盒子/RR 的 /status JSON 与台账逐条对账（判据与旁证同一份实现）。

为什么是独立文件（2026-11-05 的教训）：vm-verify.sh 里原先写的是
`python3 - <参数> > note`，程序体本该用 heredoc 从 stdin 灌进去。但这个脚本的 stdin
在 `while read … done < <(grep 台账)` 的循环体里就是台账本身；程序体没落进去的那一版
执行的是**空程序**，退出码 0、note 文件 0 字节，于是 §5 里 sr-route / sr-fib / RR
十条判据全是假绿。程序放进文件、参数走 argv，stdin 不再是接口，这类空转就没地方发生。

三个子命令（第一个参数）：
    route <status.json> <ledger.tsv> <node>        盒子里 sr-route :19001 的 /status
    fib   <status.json> <ledger.tsv> <node> <vpn>  盒子里 sr-fib  :19011 的 /status
    rr    <status.json> <tenant> <node_count>      Mac 上 rr-a / rr-b 的 /status

判据只有一条硬口径：与台账（vm-ledger.py，期望值来自控制台 GET /api/underlay +
两张手工表）逐条相等。不相等就 SystemExit 打差异；相等就在 stdout 打一行旁证
（数字，不是"正常"），vm-status.sh 原样打印这一行 —— 所以两份脚本看到的是同一个数。
"""
import json
import sys


def load(path):
    with open(path) as f:
        return json.load(f)


def ledger_rows(path, kind, node):
    out = []
    with open(path) as f:
        for line in f:
            cols = line.rstrip("\n").split("\t")
            if len(cols) > 1 and cols[0] == kind and cols[1] == str(node):
                out.append(cols)
    return out


def cmd_route(json_path, ledger, node):
    d = load(json_path)
    if not d.get("rib_ready"):
        sys.exit("rib_ready=%r（RR 会话没同步完）" % d.get("rib_ready"))
    want = {}
    for r in ledger_rows(ledger, "rib", node):
        want[r[2]] = sorted(x for x in r[3].split(",") if x)
    # sr-route 的 RIB 一条 path 一个前缀条目：站点 12 有两台（CPE1/CPE2）各播一次
    # 172.16.12.1/32，盒里就是两条（device_id 1 和 2），台账按前缀并成一条两下一跳。
    # 所以比的是「前缀 → 下一跳集合」， multipath 的语义就在这一层；条目数是旁证，
    # 合并了多少条一起打出来，避免把"两条变一条"看丢了。
    got = {}
    entries = (d.get("rib") or {}).get("0", [])
    for e in entries:
        got.setdefault(e["prefix"], []).extend(e.get("next_hops") or [])
    got = {k: sorted(v) for k, v in got.items()}
    if got != {k: sorted(v) for k, v in want.items()}:
        sys.exit("rib 不符：盒里=%s 台账=%s" % (got, want))
    wantl = sorted(r[2] for r in ledger_rows(ledger, "local", node))
    gotl = sorted(x["prefix"] for x in (d.get("local") or []))
    if gotl != wantl:
        sys.exit("local 不符：盒里=%s 台账=%s" % (gotl, wantl))
    ss = d.get("sessions") or []
    synced = sum(1 for s in ss if s.get("connected") and s.get("synced"))
    print("rib %d 前缀/%d 条路径[%s] local[%s] RR 会话 %d/%d 已同步 "
          "收到%s/导入%s/摘除%s/自身%s/无路径%s"
          % (len(got), len(entries),
             " ".join("%s→%s" % (k, ",".join(v)) for k, v in sorted(got.items())),
             " ".join(gotl), synced, len(ss),
             d.get("received", "?"), d.get("imported", "?"), d.get("withdrawn", "?"),
             d.get("dropped_own_route", "?"), d.get("dropped_no_path", "?")))


def cmd_fib(json_path, ledger, node, vpn):
    d = load(json_path)
    # 台账 tun 行：tun node tid 本端overlay 本端口 本端地址 出接口 对端node 对端overlay 对端端口 对端地址 颜色 MTU
    # sr-fib 的 next_hop 是「对端 overlay」，即第 9 列（0 基下标 8）。
    want = sorted(r[8] for r in ledger_rows(ledger, "tun", node))
    got = sorted(t.get("next_hop") for t in (d.get("tunnels") or []))
    if got != want:
        sys.exit("隧道表不符：盒里=%s 台账下一跳=%s" % (got, want))
    if sorted(str(v) for v in (d.get("vrfs") or [])) != sorted(str(v) for v in vpn.split(",")):
        sys.exit("vrfs 不符：盒里=%s 期望=%s" % (d.get("vrfs"), vpn))
    last = d.get("last") or {}
    if not last.get("converged"):
        sys.exit("上一次装表没收敛（last=%s）" % json.dumps(last, ensure_ascii=False))
    # 逃生开关产品里没有下发路径（缺口 G-2：vm-fib-sync.sh 保持盒子的现值），
    # 所以这里只报数不判红 —— 台账里没有它，硬判等于我造一个期望值。
    print("隧道表 %d 条[%s] vrfs=%s 逃生=%s 上次收敛 +%s/-%s 标签+%s/-%s"
          % (len(got), " ".join("%s→%s" % (t.get("next_hop"), t.get("interface"))
                                for t in (d.get("tunnels") or [])),
             d.get("vrfs"), bool(d.get("escape_all_colors")),
             last.get("routes_set"), last.get("routes_removed"),
             last.get("labels_set"), last.get("labels_removed")))


def cmd_rr(json_path, tenant, node_count):
    d = load(json_path)
    if d.get("tenant") != tenant:
        sys.exit("租户不符：%(got)r 期望 %(want)r" % {"got": d.get("tenant"), "want": tenant})
    ss = d.get("sessions") or []
    clients = [s for s in ss if s.get("role") == "client"]
    if len(clients) != int(node_count):
        sys.exit("客户端会话数 %(got)d，台账设备 %(want)d 台：%s"
                 % {"got": len(clients), "want": int(node_count),
                    "s": sorted(c.get("device_id") for c in clients)})
    lag = [c.get("device_id") for c in clients if not c.get("synced")]
    if lag:
        sys.exit("客户端未同步：%s" % lag)
    # RR↔RR 的连接状态在顶层 peers 里（sessions 里那条 role=rr 只有 synced，
    # 2026-11-05 实测字段：{id, addr, dialing, connected, since}）。
    peers = d.get("peers") or []
    down = [p.get("id") for p in peers if not p.get("connected")]
    if down:
        sys.exit("RR 邻居没连上：%s" % down)
    print("%s 租户 %s：客户端会话 %d/%d 已同步（各自路由数 %s），RR 邻居 %d 个全 connected，"
          "按设备路由数 %s"
          % (d.get("rr_id"), tenant, len(clients), len(clients),
             ",".join(str(c.get("routes")) for c in sorted(clients, key=lambda x: x.get("device_id"))),
             len(peers), json.dumps(d.get("routes"), sort_keys=True)))


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    cmd, args = sys.argv[1], sys.argv[2:]
    if cmd == "route" and len(args) == 3:
        cmd_route(*args)
    elif cmd == "fib" and len(args) == 4:
        cmd_fib(*args)
    elif cmd == "rr" and len(args) == 3:
        cmd_rr(*args)
    else:
        sys.exit("用法：vm-state.py route <json> <ledger> <node>"
                 " | fib <json> <ledger> <node> <vpn>"
                 " | rr <json> <tenant> <node_count>")


main()
