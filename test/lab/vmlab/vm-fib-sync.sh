#!/usr/bin/env bash
# vm-fib-sync.sh - VM 研发台的 sr-fib 隧道表同步（test/lab/loop-fib-sync.sh 的 ssh 口径孪生）。
#
# 为什么还需要这一步（缺口 G-2，产品里没有它）：控制器下发的配置包一共六段，其中
# 没有 sr-fib 要的那张表。隧道由 agent 按第 3 段建在 VPP 上（wgXXXX，本端 overlay /32
# 绑在口上），第 4 段只把「隧道的下一跳地址 + VPN 表」写进 route.json 给 sr-route 用。
# sr-fib 装 overlay 路由时必须知道「10.250.0.2 这个下一跳 = 本机的哪张口、什么颜色」，
# 这个映射只有盒子自己知道，产品里今天没有任何进程维护它 —— 于是 overlay 前缀全部
# 按「下一跳不在隧道表里，该路径跳过」丢掉，VPP FIB 一条也不装。D3 只开了 PUT /config
# 这个接缝，所以这一段暂时由 lab 脚本填。
#
# 两头都是真数据，脚本不猜：
#   下一跳 / 本端 overlay / 颜色 ← 控制台 GET /api/underlay（隧道表，就是页面上那五行）
#   接口名                      ← 盒子里 vppctl show interface addr 的 wg 口 + 口上的 /32
#   VPN 表号                    ← 盒子里 /etc/sdwan/route.json 的第 4 段（控制台发布的）
#   逃生开关                    ← 保持盒子 sr-fib 当前值并打印（产品里没有下发路径）
#
# 用法：test/lab/vmlab/vm-fib-sync.sh          # 四台都同步
#       test/lab/vmlab/vm-fib-sync.sh 2 4      # 只同步指定 node
# 判据：每台推完回读 /status —— warnings 非空或 converged=false 就退出码 1。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
if [ $# -gt 0 ]; then WANTED="$*"; else WANTED="1 2 3 4"; fi

python3 - "$HERE" "$WANTED" <<'PY'
import importlib.util
import json
import subprocess
import sys

here, wanted = sys.argv[1], sys.argv[2].split()

spec = importlib.util.spec_from_file_location("vm_console", here + "/vm-console.py")
vm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(vm)

FIB_PORT = 19011          # VM 盒子每台一个 loopback 口，端口不用像容器台那样错开
DOC_MAX = 512


def sh(serial, *cmd, stdin=None):
    r = subprocess.run(["ssh", serial] + list(cmd), input=stdin,
                       capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit("FATAL: ssh %s %s: %s" % (serial, " ".join(cmd)[:60],
                                           (r.stderr or r.stdout).strip().replace("\r", "")[:DOC_MAX]))
    return r.stdout.replace("\r", "")


api, _ = vm.api_tenant()
try:
    und = api.call("GET", "/api/underlay")
    # 控制台的一条隧道 = 两端各一个 overlay /32；按本端摊开成「本端 overlay → 对端 overlay」。
    per_node = {}
    for t in und.get("tunnels") or []:
        for end, peer in ((t["a"], t["b"]), (t["b"], t["a"])):
            per_node.setdefault(str(end["node_id"]), []).append({
                "local_overlay": end["overlay"], "peer_overlay": peer["overlay"],
                "color": t.get("color") or 0, "tunnel": t["id"]})

    fail = 0
    for n in [int(w) for w in wanted]:
        serial = vm.BOX[n]["serial"]
        base = "http://127.0.0.1:%d/status" % FIB_PORT
        cur = json.loads(sh(serial, "curl", "-sf", base))
        # 本端 overlay → wg 口名：只认 wg 口上绑的那条 /32。
        ifs, hold = {}, None
        for line in sh(serial, "vppctl", "show", "interface", "addr").splitlines():
            if line.startswith("wg") and "(up)" in line:
                hold = line.split()[0]
            elif hold and "L3 " in line:
                ifs[line.split()[1].split("/")[0]] = hold
                hold = None
        rows = []
        for t in per_node.get(str(n)) or []:
            iface = ifs.get(t["local_overlay"])
            if iface is None:
                print("   node %d: 本端 overlay %s（隧道 %s）在 VPP 里没有对应的 wg 口——隧道没装上"
                      % (n, t["local_overlay"], t["tunnel"]))
                fail += 1
                continue
            rows.append({"next_hop": t["peer_overlay"], "interface": iface,
                         "color": t["color"]})
        route = json.loads(sh(serial, "cat", "/etc/sdwan/route.json"))
        vrfs = sorted({v.get("id") for v in route.get("vrfs") or [] if "id" in v}) or [0]
        body = json.dumps({"vrfs": vrfs, "tunnels": rows,
                           "escape_all_colors": bool(cur.get("escape_all_colors"))})
        sh(serial, "curl", "-sf", "-X", "PUT", "-H", "content-type: application/json",
           "--data-binary", "@-", "http://127.0.0.1:%d/config" % FIB_PORT, stdin=body)
        st = json.loads(sh(serial, "curl", "-sf", base))
        last = st.get("last") or {}
        print("   node %d: 隧道 %d 条（%s）vrfs=%s 逃生=%s → converged=%s routes_set=%s removed=%s 标签=%s 删标签=%s%s"
              % (n, len(rows),
                 " ".join("%s→%s" % (r["next_hop"], r["interface"]) for r in rows),
                 vrfs, bool(cur.get("escape_all_colors")),
                 last.get("converged"), last.get("routes_set"), last.get("routes_removed"),
                 last.get("labels_set"), last.get("labels_removed"),
                 "" if not last.get("warnings") else " 警告:%s" %
                 json.dumps(last["warnings"], ensure_ascii=False)[:DOC_MAX]))
        if last.get("warnings") or not last.get("converged"):
            fail += 1
    sys.exit(1 if fail else 0)
finally:
    api.logout()
PY
