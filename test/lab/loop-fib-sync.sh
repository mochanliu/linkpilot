#!/usr/bin/env bash
# loop-fib-sync.sh - 把「控制台的隧道」翻译成 sr-fib 认识的下一跳↔接口表并推进盒子。
#
# 为什么需要这一步（缺口 G-2，报告里点名）：配置包里六段中没有 sr-fib 的那张表。
# 隧道由 agent 按第 3 段建在 VPP 上（wgXXXX，本端 overlay /32 绑在口上），而 sr-fib
# 装路由时必须知道「10.250.0.2 这个下一跳 = 哪张口、什么颜色」——这个映射只有盒子
# 自己知道，产品里今天没有任何进程维护它。D3 只开了 PUT /config 这个接缝。
#
# 两头都是真数据，脚本不猜：
#   下一跳/本端 overlay/颜色 ← run/loop/tunnels.json（loop-console.py 从 GET /api/underlay 落的）
#   接口名                  ← 盒子里 vppctl show interface addr 的 wg 口 + 口上的 /32
#
# 用法：test/lab/loop-fib-sync.sh            # 4 台都同步
#       test/lab/loop-fib-sync.sh 2 4        # 只同步指定 node
set -u
export DOCKER_HOST=${DOCKER_HOST:-unix:///Users/louis/.colima/default/docker.sock}

HERE=$(cd "$(dirname "$0")" && pwd)
if [ $# -gt 0 ]; then WANTED="$*"; else WANTED="1 2 3 4"; fi
python3 -c '
import json, subprocess, sys

TUN_FILE, wanted = sys.argv[1], sys.argv[2].split()
DOC_MAX = 512


def dx(c, *cmd):
    r = subprocess.run(["docker", "exec", c] + list(cmd),
                       capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit("FATAL: %s %s: %s" % (c, " ".join(cmd)[:60], r.stderr.strip()[:DOC_MAX]))
    return r.stdout.replace("\r", "")


def wg_names(container):
    """{本端 overlay IP: 接口名}，只认 wg 口上绑的 /32。"""
    out, cur = {}, None
    for line in dx(container, "vppctl", "show", "interface", "addr").splitlines():
        if line.startswith("wg") and "(up)" in line:
            cur = line.split()[0]
        elif cur and "L3 " in line:
            ip = line.split()[1].split("/")[0]
            out[ip] = cur
            cur = None
    return out


doc = json.load(open(TUN_FILE))
nodes = sorted(doc["by_node"], key=int) if not wanted else [int(w) for w in wanted]
fail = 0
for n in nodes:
    c, fib_port = "lp-cpe%d" % n, 19010 + n
    base = "http://127.0.0.1:%d/config" % fib_port
    cur = json.loads(dx(c, "curl", "-sf", base))
    ifs = wg_names(c)
    rows = []
    for t in doc["by_node"][str(n)]:
        iface = ifs.get(t["local_overlay"])
        if iface is None:
            print("   node %d: 本端 overlay %s 在 VPP 里没有对应的 wg 口（隧道没装上）"
                  % (n, t["local_overlay"]))
            fail += 1
            continue
        rows.append({"next_hop": t["peer_overlay"], "interface": iface,
                     "color": t.get("color") or 0})
    body = json.dumps({"vrfs": cur.get("vrfs") or [0], "tunnels": rows,
                       "escape_all_colors": bool(cur.get("escape_all_colors"))})
    p = subprocess.run(["docker", "exec", "-i", c, "curl", "-sf", "-X", "PUT",
                        "-H", "content-type: application/json",
                        "--data-binary", "@-", base],
                       input=body, capture_output=True, text=True)
    if p.returncode != 0:
        print("   node %d: PUT /config 失败：%s" % (n, (p.stdout + p.stderr).strip()[:DOC_MAX]))
        fail += 1
        continue
    st = json.loads(dx(c, "curl", "-sf",
                       "http://127.0.0.1:%d/status" % (19010 + n)))
    last = st.get("last") or {}
    print("   node %d: 推 %d 条下一跳（%s），sr-fib converged=%s routes_set=%s removed=%s 标签=%s%s"
          % (n, len(rows),
             " ".join("%s→%s" % (r["next_hop"], r["interface"]) for r in rows),
             last.get("converged"), last.get("routes_set"), last.get("routes_removed"),
             last.get("labels_set"),
             "" if not last.get("warnings") else " 警告:%s" %
             json.dumps(last["warnings"], ensure_ascii=False)))
    if last.get("warnings") or not last.get("converged"):
        fail += 1
sys.exit(1 if fail else 0)
' "$HERE/run/loop/tunnels.json" "$WANTED"
